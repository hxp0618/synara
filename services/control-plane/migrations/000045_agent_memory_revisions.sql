ALTER TABLE artifacts
  DROP CONSTRAINT IF EXISTS artifacts_kind_check;

ALTER TABLE artifacts
  ADD CONSTRAINT artifacts_kind_check
  CHECK (kind IN ('attachment', 'generated_file', 'terminal_log', 'diff', 'workspace_snapshot', 'checkpoint', 'memory'));

CREATE TABLE agent_memory_heads (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL,
  scope_type TEXT NOT NULL CHECK (scope_type IN ('user', 'project', 'session')),
  scope_user_id UUID,
  scope_project_id UUID,
  scope_session_id UUID,
  memory_key TEXT NOT NULL,
  current_revision_id UUID,
  version BIGINT NOT NULL DEFAULT 0 CHECK (version >= 0),
  enabled BOOLEAN NOT NULL DEFAULT FALSE,
  updated_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  FOREIGN KEY (tenant_id, scope_user_id)
    REFERENCES tenant_memberships(tenant_id, user_id) ON DELETE RESTRICT,
  FOREIGN KEY (tenant_id, scope_project_id)
    REFERENCES projects(tenant_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (tenant_id, scope_session_id)
    REFERENCES agent_sessions(tenant_id, id) ON DELETE RESTRICT,
  CHECK (memory_key ~ '^[a-z][a-z0-9._-]{0,159}$'),
  CHECK (
    (scope_type = 'user' AND scope_user_id IS NOT NULL AND scope_project_id IS NULL AND scope_session_id IS NULL)
    OR
    (scope_type = 'project' AND scope_user_id IS NULL AND scope_project_id IS NOT NULL AND scope_session_id IS NULL)
    OR
    (scope_type = 'session' AND scope_user_id IS NULL AND scope_project_id IS NULL AND scope_session_id IS NOT NULL)
  ),
  CHECK ((version = 0 AND current_revision_id IS NULL AND enabled = FALSE)
    OR (version > 0 AND current_revision_id IS NOT NULL))
);

CREATE UNIQUE INDEX uq_agent_memory_heads_user_key
  ON agent_memory_heads (tenant_id, scope_user_id, memory_key)
  WHERE scope_type = 'user';

CREATE UNIQUE INDEX uq_agent_memory_heads_project_key
  ON agent_memory_heads (tenant_id, scope_project_id, memory_key)
  WHERE scope_type = 'project';

CREATE UNIQUE INDEX uq_agent_memory_heads_session_key
  ON agent_memory_heads (tenant_id, scope_session_id, memory_key)
  WHERE scope_type = 'session';

CREATE TABLE agent_memory_revisions (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL,
  memory_head_id UUID NOT NULL,
  revision_number BIGINT NOT NULL CHECK (revision_number > 0),
  artifact_id UUID NOT NULL,
  sha256 TEXT NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
  media_type TEXT NOT NULL CHECK (media_type IN ('text/plain', 'text/markdown', 'application/json')),
  size_bytes BIGINT NOT NULL CHECK (size_bytes BETWEEN 0 AND 262144),
  created_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, memory_head_id, revision_number),
  FOREIGN KEY (tenant_id, memory_head_id)
    REFERENCES agent_memory_heads(tenant_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (tenant_id, artifact_id)
    REFERENCES artifacts(tenant_id, id) ON DELETE RESTRICT
);

ALTER TABLE agent_memory_heads
  ADD CONSTRAINT fk_agent_memory_heads_current_revision
  FOREIGN KEY (tenant_id, current_revision_id)
  REFERENCES agent_memory_revisions(tenant_id, id)
  ON DELETE RESTRICT;

CREATE INDEX idx_agent_memory_revisions_artifact
  ON agent_memory_revisions (tenant_id, artifact_id, id);

CREATE OR REPLACE FUNCTION validate_agent_memory_revision_insert()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  memory_head agent_memory_heads%ROWTYPE;
  memory_artifact artifacts%ROWTYPE;
BEGIN
  SELECT * INTO memory_head
  FROM agent_memory_heads
  WHERE tenant_id = NEW.tenant_id AND id = NEW.memory_head_id
  FOR UPDATE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'Memory Head is unavailable for this Revision'
      USING ERRCODE = '23514';
  END IF;

  IF NEW.revision_number <> memory_head.version + 1 THEN
    RAISE EXCEPTION 'Memory Revision number must advance the locked Head by exactly one'
      USING ERRCODE = '23514';
  END IF;

  -- Serialize publication with Artifact identity changes so the protecting
  -- trigger cannot miss an uncommitted Revision in a concurrent transaction.
  SELECT * INTO memory_artifact
  FROM artifacts
  WHERE tenant_id = NEW.tenant_id AND id = NEW.artifact_id
  FOR SHARE;
  IF NOT FOUND
    OR memory_artifact.kind <> 'memory'
    OR memory_artifact.status <> 'ready'
    OR memory_artifact.deleted_at IS NOT NULL
    OR memory_artifact.sha256 IS DISTINCT FROM NEW.sha256
    OR memory_artifact.size_bytes IS DISTINCT FROM NEW.size_bytes
    OR lower(btrim(split_part(memory_artifact.content_type, ';', 1))) IS DISTINCT FROM NEW.media_type THEN
    RAISE EXCEPTION 'Memory Revision requires an available ready Memory Artifact with the exact content identity'
      USING ERRCODE = '23514';
  END IF;

  IF memory_head.scope_type = 'user'
    AND (memory_artifact.created_by_type <> 'user'
      OR memory_artifact.created_by_id IS DISTINCT FROM memory_head.scope_user_id) THEN
    RAISE EXCEPTION 'User Memory Artifact ownership does not match the Memory scope'
      USING ERRCODE = '23514';
  ELSIF memory_head.scope_type = 'project'
    AND memory_artifact.project_id IS DISTINCT FROM memory_head.scope_project_id THEN
    RAISE EXCEPTION 'Project Memory Artifact ownership does not match the Memory scope'
      USING ERRCODE = '23514';
  ELSIF memory_head.scope_type = 'session'
    AND memory_artifact.session_id IS DISTINCT FROM memory_head.scope_session_id THEN
    RAISE EXCEPTION 'Session Memory Artifact ownership does not match the Memory scope'
      USING ERRCODE = '23514';
  END IF;

  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_agent_memory_revisions_insert
BEFORE INSERT ON agent_memory_revisions
FOR EACH ROW EXECUTE FUNCTION validate_agent_memory_revision_insert();

CREATE OR REPLACE FUNCTION validate_agent_memory_head_update()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  revision_number_value BIGINT;
BEGIN
  IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
    OR NEW.scope_type IS DISTINCT FROM OLD.scope_type
    OR NEW.scope_user_id IS DISTINCT FROM OLD.scope_user_id
    OR NEW.scope_project_id IS DISTINCT FROM OLD.scope_project_id
    OR NEW.scope_session_id IS DISTINCT FROM OLD.scope_session_id
    OR NEW.memory_key IS DISTINCT FROM OLD.memory_key
    OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
    RAISE EXCEPTION 'Memory Head identity and scope are immutable'
      USING ERRCODE = '23514';
  END IF;

  IF NEW.current_revision_id IS DISTINCT FROM OLD.current_revision_id THEN
    IF NEW.version <> OLD.version + 1 OR NEW.current_revision_id IS NULL THEN
      RAISE EXCEPTION 'Memory Head publication must advance exactly one Revision'
        USING ERRCODE = '23514';
    END IF;
    SELECT revision_number INTO revision_number_value
    FROM agent_memory_revisions
    WHERE tenant_id = NEW.tenant_id
      AND id = NEW.current_revision_id
      AND memory_head_id = NEW.id;
    IF NOT FOUND OR revision_number_value <> NEW.version THEN
      RAISE EXCEPTION 'Memory Head current Revision does not match its version and scope'
        USING ERRCODE = '23514';
    END IF;
  ELSIF NEW.version <> OLD.version THEN
    RAISE EXCEPTION 'Memory Head version cannot change without publishing a Revision'
      USING ERRCODE = '23514';
  END IF;

  IF NEW.enabled AND NEW.current_revision_id IS NULL THEN
    RAISE EXCEPTION 'Enabled Memory Head requires a current Revision'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_agent_memory_heads_validate_update
BEFORE UPDATE ON agent_memory_heads
FOR EACH ROW EXECUTE FUNCTION validate_agent_memory_head_update();

CREATE TRIGGER trg_agent_memory_heads_updated_at
BEFORE UPDATE ON agent_memory_heads
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE OR REPLACE FUNCTION reject_agent_memory_revision_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'Agent Memory Revisions are immutable'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER trg_agent_memory_revisions_immutable
BEFORE UPDATE OR DELETE ON agent_memory_revisions
FOR EACH ROW EXECUTE FUNCTION reject_agent_memory_revision_mutation();

CREATE OR REPLACE FUNCTION protect_agent_memory_artifact()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF EXISTS (
    SELECT 1
    FROM agent_memory_revisions AS revision
    WHERE revision.tenant_id = OLD.tenant_id
      AND revision.artifact_id = OLD.id
  ) AND (
    NEW.kind <> 'memory'
    OR NEW.status <> 'ready'
    OR NEW.deleted_at IS NOT NULL
    OR NEW.sha256 IS DISTINCT FROM OLD.sha256
    OR NEW.content_type IS DISTINCT FROM OLD.content_type
    OR NEW.size_bytes IS DISTINCT FROM OLD.size_bytes
    OR NEW.object_key IS DISTINCT FROM OLD.object_key
  ) THEN
    RAISE EXCEPTION 'Artifact is pinned by an immutable Agent Memory Revision'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_artifacts_agent_memory_protected
BEFORE UPDATE OF kind, status, deleted_at, sha256, content_type, size_bytes, object_key ON artifacts
FOR EACH ROW EXECUTE FUNCTION protect_agent_memory_artifact();

COMMENT ON TABLE agent_memory_heads IS
  'Mutable scope/key pointer for User, Project, or Session Agent Memory; every publication advances an immutable Revision.';
COMMENT ON TABLE agent_memory_revisions IS
  'Immutable Agent Memory publication bound to one ready Artifact SHA-256, normalized media type, and exact byte size; Recovery Bundles freeze exact Revision references.';

COMMENT ON TRIGGER trg_artifacts_agent_memory_protected ON artifacts IS
  'Pinned payload identity is immutable. bucket/object_version may change for verified payload migration because object_key stays fixed and every delivery revalidates SHA-256, byte size, and media type.';
