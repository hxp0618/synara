package credentials

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/credentialscope"
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
			Select("id", "tenant_id", "organization_id", "created_by", "model").
			Where("id = ? AND tenant_id = ?", execution.SessionID, execution.TenantID).
			Take(&session).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return problem.New(404, "session_not_found", "Agent Session not found.")
		}
		if err != nil {
			return problem.Wrap(500, "session_load_failed", "Agent Session could not be loaded.", err)
		}
		if execution.Provider == nil {
			return problem.New(500, "execution_provider_snapshot_invalid", "Execution Provider snapshot is invalid.")
		}
		selection, err := credentialscope.Resolve(ctx, tx, credentialscope.Request{
			TenantID: execution.TenantID, OrganizationID: session.OrganizationID,
			SessionOwnerUserID: session.CreatedBy, Provider: *execution.Provider, Model: session.Model,
			ExplicitCredentialID: &credentialID, Now: s.now(),
		})
		if err != nil {
			return err
		}
		if selection == nil {
			return problem.New(404, "credential_not_found", "Provider Credential not found.")
		}
		credential = selection.Credential
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
