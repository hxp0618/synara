package enterpriseidentity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/crewjam/saml"
	"github.com/crewjam/saml/samlsp"
	"github.com/google/uuid"
	dsig "github.com/russellhaering/goxmldsig"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
)

const maxSAMLMetadataBytes = 2 << 20

type samlKeyBundle struct {
	PrivateKeyPEM  string `json:"privateKeyPem"`
	CertificatePEM string `json:"certificatePem"`
}

func (s *Service) prepareSAMLConnection(ctx context.Context, input CreateConnectionInput, connectionID uuid.UUID) (CreateConnectionInput, map[string]any, []byte, error) {
	if input.SAML.EntityID == "" {
		input.SAML.EntityID = "urn:synara:saml:sp:" + connectionID.String()
	}
	if err := validateAbsoluteURI(input.SAML.EntityID, "invalid_saml_entity_id", "SAML entityId"); err != nil {
		return CreateConnectionInput{}, nil, nil, err
	}
	metadata, err := s.fetchSAMLMetadata(ctx, input.SAML.MetadataURL)
	if err != nil {
		return CreateConnectionInput{}, nil, nil, err
	}
	metadataIssuer := strings.TrimSpace(metadata.EntityID)
	if metadataIssuer == "" {
		return CreateConnectionInput{}, nil, nil, problem.New(400, "saml_metadata_issuer_missing", "SAML IdP metadata does not contain an entity ID.")
	}
	if input.Issuer != "" && input.Issuer != metadataIssuer {
		return CreateConnectionInput{}, nil, nil, problem.New(400, "saml_metadata_issuer_mismatch", "SAML issuer does not match the IdP metadata entity ID.")
	}
	input.Issuer = metadataIssuer
	bundle, err := newSAMLKeyBundle(s.now(), input.SAML.EntityID)
	if err != nil {
		return CreateConnectionInput{}, nil, nil, problem.Wrap(500, "saml_key_generation_failed", "SAML service-provider signing key could not be generated.", err)
	}
	protectedSecret, err := json.Marshal(bundle)
	if err != nil {
		return CreateConnectionInput{}, nil, nil, problem.Wrap(500, "saml_key_encoding_failed", "SAML service-provider signing key could not be encoded.", err)
	}
	encoded, _ := json.Marshal(input.SAML)
	var publicConfig map[string]any
	_ = json.Unmarshal(encoded, &publicConfig)
	return input, publicConfig, protectedSecret, nil
}

func (s *Service) startSAML(ctx context.Context, connection persistence.IdentityConnection, callbackURL, returnTo string) (StartResult, error) {
	config, err := decodeSAMLConfiguration(connection.Configuration)
	if err != nil {
		return StartResult{}, err
	}
	sp, err := s.samlServiceProvider(ctx, connection, config, callbackURL)
	if err != nil {
		return StartResult{}, err
	}
	idpURL := sp.GetSSOBindingLocation(saml.HTTPRedirectBinding)
	if idpURL == "" {
		return StartResult{}, problem.New(502, "saml_redirect_binding_unavailable", "SAML IdP metadata does not advertise the HTTP-Redirect SSO binding.")
	}
	authnRequest, err := sp.MakeAuthenticationRequest(idpURL, saml.HTTPRedirectBinding, saml.HTTPPostBinding)
	if err != nil {
		return StartResult{}, problem.Wrap(502, "saml_request_generation_failed", "SAML authentication request could not be generated.", err)
	}
	relayState, _, err := secretToken()
	if err != nil {
		return StartResult{}, problem.Wrap(500, "saml_state_generation_failed", "SAML login state could not be generated.", err)
	}
	attemptID := uuid.New()
	payload, _ := json.Marshal(attemptPayload{Protocol: "saml", RequestID: authnRequest.ID})
	if err := s.createLoginAttempt(ctx, connection, attemptID, relayState, payload, returnTo, "saml"); err != nil {
		return StartResult{}, err
	}
	redirectURL, err := authnRequest.Redirect(relayState, sp)
	if err != nil {
		return StartResult{}, problem.Wrap(502, "saml_redirect_generation_failed", "SAML redirect could not be generated.", err)
	}
	return StartResult{AuthorizationURL: redirectURL.String()}, nil
}

func (s *Service) CompleteSAML(ctx context.Context, connectionID uuid.UUID, relayState, callbackURL string, request *http.Request, ipAddress, userAgent, requestID string) (CallbackResult, error) {
	relayState = strings.TrimSpace(relayState)
	if relayState == "" || strings.TrimSpace(request.FormValue("SAMLResponse")) == "" {
		return CallbackResult{}, problem.New(400, "saml_callback_invalid", "SAMLResponse and RelayState are required.")
	}
	attempt, err := s.consumeLoginAttempt(ctx, connectionID, relayState, "saml")
	if err != nil {
		return CallbackResult{}, err
	}
	attemptSecret, err := s.restoreAttemptPayload(ctx, attempt, "saml")
	if err != nil {
		return CallbackResult{}, err
	}
	if attemptSecret.Protocol != "saml" || strings.TrimSpace(attemptSecret.RequestID) == "" {
		return CallbackResult{}, problem.New(400, "saml_attempt_invalid", "SAML login attempt is invalid.")
	}
	connection, err := s.loadActiveConnection(ctx, connectionID)
	if err != nil {
		return CallbackResult{}, err
	}
	if connection.Kind != "saml" {
		return CallbackResult{}, problem.New(400, "identity_connection_protocol_mismatch", "Identity connection is not configured for SAML.")
	}
	config, err := decodeSAMLConfiguration(connection.Configuration)
	if err != nil {
		return CallbackResult{}, err
	}
	sp, err := s.samlServiceProvider(ctx, connection, config, callbackURL)
	if err != nil {
		return CallbackResult{}, err
	}
	callback, err := url.Parse(callbackURL)
	if err != nil {
		return CallbackResult{}, problem.New(503, "public_control_plane_url_invalid", "Public control-plane URL is invalid.")
	}
	validatedRequest := request.Clone(ctx)
	validatedRequest.URL = callback
	validatedRequest.Host = callback.Host
	assertion, err := sp.ParseResponse(validatedRequest, []string{attemptSecret.RequestID})
	if err != nil {
		return CallbackResult{}, problem.Wrap(401, "saml_assertion_invalid", "SAML assertion verification failed.", err)
	}
	claims := samlAssertionClaims(assertion)
	subject := ""
	if assertion.Subject != nil && assertion.Subject.NameID != nil {
		subject = strings.TrimSpace(assertion.Subject.NameID.Value)
	}
	email := firstSAMLClaim(claims, config.EmailAttribute, "email", "mail", "emailaddress", "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/emailaddress")
	if email == "" && strings.Contains(subject, "@") {
		email = subject
	}
	if subject == "" || email == "" {
		return CallbackResult{}, problem.New(403, "saml_required_claim_missing", "SAML subject and email claims are required.")
	}
	if !domainAllowed(email, config.AllowedDomains) {
		return CallbackResult{}, problem.New(403, "saml_email_domain_forbidden", "SAML email domain is not allowed for this Tenant.")
	}
	displayName := firstSAMLClaim(claims, config.DisplayNameAttribute, "displayname", "name", "http://schemas.xmlsoap.org/ws/2005/05/identity/claims/name")
	if displayName == "" {
		displayName = strings.Split(email, "@")[0]
	}
	groups := samlClaimStrings(claims, config.GroupsAttribute, "groups", "group", "memberOf")
	tenantRole, grants, err := s.resolveGroupGrants(ctx, connection, config.DefaultTenantRole, groups)
	if err != nil {
		return CallbackResult{}, err
	}
	issued, err := s.identity.CompleteExternalLogin(ctx, identity.ExternalLoginInput{
		ConnectionID: connection.ID, Provider: "saml", Issuer: connection.Issuer,
		Subject: subject, Email: email, DisplayName: displayName, Profile: sanitizeClaims(claims),
		TenantID: connection.TenantID, TenantRole: tenantRole, OrganizationGrants: grants,
	}, ipAddress, userAgent, requestID)
	if err != nil {
		return CallbackResult{}, err
	}
	return CallbackResult{Session: issued, ReturnTo: attempt.ReturnTo}, nil
}

func (s *Service) SAMLMetadata(ctx context.Context, connectionID uuid.UUID, callbackURL string) ([]byte, error) {
	connection, err := s.loadActiveConnection(ctx, connectionID)
	if err != nil {
		return nil, err
	}
	if connection.Kind != "saml" {
		return nil, problem.New(404, "saml_connection_not_found", "Active SAML connection not found.")
	}
	config, err := decodeSAMLConfiguration(connection.Configuration)
	if err != nil {
		return nil, err
	}
	sp, err := s.samlServiceProvider(ctx, connection, config, callbackURL)
	if err != nil {
		return nil, err
	}
	encoded, err := xml.MarshalIndent(sp.Metadata(), "", "  ")
	if err != nil {
		return nil, problem.Wrap(500, "saml_metadata_generation_failed", "SAML service-provider metadata could not be generated.", err)
	}
	return append([]byte(xml.Header), encoded...), nil
}

func (s *Service) samlServiceProvider(ctx context.Context, connection persistence.IdentityConnection, config SAMLConfiguration, callbackURL string) (*saml.ServiceProvider, error) {
	idpMetadata, err := s.fetchSAMLMetadata(ctx, config.MetadataURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(idpMetadata.EntityID) != connection.Issuer {
		return nil, problem.New(502, "saml_metadata_issuer_mismatch", "SAML IdP metadata entity ID changed unexpectedly.")
	}
	secretValue, err := s.decryptConnectionSecret(ctx, connection)
	if err != nil {
		return nil, err
	}
	var bundle samlKeyBundle
	if err := json.Unmarshal([]byte(secretValue), &bundle); err != nil {
		return nil, problem.Wrap(500, "saml_key_bundle_invalid", "Stored SAML service-provider key is invalid.", err)
	}
	key, certificate, err := parseSAMLKeyBundle(bundle)
	if err != nil {
		return nil, problem.Wrap(500, "saml_key_bundle_invalid", "Stored SAML service-provider key is invalid.", err)
	}
	acsURL, err := url.Parse(callbackURL)
	if err != nil || acsURL.Scheme == "" || acsURL.Host == "" {
		return nil, problem.New(503, "public_control_plane_url_invalid", "Public control-plane URL is invalid.")
	}
	metadataURL := *acsURL
	metadataURL.Path = strings.TrimSuffix(acsURL.Path, "/callback") + "/metadata"
	metadataURL.RawQuery = ""
	metadataURL.Fragment = ""
	return &saml.ServiceProvider{
		EntityID: config.EntityID, Key: key, Certificate: certificate,
		MetadataURL: metadataURL, AcsURL: *acsURL, IDPMetadata: idpMetadata,
		AuthnNameIDFormat:     saml.UnspecifiedNameIDFormat,
		MetadataValidDuration: 24 * time.Hour,
		SignatureMethod:       dsig.RSASHA256SignatureMethod,
	}, nil
}

func (s *Service) fetchSAMLMetadata(ctx context.Context, metadataURL string) (*saml.EntityDescriptor, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataURL, nil)
	if err != nil {
		return nil, problem.New(400, "invalid_saml_metadata_url", "SAML metadataUrl is invalid.")
	}
	client := *s.httpClient
	configuredRedirectCheck := client.CheckRedirect
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("SAML metadata exceeded the redirect limit")
		}
		if err := validateHTTPSOrLoopbackURL(request.URL.String(), "invalid_saml_metadata_redirect", "SAML metadata redirect"); err != nil {
			return err
		}
		if configuredRedirectCheck != nil {
			return configuredRedirectCheck(request, via)
		}
		return nil
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, problem.Wrap(502, "saml_metadata_fetch_failed", "SAML IdP metadata could not be fetched.", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, problem.New(502, "saml_metadata_fetch_failed", fmt.Sprintf("SAML IdP metadata returned HTTP %d.", response.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxSAMLMetadataBytes+1))
	if err != nil {
		return nil, problem.Wrap(502, "saml_metadata_fetch_failed", "SAML IdP metadata could not be read.", err)
	}
	if len(data) > maxSAMLMetadataBytes {
		return nil, problem.New(502, "saml_metadata_too_large", "SAML IdP metadata exceeds the 2 MiB limit.")
	}
	metadata, err := samlsp.ParseMetadata(data)
	if err != nil {
		return nil, problem.Wrap(502, "saml_metadata_invalid", "SAML IdP metadata is invalid.", err)
	}
	return metadata, nil
}

func normalizeSAMLConfiguration(config SAMLConfiguration) (SAMLConfiguration, error) {
	config.MetadataURL = strings.TrimSpace(config.MetadataURL)
	if err := validateHTTPSOrLoopbackURL(config.MetadataURL, "invalid_saml_metadata_url", "SAML metadataUrl"); err != nil {
		return SAMLConfiguration{}, err
	}
	config.EntityID = strings.TrimSpace(config.EntityID)
	if config.EntityID != "" {
		if err := validateAbsoluteURI(config.EntityID, "invalid_saml_entity_id", "SAML entityId"); err != nil {
			return SAMLConfiguration{}, err
		}
	}
	var err error
	config.EmailAttribute, err = normalizeSAMLAttribute(config.EmailAttribute, "email")
	if err != nil {
		return SAMLConfiguration{}, err
	}
	config.DisplayNameAttribute, err = normalizeSAMLAttribute(config.DisplayNameAttribute, "displayName")
	if err != nil {
		return SAMLConfiguration{}, err
	}
	config.GroupsAttribute, err = normalizeSAMLAttribute(config.GroupsAttribute, "groups")
	if err != nil {
		return SAMLConfiguration{}, err
	}
	config.AllowedDomains, err = normalizeAllowedDomains(config.AllowedDomains, "invalid_saml_allowed_domain", "SAML allowedDomains")
	if err != nil {
		return SAMLConfiguration{}, err
	}
	config.DefaultTenantRole = strings.ToLower(strings.TrimSpace(config.DefaultTenantRole))
	if config.DefaultTenantRole == "" {
		config.DefaultTenantRole = "member"
	}
	if !validTenantRole(config.DefaultTenantRole) {
		return SAMLConfiguration{}, problem.New(400, "invalid_saml_default_role", "SAML defaultTenantRole is invalid.")
	}
	return config, nil
}

func decodeSAMLConfiguration(raw map[string]any) (SAMLConfiguration, error) {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return SAMLConfiguration{}, problem.Wrap(500, "saml_configuration_invalid", "Stored SAML configuration is invalid.", err)
	}
	var config SAMLConfiguration
	if err := json.Unmarshal(encoded, &config); err != nil {
		return SAMLConfiguration{}, problem.Wrap(500, "saml_configuration_invalid", "Stored SAML configuration is invalid.", err)
	}
	return normalizeSAMLConfiguration(config)
}

func normalizeSAMLAttribute(value, fallback string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = fallback
	}
	if len(value) > 500 || strings.ContainsAny(value, "\r\n\t") {
		return "", problem.New(400, "invalid_saml_attribute", "SAML attribute names must not exceed 500 characters or contain control whitespace.")
	}
	return value, nil
}

func normalizeAllowedDomains(values []string, code, label string) ([]string, error) {
	unique := map[string]struct{}{}
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" || strings.ContainsAny(value, "/:@ ") || len(value) > 253 {
			return nil, problem.New(400, code, label+" contains an invalid domain.")
		}
		unique[value] = struct{}{}
	}
	result := make([]string, 0, len(unique))
	for value := range unique {
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func validateHTTPSOrLoopbackURL(value, code, label string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return problem.New(400, code, label+" must be an HTTPS URL without query or fragment.")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	host := strings.ToLower(parsed.Hostname())
	ip := net.ParseIP(host)
	if parsed.Scheme == "http" && (host == "localhost" || ip != nil && ip.IsLoopback()) {
		return nil
	}
	return problem.New(400, code, label+" must use HTTPS; HTTP is allowed only for loopback development endpoints.")
}

func validateAbsoluteURI(value, code, label string) error {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || !parsed.IsAbs() || len(value) > 1000 || strings.ContainsAny(value, "\r\n\t ") {
		return problem.New(400, code, label+" must be an absolute URI and must not exceed 1000 characters.")
	}
	return nil
}

func newSAMLKeyBundle(now time.Time, entityID string) (samlKeyBundle, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return samlKeyBundle{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return samlKeyBundle{}, err
	}
	template := x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: entityID},
		NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return samlKeyBundle{}, err
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return samlKeyBundle{}, err
	}
	return samlKeyBundle{
		PrivateKeyPEM:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})),
		CertificatePEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER})),
	}, nil
}

func parseSAMLKeyBundle(bundle samlKeyBundle) (*rsa.PrivateKey, *x509.Certificate, error) {
	privateBlock, _ := pem.Decode([]byte(bundle.PrivateKeyPEM))
	certificateBlock, _ := pem.Decode([]byte(bundle.CertificatePEM))
	if privateBlock == nil || certificateBlock == nil {
		return nil, nil, fmt.Errorf("missing PEM block")
	}
	parsedKey, err := x509.ParsePKCS8PrivateKey(privateBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	key, ok := parsedKey.(*rsa.PrivateKey)
	if !ok {
		return nil, nil, fmt.Errorf("private key is not RSA")
	}
	certificate, err := x509.ParseCertificate(certificateBlock.Bytes)
	if err != nil {
		return nil, nil, err
	}
	publicKey, ok := certificate.PublicKey.(*rsa.PublicKey)
	if !ok || !publicKey.Equal(&key.PublicKey) {
		return nil, nil, fmt.Errorf("certificate does not match private key")
	}
	return key, certificate, nil
}

func samlAssertionClaims(assertion *saml.Assertion) map[string]any {
	claims := map[string]any{}
	for _, statement := range assertion.AttributeStatements {
		for _, attribute := range statement.Attributes {
			if len(claims) >= 100 {
				return claims
			}
			name := strings.TrimSpace(attribute.Name)
			if name == "" {
				name = strings.TrimSpace(attribute.FriendlyName)
			}
			if name == "" {
				continue
			}
			values := make([]string, 0, len(attribute.Values))
			for _, attributeValue := range attribute.Values {
				value := strings.TrimSpace(attributeValue.Value)
				if value == "" && attributeValue.NameID != nil {
					value = strings.TrimSpace(attributeValue.NameID.Value)
				}
				if value != "" && len(value) <= 2000 && len(values) < 50 {
					values = append(values, value)
				}
			}
			if len(values) == 1 {
				claims[name] = values[0]
			} else if len(values) > 1 {
				claims[name] = values
			}
		}
	}
	return claims
}

func firstSAMLClaim(claims map[string]any, names ...string) string {
	for _, name := range names {
		for key, value := range claims {
			if !strings.EqualFold(strings.TrimSpace(key), strings.TrimSpace(name)) {
				continue
			}
			values := claimStrings(value)
			if len(values) > 0 {
				return values[0]
			}
		}
	}
	return ""
}

func samlClaimStrings(claims map[string]any, names ...string) []string {
	unique := map[string]struct{}{}
	for _, name := range names {
		for key, value := range claims {
			if strings.EqualFold(strings.TrimSpace(key), strings.TrimSpace(name)) {
				for _, item := range claimStrings(value) {
					unique[item] = struct{}{}
				}
			}
		}
	}
	result := make([]string, 0, len(unique))
	for value := range unique {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func secretToken() (string, []byte, error) {
	return secret.NewToken()
}
