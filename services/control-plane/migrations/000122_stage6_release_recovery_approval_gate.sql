CREATE OR REPLACE FUNCTION enforce_stage6_release_recovery_approval_gate()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.state = 'approved' AND OLD.state = 'ready_for_review' AND NOT EXISTS (
    SELECT 1
    FROM stage6_recovery_drills AS recovery
    WHERE recovery.candidate_record_id = NEW.id
      AND recovery.operator_tenant_id = NEW.operator_tenant_id
      AND recovery.state = 'approved'
      AND recovery.eligible_for_human_gate_review
      AND recovery.measurements_within_objectives
      AND recovery.all_restore_canaries_passed
      AND recovery.all_source_approvals_approved
      AND NOT recovery.cryptographic_signatures_verified
      AND recovery.external_authority_verification_required
  ) THEN
    RAISE EXCEPTION 'Stage 6 release approval requires an approved eligible Recovery drill bound to the candidate'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_stage6_release_recovery_approval_gate ON stage6_release_candidates;
CREATE TRIGGER trg_stage6_release_recovery_approval_gate
BEFORE UPDATE ON stage6_release_candidates
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_release_recovery_approval_gate();

COMMENT ON FUNCTION enforce_stage6_release_recovery_approval_gate() IS
  'Prevents a release candidate from becoming approved until its immutable candidate-bound Recovery drill has independently reached the internal approved state with the external authority boundary preserved. This does not declare that a production restore or external backup authority was verified.';
