package httpapi

import (
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

// developerExecution is the stable external projection. Worker fencing,
// placement policy, runtime-binding, workspace, and scheduling authority fields
// remain internal even when a mutation returns the affected Execution.
type developerExecution struct {
	ID                uuid.UUID  `json:"id"`
	SessionID         uuid.UUID  `json:"sessionId"`
	TurnID            uuid.UUID  `json:"turnId"`
	Attempt           int        `json:"attempt"`
	Status            string     `json:"status"`
	ExecutionTargetID uuid.UUID  `json:"executionTargetId"`
	TargetKind        string     `json:"targetKind"`
	Provider          *string    `json:"provider"`
	QueuedAt          time.Time  `json:"queuedAt"`
	StartedAt         *time.Time `json:"startedAt"`
	FinishedAt        *time.Time `json:"finishedAt"`
	FailureCode       *string    `json:"failureCode"`
}

func projectDeveloperExecution(item executions.Execution) developerExecution {
	return developerExecution{
		ID: item.ID, SessionID: item.SessionID, TurnID: item.TurnID,
		Attempt: item.Attempt, Status: item.Status,
		ExecutionTargetID: item.ExecutionTargetID, TargetKind: item.TargetKind, Provider: item.Provider,
		QueuedAt: item.QueuedAt, StartedAt: item.StartedAt, FinishedAt: item.FinishedAt,
		FailureCode: item.FailureCode,
	}
}

type developerControlCommand struct {
	ID          uuid.UUID `json:"id"`
	ExecutionID uuid.UUID `json:"executionId"`
	SessionID   uuid.UUID `json:"sessionId"`
	TurnID      uuid.UUID `json:"turnId"`
	Provider    string    `json:"provider"`
	CommandType string    `json:"commandType"`
	Status      string    `json:"status"`
	RequestedAt time.Time `json:"requestedAt"`
}

func projectDeveloperControlCommand(item executions.ControlCommand) developerControlCommand {
	return developerControlCommand{
		ID: item.ID, ExecutionID: item.ExecutionID, SessionID: item.SessionID, TurnID: item.TurnID,
		Provider: item.Provider, CommandType: item.CommandType, Status: item.Status, RequestedAt: item.RequestedAt,
	}
}

type developerQueuedSessionOperation struct {
	Type           string                  `json:"type"`
	Turn           sessions.Turn           `json:"turn"`
	ExecutionID    uuid.UUID               `json:"executionId"`
	ControlCommand developerControlCommand `json:"controlCommand"`
}

func projectDeveloperQueuedSessionOperation(item executions.QueuedSessionOperation) developerQueuedSessionOperation {
	return developerQueuedSessionOperation{
		Type: item.Type, Turn: item.Turn, ExecutionID: item.ExecutionID,
		ControlCommand: projectDeveloperControlCommand(item.ControlCommand),
	}
}
