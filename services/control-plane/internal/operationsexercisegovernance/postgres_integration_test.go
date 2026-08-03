package operationsexercisegovernance

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/releasegovernance"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/postgresisolation"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6authority"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6candidate"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6operations"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestOperationsExerciseGovernancePostgresConvergesAuthoritiesAndProtectsReceipt(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "operations-exercise-pg-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	candidate := createPostgresOperationsExerciseCandidate(t, ctx, db, domain.TenantID, domain.UserID, false)
	now := time.Now().UTC().Truncate(time.Second)
	approvers := seedPostgresOperationsExerciseApprovers(t, db, domain.TenantID, domain.UserID, now)
	service := NewService(db, domain.TenantID)
	service.now = func() time.Time { return now }
	receipt, digest, err := stage6operations.Receipt(candidate.CandidateID, candidate.EnvironmentID)
	if err != nil {
		t.Fatal(err)
	}
	exercise, err := service.Import(ctx, domain.UserID, ImportInput{
		CandidateRecordID: candidate.ID, ReceiptBase64: base64.StdEncoding.EncodeToString(receipt), ReceiptSHA256: digest,
	}, "pg-import", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	var wait sync.WaitGroup
	errorsByRole := make(chan error, len(approvalRoles))
	for index, role := range approvalRoles {
		wait.Add(1)
		go func(index int, role string) {
			defer wait.Done()
			_, approvalErr := service.RecordApproval(ctx, approvers[index], exercise.ID, ApprovalInput{
				Role: role, Decision: "approved",
				Reason:            "PostgreSQL authority reviewed the exact candidate-bound Operations exercise receipt.",
				EvidenceReference: "https://evidence.example.test/operations/postgres/" + role,
				EvidenceSHA256:    "sha256:" + strings.Repeat(string(rune('2'+index)), 64),
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
	exercise, err = service.get(ctx, exercise.ID)
	if err != nil {
		t.Fatal(err)
	}
	if exercise.State != "approved" || len(exercise.Approvals) != 2 {
		t.Fatalf("PostgreSQL Operations exercise convergence = %#v", exercise)
	}
	if err := db.Model(&persistence.Stage6OperationsExercise{}).Where("id = ?", exercise.ID).Update("receipt", []byte(`{}`)).Error; err == nil {
		t.Fatal("PostgreSQL Operations exercise receipt was mutable")
	}
	if err := db.Model(&persistence.Stage6OperationsExerciseApproval{}).Where("operations_exercise_id = ?", exercise.ID).Update("reason", "mutated decision").Error; err == nil {
		t.Fatal("PostgreSQL Operations exercise approval was mutable")
	}
}

func TestOperationsSupportTerminalAuditMigrationReopensV1PostgresExercise(t *testing.T) {
	databaseURL := os.Getenv("TEST_POSTGRES_URL")
	if databaseURL == "" {
		t.Skip("TEST_POSTGRES_URL is not configured")
	}
	ctx := context.Background()
	db, err := database.Open(ctx, postgresisolation.URL(t, databaseURL))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := database.Migrate(ctx, db, operationsExerciseMigrationsThrough(t, "000161_internal_incident_communications.sql")); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "operations-exercise-v2-upgrade-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	candidate := createPostgresOperationsExerciseCandidate(t, ctx, db, domain.TenantID, domain.UserID, true)
	now := time.Now().UTC().Truncate(time.Second)
	approvers := seedPostgresOperationsExerciseApprovers(t, db, domain.TenantID, domain.UserID, now)
	receipt, digest, err := stage6operations.LegacyV1Receipt(candidate.CandidateID, candidate.EnvironmentID)
	if err != nil {
		t.Fatal(err)
	}
	v2Receipt, v2Digest, err := stage6operations.Receipt(candidate.CandidateID, candidate.EnvironmentID)
	if err != nil {
		t.Fatal(err)
	}
	v2Candidate := candidate
	var candidateReceipt map[string]any
	if err := json.Unmarshal(v2Candidate.EvidenceBundleReceipt, &candidateReceipt); err != nil {
		t.Fatal(err)
	}
	candidateReceipt["receipts"].(map[string]any)["operations"].(map[string]any)["sha256"] = v2Digest
	candidateReceipt["receipts"].(map[string]any)["operations"].(map[string]any)["schemaVersion"] =
		"synara.stage6-operations-browser-exercise-validation.v2"
	v2Candidate.EvidenceBundleReceipt, err = json.Marshal(candidateReceipt)
	if err != nil {
		t.Fatal(err)
	}
	model, err := bindReceipt(ImportInput{
		CandidateRecordID: candidate.ID, ReceiptBase64: base64.StdEncoding.EncodeToString(v2Receipt), ReceiptSHA256: v2Digest,
	}, v2Candidate, now)
	if err != nil {
		t.Fatal(err)
	}
	legacyDigest, err := hex.DecodeString(strings.TrimPrefix(digest, "sha256:"))
	if err != nil {
		t.Fatal(err)
	}
	model.Receipt = receipt
	model.ReceiptSHA256 = legacyDigest
	model.ReceiptSizeBytes = int64(len(receipt))
	model.Schema = "synara.stage6-operations-browser-exercise-validation.v1"
	model.ID = uuid.New()
	model.OperatorTenantID = domain.TenantID
	model.CandidateRecordID = candidate.ID
	model.CreatedBy = domain.UserID
	model.CreatedAt = now
	model.UpdatedAt = now
	if err := db.Create(&model).Error; err != nil {
		t.Fatal(err)
	}
	approvalIDs := make([]uuid.UUID, 0, len(approvalRoles))
	for index, role := range approvalRoles {
		evidenceSHA256 := "sha256:" + strings.Repeat(string(rune('2'+index)), 64)
		approval := persistence.Stage6OperationsExerciseApproval{
			ID: uuid.New(), OperationsExerciseID: model.ID, OperatorTenantID: domain.TenantID,
			Role: role, Decision: "approved", ApproverUserID: approvers[index],
			Reason:            "Operations v1 authority reviewed byte-bound evidence before terminal Support audit actions became mandatory.",
			EvidenceReference: "https://evidence.example.test/operations/v1/" + role,
			EvidenceSHA256:    &evidenceSHA256, CreatedAt: now,
		}
		if err := db.Create(&approval).Error; err != nil {
			t.Fatal(err)
		}
		approvalIDs = append(approvalIDs, approval.ID)
	}
	if err := db.Model(&persistence.Stage6OperationsExercise{}).Where("id = ?", model.ID).Updates(map[string]any{
		"state": "approved", "version": 2, "approved_at": now, "updated_at": now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&model, "id = ?", model.ID).Error; err != nil {
		t.Fatal(err)
	}
	if model.State != "recorded" || model.Version != 3 || model.ApprovedAt != nil ||
		model.Schema != "synara.stage6-operations-browser-exercise-validation.v1" {
		t.Fatalf("Operations v1 exercise after Migration 000162 = %#v", model)
	}
	for _, approvalID := range approvalIDs {
		var approval persistence.Stage6OperationsExerciseApproval
		if err := db.First(&approval, "id = ?", approvalID).Error; err != nil {
			t.Fatal(err)
		}
		if approval.SupersededAt == nil || approval.SupersededReason == nil ||
			!strings.Contains(*approval.SupersededReason, "Migration 000162") {
			t.Fatalf("Operations v1 approval retained authority: %#v", approval)
		}
	}
	service := NewService(db, domain.TenantID)
	service.now = func() time.Time { return now.Add(time.Second) }
	if _, err := service.RecordApproval(ctx, approvers[0], model.ID, ApprovalInput{
		Role: "operations", Decision: "approved",
		Reason:            "Historical Operations v1 must not regain positive release authority.",
		EvidenceReference: "https://evidence.example.test/operations/v1-forbidden",
		EvidenceSHA256:    "sha256:" + strings.Repeat("f", 64),
	}, "v1-forbidden", "127.0.0.1"); err == nil {
		t.Fatal("Migration 000162 allowed a historical Operations v1 exercise to regain approval")
	}
}

func operationsExerciseMigrationsThrough(t *testing.T, tail string) fs.FS {
	t.Helper()
	entries, err := fs.ReadDir(migrations.Files, ".")
	if err != nil {
		t.Fatal(err)
	}
	result := fstest.MapFS{}
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() > tail {
			continue
		}
		data, err := fs.ReadFile(migrations.Files, entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		result[entry.Name()] = &fstest.MapFile{Data: data}
	}
	return result
}

func createPostgresOperationsExerciseCandidate(t *testing.T, ctx context.Context, db *gorm.DB, tenantID, creatorID uuid.UUID, legacy bool) persistence.Stage6ReleaseCandidate {
	t.Helper()
	candidateID, environmentID := "v0.6.3-stage6.operations-exercise-pg", "stage6/operations-exercise-pg"
	encoded, _, err := stage6candidate.Receipt(candidateID, environmentID)
	if legacy {
		_, legacyOperationsDigest, legacyErr := stage6operations.LegacyV1Receipt(candidateID, environmentID)
		if legacyErr != nil {
			t.Fatal(legacyErr)
		}
		var receipt map[string]any
		if err := json.Unmarshal(encoded, &receipt); err != nil {
			t.Fatal(err)
		}
		operations := receipt["receipts"].(map[string]any)["operations"].(map[string]any)
		operations["schemaVersion"] = "synara.stage6-operations-browser-exercise-validation.v1"
		operations["sha256"] = legacyOperationsDigest
		encoded, err = json.Marshal(receipt)
	}
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	if legacy {
		now := time.Now().UTC().Truncate(time.Microsecond)
		lockfileSHA256, _ := hex.DecodeString(strings.Repeat("b", 64))
		finalAssetSetSHA256, _ := hex.DecodeString(strings.Repeat("d", 64))
		candidate := persistence.Stage6ReleaseCandidate{
			ID: uuid.New(), OperatorTenantID: tenantID, CandidateID: candidateID,
			SourceCommit: strings.Repeat("a", 40), LockfileSHA256: lockfileSHA256,
			EvidenceBundleSHA256: digest[:], EvidenceBundleReceipt: encoded,
			EvidenceBundleSchema:     "synara.stage6-candidate-evidence-bundle-validation.v5",
			EvidenceBundleAssessment: "evidence-consistent-not-ga-approved", EvidenceBundleValidatedAt: "2026-07-31T12:00:00Z",
			EvidenceBundleReceiptSizeBytes: int64(len(encoded)), DesktopArtifactSetSHA256: "sha256:" + strings.Repeat("e", 64),
			EvidenceReceiptBound: true, FinalAssetSetSHA256: finalAssetSetSHA256, EnvironmentID: environmentID,
			ImpactDomains: []string{"code_change"}, State: "draft", Version: 1, CreatedBy: creatorID,
			ResidualRisks: []map[string]any{}, CreatedAt: now, UpdatedAt: now,
		}
		if err := db.Create(&candidate).Error; err != nil {
			t.Fatal(err)
		}
		return candidate
	}
	view, err := releasegovernance.NewService(db, tenantID).Create(ctx, creatorID, releasegovernance.CreateInput{
		CandidateID: candidateID, SourceCommit: strings.Repeat("a", 40),
		LockfileSHA256:              "sha256:" + strings.Repeat("b", 64),
		EvidenceBundleSHA256:        "sha256:" + hex.EncodeToString(digest[:]),
		EvidenceBundleReceiptBase64: base64.StdEncoding.EncodeToString(encoded),
		FinalAssetSetSHA256:         "sha256:" + strings.Repeat("d", 64),
		EnvironmentID:               environmentID, ImpactDomains: []string{"code_change"},
	}, "pg-candidate", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	var candidate persistence.Stage6ReleaseCandidate
	if err := db.Where("id = ?", view.ID).Take(&candidate).Error; err != nil {
		t.Fatal(err)
	}
	return candidate
}

func seedPostgresOperationsExerciseApprovers(t *testing.T, db *gorm.DB, tenantID, ownerID uuid.UUID, now time.Time) []uuid.UUID {
	t.Helper()
	approvers := make([]uuid.UUID, 0, len(approvalRoles))
	for _, role := range approvalRoles {
		user := persistence.User{
			ID: uuid.New(), Email: "operations-exercise-pg-" + role + "@example.test",
			DisplayName: "Operations exercise PG " + role, Status: "active",
			EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now,
		}
		if err := db.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&persistence.TenantMembership{
			TenantID: tenantID, UserID: user.ID, Role: "admin", Status: "active",
			JoinedAt: &now, CreatedAt: now, UpdatedAt: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&persistence.Stage6GovernanceAuthorityGrant{
			ID: uuid.New(), OperatorTenantID: tenantID, UserID: user.ID,
			AuthorityKey: "operations_exercise." + role, Status: "active", Version: 1,
			ExpiresAt: now.Add(180 * 24 * time.Hour), GrantedBy: ownerID,
			Reason:            "Owner assigned the exact PostgreSQL Operations exercise governance function.",
			EvidenceReference: "https://evidence.example.test/authority/operations-exercise-pg/" + role,
			EvidenceSHA256:    stage6authority.DigestPointer("operations-exercise-pg-" + role),
			CreatedAt:         now, UpdatedAt: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
		approvers = append(approvers, user.ID)
	}
	return approvers
}
