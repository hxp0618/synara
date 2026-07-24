CREATE TABLE worker_release_auto_rollback_windows (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL,
  execution_target_id UUID NOT NULL,
  policy_version BIGINT NOT NULL CHECK (policy_version > 0),
  candidate_revision_id UUID NOT NULL,
  candidate_channel TEXT NOT NULL CHECK (candidate_channel IN ('promoted', 'canary')),
  fallback_revision_id UUID NOT NULL,
  started_at TIMESTAMPTZ NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  minimum_executions INTEGER NOT NULL CHECK (minimum_executions BETWEEN 1 AND 10000),
  failure_threshold INTEGER NOT NULL CHECK (failure_threshold BETWEEN 1 AND minimum_executions),
  failure_rate_percent INTEGER NOT NULL CHECK (failure_rate_percent BETWEEN 1 AND 100),
  enabled_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  status TEXT NOT NULL CHECK (status IN ('monitoring', 'rollback-pending', 'triggered', 'expired', 'superseded')),
  decision_reason TEXT,
  evidence JSONB NOT NULL DEFAULT '{}'::jsonb,
  decision_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (execution_target_id, policy_version),
  FOREIGN KEY (tenant_id, execution_target_id)
    REFERENCES execution_targets(tenant_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (execution_target_id, policy_version)
    REFERENCES worker_release_transitions(execution_target_id, policy_version) ON DELETE RESTRICT,
  FOREIGN KEY (execution_target_id, candidate_revision_id)
    REFERENCES worker_release_revisions(execution_target_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (execution_target_id, fallback_revision_id)
    REFERENCES worker_release_revisions(execution_target_id, id) ON DELETE RESTRICT,
  CHECK (candidate_revision_id <> fallback_revision_id),
  CHECK (expires_at > started_at),
  CHECK (jsonb_typeof(evidence) = 'object'),
  CHECK (decision_reason IS NULL OR length(btrim(decision_reason)) BETWEEN 1 AND 2000),
  CHECK (
    (status IN ('monitoring', 'expired') AND decision_reason IS NULL AND decision_at IS NULL)
    OR
    (status IN ('rollback-pending', 'triggered') AND decision_reason IS NOT NULL AND decision_at IS NOT NULL)
    OR
    status = 'superseded'
  )
);

CREATE INDEX idx_worker_release_auto_rollback_pending
  ON worker_release_auto_rollback_windows (status, expires_at, execution_target_id, policy_version)
  WHERE status IN ('monitoring', 'rollback-pending');

CREATE OR REPLACE FUNCTION enforce_worker_release_auto_rollback_window_insert()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM worker_release_transitions AS transition
    WHERE transition.execution_target_id = NEW.execution_target_id
      AND transition.policy_version = NEW.policy_version
      AND transition.tenant_id = NEW.tenant_id
      AND (
        (
          NEW.candidate_channel = 'canary'
          AND transition.action = 'canary'
          AND transition.to_canary_revision_id = NEW.candidate_revision_id
          AND transition.to_promoted_revision_id = NEW.fallback_revision_id
        )
        OR
        (
          NEW.candidate_channel = 'promoted'
          AND transition.action = 'promote'
          AND transition.to_promoted_revision_id = NEW.candidate_revision_id
          AND transition.from_promoted_revision_id = NEW.fallback_revision_id
        )
      )
  ) THEN
    RAISE EXCEPTION 'Worker release auto-rollback window does not match its immutable transition'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_worker_release_auto_rollback_window_insert
BEFORE INSERT ON worker_release_auto_rollback_windows
FOR EACH ROW EXECUTE FUNCTION enforce_worker_release_auto_rollback_window_insert();

CREATE OR REPLACE FUNCTION enforce_worker_release_auto_rollback_window_update()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.id IS DISTINCT FROM OLD.id
     OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
     OR NEW.execution_target_id IS DISTINCT FROM OLD.execution_target_id
     OR NEW.policy_version IS DISTINCT FROM OLD.policy_version
     OR NEW.candidate_revision_id IS DISTINCT FROM OLD.candidate_revision_id
     OR NEW.candidate_channel IS DISTINCT FROM OLD.candidate_channel
     OR NEW.fallback_revision_id IS DISTINCT FROM OLD.fallback_revision_id
     OR NEW.started_at IS DISTINCT FROM OLD.started_at
     OR NEW.expires_at IS DISTINCT FROM OLD.expires_at
     OR NEW.minimum_executions IS DISTINCT FROM OLD.minimum_executions
     OR NEW.failure_threshold IS DISTINCT FROM OLD.failure_threshold
     OR NEW.failure_rate_percent IS DISTINCT FROM OLD.failure_rate_percent
     OR NEW.enabled_by IS DISTINCT FROM OLD.enabled_by
     OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
    RAISE EXCEPTION 'Worker release auto-rollback window identity and policy are immutable'
      USING ERRCODE = '23514';
  END IF;

  IF NOT (
    (OLD.status = 'monitoring' AND NEW.status IN ('monitoring', 'rollback-pending', 'expired', 'superseded'))
    OR
    (OLD.status = 'rollback-pending' AND NEW.status IN ('rollback-pending', 'triggered', 'superseded'))
  ) THEN
    RAISE EXCEPTION 'Worker release auto-rollback window status is terminal or regressed'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_worker_release_auto_rollback_window_update
BEFORE UPDATE ON worker_release_auto_rollback_windows
FOR EACH ROW EXECUTE FUNCTION enforce_worker_release_auto_rollback_window_update();

CREATE OR REPLACE FUNCTION reject_worker_release_auto_rollback_window_delete()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'Worker release auto-rollback windows are durable release evidence'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER trg_worker_release_auto_rollback_window_delete
BEFORE DELETE ON worker_release_auto_rollback_windows
FOR EACH ROW EXECUTE FUNCTION reject_worker_release_auto_rollback_window_delete();

COMMENT ON TABLE worker_release_auto_rollback_windows IS
  'Durable, policy-version-scoped automatic rollback observation windows and decisions. Only Synara-controlled release failures may trigger rollback.';
