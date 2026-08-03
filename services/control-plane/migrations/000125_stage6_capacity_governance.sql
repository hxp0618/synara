ALTER TABLE stage6_governance_authority_grants
  DROP CONSTRAINT IF EXISTS stage6_governance_authority_grants_authority_key_check;
ALTER TABLE stage6_governance_authority_grants
  ADD CONSTRAINT stage6_governance_authority_grants_authority_key_check CHECK (authority_key IN (
    'release.engineering', 'release.operations', 'release.security', 'release.product', 'release.privacy_legal',
    'compliance.security', 'compliance.operations', 'compliance.legal_privacy', 'compliance.executive',
    'compliance.evidence.security', 'compliance.evidence.operations', 'compliance.evidence.legal_privacy', 'compliance.evidence.auditor',
    'provider_commercial.legal', 'provider_commercial.privacy', 'provider_commercial.security', 'provider_commercial.product',
    'recovery.database', 'recovery.kms', 'recovery.operations', 'recovery.security', 'recovery.storage',
    'penetration.engineering', 'penetration.product', 'penetration.security',
    'capacity.engineering', 'capacity.operations'
  ));

CREATE TABLE stage6_capacity_runs (
  id UUID PRIMARY KEY,
  operator_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  candidate_record_id UUID NOT NULL REFERENCES stage6_release_candidates(id) ON DELETE RESTRICT,
  run_id UUID NOT NULL UNIQUE,
  receipt BYTEA NOT NULL CHECK (octet_length(receipt) BETWEEN 1 AND 524288),
  receipt_sha256 BYTEA NOT NULL CHECK (octet_length(receipt_sha256) = 32),
  receipt_size_bytes BIGINT NOT NULL CHECK (receipt_size_bytes > 0),
  receipt_schema TEXT NOT NULL,
  assessment TEXT NOT NULL,
  release_commit TEXT NOT NULL CHECK (length(release_commit) = 40),
  environment_class TEXT NOT NULL CHECK (environment_class IN ('production', 'production-like')),
  environment_id TEXT NOT NULL CHECK (length(environment_id) BETWEEN 3 AND 300),
  started_at TIMESTAMPTZ NOT NULL,
  completed_at TIMESTAMPTZ NOT NULL,
  validated_at TIMESTAMPTZ NOT NULL,
  duration_seconds BIGINT NOT NULL CHECK (duration_seconds > 0),
  minimum_duration_seconds BIGINT NOT NULL CHECK (minimum_duration_seconds IN (86400, 259200)),
  sample_interval_seconds BIGINT NOT NULL CHECK (sample_interval_seconds BETWEEN 1 AND 60),
  external_probe_regions BIGINT NOT NULL CHECK (external_probe_regions >= 3),
  external_probe_coverage_ratio DOUBLE PRECISION NOT NULL CHECK (external_probe_coverage_ratio >= 0.95),
  forecast_headroom_covered BOOLEAN NOT NULL,
  phase_coverage_complete BOOLEAN NOT NULL,
  exercise_coverage_complete BOOLEAN NOT NULL,
  measurements_within_objectives BOOLEAN NOT NULL,
  release_eligible_environment BOOLEAN NOT NULL,
  eligible_for_human_gate_review BOOLEAN NOT NULL,
  cryptographic_signatures_verified BOOLEAN NOT NULL,
  external_authority_verification_required BOOLEAN NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('recorded', 'approved', 'rejected')),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  created_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  approved_at TIMESTAMPTZ,
  rejected_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (candidate_record_id, receipt_sha256),
  CHECK (started_at < completed_at AND completed_at <= validated_at)
);

CREATE INDEX idx_stage6_capacity_runs_candidate
  ON stage6_capacity_runs (candidate_record_id, created_at DESC, id);

CREATE TABLE stage6_capacity_phases (
  id UUID PRIMARY KEY,
  capacity_run_id UUID NOT NULL REFERENCES stage6_capacity_runs(id) ON DELETE RESTRICT,
  operator_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  phase_name TEXT NOT NULL CHECK (phase_name IN ('steady-peak', 'burst', 'tenant-hotspot', 'rolling-disruption', 'cooldown')),
  started_at TIMESTAMPTZ NOT NULL,
  completed_at TIMESTAMPTZ NOT NULL,
  duration_seconds BIGINT NOT NULL CHECK (duration_seconds > 0),
  load_multiplier DOUBLE PRECISION NOT NULL CHECK (load_multiplier >= 0.1),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (capacity_run_id, phase_name),
  CHECK (started_at < completed_at)
);

CREATE TABLE stage6_capacity_approvals (
  id UUID PRIMARY KEY,
  capacity_run_id UUID NOT NULL REFERENCES stage6_capacity_runs(id) ON DELETE RESTRICT,
  operator_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  approval_role TEXT NOT NULL CHECK (approval_role IN ('engineering', 'operations')),
  decision TEXT NOT NULL CHECK (decision IN ('approved', 'rejected')),
  approver_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  reason TEXT NOT NULL CHECK (length(trim(reason)) BETWEEN 20 AND 2000),
  evidence_reference TEXT NOT NULL CHECK (length(trim(evidence_reference)) BETWEEN 8 AND 2048),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (capacity_run_id, approval_role),
  UNIQUE (capacity_run_id, approver_user_id)
);

CREATE INDEX idx_stage6_capacity_approvals_run
  ON stage6_capacity_approvals (capacity_run_id, created_at, id);

CREATE OR REPLACE FUNCTION enforce_stage6_capacity_run()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  approval_count INTEGER;
  receipt_json JSONB;
BEGIN
  IF TG_OP = 'INSERT' THEN
    receipt_json := convert_from(NEW.receipt, 'UTF8')::jsonb;
    IF NEW.receipt_size_bytes <> octet_length(NEW.receipt)
       OR digest(NEW.receipt, 'sha256') <> NEW.receipt_sha256
       OR NEW.receipt_schema <> 'synara.capacity-soak-evidence-receipt.v1'
       OR NEW.assessment <> 'evidence-validated-not-capacity-passed'
       OR receipt_json->>'schemaVersion' <> NEW.receipt_schema
       OR receipt_json->>'assessment' <> NEW.assessment
       OR receipt_json->>'runId' <> NEW.run_id::text
       OR receipt_json->>'releaseCommit' <> NEW.release_commit
       OR receipt_json->>'environmentClass' <> NEW.environment_class
       OR receipt_json->>'environmentId' <> NEW.environment_id
       OR (receipt_json->>'durationSeconds')::numeric <> NEW.duration_seconds
       OR (receipt_json->>'minimumDurationSeconds')::numeric <> NEW.minimum_duration_seconds
       OR (receipt_json->>'sampleIntervalSeconds')::bigint <> NEW.sample_interval_seconds
       OR (receipt_json->>'externalProbeRegions')::bigint <> NEW.external_probe_regions
       OR (receipt_json->>'externalProbeCoverageRatio')::double precision <> NEW.external_probe_coverage_ratio
       OR (receipt_json->>'forecastHeadroomCovered')::boolean IS DISTINCT FROM NEW.forecast_headroom_covered
       OR (receipt_json->>'declaredMeasurementsWithinObjectives')::boolean IS DISTINCT FROM NEW.measurements_within_objectives
       OR (receipt_json->>'releaseEligibleEnvironment')::boolean IS DISTINCT FROM NEW.release_eligible_environment
       OR (receipt_json->>'eligibleForHumanGateReview')::boolean IS DISTINCT FROM NEW.eligible_for_human_gate_review
       OR jsonb_array_length(receipt_json->'phases') <> 5
       OR (SELECT count(*) FROM jsonb_object_keys(receipt_json->'evidence')) <> 7
       OR NOT NEW.forecast_headroom_covered OR NOT NEW.phase_coverage_complete
       OR NOT NEW.exercise_coverage_complete OR NOT NEW.measurements_within_objectives
       OR NOT NEW.release_eligible_environment OR NOT NEW.eligible_for_human_gate_review
       OR NEW.cryptographic_signatures_verified OR NOT NEW.external_authority_verification_required
       OR NEW.duration_seconds <> extract(epoch FROM NEW.completed_at - NEW.started_at)::bigint
       OR (NEW.environment_class = 'production' AND NEW.minimum_duration_seconds <> 259200)
       OR (NEW.environment_class = 'production-like' AND NEW.minimum_duration_seconds <> 86400)
       OR NEW.duration_seconds < NEW.minimum_duration_seconds
       OR NEW.state <> 'recorded' OR NEW.version <> 1
       OR NEW.approved_at IS NOT NULL OR NEW.rejected_at IS NOT NULL
       OR NOT EXISTS (
         SELECT 1 FROM stage6_release_candidates candidate
         WHERE candidate.id = NEW.candidate_record_id
           AND candidate.operator_tenant_id = NEW.operator_tenant_id
           AND candidate.created_by = NEW.created_by
           AND candidate.source_commit = NEW.release_commit
           AND candidate.environment_id = NEW.environment_id
           AND convert_from(candidate.evidence_bundle_receipt, 'UTF8')::jsonb
                 #>> '{candidate,environmentClass}' = NEW.environment_class
           AND convert_from(candidate.evidence_bundle_receipt, 'UTF8')::jsonb
                 #>> '{receipts,capacity,sha256}' = 'sha256:' || encode(NEW.receipt_sha256, 'hex')
       ) THEN
      RAISE EXCEPTION 'Invalid Stage 6 Capacity receipt binding' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
  END IF;

  IF NEW.id IS DISTINCT FROM OLD.id OR NEW.operator_tenant_id IS DISTINCT FROM OLD.operator_tenant_id
     OR NEW.candidate_record_id IS DISTINCT FROM OLD.candidate_record_id OR NEW.run_id IS DISTINCT FROM OLD.run_id
     OR NEW.receipt IS DISTINCT FROM OLD.receipt OR NEW.receipt_sha256 IS DISTINCT FROM OLD.receipt_sha256
     OR NEW.receipt_size_bytes IS DISTINCT FROM OLD.receipt_size_bytes OR NEW.receipt_schema IS DISTINCT FROM OLD.receipt_schema
     OR NEW.assessment IS DISTINCT FROM OLD.assessment OR NEW.release_commit IS DISTINCT FROM OLD.release_commit
     OR NEW.environment_class IS DISTINCT FROM OLD.environment_class OR NEW.environment_id IS DISTINCT FROM OLD.environment_id
     OR NEW.started_at IS DISTINCT FROM OLD.started_at OR NEW.completed_at IS DISTINCT FROM OLD.completed_at
     OR NEW.validated_at IS DISTINCT FROM OLD.validated_at OR NEW.duration_seconds IS DISTINCT FROM OLD.duration_seconds
     OR NEW.minimum_duration_seconds IS DISTINCT FROM OLD.minimum_duration_seconds
     OR NEW.sample_interval_seconds IS DISTINCT FROM OLD.sample_interval_seconds
     OR NEW.external_probe_regions IS DISTINCT FROM OLD.external_probe_regions
     OR NEW.external_probe_coverage_ratio IS DISTINCT FROM OLD.external_probe_coverage_ratio
     OR NEW.forecast_headroom_covered IS DISTINCT FROM OLD.forecast_headroom_covered
     OR NEW.phase_coverage_complete IS DISTINCT FROM OLD.phase_coverage_complete
     OR NEW.exercise_coverage_complete IS DISTINCT FROM OLD.exercise_coverage_complete
     OR NEW.measurements_within_objectives IS DISTINCT FROM OLD.measurements_within_objectives
     OR NEW.release_eligible_environment IS DISTINCT FROM OLD.release_eligible_environment
     OR NEW.eligible_for_human_gate_review IS DISTINCT FROM OLD.eligible_for_human_gate_review
     OR NEW.cryptographic_signatures_verified IS DISTINCT FROM OLD.cryptographic_signatures_verified
     OR NEW.external_authority_verification_required IS DISTINCT FROM OLD.external_authority_verification_required
     OR NEW.created_by IS DISTINCT FROM OLD.created_by OR NEW.created_at IS DISTINCT FROM OLD.created_at
     OR NEW.version <> OLD.version + 1 OR OLD.state <> 'recorded' OR NEW.state NOT IN ('approved', 'rejected') THEN
    RAISE EXCEPTION 'Stage 6 Capacity identity and evidence are immutable' USING ERRCODE = '23514';
  END IF;
  IF NEW.state = 'approved' THEN
    SELECT count(*) INTO approval_count FROM stage6_capacity_approvals
    WHERE capacity_run_id = NEW.id AND decision = 'approved';
    IF approval_count <> 2 OR NOT NEW.eligible_for_human_gate_review
       OR NEW.approved_at IS NULL OR NEW.rejected_at IS NOT NULL THEN
      RAISE EXCEPTION 'Stage 6 Capacity approval gate is incomplete' USING ERRCODE = '23514';
    END IF;
  ELSIF NEW.rejected_at IS NULL OR NEW.approved_at IS NOT NULL THEN
    RAISE EXCEPTION 'Stage 6 Capacity rejection timestamp is required' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_capacity_runs_guard
BEFORE INSERT OR UPDATE ON stage6_capacity_runs
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_capacity_run();

CREATE OR REPLACE FUNCTION enforce_stage6_capacity_phase()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  parent stage6_capacity_runs%ROWTYPE;
  phase JSONB;
BEGIN
  SELECT * INTO parent FROM stage6_capacity_runs WHERE id = NEW.capacity_run_id FOR UPDATE;
  SELECT item INTO phase FROM jsonb_array_elements(convert_from(parent.receipt, 'UTF8')::jsonb->'phases') item
  WHERE item->>'name' = NEW.phase_name;
  IF parent.id IS NULL OR parent.operator_tenant_id <> NEW.operator_tenant_id OR parent.state <> 'recorded'
     OR phase IS NULL OR (phase->>'durationSeconds')::numeric <> NEW.duration_seconds
     OR (phase->>'loadMultiplier')::double precision <> NEW.load_multiplier
     OR (phase->>'startedAt')::timestamptz <> NEW.started_at
     OR (phase->>'completedAt')::timestamptz <> NEW.completed_at THEN
    RAISE EXCEPTION 'Invalid Stage 6 Capacity phase projection' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_capacity_phases_insert
BEFORE INSERT ON stage6_capacity_phases
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_capacity_phase();

CREATE OR REPLACE FUNCTION enforce_stage6_capacity_approval()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  parent stage6_capacity_runs%ROWTYPE;
BEGIN
  SELECT * INTO parent FROM stage6_capacity_runs WHERE id = NEW.capacity_run_id FOR UPDATE;
  IF parent.id IS NULL OR parent.operator_tenant_id <> NEW.operator_tenant_id OR parent.state <> 'recorded'
     OR parent.created_by = NEW.approver_user_id
     OR NOT EXISTS (
       SELECT 1 FROM stage6_governance_authority_grants authority
       JOIN tenant_memberships membership ON membership.tenant_id = authority.operator_tenant_id
         AND membership.user_id = authority.user_id
       JOIN tenants operator_tenant ON operator_tenant.id = authority.operator_tenant_id
       JOIN users governed_user ON governed_user.id = authority.user_id
       WHERE authority.operator_tenant_id = NEW.operator_tenant_id
         AND authority.user_id = NEW.approver_user_id
         AND authority.authority_key = 'capacity.' || NEW.approval_role
         AND authority.status = 'active' AND authority.expires_at > statement_timestamp()
         AND membership.status = 'active' AND membership.role IN ('owner', 'admin', 'security_admin')
         AND operator_tenant.status = 'active' AND operator_tenant.deleted_at IS NULL
         AND governed_user.status = 'active' AND governed_user.deleted_at IS NULL
     ) THEN
    RAISE EXCEPTION 'Invalid Stage 6 Capacity approval authority' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_capacity_approvals_insert
BEFORE INSERT ON stage6_capacity_approvals
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_capacity_approval();

CREATE OR REPLACE FUNCTION reject_stage6_capacity_history_mutation()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'Stage 6 Capacity evidence history is immutable' USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER trg_stage6_capacity_phases_no_update BEFORE UPDATE ON stage6_capacity_phases
FOR EACH ROW EXECUTE FUNCTION reject_stage6_capacity_history_mutation();
CREATE TRIGGER trg_stage6_capacity_phases_no_delete BEFORE DELETE ON stage6_capacity_phases
FOR EACH ROW EXECUTE FUNCTION reject_stage6_capacity_history_mutation();
CREATE TRIGGER trg_stage6_capacity_approvals_no_update BEFORE UPDATE ON stage6_capacity_approvals
FOR EACH ROW EXECUTE FUNCTION reject_stage6_capacity_history_mutation();
CREATE TRIGGER trg_stage6_capacity_approvals_no_delete BEFORE DELETE ON stage6_capacity_approvals
FOR EACH ROW EXECUTE FUNCTION reject_stage6_capacity_history_mutation();
CREATE TRIGGER trg_stage6_capacity_runs_no_delete BEFORE DELETE ON stage6_capacity_runs
FOR EACH ROW EXECUTE FUNCTION reject_stage6_capacity_history_mutation();

COMMENT ON TABLE stage6_capacity_runs IS
  'Internal governance over exact capacity and soak receipts; approval does not authenticate the environment, telemetry, signatures, execution, or external approver authority.';
