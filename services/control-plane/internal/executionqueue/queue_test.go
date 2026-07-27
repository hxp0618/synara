package executionqueue

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestNormalizeQueueSnapshot(t *testing.T) {
	automationID := uuid.New()
	tests := []struct {
		name    string
		input   Snapshot
		class   string
		units   int
		invalid bool
	}{
		{name: "defaults", input: Snapshot{}, class: ClassInteractive, units: 1},
		{name: "automation", input: Snapshot{Class: " Automation ", AutomationID: &automationID, QuotaUnits: 4}, class: ClassAutomation, units: 4},
		{name: "batch", input: Snapshot{Class: ClassBatch, Priority: -20}, class: ClassBatch, units: 1},
		{name: "automation identity missing", input: Snapshot{Class: ClassAutomation}, invalid: true},
		{name: "interactive automation identity", input: Snapshot{AutomationID: &automationID}, invalid: true},
		{name: "priority", input: Snapshot{Priority: MaximumPriority + 1}, invalid: true},
		{name: "units", input: Snapshot{QuotaUnits: MaximumQuotaUnits + 1}, invalid: true},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			actual, err := Normalize(testCase.input)
			if testCase.invalid {
				if !errors.Is(err, ErrInvalidSnapshot) {
					t.Fatalf("Normalize() error = %v", err)
				}
				return
			}
			if err != nil || actual.Class != testCase.class || actual.QuotaUnits != testCase.units {
				t.Fatalf("Normalize() = %#v, %v", actual, err)
			}
		})
	}
}
