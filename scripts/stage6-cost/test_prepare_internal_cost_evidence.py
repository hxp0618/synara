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


DIRECTORY = pathlib.Path(__file__).resolve().parent
if str(DIRECTORY) not in sys.path:
    sys.path.insert(0, str(DIRECTORY))

PREPARE_SCRIPT = DIRECTORY / "prepare_internal_cost_evidence.py"
PREPARE_SPEC = importlib.util.spec_from_file_location(
    "prepare_internal_cost_evidence",
    PREPARE_SCRIPT,
)
assert PREPARE_SPEC is not None and PREPARE_SPEC.loader is not None
PREPARE = importlib.util.module_from_spec(PREPARE_SPEC)
PREPARE_SPEC.loader.exec_module(PREPARE)

VALIDATOR_SCRIPT = DIRECTORY / "validate_internal_cost_evidence.py"
VALIDATOR_SPEC = importlib.util.spec_from_file_location(
    "internal_cost_validator_for_prepare_test",
    VALIDATOR_SCRIPT,
)
assert VALIDATOR_SPEC is not None and VALIDATOR_SPEC.loader is not None
VALIDATOR = importlib.util.module_from_spec(VALIDATOR_SPEC)
VALIDATOR_SPEC.loader.exec_module(VALIDATOR)


class PrepareInternalCostEvidenceTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temporary.name)
        self.evidence = self.root / "evidence"
        self.evidence.mkdir()
        self.draft = self.root / "internal-cost-draft.json"
        self.manifest = self.root / "internal-cost-manifest.json"
        self.receipt = self.root / "internal-cost-receipt.json"
        self.payload = {
            "schemaVersion": PREPARE.DRAFT_SCHEMA,
            "candidate": {
                "candidateId": "v0.6.3-stage6.internal-cost",
                "sourceCommit": "a" * 40,
                "environment": "production-like",
                "environmentId": "stage6-internal-cost-01",
                "controlPlaneBaseUrl": "https://control.example.test/v1",
                "migrationTail": {
                    "name": "000161_internal_incident_communications.sql",
                    "sha256": "sha256:" + "b" * 64,
                },
            },
            "period": {
                "start": "2026-07-01T00:00:00Z",
                "end": "2026-08-01T00:00:00Z",
            },
            "usage": {
                "executionCount": 2,
                "inputTokens": 1000,
                "outputTokens": 250,
                "cachedInputTokens": 300,
                "cacheCreationInputTokens": 100,
                "providerCostReportedExecutionCount": 1,
                "providerCostUnavailableExecutionCount": 1,
                "actualPlatformAllocationCount": 1,
                "estimatedPlatformAllocationCount": 1,
            },
            "costs": {
                "providerByCurrency": {"USD": 15000},
                "platformByCurrency": {"USD": 9000, "CNY": 3200},
                "knownByCurrency": {"USD": 24000, "CNY": 3200},
            },
            "controls": {
                "tokenTotalsReconciled": True,
                "providerCoverageComplete": True,
                "actualOverridesEstimate": True,
                "currencySafeAggregation": True,
                "tenantIsolationValidated": True,
                "noPaymentDataPresent": True,
            },
            "evidence": [
                {"id": evidence_id, "path": self.evidence_path(evidence_id)}
                for evidence_id in sorted(VALIDATOR.EVIDENCE_IDS)
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
        self.assertEqual(manifest["schemaVersion"], VALIDATOR.MANIFEST_SCHEMA)
        self.assertEqual(set(manifest["evidence"][0]), {"id", "path", "sha256"})
        self.assertEqual(receipt["manifest"]["path"], self.manifest.name)
        self.assertEqual(receipt["validatedAt"], "2026-08-02T04:00:00Z")
        self.assertEqual(receipt["evidenceFileCount"], 4)
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

    def test_invalid_reconciliation_publishes_nothing(self) -> None:
        self.payload["costs"]["knownByCurrency"]["USD"] = 23999
        self.write_draft()
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("exactly equal", result.stderr)
        self.assertFalse(self.manifest.exists())
        self.assertFalse(self.receipt.exists())

    def test_secret_evidence_publishes_nothing_without_echoing_secret(self) -> None:
        secret = "Authorization: Bearer internal-cost-secret"
        self.payload["evidence"][0]["path"] = self.evidence_path(
            "unsafe-internal-cost-evidence",
            secret,
        )
        self.write_draft()
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("prohibited bearer credential material", result.stderr)
        self.assertNotIn(secret, result.stderr)
        self.assertFalse(self.manifest.exists())

    def test_rejects_symlinked_evidence_and_traversal_output(self) -> None:
        target = self.root / self.payload["evidence"][0]["path"]
        link = self.evidence / "linked-cost-evidence.json"
        link.symlink_to(target.name)
        self.payload["evidence"][0]["path"] = link.relative_to(self.root).as_posix()
        self.write_draft()
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("must not traverse a symlink", result.stderr)

        self.payload["evidence"][0]["path"] = target.relative_to(self.root).as_posix()
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
            with self.assertRaises(PREPARE.CostPreparationError):
                PREPARE.prepare(self.args())
        self.assertFalse(self.manifest.exists())
        self.assertFalse(self.manifest.with_suffix(".json.sha256").exists())
        self.assertFalse(self.receipt.exists())
        self.assertFalse(self.receipt.with_suffix(".json.sha256").exists())


if __name__ == "__main__":
    unittest.main()
