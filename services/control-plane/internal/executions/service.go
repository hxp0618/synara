package executions

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

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
	db               *gorm.DB
	sessions         *sessions.Service
	leaseTTL         time.Duration
	heartbeatTimeout time.Duration
	receiptTTL       time.Duration
	cursorCipher     *secret.CursorCipher
	targets          *executiontargets.Service
	now              func() time.Time
}

func NewService(
	db *gorm.DB,
	sessionService *sessions.Service,
	leaseTTL, heartbeatTimeout, receiptTTL time.Duration,
	cursorCipher *secret.CursorCipher,
	targetService *executiontargets.Service,
) *Service {
	return &Service{
		db: db, sessions: sessionService, leaseTTL: leaseTTL,
		heartbeatTimeout: heartbeatTimeout, receiptTTL: receiptTTL,
		cursorCipher: cursorCipher, targets: targetService, now: func() time.Time { return time.Now().UTC() },
	}
}

func (s *Service) Register(ctx context.Context, input RegisterWorkerInput) (RegisteredWorker, error) {
	normalized, err := normalizeRegistration(input)
	if err != nil {
		return RegisteredWorker{}, err
	}
	_, targetKind, err := s.targets.ResolveWorkerTarget(ctx, normalized.ExecutionTargetID, normalized.TargetKind)
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
				ID: uuid.New(), ExecutionTargetID: normalized.ExecutionTargetID, TargetKind: normalized.TargetKind,
				ClusterID: normalized.ClusterID,
				Namespace: normalized.Namespace, PodName: normalized.PodName, Version: normalized.Version,
				Capabilities: normalized.Capabilities, LeaseSupported: normalized.LeaseSupported,
				FencingSupported: normalized.FencingSupported, AuthTokenHash: tokenHash, Status: "online",
				RegisteredAt: now, LastHeartbeatAt: now,
			}
			if err := tx.WithContext(ctx).Create(&model).Error; err != nil {
				return problem.Wrap(409, "worker_registration_conflict", "Worker registration conflicts with an active instance.", err)
			}
			return nil
		}
		if err != nil {
			return problem.Wrap(500, "worker_registration_lookup_failed", "Failed to resolve the worker registration.", err)
		}
		updates := persistence.WorkerInstance{
			TargetKind: normalized.TargetKind, Version: normalized.Version,
			Capabilities: normalized.Capabilities, AuthTokenHash: tokenHash,
			LeaseSupported: normalized.LeaseSupported, FencingSupported: normalized.FencingSupported,
			Status: "online", LastHeartbeatAt: now,
		}
		result := tx.WithContext(ctx).Model(&persistence.WorkerInstance{}).
			Where("id = ?", model.ID).
			Select(
				"target_kind", "version", "capabilities", "auth_token_hash", "lease_supported",
				"fencing_supported", "status", "last_heartbeat_at", "draining_at", "terminated_at",
			).Updates(&updates)
		if err := expectOne(result, 500, "worker_registration_update_failed", "Failed to refresh the worker registration."); err != nil {
			return err
		}
		model.ExecutionTargetID = normalized.ExecutionTargetID
		model.TargetKind = normalized.TargetKind
		model.Version = normalized.Version
		model.Capabilities = normalized.Capabilities
		model.LeaseSupported = normalized.LeaseSupported
		model.FencingSupported = normalized.FencingSupported
		model.AuthTokenHash = tokenHash
		model.Status = "online"
		model.LastHeartbeatAt = now
		model.DrainingAt = nil
		model.TerminatedAt = nil
		return nil
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
	now := s.now()
	updates := persistence.WorkerInstance{LastHeartbeatAt: now}
	fields := []string{"last_heartbeat_at"}
	if worker.Status == "offline" {
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
		Where("id = ? AND status <> ?", worker.ID, "terminated").Select(fields).Updates(&updates)
	if result.Error != nil {
		return Worker{}, problem.Wrap(500, "worker_heartbeat_failed", "Failed to record the worker heartbeat.", result.Error)
	}
	if result.RowsAffected != 1 {
		return Worker{}, problem.New(401, "worker_not_active", "The worker is no longer active.")
	}
	if err := s.db.WithContext(ctx).Where("id = ?", worker.ID).Take(&worker).Error; err != nil {
		return Worker{}, problem.Wrap(500, "worker_reload_failed", "Failed to reload the worker.", err)
	}
	return toWorker(worker), nil
}

func (s *Service) markStaleWorkers(ctx context.Context) error {
	cutoff := s.now().Add(-s.heartbeatTimeout)
	err := s.db.WithContext(ctx).Model(&persistence.WorkerInstance{}).
		Where("status = ? AND last_heartbeat_at < ?", "online", cutoff).
		Update("status", "offline").Error
	if err != nil {
		return problem.Wrap(500, "worker_sweep_failed", "Failed to update stale workers.", err)
	}
	return nil
}

func normalizeRegistration(input RegisterWorkerInput) (RegisterWorkerInput, error) {
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
		ClusterID: values[0], Namespace: values[1], PodName: values[2], Version: values[3],
		Capabilities: capabilities, LeaseSupported: input.LeaseSupported,
		FencingSupported: input.FencingSupported,
	}, nil
}
