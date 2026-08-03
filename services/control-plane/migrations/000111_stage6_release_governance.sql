CREATE TABLE stage6_release_candidates (
  id UUID PRIMARY KEY,
  operator_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  candidate_id TEXT NOT NULL UNIQUE CHECK (length(candidate_id) BETWEEN 3 AND 200),
  source_commit TEXT NOT NULL CHECK (length(source_commit) = 40),
  lockfile_sha256 BYTEA NOT NULL CHECK (octet_length(lockfile_sha256) = 32),
  evidence_bundle_sha256 BYTEA NOT NULL CHECK (octet_length(evidence_bundle_sha256) = 32),
  final_asset_set_sha256 BYTEA NOT NULL CHECK (octet_length(final_asset_set_sha256) = 32),
  environment_id TEXT NOT NULL CHECK (length(environment_id) BETWEEN 3 AND 300),
  state TEXT NOT NULL CHECK (state IN (
    'draft', 'ready_for_review', 'approved', 'deploying', 'observing',
    'released', 'rejected', 'rolled_back'
  )),
  version BIGINT NOT NULL DEFAULT 1 CHECK (version > 0),
  created_by UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  decision_summary TEXT,
  residual_risk_disposition TEXT CHECK (residual_risk_disposition IN ('none', 'accepted')),
  residual_risks JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(residual_risks) = 'array'),
  approved_at TIMESTAMPTZ,
  released_at TIMESTAMPTZ,
  rejected_at TIMESTAMPTZ,
  rolled_back_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_stage6_release_candidates_state
  ON stage6_release_candidates (state, updated_at DESC, id);

CREATE TABLE stage6_release_approvals (
  id UUID PRIMARY KEY,
  candidate_record_id UUID NOT NULL REFERENCES stage6_release_candidates(id) ON DELETE RESTRICT,
  operator_tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
  approval_role TEXT NOT NULL CHECK (approval_role IN ('engineering', 'operations', 'security', 'product', 'privacy_legal')),
  decision TEXT NOT NULL CHECK (decision IN ('approved', 'rejected')),
  approver_user_id UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
  reason TEXT NOT NULL CHECK (length(trim(reason)) BETWEEN 10 AND 2000),
  evidence_reference TEXT NOT NULL CHECK (length(trim(evidence_reference)) BETWEEN 8 AND 2048),
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (candidate_record_id, approval_role),
  UNIQUE (candidate_record_id, approver_user_id)
);

CREATE INDEX idx_stage6_release_approvals_candidate
  ON stage6_release_approvals (candidate_record_id, created_at, id);

CREATE OR REPLACE FUNCTION enforce_stage6_release_candidate_transition()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  required_approvals INTEGER;
BEGIN
  IF TG_OP = 'INSERT' THEN
    IF NEW.state <> 'draft' OR NEW.version <> 1
       OR NEW.decision_summary IS NOT NULL OR NEW.residual_risk_disposition IS NOT NULL
       OR NEW.residual_risks <> '[]'::jsonb OR NEW.approved_at IS NOT NULL
       OR NEW.released_at IS NOT NULL OR NEW.rejected_at IS NOT NULL OR NEW.rolled_back_at IS NOT NULL
       OR NOT EXISTS (
         SELECT 1 FROM tenant_memberships AS membership
         JOIN tenants AS operator_tenant ON operator_tenant.id = membership.tenant_id
         JOIN users AS creator ON creator.id = membership.user_id
         WHERE membership.tenant_id = NEW.operator_tenant_id
           AND membership.user_id = NEW.created_by
           AND membership.status = 'active'
           AND membership.role IN ('owner', 'admin')
           AND operator_tenant.status = 'active' AND operator_tenant.deleted_at IS NULL
           AND creator.status = 'active' AND creator.deleted_at IS NULL
       ) THEN
      RAISE EXCEPTION 'Invalid Stage 6 release candidate initial state' USING ERRCODE = '23514';
    END IF;
    RETURN NEW;
  END IF;

  IF NEW.id IS DISTINCT FROM OLD.id
     OR NEW.operator_tenant_id IS DISTINCT FROM OLD.operator_tenant_id
     OR NEW.candidate_id IS DISTINCT FROM OLD.candidate_id
     OR NEW.source_commit IS DISTINCT FROM OLD.source_commit
     OR NEW.lockfile_sha256 IS DISTINCT FROM OLD.lockfile_sha256
     OR NEW.evidence_bundle_sha256 IS DISTINCT FROM OLD.evidence_bundle_sha256
     OR NEW.final_asset_set_sha256 IS DISTINCT FROM OLD.final_asset_set_sha256
     OR NEW.environment_id IS DISTINCT FROM OLD.environment_id
     OR NEW.created_by IS DISTINCT FROM OLD.created_by
     OR NEW.created_at IS DISTINCT FROM OLD.created_at
     OR NEW.version <> OLD.version + 1 THEN
    RAISE EXCEPTION 'Stage 6 release candidate identity is immutable' USING ERRCODE = '23514';
  END IF;

  IF (OLD.state = 'draft' AND NEW.state <> 'ready_for_review')
     OR (OLD.state = 'ready_for_review' AND NEW.state NOT IN ('approved', 'rejected'))
     OR (OLD.state = 'approved' AND NEW.state <> 'deploying')
     OR (OLD.state = 'deploying' AND NEW.state NOT IN ('observing', 'rolled_back'))
     OR (OLD.state = 'observing' AND NEW.state NOT IN ('released', 'rolled_back'))
     OR OLD.state IN ('released', 'rejected', 'rolled_back') THEN
    RAISE EXCEPTION 'Invalid Stage 6 release candidate transition' USING ERRCODE = '23514';
  END IF;

  IF NEW.state = 'approved' THEN
    SELECT count(*) INTO required_approvals
    FROM stage6_release_approvals
    WHERE candidate_record_id = NEW.id
      AND decision = 'approved'
      AND approval_role IN ('engineering', 'operations', 'security', 'product');
    IF required_approvals <> 4 OR NEW.approved_at IS NULL THEN
      RAISE EXCEPTION 'Stage 6 release candidate requires four separated approvals' USING ERRCODE = '23514';
    END IF;
  END IF;

  IF NEW.state = 'released' AND (
    NEW.released_at IS NULL
    OR length(trim(COALESCE(NEW.decision_summary, ''))) NOT BETWEEN 20 AND 4000
    OR NEW.residual_risk_disposition IS NULL
    OR (NEW.residual_risk_disposition = 'none' AND NEW.residual_risks <> '[]'::jsonb)
    OR (NEW.residual_risk_disposition = 'accepted' AND jsonb_array_length(NEW.residual_risks) = 0)
  ) THEN
    RAISE EXCEPTION 'Released Stage 6 candidate requires decision and residual-risk disposition' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_release_candidates_transition
BEFORE INSERT OR UPDATE ON stage6_release_candidates
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_release_candidate_transition();

CREATE OR REPLACE FUNCTION enforce_stage6_release_approval()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM stage6_release_candidates AS candidate
    JOIN tenant_memberships AS membership
      ON membership.tenant_id = candidate.operator_tenant_id
     AND membership.user_id = NEW.approver_user_id
     AND membership.status = 'active'
     AND membership.role IN ('owner', 'admin', 'security_admin')
    JOIN tenants AS operator_tenant
      ON operator_tenant.id = membership.tenant_id
     AND operator_tenant.status = 'active' AND operator_tenant.deleted_at IS NULL
    JOIN users AS approver
      ON approver.id = membership.user_id
     AND approver.status = 'active' AND approver.deleted_at IS NULL
    WHERE candidate.id = NEW.candidate_record_id
      AND candidate.operator_tenant_id = NEW.operator_tenant_id
      AND candidate.state = 'ready_for_review'
      AND candidate.created_by <> NEW.approver_user_id
  ) THEN
    RAISE EXCEPTION 'Stage 6 approval requires a reviewable candidate and separated approver' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_release_approvals_insert
BEFORE INSERT ON stage6_release_approvals
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_release_approval();

CREATE OR REPLACE FUNCTION reject_stage6_release_approval_mutation()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
  RAISE EXCEPTION 'Stage 6 release approvals are immutable' USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER trg_stage6_release_approvals_no_update
BEFORE UPDATE ON stage6_release_approvals
FOR EACH ROW EXECUTE FUNCTION reject_stage6_release_approval_mutation();

CREATE TRIGGER trg_stage6_release_approvals_no_delete
BEFORE DELETE ON stage6_release_approvals
FOR EACH ROW EXECUTE FUNCTION reject_stage6_release_approval_mutation();

CREATE TRIGGER trg_stage6_release_candidates_updated_at
BEFORE UPDATE ON stage6_release_candidates
FOR EACH ROW EXECUTE FUNCTION set_updated_at();

COMMENT ON TABLE stage6_release_candidates IS
  'Platform release-governance record; source and artifact identity are immutable and do not substitute for external GA evidence.';
