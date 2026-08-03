package entitlements

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/tenancy"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresConcurrentPlanAssignmentUsesSubscriptionVersion(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "postgres-entitlements-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	tenant, err := tenancy.NewService(db).CreateTenant(ctx, principal, tenancy.CreateTenantInput{
		Slug: "pg-entitled-" + uuid.NewString()[:8], Name: "PostgreSQL Entitled", PlanCode: "free", Status: "active",
	}, "pg-entitled-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(db)
	periodStart := time.Now().UTC()
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for _, status := range []string{"active", "suspended"} {
		wait.Add(1)
		go func(subscriptionStatus string) {
			defer wait.Done()
			<-start
			_, assignErr := service.AssignPlanByPlatform(ctx, domain.UserID, tenant.ID, AssignPlanInput{
				PlanCode: "enterprise", Status: subscriptionStatus, ExpectedVersion: 1,
				CurrentPeriodStart: periodStart, CurrentPeriodEnd: periodStart.AddDate(0, 1, 0),
				AssignmentSource: "platform_admin", Reason: "concurrent Plan assignment",
			}, "pg-plan-"+subscriptionStatus, "127.0.0.1")
			results <- assignErr
		}(status)
	}
	close(start)
	wait.Wait()
	close(results)
	successes, conflicts := 0, 0
	for assignErr := range results {
		if assignErr == nil {
			successes++
			continue
		}
		var apiError *problem.Error
		if errors.As(assignErr, &apiError) && apiError.Code == "tenant_entitlement_profile_version_conflict" {
			conflicts++
			continue
		}
		t.Fatalf("unexpected concurrent Plan assignment error: %v", assignErr)
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent Plan assignments: successes=%d conflicts=%d", successes, conflicts)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return tx.Model(&persistence.Tenant{}).Where("id = ?", tenant.ID).Update("plan_code", "free").Error
	}); err == nil {
		t.Fatal("PostgreSQL accepted Tenant Plan projection drift")
	}
}
