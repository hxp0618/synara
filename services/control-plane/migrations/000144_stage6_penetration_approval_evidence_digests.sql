ALTER TABLE stage6_penetration_approvals
  ADD COLUMN evidence_sha256 TEXT,
  ADD COLUMN superseded_at TIMESTAMPTZ,
  ADD COLUMN superseded_reason TEXT;

DROP TRIGGER trg_stage6_penetration_engagements_guard ON stage6_penetration_engagements;
DROP TRIGGER trg_stage6_penetration_approvals_no_update ON stage6_penetration_approvals;

UPDATE stage6_penetration_approvals
SET superseded_at = now(),
    superseded_reason = 'Automatically superseded during Migration 000144 because Penetration approval evidence was not byte-bound.'
WHERE evidence_sha256 IS NULL AND superseded_at IS NULL;

UPDATE stage6_penetration_engagements AS target_engagement
SET state = 'recorded', version = target_engagement.version + 1,
    approved_at = NULL, updated_at = now()
WHERE target_engagement.state = 'approved'
  AND EXISTS (
    SELECT 1 FROM stage6_penetration_approvals AS approval
    WHERE approval.penetration_engagement_id = target_engagement.id
      AND approval.superseded_at IS NOT NULL
  );

ALTER TABLE stage6_penetration_approvals
  ADD CONSTRAINT stage6_penetration_approval_evidence_sha256_shape CHECK (
    evidence_sha256 IS NULL OR evidence_sha256 ~ '^sha256:[0-9a-f]{64}$'
  ),
  ADD CONSTRAINT stage6_penetration_approval_supersession_shape CHECK (
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
  WHERE conrelid = 'stage6_penetration_approvals'::regclass AND contype = 'u'
    AND pg_get_constraintdef(oid) = 'UNIQUE (penetration_engagement_id, approval_role)';
  IF constraint_name IS NOT NULL THEN
    EXECUTE format('ALTER TABLE stage6_penetration_approvals DROP CONSTRAINT %I', constraint_name);
  END IF;
  SELECT conname INTO constraint_name FROM pg_constraint
  WHERE conrelid = 'stage6_penetration_approvals'::regclass AND contype = 'u'
    AND pg_get_constraintdef(oid) = 'UNIQUE (penetration_engagement_id, approver_user_id)';
  IF constraint_name IS NOT NULL THEN
    EXECUTE format('ALTER TABLE stage6_penetration_approvals DROP CONSTRAINT %I', constraint_name);
  END IF;
END;
$$;

CREATE UNIQUE INDEX uq_stage6_penetration_approvals_role_active
  ON stage6_penetration_approvals (penetration_engagement_id, approval_role)
  WHERE superseded_at IS NULL;
CREATE UNIQUE INDEX uq_stage6_penetration_approvals_user_active
  ON stage6_penetration_approvals (penetration_engagement_id, approver_user_id)
  WHERE superseded_at IS NULL;

CREATE OR REPLACE FUNCTION enforce_stage6_penetration_engagement()
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
       OR NEW.receipt_schema <> 'synara.third-party-penetration-evidence-receipt.v1'
       OR NEW.assessment <> 'evidence-validated-not-penetration-passed'
       OR receipt_json->>'schemaVersion' <> NEW.receipt_schema
       OR receipt_json->>'assessment' <> NEW.assessment
       OR receipt_json->>'engagementId' <> NEW.engagement_id::text
       OR receipt_json->>'releaseCommit' <> NEW.release_commit
       OR receipt_json->>'environmentClass' <> NEW.environment_class
       OR receipt_json->>'environmentId' <> NEW.environment_id
       OR receipt_json->>'deploymentProfile' <> NEW.deployment_profile
       OR (receipt_json->>'thirdPartyIndependenceDeclared')::boolean IS DISTINCT FROM NEW.third_party_independence_declared
       OR (receipt_json#>>'{stage5Dependency,declaredSatisfied}')::boolean IS DISTINCT FROM NEW.stage5_dependency_satisfied
       OR (receipt_json->>'assetCoverageComplete')::boolean IS DISTINCT FROM NEW.asset_coverage_complete
       OR (receipt_json->>'scopeCoverageComplete')::boolean IS DISTINCT FROM NEW.scope_coverage_complete
       OR (receipt_json->>'methodologyCoverageComplete')::boolean IS DISTINCT FROM NEW.methodology_coverage_complete
       OR (receipt_json->>'declaredNoUnacceptedHighOrCriticalFindings')::boolean IS DISTINCT FROM NEW.no_unaccepted_high_or_critical_findings
       OR (receipt_json->>'eligibleForHumanGateReview')::boolean IS DISTINCT FROM NEW.eligible_for_human_gate_review
       OR NOT (receipt_json->>'releaseEligibleEnvironment')::boolean
       OR jsonb_array_length(receipt_json->'assets') <> 4
       OR NOT NEW.third_party_independence_declared OR NOT NEW.stage5_dependency_satisfied
       OR NOT NEW.asset_coverage_complete OR NOT NEW.scope_coverage_complete
       OR NOT NEW.methodology_coverage_complete OR NOT NEW.no_unaccepted_high_or_critical_findings
       OR NOT NEW.eligible_for_human_gate_review OR NEW.cryptographic_signatures_verified
       OR NOT NEW.external_authority_verification_required
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
                 #>> '{receipts,penetration,sha256}' = 'sha256:' || encode(NEW.receipt_sha256, 'hex')
       ) THEN
      RAISE EXCEPTION 'Invalid Stage 6 Penetration engagement receipt binding' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
  END IF;

  IF NEW.id IS DISTINCT FROM OLD.id OR NEW.operator_tenant_id IS DISTINCT FROM OLD.operator_tenant_id
     OR NEW.candidate_record_id IS DISTINCT FROM OLD.candidate_record_id OR NEW.engagement_id IS DISTINCT FROM OLD.engagement_id
     OR NEW.receipt IS DISTINCT FROM OLD.receipt OR NEW.receipt_sha256 IS DISTINCT FROM OLD.receipt_sha256
     OR NEW.receipt_size_bytes IS DISTINCT FROM OLD.receipt_size_bytes OR NEW.receipt_schema IS DISTINCT FROM OLD.receipt_schema
     OR NEW.assessment IS DISTINCT FROM OLD.assessment OR NEW.release_commit IS DISTINCT FROM OLD.release_commit
     OR NEW.environment_class IS DISTINCT FROM OLD.environment_class OR NEW.environment_id IS DISTINCT FROM OLD.environment_id
     OR NEW.deployment_profile IS DISTINCT FROM OLD.deployment_profile OR NEW.started_at IS DISTINCT FROM OLD.started_at
     OR NEW.completed_at IS DISTINCT FROM OLD.completed_at OR NEW.report_issued_at IS DISTINCT FROM OLD.report_issued_at
     OR NEW.validated_at IS DISTINCT FROM OLD.validated_at
     OR NEW.third_party_independence_declared IS DISTINCT FROM OLD.third_party_independence_declared
     OR NEW.stage5_dependency_satisfied IS DISTINCT FROM OLD.stage5_dependency_satisfied
     OR NEW.asset_coverage_complete IS DISTINCT FROM OLD.asset_coverage_complete
     OR NEW.scope_coverage_complete IS DISTINCT FROM OLD.scope_coverage_complete
     OR NEW.methodology_coverage_complete IS DISTINCT FROM OLD.methodology_coverage_complete
     OR NEW.no_unaccepted_high_or_critical_findings IS DISTINCT FROM OLD.no_unaccepted_high_or_critical_findings
     OR NEW.eligible_for_human_gate_review IS DISTINCT FROM OLD.eligible_for_human_gate_review
     OR NEW.cryptographic_signatures_verified IS DISTINCT FROM OLD.cryptographic_signatures_verified
     OR NEW.external_authority_verification_required IS DISTINCT FROM OLD.external_authority_verification_required
     OR NEW.created_by IS DISTINCT FROM OLD.created_by OR NEW.created_at IS DISTINCT FROM OLD.created_at
     OR NEW.version <> OLD.version + 1 OR OLD.state <> 'recorded' OR NEW.state NOT IN ('approved', 'rejected') THEN
    RAISE EXCEPTION 'Stage 6 Penetration identity and evidence are immutable' USING ERRCODE = '23514';
  END IF;
  IF NEW.state = 'approved' THEN
    SELECT count(*) INTO approval_count FROM stage6_penetration_approvals
    WHERE penetration_engagement_id = NEW.id AND decision = 'approved'
      AND superseded_at IS NULL AND evidence_sha256 IS NOT NULL
      AND evidence_sha256 <> 'sha256:' || repeat('0', 64);
    IF approval_count <> 3 OR NOT NEW.eligible_for_human_gate_review
       OR NEW.approved_at IS NULL OR NEW.rejected_at IS NOT NULL THEN
      RAISE EXCEPTION 'Stage 6 Penetration byte-bound approval gate is incomplete' USING ERRCODE = '23514';
    END IF;
  ELSIF NEW.rejected_at IS NULL OR NEW.approved_at IS NOT NULL THEN
    RAISE EXCEPTION 'Stage 6 Penetration rejection timestamp is required' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_penetration_engagements_guard
BEFORE INSERT OR UPDATE ON stage6_penetration_engagements
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_penetration_engagement();

CREATE OR REPLACE FUNCTION enforce_stage6_penetration_approval()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  parent stage6_penetration_engagements%ROWTYPE;
BEGIN
  SELECT * INTO parent FROM stage6_penetration_engagements WHERE id = NEW.penetration_engagement_id FOR UPDATE;
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
         AND authority.authority_key = 'penetration.' || NEW.approval_role
         AND authority.status = 'active' AND authority.expires_at > statement_timestamp()
         AND membership.status = 'active' AND membership.role IN ('owner', 'admin', 'security_admin')
         AND operator_tenant.status = 'active' AND operator_tenant.deleted_at IS NULL
         AND governed_user.status = 'active' AND governed_user.deleted_at IS NULL
     ) THEN
    RAISE EXCEPTION 'Invalid Stage 6 byte-bound Penetration approval authority' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_penetration_approvals_no_update
BEFORE UPDATE ON stage6_penetration_approvals
FOR EACH ROW EXECUTE FUNCTION reject_stage6_penetration_history_mutation();

CREATE OR REPLACE FUNCTION enforce_stage6_release_penetration_approval_gate()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.state IN ('approved', 'deploying', 'observing', 'released') AND NOT EXISTS (
    SELECT 1 FROM stage6_penetration_engagements AS penetration
    WHERE penetration.candidate_record_id = NEW.id
      AND penetration.operator_tenant_id = NEW.operator_tenant_id
      AND penetration.state = 'approved'
      AND penetration.third_party_independence_declared
      AND penetration.stage5_dependency_satisfied
      AND penetration.asset_coverage_complete
      AND penetration.scope_coverage_complete
      AND penetration.methodology_coverage_complete
      AND penetration.no_unaccepted_high_or_critical_findings
      AND penetration.eligible_for_human_gate_review
      AND NOT penetration.cryptographic_signatures_verified
      AND penetration.external_authority_verification_required
      AND 3 = (
        SELECT count(*) FROM stage6_penetration_approvals AS approval
        WHERE approval.penetration_engagement_id = penetration.id
          AND approval.decision = 'approved' AND approval.superseded_at IS NULL
          AND approval.evidence_sha256 IS NOT NULL
          AND approval.evidence_sha256 <> 'sha256:' || repeat('0', 64)
      )
  ) THEN
    RAISE EXCEPTION 'Stage 6 release transition requires an approved Penetration engagement with three byte-bound decisions'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_stage6_release_penetration_approval_gate ON stage6_release_candidates;
CREATE TRIGGER trg_stage6_release_penetration_approval_gate
BEFORE UPDATE ON stage6_release_candidates
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_release_penetration_approval_gate();

COMMENT ON COLUMN stage6_penetration_approvals.evidence_sha256 IS
  'SHA-256 of the exact external evidence bytes reviewed for this immutable Penetration decision.';
