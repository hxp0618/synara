-- A managed reconciler must be able to make a durable, incarnation-fenced
-- decision that an old Worker is draining before it crosses the external
-- container deletion boundary.  The ordinary Worker `status=draining` bit is
-- caller-controlled through Heartbeat and therefore is not sufficient as the
-- server-side authority for rolling replacement or scale-down.
ALTER TABLE worker_instances
  ADD COLUMN reconciliation_drain_incarnation BIGINT,
  ADD COLUMN reconciliation_drain_instance_uid TEXT,
  ADD COLUMN reconciliation_drain_requested_at TIMESTAMPTZ,
  ADD COLUMN reconciliation_drain_reason TEXT;

ALTER TABLE worker_instances
  ADD CONSTRAINT chk_worker_instances_reconciliation_drain_shape
  CHECK (
    (
      reconciliation_drain_incarnation IS NULL
      AND reconciliation_drain_instance_uid IS NULL
      AND reconciliation_drain_requested_at IS NULL
      AND reconciliation_drain_reason IS NULL
    )
    OR
    (
      reconciliation_drain_incarnation IS NOT NULL
      AND reconciliation_drain_incarnation > 0
      AND reconciliation_drain_incarnation <= incarnation
      AND reconciliation_drain_instance_uid IS NOT NULL
      AND reconciliation_drain_instance_uid ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
      AND reconciliation_drain_requested_at IS NOT NULL
      AND reconciliation_drain_reason IS NOT NULL
      AND length(btrim(reconciliation_drain_reason)) BETWEEN 1 AND 200
      AND reconciliation_drain_reason = btrim(reconciliation_drain_reason)
    )
  );

CREATE INDEX idx_worker_instances_reconciliation_drains
  ON worker_instances (
    execution_target_id,
    target_kind,
    reconciliation_drain_requested_at,
    id
  )
  WHERE reconciliation_drain_requested_at IS NOT NULL
    AND status <> 'terminated';

CREATE UNIQUE INDEX uq_worker_instances_active_reconciliation_drain_target
  ON worker_instances (execution_target_id)
  WHERE target_kind = 'docker'
    AND reconciliation_drain_requested_at IS NOT NULL
    AND status <> 'terminated';

COMMENT ON COLUMN worker_instances.reconciliation_drain_incarnation IS
  'Incarnation at which a managed reconciler durably stopped new claims before external Worker deletion.';
COMMENT ON COLUMN worker_instances.reconciliation_drain_instance_uid IS
  'Exact physical Worker instance fenced by the managed reconciliation drain.';
COMMENT ON COLUMN worker_instances.reconciliation_drain_requested_at IS
  'Server-authored rolling replacement or scale-down drain authority; Worker heartbeats cannot clear it.';
COMMENT ON COLUMN worker_instances.reconciliation_drain_reason IS
  'Bounded low-cardinality reason for the managed reconciliation drain.';
