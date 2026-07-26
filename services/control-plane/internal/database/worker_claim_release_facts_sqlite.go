package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateWorkerClaimReleaseFactsSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`CREATE INDEX IF NOT EXISTS idx_worker_claim_release_facts_released
		 ON worker_claim_release_facts (released_at, claim_fact_id)`,
		`DROP TRIGGER IF EXISTS trg_worker_claim_release_facts_insert`,
		`CREATE TRIGGER trg_worker_claim_release_facts_insert
		 BEFORE INSERT ON worker_claim_release_facts
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Worker claim release fact')
		   WHERE NEW.release_reason NOT IN (
		       'execution_completed', 'execution_failed', 'worker_released',
		       'orphan_lease_expired', 'lease_expired', 'user_cancelled',
		       'tenant_deleted', 'session_absolute_expired', 'interaction_expired',
		       'resource_suspended_worker_attested', 'resource_suspended_pod_terminal',
		       'control_interrupted', 'control_operation_completed', 'worker_revoked',
		       'cleanup_acknowledged', 'cleanup_failed_retryable', 'cleanup_failed_terminal',
		       'cleanup_worker_released', 'cleanup_lease_expired', 'cleanup_attempts_exhausted',
		       'cleanup_worker_revoked', 'cleanup_pod_confirmed_absent'
		     )
		      OR NEW.authority_kind NOT IN ('worker', 'user', 'control-plane', 'kubernetes')
		      OR (NEW.authority_id IS NOT NULL AND length(trim(NEW.authority_id)) NOT BETWEEN 1 AND 160)
		      OR (NEW.request_id IS NOT NULL AND length(trim(NEW.request_id)) NOT BETWEEN 1 AND 160)
		      OR julianday(NEW.recorded_at) < julianday(NEW.released_at)
		      OR json_valid(NEW.metadata) <> 1
		      OR json_type(NEW.metadata) <> 'object'
		      OR length(CAST(NEW.metadata AS TEXT)) > 4096
		      OR NOT EXISTS (
		        SELECT 1 FROM worker_claim_facts AS claim
		        WHERE claim.id = NEW.claim_fact_id
		          AND julianday(NEW.released_at) >= julianday(claim.claimed_at)
		      );
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_claim_release_facts_update`,
		`CREATE TRIGGER trg_worker_claim_release_facts_update
		 BEFORE UPDATE ON worker_claim_release_facts
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker claim release facts are immutable');
		 END`,
		`DROP TRIGGER IF EXISTS trg_worker_claim_release_facts_delete`,
		`CREATE TRIGGER trg_worker_claim_release_facts_delete
		 BEFORE DELETE ON worker_claim_release_facts
		 BEGIN
		   SELECT RAISE(ABORT, 'Worker claim release facts are immutable');
		 END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply sqlite worker claim release fact safety migration: %w", err)
		}
	}
	return nil
}
