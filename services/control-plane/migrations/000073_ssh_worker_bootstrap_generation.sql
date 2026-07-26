ALTER TABLE worker_instances
  ADD COLUMN ssh_bootstrap_generation BIGINT,
  ADD CONSTRAINT chk_worker_instances_ssh_bootstrap_generation
  CHECK (
    ssh_bootstrap_generation IS NULL
    OR (
      target_kind = 'ssh'
      AND ssh_bootstrap_generation > 0
    )
  );

CREATE OR REPLACE FUNCTION assert_worker_target_contract()
RETURNS TRIGGER AS $$
BEGIN
  IF TG_OP = 'UPDATE'
     AND NEW.target_kind = 'ssh'
     AND EXISTS (
       SELECT 1 FROM execution_targets target
       WHERE target.id = NEW.execution_target_id
         AND target.kind = 'ssh'
         AND target.status = 'active'
     )
     AND (
       NEW.instance_uid IS DISTINCT FROM OLD.instance_uid
       OR NEW.ssh_bootstrap_generation IS DISTINCT FROM OLD.ssh_bootstrap_generation
     ) THEN
    RAISE EXCEPTION 'active SSH Worker bootstrap identity is immutable'
      USING ERRCODE = '23514';
  END IF;

  IF NOT EXISTS (
    SELECT 1 FROM execution_targets target
    WHERE target.id = NEW.execution_target_id
      AND target.kind = NEW.target_kind
      AND (
        target.status = 'active'
        OR (
          target.kind = 'ssh'
          AND target.status = 'offline'
          AND target.ssh_operation_kind IN ('install', 'upgrade')
          AND target.ssh_operation_generation = NEW.ssh_bootstrap_generation
          AND target.ssh_expected_instance_uid::text = NEW.instance_uid
        )
      )
  ) THEN
    RAISE EXCEPTION 'worker target is missing, inactive, or lacks exact SSH bootstrap authority'
      USING ERRCODE = '23514';
  END IF;
  IF NEW.target_kind IN ('ssh', 'docker', 'kubernetes')
     AND (NOT NEW.lease_supported OR NOT NEW.fencing_supported) THEN
    RAISE EXCEPTION 'remote workers require lease and fencing support'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_worker_instances_target_contract ON worker_instances;
CREATE CONSTRAINT TRIGGER trg_worker_instances_target_contract
AFTER INSERT OR UPDATE OF
  execution_target_id,
  target_kind,
  instance_uid,
  ssh_bootstrap_generation,
  lease_supported,
  fencing_supported
ON worker_instances
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_worker_target_contract();
