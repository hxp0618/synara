CREATE TABLE desktop_devices (
  id UUID PRIMARY KEY,
  control_plane_origin TEXT NOT NULL,
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  default_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  default_organization_id UUID,
  public_key BYTEA NOT NULL,
  public_key_sha256 BYTEA NOT NULL,
  platform TEXT NOT NULL,
  app_version TEXT NOT NULL,
  device_label TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active'
    CHECK (status IN ('active', 'revoked')),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_by UUID REFERENCES users(id) ON DELETE SET NULL,
  revocation_reason TEXT,
  revoked_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  FOREIGN KEY (default_tenant_id, default_organization_id)
    REFERENCES organizations(tenant_id, id) ON DELETE RESTRICT,
  CHECK (octet_length(public_key) = 32),
  CHECK (octet_length(public_key_sha256) = 32),
  CHECK (length(control_plane_origin) BETWEEN 8 AND 2048),
  CHECK (platform IN ('darwin', 'win32', 'linux')),
  CHECK (length(app_version) BETWEEN 1 AND 80),
  CHECK (length(btrim(device_label)) BETWEEN 1 AND 160),
  CHECK (
    (status = 'active' AND revoked_by IS NULL AND revocation_reason IS NULL AND revoked_at IS NULL)
    OR
    (status = 'revoked' AND length(btrim(revocation_reason)) BETWEEN 10 AND 1000 AND revoked_at IS NOT NULL)
  )
);

CREATE UNIQUE INDEX uq_desktop_devices_subject_key
  ON desktop_devices (control_plane_origin, user_id, public_key_sha256);

CREATE INDEX idx_desktop_devices_subject_status
  ON desktop_devices (user_id, status, last_seen_at DESC, id);

CREATE INDEX idx_desktop_devices_tenant_status
  ON desktop_devices (default_tenant_id, status, last_seen_at DESC, id);

CREATE TRIGGER trg_desktop_devices_updated_at
BEFORE UPDATE ON desktop_devices
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE login_sessions
  DROP CONSTRAINT IF EXISTS login_sessions_auth_method_check,
  DROP CONSTRAINT IF EXISTS ck_login_sessions_auth_method;

ALTER TABLE login_sessions
  ADD COLUMN audience TEXT NOT NULL DEFAULT 'web'
    CHECK (audience IN ('web', 'desktop')),
  ADD COLUMN desktop_device_id UUID REFERENCES desktop_devices(id) ON DELETE RESTRICT,
  ADD COLUMN credential_family_id UUID,
  ADD COLUMN rotated_from_session_id UUID REFERENCES login_sessions(id) ON DELETE RESTRICT,
  ADD COLUMN rotated_to_session_id UUID REFERENCES login_sessions(id) ON DELETE RESTRICT,
  ADD COLUMN rotated_at TIMESTAMPTZ,
  ADD COLUMN replay_detected_at TIMESTAMPTZ,
  ADD CONSTRAINT ck_login_sessions_auth_method CHECK (
    (auth_method = 'local' AND identity_connection_id IS NULL AND audience = 'web')
    OR
    (auth_method = 'sso' AND identity_connection_id IS NOT NULL AND audience = 'web')
    OR
    (auth_method = 'desktop' AND identity_connection_id IS NULL AND audience = 'desktop')
  ),
  ADD CONSTRAINT ck_login_sessions_desktop_shape CHECK (
    (
      audience = 'web'
      AND desktop_device_id IS NULL
      AND credential_family_id IS NULL
      AND rotated_from_session_id IS NULL
      AND rotated_to_session_id IS NULL
      AND rotated_at IS NULL
      AND replay_detected_at IS NULL
    )
    OR
    (
      audience = 'desktop'
      AND desktop_device_id IS NOT NULL
      AND credential_family_id IS NOT NULL
      AND ((rotated_to_session_id IS NULL AND rotated_at IS NULL) OR
           (rotated_to_session_id IS NOT NULL AND rotated_at IS NOT NULL AND revoked_at IS NOT NULL))
      AND (replay_detected_at IS NULL OR revoked_at IS NOT NULL)
    )
  );

CREATE UNIQUE INDEX uq_login_sessions_rotated_from
  ON login_sessions (rotated_from_session_id)
  WHERE rotated_from_session_id IS NOT NULL;

CREATE UNIQUE INDEX uq_login_sessions_rotated_to
  ON login_sessions (rotated_to_session_id)
  WHERE rotated_to_session_id IS NOT NULL;

CREATE INDEX idx_login_sessions_desktop_family_active
  ON login_sessions (credential_family_id, expires_at DESC, id)
  WHERE audience = 'desktop' AND revoked_at IS NULL;

CREATE INDEX idx_login_sessions_desktop_device_active
  ON login_sessions (desktop_device_id, expires_at DESC, id)
  WHERE audience = 'desktop' AND revoked_at IS NULL;

CREATE TABLE desktop_enrollments (
  id UUID PRIMARY KEY,
  secret_hash BYTEA NOT NULL UNIQUE,
  status TEXT NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending', 'redeemed', 'expired', 'revoked')),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  mode TEXT NOT NULL CHECK (mode IN ('connect_existing', 'provisioned_then_connect')),
  authority TEXT NOT NULL CHECK (authority = 'self'),
  control_plane_origin TEXT NOT NULL,
  issued_by_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  issued_by_session_id UUID NOT NULL REFERENCES login_sessions(id) ON DELETE RESTRICT,
  subject_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  organization_id UUID,
  membership_version_snapshot TEXT NOT NULL,
  reason TEXT NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  opened_at TIMESTAMPTZ,
  redeemed_at TIMESTAMPTZ,
  redeemed_device_id UUID REFERENCES desktop_devices(id) ON DELETE RESTRICT,
  redeemed_nonce_hash BYTEA,
  revoked_by UUID REFERENCES users(id) ON DELETE SET NULL,
  revoked_at TIMESTAMPTZ,
  terminal_reason TEXT,
  failed_attempts INTEGER NOT NULL DEFAULT 0 CHECK (failed_attempts >= 0),
  last_failure_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  FOREIGN KEY (tenant_id, organization_id)
    REFERENCES organizations(tenant_id, id) ON DELETE RESTRICT,
  CHECK (octet_length(secret_hash) = 32),
  CHECK (length(control_plane_origin) BETWEEN 8 AND 2048),
  CHECK (length(membership_version_snapshot) BETWEEN 1 AND 200),
  CHECK (length(btrim(reason)) BETWEEN 10 AND 1000),
  CHECK (expires_at > created_at),
  CHECK (redeemed_nonce_hash IS NULL OR octet_length(redeemed_nonce_hash) = 32),
  CHECK (
    (status = 'pending' AND redeemed_at IS NULL AND redeemed_device_id IS NULL
      AND redeemed_nonce_hash IS NULL AND revoked_by IS NULL AND revoked_at IS NULL
      AND terminal_reason IS NULL)
    OR
    (status = 'redeemed' AND redeemed_at IS NOT NULL AND redeemed_device_id IS NOT NULL
      AND redeemed_nonce_hash IS NOT NULL AND revoked_by IS NULL AND revoked_at IS NULL
      AND terminal_reason IS NULL)
    OR
    (status = 'expired' AND redeemed_at IS NULL AND redeemed_device_id IS NULL
      AND redeemed_nonce_hash IS NULL AND revoked_by IS NULL AND revoked_at IS NULL
      AND terminal_reason = 'expired')
    OR
    (status = 'revoked' AND redeemed_at IS NULL AND redeemed_device_id IS NULL
      AND redeemed_nonce_hash IS NULL AND revoked_at IS NOT NULL
      AND length(btrim(terminal_reason)) BETWEEN 1 AND 160)
  )
);

CREATE INDEX idx_desktop_enrollments_pending_expiry
  ON desktop_enrollments (expires_at, id)
  WHERE status = 'pending';

CREATE INDEX idx_desktop_enrollments_subject
  ON desktop_enrollments (subject_user_id, tenant_id, created_at DESC, id);

CREATE INDEX idx_desktop_enrollments_platform
  ON desktop_enrollments (tenant_id, status, created_at DESC, id);

CREATE TRIGGER trg_desktop_enrollments_updated_at
BEFORE UPDATE ON desktop_enrollments
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE desktop_enrollments IS
  'Short-lived, single-use Desktop connection authority. Raw handles are never persisted.';

COMMENT ON COLUMN login_sessions.audience IS
  'Credential audience. Desktop Bearer sessions never authorize Platform or Support operations.';
