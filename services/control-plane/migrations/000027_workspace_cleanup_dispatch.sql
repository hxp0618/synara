ALTER TABLE worker_instances
  ADD COLUMN incarnation BIGINT NOT NULL DEFAULT 1,
  ADD COLUMN instance_uid TEXT;

UPDATE worker_instances
SET instance_uid = (
  substr(md5(id::text || ':worker-incarnation:1'), 1, 8) || '-' ||
  substr(md5(id::text || ':worker-incarnation:1'), 9, 4) || '-' ||
  substr(md5(id::text || ':worker-incarnation:1'), 13, 4) || '-' ||
  substr(md5(id::text || ':worker-incarnation:1'), 17, 4) || '-' ||
  substr(md5(id::text || ':worker-incarnation:1'), 21, 12)
)
WHERE instance_uid IS NULL;

-- Worker Protocol v2 makes instanceUid and Workspace layout v3 mandatory. Every
-- pre-migration Worker speaks the legacy v1 contract, so retire all active rows
-- without deleting their leases; existing leases recover through normal expiry.
UPDATE worker_instances
SET status = 'terminated',
    draining_at = NULL,
    terminated_at = COALESCE(terminated_at, now())
WHERE status <> 'terminated';

ALTER TABLE worker_instances
  ALTER COLUMN instance_uid SET NOT NULL,
  ALTER COLUMN protocol_version SET DEFAULT 2,
  ADD CONSTRAINT chk_worker_instances_incarnation
    CHECK (incarnation > 0),
  ADD CONSTRAINT chk_worker_instances_instance_uid
    CHECK (instance_uid ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$');

CREATE UNIQUE INDEX uq_worker_instances_instance_uid
  ON worker_instances (instance_uid);

ALTER TABLE worker_request_receipts
  ADD COLUMN worker_incarnation BIGINT NOT NULL DEFAULT 1,
  ADD CONSTRAINT chk_worker_request_receipts_incarnation
    CHECK (worker_incarnation > 0);

ALTER TABLE tenant_retention_policies
  ADD COLUMN workspace_cleanup_after_days INTEGER,
  ADD CONSTRAINT chk_tenant_retention_workspace_cleanup_after_days
    CHECK (workspace_cleanup_after_days IS NULL OR workspace_cleanup_after_days BETWEEN 1 AND 36500);

CREATE TABLE workspace_materializations (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL,
  workspace_id UUID NOT NULL,
  organization_id UUID NOT NULL,
  project_id UUID NOT NULL,
  session_id UUID NOT NULL,
  execution_target_id UUID NOT NULL REFERENCES execution_targets(id) ON DELETE RESTRICT,
  target_kind TEXT NOT NULL CHECK (target_kind IN ('local', 'ssh', 'docker', 'kubernetes')),
  storage_scope TEXT NOT NULL CHECK (storage_scope IN ('target', 'pod')),
  layout_version INTEGER NOT NULL CHECK (layout_version > 0),
  incarnation_id UUID NOT NULL,
  worker_id UUID REFERENCES worker_instances(id) ON DELETE RESTRICT,
  worker_incarnation BIGINT,
  worker_instance_uid TEXT,
  last_execution_id UUID,
  last_generation BIGINT CHECK (last_generation IS NULL OR last_generation > 0),
  state TEXT NOT NULL DEFAULT 'active'
    CHECK (state IN ('active', 'retired', 'cleanup-pending', 'cleaning', 'cleaned', 'failed')),
  cleanup_reason TEXT,
  cleanup_requested_at TIMESTAMPTZ,
  failure_code TEXT,
  failure_message TEXT,
  failed_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  cleaned_at TIMESTAMPTZ,
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, workspace_id, id),
  UNIQUE (tenant_id, id, incarnation_id),
  UNIQUE (workspace_id, execution_target_id, incarnation_id),
  FOREIGN KEY (tenant_id, workspace_id)
    REFERENCES remote_workspaces(tenant_id, id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, organization_id, project_id)
    REFERENCES projects(tenant_id, organization_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (tenant_id, project_id, session_id)
    REFERENCES agent_sessions(tenant_id, project_id, id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, last_execution_id)
    REFERENCES agent_executions(tenant_id, id) ON DELETE SET NULL (last_execution_id),
  CHECK (
    (worker_id IS NULL AND worker_incarnation IS NULL AND worker_instance_uid IS NULL)
    OR
    (worker_id IS NOT NULL AND worker_incarnation > 0 AND worker_instance_uid IS NOT NULL)
  ),
  CHECK (worker_instance_uid IS NULL OR worker_instance_uid ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'),
  CHECK (cleanup_reason IS NULL OR length(btrim(cleanup_reason)) BETWEEN 1 AND 160),
  CHECK (cleanup_requested_at IS NULL OR cleanup_reason IS NOT NULL),
  CHECK (failure_code IS NULL OR length(btrim(failure_code)) BETWEEN 1 AND 160),
  CHECK (failure_message IS NULL OR length(failure_message) <= 10000),
  CHECK (
    (state = 'cleaned' AND cleaned_at IS NOT NULL)
    OR (state <> 'cleaned' AND cleaned_at IS NULL)
  ),
  CHECK (
    (state = 'failed' AND failure_code IS NOT NULL AND failed_at IS NOT NULL)
    OR (state <> 'failed' AND failure_code IS NULL AND failure_message IS NULL AND failed_at IS NULL)
  )
);

CREATE INDEX idx_workspace_materializations_workspace_state
  ON workspace_materializations (tenant_id, workspace_id, state, updated_at, id);

CREATE INDEX idx_workspace_materializations_target_state
  ON workspace_materializations (execution_target_id, target_kind, storage_scope, state, updated_at, id);

CREATE INDEX idx_workspace_materializations_cleanup_intent
  ON workspace_materializations (cleanup_requested_at, updated_at, id)
  WHERE cleanup_requested_at IS NOT NULL
    AND state IN ('active', 'retired', 'cleanup-pending', 'failed');

CREATE INDEX idx_workspace_materializations_project
  ON workspace_materializations (tenant_id, organization_id, project_id, id);

CREATE INDEX idx_workspace_materializations_session
  ON workspace_materializations (tenant_id, project_id, session_id, id);

CREATE INDEX idx_workspace_materializations_worker
  ON workspace_materializations (worker_id, id)
  WHERE worker_id IS NOT NULL;

CREATE INDEX idx_workspace_materializations_last_execution
  ON workspace_materializations (tenant_id, last_execution_id, id)
  WHERE last_execution_id IS NOT NULL;

WITH materialization_scopes AS (
  SELECT workspace.id AS workspace_id, workspace.tenant_id, workspace.organization_id,
         workspace.project_id, workspace.session_id, workspace.execution_target_id,
         target.kind AS target_kind
  FROM remote_workspaces AS workspace
  JOIN execution_targets AS target ON target.id = workspace.execution_target_id
  UNION
  SELECT workspace.id, workspace.tenant_id, workspace.organization_id,
         workspace.project_id, workspace.session_id, execution.execution_target_id,
         execution.target_kind
  FROM remote_workspaces AS workspace
  JOIN agent_executions AS execution
    ON execution.tenant_id = workspace.tenant_id
   AND execution.remote_workspace_id = workspace.id
), materialization_rows AS (
  SELECT scope.*,
         workspace.state AS workspace_state,
         workspace.cleaned_at AS workspace_cleaned_at,
         latest.id AS last_execution_id,
         latest.generation AS last_generation,
         latest.worker_id,
         worker.incarnation AS worker_incarnation,
         worker.instance_uid AS worker_instance_uid,
         (
           substr(md5(scope.workspace_id::text || ':' || scope.execution_target_id::text || ':materialization'), 1, 8) || '-' ||
           substr(md5(scope.workspace_id::text || ':' || scope.execution_target_id::text || ':materialization'), 9, 4) || '-' ||
           substr(md5(scope.workspace_id::text || ':' || scope.execution_target_id::text || ':materialization'), 13, 4) || '-' ||
           substr(md5(scope.workspace_id::text || ':' || scope.execution_target_id::text || ':materialization'), 17, 4) || '-' ||
           substr(md5(scope.workspace_id::text || ':' || scope.execution_target_id::text || ':materialization'), 21, 12)
         )::uuid AS materialization_id,
         (
           substr(md5(scope.workspace_id::text || ':' || scope.execution_target_id::text || ':incarnation:1'), 1, 8) || '-' ||
           substr(md5(scope.workspace_id::text || ':' || scope.execution_target_id::text || ':incarnation:1'), 9, 4) || '-' ||
           substr(md5(scope.workspace_id::text || ':' || scope.execution_target_id::text || ':incarnation:1'), 13, 4) || '-' ||
           substr(md5(scope.workspace_id::text || ':' || scope.execution_target_id::text || ':incarnation:1'), 17, 4) || '-' ||
           substr(md5(scope.workspace_id::text || ':' || scope.execution_target_id::text || ':incarnation:1'), 21, 12)
         )::uuid AS incarnation_id,
         scope.execution_target_id = workspace.execution_target_id AS is_current_target,
         workspace.created_at,
         workspace.updated_at
  FROM materialization_scopes AS scope
  JOIN remote_workspaces AS workspace
    ON workspace.tenant_id = scope.tenant_id AND workspace.id = scope.workspace_id
  LEFT JOIN LATERAL (
    SELECT execution.id, execution.generation, execution.worker_id
    FROM agent_executions AS execution
    WHERE execution.tenant_id = scope.tenant_id
      AND execution.remote_workspace_id = scope.workspace_id
      AND execution.execution_target_id = scope.execution_target_id
      AND execution.generation > 0
    ORDER BY execution.queued_at DESC, execution.generation DESC, execution.id DESC
    LIMIT 1
  ) AS latest ON TRUE
  LEFT JOIN worker_instances AS worker ON worker.id = latest.worker_id
)
INSERT INTO workspace_materializations (
  id, tenant_id, workspace_id, organization_id, project_id, session_id,
  execution_target_id, target_kind, storage_scope, layout_version, incarnation_id,
  worker_id, worker_incarnation, worker_instance_uid, last_execution_id, last_generation,
  state, cleanup_reason, cleanup_requested_at, created_at, updated_at, cleaned_at
)
SELECT materialization_id, tenant_id, workspace_id, organization_id, project_id, session_id,
       execution_target_id, target_kind,
       CASE WHEN target_kind = 'kubernetes' THEN 'pod' ELSE 'target' END,
       2, incarnation_id,
       CASE WHEN target_kind = 'kubernetes' THEN NULL::uuid ELSE worker_id END,
       CASE WHEN target_kind = 'kubernetes' THEN NULL::bigint ELSE worker_incarnation END,
       CASE WHEN target_kind = 'kubernetes' THEN NULL::text ELSE worker_instance_uid END,
       last_execution_id, last_generation,
       CASE
         WHEN NOT is_current_target THEN 'retired'
         WHEN workspace_state = 'cleaned' THEN 'cleaned'
         WHEN workspace_state = 'cleanup-pending' THEN 'cleanup-pending'
         ELSE 'active'
       END,
       CASE
         WHEN NOT is_current_target THEN 'migration-target-history'
         WHEN workspace_state = 'cleanup-pending' THEN 'migration-logical-cleanup-pending'
       END,
       CASE
         WHEN NOT is_current_target OR workspace_state = 'cleanup-pending' THEN now()
       END,
       created_at, updated_at,
       CASE WHEN is_current_target AND workspace_state = 'cleaned' THEN workspace_cleaned_at END
FROM materialization_rows;

ALTER TABLE remote_workspaces
  ADD COLUMN current_materialization_id UUID;

UPDATE remote_workspaces AS workspace
SET current_materialization_id = materialization.id
FROM workspace_materializations AS materialization
WHERE materialization.tenant_id = workspace.tenant_id
  AND materialization.workspace_id = workspace.id
  AND materialization.execution_target_id = workspace.execution_target_id;

ALTER TABLE remote_workspaces
  ADD CONSTRAINT fk_remote_workspaces_current_materialization
  FOREIGN KEY (tenant_id, id, current_materialization_id)
  REFERENCES workspace_materializations(tenant_id, workspace_id, id)
  ON DELETE RESTRICT
  DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE agent_executions
  ADD COLUMN workspace_materialization_id UUID;

UPDATE agent_executions AS execution
SET workspace_materialization_id = materialization.id
FROM workspace_materializations AS materialization
WHERE execution.remote_workspace_id IS NOT NULL
  AND materialization.tenant_id = execution.tenant_id
  AND materialization.workspace_id = execution.remote_workspace_id
  AND materialization.execution_target_id = execution.execution_target_id;

DO $$
BEGIN
  IF EXISTS (
    SELECT 1
    FROM agent_executions AS execution
    JOIN workspace_materializations AS materialization
      ON materialization.tenant_id = execution.tenant_id
     AND materialization.id = execution.workspace_materialization_id
    WHERE materialization.state = 'cleaned'
      AND execution.status NOT IN ('completed', 'failed', 'cancelled', 'interrupted')
  ) THEN
    RAISE EXCEPTION 'Cleaned Workspace materialization cannot retain a non-terminal Execution'
      USING ERRCODE = '23514';
  END IF;
END;
$$;

ALTER TABLE agent_executions
  ADD CONSTRAINT fk_agent_executions_workspace_materialization
  FOREIGN KEY (tenant_id, remote_workspace_id, workspace_materialization_id)
  REFERENCES workspace_materializations(tenant_id, workspace_id, id)
  ON DELETE RESTRICT
  DEFERRABLE INITIALLY DEFERRED,
  ADD CONSTRAINT chk_agent_executions_workspace_materialization_workspace
  CHECK (workspace_materialization_id IS NULL OR remote_workspace_id IS NOT NULL);

CREATE INDEX idx_agent_executions_workspace_materialization_status
  ON agent_executions (tenant_id, workspace_materialization_id, status, id)
  WHERE workspace_materialization_id IS NOT NULL;

CREATE TABLE workspace_cleanup_commands (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL,
  materialization_id UUID NOT NULL,
  materialization_incarnation_id UUID NOT NULL,
  workspace_id UUID NOT NULL,
  execution_target_id UUID NOT NULL REFERENCES execution_targets(id) ON DELETE RESTRICT,
  target_kind TEXT NOT NULL CHECK (target_kind IN ('local', 'ssh', 'docker', 'kubernetes')),
  storage_scope TEXT NOT NULL CHECK (storage_scope IN ('target', 'pod')),
  layout_version INTEGER NOT NULL CHECK (layout_version > 0),
  reason TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending', 'leased', 'running', 'acknowledged', 'failed', 'superseded')),
  lease_token_hash BYTEA,
  dispatch_generation BIGINT NOT NULL DEFAULT 0 CHECK (dispatch_generation >= 0),
  delivery_worker_id UUID REFERENCES worker_instances(id) ON DELETE RESTRICT,
  delivery_worker_incarnation BIGINT,
  delivery_attempts INTEGER NOT NULL DEFAULT 0 CHECK (delivery_attempts >= 0),
  delivery_available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  lease_expires_at TIMESTAMPTZ,
  requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  leased_at TIMESTAMPTZ,
  started_at TIMESTAMPTZ,
  acknowledged_at TIMESTAMPTZ,
  failed_at TIMESTAMPTZ,
  superseded_at TIMESTAMPTZ,
  last_error_code TEXT,
  last_error_message TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  FOREIGN KEY (tenant_id, materialization_id, materialization_incarnation_id)
    REFERENCES workspace_materializations(tenant_id, id, incarnation_id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, workspace_id, materialization_id)
    REFERENCES workspace_materializations(tenant_id, workspace_id, id) ON DELETE CASCADE,
  CHECK (length(btrim(reason)) BETWEEN 1 AND 160),
  CHECK (delivery_worker_incarnation IS NULL OR delivery_worker_incarnation > 0),
  CHECK (last_error_code IS NULL OR length(btrim(last_error_code)) BETWEEN 1 AND 160),
  CHECK (last_error_message IS NULL OR length(last_error_message) <= 10000),
  CHECK (
    (status IN ('leased', 'running')
      AND lease_token_hash IS NOT NULL
      AND dispatch_generation > 0
      AND delivery_worker_id IS NOT NULL
      AND delivery_worker_incarnation IS NOT NULL
      AND lease_expires_at IS NOT NULL
      AND leased_at IS NOT NULL)
    OR
    (status NOT IN ('leased', 'running')
      AND lease_token_hash IS NULL
      AND delivery_worker_id IS NULL
      AND delivery_worker_incarnation IS NULL
      AND lease_expires_at IS NULL)
  ),
  CHECK ((status = 'running' AND started_at IS NOT NULL) OR (status <> 'running')),
  CHECK ((status = 'acknowledged' AND acknowledged_at IS NOT NULL) OR (status <> 'acknowledged' AND acknowledged_at IS NULL)),
  CHECK ((status = 'failed' AND failed_at IS NOT NULL AND last_error_code IS NOT NULL) OR (status <> 'failed' AND failed_at IS NULL)),
  CHECK ((status = 'superseded' AND superseded_at IS NOT NULL) OR (status <> 'superseded' AND superseded_at IS NULL))
);

CREATE UNIQUE INDEX uq_workspace_cleanup_commands_active_materialization
  ON workspace_cleanup_commands (tenant_id, materialization_id)
  WHERE status IN ('pending', 'leased', 'running');

CREATE UNIQUE INDEX uq_workspace_cleanup_commands_active_lease_token
  ON workspace_cleanup_commands (lease_token_hash)
  WHERE lease_token_hash IS NOT NULL;

CREATE INDEX idx_workspace_cleanup_commands_claim
  ON workspace_cleanup_commands (
    execution_target_id, target_kind, delivery_available_at, requested_at, id
  )
  WHERE status = 'pending';

CREATE INDEX idx_workspace_cleanup_commands_lease_expiry
  ON workspace_cleanup_commands (lease_expires_at, id)
  WHERE status IN ('leased', 'running');

CREATE INDEX idx_workspace_cleanup_commands_materialization
  ON workspace_cleanup_commands (tenant_id, materialization_id, id);

CREATE INDEX idx_workspace_cleanup_commands_execution_target
  ON workspace_cleanup_commands (execution_target_id, id);

CREATE INDEX idx_workspace_cleanup_commands_delivery_worker
  ON workspace_cleanup_commands (delivery_worker_id, id)
  WHERE delivery_worker_id IS NOT NULL;

CREATE OR REPLACE FUNCTION assert_workspace_materialization_scope()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM remote_workspaces AS workspace
    JOIN execution_targets AS target ON target.id = NEW.execution_target_id
    WHERE workspace.tenant_id = NEW.tenant_id
      AND workspace.id = NEW.workspace_id
      AND workspace.organization_id = NEW.organization_id
      AND workspace.project_id = NEW.project_id
      AND workspace.session_id = NEW.session_id
      AND target.kind = NEW.target_kind
      AND (target.tenant_id IS NULL OR target.tenant_id = NEW.tenant_id)
      AND (target.organization_id IS NULL OR target.organization_id = NEW.organization_id)
  ) THEN
    RAISE EXCEPTION 'Workspace materialization does not match its logical Workspace and Target scope'
      USING ERRCODE = '23514';
  END IF;

  IF TG_OP = 'INSERT' OR (
    TG_OP = 'UPDATE' AND (
      NEW.worker_id IS DISTINCT FROM OLD.worker_id
      OR NEW.worker_incarnation IS DISTINCT FROM OLD.worker_incarnation
      OR NEW.worker_instance_uid IS DISTINCT FROM OLD.worker_instance_uid
    )
  ) THEN
    IF NEW.worker_id IS NOT NULL AND NOT EXISTS (
      SELECT 1
      FROM worker_instances AS worker
      WHERE worker.id = NEW.worker_id
        AND worker.execution_target_id = NEW.execution_target_id
        AND worker.target_kind = NEW.target_kind
        AND worker.incarnation = NEW.worker_incarnation
        AND worker.instance_uid = NEW.worker_instance_uid
    ) THEN
      RAISE EXCEPTION 'Workspace materialization Worker fencing metadata is invalid'
        USING ERRCODE = '23514';
    END IF;
  END IF;

  IF TG_OP = 'INSERT' OR (
    TG_OP = 'UPDATE' AND (
      NEW.last_execution_id IS DISTINCT FROM OLD.last_execution_id
      OR NEW.last_generation IS DISTINCT FROM OLD.last_generation
    )
  ) THEN
    IF NEW.last_execution_id IS NOT NULL AND NOT EXISTS (
      SELECT 1
      FROM agent_executions AS execution
      WHERE execution.tenant_id = NEW.tenant_id
        AND execution.id = NEW.last_execution_id
        AND execution.remote_workspace_id = NEW.workspace_id
        AND execution.execution_target_id = NEW.execution_target_id
        AND execution.target_kind = NEW.target_kind
        AND (NEW.last_generation IS NULL OR execution.generation = NEW.last_generation)
    ) THEN
      RAISE EXCEPTION 'Workspace materialization last Execution metadata is invalid'
        USING ERRCODE = '23514';
    END IF;
  END IF;

  IF NEW.state = 'cleaned' AND EXISTS (
    SELECT 1
    FROM agent_executions AS execution
    WHERE execution.tenant_id = NEW.tenant_id
      AND execution.workspace_materialization_id = NEW.id
      AND execution.status NOT IN ('completed', 'failed', 'cancelled', 'interrupted')
  ) THEN
    RAISE EXCEPTION 'Cleaned Workspace materialization cannot retain a non-terminal Execution'
      USING ERRCODE = '23514';
  END IF;

  RETURN NEW;
END;
$$;

CREATE CONSTRAINT TRIGGER trg_workspace_materializations_scope
AFTER INSERT OR UPDATE OF tenant_id, workspace_id, organization_id, project_id, session_id,
  execution_target_id, target_kind, worker_id, worker_incarnation, worker_instance_uid,
  last_execution_id, last_generation, state
ON workspace_materializations
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_workspace_materialization_scope();

CREATE OR REPLACE FUNCTION protect_workspace_materialization_identity()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.id <> OLD.id
    OR NEW.tenant_id <> OLD.tenant_id
    OR NEW.workspace_id <> OLD.workspace_id
    OR NEW.organization_id <> OLD.organization_id
    OR NEW.project_id <> OLD.project_id
    OR NEW.session_id <> OLD.session_id
    OR NEW.execution_target_id <> OLD.execution_target_id
    OR NEW.target_kind <> OLD.target_kind
    OR NEW.storage_scope <> OLD.storage_scope
    OR NEW.layout_version <> OLD.layout_version
    OR NEW.incarnation_id <> OLD.incarnation_id
    OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'Workspace materialization identity fields are immutable'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_workspace_materializations_protect_identity
BEFORE UPDATE ON workspace_materializations
FOR EACH ROW EXECUTE FUNCTION protect_workspace_materialization_identity();

CREATE OR REPLACE FUNCTION assert_current_workspace_materialization()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.current_materialization_id IS NULL THEN
    RETURN NEW;
  END IF;
  IF NOT EXISTS (
    SELECT 1
    FROM workspace_materializations AS materialization
    WHERE materialization.tenant_id = NEW.tenant_id
      AND materialization.workspace_id = NEW.id
      AND materialization.id = NEW.current_materialization_id
      AND materialization.execution_target_id = NEW.execution_target_id
  ) THEN
    RAISE EXCEPTION 'current Workspace materialization does not match the logical Workspace Target'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE CONSTRAINT TRIGGER trg_remote_workspaces_current_materialization_scope
AFTER INSERT OR UPDATE OF tenant_id, id, execution_target_id, current_materialization_id
ON remote_workspaces
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_current_workspace_materialization();

CREATE OR REPLACE FUNCTION assert_execution_materialization_scope()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.workspace_materialization_id IS NULL THEN
    RETURN NEW;
  END IF;
  IF NOT EXISTS (
    SELECT 1
    FROM workspace_materializations AS materialization
    WHERE materialization.tenant_id = NEW.tenant_id
      AND materialization.id = NEW.workspace_materialization_id
      AND materialization.workspace_id = NEW.remote_workspace_id
      AND materialization.session_id = NEW.session_id
      AND materialization.execution_target_id = NEW.execution_target_id
      AND materialization.target_kind = NEW.target_kind
      AND (
        materialization.state <> 'cleaned'
        OR NEW.status IN ('completed', 'failed', 'cancelled', 'interrupted')
      )
  ) THEN
    RAISE EXCEPTION 'Execution Workspace materialization scope is invalid'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE CONSTRAINT TRIGGER trg_agent_executions_materialization_scope
AFTER INSERT OR UPDATE OF tenant_id, session_id, status, execution_target_id, target_kind,
  remote_workspace_id, workspace_materialization_id
ON agent_executions
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_execution_materialization_scope();

CREATE OR REPLACE FUNCTION assert_workspace_cleanup_command_scope()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM workspace_materializations AS materialization
    WHERE materialization.tenant_id = NEW.tenant_id
      AND materialization.id = NEW.materialization_id
      AND materialization.incarnation_id = NEW.materialization_incarnation_id
      AND materialization.workspace_id = NEW.workspace_id
      AND materialization.execution_target_id = NEW.execution_target_id
      AND materialization.target_kind = NEW.target_kind
      AND materialization.storage_scope = NEW.storage_scope
      AND materialization.layout_version = NEW.layout_version
  ) THEN
    RAISE EXCEPTION 'Workspace Cleanup Command scope does not match the physical materialization'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE CONSTRAINT TRIGGER trg_workspace_cleanup_commands_scope
AFTER INSERT OR UPDATE OF tenant_id, materialization_id, materialization_incarnation_id,
  workspace_id, execution_target_id, target_kind, storage_scope, layout_version
ON workspace_cleanup_commands
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_workspace_cleanup_command_scope();

CREATE OR REPLACE FUNCTION protect_workspace_cleanup_command_identity()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.id <> OLD.id
    OR NEW.tenant_id <> OLD.tenant_id
    OR NEW.materialization_id <> OLD.materialization_id
    OR NEW.materialization_incarnation_id <> OLD.materialization_incarnation_id
    OR NEW.workspace_id <> OLD.workspace_id
    OR NEW.execution_target_id <> OLD.execution_target_id
    OR NEW.target_kind <> OLD.target_kind
    OR NEW.storage_scope <> OLD.storage_scope
    OR NEW.layout_version <> OLD.layout_version
    OR NEW.reason <> OLD.reason
    OR NEW.requested_at <> OLD.requested_at
    OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'Workspace Cleanup Command identity fields are immutable'
      USING ERRCODE = '23514';
  END IF;

  IF NOT (
    (OLD.status = 'pending' AND NEW.status IN ('pending', 'leased', 'acknowledged', 'superseded')) OR
    (OLD.status = 'leased' AND NEW.status IN ('leased', 'running', 'pending', 'acknowledged', 'failed', 'superseded')) OR
    (OLD.status = 'running' AND NEW.status IN ('running', 'pending', 'acknowledged', 'failed', 'superseded')) OR
    (OLD.status = 'acknowledged' AND NEW.status = 'acknowledged') OR
    (OLD.status = 'failed' AND NEW.status IN ('failed', 'pending', 'superseded')) OR
    (OLD.status = 'superseded' AND NEW.status = 'superseded')
  ) THEN
    RAISE EXCEPTION 'Workspace Cleanup Command status transition is invalid'
      USING ERRCODE = '23514';
  END IF;

  IF OLD.status IN ('acknowledged', 'superseded') AND NEW IS DISTINCT FROM OLD THEN
    RAISE EXCEPTION 'Terminal Workspace Cleanup Command is immutable'
      USING ERRCODE = '23514';
  END IF;

  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_workspace_cleanup_commands_protect_identity
BEFORE UPDATE ON workspace_cleanup_commands
FOR EACH ROW EXECUTE FUNCTION protect_workspace_cleanup_command_identity();

DROP INDEX IF EXISTS idx_remote_workspaces_state_retention;

CREATE INDEX idx_remote_workspaces_cleanup_due
  ON remote_workspaces (retention_until, last_used_at, id)
  WHERE retention_until IS NOT NULL
    AND state IN ('ready', 'dirty', 'failed', 'cleanup-pending');

COMMENT ON TABLE workspace_materializations IS
  'A fenced physical incarnation of a logical Workspace. Host filesystem paths are intentionally not persisted.';
COMMENT ON TABLE workspace_cleanup_commands IS
  'Short-lease durable queue for root-relative physical Workspace quarantine and deletion outside database transactions.';
COMMENT ON COLUMN workspace_materializations.layout_version IS
  'Agentd-controlled root-relative layout contract version; migrated physical workspaces use v2 and newly allocated materializations use v3.';
COMMENT ON COLUMN workspace_materializations.incarnation_id IS
  'Immutable physical incarnation fence; a logical Workspace rematerialization creates a new row and UUID.';
COMMENT ON COLUMN worker_instances.instance_uid IS
  'Unique Worker process identity rotated together with incarnation on every registration refresh.';
