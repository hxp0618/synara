package executiontargets

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/placement"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestOrderKubernetesExecutionsForServiceUsesTenantEqualShare(t *testing.T) {
	base := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	tenantA := uuid.MustParse("00000000-0000-0000-0000-00000000000a")
	tenantB := uuid.MustParse("00000000-0000-0000-0000-00000000000b")
	a1 := kubernetesExecution{
		ID:       uuid.MustParse("10000000-0000-0000-0000-000000000001"),
		TenantID: tenantA, Status: "queued", QueuedAt: base,
	}
	b1 := kubernetesExecution{
		ID:       uuid.MustParse("20000000-0000-0000-0000-000000000001"),
		TenantID: tenantB, Status: "queued", QueuedAt: base.Add(time.Minute),
	}
	b2 := kubernetesExecution{
		ID:       uuid.MustParse("20000000-0000-0000-0000-000000000002"),
		TenantID: tenantB, Status: "recovering", QueuedAt: base.Add(2 * time.Minute),
	}
	active := []kubernetesExecution{
		{ID: uuid.New(), TenantID: tenantA, Status: "leased"},
		{ID: uuid.New(), TenantID: tenantA, Status: "waiting-for-approval"},
	}
	items := append(append([]kubernetesExecution{}, active...), a1, b2, b1)

	ordered, err := orderKubernetesExecutionsForService(items, base.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(ordered) != len(items) || ordered[0].ID != active[0].ID || ordered[1].ID != active[1].ID ||
		ordered[2].ID != b1.ID || ordered[3].ID != b2.ID || ordered[4].ID != a1.ID {
		t.Fatalf("active-aware Kubernetes fair queue = %#v", ordered)
	}

	invalid := append([]kubernetesExecution{}, items...)
	invalid = append(invalid, kubernetesExecution{ID: uuid.New(), TenantID: tenantA, Status: "completed"})
	if _, err := orderKubernetesExecutionsForService(invalid, base.Add(time.Minute)); err == nil {
		t.Fatal("Kubernetes fair queue accepted an unexpected execution status")
	}
}

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
	namespace := client.lastKind("Namespace")
	namespaceLabels := namespace["metadata"].(map[string]any)["labels"].(map[string]string)
	if namespaceLabels["pod-security.kubernetes.io/enforce"] != "restricted" ||
		namespaceLabels["pod-security.kubernetes.io/audit"] != "restricted" ||
		namespaceLabels["pod-security.kubernetes.io/warn"] != "restricted" {
		t.Fatalf("Kubernetes managed Namespace did not enforce restricted Pod Security labels: %#v", namespaceLabels)
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
		spec["terminationGracePeriodSeconds"] != 30 || spec["enableServiceLinks"] != false ||
		spec["hostNetwork"] != false || spec["hostPID"] != false || spec["hostIPC"] != false ||
		spec["priorityClassName"] != kubernetesWorkerDefaultPriorityClassName ||
		spec["preemptionPolicy"] != placement.KubernetesPreemptionPolicyNever {
		t.Fatalf("Kubernetes Pod runtime policy is unsafe: %#v", spec)
	}
	container := spec["containers"].([]any)[0].(map[string]any)
	assertKubernetesContainerResourceLimits(t, container)
	if value, found := kubernetesEnvironmentValue(container, "SYNARA_AGENTD_WORKSPACE_ROOT"); !found || value != "/data/workspaces" {
		t.Fatalf("Kubernetes workspace root environment is %q", value)
	}
	if value, found := kubernetesEnvironmentValue(container, "SYNARA_AGENTD_GIT_CACHE_ROOT"); !found || value != "/data/git-cache" {
		t.Fatalf("Kubernetes default Git cache root environment is %q", value)
	}
	if value, found := kubernetesEnvironmentValue(container, "SYNARA_AGENTD_PRIVATE_TMP_ROOT"); !found || value != "/tmp" {
		t.Fatalf("Kubernetes Worker-private temporary root environment is %q", value)
	}
	if fieldPath, found := kubernetesEnvironmentFieldPath(container, "SYNARA_AGENTD_INSTANCE_UID"); !found || fieldPath != "metadata.uid" {
		t.Fatalf("Kubernetes Pod UID environment uses %q", fieldPath)
	}
	if value, found := kubernetesEnvironmentValue(container, "SYNARA_AGENTD_VERSION"); found {
		t.Fatalf("Kubernetes Worker overrides the immutable image version with %q", value)
	}
	volumes := spec["volumes"].([]any)
	workspaceVolume := kubernetesNamedObject(volumes, "workspace")
	if workspaceVolume == nil || workspaceVolume["emptyDir"] == nil {
		t.Fatalf("Kubernetes default workspace volume is not emptyDir: %#v", volumes)
	}
	if kubernetesNamedObject(volumes, "git-cache") != nil {
		t.Fatalf("Kubernetes default Pod created a redundant Git cache volume: %#v", volumes)
	}
	identityVolume := kubernetesNamedObject(volumes, kubernetesWorkloadIdentityVolume)
	identityJSON, _ := json.Marshal(identityVolume)
	if identityVolume == nil || !bytes.Contains(identityJSON, []byte(KubernetesWorkerRegistrationAudience(fixture.targetID))) ||
		!bytes.Contains(identityJSON, []byte(`"expirationSeconds":600`)) {
		t.Fatalf("Kubernetes Pod-bound identity projection is invalid: %s", identityJSON)
	}
	stagedTokenVolume := kubernetesNamedObject(volumes, kubernetesRegistrationTokenVolume)
	if stagedTokenVolume == nil || stagedTokenVolume["emptyDir"] == nil {
		t.Fatalf("Kubernetes one-shot registration token volume is invalid: %#v", stagedTokenVolume)
	}
	volumeMounts := container["volumeMounts"].([]any)
	workspaceMount := kubernetesNamedObject(volumeMounts, "workspace")
	if workspaceMount == nil || workspaceMount["mountPath"] != "/data" {
		t.Fatalf("Kubernetes workspace mount is invalid: %#v", volumeMounts)
	}
	if kubernetesNamedObject(volumeMounts, "git-cache") != nil {
		t.Fatalf("Kubernetes default Pod created a redundant Git cache mount: %#v", volumeMounts)
	}
	if identityMount := kubernetesNamedObject(volumeMounts, kubernetesWorkloadIdentityVolume); identityMount != nil {
		t.Fatalf("Kubernetes main container exposes the projected Pod identity: %#v", identityMount)
	}
	stagedTokenMount := kubernetesNamedObject(volumeMounts, kubernetesRegistrationTokenVolume)
	if stagedTokenMount == nil || stagedTokenMount["mountPath"] != "/var/run/secrets/synara.io/registration" {
		t.Fatalf("Kubernetes staged registration token mount is invalid: %#v", stagedTokenMount)
	}
	initContainers := spec["initContainers"].([]any)
	if len(initContainers) != 2 {
		t.Fatalf("Kubernetes Pod boundary init containers = %#v", initContainers)
	}
	networkInit := initContainers[0].(map[string]any)
	if networkInit["name"] != kubernetesNetworkBoundaryInitName ||
		!reflect.DeepEqual(networkInit["command"], []any{
			"/usr/local/bin/synara-agentd", platform.KubernetesNetworkBoundaryVerifyArgument,
		}) {
		t.Fatalf("Kubernetes network boundary init container is invalid: %#v", networkInit)
	}
	initContainer := initContainers[1].(map[string]any)
	if initContainer["name"] != kubernetesRegistrationTokenInitName {
		t.Fatalf("Kubernetes registration token init container is invalid: %#v", initContainer)
	}
	if !reflect.DeepEqual(networkInit["securityContext"], initContainer["securityContext"]) {
		t.Fatalf("Kubernetes network boundary init security is invalid: %#v", networkInit)
	}
	initMounts := initContainer["volumeMounts"].([]any)
	identityMount := kubernetesNamedObject(initMounts, kubernetesWorkloadIdentityVolume)
	if identityMount == nil || identityMount["readOnly"] != true {
		t.Fatalf("Kubernetes init Pod-bound identity mount is invalid: %#v", identityMount)
	}
	securityContext := container["securityContext"].(map[string]any)
	if securityContext["runAsNonRoot"] != true || securityContext["readOnlyRootFilesystem"] != true ||
		securityContext["allowPrivilegeEscalation"] != false {
		t.Fatalf("Kubernetes container security context is incomplete: %#v", securityContext)
	}
	seccompProfile, ok := securityContext["seccompProfile"].(map[string]any)
	if !ok || seccompProfile["type"] != "RuntimeDefault" {
		t.Fatalf("Kubernetes container seccomp profile is invalid: %#v", securityContext["seccompProfile"])
	}
	environment, err := json.Marshal(container["env"])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(environment, []byte("kubernetes-registration-secret")) ||
		bytes.Contains(environment, []byte("secretKeyRef")) ||
		!bytes.Contains(environment, []byte("SYNARA_WORKER_REGISTRATION_TOKEN_FILE")) ||
		!bytes.Contains(environment, []byte(kubernetesStagedRegistrationTokenPath)) ||
		!bytes.Contains(environment, []byte("SYNARA_AGENTD_ASSIGNED_EXECUTION_ID")) ||
		!bytes.Contains(environment, []byte("SYNARA_AGENTD_PROVIDER_HOST_PROTOCOL")) ||
		!bytes.Contains(environment, []byte(`"name":"SYNARA_AGENTD_LEASE_RENEW_INTERVAL","value":"2s"`)) ||
		!bytes.Contains(environment, []byte("SYNARA_AGENTD_DRAIN_TIMEOUT")) {
		t.Fatalf("Kubernetes Pod secret/assignment environment is invalid: %s", environment)
	}
	secret := client.lastKind("Secret")
	secretJSON, _ := json.Marshal(secret)
	if bytes.Contains(secretJSON, []byte("kubernetes-registration-secret")) ||
		client.namedKind("Secret", kubernetesSecretName(fixture.targetID)) != nil {
		t.Fatal("Kubernetes reconciliation persisted the shared Worker registration token")
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
	if len(client.deletedPods) != 1 || client.kindCount("Pod") != 1 {
		t.Fatalf("Kubernetes scheduler created a replacement before the deleted Pod released quota: deleted=%#v pods=%d", client.deletedPods, client.kindCount("Pod"))
	}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.kindCount("Pod") != 2 {
		t.Fatalf("Kubernetes scheduler did not create the next Pod after deletion settled: pods=%d", client.kindCount("Pod"))
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
	if len(audits) != 3 {
		t.Fatalf("expected three material Kubernetes reconciliation audits, got %d", len(audits))
	}
	encodedAudits, _ := json.Marshal(audits)
	if bytes.Contains(encodedAudits, []byte("kubernetes-registration-secret")) || bytes.Contains(encodedAudits, []byte("kubernetes-api-token")) {
		t.Fatalf("Kubernetes secrets leaked into Audit metadata: %s", encodedAudits)
	}
}

func TestKubernetesReconcilerDeletesSuspendedExecutionPodWithoutReplacement(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.pods) != 1 {
		t.Fatalf("initial reconciliation Pods = %d, want 1", len(client.pods))
	}
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("id = ?", fixture.executionIDs[0]).Updates(map[string]any{
		"status": "suspended", "worker_id": nil, "next_recovery_reason": "suspend-resume",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("id = ?", fixture.executionIDs[1]).Updates(map[string]any{
		"status": "completed", "finished_at": time.Now().UTC(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.deletedPods) != 1 || len(client.pods) != 0 {
		t.Fatalf("suspended execution Pod was not deleted exactly once: deleted=%#v active=%#v", client.deletedPods, client.pods)
	}
	createdPods := client.kindCount("Pod")
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.pods) != 0 || client.kindCount("Pod") != createdPods {
		t.Fatalf("suspended execution created a replacement Pod: active=%#v createdBefore=%d createdAfter=%d", client.pods, createdPods, client.kindCount("Pod"))
	}
}

func TestKubernetesReconcilerRetainsTerminalPodUntilSuspendFinalizationCommits(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.pods) != 1 {
		t.Fatalf("initial reconciliation Pods = %d, want 1", len(client.pods))
	}

	var podName string
	var pod kubernetesPod
	for podName, pod = range client.pods {
		break
	}
	pod.Phase = "Succeeded"
	pod.Containers = []kubernetesContainerStatus{{Name: "agentd", Terminated: true, ExitCode: 0}}
	client.pods[podName] = pod

	finalized := false
	finalizeCalls := 0
	fixture.reconciler.config.FinalizeResourceSuspend = func(
		_ context.Context,
		observation KubernetesPodTerminalObservation,
	) (bool, error) {
		finalizeCalls++
		if observation.ExecutionTargetID != fixture.targetID || observation.ExecutionID != fixture.executionIDs[0] ||
			observation.Generation != 1 || observation.PodName != podName || observation.PodUID != pod.UID ||
			observation.Phase != "Succeeded" {
			t.Fatalf("unexpected terminal observation: %#v", observation)
		}
		return finalized, nil
	}

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if finalizeCalls != 1 || len(client.deletedPods) != 0 {
		t.Fatalf("uncommitted suspend proof was deleted: finalizeCalls=%d deleted=%#v", finalizeCalls, client.deletedPods)
	}
	if _, found := client.pods[podName]; !found {
		t.Fatal("terminal Pod evidence was not retained for a finalization retry")
	}
	if len(client.pods) != 1 || client.kindCount("Pod") != 1 {
		t.Fatalf("reconciler created a replacement Pod while terminal proof remained pending: active=%#v created=%d", client.pods, client.kindCount("Pod"))
	}

	finalized = true
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if finalizeCalls != 2 || len(client.deletedPods) != 1 || client.deletedPods[0] != podName {
		t.Fatalf("durably finalized terminal Pod was not deleted exactly once: finalizeCalls=%d deleted=%#v", finalizeCalls, client.deletedPods)
	}
	if _, found := client.pods[podName]; found {
		t.Fatal("durably finalized terminal Pod was retained")
	}
}

func TestKubernetesReconcilerDoesNotCreatePodsForAbsoluteExpiredSessions(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	absoluteExpiry := time.Now().UTC().Add(time.Second)
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ?", fixture.tenantID).
		Update("absolute_expires_at", absoluteExpiry).Error; err != nil {
		t.Fatal(err)
	}
	fixture.reconciler.now = func() time.Time { return absoluteExpiry.Add(time.Millisecond) }

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.kindCount("Pod") != 0 || len(client.pods) != 0 {
		t.Fatalf("absolute-expired Sessions created Kubernetes Pods: created=%d active=%#v", client.kindCount("Pod"), client.pods)
	}
}

func TestKubernetesReconcilerDeletesExistingPodAtAbsoluteExpiry(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.pods) != 1 {
		t.Fatalf("initial reconciliation Pods = %d, want 1", len(client.pods))
	}
	absoluteExpiry := time.Now().UTC().Add(time.Second)
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ?", fixture.tenantID).
		Update("absolute_expires_at", absoluteExpiry).Error; err != nil {
		t.Fatal(err)
	}
	fixture.reconciler.now = func() time.Time { return absoluteExpiry.Add(time.Millisecond) }

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.deletedPods) != 1 || len(client.pods) != 0 {
		t.Fatalf("absolute-expired Pod was not deleted exactly once: deleted=%#v active=%#v", client.deletedPods, client.pods)
	}
}

func TestKubernetesReconcilerRechecksAbsoluteExpiryBeforePodApply(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	absoluteExpiry := time.Now().UTC().Add(time.Second)
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ?", fixture.tenantID).
		Update("absolute_expires_at", absoluteExpiry).Error; err != nil {
		t.Fatal(err)
	}
	preExpiry := absoluteExpiry.Add(-time.Millisecond)
	postExpiry := absoluteExpiry.Add(time.Millisecond)
	nowCalls := 0
	fixture.reconciler.now = func() time.Time {
		nowCalls++
		// The initial hash mismatch short-circuits the foundation comparison;
		// only the foundation timestamp and DB filter precede Pod Apply.
		if nowCalls <= 2 {
			return preExpiry
		}
		return postExpiry
	}

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if client.kindCount("Pod") != 0 || len(client.pods) != 0 {
		t.Fatalf("stale pre-expiry query created a post-expiry Pod: created=%d active=%#v", client.kindCount("Pod"), client.pods)
	}
}

func TestKubernetesReconcilerRechecksAbsoluteExpiryBeforeRetainingPod(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.pods) != 1 {
		t.Fatalf("initial reconciliation Pods = %d, want 1", len(client.pods))
	}
	absoluteExpiry := time.Now().UTC().Add(time.Second)
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ?", fixture.tenantID).
		Update("absolute_expires_at", absoluteExpiry).Error; err != nil {
		t.Fatal(err)
	}
	preExpiry := absoluteExpiry.Add(-time.Millisecond)
	postExpiry := absoluteExpiry.Add(time.Millisecond)
	nowCalls := 0
	fixture.reconciler.now = func() time.Time {
		nowCalls++
		// On a steady-state foundation, the DB filter is the second clock read;
		// the existing-Pod retain decision must use a fresh third read.
		if nowCalls <= 2 {
			return preExpiry
		}
		return postExpiry
	}

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.deletedPods) != 1 || len(client.pods) != 0 {
		t.Fatalf("stale pre-expiry query retained a post-expiry Pod: deleted=%#v active=%#v", client.deletedPods, client.pods)
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

func TestKubernetesReconcilerProjectsTenantNetworkPolicyAndProviderProxy(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	configuration := kubernetesTestConfiguration("")
	configuration["egressTcpPorts"] = []int{443, 8443}
	configuration["privateNetworkCidrs"] = []string{"10.20.0.0/16"}
	configuration["providerHttpProxy"] = "http://10.20.0.10:3128"
	configuration["providerHttpsProxy"] = "https://proxy.corp.example:4443"
	configuration["providerAllProxy"] = "socks5://10.20.0.11:1080"
	configuration["providerNoProxy"] = []string{"control-plane.test", ".corp.example"}
	fixture.updateConfiguration(t, configuration)
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	networkPolicy := client.lastKind("NetworkPolicy")
	spec := networkPolicy["spec"].(map[string]any)
	egress := spec["egress"].([]any)
	if len(egress) != 2 {
		t.Fatalf("Kubernetes NetworkPolicy egress rules = %#v, want DNS plus explicit TCP egress", egress)
	}
	dnsRule := egress[0].(map[string]any)
	dnsTo := dnsRule["to"].([]any)[0].(map[string]any)
	namespaceLabels := dnsTo["namespaceSelector"].(map[string]any)["matchLabels"].(map[string]any)
	podLabels := dnsTo["podSelector"].(map[string]any)["matchLabels"].(map[string]any)
	if namespaceLabels["kubernetes.io/metadata.name"] != "kube-system" || podLabels["k8s-app"] != "kube-dns" {
		t.Fatalf("Kubernetes DNS egress selector is not kube-system/kube-dns: %#v", dnsTo)
	}
	dnsPorts := dnsRule["ports"].([]any)
	if len(dnsPorts) != 2 || dnsPorts[0].(map[string]any)["protocol"] != "UDP" ||
		dnsPorts[0].(map[string]any)["port"] != 53 || dnsPorts[1].(map[string]any)["protocol"] != "TCP" ||
		dnsPorts[1].(map[string]any)["port"] != 53 {
		t.Fatalf("Kubernetes DNS egress ports = %#v", dnsPorts)
	}

	providerRule := egress[1].(map[string]any)
	ipBlock := providerRule["to"].([]any)[0].(map[string]any)["ipBlock"].(map[string]any)
	if ipBlock["cidr"] != "0.0.0.0/0" {
		t.Fatalf("Kubernetes Provider egress CIDR = %#v", ipBlock)
	}
	except, ok := ipBlock["except"].([]any)
	if !ok || !containsAnyString(except, "169.254.0.0/16") || !containsAnyString(except, "100.100.100.200/32") {
		t.Fatalf("Kubernetes Provider egress did not exclude metadata/link-local endpoints: %#v", ipBlock)
	}
	actualPorts := map[int]bool{}
	for _, raw := range providerRule["ports"].([]any) {
		port := raw.(map[string]any)
		if port["protocol"] != "TCP" {
			t.Fatalf("Kubernetes Provider egress emitted a non-TCP port: %#v", port)
		}
		actualPorts[port["port"].(int)] = true
	}
	for _, expected := range []int{443, 1080, 3128, 3780, 4443, 8443} {
		if !actualPorts[expected] {
			t.Fatalf("Kubernetes Provider egress omitted TCP port %d: %#v", expected, actualPorts)
		}
	}
	if actualPorts[22] {
		t.Fatalf("explicit Kubernetes egress ports unexpectedly retained the default SSH port: %#v", actualPorts)
	}

	pod := client.lastKind("Pod")
	container := pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	for name, expected := range map[string]string{
		"SYNARA_AGENTD_PRIVATE_NETWORK_CIDRS_JSON": `["10.20.0.0/16"]`,
		"SYNARA_PROVIDER_HTTP_PROXY":               "http://10.20.0.10:3128",
		"SYNARA_PROVIDER_HTTPS_PROXY":              "https://proxy.corp.example:4443",
		"SYNARA_PROVIDER_ALL_PROXY":                "socks5://10.20.0.11:1080",
		"SYNARA_PROVIDER_NO_PROXY":                 "control-plane.test,.corp.example",
	} {
		if value, found := kubernetesEnvironmentValue(container, name); !found || value != expected {
			t.Fatalf("Kubernetes Pod %s = %q, want %q", name, value, expected)
		}
	}
	for _, forbidden := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY"} {
		if _, found := kubernetesEnvironmentValue(container, forbidden); found {
			t.Fatalf("Kubernetes Pod received ambient proxy variable %s", forbidden)
		}
	}
}

func TestKubernetesNetworkConfigurationFailsClosed(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
		code   string
	}{
		{
			name: "metadata endpoint CIDR",
			mutate: func(configuration map[string]any) {
				configuration["egressCidrs"] = []string{"169.254.169.254/32"}
			},
			code: "invalid_kubernetes_egress_policy",
		},
		{
			name: "loopback private Git policy",
			mutate: func(configuration map[string]any) {
				configuration["privateNetworkCidrs"] = []string{"127.0.0.0/8"}
			},
			code: "invalid_kubernetes_private_network_policy",
		},
		{
			name: "private Git network outside egress policy",
			mutate: func(configuration map[string]any) {
				configuration["egressCidrs"] = []string{"192.0.2.0/24"}
				configuration["privateNetworkCidrs"] = []string{"10.20.0.0/16"}
			},
			code: "invalid_kubernetes_private_network_policy",
		},
		{
			name: "proxy URL with embedded credentials",
			mutate: func(configuration map[string]any) {
				configuration["providerHttpsProxy"] = "https://user:password@proxy.corp.example:8443"
			},
			code: "invalid_kubernetes_provider_proxy",
		},
		{
			name: "shared live Workspace PVC",
			mutate: func(configuration map[string]any) {
				configuration["workspacePersistentVolumeClaim"] = "shared-live-workspace"
			},
			code: "invalid_kubernetes_configuration",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newKubernetesReconcileFixture(t, "")
			configuration := kubernetesTestConfiguration("")
			test.mutate(configuration)
			fixture.updateConfiguration(t, configuration)
			fixture.reconciler.factory = &fakeKubernetesFactory{client: newFakeKubernetesClient()}
			err := fixture.reconciler.ReconcileOnce(context.Background())
			assertProblemCode(t, err, 400, test.code)
		})
	}
}

func TestKubernetesResourceLimitsFailClosedBeforeClusterMutation(t *testing.T) {
	for _, field := range []string{"cpuLimit", "memoryLimit", "ephemeralStorageLimit"} {
		for _, value := range []any{nil, "   "} {
			name := field + "-missing"
			if value != nil {
				name = field + "-blank"
			}
			t.Run(name, func(t *testing.T) {
				fixture := newKubernetesReconcileFixture(t, "")
				configuration := kubernetesTestConfiguration("")
				if value == nil {
					delete(configuration, field)
				} else {
					configuration[field] = value
				}
				fixture.updateConfiguration(t, configuration)
				client := newFakeKubernetesClient()
				fixture.reconciler.factory = &fakeKubernetesFactory{client: client}

				err := fixture.reconciler.ReconcileOnce(context.Background())
				assertProblemCode(t, err, 400, "kubernetes_resource_limits_required")
				if len(client.applied) != 0 {
					t.Fatalf("resource-limit-invalid configuration mutated Kubernetes: %#v", client.applied)
				}
			})
		}
	}
	for _, value := range []any{nil, 0} {
		name := "pidsLimit-missing"
		if value != nil {
			name = "pidsLimit-zero"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newKubernetesReconcileFixture(t, "")
			configuration := kubernetesTestConfiguration("")
			if value == nil {
				delete(configuration, "pidsLimit")
			} else {
				configuration["pidsLimit"] = value
			}
			fixture.updateConfiguration(t, configuration)
			client := newFakeKubernetesClient()
			fixture.reconciler.factory = &fakeKubernetesFactory{client: client}

			err := fixture.reconciler.ReconcileOnce(context.Background())
			assertProblemCode(t, err, 400, "kubernetes_resource_limits_required")
			if len(client.applied) != 0 {
				t.Fatalf("PID-limit-invalid configuration mutated Kubernetes: %#v", client.applied)
			}
		})
	}
}

func TestKubernetesNodePIDLimitAttestationFailsClosedBeforeClusterMutation(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	client := newFakeKubernetesClient()
	client.pidsLimitAttestationErr = errors.New("node worker-a podPidsLimit=-1")
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}

	err := fixture.reconciler.ReconcileOnce(context.Background())
	assertProblemCode(t, err, 503, "kubernetes_pids_limit_unverified")
	if len(client.applied) != 0 {
		t.Fatalf("PID-limit-unverified cluster was mutated: %#v", client.applied)
	}
}

func TestKubernetesSandboxTenantAllowlistFailsClosedBeforeClusterMutation(t *testing.T) {
	tests := []struct {
		name      string
		backend   string
		allowlist []string
		status    int
		code      string
	}{
		{
			name: "sandbox allowlist is empty", backend: "sandbox-operator-standard",
			allowlist: []string{}, status: 403, code: "kubernetes_sandbox_tenant_not_allowed",
		},
		{
			name: "sandbox allowlist contains another tenant", backend: "sandbox-operator-standard",
			allowlist: []string{uuid.NewString()}, status: 403, code: "kubernetes_sandbox_tenant_not_allowed",
		},
		{
			name: "native backend cannot carry sandbox allowlist", backend: "native-pod",
			allowlist: []string{uuid.NewString()}, status: 400, code: "invalid_kubernetes_allocation_backend_configuration",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newKubernetesReconcileFixture(t)
			configuration := kubernetesTestConfiguration("")
			configuration["allocationBackend"] = test.backend
			configuration["sandboxAllowedTenantIds"] = test.allowlist
			if test.backend != "native-pod" {
				configuration["sandboxTemplateName"] = "synara-worker"
				configuration["sandboxWarmPoolName"] = "synara-interactive"
			}
			fixture.updateConfiguration(t, configuration)
			client := newFakeKubernetesClient()
			fixture.reconciler.factory = &fakeKubernetesFactory{client: client}

			err := fixture.reconciler.ReconcileOnce(context.Background())
			assertProblemCode(t, err, test.status, test.code)
			if len(client.applied) != 0 {
				t.Fatalf("tenant-denied configuration mutated Kubernetes: %#v", client.applied)
			}
		})
	}
}

func TestKubernetesReconcilerRequiresNodeSpread(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	configuration := kubernetesTestConfiguration("")
	configuration["requireNodeSpread"] = true
	fixture.updateConfiguration(t, configuration)
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
	if _, found := spec["affinity"]; found {
		t.Fatalf("Kubernetes Pod unexpectedly used affinity instead of topology spread: %#v", spec["affinity"])
	}
	constraints, ok := spec["topologySpreadConstraints"].([]any)
	if !ok || len(constraints) != 1 {
		t.Fatalf("Kubernetes Pod topology spread constraints are invalid: %#v", spec["topologySpreadConstraints"])
	}
	constraint := constraints[0].(map[string]any)
	if constraint["maxSkew"] != 1 ||
		constraint["topologyKey"] != "kubernetes.io/hostname" ||
		constraint["whenUnsatisfiable"] != "DoNotSchedule" {
		t.Fatalf("Kubernetes Pod topology spread constraint policy is invalid: %#v", constraint)
	}
	labelSelector, ok := constraint["labelSelector"].(map[string]any)
	if !ok {
		t.Fatalf("Kubernetes Pod topology spread label selector is invalid: %#v", constraint["labelSelector"])
	}
	matchLabels, ok := labelSelector["matchLabels"].(map[string]any)
	if !ok || matchLabels[kubernetesTargetLabel] != fixture.targetID.String() {
		t.Fatalf("Kubernetes Pod topology spread target selector is invalid: %#v", labelSelector)
	}
}

func TestKubernetesReconcilerDefaultsToNoNodeSpreadConstraint(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
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
	if _, found := spec["topologySpreadConstraints"]; found {
		t.Fatalf("Kubernetes Pod unexpectedly emitted topology spread constraints by default: %#v", spec["topologySpreadConstraints"])
	}
}

func TestKubernetesReconcilerPinsExecutionReleaseImageAndUsesTargetPullCredential(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	configuration := kubernetesTestConfiguration("")
	configuration["image"] = "ghcr.io/synara/worker:mutable"
	fixture.updateConfiguration(t, configuration)
	firstDigest := "sha256:" + strings.Repeat("a", 64)
	secondDigest := "sha256:" + strings.Repeat("b", 64)
	firstRevision := fixture.seedReleaseRevision(t, 1, firstDigest)
	secondRevision := fixture.seedReleaseRevision(t, 2, secondDigest)
	channel := "promoted"
	if err := fixture.db.Model(&persistence.AgentExecution{}).Where("id = ?", fixture.executionIDs[0]).
		Updates(map[string]any{"worker_release_revision_id": firstRevision, "worker_release_channel": channel}).Error; err != nil {
		t.Fatal(err)
	}
	credential := &ImagePullCredential{
		BindingID: uuid.New(), CredentialID: uuid.New(), CredentialVersion: 4,
		Host: "ghcr.io", Username: "synara", Password: "registry-password-secret",
	}
	fixture.reconciler.config.ResolveImagePull = func(_ context.Context, _, _ uuid.UUID, selector string) (ImagePullCredentialResolution, error) {
		if selector != "ghcr.io" {
			t.Fatalf("Kubernetes image pull selector = %q", selector)
		}
		return ImagePullCredentialResolution{Credential: credential, Authoritative: true}, nil
	}
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	pod := client.lastKind("Pod")
	spec := pod["spec"].(map[string]any)
	if spec["priorityClassName"] != kubernetesWorkerDefaultPriorityClassName ||
		spec["preemptionPolicy"] != placement.KubernetesPreemptionPolicyNever {
		t.Fatalf("warm-pool priority policy = %#v", spec)
	}
	container := spec["containers"].([]any)[0].(map[string]any)
	if container["image"] != "ghcr.io/synara/worker@"+firstDigest {
		t.Fatalf("release Pod image = %q", container["image"])
	}
	metadata := pod["metadata"].(map[string]any)
	labels := metadata["labels"].(map[string]string)
	if labels[kubernetesReleaseLabel] != firstRevision.String() || labels[kubernetesChannelLabel] != channel {
		t.Fatalf("release Pod labels = %#v", labels)
	}
	podSpec := pod["spec"].(map[string]any)
	pullSecrets := podSpec["imagePullSecrets"].([]any)
	if kubernetesNamedObject(pullSecrets, kubernetesRegistrySecretName(fixture.targetID)) == nil {
		t.Fatalf("release Pod imagePullSecrets = %#v", pullSecrets)
	}
	registrySecret := client.namedKind("Secret", kubernetesRegistrySecretName(fixture.targetID))
	if registrySecret == nil || registrySecret["type"] != "kubernetes.io/dockerconfigjson" {
		t.Fatalf("registry Secret = %#v", registrySecret)
	}
	encodedConfig := registrySecret["data"].(map[string]any)[".dockerconfigjson"].(string)
	decodedConfig, err := base64.StdEncoding.DecodeString(encodedConfig)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(decodedConfig, []byte("registry-password-secret")) || bytes.Contains(mustJSON(t, registrySecret), []byte("registry-password-secret")) {
		t.Fatalf("registry Secret encoding is invalid: decoded=%s object=%s", decodedConfig, mustJSON(t, registrySecret))
	}

	if err := fixture.db.Model(&persistence.AgentExecution{}).Where("id = ?", fixture.executionIDs[0]).
		Updates(map[string]any{"worker_release_revision_id": secondRevision, "worker_release_channel": channel}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.deletedPods) != 1 {
		t.Fatalf("release drift deleted Pods = %#v", client.deletedPods)
	}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	newPod := client.lastKind("Pod")
	newContainer := newPod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	if newContainer["image"] != "ghcr.io/synara/worker@"+secondDigest {
		t.Fatalf("replacement release Pod image = %q", newContainer["image"])
	}
}

func TestKubernetesReconcilerCreatesWarmPoolPodForActiveWarmPool(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	configuration := kubernetesTestConfiguration("")
	configuration["image"] = "ghcr.io/synara/worker:mutable"
	configuration["maxActivePods"] = 2
	fixture.updateConfiguration(t, configuration)
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("execution_target_id = ?", fixture.targetID).
		Update("status", "completed").Error; err != nil {
		t.Fatal(err)
	}
	promotedDigest := "sha256:" + strings.Repeat("d", 64)
	promotedRevision := fixture.seedReleaseRevision(t, 1, promotedDigest)
	fixture.seedReleasePolicy(t, promotedRevision, nil, 0)
	pool := fixture.createWarmPool(t, placement.CapacityClassInteractive, 1, 1, placement.PoolStatusActive)
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.pods) != 1 || client.kindCount("Pod") != 1 {
		t.Fatalf("warm-pool reconciliation created pods=%d active=%#v", client.kindCount("Pod"), client.pods)
	}
	pod := client.lastKind("Pod")
	metadata := pod["metadata"].(map[string]any)
	labels := metadata["labels"].(map[string]string)
	if labels[kubernetesWorkerModeLabel] != kubernetesWorkerModeWarmPool ||
		labels[kubernetesWorkerPoolIDLabel] != pool.ID.String() ||
		labels[kubernetesWorkerPoolVersionLabel] != "1" ||
		labels[kubernetesCapacityClassLabel] != placement.CapacityClassInteractive ||
		labels[kubernetesReleaseLabel] != promotedRevision.String() ||
		labels[kubernetesChannelLabel] != "promoted" {
		t.Fatalf("warm-pool labels = %#v", labels)
	}
	if _, found := labels[kubernetesExecutionLabel]; found {
		t.Fatalf("warm-pool pod unexpectedly carried execution label: %#v", labels)
	}
	container := pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	assertKubernetesContainerResourceLimits(t, container)
	if container["image"] != "ghcr.io/synara/worker@"+promotedDigest {
		t.Fatalf("warm-pool image = %q", container["image"])
	}
	if value, found := kubernetesEnvironmentValue(container, "SYNARA_AGENTD_WORKER_MODE"); !found || value != kubernetesWorkerModeWarmPool {
		t.Fatalf("warm-pool worker mode env = %q", value)
	}
	if value, found := kubernetesEnvironmentValue(container, "SYNARA_AGENTD_PRIVATE_TMP_ROOT"); !found || value != "/tmp" {
		t.Fatalf("warm-pool Worker-private temporary root env = %q", value)
	}
	if _, found := kubernetesEnvironmentValue(container, "SYNARA_AGENTD_ASSIGNED_EXECUTION_ID"); found {
		t.Fatal("warm-pool pod unexpectedly carried an assigned execution environment")
	}
}

func TestKubernetesReconcilerDeletesUnleasedWarmPoolPodDuringActiveCanary(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	configuration := kubernetesTestConfiguration("")
	configuration["image"] = "ghcr.io/synara/worker:mutable"
	configuration["maxActivePods"] = 2
	fixture.updateConfiguration(t, configuration)
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("execution_target_id = ?", fixture.targetID).
		Update("status", "completed").Error; err != nil {
		t.Fatal(err)
	}
	promotedRevision := fixture.seedReleaseRevision(t, 1, "sha256:"+strings.Repeat("d", 64))
	canaryRevision := fixture.seedReleaseRevision(t, 2, "sha256:"+strings.Repeat("e", 64))
	fixture.createWarmPool(t, placement.CapacityClassInteractive, 1, 1, placement.PoolStatusActive)
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}

	fixture.seedReleasePolicy(t, promotedRevision, nil, 0)
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.pods) != 1 {
		t.Fatalf("expected initial unleased warm pod, got %#v", client.pods)
	}

	fixture.seedReleasePolicy(t, promotedRevision, &canaryRevision, 20)
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.deletedPods) != 1 || len(client.pods) != 0 {
		t.Fatalf("active canary did not delete unleased warm pod exactly once: deleted=%#v active=%#v", client.deletedPods, client.pods)
	}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.pods) != 0 || client.kindCount("Pod") != 1 {
		t.Fatalf("active canary unexpectedly recreated warm pods: created=%d active=%#v", client.kindCount("Pod"), client.pods)
	}
}

func TestKubernetesReconcilerPrioritizesExecutionPodsBeforeWarmPoolsAtTargetCap(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	fixture.createWarmPool(t, placement.CapacityClassInteractive, 1, 1, placement.PoolStatusActive)
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.pods) != 1 || client.kindCount("Pod") != 1 {
		t.Fatalf("target cap reconciliation created pods=%d active=%#v", client.kindCount("Pod"), client.pods)
	}
	pod := client.lastKind("Pod")
	labels := pod["metadata"].(map[string]any)["labels"].(map[string]string)
	if labels[kubernetesWorkerModeLabel] != kubernetesWorkerModeExecutionPinned ||
		labels[kubernetesExecutionLabel] != fixture.executionIDs[0].String() {
		t.Fatalf("execution pod was not prioritized ahead of warm pool: %#v", labels)
	}
}

func TestKubernetesReconcilerPrioritizesGuaranteedWarmPodsAndExposesBudgetTruncation(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	configuration := kubernetesTestConfiguration("")
	configuration["maxActivePods"] = 1
	fixture.updateConfiguration(t, configuration)
	pool := fixture.createWarmPoolWithMinIdle(
		t,
		placement.CapacityClassInteractive,
		2,
		2,
		2,
		placement.PoolStatusActive,
	)
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	var observations []ManagedKubernetesWarmCapacityObservation
	fixture.reconciler.config.PublishWarmCapacity = func(_ context.Context, observation ManagedKubernetesWarmCapacityObservation) error {
		observations = append(observations, observation)
		return nil
	}

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.pods) != 1 || client.kindCount("Pod") != 1 {
		t.Fatalf("budget-truncated reconcile created pods=%d active=%#v", client.kindCount("Pod"), client.pods)
	}
	pod := onlyFakePod(t, client)
	if pod.Labels[kubernetesWorkerModeLabel] != kubernetesWorkerModeWarmPool ||
		pod.Labels[kubernetesWarmSlotLabel] != "0" ||
		pod.Labels[kubernetesWorkerPoolIDLabel] != pool.ID.String() {
		t.Fatalf("guaranteed warm Pod was not prioritized ahead of cold demand: %#v", pod.Labels)
	}
	if len(observations) != 1 || observations[0].MinIdleUnits != 2 ||
		observations[0].DesiredTotalUnits != 2 || observations[0].ReadyIdleUnits != 0 {
		t.Fatalf("budget-truncated warm authority = %#v", observations)
	}
}

func TestKubernetesReconcilerReservesReadyWarmWorkerBeforeColdFallback(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	configuration := kubernetesTestConfiguration("")
	configuration["maxActivePods"] = 2
	fixture.updateConfiguration(t, configuration)
	pool := fixture.createWarmPool(t, placement.CapacityClassInteractive, 1, 1, placement.PoolStatusActive)
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("execution_target_id = ?", fixture.targetID).
		Update("status", "completed").Error; err != nil {
		t.Fatal(err)
	}
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	warmPod := onlyFakePod(t, client)
	warmPod.Phase = "Running"
	client.pods[warmPod.Name] = warmPod
	fixture.registerWarmWorker(t, pool, warmPod, "online", "active", nil, nil)
	for _, executionID := range fixture.executionIDs {
		fixture.assignExecutionToPool(t, executionID, pool, "queued")
	}

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, found := findExecutionPod(client, fixture.executionIDs[0]); found {
		t.Fatalf("first execution unexpectedly received a cold Pod despite ready warm capacity: %#v", client.pods)
	}
	executionPod, found := findExecutionPod(client, fixture.executionIDs[1])
	if !found {
		t.Fatalf("second execution did not fall back to a cold Pod: %#v", client.pods)
	}
	if executionPod.Labels[kubernetesWorkerModeLabel] != kubernetesWorkerModeExecutionPinned {
		t.Fatalf("fallback Pod worker mode = %#v", executionPod.Labels)
	}
	if len(client.deletedPods) != 0 {
		t.Fatalf("ready warm capacity unexpectedly deleted a Pod: %#v", client.deletedPods)
	}
}

func TestKubernetesReconcilerIgnoresTerminatedWarmWorkerWithReusedPodName(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	configuration := kubernetesTestConfiguration("")
	configuration["maxActivePods"] = 2
	fixture.updateConfiguration(t, configuration)
	pool := fixture.createWarmPool(t, placement.CapacityClassInteractive, 1, 1, placement.PoolStatusActive)
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("execution_target_id = ?", fixture.targetID).
		Update("status", "completed").Error; err != nil {
		t.Fatal(err)
	}
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	warmPod := onlyFakePod(t, client)
	warmPod.Phase = "Running"
	client.pods[warmPod.Name] = warmPod
	current := fixture.registerWarmWorker(t, pool, warmPod, "online", "active", nil, nil)
	terminatedAt := time.Now().UTC()
	old := current
	old.ID = uuid.New()
	old.InstanceUID = uuid.NewString()
	old.AuthTokenHash = []byte("terminated-warm-worker-hash")
	old.Status = "terminated"
	old.TerminatedAt = &terminatedAt
	if err := fixture.db.Create(&old).Error; err != nil {
		t.Fatal(err)
	}

	states, err := fixture.reconciler.loadKubernetesWarmWorkerStates(context.Background(), fixture.targetID)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].WorkerID != current.ID || states[0].InstanceUID != warmPod.UID {
		t.Fatalf("loaded warm Worker states = %#v, want only the current Pod UID", states)
	}
	for _, executionID := range fixture.executionIDs {
		fixture.assignExecutionToPool(t, executionID, pool, "queued")
	}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, found := findExecutionPod(client, fixture.executionIDs[0]); found {
		t.Fatalf("current warm Worker was hidden by a terminated same-name identity: %#v", client.pods)
	}
	if _, found := findExecutionPod(client, fixture.executionIDs[1]); !found {
		t.Fatalf("second execution did not receive the expected cold fallback: %#v", client.pods)
	}
}

func TestKubernetesWarmWorkerStateLookupUsesPodNameAndUID(t *testing.T) {
	podName := "reused-warm-pod"
	oldUID := uuid.NewString()
	currentUID := uuid.NewString()
	old := kubernetesWarmWorkerState{WorkerID: uuid.New(), PodName: podName, InstanceUID: oldUID}
	current := kubernetesWarmWorkerState{WorkerID: uuid.New(), PodName: podName, InstanceUID: currentUID}
	states := map[kubernetesWarmWorkerIdentity]kubernetesWarmWorkerState{
		kubernetesWarmWorkerIdentityKey(old.PodName, old.InstanceUID):         old,
		kubernetesWarmWorkerIdentityKey(current.PodName, current.InstanceUID): current,
	}

	matched, found := kubernetesWarmWorkerStateForPod(states, kubernetesPod{Name: podName, UID: currentUID})
	if !found || matched.WorkerID != current.WorkerID {
		t.Fatalf("composite warm Worker lookup = (%#v, %t), want current UID", matched, found)
	}
}

func TestKubernetesReconcilerFallsBackToColdPodWhenWarmPodIsUnregistered(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	configuration := kubernetesTestConfiguration("")
	configuration["maxActivePods"] = 2
	fixture.updateConfiguration(t, configuration)
	pool := fixture.createWarmPool(t, placement.CapacityClassInteractive, 1, 1, placement.PoolStatusActive)
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("execution_target_id = ?", fixture.targetID).
		Update("status", "completed").Error; err != nil {
		t.Fatal(err)
	}
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixture.assignExecutionToPool(t, fixture.executionIDs[0], pool, "queued")

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, found := findExecutionPod(client, fixture.executionIDs[0]); !found {
		t.Fatalf("queued execution did not fall back to a cold Pod with only an unregistered warm Pod: %#v", client.pods)
	}
}

func TestKubernetesReconcilerFallsBackToColdPodWhenWarmWorkerIsOffline(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	configuration := kubernetesTestConfiguration("")
	configuration["maxActivePods"] = 2
	fixture.updateConfiguration(t, configuration)
	pool := fixture.createWarmPool(t, placement.CapacityClassInteractive, 1, 1, placement.PoolStatusActive)
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("execution_target_id = ?", fixture.targetID).
		Update("status", "completed").Error; err != nil {
		t.Fatal(err)
	}
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	warmPod := onlyFakePod(t, client)
	warmPod.Phase = "Running"
	client.pods[warmPod.Name] = warmPod
	fixture.registerWarmWorker(t, pool, warmPod, "offline", "active", nil, nil)
	fixture.assignExecutionToPool(t, fixture.executionIDs[0], pool, "queued")

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, found := findExecutionPod(client, fixture.executionIDs[0]); !found {
		t.Fatalf("queued execution did not fall back to a cold Pod when the warm Worker was offline: %#v", client.pods)
	}
	if len(client.deletedPods) != 1 || client.deletedPods[0] != warmPod.Name {
		t.Fatalf("offline warm Pod was not recycled exactly once: deleted=%#v active=%#v", client.deletedPods, client.pods)
	}
}

func TestKubernetesReconcilerRetainsIncompatibleWarmWorkerWithoutReportingReadyCapacity(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	configuration := kubernetesTestConfiguration("")
	configuration["maxActivePods"] = 2
	fixture.updateConfiguration(t, configuration)
	pool := fixture.createWarmPool(t, placement.CapacityClassInteractive, 1, 1, placement.PoolStatusActive)
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("execution_target_id = ?", fixture.targetID).
		Update("status", "completed").Error; err != nil {
		t.Fatal(err)
	}
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	warmPod := onlyFakePod(t, client)
	warmPod.Phase = "Running"
	client.pods[warmPod.Name] = warmPod
	worker := fixture.registerWarmWorker(t, pool, warmPod, "online", "active", nil, nil)
	if err := fixture.db.Model(&persistence.WorkerInstance{}).Where("id = ?", worker.ID).
		Update("compatibility_status", "incompatible").Error; err != nil {
		t.Fatal(err)
	}
	fixture.assignExecutionToPool(t, fixture.executionIDs[0], pool, "queued")
	var observations []ManagedKubernetesWarmCapacityObservation
	fixture.reconciler.config.PublishWarmCapacity = func(_ context.Context, observation ManagedKubernetesWarmCapacityObservation) error {
		observations = append(observations, observation)
		return nil
	}

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, found := client.pods[warmPod.Name]; !found {
		t.Fatalf("incompatible warm Pod was deleted: deleted=%#v active=%#v", client.deletedPods, client.pods)
	}
	if _, found := findExecutionPod(client, fixture.executionIDs[0]); !found {
		t.Fatalf("queued execution did not receive a cold fallback: %#v", client.pods)
	}
	if len(observations) != 1 || observations[0].ReadyIdleUnits != 0 {
		t.Fatalf("incompatible warm capacity observations = %#v, want readyIdleUnits=0", observations)
	}
	createdPods := client.kindCount("Pod")
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, found := client.pods[warmPod.Name]; !found || len(client.deletedPods) != 0 || client.kindCount("Pod") != createdPods {
		t.Fatalf("incompatible warm Pod churned on a later pass: deleted=%#v active=%#v created=%d->%d", client.deletedPods, client.pods, createdPods, client.kindCount("Pod"))
	}
}

func TestKubernetesReconcilerEvictsNotReadyWarmWorkerForColdDemandAtTargetCap(t *testing.T) {
	tests := []struct {
		name       string
		assignWarm bool
	}{
		{name: "matching warm pool demand", assignWarm: true},
		{name: "general cold demand"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newKubernetesReconcileFixture(t, "")
			pool := fixture.createWarmPool(t, placement.CapacityClassInteractive, 1, 1, placement.PoolStatusActive)
			if err := fixture.db.Model(&persistence.AgentExecution{}).
				Where("execution_target_id = ?", fixture.targetID).
				Update("status", "completed").Error; err != nil {
				t.Fatal(err)
			}
			client := newFakeKubernetesClient()
			fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
			if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			warmPod := onlyFakePod(t, client)
			warmPod.Phase = "Running"
			client.pods[warmPod.Name] = warmPod
			worker := fixture.registerWarmWorker(t, pool, warmPod, "online", "active", nil, nil)
			if err := fixture.db.Model(&persistence.WorkerInstance{}).Where("id = ?", worker.ID).
				Update("compatibility_status", "incompatible").Error; err != nil {
				t.Fatal(err)
			}
			if test.assignWarm {
				fixture.assignExecutionToPool(t, fixture.executionIDs[0], pool, "queued")
			} else if err := fixture.db.Model(&persistence.AgentExecution{}).
				Where("id = ?", fixture.executionIDs[0]).Update("status", "queued").Error; err != nil {
				t.Fatal(err)
			}

			if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(client.deletedPods) != 1 || client.deletedPods[0] != warmPod.Name || len(client.pods) != 0 {
				t.Fatalf("first pass did not evict only the not-ready warm Pod: deleted=%#v active=%#v", client.deletedPods, client.pods)
			}
			if client.kindCount("Pod") != 1 {
				t.Fatalf("first pass recreated a Pod before deletion released quota: created=%d", client.kindCount("Pod"))
			}

			if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			executionPod, found := findExecutionPod(client, fixture.executionIDs[0])
			if !found || executionPod.Labels[kubernetesWorkerModeLabel] != kubernetesWorkerModeExecutionPinned {
				t.Fatalf("second pass did not create the cold execution Pod: %#v", client.pods)
			}
			createdPods := client.kindCount("Pod")
			if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(client.deletedPods) != 1 || client.kindCount("Pod") != createdPods || len(client.pods) != 1 {
				t.Fatalf("later pass rebuilt warm capacity or repeated deletion: deleted=%#v active=%#v created=%d->%d", client.deletedPods, client.pods, createdPods, client.kindCount("Pod"))
			}
		})
	}
}

func TestKubernetesReconcilerExemptsGuaranteedWarmSlotFromDemandFallbackEviction(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	configuration := kubernetesTestConfiguration("")
	configuration["maxActivePods"] = 2
	fixture.updateConfiguration(t, configuration)
	fixture.createWarmPoolWithMinIdle(
		t,
		placement.CapacityClassInteractive,
		2,
		1,
		2,
		placement.PoolStatusActive,
	)
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("execution_target_id = ?", fixture.targetID).
		Update("status", "completed").Error; err != nil {
		t.Fatal(err)
	}
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	warmPodsBySlot := make(map[string]kubernetesPod)
	for _, pod := range client.pods {
		warmPodsBySlot[pod.Labels[kubernetesWarmSlotLabel]] = pod
	}
	if len(warmPodsBySlot) != 2 {
		t.Fatalf("initial warm slots = %#v, want slots 0 and 1", warmPodsBySlot)
	}
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("id = ?", fixture.executionIDs[0]).
		Update("status", "queued").Error; err != nil {
		t.Fatal(err)
	}

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.deletedPods) != 1 || client.deletedPods[0] != warmPodsBySlot["1"].Name {
		t.Fatalf("demand fallback deleted %#v, want only best-effort slot 1", client.deletedPods)
	}
	if _, found := client.pods[warmPodsBySlot["0"].Name]; !found {
		t.Fatalf("guaranteed slot 0 was evicted: active=%#v", client.pods)
	}
	if _, found := findExecutionPod(client, fixture.executionIDs[0]); found {
		t.Fatalf("cold Pod was created before deletion released quota: active=%#v", client.pods)
	}

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, found := findExecutionPod(client, fixture.executionIDs[0]); !found {
		t.Fatalf("cold demand did not use capacity released by best-effort slot: active=%#v", client.pods)
	}
}

func TestKubernetesReconcilerRecyclesWarmWorkerWithStaleHeartbeat(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	pool := fixture.createWarmPool(t, placement.CapacityClassInteractive, 1, 1, placement.PoolStatusActive)
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("execution_target_id = ?", fixture.targetID).
		Update("status", "completed").Error; err != nil {
		t.Fatal(err)
	}
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	warmPod := onlyFakePod(t, client)
	warmPod.Phase = "Running"
	client.pods[warmPod.Name] = warmPod
	worker := fixture.registerWarmWorker(t, pool, warmPod, "online", "active", nil, nil)
	staleHeartbeat := time.Now().UTC().Add(-fixture.reconciler.config.WorkerHeartbeatTimeout - time.Second)
	if err := fixture.db.Model(&persistence.WorkerInstance{}).Where("id = ?", worker.ID).
		Update("last_heartbeat_at", staleHeartbeat).Error; err != nil {
		t.Fatal(err)
	}

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.deletedPods) != 1 || client.deletedPods[0] != warmPod.Name || len(client.pods) != 0 {
		t.Fatalf("stale warm Pod was not recycled exactly once: deleted=%#v active=%#v", client.deletedPods, client.pods)
	}
}

func TestKubernetesReconcilerTreatsExpiredWarmWorkerLeaseRowAsBusy(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	pool := fixture.createWarmPool(t, placement.CapacityClassInteractive, 1, 1, placement.PoolStatusActive)
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("execution_target_id = ?", fixture.targetID).
		Update("status", "completed").Error; err != nil {
		t.Fatal(err)
	}
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	warmPod := onlyFakePod(t, client)
	warmPod.Phase = "Running"
	client.pods[warmPod.Name] = warmPod
	worker := fixture.registerWarmWorker(t, pool, warmPod, "offline", "active", nil, nil)
	if err := fixture.db.Model(&persistence.AgentExecution{}).Where("id = ?", fixture.executionIDs[0]).
		Updates(map[string]any{
			"status": "leased", "worker_id": worker.ID, "generation": 1, "finished_at": nil,
		}).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := fixture.db.Create(&persistence.WorkerLease{
		ExecutionID: fixture.executionIDs[0], TenantID: fixture.tenantID,
		WorkerID: worker.ID, WorkerIncarnation: worker.Incarnation, WorkerInstanceUID: worker.InstanceUID,
		Generation: 1, LeaseTokenHash: []byte("expired-warm-worker-lease"),
		AcquiredAt: now.Add(-3 * time.Minute), HeartbeatAt: now.Add(-2 * time.Minute), ExpiresAt: now.Add(-time.Minute),
	}).Error; err != nil {
		t.Fatal(err)
	}

	states, err := fixture.reconciler.loadKubernetesWarmWorkerStates(context.Background(), fixture.targetID)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || !states[0].HasLease {
		t.Fatalf("expired lease row did not keep the warm Worker busy: %#v", states)
	}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(client.deletedPods) != 0 {
		t.Fatalf("busy warm Worker with an expired lease row was deleted: %#v", client.deletedPods)
	}
	if _, found := client.pods[warmPod.Name]; !found {
		t.Fatalf("busy warm Pod was not retained: %#v", client.pods)
	}
}

func TestKubernetesWarmPodDeletionRechecksRegistrationAndLeaseAtDeleteBoundary(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	pool := fixture.createWarmPool(t, placement.CapacityClassInteractive, 1, 1, placement.PoolStatusActive)
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("execution_target_id = ?", fixture.targetID).
		Update("status", "completed").Error; err != nil {
		t.Fatal(err)
	}
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	warmPod := onlyFakePod(t, client)
	warmPod.Phase = "Running"
	client.pods[warmPod.Name] = warmPod
	states, err := fixture.reconciler.loadKubernetesWarmWorkerStates(context.Background(), fixture.targetID)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 0 {
		t.Fatalf("pre-delete snapshot unexpectedly included a registered Worker: %#v", states)
	}

	worker := fixture.registerWarmWorker(t, pool, warmPod, "online", "active", nil, nil)
	if err := fixture.db.Model(&persistence.AgentExecution{}).Where("id = ?", fixture.executionIDs[0]).
		Updates(map[string]any{
			"status": "leased", "worker_id": worker.ID, "generation": 1, "finished_at": nil,
		}).Error; err != nil {
		t.Fatal(err)
	}
	fixture.createWarmWorkerLease(t, worker, fixture.executionIDs[0])

	deleted, retainedWorker, err := fixture.reconciler.deleteObservedPodSafely(
		context.Background(),
		client,
		fixture.targetID,
		"synara-test",
		warmPod,
		"registration-claim-race-test",
	)
	if err != nil {
		t.Fatal(err)
	}
	if deleted || retainedWorker == nil || retainedWorker.ID != worker.ID {
		t.Fatalf("delete boundary result = deleted=%t retained=%#v, want the newly registered leased Worker", deleted, retainedWorker)
	}
	if len(client.deletedPods) != 0 {
		t.Fatalf("leased Pod was deleted after a state=nil snapshot: %#v", client.deletedPods)
	}
	if _, found := client.pods[warmPod.Name]; !found {
		t.Fatalf("leased Pod disappeared after delete-boundary recheck: %#v", client.pods)
	}
	var stored persistence.WorkerInstance
	if err := fixture.db.Where("id = ?", worker.ID).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != "online" || stored.DrainingAt != nil {
		t.Fatalf("leased Worker was drained during delete-boundary recheck: %#v", stored)
	}
	var fenceCount int64
	if err := fixture.db.Model(&persistence.KubernetesPodDeletionFence{}).
		Where("execution_target_id = ? AND namespace = ? AND pod_name = ? AND pod_uid = ?", fixture.targetID, "synara-test", warmPod.Name, warmPod.UID).
		Count(&fenceCount).Error; err != nil {
		t.Fatal(err)
	}
	if fenceCount != 0 {
		t.Fatalf("leased Worker deletion wrote %d durable fences", fenceCount)
	}
}

func TestKubernetesPodDeletionRetainsWorkerWithActiveWorkspaceCleanupLease(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	client := newFakeKubernetesClient()
	pod := kubernetesPod{
		Name: "cleanup-busy-" + uuid.NewString(), UID: uuid.NewString(), Phase: "Running",
		Labels: map[string]string{kubernetesTargetLabel: fixture.targetID.String()},
	}
	client.pods[pod.Name] = pod
	now := time.Now().UTC().Truncate(time.Millisecond)
	worker := persistence.WorkerInstance{
		ID: uuid.New(), Incarnation: 1, InstanceUID: pod.UID,
		ExecutionTargetID: fixture.targetID, TargetKind: "kubernetes", WorkerMode: kubernetesWorkerModeGeneralPool,
		RegistrationTrustMode: kubernetesPodBoundRegistrationTrust,
		ClusterID:             kubernetesLocalClusterID,
		Namespace:             "synara-test",
		PodName:               pod.Name,
		Version:               "cleanup-busy-worker",
		ProtocolVersion:       kubernetesWorkerProtocolVersion,
		Capabilities:          map[string]any{},
		LeaseSupported:        true,
		FencingSupported:      true,
		AuthTokenHash:         []byte("cleanup-busy-worker-hash"),
		Status:                "online",
		AdministrativeStatus:  "active",
		CompatibilityStatus:   "unknown",
		RegisteredAt:          now,
		LastHeartbeatAt:       now,
	}
	if err := fixture.db.Create(&worker).Error; err != nil {
		t.Fatal(err)
	}
	workspaceID := uuid.New()
	materializationID := uuid.New()
	incarnationID := uuid.New()
	reason := "cleanup-busy-test"
	workspace := persistence.RemoteWorkspace{
		ID: workspaceID, TenantID: fixture.tenantID, OrganizationID: fixture.organizationID,
		ProjectID: fixture.projectID, SessionID: fixture.sessionID, ExecutionTargetID: fixture.targetID,
		WorkspaceMode: "clone", State: "cleanup-pending", DefaultBranch: "main", CreatedAt: now, UpdatedAt: now,
	}
	materialization := persistence.WorkspaceMaterialization{
		ID: materializationID, TenantID: fixture.tenantID, WorkspaceID: workspaceID,
		OrganizationID: fixture.organizationID, ProjectID: fixture.projectID, SessionID: fixture.sessionID,
		ExecutionTargetID: fixture.targetID, TargetKind: "kubernetes", StorageScope: "target", LayoutVersion: 3,
		IncarnationID: incarnationID, State: "cleanup-pending", CleanupReason: &reason,
		CleanupRequestedAt: &now, CreatedAt: now, UpdatedAt: now,
	}
	command := persistence.WorkspaceCleanupCommand{
		ID: uuid.New(), TenantID: fixture.tenantID, MaterializationID: materializationID,
		MaterializationIncarnationID: incarnationID, WorkspaceID: workspaceID,
		ExecutionTargetID: fixture.targetID, TargetKind: "kubernetes", StorageScope: "target", LayoutVersion: 3,
		Reason: reason, Status: "pending", DeliveryAvailableAt: now, RequestedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	if err := fixture.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&workspace).Error; err != nil {
			return err
		}
		if err := tx.Create(&materialization).Error; err != nil {
			return err
		}
		if err := tx.Model(&persistence.RemoteWorkspace{}).Where("id = ?", workspace.ID).
			Update("current_materialization_id", materialization.ID).Error; err != nil {
			return err
		}
		return tx.Create(&command).Error
	}); err != nil {
		t.Fatal(err)
	}
	expiresAt := now.Add(-time.Minute)
	if err := fixture.db.Model(&persistence.WorkspaceCleanupCommand{}).Where("id = ?", command.ID).
		Updates(map[string]any{
			"status": "leased", "lease_token_hash": []byte("cleanup-busy-lease"),
			"dispatch_generation": 1, "delivery_worker_id": worker.ID,
			"delivery_worker_incarnation": worker.Incarnation, "delivery_attempts": 1,
			"leased_at": now.Add(-2 * time.Minute), "lease_expires_at": expiresAt, "updated_at": now,
		}).Error; err != nil {
		t.Fatal(err)
	}

	deleted, retainedWorker, err := fixture.reconciler.deleteObservedPodSafely(
		context.Background(), client, fixture.targetID, "synara-test", pod, "cleanup-busy-delete-test",
	)
	if err != nil {
		t.Fatal(err)
	}
	if deleted || retainedWorker == nil || retainedWorker.ID != worker.ID {
		t.Fatalf("cleanup-busy deletion = deleted=%t retained=%#v", deleted, retainedWorker)
	}
	if len(client.deletedPods) != 0 {
		t.Fatalf("cleanup-busy Pod was deleted: %#v", client.deletedPods)
	}
	var fenceCount int64
	if err := fixture.db.Model(&persistence.KubernetesPodDeletionFence{}).
		Where("execution_target_id = ? AND namespace = ? AND pod_name = ? AND pod_uid = ?", fixture.targetID, "synara-test", pod.Name, pod.UID).
		Count(&fenceCount).Error; err != nil {
		t.Fatal(err)
	}
	if fenceCount != 0 {
		t.Fatalf("cleanup-busy deletion wrote %d fences", fenceCount)
	}
}

func TestKubernetesWarmPodDeletionFenceSurvivesUnknownDeleteAndRetriesIdempotently(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	fixture.createWarmPool(t, placement.CapacityClassInteractive, 1, 1, placement.PoolStatusActive)
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("execution_target_id = ?", fixture.targetID).
		Update("status", "completed").Error; err != nil {
		t.Fatal(err)
	}
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	warmPod := onlyFakePod(t, client)
	firstRequestedAt := time.Date(2026, 7, 26, 1, 2, 3, 0, time.UTC)
	fixture.reconciler.now = func() time.Time { return firstRequestedAt }
	client.deletePodErr = errors.New("delete outcome unknown")

	deleted, retainedWorker, err := fixture.reconciler.deleteObservedPodSafely(
		context.Background(), client, fixture.targetID, "synara-test", warmPod, "unknown-delete-outcome",
	)
	if err == nil || deleted || retainedWorker != nil {
		t.Fatalf("unknown DELETE result = deleted=%t retained=%#v err=%v", deleted, retainedWorker, err)
	}
	if _, found := client.pods[warmPod.Name]; !found {
		t.Fatalf("failed fake DELETE removed the Pod: %#v", client.pods)
	}
	var firstFence persistence.KubernetesPodDeletionFence
	if err := fixture.db.Where(
		"execution_target_id = ? AND namespace = ? AND pod_name = ? AND pod_uid = ?",
		fixture.targetID, "synara-test", warmPod.Name, warmPod.UID,
	).Take(&firstFence).Error; err != nil {
		t.Fatal(err)
	}
	if firstFence.Reason != "unknown-delete-outcome" || !firstFence.RequestedAt.Equal(firstRequestedAt) {
		t.Fatalf("first durable deletion fence = %#v", firstFence)
	}

	client.deletePodErr = nil
	fixture.reconciler.now = func() time.Time { return firstRequestedAt.Add(time.Minute) }
	deleted, retainedWorker, err = fixture.reconciler.deleteObservedPodSafely(
		context.Background(), client, fixture.targetID, "synara-test", warmPod, "retry-delete",
	)
	if err != nil || !deleted || retainedWorker != nil {
		t.Fatalf("retry DELETE result = deleted=%t retained=%#v err=%v", deleted, retainedWorker, err)
	}
	var fences []persistence.KubernetesPodDeletionFence
	if err := fixture.db.Where(
		"execution_target_id = ? AND namespace = ? AND pod_name = ? AND pod_uid = ?",
		fixture.targetID, "synara-test", warmPod.Name, warmPod.UID,
	).Find(&fences).Error; err != nil {
		t.Fatal(err)
	}
	if len(fences) != 1 || fences[0].Reason != firstFence.Reason || !fences[0].RequestedAt.Equal(firstFence.RequestedAt) {
		t.Fatalf("retry changed or duplicated the durable deletion fence: %#v", fences)
	}
}

func TestKubernetesPodDeleteUIDConflictIsBenignAndRetainsOldUIDFence(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	client := newFakeKubernetesClient()
	podName := "same-name-replacement"
	observed := kubernetesPod{Name: podName, UID: uuid.NewString(), Phase: "Running"}
	replacement := observed
	replacement.UID = uuid.NewString()
	client.pods[podName] = replacement

	deleted, retainedWorker, err := fixture.reconciler.deleteObservedPodSafely(
		context.Background(),
		client,
		fixture.targetID,
		"synara-test",
		observed,
		"stale-observation",
	)
	if err != nil || deleted || retainedWorker != nil {
		t.Fatalf("stale UID delete result = deleted=%t retained=%#v err=%v", deleted, retainedWorker, err)
	}
	if current := client.pods[podName]; current.UID != replacement.UID {
		t.Fatalf("same-name replacement was changed by stale deletion: %#v", current)
	}
	var oldFenceCount int64
	if err := fixture.db.Model(&persistence.KubernetesPodDeletionFence{}).
		Where(
			"execution_target_id = ? AND namespace = ? AND pod_name = ? AND pod_uid = ?",
			fixture.targetID,
			"synara-test",
			podName,
			observed.UID,
		).
		Count(&oldFenceCount).Error; err != nil {
		t.Fatal(err)
	}
	if oldFenceCount != 1 {
		t.Fatalf("stale observed UID durable fences = %d, want 1", oldFenceCount)
	}
	var replacementFenceCount int64
	if err := fixture.db.Model(&persistence.KubernetesPodDeletionFence{}).
		Where(
			"execution_target_id = ? AND namespace = ? AND pod_name = ? AND pod_uid = ?",
			fixture.targetID,
			"synara-test",
			podName,
			replacement.UID,
		).
		Count(&replacementFenceCount).Error; err != nil {
		t.Fatal(err)
	}
	if replacementFenceCount != 0 {
		t.Fatalf("replacement UID was incorrectly fenced %d times", replacementFenceCount)
	}
}

func TestKubernetesWarmWorkerReadinessAndRecyclePredicates(t *testing.T) {
	now := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	timeout := 90 * time.Second
	poolID := uuid.New()
	poolVersion := int64(1)
	capacityClass := placement.CapacityClassInteractive
	manifestID := uuid.New()
	pod := kubernetesPod{Name: "warm-ready", UID: uuid.NewString(), Phase: "Running"}
	plan := kubernetesWarmPodPlan{Pool: kubernetesWarmPool{
		ID: poolID, Version: poolVersion, CapacityClass: capacityClass,
	}}
	ready := kubernetesWarmWorkerState{
		PodName: pod.Name, InstanceUID: pod.UID,
		WorkerPoolID: &poolID, WorkerPoolVersion: &poolVersion, CapacityClass: &capacityClass,
		WorkerReleaseStatus: "unmanaged", RegistrationTrustMode: kubernetesPodBoundRegistrationTrust,
		ProtocolVersion: kubernetesWorkerProtocolVersion, CurrentManifestID: &manifestID,
		CompatibilityStatus: "compatible", LeaseSupported: true, FencingSupported: true,
		Status: "online", AdministrativeStatus: "active", LastHeartbeatAt: now.Add(-timeout),
	}
	revisionID := uuid.New()
	channel := "promoted"
	tests := []struct {
		name    string
		mutate  func(*kubernetesWarmWorkerState, *kubernetesPod, *kubernetesWarmPodPlan)
		ready   bool
		recycle bool
	}{
		{name: "heartbeat at cutoff is fresh", ready: true},
		{name: "exact Pod UID required", mutate: func(state *kubernetesWarmWorkerState, _ *kubernetesPod, _ *kubernetesWarmPodPlan) {
			state.InstanceUID = uuid.NewString()
		}},
		{name: "Pending Pod retained", mutate: func(_ *kubernetesWarmWorkerState, pod *kubernetesPod, _ *kubernetesWarmPodPlan) {
			pod.Phase = "Pending"
		}},
		{name: "active lease retained", mutate: func(state *kubernetesWarmWorkerState, _ *kubernetesPod, _ *kubernetesWarmPodPlan) {
			state.HasLease = true
		}},
		{name: "offline recycled", mutate: func(state *kubernetesWarmWorkerState, _ *kubernetesPod, _ *kubernetesWarmPodPlan) {
			state.Status = "offline"
		}, recycle: true},
		{name: "draining recycled", mutate: func(state *kubernetesWarmWorkerState, _ *kubernetesPod, _ *kubernetesWarmPodPlan) {
			state.Status = "draining"
		}, recycle: true},
		{name: "administratively revoked retained", mutate: func(state *kubernetesWarmWorkerState, _ *kubernetesPod, _ *kubernetesWarmPodPlan) {
			state.AdministrativeStatus = "revoked"
		}},
		{name: "wrong protocol retained", mutate: func(state *kubernetesWarmWorkerState, _ *kubernetesPod, _ *kubernetesWarmPodPlan) {
			state.ProtocolVersion++
		}},
		{name: "missing manifest retained", mutate: func(state *kubernetesWarmWorkerState, _ *kubernetesPod, _ *kubernetesWarmPodPlan) {
			state.CurrentManifestID = nil
		}},
		{name: "unknown compatibility retained", mutate: func(state *kubernetesWarmWorkerState, _ *kubernetesPod, _ *kubernetesWarmPodPlan) {
			state.CompatibilityStatus = "unknown"
		}},
		{name: "incompatible retained", mutate: func(state *kubernetesWarmWorkerState, _ *kubernetesPod, _ *kubernetesWarmPodPlan) {
			state.CompatibilityStatus = "incompatible"
		}},
		{name: "missing lease support retained", mutate: func(state *kubernetesWarmWorkerState, _ *kubernetesPod, _ *kubernetesWarmPodPlan) {
			state.LeaseSupported = false
		}},
		{name: "missing fencing support retained", mutate: func(state *kubernetesWarmWorkerState, _ *kubernetesPod, _ *kubernetesWarmPodPlan) {
			state.FencingSupported = false
		}},
		{name: "wrong trust retained", mutate: func(state *kubernetesWarmWorkerState, _ *kubernetesPod, _ *kubernetesWarmPodPlan) {
			state.RegistrationTrustMode = "shared-token"
		}},
		{name: "heartbeat before cutoff recycled", mutate: func(state *kubernetesWarmWorkerState, _ *kubernetesPod, _ *kubernetesWarmPodPlan) {
			state.LastHeartbeatAt = now.Add(-timeout - time.Nanosecond)
		}, recycle: true},
		{name: "unsynchronized unmanaged release retained", mutate: func(state *kubernetesWarmWorkerState, _ *kubernetesPod, _ *kubernetesWarmPodPlan) {
			state.WorkerReleaseStatus = "active"
		}},
		{name: "active managed release ready", mutate: func(state *kubernetesWarmWorkerState, _ *kubernetesPod, plan *kubernetesWarmPodPlan) {
			state.WorkerReleaseRevisionID, state.WorkerReleaseChannel, state.WorkerReleaseStatus = &revisionID, &channel, "active"
			plan.Release.RevisionID, plan.Release.Channel = &revisionID, &channel
		}, ready: true},
		{name: "managed release status lag retained", mutate: func(state *kubernetesWarmWorkerState, _ *kubernetesPod, plan *kubernetesWarmPodPlan) {
			state.WorkerReleaseRevisionID, state.WorkerReleaseChannel = &revisionID, &channel
			plan.Release.RevisionID, plan.Release.Channel = &revisionID, &channel
		}, ready: false},
		{name: "terminal Pod recycled", mutate: func(_ *kubernetesWarmWorkerState, pod *kubernetesPod, _ *kubernetesWarmPodPlan) { pod.Phase = "Failed" }, recycle: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stateCopy, podCopy, planCopy := ready, pod, plan
			if test.mutate != nil {
				test.mutate(&stateCopy, &podCopy, &planCopy)
			}
			if got := kubernetesWarmWorkerStateReadyIdle(stateCopy, podCopy, planCopy, now, timeout); got != test.ready {
				t.Fatalf("ready = %t, want %t", got, test.ready)
			}
			if got := kubernetesWarmWorkerStateShouldRecycle(stateCopy, podCopy, now, timeout); got != test.recycle {
				t.Fatalf("recycle = %t, want %t", got, test.recycle)
			}
		})
	}
}

func TestKubernetesReconcilerFallsBackAndDrainsWarmPodOnReleaseMismatch(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	configuration := kubernetesTestConfiguration("")
	configuration["image"] = "ghcr.io/synara/worker:mutable"
	configuration["maxActivePods"] = 2
	fixture.updateConfiguration(t, configuration)
	firstDigest := "sha256:" + strings.Repeat("d", 64)
	secondDigest := "sha256:" + strings.Repeat("e", 64)
	firstRevision := fixture.seedReleaseRevision(t, 1, firstDigest)
	secondRevision := fixture.seedReleaseRevision(t, 2, secondDigest)
	fixture.seedReleasePolicy(t, firstRevision, nil, 0)
	pool := fixture.createWarmPool(t, placement.CapacityClassInteractive, 1, 1, placement.PoolStatusActive)
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("execution_target_id = ?", fixture.targetID).
		Update("status", "completed").Error; err != nil {
		t.Fatal(err)
	}
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	warmPod := onlyFakePod(t, client)
	warmPod.Phase = "Running"
	client.pods[warmPod.Name] = warmPod
	channel := "promoted"
	worker := fixture.registerWarmWorker(t, pool, warmPod, "online", "active", &firstRevision, &channel)
	observations := make([]KubernetesWorkerPodObservation, 0)
	fixture.reconciler.config.ObserveWorkerPod = func(_ context.Context, observation KubernetesWorkerPodObservation) error {
		observations = append(observations, observation)
		return nil
	}
	fixture.seedReleasePolicy(t, secondRevision, nil, 0)
	fixture.assignExecutionToPool(t, fixture.executionIDs[0], pool, "queued")
	if err := fixture.db.Model(&persistence.AgentExecution{}).Where("id = ?", fixture.executionIDs[0]).
		Updates(map[string]any{"worker_release_revision_id": secondRevision, "worker_release_channel": channel}).Error; err != nil {
		t.Fatal(err)
	}

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, found := findExecutionPod(client, fixture.executionIDs[0]); !found {
		t.Fatalf("queued execution did not fall back to a cold Pod when the ready warm release was stale: %#v", client.pods)
	}
	if len(client.deletedPods) != 1 || client.deletedPods[0] != warmPod.Name {
		t.Fatalf("stale warm release Pod was not deleted exactly once: deleted=%#v active=%#v", client.deletedPods, client.pods)
	}
	var drained persistence.WorkerInstance
	if err := fixture.db.Where("id = ?", worker.ID).Take(&drained).Error; err != nil {
		t.Fatal(err)
	}
	if drained.Status != "draining" || drained.DrainingAt == nil {
		t.Fatalf("stale warm Worker was not drained before delete: %#v", drained)
	}
	executionObject := client.lastKind("Pod")
	container := executionObject["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	if container["image"] != "ghcr.io/synara/worker@"+secondDigest {
		t.Fatalf("release mismatch fallback image = %q", container["image"])
	}
	if len(observations) == 0 ||
		observations[0].Reason != "delete-requested:warm-pool-scale-down" ||
		observations[0].PodName != warmPod.Name ||
		observations[0].PodUID != warmPod.UID {
		t.Fatalf("warm delete observation = %#v", observations)
	}
}

func TestKubernetesReconcilerConfirmsMissingRegisteredPodAfterSuccessfulList(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	pool := fixture.createWarmPool(t, placement.CapacityClassInteractive, 1, 1, placement.PoolStatusActive)
	if err := fixture.db.Model(&persistence.AgentExecution{}).
		Where("execution_target_id = ?", fixture.targetID).
		Update("status", "completed").Error; err != nil {
		t.Fatal(err)
	}
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	warmPod := onlyFakePod(t, client)
	warmPod.Phase = "Running"
	client.pods[warmPod.Name] = warmPod
	fixture.registerWarmWorker(t, pool, warmPod, "draining", "active", nil, nil)
	delete(client.pods, warmPod.Name)

	observations := make([]KubernetesWorkerPodObservation, 0)
	fixture.reconciler.config.ObserveWorkerPod = func(_ context.Context, observation KubernetesWorkerPodObservation) error {
		observations = append(observations, observation)
		return nil
	}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(observations) != 1 || observations[0].PodName != warmPod.Name ||
		observations[0].PodUID != warmPod.UID || observations[0].Phase != "Missing" ||
		observations[0].Reason != "confirmed-missing:reconcile-list" {
		t.Fatalf("confirmed missing observations = %#v", observations)
	}
}

func TestKubernetesWarmPodPlansBackfillIdleSlotAfterClaim(t *testing.T) {
	pool := kubernetesWarmPool{
		ID: uuid.New(), Version: 1, CapacityClass: placement.CapacityClassInteractive,
		DesiredIdleUnits: 2, MinIdleUnits: 1, MaxActiveUnits: 3, SchedulingTemplate: map[string]any{},
		Status: placement.PoolStatusActive,
	}
	guaranteed, bestEffort, plansByName, err := kubernetesWarmPodPlans(
		[]kubernetesWarmPool{pool},
		true,
		map[uuid.UUID]int{pool.ID: 1},
		kubernetesWarmReleaseSelection{},
		strings.Repeat("a", 64),
		"ghcr.io/synara/worker:latest",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(guaranteed) != 1 || guaranteed[0].Slot != 0 {
		t.Fatalf("guaranteed warm plans = %#v, want slot 0", guaranteed)
	}
	if len(bestEffort) != 2 || bestEffort[0].Slot != 1 || bestEffort[1].Slot != 2 {
		t.Fatalf("best-effort warm plans = %#v, want slots 1 and 2", bestEffort)
	}
	if len(plansByName) != 3 {
		t.Fatalf("warm plan name index = %#v, want all three stable plans", plansByName)
	}
}

func TestKubernetesReconcilerAppliesSelectedPoolSchedulingTemplateToColdFallback(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	configuration := kubernetesTestConfiguration("")
	configuration["maxActivePods"] = 1
	fixture.updateConfiguration(t, configuration)
	pool := fixture.createWarmPool(t, placement.CapacityClassInteractive, 0, 1, placement.PoolStatusActive)
	pool.ClusterID = kubernetesLocalClusterID
	pool.Namespace = "synara-test"
	pool.SchedulingTemplate = map[string]any{
		"priorityClassName": "interactive-high",
		"preemptionPolicy":  "Never",
		"nodeSelector":      map[string]any{"pool": "warm"},
		"tolerations":       []any{map[string]any{"key": "warm", "operator": "Exists"}},
	}
	nextPoolVersion := pool.Version + 1
	if err := fixture.db.Model(&persistence.WorkerPool{}).Where("id = ? AND version = ?", pool.ID, pool.Version).Updates(&persistence.WorkerPool{
		ClusterID:          pool.ClusterID,
		Namespace:          pool.Namespace,
		SchedulingTemplate: pool.SchedulingTemplate,
		Version:            nextPoolVersion,
		UpdatedAt:          time.Now().UTC(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	pool.Version = nextPoolVersion
	fixture.assignExecutionToPool(t, fixture.executionIDs[0], pool, "queued")
	if err := fixture.db.Model(&persistence.AgentExecution{}).Where("id = ?", fixture.executionIDs[1]).
		Update("status", "completed").Error; err != nil {
		t.Fatal(err)
	}
	client := newFakeKubernetesClient()
	client.priorityClasses["interactive-high"] = kubernetesPriorityClass{
		Name: "interactive-high", PreemptionPolicy: placement.KubernetesPreemptionPolicyNever,
	}
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}

	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	pod := client.lastKind("Pod")
	spec := pod["spec"].(map[string]any)
	if spec["priorityClassName"] != "interactive-high" {
		t.Fatalf("priorityClassName = %#v", spec["priorityClassName"])
	}
	if spec["preemptionPolicy"] != placement.KubernetesPreemptionPolicyNever {
		t.Fatalf("preemptionPolicy = %#v", spec["preemptionPolicy"])
	}
	if client.priorityClassReadCount["interactive-high"] != 1 {
		t.Fatalf("PriorityClass reads = %#v", client.priorityClassReadCount)
	}
	nodeSelector, ok := spec["nodeSelector"].(map[string]string)
	if !ok || nodeSelector["pool"] != "warm" {
		t.Fatalf("nodeSelector = %#v", spec["nodeSelector"])
	}
	tolerations, ok := spec["tolerations"].([]any)
	if !ok || len(tolerations) != 1 {
		t.Fatalf("tolerations = %#v", spec["tolerations"])
	}
	toleration, ok := tolerations[0].(map[string]any)
	if !ok || toleration["key"] != "warm" || toleration["operator"] != "Exists" {
		t.Fatalf("toleration = %#v", tolerations[0])
	}
}

func TestKubernetesReconcilerRejectsPreemptingPriorityClassBeforePodApply(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	client := newFakeKubernetesClient()
	client.priorityClasses[kubernetesWorkerDefaultPriorityClassName] = kubernetesPriorityClass{
		Name: kubernetesWorkerDefaultPriorityClassName, PreemptionPolicy: "PreemptLowerPriority",
	}
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}

	err := fixture.reconciler.ReconcileOnce(context.Background())
	assertExecutionTargetProblemCode(t, err, "worker_pool_preemption_unsupported")
	if client.kindCount("Pod") != 0 || len(client.pods) != 0 {
		t.Fatalf("preempting PriorityClass reached Pod apply: applied=%d active=%#v", client.kindCount("Pod"), client.pods)
	}
	if client.priorityClassReadCount[kubernetesWorkerDefaultPriorityClassName] != 1 {
		t.Fatalf("PriorityClass reads = %#v", client.priorityClassReadCount)
	}
}

func TestKubernetesReconcilerRejectsStaleExecutionPoolSnapshot(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	pool := fixture.createWarmPool(t, placement.CapacityClassInteractive, 0, 1, placement.PoolStatusActive)
	fixture.assignExecutionToPool(t, fixture.executionIDs[0], pool, "queued")
	if err := fixture.db.Model(&persistence.WorkerPool{}).Where("id = ? AND version = ?", pool.ID, pool.Version).Updates(map[string]any{
		"version":    pool.Version + 1,
		"updated_at": time.Now().UTC(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.AgentExecution{}).Where("id = ?", fixture.executionIDs[1]).
		Update("status", "completed").Error; err != nil {
		t.Fatal(err)
	}
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}

	err := fixture.reconciler.ReconcileOnce(context.Background())
	assertExecutionTargetProblemCode(t, err, "worker_pool_assignment_mismatch")
}

func TestKubernetesReconcilerClearsAuthoritativelyInvalidRegistryCredential(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	configuration := kubernetesTestConfiguration("")
	configuration["image"] = "ghcr.io/synara/worker:mutable"
	fixture.updateConfiguration(t, configuration)
	credential := &ImagePullCredential{
		BindingID: uuid.New(), CredentialID: uuid.New(), CredentialVersion: 1,
		Host: "ghcr.io", Username: "synara", Password: "registry-password-secret",
	}
	fixture.reconciler.config.ResolveImagePull = func(context.Context, uuid.UUID, uuid.UUID, string) (ImagePullCredentialResolution, error) {
		return ImagePullCredentialResolution{Credential: credential, Authoritative: true}, nil
	}
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	initialPods := len(client.pods)
	fixture.reconciler.config.ResolveImagePull = func(context.Context, uuid.UUID, uuid.UUID, string) (ImagePullCredentialResolution, error) {
		return ImagePullCredentialResolution{Authoritative: true}, problem.New(
			409, "worker_image_pull_credential_unavailable", "Worker image pull Credential is revoked or expired.",
		)
	}
	err := fixture.reconciler.ReconcileOnce(context.Background())
	assertExecutionTargetProblemCode(t, err, "worker_image_pull_credential_unavailable")
	if len(client.pods) != initialPods {
		t.Fatalf("authoritative Credential failure created a new Pod: before=%d after=%d", initialPods, len(client.pods))
	}
	secret := client.namedKind("Secret", kubernetesRegistrySecretName(fixture.targetID))
	if auths := kubernetesRegistrySecretAuths(t, secret); len(auths) != 0 {
		t.Fatalf("revoked registry Secret retained auths: %#v", auths)
	}
	if bytes.Contains(mustJSON(t, client.applied), []byte("registry-password-secret")) {
		// The first valid Secret necessarily remains in the fake client's request
		// history. The latest named Secret above is the authoritative cluster state.
		latest := client.namedKind("Secret", kubernetesRegistrySecretName(fixture.targetID))
		if bytes.Contains(mustJSON(t, latest), []byte("registry-password-secret")) {
			t.Fatal("cleared Registry Secret still contains the revoked password")
		}
	}
}

func TestKubernetesReconcilerPreservesRegistrySecretOnTransientResolutionFailure(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	configuration := kubernetesTestConfiguration("")
	configuration["image"] = "ghcr.io/synara/worker:mutable"
	fixture.updateConfiguration(t, configuration)
	credential := &ImagePullCredential{
		BindingID: uuid.New(), CredentialID: uuid.New(), CredentialVersion: 1,
		Host: "ghcr.io", Username: "synara", Password: "registry-password-secret",
	}
	fixture.reconciler.config.ResolveImagePull = func(context.Context, uuid.UUID, uuid.UUID, string) (ImagePullCredentialResolution, error) {
		return ImagePullCredentialResolution{Credential: credential, Authoritative: true}, nil
	}
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	applied := len(client.applied)
	fixture.reconciler.config.ResolveImagePull = func(context.Context, uuid.UUID, uuid.UUID, string) (ImagePullCredentialResolution, error) {
		return ImagePullCredentialResolution{}, problem.New(503, "credential_decryption_failed", "Credential KMS is temporarily unavailable.")
	}
	err := fixture.reconciler.ReconcileOnce(context.Background())
	assertExecutionTargetProblemCode(t, err, "credential_decryption_failed")
	if len(client.applied) != applied {
		t.Fatalf("transient Credential failure mutated Kubernetes resources: before=%d after=%d", applied, len(client.applied))
	}
	secret := client.namedKind("Secret", kubernetesRegistrySecretName(fixture.targetID))
	if auths := kubernetesRegistrySecretAuths(t, secret); len(auths) != 1 {
		t.Fatalf("transient Credential failure cleared registry auths: %#v", auths)
	}
}

func TestKubernetesReconcilerRejectsBearerRegistryCredentialAndClearsSecret(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	configuration := kubernetesTestConfiguration("")
	configuration["image"] = "ghcr.io/synara/worker:mutable"
	fixture.updateConfiguration(t, configuration)
	fixture.reconciler.config.ResolveImagePull = func(context.Context, uuid.UUID, uuid.UUID, string) (ImagePullCredentialResolution, error) {
		return ImagePullCredentialResolution{
			Credential: &ImagePullCredential{
				BindingID: uuid.New(), CredentialID: uuid.New(), CredentialVersion: 1,
				Host: "ghcr.io", RegistryToken: "registry-bearer-secret",
			},
			Authoritative: true,
		}, nil
	}
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	err := fixture.reconciler.ReconcileOnce(context.Background())
	assertExecutionTargetProblemCode(t, err, "worker_image_pull_bearer_unsupported")
	if len(client.pods) != 0 {
		t.Fatalf("unsupported bearer Credential created Pods: %#v", client.pods)
	}
	secret := client.namedKind("Secret", kubernetesRegistrySecretName(fixture.targetID))
	if auths := kubernetesRegistrySecretAuths(t, secret); len(auths) != 0 {
		t.Fatalf("unsupported bearer Credential materialized auths: %#v", auths)
	}
	if bytes.Contains(mustJSON(t, client.applied), []byte("registry-bearer-secret")) {
		t.Fatal("unsupported bearer token leaked into a Kubernetes object")
	}
}

func TestKubernetesRegistrySecretUsesCanonicalDockerHubKey(t *testing.T) {
	target := persistence.ExecutionTarget{ID: uuid.New()}
	secret, err := kubernetesRegistrySecret(target, "synara-test", map[string]string{}, &ImagePullCredential{
		Host: "docker.io", Username: "synara", Password: "registry-password-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	auths := kubernetesRegistrySecretAuths(t, secret)
	if _, found := auths["https://index.docker.io/v1/"]; !found || len(auths) != 1 {
		t.Fatalf("Docker Hub registry Secret auths = %#v", auths)
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
	if err := client.DeletePod(context.Background(), "synara-test", "worker", uuid.NewString()); !errors.Is(err, errKubernetesPodUIDPreconditionFailed) {
		t.Fatalf("stale Pod UID delete error = %v, want UID precondition failure", err)
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
	userID         uuid.UUID
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
	configuration := kubernetesTestConfiguration(gitCachePersistentVolumeClaim)
	target, err := targetService.Create(ctx, principal, domain.TenantID, CreateInput{
		OrganizationID: &domain.OrganizationID, Kind: "kubernetes", Name: "managed-kubernetes",
		Configuration: configuration,
		Capabilities:  map[string]any{"workspaceModes": []string{"local", "worktree"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	projectID := uuid.New()
	sessionIDs := []uuid.UUID{uuid.New(), uuid.New()}
	executionIDs := []uuid.UUID{uuid.New(), uuid.New()}
	now := time.Now().UTC()
	models := []any{
		&persistence.Project{ID: projectID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID, Name: "Kubernetes project", DefaultBranch: "main", Visibility: "organization", CreatedBy: domain.UserID},
	}
	for index, executionID := range executionIDs {
		turnID := uuid.New()
		sessionID := sessionIDs[index]
		models = append(models,
			&persistence.AgentSession{ID: sessionID, TenantID: domain.TenantID, OrganizationID: domain.OrganizationID, ProjectID: projectID, CreatedBy: domain.UserID, Title: fmt.Sprintf("Kubernetes session %d", index), Status: "active", Visibility: "organization", Provider: "codex", ExecutionTargetID: target.ID},
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
		PublicControlPlaneURL:  "http://control-plane.test:3780",
		WorkerLeaseTTL:         6 * time.Second,
		WorkerHeartbeatTimeout: 90 * time.Second,
	}, slog.Default())
	return kubernetesReconcileFixture{
		db: store.DB(), reconciler: reconciler, tenantID: domain.TenantID, organizationID: domain.OrganizationID,
		projectID: projectID, sessionID: sessionIDs[0], targetID: target.ID, userID: domain.UserID,
		executionIDs: executionIDs,
	}
}

func kubernetesTestConfiguration(gitCachePersistentVolumeClaim string) map[string]any {
	configuration := map[string]any{
		"apiServer": "https://kubernetes.example.com", "bearerToken": "kubernetes-api-token",
		"caCertificate": "fake-ca-for-client-factory", "namespace": "synara-test", "manageNamespace": true,
		"image": "synara-agentd:test", "imagePullPolicy": "IfNotPresent",
		"controlPlaneUrl": "http://control-plane.test:3780", "allowInsecureControlPlane": true,
		"runnerCommand": []string{"provider-host", "run", "--jsonl"}, "maxActivePods": 1,
		"egressCidrs": []string{"0.0.0.0/0"}, "cpuRequest": "250m", "cpuLimit": "1", "pidsLimit": 512,
		"memoryRequest": "256Mi", "memoryLimit": "1Gi",
		"ephemeralStorageRequest": "512Mi", "ephemeralStorageLimit": "2Gi", "workspaceSizeLimit": "2Gi",
		"quotaCpuRequests": "1", "quotaCpuLimits": "2", "quotaMemoryRequests": "2Gi", "quotaMemoryLimits": "4Gi",
	}
	if gitCachePersistentVolumeClaim != "" {
		configuration["gitCachePersistentVolumeClaim"] = gitCachePersistentVolumeClaim
	}
	return configuration
}

func containsAnyString(values []any, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func assertKubernetesContainerResourceLimits(t *testing.T, container map[string]any) {
	t.Helper()
	resources, ok := container["resources"].(map[string]any)
	if !ok {
		t.Fatalf("Kubernetes container resources = %#v", container["resources"])
	}
	limits, ok := resources["limits"].(map[string]any)
	if !ok || limits["cpu"] != "1" || limits["memory"] != "1Gi" || limits["ephemeral-storage"] != "2Gi" {
		t.Fatalf("Kubernetes container resource limits = %#v", resources["limits"])
	}
}

func (f kubernetesReconcileFixture) updateConfiguration(t *testing.T, configuration map[string]any) {
	t.Helper()
	if backend, _ := configuration["allocationBackend"].(string); strings.HasPrefix(backend, "sandbox-operator-") {
		if _, configured := configuration["sandboxAllowedTenantIds"]; !configured {
			configuration["sandboxAllowedTenantIds"] = []string{f.tenantID.String()}
		}
	}
	encrypted, err := encryptConfiguration(f.reconciler.targets.cipher, configuration)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.Model(&persistence.ExecutionTarget{}).Where("id = ?", f.targetID).
		Update("configuration_encrypted", encrypted).Error; err != nil {
		t.Fatal(err)
	}
}

func (f kubernetesReconcileFixture) seedReleaseRevision(t *testing.T, revision int64, imageDigest string) uuid.UUID {
	t.Helper()
	manifestID := uuid.New()
	manifest := persistence.WorkerManifest{
		ID: manifestID, ManifestHash: fmt.Sprintf("%064x", revision+100),
		WorkerBuildVersion:    fmt.Sprintf("kubernetes-release-%d", revision),
		WorkerProtocolMinimum: 2, WorkerProtocolMaximum: 2, RuntimeEventMinimum: 2, RuntimeEventMaximum: 2,
		OperatingSystem: "linux", Architecture: "amd64", ImageDigest: &imageDigest,
		FeatureFlags: map[string]any{}, CreatedAt: time.Now().UTC(),
	}
	if err := f.db.Create(&manifest).Error; err != nil {
		t.Fatal(err)
	}
	revisionID := uuid.New()
	model := persistence.WorkerReleaseRevision{
		ID: revisionID, TenantID: f.tenantID, ExecutionTargetID: f.targetID,
		Revision: revision, WorkerManifestID: manifestID, Description: "Kubernetes release test",
		CreatedBy: f.userID, CreatedAt: time.Now().UTC(),
	}
	if err := f.db.Create(&model).Error; err != nil {
		t.Fatal(err)
	}
	return revisionID
}

func (f kubernetesReconcileFixture) seedReleasePolicy(
	t *testing.T,
	promotedRevision uuid.UUID,
	canaryRevision *uuid.UUID,
	canaryPercent int,
) {
	t.Helper()
	var existing persistence.WorkerReleasePolicy
	err := f.db.Where("execution_target_id = ?", f.targetID).Take(&existing).Error
	if err == nil {
		updated := map[string]any{
			"policy_version":       existing.PolicyVersion + 1,
			"promoted_revision_id": promotedRevision,
			"canary_revision_id":   canaryRevision,
			"canary_percent":       canaryPercent,
			"updated_by":           f.userID,
			"updated_at":           time.Now().UTC(),
		}
		if err := f.db.Model(&persistence.WorkerReleasePolicy{}).
			Where("execution_target_id = ?", f.targetID).
			Updates(updated).Error; err != nil {
			t.Fatal(err)
		}
		return
	}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatal(err)
	}
	if err := f.db.Create(&persistence.WorkerReleasePolicy{
		TenantID: f.tenantID, ExecutionTargetID: f.targetID, PolicyVersion: 1,
		PromotedRevisionID: promotedRevision, CanaryRevisionID: canaryRevision, CanaryPercent: canaryPercent,
		UpdatedBy: f.userID, UpdatedAt: time.Now().UTC(),
	}).Error; err != nil {
		t.Fatal(err)
	}
}

func (f kubernetesReconcileFixture) createWarmPool(
	t *testing.T,
	capacityClass string,
	desiredIdleUnits, maxActiveUnits int,
	status string,
) persistence.WorkerPool {
	return f.createWarmPoolWithMinIdle(t, capacityClass, desiredIdleUnits, 0, maxActiveUnits, status)
}

func (f kubernetesReconcileFixture) createWarmPoolWithMinIdle(
	t *testing.T,
	capacityClass string,
	desiredIdleUnits, minIdleUnits, maxActiveUnits int,
	status string,
) persistence.WorkerPool {
	t.Helper()
	now := time.Now().UTC()
	pool := persistence.WorkerPool{
		ID: uuid.New(), TenantID: &f.tenantID, ExecutionTargetID: f.targetID,
		Name: "warm-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:8],
		Mode: placement.PoolModeWarm, CapacityClass: capacityClass,
		DesiredIdleUnits: desiredIdleUnits, MinIdleUnits: minIdleUnits, MaxActiveUnits: maxActiveUnits,
		SchedulingTemplate: map[string]any{}, Status: status, Version: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := f.db.Create(&pool).Error; err != nil {
		t.Fatal(err)
	}
	return pool
}

func (f kubernetesReconcileFixture) assignExecutionToPool(
	t *testing.T,
	executionID uuid.UUID,
	pool persistence.WorkerPool,
	status string,
) {
	t.Helper()
	updates := map[string]any{
		"worker_pool_id":      pool.ID,
		"worker_pool_version": pool.Version,
		"capacity_class":      pool.CapacityClass,
	}
	if status != "" {
		updates["status"] = status
	}
	if err := f.db.Model(&persistence.AgentExecution{}).Where("id = ?", executionID).Updates(updates).Error; err != nil {
		t.Fatal(err)
	}
}

func (f kubernetesReconcileFixture) registerWarmWorker(
	t *testing.T,
	pool persistence.WorkerPool,
	pod kubernetesPod,
	status string,
	administrativeStatus string,
	releaseRevisionID *uuid.UUID,
	releaseChannel *string,
) persistence.WorkerInstance {
	t.Helper()
	clusterID := strings.TrimSpace(pool.ClusterID)
	if clusterID == "" {
		clusterID = kubernetesLocalClusterID
	}
	namespace := strings.TrimSpace(pool.Namespace)
	if namespace == "" {
		namespace = "synara-test"
	}
	now := time.Now().UTC()
	manifestID := uuid.New()
	if err := f.db.Create(&persistence.WorkerManifest{
		ID: manifestID, ManifestHash: strings.ReplaceAll(manifestID.String(), "-", "") + strings.Repeat("0", 32),
		WorkerBuildVersion: "warm-worker", WorkerProtocolMinimum: kubernetesWorkerProtocolVersion,
		WorkerProtocolMaximum: kubernetesWorkerProtocolVersion, RuntimeEventMinimum: 2, RuntimeEventMaximum: 2,
		OperatingSystem: "linux", Architecture: "amd64", FeatureFlags: map[string]any{}, CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	worker := persistence.WorkerInstance{
		ID: uuid.New(), Incarnation: 1, InstanceUID: pod.UID, ExecutionTargetID: f.targetID,
		TargetKind: "kubernetes", WorkerMode: kubernetesWorkerModeWarmPool,
		WorkerPoolID: &pool.ID, WorkerPoolVersion: &pool.Version, CapacityClass: &pool.CapacityClass,
		RegistrationTrustMode:   kubernetesPodBoundRegistrationTrust,
		ClusterID:               clusterID,
		Namespace:               namespace,
		PodName:                 pod.Name,
		Version:                 "warm-worker",
		ProtocolVersion:         kubernetesWorkerProtocolVersion,
		Capabilities:            map[string]any{},
		CurrentManifestID:       &manifestID,
		CompatibilityStatus:     "compatible",
		CompatibilityCheckedAt:  &now,
		WorkerReleaseRevisionID: releaseRevisionID,
		WorkerReleaseChannel:    releaseChannel,
		LeaseSupported:          true,
		FencingSupported:        true,
		AuthTokenHash:           []byte("warm-worker-hash"),
		Status:                  status,
		AdministrativeStatus:    administrativeStatus,
		WorkerReleaseStatus:     "unmanaged",
		RegisteredAt:            now,
		LastHeartbeatAt:         now,
	}
	if releaseRevisionID != nil {
		worker.WorkerReleaseStatus = "active"
		worker.WorkerReleaseCheckedAt = &now
	}
	if err := f.db.Create(&worker).Error; err != nil {
		t.Fatal(err)
	}
	return worker
}

func (f kubernetesReconcileFixture) createWarmWorkerLease(
	t *testing.T,
	worker persistence.WorkerInstance,
	executionID uuid.UUID,
) {
	t.Helper()
	now := time.Now().UTC()
	if err := f.db.Create(&persistence.WorkerLease{
		ExecutionID:       executionID,
		TenantID:          f.tenantID,
		WorkerID:          worker.ID,
		WorkerIncarnation: worker.Incarnation,
		WorkerInstanceUID: worker.InstanceUID,
		Generation:        1,
		LeaseTokenHash:    []byte("lease-token"),
		AcquiredAt:        now,
		HeartbeatAt:       now,
		ExpiresAt:         now.Add(5 * time.Minute),
	}).Error; err != nil {
		t.Fatal(err)
	}
}

func onlyFakePod(t *testing.T, client *fakeKubernetesClient) kubernetesPod {
	t.Helper()
	if len(client.pods) != 1 {
		t.Fatalf("expected exactly one active Pod, got %#v", client.pods)
	}
	for _, pod := range client.pods {
		return pod
	}
	t.Fatal("expected one active Pod")
	return kubernetesPod{}
}

func findExecutionPod(client *fakeKubernetesClient, executionID uuid.UUID) (kubernetesPod, bool) {
	for _, pod := range client.pods {
		if pod.Labels[kubernetesExecutionLabel] == executionID.String() {
			return pod, true
		}
	}
	return kubernetesPod{}, false
}

type fakeKubernetesFactory struct {
	client kubernetesClient
}

func (f *fakeKubernetesFactory) Open(kubernetesTargetConfiguration) (kubernetesClient, error) {
	return f.client, nil
}

type fakeKubernetesClient struct {
	applied                 []map[string]any
	pods                    map[string]kubernetesPod
	priorityClasses         map[string]kubernetesPriorityClass
	priorityClassReadErr    error
	priorityClassReadCount  map[string]int
	resourceQuota           kubernetesResourceQuota
	resourceQuotaReadErr    error
	deletedPods             []string
	podApplyErr             error
	podApplyErrFor          map[string]error
	listPodUIDsErr          error
	deletePodErr            error
	pidsLimitAttestationErr error
}

func newFakeKubernetesClient() *fakeKubernetesClient {
	return &fakeKubernetesClient{
		pods: map[string]kubernetesPod{},
		priorityClasses: map[string]kubernetesPriorityClass{
			kubernetesWorkerDefaultPriorityClassName: {
				Name: kubernetesWorkerDefaultPriorityClassName, PreemptionPolicy: placement.KubernetesPreemptionPolicyNever,
			},
		},
		priorityClassReadCount: map[string]int{},
	}
}

func (c *fakeKubernetesClient) AttestPodPIDsLimit(
	_ context.Context,
	_ map[string]string,
	_ uint64,
	_ string,
) error {
	return c.pidsLimitAttestationErr
}

func (c *fakeKubernetesClient) Apply(_ context.Context, _ string, object map[string]any) error {
	if object["kind"] == "Pod" {
		if c.podApplyErr != nil {
			return c.podApplyErr
		}
		if len(c.podApplyErrFor) > 0 {
			name, _ := object["metadata"].(map[string]any)["name"].(string)
			if err, found := c.podApplyErrFor[name]; found {
				return err
			}
		}
	}
	c.applied = append(c.applied, object)
	if object["kind"] == "Pod" {
		metadata := object["metadata"].(map[string]any)
		name := metadata["name"].(string)
		labels := metadata["labels"].(map[string]string)
		annotations := map[string]string{}
		if raw, ok := metadata["annotations"].(map[string]any); ok {
			for key, value := range raw {
				annotations[key], _ = value.(string)
			}
		}
		resourceRequests := map[string]string{}
		if spec, ok := object["spec"].(map[string]any); ok {
			if containers, ok := spec["containers"].([]any); ok && len(containers) > 0 {
				if container, ok := containers[0].(map[string]any); ok {
					if resources, ok := container["resources"].(map[string]any); ok {
						if requests, ok := resources["requests"].(map[string]any); ok {
							for key, value := range requests {
								resourceRequests[key], _ = value.(string)
							}
						}
					}
				}
			}
		}
		c.pods[name] = kubernetesPod{
			Name: name, UID: uuid.NewString(), Phase: "Pending", CreatedAt: time.Now().UTC(),
			Labels: labels, Annotations: annotations, ResourceRequests: resourceRequests,
		}
	}
	return nil
}

func (c *fakeKubernetesClient) GetResourceQuota(_ context.Context, _, _ string) (kubernetesResourceQuota, error) {
	if c.resourceQuotaReadErr != nil {
		return kubernetesResourceQuota{}, c.resourceQuotaReadErr
	}
	return c.resourceQuota, nil
}

func (c *fakeKubernetesClient) GetPriorityClass(_ context.Context, name string) (kubernetesPriorityClass, error) {
	c.priorityClassReadCount[name]++
	if c.priorityClassReadErr != nil {
		return kubernetesPriorityClass{}, c.priorityClassReadErr
	}
	priorityClass, found := c.priorityClasses[name]
	if !found {
		return kubernetesPriorityClass{}, &kubernetesAPIStatusError{StatusCode: http.StatusNotFound, Detail: "PriorityClass not found"}
	}
	return priorityClass, nil
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
	if c.deletePodErr != nil {
		return c.deletePodErr
	}
	pod, found := c.pods[name]
	if found && pod.UID != uid {
		return fmt.Errorf("%w: current=%s requested=%s", errKubernetesPodUIDPreconditionFailed, pod.UID, uid)
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

func (c *fakeKubernetesClient) namedKind(kind, name string) map[string]any {
	for index := len(c.applied) - 1; index >= 0; index-- {
		object := c.applied[index]
		if object["kind"] != kind {
			continue
		}
		metadata, _ := object["metadata"].(map[string]any)
		if metadata["name"] == name {
			return object
		}
	}
	return nil
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func kubernetesRegistrySecretAuths(t *testing.T, secret map[string]any) map[string]any {
	t.Helper()
	if secret == nil {
		t.Fatal("Kubernetes Registry Secret was not applied")
	}
	data, ok := secret["data"].(map[string]any)
	if !ok {
		t.Fatalf("Kubernetes Registry Secret data = %#v", secret["data"])
	}
	encoded, ok := data[".dockerconfigjson"].(string)
	if !ok {
		t.Fatalf("Kubernetes Registry Secret docker config = %#v", data)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Auths map[string]any `json:"auths"`
	}
	if err := json.Unmarshal(decoded, &config); err != nil {
		t.Fatal(err)
	}
	return config.Auths
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

func TestKubernetesPodCompletedSuccessfullyRequiresExactAgentdTermination(t *testing.T) {
	tests := []struct {
		name string
		pod  kubernetesPod
		want bool
	}{
		{
			name: "succeeded-agentd-zero",
			pod: kubernetesPod{Phase: "Succeeded", Containers: []kubernetesContainerStatus{{
				Name: "agentd", Terminated: true, ExitCode: 0,
			}}},
			want: true,
		},
		{
			name: "phase-only-is-not-proof",
			pod:  kubernetesPod{Phase: "Succeeded"},
		},
		{
			name: "failed-phase",
			pod: kubernetesPod{Phase: "Failed", Containers: []kubernetesContainerStatus{{
				Name: "agentd", Terminated: true, ExitCode: 0,
			}}},
		},
		{
			name: "nonzero-exit",
			pod: kubernetesPod{Phase: "Succeeded", Containers: []kubernetesContainerStatus{{
				Name: "agentd", Terminated: true, ExitCode: 1,
			}}},
		},
		{
			name: "unexpected-sidecar",
			pod: kubernetesPod{Phase: "Succeeded", Containers: []kubernetesContainerStatus{
				{Name: "agentd", Terminated: true, ExitCode: 0},
				{Name: "sidecar", Terminated: true, ExitCode: 0},
			}},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			if got := kubernetesPodCompletedSuccessfully(testCase.pod); got != testCase.want {
				t.Fatalf("completed successfully = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestClassifyKubernetesExecutionPodFailureUsesStableLowCardinalityClasses(t *testing.T) {
	tests := []struct {
		name       string
		pod        kubernetesPod
		wantClass  string
		wantReason string
	}{
		{
			name: "unschedulable",
			pod: kubernetesPod{Phase: "Pending", Conditions: []kubernetesPodCondition{{
				Type: "PodScheduled", Status: "False", Reason: "Unschedulable",
			}}},
			wantClass: KubernetesPodFailureUnschedulable, wantReason: "unschedulable",
		},
		{
			name: "image-pull-backoff",
			pod: kubernetesPod{Phase: "Pending", Containers: []kubernetesContainerStatus{{
				Name: "agentd", WaitingReason: "ImagePullBackOff",
			}}},
			wantClass: KubernetesPodFailureImagePull, wantReason: "image-pull-backoff",
		},
		{
			name: "container-config",
			pod: kubernetesPod{Phase: "Pending", Containers: []kubernetesContainerStatus{{
				Name: "agentd", WaitingReason: "CreateContainerConfigError",
			}}},
			wantClass: KubernetesPodFailureContainerStart, wantReason: "create-container-config-error",
		},
		{
			name:      "evicted",
			pod:       kubernetesPod{Phase: "Failed", Reason: "Evicted"},
			wantClass: KubernetesPodFailureEvicted, wantReason: "evicted",
		},
		{
			name: "oom-current-state",
			pod: kubernetesPod{Phase: "Failed", Containers: []kubernetesContainerStatus{{
				Name: "agentd", Terminated: true, TerminatedReason: "OOMKilled", ExitCode: 137,
			}}},
			wantClass: KubernetesPodFailureOOMKilled, wantReason: "oom-killed",
		},
		{
			name: "oom-last-state",
			pod: kubernetesPod{Phase: "Running", Containers: []kubernetesContainerStatus{{
				Name: "agentd", LastTerminatedReason: "OOMKilled",
			}}},
			wantClass: KubernetesPodFailureOOMKilled, wantReason: "oom-killed",
		},
		{
			name:      "generic-failed",
			pod:       kubernetesPod{Phase: "Failed", Reason: "TenantControlledRawMessage"},
			wantClass: KubernetesPodFailureGeneric, wantReason: "phase-failed",
		},
		{name: "healthy-running", pod: kubernetesPod{Phase: "Running"}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			failureClass, reason := classifyKubernetesExecutionPodFailure(testCase.pod)
			if failureClass != testCase.wantClass || reason != testCase.wantReason {
				t.Fatalf("classification = %q/%q, want %q/%q", failureClass, reason, testCase.wantClass, testCase.wantReason)
			}
		})
	}
}

func TestKubernetesClientParsesPodFailureEvidence(t *testing.T) {
	targetID := uuid.New()
	podUID := uuid.NewString()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(writer, `{
			"metadata":{"continue":""},
			"items":[{
				"metadata":{"name":"failed-worker","uid":%q,"labels":{"synara.io/execution-target-id":%q},"ownerReferences":[{"kind":"Sandbox","uid":"sandbox-uid","controller":true}]},
				"status":{
					"phase":"Failed","reason":"Evicted",
					"conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable"}],
					"containerStatuses":[{
						"name":"agentd",
						"state":{"terminated":{"exitCode":137,"reason":"OOMKilled"}},
						"lastState":{"terminated":{"reason":"OOMKilled"}}
					}]
				}
			}]
		}`, podUID, targetID.String())
	}))
	defer server.Close()
	client := &kubernetesHTTPClient{baseURL: server.URL, token: "test-token", client: server.Client()}
	pods, err := client.ListPods(context.Background(), "synara-test", targetID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 1 || pods[0].Reason != "Evicted" || len(pods[0].Conditions) != 1 ||
		len(pods[0].Containers) != 1 || pods[0].Containers[0].TerminatedReason != "OOMKilled" ||
		pods[0].Containers[0].LastTerminatedReason != "OOMKilled" ||
		pods[0].ControllerOwnerKind != "Sandbox" || pods[0].ControllerOwnerUID != "sandbox-uid" {
		t.Fatalf("parsed Kubernetes Pod evidence = %#v", pods)
	}
	if failureClass, reason := classifyKubernetesExecutionPodFailure(pods[0]); failureClass != KubernetesPodFailureEvicted || reason != "evicted" {
		t.Fatalf("parsed failure classification = %q/%q", failureClass, reason)
	}
}

func TestKubernetesReconcilerReportsPodApplyFailureBeforeUIDAssignment(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	client := newFakeKubernetesClient()
	client.podApplyErr = &kubernetesAPIStatusError{StatusCode: http.StatusTooManyRequests, Detail: "rate limited"}
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	var observations []KubernetesExecutionPodObservation
	fixture.reconciler.config.ObserveExecutionPod = func(_ context.Context, observation KubernetesExecutionPodObservation) error {
		observations = append(observations, observation)
		return nil
	}
	err := fixture.reconciler.ReconcileOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "A Kubernetes Worker Pod could not be applied") {
		t.Fatalf("reconcile apply failure = %v", err)
	}
	// Every queued Execution is attempted: one Pod that cannot be applied does
	// not stop the sweep, so each failure is classified and observed rather
	// than only the first one encountered.
	if len(observations) != len(fixture.executionIDs) {
		t.Fatalf("apply observations = %#v", observations)
	}
	observed := make(map[uuid.UUID]KubernetesExecutionPodObservation, len(observations))
	for _, observation := range observations {
		if observation.TenantID != fixture.tenantID || observation.ExecutionTargetID != fixture.targetID ||
			observation.Generation != 1 || observation.PodUID != "" || observation.Phase != "ApplyFailed" ||
			observation.FailureClass != KubernetesPodFailureApplyFailed ||
			observation.FailureReasonCode != "api-status-429" {
			t.Fatalf("apply failure observation = %#v", observation)
		}
		observed[observation.ExecutionID] = observation
	}
	for _, executionID := range fixture.executionIDs {
		if _, found := observed[executionID]; !found {
			t.Fatalf("execution %s produced no apply failure observation", executionID)
		}
	}
}

func TestKubernetesReconcilerReportsFailedPodBeforeSafeDeletion(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	pod, found := findExecutionPod(client, fixture.executionIDs[0])
	if !found {
		t.Fatalf("initial Execution Pod was not created: %#v", client.pods)
	}
	pod.Phase = "Failed"
	pod.Reason = "Evicted"
	client.pods[pod.Name] = pod
	var executionObservations []KubernetesExecutionPodObservation
	var workerObservations []KubernetesWorkerPodObservation
	fixture.reconciler.config.ObserveExecutionPod = func(_ context.Context, observation KubernetesExecutionPodObservation) error {
		executionObservations = append(executionObservations, observation)
		return nil
	}
	fixture.reconciler.config.ObserveWorkerPod = func(_ context.Context, observation KubernetesWorkerPodObservation) error {
		workerObservations = append(workerObservations, observation)
		return nil
	}
	if err := fixture.reconciler.ReconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(executionObservations) != 1 ||
		executionObservations[0].FailureClass != KubernetesPodFailureEvicted ||
		executionObservations[0].FailureReasonCode != "evicted" {
		t.Fatalf("failed Execution Pod observations = %#v", executionObservations)
	}
	if len(workerObservations) < 2 || workerObservations[0].Phase != "Failed" ||
		workerObservations[0].Reason != "terminal-observation:evicted" ||
		!strings.HasPrefix(workerObservations[1].Reason, "delete-requested:") {
		t.Fatalf("failed Worker Pod observations = %#v", workerObservations)
	}
	if len(client.deletedPods) != 1 || client.deletedPods[0] != pod.Name {
		t.Fatalf("failed Pod deletion = %#v", client.deletedPods)
	}
}

// A single Execution that cannot be placed must not starve the other queued
// Executions on the same target for the cycle. Before per-Execution isolation
// the first apply failure aborted the whole target sweep, so a malformed or
// rejected Pod spec silently blocked every Execution behind it.
func TestKubernetesReconcilerIsolatesPodApplyFailurePerExecution(t *testing.T) {
	fixture := newKubernetesReconcileFixture(t, "")
	client := newFakeKubernetesClient()
	fixture.reconciler.factory = &fakeKubernetesFactory{client: client}

	blocked := fixture.executionIDs[0]
	survivor := fixture.executionIDs[1]
	blockedPod := kubernetesPodName(kubernetesExecution{ID: blocked, Status: "queued"})
	survivorPod := kubernetesPodName(kubernetesExecution{ID: survivor, Status: "queued"})
	client.podApplyErrFor = map[string]error{
		blockedPod: &kubernetesAPIStatusError{StatusCode: http.StatusTooManyRequests, Detail: "rate limited"},
	}

	err := fixture.reconciler.ReconcileOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "A Kubernetes Worker Pod could not be applied") {
		t.Fatalf("reconcile did not report the per-Execution apply failure: %v", err)
	}
	// The failure is attributed to the Execution it belongs to.
	if !strings.Contains(err.Error(), blocked.String()) {
		t.Fatalf("apply failure was not attributed to execution %s: %v", blocked, err)
	}

	appliedPods := make(map[string]struct{})
	for _, object := range client.applied {
		if object["kind"] != "Pod" {
			continue
		}
		name, _ := object["metadata"].(map[string]any)["name"].(string)
		appliedPods[name] = struct{}{}
	}
	if _, found := appliedPods[survivorPod]; !found {
		t.Fatalf("a failed Execution starved execution %s: applied=%v", survivor, appliedPods)
	}
	if _, found := appliedPods[blockedPod]; found {
		t.Fatalf("the rejected Pod %s was recorded as applied", blockedPod)
	}
}
