package executions

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

const (
	resumeSnapshotEventLimit              = 500
	resumeSnapshotByteLimit               = 256 << 10
	resumeSnapshotTokenLimit              = 64 << 10
	resumeSnapshotPendingInteractionLimit = 64
	resumeSnapshotToolSummaryByteLimit    = 8 << 10
	resumeSnapshotCompactSummaryByteLimit = 16 << 10
	resumeSnapshotStateMarkerPageLimit    = 512
)

var resumeSnapshotEventTypes = []string{
	"turn.created",
	"turn.steer-requested",
	"runtime.output.delta",
	"content.delta",
	"tool.summary",
	"item.started",
	"item.updated",
	"item.completed",
	"thread.state.changed",
	"artifact.ready",
}

var resumeSnapshotStateEventTypes = []string{
	"item.started",
	"item.updated",
	"item.completed",
	"thread.state.changed",
}

type resumeSnapshotContext struct {
	Provider                              string
	Model                                 *string
	RuntimeMode                           string
	InteractionMode                       string
	RemoteWorkspaceID                     *uuid.UUID
	WorkspaceMaterializationID            *uuid.UUID
	WorkspaceMaterializationIncarnationID *uuid.UUID
	WorkspaceLayoutVersion                int
	WorkspaceRepositoryFingerprint        *string
	WorkspaceDefaultBranch                string
	WorkspaceCurrentBranch                *string
	WorkspaceBaseCommit                   *string
	WorkspaceHeadCommit                   *string
	Checkpoint                            *WorkspaceCheckpoint
}

type resumeArtifactEvent struct {
	Sequence   int64
	ArtifactID uuid.UUID
}

type resumeSnapshotProjection struct {
	Messages          []ResumeMessage
	ToolResults       []ResumeToolResult
	ArtifactEvents    []resumeArtifactEvent
	Review            bool
	ReviewSequence    *int64
	CompactBoundary   *ResumeCompactBoundary
	TruncationReasons []string
	DroppedBefore     *int64
}

func (s *Service) loadResumeSnapshot(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	input resumeSnapshotContext,
) (ResumeSnapshot, error) {
	snapshot := ResumeSnapshot{
		Version:            ResumeSnapshotVersionV1,
		SessionID:          execution.SessionID,
		TurnID:             execution.TurnID,
		Provider:           input.Provider,
		Model:              cloneStringPointer(input.Model),
		Messages:           make([]ResumeMessage, 0),
		ToolResults:        make([]ResumeToolResult, 0),
		ArtifactReferences: make([]ResumeArtifactReference, 0),
		Mode: ResumeMode{
			RuntimeMode:     input.RuntimeMode,
			InteractionMode: input.InteractionMode,
			Plan:            input.InteractionMode == "plan",
		},
		PendingInteractions: make([]ResumePendingInteraction, 0),
		Workspace:           resumeWorkspaceReference(input),
		Budget: ResumeSnapshotBudget{
			ByteLimit:  resumeSnapshotByteLimit,
			TokenLimit: resumeSnapshotTokenLimit,
		},
	}

	currentSequence, found, err := loadCurrentTurnSequence(ctx, tx, execution)
	if err != nil {
		return ResumeSnapshot{}, err
	}
	projection := resumeSnapshotProjection{
		Messages:       make([]ResumeMessage, 0),
		ToolResults:    make([]ResumeToolResult, 0),
		ArtifactEvents: make([]resumeArtifactEvent, 0),
	}
	if found {
		allEvents, truncated, historyErr := loadEffectiveResumeSnapshotEvents(
			ctx, tx, execution, currentSequence-1,
		)
		if historyErr != nil {
			return ResumeSnapshot{}, historyErr
		}
		snapshot.SourceSequenceRange = resumeSourceSequenceRange(allEvents, currentSequence-1)
		snapshot.AuthoritativeHistorySequence = currentSequence - 1

		projection = projectResumeSnapshotEvents(allEvents)
		projectResumeStateMarkers(allEvents, &projection)
		if truncated {
			if err := loadResumeStateMarkers(ctx, tx, execution, currentSequence, &projection); err != nil {
				return ResumeSnapshot{}, err
			}
			projection.TruncationReasons = appendUniqueString(projection.TruncationReasons, "event_limit")
			if len(allEvents) > 0 {
				droppedBefore := allEvents[0].Sequence - 1
				projection.DroppedBefore = &droppedBefore
			}
		}
		applyResumeCompactBoundary(&projection)
	}

	snapshot.Messages = projection.Messages
	snapshot.ToolResults = projection.ToolResults
	snapshot.Mode.Review = projection.Review
	snapshot.Mode.ReviewSequence = projection.ReviewSequence
	snapshot.CompactBoundary = projection.CompactBoundary
	artifactReferences, err := loadResumeArtifactReferences(ctx, tx, execution, projection.ArtifactEvents)
	if err != nil {
		return ResumeSnapshot{}, err
	}
	snapshot.ArtifactReferences = artifactReferences
	pending, pendingReasons, err := s.loadResumePendingInteractions(ctx, tx, execution)
	if err != nil {
		return ResumeSnapshot{}, err
	}
	snapshot.PendingInteractions = pending
	projection.TruncationReasons = appendUniqueStrings(projection.TruncationReasons, pendingReasons...)
	if len(projection.TruncationReasons) > 0 {
		snapshot.Truncation = &ResumeSnapshotTruncation{
			Reasons:               append([]string(nil), projection.TruncationReasons...),
			DroppedBeforeSequence: cloneInt64Pointer(projection.DroppedBefore),
		}
	}
	if err := fitResumeSnapshotBudget(&snapshot); err != nil {
		return ResumeSnapshot{}, err
	}
	if err := s.advanceAuthoritativeHistorySequence(ctx, tx, execution, snapshot.AuthoritativeHistorySequence); err != nil {
		return ResumeSnapshot{}, err
	}
	return snapshot, nil
}

func loadEffectiveResumeSnapshotEvents(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	throughSequence int64,
) ([]persistence.SessionEvent, bool, error) {
	if throughSequence <= 0 {
		return []persistence.SessionEvent{}, false, nil
	}
	remaining := resumeSnapshotEventLimit + 1
	cursor := throughSequence
	chunksNewestFirst := make([][]persistence.SessionEvent, 0, 4)
	for cursor > 0 && remaining > 0 {
		rollbacks, err := sessions.LoadLogicalEventsTail(
			ctx,
			tx,
			execution.TenantID,
			execution.SessionID,
			0,
			cursor,
			1,
			[]string{"session.history.rolled-back"},
		)
		if err != nil {
			return nil, false, problem.Wrap(500, "execution_history_load_failed", "Failed to load logical Session rollback history.", err)
		}
		lowerBound := int64(0)
		if len(rollbacks) > 0 {
			lowerBound = rollbacks[0].Event.Sequence
		}
		logical, err := sessions.LoadLogicalEventsTail(
			ctx,
			tx,
			execution.TenantID,
			execution.SessionID,
			lowerBound,
			cursor,
			remaining,
			resumeSnapshotEventTypes,
		)
		if err != nil {
			return nil, false, problem.Wrap(500, "execution_history_load_failed", "Failed to load logical Session resume history.", err)
		}
		chunk := make([]persistence.SessionEvent, 0, len(logical))
		for _, item := range logical {
			chunk = append(chunk, item.Event)
		}
		chunksNewestFirst = append(chunksNewestFirst, chunk)
		remaining -= len(chunk)
		if len(rollbacks) == 0 {
			break
		}
		fromSequence, ok := safeInt64Payload(rollbacks[0].Event.Payload, "fromSequence")
		if !ok || fromSequence <= 0 || fromSequence >= rollbacks[0].Event.Sequence {
			return nil, false, problem.New(409, "rollback_target_stale", "The rollback event contains an invalid source sequence.")
		}
		cursor = fromSequence - 1
	}

	combined := make([]persistence.SessionEvent, 0, resumeSnapshotEventLimit+1-remaining)
	for index := len(chunksNewestFirst) - 1; index >= 0; index-- {
		combined = append(combined, chunksNewestFirst[index]...)
	}
	truncated := len(combined) > resumeSnapshotEventLimit
	if truncated {
		combined = combined[len(combined)-resumeSnapshotEventLimit:]
	}
	return combined, truncated, nil
}

func applyResumeCompactBoundary(projection *resumeSnapshotProjection) {
	if projection.CompactBoundary == nil || projection.CompactBoundary.Sequence <= 0 {
		return
	}
	boundary := projection.CompactBoundary.Sequence
	messages := projection.Messages[:0]
	for _, message := range projection.Messages {
		if message.SequenceFrom > boundary {
			messages = append(messages, message)
		}
	}
	projection.Messages = messages
	tools := projection.ToolResults[:0]
	for _, result := range projection.ToolResults {
		if result.Sequence > boundary {
			tools = append(tools, result)
		}
	}
	projection.ToolResults = tools
	artifacts := projection.ArtifactEvents[:0]
	for _, reference := range projection.ArtifactEvents {
		if reference.Sequence > boundary {
			artifacts = append(artifacts, reference)
		}
	}
	projection.ArtifactEvents = artifacts
}

func resumeSourceSequenceRange(events []persistence.SessionEvent, through int64) ResumeSequenceRange {
	if through <= 0 {
		return ResumeSequenceRange{}
	}
	from := int64(0)
	for _, event := range events {
		if from == 0 || event.Sequence < from {
			from = event.Sequence
		}
	}
	return ResumeSequenceRange{From: from, Through: through}
}

func boundedResumeSnapshotEvents(all []persistence.SessionEvent) ([]persistence.SessionEvent, bool) {
	events := make([]persistence.SessionEvent, 0, len(all))
	allowed := make(map[string]struct{}, len(resumeSnapshotEventTypes))
	for _, eventType := range resumeSnapshotEventTypes {
		allowed[eventType] = struct{}{}
	}
	for _, event := range all {
		if _, ok := allowed[event.EventType]; ok {
			events = append(events, event)
		}
	}
	truncated := len(events) > resumeSnapshotEventLimit
	if truncated {
		events = events[len(events)-resumeSnapshotEventLimit:]
	}
	return events, truncated
}

func projectResumeStateMarkers(events []persistence.SessionEvent, projection *resumeSnapshotProjection) {
	for _, event := range events {
		switch event.EventType {
		case "item.started", "item.updated", "item.completed":
			switch stringPayload(event.Payload, "itemType") {
			case "review_entered":
				projection.Review = true
				sequence := event.Sequence
				projection.ReviewSequence = &sequence
			case "review_exited":
				projection.Review = false
				sequence := event.Sequence
				projection.ReviewSequence = &sequence
			case "context_compaction":
				if event.EventType == "item.completed" || stringPayload(event.Payload, "status") == "completed" {
					projection.CompactBoundary = &ResumeCompactBoundary{
						Sequence: event.Sequence,
						Summary:  truncateResumeText(firstResumeString(event.Payload, "detail", "title"), resumeSnapshotCompactSummaryByteLimit),
					}
				}
			}
		case "thread.state.changed":
			if stringPayload(event.Payload, "state") == "compacted" {
				sequence := event.Sequence
				if value, ok := safeInt64Payload(event.Payload, "compactedThroughSequence"); ok {
					sequence = value
				}
				projection.CompactBoundary = &ResumeCompactBoundary{
					Sequence: sequence,
					Summary:  truncateResumeText(strings.TrimSpace(stringPayload(event.Payload, "summary")), resumeSnapshotCompactSummaryByteLimit),
				}
			}
		}
	}
}

func firstResumeString(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(stringPayload(payload, key)); value != "" {
			return value
		}
	}
	return ""
}

func safeInt64Payload(payload map[string]any, key string) (int64, bool) {
	value, ok := payload[key]
	if !ok {
		return 0, false
	}
	switch typed := value.(type) {
	case int64:
		return typed, typed >= 0
	case int:
		return int64(typed), typed >= 0
	case float64:
		converted := int64(typed)
		return converted, typed >= 0 && float64(converted) == typed
	default:
		return 0, false
	}
}

func loadCurrentTurnSequence(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
) (int64, bool, error) {
	var current persistence.SessionEvent
	err := tx.WithContext(ctx).
		Select("tenant_id", "session_id", "sequence").
		Where("tenant_id = ? AND session_id = ? AND execution_id = ? AND event_type = ?",
			execution.TenantID, execution.SessionID, execution.ID, "turn.created").
		Order("sequence").Take(&current).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, problem.Wrap(500, "execution_history_cursor_load_failed", "Failed to locate the current Turn in Session history.", err)
	}
	return current.Sequence, true, nil
}

func loadResumeSourceSequenceRange(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	currentSequence int64,
) (ResumeSequenceRange, error) {
	var row struct {
		From    int64 `gorm:"column:sequence_from"`
		Through int64 `gorm:"column:sequence_through"`
	}
	if err := tx.WithContext(ctx).Model(&persistence.SessionEvent{}).
		Select("COALESCE(MIN(sequence), 0) AS sequence_from, COALESCE(MAX(sequence), 0) AS sequence_through").
		Where("tenant_id = ? AND session_id = ? AND sequence < ?",
			execution.TenantID, execution.SessionID, currentSequence).
		Scan(&row).Error; err != nil {
		return ResumeSequenceRange{}, problem.Wrap(500, "execution_history_range_load_failed", "Failed to load the authoritative Session history range.", err)
	}
	return ResumeSequenceRange{From: row.From, Through: row.Through}, nil
}

func loadResumeSnapshotEvents(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	currentSequence int64,
) ([]persistence.SessionEvent, bool, error) {
	events := make([]persistence.SessionEvent, 0, resumeSnapshotEventLimit+1)
	if err := tx.WithContext(ctx).
		Where("tenant_id = ? AND session_id = ? AND sequence < ? AND event_type IN ?",
			execution.TenantID, execution.SessionID, currentSequence, resumeSnapshotEventTypes).
		Order("sequence DESC").Limit(resumeSnapshotEventLimit + 1).Find(&events).Error; err != nil {
		return nil, false, problem.Wrap(500, "execution_history_load_failed", "Failed to load Session resume history.", err)
	}
	truncated := len(events) > resumeSnapshotEventLimit
	if truncated {
		events = events[:resumeSnapshotEventLimit]
	}
	sort.Slice(events, func(left, right int) bool {
		if events[left].Sequence == events[right].Sequence {
			return events[left].EventID.String() < events[right].EventID.String()
		}
		return events[left].Sequence < events[right].Sequence
	})
	return events, truncated, nil
}

func projectResumeSnapshotEvents(events []persistence.SessionEvent) resumeSnapshotProjection {
	ordered := append([]persistence.SessionEvent(nil), events...)
	sort.Slice(ordered, func(left, right int) bool {
		if ordered[left].Sequence == ordered[right].Sequence {
			return ordered[left].EventID.String() < ordered[right].EventID.String()
		}
		return ordered[left].Sequence < ordered[right].Sequence
	})
	projection := resumeSnapshotProjection{
		Messages:       make([]ResumeMessage, 0),
		ToolResults:    make([]ResumeToolResult, 0),
		ArtifactEvents: make([]resumeArtifactEvent, 0),
	}
	seenArtifacts := make(map[uuid.UUID]struct{})
	for _, event := range ordered {
		switch event.EventType {
		case "turn.created", "turn.steer-requested":
			appendResumeUserMessage(&projection.Messages, event)
		case "runtime.output.delta":
			appendResumeAssistantDelta(&projection.Messages, event.Sequence, stringPayload(event.Payload, "text"))
		case "content.delta":
			if stringPayload(event.Payload, "streamKind") == "assistant_text" {
				appendResumeAssistantDelta(&projection.Messages, event.Sequence, stringPayload(event.Payload, "delta"))
			}
		case "tool.summary":
			if summary := strings.TrimSpace(stringPayload(event.Payload, "summary")); summary != "" {
				projection.ToolResults = append(projection.ToolResults, ResumeToolResult{
					Sequence:   event.Sequence,
					Kind:       "tool_summary",
					Summary:    truncateResumeText(summary, resumeSnapshotToolSummaryByteLimit),
					ToolUseIDs: stringSlicePayload(event.Payload, "precedingToolUseIds"),
				})
			}
		case "item.started", "item.updated", "item.completed":
			projectResumeItemEvent(&projection, event)
		case "thread.state.changed":
			if stringPayload(event.Payload, "state") == "compacted" {
				projection.CompactBoundary = &ResumeCompactBoundary{Sequence: event.Sequence}
			}
		case "artifact.ready":
			artifactID, ok := uuidPayload(event.Payload, "artifactId")
			if !ok {
				continue
			}
			if _, duplicate := seenArtifacts[artifactID]; duplicate {
				continue
			}
			seenArtifacts[artifactID] = struct{}{}
			projection.ArtifactEvents = append(projection.ArtifactEvents, resumeArtifactEvent{
				Sequence:   event.Sequence,
				ArtifactID: artifactID,
			})
		}
	}
	projection.Messages = normalizeResumeMessages(projection.Messages)
	return projection
}

func loadResumeStateMarkers(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	currentSequence int64,
	projection *resumeSnapshotProjection,
) error {
	if currentSequence <= 1 {
		return nil
	}
	reviewFound := projection.ReviewSequence != nil
	compactFound := projection.CompactBoundary != nil
	cursor := currentSequence - 1
	for cursor > 0 && (!reviewFound || !compactFound) {
		rollbacks, err := sessions.LoadLogicalEventsTail(
			ctx, tx, execution.TenantID, execution.SessionID, 0, cursor, 1,
			[]string{"session.history.rolled-back"},
		)
		if err != nil {
			return problem.Wrap(500, "execution_history_state_load_failed", "Failed to load logical Session state history.", err)
		}
		lowerBound := int64(0)
		if len(rollbacks) > 0 {
			lowerBound = rollbacks[0].Event.Sequence
		}
		segmentCursor := cursor
		for segmentCursor > lowerBound && (!reviewFound || !compactFound) {
			logical, err := sessions.LoadLogicalEventsTail(
				ctx, tx, execution.TenantID, execution.SessionID,
				lowerBound, segmentCursor, resumeSnapshotStateMarkerPageLimit,
				resumeSnapshotStateEventTypes,
			)
			if err != nil {
				return problem.Wrap(500, "execution_history_state_load_failed", "Failed to load logical Session state markers.", err)
			}
			if len(logical) == 0 {
				break
			}
			for index := len(logical) - 1; index >= 0 && (!reviewFound || !compactFound); index-- {
				event := logical[index].Event
				if !reviewFound {
					reviewFound = projectLatestResumeReviewMarker(event, projection)
				}
				if !compactFound {
					compactFound = projectLatestResumeCompactMarker(event, projection)
				}
			}
			earliest := logical[0].Event.Sequence
			if earliest <= lowerBound+1 {
				break
			}
			segmentCursor = earliest - 1
		}
		if len(rollbacks) == 0 {
			break
		}
		fromSequence, ok := safeInt64Payload(rollbacks[0].Event.Payload, "fromSequence")
		if !ok || fromSequence <= 0 || fromSequence >= rollbacks[0].Event.Sequence {
			return problem.New(409, "rollback_target_stale", "The rollback event contains an invalid source sequence.")
		}
		cursor = fromSequence - 1
	}
	return nil
}

func projectLatestResumeReviewMarker(
	event persistence.SessionEvent,
	projection *resumeSnapshotProjection,
) bool {
	if event.EventType != "item.started" && event.EventType != "item.updated" && event.EventType != "item.completed" {
		return false
	}
	switch stringPayload(event.Payload, "itemType") {
	case "review_entered":
		projection.Review = true
	case "review_exited":
		projection.Review = false
	default:
		return false
	}
	sequence := event.Sequence
	projection.ReviewSequence = &sequence
	return true
}

func projectLatestResumeCompactMarker(
	event persistence.SessionEvent,
	projection *resumeSnapshotProjection,
) bool {
	sequence := event.Sequence
	summary := ""
	switch event.EventType {
	case "item.started", "item.updated", "item.completed":
		if stringPayload(event.Payload, "itemType") != "context_compaction" ||
			(event.EventType != "item.completed" && stringPayload(event.Payload, "status") != "completed") {
			return false
		}
		summary = firstResumeString(event.Payload, "detail", "title")
	case "thread.state.changed":
		if stringPayload(event.Payload, "state") != "compacted" {
			return false
		}
		if value, ok := safeInt64Payload(event.Payload, "compactedThroughSequence"); ok {
			sequence = value
		}
		summary = strings.TrimSpace(stringPayload(event.Payload, "summary"))
	default:
		return false
	}
	projection.CompactBoundary = &ResumeCompactBoundary{
		Sequence: sequence,
		Summary:  truncateResumeText(summary, resumeSnapshotCompactSummaryByteLimit),
	}
	return true
}

func appendResumeUserMessage(messages *[]ResumeMessage, event persistence.SessionEvent) {
	text := strings.TrimSpace(stringPayload(event.Payload, "inputText"))
	if text == "" {
		return
	}
	*messages = append(*messages, ResumeMessage{
		Role:            "user",
		Text:            text,
		SequenceFrom:    event.Sequence,
		SequenceThrough: event.Sequence,
	})
}

func appendResumeAssistantDelta(messages *[]ResumeMessage, sequence int64, delta string) {
	if delta == "" {
		return
	}
	items := *messages
	if len(items) > 0 && items[len(items)-1].Role == "assistant" {
		items[len(items)-1].Text += delta
		items[len(items)-1].SequenceThrough = sequence
		*messages = items
		return
	}
	*messages = append(items, ResumeMessage{
		Role:            "assistant",
		Text:            delta,
		SequenceFrom:    sequence,
		SequenceThrough: sequence,
	})
}

func normalizeResumeMessages(messages []ResumeMessage) []ResumeMessage {
	result := make([]ResumeMessage, 0, len(messages))
	for _, message := range messages {
		message.Text = strings.TrimSpace(message.Text)
		if message.Text == "" {
			continue
		}
		result = append(result, message)
	}
	return result
}

func projectResumeItemEvent(projection *resumeSnapshotProjection, event persistence.SessionEvent) {
	itemType := stringPayload(event.Payload, "itemType")
	switch itemType {
	case "review_entered":
		projection.Review = true
		sequence := event.Sequence
		projection.ReviewSequence = &sequence
	case "review_exited":
		projection.Review = false
		sequence := event.Sequence
		projection.ReviewSequence = &sequence
	case "context_compaction":
		if event.EventType == "item.completed" || stringPayload(event.Payload, "status") == "completed" {
			summary := strings.TrimSpace(stringPayload(event.Payload, "detail"))
			if summary == "" {
				summary = strings.TrimSpace(stringPayload(event.Payload, "title"))
			}
			projection.CompactBoundary = &ResumeCompactBoundary{
				Sequence: event.Sequence,
				Summary:  truncateResumeText(summary, resumeSnapshotCompactSummaryByteLimit),
			}
		}
	default:
		if event.EventType != "item.completed" || !resumeToolItemType(itemType) {
			return
		}
		summary := strings.TrimSpace(stringPayload(event.Payload, "detail"))
		title := strings.TrimSpace(stringPayload(event.Payload, "title"))
		if summary == "" {
			summary = title
		}
		if summary == "" {
			return
		}
		projection.ToolResults = append(projection.ToolResults, ResumeToolResult{
			Sequence: event.Sequence,
			Kind:     itemType,
			Title:    truncateResumeText(title, 1024),
			Summary:  truncateResumeText(summary, resumeSnapshotToolSummaryByteLimit),
		})
	}
}

func resumeToolItemType(itemType string) bool {
	switch itemType {
	case "command_execution", "file_change", "mcp_tool_call", "dynamic_tool_call",
		"collab_agent_tool_call", "web_search", "image_view", "image_generation":
		return true
	default:
		return false
	}
}

func loadResumeArtifactReferences(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	events []resumeArtifactEvent,
) ([]ResumeArtifactReference, error) {
	if len(events) == 0 {
		return make([]ResumeArtifactReference, 0), nil
	}
	ids := make([]uuid.UUID, 0, len(events))
	for _, event := range events {
		ids = append(ids, event.ArtifactID)
	}
	models := make([]persistence.Artifact, 0, len(ids))
	if err := tx.WithContext(ctx).
		Where("tenant_id = ? AND id IN ? AND status = ? AND deleted_at IS NULL",
			execution.TenantID, ids, "ready").
		Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "execution_history_artifacts_load_failed", "Failed to load authoritative Artifact references.", err)
	}
	byID := make(map[uuid.UUID]persistence.Artifact, len(models))
	for _, model := range models {
		byID[model.ID] = model
	}
	result := make([]ResumeArtifactReference, 0, len(models))
	for _, event := range events {
		model, ok := byID[event.ArtifactID]
		if !ok {
			continue
		}
		result = append(result, ResumeArtifactReference{
			Sequence:    event.Sequence,
			ArtifactID:  model.ID,
			ExecutionID: cloneUUIDPointer(model.ExecutionID),
			Kind:        model.Kind,
			ContentType: cloneStringPointer(model.ContentType),
			SizeBytes:   cloneInt64Pointer(model.SizeBytes),
			SHA256:      cloneStringPointer(model.SHA256),
		})
	}
	return result, nil
}

func (s *Service) loadResumePendingInteractions(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
) ([]ResumePendingInteraction, []string, error) {
	models := make([]persistence.ExecutionInteraction, 0, resumeSnapshotPendingInteractionLimit+1)
	if err := tx.WithContext(ctx).
		Where("tenant_id = ? AND session_id = ? AND status = ? AND expires_at > ?",
			execution.TenantID, execution.SessionID, "pending", s.now()).
		Order("requested_at DESC, id DESC").Limit(resumeSnapshotPendingInteractionLimit + 1).
		Find(&models).Error; err != nil {
		return nil, nil, problem.Wrap(500, "execution_history_interactions_load_failed", "Failed to load pending Session interactions for resume.", err)
	}
	reasons := make([]string, 0)
	if len(models) > resumeSnapshotPendingInteractionLimit {
		models = models[:resumeSnapshotPendingInteractionLimit]
		reasons = append(reasons, "pending_interaction_limit")
	}
	sort.Slice(models, func(left, right int) bool {
		if models[left].RequestedAt.Equal(models[right].RequestedAt) {
			return models[left].ID.String() < models[right].ID.String()
		}
		return models[left].RequestedAt.Before(models[right].RequestedAt)
	})
	result := make([]ResumePendingInteraction, 0, len(models))
	for _, model := range models {
		item, truncated := resumePendingInteraction(model)
		if truncated {
			reasons = appendUniqueString(reasons, "interaction_metadata_limit")
		}
		result = append(result, item)
	}
	return result, reasons, nil
}

func resumePendingInteraction(model persistence.ExecutionInteraction) (ResumePendingInteraction, bool) {
	item := ResumePendingInteraction{
		ID:           model.ID,
		ExecutionID:  model.ExecutionID,
		TurnID:       model.TurnID,
		Provider:     truncateResumeText(strings.TrimSpace(model.Provider), 160),
		RequestID:    truncateResumeText(strings.TrimSpace(model.RequestID), 200),
		EventVersion: model.EventVersion,
		Kind:         model.Kind,
		RequestedAt:  model.RequestedAt,
		ExpiresAt:    model.ExpiresAt,
	}
	truncated := false
	item.RequestType, truncated = truncatedPayloadString(model.Payload, "requestType", 160, truncated)
	for _, key := range []string{"detail", "prompt", "title", "description"} {
		if item.Detail != "" {
			break
		}
		item.Detail, truncated = truncatedPayloadString(model.Payload, key, 4096, truncated)
	}
	questions, found := anySlice(model.Payload["questions"])
	if !found {
		return item, truncated
	}
	if len(questions) > 4 {
		questions = questions[:4]
		truncated = true
	}
	item.Questions = make([]ResumeInteractionQuestion, 0, len(questions))
	for _, rawQuestion := range questions {
		questionMap, ok := anyMap(rawQuestion)
		if !ok {
			continue
		}
		question := ResumeInteractionQuestion{}
		question.ID, truncated = truncatedPayloadString(questionMap, "id", 256, truncated)
		question.Header, truncated = truncatedPayloadString(questionMap, "header", 512, truncated)
		question.Question, truncated = truncatedPayloadString(questionMap, "question", 2048, truncated)
		question.MultiSelect, _ = questionMap["multiSelect"].(bool)
		options, _ := anySlice(questionMap["options"])
		if len(options) > 6 {
			options = options[:6]
			truncated = true
		}
		question.Options = make([]ResumeInteractionOption, 0, len(options))
		for _, rawOption := range options {
			optionMap, ok := anyMap(rawOption)
			if !ok {
				continue
			}
			option := ResumeInteractionOption{}
			option.Label, truncated = truncatedPayloadString(optionMap, "label", 512, truncated)
			option.Description, truncated = truncatedPayloadString(optionMap, "description", 1024, truncated)
			question.Options = append(question.Options, option)
		}
		item.Questions = append(item.Questions, question)
	}
	return item, truncated
}

func resumeWorkspaceReference(input resumeSnapshotContext) *ResumeWorkspaceReference {
	if input.RemoteWorkspaceID == nil {
		return nil
	}
	reference := &ResumeWorkspaceReference{
		WorkspaceID:                  *input.RemoteWorkspaceID,
		MaterializationID:            cloneUUIDPointer(input.WorkspaceMaterializationID),
		MaterializationIncarnationID: cloneUUIDPointer(input.WorkspaceMaterializationIncarnationID),
		LayoutVersion:                input.WorkspaceLayoutVersion,
		RepositoryFingerprint:        cloneStringPointer(input.WorkspaceRepositoryFingerprint),
		DefaultBranch:                input.WorkspaceDefaultBranch,
		CurrentBranch:                cloneStringPointer(input.WorkspaceCurrentBranch),
		BaseCommit:                   cloneStringPointer(input.WorkspaceBaseCommit),
		HeadCommit:                   cloneStringPointer(input.WorkspaceHeadCommit),
	}
	if input.Checkpoint != nil {
		reference.Checkpoint = &ResumeCheckpointReference{
			CheckpointID:  input.Checkpoint.ID,
			Strategy:      input.Checkpoint.Strategy,
			ArtifactID:    cloneUUIDPointer(input.Checkpoint.ArtifactID),
			BaseCommit:    cloneStringPointer(input.Checkpoint.BaseCommit),
			HeadCommit:    cloneStringPointer(input.Checkpoint.HeadCommit),
			CurrentBranch: cloneStringPointer(input.Checkpoint.CurrentBranch),
			SHA256:        cloneStringPointer(input.Checkpoint.SHA256),
		}
	}
	return reference
}

func fitResumeSnapshotBudget(snapshot *ResumeSnapshot) error {
	for {
		recomputeIncludedSequenceRange(snapshot)
		usedBytes, estimatedTokens, err := refreshResumeSnapshotBudget(snapshot)
		if err != nil {
			return problem.Wrap(500, "resume_snapshot_encode_failed", "Failed to encode the authoritative Resume Snapshot.", err)
		}
		overBytes := usedBytes > snapshot.Budget.ByteLimit
		overTokens := estimatedTokens > snapshot.Budget.TokenLimit
		if !overBytes && !overTokens {
			return nil
		}
		if overBytes {
			addResumeTruncationReason(snapshot, "byte_budget")
		}
		if overTokens {
			addResumeTruncationReason(snapshot, "token_budget")
		}
		if dropOldestResumeContext(snapshot) {
			continue
		}
		if stripResumeInteractionMetadata(snapshot) {
			addResumeTruncationReason(snapshot, "interaction_metadata_budget")
			continue
		}
		if snapshot.CompactBoundary != nil && snapshot.CompactBoundary.Summary != "" {
			snapshot.CompactBoundary.Summary = ""
			addResumeTruncationReason(snapshot, "compact_summary_budget")
			continue
		}
		if len(snapshot.PendingInteractions) > 1 {
			snapshot.PendingInteractions = snapshot.PendingInteractions[1:]
			addResumeTruncationReason(snapshot, "pending_interaction_budget")
			continue
		}
		return problem.New(500, "resume_snapshot_budget_exhausted", "The authoritative Resume Snapshot metadata exceeds its fixed budget.")
	}
}

func dropOldestResumeContext(snapshot *ResumeSnapshot) bool {
	if len(snapshot.Messages) == 1 && len(snapshot.ToolResults) == 0 && len(snapshot.ArtifactReferences) == 0 {
		message := &snapshot.Messages[0]
		if len(message.Text) > 1024 {
			overBytes := snapshot.Budget.UsedBytes - snapshot.Budget.ByteLimit
			if tokenBytes := (snapshot.Budget.EstimatedTokens - snapshot.Budget.TokenLimit) * 4; tokenBytes > overBytes {
				overBytes = tokenBytes
			}
			maximumBytes := len(message.Text) - overBytes - 256
			if maximumBytes >= len(message.Text) {
				maximumBytes = len(message.Text) / 2
			}
			if maximumBytes < 1024 {
				maximumBytes = 1024
			}
			message.Text = truncateResumeTextSuffix(message.Text, maximumBytes)
			addResumeTruncationReason(snapshot, "message_text_budget")
			return true
		}
	}
	type candidate struct {
		kind     int
		sequence int64
	}
	candidates := make([]candidate, 0, 3)
	if len(snapshot.Messages) > 0 {
		candidates = append(candidates, candidate{kind: 0, sequence: snapshot.Messages[0].SequenceFrom})
	}
	if len(snapshot.ToolResults) > 0 {
		candidates = append(candidates, candidate{kind: 1, sequence: snapshot.ToolResults[0].Sequence})
	}
	if len(snapshot.ArtifactReferences) > 0 {
		candidates = append(candidates, candidate{kind: 2, sequence: snapshot.ArtifactReferences[0].Sequence})
	}
	if len(candidates) == 0 {
		return false
	}
	sort.Slice(candidates, func(left, right int) bool {
		if candidates[left].sequence == candidates[right].sequence {
			return candidates[left].kind < candidates[right].kind
		}
		return candidates[left].sequence < candidates[right].sequence
	})
	selected := candidates[0]
	droppedThrough := selected.sequence
	switch selected.kind {
	case 0:
		droppedThrough = snapshot.Messages[0].SequenceThrough
		snapshot.Messages = snapshot.Messages[1:]
	case 1:
		snapshot.ToolResults = snapshot.ToolResults[1:]
	case 2:
		snapshot.ArtifactReferences = snapshot.ArtifactReferences[1:]
	}
	setResumeDroppedBefore(snapshot, droppedThrough)
	return true
}

func stripResumeInteractionMetadata(snapshot *ResumeSnapshot) bool {
	for index := range snapshot.PendingInteractions {
		interaction := &snapshot.PendingInteractions[index]
		if interaction.Detail == "" && interaction.RequestType == "" && len(interaction.Questions) == 0 {
			continue
		}
		interaction.Detail = ""
		interaction.RequestType = ""
		interaction.Questions = nil
		return true
	}
	return false
}

func refreshResumeSnapshotBudget(snapshot *ResumeSnapshot) (int, int, error) {
	for iteration := 0; iteration < 8; iteration++ {
		encoded, err := json.Marshal(snapshot)
		if err != nil {
			return 0, 0, err
		}
		usedBytes := len(encoded)
		estimatedTokens := (usedBytes + 3) / 4
		if snapshot.Budget.UsedBytes == usedBytes && snapshot.Budget.EstimatedTokens == estimatedTokens {
			return usedBytes, estimatedTokens, nil
		}
		snapshot.Budget.UsedBytes = usedBytes
		snapshot.Budget.EstimatedTokens = estimatedTokens
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return 0, 0, err
	}
	return len(encoded), (len(encoded) + 3) / 4, nil
}

func recomputeIncludedSequenceRange(snapshot *ResumeSnapshot) {
	minimum, maximum := int64(0), int64(0)
	include := func(from, through int64) {
		if from <= 0 || through <= 0 {
			return
		}
		if minimum == 0 || from < minimum {
			minimum = from
		}
		if through > maximum {
			maximum = through
		}
	}
	for _, message := range snapshot.Messages {
		include(message.SequenceFrom, message.SequenceThrough)
	}
	for _, result := range snapshot.ToolResults {
		include(result.Sequence, result.Sequence)
	}
	for _, reference := range snapshot.ArtifactReferences {
		include(reference.Sequence, reference.Sequence)
	}
	if snapshot.Mode.ReviewSequence != nil {
		include(*snapshot.Mode.ReviewSequence, *snapshot.Mode.ReviewSequence)
	}
	if snapshot.CompactBoundary != nil {
		include(snapshot.CompactBoundary.Sequence, snapshot.CompactBoundary.Sequence)
	}
	if minimum == 0 {
		snapshot.IncludedSequenceRange = nil
		return
	}
	snapshot.IncludedSequenceRange = &ResumeSequenceRange{From: minimum, Through: maximum}
}

func addResumeTruncationReason(snapshot *ResumeSnapshot, reason string) {
	if snapshot.Truncation == nil {
		snapshot.Truncation = &ResumeSnapshotTruncation{Reasons: make([]string, 0, 1)}
	}
	snapshot.Truncation.Reasons = appendUniqueString(snapshot.Truncation.Reasons, reason)
}

func setResumeDroppedBefore(snapshot *ResumeSnapshot, sequence int64) {
	if sequence <= 0 {
		return
	}
	if snapshot.Truncation == nil {
		snapshot.Truncation = &ResumeSnapshotTruncation{Reasons: make([]string, 0)}
	}
	if snapshot.Truncation.DroppedBeforeSequence == nil || sequence > *snapshot.Truncation.DroppedBeforeSequence {
		value := sequence
		snapshot.Truncation.DroppedBeforeSequence = &value
	}
}

func (s *Service) advanceAuthoritativeHistorySequence(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	sequence int64,
) error {
	if execution.ProviderRuntimeBindingID == nil {
		return nil
	}
	result := tx.WithContext(ctx).Model(&persistence.ProviderRuntimeBinding{}).
		Where("tenant_id = ? AND id = ? AND session_id = ?",
			execution.TenantID, *execution.ProviderRuntimeBindingID, execution.SessionID).
		Updates(map[string]any{
			"authoritative_history_sequence": gorm.Expr(
				"CASE WHEN authoritative_history_sequence < ? THEN ? ELSE authoritative_history_sequence END",
				sequence, sequence,
			),
			"updated_at": s.now(),
		})
	if result.Error != nil {
		return problem.Wrap(500, "runtime_binding_history_sequence_update_failed", "Failed to advance the authoritative Session history cursor.", result.Error)
	}
	if result.RowsAffected != 1 {
		return problem.New(500, "runtime_binding_history_sequence_update_failed", "The Provider runtime binding was not available for authoritative Session history.")
	}
	return nil
}

func conversationHistoryFromResumeSnapshot(snapshot ResumeSnapshot) []ConversationMessage {
	result := make([]ConversationMessage, 0, len(snapshot.Messages))
	for _, message := range snapshot.Messages {
		result = append(result, ConversationMessage{Role: message.Role, Text: message.Text})
	}
	return result
}

func stringPayload(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return value
}

func stringSlicePayload(payload map[string]any, key string) []string {
	values, ok := anySlice(payload[key])
	if !ok {
		if typed, typedOK := payload[key].([]string); typedOK {
			return append([]string(nil), typed...)
		}
		return nil
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		text, ok := value.(string)
		if ok && strings.TrimSpace(text) != "" {
			result = append(result, text)
		}
	}
	return result
}

func uuidPayload(payload map[string]any, key string) (uuid.UUID, bool) {
	switch value := payload[key].(type) {
	case uuid.UUID:
		return value, value != uuid.Nil
	case string:
		parsed, err := uuid.Parse(strings.TrimSpace(value))
		return parsed, err == nil && parsed != uuid.Nil
	default:
		return uuid.Nil, false
	}
}

func anySlice(value any) ([]any, bool) {
	switch typed := value.(type) {
	case []any:
		return typed, true
	case []map[string]any:
		result := make([]any, 0, len(typed))
		for _, item := range typed {
			result = append(result, item)
		}
		return result, true
	default:
		return nil, false
	}
}

func anyMap(value any) (map[string]any, bool) {
	result, ok := value.(map[string]any)
	return result, ok && result != nil
}

func truncatedPayloadString(payload map[string]any, key string, maximumBytes int, alreadyTruncated bool) (string, bool) {
	value := strings.TrimSpace(stringPayload(payload, key))
	truncated := truncateResumeText(value, maximumBytes)
	return truncated, alreadyTruncated || truncated != value
}

func truncateResumeText(value string, maximumBytes int) string {
	if maximumBytes <= 0 || len(value) <= maximumBytes {
		return value
	}
	value = value[:maximumBytes]
	for !utf8.ValidString(value) && len(value) > 0 {
		value = value[:len(value)-1]
	}
	return value
}

func truncateResumeTextSuffix(value string, maximumBytes int) string {
	if maximumBytes <= 0 || len(value) <= maximumBytes {
		return value
	}
	value = value[len(value)-maximumBytes:]
	for !utf8.ValidString(value) && len(value) > 0 {
		value = value[1:]
	}
	return value
}

func appendUniqueStrings(values []string, additions ...string) []string {
	for _, addition := range additions {
		values = appendUniqueString(values, addition)
	}
	return values
}

func appendUniqueString(values []string, addition string) []string {
	if addition == "" {
		return values
	}
	for _, value := range values {
		if value == addition {
			return values
		}
	}
	return append(values, addition)
}

func cloneStringPointer(value *string) *string {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneInt64Pointer(value *int64) *int64 {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneUUIDPointer(value *uuid.UUID) *uuid.UUID {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
