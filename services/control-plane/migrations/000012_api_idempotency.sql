CREATE TABLE IF NOT EXISTS api_idempotency_keys (
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  actor_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  idempotency_key TEXT NOT NULL,
  operation TEXT NOT NULL,
  request_hash CHAR(64) NOT NULL,
  status_code INTEGER NOT NULL DEFAULT 0,
  response JSONB NOT NULL DEFAULT '{}'::jsonb,
  completed_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (tenant_id, actor_id, idempotency_key),
  CHECK (length(idempotency_key) BETWEEN 1 AND 200),
  CHECK (length(operation) BETWEEN 1 AND 120),
  CHECK (request_hash ~ '^[0-9a-f]{64}$'),
  CHECK (
    (completed_at IS NULL AND status_code = 0) OR
    (completed_at IS NOT NULL AND status_code BETWEEN 200 AND 299)
  )
);
