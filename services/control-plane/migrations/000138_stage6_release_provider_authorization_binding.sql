CREATE TABLE stage6_release_provider_authorization_bindings (
  candidate_record_id UUID NOT NULL REFERENCES stage6_release_candidates(id) ON DELETE RESTRICT,
  authorization_id UUID NOT NULL REFERENCES provider_commercial_authorizations(id) ON DELETE RESTRICT,
  operator_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  provider TEXT NOT NULL,
  authorization_key TEXT NOT NULL,
  authorization_version BIGINT NOT NULL CHECK (authorization_version > 0),
  bound_at TIMESTAMPTZ NOT NULL,
  PRIMARY KEY (candidate_record_id, authorization_id)
);

CREATE INDEX idx_stage6_release_provider_authorization_bindings_candidate
  ON stage6_release_provider_authorization_bindings (candidate_record_id, provider, authorization_id);

CREATE OR REPLACE FUNCTION enforce_stage6_release_provider_authorization_binding_insert()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  candidate stage6_release_candidates%ROWTYPE;
  commercial_authorization provider_commercial_authorizations%ROWTYPE;
  approval_count INTEGER;
  approval_role_count INTEGER;
BEGIN
  SELECT * INTO candidate
  FROM stage6_release_candidates
  WHERE id = NEW.candidate_record_id
  FOR UPDATE;

  SELECT * INTO commercial_authorization
  FROM provider_commercial_authorizations
  WHERE id = NEW.authorization_id
  FOR SHARE;

  SELECT count(*), count(DISTINCT approval_role)
  INTO approval_count, approval_role_count
  FROM provider_commercial_authorization_approvals
  WHERE authorization_id = NEW.authorization_id
    AND decision = 'approved'
    AND approval_role IN ('legal', 'privacy', 'security', 'product')
    AND evidence_sha256 IS NOT NULL
    AND evidence_sha256 <> 'sha256:' || repeat('0', 64);

  IF candidate.id IS NULL
     OR commercial_authorization.id IS NULL
     OR candidate.operator_tenant_id <> NEW.operator_tenant_id
     OR commercial_authorization.operator_tenant_id <> NEW.operator_tenant_id
     OR candidate.state <> 'draft'
     OR NOT (candidate.impact_domains ? 'provider_commercial')
     OR NEW.bound_at <> candidate.created_at
     OR commercial_authorization.state <> 'active'
     OR commercial_authorization.version <> NEW.authorization_version
     OR commercial_authorization.provider <> NEW.provider
     OR commercial_authorization.authorization_key <> NEW.authorization_key
     OR commercial_authorization.review_expires_at <= NEW.bound_at
     OR commercial_authorization.terms_sha256 IS NULL
     OR commercial_authorization.agreement_sha256 IS NULL
     OR commercial_authorization.dpa_sha256 IS NULL
     OR commercial_authorization.termination_runbook_sha256 IS NULL
     OR approval_count <> 4
     OR approval_role_count <> 4 THEN
    RAISE EXCEPTION 'Stage 6 release Provider authorization binding requires an active exact byte-bound authorization'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_release_provider_authorization_binding_insert
BEFORE INSERT ON stage6_release_provider_authorization_bindings
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_release_provider_authorization_binding_insert();

CREATE OR REPLACE FUNCTION reject_stage6_release_provider_authorization_binding_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'Stage 6 release Provider authorization bindings are immutable' USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER trg_stage6_release_provider_authorization_binding_update
BEFORE UPDATE ON stage6_release_provider_authorization_bindings
FOR EACH ROW EXECUTE FUNCTION reject_stage6_release_provider_authorization_binding_mutation();

CREATE TRIGGER trg_stage6_release_provider_authorization_binding_delete
BEFORE DELETE ON stage6_release_provider_authorization_bindings
FOR EACH ROW EXECUTE FUNCTION reject_stage6_release_provider_authorization_binding_mutation();

CREATE OR REPLACE FUNCTION enforce_stage6_release_candidate_provider_authorization_gate()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  binding_count INTEGER;
  invalid_binding_count INTEGER;
BEGIN
  IF NEW.state = OLD.state OR NEW.state IN ('rejected', 'rolled_back') THEN
    RETURN NEW;
  END IF;

  SELECT count(*), count(*) FILTER (
    WHERE auth_record.id IS NULL
       OR auth_record.operator_tenant_id <> binding.operator_tenant_id
       OR auth_record.state <> 'active'
       OR auth_record.version <> binding.authorization_version
       OR auth_record.provider <> binding.provider
       OR auth_record.authorization_key <> binding.authorization_key
       OR auth_record.review_expires_at <= NEW.updated_at
       OR auth_record.terms_sha256 IS NULL
       OR auth_record.agreement_sha256 IS NULL
       OR auth_record.dpa_sha256 IS NULL
       OR auth_record.termination_runbook_sha256 IS NULL
       OR 4 <> (
         SELECT count(*)
         FROM provider_commercial_authorization_approvals AS approval
         WHERE approval.authorization_id = binding.authorization_id
           AND approval.decision = 'approved'
           AND approval.approval_role IN ('legal', 'privacy', 'security', 'product')
           AND approval.evidence_sha256 IS NOT NULL
           AND approval.evidence_sha256 <> 'sha256:' || repeat('0', 64)
       )
  )
  INTO binding_count, invalid_binding_count
  FROM stage6_release_provider_authorization_bindings AS binding
  LEFT JOIN provider_commercial_authorizations AS auth_record
    ON auth_record.id = binding.authorization_id
  WHERE binding.candidate_record_id = NEW.id
    AND binding.operator_tenant_id = NEW.operator_tenant_id;

  IF NEW.impact_domains ? 'provider_commercial' THEN
    IF binding_count = 0 OR invalid_binding_count <> 0 THEN
      RAISE EXCEPTION 'Stage 6 release transition requires active exact Provider commercial authorization bindings'
        USING ERRCODE = '23514';
    END IF;
  ELSIF binding_count <> 0 THEN
    RAISE EXCEPTION 'Stage 6 release candidate without Provider commercial impact cannot carry Provider authorization bindings'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_release_candidate_provider_authorization_gate
BEFORE UPDATE ON stage6_release_candidates
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_release_candidate_provider_authorization_gate();

COMMENT ON TABLE stage6_release_provider_authorization_bindings IS
  'Immutable exact Provider commercial authorization versions consumed by a Stage 6 release candidate.';
