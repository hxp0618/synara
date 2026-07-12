CREATE TABLE IF NOT EXISTS provider_credentials (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  organization_id UUID,
  name TEXT NOT NULL,
  provider TEXT NOT NULL,
  credential_type TEXT NOT NULL,
  encrypted_payload BYTEA NOT NULL,
  encrypted_data_key BYTEA NOT NULL,
  kms_provider TEXT NOT NULL,
  kms_key_id TEXT NOT NULL,
  version INTEGER NOT NULL DEFAULT 1 CHECK (version > 0),
  created_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  updated_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at TIMESTAMPTZ,
  revoked_at TIMESTAMPTZ,
  revoked_by UUID REFERENCES users(id) ON DELETE RESTRICT,
  UNIQUE (tenant_id, id),
  FOREIGN KEY (tenant_id, organization_id)
    REFERENCES organizations(tenant_id, id) ON DELETE RESTRICT,
  CHECK (length(btrim(name)) BETWEEN 1 AND 160),
  CHECK (provider ~ '^[a-z0-9][a-z0-9._-]{0,79}$'),
  CHECK (credential_type ~ '^[a-z0-9][a-z0-9._-]{0,79}$'),
  CHECK (length(kms_provider) BETWEEN 1 AND 40),
  CHECK (length(kms_key_id) BETWEEN 1 AND 1024),
  CHECK (octet_length(encrypted_payload) >= 16),
  CHECK (octet_length(encrypted_data_key) >= 16),
  CHECK (expires_at IS NULL OR expires_at > created_at),
  CHECK ((revoked_at IS NULL AND revoked_by IS NULL) OR
         (revoked_at IS NOT NULL AND revoked_by IS NOT NULL))
);

CREATE INDEX IF NOT EXISTS idx_provider_credentials_tenant_provider
  ON provider_credentials (tenant_id, provider, created_at DESC, id)
  WHERE revoked_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_provider_credentials_organization
  ON provider_credentials (tenant_id, organization_id, provider, created_at DESC, id)
  WHERE organization_id IS NOT NULL AND revoked_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_provider_credentials_expiry
  ON provider_credentials (expires_at, id)
  WHERE expires_at IS NOT NULL AND revoked_at IS NULL;

DROP TRIGGER IF EXISTS trg_provider_credentials_updated_at ON provider_credentials;
CREATE TRIGGER trg_provider_credentials_updated_at BEFORE UPDATE ON provider_credentials
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_provider_credentials_tenant_immutable ON provider_credentials;
CREATE TRIGGER trg_provider_credentials_tenant_immutable
BEFORE UPDATE OF tenant_id ON provider_credentials
FOR EACH ROW EXECUTE FUNCTION reject_tenant_ownership_change();
