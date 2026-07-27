package sessions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/placement"
	"github.com/synara-ai/synara/services/control-plane/internal/providercapabilities"
	"github.com/synara-ai/synara/services/control-plane/internal/providercatalog"
	"github.com/synara-ai/synara/services/control-plane/internal/routing"
	"github.com/synara-ai/synara/services/control-plane/internal/schedulingdecision"
)

func TestSelectExecutionLaunchTargetRechecksCapabilitiesAfterPlacementLock(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	var target persistence.ExecutionTarget
	if err := fixture.db.Where("id = ?", fixture.executionTargetID).Take(&target).Error; err != nil {
		t.Fatal(err)
	}

	staleCapability := errors.New("capability projection changed after preview")
	gateCalls := 0
	var previewPoolID uuid.UUID
	err := fixture.db.Transaction(func(tx *gorm.DB) error {
		_, selectErr := SelectExecutionLaunchTarget(
			context.Background(),
			tx,
			&target,
			nil,
			ExecutionLaunchPolicyScope{
				TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, Provider: "codex",
			},
			placement.WarmPoolModeDefault,
			func(
				_ context.Context,
				_ *gorm.DB,
				_ persistence.ExecutionTarget,
				selection placement.Selection,
				_ *routing.Selection,
			) error {
				gateCalls++
				if gateCalls == 1 {
					previewPoolID = selection.Pool.ID
					return nil
				}
				if selection.Pool.ID != previewPoolID {
					t.Fatalf("final pool = %s, want unchanged preview pool %s", selection.Pool.ID, previewPoolID)
				}
				return staleCapability
			},
		)
		return selectErr
	})
	if !errors.Is(err, staleCapability) {
		t.Fatalf("selection error = %v, want stale capability rejection", err)
	}
	if gateCalls != 2 {
		t.Fatalf("capability gate calls = %d, want preview and post-lock validation", gateCalls)
	}
}

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

func TestCreateSessionTargetGroupRetriesPastUnsupportedPreferredTarget(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	source := loadCapabilityRouteTarget(t, fixture.db, fixture.executionTargetID)
	destination := createCapabilityRouteTargetCopy(t, fixture.db, source, "capability-route-destination")
	sourcePool := configureWarmDefaultPool(t, fixture.db, source)
	destinationPool := configureWarmDefaultPool(t, fixture.db, destination)
	seedTargetCapabilityWorker(t, fixture.db, source.ID, &sourcePool.ID, &sourcePool.Version, sessionCapabilityManifestOptions{
		CodexStartSession: "unsupported",
	})
	seedTargetCapabilityWorker(t, fixture.db, destination.ID, &destinationPool.ID, &destinationPool.Version, sessionCapabilityManifestOptions{})
	group := configureCapabilityRouteGroup(t, fixture, source, destination)

	created, err := fixture.service.Create(context.Background(), fixture.principal, fixture.projectID, CreateSessionInput{
		Title:                  "retry past unsupported preferred target",
		Provider:               "codex",
		ExecutionTargetID:      &source.ID,
		ExecutionTargetGroupID: &group.ID,
	}, "provider-capability-group-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if created.ExecutionTargetID != destination.ID {
		t.Fatalf("created execution target = %s, want %s", created.ExecutionTargetID, destination.ID)
	}
	if created.ExecutionTargetGroupID == nil || *created.ExecutionTargetGroupID != group.ID {
		t.Fatalf("created target group = %#v, want %s", created.ExecutionTargetGroupID, group.ID)
	}
	if created.RoutingPolicyVersion == nil || *created.RoutingPolicyVersion != group.Version {
		t.Fatalf("created routing policy version = %#v, want %d", created.RoutingPolicyVersion, group.Version)
	}
}

func TestCreateSessionTargetGroupUsesProviderAffinityWhenNoPreferredTargetIsPinned(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	source := loadCapabilityRouteTarget(t, fixture.db, fixture.executionTargetID)
	destination := createCapabilityRouteTargetCopy(t, fixture.db, source, "capability-route-affinity-destination")
	setCapabilityRouteTargetRoutingPreferences(t, fixture.db, source.ID, map[string]string{"codex": "avoid"})
	setCapabilityRouteTargetRoutingPreferences(t, fixture.db, destination.ID, map[string]string{"codex": "prefer"})
	sourcePool := configureWarmDefaultPool(t, fixture.db, source)
	destinationPool := configureWarmDefaultPool(t, fixture.db, destination)
	seedTargetCapabilityWorker(t, fixture.db, source.ID, &sourcePool.ID, &sourcePool.Version, sessionCapabilityManifestOptions{})
	seedTargetCapabilityWorker(t, fixture.db, destination.ID, &destinationPool.ID, &destinationPool.Version, sessionCapabilityManifestOptions{})
	group := configureCapabilityRouteGroup(t, fixture, source, destination)

	created, err := fixture.service.Create(context.Background(), fixture.principal, fixture.projectID, CreateSessionInput{
		Title:                  "provider affinity",
		Provider:               "codex",
		ExecutionTargetGroupID: &group.ID,
	}, "provider-affinity-group-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if created.ExecutionTargetID != destination.ID {
		t.Fatalf("created execution target = %s, want %s", created.ExecutionTargetID, destination.ID)
	}
}

func TestCreateTurnTargetGroupRetriesPastUnsupportedPreferredTarget(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	source := loadCapabilityRouteTarget(t, fixture.db, fixture.executionTargetID)
	destination := createCapabilityRouteTargetCopy(t, fixture.db, source, "capability-route-turn-destination")
	sourcePool := configureWarmDefaultPool(t, fixture.db, source)
	destinationPool := configureWarmDefaultPool(t, fixture.db, destination)
	seedTargetCapabilityWorker(t, fixture.db, source.ID, &sourcePool.ID, &sourcePool.Version, sessionCapabilityManifestOptions{
		CodexSendTurn: "unsupported",
	})
	seedTargetCapabilityWorker(t, fixture.db, destination.ID, &destinationPool.ID, &destinationPool.Version, sessionCapabilityManifestOptions{})
	group := configureCapabilityRouteGroup(t, fixture, source, destination)
	if err := fixture.db.Model(&persistence.AgentSession{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).
		Updates(map[string]any{
			"requested_execution_target_id": source.ID,
			"execution_target_group_id":     group.ID,
			"routing_policy_version":        group.Version,
			"execution_target_id":           source.ID,
		}).Error; err != nil {
		t.Fatal(err)
	}

	turn, err := fixture.service.CreateTurn(context.Background(), fixture.principal, fixture.sessionID, CreateTurnInput{
		InputText: "retry to supported target",
	}, "provider-capability-group-turn", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	var execution persistence.AgentExecution
	if err := fixture.db.Where("tenant_id = ? AND turn_id = ?", fixture.tenantID, turn.ID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	if execution.ExecutionTargetID != destination.ID {
		t.Fatalf("execution target = %s, want %s", execution.ExecutionTargetID, destination.ID)
	}
	if execution.SchedulingDecisionID == nil {
		t.Fatal("routed Execution omitted its immutable scheduling decision identity")
	}
	var decision persistence.ExecutionSchedulingDecision
	if err := fixture.db.Where("tenant_id = ? AND execution_id = ?", fixture.tenantID, execution.ID).
		Take(&decision).Error; err != nil {
		t.Fatal(err)
	}
	if decision.ID != *execution.SchedulingDecisionID || decision.AlgorithmVersion != "reservation-aware-v1" ||
		decision.EvidenceCompleteness != "complete" || decision.CandidateCount != 2 ||
		decision.SelectedExecutionTargetID != destination.ID {
		t.Fatalf("routed scheduling decision = %#v", decision)
	}
	var candidates []persistence.ExecutionSchedulingCandidate
	if err := fixture.db.Where("tenant_id = ? AND decision_id = ?", fixture.tenantID, decision.ID).
		Order("ordinal ASC").Find(&candidates).Error; err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 {
		t.Fatalf("routed scheduling candidates = %#v", candidates)
	}
	candidateByTarget := make(map[uuid.UUID]persistence.ExecutionSchedulingCandidate, len(candidates))
	for _, candidate := range candidates {
		candidateByTarget[candidate.ExecutionTargetID] = candidate
	}
	candidate := candidateByTarget[destination.ID]
	rejected := candidateByTarget[source.ID]
	var member persistence.ExecutionTargetGroupMember
	if err := fixture.db.Where("tenant_id = ? AND target_group_id = ? AND execution_target_id = ?",
		fixture.tenantID, group.ID, destination.ID).Take(&member).Error; err != nil {
		t.Fatal(err)
	}
	var health persistence.ExecutionTargetHealth
	if err := fixture.db.Where("execution_target_id = ?", destination.ID).Take(&health).Error; err != nil {
		t.Fatal(err)
	}
	if !candidate.Selected || candidate.TargetGroupMemberID == nil || *candidate.TargetGroupMemberID != member.ID ||
		candidate.HealthVersion == nil || *candidate.HealthVersion != health.Version ||
		candidate.QueuedExecutionUnits == nil || candidate.EffectiveLoadRank == nil ||
		candidate.Priority == nil || *candidate.Priority != member.Priority ||
		candidate.Weight == nil || *candidate.Weight != member.Weight ||
		candidate.PriorityRank == nil || *candidate.PriorityRank != 0 ||
		candidate.WorkerPoolID == nil || *candidate.WorkerPoolID != destinationPool.ID ||
		rejected.Selected || rejected.Eligibility != schedulingdecision.EligibilityRejected ||
		rejected.RejectionCode == nil || *rejected.RejectionCode != "capability_unsupported" ||
		rejected.WorkerPoolID == nil || *rejected.WorkerPoolID != sourcePool.ID {
		t.Fatalf("routed scheduling candidate = %#v, member = %#v, health = %#v", candidate, member, health)
	}
	var capacityAdmission persistence.ExecutionCapacityAdmission
	if err := fixture.db.Where("tenant_id = ? AND execution_id = ?", fixture.tenantID, execution.ID).
		Take(&capacityAdmission).Error; err != nil {
		t.Fatal(err)
	}
	if capacityAdmission.AdmissionMode != routing.CapacityAdmissionExactActiveV1 ||
		capacityAdmission.UnacknowledgedReservationUnits == nil ||
		capacityAdmission.SnapshotSHA256 != routing.CapacityAdmissionSHA256(capacityAdmission) {
		t.Fatalf("routed capacity admission = %#v", capacityAdmission)
	}
	var session persistence.AgentSession
	if err := fixture.db.Where("tenant_id = ? AND id = ?", fixture.tenantID, fixture.sessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	if session.ExecutionTargetID != destination.ID {
		t.Fatalf("session execution target = %s, want %s", session.ExecutionTargetID, destination.ID)
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

func TestSwitchModelCapabilityGateRequiresObservedSupportedModelSwitch(t *testing.T) {
	t.Run("unobserved", func(t *testing.T) {
		fixture := newTenantExecutionPolicyFixture(t)
		_, err := fixture.service.SwitchModel(
			context.Background(), fixture.principal, fixture.sessionID,
			SwitchModelInput{Model: "gpt-5.6", ExpectedModelProvided: true},
			"model-switch-unobserved", "127.0.0.1",
		)
		assertSessionProblemCode(t, err, providercapabilities.ReasonWorkerManifestRequired)
		assertCount(t, fixture, &persistence.SessionEvent{},
			"tenant_id = ? AND session_id = ? AND event_type = ?", 0,
			fixture.tenantID, fixture.sessionID, "session.model.changed")
	})

	t.Run("unsupported", func(t *testing.T) {
		fixture := newTenantExecutionPolicyFixture(t)
		seedSessionCapabilityWorker(t, fixture, sessionCapabilityManifestOptions{
			CodexModelSwitch: "unsupported",
		})
		_, err := fixture.service.SwitchModel(
			context.Background(), fixture.principal, fixture.sessionID,
			SwitchModelInput{Model: "gpt-5.6", ExpectedModelProvided: true},
			"model-switch-unsupported", "127.0.0.1",
		)
		assertSessionProblemCode(t, err, providercapabilities.ReasonCapabilityUnsupported)
		assertCount(t, fixture, &persistence.SessionEvent{},
			"tenant_id = ? AND session_id = ? AND event_type = ?", 0,
			fixture.tenantID, fixture.sessionID, "session.model.changed")
	})
}

type sessionCapabilityManifestOptions struct {
	CodexCompatibilityStatus string
	CodexIncompatibilityCode string
	CodexPlanMode            string
	CodexModelSwitch         string
	CodexStartSession        string
	CodexSendTurn            string
	CodexReview              string
	CodexCompact             string
}

func seedSessionCapabilityWorker(
	t *testing.T,
	fixture tenantExecutionPolicyFixture,
	options sessionCapabilityManifestOptions,
) uuid.UUID {
	t.Helper()
	return seedTargetCapabilityWorker(t, fixture.db, fixture.executionTargetID, nil, nil, options)
}

func seedTargetCapabilityWorker(
	t *testing.T,
	db *gorm.DB,
	targetID uuid.UUID,
	poolID *uuid.UUID,
	poolVersion *int64,
	options sessionCapabilityManifestOptions,
) uuid.UUID {
	t.Helper()
	manifestID := uuid.New()
	now := time.Now().UTC()
	manifestDigest := sha256.Sum256([]byte("session-capability:" + manifestID.String()))
	if err := db.Create(&persistence.WorkerManifest{
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
			if options.CodexModelSwitch != "" {
				capabilities["model-switch"] = options.CodexModelSwitch
			}
			if options.CodexStartSession != "" {
				capabilities["start-session"] = options.CodexStartSession
			}
			if options.CodexSendTurn != "" {
				capabilities["send-turn"] = options.CodexSendTurn
			}
			if options.CodexReview != "" {
				capabilities["review"] = options.CodexReview
			}
			if options.CodexCompact != "" {
				capabilities["compact"] = options.CodexCompact
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
		if err := db.Create(&persistence.WorkerProviderManifest{
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
	target := loadCapabilityRouteTarget(t, db, targetID)
	worker := persistence.WorkerInstance{
		ID: uuid.New(), Incarnation: 1, InstanceUID: uuid.NewString(), ExecutionTargetID: targetID,
		TargetKind: target.Kind, WorkerMode: "general-pool", ClusterID: uuid.NewString(), Namespace: "local", PodName: uuid.NewString(),
		Version: "session-capability-worker", ProtocolVersion: 2, Capabilities: map[string]any{},
		CurrentManifestID: &manifestID, CompatibilityStatus: "compatible", CompatibilityCheckedAt: &now,
		LeaseSupported: true, FencingSupported: true, AuthTokenHash: []byte(uuid.NewString()),
		Status: "online", RegisteredAt: now, LastHeartbeatAt: now,
	}
	if poolID != nil {
		worker.WorkerPoolID = poolID
		worker.WorkerMode = "warm-pool"
	}
	if poolVersion != nil {
		worker.WorkerPoolVersion = poolVersion
	}
	if worker.WorkerMode == "warm-pool" {
		var pool persistence.WorkerPool
		if err := db.Where("id = ?", *poolID).Take(&pool).Error; err != nil {
			t.Fatal(err)
		}
		worker.CapacityClass = &pool.CapacityClass
	}
	if err := db.Create(&worker).Error; err != nil {
		t.Fatal(err)
	}
	return manifestID
}

func loadCapabilityRouteTarget(t *testing.T, db *gorm.DB, targetID uuid.UUID) persistence.ExecutionTarget {
	t.Helper()
	var target persistence.ExecutionTarget
	if err := db.Where("id = ?", targetID).Take(&target).Error; err != nil {
		t.Fatal(err)
	}
	return target
}

func createCapabilityRouteTargetCopy(
	t *testing.T,
	db *gorm.DB,
	source persistence.ExecutionTarget,
	name string,
) persistence.ExecutionTarget {
	t.Helper()
	copy := source
	copy.ID = uuid.New()
	copy.Name = name + "-" + uuid.NewString()
	copy.Status = "active"
	if err := db.Create(&copy).Error; err != nil {
		t.Fatal(err)
	}
	return copy
}

func setCapabilityRouteTargetRoutingPreferences(
	t *testing.T,
	db *gorm.DB,
	targetID uuid.UUID,
	preferences map[string]string,
) {
	t.Helper()
	target := loadCapabilityRouteTarget(t, db, targetID)
	capabilities := make(map[string]any, len(target.Capabilities))
	for key, value := range target.Capabilities {
		capabilities[key] = value
	}
	rawPolicy, _ := capabilities["providerPolicy"].(map[string]any)
	providerPolicy := make(map[string]any, len(rawPolicy)+1)
	for key, value := range rawPolicy {
		providerPolicy[key] = value
	}
	if len(preferences) == 0 {
		delete(providerPolicy, "routingPreferences")
	} else {
		routingPreferences := make(map[string]any, len(preferences))
		for provider, preference := range preferences {
			routingPreferences[provider] = preference
		}
		providerPolicy["routingPreferences"] = routingPreferences
	}
	capabilities["providerPolicy"] = providerPolicy
	if err := db.Model(&persistence.ExecutionTarget{}).
		Where("id = ?", targetID).
		Select("capabilities").
		Updates(&persistence.ExecutionTarget{Capabilities: capabilities}).Error; err != nil {
		t.Fatal(err)
	}
}

func ensureTargetDefaultPlacement(
	t *testing.T,
	db *gorm.DB,
	target persistence.ExecutionTarget,
) placement.Selection {
	t.Helper()
	selected, err := placement.NewService(db).SelectExecution(context.Background(), db, target, placement.WarmPoolModeDisabled)
	if err != nil {
		t.Fatal(err)
	}
	return selected
}

func configureWarmDefaultPool(
	t *testing.T,
	db *gorm.DB,
	target persistence.ExecutionTarget,
) persistence.WorkerPool {
	t.Helper()
	ensureTargetDefaultPlacement(t, db, target)
	now := time.Now().UTC()
	pool := persistence.WorkerPool{
		ID: uuid.New(), TenantID: target.TenantID, ExecutionTargetID: target.ID,
		Name: "warm-default-" + uuid.NewString(), Mode: placement.PoolModeWarm, CapacityClass: placement.CapacityClassInteractive,
		DesiredIdleUnits: 0, MaxActiveUnits: 1, SchedulingTemplate: map[string]any{},
		Status: placement.PoolStatusActive, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&pool).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.ExecutionPlacementPolicy{}).
		Where("execution_target_id = ?", target.ID).
		Updates(map[string]any{
			"default_pool_id": pool.ID,
			"version":         gorm.Expr("version + 1"),
			"updated_at":      now,
		}).Error; err != nil {
		t.Fatal(err)
	}
	return pool
}

func configureCapabilityRouteGroup(
	t *testing.T,
	fixture tenantExecutionPolicyFixture,
	source persistence.ExecutionTarget,
	destination persistence.ExecutionTarget,
) persistence.ExecutionTargetGroup {
	t.Helper()
	ctx := context.Background()
	router := routing.NewService(fixture.db)
	group, err := router.CreateGroup(ctx, routing.CreateGroupInput{
		TenantID: fixture.tenantID, OrganizationID: &fixture.organizationID,
		Name: "capability-route-group-" + uuid.NewString(), Strategy: routing.StrategyPriority,
		AllowCrossRegion: true, MaxFailoverAttempts: targetFailoverIntPointer(3), HealthMaxStalenessSeconds: 90,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := router.AddMember(ctx, routing.AddMemberInput{
		TenantID: fixture.tenantID, TargetGroupID: group.ID, ExecutionTargetID: source.ID,
		Region: "cn-shanghai", ClusterID: "cluster-a", Priority: 10, Weight: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := router.AddMember(ctx, routing.AddMemberInput{
		TenantID: fixture.tenantID, TargetGroupID: group.ID, ExecutionTargetID: destination.ID,
		Region: "cn-shanghai", ClusterID: "cluster-b", Priority: 20, Weight: 100,
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	capacity := 10
	for _, targetID := range []uuid.UUID{source.ID, destination.ID} {
		if _, err := router.ObserveHealth(ctx, routing.HealthObservation{
			ExecutionTargetID:      targetID,
			Status:                 routing.HealthHealthy,
			CapacityStatus:         routing.CapacityAvailable,
			AvailableCapacityUnits: &capacity,
			ReservationAuthority: &routing.ReservationAuthorityObservation{
				Mode:             routing.ReservationAuthorityExactActiveV1,
				Acknowledgements: []routing.ReservationIdentity{},
			},
			Source:     "provider-capability-route-test",
			ObservedAt: now,
			TTL:        time.Minute,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return group
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
