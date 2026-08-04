package authorization

import (
	"context"

	"github.com/google/uuid"
)

type MachinePrincipal struct {
	ActorID        uuid.UUID
	TenantID       uuid.UUID
	OrganizationID *uuid.UUID
	Role           string
}

type machinePrincipalContextKey struct{}

func WithMachinePrincipal(ctx context.Context, principal MachinePrincipal) context.Context {
	return context.WithValue(ctx, machinePrincipalContextKey{}, principal)
}

func MachinePrincipalFromContext(ctx context.Context) (MachinePrincipal, bool) {
	principal, ok := ctx.Value(machinePrincipalContextKey{}).(MachinePrincipal)
	return principal, ok
}
