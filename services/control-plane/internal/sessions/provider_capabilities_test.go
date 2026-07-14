package sessions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/providercapabilities"
	"github.com/synara-ai/synara/services/control-plane/internal/providercatalog"
)

func TestCreateSessionCapabilityGateRejectsLocalOnlyAndDroidButAllowsUnobserved(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	for _, provider := range []string{"cursor", "droid"} {
		_, err := fixture.service.Create(ctx, fixture.principal, fixture.projectID, CreateSessionInput{
			Title: "unsupported " + provider, Provider: provider, ExecutionTargetID: &fixture.executionTargetID,
		}, "provider-capability-static", "127.0.0.1")
		assertSessionProblemCode(t, err, providercapabilities.ReasonCapabilityUnsupported)
	}
	created, err := fixture.service.Create(ctx, fixture.principal, fixture.projectID, CreateSessionInput{
		Title: "unobserved Codex", Provider: "codex", ExecutionTargetID: &fixture.executionTargetID,
	}, "provider-capability-unobserved", "127.0.0.1")
	if err != nil {
		t.Fatalf("unobserved supported Provider was rejected: %v", err)
	}
	if created.Provider != "codex" {
		t.Fatalf("created Provider = %q", created.Provider)
	}
	createdClaude, err := fixture.service.Create(ctx, fixture.principal, fixture.projectID, CreateSessionInput{
		Title: "unobserved Claude", Provider: "claudeAgent", ExecutionTargetID: &fixture.executionTargetID,
	}, "provider-capability-unobserved-claude", "127.0.0.1")
	if err != nil {
		t.Fatalf("unobserved Claude Provider was rejected: %v", err)
	}
	if createdClaude.Provider != "claudeAgent" {
		t.Fatalf("created Claude Provider = %q", createdClaude.Provider)
	}
	var storedClaude persistence.AgentSession
	if err := fixture.db.Where("id = ?", createdClaude.ID).Take(&storedClaude).Error; err != nil {
		t.Fatal(err)
	}
	if storedClaude.Provider != "claudeagent" {
		t.Fatalf("stored Claude Provider = %q", storedClaude.Provider)
	}
	loadedClaude, err := fixture.service.Get(ctx, fixture.principal, fixture.tenantID, createdClaude.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loadedClaude.Provider != "claudeAgent" {
		t.Fatalf("loaded Claude Provider = %q", loadedClaude.Provider)
	}
	listed, err := fixture.service.ListByProject(ctx, fixture.principal, fixture.projectID)
	if err != nil {
		t.Fatal(err)
	}
	foundClaude := false
	for _, session := range listed {
		if session.ID == createdClaude.ID {
			foundClaude = true
			if session.Provider != "claudeAgent" {
				t.Fatalf("listed Claude Provider = %q", session.Provider)
			}
		}
	}
	if !foundClaude {
		t.Fatal("created Claude Session was not listed")
	}
}

func TestCreateSessionCapabilityGateRejectsExplicitIncompatibleManifest(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	seedSessionCapabilityWorker(t, fixture, sessionCapabilityManifestOptions{
		CodexCompatibilityStatus: "incompatible",
		CodexIncompatibilityCode: providercapabilities.ReasonProviderVersionIncompatible,
	})
	_, err := fixture.service.Create(context.Background(), fixture.principal, fixture.projectID, CreateSessionInput{
		Title: "incompatible Codex", Provider: "codex", ExecutionTargetID: &fixture.executionTargetID,
	}, "provider-capability-incompatible", "127.0.0.1")
	assertSessionProblemCode(t, err, providercapabilities.ReasonProviderVersionIncompatible)
}

func TestCreateTurnCapabilityGateRequiresPlanModeInAdditionToSendTurn(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	seedSessionCapabilityWorker(t, fixture, sessionCapabilityManifestOptions{
		CodexCompatibilityStatus: "compatible", CodexPlanMode: "unsupported",
	})
	ctx := context.Background()
	turn, err := fixture.service.CreateTurn(ctx, fixture.principal, fixture.sessionID, CreateTurnInput{
		InputText: "default mode remains supported", RuntimeMode: "full-access", InteractionMode: "default",
	}, "provider-capability-default", "127.0.0.1")
	if err != nil {
		t.Fatalf("default Turn was rejected: %v", err)
	}
	var execution persistence.AgentExecution
	if err := fixture.db.Where("tenant_id = ? AND turn_id = ?", fixture.tenantID, turn.ID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	completeSessionExecutionForNextTurn(t, fixture, execution)

	_, err = fixture.service.CreateTurn(ctx, fixture.principal, fixture.sessionID, CreateTurnInput{
		InputText: "plan mode must be rejected", RuntimeMode: "approval-required", InteractionMode: "plan",
	}, "provider-capability-plan", "127.0.0.1")
	assertSessionProblemCode(t, err, providercapabilities.ReasonCapabilityUnsupported)
	var count int64
	if err := fixture.db.Model(&persistence.AgentTurn{}).
		Where("tenant_id = ? AND session_id = ? AND input_text = ?", fixture.tenantID, fixture.sessionID, "plan mode must be rejected").
		Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("unsupported Plan Turn persisted %d rows", count)
	}
}

func TestCapabilityGateRunsInsideIdempotentOperationAndDoesNotBreakReplay(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	sessionInput := CreateSessionInput{
		Title: "idempotent capability Session", Provider: "codex", ExecutionTargetID: &fixture.executionTargetID,
	}
	created, replayed, err := fixture.service.CreateWithIdempotency(
		ctx, fixture.principal, fixture.projectID, sessionInput, "capability-session-replay", "request-1", "127.0.0.1",
	)
	if err != nil || replayed {
		t.Fatalf("first Session create = %#v replayed=%t err=%v", created, replayed, err)
	}
	disableExperimentalProviders(t, fixture.db, fixture.executionTargetID)
	replayedSession, replayed, err := fixture.service.CreateWithIdempotency(
		ctx, fixture.principal, fixture.projectID, sessionInput, "capability-session-replay", "request-2", "127.0.0.1",
	)
	if err != nil || !replayed || replayedSession.ID != created.ID {
		t.Fatalf("Session replay = %#v replayed=%t err=%v", replayedSession, replayed, err)
	}

	enableExperimentalProviders(t, fixture.db, fixture.executionTargetID)
	turnInput := CreateTurnInput{InputText: "idempotent capability Turn", RuntimeMode: "full-access", InteractionMode: "default"}
	turn, replayed, err := fixture.service.CreateTurnWithIdempotency(
		ctx, fixture.principal, fixture.sessionID, turnInput, "capability-turn-replay", "request-3", "127.0.0.1",
	)
	if err != nil || replayed {
		t.Fatalf("first Turn create = %#v replayed=%t err=%v", turn, replayed, err)
	}
	disableExperimentalProviders(t, fixture.db, fixture.executionTargetID)
	replayedTurn, replayed, err := fixture.service.CreateTurnWithIdempotency(
		ctx, fixture.principal, fixture.sessionID, turnInput, "capability-turn-replay", "request-4", "127.0.0.1",
	)
	if err != nil || !replayed || replayedTurn.ID != turn.ID {
		t.Fatalf("Turn replay = %#v replayed=%t err=%v", replayedTurn, replayed, err)
	}
}

type sessionCapabilityManifestOptions struct {
	CodexCompatibilityStatus string
	CodexIncompatibilityCode string
	CodexPlanMode            string
}

func seedSessionCapabilityWorker(
	t *testing.T,
	fixture tenantExecutionPolicyFixture,
	options sessionCapabilityManifestOptions,
) {
	t.Helper()
	manifestID := uuid.New()
	now := time.Now().UTC()
	manifestDigest := sha256.Sum256([]byte("session-capability:" + manifestID.String()))
	if err := fixture.db.Create(&persistence.WorkerManifest{
		ID: manifestID, ManifestHash: hex.EncodeToString(manifestDigest[:]),
		WorkerBuildVersion: "session-capability-test", WorkerProtocolMinimum: 2, WorkerProtocolMaximum: 2,
		RuntimeEventMinimum: 2, RuntimeEventMaximum: 2, OperatingSystem: "linux", Architecture: "amd64",
		FeatureFlags: map[string]any{}, CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	for _, entry := range providercatalog.Providers() {
		capabilities := make(map[string]any, len(entry.Capabilities))
		for capabilityID, support := range entry.Capabilities {
			capabilities[capabilityID] = support
		}
		status := "local-only"
		code := providercapabilities.ReasonCapabilityUnsupported
		if entry.SupportTier != "local-only" {
			status = "compatible"
			code = ""
		}
		if entry.Name == "codex" {
			if options.CodexCompatibilityStatus != "" {
				status = options.CodexCompatibilityStatus
			}
			code = options.CodexIncompatibilityCode
			if options.CodexPlanMode != "" {
				capabilities["plan-mode"] = options.CodexPlanMode
			}
		}
		var codePointer, messagePointer *string
		if status != "compatible" {
			if code == "" {
				code = providercapabilities.ReasonCapabilityUnsupported
			}
			message := "Provider is not compatible for this test."
			codePointer, messagePointer = &code, &message
		}
		available := status == "compatible"
		version := entry.RuntimePolicy.CompatibleRange.MinimumInclusive
		descriptorDigest := sha256.Sum256([]byte(manifestID.String() + ":" + entry.Name))
		if err := fixture.db.Create(&persistence.WorkerProviderManifest{
			WorkerManifestID: manifestID, Provider: entry.Name, SupportTier: entry.SupportTier,
			CompatibilityStatus: status, ProviderHostMajor: 2, ProviderHostMinor: 1,
			HostBuildVersion: "host-test", AdapterVersion: entry.AdapterVersion,
			RuntimeKind: entry.RuntimePolicy.Kind, RuntimeName: entry.RuntimePolicy.Name, RuntimeVersion: &version,
			RuntimeAvailable: available, RuntimeVersionSource: entry.RuntimePolicy.VersionSource,
			RuntimeMinimumInclusive: entry.RuntimePolicy.CompatibleRange.MinimumInclusive,
			RuntimeCompatible:       available, ReleaseRequiresExplicitEnablement: entry.SupportTier == "experimental",
			ReleaseEnabled: true, MaximumCommandBytes: 1024, MaximumMessageBytes: 1024,
			RuntimeEventMinimum: 2, RuntimeEventMaximum: 2, CredentialDeliveryModes: []string{"anonymous-fd"},
			ResumeStrategies: []string{"authoritative-history"}, CapabilityDescriptorHash: hex.EncodeToString(descriptorDigest[:]),
			Capabilities: capabilities, IncompatibilityCode: codePointer, IncompatibilityMessage: messagePointer, CheckedAt: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := fixture.db.Create(&persistence.WorkerInstance{
		ID: uuid.New(), Incarnation: 1, InstanceUID: uuid.NewString(), ExecutionTargetID: fixture.executionTargetID,
		TargetKind: "local", ClusterID: uuid.NewString(), Namespace: "local", PodName: uuid.NewString(),
		Version: "session-capability-worker", ProtocolVersion: 2, Capabilities: map[string]any{},
		CurrentManifestID: &manifestID, CompatibilityStatus: "compatible", CompatibilityCheckedAt: &now,
		LeaseSupported: true, FencingSupported: true, AuthTokenHash: []byte(uuid.NewString()),
		Status: "online", RegisteredAt: now, LastHeartbeatAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
}

func disableExperimentalProviders(t *testing.T, db *gorm.DB, targetID uuid.UUID) {
	t.Helper()
	target := persistence.ExecutionTarget{
		Capabilities: map[string]any{"providerPolicy": map[string]any{"experimentalProviders": []string{}}},
	}
	if err := db.Model(&persistence.ExecutionTarget{}).Where("id = ?", targetID).
		Select("capabilities").Updates(&target).Error; err != nil {
		t.Fatal(err)
	}
}

func enableExperimentalProviders(t *testing.T, db *gorm.DB, targetID uuid.UUID) {
	t.Helper()
	target := persistence.ExecutionTarget{Capabilities: enabledProviderPolicyTestCapabilities()}
	if err := db.Model(&persistence.ExecutionTarget{}).Where("id = ?", targetID).
		Select("capabilities").Updates(&target).Error; err != nil {
		t.Fatal(err)
	}
}
