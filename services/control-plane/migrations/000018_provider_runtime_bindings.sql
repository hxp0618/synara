CREATE TABLE provider_runtime_bindings (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL,
  session_id UUID NOT NULL,
  provider TEXT NOT NULL,
  revision INTEGER NOT NULL CHECK (revision > 0),
  status TEXT NOT NULL DEFAULT 'active'
    CHECK (status IN ('active', 'recovering', 'incompatible', 'released')),
  worker_manifest_id UUID REFERENCES worker_manifests(id) ON DELETE RESTRICT,
  capability_descriptor_hash TEXT,
  provider_host_protocol_major INTEGER,
  provider_host_protocol_minor INTEGER,
  adapter_version TEXT,
  provider_cli_version TEXT,
  resume_strategy TEXT NOT NULL DEFAULT 'authoritative-history'
    CHECK (resume_strategy IN ('native-cursor', 'authoritative-history', 'none')),
  cursor_compatibility_key TEXT,
  cursor_updated_at TIMESTAMPTZ,
  authoritative_history_sequence BIGINT NOT NULL DEFAULT 0
    CHECK (authoritative_history_sequence >= 0),
  last_execution_id UUID,
  last_generation BIGINT CHECK (last_generation IS NULL OR last_generation > 0),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  released_at TIMESTAMPTZ,
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, session_id, revision),
  FOREIGN KEY (tenant_id, session_id)
    REFERENCES agent_sessions(tenant_id, id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, last_execution_id)
    REFERENCES agent_executions(tenant_id, id) ON DELETE SET NULL,
  FOREIGN KEY (worker_manifest_id, provider)
    REFERENCES worker_provider_manifests(worker_manifest_id, provider) ON DELETE RESTRICT,
  CHECK (length(provider) BETWEEN 1 AND 80),
  CHECK (capability_descriptor_hash IS NULL OR capability_descriptor_hash ~ '^[0-9a-f]{64}$'),
  CHECK (provider_host_protocol_major IS NULL OR provider_host_protocol_major > 0),
  CHECK (provider_host_protocol_minor IS NULL OR provider_host_protocol_minor >= 0),
  CHECK ((provider_host_protocol_major IS NULL) = (provider_host_protocol_minor IS NULL)),
  CHECK (adapter_version IS NULL OR length(adapter_version) BETWEEN 1 AND 160),
  CHECK (provider_cli_version IS NULL OR length(provider_cli_version) BETWEEN 1 AND 200),
  CHECK (cursor_compatibility_key IS NULL OR cursor_compatibility_key ~ '^[0-9a-f]{64}$'),
  CHECK ((status = 'released' AND released_at IS NOT NULL) OR (status <> 'released' AND released_at IS NULL))
);

ALTER TABLE agent_sessions
  ADD COLUMN current_runtime_binding_id UUID,
  ADD CONSTRAINT fk_agent_sessions_current_runtime_binding
    FOREIGN KEY (tenant_id, current_runtime_binding_id)
    REFERENCES provider_runtime_bindings(tenant_id, id)
    DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE agent_executions
  ADD COLUMN provider_runtime_binding_id UUID,
  ADD CONSTRAINT fk_agent_executions_runtime_binding
    FOREIGN KEY (tenant_id, provider_runtime_binding_id)
    REFERENCES provider_runtime_bindings(tenant_id, id) ON DELETE RESTRICT;

INSERT INTO provider_runtime_bindings (
  id, tenant_id, session_id, provider, revision, status, resume_strategy,
  authoritative_history_sequence, created_at, updated_at
)
SELECT
  (
    substr(md5(session.tenant_id::text || ':' || session.id::text || ':provider-runtime-binding:1'), 1, 8) || '-' ||
    substr(md5(session.tenant_id::text || ':' || session.id::text || ':provider-runtime-binding:1'), 9, 4) || '-' ||
    substr(md5(session.tenant_id::text || ':' || session.id::text || ':provider-runtime-binding:1'), 13, 4) || '-' ||
    substr(md5(session.tenant_id::text || ':' || session.id::text || ':provider-runtime-binding:1'), 17, 4) || '-' ||
    substr(md5(session.tenant_id::text || ':' || session.id::text || ':provider-runtime-binding:1'), 21, 12)
  )::uuid,
  session.tenant_id,
  session.id,
  session.provider,
  1,
  'active',
  CASE
    WHEN octet_length(session.provider_resume_cursor_encrypted) > 0 THEN 'native-cursor'
    ELSE 'authoritative-history'
  END,
  COALESCE((
    SELECT max(event.sequence)
    FROM session_events event
    WHERE event.tenant_id = session.tenant_id AND event.session_id = session.id
  ), 0),
  session.created_at,
  session.updated_at
FROM agent_sessions session
ON CONFLICT (tenant_id, session_id, revision) DO NOTHING;

UPDATE agent_sessions AS session
SET current_runtime_binding_id = binding.id
FROM provider_runtime_bindings AS binding
WHERE binding.tenant_id = session.tenant_id
  AND binding.session_id = session.id
  AND binding.revision = 1
  AND session.current_runtime_binding_id IS NULL;

UPDATE agent_executions AS execution
SET provider_runtime_binding_id = session.current_runtime_binding_id
FROM agent_sessions AS session
WHERE session.tenant_id = execution.tenant_id
  AND session.id = execution.session_id
  AND execution.provider_runtime_binding_id IS NULL;

CREATE OR REPLACE FUNCTION assert_execution_runtime_binding_scope()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.provider_runtime_binding_id IS NULL THEN
    RETURN NEW;
  END IF;
  IF NOT EXISTS (
    SELECT 1
    FROM provider_runtime_bindings binding
    WHERE binding.tenant_id = NEW.tenant_id
      AND binding.id = NEW.provider_runtime_binding_id
      AND binding.session_id = NEW.session_id
      AND binding.provider = NEW.provider
  ) THEN
    RAISE EXCEPTION 'provider runtime binding does not match the execution Session and Provider'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE CONSTRAINT TRIGGER trg_agent_executions_runtime_binding_scope
AFTER INSERT OR UPDATE OF tenant_id, session_id, provider, provider_runtime_binding_id ON agent_executions
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_execution_runtime_binding_scope();

CREATE INDEX idx_provider_runtime_bindings_session
  ON provider_runtime_bindings (tenant_id, session_id, status, revision DESC, id);

CREATE INDEX idx_provider_runtime_bindings_manifest
  ON provider_runtime_bindings (worker_manifest_id, provider, status, updated_at DESC, id)
  WHERE worker_manifest_id IS NOT NULL;

COMMENT ON TABLE provider_runtime_bindings IS
  'Provider runtime compatibility binding for one Agent Session revision; encrypted cursor bytes remain on agent_sessions during the compatibility migration.';
