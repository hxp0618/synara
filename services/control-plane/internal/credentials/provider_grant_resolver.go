package credentials

import (
	"context"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/executions"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
)

type ExecutionProviderCredentialGrantResolution struct {
	GrantID uuid.UUID                           `json:"grantId"`
	Payload map[string]any                      `json:"payload"`
	Access  executions.ProviderCredentialAccess `json:"access"`
}

func (s *Service) ResolveProviderGrantForExecution(
	ctx context.Context,
	executionService *executions.Service,
	worker persistence.WorkerInstance,
	executionID, grantID uuid.UUID,
	leaseInput executions.LeaseInput,
) (ExecutionProviderCredentialGrantResolution, error) {
	if s.cipher == nil {
		return ExecutionProviderCredentialGrantResolution{}, problem.New(503, "credential_kms_unavailable", "Credential KMS is not configured.")
	}
	var (
		credential      persistence.ProviderCredential
		resolvedGrantID uuid.UUID
		access          executions.ProviderCredentialAccess
	)
	err := persistence.InTransaction(ctx, s.db, func(tx *gorm.DB) error {
		resolution, err := executionService.ResolveProviderCredentialGrantAccess(
			ctx, tx, worker, executionID, grantID, leaseInput,
		)
		if err != nil {
			return err
		}
		resolvedGrantID = resolution.Grant.ID
		credential = resolution.Credential
		access = resolution.Access
		return nil
	})
	if err != nil {
		return ExecutionProviderCredentialGrantResolution{}, err
	}
	payload, err := s.resolveModel(ctx, credential)
	if err != nil {
		return ExecutionProviderCredentialGrantResolution{}, err
	}
	return ExecutionProviderCredentialGrantResolution{
		GrantID: resolvedGrantID, Payload: payload, Access: access,
	}, nil
}
