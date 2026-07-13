ALTER TABLE worker_provider_manifests
  ADD COLUMN runtime_kind TEXT NOT NULL DEFAULT 'cli',
  ADD COLUMN runtime_name TEXT NOT NULL DEFAULT 'legacy-runtime',
  ADD COLUMN runtime_version TEXT,
  ADD COLUMN runtime_available BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN runtime_version_source TEXT NOT NULL DEFAULT 'probe',
  ADD COLUMN runtime_minimum_inclusive TEXT NOT NULL DEFAULT '0.0.0',
  ADD COLUMN runtime_maximum_exclusive TEXT,
  ADD COLUMN runtime_compatible BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN release_requires_explicit_enablement BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN release_enabled BOOLEAN NOT NULL DEFAULT TRUE;

ALTER TABLE worker_provider_manifests
  DROP CONSTRAINT worker_provider_manifests_compatibility_status_check,
  ADD CONSTRAINT worker_provider_manifests_compatibility_status_check
    CHECK (compatibility_status IN ('compatible', 'incompatible', 'unavailable', 'local-only', 'disabled'));

UPDATE worker_provider_manifests
SET runtime_name = provider,
    runtime_version = provider_cli_version,
    runtime_available = FALSE,
    runtime_compatible = FALSE,
    release_requires_explicit_enablement = (support_tier = 'experimental'),
    release_enabled = (support_tier <> 'experimental'),
    compatibility_status = CASE
      WHEN support_tier = 'experimental' THEN 'disabled'
      WHEN compatibility_status = 'compatible' THEN 'incompatible'
      ELSE compatibility_status
    END,
    incompatibility_code = CASE
      WHEN support_tier = 'experimental' THEN 'capability_unsupported'
      WHEN compatibility_status = 'compatible' THEN 'worker_manifest_reregistration_required'
      ELSE incompatibility_code
    END,
    incompatibility_message = CASE
      WHEN support_tier = 'experimental' THEN 'Experimental Provider is disabled until the Execution Target explicitly enables it and the Worker re-registers.'
      WHEN compatibility_status = 'compatible' THEN 'Worker must re-register with a Provider Host Protocol 2.1 runtime and release-policy descriptor.'
      ELSE incompatibility_message
    END;

UPDATE worker_instances
SET compatibility_status = 'incompatible',
    compatibility_reason = 'Worker must re-register with a Provider Host Protocol 2.1 runtime and release-policy manifest.',
    compatibility_checked_at = now()
WHERE current_manifest_id IS NOT NULL
  AND compatibility_status <> 'revoked';

UPDATE execution_targets
SET capabilities = jsonb_set(
      capabilities,
      '{providerPolicy}',
      '{"experimentalProviders":["codex","claudeAgent"]}'::jsonb,
      true
    ),
    updated_at = now()
WHERE tenant_id IS NOT NULL
  AND NOT (capabilities ? 'providerPolicy');

ALTER TABLE worker_provider_manifests
  ADD CONSTRAINT chk_worker_provider_manifests_runtime_kind
    CHECK (runtime_kind IN ('cli', 'sdk', 'local')),
  ADD CONSTRAINT chk_worker_provider_manifests_runtime_version_source
    CHECK (runtime_version_source IN ('probe', 'package', 'build')),
  ADD CONSTRAINT chk_worker_provider_manifests_runtime_shape
    CHECK (
      length(btrim(runtime_name)) BETWEEN 1 AND 200
      AND (runtime_version IS NULL OR length(btrim(runtime_version)) BETWEEN 1 AND 200)
      AND length(btrim(runtime_minimum_inclusive)) BETWEEN 1 AND 80
      AND (runtime_maximum_exclusive IS NULL OR length(btrim(runtime_maximum_exclusive)) BETWEEN 1 AND 80)
      AND (NOT runtime_compatible OR (runtime_available AND runtime_version IS NOT NULL))
    ),
  ADD CONSTRAINT chk_worker_provider_manifests_release_policy
    CHECK (
      (support_tier <> 'experimental' OR release_requires_explicit_enablement)
      AND (compatibility_status <> 'disabled' OR (
        support_tier = 'experimental'
        AND release_requires_explicit_enablement
        AND NOT release_enabled
      ))
      AND (compatibility_status <> 'compatible' OR (
        runtime_available
        AND runtime_compatible
        AND release_enabled
      ))
    );

ALTER TABLE provider_runtime_bindings
  ADD COLUMN runtime_kind TEXT,
  ADD COLUMN runtime_name TEXT,
  ADD COLUMN runtime_version TEXT,
  ADD COLUMN runtime_available BOOLEAN,
  ADD COLUMN runtime_version_source TEXT,
  ADD COLUMN runtime_minimum_inclusive TEXT,
  ADD COLUMN runtime_maximum_exclusive TEXT,
  ADD COLUMN runtime_compatible BOOLEAN,
  ADD COLUMN release_requires_explicit_enablement BOOLEAN,
  ADD COLUMN release_enabled BOOLEAN,
  ADD CONSTRAINT chk_provider_runtime_bindings_runtime_kind
    CHECK (runtime_kind IS NULL OR runtime_kind IN ('cli', 'sdk', 'local')),
  ADD CONSTRAINT chk_provider_runtime_bindings_runtime_version_source
    CHECK (runtime_version_source IS NULL OR runtime_version_source IN ('probe', 'package', 'build')),
  ADD CONSTRAINT chk_provider_runtime_bindings_runtime_snapshot
    CHECK (
      (
        runtime_kind IS NULL
        AND runtime_name IS NULL
        AND runtime_version IS NULL
        AND runtime_available IS NULL
        AND runtime_version_source IS NULL
        AND runtime_minimum_inclusive IS NULL
        AND runtime_maximum_exclusive IS NULL
        AND runtime_compatible IS NULL
        AND release_requires_explicit_enablement IS NULL
        AND release_enabled IS NULL
      )
      OR
      (
        runtime_kind IS NOT NULL
        AND runtime_name IS NOT NULL
        AND length(btrim(runtime_name)) BETWEEN 1 AND 200
        AND runtime_available IS NOT NULL
        AND runtime_version_source IS NOT NULL
        AND runtime_minimum_inclusive IS NOT NULL
        AND length(btrim(runtime_minimum_inclusive)) BETWEEN 1 AND 80
        AND (runtime_version IS NULL OR length(btrim(runtime_version)) BETWEEN 1 AND 200)
        AND (runtime_maximum_exclusive IS NULL OR length(btrim(runtime_maximum_exclusive)) BETWEEN 1 AND 80)
        AND runtime_compatible IS NOT NULL
        AND release_requires_explicit_enablement IS NOT NULL
        AND release_enabled IS NOT NULL
        AND (NOT runtime_compatible OR (runtime_available AND runtime_version IS NOT NULL))
      )
    );

COMMENT ON COLUMN worker_provider_manifests.runtime_version IS
  'Normalized CLI, SDK package, or local runtime version. provider_cli_version is retained for legacy readers only.';
COMMENT ON COLUMN worker_provider_manifests.release_enabled IS
  'Observed Provider Host enablement after agentd applies the authoritative Execution Target Provider policy.';
COMMENT ON COLUMN provider_runtime_bindings.runtime_kind IS
  'Immutable runtime and release-policy snapshot copied from the compatible Worker Provider manifest used by this binding.';
