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

	"github.com/synara-ai/synara/services/control-plane/internal/audit"
	"github.com/synara-ai/synara/services/control-plane/internal/authorization"
	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	credentialkms "github.com/synara-ai/synara/services/control-plane/internal/kms"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/validation"
)

const maxCredentialPayloadBytes = 64 << 10

type Credential struct {
	ID             uuid.UUID  `json:"id"`
	TenantID       uuid.UUID  `json:"tenantId"`
	OrganizationID *uuid.UUID `json:"organizationId"`
	Name           string     `json:"name"`
	Purpose        string     `json:"purpose"`
	Provider       string     `json:"provider"`
	CredentialType string     `json:"credentialType"`
	KMSProvider    string     `json:"kmsProvider"`
	KMSKeyID       string     `json:"kmsKeyId"`
	Version        int        `json:"version"`
	CreatedBy      uuid.UUID  `json:"createdBy"`
	UpdatedBy      uuid.UUID  `json:"updatedBy"`
	CreatedAt      time.Time  `json:"createdAt"`
	UpdatedAt      time.Time  `json:"updatedAt"`
	ExpiresAt      *time.Time `json:"expiresAt"`
	RevokedAt      *time.Time `json:"revokedAt"`
}

type CreateInput struct {
	OrganizationID *uuid.UUID     `json:"organizationId"`
	Name           string         `json:"name"`
	Purpose        string         `json:"purpose"`
	Provider       string         `json:"provider"`
	CredentialType string         `json:"credentialType"`
	Payload        map[string]any `json:"payload"`
	ExpiresAt      *time.Time     `json:"expiresAt"`
}

type RotateInput struct {
	ExpectedVersion int            `json:"expectedVersion"`
	Payload         map[string]any `json:"payload"`
	ExpiresAt       *time.Time     `json:"expiresAt"`
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
	id := uuid.New()
	envelope, err := s.cipher.Encrypt(ctx, payload, credentialAAD(tenantID, id, normalized.Purpose, normalized.Provider, normalized.CredentialType, 1))
	if err != nil {
		return Credential{}, problem.Wrap(503, "credential_encryption_failed", "Credential could not be encrypted.", err)
	}
	model := persistence.ProviderCredential{
		ID: id, TenantID: tenantID, OrganizationID: normalized.OrganizationID, Name: normalized.Name,
		Purpose: normalized.Purpose, Provider: normalized.Provider, CredentialType: normalized.CredentialType,
		EncryptedPayload: envelope.EncryptedPayload, EncryptedDataKey: envelope.EncryptedDataKey,
		KMSProvider: envelope.KMSProvider, KMSKeyID: envelope.KMSKeyID, Version: 1,
		CreatedBy: principal.UserID, UpdatedBy: principal.UserID, ExpiresAt: normalized.ExpiresAt,
	}
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
	if purpose == PurposeGit {
		currentPayload, resolveErr := s.resolveModel(ctx, current)
		if resolveErr != nil {
			return Credential{}, resolveErr
		}
		currentGit, currentErr := decodeGitHTTPSPayload(currentPayload)
		nextGit, nextErr := decodeGitHTTPSPayload(normalizedPayload)
		if currentErr != nil || nextErr != nil || currentGit.Host != nextGit.Host {
			return Credential{}, problem.New(409, "git_credential_host_immutable", "Git Credential host cannot change during rotation.")
		}
	}
	if input.ExpiresAt != nil && !input.ExpiresAt.After(s.now()) {
		return Credential{}, problem.New(400, "invalid_credential_expiry", "Credential expiry must be in the future.")
	}
	nextVersion := current.Version + 1
	envelope, err := s.cipher.Encrypt(ctx, payload, credentialAAD(tenantID, credentialID, purpose, current.Provider, current.CredentialType, nextVersion))
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
				"version": nextVersion, "updated_by": principal.UserID, "expires_at": expiresAt,
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
	plaintext, err := s.cipher.Decrypt(ctx, credentialkms.Envelope{
		EncryptedPayload: model.EncryptedPayload, EncryptedDataKey: model.EncryptedDataKey,
		KMSProvider: model.KMSProvider, KMSKeyID: model.KMSKeyID,
	}, credentialAAD(model.TenantID, model.ID, purpose, model.Provider, model.CredentialType, model.Version))
	if err != nil {
		return nil, problem.Wrap(503, "credential_decryption_failed", "Credential could not be decrypted.", err)
	}
	defer zero(plaintext)
	var payload map[string]any
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		return nil, problem.Wrap(500, "credential_payload_invalid", "Credential payload is invalid.", err)
	}
	if purpose == PurposeGit {
		normalized, validationErr := normalizeGitHTTPSPayload(payload)
		if validationErr != nil || model.Provider != GitProvider || model.CredentialType != GitHTTPSCredentialType {
			return nil, problem.New(500, "credential_payload_invalid", "Git Credential payload is invalid.")
		}
		return map[string]any{"host": normalized.Host, "username": normalized.Username, "token": normalized.Token}, nil
	}
	return payload, nil
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
	_, payload, err := normalizeCredentialPayload(input.Purpose, input.Provider, input.CredentialType, input.Payload)
	return input, payload, err
}

func credentialAAD(tenantID, credentialID uuid.UUID, purpose, provider, credentialType string, version int) []byte {
	if purpose == PurposeProvider {
		return []byte(strings.Join([]string{"synara-credential-v1", tenantID.String(), credentialID.String(), provider, credentialType, strconv.Itoa(version)}, "\x00"))
	}
	return []byte(strings.Join([]string{"synara-credential-v2", tenantID.String(), credentialID.String(), purpose, provider, credentialType, strconv.Itoa(version)}, "\x00"))
}

func credentialAuditMetadata(model persistence.ProviderCredential) map[string]any {
	return map[string]any{"name": model.Name, "purpose": model.Purpose, "provider": model.Provider, "credentialType": model.CredentialType, "version": model.Version}
}

func credentialResourceType(purpose string) string {
	if purpose == PurposeGit {
		return "git_credential"
	}
	return "provider_credential"
}

func toCredential(model persistence.ProviderCredential) Credential {
	return Credential{
		ID: model.ID, TenantID: model.TenantID, OrganizationID: model.OrganizationID,
		Name: model.Name, Purpose: model.Purpose, Provider: model.Provider, CredentialType: model.CredentialType,
		KMSProvider: model.KMSProvider, KMSKeyID: model.KMSKeyID, Version: model.Version,
		CreatedBy: model.CreatedBy, UpdatedBy: model.UpdatedBy, CreatedAt: model.CreatedAt,
		UpdatedAt: model.UpdatedAt, ExpiresAt: model.ExpiresAt, RevokedAt: model.RevokedAt,
	}
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
