CREATE EXTENSION IF NOT EXISTS pgcrypto;

ALTER TABLE agent_executions
  ADD COLUMN scheduling_decision_id UUID;

CREATE TABLE execution_scheduling_decisions (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL,
  execution_id UUID NOT NULL,
  algorithm_version TEXT NOT NULL
    CHECK (algorithm_version IN ('queue-pressure-v1', 'fixed-target-v1', 'legacy-selected-only')),
  evidence_completeness TEXT NOT NULL
    CHECK (evidence_completeness IN ('complete', 'selected-only', 'legacy-selected-only')),
  provider TEXT NOT NULL CHECK (length(provider) BETWEEN 1 AND 80 AND provider !~ '[[:space:]]'),
  decision_kind TEXT NOT NULL CHECK (decision_kind IN ('target-group', 'fixed-target')),
  candidate_count INTEGER NOT NULL CHECK (candidate_count BETWEEN 1 AND 4096),
  selected_ordinal INTEGER NOT NULL CHECK (selected_ordinal BETWEEN 0 AND 4095),
  selected_execution_target_id UUID NOT NULL REFERENCES execution_targets(id) ON DELETE RESTRICT,
  selected_worker_pool_id UUID REFERENCES worker_pools(id) ON DELETE RESTRICT,
  selected_region TEXT NOT NULL CHECK (length(selected_region) <= 120 AND btrim(selected_region) = selected_region),
  selected_cluster_id TEXT NOT NULL CHECK (length(selected_cluster_id) <= 200 AND btrim(selected_cluster_id) = selected_cluster_id),
  candidate_set_sha256 TEXT NOT NULL CHECK (candidate_set_sha256 ~ '^[0-9a-f]{64}$'),
  decided_at TIMESTAMPTZ NOT NULL,
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, id, execution_id),
  UNIQUE (tenant_id, execution_id),
  FOREIGN KEY (tenant_id, execution_id)
    REFERENCES agent_executions(tenant_id, id) ON DELETE CASCADE
    DEFERRABLE INITIALLY DEFERRED,
  CHECK ((evidence_completeness = 'complete') OR candidate_count = 1),
  CHECK ((algorithm_version = 'legacy-selected-only') = (evidence_completeness = 'legacy-selected-only')),
  CHECK ((decision_kind = 'target-group' AND algorithm_version IN ('queue-pressure-v1', 'legacy-selected-only'))
      OR (decision_kind = 'fixed-target' AND algorithm_version IN ('fixed-target-v1', 'legacy-selected-only')))
);

CREATE TABLE execution_scheduling_candidates (
  tenant_id UUID NOT NULL,
  decision_id UUID NOT NULL,
  ordinal INTEGER NOT NULL CHECK (ordinal BETWEEN 0 AND 4095),
  execution_target_id UUID NOT NULL REFERENCES execution_targets(id) ON DELETE RESTRICT,
  target_kind TEXT NOT NULL CHECK (target_kind IN ('local', 'ssh', 'docker', 'kubernetes')),
  target_group_id UUID,
  target_group_version BIGINT CHECK (target_group_version IS NULL OR target_group_version > 0),
  target_group_member_id UUID,
  target_group_member_version BIGINT CHECK (target_group_member_version IS NULL OR target_group_member_version > 0),
  region TEXT NOT NULL CHECK (length(region) <= 120 AND btrim(region) = region),
  cluster_id TEXT NOT NULL CHECK (length(cluster_id) <= 200 AND btrim(cluster_id) = cluster_id),
  health_version BIGINT CHECK (health_version IS NULL OR health_version > 0),
  health_status TEXT CHECK (health_status IS NULL OR health_status IN ('healthy', 'degraded', 'unreachable', 'unknown')),
  capacity_status TEXT CHECK (capacity_status IS NULL OR capacity_status IN ('available', 'saturated', 'unknown')),
  health_observed_at TIMESTAMPTZ,
  health_expires_at TIMESTAMPTZ,
  dr_readiness_version BIGINT CHECK (dr_readiness_version IS NULL OR dr_readiness_version > 0),
  source_dr_domain TEXT CHECK (source_dr_domain IS NULL OR (length(source_dr_domain) BETWEEN 1 AND 160 AND btrim(source_dr_domain) = source_dr_domain)),
  dr_domain TEXT CHECK (dr_domain IS NULL OR (length(dr_domain) BETWEEN 1 AND 160 AND btrim(dr_domain) = dr_domain)),
  dr_replicated_through_at TIMESTAMPTZ,
  dr_artifacts_ready BOOLEAN,
  dr_checkpoints_ready BOOLEAN,
  dr_memory_ready BOOLEAN,
  dr_observed_at TIMESTAMPTZ,
  dr_expires_at TIMESTAMPTZ,
  available_capacity_units INTEGER CHECK (available_capacity_units IS NULL OR available_capacity_units >= 0),
  allocated_capacity_units INTEGER CHECK (allocated_capacity_units IS NULL OR allocated_capacity_units >= 0),
  queued_execution_units BIGINT CHECK (queued_execution_units IS NULL OR queued_execution_units >= 0),
  effective_load_rank BIGINT CHECK (effective_load_rank IS NULL OR effective_load_rank >= 0),
  priority INTEGER CHECK (priority IS NULL OR priority >= 0),
  weight INTEGER CHECK (weight IS NULL OR weight > 0),
  priority_rank INTEGER CHECK (priority_rank IS NULL OR priority_rank >= 0),
  region_rank INTEGER CHECK (region_rank IS NULL OR region_rank >= 0),
  capacity_rank INTEGER CHECK (capacity_rank IS NULL OR capacity_rank >= 0),
  worker_pool_id UUID REFERENCES worker_pools(id) ON DELETE RESTRICT,
  worker_pool_version BIGINT CHECK (worker_pool_version IS NULL OR worker_pool_version > 0),
  capacity_class TEXT CHECK (capacity_class IS NULL OR (length(capacity_class) BETWEEN 1 AND 80 AND btrim(capacity_class) = capacity_class)),
  placement_policy_version BIGINT CHECK (placement_policy_version IS NULL OR placement_policy_version > 0),
  eligibility TEXT NOT NULL CHECK (eligibility IN ('eligible', 'rejected')),
  rejection_code TEXT CHECK (rejection_code IS NULL OR (length(rejection_code) BETWEEN 1 AND 120 AND btrim(rejection_code) = rejection_code)),
  selected BOOLEAN NOT NULL,
  candidate_sha256 TEXT NOT NULL CHECK (candidate_sha256 ~ '^[0-9a-f]{64}$'),
  PRIMARY KEY (tenant_id, decision_id, ordinal),
  FOREIGN KEY (tenant_id, decision_id)
    REFERENCES execution_scheduling_decisions(tenant_id, id) ON DELETE CASCADE
    DEFERRABLE INITIALLY DEFERRED,
  CHECK ((health_version IS NULL AND health_status IS NULL AND capacity_status IS NULL
       AND health_observed_at IS NULL AND health_expires_at IS NULL AND allocated_capacity_units IS NULL)
      OR (health_version IS NOT NULL AND health_status IS NOT NULL AND capacity_status IS NOT NULL
       AND health_observed_at IS NOT NULL AND health_expires_at > health_observed_at
       AND allocated_capacity_units IS NOT NULL)),
  CHECK (available_capacity_units IS NULL OR health_version IS NOT NULL),
  CHECK ((dr_readiness_version IS NULL AND source_dr_domain IS NULL AND dr_domain IS NULL
       AND dr_replicated_through_at IS NULL AND dr_artifacts_ready IS NULL
       AND dr_checkpoints_ready IS NULL AND dr_memory_ready IS NULL
       AND dr_observed_at IS NULL AND dr_expires_at IS NULL)
      OR (dr_readiness_version IS NOT NULL AND source_dr_domain IS NOT NULL AND dr_domain IS NOT NULL
       AND dr_replicated_through_at IS NOT NULL AND dr_artifacts_ready IS NOT NULL
       AND dr_checkpoints_ready IS NOT NULL AND dr_memory_ready IS NOT NULL
       AND dr_observed_at IS NOT NULL AND dr_expires_at > dr_observed_at
       AND dr_replicated_through_at <= dr_observed_at)),
  CHECK ((queued_execution_units IS NULL) = (effective_load_rank IS NULL)),
  CHECK ((worker_pool_id IS NULL AND worker_pool_version IS NULL AND capacity_class IS NULL AND placement_policy_version IS NULL)
      OR (worker_pool_id IS NOT NULL AND worker_pool_version IS NOT NULL AND capacity_class IS NOT NULL AND placement_policy_version IS NOT NULL)),
  CHECK ((eligibility = 'eligible' AND rejection_code IS NULL)
      OR (eligibility = 'rejected' AND rejection_code IS NOT NULL)),
  CHECK (NOT selected OR eligibility = 'eligible')
);

CREATE UNIQUE INDEX uq_execution_scheduling_candidates_selected
  ON execution_scheduling_candidates (tenant_id, decision_id)
  WHERE selected;

CREATE INDEX idx_execution_scheduling_decisions_decided
  ON execution_scheduling_decisions (tenant_id, decided_at DESC, id);

-- Length-prefixing makes the candidate encoder unambiguous and permits the Go
-- core and migration backfill to generate byte-identical canonical digests.
CREATE OR REPLACE FUNCTION scheduling_evidence_field(field_name TEXT, field_value TEXT)
RETURNS TEXT LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
  SELECT CASE WHEN field_value IS NULL
    THEN field_name || ':-' || E'\n'
    ELSE field_name || ':' || octet_length(convert_to(field_value, 'UTF8'))::text || ':' || field_value || E'\n'
  END
$$;

WITH legacy_candidates AS (
  SELECT execution.*,
    (
      substr(md5(execution.tenant_id::text || ':' || execution.id::text || ':scheduling-decision:legacy-v1'), 1, 8) || '-' ||
      substr(md5(execution.tenant_id::text || ':' || execution.id::text || ':scheduling-decision:legacy-v1'), 9, 4) || '-' ||
      substr(md5(execution.tenant_id::text || ':' || execution.id::text || ':scheduling-decision:legacy-v1'), 13, 4) || '-' ||
      substr(md5(execution.tenant_id::text || ':' || execution.id::text || ':scheduling-decision:legacy-v1'), 17, 4) || '-' ||
      substr(md5(execution.tenant_id::text || ':' || execution.id::text || ':scheduling-decision:legacy-v1'), 21, 12)
    )::uuid AS decision_id,
    encode(digest(
      'synara/scheduling-candidate/v1' || E'\n' ||
      scheduling_evidence_field('ordinal', '0') ||
      scheduling_evidence_field('execution_target_id', execution.execution_target_id::text) ||
      scheduling_evidence_field('target_kind', execution.target_kind) ||
      scheduling_evidence_field('target_group_id', execution.target_group_id::text) ||
      scheduling_evidence_field('target_group_version', execution.target_group_version::text) ||
      scheduling_evidence_field('target_group_member_id', NULL) ||
      scheduling_evidence_field('target_group_member_version', execution.target_group_member_version::text) ||
      scheduling_evidence_field('region', CASE WHEN execution.target_group_id IS NOT NULL THEN COALESCE(execution.selected_region, execution.placement_region) ELSE execution.placement_region END) ||
      scheduling_evidence_field('cluster_id', CASE WHEN execution.target_group_id IS NOT NULL THEN COALESCE(execution.selected_cluster_id, execution.placement_cluster_id) ELSE execution.placement_cluster_id END) ||
      scheduling_evidence_field('health_version', NULL) || scheduling_evidence_field('health_status', NULL) ||
      scheduling_evidence_field('capacity_status', NULL) || scheduling_evidence_field('health_observed_at', NULL) ||
      scheduling_evidence_field('health_expires_at', NULL) || scheduling_evidence_field('dr_readiness_version', NULL) ||
      scheduling_evidence_field('source_dr_domain', NULL) || scheduling_evidence_field('dr_domain', NULL) ||
      scheduling_evidence_field('dr_replicated_through_at', NULL) || scheduling_evidence_field('dr_artifacts_ready', NULL) ||
      scheduling_evidence_field('dr_checkpoints_ready', NULL) || scheduling_evidence_field('dr_memory_ready', NULL) ||
      scheduling_evidence_field('dr_observed_at', NULL) || scheduling_evidence_field('dr_expires_at', NULL) ||
      scheduling_evidence_field('available_capacity_units', NULL) || scheduling_evidence_field('allocated_capacity_units', NULL) ||
      scheduling_evidence_field('queued_execution_units', NULL) || scheduling_evidence_field('effective_load_rank', NULL) ||
      scheduling_evidence_field('priority', NULL) || scheduling_evidence_field('weight', NULL) ||
      scheduling_evidence_field('priority_rank', NULL) || scheduling_evidence_field('region_rank', NULL) ||
      scheduling_evidence_field('capacity_rank', NULL) || scheduling_evidence_field('worker_pool_id', execution.worker_pool_id::text) ||
      scheduling_evidence_field('worker_pool_version', execution.worker_pool_version::text) ||
      scheduling_evidence_field('capacity_class', execution.capacity_class) ||
      scheduling_evidence_field('placement_policy_version', execution.placement_policy_version::text) ||
      scheduling_evidence_field('eligibility', 'eligible') || scheduling_evidence_field('rejection_code', NULL) ||
      scheduling_evidence_field('selected', 'true'),
      'sha256'
    ), 'hex') AS candidate_sha256
  FROM agent_executions AS execution
)
INSERT INTO execution_scheduling_decisions (
  id, tenant_id, execution_id, algorithm_version, evidence_completeness, provider, decision_kind,
  candidate_count, selected_ordinal, selected_execution_target_id, selected_worker_pool_id,
  selected_region, selected_cluster_id, candidate_set_sha256, decided_at
)
SELECT decision_id, tenant_id, id, 'legacy-selected-only', 'legacy-selected-only',
  CASE WHEN provider IS NULL OR length(provider) NOT BETWEEN 1 AND 80 OR provider ~ '[[:space:]]' THEN 'unknown' ELSE provider END,
  CASE WHEN target_group_id IS NULL THEN 'fixed-target' ELSE 'target-group' END,
  1, 0, execution_target_id, worker_pool_id,
  CASE WHEN target_group_id IS NOT NULL THEN COALESCE(selected_region, placement_region) ELSE placement_region END,
  CASE WHEN target_group_id IS NOT NULL THEN COALESCE(selected_cluster_id, placement_cluster_id) ELSE placement_cluster_id END,
  encode(digest(candidate_sha256 || E'\n', 'sha256'), 'hex'), queued_at
FROM legacy_candidates;

WITH legacy_candidates AS (
  SELECT execution.*,
    (
      substr(md5(execution.tenant_id::text || ':' || execution.id::text || ':scheduling-decision:legacy-v1'), 1, 8) || '-' ||
      substr(md5(execution.tenant_id::text || ':' || execution.id::text || ':scheduling-decision:legacy-v1'), 9, 4) || '-' ||
      substr(md5(execution.tenant_id::text || ':' || execution.id::text || ':scheduling-decision:legacy-v1'), 13, 4) || '-' ||
      substr(md5(execution.tenant_id::text || ':' || execution.id::text || ':scheduling-decision:legacy-v1'), 17, 4) || '-' ||
      substr(md5(execution.tenant_id::text || ':' || execution.id::text || ':scheduling-decision:legacy-v1'), 21, 12)
    )::uuid AS decision_id,
    encode(digest(
      'synara/scheduling-candidate/v1' || E'\n' ||
      scheduling_evidence_field('ordinal', '0') ||
      scheduling_evidence_field('execution_target_id', execution.execution_target_id::text) ||
      scheduling_evidence_field('target_kind', execution.target_kind) ||
      scheduling_evidence_field('target_group_id', execution.target_group_id::text) ||
      scheduling_evidence_field('target_group_version', execution.target_group_version::text) ||
      scheduling_evidence_field('target_group_member_id', NULL) ||
      scheduling_evidence_field('target_group_member_version', execution.target_group_member_version::text) ||
      scheduling_evidence_field('region', CASE WHEN execution.target_group_id IS NOT NULL THEN COALESCE(execution.selected_region, execution.placement_region) ELSE execution.placement_region END) ||
      scheduling_evidence_field('cluster_id', CASE WHEN execution.target_group_id IS NOT NULL THEN COALESCE(execution.selected_cluster_id, execution.placement_cluster_id) ELSE execution.placement_cluster_id END) ||
      scheduling_evidence_field('health_version', NULL) || scheduling_evidence_field('health_status', NULL) ||
      scheduling_evidence_field('capacity_status', NULL) || scheduling_evidence_field('health_observed_at', NULL) ||
      scheduling_evidence_field('health_expires_at', NULL) || scheduling_evidence_field('dr_readiness_version', NULL) ||
      scheduling_evidence_field('source_dr_domain', NULL) || scheduling_evidence_field('dr_domain', NULL) ||
      scheduling_evidence_field('dr_replicated_through_at', NULL) || scheduling_evidence_field('dr_artifacts_ready', NULL) ||
      scheduling_evidence_field('dr_checkpoints_ready', NULL) || scheduling_evidence_field('dr_memory_ready', NULL) ||
      scheduling_evidence_field('dr_observed_at', NULL) || scheduling_evidence_field('dr_expires_at', NULL) ||
      scheduling_evidence_field('available_capacity_units', NULL) || scheduling_evidence_field('allocated_capacity_units', NULL) ||
      scheduling_evidence_field('queued_execution_units', NULL) || scheduling_evidence_field('effective_load_rank', NULL) ||
      scheduling_evidence_field('priority', NULL) || scheduling_evidence_field('weight', NULL) ||
      scheduling_evidence_field('priority_rank', NULL) || scheduling_evidence_field('region_rank', NULL) ||
      scheduling_evidence_field('capacity_rank', NULL) || scheduling_evidence_field('worker_pool_id', execution.worker_pool_id::text) ||
      scheduling_evidence_field('worker_pool_version', execution.worker_pool_version::text) ||
      scheduling_evidence_field('capacity_class', execution.capacity_class) ||
      scheduling_evidence_field('placement_policy_version', execution.placement_policy_version::text) ||
      scheduling_evidence_field('eligibility', 'eligible') || scheduling_evidence_field('rejection_code', NULL) ||
      scheduling_evidence_field('selected', 'true'),
      'sha256'
    ), 'hex') AS candidate_sha256
  FROM agent_executions AS execution
)
INSERT INTO execution_scheduling_candidates (
  tenant_id, decision_id, ordinal, execution_target_id, target_kind,
  target_group_id, target_group_version, target_group_member_id, target_group_member_version,
  region, cluster_id, worker_pool_id, worker_pool_version, capacity_class, placement_policy_version,
  eligibility, rejection_code, selected, candidate_sha256
)
SELECT tenant_id, decision_id, 0, execution_target_id, target_kind,
  target_group_id, target_group_version, NULL, target_group_member_version,
  CASE WHEN target_group_id IS NOT NULL THEN COALESCE(selected_region, placement_region) ELSE placement_region END,
  CASE WHEN target_group_id IS NOT NULL THEN COALESCE(selected_cluster_id, placement_cluster_id) ELSE placement_cluster_id END,
  worker_pool_id, worker_pool_version, capacity_class,
  placement_policy_version, 'eligible', NULL, TRUE, candidate_sha256
FROM legacy_candidates;

UPDATE agent_executions AS execution
SET scheduling_decision_id = decision.id
FROM execution_scheduling_decisions AS decision
WHERE decision.tenant_id = execution.tenant_id AND decision.execution_id = execution.id;

ALTER TABLE agent_executions
  ADD CONSTRAINT fk_agent_executions_scheduling_decision
  FOREIGN KEY (tenant_id, scheduling_decision_id, id)
  REFERENCES execution_scheduling_decisions(tenant_id, id, execution_id) ON DELETE NO ACTION
  DEFERRABLE INITIALLY DEFERRED;

CREATE OR REPLACE FUNCTION validate_execution_scheduling_decision_graph()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
  decision execution_scheduling_decisions%ROWTYPE;
  execution agent_executions%ROWTYPE;
  selected_candidate execution_scheduling_candidates%ROWTYPE;
  actual_count INTEGER;
  selected_count INTEGER;
  minimum_ordinal INTEGER;
  maximum_ordinal INTEGER;
  actual_set_sha256 TEXT;
BEGIN
  IF TG_TABLE_NAME = 'execution_scheduling_decisions' THEN
    decision := NEW;
  ELSE
    SELECT * INTO decision FROM execution_scheduling_decisions
    WHERE tenant_id = COALESCE(NEW.tenant_id, OLD.tenant_id)
      AND id = COALESCE(NEW.decision_id, OLD.decision_id);
  END IF;
  IF decision.id IS NULL THEN RETURN NULL; END IF;

  SELECT count(*), count(*) FILTER (WHERE selected), min(ordinal), max(ordinal),
    encode(digest(string_agg(candidate_sha256, E'\n' ORDER BY ordinal) || E'\n', 'sha256'), 'hex')
  INTO actual_count, selected_count, minimum_ordinal, maximum_ordinal, actual_set_sha256
  FROM execution_scheduling_candidates
  WHERE tenant_id = decision.tenant_id AND decision_id = decision.id;
  SELECT * INTO selected_candidate FROM execution_scheduling_candidates
  WHERE tenant_id = decision.tenant_id AND decision_id = decision.id AND selected;
  SELECT * INTO execution FROM agent_executions
  WHERE tenant_id = decision.tenant_id AND id = decision.execution_id;

  IF execution.id IS NULL OR execution.scheduling_decision_id IS DISTINCT FROM decision.id
    OR actual_count <> decision.candidate_count OR selected_count <> 1
    OR minimum_ordinal <> 0 OR maximum_ordinal <> actual_count - 1
    OR actual_set_sha256 IS DISTINCT FROM decision.candidate_set_sha256
    OR selected_candidate.ordinal <> decision.selected_ordinal
    OR selected_candidate.execution_target_id IS DISTINCT FROM execution.execution_target_id
    OR selected_candidate.target_kind IS DISTINCT FROM execution.target_kind
    OR selected_candidate.target_group_id IS DISTINCT FROM execution.target_group_id
    OR selected_candidate.target_group_version IS DISTINCT FROM execution.target_group_version
    OR selected_candidate.target_group_member_version IS DISTINCT FROM execution.target_group_member_version
    OR selected_candidate.worker_pool_id IS DISTINCT FROM execution.worker_pool_id
    OR selected_candidate.worker_pool_version IS DISTINCT FROM execution.worker_pool_version
    OR selected_candidate.capacity_class IS DISTINCT FROM execution.capacity_class
    OR selected_candidate.placement_policy_version IS DISTINCT FROM execution.placement_policy_version
    OR selected_candidate.region IS DISTINCT FROM (CASE WHEN execution.target_group_id IS NOT NULL THEN execution.selected_region ELSE execution.placement_region END)
    OR selected_candidate.cluster_id IS DISTINCT FROM (CASE WHEN execution.target_group_id IS NOT NULL THEN execution.selected_cluster_id ELSE execution.placement_cluster_id END)
    OR decision.selected_execution_target_id IS DISTINCT FROM execution.execution_target_id
    OR decision.selected_worker_pool_id IS DISTINCT FROM execution.worker_pool_id
    OR decision.decision_kind IS DISTINCT FROM (CASE WHEN execution.target_group_id IS NULL THEN 'fixed-target' ELSE 'target-group' END)
    OR decision.selected_region IS DISTINCT FROM (CASE WHEN execution.target_group_id IS NOT NULL THEN execution.selected_region ELSE execution.placement_region END)
    OR decision.selected_cluster_id IS DISTINCT FROM (CASE WHEN execution.target_group_id IS NOT NULL THEN execution.selected_cluster_id ELSE execution.placement_cluster_id END) THEN
    RAISE EXCEPTION 'Execution Scheduling Decision graph is incomplete or mismatched' USING ERRCODE = '23514';
  END IF;
  RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER trg_execution_scheduling_decisions_graph
AFTER INSERT OR UPDATE OR DELETE ON execution_scheduling_decisions
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
EXECUTE FUNCTION validate_execution_scheduling_decision_graph();

CREATE CONSTRAINT TRIGGER trg_execution_scheduling_candidates_graph
AFTER INSERT OR UPDATE OR DELETE ON execution_scheduling_candidates
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
EXECUTE FUNCTION validate_execution_scheduling_decision_graph();

CREATE OR REPLACE FUNCTION reject_execution_scheduling_evidence_mutation()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'Execution Scheduling Decision evidence is immutable' USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER trg_execution_scheduling_decisions_immutable
BEFORE UPDATE ON execution_scheduling_decisions
FOR EACH ROW EXECUTE FUNCTION reject_execution_scheduling_evidence_mutation();

CREATE TRIGGER trg_execution_scheduling_candidates_immutable
BEFORE UPDATE ON execution_scheduling_candidates
FOR EACH ROW EXECUTE FUNCTION reject_execution_scheduling_evidence_mutation();

CREATE OR REPLACE FUNCTION protect_agent_execution_scheduling_decision()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
  IF OLD.scheduling_decision_id IS NOT NULL
    AND NEW.scheduling_decision_id IS DISTINCT FROM OLD.scheduling_decision_id THEN
    RAISE EXCEPTION 'Execution Scheduling Decision identity is immutable' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_agent_executions_scheduling_decision_immutable
BEFORE UPDATE OF scheduling_decision_id ON agent_executions
FOR EACH ROW EXECUTE FUNCTION protect_agent_execution_scheduling_decision();
