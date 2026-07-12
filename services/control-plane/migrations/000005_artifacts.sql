CREATE TABLE IF NOT EXISTS artifacts (
  id UUID PRIMARY KEY,
  tenant_id UUID NOT NULL,
  organization_id UUID NOT NULL,
  project_id UUID NOT NULL,
  session_id UUID NOT NULL,
  execution_id UUID,
  kind TEXT NOT NULL
    CHECK (kind IN ('attachment', 'generated_file', 'terminal_log', 'workspace_snapshot', 'checkpoint')),
  status TEXT NOT NULL DEFAULT 'pending'
    CHECK (status IN ('pending', 'ready', 'deleting', 'deleted', 'failed')),
  original_name TEXT,
  bucket TEXT NOT NULL,
  object_key TEXT NOT NULL,
  upload_object_key TEXT,
  object_version TEXT,
  content_type TEXT,
  size_bytes BIGINT,
  sha256 TEXT,
  encryption_key_id TEXT,
  created_by_type TEXT NOT NULL
    CHECK (created_by_type IN ('user', 'service_account', 'worker', 'system')),
  created_by_id UUID NOT NULL,
  upload_token_hash BYTEA,
  upload_expires_at TIMESTAMPTZ,
  ready_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at TIMESTAMPTZ,
  deleted_at TIMESTAMPTZ,
  UNIQUE (tenant_id, id),
  UNIQUE (bucket, object_key),
  FOREIGN KEY (tenant_id, organization_id, project_id)
    REFERENCES projects(tenant_id, organization_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (tenant_id, project_id, session_id)
    REFERENCES agent_sessions(tenant_id, project_id, id) ON DELETE RESTRICT,
  FOREIGN KEY (tenant_id, execution_id)
    REFERENCES agent_executions(tenant_id, id) ON DELETE RESTRICT,
  CHECK (length(kind) BETWEEN 1 AND 64),
  CHECK (length(bucket) BETWEEN 1 AND 255),
  CHECK (length(object_key) BETWEEN 1 AND 2048),
  CHECK (upload_object_key IS NULL OR length(upload_object_key) BETWEEN 1 AND 2048),
  CHECK (original_name IS NULL OR length(original_name) BETWEEN 1 AND 512),
  CHECK (object_version IS NULL OR length(object_version) BETWEEN 1 AND 1024),
  CHECK (content_type IS NULL OR length(content_type) BETWEEN 1 AND 255),
  CHECK (size_bytes IS NULL OR size_bytes >= 0),
  CHECK (sha256 IS NULL OR sha256 ~ '^[0-9a-f]{64}$'),
  CHECK (encryption_key_id IS NULL OR length(encryption_key_id) BETWEEN 1 AND 1024),
  CHECK (upload_expires_at IS NULL OR upload_expires_at > created_at),
  CHECK (ready_at IS NULL OR ready_at >= created_at),
  CHECK (expires_at IS NULL OR expires_at > created_at),
  CHECK ((execution_id IS NOT NULL) OR created_by_type <> 'worker'),
  CHECK ((status = 'ready' AND ready_at IS NOT NULL AND content_type IS NOT NULL
          AND size_bytes IS NOT NULL AND sha256 IS NOT NULL) OR status <> 'ready'),
  CHECK ((status = 'deleted' AND deleted_at IS NOT NULL) OR status <> 'deleted')
);

CREATE INDEX IF NOT EXISTS idx_artifacts_session_created
  ON artifacts (tenant_id, session_id, created_at DESC, id)
  WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_artifacts_execution_created
  ON artifacts (tenant_id, execution_id, created_at DESC, id)
  WHERE execution_id IS NOT NULL AND deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_artifacts_expiry
  ON artifacts (expires_at, id)
  WHERE expires_at IS NOT NULL AND deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_artifacts_pending_expiry
  ON artifacts (upload_expires_at, id)
  WHERE status = 'pending';

CREATE TABLE IF NOT EXISTS artifact_payload_migrations (
  artifact_id UUID NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
  destination TEXT NOT NULL,
  source_sha256 TEXT NOT NULL CHECK (source_sha256 ~ '^[0-9a-f]{64}$'),
  object_version TEXT,
  migrated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (artifact_id, destination),
  CHECK (length(destination) BETWEEN 1 AND 2304),
  CHECK (object_version IS NULL OR length(object_version) BETWEEN 1 AND 1024)
);

CREATE TABLE IF NOT EXISTS artifact_access_tokens (
  id UUID PRIMARY KEY,
  artifact_id UUID NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
  token_hash BYTEA NOT NULL UNIQUE,
  purpose TEXT NOT NULL CHECK (purpose = 'download'),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  expires_at TIMESTAMPTZ NOT NULL,
  CHECK (expires_at > created_at)
);

CREATE INDEX IF NOT EXISTS idx_artifact_access_tokens_expiry
  ON artifact_access_tokens (expires_at, id);

DROP TRIGGER IF EXISTS trg_artifacts_tenant_immutable ON artifacts;
CREATE TRIGGER trg_artifacts_tenant_immutable
BEFORE UPDATE OF tenant_id ON artifacts
FOR EACH ROW EXECUTE FUNCTION reject_tenant_ownership_change();
