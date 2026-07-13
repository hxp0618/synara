package tenancy

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
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
	userID uuid.UUID,
	tenantID uuid.UUID,
	permission authorization.Permission,
) (string, error) {
	return s.authorizer.RequireTenant(ctx, userID, tenantID, permission)
}

func (s *Service) tenantRole(ctx context.Context, userID, tenantID uuid.UUID) (string, error) {
	return s.authorizer.TenantRole(ctx, userID, tenantID)
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
	input.PlanCode, err = validation.Code(input.PlanCode, "free", "invalid_plan_code", "Tenant plan code")
	if err != nil {
		return CreateTenantInput{}, err
	}
	return input, nil
}

func validTenantRole(role string) bool {
	switch role {
	case "owner", "admin", "security_admin", "billing_admin", "auditor", "member":
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
