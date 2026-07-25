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

func bindExecutionProviderCredentialGrant(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
	now time.Time,
) (*uuid.UUID, error) {
	if execution.ProviderCredentialIDSnapshot == nil ||
		execution.ProviderCredentialVersionSnapshot == nil ||
		*execution.ProviderCredentialVersionSnapshot <= 0 {
		return nil, nil
	}
	existing, err := loadExecutionProviderCredentialGrantID(ctx, tx, execution)
	if err != nil || existing != nil {
		return existing, err
	}
	grant := persistence.ExecutionProviderCredentialGrant{
		ID: uuid.New(), TenantID: execution.TenantID, ExecutionID: execution.ID,
		Generation: execution.Generation, CredentialID: *execution.ProviderCredentialIDSnapshot,
		CredentialVersion: *execution.ProviderCredentialVersionSnapshot, CreatedAt: now,
	}
	if err := tx.WithContext(ctx).Create(&grant).Error; err != nil {
		return nil, problem.Wrap(409, "provider_credential_grant_create_failed", "Failed to snapshot the Execution Provider Credential for this generation.", err)
	}
	return &grant.ID, nil
}

func loadExecutionProviderCredentialGrantID(
	ctx context.Context,
	tx *gorm.DB,
	execution persistence.AgentExecution,
) (*uuid.UUID, error) {
	var grant persistence.ExecutionProviderCredentialGrant
	err := tx.WithContext(ctx).
		Where("tenant_id = ? AND execution_id = ? AND generation = ?", execution.TenantID, execution.ID, execution.Generation).
		Take(&grant).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, problem.Wrap(500, "provider_credential_grant_load_failed", "Failed to load the Execution Provider Credential grant.", err)
	}
	return &grant.ID, nil
}
