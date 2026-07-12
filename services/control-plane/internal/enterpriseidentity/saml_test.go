package enterpriseidentity

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/beevik/etree"
	"github.com/crewjam/saml"
	"github.com/crewjam/saml/samlsp"
	"github.com/google/uuid"
	dsig "github.com/russellhaering/goxmldsig"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	credentialkms "github.com/synara-ai/synara/services/control-plane/internal/kms"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

const samlTestCallbackURL = "https://synara.example.com/v1/auth/sso/connection/callback"

func TestSAMLConnectionEncryptsSigningKeyAndPublishesMetadata(t *testing.T) {
	fixture := newSAMLTestFixture(t, []string{"example.com"})

	var stored persistence.IdentityConnection
	if err := fixture.db.Where("id = ?", fixture.connection.ID).Take(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored.EncryptedSecret, []byte("PRIVATE KEY")) ||
		bytes.Contains(stored.EncryptedSecret, []byte("CERTIFICATE")) ||
		len(stored.EncryptedDataKey) == 0 {
		t.Fatal("SAML service-provider key bundle was not envelope encrypted")
	}
	encodedConfiguration, err := json.Marshal(stored.Configuration)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encodedConfiguration, []byte("PRIVATE KEY")) || bytes.Contains(encodedConfiguration, []byte("CERTIFICATE")) {
		t.Fatal("SAML service-provider key material leaked into public configuration")
	}

	metadata, err := fixture.service.SAMLMetadata(context.Background(), fixture.connection.ID, samlTestCallbackURL)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := samlsp.ParseMetadata(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.EntityID != fixture.connection.Configuration["entityId"] {
		t.Fatalf("unexpected SP entity ID %q", parsed.EntityID)
	}
	if len(parsed.SPSSODescriptors) != 1 || len(parsed.SPSSODescriptors[0].AssertionConsumerServices) == 0 {
		t.Fatal("SP metadata omitted its assertion consumer service")
	}
	if parsed.SPSSODescriptors[0].AssertionConsumerServices[0].Location != samlTestCallbackURL {
		t.Fatalf("unexpected ACS URL %q", parsed.SPSSODescriptors[0].AssertionConsumerServices[0].Location)
	}
	if len(parsed.SPSSODescriptors[0].KeyDescriptors) == 0 {
		t.Fatal("SP metadata omitted its signing certificate")
	}

	started, err := fixture.service.Start(context.Background(), fixture.connection.ID, samlTestCallbackURL, "/settings/identity")
	if err != nil {
		t.Fatal(err)
	}
	redirect, err := url.Parse(started.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	if redirect.Query().Get("SigAlg") != dsig.RSASHA256SignatureMethod || redirect.Query().Get("Signature") == "" {
		t.Fatalf("SAML AuthnRequest is not signed with RSA-SHA256: %s", started.AuthorizationURL)
	}
}

func TestSAMLCompleteValidatesAssertionAndAppliesGroupMappings(t *testing.T) {
	fixture := newSAMLTestFixture(t, []string{"example.com"})
	organizationID := uuid.New()
	if err := fixture.db.Create(&persistence.Organization{
		ID: organizationID, TenantID: fixture.tenantID, Slug: "engineering", Name: "Engineering",
		Kind: "team", Status: "active", Settings: map[string]any{}, CreatedBy: fixture.principal.UserID,
	}).Error; err != nil {
		t.Fatal(err)
	}
	tenantRole, organizationRole := "security_admin", "agent_operator"
	if err := fixture.db.Create(&persistence.IdentityGroupMapping{
		ID: uuid.New(), TenantID: fixture.tenantID, ConnectionID: fixture.connection.ID,
		ExternalGroup: "synara-security", TenantRole: &tenantRole,
		OrganizationID: &organizationID, OrganizationRole: &organizationRole, CreatedBy: fixture.principal.UserID,
	}).Error; err != nil {
		t.Fatal(err)
	}

	form := fixture.signedResponse(t, nil)
	result, err := fixture.complete(t, form)
	if err != nil {
		t.Fatal(err)
	}
	if result.ReturnTo != "/settings/identity" || result.Session.Token == "" {
		t.Fatalf("unexpected callback result: %#v", result)
	}

	var user persistence.User
	if err := fixture.db.Where("LOWER(email) = ?", "alice@example.com").Take(&user).Error; err != nil {
		t.Fatal(err)
	}
	var membership persistence.TenantMembership
	if err := fixture.db.Where("tenant_id = ? AND user_id = ?", fixture.tenantID, user.ID).Take(&membership).Error; err != nil {
		t.Fatal(err)
	}
	if membership.Role != tenantRole {
		t.Fatalf("unexpected Tenant role %q", membership.Role)
	}
	var organizationMembership persistence.OrganizationMembership
	if err := fixture.db.Where("organization_id = ? AND user_id = ?", organizationID, user.ID).Take(&organizationMembership).Error; err != nil {
		t.Fatal(err)
	}
	if organizationMembership.Role != organizationRole {
		t.Fatalf("unexpected Organization role %q", organizationMembership.Role)
	}
	var auditEntry persistence.AuditLog
	if err := fixture.db.Where("tenant_id = ? AND action = ?", fixture.tenantID, "identity.sso_login").Take(&auditEntry).Error; err != nil {
		t.Fatal(err)
	}
	if auditEntry.Metadata["provider"] != "saml" {
		t.Fatalf("unexpected SAML audit metadata: %#v", auditEntry.Metadata)
	}

	_, err = fixture.complete(t, form)
	assertProblemCode(t, err, "saml_state_invalid")
}

func TestSAMLRejectsInvalidSignedAssertions(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*saml.IdpAuthnRequest)
	}{
		{
			name: "audience",
			mutate: func(request *saml.IdpAuthnRequest) {
				request.Assertion.Conditions.AudienceRestrictions[0].Audience.Value = "urn:other:service-provider"
			},
		},
		{
			name: "destination",
			mutate: func(request *saml.IdpAuthnRequest) {
				request.Assertion.Subject.SubjectConfirmations[0].SubjectConfirmationData.Recipient = "https://attacker.example.com/callback"
			},
		},
		{
			name: "issuer",
			mutate: func(request *saml.IdpAuthnRequest) {
				request.Assertion.Issuer.Value = "https://attacker.example.com/idp"
			},
		},
		{
			name: "request correlation",
			mutate: func(request *saml.IdpAuthnRequest) {
				request.Assertion.Subject.SubjectConfirmations[0].SubjectConfirmationData.InResponseTo = "unexpected-request"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newSAMLTestFixture(t, []string{"example.com"})
			_, err := fixture.complete(t, fixture.signedResponse(t, test.mutate))
			assertProblemCode(t, err, "saml_assertion_invalid")
		})
	}
}

func TestSAMLRejectsUnsignedTamperedAndForbiddenDomainResponses(t *testing.T) {
	t.Run("unsigned", func(t *testing.T) {
		fixture := newSAMLTestFixture(t, []string{"example.com"})
		form := fixture.signedResponse(t, nil)
		decoded, err := base64.StdEncoding.DecodeString(form.SAMLResponse)
		if err != nil {
			t.Fatal(err)
		}
		document := etree.NewDocument()
		if err := document.ReadFromBytes(decoded); err != nil {
			t.Fatal(err)
		}
		removed := removeSAMLSignatures(document.Root())
		if removed != 2 {
			t.Fatalf("expected response and assertion signatures, removed %d", removed)
		}
		unsigned, err := document.WriteToBytes()
		if err != nil {
			t.Fatal(err)
		}
		form.SAMLResponse = base64.StdEncoding.EncodeToString(unsigned)
		_, err = fixture.complete(t, form)
		assertProblemCode(t, err, "saml_assertion_invalid")
	})

	t.Run("tampered", func(t *testing.T) {
		fixture := newSAMLTestFixture(t, []string{"example.com"})
		form := fixture.signedResponse(t, nil)
		decoded, err := base64.StdEncoding.DecodeString(form.SAMLResponse)
		if err != nil {
			t.Fatal(err)
		}
		tampered := bytes.Replace(decoded, []byte("alice@example.com"), []byte("admin@example.com"), 1)
		if bytes.Equal(tampered, decoded) {
			t.Fatal("signed SAML response did not contain the expected email claim")
		}
		form.SAMLResponse = base64.StdEncoding.EncodeToString(tampered)
		_, err = fixture.complete(t, form)
		assertProblemCode(t, err, "saml_assertion_invalid")
	})

	t.Run("forbidden domain", func(t *testing.T) {
		fixture := newSAMLTestFixture(t, []string{"other.example"})
		_, err := fixture.complete(t, fixture.signedResponse(t, nil))
		assertProblemCode(t, err, "saml_email_domain_forbidden")
	})
}

func TestSAMLConnectionRejectsMetadataIssuerMismatch(t *testing.T) {
	fixture := newSAMLTestFixtureWithoutConnection(t)
	_, err := fixture.service.Create(context.Background(), fixture.principal, fixture.tenantID, CreateConnectionInput{
		Kind: "saml", Name: "Company SSO", Issuer: "https://unexpected.example.com/idp",
		SAML: SAMLConfiguration{MetadataURL: fixture.metadataURL(), AllowedDomains: []string{"example.com"}},
	}, "saml-create", "127.0.0.1")
	assertProblemCode(t, err, "saml_metadata_issuer_mismatch")
}

func TestSAMLMetadataRejectsUnsafeRedirect(t *testing.T) {
	redirectServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "http://example.invalid/metadata")
		w.WriteHeader(http.StatusFound)
	}))
	defer redirectServer.Close()

	db, principal, tenantID := setupIdentityTest(t)
	wrapper, err := credentialkms.NewLocalKeyWrapper("identity-test-v1", bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(db, identity.NewService(db, time.Hour), credentialkms.NewEnvelopeCipher(wrapper))
	service.httpClient = redirectServer.Client()
	_, err = service.Create(context.Background(), principal, tenantID, CreateConnectionInput{
		Kind: "saml", Name: "Unsafe redirect",
		SAML: SAMLConfiguration{MetadataURL: redirectServer.URL, AllowedDomains: []string{"example.com"}},
	}, "saml-create", "127.0.0.1")
	assertProblemCode(t, err, "saml_metadata_fetch_failed")
}

type samlTestFixture struct {
	testingT   *testing.T
	db         *gorm.DB
	principal  identity.Principal
	tenantID   uuid.UUID
	service    *Service
	connection Connection
	idp        *saml.IdentityProvider
	idpServer  *httptest.Server
	spProvider *samlTestServiceProviderProvider
	session    *saml.Session
}

func newSAMLTestFixture(t *testing.T, allowedDomains []string) *samlTestFixture {
	t.Helper()
	fixture := newSAMLTestFixtureWithoutConnection(t)
	connection, err := fixture.service.Create(context.Background(), fixture.principal, fixture.tenantID, CreateConnectionInput{
		Kind: "saml", Name: "Company SSO",
		SAML: SAMLConfiguration{
			MetadataURL: fixture.metadataURL(), EmailAttribute: "email", DisplayNameAttribute: "displayName",
			GroupsAttribute: "groups", AllowedDomains: allowedDomains, DefaultTenantRole: "member",
		},
	}, "saml-create", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	fixture.connection = connection
	metadata, err := fixture.service.SAMLMetadata(context.Background(), connection.ID, samlTestCallbackURL)
	if err != nil {
		t.Fatal(err)
	}
	fixture.spProvider.metadata, err = samlsp.ParseMetadata(metadata)
	if err != nil {
		t.Fatal(err)
	}
	// Keep assertions readable in protocol tests so signature removal and byte-level
	// tampering exercise the validator. Production metadata still advertises the
	// encryption certificate and encrypted assertions are supported by crewjam/saml.
	descriptor := &fixture.spProvider.metadata.SPSSODescriptors[0]
	keyDescriptors := descriptor.KeyDescriptors[:0]
	for _, keyDescriptor := range descriptor.KeyDescriptors {
		if keyDescriptor.Use != "encryption" {
			keyDescriptors = append(keyDescriptors, keyDescriptor)
		}
	}
	descriptor.KeyDescriptors = keyDescriptors
	return fixture
}

func newSAMLTestFixtureWithoutConnection(t *testing.T) *samlTestFixture {
	t.Helper()
	db, principal, tenantID := setupIdentityTest(t)
	wrapper, err := credentialkms.NewLocalKeyWrapper("identity-test-v1", bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := newSAMLKeyBundle(time.Now().UTC(), "test-idp")
	if err != nil {
		t.Fatal(err)
	}
	key, certificate, err := parseSAMLKeyBundle(bundle)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &samlTestFixture{
		testingT: t, principal: principal, tenantID: tenantID,
		service:    NewService(db, identity.NewService(db, time.Hour), credentialkms.NewEnvelopeCipher(wrapper)),
		spProvider: &samlTestServiceProviderProvider{},
		session: &saml.Session{
			ID: uuid.NewString(), CreateTime: time.Now().UTC(), ExpireTime: time.Now().UTC().Add(time.Hour),
			Index: uuid.NewString(), NameID: "alice-idp-subject", UserEmail: "alice@example.com",
			UserCommonName: "Alice Example", CustomAttributes: []saml.Attribute{
				{Name: "email", FriendlyName: "email", Values: []saml.AttributeValue{{Type: "xs:string", Value: "alice@example.com"}}},
				{Name: "displayName", FriendlyName: "displayName", Values: []saml.AttributeValue{{Type: "xs:string", Value: "Alice Example"}}},
				{Name: "groups", FriendlyName: "groups", Values: []saml.AttributeValue{{Type: "xs:string", Value: "synara-security"}, {Type: "xs:string", Value: "engineering"}}},
			},
		},
	}
	fixture.db = db
	fixture.idp = &saml.IdentityProvider{
		Key: key, Certificate: certificate, SignatureMethod: dsig.RSASHA256SignatureMethod,
		ServiceProviderProvider: fixture.spProvider,
		SessionProvider: samlTestSessionProvider(func(http.ResponseWriter, *http.Request, *saml.IdpAuthnRequest) *saml.Session {
			return fixture.session
		}),
	}
	fixture.idpServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/metadata":
			w.Header().Set("Content-Type", "application/samlmetadata+xml")
			_ = xml.NewEncoder(w).Encode(fixture.idp.Metadata())
		case "/sso":
			fixture.idp.ServeSSO(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fixture.idpServer.Close)
	fixture.idp.MetadataURL = mustSAMLURL(t, fixture.idpServer.URL+"/metadata")
	fixture.idp.SSOURL = mustSAMLURL(t, fixture.idpServer.URL+"/sso")
	return fixture
}

func (f *samlTestFixture) metadataURL() string {
	return f.idpServer.URL + "/metadata"
}

func (f *samlTestFixture) signedResponse(t *testing.T, mutate func(*saml.IdpAuthnRequest)) saml.IdpAuthnRequestForm {
	t.Helper()
	started, err := f.service.Start(context.Background(), f.connection.ID, samlTestCallbackURL, "/settings/identity")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, started.AuthorizationURL, nil)
	idpRequest, err := saml.NewIdpAuthnRequest(f.idp, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := idpRequest.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (saml.DefaultAssertionMaker{}).MakeAssertion(idpRequest, f.session); err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		mutate(idpRequest)
	}
	form, err := idpRequest.PostBinding()
	if err != nil {
		t.Fatal(err)
	}
	return form
}

func (f *samlTestFixture) complete(t *testing.T, form saml.IdpAuthnRequestForm) (CallbackResult, error) {
	t.Helper()
	values := url.Values{"SAMLResponse": {form.SAMLResponse}, "RelayState": {form.RelayState}}
	request := httptest.NewRequest(http.MethodPost, samlTestCallbackURL, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return f.service.CompleteSAML(context.Background(), f.connection.ID, form.RelayState, samlTestCallbackURL, request, "127.0.0.1", "saml-test", "saml-callback")
}

type samlTestSessionProvider func(http.ResponseWriter, *http.Request, *saml.IdpAuthnRequest) *saml.Session

func (provider samlTestSessionProvider) GetSession(w http.ResponseWriter, r *http.Request, request *saml.IdpAuthnRequest) *saml.Session {
	return provider(w, r, request)
}

type samlTestServiceProviderProvider struct {
	metadata *saml.EntityDescriptor
}

func (provider *samlTestServiceProviderProvider) GetServiceProvider(_ *http.Request, entityID string) (*saml.EntityDescriptor, error) {
	if provider.metadata == nil || provider.metadata.EntityID != entityID {
		return nil, os.ErrNotExist
	}
	return provider.metadata, nil
}

func mustSAMLURL(t *testing.T, value string) url.URL {
	t.Helper()
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	return *parsed
}

func assertProblemCode(t *testing.T, err error, expected string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected problem code %q", expected)
	}
	var apiError *problem.Error
	if !errors.As(err, &apiError) || apiError.Code != expected {
		t.Fatalf("expected problem code %q, got %v", expected, err)
	}
}

func removeSAMLSignatures(element *etree.Element) int {
	removed := 0
	for _, child := range append([]*etree.Element(nil), element.ChildElements()...) {
		if child.Tag == "Signature" && child.NamespaceURI() == "http://www.w3.org/2000/09/xmldsig#" {
			element.RemoveChild(child)
			removed++
			continue
		}
		removed += removeSAMLSignatures(child)
	}
	return removed
}
