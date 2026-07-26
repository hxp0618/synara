package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateSharedCostAllocationSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`CREATE INDEX IF NOT EXISTS idx_billing_shared_cost_allocation_runs_target_period
		 ON billing_shared_cost_allocation_runs (
		   execution_target_id, billing_period_start_at, billing_period_end_at, id
		 )`,
		`CREATE INDEX IF NOT EXISTS idx_billing_shared_estimated_charge_slices_run
		 ON billing_shared_estimated_charge_slices (run_id, allocation_kind, usage_start_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_shared_estimated_charge_slices_tenant_period
		 ON billing_shared_estimated_charge_slices (
		   tenant_id, billing_period_start_at, billing_period_end_at, charge_kind, id
		 )`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_billing_shared_estimated_charge_slices_semantic
		 ON billing_shared_estimated_charge_slices (
		   run_id, allocation_kind, ifnull(tenant_id, '-'), ifnull(claim_fact_id, '-'),
		   tariff_id, charge_kind, usage_start_at, usage_end_at
		 )`,
		`DROP TRIGGER IF EXISTS trg_billing_shared_target_ledger_coverages_insert`,
		`CREATE TRIGGER trg_billing_shared_target_ledger_coverages_insert
		 BEFORE INSERT ON billing_shared_target_ledger_coverages
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid billing shared Target ledger coverage')
		   WHERE length(NEW.minimum_writer_version) NOT BETWEEN 1 AND 80
		      OR NEW.minimum_writer_version GLOB '*[^A-Za-z0-9._+-]*'
		      OR substr(NEW.minimum_writer_version, 1, 1) GLOB '[^A-Za-z0-9]'
		      OR length(NEW.deployment_attestation_sha256) <> 64
		      OR NEW.deployment_attestation_sha256 GLOB '*[^0-9a-f]*'
		      OR julianday(NEW.sealed_at) < julianday(NEW.complete_from_at)
		      OR NOT EXISTS (
		        SELECT 1 FROM execution_targets AS target
		        WHERE target.id = NEW.execution_target_id AND target.tenant_id IS NULL
		      );
		 END`,
		`DROP TRIGGER IF EXISTS trg_billing_shared_cost_allocation_runs_insert`,
		`CREATE TRIGGER trg_billing_shared_cost_allocation_runs_insert
		 BEFORE INSERT ON billing_shared_cost_allocation_runs
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid billing shared cost allocation run')
		   WHERE NEW.provider NOT GLOB '[a-z0-9]*'
		      OR length(NEW.provider) NOT BETWEEN 1 AND 64
		      OR NEW.provider GLOB '*[^a-z0-9_-]*'
		      OR length(NEW.region) > 160 OR trim(NEW.region) IS NOT NEW.region
		      OR length(NEW.currency_code) <> 3 OR NEW.currency_code GLOB '*[^A-Z]*'
		      OR NEW.algorithm_version <> 'closed-claim-interval-v1'
		      OR julianday(NEW.billing_period_end_at) <= julianday(NEW.billing_period_start_at)
		      OR julianday(NEW.usage_end_at) <= julianday(NEW.usage_start_at)
		      OR julianday(NEW.usage_start_at) < julianday(NEW.billing_period_start_at)
		      OR julianday(NEW.usage_end_at) > julianday(NEW.billing_period_end_at)
		      OR julianday(NEW.created_at) < julianday(NEW.usage_end_at)
		      OR NEW.claim_count < 0 OR NEW.release_count <> NEW.claim_count
			      OR NEW.tenant_allocated_seconds < 0 OR NEW.platform_idle_seconds < 0
			      OR length(NEW.ledger_sha256) <> 64 OR NEW.ledger_sha256 GLOB '*[^0-9a-f]*'
			      OR EXISTS (
			        SELECT 1 FROM billing_shared_cost_allocation_runs AS existing
			        WHERE existing.worker_id = NEW.worker_id
			          AND existing.worker_incarnation = NEW.worker_incarnation
			          AND existing.provider = NEW.provider
			          AND existing.currency_code = NEW.currency_code
			          AND existing.algorithm_version = NEW.algorithm_version
			          AND julianday(existing.billing_period_start_at) < julianday(NEW.billing_period_end_at)
			          AND julianday(existing.billing_period_end_at) > julianday(NEW.billing_period_start_at)
			          AND NOT (
			            julianday(existing.billing_period_start_at) = julianday(NEW.billing_period_start_at)
			            AND julianday(existing.billing_period_end_at) = julianday(NEW.billing_period_end_at)
			          )
			      )
			      OR NOT EXISTS (
		        SELECT 1
		        FROM billing_shared_target_ledger_coverages AS coverage
		        JOIN worker_incarnation_facts AS worker_fact
		          ON worker_fact.worker_id = NEW.worker_id
		         AND worker_fact.worker_incarnation = NEW.worker_incarnation
		        WHERE coverage.id = NEW.ledger_coverage_id
		          AND coverage.execution_target_id = NEW.execution_target_id
		          AND worker_fact.tenant_id IS NULL
		          AND worker_fact.execution_target_id = NEW.execution_target_id
		          AND worker_fact.region = NEW.region
		          AND worker_fact.current_state = 'terminated'
		          AND worker_fact.terminated_at IS NOT NULL
		          AND julianday(worker_fact.registered_at) >= julianday(coverage.complete_from_at)
		          AND julianday(NEW.usage_start_at) >= julianday(worker_fact.registered_at)
		          AND julianday(NEW.usage_end_at) <= julianday(worker_fact.terminated_at)
		      );
		 END`,
		`DROP TRIGGER IF EXISTS trg_billing_shared_estimated_charge_slices_insert`,
		`CREATE TRIGGER trg_billing_shared_estimated_charge_slices_insert
		 BEFORE INSERT ON billing_shared_estimated_charge_slices
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid billing shared estimated charge slice')
		   WHERE NEW.charge_kind NOT IN ('cpu', 'memory', 'ephemeral-storage', 'request', 'pod')
		      OR NEW.allocation_kind NOT IN ('tenant-claim', 'platform-idle')
		      OR NOT (
		        (NEW.allocation_kind = 'tenant-claim' AND NEW.tenant_id IS NOT NULL
		          AND NEW.claim_fact_id IS NOT NULL AND NEW.claim_count = 1)
		        OR
		        (NEW.allocation_kind = 'platform-idle' AND NEW.tenant_id IS NULL
		          AND NEW.claim_fact_id IS NULL AND NEW.claim_count = 0 AND NEW.charge_kind <> 'request')
		      )
		      OR length(trim(NEW.resource_correlation_key)) NOT BETWEEN 1 AND 512
		      OR julianday(NEW.billing_period_end_at) <= julianday(NEW.billing_period_start_at)
		      OR julianday(NEW.usage_end_at) <= julianday(NEW.usage_start_at)
		      OR julianday(NEW.usage_start_at) < julianday(NEW.billing_period_start_at)
		      OR julianday(NEW.usage_end_at) > julianday(NEW.billing_period_end_at)
		      OR julianday(NEW.created_at) < julianday(NEW.usage_end_at)
		      OR NEW.billable_seconds < 0 OR NEW.rate_micros < 0 OR NEW.amount_micros < 0
		      OR (NEW.requested_cpu_millicores IS NOT NULL AND NEW.requested_cpu_millicores <= 0)
		      OR (NEW.requested_memory_bytes IS NOT NULL AND NEW.requested_memory_bytes <= 0)
		      OR (NEW.requested_ephemeral_storage_bytes IS NOT NULL AND NEW.requested_ephemeral_storage_bytes <= 0)
		      OR NOT EXISTS (
		        SELECT 1
			        FROM billing_shared_cost_allocation_runs AS allocation_run
			        JOIN billing_provider_tariffs AS tariff ON tariff.id = NEW.tariff_id
			        JOIN worker_incarnation_facts AS worker_fact
			          ON worker_fact.worker_id = allocation_run.worker_id
			         AND worker_fact.worker_incarnation = allocation_run.worker_incarnation
			        WHERE allocation_run.id = NEW.run_id
		          AND julianday(NEW.billing_period_start_at) = julianday(allocation_run.billing_period_start_at)
		          AND julianday(NEW.billing_period_end_at) = julianday(allocation_run.billing_period_end_at)
		          AND julianday(NEW.usage_start_at) >= julianday(allocation_run.usage_start_at)
		          AND julianday(NEW.usage_end_at) <= julianday(allocation_run.usage_end_at)
		          AND tariff.provider = allocation_run.provider
		          AND (
		            tariff.region = allocation_run.region
		            OR (allocation_run.region <> '' AND tariff.region = '')
			          )
			          AND tariff.currency_code = allocation_run.currency_code
			          AND NEW.requested_cpu_millicores IS worker_fact.requested_cpu_millicores
			          AND NEW.requested_memory_bytes IS worker_fact.requested_memory_bytes
			          AND NEW.requested_ephemeral_storage_bytes IS worker_fact.requested_ephemeral_storage_bytes
			          AND NEW.rate_micros = CASE NEW.charge_kind
			            WHEN 'cpu' THEN tariff.cpu_core_hour_rate_micros
			            WHEN 'memory' THEN tariff.memory_gib_hour_rate_micros
			            WHEN 'ephemeral-storage' THEN tariff.ephemeral_gib_hour_rate_micros
			            WHEN 'request' THEN tariff.request_rate_micros
			            WHEN 'pod' THEN tariff.pod_hour_rate_micros
			            ELSE -1
			          END
			          AND (NEW.charge_kind <> 'request'
			            OR (NEW.billable_seconds = 0 AND NEW.amount_micros = NEW.rate_micros))
		          AND (
		            NEW.charge_kind = 'request'
		            OR (
		              julianday(NEW.usage_start_at) >= julianday(tariff.effective_start_at)
		              AND (tariff.effective_end_at IS NULL
		                OR julianday(NEW.usage_end_at) <= julianday(tariff.effective_end_at))
		            )
		          )
		      )
		      OR (
		        NEW.allocation_kind = 'tenant-claim'
			        AND NOT EXISTS (
		          SELECT 1
		          FROM billing_shared_cost_allocation_runs AS allocation_run
		          JOIN worker_claim_facts AS claim ON claim.id = NEW.claim_fact_id
		          JOIN worker_claim_release_facts AS claim_release
		            ON claim_release.claim_fact_id = claim.id
		          JOIN billing_provider_tariffs AS tariff ON tariff.id = NEW.tariff_id
		          WHERE allocation_run.id = NEW.run_id
		            AND claim.tenant_id = NEW.tenant_id
		            AND claim.worker_id = allocation_run.worker_id
		            AND claim.worker_incarnation = allocation_run.worker_incarnation
		            AND claim.execution_target_id = allocation_run.execution_target_id
		            AND (
		              (NEW.charge_kind = 'request'
		                AND julianday(claim.claimed_at) >= julianday(NEW.usage_start_at)
		                AND (
		                  julianday(claim.claimed_at) < julianday(NEW.usage_end_at)
		                  OR (
			                    julianday(claim.claimed_at) = julianday(NEW.usage_end_at)
			                    AND julianday(NEW.usage_end_at) = julianday(allocation_run.usage_end_at)
			                    AND EXISTS (
			                      SELECT 1 FROM worker_incarnation_facts AS terminal_worker
			                      WHERE terminal_worker.worker_id = allocation_run.worker_id
			                        AND terminal_worker.worker_incarnation = allocation_run.worker_incarnation
			                        AND julianday(terminal_worker.terminated_at) = julianday(allocation_run.usage_end_at)
			                    )
		                  )
		                )
		                AND julianday(claim.claimed_at) >= julianday(tariff.effective_start_at)
			                AND (
			                  tariff.effective_end_at IS NULL
			                  OR julianday(claim.claimed_at) < julianday(tariff.effective_end_at)
			                  OR (
			                    julianday(claim.claimed_at) = julianday(tariff.effective_end_at)
			                    AND julianday(claim.claimed_at) = julianday(allocation_run.usage_end_at)
			                    AND julianday(NEW.usage_end_at) = julianday(allocation_run.usage_end_at)
			                    AND EXISTS (
			                      SELECT 1 FROM worker_incarnation_facts AS terminal_worker
			                      WHERE terminal_worker.worker_id = allocation_run.worker_id
			                        AND terminal_worker.worker_incarnation = allocation_run.worker_incarnation
			                        AND julianday(terminal_worker.terminated_at) = julianday(allocation_run.usage_end_at)
			                    )
			                  )
			                ))
		              OR
		              (NEW.charge_kind <> 'request'
		                AND julianday(NEW.usage_start_at) >= julianday(claim.claimed_at)
		                AND julianday(NEW.usage_end_at) <= julianday(claim_release.released_at))
			            )
			        )
			      )
			      OR EXISTS (
			        SELECT 1
			        FROM billing_shared_cost_allocation_runs AS allocation_run
			        JOIN billing_provider_tariffs AS selected_tariff ON selected_tariff.id = NEW.tariff_id
			        WHERE allocation_run.id = NEW.run_id
			          AND selected_tariff.region = ''
			          AND allocation_run.region <> ''
			          AND EXISTS (
			            SELECT 1
			            FROM billing_provider_tariffs AS regional
			            WHERE regional.provider = allocation_run.provider
			              AND regional.region = allocation_run.region
			              AND regional.currency_code = allocation_run.currency_code
			              AND (
			                (
			                  NEW.charge_kind <> 'request'
			                  AND julianday(regional.effective_start_at) < julianday(NEW.usage_end_at)
			                  AND (regional.effective_end_at IS NULL
			                    OR julianday(regional.effective_end_at) > julianday(NEW.usage_start_at))
			                )
			                OR (
			                  NEW.charge_kind = 'request'
			                  AND EXISTS (
			                    SELECT 1 FROM worker_claim_facts AS request_claim
			                    WHERE request_claim.id = NEW.claim_fact_id
			                      AND (
			                        (
			                          julianday(request_claim.claimed_at) < julianday(allocation_run.usage_end_at)
			                          AND julianday(regional.effective_start_at) <= julianday(request_claim.claimed_at)
			                          AND (regional.effective_end_at IS NULL
			                            OR julianday(regional.effective_end_at) > julianday(request_claim.claimed_at))
			                        )
			                        OR (
			                          julianday(request_claim.claimed_at) = julianday(allocation_run.usage_end_at)
			                          AND EXISTS (
			                            SELECT 1 FROM worker_incarnation_facts AS terminal_worker
			                            WHERE terminal_worker.worker_id = allocation_run.worker_id
			                              AND terminal_worker.worker_incarnation = allocation_run.worker_incarnation
			                              AND julianday(terminal_worker.terminated_at) = julianday(allocation_run.usage_end_at)
			                          )
			                          AND julianday(regional.effective_start_at) < julianday(request_claim.claimed_at)
			                          AND (regional.effective_end_at IS NULL
			                            OR julianday(regional.effective_end_at) >= julianday(request_claim.claimed_at))
			                        )
			                      )
			                  )
			                )
			              )
			          )
			      );
			 END`,
		`DROP TRIGGER IF EXISTS trg_billing_shared_target_ledger_coverages_update`,
		`CREATE TRIGGER trg_billing_shared_target_ledger_coverages_update
		 BEFORE UPDATE ON billing_shared_target_ledger_coverages
		 BEGIN SELECT RAISE(ABORT, 'billing shared allocation accounting history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_billing_shared_target_ledger_coverages_delete`,
		`CREATE TRIGGER trg_billing_shared_target_ledger_coverages_delete
		 BEFORE DELETE ON billing_shared_target_ledger_coverages
		 BEGIN SELECT RAISE(ABORT, 'billing shared allocation accounting history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_billing_shared_cost_allocation_runs_update`,
		`CREATE TRIGGER trg_billing_shared_cost_allocation_runs_update
		 BEFORE UPDATE ON billing_shared_cost_allocation_runs
		 BEGIN SELECT RAISE(ABORT, 'billing shared allocation accounting history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_billing_shared_cost_allocation_runs_delete`,
		`CREATE TRIGGER trg_billing_shared_cost_allocation_runs_delete
		 BEFORE DELETE ON billing_shared_cost_allocation_runs
		 BEGIN SELECT RAISE(ABORT, 'billing shared allocation accounting history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_billing_shared_estimated_charge_slices_update`,
		`CREATE TRIGGER trg_billing_shared_estimated_charge_slices_update
		 BEFORE UPDATE ON billing_shared_estimated_charge_slices
		 BEGIN SELECT RAISE(ABORT, 'billing shared allocation accounting history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_billing_shared_estimated_charge_slices_delete`,
		`CREATE TRIGGER trg_billing_shared_estimated_charge_slices_delete
		 BEFORE DELETE ON billing_shared_estimated_charge_slices
		 BEGIN SELECT RAISE(ABORT, 'billing shared allocation accounting history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_execution_targets_shared_accounting_restrict_delete`,
		`CREATE TRIGGER trg_execution_targets_shared_accounting_restrict_delete
		 BEFORE DELETE ON execution_targets
		 WHEN EXISTS (
		   SELECT 1 FROM billing_shared_target_ledger_coverages WHERE execution_target_id = OLD.id
		 ) OR EXISTS (
		   SELECT 1 FROM billing_shared_cost_allocation_runs WHERE execution_target_id = OLD.id
		 )
			 BEGIN SELECT RAISE(ABORT, 'billing shared allocation parent is retained'); END`,
		`DROP TRIGGER IF EXISTS trg_execution_targets_shared_accounting_restrict_update`,
		`CREATE TRIGGER trg_execution_targets_shared_accounting_restrict_update
		 BEFORE UPDATE OF tenant_id, organization_id, kind ON execution_targets
		 WHEN (
		   NEW.tenant_id IS NOT OLD.tenant_id
		   OR NEW.organization_id IS NOT OLD.organization_id
		   OR NEW.kind IS NOT OLD.kind
		 ) AND (
		   EXISTS (SELECT 1 FROM billing_shared_target_ledger_coverages WHERE execution_target_id = OLD.id)
		   OR EXISTS (SELECT 1 FROM billing_shared_cost_allocation_runs WHERE execution_target_id = OLD.id)
		 )
		 BEGIN SELECT RAISE(ABORT, 'billing shared allocation parent is retained'); END`,
		`DROP TRIGGER IF EXISTS trg_worker_incarnation_facts_shared_accounting_restrict_delete`,
		`CREATE TRIGGER trg_worker_incarnation_facts_shared_accounting_restrict_delete
		 BEFORE DELETE ON worker_incarnation_facts
		 WHEN EXISTS (
		   SELECT 1 FROM billing_shared_cost_allocation_runs
		   WHERE worker_id = OLD.worker_id AND worker_incarnation = OLD.worker_incarnation
		 )
		 BEGIN SELECT RAISE(ABORT, 'billing shared allocation parent is retained'); END`,
		`DROP TRIGGER IF EXISTS trg_worker_claim_facts_shared_accounting_restrict_delete`,
		`CREATE TRIGGER trg_worker_claim_facts_shared_accounting_restrict_delete
		 BEFORE DELETE ON worker_claim_facts
		 WHEN EXISTS (
		   SELECT 1 FROM billing_shared_estimated_charge_slices WHERE claim_fact_id = OLD.id
		 )
		 BEGIN SELECT RAISE(ABORT, 'billing shared allocation parent is retained'); END`,
		`DROP TRIGGER IF EXISTS trg_billing_provider_tariffs_shared_accounting_restrict_delete`,
		`CREATE TRIGGER trg_billing_provider_tariffs_shared_accounting_restrict_delete
		 BEFORE DELETE ON billing_provider_tariffs
		 WHEN EXISTS (
		   SELECT 1 FROM billing_shared_estimated_charge_slices WHERE tariff_id = OLD.id
		 )
		 BEGIN SELECT RAISE(ABORT, 'billing shared allocation parent is retained'); END`,
		`DROP TRIGGER IF EXISTS trg_tenants_shared_accounting_restrict_delete`,
		`CREATE TRIGGER trg_tenants_shared_accounting_restrict_delete
		 BEFORE DELETE ON tenants
		 WHEN EXISTS (
		   SELECT 1 FROM billing_shared_estimated_charge_slices WHERE tenant_id = OLD.id
		 )
		 BEGIN SELECT RAISE(ABORT, 'billing shared allocation parent is retained'); END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply sqlite shared cost allocation safety migration: %w", err)
		}
	}
	return nil
}
