package identity

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func TestRequireActiveTenantFailsClosed(t *testing.T) {
	activeTenantID := uuid.New()
	zeroTenantID := uuid.Nil
	tests := []struct {
		name      string
		principal Principal
		tenantID  uuid.UUID
		wantError bool
	}{
		{name: "exact active Tenant", principal: Principal{ActiveTenantID: &activeTenantID}, tenantID: activeTenantID},
		{name: "missing active Tenant", principal: Principal{}, tenantID: activeTenantID, wantError: true},
		{name: "zero active Tenant", principal: Principal{ActiveTenantID: &zeroTenantID}, tenantID: activeTenantID, wantError: true},
		{name: "substituted Tenant", principal: Principal{ActiveTenantID: &activeTenantID}, tenantID: uuid.New(), wantError: true},
		{name: "zero requested Tenant", principal: Principal{ActiveTenantID: &zeroTenantID}, tenantID: uuid.Nil, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := RequireActiveTenant(test.principal, test.tenantID)
			if !test.wantError {
				if err != nil {
					t.Fatalf("RequireActiveTenant() error = %v", err)
				}
				return
			}
			var apiError *problem.Error
			if !errors.As(err, &apiError) || apiError.Status != 404 || apiError.Code != "tenant_not_found" {
				t.Fatalf("RequireActiveTenant() error = %#v, want 404 tenant_not_found", err)
			}
		})
	}
}
