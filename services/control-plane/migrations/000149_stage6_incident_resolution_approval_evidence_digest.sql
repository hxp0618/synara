ALTER TABLE stage6_incident_resolution_approvals
  ADD COLUMN evidence_sha256 TEXT,
  ADD COLUMN superseded_at TIMESTAMPTZ,
  ADD COLUMN superseded_reason TEXT;

DROP TRIGGER trg_stage6_incident_resolution_approvals_no_update ON stage6_incident_resolution_approvals;

UPDATE stage6_incident_resolution_approvals
SET superseded_at = now(),
    superseded_reason = 'Automatically superseded during Migration 000149 because Incident resolution approval evidence was not byte-bound.'
WHERE evidence_sha256 IS NULL AND superseded_at IS NULL;

ALTER TABLE stage6_incident_resolution_approvals
  ADD CONSTRAINT stage6_incident_resolution_approval_evidence_sha256_shape CHECK (
    evidence_sha256 IS NULL OR evidence_sha256 ~ '^sha256:[0-9a-f]{64}$'
  ),
  ADD CONSTRAINT stage6_incident_resolution_approval_supersession_shape CHECK (
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
  WHERE conrelid = 'stage6_incident_resolution_approvals'::regclass AND contype = 'u'
    AND pg_get_constraintdef(oid) = 'UNIQUE (incident_id)';
  IF constraint_name IS NOT NULL THEN
    EXECUTE format('ALTER TABLE stage6_incident_resolution_approvals DROP CONSTRAINT %I', constraint_name);
  END IF;
END;
$$;

CREATE UNIQUE INDEX uq_stage6_incident_resolution_approval_active
  ON stage6_incident_resolution_approvals (incident_id)
  WHERE superseded_at IS NULL;

CREATE OR REPLACE FUNCTION enforce_stage6_incident_resolution_approval()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM stage6_incidents incident
    JOIN tenant_memberships membership
      ON membership.tenant_id = incident.operator_tenant_id
     AND membership.user_id = NEW.approver_user_id
     AND membership.status = 'active' AND membership.role IN ('owner', 'admin', 'security_admin')
    JOIN users operator_user ON operator_user.id = membership.user_id
     AND operator_user.status = 'active' AND operator_user.deleted_at IS NULL
    WHERE incident.id = NEW.incident_id
      AND incident.operator_tenant_id = NEW.operator_tenant_id
      AND incident.security_privacy_impact
      AND incident.state = 'monitoring'
      AND incident.security_privacy_lead_user_id = NEW.approver_user_id
      AND incident.incident_commander_user_id <> NEW.approver_user_id
  ) OR NEW.evidence_reference !~ '^https://'
    OR NEW.evidence_sha256 IS NULL
    OR NEW.evidence_sha256 = 'sha256:' || repeat('0', 64)
    OR NEW.superseded_at IS NOT NULL OR NEW.superseded_reason IS NOT NULL THEN
    RAISE EXCEPTION 'Invalid Stage 6 byte-bound incident resolution approval' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_incident_resolution_approvals_no_update
BEFORE UPDATE ON stage6_incident_resolution_approvals
FOR EACH ROW EXECUTE FUNCTION reject_stage6_incident_update_mutation();

CREATE OR REPLACE FUNCTION enforce_stage6_incident_resolution_approval_digest_gate()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.state = 'resolved' AND NEW.security_privacy_impact AND NOT EXISTS (
    SELECT 1 FROM stage6_incident_resolution_approvals AS approval
    WHERE approval.incident_id = NEW.id
      AND approval.decision = 'approved'
      AND approval.superseded_at IS NULL
      AND approval.evidence_sha256 IS NOT NULL
      AND approval.evidence_sha256 <> 'sha256:' || repeat('0', 64)
  ) THEN
    RAISE EXCEPTION 'Stage 6 security/privacy incident resolution requires an active byte-bound approval'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_stage6_incident_resolution_approval_digest_gate ON stage6_incidents;
CREATE TRIGGER trg_stage6_incident_resolution_approval_digest_gate
BEFORE UPDATE ON stage6_incidents
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_incident_resolution_approval_digest_gate();

COMMENT ON COLUMN stage6_incident_resolution_approvals.evidence_sha256 IS
  'SHA-256 of the exact external containment and resolution evidence bytes reviewed for this immutable Security/Privacy decision.';
