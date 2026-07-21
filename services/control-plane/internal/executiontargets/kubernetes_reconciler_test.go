package executiontargets

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
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
		!bytes.Contains(environment, []byte(`"name":"SYNARA_AGENTD_LEASE_RENEW_INTERVAL","value":"2s"`)) ||
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
	container := pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
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
		RegistrationToken: "kubernetes-registration-secret", PublicControlPlaneURL: "http://control-plane.test:3780",
		WorkerLeaseTTL: 6 * time.Second,
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
		"egressCidrs": []string{"0.0.0.0/0"}, "cpuRequest": "250m", "cpuLimit": "1",
		"memoryRequest": "256Mi", "memoryLimit": "1Gi", "workspaceSizeLimit": "2Gi",
		"quotaCpuRequests": "1", "quotaCpuLimits": "2", "quotaMemoryRequests": "2Gi", "quotaMemoryLimits": "4Gi",
	}
	if gitCachePersistentVolumeClaim != "" {
		configuration["gitCachePersistentVolumeClaim"] = gitCachePersistentVolumeClaim
	}
	return configuration
}

func (f kubernetesReconcileFixture) updateConfiguration(t *testing.T, configuration map[string]any) {
	t.Helper()
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
		annotations := map[string]string{}
		if raw, ok := metadata["annotations"].(map[string]any); ok {
			for key, value := range raw {
				annotations[key], _ = value.(string)
			}
		}
		c.pods[name] = kubernetesPod{
			Name: name, UID: uuid.NewString(), Phase: "Pending", Labels: labels, Annotations: annotations,
		}
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
