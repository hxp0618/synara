package executions

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
)

func TestDisasterRecoveryBundleLinksDeclaredPredecessorExecutionAttempt(t *testing.T) {
	ctx := context.Background()
	db, _, fixture := setupSQLiteRecoveryService(t)
	now := time.Now().UTC()
	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).
		Updates(map[string]any{"status": "leased", "generation": 1}).Error; err != nil {
		t.Fatal(err)
	}
	sourceBundle := persistence.ExecutionRecoveryBundle{
		ID: uuid.New(), TenantID: fixture.TenantID, SessionID: fixture.SessionID, TurnID: fixture.TurnID,
		ExecutionID: fixture.ExecutionID, Generation: 1, SchemaVersion: 1, RecoveryReason: "initial-claim",
		Payload: map[string]any{}, PayloadSHA256: strings.Repeat("a", 64), CreatedAt: now,
	}
	if err := db.Create(&sourceBundle).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).
		Updates(map[string]any{"status": "interrupted", "finished_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	recoveryReason := "disaster-recovery"
	destination := persistence.AgentExecution{
		ID: uuid.New(), TenantID: fixture.TenantID, SessionID: fixture.SessionID, TurnID: fixture.TurnID,
		Attempt: 2, Status: "recovering", ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind,
		PredecessorExecutionID: &fixture.ExecutionID, NextRecoveryReason: &recoveryReason,
		Generation: 1, RequestedBy: fixture.UserID, QueuedAt: now,
	}
	if err := db.Create(&destination).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", fixture.TenantID, destination.ID).
		Updates(map[string]any{"status": "leased", "next_recovery_reason": nil}).Error; err != nil {
		t.Fatal(err)
	}
	destination.Status = "leased"
	reason, predecessorID, err := resolveRecoveryBundleLineage(ctx, db, destination)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "disaster-recovery" || predecessorID == nil || *predecessorID != sourceBundle.ID {
		t.Fatalf("lineage reason=%q predecessor=%v", reason, predecessorID)
	}
	destinationBundle := persistence.ExecutionRecoveryBundle{
		ID: uuid.New(), TenantID: fixture.TenantID, SessionID: fixture.SessionID, TurnID: fixture.TurnID,
		ExecutionID: destination.ID, Generation: 1, SchemaVersion: 1, RecoveryReason: reason,
		PreviousBundleID: predecessorID, Payload: map[string]any{}, PayloadSHA256: strings.Repeat("b", 64), CreatedAt: now,
	}
	if err := db.Create(&destinationBundle).Error; err != nil {
		t.Fatalf("insert cross-attempt Recovery Bundle: %v", err)
	}
}

func TestClaimFreezesImmutableRecoveryBundleAndLinksRecoveredGeneration(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	memoryReference := RecoveryMemoryReference{
		Scope: "session", ScopeID: fixture.SessionID, HeadID: uuid.New(), MemoryKey: "instructions",
		RevisionID: uuid.New(), ArtifactID: uuid.New(),
		SHA256:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		MediaType: "text/plain; charset=utf-8", SizeBytes: 128,
	}
	resolver := &staticMemoryReferenceResolver{references: []RecoveryMemoryReference{memoryReference}}
	service.memoryReferences = resolver
	firstWorker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "bundle-first")
	secondWorker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "bundle-second")
	cleanupWorkers(t, db, firstWorker.ID, secondWorker.ID)

	first, err := service.Claim(ctx, firstWorker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind,
		ExecutionID: &fixture.ExecutionID,
	}, "recovery-bundle-first-claim")
	if err != nil || first.Value.Execution == nil || first.Value.Lease == nil || first.Value.Workload == nil {
		t.Fatalf("first claim = %#v, %v", first, err)
	}
	firstBundle := first.Value.Workload.RecoveryBundle
	if firstBundle == nil || firstBundle.SchemaVersion != RecoveryBundleSchemaVersionV1 ||
		firstBundle.RecoveryReason != "initial-claim" || firstBundle.PreviousBundleID != nil ||
		firstBundle.Generation != first.Value.Execution.Generation || firstBundle.PayloadSHA256 == "" {
		t.Fatalf("first Recovery Bundle is invalid: %#v", firstBundle)
	}
	if len(first.Value.Workload.MemoryReferences) != 1 || first.Value.Workload.MemoryReferences[0] != memoryReference {
		t.Fatalf("Recovery Bundle omitted the exact authoritative Memory reference: %#v", first.Value.Workload.MemoryReferences)
	}
	if len(resolver.executionIDs) == 0 || resolver.executionIDs[0] != fixture.ExecutionID {
		t.Fatalf("memory resolver execution IDs = %#v, want first %s", resolver.executionIDs, fixture.ExecutionID)
	}
	if err := ValidateRecoveryBundle(*first.Value.Execution, *first.Value.Workload); err != nil {
		t.Fatalf("validate first Recovery Bundle: %v", err)
	}
	if firstBundle.Execution.TenantSchedulingPolicyVersion != first.Value.Execution.TenantSchedulingPolicyVersion ||
		firstBundle.Execution.TenantSchedulingPolicyDigest != first.Value.Execution.TenantSchedulingPolicyDigest ||
		firstBundle.Execution.OrganizationSchedulingPolicyVersion != first.Value.Execution.OrganizationSchedulingPolicyVersion ||
		firstBundle.Execution.OrganizationSchedulingPolicyDigest != first.Value.Execution.OrganizationSchedulingPolicyDigest {
		t.Fatalf("Recovery Bundle omitted the Execution Scheduling Policy snapshot: %#v", firstBundle.Execution)
	}
	tamperedPolicyWorkload := *first.Value.Workload
	tamperedPolicyBundle := *firstBundle
	tamperedPolicyBundle.Execution.TenantSchedulingPolicyDigest = strings.Repeat("f", 64)
	tamperedPolicyWorkload.RecoveryBundle = &tamperedPolicyBundle
	if err := ValidateRecoveryBundle(*first.Value.Execution, tamperedPolicyWorkload); err == nil {
		t.Fatal("tampered Execution Scheduling Policy snapshot passed Recovery Bundle verification")
	} else {
		assertRecoveryBundleProblem(t, err, "recovery_bundle_claim_mismatch")
	}

	var persisted persistence.ExecutionRecoveryBundle
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, firstBundle.ID).Take(&persisted).Error; err != nil {
		t.Fatal(err)
	}
	if persisted.PayloadSHA256 != firstBundle.PayloadSHA256 ||
		persisted.AuthoritativeHistorySequence != firstBundle.AuthoritativeHistorySequence {
		t.Fatalf("persisted Recovery Bundle diverged: %#v", persisted)
	}

	tampered := *first.Value.Workload
	tampered.InputText = "tampered after claim"
	if err := ValidateRecoveryBundle(*first.Value.Execution, tampered); err == nil {
		t.Fatal("tampered Workload passed Recovery Bundle verification")
	} else {
		assertRecoveryBundleProblem(t, err, "recovery_bundle_claim_integrity_failed")
	}

	for name, mutate := range map[string]func(*Workload){
		"memory media type": func(workload *Workload) {
			workload.MemoryReferences[0].MediaType = "application/json"
		},
		"memory size": func(workload *Workload) {
			workload.MemoryReferences[0].SizeBytes++
		},
	} {
		t.Run(name, func(t *testing.T) {
			tamperedMemory := *first.Value.Workload
			tamperedMemory.MemoryReferences = append(
				[]RecoveryMemoryReference(nil),
				first.Value.Workload.MemoryReferences...,
			)
			mutate(&tamperedMemory)
			if err := ValidateRecoveryBundle(*first.Value.Execution, tamperedMemory); err == nil {
				t.Fatal("tampered Memory identity passed Recovery Bundle verification")
			} else {
				assertRecoveryBundleProblem(t, err, "recovery_bundle_claim_integrity_failed")
			}
		})
	}

	replayed, err := service.Claim(ctx, firstWorker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind,
		ExecutionID: &fixture.ExecutionID,
	}, "recovery-bundle-first-claim")
	if err != nil || !replayed.Replayed || replayed.Value.Workload == nil ||
		replayed.Value.Workload.RecoveryBundle == nil || replayed.Value.Lease == nil {
		t.Fatalf("replayed claim = %#v, %v", replayed, err)
	}
	if replayed.Value.Workload.RecoveryBundle.ID != firstBundle.ID ||
		replayed.Value.Workload.RecoveryBundle.PayloadSHA256 != firstBundle.PayloadSHA256 {
		t.Fatalf("claim replay changed the frozen Recovery Bundle: %#v", replayed.Value.Workload.RecoveryBundle)
	}

	if _, err := service.Release(ctx, firstWorker, fixture.ExecutionID, ReleaseLeaseInput{
		LeaseInput: LeaseInput{
			TenantID: fixture.TenantID, Generation: replayed.Value.Lease.Generation,
			LeaseToken: replayed.Value.Lease.LeaseToken,
		},
		Reason: "exercise Recovery Bundle lineage",
	}, "recovery-bundle-release"); err != nil {
		t.Fatal(err)
	}

	second, err := service.Claim(ctx, secondWorker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind,
		ExecutionID: &fixture.ExecutionID,
	}, "recovery-bundle-second-claim")
	if err != nil || second.Value.Execution == nil || second.Value.Workload == nil ||
		second.Value.Workload.RecoveryBundle == nil {
		t.Fatalf("second claim = %#v, %v", second, err)
	}
	secondBundle := second.Value.Workload.RecoveryBundle
	if secondBundle.RecoveryReason != "execution-recovery" || secondBundle.PreviousBundleID == nil ||
		*secondBundle.PreviousBundleID != firstBundle.ID || secondBundle.Generation != firstBundle.Generation+1 {
		t.Fatalf("recovered Generation did not link its predecessor: first=%#v second=%#v", firstBundle, secondBundle)
	}
	if err := ValidateRecoveryBundle(*second.Value.Execution, *second.Value.Workload); err != nil {
		t.Fatalf("validate recovered Recovery Bundle: %v", err)
	}

	var bundleCount int64
	if err := db.Model(&persistence.ExecutionRecoveryBundle{}).
		Where("tenant_id = ? AND execution_id = ?", fixture.TenantID, fixture.ExecutionID).
		Count(&bundleCount).Error; err != nil {
		t.Fatal(err)
	}
	if bundleCount != 2 {
		t.Fatalf("Recovery Bundle count = %d, want 2", bundleCount)
	}
	if err := db.Model(&persistence.ExecutionRecoveryBundle{}).
		Where("tenant_id = ? AND id = ?", fixture.TenantID, firstBundle.ID).
		Update("payload_sha256", string(make([]byte, 64))).Error; err == nil {
		t.Fatal("immutable Recovery Bundle accepted an update")
	}
	if err := db.Delete(&persistence.ExecutionRecoveryBundle{}, "tenant_id = ? AND id = ?", fixture.TenantID, firstBundle.ID).Error; err == nil {
		t.Fatal("immutable Recovery Bundle accepted a delete")
	}

	var leasedEvent persistence.SessionEvent
	if err := db.Where(
		"tenant_id = ? AND session_id = ? AND execution_id = ? AND event_type = ? AND generation = ?",
		fixture.TenantID, fixture.SessionID, fixture.ExecutionID, "execution.leased", secondBundle.Generation,
	).Take(&leasedEvent).Error; err != nil {
		t.Fatal(err)
	}
	if leasedEvent.Payload["recoveryBundleId"] != secondBundle.ID.String() &&
		leasedEvent.Payload["recoveryBundleId"] != secondBundle.ID {
		t.Fatalf("leased Event omitted Recovery Bundle identity: %#v", leasedEvent.Payload)
	}
}

func TestSchedulingDecisionIdentityAllowsOnlyImmutableLegacyOmissionOrExactMatch(t *testing.T) {
	decisionID := uuid.New()
	otherDecisionID := uuid.New()
	if !sameSchedulingDecisionIdentity(nil, &decisionID) {
		t.Fatal("immutable pre-000076 Recovery Bundle omission was rejected")
	}
	if !sameSchedulingDecisionIdentity(&decisionID, &decisionID) {
		t.Fatal("exact scheduling decision identity was rejected")
	}
	if sameSchedulingDecisionIdentity(&decisionID, &otherDecisionID) {
		t.Fatal("mismatched scheduling decision identity was accepted")
	}
	if sameSchedulingDecisionIdentity(&decisionID, nil) {
		t.Fatal("Recovery Bundle decision identity without an Execution binding was accepted")
	}
}

func TestRecoveryBundleSurvivesReleaseReplayAndDisasterRecoverySuccessorClaim(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	firstWorker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "bundle-e2e-first")
	secondWorker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "bundle-e2e-second")
	successorWorker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "bundle-e2e-successor")
	cleanupWorkers(t, db, firstWorker.ID, secondWorker.ID, successorWorker.ID)

	var session persistence.AgentSession
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.SessionID).Take(&session).Error; err != nil {
		t.Fatal(err)
	}
	appendRecoveryBundleTestEvent(t, db, &session, fixture.ExecutionID, "turn.created", map[string]any{
		"turnId": fixture.TurnID, "executionId": fixture.ExecutionID, "inputText": "preserve DR context",
	})
	first, err := service.Claim(ctx, firstWorker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "bundle-e2e-first-claim")
	if err != nil || first.Value.Execution == nil || first.Value.Lease == nil || first.Value.Workload == nil ||
		first.Value.Workload.RecoveryBundle == nil {
		t.Fatalf("first claim = %#v, %v", first, err)
	}
	if err := ValidateRecoveryBundle(*first.Value.Execution, *first.Value.Workload); err != nil {
		t.Fatalf("validate first claim: %v", err)
	}

	appendRecoveryBundleTestEvent(t, db, &session, fixture.ExecutionID, "content.delta", map[string]any{
		"streamKind": "assistant_text", "delta": "source execution context",
	})
	readyAt := time.Now().UTC()
	artifactSHA256 := strings.Repeat("d", 64)
	artifact := persistence.Artifact{
		ID: uuid.New(), TenantID: fixture.TenantID, OrganizationID: session.OrganizationID,
		ProjectID: session.ProjectID, SessionID: fixture.SessionID, ExecutionID: &fixture.ExecutionID,
		Kind: "generated_file", Status: "ready", Bucket: "test", ObjectKey: "recovery/source.txt",
		SHA256: &artifactSHA256, CreatedByType: "worker", CreatedByID: firstWorker.ID,
		ReadyAt: &readyAt, CreatedAt: readyAt,
	}
	if err := db.Create(&artifact).Error; err != nil {
		t.Fatal(err)
	}
	appendRecoveryBundleTestEvent(t, db, &session, fixture.ExecutionID, "artifact.ready", map[string]any{
		"artifactId": artifact.ID, "kind": artifact.Kind,
	})

	if _, err := service.Release(ctx, firstWorker, fixture.ExecutionID, ReleaseLeaseInput{
		LeaseInput: LeaseInput{
			TenantID: fixture.TenantID, Generation: first.Value.Lease.Generation,
			LeaseToken: first.Value.Lease.LeaseToken,
		},
		Reason: "exercise source recovery",
	}, "bundle-e2e-first-release"); err != nil {
		t.Fatal(err)
	}
	second, err := service.Claim(ctx, secondWorker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "bundle-e2e-second-claim")
	if err != nil || second.Value.Execution == nil || second.Value.Lease == nil || second.Value.Workload == nil ||
		second.Value.Workload.RecoveryBundle == nil || second.Value.Workload.ResumeSnapshot == nil {
		t.Fatalf("second claim = %#v, %v", second, err)
	}
	if err := ValidateRecoveryBundle(*second.Value.Execution, *second.Value.Workload); err != nil {
		t.Fatalf("validate replayed source claim: %v", err)
	}
	if len(second.Value.Workload.ResumeSnapshot.ArtifactReferences) != 1 ||
		second.Value.Workload.ResumeSnapshot.ArtifactReferences[0].ArtifactID != artifact.ID {
		t.Fatalf("replayed source omitted Artifact: %#v", second.Value.Workload.ResumeSnapshot.ArtifactReferences)
	}
	replayedSecond, err := service.Claim(ctx, secondWorker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &fixture.ExecutionID,
	}, "bundle-e2e-second-claim")
	if err != nil || !replayedSecond.Replayed || replayedSecond.Value.Execution == nil ||
		replayedSecond.Value.Lease == nil || replayedSecond.Value.Workload == nil ||
		replayedSecond.Value.Workload.RecoveryBundle == nil {
		t.Fatalf("replay source claim = %#v, %v", replayedSecond, err)
	}
	if replayedSecond.Value.Workload.RecoveryBundle.ID != second.Value.Workload.RecoveryBundle.ID {
		t.Fatalf("source claim replay changed Recovery Bundle: %#v", replayedSecond.Value.Workload.RecoveryBundle)
	}
	if err := ValidateRecoveryBundle(*replayedSecond.Value.Execution, *replayedSecond.Value.Workload); err != nil {
		t.Fatalf("validate source claim replay: %v", err)
	}

	if _, err := service.Release(ctx, secondWorker, fixture.ExecutionID, ReleaseLeaseInput{
		LeaseInput: LeaseInput{
			TenantID: fixture.TenantID, Generation: replayedSecond.Value.Lease.Generation,
			LeaseToken: replayedSecond.Value.Lease.LeaseToken,
		},
		Reason: "fence source for disaster recovery",
	}, "bundle-e2e-second-release"); err != nil {
		t.Fatal(err)
	}
	var source persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).Take(&source).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	failureCode := "target_failover"
	failureMessage := "source fenced for Recovery Bundle end-to-end test"
	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", fixture.TenantID, source.ID).
		Updates(map[string]any{
			"status": "interrupted", "next_recovery_reason": nil, "finished_at": now,
			"failure_code": failureCode, "failure_message": failureMessage,
		}).Error; err != nil {
		t.Fatal(err)
	}
	recoveryReason := "disaster-recovery"
	successor := persistence.AgentExecution{
		ID: uuid.New(), TenantID: fixture.TenantID, SessionID: fixture.SessionID, TurnID: fixture.TurnID,
		Attempt: source.Attempt + 1, Status: "recovering", ExecutionTargetID: fixture.TargetID,
		TargetKind: fixture.TargetKind, Provider: source.Provider,
		ProviderRuntimeBindingID: source.ProviderRuntimeBindingID,
		RemoteWorkspaceID:        source.RemoteWorkspaceID, WorkspaceMaterializationID: source.WorkspaceMaterializationID,
		RestoreCheckpointID: source.RestoreCheckpointID, WarmPoolModeSnapshot: source.WarmPoolModeSnapshot,
		Generation: 0, RequestedBy: source.RequestedBy, QueuedAt: now,
		PredecessorExecutionID: &source.ID, NextRecoveryReason: &recoveryReason,
	}
	if err := db.Create(&successor).Error; err != nil {
		t.Fatal(err)
	}
	claimedSuccessor, err := service.Claim(ctx, successorWorker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &successor.ID,
	}, "bundle-e2e-successor-claim")
	if err != nil || claimedSuccessor.Value.Execution == nil || claimedSuccessor.Value.Workload == nil ||
		claimedSuccessor.Value.Workload.RecoveryBundle == nil || claimedSuccessor.Value.Workload.ResumeSnapshot == nil {
		t.Fatalf("successor claim = %#v, %v", claimedSuccessor, err)
	}
	successorBundle := claimedSuccessor.Value.Workload.RecoveryBundle
	if successorBundle.RecoveryReason != "disaster-recovery" || successorBundle.PreviousBundleID == nil ||
		*successorBundle.PreviousBundleID != second.Value.Workload.RecoveryBundle.ID {
		t.Fatalf("successor Recovery Bundle lineage = %#v", successorBundle)
	}
	if err := ValidateRecoveryBundle(*claimedSuccessor.Value.Execution, *claimedSuccessor.Value.Workload); err != nil {
		t.Fatalf("validate successor Recovery Bundle: %v", err)
	}
	resume := claimedSuccessor.Value.Workload.ResumeSnapshot
	if len(resume.ArtifactReferences) != 1 || resume.ArtifactReferences[0].ArtifactID != artifact.ID {
		t.Fatalf("successor omitted source Artifact: %#v", resume.ArtifactReferences)
	}
	if len(resume.Messages) < 2 || resume.Messages[len(resume.Messages)-1].Text != "source execution context" {
		t.Fatalf("successor omitted source context: %#v", resume.Messages)
	}
	replayedSuccessor, err := service.Claim(ctx, successorWorker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind, ExecutionID: &successor.ID,
	}, "bundle-e2e-successor-claim")
	if err != nil || !replayedSuccessor.Replayed || replayedSuccessor.Value.Execution == nil ||
		replayedSuccessor.Value.Workload == nil || replayedSuccessor.Value.Workload.RecoveryBundle == nil {
		t.Fatalf("replay successor claim = %#v, %v", replayedSuccessor, err)
	}
	if replayedSuccessor.Value.Workload.RecoveryBundle.ID != successorBundle.ID {
		t.Fatalf("successor claim replay changed Recovery Bundle: %#v", replayedSuccessor.Value.Workload.RecoveryBundle)
	}
	if err := ValidateRecoveryBundle(*replayedSuccessor.Value.Execution, *replayedSuccessor.Value.Workload); err != nil {
		t.Fatalf("validate successor claim replay: %v", err)
	}
}

func appendRecoveryBundleTestEvent(
	t *testing.T,
	db *gorm.DB,
	session *persistence.AgentSession,
	executionID uuid.UUID,
	eventType string,
	payload map[string]any,
) {
	t.Helper()
	if err := db.Select("last_event_sequence").
		Where("tenant_id = ? AND id = ?", session.TenantID, session.ID).
		Take(session).Error; err != nil {
		t.Fatal(err)
	}
	session.LastEventSequence++
	event := persistence.SessionEvent{
		TenantID: session.TenantID, OrganizationID: session.OrganizationID, ProjectID: session.ProjectID,
		SessionID: session.ID, Sequence: session.LastEventSequence, EventID: uuid.New(), EventVersion: 2,
		EventType: eventType, ActorType: "worker", ExecutionID: &executionID,
		Payload: payload, OccurredAt: time.Now().UTC(),
	}
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&event).Error; err != nil {
			return err
		}
		return tx.Model(&persistence.AgentSession{}).
			Where("tenant_id = ? AND id = ?", session.TenantID, session.ID).
			Update("last_event_sequence", session.LastEventSequence).Error
	}); err != nil {
		t.Fatal(err)
	}
}

type staticMemoryReferenceResolver struct {
	references   []RecoveryMemoryReference
	executionIDs []uuid.UUID
}

func (resolver *staticMemoryReferenceResolver) ResolveExecutionRecoveryMemoryReferences(
	_ context.Context,
	_ *gorm.DB,
	_, executionID uuid.UUID,
) ([]RecoveryMemoryReference, error) {
	resolver.executionIDs = append(resolver.executionIDs, executionID)
	return append([]RecoveryMemoryReference(nil), resolver.references...), nil
}

func TestClaimAdoptsLegacyGenerationWithoutInventingPredecessor(t *testing.T) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	ensureGenerationFactTable(t, db)
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "bundle-legacy")
	cleanupWorkers(t, db, worker.ID)

	// Model an execution recovered before migration 000043. The first Bundle
	// created after upgrade must be explicit legacy adoption, not a fabricated
	// Generation-1 predecessor.
	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).
		Updates(map[string]any{"status": "recovering", "generation": 4}).Error; err != nil {
		t.Fatal(err)
	}
	var legacyExecution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).
		Take(&legacyExecution).Error; err != nil {
		t.Fatal(err)
	}
	if err := persistence.InTransaction(ctx, db, func(tx *gorm.DB) error {
		return service.enqueueRecovery(ctx, tx, legacyExecution, "legacy-worker-lost", "")
	}); err != nil {
		t.Fatal(err)
	}
	seededFact := loadGenerationFactForTest(t, db, fixture, 5)
	if seededFact.RecoveryReason != "legacy-adoption" {
		t.Fatalf("legacy dispatch recovery reason = %q, want legacy-adoption", seededFact.RecoveryReason)
	}
	claim, err := service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID, TargetKind: fixture.TargetKind,
		ExecutionID: &fixture.ExecutionID,
	}, "recovery-bundle-legacy-claim")
	if err != nil || claim.Value.Workload == nil || claim.Value.Workload.RecoveryBundle == nil {
		t.Fatalf("legacy adoption claim = %#v, %v", claim, err)
	}
	bundle := claim.Value.Workload.RecoveryBundle
	if bundle.Generation != 5 || bundle.RecoveryReason != "legacy-adoption" || bundle.PreviousBundleID != nil {
		t.Fatalf("legacy generation was not adopted explicitly: %#v", bundle)
	}
}

func TestClaimReplayFailsClosedForLegacyProviderCredentialReceiptWithoutGrant(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		existingBundle bool
	}{
		{name: "missing-recovery-bundle"},
		{name: "existing-legacy-recovery-bundle", existingBundle: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			testClaimReplayFailsClosedForLegacyProviderCredentialReceiptWithoutGrant(t, testCase.existingBundle)
		})
	}
}

func testClaimReplayFailsClosedForLegacyProviderCredentialReceiptWithoutGrant(t *testing.T, existingBundle bool) {
	ctx := context.Background()
	db, service, fixture := setupSQLiteRecoveryService(t)
	worker := registerManifestTestWorker(t, service, fixture.TargetID, fixture.TargetKind, "bundle-legacy-provider-receipt")
	cleanupWorkers(t, db, worker.ID)

	now := service.now()
	provider := "codex"
	plainToken := "legacy-provider-replay-lease-token"
	if err := db.Model(&persistence.AgentExecution{}).
		Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).
		Updates(map[string]any{
			"status":                               "leased",
			"worker_id":                            worker.ID,
			"worker_manifest_id":                   worker.CurrentManifestID,
			"provider":                             provider,
			"generation":                           1,
			"provider_credential_id_snapshot":      fixture.ProviderCredentialID,
			"provider_credential_version_snapshot": 1,
			"provider_resume_strategy_snapshot":    "authoritative-history",
		}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.WorkerLease{
		ExecutionID:       fixture.ExecutionID,
		TenantID:          fixture.TenantID,
		WorkerID:          worker.ID,
		WorkerIncarnation: worker.Incarnation,
		WorkerInstanceUID: worker.InstanceUID,
		Generation:        1,
		LeaseTokenHash:    secret.HashToken(plainToken),
		AcquiredAt:        now,
		HeartbeatAt:       now,
		ExpiresAt:         now.Add(time.Minute),
	}).Error; err != nil {
		t.Fatal(err)
	}

	var execution persistence.AgentExecution
	if err := db.Where("tenant_id = ? AND id = ?", fixture.TenantID, fixture.ExecutionID).Take(&execution).Error; err != nil {
		t.Fatal(err)
	}
	normalizedTarget, err := normalizeClaimTarget(worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID,
		TargetKind:        fixture.TargetKind,
		ExecutionID:       &fixture.ExecutionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	hash, err := requestHash("execution.claim", normalizedTarget)
	if err != nil {
		t.Fatal(err)
	}
	storedWorkload := Workload{
		SessionID:                 fixture.SessionID,
		TurnID:                    fixture.TurnID,
		ProviderCredentialID:      &fixture.ProviderCredentialID,
		ProviderCredentialGrantID: nil,
	}
	if existingBundle {
		if err := db.Transaction(func(tx *gorm.DB) error {
			legacyWorkload, err := service.loadWorkload(ctx, tx, execution)
			if err != nil {
				return err
			}
			if legacyWorkload.ProviderCredentialID == nil || legacyWorkload.ProviderCredentialGrantID != nil {
				return errors.New("legacy workload did not preserve the pre-grant Provider Credential shape")
			}
			_, err = service.createRecoveryBundle(ctx, tx, execution, legacyWorkload, now)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	response, err := responseMap(ClaimResult{
		Execution: &Execution{
			ID: execution.ID, TenantID: execution.TenantID, SessionID: execution.SessionID, TurnID: execution.TurnID,
			Attempt: execution.Attempt, Status: execution.Status, ExecutionTargetID: execution.ExecutionTargetID,
			TargetKind: execution.TargetKind, Provider: execution.Provider, WorkerID: execution.WorkerID,
			WorkerManifestID: execution.WorkerManifestID, ProviderRuntimeBindingID: execution.ProviderRuntimeBindingID,
			RemoteWorkspaceID: execution.RemoteWorkspaceID, WorkspaceMaterializationID: execution.WorkspaceMaterializationID,
			RestoreCheckpointID: execution.RestoreCheckpointID, Generation: execution.Generation,
			RequestedBy: execution.RequestedBy, QueuedAt: execution.QueuedAt, StartedAt: execution.StartedAt,
			FinishedAt: execution.FinishedAt, FailureCode: execution.FailureCode, FailureMessage: execution.FailureMessage,
		},
		Lease: &Lease{
			ExecutionID: fixture.ExecutionID,
			TenantID:    fixture.TenantID,
			WorkerID:    worker.ID,
			Generation:  1,
			AcquiredAt:  now,
			HeartbeatAt: now,
			ExpiresAt:   now.Add(time.Minute),
		},
		Workload: &storedWorkload,
	})
	if err != nil {
		t.Fatal(err)
	}
	requestID := "legacy-provider-replay-receipt"
	if err := db.Create(&persistence.WorkerRequestReceipt{
		WorkerID:          worker.ID,
		WorkerIncarnation: worker.Incarnation,
		RequestID:         requestID,
		Operation:         "execution.claim",
		RequestHash:       hash,
		StatusCode:        200,
		Response:          response,
		CreatedAt:         now,
		ExpiresAt:         now.Add(time.Hour),
	}).Error; err != nil {
		t.Fatal(err)
	}

	var before persistence.WorkerLease
	if err := db.Where("tenant_id = ? AND execution_id = ?", fixture.TenantID, fixture.ExecutionID).Take(&before).Error; err != nil {
		t.Fatal(err)
	}

	_, err = service.Claim(ctx, worker, ClaimExecutionInput{
		ExecutionTargetID: fixture.TargetID,
		TargetKind:        fixture.TargetKind,
		ExecutionID:       &fixture.ExecutionID,
	}, requestID)
	assertProblemCode(t, err, "provider_credential_grant_required")

	var bundleCount int64
	if err := db.Model(&persistence.ExecutionRecoveryBundle{}).
		Where("tenant_id = ? AND execution_id = ?", fixture.TenantID, fixture.ExecutionID).
		Count(&bundleCount).Error; err != nil {
		t.Fatal(err)
	}
	expectedBundleCount := int64(0)
	if existingBundle {
		expectedBundleCount = 1
	}
	if bundleCount != expectedBundleCount {
		t.Fatalf("legacy Provider Credential replay changed Recovery Bundle count: got %d, want %d", bundleCount, expectedBundleCount)
	}
	var after persistence.WorkerLease
	if err := db.Where("tenant_id = ? AND execution_id = ?", fixture.TenantID, fixture.ExecutionID).Take(&after).Error; err != nil {
		t.Fatal(err)
	}
	if !after.ExpiresAt.Equal(before.ExpiresAt) || string(after.LeaseTokenHash) != string(before.LeaseTokenHash) {
		t.Fatalf("legacy Provider Credential replay rotated its lease: before=%#v after=%#v", before, after)
	}
}

func TestValidateRecoveryBundleFailsClosedForKubernetes(t *testing.T) {
	err := ValidateRecoveryBundle(Execution{
		ID: uuid.New(), SessionID: uuid.New(), TurnID: uuid.New(),
		Generation: 1, TargetKind: "kubernetes",
	}, Workload{})
	assertRecoveryBundleProblem(t, err, "recovery_bundle_required")

	if err := ValidateRecoveryBundle(Execution{
		ID: uuid.New(), SessionID: uuid.New(), TurnID: uuid.New(),
		Generation: 1, TargetKind: "local",
	}, Workload{}); err != nil {
		t.Fatalf("rolling-upgrade compatibility rejected a non-Kubernetes legacy claim: %v", err)
	}
}

func assertRecoveryBundleProblem(t *testing.T, err error, code string) {
	t.Helper()
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != code {
		t.Fatalf("expected Recovery Bundle problem %q, got %v", code, err)
	}
}
