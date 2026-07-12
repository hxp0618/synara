package retention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/artifacts"
	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

type Policy struct {
	TenantID                uuid.UUID `json:"tenantId"`
	SessionArchiveAfterDays *int      `json:"sessionArchiveAfterDays"`
	ArtifactDeleteAfterDays *int      `json:"artifactDeleteAfterDays"`
	UpdatedBy               uuid.UUID `json:"updatedBy"`
	CreatedAt               time.Time `json:"createdAt"`
	UpdatedAt               time.Time `json:"updatedAt"`
}

type UpdateInput struct {
	SessionArchiveAfterDays *int `json:"sessionArchiveAfterDays"`
	ArtifactDeleteAfterDays *int `json:"artifactDeleteAfterDays"`
}

type Service struct {
	db         *gorm.DB
	authorizer *authorization.Authorizer
	sessions   *sessions.Service
	artifacts  *artifacts.Service
	interval   time.Duration
	logger     *slog.Logger
	now        func() time.Time
	observer   backgroundObserver
}

type backgroundObserver interface {
	ObserveBackground(kind string, started time.Time, err error)
}

func NewService(
	db *gorm.DB,
	sessionService *sessions.Service,
	artifactService *artifacts.Service,
	interval time.Duration,
	logger *slog.Logger,
	observers ...backgroundObserver,
) *Service {
	service := &Service{
		db: db, authorizer: authorization.NewAuthorizer(db), sessions: sessionService,
		artifacts: artifactService, interval: interval, logger: logger,
		now: func() time.Time { return time.Now().UTC() },
	}
	if len(observers) > 0 {
		service.observer = observers[0]
	}
	return service
}

func (s *Service) Get(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
) (Policy, error) {
	if err := requireActiveTenant(principal, tenantID); err != nil {
		return Policy{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.RetentionRead); err != nil {
		return Policy{}, err
	}
	var model persistence.TenantRetentionPolicy
	err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Take(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Policy{TenantID: tenantID}, nil
	}
	if err != nil {
		return Policy{}, problem.Wrap(500, "retention_policy_load_failed", "Retention policy could not be loaded.", err)
	}
	return toPolicy(model), nil
}

func (s *Service) Update(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	input UpdateInput,
	requestID, ipAddress string,
) (Policy, error) {
	if err := requireActiveTenant(principal, tenantID); err != nil {
		return Policy{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.RetentionManage); err != nil {
		return Policy{}, err
	}
	if err := validateDays(input.SessionArchiveAfterDays); err != nil {
		return Policy{}, problem.New(400, "invalid_session_retention", err.Error())
	}
	if err := validateDays(input.ArtifactDeleteAfterDays); err != nil {
		return Policy{}, problem.New(400, "invalid_artifact_retention", err.Error())
	}
	model := persistence.TenantRetentionPolicy{
		TenantID: tenantID, SessionArchiveAfterDays: input.SessionArchiveAfterDays,
		ArtifactDeleteAfterDays: input.ArtifactDeleteAfterDays, UpdatedBy: principal.UserID,
	}
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "tenant_id"}},
			DoUpdates: clause.Assignments(map[string]any{
				"session_archive_after_days": input.SessionArchiveAfterDays,
				"artifact_delete_after_days": input.ArtifactDeleteAfterDays,
				"updated_by":                 principal.UserID,
			}),
		}).Create(&model).Error; err != nil {
			return problem.Wrap(409, "retention_policy_update_rejected", "Retention policy update was rejected.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "retention_policy.updated", ResourceType: "tenant_retention_policy", ResourceID: &tenantID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"sessionArchiveAfterDays": input.SessionArchiveAfterDays,
				"artifactDeleteAfterDays": input.ArtifactDeleteAfterDays,
			},
		})
	})
	if err != nil {
		return Policy{}, err
	}
	return s.Get(ctx, principal, tenantID)
}

func (s *Service) Run(ctx context.Context) {
	interval := s.interval
	if interval <= 0 {
		interval = time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		started := time.Now()
		err := s.RunOnce(ctx, 200)
		if s.observer != nil {
			s.observer.ObserveBackground("retention", started, err)
		}
		if err != nil && ctx.Err() == nil {
			s.logger.Error("retention sweep failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) RunOnce(ctx context.Context, limit int) error {
	release, acquired, err := persistence.TryAdvisoryLock(ctx, s.db, "synara:tenant-retention-sweeper")
	if err != nil {
		return problem.Wrap(500, "retention_lock_failed", "Retention sweep coordination failed.", err)
	}
	if !acquired {
		return nil
	}
	defer release()
	var policies []persistence.TenantRetentionPolicy
	if err := s.db.WithContext(ctx).Table("tenant_retention_policies AS p").
		Select("p.*").Joins("JOIN tenants AS t ON t.id = p.tenant_id").
		Where("t.deleted_at IS NULL").Order("p.tenant_id").Find(&policies).Error; err != nil {
		return problem.Wrap(500, "retention_policies_load_failed", "Retention policies could not be loaded.", err)
	}
	now := s.now()
	var failures []error
	for _, policy := range policies {
		if err := s.applyPolicy(ctx, policy, now, limit); err != nil {
			failures = append(failures, fmt.Errorf("tenant %s: %w", policy.TenantID, err))
		}
	}
	if err := s.cleanupEphemeralRecords(ctx, now, limit); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func (s *Service) cleanupEphemeralRecords(ctx context.Context, now time.Time, limit int) error {
	if limit <= 0 {
		limit = 200
	}
	longLivedCutoff := now.Add(-30 * 24 * time.Hour)
	jobs := []struct {
		name      string
		model     any
		condition string
		args      []any
	}{
		{name: "login sessions", model: &persistence.LoginSession{}, condition: "expires_at <= ? OR (revoked_at IS NOT NULL AND revoked_at <= ?)", args: []any{longLivedCutoff, longLivedCutoff}},
		{name: "Artifact access tokens", model: &persistence.ArtifactAccessToken{}, condition: "expires_at <= ?", args: []any{now}},
		{name: "identity login attempts", model: &persistence.IdentityLoginAttempt{}, condition: "expires_at <= ? OR (consumed_at IS NOT NULL AND consumed_at <= ?)", args: []any{now, now.Add(-time.Hour)}},
		{name: "Service Account tokens", model: &persistence.ServiceAccountToken{}, condition: "(expires_at IS NOT NULL AND expires_at <= ?) OR (revoked_at IS NOT NULL AND revoked_at <= ?)", args: []any{longLivedCutoff, longLivedCutoff}},
		{name: "Tenant invitations", model: &persistence.TenantInvitation{}, condition: "expires_at <= ?", args: []any{longLivedCutoff}},
	}
	var failures []error
	for _, job := range jobs {
		if err := deleteUUIDBatch(ctx, s.db, job.model, job.condition, job.args, limit); err != nil {
			failures = append(failures, fmt.Errorf("delete expired %s: %w", job.name, err))
		}
	}
	var receipts []persistence.WorkerRequestReceipt
	if err := s.db.WithContext(ctx).Where("expires_at <= ?", now).Order("expires_at").Limit(limit).Find(&receipts).Error; err != nil {
		failures = append(failures, fmt.Errorf("load expired Worker receipts: %w", err))
	} else {
		for _, receipt := range receipts {
			if err := s.db.WithContext(ctx).Where("worker_id = ? AND request_id = ?", receipt.WorkerID, receipt.RequestID).Delete(&persistence.WorkerRequestReceipt{}).Error; err != nil {
				failures = append(failures, fmt.Errorf("delete expired Worker receipt: %w", err))
				break
			}
		}
	}
	return errors.Join(failures...)
}

func deleteUUIDBatch(ctx context.Context, db *gorm.DB, model any, condition string, args []any, limit int) error {
	var ids []uuid.UUID
	if err := db.WithContext(ctx).Model(model).Select("id").Where(condition, args...).Order("id").Limit(limit).Scan(&ids).Error; err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	return db.WithContext(ctx).Where("id IN ?", ids).Delete(model).Error
}

func (s *Service) applyPolicy(
	ctx context.Context,
	policy persistence.TenantRetentionPolicy,
	now time.Time,
	limit int,
) error {
	archived, deleted := 0, 0
	var failures []error
	metadata := map[string]any{}
	if policy.SessionArchiveAfterDays != nil {
		cutoff := now.AddDate(0, 0, -*policy.SessionArchiveAfterDays)
		count, err := s.sessions.ArchiveByRetention(ctx, policy.TenantID, cutoff, now, limit)
		archived = count
		metadata["sessionCutoff"] = cutoff
		if err != nil {
			failures = append(failures, err)
		}
	}
	if policy.ArtifactDeleteAfterDays != nil {
		cutoff := now.AddDate(0, 0, -*policy.ArtifactDeleteAfterDays)
		count, err := s.artifacts.DeleteByRetention(ctx, policy.TenantID, cutoff, now, limit)
		deleted = count
		metadata["artifactCutoff"] = cutoff
		if err != nil {
			failures = append(failures, err)
		}
	}
	if archived > 0 || deleted > 0 {
		metadata["sessionsArchived"] = archived
		metadata["artifactsDeleted"] = deleted
		if err := audit.Record(ctx, s.db, audit.Entry{
			TenantID: policy.TenantID, ActorType: "system",
			Action: "retention_policy.applied", ResourceType: "tenant_retention_policy", ResourceID: &policy.TenantID,
			RequestID: "retention-sweep:" + uuid.NewString(), Metadata: metadata,
		}); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func validateDays(value *int) error {
	if value != nil && (*value < 1 || *value > 36500) {
		return errors.New("retention days must be between 1 and 36500 or null")
	}
	return nil
}

func requireActiveTenant(principal identity.Principal, tenantID uuid.UUID) error {
	if principal.ActiveTenantID == nil || *principal.ActiveTenantID != tenantID {
		return problem.New(404, "tenant_not_found", "Tenant not found.")
	}
	return nil
}

func toPolicy(model persistence.TenantRetentionPolicy) Policy {
	return Policy{
		TenantID: model.TenantID, SessionArchiveAfterDays: model.SessionArchiveAfterDays,
		ArtifactDeleteAfterDays: model.ArtifactDeleteAfterDays, UpdatedBy: model.UpdatedBy,
		CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt,
	}
}
