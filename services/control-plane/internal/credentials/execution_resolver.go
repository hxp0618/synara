package credentials

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

// ResolveForExecution returns a Credential only to the Worker that owns the
// current Execution Lease and Generation. The immutable Credential ID/version
// snapshot captured by Claim is selected in the same transaction as lease
// authorization so Session rebinding or Credential rotation cannot silently
// change the secret available to an in-flight generation.
func (s *Service) ResolveForExecution(
	ctx context.Context,
	executionService *executions.Service,
	worker persistence.WorkerInstance,
	executionID, credentialID uuid.UUID,
	leaseInput executions.LeaseInput,
) (map[string]any, error) {
	if s.cipher == nil {
		return nil, problem.New(503, "credential_kms_unavailable", "Credential KMS is not configured.")
	}
	var credential persistence.ProviderCredential
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		execution, err := executionService.AuthorizeLease(ctx, tx, worker, executionID, leaseInput)
		if err != nil {
			return err
		}
		if execution.ProviderCredentialIDSnapshot == nil || *execution.ProviderCredentialIDSnapshot != credentialID {
			return problem.New(404, "credential_not_found", "Provider Credential not found.")
		}
		if execution.ProviderCredentialVersionSnapshot == nil || *execution.ProviderCredentialVersionSnapshot <= 0 {
			return problem.New(500, "execution_credential_snapshot_invalid", "Execution Provider Credential snapshot is invalid.")
		}
		var session persistence.AgentSession
		err = tx.WithContext(ctx).
			Select("id", "tenant_id", "organization_id").
			Where("id = ? AND tenant_id = ?", execution.SessionID, execution.TenantID).
			Take(&session).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "session_not_found", "Agent Session not found.")
		}
		if err != nil {
			return problem.Wrap(500, "session_load_failed", "Agent Session could not be loaded.", err)
		}
		err = persistence.WithLocking(tx.WithContext(ctx), "SHARE", "").
			Where("tenant_id = ? AND id = ?", execution.TenantID, credentialID).
			Take(&credential).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "credential_not_found", "Provider Credential not found.")
		}
		if err != nil {
			return problem.Wrap(500, "credential_load_failed", "Provider Credential could not be loaded.", err)
		}
		if credential.OrganizationID != nil && *credential.OrganizationID != session.OrganizationID {
			return problem.New(404, "credential_not_found", "Provider Credential not found.")
		}
		if credential.Purpose != PurposeProvider {
			return problem.New(404, "credential_not_found", "Provider Credential not found.")
		}
		if execution.Provider == nil || credential.Provider != *execution.Provider {
			return problem.New(409, "credential_provider_mismatch", "Provider Credential does not match the Agent Session provider.")
		}
		if credential.Version != *execution.ProviderCredentialVersionSnapshot {
			return problem.New(409, "credential_version_fenced", "Provider Credential version no longer matches the Execution generation snapshot.")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.resolveModel(ctx, credential)
}

// ResolveGitForExecution returns the Project-bound Git Credential only to the
// Worker that owns the current Execution Lease and Generation. Repository host
// matching happens after decryption so the encrypted host never becomes
// Project metadata or a Workload field.
func (s *Service) ResolveGitForExecution(
	ctx context.Context,
	executionService *executions.Service,
	worker persistence.WorkerInstance,
	executionID, credentialID uuid.UUID,
	leaseInput executions.LeaseInput,
) (GitHTTPSPayload, error) {
	if s.cipher == nil {
		return GitHTTPSPayload{}, problem.New(503, "credential_kms_unavailable", "Credential KMS is not configured.")
	}
	var credential persistence.ProviderCredential
	var repositoryURL string
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		execution, err := executionService.AuthorizeLease(ctx, tx, worker, executionID, leaseInput)
		if err != nil {
			return err
		}
		var binding struct {
			OrganizationID  uuid.UUID  `gorm:"column:organization_id"`
			GitCredentialID *uuid.UUID `gorm:"column:git_credential_id"`
			RepositoryURL   *string    `gorm:"column:repository_url"`
		}
		err = tx.WithContext(ctx).Table("agent_sessions AS s").
			Select("s.organization_id, p.git_credential_id, p.repository_url").
			Joins("JOIN projects AS p ON p.tenant_id = s.tenant_id AND p.id = s.project_id").
			Where("s.tenant_id = ? AND s.id = ?", execution.TenantID, execution.SessionID).
			Take(&binding).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "project_not_found", "Project not found.")
		}
		if err != nil {
			return problem.Wrap(500, "project_load_failed", "Project could not be loaded.", err)
		}
		if binding.GitCredentialID == nil || *binding.GitCredentialID != credentialID || binding.RepositoryURL == nil {
			return problem.New(404, "credential_not_found", "Git Credential not found.")
		}
		repositoryURL = strings.TrimSpace(*binding.RepositoryURL)
		err = persistence.WithLocking(tx.WithContext(ctx), "SHARE", "").
			Where("tenant_id = ? AND id = ?", execution.TenantID, credentialID).
			Take(&credential).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "credential_not_found", "Git Credential not found.")
		}
		if err != nil {
			return problem.Wrap(500, "credential_load_failed", "Git Credential could not be loaded.", err)
		}
		if credential.Purpose != PurposeGit || credential.Provider != GitProvider || credential.CredentialType != GitHTTPSCredentialType {
			return problem.New(404, "credential_not_found", "Git Credential not found.")
		}
		if credential.OrganizationID != nil && *credential.OrganizationID != binding.OrganizationID {
			return problem.New(404, "credential_not_found", "Git Credential not found.")
		}
		return nil
	})
	if err != nil {
		return GitHTTPSPayload{}, err
	}
	payload, err := s.resolveModel(ctx, credential)
	if err != nil {
		return GitHTTPSPayload{}, err
	}
	gitCredential, err := decodeGitHTTPSPayload(payload)
	if err != nil {
		return GitHTTPSPayload{}, problem.New(500, "credential_payload_invalid", "Git Credential payload is invalid.")
	}
	repositoryHost, err := repositoryHostname(repositoryURL)
	if err != nil || repositoryHost != gitCredential.Host {
		return GitHTTPSPayload{}, problem.New(409, "git_credential_host_mismatch", "Git Credential host does not match the Project repository.")
	}
	return gitCredential, nil
}

func repositoryHostname(repositoryURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(repositoryURL))
	if err != nil || parsed.Opaque != "" || !strings.EqualFold(parsed.Scheme, "https") ||
		parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("invalid HTTPS repository URL")
	}
	if parsed.Port() != "" && parsed.Port() != "443" {
		return "", errors.New("Git Credentials support only HTTPS port 443")
	}
	return normalizeGitHost(parsed.Hostname())
}
