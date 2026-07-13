package executions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestListWorkerManifestsIsTenantScopedAndKeepsTargetManifestGroups(t *testing.T) {
	ctx := context.Background()
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, profile, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "manifest-projection-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 7, 14, 8, 0, 0, 0, time.UTC)
	manifestA := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	manifestB := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	seedProjectionManifest(t, store.DB(), manifestA, "worker-a", now)
	seedProjectionManifest(t, store.DB(), manifestB, "worker-b", now.Add(time.Minute))

	otherTenantID := uuid.New()
	if err := store.DB().Create(&persistence.Tenant{
		ID: otherTenantID, Slug: "projection-other-" + uuid.NewString(), Name: "Other tenant",
		Status: "active", PlanCode: "developer", Region: "local", Settings: map[string]any{},
		CreatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	platformTargetID := uuid.New()
	otherTenantTargetID := uuid.New()
	for _, target := range []persistence.ExecutionTarget{
		{
			ID: platformTargetID, Kind: "local", Name: "platform-shared-test", Status: "active",
			ConfigurationEncrypted: []byte("platform-target-secret"), Capabilities: map[string]any{},
			CreatedAt: now, UpdatedAt: now,
		},
		{
			ID: otherTenantTargetID, TenantID: &otherTenantID, Kind: "local", Name: "other-tenant-test", Status: "active",
			ConfigurationEncrypted: []byte("other-tenant-secret"), Capabilities: map[string]any{},
			CreatedAt: now, UpdatedAt: now,
		},
	} {
		if err := store.DB().Create(&target).Error; err != nil {
			t.Fatal(err)
		}
	}

	seedProjectionWorker(t, store.DB(), domain.ExecutionTargetID, &manifestA, "online", now.Add(time.Minute), nil)
	drainingAt := now.Add(2 * time.Minute)
	seedProjectionWorker(t, store.DB(), domain.ExecutionTargetID, &manifestA, "draining", drainingAt, &drainingAt)
	seedProjectionWorker(t, store.DB(), domain.ExecutionTargetID, &manifestA, "offline", now.Add(3*time.Minute), nil)
	terminatedAt := now.Add(24 * time.Hour)
	seedProjectionWorker(t, store.DB(), domain.ExecutionTargetID, &manifestA, "terminated", terminatedAt, &terminatedAt)
	seedProjectionWorker(t, store.DB(), domain.ExecutionTargetID, &manifestB, "online", now.Add(4*time.Minute), nil)
	seedProjectionWorker(t, store.DB(), domain.ExecutionTargetID, nil, "online", now.Add(5*time.Minute), nil)
	seedProjectionWorker(t, store.DB(), platformTargetID, &manifestA, "online", now.Add(6*time.Minute), nil)
	seedProjectionWorker(t, store.DB(), otherTenantTargetID, &manifestB, "online", now.Add(7*time.Minute), nil)

	service := NewService(store.DB(), nil, time.Minute, time.Minute, time.Hour, nil, nil)
	principal := identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}
	items, err := service.ListWorkerManifests(ctx, principal, domain.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %#v, want two tenant-owned Target/Manifest groups", items)
	}
	if items[0].ExecutionTargetID != domain.ExecutionTargetID || items[1].ExecutionTargetID != domain.ExecutionTargetID ||
		items[0].ManifestID != manifestA || items[1].ManifestID != manifestB {
		t.Fatalf("items are not stably ordered by Target and Manifest: %#v", items)
	}
	if got := items[0].WorkerStatusCounts; got != (WorkerManifestStatusCounts{Online: 1, Draining: 1, Offline: 1}) {
		t.Fatalf("manifest A status counts = %#v", got)
	}
	if !items[0].LastHeartbeatAt.Equal(now.Add(3 * time.Minute)) {
		t.Fatalf("manifest A last heartbeat = %s", items[0].LastHeartbeatAt)
	}
	if got := items[1].WorkerStatusCounts; got != (WorkerManifestStatusCounts{Online: 1}) {
		t.Fatalf("manifest B status counts = %#v", got)
	}
	if items[0].WorkerBuild.Version != "worker-a" || items[0].WorkerBuild.OperatingSystem != "linux" ||
		items[0].WorkerProtocol != (WorkerManifestVersionRange{Minimum: 2, Maximum: 2}) ||
		items[0].RuntimeEvent != (WorkerManifestVersionRange{Minimum: 2, Maximum: 2}) {
		t.Fatalf("unexpected manifest projection: %#v", items[0])
	}
	if len(items[0].Providers) != len(stage3ProviderNames) {
		t.Fatalf("provider count = %d", len(items[0].Providers))
	}
	for index, provider := range items[0].Providers {
		if provider.Provider != stage3ProviderNames[index] || len(provider.Capabilities) != len(stage3ProviderCapabilityIDs) {
			t.Fatalf("provider projection %d = %#v", index, provider)
		}
	}

	again, err := service.ListWorkerManifests(ctx, principal, domain.TenantID)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 2 || again[0].ManifestID != manifestA || again[1].ManifestID != manifestB {
		t.Fatalf("repeated projection order changed: %#v", again)
	}
}

func TestListWorkerManifestsEnforcesActiveTenantAndWorkerRead(t *testing.T) {
	ctx := context.Background()
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, profile, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "manifest-auth-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store.DB(), nil, time.Minute, time.Minute, time.Hour, nil, nil)

	otherTenantID := uuid.New()
	_, err = service.ListWorkerManifests(ctx, identity.Principal{
		UserID: domain.UserID, ActiveTenantID: &otherTenantID,
	}, domain.TenantID)
	assertManifestProjectionProblem(t, err, 404, "tenant_not_found")

	now := time.Now().UTC()
	memberID := uuid.New()
	if err := store.DB().Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&persistence.User{
			ID: memberID, Email: uuid.NewString() + "@example.com", DisplayName: "Tenant member",
			Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			return err
		}
		return tx.Create(&persistence.TenantMembership{
			TenantID: domain.TenantID, UserID: memberID, Role: "member", Status: "active",
			JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error
	}); err != nil {
		t.Fatal(err)
	}
	_, err = service.ListWorkerManifests(ctx, identity.Principal{
		UserID: memberID, ActiveTenantID: &domain.TenantID,
	}, domain.TenantID)
	assertManifestProjectionProblem(t, err, 403, "tenant_forbidden")
}

func seedProjectionManifest(t *testing.T, db *gorm.DB, manifestID uuid.UUID, version string, now time.Time) {
	t.Helper()
	gitSHA := "abcdef1234567890"
	imageDigest := "sha256:" + digestProjectionValue("image:"+manifestID.String())
	manifest := persistence.WorkerManifest{
		ID: manifestID, ManifestHash: digestProjectionValue("manifest:" + manifestID.String()),
		WorkerBuildVersion: version, WorkerBuildGitSHA: &gitSHA,
		WorkerProtocolMinimum: 2, WorkerProtocolMaximum: 2,
		RuntimeEventMinimum: 2, RuntimeEventMaximum: 2,
		OperatingSystem: "linux", Architecture: "amd64", ImageDigest: &imageDigest,
		FeatureFlags: map[string]any{"rawFeatureSecret": "must-not-be-projected"}, CreatedAt: now,
	}
	if err := db.Create(&manifest).Error; err != nil {
		t.Fatal(err)
	}
	capabilities := make(map[string]any, len(stage3ProviderCapabilityIDs))
	for _, capabilityID := range stage3ProviderCapabilityIDs {
		capabilities[capabilityID] = "native"
	}
	for _, provider := range stage3ProviderNames {
		experimental := provider == "codex" || provider == "claudeAgent"
		supportTier := "local-only"
		compatibilityStatus := "local-only"
		runtimeAvailable := false
		runtimeCompatible := false
		var runtimeVersion *string
		if experimental {
			supportTier = "experimental"
			compatibilityStatus = "compatible"
			runtimeAvailable = true
			runtimeCompatible = true
			version := "1.0.0"
			runtimeVersion = &version
		}
		runtimeKind := "cli"
		runtimeSource := "probe"
		if provider == "claudeAgent" {
			runtimeKind = "sdk"
			runtimeSource = "package"
		}
		model := persistence.WorkerProviderManifest{
			WorkerManifestID: manifestID, Provider: providerStorageName(provider), SupportTier: supportTier,
			CompatibilityStatus: compatibilityStatus, ProviderHostMajor: 2, ProviderHostMinor: 1,
			HostBuildVersion: "host-test", AdapterVersion: "adapter-test",
			RuntimeKind: runtimeKind, RuntimeName: provider + "-runtime", RuntimeVersion: runtimeVersion,
			RuntimeAvailable: runtimeAvailable, RuntimeVersionSource: runtimeSource,
			RuntimeMinimumInclusive: "0.0.0", RuntimeCompatible: runtimeCompatible,
			ReleaseRequiresExplicitEnablement: experimental, ReleaseEnabled: true,
			MaximumCommandBytes: 1024, MaximumMessageBytes: 1024,
			RuntimeEventMinimum: 2, RuntimeEventMaximum: 2,
			CredentialDeliveryModes: []string{"anonymous-fd"}, ResumeStrategies: []string{"authoritative-history"},
			CapabilityDescriptorHash: digestProjectionValue(manifestID.String() + ":" + provider),
			Capabilities:             capabilities, CheckedAt: now,
		}
		if err := db.Create(&model).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func seedProjectionWorker(
	t *testing.T,
	db *gorm.DB,
	targetID uuid.UUID,
	manifestID *uuid.UUID,
	status string,
	heartbeat time.Time,
	statusAt *time.Time,
) {
	t.Helper()
	workerID := uuid.New()
	compatibilityStatus := "unknown"
	var compatibilityCheckedAt *time.Time
	if manifestID != nil {
		compatibilityStatus = "compatible"
		compatibilityCheckedAt = &heartbeat
	}
	worker := persistence.WorkerInstance{
		ID: workerID, Incarnation: 1, InstanceUID: uuid.NewString(), ExecutionTargetID: targetID, TargetKind: "local",
		ClusterID: "cluster-" + workerID.String(), Namespace: "namespace-secret", PodName: "pod-" + workerID.String(),
		Version: "raw-worker-version", ProtocolVersion: 2,
		Capabilities: map[string]any{"rawWorkerToken": "must-not-be-projected"}, CurrentManifestID: manifestID,
		CompatibilityStatus: compatibilityStatus, CompatibilityCheckedAt: compatibilityCheckedAt,
		LeaseSupported: true, FencingSupported: true, AuthTokenHash: []byte("token-" + workerID.String()),
		Status: status, RegisteredAt: heartbeat.Add(-time.Hour), LastHeartbeatAt: heartbeat,
	}
	if status == "draining" {
		worker.DrainingAt = statusAt
	}
	if status == "terminated" {
		worker.TerminatedAt = statusAt
	}
	if err := db.Create(&worker).Error; err != nil {
		t.Fatal(err)
	}
}

func digestProjectionValue(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func assertManifestProjectionProblem(t *testing.T, err error, status int, code string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Status != status || apiError.Code != code {
		t.Fatalf("error = %#v, want status %d code %q", err, status, code)
	}
}
