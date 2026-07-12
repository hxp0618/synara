CREATE TABLE IF NOT EXISTS control_plane_schema_migrations (
  version BIGINT PRIMARY KEY,
  name TEXT NOT NULL,
  checksum TEXT NOT NULL,
  applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE OR REPLACE FUNCTION set_updated_at()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  NEW.updated_at = now();
  RETURN NEW;
END;
$$;

CREATE TABLE IF NOT EXISTS users (
  id UUID PRIMARY KEY,
  email TEXT NOT NULL,
  display_name TEXT NOT NULL,
  avatar_url TEXT,
  status TEXT NOT NULL DEFAULT 'active'
    CHECK (status IN ('active', 'invited', 'suspended')),
  email_verified_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at TIMESTAMPTZ,
  CHECK (email = lower(btrim(email))),
  CHECK (length(email) BETWEEN 3 AND 320),
  CHECK (length(btrim(display_name)) BETWEEN 1 AND 160)
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_users_active_email
  ON users (lower(email))
  WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS user_identities (
  id UUID PRIMARY KEY,
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider TEXT NOT NULL,
  issuer TEXT NOT NULL,
  subject TEXT NOT NULL,
  profile JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_login_at TIMESTAMPTZ,
  UNIQUE (issuer, subject),
  CHECK (length(provider) BETWEEN 1 AND 80),
  CHECK (length(issuer) BETWEEN 1 AND 500),
  CHECK (length(subject) BETWEEN 1 AND 500)
);

CREATE TABLE IF NOT EXISTS tenants (
  id UUID PRIMARY KEY,
  slug TEXT NOT NULL,
  name TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active'
    CHECK (status IN ('active', 'suspended', 'deleting')),
  plan_code TEXT NOT NULL DEFAULT 'free',
  region TEXT NOT NULL DEFAULT 'default',
  settings JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_by UUID NOT NULL REFERENCES users(id),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  deleted_at TIMESTAMPTZ,
  CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$'),
  CHECK (length(btrim(name)) BETWEEN 1 AND 160)
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_tenants_active_slug
  ON tenants (lower(slug))
  WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS tenant_memberships (
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  role TEXT NOT NULL
    CHECK (role IN ('owner', 'admin', 'security_admin', 'billing_admin', 'auditor', 'member')),
  status TEXT NOT NULL DEFAULT 'active'
    CHECK (status IN ('invited', 'active', 'suspended')),
  invited_by UUID REFERENCES users(id),
  joined_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, user_id),
  CHECK ((status = 'active' AND joined_at IS NOT NULL) OR status <> 'active')
);

CREATE INDEX IF NOT EXISTS idx_tenant_memberships_user_status
  ON tenant_memberships (user_id, status, tenant_id);

CREATE TABLE IF NOT EXISTS organizations (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  parent_organization_id UUID,
  slug TEXT NOT NULL,
  name TEXT NOT NULL,
  kind TEXT NOT NULL CHECK (kind IN ('root', 'team', 'department', 'personal')),
  status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
  settings JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_by UUID NOT NULL REFERENCES users(id),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  archived_at TIMESTAMPTZ,
  UNIQUE (tenant_id, slug),
  UNIQUE (tenant_id, id),
  FOREIGN KEY (tenant_id, parent_organization_id)
    REFERENCES organizations(tenant_id, id)
    DEFERRABLE INITIALLY DEFERRED,
  CHECK (slug ~ '^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$'),
  CHECK (length(btrim(name)) BETWEEN 1 AND 160),
  CHECK ((kind = 'root' AND parent_organization_id IS NULL) OR kind <> 'root')
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_organizations_active_root
  ON organizations (tenant_id)
  WHERE kind = 'root' AND archived_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_organizations_tenant_status
  ON organizations (tenant_id, status, created_at, id);

CREATE TABLE IF NOT EXISTS organization_memberships (
  tenant_id UUID NOT NULL,
  organization_id UUID NOT NULL,
  user_id UUID NOT NULL,
  role TEXT NOT NULL CHECK (role IN ('owner', 'admin', 'agent_operator', 'member', 'viewer')),
  status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended')),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (organization_id, user_id),
  FOREIGN KEY (tenant_id, organization_id)
    REFERENCES organizations(tenant_id, id) ON DELETE CASCADE,
  FOREIGN KEY (tenant_id, user_id)
    REFERENCES tenant_memberships(tenant_id, user_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_organization_memberships_tenant_user
  ON organization_memberships (tenant_id, user_id, status);

CREATE TABLE IF NOT EXISTS login_sessions (
  id UUID PRIMARY KEY,
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  active_tenant_id UUID REFERENCES tenants(id) ON DELETE SET NULL,
  refresh_token_hash BYTEA NOT NULL UNIQUE,
  ip_address INET,
  user_agent TEXT,
  expires_at TIMESTAMPTZ NOT NULL,
  last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (expires_at > created_at)
);

CREATE INDEX IF NOT EXISTS idx_login_sessions_user_active
  ON login_sessions (user_id, expires_at DESC)
  WHERE revoked_at IS NULL;

CREATE TABLE IF NOT EXISTS tenant_invitations (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  email TEXT NOT NULL,
  role TEXT NOT NULL
    CHECK (role IN ('owner', 'admin', 'security_admin', 'billing_admin', 'auditor', 'member')),
  token_hash BYTEA NOT NULL UNIQUE,
  invited_by UUID NOT NULL REFERENCES users(id),
  expires_at TIMESTAMPTZ NOT NULL,
  accepted_by UUID REFERENCES users(id),
  accepted_at TIMESTAMPTZ,
  revoked_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (email = lower(btrim(email))),
  CHECK (expires_at > created_at),
  CHECK ((accepted_at IS NULL AND accepted_by IS NULL) OR
         (accepted_at IS NOT NULL AND accepted_by IS NOT NULL))
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_tenant_invitations_pending_email
  ON tenant_invitations (tenant_id, lower(email))
  WHERE accepted_at IS NULL AND revoked_at IS NULL;

CREATE TABLE IF NOT EXISTS audit_logs (
  event_id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  actor_type TEXT NOT NULL CHECK (actor_type IN ('user', 'service_account', 'worker', 'system')),
  actor_id UUID,
  action TEXT NOT NULL,
  resource_type TEXT NOT NULL,
  resource_id UUID,
  organization_id UUID,
  request_id TEXT NOT NULL,
  ip_address INET,
  metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
  occurred_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  FOREIGN KEY (tenant_id, organization_id)
    REFERENCES organizations(tenant_id, id) ON DELETE RESTRICT,
  CHECK (length(action) BETWEEN 1 AND 160),
  CHECK (length(resource_type) BETWEEN 1 AND 120),
  CHECK (length(request_id) BETWEEN 1 AND 160)
);

CREATE INDEX IF NOT EXISTS idx_audit_logs_tenant_occurred
  ON audit_logs (tenant_id, occurred_at DESC, event_id);

CREATE INDEX IF NOT EXISTS idx_audit_logs_tenant_resource
  ON audit_logs (tenant_id, resource_type, resource_id, occurred_at DESC);

CREATE TABLE IF NOT EXISTS outbox_messages (
  id UUID PRIMARY KEY,
  tenant_id UUID REFERENCES tenants(id) ON DELETE CASCADE,
  topic TEXT NOT NULL,
  message_key TEXT NOT NULL,
  payload JSONB NOT NULL,
  headers JSONB NOT NULL DEFAULT '{}'::jsonb,
  attempts INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
  available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  published_at TIMESTAMPTZ,
  last_error TEXT,
  UNIQUE (topic, message_key)
);

CREATE INDEX IF NOT EXISTS idx_outbox_messages_pending
  ON outbox_messages (available_at, created_at, id)
  WHERE published_at IS NULL;

CREATE OR REPLACE FUNCTION require_active_tenant_membership()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM tenant_memberships tm
    WHERE tm.tenant_id = NEW.tenant_id
      AND tm.user_id = NEW.user_id
      AND tm.status = 'active'
  ) THEN
    RAISE EXCEPTION 'organization membership requires an active tenant membership'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_require_active_tenant_membership ON organization_memberships;
CREATE TRIGGER trg_require_active_tenant_membership
BEFORE INSERT OR UPDATE OF tenant_id, user_id, status ON organization_memberships
FOR EACH ROW
WHEN (NEW.status = 'active')
EXECUTE FUNCTION require_active_tenant_membership();

CREATE OR REPLACE FUNCTION protect_active_organization_memberships()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  target_tenant_id UUID := OLD.tenant_id;
  target_user_id UUID := OLD.user_id;
BEGIN
  IF OLD.status = 'active' AND (TG_OP = 'DELETE' OR NEW.status <> 'active') THEN
    IF EXISTS (
      SELECT 1
      FROM organization_memberships om
      WHERE om.tenant_id = target_tenant_id
        AND om.user_id = target_user_id
        AND om.status = 'active'
    ) THEN
      RAISE EXCEPTION 'remove or suspend active organization memberships first'
        USING ERRCODE = '23514';
    END IF;
  END IF;
  IF TG_OP = 'DELETE' THEN
    RETURN OLD;
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_protect_active_organization_memberships ON tenant_memberships;
CREATE TRIGGER trg_protect_active_organization_memberships
BEFORE DELETE OR UPDATE OF status ON tenant_memberships
FOR EACH ROW
EXECUTE FUNCTION protect_active_organization_memberships();

CREATE OR REPLACE FUNCTION assert_tenant_has_owner(target_tenant_id UUID)
RETURNS VOID
LANGUAGE plpgsql
AS $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM tenants t
    WHERE t.id = target_tenant_id AND t.deleted_at IS NULL
  ) AND NOT EXISTS (
    SELECT 1
    FROM tenant_memberships tm
    WHERE tm.tenant_id = target_tenant_id
      AND tm.role = 'owner'
      AND tm.status = 'active'
  ) THEN
    RAISE EXCEPTION 'tenant % must retain at least one active owner', target_tenant_id
      USING ERRCODE = '23514';
  END IF;

  RETURN;
END;
$$;

CREATE OR REPLACE FUNCTION enforce_tenant_row_has_owner()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  PERFORM assert_tenant_has_owner(COALESCE(NEW.id, OLD.id));

  IF TG_OP = 'DELETE' THEN
    RETURN OLD;
  END IF;
  RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION enforce_membership_change_retains_owner()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  PERFORM assert_tenant_has_owner(COALESCE(NEW.tenant_id, OLD.tenant_id));

  IF TG_OP = 'DELETE' THEN
    RETURN OLD;
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_tenant_insert_requires_owner ON tenants;
CREATE CONSTRAINT TRIGGER trg_tenant_insert_requires_owner
AFTER INSERT OR UPDATE OF deleted_at ON tenants
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW
EXECUTE FUNCTION enforce_tenant_row_has_owner();

DROP TRIGGER IF EXISTS trg_membership_change_retains_owner ON tenant_memberships;
CREATE CONSTRAINT TRIGGER trg_membership_change_retains_owner
AFTER INSERT OR UPDATE OR DELETE ON tenant_memberships
DEFERRABLE INITIALLY DEFERRED
FOR EACH ROW
EXECUTE FUNCTION enforce_membership_change_retains_owner();

CREATE OR REPLACE FUNCTION reject_audit_log_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'audit_logs is append-only' USING ERRCODE = '55000';
END;
$$;

DROP TRIGGER IF EXISTS trg_audit_logs_append_only ON audit_logs;
CREATE TRIGGER trg_audit_logs_append_only
BEFORE UPDATE OR DELETE ON audit_logs
FOR EACH ROW
EXECUTE FUNCTION reject_audit_log_mutation();

DROP TRIGGER IF EXISTS trg_users_updated_at ON users;
CREATE TRIGGER trg_users_updated_at BEFORE UPDATE ON users
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_tenants_updated_at ON tenants;
CREATE TRIGGER trg_tenants_updated_at BEFORE UPDATE ON tenants
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_tenant_memberships_updated_at ON tenant_memberships;
CREATE TRIGGER trg_tenant_memberships_updated_at BEFORE UPDATE ON tenant_memberships
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_organizations_updated_at ON organizations;
CREATE TRIGGER trg_organizations_updated_at BEFORE UPDATE ON organizations
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_organization_memberships_updated_at ON organization_memberships;
CREATE TRIGGER trg_organization_memberships_updated_at BEFORE UPDATE ON organization_memberships
FOR EACH ROW EXECUTE FUNCTION set_updated_at();
