ALTER TABLE tenant_memberships
  DROP CONSTRAINT IF EXISTS tenant_memberships_role_check;

ALTER TABLE tenant_invitations
  DROP CONSTRAINT IF EXISTS tenant_invitations_role_check;

ALTER TABLE identity_group_mappings
  DROP CONSTRAINT IF EXISTS identity_group_mappings_tenant_role_check;

ALTER TABLE tenant_memberships
  ADD CONSTRAINT tenant_memberships_role_check CHECK (
    role IN ('owner', 'admin', 'security_admin', 'cost_admin', 'auditor', 'member')
  ) NOT VALID;

ALTER TABLE tenant_invitations
  ADD CONSTRAINT tenant_invitations_role_check CHECK (
    role IN ('owner', 'admin', 'security_admin', 'cost_admin', 'auditor', 'member')
  ) NOT VALID;

ALTER TABLE identity_group_mappings
  ADD CONSTRAINT identity_group_mappings_tenant_role_check CHECK (
    tenant_role IS NULL
    OR tenant_role IN ('owner', 'admin', 'security_admin', 'cost_admin', 'auditor', 'member')
  ) NOT VALID;

COMMENT ON CONSTRAINT tenant_memberships_role_check ON tenant_memberships IS
  'Internal self-hosted Tenant roles use cost_admin for usage, token and cost governance; payment administration is unsupported.';

UPDATE tenant_memberships
SET role = 'cost_admin',
    updated_at = now()
WHERE role = 'billing_admin';

UPDATE tenant_invitations
SET role = 'cost_admin'
WHERE role = 'billing_admin';

UPDATE identity_group_mappings
SET tenant_role = 'cost_admin'
WHERE tenant_role = 'billing_admin';
