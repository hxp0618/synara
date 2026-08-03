from __future__ import annotations

import hashlib
import json
import pathlib
import stat
import subprocess
import sys
import tempfile
import unittest


SCRIPT = pathlib.Path(__file__).with_name("validate_slo_evidence.py")


class ValidateSLOEvidenceTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temporary.name)
        self.evidence = self.root / "evidence"
        self.evidence.mkdir()
        self.references: dict[str, dict[str, str]] = {}
        for field in (
            "prometheusQueryEvidence",
            "externalProbeEvidence",
            "alertHistoryEvidence",
            "incidentMaintenanceEvidence",
            "deploymentIdentityEvidence",
            "resultSummaryEvidence",
        ):
            path = self.evidence / f"{field}.json"
            path.write_text(f"{field} proof\n", encoding="utf-8")
            self.references[field] = {
                "path": path.name,
                "sha256": "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest(),
            }
        self.payload = {
            "schemaVersion": "synara.slo-window-evidence.v1",
            "windowId": "04a63229-2a4d-478f-9379-6626b4717344",
            "releaseCommit": "d" * 40,
            "environmentClass": "production-like",
            "environmentId": "prodlike/slo-1",
            "publicOrigin": "https://stage6.example.test",
            "windowStartedAt": "2026-06-30T00:00:00Z",
            "windowCompletedAt": "2026-07-30T00:00:00Z",
            "queryRevision": "e" * 40,
            "externalProbe": {
                "regions": ["ap-east", "eu-west", "us-west"],
                "cadenceSeconds": 30,
                "expectedSamples": 259_200,
                "observedSamples": 250_000,
            },
            "objectives": {
                "availability": self.objective(0.9995, 250_000, 0.5),
                "apiLatency": self.objective(0.995, 20_000, 0.5),
                "executionStartDelay": self.objective(0.995, 500, 0.5),
                "eventDelay": self.objective(0.9995, 5_000, 0.5),
            },
            "alertSummary": {
                "availabilityFastBurnCount": 0,
                "apiLatencyFastBurnCount": 0,
                "availabilityBudgetLowCount": 0,
                "apiLatencyBudgetLowCount": 0,
                "executionStartBudgetLowCount": 0,
                "eventDelayBudgetLowCount": 0,
                "publicProbeMissingCount": 0,
            },
            "companionSignals": {
                "executionTerminalBeforeReadyCount": 0,
                "maximumQueueOldestSeconds": 20,
                "maximumOutboxOldestSeconds": 30,
                "expiredSSELeaseCount": 0,
                "sseCatchupP95Seconds": 0.5,
                "reviewed": True,
            },
            "burnReview": {
                "allMaterialBurnsMapped": True,
                "allIncidentsReviewed": True,
                "allMaintenanceIncluded": True,
                "allExceptionsDocumented": True,
                "openUnownedCorrectiveActionCount": 0,
            },
            "evidence": self.references,
        }
        self.manifest = self.root / "manifest.json"
        self.write_manifest()

    @staticmethod
    def objective(ratio: float, samples: int, remaining: float) -> dict[str, object]:
        return {
            "goodRatio": ratio,
            "sampleCount": samples,
            "errorBudgetRemainingRatio": remaining,
            "noDataIntervalCount": 0,
            "excludedSampleCount": 0,
            "querySucceeded": True,
        }

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def write_manifest(self) -> None:
        self.manifest.write_text(json.dumps(self.payload, indent=2) + "\n", encoding="utf-8")

    def command(self) -> list[str]:
        return [
            sys.executable,
            str(SCRIPT),
            "--manifest",
            str(self.manifest),
            "--evidence-root",
            str(self.evidence),
            "--output",
            str(self.root / "receipt.json"),
            "--validated-at",
            "2026-07-30T01:00:00Z",
        ]

    def run_validator(self) -> subprocess.CompletedProcess[str]:
        self.write_manifest()
        return subprocess.run(self.command(), check=False, capture_output=True, text=True)

    def receipt(self) -> dict[str, object]:
        return json.loads((self.root / "receipt.json").read_text(encoding="utf-8"))

    def test_validates_complete_window_without_declaring_slo_passed(self) -> None:
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertEqual(receipt["assessment"], "evidence-validated-not-slo-passed")
        self.assertTrue(receipt["allObjectivesAssessable"])
        self.assertTrue(receipt["declaredMeasurementsWithinObjectives"])
        self.assertTrue(receipt["eligibleForHumanGateReview"])
        self.assertTrue((self.root / "receipt.json.sha256").is_file())
        self.assertEqual(stat.S_IMODE((self.root / "receipt.json").stat().st_mode), 0o600)
        self.assertEqual(
            stat.S_IMODE((self.root / "receipt.json.sha256").stat().st_mode), 0o600
        )

    def test_preserves_a_measured_objective_miss_as_failed_evidence(self) -> None:
        self.payload["objectives"]["apiLatency"]["goodRatio"] = 0.98
        self.payload["objectives"]["apiLatency"]["errorBudgetRemainingRatio"] = 0.0
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertFalse(receipt["declaredMeasurementsWithinObjectives"])
        self.assertFalse(receipt["eligibleForHumanGateReview"])
        self.assertEqual(
            receipt["objectives"]["apiLatency"]["errorBudgetPolicyState"],
            "reliability-freeze",
        )

    def test_insufficient_volume_is_not_assessable(self) -> None:
        self.payload["objectives"]["executionStartDelay"]["sampleCount"] = 99
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertFalse(receipt["allObjectivesAssessable"])
        self.assertFalse(receipt["objectives"]["executionStartDelay"]["volumeSufficient"])

    def test_rejects_short_release_window(self) -> None:
        self.payload["windowStartedAt"] = "2026-07-01T00:00:01Z"
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("below the minimum duration", result.stderr)

    def test_rejects_error_budget_mismatch_or_understated_probe_denominator(self) -> None:
        self.payload["objectives"]["availability"]["errorBudgetRemainingRatio"] = 0.6
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("does not match the SLO formula", result.stderr)

        self.payload["objectives"]["availability"]["errorBudgetRemainingRatio"] = 0.5
        self.payload["externalProbe"]["expectedSamples"] = 250_000
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("understates the declared window", result.stderr)

    def test_probe_missing_alert_requires_no_data_and_blocks_assessment(self) -> None:
        self.payload["alertSummary"]["publicProbeMissingCount"] = 1
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("requires an Availability no-data interval", result.stderr)

        self.payload["objectives"]["availability"]["noDataIntervalCount"] = 1
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.receipt()["allObjectivesAssessable"])

    def test_rejects_tampered_or_reused_evidence(self) -> None:
        (self.evidence / "alertHistoryEvidence.json").write_text("tampered\n", encoding="utf-8")
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("sha256 does not match", result.stderr)

        self.references["alertHistoryEvidence"] = self.references["externalProbeEvidence"]
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("duplicates another evidence file", result.stderr)

    def test_rejects_secret_evidence_without_echoing_secret(self) -> None:
        secret = "Authorization: Bearer slo-validator-secret"
        path = self.evidence / "resultSummaryEvidence.json"
        path.write_text(secret + "\n", encoding="utf-8")
        self.references["resultSummaryEvidence"] = {
            "path": path.name,
            "sha256": "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest(),
        }
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("prohibited bearer credential material", result.stderr)
        self.assertNotIn(secret, result.stderr)

    def test_rejects_duplicate_manifest_fields(self) -> None:
        self.write_manifest()
        encoded = self.manifest.read_text(encoding="utf-8")
        duplicated = encoded.replace(
            '  "schemaVersion": "synara.slo-window-evidence.v1",',
            '  "schemaVersion": "synara.slo-window-evidence.v1",\n'
            '  "schemaVersion": "synara.slo-window-evidence.v1",',
            1,
        )
        self.manifest.write_text(duplicated, encoding="utf-8")
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("duplicate JSON field", result.stderr)


if __name__ == "__main__":
    unittest.main()
