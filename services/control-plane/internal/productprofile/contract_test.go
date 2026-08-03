package productprofile

import "testing"

func TestCompatibilityMappings(t *testing.T) {
	if got := PublicProfileCode("free"); got != ProfileStandard {
		t.Fatalf("public profile = %q", got)
	}
	if got := InternalProfileCode(ProfileStandard); got != "free" {
		t.Fatalf("internal profile = %q", got)
	}
	if got := PublicLifecycleStatus("trialing"); got != StatusEvaluation {
		t.Fatalf("public status = %q", got)
	}
	if got := InternalLifecycleStatus(StatusEvaluation); got != "trialing" {
		t.Fatalf("internal status = %q", got)
	}
	if got := PublicAssignmentSource("self_service"); got != "user" {
		t.Fatalf("public assignment source = %q", got)
	}
}
