package executions

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/containmentattestation"
	"github.com/synara-ai/synara/services/control-plane/internal/executiontargets"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

func TestNormalizeWorkerManifestIsStableAndClassifiesProviders(t *testing.T) {
	now := time.Date(2026, 7, 13, 4, 0, 0, 0, time.UTC)
	capabilities := workerManifestTestCapabilities()
	setTestProviderRuntime(capabilities, "claudeAgent", nil, false, false)
	targetCapabilities := workerManifestTestTargetCapabilities()
	first, err := normalizeWorkerManifest(
		"worker-test", capabilities, targetCapabilities, platform.TargetKubernetes, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := normalizeWorkerManifest("worker-test", map[string]any{
		"workerRuntime": capabilities["workerRuntime"],
		"providerHost":  capabilities["providerHost"],
	}, targetCapabilities, platform.TargetKubernetes, now)
	if err != nil {
		t.Fatal(err)
	}
	if first == nil || second == nil || first.Manifest.ManifestHash != second.Manifest.ManifestHash ||
		first.Status != "compatible" || len(first.Providers) != len(stage3ProviderNames) {
		t.Fatalf("unexpected normalized Worker manifest: first=%#v second=%#v", first, second)
	}
	statuses := map[string]string{}
	for _, provider := range first.Providers {
		statuses[provider.Provider] = provider.CompatibilityStatus
		if provider.CapabilityDescriptorHash == "" || provider.MaximumMessageBytes == 0 ||
			len(provider.CredentialDeliveryModes) == 0 || len(provider.ResumeStrategies) == 0 ||
			len(provider.Capabilities) != len(stage3ProviderCapabilityIDs) || provider.RuntimeName == "" {
			t.Fatalf("Provider manifest omitted compatibility evidence: %#v", provider)
		}
	}
	if statuses["codex"] != "compatible" || statuses["claudeagent"] != "unavailable" ||
		statuses["cursor"] != "local-only" {
		t.Fatalf("unexpected Provider compatibility statuses: %#v", statuses)
	}
	if _, storedCanonicalName := statuses["claudeAgent"]; storedCanonicalName {
		t.Fatalf("Claude Provider was not normalized to its lowercase storage code: %#v", statuses)
	}
}

func TestNormalizeWorkerManifestHashIncludesStorageSchemaVersion(t *testing.T) {
	capabilities := workerManifestTestCapabilities()
	normalized, err := normalizeWorkerManifest(
		"worker-test", capabilities, workerManifestTestTargetCapabilities(), platform.TargetKubernetes, time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
	var runtime workerRuntimeCapability
	if err := decodeCapability(capabilities["workerRuntime"], &runtime); err != nil {
		t.Fatal(err)
	}
	var providerHost providerHostCapabilitySummary
	if err := decodeCapability(capabilities["providerHost"], &providerHost); err != nil {
		t.Fatal(err)
	}
	legacyHash, err := canonicalHash(struct {
		Runtime      workerRuntimeCapability
		ProviderHost providerHostCapabilitySummary
		FeatureFlags map[string]any
	}{runtime, providerHost, map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	expectedHash, err := canonicalHash(workerManifestHashPayload{
		StorageSchemaVersion: workerManifestStorageSchemaVersion,
		Runtime:              runtime,
		ProviderHost:         providerHost,
		FeatureFlags:         map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Manifest.ManifestHash != expectedHash {
		t.Fatalf("manifest hash = %q, want storage schema hash %q", normalized.Manifest.ManifestHash, expectedHash)
	}
	if normalized.Manifest.ManifestHash == legacyHash {
		t.Fatal("storage schema version did not fence the legacy canonical-name manifest hash")
	}
}

func TestNormalizeWorkerManifestFreezesProcessContainmentEvidence(t *testing.T) {
	capabilities := workerManifestTestCapabilities()
	registration := workerManifestTestRegistrationContext(platform.TargetKubernetes)
	addWorkerManifestTestSignedContainment(t, capabilities, registration)
	normalized, err := normalizeWorkerManifest(
		"worker-test", capabilities, workerManifestTestTargetCapabilities(), platform.TargetKubernetes, time.Now().UTC(), registration,
	)
	if err != nil {
		t.Fatal(err)
	}
	manifest := normalized.Manifest
	if manifest.ProcessContainmentMode != "cgroup-v2" ||
		manifest.ProcessContainmentSupervisorVersion == nil || *manifest.ProcessContainmentSupervisorVersion != executiontargets.ProtectedCgroupSupervisorVersionV3 ||
		manifest.ProcessContainmentProbeVersion == nil || *manifest.ProcessContainmentProbeVersion != executiontargets.ProtectedCgroupProbeVersionV3 ||
		manifest.ProcessContainmentProbeSHA256 == nil || len(*manifest.ProcessContainmentProbeSHA256) != 64 ||
		manifest.ProcessContainmentSupervisorIdentity == nil || *manifest.ProcessContainmentSupervisorIdentity != "uid:10001" ||
		manifest.ProcessContainmentProviderIdentity == nil || *manifest.ProcessContainmentProviderIdentity != "uid:10002" ||
		manifest.ProcessContainmentTrustMode != executiontargets.ProcessContainmentTrustSignedV1 ||
		manifest.ProcessContainmentAttestationKeyID == nil || *manifest.ProcessContainmentAttestationKeyID != workerManifestTestAttestationKeyID ||
		manifest.ProcessContainmentAttestationKeySHA256 == nil || *manifest.ProcessContainmentAttestationKeySHA256 == "" {
		t.Fatalf("Worker manifest omitted process-containment evidence: %#v", manifest)
	}

	unproven := workerManifestTestCapabilities()
	unproven["resourceSuspendContainment"] = "cgroup-v2"
	withoutProof, err := normalizeWorkerManifest(
		"worker-test", unproven, workerManifestTestTargetCapabilities(), platform.TargetKubernetes, time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if withoutProof.Manifest.ProcessContainmentMode != "none" ||
		withoutProof.Manifest.ManifestHash == manifest.ManifestHash {
		t.Fatalf("transient containment capability became authoritative: %#v", withoutProof.Manifest)
	}
}

func TestNormalizeWorkerManifestRejectsUntrustedProcessContainmentByDefault(t *testing.T) {
	capabilities := workerManifestTestCapabilities()
	addWorkerManifestTestContainmentEvidence(capabilities)
	_, err := normalizeWorkerManifest(
		"worker-test", capabilities, map[string]any{}, platform.TargetKubernetes, time.Now().UTC(),
	)
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "worker_containment_untrusted" {
		t.Fatalf("default trust policy error = %#v", err)
	}
}

func TestNormalizeWorkerManifestRequiresSignedAttestationWhenConfigured(t *testing.T) {
	capabilities := workerManifestTestCapabilities()
	addWorkerManifestTestContainmentEvidence(capabilities)
	_, err := normalizeWorkerManifest(
		"worker-test",
		capabilities,
		workerManifestTestTargetCapabilities(),
		platform.TargetKubernetes,
		time.Now().UTC(),
	)
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != "worker_attestation_required" {
		t.Fatalf("verified attestor policy error = %#v", err)
	}
}

func TestNormalizeWorkerManifestRejectsSignedUnsupportedProtectedCgroupEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "v1 supervisor", mutate: func(value map[string]any) {
			value["supervisorVersion"] = "agentd-protected-cgroup-supervisor-v1"
		}},
		{name: "v2 supervisor without resource limits", mutate: func(value map[string]any) {
			value["supervisorVersion"] = "agentd-protected-cgroup-supervisor-v2"
			value["probeVersion"] = 1
		}},
		{name: "v3 supervisor with old probe", mutate: func(value map[string]any) {
			value["probeVersion"] = 1
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			capabilities := workerManifestTestCapabilities()
			registration := workerManifestTestRegistrationContext(platform.TargetKubernetes)
			addWorkerManifestTestContainmentEvidence(capabilities)
			test.mutate(capabilities["workerRuntime"].(map[string]any)["processContainment"].(map[string]any))
			signWorkerManifestTestContainment(t, capabilities, registration)
			_, err := normalizeWorkerManifest(
				"worker-test", capabilities, workerManifestTestTargetCapabilities(), platform.TargetKubernetes, time.Now().UTC(), registration,
			)
			var apiError *problem.Error
			if !errors.As(err, &apiError) || apiError.Code != "worker_containment_supervisor_unsupported" {
				t.Fatalf("signed unsupported cgroup evidence error = %#v", err)
			}
		})
	}
}

func TestNormalizeWorkerManifestRejectsInvalidProcessContainmentEvidence(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{
			name: "same security identity",
			mutate: func(value map[string]any) {
				value["providerIdentity"] = value["supervisorIdentity"]
			},
		},
		{
			name: "invalid probe digest",
			mutate: func(value map[string]any) {
				value["probeSha256"] = "not-a-digest"
			},
		},
		{
			name: "wrong operating system",
			mutate: func(_ map[string]any) {
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			capabilities := workerManifestTestCapabilities()
			addWorkerManifestTestContainmentEvidence(capabilities)
			workerRuntime := capabilities["workerRuntime"].(map[string]any)
			containment := workerRuntime["processContainment"].(map[string]any)
			testCase.mutate(containment)
			if testCase.name == "wrong operating system" {
				workerRuntime["operatingSystem"] = "darwin"
			}
			if _, err := normalizeWorkerManifest(
				"worker-test", capabilities, workerManifestTestTargetCapabilities(), platform.TargetKubernetes, time.Now().UTC(),
			); err == nil {
				t.Fatal("invalid process-containment evidence was accepted")
			}
		})
	}
}

func TestNormalizeWorkerManifestRejectsWorkerBuildIdentityDrift(t *testing.T) {
	capabilities := workerManifestTestCapabilities()
	workerRuntime := capabilities["workerRuntime"].(map[string]any)
	workerRuntime["workerBuildVersion"] = "different-worker"
	if _, err := normalizeWorkerManifest(
		"worker-test", capabilities, workerManifestTestTargetCapabilities(), platform.TargetKubernetes, time.Now().UTC(),
	); err == nil {
		t.Fatal("Worker manifest accepted a build version that differed from Worker registration")
	}

	capabilities = workerManifestTestCapabilities()
	workerRuntime = capabilities["workerRuntime"].(map[string]any)
	workerRuntime["workerBuildGitSha"] = "not-a-sha"
	if _, err := normalizeWorkerManifest(
		"worker-test", capabilities, workerManifestTestTargetCapabilities(), platform.TargetKubernetes, time.Now().UTC(),
	); err == nil {
		t.Fatal("Worker manifest accepted an invalid build Git SHA")
	}

	capabilities = workerManifestTestCapabilities()
	workerRuntime = capabilities["workerRuntime"].(map[string]any)
	workerRuntime["imageDigest"] = "sha256:not-a-digest"
	if _, err := normalizeWorkerManifest(
		"worker-test", capabilities, workerManifestTestTargetCapabilities(), platform.TargetKubernetes, time.Now().UTC(),
	); err == nil {
		t.Fatal("Worker manifest accepted an invalid image digest")
	}
}

func TestNormalizeWorkerManifestPersistsWorkerImageBuildFeatureFlag(t *testing.T) {
	baselineCapabilities := workerManifestTestCapabilities()
	baseline, err := normalizeWorkerManifest(
		"worker-test", baselineCapabilities, workerManifestTestTargetCapabilities(), platform.TargetKubernetes, time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
	capabilities := workerManifestTestCapabilities()
	capabilities["featureFlags"] = map[string]any{
		"workerImageBuild": map[string]any{
			"schemaVersion": 1,
			"source":        map[string]any{"version": "worker-test", "gitSha": "abcdef1234567890"},
		},
		"existing": true,
	}
	normalized, err := normalizeWorkerManifest(
		"worker-test", capabilities, workerManifestTestTargetCapabilities(), platform.TargetKubernetes, time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
	imageBuild, ok := normalized.Manifest.FeatureFlags["workerImageBuild"].(map[string]any)
	if !ok || imageBuild["schemaVersion"] != 1 || normalized.Manifest.FeatureFlags["existing"] != true {
		t.Fatalf("Worker image build Feature Flag was not persisted: %#v", normalized.Manifest.FeatureFlags)
	}
	if normalized.Manifest.ManifestHash == baseline.Manifest.ManifestHash {
		t.Fatal("Worker image build Feature Flag did not participate in immutable Manifest identity")
	}
}

func TestNormalizeWorkerManifestRejectsIncompleteV21Summary(t *testing.T) {
	_, err := normalizeWorkerManifest("worker-test", map[string]any{
		"providerHost": map[string]any{
			"protocolVersion": map[string]any{"major": 2, "minor": 1},
			"legacy":          false,
			"providers":       map[string]any{},
		},
	}, workerManifestTestTargetCapabilities(), platform.TargetKubernetes, time.Now().UTC())
	if err == nil {
		t.Fatal("incomplete Provider Host v2.1 summary was accepted")
	}
}

func TestNormalizeWorkerManifestRejectsIncompleteCapabilityMatrix(t *testing.T) {
	capabilities := workerManifestTestCapabilities()
	providerCapabilities := testProviderCapabilityMap(capabilities, "codex")
	delete(providerCapabilities, "worker-migration")
	if _, err := normalizeWorkerManifest(
		"worker-test", capabilities, workerManifestTestTargetCapabilities(), platform.TargetKubernetes, time.Now().UTC(),
	); err == nil {
		t.Fatal("Provider descriptor with a missing Capability ID was accepted")
	}

	capabilities = workerManifestTestCapabilities()
	providers := capabilities["providerHost"].(map[string]any)["providers"].(map[string]any)
	providers["droid"] = providers["codex"]
	if _, err := normalizeWorkerManifest(
		"worker-test", capabilities, workerManifestTestTargetCapabilities(), platform.TargetKubernetes, time.Now().UTC(),
	); err == nil {
		t.Fatal("Provider Host summary with an extra Provider was accepted")
	}
}

func TestNormalizeWorkerManifestRequiresTargetPolicyMatchAndDefaultsExperimentalDisabled(t *testing.T) {
	capabilities := workerManifestTestCapabilities()
	setTestProviderReleaseEnabled(capabilities, "codex", false)
	setTestProviderReleaseEnabled(capabilities, "claudeAgent", false)
	normalized, err := normalizeWorkerManifest(
		"worker-test", capabilities, map[string]any{}, platform.TargetKubernetes, time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if normalized == nil || normalized.Status != "incompatible" {
		t.Fatalf("Worker with only disabled/local-only Providers was accepted: %#v", normalized)
	}
	for _, provider := range normalized.Providers {
		if provider.SupportTier == "experimental" && provider.CompatibilityStatus != "disabled" {
			t.Fatalf("Experimental Provider did not default to disabled: %#v", provider)
		}
	}

	capabilities = workerManifestTestCapabilities()
	if _, err := normalizeWorkerManifest(
		"worker-test", capabilities, map[string]any{}, platform.TargetKubernetes, time.Now().UTC(),
	); err == nil {
		t.Fatal("Provider Host release policy mismatch was accepted")
	}
}

func TestNormalizeWorkerManifestClassifiesUnverifiedRuntimeVersion(t *testing.T) {
	capabilities := workerManifestTestCapabilities()
	setTestProviderRuntime(capabilities, "codex", nil, true, false)
	normalized, err := normalizeWorkerManifest(
		"worker-test", capabilities, workerManifestTestTargetCapabilities(), platform.TargetKubernetes, time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range normalized.Providers {
		if provider.Provider == "codex" && (provider.CompatibilityStatus != "incompatible" || provider.RuntimeVersion != nil) {
			t.Fatalf("unverified Codex runtime version was not fenced: %#v", provider)
		}
	}
}

func TestNormalizeWorkerManifestAcceptsSemVerPrereleaseForLocalRuntime(t *testing.T) {
	capabilities := workerManifestTestCapabilities()
	setTestProviderRuntime(capabilities, "cursor", stringReference("0.2.0-dev"), true, true)
	normalized, err := normalizeWorkerManifest(
		"worker-test", capabilities, workerManifestTestTargetCapabilities(), platform.TargetLocal, time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, provider := range normalized.Providers {
		if provider.Provider == "cursor" &&
			(!provider.RuntimeCompatible || provider.IncompatibilityCode == nil ||
				*provider.IncompatibilityCode != "capability_unsupported") {
			t.Fatalf("SemVer prerelease runtime was rejected: %#v", provider)
		}
	}
}

func TestSemanticVersionPrereleaseOrdering(t *testing.T) {
	prerelease, ok := parseSemanticVersion("v1.2.3-rc.1+build.7")
	if !ok {
		t.Fatal("valid SemVer prerelease was rejected")
	}
	release, ok := parseSemanticVersion("1.2.3")
	if !ok || compareSemanticVersion(prerelease, release) >= 0 {
		t.Fatalf("SemVer prerelease ordering is invalid: prerelease=%#v release=%#v", prerelease, release)
	}
	if _, ok := parseSemanticVersion("1.2.3-01"); ok {
		t.Fatal("numeric prerelease with a leading zero was accepted")
	}
}

func TestNormalizeWorkerManifestRejectsRuntimeEventV1MasqueradingAsV2(t *testing.T) {
	capabilities := workerManifestTestCapabilities()
	workerRuntime := capabilities["workerRuntime"].(map[string]any)
	workerRuntime["runtimeEventMinimum"] = 1
	workerRuntime["runtimeEventMaximum"] = 1

	normalized, err := normalizeWorkerManifest(
		"worker-test", capabilities, workerManifestTestTargetCapabilities(), platform.TargetKubernetes, time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if normalized == nil || normalized.Status != "incompatible" || normalized.Reason == nil {
		t.Fatalf("v1-only Worker runtime was accepted as v2: %#v", normalized)
	}

	capabilities = workerManifestTestCapabilities()
	providerHost := capabilities["providerHost"].(map[string]any)
	providers := providerHost["providers"].(map[string]any)
	codex := providers["codex"].(map[string]any)
	codex["runtimeEventVersions"] = map[string]any{"minimum": 1, "maximum": 1}
	normalized, err = normalizeWorkerManifest(
		"worker-test", capabilities, workerManifestTestTargetCapabilities(), platform.TargetKubernetes, time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if normalized == nil {
		t.Fatal("Provider manifest was omitted")
	}
	for _, provider := range normalized.Providers {
		if provider.Provider == "codex" && provider.CompatibilityStatus != "incompatible" {
			t.Fatalf("v1-only Provider descriptor was accepted as v2: %#v", provider)
		}
	}
}

func TestGoProviderCatalogKeysMatchSharedCatalog(t *testing.T) {
	catalog := loadProviderCapabilityCatalogForTests()
	if len(catalog.CapabilityIDs) != len(stage3ProviderCapabilityIDs) || len(catalog.Providers) != len(stage3ProviderNames) {
		t.Fatalf("shared Provider catalog dimensions drifted: %#v", catalog)
	}
	for index, capabilityID := range stage3ProviderCapabilityIDs {
		if catalog.CapabilityIDs[index] != capabilityID {
			t.Fatalf("Capability ID %d = %q, want %q", index, catalog.CapabilityIDs[index], capabilityID)
		}
	}
	for index, provider := range stage3ProviderNames {
		if catalog.Providers[index].Provider != provider {
			t.Fatalf("Provider %d = %q, want %q", index, catalog.Providers[index].Provider, provider)
		}
		if len(catalog.Providers[index].Capabilities) != len(stage3ProviderCapabilityIDs) {
			t.Fatalf("Provider %q capability count = %d", provider, len(catalog.Providers[index].Capabilities))
		}
	}
}

func workerManifestTestCapabilities() map[string]any {
	return workerManifestTestCapabilitiesForVersion("worker-test")
}

func workerManifestTestCapabilitiesForVersion(workerVersion string) map[string]any {
	catalog := loadProviderCapabilityCatalogForTests()
	providers := make(map[string]any, len(catalog.Providers))
	for _, entry := range catalog.Providers {
		available := entry.SupportTier != "local-only"
		compatible := available
		var version *string
		switch entry.Provider {
		case "codex", "claudeAgent":
			version = stringReference(entry.RuntimePolicy.CompatibleRange.MinimumInclusive)
		}
		runtimeDescriptor := map[string]any{
			"kind": entry.RuntimePolicy.Kind, "name": entry.RuntimePolicy.Name,
			"available": available, "versionSource": entry.RuntimePolicy.VersionSource,
			"compatibleRange": map[string]any{
				"minimumInclusive": entry.RuntimePolicy.CompatibleRange.MinimumInclusive,
			},
			"compatible": compatible,
		}
		if version != nil {
			runtimeDescriptor["version"] = *version
		}
		if entry.RuntimePolicy.CompatibleRange.MaximumExclusive != nil {
			runtimeDescriptor["compatibleRange"].(map[string]any)["maximumExclusive"] =
				*entry.RuntimePolicy.CompatibleRange.MaximumExclusive
		}
		capabilityDescriptor := map[string]any{
			"provider": entry.Provider, "supportTier": entry.SupportTier,
			"adapterVersion": entry.AdapterVersion, "runtime": runtimeDescriptor,
			"releasePolicy": map[string]any{
				"requiresExplicitEnablement": entry.SupportTier == "experimental", "enabled": true,
			},
			"capabilities": stringMapToAny(entry.Capabilities),
		}
		if entry.Provider == "codex" && version != nil {
			capabilityDescriptor["providerCliVersion"] = *version
		}
		providers[entry.Provider] = map[string]any{
			"protocolVersion":  map[string]any{"major": providerHostProtocolMajor, "minor": providerHostProtocolMinimumMinor},
			"hostBuildVersion": "host-test", "maximumCommandBytes": 2 << 20,
			"maximumMessageBytes":     1 << 20,
			"runtimeEventVersions":    map[string]any{"minimum": RuntimeEventVersionV2, "maximum": RuntimeEventVersionV2},
			"credentialDeliveryModes": []string{"anonymous-fd"},
			"resumeStrategies":        []string{"native-cursor", "authoritative-history"},
			"capabilityDescriptor":    capabilityDescriptor,
		}
	}
	return map[string]any{
		"workerRuntime": map[string]any{
			"workerBuildVersion": workerVersion, "workerBuildGitSha": "abcdef1234567890",
			"workerProtocolMinimum": WorkerProtocolVersion, "workerProtocolMaximum": WorkerProtocolVersion,
			"runtimeEventMinimum": RuntimeEventVersionV2, "runtimeEventMaximum": RuntimeEventVersionV2,
			"operatingSystem": "linux", "architecture": "amd64",
			"imageDigest": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
		"providerHost": map[string]any{
			"protocolVersion": map[string]any{"major": providerHostProtocolMajor, "minor": providerHostProtocolMinimumMinor},
			"legacy":          false, "providers": providers,
		},
	}
}

func workerManifestTestTargetCapabilities() map[string]any {
	return map[string]any{
		"providerPolicy": map[string]any{
			"experimentalProviders": []any{"codex", "claudeAgent"},
		},
		"processContainmentPolicy": map[string]any{
			"trustMode":        executiontargets.ProcessContainmentTrustSignedV1,
			"keyId":            workerManifestTestAttestationKeyID,
			"ed25519PublicKey": base64.StdEncoding.EncodeToString(workerManifestTestAttestationPublicKey),
		},
	}
}

const workerManifestTestAttestationKeyID = "worker-manifest-test-key"

var (
	workerManifestTestAttestationSeed       = sha256.Sum256([]byte("synara-worker-manifest-test-attestation-key"))
	workerManifestTestAttestationPrivateKey = ed25519.NewKeyFromSeed(workerManifestTestAttestationSeed[:])
	workerManifestTestAttestationPublicKey  = workerManifestTestAttestationPrivateKey.Public().(ed25519.PublicKey)
)

func workerManifestTestRegistrationContext(targetKind platform.ExecutionTargetKind) workerManifestRegistrationContext {
	return workerManifestRegistrationContext{
		ExecutionTargetID: uuid.New(), TargetKind: targetKind, InstanceUID: uuid.NewString(),
		ClusterID: "test-cluster", Namespace: "default", PodName: "worker-test-pod",
	}
}

func addWorkerManifestTestContainmentEvidence(capabilities map[string]any) {
	capabilities["workerRuntime"].(map[string]any)["processContainment"] = map[string]any{
		"mode": "cgroup-v2", "supervisorVersion": executiontargets.ProtectedCgroupSupervisorVersionV3,
		"probeVersion":       executiontargets.ProtectedCgroupProbeVersionV3,
		"probeSha256":        "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
		"supervisorIdentity": "uid:10001", "providerIdentity": "uid:10002",
	}
}

func addWorkerManifestTestSignedContainment(
	t *testing.T,
	capabilities map[string]any,
	registration workerManifestRegistrationContext,
) {
	t.Helper()
	addWorkerManifestTestContainmentEvidence(capabilities)
	signWorkerManifestTestContainment(t, capabilities, registration)
}

func signWorkerManifestTestContainment(
	t *testing.T,
	capabilities map[string]any,
	registration workerManifestRegistrationContext,
) {
	t.Helper()
	if err := signWorkerManifestTestContainmentValue(capabilities, registration); err != nil {
		t.Fatal(err)
	}
}

func signWorkerManifestTestContainmentValue(
	capabilities map[string]any,
	registration workerManifestRegistrationContext,
) error {
	var runtimeCapability workerRuntimeCapability
	if err := decodeCapability(capabilities["workerRuntime"], &runtimeCapability); err != nil {
		return err
	}
	containment := runtimeCapability.ProcessContainment
	statement := containmentattestation.Statement{
		ExecutionTargetID: registration.ExecutionTargetID.String(), TargetKind: string(registration.TargetKind),
		InstanceUID: registration.InstanceUID, ClusterID: registration.ClusterID,
		Namespace: registration.Namespace, PodName: registration.PodName,
		WorkerBuildVersion: runtimeCapability.WorkerBuildVersion,
		WorkerBuildGitSHA:  optionalStringValue(runtimeCapability.WorkerBuildGitSHA),
		ImageDigest:        optionalStringValue(runtimeCapability.ImageDigest),
		OperatingSystem:    runtimeCapability.OperatingSystem, Architecture: runtimeCapability.Architecture,
		Mode: containment.Mode, SupervisorVersion: containment.SupervisorVersion,
		ProbeVersion: containment.ProbeVersion, ProbeSHA256: containment.ProbeSHA256,
		SupervisorIdentity: containment.SupervisorIdentity, ProviderIdentity: containment.ProviderIdentity,
	}
	envelope, err := containmentattestation.Sign(
		workerManifestTestAttestationPrivateKey, workerManifestTestAttestationKeyID, statement,
	)
	if err != nil {
		return err
	}
	capabilities["workerRuntime"].(map[string]any)["processContainment"].(map[string]any)["attestation"] = envelope
	return nil
}

func testProviderCapabilityMap(capabilities map[string]any, provider string) map[string]any {
	providerHost := capabilities["providerHost"].(map[string]any)
	providers := providerHost["providers"].(map[string]any)
	descriptor := providers[provider].(map[string]any)["capabilityDescriptor"].(map[string]any)
	return descriptor["capabilities"].(map[string]any)
}

func setTestProviderRuntime(
	capabilities map[string]any,
	provider string,
	version *string,
	available bool,
	compatible bool,
) {
	providerHost := capabilities["providerHost"].(map[string]any)
	providers := providerHost["providers"].(map[string]any)
	descriptor := providers[provider].(map[string]any)["capabilityDescriptor"].(map[string]any)
	runtimeDescriptor := descriptor["runtime"].(map[string]any)
	delete(runtimeDescriptor, "version")
	delete(descriptor, "providerCliVersion")
	if version != nil {
		runtimeDescriptor["version"] = *version
		if provider == "codex" {
			descriptor["providerCliVersion"] = *version
		}
	}
	runtimeDescriptor["available"] = available
	runtimeDescriptor["compatible"] = compatible
}

func setTestProviderReleaseEnabled(capabilities map[string]any, provider string, enabled bool) {
	providerHost := capabilities["providerHost"].(map[string]any)
	providers := providerHost["providers"].(map[string]any)
	descriptor := providers[provider].(map[string]any)["capabilityDescriptor"].(map[string]any)
	descriptor["releasePolicy"].(map[string]any)["enabled"] = enabled
}

type providerCapabilityCatalogTest struct {
	CapabilityIDs []string                             `json:"capabilityIds"`
	Providers     []providerCapabilityCatalogEntryTest `json:"providers"`
}

type providerCapabilityCatalogEntryTest struct {
	Provider       string                           `json:"provider"`
	SupportTier    string                           `json:"supportTier"`
	AdapterVersion string                           `json:"adapterVersion"`
	RuntimePolicy  providerRuntimePolicyCatalogTest `json:"runtimePolicy"`
	Capabilities   map[string]string                `json:"capabilities"`
}

type providerRuntimePolicyCatalogTest struct {
	Kind            string                          `json:"kind"`
	Name            string                          `json:"name"`
	VersionSource   string                          `json:"versionSource"`
	CompatibleRange providerRuntimeVersionRangeTest `json:"compatibleRange"`
}

type providerRuntimeVersionRangeTest struct {
	MinimumInclusive string  `json:"minimumInclusive"`
	MaximumExclusive *string `json:"maximumExclusive"`
}

var (
	providerCatalogTestOnce sync.Once
	providerCatalogTestData providerCapabilityCatalogTest
	providerCatalogTestErr  error
)

func loadProviderCapabilityCatalogForTests() providerCapabilityCatalogTest {
	providerCatalogTestOnce.Do(func() {
		_, sourceFile, _, ok := runtime.Caller(0)
		if !ok {
			providerCatalogTestErr = os.ErrNotExist
			return
		}
		path := filepath.Clean(filepath.Join(
			filepath.Dir(sourceFile), "../../../../packages/contracts/src/providerCapabilityCatalog.json",
		))
		encoded, err := os.ReadFile(path)
		if err != nil {
			providerCatalogTestErr = err
			return
		}
		providerCatalogTestErr = json.Unmarshal(encoded, &providerCatalogTestData)
	})
	if providerCatalogTestErr != nil {
		panic(providerCatalogTestErr)
	}
	return providerCatalogTestData
}
