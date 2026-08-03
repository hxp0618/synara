package tenancy

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/productprofile"
	"github.com/synara-ai/synara/services/control-plane/internal/validation"
)

type Service struct {
	db                     *gorm.DB
	authorizer             *authorization.Authorizer
	tenantRepository       persistence.Repository[persistence.Tenant]
	organizationRepository persistence.Repository[persistence.Organization]
	executionCoordinator   TenantDeletionExecutionCoordinator
}

type TenantDeletionExecutionCoordinator interface {
	PrepareTenantDeletion(
		ctx context.Context,
		tx *gorm.DB,
		tenantID, actorID uuid.UUID,
		now time.Time,
	) ([]persistence.SessionEvent, error)
	PublishTenantDeletionEvents(events []persistence.SessionEvent)
}

func NewService(db *gorm.DB, coordinators ...TenantDeletionExecutionCoordinator) *Service {
	service := &Service{
		db:                     db,
		authorizer:             authorization.NewAuthorizer(db),
		tenantRepository:       persistence.NewRepository[persistence.Tenant](db),
		organizationRepository: persistence.NewRepository[persistence.Organization](db),
	}
	if len(coordinators) > 0 {
		service.executionCoordinator = coordinators[0]
	}
	return service
}

func (s *Service) requireTenantPermission(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	permission authorization.Permission,
) (string, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return "", err
	}
	return s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, permission)
}

func (s *Service) tenantRole(ctx context.Context, principal identity.Principal, tenantID uuid.UUID) (string, error) {
	if err := identity.RequireActiveTenant(principal, tenantID); err != nil {
		return "", err
	}
	return s.authorizer.TenantRole(ctx, principal.UserID, tenantID)
}

func normalizeTenantInput(input CreateTenantInput) (CreateTenantInput, error) {
	var err error
	input.Slug, err = validation.Slug(input.Slug, "invalid_tenant_slug", "Tenant slug")
	if err != nil {
		return CreateTenantInput{}, err
	}
	input.Name, err = validation.Name(input.Name, "invalid_tenant_name", "Tenant name", 160)
	if err != nil {
		return CreateTenantInput{}, err
	}
	input.Region, err = validation.Code(input.Region, "default", "invalid_tenant_region", "Tenant region")
	if err != nil {
		return CreateTenantInput{}, err
	}
	input.PlanCode, err = validation.Code(productprofile.InternalProfileCode(input.PlanCode), "free", "invalid_entitlement_profile_code", "Tenant entitlement profile code")
	if err != nil {
		return CreateTenantInput{}, err
	}
	input.Status = productprofile.InternalLifecycleStatus(input.Status)
	if input.Status == "" {
		input.Status = "active"
	}
	if input.Status != "active" && input.Status != "trialing" {
		return CreateTenantInput{}, problem.New(400, "invalid_initial_tenant_status", "Tenant status must be active or evaluation at registration.")
	}
	if input.Status == "trialing" {
		if input.TrialExpiresAt == nil || !input.TrialExpiresAt.After(time.Now().UTC()) {
			return CreateTenantInput{}, problem.New(400, "invalid_tenant_evaluation_expiry", "An evaluation Tenant requires a future evaluationExpiresAt.")
		}
	} else if input.TrialExpiresAt != nil {
		return CreateTenantInput{}, problem.New(400, "unexpected_tenant_evaluation_expiry", "evaluationExpiresAt is only valid for an evaluation Tenant.")
	}
	return input, nil
}

func normalizeLifecycleReason(value string) (string, error) {
	reason := strings.TrimSpace(value)
	if len(reason) < 3 || len(reason) > 500 {
		return "", problem.New(400, "invalid_tenant_lifecycle_reason", "Tenant lifecycle reason must contain between 3 and 500 characters.")
	}
	return reason, nil
}

func validTenantRole(role string) bool {
	switch role {
	case "owner", "admin", "security_admin", "cost_admin", "auditor", "member":
		return true
	default:
		return false
	}
}

func validOrganizationRole(role string) bool {
	switch role {
	case "owner", "admin", "agent_operator", "member", "viewer":
		return true
	default:
		return false
	}
}

func validMembershipStatus(status string) bool { return status == "active" || status == "suspended" }

func invitationExpiry() time.Time { return time.Now().UTC().Add(7 * 24 * time.Hour) }

func normalizeRole(value string) string { return strings.ToLower(strings.TrimSpace(value)) }
