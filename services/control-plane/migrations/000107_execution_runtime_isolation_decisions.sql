CREATE OR REPLACE FUNCTION synara_jsonb_text_array_is_unique(value JSONB)
RETURNS BOOLEAN
LANGUAGE SQL
IMMUTABLE
STRICT
AS $$
  SELECT CASE
    WHEN jsonb_typeof(value) <> 'array' THEN FALSE
    ELSE (
      SELECT COUNT(*) = COUNT(DISTINCT item)
      FROM jsonb_array_elements_text(value) AS items(item)
    )
  END;
$$;

ALTER TABLE worker_release_revisions
  ADD COLUMN gvisor_compatible_providers JSONB NOT NULL DEFAULT '[]'::jsonb;

ALTER TABLE worker_release_revisions
  ADD CONSTRAINT chk_worker_release_gvisor_compatible_providers CHECK (
    jsonb_typeof(gvisor_compatible_providers) = 'array'
    AND jsonb_array_length(gvisor_compatible_providers) <= 8
    AND gvisor_compatible_providers <@ '["codex", "claudeAgent", "cursor", "antigravity", "grok", "kilo", "opencode", "pi"]'::jsonb
    AND synara_jsonb_text_array_is_unique(gvisor_compatible_providers)
    AND octet_length(gvisor_compatible_providers::text) <= 256
  );

CREATE TABLE execution_runtime_isolation_decisions (
  tenant_id UUID NOT NULL,
  execution_id UUID NOT NULL,
  generation BIGINT NOT NULL,
  execution_target_id UUID NOT NULL,
  allocation_backend TEXT NOT NULL,
  requested_runtime TEXT NOT NULL,
  requested_profile TEXT NOT NULL,
  effective_runtime TEXT,
  effective_profile TEXT,
  policy_source TEXT NOT NULL,
  decision TEXT NOT NULL,
  decision_reason_code TEXT,
  runtime_class_name TEXT,
  attestation_digest TEXT,
  attested_at TIMESTAMPTZ,
  attestation_expires_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT pk_execution_runtime_isolation_decisions
    PRIMARY KEY (tenant_id, execution_id, generation),
  CONSTRAINT fk_execution_runtime_isolation_decisions_generation
    FOREIGN KEY (tenant_id, execution_id, generation)
    REFERENCES execution_generation_facts(tenant_id, execution_id, generation) ON DELETE RESTRICT,
  CONSTRAINT fk_execution_runtime_isolation_decisions_target
    FOREIGN KEY (execution_target_id) REFERENCES execution_targets(id) ON DELETE RESTRICT,
  CONSTRAINT chk_execution_runtime_isolation_generation CHECK (generation > 0),
  CONSTRAINT chk_execution_runtime_isolation_backend CHECK (
    allocation_backend IN ('docker-engine', 'native-pod', 'sandbox-operator-standard', 'sandbox-operator-cocoon')
  ),
  CONSTRAINT chk_execution_runtime_isolation_requested_runtime CHECK (
    requested_runtime IN ('auto', 'runc', 'gvisor', 'firecracker')
  ),
  CONSTRAINT chk_execution_runtime_isolation_requested_profile CHECK (
    requested_profile IN (
      'single-tenant-trusted-v1', 'kubernetes-restricted-v1',
      'gvisor-sandboxed-v1', 'microvm-isolated-v1'
    )
  ),
  CONSTRAINT chk_execution_runtime_isolation_effective_runtime CHECK (
    effective_runtime IS NULL OR effective_runtime IN ('runc', 'gvisor', 'firecracker')
  ),
  CONSTRAINT chk_execution_runtime_isolation_effective_profile CHECK (
    effective_profile IS NULL OR effective_profile IN (
      'single-tenant-trusted-v1', 'kubernetes-restricted-v1',
      'gvisor-sandboxed-v1', 'microvm-isolated-v1'
    )
  ),
  CONSTRAINT chk_execution_runtime_isolation_policy_source CHECK (
    policy_source IN ('legacy-native', 'target-explicit', 'target-auto', 'lane-policy')
  ),
  CONSTRAINT chk_execution_runtime_isolation_decision CHECK (
    decision IN ('selected', 'fallback', 'rejected')
  ),
  CONSTRAINT chk_execution_runtime_isolation_shape CHECK (
    (
      decision IN ('selected', 'fallback')
      AND effective_runtime IS NOT NULL
      AND effective_profile IS NOT NULL
      AND decision_reason_code IS NULL
    ) OR (
      decision = 'rejected'
      AND effective_runtime IS NULL
      AND effective_profile IS NULL
      AND decision_reason_code IS NOT NULL
      AND runtime_class_name IS NULL
      AND attestation_digest IS NULL
      AND attested_at IS NULL
      AND attestation_expires_at IS NULL
    )
  ),
  CONSTRAINT chk_execution_runtime_isolation_gvisor CHECK (
    (
      allocation_backend = 'docker-engine'
      AND effective_runtime = 'gvisor'
      AND effective_profile = 'single-tenant-trusted-v1'
      AND runtime_class_name IS NULL
      AND attestation_digest IS NULL
      AND attested_at IS NULL
      AND attestation_expires_at IS NULL
    ) OR (
      allocation_backend <> 'docker-engine'
      AND effective_runtime = 'gvisor'
      AND effective_profile = 'gvisor-sandboxed-v1'
      AND runtime_class_name IS NOT NULL
      AND attestation_digest IS NOT NULL
      AND attested_at IS NOT NULL
      AND attestation_expires_at IS NOT NULL
      AND attestation_expires_at > attested_at
    ) OR (
      effective_runtime IS DISTINCT FROM 'gvisor'
      AND runtime_class_name IS NULL
    )
  ),
  CONSTRAINT chk_execution_runtime_isolation_microvm CHECK (
    effective_runtime IS DISTINCT FROM 'firecracker'
    OR (
      effective_profile = 'microvm-isolated-v1'
      AND attestation_digest IS NOT NULL
      AND attested_at IS NOT NULL
      AND attestation_expires_at IS NOT NULL
      AND attestation_expires_at > attested_at
    )
  ),
  CONSTRAINT chk_execution_runtime_isolation_identity CHECK (
    length(allocation_backend) BETWEEN 1 AND 80
    AND length(requested_runtime) BETWEEN 1 AND 32
    AND length(requested_profile) BETWEEN 1 AND 80
    AND (effective_runtime IS NULL OR length(effective_runtime) BETWEEN 1 AND 32)
    AND (effective_profile IS NULL OR length(effective_profile) BETWEEN 1 AND 80)
    AND length(policy_source) BETWEEN 1 AND 80
    AND (decision_reason_code IS NULL OR length(decision_reason_code) BETWEEN 1 AND 160)
    AND (runtime_class_name IS NULL OR length(runtime_class_name) BETWEEN 1 AND 63)
    AND (attestation_digest IS NULL OR attestation_digest ~ '^[0-9a-f]{64}$')
  )
);

CREATE INDEX idx_execution_runtime_isolation_target_profile
  ON execution_runtime_isolation_decisions (execution_target_id, effective_profile, created_at);

CREATE TABLE execution_target_runtime_isolation_observations (
  execution_target_id UUID PRIMARY KEY
    REFERENCES execution_targets(id) ON DELETE CASCADE,
  detected_runtimes JSONB NOT NULL DEFAULT '[]'::jsonb,
  detected_profiles JSONB NOT NULL DEFAULT '[]'::jsonb,
  requested_runtime TEXT NOT NULL,
  requested_profile TEXT NOT NULL,
  effective_runtime TEXT,
  effective_profile TEXT,
  policy_source TEXT NOT NULL,
  decision TEXT NOT NULL,
  state TEXT NOT NULL,
  reason_code TEXT,
  observed_at TIMESTAMPTZ NOT NULL,
  expires_at TIMESTAMPTZ NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL,
  CONSTRAINT chk_execution_target_runtime_isolation_observation_arrays CHECK (
    jsonb_typeof(detected_runtimes) = 'array'
    AND jsonb_typeof(detected_profiles) = 'array'
    AND octet_length(detected_runtimes::text) <= 512
    AND octet_length(detected_profiles::text) <= 1024
  ),
  CONSTRAINT chk_execution_target_runtime_isolation_observation_state CHECK (
    state IN ('available', 'degraded', 'unattested', 'stale')
  ),
  CONSTRAINT chk_execution_target_runtime_isolation_observation_decision CHECK (
    requested_runtime IN ('auto', 'runc', 'gvisor', 'firecracker')
    AND requested_profile IN (
      'single-tenant-trusted-v1', 'kubernetes-restricted-v1',
      'gvisor-sandboxed-v1', 'microvm-isolated-v1'
    )
    AND policy_source IN ('legacy-native', 'target-explicit', 'target-auto', 'lane-policy')
    AND (
      (
        decision IN ('selected', 'fallback')
        AND effective_runtime IN ('runc', 'gvisor', 'firecracker')
        AND effective_profile IN (
          'single-tenant-trusted-v1', 'kubernetes-restricted-v1',
          'gvisor-sandboxed-v1', 'microvm-isolated-v1'
        )
      ) OR (
        decision = 'rejected'
        AND effective_runtime IS NULL
        AND effective_profile IS NULL
      )
    )
  ),
  CONSTRAINT chk_execution_target_runtime_isolation_observation_time CHECK (
    expires_at > observed_at AND updated_at >= observed_at
  ),
  CONSTRAINT chk_execution_target_runtime_isolation_observation_reason CHECK (
    reason_code IS NULL OR length(reason_code) BETWEEN 1 AND 160
  )
);

CREATE OR REPLACE FUNCTION enforce_execution_runtime_isolation_decision()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  generation_target_id UUID;
BEGIN
  IF TG_OP IN ('UPDATE', 'DELETE') THEN
    RAISE EXCEPTION 'Execution runtime isolation decisions are append-only' USING ERRCODE = '23514';
  END IF;
  SELECT execution_target_id INTO generation_target_id
  FROM execution_generation_facts
  WHERE tenant_id = NEW.tenant_id
    AND execution_id = NEW.execution_id
    AND generation = NEW.generation;
  IF generation_target_id IS NULL OR generation_target_id <> NEW.execution_target_id THEN
    RAISE EXCEPTION 'Execution runtime isolation decision scope is invalid' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_execution_runtime_isolation_decisions
  ON execution_runtime_isolation_decisions;
CREATE TRIGGER trg_execution_runtime_isolation_decisions
BEFORE INSERT OR UPDATE OR DELETE ON execution_runtime_isolation_decisions
FOR EACH ROW EXECUTE FUNCTION enforce_execution_runtime_isolation_decision();
