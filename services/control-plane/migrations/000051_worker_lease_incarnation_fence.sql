ALTER TABLE worker_leases
  ADD COLUMN worker_incarnation BIGINT,
  ADD COLUMN worker_instance_uid TEXT;

DROP TRIGGER IF EXISTS trg_worker_lease_matches_execution ON worker_leases;

UPDATE worker_leases AS lease
SET worker_incarnation = worker.incarnation,
    worker_instance_uid = worker.instance_uid
FROM worker_instances AS worker
WHERE worker.id = lease.worker_id;

ALTER TABLE worker_leases
  ALTER COLUMN worker_incarnation SET NOT NULL,
  ALTER COLUMN worker_instance_uid SET NOT NULL,
  ADD CONSTRAINT chk_worker_leases_worker_incarnation
    CHECK (worker_incarnation > 0),
  ADD CONSTRAINT chk_worker_leases_worker_instance_uid
    CHECK (worker_instance_uid ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$');

CREATE INDEX idx_worker_leases_worker_incarnation
  ON worker_leases (worker_id, worker_incarnation, worker_instance_uid, expires_at, execution_id);

CREATE OR REPLACE FUNCTION assert_worker_lease_matches_execution()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM agent_executions AS execution
    JOIN worker_instances AS worker
      ON worker.id = NEW.worker_id
     AND worker.incarnation = NEW.worker_incarnation
     AND worker.instance_uid = NEW.worker_instance_uid
    WHERE execution.id = NEW.execution_id
      AND execution.tenant_id = NEW.tenant_id
      AND execution.worker_id = NEW.worker_id
      AND execution.generation = NEW.generation
      AND execution.status IN ('leased', 'running', 'waiting-for-approval')
  ) THEN
    RAISE EXCEPTION 'worker lease does not match the current execution generation and Worker incarnation'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE CONSTRAINT TRIGGER trg_worker_lease_matches_execution
AFTER INSERT OR UPDATE ON worker_leases
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_worker_lease_matches_execution();

CREATE OR REPLACE FUNCTION enforce_worker_lease_incarnation_immutability()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.worker_id IS DISTINCT FROM OLD.worker_id
    OR NEW.worker_incarnation IS DISTINCT FROM OLD.worker_incarnation
    OR NEW.worker_instance_uid IS DISTINCT FROM OLD.worker_instance_uid THEN
    RAISE EXCEPTION 'Worker lease incarnation lineage is immutable'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_worker_leases_incarnation_immutable
BEFORE UPDATE OF worker_id, worker_incarnation, worker_instance_uid
ON worker_leases
FOR EACH ROW EXECUTE FUNCTION enforce_worker_lease_incarnation_immutability();

COMMENT ON COLUMN worker_leases.worker_incarnation IS
  'Immutable Worker registration incarnation that acquired this lease; a later registration cannot inherit it.';

COMMENT ON COLUMN worker_leases.worker_instance_uid IS
  'Immutable physical Worker process identity that acquired this lease.';
