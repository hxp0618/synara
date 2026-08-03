ALTER TABLE stage6_release_candidates
  ADD COLUMN IF NOT EXISTS evidence_bundle_receipt BYTEA,
  ADD COLUMN IF NOT EXISTS evidence_bundle_schema TEXT,
  ADD COLUMN IF NOT EXISTS evidence_bundle_assessment TEXT,
  ADD COLUMN IF NOT EXISTS evidence_bundle_validated_at TEXT,
  ADD COLUMN IF NOT EXISTS evidence_bundle_receipt_size_bytes INTEGER,
  ADD COLUMN IF NOT EXISTS desktop_artifact_set_sha256 TEXT,
  ADD COLUMN IF NOT EXISTS evidence_receipt_bound BOOLEAN NOT NULL DEFAULT FALSE;

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
      AND evidence_bundle_schema = 'synara.stage6-candidate-evidence-bundle-validation.v2'
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
       OR evidence_receipt->>'schemaVersion' <> 'synara.stage6-candidate-evidence-bundle-validation.v2'
       OR evidence_receipt->>'assessment' <> 'evidence-consistent-not-ga-approved'
       OR (evidence_receipt->>'candidateConsistencyValidated')::boolean IS NOT TRUE
       OR (evidence_receipt->>'environmentEligible')::boolean IS NOT TRUE
       OR (evidence_receipt->>'allRequiredReceiptsReadyForCandidateReview')::boolean IS NOT TRUE
       OR (evidence_receipt->>'eligibleForCandidateEvidenceReview')::boolean IS NOT TRUE
       OR (evidence_receipt->>'requiredReceiptCount')::integer <> 9
       OR jsonb_typeof(evidence_receipt->'receipts') <> 'object'
       OR (SELECT count(*) FROM jsonb_object_keys(evidence_receipt->'receipts')) <> 9
       OR (SELECT count(*) FROM jsonb_object_keys(evidence_receipt->'receipts') AS key
           WHERE key IN ('billing', 'capacity', 'desktop', 'incident', 'operations', 'penetration', 'recovery', 'residency', 'slo')) <> 9
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

COMMENT ON COLUMN stage6_release_candidates.evidence_bundle_receipt IS
  'Exact bounded UTF-8 v2 candidate-evidence receipt bytes whose SHA-256 is stored separately.';
COMMENT ON COLUMN stage6_release_candidates.evidence_receipt_bound IS
  'True only when the service and database validated receipt eligibility and exact candidate identity.';
