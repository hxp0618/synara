CREATE TABLE IF NOT EXISTS projects (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL,
  organization_id UUID NOT NULL,
  name TEXT NOT NULL,
  repository_url TEXT,
  default_branch TEXT NOT NULL DEFAULT 'main',
  visibility TEXT NOT NULL DEFAULT 'organization'
    CHECK (visibility IN ('private', 'organization', 'tenant')),
  created_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  archived_at TIMESTAMPTZ,
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, organization_id, id),
  FOREIGN KEY (tenant_id, organization_id)
    REFERENCES organizations(tenant_id, id) ON DELETE RESTRICT,
  CHECK (length(btrim(name)) BETWEEN 1 AND 200),
  CHECK (length(default_branch) BETWEEN 1 AND 255),
  CHECK (repository_url IS NULL OR length(repository_url) BETWEEN 1 AND 2048)
);

CREATE INDEX IF NOT EXISTS idx_projects_organization_active
  ON projects (tenant_id, organization_id, created_at DESC, id)
  WHERE archived_at IS NULL;

CREATE TABLE IF NOT EXISTS agent_sessions (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL,
  organization_id UUID NOT NULL,
  project_id UUID NOT NULL,
  created_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  title TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active'
    CHECK (status IN ('active', 'archived')),
  visibility TEXT NOT NULL DEFAULT 'private'
    CHECK (visibility IN ('private', 'project', 'organization')),
  provider TEXT NOT NULL,
  model TEXT,
  execution_target_id UUID,
  provider_resume_cursor_encrypted BYTEA,
  last_event_sequence BIGINT NOT NULL DEFAULT 0 CHECK (last_event_sequence >= 0),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  archived_at TIMESTAMPTZ,
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, project_id, id),
  FOREIGN KEY (tenant_id, organization_id, project_id)
    REFERENCES projects(tenant_id, organization_id, id) ON DELETE RESTRICT,
  CHECK (length(btrim(title)) BETWEEN 1 AND 300),
  CHECK (length(provider) BETWEEN 2 AND 64),
  CHECK (model IS NULL OR length(model) BETWEEN 1 AND 200),
  CHECK ((status = 'archived' AND archived_at IS NOT NULL) OR
         (status = 'active' AND archived_at IS NULL))
);

CREATE INDEX IF NOT EXISTS idx_agent_sessions_project_active
  ON agent_sessions (tenant_id, project_id, updated_at DESC, id)
  WHERE archived_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_agent_sessions_creator_private
  ON agent_sessions (tenant_id, created_by, updated_at DESC, id)
  WHERE visibility = 'private' AND archived_at IS NULL;

CREATE TABLE IF NOT EXISTS agent_turns (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL,
  session_id UUID NOT NULL,
  created_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  status TEXT NOT NULL DEFAULT 'queued'
    CHECK (status IN ('queued', 'running', 'completed', 'failed', 'cancelled')),
  input_text TEXT NOT NULL,
  started_at TIMESTAMPTZ,
  completed_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, session_id, id),
  FOREIGN KEY (tenant_id, session_id)
    REFERENCES agent_sessions(tenant_id, id) ON DELETE RESTRICT,
  CHECK (length(input_text) BETWEEN 1 AND 1000000),
  CHECK (completed_at IS NULL OR started_at IS NULL OR completed_at >= started_at)
);

CREATE INDEX IF NOT EXISTS idx_agent_turns_session_created
  ON agent_turns (tenant_id, session_id, created_at, id);

CREATE TABLE IF NOT EXISTS session_events (
  tenant_id UUID NOT NULL,
  organization_id UUID NOT NULL,
  project_id UUID NOT NULL,
  session_id UUID NOT NULL,
  sequence BIGINT NOT NULL CHECK (sequence > 0),
  event_id UUID NOT NULL,
  event_version INTEGER NOT NULL DEFAULT 1 CHECK (event_version > 0),
  event_type TEXT NOT NULL,
  actor_type TEXT NOT NULL
    CHECK (actor_type IN ('user', 'service_account', 'worker', 'system')),
  actor_id UUID,
  execution_id UUID,
  worker_id UUID,
  generation BIGINT,
  payload JSONB NOT NULL DEFAULT '{}'::jsonb,
  occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, session_id, sequence),
  UNIQUE (tenant_id, event_id),
  FOREIGN KEY (tenant_id, organization_id, project_id)
    REFERENCES projects(tenant_id, organization_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (tenant_id, project_id, session_id)
    REFERENCES agent_sessions(tenant_id, project_id, id) ON DELETE RESTRICT,
  CHECK (length(event_type) BETWEEN 3 AND 160),
  CHECK ((worker_id IS NULL AND generation IS NULL) OR
         (worker_id IS NOT NULL AND generation IS NOT NULL AND generation > 0))
);

CREATE INDEX IF NOT EXISTS idx_session_events_event_id
  ON session_events (tenant_id, event_id);

CREATE INDEX IF NOT EXISTS idx_session_events_occurred
  ON session_events (tenant_id, session_id, occurred_at, sequence);

CREATE TABLE IF NOT EXISTS automations (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL,
  organization_id UUID NOT NULL,
  project_id UUID NOT NULL,
  created_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  name TEXT NOT NULL,
  prompt TEXT NOT NULL,
  schedule TEXT NOT NULL,
  timezone TEXT NOT NULL DEFAULT 'UTC',
  status TEXT NOT NULL DEFAULT 'active'
    CHECK (status IN ('active', 'paused', 'archived')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  archived_at TIMESTAMPTZ,
  UNIQUE (tenant_id, id),
  FOREIGN KEY (tenant_id, organization_id, project_id)
    REFERENCES projects(tenant_id, organization_id, id) ON DELETE RESTRICT,
  CHECK (length(btrim(name)) BETWEEN 1 AND 200),
  CHECK (length(prompt) BETWEEN 1 AND 1000000),
  CHECK (length(schedule) BETWEEN 1 AND 200),
  CHECK (length(timezone) BETWEEN 1 AND 100),
  CHECK ((status = 'archived' AND archived_at IS NOT NULL) OR status <> 'archived')
);

CREATE INDEX IF NOT EXISTS idx_automations_project_status
  ON automations (tenant_id, project_id, status, created_at DESC, id);

DROP TRIGGER IF EXISTS trg_projects_updated_at ON projects;
CREATE TRIGGER trg_projects_updated_at BEFORE UPDATE ON projects
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_agent_sessions_updated_at ON agent_sessions;
CREATE TRIGGER trg_agent_sessions_updated_at BEFORE UPDATE ON agent_sessions
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_automations_updated_at ON automations;
CREATE TRIGGER trg_automations_updated_at BEFORE UPDATE ON automations
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE OR REPLACE FUNCTION reject_tenant_ownership_change()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.tenant_id <> OLD.tenant_id THEN
    RAISE EXCEPTION 'tenant ownership cannot be changed in place'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_projects_tenant_immutable ON projects;
CREATE TRIGGER trg_projects_tenant_immutable
BEFORE UPDATE OF tenant_id ON projects
FOR EACH ROW EXECUTE FUNCTION reject_tenant_ownership_change();

DROP TRIGGER IF EXISTS trg_agent_sessions_tenant_immutable ON agent_sessions;
CREATE TRIGGER trg_agent_sessions_tenant_immutable
BEFORE UPDATE OF tenant_id ON agent_sessions
FOR EACH ROW EXECUTE FUNCTION reject_tenant_ownership_change();

DROP TRIGGER IF EXISTS trg_agent_turns_tenant_immutable ON agent_turns;
CREATE TRIGGER trg_agent_turns_tenant_immutable
BEFORE UPDATE OF tenant_id ON agent_turns
FOR EACH ROW EXECUTE FUNCTION reject_tenant_ownership_change();

DROP TRIGGER IF EXISTS trg_automations_tenant_immutable ON automations;
CREATE TRIGGER trg_automations_tenant_immutable
BEFORE UPDATE OF tenant_id ON automations
FOR EACH ROW EXECUTE FUNCTION reject_tenant_ownership_change();
