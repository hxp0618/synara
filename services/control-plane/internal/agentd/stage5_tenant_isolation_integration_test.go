package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	sharedrecoverybundle "github.com/synara-ai/synara/services/control-plane/internal/recoverybundle"
)

const (
	stage5AgentdProviderIsolationIntegrationEnv = "SYNARA_STAGE5_AGENTD_PROVIDER_ISOLATION_TEST"
	stage5ProviderFixturePathEnv                = "SYNARA_STAGE5_PROVIDER_FIXTURE_PATH"
)

func TestStage5SharedWorkerAgentdProviderTenantIsolation(t *testing.T) {
	if os.Getenv(stage5AgentdProviderIsolationIntegrationEnv) != "1" {
		t.Skip(stage5AgentdProviderIsolationIntegrationEnv + "=1 is required")
	}
	fixturePath := strings.TrimSpace(os.Getenv(stage5ProviderFixturePathEnv))
	if fixturePath == "" || !filepath.IsAbs(fixturePath) {
		t.Fatal(stage5ProviderFixturePathEnv + " must name an absolute Provider Host fixture path")
	}
	fixtureInfo, err := os.Lstat(fixturePath)
	if err != nil || !fixtureInfo.Mode().IsRegular() || fixtureInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("Provider Host fixture is not one regular non-symlink file: %v", err)
	}
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node is required for the Provider Host fixture")
	}
	nodePath, err = filepath.Abs(nodePath)
	if err != nil {
		t.Fatal(err)
	}

	acceptanceRoot := t.TempDir()
	workspaceRoot := filepath.Join(acceptanceRoot, "workspaces")
	gitCacheRoot := filepath.Join(acceptanceRoot, "git-cache")
	privateTempRoot := filepath.Join(acceptanceRoot, "private-tmp")
	for _, directory := range []string{workspaceRoot, gitCacheRoot, privateTempRoot} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TMPDIR", privateTempRoot)

	targetID := uuid.New()
	marker := "stage5-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	controlPlane, err := newStage5AgentdProviderIsolationControlPlane(targetID, marker)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(controlPlane)
	defer server.Close()
	controlPlaneURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	instanceUID := uuid.NewString()
	cfg := Config{
		ControlPlaneURL:              controlPlaneURL,
		RegistrationToken:            controlPlane.registrationToken,
		ExecutionTargetID:            targetID,
		WorkerMode:                   executions.WorkerModeGeneralPool,
		TargetKind:                   platform.TargetKubernetes,
		KubernetesPIDsMax:            128,
		ClusterID:                    "stage5-live",
		Namespace:                    "stage5-live",
		PodName:                      "stage5-agentd-provider-isolation",
		InstanceUID:                  instanceUID,
		Version:                      "0.0.0-stage5-agentd-provider-isolation",
		Capabilities:                 map[string]any{},
		ExperimentalProviders:        []string{"codex"},
		RunnerCommand:                []string{nodePath, fixturePath, "--protocol-v2", "--enable-providers=codex"},
		RunnerProtocol:               RunnerProtocolV2,
		WorkspaceRoot:                workspaceRoot,
		GitCacheRoot:                 gitCacheRoot,
		PrivateTempRoot:              privateTempRoot,
		ProviderOuterSandboxProfile:  providerOuterSandboxKubernetesRestricted,
		PollInterval:                 20 * time.Millisecond,
		HeartbeatInterval:            5 * time.Second,
		LeaseRenewInterval:           5 * time.Second,
		DrainTimeout:                 2 * time.Second,
		RequestTimeout:               2 * time.Second,
		SandboxAllocationBindTimeout: 2 * time.Second,
		ArtifactTimeout:              2 * time.Second,
		RunnerMessageBytes:           1 << 20,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	daemon := NewDaemon(cfg, logger)
	runContext, cancelRun := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- daemon.Run(runContext) }()

	var result stage5AgentdProviderIsolationResult
	select {
	case result = <-controlPlane.results:
	case runErr := <-runDone:
		cancelRun()
		t.Fatalf("agentd stopped before the A-to-B result: %v; control-plane errors=%v", runErr, controlPlane.Errors())
	case <-time.After(30 * time.Second):
		cancelRun()
		t.Fatalf("timed out waiting for the A-to-B result; control-plane errors=%v", controlPlane.Errors())
	}
	cancelRun()
	select {
	case runErr := <-runDone:
		if runErr != nil && !errors.Is(runErr, context.Canceled) {
			t.Fatalf("agentd shutdown failed: %v", runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agentd did not stop after the acceptance result")
	}
	if result.Error != "" {
		t.Fatal(result.Error)
	}
	if !result.ScrubClaimed || !result.ScrubAcknowledged || result.FirstWorkerID != result.SecondWorkerID {
		t.Fatalf("shared Worker scrub lifecycle was incomplete: %#v", result)
	}
	if result.SeededPathCount != 6 || result.ScannedPathCount == 0 ||
		result.ResidualPathCount != 0 || result.ResidualMarkerReadable {
		t.Fatalf("Tenant B Provider observed Tenant A residue: %#v", result)
	}
	if residuals, scanErr := stage5MarkerPaths(acceptanceRoot, marker); scanErr != nil {
		t.Fatal(scanErr)
	} else if len(residuals) != 0 {
		t.Fatalf("Tenant A marker remained after agentd scrub: %v", residuals)
	}
	t.Logf(
		"Stage 5 real agentd + Provider A-to-B isolation PASS worker=%s marker=%s seeded=%d scanned=%d",
		result.FirstWorkerID, marker, result.SeededPathCount, result.ScannedPathCount,
	)
}

type stage5AgentdProviderIsolationExecution struct {
	execution executions.Execution
	lease     executions.Lease
	workload  executions.Workload
}

type stage5AgentdProviderIsolationResult struct {
	FirstWorkerID          uuid.UUID
	SecondWorkerID         uuid.UUID
	ScrubClaimed           bool
	ScrubAcknowledged      bool
	SeededPathCount        int
	ScannedPathCount       int
	ResidualPathCount      int
	ResidualMarkerReadable bool
	Error                  string
}

type stage5AgentdProviderIsolationControlPlane struct {
	mu sync.Mutex

	targetID          uuid.UUID
	workerID          uuid.UUID
	registrationToken string
	workerToken       string
	marker            string
	executions        [2]stage5AgentdProviderIsolationExecution
	nextExecution     int
	firstCompleted    bool
	scrubClaimed      bool
	scrubAcknowledged bool
	secondCompleted   bool
	seededPathCount   int
	scannedPathCount  int
	residualPathCount int
	residualReadable  bool
	errors            []string
	results           chan stage5AgentdProviderIsolationResult
}

func newStage5AgentdProviderIsolationControlPlane(
	targetID uuid.UUID,
	marker string,
) (*stage5AgentdProviderIsolationControlPlane, error) {
	workerID := uuid.New()
	now := time.Now().UTC()
	controlPlane := &stage5AgentdProviderIsolationControlPlane{
		targetID:          targetID,
		workerID:          workerID,
		registrationToken: "stage5-registration-" + uuid.NewString(),
		workerToken:       "stage5-worker-" + uuid.NewString(),
		marker:            marker,
		results:           make(chan stage5AgentdProviderIsolationResult, 1),
	}
	for index := range controlPlane.executions {
		tenantID := uuid.New()
		executionID := uuid.New()
		sessionID := uuid.New()
		turnID := uuid.New()
		provider := "codex"
		inputText := fmt.Sprintf("[stage5-residue-seed] [stage5-marker:%s]", marker)
		if index == 1 {
			inputText = fmt.Sprintf("[stage5-residue-verify] [stage5-marker:%s]", marker)
		}
		fixture := stage5AgentdProviderIsolationExecution{
			execution: executions.Execution{
				ID: executionID, TenantID: tenantID, SessionID: sessionID, TurnID: turnID,
				Attempt: 1, Status: "claimed", ExecutionTargetID: targetID,
				TargetKind: string(platform.TargetKubernetes), Provider: &provider,
				WorkerID: &workerID, Generation: 1, RequestedBy: uuid.New(), QueuedAt: now,
			},
			lease: executions.Lease{
				ExecutionID: executionID, TenantID: tenantID, WorkerID: workerID, Generation: 1,
				LeaseToken: "stage5-lease-" + uuid.NewString(), AcquiredAt: now,
				HeartbeatAt: now, ExpiresAt: now.Add(time.Minute),
			},
			workload: executions.Workload{
				TenantID: tenantID, OrganizationID: uuid.New(), ProjectID: uuid.New(),
				SessionID: sessionID, TurnID: turnID, SessionTitle: fmt.Sprintf("Stage 5 Tenant %d", index+1),
				Provider: provider, InputText: inputText, TurnKind: "message", RuntimeMode: "full-access",
				InteractionMode: "default", DefaultBranch: "main", MemoryReferences: []executions.RecoveryMemoryReference{},
			},
		}
		if err := freezeStage5AgentdProviderRecoveryBundle(&fixture, now); err != nil {
			return nil, err
		}
		controlPlane.executions[index] = fixture
	}
	return controlPlane, nil
}

type stage5RecoveryBundlePayload struct {
	SchemaVersion                int                                  `json:"schemaVersion"`
	ExecutionID                  uuid.UUID                            `json:"executionId"`
	SessionID                    uuid.UUID                            `json:"sessionId"`
	TurnID                       uuid.UUID                            `json:"turnId"`
	Generation                   int64                                `json:"generation"`
	RecoveryReason               string                               `json:"recoveryReason"`
	PreviousBundleID             *uuid.UUID                           `json:"previousBundleId,omitempty"`
	AuthoritativeHistorySequence int64                                `json:"authoritativeHistorySequence"`
	Execution                    executions.RecoveryExecutionSnapshot `json:"execution"`
	Workload                     executions.Workload                  `json:"workload"`
}

func freezeStage5AgentdProviderRecoveryBundle(
	fixture *stage5AgentdProviderIsolationExecution,
	createdAt time.Time,
) error {
	const historySequence = int64(1)
	fixture.workload.ResumeSnapshot = &executions.ResumeSnapshot{
		Version:            executions.ResumeSnapshotVersionV1,
		SessionID:          fixture.execution.SessionID,
		TurnID:             fixture.execution.TurnID,
		Provider:           fixture.workload.Provider,
		Messages:           []executions.ResumeMessage{},
		ToolResults:        []executions.ResumeToolResult{},
		ArtifactReferences: []executions.ResumeArtifactReference{},
		Mode: executions.ResumeMode{
			RuntimeMode:     fixture.workload.RuntimeMode,
			InteractionMode: fixture.workload.InteractionMode,
		},
		PendingInteractions:          []executions.ResumePendingInteraction{},
		ResumeRecordedInteractions:   []executions.ResumeRecordedInteraction{},
		SourceSequenceRange:          executions.ResumeSequenceRange{},
		AuthoritativeHistorySequence: historySequence,
	}
	executionSnapshot := executions.RecoveryExecutionSnapshot{
		ExecutionTargetID:                   fixture.execution.ExecutionTargetID,
		TargetKind:                          fixture.execution.TargetKind,
		PlacementRegion:                     fixture.execution.PlacementRegion,
		PlacementClusterID:                  fixture.execution.PlacementClusterID,
		TenantSchedulingPolicyVersion:       fixture.execution.TenantSchedulingPolicyVersion,
		TenantSchedulingPolicyDigest:        fixture.execution.TenantSchedulingPolicyDigest,
		OrganizationSchedulingPolicyVersion: fixture.execution.OrganizationSchedulingPolicyVersion,
		OrganizationSchedulingPolicyDigest:  fixture.execution.OrganizationSchedulingPolicyDigest,
		TargetGroupID:                       fixture.execution.TargetGroupID,
		TargetGroupVersion:                  fixture.execution.TargetGroupVersion,
		TargetGroupMemberVersion:            fixture.execution.TargetGroupMemberVersion,
		SelectedRegion:                      fixture.execution.SelectedRegion,
		SelectedClusterID:                   fixture.execution.SelectedClusterID,
		RoutingReason:                       fixture.execution.RoutingReason,
		SchedulingDecisionID:                fixture.execution.SchedulingDecisionID,
		PredecessorExecutionID:              fixture.execution.PredecessorExecutionID,
		WorkerManifestID:                    fixture.execution.WorkerManifestID,
		WorkerReleaseRevisionID:             fixture.execution.WorkerReleaseRevisionID,
		WorkerReleaseChannel:                fixture.execution.WorkerReleaseChannel,
		Provider:                            fixture.execution.Provider,
		ProviderRuntimeBindingID:            fixture.execution.ProviderRuntimeBindingID,
		RemoteWorkspaceID:                   fixture.execution.RemoteWorkspaceID,
		WorkspaceMaterializationID:          fixture.execution.WorkspaceMaterializationID,
		RestoreCheckpointID:                 fixture.execution.RestoreCheckpointID,
	}
	frozenWorkload := fixture.workload
	frozenWorkload.RecoveryBundle = nil
	if frozenWorkload.MemoryReferences == nil {
		frozenWorkload.MemoryReferences = []executions.RecoveryMemoryReference{}
	}
	payload := stage5RecoveryBundlePayload{
		SchemaVersion:                executions.RecoveryBundleSchemaVersionV1,
		ExecutionID:                  fixture.execution.ID,
		SessionID:                    fixture.execution.SessionID,
		TurnID:                       fixture.execution.TurnID,
		Generation:                   fixture.execution.Generation,
		RecoveryReason:               "initial-claim",
		AuthoritativeHistorySequence: historySequence,
		Execution:                    executionSnapshot,
		Workload:                     frozenWorkload,
	}
	_, digest, err := sharedrecoverybundle.Encode(payload)
	if err != nil {
		return err
	}
	fixture.workload.RecoveryBundle = &executions.RecoveryBundle{
		ID: uuid.New(), SchemaVersion: payload.SchemaVersion,
		ExecutionID: payload.ExecutionID, SessionID: payload.SessionID, TurnID: payload.TurnID,
		Generation: payload.Generation, RecoveryReason: payload.RecoveryReason,
		AuthoritativeHistorySequence: payload.AuthoritativeHistorySequence,
		Execution:                    payload.Execution, PayloadSHA256: digest, CreatedAt: createdAt,
	}
	return nil
}

func (c *stage5AgentdProviderIsolationControlPlane) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		http.NotFound(writer, request)
		return
	}
	switch {
	case request.URL.Path == "/v1/workers/register":
		c.register(writer, request)
	case request.URL.Path == "/v1/workers/heartbeat":
		c.authorizedEmpty(writer, request)
	case request.URL.Path == "/v1/workers/storage-scrubs/claim":
		c.claimScrub(writer, request)
	case strings.HasPrefix(request.URL.Path, "/v1/workers/storage-scrubs/") && strings.HasSuffix(request.URL.Path, "/acknowledged"):
		c.acknowledgeScrub(writer, request)
	case strings.HasPrefix(request.URL.Path, "/v1/workers/storage-scrubs/") && strings.HasSuffix(request.URL.Path, "/failed"):
		c.failScrub(writer, request)
	case request.URL.Path == "/v1/workers/executions/claim":
		c.claimExecution(writer, request)
	case strings.HasPrefix(request.URL.Path, "/v1/workers/executions/"):
		c.executionRequest(writer, request)
	default:
		http.NotFound(writer, request)
	}
}

func (c *stage5AgentdProviderIsolationControlPlane) register(writer http.ResponseWriter, request *http.Request) {
	if bearerTokenForStage5Acceptance(request) != c.registrationToken {
		c.writeProblem(writer, http.StatusUnauthorized, "invalid_worker_registration_token", "registration token mismatch")
		return
	}
	var input executions.RegisterWorkerInput
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		c.writeProblem(writer, http.StatusBadRequest, "invalid_registration", err.Error())
		return
	}
	if input.ExecutionTargetID != c.targetID || input.WorkerMode != executions.WorkerModeGeneralPool ||
		input.TargetKind != string(platform.TargetKubernetes) {
		c.writeProblem(writer, http.StatusConflict, "worker_identity_invalid", "registration identity mismatch")
		return
	}
	now := time.Now().UTC()
	c.writeJSON(writer, http.StatusCreated, executions.RegisteredWorker{
		Worker: executions.Worker{
			ID: c.workerID, Incarnation: 1, InstanceUID: input.InstanceUID,
			ExecutionTargetID: c.targetID, TargetKind: input.TargetKind, WorkerMode: input.WorkerMode,
			ClusterID: input.ClusterID, Namespace: input.Namespace, PodName: input.PodName,
			Version: input.Version, ProtocolVersion: input.ProtocolVersion, Capabilities: input.Capabilities,
			CompatibilityStatus: "compatible", WorkerReleaseStatus: "compatible",
			LeaseSupported: true, FencingSupported: true, Status: "idle", AdministrativeStatus: "active",
			RegisteredAt: now, LastHeartbeatAt: now,
		},
		Token: c.workerToken,
	})
}

func (c *stage5AgentdProviderIsolationControlPlane) claimScrub(writer http.ResponseWriter, request *http.Request) {
	if !c.requireWorker(writer, request) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.firstCompleted || c.scrubAcknowledged {
		c.writeJSON(writer, http.StatusOK, executions.WorkerStorageScrubClaimResult{})
		return
	}
	c.scrubClaimed = true
	first := c.executions[0]
	c.writeJSON(writer, http.StatusOK, executions.WorkerStorageScrubClaimResult{
		Scrub: &executions.WorkerStorageScrub{
			ID:                uuid.NewSHA1(c.workerID, []byte("stage5-storage-scrub")),
			ExecutionTargetID: c.targetID, TenantID: first.execution.TenantID,
			ScopeKind: "execution", ScopeID: first.execution.ID, ScopeGeneration: first.lease.Generation,
			ScrubGeneration: 1, Status: "pending", CreatedAt: time.Now().UTC(),
		},
	})
}

func (c *stage5AgentdProviderIsolationControlPlane) acknowledgeScrub(writer http.ResponseWriter, request *http.Request) {
	if !c.requireWorker(writer, request) {
		return
	}
	var input executions.WorkerStorageScrubReceiptInput
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil || input.ScrubGeneration != 1 {
		c.writeProblem(writer, http.StatusBadRequest, "worker_storage_scrub_receipt_invalid", "invalid scrub receipt")
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.scrubClaimed {
		c.writeProblem(writer, http.StatusConflict, "worker_storage_scrub_not_claimed", "scrub was not claimed")
		return
	}
	c.scrubAcknowledged = true
	c.writeJSON(writer, http.StatusOK, map[string]any{"acknowledged": true})
}

func (c *stage5AgentdProviderIsolationControlPlane) failScrub(writer http.ResponseWriter, request *http.Request) {
	if !c.requireWorker(writer, request) {
		return
	}
	var input executions.WorkerStorageScrubFailureInput
	_ = json.NewDecoder(request.Body).Decode(&input)
	c.publishFailure("agentd reported storage scrub failure: " + input.FailureCode + ":" + input.FailureMessage)
	c.writeJSON(writer, http.StatusOK, map[string]any{"failed": true})
}

func (c *stage5AgentdProviderIsolationControlPlane) claimExecution(writer http.ResponseWriter, request *http.Request) {
	if !c.requireWorker(writer, request) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.nextExecution >= len(c.executions) {
		c.writeJSON(writer, http.StatusOK, executions.ClaimResult{})
		return
	}
	if c.nextExecution == 1 && !c.scrubAcknowledged {
		c.recordErrorLocked("Tenant B claim was attempted before agentd acknowledged the storage scrub")
		c.writeProblem(writer, http.StatusConflict, "worker_storage_scrub_required", "storage scrub is required")
		return
	}
	fixture := c.executions[c.nextExecution]
	c.nextExecution++
	c.writeJSON(writer, http.StatusOK, executions.ClaimResult{
		Execution: &fixture.execution, Lease: &fixture.lease, Workload: &fixture.workload,
	})
}

func (c *stage5AgentdProviderIsolationControlPlane) executionRequest(writer http.ResponseWriter, request *http.Request) {
	if !c.requireWorker(writer, request) {
		return
	}
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/v1/workers/executions/"), "/")
	if len(parts) < 2 {
		http.NotFound(writer, request)
		return
	}
	executionID, err := uuid.Parse(parts[0])
	if err != nil {
		c.writeProblem(writer, http.StatusBadRequest, "invalid_execution_id", "execution ID is invalid")
		return
	}
	operation := strings.Join(parts[1:], "/")
	switch operation {
	case "start", "events", "release":
		c.writeJSON(writer, http.StatusOK, map[string]any{"accepted": true})
	case "renew":
		fixture, ok := c.executionByID(executionID)
		if !ok {
			c.writeProblem(writer, http.StatusNotFound, "execution_not_found", "execution does not exist")
			return
		}
		fixture.lease.HeartbeatAt = time.Now().UTC()
		fixture.lease.ExpiresAt = fixture.lease.HeartbeatAt.Add(time.Minute)
		c.writeJSON(writer, http.StatusOK, fixture.lease)
	case "control-updates/pull":
		c.writeJSON(writer, http.StatusOK, executions.ControlUpdates{
			ControlCommands:        []executions.ControlCommandDelivery{},
			InteractionResolutions: []executions.InteractionResolutionDelivery{},
		})
	case "resource-directives/pull":
		c.writeJSON(writer, http.StatusOK, map[string]any{"directive": nil})
	case "complete":
		c.completeExecution(writer, request, executionID)
	case "fail":
		var input executions.FailExecutionInput
		_ = json.NewDecoder(request.Body).Decode(&input)
		c.publishFailure(fmt.Sprintf("agentd failed Execution %s: %s:%s", executionID, input.FailureCode, input.FailureMessage))
		c.writeJSON(writer, http.StatusOK, map[string]any{"failed": true})
	default:
		http.NotFound(writer, request)
	}
}

func (c *stage5AgentdProviderIsolationControlPlane) completeExecution(
	writer http.ResponseWriter,
	request *http.Request,
	executionID uuid.UUID,
) {
	var input executions.CompleteExecutionInput
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		c.writeProblem(writer, http.StatusBadRequest, "invalid_completion", err.Error())
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	index := -1
	for candidate := range c.executions {
		if c.executions[candidate].execution.ID == executionID {
			index = candidate
			break
		}
	}
	if index < 0 {
		c.writeProblem(writer, http.StatusNotFound, "execution_not_found", "execution does not exist")
		return
	}
	evidence, evidenceErr := stage5EvidenceFromCompletion(input.Output, c.marker)
	if evidenceErr != nil {
		c.recordErrorLocked(fmt.Sprintf("Execution %s returned invalid Stage 5 evidence: %v", executionID, evidenceErr))
		c.writeProblem(writer, http.StatusConflict, "stage5_evidence_invalid", evidenceErr.Error())
		return
	}
	if index == 0 {
		if evidence.Stage != "seeded" || evidence.SeededPathCount != 6 {
			c.recordErrorLocked(fmt.Sprintf("Tenant A seed evidence is invalid: %#v", evidence))
			c.writeProblem(writer, http.StatusConflict, "stage5_seed_invalid", "Tenant A did not seed the expected residue set")
			return
		}
		c.firstCompleted = true
		c.seededPathCount = evidence.SeededPathCount
	} else {
		if evidence.Stage != "verified" {
			c.recordErrorLocked(fmt.Sprintf("Tenant B verification evidence is invalid: %#v", evidence))
			c.writeProblem(writer, http.StatusConflict, "stage5_verification_invalid", "Tenant B did not verify storage")
			return
		}
		c.secondCompleted = true
		c.scannedPathCount = evidence.ScannedPathCount
		c.residualPathCount = evidence.ResidualPathCount
		c.residualReadable = evidence.ResidualMarkerReadable
	}
	c.writeJSON(writer, http.StatusOK, map[string]any{"completed": true})
	if index == 1 {
		result := c.resultLocked()
		select {
		case c.results <- result:
		default:
		}
	}
}

type stage5CompletionEvidence struct {
	Stage                  string
	SeededPathCount        int
	ScannedPathCount       int
	ResidualPathCount      int
	ResidualMarkerReadable bool
}

func stage5EvidenceFromCompletion(output map[string]any, marker string) (stage5CompletionEvidence, error) {
	raw, ok := output["stage5TenantIsolationEvidence"].(map[string]any)
	if !ok {
		return stage5CompletionEvidence{}, errors.New("stage5TenantIsolationEvidence is missing")
	}
	if actual, _ := raw["marker"].(string); actual != marker {
		return stage5CompletionEvidence{}, errors.New("Stage 5 marker does not match")
	}
	return stage5CompletionEvidence{
		Stage:                  stringFromStage5Evidence(raw["stage"]),
		SeededPathCount:        intFromStage5Evidence(raw["seededPathCount"]),
		ScannedPathCount:       intFromStage5Evidence(raw["scannedPathCount"]),
		ResidualPathCount:      intFromStage5Evidence(raw["residualPathCount"]),
		ResidualMarkerReadable: boolFromStage5Evidence(raw["residualMarkerReadable"]),
	}, nil
}

func stringFromStage5Evidence(value any) string {
	result, _ := value.(string)
	return result
}

func intFromStage5Evidence(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	default:
		return 0
	}
}

func boolFromStage5Evidence(value any) bool {
	result, _ := value.(bool)
	return result
}

func (c *stage5AgentdProviderIsolationControlPlane) authorizedEmpty(writer http.ResponseWriter, request *http.Request) {
	if !c.requireWorker(writer, request) {
		return
	}
	c.writeJSON(writer, http.StatusOK, map[string]any{})
}

func (c *stage5AgentdProviderIsolationControlPlane) requireWorker(writer http.ResponseWriter, request *http.Request) bool {
	if bearerTokenForStage5Acceptance(request) == c.workerToken {
		return true
	}
	c.writeProblem(writer, http.StatusUnauthorized, "worker_token_invalid", "worker token mismatch")
	return false
}

func bearerTokenForStage5Acceptance(request *http.Request) string {
	value := strings.TrimSpace(request.Header.Get("Authorization"))
	if len(value) <= len("Bearer ") || !strings.EqualFold(value[:len("Bearer ")], "Bearer ") {
		return ""
	}
	return strings.TrimSpace(value[len("Bearer "):])
}

func (c *stage5AgentdProviderIsolationControlPlane) executionByID(
	executionID uuid.UUID,
) (stage5AgentdProviderIsolationExecution, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, fixture := range c.executions {
		if fixture.execution.ID == executionID {
			return fixture, true
		}
	}
	return stage5AgentdProviderIsolationExecution{}, false
}

func (c *stage5AgentdProviderIsolationControlPlane) resultLocked() stage5AgentdProviderIsolationResult {
	result := stage5AgentdProviderIsolationResult{
		FirstWorkerID: c.workerID, SecondWorkerID: c.workerID,
		ScrubClaimed: c.scrubClaimed, ScrubAcknowledged: c.scrubAcknowledged,
		SeededPathCount: c.seededPathCount, ScannedPathCount: c.scannedPathCount,
		ResidualPathCount: c.residualPathCount, ResidualMarkerReadable: c.residualReadable,
	}
	if len(c.errors) > 0 {
		result.Error = strings.Join(c.errors, "; ")
	}
	return result
}

func (c *stage5AgentdProviderIsolationControlPlane) recordErrorLocked(message string) {
	for _, existing := range c.errors {
		if existing == message {
			return
		}
	}
	c.errors = append(c.errors, message)
}

func (c *stage5AgentdProviderIsolationControlPlane) publishFailure(message string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recordErrorLocked(message)
	select {
	case c.results <- c.resultLocked():
	default:
	}
}

func (c *stage5AgentdProviderIsolationControlPlane) Errors() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.errors...)
}

func (c *stage5AgentdProviderIsolationControlPlane) writeProblem(
	writer http.ResponseWriter,
	status int,
	code string,
	message string,
) {
	c.writeJSON(writer, status, map[string]any{"error": map[string]any{"code": code, "message": message}})
}

func (c *stage5AgentdProviderIsolationControlPlane) writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func stage5MarkerPaths(root, marker string) ([]string, error) {
	paths := make([]string, 0)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if strings.Contains(entry.Name(), marker) {
				paths = append(paths, path)
			}
			return nil
		}
		if strings.Contains(entry.Name(), marker) {
			paths = append(paths, path)
		}
		if entry.IsDir() {
			return nil
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(contents), marker) {
			paths = append(paths, path)
		}
		return nil
	})
	return paths, err
}
