package retention

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/synara-ai/synara/services/control-plane/internal/artifacts"
	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/config"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/projects"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestRetentionArchivesInactiveSessionsAndDeletesArtifactsIdempotently(t *testing.T) {
	ctx := context.Background()
	platformConfig, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, platformConfig, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "retention-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	targetService := executiontargets.NewService(store.DB(), platformConfig, nil)
	sessionService := sessions.NewService(store.DB(), projects.NewService(store.DB()), targetService)
	objectStore, err := artifacts.NewLocalStore(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	retentionConfig := config.Config{
		ArtifactPresignTTL: 15 * time.Minute, ArtifactMaxUploadBytes: 1 << 20,
		WorkerLeaseTTL: time.Minute, WorkerHeartbeatTimeout: 2 * time.Minute, WorkerReceiptTTL: time.Hour,
	}
	executionService := executions.NewService(
		store.DB(), sessionService, retentionConfig.WorkerLeaseTTL, retentionConfig.WorkerHeartbeatTimeout,
		retentionConfig.WorkerReceiptTTL, nil, targetService,
	)
	artifactService := artifacts.NewService(store.DB(), objectStore, retentionConfig, executionService, sessionService)
	service := NewService(store.DB(), sessionService, artifactService, executionService, time.Hour, slog.Default())
	now := time.Now().UTC().Truncate(time.Second)
	service.now = func() time.Time { return now }
	expiredSessionID := uuid.New()
	if err := store.DB().Create(&persistence.LoginSession{
		ID: expiredSessionID, UserID: domain.UserID, ActiveTenantID: &domain.TenantID,
		RefreshTokenHash: []byte("expired-login-session"), CreatedAt: now.Add(-62 * 24 * time.Hour),
		ExpiresAt: now.Add(-31 * 24 * time.Hour), LastSeenAt: now.Add(-31 * 24 * time.Hour),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.WorkerRequestReceipt{
		WorkerID: uuid.New(), RequestID: "expired-receipt", Operation: "claim", RequestHash: "hash",
		StatusCode: 200, Response: map[string]any{}, CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(-time.Minute),
	}).Error; err != nil {
		t.Fatal(err)
	}

	projectID, sessionID, artifactID := uuid.New(), uuid.New(), uuid.New()
	old := now.Add(-48 * time.Hour)
	if err := store.DB().Create(&persistence.Project{
		ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		Name: "Retention project", DefaultBranch: "main", Visibility: "organization", CreatedBy: domain.UserID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.AgentSession{
		ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		ProjectID: projectID, CreatedBy: domain.UserID, Title: "Old session", Status: "active",
		Visibility: "private", Provider: "codex", ExecutionTargetID: domain.ExecutionTargetID,
		CreatedAt: old, UpdatedAt: old,
	}).Error; err != nil {
		t.Fatal(err)
	}
	payload := "retention artifact"
	digest := sha256.Sum256([]byte(payload))
	objectKey := domain.TenantID.String() + "/retention.txt"
	if _, err := objectStore.Put(ctx, objectKey, strings.NewReader(payload), int64(len(payload)), "text/plain"); err != nil {
		t.Fatal(err)
	}
	size := int64(len(payload))
	contentType := "text/plain"
	sha := hex.EncodeToString(digest[:])
	if err := store.DB().Create(&persistence.Artifact{
		ID: artifactID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		ProjectID: projectID, SessionID: sessionID, Kind: "generated_file", Status: "ready",
		Bucket: objectStore.Bucket(), ObjectKey: objectKey, ContentType: &contentType,
		SizeBytes: &size, SHA256: &sha, CreatedByType: "user", CreatedByID: domain.UserID,
		ReadyAt: &old, CreatedAt: old,
	}).Error; err != nil {
		t.Fatal(err)
	}
	turnID, executionID, workspaceID, checkpointID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	finishedAt := old
	if err := store.DB().Create(&persistence.AgentTurn{
		ID: turnID, TenantID: domain.TenantID, SessionID: sessionID, CreatedBy: domain.UserID,
		Status: "completed", InputText: "retained checkpoint", CreatedAt: old, CompletedAt: &finishedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.AgentExecution{
		ID: executionID, TenantID: domain.TenantID, SessionID: sessionID, TurnID: turnID,
		Attempt: 1, Status: "completed", ExecutionTargetID: domain.ExecutionTargetID, TargetKind: "local",
		Generation: 1, RequestedBy: domain.UserID, QueuedAt: old, StartedAt: &old, FinishedAt: &finishedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.RemoteWorkspace{
		ID: workspaceID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		ProjectID: projectID, SessionID: sessionID, ExecutionTargetID: domain.ExecutionTargetID,
		WorkspaceMode: "clone", State: "ready", DefaultBranch: "main", CreatedAt: old, UpdatedAt: old,
	}).Error; err != nil {
		t.Fatal(err)
	}
	materializationID := uuid.New()
	incarnationID := uuid.New()
	if err := store.DB().Create(&persistence.WorkspaceMaterialization{
		ID: materializationID, TenantID: domain.TenantID, WorkspaceID: workspaceID,
		OrganizationID: domain.OrganizationID, ProjectID: projectID, SessionID: sessionID,
		ExecutionTargetID: domain.ExecutionTargetID, TargetKind: "local", StorageScope: "target",
		LayoutVersion: 3, IncarnationID: incarnationID, State: "active", CreatedAt: old, UpdatedAt: old,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Model(&persistence.RemoteWorkspace{}).Where("id = ?", workspaceID).
		Update("current_materialization_id", materializationID).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Model(&persistence.AgentExecution{}).Where("id = ?", executionID).
		Updates(map[string]any{
			"remote_workspace_id": workspaceID, "workspace_materialization_id": materializationID,
		}).Error; err != nil {
		t.Fatal(err)
	}
	baseCommit := strings.Repeat("a", 40)
	headCommit := strings.Repeat("b", 40)
	branch := "main"
	fileCount := 1
	checkpointBytes := int64(len(payload))
	checkpointSHA := sha
	if err := store.DB().Create(&persistence.WorkspaceCheckpoint{
		ID: checkpointID, TenantID: domain.TenantID, WorkspaceID: workspaceID, SessionID: sessionID,
		TurnID: &turnID, ExecutionID: executionID, Generation: 1, IdempotencyKey: "retention-checkpoint",
		Strategy: "snapshot", Status: "pending", BaseCommit: &baseCommit, HeadCommit: &headCommit,
		CurrentBranch: &branch, Manifest: map[string]any{"format": "synara-workspace-snapshot-v1"},
		FileCount: &fileCount, TotalBytes: &checkpointBytes, CreatedAt: old,
	}).Error; err != nil {
		t.Fatal(err)
	}
	checkpointObjectKey := domain.TenantID.String() + "/retention-checkpoint.tar"
	if _, err := objectStore.Put(ctx, checkpointObjectKey, strings.NewReader(payload), int64(len(payload)), "application/x-tar"); err != nil {
		t.Fatal(err)
	}
	checkpointContentType := "application/x-tar"
	if err := store.DB().Create(&persistence.Artifact{
		ID: checkpointID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		ProjectID: projectID, SessionID: sessionID, ExecutionID: &executionID,
		WorkspaceCheckpointID: &checkpointID, Kind: "workspace_snapshot", Status: "ready",
		Bucket: objectStore.Bucket(), ObjectKey: checkpointObjectKey, ContentType: &checkpointContentType,
		SizeBytes: &checkpointBytes, SHA256: &checkpointSHA, CreatedByType: "worker", CreatedByID: domain.UserID,
		ReadyAt: &old, CreatedAt: old,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Model(&persistence.WorkspaceCheckpoint{}).Where("id = ?", checkpointID).
		Updates(map[string]any{"status": "ready", "artifact_id": checkpointID, "sha256": checkpointSHA, "ready_at": old}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Model(&persistence.AgentExecution{}).Where("id = ?", executionID).
		Update("restore_checkpoint_id", checkpointID).Error; err != nil {
		t.Fatal(err)
	}
	currentCheckpointID := uuid.New()
	if err := store.DB().Create(&persistence.WorkspaceCheckpoint{
		ID: currentCheckpointID, TenantID: domain.TenantID, WorkspaceID: workspaceID, SessionID: sessionID,
		TurnID: &turnID, ExecutionID: executionID, Generation: 1, IdempotencyKey: "current-retention-checkpoint",
		Strategy: "snapshot", Status: "pending", BaseCommit: &baseCommit, HeadCommit: &headCommit,
		CurrentBranch: &branch, Manifest: map[string]any{"format": "synara-workspace-snapshot-v1"},
		FileCount: &fileCount, TotalBytes: &checkpointBytes, CreatedAt: old,
	}).Error; err != nil {
		t.Fatal(err)
	}
	currentObjectKey := domain.TenantID.String() + "/current-retention-checkpoint.tar"
	if _, err := objectStore.Put(ctx, currentObjectKey, strings.NewReader(payload), int64(len(payload)), "application/x-tar"); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.Artifact{
		ID: currentCheckpointID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
		ProjectID: projectID, SessionID: sessionID, ExecutionID: &executionID,
		WorkspaceCheckpointID: &currentCheckpointID, Kind: "workspace_snapshot", Status: "ready",
		Bucket: objectStore.Bucket(), ObjectKey: currentObjectKey, ContentType: &checkpointContentType,
		SizeBytes: &checkpointBytes, SHA256: &checkpointSHA, CreatedByType: "worker", CreatedByID: domain.UserID,
		ReadyAt: &old, CreatedAt: old,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Model(&persistence.WorkspaceCheckpoint{}).Where("id = ?", currentCheckpointID).
		Updates(map[string]any{"status": "ready", "artifact_id": currentCheckpointID, "sha256": checkpointSHA, "ready_at": old}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Model(&persistence.RemoteWorkspace{}).Where("id = ?", workspaceID).
		Update("current_checkpoint_id", currentCheckpointID).Error; err != nil {
		t.Fatal(err)
	}
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	days := 1
	if _, err := service.Update(ctx, principal, domain.TenantID, UpdateInput{
		SessionArchiveAfterDays: &days, ArtifactDeleteAfterDays: &days, WorkspaceCleanupAfterDays: &days,
	}, "retention-update", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := service.RunOnce(ctx, 100); err != nil {
		t.Fatal(err)
	}
	var expiredLoginSessions, expiredReceipts int64
	if err := store.DB().Model(&persistence.LoginSession{}).Where("id = ?", expiredSessionID).Count(&expiredLoginSessions).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Model(&persistence.WorkerRequestReceipt{}).Where("request_id = ?", "expired-receipt").Count(&expiredReceipts).Error; err != nil {
		t.Fatal(err)
	}
	if expiredLoginSessions != 0 || expiredReceipts != 0 {
		t.Fatalf("retention left expired ephemeral records: login=%d receipts=%d", expiredLoginSessions, expiredReceipts)
	}
	var archived persistence.AgentSession
	if err := store.DB().Where("id = ?", sessionID).Take(&archived).Error; err != nil {
		t.Fatal(err)
	}
	if archived.Status != "archived" || archived.ArchivedAt == nil {
		t.Fatalf("retention did not archive the Session: %#v", archived)
	}
	var retainedWorkspace persistence.RemoteWorkspace
	if err := store.DB().Where("id = ?", workspaceID).Take(&retainedWorkspace).Error; err != nil {
		t.Fatal(err)
	}
	if retainedWorkspace.RetentionUntil == nil ||
		retainedWorkspace.RetentionUntil.Before(now.Add(24*time.Hour-time.Minute)) ||
		retainedWorkspace.RetentionUntil.After(now.Add(24*time.Hour+time.Minute)) {
		t.Fatalf("retention did not set the Workspace cleanup deadline: %#v", retainedWorkspace)
	}
	var retainedMaterialization persistence.WorkspaceMaterialization
	if err := store.DB().Where("id = ?", materializationID).Take(&retainedMaterialization).Error; err != nil {
		t.Fatal(err)
	}
	if retainedMaterialization.CleanupReason == nil || *retainedMaterialization.CleanupReason != "retention-session-archive" ||
		retainedMaterialization.CleanupRequestedAt == nil ||
		!retainedMaterialization.CleanupRequestedAt.Equal(*retainedWorkspace.RetentionUntil) {
		t.Fatalf("retention did not persist the current materialization cleanup intent: %#v", retainedMaterialization)
	}
	var deleted persistence.Artifact
	if err := store.DB().Where("id = ?", artifactID).Take(&deleted).Error; err != nil {
		t.Fatal(err)
	}
	if deleted.Status != "deleted" || deleted.DeletedAt == nil {
		t.Fatalf("retention did not delete the Artifact: %#v", deleted)
	}
	var retainedExecution persistence.AgentExecution
	if err := store.DB().Where("id = ?", executionID).Take(&retainedExecution).Error; err != nil {
		t.Fatal(err)
	}
	if retainedExecution.RestoreCheckpointID != nil {
		t.Fatalf("retention kept a terminal Execution restore reference: %#v", retainedExecution.RestoreCheckpointID)
	}
	var expiredCheckpoint persistence.WorkspaceCheckpoint
	if err := store.DB().Where("id = ?", checkpointID).Take(&expiredCheckpoint).Error; err != nil {
		t.Fatal(err)
	}
	if expiredCheckpoint.Status != "expired" {
		t.Fatalf("retention did not expire the unreferenced Checkpoint: %#v", expiredCheckpoint)
	}
	var deletedCheckpointArtifact persistence.Artifact
	if err := store.DB().Where("id = ?", checkpointID).Take(&deletedCheckpointArtifact).Error; err != nil {
		t.Fatal(err)
	}
	if deletedCheckpointArtifact.Status != "deleted" || deletedCheckpointArtifact.DeletedAt == nil {
		t.Fatalf("retention did not delete the expired Checkpoint Artifact: %#v", deletedCheckpointArtifact)
	}
	var currentCheckpoint persistence.WorkspaceCheckpoint
	if err := store.DB().Where("id = ?", currentCheckpointID).Take(&currentCheckpoint).Error; err != nil {
		t.Fatal(err)
	}
	if currentCheckpoint.Status != "ready" {
		t.Fatalf("retention expired the current Workspace Checkpoint: %#v", currentCheckpoint)
	}
	var currentArtifact persistence.Artifact
	if err := store.DB().Where("id = ?", currentCheckpointID).Take(&currentArtifact).Error; err != nil {
		t.Fatal(err)
	}
	if currentArtifact.Status != "ready" || currentArtifact.DeletedAt != nil {
		t.Fatalf("retention deleted the current Workspace Checkpoint Artifact: %#v", currentArtifact)
	}
	if _, err := objectStore.Stat(ctx, currentObjectKey); err != nil {
		t.Fatalf("retention removed the current Workspace Checkpoint payload: %v", err)
	}
	if _, err := objectStore.Stat(ctx, objectKey); err == nil {
		t.Fatal("retention left the Artifact payload in object storage")
	}
	deletedAt := now.Add(time.Hour)
	if err := store.DB().Model(&persistence.Tenant{}).Where("id = ?", domain.TenantID).
		Updates(map[string]any{"status": "deleting", "deleted_at": deletedAt}).Error; err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now.Add(48 * time.Hour) }
	if err := service.RunOnce(ctx, 100); err != nil {
		t.Fatal(err)
	}
	var cleanupCommands int64
	if err := store.DB().Model(&persistence.WorkspaceCleanupCommand{}).
		Where("tenant_id = ? AND materialization_id = ?", domain.TenantID, materializationID).
		Count(&cleanupCommands).Error; err != nil {
		t.Fatal(err)
	}
	if cleanupCommands != 1 {
		t.Fatalf("global retention cleanup excluded a deleted Tenant: commands=%d", cleanupCommands)
	}
	var appliedAudits int64
	if err := store.DB().Model(&persistence.AuditLog{}).
		Where("tenant_id = ? AND action = ?", domain.TenantID, "retention_policy.applied").
		Count(&appliedAudits).Error; err != nil {
		t.Fatal(err)
	}
	if appliedAudits != 1 {
		t.Fatalf("idempotent retention produced %d summary audits", appliedAudits)
	}
}

func TestRetentionPolicyRejectsUnauthorizedAndInvalidUpdates(t *testing.T) {
	fixture := newPolicyFixture(t)
	invalid := 0
	_, err := fixture.service.Update(context.Background(), fixture.owner, fixture.tenantID,
		UpdateInput{SessionArchiveAfterDays: &invalid}, "invalid", "127.0.0.1")
	assertRetentionProblemCode(t, err, "invalid_session_retention")
	_, err = fixture.service.Update(context.Background(), fixture.owner, fixture.tenantID,
		UpdateInput{WorkspaceCleanupAfterDays: &invalid}, "invalid-workspace", "127.0.0.1")
	assertRetentionProblemCode(t, err, "invalid_workspace_retention")
	workspaceDays := 30
	policy, err := fixture.service.Update(context.Background(), fixture.owner, fixture.tenantID,
		UpdateInput{WorkspaceCleanupAfterDays: &workspaceDays}, "workspace-policy", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if policy.WorkspaceCleanupAfterDays == nil || *policy.WorkspaceCleanupAfterDays != workspaceDays {
		t.Fatalf("Workspace cleanup retention was not persisted: %#v", policy)
	}
	_, err = fixture.service.Update(context.Background(), fixture.member, fixture.tenantID,
		UpdateInput{}, "forbidden", "127.0.0.1")
	assertRetentionProblemCode(t, err, "tenant_forbidden")
}

type policyFixture struct {
	service  *Service
	tenantID uuid.UUID
	owner    identity.Principal
	member   identity.Principal
}

func newPolicyFixture(t *testing.T) policyFixture {
	t.Helper()
	ctx := context.Background()
	platformConfig, _ := platform.Defaults(platform.ProfilePersonal)
	store, err := database.OpenMetadataStore(ctx, platformConfig, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "retention-policy-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	memberID := uuid.New()
	now := time.Now().UTC()
	for _, model := range []any{
		&persistence.User{ID: memberID, Email: uuid.NewString() + "@example.com", DisplayName: "Member", Status: "active", EmailVerifiedAt: &now},
		&persistence.TenantMembership{TenantID: domain.TenantID, UserID: memberID, Role: "member", Status: "active", JoinedAt: &now},
	} {
		if err := store.DB().Create(model).Error; err != nil {
			t.Fatal(err)
		}
	}
	targets := executiontargets.NewService(store.DB(), platformConfig, nil)
	sessionService := sessions.NewService(store.DB(), projects.NewService(store.DB()), targets)
	localStore, _ := artifacts.NewLocalStore(filepath.Join(t.TempDir(), "artifacts"))
	artifactService := artifacts.NewService(store.DB(), localStore, config.Config{}, nil, sessionService)
	return policyFixture{
		service:  NewService(store.DB(), sessionService, artifactService, nil, time.Hour, slog.Default()),
		tenantID: domain.TenantID,
		owner:    identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID},
		member:   identity.Principal{UserID: memberID, ActiveTenantID: &domain.TenantID},
	}
}

func assertRetentionProblemCode(t *testing.T, err error, code string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != code {
		t.Fatalf("expected problem code %q, got %v", code, err)
	}
}
