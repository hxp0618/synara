package sessions

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const maximumForkLineageDepth = 32

type LogicalEvent struct {
	Event           persistence.SessionEvent
	OriginSessionID uuid.UUID
}

func requireForkLineageExtendable(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, sourceSessionID uuid.UUID,
) error {
	visited := make(map[uuid.UUID]struct{}, maximumForkLineageDepth)
	currentID := sourceSessionID
	depth := 0
	for {
		if _, cycle := visited[currentID]; cycle {
			return problem.New(409, "fork_lineage_cycle", "Session Fork lineage contains a cycle.")
		}
		visited[currentID] = struct{}{}
		var current persistence.AgentSession
		if err := tx.WithContext(ctx).
			Select("id", "tenant_id", "fork_source_session_id").
			Where("tenant_id = ? AND id = ?", tenantID, currentID).
			Take(&current).Error; err != nil {
			return problem.Wrap(404, "session_not_found", "Session Fork lineage could not be loaded.", err)
		}
		if current.ForkSourceSessionID == nil {
			return nil
		}
		depth++
		if depth >= maximumForkLineageDepth-1 {
			return problem.New(409, "fork_lineage_too_deep", "Session Fork lineage cannot be extended beyond the supported depth.")
		}
		currentID = *current.ForkSourceSessionID
	}
}

func LoadLogicalEvents(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, sessionID uuid.UUID,
	throughSequence int64,
) ([]LogicalEvent, error) {
	return loadLogicalEvents(ctx, tx, tenantID, sessionID, throughSequence, map[uuid.UUID]struct{}{}, 0)
}

func LoadLogicalEventsPage(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, sessionID uuid.UUID,
	afterSequence, throughSequence int64,
	limit int,
) ([]LogicalEvent, error) {
	if limit <= 0 || throughSequence <= afterSequence {
		return []LogicalEvent{}, nil
	}
	return loadLogicalEventsPage(
		ctx,
		tx,
		tenantID,
		sessionID,
		afterSequence,
		throughSequence,
		limit,
		map[uuid.UUID]struct{}{},
		0,
	)
}

func LoadLogicalEventsTail(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, sessionID uuid.UUID,
	afterSequence, throughSequence int64,
	limit int,
	eventTypes []string,
) ([]LogicalEvent, error) {
	if limit <= 0 || throughSequence <= afterSequence {
		return []LogicalEvent{}, nil
	}
	descending, err := loadLogicalEventsTail(
		ctx,
		tx,
		tenantID,
		sessionID,
		afterSequence,
		throughSequence,
		limit,
		eventTypes,
		map[uuid.UUID]struct{}{},
		0,
	)
	if err != nil {
		return nil, err
	}
	for left, right := 0, len(descending)-1; left < right; left, right = left+1, right-1 {
		descending[left], descending[right] = descending[right], descending[left]
	}
	return descending, nil
}

func loadLogicalEventsTail(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, sessionID uuid.UUID,
	afterSequence, throughSequence int64,
	limit int,
	eventTypes []string,
	visited map[uuid.UUID]struct{},
	depth int,
) ([]LogicalEvent, error) {
	if limit <= 0 || throughSequence <= afterSequence {
		return []LogicalEvent{}, nil
	}
	if depth >= maximumForkLineageDepth {
		return nil, problem.New(409, "fork_lineage_too_deep", "Session Fork lineage exceeds the supported depth.")
	}
	if _, cycle := visited[sessionID]; cycle {
		return nil, problem.New(409, "fork_lineage_cycle", "Session Fork lineage contains a cycle.")
	}
	visited[sessionID] = struct{}{}
	defer delete(visited, sessionID)

	var session persistence.AgentSession
	if err := tx.WithContext(ctx).
		Select("id", "tenant_id", "fork_source_session_id", "fork_source_event_sequence", "last_event_sequence").
		Where("tenant_id = ? AND id = ?", tenantID, sessionID).
		Take(&session).Error; err != nil {
		return nil, problem.Wrap(404, "session_not_found", "Session history could not be loaded.", err)
	}
	throughSequence = min(throughSequence, session.LastEventSequence)
	if throughSequence <= afterSequence {
		return []LogicalEvent{}, nil
	}
	prefixThrough := int64(0)
	if session.ForkSourceSessionID != nil && session.ForkSourceEventSequence != nil {
		prefixThrough = min(*session.ForkSourceEventSequence, throughSequence)
	}

	physicalAfter := max(afterSequence, prefixThrough)
	result := make([]LogicalEvent, 0, limit)
	if throughSequence > physicalAfter {
		models := make([]persistence.SessionEvent, 0, limit)
		query := tx.WithContext(ctx).
			Where("tenant_id = ? AND session_id = ? AND sequence > ? AND sequence <= ?",
				tenantID, sessionID, physicalAfter, throughSequence)
		if len(eventTypes) > 0 {
			query = query.Where("event_type IN ?", eventTypes)
		}
		if err := query.Order("sequence DESC").Limit(limit).Find(&models).Error; err != nil {
			return nil, problem.Wrap(500, "session_history_load_failed", "Session history could not be loaded.", err)
		}
		for _, event := range models {
			result = append(result, LogicalEvent{Event: event, OriginSessionID: sessionID})
		}
		if len(result) == limit {
			return result, nil
		}
	}

	if session.ForkSourceSessionID == nil || afterSequence >= prefixThrough {
		return result, nil
	}
	prefix, err := loadLogicalEventsTail(
		ctx,
		tx,
		tenantID,
		*session.ForkSourceSessionID,
		afterSequence,
		prefixThrough,
		limit-len(result),
		eventTypes,
		visited,
		depth+1,
	)
	if err != nil {
		return nil, err
	}
	return append(result, prefix...), nil
}

func loadLogicalEventsPage(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, sessionID uuid.UUID,
	afterSequence, throughSequence int64,
	limit int,
	visited map[uuid.UUID]struct{},
	depth int,
) ([]LogicalEvent, error) {
	if limit <= 0 || throughSequence <= afterSequence {
		return []LogicalEvent{}, nil
	}
	if depth >= maximumForkLineageDepth {
		return nil, problem.New(409, "fork_lineage_too_deep", "Session Fork lineage exceeds the supported depth.")
	}
	if _, cycle := visited[sessionID]; cycle {
		return nil, problem.New(409, "fork_lineage_cycle", "Session Fork lineage contains a cycle.")
	}
	visited[sessionID] = struct{}{}
	defer delete(visited, sessionID)

	var session persistence.AgentSession
	if err := tx.WithContext(ctx).
		Select("id", "tenant_id", "fork_source_session_id", "fork_source_event_sequence", "last_event_sequence").
		Where("tenant_id = ? AND id = ?", tenantID, sessionID).
		Take(&session).Error; err != nil {
		return nil, problem.Wrap(404, "session_not_found", "Session history could not be loaded.", err)
	}
	if throughSequence > session.LastEventSequence {
		throughSequence = session.LastEventSequence
	}
	if throughSequence <= afterSequence {
		return []LogicalEvent{}, nil
	}

	result := make([]LogicalEvent, 0, limit)
	prefixThrough := int64(0)
	if session.ForkSourceSessionID != nil && session.ForkSourceEventSequence != nil {
		prefixThrough = min(*session.ForkSourceEventSequence, throughSequence)
		if afterSequence < prefixThrough {
			prefix, err := loadLogicalEventsPage(
				ctx,
				tx,
				tenantID,
				*session.ForkSourceSessionID,
				afterSequence,
				prefixThrough,
				limit,
				visited,
				depth+1,
			)
			if err != nil {
				return nil, err
			}
			result = append(result, prefix...)
			if len(result) == limit {
				return result, nil
			}
		}
	}

	physicalAfter := max(afterSequence, prefixThrough)
	if throughSequence <= physicalAfter {
		return result, nil
	}
	models := make([]persistence.SessionEvent, 0, limit-len(result))
	if err := tx.WithContext(ctx).
		Where("tenant_id = ? AND session_id = ? AND sequence > ? AND sequence <= ?",
			tenantID, sessionID, physicalAfter, throughSequence).
		Order("sequence").
		Limit(limit - len(result)).
		Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "session_history_load_failed", "Session history could not be loaded.", err)
	}
	for _, event := range models {
		result = append(result, LogicalEvent{Event: event, OriginSessionID: sessionID})
	}
	return result, nil
}

func loadLogicalEvents(
	ctx context.Context,
	tx *gorm.DB,
	tenantID, sessionID uuid.UUID,
	throughSequence int64,
	visited map[uuid.UUID]struct{},
	depth int,
) ([]LogicalEvent, error) {
	if throughSequence <= 0 {
		return []LogicalEvent{}, nil
	}
	if depth >= maximumForkLineageDepth {
		return nil, problem.New(409, "fork_lineage_too_deep", "Session Fork lineage exceeds the supported depth.")
	}
	if _, cycle := visited[sessionID]; cycle {
		return nil, problem.New(409, "fork_lineage_cycle", "Session Fork lineage contains a cycle.")
	}
	visited[sessionID] = struct{}{}
	defer delete(visited, sessionID)

	var session persistence.AgentSession
	if err := tx.WithContext(ctx).
		Select("id", "tenant_id", "fork_source_session_id", "fork_source_event_sequence", "last_event_sequence").
		Where("tenant_id = ? AND id = ?", tenantID, sessionID).Take(&session).Error; err != nil {
		return nil, problem.Wrap(404, "session_not_found", "Session history could not be loaded.", err)
	}
	if throughSequence > session.LastEventSequence {
		throughSequence = session.LastEventSequence
	}
	result := make([]LogicalEvent, 0)
	prefixThrough := int64(0)
	if session.ForkSourceSessionID != nil && session.ForkSourceEventSequence != nil {
		prefixThrough = *session.ForkSourceEventSequence
		if prefixThrough > throughSequence {
			prefixThrough = throughSequence
		}
		prefix, err := loadLogicalEvents(
			ctx, tx, tenantID, *session.ForkSourceSessionID, prefixThrough, visited, depth+1,
		)
		if err != nil {
			return nil, err
		}
		result = append(result, prefix...)
	}
	if throughSequence > prefixThrough {
		models := make([]persistence.SessionEvent, 0)
		if err := tx.WithContext(ctx).
			Where("tenant_id = ? AND session_id = ? AND sequence > ? AND sequence <= ?",
				tenantID, sessionID, prefixThrough, throughSequence).
			Order("sequence").Find(&models).Error; err != nil {
			return nil, problem.Wrap(500, "session_history_load_failed", "Session history could not be loaded.", err)
		}
		for _, event := range models {
			result = append(result, LogicalEvent{Event: event, OriginSessionID: sessionID})
		}
	}
	sort.SliceStable(result, func(left, right int) bool {
		if result[left].Event.Sequence == result[right].Event.Sequence {
			return result[left].Event.EventID.String() < result[right].Event.EventID.String()
		}
		return result[left].Event.Sequence < result[right].Event.Sequence
	})
	return result, nil
}

func EffectiveLogicalEvents(events []LogicalEvent) ([]LogicalEvent, error) {
	result := make([]LogicalEvent, 0, len(events))
	for _, logical := range events {
		if logical.Event.EventType != "session.history.rolled-back" {
			result = append(result, logical)
			continue
		}
		fromSessionID, err := logicalPayloadUUID(logical.Event.Payload, "fromSessionId")
		if err != nil {
			return nil, fmt.Errorf("invalid rollback fromSessionId at sequence %d: %w", logical.Event.Sequence, err)
		}
		fromTurnID, err := logicalPayloadUUID(logical.Event.Payload, "fromTurnId")
		if err != nil {
			return nil, fmt.Errorf("invalid rollback fromTurnId at sequence %d: %w", logical.Event.Sequence, err)
		}
		cut := -1
		for index, candidate := range result {
			if candidate.OriginSessionID != fromSessionID || candidate.Event.EventType != "turn.created" {
				continue
			}
			turnID, turnErr := logicalPayloadUUID(candidate.Event.Payload, "turnId")
			if turnErr == nil && turnID == fromTurnID {
				cut = index
				break
			}
		}
		if cut < 0 {
			return nil, problem.New(409, "rollback_target_stale", "The rollback target is no longer part of the logical Session history.")
		}
		result = append(result[:cut], logical)
	}
	return result, nil
}

func logicalPayloadUUID(payload map[string]any, key string) (uuid.UUID, error) {
	value, ok := payload[key]
	if !ok {
		return uuid.Nil, fmt.Errorf("%s is missing", key)
	}
	switch typed := value.(type) {
	case uuid.UUID:
		if typed == uuid.Nil {
			return uuid.Nil, fmt.Errorf("%s is empty", key)
		}
		return typed, nil
	case string:
		parsed, err := uuid.Parse(typed)
		if err != nil || parsed == uuid.Nil {
			return uuid.Nil, fmt.Errorf("%s is invalid", key)
		}
		return parsed, nil
	default:
		return uuid.Nil, fmt.Errorf("%s has an invalid type", key)
	}
}
