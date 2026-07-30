package executiontargets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const (
	kubernetesGVisorRuntimeHandlerAnnotation   = "synara.io/gvisor-runtime-handler"
	kubernetesGVisorRuntimeVersionAnnotation   = "synara.io/gvisor-runtime-version"
	kubernetesGVisorRuntimeBinaryAnnotation    = "synara.io/gvisor-runtime-binary-sha256"
	kubernetesGVisorRuntimeConfigAnnotation    = "synara.io/gvisor-runtime-config-sha256"
	kubernetesGVisorAttestorInstanceAnnotation = "synara.io/gvisor-attestor-instance"
	kubernetesGVisorAttestedAtAnnotation       = "synara.io/gvisor-attested-at"
	kubernetesGVisorAttestationMaxAge          = 45 * time.Second
	kubernetesGVisorAttestationFutureSkew      = 5 * time.Second
)

type kubernetesHTTPFactory struct{}

func (kubernetesHTTPFactory) Open(configuration kubernetesTargetConfiguration) (kubernetesClient, error) {
	return newKubernetesHTTPClient(configuration)
}

type kubernetesHTTPClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func (c *kubernetesHTTPClient) Apply(ctx context.Context, path string, object map[string]any) error {
	query := url.Values{"fieldManager": {"synara-control-plane"}, "force": {"true"}}
	return c.do(ctx, http.MethodPatch, path+"?"+query.Encode(), object, nil, http.StatusOK, http.StatusCreated)
}

func (c *kubernetesHTTPClient) AttestPodPIDsLimit(
	ctx context.Context,
	nodeSelector map[string]string,
	maximum uint64,
	allocationBackend string,
) error {
	if maximum == 0 {
		return errors.New("Kubernetes Target pidsLimit is required")
	}
	selectorParts := make([]string, 0, len(nodeSelector))
	for key, value := range nodeSelector {
		selectorParts = append(selectorParts, key+"="+value)
	}
	sort.Strings(selectorParts)
	query := url.Values{}
	if len(selectorParts) > 0 {
		query.Set("labelSelector", strings.Join(selectorParts, ","))
	}
	path := "/api/v1/nodes"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var nodes struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &nodes, http.StatusOK); err != nil {
		return fmt.Errorf("list eligible Kubernetes nodes: %w", err)
	}
	if len(nodes.Items) == 0 {
		return errors.New("Kubernetes Target has no eligible nodes for PID-limit attestation")
	}
	type eligibleNode struct {
		name        string
		annotations map[string]string
	}
	eligible := make([]eligibleNode, 0, len(nodes.Items))
	seen := make(map[string]struct{}, len(nodes.Items))
	for _, item := range nodes.Items {
		name := strings.TrimSpace(item.Metadata.Name)
		if name == "" {
			return errors.New("Kubernetes node list contains an empty name")
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("Kubernetes node list contains duplicate node %q", name)
		}
		seen[name] = struct{}{}
		eligible = append(eligible, eligibleNode{name: name, annotations: item.Metadata.Annotations})
	}
	sort.Slice(eligible, func(left, right int) bool { return eligible[left].name < eligible[right].name })
	for _, node := range eligible {
		if allocationBackend == string(kubernetesAllocationBackendSandboxOperatorCocoon) {
			if err := attestCocoonHostPIDsLimit(node.name, node.annotations, maximum); err != nil {
				return err
			}
			continue
		}
		if err := c.attestNodePodPIDsLimit(ctx, node.name, maximum); err != nil {
			return err
		}
	}
	return nil
}

func (c *kubernetesHTTPClient) RuntimeIsolationCapabilities(
	ctx context.Context,
	configuration kubernetesTargetConfiguration,
	now time.Time,
) ([]runtimeIsolationCapability, error) {
	if configuration.AllocationBackend == string(kubernetesAllocationBackendSandboxOperatorCocoon) {
		return []runtimeIsolationCapability{{
			Runtime: runtimeIsolationFirecracker, Profile: platform.IsolationMicroVM,
		}}, nil
	}
	capabilities := []runtimeIsolationCapability{{
		Runtime: runtimeIsolationRunc, Profile: platform.IsolationKubernetesRestricted,
	}}
	if configuration.RuntimeIsolation == nil ||
		(!slices.Contains(configuration.RuntimeIsolation.Preferred, runtimeIsolationGVisor) &&
			configuration.RuntimeIsolation.Runtime != runtimeIsolationGVisor) {
		return capabilities, nil
	}
	capability, err := c.attestGVisorRuntime(ctx, configuration, now)
	if err != nil {
		return capabilities, err
	}
	return append([]runtimeIsolationCapability{capability}, capabilities...), nil
}

func (c *kubernetesHTTPClient) attestGVisorRuntime(
	ctx context.Context,
	configuration kubernetesTargetConfiguration,
	now time.Time,
) (runtimeIsolationCapability, error) {
	runtimeClassName := strings.TrimSpace(configuration.RuntimeIsolation.RuntimeClassName)
	var runtimeClass struct {
		Handler    string `json:"handler"`
		Scheduling *struct {
			NodeSelector map[string]string `json:"nodeSelector"`
			Tolerations  []map[string]any  `json:"tolerations"`
		} `json:"scheduling"`
	}
	if err := c.do(
		ctx,
		http.MethodGet,
		"/apis/node.k8s.io/v1/runtimeclasses/"+url.PathEscape(runtimeClassName),
		nil,
		&runtimeClass,
		http.StatusOK,
	); err != nil {
		return runtimeIsolationCapability{}, problem.Wrap(
			503,
			"gvisor_runtime_class_unavailable",
			"The configured gVisor RuntimeClass is unavailable.",
			err,
		)
	}
	if strings.TrimSpace(runtimeClass.Handler) != kubernetesApprovedGVisorRuntimeHandler {
		return runtimeIsolationCapability{}, problem.New(
			503,
			"gvisor_runtime_handler_unapproved",
			"The configured RuntimeClass does not use the approved gVisor runtime handler.",
		)
	}

	nodeSelector := cloneStringMap(configuration.NodeSelector)
	tolerations := cloneObjectList(configuration.Tolerations)
	if runtimeClass.Scheduling != nil {
		for key, value := range runtimeClass.Scheduling.NodeSelector {
			if current, exists := nodeSelector[key]; exists && current != value {
				return runtimeIsolationCapability{}, problem.New(
					503,
					"gvisor_eligible_node_unattested",
					"The RuntimeClass and Target node selectors do not identify a consistent gVisor node set.",
				)
			}
			nodeSelector[key] = value
		}
		tolerations = append(tolerations, cloneObjectList(runtimeClass.Scheduling.Tolerations)...)
	}
	nodes, err := c.listNodes(ctx, nodeSelector, tolerations)
	if err != nil {
		return runtimeIsolationCapability{}, problem.Wrap(
			503,
			"gvisor_eligible_node_unattested",
			"Eligible gVisor nodes could not be observed.",
			err,
		)
	}
	if len(nodes) == 0 {
		return runtimeIsolationCapability{}, problem.New(
			503,
			"gvisor_eligible_node_unattested",
			"The Target has no eligible attested gVisor nodes.",
		)
	}

	type attestedNode struct {
		Name       string `json:"name"`
		Instance   string `json:"instance"`
		Version    string `json:"version"`
		BinaryHash string `json:"binaryHash"`
		ConfigHash string `json:"configHash"`
	}
	attested := make([]attestedNode, 0, len(nodes))
	earliestExpiry := time.Time{}
	for _, node := range nodes {
		annotations := node.annotations
		instance := strings.TrimSpace(annotations[kubernetesGVisorAttestorInstanceAnnotation])
		if _, parseErr := uuid.Parse(instance); parseErr != nil {
			return runtimeIsolationCapability{}, problem.New(
				503,
				"gvisor_eligible_node_unattested",
				"An eligible node does not expose a valid gVisor attestor identity.",
			)
		}
		version := strings.TrimSpace(annotations[kubernetesGVisorRuntimeVersionAnnotation])
		binaryHash := strings.TrimSpace(annotations[kubernetesGVisorRuntimeBinaryAnnotation])
		configHash := strings.TrimSpace(annotations[kubernetesGVisorRuntimeConfigAnnotation])
		if strings.TrimSpace(annotations[kubernetesGVisorRuntimeHandlerAnnotation]) != kubernetesApprovedGVisorRuntimeHandler ||
			version == "" || !isLowerHexSHA256(binaryHash) || !isLowerHexSHA256(configHash) {
			return runtimeIsolationCapability{}, problem.New(
				503,
				"gvisor_eligible_node_unattested",
				"An eligible node does not expose the approved gVisor runtime identity.",
			)
		}
		observedRaw := strings.TrimSpace(annotations[kubernetesGVisorAttestedAtAnnotation])
		observedAt, parseErr := time.Parse(time.RFC3339Nano, observedRaw)
		if parseErr != nil || observedAt.Before(now.Add(-kubernetesGVisorAttestationMaxAge)) ||
			observedAt.After(now.Add(kubernetesGVisorAttestationFutureSkew)) {
			return runtimeIsolationCapability{}, problem.New(
				503,
				"gvisor_attestation_stale",
				"An eligible node has a missing or stale gVisor runtime attestation.",
			)
		}
		nodeExpiry := observedAt.Add(kubernetesGVisorAttestationMaxAge)
		if earliestExpiry.IsZero() || nodeExpiry.Before(earliestExpiry) {
			earliestExpiry = nodeExpiry
		}
		attested = append(attested, attestedNode{
			Name: node.name, Instance: instance, Version: version,
			BinaryHash: binaryHash, ConfigHash: configHash,
		})
	}
	sort.Slice(attested, func(left, right int) bool { return attested[left].Name < attested[right].Name })
	payload, err := json.Marshal(struct {
		RuntimeClass string         `json:"runtimeClass"`
		Handler      string         `json:"handler"`
		Nodes        []attestedNode `json:"nodes"`
	}{RuntimeClass: runtimeClassName, Handler: kubernetesApprovedGVisorRuntimeHandler, Nodes: attested})
	if err != nil {
		return runtimeIsolationCapability{}, err
	}
	digest := sha256.Sum256(payload)
	attestedAt := now.UTC()
	expiresAt := earliestExpiry.UTC()
	eligibleNodeNames := make([]string, 0, len(attested))
	for _, node := range attested {
		eligibleNodeNames = append(eligibleNodeNames, node.Name)
	}
	return runtimeIsolationCapability{
		Runtime: runtimeIsolationGVisor, Profile: platform.IsolationGVisorSandboxed,
		RuntimeClassName: runtimeClassName, AttestationDigest: hex.EncodeToString(digest[:]),
		AttestedAt: &attestedAt, AttestationExpiresAt: &expiresAt,
		EligibleNodeNames: eligibleNodeNames,
	}, nil
}

func (c *kubernetesHTTPClient) EnsureRuntimeIsolationCanary(
	ctx context.Context,
	target persistence.ExecutionTarget,
	configuration kubernetesTargetConfiguration,
	credential *ImagePullCredential,
) error {
	decision := configuration.RuntimeIsolationDecision
	if decision == nil || decision.EffectiveRuntime != runtimeIsolationGVisor {
		return nil
	}
	identityPayload, err := json.Marshal(struct {
		TargetID          uuid.UUID `json:"targetId"`
		Image             string    `json:"image"`
		RuntimeClassName  string    `json:"runtimeClassName"`
		AttestationDigest string    `json:"attestationDigest"`
	}{
		TargetID: target.ID, Image: configuration.Image,
		RuntimeClassName: decision.RuntimeClassName, AttestationDigest: decision.AttestationDigest,
	})
	if err != nil {
		return err
	}
	digest := sha256.Sum256(identityPayload)
	name := "synara-gvisor-canary-" + strings.ReplaceAll(target.ID.String(), "-", "")[:12] + "-" +
		hex.EncodeToString(digest[:])[:12]
	selector := url.QueryEscape("synara.io/gvisor-canary-target-id=" + target.ID.String())
	var canaries struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
				UID  string `json:"uid"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := c.do(
		ctx,
		http.MethodGet,
		strings.TrimSuffix(kubernetesNamespacedPath(configuration.Namespace, "pods", ""), "/")+"?labelSelector="+selector,
		nil,
		&canaries,
		http.StatusOK,
	); err != nil {
		return problem.Wrap(503, "gvisor_canary_failed", "Existing gVisor runtime canaries could not be listed.", err)
	}
	for _, canary := range canaries.Items {
		if canary.Metadata.Name == name {
			continue
		}
		if strings.TrimSpace(canary.Metadata.Name) == "" || strings.TrimSpace(canary.Metadata.UID) == "" {
			return problem.New(503, "gvisor_canary_failed", "An existing gVisor runtime canary has an invalid identity.")
		}
		if err := c.DeletePod(ctx, configuration.Namespace, canary.Metadata.Name, canary.Metadata.UID); err != nil {
			return problem.Wrap(503, "gvisor_canary_failed", "A stale gVisor runtime canary could not be deleted.", err)
		}
	}
	path := kubernetesNamespacedPath(configuration.Namespace, "pods", name)
	var observed struct {
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &observed, http.StatusOK); err == nil {
		switch strings.TrimSpace(observed.Status.Phase) {
		case "Succeeded":
			return nil
		case "Failed":
			return problem.New(503, "gvisor_canary_failed", "The gVisor runtime compatibility canary failed.")
		default:
			return problem.New(503, "gvisor_canary_pending", "The gVisor runtime compatibility canary is not complete.")
		}
	} else {
		var statusErr *kubernetesAPIStatusError
		if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusNotFound {
			return problem.Wrap(503, "gvisor_canary_failed", "The gVisor runtime compatibility canary could not be observed.", err)
		}
	}

	requests := map[string]any{"cpu": "10m", "memory": "16Mi", "ephemeral-storage": "16Mi"}
	limits := map[string]any{"cpu": "100m", "memory": "64Mi", "ephemeral-storage": "64Mi"}
	pod := map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{
			"name": name, "namespace": configuration.Namespace,
			"labels": map[string]string{
				"synara.io/managed": "true", "synara.io/gvisor-canary-target-id": target.ID.String(),
			},
		},
		"spec": map[string]any{
			"runtimeClassName":   decision.RuntimeClassName,
			"serviceAccountName": configuration.ServiceAccountName, "automountServiceAccountToken": false,
			"enableServiceLinks": false, "hostNetwork": false, "hostPID": false, "hostIPC": false,
			"restartPolicy": "Never", "terminationGracePeriodSeconds": 5,
			"securityContext": map[string]any{
				"runAsNonRoot": true, "fsGroup": 10001,
				"seccompProfile": map[string]any{"type": "RuntimeDefault"},
			},
			"containers": []any{map[string]any{
				"name": "runtime-verifier", "image": configuration.Image,
				"imagePullPolicy": configuration.ImagePullPolicy,
				"command":         []any{"/usr/local/bin/synara-agentd", platform.GVisorRuntimeVerifyArgument},
				"securityContext": kubernetesRestrictedContainerSecurityContext(),
				"resources":       map[string]any{"requests": requests, "limits": limits},
				"volumeMounts":    []any{map[string]any{"name": "tmp", "mountPath": "/tmp"}},
			}},
			"volumes": []any{map[string]any{"name": "tmp", "emptyDir": map[string]any{"sizeLimit": "16Mi"}}},
		},
	}
	if len(configuration.NodeSelector) > 0 {
		pod["spec"].(map[string]any)["nodeSelector"] = cloneStringMap(configuration.NodeSelector)
	}
	if len(configuration.Tolerations) > 0 {
		pod["spec"].(map[string]any)["tolerations"] = cloneObjectList(configuration.Tolerations)
	}
	if len(configuration.ImagePullSecrets) > 0 || credential != nil {
		secrets := make([]any, 0, len(configuration.ImagePullSecrets)+1)
		for _, secretName := range configuration.ImagePullSecrets {
			secrets = append(secrets, map[string]any{"name": secretName})
		}
		if credential != nil {
			secrets = append(secrets, map[string]any{"name": kubernetesRegistrySecretName(target.ID)})
		}
		pod["spec"].(map[string]any)["imagePullSecrets"] = secrets
	}
	if err := c.Apply(ctx, path, pod); err != nil {
		return problem.Wrap(503, "gvisor_canary_failed", "The gVisor runtime compatibility canary could not be created.", err)
	}
	return problem.New(503, "gvisor_canary_pending", "The gVisor runtime compatibility canary is not complete.")
}

type kubernetesEligibleNode struct {
	name        string
	annotations map[string]string
}

type kubernetesNodeTaint struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Effect string `json:"effect"`
}

func (c *kubernetesHTTPClient) listNodes(
	ctx context.Context,
	nodeSelector map[string]string,
	tolerations []any,
) ([]kubernetesEligibleNode, error) {
	selectorParts := make([]string, 0, len(nodeSelector))
	for key, value := range nodeSelector {
		selectorParts = append(selectorParts, key+"="+value)
	}
	sort.Strings(selectorParts)
	query := url.Values{}
	if len(selectorParts) > 0 {
		query.Set("labelSelector", strings.Join(selectorParts, ","))
	}
	path := "/api/v1/nodes"
	if encoded := query.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var nodes struct {
		Items []struct {
			Metadata struct {
				Name              string            `json:"name"`
				Annotations       map[string]string `json:"annotations"`
				DeletionTimestamp *time.Time        `json:"deletionTimestamp"`
			} `json:"metadata"`
			Spec struct {
				Unschedulable bool                  `json:"unschedulable"`
				Taints        []kubernetesNodeTaint `json:"taints"`
			} `json:"spec"`
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &nodes, http.StatusOK); err != nil {
		return nil, err
	}
	result := make([]kubernetesEligibleNode, 0, len(nodes.Items))
	seen := make(map[string]struct{}, len(nodes.Items))
	for _, item := range nodes.Items {
		name := strings.TrimSpace(item.Metadata.Name)
		if name == "" {
			return nil, errors.New("Kubernetes node list contains an empty name")
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("Kubernetes node list contains duplicate node %q", name)
		}
		seen[name] = struct{}{}
		ready := false
		for _, condition := range item.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				ready = true
				break
			}
		}
		if !ready || item.Spec.Unschedulable || item.Metadata.DeletionTimestamp != nil ||
			!kubernetesNodeTaintsTolerated(item.Spec.Taints, tolerations) {
			continue
		}
		result = append(result, kubernetesEligibleNode{name: name, annotations: item.Metadata.Annotations})
	}
	sort.Slice(result, func(left, right int) bool { return result[left].name < result[right].name })
	return result, nil
}

func kubernetesNodeTaintsTolerated(
	taints []kubernetesNodeTaint,
	tolerations []any,
) bool {
	for _, taint := range taints {
		if taint.Effect != "NoSchedule" && taint.Effect != "NoExecute" {
			continue
		}
		matched := false
		for _, rawToleration := range tolerations {
			toleration, ok := rawToleration.(map[string]any)
			if !ok {
				continue
			}
			key, _ := toleration["key"].(string)
			effect, _ := toleration["effect"].(string)
			if effect != "" && effect != taint.Effect {
				continue
			}
			operator, _ := toleration["operator"].(string)
			if operator == "" {
				operator = "Equal"
			}
			switch operator {
			case "Exists":
				matched = key == "" || key == taint.Key
			case "Equal":
				value, _ := toleration["value"].(string)
				matched = key == taint.Key && value == taint.Value
			}
			if matched {
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func isLowerHexSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func attestCocoonHostPIDsLimit(name string, annotations map[string]string, maximum uint64) error {
	value := strings.TrimSpace(annotations["synara.io/host-pids-limit"])
	limit, err := strconv.ParseUint(value, 10, 64)
	if err != nil || limit == 0 || limit > maximum {
		return fmt.Errorf("Cocoon node %q host agentd pidsLimit=%q is not within 1..%d", name, value, maximum)
	}
	return nil
}

func (c *kubernetesHTTPClient) attestNodePodPIDsLimit(
	ctx context.Context,
	name string,
	maximum uint64,
) error {
	name = strings.TrimSpace(name)
	if name == "" || maximum == 0 {
		return errors.New("Kubernetes node name and Target pidsLimit are required")
	}
	var config struct {
		KubeletConfig struct {
			PodPIDsLimit int64 `json:"podPidsLimit"`
		} `json:"kubeletconfig"`
	}
	configPath := "/api/v1/nodes/" + url.PathEscape(name) + "/proxy/configz"
	if err := c.do(ctx, http.MethodGet, configPath, nil, &config, http.StatusOK); err != nil {
		return fmt.Errorf("attest Kubernetes node %q kubelet podPidsLimit: %w", name, err)
	}
	if config.KubeletConfig.PodPIDsLimit <= 0 ||
		uint64(config.KubeletConfig.PodPIDsLimit) > maximum {
		return fmt.Errorf(
			"Kubernetes node %q kubelet podPidsLimit=%d is not within 1..%d",
			name,
			config.KubeletConfig.PodPIDsLimit,
			maximum,
		)
	}
	return nil
}

func (c *kubernetesHTTPClient) GetPriorityClass(ctx context.Context, name string) (kubernetesPriorityClass, error) {
	var response struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Value            int32  `json:"value"`
		GlobalDefault    bool   `json:"globalDefault"`
		PreemptionPolicy string `json:"preemptionPolicy"`
	}
	path := "/apis/scheduling.k8s.io/v1/priorityclasses/" + url.PathEscape(strings.TrimSpace(name))
	if err := c.do(ctx, http.MethodGet, path, nil, &response, http.StatusOK); err != nil {
		return kubernetesPriorityClass{}, err
	}
	return kubernetesPriorityClass{
		Name: response.Metadata.Name, Value: response.Value,
		GlobalDefault: response.GlobalDefault, PreemptionPolicy: response.PreemptionPolicy,
	}, nil
}

func (c *kubernetesHTTPClient) GetResourceQuota(
	ctx context.Context,
	namespace, name string,
) (kubernetesResourceQuota, error) {
	var response struct {
		Status struct {
			Hard map[string]string `json:"hard"`
			Used map[string]string `json:"used"`
		} `json:"status"`
	}
	if err := c.do(
		ctx,
		http.MethodGet,
		kubernetesNamespacedPath(namespace, "resourcequotas", name),
		nil,
		&response,
		http.StatusOK,
	); err != nil {
		return kubernetesResourceQuota{}, err
	}
	return kubernetesResourceQuota{Hard: response.Status.Hard, Used: response.Status.Used}, nil
}

func (c *kubernetesHTTPClient) ListPods(ctx context.Context, namespace string, targetID uuid.UUID) ([]kubernetesPod, error) {
	return c.listPods(ctx, namespace, kubernetesTargetLabel+"="+targetID.String())
}

func (c *kubernetesHTTPClient) ListPodUIDs(ctx context.Context, namespace string) ([]string, error) {
	pods, err := c.listPods(ctx, namespace, "")
	if err != nil {
		return nil, err
	}
	uids := make([]string, 0, len(pods))
	for _, pod := range pods {
		uids = append(uids, pod.UID)
	}
	return uids, nil
}

func (c *kubernetesHTTPClient) listPods(ctx context.Context, namespace, labelSelector string) ([]kubernetesPod, error) {
	items := make([]kubernetesPod, 0)
	continueToken := ""
	seenContinueTokens := map[string]struct{}{}
	for {
		var response struct {
			Metadata struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
			Items []struct {
				Metadata struct {
					Name              string            `json:"name"`
					UID               string            `json:"uid"`
					CreationTimestamp time.Time         `json:"creationTimestamp"`
					Labels            map[string]string `json:"labels"`
					Annotations       map[string]string `json:"annotations"`
					OwnerReferences   []struct {
						Kind       string `json:"kind"`
						UID        string `json:"uid"`
						Controller bool   `json:"controller"`
					} `json:"ownerReferences"`
				} `json:"metadata"`
				Spec struct {
					Containers []struct {
						Name      string `json:"name"`
						Image     string `json:"image"`
						Resources struct {
							Requests map[string]string `json:"requests"`
						} `json:"resources"`
					} `json:"containers"`
				} `json:"spec"`
				Status struct {
					Phase      string `json:"phase"`
					Reason     string `json:"reason"`
					Conditions []struct {
						Type   string `json:"type"`
						Status string `json:"status"`
						Reason string `json:"reason"`
					} `json:"conditions"`
					ContainerStatuses []struct {
						Name  string `json:"name"`
						State struct {
							Waiting *struct {
								Reason string `json:"reason"`
							} `json:"waiting"`
							Terminated *struct {
								ExitCode int    `json:"exitCode"`
								Reason   string `json:"reason"`
							} `json:"terminated"`
						} `json:"state"`
						LastState struct {
							Terminated *struct {
								Reason string `json:"reason"`
							} `json:"terminated"`
						} `json:"lastState"`
					} `json:"containerStatuses"`
				} `json:"status"`
			} `json:"items"`
		}
		query := url.Values{}
		query.Set("limit", "100")
		if labelSelector != "" {
			query.Set("labelSelector", labelSelector)
		}
		if continueToken != "" {
			query.Set("continue", continueToken)
		}
		path := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/pods"
		if encoded := query.Encode(); encoded != "" {
			path += "?" + encoded
		}
		if err := c.do(ctx, http.MethodGet, path, nil, &response, http.StatusOK); err != nil {
			return nil, err
		}
		for _, item := range response.Items {
			controllerOwnerKind, controllerOwnerUID := "", ""
			for _, owner := range item.Metadata.OwnerReferences {
				if owner.Controller {
					controllerOwnerKind = strings.TrimSpace(owner.Kind)
					controllerOwnerUID = strings.TrimSpace(owner.UID)
					break
				}
			}
			resourceRequests := map[string]string{}
			agentdImage := ""
			sandboxRuntimeImage := ""
			for index, container := range item.Spec.Containers {
				if index == 0 {
					sandboxRuntimeImage = strings.TrimSpace(container.Image)
				}
				if strings.TrimSpace(container.Name) == "agentd" {
					resourceRequests = container.Resources.Requests
					agentdImage = strings.TrimSpace(container.Image)
					break
				}
			}
			conditions := make([]kubernetesPodCondition, 0, len(item.Status.Conditions))
			for _, condition := range item.Status.Conditions {
				conditions = append(conditions, kubernetesPodCondition{
					Type: condition.Type, Status: condition.Status, Reason: condition.Reason,
				})
			}
			containers := make([]kubernetesContainerStatus, 0, len(item.Status.ContainerStatuses))
			for _, status := range item.Status.ContainerStatuses {
				container := kubernetesContainerStatus{Name: status.Name}
				if status.State.Waiting != nil {
					container.WaitingReason = status.State.Waiting.Reason
				}
				if status.State.Terminated != nil {
					container.Terminated = true
					container.ExitCode = status.State.Terminated.ExitCode
					container.TerminatedReason = status.State.Terminated.Reason
				}
				if status.LastState.Terminated != nil {
					container.LastTerminatedReason = status.LastState.Terminated.Reason
				}
				containers = append(containers, container)
			}
			items = append(items, kubernetesPod{
				Name: item.Metadata.Name, UID: item.Metadata.UID, AgentdImage: agentdImage,
				SandboxRuntimeImage: sandboxRuntimeImage, Phase: item.Status.Phase,
				Reason: item.Status.Reason, CreatedAt: item.Metadata.CreationTimestamp.UTC(),
				Labels: item.Metadata.Labels, Annotations: item.Metadata.Annotations,
				ControllerOwnerKind: controllerOwnerKind, ControllerOwnerUID: controllerOwnerUID,
				Conditions: conditions, Containers: containers, ResourceRequests: resourceRequests,
			})
		}
		continueToken = strings.TrimSpace(response.Metadata.Continue)
		if continueToken == "" {
			break
		}
		if _, seen := seenContinueTokens[continueToken]; seen {
			return nil, errors.New("Kubernetes Pod list repeated its continuation token")
		}
		seenContinueTokens[continueToken] = struct{}{}
	}
	return items, nil
}

func (c *kubernetesHTTPClient) DeletePod(ctx context.Context, namespace, name, uid string) error {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return errors.New("Kubernetes Pod UID is required for safe deletion")
	}
	path := kubernetesNamespacedPath(namespace, "pods", name) + "?gracePeriodSeconds=30&propagationPolicy=Background"
	err := c.do(ctx, http.MethodDelete, path, map[string]any{
		"apiVersion": "v1", "kind": "DeleteOptions",
		"preconditions": map[string]any{"uid": uid},
	}, nil, http.StatusOK, http.StatusAccepted, http.StatusNotFound)
	var statusErr *kubernetesAPIStatusError
	if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusConflict {
		return fmt.Errorf("%w: %s", errKubernetesPodUIDPreconditionFailed, statusErr.Detail)
	}
	return err
}

func (c *kubernetesHTTPClient) do(
	ctx context.Context,
	method, path string,
	input, output any,
	accepted ...int,
) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	if input != nil {
		contentType := "application/json"
		if method == http.MethodPatch {
			contentType = "application/apply-patch+yaml"
		}
		request.Header.Set("Content-Type", contentType)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	acceptedStatus := false
	for _, status := range accepted {
		if response.StatusCode == status {
			acceptedStatus = true
			break
		}
	}
	if !acceptedStatus {
		var status struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		}
		_ = json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&status)
		detail := strings.TrimSpace(status.Reason)
		if message := strings.TrimSpace(status.Message); message != "" {
			if detail != "" {
				detail += ": "
			}
			detail += message
		}
		if detail == "" {
			detail = http.StatusText(response.StatusCode)
		}
		return &kubernetesAPIStatusError{StatusCode: response.StatusCode, Detail: detail}
	}
	if output == nil {
		_, _ = io.Copy(io.Discard, response.Body)
		return nil
	}
	return json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(output)
}
