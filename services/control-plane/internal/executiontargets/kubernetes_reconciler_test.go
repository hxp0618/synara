package executiontargets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestKubernetesReconcilerAppliesSecurityFoundationAndExecutionPods(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	recoveryCalls := 0
	var observedPodUIDSets [][]string
	fixture.reconciler.config.RecoverExpired = func(context.Context, int) error {
		recoveryCalls++
		return nil
	}
	fixture.reconciler.config.ReconcileEphemeralWorkspaceCleanup = func(
		_ context.Context,
		targetID uuid.UUID,
		podUIDs []string,
		_ time.Time,
	) (int, error) {
		if targetID != fixture.targetID {
			t.Fatalf("unexpected ephemeral cleanup target: %s", targetID)
		}
		observedPodUIDSets = append(observedPodUIDSets, append([]string(nil), podUIDs...))
		return 0, nil
	}

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if recoveryCalls != 1 {
		t.Fatalf("Kubernetes reconciliation did not run lease recovery: %d", recoveryCalls)
	}
	if len(observedPodUIDSets) != 1 || len(observedPodUIDSets[0]) != 0 {
		t.Fatalf("first ephemeral cleanup pass saw unexpected Pods: %#v", observedPodUIDSets)
	}
	for _, kind := range []string{"Namespace", "ServiceAccount", "Secret", "ResourceQuota", "NetworkPolicy", "Pod"} {
		if client.kindCount(kind) == 0 {
			t.Fatalf("Kubernetes reconciliation omitted %s", kind)
		}
	}
	if client.kindCount("Pod") != 1 {
		t.Fatalf("maxActivePods=1 created %d Pods", client.kindCount("Pod"))
	}
	pod := client.lastKind("Pod")
	metadata := pod["metadata"].(map[string]any)
	labels := metadata["labels"].(map[string]string)
	if labels["synara.io/tenant-id"] != fixture.tenantID.String() ||
		labels["synara.io/organization-id"] != fixture.organizationID.String() ||
		labels["synara.io/project-id"] != fixture.projectID.String() ||
		labels["synara.io/session-id"] != fixture.sessionID.String() ||
		labels[kubernetesExecutionLabel] != fixture.executionIDs[0].String() || labels[kubernetesGenerationLabel] != "1" {
		t.Fatalf("Kubernetes Pod ownership/fencing labels are incomplete: %#v", labels)
	}
	spec := pod["spec"].(map[string]any)
	if spec["restartPolicy"] != "Never" || spec["automountServiceAccountToken"] != false ||
		spec["terminationGracePeriodSeconds"] != 30 {
		t.Fatalf("Kubernetes Pod runtime policy is unsafe: %#v", spec)
	}
	container := spec["containers"].([]any)[0].(map[string]any)
	if value, found := kubernetesEnvironmentValue(container, "SYNARA_AGENTD_WORKSPACE_ROOT"); !found || value != "/data/workspaces" {
		t.Fatalf("Kubernetes workspace root environment is %q", value)
	}
	if value, found := kubernetesEnvironmentValue(container, "SYNARA_AGENTD_GIT_CACHE_ROOT"); !found || value != "/data/git-cache" {
		t.Fatalf("Kubernetes default Git cache root environment is %q", value)
	}
	if fieldPath, found := kubernetesEnvironmentFieldPath(container, "SYNARA_AGENTD_INSTANCE_UID"); !found || fieldPath != "metadata.uid" {
		t.Fatalf("Kubernetes Pod UID environment uses %q", fieldPath)
	}
	volumes := spec["volumes"].([]any)
	workspaceVolume := kubernetesNamedObject(volumes, "workspace")
	if workspaceVolume == nil || workspaceVolume["emptyDir"] == nil {
		t.Fatalf("Kubernetes default workspace volume is not emptyDir: %#v", volumes)
	}
	if kubernetesNamedObject(volumes, "git-cache") != nil {
		t.Fatalf("Kubernetes default Pod created a redundant Git cache volume: %#v", volumes)
	}
	volumeMounts := container["volumeMounts"].([]any)
	workspaceMount := kubernetesNamedObject(volumeMounts, "workspace")
	if workspaceMount == nil || workspaceMount["mountPath"] != "/data" {
		t.Fatalf("Kubernetes workspace mount is invalid: %#v", volumeMounts)
	}
	if kubernetesNamedObject(volumeMounts, "git-cache") != nil {
		t.Fatalf("Kubernetes default Pod created a redundant Git cache mount: %#v", volumeMounts)
	}
	securityContext := container["securityContext"].(map[string]any)
	if securityContext["runAsNonRoot"] != true || securityContext["readOnlyRootFilesystem"] != true || securityContext["allowPrivilegeEscalation"] != false {
		t.Fatalf("Kubernetes container security context is incomplete: %#v", securityContext)
	}
	environment, err := json.Marshal(container["env"])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(environment, []byte("kubernetes-registration-secret")) ||
		!bytes.Contains(environment, []byte("secretKeyRef")) ||
		!bytes.Contains(environment, []byte("SYNARA_AGENTD_ASSIGNED_EXECUTION_ID")) ||
		!bytes.Contains(environment, []byte("SYNARA_AGENTD_PROVIDER_HOST_PROTOCOL")) ||
		!bytes.Contains(environment, []byte("SYNARA_AGENTD_DRAIN_TIMEOUT")) {
		t.Fatalf("Kubernetes Pod secret/assignment environment is invalid: %s", environment)
	}
	secret := client.lastKind("Secret")
	secretJSON, _ := json.Marshal(secret)
	if !bytes.Contains(secretJSON, []byte("kubernetes-registration-secret")) {
		t.Fatal("Kubernetes registration Secret did not receive the Worker token")
	}
	networkPolicy := client.lastKind("NetworkPolicy")
	networkJSON, _ := json.Marshal(networkPolicy)
	if !bytes.Contains(networkJSON, []byte("0.0.0.0/0")) || !bytes.Contains(networkJSON, []byte("policyTypes")) {
		t.Fatalf("Kubernetes NetworkPolicy is incomplete: %s", networkJSON)
	}
	quota := client.lastKind("ResourceQuota")
	quotaJSON, _ := json.Marshal(quota)
	if !bytes.Contains(quotaJSON, []byte(`"pods":"1"`)) {
		t.Fatalf("Kubernetes ResourceQuota is incomplete: %s", quotaJSON)
	}

	if err := fixture.db.Model(&persistence.AgentExecution{}).Where("id = ?", fixture.executionIDs[0]).Update("status", "completed").Error; err != nil {
		t.Fatal(err)
	}
	driftedPodUID := uuid.NewString()
	client.pods["label-drifted-worker"] = kubernetesPod{
		Name: "label-drifted-worker", UID: driftedPodUID, Phase: "Running",
		Labels: map[string]string{kubernetesTargetLabel: uuid.NewString()},
	}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(observedPodUIDSets) != 2 || len(observedPodUIDSets[1]) != 2 ||
		!containsString(observedPodUIDSets[1], driftedPodUID) {
		t.Fatalf("ephemeral cleanup did not receive the namespace-wide Pod UID set: %#v", observedPodUIDSets)
	}
	if len(client.deletedPods) != 1 || client.kindCount("Pod") != 2 {
		t.Fatalf("Kubernetes scheduler did not retire and replace the terminal execution Pod: deleted=%#v pods=%d", client.deletedPods, client.kindCount("Pod"))
	}
	secondPod := client.lastKind("Pod")
	secondLabels := secondPod["metadata"].(map[string]any)["labels"].(map[string]string)
	if secondLabels[kubernetesExecutionLabel] != fixture.executionIDs[1].String() {
		t.Fatalf("Kubernetes scheduler did not advance to the next queued execution: %#v", secondLabels)
	}

	var target persistence.ExecutionTarget
	if err := fixture.db.Where("id = ?", fixture.targetID).Take(&target).Error; err != nil {
		t.Fatal(err)
	}
	if target.Status != "active" {
		t.Fatalf("Kubernetes target status is %q", target.Status)
	}
	var audits []persistence.AuditLog
	if err := fixture.db.Where("resource_id = ? AND action = ?", fixture.targetID, "execution_target.kubernetes_reconciled").Find(&audits).Error; err != nil {
		t.Fatal(err)
	}
	if len(audits) != 2 {
		t.Fatalf("expected two material Kubernetes reconciliation audits, got %d", len(audits))
	}
	encodedAudits, _ := json.Marshal(audits)
	if bytes.Contains(encodedAudits, []byte("kubernetes-registration-secret")) || bytes.Contains(encodedAudits, []byte("kubernetes-api-token")) {
		t.Fatalf("Kubernetes secrets leaked into Audit metadata: %s", encodedAudits)
	}
}

func TestKubernetesReconcilerMountsPersistentGitCacheVolume(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "synara-git-cache")
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	pod := client.lastKind("Pod")
	if pod == nil {
		t.Fatal("Kubernetes reconciliation did not create a Pod")
	}
	spec := pod["spec"].(map[string]any)
	container := spec["containers"].([]any)[0].(map[string]any)
	if value, found := kubernetesEnvironmentValue(container, "SYNARA_AGENTD_WORKSPACE_ROOT"); !found || value != "/data/workspaces" {
		t.Fatalf("Kubernetes PVC Pod workspace root environment is %q", value)
	}
	if value, found := kubernetesEnvironmentValue(container, "SYNARA_AGENTD_GIT_CACHE_ROOT"); !found || value != "/git-cache" {
		t.Fatalf("Kubernetes PVC Git cache root environment is %q", value)
	}
	volumes := spec["volumes"].([]any)
	workspaceVolume := kubernetesNamedObject(volumes, "workspace")
	if workspaceVolume == nil || workspaceVolume["emptyDir"] == nil {
		t.Fatalf("Kubernetes PVC Pod lost its workspace emptyDir: %#v", volumes)
	}
	gitCacheVolume := kubernetesNamedObject(volumes, "git-cache")
	claim, ok := gitCacheVolume["persistentVolumeClaim"].(map[string]any)
	if gitCacheVolume == nil || !ok || claim["claimName"] != "synara-git-cache" {
		t.Fatalf("Kubernetes Git cache PVC is invalid: %#v", gitCacheVolume)
	}
	volumeMounts := container["volumeMounts"].([]any)
	workspaceMount := kubernetesNamedObject(volumeMounts, "workspace")
	gitCacheMount := kubernetesNamedObject(volumeMounts, "git-cache")
	if workspaceMount == nil || workspaceMount["mountPath"] != "/data" ||
		gitCacheMount == nil || gitCacheMount["mountPath"] != "/git-cache" {
		t.Fatalf("Kubernetes workspace or Git cache mount is invalid: %#v", volumeMounts)
	}
}

func TestKubernetesReconcilerSkipsCleanupWhenNamespacePodIdentityListFails(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	client := newFakeKubernetesClient()
	client.listPodUIDsErr = fmt.Errorf("namespace list failed")
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	cleanupCalls := 0
	fixture.reconciler.config.ReconcileEphemeralWorkspaceCleanup = func(
		context.Context, uuid.UUID, []string, time.Time,
	) (int, error) {
		cleanupCalls++
		return 0, nil
	}

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err == nil {
		t.Fatal("Kubernetes reconciliation accepted an incomplete namespace Pod identity list")
	}
	if cleanupCalls != 0 {
		t.Fatalf("ephemeral Workspace cleanup ran %d times with an incomplete Pod identity list", cleanupCalls)
	}
}

func TestKubernetesReconcilerRejectsEmptyNamespacePodUID(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	client := newFakeKubernetesClient()
	client.pods["invalid-identity"] = kubernetesPod{
		Name: "invalid-identity", UID: " ", Phase: "Running", Labels: map[string]string{},
	}
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	cleanupCalls := 0
	fixture.reconciler.config.ReconcileEphemeralWorkspaceCleanup = func(
		context.Context, uuid.UUID, []string, time.Time,
	) (int, error) {
		cleanupCalls++
		return 0, nil
	}

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err == nil {
		t.Fatal("Kubernetes reconciliation accepted an empty Pod UID")
	}
	if cleanupCalls != 0 {
		t.Fatalf("ephemeral Workspace cleanup ran %d times with an invalid Pod UID", cleanupCalls)
	}
}

func TestKubernetesClientDeletePodUsesExactUIDPrecondition(t *testing.T) {
	currentUID := uuid.NewString()
	deleted := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodDelete || request.URL.Path != "/api/v1/namespaces/synara-test/pods/worker" {
			t.Fatalf("unexpected Kubernetes delete request: %s %s", request.Method, request.URL.Path)
		}
		if request.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("unexpected Kubernetes delete content type %q", request.Header.Get("Content-Type"))
		}
		var options struct {
			Preconditions struct {
				UID string `json:"uid"`
			} `json:"preconditions"`
		}
		if err := json.NewDecoder(request.Body).Decode(&options); err != nil {
			t.Fatal(err)
		}
		if options.Preconditions.UID != currentUID {
			writer.WriteHeader(http.StatusConflict)
			_, _ = writer.Write([]byte(`{"reason":"Conflict","message":"UID precondition failed"}`))
			return
		}
		deleted = true
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := &kubernetesHTTPClient{baseURL: server.URL, token: "test-token", client: server.Client()}
	if err := client.DeletePod(context.Background(), "synara-test", "worker", uuid.NewString()); err == nil {
		t.Fatal("same-name replacement Pod was deleted with a stale UID")
	}
	if deleted {
		t.Fatal("Kubernetes API accepted deletion with a stale UID")
	}
	if err := client.DeletePod(context.Background(), "synara-test", "worker", currentUID); err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Fatal("exact Pod UID precondition did not allow deletion")
	}
}

func TestKubernetesClientListsNamespaceWidePodUIDsForCleanup(t *testing.T) {
	firstUID, secondUID := uuid.NewString(), uuid.NewString()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.Method != http.MethodGet || request.URL.Path != "/api/v1/namespaces/synara-test/pods" {
			t.Fatalf("unexpected Kubernetes list request: %s %s", request.Method, request.URL.Path)
		}
		if request.URL.Query().Has("labelSelector") {
			t.Fatalf("cleanup Pod identity list used a mutable label selector: %s", request.URL.RawQuery)
		}
		if request.URL.Query().Get("limit") != "100" {
			t.Fatalf("cleanup Pod identity list omitted its bounded page size: %s", request.URL.RawQuery)
		}
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Query().Get("continue") == "" {
			_, _ = fmt.Fprintf(writer, `{"metadata":{"continue":"next-page"},"items":[{"metadata":{"name":"managed","uid":%q,"labels":{"synara.io/execution-target-id":"target"}},"status":{"phase":"Running"}}]}`, firstUID)
			return
		}
		if request.URL.Query().Get("continue") != "next-page" {
			t.Fatalf("unexpected Kubernetes continuation token: %s", request.URL.RawQuery)
		}
		_, _ = fmt.Fprintf(writer, `{"metadata":{"continue":""},"items":[{"metadata":{"name":"drifted","uid":%q,"labels":{}},"status":{"phase":"Running"}}]}`, secondUID)
	}))
	defer server.Close()

	client := &kubernetesHTTPClient{baseURL: server.URL, token: "test-token", client: server.Client()}
	uids, err := client.ListPodUIDs(context.Background(), "synara-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(uids) != 2 || !containsString(uids, firstUID) || !containsString(uids, secondUID) {
		t.Fatalf("namespace-wide Pod UID list = %#v", uids)
	}
	if requests != 2 {
		t.Fatalf("namespace-wide Pod UID list used %d requests, want 2 paginated requests", requests)
	}
}

func TestKubernetesClientRejectsIncompletePaginatedPodUIDList(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Query().Get("limit") != "100" {
			t.Fatalf("Kubernetes Pod list omitted its bounded page size: %s", request.URL.RawQuery)
		}
		if requests == 1 {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"metadata":{"continue":"next-page"},"items":[{"metadata":{"name":"first","uid":"first-uid","labels":{}},"status":{"phase":"Running"}}]}`))
			return
		}
		writer.WriteHeader(http.StatusInternalServerError)
		_, _ = writer.Write([]byte(`{"reason":"InternalError","message":"second page failed"}`))
	}))
	defer server.Close()

	client := &kubernetesHTTPClient{baseURL: server.URL, token: "test-token", client: server.Client()}
	uids, err := client.ListPodUIDs(context.Background(), "synara-test")
	if err == nil {
		t.Fatalf("Kubernetes client accepted incomplete paginated Pod UIDs: %#v", uids)
	}
	if uids != nil {
		t.Fatalf("Kubernetes client returned partial Pod UIDs after a page failure: %#v", uids)
	}
	if requests != 2 {
		t.Fatalf("Kubernetes client made %d list requests, want 2", requests)
	}
}

type kubernetesReconcileFixture struct {
	db             *gorm.DB
	reconciler     *KubernetesReconciler
	tenantID       uuid.UUID
	organizationID uuid.UUID
	projectID      uuid.UUID
	sessionID      uuid.UUID
	targetID       uuid.UUID
	executionIDs   []uuid.UUID
}

func newKubernetesReconcileFixture(t *testing.T, gitCachePersistentVolumeClaims ...string) kubernetesReconcileFixture {
	t.Helper()
	gitCachePersistentVolumeClaim := ""
	if len(gitCachePersistentVolumeClaims) > 0 {
		gitCachePersistentVolumeClaim = gitCachePersistentVolumeClaims[0]
	}
	ctx := context.Background()
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "kubernetes-reconcile-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := secret.NewCursorCipher(bytes.Repeat([]byte{0x61}, 32))
	if err != nil {
		t.Fatal(err)
	}
	targetService := NewService(store.DB(), platformConfig, cipher)
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	configuration := map[string]any{
		"apiServer": "https://kubernetes.example.com", "bearerToken": "kubernetes-api-token",
		"caCertificate": "fake-ca-for-client-factory", "namespace": "synara-test", "manageNamespace": true,
		"image": "synara-agentd:test", "imagePullPolicy": "IfNotPresent",
		"controlPlaneUrl": "http://control-plane.test:3780", "allowInsecureControlPlane": true,
		"runnerCommand": []string{"provider-host", "run", "--jsonl"}, "maxActivePods": 1,
		"egressCidrs": []string{"0.0.0.0/0"}, "cpuRequest": "250m", "cpuLimit": "1",
		"memoryRequest": "256Mi", "memoryLimit": "1Gi", "workspaceSizeLimit": "2Gi",
		"quotaCpuRequests": "1", "quotaCpuLimits": "2", "quotaMemoryRequests": "2Gi", "quotaMemoryLimits": "4Gi",
	}
	if gitCachePersistentVolumeClaim != "" {
		configuration["gitCachePersistentVolumeClaim"] = gitCachePersistentVolumeClaim
	}
	target, err := targetService.Create(ctx, principal, domain.TenantID, CreateInput{
		OrganizationID: &domain.OrganizationID, Kind: "kubernetes", Name: "managed-kubernetes",
		Configuration: configuration,
		Capabilities:  map[string]any{"workspaceModes": []string{"local", "worktree"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	projectID := uuid.New()
	sessionID := uuid.New()
	executionIDs := []uuid.UUID{uuid.New(), uuid.New()}
	now := time.Now().UTC()
	models := []any{
		&persistence.Project{ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID, Name: "Kubernetes project", DefaultBranch: "main", Visibility: "organization", CreatedBy: domain.UserID},
		&persistence.AgentSession{ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID, ProjectID: projectID, CreatedBy: domain.UserID, Title: "Kubernetes session", Status: "active", Visibility: "organization", Provider: "codex", ExecutionTargetID: target.ID},
	}
	for index, executionID := range executionIDs {
		turnID := uuid.New()
		models = append(models,
			&persistence.AgentTurn{ID: turnID, TenantID: domain.TenantID, SessionID: sessionID, CreatedBy: domain.UserID, Status: "queued", InputText: fmt.Sprintf("Kubernetes turn %d", index)},
			&persistence.AgentExecution{ID: executionID, TenantID: domain.TenantID, SessionID: sessionID, TurnID: turnID, Attempt: 1, Status: "queued", ExecutionTargetID: target.ID, TargetKind: "kubernetes", RequestedBy: domain.UserID, QueuedAt: now.Add(time.Duration(index) * time.Second)},
		)
	}
	for _, model := range models {
		if err := store.DB().Create(model).Error; err != nil {
			t.Fatalf("seed Kubernetes fixture %T: %v", model, err)
		}
	}
	reconciler := NewKubernetesReconciler(targetService, KubernetesReconcilerConfig{
		RegistrationToken: "kubernetes-registration-secret", PublicControlPlaneURL: "http://control-plane.test:3780",
	}, slog.Default())
	return kubernetesReconcileFixture{
		db: store.DB(), reconciler: reconciler, tenantID: domain.TenantID, organizationID: domain.OrganizationID,
		projectID: projectID, sessionID: sessionID, targetID: target.ID, executionIDs: executionIDs,
	}
}

type fakeKubernetesFactory struct {
	client kubernetesClient
}

func (f *fakeKubernetesFactory) Open(kubernetesTargetConfiguration) (kubernetesClient, error) {
	return f.client, nil
}

type fakeKubernetesClient struct {
	applied        []map[string]any
	pods           map[string]kubernetesPod
	deletedPods    []string
	listPodUIDsErr error
}

func newFakeKubernetesClient() *fakeKubernetesClient {
	return &fakeKubernetesClient{pods: map[string]kubernetesPod{}}
}

func (c *fakeKubernetesClient) Apply(_ context.Context, _ string, object map[string]any) error {
	c.applied = append(c.applied, object)
	if object["kind"] == "Pod" {
		metadata := object["metadata"].(map[string]any)
		name := metadata["name"].(string)
		labels := metadata["labels"].(map[string]string)
		c.pods[name] = kubernetesPod{Name: name, UID: uuid.NewString(), Phase: "Pending", Labels: labels}
	}
	return nil
}

func (c *fakeKubernetesClient) ListPods(_ context.Context, _ string, targetID uuid.UUID) ([]kubernetesPod, error) {
	items := make([]kubernetesPod, 0, len(c.pods))
	for _, pod := range c.pods {
		if pod.Labels[kubernetesTargetLabel] == targetID.String() {
			items = append(items, pod)
		}
	}
	return items, nil
}

func (c *fakeKubernetesClient) ListPodUIDs(_ context.Context, _ string) ([]string, error) {
	if c.listPodUIDsErr != nil {
		return nil, c.listPodUIDsErr
	}
	uids := make([]string, 0, len(c.pods))
	for _, pod := range c.pods {
		uids = append(uids, pod.UID)
	}
	return uids, nil
}

func (c *fakeKubernetesClient) DeletePod(_ context.Context, _ string, name, uid string) error {
	pod, found := c.pods[name]
	if found && pod.UID != uid {
		return fmt.Errorf("Pod UID precondition failed: current=%s requested=%s", pod.UID, uid)
	}
	c.deletedPods = append(c.deletedPods, name)
	delete(c.pods, name)
	return nil
}

func (c *fakeKubernetesClient) kindCount(kind string) int {
	count := 0
	for _, object := range c.applied {
		if object["kind"] == kind {
			count++
		}
	}
	return count
}

func (c *fakeKubernetesClient) lastKind(kind string) map[string]any {
	for index := len(c.applied) - 1; index >= 0; index-- {
		if c.applied[index]["kind"] == kind {
			return c.applied[index]
		}
	}
	return nil
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func kubernetesEnvironmentValue(container map[string]any, name string) (string, bool) {
	for _, item := range container["env"].([]any) {
		entry := item.(map[string]any)
		if entry["name"] == name {
			value, ok := entry["value"].(string)
			return value, ok
		}
	}
	return "", false
}

func kubernetesEnvironmentFieldPath(container map[string]any, name string) (string, bool) {
	for _, item := range container["env"].([]any) {
		entry := item.(map[string]any)
		if entry["name"] != name {
			continue
		}
		valueFrom, ok := entry["valueFrom"].(map[string]any)
		if !ok {
			return "", false
		}
		fieldRef, ok := valueFrom["fieldRef"].(map[string]any)
		if !ok {
			return "", false
		}
		fieldPath, ok := fieldRef["fieldPath"].(string)
		return fieldPath, ok
	}
	return "", false
}

func kubernetesNamedObject(items []any, name string) map[string]any {
	for _, item := range items {
		entry := item.(map[string]any)
		if entry["name"] == name {
			return entry
		}
	}
	return nil
}
