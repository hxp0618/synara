CREATE TABLE tenant_support_policies (
  tenant_id UUID PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
  support_access_enabled BOOLEAN NOT NULL DEFAULT false,
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  reason TEXT NOT NULL,
  updated_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (length(btrim(reason)) BETWEEN 10 AND 1000)
);

CREATE TRIGGER trg_tenant_support_policies_updated_at
BEFORE UPDATE ON tenant_support_policies
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE support_access_grants (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  requester_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  status TEXT NOT NULL CHECK (status IN ('pending', 'active', 'denied', 'revoked', 'expired')),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  reason TEXT NOT NULL,
  requested_duration_seconds INTEGER NOT NULL CHECK (requested_duration_seconds BETWEEN 300 AND 14400),
  requested_at TIMESTAMPTZ NOT NULL,
  decided_by UUID REFERENCES users(id) ON DELETE RESTRICT,
  decision_reason TEXT,
  decided_at TIMESTAMPTZ,
  expires_at TIMESTAMPTZ,
  revoked_by UUID REFERENCES users(id) ON DELETE RESTRICT,
  revocation_reason TEXT,
  revoked_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (length(btrim(reason)) BETWEEN 10 AND 1000),
  CHECK (decision_reason IS NULL OR length(btrim(decision_reason)) BETWEEN 10 AND 1000),
  CHECK (revocation_reason IS NULL OR length(btrim(revocation_reason)) BETWEEN 10 AND 1000),
  CHECK (
    (status = 'pending' AND decided_by IS NULL AND decision_reason IS NULL AND decided_at IS NULL
      AND expires_at IS NULL AND revoked_by IS NULL AND revocation_reason IS NULL AND revoked_at IS NULL)
    OR
    (status = 'active' AND decided_by IS NOT NULL AND decision_reason IS NOT NULL AND decided_at IS NOT NULL
      AND expires_at > decided_at AND revoked_by IS NULL AND revocation_reason IS NULL AND revoked_at IS NULL)
    OR
    (status = 'denied' AND decided_by IS NOT NULL AND decision_reason IS NOT NULL AND decided_at IS NOT NULL
      AND expires_at IS NULL AND revoked_by IS NULL AND revocation_reason IS NULL AND revoked_at IS NULL)
    OR
    (status = 'revoked' AND decided_by IS NOT NULL AND decision_reason IS NOT NULL
      AND decided_at IS NOT NULL AND expires_at > decided_at AND revoked_by IS NOT NULL
      AND revocation_reason IS NOT NULL AND revoked_at IS NOT NULL)
    OR
    (status = 'expired' AND decided_by IS NOT NULL AND decision_reason IS NOT NULL
      AND decided_at IS NOT NULL AND expires_at > decided_at AND revoked_by IS NULL
      AND revocation_reason IS NULL AND revoked_at IS NULL)
  ),
  CHECK (decided_by IS NULL OR decided_by <> requester_user_id),
  CHECK (revoked_at IS NULL OR revoked_at >= decided_at)
);

CREATE UNIQUE INDEX uq_support_access_grants_pending
  ON support_access_grants (tenant_id, requester_user_id)
  WHERE status = 'pending';

CREATE UNIQUE INDEX uq_support_access_grants_active
  ON support_access_grants (tenant_id, requester_user_id)
  WHERE status = 'active';

CREATE INDEX idx_support_access_grants_operator_queue
  ON support_access_grants (status, requested_at, id);

CREATE INDEX idx_support_access_grants_authorization
  ON support_access_grants (requester_user_id, tenant_id, status, expires_at, id);

CREATE TRIGGER trg_support_access_grants_updated_at
BEFORE UPDATE ON support_access_grants
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE support_access_grants IS
  'Four-eyes, time-bounded support access. Active grants authorize only the internal support_readonly permission set.';
