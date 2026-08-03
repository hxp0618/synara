ALTER TABLE stage6_governance_authority_grants
  DROP CONSTRAINT IF EXISTS stage6_governance_authority_grants_authority_key_check;
ALTER TABLE stage6_governance_authority_grants
  ADD CONSTRAINT stage6_governance_authority_grants_authority_key_check CHECK (authority_key IN (
    'release.engineering', 'release.operations', 'release.security', 'release.product', 'release.privacy_legal',
    'compliance.security', 'compliance.operations', 'compliance.legal_privacy', 'compliance.executive',
    'compliance.evidence.security', 'compliance.evidence.operations', 'compliance.evidence.legal_privacy', 'compliance.evidence.auditor',
    'provider_commercial.legal', 'provider_commercial.privacy', 'provider_commercial.security', 'provider_commercial.product',
    'recovery.database', 'recovery.kms', 'recovery.operations', 'recovery.security', 'recovery.storage',
    'penetration.engineering', 'penetration.product', 'penetration.security',
    'capacity.engineering', 'capacity.operations',
    'incident_exercise.operations', 'incident_exercise.communications',
    'operations_exercise.operations', 'operations_exercise.security',
    'billing_exercise.finance', 'billing_exercise.security', 'billing_exercise.release',
    'internal_cost.operations', 'internal_cost.owner'
  ));

CREATE TABLE stage6_internal_cost_reviews (
  id UUID PRIMARY KEY,
  operator_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  candidate_record_id UUID NOT NULL REFERENCES stage6_release_candidates(id) ON DELETE RESTRICT,
  receipt BYTEA NOT NULL CHECK (octet_length(receipt) BETWEEN 1 AND 2097152),
  receipt_sha256 BYTEA NOT NULL CHECK (octet_length(receipt_sha256) = 32),
  receipt_size_bytes BIGINT NOT NULL CHECK (receipt_size_bytes > 0),
  receipt_schema TEXT NOT NULL,
  assessment TEXT NOT NULL,
  release_commit TEXT NOT NULL CHECK (length(release_commit) = 40),
  environment_class TEXT NOT NULL CHECK (environment_class IN ('production', 'production-like')),
  environment_id TEXT NOT NULL CHECK (length(environment_id) BETWEEN 2 AND 200),
  manifest_sha256 BYTEA NOT NULL CHECK (octet_length(manifest_sha256) = 32),
  control_plane_origin TEXT NOT NULL CHECK (length(control_plane_origin) BETWEEN 8 AND 512),
  migration_name TEXT NOT NULL,
  migration_sha256 BYTEA NOT NULL CHECK (octet_length(migration_sha256) = 32),
  period_start TIMESTAMPTZ NOT NULL,
  period_end TIMESTAMPTZ NOT NULL,
  validated_at TIMESTAMPTZ NOT NULL,
  execution_count BIGINT NOT NULL CHECK (execution_count > 0),
  input_tokens BIGINT NOT NULL CHECK (input_tokens >= 0),
  output_tokens BIGINT NOT NULL CHECK (output_tokens >= 0),
  cached_input_tokens BIGINT NOT NULL CHECK (cached_input_tokens >= 0),
  cache_creation_input_tokens BIGINT NOT NULL CHECK (cache_creation_input_tokens >= 0),
  provider_cost_reported_execution_count BIGINT NOT NULL CHECK (provider_cost_reported_execution_count >= 0),
  provider_cost_unavailable_execution_count BIGINT NOT NULL CHECK (provider_cost_unavailable_execution_count >= 0),
  actual_platform_allocation_count BIGINT NOT NULL CHECK (actual_platform_allocation_count >= 0),
  estimated_platform_allocation_count BIGINT NOT NULL CHECK (estimated_platform_allocation_count >= 0),
  evidence_file_count BIGINT NOT NULL CHECK (evidence_file_count = 4),
  token_totals_reconciled BOOLEAN NOT NULL,
  provider_coverage_complete BOOLEAN NOT NULL,
  actual_overrides_estimate BOOLEAN NOT NULL,
  currency_safe_aggregation BOOLEAN NOT NULL,
  tenant_isolation_validated BOOLEAN NOT NULL,
  no_payment_data_present BOOLEAN NOT NULL,
  eligible_for_human_gate_review BOOLEAN NOT NULL,
  cryptographic_signatures_verified BOOLEAN NOT NULL,
  external_source_authority_verification_required BOOLEAN NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('recorded', 'approved', 'rejected')),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  created_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  approved_at TIMESTAMPTZ,
  rejected_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (candidate_record_id, receipt_sha256),
  CHECK (period_start < period_end AND period_end - period_start <= interval '93 days'),
  CHECK (provider_cost_reported_execution_count + provider_cost_unavailable_execution_count = execution_count),
  CHECK (actual_platform_allocation_count + estimated_platform_allocation_count = execution_count)
);

CREATE INDEX idx_stage6_internal_cost_reviews_candidate
  ON stage6_internal_cost_reviews (candidate_record_id, created_at DESC, id);

CREATE TABLE stage6_internal_cost_review_approvals (
  id UUID PRIMARY KEY,
  internal_cost_review_id UUID NOT NULL REFERENCES stage6_internal_cost_reviews(id) ON DELETE RESTRICT,
  operator_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  approval_role TEXT NOT NULL CHECK (approval_role IN ('operations', 'owner')),
  decision TEXT NOT NULL CHECK (decision IN ('approved', 'rejected')),
  approver_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  reason TEXT NOT NULL CHECK (length(trim(reason)) BETWEEN 20 AND 2000),
  evidence_reference TEXT NOT NULL CHECK (length(trim(evidence_reference)) BETWEEN 8 AND 2048),
  evidence_sha256 TEXT NOT NULL CHECK (
    evidence_sha256 ~ '^sha256:[0-9a-f]{64}$'
    AND evidence_sha256 <> 'sha256:' || repeat('0', 64)
  ),
  superseded_at TIMESTAMPTZ,
  superseded_reason TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CHECK ((superseded_at IS NULL) = (superseded_reason IS NULL))
);

CREATE UNIQUE INDEX uq_stage6_internal_cost_approvals_role_active
  ON stage6_internal_cost_review_approvals (internal_cost_review_id, approval_role)
  WHERE superseded_at IS NULL;
CREATE UNIQUE INDEX uq_stage6_internal_cost_approvals_user_active
  ON stage6_internal_cost_review_approvals (internal_cost_review_id, approver_user_id)
  WHERE superseded_at IS NULL;

CREATE OR REPLACE FUNCTION stage6_internal_cost_maps_valid(costs JSONB)
RETURNS BOOLEAN
LANGUAGE plpgsql
IMMUTABLE
AS $$
DECLARE
  item RECORD;
  currency_code TEXT;
  provider_amount NUMERIC;
  platform_amount NUMERIC;
  known_amount NUMERIC;
BEGIN
  IF jsonb_typeof(costs) <> 'object'
     OR jsonb_typeof(costs->'providerByCurrency') <> 'object'
     OR jsonb_typeof(costs->'platformByCurrency') <> 'object'
     OR jsonb_typeof(costs->'knownByCurrency') <> 'object'
     OR (SELECT count(*) FROM jsonb_object_keys(costs)) <> 3
     OR NOT (costs ?& ARRAY['providerByCurrency', 'platformByCurrency', 'knownByCurrency'])
     OR (SELECT count(*) FROM jsonb_object_keys(costs->'providerByCurrency')) = 0
     OR (SELECT count(*) FROM jsonb_object_keys(costs->'platformByCurrency')) = 0
     OR (SELECT count(*) FROM jsonb_object_keys(costs->'knownByCurrency')) = 0 THEN
    RETURN FALSE;
  END IF;
  FOR item IN
    SELECT key, value #>> '{}' AS amount FROM jsonb_each(costs->'providerByCurrency')
    UNION ALL SELECT key, value #>> '{}' FROM jsonb_each(costs->'platformByCurrency')
    UNION ALL SELECT key, value #>> '{}' FROM jsonb_each(costs->'knownByCurrency')
  LOOP
    IF item.key !~ '^[A-Z]{3}$' OR item.amount !~ '^[0-9]+$' THEN
      RETURN FALSE;
    END IF;
  END LOOP;
  FOR currency_code IN
    SELECT key FROM jsonb_object_keys(costs->'providerByCurrency') AS key
    UNION SELECT key FROM jsonb_object_keys(costs->'platformByCurrency') AS key
    UNION SELECT key FROM jsonb_object_keys(costs->'knownByCurrency') AS key
  LOOP
    IF NOT (costs->'knownByCurrency' ? currency_code) THEN
      RETURN FALSE;
    END IF;
    provider_amount := COALESCE((costs->'providerByCurrency'->>currency_code)::numeric, 0);
    platform_amount := COALESCE((costs->'platformByCurrency'->>currency_code)::numeric, 0);
    known_amount := (costs->'knownByCurrency'->>currency_code)::numeric;
    IF known_amount <> provider_amount + platform_amount THEN
      RETURN FALSE;
    END IF;
  END LOOP;
  RETURN TRUE;
EXCEPTION WHEN OTHERS THEN
  RETURN FALSE;
END;
$$;

CREATE OR REPLACE FUNCTION enforce_stage6_internal_cost_review()
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
       OR NEW.receipt_schema <> 'synara.stage6-internal-cost-evidence-validation.v1'
       OR NEW.assessment <> 'evidence-validated-not-internal-cost-approved'
       OR jsonb_typeof(receipt_json) <> 'object'
       OR (SELECT count(*) FROM jsonb_object_keys(receipt_json)) <> 14
       OR NOT (receipt_json ?& ARRAY[
         'assessment', 'candidate', 'controls', 'costs', 'cryptographicSignaturesVerified',
         'eligibleForHumanGateReview', 'evidence', 'evidenceFileCount',
         'externalSourceAndAuthorityVerificationRequired', 'manifest', 'period',
         'schemaVersion', 'usage', 'validatedAt'
       ])
       OR receipt_json->>'schemaVersion' <> NEW.receipt_schema
       OR receipt_json->>'assessment' <> NEW.assessment
       OR receipt_json #>> '{candidate,candidateId}' IS NULL
       OR receipt_json #>> '{candidate,sourceCommit}' <> NEW.release_commit
       OR receipt_json #>> '{candidate,environment}' <> NEW.environment_class
       OR receipt_json #>> '{candidate,environmentId}' <> NEW.environment_id
       OR receipt_json #>> '{candidate,controlPlaneBaseUrl}' <> NEW.control_plane_origin
       OR receipt_json #>> '{candidate,migrationTail,name}' <> NEW.migration_name
       OR receipt_json #>> '{candidate,migrationTail,sha256}' <> 'sha256:' || encode(NEW.migration_sha256, 'hex')
       OR receipt_json #>> '{manifest,sha256}' <> 'sha256:' || encode(NEW.manifest_sha256, 'hex')
       OR (receipt_json #>> '{period,start}')::timestamptz <> NEW.period_start
       OR (receipt_json #>> '{period,end}')::timestamptz <> NEW.period_end
       OR (receipt_json->>'validatedAt')::timestamptz <> NEW.validated_at
       OR (receipt_json #>> '{usage,executionCount}')::bigint <> NEW.execution_count
       OR (receipt_json #>> '{usage,inputTokens}')::bigint <> NEW.input_tokens
       OR (receipt_json #>> '{usage,outputTokens}')::bigint <> NEW.output_tokens
       OR (receipt_json #>> '{usage,cachedInputTokens}')::bigint <> NEW.cached_input_tokens
       OR (receipt_json #>> '{usage,cacheCreationInputTokens}')::bigint <> NEW.cache_creation_input_tokens
       OR (receipt_json #>> '{usage,providerCostReportedExecutionCount}')::bigint <> NEW.provider_cost_reported_execution_count
       OR (receipt_json #>> '{usage,providerCostUnavailableExecutionCount}')::bigint <> NEW.provider_cost_unavailable_execution_count
       OR (receipt_json #>> '{usage,actualPlatformAllocationCount}')::bigint <> NEW.actual_platform_allocation_count
       OR (receipt_json #>> '{usage,estimatedPlatformAllocationCount}')::bigint <> NEW.estimated_platform_allocation_count
       OR NOT stage6_internal_cost_maps_valid(receipt_json->'costs')
       OR jsonb_typeof(receipt_json->'evidence') <> 'array'
       OR jsonb_array_length(receipt_json->'evidence') <> 4
       OR (SELECT count(DISTINCT item->>'id') FROM jsonb_array_elements(receipt_json->'evidence') item) <> 4
       OR EXISTS (
         SELECT 1 FROM jsonb_array_elements(receipt_json->'evidence') item
         WHERE item->>'id' NOT IN ('usage-export', 'provider-cost-export', 'platform-allocation-export', 'reconciliation-report')
           OR item->>'sha256' !~ '^sha256:[0-9a-f]{64}$'
           OR item->>'sha256' = 'sha256:' || repeat('0', 64)
       )
       OR (receipt_json->>'evidenceFileCount')::bigint <> NEW.evidence_file_count
       OR (receipt_json #>> '{controls,tokenTotalsReconciled}')::boolean IS DISTINCT FROM NEW.token_totals_reconciled
       OR (receipt_json #>> '{controls,providerCoverageComplete}')::boolean IS DISTINCT FROM NEW.provider_coverage_complete
       OR (receipt_json #>> '{controls,actualOverridesEstimate}')::boolean IS DISTINCT FROM NEW.actual_overrides_estimate
       OR (receipt_json #>> '{controls,currencySafeAggregation}')::boolean IS DISTINCT FROM NEW.currency_safe_aggregation
       OR (receipt_json #>> '{controls,tenantIsolationValidated}')::boolean IS DISTINCT FROM NEW.tenant_isolation_validated
       OR (receipt_json #>> '{controls,noPaymentDataPresent}')::boolean IS DISTINCT FROM NEW.no_payment_data_present
       OR (receipt_json->>'eligibleForHumanGateReview')::boolean IS DISTINCT FROM NEW.eligible_for_human_gate_review
       OR (receipt_json->>'cryptographicSignaturesVerified')::boolean IS DISTINCT FROM NEW.cryptographic_signatures_verified
       OR (receipt_json->>'externalSourceAndAuthorityVerificationRequired')::boolean IS DISTINCT FROM NEW.external_source_authority_verification_required
       OR NOT NEW.token_totals_reconciled OR NOT NEW.provider_coverage_complete OR NOT NEW.actual_overrides_estimate
       OR NOT NEW.currency_safe_aggregation OR NOT NEW.tenant_isolation_validated OR NOT NEW.no_payment_data_present
       OR NOT NEW.eligible_for_human_gate_review OR NEW.cryptographic_signatures_verified
       OR NOT NEW.external_source_authority_verification_required
       OR NEW.state <> 'recorded' OR NEW.version <> 1 OR NEW.approved_at IS NOT NULL OR NEW.rejected_at IS NOT NULL
       OR NOT EXISTS (
         SELECT 1 FROM stage6_release_candidates candidate
         WHERE candidate.id = NEW.candidate_record_id
           AND candidate.operator_tenant_id = NEW.operator_tenant_id
           AND candidate.created_by = NEW.created_by
           AND candidate.candidate_id = receipt_json #>> '{candidate,candidateId}'
           AND candidate.source_commit = NEW.release_commit
           AND candidate.environment_id = NEW.environment_id
           AND convert_from(candidate.evidence_bundle_receipt, 'UTF8')::jsonb
                 #>> '{candidate,environmentClass}' = NEW.environment_class
           AND convert_from(candidate.evidence_bundle_receipt, 'UTF8')::jsonb
                 #>> '{receipts,internalCost,sha256}' = 'sha256:' || encode(NEW.receipt_sha256, 'hex')
           AND convert_from(candidate.evidence_bundle_receipt, 'UTF8')::jsonb
                 #>> '{candidate,migrationTail,name}' = NEW.migration_name
           AND convert_from(candidate.evidence_bundle_receipt, 'UTF8')::jsonb
                 #>> '{candidate,migrationTail,sha256}' = 'sha256:' || encode(NEW.migration_sha256, 'hex')
       ) THEN
      RAISE EXCEPTION 'Invalid Stage 6 internal cost receipt binding' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
  END IF;

  IF NEW.id IS DISTINCT FROM OLD.id OR NEW.operator_tenant_id IS DISTINCT FROM OLD.operator_tenant_id
     OR NEW.candidate_record_id IS DISTINCT FROM OLD.candidate_record_id OR NEW.receipt IS DISTINCT FROM OLD.receipt
     OR NEW.receipt_sha256 IS DISTINCT FROM OLD.receipt_sha256 OR NEW.receipt_size_bytes IS DISTINCT FROM OLD.receipt_size_bytes
     OR NEW.receipt_schema IS DISTINCT FROM OLD.receipt_schema OR NEW.assessment IS DISTINCT FROM OLD.assessment
     OR NEW.release_commit IS DISTINCT FROM OLD.release_commit OR NEW.environment_class IS DISTINCT FROM OLD.environment_class
     OR NEW.environment_id IS DISTINCT FROM OLD.environment_id OR NEW.manifest_sha256 IS DISTINCT FROM OLD.manifest_sha256
     OR NEW.control_plane_origin IS DISTINCT FROM OLD.control_plane_origin OR NEW.migration_name IS DISTINCT FROM OLD.migration_name
     OR NEW.migration_sha256 IS DISTINCT FROM OLD.migration_sha256 OR NEW.period_start IS DISTINCT FROM OLD.period_start
     OR NEW.period_end IS DISTINCT FROM OLD.period_end OR NEW.validated_at IS DISTINCT FROM OLD.validated_at
     OR NEW.execution_count IS DISTINCT FROM OLD.execution_count OR NEW.input_tokens IS DISTINCT FROM OLD.input_tokens
     OR NEW.output_tokens IS DISTINCT FROM OLD.output_tokens OR NEW.cached_input_tokens IS DISTINCT FROM OLD.cached_input_tokens
     OR NEW.cache_creation_input_tokens IS DISTINCT FROM OLD.cache_creation_input_tokens
     OR NEW.provider_cost_reported_execution_count IS DISTINCT FROM OLD.provider_cost_reported_execution_count
     OR NEW.provider_cost_unavailable_execution_count IS DISTINCT FROM OLD.provider_cost_unavailable_execution_count
     OR NEW.actual_platform_allocation_count IS DISTINCT FROM OLD.actual_platform_allocation_count
     OR NEW.estimated_platform_allocation_count IS DISTINCT FROM OLD.estimated_platform_allocation_count
     OR NEW.evidence_file_count IS DISTINCT FROM OLD.evidence_file_count
     OR NEW.token_totals_reconciled IS DISTINCT FROM OLD.token_totals_reconciled
     OR NEW.provider_coverage_complete IS DISTINCT FROM OLD.provider_coverage_complete
     OR NEW.actual_overrides_estimate IS DISTINCT FROM OLD.actual_overrides_estimate
     OR NEW.currency_safe_aggregation IS DISTINCT FROM OLD.currency_safe_aggregation
     OR NEW.tenant_isolation_validated IS DISTINCT FROM OLD.tenant_isolation_validated
     OR NEW.no_payment_data_present IS DISTINCT FROM OLD.no_payment_data_present
     OR NEW.eligible_for_human_gate_review IS DISTINCT FROM OLD.eligible_for_human_gate_review
     OR NEW.cryptographic_signatures_verified IS DISTINCT FROM OLD.cryptographic_signatures_verified
     OR NEW.external_source_authority_verification_required IS DISTINCT FROM OLD.external_source_authority_verification_required
     OR NEW.created_by IS DISTINCT FROM OLD.created_by OR NEW.created_at IS DISTINCT FROM OLD.created_at
     OR NEW.version <> OLD.version + 1 OR OLD.state <> 'recorded' OR NEW.state NOT IN ('approved', 'rejected') THEN
    RAISE EXCEPTION 'Stage 6 internal cost identity and evidence are immutable' USING ERRCODE = '23514';
  END IF;
  IF NEW.state = 'approved' THEN
    SELECT count(*) INTO approval_count FROM stage6_internal_cost_review_approvals
    WHERE internal_cost_review_id = NEW.id AND decision = 'approved' AND superseded_at IS NULL;
    IF approval_count <> 2 OR NOT NEW.eligible_for_human_gate_review
       OR NEW.approved_at IS NULL OR NEW.rejected_at IS NOT NULL THEN
      RAISE EXCEPTION 'Stage 6 internal cost approval gate is incomplete' USING ERRCODE = '23514';
    END IF;
  ELSIF NEW.rejected_at IS NULL OR NEW.approved_at IS NOT NULL THEN
    RAISE EXCEPTION 'Stage 6 internal cost rejection timestamp is required' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_internal_cost_reviews_guard
BEFORE INSERT OR UPDATE ON stage6_internal_cost_reviews
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_internal_cost_review();

CREATE OR REPLACE FUNCTION enforce_stage6_internal_cost_review_approval()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  parent stage6_internal_cost_reviews%ROWTYPE;
BEGIN
  SELECT * INTO parent FROM stage6_internal_cost_reviews WHERE id = NEW.internal_cost_review_id FOR UPDATE;
  IF parent.id IS NULL OR parent.operator_tenant_id <> NEW.operator_tenant_id OR parent.state <> 'recorded'
     OR parent.created_by = NEW.approver_user_id OR NEW.superseded_at IS NOT NULL OR NEW.superseded_reason IS NOT NULL
     OR NOT EXISTS (
       SELECT 1 FROM stage6_governance_authority_grants authority
       JOIN tenant_memberships membership ON membership.tenant_id = authority.operator_tenant_id
         AND membership.user_id = authority.user_id
       JOIN tenants operator_tenant ON operator_tenant.id = authority.operator_tenant_id
       JOIN users governed_user ON governed_user.id = authority.user_id
       WHERE authority.operator_tenant_id = NEW.operator_tenant_id
         AND authority.user_id = NEW.approver_user_id
         AND authority.authority_key = 'internal_cost.' || NEW.approval_role
         AND authority.status = 'active' AND authority.expires_at > statement_timestamp()
         AND membership.status = 'active' AND membership.role IN ('owner', 'admin', 'security_admin')
         AND operator_tenant.status = 'active' AND operator_tenant.deleted_at IS NULL
         AND governed_user.status = 'active' AND governed_user.deleted_at IS NULL
     ) THEN
    RAISE EXCEPTION 'Invalid Stage 6 internal cost approval authority' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_internal_cost_approvals_insert
BEFORE INSERT ON stage6_internal_cost_review_approvals
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_internal_cost_review_approval();

CREATE OR REPLACE FUNCTION reject_stage6_internal_cost_history_mutation()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'Stage 6 internal cost evidence history is immutable' USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER trg_stage6_internal_cost_approvals_no_update
BEFORE UPDATE ON stage6_internal_cost_review_approvals
FOR EACH ROW EXECUTE FUNCTION reject_stage6_internal_cost_history_mutation();
CREATE TRIGGER trg_stage6_internal_cost_approvals_no_delete
BEFORE DELETE ON stage6_internal_cost_review_approvals
FOR EACH ROW EXECUTE FUNCTION reject_stage6_internal_cost_history_mutation();
CREATE TRIGGER trg_stage6_internal_cost_reviews_no_delete
BEFORE DELETE ON stage6_internal_cost_reviews
FOR EACH ROW EXECUTE FUNCTION reject_stage6_internal_cost_history_mutation();

COMMENT ON TABLE stage6_internal_cost_reviews IS
  'Exact-candidate internal usage, Token, Provider cost and platform allocation evidence. This table contains no payment or external subscription state.';
