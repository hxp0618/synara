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

PREPARE_SCRIPT = DIRECTORY / "prepare_desktop_native_acceptance.py"
PREPARE_SPEC = importlib.util.spec_from_file_location(
    "prepare_desktop_native_acceptance", PREPARE_SCRIPT
)
assert PREPARE_SPEC is not None and PREPARE_SPEC.loader is not None
PREPARE = importlib.util.module_from_spec(PREPARE_SPEC)
PREPARE_SPEC.loader.exec_module(PREPARE)

FIXTURE_SCRIPT = DIRECTORY / "test_validate_desktop_native_acceptance.py"
FIXTURE_SPEC = importlib.util.spec_from_file_location(
    "desktop_native_validator_fixture", FIXTURE_SCRIPT
)
assert FIXTURE_SPEC is not None and FIXTURE_SPEC.loader is not None
FIXTURE = importlib.util.module_from_spec(FIXTURE_SPEC)
FIXTURE_SPEC.loader.exec_module(FIXTURE)


class PrepareDesktopNativeAcceptanceTest(unittest.TestCase):
    def setUp(self) -> None:
        self.fixture = FIXTURE.ValidateDesktopNativeAcceptanceTest(
            "test_complete_native_candidate_is_review_eligible_but_not_ga_approved"
        )
        self.fixture.setUp()
        self.root = self.fixture.root
        self.evidence = self.fixture.evidence
        self.payload = copy.deepcopy(self.fixture.payload)
        self.payload["schemaVersion"] = PREPARE.DRAFT_SCHEMA
        for target in self.payload["targets"]:
            target["evidence"] = {
                field: (self.evidence / reference["path"])
                .relative_to(self.root)
                .as_posix()
                for field, reference in target["evidence"].items()
            }
        for approval in self.payload["approvals"]:
            approval["evidence"] = (
                self.evidence / approval["evidence"]["path"]
            ).relative_to(self.root).as_posix()
        self.draft = self.root / "desktop-native-draft.json"
        self.manifest = self.root / "desktop-native-manifest.json"
        self.receipt = self.root / "desktop-native-receipt.json"
        self.write_draft()

    def tearDown(self) -> None:
        self.fixture.tearDown()

    def write_draft(self) -> None:
        self.draft.write_text(
            json.dumps(self.payload, indent=2) + "\n", encoding="utf-8"
        )

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
            "2026-07-31T04:00:00Z",
        ]

    def args(self) -> Namespace:
        return Namespace(
            evidence_root=str(self.root),
            draft=self.draft.name,
            manifest_output=self.manifest.name,
            receipt_output=self.receipt.name,
            validated_at="2026-07-31T04:00:00Z",
        )

    def test_materializes_all_hashes_and_publishes_private_outputs(self) -> None:
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("eligibleForHumanGateReview=true", result.stdout)
        manifest = json.loads(self.manifest.read_text(encoding="utf-8"))
        receipt = json.loads(self.receipt.read_text(encoding="utf-8"))
        self.assertEqual(manifest["schemaVersion"], PREPARE.SCHEMA_VERSION)
        self.assertEqual(receipt["assessment"], PREPARE.ASSESSMENT)
        self.assertGreater(receipt["evidenceByteCount"], 0)
        self.assertEqual(receipt["evidenceFileCount"], 35)
        for target in manifest["targets"]:
            for reference in target["evidence"].values():
                self.assertEqual(set(reference), {"path", "sha256"})
        for output in (
            self.manifest,
            self.manifest.with_suffix(".json.sha256"),
            self.receipt,
            self.receipt.with_suffix(".json.sha256"),
        ):
            self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)

    def test_rejects_duplicate_draft_field_without_publication(self) -> None:
        encoded = json.dumps(self.payload, indent=2)
        duplicate = encoded.replace(
            '"schemaVersion": "synara.stage6-desktop-native-acceptance-draft.v1",',
            '"schemaVersion": "synara.stage6-desktop-native-acceptance-draft.v1",\n'
            '  "schemaVersion": "synara.stage6-desktop-native-acceptance-draft.v1",',
            1,
        )
        self.draft.write_text(duplicate + "\n", encoding="utf-8")
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("safe valid UTF-8 JSON", result.stderr)
        self.assertFalse(self.manifest.exists())

    def test_rejects_secret_or_symlinked_evidence_without_echoing_secret(self) -> None:
        secret = "Authorization: Bearer desktop-native-preparer-secret"
        unsafe = self.evidence / "unsafe-installation.json"
        unsafe.write_text(secret + "\n", encoding="utf-8")
        self.payload["targets"][0]["evidence"]["installation"] = unsafe.relative_to(
            self.root
        ).as_posix()
        self.write_draft()
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("prohibited bearer credential material", result.stderr)
        self.assertNotIn(secret, result.stderr)

        original = self.evidence / "replacement-installation.json"
        original.write_text('{"safe":true}\n', encoding="utf-8")
        link = self.evidence / "linked-installation.json"
        link.symlink_to(original.name)
        self.payload["targets"][0]["evidence"]["installation"] = link.relative_to(
            self.root
        ).as_posix()
        self.write_draft()
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("must not traverse a symlink", result.stderr)

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
            with self.assertRaises(PREPARE.DesktopAcceptancePreparationError):
                PREPARE.prepare(self.args())
        self.assertFalse(self.manifest.exists())
        self.assertFalse(self.manifest.with_suffix(".json.sha256").exists())
        self.assertFalse(self.receipt.exists())


if __name__ == "__main__":
    unittest.main()
