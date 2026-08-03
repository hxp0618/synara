ALTER TABLE stage6_release_approvals
  ADD COLUMN evidence_sha256 TEXT,
  ADD CONSTRAINT stage6_release_approval_evidence_sha256_shape CHECK (
    evidence_sha256 IS NULL OR evidence_sha256 ~ '^sha256:[0-9a-f]{64}$'
  );

CREATE OR REPLACE FUNCTION enforce_stage6_release_approval()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.evidence_sha256 IS NULL
     OR NEW.evidence_sha256 = 'sha256:' || repeat('0', 64)
     OR NOT EXISTS (
       SELECT 1 FROM stage6_release_candidates AS candidate
       JOIN tenant_memberships AS membership
         ON membership.tenant_id = candidate.operator_tenant_id
        AND membership.user_id = NEW.approver_user_id
        AND membership.status = 'active'
        AND membership.role IN ('owner', 'admin', 'security_admin')
       JOIN tenants AS operator_tenant
         ON operator_tenant.id = membership.tenant_id
        AND operator_tenant.status = 'active' AND operator_tenant.deleted_at IS NULL
       JOIN users AS approver
         ON approver.id = membership.user_id
        AND approver.status = 'active' AND approver.deleted_at IS NULL
       WHERE candidate.id = NEW.candidate_record_id
         AND candidate.operator_tenant_id = NEW.operator_tenant_id
         AND candidate.state = 'ready_for_review'
         AND candidate.created_by <> NEW.approver_user_id
     ) THEN
    RAISE EXCEPTION 'Stage 6 approval requires byte-bound evidence, a reviewable candidate and separated approver'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION enforce_stage6_release_candidate_approval_evidence_gate()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  required_approvals INTEGER;
  privacy_legal_approvals INTEGER;
BEGIN
  IF NEW.state = OLD.state OR NEW.state NOT IN ('approved', 'deploying', 'observing', 'released') THEN
    RETURN NEW;
  END IF;

  SELECT count(*) INTO required_approvals
  FROM stage6_release_approvals
  WHERE candidate_record_id = NEW.id
    AND decision = 'approved'
    AND approval_role IN ('engineering', 'operations', 'security', 'product')
    AND evidence_sha256 IS NOT NULL
    AND evidence_sha256 <> 'sha256:' || repeat('0', 64);

  SELECT count(*) INTO privacy_legal_approvals
  FROM stage6_release_approvals
  WHERE candidate_record_id = NEW.id
    AND decision = 'approved'
    AND approval_role = 'privacy_legal'
    AND evidence_sha256 IS NOT NULL
    AND evidence_sha256 <> 'sha256:' || repeat('0', 64);

  IF required_approvals <> 4
     OR (NEW.privacy_legal_required AND privacy_legal_approvals <> 1) THEN
    RAISE EXCEPTION 'Stage 6 release transition requires byte-bound impact-derived approval evidence'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_release_candidate_approval_evidence_gate
BEFORE UPDATE ON stage6_release_candidates
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_release_candidate_approval_evidence_gate();

COMMENT ON COLUMN stage6_release_approvals.evidence_sha256 IS
  'SHA-256 of the exact external evidence bytes reviewed for this immutable Release decision.';
