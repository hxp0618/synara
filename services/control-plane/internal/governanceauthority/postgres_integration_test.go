package governanceauthority

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/postgresisolation"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestGovernanceAuthorityPostgresConcurrentGrantAndImmediateRevocation(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "governance-authority-pg-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	operator := persistence.User{ID: uuid.New(), Email: "authority-pg@example.test", DisplayName: "Authority PG", Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(&operator).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&persistence.TenantMembership{TenantID: domain.TenantID, UserID: operator.ID, Role: "admin", Status: "active", JoinedAt: &now, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	service := NewService(db, domain.TenantID)
	input := CreateInput{
		UserID: operator.ID, AuthorityKey: ReleaseEngineering, ExpiresAt: now.Add(90 * 24 * time.Hour),
		Reason:            "Owner verified the PostgreSQL Engineering release function.",
		EvidenceReference: "https://evidence.example.test/pg/authority/engineering",
		EvidenceSHA256:    testGovernanceEvidenceDigest("postgres-engineering"),
	}
	var wait sync.WaitGroup
	errorsByAttempt := make(chan error, 2)
	for index := range 2 {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, createErr := service.Create(ctx, domain.UserID, input, "pg-authority-"+string(rune('a'+index)), "127.0.0.1")
			errorsByAttempt <- createErr
		}(index)
	}
	wait.Wait()
	close(errorsByAttempt)
	successes, conflicts := 0, 0
	for createErr := range errorsByAttempt {
		if createErr == nil {
			successes++
			continue
		}
		var item *problem.Error
		if errors.As(createErr, &item) && item.Code == "governance_authority_conflict" {
			conflicts++
			continue
		}
		t.Fatal(createErr)
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent grants successes/conflicts = %d/%d", successes, conflicts)
	}
	if err := service.Require(ctx, operator.ID, ReleaseEngineering); err != nil {
		t.Fatal(err)
	}
	authorization := persistence.ProviderCommercialAuthorization{
		ID: uuid.New(), OperatorTenantID: domain.TenantID, AuthorizationKey: "authority-pg-provider",
		Provider: "codex", ProviderProduct: "OpenAI API", AccountType: "Enterprise API organization",
		ContractingEntity: "OpenAI contracting entity", CredentialMode: "customer_byok",
		AllowedCredentialScopes: []string{"tenant"}, AllowedRegions: []string{"default"}, DataUsePolicy: "no_training",
		RetentionPolicy: "Approved enterprise API retention policy applies.", TermsEffectiveAt: now.Add(-24 * time.Hour),
		TermsReference: "https://evidence.example.test/pg/terms", AgreementReference: "https://evidence.example.test/pg/agreement",
		TermsSHA256: testProviderDocumentDigest("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), AgreementSHA256: testProviderDocumentDigest("sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
		DPAReference:                "https://evidence.example.test/pg/dpa",
		DPASHA256:                   testProviderDocumentDigest("sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"),
		ProhibitedUseSummary:        "Consumer login sharing and safety-control bypass are prohibited.",
		TerminationRunbookReference: "https://evidence.example.test/pg/termination", ReviewExpiresAt: now.Add(90 * 24 * time.Hour),
		TerminationRunbookSHA256: testProviderDocumentDigest("sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"),
		State:                    "draft", Version: 1, CreatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&authorization).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.ProviderCommercialAuthorization{}).Where("id = ?", authorization.ID).
		Updates(map[string]any{"state": "ready_for_review", "version": 2, "updated_at": now.Add(time.Second)}).Error; err != nil {
		t.Fatal(err)
	}
	unauthorizedApproval := persistence.ProviderCommercialAuthorizationApproval{
		ID: uuid.New(), AuthorizationID: authorization.ID, OperatorTenantID: domain.TenantID,
		ApprovalRole: "security", Decision: "approved", ApproverUserID: operator.ID,
		Reason:            "Release authority must not authorize a Provider commercial Security decision.",
		EvidenceReference: "https://evidence.example.test/pg/wrong-functional-role", CreatedAt: now.Add(-30 * 24 * time.Hour),
		EvidenceSHA256: testProviderDocumentDigest("sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"),
	}
	if err := db.Create(&unauthorizedApproval).Error; err == nil {
		t.Fatal("PostgreSQL accepted a self-asserted Provider commercial role without matching authority")
	}
	items, err := service.List(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("authority list = %#v, err = %v", items, err)
	}
	grant := items[0]
	if err := db.Model(&persistence.Stage6GovernanceAuthorityGrant{}).Where("id = ?", grant.ID).Update("expires_at", now.Add(180*24*time.Hour)).Error; err == nil {
		t.Fatal("PostgreSQL governance authority identity was mutable")
	}
	if err := db.Model(&persistence.Stage6GovernanceAuthorityGrant{}).Where("id = ?", grant.ID).
		Update("evidence_sha256", testGovernanceEvidenceDigest("mutated")).Error; err == nil {
		t.Fatal("PostgreSQL governance authority evidence digest was mutable")
	}
	if _, err := service.Revoke(ctx, domain.UserID, grant.ID, RevokeInput{ExpectedVersion: grant.Version, Reason: "Owner removed the PostgreSQL Engineering responsibility."}, "pg-revoke", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	assertGovernanceAuthorityProblem(t, service.Require(ctx, operator.ID, ReleaseEngineering), "governance_authority_required")
}

func testProviderDocumentDigest(value string) *string { return &value }
