package database

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/observability"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestStage4RecoveryBundleAndResourceLifecycleMigrations(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL is not configured")
	}
	db := openIsolatedMigrationSchema(t, databaseURL)
	if err := Migrate(context.Background(), db, migrationsThrough(t, "000016_sse_connection_leases.sql")); err != nil {
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
	if session.ResourceState != "waiting" || session.MeaningfulActivityAt.IsZero() || session.ResourceIdleSince != nil {
		t.Fatalf("000044 did not backfill the active Session resource timeline: %#v", session)
	}
	if session.AbsoluteExpiresAt != nil {
		t.Fatalf("000044 invented an absolute expiry for a legacy Session: %#v", session.AbsoluteExpiresAt)
	}
	if session.WaitingKeepAliveSeconds != 900 || session.SuspendAfterIdleSeconds != 1800 ||
		session.AbsoluteSessionLifetimeSeconds != nil || session.WorkspaceRetentionDays != 30 ||
		session.WarmPoolMode != "disabled" {
		t.Fatalf("000046 did not safely backfill the frozen Session policy snapshot: %#v", session)
	}
	waitingPolicy := 1200
	tenantLifecyclePolicy := persistence.TenantResourceLifecyclePolicy{
		TenantID: seed.tenantID, WaitingKeepAliveSeconds: &waitingPolicy,
		Version: 1, UpdatedBy: session.CreatedBy,
	}
	if err := db.Create(&tenantLifecyclePolicy).Error; err != nil {
		t.Fatalf("insert Tenant Resource Lifecycle Policy: %v", err)
	}
	projectSuspend := 600
	projectLifecyclePolicy := persistence.ProjectResourceLifecyclePolicy{
		ProjectID: session.ProjectID, TenantID: seed.tenantID,
		SuspendAfterIdleSeconds: &projectSuspend, Version: 1, UpdatedBy: session.CreatedBy,
	}
	if err := db.Create(&projectLifecyclePolicy).Error; err != nil {
		t.Fatalf("insert Project Resource Lifecycle Policy: %v", err)
	}
	invalidProjectLifecyclePolicy := persistence.ProjectResourceLifecyclePolicy{
		ProjectID: uuid.New(), TenantID: seed.tenantID,
		SuspendAfterIdleSeconds: &projectSuspend, Version: 1, UpdatedBy: session.CreatedBy,
	}
	assertStage4MigrationRejected(
		t,
		db.Create(&invalidProjectLifecyclePolicy).Error,
		"project_resource_lifecycle_policies_tenant_id_project_id_fkey",
	)
	if err := db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, session.ID).
		Update("waiting_keep_alive_seconds", waitingPolicy).Error; err == nil {
		t.Fatal("000046 allowed mutation of a frozen Session Resource Lifecycle Policy snapshot")
	}
	invalidLifecycleSession := persistence.AgentSession{
		ID: uuid.New(), TenantID: seed.tenantID, OrganizationID: session.OrganizationID,
		ProjectID: session.ProjectID, CreatedBy: session.CreatedBy, Title: "Invalid lifecycle session",
		Status: "active", Visibility: "private", Provider: "codex", ExecutionTargetID: session.ExecutionTargetID,
		ProviderResumeCursorState: "absent", ResourceState: "active", MeaningfulActivityAt: time.Now().UTC(),
		WaitingKeepAliveSeconds: 30, SuspendAfterIdleSeconds: 1800, WorkspaceRetentionDays: 30, WarmPoolMode: "disabled",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	assertStage4MigrationRejected(t, db.Create(&invalidLifecycleSession).Error, "chk_agent_sessions_waiting_keep_alive")

	memorySHA := strings.Repeat("d", 64)
	memoryContentType := "text/markdown; charset=utf-8"
	memorySize := int64(12)
	memoryReadyAt := time.Now().UTC()
	memoryArtifact := persistence.Artifact{
		ID: uuid.New(), TenantID: seed.tenantID, OrganizationID: session.OrganizationID,
		ProjectID: session.ProjectID, SessionID: session.ID, Kind: "memory", Status: "ready",
		Bucket: "migration-memory", ObjectKey: "memory/" + uuid.NewString(),
		ContentType: &memoryContentType, SizeBytes: &memorySize, SHA256: &memorySHA,
		CreatedByType: "user", CreatedByID: session.CreatedBy,
		ReadyAt: &memoryReadyAt, CreatedAt: memoryReadyAt,
	}
	if err := db.Create(&memoryArtifact).Error; err != nil {
		t.Fatalf("000045 did not allow a Memory Artifact: %v", err)
	}
	memoryHead := persistence.AgentMemoryHead{
		ID: uuid.New(), TenantID: seed.tenantID, ScopeType: "session", ScopeSessionID: &session.ID,
		MemoryKey: "instructions", Version: 0, Enabled: false,
		UpdatedBy: session.CreatedBy, CreatedAt: memoryReadyAt, UpdatedAt: memoryReadyAt,
	}
	if err := db.Create(&memoryHead).Error; err != nil {
		t.Fatalf("insert Memory Head: %v", err)
	}
	invalidMemoryHead := persistence.AgentMemoryHead{
		ID: uuid.New(), TenantID: seed.tenantID, ScopeType: "session", ScopeSessionID: &session.ID,
		MemoryKey: "Bad Key", Version: 0, Enabled: false,
		UpdatedBy: session.CreatedBy, CreatedAt: memoryReadyAt, UpdatedAt: memoryReadyAt,
	}
	assertStage4MigrationRejected(t, db.Create(&invalidMemoryHead).Error, "agent_memory_heads_memory_key_check")
	memoryRevision := persistence.AgentMemoryRevision{
		ID: uuid.New(), TenantID: seed.tenantID, MemoryHeadID: memoryHead.ID,
		RevisionNumber: 1, ArtifactID: memoryArtifact.ID, SHA256: memorySHA,
		MediaType: "text/markdown", SizeBytes: memorySize,
		CreatedBy: session.CreatedBy, CreatedAt: memoryReadyAt,
	}
	if err := db.Create(&memoryRevision).Error; err != nil {
		t.Fatalf("insert immutable Memory Revision: %v", err)
	}
	if err := db.Model(&persistence.AgentMemoryHead{}).
		Where("tenant_id = ? AND id = ? AND version = 0", seed.tenantID, memoryHead.ID).
		Updates(map[string]any{"current_revision_id": memoryRevision.ID, "version": 1, "enabled": true}).Error; err != nil {
		t.Fatalf("publish Memory Revision: %v", err)
	}
	if err := db.Model(&persistence.AgentMemoryRevision{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, memoryRevision.ID).
		Update("sha256", strings.Repeat("e", 64)).Error; err == nil {
		t.Fatal("000045 allowed immutable Memory Revision mutation")
	}
	if err := db.Model(&persistence.Artifact{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, memoryArtifact.ID).
		Update("status", "deleting").Error; err == nil {
		t.Fatal("000045 allowed deletion of a Memory-pinned Artifact")
	}
	if err := db.Model(&persistence.Artifact{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, memoryArtifact.ID).
		Update("content_type", "application/json").Error; err == nil {
		t.Fatal("000045 allowed media type mutation of a Memory-pinned Artifact")
	}
	if err := db.Model(&persistence.Artifact{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, memoryArtifact.ID).
		Update("size_bytes", memorySize+1).Error; err == nil {
		t.Fatal("000045 allowed size mutation of a Memory-pinned Artifact")
	}
	if err := db.Model(&persistence.Artifact{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, memoryArtifact.ID).
		Update("object_key", "memory/rekeyed").Error; err == nil {
		t.Fatal("000045 allowed object key mutation of a Memory-pinned Artifact")
	}
	assertStage4MigrationRejected(
		t,
		db.Delete(&persistence.Artifact{}, "tenant_id = ? AND id = ?", seed.tenantID, memoryArtifact.ID).Error,
		"agent_memory_revisions_tenant_id_artifact_id_fkey",
	)

	firstID := uuid.New()
	first := persistence.ExecutionRecoveryBundle{
		ID: firstID, TenantID: seed.tenantID, SessionID: seed.sessionID,
		TurnID: seed.turnID, ExecutionID: seed.executionID, Generation: 1,
		SchemaVersion: 1, RecoveryReason: "initial-claim", AuthoritativeHistorySequence: 0,
		Payload: map[string]any{"schemaVersion": 1}, PayloadSHA256: strings.Repeat("a", 64),
		CreatedAt: time.Now().UTC(),
	}
	if err := db.Create(&first).Error; err != nil {
		t.Fatalf("insert first Recovery Bundle: %v", err)
	}
	invalidBundle := first
	invalidBundle.ID = uuid.New()
	invalidBundle.TurnID = uuid.New()
	assertStage4MigrationRejected(
		t,
		db.Create(&invalidBundle).Error,
		"Recovery Bundle must reference an existing Tenant Session Turn",
	)
	if err := db.Model(&persistence.ExecutionRecoveryBundle{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, firstID).
		Update("payload_sha256", strings.Repeat("b", 64)).Error; err == nil {
		t.Fatal("000043 allowed Recovery Bundle mutation")
	}
	if err := db.Delete(&persistence.ExecutionRecoveryBundle{}, "tenant_id = ? AND id = ?", seed.tenantID, firstID).Error; err == nil {
		t.Fatal("000043 allowed Recovery Bundle deletion")
	}

	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).
		Update("generation", 2).Error; err != nil {
		t.Fatal(err)
	}
	second := persistence.ExecutionRecoveryBundle{
		ID: uuid.New(), TenantID: seed.tenantID, SessionID: seed.sessionID,
		TurnID: seed.turnID, ExecutionID: seed.executionID, Generation: 2,
		SchemaVersion: 1, RecoveryReason: "execution-recovery", PreviousBundleID: &firstID,
		AuthoritativeHistorySequence: 0, Payload: map[string]any{"schemaVersion": 1},
		PayloadSHA256: strings.Repeat("c", 64), CreatedAt: time.Now().UTC(),
	}
	if err := db.Create(&second).Error; err != nil {
		t.Fatalf("insert linked Recovery Bundle: %v", err)
	}
	invalid := second
	invalid.ID = uuid.New()
	invalid.Generation = 3
	invalid.PreviousBundleID = nil
	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).
		Update("generation", 3).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&invalid).Error; err == nil {
		t.Fatal("000043 accepted a recovered Generation without predecessor lineage")
	}

	var suspendedExecution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).Take(&suspendedExecution).Error; err != nil {
		t.Fatal(err)
	}
	if suspendedExecution.WorkerID == nil {
		t.Fatal("migration fixture lost its Worker before the suspend handshake")
	}
	var migratedLease persistence.WorkerLease
	if err := db.Where("tenant_id = ? AND execution_id = ?", seed.tenantID, seed.executionID).
		Take(&migratedLease).Error; err != nil {
		t.Fatal(err)
	}
	var migratedWorker persistence.WorkerInstance
	if err := db.Where("id = ?", *suspendedExecution.WorkerID).Take(&migratedWorker).Error; err != nil {
		t.Fatal(err)
	}
	if migratedLease.WorkerIncarnation != migratedWorker.Incarnation ||
		migratedLease.WorkerInstanceUID != migratedWorker.InstanceUID {
		t.Fatalf("000051 did not bind the legacy Worker lease to its physical incarnation: lease=%#v worker=%#v", migratedLease, migratedWorker)
	}
	if err := db.Model(&persistence.WorkerLease{}).
		Where("tenant_id = ? AND execution_id = ?", seed.tenantID, seed.executionID).
		Update("worker_instance_uid", uuid.NewString()).Error; err == nil {
		t.Fatal("000051 allowed Worker lease incarnation lineage mutation")
	}
	suspendRequestedAt := time.Now().UTC()
	suspendAttempt := persistence.ExecutionSuspendAttempt{
		ID: uuid.New(), TenantID: seed.tenantID, SessionID: seed.sessionID, TurnID: seed.turnID,
		ExecutionID: seed.executionID, WorkerID: *suspendedExecution.WorkerID,
		ExecutionTargetID: suspendedExecution.ExecutionTargetID,
		Generation:        suspendedExecution.Generation, Reason: "waiting-keepalive", Status: "checkpointing",
		RequestedAt: suspendRequestedAt, CheckpointDeadlineAt: suspendRequestedAt.Add(2 * time.Minute),
	}
	if err := db.Create(&suspendAttempt).Error; err != nil {
		t.Fatalf("000047 did not allow a generation-fenced suspend attempt: %v", err)
	}
	if err := db.Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND id = ? AND status = ?", seed.tenantID, suspendAttempt.ID, "checkpointing").
		Updates(map[string]any{"status": "completed", "completed_at": suspendRequestedAt}).Error; err == nil {
		t.Fatal("000047 allowed suspend completion before Provider quiesce was durably recorded")
	}
	providerQuiescedAt := suspendRequestedAt.Add(time.Minute)
	if err := db.Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND id = ? AND status = ?", seed.tenantID, suspendAttempt.ID, "checkpointing").
		Update("provider_quiesced_at", providerQuiescedAt).Error; err != nil {
		t.Fatalf("000047 rejected durable Provider quiesce recording: %v", err)
	}
	if err := db.Model(&persistence.WorkerLease{}).
		Where("tenant_id = ? AND execution_id = ?", seed.tenantID, seed.executionID).
		Delete(&persistence.WorkerLease{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND id = ? AND status = ?", seed.tenantID, suspendAttempt.ID, "checkpointing").
		Updates(map[string]any{"status": "completed", "completed_at": providerQuiescedAt}).Error; err != nil {
		t.Fatalf("000047 rejected suspend completion: %v", err)
	}
	if err := db.Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, suspendAttempt.ID).
		Update("provider_quiesced_at", providerQuiescedAt.Add(time.Second)).Error; err == nil {
		t.Fatal("000047 allowed Provider quiesce provenance to change after completion")
	}
	if err := db.Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, suspendAttempt.ID).
		Update("failure_message", "rewritten after completion").Error; err == nil {
		t.Fatal("000047 allowed terminal suspend failure provenance to change after completion")
	}
	if err := db.Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, suspendAttempt.ID).
		Update("checkpoint_deadline_at", suspendAttempt.CheckpointDeadlineAt.Add(time.Minute)).Error; err == nil {
		t.Fatal("000047 allowed suspend attempt lineage to change after completion")
	}
	if err := db.Model(&persistence.ExecutionInteraction{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, seed.interactionID).
		Updates(map[string]any{
			"delivery_status": "resume-recorded", "delivery_worker_id": nil,
			"delivery_generation": nil, "delivered_at": nil, "acknowledged_at": nil,
		}).Error; err != nil {
		t.Fatalf("000047 rejected a resolution recorded after Provider fencing: %v", err)
	}
	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).
		Updates(map[string]any{
			"status": "suspended", "worker_id": nil, "next_recovery_reason": "suspend-resume",
		}).Error; err != nil {
		t.Fatalf("000047 rejected a lease-free suspended Execution: %v", err)
	}
	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).
		Update("worker_id", *suspendedExecution.WorkerID).Error; err == nil {
		t.Fatal("000047 allowed a suspended Execution to regain a Worker owner")
	}

	workerManifest := persistence.WorkerManifest{
		ID: uuid.New(), ManifestHash: strings.Repeat("d", 64), WorkerBuildVersion: "legacy-worker",
		WorkerProtocolMinimum: 2, WorkerProtocolMaximum: 2,
		RuntimeEventMinimum: 2, RuntimeEventMaximum: 2,
		OperatingSystem: "linux", Architecture: "amd64", FeatureFlags: map[string]any{},
		CreatedAt: time.Now().UTC(),
	}
	if err := db.Create(&workerManifest).Error; err != nil {
		t.Fatalf("insert legacy-shaped Worker Manifest after 000050: %v", err)
	}
	if workerManifest.ProcessContainmentMode != "none" ||
		workerManifest.ProcessContainmentProbeSHA256 != nil {
		t.Fatalf("000050 guessed strict containment for a legacy Worker Manifest: %#v", workerManifest)
	}
	if err := db.Model(&persistence.WorkerManifest{}).
		Where("id = ?", workerManifest.ID).
		Update("process_containment_mode", "cgroup-v2").Error; err == nil {
		t.Fatal("000050 allowed Worker Manifest containment evidence to be rewritten")
	}

	assertMigrationIndex(
		t, db, "idx_execution_recovery_bundles_session_created",
		"tenant_id,session_id,created_at,id", "",
	)
	assertMigrationIndex(
		t, db, "idx_session_events_execution_started_lookup",
		"tenant_id,session_id,execution_id,generation,occurred_at", "execution.started",
	)
	assertMigrationIndex(
		t, db, "idx_agent_sessions_resource_idle",
		"tenant_id,resource_idle_since,id", "resource_idle_since IS NOT NULL",
	)
	assertMigrationIndex(
		t, db, "idx_agent_memory_revisions_artifact",
		"tenant_id,artifact_id,id", "",
	)
	assertMigrationIndex(
		t, db, "idx_project_resource_lifecycle_policies_tenant",
		"tenant_id,project_id", "",
	)
	assertMigrationIndex(
		t, db, "idx_execution_suspend_attempts_deadline",
		"checkpoint_deadline_at,tenant_id,execution_id,id", "status = 'checkpointing'",
	)
	assertMigrationIndex(
		t, db, "idx_session_events_execution_generation_lifecycle",
		"tenant_id,execution_id,generation,event_type,occurred_at,event_id", "execution.started",
	)
	assertMigrationIndex(
		t, db, "idx_session_events_provider_resume_claim_decision",
		"occurred_at,tenant_id,event_id", "execution.leased",
	)
	assertMigrationIndex(
		t, db, "idx_session_events_provider_resume_runtime_fallback",
		"occurred_at,tenant_id,event_id", "runtime.warning",
	)
}

func TestWorkerProcessContainmentAttestationMigration(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL is not configured")
	}
	db := openIsolatedMigrationSchema(t, databaseURL)
	if err := Migrate(
		context.Background(), db, migrationsThrough(t, "000050_worker_process_containment_manifest.sql"),
	); err != nil {
		t.Fatal(err)
	}

	legacyManifestID := uuid.New()
	if err := db.Exec(`
		INSERT INTO worker_manifests (
			id, manifest_hash, worker_build_version,
			worker_protocol_minimum, worker_protocol_maximum,
			runtime_event_minimum, runtime_event_maximum,
			operating_system, architecture, image_digest, feature_flags, created_at,
			process_containment_mode, process_containment_supervisor_version,
			process_containment_probe_version, process_containment_probe_sha256,
			process_containment_supervisor_identity, process_containment_provider_identity
		) VALUES (?, ?, ?, 2, 2, 2, 2, 'linux', 'amd64', ?, '{}'::jsonb, now(),
			'cgroup-v2', 'legacy-supervisor-v1', 1, ?, 'root-supervisor', 'provider-user')`,
		legacyManifestID,
		strings.Repeat("7", 64),
		"legacy-strict-worker",
		"sha256:"+strings.Repeat("8", 64),
		strings.Repeat("9", 64),
	).Error; err != nil {
		t.Fatalf("insert pre-000052 strict Worker Manifest: %v", err)
	}

	db.Config.TranslateError = false
	if err := Migrate(context.Background(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var migrated persistence.WorkerManifest
	if err := db.Where("id = ?", legacyManifestID).Take(&migrated).Error; err != nil {
		t.Fatal(err)
	}
	if migrated.ProcessContainmentTrustMode != "legacy-untrusted" ||
		migrated.ProcessContainmentAttestationKeyID != nil ||
		migrated.ProcessContainmentAttestationKeySHA256 != nil {
		t.Fatalf("000052 promoted legacy self-attestation to trusted containment: %#v", migrated)
	}

	// The table-wide content-addressed immutability trigger also rejects this
	// update. Disable it momentarily so this assertion exercises the narrower
	// 000050 trigger function that 000052 extends with attestation provenance.
	if err := db.Exec(`ALTER TABLE worker_manifests DISABLE TRIGGER trg_worker_manifests_immutable`).Error; err != nil {
		t.Fatal(err)
	}
	updateErr := db.Model(&persistence.WorkerManifest{}).
		Where("id = ?", legacyManifestID).
		Updates(map[string]any{
			"process_containment_trust_mode":             "signed-v1",
			"process_containment_attestation_key_id":     "rotated-key",
			"process_containment_attestation_key_sha256": strings.Repeat("a", 64),
		}).Error
	if err := db.Exec(`ALTER TABLE worker_manifests ENABLE TRIGGER trg_worker_manifests_immutable`).Error; err != nil {
		t.Fatal(err)
	}
	if updateErr == nil || !strings.Contains(updateErr.Error(), "process-containment evidence is immutable") {
		t.Fatalf("000052 did not extend the existing immutability trigger to attestation provenance: %v", updateErr)
	}
}

func TestStage4ObservabilityMetricsGatherOnPostgres(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL is not configured")
	}
	db := openIsolatedMigrationSchema(t, databaseURL)
	if err := Migrate(context.Background(), db, migrationsThrough(t, "000016_sse_connection_leases.sql")); err != nil {
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
	var execution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", seed.tenantID, seed.executionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.WorkerID == nil {
		t.Fatal("migration fixture lost its Worker before observability verification")
	}
	var nextSequence int64
	if err := db.Table("session_events").
		Where("tenant_id = ? AND session_id = ?", seed.tenantID, seed.sessionID).
		Select("COALESCE(MAX(sequence), 0) + 1").
		Scan(&nextSequence).Error; err != nil {
		t.Fatalf("load next Session Event sequence: %v", err)
	}

	bundleCreatedAt := time.Now().UTC().Truncate(time.Microsecond)
	startedAt := bundleCreatedAt.Add(5 * time.Second)
	bundle := persistence.ExecutionRecoveryBundle{
		ID: uuid.New(), TenantID: seed.tenantID, SessionID: seed.sessionID,
		TurnID: seed.turnID, ExecutionID: seed.executionID, Generation: execution.Generation,
		SchemaVersion: 1, RecoveryReason: "initial-claim", AuthoritativeHistorySequence: 1,
		Payload: map[string]any{"schemaVersion": 1}, PayloadSHA256: strings.Repeat("a", 64),
		CreatedAt: bundleCreatedAt,
	}
	if err := db.Create(&bundle).Error; err != nil {
		t.Fatalf("insert Recovery Bundle for observability metrics: %v", err)
	}
	if err := db.Create(&persistence.SessionEvent{
		TenantID: seed.tenantID, OrganizationID: session.OrganizationID, ProjectID: session.ProjectID,
		SessionID: session.ID, Sequence: nextSequence, EventID: uuid.New(),
		EventVersion: 1, EventType: "execution.started", ActorType: "worker",
		ExecutionID: &execution.ID, WorkerID: execution.WorkerID, Generation: &execution.Generation,
		Payload: map[string]any{}, OccurredAt: startedAt,
	}).Error; err != nil {
		t.Fatalf("insert execution.started Session Event for observability metrics: %v", err)
	}
	providerReadyAt := startedAt.Add(2 * time.Second)
	for _, event := range []persistence.SessionEvent{
		{
			TenantID: seed.tenantID, OrganizationID: session.OrganizationID, ProjectID: session.ProjectID,
			SessionID: session.ID, Sequence: nextSequence + 1, EventID: uuid.New(),
			EventVersion: 2, EventType: "session.started", ActorType: "worker",
			ExecutionID: &execution.ID, WorkerID: execution.WorkerID, Generation: &execution.Generation,
			Payload: map[string]any{}, OccurredAt: providerReadyAt,
		},
		{
			TenantID: seed.tenantID, OrganizationID: session.OrganizationID, ProjectID: session.ProjectID,
			SessionID: session.ID, Sequence: nextSequence + 2, EventID: uuid.New(),
			EventVersion: 2, EventType: "execution.leased", ActorType: "worker",
			ExecutionID: &execution.ID, WorkerID: execution.WorkerID, Generation: &execution.Generation,
			Payload: map[string]any{
				"targetKind": "kubernetes",
				"providerResume": map[string]any{
					"selectedStrategy": "authoritative-history", "reasonCode": "cursor_absent",
				},
			},
			OccurredAt: bundleCreatedAt,
		},
		{
			TenantID: seed.tenantID, OrganizationID: session.OrganizationID, ProjectID: session.ProjectID,
			SessionID: session.ID, Sequence: nextSequence + 3, EventID: uuid.New(),
			EventVersion: 2, EventType: "runtime.warning", ActorType: "worker",
			ExecutionID: &execution.ID, WorkerID: execution.WorkerID, Generation: &execution.Generation,
			Payload: map[string]any{
				"message": "Native Provider resume failed before turn activity; authoritative history was selected.",
				"detail": map[string]any{
					"kind": "session_resume", "attemptedStrategy": "native-cursor",
					"selectedStrategy": "authoritative-history", "outcome": "fallback_selected",
					"reasonCode": "session_resume_invalid", "fallbackSafety": "before_turn_activity",
					"authoritativeHistorySequence": 1, "provider": "codex",
				},
			},
			OccurredAt: providerReadyAt,
		},
	} {
		if err := db.Create(&event).Error; err != nil {
			t.Fatalf("insert Stage 4 observability Session Event %q: %v", event.EventType, err)
		}
	}
	// The production append path records these lifecycle timestamps in the
	// durable generation fact in the same transaction as the Session Event.
	// This migration fixture inserts historical events directly, so mirror that
	// authoritative write instead of asking observability to re-derive facts
	// from the event log.
	if err := db.Model(&persistence.ExecutionGenerationFact{}).
		Where("tenant_id = ? AND execution_id = ? AND generation = ?", seed.tenantID, seed.executionID, execution.Generation).
		Updates(map[string]any{
			"execution_started_at":                  startedAt,
			"provider_ready_at":                     providerReadyAt,
			"resume_attempted_strategy":             "native-cursor",
			"resume_selected_strategy":              "authoritative-history",
			"resume_fallback_outcome":               "fallback_selected",
			"resume_fallback_reason_code":           "session_resume_invalid",
			"resume_fallback_safety":                "before_turn_activity",
			"resume_fallback_provider":              "codex",
			"resume_authoritative_history_sequence": int64(1),
			"updated_at":                            providerReadyAt,
		}).Error; err != nil {
		t.Fatalf("record generation lifecycle facts for observability metrics: %v", err)
	}

	registry := observability.New(db, observability.Config{
		SessionIdleTTL:         7 * 24 * time.Hour,
		WorkerHeartbeatTimeout: 90 * time.Second,
	})
	payload, err := registry.Gather(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	metrics := string(payload)
	for _, expected := range []string{
		`synara_execution_recovery_bundles{recovery_reason="initial-claim"} 1`,
		`synara_execution_generation_start_outcomes{outcome="started",recovery_reason="initial-claim",target_kind="kubernetes"} 1`,
		`synara_execution_generation_provider_ready_outcomes{outcome="ready",recovery_reason="initial-claim",target_kind="kubernetes"} 1`,
		`synara_execution_generation_startup_duration_seconds_count{recovery_reason="initial-claim",target_kind="kubernetes"} 1`,
		`synara_execution_generation_provider_ready_duration_seconds_count{recovery_reason="initial-claim",target_kind="kubernetes"} 1`,
		`synara_provider_resume_claim_decisions{reason_code="cursor_absent",selected_strategy="authoritative-history",target_kind="kubernetes"} 1`,
		`synara_provider_resume_runtime_fallbacks{provider="codex",reason_code="session_resume_invalid"} 1`,
		`synara_metrics_collection_success 1`,
	} {
		if !strings.Contains(metrics, expected) {
			t.Fatalf("metrics omitted %q:\n%s", expected, metrics)
		}
	}
}

func assertStage4MigrationRejected(t *testing.T, err error, expected string) {
	t.Helper()
	if err == nil {
		t.Fatalf("migration accepted invalid Stage 4 state (want containing %q)", expected)
	}
	if strings.Contains(err.Error(), expected) {
		return
	}
	if expected == "Recovery Bundle must reference an existing Tenant Session Turn" &&
		(errors.Is(err, gorm.ErrCheckConstraintViolated) || errors.Is(err, gorm.ErrForeignKeyViolated)) {
		return
	}
	if strings.HasSuffix(expected, "_fkey") && errors.Is(err, gorm.ErrForeignKeyViolated) {
		return
	}
	if (strings.HasPrefix(expected, "chk_") || strings.HasSuffix(expected, "_check")) &&
		errors.Is(err, gorm.ErrCheckConstraintViolated) {
		return
	}
	t.Fatalf("migration returned wrong Stage 4 rejection: %v (want containing %q or matching translated constraint class)", err, expected)
}
