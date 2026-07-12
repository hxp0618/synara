package tenancy

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/validation"
)

func toOrganization(model persistence.Organization, currentUserRole *string) Organization {
	settings := model.Settings
	if settings == nil {
		settings = map[string]any{}
	}
	return Organization{
		ID: model.ID, TenantID: model.TenantID, ParentOrganizationID: model.ParentOrganizationID,
		Slug: model.Slug, Name: model.Name, Kind: model.Kind, Status: model.Status,
		CurrentUserRole: currentUserRole, Settings: settings, CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt,
		ArchivedAt: model.ArchivedAt,
	}
}

func (s *Service) activeOrganizationRoles(
	ctx context.Context,
	tenantID, userID uuid.UUID,
) (map[uuid.UUID]string, error) {
	models := make([]persistence.OrganizationMembership, 0)
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND user_id = ? AND status = ?", tenantID, userID, "active").
		Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "organization_roles_load_failed", "Failed to load Organization roles.", err)
	}
	roles := make(map[uuid.UUID]string, len(models))
	for _, model := range models {
		roles[model.OrganizationID] = model.Role
	}
	return roles, nil
}

func (s *Service) requireOrganizationPermission(
	ctx context.Context,
	principal identity.Principal,
	tenantID, organizationID uuid.UUID,
	permission authorization.Permission,
) error {
	_, err := s.authorizer.RequireOrganization(ctx, principal.UserID, tenantID, organizationID, permission)
	return err
}

func (s *Service) ListOrganizations(ctx context.Context, principal identity.Principal, tenantID uuid.UUID) ([]Organization, error) {
	tenantRole, err := s.tenantRole(ctx, principal.UserID, tenantID)
	if err != nil {
		return nil, err
	}
	query := s.db.WithContext(ctx).Model(&persistence.Organization{}).
		Where("organizations.tenant_id = ? AND organizations.archived_at IS NULL", tenantID)
	if !authorization.TenantAllows(tenantRole, authorization.OrganizationRead) {
		query = query.Joins("JOIN organization_memberships AS om ON om.organization_id = organizations.id AND om.tenant_id = organizations.tenant_id").
			Where("om.user_id = ? AND om.status = ?", principal.UserID, "active")
	}
	models := make([]persistence.Organization, 0)
	if err := query.Order("CASE organizations.kind WHEN 'root' THEN 0 ELSE 1 END, LOWER(organizations.name), organizations.id").Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "organizations_load_failed", "Failed to load organizations.", err)
	}
	roles, err := s.activeOrganizationRoles(ctx, tenantID, principal.UserID)
	if err != nil {
		return nil, err
	}
	result := make([]Organization, 0, len(models))
	for _, model := range models {
		role, found := roles[model.ID]
		if found {
			result = append(result, toOrganization(model, &role))
		} else {
			result = append(result, toOrganization(model, nil))
		}
	}
	return result, nil
}

func (s *Service) GetOrganization(ctx context.Context, principal identity.Principal, tenantID, organizationID uuid.UUID) (Organization, error) {
	if err := s.requireOrganizationPermission(ctx, principal, tenantID, organizationID, authorization.OrganizationRead); err != nil {
		return Organization{}, err
	}
	model, err := s.organizationRepository.First(ctx,
		persistence.TenantScope(tenantID),
		func(db *gorm.DB) *gorm.DB { return db.Where("id = ? AND archived_at IS NULL", organizationID) },
	)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Organization{}, problem.New(404, "organization_not_found", "Organization not found.")
	}
	if err != nil {
		return Organization{}, problem.Wrap(500, "organization_load_failed", "Failed to load the organization.", err)
	}
	roles, err := s.activeOrganizationRoles(ctx, tenantID, principal.UserID)
	if err != nil {
		return Organization{}, err
	}
	role, found := roles[model.ID]
	if !found {
		return toOrganization(model, nil), nil
	}
	return toOrganization(model, &role), nil
}

func (s *Service) CreateOrganization(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	input CreateOrganizationInput,
	requestID, ipAddress string,
) (Organization, error) {
	if _, err := s.requireTenantPermission(ctx, principal.UserID, tenantID, authorization.OrganizationUpdate); err != nil {
		return Organization{}, err
	}
	var err error
	input.Slug, err = validation.Slug(input.Slug, "invalid_organization_slug", "Organization slug")
	if err != nil {
		return Organization{}, err
	}
	input.Name, err = validation.Name(input.Name, "invalid_organization_name", "Organization name", 160)
	if err != nil {
		return Organization{}, err
	}
	input.Kind = normalizeRole(input.Kind)
	if input.Kind != "team" && input.Kind != "department" && input.Kind != "personal" {
		return Organization{}, problem.New(400, "invalid_organization_kind", "Organization kind must be team, department, or personal.")
	}
	settings := input.Settings
	if settings == nil {
		settings = map[string]any{}
	}
	organizationID := uuid.New()
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		model := persistence.Organization{
			ID: organizationID, TenantID: tenantID, ParentOrganizationID: input.ParentOrganizationID,
			Slug: input.Slug, Name: input.Name, Kind: input.Kind, Status: "active",
			Settings: settings, CreatedBy: principal.UserID,
		}
		if err := tx.Create(&model).Error; err != nil {
			return problem.Wrap(409, "organization_create_rejected", "Organization creation was rejected by a tenant isolation or slug constraint.", err)
		}
		if err := tx.Create(&persistence.OrganizationMembership{
			TenantID: tenantID, OrganizationID: organizationID, UserID: principal.UserID,
			Role: "owner", Status: "active",
		}).Error; err != nil {
			return problem.Wrap(500, "organization_create_failed", "Failed to create the organization owner.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "organization.created", ResourceType: "organization", ResourceID: &organizationID,
			OrganizationID: &organizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"slug": input.Slug, "kind": input.Kind},
		})
	})
	if err != nil {
		return Organization{}, err
	}
	return s.GetOrganization(ctx, principal, tenantID, organizationID)
}

func (s *Service) UpdateOrganization(
	ctx context.Context,
	principal identity.Principal,
	tenantID, organizationID uuid.UUID,
	input UpdateOrganizationInput,
	requestID, ipAddress string,
) (Organization, error) {
	if err := s.requireOrganizationPermission(ctx, principal, tenantID, organizationID, authorization.OrganizationUpdate); err != nil {
		return Organization{}, err
	}
	updates := map[string]any{}
	if input.Name != nil {
		name, err := validation.Name(*input.Name, "invalid_organization_name", "Organization name", 160)
		if err != nil {
			return Organization{}, err
		}
		updates["name"] = name
	}
	if input.Status != nil {
		status := normalizeRole(*input.Status)
		if status != "active" && status != "suspended" {
			return Organization{}, problem.New(400, "invalid_organization_status", "Organization status must be active or suspended.")
		}
		updates["status"] = status
	}
	if input.Settings != nil {
		updates["settings"] = *input.Settings
	}
	if len(updates) == 0 {
		return Organization{}, problem.New(400, "empty_update", "Provide at least one organization field to update.")
	}
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		result := tx.Model(&persistence.Organization{}).
			Where("tenant_id = ? AND id = ? AND archived_at IS NULL", tenantID, organizationID).Updates(updates)
		if result.Error != nil {
			return problem.Wrap(500, "organization_update_failed", "Failed to update the organization.", result.Error)
		}
		if result.RowsAffected == 0 {
			return problem.New(404, "organization_not_found", "Organization not found.")
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "organization.updated", ResourceType: "organization", ResourceID: &organizationID,
			OrganizationID: &organizationID, RequestID: requestID, IPAddress: ipAddress,
		})
	})
	if err != nil {
		return Organization{}, err
	}
	return s.GetOrganization(ctx, principal, tenantID, organizationID)
}

func (s *Service) ArchiveOrganization(
	ctx context.Context,
	principal identity.Principal,
	tenantID, organizationID uuid.UUID,
	requestID, ipAddress string,
) error {
	if err := s.requireOrganizationPermission(ctx, principal, tenantID, organizationID, authorization.OrganizationUpdate); err != nil {
		return err
	}
	return persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		result := tx.Model(&persistence.Organization{}).
			Where("tenant_id = ? AND id = ? AND kind <> ? AND archived_at IS NULL", tenantID, organizationID, "root").
			Updates(map[string]any{"status": "suspended", "archived_at": time.Now().UTC()})
		if result.Error != nil {
			return problem.Wrap(409, "organization_archive_rejected", "Organization archival was rejected because dependent resources still exist.", result.Error)
		}
		if result.RowsAffected == 0 {
			return problem.New(409, "organization_archive_rejected", "Root or missing organizations cannot be archived.")
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "organization.archived", ResourceType: "organization", ResourceID: &organizationID,
			OrganizationID: &organizationID, RequestID: requestID, IPAddress: ipAddress,
		})
	})
}

func (s *Service) ListOrganizationMembers(ctx context.Context, principal identity.Principal, tenantID, organizationID uuid.UUID) ([]OrganizationMember, error) {
	if err := s.requireOrganizationPermission(ctx, principal, tenantID, organizationID, authorization.OrganizationRead); err != nil {
		return nil, err
	}
	items := make([]OrganizationMember, 0)
	err := s.db.WithContext(ctx).Table("organization_memberships AS om").
		Select("om.tenant_id, om.organization_id, om.user_id, u.email, u.display_name, om.role, om.status, om.created_at, om.updated_at").
		Joins("JOIN users AS u ON u.id = om.user_id").
		Where("om.tenant_id = ? AND om.organization_id = ?", tenantID, organizationID).
		Order("CASE om.role WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END, LOWER(u.display_name), om.user_id").
		Scan(&items).Error
	if err != nil {
		return nil, problem.Wrap(500, "organization_members_load_failed", "Failed to load organization members.", err)
	}
	return items, nil
}

func (s *Service) PutOrganizationMember(
	ctx context.Context,
	principal identity.Principal,
	tenantID, organizationID uuid.UUID,
	input PutOrganizationMemberInput,
	requestID, ipAddress string,
) (OrganizationMember, error) {
	if err := s.requireOrganizationPermission(ctx, principal, tenantID, organizationID, authorization.OrganizationMembers); err != nil {
		return OrganizationMember{}, err
	}
	input.Role = normalizeRole(input.Role)
	input.Status = normalizeRole(input.Status)
	if !validOrganizationRole(input.Role) {
		return OrganizationMember{}, problem.New(400, "invalid_organization_role", "Organization role is invalid.")
	}
	if input.Status == "" {
		input.Status = "active"
	}
	if !validMembershipStatus(input.Status) {
		return OrganizationMember{}, problem.New(400, "invalid_membership_status", "Membership status must be active or suspended.")
	}
	model := persistence.OrganizationMembership{
		TenantID: tenantID, OrganizationID: organizationID, UserID: input.UserID,
		Role: input.Role, Status: input.Status,
	}
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "organization_id"}, {Name: "user_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"role", "status"}),
		}).Create(&model).Error; err != nil {
			return problem.Wrap(409, "organization_member_update_rejected", "The user must be an active member of the same tenant.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "organization.member_updated", ResourceType: "user", ResourceID: &input.UserID,
			OrganizationID: &organizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"role": input.Role, "status": input.Status},
		})
	})
	if err != nil {
		return OrganizationMember{}, err
	}
	return s.getOrganizationMember(ctx, tenantID, organizationID, input.UserID)
}

func (s *Service) UpdateOrganizationMember(
	ctx context.Context,
	principal identity.Principal,
	tenantID, organizationID, userID uuid.UUID,
	input UpdateOrganizationMemberInput,
	requestID, ipAddress string,
) (OrganizationMember, error) {
	if err := s.requireOrganizationPermission(ctx, principal, tenantID, organizationID, authorization.OrganizationMembers); err != nil {
		return OrganizationMember{}, err
	}
	updates := map[string]any{}
	if input.Role != nil {
		role := normalizeRole(*input.Role)
		if !validOrganizationRole(role) {
			return OrganizationMember{}, problem.New(400, "invalid_organization_role", "Organization role is invalid.")
		}
		updates["role"] = role
	}
	if input.Status != nil {
		status := normalizeRole(*input.Status)
		if !validMembershipStatus(status) {
			return OrganizationMember{}, problem.New(400, "invalid_membership_status", "Membership status must be active or suspended.")
		}
		updates["status"] = status
	}
	if len(updates) == 0 {
		return OrganizationMember{}, problem.New(400, "empty_update", "Provide a role or status to update.")
	}
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		result := tx.Model(&persistence.OrganizationMembership{}).
			Where("tenant_id = ? AND organization_id = ? AND user_id = ?", tenantID, organizationID, userID).Updates(updates)
		if result.Error != nil {
			return problem.Wrap(409, "organization_member_update_rejected", "Organization membership update was rejected.", result.Error)
		}
		if result.RowsAffected == 0 {
			return problem.New(404, "organization_member_not_found", "Organization member not found.")
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "organization.member_updated", ResourceType: "user", ResourceID: &userID,
			OrganizationID: &organizationID, RequestID: requestID, IPAddress: ipAddress,
		})
	})
	if err != nil {
		return OrganizationMember{}, err
	}
	return s.getOrganizationMember(ctx, tenantID, organizationID, userID)
}

func (s *Service) RemoveOrganizationMember(
	ctx context.Context,
	principal identity.Principal,
	tenantID, organizationID, userID uuid.UUID,
	requestID, ipAddress string,
) error {
	if err := s.requireOrganizationPermission(ctx, principal, tenantID, organizationID, authorization.OrganizationMembers); err != nil {
		return err
	}
	return persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		result := tx.Where("tenant_id = ? AND organization_id = ? AND user_id = ?", tenantID, organizationID, userID).
			Delete(&persistence.OrganizationMembership{})
		if result.Error != nil {
			return problem.Wrap(500, "organization_member_remove_failed", "Failed to remove the organization member.", result.Error)
		}
		if result.RowsAffected == 0 {
			return problem.New(404, "organization_member_not_found", "Organization member not found.")
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "organization.member_removed", ResourceType: "user", ResourceID: &userID,
			OrganizationID: &organizationID, RequestID: requestID, IPAddress: ipAddress,
		})
	})
}

func (s *Service) getOrganizationMember(ctx context.Context, tenantID, organizationID, userID uuid.UUID) (OrganizationMember, error) {
	var item OrganizationMember
	err := s.db.WithContext(ctx).Table("organization_memberships AS om").
		Select("om.tenant_id, om.organization_id, om.user_id, u.email, u.display_name, om.role, om.status, om.created_at, om.updated_at").
		Joins("JOIN users AS u ON u.id = om.user_id").
		Where("om.tenant_id = ? AND om.organization_id = ? AND om.user_id = ?", tenantID, organizationID, userID).
		Take(&item).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return OrganizationMember{}, problem.New(404, "organization_member_not_found", "Organization member not found.")
	}
	if err != nil {
		return OrganizationMember{}, problem.Wrap(500, "organization_member_load_failed", "Failed to load the organization member.", err)
	}
	return item, nil
}
