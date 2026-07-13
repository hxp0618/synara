package database

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/projects"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

const stage3MigrationLegacyWorkerToken = "migration-worker-token"

func TestStage3MigrationsBackfillExistingRuntimeState(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL is not configured")
	}
	db, err := Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	legacyFiles := migrationsThrough(t, "000016_sse_connection_leases.sql")
	if err := Migrate(context.Background(), db, legacyFiles); err != nil {
		t.Fatal(err)
	}
	seed := seedStage3MigrationState(t, db)
	if err := Migrate(context.Background(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}

	var session persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.sessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	if session.CurrentRuntimeBindingID == nil {
		t.Fatal("Stage 3 migration did not attach a Provider runtime binding")
	}
	var credential persistence.ProviderCredential
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.credentialID).Take(&credential).Error; err != nil {
		t.Fatal(err)
	}
	if credential.Purpose != "provider" {
		t.Fatalf("legacy Provider Credential purpose was not backfilled: %#v", credential)
	}
	var binding persistence.ProviderRuntimeBinding
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, *session.CurrentRuntimeBindingID).Take(&binding).Error; err != nil {
		t.Fatal(err)
	}
	if binding.Provider != "codex" || binding.ResumeStrategy != "native-cursor" || binding.AuthoritativeHistorySequence != 1 {
		t.Fatalf("unexpected backfilled Provider runtime binding: %#v", binding)
	}
	var execution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.Provider == nil || *execution.Provider != "codex" || execution.ProviderRuntimeBindingID == nil ||
		execution.RemoteWorkspaceID == nil {
		t.Fatalf("Stage 3 migration did not bind the existing Execution: %#v", execution)
	}
	var workspace persistence.RemoteWorkspace
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, *execution.RemoteWorkspaceID).Take(&workspace).Error; err != nil {
		t.Fatal(err)
	}
	if workspace.SessionID != seed.sessionID || workspace.WorkspaceMode != "clone" || workspace.DefaultBranch != "main" {
		t.Fatalf("unexpected backfilled remote Workspace: %#v", workspace)
	}
	var interaction persistence.ExecutionInteraction
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.interactionID).Take(&interaction).Error; err != nil {
		t.Fatal(err)
	}
	if interaction.TurnID != seed.turnID || interaction.Provider != "codex" ||
		interaction.EventVersion != executions.RuntimeEventVersionV1 ||
		interaction.ResolutionKind == nil || *interaction.ResolutionKind != "approved" ||
		interaction.DeliveryStatus != "pending" || interaction.DeliveryWorkerID == nil ||
		interaction.DeliveryGeneration == nil || interaction.DeliveryAvailableAt == nil {
		t.Fatalf("unexpected backfilled interaction delivery: %#v", interaction)
	}
}

func TestInteractionRuntimeEventVersionMigrationBackfillsExistingRows(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL is not configured")
	}
	db := openIsolatedMigrationSchema(t, databaseURL)
	if err := Migrate(context.Background(), db, migrationsThrough(t, "000016_sse_connection_leases.sql")); err != nil {
		t.Fatal(err)
	}
	seed := seedStage3MigrationState(t, db)
	if err := Migrate(context.Background(), db, migrationsThrough(t, "000027_workspace_cleanup_dispatch.sql")); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}

	var interaction persistence.ExecutionInteraction
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.interactionID).Take(&interaction).Error; err != nil {
		t.Fatal(err)
	}
	if interaction.EventVersion != executions.RuntimeEventVersionV1 {
		t.Fatalf("legacy interaction Event version = %d, want 1", interaction.EventVersion)
	}
	if err := db.Model(&persistence.ExecutionInteraction{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, seed.interactionID).
		Update("event_version", 3).Error; err == nil {
		t.Fatal("000028 accepted an unsupported interaction Runtime Event version")
	}
}

func TestWorkspaceCleanupMigrationPreservesTargetHistoryAndPhysicalFences(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_WORKSPACE_CLEANUP_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_WORKSPACE_CLEANUP_MIGRATION_DATABASE_URL is not configured")
	}
	db := openIsolatedMigrationSchema(t, databaseURL)
	if err := Migrate(context.Background(), db, migrationsThrough(t, "000016_sse_connection_leases.sql")); err != nil {
		t.Fatal(err)
	}
	seed := seedStage3MigrationState(t, db)
	if err := Migrate(context.Background(), db, migrationsThrough(t, "000026_checkpoint_retention.sql")); err != nil {
		t.Fatal(err)
	}

	var session persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.sessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	var workspace persistence.RemoteWorkspace
	if err := db.Where("tenant_id = ? AND session_id = ?", seed.tenantID, seed.sessionID).Take(&workspace).Error; err != nil {
		t.Fatal(err)
	}
	legacyTargetID := workspace.ExecutionTargetID
	newTargetID := uuid.New()
	unmaterializedTargetID := uuid.New()
	newWorkerID := uuid.New()
	newTurnID := uuid.New()
	newExecutionID := uuid.New()
	legacyGenerationZeroTurnID := uuid.New()
	legacyGenerationZeroExecutionID := uuid.New()
	unmaterializedTurnID := uuid.New()
	unmaterializedExecutionID := uuid.New()
	now := time.Now().UTC().Truncate(time.Microsecond)
	newTarget := persistence.ExecutionTarget{
		ID: newTargetID, TenantID: &seed.tenantID, OrganizationID: &session.OrganizationID,
		Kind: "docker", Name: "Migration replacement target", Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{},
	}
	unmaterializedTarget := persistence.ExecutionTarget{
		ID: unmaterializedTargetID, TenantID: &seed.tenantID, OrganizationID: &session.OrganizationID,
		Kind: "kubernetes", Name: "Migration unmaterialized target", Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{},
	}
	for _, target := range []*persistence.ExecutionTarget{&newTarget, &unmaterializedTarget} {
		if err := db.Create(target).Error; err != nil {
			t.Fatal(err)
		}
	}
	newWorker := persistence.WorkerInstance{
		ID: newWorkerID, ExecutionTargetID: newTargetID, TargetKind: "docker",
		ClusterID: "migration", Namespace: "default", PodName: "migration-docker-worker",
		Version: "legacy", ProtocolVersion: 1, Capabilities: map[string]any{},
		LeaseSupported: true, FencingSupported: true, AuthTokenHash: []byte("migration-docker-worker-token"),
		Status: "online", RegisteredAt: now, LastHeartbeatAt: now,
	}
	if err := db.Select(
		"id", "execution_target_id", "target_kind", "cluster_id", "namespace", "pod_name",
		"version", "protocol_version", "capabilities", "lease_supported", "fencing_supported",
		"auth_token_hash", "status", "registered_at", "last_heartbeat_at",
	).Create(&newWorker).Error; err != nil {
		t.Fatal(err)
	}
	createCancelledGenerationZero := func(
		tx *gorm.DB,
		targetID, turnID, executionID uuid.UUID,
		targetKind, inputText string,
		queuedAt time.Time,
	) error {
		turn := persistence.AgentTurn{
			ID: turnID, TenantID: seed.tenantID, SessionID: seed.sessionID,
			CreatedBy: session.CreatedBy, Status: "cancelled", InputText: inputText,
			RuntimeMode: "approval-required", InteractionMode: "default",
			CompletedAt: &queuedAt, CreatedAt: queuedAt,
		}
		if err := tx.Create(&turn).Error; err != nil {
			return err
		}
		execution := persistence.AgentExecution{
			ID: executionID, TenantID: seed.tenantID, SessionID: seed.sessionID, TurnID: turnID,
			Attempt: 1, Status: "cancelled", ExecutionTargetID: targetID, TargetKind: targetKind,
			RemoteWorkspaceID: &workspace.ID, Generation: 0, RequestedBy: session.CreatedBy,
			QueuedAt: queuedAt, FinishedAt: &queuedAt,
		}
		return tx.Select(
			"id", "tenant_id", "session_id", "turn_id", "attempt", "status",
			"execution_target_id", "target_kind", "remote_workspace_id", "generation",
			"requested_by", "queued_at", "finished_at",
		).Create(&execution).Error
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return createCancelledGenerationZero(
			tx, legacyTargetID, legacyGenerationZeroTurnID, legacyGenerationZeroExecutionID,
			"kubernetes", "Cancelled before legacy claim", now.Add(2*time.Second),
		)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&persistence.AgentSession{}).
			Where("tenant_id = ? AND id = ?", seed.tenantID, seed.sessionID).
			Update("execution_target_id", unmaterializedTargetID).Error; err != nil {
			return err
		}
		if err := tx.Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ?", seed.tenantID, workspace.ID).
			Updates(map[string]any{"execution_target_id": unmaterializedTargetID, "updated_at": now.Add(3 * time.Second)}).Error; err != nil {
			return err
		}
		return createCancelledGenerationZero(
			tx, unmaterializedTargetID, unmaterializedTurnID, unmaterializedExecutionID,
			"kubernetes", "Cancelled before first claim", now.Add(3*time.Second),
		)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&persistence.AgentSession{}).
			Where("tenant_id = ? AND id = ?", seed.tenantID, seed.sessionID).
			Update("execution_target_id", newTargetID).Error; err != nil {
			return err
		}
		if err := tx.Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ?", seed.tenantID, workspace.ID).
			Updates(map[string]any{
				"execution_target_id": newTargetID, "state": "cleaned",
				"cleaned_at": now, "updated_at": now,
			}).Error; err != nil {
			return err
		}
		turn := persistence.AgentTurn{
			ID: newTurnID, TenantID: seed.tenantID, SessionID: seed.sessionID,
			CreatedBy: session.CreatedBy, Status: "completed", InputText: "Target move",
			RuntimeMode: "approval-required", InteractionMode: "default",
			StartedAt: &now, CompletedAt: &now, CreatedAt: now,
		}
		if err := tx.Create(&turn).Error; err != nil {
			return err
		}
		execution := persistence.AgentExecution{
			ID: newExecutionID, TenantID: seed.tenantID, SessionID: seed.sessionID, TurnID: newTurnID,
			Attempt: 1, Status: "completed", ExecutionTargetID: newTargetID, TargetKind: "docker",
			WorkerID: &newWorkerID, RemoteWorkspaceID: &workspace.ID, Generation: 1, RequestedBy: session.CreatedBy,
			QueuedAt: now, StartedAt: &now, FinishedAt: &now,
		}
		if err := tx.Select(
			"id", "tenant_id", "session_id", "turn_id", "attempt", "status",
			"execution_target_id", "target_kind", "worker_id", "remote_workspace_id", "generation",
			"requested_by", "queued_at", "started_at", "finished_at",
		).Create(&execution).Error; err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := Migrate(context.Background(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}

	var materializations []persistence.WorkspaceMaterialization
	if err := db.Where("tenant_id = ? AND workspace_id = ?", seed.tenantID, workspace.ID).
		Order("execution_target_id").Find(&materializations).Error; err != nil {
		t.Fatal(err)
	}
	if len(materializations) != 3 {
		t.Fatalf("materialization count = %d, want 3: %#v", len(materializations), materializations)
	}
	byTarget := make(map[uuid.UUID]persistence.WorkspaceMaterialization, len(materializations))
	for _, materialization := range materializations {
		byTarget[materialization.ExecutionTargetID] = materialization
		if materialization.LayoutVersion != 2 {
			t.Fatalf("migrated materialization layoutVersion = %d, want 2", materialization.LayoutVersion)
		}
	}
	legacy := byTarget[legacyTargetID]
	if legacy.State != "retired" || legacy.CleanupReason == nil || *legacy.CleanupReason != "migration-target-history" || legacy.CleanupRequestedAt == nil {
		t.Fatalf("legacy target materialization did not preserve cleanup intent: %#v", legacy)
	}
	if legacy.LastExecutionID == nil || *legacy.LastExecutionID != seed.executionID ||
		legacy.LastGeneration == nil || *legacy.LastGeneration != 1 {
		t.Fatalf("generation-zero Execution shadowed the last physical legacy placement: %#v", legacy)
	}
	if legacy.WorkerID != nil || legacy.WorkerIncarnation != nil || legacy.WorkerInstanceUID != nil {
		t.Fatalf("migrated Kubernetes materialization trusted a synthetic Pod identity: %#v", legacy)
	}
	current := byTarget[newTargetID]
	if current.State != "cleaned" || current.CleanedAt == nil || current.LastExecutionID == nil || *current.LastExecutionID != newExecutionID {
		t.Fatalf("replacement target materialization is not current: %#v", current)
	}
	if current.WorkerID == nil || *current.WorkerID != newWorkerID || current.WorkerIncarnation == nil || current.WorkerInstanceUID == nil {
		t.Fatalf("target-scoped migrated materialization lost its Worker fence: %#v", current)
	}
	var migratedDockerWorker persistence.WorkerInstance
	if err := db.Where("id = ?", newWorkerID).Take(&migratedDockerWorker).Error; err != nil {
		t.Fatal(err)
	}
	if migratedDockerWorker.TargetKind != "docker" || migratedDockerWorker.ProtocolVersion != 1 ||
		migratedDockerWorker.Status != "terminated" || migratedDockerWorker.TerminatedAt == nil {
		t.Fatalf("000027 did not terminate the legacy non-Kubernetes Worker: %#v", migratedDockerWorker)
	}
	var protocolVersionDefault string
	if err := db.Raw(`
		SELECT column_default
		FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'worker_instances'
		  AND column_name = 'protocol_version'
	`).Scan(&protocolVersionDefault).Error; err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(protocolVersionDefault) != "2" {
		t.Fatalf("worker_instances.protocol_version default = %q, want 2", protocolVersionDefault)
	}
	unmaterialized := byTarget[unmaterializedTargetID]
	if unmaterialized.State != "retired" || unmaterialized.LastExecutionID != nil || unmaterialized.LastGeneration != nil ||
		unmaterialized.WorkerID != nil || unmaterialized.WorkerIncarnation != nil || unmaterialized.WorkerInstanceUID != nil {
		t.Fatalf("generation-zero-only Kubernetes placement was treated as physically materialized: %#v", unmaterialized)
	}
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, workspace.ID).Take(&workspace).Error; err != nil {
		t.Fatal(err)
	}
	if workspace.CurrentMaterializationID == nil || *workspace.CurrentMaterializationID != current.ID {
		t.Fatalf("logical Workspace current materialization = %v, want %s", workspace.CurrentMaterializationID, current.ID)
	}
	for executionID, wantMaterializationID := range map[uuid.UUID]uuid.UUID{
		seed.executionID: legacy.ID, newExecutionID: current.ID,
		legacyGenerationZeroExecutionID: legacy.ID, unmaterializedExecutionID: unmaterialized.ID,
	} {
		var execution persistence.AgentExecution
		if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, executionID).Take(&execution).Error; err != nil {
			t.Fatal(err)
		}
		if execution.WorkspaceMaterializationID == nil || *execution.WorkspaceMaterializationID != wantMaterializationID {
			t.Fatalf("execution %s materialization = %v, want %s", executionID, execution.WorkspaceMaterializationID, wantMaterializationID)
		}
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id = ?", seed.tenantID, newExecutionID).
			Update("status", "interrupted").Error; err != nil {
			return err
		}
		return tx.Exec("SET CONSTRAINTS ALL IMMEDIATE").Error
	}); err != nil {
		t.Fatalf("interrupted historical Execution could not retain its cleaned materialization: %v", err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id = ?", seed.tenantID, newExecutionID).
			Updates(map[string]any{"status": "queued", "finished_at": nil}).Error; err != nil {
			return err
		}
		return tx.Exec("SET CONSTRAINTS ALL IMMEDIATE").Error
	}); err == nil {
		t.Fatal("PostgreSQL allowed a non-terminal Execution to retain a cleaned materialization")
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&persistence.WorkspaceMaterialization{}).
			Where("tenant_id = ? AND id = ?", seed.tenantID, legacy.ID).
			Updates(map[string]any{"state": "cleaned", "cleaned_at": now}).Error; err != nil {
			return err
		}
		return tx.Exec("SET CONSTRAINTS ALL IMMEDIATE").Error
	}); err == nil {
		t.Fatal("PostgreSQL allowed a materialization with a non-terminal Execution to become cleaned")
	}
	var worker persistence.WorkerInstance
	if err := db.Where("execution_target_id = ?", newTargetID).Take(&worker).Error; err != nil {
		t.Fatal(err)
	}
	if worker.Incarnation != 1 {
		t.Fatalf("worker incarnation = %d, want 1", worker.Incarnation)
	}
	if _, err := uuid.Parse(worker.InstanceUID); err != nil {
		t.Fatalf("worker instance UID was not deterministically backfilled: %q", worker.InstanceUID)
	}
	replacementWorkerUID := uuid.NewString()
	if err := db.Model(&persistence.WorkerInstance{}).Where("id = ?", worker.ID).
		Updates(map[string]any{"incarnation": worker.Incarnation + 1, "instance_uid": replacementWorkerUID}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&persistence.WorkspaceMaterialization{}).
			Where("tenant_id = ? AND id = ?", seed.tenantID, current.ID).
			Updates(map[string]any{"state": "cleaned", "updated_at": now.Add(time.Second)}).Error; err != nil {
			return err
		}
		return tx.Exec("SET CONSTRAINTS ALL IMMEDIATE").Error
	}); err != nil {
		t.Fatalf("historical materialization could not be updated after Worker re-registration: %v", err)
	}
	var persistedCurrent persistence.WorkspaceMaterialization
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, current.ID).Take(&persistedCurrent).Error; err != nil {
		t.Fatal(err)
	}
	if persistedCurrent.WorkerIncarnation == nil || *persistedCurrent.WorkerIncarnation != worker.Incarnation ||
		persistedCurrent.WorkerInstanceUID == nil || *persistedCurrent.WorkerInstanceUID != worker.InstanceUID {
		t.Fatalf("historical materialization Worker fence was rewritten after re-registration: %#v", persistedCurrent)
	}
	if err := db.Model(&persistence.AgentExecution{}).Where("tenant_id = ? AND id = ?", seed.tenantID, newExecutionID).
		Update("generation", 2).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&persistence.WorkspaceMaterialization{}).
			Where("tenant_id = ? AND id = ?", seed.tenantID, current.ID).
			Updates(map[string]any{"state": "cleaned", "updated_at": now.Add(2 * time.Second)}).Error; err != nil {
			return err
		}
		return tx.Exec("SET CONSTRAINTS ALL IMMEDIATE").Error
	}); err != nil {
		t.Fatalf("historical materialization could not be updated after its Execution generation advanced: %v", err)
	}
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, current.ID).Take(&persistedCurrent).Error; err != nil {
		t.Fatal(err)
	}
	if persistedCurrent.LastGeneration == nil || *persistedCurrent.LastGeneration != 1 {
		t.Fatalf("historical materialization generation snapshot was rewritten: %#v", persistedCurrent)
	}

	var pathColumns int64
	if err := db.Raw(`
		SELECT count(*)
		FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name IN ('workspace_materializations', 'workspace_cleanup_commands')
		  AND column_name IN ('path', 'absolute_path', 'relative_path', 'workspace_path')
	`).Scan(&pathColumns).Error; err != nil {
		t.Fatal(err)
	}
	if pathColumns != 0 {
		t.Fatalf("cleanup schema persisted %d forbidden host path columns", pathColumns)
	}
	for name, expected := range map[string]struct {
		columns   string
		predicate string
	}{
		"idx_agent_executions_workspace_materialization_status": {
			columns: "tenant_id,workspace_materialization_id,status,id", predicate: "workspace_materialization_id IS NOT NULL",
		},
		"idx_workspace_cleanup_commands_materialization": {
			columns: "tenant_id,materialization_id,id",
		},
		"idx_workspace_cleanup_commands_execution_target": {
			columns: "execution_target_id,id",
		},
		"idx_workspace_cleanup_commands_delivery_worker": {
			columns: "delivery_worker_id,id", predicate: "delivery_worker_id IS NOT NULL",
		},
		"idx_workspace_materializations_project": {
			columns: "tenant_id,organization_id,project_id,id",
		},
		"idx_workspace_materializations_session": {
			columns: "tenant_id,project_id,session_id,id",
		},
		"idx_workspace_materializations_worker": {
			columns: "worker_id,id", predicate: "worker_id IS NOT NULL",
		},
		"idx_workspace_materializations_last_execution": {
			columns: "tenant_id,last_execution_id,id", predicate: "last_execution_id IS NOT NULL",
		},
	} {
		assertMigrationIndex(t, db, name, expected.columns, expected.predicate)
	}

	invalidCommand := persistence.WorkspaceCleanupCommand{
		ID: uuid.New(), TenantID: seed.tenantID, MaterializationID: current.ID,
		MaterializationIncarnationID: current.IncarnationID, WorkspaceID: current.WorkspaceID,
		ExecutionTargetID: legacyTargetID, TargetKind: current.TargetKind,
		StorageScope: current.StorageScope, LayoutVersion: current.LayoutVersion,
		Reason: "invalid-scope-test", Status: "pending", DeliveryAvailableAt: now,
		RequestedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&invalidCommand).Error; err != nil {
			return err
		}
		return tx.Exec("SET CONSTRAINTS ALL IMMEDIATE").Error
	}); err == nil {
		t.Fatal("PostgreSQL accepted a Workspace cleanup command with a mismatched Target snapshot")
	}
}

func TestWorkspaceCleanupMigrationFencesLegacyKubernetesWorkerUntilPodReregisters(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_WORKSPACE_CLEANUP_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_WORKSPACE_CLEANUP_MIGRATION_DATABASE_URL is not configured")
	}
	ctx := context.Background()
	db := openIsolatedMigrationSchema(t, databaseURL)
	if err := Migrate(ctx, db, migrationsThrough(t, "000016_sse_connection_leases.sql")); err != nil {
		t.Fatal(err)
	}
	seed := seedStage3MigrationState(t, db)
	if err := Migrate(ctx, db, migrationsThrough(t, "000026_checkpoint_retention.sql")); err != nil {
		t.Fatal(err)
	}
	if err := Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}

	service := newWorkspaceCleanupMigrationExecutionService(t, db)
	var workspace persistence.RemoteWorkspace
	if err := db.Where("tenant_id = ? AND session_id = ?", seed.tenantID, seed.sessionID).Take(&workspace).Error; err != nil {
		t.Fatal(err)
	}
	if workspace.CurrentMaterializationID == nil {
		t.Fatal("000027 did not attach the migrated Kubernetes materialization")
	}
	materializationID := *workspace.CurrentMaterializationID
	var materialization persistence.WorkspaceMaterialization
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, materializationID).Take(&materialization).Error; err != nil {
		t.Fatal(err)
	}
	if materialization.WorkerID != nil || materialization.WorkerIncarnation != nil || materialization.WorkerInstanceUID != nil {
		t.Fatalf("migrated Kubernetes materialization trusted a synthetic Pod identity: %#v", materialization)
	}

	var legacyWorker persistence.WorkerInstance
	if err := db.Where(
		"execution_target_id = ? AND target_kind = ? AND cluster_id = ? AND namespace = ? AND pod_name = ?",
		workspace.ExecutionTargetID, "kubernetes", "migration", "default", "migration-worker",
	).Take(&legacyWorker).Error; err != nil {
		t.Fatal(err)
	}
	if legacyWorker.Status != "terminated" || legacyWorker.TerminatedAt == nil {
		t.Fatalf("legacy Kubernetes Worker was not retired by 000027: %#v", legacyWorker)
	}
	if _, err := service.Authenticate(ctx, stage3MigrationLegacyWorkerToken); err == nil {
		t.Fatal("legacy Kubernetes Worker token authenticated after 000027")
	} else {
		assertMigrationProblemCode(t, err, "invalid_worker_token")
	}
	claimInput := executions.ClaimExecutionInput{
		ExecutionTargetID: workspace.ExecutionTargetID,
		TargetKind:        "kubernetes",
		ExecutionID:       &seed.executionID,
	}
	if _, err := service.Claim(ctx, legacyWorker, claimInput, "migration-legacy-worker-claim"); err == nil {
		t.Fatal("legacy Kubernetes Worker claimed work after 000027")
	} else {
		assertMigrationProblemCode(t, err, "worker_incarnation_fenced")
	}

	var legacyLease persistence.WorkerLease
	if err := db.Where("tenant_id = ? AND execution_id = ?", seed.tenantID, seed.executionID).Take(&legacyLease).Error; err != nil {
		t.Fatalf("000027 removed the legacy Worker lease instead of preserving expiry recovery: %v", err)
	}
	if legacyLease.WorkerID != legacyWorker.ID || legacyLease.Generation != 1 {
		t.Fatalf("legacy Worker lease changed during migration: %#v", legacyLease)
	}
	expiredAt := time.Now().UTC().Add(-time.Minute)
	acquiredAt := expiredAt.Add(-time.Minute)
	heartbeatAt := acquiredAt.Add(30 * time.Second)
	if err := db.Model(&persistence.WorkerLease{}).
		Where("tenant_id = ? AND execution_id = ?", seed.tenantID, seed.executionID).
		Updates(map[string]any{
			"acquired_at": acquiredAt, "heartbeat_at": heartbeatAt, "expires_at": expiredAt,
		}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, seed.sessionID).
		Updates(map[string]any{
			"last_event_sequence": 1, "provider_resume_cursor_encrypted": nil,
		}).Error; err != nil {
		t.Fatal(err)
	}
	if err := service.RecoverExpired(ctx, 10); err != nil {
		t.Fatalf("recover legacy Kubernetes Worker lease: %v", err)
	}
	var remainingLeases int64
	if err := db.Model(&persistence.WorkerLease{}).
		Where("tenant_id = ? AND execution_id = ?", seed.tenantID, seed.executionID).
		Count(&remainingLeases).Error; err != nil {
		t.Fatal(err)
	}
	if remainingLeases != 0 {
		t.Fatalf("expired legacy Worker leases remaining = %d, want 0", remainingLeases)
	}
	var recovering persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).Take(&recovering).Error; err != nil {
		t.Fatal(err)
	}
	if recovering.Status != "recovering" || recovering.WorkerID != nil || recovering.Generation != 1 {
		t.Fatalf("legacy Kubernetes Execution did not enter recovery: %#v", recovering)
	}

	actualPodUID := uuid.NewString()
	registered, err := service.Register(ctx, executions.RegisterWorkerInput{
		ExecutionTargetID: workspace.ExecutionTargetID,
		TargetKind:        "kubernetes",
		InstanceUID:       actualPodUID,
		ClusterID:         legacyWorker.ClusterID,
		Namespace:         legacyWorker.Namespace,
		PodName:           legacyWorker.PodName,
		Version:           "migration-current",
		ProtocolVersion:   executions.WorkerProtocolVersion,
		Capabilities:      map[string]any{},
		LeaseSupported:    true,
		FencingSupported:  true,
	})
	if err != nil {
		t.Fatalf("re-register Kubernetes Worker with the actual Pod UID: %v", err)
	}
	if registered.Worker.ID == legacyWorker.ID || registered.Worker.InstanceUID != actualPodUID || registered.Worker.Status != "online" {
		t.Fatalf("actual Pod UID registration did not create a fresh active Worker: %#v", registered.Worker)
	}
	authenticated, err := service.Authenticate(ctx, registered.Token)
	if err != nil {
		t.Fatalf("authenticate re-registered Kubernetes Worker: %v", err)
	}
	if authenticated.ID != registered.Worker.ID || authenticated.InstanceUID != actualPodUID {
		t.Fatalf("authenticated Worker does not match the actual Pod registration: %#v", authenticated)
	}
	claimed, err := service.Claim(ctx, authenticated, claimInput, "migration-actual-pod-claim")
	if err != nil {
		t.Fatalf("claim recovered Execution with actual Pod UID: %v", err)
	}
	if claimed.Value.Execution == nil || claimed.Value.Lease == nil || claimed.Value.Execution.ID != seed.executionID ||
		claimed.Value.Lease.WorkerID != authenticated.ID || claimed.Value.Execution.Generation != 2 {
		t.Fatalf("unexpected recovered Execution claim: %#v", claimed.Value)
	}

	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, workspace.ID).Take(&workspace).Error; err != nil {
		t.Fatal(err)
	}
	if workspace.CurrentMaterializationID == nil {
		t.Fatal("recovered Kubernetes Execution did not attach a current materialization")
	}
	materialization = persistence.WorkspaceMaterialization{}
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, *workspace.CurrentMaterializationID).Take(&materialization).Error; err != nil {
		t.Fatal(err)
	}
	if materialization.WorkerID == nil || *materialization.WorkerID != authenticated.ID ||
		materialization.WorkerIncarnation == nil || *materialization.WorkerIncarnation != authenticated.Incarnation ||
		materialization.WorkerInstanceUID == nil || *materialization.WorkerInstanceUID != actualPodUID ||
		materialization.LastExecutionID == nil || *materialization.LastExecutionID != seed.executionID ||
		materialization.LastGeneration == nil || *materialization.LastGeneration != claimed.Value.Execution.Generation {
		t.Fatalf("Kubernetes materialization did not bind the actual Pod UID fence: %#v", materialization)
	}
}

func TestWorkspaceCleanupMigrationRejectsInvalidCleanedWorkspaceBackfill(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_WORKSPACE_CLEANUP_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_WORKSPACE_CLEANUP_MIGRATION_DATABASE_URL is not configured")
	}
	db := openIsolatedMigrationSchema(t, databaseURL)
	if err := Migrate(context.Background(), db, migrationsThrough(t, "000016_sse_connection_leases.sql")); err != nil {
		t.Fatal(err)
	}
	seed := seedStage3MigrationState(t, db)
	if err := Migrate(context.Background(), db, migrationsThrough(t, "000026_checkpoint_retention.sql")); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := db.Model(&persistence.RemoteWorkspace{}).
		Where("tenant_id = ? AND session_id = ?", seed.tenantID, seed.sessionID).
		Updates(map[string]any{"state": "cleaned", "cleaned_at": now, "updated_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), db, migrations.Files); err == nil {
		t.Fatal("000027 accepted a cleaned Workspace backfill with a non-terminal Execution")
	}
	var applied int64
	if err := db.Table("control_plane_schema_migrations").Where("version = ?", 27).Count(&applied).Error; err != nil {
		t.Fatal(err)
	}
	if applied != 0 {
		t.Fatal("000027 recorded an invalid cleaned Workspace migration as applied")
	}
}

func TestGitCredentialMigrationEnforcesBindingPurposeScopeAndAvailability(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL is not configured")
	}
	db, err := Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	userID, tenantID := uuid.New(), uuid.New()
	organizationID, otherOrganizationID := uuid.New(), uuid.New()
	targetID := uuid.New()
	models := []any{
		&persistence.User{ID: userID, Email: uuid.NewString() + "@example.com", DisplayName: "Git migration", Status: "active", EmailVerifiedAt: &now},
		&persistence.Tenant{ID: tenantID, Slug: "git-migration-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10], Name: "Git migration", Status: "active", PlanCode: "free", Region: "default", Settings: map[string]any{}, CreatedBy: userID},
		&persistence.TenantMembership{TenantID: tenantID, UserID: userID, Role: "owner", Status: "active", JoinedAt: &now},
		&persistence.Organization{ID: organizationID, TenantID: tenantID, Slug: "root", Name: "Root", Kind: "root", Status: "active", Settings: map[string]any{}, CreatedBy: userID},
		&persistence.Organization{ID: otherOrganizationID, TenantID: tenantID, Slug: "other", Name: "Other", Kind: "team", Status: "active", Settings: map[string]any{}, CreatedBy: userID},
		&persistence.ExecutionTarget{ID: targetID, TenantID: &tenantID, OrganizationID: &organizationID, Kind: "local", Name: "Git migration target", Status: "active", ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}},
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		for _, model := range models {
			if err := tx.Create(model).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed Git Credential migration state: %v", err)
	}
	providerCredentialID := uuid.New()
	gitCredentialID := uuid.New()
	expiredGitCredentialID := uuid.New()
	revokedGitCredentialID := uuid.New()
	otherOrganizationGitCredentialID := uuid.New()
	expiredAt := now.Add(-time.Minute)
	revokedAt := now
	credentials := []persistence.ProviderCredential{
		migrationCredential(providerCredentialID, tenantID, &organizationID, userID, "provider", "codex", "api_key", now, nil),
		migrationCredential(gitCredentialID, tenantID, &organizationID, userID, "git", "git", "https_token", now, nil),
		migrationCredential(expiredGitCredentialID, tenantID, &organizationID, userID, "git", "git", "https_token", now.Add(-time.Hour), &expiredAt),
		migrationCredential(revokedGitCredentialID, tenantID, &organizationID, userID, "git", "git", "https_token", now, nil),
		migrationCredential(otherOrganizationGitCredentialID, tenantID, &otherOrganizationID, userID, "git", "git", "https_token", now, nil),
	}
	credentials[3].RevokedAt = &revokedAt
	credentials[3].RevokedBy = &userID
	for index := range credentials {
		if err := db.Create(&credentials[index]).Error; err != nil {
			t.Fatal(err)
		}
	}
	repositoryURL := "https://git.example.com/team/private.git"
	wrongPurposeProject := persistence.Project{
		ID: uuid.New(), TenantID: tenantID, OrganizationID: organizationID, Name: "Wrong purpose",
		RepositoryURL: &repositoryURL, DefaultBranch: "main", GitCredentialID: &providerCredentialID,
		Visibility: "organization", CreatedBy: userID,
	}
	if err := db.Create(&wrongPurposeProject).Error; err == nil {
		t.Fatal("PostgreSQL accepted a Provider Credential as a Project Git Credential")
	}
	projectID := uuid.New()
	project := persistence.Project{
		ID: projectID, TenantID: tenantID, OrganizationID: organizationID, Name: "Valid Git binding",
		RepositoryURL: &repositoryURL, DefaultBranch: "main", GitCredentialID: &gitCredentialID,
		Visibility: "organization", CreatedBy: userID,
	}
	if err := db.Create(&project).Error; err != nil {
		t.Fatal(err)
	}
	invalidSession := persistence.AgentSession{
		ID: uuid.New(), TenantID: tenantID, OrganizationID: organizationID, ProjectID: projectID,
		CreatedBy: userID, Title: "Wrong purpose", Status: "active", Visibility: "private",
		Provider: "codex", ProviderCredentialID: &gitCredentialID, ExecutionTargetID: targetID,
	}
	if err := db.Create(&invalidSession).Error; err == nil {
		t.Fatal("PostgreSQL accepted a Git Credential as an Agent Session Provider Credential")
	}
	validSession := invalidSession
	validSession.ID = uuid.New()
	validSession.Title = "Valid provider binding"
	validSession.ProviderCredentialID = &providerCredentialID
	if err := db.Create(&validSession).Error; err != nil {
		t.Fatal(err)
	}
	for name, credentialID := range map[string]uuid.UUID{
		"expired":            expiredGitCredentialID,
		"revoked":            revokedGitCredentialID,
		"cross-organization": otherOrganizationGitCredentialID,
	} {
		t.Run(name, func(t *testing.T) {
			if err := db.Model(&persistence.Project{}).Where("tenant_id = ? AND id = ?", tenantID, projectID).
				Update("git_credential_id", credentialID).Error; err == nil {
				t.Fatalf("PostgreSQL accepted %s Git Credential binding", name)
			}
		})
	}
	if err := db.Model(&persistence.ProviderCredential{}).
		Where("tenant_id = ? AND id = ?", tenantID, gitCredentialID).
		Update("organization_id", otherOrganizationID).Error; err == nil {
		t.Fatal("PostgreSQL allowed Git Credential identity metadata to mutate")
	}
}

func TestCheckpointMigrationUpgradesExistingGitReferenceAndReadyPointers(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_CHECKPOINT_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_CHECKPOINT_MIGRATION_DATABASE_URL is not configured")
	}
	db, err := Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), db, migrationsThrough(t, "000016_sse_connection_leases.sql")); err != nil {
		t.Fatal(err)
	}
	seed := seedStage3MigrationState(t, db)
	if err := Migrate(context.Background(), db, migrationsThrough(t, "000023_git_credentials.sql")); err != nil {
		t.Fatal(err)
	}
	var execution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.RemoteWorkspaceID == nil {
		t.Fatal("000020 did not bind the legacy Execution to a logical Workspace")
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	baseCommit, headCommit := strings.Repeat("a", 40), strings.Repeat("b", 40)
	branch := "synara/session-migration"
	checkpoint := persistence.WorkspaceCheckpoint{
		ID: uuid.New(), TenantID: seed.tenantID, WorkspaceID: *execution.RemoteWorkspaceID,
		SessionID: seed.sessionID, TurnID: &seed.turnID, ExecutionID: seed.executionID,
		Generation: execution.Generation, IdempotencyKey: "migration-git-reference",
		Strategy: "git-reference", Status: "pending", BaseCommit: &baseCommit,
		HeadCommit: &headCommit, CurrentBranch: &branch,
		Manifest:  map[string]any{"format": "synara-git-reference-v1", "headCommit": headCommit},
		CreatedAt: now,
	}
	if err := db.Create(&checkpoint).Error; err != nil {
		t.Fatal(err)
	}
	var session persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.sessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	legacySnapshotID := uuid.New()
	legacySnapshotBytes := int64(18)
	legacySnapshotSHA := strings.Repeat("c", 64)
	legacySnapshotContentType := "application/x-tar"
	legacySnapshotReadyAt := now.Add(time.Second)
	legacySnapshot := persistence.Artifact{
		ID: legacySnapshotID, TenantID: seed.tenantID, OrganizationID: session.OrganizationID,
		ProjectID: session.ProjectID, SessionID: seed.sessionID, ExecutionID: &seed.executionID,
		Kind: "workspace_snapshot", Status: "ready", Bucket: "migration-artifacts",
		ObjectKey: "checkpoint-migration/" + legacySnapshotID.String(), ContentType: &legacySnapshotContentType,
		SizeBytes: &legacySnapshotBytes, SHA256: &legacySnapshotSHA, CreatedByType: "system",
		CreatedByID: seed.tenantID, ReadyAt: &legacySnapshotReadyAt, CreatedAt: now,
	}
	legacySnapshotCheckpoint := persistence.WorkspaceCheckpoint{
		ID: uuid.New(), TenantID: seed.tenantID, WorkspaceID: *execution.RemoteWorkspaceID,
		SessionID: seed.sessionID, TurnID: &seed.turnID, ExecutionID: seed.executionID,
		Generation: execution.Generation, IdempotencyKey: "migration-snapshot-reference",
		Strategy: "snapshot", Status: "ready", ArtifactID: &legacySnapshotID,
		Manifest: map[string]any{"format": "synara-workspace-snapshot-v1"}, FileCount: pointerInt(1),
		TotalBytes: &legacySnapshotBytes, SHA256: &legacySnapshotSHA, CreatedAt: now,
		ReadyAt: &legacySnapshotReadyAt,
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Omit("WorkspaceCheckpointID", "UploadCleanupAt").Create(&legacySnapshot).Error; err != nil {
			return err
		}
		return tx.Create(&legacySnapshotCheckpoint).Error
	}); err != nil {
		t.Fatalf("seed pre-000025 ready Checkpoint Artifact: %v", err)
	}
	if err := Migrate(context.Background(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var migratedSnapshot persistence.Artifact
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, legacySnapshotID).Take(&migratedSnapshot).Error; err != nil {
		t.Fatal(err)
	}
	if migratedSnapshot.WorkspaceCheckpointID == nil || *migratedSnapshot.WorkspaceCheckpointID != legacySnapshotCheckpoint.ID {
		t.Fatalf("000025 did not backfill the reverse Checkpoint binding: %#v", migratedSnapshot)
	}
	readyAt := now.Add(time.Second)
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&persistence.WorkspaceCheckpoint{}).
			Where("tenant_id = ? AND id = ?", checkpoint.TenantID, checkpoint.ID).
			Updates(map[string]any{"status": "ready", "ready_at": readyAt}).Error; err != nil {
			return err
		}
		if err := tx.Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ?", checkpoint.TenantID, checkpoint.WorkspaceID).
			Update("current_checkpoint_id", checkpoint.ID).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id = ?", checkpoint.TenantID, checkpoint.ExecutionID).
			Update("restore_checkpoint_id", checkpoint.ID).Error
	}); err != nil {
		t.Fatalf("000024 did not allow an Artifact-free ready Git-reference Checkpoint: %v", err)
	}
	pending := checkpoint
	pending.ID = uuid.New()
	pending.IdempotencyKey = "migration-pending-reference"
	pending.Status = "pending"
	pending.ReadyAt = nil
	if err := db.Create(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return tx.Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ?", checkpoint.TenantID, checkpoint.WorkspaceID).
			Update("current_checkpoint_id", pending.ID).Error
	}); err == nil {
		t.Fatal("000024 allowed current_checkpoint_id to reference a pending Checkpoint")
	}
	failedAt := readyAt.Add(time.Second)
	if err := db.Model(&persistence.WorkspaceCheckpoint{}).
		Where("tenant_id = ? AND id = ?", pending.TenantID, pending.ID).
		Updates(map[string]any{
			"status": "failed", "failure_code": "migration_test_complete",
			"failure_message": "release the active Checkpoint slot", "failed_at": failedAt,
		}).Error; err != nil {
		t.Fatal(err)
	}

	boundCheckpoint := persistence.WorkspaceCheckpoint{
		ID: uuid.New(), TenantID: seed.tenantID, WorkspaceID: *execution.RemoteWorkspaceID,
		SessionID: seed.sessionID, TurnID: &seed.turnID, ExecutionID: seed.executionID,
		Generation: execution.Generation, IdempotencyKey: "migration-reverse-binding-enforcement",
		Strategy: "snapshot", Status: "pending", Manifest: map[string]any{"format": "synara-workspace-snapshot-v1"},
		FileCount: pointerInt(1), TotalBytes: &legacySnapshotBytes, CreatedAt: failedAt,
	}
	if err := db.Create(&boundCheckpoint).Error; err != nil {
		t.Fatal(err)
	}
	unboundArtifactID := uuid.New()
	unboundArtifact := legacySnapshot
	unboundArtifact.ID = unboundArtifactID
	unboundArtifact.ObjectKey = "checkpoint-migration/" + unboundArtifactID.String()
	unboundArtifact.WorkspaceCheckpointID = nil
	if err := db.Create(&unboundArtifact).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		return tx.Model(&persistence.WorkspaceCheckpoint{}).
			Where("tenant_id = ? AND id = ?", boundCheckpoint.TenantID, boundCheckpoint.ID).
			Updates(map[string]any{
				"status": "ready", "artifact_id": unboundArtifactID,
				"sha256": legacySnapshotSHA, "ready_at": failedAt.Add(time.Second),
			}).Error
	}); err == nil {
		t.Fatal("000025 allowed a ready Checkpoint to use an Artifact without the reverse binding")
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&persistence.Artifact{}).
			Where("tenant_id = ? AND id = ?", seed.tenantID, unboundArtifactID).
			Update("workspace_checkpoint_id", boundCheckpoint.ID).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.WorkspaceCheckpoint{}).
			Where("tenant_id = ? AND id = ?", boundCheckpoint.TenantID, boundCheckpoint.ID).
			Updates(map[string]any{
				"status": "ready", "artifact_id": unboundArtifactID,
				"sha256": legacySnapshotSHA, "ready_at": failedAt.Add(time.Second),
			}).Error
	}); err != nil {
		t.Fatalf("000025 rejected a correctly reverse-bound ready Checkpoint Artifact: %v", err)
	}
	if err := db.Model(&persistence.Artifact{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, unboundArtifactID).
		Update("workspace_checkpoint_id", nil).Error; err == nil {
		t.Fatal("000025 allowed a ready Checkpoint Artifact reverse binding to be removed")
	}

	scopeCheckpoint := boundCheckpoint
	scopeCheckpoint.ID = uuid.New()
	scopeCheckpoint.IdempotencyKey = "migration-artifact-scope"
	scopeCheckpoint.Status = "pending"
	scopeCheckpoint.ArtifactID = nil
	scopeCheckpoint.SHA256 = nil
	scopeCheckpoint.ReadyAt = nil
	scopeCheckpoint.CreatedAt = failedAt.Add(2 * time.Second)
	if err := db.Create(&scopeCheckpoint).Error; err != nil {
		t.Fatal(err)
	}
	wrongKind := legacySnapshot
	wrongKind.ID = uuid.New()
	wrongKind.ObjectKey = "checkpoint-migration/" + wrongKind.ID.String()
	wrongKind.Kind = "generated_file"
	wrongKind.Status = "pending"
	wrongKind.ContentType = nil
	wrongKind.SizeBytes = nil
	wrongKind.SHA256 = nil
	wrongKind.ReadyAt = nil
	wrongKind.WorkspaceCheckpointID = &scopeCheckpoint.ID
	if err := db.Create(&wrongKind).Error; err == nil {
		t.Fatal("000025 allowed a non-Checkpoint Artifact kind to bind a Workspace Checkpoint")
	}

	otherTurnID, otherExecutionID := uuid.New(), uuid.New()
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&persistence.AgentTurn{
			ID: otherTurnID, TenantID: seed.tenantID, SessionID: seed.sessionID,
			CreatedBy: session.CreatedBy, Status: "queued", InputText: "Scope test", CreatedAt: failedAt,
		}).Error; err != nil {
			return err
		}
		return tx.Create(&persistence.AgentExecution{
			ID: otherExecutionID, TenantID: seed.tenantID, SessionID: seed.sessionID, TurnID: otherTurnID,
			Attempt: 1, Status: "queued", ExecutionTargetID: execution.ExecutionTargetID,
			TargetKind: execution.TargetKind, RequestedBy: session.CreatedBy, QueuedAt: failedAt,
		}).Error
	}); err != nil {
		t.Fatal(err)
	}
	wrongExecution := legacySnapshot
	wrongExecution.ID = uuid.New()
	wrongExecution.ObjectKey = "checkpoint-migration/" + wrongExecution.ID.String()
	wrongExecution.Status = "pending"
	wrongExecution.ContentType = nil
	wrongExecution.SizeBytes = nil
	wrongExecution.SHA256 = nil
	wrongExecution.ReadyAt = nil
	wrongExecution.ExecutionID = &otherExecutionID
	wrongExecution.WorkspaceCheckpointID = &scopeCheckpoint.ID
	if err := db.Create(&wrongExecution).Error; err == nil {
		t.Fatal("000025 allowed a Checkpoint Artifact from another Execution")
	}

	otherSession := session
	otherSession.ID = uuid.New()
	otherSession.Title = "Checkpoint Artifact scope"
	otherSession.CurrentRuntimeBindingID = nil
	otherSession.ProviderResumeCursorEncrypted = nil
	otherSession.LastEventSequence = 0
	otherSession.CreatedAt = failedAt
	otherSession.UpdatedAt = failedAt
	if err := db.Create(&otherSession).Error; err != nil {
		t.Fatal(err)
	}
	wrongSession := legacySnapshot
	wrongSession.ID = uuid.New()
	wrongSession.ObjectKey = "checkpoint-migration/" + wrongSession.ID.String()
	wrongSession.Status = "pending"
	wrongSession.ContentType = nil
	wrongSession.SizeBytes = nil
	wrongSession.SHA256 = nil
	wrongSession.ReadyAt = nil
	wrongSession.SessionID = otherSession.ID
	wrongSession.WorkspaceCheckpointID = &scopeCheckpoint.ID
	if err := db.Create(&wrongSession).Error; err == nil {
		t.Fatal("000025 allowed a Checkpoint Artifact from another Session")
	}

	validPending := legacySnapshot
	validPending.ID = uuid.New()
	validPending.ObjectKey = "checkpoint-migration/" + validPending.ID.String()
	validPending.Status = "pending"
	validPending.ContentType = nil
	validPending.SizeBytes = nil
	validPending.SHA256 = nil
	validPending.ReadyAt = nil
	validPending.WorkspaceCheckpointID = &scopeCheckpoint.ID
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&validPending).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.WorkspaceCheckpoint{}).
			Where("tenant_id = ? AND id = ?", scopeCheckpoint.TenantID, scopeCheckpoint.ID).
			Updates(map[string]any{"status": "uploading", "artifact_id": validPending.ID}).Error
	}); err != nil {
		t.Fatalf("000025 rejected a valid pending Checkpoint Artifact binding: %v", err)
	}
	duplicateBinding := validPending
	duplicateBinding.ID = uuid.New()
	duplicateBinding.ObjectKey = "checkpoint-migration/" + duplicateBinding.ID.String()
	if err := db.Create(&duplicateBinding).Error; err == nil {
		t.Fatal("000025 allowed multiple Artifacts to bind the same Workspace Checkpoint")
	}
	for name, updates := range map[string]map[string]any{
		"reverse binding removal": {"workspace_checkpoint_id": nil},
		"kind change":             {"kind": "checkpoint"},
		"execution change":        {"execution_id": otherExecutionID},
		"session change":          {"session_id": otherSession.ID},
		"premature failure":       {"status": "failed"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := db.Model(&persistence.Artifact{}).
				Where("tenant_id = ? AND id = ?", seed.tenantID, validPending.ID).
				Updates(updates).Error; err == nil {
				t.Fatalf("000025 allowed active Checkpoint Artifact mutation: %#v", updates)
			}
		})
	}
	activeFailureAt := failedAt.Add(3 * time.Second)
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&persistence.WorkspaceCheckpoint{}).
			Where("tenant_id = ? AND id = ?", scopeCheckpoint.TenantID, scopeCheckpoint.ID).
			Updates(map[string]any{
				"status": "failed", "failure_code": "migration_test_failed",
				"failure_message": "active binding failure transaction", "failed_at": activeFailureAt,
			}).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.Artifact{}).
			Where("tenant_id = ? AND id = ?", seed.tenantID, validPending.ID).
			Update("status", "failed").Error
	}); err != nil {
		t.Fatalf("000025 rejected an atomic Checkpoint and Artifact failure: %v", err)
	}

	var foreignKeyDefinition string
	if err := db.Raw(`
		SELECT pg_get_constraintdef(oid)
		FROM pg_constraint
		WHERE conrelid = 'artifacts'::regclass
		  AND conname = 'fk_artifacts_workspace_checkpoint'
	`).Scan(&foreignKeyDefinition).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(foreignKeyDefinition, "FOREIGN KEY (workspace_checkpoint_id)") ||
		strings.Contains(foreignKeyDefinition, "tenant_id") {
		t.Fatalf("000025 Checkpoint Artifact foreign key should reuse the global Checkpoint primary key: %q", foreignKeyDefinition)
	}
	for tableName, constraintName := range map[string]string{
		"provider_runtime_bindings": "fk_provider_runtime_bindings_last_execution",
		"remote_workspaces":         "fk_remote_workspaces_last_execution",
	} {
		var definition string
		if err := db.Raw(`
			SELECT pg_get_constraintdef(oid)
			FROM pg_constraint
			WHERE conrelid = ?::regclass AND conname = ?
		`, tableName, constraintName).Scan(&definition).Error; err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(definition, "ON DELETE SET NULL (last_execution_id)") {
			t.Fatalf("000026 %s tenant-preserving Execution cleanup foreign key is invalid: %q", tableName, definition)
		}
	}
}

func TestCheckpointMigrationRejectsInvalidLegacyReadyArtifactKind(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_CHECKPOINT_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_CHECKPOINT_MIGRATION_DATABASE_URL is not configured")
	}
	db := openIsolatedMigrationSchema(t, databaseURL)
	if err := Migrate(context.Background(), db, migrationsThrough(t, "000016_sse_connection_leases.sql")); err != nil {
		t.Fatal(err)
	}
	seed := seedStage3MigrationState(t, db)
	if err := Migrate(context.Background(), db, migrationsThrough(t, "000023_git_credentials.sql")); err != nil {
		t.Fatal(err)
	}
	var execution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	var session persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.sessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	if execution.RemoteWorkspaceID == nil {
		t.Fatal("000020 did not bind the legacy Execution to a logical Workspace")
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	artifactID := uuid.New()
	sizeBytes := int64(18)
	sha := strings.Repeat("d", 64)
	contentType := "application/x-tar"
	readyAt := now.Add(time.Second)
	baseCommit := strings.Repeat("a", 40)
	branch := "main"
	artifact := persistence.Artifact{
		ID: artifactID, TenantID: seed.tenantID, OrganizationID: session.OrganizationID,
		ProjectID: session.ProjectID, SessionID: seed.sessionID, ExecutionID: &seed.executionID,
		Kind: "workspace_snapshot", Status: "ready", Bucket: "invalid-checkpoint-migration",
		ObjectKey: "invalid-checkpoint-migration/" + artifactID.String(), ContentType: &contentType,
		SizeBytes: &sizeBytes, SHA256: &sha, CreatedByType: "system", CreatedByID: seed.tenantID,
		ReadyAt: &readyAt, CreatedAt: now,
	}
	checkpoint := persistence.WorkspaceCheckpoint{
		ID: uuid.New(), TenantID: seed.tenantID, WorkspaceID: *execution.RemoteWorkspaceID,
		SessionID: seed.sessionID, TurnID: &seed.turnID, ExecutionID: seed.executionID,
		Generation: execution.Generation, IdempotencyKey: "invalid-legacy-ready-kind",
		Strategy: "patch", Status: "ready", BaseCommit: &baseCommit, HeadCommit: &baseCommit,
		CurrentBranch: &branch, ArtifactID: &artifactID, Manifest: map[string]any{"format": "synara-workspace-patch-v1"},
		FileCount: pointerInt(1), TotalBytes: &sizeBytes, SHA256: &sha, CreatedAt: now, ReadyAt: &readyAt,
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Omit("WorkspaceCheckpointID", "UploadCleanupAt").Create(&artifact).Error; err != nil {
			return err
		}
		return tx.Create(&checkpoint).Error
	}); err != nil {
		t.Fatalf("seed legacy kind-mismatched ready Checkpoint: %v", err)
	}
	if err := Migrate(context.Background(), db, migrationsThrough(t, "000024_checkpoint_lifecycle.sql")); err != nil {
		t.Fatalf("000024 should preserve the legacy row for 000025 validation: %v", err)
	}
	err := Migrate(context.Background(), db, migrations.Files)
	if err == nil {
		t.Fatalf("000025 did not reject the invalid legacy ready Checkpoint: %v", err)
	}
	var applied int64
	if countErr := db.Table("control_plane_schema_migrations").Where("version = ?", 25).Count(&applied).Error; countErr != nil {
		t.Fatal(countErr)
	}
	if applied != 0 {
		t.Fatal("000025 recorded a failed invalid legacy Checkpoint migration as applied")
	}
}

func openIsolatedMigrationSchema(t *testing.T, databaseURL string) *gorm.DB {
	t.Helper()
	admin, err := Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	schemaName := "checkpoint_migration_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if err := admin.Exec(`CREATE SCHEMA "` + schemaName + `"`).Error; err != nil {
		t.Fatal(err)
	}
	options := DefaultOptions()
	options.MaxOpenConnections = 1
	options.MaxIdleConnections = 1
	db, err := Open(context.Background(), databaseURL, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`SET search_path TO "` + schemaName + `"`).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sqlDB, dbErr := db.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
		_ = admin.Exec(`DROP SCHEMA IF EXISTS "` + schemaName + `" CASCADE`).Error
		if sqlDB, dbErr := admin.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

func newWorkspaceCleanupMigrationExecutionService(t *testing.T, db *gorm.DB) *executions.Service {
	t.Helper()
	cipher, err := secret.NewCursorCipher(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	config, err := platform.Defaults(platform.ProfileSingleNode)
	if err != nil {
		t.Fatal(err)
	}
	targetService := executiontargets.NewService(db, config, cipher)
	return executions.NewService(
		db,
		sessions.NewService(db, projects.NewService(db), targetService),
		30*time.Second,
		2*time.Minute,
		time.Hour,
		cipher,
		targetService,
	)
}

func assertMigrationProblemCode(t *testing.T, err error, expected string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != expected {
		t.Fatalf("problem code = %v, want %q (error: %v)", apiError, expected, err)
	}
}

func pointerInt(value int) *int { return &value }

func assertMigrationIndex(t *testing.T, db *gorm.DB, name, expectedColumns, expectedPredicate string) {
	t.Helper()
	var index struct {
		Found     bool
		Valid     bool
		Columns   string
		Predicate string
	}
	if err := db.Raw(`
		SELECT true AS found,
		       definition.indisvalid AS valid,
		       (
		         SELECT string_agg(attribute.attname, ',' ORDER BY key.ordinality)
		         FROM unnest(definition.indkey) WITH ORDINALITY AS key(attnum, ordinality)
		         JOIN pg_attribute AS attribute
		           ON attribute.attrelid = definition.indrelid
		          AND attribute.attnum = key.attnum
		       ) AS columns,
		       COALESCE(pg_get_expr(definition.indpred, definition.indrelid), '') AS predicate
		FROM pg_index AS definition
		WHERE definition.indexrelid = to_regclass(?)
	`, name).Scan(&index).Error; err != nil {
		t.Fatal(err)
	}
	if !index.Found || !index.Valid {
		t.Fatalf("migration index %s is missing or invalid: %#v", name, index)
	}
	if index.Columns != expectedColumns {
		t.Fatalf("migration index %s columns = %q, want %q", name, index.Columns, expectedColumns)
	}
	if expectedPredicate == "" {
		if index.Predicate != "" {
			t.Fatalf("migration index %s unexpectedly has predicate %q", name, index.Predicate)
		}
	} else if !strings.Contains(index.Predicate, expectedPredicate) {
		t.Fatalf("migration index %s predicate = %q, want containing %q", name, index.Predicate, expectedPredicate)
	}

	var duplicates int64
	if err := db.Raw(`
		SELECT count(*)
		FROM pg_index AS candidate
		JOIN pg_index AS duplicate
		  ON duplicate.indrelid = candidate.indrelid
		 AND duplicate.indexrelid <> candidate.indexrelid
		 AND duplicate.indisunique = candidate.indisunique
		 AND duplicate.indkey = candidate.indkey
		 AND COALESCE(pg_get_expr(duplicate.indpred, duplicate.indrelid), '') =
		     COALESCE(pg_get_expr(candidate.indpred, candidate.indrelid), '')
		WHERE candidate.indexrelid = to_regclass(?)
	`, name).Scan(&duplicates).Error; err != nil {
		t.Fatal(err)
	}
	if duplicates != 0 {
		t.Fatalf("migration index %s duplicates %d existing index definitions", name, duplicates)
	}
}

func migrationCredential(
	id, tenantID uuid.UUID,
	organizationID *uuid.UUID,
	userID uuid.UUID,
	purpose, provider, credentialType string,
	createdAt time.Time,
	expiresAt *time.Time,
) persistence.ProviderCredential {
	return persistence.ProviderCredential{
		ID: id, TenantID: tenantID, OrganizationID: organizationID,
		Name: purpose + " credential", Purpose: purpose, Provider: provider, CredentialType: credentialType,
		EncryptedPayload: []byte("encrypted-credential-payload"), EncryptedDataKey: []byte("encrypted-credential-data-key"),
		KMSProvider: "local", KMSKeyID: "migration", Version: 1,
		CreatedBy: userID, UpdatedBy: userID, CreatedAt: createdAt, UpdatedAt: createdAt, ExpiresAt: expiresAt,
	}
}

func migrationsThrough(t *testing.T, maximum string) fs.FS {
	t.Helper()
	entries, err := fs.ReadDir(migrations.Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	result := fstest.MapFS{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".sql") || name > maximum {
			continue
		}
		payload, err := fs.ReadFile(migrations.Files, name)
		if err != nil {
			t.Fatal(err)
		}
		result[name] = &fstest.MapFile{Data: payload}
	}
	return result
}

type stage3MigrationSeed struct {
	tenantID, sessionID, turnID, executionID, interactionID, credentialID uuid.UUID
}

func seedStage3MigrationState(t *testing.T, db *gorm.DB) stage3MigrationSeed {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	userID, tenantID, organizationID := uuid.New(), uuid.New(), uuid.New()
	projectID, targetID := uuid.New(), uuid.New()
	sessionID, turnID, executionID := uuid.New(), uuid.New(), uuid.New()
	workerID, interactionID := uuid.New(), uuid.New()
	credentialID := uuid.New()
	repositoryURL := "https://example.com/synara.git"
	resolvedAt := now.Add(time.Minute)
	models := []struct {
		value  any
		fields []string
	}{
		{&persistence.User{ID: userID, Email: uuid.NewString() + "@example.com", DisplayName: "Migration test", Status: "active", EmailVerifiedAt: &now}, nil},
		{&persistence.Tenant{ID: tenantID, Slug: "migration-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10], Name: "Migration tenant", Status: "active", PlanCode: "free", Region: "default", Settings: map[string]any{}, CreatedBy: userID}, nil},
		{&persistence.TenantMembership{TenantID: tenantID, UserID: userID, Role: "owner", Status: "active", JoinedAt: &now}, nil},
		{&persistence.Organization{ID: organizationID, TenantID: tenantID, Slug: "root", Name: "Root", Kind: "root", Status: "active", Settings: map[string]any{}, CreatedBy: userID}, nil},
		{&persistence.OrganizationMembership{TenantID: tenantID, OrganizationID: organizationID, UserID: userID, Role: "owner", Status: "active"}, nil},
		{&persistence.ProviderCredential{
			ID: credentialID, TenantID: tenantID, OrganizationID: &organizationID,
			Name: "Legacy provider", Provider: "codex", CredentialType: "api_key",
			EncryptedPayload: []byte("encrypted-provider-payload"), EncryptedDataKey: []byte("encrypted-provider-data-key"),
			KMSProvider: "local", KMSKeyID: "migration", Version: 1,
			CreatedBy: userID, UpdatedBy: userID, CreatedAt: now, UpdatedAt: now,
		}, []string{
			"id", "tenant_id", "organization_id", "name", "provider", "credential_type",
			"encrypted_payload", "encrypted_data_key", "kms_provider", "kms_key_id", "version",
			"created_by", "updated_by", "created_at", "updated_at",
		}},
		{&persistence.Project{ID: projectID, TenantID: tenantID, OrganizationID: organizationID, Name: "Migration project", RepositoryURL: &repositoryURL, DefaultBranch: "main", Visibility: "organization", CreatedBy: userID},
			[]string{"id", "tenant_id", "organization_id", "name", "repository_url", "default_branch", "visibility", "created_by", "created_at", "updated_at"}},
		{&persistence.ExecutionTarget{ID: targetID, TenantID: &tenantID, OrganizationID: &organizationID, Kind: "kubernetes", Name: "Migration target", Status: "active", ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}}, nil},
		{&persistence.AgentSession{ID: sessionID, TenantID: tenantID, OrganizationID: organizationID, ProjectID: projectID, CreatedBy: userID, Title: "Migration session", Status: "active", Visibility: "private", Provider: "codex", ProviderCredentialID: &credentialID, ExecutionTargetID: targetID, ProviderResumeCursorEncrypted: []byte("encrypted-cursor"), CreatedAt: now, UpdatedAt: now},
			[]string{"id", "tenant_id", "organization_id", "project_id", "created_by", "title", "status", "visibility", "provider", "provider_credential_id", "execution_target_id", "provider_resume_cursor_encrypted", "created_at", "updated_at"}},
		{&persistence.AgentTurn{ID: turnID, TenantID: tenantID, SessionID: sessionID, CreatedBy: userID, Status: "running", InputText: "Continue", StartedAt: &now, CreatedAt: now},
			[]string{"id", "tenant_id", "session_id", "created_by", "status", "input_text", "started_at", "completed_at", "created_at"}},
		{&persistence.WorkerInstance{ID: workerID, ExecutionTargetID: targetID, TargetKind: "kubernetes", ClusterID: "migration", Namespace: "default", PodName: "migration-worker", Version: "legacy", ProtocolVersion: 1, Capabilities: map[string]any{}, LeaseSupported: true, FencingSupported: true, AuthTokenHash: secret.HashToken(stage3MigrationLegacyWorkerToken), Status: "online", RegisteredAt: now, LastHeartbeatAt: now},
			[]string{"id", "execution_target_id", "target_kind", "cluster_id", "namespace", "pod_name", "version", "protocol_version", "capabilities", "lease_supported", "fencing_supported", "auth_token_hash", "status", "registered_at", "last_heartbeat_at"}},
		{&persistence.AgentExecution{ID: executionID, TenantID: tenantID, SessionID: sessionID, TurnID: turnID, Attempt: 1, Status: "waiting-for-approval", ExecutionTargetID: targetID, TargetKind: "kubernetes", WorkerID: &workerID, Generation: 1, RequestedBy: userID, QueuedAt: now, StartedAt: &now},
			[]string{"id", "tenant_id", "session_id", "turn_id", "attempt", "status", "execution_target_id", "target_kind", "worker_id", "generation", "requested_by", "queued_at", "started_at"}},
		{&persistence.WorkerLease{ExecutionID: executionID, TenantID: tenantID, WorkerID: workerID, Generation: 1, LeaseTokenHash: []byte("migration-lease-token"), AcquiredAt: now, HeartbeatAt: now, ExpiresAt: now.Add(time.Hour)}, nil},
		{&persistence.SessionEvent{TenantID: tenantID, OrganizationID: organizationID, ProjectID: projectID, SessionID: sessionID, Sequence: 1, EventID: uuid.New(), EventVersion: 1, EventType: "turn.created", ActorType: "user", ActorID: &userID, ExecutionID: &executionID, Payload: map[string]any{"inputText": "Continue"}, OccurredAt: now}, nil},
		{&persistence.ExecutionInteraction{ID: interactionID, TenantID: tenantID, ExecutionID: executionID, SessionID: sessionID, WorkerID: workerID, Generation: 1, RequestID: "approval-migration", Kind: "approval", Status: "resolved", Payload: map[string]any{"summary": "Run"}, Resolution: map[string]any{"decision": "accept"}, RequestedAt: now, ResolvedAt: &resolvedAt, ResolvedBy: &userID},
			[]string{"id", "tenant_id", "execution_id", "session_id", "worker_id", "generation", "request_id", "kind", "status", "payload", "resolution", "requested_at", "resolved_at", "resolved_by"}},
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		for _, model := range models {
			query := tx
			if len(model.fields) > 0 {
				query = query.Select(model.fields)
			}
			if err := query.Create(model.value).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed Stage 3 migration state: %v", err)
	}
	return stage3MigrationSeed{
		tenantID: tenantID, sessionID: sessionID, turnID: turnID, executionID: executionID,
		interactionID: interactionID, credentialID: credentialID,
	}
}
