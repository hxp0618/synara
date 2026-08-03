package authorization

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

type OrganizationAccess struct {
	TenantRole       string
	OrganizationRole string
}

type Authorizer struct {
	db *gorm.DB
}

func NewAuthorizer(db *gorm.DB) *Authorizer { return &Authorizer{db: db} }

func (a *Authorizer) ActiveSupportGrant(
	ctx context.Context,
	userID, tenantID uuid.UUID,
) (uuid.UUID, bool, error) {
	var row struct {
		ID uuid.UUID `gorm:"column:id"`
	}
	err := a.db.WithContext(ctx).Table("support_access_grants AS support_grant").
		Select("support_grant.id").
		Joins("JOIN tenant_support_policies AS policy ON policy.tenant_id = support_grant.tenant_id AND policy.support_access_enabled = ?", true).
		Joins("JOIN tenant_memberships AS operator_membership ON operator_membership.tenant_id = support_grant.operator_tenant_id AND operator_membership.user_id = support_grant.requester_user_id AND operator_membership.status = ? AND operator_membership.role IN ?", "active", []string{"owner", "admin", "security_admin"}).
		Joins("JOIN tenants AS operator_tenant ON operator_tenant.id = support_grant.operator_tenant_id AND operator_tenant.status = ? AND operator_tenant.deleted_at IS NULL", "active").
		Joins("JOIN users AS requester ON requester.id = support_grant.requester_user_id AND requester.status = ? AND requester.deleted_at IS NULL", "active").
		Where("support_grant.tenant_id = ? AND support_grant.requester_user_id = ? AND support_grant.status = ? AND support_grant.expires_at > CURRENT_TIMESTAMP", tenantID, userID, "active").
		Order("support_grant.expires_at DESC, support_grant.id").Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, problem.Wrap(500, "support_access_authorization_failed", "Support Access could not be authorized.", err)
	}
	return row.ID, true, nil
}

func (a *Authorizer) TenantRole(ctx context.Context, userID, tenantID uuid.UUID) (string, error) {
	var membership persistence.TenantMembership
	err := a.db.WithContext(ctx).
		Where("tenant_id = ? AND user_id = ? AND status = ?", tenantID, userID, "active").
		Take(&membership).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		if _, active, supportErr := a.ActiveSupportGrant(ctx, userID, tenantID); supportErr != nil {
			return "", supportErr
		} else if active {
			membership.Role = SupportReadOnlyRole
			err = nil
		} else {
			return "", problem.New(404, "tenant_not_found", "Tenant not found.")
		}
	}
	if err != nil {
		return "", problem.Wrap(500, "tenant_authorization_failed", "Failed to authorize tenant access.", err)
	}

	var tenant persistence.Tenant
	err = a.db.WithContext(ctx).
		Select("id").
		Where("id = ? AND deleted_at IS NULL", tenantID).
		Take(&tenant).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", problem.New(404, "tenant_not_found", "Tenant not found.")
	}
	if err != nil {
		return "", problem.Wrap(500, "tenant_authorization_failed", "Failed to authorize tenant access.", err)
	}
	return membership.Role, nil
}

func (a *Authorizer) RequireTenant(
	ctx context.Context,
	userID, tenantID uuid.UUID,
	permission Permission,
) (string, error) {
	role, err := a.TenantRole(ctx, userID, tenantID)
	if err != nil {
		return "", err
	}
	if !TenantAllows(role, permission) {
		return "", problem.New(403, "tenant_forbidden", "You do not have permission to perform this tenant action.")
	}
	return role, nil
}

func (a *Authorizer) RequireOrganization(
	ctx context.Context,
	userID, tenantID, organizationID uuid.UUID,
	permission Permission,
) (OrganizationAccess, error) {
	tenantRole, err := a.TenantRole(ctx, userID, tenantID)
	if err != nil {
		return OrganizationAccess{}, err
	}

	var organization persistence.Organization
	err = a.db.WithContext(ctx).
		Select("id", "status").
		Where("tenant_id = ? AND id = ? AND archived_at IS NULL", tenantID, organizationID).
		Take(&organization).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return OrganizationAccess{}, problem.New(404, "organization_not_found", "Organization not found.")
	}
	if err != nil {
		return OrganizationAccess{}, problem.Wrap(500, "organization_authorization_failed", "Failed to authorize organization access.", err)
	}
	if TenantAllows(tenantRole, permission) {
		return OrganizationAccess{TenantRole: tenantRole}, nil
	}

	var membership persistence.OrganizationMembership
	err = a.db.WithContext(ctx).
		Where("tenant_id = ? AND organization_id = ? AND user_id = ? AND status = ?", tenantID, organizationID, userID, "active").
		Take(&membership).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return OrganizationAccess{}, problem.New(404, "organization_not_found", "Organization not found.")
	}
	if err != nil {
		return OrganizationAccess{}, problem.Wrap(500, "organization_authorization_failed", "Failed to authorize organization access.", err)
	}
	if !OrganizationAllows(membership.Role, permission) {
		return OrganizationAccess{}, problem.New(403, "organization_forbidden", "You do not have permission to perform this organization action.")
	}
	return OrganizationAccess{TenantRole: tenantRole, OrganizationRole: membership.Role}, nil
}
