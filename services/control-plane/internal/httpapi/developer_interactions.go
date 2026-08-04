package httpapi

import (
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

// developerInteraction intentionally omits Worker fencing and resolution-delivery
// internals. Those fields are useful to first-party operators but are not part of
// the external developer contract and may contain infrastructure error details.
type developerInteraction struct {
	ID          uuid.UUID      `json:"id"`
	ExecutionID uuid.UUID      `json:"executionId"`
	SessionID   uuid.UUID      `json:"sessionId"`
	TurnID      uuid.UUID      `json:"turnId"`
	Provider    string         `json:"provider"`
	RequestID   string         `json:"requestId"`
	Kind        string         `json:"kind"`
	Status      string         `json:"status"`
	Payload     map[string]any `json:"payload"`
	Resolution  map[string]any `json:"resolution,omitempty"`
	RequestedAt time.Time      `json:"requestedAt"`
	ExpiresAt   time.Time      `json:"expiresAt"`
	ResolvedAt  *time.Time     `json:"resolvedAt"`
}

func projectDeveloperInteraction(item executions.Interaction) developerInteraction {
	return developerInteraction{
		ID: item.ID, ExecutionID: item.ExecutionID, SessionID: item.SessionID, TurnID: item.TurnID,
		Provider: item.Provider, RequestID: item.RequestID, Kind: item.Kind, Status: item.Status,
		Payload: item.Payload, Resolution: item.Resolution, RequestedAt: item.RequestedAt,
		ExpiresAt: item.ExpiresAt, ResolvedAt: item.ResolvedAt,
	}
}

func projectDeveloperInteractions(items []executions.Interaction) []developerInteraction {
	projected := make([]developerInteraction, 0, len(items))
	for _, item := range items {
		projected = append(projected, projectDeveloperInteraction(item))
	}
	return projected
}
