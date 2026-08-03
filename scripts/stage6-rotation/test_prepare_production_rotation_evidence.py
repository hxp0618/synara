from __future__ import annotations

import importlib.util
import json
import pathlib
import stat
import subprocess
import sys
import tempfile
import unittest
from argparse import Namespace
from unittest import mock


REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
DIRECTORY = pathlib.Path(__file__).resolve().parent
if str(DIRECTORY) not in sys.path:
    sys.path.insert(0, str(DIRECTORY))

PREPARE_SCRIPT = DIRECTORY / "prepare_production_rotation_evidence.py"
PREPARE_SPEC = importlib.util.spec_from_file_location(
    "prepare_production_rotation_evidence", PREPARE_SCRIPT
)
assert PREPARE_SPEC is not None and PREPARE_SPEC.loader is not None
PREPARE = importlib.util.module_from_spec(PREPARE_SPEC)
PREPARE_SPEC.loader.exec_module(PREPARE)

VALIDATOR_SCRIPT = DIRECTORY / "validate_production_rotation_evidence.py"
VALIDATOR_SPEC = importlib.util.spec_from_file_location(
    "rotation_validator_for_prepare_test", VALIDATOR_SCRIPT
)
assert VALIDATOR_SPEC is not None and VALIDATOR_SPEC.loader is not None
VALIDATOR = importlib.util.module_from_spec(VALIDATOR_SPEC)
VALIDATOR_SPEC.loader.exec_module(VALIDATOR)


class PrepareProductionRotationEvidenceTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temporary.name)
        self.evidence = self.root / "evidence"
        self.evidence.mkdir()
        self.draft = self.root / "rotation-draft.json"
        self.manifest = self.root / "rotation-manifest.json"
        self.receipt = self.root / "rotation-receipt.json"
        self.payload = {
            "schemaVersion": PREPARE.DRAFT_SCHEMA_VERSION,
            "candidate": {
                "candidateId": "stage6-rotation-rc1",
                "sourceCommit": "a" * 40,
                "controlPlaneDigest": "sha256:" + "b" * 64,
                "webDigest": "sha256:" + "c" * 64,
                "adminDigest": "sha256:" + "d" * 64,
                "environment": "production-like",
                "environmentId": "production-like/stage6-rotation-rc1",
                "regions": ["cn-east-1", "cn-north-1"],
                "migrationTail": VALIDATOR.latest_migration(REPO_ROOT),
            },
            "startedAt": "2026-08-01T00:00:00Z",
            "completedAt": "2026-08-01T06:00:00Z",
            "controls": [
                self.control(control_id, index)
                for index, control_id in enumerate(sorted(VALIDATOR.REQUIRED_CONTROLS))
            ],
            "secretScan": {
                "scannerVersion": "trivy-0.66.0",
                "findingCount": 0,
                "evidence": self.evidence_path("secret-scan"),
            },
            "approvals": [
                {
                    "role": role,
                    "subjectReference": f"approver/{role}",
                    "decision": "approved-for-human-gate-review",
                    "decidedAt": "2026-08-01T07:00:00Z",
                    "evidence": self.evidence_path(f"approval-{role}"),
                }
                for role in sorted(VALIDATOR.APPROVAL_ROLES)
            ],
        }
        self.write_draft()

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def evidence_path(self, name: str, content: str | None = None) -> str:
        path = self.evidence / f"{name}.json"
        path.write_text((content or json.dumps({"evidence": name}, sort_keys=True)) + "\n")
        return path.relative_to(self.root).as_posix()

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
                field: self.evidence_path(f"{control_id}-{field}")
                for field in sorted(VALIDATOR.EVIDENCE_FIELDS)
            },
        }

    def write_draft(self) -> None:
        self.draft.write_text(json.dumps(self.payload, indent=2) + "\n", encoding="utf-8")

    def command(self) -> list[str]:
        return [
            sys.executable,
            str(PREPARE_SCRIPT),
            "--evidence-root",
            str(self.root),
            "--draft",
            self.draft.relative_to(self.root).as_posix(),
            "--repository-root",
            str(REPO_ROOT),
            "--manifest-output",
            self.manifest.relative_to(self.root).as_posix(),
            "--receipt-output",
            self.receipt.relative_to(self.root).as_posix(),
            "--validated-at",
            "2026-08-01T08:00:00Z",
        ]

    def args(self) -> Namespace:
        return Namespace(
            evidence_root=str(self.root),
            draft=self.draft.relative_to(self.root).as_posix(),
            repository_root=str(REPO_ROOT),
            manifest_output=self.manifest.relative_to(self.root).as_posix(),
            receipt_output=self.receipt.relative_to(self.root).as_posix(),
            validated_at="2026-08-01T08:00:00Z",
        )

    def test_materializes_hashes_and_publishes_validated_outputs(self) -> None:
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("eligibleForHumanGateReview=true", result.stdout)
        manifest = json.loads(self.manifest.read_text())
        receipt = json.loads(self.receipt.read_text())
        first_reference = manifest["controls"][0]["evidence"]["changeRecord"]
        self.assertEqual(set(first_reference), {"path", "sha256"})
        self.assertTrue(first_reference["sha256"].startswith("sha256:"))
        self.assertEqual(receipt["manifest"]["path"], self.manifest.name)
        for output in (
            self.manifest,
            self.manifest.with_suffix(".json.sha256"),
            self.receipt,
            self.receipt.with_suffix(".json.sha256"),
        ):
            self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)

        original = self.manifest.read_bytes()
        repeated = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(repeated.returncode, 2)
        self.assertIn("must be a new path", repeated.stderr)
        self.assertEqual(self.manifest.read_bytes(), original)

    def test_invalid_draft_publishes_nothing(self) -> None:
        self.payload["controls"].pop()
        self.write_draft()
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("every required rotation control", result.stderr)
        self.assertFalse(self.manifest.exists())
        self.assertFalse(self.receipt.exists())

    def test_secret_material_publishes_nothing_without_echoing_value(self) -> None:
        secret = "whsec_1234567890abcdefghijklmnop"
        self.payload["controls"][0]["evidence"]["changeRecord"] = self.evidence_path(
            "unsafe-change-record", secret
        )
        self.write_draft()
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("prohibited Stripe webhook secret material", result.stderr)
        self.assertNotIn(secret, result.stderr)
        self.assertFalse(self.manifest.exists())
        self.assertFalse(self.receipt.exists())

    def test_rejects_symlinked_draft_and_traversal_output(self) -> None:
        target = self.root / "real-draft.json"
        self.draft.rename(target)
        self.draft.symlink_to(target.name)
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("must not traverse a symlink", result.stderr)

        self.draft.unlink()
        target.rename(self.draft)
        command = self.command()
        command[command.index("--manifest-output") + 1] = "../outside.json"
        result = subprocess.run(command, check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("traversal-free and relative", result.stderr)

    def test_receipt_publication_failure_rolls_back_manifest_pair(self) -> None:
        real_publish = PREPARE.publish_immutable_with_sha256
        calls = 0

        def fail_receipt(path: pathlib.Path, encoded: bytes) -> None:
            nonlocal calls
            calls += 1
            if calls == 2:
                raise PREPARE.ImmutableEvidenceIOError("simulated receipt publication failure")
            real_publish(path, encoded)

        with mock.patch.object(PREPARE, "publish_immutable_with_sha256", side_effect=fail_receipt):
            with self.assertRaises(PREPARE.RotationPreparationError):
                PREPARE.prepare(self.args())
        self.assertFalse(self.manifest.exists())
        self.assertFalse(self.manifest.with_suffix(".json.sha256").exists())
        self.assertFalse(self.receipt.exists())
        self.assertFalse(self.receipt.with_suffix(".json.sha256").exists())


if __name__ == "__main__":
    unittest.main()
