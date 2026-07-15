-- Managed Execution Target provisioning resolves one Registry Credential for
-- the Worker image. Multiple active Bindings for the same Target are
-- ambiguous even when they name different Registry hosts, so fail the forward
-- migration instead of silently choosing one.

DO $$
BEGIN
  IF EXISTS (
    SELECT 1
    FROM credential_bindings
    WHERE execution_target_id IS NOT NULL
      AND binding_kind = 'worker_image_pull'
      AND disabled_at IS NULL
    GROUP BY tenant_id, execution_target_id
    HAVING count(*) > 1
  ) THEN
    RAISE EXCEPTION 'Multiple active worker_image_pull Credential Bindings exist for one Execution Target'
      USING ERRCODE = 'P0001';
  END IF;
END
$$;

CREATE UNIQUE INDEX IF NOT EXISTS uq_credential_bindings_active_worker_image_target
  ON credential_bindings (tenant_id, execution_target_id)
  WHERE execution_target_id IS NOT NULL
    AND binding_kind = 'worker_image_pull'
    AND disabled_at IS NULL;

-- 000035 keyed these indexes by selector_value, which allowed one active
-- Binding per Registry host instead of one active Binding per Target. The new
-- unique index is also the live resolver lookup index, so both old indexes are
-- redundant.
DROP INDEX IF EXISTS uq_credential_bindings_active_target_selector;
DROP INDEX IF EXISTS idx_credential_bindings_target_lookup;
