package incidentexercisegovernance

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6incident"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestIncidentExerciseGovernancePostgresConvergesAuthoritiesAndProtectsReceipt(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "incident-exercise-pg-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	candidate := createPostgresIncidentExerciseCandidate(t, ctx, db, domain.TenantID, domain.UserID)
	now := time.Now().UTC().Truncate(time.Second)
	approvers := seedPostgresIncidentExerciseApprovers(t, db, domain.TenantID, domain.UserID, now)
	service := NewService(db, domain.TenantID)
	service.now = func() time.Time { return now }
	receipt, digest, err := stage6incident.Receipt(candidate.EnvironmentID)
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
				Reason:            "PostgreSQL authority reviewed the exact candidate-bound Incident exercise receipt.",
				EvidenceReference: "https://evidence.example.test/incident/postgres/" + role,
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
		t.Fatalf("PostgreSQL Incident exercise convergence = %#v", exercise)
	}
	if err := db.Model(&persistence.Stage6IncidentExercise{}).Where("id = ?", exercise.ID).Update("receipt", []byte(`{}`)).Error; err == nil {
		t.Fatal("PostgreSQL Incident exercise receipt was mutable")
	}
	if err := db.Model(&persistence.Stage6IncidentExerciseApproval{}).Where("incident_exercise_id = ?", exercise.ID).Update("reason", "mutated decision").Error; err == nil {
		t.Fatal("PostgreSQL Incident exercise approval was mutable")
	}
}

func TestIncidentExerciseApprovalDigestMigrationReopensLegacyPostgresExercise(t *testing.T) {
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
	if err := database.Migrate(ctx, db, incidentExerciseMigrationsThrough(t, "000145_stage6_capacity_approval_evidence_digests.sql")); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "incident-exercise-digest-upgrade-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	candidate := createPostgresIncidentExerciseCandidate(t, ctx, db, domain.TenantID, domain.UserID)
	now := time.Now().UTC().Truncate(time.Second)
	approvers := seedPostgresIncidentExerciseApprovers(t, db, domain.TenantID, domain.UserID, now)
	receipt, digest, err := stage6incident.Receipt(candidate.EnvironmentID)
	if err != nil {
		t.Fatal(err)
	}
	model, err := bindReceipt(ImportInput{
		CandidateRecordID: candidate.ID, ReceiptBase64: base64.StdEncoding.EncodeToString(receipt), ReceiptSHA256: digest,
	}, candidate, now)
	if err != nil {
		t.Fatal(err)
	}
	model.ID = uuid.New()
	model.OperatorTenantID = domain.TenantID
	model.CandidateRecordID = candidate.ID
	model.CreatedBy = domain.UserID
	model.CreatedAt = now
	model.UpdatedAt = now
	if err := db.Create(&model).Error; err != nil {
		t.Fatal(err)
	}
	legacyIDs := make([]uuid.UUID, 0, len(approvalRoles))
	for index, role := range approvalRoles {
		approval := persistence.Stage6IncidentExerciseApproval{
			ID: uuid.New(), IncidentExerciseID: model.ID, OperatorTenantID: domain.TenantID,
			Role: role, Decision: "approved", ApproverUserID: approvers[index],
			Reason:            "Legacy Incident exercise decision referenced mutable evidence without an exact digest.",
			EvidenceReference: "https://evidence.example.test/incident/legacy/" + role, CreatedAt: now,
		}
		if err := db.Omit("EvidenceSHA256", "SupersededAt", "SupersededReason").Create(&approval).Error; err != nil {
			t.Fatal(err)
		}
		legacyIDs = append(legacyIDs, approval.ID)
	}
	if err := db.Model(&persistence.Stage6IncidentExercise{}).Where("id = ?", model.ID).Updates(map[string]any{
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
	if model.State != "recorded" || model.Version != 3 || model.ApprovedAt != nil {
		t.Fatalf("legacy Incident exercise after Migration 000146 = %#v", model)
	}
	for _, approvalID := range legacyIDs {
		var legacy persistence.Stage6IncidentExerciseApproval
		if err := db.First(&legacy, "id = ?", approvalID).Error; err != nil {
			t.Fatal(err)
		}
		if legacy.SupersededAt == nil || legacy.SupersededReason == nil || !strings.Contains(*legacy.SupersededReason, "Migration 000146") {
			t.Fatalf("legacy Incident exercise approval was not superseded: %#v", legacy)
		}
	}
	service := NewService(db, domain.TenantID)
	service.now = func() time.Time { return now.Add(time.Second) }
	var exercise Exercise
	for index, role := range approvalRoles {
		exercise, err = service.RecordApproval(ctx, approvers[index], model.ID, ApprovalInput{
			Role: role, Decision: "approved", Reason: "Replacement authority reviewed the exact byte-bound Incident exercise decision evidence.",
			EvidenceReference: "https://evidence.example.test/incident/replacement/" + role,
			EvidenceSHA256:    "sha256:" + strings.Repeat(string(rune('2'+index)), 64),
		}, "replacement-"+role, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
	}
	if exercise.State != "approved" || exercise.Version != 4 {
		t.Fatalf("replacement Incident exercise = %#v", exercise)
	}
}

func incidentExerciseMigrationsThrough(t *testing.T, tail string) fs.FS {
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

func createPostgresIncidentExerciseCandidate(t *testing.T, ctx context.Context, db *gorm.DB, tenantID, creatorID uuid.UUID) persistence.Stage6ReleaseCandidate {
	t.Helper()
	encoded, _, err := stage6candidate.Receipt("v0.6.3-stage6.incident-exercise-pg", "stage6/incident-exercise-pg")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	view, err := releasegovernance.NewService(db, tenantID).Create(ctx, creatorID, releasegovernance.CreateInput{
		CandidateID: "v0.6.3-stage6.incident-exercise-pg", SourceCommit: strings.Repeat("a", 40),
		LockfileSHA256:              "sha256:" + strings.Repeat("b", 64),
		EvidenceBundleSHA256:        "sha256:" + hex.EncodeToString(digest[:]),
		EvidenceBundleReceiptBase64: base64.StdEncoding.EncodeToString(encoded),
		FinalAssetSetSHA256:         "sha256:" + strings.Repeat("d", 64),
		EnvironmentID:               "stage6/incident-exercise-pg", ImpactDomains: []string{"code_change"},
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

func seedPostgresIncidentExerciseApprovers(t *testing.T, db *gorm.DB, tenantID, ownerID uuid.UUID, now time.Time) []uuid.UUID {
	t.Helper()
	approvers := make([]uuid.UUID, 0, len(approvalRoles))
	for _, role := range approvalRoles {
		user := persistence.User{
			ID: uuid.New(), Email: "incident-exercise-pg-" + role + "@example.test",
			DisplayName: "Incident exercise PG " + role, Status: "active",
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
			AuthorityKey: "incident_exercise." + role, Status: "active", Version: 1,
			ExpiresAt: now.Add(180 * 24 * time.Hour), GrantedBy: ownerID,
			Reason:            "Owner assigned the exact PostgreSQL Incident exercise governance function.",
			EvidenceReference: "https://evidence.example.test/authority/incident-exercise-pg/" + role,
			EvidenceSHA256:    stage6authority.DigestPointer("incident-exercise-pg-" + role),
			CreatedAt:         now, UpdatedAt: now,
		}).Error; err != nil {
			t.Fatal(err)
		}
		approvers = append(approvers, user.ID)
	}
	return approvers
}
