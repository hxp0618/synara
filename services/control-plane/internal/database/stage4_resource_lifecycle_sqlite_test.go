package database

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

type stage4SQLiteFixture struct {
	db                  *gorm.DB
	tenantID            uuid.UUID
	organizationID      uuid.UUID
	projectID           uuid.UUID
	userID              uuid.UUID
	targetID            uuid.UUID
	otherTenantID       uuid.UUID
	otherOrganizationID uuid.UUID
	otherProjectID      uuid.UUID
}

func TestSQLiteStage4ResourceLifecycleSafetyMirrorsKeyConstraints(t *testing.T) {
	fixture := openStage4SQLiteFixture(t)

	for _, name := range []string{
		"trg_execution_recovery_bundles_insert",
		"trg_agent_memory_heads_validate_insert",
		"trg_artifacts_agent_memory_protected_delete",
		"trg_worker_leases_matches_execution_insert",
		"trg_execution_suspend_attempts_lineage_insert",
		"trg_execution_interactions_delivery_shape_insert",
		"trg_execution_interactions_resume_binding_update",
		"trg_tenant_resource_lifecycle_policy_parent_insert",
		"trg_project_resource_lifecycle_policy_parent_insert",
		"trg_agent_sessions_resource_lifecycle_validate_insert",
		"idx_session_events_execution_generation_lifecycle",
		"idx_session_events_provider_resume_claim_decision",
		"idx_session_events_provider_resume_runtime_fallback",
	} {
		var count int64
		if err := fixture.db.Raw(`SELECT count(*) FROM sqlite_master WHERE name = ?`, name).Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("SQLite safety object %s count = %d, want 1", name, count)
		}
	}

	now := time.Now().UTC().Truncate(time.Second)
	sessionID := fixture.createSession(t, fixture.tenantID, fixture.organizationID, fixture.projectID, "Stage 4 SQLite", now)
	turnID := fixture.createTurn(t, fixture.tenantID, sessionID, now)
	workerID, workerInstanceUID := fixture.createWorker(t, "sqlite-stage4-primary", now)
	executionID := fixture.createExecution(t, sessionID, turnID, workerID, 1, "waiting-for-approval", now)
	validLease := persistence.WorkerLease{
		ExecutionID: executionID, TenantID: fixture.tenantID, WorkerID: workerID,
		WorkerIncarnation: 1, WorkerInstanceUID: workerInstanceUID, Generation: 1,
		LeaseTokenHash: []byte("lease-token"), AcquiredAt: now, HeartbeatAt: now, ExpiresAt: now.Add(time.Minute),
	}
	if err := fixture.db.Create(&validLease).Error; err != nil {
		t.Fatalf("SQLite rejected valid Worker lease for waiting-for-approval Execution: %v", err)
	}

	initialBundle := persistence.ExecutionRecoveryBundle{
		ID: uuid.New(), TenantID: fixture.tenantID, SessionID: sessionID, TurnID: turnID, ExecutionID: executionID,
		Generation: 1, SchemaVersion: 1, RecoveryReason: "initial-claim", AuthoritativeHistorySequence: 0,
		Payload: map[string]any{"schemaVersion": 1}, PayloadSHA256: strings.Repeat("a", 64), CreatedAt: now,
	}
	if err := fixture.db.Create(&initialBundle).Error; err != nil {
		t.Fatalf("SQLite rejected valid Recovery Bundle: %v", err)
	}
	invalidBundle := initialBundle
	invalidBundle.ID = uuid.New()
	invalidBundle.TurnID = uuid.New()
	assertSQLiteStage4Rejected(t, fixture.db.Create(&invalidBundle).Error, "existing Tenant Session Turn")

	invalidProjectHead := persistence.AgentMemoryHead{
		ID: uuid.New(), TenantID: fixture.tenantID, ScopeType: "project", ScopeProjectID: &fixture.otherProjectID,
		MemoryKey: "instructions", Version: 0, Enabled: false, UpdatedBy: fixture.userID, CreatedAt: now, UpdatedAt: now,
	}
	assertSQLiteStage4Rejected(t, fixture.db.Create(&invalidProjectHead).Error, "existing Tenant Project")
	invalidKeyHead := persistence.AgentMemoryHead{
		ID: uuid.New(), TenantID: fixture.tenantID, ScopeType: "session", ScopeSessionID: &sessionID,
		MemoryKey: "Bad Key", Version: 0, Enabled: false, UpdatedBy: fixture.userID, CreatedAt: now, UpdatedAt: now,
	}
	assertSQLiteStage4Rejected(t, fixture.db.Create(&invalidKeyHead).Error, "scope, key, or version shape")

	memoryArtifact := fixture.createMemoryArtifact(t, sessionID, strings.Repeat("b", 64), now)
	memoryHead := persistence.AgentMemoryHead{
		ID: uuid.New(), TenantID: fixture.tenantID, ScopeType: "session", ScopeSessionID: &sessionID,
		MemoryKey: "instructions", Version: 0, Enabled: false, UpdatedBy: fixture.userID, CreatedAt: now, UpdatedAt: now,
	}
	if err := fixture.db.Create(&memoryHead).Error; err != nil {
		t.Fatalf("SQLite rejected valid Memory Head: %v", err)
	}
	revision := persistence.AgentMemoryRevision{
		ID: uuid.New(), TenantID: fixture.tenantID, MemoryHeadID: memoryHead.ID, RevisionNumber: 1,
		ArtifactID: memoryArtifact.ID, SHA256: *memoryArtifact.SHA256,
		MediaType: "text/markdown", SizeBytes: *memoryArtifact.SizeBytes,
		CreatedBy: fixture.userID, CreatedAt: now,
	}
	if err := fixture.db.Create(&revision).Error; err != nil {
		t.Fatalf("SQLite rejected valid Memory Revision: %v", err)
	}
	if err := fixture.db.Model(&persistence.AgentMemoryHead{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, memoryHead.ID).
		Updates(map[string]any{"current_revision_id": revision.ID, "version": 1, "enabled": true}).Error; err != nil {
		t.Fatalf("SQLite rejected valid Memory Head publication: %v", err)
	}
	secondArtifact := fixture.createMemoryArtifact(t, sessionID, strings.Repeat("c", 64), now.Add(time.Second))
	invalidRevision := persistence.AgentMemoryRevision{
		ID: uuid.New(), TenantID: fixture.tenantID, MemoryHeadID: memoryHead.ID, RevisionNumber: 2,
		ArtifactID: secondArtifact.ID, SHA256: *secondArtifact.SHA256,
		MediaType: "text/markdown", SizeBytes: *secondArtifact.SizeBytes,
		CreatedBy: uuid.New(), CreatedAt: now.Add(time.Second),
	}
	assertSQLiteStage4Rejected(t, fixture.db.Create(&invalidRevision).Error, "creator must reference an existing User")
	malformedRevision := persistence.AgentMemoryRevision{
		ID: uuid.New(), TenantID: fixture.tenantID, MemoryHeadID: memoryHead.ID, RevisionNumber: 2,
		ArtifactID: secondArtifact.ID, SHA256: *secondArtifact.SHA256,
		MediaType: "image/png", SizeBytes: *secondArtifact.SizeBytes,
		CreatedBy: fixture.userID, CreatedAt: now.Add(2 * time.Second),
	}
	assertSQLiteStage4Rejected(t, fixture.db.Create(&malformedRevision).Error, "Memory Revision shape is invalid")
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.Artifact{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, memoryArtifact.ID).
			Update("content_type", "application/json").Error,
		"pinned by an immutable Agent Memory Revision",
	)
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.Artifact{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, memoryArtifact.ID).
			Update("size_bytes", *memoryArtifact.SizeBytes+1).Error,
		"pinned by an immutable Agent Memory Revision",
	)
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.Artifact{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, memoryArtifact.ID).
			Update("object_key", "memory/rekeyed").Error,
		"pinned by an immutable Agent Memory Revision",
	)
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Delete(&persistence.Artifact{}, "tenant_id = ? AND id = ?", fixture.tenantID, memoryArtifact.ID).Error,
		"pinned by an immutable Agent Memory Revision",
	)

	validTenantPolicy := persistence.TenantResourceLifecyclePolicy{
		TenantID: fixture.tenantID, WaitingKeepAliveSeconds: intPointer(900), Version: 1, UpdatedBy: fixture.userID,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := fixture.db.Create(&validTenantPolicy).Error; err != nil {
		t.Fatalf("SQLite rejected valid Tenant Resource Lifecycle Policy: %v", err)
	}
	validProjectPolicy := persistence.ProjectResourceLifecyclePolicy{
		ProjectID: fixture.projectID, TenantID: fixture.tenantID, SuspendAfterIdleSeconds: intPointer(1800),
		Version: 1, UpdatedBy: fixture.userID, CreatedAt: now, UpdatedAt: now,
	}
	if err := fixture.db.Create(&validProjectPolicy).Error; err != nil {
		t.Fatalf("SQLite rejected valid Project Resource Lifecycle Policy: %v", err)
	}
	invalidTenantPolicy := persistence.TenantResourceLifecyclePolicy{
		TenantID: fixture.otherTenantID, WaitingKeepAliveSeconds: intPointer(900), Version: 1, UpdatedBy: uuid.New(),
		CreatedAt: now, UpdatedAt: now,
	}
	assertSQLiteStage4Rejected(t, fixture.db.Create(&invalidTenantPolicy).Error, "updater must reference an existing User")
	invalidProjectPolicy := persistence.ProjectResourceLifecyclePolicy{
		ProjectID: fixture.otherProjectID, TenantID: fixture.tenantID, SuspendAfterIdleSeconds: intPointer(1800),
		Version: 1, UpdatedBy: fixture.userID, CreatedAt: now, UpdatedAt: now,
	}
	assertSQLiteStage4Rejected(t, fixture.db.Create(&invalidProjectPolicy).Error, "existing Tenant Project")

	invalidSessionTimestamp := persistence.AgentSession{
		ID: uuid.New(), TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, ProjectID: fixture.projectID,
		CreatedBy: fixture.userID, Title: "Invalid idle since", Status: "active", Visibility: "private",
		Provider: "codex", ExecutionTargetID: fixture.targetID, ProviderResumeCursorState: "absent",
		ResourceState: "active", MeaningfulActivityAt: now, ResourceIdleSince: timePointer(now.Add(-time.Minute)),
		WaitingKeepAliveSeconds: 900, SuspendAfterIdleSeconds: 1800, WorkspaceRetentionDays: 30, WarmPoolMode: "disabled",
		CreatedAt: now, UpdatedAt: now,
	}
	assertSQLiteStage4Rejected(t, fixture.db.Create(&invalidSessionTimestamp).Error, "Session Resource Lifecycle fields are invalid")
	invalidSessionRange := persistence.AgentSession{
		ID: uuid.New(), TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, ProjectID: fixture.projectID,
		CreatedBy: fixture.userID, Title: "Invalid keepalive", Status: "active", Visibility: "private",
		Provider: "codex", ExecutionTargetID: fixture.targetID, ProviderResumeCursorState: "absent",
		ResourceState: "idle", MeaningfulActivityAt: now, WaitingKeepAliveSeconds: 30, SuspendAfterIdleSeconds: 1800,
		WorkspaceRetentionDays: 30, WarmPoolMode: "disabled", CreatedAt: now, UpdatedAt: now,
	}
	assertSQLiteStage4Rejected(t, fixture.db.Create(&invalidSessionRange).Error, "Session Resource Lifecycle fields are invalid")
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Exec(`UPDATE agent_sessions SET resource_state = NULL WHERE tenant_id = ? AND id = ?`, fixture.tenantID, sessionID).Error,
		"Session Resource Lifecycle fields are invalid",
	)

	suspendedSessionID := fixture.createSession(t, fixture.tenantID, fixture.organizationID, fixture.projectID, "Suspended execution", now.Add(time.Second))
	suspendedTurnID := fixture.createTurn(t, fixture.tenantID, suspendedSessionID, now.Add(time.Second))
	nextRecoveryReason := "suspend-resume"
	suspendedExecutionID := uuid.New()
	if err := fixture.db.Create(&persistence.AgentExecution{
		ID: suspendedExecutionID, TenantID: fixture.tenantID, SessionID: suspendedSessionID, TurnID: suspendedTurnID,
		Attempt: 1, Status: "suspended", ExecutionTargetID: fixture.targetID, TargetKind: "local",
		NextRecoveryReason: &nextRecoveryReason, Generation: 1, RequestedBy: fixture.userID,
		QueuedAt: now.Add(time.Second), StartedAt: timePointer(now.Add(time.Second)),
	}).Error; err != nil {
		t.Fatalf("create suspended execution fixture: %v", err)
	}
	invalidSuspendedLease := persistence.WorkerLease{
		ExecutionID: suspendedExecutionID, TenantID: fixture.tenantID, WorkerID: workerID,
		WorkerIncarnation: 1, WorkerInstanceUID: workerInstanceUID, Generation: 1,
		LeaseTokenHash: []byte("suspended-lease"), AcquiredAt: now.Add(time.Second),
		HeartbeatAt: now.Add(time.Second), ExpiresAt: now.Add(2 * time.Minute),
	}
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Create(&invalidSuspendedLease).Error,
		"worker lease does not match the current execution generation",
	)

	invalidSuspendAttempt := persistence.ExecutionSuspendAttempt{
		ID: uuid.New(), TenantID: fixture.tenantID, SessionID: sessionID, TurnID: turnID, ExecutionID: executionID,
		WorkerID: workerID, Generation: 2, Reason: "waiting-keepalive", Status: "checkpointing",
		RequestedAt: now, CheckpointDeadlineAt: now.Add(2 * time.Minute),
	}
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Create(&invalidSuspendAttempt).Error,
		"current Tenant Execution generation and Worker owner",
	)
	validSuspendAttempt := persistence.ExecutionSuspendAttempt{
		ID: uuid.New(), TenantID: fixture.tenantID, SessionID: sessionID, TurnID: turnID, ExecutionID: executionID,
		WorkerID: workerID, Generation: 1, Reason: "waiting-keepalive", Status: "checkpointing",
		RequestedAt: now, CheckpointDeadlineAt: now.Add(2 * time.Minute),
	}
	if err := fixture.db.Create(&validSuspendAttempt).Error; err != nil {
		t.Fatalf("SQLite rejected valid Execution suspend attempt: %v", err)
	}
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.ExecutionSuspendAttempt{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, validSuspendAttempt.ID).
			Updates(map[string]any{"status": "completed", "completed_at": now.Add(time.Minute)}).Error,
		"invalid Execution suspend attempt shape",
	)
	providerQuiescedAt := now.Add(30 * time.Second)
	if err := fixture.db.Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, validSuspendAttempt.ID).
		Update("provider_quiesced_at", providerQuiescedAt).Error; err != nil {
		t.Fatalf("SQLite rejected valid suspend quiesce provenance: %v", err)
	}
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.ExecutionSuspendAttempt{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, validSuspendAttempt.ID).
			Update("provider_quiesced_at", providerQuiescedAt.Add(time.Second)).Error,
		"invalid Execution suspend attempt shape",
	)
	if err := fixture.db.Model(&persistence.ExecutionSuspendAttempt{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, validSuspendAttempt.ID).
		Updates(map[string]any{"status": "completed", "completed_at": now.Add(time.Minute)}).Error; err != nil {
		t.Fatalf("SQLite rejected valid suspend completion after durable Provider quiesce: %v", err)
	}
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.ExecutionSuspendAttempt{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, validSuspendAttempt.ID).
			Update("provider_quiesced_at", providerQuiescedAt.Add(2*time.Second)).Error,
		"Finished Execution suspend attempts are immutable",
	)
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.ExecutionSuspendAttempt{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, validSuspendAttempt.ID).
			Update("failure_message", "rewritten after completion").Error,
		"Finished Execution suspend attempts are immutable",
	)
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.ExecutionSuspendAttempt{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, validSuspendAttempt.ID).
			Update("checkpoint_deadline_at", validSuspendAttempt.CheckpointDeadlineAt.Add(time.Minute)).Error,
		"invalid Execution suspend attempt shape",
	)

	resolvedAt := now.Add(3 * time.Second)
	resumeRecorded := fixture.createResumeRecordedInteraction(t, sessionID, turnID, executionID, workerID, 1, "approval-stage4", resolvedAt)
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.ExecutionInteraction{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, resumeRecorded.ID).
			Updates(map[string]any{
				"delivery_status":   "resume-bound",
				"resume_bundle_id":  nil,
				"resume_generation": nil,
				"resume_bound_at":   nil,
			}).Error,
		"invalid Interaction delivery shape",
	)

	otherSessionID := fixture.createSession(t, fixture.tenantID, fixture.organizationID, fixture.projectID, "Cross-bundle execution", now.Add(4*time.Second))
	otherTurnID := fixture.createTurn(t, fixture.tenantID, otherSessionID, now.Add(4*time.Second))
	otherExecutionID := fixture.createExecution(t, otherSessionID, otherTurnID, workerID, 1, "waiting-for-approval", now.Add(4*time.Second))
	otherBundle := persistence.ExecutionRecoveryBundle{
		ID: uuid.New(), TenantID: fixture.tenantID, SessionID: otherSessionID, TurnID: otherTurnID, ExecutionID: otherExecutionID,
		Generation: 1, SchemaVersion: 1, RecoveryReason: "initial-claim", AuthoritativeHistorySequence: 0,
		Payload: map[string]any{"schemaVersion": 1}, PayloadSHA256: strings.Repeat("f", 64), CreatedAt: now.Add(4 * time.Second),
	}
	if err := fixture.db.Create(&otherBundle).Error; err != nil {
		t.Fatalf("SQLite rejected valid secondary Recovery Bundle: %v", err)
	}

	crossBundleInteraction := fixture.createResumeRecordedInteraction(t, sessionID, turnID, executionID, workerID, 1, "approval-cross-bundle", resolvedAt.Add(time.Second))
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.ExecutionInteraction{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, crossBundleInteraction.ID).
			Updates(map[string]any{
				"delivery_status":   "resume-bound",
				"resume_bundle_id":  otherBundle.ID,
				"resume_generation": otherBundle.Generation,
				"resume_bound_at":   resolvedAt.Add(2 * time.Second),
			}).Error,
		"exact Execution generation Bundle",
	)

	if err := fixture.db.Model(&persistence.ExecutionInteraction{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, resumeRecorded.ID).
		Updates(map[string]any{
			"delivery_status":   "resume-bound",
			"resume_bundle_id":  initialBundle.ID,
			"resume_generation": initialBundle.Generation,
			"resume_bound_at":   resolvedAt.Add(2 * time.Second),
		}).Error; err != nil {
		t.Fatalf("SQLite rejected valid resume-bound Interaction: %v", err)
	}
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.ExecutionInteraction{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, resumeRecorded.ID).
			Updates(map[string]any{
				"resume_bundle_id":  otherBundle.ID,
				"resume_generation": otherBundle.Generation,
			}).Error,
		"cannot be rebound or replayed",
	)
	if err := fixture.db.Model(&persistence.ExecutionInteraction{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, resumeRecorded.ID).
		Updates(map[string]any{
			"delivery_status": "outcome-unknown",
			"delivery_error":  "resume outcome could not be proven exactly once",
		}).Error; err != nil {
		t.Fatalf("SQLite rejected valid outcome-unknown Interaction transition: %v", err)
	}
	assertSQLiteStage4Rejected(
		t,
		fixture.db.Model(&persistence.ExecutionInteraction{}).
			Where("tenant_id = ? AND id = ?", fixture.tenantID, resumeRecorded.ID).
			Update("delivery_status", "resume-bound").Error,
		"Outcome-unknown Interaction resolution is terminal",
	)
}

func openStage4SQLiteFixture(t *testing.T) stage4SQLiteFixture {
	t.Helper()
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "sqlite-stage4-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	projectID := uuid.New()
	if err := store.DB().Create(&persistence.Project{
		ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		Name: "SQLite Stage 4", DefaultBranch: "main", Visibility: "private",
		CreatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	otherTenantID := uuid.New()
	otherOrganizationID := uuid.New()
	otherProjectID := uuid.New()
	if err := store.DB().Create(&persistence.Tenant{
		ID: otherTenantID, Slug: "sqlite-stage4-other", Name: "SQLite Stage 4 Other", Status: "active",
		PlanCode: "personal", Region: "local", Settings: map[string]any{}, CreatedBy: domain.UserID,
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.Organization{
		ID: otherOrganizationID, TenantID: otherTenantID, Slug: "other", Name: "Other",
		Kind: "root", Status: "active", Settings: map[string]any{}, CreatedBy: domain.UserID,
		CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.Project{
		ID: otherProjectID, TenantID: otherTenantID, OrganizationID: otherOrganizationID,
		Name: "SQLite Stage 4 Other", DefaultBranch: "main", Visibility: "private",
		CreatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	return stage4SQLiteFixture{
		db: store.DB(), tenantID: domain.TenantID, organizationID: domain.OrganizationID,
		projectID: projectID, userID: domain.UserID, targetID: domain.ExecutionTargetID,
		otherTenantID: otherTenantID, otherOrganizationID: otherOrganizationID, otherProjectID: otherProjectID,
	}
}

func (fixture stage4SQLiteFixture) createSession(
	t *testing.T,
	tenantID, organizationID, projectID uuid.UUID,
	title string,
	now time.Time,
) uuid.UUID {
	t.Helper()
	sessionID := uuid.New()
	if err := fixture.db.Create(&persistence.AgentSession{
		ID: sessionID, TenantID: tenantID, OrganizationID: organizationID, ProjectID: projectID,
		CreatedBy: fixture.userID, Title: title, Status: "active", Visibility: "private",
		Provider: "codex", ExecutionTargetID: fixture.targetID, ProviderResumeCursorState: "absent",
		ResourceState: "idle", MeaningfulActivityAt: now, WaitingKeepAliveSeconds: 900, SuspendAfterIdleSeconds: 1800,
		WorkspaceRetentionDays: 30, WarmPoolMode: "disabled", CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	return sessionID
}

func (fixture stage4SQLiteFixture) createTurn(t *testing.T, tenantID, sessionID uuid.UUID, now time.Time) uuid.UUID {
	t.Helper()
	turnID := uuid.New()
	if err := fixture.db.Create(&persistence.AgentTurn{
		ID: turnID, TenantID: tenantID, SessionID: sessionID, CreatedBy: fixture.userID,
		Status: "queued", InputText: "hello", TurnKind: "message",
		RuntimeMode: "full-access", InteractionMode: "default", CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	return turnID
}

func (fixture stage4SQLiteFixture) createWorker(t *testing.T, podSuffix string, now time.Time) (uuid.UUID, string) {
	t.Helper()
	workerID := uuid.New()
	instanceUID := uuid.NewString()
	if err := fixture.db.Create(&persistence.WorkerInstance{
		ID: workerID, Incarnation: 1, InstanceUID: instanceUID, ExecutionTargetID: fixture.targetID,
		TargetKind: "local", ClusterID: "sqlite-stage4", Namespace: "default", PodName: podSuffix,
		Version: "stage4", ProtocolVersion: 1, Capabilities: map[string]any{}, LeaseSupported: true,
		FencingSupported: true, AuthTokenHash: []byte("token-" + workerID.String()), Status: "online",
		AdministrativeStatus: "active", RegisteredAt: now, LastHeartbeatAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	return workerID, instanceUID
}

func (fixture stage4SQLiteFixture) createExecution(
	t *testing.T,
	sessionID, turnID, workerID uuid.UUID,
	generation int64,
	status string,
	now time.Time,
) uuid.UUID {
	t.Helper()
	executionID := uuid.New()
	if err := fixture.db.Create(&persistence.AgentExecution{
		ID: executionID, TenantID: fixture.tenantID, SessionID: sessionID, TurnID: turnID,
		Attempt: 1, Status: status, ExecutionTargetID: fixture.targetID, TargetKind: "local",
		WorkerID: &workerID, Generation: generation, RequestedBy: fixture.userID, QueuedAt: now, StartedAt: &now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	return executionID
}

func (fixture stage4SQLiteFixture) createMemoryArtifact(
	t *testing.T,
	sessionID uuid.UUID,
	sha string,
	now time.Time,
) persistence.Artifact {
	t.Helper()
	contentType := "text/markdown; charset=utf-8"
	sizeBytes := int64(16)
	artifact := persistence.Artifact{
		ID: uuid.New(), TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, ProjectID: fixture.projectID,
		SessionID: sessionID, Kind: "memory", Status: "ready", Bucket: "sqlite-stage4",
		ObjectKey: "memory/" + uuid.NewString(), ContentType: &contentType, SizeBytes: &sizeBytes, SHA256: &sha,
		CreatedByType: "user", CreatedByID: fixture.userID, ReadyAt: &now, CreatedAt: now,
	}
	if err := fixture.db.Create(&artifact).Error; err != nil {
		t.Fatal(err)
	}
	return artifact
}

func (fixture stage4SQLiteFixture) createResumeRecordedInteraction(
	t *testing.T,
	sessionID, turnID, executionID, workerID uuid.UUID,
	generation int64,
	requestID string,
	now time.Time,
) persistence.ExecutionInteraction {
	t.Helper()
	resolutionKind := "approved"
	resolutionCommandID := requestID + ":resolution"
	interaction := persistence.ExecutionInteraction{
		ID: uuid.New(), TenantID: fixture.tenantID, ExecutionID: executionID, SessionID: sessionID, TurnID: turnID,
		WorkerID: workerID, Generation: generation, Provider: "codex", RequestID: requestID, EventVersion: 1,
		Kind: "approval", Status: "resolved", Payload: map[string]any{"summary": "Run"},
		Resolution: map[string]any{"decision": "accept"}, RequestedAt: now.Add(-time.Minute), ExpiresAt: now.Add(23 * time.Hour),
		ResolvedAt: &now, ResolvedBy: &fixture.userID, ResolutionKind: &resolutionKind,
		ResolutionCommandID: &resolutionCommandID, DeliveryStatus: "resume-recorded", DeliveryAvailableAt: &now,
	}
	if err := fixture.db.Create(&interaction).Error; err != nil {
		t.Fatal(err)
	}
	return interaction
}

func assertSQLiteStage4Rejected(t *testing.T, err error, expected string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), expected) {
		t.Fatalf("SQLite accepted invalid Stage 4 state or returned wrong error: %v (want containing %q)", err, expected)
	}
}

func intPointer(value int) *int { return &value }

func timePointer(value time.Time) *time.Time { return &value }
