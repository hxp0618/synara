package executions

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/outbox"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/projects"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

const defaultProviderCursorMaximumAge = 30 * 24 * time.Hour

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
	db                       *gorm.DB
	authorizer               *authorization.Authorizer
	sessions                 *sessions.Service
	leaseTTL                 time.Duration
	heartbeatTimeout         time.Duration
	receiptTTL               time.Duration
	cursorCipher             *secret.CursorCipher
	providerCursorMaximumAge time.Duration
	targets                  *executiontargets.Service
	projects                 *projects.Service
	now                      func() time.Time
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
		targets: targetService, now: func() time.Time { return time.Now().UTC() },
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
	target, targetKind, err := s.targets.ResolveWorkerTarget(ctx, normalized.ExecutionTargetID, normalized.TargetKind)
	if err != nil {
		return RegisteredWorker{}, err
	}
	if platform.IsRemoteTarget(targetKind) && (!normalized.LeaseSupported || !normalized.FencingSupported) {
		return RegisteredWorker{}, problem.New(400, "remote_worker_protocol_required", "Remote workers must advertise leaseSupported and fencingSupported.")
	}
	plainToken, tokenHash, err := secret.NewToken()
	if err != nil {
		return RegisteredWorker{}, problem.Wrap(500, "worker_token_generation_failed", "Failed to generate a worker credential.", err)
	}
	now := s.now()
	var model persistence.WorkerInstance
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("execution_target_id = ? AND cluster_id = ? AND namespace = ? AND pod_name = ? AND status <> ?", normalized.ExecutionTargetID, normalized.ClusterID, normalized.Namespace, normalized.PodName, "terminated").
			Take(&model).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			model = persistence.WorkerInstance{
				ID: uuid.New(), Incarnation: 1, InstanceUID: normalized.InstanceUID,
				ExecutionTargetID: normalized.ExecutionTargetID, TargetKind: normalized.TargetKind,
				ClusterID: normalized.ClusterID,
				Namespace: normalized.Namespace, PodName: normalized.PodName, Version: normalized.Version,
				ProtocolVersion: normalized.ProtocolVersion,
				Capabilities:    normalized.Capabilities, LeaseSupported: normalized.LeaseSupported,
				FencingSupported: normalized.FencingSupported, AuthTokenHash: tokenHash, Status: "online",
				CompatibilityStatus: "unknown",
				RegisteredAt:        now, LastHeartbeatAt: now,
			}
			if err := tx.WithContext(ctx).Create(&model).Error; err != nil {
				return problem.Wrap(409, "worker_registration_conflict", "Worker registration conflicts with an active instance.", err)
			}
			return persistWorkerManifest(
				ctx, tx, &model, normalized.Version, normalized.Capabilities, target.Capabilities, targetKind, now,
			)
		}
		if err != nil {
			return problem.Wrap(500, "worker_registration_lookup_failed", "Failed to resolve the worker registration.", err)
		}
		updates := persistence.WorkerInstance{
			Incarnation: model.Incarnation + 1, InstanceUID: normalized.InstanceUID,
			TargetKind: normalized.TargetKind, Version: normalized.Version, ProtocolVersion: normalized.ProtocolVersion,
			Capabilities: normalized.Capabilities, AuthTokenHash: tokenHash,
			LeaseSupported: normalized.LeaseSupported, FencingSupported: normalized.FencingSupported,
			Status: "online", LastHeartbeatAt: now,
		}
		result := tx.WithContext(ctx).Model(&persistence.WorkerInstance{}).
			Where("id = ?", model.ID).
			Select(
				"incarnation", "instance_uid", "target_kind", "version", "protocol_version", "capabilities", "auth_token_hash", "lease_supported",
				"fencing_supported", "status", "last_heartbeat_at", "draining_at", "terminated_at",
			).Updates(&updates)
		if err := expectOne(result, 500, "worker_registration_update_failed", "Failed to refresh the worker registration."); err != nil {
			return err
		}
		model.ExecutionTargetID = normalized.ExecutionTargetID
		model.Incarnation = updates.Incarnation
		model.InstanceUID = normalized.InstanceUID
		model.TargetKind = normalized.TargetKind
		model.Version = normalized.Version
		model.ProtocolVersion = normalized.ProtocolVersion
		model.Capabilities = normalized.Capabilities
		model.LeaseSupported = normalized.LeaseSupported
		model.FencingSupported = normalized.FencingSupported
		model.AuthTokenHash = tokenHash
		model.Status = "online"
		model.LastHeartbeatAt = now
		model.DrainingAt = nil
		model.TerminatedAt = nil
		return persistWorkerManifest(
			ctx, tx, &model, normalized.Version, normalized.Capabilities, target.Capabilities, targetKind, now,
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
		Where("auth_token_hash = ? AND status <> ?", secret.HashToken(plainToken), "terminated").
		Take(&worker).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.WorkerInstance{}, problem.New(401, "invalid_worker_token", "The worker bearer token is invalid.")
	}
	if err != nil {
		return persistence.WorkerInstance{}, problem.Wrap(500, "worker_authentication_failed", "Worker authentication failed.", err)
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
	var currentWorkerCount int64
	if err := s.db.WithContext(ctx).Model(&persistence.WorkerInstance{}).
		Where(
			"id = ? AND incarnation = ? AND instance_uid = ? AND auth_token_hash = ? AND status <> ?",
			worker.ID, worker.Incarnation, worker.InstanceUID, worker.AuthTokenHash, "terminated",
		).
		Count(&currentWorkerCount).Error; err != nil {
		return Worker{}, problem.Wrap(500, "worker_heartbeat_failed", "Failed to validate the worker heartbeat.", err)
	}
	if currentWorkerCount != 1 {
		return Worker{}, problem.New(409, "worker_incarnation_fenced", "The Worker registration is no longer current.")
	}
	now := s.now()
	if input.Capabilities != nil {
		target, targetKind, err := s.targets.ResolveWorkerTarget(ctx, worker.ExecutionTargetID, worker.TargetKind)
		if err != nil {
			return Worker{}, err
		}
		manifestVersion := worker.Version
		if version := strings.TrimSpace(input.Version); version != "" {
			manifestVersion = version
		}
		normalized, normalizeErr := normalizeWorkerManifest(
			manifestVersion, input.Capabilities, target.Capabilities, targetKind, now,
		)
		if normalizeErr != nil {
			return Worker{}, workerManifestReregistrationRequired(normalizeErr.Error())
		}
		matches, matchErr := workerManifestMatches(ctx, s.db, worker, normalized)
		if matchErr != nil {
			return Worker{}, matchErr
		}
		if !matches {
			return Worker{}, workerManifestReregistrationRequired("Heartbeat Provider capabilities differ from the immutable registered Worker manifest.")
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
	} else if worker.Status == "offline" {
		updates.Status = "online"
		fields = append(fields, "status")
	}
	if version := strings.TrimSpace(input.Version); version != "" {
		if len(version) > 160 {
			return Worker{}, problem.New(400, "invalid_worker_version", "Worker version must not exceed 160 characters.")
		}
		updates.Version = version
		fields = append(fields, "version")
	}
	if input.Capabilities != nil {
		updates.Capabilities = input.Capabilities
		fields = append(fields, "capabilities")
	}
	result := s.db.WithContext(ctx).Model(&persistence.WorkerInstance{}).
		Where(
			"id = ? AND incarnation = ? AND instance_uid = ? AND status <> ?",
			worker.ID, worker.Incarnation, worker.InstanceUID, "terminated",
		).
		Select(fields).Updates(&updates)
	if result.Error != nil {
		return Worker{}, problem.Wrap(500, "worker_heartbeat_failed", "Failed to record the worker heartbeat.", result.Error)
	}
	if result.RowsAffected != 1 {
		return Worker{}, problem.New(409, "worker_incarnation_fenced", "The Worker registration is no longer current.")
	}
	if err := s.db.WithContext(ctx).
		Where(
			"id = ? AND incarnation = ? AND instance_uid = ? AND status <> ?",
			worker.ID, worker.Incarnation, worker.InstanceUID, "terminated",
		).
		Take(&worker).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return Worker{}, problem.New(409, "worker_incarnation_fenced", "The Worker registration is no longer current.")
	} else if err != nil {
		return Worker{}, problem.Wrap(500, "worker_reload_failed", "Failed to reload the worker.", err)
	}
	return toWorker(worker), nil
}

func lockCurrentWorkerIncarnation(ctx context.Context, tx *gorm.DB, worker persistence.WorkerInstance) error {
	var current persistence.WorkerInstance
	err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Select("id", "incarnation", "instance_uid", "status").
		Where("id = ? AND status <> ?", worker.ID, "terminated").Take(&current).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return problem.New(409, "worker_incarnation_fenced", "The Worker registration is no longer current.")
	}
	if err != nil {
		return problem.Wrap(500, "worker_incarnation_lock_failed", "Failed to lock the current Worker registration.", err)
	}
	if current.Incarnation != worker.Incarnation || current.InstanceUID != worker.InstanceUID {
		return problem.New(409, "worker_incarnation_fenced", "The Worker registration is no longer current.")
	}
	return nil
}

func (s *Service) markStaleWorkers(ctx context.Context) error {
	cutoff := s.now().Add(-s.heartbeatTimeout)
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		workers := make([]persistence.WorkerInstance, 0, 100)
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "SKIP LOCKED").
			Where("status IN ? AND last_heartbeat_at < ?", []string{"online", "draining"}, cutoff).
			Order("last_heartbeat_at, id").Limit(100).Find(&workers).Error; err != nil {
			return err
		}
		for _, worker := range workers {
			result := tx.WithContext(ctx).Model(&persistence.WorkerInstance{}).
				Where("id = ? AND status = ? AND last_heartbeat_at = ?", worker.ID, worker.Status, worker.LastHeartbeatAt).
				Update("status", "offline")
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				continue
			}
			if err := enqueueWorkerOffline(ctx, tx, worker); err != nil {
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
	var target persistence.ExecutionTarget
	if err := tx.WithContext(ctx).Select("id", "tenant_id", "organization_id").Where("id = ?", worker.ExecutionTargetID).Take(&target).Error; err != nil {
		return err
	}
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
	capabilities := input.Capabilities
	if capabilities == nil {
		capabilities = map[string]any{}
	}
	return RegisterWorkerInput{
		ExecutionTargetID: input.ExecutionTargetID, TargetKind: string(kind),
		InstanceUID: instanceUID.String(),
		ClusterID:   values[0], Namespace: values[1], PodName: values[2], Version: values[3],
		ProtocolVersion: protocolVersion,
		Capabilities:    capabilities, LeaseSupported: input.LeaseSupported,
		FencingSupported: input.FencingSupported,
	}, nil
}

func unsupportedWorkerProtocol(received int) *problem.Error {
	err := problem.New(426, "worker_protocol_version_unsupported", "Worker Protocol v2 is required; upgrade synara-agentd.")
	err.Details = map[string]any{
		"received": received, "minimumSupported": WorkerProtocolVersion, "maximumSupported": WorkerProtocolVersion,
	}
	return err
}
