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
