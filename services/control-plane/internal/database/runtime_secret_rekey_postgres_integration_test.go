package database

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPostgresRuntimeSecretRekeyMigrationClosesReceiptsAndEvidence(t *testing.T) {
	ctx := context.Background()
	db := openPostgresIntegrationDB(t)
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "postgres-runtime-rekey-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	project := persistence.Project{
		ID: uuid.New(), TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		Name: "Runtime rekey", DefaultBranch: "main", Visibility: "private", CreatedBy: domain.UserID,
	}
	if err := db.Create(&project).Error; err != nil {
		t.Fatal(err)
	}
	session := persistence.AgentSession{
		ID: uuid.New(), TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		ProjectID: project.ID, CreatedBy: domain.UserID, Title: "Runtime rekey", Status: "active",
		Visibility: "private", Provider: "codex", ExecutionTargetID: domain.ExecutionTargetID,
	}
	if err := db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	turn := persistence.AgentTurn{
		ID: uuid.New(), TenantID: domain.TenantID, SessionID: session.ID, CreatedBy: domain.UserID,
		Status: "queued", InputText: "runtime rekey", RuntimeMode: "approval-required", InteractionMode: "plan",
	}
	if err := db.Create(&turn).Error; err != nil {
		t.Fatal(err)
	}
	provider := "codex"
	execution := persistence.AgentExecution{
		ID: uuid.New(), TenantID: domain.TenantID, SessionID: session.ID, TurnID: turn.ID,
		Attempt: 1, Status: "queued", ExecutionTargetID: domain.ExecutionTargetID,
		TargetKind: string(platform.TargetLocal), Provider: &provider,
		Generation: 1, RequestedBy: domain.UserID, QueuedAt: now,
	}
	if err := db.Create(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.AgentSession{}).Where("id = ?", session.ID).
		UpdateColumns(map[string]any{
			"provider_resume_cursor_encrypted":           []byte("old-cursor"),
			"provider_resume_cursor_state":               "usable",
			"provider_resume_cursor_source_execution_id": execution.ID,
			"provider_resume_cursor_source_generation":   int64(1),
			"provider_resume_cursor_history_sequence":    int64(0),
		}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.ExecutionTarget{}).Where("id = ?", domain.ExecutionTargetID).
		UpdateColumns(map[string]any{
			"configuration_encrypted": []byte("old-target"), "configuration_key_id": nil,
		}).Error; err != nil {
		t.Fatal(err)
	}
	run := persistence.RuntimeSecretRekeyRun{
		ID: uuid.New(), PrimaryKeyID: "runtime-v2", OperatorReference: "change-postgres-runtime", StartedAt: now,
	}
	if err := db.Create(&run).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.ExecutionTarget{}).Where("id = ?", domain.ExecutionTargetID).
		UpdateColumns(map[string]any{
			"configuration_encrypted": []byte("forged-target"), "configuration_key_id": "runtime-v2",
		}).Error; err == nil {
		t.Fatal("PostgreSQL accepted an Execution Target key change without exact evidence")
	}
	if err := db.Model(&persistence.AgentSession{}).Where("id = ?", session.ID).
		UpdateColumns(map[string]any{
			"provider_resume_cursor_encrypted": []byte("forged-cursor"),
			"provider_resume_cursor_key_id":    "runtime-v2",
		}).Error; err == nil {
		t.Fatal("PostgreSQL accepted a Provider Cursor key change without exact evidence")
	}
	if err := db.Create(&persistence.RuntimeSecretRekeyReceipt{
		RunID: run.ID, EntryDigest: make([]byte, 32), CompletedAt: now,
	}).Error; err == nil {
		t.Fatal("PostgreSQL accepted a runtime Secret receipt before convergence")
	}
	oldTargetDigest := sha256.Sum256([]byte("old-target"))
	newTargetDigest := sha256.Sum256([]byte("new-target"))
	targetEntry := persistence.RuntimeSecretRekeyEntry{
		ID: uuid.New(), RunID: run.ID, TenantID: &domain.TenantID,
		ResourceType: "execution_target_configuration", ResourceID: domain.ExecutionTargetID,
		DecryptKeyID: "runtime-v1", NewKeyID: "runtime-v2",
		OldCiphertextSHA256: oldTargetDigest[:], NewCiphertextSHA256: newTargetDigest[:], CreatedAt: now,
	}
	oldCursorDigest := sha256.Sum256([]byte("old-cursor"))
	newCursorDigest := sha256.Sum256([]byte("new-cursor"))
	cursorEntry := persistence.RuntimeSecretRekeyEntry{
		ID: uuid.New(), RunID: run.ID, TenantID: &domain.TenantID,
		ResourceType: "provider_resume_cursor", ResourceID: session.ID,
		DecryptKeyID: "runtime-v1", NewKeyID: "runtime-v2",
		OldCiphertextSHA256: oldCursorDigest[:], NewCiphertextSHA256: newCursorDigest[:], CreatedAt: now,
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&targetEntry).Error; err != nil {
			return err
		}
		if err := tx.Model(&persistence.ExecutionTarget{}).Where("id = ?", domain.ExecutionTargetID).
			UpdateColumns(map[string]any{
				"configuration_encrypted": []byte("new-target"), "configuration_key_id": "runtime-v2",
			}).Error; err != nil {
			return err
		}
		if err := tx.Create(&cursorEntry).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.AgentSession{}).Where("id = ?", session.ID).
			UpdateColumns(map[string]any{
				"provider_resume_cursor_encrypted": []byte("new-cursor"),
				"provider_resume_cursor_key_id":    "runtime-v2",
			}).Error
	}); err != nil {
		t.Fatal(err)
	}
	receipt := persistence.RuntimeSecretRekeyReceipt{
		RunID: run.ID, ExecutionTargetCount: 1, ProviderResumeCursorCount: 1,
		EntryDigest: make([]byte, 32), CompletedAt: now,
	}
	if err := db.Create(&receipt).Error; err != nil {
		t.Fatalf("PostgreSQL rejected a converged runtime Secret receipt: %v", err)
	}
	if err := db.Create(&persistence.RuntimeSecretRekeyEntry{
		ID: uuid.New(), RunID: run.ID, TenantID: &domain.TenantID,
		ResourceType: "provider_resume_cursor", ResourceID: uuid.New(),
		DecryptKeyID: "runtime-v1", NewKeyID: "runtime-v2",
		OldCiphertextSHA256: make([]byte, 32), NewCiphertextSHA256: make([]byte, 32), CreatedAt: now,
	}).Error; err == nil {
		t.Fatal("PostgreSQL accepted runtime Secret evidence after receipt")
	}
	if err := db.Model(&persistence.RuntimeSecretRekeyReceipt{}).Where("run_id = ?", run.ID).
		UpdateColumn("execution_target_count", 7).Error; err == nil {
		t.Fatal("PostgreSQL allowed runtime Secret receipt mutation")
	}
}
