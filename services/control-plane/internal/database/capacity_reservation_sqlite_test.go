package database

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/routing"
)

func TestSQLiteCapacityReservationAuthorityStagingAndAdmissionImmutability(t *testing.T) {
	ctx := context.Background()
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{TranslateError: true})
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
	if err := migrateCapacityReservationSQLiteSafety(ctx, db); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	target := persistence.ExecutionTarget{
		ID: uuid.New(), Kind: "kubernetes", Name: "sqlite-reservation", Status: "active",
		ConfigurationEncrypted: []byte{}, Capabilities: map[string]any{}, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	execution := persistence.AgentExecution{
		ID: uuid.New(), TenantID: uuid.New(), SessionID: uuid.New(), TurnID: uuid.New(),
		Attempt: 1, Status: "queued", ExecutionTargetID: target.ID, TargetKind: target.Kind,
		Generation: 0, RequestedBy: uuid.New(), QueuedAt: now,
	}
	if err := db.Create(&execution).Error; err != nil {
		t.Fatal(err)
	}
	identity := routing.ReservationIdentity{ExecutionID: execution.ID, Generation: execution.Generation}
	if err := db.Create(&persistence.ExecutionTargetReservationAcknowledgement{
		ExecutionTargetID: target.ID, ExecutionID: execution.ID, ExecutionGeneration: execution.Generation,
		HealthVersion: 1, AcknowledgedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	mode := routing.ReservationAuthorityExactActiveV1
	digest := routing.ReservationAcknowledgementsSHA256(target.ID, 1, []routing.ReservationIdentity{identity})
	ceiling := 2
	health := persistence.ExecutionTargetHealth{
		ExecutionTargetID: target.ID, Status: routing.HealthHealthy, CapacityStatus: routing.CapacityAvailable,
		AvailableCapacityUnits: &ceiling, AllocatedCapacityUnits: 1,
		ReservationAuthorityMode: &mode, ReservationAcknowledgedUnits: 1,
		ReservationAcknowledgementsSHA256: &digest,
		Source:                            "sqlite-reservation", ObservedAt: now, ExpiresAt: now.Add(time.Minute),
		Version: 1, UpdatedAt: now,
	}
	if err := db.Create(&health).Error; err != nil {
		t.Fatal(err)
	}

	second := execution
	second.ID = uuid.New()
	if err := db.Create(&second).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.ExecutionTargetReservationAcknowledgement{
		ExecutionTargetID: target.ID, ExecutionID: second.ID, ExecutionGeneration: second.Generation,
		HealthVersion: 1, AcknowledgedAt: now,
	}).Error; err == nil || !strings.Contains(err.Error(), "next Health version") {
		t.Fatalf("current-version direct acknowledgement err = %v", err)
	}

	healthVersion := health.Version
	healthSource := health.Source
	capacityStatus := health.CapacityStatus
	allocated := health.AllocatedCapacityUnits
	acknowledgedUnits := health.ReservationAcknowledgedUnits
	unacknowledged := int64(0)
	strictUsed := int64(allocated)
	admission := persistence.ExecutionCapacityAdmission{
		TenantID: execution.TenantID, ExecutionID: execution.ID, ExecutionTargetID: target.ID,
		AdmissionMode: routing.CapacityAdmissionExactActiveV1,
		HealthVersion: &healthVersion, HealthSource: &healthSource,
		HealthObservedAt: &health.ObservedAt, HealthExpiresAt: &health.ExpiresAt,
		CapacityStatus: &capacityStatus, CapacityCeilingUnits: &ceiling, AllocatedCapacityUnits: &allocated,
		ReservationAuthorityMode: &mode, ReservationAcknowledgedUnits: &acknowledgedUnits,
		ReservationAcknowledgementsSHA256: &digest,
		ActiveReservationUnits:            1, UnacknowledgedReservationUnits: &unacknowledged,
		StrictCapacityUsedUnits: &strictUsed, AdmittedAt: now,
	}
	admission.SnapshotSHA256 = routing.CapacityAdmissionSHA256(admission)
	if err := db.Create(&admission).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&admission).Update("active_reservation_units", 2).Error; err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("admission update err = %v", err)
	}
	if err := db.Delete(&persistence.AgentExecution{}, "id = ?", execution.ID).Error; err != nil {
		t.Fatal(err)
	}
	var admissionCount, acknowledgementCount int64
	if err := db.Model(&persistence.ExecutionCapacityAdmission{}).Where("execution_id = ?", execution.ID).Count(&admissionCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.ExecutionTargetReservationAcknowledgement{}).Where("execution_id = ?", execution.ID).Count(&acknowledgementCount).Error; err != nil {
		t.Fatal(err)
	}
	if admissionCount != 0 || acknowledgementCount != 0 {
		t.Fatalf("parent cascade admission=%d acknowledgement=%d", admissionCount, acknowledgementCount)
	}
}
