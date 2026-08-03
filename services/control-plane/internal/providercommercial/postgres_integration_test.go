package providercommercial

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/postgresisolation"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestProviderCommercialAuthorizationPostgresConcurrentApprovalAndHostedFence(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, db, platform.ProfilePersonal, "provider-commercial-pg-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	approvers := make([]uuid.UUID, 0, 4)
	for index := range 4 {
		user := persistence.User{ID: uuid.New(), Email: "provider-commercial-pg-" + string(rune('a'+index)) + "@example.test", DisplayName: "Provider Commercial PG " + string(rune('A'+index)), Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now}
		if err := db.Create(&user).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&persistence.TenantMembership{TenantID: domain.TenantID, UserID: user.ID, Role: "admin", Status: "active", JoinedAt: &now, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
			t.Fatal(err)
		}
		approvers = append(approvers, user.ID)
	}
	for index, role := range requiredApprovalRoles {
		seedProviderCommercialAuthority(t, db, domain.TenantID, domain.UserID, approvers[index], "provider_commercial."+role, now)
	}
	service := NewService(db, domain.TenantID)
	authorization, err := service.Create(ctx, domain.UserID, CreateInput{
		AuthorizationKey: "openai-pg-2026", Provider: "codex", ProviderProduct: "OpenAI API",
		AccountType: "Enterprise API organization", ContractingEntity: "OpenAI contracting entity",
		CredentialMode: "customer_byok", AllowedCredentialScopes: []string{"tenant"}, AllowedRegions: []string{"default"},
		DataUsePolicy: "no_training", RetentionPolicy: "Approved enterprise API retention policy applies.",
		TermsEffectiveAt: now.Add(-24 * time.Hour), TermsReference: "https://evidence.example.test/pg/terms",
		TermsSHA256:        "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		AgreementReference: "https://evidence.example.test/pg/agreement", DPAReference: "https://evidence.example.test/pg/dpa",
		AgreementSHA256: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", DPASHA256: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		ProhibitedUseSummary:        "Consumer login sharing and safety-control bypass are prohibited.",
		TerminationRunbookReference: "https://evidence.example.test/pg/termination", ReviewExpiresAt: now.Add(90 * 24 * time.Hour),
		TerminationRunbookSHA256: "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
	}, "pg-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	authorization, err = service.Transition(ctx, domain.UserID, authorization.ID, TransitionInput{ExpectedVersion: authorization.Version, TargetState: "ready_for_review", Reason: "Submit exact Provider commercial use for review."}, "pg-ready", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	errorsByRole := make(chan error, len(requiredApprovalRoles))
	for index, role := range requiredApprovalRoles {
		wait.Add(1)
		go func(index int, role string) {
			defer wait.Done()
			_, approvalErr := service.RecordApproval(ctx, approvers[index], authorization.ID, ApprovalInput{
				Role: role, Decision: "approved", Reason: "PostgreSQL role approved exact commercial and data-use evidence.",
				EvidenceReference: "https://evidence.example.test/pg/approval/" + role,
				EvidenceSHA256:    "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
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
	authorization, err = service.get(ctx, authorization.ID)
	if err != nil {
		t.Fatal(err)
	}
	authorization, err = service.Transition(ctx, domain.UserID, authorization.ID, TransitionInput{ExpectedVersion: authorization.Version, TargetState: "active", Reason: "Activate after all PostgreSQL decisions committed."}, "pg-active", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	credential := persistence.ProviderCredential{
		ID: uuid.New(), TenantID: domain.TenantID, Scope: "tenant", Name: "Commercial PG Credential", Purpose: "provider", Provider: "codex", CredentialType: "api_key",
		EncryptedPayload: []byte("encrypted-payload"), EncryptedDataKey: []byte("encrypted-data-key"), KMSProvider: "local", KMSKeyID: "test", AADVersion: 3, Version: 1,
		CreatedBy: domain.UserID, UpdatedBy: domain.UserID, CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&credential).Error; err != nil {
		t.Fatal(err)
	}
	version := credential.Version
	if err := RequireHostedExecution(ctx, db, domain.TenantID, HostedExecutionInput{TenantID: domain.TenantID, Provider: "codex", TargetKind: "kubernetes", PlacementRegion: "default", CredentialID: &credential.ID, CredentialVersion: &version}, now); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&persistence.ProviderCommercialAuthorization{}).Where("id = ?", authorization.ID).Update("agreement_reference", "https://evidence.example.test/pg/mutated").Error; err == nil {
		t.Fatal("PostgreSQL Provider commercial authorization was mutable")
	}
	if err := db.Model(&persistence.ProviderCommercialAuthorization{}).Where("id = ?", authorization.ID).Update("agreement_sha256", "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee").Error; err == nil {
		t.Fatal("PostgreSQL Provider commercial document digest was mutable")
	}
	if err := db.Model(&persistence.ProviderCommercialAuthorizationApproval{}).Where("authorization_id = ?", authorization.ID).Update("reason", "mutated").Error; err == nil {
		t.Fatal("PostgreSQL Provider commercial approval was mutable")
	}
	if err := db.Model(&persistence.ProviderCommercialAuthorizationApproval{}).Where("authorization_id = ?", authorization.ID).Update("evidence_sha256", "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff").Error; err == nil {
		t.Fatal("PostgreSQL Provider commercial approval evidence digest was mutable")
	}
}
