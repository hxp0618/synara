ALTER TABLE stage6_governance_authority_grants
  DROP CONSTRAINT IF EXISTS stage6_governance_authority_grants_authority_key_check;
ALTER TABLE stage6_governance_authority_grants
  ADD CONSTRAINT stage6_governance_authority_grants_authority_key_check CHECK (authority_key IN (
    'release.engineering', 'release.operations', 'release.security', 'release.product', 'release.privacy_legal',
    'compliance.security', 'compliance.operations', 'compliance.legal_privacy', 'compliance.executive',
    'compliance.evidence.security', 'compliance.evidence.operations', 'compliance.evidence.legal_privacy', 'compliance.evidence.auditor',
    'provider_commercial.legal', 'provider_commercial.privacy', 'provider_commercial.security', 'provider_commercial.product',
    'recovery.database', 'recovery.kms', 'recovery.operations', 'recovery.security', 'recovery.storage',
    'penetration.engineering', 'penetration.product', 'penetration.security'
  ));

CREATE TABLE stage6_penetration_engagements (
  id UUID PRIMARY KEY,
  operator_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  candidate_record_id UUID NOT NULL REFERENCES stage6_release_candidates(id) ON DELETE RESTRICT,
  engagement_id UUID NOT NULL UNIQUE,
  receipt BYTEA NOT NULL CHECK (octet_length(receipt) BETWEEN 1 AND 524288),
  receipt_sha256 BYTEA NOT NULL CHECK (octet_length(receipt_sha256) = 32),
  receipt_size_bytes BIGINT NOT NULL CHECK (receipt_size_bytes > 0),
  receipt_schema TEXT NOT NULL,
  assessment TEXT NOT NULL,
  release_commit TEXT NOT NULL CHECK (length(release_commit) = 40),
  environment_class TEXT NOT NULL CHECK (environment_class IN ('production', 'production-like')),
  environment_id TEXT NOT NULL CHECK (length(environment_id) BETWEEN 3 AND 300),
  deployment_profile TEXT NOT NULL CHECK (length(deployment_profile) BETWEEN 2 AND 160),
  started_at TIMESTAMPTZ NOT NULL,
  completed_at TIMESTAMPTZ NOT NULL,
  report_issued_at TIMESTAMPTZ NOT NULL,
  validated_at TIMESTAMPTZ NOT NULL,
  third_party_independence_declared BOOLEAN NOT NULL,
  stage5_dependency_satisfied BOOLEAN NOT NULL,
  asset_coverage_complete BOOLEAN NOT NULL,
  scope_coverage_complete BOOLEAN NOT NULL,
  methodology_coverage_complete BOOLEAN NOT NULL,
  no_unaccepted_high_or_critical_findings BOOLEAN NOT NULL,
  eligible_for_human_gate_review BOOLEAN NOT NULL,
  cryptographic_signatures_verified BOOLEAN NOT NULL,
  external_authority_verification_required BOOLEAN NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('recorded', 'approved', 'rejected')),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  created_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  approved_at TIMESTAMPTZ,
  rejected_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (candidate_record_id, receipt_sha256),
  CHECK (started_at < completed_at AND completed_at <= report_issued_at AND report_issued_at <= validated_at)
);

CREATE INDEX idx_stage6_penetration_engagements_candidate
  ON stage6_penetration_engagements (candidate_record_id, created_at DESC, id);

CREATE TABLE stage6_penetration_assets (
  id UUID PRIMARY KEY,
  penetration_engagement_id UUID NOT NULL REFERENCES stage6_penetration_engagements(id) ON DELETE RESTRICT,
  operator_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  asset_type TEXT NOT NULL CHECK (asset_type IN ('control-plane-api', 'provider-host', 'web', 'worker-runtime')),
  artifact_sha256 BYTEA NOT NULL CHECK (octet_length(artifact_sha256) = 32),
  tested BOOLEAN NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (penetration_engagement_id, asset_type)
);

CREATE TABLE stage6_penetration_approvals (
  id UUID PRIMARY KEY,
  penetration_engagement_id UUID NOT NULL REFERENCES stage6_penetration_engagements(id) ON DELETE RESTRICT,
  operator_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  approval_role TEXT NOT NULL CHECK (approval_role IN ('engineering', 'product', 'security')),
  decision TEXT NOT NULL CHECK (decision IN ('approved', 'rejected')),
  approver_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  reason TEXT NOT NULL CHECK (length(trim(reason)) BETWEEN 20 AND 2000),
  evidence_reference TEXT NOT NULL CHECK (length(trim(evidence_reference)) BETWEEN 8 AND 2048),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (penetration_engagement_id, approval_role),
  UNIQUE (penetration_engagement_id, approver_user_id)
);

CREATE INDEX idx_stage6_penetration_approvals_engagement
  ON stage6_penetration_approvals (penetration_engagement_id, created_at, id);

CREATE OR REPLACE FUNCTION enforce_stage6_penetration_engagement()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  approval_count INTEGER;
  receipt_json JSONB;
BEGIN
  IF TG_OP = 'INSERT' THEN
    receipt_json := convert_from(NEW.receipt, 'UTF8')::jsonb;
    IF NEW.receipt_size_bytes <> octet_length(NEW.receipt)
       OR digest(NEW.receipt, 'sha256') <> NEW.receipt_sha256
       OR NEW.receipt_schema <> 'synara.third-party-penetration-evidence-receipt.v1'
       OR NEW.assessment <> 'evidence-validated-not-penetration-passed'
       OR receipt_json->>'schemaVersion' <> NEW.receipt_schema
       OR receipt_json->>'assessment' <> NEW.assessment
       OR receipt_json->>'engagementId' <> NEW.engagement_id::text
       OR receipt_json->>'releaseCommit' <> NEW.release_commit
       OR receipt_json->>'environmentClass' <> NEW.environment_class
       OR receipt_json->>'environmentId' <> NEW.environment_id
       OR receipt_json->>'deploymentProfile' <> NEW.deployment_profile
       OR (receipt_json->>'thirdPartyIndependenceDeclared')::boolean IS DISTINCT FROM NEW.third_party_independence_declared
       OR (receipt_json#>>'{stage5Dependency,declaredSatisfied}')::boolean IS DISTINCT FROM NEW.stage5_dependency_satisfied
       OR (receipt_json->>'assetCoverageComplete')::boolean IS DISTINCT FROM NEW.asset_coverage_complete
       OR (receipt_json->>'scopeCoverageComplete')::boolean IS DISTINCT FROM NEW.scope_coverage_complete
       OR (receipt_json->>'methodologyCoverageComplete')::boolean IS DISTINCT FROM NEW.methodology_coverage_complete
       OR (receipt_json->>'declaredNoUnacceptedHighOrCriticalFindings')::boolean IS DISTINCT FROM NEW.no_unaccepted_high_or_critical_findings
       OR (receipt_json->>'eligibleForHumanGateReview')::boolean IS DISTINCT FROM NEW.eligible_for_human_gate_review
       OR NOT (receipt_json->>'releaseEligibleEnvironment')::boolean
       OR jsonb_array_length(receipt_json->'assets') <> 4
       OR NOT NEW.third_party_independence_declared OR NOT NEW.stage5_dependency_satisfied
       OR NOT NEW.asset_coverage_complete OR NOT NEW.scope_coverage_complete
       OR NOT NEW.methodology_coverage_complete OR NOT NEW.no_unaccepted_high_or_critical_findings
       OR NOT NEW.eligible_for_human_gate_review OR NEW.cryptographic_signatures_verified
       OR NOT NEW.external_authority_verification_required
       OR NEW.state <> 'recorded' OR NEW.version <> 1
       OR NEW.approved_at IS NOT NULL OR NEW.rejected_at IS NOT NULL
       OR NOT EXISTS (
         SELECT 1 FROM stage6_release_candidates candidate
         WHERE candidate.id = NEW.candidate_record_id
           AND candidate.operator_tenant_id = NEW.operator_tenant_id
           AND candidate.created_by = NEW.created_by
           AND candidate.source_commit = NEW.release_commit
           AND candidate.environment_id = NEW.environment_id
           AND convert_from(candidate.evidence_bundle_receipt, 'UTF8')::jsonb
                 #>> '{candidate,environmentClass}' = NEW.environment_class
           AND convert_from(candidate.evidence_bundle_receipt, 'UTF8')::jsonb
                 #>> '{receipts,penetration,sha256}' = 'sha256:' || encode(NEW.receipt_sha256, 'hex')
       ) THEN
      RAISE EXCEPTION 'Invalid Stage 6 Penetration engagement receipt binding' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
  END IF;

  IF NEW.id IS DISTINCT FROM OLD.id OR NEW.operator_tenant_id IS DISTINCT FROM OLD.operator_tenant_id
     OR NEW.candidate_record_id IS DISTINCT FROM OLD.candidate_record_id OR NEW.engagement_id IS DISTINCT FROM OLD.engagement_id
     OR NEW.receipt IS DISTINCT FROM OLD.receipt OR NEW.receipt_sha256 IS DISTINCT FROM OLD.receipt_sha256
     OR NEW.receipt_size_bytes IS DISTINCT FROM OLD.receipt_size_bytes OR NEW.receipt_schema IS DISTINCT FROM OLD.receipt_schema
     OR NEW.assessment IS DISTINCT FROM OLD.assessment OR NEW.release_commit IS DISTINCT FROM OLD.release_commit
     OR NEW.environment_class IS DISTINCT FROM OLD.environment_class OR NEW.environment_id IS DISTINCT FROM OLD.environment_id
     OR NEW.deployment_profile IS DISTINCT FROM OLD.deployment_profile OR NEW.started_at IS DISTINCT FROM OLD.started_at
     OR NEW.completed_at IS DISTINCT FROM OLD.completed_at OR NEW.report_issued_at IS DISTINCT FROM OLD.report_issued_at
     OR NEW.validated_at IS DISTINCT FROM OLD.validated_at
     OR NEW.third_party_independence_declared IS DISTINCT FROM OLD.third_party_independence_declared
     OR NEW.stage5_dependency_satisfied IS DISTINCT FROM OLD.stage5_dependency_satisfied
     OR NEW.asset_coverage_complete IS DISTINCT FROM OLD.asset_coverage_complete
     OR NEW.scope_coverage_complete IS DISTINCT FROM OLD.scope_coverage_complete
     OR NEW.methodology_coverage_complete IS DISTINCT FROM OLD.methodology_coverage_complete
     OR NEW.no_unaccepted_high_or_critical_findings IS DISTINCT FROM OLD.no_unaccepted_high_or_critical_findings
     OR NEW.eligible_for_human_gate_review IS DISTINCT FROM OLD.eligible_for_human_gate_review
     OR NEW.cryptographic_signatures_verified IS DISTINCT FROM OLD.cryptographic_signatures_verified
     OR NEW.external_authority_verification_required IS DISTINCT FROM OLD.external_authority_verification_required
     OR NEW.created_by IS DISTINCT FROM OLD.created_by OR NEW.created_at IS DISTINCT FROM OLD.created_at
     OR NEW.version <> OLD.version + 1 OR OLD.state <> 'recorded' OR NEW.state NOT IN ('approved', 'rejected') THEN
    RAISE EXCEPTION 'Stage 6 Penetration identity and evidence are immutable' USING ERRCODE = '23514';
  END IF;
  IF NEW.state = 'approved' THEN
    SELECT count(*) INTO approval_count FROM stage6_penetration_approvals
    WHERE penetration_engagement_id = NEW.id AND decision = 'approved';
    IF approval_count <> 3 OR NOT NEW.eligible_for_human_gate_review
       OR NEW.approved_at IS NULL OR NEW.rejected_at IS NOT NULL THEN
      RAISE EXCEPTION 'Stage 6 Penetration approval gate is incomplete' USING ERRCODE = '23514';
    END IF;
  ELSIF NEW.rejected_at IS NULL OR NEW.approved_at IS NOT NULL THEN
    RAISE EXCEPTION 'Stage 6 Penetration rejection timestamp is required' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_penetration_engagements_guard
BEFORE INSERT OR UPDATE ON stage6_penetration_engagements
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_penetration_engagement();

CREATE OR REPLACE FUNCTION enforce_stage6_penetration_asset()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  parent stage6_penetration_engagements%ROWTYPE;
  asset JSONB;
BEGIN
  SELECT * INTO parent FROM stage6_penetration_engagements WHERE id = NEW.penetration_engagement_id FOR UPDATE;
  SELECT item INTO asset FROM jsonb_array_elements(convert_from(parent.receipt, 'UTF8')::jsonb->'assets') item
  WHERE item->>'assetType' = NEW.asset_type;
  IF parent.id IS NULL OR parent.operator_tenant_id <> NEW.operator_tenant_id OR parent.state <> 'recorded'
     OR asset IS NULL OR NOT NEW.tested OR (asset->>'tested')::boolean IS DISTINCT FROM NEW.tested
     OR asset->>'artifactDigest' <> 'sha256:' || encode(NEW.artifact_sha256, 'hex') THEN
    RAISE EXCEPTION 'Invalid Stage 6 Penetration asset projection' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_penetration_assets_insert
BEFORE INSERT ON stage6_penetration_assets
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_penetration_asset();

CREATE OR REPLACE FUNCTION enforce_stage6_penetration_approval()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  parent stage6_penetration_engagements%ROWTYPE;
BEGIN
  SELECT * INTO parent FROM stage6_penetration_engagements WHERE id = NEW.penetration_engagement_id FOR UPDATE;
  IF parent.id IS NULL OR parent.operator_tenant_id <> NEW.operator_tenant_id OR parent.state <> 'recorded'
     OR parent.created_by = NEW.approver_user_id
     OR NOT EXISTS (
       SELECT 1 FROM stage6_governance_authority_grants authority
       JOIN tenant_memberships membership ON membership.tenant_id = authority.operator_tenant_id
         AND membership.user_id = authority.user_id
       JOIN tenants operator_tenant ON operator_tenant.id = authority.operator_tenant_id
       JOIN users governed_user ON governed_user.id = authority.user_id
       WHERE authority.operator_tenant_id = NEW.operator_tenant_id
         AND authority.user_id = NEW.approver_user_id
         AND authority.authority_key = 'penetration.' || NEW.approval_role
         AND authority.status = 'active' AND authority.expires_at > statement_timestamp()
         AND membership.status = 'active' AND membership.role IN ('owner', 'admin', 'security_admin')
         AND operator_tenant.status = 'active' AND operator_tenant.deleted_at IS NULL
         AND governed_user.status = 'active' AND governed_user.deleted_at IS NULL
     ) THEN
    RAISE EXCEPTION 'Invalid Stage 6 Penetration approval authority' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_penetration_approvals_insert
BEFORE INSERT ON stage6_penetration_approvals
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_penetration_approval();

CREATE OR REPLACE FUNCTION reject_stage6_penetration_history_mutation()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'Stage 6 Penetration evidence history is immutable' USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER trg_stage6_penetration_assets_no_update BEFORE UPDATE ON stage6_penetration_assets
FOR EACH ROW EXECUTE FUNCTION reject_stage6_penetration_history_mutation();
CREATE TRIGGER trg_stage6_penetration_assets_no_delete BEFORE DELETE ON stage6_penetration_assets
FOR EACH ROW EXECUTE FUNCTION reject_stage6_penetration_history_mutation();
CREATE TRIGGER trg_stage6_penetration_approvals_no_update BEFORE UPDATE ON stage6_penetration_approvals
FOR EACH ROW EXECUTE FUNCTION reject_stage6_penetration_history_mutation();
CREATE TRIGGER trg_stage6_penetration_approvals_no_delete BEFORE DELETE ON stage6_penetration_approvals
FOR EACH ROW EXECUTE FUNCTION reject_stage6_penetration_history_mutation();
CREATE TRIGGER trg_stage6_penetration_engagements_no_delete BEFORE DELETE ON stage6_penetration_engagements
FOR EACH ROW EXECUTE FUNCTION reject_stage6_penetration_history_mutation();

COMMENT ON TABLE stage6_penetration_engagements IS
  'Internal governance over exact third-party penetration receipts; approval does not authenticate the assessor, signatures, report, or test execution.';
