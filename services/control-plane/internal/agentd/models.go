package agentd

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

const runnerSuspendCheckpointProtocol = "provider-host-suspend-terminal-v1"

type RunnerInput struct {
	Execution              executions.Execution `json:"execution"`
	Workload               executions.Workload  `json:"workload"`
	MemoryDocuments        []MemoryDocument     `json:"memoryDocuments,omitempty"`
	ProviderResumeCursor   *string              `json:"providerResumeCursor,omitempty"`
	WorkspaceDirectory     string               `json:"workspaceDirectory"`
	ProviderStateDirectory string               `json:"providerStateDirectory,omitempty"`
	RuntimeOutputDirectory string               `json:"runtimeOutputDirectory,omitempty"`
	ProviderEnvironment    map[string]string    `json:"-"`
}

type MemoryDocument struct {
	Scope       string    `json:"scope"`
	ScopeID     uuid.UUID `json:"scopeId"`
	MemoryKey   string    `json:"memoryKey"`
	RevisionID  uuid.UUID `json:"revisionId"`
	ArtifactID  uuid.UUID `json:"artifactId"`
	SHA256      string    `json:"sha256"`
	ContentType string    `json:"contentType"`
	Content     string    `json:"content"`
}

type RunnerCredential struct {
	GrantID uuid.UUID                 `json:"grantId,omitempty"`
	Access  *ProviderCredentialAccess `json:"access,omitempty"`
	Payload map[string]any            `json:"payload"`
}

type ProviderCredentialAccess = executions.ProviderCredentialAccess

type GitHTTPSCredential struct {
	Host     string `json:"host"`
	Username string `json:"username"`
	Token    string `json:"token"`
}

type RunnerMessage struct {
	Type                 string          `json:"type"`
	EventID              *uuid.UUID      `json:"eventId,omitempty"`
	EventVersion         int             `json:"-"`
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
	SourceRoot   string `json:"sourceRoot,omitempty"`
	TerminalID   string `json:"terminalId,omitempty"`
	Encoding     string `json:"encoding,omitempty"`
	ReportedSize *int64 `json:"reportedSize,omitempty"`
	FileCount    *int64 `json:"fileCount,omitempty"`
	Additions    *int64 `json:"additions,omitempty"`
	Deletions    *int64 `json:"deletions,omitempty"`
}

type WorkspaceCheckpointCandidate struct {
	IdempotencyKey string
	Strategy       string
	BaseCommit     *string
	HeadCommit     *string
	CurrentBranch  *string
	Manifest       map[string]any
	FileCount      int
	TotalBytes     int64
	Artifact       *RunnerArtifact
	ArtifactPath   string
	Cleanup        func()
}

type RunnerResult struct {
	Output                 map[string]any
	ProviderResumeCursor   *string
	PrimaryOperationResult map[string]any
}

type RunnerPrimaryOperationControl struct {
	MarkDelivered func(context.Context) error
}

type RunnerControl struct {
	Command       RunnerControlCommand
	MarkDelivered func(context.Context) error
	Acknowledge   func(context.Context, map[string]any) error
	Done          chan<- error
	Err           error
}

type RunnerControlCommand struct {
	Provider    string
	CommandType string
	CommandID   string
	Payload     map[string]any
}

type runnerSuspended struct {
	TargetCommandID    string
	CheckpointProtocol string
}

func (e *runnerSuspended) Error() string { return "Provider turn was suspended." }

func runnerSuspendedTerminal(err error) (*runnerSuspended, bool) {
	var suspended *runnerSuspended
	if !errors.As(err, &suspended) {
		return nil, false
	}
	return suspended, true
}
