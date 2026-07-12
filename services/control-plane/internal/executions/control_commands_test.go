package executions

import "testing"

func TestControlCommandCapabilityMapping(t *testing.T) {
	expected := map[string]string{
		"SteerTurn":       "steer-turn",
		"InterruptTurn":   "interrupt-turn",
		"CompactSession":  "compact",
		"RollbackSession": "rollback",
		"ForkSession":     "fork",
		"StartReview":     "review",
	}
	for commandType, capabilityID := range expected {
		actual, ok := controlCommandCapabilityID(commandType)
		if !ok || actual != capabilityID {
			t.Fatalf("unexpected capability mapping for %s: value=%q ok=%t", commandType, actual, ok)
		}
	}
	if _, ok := controlCommandCapabilityID("StopSession"); ok {
		t.Fatal("StopSession must remain fenced until it has an explicit capability contract")
	}
}
