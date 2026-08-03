ALTER TABLE stage6_operations_exercises
  ALTER COLUMN matrix_profile SET DEFAULT 'internal-self-hosted-v4';

ALTER TABLE stage6_operations_exercises
  DROP CONSTRAINT IF EXISTS stage6_operations_exercises_matrix_profile_check;

ALTER TABLE stage6_operations_exercises
  ADD CONSTRAINT stage6_operations_exercises_matrix_profile_check CHECK (
    (matrix_profile = 'legacy-commercial-v2' AND operation_count = 48 AND evidence_file_count = 99)
    OR (matrix_profile = 'internal-self-hosted-v3' AND operation_count = 46 AND evidence_file_count = 95)
    OR (matrix_profile = 'internal-self-hosted-v4' AND operation_count = 48 AND evidence_file_count = 99)
  );

CREATE OR REPLACE FUNCTION enforce_stage6_operations_exercise_matrix_profile()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'INSERT' AND NEW.matrix_profile <> 'internal-self-hosted-v4' THEN
    RAISE EXCEPTION 'New Stage 6 Operations exercises require the internal cost-accounting matrix'
      USING ERRCODE = '23514';
  END IF;
  IF TG_OP = 'UPDATE' AND NEW.matrix_profile IS DISTINCT FROM OLD.matrix_profile THEN
    RAISE EXCEPTION 'Stage 6 Operations exercise matrix profile is immutable'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

COMMENT ON COLUMN stage6_operations_exercises.matrix_profile IS
  'Immutable product-surface profile. New exercises use the 48-operation internal self-hosted v4 matrix with internal-cost import and decision; historical v2/v3 profiles remain audit-only.';
