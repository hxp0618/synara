package agentd

import (
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

type RunnerInput struct {
	Execution            executions.Execution `json:"execution"`
	Workload             executions.Workload  `json:"workload"`
	ProviderResumeCursor *string              `json:"providerResumeCursor,omitempty"`
	WorkspaceDirectory   string               `json:"workspaceDirectory"`
}

type RunnerCredential struct {
	Payload map[string]any `json:"payload"`
}

type RunnerMessage struct {
	Type                 string          `json:"type"`
	EventID              *uuid.UUID      `json:"eventId,omitempty"`
	EventType            string          `json:"eventType,omitempty"`
	Payload              map[string]any  `json:"payload,omitempty"`
	OccurredAt           *time.Time      `json:"occurredAt,omitempty"`
	Artifact             *RunnerArtifact `json:"artifact,omitempty"`
	Output               map[string]any  `json:"output,omitempty"`
	ProviderResumeCursor *string         `json:"providerResumeCursor,omitempty"`
}

type RunnerArtifact struct {
	Path         string `json:"path"`
	Kind         string `json:"kind"`
	OriginalName string `json:"originalName,omitempty"`
	ContentType  string `json:"contentType"`
}

type RunnerResult struct {
	Output               map[string]any
	ProviderResumeCursor *string
}
