ALTER TABLE stage6_governance_authority_grants
  DROP CONSTRAINT IF EXISTS stage6_governance_authority_grants_authority_key_check;
ALTER TABLE stage6_governance_authority_grants
  ADD CONSTRAINT stage6_governance_authority_grants_authority_key_check CHECK (authority_key IN (
    'release.engineering', 'release.operations', 'release.security', 'release.product', 'release.privacy_legal',
    'compliance.security', 'compliance.operations', 'compliance.legal_privacy', 'compliance.executive',
    'compliance.evidence.security', 'compliance.evidence.operations', 'compliance.evidence.legal_privacy', 'compliance.evidence.auditor',
    'provider_commercial.legal', 'provider_commercial.privacy', 'provider_commercial.security', 'provider_commercial.product',
    'recovery.database', 'recovery.kms', 'recovery.operations', 'recovery.security', 'recovery.storage'
  ));

CREATE TABLE stage6_recovery_drills (
  id UUID PRIMARY KEY,
  operator_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  candidate_record_id UUID NOT NULL REFERENCES stage6_release_candidates(id) ON DELETE RESTRICT,
  drill_id UUID NOT NULL UNIQUE,
  receipt BYTEA NOT NULL CHECK (octet_length(receipt) BETWEEN 1 AND 524288),
  receipt_sha256 BYTEA NOT NULL CHECK (octet_length(receipt_sha256) = 32),
  receipt_size_bytes INTEGER NOT NULL CHECK (receipt_size_bytes = octet_length(receipt)),
  receipt_schema TEXT NOT NULL CHECK (receipt_schema = 'synara.recovery-drill-evidence-receipt.v2'),
  assessment TEXT NOT NULL CHECK (assessment = 'evidence-validated-not-control-passed'),
  candidate_binding_sha256 BYTEA NOT NULL CHECK (octet_length(candidate_binding_sha256) = 32),
  recovery_subject_sha256 BYTEA NOT NULL CHECK (octet_length(recovery_subject_sha256) = 32),
  started_at TIMESTAMPTZ NOT NULL,
  completed_at TIMESTAMPTZ NOT NULL,
  validated_at TIMESTAMPTZ NOT NULL,
  measurements_within_objectives BOOLEAN NOT NULL,
  all_restore_canaries_passed BOOLEAN NOT NULL,
  all_source_approvals_approved BOOLEAN NOT NULL,
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
  UNIQUE (candidate_record_id, drill_id)
);

CREATE INDEX idx_stage6_recovery_drills_candidate
  ON stage6_recovery_drills (candidate_record_id, created_at DESC, id);

CREATE TABLE stage6_recovery_components (
  id UUID PRIMARY KEY,
  recovery_drill_record_id UUID NOT NULL REFERENCES stage6_recovery_drills(id) ON DELETE RESTRICT,
  operator_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  component_key TEXT NOT NULL CHECK (component_key IN ('kms', 'object-storage', 'postgresql', 'queue')),
  profile TEXT NOT NULL,
  source_region TEXT NOT NULL,
  restore_region TEXT NOT NULL CHECK (restore_region <> source_region),
  measured_rpo_seconds DOUBLE PRECISION NOT NULL CHECK (measured_rpo_seconds >= 0),
  rpo_objective_seconds DOUBLE PRECISION NOT NULL CHECK (rpo_objective_seconds >= 0),
  measured_rto_seconds DOUBLE PRECISION NOT NULL CHECK (measured_rto_seconds >= 0),
  rto_objective_seconds DOUBLE PRECISION NOT NULL CHECK (rto_objective_seconds > 0),
  rpo_within_objective BOOLEAN NOT NULL,
  rto_within_objective BOOLEAN NOT NULL,
  restore_served_canary BOOLEAN NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (recovery_drill_record_id, component_key)
);

CREATE TABLE stage6_recovery_approvals (
  id UUID PRIMARY KEY,
  recovery_drill_record_id UUID NOT NULL REFERENCES stage6_recovery_drills(id) ON DELETE RESTRICT,
  operator_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  approval_role TEXT NOT NULL CHECK (approval_role IN ('database', 'kms', 'operations', 'security', 'storage')),
  decision TEXT NOT NULL CHECK (decision IN ('approved', 'rejected')),
  approver_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  reason TEXT NOT NULL CHECK (length(btrim(reason)) BETWEEN 20 AND 2000),
  evidence_reference TEXT NOT NULL CHECK (stage6_valid_https_reference(evidence_reference)),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (recovery_drill_record_id, approval_role),
  UNIQUE (recovery_drill_record_id, approver_user_id)
);

CREATE OR REPLACE FUNCTION enforce_stage6_recovery_drill()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  receipt_json JSONB;
  candidate_receipt JSONB;
  expected_candidate JSONB;
  candidate stage6_release_candidates%ROWTYPE;
  approval_count INTEGER;
BEGIN
  IF TG_OP = 'INSERT' THEN
    receipt_json := convert_from(NEW.receipt, 'UTF8')::jsonb;
    SELECT * INTO candidate FROM stage6_release_candidates WHERE id = NEW.candidate_record_id;
    candidate_receipt := convert_from(candidate.evidence_bundle_receipt, 'UTF8')::jsonb;
    expected_candidate := candidate_receipt -> 'candidate';
    expected_candidate := expected_candidate - 'desktopArtifactSetSha256'::text;
    IF candidate.id IS NULL OR candidate.operator_tenant_id <> NEW.operator_tenant_id
       OR candidate.created_by <> NEW.created_by
       OR digest(NEW.receipt, 'sha256') <> NEW.receipt_sha256
       OR jsonb_typeof(receipt_json) <> 'object'
       OR (SELECT count(*) FROM jsonb_object_keys(receipt_json)) <> 20
       OR receipt_json->>'schemaVersion' <> NEW.receipt_schema
       OR receipt_json->>'assessment' <> NEW.assessment
       OR (receipt_json->>'drillId')::uuid <> NEW.drill_id
       OR decode(substring(receipt_json->>'candidateBindingSha256' FROM 8), 'hex') <> NEW.candidate_binding_sha256
       OR decode(substring(receipt_json->>'recoverySubjectSha256' FROM 8), 'hex') <> NEW.recovery_subject_sha256
       OR (receipt_json->>'startedAt')::timestamptz <> NEW.started_at
       OR (receipt_json->>'completedAt')::timestamptz <> NEW.completed_at
       OR (receipt_json->>'validatedAt')::timestamptz <> NEW.validated_at
       OR NEW.completed_at <= NEW.started_at OR NEW.validated_at < NEW.completed_at
       OR NEW.validated_at > NEW.completed_at + interval '7 days'
       OR (receipt_json->>'declaredMeasurementsWithinObjectives')::boolean IS DISTINCT FROM NEW.measurements_within_objectives
       OR (receipt_json->>'allRestoreCanariesPassed')::boolean IS DISTINCT FROM NEW.all_restore_canaries_passed
       OR (receipt_json->>'allRequiredApprovalsApproved')::boolean IS DISTINCT FROM NEW.all_source_approvals_approved
       OR (receipt_json->>'eligibleForHumanGateReview')::boolean IS DISTINCT FROM NEW.eligible_for_human_gate_review
       OR (receipt_json->'verificationBoundary'->>'cryptographicSignaturesVerified')::boolean IS DISTINCT FROM NEW.cryptographic_signatures_verified
       OR (receipt_json->'verificationBoundary'->>'realBackupRestoreAndApproverAuthorityVerificationRequired')::boolean IS DISTINCT FROM NEW.external_authority_verification_required
       OR NEW.cryptographic_signatures_verified OR NOT NEW.external_authority_verification_required
       OR jsonb_typeof(receipt_json->'components') <> 'object'
       OR (SELECT count(*) FROM jsonb_object_keys(receipt_json->'components')) <> 4
       OR jsonb_typeof(receipt_json->'approvals') <> 'object'
       OR (SELECT count(*) FROM jsonb_object_keys(receipt_json->'approvals')) <> 5
       OR receipt_json->('candidate'::text) <> expected_candidate
       OR receipt_json->('restoredReleaseIdentity'::text) <> jsonb_build_object(
         'sourceCommit', receipt_json->('candidate'::text)->('sourceCommit'::text),
         'lockfileSha256', receipt_json->('candidate'::text)->('lockfileSha256'::text),
         'artifacts', receipt_json->('candidate'::text)->('artifacts'::text),
         'migrationTail', receipt_json->('candidate'::text)->('migrationTail'::text)
       )
       OR NEW.state <> 'recorded' OR NEW.version <> 1
       OR NEW.approved_at IS NOT NULL OR NEW.rejected_at IS NOT NULL THEN
      RAISE EXCEPTION 'Invalid Stage 6 Recovery drill receipt binding' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
  END IF;

  IF NEW.id IS DISTINCT FROM OLD.id OR NEW.operator_tenant_id IS DISTINCT FROM OLD.operator_tenant_id
     OR NEW.candidate_record_id IS DISTINCT FROM OLD.candidate_record_id OR NEW.drill_id IS DISTINCT FROM OLD.drill_id
     OR NEW.receipt IS DISTINCT FROM OLD.receipt OR NEW.receipt_sha256 IS DISTINCT FROM OLD.receipt_sha256
     OR NEW.receipt_size_bytes IS DISTINCT FROM OLD.receipt_size_bytes OR NEW.receipt_schema IS DISTINCT FROM OLD.receipt_schema
     OR NEW.assessment IS DISTINCT FROM OLD.assessment OR NEW.candidate_binding_sha256 IS DISTINCT FROM OLD.candidate_binding_sha256
     OR NEW.recovery_subject_sha256 IS DISTINCT FROM OLD.recovery_subject_sha256 OR NEW.started_at IS DISTINCT FROM OLD.started_at
     OR NEW.completed_at IS DISTINCT FROM OLD.completed_at OR NEW.validated_at IS DISTINCT FROM OLD.validated_at
     OR NEW.measurements_within_objectives IS DISTINCT FROM OLD.measurements_within_objectives
     OR NEW.all_restore_canaries_passed IS DISTINCT FROM OLD.all_restore_canaries_passed
     OR NEW.all_source_approvals_approved IS DISTINCT FROM OLD.all_source_approvals_approved
     OR NEW.eligible_for_human_gate_review IS DISTINCT FROM OLD.eligible_for_human_gate_review
     OR NEW.cryptographic_signatures_verified IS DISTINCT FROM OLD.cryptographic_signatures_verified
     OR NEW.external_authority_verification_required IS DISTINCT FROM OLD.external_authority_verification_required
     OR NEW.created_by IS DISTINCT FROM OLD.created_by OR NEW.created_at IS DISTINCT FROM OLD.created_at
     OR NEW.version <> OLD.version + 1 OR OLD.state <> 'recorded' OR NEW.state NOT IN ('approved', 'rejected') THEN
    RAISE EXCEPTION 'Stage 6 Recovery identity and evidence are immutable' USING ERRCODE = '23514';
  END IF;
  IF NEW.state = 'approved' THEN
    SELECT count(*) INTO approval_count FROM stage6_recovery_approvals
    WHERE recovery_drill_record_id = NEW.id AND decision = 'approved';
    IF NOT NEW.eligible_for_human_gate_review OR approval_count <> 5
       OR NEW.approved_at IS NULL OR NEW.rejected_at IS NOT NULL THEN
      RAISE EXCEPTION 'Stage 6 Recovery approval gate is incomplete' USING ERRCODE = '23514';
    END IF;
  ELSIF NEW.rejected_at IS NULL OR NEW.approved_at IS NOT NULL THEN
    RAISE EXCEPTION 'Stage 6 Recovery rejection timestamp is required' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_recovery_drills_guard
BEFORE INSERT OR UPDATE ON stage6_recovery_drills
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_recovery_drill();

CREATE OR REPLACE FUNCTION enforce_stage6_recovery_component()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  parent stage6_recovery_drills%ROWTYPE;
  component JSONB;
BEGIN
  SELECT * INTO parent FROM stage6_recovery_drills WHERE id = NEW.recovery_drill_record_id FOR UPDATE;
  component := convert_from(parent.receipt, 'UTF8')::jsonb->'components'->NEW.component_key;
  IF parent.id IS NULL OR parent.operator_tenant_id <> NEW.operator_tenant_id OR parent.state <> 'recorded'
     OR component IS NULL OR component->>'profile' <> NEW.profile
     OR component->>'sourceRegion' <> NEW.source_region OR component->>'restoreRegion' <> NEW.restore_region
     OR (component->>'measuredRpoSeconds')::double precision <> NEW.measured_rpo_seconds
     OR (component->>'rpoObjectiveSeconds')::double precision <> NEW.rpo_objective_seconds
     OR (component->>'measuredRtoSeconds')::double precision <> NEW.measured_rto_seconds
     OR (component->>'rtoObjectiveSeconds')::double precision <> NEW.rto_objective_seconds
     OR (component->>'rpoWithinObjective')::boolean IS DISTINCT FROM NEW.rpo_within_objective
     OR (component->>'rtoWithinObjective')::boolean IS DISTINCT FROM NEW.rto_within_objective
     OR (component->>'restoreServedCanary')::boolean IS DISTINCT FROM NEW.restore_served_canary THEN
    RAISE EXCEPTION 'Invalid Stage 6 Recovery component projection' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_recovery_components_insert
BEFORE INSERT ON stage6_recovery_components
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_recovery_component();

CREATE OR REPLACE FUNCTION enforce_stage6_recovery_approval()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  parent stage6_recovery_drills%ROWTYPE;
BEGIN
  SELECT * INTO parent FROM stage6_recovery_drills WHERE id = NEW.recovery_drill_record_id FOR UPDATE;
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
         AND authority.authority_key = 'recovery.' || NEW.approval_role
         AND authority.status = 'active' AND authority.expires_at > statement_timestamp()
         AND membership.status = 'active' AND membership.role IN ('owner', 'admin', 'security_admin')
         AND operator_tenant.status = 'active' AND operator_tenant.deleted_at IS NULL
         AND governed_user.status = 'active' AND governed_user.deleted_at IS NULL
     ) THEN
    RAISE EXCEPTION 'Invalid Stage 6 Recovery approval authority' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_recovery_approvals_insert
BEFORE INSERT ON stage6_recovery_approvals
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_recovery_approval();

CREATE OR REPLACE FUNCTION reject_stage6_recovery_history_mutation()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'Stage 6 Recovery evidence history is immutable' USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER trg_stage6_recovery_components_no_update BEFORE UPDATE ON stage6_recovery_components
FOR EACH ROW EXECUTE FUNCTION reject_stage6_recovery_history_mutation();
CREATE TRIGGER trg_stage6_recovery_components_no_delete BEFORE DELETE ON stage6_recovery_components
FOR EACH ROW EXECUTE FUNCTION reject_stage6_recovery_history_mutation();
CREATE TRIGGER trg_stage6_recovery_approvals_no_update BEFORE UPDATE ON stage6_recovery_approvals
FOR EACH ROW EXECUTE FUNCTION reject_stage6_recovery_history_mutation();
CREATE TRIGGER trg_stage6_recovery_approvals_no_delete BEFORE DELETE ON stage6_recovery_approvals
FOR EACH ROW EXECUTE FUNCTION reject_stage6_recovery_history_mutation();
CREATE TRIGGER trg_stage6_recovery_drills_no_delete BEFORE DELETE ON stage6_recovery_drills
FOR EACH ROW EXECUTE FUNCTION reject_stage6_recovery_history_mutation();
