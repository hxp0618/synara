package enterpriseidentity

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	credentialkms "github.com/synara-ai/synara/services/control-plane/internal/kms"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/secret"
	"github.com/synara-ai/synara/services/control-plane/internal/validation"
)

const loginAttemptTTL = 10 * time.Minute

type OIDCConfiguration struct {
	Scopes            []string `json:"scopes"`
	AllowedDomains    []string `json:"allowedDomains"`
	GroupsClaim       string   `json:"groupsClaim"`
	DefaultTenantRole string   `json:"defaultTenantRole"`
}

type SAMLConfiguration struct {
	MetadataURL          string   `json:"metadataUrl"`
	EntityID             string   `json:"entityId"`
	EmailAttribute       string   `json:"emailAttribute"`
	DisplayNameAttribute string   `json:"displayNameAttribute"`
	GroupsAttribute      string   `json:"groupsAttribute"`
	AllowedDomains       []string `json:"allowedDomains"`
	DefaultTenantRole    string   `json:"defaultTenantRole"`
}

type Connection struct {
	ID            uuid.UUID      `json:"id"`
	TenantID      uuid.UUID      `json:"tenantId"`
	Kind          string         `json:"kind"`
	Name          string         `json:"name"`
	Status        string         `json:"status"`
	Issuer        string         `json:"issuer"`
	ClientID      *string        `json:"clientId"`
	Configuration map[string]any `json:"configuration"`
	CreatedBy     uuid.UUID      `json:"createdBy"`
	UpdatedBy     uuid.UUID      `json:"updatedBy"`
	CreatedAt     time.Time      `json:"createdAt"`
	UpdatedAt     time.Time      `json:"updatedAt"`
}

type PublicConnection struct {
	ID       uuid.UUID `json:"id"`
	TenantID uuid.UUID `json:"tenantId"`
	Kind     string    `json:"kind"`
	Name     string    `json:"name"`
}

type CreateConnectionInput struct {
	Kind         string            `json:"kind"`
	Name         string            `json:"name"`
	Issuer       string            `json:"issuer"`
	ClientID     string            `json:"clientId"`
	ClientSecret string            `json:"clientSecret"`
	OIDC         OIDCConfiguration `json:"oidc"`
	SAML         SAMLConfiguration `json:"saml"`
}

type MappingInput struct {
	ExternalGroup    string     `json:"externalGroup"`
	TenantRole       *string    `json:"tenantRole"`
	OrganizationID   *uuid.UUID `json:"organizationId"`
	OrganizationRole *string    `json:"organizationRole"`
}

type Mapping struct {
	ID               uuid.UUID  `json:"id"`
	ExternalGroup    string     `json:"externalGroup"`
	TenantRole       *string    `json:"tenantRole"`
	OrganizationID   *uuid.UUID `json:"organizationId"`
	OrganizationRole *string    `json:"organizationRole"`
}

type StartResult struct {
	AuthorizationURL string `json:"authorizationUrl"`
}

type CallbackResult struct {
	Session  identity.IssuedSession
	ReturnTo string
}

type attemptPayload struct {
	Protocol     string `json:"protocol"`
	Nonce        string `json:"nonce"`
	CodeVerifier string `json:"codeVerifier"`
	RequestID    string `json:"requestId"`
}

type oidcClaims struct {
	Subject           string `json:"sub"`
	Email             string `json:"email"`
	EmailVerified     *bool  `json:"email_verified"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
	Nonce             string `json:"nonce"`
}

type Service struct {
	db         *gorm.DB
	authorizer *authorization.Authorizer
	identity   *identity.Service
	cipher     *credentialkms.EnvelopeCipher
	httpClient *http.Client
	now        func() time.Time
}

func NewService(db *gorm.DB, identityService *identity.Service, cipher *credentialkms.EnvelopeCipher) *Service {
	return &Service{db: db, authorizer: authorization.NewAuthorizer(db), identity: identityService, cipher: cipher, httpClient: &http.Client{Timeout: 15 * time.Second}, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) List(ctx context.Context, principal identity.Principal, tenantID uuid.UUID) ([]Connection, error) {
	if err := s.authorize(ctx, principal, tenantID, authorization.IdentityRead); err != nil {
		return nil, err
	}
	var models []persistence.IdentityConnection
	if err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Order("status, LOWER(name), id").Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "identity_connections_load_failed", "Identity connections could not be loaded.", err)
	}
	items := make([]Connection, 0, len(models))
	for _, model := range models {
		items = append(items, toConnection(model))
	}
	return items, nil
}

func (s *Service) ListPublic(ctx context.Context, tenantSlug string) ([]PublicConnection, error) {
	tenantSlug = strings.ToLower(strings.TrimSpace(tenantSlug))
	if tenantSlug == "" {
		return nil, problem.New(400, "tenant_slug_required", "tenantSlug is required.")
	}
	var models []persistence.IdentityConnection
	err := s.db.WithContext(ctx).Table("identity_connections AS ic").Select("ic.*").
		Joins("JOIN tenants AS t ON t.id = ic.tenant_id").
		Where("LOWER(t.slug) = ? AND t.status = ? AND t.deleted_at IS NULL AND ic.status = ?", tenantSlug, "active", "active").
		Order("LOWER(ic.name), ic.id").Find(&models).Error
	if err != nil {
		return nil, problem.Wrap(500, "identity_connections_load_failed", "Identity connections could not be loaded.", err)
	}
	items := make([]PublicConnection, 0, len(models))
	for _, model := range models {
		items = append(items, PublicConnection{ID: model.ID, TenantID: model.TenantID, Kind: model.Kind, Name: model.Name})
	}
	return items, nil
}

func (s *Service) Create(ctx context.Context, principal identity.Principal, tenantID uuid.UUID, input CreateConnectionInput, requestID, ipAddress string) (Connection, error) {
	if err := s.authorize(ctx, principal, tenantID, authorization.IdentityManage); err != nil {
		return Connection{}, err
	}
	normalized, publicConfig, err := normalizeConnection(input)
	if err != nil {
		return Connection{}, err
	}
	id := uuid.New()
	var protectedSecret []byte
	if normalized.Kind == "oidc" {
		protectedSecret = []byte(normalized.ClientSecret)
	} else {
		normalized, publicConfig, protectedSecret, err = s.prepareSAMLConnection(ctx, normalized, id)
		if err != nil {
			return Connection{}, err
		}
	}
	defer zero(protectedSecret)
	model := persistence.IdentityConnection{
		ID: id, TenantID: tenantID, Kind: normalized.Kind, Name: normalized.Name, Status: "active",
		Issuer: normalized.Issuer, Configuration: publicConfig, CreatedBy: principal.UserID, UpdatedBy: principal.UserID,
	}
	if normalized.ClientID != "" {
		model.ClientID = &normalized.ClientID
	}
	if len(protectedSecret) > 0 {
		if s.cipher == nil {
			return Connection{}, problem.New(503, "identity_kms_unavailable", "Identity connection KMS is not configured.")
		}
		envelope, err := s.cipher.Encrypt(ctx, protectedSecret, connectionAAD(tenantID, id, normalized.Kind))
		if err != nil {
			return Connection{}, problem.Wrap(503, "identity_secret_encryption_failed", "Identity connection secret could not be encrypted.", err)
		}
		model.EncryptedSecret = envelope.EncryptedPayload
		model.EncryptedDataKey = envelope.EncryptedDataKey
		model.KMSProvider = &envelope.KMSProvider
		model.KMSKeyID = &envelope.KMSKeyID
	}
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		if err := tx.Create(&model).Error; err != nil {
			return problem.Wrap(409, "identity_connection_create_rejected", "Identity connection creation was rejected.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID, Action: "identity_connection.created", ResourceType: "identity_connection", ResourceID: &id, RequestID: requestID, IPAddress: ipAddress, Metadata: map[string]any{"kind": model.Kind, "name": model.Name}})
	})
	if err != nil {
		return Connection{}, err
	}
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, id).Take(&model).Error; err != nil {
		return Connection{}, err
	}
	return toConnection(model), nil
}

func (s *Service) Disable(ctx context.Context, principal identity.Principal, tenantID, connectionID uuid.UUID, requestID, ipAddress string) error {
	if err := s.authorize(ctx, principal, tenantID, authorization.IdentityManage); err != nil {
		return err
	}
	return persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		result := tx.Model(&persistence.IdentityConnection{}).Where("tenant_id = ? AND id = ? AND status <> ?", tenantID, connectionID, "disabled").Updates(map[string]any{"status": "disabled", "updated_by": principal.UserID})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			var count int64
			if err := tx.Model(&persistence.IdentityConnection{}).Where("tenant_id = ? AND id = ?", tenantID, connectionID).Count(&count).Error; err != nil {
				return err
			}
			if count == 0 {
				return problem.New(404, "identity_connection_not_found", "Identity connection not found.")
			}
			return nil
		}
		return audit.Record(ctx, tx, audit.Entry{TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID, Action: "identity_connection.disabled", ResourceType: "identity_connection", ResourceID: &connectionID, RequestID: requestID, IPAddress: ipAddress})
	})
}

func (s *Service) ListMappings(ctx context.Context, principal identity.Principal, tenantID, connectionID uuid.UUID) ([]Mapping, error) {
	if err := s.authorize(ctx, principal, tenantID, authorization.IdentityRead); err != nil {
		return nil, err
	}
	var models []persistence.IdentityGroupMapping
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND connection_id = ?", tenantID, connectionID).Order("LOWER(external_group), organization_id, id").Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "identity_group_mappings_load_failed", "Identity Group mappings could not be loaded.", err)
	}
	items := make([]Mapping, 0, len(models))
	for _, model := range models {
		items = append(items, toMapping(model))
	}
	return items, nil
}

func (s *Service) ReplaceMappings(ctx context.Context, principal identity.Principal, tenantID, connectionID uuid.UUID, inputs []MappingInput, requestID, ipAddress string) ([]Mapping, error) {
	if err := s.authorize(ctx, principal, tenantID, authorization.IdentityManage); err != nil {
		return nil, err
	}
	models := make([]persistence.IdentityGroupMapping, 0, len(inputs))
	seen := map[string]struct{}{}
	for _, input := range inputs {
		normalized, err := normalizeMapping(input)
		if err != nil {
			return nil, err
		}
		key := strings.ToLower(normalized.ExternalGroup) + "\x00"
		if normalized.OrganizationID != nil {
			key += normalized.OrganizationID.String()
		}
		if _, duplicate := seen[key]; duplicate {
			return nil, problem.New(400, "duplicate_identity_group_mapping", "Identity Group mappings must be unique.")
		}
		seen[key] = struct{}{}
		if normalized.OrganizationID != nil {
			if _, err := s.authorizer.RequireOrganization(ctx, principal.UserID, tenantID, *normalized.OrganizationID, authorization.OrganizationRead); err != nil {
				return nil, err
			}
		}
		models = append(models, persistence.IdentityGroupMapping{ID: uuid.New(), TenantID: tenantID, ConnectionID: connectionID, ExternalGroup: normalized.ExternalGroup, TenantRole: normalized.TenantRole, OrganizationID: normalized.OrganizationID, OrganizationRole: normalized.OrganizationRole, CreatedBy: principal.UserID})
	}
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var connection persistence.IdentityConnection
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("tenant_id = ? AND id = ?", tenantID, connectionID).Take(&connection).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "identity_connection_not_found", "Identity connection not found.")
		} else if err != nil {
			return err
		}
		if err := tx.Where("tenant_id = ? AND connection_id = ?", tenantID, connectionID).Delete(&persistence.IdentityGroupMapping{}).Error; err != nil {
			return err
		}
		if len(models) > 0 {
			if err := tx.Create(&models).Error; err != nil {
				return problem.Wrap(409, "identity_group_mappings_update_rejected", "Identity Group mappings could not be updated.", err)
			}
		}
		return audit.Record(ctx, tx, audit.Entry{TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID, Action: "identity_group_mappings.replaced", ResourceType: "identity_connection", ResourceID: &connectionID, RequestID: requestID, IPAddress: ipAddress, Metadata: map[string]any{"count": len(models)}})
	})
	if err != nil {
		return nil, err
	}
	return s.ListMappings(ctx, principal, tenantID, connectionID)
}

func (s *Service) Start(ctx context.Context, connectionID uuid.UUID, callbackURL, returnTo string) (StartResult, error) {
	connection, err := s.loadActiveConnection(ctx, connectionID)
	if err != nil {
		return StartResult{}, err
	}
	if connection.Kind == "saml" {
		return s.startSAML(ctx, connection, callbackURL, returnTo)
	}
	oidcConfig, err := decodeOIDCConfiguration(connection.Configuration)
	if err != nil {
		return StartResult{}, err
	}
	provider, err := oidc.NewProvider(ctx, connection.Issuer)
	if err != nil {
		return StartResult{}, problem.Wrap(502, "oidc_discovery_failed", "OIDC discovery failed.", err)
	}
	clientSecret, err := s.decryptConnectionSecret(ctx, connection)
	if err != nil {
		return StartResult{}, err
	}
	state, _, err := secret.NewToken()
	if err != nil {
		return StartResult{}, problem.Wrap(500, "oidc_state_generation_failed", "OIDC login state could not be generated.", err)
	}
	nonce, _, err := secret.NewToken()
	if err != nil {
		return StartResult{}, problem.Wrap(500, "oidc_nonce_generation_failed", "OIDC login nonce could not be generated.", err)
	}
	verifier := oauth2.GenerateVerifier()
	attemptID := uuid.New()
	payload, _ := json.Marshal(attemptPayload{Protocol: "oidc", Nonce: nonce, CodeVerifier: verifier})
	if err := s.createLoginAttempt(ctx, connection, attemptID, state, payload, returnTo, "oidc"); err != nil {
		return StartResult{}, err
	}
	oauthConfig := oauth2.Config{ClientID: valueOrEmpty(connection.ClientID), ClientSecret: clientSecret, Endpoint: provider.Endpoint(), RedirectURL: callbackURL, Scopes: oidcConfig.Scopes}
	authorizationURL := oauthConfig.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
	return StartResult{AuthorizationURL: authorizationURL}, nil
}

func (s *Service) CompleteOIDC(ctx context.Context, connectionID uuid.UUID, state, code, callbackURL, ipAddress, userAgent, requestID string) (CallbackResult, error) {
	state = strings.TrimSpace(state)
	code = strings.TrimSpace(code)
	if state == "" || code == "" {
		return CallbackResult{}, problem.New(400, "oidc_callback_invalid", "OIDC callback state and code are required.")
	}
	attempt, err := s.consumeLoginAttempt(ctx, connectionID, state, "oidc")
	if err != nil {
		return CallbackResult{}, err
	}
	connection, oidcConfig, err := s.loadOIDCConnection(ctx, connectionID)
	if err != nil {
		return CallbackResult{}, err
	}
	attemptSecret, err := s.restoreAttemptPayload(ctx, attempt, "oidc")
	if err != nil {
		return CallbackResult{}, err
	}
	if attemptSecret.Protocol != "" && attemptSecret.Protocol != "oidc" {
		return CallbackResult{}, problem.New(400, "oidc_attempt_invalid", "OIDC login attempt is invalid.")
	}
	provider, err := oidc.NewProvider(ctx, connection.Issuer)
	if err != nil {
		return CallbackResult{}, problem.Wrap(502, "oidc_discovery_failed", "OIDC discovery failed.", err)
	}
	clientSecret, err := s.decryptConnectionSecret(ctx, connection)
	if err != nil {
		return CallbackResult{}, err
	}
	oauthConfig := oauth2.Config{ClientID: valueOrEmpty(connection.ClientID), ClientSecret: clientSecret, Endpoint: provider.Endpoint(), RedirectURL: callbackURL, Scopes: oidcConfig.Scopes}
	token, err := oauthConfig.Exchange(ctx, code, oauth2.VerifierOption(attemptSecret.CodeVerifier))
	if err != nil {
		return CallbackResult{}, problem.Wrap(401, "oidc_code_exchange_failed", "OIDC authorization code exchange failed.", err)
	}
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || strings.TrimSpace(rawIDToken) == "" {
		return CallbackResult{}, problem.New(401, "oidc_id_token_missing", "OIDC provider did not return an ID token.")
	}
	verified, err := provider.Verifier(&oidc.Config{ClientID: valueOrEmpty(connection.ClientID)}).Verify(ctx, rawIDToken)
	if err != nil {
		return CallbackResult{}, problem.Wrap(401, "oidc_id_token_invalid", "OIDC ID token verification failed.", err)
	}
	var claims map[string]any
	if err := verified.Claims(&claims); err != nil {
		return CallbackResult{}, problem.Wrap(401, "oidc_claims_invalid", "OIDC claims could not be decoded.", err)
	}
	encodedClaims, _ := json.Marshal(claims)
	var standard oidcClaims
	_ = json.Unmarshal(encodedClaims, &standard)
	if subtle.ConstantTimeCompare([]byte(standard.Nonce), []byte(attemptSecret.Nonce)) != 1 {
		return CallbackResult{}, problem.New(401, "oidc_nonce_invalid", "OIDC nonce validation failed.")
	}
	if strings.TrimSpace(standard.Subject) == "" || strings.TrimSpace(standard.Email) == "" {
		return CallbackResult{}, problem.New(403, "oidc_required_claim_missing", "OIDC subject and email claims are required.")
	}
	if standard.EmailVerified != nil && !*standard.EmailVerified {
		return CallbackResult{}, problem.New(403, "oidc_email_unverified", "OIDC email address is not verified.")
	}
	if !domainAllowed(standard.Email, oidcConfig.AllowedDomains) {
		return CallbackResult{}, problem.New(403, "oidc_email_domain_forbidden", "OIDC email domain is not allowed for this Tenant.")
	}
	displayName := strings.TrimSpace(standard.Name)
	if displayName == "" {
		displayName = strings.TrimSpace(standard.PreferredUsername)
	}
	if displayName == "" {
		displayName = strings.Split(standard.Email, "@")[0]
	}
	groups := claimStrings(claims[oidcConfig.GroupsClaim])
	tenantRole, grants, err := s.resolveGroupGrants(ctx, connection, oidcConfig.DefaultTenantRole, groups)
	if err != nil {
		return CallbackResult{}, err
	}
	issued, err := s.identity.CompleteExternalLogin(ctx, identity.ExternalLoginInput{ConnectionID: connection.ID, Provider: "oidc", Issuer: connection.Issuer, Subject: standard.Subject, Email: standard.Email, DisplayName: displayName, Profile: sanitizeClaims(claims), TenantID: connection.TenantID, TenantRole: tenantRole, OrganizationGrants: grants}, ipAddress, userAgent, requestID)
	if err != nil {
		return CallbackResult{}, err
	}
	return CallbackResult{Session: issued, ReturnTo: attempt.ReturnTo}, nil
}

func (s *Service) loadOIDCConnection(ctx context.Context, connectionID uuid.UUID) (persistence.IdentityConnection, OIDCConfiguration, error) {
	connection, err := s.loadActiveConnection(ctx, connectionID)
	if err != nil {
		return persistence.IdentityConnection{}, OIDCConfiguration{}, err
	}
	if connection.Kind != "oidc" {
		return persistence.IdentityConnection{}, OIDCConfiguration{}, problem.New(400, "identity_connection_protocol_mismatch", "Identity connection is not configured for OIDC.")
	}
	config, err := decodeOIDCConfiguration(connection.Configuration)
	if err != nil {
		return persistence.IdentityConnection{}, OIDCConfiguration{}, err
	}
	return connection, config, nil
}

func (s *Service) loadActiveConnection(ctx context.Context, connectionID uuid.UUID) (persistence.IdentityConnection, error) {
	var connection persistence.IdentityConnection
	err := s.db.WithContext(ctx).Table("identity_connections AS ic").Select("ic.*").Joins("JOIN tenants AS t ON t.id = ic.tenant_id").Where("ic.id = ? AND ic.status = ? AND t.status = ? AND t.deleted_at IS NULL", connectionID, "active", "active").Take(&connection).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return persistence.IdentityConnection{}, problem.New(404, "identity_connection_not_found", "Active identity connection not found.")
	}
	if err != nil {
		return persistence.IdentityConnection{}, problem.Wrap(500, "identity_connection_load_failed", "Identity connection could not be loaded.", err)
	}
	return connection, nil
}

func (s *Service) createLoginAttempt(ctx context.Context, connection persistence.IdentityConnection, attemptID uuid.UUID, state string, payload []byte, returnTo, protocol string) error {
	if s.cipher == nil {
		return problem.New(503, "identity_kms_unavailable", "Identity connection KMS is not configured.")
	}
	envelope, err := s.cipher.Encrypt(ctx, payload, attemptAAD(connection.TenantID, attemptID, connection.ID))
	if err != nil {
		return problem.Wrap(503, protocol+"_attempt_encryption_failed", strings.ToUpper(protocol)+" login attempt could not be protected.", err)
	}
	stateHash := sha256.Sum256([]byte(state))
	attempt := persistence.IdentityLoginAttempt{
		ID: attemptID, TenantID: connection.TenantID, ConnectionID: connection.ID, StateHash: stateHash[:],
		EncryptedPayload: envelope.EncryptedPayload, EncryptedDataKey: envelope.EncryptedDataKey,
		KMSProvider: envelope.KMSProvider, KMSKeyID: envelope.KMSKeyID,
		ReturnTo: normalizeReturnTo(returnTo), ExpiresAt: s.now().Add(loginAttemptTTL),
	}
	if err := s.db.WithContext(ctx).Create(&attempt).Error; err != nil {
		return problem.Wrap(409, protocol+"_attempt_create_rejected", strings.ToUpper(protocol)+" login attempt could not be created.", err)
	}
	return nil
}

func (s *Service) consumeLoginAttempt(ctx context.Context, connectionID uuid.UUID, state, protocol string) (persistence.IdentityLoginAttempt, error) {
	stateHash := sha256.Sum256([]byte(strings.TrimSpace(state)))
	var attempt persistence.IdentityLoginAttempt
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").Where("connection_id = ? AND state_hash = ? AND consumed_at IS NULL AND expires_at > ?", connectionID, stateHash[:], s.now()).Take(&attempt).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(400, protocol+"_state_invalid", strings.ToUpper(protocol)+" login state is invalid or expired.")
		} else if err != nil {
			return err
		}
		return tx.Model(&attempt).Update("consumed_at", s.now()).Error
	})
	return attempt, err
}

func (s *Service) restoreAttemptPayload(ctx context.Context, attempt persistence.IdentityLoginAttempt, protocol string) (attemptPayload, error) {
	if s.cipher == nil {
		return attemptPayload{}, problem.New(503, "identity_kms_unavailable", "Identity connection KMS is not configured.")
	}
	payload, err := s.cipher.Decrypt(ctx, credentialkms.Envelope{
		EncryptedPayload: attempt.EncryptedPayload, EncryptedDataKey: attempt.EncryptedDataKey,
		KMSProvider: attempt.KMSProvider, KMSKeyID: attempt.KMSKeyID,
	}, attemptAAD(attempt.TenantID, attempt.ID, attempt.ConnectionID))
	if err != nil {
		return attemptPayload{}, problem.Wrap(503, protocol+"_attempt_decryption_failed", strings.ToUpper(protocol)+" login attempt could not be restored.", err)
	}
	defer zero(payload)
	var result attemptPayload
	if err := json.Unmarshal(payload, &result); err != nil {
		return attemptPayload{}, problem.Wrap(500, protocol+"_attempt_invalid", strings.ToUpper(protocol)+" login attempt is invalid.", err)
	}
	return result, nil
}

func (s *Service) decryptConnectionSecret(ctx context.Context, connection persistence.IdentityConnection) (string, error) {
	if len(connection.EncryptedSecret) == 0 {
		return "", nil
	}
	if s.cipher == nil || connection.KMSProvider == nil || connection.KMSKeyID == nil {
		return "", problem.New(503, "identity_kms_unavailable", "Identity connection KMS is not configured.")
	}
	plaintext, err := s.cipher.Decrypt(ctx, credentialkms.Envelope{EncryptedPayload: connection.EncryptedSecret, EncryptedDataKey: connection.EncryptedDataKey, KMSProvider: *connection.KMSProvider, KMSKeyID: *connection.KMSKeyID}, connectionAAD(connection.TenantID, connection.ID, connection.Kind))
	if err != nil {
		return "", problem.Wrap(503, "identity_secret_decryption_failed", "Identity connection secret could not be decrypted.", err)
	}
	defer zero(plaintext)
	return string(plaintext), nil
}

func (s *Service) resolveGroupGrants(ctx context.Context, connection persistence.IdentityConnection, defaultRole string, groups []string) (string, []identity.OrganizationGrant, error) {
	tenantRole := defaultRole
	if tenantRole == "" {
		tenantRole = "member"
	}
	if len(groups) == 0 {
		return tenantRole, nil, nil
	}
	var mappings []persistence.IdentityGroupMapping
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND connection_id = ? AND external_group IN ?", connection.TenantID, connection.ID, groups).Find(&mappings).Error; err != nil {
		return "", nil, problem.Wrap(500, "identity_group_mappings_load_failed", "Identity Group mappings could not be loaded.", err)
	}
	grants := make(map[uuid.UUID]string)
	for _, mapping := range mappings {
		if mapping.TenantRole != nil {
			tenantRole = strongerTenantRole(tenantRole, *mapping.TenantRole)
		}
		if mapping.OrganizationID != nil && mapping.OrganizationRole != nil {
			current := grants[*mapping.OrganizationID]
			grants[*mapping.OrganizationID] = strongerOrganizationRole(current, *mapping.OrganizationRole)
		}
	}
	result := make([]identity.OrganizationGrant, 0, len(grants))
	for organizationID, role := range grants {
		result = append(result, identity.OrganizationGrant{OrganizationID: organizationID, Role: role})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].OrganizationID.String() < result[j].OrganizationID.String() })
	return tenantRole, result, nil
}

func (s *Service) authorize(ctx context.Context, principal identity.Principal, tenantID uuid.UUID, permission authorization.Permission) error {
	if principal.ActiveTenantID == nil || *principal.ActiveTenantID != tenantID {
		return problem.New(404, "tenant_not_found", "Tenant not found.")
	}
	_, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, permission)
	return err
}

func normalizeConnection(input CreateConnectionInput) (CreateConnectionInput, map[string]any, error) {
	input.Kind = strings.ToLower(strings.TrimSpace(input.Kind))
	if input.Kind != "oidc" && input.Kind != "saml" {
		return CreateConnectionInput{}, nil, problem.New(400, "invalid_identity_connection_kind", "Identity connection kind must be oidc or saml.")
	}
	var err error
	input.Name, err = validation.Name(input.Name, "invalid_identity_connection_name", "Identity connection name", 160)
	if err != nil {
		return CreateConnectionInput{}, nil, err
	}
	input.Issuer = strings.TrimRight(strings.TrimSpace(input.Issuer), "/")
	input.ClientID = strings.TrimSpace(input.ClientID)
	if input.Kind == "oidc" {
		issuerURL, err := url.Parse(input.Issuer)
		if err != nil || issuerURL.Scheme != "https" || issuerURL.Host == "" || issuerURL.RawQuery != "" || issuerURL.Fragment != "" {
			return CreateConnectionInput{}, nil, problem.New(400, "invalid_identity_connection_issuer", "OIDC issuer must be an HTTPS URL without query or fragment.")
		}
		if len(input.Issuer) > 1000 {
			return CreateConnectionInput{}, nil, problem.New(400, "invalid_identity_connection_issuer", "OIDC issuer is too long.")
		}
		if input.ClientID == "" || len(input.ClientID) > 500 {
			return CreateConnectionInput{}, nil, problem.New(400, "invalid_oidc_client_id", "OIDC clientId is required and must not exceed 500 characters.")
		}
		config, err := normalizeOIDCConfiguration(input.OIDC)
		if err != nil {
			return CreateConnectionInput{}, nil, err
		}
		encoded, _ := json.Marshal(config)
		var public map[string]any
		_ = json.Unmarshal(encoded, &public)
		return input, public, nil
	}
	input.ClientID = ""
	input.ClientSecret = ""
	config, err := normalizeSAMLConfiguration(input.SAML)
	if err != nil {
		return CreateConnectionInput{}, nil, err
	}
	input.SAML = config
	if input.Issuer != "" {
		issuerURL, err := url.Parse(input.Issuer)
		if err != nil || !issuerURL.IsAbs() || len(input.Issuer) > 1000 {
			return CreateConnectionInput{}, nil, problem.New(400, "invalid_identity_connection_issuer", "SAML issuer must be an absolute URI and must not exceed 1000 characters.")
		}
	}
	encoded, _ := json.Marshal(config)
	var public map[string]any
	_ = json.Unmarshal(encoded, &public)
	return input, public, nil
}

func normalizeOIDCConfiguration(config OIDCConfiguration) (OIDCConfiguration, error) {
	unique := map[string]struct{}{"openid": {}, "email": {}, "profile": {}}
	for _, scope := range config.Scopes {
		scope = strings.TrimSpace(scope)
		if scope != "" && len(scope) <= 120 && !strings.ContainsAny(scope, "\r\n\t ") {
			unique[scope] = struct{}{}
		}
	}
	config.Scopes = config.Scopes[:0]
	for scope := range unique {
		config.Scopes = append(config.Scopes, scope)
	}
	sort.Strings(config.Scopes)
	var err error
	config.AllowedDomains, err = normalizeAllowedDomains(config.AllowedDomains, "invalid_oidc_allowed_domain", "OIDC allowedDomains")
	if err != nil {
		return OIDCConfiguration{}, err
	}
	config.GroupsClaim = strings.TrimSpace(config.GroupsClaim)
	if config.GroupsClaim == "" {
		config.GroupsClaim = "groups"
	}
	if len(config.GroupsClaim) > 120 || strings.ContainsAny(config.GroupsClaim, "\r\n\t ") {
		return OIDCConfiguration{}, problem.New(400, "invalid_oidc_groups_claim", "OIDC groupsClaim is invalid.")
	}
	config.DefaultTenantRole = strings.ToLower(strings.TrimSpace(config.DefaultTenantRole))
	if config.DefaultTenantRole == "" {
		config.DefaultTenantRole = "member"
	}
	if !validTenantRole(config.DefaultTenantRole) {
		return OIDCConfiguration{}, problem.New(400, "invalid_oidc_default_role", "OIDC defaultTenantRole is invalid.")
	}
	return config, nil
}

func decodeOIDCConfiguration(raw map[string]any) (OIDCConfiguration, error) {
	encoded, err := json.Marshal(raw)
	if err != nil {
		return OIDCConfiguration{}, problem.Wrap(500, "oidc_configuration_invalid", "Stored OIDC configuration is invalid.", err)
	}
	var config OIDCConfiguration
	if err := json.Unmarshal(encoded, &config); err != nil {
		return OIDCConfiguration{}, problem.Wrap(500, "oidc_configuration_invalid", "Stored OIDC configuration is invalid.", err)
	}
	return normalizeOIDCConfiguration(config)
}

func normalizeMapping(input MappingInput) (MappingInput, error) {
	input.ExternalGroup = strings.TrimSpace(input.ExternalGroup)
	if input.ExternalGroup == "" || len(input.ExternalGroup) > 500 {
		return MappingInput{}, problem.New(400, "invalid_identity_group", "Identity Group name is required and must not exceed 500 characters.")
	}
	if input.TenantRole != nil {
		role := strings.ToLower(strings.TrimSpace(*input.TenantRole))
		if !validTenantRole(role) {
			return MappingInput{}, problem.New(400, "invalid_identity_group_tenant_role", "Identity Group Tenant role is invalid.")
		}
		input.TenantRole = &role
	}
	if input.OrganizationID == nil && input.OrganizationRole != nil || input.OrganizationID != nil && input.OrganizationRole == nil {
		return MappingInput{}, problem.New(400, "invalid_identity_group_organization_role", "organizationId and organizationRole must be provided together.")
	}
	if input.OrganizationRole != nil {
		role := strings.ToLower(strings.TrimSpace(*input.OrganizationRole))
		if !validOrganizationRole(role) {
			return MappingInput{}, problem.New(400, "invalid_identity_group_organization_role", "Identity Group Organization role is invalid.")
		}
		input.OrganizationRole = &role
	}
	if input.TenantRole == nil && input.OrganizationID == nil {
		return MappingInput{}, problem.New(400, "empty_identity_group_mapping", "Identity Group mapping must grant a Tenant or Organization role.")
	}
	return input, nil
}

func domainAllowed(email string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	parts := strings.Split(strings.ToLower(strings.TrimSpace(email)), "@")
	if len(parts) != 2 {
		return false
	}
	for _, domain := range allowed {
		if parts[1] == domain {
			return true
		}
	}
	return false
}

func claimStrings(value any) []string {
	unique := map[string]struct{}{}
	switch typed := value.(type) {
	case string:
		if value := strings.TrimSpace(typed); value != "" {
			unique[value] = struct{}{}
		}
	case []any:
		for _, item := range typed {
			if value, ok := item.(string); ok {
				if value = strings.TrimSpace(value); value != "" {
					unique[value] = struct{}{}
				}
			}
		}
	case []string:
		for _, value := range typed {
			if value = strings.TrimSpace(value); value != "" {
				unique[value] = struct{}{}
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

func sanitizeClaims(claims map[string]any) map[string]any {
	result := make(map[string]any, len(claims))
	for key, value := range claims {
		normalized := strings.ToLower(strings.TrimSpace(key))
		if strings.Contains(normalized, "token") || strings.Contains(normalized, "secret") {
			continue
		}
		result[key] = value
	}
	return result
}

func normalizeReturnTo(value string) string {
	value = strings.TrimSpace(value)
	parsed, err := url.Parse(value)
	if err != nil || value == "" || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") || parsed.IsAbs() || parsed.Host != "" {
		return "/"
	}
	if len(value) > 1000 {
		return "/"
	}
	return value
}

func connectionAAD(tenantID, connectionID uuid.UUID, kind string) []byte {
	return []byte(strings.Join([]string{"synara-identity-connection-v1", tenantID.String(), connectionID.String(), kind}, "\x00"))
}

func attemptAAD(tenantID, attemptID, connectionID uuid.UUID) []byte {
	return []byte(strings.Join([]string{"synara-identity-attempt-v1", tenantID.String(), attemptID.String(), connectionID.String()}, "\x00"))
}

func toConnection(model persistence.IdentityConnection) Connection {
	return Connection{ID: model.ID, TenantID: model.TenantID, Kind: model.Kind, Name: model.Name, Status: model.Status, Issuer: model.Issuer, ClientID: model.ClientID, Configuration: model.Configuration, CreatedBy: model.CreatedBy, UpdatedBy: model.UpdatedBy, CreatedAt: model.CreatedAt, UpdatedAt: model.UpdatedAt}
}

func toMapping(model persistence.IdentityGroupMapping) Mapping {
	return Mapping{ID: model.ID, ExternalGroup: model.ExternalGroup, TenantRole: model.TenantRole, OrganizationID: model.OrganizationID, OrganizationRole: model.OrganizationRole}
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

var tenantRoles = map[string]int{"member": 1, "auditor": 2, "billing_admin": 3, "security_admin": 4, "admin": 5, "owner": 6}
var organizationRoles = map[string]int{"viewer": 1, "member": 2, "agent_operator": 3, "admin": 4, "owner": 5}

func validTenantRole(role string) bool       { _, valid := tenantRoles[role]; return valid }
func validOrganizationRole(role string) bool { _, valid := organizationRoles[role]; return valid }

func strongerTenantRole(left, right string) string {
	if tenantRoles[left] >= tenantRoles[right] {
		return left
	}
	return right
}

func strongerOrganizationRole(left, right string) string {
	if organizationRoles[left] >= organizationRoles[right] {
		return left
	}
	return right
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
