package executions

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

func TestClaimBuildsAuthoritativeResumeSnapshotV1AndAdvancesBindingSequence(t *testing.T) {
	ctx := context.Background()
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "resume-snapshot-worker")
	cleanupWorkers(t, db, worker.ID)

	var session persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.SessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	workspaceID := uuid.New()
	checkpointID := uuid.New()
	artifactID := uuid.New()
	fingerprint := strings.Repeat("a", 64)
	baseCommit := strings.Repeat("b", 40)
	headCommit := strings.Repeat("c", 40)
	branch := "synara/resume-snapshot"
	contentType := "text/plain"
	sizeBytes := int64(18)
	sha256 := strings.Repeat("d", 64)
	workspace := persistence.RemoteWorkspace{
		ID: workspaceID, TenantID: fixture.TenantID, OrganizationID: session.OrganizationID,
		ProjectID: session.ProjectID, SessionID: fixture.SessionID, ExecutionTargetID: fixture.TargetID,
		WorkspaceMode: "clone", State: "recovering", RepositoryFingerprint: &fingerprint,
		DefaultBranch: "main", CurrentBranch: &branch, BaseCommit: &baseCommit, HeadCommit: &headCommit,
		CreatedAt: now, UpdatedAt: now,
	}
	checkpoint := persistence.WorkspaceCheckpoint{
		ID: checkpointID, TenantID: fixture.TenantID, WorkspaceID: workspaceID,
		SessionID: fixture.SessionID, TurnID: &fixture.TurnID, ExecutionID: fixture.ExecutionID,
		Generation: 1, IdempotencyKey: "resume-current-checkpoint", Strategy: "git-reference",
		Status: "ready", BaseCommit: &baseCommit, HeadCommit: &headCommit, CurrentBranch: &branch,
		Manifest:  map[string]any{"format": "synara-git-reference-v1", "headCommit": headCommit},
		CreatedAt: now, ReadyAt: &now,
	}
	artifact := persistence.Artifact{
		ID: artifactID, TenantID: fixture.TenantID, OrganizationID: session.OrganizationID,
		ProjectID: session.ProjectID, SessionID: fixture.SessionID, Kind: "generated_file", Status: "ready",
		Bucket: "resume-snapshot-tests", ObjectKey: "resume/" + artifactID.String(),
		ContentType: &contentType, SizeBytes: &sizeBytes, SHA256: &sha256,
		CreatedByType: "system", CreatedByID: fixture.UserID, ReadyAt: &now, CreatedAt: now,
	}
	pending := persistence.ExecutionInteraction{
		ID: uuid.New(), TenantID: fixture.TenantID, ExecutionID: fixture.ExecutionID,
		SessionID: fixture.SessionID, TurnID: fixture.TurnID, WorkerID: worker.ID, Generation: 1,
		Provider: "codex", RequestID: "approval-resume", EventVersion: RuntimeEventVersionV2,
		Kind: "approval", Status: "pending", Payload: map[string]any{
			"requestId": "approval-resume", "requestType": "exec_command_approval",
			"detail": "Run the focused verification", "secret": "must-not-leak",
		}, RequestedAt: now, ExpiresAt: now.Add(time.Hour), DeliveryStatus: "not-ready",
	}

	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&workspace).Error; err != nil {
			return err
		}
		if err := tx.Model(&persistence.AgentExecution{}).
			Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).
			Updates(map[string]any{
				"remote_workspace_id": workspaceID,
				"status":              "recovering",
				"generation":          1,
			}).Error; err != nil {
			return err
		}
		if err := tx.Create(&checkpoint).Error; err != nil {
			return err
		}
		if err := tx.Model(&persistence.RemoteWorkspace{}).
			Where("tenant_id = ? AND id = ?", fixture.TenantID, workspaceID).
			Update("current_checkpoint_id", checkpointID).Error; err != nil {
			return err
		}
		if err := tx.Create(&artifact).Error; err != nil {
			return err
		}
		if err := tx.Create(&pending).Error; err != nil {
			return err
		}
		for _, input := range []sessions.InternalEventInput{
			{EventType: "turn.created", ActorType: "user", Payload: map[string]any{"inputText": "Earlier question"}},
			{EventVersion: RuntimeEventVersionV1, EventType: "runtime.output.delta", ActorType: "worker", Payload: map[string]any{"text": "Legacy "}},
			{EventVersion: RuntimeEventVersionV2, EventType: "content.delta", ActorType: "worker", Payload: map[string]any{"streamKind": "assistant_text", "delta": "and v2"}},
			{EventVersion: RuntimeEventVersionV2, EventType: "tool.summary", ActorType: "worker", Payload: map[string]any{"summary": "Focused tests passed"}},
			{EventVersion: RuntimeEventVersionV2, EventType: "item.completed", ActorType: "worker", Payload: map[string]any{"itemType": "review_entered", "status": "completed"}},
			{EventType: "artifact.ready", ActorType: "system", Payload: map[string]any{"artifactId": artifactID, "kind": "generated_file"}},
			{EventType: "turn.created", ActorType: "user", ExecutionID: &fixture.ExecutionID, Payload: map[string]any{
				"inputText": "Continue from the verified state", "runtimeMode": "approval-required", "interactionMode": "plan",
			}},
		} {
			if _, err := service.sessions.AppendInternalEvent(ctx, tx, fixture.TenantID, fixture.SessionID, input); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	claim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID,
		TargetKind:        fixture.TargetKind,
		ExecutionID:       &fixture.ExecutionID,
	}, "resume-snapshot-claim")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Value.Workload == nil || claim.Value.Workload.ResumeSnapshot == nil {
		t.Fatalf("claim omitted Resume Snapshot: %#v", claim.Value)
	}
	workload := claim.Value.Workload
	snapshot := workload.ResumeSnapshot
	if snapshot.Version != ResumeSnapshotVersionV1 || snapshot.SessionID != fixture.SessionID ||
		snapshot.TurnID != fixture.TurnID || snapshot.Provider != "codex" {
		t.Fatalf("unexpected Resume Snapshot identity: %#v", snapshot)
	}
	if snapshot.SourceSequenceRange != (ResumeSequenceRange{From: 1, Through: 6}) ||
		snapshot.AuthoritativeHistorySequence != 6 {
		t.Fatalf("unexpected authoritative sequence range: %#v", snapshot)
	}
	if len(snapshot.Messages) != 2 || snapshot.Messages[0].Text != "Earlier question" ||
		snapshot.Messages[1].Text != "Legacy and v2" || len(workload.ConversationHistory) != 2 ||
		workload.ConversationHistory[1].Text != snapshot.Messages[1].Text {
		t.Fatalf("legacy/v2 history projection diverged: snapshot=%#v legacy=%#v", snapshot.Messages, workload.ConversationHistory)
	}
	if !snapshot.Mode.Plan || !snapshot.Mode.Review || snapshot.Mode.ReviewSequence == nil ||
		*snapshot.Mode.ReviewSequence != 5 || snapshot.Mode.RuntimeMode != "approval-required" ||
		snapshot.Mode.InteractionMode != "plan" {
		t.Fatalf("Resume Snapshot omitted plan/review mode: %#v", snapshot.Mode)
	}
	if len(snapshot.ToolResults) != 1 || snapshot.ToolResults[0].Summary != "Focused tests passed" ||
		len(snapshot.ArtifactReferences) != 1 || snapshot.ArtifactReferences[0].ArtifactID != artifactID ||
		snapshot.ArtifactReferences[0].SHA256 == nil || *snapshot.ArtifactReferences[0].SHA256 != sha256 {
		t.Fatalf("Resume Snapshot omitted safe result references: tools=%#v artifacts=%#v", snapshot.ToolResults, snapshot.ArtifactReferences)
	}
	if len(snapshot.PendingInteractions) != 1 || snapshot.PendingInteractions[0].RequestID != "approval-resume" ||
		snapshot.PendingInteractions[0].Detail != "Run the focused verification" {
		t.Fatalf("Resume Snapshot omitted pending interaction: %#v", snapshot.PendingInteractions)
	}
	encoded, err := json.Marshal(snapshot.PendingInteractions)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "must-not-leak") {
		t.Fatalf("Resume Snapshot leaked non-allowlisted interaction data: %s", encoded)
	}
	if workload.RestoreCheckpoint != nil || snapshot.Workspace == nil || snapshot.Workspace.Checkpoint == nil ||
		snapshot.Workspace.Checkpoint.CheckpointID != checkpointID ||
		snapshot.Workspace.RepositoryFingerprint == nil || *snapshot.Workspace.RepositoryFingerprint != fingerprint ||
		snapshot.Workspace.HeadCommit == nil || *snapshot.Workspace.HeadCommit != headCommit {
		t.Fatalf("Resume Snapshot did not reference the current ready Workspace Checkpoint: workload=%#v snapshot=%#v", workload.RestoreCheckpoint, snapshot.Workspace)
	}
	if snapshot.Budget.UsedBytes <= 0 || snapshot.Budget.UsedBytes > snapshot.Budget.ByteLimit ||
		snapshot.Budget.EstimatedTokens > snapshot.Budget.TokenLimit {
		t.Fatalf("Resume Snapshot budget is invalid: %#v", snapshot.Budget)
	}

	var binding persistence.ProviderRuntimeBinding
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, *workload.ProviderRuntimeBindingID).
		Take(&binding).Error; err != nil {
		t.Fatal(err)
	}
	if binding.AuthoritativeHistorySequence != 6 {
		t.Fatalf("authoritative_history_sequence = %d, want 6", binding.AuthoritativeHistorySequence)
	}
}

func TestResumeSnapshotStateMarkersSurviveEventProjectionLimit(t *testing.T) {
	ctx := context.Background()
	db := integrationDB(t)
	fixture := seedExecutionFixture(t, db)
	service := integrationService(t, db)
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "resume-marker-worker")
	cleanupWorkers(t, db, worker.ID)

	if err := db.Transaction(func(tx *gorm.DB) error {
		markers := []sessions.InternalEventInput{
			{EventVersion: RuntimeEventVersionV2, EventType: "item.completed", ActorType: "worker", Payload: map[string]any{
				"itemType": "review_entered", "status": "completed",
			}},
			{EventVersion: RuntimeEventVersionV2, EventType: "item.completed", ActorType: "worker", Payload: map[string]any{
				"itemType": "context_compaction", "status": "completed", "detail": "Durable compact summary",
			}},
		}
		for _, input := range markers {
			if _, err := service.sessions.AppendInternalEvent(ctx, tx, fixture.TenantID, fixture.SessionID, input); err != nil {
				return err
			}
		}
		for index := 0; index < resumeSnapshotEventLimit+1; index++ {
			if _, err := service.sessions.AppendInternalEvent(ctx, tx, fixture.TenantID, fixture.SessionID, sessions.InternalEventInput{
				EventVersion: RuntimeEventVersionV2,
				EventType:    "content.delta",
				ActorType:    "worker",
				Payload:      map[string]any{"streamKind": "assistant_text", "delta": "x"},
			}); err != nil {
				return err
			}
		}
		_, err := service.sessions.AppendInternalEvent(ctx, tx, fixture.TenantID, fixture.SessionID, sessions.InternalEventInput{
			EventType: "turn.created", ActorType: "user", ExecutionID: &fixture.ExecutionID,
			Payload: map[string]any{"inputText": "Continue after compaction"},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	claim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID,
		TargetKind:        fixture.TargetKind,
		ExecutionID:       &fixture.ExecutionID,
	}, "resume-marker-claim")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Value.Workload == nil || claim.Value.Workload.ResumeSnapshot == nil {
		t.Fatalf("claim omitted Resume Snapshot: %#v", claim.Value)
	}
	snapshot := claim.Value.Workload.ResumeSnapshot
	if !snapshot.Mode.Review || snapshot.Mode.ReviewSequence == nil || *snapshot.Mode.ReviewSequence != 1 {
		t.Fatalf("review state before the projection window was lost: %#v", snapshot.Mode)
	}
	if snapshot.CompactBoundary == nil || snapshot.CompactBoundary.Sequence != 2 ||
		snapshot.CompactBoundary.Summary != "Durable compact summary" {
		t.Fatalf("compact boundary before the projection window was lost: %#v", snapshot.CompactBoundary)
	}
	if snapshot.Truncation == nil || !containsString(snapshot.Truncation.Reasons, "event_limit") {
		t.Fatalf("event projection truncation was not recorded: %#v", snapshot.Truncation)
	}
	if len(snapshot.Messages) != 1 || len(snapshot.Messages[0].Text) != resumeSnapshotEventLimit {
		t.Fatalf("bounded projection did not retain the newest deltas: %#v", snapshot.Messages)
	}
}
