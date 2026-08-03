package database

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

func migrateReleaseGovernanceSQLiteSafety(ctx context.Context, db *gorm.DB) error {
	statements := []string{
		`CREATE INDEX IF NOT EXISTS idx_stage6_release_candidates_state
		 ON stage6_release_candidates (state, updated_at DESC, id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_stage6_release_approvals_role
		 ON stage6_release_approvals (candidate_record_id, approval_role)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_stage6_release_approvals_user
		 ON stage6_release_approvals (candidate_record_id, approver_user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_stage6_release_approvals_candidate
		 ON stage6_release_approvals (candidate_record_id, created_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_stage6_release_provider_authorization_bindings_candidate
		 ON stage6_release_provider_authorization_bindings (candidate_record_id, provider, authorization_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_stage6_release_final_reviews_candidate
		 ON stage6_release_final_reviews (candidate_record_id)`,
		`CREATE INDEX IF NOT EXISTS idx_stage6_release_final_reviews_candidate
		 ON stage6_release_final_reviews (candidate_record_id, created_at, id)`,
		`DROP TRIGGER IF EXISTS trg_stage6_release_candidates_insert`,
		`CREATE TRIGGER trg_stage6_release_candidates_insert
		 BEFORE INSERT ON stage6_release_candidates
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Stage 6 release candidate')
		   WHERE length(NEW.candidate_id) NOT BETWEEN 3 AND 200
		      OR length(NEW.source_commit) <> 40
		      OR length(NEW.lockfile_sha256) <> 32
		      OR length(NEW.evidence_bundle_sha256) <> 32
		      OR NEW.evidence_receipt_bound <> 1
		      OR NEW.evidence_bundle_receipt_size_bytes NOT BETWEEN 1 AND 32768
		      OR length(NEW.evidence_bundle_receipt) <> NEW.evidence_bundle_receipt_size_bytes
		      OR NEW.evidence_bundle_schema <> 'synara.stage6-candidate-evidence-bundle-validation.v5'
		      OR NEW.evidence_bundle_assessment <> 'evidence-consistent-not-ga-approved'
		      OR length(NEW.desktop_artifact_set_sha256) <> 71
		      OR substr(NEW.desktop_artifact_set_sha256, 1, 7) <> 'sha256:'
		      OR substr(NEW.desktop_artifact_set_sha256, 8) GLOB '*[^0-9a-f]*'
		      OR NEW.desktop_artifact_set_sha256 = 'sha256:' || printf('%064d', 0)
		      OR json_valid(CAST(NEW.evidence_bundle_receipt AS TEXT)) <> 1
		      OR (SELECT count(*) FROM json_each(CAST(NEW.evidence_bundle_receipt AS TEXT))) <> 13
		      OR (SELECT count(*) FROM json_each(CAST(NEW.evidence_bundle_receipt AS TEXT)) WHERE key IN (
		        'allRequiredReceiptsReadyForCandidateReview', 'assessment', 'candidate',
		        'candidateConsistencyValidated', 'compatibilityMatrix', 'eligibleForCandidateEvidenceReview', 'environmentEligible',
		        'manifest', 'receipts', 'releaseEvidence', 'requiredReceiptCount', 'schemaVersion', 'validatedAt'
		      )) <> 13
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.schemaVersion') <> NEW.evidence_bundle_schema
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.assessment') <> NEW.evidence_bundle_assessment
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.candidateConsistencyValidated') <> 1
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.environmentEligible') <> 1
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.allRequiredReceiptsReadyForCandidateReview') <> 1
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.eligibleForCandidateEvidenceReview') <> 1
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.requiredReceiptCount') <> 10
		      OR (SELECT count(*) FROM json_each(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.receipts')) <> 10
		      OR (SELECT count(*) FROM json_each(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.receipts')
		          WHERE key IN ('internalCost', 'capacity', 'desktop', 'incident', 'operations', 'penetration', 'recovery', 'residency', 'slo', 'workerSupplyChain')) <> 10
		      OR (SELECT count(*) FROM json_each(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.candidate')) <> 10
		      OR (SELECT count(*) FROM json_each(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.candidate') WHERE key IN (
		        'artifacts', 'candidateId', 'desktopArtifactSetSha256', 'environmentClass', 'environmentId',
		        'lockfileSha256', 'migrationTail', 'origins', 'regions', 'sourceCommit'
		      )) <> 10
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.candidate.candidateId') <> NEW.candidate_id
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.candidate.sourceCommit') <> NEW.source_commit
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.candidate.lockfileSha256') <> lower(hex(NEW.lockfile_sha256))
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.candidate.environmentId') <> NEW.environment_id
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.candidate.environmentClass') NOT IN ('production', 'production-like')
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.candidate.desktopArtifactSetSha256') <> NEW.desktop_artifact_set_sha256
		      OR (SELECT count(*) FROM json_each(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.compatibilityMatrix')) <> 8
		      OR (SELECT count(*) FROM json_each(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.compatibilityMatrix') WHERE key IN (
		        'assessment', 'matrixVersion', 'migrationTail', 'path', 'schemaVersion', 'sha256', 'sourceByteCount', 'sourceFileCount'
		      )) <> 8
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.compatibilityMatrix.schemaVersion') <> 'synara.release-compatibility-matrix.v1'
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.compatibilityMatrix.assessment') <> 'source-compatible-not-release-approved'
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.compatibilityMatrix.matrixVersion') <> 1
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.compatibilityMatrix.sourceFileCount') < 1
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.compatibilityMatrix.sourceByteCount') < 1
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.compatibilityMatrix.migrationTail')
		         <> json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.candidate.migrationTail')
		      OR length(json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.compatibilityMatrix.path')) NOT BETWEEN 1 AND 512
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.compatibilityMatrix.path') LIKE '/%'
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.compatibilityMatrix.path') LIKE '%\%'
		      OR ('/' || json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.compatibilityMatrix.path') || '/') LIKE '%/../%'
		      OR ('/' || json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.compatibilityMatrix.path') || '/') LIKE '%/./%'
		      OR length(json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.compatibilityMatrix.sha256')) <> 71
		      OR substr(json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.compatibilityMatrix.sha256'), 1, 7) <> 'sha256:'
		      OR substr(json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.compatibilityMatrix.sha256'), 8) GLOB '*[^0-9a-f]*'
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.compatibilityMatrix.sha256') = 'sha256:' || printf('%064d', 0)
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.validatedAt') <> NEW.evidence_bundle_validated_at
		      OR length(NEW.final_asset_set_sha256) <> 32
		      OR length(NEW.environment_id) NOT BETWEEN 3 AND 300
		      OR json_array_length(NEW.impact_domains) NOT BETWEEN 1 AND 11
		      OR EXISTS (SELECT 1 FROM json_each(NEW.impact_domains) WHERE value NOT IN (
		        'code_change', 'data_migration', 'runtime_isolation', 'provider_commercial',
		        'internal_cost', 'personal_data', 'retention_legal_hold', 'data_residency',
		        'regulated_customer', 'desktop_distribution', 'security_incident'
		      ))
		      OR EXISTS (SELECT value FROM json_each(NEW.impact_domains) GROUP BY value HAVING count(*) > 1)
		      OR NEW.privacy_legal_required <> EXISTS (
		        SELECT 1 FROM json_each(NEW.impact_domains) WHERE value IN (
		          'provider_commercial', 'personal_data', 'retention_legal_hold',
		          'data_residency', 'regulated_customer', 'security_incident'
		        )
		      )
		      OR NEW.state <> 'draft' OR NEW.version <> 1
		      OR NEW.decision_summary IS NOT NULL OR NEW.residual_risk_disposition IS NOT NULL
		      OR NEW.residual_risks <> '[]' OR NEW.approved_at IS NOT NULL
		      OR NEW.released_at IS NOT NULL OR NEW.rejected_at IS NOT NULL OR NEW.rolled_back_at IS NOT NULL
		      OR NOT EXISTS (
		        SELECT 1 FROM tenant_memberships AS membership
		        JOIN tenants AS operator_tenant ON operator_tenant.id = membership.tenant_id
		        JOIN users AS creator ON creator.id = membership.user_id
		        WHERE membership.tenant_id = NEW.operator_tenant_id
		          AND membership.user_id = NEW.created_by
		          AND membership.status = 'active' AND membership.role IN ('owner', 'admin')
		          AND operator_tenant.status = 'active' AND operator_tenant.deleted_at IS NULL
		          AND creator.status = 'active' AND creator.deleted_at IS NULL
		     );
		 END`,
		`DROP TRIGGER IF EXISTS trg_stage6_release_candidate_projection_insert`,
		`CREATE TRIGGER trg_stage6_release_candidate_projection_insert
		 BEFORE INSERT ON stage6_release_candidates
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Stage 6 release receipt projection')
		   WHERE EXISTS (
		     SELECT 1 FROM json_each(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.receipts') AS receipt
		     WHERE receipt.type <> 'object'
		        OR (SELECT count(*) FROM json_each(receipt.value)) <> CASE receipt.key
		          WHEN 'desktop' THEN 7 WHEN 'recovery' THEN 10 WHEN 'residency' THEN 14 WHEN 'workerSupplyChain' THEN 8 ELSE 6 END
		        OR (SELECT count(*) FROM json_each(receipt.value) WHERE key IN (
		          'assessment', 'path', 'readyForCandidateReview', 'schemaVersion', 'sha256', 'validatedAt'
		        )) <> 6
		        OR (receipt.key = 'workerSupplyChain' AND (SELECT count(*) FROM json_each(receipt.value) WHERE key IN (
		          'admissionReportSha256', 'registryReportSha256'
		        )) <> 2)
		        OR json_extract(receipt.value, '$.readyForCandidateReview') <> 1
		        OR json_extract(receipt.value, '$.schemaVersion') <> CASE receipt.key
		          WHEN 'internalCost' THEN 'synara.stage6-internal-cost-evidence-validation.v1'
		          WHEN 'capacity' THEN 'synara.capacity-soak-evidence-receipt.v1'
		          WHEN 'desktop' THEN 'synara.stage6-desktop-native-acceptance-validation.v1'
		          WHEN 'incident' THEN 'synara.incident-communication-exercise-evidence-receipt.v2'
		          WHEN 'operations' THEN 'synara.stage6-operations-browser-exercise-validation.v2'
		          WHEN 'penetration' THEN 'synara.third-party-penetration-evidence-receipt.v1'
		          WHEN 'recovery' THEN 'synara.recovery-drill-evidence-receipt.v2'
		          WHEN 'residency' THEN 'synara.data-residency-deployment-evidence-receipt.v1'
		          WHEN 'slo' THEN 'synara.slo-window-evidence-receipt.v1'
		          WHEN 'workerSupplyChain' THEN 'synara.stage6-worker-supply-chain-evidence.v1' END
		        OR json_extract(receipt.value, '$.assessment') <> CASE receipt.key
		          WHEN 'internalCost' THEN 'evidence-validated-not-internal-cost-approved'
		          WHEN 'capacity' THEN 'evidence-validated-not-capacity-passed'
		          WHEN 'desktop' THEN 'evidence-validated-not-desktop-ga-passed'
		          WHEN 'incident' THEN 'evidence-validated-not-operations-ready'
		          WHEN 'operations' THEN 'evidence-validated-not-operations-passed'
		          WHEN 'penetration' THEN 'evidence-validated-not-penetration-passed'
		          WHEN 'recovery' THEN 'evidence-validated-not-control-passed'
		          WHEN 'residency' THEN 'evidence-validated-not-residency-approved'
		          WHEN 'slo' THEN 'evidence-validated-not-slo-passed'
		          WHEN 'workerSupplyChain' THEN 'evidence-validated-not-worker-supply-chain-approved' END
		        OR length(json_extract(receipt.value, '$.path')) NOT BETWEEN 1 AND 512
		        OR json_extract(receipt.value, '$.path') LIKE '/%'
		        OR json_extract(receipt.value, '$.path') LIKE '%\%'
		        OR ('/' || json_extract(receipt.value, '$.path') || '/') LIKE '%/../%'
		        OR ('/' || json_extract(receipt.value, '$.path') || '/') LIKE '%/./%'
		        OR length(json_extract(receipt.value, '$.sha256')) <> 71
		        OR substr(json_extract(receipt.value, '$.sha256'), 1, 7) <> 'sha256:'
		        OR substr(json_extract(receipt.value, '$.sha256'), 8) GLOB '*[^0-9a-f]*'
		        OR json_extract(receipt.value, '$.sha256') = 'sha256:' || printf('%064d', 0)
		        OR julianday(json_extract(receipt.value, '$.validatedAt')) > julianday(NEW.evidence_bundle_validated_at)
		   )
		      OR (SELECT count(DISTINCT path) FROM (
		        SELECT json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.manifest.path') AS path
		        UNION ALL SELECT json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.releaseEvidence.path')
		        UNION ALL SELECT json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.compatibilityMatrix.path')
		        UNION ALL SELECT json_extract(value, '$.path') FROM json_each(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.receipts')
		      )) <> 13
		      OR (SELECT count(*) FROM json_each(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.candidate.artifacts')) <> 6
		      OR (SELECT count(*) FROM json_each(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.candidate.artifacts') WHERE key IN (
		        'adminArtifact', 'controlPlaneImage', 'desktopArtifacts', 'providerHostImage', 'webArtifact', 'workerImage'
		      )) <> 6
		      OR EXISTS (
		        SELECT 1 FROM json_each(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.candidate.artifacts')
		        WHERE key <> 'desktopArtifacts' AND (
		          type <> 'text' OR length(value) <> 71 OR substr(value, 1, 7) <> 'sha256:'
		          OR substr(value, 8) GLOB '*[^0-9a-f]*' OR value = 'sha256:' || printf('%064d', 0)
		        )
		      )
		      OR (SELECT count(*) FROM json_each(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.candidate.artifacts.desktopArtifacts')) <> 4
		      OR (SELECT count(*) FROM json_each(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.candidate.artifacts.desktopArtifacts') WHERE key IN (
		        'linux-x64', 'macos-arm64', 'macos-x64', 'windows-x64'
		      )) <> 4
		      OR EXISTS (
		        SELECT 1 FROM json_each(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.candidate.artifacts.desktopArtifacts')
		        WHERE type <> 'text' OR length(value) <> 71 OR substr(value, 1, 7) <> 'sha256:'
		          OR substr(value, 8) GLOB '*[^0-9a-f]*' OR value = 'sha256:' || printf('%064d', 0)
		      )
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.receipts.desktop.desktopArtifactSetSha256')
		         <> json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.candidate.desktopArtifactSetSha256')
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.receipts.recovery.cryptographicSignaturesVerified') <> 0
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.receipts.recovery.realBackupRestoreAndApproverAuthorityVerificationRequired') <> 1
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.receipts.residency.cryptographicSignaturesVerified') <> 0
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.receipts.residency.externalSignatureIdentityAndAuthorityVerificationRequired') <> 1
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.receipts.residency.allowedRegions')
		         <> json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.candidate.regions')
		      OR length(json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.receipts.workerSupplyChain.registryReportSha256')) <> 71
		      OR substr(json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.receipts.workerSupplyChain.registryReportSha256'), 1, 7) <> 'sha256:'
		      OR substr(json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.receipts.workerSupplyChain.registryReportSha256'), 8) GLOB '*[^0-9a-f]*'
		      OR length(json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.receipts.workerSupplyChain.admissionReportSha256')) <> 71
		      OR substr(json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.receipts.workerSupplyChain.admissionReportSha256'), 1, 7) <> 'sha256:'
		      OR substr(json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.receipts.workerSupplyChain.admissionReportSha256'), 8) GLOB '*[^0-9a-f]*'
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.receipts.workerSupplyChain.registryReportSha256') = 'sha256:' || printf('%064d', 0)
		      OR json_extract(CAST(NEW.evidence_bundle_receipt AS TEXT), '$.receipts.workerSupplyChain.admissionReportSha256') = 'sha256:' || printf('%064d', 0);
		 END`,
		`DROP TRIGGER IF EXISTS trg_stage6_release_candidates_update`,
		`CREATE TRIGGER trg_stage6_release_candidates_update
		 BEFORE UPDATE ON stage6_release_candidates
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Stage 6 release candidate update')
		   WHERE NEW.id IS NOT OLD.id OR NEW.operator_tenant_id IS NOT OLD.operator_tenant_id
		      OR NEW.candidate_id <> OLD.candidate_id OR NEW.source_commit <> OLD.source_commit
		      OR NEW.lockfile_sha256 <> OLD.lockfile_sha256
		      OR NEW.evidence_bundle_sha256 <> OLD.evidence_bundle_sha256
		      OR NEW.evidence_bundle_receipt IS NOT OLD.evidence_bundle_receipt
		      OR NEW.evidence_bundle_schema IS NOT OLD.evidence_bundle_schema
		      OR NEW.evidence_bundle_assessment IS NOT OLD.evidence_bundle_assessment
		      OR NEW.evidence_bundle_validated_at IS NOT OLD.evidence_bundle_validated_at
		      OR NEW.evidence_bundle_receipt_size_bytes IS NOT OLD.evidence_bundle_receipt_size_bytes
		      OR NEW.desktop_artifact_set_sha256 IS NOT OLD.desktop_artifact_set_sha256
		      OR NEW.evidence_receipt_bound IS NOT OLD.evidence_receipt_bound
		      OR NEW.final_asset_set_sha256 <> OLD.final_asset_set_sha256
		      OR NEW.environment_id <> OLD.environment_id OR NEW.impact_domains <> OLD.impact_domains
		      OR NEW.privacy_legal_required <> OLD.privacy_legal_required OR NEW.created_by IS NOT OLD.created_by
		      OR NEW.created_at <> OLD.created_at OR NEW.version <> OLD.version + 1
		      OR (OLD.state = 'draft' AND NEW.state <> 'ready_for_review')
		      OR (NEW.state = 'ready_for_review' AND NEW.evidence_receipt_bound <> 1)
		      OR (OLD.state = 'ready_for_review' AND NEW.state NOT IN ('approved', 'rejected'))
		      OR (OLD.state = 'approved' AND NEW.state <> 'deploying')
		      OR (OLD.state = 'deploying' AND NEW.state NOT IN ('observing', 'rolled_back'))
		      OR (OLD.state = 'observing' AND NEW.state NOT IN ('released', 'rolled_back'))
		      OR OLD.state IN ('released', 'rejected', 'rolled_back')
		      OR (NEW.state = 'approved' AND (
		        NEW.approved_at IS NULL OR 4 <> (
		          SELECT count(*) FROM stage6_release_approvals
		          WHERE candidate_record_id = NEW.id AND decision = 'approved'
		            AND approval_role IN ('engineering', 'operations', 'security', 'product')
		            AND evidence_sha256 IS NOT NULL
		        ) OR (NEW.privacy_legal_required = 1 AND 1 <> (
		          SELECT count(*) FROM stage6_release_approvals
		          WHERE candidate_record_id = NEW.id AND decision = 'approved'
		            AND approval_role = 'privacy_legal'
		            AND evidence_sha256 IS NOT NULL
		        ))
		      ))
		      OR (NEW.state = 'released' AND (
		        NEW.released_at IS NULL
		        OR length(trim(ifnull(NEW.decision_summary, ''))) NOT BETWEEN 20 AND 4000
		        OR NEW.residual_risk_disposition NOT IN ('none', 'accepted')
		        OR (NEW.residual_risk_disposition = 'none' AND NEW.residual_risks <> '[]')
		        OR (NEW.residual_risk_disposition = 'accepted' AND json_array_length(NEW.residual_risks) = 0)
		        OR NOT EXISTS (
		          SELECT 1 FROM stage6_release_final_reviews AS review
		          WHERE review.candidate_record_id = NEW.id
		            AND review.operator_tenant_id = NEW.operator_tenant_id
		            AND review.all_required_controls_passed = 1
		            AND review.all_required_final_approvals_approved = 1
		            AND review.eligible_for_external_ga_authority_review = 1
		            AND review.external_evidence_authority_verified = 0
		            AND review.approver_corporate_authority_verified = 0
		            AND review.external_signatures_verified = 0
		            AND review.publication_delivery_verified_by_synara = 0
		            AND review.decision_summary = NEW.decision_summary
		            AND review.residual_risk_disposition = NEW.residual_risk_disposition
		            AND review.residual_risks = NEW.residual_risks
		        )
		      ));
		 END`,
		`DROP TRIGGER IF EXISTS trg_stage6_release_final_reviews_insert`,
		`CREATE TRIGGER trg_stage6_release_final_reviews_insert
		 BEFORE INSERT ON stage6_release_final_reviews
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Stage 6 Final Review')
		   WHERE length(NEW.receipt_sha256) <> 32
		      OR synara_sha256(NEW.receipt) <> NEW.receipt_sha256
		      OR NEW.receipt_size_bytes NOT BETWEEN 1 AND 524288
		      OR length(NEW.receipt) <> NEW.receipt_size_bytes
		      OR json_valid(CAST(NEW.receipt AS TEXT)) <> 1
		      OR NEW.schema_version <> 'synara.stage6-final-ga-review-validation.v1'
		      OR NEW.assessment <> 'final-review-consistent-not-ga-authority-verified'
		      OR (SELECT count(*) FROM json_each(CAST(NEW.receipt AS TEXT))) <> 21
		      OR (SELECT count(*) FROM json_each(CAST(NEW.receipt AS TEXT), '$.candidate')) <> 15
		      OR (SELECT count(*) FROM json_each(CAST(NEW.receipt AS TEXT), '$.controlStatusCounts')) <> 3
		      OR (SELECT count(*) FROM json_each(CAST(NEW.receipt AS TEXT), '$.verificationBoundary')) <> 4
		      OR json_type(CAST(NEW.receipt AS TEXT), '$.residualRisks') <> 'array'
		      OR json_extract(CAST(NEW.receipt AS TEXT), '$.schemaVersion') <> NEW.schema_version
		      OR json_extract(CAST(NEW.receipt AS TEXT), '$.assessment') <> NEW.assessment
		      OR abs(julianday(json_extract(CAST(NEW.receipt AS TEXT), '$.validatedAt')) - julianday(NEW.validated_at)) > 0.000001
		      OR NEW.control_inventory_sha256 <> 'sha256:8ea250269216ebcee540e7a30eef3e2c3c275694d9ab808aeeaa0b1b6efefced'
		      OR json_extract(CAST(NEW.receipt AS TEXT), '$.controlInventorySha256') <> NEW.control_inventory_sha256
		      OR NEW.control_count <> 32
		      OR json_extract(CAST(NEW.receipt AS TEXT), '$.controlCount') <> 32
		      OR json_array_length(CAST(NEW.receipt AS TEXT), '$.controlDecisions') <> 32
		      OR json_extract(CAST(NEW.receipt AS TEXT), '$.controlStatusCounts.passed') <> 32
		      OR json_extract(CAST(NEW.receipt AS TEXT), '$.controlStatusCounts.failed') <> 0
		      OR json_extract(CAST(NEW.receipt AS TEXT), '$.controlStatusCounts.blocked') <> 0
		      OR json_array_length(CAST(NEW.receipt AS TEXT), '$.finalApprovals') <> NEW.final_approval_count
		      OR NEW.all_required_controls_passed <> 1
		      OR NEW.all_required_final_approvals_approved <> 1
		      OR NEW.eligible_for_external_ga_authority_review <> 1
		      OR NEW.external_evidence_authority_verified <> 0
		      OR NEW.approver_corporate_authority_verified <> 0
		      OR NEW.external_signatures_verified <> 0
		      OR NEW.publication_delivery_verified_by_synara <> 0
		      OR json_extract(CAST(NEW.receipt AS TEXT), '$.allRequiredControlsPassed') <> 1
		      OR json_extract(CAST(NEW.receipt AS TEXT), '$.allRequiredFinalApprovalsApproved') <> 1
		      OR json_extract(CAST(NEW.receipt AS TEXT), '$.eligibleForExternalGAAuthorityReview') <> 1
		      OR json_extract(CAST(NEW.receipt AS TEXT), '$.verificationBoundary.externalEvidenceAuthorityVerified') <> 0
		      OR json_extract(CAST(NEW.receipt AS TEXT), '$.verificationBoundary.approverCorporateAuthorityVerified') <> 0
		      OR json_extract(CAST(NEW.receipt AS TEXT), '$.verificationBoundary.externalSignaturesVerified') <> 0
		      OR json_extract(CAST(NEW.receipt AS TEXT), '$.verificationBoundary.publicationDeliveryVerifiedBySynara') <> 0
		      OR json_extract(CAST(NEW.receipt AS TEXT), '$.decisionSummary') <> NEW.decision_summary
		      OR json_extract(CAST(NEW.receipt AS TEXT), '$.residualRiskDisposition') <> NEW.residual_risk_disposition
		      OR json_extract(CAST(NEW.receipt AS TEXT), '$.residualRisks') <> NEW.residual_risks
		      OR julianday(json_extract(CAST(NEW.receipt AS TEXT), '$.completedAt')) <= julianday(json_extract(CAST(NEW.receipt AS TEXT), '$.startedAt'))
		      OR julianday(json_extract(CAST(NEW.receipt AS TEXT), '$.validatedAt')) < julianday(json_extract(CAST(NEW.receipt AS TEXT), '$.completedAt'))
		      OR julianday(json_extract(CAST(NEW.receipt AS TEXT), '$.candidate.protectedReleaseApprovedAt')) < julianday(json_extract(CAST(NEW.receipt AS TEXT), '$.candidate.candidateValidatedAt'))
		      OR julianday(json_extract(CAST(NEW.receipt AS TEXT), '$.candidate.protectedReleaseApprovedAt')) > julianday(json_extract(CAST(NEW.receipt AS TEXT), '$.completedAt'))
		      OR length(trim(json_extract(CAST(NEW.receipt AS TEXT), '$.releaseManagerId'))) NOT BETWEEN 2 AND 200
		      OR (SELECT count(DISTINCT json_extract(control.value, '$.id'))
		          FROM json_each(CAST(NEW.receipt AS TEXT), '$.controlDecisions') AS control) <> 32
		      OR EXISTS (
		        SELECT 1
		        FROM json_each(CAST(NEW.receipt AS TEXT), '$.controlDecisions') AS control
		        WHERE (SELECT count(*) FROM json_each(control.value)) <> 6
		           OR json_extract(control.value, '$.ownerRole') <> CASE json_extract(control.value, '$.id')
		             WHEN 'tenant-registration' THEN 'product'
		             WHEN 'tenant-lifecycle' THEN 'operations'
		             WHEN 'identity-lifecycle' THEN 'security'
		             WHEN 'sso-domain' THEN 'security'
		             WHEN 'identity-governance' THEN 'security'
		             WHEN 'offer-admission' THEN 'product'
		             WHEN 'usage-reconciliation' THEN 'engineering'
		             WHEN 'quota-concurrency' THEN 'engineering'
		             WHEN 'usage-explainability' THEN 'product'
		             WHEN 'internal-cost-reconciliation' THEN 'operations'
		             WHEN 'operations-matrix' THEN 'operations'
		             WHEN 'authority-views' THEN 'operations'
		             WHEN 'support-access' THEN 'security'
		             WHEN 'incident-communications' THEN 'operations'
		             WHEN 'audit-retention-legal-hold' THEN 'security'
		             WHEN 'privacy-workflows' THEN 'privacy_legal'
		             WHEN 'data-residency' THEN 'privacy_legal'
		             WHEN 'provider-compliance' THEN 'privacy_legal'
		             WHEN 'compliance-program' THEN 'security'
		             WHEN 'desktop-native' THEN 'engineering'
		             WHEN 'desktop-security' THEN 'security'
		             WHEN 'desktop-postgres-concurrency' THEN 'engineering'
		             WHEN 'recovery' THEN 'operations'
		             WHEN 'slo' THEN 'operations'
		             WHEN 'tracing-isolation' THEN 'security'
		             WHEN 'penetration-stage5' THEN 'security'
		             WHEN 'worker-supply-chain' THEN 'security'
		             WHEN 'compatibility-rollback' THEN 'engineering'
		             WHEN 'key-rotation' THEN 'security'
		             WHEN 'capacity' THEN 'operations'
		             WHEN 'documentation' THEN 'product'
		             WHEN 'change-notice' THEN 'product'
		             ELSE NULL
		           END
		           OR json_extract(control.value, '$.status') <> 'passed'
		           OR length(trim(json_extract(control.value, '$.approverId'))) NOT BETWEEN 2 AND 200
		           OR json_type(control.value, '$.evidence') <> 'array'
		           OR json_array_length(control.value, '$.evidence') < 1
		           OR julianday(json_extract(control.value, '$.decidedAt')) < julianday(json_extract(CAST(NEW.receipt AS TEXT), '$.startedAt'))
		           OR julianday(json_extract(control.value, '$.decidedAt')) > julianday(json_extract(CAST(NEW.receipt AS TEXT), '$.completedAt'))
		           OR EXISTS (
		             SELECT 1 FROM json_each(control.value, '$.evidence') AS evidence
		             WHERE (SELECT count(*) FROM json_each(evidence.value)) <> 2
		                OR length(json_extract(evidence.value, '$.path')) NOT BETWEEN 1 AND 512
		                OR json_extract(evidence.value, '$.path') LIKE '/%'
		                OR json_extract(evidence.value, '$.path') LIKE '%\\%'
		                OR json_extract(evidence.value, '$.path') LIKE '%//%'
		                OR json_extract(evidence.value, '$.path') GLOB '*../*'
		                OR length(json_extract(evidence.value, '$.sha256')) <> 71
		                OR json_extract(evidence.value, '$.sha256') NOT LIKE 'sha256:%'
		                OR substr(json_extract(evidence.value, '$.sha256'), 8) GLOB '*[^0-9a-f]*'
		           )
		      )
		      OR (SELECT count(DISTINCT json_extract(approval.value, '$.role'))
		          FROM json_each(CAST(NEW.receipt AS TEXT), '$.finalApprovals') AS approval) <> NEW.final_approval_count
		      OR (SELECT count(DISTINCT json_extract(approval.value, '$.approverId'))
		          FROM json_each(CAST(NEW.receipt AS TEXT), '$.finalApprovals') AS approval) <> NEW.final_approval_count
		      OR EXISTS (
		        SELECT 1
		        FROM json_each(CAST(NEW.receipt AS TEXT), '$.finalApprovals') AS approval
		        WHERE (SELECT count(*) FROM json_each(approval.value)) <> 5
		           OR json_extract(approval.value, '$.role') NOT IN ('engineering', 'operations', 'security', 'product', 'privacy_legal')
		           OR json_extract(approval.value, '$.approverId') = json_extract(CAST(NEW.receipt AS TEXT), '$.releaseManagerId')
		           OR length(trim(json_extract(approval.value, '$.approverId'))) NOT BETWEEN 2 AND 200
		           OR json_extract(approval.value, '$.decision') <> 'approved-for-ga-authority-review'
		           OR julianday(json_extract(approval.value, '$.approvedAt')) < (
		             SELECT max(julianday(json_extract(control.value, '$.decidedAt')))
		             FROM json_each(CAST(NEW.receipt AS TEXT), '$.controlDecisions') AS control
		           )
		           OR julianday(json_extract(approval.value, '$.approvedAt')) < julianday(json_extract(CAST(NEW.receipt AS TEXT), '$.candidate.protectedReleaseApprovedAt'))
		           OR julianday(json_extract(approval.value, '$.approvedAt')) > julianday(json_extract(CAST(NEW.receipt AS TEXT), '$.completedAt'))
		           OR (SELECT count(*) FROM json_each(approval.value, '$.evidence')) <> 2
		      )
		      OR json_array_length(CAST(NEW.receipt AS TEXT), '$.platformAuditRequestIds') < 1
		      OR (SELECT count(DISTINCT request.value)
		          FROM json_each(CAST(NEW.receipt AS TEXT), '$.platformAuditRequestIds') AS request)
		         <> json_array_length(CAST(NEW.receipt AS TEXT), '$.platformAuditRequestIds')
		      OR (NEW.residual_risk_disposition = 'accepted' AND EXISTS (
		        SELECT 1 FROM json_each(CAST(NEW.receipt AS TEXT), '$.residualRisks') AS risk
		        WHERE (SELECT count(*) FROM json_each(risk.value)) <> 6
		           OR length(trim(json_extract(risk.value, '$.summary'))) NOT BETWEEN 10 AND 500
		           OR length(trim(json_extract(risk.value, '$.owner'))) NOT BETWEEN 3 AND 200
		           OR length(trim(json_extract(risk.value, '$.acceptanceReason'))) NOT BETWEEN 10 AND 1000
		           OR json_extract(risk.value, '$.evidenceReference') NOT LIKE 'https://%'
		           OR julianday(json_extract(risk.value, '$.dueAt')) <= julianday(json_extract(CAST(NEW.receipt AS TEXT), '$.completedAt'))
		      ))
		      OR (NEW.residual_risk_disposition = 'accepted' AND (
		        SELECT count(DISTINCT json_extract(risk.value, '$.id'))
		        FROM json_each(CAST(NEW.receipt AS TEXT), '$.residualRisks') AS risk
		      ) <> json_array_length(CAST(NEW.receipt AS TEXT), '$.residualRisks'))
		      OR NOT EXISTS (
		        SELECT 1 FROM stage6_release_candidates AS candidate
		        JOIN tenant_memberships AS membership
		          ON membership.tenant_id = candidate.operator_tenant_id
		         AND membership.user_id = NEW.bound_by
		         AND membership.status = 'active' AND membership.role IN ('owner', 'admin')
		        JOIN tenants AS operator_tenant
		          ON operator_tenant.id = membership.tenant_id
		         AND operator_tenant.status = 'active' AND operator_tenant.deleted_at IS NULL
		        JOIN users AS actor
		          ON actor.id = membership.user_id
		         AND actor.status = 'active' AND actor.deleted_at IS NULL
		        WHERE candidate.id = NEW.candidate_record_id
		          AND candidate.operator_tenant_id = NEW.operator_tenant_id
		          AND candidate.state = 'observing'
		          AND json_extract(CAST(NEW.receipt AS TEXT), '$.candidate.candidateId') = candidate.candidate_id
		          AND json_extract(CAST(NEW.receipt AS TEXT), '$.candidate.releaseTag') = candidate.candidate_id
		          AND json_extract(CAST(NEW.receipt AS TEXT), '$.candidate.sourceCommit') = candidate.source_commit
		          AND json_extract(CAST(NEW.receipt AS TEXT), '$.candidate.environmentId') = candidate.environment_id
		          AND json_extract(CAST(NEW.receipt AS TEXT), '$.candidate.lockfileSha256') = lower(hex(candidate.lockfile_sha256))
		          AND json_extract(CAST(NEW.receipt AS TEXT), '$.candidate.desktopArtifactSetSha256') = candidate.desktop_artifact_set_sha256
		          AND json_extract(CAST(NEW.receipt AS TEXT), '$.candidate.finalAssetSetSha256') = 'sha256:' || lower(hex(candidate.final_asset_set_sha256))
		          AND json_extract(CAST(NEW.receipt AS TEXT), '$.candidate.candidateBundleReceipt.sha256') = 'sha256:' || lower(hex(candidate.evidence_bundle_sha256))
		          AND json_extract(CAST(NEW.receipt AS TEXT), '$.candidate.candidateValidatedAt') = candidate.evidence_bundle_validated_at
		          AND json_extract(CAST(NEW.receipt AS TEXT), '$.impactDomains') = candidate.impact_domains
		          AND NEW.final_approval_count = CASE WHEN candidate.privacy_legal_required = 1 THEN 5 ELSE 4 END
		          AND 4 = (
		            SELECT count(*) FROM json_each(CAST(NEW.receipt AS TEXT), '$.finalApprovals') AS approval
		            WHERE json_extract(approval.value, '$.role') IN ('engineering', 'operations', 'security', 'product')
		          )
		          AND CASE WHEN candidate.privacy_legal_required = 1 THEN 1 ELSE 0 END = (
		            SELECT count(*) FROM json_each(CAST(NEW.receipt AS TEXT), '$.finalApprovals') AS approval
		            WHERE json_extract(approval.value, '$.role') = 'privacy_legal'
		          )
		      );
		 END`,
		`DROP TRIGGER IF EXISTS trg_stage6_release_final_reviews_no_update`,
		`CREATE TRIGGER trg_stage6_release_final_reviews_no_update
		 BEFORE UPDATE ON stage6_release_final_reviews
		 BEGIN SELECT RAISE(ABORT, 'Stage 6 Final Review records are immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_release_final_reviews_no_delete`,
		`CREATE TRIGGER trg_stage6_release_final_reviews_no_delete
		 BEFORE DELETE ON stage6_release_final_reviews
		 BEGIN SELECT RAISE(ABORT, 'Stage 6 Final Review records are immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_release_slo_approval_gate`,
		`CREATE TRIGGER trg_stage6_release_slo_approval_gate
		 BEFORE UPDATE ON stage6_release_candidates
		 WHEN NEW.state = 'approved' AND OLD.state = 'ready_for_review'
		 BEGIN
		   SELECT RAISE(ABORT, 'Stage 6 release approval requires an approved eligible SLO window bound to the candidate')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM stage6_slo_windows AS slo
		     WHERE slo.candidate_record_id = NEW.id
		       AND slo.operator_tenant_id = NEW.operator_tenant_id
		       AND slo.state = 'approved'
		       AND slo.eligible_for_human_gate_review = 1
		       AND slo.all_objectives_assessable = 1
		       AND slo.all_objectives_met = 1
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_stage6_release_recovery_approval_gate`,
		`CREATE TRIGGER trg_stage6_release_recovery_approval_gate
		 BEFORE UPDATE ON stage6_release_candidates
		 WHEN NEW.state = 'approved' AND OLD.state = 'ready_for_review'
		 BEGIN
		   SELECT RAISE(ABORT, 'Stage 6 release approval requires an approved eligible Recovery drill bound to the candidate')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM stage6_recovery_drills AS recovery
		     WHERE recovery.candidate_record_id = NEW.id
		       AND recovery.operator_tenant_id = NEW.operator_tenant_id
		       AND recovery.state = 'approved'
		       AND recovery.eligible_for_human_gate_review = 1
		       AND recovery.measurements_within_objectives = 1
		       AND recovery.all_restore_canaries_passed = 1
		       AND recovery.all_source_approvals_approved = 1
		       AND recovery.cryptographic_signatures_verified = 0
		       AND recovery.external_authority_verification_required = 1
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_stage6_release_penetration_approval_gate`,
		`CREATE TRIGGER trg_stage6_release_penetration_approval_gate
		 BEFORE UPDATE ON stage6_release_candidates
		 WHEN NEW.state = 'approved' AND OLD.state = 'ready_for_review'
		 BEGIN
		   SELECT RAISE(ABORT, 'Stage 6 release approval requires an approved eligible Penetration engagement bound to the candidate')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM stage6_penetration_engagements AS penetration
		     WHERE penetration.candidate_record_id = NEW.id
		       AND penetration.operator_tenant_id = NEW.operator_tenant_id
		       AND penetration.state = 'approved'
		       AND penetration.third_party_independence_declared = 1
		       AND penetration.stage5_dependency_satisfied = 1
		       AND penetration.asset_coverage_complete = 1
		       AND penetration.scope_coverage_complete = 1
		       AND penetration.methodology_coverage_complete = 1
		       AND penetration.no_unaccepted_high_or_critical_findings = 1
		       AND penetration.eligible_for_human_gate_review = 1
		       AND penetration.cryptographic_signatures_verified = 0
		       AND penetration.external_authority_verification_required = 1
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_stage6_release_capacity_approval_gate`,
		`CREATE TRIGGER trg_stage6_release_capacity_approval_gate
		 BEFORE UPDATE ON stage6_release_candidates
		 WHEN NEW.state = 'approved' AND OLD.state = 'ready_for_review'
		 BEGIN
		   SELECT RAISE(ABORT, 'Stage 6 release approval requires an approved eligible Capacity run bound to the candidate')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM stage6_capacity_runs AS capacity
		     WHERE capacity.candidate_record_id = NEW.id
		       AND capacity.operator_tenant_id = NEW.operator_tenant_id
		       AND capacity.state = 'approved'
		       AND capacity.forecast_headroom_covered = 1
		       AND capacity.phase_coverage_complete = 1
		       AND capacity.exercise_coverage_complete = 1
		       AND capacity.measurements_within_objectives = 1
		       AND capacity.release_eligible_environment = 1
		       AND capacity.eligible_for_human_gate_review = 1
		       AND capacity.cryptographic_signatures_verified = 0
		       AND capacity.external_authority_verification_required = 1
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_stage6_release_incident_exercise_approval_gate`,
		`CREATE TRIGGER trg_stage6_release_incident_exercise_approval_gate
		 BEFORE UPDATE ON stage6_release_candidates
		 WHEN NEW.state = 'approved' AND OLD.state = 'ready_for_review'
		 BEGIN
		   SELECT RAISE(ABORT, 'Stage 6 release approval requires an approved eligible Incident exercise bound to the candidate')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM stage6_incident_exercises AS exercise
		     WHERE exercise.candidate_record_id = NEW.id
		       AND exercise.operator_tenant_id = NEW.operator_tenant_id
		       AND exercise.state = 'approved'
		       AND exercise.independent_status_page_declared = 1
		       AND exercise.role_separation_complete = 1
		       AND exercise.paging_exercise_complete = 1
		       AND exercise.status_page_components_complete = 1
		       AND exercise.public_timeline_within_targets = 1
		       AND exercise.subscriber_delivery_complete = 1
		       AND exercise.recovery_verification_complete = 1
		       AND exercise.review_complete = 1
		       AND exercise.release_eligible_environment = 1
		       AND exercise.eligible_for_human_gate_review = 1
		       AND exercise.cryptographic_signatures_verified = 0
		       AND exercise.external_authority_verification_required = 1
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_stage6_release_operations_exercise_approval_gate`,
		`CREATE TRIGGER trg_stage6_release_operations_exercise_approval_gate
		 BEFORE UPDATE ON stage6_release_candidates
		 WHEN NEW.state = 'approved' AND OLD.state = 'ready_for_review'
		 BEGIN
		   SELECT RAISE(ABORT, 'Stage 6 release approval requires an approved eligible Operations exercise bound to the candidate')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM stage6_operations_exercises AS exercise
		     WHERE exercise.candidate_record_id = NEW.id
		       AND exercise.operator_tenant_id = NEW.operator_tenant_id
		       AND exercise.state = 'approved'
		       AND exercise.all_operations_passed = 1
		       AND exercise.all_negative_authorizations_denied = 1
		       AND exercise.no_developer_fallbacks = 1
		       AND exercise.production_authentication_declared = 1
		       AND exercise.support_lifecycle_complete = 1
		       AND exercise.receipt_approvals_complete = 1
		       AND exercise.release_eligible_environment = 1
		       AND exercise.eligible_for_human_gate_review = 1
		       AND exercise.cryptographic_signatures_verified = 0
		       AND exercise.external_authority_verification_required = 1
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_stage6_release_billing_exercise_approval_gate`,
		`CREATE TRIGGER trg_stage6_release_billing_exercise_approval_gate
		 BEFORE UPDATE ON stage6_release_candidates
		 WHEN NEW.state = 'approved' AND OLD.state = 'ready_for_review'
		 BEGIN
		   SELECT RAISE(ABORT, 'Stage 6 release approval requires an approved eligible Billing exercise bound to the candidate')
		   WHERE NOT EXISTS (
		     SELECT 1 FROM stage6_billing_exercises AS exercise
		     WHERE exercise.candidate_record_id = NEW.id
		       AND exercise.operator_tenant_id = NEW.operator_tenant_id
		       AND exercise.state = 'approved'
		       AND exercise.stripe_mode = 'live'
		       AND exercise.live_mode = 1
		       AND exercise.all_scenarios_passed = 1
		       AND exercise.amounts_match = 1
		       AND exercise.cardinality_matches = 1
		       AND exercise.raw_webhook_payload_stored = 0
		       AND exercise.card_data_handled_by_synara = 0
		       AND exercise.receipt_approvals_complete = 1
		       AND exercise.eligible_for_human_gate_review = 1
		       AND exercise.cryptographic_signatures_verified = 0
		       AND exercise.external_authority_verification_required = 1
			 );
		 END`,
		`DROP TRIGGER IF EXISTS trg_stage6_release_provider_authorization_binding_insert`,
		`CREATE TRIGGER trg_stage6_release_provider_authorization_binding_insert
		 BEFORE INSERT ON stage6_release_provider_authorization_bindings
		 BEGIN
		   SELECT RAISE(ABORT, 'Stage 6 release Provider authorization binding requires an active exact byte-bound authorization')
		   WHERE NOT EXISTS (
		     SELECT 1
		     FROM stage6_release_candidates AS candidate
		     JOIN provider_commercial_authorizations AS authorization
		       ON authorization.id = NEW.authorization_id
		      AND authorization.operator_tenant_id = NEW.operator_tenant_id
		     WHERE candidate.id = NEW.candidate_record_id
		       AND candidate.operator_tenant_id = NEW.operator_tenant_id
		       AND candidate.state = 'draft'
		       AND EXISTS (SELECT 1 FROM json_each(candidate.impact_domains) WHERE value = 'provider_commercial')
		       AND NEW.bound_at = candidate.created_at
		       AND authorization.state = 'active'
		       AND authorization.version = NEW.authorization_version
		       AND authorization.provider = NEW.provider
		       AND authorization.authorization_key = NEW.authorization_key
		       AND julianday(authorization.review_expires_at) > julianday(NEW.bound_at)
		       AND authorization.terms_sha256 IS NOT NULL
		       AND authorization.agreement_sha256 IS NOT NULL
		       AND authorization.dpa_sha256 IS NOT NULL
		       AND authorization.termination_runbook_sha256 IS NOT NULL
		       AND 4 = (
		         SELECT count(*)
		         FROM provider_commercial_authorization_approvals AS approval
		         WHERE approval.authorization_id = authorization.id
		           AND approval.decision = 'approved'
		           AND approval.approval_role IN ('legal', 'privacy', 'security', 'product')
		           AND approval.evidence_sha256 IS NOT NULL
		           AND approval.evidence_sha256 <> 'sha256:' || printf('%064d', 0)
		       )
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_stage6_release_provider_authorization_binding_update`,
		`CREATE TRIGGER trg_stage6_release_provider_authorization_binding_update
		 BEFORE UPDATE ON stage6_release_provider_authorization_bindings
		 BEGIN SELECT RAISE(ABORT, 'Stage 6 release Provider authorization bindings are immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_release_provider_authorization_binding_delete`,
		`CREATE TRIGGER trg_stage6_release_provider_authorization_binding_delete
		 BEFORE DELETE ON stage6_release_provider_authorization_bindings
		 BEGIN SELECT RAISE(ABORT, 'Stage 6 release Provider authorization bindings are immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_release_candidate_provider_authorization_gate`,
		`CREATE TRIGGER trg_stage6_release_candidate_provider_authorization_gate
		 BEFORE UPDATE ON stage6_release_candidates
		 WHEN NEW.state <> OLD.state AND NEW.state NOT IN ('rejected', 'rolled_back')
		 BEGIN
		   SELECT RAISE(ABORT, 'Stage 6 release transition requires active exact Provider commercial authorization bindings')
		   WHERE (
		     EXISTS (SELECT 1 FROM json_each(NEW.impact_domains) WHERE value = 'provider_commercial')
		     AND (
		       NOT EXISTS (
		         SELECT 1 FROM stage6_release_provider_authorization_bindings
		         WHERE candidate_record_id = NEW.id AND operator_tenant_id = NEW.operator_tenant_id
		       )
		       OR EXISTS (
		         SELECT 1
		         FROM stage6_release_provider_authorization_bindings AS binding
		         LEFT JOIN provider_commercial_authorizations AS authorization
		           ON authorization.id = binding.authorization_id
		         WHERE binding.candidate_record_id = NEW.id
		           AND binding.operator_tenant_id = NEW.operator_tenant_id
		           AND (
		             authorization.id IS NULL
		             OR authorization.operator_tenant_id <> binding.operator_tenant_id
		             OR authorization.state <> 'active'
		             OR authorization.version <> binding.authorization_version
		             OR authorization.provider <> binding.provider
		             OR authorization.authorization_key <> binding.authorization_key
		             OR julianday(authorization.review_expires_at) <= julianday(NEW.updated_at)
		             OR authorization.terms_sha256 IS NULL
		             OR authorization.agreement_sha256 IS NULL
		             OR authorization.dpa_sha256 IS NULL
		             OR authorization.termination_runbook_sha256 IS NULL
		             OR 4 <> (
		               SELECT count(*)
		               FROM provider_commercial_authorization_approvals AS approval
		               WHERE approval.authorization_id = binding.authorization_id
		                 AND approval.decision = 'approved'
		                 AND approval.approval_role IN ('legal', 'privacy', 'security', 'product')
		                 AND approval.evidence_sha256 IS NOT NULL
		                 AND approval.evidence_sha256 <> 'sha256:' || printf('%064d', 0)
		             )
		           )
		       )
		     )
		   ) OR (
		     NOT EXISTS (SELECT 1 FROM json_each(NEW.impact_domains) WHERE value = 'provider_commercial')
		     AND EXISTS (
		       SELECT 1 FROM stage6_release_provider_authorization_bindings
		       WHERE candidate_record_id = NEW.id AND operator_tenant_id = NEW.operator_tenant_id
		     )
		   );
		 END`,
		`DROP TRIGGER IF EXISTS trg_stage6_release_candidate_approval_evidence_gate`,
		`CREATE TRIGGER trg_stage6_release_candidate_approval_evidence_gate
		 BEFORE UPDATE ON stage6_release_candidates
		 WHEN NEW.state <> OLD.state AND NEW.state IN ('approved', 'deploying', 'observing', 'released')
		 BEGIN
		   SELECT RAISE(ABORT, 'Stage 6 release transition requires byte-bound impact-derived approval evidence')
		   WHERE 4 <> (
		     SELECT count(*) FROM stage6_release_approvals
		     WHERE candidate_record_id = NEW.id AND decision = 'approved'
		       AND approval_role IN ('engineering', 'operations', 'security', 'product')
		       AND evidence_sha256 IS NOT NULL
		       AND evidence_sha256 <> 'sha256:' || printf('%064d', 0)
		   ) OR (NEW.privacy_legal_required = 1 AND 1 <> (
		     SELECT count(*) FROM stage6_release_approvals
		     WHERE candidate_record_id = NEW.id AND decision = 'approved'
		       AND approval_role = 'privacy_legal'
		       AND evidence_sha256 IS NOT NULL
		       AND evidence_sha256 <> 'sha256:' || printf('%064d', 0)
		   ));
		 END`,
		`DROP TRIGGER IF EXISTS trg_stage6_release_approvals_insert`,
		`CREATE TRIGGER trg_stage6_release_approvals_insert
		 BEFORE INSERT ON stage6_release_approvals
		 BEGIN
		   SELECT RAISE(ABORT, 'invalid Stage 6 release approval')
		   WHERE NEW.approval_role NOT IN ('engineering', 'operations', 'security', 'product', 'privacy_legal')
		      OR NEW.decision NOT IN ('approved', 'rejected')
		      OR length(trim(NEW.reason)) NOT BETWEEN 10 AND 2000
		      OR length(trim(NEW.evidence_reference)) NOT BETWEEN 8 AND 2048
		      OR NEW.evidence_sha256 IS NULL
		      OR length(NEW.evidence_sha256) <> 71
		      OR substr(NEW.evidence_sha256, 1, 7) <> 'sha256:'
		      OR substr(NEW.evidence_sha256, 8) GLOB '*[^0-9a-f]*'
		      OR NEW.evidence_sha256 = 'sha256:' || printf('%064d', 0)
		      OR NOT EXISTS (
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
		      );
		 END`,
		`DROP TRIGGER IF EXISTS trg_stage6_release_approvals_no_update`,
		`CREATE TRIGGER trg_stage6_release_approvals_no_update
		 BEFORE UPDATE ON stage6_release_approvals
		 BEGIN SELECT RAISE(ABORT, 'Stage 6 release approvals are immutable'); END`,
		`DROP TRIGGER IF EXISTS trg_stage6_release_approvals_no_delete`,
		`CREATE TRIGGER trg_stage6_release_approvals_no_delete
		 BEFORE DELETE ON stage6_release_approvals
		 BEGIN SELECT RAISE(ABORT, 'Stage 6 release approvals are immutable'); END`,
	}
	for _, statement := range statements {
		if err := db.WithContext(ctx).Exec(statement).Error; err != nil {
			return fmt.Errorf("apply SQLite Stage 6 release governance safety migration: %w", err)
		}
	}
	return nil
}
