from __future__ import annotations

import copy
import hashlib
import json
import pathlib
import stat
import subprocess
import sys
import tempfile
import unittest


SCRIPT = pathlib.Path(__file__).with_name("validate_data_residency_evidence.py")
EVIDENCE_NAMES = {
    "annex",
    "browserStatement",
    "evacuationExercise",
    "failoverExercise",
    "operationsApproval",
    "privacyLegalApproval",
    "runtimeInventory",
    "securityApproval",
}
BASE_EVIDENCE_NAMES = {
    "annex",
    "browserStatement",
    "evacuationExercise",
    "failoverExercise",
    "runtimeInventory",
}


class ValidateDataResidencyEvidenceTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temporary.name)
        self.evidence_root = self.root / "evidence"
        self.evidence_root.mkdir()
        self.candidate = {
            "candidateId": "stage6-rc1",
            "sourceCommit": "a" * 40,
            "lockfileSha256": "b" * 64,
            "environmentClass": "production-like",
            "environmentId": "production-like/stage6-rc1",
            "artifacts": {
                "controlPlaneImage": "sha256:" + "1" * 64,
                "workerImage": "sha256:" + "2" * 64,
                "providerHostImage": "sha256:" + "3" * 64,
                "webArtifact": "sha256:" + "4" * 64,
                "adminArtifact": "sha256:" + "5" * 64,
                "desktopArtifacts": {
                    "linux-x64": "sha256:" + "6" * 64,
                    "macos-arm64": "sha256:" + "7" * 64,
                    "macos-x64": "sha256:" + "8" * 64,
                    "windows-x64": "sha256:" + "9" * 64,
                },
            },
            "migrationTail": {
                "name": "000110_support_access_operator_authority.sql",
                "sha256": "sha256:" + "a" * 64,
            },
        }
        self.policy = {
            "statementType": "synara-data-residency-statement-v1",
            "version": 7,
            "digest": "sha256:" + "c" * 64,
            "homeRegion": "cn-east-1",
            "allowedRegions": ["cn-east-1", "cn-east-2"],
            "enforcementState": "restricted",
        }
        self.subject = {
            "tenantId": "tenant-stage6-qa",
            "candidate": self.candidate,
            "policy": self.policy,
        }
        plane_regions = {
            "disasterRecoveryDestinations": ["cn-east-2"],
            "incidentBundles": ["cn-east-1"],
            "kmsKeyBackups": ["cn-east-2"],
            "kmsKeys": ["cn-east-1"],
            "logs": ["cn-east-1"],
            "metrics": ["cn-east-1"],
            "objectStorageBackups": ["cn-east-2"],
            "objectStorageDeletionProcessing": ["cn-east-1"],
            "objectStoragePrimary": ["cn-east-1"],
            "objectStorageReplicas": ["cn-east-2"],
            "postgresqlBackups": ["cn-east-2"],
            "postgresqlPrimary": ["cn-east-1"],
            "postgresqlReplicas": ["cn-east-2"],
            "postgresqlRestoreProcessing": ["cn-east-2"],
            "providerRequestProcessing": ["cn-east-1"],
            "providerRetention": ["cn-east-1"],
            "queue": ["cn-east-1"],
            "subprocessorProcessing": ["cn-east-1"],
            "supportProcessing": ["cn-east-1"],
            "traces": ["cn-east-1"],
        }
        self.inventory = {
            name: {"status": "known", "regions": regions, "disclosed": True}
            for name, regions in plane_regions.items()
        }
        self.documents = self.build_documents()
        self.references: dict[str, dict[str, str]] = {}
        self.manifest = self.root / "manifest.json"
        self.expected_candidate_binding = self.candidate_binding(self.candidate)
        self.write_evidence_set()

    def tearDown(self) -> None:
        self.temporary.cleanup()

    @staticmethod
    def boundary(external_verification: str) -> dict[str, str]:
        return {
            "contentValidation": "strict-schema-and-subject-validated",
            "externalVerification": external_verification,
        }

    def build_documents(self) -> dict[str, dict[str, object]]:
        browser_statement = {
            "statementType": self.policy["statementType"],
            "tenantId": self.subject["tenantId"],
            "homeRegion": self.policy["homeRegion"],
            "policyVersion": self.policy["version"],
            "policyDigest": self.policy["digest"],
            "enforcementState": self.policy["enforcementState"],
            "allowedRegions": self.policy["allowedRegions"],
        }
        documents: dict[str, dict[str, object]] = {
            "annex": {
                "schemaVersion": "synara.data-residency-annex-evidence.v1",
                "subject": self.subject,
                "annex": {
                    "annexId": "annex-stage6-rc1",
                    "promiseScope": "full-data-residency",
                    "effectiveAt": "2026-07-31T12:00:00Z",
                    "reviewExpiresAt": "2027-01-31T12:00:00Z",
                    "disasterRecoveryOptInRequired": True,
                    "noAllowedDestinationBehavior": "fail-closed-no-placement",
                },
                "processingPlanes": self.inventory,
                "verificationBoundary": self.boundary(
                    "signed-annex-required-not-verified"
                ),
            },
            "runtimeInventory": {
                "schemaVersion": "synara.data-residency-runtime-inventory-evidence.v1",
                "subject": self.subject,
                "inventory": {
                    "inventoryId": "inventory-stage6-rc1",
                    "sourceAuthority": "deployment-inventory/controller-v1",
                    "capturedAt": "2026-07-28T12:00:00Z",
                    "validUntil": "2026-08-10T12:00:00Z",
                },
                "processingPlanes": copy.deepcopy(self.inventory),
                "verificationBoundary": self.boundary(
                    "deployment-inventory-authority-required-not-verified"
                ),
            },
            "browserStatement": {
                "schemaVersion": "synara.data-residency-browser-statement-evidence.v1",
                "subject": self.subject,
                "surface": "tenant-settings/data-residency",
                "generatedAt": "2026-07-30T11:00:00Z",
                "statement": browser_statement,
                "verificationBoundary": self.boundary(
                    "deployed-browser-origin-required-not-verified"
                ),
            },
        }
        for name, kind, executed_at in (
            ("failoverExercise", "failover", "2026-07-29T10:00:00Z"),
            ("evacuationExercise", "evacuation", "2026-07-30T10:00:00Z"),
        ):
            documents[name] = {
                "schemaVersion": "synara.data-residency-exercise-evidence.v1",
                "subject": self.subject,
                "exercise": {
                    "exerciseId": f"{kind}-stage6-rc1",
                    "exerciseKind": kind,
                    "executedAt": executed_at,
                    "sourceRegion": "cn-east-1",
                    "selectedDestinationRegion": "cn-east-2",
                    "result": "passed",
                    "outOfPolicyProbe": {
                        "destinationRegion": "us-east-1",
                        "selectionRejected": True,
                    },
                    "annexSha256": "sha256:" + "0" * 64,
                    "runtimeInventorySha256": "sha256:" + "0" * 64,
                },
                "verificationBoundary": self.boundary(
                    "live-exercise-authority-required-not-verified"
                ),
            }
        for name, role, approver in (
            ("operationsApproval", "operations", "operator-1"),
            ("privacyLegalApproval", "privacyLegal", "privacy-counsel-1"),
            ("securityApproval", "security", "security-approver-1"),
        ):
            documents[name] = {
                "schemaVersion": "synara.data-residency-approval-evidence.v1",
                "subject": self.subject,
                "approval": {
                    "role": role,
                    "approverId": approver,
                    "decision": "approved-for-human-review",
                    "approvedAt": "2026-07-31T10:00:00Z",
                    "expiresAt": "2027-02-28T12:00:00Z",
                    "evidenceSubjects": {
                        evidence_name: "sha256:" + "0" * 64
                        for evidence_name in sorted(BASE_EVIDENCE_NAMES)
                    },
                },
                "verificationBoundary": self.boundary(
                    "approver-identity-and-signature-required-not-verified"
                ),
            }
        return documents

    @staticmethod
    def candidate_binding(candidate: dict[str, object]) -> str:
        encoded = json.dumps(candidate, separators=(",", ":"), sort_keys=True).encode("utf-8")
        return "sha256:" + hashlib.sha256(encoded).hexdigest()

    def document_path(self, name: str) -> pathlib.Path:
        return self.evidence_root / f"{name}.json"

    def write_document(self, name: str) -> dict[str, str]:
        path = self.document_path(name)
        path.write_text(json.dumps(self.documents[name], indent=2) + "\n", encoding="utf-8")
        return {
            "path": path.name,
            "sha256": "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest(),
        }

    def write_manifest(self) -> None:
        self.manifest.write_text(
            json.dumps(
                {
                    "schemaVersion": "synara.data-residency-deployment-evidence.v1",
                    "evidence": self.references,
                },
                indent=2,
            )
            + "\n",
            encoding="utf-8",
        )

    def write_evidence_set(self) -> None:
        for name in ("annex", "runtimeInventory", "browserStatement"):
            self.references[name] = self.write_document(name)
        for name in ("failoverExercise", "evacuationExercise"):
            exercise = self.documents[name]["exercise"]
            exercise["annexSha256"] = self.references["annex"]["sha256"]
            exercise["runtimeInventorySha256"] = self.references["runtimeInventory"]["sha256"]
            self.references[name] = self.write_document(name)
        evidence_subjects = {
            name: self.references[name]["sha256"] for name in sorted(BASE_EVIDENCE_NAMES)
        }
        for name in ("operationsApproval", "privacyLegalApproval", "securityApproval"):
            self.documents[name]["approval"]["evidenceSubjects"] = copy.deepcopy(
                evidence_subjects
            )
            self.references[name] = self.write_document(name)
        self.write_manifest()

    def rewrite_one_and_manifest(self, name: str) -> None:
        self.references[name] = self.write_document(name)
        self.write_manifest()

    def command(self) -> list[str]:
        return [
            sys.executable,
            str(SCRIPT),
            "--manifest",
            str(self.manifest),
            "--evidence-root",
            str(self.evidence_root),
            "--output",
            str(self.root / "receipt.json"),
            "--expected-candidate-binding-sha256",
            self.expected_candidate_binding,
            "--validated-at",
            "2026-08-01T12:00:00Z",
        ]

    def run_validator(self) -> subprocess.CompletedProcess[str]:
        return subprocess.run(self.command(), check=False, capture_output=True, text=True)

    def receipt(self) -> dict[str, object]:
        return json.loads((self.root / "receipt.json").read_text(encoding="utf-8"))

    def test_strict_semantic_evidence_set_is_review_eligible_without_signature_claim(self) -> None:
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertTrue(receipt["eligibleForHumanGateReview"])
        self.assertEqual(
            receipt["assessment"], "evidence-validated-not-residency-approved"
        )
        self.assertTrue(
            receipt["verificationBoundary"]["strictAttachmentContentAndSubjectValidated"]
        )
        self.assertFalse(receipt["verificationBoundary"]["cryptographicSignaturesVerified"])
        encoded = "".join(
            f"{name}={self.references[name]['sha256']}\n" for name in sorted(EVIDENCE_NAMES)
        ).encode("utf-8")
        self.assertEqual(
            receipt["evidenceSetSha256"], "sha256:" + hashlib.sha256(encoded).hexdigest()
        )
        output = self.root / "receipt.json"
        self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)
        self.assertEqual(
            stat.S_IMODE(output.with_suffix(".json.sha256").stat().st_mode), 0o600
        )

    def test_rejects_arbitrary_hash_valid_annex_content(self) -> None:
        self.documents["annex"] = {"evidence": "annex"}
        self.write_evidence_set()
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("annex document fields do not match", result.stderr)

    def test_rejects_runtime_inventory_from_another_candidate(self) -> None:
        self.documents["runtimeInventory"]["subject"] = copy.deepcopy(self.subject)
        self.documents["runtimeInventory"]["subject"]["candidate"][
            "environmentId"
        ] = "production-like/other-candidate"
        self.write_evidence_set()
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("does not match the Annex subject", result.stderr)

    def test_rejects_browser_statement_from_another_policy_head(self) -> None:
        self.documents["browserStatement"]["subject"] = copy.deepcopy(self.subject)
        self.documents["browserStatement"]["subject"]["policy"]["digest"] = (
            "sha256:" + "d" * 64
        )
        self.write_evidence_set()
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("does not match the Annex subject", result.stderr)

    def test_rejects_runtime_inventory_that_disagrees_with_annex(self) -> None:
        self.documents["runtimeInventory"]["processingPlanes"]["supportProcessing"][
            "regions"
        ] = ["cn-east-2"]
        self.write_evidence_set()
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("do not match the signed Annex", result.stderr)

    def test_rejects_browser_statement_content_that_disagrees_with_subject(self) -> None:
        self.documents["browserStatement"]["statement"]["policyDigest"] = (
            "sha256:" + "e" * 64
        )
        self.write_evidence_set()
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("browser statement content does not match", result.stderr)

    def test_rejects_exercise_bound_to_different_annex(self) -> None:
        self.documents["failoverExercise"]["exercise"]["annexSha256"] = (
            "sha256:" + "f" * 64
        )
        self.rewrite_one_and_manifest("failoverExercise")
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("does not bind the exact Annex", result.stderr)

    def test_rejects_approval_bound_to_different_evidence_set(self) -> None:
        self.documents["securityApproval"]["approval"]["evidenceSubjects"]["annex"] = (
            "sha256:" + "f" * 64
        )
        self.rewrite_one_and_manifest("securityApproval")
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("does not bind the exact evidence set", result.stderr)

    def test_rejects_approval_role_reuse(self) -> None:
        self.documents["securityApproval"]["approval"]["role"] = "operations"
        self.write_evidence_set()
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("role must be security", result.stderr)

    def test_rejects_approver_identity_reuse(self) -> None:
        self.documents["securityApproval"]["approval"]["approverId"] = "operator-1"
        self.write_evidence_set()
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("three distinct approvers", result.stderr)

    def test_rejects_attachment_that_claims_signature_was_verified(self) -> None:
        self.documents["annex"]["verificationBoundary"][
            "externalVerification"
        ] = "cryptographic-signature-verified"
        self.write_evidence_set()
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("signed-annex-required-not-verified", result.stderr)

    def test_rejects_exercise_outside_runtime_inventory_window(self) -> None:
        self.documents["failoverExercise"]["exercise"]["executedAt"] = (
            "2026-07-27T10:00:00Z"
        )
        self.write_evidence_set()
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("runtime inventory validity window", result.stderr)

    def test_rejects_approval_that_predates_its_evidence(self) -> None:
        self.documents["operationsApproval"]["approval"]["approvedAt"] = (
            "2026-07-29T11:00:00Z"
        )
        self.write_evidence_set()
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("timestamps are out of order", result.stderr)

    def test_rejects_secret_attachment_without_echoing_secret(self) -> None:
        secret = "Authorization: Bearer residency-validator-secret"
        path = self.document_path("browserStatement")
        path.write_text(
            path.read_text(encoding="utf-8") + secret + "\n",
            encoding="utf-8",
        )
        self.references["browserStatement"] = {
            "path": path.name,
            "sha256": "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest(),
        }
        self.write_manifest()
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("prohibited bearer credential material", result.stderr)
        self.assertNotIn(secret, result.stderr)

    def test_rejects_duplicate_manifest_field(self) -> None:
        encoded = self.manifest.read_text(encoding="utf-8")
        duplicate = encoded.replace(
            '"schemaVersion": "synara.data-residency-deployment-evidence.v1",',
            '"schemaVersion": "synara.data-residency-deployment-evidence.v1",\n'
            '  "schemaVersion": "synara.data-residency-deployment-evidence.v1",',
            1,
        )
        self.manifest.write_text(duplicate, encoding="utf-8")
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("duplicate field", result.stderr)

    def test_execution_only_annex_is_never_review_eligible(self) -> None:
        self.documents["annex"]["annex"]["promiseScope"] = "execution-only"
        self.write_evidence_set()
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.receipt()["eligibleForHumanGateReview"])

    def test_unknown_plane_is_disclosed_but_not_review_eligible(self) -> None:
        unknown = {"status": "unknown", "regions": [], "disclosed": True}
        self.documents["annex"]["processingPlanes"]["logs"] = copy.deepcopy(unknown)
        self.documents["runtimeInventory"]["processingPlanes"]["logs"] = copy.deepcopy(
            unknown
        )
        self.write_evidence_set()
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.receipt()["eligibleForHumanGateReview"])

    def test_staging_candidate_is_not_review_eligible(self) -> None:
        self.candidate["environmentClass"] = "staging"
        self.expected_candidate_binding = self.candidate_binding(self.candidate)
        self.write_evidence_set()
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.receipt()["eligibleForHumanGateReview"])

    def test_expiring_approval_is_not_current_through_annex_review(self) -> None:
        self.documents["securityApproval"]["approval"]["expiresAt"] = (
            "2026-08-15T12:00:00Z"
        )
        self.write_evidence_set()
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertFalse(receipt["eligibleForHumanGateReview"])
        self.assertFalse(receipt["approvals"]["security"]["currentThroughAnnexReview"])

    def test_rejects_candidate_drift_against_external_binding(self) -> None:
        self.candidate["environmentId"] = "production-like/drifted"
        self.write_evidence_set()
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("candidate drift detected", result.stderr)

    def test_rejects_hash_tampering(self) -> None:
        self.document_path("annex").write_text("tampered\n", encoding="utf-8")
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("sha256 does not match", result.stderr)

    def test_rejects_duplicate_evidence_path(self) -> None:
        self.references["browserStatement"] = self.references["annex"]
        self.write_manifest()
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("duplicates another evidence file", result.stderr)

    def test_rejects_traversal_path(self) -> None:
        outside = self.root / "outside.json"
        outside.write_text("{}\n", encoding="utf-8")
        self.references["annex"] = {
            "path": "../outside.json",
            "sha256": "sha256:" + hashlib.sha256(outside.read_bytes()).hexdigest(),
        }
        self.write_manifest()
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("traversal-free", result.stderr)

    def test_rejects_symlink_evidence(self) -> None:
        path = self.document_path("annex")
        target = self.root / "outside.json"
        target.write_bytes(path.read_bytes())
        path.unlink()
        path.symlink_to(target)
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("must not traverse a symlink", result.stderr)

    def test_rejects_oversized_evidence(self) -> None:
        path = self.document_path("annex")
        with path.open("wb") as output:
            output.truncate(16 * 1024 * 1024 + 1)
        self.references["annex"]["sha256"] = (
            "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest()
        )
        self.write_manifest()
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("evidence file limit", result.stderr)


if __name__ == "__main__":
    unittest.main()
