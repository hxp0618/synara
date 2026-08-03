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
	"github.com/synara-ai/synara/services/control-plane/internal/governanceauthority"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/platform"
	"github.com/synara-ai/synara/services/control-plane/internal/supportaccess"
	"github.com/synara-ai/synara/services/control-plane/migrations"
)

func TestPlatformGovernanceAuthorityIsOwnerManagedAndAudited(t *testing.T) {
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
	domain, err := bootstrap.Ensure(ctx, store.DB(), platform.ProfilePersonal, "governance-authority-http-"+uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	operator := persistence.User{ID: uuid.New(), Email: "governance-http@example.test", DisplayName: "Governance HTTP", Status: "active", EmailVerifiedAt: &now, CreatedAt: now, UpdatedAt: now}
	if err := store.DB().Create(&operator).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.DB().Create(&persistence.TenantMembership{TenantID: domain.TenantID, UserID: operator.ID, Role: "admin", Status: "active", JoinedAt: &now, CreatedAt: now, UpdatedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	server := &Server{
		supportAccess:       supportaccess.NewService(store.DB(), domain.TenantID),
		governanceAuthority: governanceauthority.NewService(store.DB(), domain.TenantID),
	}
	input := map[string]any{
		"userId": operator.ID, "authorityKey": governanceauthority.ReleaseEngineering,
		"expiresAt": now.Add(90 * 24 * time.Hour), "reason": "Owner verified the exact Engineering release function.",
		"evidenceReference": "https://evidence.example.test/authority/http-engineering",
		"evidenceSha256":    "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	denied := callGovernanceAuthorityHandler(t, server.createPlatformGovernanceAuthority, identity.Principal{UserID: operator.ID, ActiveTenantID: &domain.TenantID}, "", input)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("Admin grant status = %d, body = %s", denied.Code, denied.Body.String())
	}
	assertHTTPProblemCode(t, denied, "governance_authority_manage_forbidden")

	created := callGovernanceAuthorityHandler(t, server.createPlatformGovernanceAuthority, identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}, "", input)
	if created.Code != http.StatusCreated {
		t.Fatalf("Owner grant status = %d, body = %s", created.Code, created.Body.String())
	}
	var grant governanceauthority.Grant
	if err := json.Unmarshal(created.Body.Bytes(), &grant); err != nil {
		t.Fatal(err)
	}
	if grant.AuthorityKey != governanceauthority.ReleaseEngineering || grant.UserID != operator.ID ||
		grant.EvidenceSHA256 == nil || *grant.EvidenceSHA256 != "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("grant = %#v", grant)
	}

	revoked := callGovernanceAuthorityHandler(t, server.revokePlatformGovernanceAuthority, identity.Principal{UserID: domain.UserID, ActiveTenantID: &domain.TenantID}, grant.ID.String(), map[string]any{
		"expectedVersion": grant.Version, "reason": "Owner removed the Engineering release responsibility.",
	})
	if revoked.Code != http.StatusOK {
		t.Fatalf("Owner revoke status = %d, body = %s", revoked.Code, revoked.Body.String())
	}
	if err := json.Unmarshal(revoked.Body.Bytes(), &grant); err != nil {
		t.Fatal(err)
	}
	if grant.Status != "revoked" {
		t.Fatalf("revoked grant = %#v", grant)
	}
}

func callGovernanceAuthorityHandler(t *testing.T, handler http.HandlerFunc, principal identity.Principal, grantID string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "http://control-plane.test/v1/platform/governance-authorities", bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Request-ID", uuid.NewString())
	if grantID != "" {
		request.SetPathValue("grantID", grantID)
	}
	request = request.WithContext(context.WithValue(request.Context(), principalContextKey{}, principal))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
