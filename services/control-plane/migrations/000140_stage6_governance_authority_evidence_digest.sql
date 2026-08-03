ALTER TABLE stage6_governance_authority_grants
  ADD COLUMN evidence_sha256 TEXT,
  ADD CONSTRAINT stage6_governance_authority_evidence_sha256_shape CHECK (
    evidence_sha256 IS NULL OR evidence_sha256 ~ '^sha256:[0-9a-f]{64}$'
  );

DROP TRIGGER IF EXISTS trg_stage6_governance_authority_update ON stage6_governance_authority_grants;

-- URL-only authority evidence remains historical, but cannot continue granting
-- a Release, Compliance, Provider or exercise decision role after this upgrade.
UPDATE stage6_governance_authority_grants
SET status = 'revoked',
    version = version + 1,
    revoked_at = COALESCE(revoked_at, now()),
    revoked_by = COALESCE(revoked_by, granted_by),
    revocation_reason = COALESCE(
      revocation_reason,
      'Automatically revoked during Migration 000140 because authority evidence was not byte-bound.'
    ),
    updated_at = now()
WHERE status = 'active'
  AND evidence_sha256 IS NULL;

ALTER TABLE stage6_governance_authority_grants
  ADD CONSTRAINT stage6_governance_authority_active_evidence_bound CHECK (
    status <> 'active'
    OR (
      evidence_sha256 IS NOT NULL
      AND evidence_sha256 <> 'sha256:' || repeat('0', 64)
    )
  );

CREATE OR REPLACE FUNCTION validate_stage6_governance_authority_grant()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.evidence_sha256 IS NULL
     OR NEW.evidence_sha256 = 'sha256:' || repeat('0', 64)
     OR NEW.created_at < statement_timestamp() - interval '5 minutes'
     OR NEW.created_at > statement_timestamp() + interval '5 minutes'
     OR NEW.expires_at > statement_timestamp() + interval '366 days' THEN
    RAISE EXCEPTION 'invalid byte-bound Stage 6 governance authority grant';
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
     OR NEW.evidence_sha256 IS DISTINCT FROM OLD.evidence_sha256
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

CREATE TRIGGER trg_stage6_governance_authority_update
BEFORE UPDATE ON stage6_governance_authority_grants
FOR EACH ROW EXECUTE FUNCTION protect_stage6_governance_authority_grant();

COMMENT ON COLUMN stage6_governance_authority_grants.evidence_sha256 IS
  'SHA-256 of the exact corporate delegation evidence bytes supporting this immutable functional authority.';
