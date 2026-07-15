CREATE TABLE worker_release_revisions (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL,
  execution_target_id UUID NOT NULL,
  revision BIGINT NOT NULL CHECK (revision > 0),
  worker_manifest_id UUID NOT NULL REFERENCES worker_manifests(id) ON DELETE RESTRICT,
  description TEXT NOT NULL DEFAULT '',
  created_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  UNIQUE (execution_target_id, id),
  UNIQUE (execution_target_id, revision),
  UNIQUE (execution_target_id, worker_manifest_id),
  FOREIGN KEY (tenant_id, execution_target_id)
    REFERENCES execution_targets(tenant_id, id) ON DELETE RESTRICT,
  CHECK (length(description) <= 2000)
);

CREATE TABLE worker_release_policies (
  tenant_id UUID NOT NULL,
  execution_target_id UUID PRIMARY KEY,
  policy_version BIGINT NOT NULL CHECK (policy_version > 0),
  promoted_revision_id UUID NOT NULL,
  canary_revision_id UUID,
  canary_percent INTEGER NOT NULL DEFAULT 0 CHECK (canary_percent BETWEEN 0 AND 100),
  updated_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, execution_target_id),
  FOREIGN KEY (tenant_id, execution_target_id)
    REFERENCES execution_targets(tenant_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (execution_target_id, promoted_revision_id)
    REFERENCES worker_release_revisions(execution_target_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (execution_target_id, canary_revision_id)
    REFERENCES worker_release_revisions(execution_target_id, id) ON DELETE RESTRICT,
  CHECK (
    (canary_revision_id IS NULL AND canary_percent = 0)
    OR
    (
      canary_revision_id IS NOT NULL
      AND canary_revision_id <> promoted_revision_id
      AND canary_percent BETWEEN 1 AND 100
    )
  )
);

CREATE TABLE worker_release_transitions (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL,
  execution_target_id UUID NOT NULL,
  policy_version BIGINT NOT NULL CHECK (policy_version > 0),
  action TEXT NOT NULL CHECK (action IN ('promote', 'canary', 'rollback')),
  from_promoted_revision_id UUID,
  from_canary_revision_id UUID,
  to_promoted_revision_id UUID NOT NULL,
  to_canary_revision_id UUID,
  canary_percent INTEGER NOT NULL DEFAULT 0 CHECK (canary_percent BETWEEN 0 AND 100),
  reason TEXT NOT NULL,
  actor_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  request_id TEXT,
  occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (execution_target_id, policy_version),
  FOREIGN KEY (tenant_id, execution_target_id)
    REFERENCES execution_targets(tenant_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (execution_target_id, from_promoted_revision_id)
    REFERENCES worker_release_revisions(execution_target_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (execution_target_id, from_canary_revision_id)
    REFERENCES worker_release_revisions(execution_target_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (execution_target_id, to_promoted_revision_id)
    REFERENCES worker_release_revisions(execution_target_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (execution_target_id, to_canary_revision_id)
    REFERENCES worker_release_revisions(execution_target_id, id) ON DELETE RESTRICT,
  CHECK (length(btrim(reason)) BETWEEN 1 AND 2000),
  CHECK (request_id IS NULL OR length(request_id) BETWEEN 1 AND 160),
  CHECK (
    (to_canary_revision_id IS NULL AND canary_percent = 0)
    OR
    (
      to_canary_revision_id IS NOT NULL
      AND to_canary_revision_id <> to_promoted_revision_id
      AND canary_percent BETWEEN 1 AND 100
    )
  )
);

ALTER TABLE worker_instances
  ADD COLUMN worker_release_revision_id UUID,
  ADD COLUMN worker_release_channel TEXT,
  ADD COLUMN worker_release_status TEXT NOT NULL DEFAULT 'unmanaged'
    CHECK (worker_release_status IN ('unmanaged', 'active', 'inactive')),
  ADD COLUMN worker_release_reason TEXT,
  ADD COLUMN worker_release_checked_at TIMESTAMPTZ,
  ADD CONSTRAINT fk_worker_instances_release_revision
    FOREIGN KEY (execution_target_id, worker_release_revision_id)
    REFERENCES worker_release_revisions(execution_target_id, id) ON DELETE RESTRICT,
  ADD CONSTRAINT chk_worker_instances_release_shape
    CHECK (
      (
        worker_release_status = 'unmanaged'
        AND worker_release_revision_id IS NULL
        AND worker_release_channel IS NULL
        AND worker_release_reason IS NULL
        AND worker_release_checked_at IS NULL
      )
      OR
      (
        worker_release_status = 'active'
        AND worker_release_revision_id IS NOT NULL
        AND worker_release_channel IN ('promoted', 'canary')
        AND worker_release_reason IS NULL
        AND worker_release_checked_at IS NOT NULL
      )
      OR
      (
        worker_release_status = 'inactive'
        AND worker_release_channel IS NULL
        AND length(btrim(worker_release_reason)) BETWEEN 1 AND 2000
        AND worker_release_checked_at IS NOT NULL
      )
    );

ALTER TABLE agent_executions
  ADD COLUMN worker_release_revision_id UUID,
  ADD COLUMN worker_release_channel TEXT,
  ADD CONSTRAINT fk_agent_executions_release_revision
    FOREIGN KEY (execution_target_id, worker_release_revision_id)
    REFERENCES worker_release_revisions(execution_target_id, id) ON DELETE RESTRICT,
  ADD CONSTRAINT chk_agent_executions_release_shape
    CHECK (
      (worker_release_revision_id IS NULL AND worker_release_channel IS NULL)
      OR
      (worker_release_revision_id IS NOT NULL AND worker_release_channel IN ('promoted', 'canary'))
    );

CREATE INDEX idx_worker_release_revisions_tenant_target
  ON worker_release_revisions (tenant_id, execution_target_id, revision DESC, id);

CREATE INDEX idx_worker_release_revisions_manifest
  ON worker_release_revisions (worker_manifest_id, execution_target_id, id);

CREATE INDEX idx_worker_release_transitions_tenant_target
  ON worker_release_transitions (tenant_id, execution_target_id, policy_version DESC, id);

CREATE INDEX idx_worker_instances_release_claimability
  ON worker_instances (
    execution_target_id,
    worker_release_status,
    worker_release_revision_id,
    worker_release_channel,
    administrative_status,
    compatibility_status,
    status,
    last_heartbeat_at,
    id
  );

CREATE INDEX idx_agent_executions_release_claimable
  ON agent_executions (
    execution_target_id,
    worker_release_revision_id,
    worker_release_channel,
    queued_at,
    id
  )
  WHERE status IN ('queued', 'recovering');

CREATE OR REPLACE FUNCTION enforce_worker_release_revision_immutable()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'worker release revisions are immutable'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER trg_worker_release_revisions_immutable
BEFORE UPDATE OR DELETE ON worker_release_revisions
FOR EACH ROW EXECUTE FUNCTION enforce_worker_release_revision_immutable();

CREATE OR REPLACE FUNCTION enforce_worker_release_transition_immutable()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'worker release transitions are immutable'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER trg_worker_release_transitions_immutable
BEFORE UPDATE OR DELETE ON worker_release_transitions
FOR EACH ROW EXECUTE FUNCTION enforce_worker_release_transition_immutable();

CREATE OR REPLACE FUNCTION enforce_worker_release_policy_cas()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'worker release policies cannot be deleted'
      USING ERRCODE = '23514';
  END IF;
  IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
     OR NEW.execution_target_id IS DISTINCT FROM OLD.execution_target_id THEN
    RAISE EXCEPTION 'worker release policy ownership is immutable'
      USING ERRCODE = '23514';
  END IF;
  IF NEW.policy_version <> OLD.policy_version + 1 THEN
    RAISE EXCEPTION 'worker release policy version must advance exactly once'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_worker_release_policies_cas
BEFORE UPDATE OR DELETE ON worker_release_policies
FOR EACH ROW EXECUTE FUNCTION enforce_worker_release_policy_cas();

COMMENT ON TABLE worker_release_revisions IS
  'Immutable, target-scoped release revisions that bind an operator release number to one immutable Worker manifest.';
COMMENT ON TABLE worker_release_policies IS
  'Single mutable CAS policy per tenant-owned Execution Target selecting one promoted and optional canary release revision.';
COMMENT ON TABLE worker_release_transitions IS
  'Immutable promotion, canary and rollback history for a Worker release policy version.';
COMMENT ON COLUMN agent_executions.worker_release_revision_id IS
  'Release revision selected when the Execution was queued or safely reassigned before lease acquisition.';
