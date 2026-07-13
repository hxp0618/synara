ALTER TABLE agent_executions
  ADD COLUMN provider_credential_id_snapshot UUID,
  ADD COLUMN provider_credential_version_snapshot INTEGER,
  ADD COLUMN provider_resume_strategy_snapshot TEXT NOT NULL DEFAULT 'authoritative-history',
  ADD COLUMN provider_cursor_binding_version INTEGER,
  ADD COLUMN provider_cursor_binding_digest BYTEA,
  ADD CONSTRAINT fk_agent_executions_provider_credential_snapshot
    FOREIGN KEY (tenant_id, provider_credential_id_snapshot)
    REFERENCES provider_credentials(tenant_id, id) ON DELETE RESTRICT,
  ADD CONSTRAINT chk_agent_executions_provider_credential_snapshot
    CHECK (
      (
        provider_credential_id_snapshot IS NULL
        AND provider_credential_version_snapshot IS NULL
      )
      OR
      (
        provider_credential_id_snapshot IS NOT NULL
        AND provider_credential_version_snapshot IS NOT NULL
        AND provider_credential_version_snapshot > 0
      )
    ),
  ADD CONSTRAINT chk_agent_executions_provider_resume_strategy_snapshot
    CHECK (provider_resume_strategy_snapshot IN ('authoritative-history', 'native-cursor')),
  ADD CONSTRAINT chk_agent_executions_provider_cursor_binding_all_or_none
    CHECK (
      (provider_cursor_binding_version IS NULL) =
      (provider_cursor_binding_digest IS NULL)
    ),
  ADD CONSTRAINT chk_agent_executions_provider_cursor_binding_shape
    CHECK (
      (
        provider_resume_strategy_snapshot = 'authoritative-history'
        AND provider_cursor_binding_version IS NULL
        AND provider_cursor_binding_digest IS NULL
      )
      OR
      (
        provider_resume_strategy_snapshot = 'native-cursor'
        AND provider IS NOT NULL
        AND worker_manifest_id IS NOT NULL
        AND provider_cursor_binding_version IS NOT NULL
        AND provider_cursor_binding_version > 0
        AND octet_length(provider_cursor_binding_digest) = 32
      )
    );

CREATE INDEX idx_agent_executions_provider_credential_snapshot
  ON agent_executions (
    tenant_id,
    provider_credential_id_snapshot,
    provider_credential_version_snapshot,
    id
  )
  WHERE provider_credential_id_snapshot IS NOT NULL;

CREATE OR REPLACE FUNCTION reject_agent_execution_snapshot_rewrite()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.generation <= OLD.generation AND (
    NEW.provider IS DISTINCT FROM OLD.provider OR
    NEW.worker_manifest_id IS DISTINCT FROM OLD.worker_manifest_id OR
    NEW.provider_credential_id_snapshot IS DISTINCT FROM OLD.provider_credential_id_snapshot OR
    NEW.provider_credential_version_snapshot IS DISTINCT FROM OLD.provider_credential_version_snapshot OR
    NEW.provider_resume_strategy_snapshot IS DISTINCT FROM OLD.provider_resume_strategy_snapshot OR
    NEW.provider_cursor_binding_version IS DISTINCT FROM OLD.provider_cursor_binding_version OR
    NEW.provider_cursor_binding_digest IS DISTINCT FROM OLD.provider_cursor_binding_digest
  ) THEN
    RAISE EXCEPTION 'execution Provider snapshot can change only when generation increases'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_agent_executions_snapshot_immutable ON agent_executions;
CREATE TRIGGER trg_agent_executions_snapshot_immutable
BEFORE UPDATE OF
  generation,
  provider,
  worker_manifest_id,
  provider_credential_id_snapshot,
  provider_credential_version_snapshot,
  provider_resume_strategy_snapshot,
  provider_cursor_binding_version,
  provider_cursor_binding_digest
ON agent_executions
FOR EACH ROW EXECUTE FUNCTION reject_agent_execution_snapshot_rewrite();

CREATE OR REPLACE FUNCTION reject_content_addressed_manifest_update()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION '% rows are immutable; insert a new content-addressed manifest instead', TG_TABLE_NAME
    USING ERRCODE = '23514';
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_manifests_immutable ON worker_manifests;
CREATE TRIGGER trg_worker_manifests_immutable
BEFORE UPDATE ON worker_manifests
FOR EACH ROW EXECUTE FUNCTION reject_content_addressed_manifest_update();

DROP TRIGGER IF EXISTS trg_worker_provider_manifests_immutable ON worker_provider_manifests;
CREATE TRIGGER trg_worker_provider_manifests_immutable
BEFORE UPDATE ON worker_provider_manifests
FOR EACH ROW EXECUTE FUNCTION reject_content_addressed_manifest_update();

COMMENT ON COLUMN agent_executions.provider_credential_id_snapshot IS
  'Provider Credential selected under lock for this Execution generation. Legacy generations remain NULL rather than guessing from mutable Session state.';
COMMENT ON COLUMN agent_executions.provider_credential_version_snapshot IS
  'Provider Credential version selected under lock for this Execution generation.';
COMMENT ON COLUMN agent_executions.provider_resume_strategy_snapshot IS
  'Immutable resume strategy selected for this Execution generation. Legacy generations safely fall back to authoritative history.';
COMMENT ON COLUMN agent_executions.provider_cursor_binding_digest IS
  'SHA-256 digest of the immutable Credential and Provider runtime binding accepted for native Cursor resume in this Execution generation.';
