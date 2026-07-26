package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateSharedActualAllocationSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`CREATE INDEX IF NOT EXISTS idx_billing_shared_actual_allocation_runs_target_period
		 ON billing_shared_actual_allocation_runs (
		   execution_target_id, billing_period_start_at, billing_period_end_at, id
		 )`,
		`CREATE INDEX IF NOT EXISTS idx_billing_shared_actual_allocation_lines_run
		 ON billing_shared_actual_allocation_lines (run_id, actual_invoice_line_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_billing_shared_actual_allocation_lines_actual
		 ON billing_shared_actual_allocation_lines (actual_invoice_line_id)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_shared_actual_charge_slices_line
		 ON billing_shared_actual_charge_slices (allocation_line_id, estimated_slice_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_billing_shared_actual_charge_slices_estimate
		 ON billing_shared_actual_charge_slices (estimated_slice_id)`,
		`CREATE INDEX IF NOT EXISTS idx_billing_shared_actual_charge_slices_tenant
		 ON billing_shared_actual_charge_slices (tenant_id, allocation_kind, id)`,
		`DROP TRIGGER IF EXISTS trg_billing_shared_actual_allocation_runs_insert`,
		`CREATE TRIGGER trg_billing_shared_actual_allocation_runs_insert
		 BEFORE INSERT ON billing_shared_actual_allocation_runs
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid billing shared actual allocation run')
		   WHERE NEW.provider NOT GLOB '[a-z0-9]*'
		      OR length(NEW.provider) NOT BETWEEN 1 AND 64
		      OR NEW.provider GLOB '*[^a-z0-9_-]*'
		      OR length(NEW.currency_code) <> 3 OR NEW.currency_code GLOB '*[^A-Z]*'
		      OR NEW.algorithm_version <> 'proportional-shared-estimate-v1'
		      OR julianday(NEW.billing_period_end_at) <= julianday(NEW.billing_period_start_at)
		      OR julianday(NEW.created_at) < julianday(NEW.billing_period_end_at)
		      OR length(NEW.source_checksum) <> 64 OR NEW.source_checksum GLOB '*[^0-9a-f]*'
		      OR length(NEW.source_scope_attestation_sha256) <> 64
		      OR NEW.source_scope_attestation_sha256 GLOB '*[^0-9a-f]*'
		      OR length(NEW.source_line_set_sha256) <> 64
		      OR NEW.source_line_set_sha256 GLOB '*[^0-9a-f]*'
		      OR NEW.import_line_count < NEW.source_line_count OR NEW.source_line_count <= 0
		      OR NEW.unallocated_line_count <> NEW.import_line_count - NEW.source_line_count
		      OR NEW.allocation_line_count <> NEW.source_line_count
		      OR NEW.allocation_slice_count < NEW.allocation_line_count
		      OR NEW.import_amount_micros <> NEW.source_amount_micros + NEW.unallocated_amount_micros
		      OR NEW.allocated_amount_micros <> NEW.source_amount_micros
		      OR NEW.state <> 'building' OR NEW.sealed_at IS NOT NULL
		      OR NOT EXISTS (
		        SELECT 1
		        FROM billing_actual_invoice_imports AS invoice_import
		        JOIN billing_shared_target_ledger_coverages AS coverage
		          ON coverage.id = NEW.ledger_coverage_id
		        JOIN execution_targets AS target
		          ON target.id = NEW.execution_target_id
		        WHERE invoice_import.tenant_id = NEW.operator_tenant_id
		          AND invoice_import.id = NEW.invoice_import_id
		          AND invoice_import.provider = NEW.provider
		          AND invoice_import.currency_code = NEW.currency_code
		          AND julianday(invoice_import.billing_period_start_at) = julianday(NEW.billing_period_start_at)
		          AND julianday(invoice_import.billing_period_end_at) = julianday(NEW.billing_period_end_at)
		          AND invoice_import.source_checksum = NEW.source_checksum
		          AND coverage.execution_target_id = NEW.execution_target_id
		          AND target.tenant_id IS NULL
		      );
		 END`,
		`DROP TRIGGER IF EXISTS trg_billing_shared_actual_allocation_lines_insert`,
		`CREATE TRIGGER trg_billing_shared_actual_allocation_lines_insert
		 BEFORE INSERT ON billing_shared_actual_allocation_lines
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid billing shared actual allocation line')
		   WHERE NEW.estimated_amount_micros < 0
		      OR NEW.allocated_amount_micros <> NEW.source_amount_micros
		      OR NEW.estimated_slice_count <= 0
		      OR length(NEW.estimated_slice_set_sha256) <> 64
		      OR NEW.estimated_slice_set_sha256 GLOB '*[^0-9a-f]*'
		      OR NOT EXISTS (
		        SELECT 1
		        FROM billing_shared_actual_allocation_runs AS allocation_run
		        JOIN billing_actual_invoice_lines AS actual_line
		          ON actual_line.id = NEW.actual_invoice_line_id
		        WHERE allocation_run.id = NEW.run_id
		          AND allocation_run.state = 'building'
		          AND actual_line.tenant_id = allocation_run.operator_tenant_id
		          AND actual_line.invoice_import_id = allocation_run.invoice_import_id
		          AND actual_line.provider = allocation_run.provider
		          AND actual_line.currency_code = allocation_run.currency_code
		          AND julianday(actual_line.billing_period_start_at) = julianday(allocation_run.billing_period_start_at)
		          AND julianday(actual_line.billing_period_end_at) = julianday(allocation_run.billing_period_end_at)
		          AND actual_line.amount_micros = NEW.source_amount_micros
		      )
		      OR NOT EXISTS (
		        SELECT 1
		        FROM billing_shared_actual_allocation_runs AS allocation_run
		        JOIN billing_actual_invoice_lines AS actual_line
		          ON actual_line.id = NEW.actual_invoice_line_id
		        JOIN billing_shared_estimated_charge_slices AS estimate_slice
		          ON estimate_slice.charge_kind = actual_line.charge_kind
		         AND estimate_slice.resource_correlation_key = actual_line.resource_correlation_key
		         AND julianday(estimate_slice.billing_period_start_at) = julianday(actual_line.billing_period_start_at)
		         AND julianday(estimate_slice.billing_period_end_at) = julianday(actual_line.billing_period_end_at)
		        JOIN billing_shared_cost_allocation_runs AS estimate_run
		          ON estimate_run.id = estimate_slice.run_id
		         AND estimate_run.provider = actual_line.provider
		         AND estimate_run.currency_code = actual_line.currency_code
		        WHERE allocation_run.id = NEW.run_id
		          AND estimate_run.execution_target_id = allocation_run.execution_target_id
		      );
		 END`,
		`DROP TRIGGER IF EXISTS trg_billing_shared_actual_charge_slices_insert`,
		`CREATE TRIGGER trg_billing_shared_actual_charge_slices_insert
		 BEFORE INSERT ON billing_shared_actual_charge_slices
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid billing shared actual charge slice')
		   WHERE NEW.allocation_kind NOT IN ('tenant-claim', 'platform-idle')
		      OR NEW.estimate_weight_micros < 0
		      OR NOT (
		        (NEW.allocation_kind = 'tenant-claim' AND NEW.tenant_id IS NOT NULL)
		        OR (NEW.allocation_kind = 'platform-idle' AND NEW.tenant_id IS NULL)
		      )
		      OR NOT EXISTS (
		        SELECT 1
		        FROM billing_shared_actual_allocation_lines AS allocation_line
		        JOIN billing_shared_actual_allocation_runs AS allocation_run
		          ON allocation_run.id = allocation_line.run_id
		        JOIN billing_actual_invoice_lines AS actual_line
		          ON actual_line.id = allocation_line.actual_invoice_line_id
		        JOIN billing_shared_estimated_charge_slices AS estimate_slice
		          ON estimate_slice.id = NEW.estimated_slice_id
		        JOIN billing_shared_cost_allocation_runs AS estimate_run
		          ON estimate_run.id = estimate_slice.run_id
		        WHERE allocation_line.id = NEW.allocation_line_id
		          AND allocation_run.state = 'building'
		          AND estimate_run.execution_target_id = allocation_run.execution_target_id
		          AND estimate_run.provider = actual_line.provider
		          AND estimate_run.currency_code = actual_line.currency_code
		          AND julianday(estimate_run.billing_period_start_at) = julianday(actual_line.billing_period_start_at)
		          AND julianday(estimate_run.billing_period_end_at) = julianday(actual_line.billing_period_end_at)
		          AND estimate_slice.charge_kind = actual_line.charge_kind
		          AND estimate_slice.resource_correlation_key = actual_line.resource_correlation_key
		          AND estimate_slice.tenant_id IS NEW.tenant_id
		          AND estimate_slice.allocation_kind = NEW.allocation_kind
		          AND estimate_slice.amount_micros = NEW.estimate_weight_micros
		      );
		 END`,
		`DROP TRIGGER IF EXISTS trg_billing_shared_actual_allocation_runs_seal`,
		`DROP TRIGGER IF EXISTS trg_billing_actual_invoice_lines_shared_allocation_fence`,
		`CREATE TRIGGER trg_billing_actual_invoice_lines_shared_allocation_fence
		 BEFORE INSERT ON billing_actual_invoice_lines
		 WHEN EXISTS (
		   SELECT 1
		   FROM billing_shared_actual_allocation_runs AS allocation_run
		   WHERE allocation_run.operator_tenant_id = NEW.tenant_id
		     AND allocation_run.invoice_import_id = NEW.invoice_import_id
		     AND allocation_run.state = 'sealed'
		 )
		 BEGIN
		   SELECT RAISE(ABORT, 'billing actual invoice import is sealed by shared allocation');
		 END`,
		`DROP TRIGGER IF EXISTS trg_billing_shared_estimated_charge_slices_actual_allocation_fence`,
		`CREATE TRIGGER trg_billing_shared_estimated_charge_slices_actual_allocation_fence
		 BEFORE INSERT ON billing_shared_estimated_charge_slices
		 WHEN EXISTS (
		   SELECT 1
		   FROM billing_shared_cost_allocation_runs AS estimate_run
		   JOIN billing_shared_actual_allocation_lines AS allocation_line
		   JOIN billing_shared_actual_allocation_runs AS allocation_run
		     ON allocation_run.id = allocation_line.run_id
		    AND allocation_run.state = 'sealed'
		   JOIN billing_actual_invoice_lines AS actual_line
		     ON actual_line.id = allocation_line.actual_invoice_line_id
		   WHERE estimate_run.id = NEW.run_id
		     AND actual_line.provider = estimate_run.provider
		     AND actual_line.currency_code = estimate_run.currency_code
		     AND julianday(actual_line.billing_period_start_at) = julianday(NEW.billing_period_start_at)
		     AND julianday(actual_line.billing_period_end_at) = julianday(NEW.billing_period_end_at)
		     AND actual_line.charge_kind = NEW.charge_kind
		     AND actual_line.resource_correlation_key = NEW.resource_correlation_key
		 )
		 BEGIN
		   SELECT RAISE(ABORT, 'billing shared estimate scope is sealed by actual allocation');
		 END`,
		`CREATE TRIGGER trg_billing_shared_actual_allocation_runs_seal
		 BEFORE UPDATE ON billing_shared_actual_allocation_runs
		 BEGIN
		   SELECT RAISE(ABORT, 'billing shared actual allocation run transition is invalid')
		   WHERE OLD.state <> 'building' OR NEW.state <> 'sealed'
		      OR NEW.sealed_at IS NULL OR julianday(NEW.sealed_at) < julianday(NEW.created_at)
		      OR NEW.id IS NOT OLD.id
		      OR NEW.operator_tenant_id IS NOT OLD.operator_tenant_id
		      OR NEW.invoice_import_id IS NOT OLD.invoice_import_id
		      OR NEW.execution_target_id IS NOT OLD.execution_target_id
		      OR NEW.ledger_coverage_id IS NOT OLD.ledger_coverage_id
		      OR NEW.provider IS NOT OLD.provider
		      OR NEW.currency_code IS NOT OLD.currency_code
		      OR NEW.billing_period_start_at IS NOT OLD.billing_period_start_at
		      OR NEW.billing_period_end_at IS NOT OLD.billing_period_end_at
		      OR NEW.algorithm_version IS NOT OLD.algorithm_version
		      OR NEW.source_checksum IS NOT OLD.source_checksum
		      OR NEW.source_scope_attestation_sha256 IS NOT OLD.source_scope_attestation_sha256
		      OR NEW.source_line_set_sha256 IS NOT OLD.source_line_set_sha256
		      OR NEW.import_line_count IS NOT OLD.import_line_count
		      OR NEW.import_amount_micros IS NOT OLD.import_amount_micros
		      OR NEW.source_line_count IS NOT OLD.source_line_count
		      OR NEW.source_amount_micros IS NOT OLD.source_amount_micros
		      OR NEW.unallocated_line_count IS NOT OLD.unallocated_line_count
		      OR NEW.unallocated_amount_micros IS NOT OLD.unallocated_amount_micros
		      OR NEW.allocation_line_count IS NOT OLD.allocation_line_count
		      OR NEW.allocation_slice_count IS NOT OLD.allocation_slice_count
		      OR NEW.allocated_amount_micros IS NOT OLD.allocated_amount_micros
		      OR NEW.created_by IS NOT OLD.created_by
		      OR NEW.created_at IS NOT OLD.created_at;

		   SELECT RAISE(ABORT, 'billing shared actual allocation conservation is invalid')
		   WHERE NEW.import_line_count <> (
		       SELECT count(*) FROM billing_actual_invoice_lines
		       WHERE tenant_id = NEW.operator_tenant_id AND invoice_import_id = NEW.invoice_import_id
		     )
		      OR NEW.import_amount_micros <> COALESCE((
		       SELECT sum(amount_micros) FROM billing_actual_invoice_lines
		       WHERE tenant_id = NEW.operator_tenant_id AND invoice_import_id = NEW.invoice_import_id
		     ), 0)
		      OR NEW.allocation_line_count <> (
		       SELECT count(*) FROM billing_shared_actual_allocation_lines WHERE run_id = NEW.id
		     )
		      OR NEW.source_amount_micros <> COALESCE((
		       SELECT sum(source_amount_micros) FROM billing_shared_actual_allocation_lines WHERE run_id = NEW.id
		     ), 0)
		      OR NEW.allocated_amount_micros <> COALESCE((
		       SELECT sum(allocated_amount_micros) FROM billing_shared_actual_allocation_lines WHERE run_id = NEW.id
		     ), 0)
		      OR NEW.allocation_slice_count <> (
		       SELECT count(*)
		       FROM billing_shared_actual_charge_slices AS actual_slice
		       JOIN billing_shared_actual_allocation_lines AS allocation_line
		         ON allocation_line.id = actual_slice.allocation_line_id
		       WHERE allocation_line.run_id = NEW.id
		     )
		      OR EXISTS (
		       SELECT 1
		       FROM billing_shared_actual_allocation_lines AS allocation_line
		       LEFT JOIN billing_shared_actual_charge_slices AS actual_slice
		         ON actual_slice.allocation_line_id = allocation_line.id
		       WHERE allocation_line.run_id = NEW.id
		       GROUP BY allocation_line.id
		       HAVING count(actual_slice.id) <> allocation_line.estimated_slice_count
		          OR COALESCE(sum(actual_slice.estimate_weight_micros), 0) <> allocation_line.estimated_amount_micros
		          OR COALESCE(sum(actual_slice.amount_micros), 0) <> allocation_line.allocated_amount_micros
		     )
		      OR EXISTS (
		       SELECT 1
		       FROM billing_actual_invoice_lines AS actual_line
		       JOIN billing_shared_estimated_charge_slices AS estimate_slice
		         ON estimate_slice.charge_kind = actual_line.charge_kind
		        AND estimate_slice.resource_correlation_key = actual_line.resource_correlation_key
		        AND julianday(estimate_slice.billing_period_start_at) = julianday(actual_line.billing_period_start_at)
		        AND julianday(estimate_slice.billing_period_end_at) = julianday(actual_line.billing_period_end_at)
		       JOIN billing_shared_cost_allocation_runs AS estimate_run
		         ON estimate_run.id = estimate_slice.run_id
		        AND estimate_run.provider = actual_line.provider
		        AND estimate_run.currency_code = actual_line.currency_code
		       WHERE actual_line.tenant_id = NEW.operator_tenant_id
		         AND actual_line.invoice_import_id = NEW.invoice_import_id
		         AND estimate_run.execution_target_id = NEW.execution_target_id
		         AND NOT EXISTS (
		           SELECT 1 FROM billing_shared_actual_allocation_lines AS allocation_line
		           WHERE allocation_line.run_id = NEW.id
		             AND allocation_line.actual_invoice_line_id = actual_line.id
		         )
		     )
		      OR EXISTS (
		       SELECT 1
		       FROM billing_shared_actual_allocation_lines AS allocation_line
		       JOIN billing_actual_invoice_lines AS actual_line
		         ON actual_line.id = allocation_line.actual_invoice_line_id
		       JOIN billing_shared_estimated_charge_slices AS estimate_slice
		         ON estimate_slice.charge_kind = actual_line.charge_kind
		        AND estimate_slice.resource_correlation_key = actual_line.resource_correlation_key
		        AND julianday(estimate_slice.billing_period_start_at) = julianday(actual_line.billing_period_start_at)
		        AND julianday(estimate_slice.billing_period_end_at) = julianday(actual_line.billing_period_end_at)
		       JOIN billing_shared_cost_allocation_runs AS estimate_run
		         ON estimate_run.id = estimate_slice.run_id
		        AND estimate_run.provider = actual_line.provider
		        AND estimate_run.currency_code = actual_line.currency_code
		       WHERE allocation_line.run_id = NEW.id
		       GROUP BY actual_line.id
		       HAVING count(DISTINCT estimate_run.execution_target_id) > 1
		     );
		 END`,
		`DROP TRIGGER IF EXISTS trg_billing_shared_actual_allocation_runs_delete`,
		`CREATE TRIGGER trg_billing_shared_actual_allocation_runs_delete
		 BEFORE DELETE ON billing_shared_actual_allocation_runs
		 BEGIN SELECT RAISE(ABORT, 'billing shared actual allocation history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_billing_shared_actual_allocation_lines_update`,
		`CREATE TRIGGER trg_billing_shared_actual_allocation_lines_update
		 BEFORE UPDATE ON billing_shared_actual_allocation_lines
		 BEGIN SELECT RAISE(ABORT, 'billing shared actual allocation history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_billing_shared_actual_allocation_lines_delete`,
		`CREATE TRIGGER trg_billing_shared_actual_allocation_lines_delete
		 BEFORE DELETE ON billing_shared_actual_allocation_lines
		 BEGIN SELECT RAISE(ABORT, 'billing shared actual allocation history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_billing_shared_actual_charge_slices_update`,
		`CREATE TRIGGER trg_billing_shared_actual_charge_slices_update
		 BEFORE UPDATE ON billing_shared_actual_charge_slices
		 BEGIN SELECT RAISE(ABORT, 'billing shared actual allocation history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_billing_shared_actual_charge_slices_delete`,
		`CREATE TRIGGER trg_billing_shared_actual_charge_slices_delete
		 BEFORE DELETE ON billing_shared_actual_charge_slices
		 BEGIN SELECT RAISE(ABORT, 'billing shared actual allocation history is immutable'); END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply sqlite shared actual allocation safety migration: %w", err)
		}
	}
	return nil
}
