ALTER TABLE workspace_checkpoints
  DROP CONSTRAINT IF EXISTS chk_workspace_checkpoints_terminal_metadata;

ALTER TABLE artifacts
  ADD COLUMN upload_cleanup_at TIMESTAMPTZ;

ALTER TABLE provider_runtime_bindings
  DROP CONSTRAINT IF EXISTS provider_runtime_bindings_tenant_id_last_execution_id_fkey,
  DROP CONSTRAINT IF EXISTS fk_provider_runtime_bindings_last_execution;

ALTER TABLE provider_runtime_bindings
  ADD CONSTRAINT fk_provider_runtime_bindings_last_execution
  FOREIGN KEY (tenant_id, last_execution_id)
  REFERENCES agent_executions(tenant_id, id)
  ON DELETE SET NULL (last_execution_id);

ALTER TABLE remote_workspaces
  DROP CONSTRAINT IF EXISTS remote_workspaces_tenant_id_last_execution_id_fkey,
  DROP CONSTRAINT IF EXISTS fk_remote_workspaces_last_execution;

ALTER TABLE remote_workspaces
  ADD CONSTRAINT fk_remote_workspaces_last_execution
  FOREIGN KEY (tenant_id, last_execution_id)
  REFERENCES agent_executions(tenant_id, id)
  ON DELETE SET NULL (last_execution_id);

ALTER TABLE workspace_checkpoints
  ADD CONSTRAINT chk_workspace_checkpoints_terminal_metadata
  CHECK (
    (
      status IN ('failed', 'expired')
      OR (failure_code IS NULL AND failure_message IS NULL AND failed_at IS NULL)
    )
    AND (ready_at IS NULL OR ready_at >= created_at)
    AND (failed_at IS NULL OR failed_at >= created_at)
  );

DROP INDEX IF EXISTS idx_artifacts_pending_expiry;

CREATE INDEX idx_artifacts_upload_expiry
  ON artifacts (upload_expires_at, id)
  WHERE upload_expires_at IS NOT NULL AND status IN ('pending', 'ready', 'failed', 'deleted');

ALTER TABLE artifacts
  ADD CONSTRAINT chk_artifacts_upload_cleanup_at
  CHECK (upload_cleanup_at IS NULL OR upload_cleanup_at >= created_at);

CREATE INDEX idx_workspace_checkpoints_active_created
  ON workspace_checkpoints (created_at, id)
  WHERE status IN ('pending', 'uploading');

COMMENT ON CONSTRAINT chk_workspace_checkpoints_terminal_metadata ON workspace_checkpoints IS
  'Expired Checkpoints retain immutable ready or failure evidence; active/non-terminal states cannot carry failure metadata.';

COMMENT ON COLUMN artifacts.upload_cleanup_at IS
  'First successful temporary-upload cleanup; upload_expires_at becomes the bounded late-PUT grace deadline until the final cleanup pass clears both fields.';

COMMENT ON CONSTRAINT fk_provider_runtime_bindings_last_execution ON provider_runtime_bindings IS
  'Deleting an Execution clears only the optional reference and never the binding tenant identity.';

COMMENT ON CONSTRAINT fk_remote_workspaces_last_execution ON remote_workspaces IS
  'Deleting an Execution clears only the optional reference and never the Workspace tenant identity.';
