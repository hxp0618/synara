CREATE OR REPLACE FUNCTION enforce_stage6_release_penetration_approval_gate()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.state = 'approved' AND OLD.state = 'ready_for_review' AND NOT EXISTS (
    SELECT 1
    FROM stage6_penetration_engagements AS penetration
    WHERE penetration.candidate_record_id = NEW.id
      AND penetration.operator_tenant_id = NEW.operator_tenant_id
      AND penetration.state = 'approved'
      AND penetration.third_party_independence_declared
      AND penetration.stage5_dependency_satisfied
      AND penetration.asset_coverage_complete
      AND penetration.scope_coverage_complete
      AND penetration.methodology_coverage_complete
      AND penetration.no_unaccepted_high_or_critical_findings
      AND penetration.eligible_for_human_gate_review
      AND NOT penetration.cryptographic_signatures_verified
      AND penetration.external_authority_verification_required
  ) THEN
    RAISE EXCEPTION 'Stage 6 release approval requires an approved eligible Penetration engagement bound to the candidate'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_stage6_release_penetration_approval_gate ON stage6_release_candidates;
CREATE TRIGGER trg_stage6_release_penetration_approval_gate
BEFORE UPDATE ON stage6_release_candidates
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_release_penetration_approval_gate();

COMMENT ON FUNCTION enforce_stage6_release_penetration_approval_gate() IS
  'Prevents a release candidate from becoming approved until its immutable candidate-bound Penetration engagement has independently reached the internal approved state with the external assessor, report, signature and execution boundary preserved. This does not declare a third-party test passed.';
