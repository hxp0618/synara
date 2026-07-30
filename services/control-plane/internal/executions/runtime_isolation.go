package executions

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/identity"
	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/problem"
	"github.com/synara-ai/synara/services/control-plane/internal/sessions"
)

type RuntimeIsolationDecision struct {
	Generation           int64      `json:"generation"`
	ExecutionTargetID    uuid.UUID  `json:"executionTargetId"`
	AllocationBackend    string     `json:"allocationBackend"`
	RequestedRuntime     string     `json:"requestedRuntime"`
	RequestedProfile     string     `json:"requestedProfile"`
	EffectiveRuntime     *string    `json:"effectiveRuntime"`
	EffectiveProfile     *string    `json:"effectiveProfile"`
	PolicySource         string     `json:"policySource"`
	Decision             string     `json:"decision"`
	DecisionReasonCode   *string    `json:"decisionReasonCode"`
	RuntimeClassName     *string    `json:"runtimeClassName"`
	AttestedAt           *time.Time `json:"attestedAt"`
	AttestationExpiresAt *time.Time `json:"attestationExpiresAt"`
	CreatedAt            time.Time  `json:"createdAt"`
}

func (s *Service) ListRuntimeIsolationDecisions(
	ctx context.Context,
	principal identity.Principal,
	executionID uuid.UUID,
) ([]RuntimeIsolationDecision, error) {
	tenantID, err := sessions.ActiveTenant(principal)
	if err != nil {
		return nil, err
	}
	var execution persistence.AgentExecution
	err = s.db.WithContext(ctx).Select("id", "session_id").
		Where("tenant_id = ? AND id = ?", tenantID, executionID).Take(&execution).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, problem.New(404, "execution_not_found", "Execution not found.")
	}
	if err != nil {
		return nil, problem.Wrap(500, "execution_load_failed", "Failed to load the execution.", err)
	}
	if _, err := s.sessions.Get(ctx, principal, tenantID, execution.SessionID); err != nil {
		return nil, err
	}
	var models []persistence.ExecutionRuntimeIsolationDecision
	if err := s.db.WithContext(ctx).
		Where("tenant_id = ? AND execution_id = ?", tenantID, executionID).
		Order("generation, created_at").Find(&models).Error; err != nil {
		return nil, problem.Wrap(
			500,
			"runtime_isolation_decisions_load_failed",
			"Runtime isolation decisions could not be loaded.",
			err,
		)
	}
	items := make([]RuntimeIsolationDecision, 0, len(models))
	for _, model := range models {
		items = append(items, RuntimeIsolationDecision{
			Generation: model.Generation, ExecutionTargetID: model.ExecutionTargetID,
			AllocationBackend: model.AllocationBackend,
			RequestedRuntime:  model.RequestedRuntime, RequestedProfile: model.RequestedProfile,
			EffectiveRuntime: model.EffectiveRuntime, EffectiveProfile: model.EffectiveProfile,
			PolicySource: model.PolicySource, Decision: model.Decision,
			DecisionReasonCode: model.DecisionReasonCode, RuntimeClassName: model.RuntimeClassName,
			AttestedAt: model.AttestedAt, AttestationExpiresAt: model.AttestationExpiresAt,
			CreatedAt: model.CreatedAt,
		})
	}
	return items, nil
}
