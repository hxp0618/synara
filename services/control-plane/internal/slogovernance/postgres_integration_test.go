package slogovernance

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/releasegovernance"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/postgresisolation"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6authority"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestSLOGovernancePostgresConvergesSeparatedApprovalsAndProtectsReceipt(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "slo-governance-pg-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := releasegovernance.NewService(db, domain.TenantID).Create(ctx, domain.UserID, candidateInput(), "pg-candidate", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	approvers := make([]uuid.UUID, 0, len(approvalRoles))
	for _, role := range approvalRoles {
		user := persistence.User{ID: uuid.New(), Email: "slo-pg-" + role + "@example.test", DisplayName: "SLO PG " + role, Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now}
		if err := db.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
		membershipRole := "admin"
		if role == "security" {
			membershipRole = "security_admin"
		}
		if err := db.Create(&persistence.TenantMembership{TenantID: domain.TenantID, UserID: user.ID, Role: membershipRole, Status: "active", JoinedAt: &now, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&persistence.Stage6GovernanceAuthorityGrant{ID: uuid.New(), OperatorTenantID: domain.TenantID, UserID: user.ID, AuthorityKey: "release." + role, Status: "active", Version: 1, ExpiresAt: now.Add(180 * 24 * time.Hour), GrantedBy: domain.UserID, Reason: "Owner assigned exact PostgreSQL SLO review authority.", EvidenceReference: "https://evidence.example.test/slo-pg/authority/" + role, EvidenceSHA256: stage6authority.DigestPointer("slo-pg-" + role), CreatedAt: now, UpdatedAt: now}).Error; err != nil {
			t.Fatal(err)
		}
		approvers = append(approvers, user.ID)
	}
	receiptBytes, err := json.Marshal(sloReceiptMap(true))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(receiptBytes)
	service := NewService(db, domain.TenantID)
	window, err := service.Import(ctx, domain.UserID, ImportInput{CandidateRecordID: candidate.ID, ReceiptBase64: base64.StdEncoding.EncodeToString(receiptBytes), ReceiptSHA256: "sha256:" + hex.EncodeToString(digest[:])}, "pg-import", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	var wait sync.WaitGroup
	errorsByRole := make(chan error, len(approvalRoles))
	for index, role := range approvalRoles {
		wait.Add(1)
		go func(index int, role string) {
			defer wait.Done()
			_, approvalErr := service.RecordApproval(ctx, approvers[index], window.ID, ApprovalInput{Role: role, Decision: "approved", Reason: "PostgreSQL authority reviewed the exact immutable SLO window receipt.", EvidenceReference: "https://evidence.example.test/slo-pg/approval/" + role, EvidenceSHA256: "sha256:" + strings.Repeat(string(rune('2'+index)), 64)}, "pg-approval-"+role, "127.0.0.1")
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
	window, err = service.get(ctx, window.ID)
	if err != nil {
		t.Fatal(err)
	}
	if window.State != "approved" || len(window.Approvals) != 4 {
		t.Fatalf("PostgreSQL SLO window = %#v", window)
	}
	if err := db.Model(&persistence.Stage6SLOWindow{}).Where("id = ?", window.ID).Update("receipt", []byte(`{}`)).Error; err == nil {
		t.Fatal("PostgreSQL SLO receipt was mutable")
	}
	if err := db.Model(&persistence.Stage6SLOApproval{}).Where("slo_window_record_id = ?", window.ID).Update("reason", "forged").Error; err == nil {
		t.Fatal("PostgreSQL SLO approval was mutable")
	}
}
