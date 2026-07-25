package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateWorkerClaimFactsSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`CREATE INDEX IF NOT EXISTS idx_worker_claim_facts_request
		 ON worker_claim_facts (worker_id, worker_incarnation, request_id)`,
		`CREATE INDEX IF NOT EXISTS idx_worker_claim_facts_billing
		 ON worker_claim_facts (worker_id, worker_incarnation, claimed_at, id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_worker_claim_facts_execution_generation
		 ON worker_claim_facts (execution_id, execution_generation)
		 WHERE claim_kind = 'execution'`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_worker_claim_facts_cleanup_dispatch
		 ON worker_claim_facts (cleanup_command_id, cleanup_dispatch_generation)
		 WHERE claim_kind = 'workspace-cleanup'`,
		`DROP TRIGGER IF EXISTS trg_worker_claim_facts_insert`,
		`CREATE TRIGGER trg_worker_claim_facts_insert
		 BEFORE INSERT ON worker_claim_facts
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Worker claim fact')
		   WHERE NEW.worker_incarnation <= 0
		      OR NEW.target_kind NOT IN ('local', 'ssh', 'docker', 'kubernetes')
		      OR NEW.claim_kind NOT IN ('execution', 'workspace-cleanup')
		      OR length(trim(ifnull(NEW.request_id, ''))) NOT BETWEEN 1 AND 160
		      OR (
		        NEW.claim_kind = 'execution' AND (
		          NEW.execution_id IS NULL
		          OR NEW.execution_generation IS NULL
		          OR NEW.execution_generation <= 0
		          OR NEW.cleanup_command_id IS NOT NULL
		          OR NEW.cleanup_dispatch_generation IS NOT NULL
		        )
		      )
		      OR (
		        NEW.claim_kind = 'workspace-cleanup' AND (
		          NEW.execution_id IS NOT NULL
		          OR NEW.execution_generation IS NOT NULL
		          OR NEW.cleanup_command_id IS NULL
		          OR NEW.cleanup_dispatch_generation IS NULL
		          OR NEW.cleanup_dispatch_generation <= 0
		        )
		      )
		      OR NOT EXISTS (
		        SELECT 1
		        FROM tenants AS tenant
		        WHERE tenant.id = NEW.tenant_id
		      )
		      OR NOT EXISTS (
		        SELECT 1
		        FROM execution_targets AS target
		        WHERE target.id = NEW.execution_target_id
		          AND target.kind = NEW.target_kind
		          AND (target.tenant_id IS NULL OR target.tenant_id IS NEW.tenant_id)
		      )
		      OR NOT EXISTS (
		        SELECT 1
		        FROM worker_incarnation_facts AS fact
		        WHERE fact.worker_id = NEW.worker_id
		          AND fact.worker_incarnation = NEW.worker_incarnation
		          AND fact.execution_target_id = NEW.execution_target_id
		          AND fact.target_kind = NEW.target_kind
		          AND (fact.tenant_id IS NULL OR fact.tenant_id IS NEW.tenant_id)
		          AND NEW.claimed_at >= fact.registered_at
		          AND (fact.terminated_at IS NULL OR NEW.claimed_at <= fact.terminated_at)
		      )
		      OR (
		        NEW.claim_kind = 'execution' AND NOT EXISTS (
		          SELECT 1
		          FROM agent_executions AS execution
		          JOIN worker_leases AS lease
		            ON lease.tenant_id = execution.tenant_id
		           AND lease.execution_id = execution.id
		           AND lease.worker_id = NEW.worker_id
		           AND lease.worker_incarnation = NEW.worker_incarnation
		           AND lease.generation = NEW.execution_generation
		          WHERE execution.id = NEW.execution_id
		            AND execution.tenant_id = NEW.tenant_id
		            AND execution.execution_target_id = NEW.execution_target_id
		            AND execution.target_kind = NEW.target_kind
		            AND execution.generation = NEW.execution_generation
		            AND execution.worker_id = NEW.worker_id
		        )
		      )
		      OR (
		        NEW.claim_kind = 'workspace-cleanup' AND NOT EXISTS (
		          SELECT 1
		          FROM workspace_cleanup_commands AS cleanup
		          WHERE cleanup.id = NEW.cleanup_command_id
		            AND cleanup.tenant_id = NEW.tenant_id
		            AND cleanup.execution_target_id = NEW.execution_target_id
		            AND cleanup.target_kind = NEW.target_kind
		            AND cleanup.dispatch_generation = NEW.cleanup_dispatch_generation
		            AND cleanup.delivery_worker_id = NEW.worker_id
		            AND cleanup.delivery_worker_incarnation = NEW.worker_incarnation
		        )
		      );
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_claim_facts_update`,
		`CREATE TRIGGER trg_worker_claim_facts_update
		 BEFORE UPDATE ON worker_claim_facts
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker claim facts are immutable');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_claim_facts_delete`,
		`CREATE TRIGGER trg_worker_claim_facts_delete
		 BEFORE DELETE ON worker_claim_facts
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker claim facts are immutable');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_claim_facts_restrict_tenant_delete`,
		`CREATE TRIGGER trg_worker_claim_facts_restrict_tenant_delete
		 BEFORE DELETE ON tenants
		 WHEN EXISTS (SELECT 1 FROM worker_claim_facts AS claim WHERE claim.tenant_id = OLD.id)
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker claim fact parent is retained');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_claim_facts_restrict_target_delete`,
		`CREATE TRIGGER trg_worker_claim_facts_restrict_target_delete
		 BEFORE DELETE ON execution_targets
		 WHEN EXISTS (SELECT 1 FROM worker_claim_facts AS claim WHERE claim.execution_target_id = OLD.id)
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker claim fact parent is retained');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_claim_facts_restrict_worker_fact_delete`,
		`CREATE TRIGGER trg_worker_claim_facts_restrict_worker_fact_delete
		 BEFORE DELETE ON worker_incarnation_facts
		 WHEN EXISTS (
		   SELECT 1 FROM worker_claim_facts AS claim
		   WHERE claim.worker_id = OLD.worker_id AND claim.worker_incarnation = OLD.worker_incarnation
		 )
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker claim fact parent is retained');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_claim_facts_restrict_execution_delete`,
		`CREATE TRIGGER trg_worker_claim_facts_restrict_execution_delete
		 BEFORE DELETE ON agent_executions
		 WHEN EXISTS (SELECT 1 FROM worker_claim_facts AS claim WHERE claim.execution_id = OLD.id)
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker claim fact parent is retained');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_claim_facts_restrict_cleanup_delete`,
		`CREATE TRIGGER trg_worker_claim_facts_restrict_cleanup_delete
		 BEFORE DELETE ON workspace_cleanup_commands
		 WHEN EXISTS (SELECT 1 FROM worker_claim_facts AS claim WHERE claim.cleanup_command_id = OLD.id)
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker claim fact parent is retained');
		 END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply sqlite worker claim fact safety migration: %w", err)
		}
	}
	return nil
}
