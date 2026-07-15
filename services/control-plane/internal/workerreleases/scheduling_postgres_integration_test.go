package workerreleases

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresReleaseSelectionLocksTargetAgainstPolicyTransition(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	first := openReleaseLockPostgresDB(t, databaseURL)
	second := openReleaseLockPostgresDB(t, databaseURL)
	if err := database.Migrate(ctx, first, migrations.Files); err != nil {
		t.Fatal(err)
	}
	targetID := uuid.New()
	now := time.Now().UTC()
	if err := first.Create(&persistence.ExecutionTarget{
		ID: targetID, Kind: "local", Name: "release-lock-" + targetID.String(), Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := first.Delete(&persistence.ExecutionTarget{}, "id = ?", targetID).Error; err != nil {
			t.Errorf("delete release lock target: %v", err)
		}
	})

	selectionTx := first.Begin()
	if selectionTx.Error != nil {
		t.Fatal(selectionTx.Error)
	}
	if _, err := SelectExecution(ctx, selectionTx, targetID, uuid.New()); err != nil {
		_ = selectionTx.Rollback().Error
		t.Fatal(err)
	}

	transitionStarted := make(chan struct{})
	transitionDone := make(chan error, 1)
	go func() {
		transitionTx := second.Begin()
		if transitionTx.Error != nil {
			transitionDone <- transitionTx.Error
			return
		}
		defer transitionTx.Rollback()
		var target persistence.ExecutionTarget
		close(transitionStarted)
		err := persistence.WithLocking(transitionTx.WithContext(ctx), "UPDATE", "").
			Select("id").Where("id = ?", targetID).Take(&target).Error
		transitionDone <- err
	}()
	<-transitionStarted

	select {
	case err := <-transitionDone:
		_ = selectionTx.Rollback().Error
		if err == nil {
			t.Fatal("release transition acquired the target UPDATE lock before selection committed")
		}
		t.Fatalf("release transition failed before selection commit: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if err := selectionTx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-transitionDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("release transition remained blocked after selection committed")
	}
}

func openReleaseLockPostgresDB(t *testing.T, databaseURL string) *gorm.DB {
	t.Helper()
	db, err := database.Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}
