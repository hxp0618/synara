package executions

import (
	"testing"
	"time"
)

func TestNormalizeWorkerManifestIsStableAndClassifiesProviders(t *testing.T) {
	now := time.Date(2026, 7, 13, 4, 0, 0, 0, time.UTC)
	capabilities := workerManifestTestCapabilities()
	first, err := normalizeWorkerManifest("worker-test", capabilities, now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := normalizeWorkerManifest("worker-test", map[string]any{
		"workerRuntime": capabilities["workerRuntime"],
		"providerHost":  capabilities["providerHost"],
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if first == nil || second == nil || first.Manifest.ManifestHash != second.Manifest.ManifestHash ||
		first.Status != "compatible" || len(first.Providers) != 2 {
		t.Fatalf("unexpected normalized Worker manifest: first=%#v second=%#v", first, second)
	}
	statuses := map[string]string{}
	for _, provider := range first.Providers {
		statuses[provider.Provider] = provider.CompatibilityStatus
		if provider.CapabilityDescriptorHash == "" || provider.MaximumMessageBytes == 0 ||
			len(provider.CredentialDeliveryModes) == 0 || len(provider.ResumeStrategies) == 0 {
			t.Fatalf("Provider manifest omitted compatibility evidence: %#v", provider)
		}
	}
	if statuses["codex"] != "compatible" || statuses["claudeAgent"] != "unavailable" {
		t.Fatalf("unexpected Provider compatibility statuses: %#v", statuses)
	}
}

func TestNormalizeWorkerManifestRejectsIncompleteV2Summary(t *testing.T) {
	_, err := normalizeWorkerManifest("worker-test", map[string]any{
		"providerHost": map[string]any{
			"protocolVersion": map[string]any{"major": 2, "minor": 0},
			"legacy":          false,
			"providers":       map[string]any{},
		},
	}, time.Now().UTC())
	if err == nil {
		t.Fatal("incomplete Provider Host v2 summary was accepted")
	}
}

func workerManifestTestCapabilities() map[string]any {
	return map[string]any{
		"workerRuntime": map[string]any{
			"workerBuildVersion": "worker-test", "workerBuildGitSha": "abcdef1234567890",
			"workerProtocolMinimum": 1, "workerProtocolMaximum": 1,
			"runtimeEventMinimum": 1, "runtimeEventMaximum": 1,
			"operatingSystem": "linux", "architecture": "amd64",
			"imageDigest": "sha256:worker-test",
		},
		"providerHost": map[string]any{
			"protocolVersion": map[string]any{"major": 2, "minor": 0},
			"legacy":          false,
			"providers": map[string]any{
				"codex":       providerManifestTestDescriptor("codex", "test-codex", "experimental"),
				"claudeAgent": providerManifestTestDescriptor("claudeAgent", "unavailable", "experimental"),
			},
		},
	}
}

func providerManifestTestDescriptor(provider, cliVersion, supportTier string) map[string]any {
	return map[string]any{
		"protocolVersion":  map[string]any{"major": 2, "minor": 0},
		"hostBuildVersion": "host-test", "maximumCommandBytes": 2 << 20,
		"maximumMessageBytes":     1 << 20,
		"runtimeEventVersions":    map[string]any{"minimum": 1, "maximum": 1},
		"credentialDeliveryModes": []string{"anonymous-fd"},
		"resumeStrategies":        []string{"native-cursor", "authoritative-history"},
		"capabilityDescriptor": map[string]any{
			"provider": provider, "supportTier": supportTier, "adapterVersion": provider + "-test",
			"providerCliVersion": cliVersion,
			"capabilities":       map[string]any{"send-turn": "native", "resume-session": "native"},
		},
	}
}
