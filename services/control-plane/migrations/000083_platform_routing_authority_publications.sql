CREATE TABLE platform_routing_publications (
  publisher_identity TEXT NOT NULL,
  nonce UUID NOT NULL,
  key_id TEXT NOT NULL,
  public_key_sha256 TEXT NOT NULL,
  execution_target_id UUID NOT NULL REFERENCES execution_targets(id) ON DELETE RESTRICT,
  request_sha256 TEXT NOT NULL,
  observed_at TIMESTAMPTZ NOT NULL,
  response JSONB NOT NULL CHECK (jsonb_typeof(response) = 'object'),
  response_sha256 TEXT NOT NULL,
  received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (publisher_identity, nonce),
  CHECK (length(btrim(publisher_identity)) BETWEEN 1 AND 160),
  CHECK (length(btrim(key_id)) BETWEEN 1 AND 160),
  CHECK (public_key_sha256 ~ '^[0-9a-f]{64}$'),
  CHECK (request_sha256 ~ '^[0-9a-f]{64}$'),
  CHECK (response_sha256 ~ '^[0-9a-f]{64}$')
);

CREATE INDEX idx_platform_routing_publications_target_received
  ON platform_routing_publications (execution_target_id, received_at DESC, publisher_identity, nonce);

CREATE OR REPLACE FUNCTION enforce_platform_routing_publication_immutable()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    RAISE EXCEPTION 'Platform routing publication receipts cannot be deleted'
      USING ERRCODE = '23514';
  END IF;
  RAISE EXCEPTION 'Platform routing publication receipts are immutable'
    USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER trg_platform_routing_publications_immutable
BEFORE UPDATE OR DELETE ON platform_routing_publications
FOR EACH ROW EXECUTE FUNCTION enforce_platform_routing_publication_immutable();

COMMENT ON TABLE platform_routing_publications IS
  'Immutable Ed25519-authenticated platform/integration routing-authority publication receipts.';
