package executiontargets

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresActiveTargetResolutionLocksLifecycleTransition(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	platformConfig, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := secret.NewCursorCipher(bytes.Repeat([]byte{0x71}, 32))
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(db, platformConfig, cipher)
	var scope persistence.Organization
	if err := db.Where("status = ?", "active").Order("created_at").Take(&scope).Error; err != nil {
		t.Skipf("PostgreSQL test database has no active tenant scope: %v", err)
	}
	tenantID, targetID := scope.TenantID, uuid.New()
	target := persistence.ExecutionTarget{
		ID: targetID, TenantID: &tenantID, OrganizationID: &scope.ID,
		Kind: "ssh", Name: "locked-active-target", Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{},
	}
	if err := db.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Where("id = ?", targetID).Delete(&persistence.ExecutionTarget{}).Error
	})

	lockingTx := db.Begin()
	if lockingTx.Error != nil {
		t.Fatal(lockingTx.Error)
	}
	committed := false
	t.Cleanup(func() {
		if !committed {
			_ = lockingTx.Rollback().Error
		}
	})
	if _, _, err := service.ResolveWorkerTargetInTransaction(ctx, lockingTx, targetID, "ssh"); err != nil {
		t.Fatal(err)
	}

	updateDone := make(chan error, 1)
	go func() {
		updateDone <- db.WithContext(ctx).Model(&persistence.ExecutionTarget{}).
			Where("id = ?", targetID).Update("status", "offline").Error
	}()
	select {
	case err := <-updateDone:
		t.Fatalf("Target lifecycle update bypassed active Target FOR UPDATE lock: %v", err)
	case <-time.After(250 * time.Millisecond):
	}
	if err := lockingTx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	committed = true
	select {
	case err := <-updateDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("Target lifecycle update remained blocked after claim-scope lock committed")
	}
}
