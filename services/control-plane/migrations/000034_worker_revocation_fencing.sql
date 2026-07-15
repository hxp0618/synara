ALTER TABLE worker_instances
  ADD COLUMN administrative_status TEXT,
  ADD COLUMN revoked_at TIMESTAMPTZ,
  ADD COLUMN revoked_by UUID REFERENCES users(id) ON DELETE RESTRICT,
  ADD COLUMN revocation_reason TEXT;

-- compatibility_status='revoked' was the pre-000034 fail-closed marker. Move it
-- into the independent administrative state before removing that overloaded
-- compatibility value. Existing draining Workers retain their operator intent.
UPDATE worker_instances
SET administrative_status = CASE
      WHEN compatibility_status = 'revoked' THEN 'revoked'
      WHEN status = 'draining' THEN 'draining'
      ELSE 'active'
    END,
    revoked_at = CASE
      WHEN compatibility_status = 'revoked' THEN
        COALESCE(compatibility_checked_at, terminated_at, last_heartbeat_at, registered_at, now())
      ELSE NULL
    END,
    revoked_by = NULL,
    revocation_reason = CASE
      WHEN compatibility_status = 'revoked' THEN 'legacy-compatibility-revoked'
      ELSE NULL
    END;

ALTER TABLE worker_instances
  ALTER COLUMN administrative_status SET DEFAULT 'active',
  ALTER COLUMN administrative_status SET NOT NULL,
  ADD CONSTRAINT chk_worker_instances_administrative_status
    CHECK (administrative_status IN ('active', 'draining', 'revoked')),
  ADD CONSTRAINT chk_worker_instances_revocation_shape
    CHECK (
      (
        administrative_status = 'revoked'
        AND revoked_at IS NOT NULL
        AND length(btrim(revocation_reason)) BETWEEN 1 AND 2000
        AND (revoked_by IS NOT NULL OR revocation_reason = 'legacy-compatibility-revoked')
      )
      OR
      (
        administrative_status <> 'revoked'
        AND revoked_at IS NULL
        AND revoked_by IS NULL
        AND revocation_reason IS NULL
      )
    );

CREATE TABLE worker_identity_tombstones (
  execution_target_id UUID NOT NULL REFERENCES execution_targets(id) ON DELETE RESTRICT,
  cluster_id TEXT NOT NULL,
  namespace TEXT NOT NULL,
  pod_name TEXT NOT NULL,
  worker_id UUID NOT NULL,
  worker_incarnation BIGINT NOT NULL,
  revoked_at TIMESTAMPTZ NOT NULL,
  revoked_by UUID REFERENCES users(id) ON DELETE RESTRICT,
  revocation_reason TEXT NOT NULL,
  PRIMARY KEY (execution_target_id, cluster_id, namespace, pod_name),
  CHECK (length(cluster_id) BETWEEN 1 AND 160),
  CHECK (length(namespace) BETWEEN 1 AND 253),
  CHECK (length(pod_name) BETWEEN 1 AND 253),
  CHECK (worker_incarnation > 0),
  CHECK (length(btrim(revocation_reason)) BETWEEN 1 AND 2000),
  CHECK (revoked_by IS NOT NULL OR revocation_reason = 'legacy-compatibility-revoked')
);

INSERT INTO worker_identity_tombstones (
  execution_target_id,
  cluster_id,
  namespace,
  pod_name,
  worker_id,
  worker_incarnation,
  revoked_at,
  revoked_by,
  revocation_reason
)
SELECT
  revoked.execution_target_id,
  revoked.cluster_id,
  revoked.namespace,
  revoked.pod_name,
  revoked.id,
  revoked.incarnation,
  revoked.revoked_at,
  revoked.revoked_by,
  revoked.revocation_reason
FROM (
  SELECT DISTINCT ON (execution_target_id, cluster_id, namespace, pod_name)
    execution_target_id,
    cluster_id,
    namespace,
    pod_name,
    id,
    incarnation,
    revoked_at,
    revoked_by,
    revocation_reason
  FROM worker_instances
  WHERE administrative_status = 'revoked'
  ORDER BY
    execution_target_id,
    cluster_id,
    namespace,
    pod_name,
    incarnation DESC,
    revoked_at DESC,
    id DESC
) AS revoked;

UPDATE worker_instances
SET compatibility_status = 'incompatible',
    compatibility_reason = COALESCE(
      NULLIF(btrim(compatibility_reason), ''),
      'Worker compatibility revocation was migrated to administrative revocation.'
    ),
    compatibility_checked_at = COALESCE(compatibility_checked_at, revoked_at)
WHERE compatibility_status = 'revoked';

ALTER TABLE worker_instances
  DROP CONSTRAINT worker_instances_compatibility_status_check,
  ADD CONSTRAINT worker_instances_compatibility_status_check
    CHECK (compatibility_status IN ('unknown', 'compatible', 'incompatible'));

DROP INDEX IF EXISTS idx_worker_instances_compatibility;
DROP INDEX IF EXISTS uq_worker_instances_active_pod;

CREATE UNIQUE INDEX uq_worker_instances_active_logical_identity
  ON worker_instances (execution_target_id, cluster_id, namespace, pod_name)
  WHERE administrative_status <> 'revoked' AND status <> 'terminated';

CREATE INDEX idx_worker_instances_claimability
  ON worker_instances (
    execution_target_id,
    administrative_status,
    compatibility_status,
    status,
    last_heartbeat_at,
    id
  );

CREATE INDEX idx_worker_instances_logical_identity
  ON worker_instances (
    execution_target_id,
    cluster_id,
    namespace,
    pod_name,
    administrative_status,
    id
  );

CREATE OR REPLACE FUNCTION enforce_worker_revocation_fencing()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'UPDATE' AND (
    NEW.execution_target_id IS DISTINCT FROM OLD.execution_target_id
    OR NEW.cluster_id IS DISTINCT FROM OLD.cluster_id
    OR NEW.namespace IS DISTINCT FROM OLD.namespace
    OR NEW.pod_name IS DISTINCT FROM OLD.pod_name
  ) THEN
    RAISE EXCEPTION 'worker logical identity is immutable'
      USING ERRCODE = '23514';
  END IF;

  PERFORM pg_advisory_xact_lock(hashtextextended(
    jsonb_build_array(
      NEW.execution_target_id,
      NEW.cluster_id,
      NEW.namespace,
      NEW.pod_name
    )::text,
    0
  ));

  IF NEW.compatibility_status = 'revoked' THEN
    RAISE EXCEPTION 'worker compatibility status cannot encode administrative revocation'
      USING ERRCODE = '23514';
  END IF;

  IF NEW.administrative_status = 'revoked'
     AND (TG_OP = 'INSERT' OR OLD.administrative_status <> 'revoked') THEN
    IF NEW.revoked_by IS NULL THEN
      RAISE EXCEPTION 'new worker administrative revocation requires an actor'
        USING ERRCODE = '23514';
    END IF;
    IF NEW.revoked_at IS NULL
       OR NEW.revocation_reason IS NULL
       OR length(btrim(NEW.revocation_reason)) NOT BETWEEN 1 AND 2000 THEN
      RAISE EXCEPTION 'worker administrative revocation requires a timestamp and reason'
        USING ERRCODE = '23514';
    END IF;
    IF EXISTS (
      SELECT 1
      FROM worker_instances AS existing
      WHERE existing.execution_target_id = NEW.execution_target_id
        AND existing.cluster_id = NEW.cluster_id
        AND existing.namespace = NEW.namespace
        AND existing.pod_name = NEW.pod_name
        AND existing.id <> NEW.id
        AND existing.administrative_status <> 'revoked'
        AND existing.status <> 'terminated'
    ) THEN
      RAISE EXCEPTION 'worker logical identity has another current registration'
        USING ERRCODE = '23514';
    END IF;
  ELSIF NEW.administrative_status <> 'revoked' AND EXISTS (
    SELECT 1
    FROM worker_identity_tombstones AS tombstone
    WHERE tombstone.execution_target_id = NEW.execution_target_id
      AND tombstone.cluster_id = NEW.cluster_id
      AND tombstone.namespace = NEW.namespace
      AND tombstone.pod_name = NEW.pod_name
  ) THEN
    RAISE EXCEPTION 'worker logical identity has been administratively revoked'
      USING ERRCODE = '23514';
  END IF;

  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_instances_revocation_fencing ON worker_instances;
CREATE TRIGGER trg_worker_instances_revocation_fencing
BEFORE INSERT OR UPDATE OF
  execution_target_id,
  cluster_id,
  namespace,
  pod_name,
  administrative_status,
  revoked_at,
  revoked_by,
  revocation_reason,
  compatibility_status
ON worker_instances
FOR EACH ROW EXECUTE FUNCTION enforce_worker_revocation_fencing();

CREATE OR REPLACE FUNCTION reject_revoked_worker_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'revoked worker registration is immutable'
    USING ERRCODE = '23514';
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_instances_revoked_immutable ON worker_instances;
CREATE TRIGGER trg_worker_instances_revoked_immutable
BEFORE UPDATE OR DELETE ON worker_instances
FOR EACH ROW
WHEN (OLD.administrative_status = 'revoked')
EXECUTE FUNCTION reject_revoked_worker_mutation();

CREATE OR REPLACE FUNCTION assert_worker_identity_tombstone_source()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM worker_instances AS worker
    WHERE worker.id = NEW.worker_id
      AND worker.execution_target_id = NEW.execution_target_id
      AND worker.cluster_id = NEW.cluster_id
      AND worker.namespace = NEW.namespace
      AND worker.pod_name = NEW.pod_name
      AND worker.incarnation = NEW.worker_incarnation
      AND worker.administrative_status = 'revoked'
      AND worker.revoked_at = NEW.revoked_at
      AND worker.revoked_by IS NOT DISTINCT FROM NEW.revoked_by
      AND worker.revocation_reason = NEW.revocation_reason
  ) THEN
    RAISE EXCEPTION 'worker identity tombstone does not match its revoked Worker source'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_identity_tombstones_source ON worker_identity_tombstones;
CREATE TRIGGER trg_worker_identity_tombstones_source
BEFORE INSERT ON worker_identity_tombstones
FOR EACH ROW EXECUTE FUNCTION assert_worker_identity_tombstone_source();

CREATE OR REPLACE FUNCTION record_worker_identity_tombstone()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  INSERT INTO worker_identity_tombstones (
    execution_target_id,
    cluster_id,
    namespace,
    pod_name,
    worker_id,
    worker_incarnation,
    revoked_at,
    revoked_by,
    revocation_reason
  ) VALUES (
    NEW.execution_target_id,
    NEW.cluster_id,
    NEW.namespace,
    NEW.pod_name,
    NEW.id,
    NEW.incarnation,
    NEW.revoked_at,
    NEW.revoked_by,
    NEW.revocation_reason
  );
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_instances_record_tombstone_insert ON worker_instances;
CREATE TRIGGER trg_worker_instances_record_tombstone_insert
AFTER INSERT ON worker_instances
FOR EACH ROW
WHEN (NEW.administrative_status = 'revoked')
EXECUTE FUNCTION record_worker_identity_tombstone();

DROP TRIGGER IF EXISTS trg_worker_instances_record_tombstone_update ON worker_instances;
CREATE TRIGGER trg_worker_instances_record_tombstone_update
AFTER UPDATE OF administrative_status ON worker_instances
FOR EACH ROW
WHEN (
  OLD.administrative_status <> 'revoked'
  AND NEW.administrative_status = 'revoked'
)
EXECUTE FUNCTION record_worker_identity_tombstone();

CREATE OR REPLACE FUNCTION reject_worker_identity_tombstone_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'worker identity tombstone is immutable'
    USING ERRCODE = '23514';
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_identity_tombstones_immutable ON worker_identity_tombstones;
CREATE TRIGGER trg_worker_identity_tombstones_immutable
BEFORE UPDATE OR DELETE ON worker_identity_tombstones
FOR EACH ROW EXECUTE FUNCTION reject_worker_identity_tombstone_mutation();

COMMENT ON COLUMN worker_instances.administrative_status IS
  'Independent operator state. revoked is terminal and fences tokens, heartbeats, claims, leases, and re-registration.';
COMMENT ON TABLE worker_identity_tombstones IS
  'Permanent logical Worker identity revocations keyed without instance_uid so a new physical incarnation cannot bypass fencing.';
