package executiontargets

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestExecutionTargetMutationsRejectInactiveTenantBeforePolicyOrStorage(t *testing.T) {
	ctx := context.Background()
	activeTenantID := uuid.New()
	requestedTenantID := uuid.New()
	principal := identity.Principal{UserID: uuid.New(), ActiveTenantID: &activeTenantID}
	config, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(nil, config, nil)

	_, err = service.Create(ctx, principal, requestedTenantID, CreateInput{})
	assertExecutionTargetProblem(t, err, 404, "tenant_not_found")
	_, err = service.UpdateProviderPolicy(ctx, principal, requestedTenantID, uuid.New(), nil)
	assertExecutionTargetProblem(t, err, 404, "tenant_not_found")
	_, err = service.UpdateProcessContainmentPolicy(ctx, principal, requestedTenantID, uuid.New(), nil)
	assertExecutionTargetProblem(t, err, 404, "tenant_not_found")
	_, err = service.UpdateRuntimeIsolationPolicy(
		ctx, principal, requestedTenantID, uuid.New(), nil,
		"execution-target-inactive-runtime-isolation", "127.0.0.1",
	)
	assertExecutionTargetProblem(t, err, 404, "tenant_not_found")
	_, _, err = service.DisableManagedKubernetesTarget(
		ctx, principal, requestedTenantID, uuid.New(),
		"execution-target-inactive-disable", "127.0.0.1",
	)
	assertExecutionTargetProblem(t, err, 404, "tenant_not_found")
}

func TestTargetAPIModelNeverExposesEncryptedConfiguration(t *testing.T) {
	ctx := context.Background()
	config, _ := platform.Defaults(platform.ProfilePersonal)
	store, err := database.OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "execution-target-test")
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := secret.NewCursorCipherWithKeyring(secret.CipherKey{
		ID: "runtime-v2", Key: bytes.Repeat([]byte{0x23}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store.DB(), config, cipher)
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	created, err := service.Create(ctx, principal, domain.TenantID, CreateInput{
		OrganizationID: &domain.OrganizationID, Kind: "ssh", Name: "build-host",
		Configuration: map[string]any{"privateKey": "secret-value", "host": "example.internal"},
		Capabilities:  map[string]any{"workspaceModes": []string{"local", "worktree"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != "offline" {
		t.Fatalf("new SSH target status = %q, want offline", created.Status)
	}
	encoded, err := json.Marshal(created)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("configuration")) || bytes.Contains(encoded, []byte("secret-value")) {
		t.Fatalf("safe target response leaked configuration: %s", encoded)
	}
	defaultPolicy, err := ParseProviderPolicy(created.Capabilities)
	if err != nil {
		t.Fatal(err)
	}
	if len(defaultPolicy.ExperimentalProviders) != 0 {
		t.Fatalf("new target enabled Experimental Providers by default: %#v", defaultPolicy)
	}
	containmentPolicy, err := ParseProcessContainmentPolicy(created.Capabilities)
	if err != nil {
		t.Fatal(err)
	}
	if containmentPolicy.TrustMode != ProcessContainmentTrustDisabled {
		t.Fatalf("new target enabled process containment trust by default: %#v", containmentPolicy)
	}
	if _, found := created.Capabilities["processContainmentPolicy"]; found {
		t.Fatalf("default process containment policy should not be materialized: %#v", created.Capabilities["processContainmentPolicy"])
	}
	var persisted persistence.ExecutionTarget
	if err := store.DB().Where("id = ?", created.ID).Take(&persisted).Error; err != nil {
		t.Fatal(err)
	}
	if len(persisted.ConfigurationEncrypted) == 0 || bytes.Contains(persisted.ConfigurationEncrypted, []byte("secret-value")) {
		t.Fatal("execution target configuration was not encrypted")
	}
	if persisted.ConfigurationKeyID == nil || *persisted.ConfigurationKeyID != "runtime-v2" {
		t.Fatalf("execution target configuration omitted its runtime key ID: %#v", persisted.ConfigurationKeyID)
	}
	decoded, metadata, err := cipher.DecryptWithMetadata(persisted.ConfigurationEncrypted)
	if err != nil || !metadata.Primary || !metadata.Keyed || !strings.Contains(decoded, "secret-value") {
		t.Fatalf("execution target keyed envelope = %q, %#v, %v", decoded, metadata, err)
	}
	if _, err := service.Create(ctx, principal, domain.TenantID, CreateInput{
		OrganizationID: &domain.OrganizationID, Kind: "local", Name: "unsafe-capabilities",
		Capabilities: map[string]any{"accessToken": "leak"},
	}); err == nil {
		t.Fatal("secret-like public capabilities were accepted")
	}
	if _, err := service.Create(ctx, principal, domain.TenantID, CreateInput{
		Kind: "local", Name: "tenant-wide-personal",
	}); err == nil {
		t.Fatal("personal execution target without organization ownership was accepted")
	}
}

func TestCreateKubernetesTargetPersistsAndProjectsAutoRuntimeIsolation(t *testing.T) {
	ctx := context.Background()
	config, _ := platform.Defaults(platform.ProfilePersonal)
	store, err := database.OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "execution-target-runtime-isolation-test")
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := secret.NewCursorCipher(bytes.Repeat([]byte{0x24}, 32))
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store.DB(), config, cipher)
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	created, err := service.Create(ctx, principal, domain.TenantID, CreateInput{
		OrganizationID: &domain.OrganizationID, Kind: "kubernetes", Name: "gvisor-auto",
		Configuration: map[string]any{"image": "synara-agentd:test"}, Capabilities: map[string]any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	policy := created.RuntimeIsolationPolicy
	if policy == nil || policy.Mode != runtimeIsolationModeAuto || policy.RequestedRuntime != "auto" ||
		!slices.Equal(policy.Preferred, []string{runtimeIsolationGVisor, runtimeIsolationRunc}) ||
		policy.MinimumProfile != platform.IsolationKubernetesRestricted ||
		policy.FallbackPolicy != runtimeIsolationFallbackAllowLower ||
		policy.RuntimeClassName != kubernetesDefaultGVisorRuntimeClassName {
		t.Fatalf("projected runtime isolation policy = %#v", policy)
	}
	var persisted persistence.ExecutionTarget
	if err := store.DB().First(&persisted, "id = ?", created.ID).Error; err != nil {
		t.Fatal(err)
	}
	decoded, err := cipher.Decrypt(persisted.ConfigurationEncrypted)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(decoded, `"runtimeIsolation"`) || strings.Contains(decoded, "secret-value") {
		t.Fatalf("persisted runtime policy = %s", decoded)
	}
	now := time.Now().UTC()
	expiresAt := now.Add(30 * time.Second)
	decision := runtimeIsolationDecision{
		RequestedRuntime: "auto", RequestedProfile: platform.IsolationKubernetesRestricted,
		EffectiveRuntime: runtimeIsolationGVisor, EffectiveProfile: platform.IsolationGVisorSandboxed,
		PolicySource: "target-auto", Decision: "selected",
	}
	if err := persistRuntimeIsolationObservation(ctx, store.DB(), created.ID, []runtimeIsolationCapability{
		{Runtime: runtimeIsolationGVisor, Profile: platform.IsolationGVisorSandboxed, AttestationExpiresAt: &expiresAt},
		{Runtime: runtimeIsolationRunc, Profile: platform.IsolationKubernetesRestricted},
	}, &decision, nil, now); err != nil {
		t.Fatal(err)
	}
	listed, err := service.List(ctx, principal, domain.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	projected, found := findTargetByID(listed, created.ID)
	if !found {
		t.Fatalf("created target missing from list: %#v", listed)
	}
	if projected.RuntimeIsolationStatus == nil || projected.RuntimeIsolationStatus.State != "available" ||
		!slices.Equal(projected.RuntimeIsolationStatus.DetectedRuntimes, []string{runtimeIsolationGVisor, runtimeIsolationRunc}) ||
		!projected.RuntimeIsolationStatus.ExpiresAt.Equal(now.Add(30*time.Second)) {
		t.Fatalf("projected runtime isolation availability = %#v", projected.RuntimeIsolationStatus)
	}
	_, err = service.Create(ctx, principal, domain.TenantID, CreateInput{
		OrganizationID: &domain.OrganizationID, Kind: "kubernetes", Name: "invalid-gvisor-policy",
		Configuration: map[string]any{
			"image": "synara-agentd:test",
			"runtimeIsolation": map[string]any{
				"mode": "explicit", "runtime": "gvisor", "unknownField": true,
			},
		},
	})
	assertProblemCode(t, err, 400, "runtime_isolation_configuration_invalid")
}

func findTargetByID(targets []Target, id uuid.UUID) (Target, bool) {
	for _, target := range targets {
		if target.ID == id {
			return target, true
		}
	}
	return Target{}, false
}

func TestPlatformSharedWeakTargetsAreExcludedFromTenantProductSurface(t *testing.T) {
	ctx := context.Background()
	config, _ := platform.Defaults(platform.ProfilePersonal)
	store, err := database.OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "execution-target-isolation-boundary-test")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store.DB(), config, nil)
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	weakTargets := make([]persistence.ExecutionTarget, 0, 3)
	for _, kind := range []string{"local", "ssh", "docker"} {
		weakTargets = append(weakTargets, persistence.ExecutionTarget{
			ID: uuid.New(), Kind: kind, Name: "platform-" + kind, Status: "active",
			ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{},
		})
	}
	platformKubernetes := persistence.ExecutionTarget{
		ID: uuid.New(), Kind: "kubernetes", Name: "platform-kubernetes", Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{},
	}
	models := append(weakTargets, platformKubernetes)
	if err := store.DB().Create(&models).Error; err != nil {
		t.Fatal(err)
	}

	listed, err := service.List(ctx, principal, domain.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	foundKubernetes := false
	for _, target := range listed {
		if target.TenantID == nil && target.Kind != "kubernetes" {
			t.Fatalf("weak platform-shared Target leaked into tenant product surface: %#v", target)
		}
		if target.ID == platformKubernetes.ID {
			foundKubernetes = true
			if target.IsolationProfile != platform.IsolationKubernetesRestricted || !target.PlatformSharedEligible ||
				target.ProductBoundary != "multi-tenant-restricted" {
				t.Fatalf("Kubernetes isolation declaration = %#v", target)
			}
		}
	}
	if !foundKubernetes {
		t.Fatal("restricted Kubernetes Target was excluded with weak Target kinds")
	}

	weakID := weakTargets[0].ID
	_, err = service.Get(ctx, principal, domain.TenantID, weakID)
	assertExecutionTargetProblem(t, err, 404, "execution_target_not_found")
	_, err = service.ResolveForSession(ctx, domain.TenantID, domain.OrganizationID, &weakID)
	assertExecutionTargetProblem(t, err, 409, "execution_target_required")
	_, _, err = service.ResolveWorkerTarget(ctx, weakID, "local")
	assertExecutionTargetProblem(t, err, 404, "execution_target_not_found")
}

func TestCreateNormalizesAndPersistsProviderPolicy(t *testing.T) {
	ctx := context.Background()
	config, _ := platform.Defaults(platform.ProfilePersonal)
	store, err := database.OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "execution-target-provider-policy-test")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store.DB(), config, nil)
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	created, err := service.Create(ctx, principal, domain.TenantID, CreateInput{
		OrganizationID: &domain.OrganizationID, Kind: "local", Name: "policy-target",
		Capabilities: map[string]any{
			"workspaceModes": []any{"local"},
			"providerPolicy": map[string]any{
				"experimentalProviders": []any{" OpenCode ", "CLAUDEAGENT", "codex"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertProviderPolicy := func(label string, capabilities map[string]any) {
		t.Helper()
		policy, parseErr := ParseProviderPolicy(capabilities)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		want := []string{"codex", "claudeAgent", "opencode"}
		if len(policy.ExperimentalProviders) != len(want) {
			t.Fatalf("%s policy = %#v, want %#v", label, policy.ExperimentalProviders, want)
		}
		for index := range want {
			if policy.ExperimentalProviders[index] != want[index] {
				t.Fatalf("%s policy = %#v, want %#v", label, policy.ExperimentalProviders, want)
			}
		}
	}
	assertProviderPolicy("created", created.Capabilities)
	var persisted persistence.ExecutionTarget
	if err := store.DB().Where("id = ?", created.ID).Take(&persisted).Error; err != nil {
		t.Fatal(err)
	}
	assertProviderPolicy("persisted", persisted.Capabilities)

	for index, capabilities := range []map[string]any{
		{"providerPolicy": map[string]any{"unknown": true}},
		{"providerPolicy": map[string]any{"experimentalProviders": []any{"codex", " CODEX "}}},
	} {
		_, createErr := service.Create(ctx, principal, domain.TenantID, CreateInput{
			OrganizationID: &domain.OrganizationID, Kind: "local",
			Name: "invalid-policy-" + string(rune('a'+index)), Capabilities: capabilities,
		})
		var apiError *problem.Error
		if !errors.As(createErr, &apiError) || apiError.Code != "invalid_execution_target_provider_policy" {
			t.Fatalf("invalid Provider Policy error = %v", createErr)
		}
	}
}

func TestCreateNormalizesAndPersistsProcessContainmentPolicy(t *testing.T) {
	ctx := context.Background()
	config, _ := platform.Defaults(platform.ProfilePersonal)
	store, err := database.OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "execution-target-process-containment-create-test")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store.DB(), config, nil)
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	created, err := service.Create(ctx, principal, domain.TenantID, CreateInput{
		OrganizationID: &domain.OrganizationID, Kind: "local", Name: "containment-policy-target",
		Capabilities: map[string]any{
			"workspaceModes": []any{"local"},
			"processContainmentPolicy": map[string]any{
				"trustMode":        ProcessContainmentTrustSignedV1,
				"keyId":            "test-key",
				"ed25519PublicKey": processContainmentTestPublicKeyBase64Raw(),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertProcessContainmentPolicy(t, created.Capabilities, ProcessContainmentTrustSignedV1, "test-key", processContainmentTestPublicKeyBase64())
	assertNormalizedProcessContainmentCapability(t, created.Capabilities, ProcessContainmentTrustSignedV1, "test-key", processContainmentTestPublicKeyBase64())
	var persisted persistence.ExecutionTarget
	if err := store.DB().Where("id = ?", created.ID).Take(&persisted).Error; err != nil {
		t.Fatal(err)
	}
	assertProcessContainmentPolicy(t, persisted.Capabilities, ProcessContainmentTrustSignedV1, "test-key", processContainmentTestPublicKeyBase64())
	assertNormalizedProcessContainmentCapability(t, persisted.Capabilities, ProcessContainmentTrustSignedV1, "test-key", processContainmentTestPublicKeyBase64())

	for index, capabilities := range []map[string]any{
		{"processContainmentPolicy": map[string]any{"unknown": true}},
		{"processContainmentPolicy": map[string]any{"trustMode": ProcessContainmentTrustDisabled, "keyId": "extra"}},
		{"processContainmentPolicy": map[string]any{"trustMode": ProcessContainmentTrustSignedV1, "keyId": "test key", "ed25519PublicKey": processContainmentTestPublicKeyBase64()}},
		{"processContainmentPolicy": map[string]any{"trustMode": ProcessContainmentTrustSignedV1, "keyId": "test-key", "ed25519PublicKey": "!!!"}},
	} {
		_, createErr := service.Create(ctx, principal, domain.TenantID, CreateInput{
			OrganizationID: &domain.OrganizationID, Kind: "local",
			Name: "invalid-containment-policy-" + string(rune('a'+index)), Capabilities: capabilities,
		})
		var apiError *problem.Error
		if !errors.As(createErr, &apiError) || apiError.Code != "invalid_execution_target_process_containment_policy" {
			t.Fatalf("invalid process containment policy error = %v", createErr)
		}
	}
}

func TestUpdateProviderPolicyNormalizesInvalidatesAndEnforcesAccess(t *testing.T) {
	ctx := context.Background()
	config, _ := platform.Defaults(platform.ProfilePersonal)
	store, err := database.OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "execution-target-provider-policy-update-test")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store.DB(), config, nil)
	owner := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	target, err := service.Create(ctx, owner, domain.TenantID, CreateInput{
		OrganizationID: &domain.OrganizationID, Kind: "local", Name: "mutable-policy-target",
		Capabilities: map[string]any{
			"workspaceModes": []any{"local", "worktree"},
			"processContainmentPolicy": map[string]any{
				"trustMode":        ProcessContainmentTrustSignedV1,
				"keyId":            "provider-policy-key",
				"ed25519PublicKey": processContainmentTestPublicKeyBase64Raw(),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	manifestID, workerID := uuid.New(), uuid.New()
	checkedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	if err := store.DB().Create(&persistence.WorkerManifest{
		ID: manifestID, ManifestHash: strings.Repeat("a", 64), WorkerBuildVersion: "policy-worker",
		WorkerProtocolMinimum: 2, WorkerProtocolMaximum: 2,
		RuntimeEventMinimum: 2, RuntimeEventMaximum: 2,
		OperatingSystem: "linux", Architecture: "amd64", FeatureFlags: map[string]any{}, CreatedAt: checkedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.WorkerInstance{
		ID: workerID, Incarnation: 1, InstanceUID: uuid.NewString(), ExecutionTargetID: target.ID,
		TargetKind: "local", ClusterID: "policy-test", Namespace: "default", PodName: "policy-worker",
		Version: "policy-worker", ProtocolVersion: 2, Capabilities: map[string]any{}, CurrentManifestID: &manifestID,
		CompatibilityStatus: "compatible", CompatibilityCheckedAt: &checkedAt,
		LeaseSupported: true, FencingSupported: true, AuthTokenHash: secret.HashToken("policy-worker-token"),
		Status: "online", RegisteredAt: checkedAt, LastHeartbeatAt: checkedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}

	updated, err := service.UpdateProviderPolicy(ctx, owner, domain.TenantID, target.ID, map[string]any{
		"experimentalProviders": []any{" CLAUDEAGENT ", "codex"},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertExperimentalProviders(t, updated.Capabilities, "codex", "claudeAgent")
	workspaceModes, err := json.Marshal(updated.Capabilities["workspaceModes"])
	if err != nil {
		t.Fatal(err)
	}
	if string(workspaceModes) != `["local","worktree"]` {
		t.Fatalf("workspace modes were not preserved: %s", workspaceModes)
	}
	var persistedTarget persistence.ExecutionTarget
	if err := store.DB().Where("id = ?", target.ID).Take(&persistedTarget).Error; err != nil {
		t.Fatal(err)
	}
	assertExperimentalProviders(t, persistedTarget.Capabilities, "codex", "claudeAgent")
	assertProcessContainmentPolicy(t, persistedTarget.Capabilities, ProcessContainmentTrustSignedV1, "provider-policy-key", processContainmentTestPublicKeyBase64())
	assertNormalizedProcessContainmentCapability(t, persistedTarget.Capabilities, ProcessContainmentTrustSignedV1, "provider-policy-key", processContainmentTestPublicKeyBase64())
	var invalidated persistence.WorkerInstance
	if err := store.DB().Where("id = ?", workerID).Take(&invalidated).Error; err != nil {
		t.Fatal(err)
	}
	if invalidated.CompatibilityStatus != "incompatible" || invalidated.CompatibilityReason == nil ||
		!strings.Contains(*invalidated.CompatibilityReason, "re-register") || invalidated.CompatibilityCheckedAt == nil ||
		!invalidated.CompatibilityCheckedAt.After(checkedAt) || invalidated.CurrentManifestID == nil ||
		*invalidated.CurrentManifestID != manifestID {
		t.Fatalf("Worker manifest was not safely invalidated: %#v", invalidated)
	}

	noOpCheckedAt := checkedAt.Add(10 * time.Minute)
	if err := store.DB().Model(&persistence.WorkerInstance{}).Where("id = ?", workerID).Updates(map[string]any{
		"compatibility_status": "compatible", "compatibility_reason": nil,
		"compatibility_checked_at": noOpCheckedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	targetUpdatedAt := persistedTarget.UpdatedAt
	if _, err := service.UpdateProviderPolicy(ctx, owner, domain.TenantID, target.ID, map[string]any{
		"experimentalProviders": []any{"CODEX", "claudeagent"},
	}); err != nil {
		t.Fatal(err)
	}
	var noOpTarget persistence.ExecutionTarget
	if err := store.DB().Where("id = ?", target.ID).Take(&noOpTarget).Error; err != nil {
		t.Fatal(err)
	}
	if !noOpTarget.UpdatedAt.Equal(targetUpdatedAt) {
		t.Fatalf("semantic no-op changed target timestamp: %s -> %s", targetUpdatedAt, noOpTarget.UpdatedAt)
	}
	var noOpWorker persistence.WorkerInstance
	if err := store.DB().Where("id = ?", workerID).Take(&noOpWorker).Error; err != nil {
		t.Fatal(err)
	}
	if noOpWorker.CompatibilityStatus != "compatible" || noOpWorker.CompatibilityCheckedAt == nil ||
		!noOpWorker.CompatibilityCheckedAt.Equal(noOpCheckedAt) || noOpWorker.CurrentManifestID == nil ||
		*noOpWorker.CurrentManifestID != manifestID {
		t.Fatalf("semantic no-op invalidated Worker: %#v", noOpWorker)
	}

	routingUpdated, err := service.UpdateProviderPolicy(ctx, owner, domain.TenantID, target.ID, map[string]any{
		"experimentalProviders": []any{"codex", "claudeAgent"},
		"routingPreferences":    map[string]any{"codex": "prefer", "claudeAgent": "avoid"},
	})
	if err != nil {
		t.Fatal(err)
	}
	routingPolicy, err := ParseProviderPolicy(routingUpdated.Capabilities)
	if err != nil {
		t.Fatal(err)
	}
	if routingPolicy.RoutingPreference("codex") != ProviderRoutingPreferencePrefer ||
		routingPolicy.RoutingPreference("claudeAgent") != ProviderRoutingPreferenceAvoid {
		t.Fatalf("routing-only policy was not persisted: %#v", routingPolicy)
	}
	var routingOnlyWorker persistence.WorkerInstance
	if err := store.DB().Where("id = ?", workerID).Take(&routingOnlyWorker).Error; err != nil {
		t.Fatal(err)
	}
	if routingOnlyWorker.CompatibilityStatus != "compatible" || routingOnlyWorker.CompatibilityReason != nil ||
		routingOnlyWorker.CompatibilityCheckedAt == nil ||
		!routingOnlyWorker.CompatibilityCheckedAt.Equal(noOpCheckedAt) ||
		routingOnlyWorker.CurrentManifestID == nil || *routingOnlyWorker.CurrentManifestID != manifestID {
		t.Fatalf("soft routing policy unnecessarily invalidated Worker compatibility: %#v", routingOnlyWorker)
	}

	sharedTargetID := uuid.New()
	if err := store.DB().Create(&persistence.ExecutionTarget{
		ID: sharedTargetID, Kind: "local", Name: "platform-shared", Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{},
	}).Error; err != nil {
		t.Fatal(err)
	}
	_, err = service.UpdateProviderPolicy(ctx, owner, domain.TenantID, sharedTargetID, map[string]any{})
	assertExecutionTargetProblem(t, err, 403, "shared_execution_target_provider_policy_immutable")

	memberID := uuid.New()
	now := time.Now().UTC()
	if err := store.DB().Create(&persistence.User{
		ID: memberID, Email: uuid.NewString() + "@example.com", DisplayName: "Tenant member",
		Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.TenantMembership{
		TenantID: domain.TenantID, UserID: memberID, Role: "member", Status: "active",
		JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	_, err = service.UpdateProviderPolicy(ctx, identity.Principal{
		UserID: memberID, ActiveTenantID: &domain.TenantID,
	}, domain.TenantID, target.ID, map[string]any{})
	assertExecutionTargetProblem(t, err, 403, "tenant_forbidden")

	otherTenantID := uuid.New()
	_, err = service.UpdateProviderPolicy(ctx, identity.Principal{
		UserID: domain.UserID, ActiveTenantID: &otherTenantID,
	}, domain.TenantID, target.ID, map[string]any{})
	assertExecutionTargetProblem(t, err, 404, "tenant_not_found")

	invalidPolicies := []map[string]any{
		{"unknown": true},
		{"experimentalProviders": "codex"},
		{"experimentalProviders": []any{1}},
		{"experimentalProviders": []any{"droid"}},
		{"experimentalProviders": []any{"codex", " CODEX "}},
	}
	for _, policy := range invalidPolicies {
		_, updateErr := service.UpdateProviderPolicy(ctx, owner, domain.TenantID, target.ID, policy)
		assertExecutionTargetProblem(t, updateErr, 400, "invalid_execution_target_provider_policy")
	}
}

func TestUpdateProviderPolicyRevalidatesExistingProcessContainmentPolicy(t *testing.T) {
	ctx := context.Background()
	config, _ := platform.Defaults(platform.ProfilePersonal)
	store, err := database.OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "execution-target-provider-policy-revalidate-test")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store.DB(), config, nil)
	owner := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	target, err := service.Create(ctx, owner, domain.TenantID, CreateInput{
		OrganizationID: &domain.OrganizationID, Kind: "local", Name: "revalidate-policy-target",
		Capabilities: map[string]any{
			"processContainmentPolicy": map[string]any{
				"trustMode":        ProcessContainmentTrustSignedV1,
				"keyId":            "revalidate-key",
				"ed25519PublicKey": processContainmentTestPublicKeyBase64(),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var corruptedTarget persistence.ExecutionTarget
	if err := store.DB().Where("id = ?", target.ID).Take(&corruptedTarget).Error; err != nil {
		t.Fatal(err)
	}
	corruptedTarget.Capabilities = map[string]any{
		"processContainmentPolicy": map[string]any{
			"trustMode": ProcessContainmentTrustSignedV1,
			"keyId":     "revalidate-key",
		},
	}
	if err := store.DB().Model(&corruptedTarget).Select("capabilities").Updates(&corruptedTarget).Error; err != nil {
		t.Fatal(err)
	}
	_, err = service.UpdateProviderPolicy(ctx, owner, domain.TenantID, target.ID, map[string]any{
		"experimentalProviders": []any{"codex"},
	})
	assertExecutionTargetProblem(t, err, 400, "invalid_execution_target_process_containment_policy")
}

func TestUpdateProcessContainmentPolicyNormalizesInvalidatesAndEnforcesAccess(t *testing.T) {
	ctx := context.Background()
	config, _ := platform.Defaults(platform.ProfilePersonal)
	store, err := database.OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "execution-target-containment-policy-update-test")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store.DB(), config, nil)
	owner := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	target, err := service.Create(ctx, owner, domain.TenantID, CreateInput{
		OrganizationID: &domain.OrganizationID, Kind: "local", Name: "mutable-containment-target",
		Capabilities: map[string]any{"workspaceModes": []any{"local", "worktree"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	manifestID, workerID := uuid.New(), uuid.New()
	checkedAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	if err := store.DB().Create(&persistence.WorkerManifest{
		ID: manifestID, ManifestHash: strings.Repeat("b", 64), WorkerBuildVersion: "containment-worker",
		WorkerProtocolMinimum: 2, WorkerProtocolMaximum: 2,
		RuntimeEventMinimum: 2, RuntimeEventMaximum: 2,
		OperatingSystem: "linux", Architecture: "amd64", FeatureFlags: map[string]any{}, CreatedAt: checkedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.WorkerInstance{
		ID: workerID, Incarnation: 1, InstanceUID: uuid.NewString(), ExecutionTargetID: target.ID,
		TargetKind: "local", ClusterID: "containment-test", Namespace: "default", PodName: "containment-worker",
		Version: "containment-worker", ProtocolVersion: 2, Capabilities: map[string]any{}, CurrentManifestID: &manifestID,
		CompatibilityStatus: "compatible", CompatibilityCheckedAt: &checkedAt,
		LeaseSupported: true, FencingSupported: true, AuthTokenHash: secret.HashToken("containment-worker-token"),
		Status: "online", RegisteredAt: checkedAt, LastHeartbeatAt: checkedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}

	updated, err := service.UpdateProcessContainmentPolicy(ctx, owner, domain.TenantID, target.ID, map[string]any{
		"trustMode":        ProcessContainmentTrustSignedV1,
		"keyId":            "test-key",
		"ed25519PublicKey": processContainmentTestPublicKeyBase64Raw(),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertProcessContainmentPolicy(t, updated.Capabilities, ProcessContainmentTrustSignedV1, "test-key", processContainmentTestPublicKeyBase64())
	assertNormalizedProcessContainmentCapability(t, updated.Capabilities, ProcessContainmentTrustSignedV1, "test-key", processContainmentTestPublicKeyBase64())
	var persistedTarget persistence.ExecutionTarget
	if err := store.DB().Where("id = ?", target.ID).Take(&persistedTarget).Error; err != nil {
		t.Fatal(err)
	}
	assertProcessContainmentPolicy(t, persistedTarget.Capabilities, ProcessContainmentTrustSignedV1, "test-key", processContainmentTestPublicKeyBase64())
	assertNormalizedProcessContainmentCapability(t, persistedTarget.Capabilities, ProcessContainmentTrustSignedV1, "test-key", processContainmentTestPublicKeyBase64())
	var invalidated persistence.WorkerInstance
	if err := store.DB().Where("id = ?", workerID).Take(&invalidated).Error; err != nil {
		t.Fatal(err)
	}
	if invalidated.CompatibilityStatus != "incompatible" || invalidated.CompatibilityReason == nil ||
		!strings.Contains(*invalidated.CompatibilityReason, "re-register") || invalidated.CompatibilityCheckedAt == nil ||
		!invalidated.CompatibilityCheckedAt.After(checkedAt) || invalidated.CurrentManifestID == nil ||
		*invalidated.CurrentManifestID != manifestID {
		t.Fatalf("Worker manifest was not safely invalidated: %#v", invalidated)
	}

	noOpCheckedAt := checkedAt.Add(10 * time.Minute)
	if err := store.DB().Model(&persistence.WorkerInstance{}).Where("id = ?", workerID).Updates(map[string]any{
		"compatibility_status": "compatible", "compatibility_reason": nil,
		"compatibility_checked_at": noOpCheckedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	targetUpdatedAt := persistedTarget.UpdatedAt
	if _, err := service.UpdateProcessContainmentPolicy(ctx, owner, domain.TenantID, target.ID, map[string]any{
		"trustMode":        ProcessContainmentTrustSignedV1,
		"keyId":            "test-key",
		"ed25519PublicKey": processContainmentTestPublicKeyBase64(),
	}); err != nil {
		t.Fatal(err)
	}
	var noOpTarget persistence.ExecutionTarget
	if err := store.DB().Where("id = ?", target.ID).Take(&noOpTarget).Error; err != nil {
		t.Fatal(err)
	}
	if !noOpTarget.UpdatedAt.Equal(targetUpdatedAt) {
		t.Fatalf("semantic no-op changed target timestamp: %s -> %s", targetUpdatedAt, noOpTarget.UpdatedAt)
	}
	var noOpWorker persistence.WorkerInstance
	if err := store.DB().Where("id = ?", workerID).Take(&noOpWorker).Error; err != nil {
		t.Fatal(err)
	}
	if noOpWorker.CompatibilityStatus != "compatible" || noOpWorker.CompatibilityCheckedAt == nil ||
		!noOpWorker.CompatibilityCheckedAt.Equal(noOpCheckedAt) || noOpWorker.CurrentManifestID == nil ||
		*noOpWorker.CurrentManifestID != manifestID {
		t.Fatalf("semantic no-op invalidated Worker: %#v", noOpWorker)
	}

	changedKeyCheckedAt := noOpCheckedAt.Add(10 * time.Minute)
	if err := store.DB().Model(&persistence.WorkerInstance{}).Where("id = ?", workerID).Updates(map[string]any{
		"compatibility_status": "compatible", "compatibility_reason": nil,
		"compatibility_checked_at": changedKeyCheckedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	changed, err := service.UpdateProcessContainmentPolicy(ctx, owner, domain.TenantID, target.ID, map[string]any{
		"trustMode":        ProcessContainmentTrustSignedV1,
		"keyId":            "rotated-key",
		"ed25519PublicKey": alternateProcessContainmentTestPublicKeyBase64(),
	})
	if err != nil {
		t.Fatal(err)
	}
	assertProcessContainmentPolicy(t, changed.Capabilities, ProcessContainmentTrustSignedV1, "rotated-key", alternateProcessContainmentTestPublicKeyBase64())
	assertNormalizedProcessContainmentCapability(t, changed.Capabilities, ProcessContainmentTrustSignedV1, "rotated-key", alternateProcessContainmentTestPublicKeyBase64())
	var changedTarget persistence.ExecutionTarget
	if err := store.DB().Where("id = ?", target.ID).Take(&changedTarget).Error; err != nil {
		t.Fatal(err)
	}
	if !changedTarget.UpdatedAt.After(targetUpdatedAt) {
		t.Fatalf("changed process containment key did not update target timestamp: %s -> %s", targetUpdatedAt, changedTarget.UpdatedAt)
	}
	var changedWorker persistence.WorkerInstance
	if err := store.DB().Where("id = ?", workerID).Take(&changedWorker).Error; err != nil {
		t.Fatal(err)
	}
	if changedWorker.CompatibilityStatus != "incompatible" || changedWorker.CompatibilityCheckedAt == nil ||
		!changedWorker.CompatibilityCheckedAt.After(changedKeyCheckedAt) {
		t.Fatalf("changed key did not invalidate Worker: %#v", changedWorker)
	}

	sharedTargetID := uuid.New()
	if err := store.DB().Create(&persistence.ExecutionTarget{
		ID: sharedTargetID, Kind: "local", Name: "platform-shared-containment", Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{},
	}).Error; err != nil {
		t.Fatal(err)
	}
	_, err = service.UpdateProcessContainmentPolicy(ctx, owner, domain.TenantID, sharedTargetID, map[string]any{})
	assertExecutionTargetProblem(t, err, 403, "shared_execution_target_process_containment_policy_immutable")

	memberID := uuid.New()
	now := time.Now().UTC()
	if err := store.DB().Create(&persistence.User{
		ID: memberID, Email: uuid.NewString() + "@example.com", DisplayName: "Tenant member",
		Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.TenantMembership{
		TenantID: domain.TenantID, UserID: memberID, Role: "member", Status: "active",
		JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	_, err = service.UpdateProcessContainmentPolicy(ctx, identity.Principal{
		UserID: memberID, ActiveTenantID: &domain.TenantID,
	}, domain.TenantID, target.ID, map[string]any{})
	assertExecutionTargetProblem(t, err, 403, "tenant_forbidden")

	otherTenantID := uuid.New()
	_, err = service.UpdateProcessContainmentPolicy(ctx, identity.Principal{
		UserID: domain.UserID, ActiveTenantID: &otherTenantID,
	}, domain.TenantID, target.ID, map[string]any{})
	assertExecutionTargetProblem(t, err, 404, "tenant_not_found")

	invalidPolicies := []map[string]any{
		{"mode": true},
		{"trustMode": true},
		{"trustMode": "kernel-attested-v9"},
		{"trustMode": ProcessContainmentTrustDisabled, "keyId": "extra"},
		{"trustMode": ProcessContainmentTrustSignedV1, "keyId": "missing-public-key"},
		{"trustMode": ProcessContainmentTrustSignedV1, "keyId": "bad key", "ed25519PublicKey": processContainmentTestPublicKeyBase64()},
		{"trustMode": ProcessContainmentTrustSignedV1, "keyId": "bad-base64", "ed25519PublicKey": "!!!"},
	}
	for _, policy := range invalidPolicies {
		_, updateErr := service.UpdateProcessContainmentPolicy(ctx, owner, domain.TenantID, target.ID, policy)
		assertExecutionTargetProblem(t, updateErr, 400, "invalid_execution_target_process_containment_policy")
	}
}

func assertExperimentalProviders(t *testing.T, capabilities map[string]any, expected ...string) {
	t.Helper()
	policy, err := ParseProviderPolicy(capabilities)
	if err != nil {
		t.Fatal(err)
	}
	if len(policy.ExperimentalProviders) != len(expected) {
		t.Fatalf("Experimental Providers = %#v, want %#v", policy.ExperimentalProviders, expected)
	}
	for index := range expected {
		if policy.ExperimentalProviders[index] != expected[index] {
			t.Fatalf("Experimental Providers = %#v, want %#v", policy.ExperimentalProviders, expected)
		}
	}
}

func assertProcessContainmentPolicy(t *testing.T, capabilities map[string]any, expectedTrustMode, expectedKeyID, expectedPublicKeyBase64 string) {
	t.Helper()
	policy, err := ParseProcessContainmentPolicy(capabilities)
	if err != nil {
		t.Fatal(err)
	}
	if policy.TrustMode != expectedTrustMode {
		t.Fatalf("process containment trust mode = %q, want %q", policy.TrustMode, expectedTrustMode)
	}
	if policy.KeyID != expectedKeyID {
		t.Fatalf("process containment key id = %q, want %q", policy.KeyID, expectedKeyID)
	}
	if expectedPublicKeyBase64 == "" {
		if len(policy.PublicKey) != 0 || policy.PublicKeySHA256 != "" {
			t.Fatalf("disabled process containment policy should not include key material: %#v", policy)
		}
		return
	}
	if actual := base64.StdEncoding.EncodeToString(policy.PublicKey); actual != expectedPublicKeyBase64 {
		t.Fatalf("process containment public key = %q, want %q", actual, expectedPublicKeyBase64)
	}
	if policy.PublicKeySHA256 != processContainmentPublicKeySHA256(policy.PublicKey) {
		t.Fatalf("process containment public key digest = %q, want %q", policy.PublicKeySHA256, processContainmentPublicKeySHA256(policy.PublicKey))
	}
}

func assertNormalizedProcessContainmentCapability(t *testing.T, capabilities map[string]any, expectedTrustMode, expectedKeyID, expectedPublicKeyBase64 string) {
	t.Helper()
	rawPolicy, ok := capabilities["processContainmentPolicy"]
	if !ok {
		t.Fatal("process containment policy was not materialized in capabilities")
	}
	policy, ok := rawPolicy.(map[string]any)
	if !ok {
		t.Fatalf("process containment capability shape = %#v", rawPolicy)
	}
	if policy["trustMode"] != expectedTrustMode {
		t.Fatalf("process containment capability trustMode = %#v, want %q", policy["trustMode"], expectedTrustMode)
	}
	if expectedTrustMode == ProcessContainmentTrustDisabled {
		if len(policy) != 1 {
			t.Fatalf("disabled process containment capability should only contain trustMode: %#v", policy)
		}
		return
	}
	if policy["keyId"] != expectedKeyID {
		t.Fatalf("process containment capability keyId = %#v, want %q", policy["keyId"], expectedKeyID)
	}
	if policy["ed25519PublicKey"] != expectedPublicKeyBase64 {
		t.Fatalf("process containment capability ed25519PublicKey = %#v, want %q", policy["ed25519PublicKey"], expectedPublicKeyBase64)
	}
	if len(policy) != 3 {
		t.Fatalf("signed process containment capability has unexpected fields: %#v", policy)
	}
}

func assertExecutionTargetProblem(t *testing.T, err error, status int, code string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Status != status || apiError.Code != code {
		t.Fatalf("problem = %#v, want status=%d code=%q (error: %v)", apiError, status, code, err)
	}
}
