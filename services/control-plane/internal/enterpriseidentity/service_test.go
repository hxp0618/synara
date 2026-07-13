package enterpriseidentity

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
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

func TestOIDCCompleteValidatesProviderAndCreatesExternalSession(t *testing.T) {
	provider := newFakeOIDCProvider(t)
	db, principal, tenantID := setupIdentityTest(t)
	wrapper, err := credentialkms.NewLocalKeyWrapper("identity-test-v1", bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	identityService := identity.NewService(db, time.Hour, 30*time.Minute)
	service := NewService(db, identityService, credentialkms.NewEnvelopeCipher(wrapper))
	connection, err := service.Create(context.Background(), principal, tenantID, CreateConnectionInput{
		Kind: "oidc", Name: "Company SSO", Issuer: provider.issuer(), ClientID: provider.clientID,
		ClientSecret: provider.clientSecret,
		OIDC: OIDCConfiguration{
			Scopes: []string{"openid", "profile", "email"}, AllowedDomains: []string{"example.com"},
			GroupsClaim: "groups", DefaultTenantRole: "member",
		},
	}, "identity-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	mappedRole := "security_admin"
	if err := db.Create(&persistence.IdentityGroupMapping{
		ID: uuid.New(), TenantID: tenantID, ConnectionID: connection.ID, ExternalGroup: "sso-admins",
		TenantRole: &mappedRole, CreatedBy: principal.UserID,
	}).Error; err != nil {
		t.Fatal(err)
	}

	callbackURL := "https://synara.example.com/v1/auth/sso/callback"
	ctx := oidc.ClientContext(context.Background(), provider.client())
	started, err := service.Start(ctx, connection.ID, callbackURL, "/settings/identity")
	if err != nil {
		t.Fatal(err)
	}
	authorizationURL, err := url.Parse(started.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	query := authorizationURL.Query()
	state := query.Get("state")
	nonce := query.Get("nonce")
	codeChallenge := query.Get("code_challenge")
	if authorizationURL.String() == "" || authorizationURL.Path != "/authorize" ||
		query.Get("response_type") != "code" || query.Get("client_id") != provider.clientID ||
		query.Get("redirect_uri") != callbackURL || state == "" || nonce == "" || codeChallenge == "" ||
		query.Get("code_challenge_method") != "S256" {
		t.Fatalf("unexpected OIDC authorization request: %s", started.AuthorizationURL)
	}
	provider.expectAuthorization(nonce, codeChallenge, callbackURL)

	completedAt := time.Now().UTC()
	result, err := service.CompleteOIDC(
		ctx, connection.ID, state, provider.authorizationCode, callbackURL,
		"203.0.113.7", "Synara OIDC test", "oidc-complete-request",
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.ReturnTo != "/settings/identity" || result.Session.Token == "" ||
		!result.Session.State.Authenticated {
		t.Fatalf("unexpected OIDC callback result: %#v", result)
	}
	issuedPrincipal := result.Session.State.User
	if issuedPrincipal.SessionID == uuid.Nil || issuedPrincipal.UserID == uuid.Nil ||
		issuedPrincipal.ActiveTenantID == nil || *issuedPrincipal.ActiveTenantID != tenantID ||
		issuedPrincipal.Email != provider.email || issuedPrincipal.DisplayName != provider.displayName {
		t.Fatalf("unexpected issued OIDC principal: %#v", issuedPrincipal)
	}
	if len(result.Session.State.Tenants) != 1 || result.Session.State.Tenants[0].ID != tenantID ||
		result.Session.State.Tenants[0].Role != mappedRole {
		t.Fatalf("unexpected OIDC Tenant access: %#v", result.Session.State.Tenants)
	}

	var external persistence.UserIdentity
	if err := db.Where("issuer = ? AND subject = ?", provider.issuer(), provider.subject).Take(&external).Error; err != nil {
		t.Fatal(err)
	}
	if external.ConnectionID == nil || *external.ConnectionID != connection.ID || external.Provider != "oidc" ||
		external.UserID != issuedPrincipal.UserID || external.LastLoginAt == nil || external.LastLoginAt.Before(completedAt.Add(-time.Second)) {
		t.Fatalf("unexpected external identity: %#v", external)
	}
	if external.Profile["department"] != "platform" || strings.Join(claimStrings(external.Profile["groups"]), ",") != "engineering,sso-admins" {
		t.Fatalf("OIDC user claims were not persisted safely: %#v", external.Profile)
	}
	if _, found := external.Profile["api_token"]; found {
		t.Fatalf("token-like OIDC claim was persisted: %#v", external.Profile)
	}

	var user persistence.User
	if err := db.Where("id = ?", issuedPrincipal.UserID).Take(&user).Error; err != nil {
		t.Fatal(err)
	}
	if user.Email != provider.email || user.DisplayName != provider.displayName || user.Status != "active" || user.EmailVerifiedAt == nil {
		t.Fatalf("unexpected OIDC user: %#v", user)
	}
	var membership persistence.TenantMembership
	if err := db.Where("tenant_id = ? AND user_id = ?", tenantID, user.ID).Take(&membership).Error; err != nil {
		t.Fatal(err)
	}
	if membership.Role != mappedRole || membership.Status != "active" {
		t.Fatalf("unexpected OIDC Tenant membership: %#v", membership)
	}

	var loginSession persistence.LoginSession
	if err := db.Where("id = ?", issuedPrincipal.SessionID).Take(&loginSession).Error; err != nil {
		t.Fatal(err)
	}
	tokenHash := sha256.Sum256([]byte(result.Session.Token))
	if loginSession.UserID != user.ID || loginSession.ActiveTenantID == nil || *loginSession.ActiveTenantID != tenantID ||
		!bytes.Equal(loginSession.RefreshTokenHash, tokenHash[:]) || loginSession.IPAddress == nil ||
		*loginSession.IPAddress != "203.0.113.7" || loginSession.UserAgent == nil ||
		*loginSession.UserAgent != "Synara OIDC test" || loginSession.RevokedAt != nil ||
		!loginSession.ExpiresAt.After(loginSession.LastSeenAt) {
		t.Fatalf("unexpected persisted OIDC LoginSession: %#v", loginSession)
	}
	authenticated, err := identityService.Authenticate(context.Background(), result.Session.Token)
	if err != nil {
		t.Fatal(err)
	}
	if authenticated.SessionID != loginSession.ID || authenticated.UserID != user.ID {
		t.Fatalf("issued OIDC token did not authenticate the persisted session: %#v", authenticated)
	}

	var attempt persistence.IdentityLoginAttempt
	if err := db.Where("connection_id = ?", connection.ID).Take(&attempt).Error; err != nil {
		t.Fatal(err)
	}
	if attempt.ConsumedAt == nil {
		t.Fatal("OIDC state was not consumed")
	}
	var auditEntry persistence.AuditLog
	if err := db.Where("tenant_id = ? AND action = ? AND request_id = ?", tenantID, "identity.sso_login", "oidc-complete-request").Take(&auditEntry).Error; err != nil {
		t.Fatal(err)
	}
	if auditEntry.ActorID == nil || *auditEntry.ActorID != user.ID || auditEntry.ResourceID == nil ||
		*auditEntry.ResourceID != connection.ID || auditEntry.IPAddress == nil || *auditEntry.IPAddress != "203.0.113.7" ||
		auditEntry.Metadata["provider"] != "oidc" {
		t.Fatalf("unexpected OIDC audit entry: %#v", auditEntry)
	}
	provider.assertCompleted(t)

	_, err = service.CompleteOIDC(
		ctx, connection.ID, state, provider.authorizationCode, callbackURL,
		"203.0.113.7", "Synara OIDC test", "oidc-replay-request",
	)
	assertProblemCode(t, err, "oidc_state_invalid")
}

type fakeOIDCProvider struct {
	server            *httptest.Server
	privateKey        *rsa.PrivateKey
	clientID          string
	clientSecret      string
	authorizationCode string
	subject           string
	email             string
	displayName       string

	mu                    sync.Mutex
	expectedNonce         string
	expectedCodeChallenge string
	expectedRedirectURI   string
	discoveryRequests     int
	jwksRequests          int
	tokenRequests         int
	tokenRequestError     string
}

func newFakeOIDCProvider(t *testing.T) *fakeOIDCProvider {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	provider := &fakeOIDCProvider{
		privateKey: privateKey, clientID: "synara-client", clientSecret: "oidc-client-secret",
		authorizationCode: "oidc-authorization-code", subject: "oidc-user-123",
		email: "alice@example.com", displayName: "Alice OIDC",
	}
	provider.server = httptest.NewTLSServer(http.HandlerFunc(provider.serveHTTP))
	t.Cleanup(provider.server.Close)
	return provider
}

func (p *fakeOIDCProvider) issuer() string { return p.server.URL }

func (p *fakeOIDCProvider) client() *http.Client { return p.server.Client() }

func (p *fakeOIDCProvider) expectAuthorization(nonce, codeChallenge, redirectURI string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expectedNonce = nonce
	p.expectedCodeChallenge = codeChallenge
	p.expectedRedirectURI = redirectURI
}

func (p *fakeOIDCProvider) serveHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		p.mu.Lock()
		p.discoveryRequests++
		p.mu.Unlock()
		writeOIDCTestJSON(w, map[string]any{
			"issuer": p.issuer(), "authorization_endpoint": p.issuer() + "/authorize",
			"token_endpoint": p.issuer() + "/token", "jwks_uri": p.issuer() + "/keys",
			"response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post"},
		})
	case "/keys":
		p.mu.Lock()
		p.jwksRequests++
		p.mu.Unlock()
		publicKey := p.privateKey.PublicKey
		writeOIDCTestJSON(w, map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "kid": "oidc-test-key", "use": "sig", "alg": "RS256",
			"n": base64.RawURLEncoding.EncodeToString(publicKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(publicKey.E)).Bytes()),
		}}})
	case "/token":
		p.handleToken(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (p *fakeOIDCProvider) handleToken(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	p.tokenRequests++
	expectedNonce := p.expectedNonce
	expectedChallenge := p.expectedCodeChallenge
	expectedRedirectURI := p.expectedRedirectURI
	p.mu.Unlock()

	if r.Method != http.MethodPost {
		p.rejectToken(w, "token request method is not POST")
		return
	}
	if err := r.ParseForm(); err != nil {
		p.rejectToken(w, fmt.Sprintf("parse token form: %v", err))
		return
	}
	clientID, clientSecret, basic := r.BasicAuth()
	if !basic {
		clientID, clientSecret = r.Form.Get("client_id"), r.Form.Get("client_secret")
	}
	verifier := r.Form.Get("code_verifier")
	challenge := sha256.Sum256([]byte(verifier))
	checks := []struct {
		valid   bool
		message string
	}{
		{clientID == p.clientID && clientSecret == p.clientSecret, "client credentials changed"},
		{r.Form.Get("grant_type") == "authorization_code", "grant_type changed"},
		{r.Form.Get("code") == p.authorizationCode, "authorization code changed"},
		{r.Form.Get("redirect_uri") == expectedRedirectURI, "redirect URI changed"},
		{verifier != "", "PKCE code_verifier is empty"},
		{base64.RawURLEncoding.EncodeToString(challenge[:]) == expectedChallenge, "PKCE verification failed"},
		{expectedNonce != "", "authorization nonce was not recorded"},
	}
	for _, check := range checks {
		if !check.valid {
			p.rejectToken(w, check.message)
			return
		}
	}
	idToken, err := p.signIDToken(expectedNonce)
	if err != nil {
		p.rejectToken(w, err.Error())
		return
	}
	writeOIDCTestJSON(w, map[string]any{
		"access_token": "oidc-access-token", "token_type": "Bearer", "expires_in": 3600,
		"id_token": idToken,
	})
}

func (p *fakeOIDCProvider) rejectToken(w http.ResponseWriter, message string) {
	p.mu.Lock()
	p.tokenRequestError = message
	p.mu.Unlock()
	http.Error(w, message, http.StatusBadRequest)
}

func (p *fakeOIDCProvider) signIDToken(nonce string) (string, error) {
	now := time.Now().UTC()
	header, err := json.Marshal(map[string]any{"alg": "RS256", "kid": "oidc-test-key", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]any{
		"iss": p.issuer(), "sub": p.subject, "aud": p.clientID,
		"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(5 * time.Minute).Unix(), "nonce": nonce,
		"email": p.email, "email_verified": true, "name": p.displayName,
		"groups": []string{"sso-admins", "engineering"}, "department": "platform",
		"api_token": "must-not-be-persisted",
	})
	if err != nil {
		return "", err
	}
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, p.privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (p *fakeOIDCProvider) assertCompleted(t *testing.T) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.discoveryRequests < 2 || p.jwksRequests != 1 || p.tokenRequests != 1 || p.tokenRequestError != "" {
		t.Fatalf(
			"incomplete fake OIDC flow: discovery=%d jwks=%d token=%d tokenError=%q",
			p.discoveryRequests, p.jwksRequests, p.tokenRequests, p.tokenRequestError,
		)
	}
}

func writeOIDCTestJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
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
