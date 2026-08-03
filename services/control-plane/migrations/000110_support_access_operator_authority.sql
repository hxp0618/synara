ALTER TABLE support_access_grants
  ADD COLUMN operator_tenant_id UUID REFERENCES tenants(id) ON DELETE RESTRICT;

-- Existing pending/active grants deliberately remain unbound and therefore
-- fail closed in authorization. Operators must create a fresh four-eyes grant
-- after this migration instead of guessing historical authority provenance.
ALTER TABLE support_access_grants
  ADD CONSTRAINT ck_support_access_grants_operator_authority
  CHECK (
    status NOT IN ('pending', 'active')
    OR (
      operator_tenant_id IS NOT NULL
      AND operator_tenant_id <> tenant_id
    )
  ) NOT VALID;

CREATE INDEX idx_support_access_grants_operator_authorization
  ON support_access_grants (
    operator_tenant_id,
    requester_user_id,
    tenant_id,
    status,
    expires_at,
    id
  );

CREATE OR REPLACE FUNCTION enforce_support_access_grant_authority()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.operator_tenant_id IS NULL
       OR NEW.operator_tenant_id = NEW.tenant_id
       OR NOT EXISTS (
         SELECT 1
         FROM tenant_memberships AS operator_membership
         JOIN tenants AS operator_tenant
           ON operator_tenant.id = operator_membership.tenant_id
          AND operator_tenant.status = 'active'
          AND operator_tenant.deleted_at IS NULL
         JOIN users AS requester
           ON requester.id = operator_membership.user_id
          AND requester.status = 'active'
          AND requester.deleted_at IS NULL
         WHERE operator_membership.tenant_id = NEW.operator_tenant_id
           AND operator_membership.user_id = NEW.requester_user_id
           AND operator_membership.status = 'active'
           AND operator_membership.role IN ('owner', 'admin', 'security_admin')
       ) THEN
      RAISE EXCEPTION 'Support Access grant requires current Platform authority' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
  END IF;

  IF NEW.id IS DISTINCT FROM OLD.id
     OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
     OR NEW.operator_tenant_id IS DISTINCT FROM OLD.operator_tenant_id
     OR NEW.requester_user_id IS DISTINCT FROM OLD.requester_user_id
     OR NEW.reason IS DISTINCT FROM OLD.reason
     OR NEW.requested_duration_seconds IS DISTINCT FROM OLD.requested_duration_seconds
     OR NEW.requested_at IS DISTINCT FROM OLD.requested_at
     OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
    RAISE EXCEPTION 'Support Access grant identity is immutable' USING ERRCODE = '23514';
  END IF;

  IF (OLD.status = 'pending' AND NEW.status NOT IN ('active', 'denied'))
     OR (OLD.status = 'active' AND NEW.status NOT IN ('revoked', 'expired'))
     OR OLD.status IN ('denied', 'revoked', 'expired') THEN
    RAISE EXCEPTION 'Invalid Support Access grant transition' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_support_access_grants_authority
BEFORE INSERT OR UPDATE ON support_access_grants
FOR EACH ROW EXECUTE FUNCTION enforce_support_access_grant_authority();

COMMENT ON COLUMN support_access_grants.operator_tenant_id IS
  'Immutable Platform Operator Tenant authority. Active access is revalidated against current requester membership and Tenant lifecycle on every authorization.';
