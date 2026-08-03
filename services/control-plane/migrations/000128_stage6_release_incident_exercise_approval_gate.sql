CREATE OR REPLACE FUNCTION enforce_stage6_release_incident_exercise_approval_gate()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.state = 'approved' AND OLD.state = 'ready_for_review' AND NOT EXISTS (
    SELECT 1
    FROM stage6_incident_exercises AS exercise
    WHERE exercise.candidate_record_id = NEW.id
      AND exercise.operator_tenant_id = NEW.operator_tenant_id
      AND exercise.state = 'approved'
      AND exercise.independent_status_page_declared
      AND exercise.role_separation_complete
      AND exercise.paging_exercise_complete
      AND exercise.status_page_components_complete
      AND exercise.public_timeline_within_targets
      AND exercise.subscriber_delivery_complete
      AND exercise.recovery_verification_complete
      AND exercise.review_complete
      AND exercise.release_eligible_environment
      AND exercise.eligible_for_human_gate_review
      AND NOT exercise.cryptographic_signatures_verified
      AND exercise.external_authority_verification_required
  ) THEN
    RAISE EXCEPTION 'Stage 6 release approval requires an approved eligible Incident exercise bound to the candidate'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_stage6_release_incident_exercise_approval_gate ON stage6_release_candidates;
CREATE TRIGGER trg_stage6_release_incident_exercise_approval_gate
BEFORE UPDATE ON stage6_release_candidates
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_release_incident_exercise_approval_gate();

COMMENT ON FUNCTION enforce_stage6_release_incident_exercise_approval_gate() IS
  'Prevents a release candidate from becoming approved until its immutable candidate-bound paging and public-communication exercise has independently reached the internal approved state with the external Status Page, paging, subscriber delivery, signature, execution, and approver-authority boundary preserved. This does not declare production operations ready.';
