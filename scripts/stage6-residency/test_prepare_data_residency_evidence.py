from __future__ import annotations

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

PREPARE_SCRIPT = DIRECTORY / "prepare_data_residency_evidence.py"
PREPARE_SPEC = importlib.util.spec_from_file_location(
    "prepare_data_residency_evidence", PREPARE_SCRIPT
)
assert PREPARE_SPEC is not None and PREPARE_SPEC.loader is not None
PREPARE = importlib.util.module_from_spec(PREPARE_SPEC)
PREPARE_SPEC.loader.exec_module(PREPARE)

FIXTURE_SCRIPT = DIRECTORY / "test_validate_data_residency_evidence.py"
FIXTURE_SPEC = importlib.util.spec_from_file_location(
    "residency_validator_fixture", FIXTURE_SCRIPT
)
assert FIXTURE_SPEC is not None and FIXTURE_SPEC.loader is not None
FIXTURE = importlib.util.module_from_spec(FIXTURE_SPEC)
FIXTURE_SPEC.loader.exec_module(FIXTURE)


class PrepareDataResidencyEvidenceTest(unittest.TestCase):
    def setUp(self) -> None:
        self.fixture = FIXTURE.ValidateDataResidencyEvidenceTest(
            "test_strict_semantic_evidence_set_is_review_eligible_without_signature_claim"
        )
        self.fixture.setUp()
        self.root = self.fixture.root
        self.evidence = self.fixture.evidence_root
        self.payload = {
            "schemaVersion": PREPARE.DRAFT_SCHEMA,
            "evidence": {
                field: (self.evidence / reference["path"])
                .relative_to(self.root)
                .as_posix()
                for field, reference in self.fixture.references.items()
            },
        }
        self.draft = self.root / "residency-draft.json"
        self.manifest = self.root / "residency-manifest.json"
        self.receipt = self.root / "residency-receipt.json"
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
            "--expected-candidate-binding-sha256",
            self.fixture.expected_candidate_binding,
            "--validated-at",
            "2026-08-01T12:00:00Z",
        ]

    def args(self) -> Namespace:
        return Namespace(
            evidence_root=str(self.root),
            draft=self.draft.name,
            manifest_output=self.manifest.name,
            receipt_output=self.receipt.name,
            expected_candidate_binding_sha256=self.fixture.expected_candidate_binding,
            validated_at="2026-08-01T12:00:00Z",
        )

    def test_materializes_hashes_and_publishes_validated_outputs(self) -> None:
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("eligibleForHumanGateReview=true", result.stdout)
        manifest = json.loads(self.manifest.read_text(encoding="utf-8"))
        receipt = json.loads(self.receipt.read_text(encoding="utf-8"))
        self.assertEqual(manifest["schemaVersion"], PREPARE.SCHEMA_VERSION)
        self.assertEqual(set(manifest["evidence"]["annex"]), {"path", "sha256"})
        self.assertEqual(receipt["manifest"]["path"], self.manifest.name)
        self.assertEqual(receipt["assessment"], PREPARE.ASSESSMENT)
        for output in (
            self.manifest,
            self.manifest.with_suffix(".json.sha256"),
            self.receipt,
            self.receipt.with_suffix(".json.sha256"),
        ):
            self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)

    def test_failed_exercise_is_preserved_as_ineligible_receipt(self) -> None:
        self.fixture.documents["failoverExercise"]["exercise"]["result"] = "failed"
        self.fixture.write_evidence_set()
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = json.loads(self.receipt.read_text(encoding="utf-8"))
        self.assertFalse(receipt["checks"]["failoverAcceptanceSatisfied"])
        self.assertFalse(receipt["eligibleForHumanGateReview"])

    def test_secret_attachment_publishes_nothing_without_echoing_secret(self) -> None:
        secret = "Authorization: Bearer residency-preparer-secret"
        unsafe = self.fixture.document_path("browserStatement")
        unsafe.write_text(
            unsafe.read_text(encoding="utf-8") + secret + "\n",
            encoding="utf-8",
        )
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("prohibited bearer credential material", result.stderr)
        self.assertNotIn(secret, result.stderr)
        self.assertFalse(self.manifest.exists())

    def test_rejects_symlinked_attachment_and_traversal_output(self) -> None:
        original = self.fixture.document_path("browserStatement")
        link = self.evidence / "linked-browser.json"
        link.symlink_to(original.name)
        self.payload["evidence"]["browserStatement"] = link.relative_to(
            self.root
        ).as_posix()
        self.write_draft()
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("must not traverse a symlink", result.stderr)

        self.payload["evidence"]["browserStatement"] = original.relative_to(
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
            PREPARE, "publish_immutable_with_sha256", side_effect=fail_receipt
        ):
            with self.assertRaises(PREPARE.DataResidencyPreparationError):
                PREPARE.prepare(self.args())
        self.assertFalse(self.manifest.exists())
        self.assertFalse(self.manifest.with_suffix(".json.sha256").exists())
        self.assertFalse(self.receipt.exists())


if __name__ == "__main__":
    unittest.main()
