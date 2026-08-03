from __future__ import annotations

import importlib.util
import json
import pathlib
import stat
import subprocess
import sys
import tempfile
import unittest
import uuid
from argparse import Namespace
from unittest import mock


REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
DIRECTORY = pathlib.Path(__file__).resolve().parent
if str(DIRECTORY) not in sys.path:
    sys.path.insert(0, str(DIRECTORY))

PREPARE_SCRIPT = DIRECTORY / "prepare_operations_exercise_evidence.py"
PREPARE_SPEC = importlib.util.spec_from_file_location(
    "prepare_operations_exercise_evidence",
    PREPARE_SCRIPT,
)
assert PREPARE_SPEC is not None and PREPARE_SPEC.loader is not None
PREPARE = importlib.util.module_from_spec(PREPARE_SPEC)
PREPARE_SPEC.loader.exec_module(PREPARE)

VALIDATOR_SCRIPT = DIRECTORY / "validate_operations_exercise_evidence.py"
VALIDATOR_SPEC = importlib.util.spec_from_file_location(
    "operations_validator_for_prepare_test",
    VALIDATOR_SCRIPT,
)
assert VALIDATOR_SPEC is not None and VALIDATOR_SPEC.loader is not None
VALIDATOR = importlib.util.module_from_spec(VALIDATOR_SPEC)
VALIDATOR_SPEC.loader.exec_module(VALIDATOR)

MATRIX = REPO_ROOT / "docs/release-matrices/stage-6-operations-ui-v1.json"
DIGEST = "sha256:" + "a" * 64


class PrepareOperationsExerciseEvidenceTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temporary.name)
        self.evidence = self.root / "evidence"
        self.evidence.mkdir()
        self.draft = self.root / "operations-draft.json"
        self.manifest = self.root / "operations-manifest.json"
        self.receipt = self.root / "operations-receipt.json"
        expected, _ = VALIDATOR.load_expected_operations(REPO_ROOT, MATRIX)
        self.expected = expected
        self.payload = {
            "schemaVersion": PREPARE.DRAFT_SCHEMA_VERSION,
            "candidate": {
                "candidateId": "stage6-operations-rc1",
                "sourceCommit": "b" * 40,
                "environment": "production-like",
                "environmentId": "production-like/stage6-operations-rc1",
                "artifacts": {
                    "controlPlane": DIGEST,
                    "web": DIGEST,
                    "admin": DIGEST,
                },
                "webBaseUrl": "https://app.example.test",
                "adminBaseUrl": "https://admin.example.test",
            },
            "startedAt": "2026-08-02T00:00:00Z",
            "completedAt": "2026-08-02T02:00:00Z",
            "accounts": [
                {
                    "role": role,
                    "subjectReference": f"actor/{role}",
                    "authenticationMethod": "sso",
                    "sessionFreshAt": "2026-08-01T23:55:00Z",
                }
                for role in sorted(VALIDATOR.ACCOUNT_ROLES)
            ],
            "operations": [
                self.operation(operation_id, operation, index)
                for index, (operation_id, operation) in enumerate(sorted(expected.items()))
            ],
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
                "evidence": self.evidence_path("support-access-lifecycle"),
            },
            "approvals": [
                {
                    "role": role,
                    "subjectReference": f"approver/{role}",
                    "approved": True,
                    "approvedAt": "2026-08-02T03:00:00Z",
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
        path.write_text(
            (content or json.dumps({"evidence": name}, sort_keys=True)) + "\n",
            encoding="utf-8",
        )
        return path.relative_to(self.root).as_posix()

    def operation(
        self,
        operation_id: str,
        operation: dict[str, str],
        index: int,
    ) -> dict[str, object]:
        return {
            "id": operation_id,
            "surface": operation["surface"],
            "actor": operation["actor"],
            "access": operation["access"],
            "status": "passed",
            "startedAt": "2026-08-02T00:10:00Z",
            "completedAt": "2026-08-02T00:11:00Z",
            "requestIds": [
                str(uuid.uuid5(uuid.NAMESPACE_URL, f"synara-operations-prepare-{index}"))
            ],
            "negativeActor": operation["negativeActor"],
            "negativeResult": "denied",
            "usedCli": False,
            "usedDatabaseClient": False,
            "usedDeveloperTools": False,
            "positiveEvidence": self.evidence_path(f"{operation_id}-positive"),
            "negativeEvidence": self.evidence_path(f"{operation_id}-negative"),
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
            "--matrix",
            str(MATRIX),
            "--manifest-output",
            self.manifest.relative_to(self.root).as_posix(),
            "--receipt-output",
            self.receipt.relative_to(self.root).as_posix(),
            "--validated-at",
            "2026-08-02T04:00:00Z",
        ]

    def args(self) -> Namespace:
        return Namespace(
            evidence_root=str(self.root),
            draft=self.draft.relative_to(self.root).as_posix(),
            repository_root=str(REPO_ROOT),
            matrix=str(MATRIX),
            manifest_output=self.manifest.relative_to(self.root).as_posix(),
            receipt_output=self.receipt.relative_to(self.root).as_posix(),
            validated_at="2026-08-02T04:00:00Z",
        )

    def test_materializes_hashes_and_publishes_validated_outputs(self) -> None:
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("eligibleForHumanGateReview=true", result.stdout)
        manifest = json.loads(self.manifest.read_text(encoding="utf-8"))
        receipt = json.loads(self.receipt.read_text(encoding="utf-8"))
        self.assertEqual(manifest["schemaVersion"], VALIDATOR.SCHEMA_VERSION)
        self.assertNotIn("matrixSha256", self.payload)
        self.assertEqual(manifest["matrixSha256"], receipt["matrix"]["sha256"])
        self.assertEqual(set(manifest["operations"][0]["positiveEvidence"]), {"path", "sha256"})
        self.assertEqual(receipt["evidenceFileCount"], len(self.expected) * 2 + 3)
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
        self.payload["operations"].pop()
        self.write_draft()
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("every source matrix row", result.stderr)
        self.assertFalse(self.manifest.exists())
        self.assertFalse(self.receipt.exists())

    def test_reused_or_secret_evidence_publishes_nothing_without_echoing_secret(self) -> None:
        self.payload["operations"][1]["positiveEvidence"] = self.payload["operations"][0][
            "positiveEvidence"
        ]
        self.write_draft()
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("duplicates another evidence file", result.stderr)
        self.assertFalse(self.manifest.exists())

        secret = "Authorization: Bearer operations-secret-value"
        self.payload["operations"][1]["positiveEvidence"] = self.evidence_path(
            "unsafe-operations-evidence",
            secret,
        )
        self.write_draft()
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("prohibited bearer credential material", result.stderr)
        self.assertNotIn(secret, result.stderr)
        self.assertFalse(self.manifest.exists())

    def test_rejects_symlinked_draft_and_traversal_output(self) -> None:
        target = self.root / "real-operations-draft.json"
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
                raise PREPARE.ImmutableEvidenceIOError(
                    "simulated receipt publication failure"
                )
            real_publish(path, encoded)

        with mock.patch.object(
            PREPARE,
            "publish_immutable_with_sha256",
            side_effect=fail_receipt,
        ):
            with self.assertRaises(PREPARE.OperationsPreparationError):
                PREPARE.prepare(self.args())
        self.assertFalse(self.manifest.exists())
        self.assertFalse(self.manifest.with_suffix(".json.sha256").exists())
        self.assertFalse(self.receipt.exists())
        self.assertFalse(self.receipt.with_suffix(".json.sha256").exists())


if __name__ == "__main__":
    unittest.main()
