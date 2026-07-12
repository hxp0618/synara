package enterpriseidentity

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	credentialkms "github.com/synara-ai/synara/services/control-plane/internal/kms"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
)

func TestOIDCConnectionEncryptsSecretAndCreatesPKCEAttempt(t *testing.T) {
	var issuer string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer": issuer, "authorization_endpoint": issuer + "/authorize",
				"token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys",
				"response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"},
				"id_token_signing_alg_values_supported": []string{"RS256"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	issuer = server.URL

	db, principal, tenantID := setupIdentityTest(t)
	wrapper, err := credentialkms.NewLocalKeyWrapper("identity-test-v1", bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	cipher := credentialkms.NewEnvelopeCipher(wrapper)
	service := NewService(db, identity.NewService(db, time.Hour, 30*time.Minute), cipher)
	connection, err := service.Create(context.Background(), principal, tenantID, CreateConnectionInput{
		Kind: "oidc", Name: "Company SSO", Issuer: issuer, ClientID: "synara-client",
		ClientSecret: "oidc-client-secret", OIDC: OIDCConfiguration{AllowedDomains: []string{"example.com"}},
	}, "identity-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	var stored persistence.IdentityConnection
	if err := db.Where("id = ?", connection.ID).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored.EncryptedSecret, []byte("oidc-client-secret")) || len(stored.EncryptedDataKey) == 0 {
		t.Fatal("OIDC client secret was not envelope encrypted")
	}
	ctx := oidc.ClientContext(context.Background(), server.Client())
	started, err := service.Start(ctx, connection.ID, "https://synara.example.com/v1/auth/sso/callback", "/settings")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(started.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Query().Get("code_challenge") == "" || parsed.Query().Get("nonce") == "" || parsed.Query().Get("state") == "" {
		t.Fatalf("OIDC authorization URL omitted state, nonce, or PKCE: %s", started.AuthorizationURL)
	}
	var attempt persistence.IdentityLoginAttempt
	if err := db.Where("connection_id = ?", connection.ID).Take(&attempt).Error; err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(attempt.EncryptedPayload), parsed.Query().Get("nonce")) || attempt.ReturnTo != "/settings" {
		t.Fatalf("OIDC attempt secret leaked or returnTo changed: %#v", attempt)
	}
}

func setupIdentityTest(t *testing.T) (*gorm.DB, identity.Principal, uuid.UUID) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(persistence.AllModels()...); err != nil {
		t.Fatal(err)
	}
	userID, tenantID := uuid.New(), uuid.New()
	now := time.Now().UTC()
	for _, model := range []any{
		&persistence.User{ID: userID, Email: "owner@example.com", DisplayName: "Owner", Status: "active", EmailVerifiedAt: &now},
		&persistence.Tenant{ID: tenantID, Slug: "identity-test", Name: "Identity Test", Status: "active", PlanCode: "enterprise", Region: "default", Settings: map[string]any{}, CreatedBy: userID},
		&persistence.TenantMembership{TenantID: tenantID, UserID: userID, Role: "owner", Status: "active", JoinedAt: &now},
	} {
		if err := db.Create(model).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db, identity.Principal{UserID: userID, SessionID: uuid.New(), ActiveTenantID: &tenantID, Email: "owner@example.com", DisplayName: "Owner"}, tenantID
}
