ALTER TABLE stage6_recovery_approvals
  ADD COLUMN evidence_sha256 TEXT,
  ADD COLUMN superseded_at TIMESTAMPTZ,
  ADD COLUMN superseded_reason TEXT;

DROP TRIGGER trg_stage6_recovery_drills_guard ON stage6_recovery_drills;
DROP TRIGGER trg_stage6_recovery_approvals_no_update ON stage6_recovery_approvals;

UPDATE stage6_recovery_approvals
SET superseded_at = now(),
    superseded_reason = 'Automatically superseded during Migration 000143 because Recovery approval evidence was not byte-bound.'
WHERE evidence_sha256 IS NULL AND superseded_at IS NULL;

UPDATE stage6_recovery_drills AS target_drill
SET state = 'recorded', version = target_drill.version + 1,
    approved_at = NULL, updated_at = now()
WHERE target_drill.state = 'approved'
  AND EXISTS (
    SELECT 1 FROM stage6_recovery_approvals AS approval
    WHERE approval.recovery_drill_record_id = target_drill.id
      AND approval.superseded_at IS NOT NULL
  );

ALTER TABLE stage6_recovery_approvals
  ADD CONSTRAINT stage6_recovery_approval_evidence_sha256_shape CHECK (
    evidence_sha256 IS NULL OR evidence_sha256 ~ '^sha256:[0-9a-f]{64}$'
  ),
  ADD CONSTRAINT stage6_recovery_approval_supersession_shape CHECK (
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
  WHERE conrelid = 'stage6_recovery_approvals'::regclass AND contype = 'u'
    AND pg_get_constraintdef(oid) = 'UNIQUE (recovery_drill_record_id, approval_role)';
  IF constraint_name IS NOT NULL THEN
    EXECUTE format('ALTER TABLE stage6_recovery_approvals DROP CONSTRAINT %I', constraint_name);
  END IF;
  SELECT conname INTO constraint_name FROM pg_constraint
  WHERE conrelid = 'stage6_recovery_approvals'::regclass AND contype = 'u'
    AND pg_get_constraintdef(oid) = 'UNIQUE (recovery_drill_record_id, approver_user_id)';
  IF constraint_name IS NOT NULL THEN
    EXECUTE format('ALTER TABLE stage6_recovery_approvals DROP CONSTRAINT %I', constraint_name);
  END IF;
END;
$$;

CREATE UNIQUE INDEX uq_stage6_recovery_approvals_role_active
  ON stage6_recovery_approvals (recovery_drill_record_id, approval_role)
  WHERE superseded_at IS NULL;
CREATE UNIQUE INDEX uq_stage6_recovery_approvals_user_active
  ON stage6_recovery_approvals (recovery_drill_record_id, approver_user_id)
  WHERE superseded_at IS NULL;

CREATE OR REPLACE FUNCTION enforce_stage6_recovery_drill()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  receipt_json JSONB;
  candidate_receipt JSONB;
  expected_candidate JSONB;
  candidate stage6_release_candidates%ROWTYPE;
  approval_count INTEGER;
BEGIN
  IF TG_OP = 'INSERT' THEN
    receipt_json := convert_from(NEW.receipt, 'UTF8')::jsonb;
    SELECT * INTO candidate FROM stage6_release_candidates WHERE id = NEW.candidate_record_id;
    candidate_receipt := convert_from(candidate.evidence_bundle_receipt, 'UTF8')::jsonb;
    expected_candidate := candidate_receipt -> 'candidate';
    expected_candidate := expected_candidate - 'desktopArtifactSetSha256'::text;
    IF candidate.id IS NULL OR candidate.operator_tenant_id <> NEW.operator_tenant_id
       OR candidate.created_by <> NEW.created_by
       OR digest(NEW.receipt, 'sha256') <> NEW.receipt_sha256
       OR jsonb_typeof(receipt_json) <> 'object'
       OR (SELECT count(*) FROM jsonb_object_keys(receipt_json)) <> 20
       OR receipt_json->>'schemaVersion' <> NEW.receipt_schema
       OR receipt_json->>'assessment' <> NEW.assessment
       OR (receipt_json->>'drillId')::uuid <> NEW.drill_id
       OR decode(substring(receipt_json->>'candidateBindingSha256' FROM 8), 'hex') <> NEW.candidate_binding_sha256
       OR decode(substring(receipt_json->>'recoverySubjectSha256' FROM 8), 'hex') <> NEW.recovery_subject_sha256
       OR (receipt_json->>'startedAt')::timestamptz <> NEW.started_at
       OR (receipt_json->>'completedAt')::timestamptz <> NEW.completed_at
       OR (receipt_json->>'validatedAt')::timestamptz <> NEW.validated_at
       OR NEW.completed_at <= NEW.started_at OR NEW.validated_at < NEW.completed_at
       OR NEW.validated_at > NEW.completed_at + interval '7 days'
       OR (receipt_json->>'declaredMeasurementsWithinObjectives')::boolean IS DISTINCT FROM NEW.measurements_within_objectives
       OR (receipt_json->>'allRestoreCanariesPassed')::boolean IS DISTINCT FROM NEW.all_restore_canaries_passed
       OR (receipt_json->>'allRequiredApprovalsApproved')::boolean IS DISTINCT FROM NEW.all_source_approvals_approved
       OR (receipt_json->>'eligibleForHumanGateReview')::boolean IS DISTINCT FROM NEW.eligible_for_human_gate_review
       OR (receipt_json->'verificationBoundary'->>'cryptographicSignaturesVerified')::boolean IS DISTINCT FROM NEW.cryptographic_signatures_verified
       OR (receipt_json->'verificationBoundary'->>'realBackupRestoreAndApproverAuthorityVerificationRequired')::boolean IS DISTINCT FROM NEW.external_authority_verification_required
       OR NEW.cryptographic_signatures_verified OR NOT NEW.external_authority_verification_required
       OR jsonb_typeof(receipt_json->'components') <> 'object'
       OR (SELECT count(*) FROM jsonb_object_keys(receipt_json->'components')) <> 4
       OR jsonb_typeof(receipt_json->'approvals') <> 'object'
       OR (SELECT count(*) FROM jsonb_object_keys(receipt_json->'approvals')) <> 5
       OR receipt_json->('candidate'::text) <> expected_candidate
       OR receipt_json->('restoredReleaseIdentity'::text) <> jsonb_build_object(
         'sourceCommit', receipt_json->('candidate'::text)->('sourceCommit'::text),
         'lockfileSha256', receipt_json->('candidate'::text)->('lockfileSha256'::text),
         'artifacts', receipt_json->('candidate'::text)->('artifacts'::text),
         'migrationTail', receipt_json->('candidate'::text)->('migrationTail'::text)
       )
       OR NEW.state <> 'recorded' OR NEW.version <> 1
       OR NEW.approved_at IS NOT NULL OR NEW.rejected_at IS NOT NULL THEN
      RAISE EXCEPTION 'Invalid Stage 6 Recovery drill receipt binding' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
  END IF;

  IF NEW.id IS DISTINCT FROM OLD.id OR NEW.operator_tenant_id IS DISTINCT FROM OLD.operator_tenant_id
     OR NEW.candidate_record_id IS DISTINCT FROM OLD.candidate_record_id OR NEW.drill_id IS DISTINCT FROM OLD.drill_id
     OR NEW.receipt IS DISTINCT FROM OLD.receipt OR NEW.receipt_sha256 IS DISTINCT FROM OLD.receipt_sha256
     OR NEW.receipt_size_bytes IS DISTINCT FROM OLD.receipt_size_bytes OR NEW.receipt_schema IS DISTINCT FROM OLD.receipt_schema
     OR NEW.assessment IS DISTINCT FROM OLD.assessment OR NEW.candidate_binding_sha256 IS DISTINCT FROM OLD.candidate_binding_sha256
     OR NEW.recovery_subject_sha256 IS DISTINCT FROM OLD.recovery_subject_sha256 OR NEW.started_at IS DISTINCT FROM OLD.started_at
     OR NEW.completed_at IS DISTINCT FROM OLD.completed_at OR NEW.validated_at IS DISTINCT FROM OLD.validated_at
     OR NEW.measurements_within_objectives IS DISTINCT FROM OLD.measurements_within_objectives
     OR NEW.all_restore_canaries_passed IS DISTINCT FROM OLD.all_restore_canaries_passed
     OR NEW.all_source_approvals_approved IS DISTINCT FROM OLD.all_source_approvals_approved
     OR NEW.eligible_for_human_gate_review IS DISTINCT FROM OLD.eligible_for_human_gate_review
     OR NEW.cryptographic_signatures_verified IS DISTINCT FROM OLD.cryptographic_signatures_verified
     OR NEW.external_authority_verification_required IS DISTINCT FROM OLD.external_authority_verification_required
     OR NEW.created_by IS DISTINCT FROM OLD.created_by OR NEW.created_at IS DISTINCT FROM OLD.created_at
     OR NEW.version <> OLD.version + 1 OR OLD.state <> 'recorded' OR NEW.state NOT IN ('approved', 'rejected') THEN
    RAISE EXCEPTION 'Stage 6 Recovery identity and evidence are immutable' USING ERRCODE = '23514';
  END IF;
  IF NEW.state = 'approved' THEN
    SELECT count(*) INTO approval_count FROM stage6_recovery_approvals
    WHERE recovery_drill_record_id = NEW.id AND decision = 'approved'
      AND superseded_at IS NULL AND evidence_sha256 IS NOT NULL
      AND evidence_sha256 <> 'sha256:' || repeat('0', 64);
    IF NOT NEW.eligible_for_human_gate_review OR approval_count <> 5
       OR NEW.approved_at IS NULL OR NEW.rejected_at IS NOT NULL THEN
      RAISE EXCEPTION 'Stage 6 Recovery byte-bound approval gate is incomplete' USING ERRCODE = '23514';
    END IF;
  ELSIF NEW.rejected_at IS NULL OR NEW.approved_at IS NOT NULL THEN
    RAISE EXCEPTION 'Stage 6 Recovery rejection timestamp is required' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_recovery_drills_guard
BEFORE INSERT OR UPDATE ON stage6_recovery_drills
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_recovery_drill();

CREATE OR REPLACE FUNCTION enforce_stage6_recovery_approval()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  parent stage6_recovery_drills%ROWTYPE;
BEGIN
  SELECT * INTO parent FROM stage6_recovery_drills WHERE id = NEW.recovery_drill_record_id FOR UPDATE;
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
         AND authority.authority_key = 'recovery.' || NEW.approval_role
         AND authority.status = 'active' AND authority.expires_at > statement_timestamp()
         AND membership.status = 'active' AND membership.role IN ('owner', 'admin', 'security_admin')
         AND operator_tenant.status = 'active' AND operator_tenant.deleted_at IS NULL
         AND governed_user.status = 'active' AND governed_user.deleted_at IS NULL
     ) THEN
    RAISE EXCEPTION 'Invalid Stage 6 byte-bound Recovery approval authority' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_recovery_approvals_no_update
BEFORE UPDATE ON stage6_recovery_approvals
FOR EACH ROW EXECUTE FUNCTION reject_stage6_recovery_history_mutation();

CREATE OR REPLACE FUNCTION enforce_stage6_release_recovery_approval_gate()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.state IN ('approved', 'deploying', 'observing', 'released') AND NOT EXISTS (
    SELECT 1 FROM stage6_recovery_drills AS recovery
    WHERE recovery.candidate_record_id = NEW.id
      AND recovery.operator_tenant_id = NEW.operator_tenant_id
      AND recovery.state = 'approved'
      AND recovery.eligible_for_human_gate_review
      AND recovery.measurements_within_objectives
      AND recovery.all_restore_canaries_passed
      AND recovery.all_source_approvals_approved
      AND NOT recovery.cryptographic_signatures_verified
      AND recovery.external_authority_verification_required
      AND 5 = (
        SELECT count(*) FROM stage6_recovery_approvals AS approval
        WHERE approval.recovery_drill_record_id = recovery.id
          AND approval.decision = 'approved' AND approval.superseded_at IS NULL
          AND approval.evidence_sha256 IS NOT NULL
          AND approval.evidence_sha256 <> 'sha256:' || repeat('0', 64)
      )
  ) THEN
    RAISE EXCEPTION 'Stage 6 release transition requires an approved Recovery drill with five byte-bound decisions'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_stage6_release_recovery_approval_gate ON stage6_release_candidates;
CREATE TRIGGER trg_stage6_release_recovery_approval_gate
BEFORE UPDATE ON stage6_release_candidates
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_release_recovery_approval_gate();

COMMENT ON COLUMN stage6_recovery_approvals.evidence_sha256 IS
  'SHA-256 of the exact external evidence bytes reviewed for this immutable Recovery decision.';
