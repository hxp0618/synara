CREATE TABLE IF NOT EXISTS stage6_governance_authority_grants (
  id UUID PRIMARY KEY,
  operator_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  authority_key TEXT NOT NULL CHECK (authority_key IN (
    'release.engineering', 'release.operations', 'release.security', 'release.product', 'release.privacy_legal',
    'compliance.security', 'compliance.operations', 'compliance.legal_privacy', 'compliance.executive',
    'compliance.evidence.security', 'compliance.evidence.operations', 'compliance.evidence.legal_privacy', 'compliance.evidence.auditor',
    'provider_commercial.legal', 'provider_commercial.privacy', 'provider_commercial.security', 'provider_commercial.product'
  )),
  status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'revoked')),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  expires_at TIMESTAMPTZ NOT NULL,
  granted_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  reason TEXT NOT NULL CHECK (length(btrim(reason)) BETWEEN 10 AND 2000),
  evidence_reference TEXT NOT NULL CHECK (stage6_valid_https_reference(evidence_reference)),
  revoked_at TIMESTAMPTZ,
  revoked_by UUID REFERENCES users(id) ON DELETE RESTRICT,
  revocation_reason TEXT CHECK (revocation_reason IS NULL OR length(btrim(revocation_reason)) BETWEEN 10 AND 2000),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  FOREIGN KEY (operator_tenant_id, user_id)
    REFERENCES tenant_memberships(tenant_id, user_id) ON DELETE RESTRICT,
  FOREIGN KEY (operator_tenant_id, granted_by)
    REFERENCES tenant_memberships(tenant_id, user_id) ON DELETE RESTRICT,
  CHECK (user_id <> granted_by),
  CHECK (expires_at > created_at AND expires_at <= created_at + interval '366 days'),
  CHECK (
    (status = 'active' AND revoked_at IS NULL AND revoked_by IS NULL AND revocation_reason IS NULL)
    OR
    (status = 'revoked' AND revoked_at IS NOT NULL AND revoked_by IS NOT NULL AND revocation_reason IS NOT NULL)
  )
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_stage6_governance_authority_active
  ON stage6_governance_authority_grants (operator_tenant_id, user_id, authority_key)
  WHERE status = 'active';

CREATE INDEX IF NOT EXISTS idx_stage6_governance_authority_lookup
  ON stage6_governance_authority_grants (operator_tenant_id, user_id, authority_key, status, expires_at DESC);

CREATE OR REPLACE FUNCTION validate_stage6_governance_authority_grant()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.created_at < statement_timestamp() - interval '5 minutes'
     OR NEW.created_at > statement_timestamp() + interval '5 minutes'
     OR NEW.expires_at > statement_timestamp() + interval '366 days' THEN
    RAISE EXCEPTION 'invalid Stage 6 governance authority time boundary';
  END IF;
  IF NOT EXISTS (
    SELECT 1
    FROM tenant_memberships AS membership
    JOIN tenants AS tenant ON tenant.id = membership.tenant_id
    JOIN users AS target_user ON target_user.id = membership.user_id
    WHERE membership.tenant_id = NEW.operator_tenant_id
      AND membership.user_id = NEW.user_id
      AND membership.status = 'active'
      AND membership.role IN ('owner', 'admin', 'security_admin')
      AND tenant.status = 'active' AND tenant.deleted_at IS NULL
      AND target_user.status = 'active' AND target_user.deleted_at IS NULL
  ) OR NOT EXISTS (
    SELECT 1
    FROM tenant_memberships AS membership
    JOIN users AS grantor ON grantor.id = membership.user_id
    WHERE membership.tenant_id = NEW.operator_tenant_id
      AND membership.user_id = NEW.granted_by
      AND membership.status = 'active' AND membership.role = 'owner'
      AND grantor.status = 'active' AND grantor.deleted_at IS NULL
  ) THEN
    RAISE EXCEPTION 'invalid Stage 6 governance authority principals';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_stage6_governance_authority_insert ON stage6_governance_authority_grants;
CREATE TRIGGER trg_stage6_governance_authority_insert
BEFORE INSERT ON stage6_governance_authority_grants
FOR EACH ROW EXECUTE FUNCTION validate_stage6_governance_authority_grant();

CREATE OR REPLACE FUNCTION protect_stage6_governance_authority_grant()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.id IS DISTINCT FROM OLD.id
     OR NEW.operator_tenant_id IS DISTINCT FROM OLD.operator_tenant_id
     OR NEW.user_id IS DISTINCT FROM OLD.user_id
     OR NEW.authority_key IS DISTINCT FROM OLD.authority_key
     OR NEW.expires_at IS DISTINCT FROM OLD.expires_at
     OR NEW.granted_by IS DISTINCT FROM OLD.granted_by
     OR NEW.reason IS DISTINCT FROM OLD.reason
     OR NEW.evidence_reference IS DISTINCT FROM OLD.evidence_reference
     OR NEW.created_at IS DISTINCT FROM OLD.created_at
     OR OLD.status <> 'active' OR NEW.status <> 'revoked'
     OR NEW.version <> OLD.version + 1
     OR NEW.revoked_at IS NULL OR NEW.revoked_by IS NULL OR NEW.revocation_reason IS NULL
     OR NEW.revoked_by = OLD.user_id
     OR NOT EXISTS (
       SELECT 1 FROM tenant_memberships AS membership
       JOIN users AS revoker ON revoker.id = membership.user_id
       WHERE membership.tenant_id = OLD.operator_tenant_id
         AND membership.user_id = NEW.revoked_by
         AND membership.status = 'active' AND membership.role = 'owner'
         AND revoker.status = 'active' AND revoker.deleted_at IS NULL
     ) THEN
    RAISE EXCEPTION 'invalid Stage 6 governance authority revocation';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_stage6_governance_authority_update ON stage6_governance_authority_grants;
CREATE TRIGGER trg_stage6_governance_authority_update
BEFORE UPDATE ON stage6_governance_authority_grants
FOR EACH ROW EXECUTE FUNCTION protect_stage6_governance_authority_grant();

CREATE OR REPLACE FUNCTION prevent_stage6_governance_authority_delete()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'Stage 6 governance authority history is immutable';
END;
$$;

DROP TRIGGER IF EXISTS trg_stage6_governance_authority_delete ON stage6_governance_authority_grants;
CREATE TRIGGER trg_stage6_governance_authority_delete
BEFORE DELETE ON stage6_governance_authority_grants
FOR EACH ROW EXECUTE FUNCTION prevent_stage6_governance_authority_delete();

CREATE OR REPLACE FUNCTION require_stage6_governance_authority()
RETURNS trigger
LANGUAGE plpgsql
AS $$
DECLARE
  required_key TEXT;
  governed_user_id UUID;
  governed_tenant_id UUID;
BEGIN
  IF TG_TABLE_NAME = 'stage6_release_approvals' THEN
    required_key := 'release.' || NEW.approval_role;
    governed_user_id := NEW.approver_user_id;
    governed_tenant_id := NEW.operator_tenant_id;
  ELSIF TG_TABLE_NAME = 'stage6_compliance_program_decisions' THEN
    required_key := 'compliance.' || NEW.decision_role;
    governed_user_id := NEW.decider_user_id;
    governed_tenant_id := NEW.operator_tenant_id;
  ELSIF TG_TABLE_NAME = 'stage6_compliance_evidence_reviews' THEN
    required_key := 'compliance.evidence.' || NEW.review_role;
    governed_user_id := NEW.reviewer_user_id;
    governed_tenant_id := NEW.operator_tenant_id;
  ELSIF TG_TABLE_NAME = 'provider_commercial_authorization_approvals' THEN
    required_key := 'provider_commercial.' || NEW.approval_role;
    governed_user_id := NEW.approver_user_id;
    governed_tenant_id := NEW.operator_tenant_id;
  ELSE
    RAISE EXCEPTION 'unsupported Stage 6 governance authority target';
  END IF;

  IF NOT EXISTS (
    SELECT 1 FROM stage6_governance_authority_grants AS authority
    JOIN tenant_memberships AS membership
      ON membership.tenant_id = authority.operator_tenant_id AND membership.user_id = authority.user_id
    JOIN tenants AS operator_tenant ON operator_tenant.id = authority.operator_tenant_id
    JOIN users AS governed_user ON governed_user.id = authority.user_id
    WHERE authority.operator_tenant_id = governed_tenant_id
      AND authority.user_id = governed_user_id
      AND authority.authority_key = required_key
      AND authority.status = 'active' AND authority.expires_at > statement_timestamp()
      AND membership.status = 'active' AND membership.role IN ('owner', 'admin', 'security_admin')
      AND operator_tenant.status = 'active' AND operator_tenant.deleted_at IS NULL
      AND governed_user.status = 'active' AND governed_user.deleted_at IS NULL
  ) THEN
    RAISE EXCEPTION 'matching active Stage 6 governance authority is required';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_stage6_release_approval_authority ON stage6_release_approvals;
CREATE TRIGGER trg_stage6_release_approval_authority
BEFORE INSERT ON stage6_release_approvals
FOR EACH ROW EXECUTE FUNCTION require_stage6_governance_authority();

DROP TRIGGER IF EXISTS trg_stage6_compliance_decision_authority ON stage6_compliance_program_decisions;
CREATE TRIGGER trg_stage6_compliance_decision_authority
BEFORE INSERT ON stage6_compliance_program_decisions
FOR EACH ROW EXECUTE FUNCTION require_stage6_governance_authority();

DROP TRIGGER IF EXISTS trg_stage6_compliance_evidence_review_authority ON stage6_compliance_evidence_reviews;
CREATE TRIGGER trg_stage6_compliance_evidence_review_authority
BEFORE INSERT ON stage6_compliance_evidence_reviews
FOR EACH ROW EXECUTE FUNCTION require_stage6_governance_authority();

DROP TRIGGER IF EXISTS trg_provider_commercial_approval_authority ON provider_commercial_authorization_approvals;
CREATE TRIGGER trg_provider_commercial_approval_authority
BEFORE INSERT ON provider_commercial_authorization_approvals
FOR EACH ROW EXECUTE FUNCTION require_stage6_governance_authority();
