CREATE TABLE tenant_resource_lifecycle_policies (
  tenant_id UUID PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
  waiting_keep_alive_seconds INTEGER,
  suspend_after_idle_seconds INTEGER,
  absolute_session_lifetime_seconds INTEGER,
  workspace_retention_days INTEGER,
  warm_pool_mode TEXT,
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  updated_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (waiting_keep_alive_seconds IS NULL OR waiting_keep_alive_seconds BETWEEN 60 AND 86400),
  CHECK (suspend_after_idle_seconds IS NULL OR suspend_after_idle_seconds BETWEEN 60 AND 604800),
  CHECK (absolute_session_lifetime_seconds IS NULL OR absolute_session_lifetime_seconds BETWEEN 3600 AND 31536000),
  CHECK (workspace_retention_days IS NULL OR workspace_retention_days BETWEEN 1 AND 3650),
  CHECK (warm_pool_mode IS NULL OR warm_pool_mode IN ('disabled', 'balanced', 'low-latency'))
);

CREATE TRIGGER trg_tenant_resource_lifecycle_policies_updated_at
BEFORE UPDATE ON tenant_resource_lifecycle_policies
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE project_resource_lifecycle_policies (
  project_id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL,
  waiting_keep_alive_seconds INTEGER,
  suspend_after_idle_seconds INTEGER,
  absolute_session_lifetime_seconds INTEGER,
  workspace_retention_days INTEGER,
  warm_pool_mode TEXT,
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  updated_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, project_id),
  FOREIGN KEY (tenant_id, project_id)
    REFERENCES projects(tenant_id, id) ON DELETE CASCADE,
  CHECK (waiting_keep_alive_seconds IS NULL OR waiting_keep_alive_seconds BETWEEN 60 AND 86400),
  CHECK (suspend_after_idle_seconds IS NULL OR suspend_after_idle_seconds BETWEEN 60 AND 604800),
  CHECK (absolute_session_lifetime_seconds IS NULL OR absolute_session_lifetime_seconds BETWEEN 3600 AND 31536000),
  CHECK (workspace_retention_days IS NULL OR workspace_retention_days BETWEEN 1 AND 3650),
  CHECK (warm_pool_mode IS NULL OR warm_pool_mode IN ('disabled', 'balanced', 'low-latency'))
);

CREATE INDEX idx_project_resource_lifecycle_policies_tenant
  ON project_resource_lifecycle_policies (tenant_id, project_id);

CREATE TRIGGER trg_project_resource_lifecycle_policies_updated_at
BEFORE UPDATE ON project_resource_lifecycle_policies
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE agent_sessions
  ADD COLUMN waiting_keep_alive_seconds INTEGER NOT NULL DEFAULT 900,
  ADD COLUMN suspend_after_idle_seconds INTEGER NOT NULL DEFAULT 1800,
  ADD COLUMN absolute_session_lifetime_seconds INTEGER,
  ADD COLUMN workspace_retention_days INTEGER NOT NULL DEFAULT 30,
  ADD COLUMN warm_pool_mode TEXT NOT NULL DEFAULT 'disabled',
  ADD CONSTRAINT chk_agent_sessions_waiting_keep_alive
    CHECK (waiting_keep_alive_seconds BETWEEN 60 AND 86400),
  ADD CONSTRAINT chk_agent_sessions_suspend_after_idle
    CHECK (suspend_after_idle_seconds BETWEEN 60 AND 604800),
  ADD CONSTRAINT chk_agent_sessions_absolute_lifetime
    CHECK (absolute_session_lifetime_seconds IS NULL OR absolute_session_lifetime_seconds BETWEEN 3600 AND 31536000),
  ADD CONSTRAINT chk_agent_sessions_workspace_retention
    CHECK (workspace_retention_days BETWEEN 1 AND 3650),
  ADD CONSTRAINT chk_agent_sessions_warm_pool_mode
    CHECK (warm_pool_mode IN ('disabled', 'balanced', 'low-latency'));

CREATE OR REPLACE FUNCTION protect_agent_session_lifecycle_policy_snapshot()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.waiting_keep_alive_seconds IS DISTINCT FROM OLD.waiting_keep_alive_seconds
    OR NEW.suspend_after_idle_seconds IS DISTINCT FROM OLD.suspend_after_idle_seconds
    OR NEW.absolute_session_lifetime_seconds IS DISTINCT FROM OLD.absolute_session_lifetime_seconds
    OR NEW.workspace_retention_days IS DISTINCT FROM OLD.workspace_retention_days
    OR NEW.warm_pool_mode IS DISTINCT FROM OLD.warm_pool_mode THEN
    RAISE EXCEPTION 'Session Resource Lifecycle Policy snapshot is immutable'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_agent_sessions_lifecycle_policy_immutable
BEFORE UPDATE OF
  waiting_keep_alive_seconds,
  suspend_after_idle_seconds,
  absolute_session_lifetime_seconds,
  workspace_retention_days,
  warm_pool_mode
ON agent_sessions
FOR EACH ROW EXECUTE FUNCTION protect_agent_session_lifecycle_policy_snapshot();

COMMENT ON TABLE tenant_resource_lifecycle_policies IS
  'Tenant override layer for server-authoritative execution-resource lifecycle policy; NULL fields inherit deployment defaults.';
COMMENT ON TABLE project_resource_lifecycle_policies IS
  'Project override layer for server-authoritative execution-resource lifecycle policy; NULL fields inherit the Tenant layer.';
COMMENT ON COLUMN agent_sessions.waiting_keep_alive_seconds IS
  'Frozen effective policy snapshot; later parent-policy edits do not mutate an existing Session.';
