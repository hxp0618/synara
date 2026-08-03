package tenantstate

import (
	"testing"
	"time"
)

func TestIsOperational(t *testing.T) {
	now := time.Date(2026, time.July, 30, 0, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	past := now.Add(-time.Hour)
	tests := []struct {
		name            string
		status          string
		trialExpiresAt  *time.Time
		wantOperational bool
	}{
		{name: "active", status: "active", wantOperational: true},
		{name: "current trial", status: "trialing", trialExpiresAt: &future, wantOperational: true},
		{name: "expired trial", status: "trialing", trialExpiresAt: &past},
		{name: "trial without expiry", status: "trialing"},
		{name: "suspended", status: "suspended", trialExpiresAt: &future},
		{name: "closed", status: "closed"},
		{name: "deleting", status: "deleting"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := IsOperational(test.status, test.trialExpiresAt, now); got != test.wantOperational {
				t.Fatalf("IsOperational(%q) = %v, want %v", test.status, got, test.wantOperational)
			}
		})
	}
}
