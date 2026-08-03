from __future__ import annotations

import copy
import hashlib
import json
import pathlib
import stat
import subprocess
import sys
import unittest

import test_validate_final_ga_review as validator_fixture


SCRIPT = pathlib.Path(__file__).with_name("prepare_final_ga_review.py")
DRAFT_SCHEMA_VERSION = "synara.stage6-final-ga-review-draft.v1"
REFERENCE_FIELDS = (
    "candidateBundleReceipt",
    "protectedReleaseApproval",
    "finalAssetSet",
    "copiedChecklist",
    "changeNotice",
    "platformAuditExport",
)


class PrepareFinalGAReviewTest(unittest.TestCase):
    def setUp(self) -> None:
        self.fixture = validator_fixture.ValidateFinalGAReviewTest(
            methodName="test_validates_exact_final_archive_without_claiming_ga_authority"
        )
        self.fixture.setUp()
        self.root = self.fixture.root
        self.draft_path = self.root / "final-review-draft.json"
        self.manifest_output = self.root / "prepared-final-review.json"
        self.receipt_output = self.root / "prepared-final-review-receipt.json"
        self.reset_draft()

    def tearDown(self) -> None:
        self.fixture.tearDown()

    def reset_draft(self) -> None:
        self.draft = copy.deepcopy(self.fixture.manifest)
        self.draft["schemaVersion"] = DRAFT_SCHEMA_VERSION
        for field in REFERENCE_FIELDS:
            self.draft["candidate"][field] = self.draft["candidate"][field]["path"]
        for control in self.draft["controlDecisions"]:
            control["evidence"] = [reference["path"] for reference in control["evidence"]]
        for approval in self.draft["finalApprovals"]:
            approval["evidence"] = approval["evidence"]["path"]

    def command(self) -> list[str]:
        return [
            sys.executable,
            str(SCRIPT),
            "--evidence-root",
            str(self.root),
            "--draft",
            self.draft_path.name,
            "--manifest-output",
            self.manifest_output.name,
            "--receipt-output",
            self.receipt_output.name,
            "--validated-at",
            "2026-08-02T04:00:00Z",
        ]

    def run_preparer(self) -> subprocess.CompletedProcess[str]:
        self.draft_path.write_text(json.dumps(self.draft, indent=2) + "\n", encoding="utf-8")
        return subprocess.run(self.command(), check=False, capture_output=True, text=True)

    def test_computes_references_validates_and_publishes_complete_archive(self) -> None:
        result = self.run_preparer()
        self.assertEqual(result.returncode, 0, result.stderr)
        manifest = json.loads(self.manifest_output.read_text(encoding="utf-8"))
        receipt = json.loads(self.receipt_output.read_text(encoding="utf-8"))
        candidate_reference = manifest["candidate"]["candidateBundleReceipt"]
        candidate_bytes = (self.root / candidate_reference["path"]).read_bytes()
        self.assertEqual(
            candidate_reference["sha256"],
            "sha256:" + hashlib.sha256(candidate_bytes).hexdigest(),
        )
        self.assertEqual(manifest["schemaVersion"], "synara.stage6-final-ga-review.v1")
        self.assertTrue(receipt["eligibleForExternalGAAuthorityReview"])
        for path in (
            self.manifest_output,
            self.root / "prepared-final-review.json.sha256",
            self.receipt_output,
            self.root / "prepared-final-review-receipt.json.sha256",
        ):
            self.assertTrue(path.is_file())
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
        self.assertEqual(list(self.root.glob(".stage6-final-review-*.json")), [])

    def test_preserves_noneligible_failed_review_without_relabeling_it(self) -> None:
        self.draft["controlDecisions"][0]["status"] = "blocked"
        result = self.run_preparer()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = json.loads(self.receipt_output.read_text(encoding="utf-8"))
        self.assertFalse(receipt["eligibleForExternalGAAuthorityReview"])
        self.assertEqual(receipt["controlStatusCounts"]["blocked"], 1)

    def test_semantic_failure_leaves_no_outputs(self) -> None:
        self.draft["candidate"]["sourceCommit"] = "f" * 40
        result = self.run_preparer()
        self.assertEqual(result.returncode, 2)
        self.assertIn("does not match final review identity", result.stderr)
        self.assertFalse(self.manifest_output.exists())
        self.assertFalse(self.receipt_output.exists())
        self.assertEqual(list(self.root.glob(".stage6-final-review-*.json")), [])

    def test_existing_receipt_rolls_back_without_publishing_manifest(self) -> None:
        retained = "retained receipt\n"
        self.receipt_output.write_text(retained, encoding="utf-8")
        result = self.run_preparer()
        self.assertEqual(result.returncode, 2)
        self.assertIn("must be a new path", result.stderr)
        self.assertFalse(self.manifest_output.exists())
        self.assertEqual(self.receipt_output.read_text(encoding="utf-8"), retained)

    def test_rejects_symlinked_path_only_evidence(self) -> None:
        target = self.root / self.draft["controlDecisions"][0]["evidence"][0]
        linked = self.root / "linked-control-evidence.json"
        linked.symlink_to(target)
        self.draft["controlDecisions"][0]["evidence"][0] = linked.name
        result = self.run_preparer()
        self.assertEqual(result.returncode, 2)
        self.assertIn("must not traverse a symlink", result.stderr)
        self.assertFalse(self.manifest_output.exists())


if __name__ == "__main__":
    unittest.main()
