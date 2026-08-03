package tenancy

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/productprofile"
	"github.com/synara-ai/synara/services/control-plane/internal/retentiongate"
	"github.com/synara-ai/synara/services/control-plane/internal/schedulingpolicy"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/internal/tenantuseraccess"
	"github.com/synara-ai/synara/services/control-plane/internal/validation"
)

func toTenant(model persistence.Tenant, role string) Tenant {
	settings := model.Settings
	if settings == nil {
		settings = map[string]any{}
	}
	return Tenant{
		ID: model.ID, Slug: model.Slug, Name: model.Name, Status: productprofile.PublicLifecycleStatus(model.Status),
		LifecycleVersion: model.LifecycleVersion, TrialExpiresAt: model.TrialExpiresAt,
		SuspendedAt: model.SuspendedAt, ClosedAt: model.ClosedAt, DeletionRequestedAt: model.DeletedAt,
		PlanCode: productprofile.PublicProfileCode(model.PlanCode), Region: model.Region, Settings: settings, Role: role,
		CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt,
	}
}

func (s *Service) CreateTenant(
	ctx context.Context,
	principal identity.Principal,
	input CreateTenantInput,
	requestID string,
	ipAddress string,
) (Tenant, error) {
	model, err := s.createTenantForOwner(
		ctx, principal, principal.UserID, input, "self_service", requestID, ipAddress,
	)
	if err != nil {
		return Tenant{}, err
	}
	return toTenant(model, "owner"), nil
}

func (s *Service) CreateSelfServiceTenant(
	ctx context.Context,
	principal identity.Principal,
	input CreateTenantInput,
	requestID string,
	ipAddress string,
) (Tenant, error) {
	planCode := productprofile.InternalProfileCode(input.PlanCode)
	if planCode != "" && planCode != "free" {
		return Tenant{}, problem.New(403, "self_service_entitlement_profile_forbidden", "Self-service Tenant creation is limited to the Standard entitlement profile.")
	}
	status := strings.ToLower(strings.TrimSpace(input.Status))
	if status != "" && status != "active" {
		return Tenant{}, problem.New(403, "self_service_status_forbidden", "Self-service Tenant creation starts active on the Standard entitlement profile.")
	}
	if input.TrialExpiresAt != nil {
		return Tenant{}, problem.New(400, "self_service_evaluation_forbidden", "Self-service Standard Tenants do not accept a client-supplied evaluation expiry.")
	}
	input.PlanCode = "free"
	input.Status = "active"
	model, err := s.createTenantForOwner(
		ctx, principal, principal.UserID, input, "self_service", requestID, ipAddress,
	)
	if err != nil {
		return Tenant{}, err
	}
	return toTenant(model, "owner"), nil
}

func (s *Service) ProvisionTenant(
	ctx context.Context,
	principal identity.Principal,
	input ProvisionTenantInput,
	requestID string,
	ipAddress string,
) (ProvisionedTenant, error) {
	ownerEmail, err := validation.Email(input.OwnerEmail)
	if err != nil {
		return ProvisionedTenant{}, err
	}
	var owner persistence.User
	if err := s.db.WithContext(ctx).
		Where("LOWER(email) = ? AND status = ? AND deleted_at IS NULL", ownerEmail, "active").
		Take(&owner).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return ProvisionedTenant{}, problem.New(409, "tenant_owner_not_found", "The initial owner must have an active Synara user account.")
	} else if err != nil {
		return ProvisionedTenant{}, problem.Wrap(500, "tenant_owner_load_failed", "The initial Tenant owner could not be loaded.", err)
	}
	model, err := s.createTenantForOwner(ctx, principal, owner.ID, CreateTenantInput{
		Slug: input.Slug, Name: input.Name, Region: input.Region, PlanCode: input.PlanCode,
		Status: input.Status, TrialExpiresAt: input.TrialExpiresAt,
	}, "platform_admin", requestID, ipAddress)
	if err != nil {
		return ProvisionedTenant{}, err
	}
	return ProvisionedTenant{
		ID: model.ID, Slug: model.Slug, Name: model.Name, Status: productprofile.PublicLifecycleStatus(model.Status),
		LifecycleVersion: model.LifecycleVersion, TrialExpiresAt: model.TrialExpiresAt,
		PlanCode: productprofile.PublicProfileCode(model.PlanCode), Region: model.Region, OwnerUserID: owner.ID,
		OwnerEmail: ownerEmail, CreatedAt: model.CreatedAt,
	}, nil
}

func (s *Service) createTenantForOwner(
	ctx context.Context,
	principal identity.Principal,
	ownerUserID uuid.UUID,
	input CreateTenantInput,
	assignmentSource string,
	requestID string,
	ipAddress string,
) (persistence.Tenant, error) {
	normalized, err := normalizeTenantInput(input)
	if err != nil {
		return persistence.Tenant{}, err
	}
	tenantID, organizationID := uuid.New(), uuid.New()
	tenantModel := persistence.Tenant{
		ID: tenantID, Slug: normalized.Slug, Name: normalized.Name, Status: normalized.Status,
		LifecycleVersion: 1, TrialExpiresAt: normalized.TrialExpiresAt,
		PlanCode: normalized.PlanCode, Region: normalized.Region, Settings: map[string]any{},
		CreatedBy: principal.UserID,
	}
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var plan persistence.SaaSPlan
		if err := tx.Where("code = ? AND status = ?", normalized.PlanCode, "active").Take(&plan).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(400, "tenant_entitlement_profile_unavailable", "The requested Tenant entitlement profile is unavailable.")
		} else if err != nil {
			return problem.Wrap(500, "tenant_entitlement_profile_load_failed", "Failed to load the Tenant entitlement profile.", err)
		}
		if err := tx.Create(&tenantModel).Error; err != nil {
			return problem.Wrap(409, "tenant_slug_conflict", "A tenant with this slug already exists.", err)
		}
		now := time.Now().UTC()
		if err := tx.Create(&persistence.TenantSubscription{
			TenantID: tenantID, PlanCode: normalized.PlanCode, Status: normalized.Status, Version: 1,
			TrialEndsAt: normalized.TrialExpiresAt, CurrentPeriodStart: now,
			CurrentPeriodEnd: now.AddDate(0, 1, 0), AssignmentSource: assignmentSource,
		}).Error; err != nil {
			return problem.Wrap(409, "tenant_entitlement_profile_create_failed", "Failed to create the Tenant entitlement profile assignment.", err)
		}
		if err := tx.Create(&persistence.TenantMembership{
			TenantID: tenantID, UserID: ownerUserID, Role: "owner", Status: "active", JoinedAt: &now,
		}).Error; err != nil {
			return problem.Wrap(500, "tenant_create_failed", "Failed to create the owner membership.", err)
		}
		if err := tx.Create(&persistence.Organization{
			ID: organizationID, TenantID: tenantID, Slug: "root", Name: "Root organization",
			Kind: "root", Status: "active", Settings: map[string]any{}, CreatedBy: principal.UserID,
		}).Error; err != nil {
			return problem.Wrap(500, "tenant_create_failed", "Failed to create the root organization.", err)
		}
		if err := tx.Create(&persistence.OrganizationMembership{
			TenantID: tenantID, OrganizationID: organizationID, UserID: ownerUserID,
			Role: "owner", Status: "active",
		}).Error; err != nil {
			return problem.Wrap(500, "tenant_create_failed", "Failed to create the root organization owner.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "tenant.created", ResourceType: "tenant", ResourceID: &tenantID,
			OrganizationID: &organizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"slug":                   normalized.Slug,
				"entitlementProfileCode": productprofile.PublicProfileCode(normalized.PlanCode),
				"status":                 productprofile.PublicLifecycleStatus(normalized.Status), "lifecycleVersion": 1,
				"ownerUserId": ownerUserID, "assignmentSource": assignmentSource,
			},
		})
	})
	if err != nil {
		return persistence.Tenant{}, err
	}
	return tenantModel, nil
}

func (s *Service) GetTenant(ctx context.Context, principal identity.Principal, tenantID uuid.UUID) (Tenant, error) {
	role, err := s.requireTenantPermission(ctx, principal, tenantID, authorization.TenantRead)
	if err != nil {
		return Tenant{}, err
	}
	return s.loadTenantForRole(ctx, tenantID, role)
}

func (s *Service) loadTenantForRole(ctx context.Context, tenantID uuid.UUID, role string) (Tenant, error) {
	model, err := s.tenantRepository.First(ctx,
		func(db *gorm.DB) *gorm.DB { return db.Where("id = ? AND deleted_at IS NULL", tenantID) },
	)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Tenant{}, problem.New(404, "tenant_not_found", "Tenant not found.")
	}
	if err != nil {
		return Tenant{}, problem.Wrap(500, "tenant_load_failed", "Failed to load the tenant.", err)
	}
	return toTenant(model, role), nil
}

func (s *Service) ListTenants(ctx context.Context, principal identity.Principal) ([]Tenant, error) {
	type tenantRow struct {
		persistence.Tenant
		Role string `gorm:"column:role"`
	}
	rows := make([]tenantRow, 0)
	err := s.db.WithContext(ctx).
		Table("tenant_memberships AS tm").
		Select("t.*, tm.role").
		Joins("JOIN tenants AS t ON t.id = tm.tenant_id").
		Where("tm.user_id = ? AND tm.status = ? AND t.deleted_at IS NULL", principal.UserID, "active").
		Order("LOWER(t.name), t.id").Scan(&rows).Error
	if err != nil {
		return nil, problem.Wrap(500, "tenants_load_failed", "Failed to load tenants.", err)
	}
	result := make([]Tenant, 0, len(rows))
	for _, row := range rows {
		result = append(result, toTenant(row.Tenant, row.Role))
	}
	return result, nil
}

// ListDeletingTenants returns only deletion requests that the current user can still restore as a Tenant owner.
// It intentionally uses a separate read path because normal Tenant/session lists exclude soft-deleted rows.
func (s *Service) ListDeletingTenants(ctx context.Context, principal identity.Principal) ([]Tenant, error) {
	type tenantRow struct {
		persistence.Tenant
		Role string `gorm:"column:role"`
	}
	rows := make([]tenantRow, 0)
	err := s.db.WithContext(ctx).Unscoped().
		Table("tenant_memberships AS tm").
		Select("t.*, tm.role").
		Joins("JOIN tenants AS t ON t.id = tm.tenant_id").
		Where("tm.user_id = ? AND tm.status = ? AND tm.role = ?", principal.UserID, "active", "owner").
		Where("t.status = ? AND t.deleted_at IS NOT NULL", "deleting").
		Order("t.deleted_at DESC, t.id").Scan(&rows).Error
	if err != nil {
		return nil, problem.Wrap(500, "tenant_deletion_requests_load_failed", "Tenant deletion requests could not be loaded.", err)
	}
	result := make([]Tenant, 0, len(rows))
	for _, row := range rows {
		result = append(result, toTenant(row.Tenant, row.Role))
	}
	return result, nil
}

func (s *Service) UpdateTenant(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	input UpdateTenantInput,
	requestID string,
	ipAddress string,
) (Tenant, error) {
	role, err := s.requireTenantPermission(ctx, principal, tenantID, authorization.TenantUpdate)
	if err != nil {
		return Tenant{}, err
	}
	updates := map[string]any{}
	if input.Name != nil {
		name, err := validation.Name(*input.Name, "invalid_tenant_name", "Tenant name", 160)
		if err != nil {
			return Tenant{}, err
		}
		updates["name"] = name
	}
	if input.Status != nil {
		if role != "owner" {
			return Tenant{}, problem.New(403, "tenant_status_forbidden", "Only a tenant owner can change tenant status.")
		}
		return Tenant{}, problem.New(400, "tenant_lifecycle_transition_required", "Use the Tenant lifecycle transition endpoint to change status with a reason and expected version.")
	}
	if input.Region != nil {
		region, err := validation.Code(*input.Region, "", "invalid_tenant_region", "Tenant region")
		if err != nil {
			return Tenant{}, err
		}
		updates["region"] = region
	}
	if input.Settings != nil {
		updates["settings"] = *input.Settings
	}
	if len(updates) == 0 {
		return Tenant{}, problem.New(400, "empty_update", "Provide at least one tenant field to update.")
	}

	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		if region, changingRegion := updates["region"].(string); changingRegion {
			policyService := schedulingpolicy.NewService(s.db)
			snapshot, err := policyService.Resolve(ctx, tx, tenantID, nil)
			if err != nil {
				return problem.Wrap(500, "tenant_region_policy_load_failed", "Tenant execution Region policy could not be loaded.", err)
			}
			if err := policyService.LockEffectiveForCommit(ctx, tx, tenantID, nil, snapshot); err != nil {
				return problem.Wrap(409, "tenant_region_policy_stale", "Tenant execution Region policy changed before the home Region update.", err)
			}
			if snapshot.Effective.Region.Mode == schedulingpolicy.ModeAllow &&
				!slices.Contains(snapshot.Effective.Region.Values, region) {
				return problem.New(409, "tenant_region_outside_execution_boundary", "Tenant home Region must remain inside the enforced execution Region boundary.")
			}
		}
		result := tx.Model(&persistence.Tenant{}).
			Where("id = ? AND deleted_at IS NULL", tenantID).Updates(updates)
		if result.Error != nil {
			return problem.Wrap(500, "tenant_update_failed", "Failed to update the tenant.", result.Error)
		}
		if result.RowsAffected == 0 {
			return problem.New(404, "tenant_not_found", "Tenant not found.")
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "tenant.updated", ResourceType: "tenant", ResourceID: &tenantID,
			RequestID: requestID, IPAddress: ipAddress,
		})
	})
	if err != nil {
		return Tenant{}, err
	}
	return s.GetTenant(ctx, principal, tenantID)
}

func (s *Service) DeleteTenant(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	requestID string,
	ipAddress string,
) error {
	return s.deleteTenant(ctx, principal, tenantID, nil, "legacy DELETE request", requestID, ipAddress)
}

func (s *Service) RequestTenantDeletion(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	input DeleteTenantInput,
	requestID string,
	ipAddress string,
) error {
	if input.ExpectedVersion < 1 {
		return problem.New(400, "invalid_tenant_lifecycle_version", "expectedVersion must be a positive integer.")
	}
	reason, err := normalizeLifecycleReason(input.Reason)
	if err != nil {
		return err
	}
	return s.deleteTenant(ctx, principal, tenantID, &input.ExpectedVersion, reason, requestID, ipAddress)
}

func (s *Service) deleteTenant(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	expectedVersion *int64,
	reason string,
	requestID string,
	ipAddress string,
) error {
	if _, err := s.requireTenantPermission(ctx, principal, tenantID, authorization.TenantDelete); err != nil {
		return err
	}
	var executionEvents []persistence.SessionEvent
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		now := time.Now().UTC()
		var tenant persistence.Tenant
		lockErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("id = ? AND deleted_at IS NULL", tenantID).Take(&tenant).Error
		if errors.Is(lockErr, gorm.ErrRecordNotFound) {
			return problem.New(404, "tenant_not_found", "Tenant not found.")
		}
		if lockErr != nil {
			return problem.Wrap(500, "tenant_lock_failed", "Failed to lock the tenant for deletion.", lockErr)
		}
		if expectedVersion != nil && tenant.LifecycleVersion != *expectedVersion {
			return problem.New(409, "tenant_lifecycle_version_conflict", "Tenant lifecycle changed; reload it before retrying.")
		}
		if held, err := retentiongate.HasActiveTenantHold(tx.WithContext(ctx), tenantID); err != nil {
			return problem.Wrap(500, "tenant_legal_hold_check_failed", "Tenant Legal Holds could not be checked.", err)
		} else if held {
			return problem.New(409, "tenant_legal_hold_active", "Tenant deletion is blocked while any Legal Hold is active.")
		}

		if s.executionCoordinator != nil {
			var err error
			executionEvents, err = s.executionCoordinator.PrepareTenantDeletion(
				ctx, tx, tenantID, principal.UserID, now,
			)
			if err != nil {
				return err
			}
		} else {
			var activeExecutions int64
			if err := tx.WithContext(ctx).Model(&persistence.AgentExecution{}).
				Where("tenant_id = ? AND status IN ?", tenantID,
					[]string{"queued", "recovering", "leased", "running", "waiting-for-approval", "suspended"}).
				Count(&activeExecutions).Error; err != nil {
				return problem.Wrap(500, "tenant_execution_cleanup_check_failed", "Failed to inspect active Tenant Executions.", err)
			}
			if activeExecutions != 0 {
				return problem.New(500, "tenant_execution_cleanup_unconfigured", "Tenant deletion cannot safely stop active Executions.")
			}
		}

		if err := tx.WithContext(ctx).Model(&persistence.WorkspaceMaterialization{}).
			Where("tenant_id = ? AND state <> ?", tenantID, "cleaned").
			Updates(map[string]any{
				"cleanup_reason": "tenant-delete",
				"cleanup_requested_at": gorm.Expr(
					"CASE WHEN cleanup_requested_at IS NULL OR cleanup_requested_at > ? THEN ? ELSE cleanup_requested_at END",
					now, now,
				),
				"updated_at": now,
			}).Error; err != nil {
			return problem.Wrap(500, "tenant_workspace_cleanup_intent_failed", "Failed to schedule Tenant Workspace cleanup.", err)
		}
		result := tx.Model(&persistence.Tenant{}).Where("id = ? AND deleted_at IS NULL", tenantID).
			Updates(map[string]any{
				"status": "deleting", "deleted_at": now,
				"lifecycle_version": gorm.Expr("lifecycle_version + 1"),
			})
		if result.Error != nil {
			return problem.Wrap(500, "tenant_delete_failed", "Failed to delete the tenant.", result.Error)
		}
		if result.RowsAffected == 0 {
			return problem.New(404, "tenant_not_found", "Tenant not found.")
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "tenant.deleted", ResourceType: "tenant", ResourceID: &tenantID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"fromStatus": tenant.Status, "toStatus": "deleting",
				"fromVersion": tenant.LifecycleVersion, "toVersion": tenant.LifecycleVersion + 1,
				"reason": reason,
			},
		})
	})
	if err == nil && s.executionCoordinator != nil {
		s.executionCoordinator.PublishTenantDeletionEvents(executionEvents)
	}
	return err
}

func (s *Service) ListTenantMembers(ctx context.Context, principal identity.Principal, tenantID uuid.UUID) ([]TenantMember, error) {
	if _, err := s.requireTenantPermission(ctx, principal, tenantID, authorization.TenantMembersRead); err != nil {
		return nil, err
	}
	items := make([]TenantMember, 0)
	err := s.db.WithContext(ctx).Table("tenant_memberships AS tm").
		Select("tm.tenant_id, tm.user_id, u.email, u.display_name, tm.role, tm.status, tm.joined_at, tm.created_at, tm.updated_at").
		Joins("JOIN users AS u ON u.id = tm.user_id").
		Where("tm.tenant_id = ?", tenantID).
		Order("CASE tm.role WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END, LOWER(u.display_name), tm.user_id").
		Scan(&items).Error
	if err != nil {
		return nil, problem.Wrap(500, "tenant_members_load_failed", "Failed to load tenant members.", err)
	}
	return items, nil
}

func (s *Service) InviteTenantMember(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	input InviteTenantMemberInput,
	requestID string,
	ipAddress string,
) (Invitation, error) {
	actorRole, err := s.requireTenantPermission(ctx, principal, tenantID, authorization.TenantMembersInvite)
	if err != nil {
		return Invitation{}, err
	}
	email, err := validation.Email(input.Email)
	if err != nil {
		return Invitation{}, err
	}
	role := normalizeRole(input.Role)
	if !validTenantRole(role) {
		return Invitation{}, problem.New(400, "invalid_tenant_role", "Tenant role is invalid.")
	}
	if role == "owner" && actorRole != "owner" {
		return Invitation{}, problem.New(403, "owner_invitation_forbidden", "Only an owner can invite another owner.")
	}
	var existingMemberCount int64
	if err := s.db.WithContext(ctx).Table("tenant_memberships AS tm").
		Joins("JOIN users AS u ON u.id = tm.user_id").
		Where("tm.tenant_id = ? AND LOWER(u.email) = ?", tenantID, email).
		Count(&existingMemberCount).Error; err != nil {
		return Invitation{}, problem.Wrap(500, "invitation_create_failed", "Failed to check existing tenant membership.", err)
	}
	if existingMemberCount > 0 {
		return Invitation{}, problem.New(409, "tenant_member_exists", "This user is already a tenant member.")
	}
	token, tokenHash, err := secret.NewToken()
	if err != nil {
		return Invitation{}, problem.Wrap(500, "invitation_create_failed", "Failed to generate the invitation.", err)
	}
	model := persistence.TenantInvitation{
		ID: uuid.New(), TenantID: tenantID, Email: email, Role: role, TokenHash: tokenHash,
		InvitedBy: principal.UserID, ExpiresAt: invitationExpiry(),
	}
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		now := time.Now().UTC()
		if err := tx.Model(&persistence.TenantInvitation{}).
			Where("tenant_id = ? AND LOWER(email) = ? AND accepted_at IS NULL AND revoked_at IS NULL", tenantID, email).
			Update("revoked_at", now).Error; err != nil {
			return problem.Wrap(500, "invitation_create_failed", "Failed to replace the previous invitation.", err)
		}
		if err := tx.Create(&model).Error; err != nil {
			return problem.Wrap(500, "invitation_create_failed", "Failed to persist the invitation.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "tenant.member_invited", ResourceType: "tenant_invitation", ResourceID: &model.ID,
			RequestID: requestID, IPAddress: ipAddress, Metadata: map[string]any{"email": email, "role": role},
		})
	})
	if err != nil {
		return Invitation{}, err
	}
	return Invitation{
		ID: model.ID, TenantID: tenantID, Email: email, Role: role, Token: token,
		ExpiresAt: model.ExpiresAt, CreatedAt: model.CreatedAt,
	}, nil
}

func (s *Service) AcceptInvitation(
	ctx context.Context,
	principal identity.Principal,
	token string,
	requestID string,
	ipAddress string,
) (Tenant, error) {
	if strings.TrimSpace(token) == "" {
		return Tenant{}, problem.New(400, "invalid_invitation", "Invitation token is required.")
	}
	var tenantID uuid.UUID
	var tenantRole string
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var invitation persistence.TenantInvitation
		err := persistence.WithLocking(tx, "UPDATE", "").
			Where("token_hash = ? AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > ?", secret.HashToken(token), time.Now().UTC()).
			Take(&invitation).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "invitation_not_found", "Invitation is invalid, expired, or already used.")
		}
		if err != nil {
			return problem.Wrap(500, "invitation_accept_failed", "Failed to load the invitation.", err)
		}
		if !strings.EqualFold(invitation.Email, principal.Email) {
			return problem.New(403, "invitation_email_mismatch", "This invitation belongs to a different email address.")
		}
		tenantID = invitation.TenantID
		tenantRole = invitation.Role
		now := time.Now().UTC()
		var existingMembership persistence.TenantMembership
		existingMembershipError := tx.Where("tenant_id = ? AND user_id = ?", tenantID, principal.UserID).
			Take(&existingMembership).Error
		if existingMembershipError == nil {
			return problem.New(409, "tenant_member_exists", "This user is already a tenant member.")
		}
		if !errors.Is(existingMembershipError, gorm.ErrRecordNotFound) {
			return problem.Wrap(500, "invitation_accept_failed", "Failed to check existing tenant membership.", existingMembershipError)
		}
		membership := persistence.TenantMembership{
			TenantID: tenantID, UserID: principal.UserID, Role: invitation.Role,
			Status: "active", InvitedBy: &invitation.InvitedBy, JoinedAt: &now,
		}
		if err := tx.Create(&membership).Error; err != nil {
			return problem.Wrap(500, "invitation_accept_failed", "Failed to create the tenant membership.", err)
		}
		if invitation.Role == "owner" || invitation.Role == "admin" {
			var root persistence.Organization
			if err := tx.Where("tenant_id = ? AND kind = ? AND archived_at IS NULL", tenantID, "root").Take(&root).Error; err != nil {
				return problem.Wrap(500, "invitation_accept_failed", "Failed to load the root organization.", err)
			}
			orgMembership := persistence.OrganizationMembership{
				TenantID: tenantID, OrganizationID: root.ID, UserID: principal.UserID,
				Role: invitation.Role, Status: "active",
			}
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "organization_id"}, {Name: "user_id"}},
				DoUpdates: clause.AssignmentColumns([]string{"role", "status"}),
			}).Create(&orgMembership).Error; err != nil {
				return problem.Wrap(500, "invitation_accept_failed", "Failed to grant root organization access.", err)
			}
		}
		if err := tx.Model(&invitation).Updates(map[string]any{
			"accepted_by": principal.UserID, "accepted_at": now,
		}).Error; err != nil {
			return problem.Wrap(500, "invitation_accept_failed", "Failed to finalize the invitation.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "tenant.invitation_accepted", ResourceType: "tenant_invitation", ResourceID: &invitation.ID,
			RequestID: requestID, IPAddress: ipAddress,
		})
	}, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return Tenant{}, err
	}
	return s.loadTenantForRole(ctx, tenantID, tenantRole)
}

func (s *Service) UpdateTenantMember(
	ctx context.Context,
	principal identity.Principal,
	tenantID, userID uuid.UUID,
	input UpdateTenantMemberInput,
	requestID, ipAddress string,
) (TenantMember, error) {
	actorRole, err := s.requireTenantPermission(ctx, principal, tenantID, authorization.TenantMembersUpdate)
	if err != nil {
		return TenantMember{}, err
	}
	var target persistence.TenantMembership
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND user_id = ?", tenantID, userID).Take(&target).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return TenantMember{}, problem.New(404, "tenant_member_not_found", "Tenant member not found.")
	} else if err != nil {
		return TenantMember{}, problem.Wrap(500, "tenant_member_update_failed", "Failed to load the tenant member.", err)
	}
	updates := map[string]any{}
	if input.Role != nil {
		role := normalizeRole(*input.Role)
		if !validTenantRole(role) {
			return TenantMember{}, problem.New(400, "invalid_tenant_role", "Tenant role is invalid.")
		}
		if (target.Role == "owner" || role == "owner") && actorRole != "owner" {
			return TenantMember{}, problem.New(403, "owner_update_forbidden", "Only an owner can change owner memberships.")
		}
		updates["role"] = role
	}
	if input.Status != nil {
		status := normalizeRole(*input.Status)
		if !validMembershipStatus(status) {
			return TenantMember{}, problem.New(400, "invalid_membership_status", "Membership status must be active or suspended.")
		}
		updates["status"] = status
		if status == "active" && target.JoinedAt == nil {
			updates["joined_at"] = time.Now().UTC()
		}
	}
	if len(updates) == 0 {
		return TenantMember{}, problem.New(400, "empty_update", "Provide a role or status to update.")
	}
	var suspension tenantuseraccess.SuspensionResult
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if status, ok := updates["status"]; ok && status == "suspended" {
			var err error
			suspension, err = tenantuseraccess.Suspend(ctx, tx, tenantID, userID, principal.UserID, time.Now().UTC())
			if err != nil {
				return err
			}
		}
		result := tx.Model(&persistence.TenantMembership{}).
			Where("tenant_id = ? AND user_id = ?", tenantID, userID).Updates(updates)
		if result.Error != nil {
			return problem.Wrap(409, "tenant_member_update_rejected", "Tenant membership update was rejected by an isolation invariant.", result.Error)
		}
		if result.RowsAffected == 0 {
			return problem.New(404, "tenant_member_not_found", "Tenant member not found.")
		}
		if role, changingRole := updates["role"].(string); changingRole {
			finalStatus := target.Status
			if status, changingStatus := updates["status"].(string); changingStatus {
				finalStatus = status
			}
			if err := syncRootOrganizationMembershipForTenantRole(
				ctx, tx, tenantID, userID, target.Role, role, finalStatus,
			); err != nil {
				return err
			}
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "tenant.member_updated", ResourceType: "user", ResourceID: &userID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"fromRole": target.Role, "toRole": updates["role"],
				"fromStatus": target.Status, "toStatus": updates["status"],
				"revokedSessionCount":           suspension.RevokedSessionCount,
				"revokedCredentialCount":        suspension.RevokedCredentialCount,
				"revokedDesktopEnrollmentCount": suspension.RevokedDesktopEnrollmentCount,
			},
		})
	}, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		if persistence.IsConstraintViolation(err) {
			return TenantMember{}, problem.Wrap(409, "tenant_member_update_rejected", "Tenant membership update was rejected by an isolation invariant.", err)
		}
		return TenantMember{}, err
	}
	return s.getTenantMember(ctx, tenantID, userID)
}

func syncRootOrganizationMembershipForTenantRole(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, userID uuid.UUID,
	previousRole, nextRole, membershipStatus string,
) error {
	var root persistence.Organization
	if err := tx.WithContext(ctx).
		Where("tenant_id = ? AND kind = ? AND archived_at IS NULL", tenantID, "root").
		Take(&root).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return problem.New(409, "root_organization_not_found", "The Tenant root Organization is unavailable.")
	} else if err != nil {
		return problem.Wrap(500, "root_organization_load_failed", "The Tenant root Organization could not be loaded.", err)
	}
	previousRoleGrantedRoot := previousRole == "owner" || previousRole == "admin"
	nextRoleGrantsRoot := nextRole == "owner" || nextRole == "admin"
	if previousRoleGrantedRoot && !nextRoleGrantsRoot {
		if err := tx.WithContext(ctx).
			Where(
				"tenant_id = ? AND organization_id = ? AND user_id = ? AND role = ?",
				tenantID, root.ID, userID, previousRole,
			).
			Delete(&persistence.OrganizationMembership{}).Error; err != nil {
			return problem.Wrap(500, "root_organization_membership_revoke_failed", "The automatic root Organization role could not be revoked.", err)
		}
		return nil
	}
	if !nextRoleGrantsRoot {
		return nil
	}
	membership := persistence.OrganizationMembership{
		TenantID: tenantID, OrganizationID: root.ID, UserID: userID,
		Role: nextRole, Status: membershipStatus,
	}
	if err := tx.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "organization_id"}, {Name: "user_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"role", "status", "updated_at"}),
	}).Create(&membership).Error; err != nil {
		return problem.Wrap(409, "root_organization_membership_update_rejected", "The automatic root Organization role could not be synchronized.", err)
	}
	return nil
}

func (s *Service) RemoveTenantMember(
	ctx context.Context,
	principal identity.Principal,
	tenantID, userID uuid.UUID,
	requestID, ipAddress string,
) error {
	actorRole, err := s.requireTenantPermission(ctx, principal, tenantID, authorization.TenantMembersRemove)
	if err != nil {
		return err
	}
	var target persistence.TenantMembership
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND user_id = ?", tenantID, userID).Take(&target).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return problem.New(404, "tenant_member_not_found", "Tenant member not found.")
	} else if err != nil {
		return problem.Wrap(500, "tenant_member_remove_failed", "Failed to load the tenant member.", err)
	}
	if target.Role == "owner" && actorRole != "owner" {
		return problem.New(403, "owner_remove_forbidden", "Only an owner can remove another owner.")
	}
	var suspension tenantuseraccess.SuspensionResult
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		suspension, err = tenantuseraccess.Suspend(ctx, tx, tenantID, userID, principal.UserID, time.Now().UTC())
		if err != nil {
			return err
		}
		if err := tx.Where("tenant_id = ? AND user_id = ?", tenantID, userID).
			Delete(&persistence.OrganizationMembership{}).Error; err != nil {
			return problem.Wrap(500, "tenant_member_remove_failed", "Failed to remove organization memberships.", err)
		}
		result := tx.Where("tenant_id = ? AND user_id = ?", tenantID, userID).
			Delete(&persistence.TenantMembership{})
		if result.Error != nil {
			return problem.Wrap(409, "tenant_member_remove_rejected", "Tenant member removal was rejected by an isolation invariant.", result.Error)
		}
		if result.RowsAffected == 0 {
			return problem.New(404, "tenant_member_not_found", "Tenant member not found.")
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "tenant.member_removed", ResourceType: "user", ResourceID: &userID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"role": target.Role, "status": target.Status,
				"revokedSessionCount":           suspension.RevokedSessionCount,
				"revokedCredentialCount":        suspension.RevokedCredentialCount,
				"revokedDesktopEnrollmentCount": suspension.RevokedDesktopEnrollmentCount,
			},
		})
	}, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if persistence.IsConstraintViolation(err) {
		return problem.Wrap(409, "tenant_member_remove_rejected", "Tenant member removal was rejected by an isolation invariant.", err)
	}
	return err
}

func (s *Service) getTenantMember(ctx context.Context, tenantID, userID uuid.UUID) (TenantMember, error) {
	var item TenantMember
	err := s.db.WithContext(ctx).Table("tenant_memberships AS tm").
		Select("tm.tenant_id, tm.user_id, u.email, u.display_name, tm.role, tm.status, tm.joined_at, tm.created_at, tm.updated_at").
		Joins("JOIN users AS u ON u.id = tm.user_id").
		Where("tm.tenant_id = ? AND tm.user_id = ?", tenantID, userID).Take(&item).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return TenantMember{}, problem.New(404, "tenant_member_not_found", "Tenant member not found.")
	}
	if err != nil {
		return TenantMember{}, problem.Wrap(500, "tenant_member_load_failed", "Failed to load the tenant member.", err)
	}
	return item, nil
}
