DROP TRIGGER IF EXISTS trg_provider_commercial_authorizations ON provider_commercial_authorizations;
DROP TRIGGER IF EXISTS trg_provider_commercial_authorization_approvals_insert ON provider_commercial_authorization_approvals;

ALTER TABLE provider_commercial_authorization_approvals
  ADD COLUMN evidence_sha256 TEXT,
  ADD CONSTRAINT provider_commercial_approval_evidence_sha256_shape CHECK (
    evidence_sha256 IS NULL OR evidence_sha256 ~ '^sha256:[0-9a-f]{64}$'
  );

-- URL-only decisions remain immutable history, but cannot support an active
-- authorization after approval evidence becomes byte-bound.
UPDATE provider_commercial_authorizations AS auth_record
SET state = 'revoked',
    version = auth_record.version + 1,
    revoked_at = COALESCE(auth_record.revoked_at, now()),
    updated_at = now()
WHERE auth_record.state NOT IN ('rejected', 'revoked')
  AND EXISTS (
    SELECT 1
    FROM provider_commercial_authorization_approvals AS approval
    WHERE approval.authorization_id = auth_record.id
      AND approval.evidence_sha256 IS NULL
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
      AND approval_role IN ('legal', 'privacy', 'security', 'product')
      AND evidence_sha256 IS NOT NULL;
    IF required_approvals <> 4 OR NEW.activated_at IS NULL OR NEW.review_expires_at <= NEW.activated_at
       OR NEW.terms_sha256 IS NULL OR NEW.agreement_sha256 IS NULL OR NEW.dpa_sha256 IS NULL
       OR NEW.termination_runbook_sha256 IS NULL THEN
      RAISE EXCEPTION 'Provider commercial authorization requires byte-bound documents and approvals, separated decisions and unexpired review' USING ERRCODE = '23514';
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
  IF NEW.evidence_sha256 IS NULL
     OR NEW.evidence_sha256 = 'sha256:' || repeat('0', 64)
     OR NOT EXISTS (
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
    RAISE EXCEPTION 'Provider commercial approval requires byte-bound evidence and separated active operator authority' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_provider_commercial_authorization_approvals_insert
BEFORE INSERT ON provider_commercial_authorization_approvals
FOR EACH ROW EXECUTE FUNCTION enforce_provider_commercial_authorization_approval();

COMMENT ON COLUMN provider_commercial_authorization_approvals.evidence_sha256 IS
  'SHA-256 of the exact external approval evidence bytes reviewed for this immutable decision.';
