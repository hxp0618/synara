package sessions

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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
	"github.com/synara-ai/synara/services/control-plane/internal/projects"
	"github.com/synara-ai/synara/services/control-plane/internal/routing"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

const (
	dualClusterIntegrationEvidencePathEnv  = "SYNARA_DUAL_CLUSTER_INTEGRATION_EVIDENCE_DETAIL_FILE"
	dualClusterIntegrationWorkerImageEnv   = "SYNARA_DUAL_CLUSTER_INTEGRATION_WORKER_IMAGE"
	dualClusterIntegrationWorkerImageIDEnv = "SYNARA_DUAL_CLUSTER_INTEGRATION_WORKER_IMAGE_ID"
	dualClusterIntegrationRunIDEnv         = "SYNARA_DUAL_CLUSTER_INTEGRATION_RUN_ID"
	dualClusterIntegrationDatabaseURLEnv   = "SYNARA_DUAL_CLUSTER_INTEGRATION_DATABASE_URL"
	dualClusterWorkerAPIHostAliasSuffix    = "_WORKER_API_HOST_ALIAS"
	dualClusterWorkerAPIRequestLimit       = 64 << 10
	dualClusterPressurePerCluster          = 8
	dualClusterChaosCycles                 = 3
)

type dualClusterIntegrationContext struct {
	APIServer       string
	Token           string
	CACertificate   string
	Namespace       string
	NamespaceUID    string
	ContextLabel    string
	AcceptanceRunID string
	WorkerAPIHost   string
}

type dualClusterKubeAPI struct {
	baseURL string
	token   string
	client  *http.Client
}

type dualClusterPod struct {
	Metadata struct {
		Name              string            `json:"name"`
		UID               string            `json:"uid"`
		Labels            map[string]string `json:"labels"`
		DeletionTimestamp *time.Time        `json:"deletionTimestamp,omitempty"`
	} `json:"metadata"`
	Status struct {
		Phase      string `json:"phase"`
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
		ContainerStatuses []struct {
			Name         string `json:"name"`
			Ready        bool   `json:"ready"`
			Started      *bool  `json:"started,omitempty"`
			RestartCount int32  `json:"restartCount"`
			ImageID      string `json:"imageID"`
			State        struct {
				Running *struct {
					StartedAt time.Time `json:"startedAt"`
				} `json:"running,omitempty"`
			} `json:"state"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

type dualClusterPodList struct {
	Items []dualClusterPod `json:"items"`
}

type dualClusterWorkerObservation struct {
	TargetID                 string
	ExecutionID              string
	PodName                  string
	PodUID                   string
	PodBoundIdentityVerified bool
	Heartbeats               int
	Claims                   int
}

type dualClusterWorkerAPIStub struct {
	registrationVerifier *executiontargets.Service
	mutex                sync.RWMutex
	observations         map[string]dualClusterWorkerObservation
	tokenToKey           map[string]string
}

func newDualClusterWorkerAPIStub(
	t *testing.T,
	registrationVerifier *executiontargets.Service,
	primaryHostAlias string,
	secondaryHostAlias string,
) (*dualClusterWorkerAPIStub, string, string) {
	t.Helper()
	if registrationVerifier == nil {
		t.Fatal("production Kubernetes Worker registration verifier is required")
	}
	listener, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatalf("listen for bounded Worker API acceptance stub: %v", err)
	}
	stub := &dualClusterWorkerAPIStub{
		registrationVerifier: registrationVerifier,
		observations:         make(map[string]dualClusterWorkerObservation),
		tokenToKey:           make(map[string]string),
	}
	server := &http.Server{
		Handler:           http.HandlerFunc(stub.serveHTTP),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(listener)
	}()
	t.Cleanup(func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownContext)
		if serveErr := <-serveDone; serveErr != nil && serveErr != http.ErrServerClosed {
			t.Errorf("bounded Worker API acceptance stub stopped unexpectedly: %v", serveErr)
		}
	})
	port := listener.Addr().(*net.TCPAddr).Port
	return stub,
		dualClusterWorkerAPIURL(t, primaryHostAlias, port),
		dualClusterWorkerAPIURL(t, secondaryHostAlias, port)
}

func dualClusterWorkerAPIURL(t *testing.T, hostAlias string, port int) string {
	t.Helper()
	hostAlias = strings.TrimSpace(hostAlias)
	if hostAlias == "" || strings.Contains(hostAlias, "://") || strings.ContainsAny(hostAlias, "/?# ") {
		t.Fatalf("dual-cluster Worker API host alias %q must be a host name or IP address without a scheme or path", hostAlias)
	}
	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(hostAlias, fmt.Sprint(port))}).String()
}

func (stub *dualClusterWorkerAPIStub) serveHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	switch request.URL.Path {
	case "/v1/workers/register":
		stub.register(response, request)
	case "/v1/workers/heartbeat":
		stub.heartbeat(response, request)
	case "/v1/workers/executions/claim":
		stub.claim(response, request)
	default:
		http.Error(response, "not found", http.StatusNotFound)
	}
}

func (stub *dualClusterWorkerAPIStub) register(response http.ResponseWriter, request *http.Request) {
	bearerToken, found := dualClusterBearerToken(request)
	if !found {
		http.Error(response, "authorization required", http.StatusUnauthorized)
		return
	}
	var input struct {
		ExecutionTargetID   string  `json:"executionTargetId"`
		AssignedExecutionID *string `json:"assignedExecutionId"`
		TargetKind          string  `json:"targetKind"`
		Namespace           string  `json:"namespace"`
		PodName             string  `json:"podName"`
		InstanceUID         string  `json:"instanceUid"`
	}
	if !decodeDualClusterWorkerAPIRequest(response, request, &input) {
		return
	}
	targetID, err := uuid.Parse(input.ExecutionTargetID)
	if err != nil || input.AssignedExecutionID == nil {
		http.Error(response, "invalid registration identity", http.StatusBadRequest)
		return
	}
	executionID, err := uuid.Parse(*input.AssignedExecutionID)
	if err != nil || input.TargetKind != "kubernetes" || strings.TrimSpace(input.Namespace) == "" ||
		strings.TrimSpace(input.PodName) == "" {
		http.Error(response, "invalid registration identity", http.StatusBadRequest)
		return
	}
	if _, err := uuid.Parse(input.InstanceUID); err != nil {
		http.Error(response, "invalid registration identity", http.StatusBadRequest)
		return
	}
	verified, err := stub.registrationVerifier.VerifyKubernetesWorkerRegistration(
		request.Context(), targetID, input.Namespace, input.PodName, input.InstanceUID, bearerToken,
	)
	if err != nil {
		http.Error(response, "pod-bound Kubernetes workload identity rejected", http.StatusUnauthorized)
		return
	}
	if verified.TargetID != targetID || verified.Namespace != input.Namespace || verified.PodName != input.PodName ||
		verified.PodUID != input.InstanceUID || verified.WorkerMode != "execution-pinned" ||
		verified.AssignedExecutionID == nil || *verified.AssignedExecutionID != executionID ||
		verified.Audience != executiontargets.KubernetesWorkerRegistrationAudience(targetID) ||
		strings.TrimSpace(verified.ServiceAccountName) == "" {
		http.Error(response, "verified Kubernetes workload identity did not match registration", http.StatusConflict)
		return
	}
	observation := dualClusterWorkerObservation{
		TargetID: input.ExecutionTargetID, ExecutionID: *input.AssignedExecutionID,
		PodName: input.PodName, PodUID: input.InstanceUID, PodBoundIdentityVerified: true,
	}
	key := dualClusterWorkerObservationKey(observation.TargetID, observation.ExecutionID, observation.PodName, observation.PodUID)
	workerToken := uuid.NewString()
	stub.mutex.Lock()
	if existing, found := stub.observations[key]; found {
		observation.Heartbeats = existing.Heartbeats
		observation.Claims = existing.Claims
	}
	stub.observations[key] = observation
	stub.tokenToKey[workerToken] = key
	stub.mutex.Unlock()
	response.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(response).Encode(map[string]any{
		"worker": map[string]any{
			"id": uuid.NewString(), "executionTargetId": observation.TargetID,
			"assignedExecutionId": observation.ExecutionID, "targetKind": "kubernetes",
			"workerMode": "execution-pinned", "podName": observation.PodName,
			"instanceUid": observation.PodUID, "protocolVersion": 2, "status": "online",
		},
		"token": workerToken,
	})
}

func (stub *dualClusterWorkerAPIStub) heartbeat(response http.ResponseWriter, request *http.Request) {
	key, found := stub.authorizedObservationKey(request)
	if !found {
		http.Error(response, "invalid Worker authorization", http.StatusUnauthorized)
		return
	}
	var input map[string]any
	if !decodeDualClusterWorkerAPIRequest(response, request, &input) {
		return
	}
	stub.mutex.Lock()
	observation := stub.observations[key]
	observation.Heartbeats++
	stub.observations[key] = observation
	stub.mutex.Unlock()
	response.WriteHeader(http.StatusNoContent)
}

func (stub *dualClusterWorkerAPIStub) claim(response http.ResponseWriter, request *http.Request) {
	key, found := stub.authorizedObservationKey(request)
	if !found {
		http.Error(response, "invalid Worker authorization", http.StatusUnauthorized)
		return
	}
	var input struct {
		ExecutionTargetID string  `json:"executionTargetId"`
		ExecutionID       *string `json:"executionId"`
		TargetKind        string  `json:"targetKind"`
	}
	if !decodeDualClusterWorkerAPIRequest(response, request, &input) {
		return
	}
	stub.mutex.Lock()
	observation := stub.observations[key]
	if input.ExecutionID == nil || observation.TargetID != input.ExecutionTargetID ||
		observation.ExecutionID != *input.ExecutionID || input.TargetKind != "kubernetes" {
		stub.mutex.Unlock()
		http.Error(response, "claim identity does not match registered Worker", http.StatusConflict)
		return
	}
	observation.Claims++
	stub.observations[key] = observation
	stub.mutex.Unlock()
	response.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(response, "{}\n")
}

func decodeDualClusterWorkerAPIRequest(response http.ResponseWriter, request *http.Request, output any) bool {
	request.Body = http.MaxBytesReader(response, request.Body, dualClusterWorkerAPIRequestLimit)
	decoder := json.NewDecoder(request.Body)
	if err := decoder.Decode(output); err != nil {
		http.Error(response, "invalid bounded JSON request", http.StatusBadRequest)
		return false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		http.Error(response, "invalid bounded JSON request", http.StatusBadRequest)
		return false
	}
	return true
}

func dualClusterBearerToken(request *http.Request) (string, bool) {
	value := strings.TrimSpace(request.Header.Get("Authorization"))
	if !strings.HasPrefix(value, "Bearer ") {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(value, "Bearer "))
	return token, token != ""
}

func (stub *dualClusterWorkerAPIStub) authorizedObservationKey(request *http.Request) (string, bool) {
	token, present := dualClusterBearerToken(request)
	if !present {
		return "", false
	}
	stub.mutex.RLock()
	defer stub.mutex.RUnlock()
	key, found := stub.tokenToKey[token]
	return key, found
}

func (stub *dualClusterWorkerAPIStub) observedRuntime(
	targetID uuid.UUID,
	executionID uuid.UUID,
	podName string,
	podUID string,
) bool {
	key := dualClusterWorkerObservationKey(targetID.String(), executionID.String(), podName, podUID)
	stub.mutex.RLock()
	defer stub.mutex.RUnlock()
	observation, found := stub.observations[key]
	return found && observation.PodBoundIdentityVerified && observation.Heartbeats > 0 && observation.Claims > 0
}

func dualClusterWorkerObservationKey(targetID, executionID, podName, podUID string) string {
	return strings.Join([]string{targetID, executionID, podName, podUID}, "|")
}

func newDualClusterIntegrationFixture(t *testing.T) (tenantExecutionPolicyFixture, string) {
	t.Helper()
	databaseURL := strings.TrimSpace(os.Getenv(dualClusterIntegrationDatabaseURLEnv))
	if databaseURL == "" {
		return newTenantExecutionPolicyFixture(t), "isolated-migrated-sqlite"
	}
	ctx := context.Background()
	db, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open disposable dual-cluster PostgreSQL authority: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := database.Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatalf("migrate disposable dual-cluster PostgreSQL authority: %v", err)
	}
	domain, err := bootstrap.Ensure(
		ctx,
		db,
		platform.ProfilePersonal,
		"dual-cluster-integration-"+uuid.NewString(),
	)
	if err != nil {
		t.Fatalf("bootstrap disposable dual-cluster PostgreSQL authority: %v", err)
	}
	projectID := uuid.New()
	sessionID := uuid.New()
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&persistence.Project{
			ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
			Name: "Dual-cluster integration", DefaultBranch: "main", Visibility: "organization",
			CreatedBy: domain.UserID,
		}).Error; err != nil {
			return err
		}
		return tx.Create(&persistence.AgentSession{
			ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID,
			ProjectID: projectID, CreatedBy: domain.UserID, Title: "Dual-cluster integration",
			Status: "active", Visibility: "private", Provider: "codex",
			ExecutionTargetID: domain.ExecutionTargetID,
		}).Error
	}); err != nil {
		t.Fatalf("seed disposable dual-cluster PostgreSQL authority: %v", err)
	}
	return tenantExecutionPolicyFixture{
		db: db,
		principal: identity.Principal{
			UserID: domain.UserID, ActiveTenantID: &domain.TenantID,
		},
		tenantID: domain.TenantID, organizationID: domain.OrganizationID,
		projectID: projectID, sessionID: sessionID, executionTargetID: domain.ExecutionTargetID,
	}, "disposable-migrated-postgresql"
}

// TestStage4DualClusterDisasterRecoveryAgainstRealAPIServers is an opt-in
// environment acceptance test. Kubernetes state is real; control-plane
// metadata uses a run-owned PostgreSQL database when the wrapper supplies one,
// and otherwise falls back to a bounded SQLite developer fixture.
func TestStage4DualClusterDisasterRecoveryAgainstRealAPIServers(t *testing.T) {
	primary, secondary := loadDualClusterIntegrationContexts(t)
	workerImage := strings.TrimSpace(os.Getenv(dualClusterIntegrationWorkerImageEnv))
	if workerImage == "" {
		t.Fatalf("%s is required for the runtime-ready dual-cluster acceptance test", dualClusterIntegrationWorkerImageEnv)
	}
	workerImageID := strings.TrimSpace(os.Getenv(dualClusterIntegrationWorkerImageIDEnv))
	if workerImageID == "" {
		t.Fatalf("%s is required for the runtime-ready dual-cluster acceptance test", dualClusterIntegrationWorkerImageIDEnv)
	}
	expectedWorkerImageIDs := parseDualClusterExpectedImageIDs(t, workerImageID)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	fixture, metadataStoreBoundary := newDualClusterIntegrationFixture(t)
	if metadataStoreBoundary != "disposable-migrated-postgresql" {
		t.Fatalf("%s is required for the shared-authority multi-replica acceptance lane", dualClusterIntegrationDatabaseURLEnv)
	}
	platformConfig, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := secret.NewCursorCipher(bytes.Repeat([]byte{0x64}, 32))
	if err != nil {
		t.Fatal(err)
	}
	targetService := executiontargets.NewService(fixture.db, platformConfig, cipher)
	workerAPI, primaryControlPlaneURL, secondaryControlPlaneURL := newDualClusterWorkerAPIStub(
		t,
		targetService,
		primary.WorkerAPIHost,
		secondary.WorkerAPIHost,
	)
	primaryControlPlane := NewService(fixture.db, projects.NewService(fixture.db), targetService)
	secondaryControlPlane := NewService(fixture.db, projects.NewService(fixture.db), targetService)
	fixture.service = primaryControlPlane
	routingPublisher := executiontargets.NewManagedKubernetesRoutingPublisher(
		targetService,
		executiontargets.ManagedKubernetesRoutingPublisherConfig{
			PublisherIdentity: "managed-kubernetes-routing-publisher:dual-cluster-integration",
			ObservationTTL:    2 * time.Minute,
		},
	)
	warmPublisher := executiontargets.NewManagedKubernetesWarmCapacityPublisher(
		targetService,
		executiontargets.ManagedKubernetesWarmCapacityPublisherConfig{
			PublisherIdentity: "managed-kubernetes-warm-capacity-publisher:dual-cluster-integration",
			ObservationTTL:    2 * time.Minute,
		},
	)
	reconcilerConfig := executiontargets.KubernetesReconcilerConfig{
		PublicControlPlaneURL: "http://127.0.0.1:3780",
		WorkerLeaseTTL:        30 * time.Second,
		PublishRoutingHealth:  routingPublisher.PublishReconcile,
		PublishWarmCapacity:   warmPublisher.PublishReconcile,
	}
	primaryReconciler := executiontargets.NewKubernetesReconciler(targetService, reconcilerConfig, slog.Default())
	secondaryReconciler := executiontargets.NewKubernetesReconciler(targetService, reconcilerConfig, slog.Default())

	primaryAPI := newDualClusterKubeAPI(t, primary)
	secondaryAPI := newDualClusterKubeAPI(t, secondary)
	if primaryAPI.baseURL == secondaryAPI.baseURL {
		t.Fatal("primary and secondary contexts must address distinct Kubernetes API servers")
	}
	assertDualClusterNamespaceOwnership(t, ctx, primaryAPI, primary)
	assertDualClusterNamespaceOwnership(t, ctx, secondaryAPI, secondary)

	capabilities := managedKubernetesFailoverTargetCapabilities()
	primaryView, err := targetService.Create(ctx, fixture.principal, fixture.tenantID, executiontargets.CreateInput{
		OrganizationID: &fixture.organizationID,
		Kind:           "kubernetes",
		Name:           "dual-cluster-primary-" + uuid.NewString(),
		Configuration:  dualClusterTargetConfiguration(primary, workerImage, primaryControlPlaneURL),
		Capabilities:   capabilities,
	})
	if err != nil {
		t.Fatal(err)
	}
	secondaryView, err := targetService.Create(ctx, fixture.principal, fixture.tenantID, executiontargets.CreateInput{
		OrganizationID: &fixture.organizationID,
		Kind:           "kubernetes",
		Name:           "dual-cluster-secondary-" + uuid.NewString(),
		Configuration:  dualClusterTargetConfiguration(secondary, workerImage, secondaryControlPlaneURL),
		Capabilities:   capabilities,
	})
	if err != nil {
		t.Fatal(err)
	}
	primaryTarget := loadTargetFailoverExecutionTarget(t, fixture.db, primaryView.ID)
	secondaryTarget := loadTargetFailoverExecutionTarget(t, fixture.db, secondaryView.ID)
	if primaryTarget.ID == secondaryTarget.ID || primary.Namespace == secondary.Namespace || primary.ContextLabel == secondary.ContextLabel {
		t.Fatal("dual-cluster integration contexts did not produce independent target, namespace, and context identities")
	}

	const primaryRegion = "integration-primary"
	const secondaryRegion = "integration-secondary"
	router := routing.NewService(fixture.db)
	maxFailovers := 2
	group, err := router.CreateGroup(ctx, routing.CreateGroupInput{
		TenantID: fixture.tenantID, OrganizationID: &fixture.organizationID,
		Name: "dual-cluster-dr-" + uuid.NewString(), Strategy: routing.StrategyPriority,
		AllowCrossRegion: true, MaxFailoverAttempts: &maxFailovers, HealthMaxStalenessSeconds: 120,
	})
	if err != nil {
		t.Fatal(err)
	}
	primaryMember, err := router.AddMember(ctx, routing.AddMemberInput{
		TenantID: fixture.tenantID, TargetGroupID: group.ID, ExecutionTargetID: primaryTarget.ID,
		Region: primaryRegion, ClusterID: primary.ContextLabel, Priority: 10, Weight: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := router.AddMember(ctx, routing.AddMemberInput{
		TenantID: fixture.tenantID, TargetGroupID: group.ID, ExecutionTargetID: secondaryTarget.ID,
		Region: secondaryRegion, ClusterID: secondary.ContextLabel, Priority: 20, Weight: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Updates(map[string]any{
			"requested_execution_target_id": primaryTarget.ID,
			"execution_target_group_id":     group.ID,
			"routing_policy_version":        group.Version,
			"execution_target_id":           primaryTarget.ID,
		}).Error; err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Add(-5 * time.Second).Truncate(time.Second)
	turnID := uuid.New()
	if err := fixture.db.Create(&persistence.AgentTurn{
		ID: turnID, TenantID: fixture.tenantID, SessionID: fixture.sessionID,
		CreatedBy: fixture.principal.UserID, Status: "queued", InputText: "dual-cluster-disaster-recovery", CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	provider := "codex"
	source := persistence.AgentExecution{
		ID: uuid.New(), TenantID: fixture.tenantID, SessionID: fixture.sessionID, TurnID: turnID,
		Attempt: 1, Status: "queued", ExecutionTargetID: primaryTarget.ID, TargetKind: "kubernetes",
		Provider: &provider, ProviderResumeStrategySnapshot: "authoritative-history",
		WarmPoolModeSnapshot: "disabled", Generation: 1, RequestedBy: fixture.principal.UserID, QueuedAt: now,
	}
	routing.ApplyExecutionSelection(&source, routing.Selection{
		Target: primaryTarget, Group: group, Member: primaryMember, RoutingReason: routing.StrategyPriority,
	})
	if err := fixture.db.Create(&source).Error; err != nil {
		t.Fatal(err)
	}

	if err := primaryReconciler.ReconcileOnce(ctx); err != nil {
		t.Fatalf("initial two-cluster Kubernetes reconcile: %v", err)
	}
	assertDualClusterFoundation(t, ctx, primaryAPI, primary.Namespace, primaryTarget.ID)
	assertDualClusterFoundation(t, ctx, secondaryAPI, secondary.Namespace, secondaryTarget.ID)
	sourcePod, sourceRuntimeReadyAt := waitForDualClusterExecutionRuntimeReady(
		t, ctx, primaryAPI, workerAPI, primary.Namespace, primaryTarget.ID, source.ID, expectedWorkerImageIDs, "primary source",
	)
	assertDualClusterUnboundAPICredentialRejected(
		t, ctx, targetService, primaryTarget.ID, primary, sourcePod,
	)
	if pods := secondaryAPI.listTargetPods(t, ctx, secondary.Namespace, secondaryTarget.ID); len(pods) != 0 {
		t.Fatalf("secondary target unexpectedly had Pods before failover: %#v", pods)
	}

	artifactReadyAt := time.Now().UTC().Add(-2 * time.Second).Truncate(time.Second)
	artifact := createReadyExecutionArtifact(t, fixture, fixture.sessionID, &source.ID, artifactReadyAt)
	artifactEvent := appendArtifactReadyEvent(t, fixture, fixture.sessionID, &source.ID, artifact.ID, artifactReadyAt)
	bundleBoundaryTransition := fixture.db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ? AND status = ? AND generation = ?", fixture.tenantID, source.ID, "queued", source.Generation).
		Update("status", "waiting-for-approval")
	if bundleBoundaryTransition.Error != nil || bundleBoundaryTransition.RowsAffected != 1 {
		t.Fatalf(
			"establish source Recovery Bundle boundary: rows=%d err=%v",
			bundleBoundaryTransition.RowsAffected,
			bundleBoundaryTransition.Error,
		)
	}
	bundlePayload := map[string]any{
		"execution": map[string]any{
			"selectedRegion": primaryRegion, "selectedClusterId": primary.ContextLabel,
		},
		"workload": map[string]any{
			"resumeSnapshot": map[string]any{
				"artifactReferences": []map[string]any{{
					"sequence": artifactEvent.Sequence, "artifactId": artifact.ID, "executionId": source.ID,
				}},
			},
			"memoryReferences": []map[string]any{},
		},
	}
	bundle := persistence.ExecutionRecoveryBundle{
		ID: uuid.New(), TenantID: fixture.tenantID, SessionID: fixture.sessionID, TurnID: turnID,
		ExecutionID: source.ID, Generation: source.Generation, SchemaVersion: 1, RecoveryReason: "initial-claim",
		AuthoritativeHistorySequence: artifactEvent.Sequence, Payload: bundlePayload,
		CreatedAt: artifactReadyAt,
	}
	sealTargetFailoverRecoveryBundle(t, &bundle)
	if err := fixture.db.Create(&bundle).Error; err != nil {
		t.Fatal(err)
	}
	recoveryReason := "suspend-resume"
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, source.ID).
		Updates(map[string]any{"status": "suspended", "next_recovery_reason": recoveryReason}).Error; err != nil {
		t.Fatal(err)
	}

	assertFailoverMissingReadinessIsMutationFree(t, ctx, fixture, source, primaryTarget.ID)

	sourceDRDomain := routing.DRDomainForLocation(primaryRegion, primary.ContextLabel)
	destinationDRDomain := routing.DRDomainForLocation(secondaryRegion, secondary.ContextLabel)
	readinessObservedAt := time.Now().UTC()
	readiness, err := router.ObserveDRReadiness(ctx, routing.DRReadinessObservation{
		ExecutionTargetID: secondaryTarget.ID, SourceDRDomain: sourceDRDomain, DRDomain: destinationDRDomain,
		ReplicatedThroughAt: artifactReadyAt, ArtifactsReady: true,
		CheckpointsReady: false, MemoryReady: false,
		PublisherIdentity: "dual-cluster-dr-readiness:" + secondary.ContextLabel,
		ObservedAt:        readinessObservedAt, TTL: 2 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if readiness.SourceDRDomain != sourceDRDomain || readiness.DRDomain != destinationDRDomain ||
		!readiness.ArtifactsReady || !readiness.ReplicatedThroughAt.Equal(artifactReadyAt) {
		t.Fatalf("persisted DR readiness was not the exact source-domain artifact watermark: %#v", readiness)
	}
	if _, err := router.ObserveLocationOutage(ctx, routing.LocationOutageObservation{
		TenantID: fixture.tenantID, Region: primaryRegion, ClusterID: primary.ContextLabel,
		Status: routing.LocationStatusUnreachable, PublisherIdentity: "dual-cluster-outage:" + primary.ContextLabel,
		ObservedAt: time.Now().UTC(), TTL: 2 * time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	leaderFence := createTargetFailoverFence(t, fixture.db, 44004)
	failoverStartedAt := time.Now().UTC()
	type sweepResult struct {
		summary TargetFailoverSweep
		err     error
	}
	sweepResults := make(chan sweepResult, 2)
	for _, controlPlane := range []*Service{primaryControlPlane, secondaryControlPlane} {
		controlPlane := controlPlane
		go func() {
			summary, sweepErr := controlPlane.ReconcileTargetFailovers(ctx, leaderFence, 10)
			sweepResults <- sweepResult{summary: summary, err: sweepErr}
		}()
	}
	totalSweep := TargetFailoverSweep{Failures: make(map[string]int)}
	for range 2 {
		result := <-sweepResults
		if result.err != nil {
			t.Fatalf("concurrent dual-cluster failover sweep: %v", result.err)
		}
		totalSweep.Candidates += result.summary.Candidates
		totalSweep.Committed += result.summary.Committed
		totalSweep.Rejected += result.summary.Rejected
		for code, count := range result.summary.Failures {
			totalSweep.Failures[code] += count
		}
	}
	if totalSweep.Candidates < 1 || totalSweep.Candidates > 2 || totalSweep.Committed != 1 ||
		totalSweep.Rejected != totalSweep.Candidates-totalSweep.Committed {
		t.Fatalf("concurrent dual-cluster failover sweep = %#v", totalSweep)
	}
	allowedConcurrentRejections := map[string]bool{
		"target_failover_source_stale":    true,
		"target_failover_plan_conflict":   true,
		"target_failover_source_conflict": true,
		"target_failover_commit_conflict": true,
	}
	concurrentSweepRejectionCode := ""
	for code, count := range totalSweep.Failures {
		if !allowedConcurrentRejections[code] || count != 1 || concurrentSweepRejectionCode != "" {
			t.Fatalf("concurrent dual-cluster failover had an unsafe rejection: %#v", totalSweep)
		}
		concurrentSweepRejectionCode = code
	}
	if totalSweep.Rejected == 0 && concurrentSweepRejectionCode != "" ||
		totalSweep.Rejected == 1 && concurrentSweepRejectionCode == "" {
		t.Fatalf("concurrent dual-cluster failover rejection evidence is inconsistent: %#v", totalSweep)
	}
	secondSweep, err := secondaryControlPlane.ReconcileTargetFailovers(ctx, leaderFence, 10)
	if err != nil {
		t.Fatal(err)
	}
	if secondSweep.Candidates != 0 || secondSweep.Committed != 0 || secondSweep.Rejected != 0 {
		t.Fatalf("repeat failover sweep was not idempotent: %#v", secondSweep)
	}

	destination, audit := assertCommittedDualClusterFailover(
		t, fixture.db, fixture.tenantID, source, primaryTarget, secondaryTarget, bundle.ID, leaderFence,
	)
	if err := secondaryReconciler.ReconcileOnce(ctx); err != nil {
		t.Fatalf("post-failover two-cluster Kubernetes reconcile: %v", err)
	}
	successorPod, successorRuntimeReadyAt := waitForDualClusterExecutionRuntimeReady(
		t, ctx, secondaryAPI, workerAPI, secondary.Namespace, secondaryTarget.ID, destination.ID, expectedWorkerImageIDs, "secondary successor",
	)
	assertDualClusterUnboundAPICredentialRejected(
		t, ctx, targetService, secondaryTarget.ID, secondary, successorPod,
	)
	waitForDualClusterPodUIDAbsent(t, ctx, primaryAPI, primary.Namespace, primaryTarget.ID, sourcePod.Metadata.UID)
	pressure := exerciseDualClusterPressureAndChaos(
		t,
		ctx,
		fixture,
		primaryReconciler,
		secondaryReconciler,
		workerAPI,
		primaryAPI,
		secondaryAPI,
		primary,
		secondary,
		primaryTarget,
		secondaryTarget,
		destination,
		successorPod,
		expectedWorkerImageIDs,
	)

	evidencePath := strings.TrimSpace(os.Getenv(dualClusterIntegrationEvidencePathEnv))
	if evidencePath != "" {
		detail := map[string]any{
			"schemaVersion": 1,
			"test":          "stage4-dual-cluster-disaster-recovery",
			"observedAt":    time.Now().UTC(),
			"boundary": map[string]any{
				"metadataStore":                             metadataStoreBoundary,
				"controlPlaneServiceInstances":              2,
				"kubernetes":                                "two-real-api-servers",
				"workerAPI":                                 "bounded-stub-with-production-kubernetes-identity-verifier",
				"podBoundWorkloadIdentityVerified":          true,
				"fullWorkerRegistrationPersistenceVerified": false,
				"recoveryBundleIntegrityVerified":           true,
				"recoveryBundleRuntimeConsumptionVerified":  false,
				"replicationVerified":                       false,
			},
			"primary": map[string]any{
				"contextLabel": primary.ContextLabel, "namespace": primary.Namespace,
				"targetId": primaryTarget.ID, "foundationVerified": true,
				"sourcePodCreated": true, "sourceRuntimeReady": true, "sourcePodAbsent": true,
				"sourceRuntimeReadyAt": sourceRuntimeReadyAt,
			},
			"secondary": map[string]any{
				"contextLabel": secondary.ContextLabel, "namespace": secondary.Namespace,
				"targetId": secondaryTarget.ID, "foundationVerified": true, "successorPodCreated": true,
				"successorRuntimeReady": true, "successorRuntimeReadyAt": successorRuntimeReadyAt,
			},
			"failover": map[string]any{
				"sourceExecutionId": source.ID, "destinationExecutionId": destination.ID,
				"sourceRecoveryBundleId": bundle.ID, "leaderFencingToken": audit.LeaderFencingToken,
				"committedAttempts": 1, "eventCount": 1, "outboxCount": 1,
			},
			"metrics": map[string]any{
				"controlPathSuccessorRuntimeReadyLatencyMillis": successorRuntimeReadyAt.Sub(failoverStartedAt).Milliseconds(),
				"declaredWatermarkObservationLagMillis":         readinessObservedAt.Sub(artifactReadyAt).Milliseconds(),
				"replicatedThroughAt":                           readiness.ReplicatedThroughAt,
				"successorRuntimeReadyAt":                       successorRuntimeReadyAt,
				"pressureExecutionsPerCluster":                  pressure.ExecutionsPerCluster,
				"pressurePeakPodsPerCluster":                    pressure.PeakPodsPerCluster,
				"chaosCycles":                                   pressure.ChaosCycles,
				"replacedPodUIDs":                               pressure.ReplacedPodUIDs,
				"concurrentSweepCandidates":                     totalSweep.Candidates,
				"concurrentSweepRejected":                       totalSweep.Rejected,
				"concurrentSweepRejectionCode":                  concurrentSweepRejectionCode,
			},
			"assertions": map[string]any{
				"missingReadinessFailedClosed": true, "exactReadinessAccepted": true,
				"sourcePlacementImmutable": true, "singleSuccessor": true,
				"successorLineagePersisted": true, "obsoletePrimaryPodAbsent": true,
				"sourceRuntimeReady": true, "successorRuntimeReady": true,
				"recoveryBundleIntegrityVerified": true, "podBoundWorkloadIdentityVerified": true,
				"postgresSharedAuthorityVerified":         metadataStoreBoundary == "disposable-migrated-postgresql",
				"concurrentControlPlaneSweepSingleCommit": true,
				"multiClusterPressureVerified":            pressure.MultiClusterPressureVerified,
				"reconcilerHandoffVerified":               pressure.ReconcilerHandoffVerified,
				"boundedChaosCyclesCompleted":             pressure.ChaosCycles == dualClusterChaosCycles,
			},
		}
		if err := writeDualClusterEvidenceAtomically(evidencePath, detail); err != nil {
			t.Fatalf("write redacted dual-cluster evidence detail: %v", err)
		}
	}
}

type dualClusterPressureResult struct {
	ExecutionsPerCluster         int
	PeakPodsPerCluster           int
	ChaosCycles                  int
	ReplacedPodUIDs              int
	MultiClusterPressureVerified bool
	ReconcilerHandoffVerified    bool
}

type dualClusterPressureExecution struct {
	ID       uuid.UUID
	TargetID uuid.UUID
}

func exerciseDualClusterPressureAndChaos(
	t *testing.T,
	ctx context.Context,
	fixture tenantExecutionPolicyFixture,
	primaryReconciler *executiontargets.KubernetesReconciler,
	secondaryReconciler *executiontargets.KubernetesReconciler,
	workerAPI *dualClusterWorkerAPIStub,
	primaryAPI *dualClusterKubeAPI,
	secondaryAPI *dualClusterKubeAPI,
	primary dualClusterIntegrationContext,
	secondary dualClusterIntegrationContext,
	primaryTarget persistence.ExecutionTarget,
	secondaryTarget persistence.ExecutionTarget,
	priorSuccessor persistence.AgentExecution,
	priorSuccessorPod dualClusterPod,
	expectedWorkerImageIDs []string,
) dualClusterPressureResult {
	t.Helper()
	finishedAt := time.Now().UTC()
	completed := fixture.db.WithContext(ctx).Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ? AND status = ?", fixture.tenantID, priorSuccessor.ID, "recovering").
		Updates(map[string]any{"status": "completed", "next_recovery_reason": nil, "finished_at": finishedAt})
	if completed.Error != nil || completed.RowsAffected != 1 {
		t.Fatalf("complete pre-pressure successor: rows=%d err=%v", completed.RowsAffected, completed.Error)
	}
	if err := secondaryReconciler.ReconcileOnce(ctx); err != nil {
		t.Fatalf("handoff reconciler cleanup before pressure: %v", err)
	}
	waitForDualClusterPodUIDAbsent(
		t, ctx, secondaryAPI, secondary.Namespace, secondaryTarget.ID, priorSuccessorPod.Metadata.UID,
	)

	type clusterLane struct {
		name       string
		api        *dualClusterKubeAPI
		context    dualClusterIntegrationContext
		target     persistence.ExecutionTarget
		executions []dualClusterPressureExecution
	}
	lanes := []*clusterLane{
		{name: "primary", api: primaryAPI, context: primary, target: primaryTarget},
		{name: "secondary", api: secondaryAPI, context: secondary, target: secondaryTarget},
	}
	queuedAt := time.Now().UTC().Truncate(time.Microsecond)
	if err := fixture.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, lane := range lanes {
			for index := 0; index < dualClusterPressurePerCluster; index++ {
				sessionID := uuid.New()
				turnID := uuid.New()
				executionID := uuid.New()
				createdAt := queuedAt.Add(time.Duration(index) * time.Millisecond)
				models := []any{
					&persistence.AgentSession{
						ID: sessionID, TenantID: fixture.tenantID, OrganizationID: fixture.organizationID,
						ProjectID: fixture.projectID, CreatedBy: fixture.principal.UserID,
						Title: "Dual-cluster pressure " + lane.name, Status: "active", Visibility: "private",
						Provider: "codex", ExecutionTargetID: lane.target.ID,
						CreatedAt: createdAt, UpdatedAt: createdAt,
					},
					&persistence.AgentTurn{
						ID: turnID, TenantID: fixture.tenantID, SessionID: sessionID,
						CreatedBy: fixture.principal.UserID, Status: "queued",
						InputText: fmt.Sprintf("dual-cluster-pressure-%s-%d", lane.name, index),
						CreatedAt: createdAt,
					},
					&persistence.AgentExecution{
						ID: executionID, TenantID: fixture.tenantID, SessionID: sessionID, TurnID: turnID,
						QueueClass: "interactive", QueuePriority: 0, QuotaUnits: 1,
						Attempt: 1, Status: "queued", ExecutionTargetID: lane.target.ID, TargetKind: "kubernetes",
						Provider: stringPointer("codex"), ProviderResumeStrategySnapshot: "authoritative-history",
						WarmPoolModeSnapshot: "disabled", Generation: 1,
						RequestedBy: fixture.principal.UserID, QueuedAt: createdAt,
					},
				}
				for _, model := range models {
					if err := tx.Create(model).Error; err != nil {
						return err
					}
				}
				lane.executions = append(lane.executions, dualClusterPressureExecution{ID: executionID, TargetID: lane.target.ID})
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed dual-cluster pressure queue: %v", err)
	}

	reconcileConcurrently := func() {
		errors := make(chan error, 2)
		go func() { errors <- primaryReconciler.ReconcileOnce(ctx) }()
		go func() { errors <- secondaryReconciler.ReconcileOnce(ctx) }()
		for range 2 {
			if err := <-errors; err != nil {
				t.Fatalf("concurrent control-plane reconciler cycle: %v", err)
			}
		}
	}

	const waveSize = 4
	replacedPodUIDs := 0
	for waveStart := 0; waveStart < dualClusterPressurePerCluster; waveStart += waveSize {
		if waveStart == 0 {
			reconcileConcurrently()
		} else if err := secondaryReconciler.ReconcileOnce(ctx); err != nil {
			t.Fatalf("handoff reconciler pressure cycle: %v", err)
		}
		activePods := make(map[uuid.UUID]dualClusterPod, waveSize*len(lanes))
		for _, lane := range lanes {
			pods := lane.api.listTargetPods(t, ctx, lane.context.Namespace, lane.target.ID)
			if len(pods) != waveSize {
				t.Fatalf("%s pressure wave %d Pod count = %d, want hard cap %d", lane.name, waveStart/waveSize, len(pods), waveSize)
			}
			for _, execution := range lane.executions[waveStart : waveStart+waveSize] {
				pod, _ := waitForDualClusterExecutionRuntimeReady(
					t, ctx, lane.api, workerAPI, lane.context.Namespace, lane.target.ID,
					execution.ID, expectedWorkerImageIDs, lane.name+" pressure execution",
				)
				activePods[execution.ID] = pod
			}
		}

		if waveStart == 0 {
			for cycle := 0; cycle < dualClusterChaosCycles; cycle++ {
				for _, lane := range lanes {
					execution := lane.executions[cycle%waveSize]
					pod := activePods[execution.ID]
					if err := lane.api.deletePodWithUID(
						ctx, lane.context.Namespace, pod.Metadata.Name, pod.Metadata.UID,
					); err != nil {
						t.Fatalf("delete exact %s chaos Pod UID: %v", lane.name, err)
					}
					waitForDualClusterPodUIDAbsent(
						t, ctx, lane.api, lane.context.Namespace, lane.target.ID, pod.Metadata.UID,
					)
				}
				reconciler := primaryReconciler
				if cycle%2 == 0 {
					reconciler = secondaryReconciler
				}
				if err := reconciler.ReconcileOnce(ctx); err != nil {
					t.Fatalf("chaos recovery reconciler cycle %d: %v", cycle, err)
				}
				for _, lane := range lanes {
					execution := lane.executions[cycle%waveSize]
					oldPod := activePods[execution.ID]
					replacement, _ := waitForDualClusterExecutionRuntimeReady(
						t, ctx, lane.api, workerAPI, lane.context.Namespace, lane.target.ID,
						execution.ID, expectedWorkerImageIDs, lane.name+" chaos replacement",
					)
					if replacement.Metadata.UID == oldPod.Metadata.UID {
						t.Fatalf("%s chaos cycle %d reused deleted Pod UID %s", lane.name, cycle, oldPod.Metadata.UID)
					}
					activePods[execution.ID] = replacement
					replacedPodUIDs++
				}
			}
		}

		waveFinishedAt := time.Now().UTC()
		if err := fixture.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			for _, lane := range lanes {
				ids := make([]uuid.UUID, 0, waveSize)
				for _, execution := range lane.executions[waveStart : waveStart+waveSize] {
					ids = append(ids, execution.ID)
				}
				updated := tx.Model(&persistence.AgentExecution{}).
					Where("tenant_id = ? AND id IN ? AND status = ?", fixture.tenantID, ids, "queued").
					Updates(map[string]any{"status": "completed", "finished_at": waveFinishedAt})
				if updated.Error != nil {
					return updated.Error
				}
				if updated.RowsAffected != int64(len(ids)) {
					return fmt.Errorf("complete %s pressure wave: rows=%d want=%d", lane.name, updated.RowsAffected, len(ids))
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := secondaryReconciler.ReconcileOnce(ctx); err != nil {
			t.Fatalf("cleanup completed pressure wave: %v", err)
		}
		for _, lane := range lanes {
			for _, execution := range lane.executions[waveStart : waveStart+waveSize] {
				waitForDualClusterPodUIDAbsent(
					t, ctx, lane.api, lane.context.Namespace, lane.target.ID, activePods[execution.ID].Metadata.UID,
				)
			}
			if pods := lane.api.listTargetPods(t, ctx, lane.context.Namespace, lane.target.ID); len(pods) != 0 {
				t.Fatalf("%s completed wave retained or prematurely replaced Pods: %#v", lane.name, pods)
			}
		}
	}

	allIDs := make([]uuid.UUID, 0, dualClusterPressurePerCluster*len(lanes))
	for _, lane := range lanes {
		for _, execution := range lane.executions {
			allIDs = append(allIDs, execution.ID)
		}
	}
	var terminalCount int64
	if err := fixture.db.WithContext(ctx).Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id IN ? AND status = ?", fixture.tenantID, allIDs, "completed").
		Count(&terminalCount).Error; err != nil {
		t.Fatal(err)
	}
	if terminalCount != int64(len(allIDs)) {
		t.Fatalf("dual-cluster pressure terminal executions = %d, want %d", terminalCount, len(allIDs))
	}
	return dualClusterPressureResult{
		ExecutionsPerCluster:         dualClusterPressurePerCluster,
		PeakPodsPerCluster:           waveSize,
		ChaosCycles:                  dualClusterChaosCycles,
		ReplacedPodUIDs:              replacedPodUIDs,
		MultiClusterPressureVerified: true,
		ReconcilerHandoffVerified:    true,
	}
}

func assertDualClusterUnboundAPICredentialRejected(
	t *testing.T,
	ctx context.Context,
	targets *executiontargets.Service,
	targetID uuid.UUID,
	cluster dualClusterIntegrationContext,
	pod dualClusterPod,
) {
	t.Helper()
	if _, err := targets.VerifyKubernetesWorkerRegistration(
		ctx,
		targetID,
		cluster.Namespace,
		pod.Metadata.Name,
		pod.Metadata.UID,
		cluster.Token,
	); err == nil {
		t.Fatal("unbound Kubernetes API credential was accepted as a Pod-bound Worker identity")
	}
}

func loadDualClusterIntegrationContexts(t *testing.T) (dualClusterIntegrationContext, dualClusterIntegrationContext) {
	t.Helper()
	load := func(prefix string) (dualClusterIntegrationContext, []string) {
		values := dualClusterIntegrationContext{
			APIServer:       strings.TrimSpace(os.Getenv(prefix + "_API_SERVER")),
			Token:           strings.TrimSpace(os.Getenv(prefix + "_TOKEN")),
			CACertificate:   strings.TrimSpace(os.Getenv(prefix + "_CA")),
			Namespace:       strings.TrimSpace(os.Getenv(prefix + "_NAMESPACE")),
			NamespaceUID:    strings.TrimSpace(os.Getenv(prefix + "_NAMESPACE_UID")),
			ContextLabel:    strings.TrimSpace(os.Getenv(prefix + "_CONTEXT_LABEL")),
			AcceptanceRunID: strings.TrimSpace(os.Getenv(dualClusterIntegrationRunIDEnv)),
			WorkerAPIHost:   strings.TrimSpace(os.Getenv(prefix + dualClusterWorkerAPIHostAliasSuffix)),
		}
		missing := make([]string, 0, 8)
		for suffix, value := range map[string]string{
			"_API_SERVER": values.APIServer, "_TOKEN": values.Token, "_CA": values.CACertificate,
			"_NAMESPACE": values.Namespace, "_NAMESPACE_UID": values.NamespaceUID,
			"_CONTEXT_LABEL": values.ContextLabel, dualClusterWorkerAPIHostAliasSuffix: values.WorkerAPIHost,
		} {
			if value == "" {
				missing = append(missing, prefix+suffix)
			}
		}
		if values.AcceptanceRunID == "" {
			missing = append(missing, dualClusterIntegrationRunIDEnv)
		}
		return values, missing
	}
	primary, primaryMissing := load("SYNARA_DUAL_CLUSTER_INTEGRATION_PRIMARY")
	secondary, secondaryMissing := load("SYNARA_DUAL_CLUSTER_INTEGRATION_SECONDARY")
	missing := append(primaryMissing, secondaryMissing...)
	if len(missing) != 0 {
		t.Skipf("set all dual-cluster integration variables; missing %s", strings.Join(missing, ", "))
	}
	if primary.ContextLabel == secondary.ContextLabel || primary.Namespace == secondary.Namespace {
		t.Fatal("primary and secondary context labels and namespaces must be distinct")
	}
	validateDualClusterNamespace(t, primary.Namespace)
	validateDualClusterNamespace(t, secondary.Namespace)
	if len(primary.AcceptanceRunID) > 63 || !dualClusterNamespacePattern.MatchString(primary.AcceptanceRunID) {
		t.Fatalf("dual-cluster acceptance run ID %q must be a lowercase Kubernetes label no longer than 63 characters", primary.AcceptanceRunID)
	}
	return primary, secondary
}

var dualClusterNamespacePattern = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$`)

func validateDualClusterNamespace(t *testing.T, namespace string) {
	t.Helper()
	if len(namespace) > 63 || !dualClusterNamespacePattern.MatchString(namespace) {
		t.Fatalf("dual-cluster namespace %q must be a lowercase DNS label no longer than 63 characters", namespace)
	}
}

func dualClusterTargetConfiguration(
	cluster dualClusterIntegrationContext,
	workerImage string,
	controlPlaneURL string,
) map[string]any {
	return map[string]any{
		"apiServer": cluster.APIServer, "bearerToken": cluster.Token, "caCertificate": cluster.CACertificate,
		"namespace": cluster.Namespace, "manageNamespace": false,
		"image": workerImage, "imagePullPolicy": "Never",
		"controlPlaneUrl": controlPlaneURL, "allowInsecureControlPlane": true,
		"runnerCommand": []string{"node", "/opt/synara/acceptance/provider-host-fixture.mjs", "--protocol-v2"}, "maxActivePods": 4,
		"egressCidrs": []string{"0.0.0.0/0"}, "cpuRequest": "50m", "cpuLimit": "1", "pidsLimit": 512,
		"memoryRequest": "128Mi", "memoryLimit": "512Mi",
		"ephemeralStorageRequest": "128Mi", "ephemeralStorageLimit": "1Gi", "workspaceSizeLimit": "128Mi",
		"quotaCpuRequests": "500m", "quotaCpuLimits": "4", "quotaMemoryRequests": "512Mi",
		"quotaMemoryLimits": "2Gi", "quotaEphemeralStorage": "2Gi",
	}
}

func assertDualClusterNamespaceOwnership(
	t *testing.T,
	ctx context.Context,
	api *dualClusterKubeAPI,
	cluster dualClusterIntegrationContext,
) {
	t.Helper()
	var namespace struct {
		Metadata struct {
			UID    string            `json:"uid"`
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	}
	path := "/api/v1/namespaces/" + url.PathEscape(cluster.Namespace)
	if err := api.do(ctx, http.MethodGet, path, &namespace, 200); err != nil {
		t.Fatalf("verify %s integration namespace ownership: %v", cluster.ContextLabel, err)
	}
	if namespace.Metadata.UID != cluster.NamespaceUID ||
		namespace.Metadata.Labels["synara.io/acceptance-run-id"] != cluster.AcceptanceRunID {
		t.Fatalf("%s integration namespace ownership changed; refusing reconciliation", cluster.ContextLabel)
	}
}

func newDualClusterKubeAPI(t *testing.T, cluster dualClusterIntegrationContext) *dualClusterKubeAPI {
	t.Helper()
	parsed, err := url.Parse(cluster.APIServer)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Path != "" {
		t.Fatalf("%s API server must be an HTTPS origin", cluster.ContextLabel)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(cluster.CACertificate)) {
		t.Fatalf("%s CA did not contain a PEM certificate", cluster.ContextLabel)
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	t.Cleanup(transport.CloseIdleConnections)
	return &dualClusterKubeAPI{
		baseURL: strings.TrimRight(cluster.APIServer, "/"), token: cluster.Token,
		client: &http.Client{Transport: transport, Timeout: 30 * time.Second},
	}
}

func (api *dualClusterKubeAPI) do(ctx context.Context, method, path string, out any, expected ...int) error {
	request, err := http.NewRequestWithContext(ctx, method, api.baseURL+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+api.token)
	response, err := api.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	for _, status := range expected {
		if response.StatusCode == status {
			if out != nil && len(body) != 0 {
				return json.Unmarshal(body, out)
			}
			return nil
		}
	}
	return fmt.Errorf("Kubernetes API %s %s returned HTTP %d: %s", method, path, response.StatusCode, strings.TrimSpace(string(body)))
}

func (api *dualClusterKubeAPI) deletePodWithUID(
	ctx context.Context,
	namespace string,
	podName string,
	podUID string,
) error {
	payload, err := json.Marshal(map[string]any{
		"apiVersion":         "v1",
		"kind":               "DeleteOptions",
		"gracePeriodSeconds": 0,
		"propagationPolicy":  "Background",
		"preconditions":      map[string]any{"uid": podUID},
	})
	if err != nil {
		return err
	}
	path := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/pods/" + url.PathEscape(podName)
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, api.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+api.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := api.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusAccepted {
		return fmt.Errorf(
			"Kubernetes API DELETE %s returned HTTP %d: %s",
			path,
			response.StatusCode,
			strings.TrimSpace(string(body)),
		)
	}
	return nil
}

func (api *dualClusterKubeAPI) listTargetPods(
	t *testing.T,
	ctx context.Context,
	namespace string,
	targetID uuid.UUID,
) []dualClusterPod {
	t.Helper()
	selector := url.QueryEscape("synara.io/execution-target-id=" + targetID.String())
	var pods dualClusterPodList
	path := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/pods?labelSelector=" + selector
	if err := api.do(ctx, http.MethodGet, path, &pods, 200); err != nil {
		t.Fatal(err)
	}
	return pods.Items
}

func waitForDualClusterExecutionRuntimeReady(
	t *testing.T,
	ctx context.Context,
	api *dualClusterKubeAPI,
	workerAPI *dualClusterWorkerAPIStub,
	namespace string,
	targetID uuid.UUID,
	executionID uuid.UUID,
	expectedImageIDs []string,
	description string,
) (dualClusterPod, time.Time) {
	t.Helper()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	lastState := "Pod not observed"
	for {
		pods := api.listTargetPods(t, ctx, namespace, targetID)
		matching := make([]dualClusterPod, 0, 1)
		for _, pod := range pods {
			if pod.Metadata.Labels["synara.io/execution-id"] == executionID.String() {
				matching = append(matching, pod)
			}
		}
		if len(matching) > 1 {
			t.Fatalf("%s had multiple exact execution Pods: %#v", description, matching)
		}
		if len(matching) == 1 {
			pod := matching[0]
			if pod.Status.Phase == "Failed" || pod.Status.Phase == "Succeeded" {
				t.Fatalf("%s Pod %q became terminal before runtime readiness: phase=%s", description, pod.Metadata.Name, pod.Status.Phase)
			}
			podReady, state := dualClusterPodRuntimeReady(pod, expectedImageIDs)
			workerObserved := workerAPI.observedRuntime(targetID, executionID, pod.Metadata.Name, pod.Metadata.UID)
			lastState = state + fmt.Sprintf(", workerRegisterHeartbeatClaim=%t", workerObserved)
			if podReady && workerObserved {
				return pod, time.Now().UTC()
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for %s runtime readiness: %v (%s)", description, ctx.Err(), lastState)
		case <-ticker.C:
		}
	}
}

func dualClusterPodRuntimeReady(pod dualClusterPod, expectedImageIDs []string) (bool, string) {
	if pod.Metadata.DeletionTimestamp != nil {
		return false, "Pod is terminating"
	}
	if pod.Status.Phase != "Running" {
		return false, "phase=" + pod.Status.Phase
	}
	podReady := false
	for _, condition := range pod.Status.Conditions {
		if condition.Type == "Ready" && condition.Status == "True" {
			podReady = true
			break
		}
	}
	if !podReady {
		return false, "Pod Ready condition is not True"
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != "agentd" {
			continue
		}
		if status.Started == nil || !*status.Started || !status.Ready || status.State.Running == nil {
			return false, "agentd is not started, ready, and running"
		}
		if status.RestartCount != 0 {
			return false, fmt.Sprintf("agentd restartCount=%d", status.RestartCount)
		}
		if strings.TrimSpace(status.ImageID) == "" {
			return false, "agentd imageID is empty"
		}
		if !dualClusterImageIDMatches(status.ImageID, expectedImageIDs) {
			return false, "agentd imageID does not match the required acceptance image ID"
		}
		return true, "Pod and agentd runtime are ready"
	}
	return false, "agentd container status is absent"
}

func parseDualClusterExpectedImageIDs(t *testing.T, value string) []string {
	t.Helper()
	parts := strings.Split(value, ",")
	if len(parts) == 0 || len(parts) > 2 {
		t.Fatalf("%s must contain one or two comma-separated sha256 image IDs", dualClusterIntegrationWorkerImageIDEnv)
	}
	result := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		normalized := normalizeDualClusterImageID(part)
		if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(normalized) {
			t.Fatalf("%s contained an invalid sha256 image ID", dualClusterIntegrationWorkerImageIDEnv)
		}
		if _, duplicate := seen[normalized]; duplicate {
			continue
		}
		seen[normalized] = struct{}{}
		result = append(result, normalized)
	}
	return result
}

func dualClusterImageIDMatches(observed string, expected []string) bool {
	observed = normalizeDualClusterImageID(observed)
	for _, candidate := range expected {
		if observed == normalizeDualClusterImageID(candidate) {
			return true
		}
	}
	return false
}

func normalizeDualClusterImageID(imageID string) string {
	value := strings.TrimSpace(imageID)
	if separator := strings.LastIndex(value, "@"); separator >= 0 {
		value = value[separator+1:]
	}
	for _, prefix := range []string{"docker-pullable://", "docker://", "containerd://"} {
		value = strings.TrimPrefix(value, prefix)
	}
	return value
}

func waitForDualClusterPodUIDAbsent(
	t *testing.T,
	ctx context.Context,
	api *dualClusterKubeAPI,
	namespace string,
	targetID uuid.UUID,
	podUID string,
) {
	t.Helper()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		found := false
		for _, pod := range api.listTargetPods(t, ctx, namespace, targetID) {
			if pod.Metadata.UID == podUID {
				found = true
				break
			}
		}
		if !found {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for obsolete source Pod UID %s to disappear: %v", podUID, ctx.Err())
		case <-ticker.C:
		}
	}
}

func assertDualClusterFoundation(
	t *testing.T,
	ctx context.Context,
	api *dualClusterKubeAPI,
	namespace string,
	targetID uuid.UUID,
) {
	t.Helper()
	compactTargetID := strings.ReplaceAll(targetID.String(), "-", "")[:12]
	baseName := "synara-agentd-" + compactTargetID
	resources := []string{
		"/api/v1/namespaces/" + url.PathEscape(namespace) + "/serviceaccounts/" + baseName,
		"/api/v1/namespaces/" + url.PathEscape(namespace) + "/secrets/" + baseName + "-registry",
		"/api/v1/namespaces/" + url.PathEscape(namespace) + "/resourcequotas/" + baseName,
		"/apis/networking.k8s.io/v1/namespaces/" + url.PathEscape(namespace) + "/networkpolicies/" + baseName,
	}
	for _, path := range resources {
		var resource struct {
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		}
		if err := api.do(ctx, http.MethodGet, path, &resource, 200); err != nil {
			t.Fatalf("foundation resource %s: %v", path, err)
		}
		if resource.Metadata.Labels["synara.io/execution-target-id"] != targetID.String() {
			t.Fatalf("foundation resource %s did not retain target identity: %#v", path, resource.Metadata.Labels)
		}
	}
}

func assertSingleExecutionPod(t *testing.T, pods []dualClusterPod, executionID uuid.UUID, description string) {
	t.Helper()
	if len(pods) != 1 || pods[0].Metadata.Labels["synara.io/execution-id"] != executionID.String() {
		t.Fatalf("%s Pod set = %#v", description, pods)
	}
}

func assertFailoverMissingReadinessIsMutationFree(
	t *testing.T,
	ctx context.Context,
	fixture tenantExecutionPolicyFixture,
	source persistence.AgentExecution,
	primaryTargetID uuid.UUID,
) {
	t.Helper()
	before := failoverMutationCounts(t, fixture.db, fixture.tenantID, fixture.sessionID, source.ID)
	_, err := fixture.service.FailoverExecution(
		ctx, fixture.tenantID, source.ID, createTargetFailoverFence(t, fixture.db, 44003), FailoverReasonRegionFailover,
	)
	assertSessionProblemCode(t, err, "target_group_dr_readiness_required")
	after := failoverMutationCounts(t, fixture.db, fixture.tenantID, fixture.sessionID, source.ID)
	if before != after {
		t.Fatalf("missing destination DR readiness mutated failover state: before=%#v after=%#v", before, after)
	}
	var storedSource persistence.AgentExecution
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, source.ID).Take(&storedSource).Error; err != nil {
		t.Fatal(err)
	}
	if storedSource.Status != "suspended" || storedSource.ExecutionTargetID != primaryTargetID ||
		storedSource.SelectedRegion == nil || source.SelectedRegion == nil || *storedSource.SelectedRegion != *source.SelectedRegion ||
		storedSource.SelectedClusterID == nil || source.SelectedClusterID == nil || *storedSource.SelectedClusterID != *source.SelectedClusterID {
		t.Fatalf("missing readiness rewrote source authority: %#v", storedSource)
	}
	var session persistence.AgentSession
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	if session.ExecutionTargetID != primaryTargetID {
		t.Fatalf("missing readiness advanced Session target authority to %s", session.ExecutionTargetID)
	}
}

type dualClusterFailoverMutationCounts struct {
	Audits     int64
	Successors int64
	Events     int64
	Outbox     int64
}

func failoverMutationCounts(
	t *testing.T,
	db *gorm.DB,
	tenantID, sessionID, sourceExecutionID uuid.UUID,
) dualClusterFailoverMutationCounts {
	t.Helper()
	var counts dualClusterFailoverMutationCounts
	queries := []struct {
		model any
		where string
		args  []any
		out   *int64
	}{
		{&persistence.ExecutionFailoverAttempt{}, "tenant_id = ? AND source_execution_id = ?", []any{tenantID, sourceExecutionID}, &counts.Audits},
		{&persistence.AgentExecution{}, "tenant_id = ? AND predecessor_execution_id = ?", []any{tenantID, sourceExecutionID}, &counts.Successors},
		{&persistence.SessionEvent{}, "tenant_id = ? AND session_id = ? AND event_type = ?", []any{tenantID, sessionID, "execution.failover-committed"}, &counts.Events},
		{&persistence.OutboxMessage{}, "tenant_id = ? AND topic = ?", []any{tenantID, "execution.failover-committed"}, &counts.Outbox},
	}
	for _, query := range queries {
		if err := db.Model(query.model).Where(query.where, query.args...).Count(query.out).Error; err != nil {
			t.Fatal(err)
		}
	}
	return counts
}

func assertCommittedDualClusterFailover(
	t *testing.T,
	db *gorm.DB,
	tenantID uuid.UUID,
	source persistence.AgentExecution,
	primaryTarget, secondaryTarget persistence.ExecutionTarget,
	bundleID uuid.UUID,
	leaderFence TargetFailoverFence,
) (persistence.AgentExecution, persistence.ExecutionFailoverAttempt) {
	t.Helper()
	counts := failoverMutationCounts(t, db, tenantID, source.SessionID, source.ID)
	if counts != (dualClusterFailoverMutationCounts{Audits: 1, Successors: 1, Events: 1, Outbox: 1}) {
		t.Fatalf("failover did not atomically create exactly one audit/successor/event/outbox: %#v", counts)
	}
	var storedSource persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", tenantID, source.ID).Take(&storedSource).Error; err != nil {
		t.Fatal(err)
	}
	if storedSource.Status != "interrupted" || storedSource.ExecutionTargetID != primaryTarget.ID ||
		storedSource.SelectedRegion == nil || source.SelectedRegion == nil || *storedSource.SelectedRegion != *source.SelectedRegion ||
		storedSource.SelectedClusterID == nil || source.SelectedClusterID == nil || *storedSource.SelectedClusterID != *source.SelectedClusterID {
		t.Fatalf("source placement snapshot was rewritten: %#v", storedSource)
	}
	var destination persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND predecessor_execution_id = ?", tenantID, source.ID).Take(&destination).Error; err != nil {
		t.Fatal(err)
	}
	if destination.ExecutionTargetID != secondaryTarget.ID || destination.Attempt != source.Attempt+1 ||
		destination.Status != "recovering" || destination.PredecessorExecutionID == nil || *destination.PredecessorExecutionID != source.ID ||
		destination.RoutingReason == nil || *destination.RoutingReason != "disaster-recovery" {
		t.Fatalf("successor lineage/placement = %#v", destination)
	}
	var audit persistence.ExecutionFailoverAttempt
	if err := db.Where("tenant_id = ? AND source_execution_id = ?", tenantID, source.ID).Take(&audit).Error; err != nil {
		t.Fatal(err)
	}
	if audit.Status != "committed" || audit.SourceExecutionTargetID != primaryTarget.ID ||
		audit.DestinationExecutionTargetID != secondaryTarget.ID || audit.DestinationExecutionID == nil || *audit.DestinationExecutionID != destination.ID ||
		audit.SourceRecoveryBundleID == nil || *audit.SourceRecoveryBundleID != bundleID ||
		audit.LeaderFencingToken != leaderFence.FencingToken || audit.Reason != FailoverReasonRegionFailover {
		t.Fatalf("failover audit = %#v", audit)
	}
	var event persistence.SessionEvent
	if err := db.Where("tenant_id = ? AND session_id = ? AND event_type = ?", tenantID, source.SessionID, "execution.failover-committed").Take(&event).Error; err != nil {
		t.Fatal(err)
	}
	if event.ExecutionID == nil || *event.ExecutionID != destination.ID ||
		fmt.Sprint(event.Payload["sourceExecutionId"]) != source.ID.String() ||
		fmt.Sprint(event.Payload["destinationExecutionId"]) != destination.ID.String() ||
		fmt.Sprint(event.Payload["sourceExecutionTargetId"]) != primaryTarget.ID.String() ||
		fmt.Sprint(event.Payload["destinationExecutionTargetId"]) != secondaryTarget.ID.String() {
		t.Fatalf("failover event payload = %#v", event)
	}
	var outbox persistence.OutboxMessage
	if err := db.Where("tenant_id = ? AND topic = ?", tenantID, "execution.failover-committed").Take(&outbox).Error; err != nil {
		t.Fatal(err)
	}
	if outbox.MessageKey != destination.ID.String() ||
		fmt.Sprint(outbox.Payload["sourceExecutionId"]) != source.ID.String() ||
		fmt.Sprint(outbox.Payload["executionId"]) != destination.ID.String() ||
		fmt.Sprint(outbox.Payload["sourceExecutionTargetId"]) != primaryTarget.ID.String() ||
		fmt.Sprint(outbox.Payload["executionTargetId"]) != secondaryTarget.ID.String() {
		t.Fatalf("failover outbox payload = %#v", outbox)
	}
	return destination, audit
}

func writeDualClusterEvidenceAtomically(path string, detail any) error {
	payload, err := json.MarshalIndent(detail, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".synara-dual-cluster-evidence-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return nil
}
