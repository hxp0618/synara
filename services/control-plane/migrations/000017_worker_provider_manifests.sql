CREATE TABLE worker_manifests (
  id UUID PRIMARY KEY,
  manifest_hash TEXT NOT NULL UNIQUE CHECK (manifest_hash ~ '^[0-9a-f]{64}$'),
  worker_build_version TEXT NOT NULL,
  worker_build_git_sha TEXT,
  worker_protocol_minimum INTEGER NOT NULL CHECK (worker_protocol_minimum > 0),
  worker_protocol_maximum INTEGER NOT NULL CHECK (worker_protocol_maximum >= worker_protocol_minimum),
  runtime_event_minimum INTEGER NOT NULL CHECK (runtime_event_minimum > 0),
  runtime_event_maximum INTEGER NOT NULL CHECK (runtime_event_maximum >= runtime_event_minimum),
  operating_system TEXT NOT NULL,
  architecture TEXT NOT NULL,
  image_digest TEXT,
  feature_flags JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK (length(worker_build_version) BETWEEN 1 AND 160),
  CHECK (worker_build_git_sha IS NULL OR worker_build_git_sha ~ '^[0-9a-f]{7,64}$'),
  CHECK (length(operating_system) BETWEEN 1 AND 80),
  CHECK (length(architecture) BETWEEN 1 AND 80),
  CHECK (image_digest IS NULL OR length(image_digest) BETWEEN 1 AND 512)
);

CREATE TABLE worker_provider_manifests (
  worker_manifest_id UUID NOT NULL REFERENCES worker_manifests(id) ON DELETE CASCADE,
  provider TEXT NOT NULL,
  support_tier TEXT NOT NULL
    CHECK (support_tier IN ('tier-1', 'tier-2', 'experimental', 'local-only')),
  compatibility_status TEXT NOT NULL
    CHECK (compatibility_status IN ('compatible', 'incompatible', 'unavailable', 'local-only')),
  provider_host_protocol_major INTEGER NOT NULL CHECK (provider_host_protocol_major > 0),
  provider_host_protocol_minor INTEGER NOT NULL CHECK (provider_host_protocol_minor >= 0),
  host_build_version TEXT NOT NULL,
  adapter_version TEXT NOT NULL,
  provider_cli_version TEXT,
  maximum_command_bytes INTEGER NOT NULL CHECK (maximum_command_bytes > 0),
  maximum_message_bytes INTEGER NOT NULL CHECK (maximum_message_bytes > 0),
  runtime_event_minimum INTEGER NOT NULL CHECK (runtime_event_minimum > 0),
  runtime_event_maximum INTEGER NOT NULL CHECK (runtime_event_maximum >= runtime_event_minimum),
  credential_delivery_modes JSONB NOT NULL DEFAULT '[]'::jsonb,
  resume_strategies JSONB NOT NULL DEFAULT '[]'::jsonb,
  capability_descriptor_hash TEXT NOT NULL
    CHECK (capability_descriptor_hash ~ '^[0-9a-f]{64}$'),
  capabilities JSONB NOT NULL DEFAULT '{}'::jsonb,
  incompatibility_code TEXT,
  incompatibility_message TEXT,
  checked_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (worker_manifest_id, provider),
  CHECK (length(provider) BETWEEN 1 AND 80),
  CHECK (length(host_build_version) BETWEEN 1 AND 160),
  CHECK (length(adapter_version) BETWEEN 1 AND 160),
  CHECK (provider_cli_version IS NULL OR length(provider_cli_version) BETWEEN 1 AND 200),
  CHECK (incompatibility_code IS NULL OR length(incompatibility_code) BETWEEN 1 AND 160),
  CHECK (incompatibility_message IS NULL OR length(incompatibility_message) <= 2000),
  CHECK (
    (compatibility_status = 'compatible' AND incompatibility_code IS NULL AND incompatibility_message IS NULL) OR
    compatibility_status <> 'compatible'
  )
);

ALTER TABLE worker_instances
  ADD COLUMN current_manifest_id UUID,
  ADD COLUMN compatibility_status TEXT NOT NULL DEFAULT 'unknown'
    CHECK (compatibility_status IN ('unknown', 'compatible', 'incompatible', 'revoked')),
  ADD COLUMN compatibility_reason TEXT,
  ADD COLUMN compatibility_checked_at TIMESTAMPTZ,
  ADD CONSTRAINT fk_worker_instances_current_manifest
    FOREIGN KEY (current_manifest_id) REFERENCES worker_manifests(id) ON DELETE RESTRICT,
  ADD CONSTRAINT chk_worker_instances_compatibility_reason
    CHECK (compatibility_reason IS NULL OR length(compatibility_reason) <= 2000),
  ADD CONSTRAINT chk_worker_instances_compatibility_timestamp
    CHECK (
      (compatibility_status = 'unknown' AND compatibility_checked_at IS NULL) OR
      (compatibility_status <> 'unknown' AND compatibility_checked_at IS NOT NULL)
    );

ALTER TABLE agent_executions
  ADD COLUMN provider TEXT;

UPDATE agent_executions AS execution
SET provider = session.provider
FROM agent_sessions AS session
WHERE session.tenant_id = execution.tenant_id
  AND session.id = execution.session_id
  AND execution.provider IS NULL;

ALTER TABLE agent_executions
  ADD COLUMN worker_manifest_id UUID,
  ADD CONSTRAINT fk_agent_executions_worker_manifest
    FOREIGN KEY (worker_manifest_id) REFERENCES worker_manifests(id) ON DELETE RESTRICT,
  ADD CONSTRAINT fk_agent_executions_provider_manifest
    FOREIGN KEY (worker_manifest_id, provider)
    REFERENCES worker_provider_manifests(worker_manifest_id, provider) ON DELETE RESTRICT,
  ADD CONSTRAINT chk_agent_executions_provider
    CHECK (provider IS NULL OR length(provider) BETWEEN 1 AND 80);

CREATE INDEX idx_worker_instances_compatibility
  ON worker_instances (execution_target_id, compatibility_status, status, last_heartbeat_at, id);

CREATE INDEX idx_worker_provider_manifests_compatibility
  ON worker_provider_manifests (provider, compatibility_status, support_tier, checked_at DESC, worker_manifest_id);

CREATE INDEX idx_agent_executions_worker_manifest
  ON agent_executions (worker_manifest_id, provider, queued_at DESC, id)
  WHERE worker_manifest_id IS NOT NULL;

COMMENT ON TABLE worker_manifests IS
  'Immutable, credential-free Worker build/image/protocol manifest addressed by SHA-256.';
COMMENT ON TABLE worker_provider_manifests IS
  'Per-Provider Host/CLI/capability compatibility descriptor within an immutable Worker manifest.';
