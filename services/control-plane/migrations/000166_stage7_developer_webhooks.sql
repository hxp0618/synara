CREATE TABLE developer_webhook_endpoints (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  name TEXT NOT NULL,
  url TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'active',
  event_types JSONB NOT NULL DEFAULT '[]'::jsonb,
  secret_version BIGINT NOT NULL DEFAULT 1,
  encrypted_secret BYTEA NOT NULL,
  encrypted_data_key BYTEA NOT NULL,
  kms_provider TEXT NOT NULL,
  kms_key_id TEXT NOT NULL,
  created_by UUID NOT NULL REFERENCES users(id),
  last_delivered_at TIMESTAMPTZ,
  last_failed_at TIMESTAMPTZ,
  last_failure_summary TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  revoked_at TIMESTAMPTZ,
  CONSTRAINT uq_developer_webhook_endpoint_name UNIQUE (tenant_id, name),
  CONSTRAINT chk_developer_webhook_endpoint_status CHECK (status IN ('active', 'disabled', 'revoked')),
  CONSTRAINT chk_developer_webhook_endpoint_name CHECK (char_length(name) BETWEEN 1 AND 120),
  CONSTRAINT chk_developer_webhook_endpoint_url CHECK (char_length(url) BETWEEN 1 AND 2048),
  CONSTRAINT chk_developer_webhook_endpoint_secret_version CHECK (secret_version > 0),
  CONSTRAINT chk_developer_webhook_endpoint_event_types CHECK (
    jsonb_typeof(event_types) = 'array' AND jsonb_array_length(event_types) BETWEEN 1 AND 20
  ),
  CONSTRAINT chk_developer_webhook_endpoint_secret_envelope CHECK (
    octet_length(encrypted_secret) > 0 AND octet_length(encrypted_data_key) > 0
    AND char_length(kms_provider) > 0 AND char_length(kms_key_id) > 0
  ),
  CONSTRAINT chk_developer_webhook_endpoint_lifecycle CHECK (
    (status = 'revoked' AND revoked_at IS NOT NULL) OR
    (status IN ('active', 'disabled') AND revoked_at IS NULL)
  )
);

CREATE INDEX idx_developer_webhook_endpoints_active
  ON developer_webhook_endpoints (tenant_id, id)
  WHERE status = 'active';

CREATE TABLE developer_webhook_deliveries (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  endpoint_id UUID NOT NULL REFERENCES developer_webhook_endpoints(id) ON DELETE CASCADE,
  session_event_id UUID NOT NULL,
  event_type TEXT NOT NULL,
  session_id UUID NOT NULL,
  execution_id UUID,
  sequence BIGINT NOT NULL,
  outbox_message_id UUID NOT NULL REFERENCES outbox_messages(id) ON DELETE CASCADE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT uq_developer_webhook_delivery_event UNIQUE (endpoint_id, session_event_id),
  CONSTRAINT uq_developer_webhook_delivery_outbox UNIQUE (outbox_message_id),
  CONSTRAINT chk_developer_webhook_delivery_sequence CHECK (sequence > 0),
  CONSTRAINT chk_developer_webhook_delivery_type CHECK (char_length(event_type) BETWEEN 1 AND 160)
);

CREATE INDEX idx_developer_webhook_deliveries_endpoint
  ON developer_webhook_deliveries (tenant_id, endpoint_id, created_at DESC, id DESC);
CREATE INDEX idx_developer_webhook_deliveries_session
  ON developer_webhook_deliveries (tenant_id, session_id, sequence, endpoint_id);
