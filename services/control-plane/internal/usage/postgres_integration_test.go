package usage

import (
	"context"
	"io/fs"
	"os"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/tenancy"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/postgresisolation"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresConcurrentSoftQuotaProjectionIsIdempotent(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	db, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := database.Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "postgres-soft-quota-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	planCode := "soft-quota-" + uuid.NewString()[:8]
	if err := db.Create(&persistence.SaaSPlan{
		Code: planCode, DisplayName: "Soft quota integration", Status: "active", Version: 1,
	}).Error; err != nil {
		t.Fatal(err)
	}
	limit, warning := int64(10), int64(80)
	for _, entitlement := range []persistence.PlanEntitlement{
		{PlanCode: planCode, Key: executionSecondsEntitlement, ValueKind: "integer", IntegerValue: &limit},
		{PlanCode: planCode, Key: softWarningEntitlement, ValueKind: "integer", IntegerValue: &warning},
	} {
		if err := db.Create(&entitlement).Error; err != nil {
			t.Fatal(err)
		}
	}
	tenant, err := tenancy.NewService(db).CreateTenant(ctx, principal, tenancy.CreateTenantInput{
		Slug: "pg-soft-quota-" + uuid.NewString()[:8], Name: "PostgreSQL soft quota", PlanCode: planCode, Status: "active",
	}, "pg-soft-quota-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	var organization persistence.Organization
	if err := db.Where("tenant_id = ? AND kind = ?", tenant.ID, "root").Take(&organization).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	target := persistence.ExecutionTarget{
		ID: uuid.New(), TenantID: &tenant.ID, OrganizationID: &organization.ID,
		Kind: "local", Name: "soft-quota-target-" + uuid.NewString()[:8], Status: "active",
		ConfigurationEncrypted: []byte("{}"), Capabilities: map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	projectID, sessionID, turnID, executionID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	provider := "codex"
	for _, model := range []any{
		&persistence.Project{ID: projectID, TenantID: tenant.ID, OrganizationID: organization.ID, Name: "Soft quota", DefaultBranch: "main", Visibility: "organization", CreatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now},
		&persistence.AgentSession{ID: sessionID, TenantID: tenant.ID, OrganizationID: organization.ID, ProjectID: projectID, CreatedBy: domain.UserID, Title: "Soft quota", Status: "active", Visibility: "private", Provider: provider, ExecutionTargetID: target.ID, ProviderResumeCursorState: "absent", ResourceState: "active", MeaningfulActivityAt: now, WaitingKeepAliveSeconds: 900, SuspendAfterIdleSeconds: 1800, WorkspaceRetentionDays: 30, WarmPoolMode: "disabled", CreatedAt: now, UpdatedAt: now},
		&persistence.AgentTurn{ID: turnID, TenantID: tenant.ID, SessionID: sessionID, CreatedBy: domain.UserID, Status: "completed", InputText: "Use quota", TurnKind: "message", RuntimeMode: "approval-required", InteractionMode: "default", StartedAt: &now, CompletedAt: &now, CreatedAt: now},
		&persistence.AgentExecution{ID: executionID, TenantID: tenant.ID, SessionID: sessionID, TurnID: turnID, Attempt: 1, Status: "completed", ExecutionTargetID: target.ID, TargetKind: "local", WarmPoolModeSnapshot: "disabled", Generation: 1, Provider: &provider, RequestedBy: domain.UserID, QueuedAt: now, StartedAt: &now, FinishedAt: &now},
		&persistence.ExecutionUsageSummary{TenantID: tenant.ID, ExecutionID: executionID, Generation: 1, SessionID: sessionID, TurnID: turnID, Provider: provider, DurationMillis: 10001, CurrencyCode: "USD", Final: true, LatestEventSequence: 1},
	} {
		if err := db.Create(model).Error; err != nil {
			t.Fatalf("seed %T: %v", model, err)
		}
	}
	secondDB, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	secondSQLDB, err := secondDB.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = secondSQLDB.Close() })
	start := make(chan struct{})
	errorsByProjector := make(chan error, 2)
	var wait sync.WaitGroup
	for _, projectorDB := range []*gorm.DB{db, secondDB} {
		wait.Add(1)
		go func(projectorDB *gorm.DB) {
			defer wait.Done()
			<-start
			errorsByProjector <- ProjectSoftQuotaAlerts(ctx, projectorDB, tenant.ID, now)
		}(projectorDB)
	}
	close(start)
	wait.Wait()
	close(errorsByProjector)
	for projectionErr := range errorsByProjector {
		if projectionErr != nil {
			t.Fatalf("concurrent soft quota projection failed: %v", projectionErr)
		}
	}
	var alertCount int64
	if err := db.Model(&persistence.TenantUsageQuotaAlert{}).Where("tenant_id = ?", tenant.ID).Count(&alertCount).Error; err != nil {
		t.Fatal(err)
	}
	if alertCount != 2 {
		t.Fatalf("PostgreSQL soft quota alert rows = %d, want 2", alertCount)
	}
}

func TestProviderCostReportingMigrationPreservesUnknownVersusExplicitCost(t *testing.T) {
	databaseURL := os.Getenv("TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("TEST_POSTGRES_URL is not configured")
	}
	ctx := context.Background()
	db, err := database.Open(ctx, postgresisolation.URL(t, databaseURL))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := database.Migrate(ctx, db, usageMigrationsThrough(t, "000149_stage6_incident_resolution_approval_evidence_digest.sql")); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "provider-cost-reporting-upgrade-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	projectID, sessionID := uuid.New(), uuid.New()
	provider := "codex"
	for _, model := range []any{
		&persistence.Project{ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID, Name: "Provider cost coverage", DefaultBranch: "main", Visibility: "organization", CreatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now},
		&persistence.AgentSession{ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID, ProjectID: projectID, CreatedBy: domain.UserID, Title: "Provider cost coverage", Status: "active", Visibility: "private", Provider: provider, ExecutionTargetID: domain.ExecutionTargetID, ProviderResumeCursorState: "absent", ResourceState: "active", MeaningfulActivityAt: now, WaitingKeepAliveSeconds: 900, SuspendAfterIdleSeconds: 1800, WorkspaceRetentionDays: 30, WarmPoolMode: "disabled", CreatedAt: now, UpdatedAt: now},
	} {
		if err := db.Create(model).Error; err != nil {
			t.Fatalf("seed %T: %v", model, err)
		}
	}
	type usageIdentity struct{ TurnID, ExecutionID uuid.UUID }
	identities := []usageIdentity{{uuid.New(), uuid.New()}, {uuid.New(), uuid.New()}}
	for index, identity := range identities {
		turn := persistence.AgentTurn{ID: identity.TurnID, TenantID: domain.TenantID, SessionID: sessionID, CreatedBy: domain.UserID, Status: "completed", InputText: "Explain Provider cost coverage", TurnKind: "message", RuntimeMode: "approval-required", InteractionMode: "default", StartedAt: &now, CompletedAt: &now, CreatedAt: now}
		if err := db.Create(&turn).Error; err != nil {
			t.Fatal(err)
		}
		execution := persistence.AgentExecution{ID: identity.ExecutionID, TenantID: domain.TenantID, SessionID: sessionID, TurnID: identity.TurnID, Attempt: 1, Status: "completed", ExecutionTargetID: domain.ExecutionTargetID, TargetKind: "local", WarmPoolModeSnapshot: "disabled", Generation: 1, Provider: &provider, RequestedBy: domain.UserID, QueuedAt: now, StartedAt: &now, FinishedAt: &now}
		if err := db.Create(&execution).Error; err != nil {
			t.Fatal(err)
		}
		cost := int64(0)
		if index == 0 {
			cost = 15000
		}
		summary := persistence.ExecutionUsageSummary{TenantID: domain.TenantID, ExecutionID: identity.ExecutionID, Generation: 1, SessionID: sessionID, TurnID: identity.TurnID, Provider: provider, ProviderCostMicros: cost, CurrencyCode: "USD", Final: true, LatestEventSequence: 1}
		if err := db.Omit("ProviderCostReported").Create(&summary).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := database.Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var positive, unknown persistence.ExecutionUsageSummary
	if err := db.Where("tenant_id = ? AND execution_id = ?", domain.TenantID, identities[0].ExecutionID).Take(&positive).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Where("tenant_id = ? AND execution_id = ?", domain.TenantID, identities[1].ExecutionID).Take(&unknown).Error; err != nil {
		t.Fatal(err)
	}
	if !positive.ProviderCostReported || unknown.ProviderCostReported {
		t.Fatalf("Provider cost migration positive/unknown = %#v / %#v", positive, unknown)
	}
	result, err := NewService(db).GetSessionUsage(ctx, identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	itemsByExecution := make(map[uuid.UUID]ExecutionUsage, len(result.Items))
	for _, item := range result.Items {
		itemsByExecution[item.ExecutionID] = item
	}
	positiveItem, positiveFound := itemsByExecution[identities[0].ExecutionID]
	unknownItem, unknownFound := itemsByExecution[identities[1].ExecutionID]
	if !positiveFound || !unknownFound || !positiveItem.ProviderCostReported || unknownItem.ProviderCostReported ||
		unknownItem.CostCoverage != "provider-unavailable" || len(unknownItem.TotalCostByCurrency) != 0 {
		t.Fatalf("Provider cost availability projection = %#v", result.Items)
	}
	if err := db.Model(&persistence.ExecutionUsageSummary{}).
		Where("tenant_id = ? AND execution_id = ?", domain.TenantID, positive.ExecutionID).
		Updates(map[string]any{"provider_cost_micros": 0, "provider_cost_reported": false}).Error; err == nil {
		t.Fatal("PostgreSQL allowed Provider cost reporting to regress")
	}
	if err := db.Model(&persistence.ExecutionUsageSummary{}).
		Where("tenant_id = ? AND execution_id = ?", domain.TenantID, unknown.ExecutionID).
		Update("provider_cost_reported", true).Error; err != nil {
		t.Fatalf("record explicit zero Provider cost: %v", err)
	}
}

func TestPostgresInternalCostCenterProjectionPrefersActualAllocation(t *testing.T) {
	databaseURL := os.Getenv("TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("TEST_POSTGRES_URL is not configured")
	}
	ctx := context.Background()
	db, err := database.Open(ctx, postgresisolation.URL(t, databaseURL))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := database.Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(
		ctx,
		db,
		platform.ProfilePersonal,
		"postgres-internal-cost-center-"+uuid.NewString(),
	)
	if err != nil {
		t.Fatal(err)
	}
	assertInternalCostCenterProjection(t, ctx, db, domain, "PostgreSQL")
}

func usageMigrationsThrough(t *testing.T, tail string) fs.FS {
	t.Helper()
	entries, err := fs.ReadDir(migrations.Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	result := fstest.MapFS{}
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() > tail {
			continue
		}
		data, err := fs.ReadFile(migrations.Files, entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		result[entry.Name()] = &fstest.MapFile{Data: data}
	}
	return result
}
