package executiontargets

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/placement"
)

func TestKubernetesObservabilityAuthorityIsTargetScoped(t *testing.T) {
	first := uuid.New()
	second := uuid.New()
	firstConfig := kubernetesObservabilityConfigMapName(first)
	secondConfig := kubernetesObservabilityConfigMapName(second)
	if firstConfig == secondConfig {
		t.Fatal("distinct Execution Targets shared an observability authority name")
	}
	if !strings.HasSuffix(firstConfig, first.String()) {
		t.Fatalf("observability authority omitted full Target identity: %q", firstConfig)
	}
}

func TestApplyKubernetesWorkerPoolSchedulingTemplateForcesNonPreemption(t *testing.T) {
	podSpec := map[string]any{
		"preemptionPolicy": placement.KubernetesPreemptionPolicyNever,
		"nodeSelector":     map[string]string{"zone": "local"},
		"tolerations":      []any{map[string]any{"key": "base", "operator": "Exists"}},
	}
	err := applyKubernetesWorkerPoolSchedulingTemplate(podSpec, map[string]any{
		"priorityClassName": " interactive-high ",
		"preemptionPolicy":  "Never",
		"nodeSelector":      map[string]any{"pool": "warm"},
		"tolerations":       []any{map[string]any{"key": "warm", "operator": "Exists"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if podSpec["priorityClassName"] != "interactive-high" ||
		podSpec["preemptionPolicy"] != placement.KubernetesPreemptionPolicyNever {
		t.Fatalf("priority policy = %#v", podSpec)
	}
	nodeSelector, ok := podSpec["nodeSelector"].(map[string]string)
	if !ok || nodeSelector["zone"] != "local" || nodeSelector["pool"] != "warm" {
		t.Fatalf("merged nodeSelector = %#v", podSpec["nodeSelector"])
	}
	tolerations, ok := podSpec["tolerations"].([]any)
	if !ok || len(tolerations) != 2 {
		t.Fatalf("merged tolerations = %#v", podSpec["tolerations"])
	}
}

func TestApplyKubernetesWorkerPoolSchedulingTemplateRejectsPreemption(t *testing.T) {
	err := applyKubernetesWorkerPoolSchedulingTemplate(map[string]any{}, map[string]any{
		"priorityClassName": "interactive-high",
		"preemptionPolicy":  "PreemptLowerPriority",
	})
	assertProblemCode(t, err, 409, "worker_pool_preemption_unsupported")
}

func TestApplyKubernetesWorkerPoolSchedulingTemplateUsesDefaultNonPreemptingClass(t *testing.T) {
	podSpec := map[string]any{
		"priorityClassName": kubernetesWorkerDefaultPriorityClassName,
		"preemptionPolicy":  placement.KubernetesPreemptionPolicyNever,
	}
	if err := applyKubernetesWorkerPoolSchedulingTemplate(podSpec, nil); err != nil {
		t.Fatal(err)
	}
	if podSpec["priorityClassName"] != kubernetesWorkerDefaultPriorityClassName ||
		podSpec["preemptionPolicy"] != placement.KubernetesPreemptionPolicyNever {
		t.Fatalf("default priority policy = %#v", podSpec)
	}
}

func TestValidateKubernetesWorkerPodPriorityClass(t *testing.T) {
	newPod := func(className string) map[string]any {
		return map[string]any{"spec": map[string]any{
			"priorityClassName": className,
			"preemptionPolicy":  placement.KubernetesPreemptionPolicyNever,
		}}
	}
	t.Run("accepts and caches non-preempting class", func(t *testing.T) {
		client := newFakeKubernetesClient()
		cache := map[string]error{}
		for range 2 {
			if err := validateKubernetesWorkerPodPriorityClass(t.Context(), client, newPod(kubernetesWorkerDefaultPriorityClassName), cache); err != nil {
				t.Fatal(err)
			}
		}
		if client.priorityClassReadCount[kubernetesWorkerDefaultPriorityClassName] != 1 {
			t.Fatalf("PriorityClass read count = %#v", client.priorityClassReadCount)
		}
	})
	t.Run("rejects preempting class", func(t *testing.T) {
		client := newFakeKubernetesClient()
		client.priorityClasses["preempting"] = kubernetesPriorityClass{Name: "preempting", PreemptionPolicy: "PreemptLowerPriority"}
		err := validateKubernetesWorkerPodPriorityClass(t.Context(), client, newPod("preempting"), map[string]error{})
		assertProblemCode(t, err, 409, "worker_pool_preemption_unsupported")
	})
	t.Run("rejects drifted default class", func(t *testing.T) {
		client := newFakeKubernetesClient()
		client.priorityClasses[kubernetesWorkerDefaultPriorityClassName] = kubernetesPriorityClass{
			Name: kubernetesWorkerDefaultPriorityClassName, Value: 100,
			PreemptionPolicy: placement.KubernetesPreemptionPolicyNever,
		}
		err := validateKubernetesWorkerPodPriorityClass(t.Context(), client, newPod(kubernetesWorkerDefaultPriorityClassName), map[string]error{})
		assertProblemCode(t, err, 409, "kubernetes_default_priority_class_invalid")
	})
	t.Run("rejects missing class", func(t *testing.T) {
		client := newFakeKubernetesClient()
		err := validateKubernetesWorkerPodPriorityClass(t.Context(), client, newPod("missing"), map[string]error{})
		assertProblemCode(t, err, 409, "worker_pool_priority_class_not_found")
	})
	t.Run("fails closed on API read error", func(t *testing.T) {
		client := newFakeKubernetesClient()
		client.priorityClassReadErr = &kubernetesAPIStatusError{StatusCode: http.StatusForbidden, Detail: "forbidden"}
		err := validateKubernetesWorkerPodPriorityClass(t.Context(), client, newPod(kubernetesWorkerDefaultPriorityClassName), map[string]error{})
		assertProblemCode(t, err, 502, "kubernetes_priority_class_read_failed")
	})
	t.Run("rejects malformed production Pod", func(t *testing.T) {
		client := newFakeKubernetesClient()
		err := validateKubernetesWorkerPodPriorityClass(t.Context(), client, map[string]any{"spec": map[string]any{}}, map[string]error{})
		assertProblemCode(t, err, 500, "kubernetes_worker_pod_priority_invalid")
	})
}
