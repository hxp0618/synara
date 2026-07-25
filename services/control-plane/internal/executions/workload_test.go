package executions

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func TestProjectResumeSnapshotEventsAggregatesLegacyAndV2AssistantTextBySequence(t *testing.T) {
	artifactID := uuid.New()
	events := []persistence.SessionEvent{
		resumeTestEvent(9, "turn.steer-requested", map[string]any{"inputText": "Please verify it"}),
		resumeTestEvent(4, "content.delta", map[string]any{
			"streamKind": "assistant_text", "delta": "world",
		}),
		resumeTestEvent(1, "turn.created", map[string]any{"inputText": "Investigate it"}),
		resumeTestEvent(3, "content.delta", map[string]any{
			"streamKind": "reasoning_text", "delta": "private reasoning",
		}),
		resumeTestEvent(2, "runtime.output.delta", map[string]any{"text": "Hello "}),
		resumeTestEvent(5, "tool.summary", map[string]any{
			"summary": "Tests passed", "precedingToolUseIds": []any{"tool-1"},
		}),
		resumeTestEvent(6, "item.completed", map[string]any{
			"itemType": "review_entered", "status": "completed",
		}),
		resumeTestEvent(7, "item.completed", map[string]any{
			"itemType": "context_compaction", "status": "completed", "detail": "Earlier work summary",
		}),
		resumeTestEvent(8, "artifact.ready", map[string]any{"artifactId": artifactID.String()}),
	}

	projection := projectResumeSnapshotEvents(events)
	if len(projection.Messages) != 3 {
		t.Fatalf("messages = %#v, want three ordered messages", projection.Messages)
	}
	if projection.Messages[0].Role != "user" || projection.Messages[0].Text != "Investigate it" ||
		projection.Messages[1].Role != "assistant" || projection.Messages[1].Text != "Hello world" ||
		projection.Messages[1].SequenceFrom != 2 || projection.Messages[1].SequenceThrough != 4 ||
		projection.Messages[2].Role != "user" || projection.Messages[2].Text != "Please verify it" {
		t.Fatalf("unexpected deterministic message projection: %#v", projection.Messages)
	}
	if len(projection.ToolResults) != 1 || projection.ToolResults[0].Summary != "Tests passed" ||
		len(projection.ToolResults[0].ToolUseIDs) != 1 || projection.ToolResults[0].ToolUseIDs[0] != "tool-1" {
		t.Fatalf("unexpected safe tool summary: %#v", projection.ToolResults)
	}
	if !projection.Review || projection.ReviewSequence == nil || *projection.ReviewSequence != 6 {
		t.Fatalf("review state was not projected: %#v", projection)
	}
	if projection.CompactBoundary == nil || projection.CompactBoundary.Sequence != 7 ||
		projection.CompactBoundary.Summary != "Earlier work summary" {
		t.Fatalf("compact boundary was not projected: %#v", projection.CompactBoundary)
	}
	if len(projection.ArtifactEvents) != 1 || projection.ArtifactEvents[0].ArtifactID != artifactID ||
		projection.ArtifactEvents[0].Sequence != 8 {
		t.Fatalf("artifact reference event was not projected: %#v", projection.ArtifactEvents)
	}
}

func TestFitResumeSnapshotBudgetKeepsNewestContextAndRecordsLimits(t *testing.T) {
	snapshot := ResumeSnapshot{
		Version:   ResumeSnapshotVersionV1,
		SessionID: uuid.New(),
		TurnID:    uuid.New(),
		Provider:  "codex",
		Messages: []ResumeMessage{
			{Role: "user", Text: strings.Repeat("x", resumeSnapshotByteLimit), SequenceFrom: 1, SequenceThrough: 1},
			{Role: "assistant", Text: "newest answer", SequenceFrom: 2, SequenceThrough: 2},
		},
		ToolResults:                  make([]ResumeToolResult, 0),
		ArtifactReferences:           make([]ResumeArtifactReference, 0),
		PendingInteractions:          make([]ResumePendingInteraction, 0),
		SourceSequenceRange:          ResumeSequenceRange{From: 1, Through: 2},
		AuthoritativeHistorySequence: 2,
		Budget:                       ResumeSnapshotBudget{ByteLimit: resumeSnapshotByteLimit, TokenLimit: resumeSnapshotTokenLimit},
	}
	if err := fitResumeSnapshotBudget(&snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Messages) != 1 || snapshot.Messages[0].Text != "newest answer" {
		t.Fatalf("budget policy did not keep newest complete context: %#v", snapshot.Messages)
	}
	if snapshot.Truncation == nil || !containsString(snapshot.Truncation.Reasons, "byte_budget") ||
		snapshot.Truncation.DroppedBeforeSequence == nil || *snapshot.Truncation.DroppedBeforeSequence != 1 {
		t.Fatalf("budget truncation was not auditable: %#v", snapshot.Truncation)
	}
	if snapshot.Budget.UsedBytes > snapshot.Budget.ByteLimit ||
		snapshot.Budget.EstimatedTokens > snapshot.Budget.TokenLimit {
		t.Fatalf("snapshot exceeded its declared budget: %#v", snapshot.Budget)
	}
	if snapshot.IncludedSequenceRange == nil || snapshot.IncludedSequenceRange.From != 2 ||
		snapshot.IncludedSequenceRange.Through != 2 {
		t.Fatalf("included sequence range did not follow retained context: %#v", snapshot.IncludedSequenceRange)
	}
}

func TestFitResumeSnapshotBudgetRetainsSuffixOfSingleOversizedNewestMessage(t *testing.T) {
	suffix := "durable conclusion"
	snapshot := ResumeSnapshot{
		Version:   ResumeSnapshotVersionV1,
		SessionID: uuid.New(),
		TurnID:    uuid.New(),
		Provider:  "codex",
		Messages: []ResumeMessage{{
			Role: "assistant", Text: strings.Repeat("x", resumeSnapshotByteLimit) + suffix,
			SequenceFrom: 1, SequenceThrough: 2,
		}},
		ToolResults:                  make([]ResumeToolResult, 0),
		ArtifactReferences:           make([]ResumeArtifactReference, 0),
		PendingInteractions:          make([]ResumePendingInteraction, 0),
		SourceSequenceRange:          ResumeSequenceRange{From: 1, Through: 2},
		AuthoritativeHistorySequence: 2,
		Budget:                       ResumeSnapshotBudget{ByteLimit: resumeSnapshotByteLimit, TokenLimit: resumeSnapshotTokenLimit},
	}
	if err := fitResumeSnapshotBudget(&snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Messages) != 1 || !strings.HasSuffix(snapshot.Messages[0].Text, suffix) {
		t.Fatalf("single newest message was discarded instead of bounded: %#v", snapshot.Messages)
	}
	if snapshot.Truncation == nil || !containsString(snapshot.Truncation.Reasons, "message_text_budget") {
		t.Fatalf("single-message truncation was not recorded: %#v", snapshot.Truncation)
	}
}

func TestFitResumeSnapshotBudgetKeepsArtifactReferencesWhenEvictingOldNarrative(t *testing.T) {
	contentType := strings.Repeat("text/plain;", 256)
	sizeBytes := int64(12345)
	sha256 := strings.Repeat("a", 64)
	snapshot := ResumeSnapshot{
		Version:   ResumeSnapshotVersionV1,
		SessionID: uuid.New(),
		TurnID:    uuid.New(),
		Provider:  "codex",
		Messages: []ResumeMessage{
			{Role: "user", Text: strings.Repeat("x", resumeSnapshotByteLimit), SequenceFrom: 1, SequenceThrough: 1},
			{Role: "assistant", Text: "newest answer", SequenceFrom: 2, SequenceThrough: 2},
		},
		ArtifactReferences: []ResumeArtifactReference{{
			Sequence: 3, ArtifactID: uuid.New(), ExecutionID: pointerUUID(uuid.New()),
			Kind: "generated_file", ContentType: &contentType, SizeBytes: &sizeBytes, SHA256: &sha256,
		}},
		PendingInteractions:          make([]ResumePendingInteraction, 0),
		ResumeRecordedInteractions:   make([]ResumeRecordedInteraction, 0),
		SourceSequenceRange:          ResumeSequenceRange{From: 1, Through: 3},
		AuthoritativeHistorySequence: 3,
		Budget:                       ResumeSnapshotBudget{ByteLimit: resumeSnapshotByteLimit, TokenLimit: resumeSnapshotTokenLimit},
	}
	if err := fitResumeSnapshotBudget(&snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Messages) != 1 || snapshot.Messages[0].Text != "newest answer" {
		t.Fatalf("narrative eviction was not deterministic: %#v", snapshot.Messages)
	}
	if len(snapshot.ArtifactReferences) != 1 || snapshot.ArtifactReferences[0].ArtifactID == uuid.Nil ||
		snapshot.ArtifactReferences[0].SHA256 == nil || *snapshot.ArtifactReferences[0].SHA256 != sha256 {
		t.Fatalf("artifact reference was dropped or mutated: %#v", snapshot.ArtifactReferences)
	}
}

func TestFitResumeSnapshotBudgetStripsOnlyOptionalArtifactMetadata(t *testing.T) {
	contentType := strings.Repeat("application/octet-stream;", 512)
	sizeBytes := int64(987654321)
	sha256 := strings.Repeat("b", 64)
	snapshot := ResumeSnapshot{
		Version:   ResumeSnapshotVersionV1,
		SessionID: uuid.New(),
		TurnID:    uuid.New(),
		Provider:  "codex",
		ArtifactReferences: []ResumeArtifactReference{{
			Sequence: 1, ArtifactID: uuid.New(), ExecutionID: pointerUUID(uuid.New()),
			Kind: "generated_file", ContentType: &contentType, SizeBytes: &sizeBytes, SHA256: &sha256,
		}},
		PendingInteractions:          make([]ResumePendingInteraction, 0),
		ResumeRecordedInteractions:   make([]ResumeRecordedInteraction, 0),
		SourceSequenceRange:          ResumeSequenceRange{From: 1, Through: 1},
		AuthoritativeHistorySequence: 1,
		Budget:                       ResumeSnapshotBudget{ByteLimit: 1024, TokenLimit: 256},
	}
	if err := fitResumeSnapshotBudget(&snapshot); err != nil {
		t.Fatal(err)
	}
	reference := snapshot.ArtifactReferences[0]
	if reference.ContentType != nil || reference.SizeBytes != nil ||
		reference.SHA256 == nil || *reference.SHA256 != sha256 {
		t.Fatalf("artifact metadata stripping did not preserve required authority fields: %#v", reference)
	}
	if snapshot.Truncation == nil || !containsString(snapshot.Truncation.Reasons, "artifact_optional_metadata_budget") {
		t.Fatalf("artifact metadata truncation was not recorded: %#v", snapshot.Truncation)
	}
}

func TestFitResumeSnapshotBudgetFailsClosedWhenArtifactAuthorityAloneExceedsBudget(t *testing.T) {
	sha256 := strings.Repeat("c", 64)
	snapshot := ResumeSnapshot{
		Version:   ResumeSnapshotVersionV1,
		SessionID: uuid.New(),
		TurnID:    uuid.New(),
		Provider:  "codex",
		ArtifactReferences: []ResumeArtifactReference{{
			Sequence: 1, ArtifactID: uuid.New(), ExecutionID: pointerUUID(uuid.New()),
			Kind: "generated_file", SHA256: &sha256,
		}},
		PendingInteractions:          make([]ResumePendingInteraction, 0),
		ResumeRecordedInteractions:   make([]ResumeRecordedInteraction, 0),
		SourceSequenceRange:          ResumeSequenceRange{From: 1, Through: 1},
		AuthoritativeHistorySequence: 1,
		Budget:                       ResumeSnapshotBudget{ByteLimit: 64, TokenLimit: 16},
	}
	err := fitResumeSnapshotBudget(&snapshot)
	if err == nil {
		t.Fatal("expected artifact authority budget failure")
	}
	assertExecutionProblemCode(t, err, "resume_snapshot_artifact_authority_budget_exhausted")
}

func TestFitResumeSnapshotBudgetDropsExcessPendingInteractionBeforeArtifactMetadata(t *testing.T) {
	contentType := "application/octet-stream"
	sizeBytes := int64(42)
	sha256 := strings.Repeat("d", 64)
	interaction := func(requestID string) ResumePendingInteraction {
		return ResumePendingInteraction{
			ID: uuid.New(), ExecutionID: uuid.New(), TurnID: uuid.New(), Provider: "codex",
			RequestID: requestID, EventVersion: RuntimeEventVersionV2, Kind: "approval",
			RequestedAt: time.Unix(100, 0).UTC(), ExpiresAt: time.Unix(200, 0).UTC(),
		}
	}
	first := interaction("approval-1")
	second := interaction("approval-2")
	snapshot := ResumeSnapshot{
		Version:   ResumeSnapshotVersionV1,
		SessionID: uuid.New(),
		TurnID:    uuid.New(),
		Provider:  "codex",
		ArtifactReferences: []ResumeArtifactReference{{
			Sequence: 1, ArtifactID: uuid.New(), ExecutionID: pointerUUID(uuid.New()),
			Kind: "generated_file", ContentType: &contentType, SizeBytes: &sizeBytes, SHA256: &sha256,
		}},
		PendingInteractions:          []ResumePendingInteraction{first, second},
		ResumeRecordedInteractions:   make([]ResumeRecordedInteraction, 0),
		SourceSequenceRange:          ResumeSequenceRange{From: 1, Through: 1},
		AuthoritativeHistorySequence: 1,
		Budget:                       ResumeSnapshotBudget{ByteLimit: 999999, TokenLimit: 999999},
	}
	single := snapshot
	single.PendingInteractions = []ResumePendingInteraction{second}
	if _, _, err := refreshResumeSnapshotBudget(&single); err != nil {
		t.Fatal(err)
	}
	snapshot.Budget.ByteLimit = single.Budget.UsedBytes + 192
	if err := fitResumeSnapshotBudget(&snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.PendingInteractions) != 1 || snapshot.PendingInteractions[0].RequestID != second.RequestID {
		t.Fatalf("oldest excess pending interaction was not dropped: %#v", snapshot.PendingInteractions)
	}
	reference := snapshot.ArtifactReferences[0]
	if reference.ContentType == nil || reference.SizeBytes == nil || reference.SHA256 == nil {
		t.Fatalf("artifact metadata was stripped before excess pending interaction: %#v", reference)
	}
	if snapshot.Truncation == nil || !containsString(snapshot.Truncation.Reasons, "pending_interaction_budget") {
		t.Fatalf("pending interaction truncation was not recorded: %#v", snapshot.Truncation)
	}
}

func TestResumePendingInteractionAllowsOnlySafePromptMetadata(t *testing.T) {
	now := time.Now().UTC()
	interaction, truncated := resumePendingInteraction(persistence.ExecutionInteraction{
		ID: uuid.New(), ExecutionID: uuid.New(), TurnID: uuid.New(), Provider: "codex",
		RequestID: "approval-1", EventVersion: RuntimeEventVersionV2, Kind: "approval",
		RequestedAt: now, ExpiresAt: now.Add(time.Hour),
		Payload: map[string]any{
			"requestType": "exec_command_approval",
			"detail":      strings.Repeat("d", 5000),
			"command":     "rm -rf /sensitive/path",
			"secret":      "provider-secret",
			"questions": []any{map[string]any{
				"id": "environment", "header": "Environment", "question": "Which environment?",
				"options": []any{map[string]any{"label": "Staging", "description": "Use staging"}},
			}},
		},
	})
	if !truncated || len(interaction.Detail) != 4096 || interaction.RequestType != "exec_command_approval" ||
		len(interaction.Questions) != 1 || len(interaction.Questions[0].Options) != 1 {
		t.Fatalf("unexpected safe pending interaction: %#v truncated=%v", interaction, truncated)
	}
	encoded, err := json.Marshal(interaction)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "provider-secret") || strings.Contains(string(encoded), "rm -rf") ||
		strings.Contains(string(encoded), "sensitive/path") {
		t.Fatalf("unsafe interaction payload leaked into Resume Snapshot: %s", encoded)
	}
}

func TestConversationHistoryCompatibilityUsesResumeSnapshotMessages(t *testing.T) {
	snapshot := ResumeSnapshot{Messages: []ResumeMessage{
		{Role: "user", Text: "question", SequenceFrom: 1, SequenceThrough: 1},
		{Role: "assistant", Text: "answer", SequenceFrom: 2, SequenceThrough: 3},
	}}
	history := conversationHistoryFromResumeSnapshot(snapshot)
	if len(history) != 2 || history[0] != (ConversationMessage{Role: "user", Text: "question"}) ||
		history[1] != (ConversationMessage{Role: "assistant", Text: "answer"}) {
		t.Fatalf("legacy ConversationHistory diverged from Resume Snapshot: %#v", history)
	}
}

func TestResumeSnapshotStateAndSequenceQueriesAreSQLiteCompatible(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&persistence.AgentSession{}, &persistence.SessionEvent{}); err != nil {
		t.Fatal(err)
	}
	tenantID := uuid.New()
	sessionID := uuid.New()
	executionID := uuid.New()
	now := time.Now().UTC()
	if err := db.Create(&persistence.AgentSession{
		ID: sessionID, TenantID: tenantID, OrganizationID: uuid.New(), ProjectID: uuid.New(),
		CreatedBy: uuid.New(), Title: "SQLite state marker projection", Status: "active",
		Visibility: "private", Provider: "codex", ExecutionTargetID: uuid.New(),
		LastEventSequence: 2, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	for _, event := range []persistence.SessionEvent{
		{
			TenantID: tenantID, SessionID: sessionID, Sequence: 1, EventID: uuid.New(),
			EventVersion: RuntimeEventVersionV2, EventType: "item.completed", ActorType: "worker",
			Payload: map[string]any{"itemType": "review_entered", "status": "completed"}, OccurredAt: now,
		},
		{
			TenantID: tenantID, SessionID: sessionID, Sequence: 2, EventID: uuid.New(),
			EventVersion: RuntimeEventVersionV2, EventType: "item.completed", ActorType: "worker",
			Payload: map[string]any{"itemType": "context_compaction", "status": "completed", "detail": "summary"}, OccurredAt: now,
		},
	} {
		if err := db.Create(&event).Error; err != nil {
			t.Fatal(err)
		}
	}
	projection := resumeSnapshotProjection{}
	if err := loadResumeStateMarkers(context.Background(), db, persistence.AgentExecution{
		ID: executionID, TenantID: tenantID, SessionID: sessionID,
	}, 3, &projection); err != nil {
		t.Fatal(err)
	}
	if !projection.Review || projection.ReviewSequence == nil || *projection.ReviewSequence != 1 ||
		projection.CompactBoundary == nil || projection.CompactBoundary.Sequence != 2 {
		t.Fatalf("SQLite state marker projection failed: %#v", projection)
	}

	if err := db.Exec(`CREATE TABLE provider_runtime_bindings (
		id TEXT PRIMARY KEY,
		tenant_id TEXT NOT NULL,
		session_id TEXT NOT NULL,
		authoritative_history_sequence INTEGER NOT NULL,
		updated_at DATETIME NOT NULL
	)`).Error; err != nil {
		t.Fatal(err)
	}
	bindingID := uuid.New()
	if err := db.Exec(
		"INSERT INTO provider_runtime_bindings (id, tenant_id, session_id, authoritative_history_sequence, updated_at) VALUES (?, ?, ?, ?, ?)",
		bindingID, tenantID, sessionID, 2, now,
	).Error; err != nil {
		t.Fatal(err)
	}
	service := &Service{now: func() time.Time { return now.Add(time.Minute) }}
	execution := persistence.AgentExecution{
		ID: executionID, TenantID: tenantID, SessionID: sessionID, ProviderRuntimeBindingID: &bindingID,
	}
	if err := service.advanceAuthoritativeHistorySequence(context.Background(), db, execution, 7); err != nil {
		t.Fatal(err)
	}
	if err := service.advanceAuthoritativeHistorySequence(context.Background(), db, execution, 3); err != nil {
		t.Fatal(err)
	}
	var sequence int64
	if err := db.Table("provider_runtime_bindings").Select("authoritative_history_sequence").
		Where("id = ?", bindingID).Scan(&sequence).Error; err != nil {
		t.Fatal(err)
	}
	if sequence != 7 {
		t.Fatalf("SQLite authoritative history sequence regressed: got %d want 7", sequence)
	}
}

func TestLoadResumeArtifactReferencesFailClosedForUnavailableOrScopeMismatch(t *testing.T) {
	t.Run("missing or unready artifact fails closed", func(t *testing.T) {
		db := newResumeArtifactAuthorityTestDB(t)
		tenantID := uuid.New()
		sessionID := uuid.New()
		executionID := uuid.New()
		now := time.Now().UTC()
		seedResumeArtifactAuthoritySession(t, db, tenantID, sessionID, now)
		pendingArtifact := persistence.Artifact{
			ID: uuid.New(), TenantID: tenantID, OrganizationID: uuid.New(), ProjectID: uuid.New(),
			SessionID: sessionID, ExecutionID: &executionID, Kind: "generated_file", Status: "pending",
			Bucket: "resume-test", ObjectKey: "artifact/pending", CreatedByType: "system", CreatedByID: uuid.New(), CreatedAt: now,
		}
		seedResumeArtifactAuthorityArtifact(t, db, pendingArtifact)
		for _, artifactID := range []uuid.UUID{uuid.New(), pendingArtifact.ID} {
			_, err := loadResumeArtifactReferences(context.Background(), db, persistence.AgentExecution{
				TenantID: tenantID, SessionID: sessionID,
			}, []resumeArtifactEvent{{
				Sequence: 1, ArtifactID: artifactID, SessionID: sessionID, ExecutionID: &executionID,
			}})
			assertExecutionProblemCode(t, err, "execution_history_artifact_unavailable")
		}
	})

	t.Run("deleted artifact fails closed", func(t *testing.T) {
		db := newResumeArtifactAuthorityTestDB(t)
		tenantID := uuid.New()
		sessionID := uuid.New()
		executionID := uuid.New()
		now := time.Now().UTC()
		seedResumeArtifactAuthoritySession(t, db, tenantID, sessionID, now)
		readyAt := now
		deletedAt := now.Add(time.Minute)
		artifact := persistence.Artifact{
			ID: uuid.New(), TenantID: tenantID, OrganizationID: uuid.New(), ProjectID: uuid.New(),
			SessionID: sessionID, ExecutionID: &executionID, Kind: "generated_file", Status: "ready",
			Bucket: "resume-test", ObjectKey: "artifact/deleted", CreatedByType: "system", CreatedByID: uuid.New(),
			ReadyAt: &readyAt, CreatedAt: now, DeletedAt: &deletedAt,
		}
		seedResumeArtifactAuthorityArtifact(t, db, artifact)
		_, err := loadResumeArtifactReferences(context.Background(), db, persistence.AgentExecution{
			TenantID: tenantID, SessionID: sessionID,
		}, []resumeArtifactEvent{{
			Sequence: 1, ArtifactID: artifact.ID, SessionID: sessionID, ExecutionID: &executionID,
		}})
		assertExecutionProblemCode(t, err, "execution_history_artifact_unavailable")
	})

	t.Run("ready artifact without content hash fails closed", func(t *testing.T) {
		db := newResumeArtifactAuthorityTestDB(t)
		tenantID := uuid.New()
		sessionID := uuid.New()
		executionID := uuid.New()
		now := time.Now().UTC()
		seedResumeArtifactAuthoritySession(t, db, tenantID, sessionID, now)
		readyAt := now
		artifact := persistence.Artifact{
			ID: uuid.New(), TenantID: tenantID, OrganizationID: uuid.New(), ProjectID: uuid.New(),
			SessionID: sessionID, ExecutionID: &executionID, Kind: "generated_file", Status: "ready",
			Bucket: "resume-test", ObjectKey: "artifact/hashless", CreatedByType: "system", CreatedByID: uuid.New(),
			ReadyAt: &readyAt, CreatedAt: now,
		}
		seedResumeArtifactAuthorityArtifact(t, db, artifact)
		_, err := loadResumeArtifactReferences(context.Background(), db, persistence.AgentExecution{
			TenantID: tenantID, SessionID: sessionID,
		}, []resumeArtifactEvent{{
			Sequence: 1, ArtifactID: artifact.ID, SessionID: sessionID, ExecutionID: &executionID,
		}})
		assertExecutionProblemCode(t, err, "execution_history_artifact_unavailable")
	})

	t.Run("cross session fails closed", func(t *testing.T) {
		db := newResumeArtifactAuthorityTestDB(t)
		tenantID := uuid.New()
		sessionID := uuid.New()
		otherSessionID := uuid.New()
		executionID := uuid.New()
		now := time.Now().UTC()
		seedResumeArtifactAuthoritySession(t, db, tenantID, sessionID, now)
		seedResumeArtifactAuthoritySession(t, db, tenantID, otherSessionID, now)
		readyAt := now
		artifact := persistence.Artifact{
			ID: uuid.New(), TenantID: tenantID, OrganizationID: uuid.New(), ProjectID: uuid.New(),
			SessionID: otherSessionID, ExecutionID: &executionID, Kind: "generated_file", Status: "ready",
			Bucket: "resume-test", ObjectKey: "artifact/other-session", CreatedByType: "system", CreatedByID: uuid.New(),
			ReadyAt: &readyAt, CreatedAt: now,
		}
		seedResumeArtifactAuthorityArtifact(t, db, artifact)
		_, err := loadResumeArtifactReferences(context.Background(), db, persistence.AgentExecution{
			TenantID: tenantID, SessionID: sessionID,
		}, []resumeArtifactEvent{{
			Sequence: 1, ArtifactID: artifact.ID, SessionID: sessionID, ExecutionID: &executionID,
		}})
		assertExecutionProblemCode(t, err, "execution_history_artifact_scope_mismatch")
	})

	t.Run("cross execution fails closed", func(t *testing.T) {
		db := newResumeArtifactAuthorityTestDB(t)
		tenantID := uuid.New()
		sessionID := uuid.New()
		executionID := uuid.New()
		otherExecutionID := uuid.New()
		now := time.Now().UTC()
		seedResumeArtifactAuthoritySession(t, db, tenantID, sessionID, now)
		readyAt := now
		artifact := persistence.Artifact{
			ID: uuid.New(), TenantID: tenantID, OrganizationID: uuid.New(), ProjectID: uuid.New(),
			SessionID: sessionID, ExecutionID: &otherExecutionID, Kind: "generated_file", Status: "ready",
			Bucket: "resume-test", ObjectKey: "artifact/other-exec", CreatedByType: "system", CreatedByID: uuid.New(),
			ReadyAt: &readyAt, CreatedAt: now,
		}
		seedResumeArtifactAuthorityArtifact(t, db, artifact)
		_, err := loadResumeArtifactReferences(context.Background(), db, persistence.AgentExecution{
			TenantID: tenantID, SessionID: sessionID,
		}, []resumeArtifactEvent{{
			Sequence: 1, ArtifactID: artifact.ID, SessionID: sessionID, ExecutionID: &executionID,
		}})
		assertExecutionProblemCode(t, err, "execution_history_artifact_scope_mismatch")
	})
}

func resumeTestEvent(sequence int64, eventType string, payload map[string]any) persistence.SessionEvent {
	return persistence.SessionEvent{
		EventID: uuid.New(), Sequence: sequence, EventType: eventType, Payload: payload,
	}
}

func newResumeArtifactAuthorityTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&persistence.AgentSession{}, &persistence.Artifact{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func seedResumeArtifactAuthoritySession(t *testing.T, db *gorm.DB, tenantID, sessionID uuid.UUID, now time.Time) {
	t.Helper()
	if err := db.Create(&persistence.AgentSession{
		ID: sessionID, TenantID: tenantID, OrganizationID: uuid.New(), ProjectID: uuid.New(), CreatedBy: uuid.New(),
		Title: "Resume Artifact Authority", Status: "active", Visibility: "private", Provider: "codex",
		ExecutionTargetID: uuid.New(), LastEventSequence: 1, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
}

func seedResumeArtifactAuthorityArtifact(t *testing.T, db *gorm.DB, artifact persistence.Artifact) {
	t.Helper()
	if err := db.Create(&artifact).Error; err != nil {
		t.Fatal(err)
	}
}

func pointerUUID(value uuid.UUID) *uuid.UUID { return &value }

func assertExecutionProblemCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected problem code %q, got nil", code)
	}
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != code {
		t.Fatalf("problem error = %v, want code %q", err, code)
	}
}
