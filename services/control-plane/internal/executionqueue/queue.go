// Package executionqueue defines the immutable queue metadata shared by
// admission, Worker Claim, and Kubernetes reconciliation.
package executionqueue

import (
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	ClassInteractive = "interactive"
	ClassAutomation  = "automation"
	ClassBatch       = "batch"

	MinimumPriority   = -100
	MaximumPriority   = 100
	MaximumQuotaUnits = 1_000_000

	DefaultStarvationThreshold = 5 * time.Minute
)

var ErrInvalidSnapshot = errors.New("execution queue snapshot is invalid")

type Snapshot struct {
	Class        string
	Priority     int
	QuotaUnits   int
	AutomationID *uuid.UUID
}

func Normalize(snapshot Snapshot) (Snapshot, error) {
	snapshot.Class = strings.ToLower(strings.TrimSpace(snapshot.Class))
	if snapshot.Class == "" {
		snapshot.Class = ClassInteractive
	}
	if snapshot.QuotaUnits == 0 {
		snapshot.QuotaUnits = 1
	}
	if snapshot.Priority < MinimumPriority || snapshot.Priority > MaximumPriority ||
		snapshot.QuotaUnits < 1 || snapshot.QuotaUnits > MaximumQuotaUnits {
		return Snapshot{}, ErrInvalidSnapshot
	}
	switch snapshot.Class {
	case ClassInteractive, ClassBatch:
		if snapshot.AutomationID != nil {
			return Snapshot{}, ErrInvalidSnapshot
		}
	case ClassAutomation:
		if snapshot.AutomationID == nil || *snapshot.AutomationID == uuid.Nil {
			return Snapshot{}, ErrInvalidSnapshot
		}
	default:
		return Snapshot{}, ErrInvalidSnapshot
	}
	return snapshot, nil
}

func ClassRank(class string) (int, bool) {
	switch strings.ToLower(strings.TrimSpace(class)) {
	case "", ClassInteractive:
		return 0, true
	case ClassAutomation:
		return 1, true
	case ClassBatch:
		return 2, true
	default:
		return 0, false
	}
}
