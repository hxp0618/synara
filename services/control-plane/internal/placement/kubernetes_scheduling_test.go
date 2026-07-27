package placement

import "testing"

func TestNormalizeKubernetesSchedulingTemplateKeepsPriorityNonPreempting(t *testing.T) {
	normalized, err := NormalizeKubernetesSchedulingTemplate(map[string]any{
		"priorityClassName": " interactive-high ",
		"preemptionPolicy":  "Never",
		"nodeSelector":      map[string]any{" pool ": " warm "},
		"tolerations":       []any{map[string]any{"key": "warm", "operator": "Exists"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if normalized.PriorityClassName != "interactive-high" ||
		normalized.PreemptionPolicy != KubernetesPreemptionPolicyNever ||
		normalized.NodeSelector["pool"] != "warm" || len(normalized.Tolerations) != 1 {
		t.Fatalf("normalized Kubernetes scheduling template = %#v", normalized)
	}
}

func TestNormalizeKubernetesSchedulingTemplateDefaultsToNonPreempting(t *testing.T) {
	normalized, err := NormalizeKubernetesSchedulingTemplate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.PreemptionPolicy != KubernetesPreemptionPolicyNever {
		t.Fatalf("default preemptionPolicy = %q", normalized.PreemptionPolicy)
	}
}

func TestNormalizeKubernetesSchedulingTemplateRejectsPreemption(t *testing.T) {
	for _, policy := range []string{"PreemptLowerPriority", "never", "Default"} {
		t.Run(policy, func(t *testing.T) {
			_, err := NormalizeKubernetesSchedulingTemplate(map[string]any{"preemptionPolicy": policy})
			if code := problemCode(err); code != "worker_pool_preemption_unsupported" {
				t.Fatalf("preemption policy %q problem code = %q (error: %v)", policy, code, err)
			}
		})
	}
}

func TestNormalizeKubernetesSchedulingTemplateRejectsMalformedOrUnknownFields(t *testing.T) {
	for _, test := range []struct {
		name     string
		template map[string]any
		wantCode string
	}{
		{name: "non-string preemption", template: map[string]any{"preemptionPolicy": true}, wantCode: "worker_pool_scheduling_template_invalid"},
		{name: "empty preemption", template: map[string]any{"preemptionPolicy": ""}, wantCode: "worker_pool_scheduling_template_invalid"},
		{name: "unknown field", template: map[string]any{"schedulerName": "custom"}, wantCode: "worker_pool_scheduling_template_unsupported"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := NormalizeKubernetesSchedulingTemplate(test.template)
			if code := problemCode(err); code != test.wantCode {
				t.Fatalf("problem code = %q, want %q (error: %v)", code, test.wantCode, err)
			}
		})
	}
}
