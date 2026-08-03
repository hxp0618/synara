package penetrationgovernance

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
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6penetration"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPenetrationGovernancePostgresConvergesThreeAuthoritiesAndProtectsReceipt(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "penetration-pg-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	candidate := createPostgresPenetrationCandidate(t, ctx, db, domain.TenantID, domain.UserID)
	now := time.Now().UTC().Truncate(time.Second)
	approvers := seedPostgresPenetrationApprovers(t, db, domain.TenantID, domain.UserID, now)
	service := NewService(db, domain.TenantID)
	service.now = func() time.Time { return now }
	receipt, digest, err := stage6penetration.Receipt(candidate.EnvironmentID)
	if err != nil {
		t.Fatal(err)
	}
	engagement, err := service.Import(ctx, domain.UserID, ImportInput{CandidateRecordID: candidate.ID, ReceiptBase64: base64.StdEncoding.EncodeToString(receipt), ReceiptSHA256: digest}, "pg-import", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	var wait sync.WaitGroup
	errorsByRole := make(chan error, len(approvalRoles))
	for index, role := range approvalRoles {
		wait.Add(1)
		go func(index int, role string) {
			defer wait.Done()
			_, approvalErr := service.RecordApproval(ctx, approvers[index], engagement.ID, ApprovalInput{Role: role, Decision: "approved", Reason: "PostgreSQL authority reviewed the exact candidate-bound Penetration receipt.", EvidenceReference: "https://evidence.example.test/penetration/postgres/" + role, EvidenceSHA256: "sha256:" + strings.Repeat(string(rune('2'+index)), 64)}, "pg-approval-"+role, "127.0.0.1")
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
	engagement, err = service.get(ctx, engagement.ID)
	if err != nil {
		t.Fatal(err)
	}
	if engagement.State != "approved" || len(engagement.Approvals) != 3 {
		t.Fatalf("PostgreSQL Penetration convergence = %#v", engagement)
	}
	if err := db.Model(&persistence.Stage6PenetrationEngagement{}).Where("id = ?", engagement.ID).Update("receipt", []byte(`{}`)).Error; err == nil {
		t.Fatal("PostgreSQL Penetration receipt was mutable")
	}
	if err := db.Model(&persistence.Stage6PenetrationApproval{}).Where("penetration_engagement_id = ?", engagement.ID).Update("reason", "mutated decision").Error; err == nil {
		t.Fatal("PostgreSQL Penetration approval was mutable")
	}
}

func TestPenetrationApprovalDigestMigrationReopensLegacyPostgresEngagement(t *testing.T) {
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
	if err := database.Migrate(ctx, db, penetrationMigrationsThrough(t, "000143_stage6_recovery_approval_evidence_digests.sql")); err != nil {
		t.Fatal(err)
	}
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "penetration-digest-upgrade-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	candidate := createPostgresPenetrationCandidate(t, ctx, db, domain.TenantID, domain.UserID)
	now := time.Now().UTC().Truncate(time.Second)
	approvers := seedPostgresPenetrationApprovers(t, db, domain.TenantID, domain.UserID, now)
	receipt, digest, err := stage6penetration.Receipt(candidate.EnvironmentID)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := bindReceipt(ImportInput{
		CandidateRecordID: candidate.ID, ReceiptBase64: base64.StdEncoding.EncodeToString(receipt), ReceiptSHA256: digest,
	}, candidate, now)
	if err != nil {
		t.Fatal(err)
	}
	model := binding.model
	model.ID = uuid.New()
	model.OperatorTenantID = domain.TenantID
	model.CandidateRecordID = candidate.ID
	model.CreatedBy = domain.UserID
	model.CreatedAt = now
	model.UpdatedAt = now
	if err := db.Create(&model).Error; err != nil {
		t.Fatal(err)
	}
	for _, asset := range binding.assets {
		asset.ID = uuid.New()
		asset.PenetrationEngagementID = model.ID
		asset.OperatorTenantID = domain.TenantID
		asset.CreatedAt = now
		if err := db.Create(&asset).Error; err != nil {
			t.Fatal(err)
		}
	}
	legacyIDs := make([]uuid.UUID, 0, len(approvalRoles))
	for index, role := range approvalRoles {
		approval := persistence.Stage6PenetrationApproval{
			ID: uuid.New(), PenetrationEngagementID: model.ID, OperatorTenantID: domain.TenantID,
			Role: role, Decision: "approved", ApproverUserID: approvers[index],
			Reason:            "Legacy Penetration decision referenced mutable evidence without an exact digest.",
			EvidenceReference: "https://evidence.example.test/penetration/legacy/" + role, CreatedAt: now,
		}
		if err := db.Omit("EvidenceSHA256", "SupersededAt", "SupersededReason").Create(&approval).Error; err != nil {
			t.Fatal(err)
		}
		legacyIDs = append(legacyIDs, approval.ID)
	}
	if err := db.Model(&persistence.Stage6PenetrationEngagement{}).Where("id = ?", model.ID).Updates(map[string]any{
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
		t.Fatalf("legacy Penetration engagement after Migration 000144 = %#v", model)
	}
	for _, approvalID := range legacyIDs {
		var legacy persistence.Stage6PenetrationApproval
		if err := db.First(&legacy, "id = ?", approvalID).Error; err != nil {
			t.Fatal(err)
		}
		if legacy.SupersededAt == nil || legacy.SupersededReason == nil || !strings.Contains(*legacy.SupersededReason, "Migration 000144") {
			t.Fatalf("legacy Penetration approval was not superseded: %#v", legacy)
		}
	}
	service := NewService(db, domain.TenantID)
	service.now = func() time.Time { return now.Add(time.Second) }
	var engagement Engagement
	for index, role := range approvalRoles {
		engagement, err = service.RecordApproval(ctx, approvers[index], model.ID, ApprovalInput{
			Role: role, Decision: "approved", Reason: "Replacement authority reviewed the exact byte-bound Penetration decision evidence.",
			EvidenceReference: "https://evidence.example.test/penetration/replacement/" + role,
			EvidenceSHA256:    "sha256:" + strings.Repeat(string(rune('2'+index)), 64),
		}, "replacement-"+role, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
	}
	if engagement.State != "approved" || engagement.Version != 4 {
		t.Fatalf("replacement Penetration engagement = %#v", engagement)
	}
}

func penetrationMigrationsThrough(t *testing.T, tail string) fs.FS {
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

func createPostgresPenetrationCandidate(t *testing.T, ctx context.Context, db *gorm.DB, tenantID, creatorID uuid.UUID) persistence.Stage6ReleaseCandidate {
	t.Helper()
	encoded, _, err := stage6candidate.Receipt("v0.6.3-stage6.penetration-pg", "stage6/penetration-pg")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	view, err := releasegovernance.NewService(db, tenantID).Create(ctx, creatorID, releasegovernance.CreateInput{CandidateID: "v0.6.3-stage6.penetration-pg", SourceCommit: strings.Repeat("a", 40), LockfileSHA256: "sha256:" + strings.Repeat("b", 64), EvidenceBundleSHA256: "sha256:" + hex.EncodeToString(digest[:]), EvidenceBundleReceiptBase64: base64.StdEncoding.EncodeToString(encoded), FinalAssetSetSHA256: "sha256:" + strings.Repeat("d", 64), EnvironmentID: "stage6/penetration-pg", ImpactDomains: []string{"runtime_isolation"}}, "pg-candidate", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	var candidate persistence.Stage6ReleaseCandidate
	if err := db.Where("id = ?", view.ID).Take(&candidate).Error; err != nil {
		t.Fatal(err)
	}
	return candidate
}

func seedPostgresPenetrationApprovers(t *testing.T, db *gorm.DB, tenantID, ownerID uuid.UUID, now time.Time) []uuid.UUID {
	t.Helper()
	approvers := make([]uuid.UUID, 0, len(approvalRoles))
	for _, role := range approvalRoles {
		user := persistence.User{ID: uuid.New(), Email: "penetration-pg-" + role + "@example.test", DisplayName: "Penetration PG " + role, Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now}
		if err := db.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
		membershipRole := "admin"
		if role == "security" {
			membershipRole = "security_admin"
		}
		if err := db.Create(&persistence.TenantMembership{TenantID: tenantID, UserID: user.ID, Role: membershipRole, Status: "active", JoinedAt: &now, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&persistence.Stage6GovernanceAuthorityGrant{ID: uuid.New(), OperatorTenantID: tenantID, UserID: user.ID, AuthorityKey: "penetration." + role, Status: "active", Version: 1, ExpiresAt: now.Add(180 * 24 * time.Hour), GrantedBy: ownerID, Reason: "Owner assigned the exact PostgreSQL Penetration governance function.", EvidenceReference: "https://evidence.example.test/authority/penetration-pg/" + role, EvidenceSHA256: stage6authority.DigestPointer("penetration-pg-" + role), CreatedAt: now, UpdatedAt: now}).Error; err != nil {
			t.Fatal(err)
		}
		approvers = append(approvers, user.ID)
	}
	return approvers
}
