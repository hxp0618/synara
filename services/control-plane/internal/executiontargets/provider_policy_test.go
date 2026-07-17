package executiontargets

import "testing"

func TestCanonicalStage3Provider(t *testing.T) {
	tests := []struct {
		input string
		want  string
		valid bool
	}{
		{input: " claudeagent ", want: "claudeAgent", valid: true},
		{input: "CODEX", want: "codex", valid: true},
		{input: "droid", valid: false},
	}
	for _, test := range tests {
		got, valid := CanonicalStage3Provider(test.input)
		if got != test.want || valid != test.valid {
			t.Fatalf("CanonicalStage3Provider(%q) = (%q, %t), want (%q, %t)",
				test.input, got, valid, test.want, test.valid)
		}
	}
}

func TestParseProviderPolicyNormalizesExperimentalProviders(t *testing.T) {
	policy, err := ParseProviderPolicy(map[string]any{
		"providerPolicy": map[string]any{
			"experimentalProviders": []any{" OpenCode ", "CLAUDEAGENT", "codex"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"codex", "claudeAgent", "opencode"}
	if len(policy.ExperimentalProviders) != len(want) {
		t.Fatalf("experimental Providers = %#v, want %#v", policy.ExperimentalProviders, want)
	}
	for index := range want {
		if policy.ExperimentalProviders[index] != want[index] {
			t.Fatalf("experimental Providers = %#v, want %#v", policy.ExperimentalProviders, want)
		}
	}
	if !policy.ExperimentalProviderEnabled(" CLAUDEAGENT ") || policy.ExperimentalProviderEnabled("droid") {
		t.Fatalf("unexpected Provider enablement: %#v", policy)
	}
	enabled, err := ExperimentalProviderEnabled(map[string]any{}, "codex")
	if err != nil || enabled {
		t.Fatalf("missing policy enabled Codex: enabled=%t err=%v", enabled, err)
	}
}

func TestParseProviderPolicyAcceptsOnlyStage3ProviderSet(t *testing.T) {
	providers := []any{"codex", "claudeAgent", "cursor", "antigravity", "grok", "kilo", "opencode", "pi"}
	policy, err := ParseProviderPolicy(map[string]any{
		"providerPolicy": map[string]any{"experimentalProviders": providers},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(policy.ExperimentalProviders) != len(stage3ProviderOrder) {
		t.Fatalf("Stage 3 Provider set = %#v", policy.ExperimentalProviders)
	}
	for index := range stage3ProviderOrder {
		if policy.ExperimentalProviders[index] != stage3ProviderOrder[index] {
			t.Fatalf("Stage 3 Provider set = %#v, want %#v", policy.ExperimentalProviders, stage3ProviderOrder)
		}
	}
}

func TestParseProviderPolicyRejectsInvalidShape(t *testing.T) {
	tests := []struct {
		name         string
		capabilities map[string]any
	}{
		{name: "policy-not-object", capabilities: map[string]any{"providerPolicy": "codex"}},
		{name: "unknown-field", capabilities: map[string]any{"providerPolicy": map[string]any{"providers": []any{"codex"}}}},
		{name: "providers-not-array", capabilities: map[string]any{"providerPolicy": map[string]any{"experimentalProviders": "codex"}}},
		{name: "provider-not-string", capabilities: map[string]any{"providerPolicy": map[string]any{"experimentalProviders": []any{1}}}},
		{name: "unknown-provider", capabilities: map[string]any{"providerPolicy": map[string]any{"experimentalProviders": []any{"droid"}}}},
		{name: "duplicate-after-normalization", capabilities: map[string]any{"providerPolicy": map[string]any{"experimentalProviders": []any{"codex", " CODEX "}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseProviderPolicy(test.capabilities); err == nil {
				t.Fatalf("invalid policy was accepted: %#v", test.capabilities)
			}
		})
	}
}
