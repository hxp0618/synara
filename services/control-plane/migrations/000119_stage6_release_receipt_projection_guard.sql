CREATE OR REPLACE FUNCTION stage6_valid_evidence_reference(value JSONB)
RETURNS BOOLEAN
LANGUAGE plpgsql
IMMUTABLE
AS $$
DECLARE
  path_value TEXT;
  digest_value TEXT;
BEGIN
  IF jsonb_typeof(value) <> 'object'
     OR (SELECT count(*) FROM jsonb_object_keys(value)) <> 2
     OR NOT (value ?& ARRAY['path', 'sha256']) THEN
    RETURN FALSE;
  END IF;
  path_value := value->>'path';
  digest_value := value->>'sha256';
  RETURN COALESCE(length(path_value) BETWEEN 1 AND 512
    AND path_value ~ '^[A-Za-z0-9._@+-][A-Za-z0-9._/@+-]*$'
    AND path_value !~ '(^|/)\.{1,2}(/|$)'
    AND digest_value ~ '^sha256:[0-9a-f]{64}$'
    AND digest_value <> 'sha256:' || repeat('0', 64), FALSE);
END;
$$;

CREATE OR REPLACE FUNCTION stage6_valid_projected_control_receipt(
  value JSONB,
  control_name TEXT,
  expected_schema TEXT,
  expected_assessment TEXT,
  bundle_validated_at TIMESTAMPTZ
)
RETURNS BOOLEAN
LANGUAGE plpgsql
STABLE
AS $$
DECLARE
  expected_count INTEGER := 6;
BEGIN
  IF control_name = 'desktop' THEN
    expected_count := 7;
  ELSIF control_name = 'recovery' THEN
    expected_count := 10;
  ELSIF control_name = 'residency' THEN
    expected_count := 14;
  END IF;
  IF jsonb_typeof(value) <> 'object'
     OR (SELECT count(*) FROM jsonb_object_keys(value)) <> expected_count
     OR NOT (value ?& ARRAY['assessment', 'path', 'readyForCandidateReview', 'schemaVersion', 'sha256', 'validatedAt'])
     OR NOT stage6_valid_evidence_reference(jsonb_build_object('path', value->'path', 'sha256', value->'sha256'))
     OR value->>'schemaVersion' <> expected_schema
     OR value->>'assessment' <> expected_assessment
     OR jsonb_typeof(value->'readyForCandidateReview') <> 'boolean'
     OR (value->>'readyForCandidateReview')::boolean IS NOT TRUE
     OR jsonb_typeof(value->'validatedAt') <> 'string'
     OR (value->>'validatedAt')::timestamptz > bundle_validated_at THEN
    RETURN FALSE;
  END IF;
  IF control_name = 'desktop' THEN
    RETURN COALESCE(value ? 'desktopArtifactSetSha256'
      AND value->>'desktopArtifactSetSha256' ~ '^sha256:[0-9a-f]{64}$'
      AND value->>'desktopArtifactSetSha256' <> 'sha256:' || repeat('0', 64), FALSE);
  ELSIF control_name = 'recovery' THEN
    RETURN COALESCE(value ?& ARRAY[
      'candidateBindingSha256', 'cryptographicSignaturesVerified',
      'realBackupRestoreAndApproverAuthorityVerificationRequired', 'recoverySubjectSha256'
    ]
      AND value->>'candidateBindingSha256' ~ '^sha256:[0-9a-f]{64}$'
      AND value->>'candidateBindingSha256' <> 'sha256:' || repeat('0', 64)
      AND value->>'recoverySubjectSha256' ~ '^sha256:[0-9a-f]{64}$'
      AND value->>'recoverySubjectSha256' <> 'sha256:' || repeat('0', 64)
      AND jsonb_typeof(value->'cryptographicSignaturesVerified') = 'boolean'
      AND (value->>'cryptographicSignaturesVerified')::boolean IS FALSE
      AND jsonb_typeof(value->'realBackupRestoreAndApproverAuthorityVerificationRequired') = 'boolean'
      AND (value->>'realBackupRestoreAndApproverAuthorityVerificationRequired')::boolean IS TRUE, FALSE);
  ELSIF control_name = 'residency' THEN
    RETURN COALESCE(value ?& ARRAY[
      'allowedRegions', 'candidateBindingSha256', 'cryptographicSignaturesVerified',
      'evidenceSetSha256', 'externalSignatureIdentityAndAuthorityVerificationRequired',
      'homeRegion', 'policyDigest', 'policyVersion'
    ]
      AND value->>'candidateBindingSha256' ~ '^sha256:[0-9a-f]{64}$'
      AND value->>'candidateBindingSha256' <> 'sha256:' || repeat('0', 64)
      AND value->>'evidenceSetSha256' ~ '^sha256:[0-9a-f]{64}$'
      AND value->>'evidenceSetSha256' <> 'sha256:' || repeat('0', 64)
      AND value->>'policyDigest' ~ '^sha256:[0-9a-f]{64}$'
      AND value->>'policyDigest' <> 'sha256:' || repeat('0', 64)
      AND jsonb_typeof(value->'policyVersion') = 'number'
      AND (value->>'policyVersion')::integer >= 1
      AND value->>'homeRegion' ~ '^[A-Za-z0-9][A-Za-z0-9._:/@+-]{1,199}$'
      AND jsonb_typeof(value->'allowedRegions') = 'array'
      AND jsonb_typeof(value->'cryptographicSignaturesVerified') = 'boolean'
      AND (value->>'cryptographicSignaturesVerified')::boolean IS FALSE
      AND jsonb_typeof(value->'externalSignatureIdentityAndAuthorityVerificationRequired') = 'boolean'
      AND (value->>'externalSignatureIdentityAndAuthorityVerificationRequired')::boolean IS TRUE, FALSE);
  END IF;
  RETURN TRUE;
END;
$$;

CREATE OR REPLACE FUNCTION enforce_stage6_release_receipt_projection()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  receipt JSONB;
  candidate JSONB;
  artifacts JSONB;
  desktop_artifacts JSONB;
  origins JSONB;
  regions JSONB;
  migration_tail JSONB;
  release_evidence JSONB;
  bundle_validated_at TIMESTAMPTZ;
  projected JSONB;
  control_name TEXT;
  control_schema TEXT;
  control_assessment TEXT;
BEGIN
  IF NOT NEW.evidence_receipt_bound THEN
    RETURN NEW;
  END IF;
  receipt := convert_from(NEW.evidence_bundle_receipt, 'UTF8')::jsonb;
  bundle_validated_at := (receipt->>'validatedAt')::timestamptz;
  candidate := receipt->'candidate';
  IF jsonb_typeof(candidate) <> 'object' THEN
    RAISE EXCEPTION 'Invalid Stage 6 release candidate projection' USING ERRCODE = '23514';
  END IF;
  artifacts := candidate->'artifacts';
  desktop_artifacts := artifacts->'desktopArtifacts';
  IF jsonb_typeof(artifacts) <> 'object'
     OR (SELECT count(*) FROM jsonb_object_keys(artifacts)) <> 6
     OR NOT (artifacts ?& ARRAY['adminArtifact', 'controlPlaneImage', 'desktopArtifacts', 'providerHostImage', 'webArtifact', 'workerImage'])
     OR EXISTS (
       SELECT 1 FROM jsonb_each(artifacts) AS item(key, value)
       WHERE key <> 'desktopArtifacts' AND (
         jsonb_typeof(value) <> 'string'
         OR value #>> '{}' !~ '^sha256:[0-9a-f]{64}$'
         OR value #>> '{}' = 'sha256:' || repeat('0', 64)
       )
     )
     OR jsonb_typeof(desktop_artifacts) <> 'object'
     OR (SELECT count(*) FROM jsonb_object_keys(desktop_artifacts)) <> 4
     OR NOT (desktop_artifacts ?& ARRAY['linux-x64', 'macos-arm64', 'macos-x64', 'windows-x64'])
     OR EXISTS (
       SELECT 1 FROM jsonb_each(desktop_artifacts) AS item(key, value)
       WHERE jsonb_typeof(value) <> 'string'
         OR value #>> '{}' !~ '^sha256:[0-9a-f]{64}$'
         OR value #>> '{}' = 'sha256:' || repeat('0', 64)
     ) THEN
    RAISE EXCEPTION 'Invalid Stage 6 release artifact projection' USING ERRCODE = '23514';
  END IF;

  migration_tail := candidate->'migrationTail';
  origins := candidate->'origins';
  regions := candidate->'regions';
  IF jsonb_typeof(migration_tail) <> 'object'
     OR (SELECT count(*) FROM jsonb_object_keys(migration_tail)) <> 2
     OR NOT (migration_tail ?& ARRAY['name', 'sha256'])
     OR migration_tail->>'name' !~ '^[0-9]{6}_[a-z0-9_]+\.sql$'
     OR migration_tail->>'sha256' !~ '^sha256:[0-9a-f]{64}$'
     OR migration_tail->>'sha256' = 'sha256:' || repeat('0', 64)
     OR jsonb_typeof(origins) <> 'object'
     OR (SELECT count(*) FROM jsonb_object_keys(origins)) <> 3
     OR NOT (origins ?& ARRAY['adminBaseUrl', 'controlPlaneBaseUrl', 'webBaseUrl'])
     OR origins->>'controlPlaneBaseUrl' !~ '^https://[^/?#@]+(/[^?#]*)?$'
     OR origins->>'webBaseUrl' !~ '^https://[^/?#@]+$'
     OR origins->>'adminBaseUrl' !~ '^https://[^/?#@]+$'
     OR origins->>'webBaseUrl' = origins->>'adminBaseUrl'
     OR jsonb_typeof(regions) <> 'array'
     OR jsonb_array_length(regions) < 1
     OR EXISTS (
       SELECT 1 FROM jsonb_array_elements(regions) AS region(value)
       WHERE jsonb_typeof(value) <> 'string'
         OR value #>> '{}' !~ '^[A-Za-z0-9][A-Za-z0-9._:/@+-]{1,199}$'
     )
     OR (SELECT count(DISTINCT value #>> '{}') FROM jsonb_array_elements(regions) AS region(value)) <> jsonb_array_length(regions)
     OR (SELECT jsonb_agg(value ORDER BY value #>> '{}') FROM jsonb_array_elements(regions) AS region(value)) <> regions THEN
    RAISE EXCEPTION 'Invalid Stage 6 release deployment projection' USING ERRCODE = '23514';
  END IF;

  IF NOT stage6_valid_evidence_reference(receipt->'manifest') THEN
    RAISE EXCEPTION 'Invalid Stage 6 release manifest projection' USING ERRCODE = '23514';
  END IF;
  release_evidence := receipt->'releaseEvidence';
  IF jsonb_typeof(release_evidence) <> 'object'
     OR (SELECT count(*) FROM jsonb_object_keys(release_evidence)) <> 5
     OR NOT (release_evidence ?& ARRAY['assessment', 'generatedAt', 'path', 'schemaVersion', 'sha256'])
     OR NOT stage6_valid_evidence_reference(jsonb_build_object('path', release_evidence->'path', 'sha256', release_evidence->'sha256'))
     OR release_evidence->>'schemaVersion' <> 'synara-stage6-release-evidence-v2'
     OR release_evidence->>'assessment' <> 'evidence-collected-not-control-passed'
     OR (release_evidence->>'generatedAt')::timestamptz > bundle_validated_at THEN
    RAISE EXCEPTION 'Invalid Stage 6 release evidence projection' USING ERRCODE = '23514';
  END IF;

  FOR control_name, control_schema, control_assessment IN
    SELECT * FROM (VALUES
      ('billing', 'synara.stage6-stripe-billing-exercise-validation.v1', 'evidence-validated-not-billing-passed'),
      ('capacity', 'synara.capacity-soak-evidence-receipt.v1', 'evidence-validated-not-capacity-passed'),
      ('desktop', 'synara.stage6-desktop-native-acceptance-validation.v1', 'evidence-validated-not-desktop-ga-passed'),
      ('incident', 'synara.incident-communication-exercise-evidence-receipt.v1', 'evidence-validated-not-operations-ready'),
      ('operations', 'synara.stage6-operations-browser-exercise-validation.v1', 'evidence-validated-not-operations-passed'),
      ('penetration', 'synara.third-party-penetration-evidence-receipt.v1', 'evidence-validated-not-penetration-passed'),
      ('recovery', 'synara.recovery-drill-evidence-receipt.v2', 'evidence-validated-not-control-passed'),
      ('residency', 'synara.data-residency-deployment-evidence-receipt.v1', 'evidence-validated-not-residency-approved'),
      ('slo', 'synara.slo-window-evidence-receipt.v1', 'evidence-validated-not-slo-passed')
    ) AS controls(name, schema_name, assessment_name)
  LOOP
    projected := receipt->'receipts'->control_name;
    IF NOT stage6_valid_projected_control_receipt(projected, control_name, control_schema, control_assessment, bundle_validated_at) THEN
      RAISE EXCEPTION 'Invalid Stage 6 projected % receipt', control_name USING ERRCODE = '23514';
    END IF;
  END LOOP;

  IF receipt->'receipts'->'desktop'->>'desktopArtifactSetSha256' <> candidate->>'desktopArtifactSetSha256'
     OR receipt->'receipts'->'residency'->'allowedRegions' <> regions
     OR (SELECT count(DISTINCT path_value) FROM (
       SELECT receipt->'manifest'->>'path' AS path_value
       UNION ALL SELECT release_evidence->>'path'
       UNION ALL SELECT value->>'path' FROM jsonb_each(receipt->'receipts') AS item(key, value)
     ) AS paths) <> 11 THEN
    RAISE EXCEPTION 'Stage 6 projected receipt bindings are inconsistent' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_release_candidate_projection_guard
BEFORE INSERT OR UPDATE ON stage6_release_candidates
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_release_receipt_projection();

COMMENT ON FUNCTION enforce_stage6_release_receipt_projection() IS
  'Rejects empty, drifted or cross-candidate Stage 6 control projections before release governance can retain them.';
