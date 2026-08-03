package executiontargets

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

type kubernetesSandboxClaimObservation struct {
	UID                 string
	SandboxName         string
	ConfigurationDigest string
	WarmPoolName        string
	Ready               bool
	Reason              string
}

type kubernetesSandboxObservation struct {
	UID     string
	PodName string
}

type kubernetesSandboxTemplateToleration struct {
	Key      string `json:"key"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
	Effect   string `json:"effect"`
}

type kubernetesSandboxTemplateEnvironment struct {
	Name      string `json:"name"`
	Value     string `json:"value"`
	ValueFrom struct {
		FieldRef struct {
			FieldPath string `json:"fieldPath"`
		} `json:"fieldRef"`
		ConfigMapKeyRef *struct {
			Name     string `json:"name"`
			Key      string `json:"key"`
			Optional *bool  `json:"optional"`
		} `json:"configMapKeyRef"`
	} `json:"valueFrom"`
}

type kubernetesSandboxTemplateVolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly"`
}

type kubernetesSandboxTemplateContainer struct {
	Name         string                                 `json:"name"`
	Image        string                                 `json:"image"`
	Env          []kubernetesSandboxTemplateEnvironment `json:"env"`
	VolumeMounts []kubernetesSandboxTemplateVolumeMount `json:"volumeMounts"`
}

type kubernetesSandboxTemplateVolume struct {
	Name        string `json:"name"`
	DownwardAPI struct {
		Items []struct {
			Path     string `json:"path"`
			FieldRef struct {
				FieldPath string `json:"fieldPath"`
			} `json:"fieldRef"`
		} `json:"items"`
	} `json:"downwardAPI"`
	Secret *struct {
		SecretName  string `json:"secretName"`
		Optional    *bool  `json:"optional"`
		DefaultMode int32  `json:"defaultMode"`
		Items       []struct {
			Key  string `json:"key"`
			Path string `json:"path"`
		} `json:"items"`
	} `json:"secret"`
}

type kubernetesSandboxAcceptanceObservation struct {
	SandboxAPIReady                bool
	SandboxClaimAPIReady           bool
	SandboxTemplateAPIReady        bool
	SandboxWarmPoolAPIReady        bool
	OperatorReady                  bool
	TemplateIdentity               string
	TemplateRuntime                string
	TemplateAgentdImage            string
	TemplateSandboxRuntimeImage    string
	TemplateSandboxRuntimeName     string
	AssignedExecutionFieldRefReady bool
	TemplateObservabilityReady     bool
	WarmPoolTemplateReady          bool
	WarmPoolReady                  bool
	WarmPoolDesiredReplicas        int
	WarmPoolUpdateStrategy         string
	WarmPoolTemplateImageFresh     bool
	VirtualNodeReady               bool
	KVMRuntimeReady                bool
	HostSupervisorReady            bool
	FencedVSockReady               bool
	GuestIsolationReady            bool
	CocoonTemplateSchedulingReady  bool
	CocoonTemplateCleanupReady     bool
	CocoonTemplateWorkspaceReady   bool
}

const (
	kubernetesCocoonKVMReadyLabel                  = "sandbox.cocoonstack.io/kvm-ready"
	kubernetesCocoonHostSupervisorLabel            = "synara.io/host-supervisor"
	kubernetesCocoonProviderTransportLabel         = "synara.io/provider-transport"
	kubernetesCocoonIsolationProfileLabel          = "synara.io/isolation-profile"
	kubernetesCocoonSupervisorInstanceAnnotation   = "synara.io/host-supervisor-instance"
	kubernetesCocoonSupervisorObservedAtAnnotation = "synara.io/host-supervisor-observed-at"
	kubernetesCocoonSnapshotPolicyAnnotation       = "cocoonset.cocoonstack.io/snapshot-policy"
	kubernetesCocoonSharedMemoryAnnotation         = "vm.cocoonstack.io/shared-memory"

	kubernetesCocoonHostSupervisorV1    = "v1"
	kubernetesCocoonProviderTransportV2 = "vsock-v2"
	kubernetesCocoonIsolationProfileV1  = "microvm-isolated-v1"
	kubernetesCocoonVirtualNodeType     = "virtual-node"
	kubernetesCocoonVirtualProvider     = "cocoon"
	kubernetesCocoonGuestContainerName  = "agent"
	kubernetesCocoonEphemeralPolicy     = "never"

	kubernetesCocoonSupervisorHeartbeatMaxAge     = 45 * time.Second
	kubernetesCocoonSupervisorHeartbeatFutureSkew = 5 * time.Second
)

type kubernetesSandboxClient interface {
	ListPods(context.Context, string, uuid.UUID) ([]kubernetesPod, error)
	GetSandboxClaim(context.Context, string, string) (kubernetesSandboxClaimObservation, bool, error)
	GetSandbox(context.Context, string, string) (kubernetesSandboxObservation, bool, error)
	DeleteSandboxClaim(context.Context, string, string, string) error
	ObserveSandboxAcceptance(context.Context, uuid.UUID, kubernetesTargetConfiguration) (kubernetesSandboxAcceptanceObservation, error)
}

func (c *kubernetesHTTPClient) GetSandboxClaim(
	ctx context.Context,
	namespace, name string,
) (kubernetesSandboxClaimObservation, bool, error) {
	var response struct {
		Metadata struct {
			UID         string            `json:"uid"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
		Spec struct {
			WarmPoolRef struct {
				Name string `json:"name"`
			} `json:"warmPoolRef"`
		} `json:"spec"`
		Status struct {
			Sandbox struct {
				Name string `json:"name"`
			} `json:"sandbox"`
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
				Reason string `json:"reason"`
			} `json:"conditions"`
		} `json:"status"`
	}
	path := kubernetesSandboxNamespacedPath(namespace, "sandboxclaims", name)
	found, err := c.getOptional(ctx, path, &response)
	if err != nil || !found {
		return kubernetesSandboxClaimObservation{}, found, err
	}
	observation := kubernetesSandboxClaimObservation{
		UID: strings.TrimSpace(response.Metadata.UID), SandboxName: strings.TrimSpace(response.Status.Sandbox.Name),
		ConfigurationDigest: strings.TrimSpace(response.Metadata.Annotations[kubernetesConfigAnnotation]),
		WarmPoolName:        strings.TrimSpace(response.Spec.WarmPoolRef.Name),
	}
	for _, condition := range response.Status.Conditions {
		if condition.Type == "Ready" {
			observation.Ready = condition.Status == "True"
			observation.Reason = strings.TrimSpace(condition.Reason)
			break
		}
	}
	return observation, true, nil
}

func (c *kubernetesHTTPClient) GetSandbox(
	ctx context.Context,
	namespace, name string,
) (kubernetesSandboxObservation, bool, error) {
	var response struct {
		Metadata struct {
			UID         string            `json:"uid"`
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	path := kubernetesCoreSandboxNamespacedPath(namespace, "sandboxes", name)
	found, err := c.getOptional(ctx, path, &response)
	if err != nil || !found {
		return kubernetesSandboxObservation{}, found, err
	}
	return kubernetesSandboxObservation{
		UID:     strings.TrimSpace(response.Metadata.UID),
		PodName: strings.TrimSpace(response.Metadata.Annotations["agents.x-k8s.io/pod-name"]),
	}, true, nil
}

func (c *kubernetesHTTPClient) DeleteSandboxClaim(
	ctx context.Context,
	namespace, name, uid string,
) error {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return errors.New("Kubernetes SandboxClaim UID is required for safe deletion")
	}
	path := kubernetesSandboxNamespacedPath(namespace, "sandboxclaims", name) +
		"?gracePeriodSeconds=0&propagationPolicy=Foreground"
	return c.do(ctx, http.MethodDelete, path, map[string]any{
		"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": map[string]any{"uid": uid},
	}, nil, http.StatusOK, http.StatusAccepted, http.StatusNotFound)
}

func (c *kubernetesHTTPClient) ObserveSandboxAcceptance(
	ctx context.Context,
	targetID uuid.UUID,
	configuration kubernetesTargetConfiguration,
) (kubernetesSandboxAcceptanceObservation, error) {
	observation := kubernetesSandboxAcceptanceObservation{}
	crds := []struct {
		name  string
		ready *bool
	}{
		{name: "sandboxes.agents.x-k8s.io", ready: &observation.SandboxAPIReady},
		{name: "sandboxclaims.extensions.agents.x-k8s.io", ready: &observation.SandboxClaimAPIReady},
		{name: "sandboxtemplates.extensions.agents.x-k8s.io", ready: &observation.SandboxTemplateAPIReady},
		{name: "sandboxwarmpools.extensions.agents.x-k8s.io", ready: &observation.SandboxWarmPoolAPIReady},
	}
	for _, crd := range crds {
		ready, err := c.sandboxCRDReady(ctx, crd.name)
		if err != nil {
			return kubernetesSandboxAcceptanceObservation{}, err
		}
		*crd.ready = ready
	}
	var template struct {
		Metadata struct {
			UID             string `json:"uid"`
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Spec struct {
			PodTemplate struct {
				Metadata struct {
					Annotations map[string]string `json:"annotations"`
				} `json:"metadata"`
				Spec struct {
					NodeSelector map[string]string                     `json:"nodeSelector"`
					Tolerations  []kubernetesSandboxTemplateToleration `json:"tolerations"`
					Containers   []kubernetesSandboxTemplateContainer  `json:"containers"`
					Volumes      []kubernetesSandboxTemplateVolume     `json:"volumes"`
				} `json:"spec"`
			} `json:"podTemplate"`
		} `json:"spec"`
	}
	if err := c.do(
		ctx, http.MethodGet,
		kubernetesSandboxNamespacedPath(configuration.Namespace, "sandboxtemplates", configuration.SandboxTemplateName),
		nil, &template, http.StatusOK,
	); err != nil {
		return kubernetesSandboxAcceptanceObservation{}, err
	}
	var pool struct {
		Metadata struct {
			UID string `json:"uid"`
		} `json:"metadata"`
		Spec struct {
			Replicas           int `json:"replicas"`
			SandboxTemplateRef struct {
				Name string `json:"name"`
			} `json:"sandboxTemplateRef"`
			UpdateStrategy struct {
				Type string `json:"type"`
			} `json:"updateStrategy"`
		} `json:"spec"`
		Status struct {
			Replicas      int `json:"replicas"`
			ReadyReplicas int `json:"readyReplicas"`
		} `json:"status"`
	}
	if err := c.do(
		ctx, http.MethodGet,
		kubernetesSandboxNamespacedPath(configuration.Namespace, "sandboxwarmpools", configuration.SandboxWarmPoolName),
		nil, &pool, http.StatusOK,
	); err != nil {
		return kubernetesSandboxAcceptanceObservation{}, err
	}
	observation.TemplateRuntime = strings.TrimSpace(template.Spec.PodTemplate.Metadata.Annotations["sandbox.cocoonstack.io/runtime"])
	observation.CocoonTemplateCleanupReady = strings.TrimSpace(
		template.Spec.PodTemplate.Metadata.Annotations[kubernetesCocoonSnapshotPolicyAnnotation],
	) == kubernetesCocoonEphemeralPolicy
	observation.CocoonTemplateWorkspaceReady = strings.EqualFold(strings.TrimSpace(
		template.Spec.PodTemplate.Metadata.Annotations[kubernetesCocoonSharedMemoryAnnotation],
	), "true")
	if uid, resourceVersion := strings.TrimSpace(template.Metadata.UID), strings.TrimSpace(template.Metadata.ResourceVersion); uid != "" && resourceVersion != "" {
		observation.TemplateIdentity = uid + ":" + resourceVersion
	}
	observation.OperatorReady = pool.Status.Replicas == pool.Spec.Replicas
	observation.WarmPoolTemplateReady = strings.TrimSpace(pool.Spec.SandboxTemplateRef.Name) == configuration.SandboxTemplateName
	observation.WarmPoolReady = pool.Status.ReadyReplicas >= pool.Spec.Replicas
	observation.WarmPoolDesiredReplicas = pool.Spec.Replicas
	observation.WarmPoolUpdateStrategy = strings.TrimSpace(pool.Spec.UpdateStrategy.Type)
	for index, container := range template.Spec.PodTemplate.Spec.Containers {
		if index == 0 {
			observation.TemplateSandboxRuntimeName = strings.TrimSpace(container.Name)
			observation.TemplateSandboxRuntimeImage = strings.TrimSpace(container.Image)
		}
		if strings.TrimSpace(container.Name) == "agentd" {
			observation.TemplateAgentdImage = strings.TrimSpace(container.Image)
			break
		}
	}
	var sandboxes struct {
		Items []struct {
			Metadata struct {
				OwnerReferences []struct {
					UID string `json:"uid"`
				} `json:"ownerReferences"`
			} `json:"metadata"`
			Spec struct {
				PodTemplate struct {
					Spec struct {
						Containers []struct {
							Name  string `json:"name"`
							Image string `json:"image"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"podTemplate"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := c.do(
		ctx, http.MethodGet,
		"/apis/agents.x-k8s.io/v1beta1/namespaces/"+url.PathEscape(configuration.Namespace)+"/sandboxes",
		nil, &sandboxes, http.StatusOK,
	); err != nil {
		return kubernetesSandboxAcceptanceObservation{}, err
	}
	ownedPoolMembers := 0
	poolMembersFresh := true
	for _, sandbox := range sandboxes.Items {
		owned := false
		for _, owner := range sandbox.Metadata.OwnerReferences {
			if strings.TrimSpace(owner.UID) == strings.TrimSpace(pool.Metadata.UID) && strings.TrimSpace(pool.Metadata.UID) != "" {
				owned = true
				break
			}
		}
		if !owned {
			continue
		}
		ownedPoolMembers++
		image := ""
		for index, container := range sandbox.Spec.PodTemplate.Spec.Containers {
			if configuration.AllocationBackend == string(kubernetesAllocationBackendSandboxOperatorCocoon) {
				if index == 0 {
					image = strings.TrimSpace(container.Image)
				}
				break
			}
			if strings.TrimSpace(container.Name) == "agentd" {
				image = strings.TrimSpace(container.Image)
				break
			}
		}
		templateImage := kubernetesSandboxAcceptanceRuntimeImage(configuration.AllocationBackend, observation)
		if image == "" || image != templateImage {
			poolMembersFresh = false
		}
	}
	observation.WarmPoolTemplateImageFresh = poolMembersFresh &&
		ownedPoolMembers == pool.Status.Replicas && pool.Status.Replicas == pool.Spec.Replicas
	expectedFieldPath := "metadata.labels['" + kubernetesSandboxAssignedExecutionLabel + "']"
	assignmentFilePath := ""
	assignmentMounts := map[string]string{}
	for _, container := range template.Spec.PodTemplate.Spec.Containers {
		if strings.TrimSpace(container.Name) != "agentd" {
			continue
		}
		for _, environment := range container.Env {
			if environment.Name == "SYNARA_AGENTD_ASSIGNED_EXECUTION_ID_FILE" {
				assignmentFilePath = strings.TrimSpace(environment.Value)
			}
		}
		for _, mount := range container.VolumeMounts {
			assignmentMounts[mount.Name] = strings.TrimRight(strings.TrimSpace(mount.MountPath), "/")
		}
		break
	}
	for _, container := range template.Spec.PodTemplate.Spec.Containers {
		if strings.TrimSpace(container.Name) == "agentd" {
			observation.TemplateObservabilityReady = kubernetesSandboxTemplateObservabilityReady(
				targetID,
				container,
			)
			break
		}
	}
	for _, volume := range template.Spec.PodTemplate.Spec.Volumes {
		for _, item := range volume.DownwardAPI.Items {
			mountPath := assignmentMounts[volume.Name]
			expectedPath := mountPath + "/" + strings.TrimLeft(strings.TrimSpace(item.Path), "/")
			if strings.TrimSpace(item.FieldRef.FieldPath) == expectedFieldPath && mountPath != "" &&
				assignmentFilePath == expectedPath {
				observation.AssignedExecutionFieldRefReady = true
			}
		}
	}
	if configuration.AllocationBackend != string(kubernetesAllocationBackendSandboxOperatorCocoon) {
		return observation, nil
	}
	observation.CocoonTemplateSchedulingReady = kubernetesCocoonTemplateSchedulingReady(
		template.Spec.PodTemplate.Spec.NodeSelector,
		template.Spec.PodTemplate.Spec.Tolerations,
	)
	var nodes struct {
		Items []struct {
			Metadata struct {
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	query := url.Values{"labelSelector": {"node.kubernetes.io/instance-type=virtual-node"}}
	if err := c.do(ctx, http.MethodGet, "/api/v1/nodes?"+query.Encode(), nil, &nodes, http.StatusOK); err != nil {
		return kubernetesSandboxAcceptanceObservation{}, err
	}
	observedAt := time.Now().UTC()
	liveSchedulableNodes := 0
	staleSchedulableNode := false
	for _, node := range nodes.Items {
		ready := false
		for _, condition := range node.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				ready = true
				break
			}
		}
		if !ready {
			continue
		}
		observation.VirtualNodeReady = true
		if node.Metadata.Labels[kubernetesCocoonKVMReadyLabel] == "true" {
			observation.KVMRuntimeReady = true
		}
		if !kubernetesCocoonNodeMatchesSchedulingFence(node.Metadata.Labels) {
			continue
		}
		if kubernetesCocoonSupervisorHeartbeatReady(node.Metadata.Annotations, observedAt) {
			liveSchedulableNodes++
		} else {
			staleSchedulableNode = true
		}
	}
	if liveSchedulableNodes > 0 && !staleSchedulableNode {
		observation.HostSupervisorReady = true
		observation.FencedVSockReady = true
		observation.GuestIsolationReady = true
	}
	return observation, nil
}

func kubernetesSandboxTemplateObservabilityReady(
	targetID uuid.UUID,
	container kubernetesSandboxTemplateContainer,
) bool {
	wantConfig := map[string]struct{}{
		"OTEL_EXPORTER_OTLP_ENDPOINT":      {},
		"OTEL_EXPORTER_OTLP_PROTOCOL":      {},
		"OTEL_EXPORTER_OTLP_CERTIFICATE":   {},
		"SYNARA_OTEL_TRACE_SAMPLE_RATIO":   {},
		"SYNARA_OTEL_COLLECTOR_REGION":     {},
		"SYNARA_OTEL_TRACE_RETENTION_DAYS": {},
	}
	seenConfig := make(map[string]struct{}, len(wantConfig))
	for _, item := range container.Env {
		name := strings.TrimSpace(item.Name)
		if _, forbidden := kubernetesForbiddenWorkerObservabilityEnvironment[name]; forbidden {
			return false
		}
		if _, required := wantConfig[name]; required {
			ref := item.ValueFrom.ConfigMapKeyRef
			if _, duplicate := seenConfig[name]; duplicate || strings.TrimSpace(item.Value) != "" || ref == nil ||
				strings.TrimSpace(ref.Name) != kubernetesObservabilityConfigMapName(targetID) || strings.TrimSpace(ref.Key) != name ||
				ref.Optional == nil || !*ref.Optional {
				return false
			}
			seenConfig[name] = struct{}{}
			continue
		}
	}
	return len(seenConfig) == len(wantConfig)
}

func kubernetesCocoonNodeMatchesSchedulingFence(labels map[string]string) bool {
	return strings.TrimSpace(labels["node.kubernetes.io/instance-type"]) == kubernetesCocoonVirtualNodeType &&
		strings.TrimSpace(labels[kubernetesCocoonKVMReadyLabel]) == "true" &&
		strings.TrimSpace(labels[kubernetesCocoonHostSupervisorLabel]) == kubernetesCocoonHostSupervisorV1 &&
		strings.TrimSpace(labels[kubernetesCocoonProviderTransportLabel]) == kubernetesCocoonProviderTransportV2 &&
		strings.TrimSpace(labels[kubernetesCocoonIsolationProfileLabel]) == kubernetesCocoonIsolationProfileV1
}

func kubernetesCocoonTemplateSchedulingReady(
	nodeSelector map[string]string,
	tolerations []kubernetesSandboxTemplateToleration,
) bool {
	requiredSelectors := map[string]string{
		"node.kubernetes.io/instance-type":     kubernetesCocoonVirtualNodeType,
		kubernetesCocoonKVMReadyLabel:          "true",
		kubernetesCocoonHostSupervisorLabel:    kubernetesCocoonHostSupervisorV1,
		kubernetesCocoonProviderTransportLabel: kubernetesCocoonProviderTransportV2,
		kubernetesCocoonIsolationProfileLabel:  kubernetesCocoonIsolationProfileV1,
	}
	for key, value := range requiredSelectors {
		if strings.TrimSpace(nodeSelector[key]) != value {
			return false
		}
	}
	for _, toleration := range tolerations {
		operator := strings.TrimSpace(toleration.Operator)
		if operator == "" {
			operator = "Equal"
		}
		effect := strings.TrimSpace(toleration.Effect)
		if strings.TrimSpace(toleration.Key) == "virtual-kubelet.io/provider" &&
			operator == "Equal" && strings.TrimSpace(toleration.Value) == kubernetesCocoonVirtualProvider &&
			effect == "NoSchedule" {
			return true
		}
	}
	return false
}

func kubernetesCocoonSupervisorHeartbeatReady(annotations map[string]string, observedAt time.Time) bool {
	instanceID, err := uuid.Parse(strings.TrimSpace(annotations[kubernetesCocoonSupervisorInstanceAnnotation]))
	if err != nil || instanceID == uuid.Nil {
		return false
	}
	heartbeatAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(annotations[kubernetesCocoonSupervisorObservedAtAnnotation]))
	if err != nil {
		return false
	}
	age := observedAt.Sub(heartbeatAt)
	return age >= -kubernetesCocoonSupervisorHeartbeatFutureSkew && age <= kubernetesCocoonSupervisorHeartbeatMaxAge
}

func kubernetesSandboxAcceptanceRuntimeImage(
	backend string,
	observation kubernetesSandboxAcceptanceObservation,
) string {
	if backend == string(kubernetesAllocationBackendSandboxOperatorCocoon) {
		return strings.TrimSpace(observation.TemplateSandboxRuntimeImage)
	}
	return strings.TrimSpace(observation.TemplateAgentdImage)
}

func kubernetesSandboxPodRuntimeImage(backend string, pod kubernetesPod) string {
	if backend == string(kubernetesAllocationBackendSandboxOperatorCocoon) {
		return strings.TrimSpace(pod.SandboxRuntimeImage)
	}
	return strings.TrimSpace(pod.AgentdImage)
}

func (c *kubernetesHTTPClient) sandboxCRDReady(ctx context.Context, name string) (bool, error) {
	var crd struct {
		Status struct {
			StoredVersions []string `json:"storedVersions"`
			Conditions     []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := c.do(
		ctx, http.MethodGet, "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/"+url.PathEscape(name),
		nil, &crd, http.StatusOK,
	); err != nil {
		return false, err
	}
	stored := false
	for _, version := range crd.Status.StoredVersions {
		if version == "v1beta1" {
			stored = true
			break
		}
	}
	established := false
	for _, condition := range crd.Status.Conditions {
		if condition.Type == "Established" && condition.Status == "True" {
			established = true
			break
		}
	}
	return stored && established, nil
}

func (c *kubernetesHTTPClient) getOptional(ctx context.Context, path string, output any) (bool, error) {
	err := c.do(ctx, http.MethodGet, path, nil, output, http.StatusOK)
	var statusErr *kubernetesAPIStatusError
	if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}

func kubernetesSandboxNamespacedPath(namespace, resource, name string) string {
	return "/apis/extensions.agents.x-k8s.io/v1beta1/namespaces/" + url.PathEscape(namespace) + "/" +
		resource + "/" + url.PathEscape(name)
}

func kubernetesCoreSandboxNamespacedPath(namespace, resource, name string) string {
	return "/apis/agents.x-k8s.io/v1beta1/namespaces/" + url.PathEscape(namespace) + "/" +
		resource + "/" + url.PathEscape(name)
}
