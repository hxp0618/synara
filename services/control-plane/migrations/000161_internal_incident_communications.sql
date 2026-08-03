-- Migrate active Stage 6 incident evidence from an external customer Status Page
-- contract to the enterprise-internal self-hosted Status Board contract. Historical
-- table and column names remain unchanged so prior immutable evidence stays readable.

ALTER TABLE stage6_incident_exercises
  DROP CONSTRAINT IF EXISTS stage6_incident_exercises_exercise_mode_check;
ALTER TABLE stage6_incident_exercises
  ADD CONSTRAINT stage6_incident_exercises_exercise_mode_check
  CHECK (exercise_mode IN ('live-public-communication', 'live-internal-communication'));

DO $$
DECLARE
  definition TEXT;
  previous_definition TEXT;
BEGIN
  SELECT pg_get_functiondef('enforce_stage6_incident_exercise()'::regprocedure)
  INTO definition;
  IF definition IS NULL THEN
    RAISE EXCEPTION 'Stage 6 Incident exercise guard is unavailable';
  END IF;

  previous_definition := definition;
  definition := replace(
    definition,
    'synara.incident-communication-exercise-evidence-receipt.v1',
    'synara.incident-communication-exercise-evidence-receipt.v2'
  );
  definition := replace(definition, 'statusPageOrigin', 'internalStatusBoardOrigin');
  definition := replace(
    definition,
    'independentStatusPageDeclared',
    'independentInternalStatusBoardDeclared'
  );
  definition := replace(
    definition,
    'statusPageComponentsComplete',
    'internalStatusBoardComponentsComplete'
  );
  definition := replace(definition, 'publicTimelineWithinTargets', 'internalTimelineWithinTargets');
  definition := replace(
    definition,
    'subscriberDeliveryComplete',
    'employeeNotificationDeliveryComplete'
  );
  definition := replace(definition, 'statusPageComponents', 'internalStatusBoardComponents');
  definition := replace(
    definition,
    'OR receipt_json->>''exerciseMode'' <> NEW.exercise_mode',
    'OR receipt_json->>''exerciseMode'' <> NEW.exercise_mode
       OR NEW.exercise_mode <> ''live-internal-communication'''
  );

  IF definition = previous_definition
     OR position('synara.incident-communication-exercise-evidence-receipt.v1' IN definition) <> 0
     OR position('statusPageOrigin' IN definition) <> 0
     OR position('publicTimelineWithinTargets' IN definition) <> 0
     OR position('subscriberDeliveryComplete' IN definition) <> 0
     OR position('NEW.exercise_mode <> ''live-internal-communication''' IN definition) = 0 THEN
    RAISE EXCEPTION 'Stage 6 Incident exercise guard could not be migrated safely';
  END IF;
  EXECUTE definition;
END;
$$;

DO $$
DECLARE
  definition TEXT;
  previous_definition TEXT;
BEGIN
  SELECT pg_get_functiondef('enforce_stage6_release_receipt_projection()'::regprocedure)
  INTO definition;
  IF definition IS NULL THEN
    RAISE EXCEPTION 'Stage 6 release receipt projection guard is unavailable';
  END IF;

  previous_definition := definition;
  definition := replace(
    definition,
    'synara.incident-communication-exercise-evidence-receipt.v1',
    'synara.incident-communication-exercise-evidence-receipt.v2'
  );
  IF definition = previous_definition
     OR position('synara.incident-communication-exercise-evidence-receipt.v1' IN definition) <> 0 THEN
    RAISE EXCEPTION 'Stage 6 release receipt projection guard could not be migrated safely';
  END IF;
  EXECUTE definition;
END;
$$;

COMMENT ON FUNCTION enforce_stage6_incident_exercise() IS
  'Validates v2 enterprise-internal Status Board, employee notification, paging, recovery and separated approval evidence while retaining historical column names.';

COMMENT ON FUNCTION enforce_stage6_release_receipt_projection() IS
  'Rejects candidate bundles whose Incident projection is not the v2 enterprise-internal communication receipt.';
