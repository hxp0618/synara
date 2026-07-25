CREATE TABLE IF NOT EXISTS reconciler_leases (
  lease_name TEXT PRIMARY KEY,
  holder_id TEXT NOT NULL,
  fencing_token BIGINT NOT NULL CHECK (fencing_token > 0),
  acquired_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  renewed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at TIMESTAMPTZ NOT NULL,
  CHECK (length(lease_name) BETWEEN 1 AND 160),
  CHECK (length(holder_id) BETWEEN 1 AND 160),
  CHECK (renewed_at >= acquired_at),
  CHECK (expires_at >= renewed_at)
);

CREATE INDEX IF NOT EXISTS idx_reconciler_leases_expiry
  ON reconciler_leases (expires_at, lease_name);

COMMENT ON TABLE reconciler_leases IS
  'Durable leader-election leases for singleton control-plane reconcilers across replicas.';
COMMENT ON COLUMN reconciler_leases.lease_name IS
  'Caller-defined reconciler scope key. Reconciler names should be globally unique within the control plane.';
COMMENT ON COLUMN reconciler_leases.holder_id IS
  'Replica/process incarnation currently holding the reconciler leadership epoch.';
COMMENT ON COLUMN reconciler_leases.fencing_token IS
  'Monotonic leadership epoch token. It changes only when an expired or released epoch is taken over.';
