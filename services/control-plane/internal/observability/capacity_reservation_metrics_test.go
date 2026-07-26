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

func TestCapacityReservationMetricsRemainBoundedAndDoNotExposeTargetIdentity(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:capacity-reservation-metrics?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(
		&persistence.ExecutionTarget{},
		&persistence.AgentExecution{},
		&persistence.ExecutionTargetHealth{},
		&persistence.ExecutionTargetReservationAcknowledgement{},
		&persistence.ExecutionCapacityAdmission{},
	); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 26, 15, 0, 0, 0, time.UTC)
	targetID := uuid.New()
	if err := db.Create(&persistence.ExecutionTarget{
		ID: targetID, Kind: "kubernetes", Name: "capacity-reservation-metrics", Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	mode := "exact-active-v1"
	digest := strings.Repeat("a", 64)
	ceiling := 3
	if err := db.Create(&persistence.ExecutionTargetHealth{
		ExecutionTargetID: targetID, Status: "healthy", CapacityStatus: "available",
		AvailableCapacityUnits: &ceiling, AllocatedCapacityUnits: 1,
		ReservationAuthorityMode: &mode, ReservationAcknowledgedUnits: 1,
		ReservationAcknowledgementsSHA256: &digest,
		Source:                            "metrics", ObservedAt: now, ExpiresAt: now.Add(time.Minute), Version: 1, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	tenantID := uuid.New()
	acknowledged := capacityReservationMetricExecution(tenantID, targetID, now)
	unacknowledged := capacityReservationMetricExecution(tenantID, targetID, now)
	if err := db.Create(&[]persistence.AgentExecution{acknowledged, unacknowledged}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.ExecutionTargetReservationAcknowledgement{
		ExecutionTargetID: targetID, ExecutionID: acknowledged.ID, ExecutionGeneration: 0,
		HealthVersion: 1, AcknowledgedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.ExecutionCapacityAdmission{
		TenantID: tenantID, ExecutionID: acknowledged.ID, ExecutionTargetID: targetID,
		AdmissionMode: "fixed-unbounded-v1", ActiveReservationUnits: 0,
		AdmittedAt: now, SnapshotSHA256: strings.Repeat("b", 64),
	}).Error; err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	if err := New(db).writeCapacityReservationMetrics(context.Background(), &output, now); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		`synara_execution_capacity_admission_evidence{mode="fixed-unbounded-v1"} 1`,
		`synara_execution_target_reservation_authorities{freshness="fresh"} 1`,
		`synara_execution_target_reservation_acknowledged_units{freshness="fresh"} 1`,
		`synara_execution_target_reservation_unacknowledged_units{freshness="fresh"} 1`,
		`synara_execution_target_strict_capacity_used_units{freshness="fresh"} 2`,
	} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("metrics missing %q:\n%s", expected, output.String())
		}
	}
	if strings.Contains(output.String(), targetID.String()) || strings.Contains(output.String(), tenantID.String()) {
		t.Fatalf("capacity metrics exposed a high-cardinality identity:\n%s", output.String())
	}
}

func capacityReservationMetricExecution(
	tenantID uuid.UUID,
	targetID uuid.UUID,
	now time.Time,
) persistence.AgentExecution {
	return persistence.AgentExecution{
		ID: uuid.New(), TenantID: tenantID, SessionID: uuid.New(), TurnID: uuid.New(),
		Attempt: 1, Status: "queued", ExecutionTargetID: targetID, TargetKind: "kubernetes",
		Generation: 0, RequestedBy: uuid.New(), QueuedAt: now,
	}
}
