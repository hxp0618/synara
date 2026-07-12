package agentd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
		ClusterID: cfg.ClusterID, Namespace: cfg.Namespace, PodName: cfg.PodName,
		Version: cfg.Version, Capabilities: cfg.Capabilities, LeaseSupported: true, FencingSupported: true,
	}, &output)
	if err != nil {
		return executions.RegisteredWorker{}, err
	}
	c.workerToken = output.Token
	return output, nil
}

func (c *Client) Heartbeat(ctx context.Context, cfg Config) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/workers/heartbeat", c.workerToken, "", executions.HeartbeatInput{
		Version: cfg.Version, Capabilities: cfg.Capabilities,
	}, nil)
}

func (c *Client) Claim(ctx context.Context, cfg Config) (executions.ClaimResult, error) {
	var output executions.ClaimResult
	err := c.doJSON(ctx, http.MethodPost, "/v1/workers/executions/claim", c.workerToken, uuid.NewString(), executions.ClaimExecutionInput{
		ExecutionTargetID: cfg.ExecutionTargetID, TargetKind: string(cfg.TargetKind), ExecutionID: cfg.AssignedExecutionID,
	}, &output)
	return output, err
}

func (c *Client) Start(ctx context.Context, executionID uuid.UUID, lease executions.Lease) error {
	return c.executionRequest(ctx, executionID, "start", executions.LeaseInput{
		TenantID: lease.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
	}, nil)
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
		EventID:    eventID, EventVersion: 1, EventType: message.EventType, Payload: message.Payload, OccurredAt: occurredAt,
	}, nil)
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
) error {
	var grant artifacts.UploadGrant
	err := c.doJSON(ctx, http.MethodPost, executionPath(executionID, "artifacts"), c.workerToken, uuid.NewString(), artifacts.WorkerCreateInput{
		TenantID: lease.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
		Kind: artifact.Kind, OriginalName: optionalName(artifact.OriginalName),
	}, &grant)
	if err != nil {
		return err
	}
	file, err := os.Open(absolutePath)
	if err != nil {
		return fmt.Errorf("open runner artifact: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat runner artifact: %w", err)
	}
	hash := sha256.New()
	uploadURL, err := c.resolveURL(grant.URL)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, grant.Method, uploadURL.String(), io.TeeReader(file, hash))
	if err != nil {
		return err
	}
	request.ContentLength = info.Size()
	for name, value := range grant.Headers {
		request.Header.Set(name, value)
	}
	request.Header.Set("Content-Type", artifact.ContentType)
	response, err := c.uploadHTTP.Do(request)
	if err != nil {
		return fmt.Errorf("upload runner artifact: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return responseError(response)
	}
	return c.doJSONUsing(ctx, c.uploadHTTP, http.MethodPost, executionPath(executionID, "artifacts/"+grant.Artifact.ID.String()+"/complete"), c.workerToken, uuid.NewString(), artifacts.WorkerCompleteInput{
		TenantID: lease.TenantID, Generation: lease.Generation, LeaseToken: lease.LeaseToken,
		CompleteInput: artifacts.CompleteInput{
			SizeBytes: info.Size(), SHA256: hex.EncodeToString(hash.Sum(nil)), ContentType: artifact.ContentType,
		},
	}, nil)
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
