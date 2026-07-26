CREATE TABLE execution_scheduling_policy_heads (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  scope_kind TEXT NOT NULL CHECK (scope_kind IN ('tenant', 'organization')),
  scope_id UUID NOT NULL,
  organization_id UUID,
  current_revision_id UUID,
  version BIGINT NOT NULL DEFAULT 0 CHECK (version >= 0),
  updated_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, scope_kind, scope_id),
  FOREIGN KEY (tenant_id, organization_id) REFERENCES organizations(tenant_id, id) ON DELETE CASCADE,
  CHECK ((scope_kind = 'tenant' AND scope_id = tenant_id AND organization_id IS NULL)
      OR (scope_kind = 'organization' AND scope_id = organization_id AND organization_id IS NOT NULL)),
  CHECK ((version = 0 AND current_revision_id IS NULL)
      OR (version > 0 AND current_revision_id IS NOT NULL))
);

CREATE TABLE execution_scheduling_policy_revisions (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL,
  policy_head_id UUID NOT NULL,
  revision_number BIGINT NOT NULL CHECK (revision_number > 0),
  sha256 TEXT NOT NULL CHECK (sha256 ~ '^[0-9a-f]{64}$'),
  deny_all BOOLEAN NOT NULL DEFAULT FALSE,
  created_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, policy_head_id, revision_number),
  FOREIGN KEY (tenant_id, policy_head_id)
    REFERENCES execution_scheduling_policy_heads(tenant_id, id) ON DELETE RESTRICT
);

CREATE TABLE execution_scheduling_policy_rules (
  tenant_id UUID NOT NULL,
  revision_id UUID NOT NULL,
  dimension TEXT NOT NULL CHECK (dimension IN ('target', 'region', 'cluster', 'provider', 'capacity_class')),
  mode TEXT NOT NULL CHECK (mode IN ('any', 'allow')),
  PRIMARY KEY (tenant_id, revision_id, dimension),
  FOREIGN KEY (tenant_id, revision_id)
    REFERENCES execution_scheduling_policy_revisions(tenant_id, id) ON DELETE RESTRICT
);

CREATE TABLE execution_scheduling_policy_rule_values (
  tenant_id UUID NOT NULL,
  revision_id UUID NOT NULL,
  dimension TEXT NOT NULL,
  value TEXT NOT NULL CHECK (length(value) BETWEEN 1 AND 200 AND btrim(value) = value),
  PRIMARY KEY (tenant_id, revision_id, dimension, value),
  FOREIGN KEY (tenant_id, revision_id, dimension)
    REFERENCES execution_scheduling_policy_rules(tenant_id, revision_id, dimension) ON DELETE RESTRICT
);

ALTER TABLE execution_scheduling_policy_heads
  ADD CONSTRAINT fk_execution_scheduling_policy_heads_current_revision
  FOREIGN KEY (tenant_id, current_revision_id)
  REFERENCES execution_scheduling_policy_revisions(tenant_id, id) ON DELETE RESTRICT;

CREATE INDEX idx_execution_scheduling_policy_revisions_head
  ON execution_scheduling_policy_revisions (tenant_id, policy_head_id, revision_number DESC);

CREATE OR REPLACE FUNCTION validate_execution_scheduling_policy_head()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
  revision execution_scheduling_policy_revisions%ROWTYPE;
  rule_count INTEGER;
  revision_found BOOLEAN;
BEGIN
  IF NEW.scope_kind = 'organization' AND NOT EXISTS (
    SELECT 1 FROM organizations o WHERE o.tenant_id = NEW.tenant_id AND o.id = NEW.organization_id
  ) THEN
    RAISE EXCEPTION 'Execution Scheduling Policy Organization is outside the Tenant' USING ERRCODE = '23514';
  END IF;
  IF TG_OP = 'UPDATE' THEN
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id OR NEW.scope_kind IS DISTINCT FROM OLD.scope_kind
      OR NEW.scope_id IS DISTINCT FROM OLD.scope_id OR NEW.organization_id IS DISTINCT FROM OLD.organization_id
      OR NEW.id IS DISTINCT FROM OLD.id OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
      RAISE EXCEPTION 'Execution Scheduling Policy Head identity is immutable' USING ERRCODE = '23514';
    END IF;
    IF NEW.current_revision_id IS NOT DISTINCT FROM OLD.current_revision_id
      OR NEW.version <> OLD.version + 1 THEN
      RAISE EXCEPTION 'Execution Scheduling Policy Head must publish exactly one Revision' USING ERRCODE = '23514';
    END IF;
    SELECT * INTO revision FROM execution_scheduling_policy_revisions
      WHERE tenant_id = NEW.tenant_id AND id = NEW.current_revision_id
        AND policy_head_id = NEW.id AND revision_number = NEW.version;
    revision_found := FOUND;
    SELECT count(*) INTO rule_count FROM execution_scheduling_policy_rules
      WHERE tenant_id = NEW.tenant_id AND revision_id = NEW.current_revision_id;
    IF NOT revision_found OR rule_count <> 5 THEN
      RAISE EXCEPTION 'Execution Scheduling Policy Head Revision is incomplete or outside its scope' USING ERRCODE = '23514';
    END IF;
    IF EXISTS (
      SELECT 1 FROM execution_scheduling_policy_rules r
      JOIN execution_scheduling_policy_rule_values v USING (tenant_id, revision_id, dimension)
      WHERE r.tenant_id = NEW.tenant_id AND r.revision_id = NEW.current_revision_id AND r.mode = 'any'
    ) THEN
      RAISE EXCEPTION 'Execution Scheduling Policy any rules cannot contain values' USING ERRCODE = '23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_execution_scheduling_policy_heads_validate
BEFORE INSERT OR UPDATE ON execution_scheduling_policy_heads
FOR EACH ROW EXECUTE FUNCTION validate_execution_scheduling_policy_head();

CREATE OR REPLACE FUNCTION validate_execution_scheduling_policy_revision_build()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE head execution_scheduling_policy_heads%ROWTYPE;
BEGIN
  IF TG_TABLE_NAME = 'execution_scheduling_policy_revisions' THEN
    SELECT * INTO head FROM execution_scheduling_policy_heads
      WHERE tenant_id = NEW.tenant_id AND id = NEW.policy_head_id FOR UPDATE;
    IF NOT FOUND OR NEW.revision_number <> head.version + 1 THEN
      RAISE EXCEPTION 'Execution Scheduling Policy Revision must advance its locked Head by one' USING ERRCODE = '23514';
    END IF;
  ELSE
    SELECT h.* INTO head FROM execution_scheduling_policy_heads h
      JOIN execution_scheduling_policy_revisions r ON r.tenant_id = h.tenant_id AND r.policy_head_id = h.id
      WHERE r.tenant_id = NEW.tenant_id AND r.id = NEW.revision_id AND r.revision_number = h.version + 1 FOR UPDATE OF h;
    IF NOT FOUND THEN
      RAISE EXCEPTION 'Execution Scheduling Policy rules may only build the next unpublished Revision' USING ERRCODE = '23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_execution_scheduling_policy_revisions_build
BEFORE INSERT ON execution_scheduling_policy_revisions
FOR EACH ROW EXECUTE FUNCTION validate_execution_scheduling_policy_revision_build();
CREATE TRIGGER trg_execution_scheduling_policy_rules_build
BEFORE INSERT ON execution_scheduling_policy_rules
FOR EACH ROW EXECUTE FUNCTION validate_execution_scheduling_policy_revision_build();
CREATE TRIGGER trg_execution_scheduling_policy_rule_values_build
BEFORE INSERT ON execution_scheduling_policy_rule_values
FOR EACH ROW EXECUTE FUNCTION validate_execution_scheduling_policy_revision_build();

CREATE OR REPLACE FUNCTION reject_execution_scheduling_policy_immutable_mutation()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'Execution Scheduling Policy Revisions and rules are immutable' USING ERRCODE = '23514';
END;
$$;
CREATE TRIGGER trg_execution_scheduling_policy_revisions_immutable
BEFORE UPDATE OR DELETE ON execution_scheduling_policy_revisions
FOR EACH ROW EXECUTE FUNCTION reject_execution_scheduling_policy_immutable_mutation();
CREATE TRIGGER trg_execution_scheduling_policy_rules_immutable
BEFORE UPDATE OR DELETE ON execution_scheduling_policy_rules
FOR EACH ROW EXECUTE FUNCTION reject_execution_scheduling_policy_immutable_mutation();
CREATE TRIGGER trg_execution_scheduling_policy_rule_values_immutable
BEFORE UPDATE OR DELETE ON execution_scheduling_policy_rule_values
FOR EACH ROW EXECUTE FUNCTION reject_execution_scheduling_policy_immutable_mutation();

ALTER TABLE agent_executions
  ADD COLUMN tenant_scheduling_policy_version BIGINT NOT NULL DEFAULT 0 CHECK (tenant_scheduling_policy_version >= 0),
  ADD COLUMN tenant_scheduling_policy_digest TEXT NOT NULL DEFAULT '48646d468c45b8a2257c2080fce3ef0697f7ec181ca7e085eb49a194a92a90b2'
    CHECK (tenant_scheduling_policy_digest ~ '^[0-9a-f]{64}$'),
  ADD COLUMN organization_scheduling_policy_version BIGINT NOT NULL DEFAULT 0 CHECK (organization_scheduling_policy_version >= 0),
  ADD COLUMN organization_scheduling_policy_digest TEXT NOT NULL DEFAULT '48646d468c45b8a2257c2080fce3ef0697f7ec181ca7e085eb49a194a92a90b2'
    CHECK (organization_scheduling_policy_digest ~ '^[0-9a-f]{64}$'),
  ADD COLUMN placement_region TEXT NOT NULL DEFAULT ''
    CHECK (length(placement_region) <= 120 AND btrim(placement_region) = placement_region),
  ADD COLUMN placement_cluster_id TEXT NOT NULL DEFAULT ''
    CHECK (length(placement_cluster_id) <= 200 AND btrim(placement_cluster_id) = placement_cluster_id);

CREATE OR REPLACE FUNCTION validate_execution_scheduling_policy_snapshot_insert()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
  organization_id_value UUID;
  tenant_head execution_scheduling_policy_heads%ROWTYPE;
  organization_head execution_scheduling_policy_heads%ROWTYPE;
  tenant_revision execution_scheduling_policy_revisions%ROWTYPE;
  organization_revision execution_scheduling_policy_revisions%ROWTYPE;
  selected_pool worker_pools%ROWTYPE;
  pool_found BOOLEAN;
  effective_region TEXT;
  effective_cluster_id TEXT;
BEGIN
  -- This repeats the coordinator lock order so a direct insert cannot use the
  -- v0 defaults while a restricted authority exists or changes concurrently.
  PERFORM 1 FROM tenants WHERE id = NEW.tenant_id FOR UPDATE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'Execution Scheduling Policy snapshot Tenant is unavailable' USING ERRCODE = '23514';
  END IF;
  SELECT organization_id INTO organization_id_value
  FROM agent_sessions WHERE tenant_id = NEW.tenant_id AND id = NEW.session_id FOR SHARE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'Execution Scheduling Policy snapshot Session is outside the Tenant' USING ERRCODE = '23514';
  END IF;
  PERFORM 1 FROM organizations
  WHERE tenant_id = NEW.tenant_id AND id = organization_id_value FOR UPDATE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'Execution Scheduling Policy snapshot Organization is unavailable' USING ERRCODE = '23514';
  END IF;

  SELECT * INTO tenant_head FROM execution_scheduling_policy_heads
  WHERE tenant_id = NEW.tenant_id AND scope_kind = 'tenant' AND scope_id = NEW.tenant_id FOR SHARE;
  IF FOUND THEN
    SELECT * INTO tenant_revision FROM execution_scheduling_policy_revisions
    WHERE tenant_id = NEW.tenant_id AND id = tenant_head.current_revision_id
      AND policy_head_id = tenant_head.id AND revision_number = tenant_head.version FOR SHARE;
    IF NOT FOUND OR NEW.tenant_scheduling_policy_version <> tenant_head.version
      OR NEW.tenant_scheduling_policy_digest IS DISTINCT FROM tenant_revision.sha256 THEN
      RAISE EXCEPTION 'Execution Tenant Scheduling Policy snapshot is stale or corrupt' USING ERRCODE = '23514';
    END IF;
  ELSIF NEW.tenant_scheduling_policy_version <> 0
    OR NEW.tenant_scheduling_policy_digest <> '48646d468c45b8a2257c2080fce3ef0697f7ec181ca7e085eb49a194a92a90b2' THEN
    RAISE EXCEPTION 'Execution Tenant Scheduling Policy v0 snapshot is invalid' USING ERRCODE = '23514';
  END IF;

  SELECT * INTO organization_head FROM execution_scheduling_policy_heads
  WHERE tenant_id = NEW.tenant_id AND scope_kind = 'organization' AND scope_id = organization_id_value FOR SHARE;
  IF FOUND THEN
    SELECT * INTO organization_revision FROM execution_scheduling_policy_revisions
    WHERE tenant_id = NEW.tenant_id AND id = organization_head.current_revision_id
      AND policy_head_id = organization_head.id AND revision_number = organization_head.version FOR SHARE;
    IF NOT FOUND OR NEW.organization_scheduling_policy_version <> organization_head.version
      OR NEW.organization_scheduling_policy_digest IS DISTINCT FROM organization_revision.sha256 THEN
      RAISE EXCEPTION 'Execution Organization Scheduling Policy snapshot is stale or corrupt' USING ERRCODE = '23514';
    END IF;
  ELSIF NEW.organization_scheduling_policy_version <> 0
    OR NEW.organization_scheduling_policy_digest <> '48646d468c45b8a2257c2080fce3ef0697f7ec181ca7e085eb49a194a92a90b2' THEN
    RAISE EXCEPTION 'Execution Organization Scheduling Policy v0 snapshot is invalid' USING ERRCODE = '23514';
  END IF;

  pool_found := FALSE;
  IF NEW.worker_pool_id IS NOT NULL THEN
    SELECT * INTO selected_pool FROM worker_pools
    WHERE id = NEW.worker_pool_id AND execution_target_id = NEW.execution_target_id FOR SHARE;
    pool_found := FOUND;
    IF NOT pool_found THEN
      RAISE EXCEPTION 'Execution placement location Worker Pool is unavailable for the Target' USING ERRCODE = '23514';
    END IF;
  END IF;
  IF NEW.target_group_id IS NOT NULL THEN
    IF NEW.selected_region IS NULL OR NEW.selected_cluster_id IS NULL
      OR (pool_found AND selected_pool.region <> '' AND selected_pool.region <> NEW.selected_region)
      OR (pool_found AND selected_pool.cluster_id <> '' AND selected_pool.cluster_id <> NEW.selected_cluster_id) THEN
      RAISE EXCEPTION 'Execution routed placement location does not match its Member and Worker Pool authority' USING ERRCODE = '23514';
    END IF;
    effective_region := NEW.selected_region;
    effective_cluster_id := NEW.selected_cluster_id;
    IF pool_found AND selected_pool.region <> '' THEN effective_region := selected_pool.region; END IF;
    IF pool_found AND selected_pool.cluster_id <> '' THEN effective_cluster_id := selected_pool.cluster_id; END IF;
  ELSE
    effective_region := '';
    effective_cluster_id := '';
    IF pool_found THEN
      effective_region := selected_pool.region;
      effective_cluster_id := selected_pool.cluster_id;
    END IF;
  END IF;
  IF NEW.placement_region <> effective_region OR NEW.placement_cluster_id <> effective_cluster_id THEN
    RAISE EXCEPTION 'Execution placement location does not match its effective routing and Worker Pool authority' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_agent_executions_scheduling_policy_snapshot_insert
BEFORE INSERT ON agent_executions
FOR EACH ROW EXECUTE FUNCTION validate_execution_scheduling_policy_snapshot_insert();

CREATE OR REPLACE FUNCTION protect_execution_scheduling_policy_snapshot()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.tenant_scheduling_policy_version IS DISTINCT FROM OLD.tenant_scheduling_policy_version
    OR NEW.tenant_scheduling_policy_digest IS DISTINCT FROM OLD.tenant_scheduling_policy_digest
    OR NEW.organization_scheduling_policy_version IS DISTINCT FROM OLD.organization_scheduling_policy_version
    OR NEW.organization_scheduling_policy_digest IS DISTINCT FROM OLD.organization_scheduling_policy_digest
    OR NEW.placement_region IS DISTINCT FROM OLD.placement_region
    OR NEW.placement_cluster_id IS DISTINCT FROM OLD.placement_cluster_id THEN
    RAISE EXCEPTION 'Execution Scheduling Policy snapshot is immutable' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;
CREATE TRIGGER trg_agent_executions_scheduling_policy_snapshot_immutable
BEFORE UPDATE OF tenant_scheduling_policy_version, tenant_scheduling_policy_digest,
  organization_scheduling_policy_version, organization_scheduling_policy_digest,
  placement_region, placement_cluster_id ON agent_executions
FOR EACH ROW EXECUTE FUNCTION protect_execution_scheduling_policy_snapshot();
