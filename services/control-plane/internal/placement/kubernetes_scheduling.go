package placement

import (
	"strings"

	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const KubernetesPreemptionPolicyNever = "Never"

type KubernetesSchedulingTemplate struct {
	PriorityClassName string
	PreemptionPolicy  string
	NodeSelector      map[string]string
	Tolerations       []any
}

// NormalizeKubernetesSchedulingTemplate is the single validation authority for
// Worker Pool fields that are projected into a Kubernetes PodSpec. Priority is
// supported as scheduling order only: Synara v1 Workers must never displace an
// already running Pod.
func NormalizeKubernetesSchedulingTemplate(template map[string]any) (KubernetesSchedulingTemplate, error) {
	normalized := KubernetesSchedulingTemplate{
		PreemptionPolicy: KubernetesPreemptionPolicyNever,
		NodeSelector:     map[string]string{},
		Tolerations:      []any{},
	}
	for key := range template {
		switch key {
		case "priorityClassName", "preemptionPolicy", "nodeSelector", "tolerations":
		default:
			return KubernetesSchedulingTemplate{}, problem.New(409, "worker_pool_scheduling_template_unsupported", "Worker pool schedulingTemplate contains an unsupported field.")
		}
	}
	if value, found := template["priorityClassName"]; found {
		text, ok := value.(string)
		if !ok || strings.TrimSpace(text) == "" || strings.ContainsAny(text, "\r\n\t") {
			return KubernetesSchedulingTemplate{}, problem.New(409, "worker_pool_scheduling_template_invalid", "Worker pool priorityClassName must be a non-empty string.")
		}
		normalized.PriorityClassName = strings.TrimSpace(text)
	}
	if value, found := template["preemptionPolicy"]; found {
		text, ok := value.(string)
		if !ok || strings.TrimSpace(text) == "" || strings.ContainsAny(text, "\r\n\t") {
			return KubernetesSchedulingTemplate{}, problem.New(409, "worker_pool_scheduling_template_invalid", "Worker pool preemptionPolicy must be a non-empty string.")
		}
		if strings.TrimSpace(text) != KubernetesPreemptionPolicyNever {
			return KubernetesSchedulingTemplate{}, problem.New(
				409,
				"worker_pool_preemption_unsupported",
				"Kubernetes Worker pools support priority ordering only; preemptionPolicy must be Never.",
			)
		}
	}
	if value, found := template["nodeSelector"]; found {
		nodeSelector, err := normalizeKubernetesSchedulingStringMap(value, "nodeSelector")
		if err != nil {
			return KubernetesSchedulingTemplate{}, err
		}
		normalized.NodeSelector = nodeSelector
	}
	if value, found := template["tolerations"]; found {
		tolerations, err := normalizeKubernetesSchedulingTolerations(value)
		if err != nil {
			return KubernetesSchedulingTemplate{}, err
		}
		normalized.Tolerations = tolerations
	}
	return normalized, nil
}

func normalizeKubernetesSchedulingStringMap(value any, field string) (map[string]string, error) {
	raw, ok := value.(map[string]any)
	if !ok {
		return nil, problem.New(409, "worker_pool_scheduling_template_invalid", "Worker pool "+field+" must be an object.")
	}
	normalized := make(map[string]string, len(raw))
	for key, item := range raw {
		text, ok := item.(string)
		if !ok || strings.TrimSpace(key) == "" || strings.TrimSpace(text) == "" || strings.ContainsAny(key+text, "\r\n\t") {
			return nil, problem.New(409, "worker_pool_scheduling_template_invalid", "Worker pool "+field+" values must be non-empty strings.")
		}
		normalized[strings.TrimSpace(key)] = strings.TrimSpace(text)
	}
	return normalized, nil
}

func normalizeKubernetesSchedulingTolerations(value any) ([]any, error) {
	items, ok := value.([]any)
	if !ok {
		return nil, problem.New(409, "worker_pool_scheduling_template_invalid", "Worker pool tolerations must be an array.")
	}
	normalized := make([]any, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, problem.New(409, "worker_pool_scheduling_template_invalid", "Worker pool tolerations must contain objects.")
		}
		clone := make(map[string]any, len(object))
		for key, value := range object {
			clone[key] = value
		}
		normalized = append(normalized, clone)
	}
	return normalized, nil
}
