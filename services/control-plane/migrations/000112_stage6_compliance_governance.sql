CREATE OR REPLACE FUNCTION stage6_valid_https_reference(value TEXT)
RETURNS BOOLEAN
LANGUAGE sql
IMMUTABLE
AS $$
  SELECT value ~ '^https://[^/?#[:space:]@]+(/[^?#[:space:]]*)?$';
$$;

CREATE TABLE stage6_compliance_programs (
  id UUID PRIMARY KEY,
  operator_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  program_key TEXT NOT NULL UNIQUE CHECK (program_key ~ '^[a-z0-9][a-z0-9._-]{2,79}$'),
  framework TEXT NOT NULL CHECK (framework IN ('soc2_type2', 'iso27001')),
  scope_version TEXT NOT NULL CHECK (length(trim(scope_version)) BETWEEN 1 AND 80),
  scope_summary TEXT NOT NULL CHECK (length(trim(scope_summary)) BETWEEN 20 AND 4000),
  executive_sponsor_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  auditor_organization TEXT NOT NULL CHECK (length(trim(auditor_organization)) BETWEEN 2 AND 300),
  auditor_engagement_reference TEXT NOT NULL CHECK (length(trim(auditor_engagement_reference)) BETWEEN 8 AND 2048 AND stage6_valid_https_reference(auditor_engagement_reference)),
  observation_start TIMESTAMPTZ NOT NULL,
  observation_end TIMESTAMPTZ NOT NULL CHECK (observation_end > observation_start),
  evidence_repository_reference TEXT NOT NULL CHECK (length(trim(evidence_repository_reference)) BETWEEN 8 AND 2048 AND stage6_valid_https_reference(evidence_repository_reference)),
  evidence_access_policy_reference TEXT NOT NULL CHECK (length(trim(evidence_access_policy_reference)) BETWEEN 8 AND 2048 AND stage6_valid_https_reference(evidence_access_policy_reference)),
  evidence_retention_days INTEGER NOT NULL CHECK (evidence_retention_days BETWEEN 365 AND 3650),
  vendor_register_reference TEXT NOT NULL CHECK (length(trim(vendor_register_reference)) BETWEEN 8 AND 2048 AND stage6_valid_https_reference(vendor_register_reference)),
  risk_register_reference TEXT NOT NULL CHECK (length(trim(risk_register_reference)) BETWEEN 8 AND 2048 AND stage6_valid_https_reference(risk_register_reference)),
  state TEXT NOT NULL CHECK (state IN ('draft', 'ready_for_review', 'record_complete')),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  created_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  record_completed_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_stage6_compliance_programs_state
  ON stage6_compliance_programs (state, updated_at DESC, id);

CREATE TABLE stage6_compliance_controls (
  id UUID PRIMARY KEY,
  program_id UUID NOT NULL REFERENCES stage6_compliance_programs(id) ON DELETE RESTRICT,
  operator_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  control_id TEXT NOT NULL CHECK (control_id ~ '^[A-Z0-9][A-Z0-9._-]{1,79}$'),
  control_family TEXT NOT NULL CHECK (control_family IN (
    'logical_access', 'change_release', 'operations', 'data_governance',
    'resilience', 'vendor_provider', 'security_testing'
  )),
  title TEXT NOT NULL CHECK (length(trim(title)) BETWEEN 3 AND 200),
  description TEXT NOT NULL CHECK (length(trim(description)) BETWEEN 20 AND 4000),
  owner_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  cadence TEXT NOT NULL CHECK (cadence IN (
    'continuous', 'daily', 'monthly', 'quarterly', 'annual', 'per_release', 'per_incident'
  )),
  evidence_requirement TEXT NOT NULL CHECK (length(trim(evidence_requirement)) BETWEEN 20 AND 4000),
  created_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (program_id, control_id)
);

CREATE INDEX idx_stage6_compliance_controls_program
  ON stage6_compliance_controls (program_id, control_family, control_id);

CREATE TABLE stage6_compliance_evidence (
  id UUID PRIMARY KEY,
  program_id UUID NOT NULL REFERENCES stage6_compliance_programs(id) ON DELETE RESTRICT,
  control_record_id UUID NOT NULL REFERENCES stage6_compliance_controls(id) ON DELETE RESTRICT,
  operator_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  evidence_id TEXT NOT NULL CHECK (evidence_id ~ '^[A-Za-z0-9][A-Za-z0-9._-]{2,119}$'),
  evidence_type TEXT NOT NULL CHECK (evidence_type IN (
    'release_manifest', 'access_review', 'release', 'incident', 'recovery',
    'vendor_review', 'security_test', 'data_governance', 'other'
  )),
  period_start TIMESTAMPTZ NOT NULL,
  period_end TIMESTAMPTZ NOT NULL CHECK (period_end >= period_start),
  source_reference TEXT NOT NULL CHECK (length(trim(source_reference)) BETWEEN 8 AND 2048 AND stage6_valid_https_reference(source_reference)),
  sha256 BYTEA NOT NULL CHECK (octet_length(sha256) = 32),
  media_type TEXT NOT NULL CHECK (length(trim(media_type)) BETWEEN 3 AND 120),
  classification TEXT NOT NULL CHECK (classification IN ('internal', 'confidential', 'restricted')),
  collected_at TIMESTAMPTZ NOT NULL,
  retention_until TIMESTAMPTZ NOT NULL,
  submitted_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (program_id, evidence_id)
);

CREATE INDEX idx_stage6_compliance_evidence_control
  ON stage6_compliance_evidence (control_record_id, period_end DESC, id);

CREATE TABLE stage6_compliance_evidence_reviews (
  id UUID PRIMARY KEY,
  evidence_record_id UUID NOT NULL UNIQUE REFERENCES stage6_compliance_evidence(id) ON DELETE RESTRICT,
  program_id UUID NOT NULL REFERENCES stage6_compliance_programs(id) ON DELETE RESTRICT,
  operator_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  decision TEXT NOT NULL CHECK (decision IN ('accepted', 'rejected')),
  review_role TEXT NOT NULL CHECK (review_role IN ('security', 'operations', 'legal_privacy', 'auditor')),
  reviewer_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  reason TEXT NOT NULL CHECK (length(trim(reason)) BETWEEN 10 AND 2000),
  evidence_reference TEXT NOT NULL CHECK (length(trim(evidence_reference)) BETWEEN 8 AND 2048 AND stage6_valid_https_reference(evidence_reference)),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE stage6_compliance_program_decisions (
  id UUID PRIMARY KEY,
  program_id UUID NOT NULL REFERENCES stage6_compliance_programs(id) ON DELETE RESTRICT,
  operator_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  decision_role TEXT NOT NULL CHECK (decision_role IN ('security', 'operations', 'legal_privacy', 'executive')),
  decision TEXT NOT NULL CHECK (decision IN ('approved', 'rejected')),
  decider_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  reason TEXT NOT NULL CHECK (length(trim(reason)) BETWEEN 10 AND 2000),
  evidence_reference TEXT NOT NULL CHECK (length(trim(evidence_reference)) BETWEEN 8 AND 2048 AND stage6_valid_https_reference(evidence_reference)),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (program_id, decision_role),
  UNIQUE (program_id, decider_user_id)
);

CREATE OR REPLACE FUNCTION stage6_compliance_active_operator(
  expected_tenant UUID,
  expected_user UUID,
  allowed_roles TEXT[]
) RETURNS BOOLEAN
LANGUAGE sql
STABLE
AS $$
  SELECT EXISTS (
    SELECT 1
    FROM tenant_memberships AS membership
    JOIN tenants AS operator_tenant ON operator_tenant.id = membership.tenant_id
    JOIN users AS operator_user ON operator_user.id = membership.user_id
    WHERE membership.tenant_id = expected_tenant
      AND membership.user_id = expected_user
      AND membership.status = 'active'
      AND membership.role = ANY(allowed_roles)
      AND operator_tenant.status = 'active' AND operator_tenant.deleted_at IS NULL
      AND operator_user.status = 'active' AND operator_user.deleted_at IS NULL
  );
$$;

CREATE OR REPLACE FUNCTION enforce_stage6_compliance_program()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  family_count INTEGER;
  approval_count INTEGER;
  accepted_manifest_count INTEGER;
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.state <> 'draft' OR NEW.version <> 1 OR NEW.record_completed_at IS NOT NULL
       OR NOT stage6_compliance_active_operator(NEW.operator_tenant_id, NEW.created_by, ARRAY['owner', 'admin'])
       OR NOT stage6_compliance_active_operator(NEW.operator_tenant_id, NEW.executive_sponsor_user_id, ARRAY['owner', 'admin']) THEN
      RAISE EXCEPTION 'Invalid Stage 6 compliance program initial state' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
  END IF;

  IF NEW.id IS DISTINCT FROM OLD.id
     OR NEW.operator_tenant_id IS DISTINCT FROM OLD.operator_tenant_id
     OR NEW.program_key IS DISTINCT FROM OLD.program_key
     OR NEW.framework IS DISTINCT FROM OLD.framework
     OR NEW.scope_version IS DISTINCT FROM OLD.scope_version
     OR NEW.scope_summary IS DISTINCT FROM OLD.scope_summary
     OR NEW.executive_sponsor_user_id IS DISTINCT FROM OLD.executive_sponsor_user_id
     OR NEW.auditor_organization IS DISTINCT FROM OLD.auditor_organization
     OR NEW.auditor_engagement_reference IS DISTINCT FROM OLD.auditor_engagement_reference
     OR NEW.observation_start IS DISTINCT FROM OLD.observation_start
     OR NEW.observation_end IS DISTINCT FROM OLD.observation_end
     OR NEW.evidence_repository_reference IS DISTINCT FROM OLD.evidence_repository_reference
     OR NEW.evidence_access_policy_reference IS DISTINCT FROM OLD.evidence_access_policy_reference
     OR NEW.evidence_retention_days IS DISTINCT FROM OLD.evidence_retention_days
     OR NEW.vendor_register_reference IS DISTINCT FROM OLD.vendor_register_reference
     OR NEW.risk_register_reference IS DISTINCT FROM OLD.risk_register_reference
     OR NEW.created_by IS DISTINCT FROM OLD.created_by
     OR NEW.created_at IS DISTINCT FROM OLD.created_at
     OR NEW.version <> OLD.version + 1 THEN
    RAISE EXCEPTION 'Stage 6 compliance program identity and scope are immutable' USING ERRCODE = '23514';
  END IF;

  IF (OLD.state = 'draft' AND NEW.state <> 'ready_for_review')
     OR (OLD.state = 'ready_for_review' AND NEW.state <> 'record_complete')
     OR OLD.state = 'record_complete' THEN
    RAISE EXCEPTION 'Invalid Stage 6 compliance program transition' USING ERRCODE = '23514';
  END IF;

  IF NEW.state = 'record_complete' THEN
    SELECT count(DISTINCT control_family) INTO family_count
    FROM stage6_compliance_controls WHERE program_id = NEW.id;
    SELECT count(*) INTO approval_count
    FROM stage6_compliance_program_decisions
    WHERE program_id = NEW.id AND decision = 'approved'
      AND decision_role IN ('security', 'operations', 'legal_privacy', 'executive');
    SELECT count(*) INTO accepted_manifest_count
    FROM stage6_compliance_evidence AS evidence
    JOIN stage6_compliance_evidence_reviews AS review
      ON review.evidence_record_id = evidence.id AND review.decision = 'accepted'
    WHERE evidence.program_id = NEW.id AND evidence.evidence_type = 'release_manifest';
    IF family_count <> 7 OR approval_count <> 4 OR accepted_manifest_count < 1
       OR NEW.record_completed_at IS NULL THEN
      RAISE EXCEPTION 'Compliance record requires all control families, four decisions and accepted release manifest' USING ERRCODE = '23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_compliance_programs
BEFORE INSERT OR UPDATE ON stage6_compliance_programs
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_compliance_program();

CREATE OR REPLACE FUNCTION enforce_stage6_compliance_child_insert()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  parent_state TEXT;
  parent_tenant UUID;
  parent_retention INTEGER;
  actor UUID;
BEGIN
  SELECT state, operator_tenant_id, evidence_retention_days
    INTO parent_state, parent_tenant, parent_retention
  FROM stage6_compliance_programs WHERE id = NEW.program_id;
  IF parent_state IS NULL OR parent_state = 'record_complete' OR NEW.operator_tenant_id <> parent_tenant THEN
    RAISE EXCEPTION 'Compliance child record does not match an open program' USING ERRCODE = '23514';
  END IF;
  IF TG_TABLE_NAME = 'stage6_compliance_controls' THEN
    actor := NEW.created_by;
    IF NOT stage6_compliance_active_operator(parent_tenant, NEW.owner_user_id, ARRAY['owner', 'admin', 'security_admin']) THEN
      RAISE EXCEPTION 'Compliance control owner is not an active operator' USING ERRCODE = '23514';
    END IF;
  ELSE
    actor := NEW.submitted_by;
    IF NOT EXISTS (
      SELECT 1 FROM stage6_compliance_controls
      WHERE id = NEW.control_record_id AND program_id = NEW.program_id AND operator_tenant_id = NEW.operator_tenant_id
    ) OR NEW.retention_until < NEW.collected_at + make_interval(days => parent_retention) THEN
      RAISE EXCEPTION 'Compliance evidence subject or retention is invalid' USING ERRCODE = '23514';
    END IF;
  END IF;
  IF NOT stage6_compliance_active_operator(parent_tenant, actor, ARRAY['owner', 'admin', 'security_admin']) THEN
    RAISE EXCEPTION 'Compliance child record requires active operator authority' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_compliance_controls_insert
BEFORE INSERT ON stage6_compliance_controls
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_compliance_child_insert();

CREATE TRIGGER trg_stage6_compliance_evidence_insert
BEFORE INSERT ON stage6_compliance_evidence
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_compliance_child_insert();

CREATE OR REPLACE FUNCTION enforce_stage6_compliance_review_insert()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  evidence_submitter UUID;
  evidence_program UUID;
  evidence_tenant UUID;
  program_state TEXT;
BEGIN
  SELECT evidence.submitted_by, evidence.program_id, evidence.operator_tenant_id, program.state
    INTO evidence_submitter, evidence_program, evidence_tenant, program_state
  FROM stage6_compliance_evidence AS evidence
  JOIN stage6_compliance_programs AS program ON program.id = evidence.program_id
  WHERE evidence.id = NEW.evidence_record_id;
  IF evidence_program IS NULL OR NEW.program_id <> evidence_program OR NEW.operator_tenant_id <> evidence_tenant
     OR program_state = 'record_complete' OR NEW.reviewer_user_id = evidence_submitter
     OR NOT stage6_compliance_active_operator(evidence_tenant, NEW.reviewer_user_id, ARRAY['owner', 'admin', 'security_admin']) THEN
    RAISE EXCEPTION 'Compliance evidence review requires a separated active operator' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_compliance_evidence_reviews_insert
BEFORE INSERT ON stage6_compliance_evidence_reviews
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_compliance_review_insert();

CREATE OR REPLACE FUNCTION enforce_stage6_compliance_decision_insert()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  program_creator UUID;
  program_tenant UUID;
  program_state TEXT;
BEGIN
  SELECT created_by, operator_tenant_id, state INTO program_creator, program_tenant, program_state
  FROM stage6_compliance_programs WHERE id = NEW.program_id;
  IF program_creator IS NULL OR NEW.operator_tenant_id <> program_tenant
     OR program_state <> 'ready_for_review' OR NEW.decider_user_id = program_creator
     OR NOT stage6_compliance_active_operator(program_tenant, NEW.decider_user_id, ARRAY['owner', 'admin', 'security_admin']) THEN
    RAISE EXCEPTION 'Compliance decision requires review state and separated active operator' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_compliance_program_decisions_insert
BEFORE INSERT ON stage6_compliance_program_decisions
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_compliance_decision_insert();

CREATE OR REPLACE FUNCTION reject_stage6_compliance_append_only_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'Stage 6 compliance records are append-only' USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER trg_stage6_compliance_controls_no_update
BEFORE UPDATE OR DELETE ON stage6_compliance_controls
FOR EACH ROW EXECUTE FUNCTION reject_stage6_compliance_append_only_mutation();
CREATE TRIGGER trg_stage6_compliance_evidence_no_update
BEFORE UPDATE OR DELETE ON stage6_compliance_evidence
FOR EACH ROW EXECUTE FUNCTION reject_stage6_compliance_append_only_mutation();
CREATE TRIGGER trg_stage6_compliance_reviews_no_update
BEFORE UPDATE OR DELETE ON stage6_compliance_evidence_reviews
FOR EACH ROW EXECUTE FUNCTION reject_stage6_compliance_append_only_mutation();
CREATE TRIGGER trg_stage6_compliance_decisions_no_update
BEFORE UPDATE OR DELETE ON stage6_compliance_program_decisions
FOR EACH ROW EXECUTE FUNCTION reject_stage6_compliance_append_only_mutation();

CREATE TRIGGER trg_stage6_compliance_programs_updated_at
BEFORE UPDATE ON stage6_compliance_programs
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE stage6_compliance_programs IS
  'Platform compliance readiness register; record_complete means metadata completeness, never external audit activation or certification.';
