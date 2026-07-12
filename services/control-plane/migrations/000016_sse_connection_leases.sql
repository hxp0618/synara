CREATE TABLE IF NOT EXISTS sse_connection_leases (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  session_id UUID NOT NULL,
  instance_id TEXT NOT NULL,
  connected_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  renewed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at TIMESTAMPTZ NOT NULL,
  FOREIGN KEY (tenant_id, session_id)
    REFERENCES agent_sessions(tenant_id, id) ON DELETE CASCADE,
  CHECK (length(instance_id) BETWEEN 1 AND 160)
);

CREATE INDEX IF NOT EXISTS idx_sse_connection_leases_tenant_expiry
  ON sse_connection_leases (tenant_id, expires_at, id);

CREATE INDEX IF NOT EXISTS idx_sse_connection_leases_user_expiry
  ON sse_connection_leases (tenant_id, user_id, expires_at, id);

CREATE INDEX IF NOT EXISTS idx_sse_connection_leases_expiry
  ON sse_connection_leases (expires_at, id);
