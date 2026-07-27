package observability

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestWorkerPoolWarmCapacityMetricsAreFreshBoundedAndStable(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&persistence.WorkerPoolWarmCapacity{}); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, time.July, 25, 12, 0, 0, 0, time.UTC)
	tenantID := uuid.New()
	targetID := uuid.New()
	poolIDs := []uuid.UUID{uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()}
	unboundedClass := "tenant-" + uuid.NewString()
	rows := []persistence.WorkerPoolWarmCapacity{
		warmCapacityMetricFixture(poolIDs[0], tenantID, targetID, "standard", true, 4, 4, 5, 2, 3, now.Add(time.Minute)),
		warmCapacityMetricFixture(poolIDs[1], tenantID, targetID, "standard", false, 2, 2, 0, 4, 0, now.Add(time.Minute)),
		warmCapacityMetricFixture(poolIDs[2], tenantID, targetID, "interactive", true, 2, 2, 2, 1, 1, now.Add(time.Minute)),
		warmCapacityMetricFixture(poolIDs[3], tenantID, targetID, "interactive", false, 7, 7, 0, 7, 0, now),
		warmCapacityMetricFixture(poolIDs[4], tenantID, targetID, unboundedClass, true, 3, 3, 4, 3, 2, now.Add(time.Minute)),
	}
	if err := db.Create(&rows).Error; err != nil {
		t.Fatal(err)
	}

	registry := New(db)
	var first bytes.Buffer
	if err := registry.writeWorkerPoolWarmCapacityMetrics(context.Background(), &first, now); err != nil {
		t.Fatal(err)
	}
	var second bytes.Buffer
	if err := registry.writeWorkerPoolWarmCapacityMetrics(context.Background(), &second, now); err != nil {
		t.Fatal(err)
	}
	if first.String() != second.String() {
		t.Fatalf("warm-capacity metric output is not stable:\nfirst:\n%s\nsecond:\n%s", first.String(), second.String())
	}

	metrics := first.String()
	for _, expected := range []string{
		`synara_worker_pool_warm_capacity_authorities{capacity_class="interactive",freshness="expired",warm_supported="false"} 1`,
		`synara_worker_pool_warm_capacity_authorities{capacity_class="interactive",freshness="fresh",warm_supported="true"} 1`,
		`synara_worker_pool_warm_capacity_authorities{capacity_class="other",freshness="fresh",warm_supported="true"} 1`,
		`synara_worker_pool_warm_capacity_authorities{capacity_class="standard",freshness="fresh",warm_supported="false"} 1`,
		`synara_worker_pool_warm_capacity_authorities{capacity_class="standard",freshness="fresh",warm_supported="true"} 1`,
		`synara_worker_pool_warm_capacity_units{capacity_class="interactive",kind="desired_idle"} 2`,
		`synara_worker_pool_warm_capacity_units{capacity_class="interactive",kind="min_idle"} 2`,
		`synara_worker_pool_warm_capacity_units{capacity_class="interactive",kind="desired_total"} 2`,
		`synara_worker_pool_warm_capacity_units{capacity_class="interactive",kind="claimed"} 1`,
		`synara_worker_pool_warm_capacity_units{capacity_class="interactive",kind="ready_idle"} 1`,
		`synara_worker_pool_warm_capacity_units{capacity_class="other",kind="desired_idle"} 3`,
		`synara_worker_pool_warm_capacity_units{capacity_class="other",kind="min_idle"} 3`,
		`synara_worker_pool_warm_capacity_units{capacity_class="other",kind="desired_total"} 4`,
		`synara_worker_pool_warm_capacity_units{capacity_class="standard",kind="desired_idle"} 6`,
		`synara_worker_pool_warm_capacity_units{capacity_class="standard",kind="min_idle"} 6`,
		`synara_worker_pool_warm_capacity_units{capacity_class="standard",kind="desired_total"} 5`,
		`synara_worker_pool_warm_capacity_units{capacity_class="standard",kind="claimed"} 6`,
		`synara_worker_pool_warm_capacity_units{capacity_class="standard",kind="ready_idle"} 3`,
		`synara_worker_pool_warm_deficit{capacity_class="interactive"} 1`,
		`synara_worker_pool_warm_deficit{capacity_class="other"} 1`,
		`synara_worker_pool_warm_deficit{capacity_class="standard"} 3`,
	} {
		if !strings.Contains(metrics, expected) {
			t.Fatalf("metrics omitted %q:\n%s", expected, metrics)
		}
	}
	for _, capacityClass := range []string{"interactive", "other", "standard"} {
		for _, kind := range []string{"desired_idle", "min_idle", "desired_total", "claimed", "ready_idle"} {
			sample := `synara_worker_pool_warm_capacity_units{capacity_class="` + capacityClass + `",kind="` + kind + `"}`
			if count := strings.Count(metrics, sample); count != 1 {
				t.Fatalf("warm-capacity sample %q appeared %d times, want exactly once:\n%s", sample, count, metrics)
			}
		}
	}
	if strings.Contains(metrics, `capacity_class="interactive",kind="claimed"} 8`) {
		t.Fatalf("expired warm-capacity units contributed to fresh unit sums:\n%s", metrics)
	}
	if strings.Contains(metrics, `synara_worker_pool_warm_deficit{capacity_class="interactive"} 8`) {
		t.Fatalf("expired warm-capacity deficit contributed to fresh deficit sums:\n%s", metrics)
	}
	for _, forbidden := range append([]string{tenantID.String(), targetID.String(), unboundedClass}, uuidStrings(poolIDs)...) {
		if strings.Contains(metrics, forbidden) {
			t.Fatalf("metrics leaked high-cardinality identifier %q:\n%s", forbidden, metrics)
		}
	}
}

func warmCapacityMetricFixture(
	poolID, tenantID, targetID uuid.UUID,
	capacityClass string,
	warmSupported bool,
	desiredIdle, minIdle int,
	desiredTotal, claimed, readyIdle int,
	expiresAt time.Time,
) persistence.WorkerPoolWarmCapacity {
	observedAt := expiresAt.Add(-time.Minute)
	return persistence.WorkerPoolWarmCapacity{
		WorkerPoolID: poolID, WorkerPoolVersion: 1, TenantID: tenantID, ExecutionTargetID: targetID,
		CapacityClass: capacityClass, WarmSupported: warmSupported,
		DesiredIdleUnits: desiredIdle, MinIdleUnits: minIdle, MaxActiveUnits: 10,
		DesiredTotalUnits: desiredTotal, ClaimedUnits: claimed, ReadyIdleUnits: readyIdle,
		Source: "test", ObservedAt: observedAt, ExpiresAt: expiresAt, Version: 1, UpdatedAt: observedAt,
	}
}

func uuidStrings(values []uuid.UUID) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value.String())
	}
	return result
}
