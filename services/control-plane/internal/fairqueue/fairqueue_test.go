package fairqueue

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestOrderBalancesTenantsWithinOneBatchAndPreservesTenantFIFO(t *testing.T) {
	base := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	tenantA := mustUUID("00000000-0000-0000-0000-00000000000a")
	tenantB := mustUUID("00000000-0000-0000-0000-00000000000b")
	a1 := candidate(tenantA, "10000000-0000-0000-0000-000000000001", base)
	a2 := candidate(tenantA, "10000000-0000-0000-0000-000000000002", base.Add(time.Second))
	a3 := candidate(tenantA, "10000000-0000-0000-0000-000000000003", base.Add(2*time.Second))
	b1 := candidate(tenantB, "20000000-0000-0000-0000-000000000001", base.Add(3*time.Second))

	ordered, err := Order([]Candidate{a3, a2, b1, a1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := candidateIDs(ordered), []uuid.UUID{a1.ID, b1.ID, a2.ID, a3.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("equal-share order = %v, want %v", got, want)
	}
}

func TestServiceStatusSetsAreStableAndDefensivelyCopied(t *testing.T) {
	statuses := ActiveServiceStatuses()
	if got, want := statuses, []string{"leased", "running", "waiting-for-approval"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("active service statuses = %v, want %v", got, want)
	}
	statuses[0] = "mutated"
	if !IsActiveServiceStatus("leased") || IsActiveServiceStatus("queued") ||
		!IsQueuedStatus("queued") || !IsQueuedStatus("recovering") || IsQueuedStatus("running") {
		t.Fatal("fair queue status authority changed through a caller-owned slice")
	}
}

func TestOrderAccountsForExistingActiveServiceUnits(t *testing.T) {
	base := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	tenantA := mustUUID("00000000-0000-0000-0000-00000000000a")
	tenantB := mustUUID("00000000-0000-0000-0000-00000000000b")
	a1 := candidate(tenantA, "10000000-0000-0000-0000-000000000001", base)
	b1 := candidate(tenantB, "20000000-0000-0000-0000-000000000001", base.Add(time.Minute))
	b2 := candidate(tenantB, "20000000-0000-0000-0000-000000000002", base.Add(2*time.Minute))

	active := map[uuid.UUID]int64{tenantA: 2, tenantB: 0}
	ordered, err := Order([]Candidate{a1, b2, b1}, active)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := candidateIDs(ordered), []uuid.UUID{b1.ID, b2.ID, a1.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("active-aware order = %v, want %v", got, want)
	}
	if active[tenantB] != 0 {
		t.Fatalf("Order mutated caller active units: %#v", active)
	}
}

func TestOrderUsesQueueClassAndPriorityInsideTenantShare(t *testing.T) {
	base := time.Date(2026, time.July, 27, 12, 0, 0, 0, time.UTC)
	tenantA := mustUUID("00000000-0000-0000-0000-00000000000a")
	tenantB := mustUUID("00000000-0000-0000-0000-00000000000b")
	batch := candidate(tenantA, "10000000-0000-0000-0000-000000000001", base)
	batch.QueueClass = "batch"
	interactive := candidate(tenantA, "10000000-0000-0000-0000-000000000002", base.Add(time.Minute))
	interactive.QueueClass = "interactive"
	automationLow := candidate(tenantB, "20000000-0000-0000-0000-000000000001", base)
	automationLow.QueueClass = "automation"
	automationLow.QueuePriority = -10
	automationHigh := candidate(tenantB, "20000000-0000-0000-0000-000000000002", base.Add(time.Minute))
	automationHigh.QueueClass = "automation"
	automationHigh.QueuePriority = 10

	ordered, err := OrderWithPolicy(
		[]Candidate{batch, automationLow, interactive, automationHigh},
		nil,
		Policy{Now: base.Add(2 * time.Minute), StarvationThreshold: 5 * time.Minute},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := candidateIDs(ordered), []uuid.UUID{
		interactive.ID, automationHigh.ID, automationLow.ID, batch.ID,
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("class/priority fair order = %v, want %v", got, want)
	}
}

func TestOrderPromotesStarvedCandidateWithoutDroppingTenantFairness(t *testing.T) {
	base := time.Date(2026, time.July, 27, 12, 0, 0, 0, time.UTC)
	tenantA := mustUUID("00000000-0000-0000-0000-00000000000a")
	tenantB := mustUUID("00000000-0000-0000-0000-00000000000b")
	starvedBatch := candidate(tenantA, "10000000-0000-0000-0000-000000000001", base)
	starvedBatch.QueueClass = "batch"
	freshInteractive := candidate(tenantB, "20000000-0000-0000-0000-000000000001", base.Add(9*time.Minute))
	freshInteractive.QueueClass = "interactive"

	ordered, err := OrderWithPolicy(
		[]Candidate{freshInteractive, starvedBatch},
		map[uuid.UUID]int64{tenantA: 1},
		Policy{Now: base.Add(10 * time.Minute), StarvationThreshold: 5 * time.Minute},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := candidateIDs(ordered); !reflect.DeepEqual(got, []uuid.UUID{starvedBatch.ID, freshInteractive.ID}) {
		t.Fatalf("starvation order = %v", got)
	}
}

func TestOrderIsDeterministicForEqualTimestamps(t *testing.T) {
	queuedAt := time.Date(2026, time.July, 26, 12, 0, 0, 0, time.UTC)
	tenantA := mustUUID("00000000-0000-0000-0000-00000000000a")
	tenantB := mustUUID("00000000-0000-0000-0000-00000000000b")
	higherID := candidate(tenantA, "f0000000-0000-0000-0000-000000000001", queuedAt)
	lowerID := candidate(tenantB, "10000000-0000-0000-0000-000000000001", queuedAt)

	ordered, err := Order([]Candidate{higherID, lowerID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := candidateIDs(ordered), []uuid.UUID{lowerID.ID, higherID.ID}; !reflect.DeepEqual(got, want) {
		t.Fatalf("deterministic order = %v, want %v", got, want)
	}
}

func TestOrderRejectsInvalidAuthorityWithoutPartialResult(t *testing.T) {
	valid := Candidate{TenantID: uuid.New(), ID: uuid.New(), QueuedAt: time.Now().UTC()}
	for _, testCase := range []struct {
		name       string
		candidates []Candidate
		active     map[uuid.UUID]int64
	}{
		{name: "missing tenant", candidates: []Candidate{{ID: uuid.New(), QueuedAt: valid.QueuedAt}}},
		{name: "missing execution", candidates: []Candidate{{TenantID: uuid.New(), QueuedAt: valid.QueuedAt}}},
		{name: "missing queue time", candidates: []Candidate{{TenantID: uuid.New(), ID: uuid.New()}}},
		{name: "duplicate execution", candidates: []Candidate{valid, valid}},
		{name: "negative active units", candidates: []Candidate{valid}, active: map[uuid.UUID]int64{valid.TenantID: -1}},
		{name: "missing active tenant", candidates: []Candidate{valid}, active: map[uuid.UUID]int64{uuid.Nil: 1}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ordered, err := Order(testCase.candidates, testCase.active)
			if !errors.Is(err, ErrInvalidCandidate) || ordered != nil {
				t.Fatalf("Order = %#v, %v", ordered, err)
			}
		})
	}
}

func candidate(tenantID uuid.UUID, id string, queuedAt time.Time) Candidate {
	return Candidate{TenantID: tenantID, ID: mustUUID(id), QueuedAt: queuedAt}
}

func candidateIDs(candidates []Candidate) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(candidates))
	for _, candidate := range candidates {
		ids = append(ids, candidate.ID)
	}
	return ids
}

func mustUUID(value string) uuid.UUID {
	return uuid.MustParse(value)
}
