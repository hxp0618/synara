package providercommercial

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/governanceauthority"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6authority"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestProviderCommercialAuthorizationRequiresSeparatedApprovalAndFencesHostedExecution(t *testing.T) {
	fixture := newProviderCommercialFixture(t)
	authorization := fixture.createAuthorization(t)
	authorization = fixture.transition(t, authorization, "ready_for_review")

	_, err := fixture.service.RecordApproval(fixture.ctx, fixture.creatorID, authorization.ID, ApprovalInput{
		Role: "legal", Decision: "approved", Reason: "Creator must not approve their own authorization.",
		EvidenceReference: "https://evidence.example.test/provider/self-review",
		EvidenceSHA256:    "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
	}, "self-review", "127.0.0.1")
	assertProviderCommercialProblem(t, err, "provider_commercial_authorization_not_reviewable")
	_, err = fixture.service.RecordApproval(fixture.ctx, fixture.approverIDs[0], authorization.ID, ApprovalInput{
		Role: "legal", Decision: "approved", Reason: "A zero digest must not bind approval evidence.",
		EvidenceReference: "https://evidence.example.test/provider/zero-approval",
		EvidenceSHA256:    "sha256:" + strings.Repeat("0", 64),
	}, "zero-approval-digest", "127.0.0.1")
	assertProviderCommercialProblem(t, err, "provider_commercial_approval_invalid")

	for index, role := range requiredApprovalRoles {
		authorization, err = fixture.service.RecordApproval(fixture.ctx, fixture.approverIDs[index], authorization.ID, ApprovalInput{
			Role: role, Decision: "approved", Reason: "Reviewed exact commercial terms, data boundary and approved use case.",
			EvidenceReference: "https://evidence.example.test/provider/approval/" + role,
			EvidenceSHA256:    "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		}, "approval-"+role, "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
	}
	authorization = fixture.transition(t, authorization, "active")
	if authorization.State != "active" || authorization.ActivatedAt == nil || len(authorization.Approvals) != 4 {
		t.Fatalf("active authorization = %#v", authorization)
	}
	if authorization.TermsSHA256 == nil || *authorization.TermsSHA256 != fixture.createInput().TermsSHA256 ||
		authorization.AgreementSHA256 == nil || authorization.DPASHA256 == nil || authorization.TerminationRunbookSHA256 == nil {
		t.Fatalf("authorization document digests = %#v", authorization)
	}
	for _, approval := range authorization.Approvals {
		if approval.EvidenceSHA256 == nil || *approval.EvidenceSHA256 != "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee" {
			t.Fatalf("approval evidence digest = %#v", approval)
		}
	}

	credential := fixture.createCredential(t, "tenant")
	version := credential.Version
	input := HostedExecutionInput{
		TenantID: fixture.operatorTenantID, Provider: "codex", TargetKind: "kubernetes",
		PlacementRegion: "cn-north-1", CredentialID: &credential.ID, CredentialVersion: &version,
	}
	if err := RequireHostedExecution(fixture.ctx, fixture.db, fixture.operatorTenantID, input, fixture.now); err != nil {
		t.Fatal(err)
	}
	input.PlacementRegion = "eu-west-1"
	err = RequireHostedExecution(fixture.ctx, fixture.db, fixture.operatorTenantID, input, fixture.now)
	assertProviderCommercialProblem(t, err, "provider_commercial_authorization_required")
	input.PlacementRegion = "cn-north-1"
	authorization = fixture.transition(t, authorization, "revoked")
	err = RequireHostedExecution(fixture.ctx, fixture.db, fixture.operatorTenantID, input, fixture.now)
	assertProviderCommercialProblem(t, err, "provider_commercial_authorization_required")

	if err := fixture.db.Model(&persistence.ProviderCommercialAuthorization{}).Where("id = ?", authorization.ID).
		Update("agreement_reference", "https://evidence.example.test/provider/mutated").Error; err == nil {
		t.Fatal("Provider commercial authorization was mutable")
	}
	if err := fixture.db.Model(&persistence.ProviderCommercialAuthorization{}).Where("id = ?", authorization.ID).
		Update("agreement_sha256", "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee").Error; err == nil {
		t.Fatal("Provider commercial document digest was mutable")
	}
	if err := fixture.db.Where("authorization_id = ?", authorization.ID).
		Delete(&persistence.ProviderCommercialAuthorizationApproval{}).Error; err == nil {
		t.Fatal("Provider commercial approvals were deletable")
	}
	if err := fixture.db.Model(&persistence.ProviderCommercialAuthorizationApproval{}).Where("authorization_id = ?", authorization.ID).
		Update("evidence_sha256", "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff").Error; err == nil {
		t.Fatal("Provider commercial approval evidence digest was mutable")
	}
	var auditCount int64
	if err := fixture.db.Model(&persistence.AuditLog{}).Where("tenant_id = ? AND action LIKE ?", fixture.operatorTenantID, "provider.commercial_%").Count(&auditCount).Error; err != nil {
		t.Fatal(err)
	}
	if auditCount != 8 {
		t.Fatalf("Provider commercial Audit count = %d, want 8", auditCount)
	}
}

func TestProviderCommercialAuthorizationRejectsLocalOnlyIncompleteExpiredAndScopeMismatch(t *testing.T) {
	fixture := newProviderCommercialFixture(t)
	invalid := fixture.createInput()
	invalid.AuthorizationKey = "cursor-hosted"
	invalid.Provider = "cursor"
	_, err := fixture.service.Create(fixture.ctx, fixture.creatorID, invalid, "local-only", "127.0.0.1")
	assertProviderCommercialProblem(t, err, "provider_commercial_authorization_invalid")

	invalid = fixture.createInput()
	invalid.AuthorizationKey = "zero-digest"
	invalid.TermsSHA256 = "sha256:" + strings.Repeat("0", 64)
	_, err = fixture.service.Create(fixture.ctx, fixture.creatorID, invalid, "zero-digest", "127.0.0.1")
	assertProviderCommercialProblem(t, err, "provider_commercial_digest_invalid")

	invalid = fixture.createInput()
	invalid.AuthorizationKey = "query-reference"
	invalid.AgreementReference = "https://evidence.example.test/provider/agreement?token=secret"
	_, err = fixture.service.Create(fixture.ctx, fixture.creatorID, invalid, "query-reference", "127.0.0.1")
	assertProviderCommercialProblem(t, err, "provider_commercial_reference_invalid")

	authorization := fixture.createAuthorization(t)
	authorization = fixture.transition(t, authorization, "ready_for_review")
	_, err = fixture.service.Transition(fixture.ctx, fixture.creatorID, authorization.ID, TransitionInput{
		ExpectedVersion: authorization.Version, TargetState: "active", Reason: "Attempt activation without required approvals.",
	}, "incomplete", "127.0.0.1")
	assertProviderCommercialProblem(t, err, "provider_commercial_approvals_incomplete")

	credential := fixture.createCredential(t, "organization")
	version := credential.Version
	err = RequireHostedExecution(fixture.ctx, fixture.db, fixture.operatorTenantID, HostedExecutionInput{
		TenantID: fixture.operatorTenantID, Provider: "codex", TargetKind: "docker", PlacementRegion: "cn-north-1",
		CredentialID: &credential.ID, CredentialVersion: &version,
	}, fixture.now)
	assertProviderCommercialProblem(t, err, "provider_commercial_authorization_required")

	if err := RequireHostedExecution(fixture.ctx, fixture.db, fixture.operatorTenantID, HostedExecutionInput{
		TenantID: fixture.operatorTenantID, Provider: "cursor", TargetKind: "local", PlacementRegion: "",
	}, fixture.now); err != nil {
		t.Fatalf("local-only local execution was incorrectly gated: %v", err)
	}
}

type providerCommercialFixture struct {
	ctx              context.Context
	db               *gorm.DB
	service          *Service
	operatorTenantID uuid.UUID
	organizationID   uuid.UUID
	creatorID        uuid.UUID
	approverIDs      []uuid.UUID
	now              time.Time
}

func newProviderCommercialFixture(t *testing.T) providerCommercialFixture {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "provider-commercial-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	approvers := make([]uuid.UUID, 0, 4)
	for index := range 4 {
		user := persistence.User{ID: uuid.New(), Email: "provider-commercial-" + string(rune('a'+index)) + "@example.test", DisplayName: "Provider Commercial " + string(rune('A'+index)), Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now}
		if err := store.DB().Create(&user).Error; err != nil {
			t.Fatal(err)
		}
		if err := store.DB().Create(&persistence.TenantMembership{TenantID: domain.TenantID, UserID: user.ID, Role: "admin", Status: "active", JoinedAt: &now, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
			t.Fatal(err)
		}
		approvers = append(approvers, user.ID)
	}
	for index, role := range requiredApprovalRoles {
		seedProviderCommercialAuthority(t, store.DB(), domain.TenantID, domain.UserID, approvers[index], governanceauthority.ProviderCommercialPrefix+role, now)
	}
	return providerCommercialFixture{ctx: ctx, db: store.DB(), service: NewService(store.DB(), domain.TenantID), operatorTenantID: domain.TenantID, organizationID: domain.OrganizationID, creatorID: domain.UserID, approverIDs: approvers, now: now}
}

func seedProviderCommercialAuthority(t *testing.T, db *gorm.DB, tenantID, ownerID, userID uuid.UUID, key string, now time.Time) {
	t.Helper()
	grant := persistence.Stage6GovernanceAuthorityGrant{
		ID: uuid.New(), OperatorTenantID: tenantID, UserID: userID, AuthorityKey: key,
		Status: "active", Version: 1, ExpiresAt: now.Add(180 * 24 * time.Hour), GrantedBy: ownerID,
		Reason:            "Owner assigned the exact Provider commercial governance function.",
		EvidenceReference: "https://evidence.example.test/governance-authority/" + strings.ReplaceAll(key, ".", "/"),
		EvidenceSHA256:    stage6authority.DigestPointer("provider-authority-" + key),
		CreatedAt:         now, UpdatedAt: now,
	}
	if err := db.Create(&grant).Error; err != nil {
		t.Fatal(err)
	}
}

func (f providerCommercialFixture) createInput() CreateInput {
	return CreateInput{
		AuthorizationKey: "openai-api-2026", Provider: "codex", ProviderProduct: "OpenAI API",
		AccountType: "Enterprise API organization", ContractingEntity: "OpenAI contracting entity",
		CredentialMode: "customer_byok", AllowedCredentialScopes: []string{"tenant", "organization"},
		AllowedRegions: []string{"cn-north-1", "cn-east-1"}, DataUsePolicy: "no_training",
		RetentionPolicy:  "API data controls and configured zero-data-retention eligibility apply.",
		TermsEffectiveAt: f.now.Add(-24 * time.Hour), TermsReference: "https://evidence.example.test/provider/terms",
		TermsSHA256:        "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		AgreementReference: "https://evidence.example.test/provider/agreement", DPAReference: "https://evidence.example.test/provider/dpa",
		AgreementSHA256: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", DPASHA256: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		ProhibitedUseSummary:        "Consumer login sharing, safety-control bypass and unapproved training are prohibited.",
		TerminationRunbookReference: "https://evidence.example.test/provider/termination",
		TerminationRunbookSHA256:    "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		ReviewExpiresAt:             f.now.Add(90 * 24 * time.Hour),
	}
}

func (f providerCommercialFixture) createAuthorization(t *testing.T) Authorization {
	t.Helper()
	authorization, err := f.service.Create(f.ctx, f.creatorID, f.createInput(), "create-authorization", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return authorization
}

func (f providerCommercialFixture) transition(t *testing.T, authorization Authorization, target string) Authorization {
	t.Helper()
	updated, err := f.service.Transition(f.ctx, f.creatorID, authorization.ID, TransitionInput{
		ExpectedVersion: authorization.Version, TargetState: target,
		Reason: "Advance the exact commercial authorization after the bounded gate completed.",
	}, "transition-"+target, "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func (f providerCommercialFixture) createCredential(t *testing.T, scope string) persistence.ProviderCredential {
	t.Helper()
	var organizationID *uuid.UUID
	if scope == "organization" {
		organizationID = &f.organizationID
	}
	credential := persistence.ProviderCredential{
		ID: uuid.New(), TenantID: f.operatorTenantID, OrganizationID: organizationID, Scope: scope,
		Name: "Commercial API Credential", Purpose: "provider", Provider: "codex", CredentialType: "api_key",
		EncryptedPayload: []byte("encrypted-payload"), EncryptedDataKey: []byte("encrypted-data-key"),
		KMSProvider: "local", KMSKeyID: "test", AADVersion: 3, Version: 1,
		CreatedBy: f.creatorID, UpdatedBy: f.creatorID, CreatedAt: f.now, UpdatedAt: f.now,
	}
	if err := f.db.Create(&credential).Error; err != nil {
		t.Fatal(err)
	}
	return credential
}

func assertProviderCommercialProblem(t *testing.T, err error, code string) {
	t.Helper()
	var item *problem.Error
	if !errors.As(err, &item) || item.Code != code {
		t.Fatalf("error = %v, want problem code %s", err, code)
	}
}
