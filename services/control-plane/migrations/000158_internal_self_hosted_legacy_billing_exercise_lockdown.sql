DROP TRIGGER IF EXISTS trg_stage6_billing_exercises_guard ON stage6_billing_exercises;
DROP TRIGGER IF EXISTS trg_stage6_billing_exercises_no_delete ON stage6_billing_exercises;
DROP TRIGGER IF EXISTS trg_stage6_billing_exercise_approvals_insert ON stage6_billing_exercise_approvals;
DROP TRIGGER IF EXISTS trg_stage6_billing_exercise_approvals_no_update ON stage6_billing_exercise_approvals;
DROP TRIGGER IF EXISTS trg_stage6_billing_exercise_approvals_no_delete ON stage6_billing_exercise_approvals;
DROP TRIGGER IF EXISTS trg_stage6_release_billing_exercise_approval_gate ON stage6_release_candidates;

CREATE OR REPLACE FUNCTION reject_legacy_billing_exercise_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'historical Billing exercise state is read-only in the internal-self-hosted product'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER trg_legacy_billing_exercises_no_insert
BEFORE INSERT ON stage6_billing_exercises
FOR EACH ROW EXECUTE FUNCTION reject_legacy_billing_exercise_mutation();
CREATE TRIGGER trg_legacy_billing_exercises_no_update
BEFORE UPDATE ON stage6_billing_exercises
FOR EACH ROW EXECUTE FUNCTION reject_legacy_billing_exercise_mutation();
CREATE TRIGGER trg_legacy_billing_exercises_no_delete
BEFORE DELETE ON stage6_billing_exercises
FOR EACH ROW EXECUTE FUNCTION reject_legacy_billing_exercise_mutation();

CREATE TRIGGER trg_legacy_billing_exercise_approvals_no_insert
BEFORE INSERT ON stage6_billing_exercise_approvals
FOR EACH ROW EXECUTE FUNCTION reject_legacy_billing_exercise_mutation();
CREATE TRIGGER trg_legacy_billing_exercise_approvals_no_update
BEFORE UPDATE ON stage6_billing_exercise_approvals
FOR EACH ROW EXECUTE FUNCTION reject_legacy_billing_exercise_mutation();
CREATE TRIGGER trg_legacy_billing_exercise_approvals_no_delete
BEFORE DELETE ON stage6_billing_exercise_approvals
FOR EACH ROW EXECUTE FUNCTION reject_legacy_billing_exercise_mutation();

COMMENT ON TABLE stage6_billing_exercises IS
  'Historical Migration 000131 audit compatibility only; current internal-self-hosted runtime cannot create or mutate Billing exercise state.';
COMMENT ON TABLE stage6_billing_exercise_approvals IS
  'Historical Migration 000131 audit compatibility only; current internal-self-hosted runtime cannot create or mutate Billing exercise approvals.';
