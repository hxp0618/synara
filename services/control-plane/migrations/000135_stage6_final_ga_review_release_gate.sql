CREATE TABLE stage6_release_final_reviews (
  id UUID PRIMARY KEY,
  candidate_record_id UUID NOT NULL UNIQUE REFERENCES stage6_release_candidates(id) ON DELETE RESTRICT,
  operator_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  receipt_sha256 BYTEA NOT NULL CHECK (octet_length(receipt_sha256) = 32),
  receipt BYTEA NOT NULL CHECK (octet_length(receipt) BETWEEN 1 AND 524288),
  schema_version TEXT NOT NULL,
  assessment TEXT NOT NULL,
  validated_at TIMESTAMPTZ NOT NULL,
  receipt_size_bytes BIGINT NOT NULL CHECK (receipt_size_bytes BETWEEN 1 AND 524288),
  control_inventory_sha256 TEXT NOT NULL CHECK (control_inventory_sha256 ~ '^sha256:[0-9a-f]{64}$'),
  control_count INTEGER NOT NULL CHECK (control_count = 32),
  final_approval_count INTEGER NOT NULL CHECK (final_approval_count IN (4, 5)),
  decision_summary TEXT NOT NULL CHECK (length(trim(decision_summary)) BETWEEN 20 AND 4000),
  residual_risk_disposition TEXT NOT NULL CHECK (residual_risk_disposition IN ('none', 'accepted')),
  residual_risks JSONB NOT NULL CHECK (jsonb_typeof(residual_risks) = 'array'),
  all_required_controls_passed BOOLEAN NOT NULL,
  all_required_final_approvals_approved BOOLEAN NOT NULL,
  eligible_for_external_ga_authority_review BOOLEAN NOT NULL,
  external_evidence_authority_verified BOOLEAN NOT NULL DEFAULT FALSE,
  approver_corporate_authority_verified BOOLEAN NOT NULL DEFAULT FALSE,
  external_signatures_verified BOOLEAN NOT NULL DEFAULT FALSE,
  publication_delivery_verified_by_synara BOOLEAN NOT NULL DEFAULT FALSE,
  bound_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_stage6_release_final_reviews_candidate
  ON stage6_release_final_reviews (candidate_record_id, created_at, id);

CREATE OR REPLACE FUNCTION stage6_final_review_reference_valid(value JSONB)
RETURNS BOOLEAN
LANGUAGE sql
IMMUTABLE
PARALLEL SAFE
AS $$
  SELECT CASE
    WHEN jsonb_typeof(value) <> 'object' THEN FALSE
    ELSE
      (SELECT count(*) FROM jsonb_object_keys(value)) = 2
      AND value ?& ARRAY['path', 'sha256']
      AND length(value->>'path') BETWEEN 1 AND 512
      AND value->>'path' !~ '[\\]'
      AND value->>'path' !~ '^/'
      AND value->>'path' !~ '/$'
      AND value->>'path' !~ '//'
      AND value->>'path' !~ '(^|/)\.{1,2}(/|$)'
      AND value->>'sha256' ~ '^sha256:[0-9a-f]{64}$'
      AND value->>'sha256' <> 'sha256:' || repeat('0', 64)
  END;
$$;

CREATE OR REPLACE FUNCTION enforce_stage6_release_final_review_insert()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  candidate stage6_release_candidates%ROWTYPE;
  document JSONB;
  item JSONB;
  evidence_ref JSONB;
  item_id TEXT;
  item_role TEXT;
  item_identity TEXT;
  item_time TIMESTAMPTZ;
  started_at TIMESTAMPTZ;
  completed_at TIMESTAMPTZ;
  validated_at TIMESTAMPTZ;
  candidate_validated_at TIMESTAMPTZ;
  protected_approved_at TIMESTAMPTZ;
  latest_control_at TIMESTAMPTZ;
  control_ids TEXT[] := ARRAY[
    'tenant-registration', 'tenant-lifecycle', 'identity-lifecycle', 'sso-domain',
    'identity-governance', 'offer-admission', 'usage-reconciliation', 'quota-concurrency',
    'usage-explainability', 'billing-settlement', 'operations-matrix', 'authority-views',
    'support-access', 'incident-communications', 'audit-retention-legal-hold', 'privacy-workflows',
    'data-residency', 'provider-compliance', 'compliance-program', 'desktop-native',
    'desktop-security', 'desktop-postgres-concurrency', 'recovery', 'slo', 'tracing-isolation',
    'penetration-stage5', 'worker-supply-chain', 'compatibility-rollback', 'key-rotation',
    'capacity', 'documentation', 'change-notice'
  ];
  control_owners TEXT[] := ARRAY[
    'product', 'operations', 'security', 'security', 'security', 'product', 'engineering',
    'engineering', 'product', 'finance', 'operations', 'operations', 'security', 'operations',
    'security', 'privacy_legal', 'privacy_legal', 'privacy_legal', 'security', 'engineering',
    'security', 'engineering', 'operations', 'operations', 'security', 'security', 'security',
    'engineering', 'security', 'operations', 'product', 'product'
  ];
  required_roles TEXT[];
  seen_controls TEXT[] := ARRAY[]::TEXT[];
  seen_roles TEXT[] := ARRAY[]::TEXT[];
  seen_approvers TEXT[] := ARRAY[]::TEXT[];
  seen_requests UUID[] := ARRAY[]::UUID[];
  seen_risks TEXT[] := ARRAY[]::TEXT[];
  request_id UUID;
BEGIN
  SELECT * INTO candidate
  FROM stage6_release_candidates
  WHERE id = NEW.candidate_record_id
  FOR UPDATE;

  IF NOT FOUND OR candidate.operator_tenant_id <> NEW.operator_tenant_id OR candidate.state <> 'observing'
     OR digest(NEW.receipt, 'sha256') <> NEW.receipt_sha256
     OR octet_length(NEW.receipt) <> NEW.receipt_size_bytes
     OR NOT EXISTS (
       SELECT 1 FROM tenant_memberships AS membership
       JOIN tenants AS operator_tenant ON operator_tenant.id = membership.tenant_id
       JOIN users AS actor ON actor.id = membership.user_id
       WHERE membership.tenant_id = NEW.operator_tenant_id
         AND membership.user_id = NEW.bound_by
         AND membership.status = 'active' AND membership.role IN ('owner', 'admin')
         AND operator_tenant.status = 'active' AND operator_tenant.deleted_at IS NULL
         AND actor.status = 'active' AND actor.deleted_at IS NULL
     ) THEN
    RAISE EXCEPTION 'Stage 6 Final Review requires an observing exact candidate and active Platform manager'
      USING ERRCODE = '23514';
  END IF;

  document := convert_from(NEW.receipt, 'UTF8')::jsonb;
  IF jsonb_typeof(document) <> 'object'
     OR (SELECT count(*) FROM jsonb_object_keys(document)) <> 21
     OR NOT (document ?& ARRAY[
       'schemaVersion', 'candidate', 'releaseManagerId', 'impactDomains', 'controlInventorySha256',
       'controlCount', 'controlStatusCounts', 'controlDecisions', 'finalApprovals',
       'platformAuditRequestIds', 'decisionSummary', 'residualRiskDisposition', 'residualRisks',
       'startedAt', 'completedAt', 'validatedAt', 'allRequiredControlsPassed',
       'allRequiredFinalApprovalsApproved', 'eligibleForExternalGAAuthorityReview',
       'verificationBoundary', 'assessment'
     ])
     OR document->>'schemaVersion' <> 'synara.stage6-final-ga-review-validation.v1'
     OR document->>'assessment' <> 'final-review-consistent-not-ga-authority-verified'
     OR document->>'schemaVersion' <> NEW.schema_version
     OR document->>'assessment' <> NEW.assessment
     OR (document->>'validatedAt')::timestamptz <> NEW.validated_at
     OR document->>'controlInventorySha256' <> 'sha256:d48fb91c4934e284c87c445ef2c8b2ea7fdcb0ac086e1ea2cbc10738b3512cd8'
     OR document->>'controlInventorySha256' <> NEW.control_inventory_sha256
     OR (document->>'controlCount')::integer <> NEW.control_count
     OR jsonb_array_length(document->'controlDecisions') <> 32
     OR (document->'controlStatusCounts'->>'passed')::integer <> 32
     OR (document->'controlStatusCounts'->>'failed')::integer <> 0
     OR (document->'controlStatusCounts'->>'blocked')::integer <> 0
     OR jsonb_array_length(document->'finalApprovals') <> NEW.final_approval_count
     OR NEW.final_approval_count <> (CASE WHEN candidate.privacy_legal_required THEN 5 ELSE 4 END)
     OR (document->>'allRequiredControlsPassed')::boolean IS NOT TRUE
     OR (document->>'allRequiredFinalApprovalsApproved')::boolean IS NOT TRUE
     OR (document->>'eligibleForExternalGAAuthorityReview')::boolean IS NOT TRUE
     OR NOT NEW.all_required_controls_passed
     OR NOT NEW.all_required_final_approvals_approved
     OR NOT NEW.eligible_for_external_ga_authority_review
     OR (document->'verificationBoundary'->>'externalEvidenceAuthorityVerified')::boolean IS NOT FALSE
     OR (document->'verificationBoundary'->>'approverCorporateAuthorityVerified')::boolean IS NOT FALSE
     OR (document->'verificationBoundary'->>'externalSignaturesVerified')::boolean IS NOT FALSE
     OR (document->'verificationBoundary'->>'publicationDeliveryVerifiedBySynara')::boolean IS NOT FALSE
     OR NEW.external_evidence_authority_verified
     OR NEW.approver_corporate_authority_verified
     OR NEW.external_signatures_verified
     OR NEW.publication_delivery_verified_by_synara
     OR document->'candidate'->>'candidateId' <> candidate.candidate_id
     OR document->'candidate'->>'releaseTag' <> candidate.candidate_id
     OR document->'candidate'->>'sourceCommit' <> candidate.source_commit
     OR document->'candidate'->>'environmentId' <> candidate.environment_id
     OR document->'candidate'->>'lockfileSha256' <> encode(candidate.lockfile_sha256, 'hex')
     OR document->'candidate'->>'desktopArtifactSetSha256' <> candidate.desktop_artifact_set_sha256
     OR document->'candidate'->>'finalAssetSetSha256' <> 'sha256:' || encode(candidate.final_asset_set_sha256, 'hex')
     OR document->'candidate'->'candidateBundleReceipt'->>'sha256' <> 'sha256:' || encode(candidate.evidence_bundle_sha256, 'hex')
     OR document->'candidate'->>'candidateValidatedAt' <> candidate.evidence_bundle_validated_at
     OR document->'impactDomains' <> candidate.impact_domains
     OR document->>'decisionSummary' <> NEW.decision_summary
     OR document->>'residualRiskDisposition' <> NEW.residual_risk_disposition
     OR document->'residualRisks' <> NEW.residual_risks
     OR (NEW.residual_risk_disposition = 'none' AND NEW.residual_risks <> '[]'::jsonb)
     OR (NEW.residual_risk_disposition = 'accepted' AND jsonb_array_length(NEW.residual_risks) = 0)
     OR NEW.validated_at > now() + interval '5 minutes' THEN
    RAISE EXCEPTION 'Stage 6 Final Review receipt is not an eligible exact-candidate archive'
      USING ERRCODE = '23514';
  END IF;

  IF jsonb_typeof(document->'candidate') <> 'object'
     OR (SELECT count(*) FROM jsonb_object_keys(document->'candidate')) <> 15
     OR NOT (document->'candidate' ?& ARRAY[
       'candidateId', 'releaseTag', 'sourceCommit', 'environmentId', 'candidateBundleReceipt',
       'protectedReleaseApproval', 'finalAssetSet', 'finalAssetSetSha256', 'lockfileSha256',
       'desktopArtifactSetSha256', 'copiedChecklist', 'changeNotice', 'platformAuditExport',
       'candidateValidatedAt', 'protectedReleaseApprovedAt'
     ])
     OR jsonb_typeof(document->'controlStatusCounts') <> 'object'
     OR (SELECT count(*) FROM jsonb_object_keys(document->'controlStatusCounts')) <> 3
     OR jsonb_typeof(document->'verificationBoundary') <> 'object'
     OR (SELECT count(*) FROM jsonb_object_keys(document->'verificationBoundary')) <> 4
     OR jsonb_typeof(document->'impactDomains') <> 'array'
     OR jsonb_typeof(document->'controlDecisions') <> 'array'
     OR jsonb_typeof(document->'finalApprovals') <> 'array'
     OR jsonb_typeof(document->'platformAuditRequestIds') <> 'array'
     OR jsonb_typeof(document->'residualRisks') <> 'array'
     OR length(trim(document->>'releaseManagerId')) NOT BETWEEN 2 AND 200
     OR document->>'releaseManagerId' ~ '[[:cntrl:]]'
     OR NOT stage6_final_review_reference_valid(document->'candidate'->'candidateBundleReceipt')
     OR NOT stage6_final_review_reference_valid(document->'candidate'->'protectedReleaseApproval')
     OR NOT stage6_final_review_reference_valid(document->'candidate'->'finalAssetSet')
     OR NOT stage6_final_review_reference_valid(document->'candidate'->'copiedChecklist')
     OR NOT stage6_final_review_reference_valid(document->'candidate'->'changeNotice')
     OR NOT stage6_final_review_reference_valid(document->'candidate'->'platformAuditExport') THEN
    RAISE EXCEPTION 'Stage 6 Final Review receipt structure is invalid' USING ERRCODE = '23514';
  END IF;

  started_at := (document->>'startedAt')::timestamptz;
  completed_at := (document->>'completedAt')::timestamptz;
  validated_at := (document->>'validatedAt')::timestamptz;
  candidate_validated_at := (document->'candidate'->>'candidateValidatedAt')::timestamptz;
  protected_approved_at := (document->'candidate'->>'protectedReleaseApprovedAt')::timestamptz;
  latest_control_at := started_at;
  IF completed_at <= started_at OR validated_at < completed_at
     OR candidate_validated_at > protected_approved_at OR protected_approved_at > completed_at THEN
    RAISE EXCEPTION 'Stage 6 Final Review time closure is invalid' USING ERRCODE = '23514';
  END IF;

  FOR item IN SELECT value FROM jsonb_array_elements(document->'controlDecisions') LOOP
    item_id := item->>'id';
    IF jsonb_typeof(item) <> 'object'
       OR (SELECT count(*) FROM jsonb_object_keys(item)) <> 6
       OR NOT (item ?& ARRAY['id', 'ownerRole', 'approverId', 'status', 'decidedAt', 'evidence'])
       OR array_position(control_ids, item_id) IS NULL
       OR item->>'ownerRole' <> control_owners[array_position(control_ids, item_id)]
       OR item->>'status' <> 'passed'
       OR length(trim(item->>'approverId')) NOT BETWEEN 2 AND 200
       OR item->>'approverId' ~ '[[:cntrl:]]'
       OR array_position(seen_controls, item_id) IS NOT NULL
       OR jsonb_typeof(item->'evidence') <> 'array'
       OR jsonb_array_length(item->'evidence') < 1 THEN
      RAISE EXCEPTION 'Stage 6 Final Review control inventory is invalid' USING ERRCODE = '23514';
    END IF;
    item_time := (item->>'decidedAt')::timestamptz;
    IF item_time < started_at OR item_time > completed_at THEN
      RAISE EXCEPTION 'Stage 6 Final Review control time is invalid' USING ERRCODE = '23514';
    END IF;
    latest_control_at := greatest(latest_control_at, item_time);
    FOR evidence_ref IN SELECT value FROM jsonb_array_elements(item->'evidence') LOOP
      IF NOT stage6_final_review_reference_valid(evidence_ref) THEN
        RAISE EXCEPTION 'Stage 6 Final Review control evidence is invalid' USING ERRCODE = '23514';
      END IF;
    END LOOP;
    seen_controls := array_append(seen_controls, item_id);
  END LOOP;
  IF cardinality(seen_controls) <> cardinality(control_ids) THEN
    RAISE EXCEPTION 'Stage 6 Final Review control inventory is incomplete' USING ERRCODE = '23514';
  END IF;

  required_roles := ARRAY['engineering', 'operations', 'security', 'product'];
  IF candidate.privacy_legal_required THEN
    required_roles := array_append(required_roles, 'privacy_legal');
  END IF;
  seen_approvers := ARRAY[document->>'releaseManagerId'];
  FOR item IN SELECT value FROM jsonb_array_elements(document->'finalApprovals') LOOP
    item_role := item->>'role';
    item_identity := item->>'approverId';
    IF jsonb_typeof(item) <> 'object'
       OR (SELECT count(*) FROM jsonb_object_keys(item)) <> 5
       OR NOT (item ?& ARRAY['role', 'approverId', 'decision', 'approvedAt', 'evidence'])
       OR array_position(required_roles, item_role) IS NULL
       OR array_position(seen_roles, item_role) IS NOT NULL
       OR array_position(seen_approvers, item_identity) IS NOT NULL
       OR length(trim(item_identity)) NOT BETWEEN 2 AND 200
       OR item_identity ~ '[[:cntrl:]]'
       OR item->>'decision' <> 'approved-for-ga-authority-review'
       OR NOT stage6_final_review_reference_valid(item->'evidence') THEN
      RAISE EXCEPTION 'Stage 6 Final Review final approvals are invalid' USING ERRCODE = '23514';
    END IF;
    item_time := (item->>'approvedAt')::timestamptz;
    IF item_time < latest_control_at OR item_time < protected_approved_at OR item_time > completed_at THEN
      RAISE EXCEPTION 'Stage 6 Final Review approval time is invalid' USING ERRCODE = '23514';
    END IF;
    seen_roles := array_append(seen_roles, item_role);
    seen_approvers := array_append(seen_approvers, item_identity);
  END LOOP;
  IF cardinality(seen_roles) <> cardinality(required_roles) THEN
    RAISE EXCEPTION 'Stage 6 Final Review required approvals are incomplete' USING ERRCODE = '23514';
  END IF;

  IF jsonb_array_length(document->'platformAuditRequestIds') < 1 THEN
    RAISE EXCEPTION 'Stage 6 Final Review Audit request inventory is empty' USING ERRCODE = '23514';
  END IF;
  FOR item_id IN SELECT value FROM jsonb_array_elements_text(document->'platformAuditRequestIds') LOOP
    request_id := item_id::UUID;
    IF array_position(seen_requests, request_id) IS NOT NULL THEN
      RAISE EXCEPTION 'Stage 6 Final Review Audit request inventory has duplicates' USING ERRCODE = '23514';
    END IF;
    seen_requests := array_append(seen_requests, request_id);
  END LOOP;

  IF NEW.residual_risk_disposition = 'accepted' THEN
    FOR item IN SELECT value FROM jsonb_array_elements(document->'residualRisks') LOOP
      item_id := item->>'id';
      IF jsonb_typeof(item) <> 'object'
         OR (SELECT count(*) FROM jsonb_object_keys(item)) <> 6
         OR NOT (item ?& ARRAY['id', 'summary', 'owner', 'dueAt', 'acceptanceReason', 'evidenceReference'])
         OR item_id !~ '^[a-z0-9][a-z0-9._-]{2,79}$'
         OR array_position(seen_risks, item_id) IS NOT NULL
         OR length(trim(item->>'summary')) NOT BETWEEN 10 AND 500
         OR length(trim(item->>'owner')) NOT BETWEEN 3 AND 200
         OR length(trim(item->>'acceptanceReason')) NOT BETWEEN 10 AND 1000
         OR length(item->>'evidenceReference') NOT BETWEEN 8 AND 2048
         OR NOT stage6_valid_https_reference(item->>'evidenceReference')
         OR (item->>'dueAt')::timestamptz <= completed_at THEN
        RAISE EXCEPTION 'Stage 6 Final Review residual risk inventory is invalid' USING ERRCODE = '23514';
      END IF;
      seen_risks := array_append(seen_risks, item_id);
    END LOOP;
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_release_final_reviews_insert
BEFORE INSERT ON stage6_release_final_reviews
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_release_final_review_insert();

CREATE OR REPLACE FUNCTION reject_stage6_release_final_review_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'Stage 6 Final Review records are immutable' USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER trg_stage6_release_final_reviews_no_update
BEFORE UPDATE ON stage6_release_final_reviews
FOR EACH ROW EXECUTE FUNCTION reject_stage6_release_final_review_mutation();

CREATE TRIGGER trg_stage6_release_final_reviews_no_delete
BEFORE DELETE ON stage6_release_final_reviews
FOR EACH ROW EXECUTE FUNCTION reject_stage6_release_final_review_mutation();

CREATE OR REPLACE FUNCTION enforce_stage6_release_final_review_gate()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NEW.state = 'released' AND OLD.state = 'observing' AND NOT EXISTS (
    SELECT 1
    FROM stage6_release_final_reviews AS review
    WHERE review.candidate_record_id = NEW.id
      AND review.operator_tenant_id = NEW.operator_tenant_id
      AND review.all_required_controls_passed
      AND review.all_required_final_approvals_approved
      AND review.eligible_for_external_ga_authority_review
      AND NOT review.external_evidence_authority_verified
      AND NOT review.approver_corporate_authority_verified
      AND NOT review.external_signatures_verified
      AND NOT review.publication_delivery_verified_by_synara
      AND review.decision_summary = NEW.decision_summary
      AND review.residual_risk_disposition = NEW.residual_risk_disposition
      AND review.residual_risks = NEW.residual_risks
  ) THEN
    RAISE EXCEPTION 'Released Stage 6 candidate requires an exact eligible immutable Final Review'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_release_final_review_gate
BEFORE UPDATE ON stage6_release_candidates
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_release_final_review_gate();

COMMENT ON TABLE stage6_release_final_reviews IS
  'Immutable exact-candidate Final Review binding required before released; eligibility remains distinct from external GA authority verification.';
