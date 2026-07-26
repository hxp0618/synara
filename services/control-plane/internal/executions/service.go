package executions

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/outbox"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/podlifecycle"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/projects"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
	"github.com/synara-ai/synara/services/control-plane/internal/workerreleases"
)

const defaultProviderCursorMaximumAge = 30 * 24 * time.Hour
const defaultProviderCredentialAccessTTL = 5 * time.Minute
const defaultClaimRecoverySweepInterval = 2 * time.Second

type ServiceOption func(*Service)

func WithProviderCursorMaximumAge(maximumAge time.Duration) ServiceOption {
	return func(service *Service) {
		if maximumAge > 0 {
			service.providerCursorMaximumAge = maximumAge
		}
	}
}

func WithProjectService(projectService *projects.Service) ServiceOption {
	return func(service *Service) {
		service.projects = projectService
	}
}

func WithProviderCredentialAccessTTL(ttl time.Duration) ServiceOption {
	return func(service *Service) {
		if ttl > 0 {
			service.providerCredentialAccessTTL = ttl
		}
	}
}

type MemoryReferenceResolver interface {
	ResolveExecutionRecoveryMemoryReferences(
		context.Context,
		*gorm.DB,
		uuid.UUID,
		uuid.UUID,
	) ([]RecoveryMemoryReference, error)
}

func WithMemoryReferenceResolver(resolver MemoryReferenceResolver) ServiceOption {
	return func(service *Service) {
		service.memoryReferences = resolver
	}
}

// WithClaimRecoverySweepInterval throttles the opportunistic expired-state
// recovery sweeps performed on the worker claim hot paths so that at most one
// sweep per scope runs per interval across the process. The leader-elected
// reconciler sweep is unaffected and remains the recovery authority.
func WithClaimRecoverySweepInterval(interval time.Duration) ServiceOption {
	return func(service *Service) {
		if interval > 0 {
			service.claimRecoverySweeps = newRecoverySweepThrottle(interval)
		}
	}
}

func expectOne(result *gorm.DB, status int, code, message string) error {
	if result.Error != nil {
		return problem.Wrap(status, code, message, result.Error)
	}
	if result.RowsAffected != 1 {
		return problem.New(status, code, message)
	}
	return nil
}

type Service struct {
	db                          *gorm.DB
	authorizer                  *authorization.Authorizer
	sessions                    *sessions.Service
	leaseTTL                    time.Duration
	heartbeatTimeout            time.Duration
	receiptTTL                  time.Duration
	cursorCipher                *secret.CursorCipher
	providerCursorMaximumAge    time.Duration
	providerCredentialAccessTTL time.Duration
	targets                     *executiontargets.Service
	projects                    *projects.Service
	memoryReferences            MemoryReferenceResolver
	now                         func() time.Time
	claimRecoverySweeps         *recoverySweepThrottle
}

// recoverySweepThrottle rate-limits the opportunistic expired-state sweeps
// that run on the worker claim hot paths. Every idle worker polls claim at
// ~1s, so without a gate the fleet performs one full recovery sweep per
// worker per second — all redundant with the leader-elected reconciler,
// which remains the unthrottled authority for expired-state recovery.
type recoverySweepThrottle struct {
	interval time.Duration
	mu       sync.Mutex
	lastRun  map[string]time.Time
}

func newRecoverySweepThrottle(interval time.Duration) *recoverySweepThrottle {
	return &recoverySweepThrottle{interval: interval, lastRun: make(map[string]time.Time)}
}

// acquire reports whether a sweep for the given scope should run now and, if
// so, records the run. Skipped sweeps only delay recovery by at most the
// interval; the reconciler backstop is never throttled.
func (t *recoverySweepThrottle) acquire(scope string, now time.Time) bool {
	if t == nil || t.interval <= 0 {
		return true
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	last, seen := t.lastRun[scope]
	if seen && now.Sub(last) < t.interval && !now.Before(last.Add(-t.interval)) {
		return false
	}
	t.lastRun[scope] = now
	return true
}

func NewService(
	db *gorm.DB,
	sessionService *sessions.Service,
	leaseTTL, heartbeatTimeout, receiptTTL time.Duration,
	cursorCipher *secret.CursorCipher,
	targetService *executiontargets.Service,
	options ...ServiceOption,
) *Service {
	service := &Service{
		db: db, authorizer: authorization.NewAuthorizer(db), sessions: sessionService, leaseTTL: leaseTTL,
		heartbeatTimeout: heartbeatTimeout, receiptTTL: receiptTTL,
		cursorCipher: cursorCipher, providerCursorMaximumAge: defaultProviderCursorMaximumAge,
		providerCredentialAccessTTL: defaultProviderCredentialAccessTTL,
		targets:                     targetService, now: func() time.Time { return time.Now().UTC() },
		claimRecoverySweeps:         newRecoverySweepThrottle(defaultClaimRecoverySweepInterval),
	}
	for _, option := range options {
		option(service)
	}
	return service
}

func (s *Service) Register(ctx context.Context, input RegisterWorkerInput) (RegisteredWorker, error) {
	normalized, err := normalizeRegistration(input)
	if err != nil {
		return RegisteredWorker{}, err
	}
	targetKind := platform.ExecutionTargetKind(normalized.TargetKind)
	if platform.IsRemoteTarget(targetKind) && (!normalized.LeaseSupported || !normalized.FencingSupported) {
		return RegisteredWorker{}, problem.New(400, "remote_worker_protocol_required", "Remote workers must advertise leaseSupported and fencingSupported.")
	}
	plainToken, tokenHash, err := secret.NewToken()
	if err != nil {
		return RegisteredWorker{}, problem.Wrap(500, "worker_token_generation_failed", "Failed to generate a worker credential.", err)
	}
	now := s.now()
	var podIdentity *podlifecycle.ExactPodIdentity
	if normalized.RegistrationTrustMode == WorkerRegistrationTrustKubernetesPodBoundV1 {
		identity, err := podlifecycle.NewExactPodIdentity(
			normalized.ExecutionTargetID,
			normalized.Namespace,
			normalized.PodName,
			normalized.InstanceUID,
		)
		if err != nil {
			return RegisteredWorker{}, problem.Wrap(400, "invalid_worker_registration", "The Kubernetes Pod identity is invalid.", err)
		}
		podIdentity = &identity
	}
	var model persistence.WorkerInstance
	var target persistence.ExecutionTarget
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		resolvedTarget, resolvedKind, err := s.targets.ResolveWorkerRegistrationTargetInTransaction(
			ctx, tx, normalized.ExecutionTargetID, normalized.TargetKind,
			normalized.InstanceUID, normalized.SSHBootstrapGeneration,
		)
		if err != nil {
			return err
		}
		target = resolvedTarget
		targetKind = resolvedKind
		if podIdentity != nil {
			acquired, err := podlifecycle.TryTransactionLogicalIdentityLock(ctx, tx, *podIdentity)
			if err != nil {
				return problem.Wrap(500, "kubernetes_pod_lifecycle_lock_failed", "The Kubernetes Pod lifecycle lock could not be acquired.", err)
			}
			if !acquired {
				return problem.New(503, "kubernetes_pod_lifecycle_lock_unavailable", "The Kubernetes Pod lifecycle is changing; retry registration.")
			}
			fenced, err := podlifecycle.IsDeletionFenced(ctx, tx, *podIdentity)
			if err != nil {
				return problem.Wrap(500, "kubernetes_pod_deletion_fence_lookup_failed", "The Kubernetes Pod deletion fence could not be inspected.", err)
			}
			if fenced {
				return problem.New(409, "kubernetes_pod_deletion_fenced", "This Kubernetes Pod UID was fenced for deletion and cannot register.")
			}
		}
		if err := validateRegisteredWorkerIdentity(ctx, tx, normalized); err != nil {
			return err
		}
		var tombstone persistence.WorkerIdentityTombstone
		tombstoneErr := tx.WithContext(ctx).
			Where(
				"execution_target_id = ? AND cluster_id = ? AND namespace = ? AND pod_name = ?",
				normalized.ExecutionTargetID, normalized.ClusterID, normalized.Namespace, normalized.PodName,
			).
			Take(&tombstone).Error
		if tombstoneErr == nil {
			return problem.New(409, "worker_identity_revoked", "The logical Worker identity was administratively revoked and cannot be registered again.")
		}
		if !errors.Is(tombstoneErr, gorm.ErrRecordNotFound) {
			return problem.Wrap(500, "worker_revocation_lookup_failed", "Failed to inspect the Worker revocation fence.", tombstoneErr)
		}
		err = persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("execution_target_id = ? AND cluster_id = ? AND namespace = ? AND pod_name = ? AND status <> ?", normalized.ExecutionTargetID, normalized.ClusterID, normalized.Namespace, normalized.PodName, "terminated").
			Take(&model).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if targetKind == platform.TargetSSH && target.Status == "active" {
				return problem.New(
					409,
					"ssh_active_reregistration_invalid",
					"Active SSH Targets only allow restart registration for the current logical Worker identity.",
				)
			}
			model = persistence.WorkerInstance{
				ID: uuid.New(), Incarnation: 1, InstanceUID: normalized.InstanceUID,
				SSHBootstrapGeneration: normalized.SSHBootstrapGeneration,
				ExecutionTargetID:      normalized.ExecutionTargetID, TargetKind: normalized.TargetKind,
				WorkerMode:            normalized.WorkerMode,
				AssignedExecutionID:   normalized.AssignedExecutionID,
				WorkerPoolID:          normalized.WorkerPoolID,
				WorkerPoolVersion:     normalized.WorkerPoolVersion,
				CapacityClass:         normalized.CapacityClass,
				RegistrationTrustMode: normalized.RegistrationTrustMode,
				ClusterID:             normalized.ClusterID,
				Namespace:             normalized.Namespace, PodName: normalized.PodName, Version: normalized.Version,
				ProtocolVersion: normalized.ProtocolVersion,
				Capabilities:    normalized.Capabilities, LeaseSupported: normalized.LeaseSupported,
				FencingSupported: normalized.FencingSupported, AuthTokenHash: tokenHash, Status: "online",
				CompatibilityStatus:  "unknown",
				AdministrativeStatus: "active",
				RegisteredAt:         now, LastHeartbeatAt: now,
			}
			if err := tx.WithContext(ctx).Create(&model).Error; err != nil {
				return problem.Wrap(409, "worker_registration_conflict", "Worker registration conflicts with an active instance.", err)
			}
			if err := persistWorkerManifest(
				ctx, tx, &model, normalized.Version, normalized.Capabilities, target.Capabilities, targetKind, now,
			); err != nil {
				return err
			}
			if err := workerreleases.SynchronizeWorker(ctx, tx, &model, now); err != nil {
				return err
			}
			return ensureWorkerIncarnationFactWithResourcesLocked(
				ctx, tx, model, now, "",
				normalized.RequestedCPUMillicores,
				normalized.RequestedMemoryBytes,
				normalized.RequestedEphemeralStorageBytes,
			)
		}
		if err != nil {
			return problem.Wrap(500, "worker_registration_lookup_failed", "Failed to resolve the worker registration.", err)
		}
		if model.AdministrativeStatus == "revoked" {
			return problem.New(409, "worker_identity_revoked", "The logical Worker identity was administratively revoked and cannot be registered again.")
		}
		if targetKind == platform.TargetSSH && target.Status == "active" &&
			(model.InstanceUID != normalized.InstanceUID ||
				!equalOptionalInt64(model.SSHBootstrapGeneration, normalized.SSHBootstrapGeneration)) {
			return problem.New(
				409,
				"ssh_active_reregistration_invalid",
				"Active SSH Worker restart registration must match the current instance UID and bootstrap generation.",
			)
		}
		if normalized.RegistrationTrustMode == WorkerRegistrationTrustKubernetesPodBoundV1 &&
			model.RegistrationTrustMode == WorkerRegistrationTrustKubernetesPodBoundV1 &&
			model.InstanceUID == normalized.InstanceUID {
			// The projected credential remains readable inside the Pod for token
			// rotation. Make registration itself one-shot for the physical Pod UID
			// so a Provider subprocess cannot replay it to replace agentd's Worker
			// credential and incarnation.
			return problem.New(409, "kubernetes_worker_instance_already_registered", "This Kubernetes Pod UID already registered its Worker instance.")
		}
		if err := transitionWorkerIncarnationFactLocked(
			ctx, tx, model, now, workerFactStateTerminated, false, "worker-reregistered",
		); err != nil {
			return err
		}
		updates := persistence.WorkerInstance{
			Incarnation: model.Incarnation + 1, InstanceUID: normalized.InstanceUID,
			SSHBootstrapGeneration: normalized.SSHBootstrapGeneration,
			TargetKind:             normalized.TargetKind, WorkerMode: normalized.WorkerMode,
			AssignedExecutionID: normalized.AssignedExecutionID,
			WorkerPoolID:        normalized.WorkerPoolID,
			WorkerPoolVersion:   normalized.WorkerPoolVersion,
			CapacityClass:       normalized.CapacityClass,
			Version:             normalized.Version, ProtocolVersion: normalized.ProtocolVersion,
			RegistrationTrustMode: normalized.RegistrationTrustMode,
			Capabilities:          normalized.Capabilities, AuthTokenHash: tokenHash,
			LeaseSupported: normalized.LeaseSupported, FencingSupported: normalized.FencingSupported,
			Status: "online", RegisteredAt: now, LastHeartbeatAt: now,
		}
		result := tx.WithContext(ctx).Model(&persistence.WorkerInstance{}).
			Where("id = ?", model.ID).
			Select(
				"incarnation", "instance_uid", "ssh_bootstrap_generation", "target_kind", "worker_mode", "assigned_execution_id", "worker_pool_id", "worker_pool_version", "capacity_class", "registration_trust_mode", "version", "protocol_version", "capabilities", "auth_token_hash", "lease_supported",
				"fencing_supported", "status", "registered_at", "last_heartbeat_at", "draining_at", "terminated_at",
			).Updates(&updates)
		if err := expectOne(result, 500, "worker_registration_update_failed", "Failed to refresh the worker registration."); err != nil {
			return err
		}
		model.ExecutionTargetID = normalized.ExecutionTargetID
		model.Incarnation = updates.Incarnation
		model.InstanceUID = normalized.InstanceUID
		model.SSHBootstrapGeneration = normalized.SSHBootstrapGeneration
		model.TargetKind = normalized.TargetKind
		model.WorkerMode = normalized.WorkerMode
		model.AssignedExecutionID = normalized.AssignedExecutionID
		model.WorkerPoolID = normalized.WorkerPoolID
		model.WorkerPoolVersion = normalized.WorkerPoolVersion
		model.CapacityClass = normalized.CapacityClass
		model.RegistrationTrustMode = normalized.RegistrationTrustMode
		model.Version = normalized.Version
		model.ProtocolVersion = normalized.ProtocolVersion
		model.Capabilities = normalized.Capabilities
		model.LeaseSupported = normalized.LeaseSupported
		model.FencingSupported = normalized.FencingSupported
		model.AuthTokenHash = tokenHash
		model.Status = "online"
		model.RegisteredAt = now
		model.LastHeartbeatAt = now
		model.DrainingAt = nil
		model.TerminatedAt = nil
		if err := persistWorkerManifest(
			ctx, tx, &model, normalized.Version, normalized.Capabilities, target.Capabilities, targetKind, now,
		); err != nil {
			return err
		}
		if err := workerreleases.SynchronizeWorker(ctx, tx, &model, now); err != nil {
			return err
		}
		return ensureWorkerIncarnationFactWithResourcesLocked(
			ctx, tx, model, now, "",
			normalized.RequestedCPUMillicores,
			normalized.RequestedMemoryBytes,
			normalized.RequestedEphemeralStorageBytes,
		)
	})
	if err != nil {
		return RegisteredWorker{}, err
	}
	return RegisteredWorker{Worker: toWorker(model), Token: plainToken}, nil
}

func (s *Service) Authenticate(ctx context.Context, plainToken string) (persistence.WorkerInstance, error) {
	plainToken = strings.TrimSpace(plainToken)
	if plainToken == "" {
		return persistence.WorkerInstance{}, problem.New(401, "worker_authentication_required", "A worker bearer token is required.")
	}
	var worker persistence.WorkerInstance
	err := s.db.WithContext(ctx).
		Where("auth_token_hash = ?", secret.HashToken(plainToken)).
		Take(&worker).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.WorkerInstance{}, problem.New(401, "invalid_worker_token", "The worker bearer token is invalid.")
	}
	if err != nil {
		return persistence.WorkerInstance{}, problem.Wrap(500, "worker_authentication_failed", "Worker authentication failed.", err)
	}
	if worker.AdministrativeStatus == "revoked" {
		return persistence.WorkerInstance{}, workerTokenRevoked()
	}
	if worker.Status == "terminated" {
		return persistence.WorkerInstance{}, problem.New(401, "invalid_worker_token", "The worker bearer token is invalid.")
	}
	if err := requireKubernetesPodNotDeletionFenced(ctx, s.db, worker); err != nil {
		return persistence.WorkerInstance{}, err
	}
	return worker, nil
}

func (s *Service) Heartbeat(
	ctx context.Context,
	worker persistence.WorkerInstance,
	input HeartbeatInput,
) (Worker, error) {
	if input.ProtocolVersion != WorkerProtocolVersion {
		return Worker{}, unsupportedWorkerProtocol(input.ProtocolVersion)
	}
	if worker.ProtocolVersion != WorkerProtocolVersion {
		return Worker{}, unsupportedWorkerProtocol(worker.ProtocolVersion)
	}
	version := strings.TrimSpace(input.Version)
	if len(version) > 160 {
		return Worker{}, problem.New(400, "invalid_worker_version", "Worker version must not exceed 160 characters.")
	}
	var current persistence.WorkerInstance
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		target, targetKind, err := s.targets.ResolveWorkerBootstrapTargetInTransaction(
			ctx, tx, worker.ExecutionTargetID, worker.TargetKind,
			worker.InstanceUID, input.SSHBootstrapGeneration,
		)
		if err != nil {
			return err
		}
		locked, err := lockCurrentWorker(ctx, tx, worker)
		if err != nil {
			return err
		}
		current = locked
		if current.TargetKind == "ssh" &&
			!equalOptionalInt64(current.SSHBootstrapGeneration, input.SSHBootstrapGeneration) {
			return problem.New(
				409,
				"ssh_bootstrap_authority_invalid",
				"SSH Worker bootstrap generation does not match its registered authority.",
			)
		}
		if err := requireKubernetesPodNotDeletionFenced(ctx, tx, current); err != nil {
			return err
		}
		now := s.now()
		if input.Capabilities != nil {
			manifestVersion := current.Version
			if version != "" {
				manifestVersion = version
			}
			normalized, normalizeErr := normalizeWorkerManifest(
				manifestVersion, input.Capabilities, target.Capabilities, targetKind, now,
				workerManifestRegistrationContext{
					ExecutionTargetID: current.ExecutionTargetID, TargetKind: targetKind,
					InstanceUID: current.InstanceUID, ClusterID: current.ClusterID,
					Namespace: current.Namespace, PodName: current.PodName,
				},
			)
			if normalizeErr != nil {
				return workerManifestReregistrationRequired(normalizeErr.Error())
			}
			matches, matchErr := workerManifestMatches(ctx, tx, current, normalized)
			if matchErr != nil {
				return matchErr
			}
			if !matches {
				return workerManifestReregistrationRequired("Heartbeat Provider capabilities differ from the immutable registered Worker manifest.")
			}
		}
		updates := persistence.WorkerInstance{LastHeartbeatAt: now}
		fields := []string{"last_heartbeat_at"}
		if input.Draining != nil && *input.Draining {
			updates.Status = "draining"
			updates.DrainingAt = &now
			fields = append(fields, "status", "draining_at")
		} else if input.Draining != nil && !*input.Draining {
			updates.Status = "online"
			updates.DrainingAt = nil
			fields = append(fields, "status", "draining_at")
		} else if current.Status == "offline" {
			updates.Status = "online"
			fields = append(fields, "status")
		}
		if version != "" {
			updates.Version = version
			fields = append(fields, "version")
		}
		if input.Capabilities != nil {
			updates.Capabilities = input.Capabilities
			fields = append(fields, "capabilities")
		}
		result := tx.WithContext(ctx).Model(&persistence.WorkerInstance{}).
			Where("id = ? AND incarnation = ? AND instance_uid = ? AND administrative_status <> ?",
				current.ID, current.Incarnation, current.InstanceUID, "revoked").
			Select(fields).Updates(&updates)
		if result.Error != nil {
			return problem.Wrap(500, "worker_heartbeat_failed", "Failed to record the worker heartbeat.", result.Error)
		}
		if result.RowsAffected != 1 {
			return workerTokenRevoked()
		}
		if err := tx.WithContext(ctx).Where("id = ?", current.ID).Take(&current).Error; err != nil {
			return problem.Wrap(500, "worker_reload_failed", "Failed to reload the worker.", err)
		}
		if err := workerreleases.SynchronizeWorker(ctx, tx, &current, now); err != nil {
			return err
		}
		factState, err := currentWorkerFactStateLocked(ctx, tx, current)
		if err != nil {
			return err
		}
		return transitionWorkerIncarnationFactLocked(ctx, tx, current, now, factState, false, "")
	})
	if err != nil {
		return Worker{}, err
	}
	return toWorker(current), nil
}

func equalOptionalInt64(left, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func lockCurrentWorkerIncarnation(ctx context.Context, tx *gorm.DB, worker persistence.WorkerInstance) error {
	_, err := lockCurrentWorker(ctx, tx, worker)
	return err
}

func lockCurrentWorker(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
) (persistence.WorkerInstance, error) {
	var current persistence.WorkerInstance
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("id = ?", worker.ID).Take(&current).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.WorkerInstance{}, problem.New(409, "worker_incarnation_fenced", "The Worker registration is no longer current.")
	}
	if err != nil {
		return persistence.WorkerInstance{}, problem.Wrap(500, "worker_incarnation_lock_failed", "Failed to lock the current Worker registration.", err)
	}
	if current.AdministrativeStatus == "revoked" {
		return persistence.WorkerInstance{}, workerTokenRevoked()
	}
	if current.Status == "terminated" {
		return persistence.WorkerInstance{}, problem.New(409, "worker_incarnation_fenced", "The Worker registration is no longer current.")
	}
	if current.Incarnation != worker.Incarnation || current.InstanceUID != worker.InstanceUID ||
		!equalTokenHashes(current.AuthTokenHash, worker.AuthTokenHash) {
		return persistence.WorkerInstance{}, problem.New(409, "worker_incarnation_fenced", "The Worker registration is no longer current.")
	}
	return current, nil
}

func requireKubernetesPodNotDeletionFenced(
	ctx context.Context,
	db *gorm.DB,
	worker persistence.WorkerInstance,
) error {
	if worker.TargetKind != "kubernetes" ||
		worker.RegistrationTrustMode != WorkerRegistrationTrustKubernetesPodBoundV1 {
		return nil
	}
	identity, err := podlifecycle.NewExactPodIdentity(
		worker.ExecutionTargetID,
		worker.Namespace,
		worker.PodName,
		worker.InstanceUID,
	)
	if err != nil {
		return problem.Wrap(500, "kubernetes_pod_identity_invalid", "The persisted Kubernetes Pod identity is invalid.", err)
	}
	fenced, err := podlifecycle.IsDeletionFenced(ctx, db, identity)
	if err != nil {
		return problem.Wrap(500, "kubernetes_pod_deletion_fence_lookup_failed", "The Kubernetes Pod deletion fence could not be inspected.", err)
	}
	if fenced {
		return problem.New(409, "kubernetes_pod_deletion_fenced", "This Kubernetes Pod UID was fenced for deletion and cannot perform Worker operations.")
	}
	return nil
}

func (s *Service) markStaleWorkers(ctx context.Context) error {
	now := s.now()
	cutoff := now.Add(-s.heartbeatTimeout)
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		workers := make([]persistence.WorkerInstance, 0, 100)
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "SKIP LOCKED").
			Where("administrative_status <> ? AND status IN ? AND last_heartbeat_at < ?", "revoked", []string{"online", "draining"}, cutoff).
			Order("last_heartbeat_at, id").Limit(100).Find(&workers).Error; err != nil {
			return err
		}
		// One sweep commonly marks many Workers on the same Execution Target,
		// so the offline outbox scope is resolved once per Target instead of
		// once per Worker.
		targets := make(map[uuid.UUID]persistence.ExecutionTarget, len(workers))
		for _, worker := range workers {
			result := tx.WithContext(ctx).Model(&persistence.WorkerInstance{}).
				Where("id = ? AND administrative_status <> ? AND status = ? AND last_heartbeat_at = ?", worker.ID, "revoked", worker.Status, worker.LastHeartbeatAt).
				Update("status", "offline")
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				continue
			}
			worker.Status = "offline"
			if err := transitionWorkerIncarnationFactLocked(
				ctx, tx, worker, now, workerFactStateOffline, false, "",
			); err != nil {
				return err
			}
			target, cached := targets[worker.ExecutionTargetID]
			if !cached {
				loaded, err := loadWorkerOfflineTarget(ctx, tx, worker.ExecutionTargetID)
				if err != nil {
					return err
				}
				targets[worker.ExecutionTargetID] = loaded
				target = loaded
			}
			if err := enqueueWorkerOfflineForTarget(ctx, tx, worker, target); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return problem.Wrap(500, "worker_sweep_failed", "Failed to update stale workers.", err)
	}
	return nil
}

func enqueueWorkerOffline(ctx context.Context, tx *gorm.DB, worker persistence.WorkerInstance) error {
	target, err := loadWorkerOfflineTarget(ctx, tx, worker.ExecutionTargetID)
	if err != nil {
		return err
	}
	return enqueueWorkerOfflineForTarget(ctx, tx, worker, target)
}

func loadWorkerOfflineTarget(
	ctx context.Context,
	tx *gorm.DB,
	targetID uuid.UUID,
) (persistence.ExecutionTarget, error) {
	var target persistence.ExecutionTarget
	if err := tx.WithContext(ctx).Select("id", "tenant_id", "organization_id").
		Where("id = ?", targetID).Take(&target).Error; err != nil {
		return persistence.ExecutionTarget{}, err
	}
	return target, nil
}

func enqueueWorkerOfflineForTarget(
	ctx context.Context,
	tx *gorm.DB,
	worker persistence.WorkerInstance,
	target persistence.ExecutionTarget,
) error {
	return outbox.Enqueue(ctx, tx, outbox.EnqueueInput{
		TenantID: target.TenantID, Topic: "worker.offline",
		MessageKey: worker.ID.String() + ":" + worker.LastHeartbeatAt.UTC().Format(time.RFC3339Nano),
		Payload: map[string]any{
			"tenantId": target.TenantID, "organizationId": target.OrganizationID,
			"workerId": worker.ID, "executionTargetId": worker.ExecutionTargetID,
			"targetKind": worker.TargetKind, "lastHeartbeatAt": worker.LastHeartbeatAt,
		},
	})
}

func normalizeRegistration(input RegisterWorkerInput) (RegisterWorkerInput, error) {
	protocolVersion := input.ProtocolVersion
	if protocolVersion != WorkerProtocolVersion {
		return RegisterWorkerInput{}, unsupportedWorkerProtocol(protocolVersion)
	}
	workerMode, err := normalizeWorkerMode(input.WorkerMode)
	if err != nil {
		return RegisterWorkerInput{}, err
	}
	instanceUID, err := uuid.Parse(strings.TrimSpace(input.InstanceUID))
	if err != nil || instanceUID == uuid.Nil || instanceUID.String() != strings.TrimSpace(input.InstanceUID) {
		return RegisterWorkerInput{}, problem.New(400, "invalid_worker_registration", "instanceUid must be a canonical lowercase UUID.")
	}
	fields := []struct {
		value   string
		name    string
		maximum int
	}{
		{input.ClusterID, "clusterId", 160}, {input.Namespace, "namespace", 253},
		{input.PodName, "podName", 253}, {input.Version, "version", 160},
	}
	values := make([]string, len(fields))
	for index, field := range fields {
		values[index] = strings.TrimSpace(field.value)
		if values[index] == "" || len(values[index]) > field.maximum || strings.ContainsAny(values[index], "\r\n\t") {
			return RegisterWorkerInput{}, problem.New(400, "invalid_worker_registration", field.name+" is invalid.")
		}
	}
	if input.ExecutionTargetID == uuid.Nil {
		return RegisterWorkerInput{}, problem.New(400, "invalid_worker_registration", "executionTargetId is required.")
	}
	kind, err := platform.ParseExecutionTargetKind(input.TargetKind)
	if err != nil {
		return RegisterWorkerInput{}, problem.New(400, "invalid_worker_registration", "targetKind is invalid.")
	}
	var sshBootstrapGeneration *int64
	if kind == platform.TargetSSH {
		if input.SSHBootstrapGeneration != nil && *input.SSHBootstrapGeneration <= 0 {
			return RegisterWorkerInput{}, problem.New(400, "invalid_worker_registration", "sshBootstrapGeneration must be greater than zero when provided.")
		}
		if input.SSHBootstrapGeneration != nil {
			value := *input.SSHBootstrapGeneration
			sshBootstrapGeneration = &value
		}
	} else if input.SSHBootstrapGeneration != nil {
		return RegisterWorkerInput{}, problem.New(400, "invalid_worker_registration", "sshBootstrapGeneration is only valid for SSH Workers.")
	}
	capabilities := input.Capabilities
	if capabilities == nil {
		capabilities = map[string]any{}
	}
	registrationTrustMode := strings.TrimSpace(input.RegistrationTrustMode)
	if registrationTrustMode == "" {
		registrationTrustMode = WorkerRegistrationTrustSharedToken
	}
	if registrationTrustMode != WorkerRegistrationTrustSharedToken &&
		registrationTrustMode != WorkerRegistrationTrustKubernetesPodBoundV1 {
		return RegisterWorkerInput{}, problem.New(400, "invalid_worker_registration_trust", "Worker registration trust mode is invalid.")
	}
	if registrationTrustMode == WorkerRegistrationTrustKubernetesPodBoundV1 && kind != platform.TargetKubernetes {
		return RegisterWorkerInput{}, problem.New(400, "invalid_worker_registration_trust", "Pod-bound registration trust is only valid for Kubernetes Workers.")
	}
	if registrationTrustMode == WorkerRegistrationTrustKubernetesPodBoundV1 && values[0] != podlifecycle.KubernetesClusterID {
		return RegisterWorkerInput{}, problem.New(400, "invalid_worker_registration_trust", "Pod-bound Kubernetes Workers must use the canonical cluster identity.")
	}
	assignedExecutionID, workerPoolID, workerPoolVersion, capacityClass, err := normalizeWorkerIdentityRegistration(
		workerMode,
		input.AssignedExecutionID,
		input.WorkerPoolID,
		input.WorkerPoolVersion,
		input.CapacityClass,
	)
	if err != nil {
		return RegisterWorkerInput{}, err
	}
	requestedCPU, err := normalizeWorkerRequestedResource(input.RequestedCPUMillicores, "requestedCpuMillicores")
	if err != nil {
		return RegisterWorkerInput{}, err
	}
	requestedMemory, err := normalizeWorkerRequestedResource(input.RequestedMemoryBytes, "requestedMemoryBytes")
	if err != nil {
		return RegisterWorkerInput{}, err
	}
	requestedEphemeral, err := normalizeWorkerRequestedResource(input.RequestedEphemeralStorageBytes, "requestedEphemeralStorageBytes")
	if err != nil {
		return RegisterWorkerInput{}, err
	}
	return RegisterWorkerInput{
		ExecutionTargetID:              input.ExecutionTargetID,
		TargetKind:                     string(kind),
		SSHBootstrapGeneration:         sshBootstrapGeneration,
		WorkerMode:                     workerMode,
		AssignedExecutionID:            assignedExecutionID,
		WorkerPoolID:                   workerPoolID,
		WorkerPoolVersion:              workerPoolVersion,
		CapacityClass:                  capacityClass,
		InstanceUID:                    instanceUID.String(),
		ClusterID:                      values[0],
		Namespace:                      values[1],
		PodName:                        values[2],
		Version:                        values[3],
		ProtocolVersion:                protocolVersion,
		Capabilities:                   capabilities,
		LeaseSupported:                 input.LeaseSupported,
		FencingSupported:               input.FencingSupported,
		RequestedCPUMillicores:         requestedCPU,
		RequestedMemoryBytes:           requestedMemory,
		RequestedEphemeralStorageBytes: requestedEphemeral,
		RegistrationTrustMode:          registrationTrustMode,
	}, nil
}

func normalizeWorkerRequestedResource(value *int64, field string) (*int64, error) {
	if value == nil {
		return nil, nil
	}
	if *value <= 0 {
		return nil, problem.New(400, "invalid_worker_registration", field+" must be positive when present.")
	}
	copy := *value
	return &copy, nil
}

func normalizeWorkerIdentityRegistration(
	workerMode string,
	assignedExecutionID, workerPoolID *uuid.UUID,
	workerPoolVersion *int64,
	capacityClass *string,
) (*uuid.UUID, *uuid.UUID, *int64, *string, error) {
	if assignedExecutionID != nil && *assignedExecutionID == uuid.Nil {
		return nil, nil, nil, nil, problem.New(400, "invalid_worker_registration", "assignedExecutionId must not be an empty UUID.")
	}
	if workerPoolID != nil && *workerPoolID == uuid.Nil {
		return nil, nil, nil, nil, problem.New(400, "invalid_worker_registration", "workerPoolId must not be an empty UUID.")
	}
	if workerPoolVersion != nil && *workerPoolVersion <= 0 {
		return nil, nil, nil, nil, problem.New(400, "invalid_worker_registration", "workerPoolVersion must be positive.")
	}
	var normalizedCapacityClass *string
	if capacityClass != nil {
		value := strings.ToLower(strings.TrimSpace(*capacityClass))
		switch value {
		case "standard", "interactive":
			normalizedCapacityClass = &value
		default:
			return nil, nil, nil, nil, problem.New(400, "invalid_worker_registration", "capacityClass is invalid.")
		}
	}
	hasPoolIdentity := workerPoolID != nil || workerPoolVersion != nil || normalizedCapacityClass != nil
	switch workerMode {
	case WorkerModeExecutionPinned:
		if assignedExecutionID == nil {
			return nil, nil, nil, nil, problem.New(400, "invalid_worker_registration", "execution-pinned workers must register an assignedExecutionId.")
		}
		if hasPoolIdentity {
			return nil, nil, nil, nil, problem.New(400, "invalid_worker_registration", "execution-pinned workers cannot register a Worker pool identity.")
		}
		return assignedExecutionID, nil, nil, nil, nil
	case WorkerModeWarmPool:
		if assignedExecutionID != nil {
			return nil, nil, nil, nil, problem.New(400, "invalid_worker_registration", "warm-pool workers cannot register an assignedExecutionId.")
		}
		if workerPoolID == nil || workerPoolVersion == nil || normalizedCapacityClass == nil {
			return nil, nil, nil, nil, problem.New(400, "invalid_worker_registration", "warm-pool workers must register workerPoolId, workerPoolVersion, and capacityClass together.")
		}
		return nil, workerPoolID, workerPoolVersion, normalizedCapacityClass, nil
	default:
		if assignedExecutionID != nil || hasPoolIdentity {
			return nil, nil, nil, nil, problem.New(400, "invalid_worker_registration", "general-pool workers cannot register assigned execution or Worker pool identity.")
		}
		return nil, nil, nil, nil, nil
	}
}

func validateRegisteredWorkerIdentity(
	ctx context.Context,
	tx *gorm.DB,
	input RegisterWorkerInput,
) error {
	if input.AssignedExecutionID != nil {
		var count int64
		if err := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
			Where("id = ? AND execution_target_id = ? AND target_kind = ?", *input.AssignedExecutionID, input.ExecutionTargetID, input.TargetKind).
			Count(&count).Error; err != nil {
			return problem.Wrap(500, "worker_registration_identity_lookup_failed", "The assigned execution identity could not be verified.", err)
		}
		if count != 1 {
			return problem.New(409, "worker_registration_assignment_mismatch", "The assigned execution identity does not belong to this execution target.")
		}
	}
	if input.WorkerPoolID == nil {
		return nil
	}
	var count int64
	if err := tx.WithContext(ctx).Model(&persistence.WorkerPool{}).
		Where(
			"id = ? AND execution_target_id = ? AND mode = ? AND status = ? AND version = ? AND capacity_class = ?",
			*input.WorkerPoolID, input.ExecutionTargetID, "warm", "active", *input.WorkerPoolVersion, *input.CapacityClass,
		).
		Count(&count).Error; err != nil {
		return problem.Wrap(500, "worker_registration_identity_lookup_failed", "The Worker pool identity could not be verified.", err)
	}
	if count != 1 {
		return problem.New(409, "worker_registration_pool_identity_mismatch", "The Worker pool identity does not match an active warm-pool snapshot on this execution target.")
	}
	return nil
}

func unsupportedWorkerProtocol(received int) *problem.Error {
	err := problem.New(426, "worker_protocol_version_unsupported", "Worker Protocol v2 is required; upgrade synara-agentd.")
	err.Details = map[string]any{
		"received": received, "minimumSupported": WorkerProtocolVersion, "maximumSupported": WorkerProtocolVersion,
	}
	return err
}
