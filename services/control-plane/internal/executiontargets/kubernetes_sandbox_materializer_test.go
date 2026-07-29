package executiontargets

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

type fakeSandboxClient struct {
	*fakeKubernetesClient
	acceptance kubernetesSandboxAcceptanceObservation
	claims     map[string]kubernetesSandboxClaimObservation
	sandboxes  map[string]kubernetesSandboxObservation
	deleted    []string
}

func newFakeSandboxClient(runtime string) *fakeSandboxClient {
	return &fakeSandboxClient{
		fakeKubernetesClient: newFakeKubernetesClient(),
		acceptance: kubernetesSandboxAcceptanceObservation{
			SandboxAPIReady: true, SandboxClaimAPIReady: true, SandboxTemplateAPIReady: true,
			SandboxWarmPoolAPIReady: true, OperatorReady: true,
			TemplateIdentity: "template-uid:1", TemplateRuntime: runtime, TemplateAgentdImage: "synara-agentd:test", AssignedExecutionFieldRefReady: true,
			TemplateSandboxRuntimeImage: "synara-agentd:test",
			TemplateSandboxRuntimeName:  kubernetesCocoonGuestContainerName,
			WarmPoolTemplateReady:       true, WarmPoolReady: true, WarmPoolDesiredReplicas: 0,
			WarmPoolUpdateStrategy: "Recreate", WarmPoolTemplateImageFresh: true,
			VirtualNodeReady: runtime == "vk-cocoon", KVMRuntimeReady: runtime == "vk-cocoon",
			CocoonTemplateSchedulingReady: runtime == "vk-cocoon",
			CocoonTemplateCleanupReady:    runtime == "vk-cocoon",
		},
		claims: map[string]kubernetesSandboxClaimObservation{}, sandboxes: map[string]kubernetesSandboxObservation{},
	}
}

func TestKubernetesCocoonSandboxMaterializerUsesGuestImageWithoutPodAgentdContract(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t)
	configuration := kubernetesTestConfiguration("")
	configuration["allocationBackend"] = "sandbox-operator-cocoon"
	configuration["sandboxTemplateName"] = "synara-worker"
	configuration["sandboxWarmPoolName"] = "synara-interactive"
	fixture.updateConfiguration(t, configuration)
	seedKubernetesAllocationGenerationFacts(t, fixture)
	cancelAdditionalSandboxExecutions(t, fixture)

	client := newFakeSandboxClient("vk-cocoon")
	client.acceptance.TemplateAgentdImage = ""
	client.acceptance.AssignedExecutionFieldRefReady = false
	client.acceptance.HostSupervisorReady = true
	client.acceptance.FencedVSockReady = true
	client.acceptance.GuestIsolationReady = true
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("materialize Cocoon SandboxClaim: %v", err)
	}

	var allocation persistence.ExecutionKubernetesAllocation
	if err := fixture.db.Where("execution_id = ?", fixture.executionIDs[0]).Take(&allocation).Error; err != nil {
		t.Fatal(err)
	}
	claim := client.claims[allocation.ClaimName]
	claim.Ready = true
	claim.SandboxName = "cocoon-sandbox"
	client.claims[allocation.ClaimName] = claim
	client.sandboxes[claim.SandboxName] = kubernetesSandboxObservation{UID: uuid.NewString(), PodName: "cocoon-pod"}
	client.pods["cocoon-pod"] = kubernetesPod{
		Name: "cocoon-pod", UID: uuid.NewString(), SandboxRuntimeImage: "synara-agentd:test",
		AgentdImage: "ignored-kubernetes-agentd-placeholder:old", Phase: "Running",
		Labels: map[string]string{kubernetesTargetLabel: fixture.targetID.String()},
	}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("bind Cocoon allocation by guest image: %v", err)
	}
	if err := fixture.db.Where("execution_id = ?", fixture.executionIDs[0]).Take(&allocation).Error; err != nil {
		t.Fatal(err)
	}
	if allocation.Status != "bound" || allocation.PodUID == nil {
		t.Fatalf("Cocoon allocation = %#v, want bound guest", allocation)
	}
}

func TestKubernetesSandboxAllocationDigestKeepsStandardCompatibilityAndVersionsCocoon(t *testing.T) {
	configuration := kubernetesTargetConfiguration{
		AllocationBackend: "sandbox-operator-standard", Namespace: "synara-workers",
		SandboxTemplateName: "synara-worker", SandboxWarmPoolName: "synara-interactive",
		SandboxClaimReadyTimeoutSeconds: 30,
	}
	standard, err := kubernetesSandboxAllocationConfigurationDigest(configuration, "template-uid:42", "synara-agentd:test")
	if err != nil {
		t.Fatal(err)
	}
	const legacyStandardDigest = "d26b9aab9bdd1bcdb179e97750592e540978ca87546cd79bf591bece1e993519"
	if standard != legacyStandardDigest {
		t.Fatalf("standard digest = %s, want legacy-compatible %s", standard, legacyStandardDigest)
	}
	configuration.AllocationBackend = "sandbox-operator-cocoon"
	cocoon, err := kubernetesSandboxAllocationConfigurationDigest(configuration, "template-uid:42", "synara-agentd:test")
	if err != nil {
		t.Fatal(err)
	}
	if cocoon == standard {
		t.Fatal("Cocoon supervisor/image contract must have a distinct versioned allocation digest")
	}
}

func (c *fakeSandboxClient) Apply(ctx context.Context, path string, object map[string]any) error {
	if object["kind"] != "SandboxClaim" {
		return c.fakeKubernetesClient.Apply(ctx, path, object)
	}
	c.applied = append(c.applied, object)
	metadata := object["metadata"].(map[string]any)
	name := metadata["name"].(string)
	if _, found := c.claims[name]; !found {
		spec := object["spec"].(map[string]any)
		warmPool := spec["warmPoolRef"].(map[string]any)["name"].(string)
		annotations := metadata["annotations"].(map[string]any)
		c.claims[name] = kubernetesSandboxClaimObservation{
			UID: uuid.NewString(), WarmPoolName: warmPool,
			ConfigurationDigest: annotations[kubernetesConfigAnnotation].(string),
		}
	}
	return nil
}

func (c *fakeSandboxClient) GetSandboxClaim(
	_ context.Context,
	_, name string,
) (kubernetesSandboxClaimObservation, bool, error) {
	claim, found := c.claims[name]
	return claim, found, nil
}

func (c *fakeSandboxClient) GetSandbox(
	_ context.Context,
	_, name string,
) (kubernetesSandboxObservation, bool, error) {
	sandbox, found := c.sandboxes[name]
	return sandbox, found, nil
}

func (c *fakeSandboxClient) DeleteSandboxClaim(_ context.Context, _, name, uid string) error {
	claim, found := c.claims[name]
	if found && claim.UID != uid {
		return errKubernetesPodUIDPreconditionFailed
	}
	delete(c.claims, name)
	c.deleted = append(c.deleted, name)
	return nil
}

func (c *fakeSandboxClient) ObserveSandboxAcceptance(
	context.Context,
	kubernetesTargetConfiguration,
) (kubernetesSandboxAcceptanceObservation, error) {
	return c.acceptance, nil
}

func TestKubernetesSandboxClaimMaterializerPersistsIdentityAndCleansUpByClaimUID(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t)
	configuration := kubernetesTestConfiguration("")
	configuration["allocationBackend"] = "sandbox-operator-standard"
	configuration["sandboxTemplateName"] = "synara-worker"
	configuration["sandboxWarmPoolName"] = "synara-interactive"
	fixture.updateConfiguration(t, configuration)
	seedKubernetesAllocationGenerationFacts(t, fixture)

	client := newFakeSandboxClient("standard")
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("materialize SandboxClaim: %v", err)
	}
	var allocation persistence.ExecutionKubernetesAllocation
	if err := fixture.db.Where("execution_id = ?", fixture.executionIDs[0]).Take(&allocation).Error; err != nil {
		t.Fatalf("load materializing allocation: %v", err)
	}
	if allocation.Status != "materializing" || allocation.Backend != "sandbox-operator-standard" {
		t.Fatalf("materializing allocation = %#v", allocation)
	}
	claim := client.claims[allocation.ClaimName]
	claim.Ready = true
	claim.SandboxName = "sandbox-one"
	client.claims[allocation.ClaimName] = claim
	client.sandboxes[claim.SandboxName] = kubernetesSandboxObservation{UID: uuid.NewString(), PodName: "sandbox-pod-one"}
	client.pods["sandbox-pod-one"] = kubernetesPod{
		Name: "sandbox-pod-one", UID: uuid.NewString(), AgentdImage: "synara-agentd:test", Phase: "Running", CreatedAt: time.Now().UTC(),
		Labels: map[string]string{kubernetesTargetLabel: fixture.targetID.String(), kubernetesExecutionLabel: fixture.executionIDs[0].String()},
	}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("bind Sandbox allocation: %v", err)
	}
	if err := fixture.db.Where("execution_id = ?", fixture.executionIDs[0]).Take(&allocation).Error; err != nil {
		t.Fatalf("reload bound allocation: %v", err)
	}
	if allocation.Status != "bound" || allocation.ClaimReadyAt == nil || allocation.ClaimUID == nil || allocation.SandboxUID == nil || allocation.PodUID == nil {
		t.Fatalf("bound allocation = %#v", allocation)
	}
	verifiedPod := kubernetesVerifiedPod{
		Name: *allocation.PodName, UID: *allocation.PodUID,
		Labels: map[string]string{
			kubernetesExecutionLabel:  fixture.executionIDs[0].String(),
			kubernetesGenerationLabel: "1", kubernetesWorkerModeLabel: kubernetesWorkerModeExecutionPinned,
		},
	}
	assignedExecutionID := fixture.executionIDs[0]
	if err := fixture.reconciler.targets.verifyKubernetesSandboxAllocationIdentity(
		context.Background(), fixture.targetID,
		kubernetesTargetConfiguration{AllocationBackend: "sandbox-operator-standard", Namespace: allocation.Namespace},
		verifiedPod,
		kubernetesVerifiedWorkerIdentity{WorkerMode: kubernetesWorkerModeExecutionPinned, AssignedExecutionID: &assignedExecutionID},
	); err != nil {
		t.Fatalf("verify bound Sandbox allocation identity: %v", err)
	}
	var nextFact persistence.ExecutionGenerationFact
	if err := fixture.db.Where("execution_id = ? AND generation = ?", fixture.executionIDs[0], 1).Take(&nextFact).Error; err != nil {
		t.Fatal(err)
	}
	nextFact.Generation = 2
	nextFact.CreatedAt = nextFact.CreatedAt.Add(time.Second)
	nextFact.UpdatedAt = nextFact.UpdatedAt.Add(time.Second)
	if err := fixture.db.Create(&nextFact).Error; err != nil {
		t.Fatalf("seed replacement Generation: %v", err)
	}
	err := fixture.reconciler.targets.verifyKubernetesSandboxAllocationIdentity(
		context.Background(), fixture.targetID,
		kubernetesTargetConfiguration{AllocationBackend: "sandbox-operator-standard", Namespace: allocation.Namespace},
		verifiedPod,
		kubernetesVerifiedWorkerIdentity{WorkerMode: kubernetesWorkerModeExecutionPinned, AssignedExecutionID: &assignedExecutionID},
	)
	assertProblemCode(t, err, 409, "kubernetes_sandbox_allocation_generation_stale")
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("id = ?", fixture.executionIDs[0]).Update("status", "cancelled").Error; err != nil {
		t.Fatalf("terminalize execution: %v", err)
	}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("request exact Claim cleanup: %v", err)
	}
	if len(client.deleted) != 1 || client.deleted[0] != allocation.ClaimName {
		t.Fatalf("deleted claims = %#v, want %q", client.deleted, allocation.ClaimName)
	}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("wait for Sandbox lineage cleanup: %v", err)
	}
	if err := fixture.db.Where("execution_id = ?", fixture.executionIDs[0]).Take(&allocation).Error; err != nil {
		t.Fatalf("reload deleting allocation: %v", err)
	}
	if allocation.Status != "deleting" || allocation.DeletedAt != nil {
		t.Fatalf("allocation completed before Sandbox lineage disappeared: %#v", allocation)
	}
	delete(client.sandboxes, *allocation.SandboxName)
	delete(client.pods, *allocation.PodName)
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("observe exact Claim cleanup: %v", err)
	}
	if err := fixture.db.Where("execution_id = ?", fixture.executionIDs[0]).Take(&allocation).Error; err != nil {
		t.Fatalf("reload deleted allocation: %v", err)
	}
	if allocation.Status != "deleted" || allocation.DeletedAt == nil {
		t.Fatalf("deleted allocation = %#v", allocation)
	}
}

func TestKubernetesSandboxAllocationCleanupIgnoresReusedLineageNames(t *testing.T) {
	client := newFakeSandboxClient("standard")
	sandboxName, sandboxUID := "sandbox-reused", "sandbox-old-uid"
	podName, podUID := "sandbox-reused-pod", "pod-old-uid"
	allocation := persistence.ExecutionKubernetesAllocation{
		ExecutionTargetID: uuid.New(), Namespace: "synara-workers",
		SandboxName: &sandboxName, SandboxUID: &sandboxUID,
		PodName: &podName, PodUID: &podUID,
	}
	client.sandboxes[sandboxName] = kubernetesSandboxObservation{
		UID: "sandbox-new-uid", PodName: podName,
	}
	client.pods[podName] = kubernetesPod{Name: podName, UID: "pod-new-uid"}

	present, err := kubernetesSandboxAllocationLineagePresent(context.Background(), client, allocation)
	if err != nil {
		t.Fatalf("observe reused lineage names: %v", err)
	}
	if present {
		t.Fatal("new Kubernetes UIDs with reused names were mistaken for the deleted allocation lineage")
	}

	client.sandboxes[sandboxName] = kubernetesSandboxObservation{UID: sandboxUID, PodName: podName}
	present, err = kubernetesSandboxAllocationLineagePresent(context.Background(), client, allocation)
	if err != nil {
		t.Fatalf("observe surviving Sandbox UID: %v", err)
	}
	if !present {
		t.Fatal("the persisted Sandbox UID was not retained as live cleanup lineage")
	}
}

func TestKubernetesSandboxClaimMaterializerRejectsClaimConfigurationDrift(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t)
	configuration := kubernetesTestConfiguration("")
	configuration["allocationBackend"] = "sandbox-operator-standard"
	configuration["sandboxTemplateName"] = "synara-worker"
	configuration["sandboxWarmPoolName"] = "synara-interactive"
	fixture.updateConfiguration(t, configuration)
	seedKubernetesAllocationGenerationFacts(t, fixture)

	client := newFakeSandboxClient("standard")
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("materialize SandboxClaim: %v", err)
	}
	var allocation persistence.ExecutionKubernetesAllocation
	if err := fixture.db.Where("execution_id = ?", fixture.executionIDs[0]).Take(&allocation).Error; err != nil {
		t.Fatalf("load allocation: %v", err)
	}
	claim := client.claims[allocation.ClaimName]
	claim.ConfigurationDigest = "externally-replaced"
	client.claims[allocation.ClaimName] = claim

	err := fixture.reconciler.ReconcileOnce(context.Background())
	assertProblemCode(t, err, 409, "kubernetes_sandbox_claim_drift")
}

func TestKubernetesSandboxClaimMaterializerRejectsReleaseTemplateMismatch(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t)
	configuration := kubernetesTestConfiguration("")
	configuration["allocationBackend"] = "sandbox-operator-standard"
	configuration["sandboxTemplateName"] = "synara-worker"
	configuration["sandboxWarmPoolName"] = "synara-interactive"
	fixture.updateConfiguration(t, configuration)
	seedKubernetesAllocationGenerationFacts(t, fixture)
	cancelAdditionalSandboxExecutions(t, fixture)

	digest := "sha256:" + strings.Repeat("a", 64)
	revisionID := fixture.seedReleaseRevision(t, 1, digest)
	channel := "promoted"
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("id = ?", fixture.executionIDs[0]).
		Updates(map[string]any{"worker_release_revision_id": revisionID, "worker_release_channel": channel}).Error; err != nil {
		t.Fatal(err)
	}
	client := newFakeSandboxClient("standard")
	client.acceptance.TemplateAgentdImage = "synara-agentd:test"
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	err := fixture.reconciler.ReconcileOnce(context.Background())
	assertProblemCode(t, err, 503, "kubernetes_sandbox_worker_release_template_mismatch")
	if len(client.claims) != 0 {
		t.Fatalf("release-mismatched template materialized Claims: %#v", client.claims)
	}
}

func TestKubernetesSandboxClaimMaterializerRejectsSinglePoolCanary(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t)
	configuration := kubernetesTestConfiguration("")
	configuration["allocationBackend"] = "sandbox-operator-standard"
	configuration["sandboxTemplateName"] = "synara-worker"
	configuration["sandboxWarmPoolName"] = "synara-interactive"
	fixture.updateConfiguration(t, configuration)
	seedKubernetesAllocationGenerationFacts(t, fixture)
	cancelAdditionalSandboxExecutions(t, fixture)

	digest := "sha256:" + strings.Repeat("b", 64)
	revisionID := fixture.seedReleaseRevision(t, 2, digest)
	channel := "canary"
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("id = ?", fixture.executionIDs[0]).
		Updates(map[string]any{"worker_release_revision_id": revisionID, "worker_release_channel": channel}).Error; err != nil {
		t.Fatal(err)
	}
	client := newFakeSandboxClient("standard")
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	err := fixture.reconciler.ReconcileOnce(context.Background())
	assertProblemCode(t, err, 409, "kubernetes_sandbox_canary_release_unsupported")
	if len(client.claims) != 0 {
		t.Fatalf("single-pool canary materialized Claims: %#v", client.claims)
	}
}

func TestKubernetesSandboxClaimMaterializerWaitsForRecreateRollout(t *testing.T) {
	tests := []struct {
		name       string
		strategy   string
		imageFresh bool
	}{
		{name: "legacy update strategy", strategy: "OnReplenish", imageFresh: true},
		{name: "stale warm pool member", strategy: "Recreate", imageFresh: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newKubernetesReconcileFixture(t)
			configuration := kubernetesTestConfiguration("")
			configuration["allocationBackend"] = "sandbox-operator-standard"
			configuration["sandboxTemplateName"] = "synara-worker"
			configuration["sandboxWarmPoolName"] = "synara-interactive"
			fixture.updateConfiguration(t, configuration)
			seedKubernetesAllocationGenerationFacts(t, fixture)

			client := newFakeSandboxClient("standard")
			client.acceptance.WarmPoolUpdateStrategy = test.strategy
			client.acceptance.WarmPoolTemplateImageFresh = test.imageFresh
			fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
			err := fixture.reconciler.ReconcileOnce(context.Background())
			assertProblemCode(t, err, 503, "kubernetes_sandbox_warm_pool_rollout_pending")
			if len(client.claims) != 0 {
				t.Fatalf("rollout-pending pool materialized Claims: %#v", client.claims)
			}
		})
	}
}

func TestKubernetesSandboxClaimMaterializerDeletesReleaseMismatchedPod(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t)
	configuration := kubernetesTestConfiguration("")
	configuration["allocationBackend"] = "sandbox-operator-standard"
	configuration["sandboxTemplateName"] = "synara-worker"
	configuration["sandboxWarmPoolName"] = "synara-interactive"
	fixture.updateConfiguration(t, configuration)
	seedKubernetesAllocationGenerationFacts(t, fixture)
	cancelAdditionalSandboxExecutions(t, fixture)

	digest := "sha256:" + strings.Repeat("c", 64)
	revisionID := fixture.seedReleaseRevision(t, 3, digest)
	channel := "promoted"
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("id = ?", fixture.executionIDs[0]).
		Updates(map[string]any{"worker_release_revision_id": revisionID, "worker_release_channel": channel}).Error; err != nil {
		t.Fatal(err)
	}
	expectedImage, err := pinImageReference("synara-agentd:test", digest)
	if err != nil {
		t.Fatal(err)
	}
	client := newFakeSandboxClient("standard")
	client.acceptance.TemplateAgentdImage = expectedImage
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("materialize release Claim: %v", err)
	}
	var allocation persistence.ExecutionKubernetesAllocation
	if err := fixture.db.Where("execution_id = ?", fixture.executionIDs[0]).Take(&allocation).Error; err != nil {
		t.Fatal(err)
	}
	claim := client.claims[allocation.ClaimName]
	claim.Ready = true
	claim.SandboxName = "release-sandbox"
	client.claims[allocation.ClaimName] = claim
	client.sandboxes[claim.SandboxName] = kubernetesSandboxObservation{UID: uuid.NewString(), PodName: "release-pod"}
	client.pods["release-pod"] = kubernetesPod{
		Name: "release-pod", UID: uuid.NewString(), AgentdImage: "synara-agentd:old", Phase: "Running",
		Labels: map[string]string{kubernetesTargetLabel: fixture.targetID.String()},
	}
	err = fixture.reconciler.ReconcileOnce(context.Background())
	assertProblemCode(t, err, 409, "kubernetes_sandbox_worker_release_pod_mismatch")
	if len(client.deleted) != 1 || client.deleted[0] != allocation.ClaimName {
		t.Fatalf("release-mismatched Claim cleanup = %#v", client.deleted)
	}
}

func TestKubernetesSandboxClaimMaterializerRestartPreservesConcurrentGenerationIdentity(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t)
	configuration := kubernetesTestConfiguration("")
	configuration["allocationBackend"] = "sandbox-operator-standard"
	configuration["sandboxTemplateName"] = "synara-worker"
	configuration["sandboxWarmPoolName"] = "synara-interactive"
	configuration["maxActivePods"] = 2
	fixture.updateConfiguration(t, configuration)
	seedKubernetesAllocationGenerationFacts(t, fixture)
	client := newFakeSandboxClient("standard")
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("materialize concurrent Claims before restart: %v", err)
	}
	if len(client.claims) != 2 {
		t.Fatalf("Claims before restart = %d, want 2", len(client.claims))
	}
	var before []persistence.ExecutionKubernetesAllocation
	if err := fixture.db.Order("execution_id").Find(&before).Error; err != nil {
		t.Fatal(err)
	}
	if len(before) != 2 {
		t.Fatalf("persisted allocations before restart = %d, want 2", len(before))
	}

	restarted := NewKubernetesReconciler(fixture.reconciler.targets, fixture.reconciler.config, fixture.reconciler.logger)
	restarted.factory = &fakeKubernetesFactory{client: client}
	for index, allocation := range before {
		claim := client.claims[allocation.ClaimName]
		claim.Ready = true
		claim.SandboxName = fmt.Sprintf("restart-sandbox-%d", index)
		client.claims[allocation.ClaimName] = claim
		podName := fmt.Sprintf("restart-pod-%d", index)
		client.sandboxes[claim.SandboxName] = kubernetesSandboxObservation{UID: uuid.NewString(), PodName: podName}
		client.pods[podName] = kubernetesPod{
			Name: podName, UID: uuid.NewString(), AgentdImage: "synara-agentd:test", Phase: "Running",
			Labels: map[string]string{kubernetesTargetLabel: fixture.targetID.String()},
		}
	}
	if err := restarted.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("bind concurrent Claims after restart: %v", err)
	}
	var after []persistence.ExecutionKubernetesAllocation
	if err := fixture.db.Order("execution_id").Find(&after).Error; err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("restart duplicated allocations: before=%d after=%d", len(before), len(after))
	}
	claimUIDs := map[string]struct{}{}
	podUIDs := map[string]struct{}{}
	for index, allocation := range after {
		if allocation.Status != "bound" || allocation.ClaimUID == nil || allocation.PodUID == nil ||
			allocation.ExecutionID != before[index].ExecutionID || allocation.ClaimName != before[index].ClaimName {
			t.Fatalf("allocation identity changed across restart: before=%#v after=%#v", before[index], allocation)
		}
		claimUIDs[*allocation.ClaimUID] = struct{}{}
		podUIDs[*allocation.PodUID] = struct{}{}
	}
	if len(claimUIDs) != 2 || len(podUIDs) != 2 {
		t.Fatalf("restart reused physical identities: claims=%#v pods=%#v", claimUIDs, podUIDs)
	}
}

func TestKubernetesSandboxClaimMaterializerRetainsLeasedGenerationAllocation(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t)
	configuration := kubernetesTestConfiguration("")
	configuration["allocationBackend"] = "sandbox-operator-standard"
	configuration["sandboxTemplateName"] = "synara-worker"
	configuration["sandboxWarmPoolName"] = "synara-interactive"
	fixture.updateConfiguration(t, configuration)
	seedKubernetesAllocationGenerationFacts(t, fixture)
	cancelAdditionalSandboxExecutions(t, fixture)

	client := newFakeSandboxClient("standard")
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("materialize queued Generation: %v", err)
	}
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("id = ?", fixture.executionIDs[0]).
		Updates(map[string]any{"status": "leased", "generation": 0}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("retain leased Generation allocation: %v", err)
	}
	var allocation persistence.ExecutionKubernetesAllocation
	if err := fixture.db.Where("execution_id = ? AND generation = ?", fixture.executionIDs[0], 1).Take(&allocation).Error; err != nil {
		t.Fatal(err)
	}
	if allocation.Status != "materializing" || len(client.deleted) != 0 {
		t.Fatalf("leased Generation allocation was cleaned: allocation=%#v deleted=%#v", allocation, client.deleted)
	}
}

func cancelAdditionalSandboxExecutions(t *testing.T, fixture kubernetesReconcileFixture) {
	t.Helper()
	if len(fixture.executionIDs) < 2 {
		return
	}
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("id IN ?", fixture.executionIDs[1:]).Update("status", "cancelled").Error; err != nil {
		t.Fatal(err)
	}
}

func TestKubernetesSandboxClaimMaterializerTimesOutAndDeletesExactClaim(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t)
	configuration := kubernetesTestConfiguration("")
	configuration["allocationBackend"] = "sandbox-operator-standard"
	configuration["sandboxTemplateName"] = "synara-worker"
	configuration["sandboxWarmPoolName"] = "synara-interactive"
	configuration["sandboxClaimReadyTimeoutSeconds"] = 1
	fixture.updateConfiguration(t, configuration)
	seedKubernetesAllocationGenerationFacts(t, fixture)

	now := time.Now().UTC()
	fixture.reconciler.now = func() time.Time { return now }
	client := newFakeSandboxClient("standard")
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("materialize SandboxClaim: %v", err)
	}
	var allocation persistence.ExecutionKubernetesAllocation
	if err := fixture.db.Where("execution_id = ?", fixture.executionIDs[0]).Take(&allocation).Error; err != nil {
		t.Fatalf("load allocation: %v", err)
	}
	expectedUID := client.claims[allocation.ClaimName].UID
	now = now.Add(2 * time.Second)
	err := fixture.reconciler.ReconcileOnce(context.Background())
	assertProblemCode(t, err, 504, "kubernetes_sandbox_claim_ready_timeout")
	if len(client.deleted) != 1 || client.deleted[0] != allocation.ClaimName {
		t.Fatalf("deleted claims = %#v, want exact claim %q (%s)", client.deleted, allocation.ClaimName, expectedUID)
	}
	if err := fixture.db.Where("execution_id = ?", fixture.executionIDs[0]).Take(&allocation).Error; err != nil {
		t.Fatalf("reload timed-out allocation: %v", err)
	}
	if allocation.Status != "deleting" || allocation.ClaimUID == nil || *allocation.ClaimUID != expectedUID || allocation.DeleteRequestedAt == nil {
		t.Fatalf("timed-out allocation = %#v", allocation)
	}
}

func TestKubernetesSandboxClaimMaterializerFailsFastOnTerminalRejection(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t)
	configuration := kubernetesTestConfiguration("")
	configuration["allocationBackend"] = "sandbox-operator-standard"
	configuration["sandboxTemplateName"] = "synara-worker"
	configuration["sandboxWarmPoolName"] = "synara-interactive"
	fixture.updateConfiguration(t, configuration)
	seedKubernetesAllocationGenerationFacts(t, fixture)
	client := newFakeSandboxClient("standard")
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("materialize SandboxClaim: %v", err)
	}
	var allocation persistence.ExecutionKubernetesAllocation
	if err := fixture.db.Where("execution_id = ?", fixture.executionIDs[0]).Take(&allocation).Error; err != nil {
		t.Fatal(err)
	}
	claim := client.claims[allocation.ClaimName]
	claim.Reason = "InvalidMetadata"
	client.claims[allocation.ClaimName] = claim
	err := fixture.reconciler.ReconcileOnce(context.Background())
	assertProblemCode(t, err, 409, "kubernetes_sandbox_claim_rejected")
	if len(client.deleted) != 1 || client.deleted[0] != allocation.ClaimName {
		t.Fatalf("terminally rejected Claim cleanup = %#v", client.deleted)
	}
}

func TestKubernetesSandboxDesiredWarmReplicasProjectsSynaraAuthority(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t)
	fixture.createWarmPool(t, "interactive", 2, 5, "active")
	executions, err := fixture.reconciler.loadKubernetesExecutions(context.Background(), fixture.targetID)
	if err != nil {
		t.Fatal(err)
	}
	replicas, err := fixture.reconciler.kubernetesSandboxDesiredWarmReplicas(
		context.Background(), fixture.targetID,
		kubernetesTargetConfiguration{MaxActivePods: 3}, executions, time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if replicas != 1 {
		t.Fatalf("SandboxWarmPool replicas = %d, want one idle unit after two claims reserve target headroom", replicas)
	}
}

func TestKubernetesAllocationBackendTransitionCleansSandboxBeforeNative(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t)
	configuration := kubernetesTestConfiguration("")
	configuration["allocationBackend"] = "sandbox-operator-standard"
	configuration["sandboxTemplateName"] = "synara-worker"
	configuration["sandboxWarmPoolName"] = "synara-interactive"
	fixture.updateConfiguration(t, configuration)
	seedKubernetesAllocationGenerationFacts(t, fixture)
	client := newFakeSandboxClient("standard")
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("materialize SandboxClaims: %v", err)
	}
	var target persistence.ExecutionTarget
	if err := fixture.db.First(&target, "id = ?", fixture.targetID).Error; err != nil {
		t.Fatal(err)
	}
	native := kubernetesTargetConfiguration{AllocationBackend: "native-pod", Namespace: "synara-workers"}
	err := fixture.reconciler.fenceKubernetesAllocationBackendTransition(context.Background(), client, target, native)
	assertProblemCode(t, err, 409, "kubernetes_allocation_backend_transition_pending")
	err = fixture.reconciler.fenceKubernetesAllocationBackendTransition(context.Background(), client, target, native)
	assertProblemCode(t, err, 409, "kubernetes_allocation_backend_transition_observed")
	if err := fixture.reconciler.fenceKubernetesAllocationBackendTransition(context.Background(), client, target, native); err != nil {
		t.Fatalf("allow native backend after exact cleanup: %v", err)
	}
	var remaining int64
	if err := fixture.db.Model(&persistence.ExecutionKubernetesAllocation{}).
		Where("execution_target_id = ? AND status <> ?", fixture.targetID, "deleted").Count(&remaining).Error; err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("remaining Sandbox allocations = %d, want 0", remaining)
	}
}

func TestKubernetesSandboxBackendTransitionIgnoresOperatorOwnedWarmPods(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t)
	client := newFakeSandboxClient("standard")
	client.pods["warm-sandbox-pod"] = kubernetesPod{
		Name: "warm-sandbox-pod", UID: uuid.NewString(),
		ControllerOwnerKind: "Sandbox", ControllerOwnerUID: uuid.NewString(),
		Labels: map[string]string{kubernetesTargetLabel: fixture.targetID.String()},
	}
	var target persistence.ExecutionTarget
	if err := fixture.db.First(&target, "id = ?", fixture.targetID).Error; err != nil {
		t.Fatal(err)
	}
	configuration := kubernetesTargetConfiguration{
		AllocationBackend: "sandbox-operator-standard", Namespace: "synara-test",
	}
	if err := fixture.reconciler.fenceKubernetesAllocationBackendTransition(
		context.Background(), client, target, configuration,
	); err != nil {
		t.Fatalf("operator-owned warm Pod blocked Sandbox backend: %v", err)
	}
	client.pods["native-pod"] = kubernetesPod{
		Name: "native-pod", UID: uuid.NewString(),
		Labels: map[string]string{kubernetesTargetLabel: fixture.targetID.String()},
	}
	err := fixture.reconciler.fenceKubernetesAllocationBackendTransition(
		context.Background(), client, target, configuration,
	)
	assertProblemCode(t, err, 409, "kubernetes_allocation_backend_native_pods_present")
}

func seedKubernetesAllocationGenerationFacts(t *testing.T, fixture kubernetesReconcileFixture) {
	t.Helper()
	for index, executionID := range fixture.executionIDs {
		var execution persistence.AgentExecution
		if err := fixture.db.First(&execution, "id = ?", executionID).Error; err != nil {
			t.Fatal(err)
		}
		now := time.Now().UTC().Add(time.Duration(index) * time.Millisecond)
		fact := persistence.ExecutionGenerationFact{
			TenantID: fixture.tenantID, ExecutionID: executionID, Generation: 1,
			SessionID: execution.SessionID, TurnID: execution.TurnID, ExecutionTargetID: fixture.targetID,
			TargetKind: "kubernetes", Provider: "codex", RecoveryReason: "initial-claim",
			WarmPoolMode: "disabled", WarmPoolResult: "not-requested", DispatchRequestedAt: &now,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := fixture.db.Create(&fact).Error; err != nil {
			t.Fatalf("seed Generation fact: %v", err)
		}
	}
}
