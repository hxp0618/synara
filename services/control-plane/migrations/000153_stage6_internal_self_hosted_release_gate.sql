DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM stage6_release_candidates
    WHERE evidence_receipt_bound
      AND evidence_bundle_schema IN (
        'synara.stage6-candidate-evidence-bundle-validation.v2',
        'synara.stage6-candidate-evidence-bundle-validation.v3'
      )
      AND state NOT IN ('released', 'rejected', 'rolled_back')
  ) THEN
    RAISE EXCEPTION 'Stage 6 internal self-hosted candidate v4 migration requires every active commercial candidate to be replaced or closed'
      USING ERRCODE = '23514';
  END IF;
END;
$$;

DO $$
DECLARE
  definition TEXT;
BEGIN
  SELECT pg_get_functiondef('enforce_stage6_release_final_review_insert()'::regprocedure) INTO definition;
  IF position('billing-settlement' IN definition) = 0
     OR position('''engineering'', ''product'', ''finance'', ''operations''' IN definition) = 0 THEN
    RAISE EXCEPTION 'Unexpected Stage 6 final review function baseline' USING ERRCODE = '23514';
  END IF;
  definition := replace(definition, 'billing-settlement', 'internal-cost-reconciliation');
  definition := replace(
    definition,
    'sha256:d48fb91c4934e284c87c445ef2c8b2ea7fdcb0ac086e1ea2cbc10738b3512cd8',
    'sha256:8ea250269216ebcee540e7a30eef3e2c3c275694d9ab808aeeaa0b1b6efefced'
  );
  definition := replace(
    definition,
    '''engineering'', ''product'', ''finance'', ''operations''',
    '''engineering'', ''product'', ''operations'', ''operations'''
  );
  EXECUTE definition;
END;
$$;

ALTER TABLE stage6_release_candidates
  DROP CONSTRAINT IF EXISTS chk_stage6_release_candidate_evidence_binding;
ALTER TABLE stage6_release_candidates
  ADD CONSTRAINT chk_stage6_release_candidate_evidence_binding CHECK (
    (NOT evidence_receipt_bound
      AND evidence_bundle_receipt IS NULL
      AND evidence_bundle_schema IS NULL
      AND evidence_bundle_assessment IS NULL
      AND evidence_bundle_validated_at IS NULL
      AND evidence_bundle_receipt_size_bytes IS NULL
      AND desktop_artifact_set_sha256 IS NULL)
    OR
    (evidence_receipt_bound
      AND evidence_bundle_receipt IS NOT NULL
      AND (
        evidence_bundle_schema = 'synara.stage6-candidate-evidence-bundle-validation.v4'
        OR (
          evidence_bundle_schema IN (
            'synara.stage6-candidate-evidence-bundle-validation.v2',
            'synara.stage6-candidate-evidence-bundle-validation.v3'
          )
          AND state IN ('released', 'rejected', 'rolled_back')
        )
      )
      AND evidence_bundle_assessment = 'evidence-consistent-not-ga-approved'
      AND evidence_bundle_validated_at IS NOT NULL
      AND evidence_bundle_receipt_size_bytes BETWEEN 1 AND 32768
      AND octet_length(evidence_bundle_receipt) = evidence_bundle_receipt_size_bytes
      AND desktop_artifact_set_sha256 ~ '^sha256:[0-9a-f]{64}$')
  );

DO $$
DECLARE
  definition TEXT;
BEGIN
  SELECT pg_get_functiondef('enforce_stage6_release_candidate_transition()'::regprocedure) INTO definition;
  IF position('synara.stage6-candidate-evidence-bundle-validation.v3' IN definition) = 0
     OR position('''billing'', ''capacity''' IN definition) = 0 THEN
    RAISE EXCEPTION 'Unexpected Stage 6 candidate transition function baseline' USING ERRCODE = '23514';
  END IF;
  definition := replace(
    definition,
    'synara.stage6-candidate-evidence-bundle-validation.v3',
    'synara.stage6-candidate-evidence-bundle-validation.v4'
  );
  definition := replace(definition, '''billing'', ''capacity''', '''internalCost'', ''capacity''');
  definition := replace(definition, 'billing_commercial', 'internal_cost');
  definition := replace(
    definition,
    '''provider_commercial'', ''internal_cost'', ''personal_data''',
    '''provider_commercial'', ''personal_data'''
  );
  EXECUTE definition;

  SELECT pg_get_functiondef('enforce_stage6_release_receipt_projection()'::regprocedure) INTO definition;
  IF position('(''billing'', ''synara.stage6-stripe-billing-exercise-validation.v1'', ''evidence-validated-not-billing-passed'')' IN definition) = 0 THEN
    RAISE EXCEPTION 'Unexpected Stage 6 projected receipt function baseline' USING ERRCODE = '23514';
  END IF;
  definition := replace(
    definition,
    '(''billing'', ''synara.stage6-stripe-billing-exercise-validation.v1'', ''evidence-validated-not-billing-passed'')',
    '(''internalCost'', ''synara.stage6-internal-cost-evidence-validation.v1'', ''evidence-validated-not-internal-cost-approved'')'
  );
  EXECUTE definition;
END;
$$;

CREATE OR REPLACE FUNCTION enforce_stage6_release_internal_cost_approval_gate()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM stage6_internal_cost_reviews AS review
    WHERE review.candidate_record_id = NEW.id
      AND review.operator_tenant_id = NEW.operator_tenant_id
      AND review.state = 'approved'
      AND review.execution_count > 0
      AND review.provider_cost_reported_execution_count + review.provider_cost_unavailable_execution_count = review.execution_count
      AND review.actual_platform_allocation_count + review.estimated_platform_allocation_count = review.execution_count
      AND review.token_totals_reconciled
      AND review.provider_coverage_complete
      AND review.actual_overrides_estimate
      AND review.currency_safe_aggregation
      AND review.tenant_isolation_validated
      AND review.no_payment_data_present
      AND review.eligible_for_human_gate_review
      AND NOT review.cryptographic_signatures_verified
      AND review.external_source_authority_verification_required
      AND 2 = (
        SELECT count(*) FROM stage6_internal_cost_review_approvals AS approval
        WHERE approval.internal_cost_review_id = review.id
          AND approval.decision = 'approved'
          AND approval.superseded_at IS NULL
          AND approval.evidence_sha256 IS NOT NULL
          AND approval.evidence_sha256 <> 'sha256:' || repeat('0', 64)
      )
  ) THEN
    RAISE EXCEPTION 'Stage 6 release transition requires an approved internal usage and cost review with two byte-bound decisions'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_stage6_release_billing_exercise_approval_gate ON stage6_release_candidates;
DROP TRIGGER IF EXISTS trg_stage6_release_internal_cost_approval_gate ON stage6_release_candidates;
CREATE TRIGGER trg_stage6_release_internal_cost_approval_gate
BEFORE INSERT OR UPDATE ON stage6_release_candidates
FOR EACH ROW
WHEN (NEW.state IN ('approved', 'deploying', 'observing', 'released'))
EXECUTE FUNCTION enforce_stage6_release_internal_cost_approval_gate();

COMMENT ON COLUMN stage6_release_candidates.evidence_bundle_receipt IS
  'Exact bounded UTF-8 internal-self-hosted v4 candidate-evidence receipt bytes; terminal v2/v3 rows remain historical only.';
COMMENT ON FUNCTION enforce_stage6_release_internal_cost_approval_gate() IS
  'Replaces the historical Stripe exercise release gate with exact-candidate Token, Provider cost and actual-over-estimated platform allocation review.';
