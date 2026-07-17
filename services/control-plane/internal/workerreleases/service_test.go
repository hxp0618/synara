package workerreleases

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
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

func TestWorkerReleaseCanaryPromotionRollbackAndScheduling(t *testing.T) {
	fixture := newReleaseFixture(t)
	first := fixture.createRevision(t, fixture.firstManifestID, "initial production release", "release-first")
	second := fixture.createRevision(t, fixture.secondManifestID, "candidate release", "release-second")

	promoted := fixture.changePolicy(t, "promote", first.ID, PolicyChangeInput{
		ExpectedPolicyVersion: 0, Reason: "initial promotion",
	}, "promote-first")
	assertPolicy(t, promoted, 1, first.ID, nil, 0)
	fixture.assertWorkerRelease(t, fixture.firstWorkerID, "active", first.ID, ChannelPromoted)
	fixture.assertWorkerRelease(t, fixture.secondWorkerID, "inactive", second.ID, "")

	stableSelection, err := SelectExecution(fixture.ctx, fixture.db, fixture.targetID, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if stableSelection == nil || stableSelection.RevisionID != first.ID || stableSelection.Channel != ChannelPromoted {
		t.Fatalf("stable selection = %#v", stableSelection)
	}

	canary := fixture.changePolicy(t, "canary", second.ID, PolicyChangeInput{
		ExpectedPolicyVersion: 1, CanaryPercent: 100, Reason: "all deterministic fixture traffic",
	}, "canary-second")
	assertPolicy(t, canary, 2, first.ID, &second.ID, 100)
	fixture.assertWorkerRelease(t, fixture.firstWorkerID, "active", first.ID, ChannelPromoted)
	fixture.assertWorkerRelease(t, fixture.secondWorkerID, "active", second.ID, ChannelCanary)

	canarySelection, err := SelectExecution(fixture.ctx, fixture.db, fixture.targetID, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if canarySelection == nil || canarySelection.RevisionID != second.ID || canarySelection.Channel != ChannelCanary {
		t.Fatalf("canary selection = %#v", canarySelection)
	}

	executionID := fixture.seedQueuedExecution(t, first.ID, ChannelPromoted)
	promoted = fixture.changePolicy(t, "promote", second.ID, PolicyChangeInput{
		ExpectedPolicyVersion: 2, Reason: "canary passed smoke gates",
	}, "promote-second")
	assertPolicy(t, promoted, 3, second.ID, nil, 0)
	fixture.assertExecutionRelease(t, executionID, second.ID, ChannelPromoted)
	fixture.assertWorkerRelease(t, fixture.firstWorkerID, "inactive", first.ID, "")
	fixture.assertWorkerRelease(t, fixture.secondWorkerID, "active", second.ID, ChannelPromoted)

	rolledBack := fixture.changePolicy(t, "rollback", first.ID, PolicyChangeInput{
		ExpectedPolicyVersion: 3, Reason: "runtime event regression",
	}, "rollback-first")
	assertPolicy(t, rolledBack, 4, first.ID, nil, 0)
	fixture.assertExecutionRelease(t, executionID, first.ID, ChannelPromoted)
	fixture.assertWorkerRelease(t, fixture.firstWorkerID, "active", first.ID, ChannelPromoted)
	fixture.assertWorkerRelease(t, fixture.secondWorkerID, "inactive", second.ID, "")
	if err := fixture.db.Model(&persistence.WorkerInstance{}).Where("id = ?", fixture.secondWorkerID).
		Update("status", "offline").Error; err != nil {
		t.Fatal(err)
	}

	var inactiveWorker persistence.WorkerInstance
	if err := fixture.db.Where("id = ?", fixture.secondWorkerID).Take(&inactiveWorker).Error; err != nil {
		t.Fatal(err)
	}
	err = fixture.db.Transaction(func(tx *gorm.DB) error {
		return RequireActiveWorker(fixture.ctx, tx, inactiveWorker)
	})
	assertProblem(t, err, 409, "worker_release_inactive")

	_, err = fixture.service.StartCanary(
		fixture.ctx, fixture.principal, fixture.tenantID, fixture.targetID, second.ID,
		PolicyChangeInput{ExpectedPolicyVersion: 2, CanaryPercent: 10, Reason: "stale operator view"},
		"stale-canary", "request-stale", "127.0.0.1",
	)
	assertProblem(t, err, 409, "worker_release_policy_version_conflict")

	var transitionCount, auditCount, outboxCount int64
	if err := fixture.db.Model(&persistence.WorkerReleaseTransition{}).
		Where("tenant_id = ? AND execution_target_id = ?", fixture.tenantID, fixture.targetID).
		Count(&transitionCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.AuditLog{}).
		Where("tenant_id = ? AND action LIKE ?", fixture.tenantID, "worker_release.%").Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.OutboxMessage{}).
		Where("tenant_id = ? AND topic LIKE ?", fixture.tenantID, "worker.release.%").Count(&outboxCount).Error; err != nil {
		t.Fatal(err)
	}
	if transitionCount != 4 || auditCount != 6 || outboxCount != 6 {
		t.Fatalf("transition/audit/outbox counts = %d/%d/%d, want 4/6/6", transitionCount, auditCount, outboxCount)
	}

	if err := fixture.db.Model(&persistence.WorkerReleaseRevision{}).
		Where("id = ?", first.ID).Update("description", "mutated").Error; err == nil {
		t.Fatal("SQLite safety accepted mutation of an immutable Worker release revision")
	}
	if err := fixture.db.Model(&persistence.WorkerReleaseTransition{}).
		Where("execution_target_id = ? AND policy_version = ?", fixture.targetID, 4).
		Update("reason", "mutated").Error; err == nil {
		t.Fatal("SQLite safety accepted mutation of immutable Worker release history")
	}
}

func TestWorkerReleaseRequiresCanaryBeforeSubsequentPromotion(t *testing.T) {
	fixture := newReleaseFixture(t)
	first := fixture.createRevision(t, fixture.firstManifestID, "initial", "release-first")
	second := fixture.createRevision(t, fixture.secondManifestID, "next", "release-second")
	fixture.changePolicy(t, "promote", first.ID, PolicyChangeInput{
		ExpectedPolicyVersion: 0, Reason: "initial promotion",
	}, "promote-first")

	_, err := fixture.service.Promote(
		fixture.ctx, fixture.principal, fixture.tenantID, fixture.targetID, second.ID,
		PolicyChangeInput{ExpectedPolicyVersion: 1, Reason: "skip canary"},
		"skip-canary", "request-skip", "127.0.0.1",
	)
	assertProblem(t, err, 409, "worker_release_revision_not_canary")
}

func TestInitialWorkerReleaseWaitsForActiveUnmanagedExecutions(t *testing.T) {
	for _, status := range []string{"running", "recovering"} {
		t.Run(status, func(t *testing.T) {
			fixture := newReleaseFixture(t)
			first := fixture.createRevision(t, fixture.firstManifestID, "initial", "initial-active-first")
			executionID := fixture.seedQueuedExecution(t, first.ID, ChannelPromoted)
			if err := fixture.db.Model(&persistence.AgentExecution{}).Where("id = ?", executionID).
				Updates(map[string]any{
					"status": status, "worker_release_revision_id": nil, "worker_release_channel": nil,
				}).Error; err != nil {
				t.Fatal(err)
			}

			_, err := fixture.service.Promote(
				fixture.ctx, fixture.principal, fixture.tenantID, fixture.targetID, first.ID,
				PolicyChangeInput{ExpectedPolicyVersion: 0, Reason: "do not strand unmanaged work"},
				"initial-active-promote", "request-initial-active-promote", "127.0.0.1",
			)
			assertProblem(t, err, 409, "worker_release_active_executions")

			if err := fixture.db.Model(&persistence.AgentExecution{}).Where("id = ?", executionID).
				Update("status", "queued").Error; err != nil {
				t.Fatal(err)
			}
			promoted := fixture.changePolicy(t, "promote", first.ID, PolicyChangeInput{
				ExpectedPolicyVersion: 0, Reason: "unmanaged execution returned to the queue",
			}, "initial-active-promote-after-drain")
			assertPolicy(t, promoted, 1, first.ID, nil, 0)
			fixture.assertExecutionRelease(t, executionID, first.ID, ChannelPromoted)
		})
	}
}

func TestWorkerReleasePromotionWaitsForActiveExecutionsOnTheOldChannel(t *testing.T) {
	for _, status := range []string{"running", "recovering"} {
		t.Run(status, func(t *testing.T) {
			fixture := newReleaseFixture(t)
			first := fixture.createRevision(t, fixture.firstManifestID, "initial", "active-release-first")
			second := fixture.createRevision(t, fixture.secondManifestID, "next", "active-release-second")
			fixture.changePolicy(t, "promote", first.ID, PolicyChangeInput{
				ExpectedPolicyVersion: 0, Reason: "initial promotion",
			}, "active-promote-first")
			fixture.changePolicy(t, "canary", second.ID, PolicyChangeInput{
				ExpectedPolicyVersion: 1, CanaryPercent: 100, Reason: "start canary",
			}, "active-canary-second")
			executionID := fixture.seedQueuedExecution(t, second.ID, ChannelCanary)
			if err := fixture.db.Model(&persistence.AgentExecution{}).Where("id = ?", executionID).
				Update("status", status).Error; err != nil {
				t.Fatal(err)
			}

			_, err := fixture.service.Promote(
				fixture.ctx, fixture.principal, fixture.tenantID, fixture.targetID, second.ID,
				PolicyChangeInput{ExpectedPolicyVersion: 2, Reason: "do not strand the active canary"},
				"active-promote-second", "request-active-promote-second", "127.0.0.1",
			)
			assertProblem(t, err, 409, "worker_release_active_executions")

			if err := fixture.db.Model(&persistence.AgentExecution{}).Where("id = ?", executionID).
				Update("status", "completed").Error; err != nil {
				t.Fatal(err)
			}
			promoted := fixture.changePolicy(t, "promote", second.ID, PolicyChangeInput{
				ExpectedPolicyVersion: 2, Reason: "canary execution finished",
			}, "active-promote-second-after-drain")
			assertPolicy(t, promoted, 3, second.ID, nil, 0)
		})
	}
}

func TestWorkerReleaseRollbackWaitsForRecoveringCanaryExecution(t *testing.T) {
	fixture := newReleaseFixture(t)
	first := fixture.createRevision(t, fixture.firstManifestID, "initial", "recovering-rollback-first")
	second := fixture.createRevision(t, fixture.secondManifestID, "candidate", "recovering-rollback-second")
	fixture.changePolicy(t, "promote", first.ID, PolicyChangeInput{
		ExpectedPolicyVersion: 0, Reason: "initial promotion",
	}, "recovering-rollback-promote")
	fixture.changePolicy(t, "canary", second.ID, PolicyChangeInput{
		ExpectedPolicyVersion: 1, CanaryPercent: 100, Reason: "start canary",
	}, "recovering-rollback-canary")
	executionID := fixture.seedQueuedExecution(t, second.ID, ChannelCanary)
	if err := fixture.db.Model(&persistence.AgentExecution{}).Where("id = ?", executionID).
		Update("status", "recovering").Error; err != nil {
		t.Fatal(err)
	}

	_, err := fixture.service.Rollback(
		fixture.ctx, fixture.principal, fixture.tenantID, fixture.targetID, first.ID,
		PolicyChangeInput{ExpectedPolicyVersion: 2, Reason: "do not rewrite recovering canary work"},
		"recovering-rollback-blocked", "request-recovering-rollback-blocked", "127.0.0.1",
	)
	assertProblem(t, err, 409, "worker_release_active_executions")

	if err := fixture.db.Model(&persistence.AgentExecution{}).Where("id = ?", executionID).
		Update("status", "queued").Error; err != nil {
		t.Fatal(err)
	}
	rolledBack := fixture.changePolicy(t, "rollback", first.ID, PolicyChangeInput{
		ExpectedPolicyVersion: 2, Reason: "queued canary can be reassigned",
	}, "recovering-rollback-after-queue")
	assertPolicy(t, rolledBack, 3, first.ID, nil, 0)
	fixture.assertExecutionRelease(t, executionID, first.ID, ChannelPromoted)
}

func TestWorkerReleaseRollbackToPromotedRevisionAbortsCanary(t *testing.T) {
	fixture := newReleaseFixture(t)
	first := fixture.createRevision(t, fixture.firstManifestID, "initial", "abort-release-first")
	second := fixture.createRevision(t, fixture.secondManifestID, "candidate", "abort-release-second")
	fixture.changePolicy(t, "promote", first.ID, PolicyChangeInput{
		ExpectedPolicyVersion: 0, Reason: "initial promotion",
	}, "abort-promote-first")
	fixture.changePolicy(t, "canary", second.ID, PolicyChangeInput{
		ExpectedPolicyVersion: 1, CanaryPercent: 25, Reason: "start candidate",
	}, "abort-canary-second")
	executionID := fixture.seedQueuedExecution(t, second.ID, ChannelCanary)

	aborted := fixture.changePolicy(t, "rollback", first.ID, PolicyChangeInput{
		ExpectedPolicyVersion: 2, Reason: "candidate health gate failed",
	}, "abort-canary")
	assertPolicy(t, aborted, 3, first.ID, nil, 0)
	fixture.assertExecutionRelease(t, executionID, first.ID, ChannelPromoted)
	fixture.assertWorkerRelease(t, fixture.firstWorkerID, "active", first.ID, ChannelPromoted)
	fixture.assertWorkerRelease(t, fixture.secondWorkerID, "inactive", second.ID, "")

	overview, err := fixture.service.List(fixture.ctx, fixture.principal, fixture.tenantID, fixture.targetID)
	if err != nil {
		t.Fatal(err)
	}
	if len(overview.Transitions) == 0 || overview.Transitions[0].Action != "abort-canary" {
		t.Fatalf("latest transition = %#v, want abort-canary", overview.Transitions)
	}
	var auditCount, outboxCount int64
	if err := fixture.db.Model(&persistence.AuditLog{}).
		Where("tenant_id = ? AND action = ?", fixture.tenantID, "worker_release.canary_aborted").
		Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.OutboxMessage{}).
		Where("tenant_id = ? AND topic = ?", fixture.tenantID, "worker.release.canary-aborted").
		Count(&outboxCount).Error; err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 || outboxCount != 1 {
		t.Fatalf("abort canary audit/outbox counts = %d/%d, want 1/1", auditCount, outboxCount)
	}
}

func TestManagedWorkerReleaseRollbackDoesNotRequireAnOnlineOldRevision(t *testing.T) {
	fixture := newReleaseFixture(t)
	firstDigest := "sha256:" + releaseDigest("managed-first")
	secondDigest := "sha256:" + releaseDigest("managed-second")
	if err := fixture.db.Model(&persistence.ExecutionTarget{}).Where("id = ?", fixture.targetID).
		Update("kind", "docker").Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.WorkerManifest{}).Where("id = ?", fixture.firstManifestID).
		Update("image_digest", firstDigest).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.WorkerManifest{}).Where("id = ?", fixture.secondManifestID).
		Update("image_digest", secondDigest).Error; err != nil {
		t.Fatal(err)
	}
	first := fixture.createRevision(t, fixture.firstManifestID, "managed baseline", "managed-first")
	second := fixture.createRevision(t, fixture.secondManifestID, "managed candidate", "managed-second")
	fixture.changePolicy(t, "promote", first.ID, PolicyChangeInput{
		ExpectedPolicyVersion: 0, Reason: "initial managed release",
	}, "managed-promote-first")
	fixture.changePolicy(t, "canary", second.ID, PolicyChangeInput{
		ExpectedPolicyVersion: 1, CanaryPercent: 100, Reason: "managed canary",
	}, "managed-canary-second")
	fixture.changePolicy(t, "promote", second.ID, PolicyChangeInput{
		ExpectedPolicyVersion: 2, Reason: "managed promotion",
	}, "managed-promote-second")
	if err := fixture.db.Model(&persistence.WorkerInstance{}).Where("id = ?", fixture.firstWorkerID).
		Update("status", "offline").Error; err != nil {
		t.Fatal(err)
	}

	rolledBack := fixture.changePolicy(t, "rollback", first.ID, PolicyChangeInput{
		ExpectedPolicyVersion: 3, Reason: "restore old immutable digest",
	}, "managed-rollback-first")
	assertPolicy(t, rolledBack, 4, first.ID, nil, 0)
}

func TestWorkerReleaseManagedTargetRequiresImageDigest(t *testing.T) {
	fixture := newReleaseFixture(t)
	if err := fixture.db.Model(&persistence.ExecutionTarget{}).Where("id = ?", fixture.targetID).
		Update("kind", "docker").Error; err != nil {
		t.Fatal(err)
	}
	_, err := fixture.service.CreateRevision(
		fixture.ctx, fixture.principal, fixture.tenantID, fixture.targetID,
		CreateRevisionInput{WorkerManifestID: fixture.firstManifestID, Description: "missing digest"},
		"managed-release-missing-digest", "request-managed-release-missing-digest", "127.0.0.1",
	)
	assertProblem(t, err, 409, "worker_release_image_digest_required")
}

func TestManagedWorkerReleaseCanRegisterAnImmutableManifestObservedElsewhere(t *testing.T) {
	fixture := newReleaseFixture(t)
	digest := "sha256:" + releaseDigest("managed-cross-target")
	stagingTargetID := uuid.New()
	if err := fixture.db.Create(&persistence.ExecutionTarget{
		ID: stagingTargetID, TenantID: &fixture.tenantID, OrganizationID: &fixture.organizationID,
		Kind: "docker", Name: "managed staging", Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{},
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.ExecutionTarget{}).Where("id = ?", fixture.targetID).
		Update("kind", "kubernetes").Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.WorkerManifest{}).Where("id = ?", fixture.firstManifestID).
		Update("image_digest", digest).Error; err != nil {
		t.Fatal(err)
	}
	var stagingWorker persistence.WorkerInstance
	if err := fixture.db.Where("id = ?", fixture.firstWorkerID).Take(&stagingWorker).Error; err != nil {
		t.Fatal(err)
	}
	stagingWorker.ID = uuid.New()
	stagingWorker.ExecutionTargetID = stagingTargetID
	stagingWorker.InstanceUID = uuid.NewString()
	stagingWorker.ClusterID = "managed-staging"
	stagingWorker.PodName = "managed-staging-worker"
	stagingWorker.AuthTokenHash = []byte(uuid.NewString())
	if err := fixture.db.Create(&stagingWorker).Error; err != nil {
		t.Fatal(err)
	}

	revision := fixture.createRevision(
		t,
		fixture.firstManifestID,
		"immutable manifest promoted from a staging target",
		"managed-cross-target",
	)
	if revision.ImageDigest == nil || *revision.ImageDigest != digest {
		t.Fatalf("managed cross-target revision digest = %v, want %s", revision.ImageDigest, digest)
	}
}

func TestManagedWorkerReleaseHidesAnotherTenantsManifestBeforeDigestValidation(t *testing.T) {
	fixture := newReleaseFixture(t)
	if err := fixture.db.Model(&persistence.ExecutionTarget{}).Where("id = ?", fixture.targetID).
		Update("kind", "docker").Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	otherTenantID, otherOrganizationID, otherTargetID, otherManifestID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	models := []any{
		&persistence.Tenant{
			ID: otherTenantID, Slug: "other-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12],
			Name: "Other tenant", Status: "active", PlanCode: "free", Region: "default",
			Settings: map[string]any{}, CreatedBy: fixture.userID,
		},
		&persistence.Organization{
			ID: otherOrganizationID, TenantID: otherTenantID, Slug: "root", Name: "Other root",
			Kind: "root", Status: "active", Settings: map[string]any{}, CreatedBy: fixture.userID,
		},
		&persistence.ExecutionTarget{
			ID: otherTargetID, TenantID: &otherTenantID, OrganizationID: &otherOrganizationID,
			Kind: "docker", Name: "other managed target", Status: "active",
			ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{},
		},
		&persistence.WorkerManifest{
			ID: otherManifestID, ManifestHash: releaseDigest("other-tenant-manifest"), WorkerBuildVersion: "other-v1",
			WorkerProtocolMinimum: 2, WorkerProtocolMaximum: 2,
			RuntimeEventMinimum: 2, RuntimeEventMaximum: 2,
			OperatingSystem: "linux", Architecture: "amd64", FeatureFlags: map[string]any{}, CreatedAt: now,
		},
	}
	for _, model := range models {
		if err := fixture.db.Create(model).Error; err != nil {
			t.Fatal(err)
		}
	}
	var otherWorker persistence.WorkerInstance
	if err := fixture.db.Where("id = ?", fixture.firstWorkerID).Take(&otherWorker).Error; err != nil {
		t.Fatal(err)
	}
	otherWorker.ID = uuid.New()
	otherWorker.ExecutionTargetID = otherTargetID
	otherWorker.InstanceUID = uuid.NewString()
	otherWorker.ClusterID = "other-tenant"
	otherWorker.PodName = "other-tenant-worker"
	otherWorker.CurrentManifestID = &otherManifestID
	otherWorker.AuthTokenHash = []byte(uuid.NewString())
	if err := fixture.db.Create(&otherWorker).Error; err != nil {
		t.Fatal(err)
	}

	_, err := fixture.service.CreateRevision(
		fixture.ctx, fixture.principal, fixture.tenantID, fixture.targetID,
		CreateRevisionInput{WorkerManifestID: otherManifestID, Description: "must stay hidden"},
		"cross-tenant-manifest", "request-cross-tenant-manifest", "127.0.0.1",
	)
	assertProblem(t, err, 404, "worker_manifest_not_found")
}

type releaseFixture struct {
	ctx              context.Context
	db               *gorm.DB
	service          *Service
	principal        identity.Principal
	tenantID         uuid.UUID
	organizationID   uuid.UUID
	targetID         uuid.UUID
	userID           uuid.UUID
	firstManifestID  uuid.UUID
	secondManifestID uuid.UUID
	firstWorkerID    uuid.UUID
	secondWorkerID   uuid.UUID
}

func newReleaseFixture(t *testing.T) releaseFixture {
	t.Helper()
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "worker-release-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	firstManifestID, secondManifestID := uuid.New(), uuid.New()
	firstWorkerID, secondWorkerID := uuid.New(), uuid.New()
	models := []any{
		&persistence.WorkerManifest{
			ID: firstManifestID, ManifestHash: releaseDigest("first"), WorkerBuildVersion: "worker-v1",
			WorkerProtocolMinimum: 2, WorkerProtocolMaximum: 2, RuntimeEventMinimum: 2, RuntimeEventMaximum: 2,
			OperatingSystem: "linux", Architecture: "amd64", FeatureFlags: map[string]any{}, CreatedAt: now,
		},
		&persistence.WorkerManifest{
			ID: secondManifestID, ManifestHash: releaseDigest("second"), WorkerBuildVersion: "worker-v2",
			WorkerProtocolMinimum: 2, WorkerProtocolMaximum: 2, RuntimeEventMinimum: 2, RuntimeEventMaximum: 2,
			OperatingSystem: "linux", Architecture: "amd64", FeatureFlags: map[string]any{}, CreatedAt: now,
		},
		&persistedReleaseWorker{
			WorkerInstance: persistence.WorkerInstance{
				ID: firstWorkerID, Incarnation: 1, InstanceUID: uuid.NewString(), ExecutionTargetID: domain.ExecutionTargetID,
				TargetKind: "local", ClusterID: "release", Namespace: "default", PodName: "worker-v1",
				Version: "worker-v1", ProtocolVersion: 2, Capabilities: map[string]any{},
				CurrentManifestID: &firstManifestID, CompatibilityStatus: "compatible", CompatibilityCheckedAt: &now,
				LeaseSupported: true, FencingSupported: true, AuthTokenHash: []byte(uuid.NewString()),
				Status: "online", AdministrativeStatus: "active", RegisteredAt: now, LastHeartbeatAt: now,
			},
		},
		&persistedReleaseWorker{
			WorkerInstance: persistence.WorkerInstance{
				ID: secondWorkerID, Incarnation: 1, InstanceUID: uuid.NewString(), ExecutionTargetID: domain.ExecutionTargetID,
				TargetKind: "local", ClusterID: "release", Namespace: "default", PodName: "worker-v2",
				Version: "worker-v2", ProtocolVersion: 2, Capabilities: map[string]any{},
				CurrentManifestID: &secondManifestID, CompatibilityStatus: "compatible", CompatibilityCheckedAt: &now,
				LeaseSupported: true, FencingSupported: true, AuthTokenHash: []byte(uuid.NewString()),
				Status: "online", AdministrativeStatus: "active", RegisteredAt: now, LastHeartbeatAt: now,
			},
		},
	}
	for _, model := range models {
		switch value := model.(type) {
		case *persistedReleaseWorker:
			if err := store.DB().Create(&value.WorkerInstance).Error; err != nil {
				t.Fatal(err)
			}
		default:
			if err := store.DB().Create(model).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	activeTenantID := domain.TenantID
	return releaseFixture{
		ctx: ctx, db: store.DB(), service: NewService(store.DB()),
		principal: identity.Principal{UserID: domain.UserID, ActiveTenantID: &activeTenantID},
		tenantID:  domain.TenantID, organizationID: domain.OrganizationID,
		targetID: domain.ExecutionTargetID, userID: domain.UserID,
		firstManifestID: firstManifestID, secondManifestID: secondManifestID,
		firstWorkerID: firstWorkerID, secondWorkerID: secondWorkerID,
	}
}

type persistedReleaseWorker struct {
	persistence.WorkerInstance
}

func (f releaseFixture) createRevision(
	t *testing.T,
	manifestID uuid.UUID,
	description, idempotencyKey string,
) Revision {
	t.Helper()
	result, err := f.service.CreateRevision(
		f.ctx, f.principal, f.tenantID, f.targetID,
		CreateRevisionInput{WorkerManifestID: manifestID, Description: description},
		idempotencyKey, "request-"+idempotencyKey, "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	return result.Value
}

func (f releaseFixture) changePolicy(
	t *testing.T,
	action string,
	revisionID uuid.UUID,
	input PolicyChangeInput,
	idempotencyKey string,
) Policy {
	t.Helper()
	var (
		result OperationResult[Policy]
		err    error
	)
	switch action {
	case "promote":
		result, err = f.service.Promote(f.ctx, f.principal, f.tenantID, f.targetID, revisionID, input, idempotencyKey, "request-"+idempotencyKey, "127.0.0.1")
	case "canary":
		result, err = f.service.StartCanary(f.ctx, f.principal, f.tenantID, f.targetID, revisionID, input, idempotencyKey, "request-"+idempotencyKey, "127.0.0.1")
	case "rollback":
		result, err = f.service.Rollback(f.ctx, f.principal, f.tenantID, f.targetID, revisionID, input, idempotencyKey, "request-"+idempotencyKey, "127.0.0.1")
	}
	if err != nil {
		t.Fatal(err)
	}
	return result.Value
}

func (f releaseFixture) assertWorkerRelease(
	t *testing.T,
	workerID uuid.UUID,
	status string,
	revisionID uuid.UUID,
	channel string,
) {
	t.Helper()
	var worker persistence.WorkerInstance
	if err := f.db.Where("id = ?", workerID).Take(&worker).Error; err != nil {
		t.Fatal(err)
	}
	if worker.WorkerReleaseStatus != status || worker.WorkerReleaseRevisionID == nil ||
		*worker.WorkerReleaseRevisionID != revisionID {
		t.Fatalf("Worker release status/revision = %q/%v, want %q/%s", worker.WorkerReleaseStatus, worker.WorkerReleaseRevisionID, status, revisionID)
	}
	if channel == "" {
		if worker.WorkerReleaseChannel != nil || worker.WorkerReleaseReason == nil {
			t.Fatalf("inactive Worker release channel/reason = %v/%v", worker.WorkerReleaseChannel, worker.WorkerReleaseReason)
		}
	} else if worker.WorkerReleaseChannel == nil || *worker.WorkerReleaseChannel != channel || worker.WorkerReleaseReason != nil {
		t.Fatalf("active Worker release channel/reason = %v/%v, want %q/nil", worker.WorkerReleaseChannel, worker.WorkerReleaseReason, channel)
	}
}

func (f releaseFixture) seedQueuedExecution(
	t *testing.T,
	revisionID uuid.UUID,
	channel string,
) uuid.UUID {
	t.Helper()
	now := time.Now().UTC()
	projectID, sessionID, turnID, executionID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	models := []any{
		&persistence.Project{
			ID: projectID, TenantID: f.tenantID, OrganizationID: f.organizationID,
			Name: "Release project", DefaultBranch: "main", Visibility: "organization", CreatedBy: f.userID,
		},
		&persistence.AgentSession{
			ID: sessionID, TenantID: f.tenantID, OrganizationID: f.organizationID, ProjectID: projectID,
			CreatedBy: f.userID, Title: "Release session", Status: "active", Visibility: "private",
			Provider: "codex", ExecutionTargetID: f.targetID,
		},
		&persistence.AgentTurn{
			ID: turnID, TenantID: f.tenantID, SessionID: sessionID, CreatedBy: f.userID,
			Status: "queued", InputText: "release test", TurnKind: "message",
			RuntimeMode: "approval-required", InteractionMode: "default", CreatedAt: now,
		},
		&persistence.AgentExecution{
			ID: executionID, TenantID: f.tenantID, SessionID: sessionID, TurnID: turnID,
			Attempt: 1, Status: "queued", ExecutionTargetID: f.targetID, TargetKind: "local",
			WorkerReleaseRevisionID: &revisionID, WorkerReleaseChannel: &channel,
			Generation: 0, RequestedBy: f.userID, QueuedAt: now,
		},
	}
	if err := f.db.Transaction(func(tx *gorm.DB) error {
		for _, model := range models {
			if err := tx.Create(model).Error; err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return executionID
}

func (f releaseFixture) assertExecutionRelease(t *testing.T, executionID, revisionID uuid.UUID, channel string) {
	t.Helper()
	var execution persistence.AgentExecution
	if err := f.db.Where("id = ?", executionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.WorkerReleaseRevisionID == nil || *execution.WorkerReleaseRevisionID != revisionID ||
		execution.WorkerReleaseChannel == nil || *execution.WorkerReleaseChannel != channel {
		t.Fatalf("Execution release = %v/%v, want %s/%s", execution.WorkerReleaseRevisionID, execution.WorkerReleaseChannel, revisionID, channel)
	}
}

func assertPolicy(t *testing.T, policy Policy, version int64, promoted uuid.UUID, canary *uuid.UUID, percent int) {
	t.Helper()
	if policy.PolicyVersion != version || policy.PromotedRevisionID != promoted || policy.CanaryPercent != percent {
		t.Fatalf("policy = %#v", policy)
	}
	if canary == nil {
		if policy.CanaryRevisionID != nil {
			t.Fatalf("policy canary = %v, want nil", policy.CanaryRevisionID)
		}
	} else if policy.CanaryRevisionID == nil || *policy.CanaryRevisionID != *canary {
		t.Fatalf("policy canary = %v, want %s", policy.CanaryRevisionID, *canary)
	}
}

func assertProblem(t *testing.T, err error, status int, code string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Status != status || apiError.Code != code {
		t.Fatalf("error = %#v, want %d/%s", err, status, code)
	}
}

func releaseDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
