package runtimesecretrotation

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestRuntimeSecretOnlineRekeyIsAuditedResumableAndIdempotent(t *testing.T) {
	ctx := context.Background()
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, profile, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "runtime-rekey-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}

	oldKey := bytes.Repeat([]byte{0x71}, 32)
	newKey := bytes.Repeat([]byte{0x72}, 32)
	oldCipher, err := secret.NewCursorCipher(oldKey)
	if err != nil {
		t.Fatal(err)
	}
	keyring, err := secret.NewCursorCipherWithKeyring(
		secret.CipherKey{ID: "runtime-v2", Key: newKey},
		secret.CipherKey{ID: "runtime-v1", Key: oldKey},
	)
	if err != nil {
		t.Fatal(err)
	}
	targetPlaintext := `{"host":"worker.example.internal","token":"target-secret"}`
	targetCiphertext, err := oldCipher.Encrypt(targetPlaintext)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Model(&persistence.ExecutionTarget{}).Where("id = ?", domain.ExecutionTargetID).
		UpdateColumns(map[string]any{
			"configuration_encrypted": targetCiphertext, "configuration_key_id": nil,
		}).Error; err != nil {
		t.Fatal(err)
	}

	projectID := uuid.New()
	sessionID := uuid.New()
	turnID := uuid.New()
	executionID := uuid.New()
	now := time.Now().UTC()
	provider := "codex"
	bindingVersion := 1
	bindingDigest := bytes.Repeat([]byte{0x35}, 32)
	models := []any{
		&persistence.Project{
			ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
			Name: "Runtime rekey", DefaultBranch: "main", Visibility: "private", CreatedBy: domain.UserID,
		},
		&persistence.AgentSession{
			ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
			ProjectID: projectID, CreatedBy: domain.UserID, Title: "Runtime rekey", Status: "active",
			Visibility: "private", Provider: provider, ExecutionTargetID: domain.ExecutionTargetID,
		},
		&persistence.AgentTurn{
			ID: turnID, TenantID: domain.TenantID, SessionID: sessionID, CreatedBy: domain.UserID,
			Status: "completed", InputText: "runtime rekey", RuntimeMode: "approval-required", InteractionMode: "plan",
		},
		&persistence.AgentExecution{
			ID: executionID, TenantID: domain.TenantID, SessionID: sessionID, TurnID: turnID,
			Attempt: 1, Status: "completed", ExecutionTargetID: domain.ExecutionTargetID,
			TargetKind: string(platform.TargetLocal), Provider: &provider,
			ProviderResumeStrategySnapshot: "native-cursor",
			ProviderCursorBindingVersion:   &bindingVersion, ProviderCursorBindingDigest: bindingDigest,
			Generation: 1, RequestedBy: domain.UserID, QueuedAt: now, FinishedAt: &now,
		},
	}
	for _, model := range models {
		if err := store.DB().Create(model).Error; err != nil {
			t.Fatalf("create %T: %v", model, err)
		}
	}
	var digest [32]byte
	copy(digest[:], bindingDigest)
	cursorPlaintext := []byte(`{"cursor":"provider-native-cursor"}`)
	cursorCiphertext, err := oldCipher.SealV2(cursorPlaintext, byte(bindingVersion), digest)
	if err != nil {
		t.Fatal(err)
	}
	generation := int64(1)
	historySequence := int64(0)
	if err := store.DB().Model(&persistence.AgentSession{}).Where("id = ?", sessionID).
		UpdateColumns(map[string]any{
			"provider_resume_cursor_encrypted":           cursorCiphertext,
			"provider_resume_cursor_key_id":              nil,
			"provider_resume_cursor_state":               "usable",
			"provider_resume_cursor_source_execution_id": executionID,
			"provider_resume_cursor_source_generation":   generation,
			"provider_resume_cursor_history_sequence":    historySequence,
		}).Error; err != nil {
		t.Fatal(err)
	}

	newOnly, err := secret.NewCursorCipherWithKeyring(secret.CipherKey{ID: "runtime-v2", Key: newKey})
	if err != nil {
		t.Fatal(err)
	}
	newOnlyService, err := New(store.DB(), newOnly)
	if err != nil {
		t.Fatal(err)
	}
	missingPlan, err := newOnlyService.Plan(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if missingPlan.PendingReencrypt != 2 || missingPlan.Unconfigured != 2 {
		t.Fatalf("unexpected missing-key plan: %#v", missingPlan)
	}
	if _, err := newOnlyService.Execute(ctx, ExecuteOptions{OperatorReference: "change-runtime-1"}); err == nil ||
		!strings.Contains(err.Error(), "configure every old key") {
		t.Fatalf("expected missing-key fail closed, got %v", err)
	}
	assertModelCount(t, store.DB(), &persistence.RuntimeSecretRekeyRun{}, 0)

	service, err := New(store.DB(), keyring)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.Plan(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if plan.PendingReencrypt != 2 || plan.Unconfigured != 0 || plan.AlreadyPrimary != 0 {
		t.Fatalf("unexpected runtime Secret plan: %#v", plan)
	}
	assertModelCount(t, store.DB(), &persistence.RuntimeSecretRekeyRun{}, 0)
	if err := store.DB().Model(&persistence.ExecutionTarget{}).Where("id = ?", domain.ExecutionTargetID).
		UpdateColumns(map[string]any{
			"configuration_encrypted": []byte("forged-target"), "configuration_key_id": "runtime-v2",
		}).Error; err == nil {
		t.Fatal("Execution Target runtime key changed without rekey evidence")
	}
	if err := store.DB().Model(&persistence.AgentSession{}).Where("id = ?", sessionID).
		UpdateColumns(map[string]any{
			"provider_resume_cursor_encrypted": []byte("forged-cursor"),
			"provider_resume_cursor_key_id":    "runtime-v2",
		}).Error; err == nil {
		t.Fatal("Provider Cursor runtime key changed without rekey evidence")
	}

	preparedRun := persistence.RuntimeSecretRekeyRun{
		ID: uuid.New(), PrimaryKeyID: "runtime-v2",
		OperatorReference: "change-runtime-1", StartedAt: time.Now().UTC(),
	}
	if err := store.DB().Create(&preparedRun).Error; err != nil {
		t.Fatal(err)
	}
	report, err := service.Execute(ctx, ExecuteOptions{
		OperatorReference: "change-runtime-1", BatchSize: 1, ResumeRunID: &preparedRun.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.RunID != preparedRun.ID {
		t.Fatalf("runtime Secret execution did not resume prepared run: %s != %s", report.RunID, preparedRun.ID)
	}
	if report.ExecutionTargetCount != 1 || report.ProviderResumeCursorCount != 1 || len(report.EntryDigest) != 64 {
		t.Fatalf("unexpected runtime Secret receipt: %#v", report)
	}
	var target persistence.ExecutionTarget
	if err := store.DB().First(&target, "id = ?", domain.ExecutionTargetID).Error; err != nil {
		t.Fatal(err)
	}
	if target.ConfigurationKeyID == nil || *target.ConfigurationKeyID != "runtime-v2" ||
		bytes.Equal(target.ConfigurationEncrypted, targetCiphertext) {
		t.Fatalf("Execution Target did not converge to the primary key: %#v", target)
	}
	decodedTarget, metadata, err := keyring.DecryptWithMetadata(target.ConfigurationEncrypted)
	if err != nil || decodedTarget != targetPlaintext || !metadata.Primary || !metadata.Keyed {
		t.Fatalf("rewrapped target = %q, %#v, %v", decodedTarget, metadata, err)
	}
	var session persistence.AgentSession
	if err := store.DB().First(&session, "id = ?", sessionID).Error; err != nil {
		t.Fatal(err)
	}
	if session.ProviderResumeCursorKeyID == nil || *session.ProviderResumeCursorKeyID != "runtime-v2" ||
		bytes.Equal(session.ProviderResumeCursorEncrypted, cursorCiphertext) ||
		session.ProviderResumeCursorSourceExecutionID == nil || *session.ProviderResumeCursorSourceExecutionID != executionID {
		t.Fatalf("Provider Cursor lineage changed or did not converge: %#v", session)
	}
	opened, status, metadata, err := keyring.OpenV2WithMetadata(
		session.ProviderResumeCursorEncrypted, byte(bindingVersion), digest,
	)
	if err != nil || status != secret.CursorOpenValid || !bytes.Equal(opened, cursorPlaintext) ||
		!metadata.Primary || !metadata.Keyed {
		t.Fatalf("rewrapped Cursor = %q, %s, %#v, %v", opened, status, metadata, err)
	}

	completed, err := service.Plan(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if completed.PendingReencrypt != 0 || completed.AlreadyPrimary != 2 {
		t.Fatalf("unexpected completed runtime Secret plan: %#v", completed)
	}
	assertModelCount(t, store.DB(), &persistence.RuntimeSecretRekeyEntry{}, 2)
	assertModelCount(t, store.DB(), &persistence.RuntimeSecretRekeyReceipt{}, 1)
	var auditCount int64
	if err := store.DB().Model(&persistence.AuditLog{}).
		Where("action = ?", "security.runtime_secret_reencrypted").Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if auditCount != 2 {
		t.Fatalf("expected two runtime Secret rekey audits, got %d", auditCount)
	}
	if err := store.DB().Model(&persistence.RuntimeSecretRekeyReceipt{}).Where("run_id = ?", report.RunID).
		UpdateColumn("execution_target_count", 9).Error; err == nil {
		t.Fatal("runtime Secret receipt was mutable")
	}
	if err := store.DB().Create(&persistence.RuntimeSecretRekeyEntry{
		ID: uuid.New(), RunID: report.RunID, TenantID: &domain.TenantID,
		ResourceType: ResourceExecutionTargetConfiguration, ResourceID: uuid.New(),
		DecryptKeyID: "runtime-v1", NewKeyID: "runtime-v2",
		OldCiphertextSHA256: make([]byte, 32), NewCiphertextSHA256: make([]byte, 32), CreatedAt: now,
	}).Error; err == nil {
		t.Fatal("completed runtime Secret run accepted new evidence")
	}

	second, err := service.Execute(ctx, ExecuteOptions{OperatorReference: "change-runtime-2"})
	if err != nil {
		t.Fatal(err)
	}
	if second.ExecutionTargetCount != 0 || second.ProviderResumeCursorCount != 0 {
		t.Fatalf("idempotent runtime Secret rerun mutated resources: %#v", second)
	}
}

func assertModelCount(t *testing.T, db *gorm.DB, model any, expected int64) {
	t.Helper()
	var actual int64
	if err := db.Model(model).Count(&actual).Error; err != nil {
		t.Fatal(err)
	}
	if actual != expected {
		t.Fatalf("unexpected %T count: got %d, want %d", model, actual, expected)
	}
}
