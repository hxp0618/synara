ALTER TABLE artifacts
  ADD COLUMN workspace_checkpoint_id UUID;

UPDATE artifacts AS artifact
SET workspace_checkpoint_id = checkpoint.id
FROM workspace_checkpoints AS checkpoint
WHERE artifact.workspace_checkpoint_id IS NULL
  AND checkpoint.tenant_id = artifact.tenant_id
  AND checkpoint.artifact_id = artifact.id;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM pg_constraint
    WHERE conname = 'fk_artifacts_workspace_checkpoint'
      AND conrelid = 'artifacts'::regclass
  ) THEN
    ALTER TABLE artifacts
      ADD CONSTRAINT fk_artifacts_workspace_checkpoint
      FOREIGN KEY (workspace_checkpoint_id)
      REFERENCES workspace_checkpoints(id) ON DELETE RESTRICT
      DEFERRABLE INITIALLY DEFERRED;
  END IF;
END $$;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM pg_constraint
    WHERE conname = 'chk_artifacts_workspace_checkpoint_kind'
      AND conrelid = 'artifacts'::regclass
  ) THEN
    ALTER TABLE artifacts
      ADD CONSTRAINT chk_artifacts_workspace_checkpoint_kind
      CHECK (
        workspace_checkpoint_id IS NULL
        OR kind IN ('checkpoint', 'workspace_snapshot')
      );
  END IF;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS uq_artifacts_workspace_checkpoint
  ON artifacts (workspace_checkpoint_id)
  WHERE workspace_checkpoint_id IS NOT NULL;

CREATE OR REPLACE FUNCTION checkpoint_artifact_binding_is_valid(checkpoint_row workspace_checkpoints)
RETURNS BOOLEAN
LANGUAGE plpgsql
STABLE
AS $$
DECLARE
  binding_valid BOOLEAN;
BEGIN
  IF checkpoint_row.strategy = 'git-reference'
    OR checkpoint_row.status NOT IN ('pending', 'uploading', 'ready') THEN
    RETURN TRUE;
  END IF;

  IF checkpoint_row.artifact_id IS NULL THEN
    RETURN checkpoint_row.status = 'pending';
  END IF;

  SELECT EXISTS (
    SELECT 1
    FROM artifacts artifact
    WHERE artifact.tenant_id = checkpoint_row.tenant_id
      AND artifact.id = checkpoint_row.artifact_id
      AND artifact.workspace_checkpoint_id = checkpoint_row.id
      AND artifact.session_id = checkpoint_row.session_id
      AND artifact.execution_id = checkpoint_row.execution_id
      AND artifact.kind = CASE checkpoint_row.strategy
        WHEN 'patch' THEN 'checkpoint'
        WHEN 'snapshot' THEN 'workspace_snapshot'
      END
      AND artifact.deleted_at IS NULL
      AND (
        (
          checkpoint_row.status IN ('pending', 'uploading')
          AND artifact.status IN ('pending', 'ready')
        )
        OR (
          checkpoint_row.status = 'ready'
          AND artifact.status = 'ready'
          AND artifact.sha256 = checkpoint_row.sha256
        )
      )
  ) INTO binding_valid;

  RETURN binding_valid;
END;
$$;

DO $$
BEGIN
  IF EXISTS (
    SELECT 1
    FROM workspace_checkpoints checkpoint
    WHERE checkpoint.status = 'ready'
      AND checkpoint.strategy IN ('patch', 'snapshot')
      AND NOT checkpoint_artifact_binding_is_valid(checkpoint)
  ) THEN
    RAISE EXCEPTION 'existing ready Workspace Checkpoint Artifact bindings are invalid'
      USING ERRCODE = '23514';
  END IF;
END $$;

CREATE OR REPLACE FUNCTION assert_artifact_checkpoint_scope()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.workspace_checkpoint_id IS NULL THEN
    RETURN NEW;
  END IF;

  IF NOT EXISTS (
    SELECT 1
    FROM workspace_checkpoints checkpoint
    WHERE checkpoint.tenant_id = NEW.tenant_id
      AND checkpoint.id = NEW.workspace_checkpoint_id
      AND checkpoint.session_id = NEW.session_id
      AND checkpoint.execution_id = NEW.execution_id
      AND NEW.kind = CASE checkpoint.strategy
        WHEN 'patch' THEN 'checkpoint'
        WHEN 'snapshot' THEN 'workspace_snapshot'
      END
      AND checkpoint.artifact_id = NEW.id
  ) THEN
    RAISE EXCEPTION 'Artifact does not match the bound Workspace Checkpoint scope'
      USING ERRCODE = '23514';
  END IF;

  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_artifacts_checkpoint_scope ON artifacts;
CREATE CONSTRAINT TRIGGER trg_artifacts_checkpoint_scope
AFTER INSERT OR UPDATE OF id, tenant_id, session_id, execution_id, kind, workspace_checkpoint_id
ON artifacts
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_artifact_checkpoint_scope();

CREATE OR REPLACE FUNCTION assert_ready_checkpoint_artifact()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NOT checkpoint_artifact_binding_is_valid(NEW) THEN
    RAISE EXCEPTION 'active Checkpoint requires a reverse-bound matching Artifact'
      USING ERRCODE = '23514';
  END IF;

  RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION protect_referenced_checkpoint_artifact()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF EXISTS (
    SELECT 1
    FROM workspace_checkpoints checkpoint
    WHERE checkpoint.status IN ('pending', 'uploading', 'ready')
      AND checkpoint.artifact_id IN (OLD.id, NEW.id)
      AND NOT checkpoint_artifact_binding_is_valid(checkpoint)
  ) THEN
    RAISE EXCEPTION 'Artifact is referenced by an active Workspace Checkpoint'
      USING ERRCODE = '23503';
  END IF;

  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_artifacts_protect_checkpoint_reference ON artifacts;
CREATE CONSTRAINT TRIGGER trg_artifacts_protect_checkpoint_reference
AFTER UPDATE ON artifacts
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION protect_referenced_checkpoint_artifact();

COMMENT ON COLUMN artifacts.workspace_checkpoint_id IS
  'Reverse binding from a Checkpoint payload upload to its logical Workspace Checkpoint; never accepted as an arbitrary client object key.';
