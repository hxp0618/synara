package internalcostgovernance

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/releasegovernance"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/postgresisolation"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6authority"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6candidate"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6internalcost"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestInternalCostGovernanceBindsUsageTokensCostsAndSeparatedAuthorities(t *testing.T) {
	fixture := newInternalCostFixture(t)
	review, err := fixture.service.Import(fixture.ctx, fixture.creatorID, fixture.importInput(t), "cost-import", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if review.State != "recorded" || review.ExecutionCount != 2 || review.InputTokens != 1000 ||
		review.KnownCostByCurrency["USD"] != 24000 || review.KnownCostByCurrency["CNY"] != 3200 ||
		!review.NoPaymentDataPresent || !review.ActualOverridesEstimate {
		t.Fatalf("imported internal cost review = %#v", review)
	}
	_, err = fixture.service.RecordApproval(fixture.ctx, fixture.creatorID, review.ID, ApprovalInput{
		Role: "operations", Decision: "approved", Reason: "The importer cannot approve their own internal cost evidence.",
		EvidenceReference: "https://evidence.example.test/internal-cost/self", EvidenceSHA256: "sha256:" + strings.Repeat("1", 64),
	}, "self", "127.0.0.1")
	assertInternalCostProblem(t, err, "internal_cost_review_not_reviewable")
	for index, role := range approvalRoles {
		review, err = fixture.service.RecordApproval(fixture.ctx, fixture.approverIDs[index], review.ID, ApprovalInput{
			Role: role, Decision: "approved", Reason: "The assigned authority reviewed the exact internal usage and cost evidence bytes.",
			EvidenceReference: "https://evidence.example.test/internal-cost/" + role,
			EvidenceSHA256:    "sha256:" + strings.Repeat(string(rune('2'+index)), 64),
		}, "approval-"+role, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
	}
	if review.State != "approved" || review.ApprovedAt == nil || len(review.Approvals) != 2 {
		t.Fatalf("approved internal cost review = %#v", review)
	}
	if err := fixture.db.Model(&persistence.Stage6InternalCostReview{}).Where("id = ?", review.ID).Update("receipt", []byte(`{}`)).Error; err == nil {
		t.Fatal("internal cost receipt was mutable")
	}
	if err := fixture.db.Model(&persistence.Stage6InternalCostReviewApproval{}).Where("internal_cost_review_id = ?", review.ID).Update("reason", "mutated").Error; err == nil {
		t.Fatal("internal cost approval was mutable")
	}
}

func TestInternalCostGovernanceRejectsCostArithmeticAndCoverageDrift(t *testing.T) {
	fixture := newInternalCostFixture(t)
	for name, mutate := range map[string]func(map[string]any){
		"known cost arithmetic": func(receipt map[string]any) {
			receipt["costs"].(map[string]any)["knownByCurrency"].(map[string]any)["USD"] = float64(23999)
		},
		"provider coverage": func(receipt map[string]any) {
			receipt["usage"].(map[string]any)["providerCostUnavailableExecutionCount"] = float64(0)
		},
		"payment field": func(receipt map[string]any) {
			receipt["candidate"].(map[string]any)["stripeMode"] = "disabled"
		},
	} {
		t.Run(name, func(t *testing.T) {
			input := fixture.mutatedInput(t, mutate)
			_, _, err := bindReceipt(input, fixture.candidate, fixture.now)
			assertInternalCostProblem(t, err, "internal_cost_receipt_invalid")
		})
	}
	review, err := fixture.service.Import(fixture.ctx, fixture.creatorID, fixture.importInput(t), "valid", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&persistence.Stage6InternalCostReview{}).Where("id = ?", review.ID).Updates(map[string]any{
		"state": "approved", "version": 2, "approved_at": fixture.now,
	}).Error; err == nil {
		t.Fatal("SQLite allowed internal cost approval without separated decisions")
	}
}

func TestInternalCostGovernancePostgresSerializesSeparatedApprovalsAndRejectsBypass(t *testing.T) {
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
	if err := database.Migrate(ctx, db, migrations.Files); err != nil {
		t.Fatal(err)
	}
	fixture := newInternalCostFixtureOnDB(t, ctx, db)
	review, err := fixture.service.Import(ctx, fixture.creatorID, fixture.importInput(t), "postgres-cost-import", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errorsByRole := make([]error, len(approvalRoles))
	var wait sync.WaitGroup
	for index, role := range approvalRoles {
		wait.Add(1)
		go func(index int, role string) {
			defer wait.Done()
			<-start
			_, errorsByRole[index] = fixture.service.RecordApproval(ctx, fixture.approverIDs[index], review.ID, ApprovalInput{
				Role: role, Decision: "approved", Reason: "The assigned authority reviewed the exact PostgreSQL internal cost evidence bytes.",
				EvidenceReference: "https://evidence.example.test/internal-cost/postgres/" + role,
				EvidenceSHA256:    "sha256:" + strings.Repeat(string(rune('4'+index)), 64),
			}, "postgres-approval-"+role, "127.0.0.1")
		}(index, role)
	}
	close(start)
	wait.Wait()
	for index, err := range errorsByRole {
		if err != nil {
			t.Fatalf("concurrent %s approval failed: %v", approvalRoles[index], err)
		}
	}
	review, err = fixture.service.get(ctx, review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if review.State != "approved" || len(review.Approvals) != len(approvalRoles) {
		t.Fatalf("PostgreSQL internal cost review = %#v", review)
	}
	if err := db.Exec(
		`UPDATE stage6_internal_cost_reviews SET receipt = convert_to(repeat('x', receipt_size_bytes::integer), 'UTF8') WHERE id = ?`, review.ID,
	).Error; err == nil {
		t.Fatal("PostgreSQL allowed internal cost receipt mutation")
	}
	if err := db.Delete(&persistence.Stage6InternalCostReviewApproval{}, "internal_cost_review_id = ?", review.ID).Error; err == nil {
		t.Fatal("PostgreSQL allowed internal cost approval deletion")
	}
}

type internalCostFixture struct {
	ctx         context.Context
	db          *gorm.DB
	service     *Service
	creatorID   uuid.UUID
	candidate   persistence.Stage6ReleaseCandidate
	approverIDs []uuid.UUID
	now         time.Time
}

func newInternalCostFixture(t *testing.T) internalCostFixture {
	t.Helper()
	ctx := context.Background()
	profile, err := platform.Defaults(platform.ProfilePersonal)
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenMetadataStore(ctx, profile, "", filepath.Join(t.TempDir(), "metadata.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx, migrations.Files); err != nil {
		t.Fatal(err)
	}
	return newInternalCostFixtureOnDB(t, ctx, store.DB())
}

func newInternalCostFixtureOnDB(t *testing.T, ctx context.Context, db *gorm.DB) internalCostFixture {
	t.Helper()
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "internal-cost-governance-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	candidateID, environmentID := "v0.6.3-stage6.internal-cost", "stage6/internal-cost"
	candidateReceipt, _, err := stage6candidate.Receipt(candidateID, environmentID)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(candidateReceipt)
	view, err := releasegovernance.NewService(db, domain.TenantID).Create(ctx, domain.UserID, releasegovernance.CreateInput{
		CandidateID: candidateID, SourceCommit: strings.Repeat("a", 40),
		LockfileSHA256: "sha256:" + strings.Repeat("b", 64), EvidenceBundleSHA256: "sha256:" + hex.EncodeToString(digest[:]),
		EvidenceBundleReceiptBase64: base64.StdEncoding.EncodeToString(candidateReceipt),
		FinalAssetSetSHA256:         "sha256:" + strings.Repeat("d", 64), EnvironmentID: environmentID,
		ImpactDomains: []string{"code_change", "internal_cost"},
	}, "candidate", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	var candidate persistence.Stage6ReleaseCandidate
	if err := db.Where("id = ?", view.ID).Take(&candidate).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	approvers := make([]uuid.UUID, 0, len(approvalRoles))
	for _, role := range approvalRoles {
		user := persistence.User{ID: uuid.New(), Email: "internal-cost-" + role + "@example.test", DisplayName: "Internal cost " + role, Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now}
		if err := db.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
		membership := persistence.TenantMembership{TenantID: domain.TenantID, UserID: user.ID, Role: "admin", Status: "active", JoinedAt: &now, CreatedAt: now, UpdatedAt: now}
		if err := db.Create(&membership).Error; err != nil {
			t.Fatal(err)
		}
		grant := persistence.Stage6GovernanceAuthorityGrant{
			ID: uuid.New(), OperatorTenantID: domain.TenantID, UserID: user.ID, AuthorityKey: "internal_cost." + role,
			Status: "active", Version: 1, ExpiresAt: now.Add(30 * 24 * time.Hour), GrantedBy: domain.UserID,
			Reason: "Owner assigned internal usage and cost evidence review authority.", EvidenceReference: "https://evidence.example.test/internal-cost/authority/" + role,
			EvidenceSHA256: stage6authority.DigestPointer("internal-cost-authority-" + role), CreatedAt: now, UpdatedAt: now,
		}
		if err := db.Create(&grant).Error; err != nil {
			t.Fatal(err)
		}
		approvers = append(approvers, user.ID)
	}
	return internalCostFixture{ctx: ctx, db: db, service: NewService(db, domain.TenantID), creatorID: domain.UserID, candidate: candidate, approverIDs: approvers, now: now}
}

func (f internalCostFixture) importInput(t *testing.T) ImportInput {
	t.Helper()
	var envelope struct {
		Candidate struct {
			MigrationTail struct{ Name, SHA256 string } `json:"migrationTail"`
		} `json:"candidate"`
	}
	if err := json.Unmarshal(f.candidate.EvidenceBundleReceipt, &envelope); err != nil {
		t.Fatal(err)
	}
	receipt, digest, err := stage6internalcost.Receipt(f.candidate.CandidateID, f.candidate.EnvironmentID, envelope.Candidate.MigrationTail.Name, envelope.Candidate.MigrationTail.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	return ImportInput{CandidateRecordID: f.candidate.ID, ReceiptBase64: base64.StdEncoding.EncodeToString(receipt), ReceiptSHA256: digest}
}

func (f internalCostFixture) mutatedInput(t *testing.T, mutate func(map[string]any)) ImportInput {
	t.Helper()
	input := f.importInput(t)
	data, err := base64.StdEncoding.DecodeString(input.ReceiptBase64)
	if err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	mutate(receipt)
	data, err = json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	input.ReceiptBase64 = base64.StdEncoding.EncodeToString(data)
	input.ReceiptSHA256 = "sha256:" + hex.EncodeToString(digest[:])
	return input
}

func assertInternalCostProblem(t *testing.T, err error, code string) {
	t.Helper()
	var item *problem.Error
	if !errors.As(err, &item) || item.Code != code {
		t.Fatalf("error = %v, want %s", err, code)
	}
}
