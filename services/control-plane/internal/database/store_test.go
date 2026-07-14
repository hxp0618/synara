package database

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestSQLiteMetadataStoreAutoMigratesAllModels(t *testing.T) {
	config, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenMetadataStore(context.Background(), config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(context.Background(), migrations.Files); err != nil {
		t.Fatal(err)
	}
	for _, model := range persistence.AllModels() {
		if !store.DB().Migrator().HasTable(model) {
			t.Fatalf("sqlite migration omitted model %T", model)
		}
	}
	if !store.DB().Migrator().HasTable(&persistence.ExecutionControlCommand{}) {
		t.Fatal("sqlite migration omitted execution_control_commands")
	}
}

func TestSQLiteMetadataStoreQuarantinesLegacyProviderCursorAndCreatesSessionActiveIndex(t *testing.T) {
	ctx := context.Background()
	config, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "sqlite-cursor-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	projectID := uuid.New()
	if err := store.DB().Create(&persistence.Project{
		ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		Name: "SQLite Cursor Upgrade", DefaultBranch: "main", Visibility: "organization", CreatedBy: domain.UserID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	legacyID := uuid.New()
	emptyID := uuid.New()
	for _, session := range []persistence.AgentSession{
		{
			ID: legacyID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
			ProjectID: projectID, CreatedBy: domain.UserID, Title: "Legacy Cursor", Status: "active",
			Visibility: "private", Provider: "codex", ExecutionTargetID: domain.ExecutionTargetID,
		},
		{
			ID: emptyID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
			ProjectID: projectID, CreatedBy: domain.UserID, Title: "Empty Cursor", Status: "active",
			Visibility: "private", Provider: "codex", ExecutionTargetID: domain.ExecutionTargetID,
		},
	} {
		if err := store.DB().Create(&session).Error; err != nil {
			t.Fatal(err)
		}
	}
	legacyCiphertext := []byte("legacy-encrypted-provider-cursor")
	if err := store.DB().Exec(`UPDATE agent_sessions
		SET provider_resume_cursor_encrypted = ?, provider_resume_cursor_state = 'absent'
		WHERE tenant_id = ? AND id = ?`, legacyCiphertext, domain.TenantID, legacyID).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Exec(`UPDATE agent_sessions
		SET provider_resume_cursor_encrypted = X'', provider_resume_cursor_state = ''
		WHERE tenant_id = ? AND id = ?`, domain.TenantID, emptyID).Error; err != nil {
		t.Fatal(err)
	}

	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var legacy persistence.AgentSession
	if err := store.DB().Where("tenant_id = ? AND id = ?", domain.TenantID, legacyID).Take(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	if legacy.ProviderResumeCursorState != "quarantined" ||
		!bytes.Equal(legacy.ProviderResumeCursorEncrypted, legacyCiphertext) ||
		legacy.ProviderResumeCursorSourceExecutionID != nil ||
		legacy.ProviderResumeCursorSourceGeneration != nil || legacy.ProviderResumeCursorHistorySequence != nil {
		t.Fatalf("legacy SQLite Cursor was not safely quarantined: %#v", legacy)
	}
	var empty persistence.AgentSession
	if err := store.DB().Where("tenant_id = ? AND id = ?", domain.TenantID, emptyID).Take(&empty).Error; err != nil {
		t.Fatal(err)
	}
	if empty.ProviderResumeCursorState != "absent" || len(empty.ProviderResumeCursorEncrypted) != 0 {
		t.Fatalf("empty SQLite Cursor was not normalized to absent: %#v", empty)
	}
	var indexCount int64
	if err := store.DB().Raw(`SELECT count(*) FROM sqlite_master
		WHERE type = 'index' AND name = 'uq_agent_executions_session_active'`).Scan(&indexCount).Error; err != nil {
		t.Fatal(err)
	}
	if indexCount != 1 {
		t.Fatal("SQLite migration omitted the one-active-Execution Session index")
	}
	if err := store.DB().Raw(`SELECT count(*) FROM sqlite_master
		WHERE type = 'index' AND name = 'uq_execution_interactions_request'`).Scan(&indexCount).Error; err != nil {
		t.Fatal(err)
	}
	if indexCount != 1 {
		t.Fatal("SQLite migration omitted the execution-wide Interaction request index")
	}
}

func TestSQLiteMetadataStoreRejectsAmbiguousLegacyActiveExecutions(t *testing.T) {
	ctx := context.Background()
	config, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Exec(`DROP INDEX uq_agent_executions_session_active`).Error; err != nil {
		t.Fatal(err)
	}

	tenantID := uuid.New()
	sessionID := uuid.New()
	for _, status := range []string{"queued", "recovering"} {
		execution := persistence.AgentExecution{
			ID: uuid.New(), TenantID: tenantID, SessionID: sessionID, TurnID: uuid.New(),
			Status: status, ExecutionTargetID: uuid.New(), Generation: 1, RequestedBy: uuid.New(),
		}
		if err := store.DB().Create(&execution).Error; err != nil {
			t.Fatal(err)
		}
	}

	if err := store.Migrate(ctx, migrations.Files); err == nil {
		t.Fatal("SQLite migration accepted multiple active Executions for one Session")
	}
	var activeCount int64
	if err := store.DB().Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND session_id = ?", tenantID, sessionID).
		Count(&activeCount).Error; err != nil {
		t.Fatal(err)
	}
	if activeCount != 2 {
		t.Fatalf("SQLite migration mutated ambiguous legacy active Executions: count = %d", activeCount)
	}
}
