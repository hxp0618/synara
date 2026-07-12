CREATE TABLE identity_connections (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  kind TEXT NOT NULL CHECK (kind IN ('oidc', 'saml')),
  name TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
  issuer TEXT NOT NULL,
  client_id TEXT,
  encrypted_secret BYTEA,
  encrypted_data_key BYTEA,
  kms_provider TEXT,
  kms_key_id TEXT,
  configuration JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_by UUID NOT NULL REFERENCES users(id),
  updated_by UUID NOT NULL REFERENCES users(id),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, name),
  CHECK (length(btrim(name)) BETWEEN 1 AND 160),
  CHECK (length(btrim(issuer)) BETWEEN 1 AND 1000),
  CHECK ((kind = 'oidc' AND client_id IS NOT NULL AND length(btrim(client_id)) BETWEEN 1 AND 500) OR kind = 'saml'),
  CHECK ((encrypted_secret IS NULL AND encrypted_data_key IS NULL AND kms_provider IS NULL AND kms_key_id IS NULL) OR
         (encrypted_secret IS NOT NULL AND encrypted_data_key IS NOT NULL AND kms_provider IS NOT NULL AND kms_key_id IS NOT NULL))
);

CREATE INDEX idx_identity_connections_tenant_status
  ON identity_connections (tenant_id, status, kind, id);

CREATE TRIGGER trg_identity_connections_updated_at
BEFORE UPDATE ON identity_connections
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE user_identities
  ADD COLUMN connection_id UUID REFERENCES identity_connections(id) ON DELETE SET NULL;

CREATE INDEX idx_user_identities_connection_subject
  ON user_identities (connection_id, subject)
  WHERE connection_id IS NOT NULL;

CREATE TABLE identity_login_attempts (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL,
  connection_id UUID NOT NULL,
  state_hash BYTEA NOT NULL UNIQUE,
  encrypted_payload BYTEA NOT NULL,
  encrypted_data_key BYTEA NOT NULL,
  kms_provider TEXT NOT NULL,
  kms_key_id TEXT NOT NULL,
  return_to TEXT NOT NULL DEFAULT '/',
  expires_at TIMESTAMPTZ NOT NULL,
  consumed_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  FOREIGN KEY (tenant_id, connection_id)
    REFERENCES identity_connections(tenant_id, id) ON DELETE CASCADE,
  CHECK (expires_at > created_at),
  CHECK (length(return_to) BETWEEN 1 AND 1000)
);

CREATE INDEX idx_identity_login_attempts_expiry
  ON identity_login_attempts (expires_at, id)
  WHERE consumed_at IS NULL;

CREATE TABLE service_accounts (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  organization_id UUID,
  name TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'revoked')),
  scopes JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(scopes) = 'array'),
  created_by UUID NOT NULL REFERENCES users(id),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at TIMESTAMPTZ,
  UNIQUE (tenant_id, id),
  UNIQUE (tenant_id, name),
  FOREIGN KEY (tenant_id, organization_id)
    REFERENCES organizations(tenant_id, id) ON DELETE RESTRICT,
  CHECK (length(btrim(name)) BETWEEN 1 AND 160),
  CHECK (length(description) <= 1000),
  CHECK ((status = 'revoked' AND revoked_at IS NOT NULL) OR status = 'active')
);

CREATE TRIGGER trg_service_accounts_updated_at
BEFORE UPDATE ON service_accounts
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE service_account_tokens (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL,
  service_account_id UUID NOT NULL,
  token_hash BYTEA NOT NULL UNIQUE,
  expires_at TIMESTAMPTZ,
  last_used_at TIMESTAMPTZ,
  revoked_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  FOREIGN KEY (tenant_id, service_account_id)
    REFERENCES service_accounts(tenant_id, id) ON DELETE CASCADE,
  CHECK (expires_at IS NULL OR expires_at > created_at)
);

CREATE INDEX idx_service_account_tokens_active
  ON service_account_tokens (service_account_id, expires_at)
  WHERE revoked_at IS NULL;

CREATE TABLE identity_groups (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  external_id TEXT,
  display_name TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'deleted')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at TIMESTAMPTZ,
  UNIQUE (tenant_id, id),
  CHECK (length(btrim(display_name)) BETWEEN 1 AND 160),
  CHECK (external_id IS NULL OR length(external_id) BETWEEN 1 AND 500),
  CHECK ((status = 'deleted' AND deleted_at IS NOT NULL) OR status = 'active')
);

CREATE UNIQUE INDEX uq_identity_groups_external_id
  ON identity_groups (tenant_id, external_id)
  WHERE external_id IS NOT NULL AND deleted_at IS NULL;

CREATE TRIGGER trg_identity_groups_updated_at
BEFORE UPDATE ON identity_groups
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE identity_group_members (
  tenant_id UUID NOT NULL,
  group_id UUID NOT NULL,
  user_id UUID NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (group_id, user_id),
  FOREIGN KEY (tenant_id, group_id)
    REFERENCES identity_groups(tenant_id, id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, user_id)
    REFERENCES tenant_memberships(tenant_id, user_id) ON DELETE CASCADE
);

CREATE INDEX idx_identity_group_members_tenant_user
  ON identity_group_members (tenant_id, user_id, group_id);

CREATE TABLE identity_group_mappings (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL,
  connection_id UUID NOT NULL,
  external_group TEXT NOT NULL,
  tenant_role TEXT CHECK (tenant_role IN ('owner', 'admin', 'security_admin', 'billing_admin', 'auditor', 'member')),
  organization_id UUID,
  organization_role TEXT CHECK (organization_role IN ('owner', 'admin', 'agent_operator', 'member', 'viewer')),
  created_by UUID NOT NULL REFERENCES users(id),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  FOREIGN KEY (tenant_id, connection_id)
    REFERENCES identity_connections(tenant_id, id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, organization_id)
    REFERENCES organizations(tenant_id, id) ON DELETE CASCADE,
  CHECK (length(btrim(external_group)) BETWEEN 1 AND 500),
  CHECK (tenant_role IS NOT NULL OR (organization_id IS NOT NULL AND organization_role IS NOT NULL)),
  CHECK ((organization_id IS NULL AND organization_role IS NULL) OR (organization_id IS NOT NULL AND organization_role IS NOT NULL))
);

CREATE INDEX idx_identity_group_mappings_connection_group
  ON identity_group_mappings (connection_id, external_group);

CREATE UNIQUE INDEX uq_identity_group_mappings_tenant_role
  ON identity_group_mappings (connection_id, external_group)
  WHERE organization_id IS NULL;

CREATE UNIQUE INDEX uq_identity_group_mappings_organization_role
  ON identity_group_mappings (connection_id, external_group, organization_id)
  WHERE organization_id IS NOT NULL;
