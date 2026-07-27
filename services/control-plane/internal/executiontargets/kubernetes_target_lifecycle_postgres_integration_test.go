package executiontargets

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/routing"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestDisableManagedKubernetesTargetPostgresSharesReconcilerAdvisoryLock(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	userID, tenantID, organizationID, targetID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if err := db.Transaction(func(tx *gorm.DB) error {
		models := []any{
			&persistence.User{
				ID: userID, Email: uuid.NewString() + "@example.com", DisplayName: "Target disable PostgreSQL",
				Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
			},
			&persistence.Tenant{
				ID: tenantID, Slug: "target-disable-" + uuid.NewString(), Name: "Target disable PostgreSQL",
				Status: "active", PlanCode: "test", Region: "local", Settings: map[string]any{},
				CreatedBy: userID, CreatedAt: now, UpdatedAt: now,
			},
			&persistence.TenantMembership{
				TenantID: tenantID, UserID: userID, Role: "owner", Status: "active",
				JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
			},
			&persistence.Organization{
				ID: organizationID, TenantID: tenantID, Slug: "root", Name: "Target disable PostgreSQL",
				Kind: "root", Status: "active", Settings: map[string]any{}, CreatedBy: userID,
				CreatedAt: now, UpdatedAt: now,
			},
			&persistence.OrganizationMembership{
				TenantID: tenantID, OrganizationID: organizationID, UserID: userID,
				Role: "owner", Status: "active", CreatedAt: now, UpdatedAt: now,
			},
			&persistence.ExecutionTarget{
				ID: targetID, TenantID: &tenantID, OrganizationID: &organizationID,
				Kind: "kubernetes", Name: "target-disable-postgres", Status: "active",
				ConfigurationEncrypted: []byte("encrypted"), Capabilities: map[string]any{},
				CreatedAt: now, UpdatedAt: now,
			},
		}
		for _, model := range models {
			if err := tx.Create(model).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	available := 16
	if _, err := routing.NewService(db).ObserveHealth(ctx, routing.HealthObservation{
		ExecutionTargetID: targetID, Status: routing.HealthHealthy, CapacityStatus: routing.CapacityAvailable,
		AvailableCapacityUnits: &available,
		ReservationAuthority:   &routing.ReservationAuthorityObservation{Mode: routing.ReservationAuthorityExactActiveV1},
		Source:                 managedKubernetesRoutingPublisherPrefix + "postgres-test",
		ObservedAt:             now, TTL: time.Hour,
	}); err != nil {
		t.Fatal(err)
	}

	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(db, profile, nil)
	principal := identity.Principal{UserID: userID, ActiveTenantID: &tenantID}
	release, acquired, err := persistence.TryAdvisoryLock(ctx, db, kubernetesReconcilerAdvisoryLock)
	if err != nil || !acquired {
		t.Fatalf("hold Reconciler lock = acquired=%t err=%v", acquired, err)
	}
	_, _, err = service.DisableManagedKubernetesTarget(
		ctx, principal, tenantID, targetID, "target-disable-pg-busy", "127.0.0.1",
	)
	assertExecutionTargetProblem(t, err, 409, "kubernetes_reconciler_busy")
	release()

	disabled, replayed, err := service.DisableManagedKubernetesTarget(
		ctx, principal, tenantID, targetID, "target-disable-pg-commit", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if replayed || disabled.Status != "disabled" {
		t.Fatalf("PostgreSQL disable = %#v replayed=%t", disabled, replayed)
	}
	var auditCount int64
	if err := db.Model(&persistence.AuditLog{}).
		Where("tenant_id = ? AND resource_id = ? AND action = ?", tenantID, targetID, "execution_target.kubernetes_disabled").
		Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("PostgreSQL disable audit count = %d, want 1", auditCount)
	}
}
