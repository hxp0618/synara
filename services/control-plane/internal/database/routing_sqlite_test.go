package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestSQLiteTargetDRReadinessSafetyObjectsAndMonotonicity(t *testing.T) {
	ctx := context.Background()
	config, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "sqlite-routing-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{
		"trg_execution_target_dr_readiness_route_insert",
		"trg_execution_target_dr_readiness_route_update",
		"trg_execution_target_dr_readiness_route_delete",
	} {
		var count int64
		if err := store.DB().Raw(`SELECT count(*) FROM sqlite_master WHERE name = ?`, name).Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("SQLite DR readiness safety object %s count = %d, want 1", name, count)
		}
	}

	now := time.Now().UTC().Truncate(time.Second)
	readiness := persistence.ExecutionTargetDRReadiness{
		ExecutionTargetID:   domain.ExecutionTargetID,
		SourceDRDomain:      "cn-shanghai/source-a",
		DRDomain:            "cn-beijing/destination-a",
		ReplicatedThroughAt: now.Add(-time.Second),
		CheckpointsReady:    true,
		PublisherIdentity:   "sqlite-routing-test",
		ObservedAt:          now,
		ExpiresAt:           now.Add(time.Minute),
		Version:             1,
		UpdatedAt:           now,
	}
	if err := store.DB().Create(&readiness).Error; err != nil {
		t.Fatalf("create valid SQLite DR readiness row: %v", err)
	}

	invalidWatermark := readiness
	invalidWatermark.SourceDRDomain = "cn-shanghai/source-b"
	invalidWatermark.DRDomain = "cn-beijing/destination-b"
	invalidWatermark.ReplicatedThroughAt = now.Add(time.Second)
	if err := store.DB().Create(&invalidWatermark).Error; err == nil {
		t.Fatal("SQLite accepted replicated_through_at later than observed_at")
	}

	if err := store.DB().Exec(
		`UPDATE execution_target_dr_readiness
		    SET version = ?, observed_at = ?
		  WHERE execution_target_id = ? AND source_dr_domain = ?`,
		2, now, readiness.ExecutionTargetID, readiness.SourceDRDomain,
	).Error; err == nil {
		t.Fatal("SQLite accepted a non-monotonic DR readiness update")
	}

	if err := store.DB().Exec(
		`UPDATE execution_target_dr_readiness
		    SET version = ?, observed_at = ?, replicated_through_at = ?
		  WHERE execution_target_id = ? AND source_dr_domain = ?`,
		2, now.Add(time.Second), now.Add(2*time.Second), readiness.ExecutionTargetID, readiness.SourceDRDomain,
	).Error; err == nil {
		t.Fatal("SQLite accepted a DR readiness update with a future watermark")
	}

	if err := store.DB().Delete(&persistence.ExecutionTargetDRReadiness{}, "execution_target_id = ? AND source_dr_domain = ?", readiness.ExecutionTargetID, readiness.SourceDRDomain).Error; err == nil {
		t.Fatal("SQLite allowed a DR readiness authority row to be deleted")
	}
}

func TestSQLiteLocationOutageSafetyObjectsAndMonotonicity(t *testing.T) {
	ctx := context.Background()
	config, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenMetadataStore(ctx, config, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "sqlite-location-outage-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{
		"idx_execution_location_outages_route",
		"trg_execution_location_outages_route_insert",
		"trg_execution_location_outages_route_update",
		"trg_execution_location_outages_route_delete",
	} {
		var count int64
		if err := store.DB().Raw(`SELECT count(*) FROM sqlite_master WHERE name = ?`, name).Scan(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("SQLite location outage safety object %s count = %d, want 1", name, count)
		}
	}

	now := time.Now().UTC().Truncate(time.Second)
	outage := persistence.ExecutionLocationOutage{
		TenantID:          domain.TenantID,
		Region:            "cn-shanghai",
		ClusterID:         "",
		Status:            "draining",
		PublisherIdentity: "sqlite-routing-test",
		ObservedAt:        now,
		ExpiresAt:         now.Add(time.Minute),
		Version:           1,
		UpdatedAt:         now,
	}
	if err := store.DB().Create(&outage).Error; err != nil {
		t.Fatalf("create valid SQLite location outage row: %v", err)
	}

	clusterOutage := outage
	clusterOutage.ClusterID = "cluster-a"
	clusterOutage.Status = "unreachable"
	clusterOutage.ObservedAt = now.Add(time.Second)
	clusterOutage.ExpiresAt = now.Add(2 * time.Minute)
	if err := store.DB().Create(&clusterOutage).Error; err != nil {
		t.Fatalf("create valid SQLite cluster outage row: %v", err)
	}

	invalid := outage
	invalid.Region = "cn-beijing"
	invalid.ExpiresAt = now
	if err := store.DB().Create(&invalid).Error; err == nil {
		t.Fatal("SQLite accepted an already-expired location outage observation")
	}

	if err := store.DB().Exec(
		`UPDATE execution_location_outages
		    SET version = ?, observed_at = ?
		  WHERE tenant_id = ? AND region = ? AND cluster_id = ?`,
		2, now, outage.TenantID, outage.Region, outage.ClusterID,
	).Error; err == nil {
		t.Fatal("SQLite accepted a non-monotonic location outage update")
	}

	if err := store.DB().Delete(
		&persistence.ExecutionLocationOutage{},
		"tenant_id = ? AND region = ? AND cluster_id = ?",
		outage.TenantID, outage.Region, outage.ClusterID,
	).Error; err == nil {
		t.Fatal("SQLite allowed a location outage authority row to be deleted")
	}
}
