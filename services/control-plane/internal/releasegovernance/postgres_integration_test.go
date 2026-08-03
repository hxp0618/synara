package releasegovernance

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/governanceauthority"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/postgresisolation"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestReleaseGovernancePostgresSeparatedApprovalAndImmutableEvidence(t *testing.T) {
	databaseURL := os.Getenv("TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("TEST_POSTGRES_URL is not configured")
	}
	databaseURL = postgresisolation.URL(t, databaseURL)
	ctx := context.Background()
	db, err := database.Open(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := database.Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "release-governance-pg-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	approvalRoles := append(append([]string{}, requiredApprovalRoles...), "privacy_legal")
	approvers := make([]uuid.UUID, 0, len(approvalRoles))
	for index := range approvalRoles {
		user := persistence.User{
			ID: uuid.New(), Email: "release-pg-" + string(rune('a'+index)) + "@example.test",
			DisplayName: "Release PG Approver " + string(rune('A'+index)), Status: "active",
			EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
		}
		if err := db.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&persistence.TenantMembership{
			TenantID: domain.TenantID, UserID: user.ID, Role: "admin", Status: "active",
			JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
		approvers = append(approvers, user.ID)
	}
	for index, role := range approvalRoles {
		seedReleaseAuthority(t, db, domain.TenantID, domain.UserID, approvers[index], "release."+role, now)
	}
	for index, role := range []string{"legal", "privacy", "security", "product"} {
		seedReleaseAuthority(t, db, domain.TenantID, domain.UserID, approvers[index], governanceauthority.ProviderCommercialPrefix+role, now)
	}
	providerAuthorizationID := seedActiveProviderCommercialAuthorization(
		t, ctx, db, domain.TenantID, domain.UserID, approvers[:4], now,
	)
	service := NewService(db, domain.TenantID)
	directUnboundInput := newCandidateCreateInput("v0.6.3-stage6.pg-direct-unbound", "stage6/postgres-direct-unbound", []string{"code_change", "provider_commercial"})
	directUnbound, err := service.normalizeCreate(domain.UserID, directUnboundInput)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&directUnbound).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.Stage6ReleaseCandidate{}).Where("id = ?", directUnbound.ID).Updates(map[string]any{
		"state": "ready_for_review", "version": 2, "updated_at": now,
	}).Error; err == nil {
		t.Fatal("PostgreSQL allowed a Provider-commercial candidate without an exact authorization binding to enter review")
	}
	createInput := newCandidateCreateInput("v0.6.3-stage6.pg", "stage6/postgres", []string{"code_change", "provider_commercial"})
	createInput.ProviderCommercialAuthorizationIDs = []uuid.UUID{providerAuthorizationID}
	candidate, err := service.Create(
		ctx, domain.UserID,
		createInput,
		"pg-create", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.Stage6ReleaseProviderAuthorizationBinding{}).
		Where("candidate_record_id = ?", candidate.ID).Update("authorization_version", 99).Error; err == nil {
		t.Fatal("PostgreSQL allowed a release Provider authorization binding to be mutated")
	}
	if err := db.Where("candidate_record_id = ?", candidate.ID).
		Delete(&persistence.Stage6ReleaseProviderAuthorizationBinding{}).Error; err == nil {
		t.Fatal("PostgreSQL allowed a release Provider authorization binding to be deleted")
	}
	candidate, err = service.Transition(ctx, domain.UserID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "ready_for_review",
		Reason: "Submit exact PostgreSQL candidate for separated role review.",
	}, "pg-ready", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.Stage6ReleaseApproval{
		ID: uuid.New(), CandidateRecordID: candidate.ID, OperatorTenantID: domain.TenantID,
		Role: "engineering", Decision: "approved", ApproverUserID: approvers[0],
		Reason:            "PostgreSQL must reject URL-only Release approval evidence.",
		EvidenceReference: "https://evidence.example.test/postgres/url-only-release-approval", CreatedAt: now,
	}).Error; err == nil {
		t.Fatal("PostgreSQL accepted a Release approval without an evidence digest")
	}

	var wait sync.WaitGroup
	errorsByRole := make(chan error, len(approvalRoles))
	for index, role := range approvalRoles {
		wait.Add(1)
		go func(index int, role string) {
			defer wait.Done()
			_, approvalErr := service.RecordApproval(ctx, approvers[index], candidate.ID, ApprovalInput{
				Role: role, Decision: "approved", Reason: "PostgreSQL concurrent role review accepted exact evidence.",
				EvidenceReference: "https://evidence.example.test/postgres/" + role,
				EvidenceSHA256:    testReleaseApprovalDigest("postgres-" + role),
			}, "pg-approval-"+role, "127.0.0.1")
			errorsByRole <- approvalErr
		}(index, role)
	}
	wait.Wait()
	close(errorsByRole)
	for approvalErr := range errorsByRole {
		if approvalErr != nil {
			t.Fatal(approvalErr)
		}
	}
	candidate, err = service.get(ctx, candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidate.Approvals) != len(approvalRoles) {
		t.Fatalf("PostgreSQL approval count = %d", len(candidate.Approvals))
	}
	_, err = service.Transition(ctx, domain.UserID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "approved",
		Reason: "Reject PostgreSQL approval until the candidate-bound SLO window is independently approved.",
	}, "pg-missing-slo", "127.0.0.1")
	assertProblemCode(t, err, "release_slo_gate_incomplete")
	if err := db.Model(&persistence.Stage6ReleaseCandidate{}).Where("id = ?", candidate.ID).Updates(map[string]any{
		"state": "approved", "version": candidate.Version + 1, "approved_at": now, "updated_at": now,
	}).Error; err == nil {
		t.Fatal("PostgreSQL allowed release approval without an approved candidate-bound SLO window")
	}
	seedApprovedSLOGate(t, db, domain.TenantID, domain.UserID, approvers, candidate, now)
	_, err = service.Transition(ctx, domain.UserID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "approved",
		Reason: "Reject PostgreSQL approval until the candidate-bound Recovery drill is independently approved.",
	}, "pg-missing-recovery", "127.0.0.1")
	assertProblemCode(t, err, "release_recovery_gate_incomplete")
	if err := db.Model(&persistence.Stage6ReleaseCandidate{}).Where("id = ?", candidate.ID).Updates(map[string]any{
		"state": "approved", "version": candidate.Version + 1, "approved_at": now, "updated_at": now,
	}).Error; err == nil {
		t.Fatal("PostgreSQL allowed release approval without an approved candidate-bound Recovery drill")
	}
	seedApprovedRecoveryGate(t, ctx, db, domain.TenantID, domain.UserID, approvers, candidate, now)
	_, err = service.Transition(ctx, domain.UserID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "approved",
		Reason: "Reject PostgreSQL approval until the candidate-bound Penetration engagement is independently approved.",
	}, "pg-missing-penetration", "127.0.0.1")
	assertProblemCode(t, err, "release_penetration_gate_incomplete")
	if err := db.Model(&persistence.Stage6ReleaseCandidate{}).Where("id = ?", candidate.ID).Updates(map[string]any{
		"state": "approved", "version": candidate.Version + 1, "approved_at": now, "updated_at": now,
	}).Error; err == nil {
		t.Fatal("PostgreSQL allowed release approval without an approved candidate-bound Penetration engagement")
	}
	seedApprovedPenetrationGate(t, ctx, db, domain.TenantID, domain.UserID, approvers, candidate, now)
	_, err = service.Transition(ctx, domain.UserID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "approved",
		Reason: "Reject PostgreSQL approval until the candidate-bound Capacity run is independently approved.",
	}, "pg-missing-capacity", "127.0.0.1")
	assertProblemCode(t, err, "release_capacity_gate_incomplete")
	if err := db.Model(&persistence.Stage6ReleaseCandidate{}).Where("id = ?", candidate.ID).Updates(map[string]any{
		"state": "approved", "version": candidate.Version + 1, "approved_at": now, "updated_at": now,
	}).Error; err == nil {
		t.Fatal("PostgreSQL allowed release approval without an approved candidate-bound Capacity run")
	}
	seedApprovedCapacityGate(t, ctx, db, domain.TenantID, domain.UserID, approvers, candidate, now)
	_, err = service.Transition(ctx, domain.UserID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "approved",
		Reason: "Reject PostgreSQL approval until the candidate-bound Incident exercise is independently approved.",
	}, "pg-missing-incident-exercise", "127.0.0.1")
	assertProblemCode(t, err, "release_incident_exercise_gate_incomplete")
	if err := db.Model(&persistence.Stage6ReleaseCandidate{}).Where("id = ?", candidate.ID).Updates(map[string]any{
		"state": "approved", "version": candidate.Version + 1, "approved_at": now, "updated_at": now,
	}).Error; err == nil {
		t.Fatal("PostgreSQL allowed release approval without an approved candidate-bound Incident exercise")
	}
	seedApprovedIncidentExerciseGate(t, ctx, db, domain.TenantID, domain.UserID, approvers, candidate, now)
	_, err = service.Transition(ctx, domain.UserID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "approved",
		Reason: "Reject PostgreSQL approval until the candidate-bound Operations exercise is independently approved.",
	}, "pg-missing-operations-exercise", "127.0.0.1")
	assertProblemCode(t, err, "release_operations_exercise_gate_incomplete")
	if err := db.Model(&persistence.Stage6ReleaseCandidate{}).Where("id = ?", candidate.ID).Updates(map[string]any{
		"state": "approved", "version": candidate.Version + 1, "approved_at": now, "updated_at": now,
	}).Error; err == nil {
		t.Fatal("PostgreSQL allowed release approval without an approved candidate-bound Operations exercise")
	}
	seedApprovedOperationsExerciseGate(t, ctx, db, domain.TenantID, domain.UserID, approvers, candidate, now)
	_, err = service.Transition(ctx, domain.UserID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "approved",
		Reason: "Reject PostgreSQL approval until the candidate-bound internal cost review is independently approved.",
	}, "pg-missing-internal-cost", "127.0.0.1")
	assertProblemCode(t, err, "release_internal_cost_gate_incomplete")
	if err := db.Model(&persistence.Stage6ReleaseCandidate{}).Where("id = ?", candidate.ID).Updates(map[string]any{
		"state": "approved", "version": candidate.Version + 1, "approved_at": now, "updated_at": now,
	}).Error; err == nil {
		t.Fatal("PostgreSQL allowed release approval without an approved candidate-bound internal cost review")
	}
	seedApprovedInternalCostGate(t, ctx, db, domain.TenantID, domain.UserID, approvers, candidate, now)
	readiness, err := service.GetReadiness(ctx, candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !readiness.InternalGatesSatisfied || !readiness.ApprovalTransitionEligible || len(readiness.Gates) != 10 {
		t.Fatalf("PostgreSQL readiness = %#v", readiness)
	}
	candidate, err = service.Transition(ctx, domain.UserID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "approved",
		Reason: "Advance only after all concurrent PostgreSQL decisions committed.",
	}, "pg-approved", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	candidate, err = service.Transition(ctx, domain.UserID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "deploying",
		Reason: "Deploy the exact PostgreSQL candidate after all internal gates passed.",
	}, "pg-deploying", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	candidate, err = service.Transition(ctx, domain.UserID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "observing",
		Reason: "Observe the exact PostgreSQL candidate before final release review.",
	}, "pg-observing", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	decisionSummary := "All PostgreSQL candidate controls and the observation window were reviewed for this exact release."
	disposition := "none"
	if err := db.Model(&persistence.Stage6ReleaseCandidate{}).Where("id = ?", candidate.ID).Updates(map[string]any{
		"state": "released", "version": candidate.Version + 1, "released_at": now,
		"decision_summary": decisionSummary, "residual_risk_disposition": disposition,
		"residual_risks": "[]", "updated_at": now,
	}).Error; err == nil {
		t.Fatal("PostgreSQL allowed released state without an exact-candidate Final Review")
	}
	finalReviewInput := newFinalReviewInput(t, candidate, decisionSummary, disposition, nil)
	digestMismatch := mutatedFinalReviewModel(
		t, db, candidate.ID, domain.UserID, finalReviewInput, func(*finalReviewReceipt) {},
	)
	digestMismatch.ReceiptSHA256[0] ^= 0xff
	if err := db.Create(&digestMismatch).Error; err == nil {
		t.Fatal("PostgreSQL accepted Final Review bytes whose stored SHA-256 projection did not match")
	}
	for _, forged := range []struct {
		name   string
		mutate func(*finalReviewReceipt)
	}{
		{name: "duplicated control", mutate: func(receipt *finalReviewReceipt) {
			receipt.ControlDecisions[1] = receipt.ControlDecisions[0]
		}},
		{name: "wrong control owner", mutate: func(receipt *finalReviewReceipt) {
			receipt.ControlDecisions[0].OwnerRole = "wrong-owner"
		}},
		{name: "reused final approver", mutate: func(receipt *finalReviewReceipt) {
			receipt.FinalApprovals[1].ApproverID = receipt.FinalApprovals[0].ApproverID
		}},
		{name: "null residual-risk array", mutate: func(receipt *finalReviewReceipt) {
			receipt.ResidualRisks = nil
		}},
	} {
		model := mutatedFinalReviewModel(t, db, candidate.ID, domain.UserID, finalReviewInput, forged.mutate)
		if err := db.Create(&model).Error; err == nil {
			t.Fatalf("PostgreSQL accepted a Final Review with %s through direct insert", forged.name)
		}
	}
	candidate, err = service.RecordFinalReview(
		ctx, domain.UserID, candidate.ID,
		finalReviewInput,
		"pg-final-review", "127.0.0.1",
	)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err = service.Transition(ctx, domain.UserID, candidate.ID, TransitionInput{
		ExpectedVersion: candidate.Version, TargetState: "released",
		Reason:          "Release only after binding the immutable exact-candidate Final Review.",
		DecisionSummary: &decisionSummary, ResidualRiskDisposition: &disposition,
	}, "pg-released", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if candidate.FinalReview == nil || candidate.State != "released" {
		t.Fatalf("PostgreSQL released candidate Final Review = %#v", candidate.FinalReview)
	}
	if err := db.Model(&persistence.Stage6ReleaseFinalReview{}).
		Where("candidate_record_id = ?", candidate.ID).Update("decision_summary", "mutated evidence").Error; err == nil {
		t.Fatal("PostgreSQL Final Review was mutable")
	}
	if err := db.Model(&persistence.Stage6ReleaseApproval{}).
		Where("candidate_record_id = ?", candidate.ID).Update("reason", "mutated evidence").Error; err == nil {
		t.Fatal("PostgreSQL release approval was mutable")
	}
	if err := db.Model(&persistence.Stage6ReleaseApproval{}).
		Where("candidate_record_id = ?", candidate.ID).Update("evidence_sha256", testReleaseApprovalDigest("mutated")).Error; err == nil {
		t.Fatal("PostgreSQL release approval evidence digest was mutable")
	}
	if err := db.Model(&persistence.Stage6ReleaseCandidate{}).
		Where("id = ?", candidate.ID).Update("source_commit", repeat("e", 40)).Error; err == nil {
		t.Fatal("PostgreSQL candidate identity was mutable")
	}
	if err := db.Model(&persistence.Stage6ReleaseCandidate{}).
		Where("id = ?", candidate.ID).Update("evidence_bundle_receipt", []byte(`{}`)).Error; err == nil {
		t.Fatal("PostgreSQL candidate evidence receipt was mutable")
	}
	legacyInput := newCandidateCreateInput("v0.6.3-stage6.pg-legacy-approval", "stage6/postgres-legacy-approval", []string{"code_change"})
	legacyCandidate, err := service.Create(ctx, domain.UserID, legacyInput, "pg-legacy-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	legacyCandidate, err = service.Transition(ctx, domain.UserID, legacyCandidate.ID, TransitionInput{
		ExpectedVersion: legacyCandidate.Version, TargetState: "ready_for_review",
		Reason: "Create a historical URL-only Release decision fixture for the upgrade fence.",
	}, "pg-legacy-ready", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("ALTER TABLE stage6_release_approvals DISABLE TRIGGER trg_stage6_release_approvals_insert").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.Stage6ReleaseApproval{
		ID: uuid.New(), CandidateRecordID: legacyCandidate.ID, OperatorTenantID: domain.TenantID,
		Role: "engineering", Decision: "approved", ApproverUserID: approvers[0],
		Reason:            "Historical Release decision retained a mutable URL-only evidence reference.",
		EvidenceReference: "https://evidence.example.test/postgres/legacy-release-engineering", CreatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("ALTER TABLE stage6_release_approvals ENABLE TRIGGER trg_stage6_release_approvals_insert").Error; err != nil {
		t.Fatal(err)
	}
	for index, role := range []string{"operations", "security", "product"} {
		legacyCandidate, err = service.RecordApproval(ctx, approvers[index+1], legacyCandidate.ID, ApprovalInput{
			Role: role, Decision: "approved", Reason: "Approved exact evidence while preserving one historical URL-only decision.",
			EvidenceReference: "https://evidence.example.test/postgres/legacy-release-" + role,
			EvidenceSHA256:    testReleaseApprovalDigest("postgres-legacy-" + role),
		}, "pg-legacy-approval-"+role, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
	}
	legacyReadiness, err := service.GetReadiness(ctx, legacyCandidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if legacyReadiness.Gates[2].ID != "release_approvals" || legacyReadiness.Gates[2].Satisfied {
		t.Fatalf("PostgreSQL legacy URL-only Release approval readiness = %#v", legacyReadiness.Gates[2])
	}
	if err := db.Model(&persistence.Stage6ReleaseCandidate{}).Where("id = ?", legacyCandidate.ID).Updates(map[string]any{
		"state": "approved", "version": legacyCandidate.Version + 1, "approved_at": now, "updated_at": now,
	}).Error; err == nil {
		t.Fatal("PostgreSQL allowed a legacy URL-only Release decision to authorize the candidate")
	}
	var forged persistence.Stage6ReleaseCandidate
	if err := db.Where("id = ?", candidate.ID).Take(&forged).Error; err != nil {
		t.Fatal(err)
	}
	forged.ID = uuid.New()
	forged.CandidateID = "v0.6.3-stage6.pg-forged"
	forged.State = "draft"
	forged.Version = 1
	forged.ApprovedAt = nil
	forged.CreatedAt = now
	forged.UpdatedAt = now
	if err := db.Create(&forged).Error; err == nil {
		t.Fatal("PostgreSQL accepted receipt bytes bound to another candidate identity")
	}

	projectionInput := newCandidateCreateInput("v0.6.3-stage6.pg-empty-projection", "stage6/postgres-empty-projection", []string{"code_change"})
	emptyProjection, err := service.normalizeCreate(domain.UserID, projectionInput)
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	if err := json.Unmarshal(emptyProjection.EvidenceBundleReceipt, &receipt); err != nil {
		t.Fatal(err)
	}
	receipt["receipts"].(map[string]any)["internalCost"] = map[string]any{}
	emptyProjection.EvidenceBundleReceipt, err = json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(emptyProjection.EvidenceBundleReceipt)
	emptyProjection.EvidenceBundleSHA256 = digest[:]
	emptyProjection.EvidenceBundleReceiptSizeBytes = int64(len(emptyProjection.EvidenceBundleReceipt))
	if err := db.Create(&emptyProjection).Error; err == nil {
		t.Fatal("PostgreSQL accepted an empty projected control receipt through a direct insert")
	}
	workerProjectionInput := newCandidateCreateInput("v0.6.3-stage6.pg-worker-projection", "stage6/postgres-worker-projection", []string{"code_change"})
	workerProjection, err := service.normalizeCreate(domain.UserID, workerProjectionInput)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(workerProjection.EvidenceBundleReceipt, &receipt); err != nil {
		t.Fatal(err)
	}
	receipt["receipts"].(map[string]any)["workerSupplyChain"].(map[string]any)["registryReportSha256"] = "sha256:" + repeat("0", 64)
	workerProjection.EvidenceBundleReceipt, err = json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	digest = sha256.Sum256(workerProjection.EvidenceBundleReceipt)
	workerProjection.EvidenceBundleSHA256 = digest[:]
	workerProjection.EvidenceBundleReceiptSizeBytes = int64(len(workerProjection.EvidenceBundleReceipt))
	if err := db.Create(&workerProjection).Error; err == nil {
		t.Fatal("PostgreSQL accepted an invalid Worker supply-chain projection through a direct insert")
	}
}
