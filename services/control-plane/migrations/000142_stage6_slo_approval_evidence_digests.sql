ALTER TABLE stage6_slo_approvals
  ADD COLUMN evidence_sha256 TEXT,
  ADD COLUMN superseded_at TIMESTAMPTZ,
  ADD COLUMN superseded_reason TEXT;

DROP TRIGGER trg_stage6_slo_windows_guard ON stage6_slo_windows;
DROP TRIGGER trg_stage6_slo_approvals_no_update ON stage6_slo_approvals;

UPDATE stage6_slo_approvals
SET superseded_at = now(),
    superseded_reason = 'Automatically superseded during Migration 000142 because SLO approval evidence was not byte-bound.'
WHERE evidence_sha256 IS NULL AND superseded_at IS NULL;

UPDATE stage6_slo_windows AS target_window
SET state = 'recorded',
    version = target_window.version + 1,
    approved_at = NULL,
    updated_at = now()
WHERE target_window.state = 'approved'
  AND EXISTS (
    SELECT 1 FROM stage6_slo_approvals AS approval
    WHERE approval.slo_window_record_id = target_window.id
      AND approval.superseded_at IS NOT NULL
  );

ALTER TABLE stage6_slo_approvals
  ADD CONSTRAINT stage6_slo_approval_evidence_sha256_shape CHECK (
    evidence_sha256 IS NULL OR evidence_sha256 ~ '^sha256:[0-9a-f]{64}$'
  ),
  ADD CONSTRAINT stage6_slo_approval_supersession_shape CHECK (
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
  SELECT constraint_record.conname INTO constraint_name
  FROM pg_constraint AS constraint_record
  WHERE constraint_record.conrelid = 'stage6_slo_approvals'::regclass
    AND constraint_record.contype = 'u'
    AND pg_get_constraintdef(constraint_record.oid) = 'UNIQUE (slo_window_record_id, approval_role)';
  IF constraint_name IS NOT NULL THEN
    EXECUTE format('ALTER TABLE stage6_slo_approvals DROP CONSTRAINT %I', constraint_name);
  END IF;

  SELECT constraint_record.conname INTO constraint_name
  FROM pg_constraint AS constraint_record
  WHERE constraint_record.conrelid = 'stage6_slo_approvals'::regclass
    AND constraint_record.contype = 'u'
    AND pg_get_constraintdef(constraint_record.oid) = 'UNIQUE (slo_window_record_id, approver_user_id)';
  IF constraint_name IS NOT NULL THEN
    EXECUTE format('ALTER TABLE stage6_slo_approvals DROP CONSTRAINT %I', constraint_name);
  END IF;
END;
$$;

CREATE UNIQUE INDEX uq_stage6_slo_approvals_role_active
  ON stage6_slo_approvals (slo_window_record_id, approval_role)
  WHERE superseded_at IS NULL;
CREATE UNIQUE INDEX uq_stage6_slo_approvals_user_active
  ON stage6_slo_approvals (slo_window_record_id, approver_user_id)
  WHERE superseded_at IS NULL;

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
    WHERE slo_window_record_id = NEW.id AND decision = 'approved'
      AND superseded_at IS NULL AND evidence_sha256 IS NOT NULL
      AND evidence_sha256 <> 'sha256:' || repeat('0', 64);
    IF NOT NEW.eligible_for_human_gate_review OR approval_count <> 4
       OR NEW.approved_at IS NULL OR NEW.rejected_at IS NOT NULL THEN
      RAISE EXCEPTION 'Stage 6 SLO byte-bound approval gate is incomplete' USING ERRCODE = '23514';
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
         AND authority.authority_key = 'release.' || NEW.approval_role
         AND authority.status = 'active' AND authority.expires_at > statement_timestamp()
         AND membership.status = 'active' AND membership.role IN ('owner', 'admin', 'security_admin')
         AND operator_tenant.status = 'active' AND operator_tenant.deleted_at IS NULL
         AND governed_user.status = 'active' AND governed_user.deleted_at IS NULL
     ) THEN
    RAISE EXCEPTION 'Invalid Stage 6 byte-bound SLO approval authority' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_slo_approvals_no_update
BEFORE UPDATE ON stage6_slo_approvals
FOR EACH ROW EXECUTE FUNCTION reject_stage6_slo_history_mutation();

CREATE OR REPLACE FUNCTION enforce_stage6_release_slo_approval_gate()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.state <> OLD.state OR NEW.state IN ('approved', 'deploying', 'observing', 'released') THEN
    IF NEW.state IN ('approved', 'deploying', 'observing', 'released') AND NOT EXISTS (
      SELECT 1
      FROM stage6_slo_windows AS slo
      WHERE slo.candidate_record_id = NEW.id
        AND slo.operator_tenant_id = NEW.operator_tenant_id
        AND slo.state = 'approved'
        AND slo.eligible_for_human_gate_review
        AND slo.all_objectives_assessable
        AND slo.all_objectives_met
        AND 4 = (
          SELECT count(*) FROM stage6_slo_approvals AS approval
          WHERE approval.slo_window_record_id = slo.id
            AND approval.decision = 'approved'
            AND approval.superseded_at IS NULL
            AND approval.evidence_sha256 IS NOT NULL
            AND approval.evidence_sha256 <> 'sha256:' || repeat('0', 64)
        )
    ) THEN
      RAISE EXCEPTION 'Stage 6 release transition requires an approved SLO window with four byte-bound decisions'
        USING ERRCODE = '23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_stage6_release_slo_approval_gate ON stage6_release_candidates;
CREATE TRIGGER trg_stage6_release_slo_approval_gate
BEFORE UPDATE ON stage6_release_candidates
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_release_slo_approval_gate();

COMMENT ON COLUMN stage6_slo_approvals.evidence_sha256 IS
  'SHA-256 of the exact external evidence bytes reviewed for this immutable SLO decision.';
