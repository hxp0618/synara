package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateInternalCostGovernanceSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`CREATE INDEX IF NOT EXISTS idx_stage6_internal_cost_reviews_candidate
		 ON stage6_internal_cost_reviews (candidate_record_id, created_at DESC, id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_stage6_internal_cost_review_candidate_receipt
		 ON stage6_internal_cost_reviews (candidate_record_id, receipt_sha256)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_stage6_internal_cost_approvals_role_active
		 ON stage6_internal_cost_review_approvals (internal_cost_review_id, approval_role) WHERE superseded_at IS NULL`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_stage6_internal_cost_approvals_user_active
		 ON stage6_internal_cost_review_approvals (internal_cost_review_id, approver_user_id) WHERE superseded_at IS NULL`,
		`DROP TRIGGER IF EXISTS trg_stage6_internal_cost_reviews_insert`,
		`CREATE TRIGGER trg_stage6_internal_cost_reviews_insert BEFORE INSERT ON stage6_internal_cost_reviews BEGIN
		  SELECT RAISE(ABORT, 'invalid Stage 6 internal cost review') WHERE
		    length(NEW.receipt) NOT BETWEEN 1 AND 2097152 OR length(NEW.receipt_sha256) <> 32
		    OR NEW.receipt_size_bytes <> length(NEW.receipt) OR length(NEW.manifest_sha256) <> 32
		    OR length(NEW.migration_sha256) <> 32
		    OR NEW.receipt_schema <> 'synara.stage6-internal-cost-evidence-validation.v1'
		    OR NEW.assessment <> 'evidence-validated-not-internal-cost-approved'
		    OR json_valid(CAST(NEW.receipt AS TEXT)) <> 1
		    OR (SELECT count(*) FROM json_each(CAST(NEW.receipt AS TEXT))) <> 14
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.schemaVersion') <> NEW.receipt_schema
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.assessment') <> NEW.assessment
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.candidate.sourceCommit') <> NEW.release_commit
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.candidate.environment') <> NEW.environment_class
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.candidate.environmentId') <> NEW.environment_id
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.candidate.controlPlaneBaseUrl') <> NEW.control_plane_origin
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.candidate.migrationTail.name') <> NEW.migration_name
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.candidate.migrationTail.sha256') <> 'sha256:' || lower(hex(NEW.migration_sha256))
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.manifest.sha256') <> 'sha256:' || lower(hex(NEW.manifest_sha256))
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.period.start') <> strftime('%Y-%m-%dT%H:%M:%SZ', NEW.period_start)
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.period.end') <> strftime('%Y-%m-%dT%H:%M:%SZ', NEW.period_end)
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.validatedAt') <> strftime('%Y-%m-%dT%H:%M:%SZ', NEW.validated_at)
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.usage.executionCount') <> NEW.execution_count
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.usage.inputTokens') <> NEW.input_tokens
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.usage.outputTokens') <> NEW.output_tokens
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.usage.cachedInputTokens') <> NEW.cached_input_tokens
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.usage.cacheCreationInputTokens') <> NEW.cache_creation_input_tokens
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.usage.providerCostReportedExecutionCount') <> NEW.provider_cost_reported_execution_count
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.usage.providerCostUnavailableExecutionCount') <> NEW.provider_cost_unavailable_execution_count
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.usage.actualPlatformAllocationCount') <> NEW.actual_platform_allocation_count
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.usage.estimatedPlatformAllocationCount') <> NEW.estimated_platform_allocation_count
		    OR NEW.provider_cost_reported_execution_count + NEW.provider_cost_unavailable_execution_count <> NEW.execution_count
		    OR NEW.actual_platform_allocation_count + NEW.estimated_platform_allocation_count <> NEW.execution_count
		    OR (SELECT count(*) FROM json_each(CAST(NEW.receipt AS TEXT), '$.costs.providerByCurrency')) = 0
		    OR (SELECT count(*) FROM json_each(CAST(NEW.receipt AS TEXT), '$.costs.platformByCurrency')) = 0
		    OR (SELECT count(*) FROM json_each(CAST(NEW.receipt AS TEXT), '$.costs.knownByCurrency')) = 0
		    OR EXISTS (
		      SELECT 1 FROM (
		        SELECT key, value, type FROM json_each(CAST(NEW.receipt AS TEXT), '$.costs.providerByCurrency')
		        UNION ALL SELECT key, value, type FROM json_each(CAST(NEW.receipt AS TEXT), '$.costs.platformByCurrency')
		        UNION ALL SELECT key, value, type FROM json_each(CAST(NEW.receipt AS TEXT), '$.costs.knownByCurrency')
		      ) cost WHERE length(cost.key) <> 3 OR cost.key GLOB '*[^A-Z]*' OR cost.type <> 'integer' OR cost.value < 0
		    )
		    OR EXISTS (
		      SELECT 1 FROM (
		        SELECT key FROM json_each(CAST(NEW.receipt AS TEXT), '$.costs.providerByCurrency')
		        UNION SELECT key FROM json_each(CAST(NEW.receipt AS TEXT), '$.costs.platformByCurrency')
		        UNION SELECT key FROM json_each(CAST(NEW.receipt AS TEXT), '$.costs.knownByCurrency')
		      ) currency
		      WHERE COALESCE(json_extract(CAST(NEW.receipt AS TEXT), '$.costs.knownByCurrency.' || currency.key), -1)
		        <> COALESCE(json_extract(CAST(NEW.receipt AS TEXT), '$.costs.providerByCurrency.' || currency.key), 0)
		         + COALESCE(json_extract(CAST(NEW.receipt AS TEXT), '$.costs.platformByCurrency.' || currency.key), 0)
		    )
		    OR (SELECT count(*) FROM json_each(CAST(NEW.receipt AS TEXT), '$.evidence')) <> 4
		    OR (SELECT count(DISTINCT json_extract(value, '$.id')) FROM json_each(CAST(NEW.receipt AS TEXT), '$.evidence')) <> 4
		    OR EXISTS (
		      SELECT 1 FROM json_each(CAST(NEW.receipt AS TEXT), '$.evidence')
		      WHERE json_extract(value, '$.id') NOT IN ('usage-export', 'provider-cost-export', 'platform-allocation-export', 'reconciliation-report')
		        OR length(json_extract(value, '$.sha256')) <> 71
		        OR substr(json_extract(value, '$.sha256'), 1, 7) <> 'sha256:'
		        OR substr(json_extract(value, '$.sha256'), 8) GLOB '*[^0-9a-f]*'
		    )
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.evidenceFileCount') <> NEW.evidence_file_count
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.controls.tokenTotalsReconciled') IS NOT NEW.token_totals_reconciled
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.controls.providerCoverageComplete') IS NOT NEW.provider_coverage_complete
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.controls.actualOverridesEstimate') IS NOT NEW.actual_overrides_estimate
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.controls.currencySafeAggregation') IS NOT NEW.currency_safe_aggregation
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.controls.tenantIsolationValidated') IS NOT NEW.tenant_isolation_validated
		    OR json_extract(CAST(NEW.receipt AS TEXT), '$.controls.noPaymentDataPresent') IS NOT NEW.no_payment_data_present
		    OR NEW.token_totals_reconciled <> 1 OR NEW.provider_coverage_complete <> 1 OR NEW.actual_overrides_estimate <> 1
		    OR NEW.currency_safe_aggregation <> 1 OR NEW.tenant_isolation_validated <> 1 OR NEW.no_payment_data_present <> 1
		    OR NEW.eligible_for_human_gate_review <> 1 OR NEW.cryptographic_signatures_verified <> 0
		    OR NEW.external_source_authority_verification_required <> 1 OR NEW.state <> 'recorded' OR NEW.version <> 1
		    OR NEW.approved_at IS NOT NULL OR NEW.rejected_at IS NOT NULL
		    OR NOT EXISTS (
		      SELECT 1 FROM stage6_release_candidates candidate
		      WHERE candidate.id = NEW.candidate_record_id AND candidate.operator_tenant_id = NEW.operator_tenant_id
		        AND candidate.created_by = NEW.created_by
		        AND candidate.candidate_id = json_extract(CAST(NEW.receipt AS TEXT), '$.candidate.candidateId')
		        AND candidate.source_commit = NEW.release_commit AND candidate.environment_id = NEW.environment_id
		        AND json_extract(CAST(candidate.evidence_bundle_receipt AS TEXT), '$.candidate.environmentClass') = NEW.environment_class
		        AND json_extract(CAST(candidate.evidence_bundle_receipt AS TEXT), '$.receipts.internalCost.sha256') = 'sha256:' || lower(hex(NEW.receipt_sha256))
		        AND json_extract(CAST(candidate.evidence_bundle_receipt AS TEXT), '$.candidate.migrationTail.name') = NEW.migration_name
		        AND json_extract(CAST(candidate.evidence_bundle_receipt AS TEXT), '$.candidate.migrationTail.sha256') = 'sha256:' || lower(hex(NEW.migration_sha256))
		    );
		END`,
		`DROP TRIGGER IF EXISTS trg_stage6_internal_cost_reviews_update`,
		`CREATE TRIGGER trg_stage6_internal_cost_reviews_update BEFORE UPDATE ON stage6_internal_cost_reviews BEGIN
		  SELECT RAISE(ABORT, 'invalid Stage 6 internal cost review update') WHERE
		    NEW.id IS NOT OLD.id OR NEW.operator_tenant_id IS NOT OLD.operator_tenant_id
		    OR NEW.candidate_record_id IS NOT OLD.candidate_record_id OR NEW.receipt IS NOT OLD.receipt
		    OR NEW.receipt_sha256 IS NOT OLD.receipt_sha256 OR NEW.receipt_size_bytes <> OLD.receipt_size_bytes
		    OR NEW.receipt_schema <> OLD.receipt_schema OR NEW.assessment <> OLD.assessment
		    OR NEW.release_commit <> OLD.release_commit OR NEW.environment_class <> OLD.environment_class
		    OR NEW.environment_id <> OLD.environment_id OR NEW.manifest_sha256 IS NOT OLD.manifest_sha256
		    OR NEW.control_plane_origin <> OLD.control_plane_origin OR NEW.migration_name <> OLD.migration_name
		    OR NEW.migration_sha256 IS NOT OLD.migration_sha256 OR NEW.period_start <> OLD.period_start
		    OR NEW.period_end <> OLD.period_end OR NEW.validated_at <> OLD.validated_at
		    OR NEW.execution_count <> OLD.execution_count OR NEW.input_tokens <> OLD.input_tokens OR NEW.output_tokens <> OLD.output_tokens
		    OR NEW.cached_input_tokens <> OLD.cached_input_tokens OR NEW.cache_creation_input_tokens <> OLD.cache_creation_input_tokens
		    OR NEW.provider_cost_reported_execution_count <> OLD.provider_cost_reported_execution_count
		    OR NEW.provider_cost_unavailable_execution_count <> OLD.provider_cost_unavailable_execution_count
		    OR NEW.actual_platform_allocation_count <> OLD.actual_platform_allocation_count
		    OR NEW.estimated_platform_allocation_count <> OLD.estimated_platform_allocation_count
		    OR NEW.evidence_file_count <> OLD.evidence_file_count OR NEW.token_totals_reconciled <> OLD.token_totals_reconciled
		    OR NEW.provider_coverage_complete <> OLD.provider_coverage_complete OR NEW.actual_overrides_estimate <> OLD.actual_overrides_estimate
		    OR NEW.currency_safe_aggregation <> OLD.currency_safe_aggregation OR NEW.tenant_isolation_validated <> OLD.tenant_isolation_validated
		    OR NEW.no_payment_data_present <> OLD.no_payment_data_present OR NEW.eligible_for_human_gate_review <> OLD.eligible_for_human_gate_review
		    OR NEW.cryptographic_signatures_verified <> OLD.cryptographic_signatures_verified
		    OR NEW.external_source_authority_verification_required <> OLD.external_source_authority_verification_required
		    OR NEW.created_by IS NOT OLD.created_by OR NEW.created_at <> OLD.created_at OR NEW.version <> OLD.version + 1
		    OR OLD.state <> 'recorded' OR NEW.state NOT IN ('approved', 'rejected')
		    OR (NEW.state = 'approved' AND (
		      NEW.approved_at IS NULL OR NEW.rejected_at IS NOT NULL OR 2 <> (
		        SELECT count(*) FROM stage6_internal_cost_review_approvals
		        WHERE internal_cost_review_id = NEW.id AND decision = 'approved' AND superseded_at IS NULL
		      )
		    ))
		    OR (NEW.state = 'rejected' AND (NEW.rejected_at IS NULL OR NEW.approved_at IS NOT NULL));
		END`,
		`DROP TRIGGER IF EXISTS trg_stage6_internal_cost_approvals_insert`,
		`CREATE TRIGGER trg_stage6_internal_cost_approvals_insert BEFORE INSERT ON stage6_internal_cost_review_approvals BEGIN
		  SELECT RAISE(ABORT, 'invalid Stage 6 internal cost approval') WHERE
		    NEW.approval_role NOT IN ('operations', 'owner') OR NEW.decision NOT IN ('approved', 'rejected')
		    OR length(trim(NEW.reason)) NOT BETWEEN 20 AND 2000 OR substr(NEW.evidence_reference, 1, 8) <> 'https://'
		    OR length(NEW.evidence_sha256) <> 71 OR substr(NEW.evidence_sha256, 1, 7) <> 'sha256:'
		    OR substr(NEW.evidence_sha256, 8) GLOB '*[^0-9a-f]*'
		    OR NEW.evidence_sha256 = 'sha256:' || printf('%064d', 0)
		    OR NEW.superseded_at IS NOT NULL OR NEW.superseded_reason IS NOT NULL
		    OR NOT EXISTS (
		      SELECT 1 FROM stage6_internal_cost_reviews parent
		      JOIN stage6_governance_authority_grants authority ON authority.operator_tenant_id = parent.operator_tenant_id
		        AND authority.user_id = NEW.approver_user_id AND authority.authority_key = 'internal_cost.' || NEW.approval_role
		        AND authority.status = 'active' AND unixepoch(authority.expires_at) > unixepoch('now')
		      JOIN tenant_memberships membership ON membership.tenant_id = authority.operator_tenant_id
		        AND membership.user_id = authority.user_id AND membership.status = 'active'
		        AND membership.role IN ('owner', 'admin', 'security_admin')
		      JOIN tenants operator_tenant ON operator_tenant.id = authority.operator_tenant_id
		        AND operator_tenant.status = 'active' AND operator_tenant.deleted_at IS NULL
		      JOIN users governed_user ON governed_user.id = authority.user_id
		        AND governed_user.status = 'active' AND governed_user.deleted_at IS NULL
		      WHERE parent.id = NEW.internal_cost_review_id AND parent.operator_tenant_id = NEW.operator_tenant_id
		        AND parent.state = 'recorded' AND parent.created_by <> NEW.approver_user_id
		    );
		END`,
		`DROP TRIGGER IF EXISTS trg_stage6_internal_cost_approvals_no_update`,
		`CREATE TRIGGER trg_stage6_internal_cost_approvals_no_update BEFORE UPDATE ON stage6_internal_cost_review_approvals BEGIN SELECT RAISE(ABORT, 'Stage 6 internal cost evidence history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_internal_cost_approvals_no_delete`,
		`CREATE TRIGGER trg_stage6_internal_cost_approvals_no_delete BEFORE DELETE ON stage6_internal_cost_review_approvals BEGIN SELECT RAISE(ABORT, 'Stage 6 internal cost evidence history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_internal_cost_reviews_no_delete`,
		`CREATE TRIGGER trg_stage6_internal_cost_reviews_no_delete BEFORE DELETE ON stage6_internal_cost_reviews BEGIN SELECT RAISE(ABORT, 'Stage 6 internal cost evidence history is immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_release_billing_exercise_approval_gate`,
		`DROP TRIGGER IF EXISTS trg_stage6_release_internal_cost_approval_gate`,
		`CREATE TRIGGER trg_stage6_release_internal_cost_approval_gate
		 BEFORE UPDATE ON stage6_release_candidates
		 WHEN NEW.state IN ('approved', 'deploying', 'observing', 'released')
		 BEGIN
		   SELECT RAISE(ABORT, 'Stage 6 release transition requires an approved internal usage and cost review')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM stage6_internal_cost_reviews review
		     WHERE review.candidate_record_id = NEW.id AND review.operator_tenant_id = NEW.operator_tenant_id
		       AND review.state = 'approved' AND review.execution_count > 0
		       AND review.provider_cost_reported_execution_count + review.provider_cost_unavailable_execution_count = review.execution_count
		       AND review.actual_platform_allocation_count + review.estimated_platform_allocation_count = review.execution_count
		       AND review.token_totals_reconciled = 1 AND review.provider_coverage_complete = 1
		       AND review.actual_overrides_estimate = 1 AND review.currency_safe_aggregation = 1
		       AND review.tenant_isolation_validated = 1 AND review.no_payment_data_present = 1
		       AND review.eligible_for_human_gate_review = 1 AND review.cryptographic_signatures_verified = 0
		       AND review.external_source_authority_verification_required = 1
		       AND 2 = (
		         SELECT count(*) FROM stage6_internal_cost_review_approvals approval
		         WHERE approval.internal_cost_review_id = review.id AND approval.decision = 'approved'
		           AND approval.superseded_at IS NULL AND approval.evidence_sha256 IS NOT NULL
		           AND approval.evidence_sha256 <> 'sha256:' || printf('%064d', 0)
		       )
		   );
		 END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply SQLite Stage 6 internal cost governance safety migration: %w", err)
		}
	}
	return nil
}
