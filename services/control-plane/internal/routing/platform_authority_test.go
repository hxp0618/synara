package routing

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPlatformAuthorityPublicationIsSignedScopedAtomicAndReplayable(t *testing.T) {
	fixture := newPlatformAuthorityFixture(t)
	publication := fixture.publication(t, fixture.now.Add(-time.Second))

	result, err := fixture.service.Publish(context.Background(), fixture.targetID, publication)
	if err != nil {
		t.Fatal(err)
	}
	if result.Replayed || result.ExecutionTargetID != fixture.targetID || result.TenantID != nil ||
		result.Health == nil || result.Health.Status != HealthHealthy || result.Health.Version != 1 ||
		len(result.DRReadiness) != 1 || result.DRReadiness[0].Version != 1 ||
		!result.DRReadiness[0].ArtifactsReady || !result.DRReadiness[0].CheckpointsReady ||
		!result.DRReadiness[0].MemoryReady {
		t.Fatalf("publication result = %#v", result)
	}

	fixture.service.now = func() time.Time { return fixture.now.Add(2 * time.Hour) }
	replayed, err := fixture.service.Publish(context.Background(), fixture.targetID, publication)
	if err != nil {
		t.Fatal(err)
	}
	if !replayed.Replayed || replayed.Health == nil || replayed.Health.Version != 1 ||
		len(replayed.DRReadiness) != 1 || replayed.DRReadiness[0].Version != 1 {
		t.Fatalf("replayed result = %#v", replayed)
	}

	var health persistence.ExecutionTargetHealth
	if err := fixture.db.Where("execution_target_id = ?", fixture.targetID).Take(&health).Error; err != nil {
		t.Fatal(err)
	}
	var readiness persistence.ExecutionTargetDRReadiness
	if err := fixture.db.Where(
		"execution_target_id = ? AND source_dr_domain = ?", fixture.targetID, "region-a/cluster-a",
	).Take(&readiness).Error; err != nil {
		t.Fatal(err)
	}
	var receipts int64
	if err := fixture.db.Model(&persistence.PlatformRoutingPublication{}).Count(&receipts).Error; err != nil {
		t.Fatal(err)
	}
	if health.Version != 1 || readiness.Version != 1 || receipts != 1 ||
		health.Source != fixture.publisherIdentity || readiness.PublisherIdentity != fixture.publisherIdentity {
		t.Fatalf("health=%#v readiness=%#v receipts=%d", health, readiness, receipts)
	}

	conflict := publication
	conflict.Health = &PlatformAuthorityHealth{
		Status: HealthUnreachable, CapacityStatus: CapacityUnknown,
		AllocatedCapacityUnits: 3, TTLSeconds: 120,
	}
	conflict, err = SignPlatformAuthorityPublication(fixture.privateKey, conflict)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.Publish(context.Background(), fixture.targetID, conflict); codeOf(err) != "platform_routing_publication_nonce_conflict" {
		t.Fatalf("nonce conflict err = %v", err)
	}

	if err := fixture.db.Model(&persistence.PlatformRoutingPublication{}).
		Where("publisher_identity = ?", fixture.publisherIdentity).
		Update("response_sha256", platformAuthorityTestDigest('f')).Error; err == nil {
		t.Fatal("immutable publication receipt accepted an update")
	}
	if err := fixture.db.Where("publisher_identity = ?", fixture.publisherIdentity).
		Delete(&persistence.PlatformRoutingPublication{}).Error; err == nil {
		t.Fatal("immutable publication receipt accepted a delete")
	}
}

func TestPlatformAuthorityPublicationRollsBackWholeBundleWhenOneAuthorityIsStale(t *testing.T) {
	fixture := newPlatformAuthorityFixture(t)
	routingService := NewService(fixture.db)
	seedHealthAt := fixture.now.Add(-3 * time.Minute)
	health, err := routingService.ObserveHealth(context.Background(), HealthObservation{
		ExecutionTargetID: fixture.targetID, Status: HealthDegraded, CapacityStatus: CapacityAvailable,
		AvailableCapacityUnits: intPointer(4), AllocatedCapacityUnits: 1, Source: "seed-health",
		ObservedAt: seedHealthAt, TTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	seedReadinessAt := fixture.now.Add(-30 * time.Second)
	readiness, err := routingService.ObserveDRReadiness(context.Background(), DRReadinessObservation{
		ExecutionTargetID: fixture.targetID, SourceDRDomain: "region-a/cluster-a",
		DRDomain: "region-b/cluster-b", ReplicatedThroughAt: fixture.now.Add(-2 * time.Minute),
		ArtifactsReady: true, PublisherIdentity: "seed-readiness", ObservedAt: seedReadinessAt, TTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	publication := fixture.publication(t, fixture.now.Add(-time.Minute))
	if _, err := fixture.service.Publish(context.Background(), fixture.targetID, publication); codeOf(err) != "target_dr_readiness_observation_stale" {
		t.Fatalf("stale bundle err = %v", err)
	}
	var currentHealth persistence.ExecutionTargetHealth
	if err := fixture.db.Where("execution_target_id = ?", fixture.targetID).Take(&currentHealth).Error; err != nil {
		t.Fatal(err)
	}
	var currentReadiness persistence.ExecutionTargetDRReadiness
	if err := fixture.db.Where(
		"execution_target_id = ? AND source_dr_domain = ?", fixture.targetID, "region-a/cluster-a",
	).Take(&currentReadiness).Error; err != nil {
		t.Fatal(err)
	}
	var receipts int64
	if err := fixture.db.Model(&persistence.PlatformRoutingPublication{}).Count(&receipts).Error; err != nil {
		t.Fatal(err)
	}
	if currentHealth.Version != health.Version || currentHealth.Source != health.Source ||
		currentReadiness.Version != readiness.Version || currentReadiness.PublisherIdentity != readiness.PublisherIdentity ||
		receipts != 0 {
		t.Fatalf("partial bundle committed: health=%#v readiness=%#v receipts=%d", currentHealth, currentReadiness, receipts)
	}
}

func TestPlatformAuthorityPublicationRejectsBadSignatureAndRuntimeOwnershipMismatch(t *testing.T) {
	fixture := newPlatformAuthorityFixture(t)
	publication := fixture.publication(t, fixture.now.Add(-time.Second))
	publication.Signature = platformAuthorityTestDigest('a')
	if _, err := fixture.service.Publish(context.Background(), fixture.targetID, publication); codeOf(err) != "platform_routing_publisher_authentication_failed" {
		t.Fatalf("bad signature err = %v", err)
	}

	service, err := NewPlatformAuthorityService(fixture.db, []PlatformAuthorityPublisherConfig{{
		PublisherIdentity: fixture.publisherIdentity,
		Keys:              []PlatformAuthorityPublisherKey{{KeyID: fixture.keyID, PublicKey: fixture.privateKey.Public().(ed25519.PublicKey)}},
		Targets: []PlatformAuthorityTargetScope{{
			ExecutionTargetID: fixture.ownedTargetID,
			Ownership:         PlatformAuthorityOwnershipShared,
			PublishHealth:     true,
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return fixture.now }
	publication = fixture.publicationForTarget(t, fixture.ownedTargetID, fixture.now.Add(-time.Second))
	publication.DRReadiness = nil
	publication, err = SignPlatformAuthorityPublication(fixture.privateKey, publication)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Publish(context.Background(), fixture.ownedTargetID, publication); codeOf(err) != "platform_routing_publisher_scope_denied" {
		t.Fatalf("ownership mismatch err = %v", err)
	}
}

func TestPlatformAuthorityPublicationCommitsScopedReservationAuthority(t *testing.T) {
	fixture := newPlatformAuthorityFixture(t)
	publication := fixture.publication(t, fixture.now.Add(-time.Second))
	publication.DRReadiness = nil
	publication.Health.ReservationAuthority = &ReservationAuthorityObservation{
		Mode:             ReservationAuthorityExactActiveV1,
		Acknowledgements: []ReservationIdentity{},
	}
	var err error
	publication, err = SignPlatformAuthorityPublication(fixture.privateKey, publication)
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.service.Publish(context.Background(), fixture.targetID, publication)
	if err != nil {
		t.Fatal(err)
	}
	if result.Health == nil || result.Health.ReservationAuthority == nil ||
		result.Health.ReservationAuthority.Mode != ReservationAuthorityExactActiveV1 ||
		result.Health.ReservationAuthority.AcknowledgedUnits != 0 ||
		len(result.Health.ReservationAuthority.AcknowledgementsSHA256) != 64 {
		t.Fatalf("reservation authority result = %#v", result)
	}
}

func TestNormalizePlatformAuthorityPublisherConfigsRejectsOverlappingAuthorities(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	targetID := uuid.New()
	baseTarget := PlatformAuthorityTargetScope{
		ExecutionTargetID: targetID, Ownership: PlatformAuthorityOwnershipShared, PublishHealth: true,
		DRRoutes: []PlatformAuthorityDRRouteScope{{SourceDRDomain: "source/domain", DRDomain: "destination/domain"}},
	}
	_, err = NormalizePlatformAuthorityPublisherConfigs([]PlatformAuthorityPublisherConfig{
		{PublisherIdentity: "publisher-a", Keys: []PlatformAuthorityPublisherKey{{KeyID: "key-a", PublicKey: publicKey}}, Targets: []PlatformAuthorityTargetScope{baseTarget}},
		{PublisherIdentity: "publisher-b", Keys: []PlatformAuthorityPublisherKey{{KeyID: "key-b", PublicKey: publicKey}}, Targets: []PlatformAuthorityTargetScope{baseTarget}},
	})
	if err == nil {
		t.Fatal("overlapping health/DR authorities were accepted")
	}
}

type platformAuthorityFixture struct {
	db                *gorm.DB
	service           *PlatformAuthorityService
	privateKey        ed25519.PrivateKey
	targetID          uuid.UUID
	ownedTargetID     uuid.UUID
	publisherIdentity string
	keyID             string
	now               time.Time
}

func newPlatformAuthorityFixture(t *testing.T) platformAuthorityFixture {
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
	domain, err := bootstrap.Ensure(
		ctx, store.DB(), platform.ProfilePersonal, "platform-authority-owner-"+uuid.NewString(),
	)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	targetID := uuid.New()
	if err := store.DB().Create(&persistence.ExecutionTarget{
		ID: targetID, Kind: "kubernetes", Name: "platform-routing-" + uuid.NewString(), Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const publisherIdentity = "platform-routing-test-publisher"
	const keyID = "platform-routing-test-key-v1"
	service, err := NewPlatformAuthorityService(store.DB(), []PlatformAuthorityPublisherConfig{{
		PublisherIdentity: publisherIdentity,
		Keys:              []PlatformAuthorityPublisherKey{{KeyID: keyID, PublicKey: publicKey}},
		Targets: []PlatformAuthorityTargetScope{{
			ExecutionTargetID:   targetID,
			Ownership:           PlatformAuthorityOwnershipShared,
			PublishHealth:       true,
			PublishReservations: true,
			DRRoutes: []PlatformAuthorityDRRouteScope{{
				SourceDRDomain: "region-a/cluster-a", DRDomain: "region-b/cluster-b",
			}},
		}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	return platformAuthorityFixture{
		db: store.DB(), service: service, privateKey: privateKey, targetID: targetID,
		ownedTargetID:     domain.ExecutionTargetID,
		publisherIdentity: publisherIdentity, keyID: keyID, now: now,
	}
}

func (f platformAuthorityFixture) publication(t *testing.T, observedAt time.Time) PlatformAuthorityPublication {
	t.Helper()
	return f.publicationForTarget(t, f.targetID, observedAt)
}

func (f platformAuthorityFixture) publicationForTarget(
	t *testing.T,
	targetID uuid.UUID,
	observedAt time.Time,
) PlatformAuthorityPublication {
	t.Helper()
	publication := PlatformAuthorityPublication{
		SchemaVersion:     PlatformAuthoritySchemaVersionV1,
		PublisherIdentity: f.publisherIdentity,
		KeyID:             f.keyID,
		Nonce:             uuid.NewString(),
		IssuedAt:          f.now.Add(-time.Second).Format(time.RFC3339Nano),
		ExpiresAt:         f.now.Add(time.Minute).Format(time.RFC3339Nano),
		ExecutionTargetID: targetID.String(),
		ObservedAt:        observedAt.UTC().Format(time.RFC3339Nano),
		Health: &PlatformAuthorityHealth{
			Status: HealthHealthy, CapacityStatus: CapacityAvailable,
			AvailableCapacityUnits: intPointer(10), AllocatedCapacityUnits: 2, TTLSeconds: 600,
		},
		DRReadiness: []PlatformAuthorityDRReadiness{{
			SourceDRDomain: "region-a/cluster-a", DRDomain: "region-b/cluster-b",
			ReplicatedThroughAt: observedAt.Add(-time.Second).UTC().Format(time.RFC3339Nano),
			ArtifactsReady:      true, CheckpointsReady: true, MemoryReady: true, TTLSeconds: 600,
		}},
	}
	signed, err := SignPlatformAuthorityPublication(f.privateKey, publication)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func platformAuthorityTestDigest(value byte) string {
	return strings.Repeat(string(value), 64)
}
