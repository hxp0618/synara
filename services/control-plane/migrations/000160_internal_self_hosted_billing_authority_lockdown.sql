DROP TRIGGER IF EXISTS trg_stage6_governance_authority_update ON stage6_governance_authority_grants;

UPDATE stage6_governance_authority_grants
SET status = 'revoked',
    version = version + 1,
    revoked_at = COALESCE(revoked_at, statement_timestamp()),
    revoked_by = COALESCE(revoked_by, granted_by),
    revocation_reason = COALESCE(
      revocation_reason,
      'Automatically revoked during Migration 000160 because internal self-hosted products do not support billing exercises.'
    ),
    updated_at = statement_timestamp()
WHERE status = 'active'
  AND authority_key LIKE 'billing_exercise.%';

CREATE TRIGGER trg_stage6_governance_authority_update
BEFORE UPDATE ON stage6_governance_authority_grants
FOR EACH ROW EXECUTE FUNCTION protect_stage6_governance_authority_grant();

ALTER TABLE stage6_governance_authority_grants
  DROP CONSTRAINT IF EXISTS stage6_governance_authority_grants_authority_key_check;
ALTER TABLE stage6_governance_authority_grants
  ADD CONSTRAINT stage6_governance_authority_grants_authority_key_check CHECK (
    authority_key IN (
      'release.engineering', 'release.operations', 'release.security', 'release.product', 'release.privacy_legal',
      'compliance.security', 'compliance.operations', 'compliance.legal_privacy', 'compliance.executive',
      'compliance.evidence.security', 'compliance.evidence.operations', 'compliance.evidence.legal_privacy', 'compliance.evidence.auditor',
      'provider_commercial.legal', 'provider_commercial.privacy', 'provider_commercial.security', 'provider_commercial.product',
      'recovery.database', 'recovery.kms', 'recovery.operations', 'recovery.security', 'recovery.storage',
      'penetration.engineering', 'penetration.product', 'penetration.security',
      'capacity.engineering', 'capacity.operations',
      'incident_exercise.operations', 'incident_exercise.communications',
      'operations_exercise.operations', 'operations_exercise.security',
      'internal_cost.operations', 'internal_cost.owner'
    ) OR (
      status = 'revoked'
      AND authority_key IN ('billing_exercise.finance', 'billing_exercise.security', 'billing_exercise.release')
    )
  );

CREATE OR REPLACE FUNCTION validate_stage6_governance_authority_grant()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.authority_key LIKE 'billing_exercise.%' THEN
    RAISE EXCEPTION 'billing exercise governance authority is historical-only for internal self-hosted products';
  END IF;
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
