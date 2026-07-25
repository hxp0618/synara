ALTER TABLE worker_manifests
  ADD COLUMN process_containment_trust_mode TEXT NOT NULL DEFAULT 'none',
  ADD COLUMN process_containment_attestation_key_id TEXT,
  ADD COLUMN process_containment_attestation_key_sha256 TEXT;

-- The 000050 trigger intentionally treats containment evidence as immutable.
-- This one migration is the only authority allowed to add trust provenance to
-- an existing row, and it always downgrades legacy evidence to untrusted.
ALTER TABLE worker_manifests
  DISABLE TRIGGER trg_worker_manifests_immutable;

ALTER TABLE worker_manifests
  DISABLE TRIGGER trg_worker_manifest_process_containment_immutable;

UPDATE worker_manifests
SET process_containment_trust_mode = 'legacy-untrusted'
WHERE process_containment_mode <> 'none';

ALTER TABLE worker_manifests
  ENABLE TRIGGER trg_worker_manifest_process_containment_immutable;

ALTER TABLE worker_manifests
  ENABLE TRIGGER trg_worker_manifests_immutable;

ALTER TABLE worker_manifests
  ADD CONSTRAINT chk_worker_manifests_process_containment_attestation
  CHECK (
    (
      process_containment_mode = 'none'
      AND process_containment_trust_mode = 'none'
      AND process_containment_attestation_key_id IS NULL
      AND process_containment_attestation_key_sha256 IS NULL
    ) OR (
      process_containment_mode IN ('cgroup-v2', 'job-object')
      AND process_containment_trust_mode = 'signed-v1'
      AND length(process_containment_attestation_key_id) BETWEEN 1 AND 160
      AND process_containment_attestation_key_sha256 ~ '^[0-9a-f]{64}$'
    ) OR (
      process_containment_mode IN ('cgroup-v2', 'job-object')
      AND process_containment_trust_mode = 'legacy-untrusted'
      AND process_containment_attestation_key_id IS NULL
      AND process_containment_attestation_key_sha256 IS NULL
    )
  );

CREATE INDEX idx_worker_manifests_process_containment_trust
  ON worker_manifests (process_containment_trust_mode, process_containment_mode, id);

-- Migration 000050 already installed the trigger against this function name.
-- Replace that exact function so the existing trigger also protects the new
-- trust provenance columns; defining a second, unattached function would leave
-- them mutable.
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
    OR NEW.process_containment_provider_identity IS DISTINCT FROM OLD.process_containment_provider_identity
    OR NEW.process_containment_trust_mode IS DISTINCT FROM OLD.process_containment_trust_mode
    OR NEW.process_containment_attestation_key_id IS DISTINCT FROM OLD.process_containment_attestation_key_id
    OR NEW.process_containment_attestation_key_sha256 IS DISTINCT FROM OLD.process_containment_attestation_key_sha256 THEN
    RAISE EXCEPTION 'Worker Manifest process-containment evidence is immutable'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER trg_worker_manifest_process_containment_immutable ON worker_manifests;
CREATE TRIGGER trg_worker_manifest_process_containment_immutable
BEFORE UPDATE OF
  process_containment_mode,
  process_containment_supervisor_version,
  process_containment_probe_version,
  process_containment_probe_sha256,
  process_containment_supervisor_identity,
  process_containment_provider_identity,
  process_containment_trust_mode,
  process_containment_attestation_key_id,
  process_containment_attestation_key_sha256
ON worker_manifests
FOR EACH ROW EXECUTE FUNCTION protect_worker_manifest_process_containment();

COMMENT ON COLUMN worker_manifests.process_containment_trust_mode IS
  'Server-verified trust provenance for strict process containment; legacy-untrusted is never Suspend-eligible.';

COMMENT ON COLUMN worker_manifests.process_containment_attestation_key_sha256 IS
  'SHA-256 of the authoritative Execution Target Ed25519 public key that verified this immutable Manifest.';
