package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"

	"github.com/synara-ai/synara/services/control-plane/internal/persistence"
	"github.com/synara-ai/synara/services/control-plane/internal/schedulingdecision"
)

func migrateExecutionSchedulingDecisionsSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	// AutoMigrate cannot compute SHA-256 backfill values. Keep the canonical
	// encoder in schedulingdecision as the one source of truth and insert each
	// legacy graph transactionally before installing/reinstalling its fences.
	var legacyExecutions []persistence.AgentExecution
	if err := db.WithContext(ctx).
		Where(`NOT EXISTS (
			SELECT 1 FROM execution_scheduling_decisions AS decision
			WHERE decision.tenant_id = agent_executions.tenant_id
			  AND decision.execution_id = agent_executions.id
		)`).
		Find(&legacyExecutions).Error; err != nil {
		return fmt.Errorf("load sqlite legacy Executions for Scheduling Decision backfill: %w", err)
	}
	for _, execution := range legacyExecutions {
		decision, candidates, err := schedulingdecision.BuildLegacySelectedOnly(execution)
		if err != nil {
			return fmt.Errorf("build sqlite legacy Scheduling Decision for Execution %s: %w", execution.ID, err)
		}
		if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Create(&decision).Error; err != nil {
				return err
			}
			if err := tx.Create(&candidates).Error; err != nil {
				return err
			}
			return tx.Model(&persistence.AgentExecution{}).
				Where("tenant_id = ? AND id = ? AND scheduling_decision_id IS NULL", execution.TenantID, execution.ID).
				Update("scheduling_decision_id", decision.ID).Error
		}); err != nil {
			return fmt.Errorf("backfill sqlite Scheduling Decision for Execution %s: %w", execution.ID, err)
		}
	}

	// SQLite has no deferred commit trigger or built-in SHA-256 aggregate. These
	// fences enforce row shape, one selected row at most, selected/execution
	// identity, and immutability. CreateExecution is the enforcement boundary for
	// exact candidate_count, contiguous ordinals, one selected row, candidate
	// canonical hashes, and candidate_set_sha256.
	statements := []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_execution_scheduling_decisions_tenant_id
		 ON execution_scheduling_decisions (tenant_id, id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_execution_scheduling_decisions_execution
		 ON execution_scheduling_decisions (tenant_id, execution_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_execution_scheduling_candidates_selected
		 ON execution_scheduling_candidates (tenant_id, decision_id) WHERE selected = 1`,
		`CREATE INDEX IF NOT EXISTS idx_execution_scheduling_decisions_decided
		 ON execution_scheduling_decisions (tenant_id, decided_at DESC, id)`,
		`DROP TRIGGER IF EXISTS trg_execution_scheduling_decisions_insert`,
		`CREATE TRIGGER trg_execution_scheduling_decisions_insert
		 BEFORE INSERT ON execution_scheduling_decisions
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Execution Scheduling Decision shape')
		   WHERE NEW.algorithm_version NOT IN ('queue-pressure-v1', 'fixed-target-v1', 'legacy-selected-only')
		      OR NEW.evidence_completeness NOT IN ('complete', 'selected-only', 'legacy-selected-only')
		      OR NEW.decision_kind NOT IN ('target-group', 'fixed-target')
		      OR length(NEW.provider) NOT BETWEEN 1 AND 80
		      OR instr(NEW.provider, ' ') > 0 OR instr(NEW.provider, char(9)) > 0
		      OR instr(NEW.provider, char(10)) > 0 OR instr(NEW.provider, char(13)) > 0
		      OR NEW.candidate_count NOT BETWEEN 1 AND 4096
		      OR NEW.selected_ordinal NOT BETWEEN 0 AND 4095
		      OR (NEW.evidence_completeness <> 'complete' AND NEW.candidate_count <> 1)
		      OR ((NEW.algorithm_version = 'legacy-selected-only') <> (NEW.evidence_completeness = 'legacy-selected-only'))
		      OR (NEW.decision_kind = 'target-group' AND NEW.algorithm_version NOT IN ('queue-pressure-v1', 'legacy-selected-only'))
		      OR (NEW.decision_kind = 'fixed-target' AND NEW.algorithm_version NOT IN ('fixed-target-v1', 'legacy-selected-only'))
		      OR length(NEW.candidate_set_sha256) <> 64 OR NEW.candidate_set_sha256 GLOB '*[^0-9a-f]*'
		      OR length(NEW.selected_region) > 120 OR trim(NEW.selected_region) <> NEW.selected_region
		      OR length(NEW.selected_cluster_id) > 200 OR trim(NEW.selected_cluster_id) <> NEW.selected_cluster_id;
		   SELECT RAISE(ABORT, 'Execution Scheduling Decision does not match its Execution')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM agent_executions AS execution
		     WHERE execution.tenant_id = NEW.tenant_id AND execution.id = NEW.execution_id
		       AND (execution.scheduling_decision_id IS NULL OR execution.scheduling_decision_id = NEW.id)
		       AND execution.execution_target_id = NEW.selected_execution_target_id
		       AND execution.worker_pool_id IS NEW.selected_worker_pool_id
		       AND NEW.decision_kind = CASE WHEN execution.target_group_id IS NULL THEN 'fixed-target' ELSE 'target-group' END
		       AND CASE WHEN execution.target_group_id IS NOT NULL THEN execution.selected_region ELSE execution.placement_region END = NEW.selected_region
		       AND CASE WHEN execution.target_group_id IS NOT NULL THEN execution.selected_cluster_id ELSE execution.placement_cluster_id END = NEW.selected_cluster_id
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_scheduling_candidates_insert`,
		`CREATE TRIGGER trg_execution_scheduling_candidates_insert
		 BEFORE INSERT ON execution_scheduling_candidates
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Execution Scheduling Candidate shape')
		   WHERE NEW.ordinal NOT BETWEEN 0 AND 4095
		      OR NEW.target_kind NOT IN ('local', 'ssh', 'docker', 'kubernetes')
		      OR length(NEW.region) > 120 OR trim(NEW.region) <> NEW.region
		      OR length(NEW.cluster_id) > 200 OR trim(NEW.cluster_id) <> NEW.cluster_id
		      OR NEW.eligibility NOT IN ('eligible', 'rejected')
		      OR (NEW.eligibility = 'eligible' AND NEW.rejection_code IS NOT NULL)
		      OR (NEW.eligibility = 'rejected' AND (NEW.rejection_code IS NULL OR length(trim(NEW.rejection_code)) NOT BETWEEN 1 AND 120))
		      OR (NEW.selected = 1 AND NEW.eligibility <> 'eligible')
		      OR length(NEW.candidate_sha256) <> 64 OR NEW.candidate_sha256 GLOB '*[^0-9a-f]*'
		      OR NEW.health_version < 1 OR NEW.available_capacity_units < 0 OR NEW.allocated_capacity_units < 0
		      OR NEW.queued_execution_units < 0 OR NEW.effective_load_rank < 0
		      OR NEW.priority < 0 OR NEW.weight < 1 OR NEW.priority_rank < 0 OR NEW.region_rank < 0 OR NEW.capacity_rank < 0
		      OR NEW.worker_pool_version < 1 OR NEW.placement_policy_version < 1
		      OR ((NEW.queued_execution_units IS NULL) <> (NEW.effective_load_rank IS NULL))
		      OR ((NEW.worker_pool_id IS NULL AND NEW.worker_pool_version IS NULL AND NEW.capacity_class IS NULL AND NEW.placement_policy_version IS NULL) = 0
		          AND (NEW.worker_pool_id IS NOT NULL AND NEW.worker_pool_version IS NOT NULL AND NEW.capacity_class IS NOT NULL AND NEW.placement_policy_version IS NOT NULL) = 0)
		      OR ((NEW.health_version IS NULL AND NEW.health_status IS NULL AND NEW.capacity_status IS NULL
		           AND NEW.health_observed_at IS NULL AND NEW.health_expires_at IS NULL AND NEW.allocated_capacity_units IS NULL) = 0
		          AND (NEW.health_version IS NOT NULL AND NEW.health_status IN ('healthy', 'degraded', 'unreachable', 'unknown')
		           AND NEW.capacity_status IN ('available', 'saturated', 'unknown') AND NEW.health_observed_at IS NOT NULL
		           AND julianday(NEW.health_expires_at) > julianday(NEW.health_observed_at) AND NEW.allocated_capacity_units IS NOT NULL) = 0)
		      OR (NEW.available_capacity_units IS NOT NULL AND NEW.health_version IS NULL)
		      OR ((NEW.dr_readiness_version IS NULL AND NEW.source_dr_domain IS NULL AND NEW.dr_domain IS NULL
		           AND NEW.dr_replicated_through_at IS NULL AND NEW.dr_artifacts_ready IS NULL
		           AND NEW.dr_checkpoints_ready IS NULL AND NEW.dr_memory_ready IS NULL
		           AND NEW.dr_observed_at IS NULL AND NEW.dr_expires_at IS NULL) = 0
		          AND (NEW.dr_readiness_version > 0 AND length(trim(NEW.source_dr_domain)) BETWEEN 1 AND 160
		           AND length(trim(NEW.dr_domain)) BETWEEN 1 AND 160 AND NEW.dr_replicated_through_at IS NOT NULL
		           AND NEW.dr_artifacts_ready IS NOT NULL AND NEW.dr_checkpoints_ready IS NOT NULL AND NEW.dr_memory_ready IS NOT NULL
		           AND julianday(NEW.dr_replicated_through_at) <= julianday(NEW.dr_observed_at)
		           AND julianday(NEW.dr_expires_at) > julianday(NEW.dr_observed_at)) = 0);
		   SELECT RAISE(ABORT, 'Execution Scheduling Candidate Decision is unavailable')
		   WHERE NOT EXISTS (SELECT 1 FROM execution_scheduling_decisions AS decision
		     WHERE decision.tenant_id = NEW.tenant_id AND decision.id = NEW.decision_id);
		   SELECT RAISE(ABORT, 'selected Execution Scheduling Candidate does not match its Execution')
		   WHERE NEW.selected = 1 AND NOT EXISTS (
		     SELECT 1 FROM execution_scheduling_decisions AS decision
		     JOIN agent_executions AS execution
		       ON execution.tenant_id = decision.tenant_id AND execution.id = decision.execution_id
		     WHERE decision.tenant_id = NEW.tenant_id AND decision.id = NEW.decision_id
		       AND decision.selected_ordinal = NEW.ordinal
		       AND decision.selected_execution_target_id = NEW.execution_target_id
		       AND decision.selected_worker_pool_id IS NEW.worker_pool_id
		       AND decision.selected_region = NEW.region AND decision.selected_cluster_id = NEW.cluster_id
		       AND execution.execution_target_id = NEW.execution_target_id
		       AND execution.target_kind = NEW.target_kind
		       AND execution.target_group_id IS NEW.target_group_id
		       AND execution.target_group_version IS NEW.target_group_version
		       AND execution.target_group_member_version IS NEW.target_group_member_version
		       AND execution.worker_pool_id IS NEW.worker_pool_id
		       AND execution.worker_pool_version IS NEW.worker_pool_version
		       AND execution.capacity_class IS NEW.capacity_class
		       AND execution.placement_policy_version IS NEW.placement_policy_version
		       AND CASE WHEN execution.target_group_id IS NOT NULL THEN execution.selected_region ELSE execution.placement_region END = NEW.region
		       AND CASE WHEN execution.target_group_id IS NOT NULL THEN execution.selected_cluster_id ELSE execution.placement_cluster_id END = NEW.cluster_id
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_scheduling_decisions_update`,
		`CREATE TRIGGER trg_execution_scheduling_decisions_update BEFORE UPDATE ON execution_scheduling_decisions
		 BEGIN SELECT RAISE(ABORT, 'Execution Scheduling Decision evidence is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_execution_scheduling_decisions_delete`,
		`CREATE TRIGGER trg_execution_scheduling_decisions_delete BEFORE DELETE ON execution_scheduling_decisions
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution Scheduling Decision is retained by its Execution')
		   WHERE EXISTS (SELECT 1 FROM agent_executions AS execution
		     WHERE execution.tenant_id = OLD.tenant_id AND execution.id = OLD.execution_id);
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_scheduling_decisions_delete_candidates`,
		`CREATE TRIGGER trg_execution_scheduling_decisions_delete_candidates
		 AFTER DELETE ON execution_scheduling_decisions
		 BEGIN
		   DELETE FROM execution_scheduling_candidates
		   WHERE tenant_id = OLD.tenant_id AND decision_id = OLD.id;
		 END`,
		`DROP TRIGGER IF EXISTS trg_execution_scheduling_candidates_update`,
		`CREATE TRIGGER trg_execution_scheduling_candidates_update BEFORE UPDATE ON execution_scheduling_candidates
		 BEGIN SELECT RAISE(ABORT, 'Execution Scheduling Decision evidence is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_execution_scheduling_candidates_delete`,
		`CREATE TRIGGER trg_execution_scheduling_candidates_delete BEFORE DELETE ON execution_scheduling_candidates
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution Scheduling Candidate is retained by its Decision')
		   WHERE EXISTS (SELECT 1 FROM execution_scheduling_decisions AS decision
		     WHERE decision.tenant_id = OLD.tenant_id AND decision.id = OLD.decision_id);
		 END`,
		`DROP TRIGGER IF EXISTS trg_agent_executions_scheduling_decision_update`,
		`CREATE TRIGGER trg_agent_executions_scheduling_decision_update
		 BEFORE UPDATE OF scheduling_decision_id ON agent_executions
		 BEGIN
		   SELECT RAISE(ABORT, 'Execution Scheduling Decision identity is immutable')
		   WHERE OLD.scheduling_decision_id IS NOT NULL AND NEW.scheduling_decision_id IS NOT OLD.scheduling_decision_id;
		   SELECT RAISE(ABORT, 'Execution Scheduling Decision identity is unavailable')
		   WHERE NEW.scheduling_decision_id IS NOT NULL AND NOT EXISTS (
		     SELECT 1 FROM execution_scheduling_decisions AS decision
		     WHERE decision.tenant_id = NEW.tenant_id AND decision.id = NEW.scheduling_decision_id
		       AND decision.execution_id = NEW.id
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_agent_executions_scheduling_decision_delete`,
		`CREATE TRIGGER trg_agent_executions_scheduling_decision_delete
		 AFTER DELETE ON agent_executions
		 BEGIN
		   DELETE FROM execution_scheduling_decisions
		   WHERE tenant_id = OLD.tenant_id AND execution_id = OLD.id;
		 END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply sqlite Execution Scheduling Decision safety migration: %w", err)
		}
	}
	return nil
}
