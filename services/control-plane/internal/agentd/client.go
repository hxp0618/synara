package agentd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/artifacts"
	"github.com/synara-ai/synara/services/control-plane/internal/executions"
)

type Client struct {
	baseURL           *url.URL
	http              *http.Client
	uploadHTTP        *http.Client
	registrationToken string
	workerToken       string
}

func NewClient(cfg Config) *Client {
	return &Client{
		baseURL: cfg.ControlPlaneURL, registrationToken: cfg.RegistrationToken,
		http: &http.Client{Timeout: cfg.RequestTimeout}, uploadHTTP: &http.Client{Timeout: cfg.ArtifactTimeout},
	}
}

func (c *Client) Register(ctx context.Context, cfg Config) (executions.RegisteredWorker, error) {
	var output executions.RegisteredWorker
	err := c.doJSON(ctx, http.MethodPost, "/v1/workers/register", c.registrationToken, "", executions.RegisterWorkerInput{
		ExecutionTargetID: cfg.ExecutionTargetID, TargetKind: string(cfg.TargetKind),
		ClusterID: cfg.ClusterID, Namespace: cfg.Namespace, PodName: cfg.PodName, InstanceUID: cfg.InstanceUID,
		Version: cfg.Version, ProtocolVersion: executions.WorkerProtocolVersion,
		Capabilities: cfg.Capabilities, LeaseSupported: true, FencingSupported: true,
	}, &output)
	if err != nil {
		return executions.RegisteredWorker{}, err
	}
	c.workerToken = output.Token
	return output, nil
}

func (c *Client) Heartbeat(ctx context.Context, cfg Config, draining bool) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/workers/heartbeat", c.workerToken, "", executions.HeartbeatInput{
		Version: cfg.Version, ProtocolVersion: executions.WorkerProtocolVersion, Capabilities: cfg.Capabilities,
		Draining: &draining,
	}, nil)
}

func (c *Client) Claim(ctx context.Context, cfg Config) (executions.ClaimResult, error) {
	var output executions.ClaimResult
	err := c.doJSON(ctx, http.MethodPost, "/v1/workers/executions/claim", c.workerToken, uuid.NewString(), executions.ClaimExecutionInput{
		ExecutionTargetID: cfg.ExecutionTargetID, TargetKind: string(cfg.TargetKind), ExecutionID: cfg.AssignedExecutionID,
	}, &output)
	return output, err
}

func (c *Client) ClaimWorkspaceCleanup(
	ctx context.Context,
	cfg Config,
) (executions.WorkspaceCleanupClaimResult, error) {
	var output executions.WorkspaceCleanupClaimResult
	err := c.doJSON(
		ctx,
		http.MethodPost,
		"/v1/workers/workspace-cleanups/claim",
		c.workerToken,
		uuid.NewString(),
		executions.WorkspaceCleanupClaimInput{
			ExecutionTargetID: cfg.ExecutionTargetID,
			TargetKind:        string(cfg.TargetKind),
		},
		&output,
	)
	return output, err
}

func (c *Client) RenewWorkspaceCleanup(
	ctx context.Context,
	claim executions.WorkspaceCleanupClaim,
) error {
	return c.workspaceCleanupRequest(ctx, claim, "renew", nil)
}

func (c *Client) StartWorkspaceCleanup(
	ctx context.Context,
	claim executions.WorkspaceCleanupClaim,
) error {
	return c.workspaceCleanupRequest(ctx, claim, "started", nil)
}

func (c *Client) AcknowledgeWorkspaceCleanup(
	ctx context.Context,
	claim executions.WorkspaceCleanupClaim,
) error {
	return c.workspaceCleanupRequest(ctx, claim, "acknowledged", nil)
}

func (c *Client) FailWorkspaceCleanup(
	ctx context.Context,
	claim executions.WorkspaceCleanupClaim,
	code, message string,
	retryable bool,
) error {
	input := executions.WorkspaceCleanupFailedInput{
		WorkspaceCleanupLeaseInput: workspaceCleanupLeaseInput(claim),
		ErrorCode:                  code,
		ErrorMessage:               message,
		Retryable:                  retryable,
	}
	return c.workspaceCleanupRequest(ctx, claim, "failed", input)
}

func (c *Client) ReleaseWorkspaceCleanup(
	ctx context.Context,
	claim executions.WorkspaceCleanupClaim,
) error {
	return c.workspaceCleanupRequest(ctx, claim, "release", nil)
}

func (c *Client) workspaceCleanupRequest(
	ctx context.Context,
	claim executions.WorkspaceCleanupClaim,
	action string,
	input any,
) error {
	if input == nil {
		input = workspaceCleanupLeaseInput(claim)
	}
	requestID := workspaceCleanupRequestID(claim, action)
	if action == "renew" {
		// Each renewal must extend the database lease. Reusing one receipt would
		// replay the first expiry instead of committing a later heartbeat.
		requestID = uuid.NewString()
	}
	return c.doJSON(
		ctx,
		http.MethodPost,
		workspaceCleanupPath(claim.CleanupID, action),
		c.workerToken,
		requestID,
		input,
		nil,
	)
}

func workspaceCleanupLeaseInput(claim executions.WorkspaceCleanupClaim) executions.WorkspaceCleanupLeaseInput {
	return executions.WorkspaceCleanupLeaseInput{
		DispatchGeneration: claim.DispatchGeneration,
		LeaseToken:         claim.Lease.LeaseToken,
	}
}

func workspaceCleanupPath(cleanupID uuid.UUID, action string) string {
	return "/v1/workers/workspace-cleanups/" + cleanupID.String() + "/" + action
}

func workspaceCleanupRequestID(claim executions.WorkspaceCleanupClaim, action string) string {
	return fmt.Sprintf("%s:%d:workspace-cleanup:%s", claim.CleanupID, claim.DispatchGeneration, action)
}

func (c *Client) Start(ctx context.Context, executionID uuid.UUID, lease executions.Lease) error {
	return c.executionRequest(ctx, executionID, "start", executions.LeaseInput{
		TenantID: lease.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
	}, nil)
}

func (c *Client) MarkWorkspaceReady(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
	result WorkspaceMaterialization,
) error {
	return c.doJSON(
		ctx, http.MethodPost, executionPath(executionID, "workspace/ready"), c.workerToken,
		workspaceRequestID(executionID, lease, "ready"), executions.WorkspaceReadyInput{
			LeaseInput: executions.LeaseInput{
				TenantID: lease.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
			},
			RepositoryFingerprint: result.RepositoryFingerprint, CurrentBranch: result.CurrentBranch,
			BaseCommit: result.BaseCommit, HeadCommit: result.HeadCommit,
			RestoredCheckpointID: result.RestoredCheckpointID,
		}, nil,
	)
}

func (c *Client) MarkWorkspaceDirty(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
	result WorkspaceInspection,
) error {
	return c.doJSON(
		ctx, http.MethodPost, executionPath(executionID, "workspace/dirty"), c.workerToken,
		workspaceRequestID(executionID, lease, "dirty"), executions.WorkspaceDirtyInput{
			LeaseInput: executions.LeaseInput{
				TenantID: lease.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
			},
			CurrentBranch: result.CurrentBranch, HeadCommit: result.HeadCommit,
		}, nil,
	)
}

func (c *Client) MarkWorkspaceFailed(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
	failureCode, failureMessage string,
) error {
	return c.doJSON(
		ctx, http.MethodPost, executionPath(executionID, "workspace/failed"), c.workerToken,
		workspaceRequestID(executionID, lease, "failed"), executions.WorkspaceFailedInput{
			LeaseInput: executions.LeaseInput{
				TenantID: lease.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
			},
			FailureCode: failureCode, FailureMessage: failureMessage,
		}, nil,
	)
}

func workspaceRequestID(executionID uuid.UUID, lease executions.Lease, state string) string {
	return fmt.Sprintf("%s:%d:workspace:%s", executionID, lease.Generation, state)
}

func (c *Client) ResolveCredential(
	ctx context.Context,
	executionID, credentialID uuid.UUID,
	lease executions.Lease,
) (RunnerCredential, error) {
	var output RunnerCredential
	err := c.doJSON(
		ctx,
		http.MethodPost,
		executionPath(executionID, "credentials/"+credentialID.String()+"/resolve"),
		c.workerToken,
		uuid.NewString(),
		executions.LeaseInput{
			TenantID: lease.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
		},
		&output,
	)
	return output, err
}

func (c *Client) ResolveGitCredential(
	ctx context.Context,
	executionID, credentialID uuid.UUID,
	lease executions.Lease,
) (RunnerGitCredential, error) {
	var output RunnerGitCredential
	err := c.doJSON(
		ctx,
		http.MethodPost,
		executionPath(executionID, "git-credentials/"+credentialID.String()+"/resolve"),
		c.workerToken,
		uuid.NewString(),
		executions.LeaseInput{
			TenantID: lease.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
		},
		&output,
	)
	return output, err
}

func (c *Client) Renew(ctx context.Context, executionID uuid.UUID, lease executions.Lease) error {
	return c.executionRequest(ctx, executionID, "renew", executions.RenewLeaseInput{LeaseInput: executions.LeaseInput{
		TenantID: lease.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
	}}, nil)
}

func (c *Client) AppendEvent(ctx context.Context, executionID uuid.UUID, lease executions.Lease, message RunnerMessage) error {
	eventVersion := message.EventVersion
	if eventVersion == 0 {
		eventVersion = executions.RuntimeEventVersionV1
	}
	if eventVersion != executions.RuntimeEventVersionV1 && eventVersion != executions.RuntimeEventVersionV2 {
		return fmt.Errorf("append Runtime Event: unsupported event version %d", eventVersion)
	}
	eventID := uuid.New()
	if message.EventID != nil && *message.EventID != uuid.Nil {
		eventID = *message.EventID
	}
	occurredAt := time.Now().UTC()
	if message.OccurredAt != nil {
		occurredAt = message.OccurredAt.UTC()
	}
	return c.doJSON(ctx, http.MethodPost, executionPath(executionID, "events"), c.workerToken, uuid.NewString(), executions.RuntimeEventInput{
		LeaseInput: executions.LeaseInput{TenantID: lease.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken},
		EventID:    eventID, EventVersion: eventVersion, EventType: message.EventType, Payload: message.Payload, OccurredAt: occurredAt,
	}, nil)
}

func (c *Client) PullInteractionResolutions(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
) ([]executions.InteractionResolutionDelivery, error) {
	var output struct {
		Items []executions.InteractionResolutionDelivery `json:"items"`
	}
	err := c.doJSON(
		ctx, http.MethodPost, executionPath(executionID, "interaction-resolutions/pull"),
		c.workerToken, uuid.NewString(), executions.PullInteractionResolutionsInput{
			LeaseInput: executions.LeaseInput{
				TenantID: lease.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
			},
			Limit: 1,
		}, &output,
	)
	return output.Items, err
}

func (c *Client) MarkInteractionResolutionDelivered(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
	delivery executions.InteractionResolutionDelivery,
) error {
	return c.updateInteractionResolutionDelivery(ctx, executionID, lease, delivery, "delivered")
}

func (c *Client) AcknowledgeInteractionResolution(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
	delivery executions.InteractionResolutionDelivery,
) error {
	return c.updateInteractionResolutionDelivery(ctx, executionID, lease, delivery, "acknowledged")
}

func (c *Client) updateInteractionResolutionDelivery(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
	delivery executions.InteractionResolutionDelivery,
	status string,
) error {
	return c.doJSON(
		ctx, http.MethodPost,
		executionPath(executionID, "interaction-resolutions/"+delivery.InteractionID.String()+"/"+status),
		c.workerToken, interactionDeliveryRequestID(executionID, lease, delivery, status), executions.InteractionResolutionDeliveryInput{
			LeaseInput: executions.LeaseInput{
				TenantID: lease.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
			},
			ResolutionCommandID: delivery.CommandID,
		}, nil,
	)
}

func interactionDeliveryRequestID(
	executionID uuid.UUID,
	lease executions.Lease,
	delivery executions.InteractionResolutionDelivery,
	status string,
) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{
		executionID.String(), delivery.InteractionID.String(), delivery.CommandID,
		fmt.Sprintf("%d", lease.Generation), status,
	}, "\x00")))
	return "interaction-" + status + "-" + hex.EncodeToString(digest[:16])
}

func (c *Client) PullControlCommands(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
) ([]executions.ControlCommandDelivery, error) {
	var output struct {
		Items []executions.ControlCommandDelivery `json:"items"`
	}
	err := c.doJSON(
		ctx, http.MethodPost, executionPath(executionID, "control-commands/pull"),
		c.workerToken, uuid.NewString(), executions.PullControlCommandsInput{
			LeaseInput: executions.LeaseInput{
				TenantID: lease.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
			},
			Limit: 1,
		}, &output,
	)
	return output.Items, err
}

func (c *Client) MarkControlCommandDelivered(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
	delivery executions.ControlCommandDelivery,
) error {
	return c.updateControlCommandDelivery(ctx, executionID, lease, delivery, "delivered", nil)
}

func (c *Client) AcknowledgeControlCommand(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
	delivery executions.ControlCommandDelivery,
	result map[string]any,
) error {
	return c.updateControlCommandDelivery(ctx, executionID, lease, delivery, "acknowledged", result)
}

func (c *Client) updateControlCommandDelivery(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
	delivery executions.ControlCommandDelivery,
	status string,
	result map[string]any,
) error {
	var providerResumeCursor *string
	if value, ok := result["providerResumeCursor"].(string); ok && strings.TrimSpace(value) != "" {
		trimmed := strings.TrimSpace(value)
		providerResumeCursor = &trimmed
	}
	return c.doJSON(
		ctx, http.MethodPost,
		executionPath(executionID, "control-commands/"+delivery.ControlCommandID.String()+"/"+status),
		c.workerToken, controlCommandDeliveryRequestID(executionID, lease, delivery, status), executions.ControlCommandDeliveryInput{
			LeaseInput: executions.LeaseInput{
				TenantID: lease.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
			},
			CommandID: delivery.CommandID, ProviderResumeCursor: providerResumeCursor, Result: result,
		}, nil,
	)
}

func controlCommandDeliveryRequestID(
	executionID uuid.UUID,
	lease executions.Lease,
	delivery executions.ControlCommandDelivery,
	status string,
) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{
		executionID.String(), delivery.ControlCommandID.String(), delivery.CommandID,
		fmt.Sprintf("%d", lease.Generation), status,
	}, "\x00")))
	return "control-command-" + status + "-" + hex.EncodeToString(digest[:16])
}

func (c *Client) Complete(ctx context.Context, executionID uuid.UUID, lease executions.Lease, result RunnerResult) error {
	return c.executionRequest(ctx, executionID, "complete", executions.CompleteExecutionInput{
		LeaseInput:           executions.LeaseInput{TenantID: lease.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken},
		ProviderResumeCursor: result.ProviderResumeCursor, Output: result.Output,
	}, nil)
}

func (c *Client) Fail(ctx context.Context, executionID uuid.UUID, lease executions.Lease, code, message string) error {
	return c.executionRequest(ctx, executionID, "fail", executions.FailExecutionInput{
		LeaseInput:  executions.LeaseInput{TenantID: lease.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken},
		FailureCode: code, FailureMessage: message,
	}, nil)
}

func (c *Client) Release(ctx context.Context, executionID uuid.UUID, lease executions.Lease, reason string) error {
	return c.executionRequest(ctx, executionID, "release", executions.ReleaseLeaseInput{
		LeaseInput: executions.LeaseInput{TenantID: lease.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken},
		Reason:     reason,
	}, nil)
}

func (c *Client) UploadArtifact(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
	artifact RunnerArtifact,
	absolutePath string,
) (artifacts.Artifact, error) {
	return c.uploadArtifactAttempt(
		ctx, executionID, lease, artifact, absolutePath, nil, uuid.NewString(), uuid.NewString(),
	)
}

func (c *Client) UploadCheckpointArtifact(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
	checkpoint executions.WorkspaceCheckpoint,
	candidate WorkspaceCheckpointCandidate,
) (artifacts.Artifact, error) {
	var lastErr error
	createRequestID := checkpointRequestID(executionID, lease, candidate.IdempotencyKey, "artifact-create")
	completeRequestID := checkpointRequestID(executionID, lease, candidate.IdempotencyKey, "artifact-complete")
	for attempt := 1; attempt <= 3; attempt++ {
		artifact, err := c.uploadArtifactAttempt(
			ctx, executionID, lease, *candidate.Artifact, candidate.ArtifactPath,
			&checkpoint.ID, createRequestID, completeRequestID,
		)
		if err == nil {
			return artifact, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return artifacts.Artifact{}, lastErr
}

func (c *Client) uploadArtifactAttempt(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
	artifact RunnerArtifact,
	absolutePath string,
	checkpointID *uuid.UUID,
	createRequestID, completeRequestID string,
) (artifacts.Artifact, error) {
	var grant artifacts.UploadGrant
	err := c.doJSON(ctx, http.MethodPost, executionPath(executionID, "artifacts"), c.workerToken, createRequestID, artifacts.WorkerCreateInput{
		TenantID: lease.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
		CheckpointID: checkpointID, Kind: artifact.Kind, OriginalName: optionalName(artifact.OriginalName),
	}, &grant)
	if err != nil {
		return artifacts.Artifact{}, err
	}
	if grant.Artifact.Status == "ready" {
		if err := verifyReadyArtifactFile(absolutePath, artifact.ContentType, grant.Artifact); err != nil {
			return artifacts.Artifact{}, err
		}
		return grant.Artifact, nil
	}
	if strings.TrimSpace(grant.URL) == "" || strings.TrimSpace(grant.Method) == "" {
		return artifacts.Artifact{}, errors.New("control plane returned a pending Artifact without an upload grant")
	}
	file, err := os.Open(absolutePath)
	if err != nil {
		return artifacts.Artifact{}, fmt.Errorf("open runner artifact: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return artifacts.Artifact{}, fmt.Errorf("stat runner artifact: %w", err)
	}
	hash := sha256.New()
	uploadURL, err := c.resolveURL(grant.URL)
	if err != nil {
		return artifacts.Artifact{}, err
	}
	request, err := http.NewRequestWithContext(ctx, grant.Method, uploadURL.String(), io.TeeReader(file, hash))
	if err != nil {
		return artifacts.Artifact{}, err
	}
	request.ContentLength = info.Size()
	for name, value := range grant.Headers {
		request.Header.Set(name, value)
	}
	request.Header.Set("Content-Type", artifact.ContentType)
	response, err := c.uploadHTTP.Do(request)
	if err != nil {
		return artifacts.Artifact{}, fmt.Errorf("upload runner artifact: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return artifacts.Artifact{}, responseError(response)
	}
	var completed artifacts.Artifact
	err = c.doJSONUsing(ctx, c.uploadHTTP, http.MethodPost, executionPath(executionID, "artifacts/"+grant.Artifact.ID.String()+"/complete"), c.workerToken, completeRequestID, artifacts.WorkerCompleteInput{
		TenantID: lease.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
		CompleteInput: artifacts.CompleteInput{
			SizeBytes: info.Size(), SHA256: hex.EncodeToString(hash.Sum(nil)), ContentType: artifact.ContentType,
		},
	}, &completed)
	return completed, err
}

func verifyReadyArtifactFile(absolutePath, contentType string, ready artifacts.Artifact) error {
	if ready.SizeBytes == nil || ready.SHA256 == nil || ready.ContentType == nil {
		return errors.New("ready Checkpoint Artifact is missing verification metadata")
	}
	file, err := os.Open(absolutePath)
	if err != nil {
		return fmt.Errorf("open ready Checkpoint Artifact source: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat ready Checkpoint Artifact source: %w", err)
	}
	hash := sha256.New()
	written, err := io.Copy(hash, file)
	if err != nil {
		return fmt.Errorf("hash ready Checkpoint Artifact source: %w", err)
	}
	if info.Size() != *ready.SizeBytes || written != *ready.SizeBytes ||
		hex.EncodeToString(hash.Sum(nil)) != *ready.SHA256 || contentType != *ready.ContentType {
		return errors.New("ready Checkpoint Artifact does not match the current local payload")
	}
	return nil
}

func (c *Client) CreateWorkspaceCheckpoint(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
	candidate WorkspaceCheckpointCandidate,
) (executions.WorkspaceCheckpoint, error) {
	var checkpoint executions.WorkspaceCheckpoint
	requestID := checkpointRequestID(executionID, lease, candidate.IdempotencyKey, "create")
	input := executions.CreateWorkspaceCheckpointInput{
		LeaseInput: executions.LeaseInput{
			TenantID: lease.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
		},
		IdempotencyKey: candidate.IdempotencyKey, Strategy: candidate.Strategy,
		BaseCommit: candidate.BaseCommit, HeadCommit: candidate.HeadCommit,
		CurrentBranch: candidate.CurrentBranch, Manifest: candidate.Manifest,
		FileCount: &candidate.FileCount, TotalBytes: &candidate.TotalBytes,
	}
	err := retryCheckpointOperation(ctx, func() error {
		return c.doJSON(
			ctx, http.MethodPost, executionPath(executionID, "workspace/checkpoints"), c.workerToken,
			requestID, input, &checkpoint,
		)
	})
	return checkpoint, err
}

func (c *Client) MarkWorkspaceCheckpointReady(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
	candidate WorkspaceCheckpointCandidate,
	checkpoint executions.WorkspaceCheckpoint,
	artifact *artifacts.Artifact,
) error {
	input := executions.WorkspaceCheckpointReadyInput{LeaseInput: executions.LeaseInput{
		TenantID: lease.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
	}}
	if artifact != nil {
		input.ArtifactID = &artifact.ID
		input.SHA256 = artifact.SHA256
	}
	requestPath := executionPath(executionID, "workspace/checkpoints/"+checkpoint.ID.String()+"/ready")
	requestID := checkpointRequestID(executionID, lease, candidate.IdempotencyKey, "ready")
	return retryCheckpointOperation(ctx, func() error {
		return c.doJSON(ctx, http.MethodPost, requestPath, c.workerToken, requestID, input, nil)
	})
}

func (c *Client) MarkWorkspaceCheckpointFailed(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
	candidate WorkspaceCheckpointCandidate,
	checkpoint executions.WorkspaceCheckpoint,
	failure error,
) error {
	message := strings.TrimSpace(failure.Error())
	if len(message) > 10_000 {
		message = message[:10_000]
	}
	requestPath := executionPath(executionID, "workspace/checkpoints/"+checkpoint.ID.String()+"/failed")
	requestID := checkpointRequestID(executionID, lease, candidate.IdempotencyKey, "failed")
	input := executions.WorkspaceCheckpointFailedInput{
		LeaseInput: executions.LeaseInput{
			TenantID: lease.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
		},
		FailureCode: "checkpoint_persist_failed", FailureMessage: message,
	}
	return retryCheckpointOperation(ctx, func() error {
		return c.doJSON(ctx, http.MethodPost, requestPath, c.workerToken, requestID, input, nil)
	})
}

func retryCheckpointOperation(ctx context.Context, operation func() error) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := operation(); err != nil {
			lastErr = err
			if ctx.Err() != nil {
				break
			}
			continue
		}
		return nil
	}
	return lastErr
}

func (c *Client) DownloadWorkspaceCheckpointArtifact(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
	checkpoint executions.WorkspaceCheckpoint,
) (string, func(), error) {
	if checkpoint.ArtifactID == nil || checkpoint.SHA256 == nil {
		return "", nil, errors.New("Workspace Checkpoint does not reference an Artifact")
	}
	var grant artifacts.DownloadGrant
	err := c.doJSON(
		ctx, http.MethodPost,
		executionPath(executionID, "workspace/checkpoints/"+checkpoint.ID.String()+"/artifact/download"),
		c.workerToken, checkpointRequestID(executionID, lease, checkpoint.IdempotencyKey, "download"),
		executions.LeaseInput{
			TenantID: lease.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
		}, &grant,
	)
	if err != nil {
		return "", nil, err
	}
	if grant.Artifact.ID != *checkpoint.ArtifactID || grant.Artifact.SizeBytes == nil || grant.Artifact.SHA256 == nil ||
		*grant.Artifact.SHA256 != *checkpoint.SHA256 || *grant.Artifact.SizeBytes < 0 || *grant.Artifact.SizeBytes > checkpointSnapshotMaxBytes {
		return "", nil, errors.New("Workspace Checkpoint Artifact grant does not match the persisted Checkpoint")
	}
	downloadURL, err := c.resolveURL(grant.URL)
	if err != nil {
		return "", nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL.String(), nil)
	if err != nil {
		return "", nil, err
	}
	response, err := c.uploadHTTP.Do(request)
	if err != nil {
		return "", nil, fmt.Errorf("download Workspace Checkpoint Artifact: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", nil, responseError(response)
	}
	file, err := os.CreateTemp("", "synara-workspace-restore-*.tar")
	if err != nil {
		return "", nil, err
	}
	path := file.Name()
	cleanup := func() { _ = os.Remove(path) }
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, *grant.Artifact.SizeBytes+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || written != *grant.Artifact.SizeBytes ||
		hex.EncodeToString(hash.Sum(nil)) != *checkpoint.SHA256 {
		cleanup()
		if copyErr != nil {
			return "", nil, copyErr
		}
		if closeErr != nil {
			return "", nil, closeErr
		}
		return "", nil, errors.New("Workspace Checkpoint Artifact size or SHA-256 verification failed")
	}
	return path, cleanup, nil
}

func checkpointRequestID(
	executionID uuid.UUID,
	lease executions.Lease,
	idempotencyKey, operation string,
) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{
		executionID.String(), fmt.Sprintf("%d", lease.Generation), idempotencyKey, operation,
	}, "\x00")))
	return "checkpoint-" + operation + "-" + hex.EncodeToString(digest[:16])
}

func (c *Client) executionRequest(ctx context.Context, executionID uuid.UUID, operation string, input, output any) error {
	return c.doJSON(ctx, http.MethodPost, executionPath(executionID, operation), c.workerToken, uuid.NewString(), input, output)
}

func executionPath(executionID uuid.UUID, suffix string) string {
	return "/v1/workers/executions/" + executionID.String() + "/" + suffix
}

func (c *Client) doJSON(ctx context.Context, method, requestPath, bearer, requestID string, input, output any) error {
	return c.doJSONUsing(ctx, c.http, method, requestPath, bearer, requestID, input, output)
}

func (c *Client) doJSONUsing(ctx context.Context, client *http.Client, method, requestPath, bearer, requestID string, input, output any) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	target, err := c.resolveURL(requestPath)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return err
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	if requestID != "" {
		request.Header.Set("X-Request-ID", requestID)
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return responseError(response)
	}
	if output == nil || response.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, response.Body)
		return nil
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4<<20))
	if err := decoder.Decode(output); err != nil {
		return fmt.Errorf("decode control-plane response: %w", err)
	}
	return nil
}

func (c *Client) resolveURL(value string) (*url.URL, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return nil, err
	}
	if parsed.IsAbs() {
		return parsed, nil
	}
	base := *c.baseURL
	base.Path = path.Join(base.Path, parsed.Path)
	base.RawQuery = parsed.RawQuery
	return &base, nil
}

func responseError(response *http.Response) error {
	encoded, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(encoded, &envelope)
	if envelope.Error.Message != "" {
		return fmt.Errorf("control plane %s (%d): %s", envelope.Error.Code, response.StatusCode, envelope.Error.Message)
	}
	return fmt.Errorf("control plane request failed with status %d: %s", response.StatusCode, strings.TrimSpace(string(encoded)))
}

func optionalName(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}
