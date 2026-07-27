package executions

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/synara-ai/synara/services/control-plane/internal/executionqueue"
	"github.com/synara-ai/synara/services/control-plane/internal/fairqueue"
)

// applyClaimFairShareOrder chooses the Tenant with the fewest active service
// units before applying the existing per-Tenant FIFO. Claim already retains
// the Execution Target row lock, so sequential claims for one shared Target
// observe the previous winner as active before selecting the next Tenant.
//
// The ORDER BY is a correlated subquery, so PostgreSQL evaluates it once per
// candidate row and cannot satisfy the ordering from an index. That makes
// `idx_agent_executions_fair_share_active` (migration 000085) load-bearing
// rather than an optional optimization: measured on PostgreSQL 17 with 200k
// non-terminal rows across 50 Tenants and 5 Targets, one claim took ~55ms with
// that index and ~8.2s after dropping it — roughly 150x. At a queue depth of
// 500 it is ~25ms. Removing the index, or widening the status set here without
// widening the index predicate to match, silently reintroduces the 8s claim.

func applyClaimFairShareOrder(tx, claimQuery *gorm.DB, warmPool bool, now time.Time) *gorm.DB {
	activeServiceUnits := tx.Table("agent_executions AS fair_active").
		Select("COUNT(*)").
		Where("fair_active.execution_target_id = agent_executions.execution_target_id").
		Where("fair_active.target_kind = agent_executions.target_kind").
		Where("fair_active.tenant_id = agent_executions.tenant_id").
		Where("fair_active.status IN ?", fairqueue.ActiveServiceStatuses())
	starvationBoundary := now.UTC().Add(-executionqueue.DefaultStarvationThreshold)
	orderSQL := `CASE WHEN agent_executions.queued_at <= ? THEN 0 ELSE 1 END,
		(?) ASC,
		CASE agent_executions.queue_class WHEN 'interactive' THEN 0 WHEN 'automation' THEN 1 ELSE 2 END,
		agent_executions.queue_priority DESC,
		agent_executions.queued_at,
		agent_executions.id`
	vars := []any{starvationBoundary, activeServiceUnits}
	if warmPool {
		orderSQL = `CASE WHEN agent_executions.queued_at <= ? THEN 0 ELSE 1 END,
			(?) ASC,
			CASE agent_executions.queue_class WHEN 'interactive' THEN 0 WHEN 'automation' THEN 1 ELSE 2 END,
			agent_executions.queue_priority DESC,
			CASE claim_session.warm_pool_mode
			WHEN 'low-latency' THEN 0
			WHEN 'balanced' THEN 1
			ELSE 2 END, agent_executions.queued_at, agent_executions.id`
	}
	return claimQuery.Order(clause.OrderBy{Expression: clause.Expr{
		SQL:                orderSQL,
		Vars:               vars,
		WithoutParentheses: true,
	}})
}
