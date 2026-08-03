CREATE OR REPLACE FUNCTION enforce_stage6_release_capacity_approval_gate()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.state = 'approved' AND OLD.state = 'ready_for_review' AND NOT EXISTS (
    SELECT 1
    FROM stage6_capacity_runs AS capacity
    WHERE capacity.candidate_record_id = NEW.id
      AND capacity.operator_tenant_id = NEW.operator_tenant_id
      AND capacity.state = 'approved'
      AND capacity.forecast_headroom_covered
      AND capacity.phase_coverage_complete
      AND capacity.exercise_coverage_complete
      AND capacity.measurements_within_objectives
      AND capacity.release_eligible_environment
      AND capacity.eligible_for_human_gate_review
      AND NOT capacity.cryptographic_signatures_verified
      AND capacity.external_authority_verification_required
  ) THEN
    RAISE EXCEPTION 'Stage 6 release approval requires an approved eligible Capacity run bound to the candidate'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_stage6_release_capacity_approval_gate ON stage6_release_candidates;
CREATE TRIGGER trg_stage6_release_capacity_approval_gate
BEFORE UPDATE ON stage6_release_candidates
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_release_capacity_approval_gate();

COMMENT ON FUNCTION enforce_stage6_release_capacity_approval_gate() IS
  'Prevents a release candidate from becoming approved until its immutable candidate-bound Capacity run has independently reached the internal approved state with the external environment, telemetry, signature, execution, and approver-authority boundary preserved. This does not declare a production capacity test passed.';
