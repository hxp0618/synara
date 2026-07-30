package executiontargets

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/placement"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/workertiming"
)

func kubernetesPodName(execution kubernetesExecution) string {
	generation := execution.Generation
	if execution.Status == "queued" || execution.Status == "recovering" {
		generation++
	}
	compactID := strings.ReplaceAll(execution.ID.String(), "-", "")
	return "synara-exec-" + compactID[:28] + "-g" + strconv.FormatInt(generation, 16)
}

func kubernetesRegistrationTokenVolumes(targetID uuid.UUID) []any {
	return []any{
		map[string]any{
			"name": kubernetesWorkloadIdentityVolume,
			"projected": map[string]any{
				"defaultMode": 0o440,
				"sources": []any{map[string]any{
					"serviceAccountToken": map[string]any{
						"audience":          KubernetesWorkerRegistrationAudience(targetID),
						"expirationSeconds": 600,
						"path":              "token",
					},
				}},
			},
		},
		map[string]any{"name": kubernetesRegistrationTokenVolume, "emptyDir": map[string]any{}},
	}
}

func kubernetesRegistrationTokenMainMount() map[string]any {
	return map[string]any{
		"name": kubernetesRegistrationTokenVolume, "mountPath": "/var/run/secrets/synara.io/registration",
	}
}

func kubernetesNetworkBoundaryInitContainer(
	image string,
	imagePullPolicy string,
	requests map[string]any,
	limits map[string]any,
) map[string]any {
	return map[string]any{
		"name": kubernetesNetworkBoundaryInitName, "image": image,
		"imagePullPolicy": imagePullPolicy,
		"command": []any{
			"/usr/local/bin/synara-agentd",
			platform.KubernetesNetworkBoundaryVerifyArgument,
		},
		"securityContext": kubernetesRestrictedContainerSecurityContext(),
		"resources":       map[string]any{"requests": requests, "limits": limits},
	}
}

func kubernetesRegistrationTokenInitContainer(
	image string,
	imagePullPolicy string,
	requests map[string]any,
	limits map[string]any,
) map[string]any {
	return map[string]any{
		"name": kubernetesRegistrationTokenInitName, "image": image,
		"imagePullPolicy": imagePullPolicy,
		"command": []any{
			"/usr/local/bin/synara-agentd",
			platform.KubernetesRegistrationTokenStageArgument,
		},
		"volumeMounts": []any{
			map[string]any{
				"name":      kubernetesWorkloadIdentityVolume,
				"mountPath": "/var/run/secrets/synara.io/workload-identity", "readOnly": true,
			},
			map[string]any{
				"name":      kubernetesRegistrationTokenVolume,
				"mountPath": "/var/run/secrets/synara.io/registration",
			},
		},
		"securityContext": kubernetesRestrictedContainerSecurityContext(),
		"resources":       map[string]any{"requests": requests, "limits": limits},
	}
}

func kubernetesRestrictedContainerSecurityContext() map[string]any {
	return map[string]any{
		"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true,
		"runAsNonRoot": true, "runAsUser": 10001, "runAsGroup": 10001,
		"capabilities":   map[string]any{"drop": []any{"ALL"}},
		"seccompProfile": map[string]any{"type": "RuntimeDefault"},
	}
}

func (r *KubernetesReconciler) foundationHash(
	target persistence.ExecutionTarget,
	configuration kubernetesTargetConfiguration,
) (string, error) {
	capabilities, err := json.Marshal(target.Capabilities)
	if err != nil {
		return "", err
	}
	runtimeDecision := configuration.RuntimeIsolationDecision
	if runtimeDecision != nil {
		stableDecision := *runtimeDecision
		// The observation window proves that the attestation is currently fresh,
		// but it does not change the Pod specification. Keep the attestation
		// digest in the hash so an actual runtime or eligible-node identity change
		// still rolls the Pod without recycling it on every reconciliation pass.
		stableDecision.AttestedAt = nil
		stableDecision.AttestationExpiresAt = nil
		runtimeDecision = &stableDecision
	}
	payload, err := json.Marshal(struct {
		Configuration   kubernetesTargetConfiguration
		RuntimeDecision *runtimeIsolationDecision
		Capabilities    json.RawMessage
		LeaseRenew      time.Duration
		PodSpecRevision string
	}{
		Configuration:   configuration,
		RuntimeDecision: runtimeDecision,
		Capabilities:    capabilities,
		LeaseRenew:      workertiming.LeaseRenewInterval(r.config.WorkerLeaseTTL),
		PodSpecRevision: kubernetesWorkerPodSpecRevision,
	})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func (r *KubernetesReconciler) applyFoundation(
	ctx context.Context,
	client kubernetesClient,
	target persistence.ExecutionTarget,
	configuration kubernetesTargetConfiguration,
	credential *ImagePullCredential,
) error {
	labels := kubernetesTargetLabels(target)
	if configuration.ManageNamespace != nil && *configuration.ManageNamespace {
		namespace := map[string]any{
			"apiVersion": "v1", "kind": "Namespace",
			"metadata": map[string]any{
				"name":   configuration.Namespace,
				"labels": kubernetesManagedNamespaceLabels(labels),
			},
		}
		if err := client.Apply(ctx, "/api/v1/namespaces/"+url.PathEscape(configuration.Namespace), namespace); err != nil {
			return problem.Wrap(502, "kubernetes_namespace_apply_failed", "Kubernetes Worker Namespace could not be applied.", err)
		}
	}
	serviceAccount := map[string]any{
		"apiVersion": "v1", "kind": "ServiceAccount",
		"metadata":                     map[string]any{"name": configuration.ServiceAccountName, "namespace": configuration.Namespace, "labels": labels},
		"automountServiceAccountToken": false,
	}
	if err := client.Apply(ctx, kubernetesNamespacedPath(configuration.Namespace, "serviceaccounts", configuration.ServiceAccountName), serviceAccount); err != nil {
		return problem.Wrap(502, "kubernetes_service_account_apply_failed", "Kubernetes Worker ServiceAccount could not be applied.", err)
	}
	registrySecret, err := kubernetesRegistrySecret(target, configuration.Namespace, labels, credential)
	if err != nil {
		return err
	}
	registrySecretName := kubernetesRegistrySecretName(target.ID)
	if err := client.Apply(ctx, kubernetesNamespacedPath(configuration.Namespace, "secrets", registrySecretName), registrySecret); err != nil {
		return problem.Wrap(502, "kubernetes_registry_secret_apply_failed", "Kubernetes Worker Registry Secret could not be applied.", err)
	}
	hard := map[string]any{"pods": strconv.Itoa(configuration.MaxActivePods)}
	for key, value := range map[string]string{
		"requests.cpu": configuration.QuotaCPURequests, "limits.cpu": configuration.QuotaCPULimits,
		"requests.memory": configuration.QuotaMemoryRequests, "limits.memory": configuration.QuotaMemoryLimits,
		"requests.ephemeral-storage": configuration.QuotaEphemeralStorage,
	} {
		if value != "" {
			hard[key] = value
		}
	}
	if configuration.GPUResourceName != "" {
		hard["requests."+configuration.GPUResourceName] = configuration.QuotaGPURequests
		hard["limits."+configuration.GPUResourceName] = configuration.QuotaGPURequests
	}
	quotaName := kubernetesResourceQuotaName(target.ID)
	quota := map[string]any{
		"apiVersion": "v1", "kind": "ResourceQuota",
		"metadata": map[string]any{"name": quotaName, "namespace": configuration.Namespace, "labels": labels},
		"spec":     map[string]any{"hard": hard},
	}
	if err := client.Apply(ctx, kubernetesNamespacedPath(configuration.Namespace, "resourcequotas", quotaName), quota); err != nil {
		return problem.Wrap(502, "kubernetes_quota_apply_failed", "Kubernetes Worker ResourceQuota could not be applied.", err)
	}
	ipBlocks := make([]any, 0, len(configuration.EgressCIDRs))
	for _, cidr := range configuration.EgressCIDRs {
		ipBlocks = append(ipBlocks, map[string]any{"ipBlock": kubernetesEgressIPBlock(strings.TrimSpace(cidr))})
	}
	egressPorts := make([]any, 0, len(configuration.EgressTCPPorts))
	for _, port := range configuration.EgressTCPPorts {
		egressPorts = append(egressPorts, map[string]any{"protocol": "TCP", "port": port})
	}
	networkPolicy := map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
		"metadata": map[string]any{"name": quotaName, "namespace": configuration.Namespace, "labels": labels},
		"spec": map[string]any{
			"podSelector": map[string]any{"matchLabels": map[string]any{kubernetesTargetLabel: target.ID.String()}},
			"policyTypes": []any{"Ingress", "Egress"}, "ingress": []any{},
			"egress": []any{
				map[string]any{
					"to": []any{map[string]any{
						"namespaceSelector": map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": "kube-system"}},
						"podSelector":       map[string]any{"matchLabels": map[string]any{"k8s-app": "kube-dns"}},
					}},
					"ports": []any{map[string]any{"protocol": "UDP", "port": 53}, map[string]any{"protocol": "TCP", "port": 53}},
				},
				map[string]any{"to": ipBlocks, "ports": egressPorts},
			},
		},
	}
	if err := client.Apply(ctx, kubernetesNetworkPolicyPath(configuration.Namespace, quotaName), networkPolicy); err != nil {
		return problem.Wrap(502, "kubernetes_network_policy_apply_failed", "Kubernetes Worker NetworkPolicy could not be applied.", err)
	}
	return nil
}

func kubernetesEgressIPBlock(cidr string) map[string]any {
	result := map[string]any{"cidr": cidr}
	_, allowed, err := net.ParseCIDR(cidr)
	if err != nil {
		return result
	}
	except := make([]any, 0, len(kubernetesForbiddenEgressCIDRs))
	for _, deniedCIDR := range kubernetesForbiddenEgressCIDRs {
		_, denied, _ := net.ParseCIDR(deniedCIDR)
		if cidrContainsCIDR(allowed, denied) && allowed.String() != denied.String() {
			except = append(except, denied.String())
		}
	}
	if len(except) > 0 {
		result["except"] = except
	}
	return result
}

var kubernetesForbiddenEgressCIDRs = []string{
	"169.254.0.0/16",
	"100.100.100.200/32",
	"fe80::/10",
	"fd00:ec2::254/128",
}

func cidrContainsCIDR(parent, child *net.IPNet) bool {
	parentOnes, parentBits := parent.Mask.Size()
	childOnes, childBits := child.Mask.Size()
	return parentBits == childBits && parentOnes <= childOnes && parent.Contains(child.IP)
}

func kubernetesTenantNetworkEnvironment(configuration kubernetesTargetConfiguration) []any {
	result := make([]any, 0, 5)
	if len(configuration.PrivateNetworkCIDRs) > 0 {
		encoded, _ := json.Marshal(configuration.PrivateNetworkCIDRs)
		result = append(result, map[string]any{
			"name": "SYNARA_AGENTD_PRIVATE_NETWORK_CIDRS_JSON", "value": string(encoded),
		})
	}
	for _, item := range []struct{ name, value string }{
		{name: "SYNARA_PROVIDER_HTTP_PROXY", value: configuration.ProviderHTTPProxy},
		{name: "SYNARA_PROVIDER_HTTPS_PROXY", value: configuration.ProviderHTTPSProxy},
		{name: "SYNARA_PROVIDER_ALL_PROXY", value: configuration.ProviderAllProxy},
	} {
		if item.value != "" {
			result = append(result, map[string]any{"name": item.name, "value": item.value})
		}
	}
	if len(configuration.ProviderNoProxy) > 0 {
		result = append(result, map[string]any{
			"name": "SYNARA_PROVIDER_NO_PROXY", "value": strings.Join(configuration.ProviderNoProxy, ","),
		})
	}
	return result
}

func kubernetesResourceQuotaName(targetID uuid.UUID) string {
	return "synara-agentd-" + strings.ReplaceAll(targetID.String(), "-", "")[:12]
}

func (r *KubernetesReconciler) executionPod(
	target persistence.ExecutionTarget,
	configuration kubernetesTargetConfiguration,
	configHash string,
	execution kubernetesExecution,
	credential *ImagePullCredential,
) (map[string]any, error) {
	runner, err := json.Marshal(configuration.RunnerCommand)
	if err != nil {
		return nil, err
	}
	capabilities, err := json.Marshal(target.Capabilities)
	if err != nil {
		return nil, err
	}
	placementSnapshot, err := kubernetesExecutionPodPlacement(configuration, execution)
	if err != nil {
		return nil, err
	}
	generation := execution.Generation + 1
	labels := kubernetesTargetLabels(target)
	labels["synara.io/project-id"] = execution.ProjectID.String()
	labels["synara.io/session-id"] = execution.SessionID.String()
	labels[kubernetesExecutionLabel] = execution.ID.String()
	labels[kubernetesGenerationLabel] = strconv.FormatInt(generation, 10)
	labels[kubernetesWorkerModeLabel] = kubernetesWorkerModeExecutionPinned
	image, err := kubernetesExecutionImage(configuration.Image, execution)
	if err != nil {
		return nil, err
	}
	if err := validateKubernetesImagePullCredential(image, credential); err != nil {
		return nil, err
	}
	if execution.WorkerReleaseRevisionID != nil {
		labels[kubernetesReleaseLabel] = execution.WorkerReleaseRevisionID.String()
		labels[kubernetesChannelLabel] = stringValue(execution.WorkerReleaseChannel)
	}
	requests := map[string]any{}
	limits := map[string]any{}
	for key, value := range map[string]string{"cpu": configuration.CPURequest, "memory": configuration.MemoryRequest, "ephemeral-storage": configuration.EphemeralStorageRequest} {
		if value != "" {
			requests[key] = value
		}
	}
	for key, value := range map[string]string{"cpu": configuration.CPULimit, "memory": configuration.MemoryLimit, "ephemeral-storage": configuration.EphemeralStorageLimit} {
		if value != "" {
			limits[key] = value
		}
	}
	if configuration.GPUResourceName != "" {
		requests[configuration.GPUResourceName] = configuration.GPURequest
		limits[configuration.GPUResourceName] = configuration.GPURequest
	}
	gitCacheRoot := "/data/git-cache"
	if configuration.GitCachePersistentVolumeClaim != "" {
		gitCacheRoot = "/git-cache"
	}
	environment := []any{
		map[string]any{"name": "SYNARA_CONTROL_PLANE_URL", "value": configuration.ControlPlaneURL},
		map[string]any{"name": "SYNARA_WORKER_REGISTRATION_TOKEN_FILE", "value": kubernetesStagedRegistrationTokenPath},
		map[string]any{"name": "SYNARA_EXECUTION_TARGET_ID", "value": target.ID.String()},
		map[string]any{"name": "SYNARA_EXECUTION_TARGET_KIND", "value": "kubernetes"},
		map[string]any{"name": "SYNARA_AGENTD_ASSIGNED_EXECUTION_ID", "value": execution.ID.String()},
		map[string]any{"name": "SYNARA_AGENTD_CLUSTER_ID", "value": placementSnapshot.ClusterID},
		map[string]any{"name": "SYNARA_AGENTD_NAMESPACE", "value": placementSnapshot.Namespace},
		map[string]any{"name": "SYNARA_AGENTD_INSTANCE_ID", "valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "metadata.name"}}},
		map[string]any{"name": "SYNARA_AGENTD_INSTANCE_UID", "valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "metadata.uid"}}},
		map[string]any{"name": "SYNARA_AGENTD_CAPABILITIES_JSON", "value": string(capabilities)},
		map[string]any{"name": "SYNARA_AGENTD_RUNNER_COMMAND_JSON", "value": string(runner)},
		map[string]any{"name": "SYNARA_AGENTD_PROVIDER_HOST_PROTOCOL", "value": "v2"},
		map[string]any{"name": platform.KubernetesPIDsLimitEnvironment, "value": strconv.FormatUint(configuration.PIDsLimit, 10)},
		map[string]any{"name": "SYNARA_AGENTD_LEASE_RENEW_INTERVAL", "value": workertiming.LeaseRenewInterval(r.config.WorkerLeaseTTL).String()},
		map[string]any{"name": "SYNARA_AGENTD_DRAIN_TIMEOUT", "value": "20s"},
		map[string]any{"name": "SYNARA_AGENTD_WORKSPACE_ROOT", "value": "/data/workspaces"},
		map[string]any{"name": "SYNARA_AGENTD_GIT_CACHE_ROOT", "value": gitCacheRoot},
		map[string]any{"name": "SYNARA_AGENTD_PRIVATE_TMP_ROOT", "value": "/tmp"},
	}
	environment = append(environment, kubernetesRuntimeIsolationEnvironment(configuration)...)
	environment = append(environment, kubernetesTenantNetworkEnvironment(configuration)...)
	if digest := immutableImageDigest(image); digest != "" {
		environment = append(environment, map[string]any{"name": "SYNARA_AGENTD_IMAGE_DIGEST", "value": digest})
	}
	volumes := []any{
		map[string]any{"name": "workspace", "emptyDir": map[string]any{}},
		map[string]any{"name": "tmp", "emptyDir": map[string]any{}},
		map[string]any{"name": "home", "emptyDir": map[string]any{}},
	}
	volumes = append(volumes, kubernetesRegistrationTokenVolumes(target.ID)...)
	if configuration.WorkspaceSizeLimit != "" {
		volumes[0] = map[string]any{"name": "workspace", "emptyDir": map[string]any{"sizeLimit": configuration.WorkspaceSizeLimit}}
	}
	volumeMounts := []any{
		map[string]any{"name": "workspace", "mountPath": "/data"},
		map[string]any{"name": "tmp", "mountPath": "/tmp"},
		map[string]any{"name": "home", "mountPath": "/home/synara"},
		kubernetesRegistrationTokenMainMount(),
	}
	if configuration.GitCachePersistentVolumeClaim != "" {
		volumes = append(volumes, map[string]any{
			"name": "git-cache", "persistentVolumeClaim": map[string]any{"claimName": configuration.GitCachePersistentVolumeClaim},
		})
		volumeMounts = append(volumeMounts, map[string]any{"name": "git-cache", "mountPath": "/git-cache"})
	}
	container := map[string]any{
		"name": "agentd", "image": image, "imagePullPolicy": configuration.ImagePullPolicy,
		"command": []any{"/usr/local/bin/synara-agentd"}, "env": environment,
		"workingDir": "/data", "volumeMounts": volumeMounts,
		"securityContext": kubernetesRestrictedContainerSecurityContext(),
		"resources":       map[string]any{"requests": requests, "limits": limits},
	}
	podSpec := map[string]any{
		"serviceAccountName": configuration.ServiceAccountName, "automountServiceAccountToken": false,
		"enableServiceLinks": false, "hostNetwork": false, "hostPID": false, "hostIPC": false,
		"restartPolicy": "Never", "terminationGracePeriodSeconds": 30,
		"priorityClassName": kubernetesWorkerDefaultPriorityClassName,
		"preemptionPolicy":  placement.KubernetesPreemptionPolicyNever,
		"securityContext":   map[string]any{"runAsNonRoot": true, "fsGroup": 10001, "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
		"initContainers": []any{
			kubernetesNetworkBoundaryInitContainer(image, configuration.ImagePullPolicy, requests, limits),
			kubernetesRegistrationTokenInitContainer(image, configuration.ImagePullPolicy, requests, limits),
		},
		"containers": []any{container}, "volumes": volumes,
	}
	applyKubernetesRuntimeIsolation(podSpec, configuration)
	if len(configuration.NodeSelector) > 0 {
		podSpec["nodeSelector"] = cloneStringMap(configuration.NodeSelector)
	}
	if len(configuration.Tolerations) > 0 {
		podSpec["tolerations"] = cloneObjectList(configuration.Tolerations)
	}
	if configuration.RequireNodeSpread {
		podSpec["topologySpreadConstraints"] = kubernetesNodeSpreadConstraints(target.ID)
	}
	if err := applyKubernetesWorkerPoolSchedulingTemplate(podSpec, placementSnapshot.SchedulingTemplate); err != nil {
		return nil, err
	}
	if len(configuration.ImagePullSecrets) > 0 || credential != nil {
		secrets := make([]any, 0, len(configuration.ImagePullSecrets)+1)
		seen := make(map[string]struct{}, len(configuration.ImagePullSecrets)+1)
		for _, name := range configuration.ImagePullSecrets {
			if _, duplicate := seen[name]; duplicate {
				continue
			}
			secrets = append(secrets, map[string]any{"name": name})
			seen[name] = struct{}{}
		}
		if credential != nil {
			name := kubernetesRegistrySecretName(target.ID)
			if _, duplicate := seen[name]; !duplicate {
				secrets = append(secrets, map[string]any{"name": name})
			}
		}
		podSpec["imagePullSecrets"] = secrets
	}
	return map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{
			"name": kubernetesPodName(execution), "namespace": configuration.Namespace, "labels": labels,
			"annotations": map[string]any{kubernetesConfigAnnotation: configHash},
		},
		"spec": podSpec,
	}, nil
}

type kubernetesExecutionPodPlacementSnapshot struct {
	ClusterID          string
	Namespace          string
	SchedulingTemplate map[string]any
}

func kubernetesExecutionPodPlacement(
	configuration kubernetesTargetConfiguration,
	execution kubernetesExecution,
) (kubernetesExecutionPodPlacementSnapshot, error) {
	placementSnapshot := kubernetesExecutionPodPlacementSnapshot{
		ClusterID:          kubernetesLocalClusterID,
		Namespace:          configuration.Namespace,
		SchedulingTemplate: map[string]any{},
	}
	if execution.WorkerPool == nil {
		return placementSnapshot, nil
	}
	clusterID := strings.TrimSpace(execution.WorkerPool.ClusterID)
	if clusterID != "" && clusterID != kubernetesLocalClusterID {
		return kubernetesExecutionPodPlacementSnapshot{}, problem.New(
			409,
			"worker_pool_cluster_mismatch",
			"Worker pool cluster does not match the Kubernetes execution target cluster.",
		)
	}
	if namespace := strings.TrimSpace(execution.WorkerPool.Namespace); namespace != "" && namespace != configuration.Namespace {
		return kubernetesExecutionPodPlacementSnapshot{}, problem.New(
			409,
			"worker_pool_namespace_mismatch",
			"Worker pool namespace does not match the Kubernetes execution target namespace.",
		)
	}
	placementSnapshot.SchedulingTemplate = execution.WorkerPool.SchedulingTemplate
	return placementSnapshot, nil
}

func (r *KubernetesReconciler) warmPoolPod(
	target persistence.ExecutionTarget,
	configuration kubernetesTargetConfiguration,
	plan kubernetesWarmPodPlan,
	credential *ImagePullCredential,
) (map[string]any, error) {
	runner, err := json.Marshal(configuration.RunnerCommand)
	if err != nil {
		return nil, err
	}
	capabilities, err := json.Marshal(target.Capabilities)
	if err != nil {
		return nil, err
	}
	clusterID := strings.TrimSpace(plan.Pool.ClusterID)
	if clusterID == "" {
		clusterID = kubernetesLocalClusterID
	}
	if clusterID != kubernetesLocalClusterID {
		return nil, problem.New(409, "worker_pool_cluster_mismatch", "Warm Worker pool cluster does not match the Kubernetes execution target cluster.")
	}
	if namespace := strings.TrimSpace(plan.Pool.Namespace); namespace != "" && namespace != configuration.Namespace {
		return nil, problem.New(409, "worker_pool_namespace_mismatch", "Warm Worker pool namespace does not match the Kubernetes execution target namespace.")
	}
	image, err := kubernetesWarmPoolImage(configuration.Image, plan.Release)
	if err != nil {
		return nil, err
	}
	if err := validateKubernetesImagePullCredential(image, credential); err != nil {
		return nil, err
	}
	labels := kubernetesTargetLabels(target)
	labels[kubernetesWorkerModeLabel] = kubernetesWorkerModeWarmPool
	labels[kubernetesWorkerPoolIDLabel] = plan.Pool.ID.String()
	labels[kubernetesWorkerPoolVersionLabel] = strconv.FormatInt(plan.Pool.Version, 10)
	labels[kubernetesCapacityClassLabel] = plan.Pool.CapacityClass
	labels[kubernetesWarmSlotLabel] = strconv.Itoa(plan.Slot)
	if plan.Release.RevisionID != nil {
		labels[kubernetesReleaseLabel] = plan.Release.RevisionID.String()
		labels[kubernetesChannelLabel] = stringValue(plan.Release.Channel)
	}
	requests := map[string]any{}
	limits := map[string]any{}
	for key, value := range map[string]string{"cpu": configuration.CPURequest, "memory": configuration.MemoryRequest, "ephemeral-storage": configuration.EphemeralStorageRequest} {
		if value != "" {
			requests[key] = value
		}
	}
	for key, value := range map[string]string{"cpu": configuration.CPULimit, "memory": configuration.MemoryLimit, "ephemeral-storage": configuration.EphemeralStorageLimit} {
		if value != "" {
			limits[key] = value
		}
	}
	if configuration.GPUResourceName != "" {
		requests[configuration.GPUResourceName] = configuration.GPURequest
		limits[configuration.GPUResourceName] = configuration.GPURequest
	}
	gitCacheRoot := "/data/git-cache"
	if configuration.GitCachePersistentVolumeClaim != "" {
		gitCacheRoot = "/git-cache"
	}
	environment := []any{
		map[string]any{"name": "SYNARA_CONTROL_PLANE_URL", "value": configuration.ControlPlaneURL},
		map[string]any{"name": "SYNARA_WORKER_REGISTRATION_TOKEN_FILE", "value": kubernetesStagedRegistrationTokenPath},
		map[string]any{"name": "SYNARA_EXECUTION_TARGET_ID", "value": target.ID.String()},
		map[string]any{"name": "SYNARA_EXECUTION_TARGET_KIND", "value": "kubernetes"},
		map[string]any{"name": "SYNARA_AGENTD_WORKER_MODE", "value": kubernetesWorkerModeWarmPool},
		map[string]any{"name": "SYNARA_AGENTD_CLUSTER_ID", "value": clusterID},
		map[string]any{"name": "SYNARA_AGENTD_NAMESPACE", "value": configuration.Namespace},
		map[string]any{"name": "SYNARA_AGENTD_INSTANCE_ID", "valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "metadata.name"}}},
		map[string]any{"name": "SYNARA_AGENTD_INSTANCE_UID", "valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "metadata.uid"}}},
		map[string]any{"name": "SYNARA_AGENTD_CAPABILITIES_JSON", "value": string(capabilities)},
		map[string]any{"name": "SYNARA_AGENTD_RUNNER_COMMAND_JSON", "value": string(runner)},
		map[string]any{"name": "SYNARA_AGENTD_PROVIDER_HOST_PROTOCOL", "value": "v2"},
		map[string]any{"name": platform.KubernetesPIDsLimitEnvironment, "value": strconv.FormatUint(configuration.PIDsLimit, 10)},
		map[string]any{"name": "SYNARA_AGENTD_LEASE_RENEW_INTERVAL", "value": workertiming.LeaseRenewInterval(r.config.WorkerLeaseTTL).String()},
		map[string]any{"name": "SYNARA_AGENTD_DRAIN_TIMEOUT", "value": "20s"},
		map[string]any{"name": "SYNARA_AGENTD_WORKSPACE_ROOT", "value": "/data/workspaces"},
		map[string]any{"name": "SYNARA_AGENTD_GIT_CACHE_ROOT", "value": gitCacheRoot},
		map[string]any{"name": "SYNARA_AGENTD_PRIVATE_TMP_ROOT", "value": "/tmp"},
	}
	environment = append(environment, kubernetesRuntimeIsolationEnvironment(configuration)...)
	environment = append(environment, kubernetesTenantNetworkEnvironment(configuration)...)
	if digest := immutableImageDigest(image); digest != "" {
		environment = append(environment, map[string]any{"name": "SYNARA_AGENTD_IMAGE_DIGEST", "value": digest})
	}
	volumes := []any{
		map[string]any{"name": "workspace", "emptyDir": map[string]any{}},
		map[string]any{"name": "tmp", "emptyDir": map[string]any{}},
		map[string]any{"name": "home", "emptyDir": map[string]any{}},
	}
	volumes = append(volumes, kubernetesRegistrationTokenVolumes(target.ID)...)
	if configuration.WorkspaceSizeLimit != "" {
		volumes[0] = map[string]any{"name": "workspace", "emptyDir": map[string]any{"sizeLimit": configuration.WorkspaceSizeLimit}}
	}
	volumeMounts := []any{
		map[string]any{"name": "workspace", "mountPath": "/data"},
		map[string]any{"name": "tmp", "mountPath": "/tmp"},
		map[string]any{"name": "home", "mountPath": "/home/synara"},
		kubernetesRegistrationTokenMainMount(),
	}
	if configuration.GitCachePersistentVolumeClaim != "" {
		volumes = append(volumes, map[string]any{
			"name": "git-cache", "persistentVolumeClaim": map[string]any{"claimName": configuration.GitCachePersistentVolumeClaim},
		})
		volumeMounts = append(volumeMounts, map[string]any{"name": "git-cache", "mountPath": "/git-cache"})
	}
	container := map[string]any{
		"name": "agentd", "image": image, "imagePullPolicy": configuration.ImagePullPolicy,
		"command": []any{"/usr/local/bin/synara-agentd"}, "env": environment,
		"workingDir": "/data", "volumeMounts": volumeMounts,
		"securityContext": kubernetesRestrictedContainerSecurityContext(),
		"resources":       map[string]any{"requests": requests, "limits": limits},
	}
	podSpec := map[string]any{
		"serviceAccountName": configuration.ServiceAccountName, "automountServiceAccountToken": false,
		"enableServiceLinks": false, "hostNetwork": false, "hostPID": false, "hostIPC": false,
		"restartPolicy": "Never", "terminationGracePeriodSeconds": 30,
		"priorityClassName": kubernetesWorkerDefaultPriorityClassName,
		"preemptionPolicy":  placement.KubernetesPreemptionPolicyNever,
		"securityContext":   map[string]any{"runAsNonRoot": true, "fsGroup": 10001, "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
		"initContainers": []any{
			kubernetesNetworkBoundaryInitContainer(image, configuration.ImagePullPolicy, requests, limits),
			kubernetesRegistrationTokenInitContainer(image, configuration.ImagePullPolicy, requests, limits),
		},
		"containers": []any{container}, "volumes": volumes,
	}
	applyKubernetesRuntimeIsolation(podSpec, configuration)
	if len(configuration.NodeSelector) > 0 {
		podSpec["nodeSelector"] = cloneStringMap(configuration.NodeSelector)
	}
	if len(configuration.Tolerations) > 0 {
		podSpec["tolerations"] = cloneObjectList(configuration.Tolerations)
	}
	if configuration.RequireNodeSpread {
		podSpec["topologySpreadConstraints"] = kubernetesNodeSpreadConstraints(target.ID)
	}
	if err := applyKubernetesWorkerPoolSchedulingTemplate(podSpec, plan.Pool.SchedulingTemplate); err != nil {
		return nil, err
	}
	if len(configuration.ImagePullSecrets) > 0 || credential != nil {
		secrets := make([]any, 0, len(configuration.ImagePullSecrets)+1)
		seen := make(map[string]struct{}, len(configuration.ImagePullSecrets)+1)
		for _, name := range configuration.ImagePullSecrets {
			if _, duplicate := seen[name]; duplicate {
				continue
			}
			secrets = append(secrets, map[string]any{"name": name})
			seen[name] = struct{}{}
		}
		if credential != nil {
			name := kubernetesRegistrySecretName(target.ID)
			if _, duplicate := seen[name]; !duplicate {
				secrets = append(secrets, map[string]any{"name": name})
			}
		}
		podSpec["imagePullSecrets"] = secrets
	}
	return map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{
			"name": kubernetesWarmPodName(plan), "namespace": configuration.Namespace, "labels": labels,
			"annotations": map[string]any{kubernetesConfigAnnotation: plan.ConfigHash},
		},
		"spec": podSpec,
	}, nil
}

func applyKubernetesWorkerPoolSchedulingTemplate(podSpec map[string]any, template map[string]any) error {
	normalized, err := placement.NormalizeKubernetesSchedulingTemplate(template)
	if err != nil {
		return err
	}
	podSpec["preemptionPolicy"] = placement.KubernetesPreemptionPolicyNever
	if normalized.PriorityClassName != "" {
		podSpec["priorityClassName"] = normalized.PriorityClassName
	}
	if len(normalized.NodeSelector) > 0 {
		merged := map[string]string{}
		if existing, ok := podSpec["nodeSelector"].(map[string]string); ok {
			merged = cloneStringMap(existing)
		}
		for nodeKey, nodeValue := range normalized.NodeSelector {
			merged[nodeKey] = nodeValue
		}
		podSpec["nodeSelector"] = merged
	}
	if len(normalized.Tolerations) > 0 {
		existing := make([]any, 0)
		if current, ok := podSpec["tolerations"].([]any); ok {
			existing = append(existing, current...)
		}
		podSpec["tolerations"] = append(existing, normalized.Tolerations...)
	}
	return nil
}

func applyKubernetesRuntimeIsolation(podSpec map[string]any, configuration kubernetesTargetConfiguration) {
	if configuration.RuntimeIsolationDecision == nil ||
		configuration.RuntimeIsolationDecision.EffectiveRuntime != runtimeIsolationGVisor {
		return
	}
	podSpec["runtimeClassName"] = configuration.RuntimeIsolationDecision.RuntimeClassName
}

func kubernetesRuntimeIsolationEnvironment(configuration kubernetesTargetConfiguration) []any {
	if configuration.RuntimeIsolationDecision == nil {
		return nil
	}
	return []any{map[string]any{
		"name":  platform.KubernetesRuntimeIsolationProfileEnvironment,
		"value": string(configuration.RuntimeIsolationDecision.EffectiveProfile),
	}}
}

func cloneStringMap(input map[string]string) map[string]string {
	cloned := make(map[string]string, len(input))
	for key, value := range input {
		cloned[key] = value
	}
	return cloned
}

func cloneObjectList(input []map[string]any) []any {
	cloned := make([]any, 0, len(input))
	for _, item := range input {
		copy := make(map[string]any, len(item))
		for key, value := range item {
			copy[key] = value
		}
		cloned = append(cloned, copy)
	}
	return cloned
}

func kubernetesNodeSpreadConstraints(targetID uuid.UUID) []any {
	return []any{
		map[string]any{
			"maxSkew":           1,
			"topologyKey":       "kubernetes.io/hostname",
			"whenUnsatisfiable": "DoNotSchedule",
			"labelSelector": map[string]any{
				"matchLabels": map[string]any{
					kubernetesTargetLabel: targetID.String(),
				},
			},
		},
	}
}

func kubernetesTargetLabels(target persistence.ExecutionTarget) map[string]string {
	labels := map[string]string{
		kubernetesManagedLabel: "true", kubernetesTargetLabel: target.ID.String(),
	}
	if target.TenantID != nil {
		labels["synara.io/tenant-id"] = target.TenantID.String()
	}
	if target.OrganizationID != nil {
		labels["synara.io/organization-id"] = target.OrganizationID.String()
	}
	return labels
}

func kubernetesManagedNamespaceLabels(labels map[string]string) map[string]string {
	namespaced := make(map[string]string, len(labels)+3)
	for key, value := range labels {
		namespaced[key] = value
	}
	namespaced["pod-security.kubernetes.io/enforce"] = "restricted"
	namespaced["pod-security.kubernetes.io/audit"] = "restricted"
	namespaced["pod-security.kubernetes.io/warn"] = "restricted"
	return namespaced
}

func kubernetesSecretName(targetID uuid.UUID) string {
	return "synara-agentd-" + strings.ReplaceAll(targetID.String(), "-", "")[:12]
}

func kubernetesRegistrySecretName(targetID uuid.UUID) string {
	return kubernetesSecretName(targetID) + "-registry"
}

func kubernetesRegistrySecret(
	target persistence.ExecutionTarget,
	namespace string,
	labels map[string]string,
	credential *ImagePullCredential,
) (map[string]any, error) {
	auths := map[string]any{}
	if credential != nil {
		if credential.RegistryToken != "" {
			return nil, problem.New(
				409,
				"worker_image_pull_bearer_unsupported",
				"Kubernetes Worker image pull Secrets require an OCI Registry basic Credential.",
			)
		}
		authority, err := normalizeRegistryAuthority(credential.Host)
		if err != nil {
			return nil, problem.New(500, "worker_image_pull_credential_invalid", "Worker image pull Credential projection is invalid.")
		}
		auths[registryAuthServerAddress(authority)] = map[string]any{
			"username": credential.Username,
			"password": credential.Password,
			"auth":     base64.StdEncoding.EncodeToString([]byte(credential.Username + ":" + credential.Password)),
		}
	}
	config, err := json.Marshal(map[string]any{"auths": auths})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/dockerconfigjson",
		"metadata": map[string]any{
			"name": kubernetesRegistrySecretName(target.ID), "namespace": namespace, "labels": labels,
		},
		"data": map[string]any{".dockerconfigjson": base64.StdEncoding.EncodeToString(config)},
	}, nil
}

func kubernetesFoundationApplyHash(baseHash string, credential *ImagePullCredential) (string, error) {
	var identity any
	if credential != nil {
		identity = struct {
			BindingID         uuid.UUID
			CredentialID      uuid.UUID
			CredentialVersion int
			Host              string
		}{credential.BindingID, credential.CredentialID, credential.CredentialVersion, credential.Host}
	}
	payload, err := json.Marshal(struct {
		BaseHash   string
		Credential any
	}{baseHash, identity})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func kubernetesExecutionPodHash(baseHash, baseImage string, execution kubernetesExecution) (string, error) {
	image, err := kubernetesExecutionImage(baseImage, execution)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(struct {
		BaseHash   string
		Image      string
		RevisionID *uuid.UUID
		Channel    *string
		WorkerPool *kubernetesExecutionPoolSnapshot
	}{baseHash, image, execution.WorkerReleaseRevisionID, execution.WorkerReleaseChannel, execution.WorkerPool})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func kubernetesExecutionImage(baseImage string, execution kubernetesExecution) (string, error) {
	if execution.WorkerReleaseRevisionID == nil && execution.WorkerReleaseChannel == nil && execution.WorkerReleaseImageDigest == nil {
		return baseImage, nil
	}
	if execution.WorkerReleaseRevisionID == nil || execution.WorkerReleaseChannel == nil ||
		execution.WorkerReleaseImageDigest == nil ||
		(*execution.WorkerReleaseChannel != "promoted" && *execution.WorkerReleaseChannel != "canary") {
		return "", problem.New(409, "worker_release_execution_invalid", "Execution Worker release selection is invalid.")
	}
	return pinImageReference(baseImage, *execution.WorkerReleaseImageDigest)
}

func validateKubernetesImagePullCredential(image string, credential *ImagePullCredential) error {
	if err := validateImagePullCredential(image, credential); err != nil {
		return err
	}
	if credential != nil && credential.RegistryToken != "" {
		return problem.New(
			409,
			"worker_image_pull_bearer_unsupported",
			"Kubernetes Worker image pull Secrets require an OCI Registry basic Credential.",
		)
	}
	return nil
}
