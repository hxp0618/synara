package executions

import (
	"testing"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestProviderCursorBindingDigestCoversSecurityMaterial(t *testing.T) {
	model := "gpt-5"
	credentialID := uuid.New()
	credentialVersion := 3
	cliVersion := "0.144.1"
	runtimeVersion := "0.144.1"
	maximumVersion := "1.0.0"
	base := providerCursorBindingMaterial{
		TenantID: uuid.New(), SessionID: uuid.New(), Provider: "codex", Model: &model,
		CredentialID: &credentialID, CredentialVersion: &credentialVersion,
		CapabilityDescriptorHash:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ProviderHostProtocolMajor: 2, ProviderHostProtocolMinor: 1,
		HostBuildVersion: "provider-host-v2", AdapterVersion: "codex-app-server-v2",
		ProviderCLIVersion: &cliVersion, RuntimeKind: "cli", RuntimeName: "codex",
		RuntimeVersion: &runtimeVersion, RuntimeVersionSource: "probe", RuntimeMinimumInclusive: "0.100.0",
		RuntimeMaximumExclusive: &maximumVersion, RuntimeAvailable: true, RuntimeCompatible: true,
		ReleaseRequiresExplicitEnablement: true, ReleaseEnabled: true, ResumeStrategy: "native-cursor",
	}
	baseDigest := base.digest()
	mutations := map[string]func(*providerCursorBindingMaterial){
		"tenant":             func(value *providerCursorBindingMaterial) { value.TenantID = uuid.New() },
		"session":            func(value *providerCursorBindingMaterial) { value.SessionID = uuid.New() },
		"provider":           func(value *providerCursorBindingMaterial) { value.Provider = "claudeAgent" },
		"model":              func(value *providerCursorBindingMaterial) { changed := "gpt-6"; value.Model = &changed },
		"credential id":      func(value *providerCursorBindingMaterial) { changed := uuid.New(); value.CredentialID = &changed },
		"credential version": func(value *providerCursorBindingMaterial) { changed := 4; value.CredentialVersion = &changed },
		"capability": func(value *providerCursorBindingMaterial) {
			value.CapabilityDescriptorHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		},
		"host protocol":   func(value *providerCursorBindingMaterial) { value.ProviderHostProtocolMinor++ },
		"host build":      func(value *providerCursorBindingMaterial) { value.HostBuildVersion += "-next" },
		"adapter":         func(value *providerCursorBindingMaterial) { value.AdapterVersion += "-next" },
		"cli":             func(value *providerCursorBindingMaterial) { changed := "0.145.0"; value.ProviderCLIVersion = &changed },
		"runtime kind":    func(value *providerCursorBindingMaterial) { value.RuntimeKind = "sdk" },
		"runtime name":    func(value *providerCursorBindingMaterial) { value.RuntimeName = "codex-sdk" },
		"runtime version": func(value *providerCursorBindingMaterial) { changed := "0.145.0"; value.RuntimeVersion = &changed },
		"version source":  func(value *providerCursorBindingMaterial) { value.RuntimeVersionSource = "package" },
		"minimum":         func(value *providerCursorBindingMaterial) { value.RuntimeMinimumInclusive = "0.110.0" },
		"maximum": func(value *providerCursorBindingMaterial) {
			changed := "2.0.0"
			value.RuntimeMaximumExclusive = &changed
		},
		"available":       func(value *providerCursorBindingMaterial) { value.RuntimeAvailable = false },
		"compatible":      func(value *providerCursorBindingMaterial) { value.RuntimeCompatible = false },
		"release policy":  func(value *providerCursorBindingMaterial) { value.ReleaseRequiresExplicitEnablement = false },
		"release enabled": func(value *providerCursorBindingMaterial) { value.ReleaseEnabled = false },
		"resume strategy": func(value *providerCursorBindingMaterial) { value.ResumeStrategy = "authoritative-history" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := base
			mutate(&changed)
			if changed.digest() == baseDigest {
				t.Fatalf("%s did not change the Provider Cursor binding digest", name)
			}
		})
	}
}

func TestProviderCursorBindingEncodingIsUnambiguous(t *testing.T) {
	first := providerCursorBindingMaterial{TenantID: uuid.New(), SessionID: uuid.New(), Provider: "ab"}
	second := first
	first.Model = stringPointer("c")
	second.Provider = "a"
	second.Model = stringPointer("bc")
	if first.digest() == second.digest() {
		t.Fatal("length-delimited Provider Cursor binding allowed an ambiguous field boundary")
	}
	withoutModel := first
	withoutModel.Model = nil
	emptyModel := first
	emptyModel.Model = stringPointer("")
	if withoutModel.digest() == emptyModel.digest() {
		t.Fatal("nullable Provider Cursor binding collapsed nil and empty values")
	}
}

func TestProviderCursorNativeResumeRequiresCompleteCompatibleRuntime(t *testing.T) {
	runtimeVersion := "0.144.1"
	manifest := persistence.WorkerProviderManifest{
		CompatibilityStatus: "compatible", ProviderHostMajor: 2, ProviderHostMinor: 1,
		HostBuildVersion: "host-v2", AdapterVersion: "adapter-v2", RuntimeKind: "cli", RuntimeName: "codex",
		RuntimeVersion: &runtimeVersion, RuntimeVersionSource: "probe", RuntimeMinimumInclusive: "0.100.0",
		RuntimeAvailable: true, RuntimeCompatible: true, ReleaseEnabled: true,
		ResumeStrategies:         []string{"native-cursor", "authoritative-history"},
		CapabilityDescriptorHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Capabilities:             map[string]any{"resume-session": "native"},
	}
	if !providerCursorNativeResumeEligible(manifest) {
		t.Fatal("complete compatible native runtime was not eligible for Provider Cursor persistence")
	}
	checks := map[string]func(*persistence.WorkerProviderManifest){
		"status":     func(value *persistence.WorkerProviderManifest) { value.CompatibilityStatus = "incompatible" },
		"capability": func(value *persistence.WorkerProviderManifest) { value.Capabilities["resume-session"] = "emulated" },
		"strategy": func(value *persistence.WorkerProviderManifest) {
			value.ResumeStrategies = []string{"authoritative-history"}
		},
		"protocol":        func(value *persistence.WorkerProviderManifest) { value.ProviderHostMinor = 0 },
		"descriptor":      func(value *persistence.WorkerProviderManifest) { value.CapabilityDescriptorHash = "" },
		"adapter":         func(value *persistence.WorkerProviderManifest) { value.AdapterVersion = "" },
		"runtime version": func(value *persistence.WorkerProviderManifest) { value.RuntimeVersion = nil },
		"available":       func(value *persistence.WorkerProviderManifest) { value.RuntimeAvailable = false },
		"compatible":      func(value *persistence.WorkerProviderManifest) { value.RuntimeCompatible = false },
		"release":         func(value *persistence.WorkerProviderManifest) { value.ReleaseEnabled = false },
	}
	for name, mutate := range checks {
		t.Run(name, func(t *testing.T) {
			changed := manifest
			changed.Capabilities = map[string]any{"resume-session": "native"}
			mutate(&changed)
			if providerCursorNativeResumeEligible(changed) {
				t.Fatalf("incomplete runtime remained native-Cursor eligible: %#v", changed)
			}
		})
	}
}

func stringPointer(value string) *string { return &value }
