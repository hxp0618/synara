CREATE TABLE IF NOT EXISTS tenant_quotas (
  tenant_id UUID PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
  max_concurrent_executions INTEGER CHECK (max_concurrent_executions IS NULL OR max_concurrent_executions > 0),
  max_artifact_bytes BIGINT CHECK (max_artifact_bytes IS NULL OR max_artifact_bytes > 0),
  updated_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

DROP TRIGGER IF EXISTS trg_tenant_quotas_updated_at ON tenant_quotas;
CREATE TRIGGER trg_tenant_quotas_updated_at BEFORE UPDATE ON tenant_quotas
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

DROP TRIGGER IF EXISTS trg_tenant_quotas_tenant_immutable ON tenant_quotas;
CREATE TRIGGER trg_tenant_quotas_tenant_immutable
BEFORE UPDATE OF tenant_id ON tenant_quotas
FOR EACH ROW EXECUTE FUNCTION reject_tenant_ownership_change();
