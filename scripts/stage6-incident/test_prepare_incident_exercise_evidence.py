from __future__ import annotations

import copy
import importlib.util
import json
import pathlib
import stat
import subprocess
import sys
import unittest
from argparse import Namespace
from unittest import mock


DIRECTORY = pathlib.Path(__file__).resolve().parent
if str(DIRECTORY) not in sys.path:
    sys.path.insert(0, str(DIRECTORY))

PREPARE_SCRIPT = DIRECTORY / "prepare_incident_exercise_evidence.py"
PREPARE_SPEC = importlib.util.spec_from_file_location(
    "prepare_incident_exercise_evidence",
    PREPARE_SCRIPT,
)
assert PREPARE_SPEC is not None and PREPARE_SPEC.loader is not None
PREPARE = importlib.util.module_from_spec(PREPARE_SPEC)
PREPARE_SPEC.loader.exec_module(PREPARE)

FIXTURE_SCRIPT = DIRECTORY / "test_validate_incident_exercise_evidence.py"
FIXTURE_SPEC = importlib.util.spec_from_file_location(
    "incident_validator_fixture",
    FIXTURE_SCRIPT,
)
assert FIXTURE_SPEC is not None and FIXTURE_SPEC.loader is not None
FIXTURE = importlib.util.module_from_spec(FIXTURE_SPEC)
FIXTURE_SPEC.loader.exec_module(FIXTURE)


class PrepareIncidentExerciseEvidenceTest(unittest.TestCase):
    def setUp(self) -> None:
        self.fixture = FIXTURE.ValidateIncidentExerciseEvidenceTest(
            "test_validates_live_exercise_without_declaring_operations_ready"
        )
        self.fixture.setUp()
        self.root = self.fixture.root
        self.evidence = self.fixture.evidence
        self.payload = copy.deepcopy(self.fixture.payload)
        self.payload["schemaVersion"] = PREPARE.DRAFT_SCHEMA
        self.payload["evidence"] = {
            field: (self.evidence / reference["path"]).relative_to(self.root).as_posix()
            for field, reference in self.fixture.references.items()
        }
        self.draft = self.root / "incident-draft.json"
        self.manifest = self.root / "incident-manifest.json"
        self.receipt = self.root / "incident-receipt.json"
        self.write_draft()

    def tearDown(self) -> None:
        self.fixture.tearDown()

    def write_draft(self) -> None:
        self.draft.write_text(json.dumps(self.payload, indent=2) + "\n", encoding="utf-8")

    def command(self) -> list[str]:
        return [
            sys.executable,
            str(PREPARE_SCRIPT),
            "--evidence-root",
            str(self.root),
            "--draft",
            self.draft.name,
            "--manifest-output",
            self.manifest.name,
            "--receipt-output",
            self.receipt.name,
            "--validated-at",
            "2026-07-30T03:00:00Z",
        ]

    def args(self) -> Namespace:
        return Namespace(
            evidence_root=str(self.root),
            draft=self.draft.name,
            manifest_output=self.manifest.name,
            receipt_output=self.receipt.name,
            validated_at="2026-07-30T03:00:00Z",
        )

    def test_materializes_hashes_and_publishes_validated_outputs(self) -> None:
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("eligibleForHumanGateReview=true", result.stdout)
        manifest = json.loads(self.manifest.read_text(encoding="utf-8"))
        receipt = json.loads(self.receipt.read_text(encoding="utf-8"))
        self.assertEqual(manifest["schemaVersion"], PREPARE.MANIFEST_SCHEMA)
        self.assertEqual(set(manifest["evidence"]["onCallRotaEvidence"]), {"path", "sha256"})
        self.assertEqual(receipt["manifest"]["path"], self.manifest.name)
        self.assertEqual(receipt["assessment"], PREPARE.ASSESSMENT)
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

    def test_failed_exercise_is_preserved_but_not_review_eligible(self) -> None:
        self.payload["employeeNotificationDelivery"]["deliverySucceeded"] = False
        self.write_draft()
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = json.loads(self.receipt.read_text(encoding="utf-8"))
        self.assertFalse(receipt["employeeNotificationDeliveryComplete"])
        self.assertFalse(receipt["eligibleForHumanGateReview"])

    def test_secret_evidence_publishes_nothing_without_echoing_secret(self) -> None:
        secret = "Authorization: Bearer incident-exercise-secret"
        unsafe = self.evidence / "unsafe-incident-evidence.json"
        unsafe.write_text(secret + "\n", encoding="utf-8")
        self.payload["evidence"]["pagingProviderEvidence"] = unsafe.relative_to(
            self.root
        ).as_posix()
        self.write_draft()
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("prohibited bearer credential material", result.stderr)
        self.assertNotIn(secret, result.stderr)
        self.assertFalse(self.manifest.exists())

    def test_rejects_symlinked_evidence_and_traversal_output(self) -> None:
        original = self.root / self.payload["evidence"]["onCallRotaEvidence"]
        link = self.evidence / "linked-rota-evidence.json"
        link.symlink_to(original.name)
        self.payload["evidence"]["onCallRotaEvidence"] = link.relative_to(
            self.root
        ).as_posix()
        self.write_draft()
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("must not traverse a symlink", result.stderr)

        self.payload["evidence"]["onCallRotaEvidence"] = original.relative_to(
            self.root
        ).as_posix()
        self.write_draft()
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
            with self.assertRaises(PREPARE.IncidentPreparationError):
                PREPARE.prepare(self.args())
        self.assertFalse(self.manifest.exists())
        self.assertFalse(self.manifest.with_suffix(".json.sha256").exists())
        self.assertFalse(self.receipt.exists())
        self.assertFalse(self.receipt.with_suffix(".json.sha256").exists())


if __name__ == "__main__":
    unittest.main()
