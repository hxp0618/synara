CREATE TABLE remote_workspaces (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL,
  organization_id UUID NOT NULL,
  project_id UUID NOT NULL,
  session_id UUID NOT NULL,
  execution_target_id UUID NOT NULL REFERENCES execution_targets(id) ON DELETE RESTRICT,
  workspace_mode TEXT NOT NULL CHECK (workspace_mode IN ('empty', 'clone', 'worktree', 'snapshot')),
  state TEXT NOT NULL DEFAULT 'pending'
    CHECK (state IN ('pending', 'preparing', 'ready', 'dirty', 'checkpointing', 'recovering', 'cleanup-pending', 'cleaned', 'failed')),
  repository_fingerprint TEXT,
  default_branch TEXT NOT NULL,
  current_branch TEXT,
  base_commit TEXT,
  head_commit TEXT,
  last_worker_id UUID REFERENCES worker_instances(id) ON DELETE SET NULL,
  last_execution_id UUID,
  last_generation BIGINT CHECK (last_generation IS NULL OR last_generation > 0),
  current_checkpoint_id UUID,
  retention_until TIMESTAMPTZ,
  last_used_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  cleaned_at TIMESTAMPTZ,
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, session_id),
  FOREIGN KEY (tenant_id, organization_id, project_id)
    REFERENCES projects(tenant_id, organization_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (tenant_id, project_id, session_id)
    REFERENCES agent_sessions(tenant_id, project_id, id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, last_execution_id)
    REFERENCES agent_executions(tenant_id, id) ON DELETE SET NULL,
  CHECK (repository_fingerprint IS NULL OR repository_fingerprint ~ '^[0-9a-f]{64}$'),
  CHECK (length(default_branch) BETWEEN 1 AND 255),
  CHECK (current_branch IS NULL OR length(current_branch) BETWEEN 1 AND 255),
  CHECK (base_commit IS NULL OR length(base_commit) BETWEEN 7 AND 128),
  CHECK (head_commit IS NULL OR length(head_commit) BETWEEN 7 AND 128),
  CHECK ((state = 'cleaned' AND cleaned_at IS NOT NULL) OR (state <> 'cleaned' AND cleaned_at IS NULL)),
  CHECK (retention_until IS NULL OR retention_until > created_at)
);

CREATE TABLE workspace_checkpoints (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL,
  workspace_id UUID NOT NULL,
  session_id UUID NOT NULL,
  turn_id UUID,
  execution_id UUID NOT NULL,
  generation BIGINT NOT NULL CHECK (generation > 0),
  idempotency_key TEXT NOT NULL,
  strategy TEXT NOT NULL CHECK (strategy IN ('git-reference', 'patch', 'snapshot')),
  status TEXT NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending', 'uploading', 'ready', 'failed', 'superseded', 'expired')),
  base_commit TEXT,
  head_commit TEXT,
  current_branch TEXT,
  artifact_id UUID,
  manifest JSONB NOT NULL DEFAULT '{}'::jsonb,
  file_count INTEGER CHECK (file_count IS NULL OR file_count >= 0),
  total_bytes BIGINT CHECK (total_bytes IS NULL OR total_bytes >= 0),
  sha256 TEXT,
  failure_code TEXT,
  failure_message TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  ready_at TIMESTAMPTZ,
  failed_at TIMESTAMPTZ,
  expires_at TIMESTAMPTZ,
  UNIQUE (tenant_id, workspace_id, id),
  UNIQUE (tenant_id, workspace_id, idempotency_key),
  FOREIGN KEY (tenant_id, workspace_id)
    REFERENCES remote_workspaces(tenant_id, id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, session_id, turn_id)
    REFERENCES agent_turns(tenant_id, session_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (tenant_id, execution_id)
    REFERENCES agent_executions(tenant_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (tenant_id, artifact_id)
    REFERENCES artifacts(tenant_id, id) ON DELETE RESTRICT,
  CHECK (length(idempotency_key) BETWEEN 1 AND 200),
  CHECK (base_commit IS NULL OR length(base_commit) BETWEEN 7 AND 128),
  CHECK (head_commit IS NULL OR length(head_commit) BETWEEN 7 AND 128),
  CHECK (current_branch IS NULL OR length(current_branch) BETWEEN 1 AND 255),
  CHECK (sha256 IS NULL OR sha256 ~ '^[0-9a-f]{64}$'),
  CHECK (failure_code IS NULL OR length(failure_code) BETWEEN 1 AND 160),
  CHECK (failure_message IS NULL OR length(failure_message) <= 10000),
  CHECK (expires_at IS NULL OR expires_at > created_at),
  CHECK (
    (status = 'ready' AND artifact_id IS NOT NULL AND sha256 IS NOT NULL AND ready_at IS NOT NULL AND failed_at IS NULL) OR
    status <> 'ready'
  ),
  CHECK (
    (status = 'failed' AND failure_code IS NOT NULL AND failed_at IS NOT NULL AND ready_at IS NULL) OR
    status <> 'failed'
  )
);

ALTER TABLE remote_workspaces
  ADD CONSTRAINT fk_remote_workspaces_current_checkpoint
  FOREIGN KEY (tenant_id, id, current_checkpoint_id)
  REFERENCES workspace_checkpoints(tenant_id, workspace_id, id) ON DELETE RESTRICT
  DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE agent_executions
  ADD COLUMN remote_workspace_id UUID,
  ADD COLUMN restore_checkpoint_id UUID,
  ADD CONSTRAINT fk_agent_executions_remote_workspace
    FOREIGN KEY (tenant_id, remote_workspace_id)
    REFERENCES remote_workspaces(tenant_id, id) ON DELETE RESTRICT,
  ADD CONSTRAINT fk_agent_executions_restore_checkpoint
    FOREIGN KEY (tenant_id, remote_workspace_id, restore_checkpoint_id)
    REFERENCES workspace_checkpoints(tenant_id, workspace_id, id) ON DELETE RESTRICT,
  ADD CONSTRAINT chk_agent_executions_restore_checkpoint_workspace
    CHECK (restore_checkpoint_id IS NULL OR remote_workspace_id IS NOT NULL);

CREATE INDEX idx_remote_workspaces_state_retention
  ON remote_workspaces (state, retention_until, last_used_at, id);

CREATE INDEX idx_remote_workspaces_target_state
  ON remote_workspaces (execution_target_id, state, updated_at, id);

CREATE INDEX idx_workspace_checkpoints_workspace_created
  ON workspace_checkpoints (tenant_id, workspace_id, created_at DESC, id);

CREATE INDEX idx_workspace_checkpoints_expiry
  ON workspace_checkpoints (expires_at, status, id)
  WHERE expires_at IS NOT NULL AND status IN ('ready', 'failed', 'superseded');

INSERT INTO remote_workspaces (
  id, tenant_id, organization_id, project_id, session_id, execution_target_id,
  workspace_mode, state, default_branch, created_at, updated_at
)
SELECT
  (
    substr(md5(session.tenant_id::text || ':' || session.id::text || ':remote-workspace'), 1, 8) || '-' ||
    substr(md5(session.tenant_id::text || ':' || session.id::text || ':remote-workspace'), 9, 4) || '-' ||
    substr(md5(session.tenant_id::text || ':' || session.id::text || ':remote-workspace'), 13, 4) || '-' ||
    substr(md5(session.tenant_id::text || ':' || session.id::text || ':remote-workspace'), 17, 4) || '-' ||
    substr(md5(session.tenant_id::text || ':' || session.id::text || ':remote-workspace'), 21, 12)
  )::uuid,
  session.tenant_id,
  session.organization_id,
  session.project_id,
  session.id,
  session.execution_target_id,
  CASE WHEN project.repository_url IS NULL THEN 'empty' ELSE 'clone' END,
  'pending',
  project.default_branch,
  session.created_at,
  session.updated_at
FROM agent_sessions session
JOIN projects project
  ON project.tenant_id = session.tenant_id AND project.id = session.project_id
ON CONFLICT (tenant_id, session_id) DO NOTHING;

UPDATE agent_executions AS execution
SET remote_workspace_id = workspace.id
FROM remote_workspaces AS workspace
WHERE workspace.tenant_id = execution.tenant_id
  AND workspace.session_id = execution.session_id
  AND execution.remote_workspace_id IS NULL;

CREATE OR REPLACE FUNCTION assert_execution_workspace_scope()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.remote_workspace_id IS NULL THEN
    RETURN NEW;
  END IF;
  IF NOT EXISTS (
    SELECT 1
    FROM remote_workspaces workspace
    WHERE workspace.tenant_id = NEW.tenant_id
      AND workspace.id = NEW.remote_workspace_id
      AND workspace.session_id = NEW.session_id
      AND workspace.execution_target_id = NEW.execution_target_id
  ) THEN
    RAISE EXCEPTION 'remote Workspace does not match the execution Session and Target'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE CONSTRAINT TRIGGER trg_agent_executions_workspace_scope
AFTER INSERT OR UPDATE OF tenant_id, session_id, execution_target_id, remote_workspace_id ON agent_executions
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_execution_workspace_scope();

CREATE OR REPLACE FUNCTION assert_ready_checkpoint_artifact()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.status = 'ready' AND NOT EXISTS (
    SELECT 1
    FROM artifacts artifact
    WHERE artifact.tenant_id = NEW.tenant_id
      AND artifact.id = NEW.artifact_id
      AND artifact.session_id = NEW.session_id
      AND artifact.execution_id = NEW.execution_id
      AND artifact.kind IN ('checkpoint', 'workspace_snapshot')
      AND artifact.status = 'ready'
      AND artifact.sha256 = NEW.sha256
  ) THEN
    RAISE EXCEPTION 'ready Checkpoint requires a ready matching Artifact'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE CONSTRAINT TRIGGER trg_workspace_checkpoints_ready_artifact
AFTER INSERT OR UPDATE OF status, artifact_id, sha256 ON workspace_checkpoints
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_ready_checkpoint_artifact();

COMMENT ON TABLE remote_workspaces IS
  'Logical, recoverable Session Workspace identity; Worker paths and credentials are intentionally not persisted.';
COMMENT ON TABLE workspace_checkpoints IS
  'Idempotent Git/Patch/Snapshot recovery point whose payload is stored only as a ready Artifact.';
