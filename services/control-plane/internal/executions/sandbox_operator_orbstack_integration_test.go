package executions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/projects"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

const sandboxOperatorControlPlaneIntegrationEnv = "SYNARA_SANDBOX_OPERATOR_CONTROL_PLANE_TEST"

func TestSandboxOperatorRealControlPlaneRegistrationAndGenerationFencing(t *testing.T) {
	if os.Getenv(sandboxOperatorControlPlaneIntegrationEnv) != "1" {
		t.Skip(sandboxOperatorControlPlaneIntegrationEnv + "=1 is required")
	}
	apiServer := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_API_SERVER"))
	bearerToken := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_BEARER_TOKEN"))
	caCertificate := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_CA_CERTIFICATE"))
	workerImage := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_WORKER_IMAGE"))
	kubernetesContext := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_CONTEXT"))
	allocationBackend := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_ALLOCATION_BACKEND"))
	if allocationBackend == "" {
		allocationBackend = "sandbox-operator-standard"
	}
	templateRuntime := "standard"
	if allocationBackend == "sandbox-operator-cocoon" {
		templateRuntime = "vk-cocoon"
	} else if allocationBackend != "sandbox-operator-standard" {
		t.Fatalf("SYNARA_TEST_KUBERNETES_ALLOCATION_BACKEND = %q, want sandbox-operator-standard or sandbox-operator-cocoon", allocationBackend)
	}
	if strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_NODE_LOSS_HOOK")) != "" && allocationBackend != "sandbox-operator-cocoon" {
		t.Fatal("SYNARA_TEST_KUBERNETES_NODE_LOSS_HOOK requires sandbox-operator-cocoon")
	}
	if apiServer == "" || bearerToken == "" || caCertificate == "" || workerImage == "" || kubernetesContext == "" {
		t.Fatal("Kubernetes API, bearer token, CA certificate, worker image, and context are required")
	}
	testTimeout := 8 * time.Minute
	if configured := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_TIMEOUT")); configured != "" {
		parsed, err := time.ParseDuration(configured)
		if err != nil || parsed < 2*time.Minute || parsed > 30*time.Minute {
			t.Fatalf("SYNARA_TEST_KUBERNETES_TIMEOUT = %q, want a duration from 2m through 30m", configured)
		}
		testTimeout = parsed
	}
	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()
	platformConfig, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, platformConfig, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "sandbox-control-plane-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := secret.NewCursorCipher(bytes.Repeat([]byte{0x93}, 32))
	if err != nil {
		t.Fatal(err)
	}
	targetService := executiontargets.NewService(store.DB(), platformConfig, cipher)
	executionService := NewService(
		store.DB(), sessions.NewService(store.DB(), projects.NewService(store.DB()), targetService),
		30*time.Second, 2*time.Minute, time.Hour, cipher, targetService,
	)
	workerAPI := newSandboxControlPlaneWorkerAPI(targetService, executionService)
	server := httptest.NewServer(workerAPI)
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	controlPlaneHost := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_CONTROL_PLANE_HOST"))
	if controlPlaneHost == "" {
		controlPlaneHost = "host.docker.internal"
	}
	controlPlaneURL := "http://" + controlPlaneHost + ":" + serverURL.Port()
	namespace := "synara-sandbox-cp-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10]
	serviceAccount := "synara-stage-b"
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	targetCapabilities := workerManifestTestTargetCapabilities()
	targetCapabilities["workspaceModes"] = []string{"local", "worktree"}
	target, err := targetService.Create(ctx, principal, domain.TenantID, executiontargets.CreateInput{
		OrganizationID: &domain.OrganizationID, Kind: "kubernetes", Name: "sandbox-control-plane",
		Configuration: map[string]any{
			"allocationBackend": allocationBackend, "sandboxTemplateName": "synara-worker",
			"sandboxWarmPoolName": "synara-worker-interactive", "sandboxClaimReadyTimeoutSeconds": 180,
			"sandboxAllowedTenantIds": []string{domain.TenantID.String()},
			"apiServer":               apiServer, "bearerToken": bearerToken, "caCertificate": caCertificate,
			"namespace": namespace, "manageNamespace": false, "serviceAccountName": serviceAccount,
			"image": workerImage, "imagePullPolicy": "IfNotPresent", "controlPlaneUrl": controlPlaneURL,
			"allowInsecureControlPlane": true, "runnerCommand": []string{"provider-host", "run", "--jsonl"},
			"maxActivePods": 4, "egressCidrs": []string{"0.0.0.0/0"},
			"cpuRequest": "100m", "cpuLimit": "2", "pidsLimit": 512, "memoryRequest": "256Mi", "memoryLimit": "2Gi",
			"ephemeralStorageRequest": "512Mi", "ephemeralStorageLimit": "4Gi",
			"workspaceSizeLimit": "256Mi", "quotaCpuRequests": "2", "quotaCpuLimits": "8",
			"quotaMemoryRequests": "2Gi", "quotaMemoryLimits": "8Gi", "quotaEphemeralStorage": "4Gi",
		},
		Capabilities: targetCapabilities,
	})
	if err != nil {
		t.Fatal(err)
	}
	workerAPI.targetID = target.ID
	workerAPI.targetKind = target.Kind

	now := time.Now().UTC().Truncate(time.Microsecond)
	projectID, sessionID, turnID, executionID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	workerAPI.blockedStartExecutionID = executionID
	providerCredentialID := uuid.New()
	providerCredentialVersion := 1
	models := []any{
		&persistence.Project{ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID, Name: "Sandbox control plane", DefaultBranch: "main", Visibility: "organization", CreatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now},
		&persistence.WorkerPool{
			ID: uuid.New(), TenantID: &domain.TenantID, ExecutionTargetID: target.ID,
			Name: "sandbox-interactive", Mode: "warm", CapacityClass: "interactive", TenantIsolation: "pinned",
			Namespace: namespace, DesiredIdleUnits: 1, MinIdleUnits: 1, MaxActiveUnits: 4,
			SchedulingTemplate: map[string]any{}, Status: "active", Version: 1, CreatedAt: now, UpdatedAt: now,
		},
		&persistence.ProviderCredential{ID: providerCredentialID, TenantID: domain.TenantID, OrganizationID: &domain.OrganizationID, Scope: "organization", Name: "Sandbox isolated fixture", Purpose: "provider", Provider: "codex", CredentialType: "api_key", EncryptedPayload: []byte("opaque-test-ciphertext"), EncryptedDataKey: []byte("opaque-test-key"), KMSProvider: "test", KMSKeyID: "sandbox-stage-b", AADVersion: 3, Version: providerCredentialVersion, CreatedBy: domain.UserID, UpdatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now},
		&persistence.AgentSession{ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID, ProjectID: projectID, CreatedBy: domain.UserID, Title: "Sandbox control plane", Status: "active", Visibility: "organization", Provider: "codex", ProviderCredentialID: &providerCredentialID, ExecutionTargetID: target.ID, ResourceState: "provisioning", MeaningfulActivityAt: now, SuspendAfterIdleSeconds: 300, CreatedAt: now, UpdatedAt: now},
		&persistence.AgentTurn{ID: turnID, TenantID: domain.TenantID, SessionID: sessionID, CreatedBy: domain.UserID, Status: "queued", InputText: "[credential] sandbox control plane", TurnKind: "message", RuntimeMode: "full-access", InteractionMode: "default", CreatedAt: now},
		&persistence.AgentExecution{ID: executionID, TenantID: domain.TenantID, SessionID: sessionID, TurnID: turnID, Attempt: 1, Status: "scheduled", ExecutionTargetID: target.ID, TargetKind: "kubernetes", Provider: sandboxStringPointer("codex"), Generation: 0, RequestedBy: domain.UserID, QueuedAt: now},
		&persistence.ExecutionGenerationFact{TenantID: domain.TenantID, ExecutionID: executionID, Generation: 1, SessionID: sessionID, TurnID: turnID, ExecutionTargetID: target.ID, TargetKind: "kubernetes", Provider: "codex", RecoveryReason: "initial-claim", WarmPoolMode: "disabled", WarmPoolResult: "not-requested", DispatchRequestedAt: &now, CreatedAt: now, UpdatedAt: now},
	}
	for _, model := range models {
		if err := store.DB().Create(model).Error; err != nil {
			t.Fatal(err)
		}
	}
	applySandboxControlPlaneTemplate(t, ctx, kubernetesContext, namespace, target.ID, executionID, serviceAccount, workerImage, controlPlaneURL, templateRuntime)
	t.Cleanup(func() {
		sandboxKubectl(t, context.Background(), kubernetesContext, nil, "delete", "namespace", namespace, "--ignore-not-found", "--wait=true", "--timeout=120s")
	})

	reconciler := executiontargets.NewKubernetesReconciler(targetService, executiontargets.KubernetesReconcilerConfig{
		PublicControlPlaneURL: controlPlaneURL, WorkerLeaseTTL: 30 * time.Second, WorkerHeartbeatTimeout: 2 * time.Minute,
	}, slog.Default())
	prewarmDeadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(prewarmDeadline) {
		reconcileErr := reconciler.ReconcileOnce(ctx)
		if reconcileErr != nil {
			var apiError *problem.Error
			if !errors.As(reconcileErr, &apiError) || apiError.Status < 500 {
				t.Fatalf("prewarm Sandbox pool: %v", reconcileErr)
			}
		}
		desired, ready := sandboxWarmPoolCapacity(t, ctx, kubernetesContext, namespace, "synara-worker-interactive")
		if desired == 1 && ready == 1 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	desired, ready := sandboxWarmPoolCapacity(t, ctx, kubernetesContext, namespace, "synara-worker-interactive")
	if desired != 1 || ready != 1 {
		t.Fatalf("SandboxWarmPool did not prewarm before dispatch: desired=%d ready=%d", desired, ready)
	}
	if err := store.DB().Model(&persistence.AgentExecution{}).Where("id = ?", executionID).Update("status", "queued").Error; err != nil {
		t.Fatal(err)
	}
	var registered RegisteredWorker
	registeredOK := false
	var lastReconcileErr error
	lastReconcileErrorText := ""
	providerReadyStartedAt := time.Now()
	for !registeredOK {
		select {
		case registered = <-workerAPI.registered:
			registeredOK = true
		default:
			lastReconcileErr = reconciler.ReconcileOnce(ctx)
			if lastReconcileErr != nil && lastReconcileErr.Error() != lastReconcileErrorText {
				lastReconcileErrorText = lastReconcileErr.Error()
				t.Logf("Sandbox registration reconciliation pending: %v", lastReconcileErr)
			}
			if ctx.Err() != nil {
				t.Fatalf("wait for real control-plane registration: %v, last reconcile: %v", ctx.Err(), lastReconcileErr)
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
	providerReadyDuration := time.Since(providerReadyStartedAt)
	var allocation persistence.ExecutionKubernetesAllocation
	if err := store.DB().Where("execution_id = ? AND generation = ?", executionID, 1).Take(&allocation).Error; err != nil {
		t.Fatal(err)
	}
	if allocation.Status != "bound" || allocation.SandboxName == nil || allocation.PodName == nil || allocation.PodUID == nil ||
		registered.Worker.AssignedExecutionID == nil || *registered.Worker.AssignedExecutionID != executionID ||
		registered.Worker.PodName != *allocation.PodName || registered.Worker.InstanceUID != *allocation.PodUID ||
		!workerAPI.observedProviderHost() {
		t.Fatalf("registered Worker/allocation identity mismatch: worker=%#v allocation=%#v", registered.Worker, allocation)
	}
	launchType := strings.TrimSpace(string(sandboxKubectlOutput(
		t, ctx, kubernetesContext, nil, "-n", namespace, "get", "sandbox", *allocation.SandboxName,
		"-o", "jsonpath={.metadata.labels.agents\\.x-k8s\\.io/launch-type}",
	)))
	if launchType != "warm" {
		t.Fatalf("initial Sandbox launch type = %q, want warm", launchType)
	}
	var claimed ClaimResult
	select {
	case claimed = <-workerAPI.claimed:
	case <-ctx.Done():
		t.Fatal("agentd did not claim the allocated Sandbox Execution")
	}
	if claimed.Execution == nil || claimed.Lease == nil || claimed.Workload == nil || claimed.Workload.ProviderCredentialGrantID == nil {
		t.Fatalf("agentd claim is incomplete: %#v", claimed)
	}
	bundle := claimed.Workload.RecoveryBundle
	if bundle == nil || bundle.SchemaVersion != RecoveryBundleSchemaVersionV1 ||
		bundle.ExecutionID != executionID || bundle.Generation != 1 ||
		bundle.Execution.ExecutionTargetID != target.ID || bundle.Execution.TargetKind != string(platform.TargetKubernetes) ||
		bundle.PayloadSHA256 == "" {
		t.Fatalf("Sandbox Worker Recovery Bundle is incomplete: %#v", bundle)
	}
	if err := ValidateRecoveryBundle(*claimed.Execution, *claimed.Workload); err != nil {
		t.Fatalf("validate Sandbox Worker Recovery Bundle with agentd claim guard: %v", err)
	}
	var persistedBundle persistence.ExecutionRecoveryBundle
	if err := store.DB().Where("tenant_id = ? AND execution_id = ? AND generation = ?", domain.TenantID, executionID, 1).
		Take(&persistedBundle).Error; err != nil {
		t.Fatal(err)
	}
	if persistedBundle.ID != bundle.ID || persistedBundle.PayloadSHA256 != bundle.PayloadSHA256 {
		t.Fatalf("persisted Recovery Bundle differs from claimed bundle: persisted=%#v claimed=%#v", persistedBundle, bundle)
	}
	select {
	case blockedGeneration := <-workerAPI.startBlocked:
		if blockedGeneration != 1 {
			t.Fatalf("blocked Provider start Generation = %d, want 1", blockedGeneration)
		}
	case <-ctx.Done():
		t.Fatal("agentd did not reach the Generation-1 Provider start fence")
	}
	assertSandboxControlPlaneIsolationAndQuota(t, ctx, kubernetesContext, namespace, target.ID, *allocation.PodName)
	nodeLossEvidence := runSandboxBackingPodLossAcceptance(
		t, ctx, store.DB(), reconciler, targetService, workerAPI, kubernetesContext,
		namespace, target.ID, executionID, bundle.ID, allocation,
	)
	credentialResolved, crossTenantRejected, credentialEventProof, credentialLeaked := workerAPI.credentialEvidence()
	if !credentialResolved || !crossTenantRejected || !credentialEventProof || credentialLeaked {
		t.Fatalf(
			"Provider Credential isolation evidence after recovery: resolved=%t crossTenantRejected=%t eventProof=%t leaked=%t",
			credentialResolved, crossTenantRejected, credentialEventProof, credentialLeaked,
		)
	}
	if nodeLossEvidence.RecoveredClaim.Workload == nil || nodeLossEvidence.RecoveredClaim.Workload.ProviderCredentialGrantID == nil {
		t.Fatalf("recovered Generation omitted its Provider Credential Grant: %#v", nodeLossEvidence.RecoveredClaim)
	}
	var providerGrant persistence.ExecutionProviderCredentialGrant
	if err := store.DB().Where("id = ?", *nodeLossEvidence.RecoveredClaim.Workload.ProviderCredentialGrantID).Take(&providerGrant).Error; err != nil {
		t.Fatal(err)
	}
	if providerGrant.TenantID != domain.TenantID || providerGrant.ExecutionID != executionID ||
		providerGrant.Generation != 2 || providerGrant.CredentialID != providerCredentialID ||
		providerGrant.CredentialVersion != providerCredentialVersion {
		t.Fatalf("recovered Provider Credential Grant is not generation-bound: %#v", providerGrant)
	}
	var storedCredential persistence.ProviderCredential
	if err := store.DB().Where("id = ?", providerCredentialID).Take(&storedCredential).Error; err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(storedCredential.EncryptedPayload, []byte(sandboxProviderCredentialSentinel)) ||
		bytes.Contains(storedCredential.EncryptedDataKey, []byte(sandboxProviderCredentialSentinel)) {
		t.Fatal("Provider Credential plaintext leaked into persisted credential fields")
	}
	restartEvidence := runSandboxOperatorConcurrentRestartAcceptance(
		t, ctx, store.DB(), reconciler, workerAPI, kubernetesContext, namespace,
		domain, target.ID, projectID, providerCredentialID,
	)
	t.Logf("Sandbox real control-plane PASS target=%s execution=%s worker=%s claim=%s pod=%s/%s initialLaunchType=%s providerReady=%s recoveryBundle=%s staleCode=%s nodeLossMode=%s nodeLossRecoveryAuthority=%s nodeLossOldNode=%s nodeLossNewNode=%s nodeLossOldPodUID=%s nodeLossNewPodUID=%s nodeLossRecovery=%s restartExecutions=%v restartRecovery=%s leaderBefore=%s leaderAfter=%s failover=%s maxWarmPoolDeficit=%d finalWarmPoolDeficit=%d", target.ID, executionID, registered.Worker.ID, allocation.ClaimName, *allocation.PodName, *allocation.PodUID, launchType, providerReadyDuration, bundle.ID, nodeLossEvidence.StaleCode, nodeLossEvidence.Mode, nodeLossEvidence.RecoveryAuthority, nodeLossEvidence.OldNode, nodeLossEvidence.NewNode, nodeLossEvidence.OldPodUID, nodeLossEvidence.NewPodUID, nodeLossEvidence.RecoveryDuration, restartEvidence.ExecutionIDs, restartEvidence.RecoveryDuration, restartEvidence.LeaderBefore, restartEvidence.LeaderAfter, restartEvidence.FailoverDuration, restartEvidence.MaxWarmPoolDeficit, restartEvidence.FinalWarmPoolDeficit)
}

type sandboxBackingPodLossEvidence struct {
	OldPodUID         string
	NewPodUID         string
	Mode              string
	OldNode           string
	NewNode           string
	StaleCode         string
	RecoveryAuthority string
	RecoveryDuration  time.Duration
	RecoveredClaim    ClaimResult
}

func runSandboxBackingPodLossAcceptance(
	t *testing.T,
	ctx context.Context,
	db *gorm.DB,
	reconciler *executiontargets.KubernetesReconciler,
	targetService *executiontargets.Service,
	workerAPI *sandboxControlPlaneWorkerAPI,
	kubernetesContext string,
	namespace string,
	targetID uuid.UUID,
	executionID uuid.UUID,
	previousBundleID uuid.UUID,
	previousAllocation persistence.ExecutionKubernetesAllocation,
) sandboxBackingPodLossEvidence {
	t.Helper()
	if previousAllocation.ClaimUID == nil || previousAllocation.PodName == nil || previousAllocation.PodUID == nil {
		t.Fatalf("backing Pod loss requires a fully bound allocation: %#v", previousAllocation)
	}
	oldPodName, oldPodUID := *previousAllocation.PodName, *previousAllocation.PodUID
	oldRegistrationBearer := workerAPI.registrationBearer()
	loss := injectSandboxBackingPodLoss(t, ctx, kubernetesContext, namespace, oldPodName)
	workerAPI.releaseBlockedStart()
	recoveryStartedAt := time.Now()
	leaseExpiryDeadline := time.Now().Add(45 * time.Second)
	var lostLease persistence.WorkerLease
	for time.Now().Before(leaseExpiryDeadline) {
		if err := db.WithContext(ctx).Where(
			"execution_id = ? AND generation = ?", executionID, previousAllocation.Generation,
		).Take(&lostLease).Error; err != nil {
			t.Fatalf("observe lost backing Pod lease: %v", err)
		}
		if !lostLease.ExpiresAt.After(workerAPI.executions.now()) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if lostLease.ExpiresAt.After(workerAPI.executions.now()) {
		t.Fatalf("lost backing Pod lease kept renewing after Pod deletion: expiresAt=%s now=%s", lostLease.ExpiresAt, workerAPI.executions.now())
	}
	if err := workerAPI.executions.RecoverExpired(ctx, 10); err != nil {
		t.Fatalf("recover expired backing Pod lease: %v", err)
	}
	var recoveringExecution persistence.AgentExecution
	if err := db.WithContext(ctx).Where("id = ?", executionID).Take(&recoveringExecution).Error; err != nil {
		t.Fatal(err)
	}
	if recoveringExecution.Status != "recovering" || recoveringExecution.WorkerID != nil ||
		recoveringExecution.Generation != previousAllocation.Generation {
		t.Fatalf("lease expiry did not authoritatively enter recovery: %#v", recoveringExecution)
	}
	var recoveryFact persistence.ExecutionGenerationFact
	if err := db.WithContext(ctx).Where(
		"execution_id = ? AND generation = ?", executionID, previousAllocation.Generation+1,
	).Take(&recoveryFact).Error; err != nil {
		t.Fatalf("load lease-expiry recovery Generation fact: %v", err)
	}
	if recoveryFact.RecoveryReason != "execution-recovery" || recoveryFact.DispatchRequestedAt == nil {
		t.Fatalf("lease expiry did not persist a dispatchable recovery Generation: %#v", recoveryFact)
	}

	var replacement RegisteredWorker
	registered := false
	deadline := time.Now().Add(2 * time.Minute)
	for !registered && time.Now().Before(deadline) {
		select {
		case candidate := <-workerAPI.registered:
			if candidate.Worker.AssignedExecutionID != nil && *candidate.Worker.AssignedExecutionID == executionID &&
				candidate.Worker.InstanceUID != oldPodUID {
				replacement = candidate
				registered = true
			}
		default:
			if err := reconciler.ReconcileOnce(ctx); err != nil {
				var apiError *problem.Error
				if !errors.As(err, &apiError) || apiError.Status < 500 {
					t.Fatalf("reconcile backing Pod loss recovery: %v", err)
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	if !registered {
		t.Fatalf("replacement Sandbox did not register after backing Pod loss; registrationErrors=%v", workerAPI.registrationErrorEvidence())
	}
	var replacementAllocation persistence.ExecutionKubernetesAllocation
	if err := db.WithContext(ctx).Where(
		"execution_id = ? AND generation = ?", executionID, previousAllocation.Generation+1,
	).Take(&replacementAllocation).Error; err != nil {
		t.Fatal(err)
	}
	if replacementAllocation.Status != "bound" || replacementAllocation.ClaimUID == nil ||
		replacementAllocation.PodUID == nil || replacementAllocation.PodName == nil ||
		*replacementAllocation.ClaimUID == *previousAllocation.ClaimUID ||
		*replacementAllocation.PodUID == oldPodUID ||
		replacement.Worker.InstanceUID != *replacementAllocation.PodUID {
		t.Fatalf("backing Pod loss reused a fenced physical identity: old=%#v replacement=%#v worker=%#v", previousAllocation, replacementAllocation, replacement.Worker)
	}
	newNode := strings.TrimSpace(string(sandboxKubectlOutput(
		t, ctx, kubernetesContext, nil, "-n", namespace, "get", "pod", *replacementAllocation.PodName,
		"-o", "jsonpath={.spec.nodeName}",
	)))
	if newNode == "" || (loss.Mode == "node-hook" && newNode == loss.OldNode) {
		t.Fatalf("node-loss replacement placement is not distinct: mode=%s oldNode=%q newNode=%q", loss.Mode, loss.OldNode, newNode)
	}
	var recovered ClaimResult
	select {
	case recovered = <-workerAPI.claimed:
	case <-time.After(30 * time.Second):
		t.Fatal("replacement agentd did not claim the backing Pod loss recovery Generation")
	}
	if recovered.Execution == nil || recovered.Workload == nil || recovered.Workload.RecoveryBundle == nil ||
		recovered.Execution.Generation != previousAllocation.Generation+1 ||
		recovered.Workload.RecoveryBundle.PreviousBundleID == nil ||
		*recovered.Workload.RecoveryBundle.PreviousBundleID != previousBundleID {
		t.Fatalf("backing Pod loss Recovery Bundle did not preserve lineage: %#v", recovered)
	}
	select {
	case completedID := <-workerAPI.completed:
		if completedID != executionID {
			t.Fatalf("backing Pod loss completed Execution %s, want %s", completedID, executionID)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("replacement agentd did not complete the backing Pod loss recovery Generation")
	}
	var recoveredExecution persistence.AgentExecution
	if err := db.WithContext(ctx).Where("id = ?", executionID).Take(&recoveredExecution).Error; err != nil {
		t.Fatal(err)
	}
	if recoveredExecution.Status != "completed" || recoveredExecution.Generation != previousAllocation.Generation+1 {
		t.Fatalf("replacement Generation did not reach completed before stale-fence probe: %#v", recoveredExecution)
	}
	staleDispatchedAt := time.Now().UTC().Truncate(time.Microsecond)
	if err := workerAPI.executions.recordExecutionGenerationDispatchRequested(
		ctx, db, recoveredExecution, recoveredExecution.Generation+1, staleDispatchedAt, "execution-recovery",
	); err != nil {
		t.Fatalf("persist stale-fence probe Generation: %v", err)
	}
	staleErr := targetService.VerifyWorkerRegistration(
		ctx, targetID, namespace, *replacementAllocation.PodName, *replacementAllocation.PodUID,
		workerAPI.registrationBearer(),
	)
	var staleProblem *problem.Error
	if !errors.As(staleErr, &staleProblem) || staleProblem.Code != "kubernetes_sandbox_allocation_generation_stale" {
		t.Fatalf("recovered Pod stale Generation registration = %v, want kubernetes_sandbox_allocation_generation_stale", staleErr)
	}
	if err := targetService.VerifyWorkerRegistration(
		ctx, targetID, namespace, oldPodName, oldPodUID, oldRegistrationBearer,
	); err == nil {
		t.Fatal("the lost physical Pod UID was accepted after its replacement Generation bound")
	}

	if err := db.WithContext(ctx).Model(&persistence.AgentExecution{}).Where("id = ?", executionID).
		Update("status", "cancelled").Error; err != nil {
		t.Fatal(err)
	}
	cleanupDeadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(cleanupDeadline) {
		_ = reconciler.ReconcileOnce(ctx)
		var remaining int64
		if err := db.WithContext(ctx).Model(&persistence.ExecutionKubernetesAllocation{}).
			Where("execution_id = ? AND status <> ?", executionID, "deleted").Count(&remaining).Error; err != nil {
			t.Fatal(err)
		}
		if remaining == 0 {
			loss.Restore()
			return sandboxBackingPodLossEvidence{
				OldPodUID: oldPodUID, NewPodUID: *replacementAllocation.PodUID,
				Mode: loss.Mode, OldNode: loss.OldNode, NewNode: newNode,
				StaleCode: staleProblem.Code, RecoveryDuration: time.Since(recoveryStartedAt),
				RecoveryAuthority: "lease-expiry", RecoveredClaim: recovered,
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatal("backing Pod loss allocations did not complete exact cleanup")
	return sandboxBackingPodLossEvidence{}
}

type sandboxOperatorRestartEvidence struct {
	ExecutionIDs         []uuid.UUID
	RecoveryDuration     time.Duration
	LeaderBefore         string
	LeaderAfter          string
	FailoverDuration     time.Duration
	MaxWarmPoolDeficit   int
	FinalWarmPoolDeficit int
}

func runSandboxOperatorConcurrentRestartAcceptance(
	t *testing.T,
	ctx context.Context,
	db *gorm.DB,
	reconciler *executiontargets.KubernetesReconciler,
	workerAPI *sandboxControlPlaneWorkerAPI,
	kubernetesContext string,
	namespace string,
	domain bootstrap.Result,
	targetID uuid.UUID,
	projectID uuid.UUID,
	providerCredentialID uuid.UUID,
) sandboxOperatorRestartEvidence {
	t.Helper()
	startedAt := time.Now()
	executionIDs := make([]uuid.UUID, 0, 3)
	now := time.Now().UTC().Truncate(time.Microsecond)
	seedExecution := func(index int, inputText string) uuid.UUID {
		sessionID, turnID, executionID := uuid.New(), uuid.New(), uuid.New()
		models := []any{
			&persistence.AgentSession{
				ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
				ProjectID: projectID, CreatedBy: domain.UserID, Title: fmt.Sprintf("Sandbox restart %d", index),
				Status: "active", Visibility: "organization", Provider: "codex",
				ProviderCredentialID: &providerCredentialID, ExecutionTargetID: targetID,
				ResourceState: "provisioning", MeaningfulActivityAt: now, SuspendAfterIdleSeconds: 300,
				CreatedAt: now, UpdatedAt: now,
			},
			&persistence.AgentTurn{
				ID: turnID, TenantID: domain.TenantID, SessionID: sessionID, CreatedBy: domain.UserID,
				Status: "queued", InputText: inputText, TurnKind: "message",
				RuntimeMode: "full-access", InteractionMode: "default", CreatedAt: now,
			},
			&persistence.AgentExecution{
				ID: executionID, TenantID: domain.TenantID, SessionID: sessionID, TurnID: turnID,
				Attempt: 1, Status: "queued", ExecutionTargetID: targetID, TargetKind: "kubernetes",
				Provider: sandboxStringPointer("codex"), Generation: 0, RequestedBy: domain.UserID,
				QueuedAt: now.Add(time.Duration(index) * time.Millisecond),
			},
			&persistence.ExecutionGenerationFact{
				TenantID: domain.TenantID, ExecutionID: executionID, Generation: 1,
				SessionID: sessionID, TurnID: turnID, ExecutionTargetID: targetID, TargetKind: "kubernetes",
				Provider: "codex", RecoveryReason: "initial-claim", WarmPoolMode: "disabled",
				WarmPoolResult: "not-requested", DispatchRequestedAt: &now, CreatedAt: now, UpdatedAt: now,
			},
		}
		for _, model := range models {
			if err := db.WithContext(ctx).Create(model).Error; err != nil {
				t.Fatalf("seed concurrent restart %T: %v", model, err)
			}
		}
		executionIDs = append(executionIDs, executionID)
		return executionID
	}
	for index := 0; index < 2; index++ {
		seedExecution(index, "[credential] concurrent operator restart")
	}

	sandboxKubectl(t, ctx, kubernetesContext, nil, "-n", "sandbox-operator-system", "scale", "deployment/sandbox-operator", "--replicas=0")
	sandboxKubectl(t, ctx, kubernetesContext, nil, "-n", "sandbox-operator-system", "rollout", "status", "deployment/sandbox-operator", "--timeout=60s")
	if err := reconciler.ReconcileOnce(ctx); err != nil {
		t.Fatalf("persist Claims while sandbox-operator is stopped: %v", err)
	}
	var pending []persistence.ExecutionKubernetesAllocation
	if err := db.WithContext(ctx).Where("execution_id IN ?", executionIDs).Order("execution_id").Find(&pending).Error; err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("controller-stop allocations = %d, want 2", len(pending))
	}
	for _, allocation := range pending {
		if allocation.Status != "materializing" {
			t.Fatalf("controller-stop allocation was not durably materializing: %#v", allocation)
		}
		claimUID := strings.TrimSpace(string(sandboxKubectlOutput(
			t, ctx, kubernetesContext, nil, "-n", namespace, "get", "sandboxclaim", allocation.ClaimName,
			"-o", "jsonpath={.metadata.uid}",
		)))
		if claimUID == "" {
			t.Fatalf("controller-stop Claim %s has no Kubernetes UID", allocation.ClaimName)
		}
		ready := strings.TrimSpace(string(sandboxKubectlOutput(
			t, ctx, kubernetesContext, nil, "-n", namespace, "get", "sandboxclaim", allocation.ClaimName,
			"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}",
		)))
		if ready == "True" {
			t.Fatalf("Claim %s became Ready while controller was stopped", allocation.ClaimName)
		}
	}

	sandboxKubectl(t, ctx, kubernetesContext, nil, "-n", "sandbox-operator-system", "scale", "deployment/sandbox-operator", "--replicas=2")
	sandboxKubectl(t, ctx, kubernetesContext, nil, "-n", "sandbox-operator-system", "rollout", "status", "deployment/sandbox-operator", "--timeout=120s")
	leaderBefore := waitForSandboxOperatorActiveLeader(t, ctx, kubernetesContext, "", 60*time.Second)
	leaderPod := strings.SplitN(leaderBefore, "_", 2)[0]
	failoverStartedAt := time.Now()
	sandboxKubectl(t, ctx, kubernetesContext, nil, "-n", "sandbox-operator-system", "delete", "pod", leaderPod, "--wait=false")
	seedExecution(2, "[credential] operator leader failover")
	if err := reconciler.ReconcileOnce(ctx); err != nil {
		var apiError *problem.Error
		if !errors.As(err, &apiError) || apiError.Status < 500 {
			t.Fatalf("persist Claim during operator leader failover: %v", err)
		}
	}
	leaderAfter := waitForSandboxOperatorActiveLeader(t, ctx, kubernetesContext, leaderBefore, 60*time.Second)
	failoverDuration := time.Since(failoverStartedAt)
	completed := make(map[uuid.UUID]struct{}, len(executionIDs))
	maxWarmPoolDeficit := 0
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		if deficit := sandboxWarmPoolDeficit(t, ctx, kubernetesContext, namespace, "synara-worker-interactive"); deficit > maxWarmPoolDeficit {
			maxWarmPoolDeficit = deficit
		}
		if err := reconciler.ReconcileOnce(ctx); err != nil {
			var apiError *problem.Error
			if !errors.As(err, &apiError) || apiError.Status < 500 {
				var executionStates []struct {
					ID         uuid.UUID
					Status     string
					Generation int64
				}
				_ = db.WithContext(ctx).Model(&persistence.AgentExecution{}).
					Select("id, status, generation").Where("id IN ?", executionIDs).Order("id").Scan(&executionStates).Error
				var allocationStates []struct {
					ExecutionID       uuid.UUID
					Generation        int64
					Status            string
					FailureReasonCode string
					ClaimName         string
					PodName           *string
				}
				_ = db.WithContext(ctx).Model(&persistence.ExecutionKubernetesAllocation{}).
					Select("execution_id, generation, status, COALESCE(failure_reason_code, '') AS failure_reason_code, claim_name, pod_name").
					Where("execution_id IN ?", executionIDs).Order("execution_id, generation").Scan(&allocationStates).Error
				podCommand := exec.CommandContext(
					ctx, "kubectl", "--context", kubernetesContext, "-n", namespace,
					"get", "pods", "-l", "synara.io/execution-target-id="+targetID.String(), "-o", "wide",
				)
				podOutput, _ := podCommand.CombinedOutput()
				logCommand := exec.CommandContext(
					ctx, "kubectl", "--context", kubernetesContext, "-n", namespace,
					"logs", "-l", "synara.io/execution-target-id="+targetID.String(), "--all-containers=true", "--prefix", "--tail=120",
				)
				logOutput, _ := logCommand.CombinedOutput()
				var targetState struct {
					ID     uuid.UUID
					Kind   string
					Status string
				}
				_ = db.WithContext(ctx).Model(&persistence.ExecutionTarget{}).
					Select("id, kind, status").Where("id = ?", targetID).Take(&targetState).Error
				t.Fatalf(
					"reconcile concurrent restart allocations: %v; target=%#v executions=%#v allocations=%#v completed=%v registrationErrors=%v\npods:\n%s\nlogs:\n%s",
					err, targetState, executionStates, allocationStates, completed,
					workerAPI.registrationErrorEvidence(), podOutput, logOutput,
				)
			}
		}
		for {
			select {
			case completedID := <-workerAPI.completed:
				completed[completedID] = struct{}{}
			default:
				goto completionsDrained
			}
		}
	completionsDrained:
		if len(completed) == len(executionIDs) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	for _, executionID := range executionIDs {
		if _, ok := completed[executionID]; !ok {
			t.Fatalf("Execution %s did not complete after operator restart; completed=%v", executionID, completed)
		}
	}
	var allocations []persistence.ExecutionKubernetesAllocation
	if err := db.WithContext(ctx).Where("execution_id IN ?", executionIDs).Order("execution_id").Find(&allocations).Error; err != nil {
		t.Fatal(err)
	}
	claimUIDs, podUIDs := map[string]struct{}{}, map[string]struct{}{}
	for _, allocation := range allocations {
		if allocation.BoundAt == nil || allocation.ClaimUID == nil || allocation.PodUID == nil {
			t.Fatalf("restart allocation never reached an exact bound identity: %#v", allocation)
		}
		claimUIDs[*allocation.ClaimUID] = struct{}{}
		podUIDs[*allocation.PodUID] = struct{}{}
	}
	if len(allocations) != len(executionIDs) || len(claimUIDs) != len(executionIDs) || len(podUIDs) != len(executionIDs) {
		t.Fatalf("restart allocations are not unique: allocations=%d claims=%v pods=%v", len(allocations), claimUIDs, podUIDs)
	}
	cleanupDeadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(cleanupDeadline) {
		_ = reconciler.ReconcileOnce(ctx)
		var remaining int64
		if err := db.WithContext(ctx).Model(&persistence.ExecutionKubernetesAllocation{}).
			Where("execution_id IN ? AND status <> ?", executionIDs, "deleted").Count(&remaining).Error; err != nil {
			t.Fatal(err)
		}
		if remaining == 0 {
			finalWarmPoolDeficit := sandboxWarmPoolDeficit(t, ctx, kubernetesContext, namespace, "synara-worker-interactive")
			if finalWarmPoolDeficit > maxWarmPoolDeficit {
				maxWarmPoolDeficit = finalWarmPoolDeficit
			}
			if finalWarmPoolDeficit != 0 {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			return sandboxOperatorRestartEvidence{
				ExecutionIDs: executionIDs, RecoveryDuration: time.Since(startedAt),
				LeaderBefore: leaderBefore, LeaderAfter: leaderAfter, FailoverDuration: failoverDuration,
				MaxWarmPoolDeficit: maxWarmPoolDeficit, FinalWarmPoolDeficit: finalWarmPoolDeficit,
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("concurrent restart allocations did not clean up")
	return sandboxOperatorRestartEvidence{}
}

type sandboxControlPlaneWorkerAPI struct {
	targets                 *executiontargets.Service
	executions              *Service
	targetID                uuid.UUID
	targetKind              string
	registered              chan RegisteredWorker
	claimed                 chan ClaimResult
	completed               chan uuid.UUID
	startBlocked            chan int64
	startRelease            chan struct{}
	startReleaseOnce        sync.Once
	blockedStartExecutionID uuid.UUID
	mu                      sync.Mutex
	bearerToken             string
	providerHost            bool
	workerTokens            map[string]uuid.UUID
	credentialResolved      bool
	crossTenantRejected     bool
	credentialEventProof    bool
	credentialLeaked        bool
	registrationErrors      []string
}

func newSandboxControlPlaneWorkerAPI(targets *executiontargets.Service, executions *Service) *sandboxControlPlaneWorkerAPI {
	return &sandboxControlPlaneWorkerAPI{
		targets: targets, executions: executions, registered: make(chan RegisteredWorker, 8),
		claimed: make(chan ClaimResult, 8), completed: make(chan uuid.UUID, 8),
		startBlocked: make(chan int64, 1), startRelease: make(chan struct{}),
		workerTokens: make(map[string]uuid.UUID),
	}
}

func (a *sandboxControlPlaneWorkerAPI) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	switch request.URL.Path {
	case "/v1/workers/register":
		var input RegisterWorkerInput
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			a.writeError(writer, problem.New(400, "invalid_registration", err.Error()))
			return
		}
		bearer := strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "))
		verified, err := a.targets.VerifyKubernetesWorkerRegistration(request.Context(), input.ExecutionTargetID, input.Namespace, input.PodName, input.InstanceUID, bearer)
		if err != nil {
			var targetProbe struct {
				ID     uuid.UUID
				Kind   string
				Status string
			}
			probeErr := a.executions.db.WithContext(request.Context()).Model(&persistence.ExecutionTarget{}).
				Select("id, kind, status").Where("id = ?", input.ExecutionTargetID).Take(&targetProbe).Error
			a.mu.Lock()
			a.registrationErrors = append(a.registrationErrors, fmt.Sprintf(
				"target=%s namespace=%s pod=%s uid=%s error=%v probe=%s/%s/%s probeError=%v",
				input.ExecutionTargetID, input.Namespace, input.PodName, input.InstanceUID, err,
				targetProbe.ID, targetProbe.Kind, targetProbe.Status, probeErr,
			))
			a.mu.Unlock()
			a.writeError(writer, err)
			return
		}
		input.WorkerMode, input.AssignedExecutionID = verified.WorkerMode, verified.AssignedExecutionID
		input.ClusterID, input.RegistrationTrustMode = verified.ClusterID, WorkerRegistrationTrustKubernetesPodBoundV1
		input.RequestedCPUMillicores, input.RequestedMemoryBytes, input.RequestedEphemeralStorageBytes = verified.RequestedCPUMillicores, verified.RequestedMemoryBytes, verified.RequestedEphemeralStorageBytes
		_, providerHostObserved := input.Capabilities["providerHost"]
		input.Capabilities = workerManifestTestCapabilitiesForVersion(input.Version)
		addWorkerManifestTestContainmentEvidence(input.Capabilities)
		if err := signWorkerManifestTestContainmentValue(input.Capabilities, workerManifestRegistrationContext{ExecutionTargetID: input.ExecutionTargetID, TargetKind: platform.TargetKubernetes, InstanceUID: input.InstanceUID, ClusterID: input.ClusterID, Namespace: input.Namespace, PodName: input.PodName}); err != nil {
			a.writeError(writer, err)
			return
		}
		registered, err := a.executions.Register(request.Context(), input)
		if err != nil {
			a.writeError(writer, err)
			return
		}
		a.mu.Lock()
		a.bearerToken = bearer
		a.providerHost = providerHostObserved
		a.workerTokens[registered.Token] = registered.Worker.ID
		a.mu.Unlock()
		select {
		case a.registered <- registered:
		default:
		}
		a.writeJSON(writer, http.StatusCreated, registered)
	case "/v1/workers/executions/claim":
		var input ClaimExecutionInput
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			a.writeError(writer, problem.New(400, "invalid_claim", err.Error()))
			return
		}
		worker, err := a.registeredWorker(request)
		if err != nil {
			a.writeError(writer, err)
			return
		}
		result, err := a.executions.Claim(request.Context(), worker, input, request.Header.Get("X-Request-ID"))
		if err != nil {
			a.writeError(writer, err)
			return
		}
		if result.Value.Execution != nil {
			select {
			case a.claimed <- result.Value:
			default:
			}
		}
		a.writeJSON(writer, result.StatusCode, result.Value)
	default:
		a.serveExecutionLifecycle(writer, request)
	}
}

func (a *sandboxControlPlaneWorkerAPI) serveExecutionLifecycle(writer http.ResponseWriter, request *http.Request) {
	const prefix = "/v1/workers/executions/"
	path := strings.TrimPrefix(request.URL.Path, prefix)
	parts := strings.Split(path, "/")
	if path == request.URL.Path || len(parts) < 2 {
		a.writeJSON(writer, http.StatusOK, map[string]any{})
		return
	}
	executionID, err := uuid.Parse(parts[0])
	if err != nil {
		a.writeError(writer, problem.New(400, "invalid_execution", err.Error()))
		return
	}
	worker, err := a.registeredWorker(request)
	if err != nil {
		a.writeError(writer, err)
		return
	}
	requestID := request.Header.Get("X-Request-ID")
	action := strings.Join(parts[1:], "/")
	switch action {
	case "start":
		var input LeaseInput
		if !a.decode(writer, request, &input) {
			return
		}
		if executionID == a.blockedStartExecutionID && input.Generation == 1 {
			select {
			case a.startBlocked <- input.Generation:
			default:
			}
			select {
			case <-a.startRelease:
			case <-request.Context().Done():
			}
			return
		}
		result, callErr := a.executions.Start(request.Context(), worker, executionID, input, requestID)
		if callErr != nil {
			a.writeError(writer, callErr)
			return
		}
		a.writeJSON(writer, result.StatusCode, result.Value)
	case "events":
		var input RuntimeEventInput
		if !a.decode(writer, request, &input) {
			return
		}
		result, callErr := a.executions.AppendRuntimeEvent(request.Context(), worker, executionID, input, requestID)
		if callErr != nil {
			a.writeError(writer, callErr)
			return
		}
		encoded, _ := json.Marshal(input.Payload)
		a.mu.Lock()
		if bytes.Contains(encoded, []byte(sandboxProviderCredentialSentinel)) {
			a.credentialLeaked = true
		}
		if bytes.Contains(encoded, []byte(`"credentialVerified":true`)) {
			a.credentialEventProof = true
		}
		a.mu.Unlock()
		a.writeJSON(writer, result.StatusCode, result.Value)
	case "complete":
		var input CompleteExecutionInput
		if !a.decode(writer, request, &input) {
			return
		}
		encoded, _ := json.Marshal(input)
		a.mu.Lock()
		if bytes.Contains(encoded, []byte(sandboxProviderCredentialSentinel)) {
			a.credentialLeaked = true
		}
		if bytes.Contains(encoded, []byte(`"credentialVerified":true`)) {
			a.credentialEventProof = true
		}
		a.mu.Unlock()
		result, callErr := a.executions.Complete(request.Context(), worker, executionID, input, requestID)
		if callErr == nil {
			select {
			case a.completed <- executionID:
			default:
			}
		}
		if callErr != nil {
			a.writeError(writer, callErr)
			return
		}
		a.writeJSON(writer, result.StatusCode, result.Value)
	case "control-updates/pull":
		a.writeJSON(writer, http.StatusOK, ControlUpdates{})
	case "resource-directives/pull":
		a.writeJSON(writer, http.StatusOK, map[string]any{"directive": nil})
	default:
		if len(parts) == 4 && parts[1] == "provider-credential-grants" && parts[3] == "resolve" {
			a.resolveProviderCredentialGrant(writer, request, worker, executionID, parts[2])
			return
		}
		a.writeJSON(writer, http.StatusOK, map[string]any{})
	}
}

const sandboxProviderCredentialSentinel = "stage3-provider-acceptance-credential-v1"

func (a *sandboxControlPlaneWorkerAPI) resolveProviderCredentialGrant(
	writer http.ResponseWriter,
	request *http.Request,
	worker persistence.WorkerInstance,
	executionID uuid.UUID,
	rawGrantID string,
) {
	grantID, err := uuid.Parse(rawGrantID)
	if err != nil {
		a.writeError(writer, problem.New(400, "invalid_provider_credential_grant", err.Error()))
		return
	}
	var input LeaseInput
	if !a.decode(writer, request, &input) {
		return
	}
	wrongTenant := input
	wrongTenant.TenantID = uuid.New()
	var crossTenantErr error
	_ = persistence.InTransaction(request.Context(), a.executions.db, func(tx *gorm.DB) error {
		_, crossTenantErr = a.executions.ResolveProviderCredentialGrantAccess(
			request.Context(), tx, worker, executionID, grantID, wrongTenant,
		)
		return nil
	})
	if crossTenantErr == nil {
		a.writeError(writer, problem.New(500, "cross_tenant_credential_access_accepted", "Cross-tenant Provider Credential access was accepted"))
		return
	}
	var crossTenantProblem *problem.Error
	if !errors.As(crossTenantErr, &crossTenantProblem) || crossTenantProblem.Code != "lease_not_current" {
		a.writeError(writer, problem.New(500, "cross_tenant_credential_fence_unexpected", "Cross-tenant Provider Credential access did not fail at the lease tenant fence"))
		return
	}
	var resolution ProviderCredentialGrantAccessResolution
	err = persistence.InTransaction(request.Context(), a.executions.db, func(tx *gorm.DB) error {
		var resolveErr error
		resolution, resolveErr = a.executions.ResolveProviderCredentialGrantAccess(
			request.Context(), tx, worker, executionID, grantID, input,
		)
		return resolveErr
	})
	if err != nil {
		a.writeError(writer, err)
		return
	}
	a.mu.Lock()
	a.credentialResolved = true
	a.crossTenantRejected = true
	a.mu.Unlock()
	writer.Header().Set("Cache-Control", "no-store")
	a.writeJSON(writer, http.StatusOK, map[string]any{
		"grantId": resolution.Grant.ID,
		"payload": map[string]any{"apiKey": sandboxProviderCredentialSentinel},
		"access":  resolution.Access,
	})
}

func (a *sandboxControlPlaneWorkerAPI) registeredWorker(request *http.Request) (persistence.WorkerInstance, error) {
	token := strings.TrimSpace(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "))
	a.mu.Lock()
	workerID := a.workerTokens[token]
	a.mu.Unlock()
	if workerID == uuid.Nil {
		return persistence.WorkerInstance{}, problem.New(401, "worker_unauthorized", "Worker has not registered")
	}
	var worker persistence.WorkerInstance
	if err := a.executions.db.WithContext(request.Context()).Where("id = ?", workerID).Take(&worker).Error; err != nil {
		return persistence.WorkerInstance{}, err
	}
	return worker, nil
}

func (a *sandboxControlPlaneWorkerAPI) decode(writer http.ResponseWriter, request *http.Request, output any) bool {
	if err := json.NewDecoder(request.Body).Decode(output); err != nil {
		a.writeError(writer, problem.New(400, "invalid_request", err.Error()))
		return false
	}
	return true
}

func (a *sandboxControlPlaneWorkerAPI) registrationBearer() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.bearerToken
}
func (a *sandboxControlPlaneWorkerAPI) observedProviderHost() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.providerHost
}
func (a *sandboxControlPlaneWorkerAPI) credentialEvidence() (bool, bool, bool, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.credentialResolved, a.crossTenantRejected, a.credentialEventProof, a.credentialLeaked
}
func (a *sandboxControlPlaneWorkerAPI) registrationErrorEvidence() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.registrationErrors...)
}
func (a *sandboxControlPlaneWorkerAPI) releaseBlockedStart() {
	a.startReleaseOnce.Do(func() { close(a.startRelease) })
}
func (a *sandboxControlPlaneWorkerAPI) writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
func (a *sandboxControlPlaneWorkerAPI) writeError(writer http.ResponseWriter, err error) {
	status := 500
	code := "internal_error"
	if apiErr := new(problem.Error); errors.As(err, &apiErr) {
		status, code = apiErr.Status, apiErr.Code
	}
	a.writeJSON(writer, status, map[string]any{"error": map[string]any{"status": status, "code": code, "message": err.Error()}})
}

func applySandboxControlPlaneTemplate(t *testing.T, ctx context.Context, kubernetesContext, namespace string, targetID, executionID uuid.UUID, serviceAccount, image, controlPlaneURL, templateRuntime string) {
	t.Helper()
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata: {name: %s}
---
apiVersion: v1
kind: ServiceAccount
metadata: {name: %s, namespace: %s}
---
apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxTemplate
metadata: {name: synara-worker, namespace: %s}
spec:
  networkPolicyManagement: Unmanaged
  envVarsInjectionPolicy: Disallowed
  podTemplate:
    metadata:
      annotations: {sandbox.cocoonstack.io/runtime: %s}
      labels: {synara.io/managed: "true", synara.io/execution-target-id: "%s"}
    spec:
      serviceAccountName: %s
      automountServiceAccountToken: false
      restartPolicy: Never
      containers:
      - name: agentd
        image: %s
        imagePullPolicy: IfNotPresent
        command: ["/usr/local/bin/synara-agentd"]
        env:
        - {name: SYNARA_CONTROL_PLANE_URL, value: "%s"}
        - {name: SYNARA_WORKER_REGISTRATION_TOKEN_FILE, value: /var/run/secrets/synara.io/workload-identity/token}
        - {name: SYNARA_EXECUTION_TARGET_ID, value: "%s"}
        - {name: SYNARA_EXECUTION_TARGET_KIND, value: kubernetes}
        - {name: SYNARA_AGENTD_WORKER_MODE, value: execution-pinned}
        - {name: SYNARA_AGENTD_ASSIGNED_EXECUTION_ID_FILE, value: /var/run/secrets/synara.io/assignment/execution-id}
        - {name: SYNARA_AGENTD_SANDBOX_ALLOCATION_BIND_TIMEOUT, value: 180s}
        - {name: SYNARA_AGENTD_CLUSTER_ID, value: local}
        - {name: SYNARA_AGENTD_NAMESPACE, value: "%s"}
        - name: SYNARA_AGENTD_INSTANCE_ID
          valueFrom: {fieldRef: {fieldPath: metadata.name}}
        - name: SYNARA_AGENTD_INSTANCE_UID
          valueFrom: {fieldRef: {fieldPath: metadata.uid}}
        - {name: SYNARA_AGENTD_CAPABILITIES_JSON, value: '{"providerPolicy":{"experimentalProviders":["codex"]}}'}
        - {name: SYNARA_AGENTD_RUNNER_COMMAND_JSON, value: '["node","/opt/synara/provider-host/fixture.mjs"]'}
        - {name: SYNARA_AGENTD_PROVIDER_HOST_PROTOCOL, value: v2}
        - {name: SYNARA_AGENTD_WORKSPACE_ROOT, value: /data/workspaces}
        - {name: SYNARA_AGENTD_GIT_CACHE_ROOT, value: /data/git-cache}
        resources: {requests: {cpu: 100m, memory: 256Mi}, limits: {cpu: "2", memory: 2Gi}}
        securityContext: {allowPrivilegeEscalation: false, readOnlyRootFilesystem: true, runAsNonRoot: true, runAsUser: 10001, runAsGroup: 10001, capabilities: {drop: [ALL]}}
        volumeMounts:
        - {name: workspace, mountPath: /data}
        - {name: tmp, mountPath: /tmp}
        - {name: home, mountPath: /home/synara}
        - {name: workload-identity, mountPath: /var/run/secrets/synara.io/workload-identity, readOnly: true}
        - {name: execution-assignment, mountPath: /var/run/secrets/synara.io/assignment, readOnly: true}
      volumes:
      - {name: workspace, emptyDir: {}}
      - {name: tmp, emptyDir: {}}
      - {name: home, emptyDir: {}}
      - name: workload-identity
        projected: {defaultMode: 288, sources: [{serviceAccountToken: {audience: "synara.execution-target.%s", expirationSeconds: 600, path: token}}]}
      - name: execution-assignment
        downwardAPI: {items: [{path: execution-id, fieldRef: {fieldPath: "metadata.labels['synara.io/assigned-execution-id']"}}]}
---
apiVersion: extensions.agents.x-k8s.io/v1beta1
kind: SandboxWarmPool
metadata: {name: synara-worker-interactive, namespace: %s}
spec: {replicas: 0, sandboxTemplateRef: {name: synara-worker}, updateStrategy: {type: Recreate}}
`, namespace, serviceAccount, namespace, namespace, templateRuntime, targetID, serviceAccount, image, controlPlaneURL, targetID, namespace, targetID, namespace)
	sandboxKubectl(t, ctx, kubernetesContext, []byte(manifest), "apply", "-f", "-")
	_ = executionID
}

func sandboxKubectl(t *testing.T, ctx context.Context, kubernetesContext string, input []byte, args ...string) {
	t.Helper()
	commandArgs := append([]string{"--context", kubernetesContext}, args...)
	command := exec.CommandContext(ctx, "kubectl", commandArgs...)
	command.Stdin = bytes.NewReader(input)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

type sandboxPodLossInjection struct {
	Mode    string
	OldNode string
	Restore func()
}

func injectSandboxBackingPodLoss(
	t *testing.T,
	ctx context.Context,
	kubernetesContext string,
	namespace string,
	podName string,
) sandboxPodLossInjection {
	t.Helper()
	oldNode := strings.TrimSpace(string(sandboxKubectlOutput(
		t, ctx, kubernetesContext, nil, "-n", namespace, "get", "pod", podName,
		"-o", "jsonpath={.spec.nodeName}",
	)))
	if oldNode == "" {
		t.Fatal("the backing Pod has no scheduled node before loss injection")
	}
	hook := strings.TrimSpace(os.Getenv("SYNARA_TEST_KUBERNETES_NODE_LOSS_HOOK"))
	if hook == "" {
		sandboxKubectl(
			t, ctx, kubernetesContext, nil, "-n", namespace, "delete", "pod", podName,
			"--wait=true", "--timeout=60s",
		)
		return sandboxPodLossInjection{Mode: "pod-delete", OldNode: oldNode, Restore: func() {}}
	}
	if !filepath.IsAbs(hook) {
		t.Fatal("SYNARA_TEST_KUBERNETES_NODE_LOSS_HOOK must be an absolute executable path")
	}
	info, err := os.Stat(hook)
	if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("SYNARA_TEST_KUBERNETES_NODE_LOSS_HOOK is not executable: %v", err)
	}
	readyNodes := sandboxReadyKVMNodeNames(t, ctx, kubernetesContext)
	if len(readyNodes) < 2 || !slices.Contains(readyNodes, oldNode) {
		t.Fatalf("real node-loss injection requires at least two Ready KVM virtual nodes including %q; ready=%v", oldNode, readyNodes)
	}
	if err := exec.CommandContext(ctx, hook, "lose", kubernetesContext, oldNode).Run(); err != nil {
		t.Fatalf("node-loss hook failed for %s: %v", oldNode, err)
	}
	notReadyDeadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(notReadyDeadline) && sandboxNodeReady(ctx, kubernetesContext, oldNode) {
		time.Sleep(time.Second)
	}
	if sandboxNodeReady(ctx, kubernetesContext, oldNode) {
		t.Fatalf("node-loss hook returned but node %s remained Ready", oldNode)
	}
	sandboxKubectl(
		t, ctx, kubernetesContext, nil, "-n", namespace, "delete", "pod", podName,
		"--force", "--grace-period=0", "--ignore-not-found", "--wait=true", "--timeout=60s",
	)
	var restoreOnce sync.Once
	restore := func() {
		restoreOnce.Do(func() {
			restoreCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := exec.CommandContext(restoreCtx, hook, "restore", kubernetesContext, oldNode).Run(); err != nil {
				t.Errorf("restore node %s with Stage C hook: %v", oldNode, err)
				return
			}
			deadline := time.Now().Add(5 * time.Minute)
			for time.Now().Before(deadline) && !sandboxNodeReady(restoreCtx, kubernetesContext, oldNode) {
				time.Sleep(time.Second)
			}
			if !sandboxNodeReady(restoreCtx, kubernetesContext, oldNode) {
				t.Errorf("restored node %s did not return Ready", oldNode)
			}
		})
	}
	t.Cleanup(restore)
	return sandboxPodLossInjection{Mode: "node-hook", OldNode: oldNode, Restore: restore}
}

func sandboxReadyKVMNodeNames(
	t *testing.T,
	ctx context.Context,
	kubernetesContext string,
) []string {
	t.Helper()
	payload := sandboxKubectlOutput(
		t, ctx, kubernetesContext, nil, "get", "nodes",
		"-l", "node.kubernetes.io/instance-type=virtual-node,sandbox.cocoonstack.io/kvm-ready=true,synara.io/host-supervisor=v1,synara.io/provider-transport=vsock-v2,synara.io/isolation-profile=microvm-isolated-v1",
		"-o", "json",
	)
	var nodes struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(payload, &nodes); err != nil {
		t.Fatalf("decode KVM virtual nodes: %v", err)
	}
	ready := make([]string, 0, len(nodes.Items))
	for _, node := range nodes.Items {
		for _, condition := range node.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				ready = append(ready, node.Metadata.Name)
				break
			}
		}
	}
	slices.Sort(ready)
	return ready
}

func sandboxNodeReady(ctx context.Context, kubernetesContext, nodeName string) bool {
	command := exec.CommandContext(
		ctx, "kubectl", "--context", kubernetesContext, "get", "node", nodeName,
		"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}",
	)
	output, err := command.Output()
	return err == nil && strings.TrimSpace(string(output)) == "True"
}

func sandboxOperatorLeaderIdentity(t *testing.T, ctx context.Context, kubernetesContext string) string {
	t.Helper()
	return strings.TrimSpace(string(sandboxKubectlOutput(
		t, ctx, kubernetesContext, nil,
		"-n", "sandbox-operator-system", "get", "lease", "sandbox-operator.agents.x-k8s.io",
		"-o", "jsonpath={.spec.holderIdentity}",
	)))
}

func waitForSandboxOperatorActiveLeader(
	t *testing.T,
	ctx context.Context,
	kubernetesContext string,
	previousIdentity string,
	timeout time.Duration,
) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	lastIdentity := ""
	for time.Now().Before(deadline) {
		lastIdentity = sandboxOperatorLeaderIdentity(t, ctx, kubernetesContext)
		podNames := strings.Fields(string(sandboxKubectlOutput(
			t, ctx, kubernetesContext, nil,
			"-n", "sandbox-operator-system", "get", "pods", "-l", "app.kubernetes.io/name=sandbox-operator",
			"-o", "jsonpath={range .items[*]}{.metadata.name}{' '}{end}",
		)))
		leaderPod := strings.SplitN(lastIdentity, "_", 2)[0]
		if lastIdentity != "" && lastIdentity != previousIdentity && slices.Contains(podNames, leaderPod) {
			return lastIdentity
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("sandbox-operator did not expose an active leader: previous=%q last=%q", previousIdentity, lastIdentity)
	return ""
}

func sandboxWarmPoolDeficit(
	t *testing.T,
	ctx context.Context,
	kubernetesContext string,
	namespace string,
	poolName string,
) int {
	t.Helper()
	desired, ready := sandboxWarmPoolCapacity(t, ctx, kubernetesContext, namespace, poolName)
	if ready >= desired {
		return 0
	}
	return desired - ready
}

func sandboxWarmPoolCapacity(
	t *testing.T,
	ctx context.Context,
	kubernetesContext string,
	namespace string,
	poolName string,
) (int, int) {
	t.Helper()
	payload := sandboxKubectlOutput(
		t, ctx, kubernetesContext, nil,
		"-n", namespace, "get", "sandboxwarmpool", poolName,
		"-o", "json",
	)
	var pool struct {
		Spec struct {
			Replicas int `json:"replicas"`
		} `json:"spec"`
		Status struct {
			ReadyReplicas int `json:"readyReplicas"`
		} `json:"status"`
	}
	if err := json.Unmarshal(payload, &pool); err != nil {
		t.Fatalf("decode SandboxWarmPool capacity observation: %v", err)
	}
	return pool.Spec.Replicas, pool.Status.ReadyReplicas
}

func assertSandboxControlPlaneIsolationAndQuota(
	t *testing.T,
	ctx context.Context,
	kubernetesContext string,
	namespace string,
	targetID uuid.UUID,
	podName string,
) {
	t.Helper()
	podJSON := sandboxKubectlOutput(t, ctx, kubernetesContext, nil, "-n", namespace, "get", "pod", podName, "-o", "json")
	var pod struct {
		Spec struct {
			AutomountServiceAccountToken *bool `json:"automountServiceAccountToken"`
			Containers                   []struct {
				Name         string                         `json:"name"`
				Env          []struct{ Name, Value string } `json:"env"`
				VolumeMounts []struct {
					Name      string `json:"name"`
					MountPath string `json:"mountPath"`
					ReadOnly  bool   `json:"readOnly"`
				} `json:"volumeMounts"`
			} `json:"containers"`
			Volumes []struct {
				Name      string `json:"name"`
				Projected *struct {
					Sources []struct {
						ServiceAccountToken *struct {
							Audience string `json:"audience"`
							Path     string `json:"path"`
						} `json:"serviceAccountToken"`
					} `json:"sources"`
				} `json:"projected"`
			} `json:"volumes"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(podJSON, &pod); err != nil {
		t.Fatal(err)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatalf("Sandbox Pod default ServiceAccount token automount is not disabled: %s", podJSON)
	}
	expectedAudience := "synara.execution-target." + targetID.String()
	projectedIdentity := false
	for _, volume := range pod.Spec.Volumes {
		if strings.HasPrefix(volume.Name, "kube-api-access-") {
			t.Fatalf("Sandbox Pod contains an implicit Kubernetes API token volume %q", volume.Name)
		}
		if volume.Name != "workload-identity" || volume.Projected == nil {
			continue
		}
		for _, source := range volume.Projected.Sources {
			if source.ServiceAccountToken != nil && source.ServiceAccountToken.Audience == expectedAudience && source.ServiceAccountToken.Path == "token" {
				projectedIdentity = true
			}
		}
	}
	if !projectedIdentity {
		t.Fatalf("Sandbox Pod omitted the target-audience projected workload identity")
	}
	identityMount := false
	for _, container := range pod.Spec.Containers {
		for _, environment := range container.Env {
			if strings.Contains(strings.ToLower(environment.Name), "token") &&
				!strings.HasSuffix(environment.Name, "_FILE") && strings.TrimSpace(environment.Value) != "" {
				t.Fatalf("Sandbox Pod exposes a token through environment %s", environment.Name)
			}
		}
		for _, mount := range container.VolumeMounts {
			if mount.Name == "workload-identity" && mount.MountPath == "/var/run/secrets/synara.io/workload-identity" && mount.ReadOnly {
				identityMount = true
			}
		}
	}
	if !identityMount {
		t.Fatal("Sandbox Pod workload identity is not mounted read-only at the dedicated path")
	}

	resourceName := "synara-agentd-" + strings.ReplaceAll(targetID.String(), "-", "")[:12]
	quotaJSON := sandboxKubectlOutput(t, ctx, kubernetesContext, nil, "-n", namespace, "get", "resourcequota", resourceName, "-o", "json")
	var quota struct {
		Spec struct {
			Hard map[string]string `json:"hard"`
		} `json:"spec"`
		Status struct {
			Used map[string]string `json:"used"`
		} `json:"status"`
	}
	if err := json.Unmarshal(quotaJSON, &quota); err != nil {
		t.Fatal(err)
	}
	if quota.Spec.Hard["pods"] != "4" {
		t.Fatalf("Sandbox namespace pod quota = %q, want 4", quota.Spec.Hard["pods"])
	}
	policyJSON := sandboxKubectlOutput(t, ctx, kubernetesContext, nil, "-n", namespace, "get", "networkpolicy", resourceName, "-o", "json")
	var policy struct {
		Spec struct {
			PodSelector struct {
				MatchLabels map[string]string `json:"matchLabels"`
			} `json:"podSelector"`
			PolicyTypes []string `json:"policyTypes"`
			Ingress     []any    `json:"ingress"`
			Egress      []any    `json:"egress"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(policyJSON, &policy); err != nil {
		t.Fatal(err)
	}
	if policy.Spec.PodSelector.MatchLabels["synara.io/execution-target-id"] != targetID.String() ||
		len(policy.Spec.Ingress) != 0 || len(policy.Spec.Egress) < 2 ||
		!slices.Contains(policy.Spec.PolicyTypes, "Ingress") || !slices.Contains(policy.Spec.PolicyTypes, "Egress") {
		t.Fatalf("Sandbox NetworkPolicy is incomplete: %s", policyJSON)
	}
	assertSandboxNetworkPolicyEnforced(t, ctx, kubernetesContext, namespace, targetID)
	quotaJSON = sandboxKubectlOutput(t, ctx, kubernetesContext, nil, "-n", namespace, "get", "resourcequota", resourceName, "-o", "json")
	if err := json.Unmarshal(quotaJSON, &quota); err != nil {
		t.Fatal(err)
	}

	probeManifest := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata: {name: quota-holder, namespace: %s}
spec:
  restartPolicy: Never
  containers:
  - name: holder
    image: busybox:1.36.1
    command: ["sh", "-c", "sleep 300"]
    resources:
      requests: {cpu: 10m, memory: 16Mi, ephemeral-storage: 1Mi}
      limits: {cpu: 20m, memory: 32Mi}
`, namespace)
	usedPods, err := strconv.Atoi(quota.Status.Used["pods"])
	if err != nil || usedPods < 0 || usedPods > 4 {
		t.Fatalf("Sandbox namespace used.pods = %q", quota.Status.Used["pods"])
	}
	holderNames := make([]string, 0, 4-usedPods)
	for index := usedPods; index < 4; index++ {
		holderName := fmt.Sprintf("quota-holder-%d", index)
		holderNames = append(holderNames, holderName)
		holderManifest := strings.Replace(probeManifest, "quota-holder", holderName, 1)
		sandboxKubectl(t, ctx, kubernetesContext, []byte(holderManifest), "apply", "-f", "-")
	}
	overflowManifest := strings.Replace(probeManifest, "quota-holder", "quota-overflow", 1)
	command := exec.CommandContext(ctx, "kubectl", "--context", kubernetesContext, "apply", "-f", "-")
	command.Stdin = strings.NewReader(overflowManifest)
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "exceeded quota") {
		t.Fatalf("ResourceQuota did not reject a Pod above the active limit: err=%v output=%s", err, output)
	}
	if len(holderNames) > 0 {
		deleteArgs := []string{"-n", namespace, "delete", "pod"}
		deleteArgs = append(deleteArgs, holderNames...)
		deleteArgs = append(deleteArgs, "--wait=true", "--timeout=60s")
		sandboxKubectl(t, ctx, kubernetesContext, nil, deleteArgs...)
	}
}

func assertSandboxNetworkPolicyEnforced(
	t *testing.T,
	ctx context.Context,
	kubernetesContext string,
	namespace string,
	targetID uuid.UUID,
) {
	t.Helper()
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Pod
metadata:
  name: network-policy-server
  namespace: %s
  labels: {synara.io/execution-target-id: "%s"}
spec:
  restartPolicy: Never
  containers:
  - name: server
    image: busybox:1.36.1
    command: ["sh", "-c", "mkdir -p /www; echo ok > /www/index.html; exec httpd -f -p 8080 -h /www"]
    resources:
      requests: {cpu: 10m, memory: 16Mi, ephemeral-storage: 1Mi}
      limits: {cpu: 20m, memory: 32Mi}
---
apiVersion: v1
kind: Pod
metadata: {name: network-policy-client, namespace: %s}
spec:
  restartPolicy: Never
  containers:
  - name: client
    image: busybox:1.36.1
    command: ["sh", "-c", "sleep 300"]
    resources:
      requests: {cpu: 10m, memory: 16Mi, ephemeral-storage: 1Mi}
      limits: {cpu: 20m, memory: 32Mi}
`, namespace, targetID, namespace)
	sandboxKubectl(t, ctx, kubernetesContext, []byte(manifest), "apply", "-f", "-")
	sandboxKubectl(t, ctx, kubernetesContext, nil, "-n", namespace, "wait", "--for=condition=Ready", "pod/network-policy-server", "pod/network-policy-client", "--timeout=60s")
	serverIP := strings.TrimSpace(string(sandboxKubectlOutput(t, ctx, kubernetesContext, nil, "-n", namespace, "get", "pod", "network-policy-server", "-o", "jsonpath={.status.podIP}")))
	sandboxKubectl(t, ctx, kubernetesContext, nil, "-n", namespace, "exec", "network-policy-server", "--", "wget", "-q", "-T", "3", "-O-", "http://127.0.0.1:8080")
	command := exec.CommandContext(ctx, "kubectl", "--context", kubernetesContext, "-n", namespace, "exec", "network-policy-client", "--", "wget", "-q", "-T", "3", "-O-", "http://"+serverIP+":8080")
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("NetworkPolicy allowed ingress from the untrusted Pod: %s", output)
	}
	sandboxKubectl(t, ctx, kubernetesContext, nil, "-n", namespace, "delete", "pod", "network-policy-server", "network-policy-client", "--wait=true", "--timeout=60s")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		usedRaw := strings.TrimSpace(string(sandboxKubectlOutput(t, ctx, kubernetesContext, nil, "-n", namespace, "get", "resourcequota", "-o", "jsonpath={.items[0].status.used.pods}")))
		used, err := strconv.Atoi(usedRaw)
		if err == nil && used <= 1 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("ResourceQuota usage did not release NetworkPolicy probe Pods")
}

func sandboxKubectlOutput(t *testing.T, ctx context.Context, kubernetesContext string, input []byte, args ...string) []byte {
	t.Helper()
	commandArgs := append([]string{"--context", kubernetesContext}, args...)
	command := exec.CommandContext(ctx, "kubectl", commandArgs...)
	command.Stdin = bytes.NewReader(input)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return output
}

func sandboxStringPointer(value string) *string { return &value }
