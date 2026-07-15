package database

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestProviderRuntimeReleasePolicyMigrationFencesLegacyManifests(t *testing.T) {
	databaseURL := os.Getenv("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("SYNARA_TEST_STAGE3_MIGRATION_DATABASE_URL is not configured")
	}
	db := openIsolatedMigrationSchema(t, databaseURL)
	if err := Migrate(context.Background(), db, migrationsThrough(t, "000028_interaction_runtime_event_version.sql")); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	targetID, manifestID, workerID := uuid.New(), uuid.New(), uuid.New()
	target := persistence.ExecutionTarget{
		ID: targetID, Kind: "kubernetes", Name: "runtime-policy-migration", Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{},
	}
	manifest := persistence.WorkerManifest{
		ID: manifestID, ManifestHash: strings.Repeat("a", 64), WorkerBuildVersion: "legacy-worker",
		WorkerProtocolMinimum: 2, WorkerProtocolMaximum: 2,
		RuntimeEventMinimum: 2, RuntimeEventMaximum: 2,
		OperatingSystem: "linux", Architecture: "amd64", FeatureFlags: map[string]any{}, CreatedAt: now,
	}
	if err := db.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	userID, tenantID := uuid.New(), uuid.New()
	if err := db.Create(&persistence.User{
		ID: userID, Email: uuid.NewString() + "@example.com", DisplayName: "Migration owner",
		Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&persistence.Tenant{
			ID: tenantID, Slug: "runtime-policy-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12],
			Name: "Runtime policy migration", Status: "active", PlanCode: "free", Region: "default",
			Settings: map[string]any{}, CreatedBy: userID, CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			return err
		}
		return tx.Create(&persistence.TenantMembership{
			TenantID: tenantID, UserID: userID, Role: "owner", Status: "active",
			JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error
	}); err != nil {
		t.Fatal(err)
	}
	missingPolicyTargetID, emptyPolicyTargetID, explicitPolicyTargetID := uuid.New(), uuid.New(), uuid.New()
	ownedTargets := []persistence.ExecutionTarget{
		{
			ID: missingPolicyTargetID, TenantID: &tenantID, Kind: "local", Name: "missing-policy",
			Status: "active", ConfigurationEncrypted: []byte{},
			Capabilities: map[string]any{"workspaceModes": []any{"local", "worktree"}, "preserved": true},
		},
		{
			ID: emptyPolicyTargetID, TenantID: &tenantID, Kind: "local", Name: "empty-policy",
			Status: "active", ConfigurationEncrypted: []byte{},
			Capabilities: map[string]any{"providerPolicy": map[string]any{"experimentalProviders": []any{}}},
		},
		{
			ID: explicitPolicyTargetID, TenantID: &tenantID, Kind: "local", Name: "explicit-policy",
			Status: "active", ConfigurationEncrypted: []byte{},
			Capabilities: map[string]any{"providerPolicy": map[string]any{"experimentalProviders": []any{"opencode"}}},
		},
	}
	for index := range ownedTargets {
		if err := db.Create(&ownedTargets[index]).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&manifest).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`
		INSERT INTO worker_provider_manifests (
			worker_manifest_id, provider, support_tier, compatibility_status,
			provider_host_protocol_major, provider_host_protocol_minor,
			host_build_version, adapter_version, provider_cli_version,
			maximum_command_bytes, maximum_message_bytes,
			runtime_event_minimum, runtime_event_maximum,
			credential_delivery_modes, resume_strategies,
			capability_descriptor_hash, capabilities, checked_at
		) VALUES (?, 'codex', 'experimental', 'compatible', 2, 0, 'legacy-host', 'legacy-adapter', '0.143.0',
			2097152, 1048576, 2, 2, '["anonymous-fd"]'::jsonb,
			'["authoritative-history"]'::jsonb, ?, '{"send-turn":"native"}'::jsonb, ?)
	`, manifestID, strings.Repeat("b", 64), now).Error; err != nil {
		t.Fatal(err)
	}
	worker := persistence.WorkerInstance{
		ID: workerID, Incarnation: 1, InstanceUID: uuid.NewString(), ExecutionTargetID: targetID,
		TargetKind: "kubernetes", ClusterID: "migration", Namespace: "default", PodName: "legacy-worker",
		Version: "legacy-worker", ProtocolVersion: 2, Capabilities: map[string]any{}, CurrentManifestID: &manifestID,
		CompatibilityStatus: "compatible", CompatibilityCheckedAt: &now,
		LeaseSupported: true, FencingSupported: true, AuthTokenHash: secret.HashToken("runtime-policy-migration"),
		Status: "online", RegisteredAt: now, LastHeartbeatAt: now,
	}
	if err := db.Omit(
		"AdministrativeStatus", "RevokedAt", "RevokedBy", "RevocationReason",
		"WorkerReleaseRevisionID", "WorkerReleaseChannel", "WorkerReleaseStatus",
		"WorkerReleaseReason", "WorkerReleaseCheckedAt",
	).Create(&worker).Error; err != nil {
		t.Fatal(err)
	}

	if err := Migrate(context.Background(), db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	var provider persistence.WorkerProviderManifest
	if err := db.Where("worker_manifest_id = ? AND provider = ?", manifestID, "codex").Take(&provider).Error; err != nil {
		t.Fatal(err)
	}
	if provider.CompatibilityStatus != "disabled" || !provider.ReleaseRequiresExplicitEnablement ||
		provider.ReleaseEnabled || provider.RuntimeAvailable || provider.RuntimeCompatible ||
		provider.RuntimeVersion == nil || *provider.RuntimeVersion != "0.143.0" ||
		provider.IncompatibilityCode == nil || *provider.IncompatibilityCode != "capability_unsupported" {
		t.Fatalf("legacy Experimental Provider was not fenced: %#v", provider)
	}
	var missingPolicyTarget persistence.ExecutionTarget
	if err := db.Where("id = ?", missingPolicyTargetID).Take(&missingPolicyTarget).Error; err != nil {
		t.Fatal(err)
	}
	assertMigrationProviderPolicy(t, missingPolicyTarget.Capabilities, true, "codex", "claudeAgent")
	workspaceModes, err := json.Marshal(missingPolicyTarget.Capabilities["workspaceModes"])
	if err != nil {
		t.Fatal(err)
	}
	if string(workspaceModes) != `["local","worktree"]` || missingPolicyTarget.Capabilities["preserved"] != true {
		t.Fatalf("tenant-owned Target capabilities were not preserved: %#v", missingPolicyTarget.Capabilities)
	}
	var emptyPolicyTarget persistence.ExecutionTarget
	if err := db.Where("id = ?", emptyPolicyTargetID).Take(&emptyPolicyTarget).Error; err != nil {
		t.Fatal(err)
	}
	assertMigrationProviderPolicy(t, emptyPolicyTarget.Capabilities, true)
	var explicitPolicyTarget persistence.ExecutionTarget
	if err := db.Where("id = ?", explicitPolicyTargetID).Take(&explicitPolicyTarget).Error; err != nil {
		t.Fatal(err)
	}
	assertMigrationProviderPolicy(t, explicitPolicyTarget.Capabilities, true, "opencode")
	var sharedTarget persistence.ExecutionTarget
	if err := db.Where("id = ?", targetID).Take(&sharedTarget).Error; err != nil {
		t.Fatal(err)
	}
	assertMigrationProviderPolicy(t, sharedTarget.Capabilities, false)
	var migratedWorker persistence.WorkerInstance
	if err := db.Where("id = ?", workerID).Take(&migratedWorker).Error; err != nil {
		t.Fatal(err)
	}
	if migratedWorker.CompatibilityStatus != "incompatible" || migratedWorker.CompatibilityReason == nil ||
		!strings.Contains(*migratedWorker.CompatibilityReason, "re-register") {
		t.Fatalf("legacy Worker was not forced to re-register: %#v", migratedWorker)
	}
	if err := db.Model(&persistence.WorkerProviderManifest{}).
		Where("worker_manifest_id = ? AND provider = ?", manifestID, "codex").
		Updates(map[string]any{"compatibility_status": "disabled", "release_enabled": true}).Error; err == nil {
		t.Fatal("runtime/release-policy migration accepted an enabled Provider with disabled compatibility status")
	}
	var bindingRuntimeColumns int64
	if err := db.Raw(`
		SELECT count(*)
		FROM information_schema.columns
		WHERE table_schema = current_schema()
		  AND table_name = 'provider_runtime_bindings'
		  AND column_name IN (
			'runtime_kind', 'runtime_name', 'runtime_version', 'runtime_available',
			'runtime_version_source', 'runtime_minimum_inclusive', 'runtime_maximum_exclusive',
			'runtime_compatible', 'release_requires_explicit_enablement', 'release_enabled'
		  )
	`).Scan(&bindingRuntimeColumns).Error; err != nil {
		t.Fatal(err)
	}
	if bindingRuntimeColumns != 10 {
		t.Fatalf("provider_runtime_bindings runtime snapshot columns = %d, want 10", bindingRuntimeColumns)
	}
}

func assertMigrationProviderPolicy(t *testing.T, capabilities map[string]any, expectedPresent bool, expected ...string) {
	t.Helper()
	rawPolicy, present := capabilities["providerPolicy"]
	if present != expectedPresent {
		t.Fatalf("providerPolicy presence = %t, want %t in %#v", present, expectedPresent, capabilities)
	}
	if !expectedPresent {
		return
	}
	policy, ok := rawPolicy.(map[string]any)
	if !ok {
		t.Fatalf("providerPolicy = %#v", rawPolicy)
	}
	rawProviders, ok := policy["experimentalProviders"]
	if !ok {
		t.Fatalf("providerPolicy.experimentalProviders missing: %#v", policy)
	}
	providers := make([]string, 0)
	switch values := rawProviders.(type) {
	case []any:
		for _, value := range values {
			provider, ok := value.(string)
			if !ok {
				t.Fatalf("Provider name = %#v", value)
			}
			providers = append(providers, provider)
		}
	case []string:
		providers = append(providers, values...)
	default:
		t.Fatalf("experimentalProviders = %#v", rawProviders)
	}
	if len(providers) != len(expected) {
		t.Fatalf("Experimental Providers = %#v, want %#v", providers, expected)
	}
	for index := range expected {
		if providers[index] != expected[index] {
			t.Fatalf("Experimental Providers = %#v, want %#v", providers, expected)
		}
	}
}
