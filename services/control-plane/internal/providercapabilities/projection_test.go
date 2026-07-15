package providercapabilities

import (
	"testing"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/providercatalog"
)

func TestTargetProjectionSeparatesStaticUnsupportedUnobservedAndObservedSupport(t *testing.T) {
	targetID := uuid.New()
	projection, err := ProjectTarget(TargetInput{
		ExecutionTargetID: targetID, TargetKind: "kubernetes", TargetStatus: "active",
		ExperimentalProviderEnabled: map[string]bool{"codex": true, "claudeAgent": true},
		Observations:                map[string][]ManifestObservation{},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, Check(projection, "codex", "send-turn"), StatusUnobserved, ReasonWorkerManifestRequired)
	assertDecision(t, Check(projection, "codex", "compact"), StatusUnobserved, ReasonWorkerManifestRequired)
	assertDecision(t, Check(projection, "claudeAgent", "review"), StatusUnobserved, ReasonWorkerManifestRequired)
	assertDecision(t, Check(projection, "claudeAgent", "compact"), StatusUnsupported, ReasonCapabilityUnsupported)
	assertSupportedMode(t, Check(projection, "codex", "rollback"), SupportModeEmulated)
	assertSupportedMode(t, Check(projection, "codex", "fork"), SupportModeEmulated)
	assertSupportedMode(t, Check(projection, "claudeAgent", "rollback"), SupportModeEmulated)
	assertSupportedMode(t, Check(projection, "claudeAgent", "fork"), SupportModeEmulated)
	assertDecision(t, Check(projection, "cursor", "send-turn"), StatusUnsupported, ReasonCapabilityUnsupported)
	assertDecision(t, Check(projection, "droid", "send-turn"), StatusUnsupported, ReasonCapabilityUnsupported)
}

func TestControlPlaneHistoryCapabilitiesDoNotDependOnWorkerOrTargetAvailability(t *testing.T) {
	projection, err := ProjectTarget(TargetInput{
		ExecutionTargetID: uuid.New(), TargetKind: "kubernetes", TargetStatus: "unavailable",
		ExperimentalProviderEnabled: map[string]bool{}, Observations: map[string][]ManifestObservation{},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertSupportedMode(t, Check(projection, "codex", "rollback"), SupportModeEmulated)
	assertSupportedMode(t, Check(projection, "codex", "fork"), SupportModeEmulated)
	assertDecision(t, Check(projection, "codex", "send-turn"), StatusUnsupported, ReasonExecutionTargetUnavailable)
	assertDecision(t, Check(projection, "cursor", "rollback"), StatusUnsupported, ReasonCapabilityUnsupported)

	execution, err := ProjectExecution(ExecutionInput{
		ExecutionTargetID: uuid.New(), TargetKind: "kubernetes", TargetStatus: "unavailable",
		ExecutionID: uuid.New(), Provider: "codex",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertSupportedMode(t, Check(execution, "codex", "rollback"), SupportModeEmulated)
	assertSupportedMode(t, Check(execution, "codex", "fork"), SupportModeEmulated)
	assertDecision(t, Check(execution, "codex", "send-turn"), StatusUnsupported, ReasonExecutionTargetUnavailable)
}

func TestTargetProjectionRequiresEveryClaimableManifestToSupportCapability(t *testing.T) {
	capabilitiesA := fullCapabilityMap("native")
	capabilitiesB := fullCapabilityMap("native")
	capabilitiesB["plan-mode"] = "unsupported"
	projection, err := ProjectTarget(TargetInput{
		ExecutionTargetID: uuid.New(), TargetKind: "kubernetes", TargetStatus: "active",
		ExperimentalProviderEnabled: map[string]bool{"codex": true},
		Observations: map[string][]ManifestObservation{
			"codex": {
				{WorkerCompatible: true, CompatibilityStatus: "compatible", Capabilities: capabilitiesA},
				{WorkerCompatible: true, CompatibilityStatus: "compatible", Capabilities: capabilitiesB},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, Check(projection, "codex", "send-turn"), StatusSupported, ReasonCapabilitySupported)
	assertDecision(t, Check(projection, "codex", "plan-mode"), StatusUnsupported, ReasonCapabilityUnsupported)
}

func TestTargetProjectionUsesLowestCommonSupportedMode(t *testing.T) {
	native := fullCapabilityMap("native")
	emulated := fullCapabilityMap("native")
	emulated["read-history"] = "emulated"
	projection, err := ProjectTarget(TargetInput{
		ExecutionTargetID: uuid.New(), TargetKind: "docker", TargetStatus: "active",
		ExperimentalProviderEnabled: map[string]bool{"codex": true},
		Observations: map[string][]ManifestObservation{
			"codex": {
				{WorkerCompatible: true, CompatibilityStatus: "compatible", Capabilities: native},
				{WorkerCompatible: true, CompatibilityStatus: "compatible", Capabilities: emulated},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	decision := Check(projection, "codex", "read-history")
	assertDecision(t, decision, StatusSupported, ReasonCapabilitySupported)
	if decision.SupportMode == nil || *decision.SupportMode != SupportModeEmulated {
		t.Fatalf("support mode = %#v, want emulated", decision.SupportMode)
	}
}

func TestExecutionProjectionUsesOnlyPinnedManifestAndNilIsUnobserved(t *testing.T) {
	executionID := uuid.New()
	withoutManifest, err := ProjectExecution(ExecutionInput{
		ExecutionTargetID: uuid.New(), TargetKind: "kubernetes", TargetStatus: "active",
		ExecutionID: executionID, Provider: "codex",
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, Check(withoutManifest, "codex", "steer-turn"), StatusUnobserved, ReasonWorkerManifestRequired)
	if withoutManifest.Basis != BasisExecution || withoutManifest.ExecutionID == nil || *withoutManifest.ExecutionID != executionID {
		t.Fatalf("execution projection identity = %#v", withoutManifest)
	}

	capabilities := fullCapabilityMap("native")
	capabilities["steer-turn"] = "unsupported"
	withManifest, err := ProjectExecution(ExecutionInput{
		ExecutionTargetID: uuid.New(), TargetKind: "kubernetes", TargetStatus: "active",
		ExecutionID: executionID, Provider: "codex",
		Manifest: &ManifestObservation{
			WorkerCompatible: true, CompatibilityStatus: "compatible", Capabilities: capabilities,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, Check(withManifest, "codex", "steer-turn"), StatusUnsupported, ReasonCapabilityUnsupported)
}

func assertDecision(t *testing.T, decision Decision, status Status, reason string) {
	t.Helper()
	if decision.Status != status || decision.ReasonCode != reason {
		t.Fatalf("decision = %#v, want status=%q reason=%q", decision, status, reason)
	}
}

func assertSupportedMode(t *testing.T, decision Decision, mode SupportMode) {
	t.Helper()
	assertDecision(t, decision, StatusSupported, ReasonCapabilitySupported)
	if decision.SupportMode == nil || *decision.SupportMode != mode {
		t.Fatalf("decision support mode = %#v, want %q", decision.SupportMode, mode)
	}
}

func fullCapabilityMap(value string) map[string]any {
	result := map[string]any{}
	for _, capabilityID := range providercatalog.CapabilityIDs() {
		result[capabilityID] = value
	}
	return result
}
