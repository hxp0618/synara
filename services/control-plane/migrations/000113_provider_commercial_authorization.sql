CREATE TABLE provider_commercial_authorizations (
  id UUID PRIMARY KEY,
  operator_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  authorization_key TEXT NOT NULL UNIQUE CHECK (authorization_key ~ '^[a-z0-9][a-z0-9._-]{2,119}$'),
  provider TEXT NOT NULL CHECK (provider IN ('codex', 'claude_agent')),
  provider_product TEXT NOT NULL CHECK (length(trim(provider_product)) BETWEEN 2 AND 200),
  account_type TEXT NOT NULL CHECK (length(trim(account_type)) BETWEEN 2 AND 200),
  contracting_entity TEXT NOT NULL CHECK (length(trim(contracting_entity)) BETWEEN 2 AND 300),
  credential_mode TEXT NOT NULL CHECK (credential_mode IN ('customer_byok', 'platform_managed')),
  allowed_credential_scopes JSONB NOT NULL CHECK (
    jsonb_typeof(allowed_credential_scopes) = 'array' AND jsonb_array_length(allowed_credential_scopes) BETWEEN 1 AND 4
  ),
  allowed_regions JSONB NOT NULL CHECK (
    jsonb_typeof(allowed_regions) = 'array' AND jsonb_array_length(allowed_regions) BETWEEN 1 AND 32
  ),
  data_use_policy TEXT NOT NULL CHECK (data_use_policy IN ('no_training', 'tenant_explicit_opt_in')),
  retention_policy TEXT NOT NULL CHECK (length(trim(retention_policy)) BETWEEN 3 AND 1000),
  terms_effective_at TIMESTAMPTZ NOT NULL,
  terms_reference TEXT NOT NULL CHECK (stage6_valid_https_reference(terms_reference)),
  agreement_reference TEXT NOT NULL CHECK (stage6_valid_https_reference(agreement_reference)),
  dpa_reference TEXT NOT NULL CHECK (stage6_valid_https_reference(dpa_reference)),
  prohibited_use_summary TEXT NOT NULL CHECK (length(trim(prohibited_use_summary)) BETWEEN 20 AND 4000),
  termination_runbook_reference TEXT NOT NULL CHECK (stage6_valid_https_reference(termination_runbook_reference)),
  review_expires_at TIMESTAMPTZ NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('draft', 'ready_for_review', 'active', 'rejected', 'revoked')),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  created_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  activated_at TIMESTAMPTZ,
  rejected_at TIMESTAMPTZ,
  revoked_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_provider_commercial_authorizations_lookup
  ON provider_commercial_authorizations (operator_tenant_id, provider, state, review_expires_at DESC);

CREATE TABLE provider_commercial_authorization_approvals (
  id UUID PRIMARY KEY,
  authorization_id UUID NOT NULL REFERENCES provider_commercial_authorizations(id) ON DELETE RESTRICT,
  operator_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  approval_role TEXT NOT NULL CHECK (approval_role IN ('legal', 'privacy', 'security', 'product')),
  decision TEXT NOT NULL CHECK (decision IN ('approved', 'rejected')),
  approver_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  reason TEXT NOT NULL CHECK (length(trim(reason)) BETWEEN 10 AND 2000),
  evidence_reference TEXT NOT NULL CHECK (stage6_valid_https_reference(evidence_reference)),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (authorization_id, approval_role),
  UNIQUE (authorization_id, approver_user_id)
);

CREATE INDEX idx_provider_commercial_authorization_approvals
  ON provider_commercial_authorization_approvals (authorization_id, created_at, id);

CREATE OR REPLACE FUNCTION enforce_provider_commercial_authorization()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  required_approvals INTEGER;
  scope_count INTEGER;
  region_count INTEGER;
BEGIN
  SELECT count(DISTINCT value) INTO scope_count FROM jsonb_array_elements_text(NEW.allowed_credential_scopes);
  SELECT count(DISTINCT value) INTO region_count FROM jsonb_array_elements_text(NEW.allowed_regions);
  IF scope_count <> jsonb_array_length(NEW.allowed_credential_scopes)
     OR EXISTS (SELECT 1 FROM jsonb_array_elements_text(NEW.allowed_credential_scopes) AS item(value)
                WHERE value NOT IN ('user', 'organization', 'tenant', 'platform'))
     OR region_count <> jsonb_array_length(NEW.allowed_regions)
     OR EXISTS (SELECT 1 FROM jsonb_array_elements_text(NEW.allowed_regions) AS item(value)
                WHERE value !~ '^[a-z0-9][a-z0-9._-]{1,63}$') THEN
    RAISE EXCEPTION 'Invalid Provider commercial authorization scope or Region set' USING ERRCODE = '23514';
  END IF;

  IF TG_OP = 'INSERT' THEN
    IF NEW.state <> 'draft' OR NEW.version <> 1
       OR NEW.activated_at IS NOT NULL OR NEW.rejected_at IS NOT NULL OR NEW.revoked_at IS NOT NULL
       OR NEW.review_expires_at <= NEW.created_at
       OR NEW.review_expires_at > NEW.created_at + interval '180 days'
       OR NOT stage6_compliance_active_operator(NEW.operator_tenant_id, NEW.created_by, ARRAY['owner', 'admin']) THEN
      RAISE EXCEPTION 'Invalid Provider commercial authorization initial state' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
  END IF;

  IF NEW.id IS DISTINCT FROM OLD.id
     OR NEW.operator_tenant_id IS DISTINCT FROM OLD.operator_tenant_id
     OR NEW.authorization_key IS DISTINCT FROM OLD.authorization_key
     OR NEW.provider IS DISTINCT FROM OLD.provider
     OR NEW.provider_product IS DISTINCT FROM OLD.provider_product
     OR NEW.account_type IS DISTINCT FROM OLD.account_type
     OR NEW.contracting_entity IS DISTINCT FROM OLD.contracting_entity
     OR NEW.credential_mode IS DISTINCT FROM OLD.credential_mode
     OR NEW.allowed_credential_scopes IS DISTINCT FROM OLD.allowed_credential_scopes
     OR NEW.allowed_regions IS DISTINCT FROM OLD.allowed_regions
     OR NEW.data_use_policy IS DISTINCT FROM OLD.data_use_policy
     OR NEW.retention_policy IS DISTINCT FROM OLD.retention_policy
     OR NEW.terms_effective_at IS DISTINCT FROM OLD.terms_effective_at
     OR NEW.terms_reference IS DISTINCT FROM OLD.terms_reference
     OR NEW.agreement_reference IS DISTINCT FROM OLD.agreement_reference
     OR NEW.dpa_reference IS DISTINCT FROM OLD.dpa_reference
     OR NEW.prohibited_use_summary IS DISTINCT FROM OLD.prohibited_use_summary
     OR NEW.termination_runbook_reference IS DISTINCT FROM OLD.termination_runbook_reference
     OR NEW.review_expires_at IS DISTINCT FROM OLD.review_expires_at
     OR NEW.created_by IS DISTINCT FROM OLD.created_by
     OR NEW.created_at IS DISTINCT FROM OLD.created_at
     OR NEW.version <> OLD.version + 1 THEN
    RAISE EXCEPTION 'Provider commercial authorization identity is immutable' USING ERRCODE = '23514';
  END IF;

  IF (OLD.state = 'draft' AND NEW.state <> 'ready_for_review')
     OR (OLD.state = 'ready_for_review' AND NEW.state NOT IN ('active', 'rejected'))
     OR (OLD.state = 'active' AND NEW.state <> 'revoked')
     OR OLD.state IN ('rejected', 'revoked') THEN
    RAISE EXCEPTION 'Invalid Provider commercial authorization transition' USING ERRCODE = '23514';
  END IF;

  IF NEW.state = 'active' THEN
    SELECT count(*) INTO required_approvals
    FROM provider_commercial_authorization_approvals
    WHERE authorization_id = NEW.id AND decision = 'approved'
      AND approval_role IN ('legal', 'privacy', 'security', 'product');
    IF required_approvals <> 4 OR NEW.activated_at IS NULL OR NEW.review_expires_at <= NEW.activated_at THEN
      RAISE EXCEPTION 'Provider commercial authorization requires four separated approvals and unexpired review' USING ERRCODE = '23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_provider_commercial_authorizations
BEFORE INSERT OR UPDATE ON provider_commercial_authorizations
FOR EACH ROW EXECUTE FUNCTION enforce_provider_commercial_authorization();

CREATE OR REPLACE FUNCTION enforce_provider_commercial_authorization_approval()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM provider_commercial_authorizations AS commercial_auth
    WHERE commercial_auth.id = NEW.authorization_id
      AND commercial_auth.operator_tenant_id = NEW.operator_tenant_id
      AND commercial_auth.state = 'ready_for_review'
      AND commercial_auth.created_by <> NEW.approver_user_id
      AND commercial_auth.review_expires_at > NEW.created_at
      AND stage6_compliance_active_operator(
        commercial_auth.operator_tenant_id, NEW.approver_user_id, ARRAY['owner', 'admin', 'security_admin']
      )
  ) THEN
    RAISE EXCEPTION 'Provider commercial approval requires separated active operator authority' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_provider_commercial_authorization_approvals_insert
BEFORE INSERT ON provider_commercial_authorization_approvals
FOR EACH ROW EXECUTE FUNCTION enforce_provider_commercial_authorization_approval();

CREATE OR REPLACE FUNCTION reject_provider_commercial_authorization_approval_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'Provider commercial authorization approvals are immutable' USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER trg_provider_commercial_authorization_approvals_no_update
BEFORE UPDATE ON provider_commercial_authorization_approvals
FOR EACH ROW EXECUTE FUNCTION reject_provider_commercial_authorization_approval_mutation();
CREATE TRIGGER trg_provider_commercial_authorization_approvals_no_delete
BEFORE DELETE ON provider_commercial_authorization_approvals
FOR EACH ROW EXECUTE FUNCTION reject_provider_commercial_authorization_approval_mutation();

CREATE TRIGGER trg_provider_commercial_authorizations_updated_at
BEFORE UPDATE ON provider_commercial_authorizations
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE provider_commercial_authorizations IS
  'Hosted Provider commercial-use authorization metadata; active records require separated review but do not validate external contract authority.';
