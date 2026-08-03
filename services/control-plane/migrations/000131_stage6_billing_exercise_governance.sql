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
    'billing_exercise.finance', 'billing_exercise.security', 'billing_exercise.release'
  ));

CREATE TABLE stage6_billing_exercises (
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
  environment_id TEXT NOT NULL CHECK (length(environment_id) BETWEEN 2 AND 160),
  manifest_sha256 BYTEA NOT NULL CHECK (octet_length(manifest_sha256) = 32),
  control_plane_origin TEXT NOT NULL CHECK (length(control_plane_origin) BETWEEN 8 AND 512),
  stripe_mode TEXT NOT NULL CHECK (stripe_mode = 'live'),
  stripe_api_version TEXT NOT NULL CHECK (stripe_api_version = '2026-05-27.dahlia'),
  migration_name TEXT NOT NULL CHECK (length(migration_name) BETWEEN 12 AND 255),
  migration_sha256 BYTEA NOT NULL CHECK (octet_length(migration_sha256) = 32),
  started_at TIMESTAMPTZ NOT NULL,
  completed_at TIMESTAMPTZ NOT NULL,
  validated_at TIMESTAMPTZ NOT NULL,
  scenario_count BIGINT NOT NULL CHECK (scenario_count = 10),
  evidence_file_count BIGINT NOT NULL CHECK (evidence_file_count = 24),
  all_scenarios_passed BOOLEAN NOT NULL,
  currency TEXT NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
  expected_amount_minor BIGINT NOT NULL CHECK (expected_amount_minor > 0),
  invoice_amount_minor BIGINT NOT NULL CHECK (invoice_amount_minor >= 0),
  settled_amount_minor BIGINT NOT NULL CHECK (settled_amount_minor >= 0),
  expected_tax_minor BIGINT NOT NULL CHECK (expected_tax_minor >= 0),
  invoice_tax_minor BIGINT NOT NULL CHECK (invoice_tax_minor >= 0),
  charge_count BIGINT NOT NULL CHECK (charge_count >= 0),
  subscription_count BIGINT NOT NULL CHECK (subscription_count >= 0),
  duplicate_charge_count BIGINT NOT NULL CHECK (duplicate_charge_count >= 0),
  raw_webhook_payload_stored BOOLEAN NOT NULL,
  card_data_handled_by_synara BOOLEAN NOT NULL,
  amounts_match BOOLEAN NOT NULL,
  cardinality_matches BOOLEAN NOT NULL,
  receipt_approvals_complete BOOLEAN NOT NULL,
  live_mode BOOLEAN NOT NULL,
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
  CHECK (started_at < completed_at AND completed_at <= validated_at)
);

CREATE INDEX idx_stage6_billing_exercises_candidate
  ON stage6_billing_exercises (candidate_record_id, created_at DESC, id);

CREATE TABLE stage6_billing_exercise_approvals (
  id UUID PRIMARY KEY,
  billing_exercise_id UUID NOT NULL REFERENCES stage6_billing_exercises(id) ON DELETE RESTRICT,
  operator_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  approval_role TEXT NOT NULL CHECK (approval_role IN ('finance', 'security', 'release')),
  decision TEXT NOT NULL CHECK (decision IN ('approved', 'rejected')),
  approver_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  reason TEXT NOT NULL CHECK (length(trim(reason)) BETWEEN 20 AND 2000),
  evidence_reference TEXT NOT NULL CHECK (length(trim(evidence_reference)) BETWEEN 8 AND 2048),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (billing_exercise_id, approval_role),
  UNIQUE (billing_exercise_id, approver_user_id)
);

CREATE INDEX idx_stage6_billing_exercise_approvals_exercise
  ON stage6_billing_exercise_approvals (billing_exercise_id, created_at, id);

CREATE OR REPLACE FUNCTION enforce_stage6_billing_exercise()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
  approval_count INTEGER;
  receipt_json JSONB;
BEGIN
  IF TG_OP = 'INSERT' THEN
    receipt_json := convert_from(NEW.receipt, 'UTF8')::jsonb;
    IF NEW.receipt_size_bytes <> octet_length(NEW.receipt)
       OR digest(NEW.receipt, 'sha256') <> NEW.receipt_sha256
       OR NEW.receipt_schema <> 'synara.stage6-stripe-billing-exercise-validation.v1'
       OR NEW.assessment <> 'evidence-validated-not-billing-passed'
       OR receipt_json->>'schemaVersion' <> NEW.receipt_schema
       OR receipt_json->>'assessment' <> NEW.assessment
       OR receipt_json #>> '{candidate,sourceCommit}' <> NEW.release_commit
       OR receipt_json #>> '{candidate,environment}' <> NEW.environment_class
       OR receipt_json #>> '{candidate,environmentId}' <> NEW.environment_id
       OR receipt_json #>> '{candidate,controlPlaneBaseUrl}' <> NEW.control_plane_origin
       OR receipt_json #>> '{candidate,stripeMode}' <> NEW.stripe_mode
       OR receipt_json #>> '{candidate,stripeApiVersion}' <> NEW.stripe_api_version
       OR receipt_json #>> '{candidate,migrationTail,name}' <> NEW.migration_name
       OR receipt_json #>> '{candidate,migrationTail,sha256}' <> 'sha256:' || encode(NEW.migration_sha256, 'hex')
       OR receipt_json #>> '{manifest,sha256}' <> 'sha256:' || encode(NEW.manifest_sha256, 'hex')
       OR jsonb_array_length(receipt_json->'scenarios') <> NEW.scenario_count
       OR (receipt_json #>> '{scenarioCounts,passed}')::bigint <> NEW.scenario_count
       OR receipt_json #>> '{reconciliation,currency}' <> NEW.currency
       OR (receipt_json #>> '{reconciliation,expectedAmountMinor}')::bigint <> NEW.expected_amount_minor
       OR (receipt_json #>> '{reconciliation,invoiceAmountMinor}')::bigint <> NEW.invoice_amount_minor
       OR (receipt_json #>> '{reconciliation,settledAmountMinor}')::bigint <> NEW.settled_amount_minor
       OR (receipt_json #>> '{reconciliation,expectedTaxMinor}')::bigint <> NEW.expected_tax_minor
       OR (receipt_json #>> '{reconciliation,invoiceTaxMinor}')::bigint <> NEW.invoice_tax_minor
       OR (receipt_json #>> '{reconciliation,chargeCount}')::bigint <> NEW.charge_count
       OR (receipt_json #>> '{reconciliation,subscriptionCount}')::bigint <> NEW.subscription_count
       OR (receipt_json #>> '{reconciliation,duplicateChargeCount}')::bigint <> NEW.duplicate_charge_count
       OR (receipt_json #>> '{eligibleForHumanGateReview}')::boolean IS DISTINCT FROM NEW.eligible_for_human_gate_review
       OR NOT NEW.all_scenarios_passed OR NOT NEW.amounts_match OR NOT NEW.cardinality_matches
       OR NEW.raw_webhook_payload_stored OR NEW.card_data_handled_by_synara
       OR NOT NEW.receipt_approvals_complete OR NOT NEW.live_mode OR NOT NEW.eligible_for_human_gate_review
       OR NEW.expected_amount_minor <> NEW.invoice_amount_minor OR NEW.invoice_amount_minor <> NEW.settled_amount_minor
       OR NEW.expected_tax_minor <> NEW.invoice_tax_minor
       OR NEW.charge_count <> 1 OR NEW.subscription_count <> 1 OR NEW.duplicate_charge_count <> 0
       OR NEW.cryptographic_signatures_verified OR NOT NEW.external_authority_verification_required
       OR NEW.state <> 'recorded' OR NEW.version <> 1 OR NEW.approved_at IS NOT NULL OR NEW.rejected_at IS NOT NULL
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
                 #>> '{candidate,migrationTail,name}' = NEW.migration_name
           AND convert_from(candidate.evidence_bundle_receipt, 'UTF8')::jsonb
                 #>> '{candidate,migrationTail,sha256}' = 'sha256:' || encode(NEW.migration_sha256, 'hex')
           AND convert_from(candidate.evidence_bundle_receipt, 'UTF8')::jsonb
                 #>> '{receipts,billing,sha256}' = 'sha256:' || encode(NEW.receipt_sha256, 'hex')
       ) THEN
      RAISE EXCEPTION 'Invalid Stage 6 Billing exercise receipt binding' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
  END IF;

  IF NEW.id IS DISTINCT FROM OLD.id OR NEW.operator_tenant_id IS DISTINCT FROM OLD.operator_tenant_id
     OR NEW.candidate_record_id IS DISTINCT FROM OLD.candidate_record_id OR NEW.receipt IS DISTINCT FROM OLD.receipt
     OR NEW.receipt_sha256 IS DISTINCT FROM OLD.receipt_sha256 OR NEW.receipt_size_bytes IS DISTINCT FROM OLD.receipt_size_bytes
     OR NEW.receipt_schema IS DISTINCT FROM OLD.receipt_schema OR NEW.assessment IS DISTINCT FROM OLD.assessment
     OR NEW.release_commit IS DISTINCT FROM OLD.release_commit OR NEW.environment_class IS DISTINCT FROM OLD.environment_class
     OR NEW.environment_id IS DISTINCT FROM OLD.environment_id OR NEW.manifest_sha256 IS DISTINCT FROM OLD.manifest_sha256
     OR NEW.control_plane_origin IS DISTINCT FROM OLD.control_plane_origin OR NEW.stripe_mode IS DISTINCT FROM OLD.stripe_mode
     OR NEW.stripe_api_version IS DISTINCT FROM OLD.stripe_api_version OR NEW.migration_name IS DISTINCT FROM OLD.migration_name
     OR NEW.migration_sha256 IS DISTINCT FROM OLD.migration_sha256 OR NEW.started_at IS DISTINCT FROM OLD.started_at
     OR NEW.completed_at IS DISTINCT FROM OLD.completed_at OR NEW.validated_at IS DISTINCT FROM OLD.validated_at
     OR NEW.scenario_count IS DISTINCT FROM OLD.scenario_count OR NEW.evidence_file_count IS DISTINCT FROM OLD.evidence_file_count
     OR NEW.all_scenarios_passed IS DISTINCT FROM OLD.all_scenarios_passed OR NEW.currency IS DISTINCT FROM OLD.currency
     OR NEW.expected_amount_minor IS DISTINCT FROM OLD.expected_amount_minor OR NEW.invoice_amount_minor IS DISTINCT FROM OLD.invoice_amount_minor
     OR NEW.settled_amount_minor IS DISTINCT FROM OLD.settled_amount_minor OR NEW.expected_tax_minor IS DISTINCT FROM OLD.expected_tax_minor
     OR NEW.invoice_tax_minor IS DISTINCT FROM OLD.invoice_tax_minor OR NEW.charge_count IS DISTINCT FROM OLD.charge_count
     OR NEW.subscription_count IS DISTINCT FROM OLD.subscription_count OR NEW.duplicate_charge_count IS DISTINCT FROM OLD.duplicate_charge_count
     OR NEW.raw_webhook_payload_stored IS DISTINCT FROM OLD.raw_webhook_payload_stored
     OR NEW.card_data_handled_by_synara IS DISTINCT FROM OLD.card_data_handled_by_synara
     OR NEW.amounts_match IS DISTINCT FROM OLD.amounts_match OR NEW.cardinality_matches IS DISTINCT FROM OLD.cardinality_matches
     OR NEW.receipt_approvals_complete IS DISTINCT FROM OLD.receipt_approvals_complete OR NEW.live_mode IS DISTINCT FROM OLD.live_mode
     OR NEW.eligible_for_human_gate_review IS DISTINCT FROM OLD.eligible_for_human_gate_review
     OR NEW.cryptographic_signatures_verified IS DISTINCT FROM OLD.cryptographic_signatures_verified
     OR NEW.external_authority_verification_required IS DISTINCT FROM OLD.external_authority_verification_required
     OR NEW.created_by IS DISTINCT FROM OLD.created_by OR NEW.created_at IS DISTINCT FROM OLD.created_at
     OR NEW.version <> OLD.version + 1 OR OLD.state <> 'recorded' OR NEW.state NOT IN ('approved', 'rejected') THEN
    RAISE EXCEPTION 'Stage 6 Billing exercise identity and evidence are immutable' USING ERRCODE = '23514';
  END IF;
  IF NEW.state = 'approved' THEN
    SELECT count(*) INTO approval_count FROM stage6_billing_exercise_approvals
    WHERE billing_exercise_id = NEW.id AND decision = 'approved';
    IF approval_count <> 3 OR NOT NEW.eligible_for_human_gate_review
       OR NEW.approved_at IS NULL OR NEW.rejected_at IS NOT NULL THEN
      RAISE EXCEPTION 'Stage 6 Billing exercise approval gate is incomplete' USING ERRCODE = '23514';
    END IF;
  ELSIF NEW.rejected_at IS NULL OR NEW.approved_at IS NOT NULL THEN
    RAISE EXCEPTION 'Stage 6 Billing exercise rejection timestamp is required' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_billing_exercises_guard
BEFORE INSERT OR UPDATE ON stage6_billing_exercises
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_billing_exercise();

CREATE OR REPLACE FUNCTION enforce_stage6_billing_exercise_approval()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
  parent stage6_billing_exercises%ROWTYPE;
BEGIN
  SELECT * INTO parent FROM stage6_billing_exercises WHERE id = NEW.billing_exercise_id FOR UPDATE;
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
         AND authority.authority_key = 'billing_exercise.' || NEW.approval_role
         AND authority.status = 'active' AND authority.expires_at > statement_timestamp()
         AND membership.status = 'active' AND membership.role IN ('owner', 'admin', 'security_admin')
         AND operator_tenant.status = 'active' AND operator_tenant.deleted_at IS NULL
         AND governed_user.status = 'active' AND governed_user.deleted_at IS NULL
     ) THEN
    RAISE EXCEPTION 'Invalid Stage 6 Billing exercise approval authority' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_billing_exercise_approvals_insert
BEFORE INSERT ON stage6_billing_exercise_approvals
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_billing_exercise_approval();

CREATE OR REPLACE FUNCTION reject_stage6_billing_exercise_history_mutation()
RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
  RAISE EXCEPTION 'Stage 6 Billing exercise evidence history is immutable' USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER trg_stage6_billing_exercise_approvals_no_update BEFORE UPDATE ON stage6_billing_exercise_approvals
FOR EACH ROW EXECUTE FUNCTION reject_stage6_billing_exercise_history_mutation();
CREATE TRIGGER trg_stage6_billing_exercise_approvals_no_delete BEFORE DELETE ON stage6_billing_exercise_approvals
FOR EACH ROW EXECUTE FUNCTION reject_stage6_billing_exercise_history_mutation();
CREATE TRIGGER trg_stage6_billing_exercises_no_delete BEFORE DELETE ON stage6_billing_exercises
FOR EACH ROW EXECUTE FUNCTION reject_stage6_billing_exercise_history_mutation();

COMMENT ON TABLE stage6_billing_exercises IS
  'Internal governance over exact Stripe billing exercise receipts; approval does not authenticate Stripe, settlement, tax, evidence files, signatures, execution, or external approver authority.';
