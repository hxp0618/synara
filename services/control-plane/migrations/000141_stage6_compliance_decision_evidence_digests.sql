ALTER TABLE stage6_compliance_evidence_reviews
  ADD COLUMN evidence_sha256 TEXT,
  ADD COLUMN superseded_at TIMESTAMPTZ,
  ADD COLUMN superseded_reason TEXT;

ALTER TABLE stage6_compliance_program_decisions
  ADD COLUMN evidence_sha256 TEXT,
  ADD COLUMN superseded_at TIMESTAMPTZ,
  ADD COLUMN superseded_reason TEXT;

DROP TRIGGER trg_stage6_compliance_reviews_no_update ON stage6_compliance_evidence_reviews;
DROP TRIGGER trg_stage6_compliance_decisions_no_update ON stage6_compliance_program_decisions;

UPDATE stage6_compliance_evidence_reviews
SET superseded_at = now(),
    superseded_reason = 'Automatically superseded during Migration 000141 because review evidence was not byte-bound.'
WHERE evidence_sha256 IS NULL AND superseded_at IS NULL;

UPDATE stage6_compliance_program_decisions
SET superseded_at = now(),
    superseded_reason = 'Automatically superseded during Migration 000141 because decision evidence was not byte-bound.'
WHERE evidence_sha256 IS NULL AND superseded_at IS NULL;

ALTER TABLE stage6_compliance_evidence_reviews
  ADD CONSTRAINT stage6_compliance_review_evidence_sha256_shape CHECK (
    evidence_sha256 IS NULL OR evidence_sha256 ~ '^sha256:[0-9a-f]{64}$'
  ),
  ADD CONSTRAINT stage6_compliance_review_supersession_shape CHECK (
    (superseded_at IS NULL AND superseded_reason IS NULL
      AND evidence_sha256 IS NOT NULL
      AND evidence_sha256 <> 'sha256:' || repeat('0', 64))
    OR
    (superseded_at IS NOT NULL AND length(trim(superseded_reason)) BETWEEN 10 AND 500)
  );

ALTER TABLE stage6_compliance_program_decisions
  ADD CONSTRAINT stage6_compliance_decision_evidence_sha256_shape CHECK (
    evidence_sha256 IS NULL OR evidence_sha256 ~ '^sha256:[0-9a-f]{64}$'
  ),
  ADD CONSTRAINT stage6_compliance_decision_supersession_shape CHECK (
    (superseded_at IS NULL AND superseded_reason IS NULL
      AND evidence_sha256 IS NOT NULL
      AND evidence_sha256 <> 'sha256:' || repeat('0', 64))
    OR
    (superseded_at IS NOT NULL AND length(trim(superseded_reason)) BETWEEN 10 AND 500)
  );

DO $$
DECLARE
  constraint_name TEXT;
BEGIN
  SELECT constraint_record.conname INTO constraint_name
  FROM pg_constraint AS constraint_record
  JOIN pg_class AS relation ON relation.oid = constraint_record.conrelid
  WHERE relation.oid = 'stage6_compliance_evidence_reviews'::regclass
    AND constraint_record.contype = 'u'
    AND pg_get_constraintdef(constraint_record.oid) = 'UNIQUE (evidence_record_id)';
  IF constraint_name IS NOT NULL THEN
    EXECUTE format('ALTER TABLE stage6_compliance_evidence_reviews DROP CONSTRAINT %I', constraint_name);
  END IF;

  SELECT constraint_record.conname INTO constraint_name
  FROM pg_constraint AS constraint_record
  JOIN pg_class AS relation ON relation.oid = constraint_record.conrelid
  WHERE relation.oid = 'stage6_compliance_program_decisions'::regclass
    AND constraint_record.contype = 'u'
    AND pg_get_constraintdef(constraint_record.oid) = 'UNIQUE (program_id, decision_role)';
  IF constraint_name IS NOT NULL THEN
    EXECUTE format('ALTER TABLE stage6_compliance_program_decisions DROP CONSTRAINT %I', constraint_name);
  END IF;

  SELECT constraint_record.conname INTO constraint_name
  FROM pg_constraint AS constraint_record
  JOIN pg_class AS relation ON relation.oid = constraint_record.conrelid
  WHERE relation.oid = 'stage6_compliance_program_decisions'::regclass
    AND constraint_record.contype = 'u'
    AND pg_get_constraintdef(constraint_record.oid) = 'UNIQUE (program_id, decider_user_id)';
  IF constraint_name IS NOT NULL THEN
    EXECUTE format('ALTER TABLE stage6_compliance_program_decisions DROP CONSTRAINT %I', constraint_name);
  END IF;
END;
$$;

CREATE UNIQUE INDEX uq_stage6_compliance_reviews_evidence_active
  ON stage6_compliance_evidence_reviews (evidence_record_id)
  WHERE superseded_at IS NULL;
CREATE UNIQUE INDEX uq_stage6_compliance_decisions_role_active
  ON stage6_compliance_program_decisions (program_id, decision_role)
  WHERE superseded_at IS NULL;
CREATE UNIQUE INDEX uq_stage6_compliance_decisions_user_active
  ON stage6_compliance_program_decisions (program_id, decider_user_id)
  WHERE superseded_at IS NULL;

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
     OR NEW.evidence_sha256 IS NULL OR NEW.evidence_sha256 = 'sha256:' || repeat('0', 64)
     OR NEW.superseded_at IS NOT NULL OR NEW.superseded_reason IS NOT NULL
     OR NOT stage6_compliance_active_operator(evidence_tenant, NEW.reviewer_user_id, ARRAY['owner', 'admin', 'security_admin']) THEN
    RAISE EXCEPTION 'Compliance evidence review requires byte-bound evidence and a separated active operator' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

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
     OR NEW.evidence_sha256 IS NULL OR NEW.evidence_sha256 = 'sha256:' || repeat('0', 64)
     OR NEW.superseded_at IS NOT NULL OR NEW.superseded_reason IS NOT NULL
     OR NOT stage6_compliance_active_operator(program_tenant, NEW.decider_user_id, ARRAY['owner', 'admin', 'security_admin']) THEN
    RAISE EXCEPTION 'Compliance decision requires byte-bound evidence, review state and a separated active operator' USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE OR REPLACE FUNCTION enforce_stage6_compliance_decision_evidence_gate()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  approval_count INTEGER;
  accepted_manifest_count INTEGER;
BEGIN
  IF NEW.state = OLD.state OR NEW.state <> 'record_complete' THEN
    RETURN NEW;
  END IF;

  SELECT count(*) INTO approval_count
  FROM stage6_compliance_program_decisions
  WHERE program_id = NEW.id AND decision = 'approved'
    AND decision_role IN ('security', 'operations', 'legal_privacy', 'executive')
    AND superseded_at IS NULL AND evidence_sha256 IS NOT NULL
    AND evidence_sha256 <> 'sha256:' || repeat('0', 64);

  SELECT count(*) INTO accepted_manifest_count
  FROM stage6_compliance_evidence AS evidence
  JOIN stage6_compliance_evidence_reviews AS review
    ON review.evidence_record_id = evidence.id
   AND review.decision = 'accepted'
   AND review.superseded_at IS NULL
   AND review.evidence_sha256 IS NOT NULL
   AND review.evidence_sha256 <> 'sha256:' || repeat('0', 64)
  WHERE evidence.program_id = NEW.id AND evidence.evidence_type = 'release_manifest';

  IF approval_count <> 4 OR accepted_manifest_count < 1 THEN
    RAISE EXCEPTION 'Compliance completion requires byte-bound decisions and release-manifest review evidence'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

CREATE TRIGGER trg_stage6_compliance_decision_evidence_gate
BEFORE UPDATE ON stage6_compliance_programs
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_compliance_decision_evidence_gate();

CREATE TRIGGER trg_stage6_compliance_reviews_no_update
BEFORE UPDATE OR DELETE ON stage6_compliance_evidence_reviews
FOR EACH ROW EXECUTE FUNCTION reject_stage6_compliance_append_only_mutation();
CREATE TRIGGER trg_stage6_compliance_decisions_no_update
BEFORE UPDATE OR DELETE ON stage6_compliance_program_decisions
FOR EACH ROW EXECUTE FUNCTION reject_stage6_compliance_append_only_mutation();

COMMENT ON COLUMN stage6_compliance_evidence_reviews.evidence_sha256 IS
  'SHA-256 of the exact external evidence bytes reviewed for this immutable compliance evidence decision.';
COMMENT ON COLUMN stage6_compliance_program_decisions.evidence_sha256 IS
  'SHA-256 of the exact external evidence bytes reviewed for this immutable compliance program decision.';
