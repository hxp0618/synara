package supportaccess

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/databasetime"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/productprofile"
)

const (
	minimumDurationSeconds = 5 * 60
	maximumDurationSeconds = 4 * 60 * 60
)

type Policy struct {
	TenantID             uuid.UUID  `json:"tenantId"`
	SupportAccessEnabled bool       `json:"supportAccessEnabled"`
	Version              int64      `json:"version"`
	Reason               string     `json:"reason"`
	UpdatedBy            *uuid.UUID `json:"updatedBy"`
	CreatedAt            *time.Time `json:"createdAt"`
	UpdatedAt            *time.Time `json:"updatedAt"`
}

type UpdatePolicyInput struct {
	SupportAccessEnabled bool   `json:"supportAccessEnabled"`
	ExpectedVersion      int64  `json:"expectedVersion"`
	Reason               string `json:"reason"`
}

type RequestInput struct {
	TenantID                 uuid.UUID `json:"tenantId"`
	Reason                   string    `json:"reason"`
	RequestedDurationSeconds int       `json:"requestedDurationSeconds"`
}

type DecisionInput struct {
	ExpectedVersion int64  `json:"expectedVersion"`
	Reason          string `json:"reason"`
}

type Grant struct {
	ID                       uuid.UUID  `json:"id"`
	TenantID                 uuid.UUID  `json:"tenantId"`
	TenantName               string     `json:"tenantName"`
	RequesterUserID          uuid.UUID  `json:"requesterUserId"`
	RequesterEmail           string     `json:"requesterEmail"`
	RequesterDisplayName     string     `json:"requesterDisplayName"`
	Status                   string     `json:"status"`
	Version                  int64      `json:"version"`
	Reason                   string     `json:"reason"`
	RequestedDurationSeconds int        `json:"requestedDurationSeconds"`
	RequestedAt              time.Time  `json:"requestedAt"`
	DecidedBy                *uuid.UUID `json:"decidedBy"`
	DecisionReason           *string    `json:"decisionReason"`
	DecidedAt                *time.Time `json:"decidedAt"`
	ExpiresAt                *time.Time `json:"expiresAt"`
	RevokedBy                *uuid.UUID `json:"revokedBy"`
	RevocationReason         *string    `json:"revocationReason"`
	RevokedAt                *time.Time `json:"revokedAt"`
	CreatedAt                time.Time  `json:"createdAt"`
	UpdatedAt                time.Time  `json:"updatedAt"`
}

type TenantOverview struct {
	ID                              uuid.UUID  `json:"id"`
	Slug                            string     `json:"slug"`
	Name                            string     `json:"name"`
	Status                          string     `json:"status"`
	PlanCode                        string     `json:"entitlementProfileCode"`
	Region                          string     `json:"region"`
	ActiveMemberCount               int64      `json:"activeMemberCount"`
	OrganizationCount               int64      `json:"organizationCount"`
	SessionCount                    int64      `json:"sessionCount"`
	ExecutionTargetCount            int64      `json:"executionTargetCount"`
	WorkerCount                     int64      `json:"workerCount"`
	OfflineWorkerCount              int64      `json:"offlineWorkerCount"`
	ActiveExecutionCount            int64      `json:"activeExecutionCount"`
	QueuedExecutionCount            int64      `json:"queuedExecutionCount"`
	OldestQueuedAt                  *time.Time `json:"oldestQueuedAt" gorm:"-"`
	FailedExecutionCount24h         int64      `json:"failedExecutionCount24h"`
	ArtifactCount                   int64      `json:"artifactCount"`
	ArtifactBytes                   int64      `json:"artifactBytes"`
	PendingArtifactCount            int64      `json:"pendingArtifactCount"`
	ActiveCredentialCount           int64      `json:"activeCredentialCount"`
	UnavailableCredentialCount      int64      `json:"unavailableCredentialCount"`
	ActiveIdentityConnectionCount   int64      `json:"activeIdentityConnectionCount"`
	DisabledIdentityConnectionCount int64      `json:"disabledIdentityConnectionCount"`
}

type PlatformTenantOverview struct {
	OperatorRole string           `json:"operatorRole"`
	GeneratedAt  time.Time        `json:"generatedAt"`
	Items        []TenantOverview `json:"items"`
}

type Service struct {
	db               *gorm.DB
	authorizer       *authorization.Authorizer
	operatorTenantID uuid.UUID
	now              func() time.Time
}

func NewService(db *gorm.DB, operatorTenantID uuid.UUID) *Service {
	return &Service{
		db: db, authorizer: authorization.NewAuthorizer(db), operatorTenantID: operatorTenantID,
		now: func() time.Time { return time.Now().UTC() },
	}
}

func (s *Service) GetPolicy(ctx context.Context, principal identity.Principal, tenantID uuid.UUID) (Policy, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return Policy{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.TenantRead); err != nil {
		return Policy{}, err
	}
	var model persistence.TenantSupportPolicy
	err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Take(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Policy{TenantID: tenantID, Version: 0}, nil
	}
	if err != nil {
		return Policy{}, problem.Wrap(500, "tenant_support_policy_load_failed", "Tenant Support policy could not be loaded.", err)
	}
	return policyView(model), nil
}

func (s *Service) UpdatePolicy(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	input UpdatePolicyInput,
	requestID, ipAddress string,
) (Policy, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return Policy{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.TenantUpdate); err != nil {
		return Policy{}, err
	}
	reason, err := normalizeReason(input.Reason)
	if err != nil {
		return Policy{}, err
	}
	var result persistence.TenantSupportPolicy
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var current persistence.TenantSupportPolicy
		loadErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ?", tenantID).Take(&current).Error
		if errors.Is(loadErr, gorm.ErrRecordNotFound) {
			if input.ExpectedVersion != 0 {
				return versionConflict("tenant_support_policy_version_conflict", "Tenant Support policy changed; reload it before saving.")
			}
			current = persistence.TenantSupportPolicy{
				TenantID: tenantID, SupportAccessEnabled: input.SupportAccessEnabled,
				Version: 1, Reason: reason, UpdatedBy: principal.UserID,
			}
			if err := tx.WithContext(ctx).Create(&current).Error; err != nil {
				return problem.Wrap(409, "tenant_support_policy_create_rejected", "Tenant Support policy could not be created.", err)
			}
		} else if loadErr != nil {
			return problem.Wrap(500, "tenant_support_policy_load_failed", "Tenant Support policy could not be loaded.", loadErr)
		} else {
			if input.ExpectedVersion != current.Version {
				return versionConflict("tenant_support_policy_version_conflict", "Tenant Support policy changed; reload it before saving.")
			}
			updated := tx.WithContext(ctx).Model(&persistence.TenantSupportPolicy{}).
				Where("tenant_id = ? AND version = ?", tenantID, current.Version).
				Updates(map[string]any{
					"support_access_enabled": input.SupportAccessEnabled, "version": current.Version + 1,
					"reason": reason, "updated_by": principal.UserID,
				})
			if updated.Error != nil || updated.RowsAffected != 1 {
				return problem.Wrap(409, "tenant_support_policy_version_conflict", "Tenant Support policy changed; reload it before saving.", updated.Error)
			}
			current.SupportAccessEnabled = input.SupportAccessEnabled
			current.Version++
			current.Reason = reason
			current.UpdatedBy = principal.UserID
		}
		if !input.SupportAccessEnabled {
			if err := s.expireActiveGrantsTx(ctx, tx, s.now()); err != nil {
				return err
			}
			if err := s.revokeActiveGrantsForPolicy(
				ctx, tx, principal.UserID, tenantID, reason, requestID, ipAddress, s.now(),
			); err != nil {
				return err
			}
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "support.policy_updated", ResourceType: "tenant_support_policy", ResourceID: &tenantID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"supportAccessEnabled": input.SupportAccessEnabled, "version": current.Version, "reason": reason,
			},
		}); err != nil {
			return err
		}
		result = current
		return nil
	})
	if err != nil {
		return Policy{}, err
	}
	return policyView(result), nil
}

func (s *Service) Request(
	ctx context.Context,
	principal identity.Principal,
	input RequestInput,
	requestID, ipAddress string,
) (Grant, error) {
	if input.TenantID == uuid.Nil || input.TenantID == s.operatorTenantID {
		return Grant{}, problem.New(400, "invalid_support_access_tenant", "A customer Tenant is required.")
	}
	if _, err := s.requireOperator(ctx, principal, false); err != nil {
		return Grant{}, err
	}
	reason, err := normalizeReason(input.Reason)
	if err != nil {
		return Grant{}, err
	}
	if input.RequestedDurationSeconds < minimumDurationSeconds || input.RequestedDurationSeconds > maximumDurationSeconds {
		return Grant{}, problem.New(400, "invalid_support_access_duration", "Support Access duration must be between 5 minutes and 4 hours.")
	}
	now := s.now()
	operatorTenantID := s.operatorTenantID
	model := persistence.SupportAccessGrant{
		ID: uuid.New(), TenantID: input.TenantID, OperatorTenantID: &operatorTenantID,
		RequesterUserID: principal.UserID,
		Status:          "pending", Version: 1, Reason: reason,
		RequestedDurationSeconds: input.RequestedDurationSeconds, RequestedAt: now,
	}
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		if err := s.expireActiveGrantsTx(ctx, tx, now); err != nil {
			return err
		}
		var policy persistence.TenantSupportPolicy
		if err := tx.WithContext(ctx).
			Where("tenant_id = ? AND support_access_enabled = ?", input.TenantID, true).
			Take(&policy).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(403, "tenant_support_access_disabled", "This Tenant has disabled Support Access.")
		} else if err != nil {
			return problem.Wrap(500, "tenant_support_policy_load_failed", "Tenant Support policy could not be loaded.", err)
		}
		if err := tx.WithContext(ctx).Create(&model).Error; err != nil {
			return problem.Wrap(409, "support_access_request_conflict", "A pending or active Support Access request already exists.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: input.TenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "support.access_requested", ResourceType: "support_access_grant", ResourceID: &model.ID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"reason": reason, "requestedDurationSeconds": input.RequestedDurationSeconds,
			},
		})
	})
	if err != nil {
		return Grant{}, err
	}
	return s.getGrantView(ctx, s.db, model.ID)
}

func (s *Service) Approve(
	ctx context.Context,
	principal identity.Principal,
	grantID uuid.UUID,
	input DecisionInput,
	requestID, ipAddress string,
) (Grant, error) {
	if _, err := s.requireOperator(ctx, principal, true); err != nil {
		return Grant{}, err
	}
	reason, err := normalizeReason(input.Reason)
	if err != nil {
		return Grant{}, err
	}
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var grant persistence.SupportAccessGrant
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("id = ?", grantID).Take(&grant).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "support_access_not_found", "Support Access request not found.")
		} else if err != nil {
			return problem.Wrap(500, "support_access_load_failed", "Support Access request could not be loaded.", err)
		}
		if grant.Status != "pending" || grant.Version != input.ExpectedVersion {
			return versionConflict("support_access_version_conflict", "Support Access request changed; reload it before approving.")
		}
		if grant.RequesterUserID == principal.UserID {
			return problem.New(403, "support_access_self_approval_forbidden", "Support Access requires approval by a different Platform Admin.")
		}
		if err := s.requireRequesterStillOperator(ctx, tx, grant.RequesterUserID); err != nil {
			return err
		}
		var policy persistence.TenantSupportPolicy
		if err := tx.WithContext(ctx).Where("tenant_id = ? AND support_access_enabled = ?", grant.TenantID, true).Take(&policy).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(409, "tenant_support_access_disabled", "This Tenant disabled Support Access before approval.")
		} else if err != nil {
			return err
		}
		now := s.now()
		expiresAt := now.Add(time.Duration(grant.RequestedDurationSeconds) * time.Second)
		updated := tx.WithContext(ctx).Model(&persistence.SupportAccessGrant{}).
			Where("id = ? AND status = ? AND version = ?", grant.ID, "pending", grant.Version).
			Updates(map[string]any{
				"status": "active", "version": grant.Version + 1, "decided_by": principal.UserID,
				"decision_reason": reason, "decided_at": now, "expires_at": expiresAt,
			})
		if updated.Error != nil || updated.RowsAffected != 1 {
			return problem.Wrap(409, "support_access_version_conflict", "Support Access request changed; reload it before approving.", updated.Error)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: grant.TenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "support.access_approved", ResourceType: "support_access_grant", ResourceID: &grant.ID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"requesterUserId": grant.RequesterUserID, "expiresAt": expiresAt, "reason": reason,
			},
		})
	})
	if err != nil {
		return Grant{}, err
	}
	return s.getGrantView(ctx, s.db, grantID)
}

func (s *Service) Deny(
	ctx context.Context,
	principal identity.Principal,
	grantID uuid.UUID,
	input DecisionInput,
	requestID, ipAddress string,
) (Grant, error) {
	if _, err := s.requireOperator(ctx, principal, true); err != nil {
		return Grant{}, err
	}
	reason, err := normalizeReason(input.Reason)
	if err != nil {
		return Grant{}, err
	}
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		grant, err := lockGrant(ctx, tx, grantID)
		if err != nil {
			return err
		}
		if grant.Status != "pending" || grant.Version != input.ExpectedVersion {
			return versionConflict("support_access_version_conflict", "Support Access request changed; reload it before denying.")
		}
		if grant.RequesterUserID == principal.UserID {
			return problem.New(403, "support_access_self_decision_forbidden", "Support Access requires a decision by a different Platform Admin.")
		}
		now := s.now()
		updated := tx.WithContext(ctx).Model(&persistence.SupportAccessGrant{}).
			Where("id = ? AND status = ? AND version = ?", grant.ID, "pending", grant.Version).
			Updates(map[string]any{
				"status": "denied", "version": grant.Version + 1, "decided_by": principal.UserID,
				"decision_reason": reason, "decided_at": now,
			})
		if updated.Error != nil || updated.RowsAffected != 1 {
			return problem.Wrap(409, "support_access_version_conflict", "Support Access request changed; reload it before denying.", updated.Error)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: grant.TenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "support.access_denied", ResourceType: "support_access_grant", ResourceID: &grant.ID,
			RequestID: requestID, IPAddress: ipAddress, Metadata: map[string]any{"reason": reason},
		})
	})
	if err != nil {
		return Grant{}, err
	}
	return s.getGrantView(ctx, s.db, grantID)
}

func (s *Service) Revoke(
	ctx context.Context,
	principal identity.Principal,
	grantID uuid.UUID,
	input DecisionInput,
	requestID, ipAddress string,
) (Grant, error) {
	reason, err := normalizeReason(input.Reason)
	if err != nil {
		return Grant{}, err
	}
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		grant, err := lockGrant(ctx, tx, grantID)
		if err != nil {
			return err
		}
		operatorAdmin := false
		if principal.ActiveTenantID != nil && *principal.ActiveTenantID == s.operatorTenantID {
			if _, operatorErr := s.requireOperator(ctx, principal, true); operatorErr == nil {
				operatorAdmin = true
			}
		}
		if !operatorAdmin {
			if err := identity.RequireActiveTenant(principal, grant.TenantID); err != nil {
				return err
			}
			if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, grant.TenantID, authorization.TenantUpdate); err != nil {
				return err
			}
		}
		if grant.Status != "active" || grant.Version != input.ExpectedVersion {
			return versionConflict("support_access_version_conflict", "Support Access changed; reload it before revoking.")
		}
		now := s.now()
		updated := tx.WithContext(ctx).Model(&persistence.SupportAccessGrant{}).
			Where("id = ? AND status = ? AND version = ?", grant.ID, "active", grant.Version).
			Updates(map[string]any{
				"status": "revoked", "version": grant.Version + 1, "revoked_by": principal.UserID,
				"revocation_reason": reason, "revoked_at": now,
			})
		if updated.Error != nil || updated.RowsAffected != 1 {
			return problem.Wrap(409, "support_access_version_conflict", "Support Access changed; reload it before revoking.", updated.Error)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: grant.TenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "support.access_revoked", ResourceType: "support_access_grant", ResourceID: &grant.ID,
			RequestID: requestID, IPAddress: ipAddress, Metadata: map[string]any{"reason": reason},
		})
	})
	if err != nil {
		return Grant{}, err
	}
	return s.getGrantView(ctx, s.db, grantID)
}

func (s *Service) ListPlatform(ctx context.Context, principal identity.Principal) ([]Grant, error) {
	role, err := s.requireOperator(ctx, principal, false)
	if err != nil {
		return nil, err
	}
	if err := s.expireActiveGrants(ctx, s.db, s.now()); err != nil {
		return nil, err
	}
	query := s.grantViewQuery(ctx, s.db)
	if role == "security_admin" {
		query = query.Where("support_grant.requester_user_id = ?", principal.UserID)
	}
	var rows []grantRow
	if err := query.Order("support_grant.requested_at DESC, support_grant.id").Scan(&rows).Error; err != nil {
		return nil, problem.Wrap(500, "support_access_list_failed", "Support Access requests could not be loaded.", err)
	}
	return grantViews(rows), nil
}

func (s *Service) ListPlatformTenants(
	ctx context.Context,
	principal identity.Principal,
) (PlatformTenantOverview, error) {
	role, err := s.requireOperator(ctx, principal, false)
	if err != nil {
		return PlatformTenantOverview{}, err
	}
	now := s.now()
	rows, err := s.loadTenantOverviewRows(ctx, now, nil)
	if err != nil {
		return PlatformTenantOverview{}, problem.Wrap(500, "platform_tenants_load_failed", "Platform Tenants could not be loaded.", err)
	}
	items, err := tenantOverviewViews(rows)
	if err != nil {
		return PlatformTenantOverview{}, problem.Wrap(500, "platform_tenants_load_failed", "Platform Tenant queue time could not be loaded.", err)
	}
	return PlatformTenantOverview{OperatorRole: role, GeneratedAt: now, Items: items}, nil
}

type platformTenantOverviewRow struct {
	TenantOverview `gorm:"embedded"`
	OldestQueuedAt sql.NullString `gorm:"column:oldest_queued_at"`
}

// GetTenantOverview is the tenant-scoped, read-only counterpart to the
// Platform operations inventory. It deliberately returns aggregate health
// counts only; credentials, artifacts and execution payloads are never
// materialized. Support Read-Only can use this through the normal TenantRead
// permission without receiving Platform Operator authority.
func (s *Service) GetTenantOverview(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
) (TenantOverview, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return TenantOverview{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.TenantRead); err != nil {
		return TenantOverview{}, err
	}
	rows, err := s.loadTenantOverviewRows(ctx, s.now(), &tenantID)
	if err != nil {
		return TenantOverview{}, problem.Wrap(500, "tenant_operations_overview_load_failed", "Tenant operations overview could not be loaded.", err)
	}
	if len(rows) != 1 {
		return TenantOverview{}, problem.New(404, "tenant_not_found", "Tenant not found.")
	}
	items, err := tenantOverviewViews(rows)
	if err != nil {
		return TenantOverview{}, problem.Wrap(500, "tenant_operations_overview_load_failed", "Tenant queue state could not be loaded.", err)
	}
	return items[0], nil
}

func (s *Service) loadTenantOverviewRows(
	ctx context.Context,
	now time.Time,
	tenantID *uuid.UUID,
) ([]platformTenantOverviewRow, error) {
	failedSince := now.Add(-24 * time.Hour)
	query := s.db.WithContext(ctx).Table("tenants AS tenant").
		Select(`tenant.id, tenant.slug, tenant.name, tenant.status, tenant.plan_code, tenant.region,
			(SELECT COUNT(*) FROM tenant_memberships AS membership WHERE membership.tenant_id = tenant.id AND membership.status = 'active') AS active_member_count,
			(SELECT COUNT(*) FROM organizations AS organization WHERE organization.tenant_id = tenant.id AND organization.archived_at IS NULL) AS organization_count,
			(SELECT COUNT(*) FROM agent_sessions AS session WHERE session.tenant_id = tenant.id AND session.archived_at IS NULL) AS session_count,
			(SELECT COUNT(*) FROM execution_targets AS target WHERE target.tenant_id = tenant.id) AS execution_target_count,
			(SELECT COUNT(*) FROM worker_instances AS worker JOIN execution_targets AS worker_target ON worker_target.id = worker.execution_target_id WHERE (worker_target.tenant_id = tenant.id OR worker.tenant_binding_id = tenant.id) AND worker.administrative_status <> 'revoked') AS worker_count,
			(SELECT COUNT(*) FROM worker_instances AS worker JOIN execution_targets AS worker_target ON worker_target.id = worker.execution_target_id WHERE (worker_target.tenant_id = tenant.id OR worker.tenant_binding_id = tenant.id) AND worker.administrative_status <> 'revoked' AND worker.status = 'offline') AS offline_worker_count,
			(SELECT COUNT(*) FROM agent_executions AS execution WHERE execution.tenant_id = tenant.id AND execution.status IN ('queued', 'leased', 'running', 'waiting-for-approval', 'recovering')) AS active_execution_count,
			(SELECT COUNT(*) FROM agent_executions AS execution WHERE execution.tenant_id = tenant.id AND execution.status IN ('queued', 'recovering')) AS queued_execution_count,
			(SELECT MIN(execution.queued_at) FROM agent_executions AS execution WHERE execution.tenant_id = tenant.id AND execution.status IN ('queued', 'recovering')) AS oldest_queued_at,
			(SELECT COUNT(*) FROM agent_executions AS execution WHERE execution.tenant_id = tenant.id AND execution.failure_code IS NOT NULL AND execution.finished_at >= ?) AS failed_execution_count24h,
			(SELECT COUNT(*) FROM artifacts AS artifact WHERE artifact.tenant_id = tenant.id AND artifact.deleted_at IS NULL) AS artifact_count,
			(SELECT COALESCE(SUM(artifact.size_bytes), 0) FROM artifacts AS artifact WHERE artifact.tenant_id = tenant.id AND artifact.deleted_at IS NULL) AS artifact_bytes,
			(SELECT COUNT(*) FROM artifacts AS artifact WHERE artifact.tenant_id = tenant.id AND artifact.deleted_at IS NULL AND artifact.status IN ('pending', 'uploading')) AS pending_artifact_count,
			(SELECT COUNT(*) FROM provider_credentials AS credential WHERE credential.tenant_id = tenant.id AND credential.revoked_at IS NULL AND (credential.expires_at IS NULL OR credential.expires_at > ?)) AS active_credential_count,
			(SELECT COUNT(*) FROM provider_credentials AS credential WHERE credential.tenant_id = tenant.id AND (credential.revoked_at IS NOT NULL OR credential.expires_at <= ?)) AS unavailable_credential_count,
			(SELECT COUNT(*) FROM identity_connections AS identity_connection WHERE identity_connection.tenant_id = tenant.id AND identity_connection.status = 'active') AS active_identity_connection_count,
			(SELECT COUNT(*) FROM identity_connections AS identity_connection WHERE identity_connection.tenant_id = tenant.id AND identity_connection.status <> 'active') AS disabled_identity_connection_count`,
			failedSince, now, now).
		Where("tenant.deleted_at IS NULL AND tenant.id <> ?", s.operatorTenantID)
	if tenantID != nil {
		query = query.Where("tenant.id = ?", *tenantID)
	} else {
		query = query.Order("LOWER(tenant.name), tenant.id")
	}
	var rows []platformTenantOverviewRow
	if err := query.Scan(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func tenantOverviewViews(rows []platformTenantOverviewRow) ([]TenantOverview, error) {
	items := make([]TenantOverview, 0, len(rows))
	for _, row := range rows {
		item := row.TenantOverview
		item.Status = productprofile.PublicLifecycleStatus(item.Status)
		item.PlanCode = productprofile.PublicProfileCode(item.PlanCode)
		if row.OldestQueuedAt.Valid {
			parsed, err := databasetime.Parse(row.OldestQueuedAt.String)
			if err != nil {
				return nil, err
			}
			item.OldestQueuedAt = &parsed
		}
		items = append(items, item)
	}
	return items, nil
}

func (s *Service) ListTenant(ctx context.Context, principal identity.Principal, tenantID uuid.UUID) ([]Grant, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return nil, err
	}
	role, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.TenantRead)
	if err != nil {
		return nil, err
	}
	if err := s.expireActiveGrants(ctx, s.db, s.now()); err != nil {
		return nil, err
	}
	if role != authorization.SupportReadOnlyRole && role != "owner" && role != "admin" && role != "security_admin" && role != "auditor" {
		return nil, problem.New(403, "support_access_history_forbidden", "Tenant administrator or auditor access is required.")
	}
	query := s.grantViewQuery(ctx, s.db).Where("support_grant.tenant_id = ?", tenantID)
	if role == authorization.SupportReadOnlyRole {
		query = query.Where("support_grant.requester_user_id = ?", principal.UserID)
	}
	var rows []grantRow
	if err := query.Order("support_grant.requested_at DESC, support_grant.id").Scan(&rows).Error; err != nil {
		return nil, problem.Wrap(500, "support_access_list_failed", "Support Access requests could not be loaded.", err)
	}
	return grantViews(rows), nil
}

func (s *Service) RecordAccess(
	ctx context.Context,
	principal identity.Principal,
	method, route, requestID, ipAddress string,
) error {
	if principal.SupportAccessGrantID == nil || principal.ActiveTenantID == nil {
		return nil
	}
	grantID, active, err := s.authorizer.ActiveSupportGrant(ctx, principal.UserID, *principal.ActiveTenantID)
	if err != nil {
		return err
	}
	if !active || grantID != *principal.SupportAccessGrantID {
		return problem.New(403, "support_access_expired", "Support Access expired or was revoked.")
	}
	readOnly := method == "GET" || method == "HEAD"
	action := "support.read_accessed"
	if !readOnly {
		action = "support.control_attempted"
	}
	return audit.Record(ctx, s.db, audit.Entry{
		TenantID: *principal.ActiveTenantID, ActorType: "user", ActorID: &principal.UserID,
		Action: action, ResourceType: "support_access_grant", ResourceID: &grantID,
		RequestID: requestID, IPAddress: ipAddress,
		Metadata: map[string]any{"method": method, "route": route, "readOnly": readOnly},
	})
}

func (s *Service) requireOperator(
	ctx context.Context,
	principal identity.Principal,
	admin bool,
) (string, error) {
	if principal.Audience != "" && principal.Audience != "web" {
		return "", problem.New(403, "platform_web_session_required", "Platform operations require a Web session.")
	}
	if s.operatorTenantID == uuid.Nil {
		return "", problem.New(503, "platform_operations_unavailable", "Platform operations require SYNARA_PLATFORM_OPERATOR_TENANT_ID.")
	}
	if principal.ActiveTenantID == nil || *principal.ActiveTenantID != s.operatorTenantID {
		return "", problem.New(403, "platform_operator_forbidden", "Switch to the configured Platform Operator Tenant first.")
	}
	var membership persistence.TenantMembership
	if err := s.db.WithContext(ctx).Table("tenant_memberships AS membership").
		Select("membership.*").
		Joins("JOIN tenants AS operator_tenant ON operator_tenant.id = membership.tenant_id AND operator_tenant.status = ? AND operator_tenant.deleted_at IS NULL", "active").
		Where("membership.tenant_id = ? AND membership.user_id = ? AND membership.status = ?", s.operatorTenantID, principal.UserID, "active").
		Take(&membership).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return "", problem.New(403, "platform_operator_forbidden", "Platform Operator access is required.")
	} else if err != nil {
		return "", problem.Wrap(500, "platform_operator_authorization_failed", "Platform Operator access could not be authorized.", err)
	}
	if admin && membership.Role != "owner" && membership.Role != "admin" {
		return "", problem.New(403, "platform_admin_forbidden", "Platform Admin access is required.")
	}
	if !admin && membership.Role != "owner" && membership.Role != "admin" && membership.Role != "security_admin" {
		return "", problem.New(403, "platform_support_forbidden", "Platform Support access is required.")
	}
	return membership.Role, nil
}

// RequirePlatformAdmin exposes the same fail-closed Platform Operator Tenant
// boundary to other HTTP platform-governance handlers without duplicating the
// membership query or weakening the active-Tenant requirement.
func (s *Service) RequirePlatformAdmin(
	ctx context.Context,
	principal identity.Principal,
) (string, error) {
	return s.requireOperator(ctx, principal, true)
}

// RequirePlatformOperator admits the bounded owner/admin/security_admin set for
// role-separated governance review. Callers must still apply their operation's
// narrower write authority after receiving the current role.
func (s *Service) RequirePlatformOperator(
	ctx context.Context,
	principal identity.Principal,
) (string, error) {
	return s.requireOperator(ctx, principal, false)
}

func (s *Service) requireRequesterStillOperator(ctx context.Context, tx *gorm.DB, userID uuid.UUID) error {
	var membership persistence.TenantMembership
	if err := tx.WithContext(ctx).Table("tenant_memberships AS membership").
		Select("membership.*").
		Joins("JOIN tenants AS operator_tenant ON operator_tenant.id = membership.tenant_id AND operator_tenant.status = ? AND operator_tenant.deleted_at IS NULL", "active").
		Where("membership.tenant_id = ? AND membership.user_id = ? AND membership.status = ? AND membership.role IN ?", s.operatorTenantID, userID, "active", []string{"owner", "admin", "security_admin"}).
		Take(&membership).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return problem.New(409, "support_requester_no_longer_authorized", "The requester no longer has Platform Support access.")
	} else if err != nil {
		return problem.Wrap(500, "platform_operator_authorization_failed", "Platform Operator access could not be authorized.", err)
	}
	return nil
}

func (s *Service) expireActiveGrants(ctx context.Context, db *gorm.DB, now time.Time) error {
	return persistence.InTransaction(ctx, db, func(tx *gorm.DB) error {
		return s.expireActiveGrantsTx(ctx, tx, now)
	})
}

func (s *Service) expireActiveGrantsTx(ctx context.Context, tx *gorm.DB, now time.Time) error {
	var grants []persistence.SupportAccessGrant
	if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("status = ? AND expires_at <= ?", "active", now).
		Order("tenant_id, id").Find(&grants).Error; err != nil {
		return problem.Wrap(500, "support_access_expiry_failed", "Expired Support Access could not be loaded.", err)
	}
	for _, grant := range grants {
		updated := tx.WithContext(ctx).Model(&persistence.SupportAccessGrant{}).
			Where("id = ? AND status = ? AND version = ?", grant.ID, "active", grant.Version).
			Updates(map[string]any{"status": "expired", "version": grant.Version + 1})
		if updated.Error != nil {
			return problem.Wrap(500, "support_access_expiry_failed", "Expired Support Access could not be reconciled.", updated.Error)
		}
		if updated.RowsAffected != 1 {
			return versionConflict("support_access_expiry_conflict", "Support Access changed while expiry was reconciled.")
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: grant.TenantID, ActorType: "system",
			Action: "support.access_expired", ResourceType: "support_access_grant", ResourceID: &grant.ID,
			RequestID: "support-expiry:" + grant.ID.String(),
			Metadata: map[string]any{
				"expiresAt": grant.ExpiresAt, "reconciledAt": now,
				"previousVersion": grant.Version, "version": grant.Version + 1,
			},
		}); err != nil {
			return problem.Wrap(500, "support_access_expiry_audit_failed", "Support Access expiry could not be audited.", err)
		}
	}
	return nil
}

func (s *Service) revokeActiveGrantsForPolicy(
	ctx context.Context,
	tx *gorm.DB,
	actorID, tenantID uuid.UUID,
	reason, requestID, ipAddress string,
	now time.Time,
) error {
	var grants []persistence.SupportAccessGrant
	if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
		Where("tenant_id = ? AND status = ?", tenantID, "active").
		Order("id").Find(&grants).Error; err != nil {
		return problem.Wrap(500, "support_access_revoke_failed", "Active Support Access could not be loaded.", err)
	}
	for _, grant := range grants {
		updated := tx.WithContext(ctx).Model(&persistence.SupportAccessGrant{}).
			Where("id = ? AND status = ? AND version = ?", grant.ID, "active", grant.Version).
			Updates(map[string]any{
				"status": "revoked", "version": grant.Version + 1,
				"revoked_by": actorID, "revocation_reason": reason, "revoked_at": now,
			})
		if updated.Error != nil {
			return problem.Wrap(500, "support_access_revoke_failed", "Active Support Access could not be revoked.", updated.Error)
		}
		if updated.RowsAffected != 1 {
			return versionConflict("support_access_version_conflict", "Support Access changed while the Tenant policy was disabled.")
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &actorID,
			Action: "support.access_revoked", ResourceType: "support_access_grant", ResourceID: &grant.ID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"reason": reason, "source": "tenant_support_policy_disabled",
				"previousVersion": grant.Version, "version": grant.Version + 1,
			},
		}); err != nil {
			return problem.Wrap(500, "support_access_revoke_audit_failed", "Support Access revocation could not be audited.", err)
		}
	}
	return nil
}

type grantRow struct {
	persistence.SupportAccessGrant
	TenantName           string `gorm:"column:tenant_name"`
	RequesterEmail       string `gorm:"column:requester_email"`
	RequesterDisplayName string `gorm:"column:requester_display_name"`
}

func (s *Service) grantViewQuery(ctx context.Context, db *gorm.DB) *gorm.DB {
	return db.WithContext(ctx).Table("support_access_grants AS support_grant").
		Select("support_grant.*, tenant.name AS tenant_name, requester.email AS requester_email, requester.display_name AS requester_display_name").
		Joins("JOIN tenants AS tenant ON tenant.id = support_grant.tenant_id").
		Joins("JOIN users AS requester ON requester.id = support_grant.requester_user_id")
}

func (s *Service) getGrantView(ctx context.Context, db *gorm.DB, grantID uuid.UUID) (Grant, error) {
	var row grantRow
	if err := s.grantViewQuery(ctx, db).Where("support_grant.id = ?", grantID).Take(&row).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return Grant{}, problem.New(404, "support_access_not_found", "Support Access request not found.")
	} else if err != nil {
		return Grant{}, problem.Wrap(500, "support_access_load_failed", "Support Access request could not be loaded.", err)
	}
	return grantView(row), nil
}

func grantViews(rows []grantRow) []Grant {
	result := make([]Grant, 0, len(rows))
	for _, row := range rows {
		result = append(result, grantView(row))
	}
	return result
}

func grantView(row grantRow) Grant {
	model := row.SupportAccessGrant
	return Grant{
		ID: model.ID, TenantID: model.TenantID, TenantName: row.TenantName,
		RequesterUserID: model.RequesterUserID, RequesterEmail: row.RequesterEmail,
		RequesterDisplayName: row.RequesterDisplayName, Status: model.Status, Version: model.Version,
		Reason: model.Reason, RequestedDurationSeconds: model.RequestedDurationSeconds,
		RequestedAt: model.RequestedAt, DecidedBy: model.DecidedBy, DecisionReason: model.DecisionReason,
		DecidedAt: model.DecidedAt, ExpiresAt: model.ExpiresAt, RevokedBy: model.RevokedBy,
		RevocationReason: model.RevocationReason, RevokedAt: model.RevokedAt,
		CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt,
	}
}

func policyView(model persistence.TenantSupportPolicy) Policy {
	updatedBy := model.UpdatedBy
	createdAt, updatedAt := model.CreatedAt, model.UpdatedAt
	return Policy{
		TenantID: model.TenantID, SupportAccessEnabled: model.SupportAccessEnabled,
		Version: model.Version, Reason: model.Reason, UpdatedBy: &updatedBy,
		CreatedAt: &createdAt, UpdatedAt: &updatedAt,
	}
}

func lockGrant(ctx context.Context, tx *gorm.DB, grantID uuid.UUID) (persistence.SupportAccessGrant, error) {
	var grant persistence.SupportAccessGrant
	if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("id = ?", grantID).Take(&grant).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.SupportAccessGrant{}, problem.New(404, "support_access_not_found", "Support Access request not found.")
	} else if err != nil {
		return persistence.SupportAccessGrant{}, problem.Wrap(500, "support_access_load_failed", "Support Access request could not be loaded.", err)
	}
	return grant, nil
}

func normalizeReason(value string) (string, error) {
	reason := strings.TrimSpace(value)
	if len(reason) < 10 || len(reason) > 1000 {
		return "", problem.New(400, "invalid_support_access_reason", "Support Access reason must contain between 10 and 1000 characters.")
	}
	return reason, nil
}

func versionConflict(code, message string) error { return problem.New(409, code, message) }
