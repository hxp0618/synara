DO $$
BEGIN
  IF EXISTS (
    SELECT 1
    FROM stage6_release_candidates
    WHERE evidence_receipt_bound
      AND evidence_bundle_schema = 'synara.stage6-candidate-evidence-bundle-validation.v2'
      AND state NOT IN ('released', 'rejected', 'rolled_back')
  ) THEN
    RAISE EXCEPTION 'Stage 6 candidate evidence v3 migration requires every active v2 candidate to be replaced or closed'
      USING ERRCODE = '23514';
  END IF;
END;
$$;

ALTER TABLE stage6_release_candidates
  DROP CONSTRAINT IF EXISTS chk_stage6_release_candidate_evidence_binding;
ALTER TABLE stage6_release_candidates
  ADD CONSTRAINT chk_stage6_release_candidate_evidence_binding CHECK (
    (NOT evidence_receipt_bound
      AND evidence_bundle_receipt IS NULL
      AND evidence_bundle_schema IS NULL
      AND evidence_bundle_assessment IS NULL
      AND evidence_bundle_validated_at IS NULL
      AND evidence_bundle_receipt_size_bytes IS NULL
      AND desktop_artifact_set_sha256 IS NULL)
    OR
    (evidence_receipt_bound
      AND evidence_bundle_receipt IS NOT NULL
      AND (
        evidence_bundle_schema = 'synara.stage6-candidate-evidence-bundle-validation.v3'
        OR (
          evidence_bundle_schema = 'synara.stage6-candidate-evidence-bundle-validation.v2'
          AND state IN ('released', 'rejected', 'rolled_back')
        )
      )
      AND evidence_bundle_assessment = 'evidence-consistent-not-ga-approved'
      AND evidence_bundle_validated_at IS NOT NULL
      AND evidence_bundle_receipt_size_bytes BETWEEN 1 AND 32768
      AND octet_length(evidence_bundle_receipt) = evidence_bundle_receipt_size_bytes
      AND desktop_artifact_set_sha256 ~ '^sha256:[0-9a-f]{64}$')
  );

CREATE OR REPLACE FUNCTION enforce_stage6_release_candidate_transition()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  required_approvals INTEGER;
  privacy_legal_approvals INTEGER;
  derived_privacy_legal_required BOOLEAN;
  evidence_receipt JSONB;
BEGIN
  IF jsonb_typeof(NEW.impact_domains) <> 'array' THEN
    RAISE EXCEPTION 'Stage 6 release impact domains must be a JSON array' USING ERRCODE = '23514';
  END IF;

  derived_privacy_legal_required := EXISTS (
    SELECT 1 FROM jsonb_array_elements_text(NEW.impact_domains) AS domain(value)
    WHERE value IN (
      'provider_commercial', 'billing_commercial', 'personal_data', 'retention_legal_hold',
      'data_residency', 'regulated_customer', 'security_incident'
    )
  );

  IF NEW.evidence_receipt_bound THEN
    evidence_receipt := convert_from(NEW.evidence_bundle_receipt, 'UTF8')::jsonb;
    IF jsonb_typeof(evidence_receipt) <> 'object'
       OR (SELECT count(*) FROM jsonb_object_keys(evidence_receipt)) <> 12
       OR NOT (evidence_receipt ?& ARRAY[
         'allRequiredReceiptsReadyForCandidateReview', 'assessment', 'candidate',
         'candidateConsistencyValidated', 'eligibleForCandidateEvidenceReview', 'environmentEligible',
         'manifest', 'receipts', 'releaseEvidence', 'requiredReceiptCount', 'schemaVersion', 'validatedAt'
       ])
       OR evidence_receipt->>'schemaVersion' <> 'synara.stage6-candidate-evidence-bundle-validation.v3'
       OR evidence_receipt->>'assessment' <> 'evidence-consistent-not-ga-approved'
       OR (evidence_receipt->>'candidateConsistencyValidated')::boolean IS NOT TRUE
       OR (evidence_receipt->>'environmentEligible')::boolean IS NOT TRUE
       OR (evidence_receipt->>'allRequiredReceiptsReadyForCandidateReview')::boolean IS NOT TRUE
       OR (evidence_receipt->>'eligibleForCandidateEvidenceReview')::boolean IS NOT TRUE
       OR (evidence_receipt->>'requiredReceiptCount')::integer <> 10
       OR jsonb_typeof(evidence_receipt->'receipts') <> 'object'
       OR (SELECT count(*) FROM jsonb_object_keys(evidence_receipt->'receipts')) <> 10
       OR (SELECT count(*) FROM jsonb_object_keys(evidence_receipt->'receipts') AS key
           WHERE key IN (
             'billing', 'capacity', 'desktop', 'incident', 'operations', 'penetration',
             'recovery', 'residency', 'slo', 'workerSupplyChain'
           )) <> 10
       OR jsonb_typeof(evidence_receipt->'candidate') <> 'object'
       OR (SELECT count(*) FROM jsonb_object_keys(evidence_receipt->'candidate')) <> 10
       OR NOT (evidence_receipt->'candidate' ?& ARRAY[
         'artifacts', 'candidateId', 'desktopArtifactSetSha256', 'environmentClass', 'environmentId',
         'lockfileSha256', 'migrationTail', 'origins', 'regions', 'sourceCommit'
       ])
       OR evidence_receipt->'candidate'->>'candidateId' <> NEW.candidate_id
       OR evidence_receipt->'candidate'->>'sourceCommit' <> NEW.source_commit
       OR evidence_receipt->'candidate'->>'lockfileSha256' <> encode(NEW.lockfile_sha256, 'hex')
       OR evidence_receipt->'candidate'->>'environmentId' <> NEW.environment_id
       OR evidence_receipt->'candidate'->>'environmentClass' NOT IN ('production', 'production-like')
       OR evidence_receipt->'candidate'->>'desktopArtifactSetSha256' <> NEW.desktop_artifact_set_sha256
       OR NEW.desktop_artifact_set_sha256 = 'sha256:' || repeat('0', 64)
       OR evidence_receipt->>'validatedAt' <> NEW.evidence_bundle_validated_at
       OR NEW.evidence_bundle_schema <> evidence_receipt->>'schemaVersion'
       OR NEW.evidence_bundle_assessment <> evidence_receipt->>'assessment'
       OR NEW.evidence_bundle_receipt_size_bytes <> octet_length(NEW.evidence_bundle_receipt)
       OR (NEW.evidence_bundle_validated_at)::timestamptz > now() + interval '5 minutes' THEN
      RAISE EXCEPTION 'Stage 6 release candidate evidence receipt is not bound to the candidate' USING ERRCODE = '23514';
    END IF;
  END IF;

  IF TG_OP = 'INSERT' THEN
    IF NOT NEW.evidence_receipt_bound
       OR jsonb_array_length(NEW.impact_domains) NOT BETWEEN 1 AND 11
       OR EXISTS (
         SELECT 1 FROM jsonb_array_elements_text(NEW.impact_domains) AS domain(value)
         WHERE value NOT IN (
           'code_change', 'data_migration', 'runtime_isolation', 'provider_commercial',
           'billing_commercial', 'personal_data', 'retention_legal_hold', 'data_residency',
           'regulated_customer', 'desktop_distribution', 'security_incident'
         )
       )
       OR (SELECT count(DISTINCT value) FROM jsonb_array_elements_text(NEW.impact_domains)) <> jsonb_array_length(NEW.impact_domains)
       OR NEW.privacy_legal_required IS DISTINCT FROM derived_privacy_legal_required
       OR NEW.state <> 'draft' OR NEW.version <> 1
       OR NEW.decision_summary IS NOT NULL OR NEW.residual_risk_disposition IS NOT NULL
       OR NEW.residual_risks <> '[]'::jsonb OR NEW.approved_at IS NOT NULL
       OR NEW.released_at IS NOT NULL OR NEW.rejected_at IS NOT NULL OR NEW.rolled_back_at IS NOT NULL
       OR NOT EXISTS (
         SELECT 1 FROM tenant_memberships AS membership
         JOIN tenants AS operator_tenant ON operator_tenant.id = membership.tenant_id
         JOIN users AS creator ON creator.id = membership.user_id
         WHERE membership.tenant_id = NEW.operator_tenant_id
           AND membership.user_id = NEW.created_by
           AND membership.status = 'active'
           AND membership.role IN ('owner', 'admin')
           AND operator_tenant.status = 'active' AND operator_tenant.deleted_at IS NULL
           AND creator.status = 'active' AND creator.deleted_at IS NULL
       ) THEN
      RAISE EXCEPTION 'Invalid Stage 6 release candidate initial state' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
  END IF;

  IF NEW.id IS DISTINCT FROM OLD.id
     OR NEW.operator_tenant_id IS DISTINCT FROM OLD.operator_tenant_id
     OR NEW.candidate_id IS DISTINCT FROM OLD.candidate_id
     OR NEW.source_commit IS DISTINCT FROM OLD.source_commit
     OR NEW.lockfile_sha256 IS DISTINCT FROM OLD.lockfile_sha256
     OR NEW.evidence_bundle_sha256 IS DISTINCT FROM OLD.evidence_bundle_sha256
     OR NEW.evidence_bundle_receipt IS DISTINCT FROM OLD.evidence_bundle_receipt
     OR NEW.evidence_bundle_schema IS DISTINCT FROM OLD.evidence_bundle_schema
     OR NEW.evidence_bundle_assessment IS DISTINCT FROM OLD.evidence_bundle_assessment
     OR NEW.evidence_bundle_validated_at IS DISTINCT FROM OLD.evidence_bundle_validated_at
     OR NEW.evidence_bundle_receipt_size_bytes IS DISTINCT FROM OLD.evidence_bundle_receipt_size_bytes
     OR NEW.desktop_artifact_set_sha256 IS DISTINCT FROM OLD.desktop_artifact_set_sha256
     OR NEW.evidence_receipt_bound IS DISTINCT FROM OLD.evidence_receipt_bound
     OR NEW.final_asset_set_sha256 IS DISTINCT FROM OLD.final_asset_set_sha256
     OR NEW.environment_id IS DISTINCT FROM OLD.environment_id
     OR NEW.impact_domains IS DISTINCT FROM OLD.impact_domains
     OR NEW.privacy_legal_required IS DISTINCT FROM OLD.privacy_legal_required
     OR NEW.created_by IS DISTINCT FROM OLD.created_by
     OR NEW.created_at IS DISTINCT FROM OLD.created_at
     OR NEW.version <> OLD.version + 1 THEN
    RAISE EXCEPTION 'Stage 6 release candidate identity and evidence are immutable' USING ERRCODE = '23514';
  END IF;

  IF (OLD.state = 'draft' AND NEW.state <> 'ready_for_review')
     OR (OLD.state = 'ready_for_review' AND NEW.state NOT IN ('approved', 'rejected'))
     OR (OLD.state = 'approved' AND NEW.state <> 'deploying')
     OR (OLD.state = 'deploying' AND NEW.state NOT IN ('observing', 'rolled_back'))
     OR (OLD.state = 'observing' AND NEW.state NOT IN ('released', 'rolled_back'))
     OR OLD.state IN ('released', 'rejected', 'rolled_back')
     OR (NEW.state = 'ready_for_review' AND NOT NEW.evidence_receipt_bound) THEN
    RAISE EXCEPTION 'Invalid Stage 6 release candidate transition' USING ERRCODE = '23514';
  END IF;

  IF NEW.state = 'approved' THEN
    SELECT count(*) INTO required_approvals
    FROM stage6_release_approvals
    WHERE candidate_record_id = NEW.id AND decision = 'approved'
      AND approval_role IN ('engineering', 'operations', 'security', 'product');
    SELECT count(*) INTO privacy_legal_approvals
    FROM stage6_release_approvals
    WHERE candidate_record_id = NEW.id AND decision = 'approved' AND approval_role = 'privacy_legal';
    IF required_approvals <> 4
       OR (NEW.privacy_legal_required AND privacy_legal_approvals <> 1)
       OR NEW.approved_at IS NULL THEN
      RAISE EXCEPTION 'Stage 6 release candidate requires all impact-derived separated approvals' USING ERRCODE = '23514';
    END IF;
  END IF;

  IF NEW.state = 'released' AND (
    NEW.released_at IS NULL
    OR length(trim(COALESCE(NEW.decision_summary, ''))) NOT BETWEEN 20 AND 4000
    OR NEW.residual_risk_disposition IS NULL
    OR (NEW.residual_risk_disposition = 'none' AND NEW.residual_risks <> '[]'::jsonb)
    OR (NEW.residual_risk_disposition = 'accepted' AND jsonb_array_length(NEW.residual_risks) = 0)
  ) THEN
    RAISE EXCEPTION 'Released Stage 6 candidate requires decision and residual-risk disposition' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
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
  ELSIF control_name = 'workerSupplyChain' THEN
    expected_count := 8;
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
  ELSIF control_name = 'workerSupplyChain' THEN
    RETURN COALESCE(value ?& ARRAY['admissionReportSha256', 'registryReportSha256']
      AND value->>'admissionReportSha256' ~ '^sha256:[0-9a-f]{64}$'
      AND value->>'admissionReportSha256' <> 'sha256:' || repeat('0', 64)
      AND value->>'registryReportSha256' ~ '^sha256:[0-9a-f]{64}$'
      AND value->>'registryReportSha256' <> 'sha256:' || repeat('0', 64), FALSE);
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
      ('slo', 'synara.slo-window-evidence-receipt.v1', 'evidence-validated-not-slo-passed'),
      ('workerSupplyChain', 'synara.stage6-worker-supply-chain-evidence.v1', 'evidence-validated-not-worker-supply-chain-approved')
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
     ) AS paths) <> 12 THEN
    RAISE EXCEPTION 'Stage 6 projected receipt bindings are inconsistent' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

COMMENT ON COLUMN stage6_release_candidates.evidence_bundle_receipt IS
  'Exact bounded UTF-8 v3 candidate-evidence receipt bytes whose SHA-256 is stored separately; terminal v2 rows remain historical only.';
COMMENT ON FUNCTION enforce_stage6_release_candidate_transition() IS
  'Requires every active Stage 6 release candidate to bind the v3 ten-receipt evidence bundle.';
COMMENT ON FUNCTION enforce_stage6_release_receipt_projection() IS
  'Rejects empty, drifted or cross-candidate Stage 6 v3 control projections, including Worker supply-chain evidence.';
