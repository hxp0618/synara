DO $$
DECLARE
  constraint_name TEXT;
BEGIN
  SELECT conname
  INTO constraint_name
  FROM pg_constraint
  WHERE conrelid = 'workspace_checkpoints'::regclass
    AND contype = 'c'
    AND pg_get_constraintdef(oid) LIKE '%status = ''ready''%'
    AND pg_get_constraintdef(oid) LIKE '%artifact_id IS NOT NULL%'
    AND pg_get_constraintdef(oid) LIKE '%ready_at IS NOT NULL%'
  ORDER BY oid
  LIMIT 1;

  IF constraint_name IS NOT NULL THEN
    EXECUTE format('ALTER TABLE workspace_checkpoints DROP CONSTRAINT %I', constraint_name);
  END IF;
END $$;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM pg_constraint
    WHERE conname = 'chk_workspace_checkpoints_ready_shape'
      AND conrelid = 'workspace_checkpoints'::regclass
  ) THEN
    ALTER TABLE workspace_checkpoints
      ADD CONSTRAINT chk_workspace_checkpoints_ready_shape
      CHECK (
        status <> 'ready' OR (
          ready_at IS NOT NULL
          AND failed_at IS NULL
          AND (
            (
              strategy = 'git-reference'
              AND artifact_id IS NULL
              AND sha256 IS NULL
              AND head_commit IS NOT NULL
              AND current_branch IS NOT NULL
            ) OR
            (
              strategy IN ('patch', 'snapshot')
              AND artifact_id IS NOT NULL
              AND sha256 IS NOT NULL
              AND file_count IS NOT NULL
              AND total_bytes IS NOT NULL
              AND (strategy <> 'patch' OR base_commit IS NOT NULL)
            )
          )
          AND failure_code IS NULL
          AND failure_message IS NULL
        )
      );
  END IF;
END $$;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM pg_constraint
    WHERE conname = 'chk_workspace_checkpoints_terminal_metadata'
      AND conrelid = 'workspace_checkpoints'::regclass
  ) THEN
    ALTER TABLE workspace_checkpoints
      ADD CONSTRAINT chk_workspace_checkpoints_terminal_metadata
      CHECK (
        (status = 'failed' OR (failure_code IS NULL AND failure_message IS NULL AND failed_at IS NULL))
        AND (ready_at IS NULL OR ready_at >= created_at)
        AND (failed_at IS NULL OR failed_at >= created_at)
      );
  END IF;
END $$;

UPDATE workspace_checkpoints AS checkpoint
SET turn_id = execution.turn_id
FROM agent_executions AS execution
WHERE checkpoint.turn_id IS NULL
  AND execution.tenant_id = checkpoint.tenant_id
  AND execution.id = checkpoint.execution_id;

ALTER TABLE workspace_checkpoints
  ALTER COLUMN turn_id SET NOT NULL;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM pg_constraint
    WHERE conname = 'chk_workspace_checkpoints_manifest_object'
      AND conrelid = 'workspace_checkpoints'::regclass
  ) THEN
    ALTER TABLE workspace_checkpoints
      ADD CONSTRAINT chk_workspace_checkpoints_manifest_object
      CHECK (jsonb_typeof(manifest) = 'object');
  END IF;
END $$;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM pg_constraint
    WHERE conname = 'chk_workspace_checkpoints_git_reference_payload'
      AND conrelid = 'workspace_checkpoints'::regclass
  ) THEN
    ALTER TABLE workspace_checkpoints
      ADD CONSTRAINT chk_workspace_checkpoints_git_reference_payload
      CHECK (strategy <> 'git-reference' OR (artifact_id IS NULL AND sha256 IS NULL));
  END IF;
END $$;

CREATE OR REPLACE FUNCTION assert_checkpoint_scope()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM remote_workspaces workspace
    WHERE workspace.tenant_id = NEW.tenant_id
      AND workspace.id = NEW.workspace_id
      AND workspace.session_id = NEW.session_id
  ) THEN
    RAISE EXCEPTION 'Checkpoint does not match the logical Workspace Session'
      USING ERRCODE = '23514';
  END IF;

  IF NOT EXISTS (
    SELECT 1
    FROM agent_executions execution
    WHERE execution.tenant_id = NEW.tenant_id
      AND execution.id = NEW.execution_id
      AND execution.session_id = NEW.session_id
      AND execution.remote_workspace_id = NEW.workspace_id
      AND execution.generation = NEW.generation
      AND execution.turn_id = NEW.turn_id
  ) THEN
    RAISE EXCEPTION 'Checkpoint does not match the Execution Workspace and Generation'
      USING ERRCODE = '23514';
  END IF;

  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_workspace_checkpoints_scope ON workspace_checkpoints;
CREATE CONSTRAINT TRIGGER trg_workspace_checkpoints_scope
AFTER INSERT OR UPDATE OF tenant_id, workspace_id, session_id, turn_id, execution_id, generation
ON workspace_checkpoints
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_checkpoint_scope();

CREATE OR REPLACE FUNCTION protect_checkpoint_identity_and_transition()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.id <> OLD.id
    OR NEW.tenant_id <> OLD.tenant_id
    OR NEW.workspace_id <> OLD.workspace_id
    OR NEW.session_id <> OLD.session_id
    OR NEW.turn_id <> OLD.turn_id
    OR NEW.execution_id <> OLD.execution_id
    OR NEW.generation <> OLD.generation
    OR NEW.idempotency_key <> OLD.idempotency_key
    OR NEW.strategy <> OLD.strategy
    OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'Checkpoint identity fields are immutable'
      USING ERRCODE = '23514';
  END IF;

  IF NOT (
    (OLD.status = 'pending' AND NEW.status IN ('pending', 'uploading', 'ready', 'failed')) OR
    (OLD.status = 'uploading' AND NEW.status IN ('uploading', 'ready', 'failed')) OR
    (OLD.status = 'ready' AND NEW.status IN ('ready', 'superseded', 'expired')) OR
    (OLD.status = 'failed' AND NEW.status IN ('failed', 'expired')) OR
    (OLD.status = 'superseded' AND NEW.status IN ('superseded', 'expired')) OR
    (OLD.status = 'expired' AND NEW.status = 'expired')
  ) THEN
    RAISE EXCEPTION 'Checkpoint status transition is invalid'
      USING ERRCODE = '23514';
  END IF;

  IF OLD.status IN ('ready', 'failed', 'superseded', 'expired')
    AND (
      NEW.artifact_id IS DISTINCT FROM OLD.artifact_id
      OR NEW.sha256 IS DISTINCT FROM OLD.sha256
      OR NEW.base_commit IS DISTINCT FROM OLD.base_commit
      OR NEW.head_commit IS DISTINCT FROM OLD.head_commit
      OR NEW.current_branch IS DISTINCT FROM OLD.current_branch
      OR NEW.manifest IS DISTINCT FROM OLD.manifest
      OR NEW.file_count IS DISTINCT FROM OLD.file_count
      OR NEW.total_bytes IS DISTINCT FROM OLD.total_bytes
      OR NEW.failure_code IS DISTINCT FROM OLD.failure_code
      OR NEW.failure_message IS DISTINCT FROM OLD.failure_message
      OR NEW.ready_at IS DISTINCT FROM OLD.ready_at
      OR NEW.failed_at IS DISTINCT FROM OLD.failed_at
    ) THEN
    RAISE EXCEPTION 'Terminal Checkpoint content is immutable'
      USING ERRCODE = '23514';
  END IF;

  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_workspace_checkpoints_protect_identity ON workspace_checkpoints;
CREATE TRIGGER trg_workspace_checkpoints_protect_identity
BEFORE UPDATE ON workspace_checkpoints
FOR EACH ROW EXECUTE FUNCTION protect_checkpoint_identity_and_transition();

CREATE OR REPLACE FUNCTION protect_remote_workspace_identity()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.id <> OLD.id
    OR NEW.tenant_id <> OLD.tenant_id
    OR NEW.organization_id <> OLD.organization_id
    OR NEW.project_id <> OLD.project_id
    OR NEW.session_id <> OLD.session_id
    OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'Remote Workspace identity fields are immutable'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_remote_workspaces_protect_identity ON remote_workspaces;
CREATE TRIGGER trg_remote_workspaces_protect_identity
BEFORE UPDATE ON remote_workspaces
FOR EACH ROW EXECUTE FUNCTION protect_remote_workspace_identity();

CREATE OR REPLACE FUNCTION assert_ready_checkpoint_artifact()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.status <> 'ready' OR NEW.strategy = 'git-reference' THEN
    RETURN NEW;
  END IF;

  IF NOT EXISTS (
    SELECT 1
    FROM artifacts artifact
    WHERE artifact.tenant_id = NEW.tenant_id
      AND artifact.id = NEW.artifact_id
      AND artifact.session_id = NEW.session_id
      AND artifact.execution_id = NEW.execution_id
      AND artifact.kind = CASE NEW.strategy
        WHEN 'patch' THEN 'checkpoint'
        WHEN 'snapshot' THEN 'workspace_snapshot'
      END
      AND artifact.status = 'ready'
      AND artifact.deleted_at IS NULL
      AND artifact.sha256 = NEW.sha256
  ) THEN
    RAISE EXCEPTION 'ready Checkpoint requires a ready matching Artifact'
      USING ERRCODE = '23514';
  END IF;

  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_workspace_checkpoints_ready_artifact ON workspace_checkpoints;
CREATE CONSTRAINT TRIGGER trg_workspace_checkpoints_ready_artifact
AFTER INSERT OR UPDATE OF tenant_id, session_id, execution_id, strategy, status, artifact_id, sha256
ON workspace_checkpoints
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_ready_checkpoint_artifact();

CREATE OR REPLACE FUNCTION assert_workspace_current_checkpoint_ready()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.current_checkpoint_id IS NOT NULL AND NOT EXISTS (
    SELECT 1
    FROM workspace_checkpoints checkpoint
    WHERE checkpoint.tenant_id = NEW.tenant_id
      AND checkpoint.workspace_id = NEW.id
      AND checkpoint.id = NEW.current_checkpoint_id
      AND checkpoint.status = 'ready'
  ) THEN
    RAISE EXCEPTION 'Workspace current Checkpoint must be ready and belong to the Workspace'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_remote_workspaces_current_checkpoint_ready ON remote_workspaces;
CREATE CONSTRAINT TRIGGER trg_remote_workspaces_current_checkpoint_ready
AFTER INSERT OR UPDATE OF tenant_id, id, current_checkpoint_id ON remote_workspaces
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_workspace_current_checkpoint_ready();

CREATE OR REPLACE FUNCTION assert_execution_restore_checkpoint_ready()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.restore_checkpoint_id IS NOT NULL AND NOT EXISTS (
    SELECT 1
    FROM workspace_checkpoints checkpoint
    WHERE checkpoint.tenant_id = NEW.tenant_id
      AND checkpoint.workspace_id = NEW.remote_workspace_id
      AND checkpoint.id = NEW.restore_checkpoint_id
      AND checkpoint.session_id = NEW.session_id
      AND checkpoint.status = 'ready'
  ) THEN
    RAISE EXCEPTION 'Execution restore Checkpoint must be ready and belong to the Execution Workspace'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_agent_executions_restore_checkpoint_ready ON agent_executions;
CREATE CONSTRAINT TRIGGER trg_agent_executions_restore_checkpoint_ready
AFTER INSERT OR UPDATE OF tenant_id, session_id, remote_workspace_id, restore_checkpoint_id ON agent_executions
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_execution_restore_checkpoint_ready();

CREATE OR REPLACE FUNCTION protect_referenced_checkpoint_status()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.status = 'ready' THEN
    RETURN NEW;
  END IF;

  IF EXISTS (
    SELECT 1
    FROM remote_workspaces workspace
    WHERE workspace.tenant_id = OLD.tenant_id
      AND workspace.id = OLD.workspace_id
      AND workspace.current_checkpoint_id = OLD.id
  ) OR EXISTS (
    SELECT 1
    FROM agent_executions execution
    WHERE execution.tenant_id = OLD.tenant_id
      AND execution.remote_workspace_id = OLD.workspace_id
      AND execution.restore_checkpoint_id = OLD.id
  ) THEN
    RAISE EXCEPTION 'Referenced Checkpoint must remain ready'
      USING ERRCODE = '23514';
  END IF;

  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_workspace_checkpoints_protect_reference ON workspace_checkpoints;
CREATE CONSTRAINT TRIGGER trg_workspace_checkpoints_protect_reference
AFTER UPDATE OF status ON workspace_checkpoints
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION protect_referenced_checkpoint_status();

CREATE OR REPLACE FUNCTION protect_referenced_checkpoint_artifact()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF EXISTS (
    SELECT 1
    FROM workspace_checkpoints checkpoint
    WHERE checkpoint.tenant_id = OLD.tenant_id
      AND checkpoint.artifact_id = OLD.id
      AND checkpoint.status = 'ready'
      AND NOT (
        NEW.tenant_id = checkpoint.tenant_id
        AND NEW.session_id = checkpoint.session_id
        AND NEW.execution_id = checkpoint.execution_id
        AND NEW.kind = CASE checkpoint.strategy
          WHEN 'patch' THEN 'checkpoint'
          WHEN 'snapshot' THEN 'workspace_snapshot'
        END
        AND NEW.status = 'ready'
        AND NEW.deleted_at IS NULL
        AND NEW.sha256 = checkpoint.sha256
      )
  ) THEN
    RAISE EXCEPTION 'Artifact is referenced by an active Workspace Checkpoint'
      USING ERRCODE = '23503';
  END IF;

  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_artifacts_protect_checkpoint_reference ON artifacts;
CREATE TRIGGER trg_artifacts_protect_checkpoint_reference
BEFORE UPDATE OF tenant_id, session_id, execution_id, kind, status, sha256, deleted_at ON artifacts
FOR EACH ROW EXECUTE FUNCTION protect_referenced_checkpoint_artifact();

CREATE INDEX IF NOT EXISTS idx_workspace_checkpoints_ready_latest
  ON workspace_checkpoints (tenant_id, workspace_id, ready_at DESC, id DESC)
  WHERE status = 'ready';

CREATE UNIQUE INDEX IF NOT EXISTS uq_workspace_checkpoints_active
  ON workspace_checkpoints (tenant_id, workspace_id)
  WHERE status IN ('pending', 'uploading');

CREATE UNIQUE INDEX IF NOT EXISTS uq_workspace_checkpoints_artifact
  ON workspace_checkpoints (tenant_id, artifact_id)
  WHERE artifact_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_remote_workspaces_current_checkpoint
  ON remote_workspaces (tenant_id, current_checkpoint_id)
  WHERE current_checkpoint_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_agent_executions_restore_checkpoint
  ON agent_executions (tenant_id, restore_checkpoint_id)
  WHERE restore_checkpoint_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_agent_executions_remote_workspace
  ON agent_executions (tenant_id, remote_workspace_id)
  WHERE remote_workspace_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_workspace_checkpoints_execution_generation
  ON workspace_checkpoints (tenant_id, execution_id, generation, id);

CREATE INDEX IF NOT EXISTS idx_workspace_checkpoints_turn
  ON workspace_checkpoints (tenant_id, session_id, turn_id, id);

CREATE INDEX IF NOT EXISTS idx_remote_workspaces_project
  ON remote_workspaces (tenant_id, organization_id, project_id, id);

CREATE INDEX IF NOT EXISTS idx_remote_workspaces_last_execution
  ON remote_workspaces (tenant_id, last_execution_id)
  WHERE last_execution_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_remote_workspaces_last_worker
  ON remote_workspaces (last_worker_id)
  WHERE last_worker_id IS NOT NULL;

COMMENT ON TABLE workspace_checkpoints IS
  'Generation-fenced Git/Patch/Snapshot recovery points; only Patch/Snapshot payloads require a ready Artifact.';
