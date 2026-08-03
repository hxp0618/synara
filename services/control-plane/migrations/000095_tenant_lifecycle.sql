ALTER TABLE tenants
  ADD COLUMN lifecycle_version BIGINT NOT NULL DEFAULT 1,
  ADD COLUMN trial_expires_at TIMESTAMPTZ,
  ADD COLUMN suspended_at TIMESTAMPTZ,
  ADD COLUMN closed_at TIMESTAMPTZ;

UPDATE tenants
SET suspended_at = COALESCE(updated_at, now())
WHERE status = 'suspended' AND suspended_at IS NULL;

ALTER TABLE tenants DROP CONSTRAINT IF EXISTS tenants_status_check;
ALTER TABLE tenants DROP CONSTRAINT IF EXISTS ck_tenants_lifecycle_version;
ALTER TABLE tenants DROP CONSTRAINT IF EXISTS ck_tenants_lifecycle_shape;

ALTER TABLE tenants
  ADD CONSTRAINT ck_tenants_lifecycle_version CHECK (lifecycle_version > 0),
  ADD CONSTRAINT ck_tenants_lifecycle_shape CHECK (
    (
      status = 'trialing'
      AND trial_expires_at IS NOT NULL
      AND suspended_at IS NULL
      AND closed_at IS NULL
      AND deleted_at IS NULL
    )
    OR
    (
      status = 'active'
      AND trial_expires_at IS NULL
      AND suspended_at IS NULL
      AND closed_at IS NULL
      AND deleted_at IS NULL
    )
    OR
    (
      status = 'suspended'
      AND trial_expires_at IS NULL
      AND suspended_at IS NOT NULL
      AND closed_at IS NULL
      AND deleted_at IS NULL
    )
    OR
    (
      status = 'closed'
      AND trial_expires_at IS NULL
      AND suspended_at IS NULL
      AND closed_at IS NOT NULL
      AND deleted_at IS NULL
    )
    OR
    (status = 'deleting' AND deleted_at IS NOT NULL)
  );

CREATE INDEX idx_tenants_lifecycle_status
  ON tenants (status, trial_expires_at, id)
  WHERE deleted_at IS NULL;
