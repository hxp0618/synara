CREATE TABLE worker_storage_scrubs (
  id UUID PRIMARY KEY,
  worker_id UUID NOT NULL REFERENCES worker_instances(id) ON DELETE RESTRICT,
  worker_incarnation BIGINT NOT NULL CHECK (worker_incarnation > 0),
  worker_instance_uid TEXT NOT NULL,
  execution_target_id UUID NOT NULL REFERENCES execution_targets(id) ON DELETE RESTRICT,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  scope_kind TEXT NOT NULL CHECK (scope_kind IN ('execution', 'workspace-cleanup')),
  scope_id UUID NOT NULL,
  scope_generation BIGINT NOT NULL CHECK (scope_generation > 0),
  scrub_generation BIGINT NOT NULL CHECK (scrub_generation > 0),
  status TEXT NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending', 'acknowledged', 'failed')),
  created_at TIMESTAMPTZ NOT NULL,
  acknowledged_at TIMESTAMPTZ,
  failed_at TIMESTAMPTZ,
  failure_code TEXT,
  failure_message TEXT,
  UNIQUE (scope_kind, scope_id, scope_generation),
  UNIQUE (worker_id, worker_incarnation, scrub_generation),
  CHECK (length(worker_instance_uid) BETWEEN 1 AND 253),
  CHECK (
    (status = 'pending' AND acknowledged_at IS NULL AND failed_at IS NULL
      AND failure_code IS NULL AND failure_message IS NULL)
    OR
    (status = 'acknowledged' AND acknowledged_at IS NOT NULL AND failed_at IS NULL
      AND failure_code IS NULL AND failure_message IS NULL)
    OR
    (status = 'failed' AND acknowledged_at IS NULL AND failed_at IS NOT NULL
      AND failure_code IS NOT NULL)
  ),
  CHECK (failure_code IS NULL OR length(failure_code) BETWEEN 1 AND 160),
  CHECK (failure_message IS NULL OR length(failure_message) <= 10000)
);

CREATE INDEX idx_worker_storage_scrubs_claim
  ON worker_storage_scrubs (worker_id, worker_incarnation, status, scrub_generation, id);

CREATE UNIQUE INDEX uq_worker_storage_scrubs_active
  ON worker_storage_scrubs (worker_id, worker_incarnation)
  WHERE status IN ('pending', 'failed');

CREATE OR REPLACE FUNCTION enforce_worker_storage_scrub_insert()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM worker_instances AS worker
    WHERE worker.id = NEW.worker_id
      AND worker.incarnation = NEW.worker_incarnation
      AND worker.instance_uid = NEW.worker_instance_uid
      AND worker.execution_target_id = NEW.execution_target_id
      AND worker.worker_mode = 'general-pool'
  ) THEN
    RAISE EXCEPTION 'Worker storage scrub physical identity is invalid' USING ERRCODE = '23514';
  END IF;
  IF NEW.scrub_generation <> COALESCE((
    SELECT MAX(existing.scrub_generation) + 1
    FROM worker_storage_scrubs AS existing
    WHERE existing.worker_id = NEW.worker_id
      AND existing.worker_incarnation = NEW.worker_incarnation
  ), 1) THEN
    RAISE EXCEPTION 'Worker storage scrub generation is not monotonic' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_storage_scrub_insert ON worker_storage_scrubs;
CREATE TRIGGER trg_worker_storage_scrub_insert
BEFORE INSERT ON worker_storage_scrubs
FOR EACH ROW EXECUTE FUNCTION enforce_worker_storage_scrub_insert();

CREATE OR REPLACE FUNCTION enforce_worker_storage_scrub_fence()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'Worker storage scrubs are append-only' USING ERRCODE = '23514';
  END IF;
  IF NEW.worker_id <> OLD.worker_id
     OR NEW.worker_instance_uid <> OLD.worker_instance_uid
     OR NEW.execution_target_id <> OLD.execution_target_id
     OR NEW.tenant_id <> OLD.tenant_id
     OR NEW.scope_kind <> OLD.scope_kind
     OR NEW.scope_id <> OLD.scope_id
     OR NEW.scope_generation <> OLD.scope_generation
     OR NEW.scrub_generation <> OLD.scrub_generation
     OR NEW.created_at <> OLD.created_at
     OR (
       NEW.worker_incarnation <> OLD.worker_incarnation
       AND NOT (
         OLD.status = 'pending'
         AND NEW.status = 'pending'
         AND NEW.worker_incarnation = OLD.worker_incarnation + 1
         AND EXISTS (
           SELECT 1 FROM worker_instances AS worker
           WHERE worker.id = NEW.worker_id
             AND worker.incarnation = NEW.worker_incarnation
             AND worker.instance_uid = NEW.worker_instance_uid
         )
       )
     ) THEN
    RAISE EXCEPTION 'Worker storage scrub identity is immutable' USING ERRCODE = '23514';
  END IF;
  IF OLD.status IN ('acknowledged', 'failed') AND ROW(NEW.*) IS DISTINCT FROM ROW(OLD.*) THEN
    RAISE EXCEPTION 'Worker storage scrub terminal receipt is immutable' USING ERRCODE = '23514';
  END IF;
  IF OLD.status = 'pending'
     AND NEW.status NOT IN ('acknowledged', 'failed')
     AND NOT (
       NEW.status = 'pending'
       AND NEW.worker_incarnation = OLD.worker_incarnation + 1
       AND EXISTS (
         SELECT 1 FROM worker_instances AS worker
         WHERE worker.id = NEW.worker_id
           AND worker.incarnation = NEW.worker_incarnation
           AND worker.instance_uid = NEW.worker_instance_uid
       )
     ) THEN
    RAISE EXCEPTION 'Worker storage scrub status transition is invalid' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_storage_scrub_fence ON worker_storage_scrubs;
CREATE TRIGGER trg_worker_storage_scrub_fence
BEFORE UPDATE OR DELETE ON worker_storage_scrubs
FOR EACH ROW EXECUTE FUNCTION enforce_worker_storage_scrub_fence();

COMMENT ON TABLE worker_storage_scrubs IS
  'Fail-closed physical-storage scrub fence created whenever a shared general-pool Worker releases Tenant-scoped work.';
COMMENT ON COLUMN worker_storage_scrubs.scrub_generation IS
  'Monotonic per Worker incarnation; receipts must bind this generation and the exact physical instance UID.';
