// Package fairqueue provides deterministic equal-share ordering for durable
// Execution queues. It intentionally contains no database or runtime policy so
// Worker Claim and Kubernetes reconciliation can share exactly one algorithm.
package fairqueue

import (
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"
)

var ErrInvalidCandidate = errors.New("fair queue candidate is invalid")

var activeServiceStatuses = []string{"leased", "running", "waiting-for-approval"}

// ActiveServiceStatuses returns the statuses that consume an existing Tenant
// service unit before another queued candidate is selected.
func ActiveServiceStatuses() []string {
	return append([]string(nil), activeServiceStatuses...)
}

func IsActiveServiceStatus(status string) bool {
	for _, activeStatus := range activeServiceStatuses {
		if status == activeStatus {
			return true
		}
	}
	return false
}

func IsQueuedStatus(status string) bool {
	return status == "queued" || status == "recovering"
}

// Candidate is the immutable identity and FIFO authority used by equal-share
// scheduling. Each candidate consumes one service unit in v1.
type Candidate struct {
	TenantID uuid.UUID
	ID       uuid.UUID
	QueuedAt time.Time
}

// Order returns a new deterministic service order without mutating candidates
// or activeServiceUnits. It repeatedly chooses the Tenant with the fewest
// active service units, takes that Tenant's oldest candidate, and increments a
// transaction-local unit for subsequent choices in the same batch.
func Order(candidates []Candidate, activeServiceUnits map[uuid.UUID]int64) ([]Candidate, error) {
	queues := make(map[uuid.UUID][]Candidate)
	seenCandidateIDs := make(map[uuid.UUID]struct{}, len(candidates))
	for _, candidate := range candidates {
		if candidate.TenantID == uuid.Nil || candidate.ID == uuid.Nil || candidate.QueuedAt.IsZero() {
			return nil, ErrInvalidCandidate
		}
		if _, duplicate := seenCandidateIDs[candidate.ID]; duplicate {
			return nil, ErrInvalidCandidate
		}
		seenCandidateIDs[candidate.ID] = struct{}{}
		queues[candidate.TenantID] = append(queues[candidate.TenantID], candidate)
	}
	for tenantID := range queues {
		tenantQueue := queues[tenantID]
		sort.Slice(tenantQueue, func(left, right int) bool {
			if !tenantQueue[left].QueuedAt.Equal(tenantQueue[right].QueuedAt) {
				return tenantQueue[left].QueuedAt.Before(tenantQueue[right].QueuedAt)
			}
			return tenantQueue[left].ID.String() < tenantQueue[right].ID.String()
		})
		queues[tenantID] = tenantQueue
	}

	serviceUnits := make(map[uuid.UUID]int64, len(queues)+len(activeServiceUnits))
	for tenantID, units := range activeServiceUnits {
		if tenantID == uuid.Nil || units < 0 {
			return nil, ErrInvalidCandidate
		}
		serviceUnits[tenantID] = units
	}
	ordered := make([]Candidate, 0, len(candidates))
	for len(ordered) < len(candidates) {
		selectedTenantID, ok := nextTenant(queues, serviceUnits)
		if !ok {
			return nil, ErrInvalidCandidate
		}
		selected := queues[selectedTenantID][0]
		queues[selectedTenantID] = queues[selectedTenantID][1:]
		ordered = append(ordered, selected)
		serviceUnits[selectedTenantID]++
	}
	return ordered, nil
}

func nextTenant(
	queues map[uuid.UUID][]Candidate,
	serviceUnits map[uuid.UUID]int64,
) (uuid.UUID, bool) {
	var selectedTenantID uuid.UUID
	var selectedHead Candidate
	selected := false
	for tenantID, queue := range queues {
		if len(queue) == 0 {
			continue
		}
		if !selected || tenantBefore(
			tenantID,
			queue[0],
			serviceUnits[tenantID],
			selectedTenantID,
			selectedHead,
			serviceUnits[selectedTenantID],
		) {
			selectedTenantID = tenantID
			selectedHead = queue[0]
			selected = true
		}
	}
	return selectedTenantID, selected
}

func tenantBefore(
	leftTenantID uuid.UUID,
	leftHead Candidate,
	leftServiceUnits int64,
	rightTenantID uuid.UUID,
	rightHead Candidate,
	rightServiceUnits int64,
) bool {
	if leftServiceUnits != rightServiceUnits {
		return leftServiceUnits < rightServiceUnits
	}
	if !leftHead.QueuedAt.Equal(rightHead.QueuedAt) {
		return leftHead.QueuedAt.Before(rightHead.QueuedAt)
	}
	if leftHead.ID != rightHead.ID {
		return leftHead.ID.String() < rightHead.ID.String()
	}
	return leftTenantID.String() < rightTenantID.String()
}
