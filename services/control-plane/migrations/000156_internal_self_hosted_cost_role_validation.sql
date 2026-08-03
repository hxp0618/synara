ALTER TABLE tenant_memberships
  VALIDATE CONSTRAINT tenant_memberships_role_check;

ALTER TABLE tenant_invitations
  VALIDATE CONSTRAINT tenant_invitations_role_check;

ALTER TABLE identity_group_mappings
  VALIDATE CONSTRAINT identity_group_mappings_tenant_role_check;
