package credentials

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/credentialscope"
	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/gitpolicy"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

type ResolvedWorkspaceCredential struct {
	GrantID        uuid.UUID      `json:"grantId"`
	BindingKind    string         `json:"bindingKind"`
	Purpose        string         `json:"purpose"`
	Provider       string         `json:"provider"`
	CredentialType string         `json:"credentialType"`
	Selector       string         `json:"selector"`
	Payload        map[string]any `json:"payload"`
}

// ResolveGrantForExecution resolves a Workspace Credential exclusively through
// the immutable Grant ID included in the current generation Workload. Neither
// the Credential ID nor its Version is accepted from agentd.
func (s *Service) ResolveGrantForExecution(
	ctx context.Context,
	executionService *executions.Service,
	worker persistence.WorkerInstance,
	executionID, grantID uuid.UUID,
	leaseInput executions.LeaseInput,
) (ResolvedWorkspaceCredential, error) {
	if s.cipher == nil {
		return ResolvedWorkspaceCredential{}, problem.New(503, "credential_kms_unavailable", "Credential KMS is not configured.")
	}
	var grant persistence.ExecutionCredentialGrant
	var binding persistence.CredentialBinding
	var credential persistence.ProviderCredential
	var repositoryURL *string
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		execution, err := executionService.AuthorizeLease(ctx, tx, worker, executionID, leaseInput)
		if err != nil {
			return err
		}
		err = persistence.WithLocking(tx.WithContext(ctx), "SHARE", "").
			Where(
				"tenant_id = ? AND execution_id = ? AND generation = ? AND id = ?",
				execution.TenantID, execution.ID, execution.Generation, grantID,
			).
			Take(&grant).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "credential_grant_not_found", "Workspace Credential Grant not found.")
		}
		if err != nil {
			return problem.Wrap(500, "credential_grant_load_failed", "Workspace Credential Grant could not be loaded.", err)
		}
		err = persistence.WithLocking(tx.WithContext(ctx), "SHARE", "").
			Where("tenant_id = ? AND id = ?", execution.TenantID, grant.BindingID).
			Take(&binding).Error
		if errors.Is(err, gorm.ErrRecordNotFound) ||
			(err == nil && (binding.DisabledAt != nil || binding.CredentialID != grant.CredentialID)) {
			return problem.New(409, "credential_grant_unavailable", "Workspace Credential Grant is no longer available.")
		}
		if err != nil {
			return problem.Wrap(500, "credential_binding_load_failed", "Workspace Credential Binding could not be loaded.", err)
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
			return problem.New(409, "credential_grant_owner_unavailable", "The Execution Project is unavailable.")
		}
		if err != nil {
			return problem.Wrap(500, "credential_grant_owner_load_failed", "The Execution Project could not be loaded.", err)
		}
		if binding.ProjectID == nil || *binding.ProjectID != owner.ProjectID || binding.ExecutionTargetID != nil ||
			binding.BindingKind == "worker_image_pull" {
			return problem.New(409, "credential_grant_owner_mismatch", "Workspace Credential Grant does not belong to the Execution Project.")
		}
		repositoryURL = owner.RepositoryURL

		err = persistence.WithLocking(tx.WithContext(ctx), "SHARE", "").
			Where("tenant_id = ? AND id = ?", execution.TenantID, grant.CredentialID).
			Take(&credential).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(409, "credential_grant_unavailable", "Workspace Credential Grant is no longer available.")
		}
		if err != nil {
			return problem.Wrap(500, "credential_load_failed", "Workspace Credential could not be loaded.", err)
		}
		if credential.Version != grant.CredentialVersion {
			return problem.New(409, "credential_grant_version_fenced", "Workspace Credential rotated after this Execution generation was claimed.")
		}
		if credential.RevokedAt != nil ||
			(credential.ExpiresAt != nil && !credential.ExpiresAt.After(s.now())) {
			return problem.New(409, "credential_grant_unavailable", "Workspace Credential is revoked or expired.")
		}
		if credential.Scope != credentialscope.ScopeOrganization &&
			credential.Scope != credentialscope.ScopeTenant {
			return problem.New(409, "credential_grant_scope_mismatch", "Workspace Credential Grant scope is invalid.")
		}
		if credential.Scope == credentialscope.ScopeOrganization &&
			(credential.OrganizationID == nil || *credential.OrganizationID != owner.OrganizationID) {
			return problem.New(409, "credential_grant_scope_mismatch", "Workspace Credential Grant no longer matches the Execution Organization.")
		}
		if !grantBindingKindMatchesPurpose(binding.BindingKind, credential.Purpose) {
			return problem.New(409, "credential_grant_kind_mismatch", "Workspace Credential Grant purpose is invalid.")
		}
		return nil
	})
	if err != nil {
		return ResolvedWorkspaceCredential{}, err
	}
	payload, err := s.resolveModel(ctx, credential)
	if err != nil {
		return ResolvedWorkspaceCredential{}, err
	}
	if err := validateGrantSelector(binding, credential, repositoryURL, payload); err != nil {
		return ResolvedWorkspaceCredential{}, err
	}
	return ResolvedWorkspaceCredential{
		GrantID: grant.ID, BindingKind: binding.BindingKind, Purpose: credential.Purpose,
		Provider: credential.Provider, CredentialType: credential.CredentialType,
		Selector: binding.SelectorValue, Payload: payload,
	}, nil
}

func validateGrantSelector(
	binding persistence.CredentialBinding,
	credential persistence.ProviderCredential,
	repositoryURL *string,
	payload map[string]any,
) error {
	if credential.Purpose != PurposeGit {
		selector, err := credentialPayloadIdentity(
			credential.Purpose, credential.Provider, credential.CredentialType, payload,
		)
		if err != nil || selector == "" || selector != binding.SelectorValue {
			return problem.New(409, "credential_grant_selector_mismatch", "Workspace Credential no longer matches its immutable Binding selector.")
		}
		return nil
	}
	if repositoryURL == nil || strings.TrimSpace(*repositoryURL) != binding.SelectorValue {
		return problem.New(409, "credential_grant_selector_mismatch", "Git Credential Binding no longer matches the Project repository.")
	}
	parsed, err := url.Parse(strings.TrimSpace(*repositoryURL))
	if err != nil || parsed.Opaque != "" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return problem.New(409, "credential_grant_selector_mismatch", "Project repository URL is invalid for its Git Credential Binding.")
	}
	host, err := gitpolicy.NormalizeHostname(parsed.Hostname())
	if err != nil {
		return problem.New(409, "credential_grant_selector_mismatch", "Project repository host is invalid for its Git Credential Binding.")
	}
	switch credential.CredentialType {
	case GitHTTPSCredentialType:
		value, decodeErr := decodeGitHTTPSPayload(payload)
		if decodeErr != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.User != nil ||
			(parsed.Port() != "" && parsed.Port() != "443") || host != value.Host {
			return problem.New(409, "credential_grant_selector_mismatch", "Git HTTPS Credential does not match the Project repository.")
		}
	case GitSSHCredentialType:
		value, decodeErr := decodeGitSSHPayload(payload)
		port := parsed.Port()
		if port == "" {
			port = "22"
		}
		parsedPort, portErr := strconv.Atoi(port)
		if decodeErr != nil || !strings.EqualFold(parsed.Scheme, "ssh") || parsed.User == nil ||
			parsed.User.Username() != value.Username || parsedPort != value.Port || portErr != nil ||
			host != value.Host {
			return problem.New(409, "credential_grant_selector_mismatch", "Git SSH Credential does not match the Project repository.")
		}
		if _, present := parsed.User.Password(); present {
			return problem.New(409, "credential_grant_selector_mismatch", "Project SSH repository URL must not contain a password.")
		}
	default:
		return problem.New(409, "credential_grant_kind_mismatch", "Git Credential type is unsupported.")
	}
	return nil
}

func grantBindingKindMatchesPurpose(kind, purpose string) bool {
	switch purpose {
	case PurposeGit:
		return kind == "git_fetch" || kind == "git_push"
	case PurposeRegistry:
		return kind == "registry_pull" || kind == "registry_push"
	case PurposePackage:
		return kind == "package_read" || kind == "package_publish"
	default:
		return false
	}
}
