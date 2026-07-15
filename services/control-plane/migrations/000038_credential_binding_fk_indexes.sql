-- Foreign-key enforcement must be able to find disabled historical Bindings as
-- well as active ones. The live lookup indexes from 000035 are intentionally
-- partial on disabled_at IS NULL and therefore cannot serve these FK checks.

CREATE INDEX IF NOT EXISTS idx_credential_bindings_project_fk
  ON credential_bindings (tenant_id, project_id)
  WHERE project_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_credential_bindings_execution_target_fk
  ON credential_bindings (tenant_id, execution_target_id)
  WHERE execution_target_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_credential_bindings_created_by_fk
  ON credential_bindings (tenant_id, created_by);

CREATE INDEX IF NOT EXISTS idx_credential_bindings_disabled_by_fk
  ON credential_bindings (tenant_id, disabled_by)
  WHERE disabled_by IS NOT NULL;
