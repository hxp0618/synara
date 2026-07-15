package executions

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

type CredentialGrantDescriptor struct {
	GrantID        uuid.UUID `json:"grantId"`
	BindingKind    string    `json:"bindingKind"`
	Purpose        string    `json:"purpose"`
	Provider       string    `json:"provider"`
	CredentialType string    `json:"credentialType"`
	Selector       string    `json:"selector"`
}

// bindExecutionCredentialGrants snapshots every active Project Workspace
// Credential Binding into the newly claimed Execution generation. Infrastructure
// worker_image_pull Bindings intentionally remain outside the Workload and are
// resolved by the target provisioner rather than by agentd.
func bindExecutionCredentialGrants(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	now time.Time,
) ([]CredentialGrantDescriptor, error) {
	existing, err := loadExecutionCredentialGrantDescriptors(ctx, tx, execution)
	if err != nil || len(existing) > 0 {
		return existing, err
	}

	var owner struct {
		ProjectID      uuid.UUID `gorm:"column:project_id"`
		OrganizationID uuid.UUID `gorm:"column:organization_id"`
		RepositoryURL  *string   `gorm:"column:repository_url"`
	}
	err = tx.WithContext(ctx).Table("agent_sessions AS session").
		Select("session.project_id, session.organization_id, project.repository_url").
		Joins("JOIN projects AS project ON project.tenant_id = session.tenant_id AND project.id = session.project_id").
		Where("session.tenant_id = ? AND session.id = ?", execution.TenantID, execution.SessionID).
		Take(&owner).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, problem.New(409, "credential_grant_owner_unavailable", "The Execution Project is unavailable.")
	}
	if err != nil {
		return nil, problem.Wrap(500, "credential_grant_owner_load_failed", "Failed to load the Execution Project for Credential Grants.", err)
	}

	var bindings []persistence.CredentialBinding
	err = persistence.WithLocking(tx.WithContext(ctx), "SHARE", "").
		Where("tenant_id = ? AND project_id = ? AND disabled_at IS NULL", execution.TenantID, owner.ProjectID).
		Order("binding_kind, selector_value, id").
		Find(&bindings).Error
	if err != nil {
		return nil, problem.Wrap(500, "credential_bindings_load_failed", "Failed to load Project Credential Bindings for the Execution.", err)
	}

	descriptors := make([]CredentialGrantDescriptor, 0, len(bindings))
	for _, binding := range bindings {
		if binding.BindingKind == "worker_image_pull" {
			continue
		}
		if (binding.BindingKind == "git_fetch" || binding.BindingKind == "git_push") &&
			(owner.RepositoryURL == nil || binding.SelectorValue != *owner.RepositoryURL) {
			return nil, problem.New(409, "credential_grant_selector_mismatch", "A Git Credential Binding no longer matches the Project repository.")
		}
		var credential persistence.ProviderCredential
		err := persistence.WithLocking(tx.WithContext(ctx), "SHARE", "").
			Where("tenant_id = ? AND id = ?", execution.TenantID, binding.CredentialID).
			Take(&credential).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, problem.New(409, "credential_grant_credential_unavailable", "A bound Workspace Credential is unavailable.")
		}
		if err != nil {
			return nil, problem.Wrap(500, "credential_grant_credential_load_failed", "Failed to load a bound Workspace Credential.", err)
		}
		if credential.RevokedAt != nil ||
			(credential.ExpiresAt != nil && !credential.ExpiresAt.After(now)) {
			return nil, problem.New(409, "credential_grant_credential_unavailable", "A bound Workspace Credential is revoked or expired.")
		}
		if credential.Scope != "organization" && credential.Scope != "tenant" {
			return nil, problem.New(409, "credential_grant_scope_mismatch", "Workspace Credential Grants require Organization or Tenant scope.")
		}
		if credential.Scope == "organization" &&
			(credential.OrganizationID == nil || *credential.OrganizationID != owner.OrganizationID) {
			return nil, problem.New(409, "credential_grant_scope_mismatch", "A bound Workspace Credential no longer matches the Execution Organization.")
		}
		if !credentialBindingKindMatchesPurpose(binding.BindingKind, credential.Purpose) {
			return nil, problem.New(409, "credential_grant_kind_mismatch", "A Credential Binding no longer matches its Credential purpose.")
		}

		grant := persistence.ExecutionCredentialGrant{
			ID: uuid.New(), TenantID: execution.TenantID, ExecutionID: execution.ID,
			Generation: execution.Generation, BindingID: binding.ID,
			CredentialID: credential.ID, CredentialVersion: credential.Version, CreatedAt: now,
		}
		if err := tx.WithContext(ctx).Create(&grant).Error; err != nil {
			return nil, problem.Wrap(409, "credential_grant_create_failed", "Failed to snapshot a Workspace Credential Binding for this Execution generation.", err)
		}
		descriptors = append(descriptors, credentialGrantDescriptor(grant, binding, credential))
	}
	return descriptors, nil
}

func loadExecutionCredentialGrantDescriptors(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
) ([]CredentialGrantDescriptor, error) {
	var rows []struct {
		GrantID        uuid.UUID `gorm:"column:grant_id"`
		BindingKind    string    `gorm:"column:binding_kind"`
		Selector       string    `gorm:"column:selector_value"`
		Purpose        string    `gorm:"column:purpose"`
		Provider       string    `gorm:"column:provider"`
		CredentialType string    `gorm:"column:credential_type"`
	}
	err := tx.WithContext(ctx).Table("execution_credential_grants AS execution_grant").
		Select(`execution_grant.id AS grant_id, binding.binding_kind, binding.selector_value,
			credential.purpose, credential.provider, credential.credential_type`).
		Joins("JOIN credential_bindings AS binding ON binding.tenant_id = execution_grant.tenant_id AND binding.id = execution_grant.binding_id").
		Joins("JOIN provider_credentials AS credential ON credential.tenant_id = execution_grant.tenant_id AND credential.id = execution_grant.credential_id").
		Where("execution_grant.tenant_id = ? AND execution_grant.execution_id = ? AND execution_grant.generation = ?", execution.TenantID, execution.ID, execution.Generation).
		Order("binding.binding_kind, binding.selector_value, execution_grant.id").
		Find(&rows).Error
	if err != nil {
		return nil, problem.Wrap(500, "credential_grants_load_failed", "Failed to load Execution Credential Grant descriptors.", err)
	}
	descriptors := make([]CredentialGrantDescriptor, 0, len(rows))
	for _, row := range rows {
		descriptors = append(descriptors, CredentialGrantDescriptor{
			GrantID: row.GrantID, BindingKind: row.BindingKind, Purpose: row.Purpose,
			Provider: row.Provider, CredentialType: row.CredentialType, Selector: row.Selector,
		})
	}
	return descriptors, nil
}

func credentialGrantDescriptor(
	grant persistence.ExecutionCredentialGrant,
	binding persistence.CredentialBinding,
	credential persistence.ProviderCredential,
) CredentialGrantDescriptor {
	return CredentialGrantDescriptor{
		GrantID: grant.ID, BindingKind: binding.BindingKind, Purpose: credential.Purpose,
		Provider: credential.Provider, CredentialType: credential.CredentialType,
		Selector: binding.SelectorValue,
	}
}

func credentialBindingKindMatchesPurpose(kind, purpose string) bool {
	switch purpose {
	case "git":
		return kind == "git_fetch" || kind == "git_push"
	case "registry":
		return kind == "registry_pull" || kind == "registry_push" || kind == "worker_image_pull"
	case "package":
		return kind == "package_read" || kind == "package_publish"
	default:
		return false
	}
}
