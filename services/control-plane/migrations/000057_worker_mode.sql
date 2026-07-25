ALTER TABLE worker_instances
  ADD COLUMN worker_mode TEXT NOT NULL DEFAULT 'general-pool',
  ADD CONSTRAINT chk_worker_instances_worker_mode
    CHECK (worker_mode IN ('execution-pinned', 'warm-pool', 'general-pool'));

UPDATE worker_instances
SET worker_mode = 'execution-pinned'
WHERE target_kind = 'kubernetes';

DROP INDEX IF EXISTS idx_worker_instances_claimability;
CREATE INDEX idx_worker_instances_claimability
  ON worker_instances (
    execution_target_id,
    worker_mode,
    administrative_status,
    compatibility_status,
    status,
    last_heartbeat_at,
    id
  );

COMMENT ON COLUMN worker_instances.worker_mode IS
  'Authoritative Worker allocation mode: execution-pinned handles one assigned execution, warm-pool handles one claimed execution then exits, general-pool remains standing and may claim workspace cleanup.';
