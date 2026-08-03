DROP TRIGGER IF EXISTS trg_provider_commercial_authorizations ON provider_commercial_authorizations;

ALTER TABLE provider_commercial_authorizations
  ADD COLUMN terms_sha256 TEXT,
  ADD COLUMN agreement_sha256 TEXT,
  ADD COLUMN dpa_sha256 TEXT,
  ADD COLUMN termination_runbook_sha256 TEXT,
  ADD CONSTRAINT provider_commercial_terms_sha256_shape CHECK (
    terms_sha256 IS NULL OR terms_sha256 ~ '^sha256:[0-9a-f]{64}$'
  ),
  ADD CONSTRAINT provider_commercial_agreement_sha256_shape CHECK (
    agreement_sha256 IS NULL OR agreement_sha256 ~ '^sha256:[0-9a-f]{64}$'
  ),
  ADD CONSTRAINT provider_commercial_dpa_sha256_shape CHECK (
    dpa_sha256 IS NULL OR dpa_sha256 ~ '^sha256:[0-9a-f]{64}$'
  ),
  ADD CONSTRAINT provider_commercial_termination_runbook_sha256_shape CHECK (
    termination_runbook_sha256 IS NULL OR termination_runbook_sha256 ~ '^sha256:[0-9a-f]{64}$'
  );

-- Existing authorizations approved only a mutable URL. They remain visible as
-- historical records but cannot authorize a new hosted claim after this migration.
UPDATE provider_commercial_authorizations
SET state = 'revoked',
    version = version + 1,
    revoked_at = COALESCE(revoked_at, now()),
    updated_at = now()
WHERE state NOT IN ('rejected', 'revoked')
  AND (
    terms_sha256 IS NULL OR agreement_sha256 IS NULL OR dpa_sha256 IS NULL
    OR termination_runbook_sha256 IS NULL
  );

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
       OR NEW.terms_sha256 IS NULL OR NEW.terms_sha256 = 'sha256:' || repeat('0', 64)
       OR NEW.agreement_sha256 IS NULL OR NEW.agreement_sha256 = 'sha256:' || repeat('0', 64)
       OR NEW.dpa_sha256 IS NULL OR NEW.dpa_sha256 = 'sha256:' || repeat('0', 64)
       OR NEW.termination_runbook_sha256 IS NULL
       OR NEW.termination_runbook_sha256 = 'sha256:' || repeat('0', 64)
       OR NOT stage6_compliance_active_operator(NEW.operator_tenant_id, NEW.created_by, ARRAY['owner', 'admin']) THEN
      RAISE EXCEPTION 'Invalid Provider commercial authorization initial state or document digest' USING ERRCODE = '23514';
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
     OR NEW.terms_sha256 IS DISTINCT FROM OLD.terms_sha256
     OR NEW.agreement_reference IS DISTINCT FROM OLD.agreement_reference
     OR NEW.agreement_sha256 IS DISTINCT FROM OLD.agreement_sha256
     OR NEW.dpa_reference IS DISTINCT FROM OLD.dpa_reference
     OR NEW.dpa_sha256 IS DISTINCT FROM OLD.dpa_sha256
     OR NEW.prohibited_use_summary IS DISTINCT FROM OLD.prohibited_use_summary
     OR NEW.termination_runbook_reference IS DISTINCT FROM OLD.termination_runbook_reference
     OR NEW.termination_runbook_sha256 IS DISTINCT FROM OLD.termination_runbook_sha256
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
    IF required_approvals <> 4 OR NEW.activated_at IS NULL OR NEW.review_expires_at <= NEW.activated_at
       OR NEW.terms_sha256 IS NULL OR NEW.agreement_sha256 IS NULL OR NEW.dpa_sha256 IS NULL
       OR NEW.termination_runbook_sha256 IS NULL THEN
      RAISE EXCEPTION 'Provider commercial authorization requires bound documents, four separated approvals and unexpired review' USING ERRCODE = '23514';
    END IF;
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_provider_commercial_authorizations
BEFORE INSERT OR UPDATE ON provider_commercial_authorizations
FOR EACH ROW EXECUTE FUNCTION enforce_provider_commercial_authorization();

COMMENT ON COLUMN provider_commercial_authorizations.terms_sha256 IS
  'SHA-256 of the exact Provider terms bytes reviewed by the authorization.';
COMMENT ON COLUMN provider_commercial_authorizations.agreement_sha256 IS
  'SHA-256 of the exact executed agreement or Order Form bytes reviewed by the authorization.';
COMMENT ON COLUMN provider_commercial_authorizations.dpa_sha256 IS
  'SHA-256 of the exact DPA bytes reviewed by the authorization.';
COMMENT ON COLUMN provider_commercial_authorizations.termination_runbook_sha256 IS
  'SHA-256 of the exact termination runbook bytes reviewed by the authorization.';
