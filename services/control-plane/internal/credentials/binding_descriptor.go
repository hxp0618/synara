package credentials

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

type BindingDescriptor struct {
	ID             uuid.UUID
	TenantID       uuid.UUID
	OrganizationID *uuid.UUID
	Scope          string
	Purpose        string
	Provider       string
	CredentialType string
	Version        int
	Selector       string
	EndpointHost   string
	EndpointPort   int
	EndpointUser   string
}

// LoadBindingDescriptor decrypts only long enough to derive the normalized,
// non-secret selector used by an immutable Workspace Credential Binding. The
// caller must apply user authorization and recheck the returned version while
// locking the Credential in its Binding transaction.
func (s *Service) LoadBindingDescriptor(
	ctx context.Context,
	tenantID, credentialID uuid.UUID,
) (BindingDescriptor, error) {
	var model persistence.ProviderCredential
	err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND id = ?", tenantID, credentialID).
		Take(&model).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return BindingDescriptor{}, problem.New(404, "credential_not_found", "Credential not found.")
	}
	if err != nil {
		return BindingDescriptor{}, problem.Wrap(500, "credential_load_failed", "Credential could not be loaded.", err)
	}
	if model.Purpose == PurposeProvider {
		return BindingDescriptor{}, problem.New(409, "credential_binding_kind_mismatch", "Provider Credentials cannot be bound to Workspace stages.")
	}
	payload, err := s.resolveModel(ctx, model)
	if err != nil {
		return BindingDescriptor{}, err
	}
	selector, err := credentialPayloadIdentity(model.Purpose, model.Provider, model.CredentialType, payload)
	if err != nil || selector == "" {
		return BindingDescriptor{}, problem.New(500, "credential_payload_invalid", "Credential selector metadata is invalid.")
	}
	descriptor := BindingDescriptor{
		ID: model.ID, TenantID: model.TenantID, OrganizationID: model.OrganizationID,
		Scope: model.Scope, Purpose: model.Purpose, Provider: model.Provider,
		CredentialType: model.CredentialType, Version: model.Version, Selector: selector,
	}
	if model.Purpose == PurposeGit {
		switch model.CredentialType {
		case GitHTTPSCredentialType:
			value, decodeErr := decodeGitHTTPSPayload(payload)
			if decodeErr != nil {
				return BindingDescriptor{}, problem.New(500, "credential_payload_invalid", "Git Credential payload is invalid.")
			}
			descriptor.EndpointHost, descriptor.EndpointPort, descriptor.EndpointUser = value.Host, 443, value.Username
		case GitSSHCredentialType:
			value, decodeErr := decodeGitSSHPayload(payload)
			if decodeErr != nil {
				return BindingDescriptor{}, problem.New(500, "credential_payload_invalid", "Git Credential payload is invalid.")
			}
			descriptor.EndpointHost, descriptor.EndpointPort, descriptor.EndpointUser = value.Host, value.Port, value.Username
		}
	}
	return descriptor, nil
}
