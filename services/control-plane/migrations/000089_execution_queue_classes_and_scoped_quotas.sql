ALTER TABLE agent_executions
  ADD COLUMN automation_id UUID REFERENCES automations(id) ON DELETE RESTRICT,
  ADD COLUMN queue_class TEXT NOT NULL DEFAULT 'interactive',
  ADD COLUMN queue_priority INTEGER NOT NULL DEFAULT 0,
  ADD COLUMN quota_units INTEGER NOT NULL DEFAULT 1;

ALTER TABLE agent_executions
  ADD CONSTRAINT chk_agent_executions_queue_class
    CHECK (queue_class IN ('interactive', 'automation', 'batch')),
  ADD CONSTRAINT chk_agent_executions_queue_priority
    CHECK (queue_priority BETWEEN -100 AND 100),
  ADD CONSTRAINT chk_agent_executions_quota_units
    CHECK (quota_units BETWEEN 1 AND 1000000),
  ADD CONSTRAINT chk_agent_executions_automation_class
    CHECK ((queue_class = 'automation') = (automation_id IS NOT NULL));

CREATE INDEX idx_agent_executions_service_queue
  ON agent_executions (
    execution_target_id,
    target_kind,
    queue_class,
    queue_priority DESC,
    queued_at,
    id
  )
  WHERE status IN ('queued', 'recovering');

ALTER TABLE tenant_quotas
  ADD COLUMN max_queued_executions INTEGER,
  ADD COLUMN max_concurrent_execution_units BIGINT;

ALTER TABLE tenant_quotas
  ADD CONSTRAINT chk_tenant_quotas_max_queued_executions
    CHECK (max_queued_executions IS NULL OR max_queued_executions > 0),
  ADD CONSTRAINT chk_tenant_quotas_max_concurrent_execution_units
    CHECK (max_concurrent_execution_units IS NULL OR max_concurrent_execution_units > 0);

CREATE TABLE execution_quota_policies (
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  scope_kind TEXT NOT NULL CHECK (scope_kind IN ('project', 'session', 'automation')),
  scope_id UUID NOT NULL,
  max_concurrent_executions INTEGER
    CHECK (max_concurrent_executions IS NULL OR max_concurrent_executions > 0),
  max_queued_executions INTEGER
    CHECK (max_queued_executions IS NULL OR max_queued_executions > 0),
  max_concurrent_execution_units BIGINT
    CHECK (max_concurrent_execution_units IS NULL OR max_concurrent_execution_units > 0),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  updated_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, scope_kind, scope_id),
  CHECK (
    max_concurrent_executions IS NOT NULL OR
    max_queued_executions IS NOT NULL OR
    max_concurrent_execution_units IS NOT NULL
  )
);

CREATE INDEX idx_execution_quota_policies_scope
  ON execution_quota_policies (tenant_id, scope_kind, scope_id);

DROP TRIGGER IF EXISTS trg_execution_quota_policies_updated_at ON execution_quota_policies;
CREATE TRIGGER trg_execution_quota_policies_updated_at
BEFORE UPDATE ON execution_quota_policies
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE OR REPLACE FUNCTION enforce_execution_quota_policy_scope()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'UPDATE' THEN
    IF NEW.tenant_id <> OLD.tenant_id
       OR NEW.scope_kind <> OLD.scope_kind
       OR NEW.scope_id <> OLD.scope_id THEN
      RAISE EXCEPTION 'Execution quota policy scope is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.version <> OLD.version + 1 THEN
      RAISE EXCEPTION 'Execution quota policy version must advance exactly once' USING ERRCODE = '23514';
    END IF;
  END IF;

  IF NEW.scope_kind = 'project' AND NOT EXISTS (
    SELECT 1 FROM projects p
    WHERE p.tenant_id = NEW.tenant_id AND p.id = NEW.scope_id AND p.archived_at IS NULL
  ) THEN
    RAISE EXCEPTION 'Execution quota Project scope is outside its Tenant' USING ERRCODE = '23514';
  ELSIF NEW.scope_kind = 'session' AND NOT EXISTS (
    SELECT 1 FROM agent_sessions s
    WHERE s.tenant_id = NEW.tenant_id AND s.id = NEW.scope_id AND s.archived_at IS NULL
  ) THEN
    RAISE EXCEPTION 'Execution quota Session scope is outside its Tenant' USING ERRCODE = '23514';
  ELSIF NEW.scope_kind = 'automation' AND NOT EXISTS (
    SELECT 1 FROM automations a
    WHERE a.tenant_id = NEW.tenant_id AND a.id = NEW.scope_id AND a.archived_at IS NULL
  ) THEN
    RAISE EXCEPTION 'Execution quota Automation scope is outside its Tenant' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_execution_quota_policies_scope ON execution_quota_policies;
CREATE TRIGGER trg_execution_quota_policies_scope
BEFORE INSERT OR UPDATE ON execution_quota_policies
FOR EACH ROW EXECUTE FUNCTION enforce_execution_quota_policy_scope();

CREATE OR REPLACE FUNCTION enforce_agent_execution_queue_scope()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'UPDATE' AND (
    NEW.automation_id IS DISTINCT FROM OLD.automation_id
    OR NEW.queue_class <> OLD.queue_class
    OR NEW.queue_priority <> OLD.queue_priority
    OR NEW.quota_units <> OLD.quota_units
  ) THEN
    RAISE EXCEPTION 'Execution queue and quota snapshot is immutable' USING ERRCODE = '23514';
  END IF;

  IF NEW.automation_id IS NOT NULL AND NOT EXISTS (
    SELECT 1
    FROM automations a
    JOIN agent_sessions s
      ON s.tenant_id = a.tenant_id AND s.project_id = a.project_id
    WHERE a.tenant_id = NEW.tenant_id
      AND a.id = NEW.automation_id
      AND a.archived_at IS NULL
      AND s.id = NEW.session_id
      AND s.archived_at IS NULL
  ) THEN
    RAISE EXCEPTION 'Execution Automation is outside its Session Project' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_agent_executions_queue_scope ON agent_executions;
CREATE TRIGGER trg_agent_executions_queue_scope
BEFORE INSERT OR UPDATE OF automation_id, queue_class, queue_priority, quota_units ON agent_executions
FOR EACH ROW EXECUTE FUNCTION enforce_agent_execution_queue_scope();
