package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/synara-ai/synara/services/control-plane/internal/bootstrap"
	"github.com/synara-ai/synara/services/control-plane/internal/database"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/providercommercial"
	"github.com/synara-ai/synara/services/control-plane/internal/supportaccess"
	"github.com/synara-ai/synara/services/control-plane/internal/testsupport/stage6authority"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPlatformProviderCommercialAuthorizationSeparatesManagementAndReview(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "provider-commercial-http-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	security := persistence.User{ID: uuid.New(), Email: "provider-commercial-security@example.test", DisplayName: "Provider Commercial Security", Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now}
	if err := store.DB().Create(&security).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.TenantMembership{TenantID: domain.TenantID, UserID: security.ID, Role: "security_admin", Status: "active", JoinedAt: &now, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.Stage6GovernanceAuthorityGrant{
		ID: uuid.New(), OperatorTenantID: domain.TenantID, UserID: security.ID, AuthorityKey: "provider_commercial.security",
		Status: "active", Version: 1, ExpiresAt: now.Add(90 * 24 * time.Hour), GrantedBy: domain.UserID,
		Reason: "Owner assigned the exact Provider commercial Security approval function.", EvidenceReference: "https://evidence.example.test/authority/provider-security",
		EvidenceSHA256: stage6authority.DigestPointer("http-provider-security"),
		CreatedAt:      now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	server := &Server{supportAccess: supportaccess.NewService(store.DB(), domain.TenantID), providerCommercial: providercommercial.NewService(store.DB(), domain.TenantID)}
	createBody := map[string]any{
		"authorizationKey": "openai-http-2026", "provider": "codex", "providerProduct": "OpenAI API",
		"accountType": "Enterprise API organization", "contractingEntity": "OpenAI contracting entity",
		"credentialMode": "customer_byok", "allowedCredentialScopes": []string{"organization", "tenant"},
		"allowedRegions": []string{"default"}, "dataUsePolicy": "no_training",
		"retentionPolicy": "Approved enterprise API retention policy applies.", "termsEffectiveAt": now.Add(-24 * time.Hour),
		"termsReference": "https://evidence.example.test/http/terms", "agreementReference": "https://evidence.example.test/http/agreement",
		"termsSha256": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "agreementSha256": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"dpaReference":                "https://evidence.example.test/http/dpa",
		"dpaSha256":                   "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		"prohibitedUseSummary":        "Consumer login sharing and safety-control bypass are prohibited.",
		"terminationRunbookReference": "https://evidence.example.test/http/termination", "reviewExpiresAt": now.Add(90 * 24 * time.Hour),
		"terminationRunbookSha256": "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
	}
	denied := callProviderCommercialHandler(t, server.createPlatformProviderCommercialAuthorization, identity.Principal{UserID: security.ID, ActiveTenantID: &domain.TenantID}, "", createBody)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("security create status = %d, body = %s", denied.Code, denied.Body.String())
	}
	assertHTTPProblemCode(t, denied, "provider_commercial_manage_forbidden")

	created := callProviderCommercialHandler(t, server.createPlatformProviderCommercialAuthorization, identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}, "", createBody)
	if created.Code != http.StatusCreated {
		t.Fatalf("authorization create status = %d, body = %s", created.Code, created.Body.String())
	}
	var authorization providercommercial.Authorization
	if err := json.Unmarshal(created.Body.Bytes(), &authorization); err != nil {
		t.Fatal(err)
	}
	if authorization.TermsSHA256 == nil || *authorization.TermsSHA256 != "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" ||
		authorization.AgreementSHA256 == nil || authorization.DPASHA256 == nil || authorization.TerminationRunbookSHA256 == nil {
		t.Fatalf("created authorization document digests = %#v", authorization)
	}
	ready := callProviderCommercialHandler(t, server.transitionPlatformProviderCommercialAuthorization, identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}, authorization.ID.String(), map[string]any{
		"expectedVersion": authorization.Version, "targetState": "ready_for_review", "reason": "Submit exact hosted use for separated review.",
	})
	if ready.Code != http.StatusOK {
		t.Fatalf("authorization ready status = %d, body = %s", ready.Code, ready.Body.String())
	}
	approved := callProviderCommercialHandler(t, server.recordPlatformProviderCommercialApproval, identity.Principal{UserID: security.ID, ActiveTenantID: &domain.TenantID}, authorization.ID.String(), map[string]any{
		"role": "security", "decision": "approved", "reason": "Security approved the exact hosted credential and Region boundary.",
		"evidenceReference": "https://evidence.example.test/http/security",
		"evidenceSha256":    "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
	})
	if approved.Code != http.StatusOK {
		t.Fatalf("security approval status = %d, body = %s", approved.Code, approved.Body.String())
	}
	if err := json.Unmarshal(approved.Body.Bytes(), &authorization); err != nil {
		t.Fatal(err)
	}
	if len(authorization.Approvals) != 1 || authorization.Approvals[0].ApproverUserID != security.ID {
		t.Fatalf("authorization approvals = %#v", authorization.Approvals)
	}
	if authorization.Approvals[0].EvidenceSHA256 == nil || *authorization.Approvals[0].EvidenceSHA256 != "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee" {
		t.Fatalf("authorization approval evidence digest = %#v", authorization.Approvals[0])
	}
}

func callProviderCommercialHandler(t *testing.T, handler http.HandlerFunc, principal identity.Principal, authorizationID string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://control-plane.test/v1/platform/provider-commercial-authorizations", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Request-ID", uuid.NewString())
	if authorizationID != "" {
		request.SetPathValue("authorizationID", authorizationID)
	}
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, principal))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
