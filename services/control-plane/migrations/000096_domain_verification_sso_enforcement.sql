CREATE TABLE tenant_domains (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  domain TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'verified', 'revoked')),
  verification_token_hash BYTEA NOT NULL,
  verification_expires_at TIMESTAMPTZ NOT NULL,
  verified_at TIMESTAMPTZ,
  verified_by UUID REFERENCES users(id) ON DELETE RESTRICT,
  revoked_at TIMESTAMPTZ,
  revoked_by UUID REFERENCES users(id) ON DELETE RESTRICT,
  created_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  CHECK (
    domain = lower(domain)
    AND domain ~ '^[a-z0-9][a-z0-9.-]*[a-z0-9]$'
    AND position('.' IN domain) > 1
    AND domain NOT LIKE '%..%'
  ),
  CHECK (length(domain) <= 253),
  CHECK (octet_length(verification_token_hash) = 32),
  CHECK (verification_expires_at > created_at),
  CHECK (
    (status = 'pending' AND verified_at IS NULL AND verified_by IS NULL AND revoked_at IS NULL AND revoked_by IS NULL)
    OR (status = 'verified' AND verified_at IS NOT NULL AND verified_by IS NOT NULL AND revoked_at IS NULL AND revoked_by IS NULL)
    OR (status = 'revoked' AND revoked_at IS NOT NULL AND revoked_by IS NOT NULL)
  )
);

CREATE UNIQUE INDEX uq_tenant_domains_claimed_domain
  ON tenant_domains (lower(domain))
  WHERE status IN ('pending', 'verified');

CREATE INDEX idx_tenant_domains_tenant_status
  ON tenant_domains (tenant_id, status, domain, id);

CREATE TRIGGER trg_tenant_domains_updated_at
BEFORE UPDATE ON tenant_domains
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE tenant_identity_policies (
  tenant_id UUID PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
  sso_enforcement TEXT NOT NULL DEFAULT 'optional' CHECK (sso_enforcement IN ('optional', 'required')),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  recovery_user_id UUID REFERENCES users(id) ON DELETE RESTRICT,
  enforcement_set_at TIMESTAMPTZ,
  enforcement_set_by UUID REFERENCES users(id) ON DELETE RESTRICT,
  updated_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (
    (sso_enforcement = 'optional' AND recovery_user_id IS NULL AND enforcement_set_at IS NULL AND enforcement_set_by IS NULL)
    OR
    (sso_enforcement = 'required' AND recovery_user_id IS NOT NULL AND enforcement_set_at IS NOT NULL AND enforcement_set_by IS NOT NULL)
  )
);

CREATE TRIGGER trg_tenant_identity_policies_updated_at
BEFORE UPDATE ON tenant_identity_policies
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE login_sessions
  ADD COLUMN auth_method TEXT NOT NULL DEFAULT 'local'
    CHECK (auth_method IN ('local', 'sso')),
  ADD COLUMN identity_connection_id UUID REFERENCES identity_connections(id) ON DELETE SET NULL,
  ADD CONSTRAINT ck_login_sessions_auth_method CHECK (
    (auth_method = 'local' AND identity_connection_id IS NULL)
    OR (auth_method = 'sso' AND identity_connection_id IS NOT NULL)
  );

CREATE INDEX idx_login_sessions_identity_connection_active
  ON login_sessions (identity_connection_id, user_id, id)
  WHERE revoked_at IS NULL AND identity_connection_id IS NOT NULL;
