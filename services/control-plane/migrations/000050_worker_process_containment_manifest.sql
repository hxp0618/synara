ALTER TABLE worker_manifests
  ADD COLUMN process_containment_mode TEXT NOT NULL DEFAULT 'none',
  ADD COLUMN process_containment_supervisor_version TEXT,
  ADD COLUMN process_containment_probe_version INTEGER,
  ADD COLUMN process_containment_probe_sha256 TEXT,
  ADD COLUMN process_containment_supervisor_identity TEXT,
  ADD COLUMN process_containment_provider_identity TEXT,
  ADD CONSTRAINT chk_worker_manifest_process_containment
    CHECK (
      (
        process_containment_mode = 'none'
        AND process_containment_supervisor_version IS NULL
        AND process_containment_probe_version IS NULL
        AND process_containment_probe_sha256 IS NULL
        AND process_containment_supervisor_identity IS NULL
        AND process_containment_provider_identity IS NULL
      )
      OR
      (
        process_containment_mode IN ('cgroup-v2', 'job-object')
        AND length(btrim(process_containment_supervisor_version)) BETWEEN 1 AND 160
        AND process_containment_probe_version > 0
        AND process_containment_probe_sha256 ~ '^[0-9a-f]{64}$'
        AND length(btrim(process_containment_supervisor_identity)) BETWEEN 1 AND 160
        AND length(btrim(process_containment_provider_identity)) BETWEEN 1 AND 160
        AND process_containment_supervisor_identity <> process_containment_provider_identity
        AND (
          (process_containment_mode = 'cgroup-v2' AND operating_system = 'linux')
          OR (process_containment_mode = 'job-object' AND operating_system = 'windows')
        )
      )
    );

CREATE OR REPLACE FUNCTION protect_worker_manifest_process_containment()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.process_containment_mode IS DISTINCT FROM OLD.process_containment_mode
    OR NEW.process_containment_supervisor_version IS DISTINCT FROM OLD.process_containment_supervisor_version
    OR NEW.process_containment_probe_version IS DISTINCT FROM OLD.process_containment_probe_version
    OR NEW.process_containment_probe_sha256 IS DISTINCT FROM OLD.process_containment_probe_sha256
    OR NEW.process_containment_supervisor_identity IS DISTINCT FROM OLD.process_containment_supervisor_identity
    OR NEW.process_containment_provider_identity IS DISTINCT FROM OLD.process_containment_provider_identity THEN
    RAISE EXCEPTION 'Worker Manifest process containment evidence is immutable'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_worker_manifest_process_containment_immutable
BEFORE UPDATE OF
  process_containment_mode,
  process_containment_supervisor_version,
  process_containment_probe_version,
  process_containment_probe_sha256,
  process_containment_supervisor_identity,
  process_containment_provider_identity
ON worker_manifests
FOR EACH ROW EXECUTE FUNCTION protect_worker_manifest_process_containment();

COMMENT ON COLUMN worker_manifests.process_containment_mode IS
  'Immutable runtime-probed Provider process-tree containment mode. Operator-supplied transient Worker capabilities are not authoritative.';
COMMENT ON COLUMN worker_manifests.process_containment_probe_sha256 IS
  'SHA-256 identity of the escape-resistance probe result frozen into the Worker Manifest.';
