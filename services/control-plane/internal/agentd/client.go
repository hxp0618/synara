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
	"mime"
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
	baseURL                      *url.URL
	http                         *http.Client
	uploadHTTP                   *http.Client
	registrationToken            string
	workerToken                  string
	artifactIdempotencySupported bool
}

type controlPlaneProblem struct {
	Code    string
	Status  int
	Message string
}

func (e *controlPlaneProblem) Error() string {
	if strings.TrimSpace(e.Code) != "" && strings.TrimSpace(e.Message) != "" {
		return fmt.Sprintf("control plane %s (%d): %s", e.Code, e.Status, e.Message)
	}
	if strings.TrimSpace(e.Code) != "" {
		return fmt.Sprintf("control plane %s (%d)", e.Code, e.Status)
	}
	return fmt.Sprintf("control plane request failed with status %d", e.Status)
}

func isWorkerRevocationError(err error) bool {
	var problem *controlPlaneProblem
	if !errors.As(err, &problem) {
		return false
	}
	return problem.Code == "worker_token_revoked" || problem.Code == "worker_identity_revoked"
}

func NewClient(cfg Config) *Client {
	return &Client{
		baseURL: cfg.ControlPlaneURL, registrationToken: cfg.RegistrationToken,
		http: &http.Client{Timeout: cfg.RequestTimeout}, uploadHTTP: &http.Client{Timeout: cfg.ArtifactTimeout},
	}
}

func (c *Client) Register(ctx context.Context, cfg Config) (executions.RegisteredWorker, error) {
	var output executions.RegisteredWorker
	headers, err := c.doJSONResponse(ctx, http.MethodPost, "/v1/workers/register", c.registrationToken, "", executions.RegisterWorkerInput{
		ExecutionTargetID: cfg.ExecutionTargetID, TargetKind: string(cfg.TargetKind),
		ClusterID: cfg.ClusterID, Namespace: cfg.Namespace, PodName: cfg.PodName, InstanceUID: cfg.InstanceUID,
		Version: cfg.Version, ProtocolVersion: executions.WorkerProtocolVersion,
		Capabilities: cfg.Capabilities, LeaseSupported: true, FencingSupported: true,
	}, &output)
	if err != nil {
		return executions.RegisteredWorker{}, err
	}
	c.workerToken = output.Token
	c.artifactIdempotencySupported =
		headers.Get(artifacts.WorkerIdempotencyFeatureHeader) == artifacts.WorkerIdempotencyFeatureHeaderValue
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
	deliveryResult := result
	if result != nil {
		deliveryResult = make(map[string]any, len(result))
		for key, value := range result {
			if key != "providerResumeCursor" {
				deliveryResult[key] = value
			}
		}
	}
	return c.doJSON(
		ctx, http.MethodPost,
		executionPath(executionID, "control-commands/"+delivery.ControlCommandID.String()+"/"+status),
		c.workerToken, controlCommandDeliveryRequestID(executionID, lease, delivery, status), executions.ControlCommandDeliveryInput{
			LeaseInput: executions.LeaseInput{
				TenantID: lease.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
			},
			CommandID: delivery.CommandID, ProviderResumeCursor: providerResumeCursor, Result: deliveryResult,
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
	input := executions.ReleaseLeaseInput{
		LeaseInput: executions.LeaseInput{TenantID: lease.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken},
		Reason:     reason,
	}
	requestID := executionLifecycleRequestID(executionID, lease, "release", reason)
	return retryCheckpointOperation(ctx, func() error {
		return c.doJSON(
			ctx, http.MethodPost, executionPath(executionID, "release"), c.workerToken, requestID, input, nil,
		)
	})
}

func executionLifecycleRequestID(
	executionID uuid.UUID,
	lease executions.Lease,
	operation, detail string,
) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{
		executionID.String(), fmt.Sprintf("%d", lease.Generation), operation, detail,
	}, "\x00")))
	return "execution-" + operation + "-" + hex.EncodeToString(digest[:16])
}

func (c *Client) UploadArtifact(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
	artifact RunnerArtifact,
	absolutePath string,
) (artifacts.Artifact, error) {
	source, err := openRegularArtifactSource(absolutePath)
	if err != nil {
		return artifacts.Artifact{}, fmt.Errorf("open runner artifact: %w", err)
	}
	defer source.Close()
	return c.uploadArtifactSource(ctx, executionID, lease, artifact, source)
}

func (c *Client) uploadArtifactSource(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
	artifact RunnerArtifact,
	source *artifactUploadSource,
) (artifacts.Artifact, error) {
	contentType, err := normalizeRunnerArtifactContentType(artifact.ContentType)
	if err != nil {
		return artifacts.Artifact{}, err
	}
	artifact.ContentType = contentType
	identity, err := inspectArtifactUploadIdentity(ctx, executionID, lease, artifact, source)
	if err != nil {
		return artifacts.Artifact{}, err
	}
	var lastErr error
	createRequestID := artifactRequestID(executionID, lease, identity.IdempotencyKey, "create")
	completeRequestID := artifactRequestID(executionID, lease, identity.IdempotencyKey, "complete")
	maximumAttempts := 1
	if c.artifactIdempotencySupported {
		maximumAttempts = 3
	}
	for attempt := 1; attempt <= maximumAttempts; attempt++ {
		artifact, err := c.uploadArtifactAttempt(
			ctx, executionID, lease, artifact, source, nil, &identity,
			createRequestID, completeRequestID,
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

func (c *Client) UploadCheckpointArtifact(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
	checkpoint executions.WorkspaceCheckpoint,
	candidate WorkspaceCheckpointCandidate,
) (artifacts.Artifact, error) {
	contentType, err := normalizeRunnerArtifactContentType(candidate.Artifact.ContentType)
	if err != nil {
		return artifacts.Artifact{}, err
	}
	artifact := *candidate.Artifact
	artifact.ContentType = contentType
	source, err := openRegularArtifactSource(candidate.ArtifactPath)
	if err != nil {
		return artifacts.Artifact{}, fmt.Errorf("open Workspace Checkpoint Artifact: %w", err)
	}
	defer source.Close()
	uploadSource := source
	cleanupUploadSource := func() {}
	if guard := executionSecretGuardFromContext(ctx); guard != nil {
		uploadSource, cleanupUploadSource, err = guardedArtifactUploadSource(
			ctx, guard, artifact.ContentType, source,
		)
		if err != nil {
			return artifacts.Artifact{}, err
		}
	}
	defer cleanupUploadSource()
	var lastErr error
	createRequestID := checkpointRequestID(executionID, lease, candidate.IdempotencyKey, "artifact-create")
	completeRequestID := checkpointRequestID(executionID, lease, candidate.IdempotencyKey, "artifact-complete")
	for attempt := 1; attempt <= 3; attempt++ {
		completed, err := c.uploadArtifactAttempt(
			ctx, executionID, lease, artifact, uploadSource,
			&checkpoint.ID, nil, createRequestID, completeRequestID,
		)
		if err == nil {
			return completed, nil
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
	source *artifactUploadSource,
	checkpointID *uuid.UUID,
	identity *artifactUploadIdentity,
	createRequestID, completeRequestID string,
) (artifacts.Artifact, error) {
	var grant artifacts.UploadGrant
	var idempotencyKey *string
	requestHeaders := make(http.Header)
	if identity != nil && c.artifactIdempotencySupported {
		idempotencyKey = &identity.IdempotencyKey
		requestHeaders.Set(artifacts.WorkerIdempotencyKeyHeader, identity.IdempotencyKey)
	}
	err := c.doJSONWithHeaders(ctx, http.MethodPost, executionPath(executionID, "artifacts"), c.workerToken, createRequestID, requestHeaders, artifacts.WorkerCreateInput{
		TenantID: lease.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
		CheckpointID: checkpointID, IdempotencyKey: idempotencyKey,
		Kind: artifact.Kind, OriginalName: optionalName(artifact.OriginalName),
	}, &grant)
	if err != nil {
		return artifacts.Artifact{}, err
	}
	if grant.Artifact.Status == "ready" {
		if err := verifyReadyArtifactSource(ctx, source, artifact.ContentType, grant.Artifact); err != nil {
			return artifacts.Artifact{}, err
		}
		return grant.Artifact, nil
	}
	if strings.TrimSpace(grant.URL) == "" || strings.TrimSpace(grant.Method) == "" {
		return artifacts.Artifact{}, errors.New("control plane returned a pending Artifact without an upload grant")
	}
	info, err := source.rewind()
	if err != nil {
		return artifacts.Artifact{}, fmt.Errorf("rewind runner artifact: %w", err)
	}
	if identity != nil && info.Size() != identity.SizeBytes {
		return artifacts.Artifact{}, errors.New("runner artifact changed after its upload identity was calculated")
	}
	hash := sha256.New()
	uploadURL, err := c.resolveURL(grant.URL)
	if err != nil {
		return artifacts.Artifact{}, errors.New("control plane returned an invalid Artifact upload URL")
	}
	request, err := http.NewRequestWithContext(ctx, grant.Method, uploadURL.String(), io.TeeReader(source.file, hash))
	if err != nil {
		return artifacts.Artifact{}, errors.New("control plane returned an invalid Artifact upload request")
	}
	request.ContentLength = info.Size()
	for name, value := range grant.Headers {
		request.Header.Set(name, value)
	}
	request.Header.Set("Content-Type", artifact.ContentType)
	response, err := c.uploadHTTP.Do(request)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return artifacts.Artifact{}, fmt.Errorf("upload runner Artifact: %w", ctxErr)
		}
		return artifacts.Artifact{}, fmt.Errorf(
			"upload runner Artifact to %s failed", safeArtifactUploadTarget(uploadURL),
		)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return artifacts.Artifact{}, fmt.Errorf(
			"upload runner Artifact to %s failed with status %d",
			safeArtifactUploadTarget(uploadURL), response.StatusCode,
		)
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if identity != nil && digest != identity.SHA256 {
		return artifacts.Artifact{}, errors.New("runner artifact changed while it was being uploaded")
	}
	var completed artifacts.Artifact
	err = c.doJSONUsing(ctx, c.uploadHTTP, http.MethodPost, executionPath(executionID, "artifacts/"+grant.Artifact.ID.String()+"/complete"), c.workerToken, completeRequestID, artifacts.WorkerCompleteInput{
		TenantID: lease.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
		CompleteInput: artifacts.CompleteInput{
			SizeBytes: info.Size(), SHA256: digest, ContentType: artifact.ContentType,
		},
	}, &completed)
	return completed, err
}

type artifactUploadIdentity struct {
	IdempotencyKey string
	SizeBytes      int64
	SHA256         string
}

func inspectArtifactUploadIdentity(
	ctx context.Context,
	executionID uuid.UUID,
	lease executions.Lease,
	artifact RunnerArtifact,
	source *artifactUploadSource,
) (artifactUploadIdentity, error) {
	info, err := source.rewind()
	if err != nil {
		return artifactUploadIdentity{}, fmt.Errorf("rewind runner artifact: %w", err)
	}
	hash := sha256.New()
	written, err := copyWithContext(ctx, hash, source.file)
	if err != nil {
		return artifactUploadIdentity{}, fmt.Errorf("runner artifact could not be hashed for stable upload identity: %w", err)
	}
	if written != info.Size() {
		return artifactUploadIdentity{}, errors.New("runner artifact could not be hashed for stable upload identity")
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	seed := strings.Join([]string{
		executionID.String(), fmt.Sprintf("%d", lease.Generation), artifact.Path, artifact.Kind,
		artifact.OriginalName, artifact.ContentType, fmt.Sprintf("%d", info.Size()), digest,
	}, "\x00")
	idempotencyDigest := sha256.Sum256([]byte(seed))
	return artifactUploadIdentity{
		IdempotencyKey: "artifact-" + hex.EncodeToString(idempotencyDigest[:]),
		SizeBytes:      info.Size(), SHA256: digest,
	}, nil
}

func artifactRequestID(
	executionID uuid.UUID,
	lease executions.Lease,
	idempotencyKey, operation string,
) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{
		executionID.String(), fmt.Sprintf("%d", lease.Generation), idempotencyKey, operation,
	}, "\x00")))
	return "artifact-" + operation + "-" + hex.EncodeToString(digest[:16])
}

func verifyReadyArtifactFile(absolutePath, contentType string, ready artifacts.Artifact) error {
	source, err := openRegularArtifactSource(absolutePath)
	if err != nil {
		return fmt.Errorf("open ready Artifact source: %w", err)
	}
	defer source.Close()
	normalized, err := normalizeRunnerArtifactContentType(contentType)
	if err != nil {
		return err
	}
	return verifyReadyArtifactSource(context.Background(), source, normalized, ready)
}

func verifyReadyArtifactSource(
	ctx context.Context,
	source *artifactUploadSource,
	contentType string,
	ready artifacts.Artifact,
) error {
	if ready.SizeBytes == nil || ready.SHA256 == nil || ready.ContentType == nil {
		return errors.New("ready Artifact is missing verification metadata")
	}
	info, err := source.rewind()
	if err != nil {
		return fmt.Errorf("rewind ready Artifact source: %w", err)
	}
	hash := sha256.New()
	written, err := copyWithContext(ctx, hash, source.file)
	if err != nil {
		return fmt.Errorf("hash ready Artifact source: %w", err)
	}
	if info.Size() != *ready.SizeBytes || written != *ready.SizeBytes ||
		hex.EncodeToString(hash.Sum(nil)) != *ready.SHA256 || contentType != *ready.ContentType {
		return errors.New("ready Artifact does not match the current local payload")
	}
	return nil
}

func normalizeRunnerArtifactContentType(value string) (string, error) {
	mediaType, parameters, err := mime.ParseMediaType(strings.TrimSpace(value))
	if err != nil || len(mediaType) > 200 {
		return "", errors.New("runner artifact Content-Type is invalid")
	}
	normalized := mime.FormatMediaType(strings.ToLower(mediaType), parameters)
	if normalized == "" || len(normalized) > 255 {
		return "", errors.New("runner artifact Content-Type is invalid")
	}
	return normalized, nil
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
	failure = sanitizeExecutionContextError(ctx, failure)
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

func safeArtifactUploadTarget(value *url.URL) string {
	if value == nil {
		return "the granted target"
	}
	safe := *value
	safe.User = nil
	safe.RawQuery = ""
	safe.ForceQuery = false
	safe.Fragment = ""
	return safe.String()
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
	return c.doJSONWithHeaders(ctx, method, requestPath, bearer, requestID, nil, input, output)
}

func (c *Client) doJSONResponse(
	ctx context.Context,
	method, requestPath, bearer, requestID string,
	input, output any,
) (http.Header, error) {
	return c.doJSONUsingHeadersResponse(ctx, c.http, method, requestPath, bearer, requestID, nil, input, output)
}

func (c *Client) doJSONWithHeaders(
	ctx context.Context,
	method, requestPath, bearer, requestID string,
	headers http.Header,
	input, output any,
) error {
	_, err := c.doJSONUsingHeadersResponse(ctx, c.http, method, requestPath, bearer, requestID, headers, input, output)
	return err
}

func (c *Client) doJSONUsing(ctx context.Context, client *http.Client, method, requestPath, bearer, requestID string, input, output any) error {
	return c.doJSONUsingHeaders(ctx, client, method, requestPath, bearer, requestID, nil, input, output)
}

func (c *Client) doJSONUsingHeaders(
	ctx context.Context,
	client *http.Client,
	method, requestPath, bearer, requestID string,
	headers http.Header,
	input, output any,
) error {
	_, err := c.doJSONUsingHeadersResponse(ctx, client, method, requestPath, bearer, requestID, headers, input, output)
	return err
}

func (c *Client) doJSONUsingHeadersResponse(
	ctx context.Context,
	client *http.Client,
	method, requestPath, bearer, requestID string,
	headers http.Header,
	input, output any,
) (http.Header, error) {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(encoded)
	}
	target, err := c.resolveURL(requestPath)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, err
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
	for name, values := range headers {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, responseError(response)
	}
	responseHeaders := response.Header.Clone()
	if output == nil || response.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, response.Body)
		return responseHeaders, nil
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 4<<20))
	if err := decoder.Decode(output); err != nil {
		return nil, fmt.Errorf("decode control-plane response: %w", err)
	}
	return responseHeaders, nil
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
	if envelope.Error.Code != "" || envelope.Error.Message != "" {
		return &controlPlaneProblem{
			Code: envelope.Error.Code, Status: response.StatusCode, Message: envelope.Error.Message,
		}
	}
	return &controlPlaneProblem{Status: response.StatusCode}
}

func optionalName(value string) *string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return &value
}
