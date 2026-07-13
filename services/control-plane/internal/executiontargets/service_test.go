package executiontargets

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
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
	cipher, err := secret.NewCursorCipher(bytes.Repeat([]byte{0x23}, 32))
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
	var persisted persistence.ExecutionTarget
	if err := store.DB().Where("id = ?", created.ID).Take(&persisted).Error; err != nil {
		t.Fatal(err)
	}
	if len(persisted.ConfigurationEncrypted) == 0 || bytes.Contains(persisted.ConfigurationEncrypted, []byte("secret-value")) {
		t.Fatal("execution target configuration was not encrypted")
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
		Capabilities: map[string]any{"workspaceModes": []any{"local", "worktree"}},
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

func assertExecutionTargetProblem(t *testing.T, err error, status int, code string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Status != status || apiError.Code != code {
		t.Fatalf("problem = %#v, want status=%d code=%q (error: %v)", apiError, status, code, err)
	}
}
