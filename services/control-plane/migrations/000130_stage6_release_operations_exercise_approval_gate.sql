CREATE OR REPLACE FUNCTION enforce_stage6_release_operations_exercise_approval_gate()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.state = 'approved' AND OLD.state = 'ready_for_review' AND NOT EXISTS (
    SELECT 1
    FROM stage6_operations_exercises AS exercise
    WHERE exercise.candidate_record_id = NEW.id
      AND exercise.operator_tenant_id = NEW.operator_tenant_id
      AND exercise.state = 'approved'
      AND exercise.all_operations_passed
      AND exercise.all_negative_authorizations_denied
      AND exercise.no_developer_fallbacks
      AND exercise.production_authentication_declared
      AND exercise.support_lifecycle_complete
      AND exercise.receipt_approvals_complete
      AND exercise.release_eligible_environment
      AND exercise.eligible_for_human_gate_review
      AND NOT exercise.cryptographic_signatures_verified
      AND exercise.external_authority_verification_required
  ) THEN
    RAISE EXCEPTION 'Stage 6 release approval requires an approved eligible Operations exercise bound to the candidate'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_stage6_release_operations_exercise_approval_gate ON stage6_release_candidates;
CREATE TRIGGER trg_stage6_release_operations_exercise_approval_gate
BEFORE UPDATE ON stage6_release_candidates
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_release_operations_exercise_approval_gate();

COMMENT ON FUNCTION enforce_stage6_release_operations_exercise_approval_gate() IS
  'Prevents a release candidate from becoming approved until its immutable candidate-bound browser operations exercise has independently reached the internal approved state with the external deployment, browser session, Audit, evidence, signature, execution, and approver-authority boundary preserved. This does not declare production operations passed.';
