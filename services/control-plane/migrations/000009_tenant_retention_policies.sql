CREATE TABLE IF NOT EXISTS tenant_retention_policies (
  tenant_id UUID PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
  session_archive_after_days INTEGER,
  artifact_delete_after_days INTEGER,
  updated_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (session_archive_after_days IS NULL OR session_archive_after_days BETWEEN 1 AND 36500),
  CHECK (artifact_delete_after_days IS NULL OR artifact_delete_after_days BETWEEN 1 AND 36500)
);

DROP TRIGGER IF EXISTS trg_tenant_retention_policies_updated_at ON tenant_retention_policies;
CREATE TRIGGER trg_tenant_retention_policies_updated_at BEFORE UPDATE ON tenant_retention_policies
FOR EACH ROW EXECUTE FUNCTION set_updated_at();
