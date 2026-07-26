CREATE TABLE kubernetes_pod_deletion_fences (
  execution_target_id UUID NOT NULL REFERENCES execution_targets(id) ON DELETE CASCADE,
  namespace TEXT NOT NULL,
  pod_name TEXT NOT NULL,
  pod_uid UUID NOT NULL,
  requested_at TIMESTAMPTZ NOT NULL,
  reason TEXT NOT NULL,
  PRIMARY KEY (execution_target_id, namespace, pod_name, pod_uid),
  CHECK (length(btrim(namespace)) BETWEEN 1 AND 253),
  CHECK (length(btrim(pod_name)) BETWEEN 1 AND 253),
  CHECK (pod_uid <> '00000000-0000-0000-0000-000000000000'::uuid),
  CHECK (length(btrim(reason)) BETWEEN 1 AND 2000)
);

CREATE INDEX idx_kubernetes_pod_deletion_fences_requested
  ON kubernetes_pod_deletion_fences (execution_target_id, requested_at DESC);

CREATE OR REPLACE FUNCTION try_lock_worker_logical_identity(
  target_id UUID,
  cluster_id TEXT,
  worker_namespace TEXT,
  worker_pod_name TEXT
)
RETURNS BOOLEAN
LANGUAGE sql
AS $$
  SELECT pg_try_advisory_xact_lock(hashtextextended(
    jsonb_build_array(target_id, cluster_id, worker_namespace, worker_pod_name)::text,
    0
  ));
$$;

CREATE OR REPLACE FUNCTION fence_kubernetes_pod_deletion_insert()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM execution_targets AS target
    WHERE target.id = NEW.execution_target_id
      AND target.kind = 'kubernetes'
  ) THEN
    RAISE EXCEPTION 'Kubernetes Pod deletion fence target scope is invalid'
      USING ERRCODE = '23514';
  END IF;

  IF NOT try_lock_worker_logical_identity(
    NEW.execution_target_id,
    'kubernetes',
    NEW.namespace,
    NEW.pod_name
  ) THEN
    RAISE EXCEPTION 'Worker logical identity lock is unavailable'
      USING ERRCODE = '40001';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_kubernetes_pod_deletion_fences_insert
  ON kubernetes_pod_deletion_fences;
CREATE TRIGGER trg_kubernetes_pod_deletion_fences_insert
BEFORE INSERT ON kubernetes_pod_deletion_fences
FOR EACH ROW EXECUTE FUNCTION fence_kubernetes_pod_deletion_insert();

CREATE OR REPLACE FUNCTION reject_kubernetes_pod_deletion_fence_update()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'Kubernetes Pod deletion fence is immutable'
    USING ERRCODE = '23514';
END;
$$;

DROP TRIGGER IF EXISTS trg_kubernetes_pod_deletion_fences_immutable
  ON kubernetes_pod_deletion_fences;
CREATE TRIGGER trg_kubernetes_pod_deletion_fences_immutable
BEFORE UPDATE ON kubernetes_pod_deletion_fences
FOR EACH ROW EXECUTE FUNCTION reject_kubernetes_pod_deletion_fence_update();

CREATE OR REPLACE FUNCTION reject_kubernetes_pod_deletion_fence_delete()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM execution_targets AS target
    WHERE target.id = OLD.execution_target_id
  ) THEN
    RAISE EXCEPTION 'Kubernetes Pod deletion fence is immutable'
      USING ERRCODE = '23514';
  END IF;
  RETURN OLD;
END;
$$;

DROP TRIGGER IF EXISTS trg_kubernetes_pod_deletion_fences_delete
  ON kubernetes_pod_deletion_fences;
CREATE TRIGGER trg_kubernetes_pod_deletion_fences_delete
BEFORE DELETE ON kubernetes_pod_deletion_fences
FOR EACH ROW EXECUTE FUNCTION reject_kubernetes_pod_deletion_fence_delete();

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

  IF NOT try_lock_worker_logical_identity(
    NEW.execution_target_id,
    NEW.cluster_id,
    NEW.namespace,
    NEW.pod_name
  ) THEN
    RAISE EXCEPTION 'Worker logical identity lock is unavailable'
      USING ERRCODE = '40001';
  END IF;

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

CREATE OR REPLACE FUNCTION enforce_kubernetes_pod_deletion_fencing()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.target_kind <> 'kubernetes'
     OR NEW.registration_trust_mode <> 'kubernetes-pod-bound-v1' THEN
    RETURN NEW;
  END IF;
  IF NEW.cluster_id <> 'kubernetes' THEN
    RAISE EXCEPTION 'Pod-bound Kubernetes Worker cluster identity is invalid'
      USING ERRCODE = '23514';
  END IF;
  IF NOT try_lock_worker_logical_identity(
    NEW.execution_target_id,
    'kubernetes',
    NEW.namespace,
    NEW.pod_name
  ) THEN
    RAISE EXCEPTION 'Worker logical identity lock is unavailable'
      USING ERRCODE = '40001';
  END IF;
  IF EXISTS (
    SELECT 1
    FROM kubernetes_pod_deletion_fences AS fence
    WHERE fence.execution_target_id = NEW.execution_target_id
      AND fence.namespace = NEW.namespace
      AND fence.pod_name = NEW.pod_name
      AND fence.pod_uid::text = NEW.instance_uid
  ) THEN
    RAISE EXCEPTION 'Kubernetes Pod UID is deletion fenced'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_instances_pod_deletion_fencing ON worker_instances;
CREATE TRIGGER trg_worker_instances_pod_deletion_fencing
BEFORE INSERT OR UPDATE OF
  execution_target_id,
  target_kind,
  cluster_id,
  namespace,
  pod_name,
  instance_uid,
  registration_trust_mode
ON worker_instances
FOR EACH ROW EXECUTE FUNCTION enforce_kubernetes_pod_deletion_fencing();

CREATE OR REPLACE FUNCTION prevent_fenced_kubernetes_worker_reactivation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.target_kind <> 'kubernetes'
     OR NEW.registration_trust_mode <> 'kubernetes-pod-bound-v1'
     OR NOT (
       (NEW.status = 'online' AND NEW.status IS DISTINCT FROM OLD.status)
       OR (
         NEW.administrative_status = 'active'
         AND NEW.administrative_status IS DISTINCT FROM OLD.administrative_status
       )
     ) THEN
    RETURN NEW;
  END IF;
  IF NOT try_lock_worker_logical_identity(
    NEW.execution_target_id,
    'kubernetes',
    NEW.namespace,
    NEW.pod_name
  ) THEN
    RAISE EXCEPTION 'Worker logical identity lock is unavailable'
      USING ERRCODE = '40001';
  END IF;
  IF EXISTS (
    SELECT 1
    FROM kubernetes_pod_deletion_fences AS fence
    WHERE fence.execution_target_id = NEW.execution_target_id
      AND fence.namespace = NEW.namespace
      AND fence.pod_name = NEW.pod_name
      AND fence.pod_uid::text = NEW.instance_uid
  ) THEN
    RAISE EXCEPTION 'Kubernetes Pod UID is deletion fenced'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_instances_fenced_reactivation ON worker_instances;
CREATE TRIGGER trg_worker_instances_fenced_reactivation
BEFORE UPDATE OF status, administrative_status ON worker_instances
FOR EACH ROW EXECUTE FUNCTION prevent_fenced_kubernetes_worker_reactivation();
