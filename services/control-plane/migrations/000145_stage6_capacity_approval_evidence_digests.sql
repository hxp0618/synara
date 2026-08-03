ALTER TABLE stage6_capacity_approvals
  ADD COLUMN evidence_sha256 TEXT,
  ADD COLUMN superseded_at TIMESTAMPTZ,
  ADD COLUMN superseded_reason TEXT;

DROP TRIGGER trg_stage6_capacity_runs_guard ON stage6_capacity_runs;
DROP TRIGGER trg_stage6_capacity_approvals_no_update ON stage6_capacity_approvals;

UPDATE stage6_capacity_approvals
SET superseded_at = now(),
    superseded_reason = 'Automatically superseded during Migration 000145 because Capacity approval evidence was not byte-bound.'
WHERE evidence_sha256 IS NULL AND superseded_at IS NULL;

UPDATE stage6_capacity_runs AS target_run
SET state = 'recorded', version = target_run.version + 1,
    approved_at = NULL, updated_at = now()
WHERE target_run.state = 'approved'
  AND EXISTS (
    SELECT 1 FROM stage6_capacity_approvals AS approval
    WHERE approval.capacity_run_id = target_run.id
      AND approval.superseded_at IS NOT NULL
  );

ALTER TABLE stage6_capacity_approvals
  ADD CONSTRAINT stage6_capacity_approval_evidence_sha256_shape CHECK (
    evidence_sha256 IS NULL OR evidence_sha256 ~ '^sha256:[0-9a-f]{64}$'
  ),
  ADD CONSTRAINT stage6_capacity_approval_supersession_shape CHECK (
    (superseded_at IS NULL AND superseded_reason IS NULL
      AND evidence_sha256 IS NOT NULL
      AND evidence_sha256 <> 'sha256:' || repeat('0', 64))
    OR
    (superseded_at IS NOT NULL AND length(trim(superseded_reason)) BETWEEN 10 AND 500)
  );

DO $$
DECLARE
  constraint_name TEXT;
BEGIN
  SELECT conname INTO constraint_name FROM pg_constraint
  WHERE conrelid = 'stage6_capacity_approvals'::regclass AND contype = 'u'
    AND pg_get_constraintdef(oid) = 'UNIQUE (capacity_run_id, approval_role)';
  IF constraint_name IS NOT NULL THEN
    EXECUTE format('ALTER TABLE stage6_capacity_approvals DROP CONSTRAINT %I', constraint_name);
  END IF;
  SELECT conname INTO constraint_name FROM pg_constraint
  WHERE conrelid = 'stage6_capacity_approvals'::regclass AND contype = 'u'
    AND pg_get_constraintdef(oid) = 'UNIQUE (capacity_run_id, approver_user_id)';
  IF constraint_name IS NOT NULL THEN
    EXECUTE format('ALTER TABLE stage6_capacity_approvals DROP CONSTRAINT %I', constraint_name);
  END IF;
END;
$$;

CREATE UNIQUE INDEX uq_stage6_capacity_approvals_role_active
  ON stage6_capacity_approvals (capacity_run_id, approval_role)
  WHERE superseded_at IS NULL;
CREATE UNIQUE INDEX uq_stage6_capacity_approvals_user_active
  ON stage6_capacity_approvals (capacity_run_id, approver_user_id)
  WHERE superseded_at IS NULL;

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
    WHERE capacity_run_id = NEW.id AND decision = 'approved'
      AND superseded_at IS NULL AND evidence_sha256 IS NOT NULL
      AND evidence_sha256 <> 'sha256:' || repeat('0', 64);
    IF approval_count <> 2 OR NOT NEW.eligible_for_human_gate_review
       OR NEW.approved_at IS NULL OR NEW.rejected_at IS NOT NULL THEN
      RAISE EXCEPTION 'Stage 6 Capacity byte-bound approval gate is incomplete' USING ERRCODE = '23514';
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
     OR NEW.evidence_sha256 IS NULL OR NEW.evidence_sha256 = 'sha256:' || repeat('0', 64)
     OR NEW.superseded_at IS NOT NULL OR NEW.superseded_reason IS NOT NULL
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
    RAISE EXCEPTION 'Invalid Stage 6 byte-bound Capacity approval authority' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_capacity_approvals_no_update
BEFORE UPDATE ON stage6_capacity_approvals
FOR EACH ROW EXECUTE FUNCTION reject_stage6_capacity_history_mutation();

CREATE OR REPLACE FUNCTION enforce_stage6_release_capacity_approval_gate()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.state IN ('approved', 'deploying', 'observing', 'released') AND NOT EXISTS (
    SELECT 1 FROM stage6_capacity_runs AS capacity
    WHERE capacity.candidate_record_id = NEW.id
      AND capacity.operator_tenant_id = NEW.operator_tenant_id
      AND capacity.state = 'approved'
      AND capacity.forecast_headroom_covered
      AND capacity.phase_coverage_complete
      AND capacity.exercise_coverage_complete
      AND capacity.measurements_within_objectives
      AND capacity.release_eligible_environment
      AND capacity.eligible_for_human_gate_review
      AND NOT capacity.cryptographic_signatures_verified
      AND capacity.external_authority_verification_required
      AND 2 = (
        SELECT count(*) FROM stage6_capacity_approvals AS approval
        WHERE approval.capacity_run_id = capacity.id
          AND approval.decision = 'approved' AND approval.superseded_at IS NULL
          AND approval.evidence_sha256 IS NOT NULL
          AND approval.evidence_sha256 <> 'sha256:' || repeat('0', 64)
      )
  ) THEN
    RAISE EXCEPTION 'Stage 6 release transition requires an approved Capacity run with two byte-bound decisions'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_stage6_release_capacity_approval_gate ON stage6_release_candidates;
CREATE TRIGGER trg_stage6_release_capacity_approval_gate
BEFORE UPDATE ON stage6_release_candidates
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_release_capacity_approval_gate();

COMMENT ON COLUMN stage6_capacity_approvals.evidence_sha256 IS
  'SHA-256 of the exact external evidence bytes reviewed for this immutable Capacity decision.';
