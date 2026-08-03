from __future__ import annotations

import hashlib
import importlib.util
import json
import pathlib
import stat
import subprocess
import sys
import tempfile
import unittest
import uuid


REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
MATRIX = REPO_ROOT / "docs/release-matrices/stage-6-operations-ui-v1.json"
SCRIPT = pathlib.Path(__file__).with_name("validate_operations_exercise_evidence.py")
SPEC = importlib.util.spec_from_file_location("validate_operations_exercise_evidence", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)
DIGEST = "sha256:" + "a" * 64


class ValidateOperationsExerciseEvidenceTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temporary.name)
        self.evidence = self.root / "evidence"
        self.evidence.mkdir()
        self.manifest = self.root / "manifest.json"
        self.output = self.root / "receipt.json"
        expected, matrix_receipt = MODULE.load_expected_operations(REPO_ROOT, MATRIX)
        self.expected = expected
        self.references: dict[str, dict[str, str]] = {}
        accounts = [
            {
                "role": role,
                "subjectReference": f"actor/{role}",
                "authenticationMethod": "sso",
                "sessionFreshAt": "2026-07-30T23:55:00Z",
            }
            for role in sorted(MODULE.ACCOUNT_ROLES)
        ]
        operations = []
        for index, operation_id in enumerate(sorted(expected)):
            operation = expected[operation_id]
            operations.append(
                {
                    "id": operation_id,
                    "surface": operation["surface"],
                    "actor": operation["actor"],
                    "access": operation["access"],
                    "status": "passed",
                    "startedAt": "2026-07-31T00:10:00Z",
                    "completedAt": "2026-07-31T00:11:00Z",
                    "requestIds": [
                        str(uuid.uuid5(uuid.NAMESPACE_URL, f"synara-operation-{index}"))
                    ],
                    "negativeActor": MODULE.EXPECTED_NEGATIVE_ACTORS[operation_id],
                    "negativeResult": "denied",
                    "usedCli": False,
                    "usedDatabaseClient": False,
                    "usedDeveloperTools": False,
                    "positiveEvidence": self.reference(f"{operation_id}-positive"),
                    "negativeEvidence": self.reference(f"{operation_id}-negative"),
                }
            )
        self.payload = {
            "schemaVersion": MODULE.SCHEMA_VERSION,
            "candidate": {
                "candidateId": "stage6-rc1",
                "sourceCommit": "b" * 40,
                "environment": "production-like",
                "environmentId": "production-like/stage6-rc1",
                "artifacts": {
                    "controlPlane": DIGEST,
                    "web": DIGEST,
                    "admin": DIGEST,
                },
                "webBaseUrl": "https://app.example.test",
                "adminBaseUrl": "https://admin.example.test",
            },
            "matrixSha256": matrix_receipt["sha256"],
            "startedAt": "2026-07-31T00:00:00Z",
            "completedAt": "2026-07-31T01:00:00Z",
            "accounts": accounts,
            "operations": operations,
            "supportAccess": {
                "requesterSubjectReference": "actor/platform-operator",
                "approverSubjectReference": "actor/platform-admin",
                "supportSubjectReference": "actor/support-engineer",
                "requesterAndApproverDistinct": True,
                "readOnlyWriteDenied": True,
                "revocationAuditAction": "support.access_revoked",
                "expiryAuditAction": "support.access_expired",
                "revocationAuditVisible": True,
                "expiryAuditVisible": True,
                "tenantAuditVisible": True,
                "evidence": self.reference("support-access-lifecycle"),
            },
            "approvals": [
                {
                    "role": "operations",
                    "subjectReference": "approver/operations",
                    "approved": True,
                    "approvedAt": "2026-07-31T01:30:00Z",
                    "evidence": self.reference("operations-approval"),
                },
                {
                    "role": "security",
                    "subjectReference": "approver/security",
                    "approved": True,
                    "approvedAt": "2026-07-31T01:35:00Z",
                    "evidence": self.reference("security-approval"),
                },
            ],
        }

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def reference(self, name: str) -> dict[str, str]:
        path = self.evidence / f"{name}.json"
        content = json.dumps({"evidence": name}, sort_keys=True) + "\n"
        path.write_text(content, encoding="utf-8")
        reference = {
            "path": path.relative_to(self.evidence).as_posix(),
            "sha256": "sha256:" + hashlib.sha256(content.encode("utf-8")).hexdigest(),
        }
        self.references[name] = reference
        return reference

    def write_manifest(self) -> None:
        self.manifest.write_text(json.dumps(self.payload, indent=2) + "\n", encoding="utf-8")

    def run_validator(self) -> subprocess.CompletedProcess[str]:
        self.write_manifest()
        return subprocess.run(
            [
                sys.executable,
                str(SCRIPT),
                "--manifest",
                str(self.manifest),
                "--evidence-root",
                str(self.evidence),
                "--repository-root",
                str(REPO_ROOT),
                "--matrix",
                str(MATRIX),
                "--output",
                str(self.output),
                "--validated-at",
                "2026-07-31T02:00:00Z",
            ],
            check=False,
            capture_output=True,
            text=True,
        )

    def receipt(self) -> dict[str, object]:
        return json.loads(self.output.read_text(encoding="utf-8"))

    def test_complete_production_like_exercise_is_review_eligible_but_not_approved(self) -> None:
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertTrue(receipt["eligibleForHumanGateReview"])
        self.assertEqual(receipt["operationCounts"]["passed"], len(self.expected))
        self.assertEqual(receipt["negativeAuthorizationCounts"]["denied"], len(self.expected))
        self.assertEqual(receipt["fallbackCounts"], {"cli": 0, "databaseClient": 0, "developerTools": 0})
        self.assertEqual(receipt["assessment"], "evidence-validated-not-operations-passed")
        self.assertTrue(self.output.with_suffix(".json.sha256").is_file())
        self.assertEqual(stat.S_IMODE(self.output.stat().st_mode), 0o600)
        self.assertEqual(
            stat.S_IMODE(self.output.with_suffix(".json.sha256").stat().st_mode), 0o600
        )

    def test_preserves_failed_or_tool_assisted_rows_as_ineligible(self) -> None:
        self.payload["operations"][0]["status"] = "failed"
        self.payload["operations"][0]["usedCli"] = True
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertFalse(receipt["eligibleForHumanGateReview"])
        self.assertEqual(receipt["operationCounts"]["failed"], 1)
        self.assertEqual(receipt["fallbackCounts"]["cli"], 1)

    def test_preserves_unexpected_authorization_or_incomplete_support_as_ineligible(self) -> None:
        self.payload["operations"][0]["negativeResult"] = "unexpectedly-allowed"
        self.payload["supportAccess"]["requesterAndApproverDistinct"] = False
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertFalse(receipt["eligibleForHumanGateReview"])
        self.assertFalse(receipt["supportLifecycleComplete"])
        self.assertEqual(receipt["negativeAuthorizationCounts"]["unexpectedly-allowed"], 1)

    def test_terminal_support_audit_actions_and_visibility_are_required(self) -> None:
        self.payload["supportAccess"]["revocationAuditAction"] = "support.policy_updated"
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("support.access_revoked", result.stderr)

        self.payload["supportAccess"]["revocationAuditAction"] = "support.access_revoked"
        self.payload["supportAccess"]["expiryAuditVisible"] = False
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertFalse(receipt["supportLifecycleComplete"])
        self.assertFalse(receipt["eligibleForHumanGateReview"])

    def test_staging_or_fixture_authentication_remains_ineligible(self) -> None:
        self.payload["candidate"]["environment"] = "staging"
        for account in self.payload["accounts"]:
            account["authenticationMethod"] = "fixture"
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertFalse(receipt["environmentEligible"])
        self.assertFalse(receipt["productionAuthenticationDeclared"])
        self.assertFalse(receipt["eligibleForHumanGateReview"])

    def test_rejects_missing_row_or_source_matrix_role_drift(self) -> None:
        removed = self.payload["operations"].pop()
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("every source matrix row", result.stderr)

        self.payload["operations"].append(removed)
        self.payload["operations"][0]["actor"] = "tenant-member"
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("actor does not match the source matrix", result.stderr)

    def test_rejects_reused_tampered_or_symlinked_evidence(self) -> None:
        original_reference = self.payload["operations"][1]["positiveEvidence"]
        self.payload["operations"][1]["positiveEvidence"] = self.payload["operations"][0][
            "positiveEvidence"
        ]
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("duplicates another evidence file", result.stderr)

        self.payload["operations"][1]["positiveEvidence"] = original_reference
        evidence_path = self.evidence / self.payload["operations"][0]["positiveEvidence"]["path"]
        original_content = evidence_path.read_text(encoding="utf-8")
        evidence_path.write_text("tampered\n", encoding="utf-8")
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("sha256 does not match", result.stderr)

        evidence_path.write_text(original_content, encoding="utf-8")
        target = self.evidence / self.payload["operations"][0]["positiveEvidence"]["path"]
        link = self.evidence / "linked-evidence.json"
        link.symlink_to(target.name)
        self.payload["operations"][0]["positiveEvidence"] = {
            "path": link.name,
            "sha256": self.payload["operations"][0]["positiveEvidence"]["sha256"],
        }
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("must not traverse a symlink", result.stderr)

    def test_rejects_duplicate_request_id_and_wrong_denial_actor(self) -> None:
        original_request_ids = self.payload["operations"][1]["requestIds"]
        self.payload["operations"][1]["requestIds"] = self.payload["operations"][0]["requestIds"]
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("globally unique", result.stderr)

        self.payload["operations"][1]["requestIds"] = original_request_ids
        self.payload["operations"][0]["negativeActor"] = "tenant-member"
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("reviewed denial matrix", result.stderr)

    def test_rejects_secret_material_in_direct_validator_input(self) -> None:
        reference = self.payload["operations"][0]["positiveEvidence"]
        evidence_path = self.evidence / reference["path"]
        secret = "Authorization: Bearer operations-direct-secret"
        evidence_path.write_text(secret + "\n", encoding="utf-8")
        reference["sha256"] = "sha256:" + hashlib.sha256(
            (secret + "\n").encode("utf-8")
        ).hexdigest()
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("prohibited bearer credential material", result.stderr)
        self.assertNotIn(secret, result.stderr)

    def test_rejects_duplicate_manifest_fields(self) -> None:
        self.write_manifest()
        encoded = self.manifest.read_text(encoding="utf-8")
        marker = f'"schemaVersion": "{MODULE.SCHEMA_VERSION}",'
        self.manifest.write_text(
            encoded.replace(marker, f"{marker}\n  {marker}", 1),
            encoding="utf-8",
        )
        result = subprocess.run(
            [
                sys.executable,
                str(SCRIPT),
                "--manifest",
                str(self.manifest),
                "--evidence-root",
                str(self.evidence),
                "--repository-root",
                str(REPO_ROOT),
                "--matrix",
                str(MATRIX),
                "--output",
                str(self.output),
            ],
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, 2)
        self.assertIn("duplicate JSON field", result.stderr)


if __name__ == "__main__":
    unittest.main()
