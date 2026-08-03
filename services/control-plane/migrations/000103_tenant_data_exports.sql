CREATE TABLE tenant_data_exports (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  requested_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  schema_version TEXT NOT NULL,
  digest_sha256 TEXT NOT NULL,
  byte_count BIGINT NOT NULL CHECK (byte_count > 0),
  row_counts JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (tenant_id, id),
  CHECK (length(schema_version) BETWEEN 1 AND 80),
  CHECK (digest_sha256 ~ '^[0-9a-f]{64}$')
);

CREATE INDEX idx_tenant_data_exports_history
  ON tenant_data_exports (tenant_id, created_at DESC, id);

CREATE OR REPLACE FUNCTION reject_tenant_data_export_mutation()
RETURNS trigger AS $$
BEGIN
  RAISE EXCEPTION 'Tenant data export receipts are immutable';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_tenant_data_exports_update
BEFORE UPDATE ON tenant_data_exports
FOR EACH ROW EXECUTE FUNCTION reject_tenant_data_export_mutation();

CREATE TRIGGER trg_tenant_data_exports_delete
BEFORE DELETE ON tenant_data_exports
FOR EACH ROW EXECUTE FUNCTION reject_tenant_data_export_mutation();

COMMENT ON TABLE tenant_data_exports IS
  'Immutable digest receipts for administrator-generated, Tenant-scoped data export bundles.';
