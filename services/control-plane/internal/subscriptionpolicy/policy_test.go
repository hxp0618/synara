package subscriptionpolicy

import (
	"errors"
	"testing"
)

func TestEffectiveSubscriptionV1(t *testing.T) {
	tests := []struct {
		status string
		active bool
		grace  bool
		reason string
	}{
		{status: "trialing", active: true, reason: "trialing"},
		{status: "active", active: true, reason: "active"},
		{status: "suspended", reason: "internally_suspended"},
		{status: "cancelled", reason: "internally_cancelled"},
	}
	for _, test := range tests {
		t.Run(test.status, func(t *testing.T) {
			decision, err := Evaluate(test.status)
			if err != nil {
				t.Fatal(err)
			}
			if decision.EntitlementsActive != test.active || decision.Grace != test.grace || decision.Reason != test.reason {
				t.Fatalf("decision = %#v", decision)
			}
		})
	}
	if _, err := Evaluate("incomplete"); !errors.Is(err, ErrUnknownStatus) {
		t.Fatalf("unknown stored status error = %v", err)
	}
}
