DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM stage6_release_candidates
    WHERE evidence_receipt_bound
      AND evidence_bundle_schema = 'synara.stage6-candidate-evidence-bundle-validation.v4'
      AND state NOT IN ('released', 'rejected', 'rolled_back')
  ) THEN
    RAISE EXCEPTION 'Stage 6 candidate v5 migration requires every active v4 candidate to be replaced or closed'
      USING ERRCODE = '23514';
  END IF;
END;
$$;

ALTER TABLE stage6_release_candidates
  DROP CONSTRAINT IF EXISTS chk_stage6_release_candidate_evidence_binding;
ALTER TABLE stage6_release_candidates
  ADD CONSTRAINT chk_stage6_release_candidate_evidence_binding CHECK (
    (NOT evidence_receipt_bound
      AND evidence_bundle_receipt IS NULL
      AND evidence_bundle_schema IS NULL
      AND evidence_bundle_assessment IS NULL
      AND evidence_bundle_validated_at IS NULL
      AND evidence_bundle_receipt_size_bytes IS NULL
      AND desktop_artifact_set_sha256 IS NULL)
    OR
    (evidence_receipt_bound
      AND evidence_bundle_receipt IS NOT NULL
      AND (
        evidence_bundle_schema = 'synara.stage6-candidate-evidence-bundle-validation.v5'
        OR (
          evidence_bundle_schema IN (
            'synara.stage6-candidate-evidence-bundle-validation.v2',
            'synara.stage6-candidate-evidence-bundle-validation.v3',
            'synara.stage6-candidate-evidence-bundle-validation.v4'
          )
          AND state IN ('released', 'rejected', 'rolled_back')
        )
      )
      AND evidence_bundle_assessment = 'evidence-consistent-not-ga-approved'
      AND evidence_bundle_validated_at IS NOT NULL
      AND evidence_bundle_receipt_size_bytes BETWEEN 1 AND 32768
      AND octet_length(evidence_bundle_receipt) = evidence_bundle_receipt_size_bytes
      AND desktop_artifact_set_sha256 ~ '^sha256:[0-9a-f]{64}$')
  );

DO $$
DECLARE
  definition TEXT;
BEGIN
  SELECT pg_get_functiondef('enforce_stage6_release_candidate_transition()'::regprocedure) INTO definition;
  IF position('synara.stage6-candidate-evidence-bundle-validation.v4' IN definition) = 0
     OR position('(SELECT count(*) FROM jsonb_object_keys(evidence_receipt)) <> 12' IN definition) = 0
     OR position('''candidateConsistencyValidated'', ''eligibleForCandidateEvidenceReview''' IN definition) = 0 THEN
    RAISE EXCEPTION 'Unexpected Stage 6 candidate v4 transition baseline' USING ERRCODE = '23514';
  END IF;
  definition := replace(
    definition,
    'synara.stage6-candidate-evidence-bundle-validation.v4',
    'synara.stage6-candidate-evidence-bundle-validation.v5'
  );
  definition := replace(
    definition,
    '(SELECT count(*) FROM jsonb_object_keys(evidence_receipt)) <> 12',
    '(SELECT count(*) FROM jsonb_object_keys(evidence_receipt)) <> 13'
  );
  definition := replace(
    definition,
    '''candidateConsistencyValidated'', ''eligibleForCandidateEvidenceReview''',
    '''candidateConsistencyValidated'', ''compatibilityMatrix'', ''eligibleForCandidateEvidenceReview'''
  );
  EXECUTE definition;
END;
$$;

CREATE OR REPLACE FUNCTION enforce_stage6_candidate_compatibility_matrix()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
DECLARE
  receipt JSONB;
  compatibility JSONB;
  candidate_migration JSONB;
BEGIN
  IF NOT NEW.evidence_receipt_bound THEN
    RETURN NEW;
  END IF;
  receipt := convert_from(NEW.evidence_bundle_receipt, 'UTF8')::jsonb;
  compatibility := receipt->'compatibilityMatrix';
  candidate_migration := receipt->'candidate'->'migrationTail';
  IF jsonb_typeof(compatibility) <> 'object'
     OR (SELECT count(*) FROM jsonb_object_keys(compatibility)) <> 8
     OR NOT (compatibility ?& ARRAY[
       'assessment', 'matrixVersion', 'migrationTail', 'path', 'schemaVersion', 'sha256',
       'sourceByteCount', 'sourceFileCount'
     ])
     OR NOT stage6_valid_evidence_reference(jsonb_build_object(
       'path', compatibility->'path', 'sha256', compatibility->'sha256'
     ))
     OR compatibility->>'schemaVersion' <> 'synara.release-compatibility-matrix.v1'
     OR compatibility->>'assessment' <> 'source-compatible-not-release-approved'
     OR jsonb_typeof(compatibility->'matrixVersion') <> 'number'
     OR (compatibility->>'matrixVersion')::integer <> 1
     OR jsonb_typeof(compatibility->'sourceFileCount') <> 'number'
     OR (compatibility->>'sourceFileCount')::integer < 1
     OR jsonb_typeof(compatibility->'sourceByteCount') <> 'number'
     OR (compatibility->>'sourceByteCount')::bigint < 1
     OR compatibility->'migrationTail' IS DISTINCT FROM candidate_migration
     OR (SELECT count(DISTINCT path_value) FROM (
       SELECT receipt->'manifest'->>'path' AS path_value
       UNION ALL SELECT receipt->'releaseEvidence'->>'path'
       UNION ALL SELECT compatibility->>'path'
       UNION ALL SELECT value->>'path' FROM jsonb_each(receipt->'receipts') AS item(key, value)
     ) AS paths) <> 13 THEN
    RAISE EXCEPTION 'Stage 6 candidate requires an exact source-current compatibility matrix projection'
      USING ERRCODE = '23514';
  END IF;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_stage6_candidate_compatibility_matrix ON stage6_release_candidates;
CREATE TRIGGER trg_stage6_candidate_compatibility_matrix
BEFORE INSERT OR UPDATE ON stage6_release_candidates
FOR EACH ROW EXECUTE FUNCTION enforce_stage6_candidate_compatibility_matrix();

COMMENT ON COLUMN stage6_release_candidates.evidence_bundle_receipt IS
  'Exact bounded UTF-8 internal-self-hosted v5 candidate receipt bytes with source-current compatibility matrix binding; terminal v2-v4 rows remain historical only.';
COMMENT ON FUNCTION enforce_stage6_candidate_compatibility_matrix() IS
  'Rejects Candidate v5 receipts that omit, drift or reuse the exact source-current cross-component compatibility matrix projection.';
