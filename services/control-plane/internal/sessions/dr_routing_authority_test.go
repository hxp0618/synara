package sessions

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/routing"
)

func TestCreateTurnBlocksCrossDomainRerouteWithoutFrozenSourceAuthority(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	sourceTarget, destinationTarget, _, _ := configureFailoverTargets(t, fixture)
	router := routing.NewService(fixture.db)
	now := time.Now().UTC().Add(2 * time.Second).Truncate(time.Second)

	if _, err := router.ObserveHealth(ctx, routing.HealthObservation{
		ExecutionTargetID: sourceTarget.ID, Status: routing.HealthHealthy, CapacityStatus: routing.CapacityAvailable,
		AvailableCapacityUnits: targetFailoverIntPointer(10), Source: "create-turn-dr-test", ObservedAt: now, TTL: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.CreateTurn(
		ctx,
		fixture.principal,
		fixture.sessionID,
		CreateTurnInput{InputText: "legacy-source-turn"},
		"legacy-source-turn",
		"127.0.0.1",
	); err != nil {
		t.Fatal(err)
	}

	sourceExecution := loadSessionExecution(t, fixture, "legacy-source-turn")
	completeSessionExecutionForNextTurn(t, fixture, sourceExecution)

	legacyTurnID := uuid.New()
	legacyQueuedAt := now.Add(time.Second)
	if err := fixture.db.Create(&persistence.AgentTurn{
		ID: legacyTurnID, TenantID: fixture.tenantID, SessionID: fixture.sessionID,
		CreatedBy: fixture.principal.UserID, Status: "completed", InputText: "legacy-frozen-source",
		CreatedAt: legacyQueuedAt, CompletedAt: &legacyQueuedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	provider := "codex"
	legacyExecution := persistence.AgentExecution{
		ID: uuid.New(), TenantID: fixture.tenantID, SessionID: fixture.sessionID, TurnID: legacyTurnID,
		Attempt: 2, Status: "completed", ExecutionTargetID: sourceTarget.ID, TargetKind: sourceTarget.Kind,
		Provider: &provider, ProviderResumeStrategySnapshot: "authoritative-history",
		WarmPoolModeSnapshot: "disabled", RequestedBy: fixture.principal.UserID, QueuedAt: legacyQueuedAt,
		FinishedAt: &legacyQueuedAt,
	}
	if err := fixture.db.Create(&legacyExecution).Error; err != nil {
		t.Fatal(err)
	}

	var workspace persistence.RemoteWorkspace
	if err := fixture.db.Where("tenant_id = ? AND session_id = ?", fixture.tenantID, fixture.sessionID).
		Take(&workspace).Error; err != nil {
		t.Fatal(err)
	}
	readyAt := now.Add(time.Minute)
	turnID := legacyTurnID
	checkpoint := persistence.WorkspaceCheckpoint{
		ID: uuid.New(), TenantID: fixture.tenantID, WorkspaceID: workspace.ID, SessionID: fixture.sessionID,
		TurnID: &turnID, ExecutionID: legacyExecution.ID, Generation: legacyExecution.Generation,
		IdempotencyKey: "legacy-source-checkpoint", Strategy: "manual", Status: "ready",
		CreatedAt: readyAt.Add(-time.Second), ReadyAt: &readyAt,
	}
	if err := fixture.db.Create(&checkpoint).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.RemoteWorkspace{}).
		Where("tenant_id = ? AND id = ?", fixture.tenantID, workspace.ID).
		Update("current_checkpoint_id", checkpoint.ID).Error; err != nil {
		t.Fatal(err)
	}

	if _, err := router.ObserveHealth(ctx, routing.HealthObservation{
		ExecutionTargetID: sourceTarget.ID, Status: routing.HealthUnreachable, CapacityStatus: routing.CapacityUnknown,
		Source: "create-turn-dr-test", ObservedAt: readyAt.Add(time.Second), TTL: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := router.ObserveHealth(ctx, routing.HealthObservation{
		ExecutionTargetID: destinationTarget.ID, Status: routing.HealthHealthy, CapacityStatus: routing.CapacityAvailable,
		AvailableCapacityUnits: targetFailoverIntPointer(10), Source: "create-turn-dr-test",
		ObservedAt: readyAt.Add(2 * time.Second), TTL: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}

	_, err := fixture.service.CreateTurn(
		ctx,
		fixture.principal,
		fixture.sessionID,
		CreateTurnInput{InputText: "must-fail-closed"},
		"must-fail-closed",
		"127.0.0.1",
	)
	assertSessionProblemCode(t, err, "session_routing_source_domain_missing")
}

func TestCreateTurnBlocksLegacyGroupRoutingWithoutFrozenSourceDomainEvenWhenTargetIsStillHealthy(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	sourceTarget, _, _, _ := configureFailoverTargets(t, fixture)
	router := routing.NewService(fixture.db)
	now := time.Now().UTC().Add(2 * time.Second).Truncate(time.Second)

	if _, err := router.ObserveHealth(ctx, routing.HealthObservation{
		ExecutionTargetID: sourceTarget.ID, Status: routing.HealthHealthy, CapacityStatus: routing.CapacityAvailable,
		AvailableCapacityUnits: targetFailoverIntPointer(10), Source: "create-turn-dr-test", ObservedAt: now, TTL: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.CreateTurn(
		ctx,
		fixture.principal,
		fixture.sessionID,
		CreateTurnInput{InputText: "seed-frozen-source"},
		"seed-frozen-source",
		"127.0.0.1",
	); err != nil {
		t.Fatal(err)
	}

	sourceExecution := loadSessionExecution(t, fixture, "seed-frozen-source")
	completeSessionExecutionForNextTurn(t, fixture, sourceExecution)

	legacyTurnID := uuid.New()
	legacyQueuedAt := now.Add(time.Second)
	if err := fixture.db.Create(&persistence.AgentTurn{
		ID: legacyTurnID, TenantID: fixture.tenantID, SessionID: fixture.sessionID,
		CreatedBy: fixture.principal.UserID, Status: "completed", InputText: "legacy-no-snapshot",
		CreatedAt: legacyQueuedAt, CompletedAt: &legacyQueuedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	provider := "codex"
	legacyExecution := persistence.AgentExecution{
		ID: uuid.New(), TenantID: fixture.tenantID, SessionID: fixture.sessionID, TurnID: legacyTurnID,
		Attempt: 2, Status: "completed", ExecutionTargetID: sourceTarget.ID, TargetKind: sourceTarget.Kind,
		Provider: &provider, ProviderResumeStrategySnapshot: "authoritative-history",
		WarmPoolModeSnapshot: "disabled", RequestedBy: fixture.principal.UserID, QueuedAt: legacyQueuedAt,
		FinishedAt: &legacyQueuedAt,
	}
	if err := fixture.db.Create(&legacyExecution).Error; err != nil {
		t.Fatal(err)
	}

	_, err := fixture.service.CreateTurn(
		ctx,
		fixture.principal,
		fixture.sessionID,
		CreateTurnInput{InputText: "must-block-before-reroute"},
		"must-block-before-reroute",
		"127.0.0.1",
	)
	assertSessionProblemCode(t, err, "session_routing_source_domain_missing")
}

func TestCreateTurnCrossDomainRerouteRequiresFreshArtifactAuthority(t *testing.T) {
	t.Run("artifacts unready blocks reroute", func(t *testing.T) {
		fixture, destinationTarget, sourceDRDomain, readyAt := seedCreateTurnArtifactAuthorityFixture(t)
		observeTargetDRReadiness(
			t,
			fixture,
			destinationTarget.ID,
			sourceDRDomain,
			routing.DRDomainForLocation("cn-beijing", "cluster-b"),
			readyAt,
			false,
			false,
			false,
			readyAt.Add(time.Second),
		)

		_, err := fixture.service.CreateTurn(
			context.Background(),
			fixture.principal,
			fixture.sessionID,
			CreateTurnInput{InputText: "artifact-authority-unready"},
			"artifact-authority-unready",
			"127.0.0.1",
		)
		assertSessionProblemCode(t, err, "target_group_dr_readiness_unready")
	})

	t.Run("stale watermark blocks reroute", func(t *testing.T) {
		fixture, destinationTarget, sourceDRDomain, readyAt := seedCreateTurnArtifactAuthorityFixture(t)
		observeTargetDRReadiness(
			t,
			fixture,
			destinationTarget.ID,
			sourceDRDomain,
			routing.DRDomainForLocation("cn-beijing", "cluster-b"),
			readyAt.Add(-time.Second),
			true,
			false,
			false,
			readyAt.Add(time.Second),
		)

		_, err := fixture.service.CreateTurn(
			context.Background(),
			fixture.principal,
			fixture.sessionID,
			CreateTurnInput{InputText: "artifact-authority-stale"},
			"artifact-authority-stale",
			"127.0.0.1",
		)
		assertSessionProblemCode(t, err, "target_group_dr_recovery_watermark_stale")
	})

	t.Run("fresh authority reroutes successfully", func(t *testing.T) {
		fixture, destinationTarget, sourceDRDomain, readyAt := seedCreateTurnArtifactAuthorityFixture(t)
		observeTargetDRReadiness(
			t,
			fixture,
			destinationTarget.ID,
			sourceDRDomain,
			routing.DRDomainForLocation("cn-beijing", "cluster-b"),
			readyAt,
			true,
			false,
			false,
			readyAt.Add(time.Second),
		)

		if _, err := fixture.service.CreateTurn(
			context.Background(),
			fixture.principal,
			fixture.sessionID,
			CreateTurnInput{InputText: "artifact-authority-fresh"},
			"artifact-authority-fresh",
			"127.0.0.1",
		); err != nil {
			t.Fatal(err)
		}

		execution := loadSessionExecution(t, fixture, "artifact-authority-fresh")
		if execution.ExecutionTargetID != destinationTarget.ID {
			t.Fatalf("execution target = %s, want %s", execution.ExecutionTargetID, destinationTarget.ID)
		}
	})
}

func seedCreateTurnArtifactAuthorityFixture(
	t *testing.T,
) (tenantExecutionPolicyFixture, persistence.ExecutionTarget, string, time.Time) {
	t.Helper()
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	sourceTarget, destinationTarget, _, _ := configureFailoverTargets(t, fixture)
	router := routing.NewService(fixture.db)
	now := time.Now().UTC().Add(2 * time.Second).Truncate(time.Second)

	if _, err := router.ObserveHealth(ctx, routing.HealthObservation{
		ExecutionTargetID: sourceTarget.ID, Status: routing.HealthHealthy, CapacityStatus: routing.CapacityAvailable,
		AvailableCapacityUnits: targetFailoverIntPointer(10), Source: "create-turn-dr-artifact-test", ObservedAt: now, TTL: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.service.CreateTurn(
		ctx,
		fixture.principal,
		fixture.sessionID,
		CreateTurnInput{InputText: "artifact-authority-seed"},
		"artifact-authority-seed",
		"127.0.0.1",
	); err != nil {
		t.Fatal(err)
	}

	sourceExecution := loadSessionExecution(t, fixture, "artifact-authority-seed")
	completeSessionExecutionForNextTurn(t, fixture, sourceExecution)

	readyAt := now.Add(time.Minute)
	artifact := createReadyExecutionArtifact(t, fixture, fixture.sessionID, &sourceExecution.ID, readyAt)
	appendArtifactReadyEvent(t, fixture, fixture.sessionID, &sourceExecution.ID, artifact.ID, readyAt)

	if _, err := router.ObserveHealth(ctx, routing.HealthObservation{
		ExecutionTargetID: sourceTarget.ID, Status: routing.HealthUnreachable, CapacityStatus: routing.CapacityUnknown,
		Source: "create-turn-dr-artifact-test", ObservedAt: readyAt.Add(2 * time.Second), TTL: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := router.ObserveHealth(ctx, routing.HealthObservation{
		ExecutionTargetID: destinationTarget.ID, Status: routing.HealthHealthy, CapacityStatus: routing.CapacityAvailable,
		AvailableCapacityUnits: targetFailoverIntPointer(10), Source: "create-turn-dr-artifact-test",
		ObservedAt: readyAt.Add(3 * time.Second), TTL: time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
	return fixture, destinationTarget, routing.DRDomainForLocation("cn-shanghai", "cluster-a"), readyAt
}

func createReadyExecutionArtifact(
	t *testing.T,
	fixture tenantExecutionPolicyFixture,
	sessionID uuid.UUID,
	executionID *uuid.UUID,
	readyAt time.Time,
) persistence.Artifact {
	t.Helper()
	contentType := "text/plain"
	sizeBytes := int64(32)
	sha256 := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	artifact := persistence.Artifact{
		ID: uuid.New(), TenantID: fixture.tenantID, OrganizationID: fixture.organizationID,
		ProjectID: fixture.projectID, SessionID: sessionID, ExecutionID: executionID,
		Kind: "generated_file", Status: "ready", Bucket: "dr-routing-tests",
		ObjectKey: "artifacts/" + uuid.NewString(), ContentType: &contentType,
		SizeBytes: &sizeBytes, SHA256: &sha256, CreatedByType: "system",
		CreatedByID: fixture.principal.UserID, ReadyAt: &readyAt, CreatedAt: readyAt,
	}
	if err := fixture.db.Create(&artifact).Error; err != nil {
		t.Fatal(err)
	}
	return artifact
}

func appendArtifactReadyEvent(
	t *testing.T,
	fixture tenantExecutionPolicyFixture,
	sessionID uuid.UUID,
	executionID *uuid.UUID,
	artifactID uuid.UUID,
	occurredAt time.Time,
) persistence.SessionEvent {
	t.Helper()
	var appended persistence.SessionEvent
	if err := fixture.db.Transaction(func(tx *gorm.DB) error {
		event, err := fixture.service.AppendInternalEvent(
			context.Background(),
			tx,
			fixture.tenantID,
			sessionID,
			InternalEventInput{
				EventType:   "artifact.ready",
				ActorType:   "system",
				ExecutionID: executionID,
				Payload:     map[string]any{"artifactId": artifactID, "kind": "generated_file"},
				OccurredAt:  &occurredAt,
			},
		)
		if err == nil {
			appended = event
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return appended
}

func observeTargetDRReadiness(
	t *testing.T,
	fixture tenantExecutionPolicyFixture,
	targetID uuid.UUID,
	sourceDRDomain string,
	drDomain string,
	replicatedThroughAt time.Time,
	artifactsReady, checkpointsReady, memoryReady bool,
	observedAt time.Time,
) {
	t.Helper()
	if _, err := routing.NewService(fixture.db).ObserveDRReadiness(context.Background(), routing.DRReadinessObservation{
		ExecutionTargetID:   targetID,
		SourceDRDomain:      sourceDRDomain,
		DRDomain:            drDomain,
		ReplicatedThroughAt: replicatedThroughAt,
		ArtifactsReady:      artifactsReady,
		CheckpointsReady:    checkpointsReady,
		MemoryReady:         memoryReady,
		PublisherIdentity:   "test-dr-publisher",
		ObservedAt:          observedAt,
		TTL:                 time.Minute,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestBundleArtifactAuthorityExpectationsResolvesForkAncestorOutsideCurrentTail(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()
	ancestorSessionID := uuid.New()
	prefixSequence := int64(3)
	strategy := "emulated"
	createAuthoritySession(t, fixture, ancestorSessionID, fixture.executionTargetID, nil, nil, prefixSequence)
	childSession := createForkAuthoritySession(
		t,
		fixture,
		fixture.executionTargetID,
		nil,
		nil,
		nil,
		ancestorSessionID,
		prefixSequence,
		strategy,
		504,
	)

	ancestorExecutionID := uuid.New()
	provider := "codex"
	now := time.Now().UTC().Truncate(time.Second)
	if err := fixture.db.Create(&persistence.AgentExecution{
		ID: ancestorExecutionID, TenantID: fixture.tenantID, SessionID: ancestorSessionID, TurnID: uuid.New(),
		Attempt: 1, Status: "completed", ExecutionTargetID: fixture.executionTargetID, TargetKind: "kubernetes",
		Provider: &provider, RequestedBy: fixture.principal.UserID, QueuedAt: now, FinishedAt: &now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	ancestorArtifact := createReadyExecutionArtifact(t, fixture, ancestorSessionID, &ancestorExecutionID, now)
	ancestorEvents := []persistence.SessionEvent{
		{
			TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, ProjectID: fixture.projectID,
			SessionID: ancestorSessionID, Sequence: 1, EventID: uuid.New(), EventVersion: 1,
			EventType: "content.delta", ActorType: "worker",
			Payload: map[string]any{"streamKind": "assistant_text", "delta": "a"}, OccurredAt: now,
		},
		{
			TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, ProjectID: fixture.projectID,
			SessionID: ancestorSessionID, Sequence: 2, EventID: uuid.New(), EventVersion: 1,
			EventType: "artifact.ready", ActorType: "system", ExecutionID: &ancestorExecutionID,
			Payload: map[string]any{"artifactId": ancestorArtifact.ID, "kind": "generated_file"}, OccurredAt: now.Add(time.Second),
		},
		{
			TenantID: fixture.tenantID, OrganizationID: fixture.organizationID, ProjectID: fixture.projectID,
			SessionID: ancestorSessionID, Sequence: 3, EventID: uuid.New(), EventVersion: 1,
			EventType: "content.delta", ActorType: "worker",
			Payload: map[string]any{"streamKind": "assistant_text", "delta": "b"}, OccurredAt: now.Add(2 * time.Second),
		},
	}
	if err := fixture.db.Create(&ancestorEvents).Error; err != nil {
		t.Fatal(err)
	}

	createLogicalHistoryEvents(t, fixture.db, fixture.tenantID, childSession.ID, 4, 504)

	tail, err := loadResumeArtifactExpectations(ctx, fixture.db, fixture.tenantID, childSession.ID, 504)
	if err != nil {
		t.Fatal(err)
	}
	for _, expectation := range tail {
		if expectation.ArtifactID == ancestorArtifact.ID {
			t.Fatalf("aged-out ancestor artifact unexpectedly remained in current 500-tail: %#v", tail)
		}
	}

	expectations, err := bundleArtifactAuthorityExpectations(
		ctx,
		fixture.db,
		fixture.tenantID,
		childSession.ID,
		504,
		[]failoverBundleArtifactReference{{
			Sequence:    2,
			ArtifactID:  ancestorArtifact.ID,
			ExecutionID: &ancestorExecutionID,
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(expectations) != 1 || expectations[0].SessionID != ancestorSessionID ||
		expectations[0].ExecutionID == nil || *expectations[0].ExecutionID != ancestorExecutionID {
		t.Fatalf("bundle expectations = %#v", expectations)
	}
	watermark, err := loadReadyArtifactAuthorityWatermark(
		ctx,
		fixture.db,
		fixture.tenantID,
		expectations,
		"load",
		"load",
		"missing",
		"missing",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !watermark.Equal(now.UTC()) {
		t.Fatalf("watermark = %s, want %s", watermark, now.UTC())
	}
}

func TestBundleArtifactAuthorityExpectationsFailClosedForMalformedOrConflictingRefs(t *testing.T) {
	fixture := newTenantExecutionPolicyFixture(t)
	ctx := context.Background()

	artifactAt := time.Now().UTC().Truncate(time.Second)
	executionID := uuid.New()
	provider := "codex"
	if err := fixture.db.Create(&persistence.AgentExecution{
		ID: executionID, TenantID: fixture.tenantID, SessionID: fixture.sessionID, TurnID: uuid.New(),
		Attempt: 1, Status: "completed", ExecutionTargetID: fixture.executionTargetID, TargetKind: "kubernetes",
		Provider: &provider, RequestedBy: fixture.principal.UserID, QueuedAt: artifactAt, FinishedAt: &artifactAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	artifact := createReadyExecutionArtifact(t, fixture, fixture.sessionID, &executionID, artifactAt)
	event := appendArtifactReadyEvent(t, fixture, fixture.sessionID, &executionID, artifact.ID, artifactAt)

	t.Run("nil artifact id", func(t *testing.T) {
		_, err := bundleArtifactAuthorityExpectations(
			ctx,
			fixture.db,
			fixture.tenantID,
			fixture.sessionID,
			event.Sequence,
			[]failoverBundleArtifactReference{{Sequence: event.Sequence}},
		)
		assertSessionProblemCode(t, err, "target_failover_bundle_authority_invalid")
	})

	t.Run("missing sequence", func(t *testing.T) {
		_, err := bundleArtifactAuthorityExpectations(
			ctx,
			fixture.db,
			fixture.tenantID,
			fixture.sessionID,
			event.Sequence,
			[]failoverBundleArtifactReference{{ArtifactID: artifact.ID, ExecutionID: &executionID}},
		)
		assertSessionProblemCode(t, err, "target_failover_bundle_authority_invalid")
	})

	t.Run("conflicting execution ids", func(t *testing.T) {
		conflictExecutionID := uuid.New()
		_, err := bundleArtifactAuthorityExpectations(
			ctx,
			fixture.db,
			fixture.tenantID,
			fixture.sessionID,
			event.Sequence,
			[]failoverBundleArtifactReference{
				{Sequence: event.Sequence, ArtifactID: artifact.ID, ExecutionID: &executionID},
				{Sequence: event.Sequence, ArtifactID: artifact.ID, ExecutionID: &conflictExecutionID},
			},
		)
		assertSessionProblemCode(t, err, "target_failover_bundle_authority_invalid")
	})
}

func createForkAuthoritySession(
	t *testing.T,
	fixture tenantExecutionPolicyFixture,
	executionTargetID uuid.UUID,
	requestedExecutionTargetID *uuid.UUID,
	targetGroupID *uuid.UUID,
	routingPolicyVersion *int64,
	ancestorSessionID uuid.UUID,
	prefixSequence int64,
	strategy string,
	lastEventSequence int64,
) persistence.AgentSession {
	t.Helper()
	now := time.Now().UTC()
	sessionID := uuid.New()
	requestedTarget := executionTargetID
	if requestedExecutionTargetID != nil {
		requestedTarget = *requestedExecutionTargetID
	}
	session := persistence.AgentSession{
		ID: sessionID, TenantID: fixture.tenantID, OrganizationID: fixture.organizationID,
		ProjectID: fixture.projectID, CreatedBy: fixture.principal.UserID, Title: "Fork authority child",
		Status: "active", Visibility: "private", Provider: "codex",
		ExecutionTargetID: executionTargetID, RequestedExecutionTargetID: requestedTarget,
		ExecutionTargetGroupID:  targetGroupID,
		RoutingPolicyVersion:    routingPolicyVersion,
		ForkSourceSessionID:     &ancestorSessionID,
		ForkSourceEventSequence: &prefixSequence,
		ForkStrategy:            &strategy,
		LastEventSequence:       lastEventSequence,
		ResourceState:           "idle",
		CreatedAt:               now,
		UpdatedAt:               now,
	}
	if err := fixture.db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	return session
}

func createAuthoritySession(
	t *testing.T,
	fixture tenantExecutionPolicyFixture,
	sessionID uuid.UUID,
	executionTargetID uuid.UUID,
	requestedExecutionTargetID *uuid.UUID,
	targetGroupID *uuid.UUID,
	lastEventSequence int64,
) persistence.AgentSession {
	t.Helper()
	now := time.Now().UTC()
	requestedTarget := executionTargetID
	if requestedExecutionTargetID != nil {
		requestedTarget = *requestedExecutionTargetID
	}
	session := persistence.AgentSession{
		ID: sessionID, TenantID: fixture.tenantID, OrganizationID: fixture.organizationID,
		ProjectID: fixture.projectID, CreatedBy: fixture.principal.UserID, Title: "Authority root",
		Status: "active", Visibility: "private", Provider: "codex",
		ExecutionTargetID: executionTargetID, RequestedExecutionTargetID: requestedTarget,
		ExecutionTargetGroupID: targetGroupID,
		LastEventSequence:      lastEventSequence,
		ResourceState:          "idle",
		CreatedAt:              now,
		UpdatedAt:              now,
	}
	if err := fixture.db.Create(&session).Error; err != nil {
		t.Fatal(err)
	}
	return session
}
