package routing

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestPlatformAuthorityPostgresSerializesReplicaReplayAndProtectsReceipt(t *testing.T) {
	db := openRoutingCommitPostgresDB(t)
	now := time.Now().UTC().Add(-time.Second).Truncate(time.Microsecond)
	targetID := uuid.New()
	if err := db.Create(&persistence.ExecutionTarget{
		ID: targetID, Kind: "kubernetes", Name: "platform-routing-pg-" + uuid.NewString(), Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	const publisherIdentity = "platform-routing-postgres-publisher"
	const keyID = "platform-routing-postgres-key-v1"
	configs := []PlatformAuthorityPublisherConfig{{
		PublisherIdentity: publisherIdentity,
		Keys:              []PlatformAuthorityPublisherKey{{KeyID: keyID, PublicKey: publicKey}},
		Targets: []PlatformAuthorityTargetScope{{
			ExecutionTargetID: targetID, Ownership: PlatformAuthorityOwnershipShared, PublishHealth: true,
			DRRoutes: []PlatformAuthorityDRRouteScope{{
				SourceDRDomain: "region-a/cluster-a", DRDomain: "region-b/cluster-b",
			}},
		}},
	}}
	firstService, err := NewPlatformAuthorityService(db, configs)
	if err != nil {
		t.Fatal(err)
	}
	secondService, err := NewPlatformAuthorityService(db, configs)
	if err != nil {
		t.Fatal(err)
	}
	publication, err := SignPlatformAuthorityPublication(privateKey, PlatformAuthorityPublication{
		SchemaVersion: PlatformAuthoritySchemaVersionV1, PublisherIdentity: publisherIdentity,
		KeyID: keyID, Nonce: uuid.NewString(), IssuedAt: now.Format(time.RFC3339Nano),
		ExpiresAt: now.Add(time.Minute).Format(time.RFC3339Nano), ExecutionTargetID: targetID.String(),
		ObservedAt: now.Format(time.RFC3339Nano),
		Health: &PlatformAuthorityHealth{
			Status: HealthHealthy, CapacityStatus: CapacityAvailable,
			AvailableCapacityUnits: intPointer(12), AllocatedCapacityUnits: 4, TTLSeconds: 120,
		},
		DRReadiness: []PlatformAuthorityDRReadiness{{
			SourceDRDomain: "region-a/cluster-a", DRDomain: "region-b/cluster-b",
			ReplicatedThroughAt: now.Add(-time.Second).Format(time.RFC3339Nano),
			ArtifactsReady:      true, CheckpointsReady: true, MemoryReady: true, TTLSeconds: 120,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		result PlatformAuthorityPublicationResult
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	var wait sync.WaitGroup
	for _, service := range []*PlatformAuthorityService{firstService, secondService} {
		wait.Add(1)
		go func(service *PlatformAuthorityService) {
			defer wait.Done()
			<-start
			result, publishErr := service.Publish(context.Background(), targetID, publication)
			outcomes <- outcome{result: result, err: publishErr}
		}(service)
	}
	close(start)
	wait.Wait()
	close(outcomes)
	accepted := 0
	replayed := 0
	for item := range outcomes {
		if item.err != nil {
			t.Fatal(item.err)
		}
		if item.result.Replayed {
			replayed++
		} else {
			accepted++
		}
	}
	if accepted != 1 || replayed != 1 {
		t.Fatalf("accepted=%d replayed=%d", accepted, replayed)
	}

	var receiptCount int64
	if err := db.Model(&persistence.PlatformRoutingPublication{}).Where(
		"publisher_identity = ?", publisherIdentity,
	).Count(&receiptCount).Error; err != nil {
		t.Fatal(err)
	}
	var health persistence.ExecutionTargetHealth
	if err := db.Where("execution_target_id = ?", targetID).Take(&health).Error; err != nil {
		t.Fatal(err)
	}
	var readiness persistence.ExecutionTargetDRReadiness
	if err := db.Where(
		"execution_target_id = ? AND source_dr_domain = ?", targetID, "region-a/cluster-a",
	).Take(&readiness).Error; err != nil {
		t.Fatal(err)
	}
	if receiptCount != 1 || health.Version != 1 || readiness.Version != 1 {
		t.Fatalf("receiptCount=%d health=%#v readiness=%#v", receiptCount, health, readiness)
	}
	if err := db.Model(&persistence.PlatformRoutingPublication{}).
		Where("publisher_identity = ?", publisherIdentity).
		Update("response_sha256", strings.Repeat("f", 64)).Error; err == nil {
		t.Fatal("PostgreSQL receipt trigger accepted an update")
	}
	if err := db.Where("publisher_identity = ?", publisherIdentity).
		Delete(&persistence.PlatformRoutingPublication{}).Error; err == nil {
		t.Fatal("PostgreSQL receipt trigger accepted a delete")
	}
}
