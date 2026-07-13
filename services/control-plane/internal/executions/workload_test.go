package executions

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
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
	if err := db.AutoMigrate(&persistence.SessionEvent{}); err != nil {
		t.Fatal(err)
	}
	tenantID := uuid.New()
	sessionID := uuid.New()
	executionID := uuid.New()
	now := time.Now().UTC()
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

func resumeTestEvent(sequence int64, eventType string, payload map[string]any) persistence.SessionEvent {
	return persistence.SessionEvent{
		EventID: uuid.New(), Sequence: sequence, EventType: eventType, Payload: payload,
	}
}
