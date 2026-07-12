CREATE TABLE IF NOT EXISTS platform_installations (
  key TEXT PRIMARY KEY,
  installation_id TEXT NOT NULL UNIQUE,
  profile TEXT NOT NULL CHECK (profile IN ('personal', 'single-node', 'enterprise')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (length(key) BETWEEN 1 AND 80),
  CHECK (length(installation_id) BETWEEN 1 AND 160)
);

CREATE TABLE IF NOT EXISTS metadata_imports (
  manifest_id TEXT PRIMARY KEY,
  checksum TEXT NOT NULL,
  imported_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (length(manifest_id) BETWEEN 1 AND 160),
  CHECK (length(checksum) = 64)
);

CREATE TABLE IF NOT EXISTS execution_targets (
  id UUID PRIMARY KEY,
  tenant_id UUID REFERENCES tenants(id) ON DELETE CASCADE,
  organization_id UUID,
  kind TEXT NOT NULL CHECK (kind IN ('local', 'ssh', 'docker', 'kubernetes')),
  name TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled', 'offline')),
  configuration_encrypted BYTEA NOT NULL DEFAULT ''::bytea,
  capabilities JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  FOREIGN KEY (tenant_id, organization_id)
    REFERENCES organizations(tenant_id, id) ON DELETE CASCADE,
  CHECK ((tenant_id IS NULL AND organization_id IS NULL) OR tenant_id IS NOT NULL),
  CHECK (length(btrim(name)) BETWEEN 1 AND 160)
);

CREATE INDEX IF NOT EXISTS idx_execution_targets_tenant_status
  ON execution_targets (tenant_id, organization_id, status, name, id);

WITH owned_targets AS (
  SELECT DISTINCT
    tenant_id,
    organization_id,
    (
      substr(md5(tenant_id::text || ':' || organization_id::text || ':local-default'), 1, 8) || '-' ||
      substr(md5(tenant_id::text || ':' || organization_id::text || ':local-default'), 9, 4) || '-' ||
      substr(md5(tenant_id::text || ':' || organization_id::text || ':local-default'), 13, 4) || '-' ||
      substr(md5(tenant_id::text || ':' || organization_id::text || ':local-default'), 17, 4) || '-' ||
      substr(md5(tenant_id::text || ':' || organization_id::text || ':local-default'), 21, 12)
    )::uuid AS target_id
  FROM agent_sessions
)
INSERT INTO execution_targets (
  id, tenant_id, organization_id, kind, name, status, configuration_encrypted, capabilities
)
SELECT
  target_id, tenant_id, organization_id, 'local', 'local-default', 'active', ''::bytea,
  '{"workspaceModes":["local","worktree"],"migrationBackfill":true}'::jsonb
FROM owned_targets
ON CONFLICT (id) DO NOTHING;

UPDATE agent_sessions AS session
SET execution_target_id = target.id
FROM execution_targets AS target
WHERE (session.execution_target_id IS NULL OR NOT EXISTS (
    SELECT 1 FROM execution_targets existing WHERE existing.id = session.execution_target_id
  ))
  AND target.tenant_id = session.tenant_id
  AND target.organization_id = session.organization_id
  AND target.name = 'local-default';

ALTER TABLE agent_sessions
  ALTER COLUMN execution_target_id SET NOT NULL;

ALTER TABLE agent_sessions
  ADD CONSTRAINT fk_agent_sessions_execution_target
  FOREIGN KEY (execution_target_id) REFERENCES execution_targets(id) ON DELETE RESTRICT;

DROP INDEX IF EXISTS idx_agent_executions_claimable;

ALTER TABLE agent_executions
  ADD COLUMN execution_target_id UUID,
  ADD COLUMN target_kind TEXT;

UPDATE agent_executions AS execution
SET execution_target_id = session.execution_target_id,
    target_kind = target.kind
FROM agent_sessions AS session
JOIN execution_targets AS target ON target.id = session.execution_target_id
WHERE execution.tenant_id = session.tenant_id
  AND execution.session_id = session.id;

ALTER TABLE agent_executions
  ALTER COLUMN execution_target_id SET NOT NULL,
  ALTER COLUMN target_kind SET NOT NULL,
  ADD CONSTRAINT fk_agent_executions_execution_target
    FOREIGN KEY (execution_target_id) REFERENCES execution_targets(id) ON DELETE RESTRICT,
  ADD CONSTRAINT chk_agent_executions_target_kind
    CHECK (target_kind IN ('local', 'ssh', 'docker', 'kubernetes')),
  DROP COLUMN target_type;

CREATE INDEX IF NOT EXISTS idx_agent_executions_claimable
  ON agent_executions (execution_target_id, target_kind, queued_at, id)
  WHERE status IN ('queued', 'recovering');

INSERT INTO execution_targets (
  id, tenant_id, organization_id, kind, name, status, configuration_encrypted, capabilities
)
SELECT
  '00000000-0000-5000-8000-000000000004'::uuid,
  NULL,
  NULL,
  'kubernetes',
  'legacy-worker-pool',
  'active',
  ''::bytea,
  '{"migrationBackfill":true}'::jsonb
WHERE EXISTS (SELECT 1 FROM worker_instances)
ON CONFLICT (id) DO NOTHING;

DROP INDEX IF EXISTS idx_worker_instances_pool_status_heartbeat;

ALTER TABLE worker_instances
  ADD COLUMN execution_target_id UUID,
  ADD COLUMN target_kind TEXT,
  ADD COLUMN lease_supported BOOLEAN NOT NULL DEFAULT false,
  ADD COLUMN fencing_supported BOOLEAN NOT NULL DEFAULT false;

UPDATE worker_instances
SET execution_target_id = '00000000-0000-5000-8000-000000000004'::uuid,
    target_kind = 'kubernetes',
    lease_supported = true,
    fencing_supported = true
WHERE execution_target_id IS NULL;

ALTER TABLE worker_instances
  ALTER COLUMN execution_target_id SET NOT NULL,
  ALTER COLUMN target_kind SET NOT NULL,
  ADD CONSTRAINT fk_worker_instances_execution_target
    FOREIGN KEY (execution_target_id) REFERENCES execution_targets(id) ON DELETE RESTRICT,
  ADD CONSTRAINT chk_worker_instances_target_kind
    CHECK (target_kind IN ('local', 'ssh', 'docker', 'kubernetes')),
  DROP COLUMN pool_id;

CREATE INDEX IF NOT EXISTS idx_worker_instances_target_status_heartbeat
  ON worker_instances (execution_target_id, target_kind, status, last_heartbeat_at, id);

CREATE OR REPLACE FUNCTION assert_execution_target_scope()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  target execution_targets%ROWTYPE;
BEGIN
  SELECT * INTO target FROM execution_targets WHERE id = NEW.execution_target_id;
  IF NOT FOUND OR target.status <> 'active' THEN
    RAISE EXCEPTION 'execution target is missing or inactive' USING ERRCODE = '23514';
  END IF;
  IF target.tenant_id IS NOT NULL AND target.tenant_id <> NEW.tenant_id THEN
    RAISE EXCEPTION 'execution target belongs to another tenant' USING ERRCODE = '23514';
  END IF;
  IF target.organization_id IS NOT NULL AND target.organization_id <> NEW.organization_id THEN
    RAISE EXCEPTION 'execution target belongs to another organization' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_agent_sessions_execution_target_scope ON agent_sessions;
CREATE CONSTRAINT TRIGGER trg_agent_sessions_execution_target_scope
AFTER INSERT OR UPDATE OF tenant_id, organization_id, execution_target_id ON agent_sessions
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_execution_target_scope();

CREATE OR REPLACE FUNCTION assert_execution_target_matches_session()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM agent_sessions session
    JOIN execution_targets target ON target.id = session.execution_target_id
    WHERE session.tenant_id = NEW.tenant_id
      AND session.id = NEW.session_id
      AND session.execution_target_id = NEW.execution_target_id
      AND target.kind = NEW.target_kind
      AND target.status = 'active'
  ) THEN
    RAISE EXCEPTION 'execution target does not match the agent session target'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_agent_executions_target_matches_session ON agent_executions;
CREATE CONSTRAINT TRIGGER trg_agent_executions_target_matches_session
AFTER INSERT OR UPDATE OF tenant_id, session_id, execution_target_id, target_kind ON agent_executions
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_execution_target_matches_session();

CREATE OR REPLACE FUNCTION assert_worker_target_contract()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM execution_targets target
    WHERE target.id = NEW.execution_target_id
      AND target.kind = NEW.target_kind
      AND target.status = 'active'
  ) THEN
    RAISE EXCEPTION 'worker target is missing, inactive, or has another kind'
      USING ERRCODE = '23514';
  END IF;
  IF NEW.target_kind IN ('ssh', 'docker', 'kubernetes')
     AND (NOT NEW.lease_supported OR NOT NEW.fencing_supported) THEN
    RAISE EXCEPTION 'remote workers require lease and fencing support'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_worker_instances_target_contract ON worker_instances;
CREATE CONSTRAINT TRIGGER trg_worker_instances_target_contract
AFTER INSERT OR UPDATE OF execution_target_id, target_kind, lease_supported, fencing_supported ON worker_instances
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_worker_target_contract();

CREATE OR REPLACE FUNCTION reject_execution_target_ownership_change()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id OR
     NEW.organization_id IS DISTINCT FROM OLD.organization_id OR
     NEW.kind <> OLD.kind THEN
    RAISE EXCEPTION 'execution target ownership and kind cannot be changed in place'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_execution_targets_ownership_immutable ON execution_targets;
CREATE TRIGGER trg_execution_targets_ownership_immutable
BEFORE UPDATE OF tenant_id, organization_id, kind ON execution_targets
FOR EACH ROW EXECUTE FUNCTION reject_execution_target_ownership_change();

DROP TRIGGER IF EXISTS trg_platform_installations_updated_at ON platform_installations;
CREATE TRIGGER trg_platform_installations_updated_at BEFORE UPDATE ON platform_installations
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_execution_targets_updated_at ON execution_targets;
CREATE TRIGGER trg_execution_targets_updated_at BEFORE UPDATE ON execution_targets
FOR EACH ROW EXECUTE FUNCTION set_updated_at();
