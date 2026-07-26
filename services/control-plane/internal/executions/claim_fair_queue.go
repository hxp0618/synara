package executions

import (
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/fairqueue"
)

// applyClaimFairShareOrder chooses the Tenant with the fewest active service
// units before applying the existing per-Tenant FIFO. Claim already retains
// the Execution Target row lock, so sequential claims for one shared Target
// observe the previous winner as active before selecting the next Tenant.
func applyClaimFairShareOrder(tx, claimQuery *gorm.DB, warmPool bool) *gorm.DB {
	activeServiceUnits := tx.Table("agent_executions AS fair_active").
		Select("COUNT(*)").
		Where("fair_active.execution_target_id = agent_executions.execution_target_id").
		Where("fair_active.target_kind = agent_executions.target_kind").
		Where("fair_active.tenant_id = agent_executions.tenant_id").
		Where("fair_active.status IN ?", fairqueue.ActiveServiceStatuses())
	orderSQL := "(?) ASC, agent_executions.queued_at, agent_executions.id"
	if warmPool {
		orderSQL = `(?) ASC, CASE claim_session.warm_pool_mode
			WHEN 'low-latency' THEN 0
			WHEN 'balanced' THEN 1
			ELSE 2 END, agent_executions.queued_at, agent_executions.id`
	}
	return claimQuery.Order(clause.OrderBy{Expression: clause.Expr{
		SQL:                orderSQL,
		Vars:               []any{activeServiceUnits},
		WithoutParentheses: true,
	}})
}
