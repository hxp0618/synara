package providercapabilities

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/providercatalog"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestLoadTargetPoolProjectionIsPoolScoped(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "resolver-pool-scope-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}

	var target persistence.ExecutionTarget
	if err := store.DB().Where("id = ?", domain.ExecutionTargetID).Take(&target).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	defaultPool := persistence.WorkerPool{
		ID: uuid.New(), TenantID: target.TenantID, ExecutionTargetID: target.ID,
		Name: "default", Mode: "warm", CapacityClass: "standard",
		DesiredIdleUnits: 0, MaxActiveUnits: 1, SchedulingTemplate: map[string]any{},
		Status: "active", Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	alternatePool := persistence.WorkerPool{
		ID: uuid.New(), TenantID: target.TenantID, ExecutionTargetID: target.ID,
		Name: "alternate", Mode: "warm", CapacityClass: "interactive",
		DesiredIdleUnits: 0, MaxActiveUnits: 1, SchedulingTemplate: map[string]any{},
		Status: "active", Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := store.DB().Create(&defaultPool).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&alternatePool).Error; err != nil {
		t.Fatal(err)
	}
	seedResolverPoolWorker(t, store.DB(), target, alternatePool)

	targetProjection, err := LoadTargetProjection(ctx, store.DB(), target, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if decision := Check(targetProjection, "codex", "send-turn"); decision.Status != StatusSupported {
		t.Fatalf("target-wide decision = %#v, want supported", decision)
	}

	poolProjection, err := LoadTargetPoolProjection(ctx, store.DB(), target, defaultPool.ID, defaultPool.Version, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	decision := Check(poolProjection, "codex", "send-turn")
	if decision.Status != StatusUnobserved || decision.ReasonCode != ReasonWorkerManifestRequired {
		t.Fatalf("pool-scoped decision = %#v, want unobserved worker_manifest_required", decision)
	}
}

func seedResolverPoolWorker(
	t *testing.T,
	db *gorm.DB,
	target persistence.ExecutionTarget,
	pool persistence.WorkerPool,
) {
	t.Helper()
	now := time.Now().UTC()
	manifestID := uuid.New()
	manifestDigest := sha256.Sum256([]byte("resolver-pool-worker:" + manifestID.String()))
	if err := db.Create(&persistence.WorkerManifest{
		ID: manifestID, ManifestHash: hex.EncodeToString(manifestDigest[:]),
		WorkerBuildVersion: "resolver-pool-worker", WorkerProtocolMinimum: 2, WorkerProtocolMaximum: 2,
		RuntimeEventMinimum: 2, RuntimeEventMaximum: 2, OperatingSystem: "linux", Architecture: "amd64",
		FeatureFlags: map[string]any{}, CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	for _, entry := range providercatalog.Providers() {
		capabilities := make(map[string]any, len(entry.Capabilities))
		for capabilityID, support := range entry.Capabilities {
			capabilities[capabilityID] = support
		}
		version := entry.RuntimePolicy.CompatibleRange.MinimumInclusive
		descriptorDigest := sha256.Sum256([]byte(manifestID.String() + ":" + entry.Name))
		if err := db.Create(&persistence.WorkerProviderManifest{
			WorkerManifestID: manifestID, Provider: entry.Name, SupportTier: entry.SupportTier,
			CompatibilityStatus: "compatible", ProviderHostMajor: 2, ProviderHostMinor: 1,
			HostBuildVersion: "host-test", AdapterVersion: entry.AdapterVersion,
			RuntimeKind: entry.RuntimePolicy.Kind, RuntimeName: entry.RuntimePolicy.Name, RuntimeVersion: &version,
			RuntimeAvailable: true, RuntimeVersionSource: entry.RuntimePolicy.VersionSource,
			RuntimeMinimumInclusive: entry.RuntimePolicy.CompatibleRange.MinimumInclusive,
			RuntimeCompatible:       true, ReleaseRequiresExplicitEnablement: entry.SupportTier == "experimental",
			ReleaseEnabled: true, MaximumCommandBytes: 1024, MaximumMessageBytes: 1024,
			RuntimeEventMinimum: 2, RuntimeEventMaximum: 2, CredentialDeliveryModes: []string{"anonymous-fd"},
			ResumeStrategies: []string{"authoritative-history"}, CapabilityDescriptorHash: hex.EncodeToString(descriptorDigest[:]),
			Capabilities: capabilities, CheckedAt: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Create(&persistence.WorkerInstance{
		ID: uuid.New(), Incarnation: 1, InstanceUID: uuid.NewString(), ExecutionTargetID: target.ID,
		TargetKind: target.Kind, WorkerMode: "warm-pool", WorkerPoolID: &pool.ID, WorkerPoolVersion: &pool.Version, CapacityClass: &pool.CapacityClass,
		ClusterID: uuid.NewString(), Namespace: "default", PodName: uuid.NewString(),
		Version: "resolver-pool-worker", ProtocolVersion: 2, Capabilities: map[string]any{},
		CurrentManifestID: &manifestID, CompatibilityStatus: "compatible", CompatibilityCheckedAt: &now,
		LeaseSupported: true, FencingSupported: true, AuthTokenHash: []byte(uuid.NewString()),
		Status: "online", AdministrativeStatus: "active", RegisteredAt: now, LastHeartbeatAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
}
