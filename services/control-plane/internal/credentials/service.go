package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/credentialscope"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	credentialkms "github.com/synara-ai/synara/services/control-plane/internal/kms"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/validation"
)

const maxCredentialPayloadBytes = 64 << 10

type Credential struct {
	ID                     uuid.UUID  `json:"id"`
	TenantID               uuid.UUID  `json:"tenantId"`
	OrganizationID         *uuid.UUID `json:"organizationId"`
	Scope                  string     `json:"scope"`
	ScopeUserID            *uuid.UUID `json:"scopeUserId"`
	SelectorOrganizationID *uuid.UUID `json:"selectorOrganizationId"`
	SelectorModel          *string    `json:"selectorModel"`
	AutoSelectEnabled      bool       `json:"autoSelectEnabled"`
	Name                   string     `json:"name"`
	Purpose                string     `json:"purpose"`
	Provider               string     `json:"provider"`
	CredentialType         string     `json:"credentialType"`
	KMSProvider            string     `json:"kmsProvider"`
	KMSKeyID               string     `json:"kmsKeyId"`
	Version                int        `json:"version"`
	CreatedBy              uuid.UUID  `json:"createdBy"`
	UpdatedBy              uuid.UUID  `json:"updatedBy"`
	CreatedAt              time.Time  `json:"createdAt"`
	UpdatedAt              time.Time  `json:"updatedAt"`
	ExpiresAt              *time.Time `json:"expiresAt"`
	RevokedAt              *time.Time `json:"revokedAt"`
}

type CreateInput struct {
	OrganizationID         *uuid.UUID     `json:"organizationId"`
	Scope                  string         `json:"scope"`
	ScopeUserID            *uuid.UUID     `json:"scopeUserId"`
	SelectorOrganizationID *uuid.UUID     `json:"selectorOrganizationId"`
	SelectorModel          *string        `json:"selectorModel"`
	AutoSelectEnabled      bool           `json:"autoSelectEnabled"`
	Name                   string         `json:"name"`
	Purpose                string         `json:"purpose"`
	Provider               string         `json:"provider"`
	CredentialType         string         `json:"credentialType"`
	Payload                map[string]any `json:"payload"`
	ExpiresAt              *time.Time     `json:"expiresAt"`
}

type RotateInput struct {
	ExpectedVersion int            `json:"expectedVersion"`
	Payload         map[string]any `json:"payload"`
	ExpiresAt       *time.Time     `json:"expiresAt"`
}

type SetAutoSelectInput struct {
	Enabled bool `json:"enabled"`
}

type ScopePolicy struct {
	TenantID                     uuid.UUID  `json:"tenantId"`
	PlatformCredentialsEnabled   bool       `json:"platformCredentialsEnabled"`
	PlatformCredentialAutoSelect bool       `json:"platformCredentialAutoSelect"`
	UpdatedBy                    *uuid.UUID `json:"updatedBy"`
	CreatedAt                    *time.Time `json:"createdAt"`
	UpdatedAt                    *time.Time `json:"updatedAt"`
}

type UpdateScopePolicyInput struct {
	PlatformCredentialsEnabled   bool `json:"platformCredentialsEnabled"`
	PlatformCredentialAutoSelect bool `json:"platformCredentialAutoSelect"`
}

type Service struct {
	db         *gorm.DB
	authorizer *authorization.Authorizer
	cipher     *credentialkms.EnvelopeCipher
	now        func() time.Time
}

func NewService(db *gorm.DB, cipher *credentialkms.EnvelopeCipher) *Service {
	return &Service{db: db, authorizer: authorization.NewAuthorizer(db), cipher: cipher, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) Create(ctx context.Context, principal identity.Principal, tenantID uuid.UUID, input CreateInput, requestID, ipAddress string) (Credential, error) {
	if err := requireActiveTenant(principal, tenantID); err != nil {
		return Credential{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.CredentialsManage); err != nil {
		return Credential{}, err
	}
	if s.cipher == nil {
		return Credential{}, problem.New(503, "credential_kms_unavailable", "Credential KMS is not configured.")
	}
	normalized, payload, err := normalizeCreate(input, s.now())
	if err != nil {
		return Credential{}, err
	}
	if normalized.OrganizationID != nil {
		if _, err := s.authorizer.RequireOrganization(ctx, principal.UserID, tenantID, *normalized.OrganizationID, authorization.OrganizationRead); err != nil {
			return Credential{}, err
		}
	}
	if normalized.ScopeUserID != nil {
		if err := s.requireActiveTenantUser(ctx, tenantID, *normalized.ScopeUserID); err != nil {
			return Credential{}, err
		}
	}
	if normalized.SelectorOrganizationID != nil {
		if _, err := s.authorizer.RequireOrganization(ctx, principal.UserID, tenantID, *normalized.SelectorOrganizationID, authorization.OrganizationRead); err != nil {
			return Credential{}, err
		}
	}
	if normalized.Scope == credentialscope.ScopePlatform {
		access, err := credentialscope.LoadPlatformAccess(ctx, s.db, tenantID)
		if err != nil {
			return Credential{}, err
		}
		if !access.Enabled {
			return Credential{}, problem.New(403, "platform_credential_not_entitled", "Platform Credentials require an enterprise entitlement and explicit Tenant policy.")
		}
		if normalized.AutoSelectEnabled && !access.AutoSelect {
			return Credential{}, problem.New(403, "platform_credential_auto_select_forbidden", "Platform Credential automatic selection requires explicit Tenant policy.")
		}
	}
	id := uuid.New()
	model := persistence.ProviderCredential{
		ID: id, TenantID: tenantID, OrganizationID: normalized.OrganizationID,
		Scope: normalized.Scope, ScopeUserID: normalized.ScopeUserID,
		SelectorOrganizationID: normalized.SelectorOrganizationID,
		SelectorModel:          normalized.SelectorModel, AutoSelectEnabled: normalized.AutoSelectEnabled,
		Name: normalized.Name, Purpose: normalized.Purpose, Provider: normalized.Provider,
		CredentialType: normalized.CredentialType, AADVersion: 3, Version: 1,
		CreatedBy: principal.UserID, UpdatedBy: principal.UserID, ExpiresAt: normalized.ExpiresAt,
	}
	aad, err := credentialAAD(model)
	if err != nil {
		return Credential{}, err
	}
	envelope, err := s.cipher.Encrypt(ctx, payload, aad)
	if err != nil {
		return Credential{}, problem.Wrap(503, "credential_encryption_failed", "Credential could not be encrypted.", err)
	}
	model.EncryptedPayload = envelope.EncryptedPayload
	model.EncryptedDataKey = envelope.EncryptedDataKey
	model.KMSProvider = envelope.KMSProvider
	model.KMSKeyID = envelope.KMSKeyID
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		if err := tx.Create(&model).Error; err != nil {
			return problem.Wrap(409, "credential_create_rejected", "Credential creation was rejected.", err)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "credential.created", ResourceType: credentialResourceType(model.Purpose), ResourceID: &id,
			OrganizationID: normalized.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: credentialAuditMetadata(model),
		})
	})
	if err != nil {
		return Credential{}, err
	}
	return toCredential(model), nil
}

func (s *Service) List(ctx context.Context, principal identity.Principal, tenantID uuid.UUID) ([]Credential, error) {
	if err := requireActiveTenant(principal, tenantID); err != nil {
		return nil, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.CredentialsRead); err != nil {
		return nil, err
	}
	models := make([]persistence.ProviderCredential, 0)
	if err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Order("revoked_at IS NOT NULL, LOWER(name), id").Find(&models).Error; err != nil {
		return nil, problem.Wrap(500, "credentials_load_failed", "Credentials could not be loaded.", err)
	}
	items := make([]Credential, 0, len(models))
	for _, model := range models {
		items = append(items, toCredential(model))
	}
	return items, nil
}

func (s *Service) Rotate(ctx context.Context, principal identity.Principal, tenantID, credentialID uuid.UUID, input RotateInput, requestID, ipAddress string) (Credential, error) {
	if err := requireActiveTenant(principal, tenantID); err != nil {
		return Credential{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.CredentialsManage); err != nil {
		return Credential{}, err
	}
	if s.cipher == nil {
		return Credential{}, problem.New(503, "credential_kms_unavailable", "Credential KMS is not configured.")
	}
	var current persistence.ProviderCredential
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, credentialID).Take(&current).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return Credential{}, problem.New(404, "credential_not_found", "Credential not found.")
	} else if err != nil {
		return Credential{}, problem.Wrap(500, "credential_load_failed", "Credential could not be loaded.", err)
	}
	if current.RevokedAt != nil {
		return Credential{}, problem.New(409, "credential_revoked", "Provider Credential is revoked.")
	}
	if input.ExpectedVersion != current.Version {
		return Credential{}, problem.New(409, "credential_version_conflict", "Provider Credential version has changed.")
	}
	purpose, err := normalizePurpose(current.Purpose)
	if err != nil {
		return Credential{}, problem.New(500, "credential_purpose_invalid", "Credential purpose is invalid.")
	}
	normalizedPayload, payload, err := normalizeCredentialPayload(purpose, current.Provider, current.CredentialType, input.Payload)
	if err != nil {
		return Credential{}, err
	}
	if purpose != PurposeProvider {
		currentPayload, resolveErr := s.resolveModel(ctx, current)
		if resolveErr != nil {
			return Credential{}, resolveErr
		}
		currentIdentity, currentErr := credentialPayloadIdentity(purpose, current.Provider, current.CredentialType, currentPayload)
		nextIdentity, nextErr := credentialPayloadIdentity(purpose, current.Provider, current.CredentialType, normalizedPayload)
		if currentErr != nil || nextErr != nil || currentIdentity != nextIdentity {
			if purpose == PurposeGit && current.CredentialType == GitHTTPSCredentialType {
				return Credential{}, problem.New(409, "git_credential_host_immutable", "Git Credential host cannot change during rotation.")
			}
			return Credential{}, problem.New(409, "credential_selector_immutable", "Credential selector identity cannot change during rotation.")
		}
	}
	if input.ExpiresAt != nil && !input.ExpiresAt.After(s.now()) {
		return Credential{}, problem.New(400, "invalid_credential_expiry", "Credential expiry must be in the future.")
	}
	nextVersion := current.Version + 1
	next := current
	next.AADVersion = 3
	next.Version = nextVersion
	aad, err := credentialAAD(next)
	if err != nil {
		return Credential{}, err
	}
	envelope, err := s.cipher.Encrypt(ctx, payload, aad)
	if err != nil {
		return Credential{}, problem.Wrap(503, "credential_encryption_failed", "Credential could not be encrypted.", err)
	}
	err = persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		expiresAt := any(gorm.Expr("NULL"))
		if input.ExpiresAt != nil {
			expiresAt = *input.ExpiresAt
		}
		result := tx.Model(&persistence.ProviderCredential{}).
			Where("tenant_id = ? AND id = ? AND version = ? AND revoked_at IS NULL", tenantID, credentialID, input.ExpectedVersion).
			Updates(map[string]any{
				"encrypted_payload": envelope.EncryptedPayload, "encrypted_data_key": envelope.EncryptedDataKey,
				"kms_provider": envelope.KMSProvider, "kms_key_id": envelope.KMSKeyID,
				"aad_version": 3, "version": nextVersion, "updated_by": principal.UserID, "expires_at": expiresAt,
			})
		if result.Error != nil || result.RowsAffected != 1 {
			return problem.Wrap(409, "credential_version_conflict", "Provider Credential version has changed.", result.Error)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "credential.rotated", ResourceType: credentialResourceType(purpose), ResourceID: &credentialID,
			OrganizationID: current.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{"purpose": purpose, "provider": current.Provider, "credentialType": current.CredentialType, "version": nextVersion},
		})
	})
	if err != nil {
		return Credential{}, err
	}
	current = persistence.ProviderCredential{}
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, credentialID).Take(&current).Error; err != nil {
		return Credential{}, err
	}
	return toCredential(current), nil
}

func (s *Service) Revoke(ctx context.Context, principal identity.Principal, tenantID, credentialID uuid.UUID, requestID, ipAddress string) error {
	if err := requireActiveTenant(principal, tenantID); err != nil {
		return err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.CredentialsManage); err != nil {
		return err
	}
	now := s.now()
	return persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		var current persistence.ProviderCredential
		if err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND id = ?", tenantID, credentialID).
			Take(&current).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "credential_not_found", "Credential not found.")
		} else if err != nil {
			return err
		}
		if current.RevokedAt != nil {
			return nil
		}
		if err := tx.Model(&current).Updates(map[string]any{"revoked_at": now, "revoked_by": principal.UserID, "updated_by": principal.UserID}).Error; err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "credential.revoked", ResourceType: credentialResourceType(current.Purpose), ResourceID: &credentialID,
			OrganizationID: current.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: credentialAuditMetadata(current),
		})
	})
}

func (s *Service) SetAutoSelect(
	ctx context.Context,
	principal identity.Principal,
	tenantID, credentialID uuid.UUID,
	input SetAutoSelectInput,
	requestID, ipAddress string,
) (Credential, error) {
	if err := requireActiveTenant(principal, tenantID); err != nil {
		return Credential{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.CredentialsManage); err != nil {
		return Credential{}, err
	}
	var current persistence.ProviderCredential
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		err := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ? AND id = ?", tenantID, credentialID).
			Take(&current).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "credential_not_found", "Credential not found.")
		}
		if err != nil {
			return problem.Wrap(500, "credential_load_failed", "Credential could not be loaded.", err)
		}
		if current.Purpose != PurposeProvider {
			return problem.New(409, "credential_purpose_mismatch", "Only Provider Credentials support automatic selection.")
		}
		if current.RevokedAt != nil || (current.ExpiresAt != nil && !current.ExpiresAt.After(s.now())) {
			return problem.New(409, "credential_unavailable", "Credential is revoked or expired.")
		}
		if input.Enabled && current.Scope == credentialscope.ScopePlatform {
			access, accessErr := credentialscope.LoadPlatformAccess(ctx, tx, tenantID)
			if accessErr != nil {
				return accessErr
			}
			if !access.Enabled || !access.AutoSelect {
				return problem.New(403, "platform_credential_auto_select_forbidden", "Platform Credential automatic selection requires explicit Tenant policy.")
			}
		}
		if current.AutoSelectEnabled == input.Enabled {
			return nil
		}
		if err := tx.Model(&persistence.ProviderCredential{}).
			Where("tenant_id = ? AND id = ?", tenantID, credentialID).
			Updates(map[string]any{
				"auto_select_enabled": input.Enabled,
				"updated_by":          principal.UserID,
			}).Error; err != nil {
			return problem.Wrap(409, "credential_auto_select_update_rejected", "Credential automatic selection could not be updated.", err)
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "credential.auto_select.changed", ResourceType: credentialResourceType(current.Purpose), ResourceID: &credentialID,
			OrganizationID: current.OrganizationID, RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"purpose": current.Purpose, "provider": current.Provider, "scope": current.Scope,
				"autoSelectEnabled": input.Enabled,
			},
		}); err != nil {
			return err
		}
		current.AutoSelectEnabled = input.Enabled
		return nil
	})
	if err != nil {
		return Credential{}, err
	}
	if err := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, credentialID).Take(&current).Error; err != nil {
		return Credential{}, problem.Wrap(500, "credential_load_failed", "Credential could not be loaded.", err)
	}
	return toCredential(current), nil
}

func (s *Service) GetScopePolicy(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
) (ScopePolicy, error) {
	if err := requireActiveTenant(principal, tenantID); err != nil {
		return ScopePolicy{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.CredentialsRead); err != nil {
		return ScopePolicy{}, err
	}
	var model persistence.ProviderCredentialScopePolicy
	err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Take(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ScopePolicy{TenantID: tenantID}, nil
	}
	if err != nil {
		return ScopePolicy{}, problem.Wrap(500, "credential_scope_policy_load_failed", "Provider Credential scope policy could not be loaded.", err)
	}
	return toScopePolicy(model), nil
}

func (s *Service) UpdateScopePolicy(
	ctx context.Context,
	principal identity.Principal,
	tenantID uuid.UUID,
	input UpdateScopePolicyInput,
	requestID, ipAddress string,
) (ScopePolicy, error) {
	if err := requireActiveTenant(principal, tenantID); err != nil {
		return ScopePolicy{}, err
	}
	if _, err := s.authorizer.RequireTenant(ctx, principal.UserID, tenantID, authorization.CredentialsManage); err != nil {
		return ScopePolicy{}, err
	}
	if input.PlatformCredentialAutoSelect && !input.PlatformCredentialsEnabled {
		return ScopePolicy{}, problem.New(400, "invalid_credential_scope_policy", "Platform Credential automatic selection requires Platform Credentials to be enabled.")
	}

	var model persistence.ProviderCredentialScopePolicy
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		loadErr := persistence.WithLocking(tx.WithContext(ctx), "UPDATE", "").
			Where("tenant_id = ?", tenantID).
			Take(&model).Error
		if loadErr != nil && !errors.Is(loadErr, gorm.ErrRecordNotFound) {
			return problem.Wrap(500, "credential_scope_policy_load_failed", "Provider Credential scope policy could not be loaded.", loadErr)
		}
		if loadErr == nil &&
			model.PlatformCredentialsEnabled == input.PlatformCredentialsEnabled &&
			model.PlatformCredentialAutoSelect == input.PlatformCredentialAutoSelect {
			return nil
		}
		if input.PlatformCredentialsEnabled || input.PlatformCredentialAutoSelect {
			entitled, entitlementErr := credentialscope.LoadPlatformEntitlement(ctx, tx, tenantID)
			if entitlementErr != nil {
				return entitlementErr
			}
			if !entitled {
				return problem.New(403, "platform_credential_not_entitled", "Platform Credentials require an enterprise installation and Tenant entitlement.")
			}
		}
		now := s.now()
		model = persistence.ProviderCredentialScopePolicy{
			TenantID:                     tenantID,
			PlatformCredentialsEnabled:   input.PlatformCredentialsEnabled,
			PlatformCredentialAutoSelect: input.PlatformCredentialAutoSelect,
			UpdatedBy:                    principal.UserID, CreatedAt: now, UpdatedAt: now,
		}
		result := tx.WithContext(ctx).Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "tenant_id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"platform_credentials_enabled", "platform_credential_auto_select", "updated_by", "updated_at",
			}),
		}).Create(&model)
		if result.Error != nil {
			return problem.Wrap(409, "credential_scope_policy_update_rejected", "Provider Credential scope policy could not be updated.", result.Error)
		}
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: "user", ActorID: &principal.UserID,
			Action: "credential.scope_policy.updated", ResourceType: "provider_credential_scope_policy", ResourceID: &tenantID,
			RequestID: requestID, IPAddress: ipAddress,
			Metadata: map[string]any{
				"platformCredentialsEnabled":   input.PlatformCredentialsEnabled,
				"platformCredentialAutoSelect": input.PlatformCredentialAutoSelect,
			},
		})
	})
	if err != nil {
		return ScopePolicy{}, err
	}
	if err := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID).Take(&model).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return ScopePolicy{TenantID: tenantID}, nil
	} else if err != nil {
		return ScopePolicy{}, problem.Wrap(500, "credential_scope_policy_load_failed", "Provider Credential scope policy could not be loaded.", err)
	}
	return toScopePolicy(model), nil
}

func (s *Service) Resolve(ctx context.Context, tenantID, credentialID uuid.UUID) (map[string]any, error) {
	if s.cipher == nil {
		return nil, problem.New(503, "credential_kms_unavailable", "Credential KMS is not configured.")
	}
	var model persistence.ProviderCredential
	err := s.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, credentialID).Take(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, problem.New(404, "credential_not_found", "Credential not found.")
	}
	if err != nil {
		return nil, problem.Wrap(500, "credential_load_failed", "Credential could not be loaded.", err)
	}
	return s.resolveModel(ctx, model)
}

func (s *Service) resolveModel(ctx context.Context, model persistence.ProviderCredential) (map[string]any, error) {
	if model.RevokedAt != nil || (model.ExpiresAt != nil && !model.ExpiresAt.After(s.now())) {
		return nil, problem.New(409, "credential_unavailable", "Credential is revoked or expired.")
	}
	purpose, purposeErr := normalizePurpose(model.Purpose)
	if purposeErr != nil {
		return nil, problem.New(500, "credential_purpose_invalid", "Credential purpose is invalid.")
	}
	aad, err := credentialAAD(model)
	if err != nil {
		return nil, err
	}
	plaintext, err := s.cipher.Decrypt(ctx, credentialkms.Envelope{
		EncryptedPayload: model.EncryptedPayload, EncryptedDataKey: model.EncryptedDataKey,
		KMSProvider: model.KMSProvider, KMSKeyID: model.KMSKeyID,
	}, aad)
	if err != nil {
		return nil, problem.Wrap(503, "credential_decryption_failed", "Credential could not be decrypted.", err)
	}
	defer zero(plaintext)
	var payload map[string]any
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		return nil, problem.Wrap(500, "credential_payload_invalid", "Credential payload is invalid.", err)
	}
	normalized, _, validationErr := normalizeCredentialPayload(
		purpose,
		model.Provider,
		model.CredentialType,
		payload,
	)
	if validationErr != nil {
		return nil, problem.New(500, "credential_payload_invalid", "Credential payload is invalid.")
	}
	return normalized, nil
}

func normalizeCreate(input CreateInput, now time.Time) (CreateInput, []byte, error) {
	var err error
	input.Name, err = validation.Name(input.Name, "invalid_credential_name", "Credential name", 160)
	if err != nil {
		return CreateInput{}, nil, err
	}
	input.Purpose, err = normalizePurpose(input.Purpose)
	if err != nil {
		return CreateInput{}, nil, err
	}
	input.Provider, err = validation.Code(input.Provider, "", "invalid_credential_provider", "Credential provider")
	if err != nil {
		return CreateInput{}, nil, err
	}
	input.CredentialType, err = validation.Code(input.CredentialType, "", "invalid_credential_type", "Credential type")
	if err != nil {
		return CreateInput{}, nil, err
	}
	if input.ExpiresAt != nil && !input.ExpiresAt.After(now) {
		return CreateInput{}, nil, problem.New(400, "invalid_credential_expiry", "Credential expiry must be in the future.")
	}
	if err := normalizeScopeInput(&input); err != nil {
		return CreateInput{}, nil, err
	}
	_, payload, err := normalizeCredentialPayload(input.Purpose, input.Provider, input.CredentialType, input.Payload)
	return input, payload, err
}

func credentialAAD(model persistence.ProviderCredential) ([]byte, error) {
	switch model.AADVersion {
	case 1:
		if model.Purpose != PurposeProvider {
			return nil, problem.New(500, "credential_aad_version_invalid", "Credential AAD version is invalid.")
		}
		return []byte(strings.Join([]string{
			"synara-credential-v1", model.TenantID.String(), model.ID.String(), model.Provider,
			model.CredentialType, strconv.Itoa(model.Version),
		}, "\x00")), nil
	case 2:
		if model.Purpose != PurposeGit {
			return nil, problem.New(500, "credential_aad_version_invalid", "Credential AAD version is invalid.")
		}
		return []byte(strings.Join([]string{
			"synara-credential-v2", model.TenantID.String(), model.ID.String(), model.Purpose,
			model.Provider, model.CredentialType, strconv.Itoa(model.Version),
		}, "\x00")), nil
	case 3:
		return []byte(strings.Join([]string{
			"synara-credential-v3", model.TenantID.String(), model.ID.String(), model.Purpose,
			model.Provider, model.CredentialType, model.Scope, uuidValue(model.ScopeUserID),
			uuidValue(model.OrganizationID), uuidValue(model.SelectorOrganizationID),
			stringValue(model.SelectorModel), strconv.Itoa(model.Version),
		}, "\x00")), nil
	default:
		return nil, problem.New(500, "credential_aad_version_unsupported", "Credential AAD version is unsupported.")
	}
}

func credentialAuditMetadata(model persistence.ProviderCredential) map[string]any {
	return map[string]any{
		"purpose": model.Purpose, "provider": model.Provider,
		"credentialType": model.CredentialType, "scope": model.Scope,
		"scopeUserId": model.ScopeUserID, "selectorOrganizationId": model.SelectorOrganizationID,
		"autoSelectEnabled": model.AutoSelectEnabled, "version": model.Version,
	}
}

func credentialResourceType(purpose string) string {
	switch purpose {
	case PurposeGit:
		return "git_credential"
	case PurposeRegistry:
		return "registry_credential"
	case PurposePackage:
		return "package_credential"
	default:
		return "provider_credential"
	}
}

func toCredential(model persistence.ProviderCredential) Credential {
	return Credential{
		ID: model.ID, TenantID: model.TenantID, OrganizationID: model.OrganizationID,
		Scope: model.Scope, ScopeUserID: model.ScopeUserID,
		SelectorOrganizationID: model.SelectorOrganizationID,
		SelectorModel:          model.SelectorModel, AutoSelectEnabled: model.AutoSelectEnabled,
		Name: model.Name, Purpose: model.Purpose, Provider: model.Provider, CredentialType: model.CredentialType,
		KMSProvider: model.KMSProvider, KMSKeyID: model.KMSKeyID, Version: model.Version,
		CreatedBy: model.CreatedBy, UpdatedBy: model.UpdatedBy, CreatedAt: model.CreatedAt,
		UpdatedAt: model.UpdatedAt, ExpiresAt: model.ExpiresAt, RevokedAt: model.RevokedAt,
	}
}

func toScopePolicy(model persistence.ProviderCredentialScopePolicy) ScopePolicy {
	updatedBy := model.UpdatedBy
	createdAt := model.CreatedAt
	updatedAt := model.UpdatedAt
	return ScopePolicy{
		TenantID:                     model.TenantID,
		PlatformCredentialsEnabled:   model.PlatformCredentialsEnabled,
		PlatformCredentialAutoSelect: model.PlatformCredentialAutoSelect,
		UpdatedBy:                    &updatedBy, CreatedAt: &createdAt, UpdatedAt: &updatedAt,
	}
}

func normalizeScopeInput(input *CreateInput) error {
	input.Scope = strings.ToLower(strings.TrimSpace(input.Scope))
	if input.Scope == "" {
		if input.OrganizationID != nil {
			input.Scope = credentialscope.ScopeOrganization
		} else {
			input.Scope = credentialscope.ScopeTenant
		}
	}
	if input.SelectorModel != nil {
		value := strings.TrimSpace(*input.SelectorModel)
		if value == "" || len(value) > 200 || strings.ContainsAny(value, "\r\n\t") {
			return problem.New(400, "invalid_credential_selector_model", "Credential selector model is invalid.")
		}
		input.SelectorModel = &value
	}

	selectorsPresent := input.SelectorOrganizationID != nil || input.SelectorModel != nil
	if input.Purpose != PurposeProvider && input.AutoSelectEnabled {
		return problem.New(400, "invalid_credential_auto_select", "Workspace Credentials cannot be selected as Provider Credentials.")
	}
	switch input.Scope {
	case credentialscope.ScopeUser:
		if input.Purpose != PurposeProvider || input.ScopeUserID == nil || input.OrganizationID != nil || selectorsPresent {
			return problem.New(400, "invalid_credential_scope", "User Credential scope requires one User and no Organization or Tenant selectors.")
		}
	case credentialscope.ScopeOrganization:
		if input.OrganizationID == nil || input.ScopeUserID != nil || selectorsPresent {
			return problem.New(400, "invalid_credential_scope", "Organization Credential scope requires one Organization and no other selectors.")
		}
	case credentialscope.ScopeTenant:
		if input.OrganizationID != nil || input.ScopeUserID != nil {
			return problem.New(400, "invalid_credential_scope", "Tenant Credential scope cannot have a User or Organization owner.")
		}
		if input.Purpose != PurposeProvider && selectorsPresent {
			return problem.New(400, "invalid_credential_scope", "Workspace Tenant Credentials cannot have Organization or Model selectors.")
		}
	case credentialscope.ScopePlatform:
		if input.Purpose != PurposeProvider || input.OrganizationID != nil || input.ScopeUserID != nil || selectorsPresent {
			return problem.New(400, "invalid_credential_scope", "Platform Credential scope cannot have a User, Organization, or Tenant selector.")
		}
	default:
		return problem.New(400, "invalid_credential_scope", "Credential scope must be user, organization, tenant, or platform.")
	}
	return nil
}

func (s *Service) requireActiveTenantUser(ctx context.Context, tenantID, userID uuid.UUID) error {
	var count int64
	err := s.db.WithContext(ctx).Model(&persistence.TenantMembership{}).
		Where("tenant_id = ? AND user_id = ? AND status = ?", tenantID, userID, "active").
		Count(&count).Error
	if err != nil {
		return problem.Wrap(500, "credential_scope_user_lookup_failed", "Credential scope User could not be loaded.", err)
	}
	if count != 1 {
		return problem.New(404, "credential_scope_user_not_found", "Credential scope User not found.")
	}
	return nil
}

func uuidValue(value *uuid.UUID) string {
	if value == nil {
		return ""
	}
	return value.String()
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func requireActiveTenant(principal identity.Principal, tenantID uuid.UUID) error {
	if principal.ActiveTenantID == nil || *principal.ActiveTenantID != tenantID {
		return problem.New(404, "tenant_not_found", "Tenant not found.")
	}
	return nil
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
