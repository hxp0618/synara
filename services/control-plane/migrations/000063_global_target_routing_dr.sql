CREATE TABLE execution_target_groups (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  organization_id UUID REFERENCES organizations(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  strategy TEXT NOT NULL DEFAULT 'priority'
    CHECK (strategy IN ('priority', 'balanced', 'latency')),
  preferred_regions JSONB NOT NULL DEFAULT '[]'::jsonb
    CHECK (jsonb_typeof(preferred_regions) = 'array'),
  allow_cross_region BOOLEAN NOT NULL DEFAULT false,
  max_failover_attempts INTEGER NOT NULL DEFAULT 2
    CHECK (max_failover_attempts BETWEEN 0 AND 20),
  health_max_staleness_seconds INTEGER NOT NULL DEFAULT 90
    CHECK (health_max_staleness_seconds BETWEEN 10 AND 3600),
  status TEXT NOT NULL DEFAULT 'active'
    CHECK (status IN ('active', 'draining', 'disabled')),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, name),
  UNIQUE (tenant_id, id),
  CHECK (length(btrim(name)) BETWEEN 1 AND 160)
);

CREATE TABLE execution_target_group_members (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  target_group_id UUID NOT NULL,
  execution_target_id UUID NOT NULL REFERENCES execution_targets(id) ON DELETE RESTRICT,
  region TEXT NOT NULL,
  cluster_id TEXT NOT NULL,
  priority INTEGER NOT NULL DEFAULT 100 CHECK (priority BETWEEN 0 AND 1000000),
  weight INTEGER NOT NULL DEFAULT 100 CHECK (weight BETWEEN 1 AND 10000),
  status TEXT NOT NULL DEFAULT 'active'
    CHECK (status IN ('active', 'draining', 'disabled')),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (target_group_id, execution_target_id),
  UNIQUE (tenant_id, id),
  FOREIGN KEY (tenant_id, target_group_id)
    REFERENCES execution_target_groups(tenant_id, id) ON DELETE CASCADE,
  CHECK (length(btrim(region)) BETWEEN 1 AND 120),
  CHECK (length(btrim(cluster_id)) BETWEEN 1 AND 200)
);

CREATE INDEX idx_execution_target_group_members_route
  ON execution_target_group_members
  (tenant_id, target_group_id, status, priority, execution_target_id);

CREATE TABLE execution_target_health (
  execution_target_id UUID PRIMARY KEY REFERENCES execution_targets(id) ON DELETE CASCADE,
  status TEXT NOT NULL CHECK (status IN ('healthy', 'degraded', 'unreachable', 'unknown')),
  capacity_status TEXT NOT NULL DEFAULT 'unknown'
    CHECK (capacity_status IN ('available', 'saturated', 'unknown')),
  available_capacity_units INTEGER CHECK (available_capacity_units IS NULL OR available_capacity_units >= 0),
  allocated_capacity_units INTEGER NOT NULL DEFAULT 0 CHECK (allocated_capacity_units >= 0),
  source TEXT NOT NULL,
  reason TEXT,
  observed_at TIMESTAMPTZ NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (expires_at > observed_at),
  CHECK (length(btrim(source)) BETWEEN 1 AND 160),
  CHECK (reason IS NULL OR length(reason) <= 2000)
);

CREATE INDEX idx_execution_target_health_route
  ON execution_target_health (status, capacity_status, expires_at, execution_target_id);

ALTER TABLE agent_sessions
  ADD COLUMN requested_execution_target_id UUID REFERENCES execution_targets(id) ON DELETE RESTRICT,
  ADD COLUMN execution_target_group_id UUID,
  ADD COLUMN routing_policy_version BIGINT,
  ADD COLUMN preferred_execution_region TEXT,
  ADD CONSTRAINT fk_agent_sessions_target_group
    FOREIGN KEY (tenant_id, execution_target_group_id)
    REFERENCES execution_target_groups(tenant_id, id) ON DELETE RESTRICT,
  ADD CONSTRAINT chk_agent_sessions_target_routing_shape CHECK (
    (execution_target_group_id IS NULL AND routing_policy_version IS NULL)
    OR
    (execution_target_group_id IS NOT NULL AND routing_policy_version > 0)
  ),
  ADD CONSTRAINT chk_agent_sessions_preferred_region CHECK (
    preferred_execution_region IS NULL OR length(btrim(preferred_execution_region)) BETWEEN 1 AND 120
  );

UPDATE agent_sessions
SET requested_execution_target_id = execution_target_id
WHERE requested_execution_target_id IS NULL;

ALTER TABLE agent_sessions
  ALTER COLUMN requested_execution_target_id SET NOT NULL;

ALTER TABLE agent_executions
  ADD COLUMN target_group_id UUID,
  ADD COLUMN target_group_version BIGINT,
  ADD COLUMN target_group_member_version BIGINT,
  ADD COLUMN selected_region TEXT,
  ADD COLUMN selected_cluster_id TEXT,
  ADD COLUMN routing_reason TEXT,
  ADD COLUMN predecessor_execution_id UUID,
  ADD CONSTRAINT fk_agent_executions_target_group
    FOREIGN KEY (tenant_id, target_group_id)
    REFERENCES execution_target_groups(tenant_id, id) ON DELETE RESTRICT,
  ADD CONSTRAINT fk_agent_executions_predecessor
    FOREIGN KEY (tenant_id, predecessor_execution_id)
    REFERENCES agent_executions(tenant_id, id) ON DELETE RESTRICT,
  ADD CONSTRAINT chk_agent_executions_target_routing_shape CHECK (
    (target_group_id IS NULL
      AND target_group_version IS NULL
      AND target_group_member_version IS NULL
      AND selected_region IS NULL
      AND selected_cluster_id IS NULL
      AND routing_reason IS NULL)
    OR
    (target_group_id IS NOT NULL
      AND target_group_version > 0
      AND target_group_member_version > 0
      AND length(btrim(selected_region)) BETWEEN 1 AND 120
      AND length(btrim(selected_cluster_id)) BETWEEN 1 AND 200
      AND routing_reason IN ('preferred-target', 'priority', 'balanced', 'latency', 'disaster-recovery'))
  ),
  ADD CONSTRAINT chk_agent_executions_predecessor_attempt CHECK (
    predecessor_execution_id IS NULL OR attempt > 1
  );

CREATE INDEX idx_agent_executions_target_routing
  ON agent_executions (target_group_id, execution_target_id, status, queued_at, id)
  WHERE target_group_id IS NOT NULL
    AND status IN ('queued', 'recovering', 'leased', 'running', 'waiting-for-approval', 'suspended');

CREATE TABLE execution_failover_attempts (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  session_id UUID NOT NULL,
  turn_id UUID NOT NULL,
  target_group_id UUID NOT NULL,
  source_execution_id UUID NOT NULL,
  destination_execution_id UUID,
  source_execution_target_id UUID NOT NULL REFERENCES execution_targets(id) ON DELETE RESTRICT,
  destination_execution_target_id UUID NOT NULL REFERENCES execution_targets(id) ON DELETE RESTRICT,
  source_generation BIGINT NOT NULL CHECK (source_generation >= 0),
  source_recovery_bundle_id UUID,
  leader_fencing_token BIGINT NOT NULL CHECK (leader_fencing_token > 0),
  reason TEXT NOT NULL CHECK (reason IN ('target-unreachable', 'target-draining', 'region-failover', 'operator-request')),
  status TEXT NOT NULL CHECK (status IN ('planned', 'committed', 'failed', 'aborted')),
  requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  completed_at TIMESTAMPTZ,
  failure_code TEXT,
  failure_message TEXT,
  UNIQUE (tenant_id, id),
  FOREIGN KEY (tenant_id, session_id, turn_id)
    REFERENCES agent_turns(tenant_id, session_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (tenant_id, target_group_id)
    REFERENCES execution_target_groups(tenant_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (tenant_id, source_execution_id)
    REFERENCES agent_executions(tenant_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (tenant_id, destination_execution_id)
    REFERENCES agent_executions(tenant_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (tenant_id, source_recovery_bundle_id)
    REFERENCES execution_recovery_bundles(tenant_id, id) ON DELETE RESTRICT,
  CHECK (source_execution_target_id <> destination_execution_target_id),
  CHECK (failure_code IS NULL OR length(failure_code) BETWEEN 1 AND 160),
  CHECK (failure_message IS NULL OR length(failure_message) <= 2000),
  CHECK (
    (status = 'planned' AND destination_execution_id IS NULL AND completed_at IS NULL
      AND failure_code IS NULL AND failure_message IS NULL)
    OR
    (status = 'committed' AND destination_execution_id IS NOT NULL AND completed_at IS NOT NULL
      AND failure_code IS NULL AND failure_message IS NULL)
    OR
    (status IN ('failed', 'aborted') AND completed_at IS NOT NULL AND failure_code IS NOT NULL)
  )
);

CREATE UNIQUE INDEX uq_execution_failover_attempts_live
  ON execution_failover_attempts (tenant_id, source_execution_id)
  WHERE status = 'planned';

CREATE INDEX idx_execution_failover_attempts_session
  ON execution_failover_attempts (tenant_id, session_id, requested_at DESC, id);

ALTER TABLE execution_recovery_bundles
  DROP CONSTRAINT IF EXISTS execution_recovery_bundles_recovery_reason_check,
  ADD CONSTRAINT chk_execution_recovery_bundles_recovery_reason
    CHECK (recovery_reason IN (
      'initial-claim', 'execution-recovery', 'suspend-resume', 'legacy-adoption', 'disaster-recovery'
    ));

ALTER TABLE agent_executions
  DROP CONSTRAINT IF EXISTS chk_agent_executions_next_recovery_reason,
  ADD CONSTRAINT chk_agent_executions_next_recovery_reason CHECK (
    next_recovery_reason IS NULL OR
    (next_recovery_reason = 'suspend-resume' AND status IN ('suspended', 'recovering')) OR
    (next_recovery_reason = 'disaster-recovery' AND status IN ('queued', 'recovering'))
  );

ALTER TABLE execution_interactions
  ADD COLUMN source_execution_id UUID;

UPDATE execution_interactions
SET source_execution_id = execution_id
WHERE source_execution_id IS NULL;

ALTER TABLE execution_interactions
  ALTER COLUMN source_execution_id SET NOT NULL,
  ADD CONSTRAINT fk_execution_interactions_source_execution
    FOREIGN KEY (tenant_id, source_execution_id)
    REFERENCES agent_executions(tenant_id, id) ON DELETE RESTRICT;

CREATE OR REPLACE FUNCTION assert_execution_target_group_member_scope()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  target_group execution_target_groups%ROWTYPE;
  target execution_targets%ROWTYPE;
BEGIN
  SELECT * INTO target_group
  FROM execution_target_groups
  WHERE tenant_id = NEW.tenant_id AND id = NEW.target_group_id;
  SELECT * INTO target
  FROM execution_targets
  WHERE id = NEW.execution_target_id;
  IF NOT FOUND OR target_group.id IS NULL THEN
    RAISE EXCEPTION 'Execution Target Group member scope is missing' USING ERRCODE = '23514';
  END IF;
  IF target.tenant_id IS NOT NULL AND target.tenant_id <> NEW.tenant_id THEN
    RAISE EXCEPTION 'Execution Target Group member crosses tenant scope' USING ERRCODE = '23514';
  END IF;
  IF target_group.organization_id IS NOT NULL
    AND target.organization_id IS NOT NULL
    AND target.organization_id <> target_group.organization_id THEN
    RAISE EXCEPTION 'Execution Target Group member crosses organization scope' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE CONSTRAINT TRIGGER trg_execution_target_group_members_scope
AFTER INSERT OR UPDATE OF tenant_id, target_group_id, execution_target_id
ON execution_target_group_members
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_execution_target_group_member_scope();

CREATE OR REPLACE FUNCTION enforce_execution_target_group_cas()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'Execution Target Groups cannot be deleted' USING ERRCODE = '23514';
  END IF;
  IF NEW.id <> OLD.id OR NEW.tenant_id <> OLD.tenant_id
    OR NEW.organization_id IS DISTINCT FROM OLD.organization_id THEN
    RAISE EXCEPTION 'Execution Target Group ownership is immutable' USING ERRCODE = '23514';
  END IF;
  IF NEW.version <> OLD.version + 1 THEN
    RAISE EXCEPTION 'Execution Target Group version must advance exactly once' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_execution_target_groups_cas
BEFORE UPDATE OR DELETE ON execution_target_groups
FOR EACH ROW EXECUTE FUNCTION enforce_execution_target_group_cas();

CREATE OR REPLACE FUNCTION enforce_execution_target_group_member_cas()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'Execution Target Group members cannot be deleted' USING ERRCODE = '23514';
  END IF;
  IF NEW.id <> OLD.id OR NEW.tenant_id <> OLD.tenant_id
    OR NEW.target_group_id <> OLD.target_group_id
    OR NEW.execution_target_id <> OLD.execution_target_id THEN
    RAISE EXCEPTION 'Execution Target Group member identity is immutable' USING ERRCODE = '23514';
  END IF;
  IF NEW.version <> OLD.version + 1 THEN
    RAISE EXCEPTION 'Execution Target Group member version must advance exactly once' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_execution_target_group_members_cas
BEFORE UPDATE OR DELETE ON execution_target_group_members
FOR EACH ROW EXECUTE FUNCTION enforce_execution_target_group_member_cas();

CREATE OR REPLACE FUNCTION enforce_execution_target_health_monotonic()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'Execution Target health observations cannot be deleted' USING ERRCODE = '23514';
  END IF;
  IF NEW.execution_target_id <> OLD.execution_target_id
    OR NEW.version <> OLD.version + 1
    OR NEW.observed_at <= OLD.observed_at THEN
    RAISE EXCEPTION 'Execution Target health update is stale or not monotonic' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_execution_target_health_monotonic
BEFORE UPDATE OR DELETE ON execution_target_health
FOR EACH ROW EXECUTE FUNCTION enforce_execution_target_health_monotonic();

CREATE OR REPLACE FUNCTION enforce_agent_execution_routing_immutable()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF OLD.target_group_id IS NOT NULL AND (
    NEW.target_group_id IS DISTINCT FROM OLD.target_group_id
    OR NEW.target_group_version IS DISTINCT FROM OLD.target_group_version
    OR NEW.target_group_member_version IS DISTINCT FROM OLD.target_group_member_version
    OR NEW.selected_region IS DISTINCT FROM OLD.selected_region
    OR NEW.selected_cluster_id IS DISTINCT FROM OLD.selected_cluster_id
    OR NEW.routing_reason IS DISTINCT FROM OLD.routing_reason
    OR NEW.predecessor_execution_id IS DISTINCT FROM OLD.predecessor_execution_id
  ) THEN
    RAISE EXCEPTION 'Execution global routing snapshot is immutable' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_agent_executions_routing_immutable
BEFORE UPDATE OF target_group_id, target_group_version, target_group_member_version,
  selected_region, selected_cluster_id, routing_reason, predecessor_execution_id
ON agent_executions
FOR EACH ROW EXECUTE FUNCTION enforce_agent_execution_routing_immutable();

CREATE OR REPLACE FUNCTION assert_agent_session_global_routing_scope()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.execution_target_group_id IS NULL THEN
    RETURN NEW;
  END IF;
  IF NOT EXISTS (
    SELECT 1
    FROM execution_target_groups AS target_group
    JOIN execution_target_group_members AS member
      ON member.tenant_id = target_group.tenant_id
     AND member.target_group_id = target_group.id
     AND member.execution_target_id = NEW.execution_target_id
    WHERE target_group.tenant_id = NEW.tenant_id
      AND target_group.id = NEW.execution_target_group_id
      AND target_group.version = NEW.routing_policy_version
      AND target_group.status = 'active'
      AND member.status = 'active'
  ) THEN
    RAISE EXCEPTION 'Session Target routing snapshot does not match an active Target Group member'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE CONSTRAINT TRIGGER trg_agent_sessions_global_routing_scope
AFTER INSERT OR UPDATE OF tenant_id, execution_target_id, execution_target_group_id, routing_policy_version
ON agent_sessions
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_agent_session_global_routing_scope();

CREATE OR REPLACE FUNCTION assert_agent_execution_global_routing_scope()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  predecessor agent_executions%ROWTYPE;
BEGIN
  IF NEW.target_group_id IS NOT NULL AND NOT EXISTS (
    SELECT 1
    FROM execution_target_groups AS target_group
    JOIN execution_target_group_members AS member
      ON member.tenant_id = target_group.tenant_id
     AND member.target_group_id = target_group.id
     AND member.execution_target_id = NEW.execution_target_id
    WHERE target_group.tenant_id = NEW.tenant_id
      AND target_group.id = NEW.target_group_id
      AND target_group.version = NEW.target_group_version
      AND member.version = NEW.target_group_member_version
      AND member.region = NEW.selected_region
      AND member.cluster_id = NEW.selected_cluster_id
  ) THEN
    RAISE EXCEPTION 'Execution global routing snapshot does not match its Target Group member'
      USING ERRCODE = '23514';
  END IF;
  IF NEW.predecessor_execution_id IS NOT NULL THEN
    SELECT * INTO predecessor
    FROM agent_executions
    WHERE tenant_id = NEW.tenant_id AND id = NEW.predecessor_execution_id;
    IF NOT FOUND
      OR predecessor.session_id <> NEW.session_id
      OR predecessor.turn_id <> NEW.turn_id
      OR predecessor.attempt >= NEW.attempt
      OR predecessor.status NOT IN ('completed', 'failed', 'cancelled', 'interrupted')
      OR EXISTS (
        SELECT 1 FROM worker_leases AS lease
        WHERE lease.tenant_id = NEW.tenant_id AND lease.execution_id = predecessor.id
      ) THEN
      RAISE EXCEPTION 'Execution predecessor must be an earlier fenced terminal attempt of the same Session Turn'
        USING ERRCODE = '23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;

CREATE CONSTRAINT TRIGGER trg_agent_executions_global_routing_scope
AFTER INSERT OR UPDATE OF tenant_id, session_id, turn_id, attempt, execution_target_id,
  target_group_id, target_group_version, target_group_member_version,
  selected_region, selected_cluster_id, predecessor_execution_id
ON agent_executions
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW EXECUTE FUNCTION assert_agent_execution_global_routing_scope();

CREATE OR REPLACE FUNCTION enforce_execution_recovery_bundle_insert()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  current_execution agent_executions%ROWTYPE;
  predecessor execution_recovery_bundles%ROWTYPE;
  predecessor_attempt INTEGER;
BEGIN
  SELECT * INTO current_execution
  FROM agent_executions AS execution
  WHERE execution.tenant_id = NEW.tenant_id
    AND execution.id = NEW.execution_id
    AND execution.session_id = NEW.session_id
    AND execution.turn_id = NEW.turn_id
    AND execution.generation = NEW.generation
    AND execution.status IN ('leased', 'running', 'waiting-for-approval');
  IF NOT FOUND THEN
    RAISE EXCEPTION 'Recovery Bundle does not match the current leased Execution generation'
      USING ERRCODE = '23514';
  END IF;

  IF NEW.previous_bundle_id IS NOT NULL THEN
    SELECT * INTO predecessor
    FROM execution_recovery_bundles AS bundle
    WHERE bundle.tenant_id = NEW.tenant_id
      AND bundle.id = NEW.previous_bundle_id;
    IF NOT FOUND OR predecessor.session_id <> NEW.session_id OR predecessor.turn_id <> NEW.turn_id THEN
      RAISE EXCEPTION 'Recovery Bundle predecessor must belong to the same Session Turn'
        USING ERRCODE = '23514';
    END IF;
    IF predecessor.execution_id = NEW.execution_id THEN
      IF predecessor.generation >= NEW.generation THEN
        RAISE EXCEPTION 'Recovery Bundle predecessor must be from an earlier Generation'
          USING ERRCODE = '23514';
      END IF;
    ELSE
      SELECT attempt INTO predecessor_attempt
      FROM agent_executions
      WHERE tenant_id = NEW.tenant_id AND id = predecessor.execution_id;
      IF current_execution.predecessor_execution_id IS DISTINCT FROM predecessor.execution_id
        OR predecessor_attempt >= current_execution.attempt THEN
        RAISE EXCEPTION 'Cross-Execution Recovery Bundle predecessor must be the declared earlier attempt'
          USING ERRCODE = '23514';
      END IF;
    END IF;
  END IF;

  IF NEW.recovery_reason = 'initial-claim' AND (
    NEW.generation <> 1 OR NEW.previous_bundle_id IS NOT NULL
  ) THEN
    RAISE EXCEPTION 'Initial Recovery Bundle must be Generation 1 without a predecessor'
      USING ERRCODE = '23514';
  END IF;
  IF NEW.recovery_reason IN ('execution-recovery', 'suspend-resume', 'disaster-recovery')
    AND NEW.previous_bundle_id IS NULL THEN
    RAISE EXCEPTION 'Recovered Execution generations require a predecessor Recovery Bundle'
      USING ERRCODE = '23514';
  END IF;
  IF NEW.recovery_reason = 'disaster-recovery'
    AND predecessor.execution_id = NEW.execution_id THEN
    RAISE EXCEPTION 'Disaster Recovery requires a predecessor from another Execution attempt'
      USING ERRCODE = '23514';
  END IF;
  IF NEW.recovery_reason = 'legacy-adoption' AND (
    NEW.generation <= 1 OR NEW.previous_bundle_id IS NOT NULL OR
    EXISTS (
      SELECT 1 FROM execution_recovery_bundles AS existing
      WHERE existing.tenant_id = NEW.tenant_id AND existing.execution_id = NEW.execution_id
    )
  ) THEN
    RAISE EXCEPTION 'Legacy Recovery Bundle adoption is allowed only once for an existing Generation without history'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION enforce_execution_interaction_failover_lineage()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  destination agent_executions%ROWTYPE;
BEGIN
  IF TG_OP = 'UPDATE' AND NEW.source_execution_id IS DISTINCT FROM OLD.source_execution_id THEN
    RAISE EXCEPTION 'Interaction source Execution is immutable' USING ERRCODE = '23514';
  END IF;
  IF TG_OP = 'UPDATE' AND NEW.execution_id IS DISTINCT FROM OLD.execution_id THEN
    IF OLD.delivery_status NOT IN ('not-ready', 'resume-recorded')
      OR NEW.delivery_status <> OLD.delivery_status
      OR NEW.resume_bundle_id IS NOT NULL THEN
      RAISE EXCEPTION 'Only unbound Interaction state can move during failover' USING ERRCODE = '23514';
    END IF;
    SELECT * INTO destination
    FROM agent_executions
    WHERE tenant_id = NEW.tenant_id AND id = NEW.execution_id;
    IF NOT FOUND OR destination.session_id <> NEW.session_id OR destination.turn_id <> NEW.turn_id
      OR destination.predecessor_execution_id IS DISTINCT FROM OLD.execution_id THEN
      RAISE EXCEPTION 'Interaction failover destination is not the declared successor Execution'
        USING ERRCODE = '23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_execution_interactions_failover_lineage
BEFORE UPDATE OF execution_id, source_execution_id ON execution_interactions
FOR EACH ROW EXECUTE FUNCTION enforce_execution_interaction_failover_lineage();

CREATE OR REPLACE FUNCTION enforce_execution_failover_attempt_immutable()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'Execution failover attempts cannot be deleted' USING ERRCODE = '23514';
  END IF;
  IF OLD.status <> 'planned' OR NEW.id <> OLD.id OR NEW.tenant_id <> OLD.tenant_id
    OR NEW.session_id <> OLD.session_id OR NEW.turn_id <> OLD.turn_id
    OR NEW.target_group_id <> OLD.target_group_id
    OR NEW.source_execution_id <> OLD.source_execution_id
    OR NEW.source_execution_target_id <> OLD.source_execution_target_id
    OR NEW.destination_execution_target_id <> OLD.destination_execution_target_id
    OR NEW.source_generation <> OLD.source_generation
    OR NEW.source_recovery_bundle_id IS DISTINCT FROM OLD.source_recovery_bundle_id
    OR NEW.leader_fencing_token <> OLD.leader_fencing_token
    OR NEW.reason <> OLD.reason OR NEW.requested_at <> OLD.requested_at THEN
    RAISE EXCEPTION 'Execution failover attempt lineage is immutable' USING ERRCODE = '23514';
  END IF;
  IF NEW.status NOT IN ('committed', 'failed', 'aborted') THEN
    RAISE EXCEPTION 'Execution failover attempt can only transition once from planned'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_execution_failover_attempts_immutable
BEFORE UPDATE OR DELETE ON execution_failover_attempts
FOR EACH ROW EXECUTE FUNCTION enforce_execution_failover_attempt_immutable();

DROP TRIGGER IF EXISTS trg_execution_target_groups_updated_at ON execution_target_groups;
CREATE TRIGGER trg_execution_target_groups_updated_at
BEFORE UPDATE ON execution_target_groups
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_execution_target_group_members_updated_at ON execution_target_group_members;
CREATE TRIGGER trg_execution_target_group_members_updated_at
BEFORE UPDATE ON execution_target_group_members
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE execution_target_groups IS
  'Tenant-scoped, versioned routing and disaster-recovery policy across Execution Targets, regions, and clusters.';

COMMENT ON TABLE execution_target_health IS
  'Authoritative expiring Control Plane health and capacity observations; browser or transport heartbeats never write this table.';

COMMENT ON TABLE execution_failover_attempts IS
  'Immutable fenced audit of cross-Target successor Execution creation. A committed successor is a new attempt and never rewrites frozen source placement.';
