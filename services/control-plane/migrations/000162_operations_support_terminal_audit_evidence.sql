DROP TRIGGER trg_stage6_operations_exercise_approvals_no_update
  ON stage6_operations_exercise_approvals;
DROP TRIGGER trg_stage6_operations_exercises_guard
  ON stage6_operations_exercises;

UPDATE stage6_operations_exercise_approvals AS approval
SET superseded_at = now(),
    superseded_reason =
      'Automatically superseded during Migration 000162 because Operations v1 did not bind terminal Support Grant audit actions.'
FROM stage6_operations_exercises AS exercise
WHERE exercise.id = approval.operations_exercise_id
  AND exercise.receipt_schema = 'synara.stage6-operations-browser-exercise-validation.v1'
  AND approval.superseded_at IS NULL;

UPDATE stage6_operations_exercises AS exercise
SET state = 'recorded', version = exercise.version + 1,
    approved_at = NULL, updated_at = now()
WHERE exercise.receipt_schema = 'synara.stage6-operations-browser-exercise-validation.v1'
  AND exercise.state = 'approved';

DO $$
DECLARE
  definition TEXT;
  support_marker TEXT :=
    'OR (receipt_json->>''supportLifecycleComplete'')::boolean IS DISTINCT FROM NEW.support_lifecycle_complete';
  support_replacement TEXT :=
    'OR (receipt_json->>''supportLifecycleComplete'')::boolean IS DISTINCT FROM NEW.support_lifecycle_complete
       OR receipt_json #>> ''{supportAccess,revocationAuditAction}'' <> ''support.access_revoked''
       OR receipt_json #>> ''{supportAccess,expiryAuditAction}'' <> ''support.access_expired''
       OR (receipt_json #>> ''{supportAccess,revocationAuditVisible}'')::boolean IS DISTINCT FROM true
       OR (receipt_json #>> ''{supportAccess,expiryAuditVisible}'')::boolean IS DISTINCT FROM true';
BEGIN
  SELECT pg_get_functiondef('enforce_stage6_operations_exercise()'::regprocedure) INTO definition;
  IF position('synara.stage6-operations-browser-exercise-validation.v1' IN definition) = 0
     OR position(support_marker IN definition) = 0 THEN
    RAISE EXCEPTION 'Unexpected Stage 6 Operations exercise v1 guard baseline'
      USING ERRCODE = '23514';
  END IF;
  definition := replace(
    definition,
    'synara.stage6-operations-browser-exercise-validation.v1',
    'synara.stage6-operations-browser-exercise-validation.v2'
  );
  definition := replace(definition, support_marker, support_replacement);
  definition := replace(
    definition,
    'OR NEW.version <> OLD.version + 1 OR OLD.state <> ''recorded''',
    'OR NEW.receipt_schema <> ''synara.stage6-operations-browser-exercise-validation.v2''
     OR NEW.version <> OLD.version + 1 OR OLD.state <> ''recorded'''
  );
  EXECUTE definition;

  SELECT pg_get_functiondef('enforce_stage6_operations_exercise_approval()'::regprocedure) INTO definition;
  IF position('parent.id IS NULL OR parent.operator_tenant_id <> NEW.operator_tenant_id OR parent.state <> ''recorded''' IN definition) = 0 THEN
    RAISE EXCEPTION 'Unexpected Stage 6 Operations approval guard baseline'
      USING ERRCODE = '23514';
  END IF;
  definition := replace(
    definition,
    'parent.id IS NULL OR parent.operator_tenant_id <> NEW.operator_tenant_id OR parent.state <> ''recorded''',
    'parent.id IS NULL OR parent.operator_tenant_id <> NEW.operator_tenant_id OR parent.state <> ''recorded''
     OR parent.receipt_schema <> ''synara.stage6-operations-browser-exercise-validation.v2'''
  );
  EXECUTE definition;

  SELECT pg_get_functiondef('enforce_stage6_release_operations_exercise_approval_gate()'::regprocedure) INTO definition;
  IF position('AND exercise.operator_tenant_id = NEW.operator_tenant_id' IN definition) = 0
     OR position('exercise.receipt_schema = ''synara.stage6-operations-browser-exercise-validation.v2''' IN definition) > 0 THEN
    RAISE EXCEPTION 'Unexpected Stage 6 Operations release gate baseline'
      USING ERRCODE = '23514';
  END IF;
  definition := replace(
    definition,
    'AND exercise.operator_tenant_id = NEW.operator_tenant_id',
    'AND exercise.operator_tenant_id = NEW.operator_tenant_id
      AND exercise.receipt_schema = ''synara.stage6-operations-browser-exercise-validation.v2'''
  );
  EXECUTE definition;

  SELECT pg_get_functiondef('enforce_stage6_release_receipt_projection()'::regprocedure) INTO definition;
  IF position('synara.stage6-operations-browser-exercise-validation.v1' IN definition) = 0 THEN
    RAISE EXCEPTION 'Unexpected Stage 6 candidate receipt projection baseline'
      USING ERRCODE = '23514';
  END IF;
  definition := replace(
    definition,
    'synara.stage6-operations-browser-exercise-validation.v1',
    'synara.stage6-operations-browser-exercise-validation.v2'
  );
  EXECUTE definition;
END;
$$;

CREATE TRIGGER trg_stage6_operations_exercises_guard
BEFORE INSERT OR UPDATE ON stage6_operations_exercises
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_operations_exercise();

CREATE TRIGGER trg_stage6_operations_exercise_approvals_no_update
BEFORE UPDATE ON stage6_operations_exercise_approvals
FOR EACH ROW EXECUTE FUNCTION reject_stage6_operations_exercise_history_mutation();

COMMENT ON FUNCTION enforce_stage6_operations_exercise() IS
  'Requires Operations v2 to bind both Grant-correlated support.access_revoked and system support.access_expired evidence. Historical v1 receipts remain immutable audit records and cannot regain positive release authority.';

COMMENT ON FUNCTION enforce_stage6_release_operations_exercise_approval_gate() IS
  'Requires an approved exact-candidate Operations v2 exercise with byte-bound decisions. It does not authenticate the deployed browser, Audit source, operators, evidence repository, or external GA authority.';
