package credentials

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

// ResolveForExecution returns a Credential only to the Worker that owns the
// current Execution Lease and Generation. The encrypted snapshot is selected
// in the same transaction as lease authorization so a Worker cannot retrieve a
// Credential for another Tenant or Organization.
func (s *Service) ResolveForExecution(
	ctx context.Context,
	executionService *executions.Service,
	worker persistence.WorkerInstance,
	executionID, credentialID uuid.UUID,
	leaseInput executions.LeaseInput,
) (map[string]any, error) {
	if s.cipher == nil {
		return nil, problem.New(503, "credential_kms_unavailable", "Provider Credential KMS is not configured.")
	}
	var credential persistence.ProviderCredential
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		execution, err := executionService.AuthorizeLease(ctx, tx, worker, executionID, leaseInput)
		if err != nil {
			return err
		}
		var session persistence.AgentSession
		err = tx.WithContext(ctx).
			Select("id", "tenant_id", "organization_id", "provider", "provider_credential_id").
			Where("id = ? AND tenant_id = ?", execution.SessionID, execution.TenantID).
			Take(&session).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "session_not_found", "Agent Session not found.")
		}
		if err != nil {
			return problem.Wrap(500, "session_load_failed", "Agent Session could not be loaded.", err)
		}
		if session.ProviderCredentialID == nil || *session.ProviderCredentialID != credentialID {
			return problem.New(404, "credential_not_found", "Provider Credential not found.")
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
		if credential.Provider != session.Provider {
			return problem.New(409, "credential_provider_mismatch", "Provider Credential does not match the Agent Session provider.")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.resolveModel(ctx, credential)
}
