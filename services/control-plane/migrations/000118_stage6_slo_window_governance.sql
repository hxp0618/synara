CREATE TABLE stage6_slo_windows (
  id UUID PRIMARY KEY,
  operator_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  candidate_record_id UUID NOT NULL REFERENCES stage6_release_candidates(id) ON DELETE RESTRICT,
  window_id UUID NOT NULL UNIQUE,
  receipt BYTEA NOT NULL CHECK (octet_length(receipt) BETWEEN 1 AND 262144),
  receipt_sha256 BYTEA NOT NULL CHECK (octet_length(receipt_sha256) = 32),
  receipt_size_bytes INTEGER NOT NULL CHECK (receipt_size_bytes = octet_length(receipt)),
  receipt_schema TEXT NOT NULL CHECK (receipt_schema = 'synara.slo-window-evidence-receipt.v1'),
  assessment TEXT NOT NULL CHECK (assessment = 'evidence-validated-not-slo-passed'),
  release_commit TEXT NOT NULL CHECK (release_commit ~ '^[0-9a-f]{40}$'),
  environment_class TEXT NOT NULL CHECK (environment_class IN ('production', 'production-like')),
  environment_id TEXT NOT NULL CHECK (length(environment_id) BETWEEN 2 AND 160),
  public_origin TEXT NOT NULL CHECK (public_origin ~ '^https://[A-Za-z0-9.-]+(:[0-9]{1,5})?$'),
  window_started_at TIMESTAMPTZ NOT NULL,
  window_completed_at TIMESTAMPTZ NOT NULL,
  validated_at TIMESTAMPTZ NOT NULL,
  query_revision TEXT NOT NULL CHECK (query_revision ~ '^[0-9a-f]{40}$'),
  all_objectives_assessable BOOLEAN NOT NULL,
  all_objectives_met BOOLEAN NOT NULL,
  eligible_for_human_gate_review BOOLEAN NOT NULL,
  worst_budget_remaining_ratio DOUBLE PRECISION NOT NULL CHECK (worst_budget_remaining_ratio BETWEEN 0 AND 1),
  budget_policy_state TEXT NOT NULL CHECK (budget_policy_state IN (
    'normal-delivery', 'risk-note-required', 'risky-rollout-paused', 'reliability-freeze'
  )),
  state TEXT NOT NULL CHECK (state IN ('recorded', 'approved', 'rejected')),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  created_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  approved_at TIMESTAMPTZ,
  rejected_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (candidate_record_id, window_id)
);

CREATE INDEX idx_stage6_slo_windows_candidate
  ON stage6_slo_windows (candidate_record_id, created_at DESC, id);

CREATE TABLE stage6_slo_objectives (
  id UUID PRIMARY KEY,
  slo_window_record_id UUID NOT NULL REFERENCES stage6_slo_windows(id) ON DELETE RESTRICT,
  operator_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  objective_key TEXT NOT NULL CHECK (objective_key IN (
    'availability', 'apiLatency', 'executionStartDelay', 'eventDelay'
  )),
  target_ratio DOUBLE PRECISION NOT NULL CHECK (target_ratio BETWEEN 0 AND 1),
  good_ratio DOUBLE PRECISION NOT NULL CHECK (good_ratio BETWEEN 0 AND 1),
  sample_count BIGINT NOT NULL CHECK (sample_count >= 0),
  error_budget_remaining_ratio DOUBLE PRECISION NOT NULL CHECK (error_budget_remaining_ratio BETWEEN 0 AND 1),
  policy_state TEXT NOT NULL CHECK (policy_state IN (
    'normal-delivery', 'risk-note-required', 'risky-rollout-paused', 'reliability-freeze'
  )),
  assessable BOOLEAN NOT NULL,
  objective_met BOOLEAN NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (slo_window_record_id, objective_key)
);

CREATE TABLE stage6_slo_approvals (
  id UUID PRIMARY KEY,
  slo_window_record_id UUID NOT NULL REFERENCES stage6_slo_windows(id) ON DELETE RESTRICT,
  operator_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  approval_role TEXT NOT NULL CHECK (approval_role IN ('engineering', 'operations', 'security', 'product')),
  decision TEXT NOT NULL CHECK (decision IN ('approved', 'rejected')),
  approver_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  reason TEXT NOT NULL CHECK (length(trim(reason)) BETWEEN 20 AND 2000),
  evidence_reference TEXT NOT NULL CHECK (length(trim(evidence_reference)) BETWEEN 12 AND 2048),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (slo_window_record_id, approval_role),
  UNIQUE (slo_window_record_id, approver_user_id)
);

CREATE OR REPLACE FUNCTION enforce_stage6_slo_window()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  receipt_json JSONB;
  candidate stage6_release_candidates%ROWTYPE;
  approval_count INTEGER;
BEGIN
  IF TG_OP = 'INSERT' THEN
    receipt_json := convert_from(NEW.receipt, 'UTF8')::jsonb;
    SELECT * INTO candidate FROM stage6_release_candidates WHERE id = NEW.candidate_record_id;
    IF candidate.id IS NULL OR candidate.operator_tenant_id <> NEW.operator_tenant_id
       OR candidate.source_commit <> NEW.release_commit OR candidate.environment_id <> NEW.environment_id
       OR NEW.created_by <> candidate.created_by
       OR NEW.state <> 'recorded' OR NEW.version <> 1
       OR NEW.approved_at IS NOT NULL OR NEW.rejected_at IS NOT NULL
       OR NEW.window_completed_at - NEW.window_started_at < interval '30 days'
       OR NEW.validated_at < NEW.window_completed_at OR NEW.validated_at > now() + interval '5 minutes'
       OR jsonb_typeof(receipt_json) <> 'object'
       OR receipt_json->>'schemaVersion' <> NEW.receipt_schema
       OR receipt_json->>'assessment' <> NEW.assessment
       OR (receipt_json->>'windowId')::uuid <> NEW.window_id
       OR receipt_json->>'releaseCommit' <> NEW.release_commit
       OR receipt_json->>'environmentClass' <> NEW.environment_class
       OR receipt_json->>'environmentId' <> NEW.environment_id
       OR receipt_json->>'publicOrigin' <> NEW.public_origin
       OR (receipt_json->>'windowStartedAt')::timestamptz <> NEW.window_started_at
       OR (receipt_json->>'windowCompletedAt')::timestamptz <> NEW.window_completed_at
       OR (receipt_json->>'validatedAt')::timestamptz <> NEW.validated_at
       OR receipt_json->>'queryRevision' <> NEW.query_revision
       OR (receipt_json->>'allObjectivesAssessable')::boolean IS DISTINCT FROM NEW.all_objectives_assessable
       OR (receipt_json->>'declaredMeasurementsWithinObjectives')::boolean IS DISTINCT FROM NEW.all_objectives_met
       OR (receipt_json->>'eligibleForHumanGateReview')::boolean IS DISTINCT FROM NEW.eligible_for_human_gate_review
       OR jsonb_typeof(receipt_json->'objectives') <> 'object'
       OR (SELECT count(*) FROM jsonb_object_keys(receipt_json->'objectives')) <> 4 THEN
      RAISE EXCEPTION 'Invalid Stage 6 SLO window receipt binding' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
  END IF;

  IF NEW.id IS DISTINCT FROM OLD.id OR NEW.operator_tenant_id IS DISTINCT FROM OLD.operator_tenant_id
     OR NEW.candidate_record_id IS DISTINCT FROM OLD.candidate_record_id OR NEW.window_id IS DISTINCT FROM OLD.window_id
     OR NEW.receipt IS DISTINCT FROM OLD.receipt OR NEW.receipt_sha256 IS DISTINCT FROM OLD.receipt_sha256
     OR NEW.receipt_size_bytes IS DISTINCT FROM OLD.receipt_size_bytes OR NEW.receipt_schema IS DISTINCT FROM OLD.receipt_schema
     OR NEW.assessment IS DISTINCT FROM OLD.assessment OR NEW.release_commit IS DISTINCT FROM OLD.release_commit
     OR NEW.environment_class IS DISTINCT FROM OLD.environment_class OR NEW.environment_id IS DISTINCT FROM OLD.environment_id
     OR NEW.public_origin IS DISTINCT FROM OLD.public_origin OR NEW.window_started_at IS DISTINCT FROM OLD.window_started_at
     OR NEW.window_completed_at IS DISTINCT FROM OLD.window_completed_at OR NEW.validated_at IS DISTINCT FROM OLD.validated_at
     OR NEW.query_revision IS DISTINCT FROM OLD.query_revision
     OR NEW.all_objectives_assessable IS DISTINCT FROM OLD.all_objectives_assessable
     OR NEW.all_objectives_met IS DISTINCT FROM OLD.all_objectives_met
     OR NEW.eligible_for_human_gate_review IS DISTINCT FROM OLD.eligible_for_human_gate_review
     OR NEW.worst_budget_remaining_ratio IS DISTINCT FROM OLD.worst_budget_remaining_ratio
     OR NEW.budget_policy_state IS DISTINCT FROM OLD.budget_policy_state
     OR NEW.created_by IS DISTINCT FROM OLD.created_by OR NEW.created_at IS DISTINCT FROM OLD.created_at
     OR NEW.version <> OLD.version + 1 OR OLD.state <> 'recorded'
     OR NEW.state NOT IN ('approved', 'rejected') THEN
    RAISE EXCEPTION 'Stage 6 SLO window identity and evidence are immutable' USING ERRCODE = '23514';
  END IF;
  IF NEW.state = 'approved' THEN
    SELECT count(*) INTO approval_count FROM stage6_slo_approvals
    WHERE slo_window_record_id = NEW.id AND decision = 'approved';
    IF NOT NEW.eligible_for_human_gate_review OR approval_count <> 4
       OR NEW.approved_at IS NULL OR NEW.rejected_at IS NOT NULL THEN
      RAISE EXCEPTION 'Stage 6 SLO approval gate is incomplete' USING ERRCODE = '23514';
    END IF;
  ELSIF NEW.rejected_at IS NULL OR NEW.approved_at IS NOT NULL THEN
    RAISE EXCEPTION 'Stage 6 SLO rejection timestamp is required' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_slo_windows_guard
BEFORE INSERT OR UPDATE ON stage6_slo_windows
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_slo_window();

CREATE OR REPLACE FUNCTION enforce_stage6_slo_objective()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  parent stage6_slo_windows%ROWTYPE;
  receipt_objective JSONB;
BEGIN
  SELECT * INTO parent FROM stage6_slo_windows WHERE id = NEW.slo_window_record_id FOR UPDATE;
  receipt_objective := convert_from(parent.receipt, 'UTF8')::jsonb->'objectives'->NEW.objective_key;
  IF parent.id IS NULL OR parent.operator_tenant_id <> NEW.operator_tenant_id
     OR parent.state <> 'recorded' OR receipt_objective IS NULL
     OR (receipt_objective->>'targetRatio')::double precision <> NEW.target_ratio
     OR (receipt_objective->>'goodRatio')::double precision <> NEW.good_ratio
     OR (receipt_objective->>'sampleCount')::bigint <> NEW.sample_count
     OR (receipt_objective->>'errorBudgetRemainingRatio')::double precision <> NEW.error_budget_remaining_ratio
     OR receipt_objective->>'errorBudgetPolicyState' <> NEW.policy_state
     OR (receipt_objective->>'assessable')::boolean IS DISTINCT FROM NEW.assessable
     OR (receipt_objective->>'objectiveMet')::boolean IS DISTINCT FROM NEW.objective_met THEN
    RAISE EXCEPTION 'Invalid Stage 6 SLO objective projection' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_slo_objectives_insert
BEFORE INSERT ON stage6_slo_objectives
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_slo_objective();

CREATE OR REPLACE FUNCTION enforce_stage6_slo_approval()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  parent stage6_slo_windows%ROWTYPE;
BEGIN
  SELECT * INTO parent FROM stage6_slo_windows WHERE id = NEW.slo_window_record_id FOR UPDATE;
  IF parent.id IS NULL OR parent.operator_tenant_id <> NEW.operator_tenant_id OR parent.state <> 'recorded'
     OR parent.created_by = NEW.approver_user_id
     OR NEW.evidence_reference !~ '^https://'
     OR NOT EXISTS (
       SELECT 1 FROM stage6_governance_authority_grants authority
       JOIN tenant_memberships membership ON membership.tenant_id = authority.operator_tenant_id
         AND membership.user_id = authority.user_id
       JOIN tenants operator_tenant ON operator_tenant.id = authority.operator_tenant_id
       JOIN users governed_user ON governed_user.id = authority.user_id
       WHERE authority.operator_tenant_id = NEW.operator_tenant_id
         AND authority.user_id = NEW.approver_user_id
         AND authority.authority_key = 'release.' || NEW.approval_role
         AND authority.status = 'active' AND authority.expires_at > statement_timestamp()
         AND membership.status = 'active' AND membership.role IN ('owner', 'admin', 'security_admin')
         AND operator_tenant.status = 'active' AND operator_tenant.deleted_at IS NULL
         AND governed_user.status = 'active' AND governed_user.deleted_at IS NULL
     ) THEN
    RAISE EXCEPTION 'Invalid Stage 6 SLO approval authority' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_slo_approvals_insert
BEFORE INSERT ON stage6_slo_approvals
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_slo_approval();

CREATE OR REPLACE FUNCTION reject_stage6_slo_history_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'Stage 6 SLO evidence history is immutable' USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER trg_stage6_slo_objectives_no_update BEFORE UPDATE ON stage6_slo_objectives
FOR EACH ROW EXECUTE FUNCTION reject_stage6_slo_history_mutation();
CREATE TRIGGER trg_stage6_slo_objectives_no_delete BEFORE DELETE ON stage6_slo_objectives
FOR EACH ROW EXECUTE FUNCTION reject_stage6_slo_history_mutation();
CREATE TRIGGER trg_stage6_slo_approvals_no_update BEFORE UPDATE ON stage6_slo_approvals
FOR EACH ROW EXECUTE FUNCTION reject_stage6_slo_history_mutation();
CREATE TRIGGER trg_stage6_slo_approvals_no_delete BEFORE DELETE ON stage6_slo_approvals
FOR EACH ROW EXECUTE FUNCTION reject_stage6_slo_history_mutation();
CREATE TRIGGER trg_stage6_slo_windows_no_delete BEFORE DELETE ON stage6_slo_windows
FOR EACH ROW EXECUTE FUNCTION reject_stage6_slo_history_mutation();
