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


REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
SCRIPT = pathlib.Path(__file__).with_name("validate_production_rotation_evidence.py")
SPEC = importlib.util.spec_from_file_location("validate_production_rotation_evidence", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class ValidateProductionRotationEvidenceTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temporary.name)
        self.evidence = self.root / "evidence"
        self.evidence.mkdir()
        self.manifest = self.root / "manifest.json"
        self.output = self.root / "receipt.json"
        self.payload = {
            "schemaVersion": MODULE.SCHEMA_VERSION,
            "candidate": {
                "candidateId": "stage6-rotation-rc1",
                "sourceCommit": "a" * 40,
                "controlPlaneDigest": "sha256:" + "b" * 64,
                "webDigest": "sha256:" + "c" * 64,
                "adminDigest": "sha256:" + "d" * 64,
                "environment": "production-like",
                "environmentId": "production-like/stage6-rotation-rc1",
                "regions": ["cn-east-1", "cn-north-1"],
                "migrationTail": MODULE.latest_migration(REPO_ROOT),
            },
            "startedAt": "2026-08-01T00:00:00Z",
            "completedAt": "2026-08-01T06:00:00Z",
            "controls": [self.control(control_id, index) for index, control_id in enumerate(sorted(MODULE.REQUIRED_CONTROLS))],
            "secretScan": {
                "scannerVersion": "trivy-0.66.0",
                "findingCount": 0,
                "evidence": self.reference("secret-scan"),
            },
            "approvals": [
                {
                    "role": role,
                    "subjectReference": f"approver/{role}",
                    "decision": "approved-for-human-gate-review",
                    "decidedAt": "2026-08-01T07:00:00Z",
                    "evidence": self.reference(f"approval-{role}"),
                }
                for role in sorted(MODULE.APPROVAL_ROLES)
            ],
        }

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def reference(self, name: str, content: str | None = None) -> dict[str, str]:
        path = self.evidence / f"{name}.json"
        encoded = ((content or json.dumps({"evidence": name}, sort_keys=True)) + "\n").encode()
        path.write_bytes(encoded)
        return {
            "path": path.relative_to(self.evidence).as_posix(),
            "sha256": "sha256:" + hashlib.sha256(encoded).hexdigest(),
        }

    def control(self, control_id: str, index: int) -> dict[str, object]:
        return {
            "id": control_id,
            "status": "passed",
            "changeReference": f"CHG-{1000 + index}",
            "ownerSubject": f"owner/{control_id}",
            "approverSubject": f"reviewer/{control_id}",
            "previousAuthorityId": f"authority/{control_id}/v1",
            "newAuthorityId": f"authority/{control_id}/v2",
            "startedAt": "2026-08-01T01:00:00Z",
            "completedAt": "2026-08-01T02:00:00Z",
            "allConsumersUpdated": True,
            "newPathPassed": True,
            "oldPathFinalStateVerified": True,
            "rollbackVerified": True,
            "unresolvedRiskCount": 0,
            "evidence": {
                field: self.reference(f"{control_id}-{field}")
                for field in sorted(MODULE.EVIDENCE_FIELDS)
            },
        }

    def run_validator(self) -> subprocess.CompletedProcess[str]:
        self.manifest.write_text(json.dumps(self.payload, indent=2) + "\n", encoding="utf-8")
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
                "--validated-at",
                "2026-08-01T08:00:00Z",
                "--output",
                str(self.output),
            ],
            check=False,
            capture_output=True,
            text=True,
        )

    def receipt(self) -> dict[str, object]:
        return json.loads(self.output.read_text(encoding="utf-8"))

    def test_complete_evidence_is_review_eligible_but_never_self_approves(self) -> None:
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertTrue(receipt["eligibleForHumanGateReview"])
        self.assertEqual(receipt["assessment"], MODULE.ASSESSMENT)
        self.assertEqual(receipt["controlCounts"], {"passed": len(MODULE.REQUIRED_CONTROLS)})
        self.assertEqual(receipt["evidenceFileCount"], len(MODULE.REQUIRED_CONTROLS) * 5 + 4)
        self.assertIn("internal-incident-publisher-hmac-key", receipt["requiredControlIds"])
        self.assertNotIn("billing-provider-credential", receipt["requiredControlIds"])
        self.assertFalse(receipt["externalEvidenceAuthorityVerified"])
        self.assertFalse(receipt["approverAuthorityVerified"])
        self.assertFalse(receipt["cryptographicSignaturesVerified"])
        self.assertNotIn(str(self.root), json.dumps(receipt))
        sidecar = self.output.with_suffix(".json.sha256")
        self.assertEqual(stat.S_IMODE(self.output.stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(sidecar.stat().st_mode), 0o600)
        original = self.output.read_bytes()
        repeated = self.run_validator()
        self.assertEqual(repeated.returncode, 2)
        self.assertIn("must not already exist", repeated.stderr)
        self.assertEqual(self.output.read_bytes(), original)

    def test_failed_control_or_scan_finding_remains_ineligible(self) -> None:
        self.payload["controls"][0]["status"] = "failed"
        self.payload["controls"][1]["oldPathFinalStateVerified"] = False
        self.payload["secretScan"]["findingCount"] = 1
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.receipt()["eligibleForHumanGateReview"])

    def test_rejects_missing_or_duplicate_control(self) -> None:
        removed = self.payload["controls"].pop()
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("every required rotation control exactly once", result.stderr)

        self.payload["controls"].append(removed)
        self.payload["controls"][1]["id"] = self.payload["controls"][0]["id"]
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("every required rotation control exactly once", result.stderr)

    def test_rejects_historical_billing_control_in_active_v2_manifest(self) -> None:
        incident = next(
            control
            for control in self.payload["controls"]
            if control["id"] == "internal-incident-publisher-hmac-key"
        )
        incident["id"] = "billing-provider-credential"
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("every required rotation control exactly once", result.stderr)

    def test_rejects_owner_approver_alias_and_bad_timing(self) -> None:
        control = self.payload["controls"][0]
        control["approverSubject"] = control["ownerSubject"]
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("owner and approver must be distinct", result.stderr)

        control["approverSubject"] = "reviewer/replacement"
        control["completedAt"] = "2026-08-01T07:00:00Z"
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("inside the exercise window", result.stderr)

    def test_rejects_reused_tampered_or_symlinked_evidence(self) -> None:
        controls = self.payload["controls"]
        controls[1]["evidence"]["changeRecord"] = controls[0]["evidence"]["changeRecord"]
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("evidence file is reused", result.stderr)

        controls[1]["evidence"]["changeRecord"] = self.reference("replacement-change-record")
        reference = controls[0]["evidence"]["changeRecord"]
        (self.evidence / reference["path"]).write_text("tampered\n", encoding="utf-8")
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("sha256 does not match", result.stderr)

        replacement = self.reference("restored-change-record")
        controls[0]["evidence"]["changeRecord"] = replacement
        path = self.evidence / replacement["path"]
        target = self.evidence / "real-change-record.json"
        path.rename(target)
        path.symlink_to(target.name)
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("must not traverse a symlink", result.stderr)

    def test_rejects_secret_material_without_echoing_it(self) -> None:
        secret = "sk_" + "live_" + "1234567890abcdefghijklmnop"
        control = self.payload["controls"][0]
        control["evidence"]["changeRecord"] = self.reference("unsafe-change-record", secret)
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("prohibited Stripe live key material", result.stderr)
        self.assertNotIn(secret, result.stderr)
        self.assertFalse(self.output.exists())

    def test_rejects_migration_drift_and_duplicate_approval_subject(self) -> None:
        self.payload["candidate"]["migrationTail"]["sha256"] = "sha256:" + "0" * 64
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("does not match the repository", result.stderr)

        self.payload["candidate"]["migrationTail"] = MODULE.latest_migration(REPO_ROOT)
        self.payload["approvals"][1]["subjectReference"] = self.payload["approvals"][0]["subjectReference"]
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("approval subjects must be distinct", result.stderr)

    def test_rejects_symlinked_or_oversized_manifest(self) -> None:
        self.manifest.write_text(json.dumps(self.payload), encoding="utf-8")
        target = self.root / "manifest-target.json"
        self.manifest.rename(target)
        self.manifest.symlink_to(target)
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("non-symlink", result.stderr)

        self.manifest.unlink()
        self.manifest.write_bytes(b" " * (MODULE.MAX_MANIFEST_BYTES + 1))
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
                "--output",
                str(self.output),
            ],
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, 2)
        self.assertIn("bounded regular", result.stderr)

    def test_rejects_duplicate_json_fields(self) -> None:
        self.manifest.write_text(
            '{"schemaVersion":"first","schemaVersion":"second"}\n', encoding="utf-8"
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
