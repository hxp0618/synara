package tenancy

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/schedulingpolicy"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestTenantHomeRegionCannotEscapeExecutionRegionBoundary(t *testing.T) {
	ctx := context.Background()
	config, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "tenant-region-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	policyService := schedulingpolicy.NewService(store.DB())
	document := schedulingpolicy.UnrestrictedDocument()
	document.Region = schedulingpolicy.Rule{Mode: schedulingpolicy.ModeAllow, Values: []string{"local"}}
	snapshot, err := policyService.UpdateTenant(ctx, domain.TenantID, schedulingpolicy.UpdateInput{
		ExpectedVersion: 0, Document: document, ActorID: domain.UserID,
		RequestID: "tenant-region-policy", IPAddress: "127.0.0.1",
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store.DB())
	outsideRegion := "cn-east-1"
	if _, err := service.UpdateTenant(ctx, principal, domain.TenantID, UpdateTenantInput{
		Region: &outsideRegion,
	}, "tenant-region-outside", "127.0.0.1"); err == nil {
		t.Fatal("Tenant home Region escaped the enforced execution Region boundary")
	} else {
		assertTenantLifecycleProblem(t, err, "tenant_region_outside_execution_boundary")
	}
	document.Region.Values = []string{"cn-east-1", "local"}
	if _, err := policyService.UpdateTenant(ctx, domain.TenantID, schedulingpolicy.UpdateInput{
		ExpectedVersion: snapshot.Tenant.Version, Document: document, ActorID: domain.UserID,
		RequestID: "tenant-region-policy-expand", IPAddress: "127.0.0.1",
	}); err != nil {
		t.Fatal(err)
	}
	updated, err := service.UpdateTenant(ctx, principal, domain.TenantID, UpdateTenantInput{
		Region: &outsideRegion,
	}, "tenant-region-inside", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Region != outsideRegion {
		t.Fatalf("Tenant Region = %q, want %q", updated.Region, outsideRegion)
	}
}
