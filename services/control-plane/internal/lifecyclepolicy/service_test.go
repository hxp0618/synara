package lifecyclepolicy

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestLifecyclePoliciesRejectInactiveTenantBeforeStorageAccess(t *testing.T) {
	service, err := NewService(nil, DefaultConfig(platform.ProfilePersonal))
	if err != nil {
		t.Fatal(err)
	}
	activeTenantID := uuid.New()
	requestedTenantID := uuid.New()
	principal := identity.Principal{UserID: uuid.New(), ActiveTenantID: &activeTenantID}
	projectID := uuid.New()

	for name, operation := range map[string]func() error{
		"get Tenant policy": func() error {
			_, err := service.GetTenant(context.Background(), principal, requestedTenantID)
			return err
		},
		"update Tenant policy": func() error {
			_, err := service.UpdateTenant(context.Background(), principal, requestedTenantID, UpdateInput{}, "inactive-tenant", "127.0.0.1")
			return err
		},
		"get Project policy": func() error {
			_, err := service.GetProject(context.Background(), principal, requestedTenantID, projectID)
			return err
		},
		"update Project policy": func() error {
			_, err := service.UpdateProject(context.Background(), principal, requestedTenantID, projectID, UpdateInput{}, "inactive-project", "127.0.0.1")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			assertPolicyProblem(t, operation(), "tenant_not_found")
		})
	}
}

func TestLifecyclePolicyHierarchyBoundsAndOptimisticConcurrency(t *testing.T) {
	ctx := context.Background()
	platformConfig, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, platformConfig, "", filepath.Join(t.TempDir(), "lifecycle.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "lifecycle-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	project := persistence.Project{
		ID: uuid.New(), TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		Name: "Lifecycle project", DefaultBranch: "main", Visibility: "organization", CreatedBy: domain.UserID,
	}
	if err := store.DB().Create(&project).Error; err != nil {
		t.Fatal(err)
	}
	service, err := NewService(store.DB(), DefaultConfig(platform.ProfilePersonal))
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	waiting := 1200
	absolute := 7200
	tenantPolicy, err := service.UpdateTenant(ctx, principal, domain.TenantID, UpdateInput{
		ExpectedVersion: 0,
		Overrides: Overrides{
			WaitingKeepAliveSeconds: &waiting, AbsoluteSessionLifetimeSeconds: &absolute,
		},
	}, "tenant-lifecycle", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if tenantPolicy.Version != 1 || tenantPolicy.Effective.WaitingKeepAliveSeconds != waiting ||
		tenantPolicy.Effective.AbsoluteSessionLifetimeSeconds == nil ||
		*tenantPolicy.Effective.AbsoluteSessionLifetimeSeconds != absolute {
		t.Fatalf("unexpected Tenant policy: %#v", tenantPolicy)
	}

	suspendAfterIdle := 600
	warmPool := "low-latency"
	projectPolicy, err := service.UpdateProject(ctx, principal, domain.TenantID, project.ID, UpdateInput{
		ExpectedVersion: 0,
		Overrides: Overrides{
			SuspendAfterIdleSeconds: &suspendAfterIdle, WarmPoolMode: &warmPool,
		},
	}, "project-lifecycle", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if projectPolicy.Effective.WaitingKeepAliveSeconds != waiting ||
		projectPolicy.Effective.SuspendAfterIdleSeconds != suspendAfterIdle ||
		projectPolicy.Effective.WarmPoolMode != warmPool {
		t.Fatalf("Project policy did not inherit and override deterministically: %#v", projectPolicy)
	}

	workspaceRetention := 7
	effective, err := service.ResolveForSession(ctx, store.DB(), domain.TenantID, project.ID, &Overrides{
		WorkspaceRetentionDays: &workspaceRetention,
	})
	if err != nil {
		t.Fatal(err)
	}
	if effective.WaitingKeepAliveSeconds != waiting || effective.SuspendAfterIdleSeconds != suspendAfterIdle ||
		effective.WorkspaceRetentionDays != workspaceRetention || effective.WarmPoolMode != warmPool ||
		effective.AbsoluteSessionLifetimeSeconds == nil || *effective.AbsoluteSessionLifetimeSeconds != absolute {
		t.Fatalf("Session effective policy is not the expected hierarchy: %#v", effective)
	}

	_, err = service.UpdateTenant(ctx, principal, domain.TenantID, UpdateInput{
		ExpectedVersion: 0, Overrides: Overrides{WaitingKeepAliveSeconds: &waiting},
	}, "stale-policy", "127.0.0.1")
	assertPolicyProblem(t, err, "lifecycle_policy_version_conflict")
	tooShort := 59
	_, err = service.UpdateProject(ctx, principal, domain.TenantID, project.ID, UpdateInput{
		ExpectedVersion: 1, Overrides: Overrides{WaitingKeepAliveSeconds: &tooShort},
	}, "invalid-policy", "127.0.0.1")
	assertPolicyProblem(t, err, "invalid_waiting_keep_alive")
}

func assertPolicyProblem(t *testing.T, err error, code string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != code {
		t.Fatalf("expected problem code %q, got %v", code, err)
	}
}
