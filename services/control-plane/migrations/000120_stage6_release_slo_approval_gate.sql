CREATE OR REPLACE FUNCTION enforce_stage6_release_slo_approval_gate()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.state = 'approved' AND OLD.state = 'ready_for_review' AND NOT EXISTS (
    SELECT 1
    FROM stage6_slo_windows AS slo
    WHERE slo.candidate_record_id = NEW.id
      AND slo.operator_tenant_id = NEW.operator_tenant_id
      AND slo.state = 'approved'
      AND slo.eligible_for_human_gate_review
      AND slo.all_objectives_assessable
      AND slo.all_objectives_met
  ) THEN
    RAISE EXCEPTION 'Stage 6 release approval requires an approved eligible SLO window bound to the candidate'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_stage6_release_slo_approval_gate ON stage6_release_candidates;
CREATE TRIGGER trg_stage6_release_slo_approval_gate
BEFORE UPDATE ON stage6_release_candidates
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_release_slo_approval_gate();

COMMENT ON FUNCTION enforce_stage6_release_slo_approval_gate() IS
  'Prevents a release candidate from becoming approved until its immutable candidate-bound SLO window has independently reached the internal approved state. This does not declare that a production SLO was achieved.';
