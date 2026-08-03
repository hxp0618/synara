package identity

import (
	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

// RequireActiveTenant is the common user-session boundary for services that
// accept an explicit Tenant ID. Membership and RBAC checks are intentionally
// separate: holding access to more than one Tenant must not let a caller use a
// stale or substituted Tenant context.
func RequireActiveTenant(principal Principal, tenantID uuid.UUID) error {
	if tenantID == uuid.Nil || principal.ActiveTenantID == nil ||
		*principal.ActiveTenantID == uuid.Nil || *principal.ActiveTenantID != tenantID {
		return problem.New(404, "tenant_not_found", "Tenant not found.")
	}
	return nil
}
