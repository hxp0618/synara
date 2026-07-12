package tenancy

import (
	"context"
	"database/sql"
	"errors"
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
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/internal/validation"
)

func toTenant(model persistence.Tenant, role string) Tenant {
	settings := model.Settings
	if settings == nil {
		settings = map[string]any{}
	}
	return Tenant{
		ID: model.ID, Slug: model.Slug, Name: model.Name, Status: model.Status,
		PlanCode: model.PlanCode, Region: model.Region, Settings: settings, Role: role,
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
	normalized, err := normalizeTenantInput(input)
	if err != nil {
		return Tenant{}, err
	}
	tenantID, organizationID := uuid.New(), uuid.New()
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		if err := tx.Create(&persistence.Tenant{
			ID: tenantID, Slug: normalized.Slug, Name: normalized.Name, Status: "active",
			PlanCode: normalized.PlanCode, Region: normalized.Region, Settings: map[string]any{},
			CreatedBy: principal.UserID,
		}).Error; err != nil {
			return problem.Wrap(409, "tenant_slug_conflict", "A tenant with this slug already exists.", err)
		}
		now := time.Now().UTC()
		if err := tx.Create(&persistence.TenantMembership{
			TenantID: tenantID, UserID: principal.UserID, Role: "owner", Status: "active", JoinedAt: &now,
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
			TenantID: tenantID, OrganizationID: organizationID, UserID: principal.UserID,
			Role: "owner", Status: "active",
		}).Error; err != nil {
			return problem.Wrap(500, "tenant_create_failed", "Failed to create the root organization owner.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "tenant.created", ResourceType: "tenant", ResourceID: &tenantID,
			OrganizationID: &organizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"slug": normalized.Slug, "planCode": normalized.PlanCode},
		})
	})
	if err != nil {
		return Tenant{}, err
	}
	return s.GetTenant(ctx, principal, tenantID)
}

func (s *Service) GetTenant(ctx context.Context, principal identity.Principal, tenantID uuid.UUID) (Tenant, error) {
	role, err := s.requireTenantPermission(ctx, principal.UserID, tenantID, authorization.TenantRead)
	if err != nil {
		return Tenant{}, err
	}
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

func (s *Service) UpdateTenant(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	input UpdateTenantInput,
	requestID string,
	ipAddress string,
) (Tenant, error) {
	role, err := s.requireTenantPermission(ctx, principal.UserID, tenantID, authorization.TenantUpdate)
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
		if *input.Status != "active" && *input.Status != "suspended" {
			return Tenant{}, problem.New(400, "invalid_tenant_status", "Tenant status must be active or suspended.")
		}
		if role != "owner" {
			return Tenant{}, problem.New(403, "tenant_status_forbidden", "Only a tenant owner can change tenant status.")
		}
		updates["status"] = *input.Status
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
	if _, err := s.requireTenantPermission(ctx, principal.UserID, tenantID, authorization.TenantDelete); err != nil {
		return err
	}
	return persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		now := time.Now().UTC()
		result := tx.Model(&persistence.Tenant{}).Where("id = ? AND deleted_at IS NULL", tenantID).
			Updates(map[string]any{"status": "deleting", "deleted_at": now})
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
		})
	})
}

func (s *Service) ListTenantMembers(ctx context.Context, principal identity.Principal, tenantID uuid.UUID) ([]TenantMember, error) {
	if _, err := s.requireTenantPermission(ctx, principal.UserID, tenantID, authorization.TenantMembersRead); err != nil {
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
	actorRole, err := s.requireTenantPermission(ctx, principal.UserID, tenantID, authorization.TenantMembersInvite)
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
	return s.GetTenant(ctx, principal, tenantID)
}

func (s *Service) UpdateTenantMember(
	ctx context.Context,
	principal identity.Principal,
	tenantID, userID uuid.UUID,
	input UpdateTenantMemberInput,
	requestID, ipAddress string,
) (TenantMember, error) {
	actorRole, err := s.requireTenantPermission(ctx, principal.UserID, tenantID, authorization.TenantMembersUpdate)
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
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if status, ok := updates["status"]; ok && status == "suspended" {
			if err := tx.Model(&persistence.OrganizationMembership{}).
				Where("tenant_id = ? AND user_id = ? AND status = ?", tenantID, userID, "active").
				Update("status", "suspended").Error; err != nil {
				return problem.Wrap(500, "tenant_member_update_failed", "Failed to suspend organization memberships.", err)
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
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "tenant.member_updated", ResourceType: "user", ResourceID: &userID,
			RequestID: requestID, IPAddress: ipAddress,
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

func (s *Service) RemoveTenantMember(
	ctx context.Context,
	principal identity.Principal,
	tenantID, userID uuid.UUID,
	requestID, ipAddress string,
) error {
	actorRole, err := s.requireTenantPermission(ctx, principal.UserID, tenantID, authorization.TenantMembersRemove)
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
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
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
