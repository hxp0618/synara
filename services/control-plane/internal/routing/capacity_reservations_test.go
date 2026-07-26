package routing

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestHealthReservationAuthorityFiltersStaleGenerationsAndCannotDowngrade(t *testing.T) {
	fixture := newRoutingFixture(t)
	target := fixture.createTarget(t, "reservation-authority")
	execution := fixture.createCapacityExecution(t, target.ID, "queued", 0)

	health, err := fixture.service.ObserveHealth(context.Background(), HealthObservation{
		ExecutionTargetID: target.ID, Status: HealthHealthy, CapacityStatus: CapacityAvailable,
		AvailableCapacityUnits: intPointer(2), AllocatedCapacityUnits: 1,
		ReservationAuthority: &ReservationAuthorityObservation{
			Mode: ReservationAuthorityExactActiveV1,
			Acknowledgements: []ReservationIdentity{{
				ExecutionID: execution.ID, Generation: execution.Generation,
			}},
		},
		Source: "reservation-test", ObservedAt: fixture.now, TTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if health.ReservationAuthorityMode == nil || *health.ReservationAuthorityMode != ReservationAuthorityExactActiveV1 ||
		health.ReservationAcknowledgedUnits != 1 || health.ReservationAcknowledgementsSHA256 == nil {
		t.Fatalf("health authority = %#v", health)
	}
	if got := ReservationAcknowledgementsSHA256(target.ID, 1, []ReservationIdentity{{
		ExecutionID: execution.ID, Generation: 0,
	}}); got != *health.ReservationAcknowledgementsSHA256 {
		t.Fatalf("ack digest = %s want %s", *health.ReservationAcknowledgementsSHA256, got)
	}

	fixture.now = fixture.now.Add(time.Second)
	_, err = fixture.service.ObserveHealth(context.Background(), HealthObservation{
		ExecutionTargetID: target.ID, Status: HealthHealthy, CapacityStatus: CapacityAvailable,
		AvailableCapacityUnits: intPointer(2), AllocatedCapacityUnits: 1,
		Source: "reservation-test", ObservedAt: fixture.now, TTL: time.Minute,
	})
	if codeOf(err) != "target_reservation_authority_required" {
		t.Fatalf("downgrade err = %v", err)
	}

	if err := fixture.db.Model(&persistence.AgentExecution{}).Where("id = ?", execution.ID).
		Updates(map[string]any{"status": "recovering", "generation": 1}).Error; err != nil {
		t.Fatal(err)
	}
	fixture.now = fixture.now.Add(time.Second)
	health, err = fixture.service.ObserveHealth(context.Background(), HealthObservation{
		ExecutionTargetID: target.ID, Status: HealthHealthy, CapacityStatus: CapacityAvailable,
		AvailableCapacityUnits: intPointer(2), AllocatedCapacityUnits: 1,
		ReservationAuthority: &ReservationAuthorityObservation{
			Mode: ReservationAuthorityExactActiveV1,
			Acknowledgements: []ReservationIdentity{{
				ExecutionID: execution.ID, Generation: 0,
			}},
		},
		Source: "reservation-test", ObservedAt: fixture.now, TTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if health.ReservationAcknowledgedUnits != 0 || health.ReservationAcknowledgementsSHA256 == nil {
		t.Fatalf("stale generation authority = %#v", health)
	}
	var acknowledgements int64
	if err := fixture.db.Model(&persistence.ExecutionTargetReservationAcknowledgement{}).
		Where("execution_target_id = ?", target.ID).Count(&acknowledgements).Error; err != nil {
		t.Fatal(err)
	}
	if acknowledgements != 0 {
		t.Fatalf("acknowledgements = %d", acknowledgements)
	}
}

func TestExactReservationAuthorityEliminatesQueuedPodDoubleCountAndGatesUnacknowledgedWork(t *testing.T) {
	fixture := newRoutingFixture(t)
	group := fixture.createGroup(t, StrategyBalanced, true, []string{"cn-shanghai"})
	target := fixture.createTarget(t, "strict-capacity")
	fixture.addMember(t, group.ID, target.ID, "cn-shanghai", "strict-capacity", 10, 100)
	acknowledged := fixture.createCapacityExecution(t, target.ID, "queued", 0)

	_, err := fixture.service.ObserveHealth(context.Background(), HealthObservation{
		ExecutionTargetID: target.ID, Status: HealthHealthy, CapacityStatus: CapacityAvailable,
		AvailableCapacityUnits: intPointer(2), AllocatedCapacityUnits: 1,
		ReservationAuthority: &ReservationAuthorityObservation{
			Mode: ReservationAuthorityExactActiveV1,
			Acknowledgements: []ReservationIdentity{{
				ExecutionID: acknowledged.ID, Generation: acknowledged.Generation,
			}},
		},
		Source: "reservation-test", ObservedAt: fixture.now, TTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	selection, err := fixture.service.Select(context.Background(), fixture.db, SelectRequest{
		TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, TargetGroupID: group.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if selection.QueuePressure.QueuedExecutionUnits != 1 ||
		selection.QueuePressure.UnacknowledgedReservationUnits != 0 ||
		selection.QueuePressure.StrictCapacityUsedUnits == nil ||
		*selection.QueuePressure.StrictCapacityUsedUnits != 1 {
		t.Fatalf("reservation pressure = %#v", selection.QueuePressure)
	}

	fixture.createCapacityExecution(t, target.ID, "recovering", 0)
	_, err = fixture.service.Select(context.Background(), fixture.db, SelectRequest{
		TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, TargetGroupID: group.ID,
	})
	if codeOf(err) != "target_group_no_eligible_destination" {
		t.Fatalf("strict capacity selection err = %v", err)
	}
}

func TestCapacityAdmissionCountsNewAndRecoveredGenerationsAsUnacknowledged(t *testing.T) {
	fixture := newRoutingFixture(t)
	target := fixture.createTarget(t, "fixed-strict-capacity")
	acknowledged := fixture.createCapacityExecution(t, target.ID, "queued", 0)
	_, err := fixture.service.ObserveHealth(context.Background(), HealthObservation{
		ExecutionTargetID: target.ID, Status: HealthHealthy, CapacityStatus: CapacityAvailable,
		AvailableCapacityUnits: intPointer(2), AllocatedCapacityUnits: 1,
		ReservationAuthority: &ReservationAuthorityObservation{
			Mode: ReservationAuthorityExactActiveV1,
			Acknowledgements: []ReservationIdentity{{
				ExecutionID: acknowledged.ID, Generation: acknowledged.Generation,
			}},
		},
		Source: "reservation-test", ObservedAt: fixture.now, TTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	newExecutionID := uuid.New()
	admission, err := AdmitExecutionCapacity(
		context.Background(), fixture.db, fixture.tenantID, newExecutionID, target.ID, false, fixture.now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if admission.Evidence.AdmissionMode != CapacityAdmissionExactActiveV1 ||
		admission.Evidence.StrictCapacityUsedUnits == nil || *admission.Evidence.StrictCapacityUsedUnits != 1 ||
		admission.Evidence.UnacknowledgedReservationUnits == nil || *admission.Evidence.UnacknowledgedReservationUnits != 0 {
		t.Fatalf("admission = %#v", admission.Evidence)
	}

	newExecution := fixture.createCapacityExecutionWithID(t, newExecutionID, target.ID, "queued", 0)
	_, err = AdmitExecutionCapacity(
		context.Background(), fixture.db, fixture.tenantID, uuid.New(), target.ID, false, fixture.now,
	)
	if codeOf(err) != "execution_target_capacity_reserved" {
		t.Fatalf("unacknowledged new execution err = %v", err)
	}
	if err := fixture.db.Model(&persistence.AgentExecution{}).Where("id = ?", newExecution.ID).
		Update("status", "completed").Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.AgentExecution{}).Where("id = ?", acknowledged.ID).
		Updates(map[string]any{"status": "recovering", "generation": 1}).Error; err != nil {
		t.Fatal(err)
	}
	_, err = AdmitExecutionCapacity(
		context.Background(), fixture.db, fixture.tenantID, uuid.New(), target.ID, false, fixture.now,
	)
	if codeOf(err) != "execution_target_capacity_reserved" {
		t.Fatalf("recovered generation err = %v", err)
	}
}

func (f *routingFixture) createCapacityExecution(
	t *testing.T,
	targetID uuid.UUID,
	status string,
	generation int64,
) persistence.AgentExecution {
	return f.createCapacityExecutionWithID(t, uuid.New(), targetID, status, generation)
}

func (f *routingFixture) createCapacityExecutionWithID(
	t *testing.T,
	executionID uuid.UUID,
	targetID uuid.UUID,
	status string,
	generation int64,
) persistence.AgentExecution {
	t.Helper()
	execution := persistence.AgentExecution{
		ID: executionID, TenantID: f.tenantID, SessionID: uuid.New(), TurnID: uuid.New(),
		Attempt: 1, Status: status, ExecutionTargetID: targetID, TargetKind: "kubernetes",
		Generation: generation, RequestedBy: uuid.New(), QueuedAt: f.now,
	}
	if err := f.db.Create(&execution).Error; err != nil {
		t.Fatal(err)
	}
	return execution
}
