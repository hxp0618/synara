from __future__ import annotations

import hashlib
import json
import pathlib
import stat
import subprocess
import sys
import tempfile
import unittest


SCRIPT = pathlib.Path(__file__).with_name("validate_capacity_evidence.py")


class ValidateCapacityEvidenceTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temporary.name)
        self.evidence = self.root / "evidence"
        self.evidence.mkdir()
        self.references = {}
        for field in (
            "workloadGeneratorEvidence",
            "prometheusRangeEvidence",
            "externalCanaryEvidence",
            "databaseEvidence",
            "kubernetesEvidence",
            "applicationLogEvidence",
            "resultSummaryEvidence",
        ):
            path = self.evidence / f"{field}.json"
            path.write_text(f"{field} proof\n", encoding="utf-8")
            self.references[field] = {
                "path": path.name,
                "sha256": "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest(),
            }
        phase_names = ["steady-peak", "burst", "tenant-hotspot", "rolling-disruption", "cooldown"]
        self.phases = []
        for index, name in enumerate(phase_names):
            self.phases.append(
                {
                    "name": name,
                    "startedAt": f"2026-07-30T00:0{index}:00Z",
                    "completedAt": f"2026-07-30T00:0{index}:30Z",
                    "loadMultiplier": 1.2 if name != "burst" else 2.0,
                    "executed": True,
                }
            )
        self.payload = {
            "schemaVersion": "synara.capacity-soak-evidence.v1",
            "runId": "281f2d9a-3f61-4d94-a6aa-a1a4b5537a63",
            "releaseCommit": "b" * 40,
            "environmentClass": "fixture",
            "environmentId": "fixture/capacity-1",
            "startedAt": "2026-07-30T00:00:00Z",
            "completedAt": "2026-07-30T00:10:00Z",
            "sampleIntervalSeconds": 30,
            "observedExternalProbeSamples": 60,
            "forecast": {
                "peakConcurrentSessions": 100,
                "peakExecutionStartsPerMinute": 50,
                "peakEventAppendsPerSecond": 100,
                "peakSSEConnections": 200,
                "requiredHeadroomPercent": 20,
            },
            "load": {
                "peakConcurrentSessions": 120,
                "peakExecutionStartsPerMinute": 60,
                "peakEventAppendsPerSecond": 120,
                "peakSSEConnections": 240,
            },
            "phases": self.phases,
            "measurements": {
                "availabilityGoodRatio": 1.0,
                "apiLatencyGoodRatio": 0.995,
                "executionStartGoodRatio": 0.995,
                "eventDelayGoodRatio": 1.0,
                "httpRequestCount": 10000,
                "executionStartCount": 100,
                "eventAppendCount": 1000,
                "maximumDatabaseConnectionUtilizationRatio": 0.7,
                "maximumControlPlaneCPUUtilizationRatio": 0.7,
                "maximumControlPlaneMemoryUtilizationRatio": 0.7,
                "maximumOutboxOldestSeconds": 30,
                "maximumQueueOldestSeconds": 20,
                "maximumWarmDeficitUnits": 0,
                "deadLetterCount": 0,
                "oomKillCount": 0,
                "unexpectedRestartCount": 0,
                "minimumTenantSuccessRatio": 0.9,
                "maximumTenantSuccessRatio": 1.0,
                "failedAssertionCount": 0,
            },
            "exercises": {
                "externalProbeRegions": 3,
                "controlPlaneRollingRestart": True,
                "workerChurn": True,
                "databaseConnectionPressure": True,
                "outboxBackpressure": True,
                "tenantHotspot": True,
            },
            "evidence": self.references,
        }
        self.manifest = self.root / "manifest.json"
        self.write_manifest()

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

    def test_validates_headroom_slo_and_non_pass_assessment(self) -> None:
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = json.loads((self.root / "receipt.json").read_text(encoding="utf-8"))
        self.assertEqual(receipt["assessment"], "evidence-validated-not-capacity-passed")
        self.assertTrue(receipt["forecastHeadroomCovered"])
        self.assertTrue(receipt["declaredMeasurementsWithinObjectives"])
        self.assertFalse(receipt["releaseEligibleEnvironment"])
        self.assertFalse(receipt["eligibleForHumanGateReview"])
        self.assertEqual(receipt["startedAt"], self.payload["startedAt"])
        self.assertEqual(receipt["completedAt"], self.payload["completedAt"])
        self.assertEqual(receipt["forecast"], self.payload["forecast"])
        self.assertEqual(receipt["load"], self.payload["load"])
        self.assertEqual(receipt["exercises"], self.payload["exercises"])
        self.assertTrue((self.root / "receipt.json.sha256").is_file())
        self.assertEqual(stat.S_IMODE((self.root / "receipt.json").stat().st_mode), 0o600)
        self.assertEqual(
            stat.S_IMODE((self.root / "receipt.json.sha256").stat().st_mode), 0o600
        )

    def test_records_measurement_failure_without_calling_capacity_passed(self) -> None:
        self.payload["measurements"]["oomKillCount"] = 1
        self.write_manifest()
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = json.loads((self.root / "receipt.json").read_text(encoding="utf-8"))
        self.assertFalse(receipt["declaredMeasurementsWithinObjectives"])
        self.assertFalse(receipt["eligibleForHumanGateReview"])
        self.assertEqual(receipt["assessment"], "evidence-validated-not-capacity-passed")

    def test_marks_a_complete_production_like_run_eligible_for_human_review(self) -> None:
        self.payload["environmentClass"] = "production-like"
        self.payload["completedAt"] = "2026-07-31T00:00:00Z"
        self.payload["observedExternalProbeSamples"] = 8640
        self.write_manifest()
        command = self.command()
        command[-1] = "2026-07-31T01:00:00Z"
        result = subprocess.run(command, check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = json.loads((self.root / "receipt.json").read_text(encoding="utf-8"))
        self.assertTrue(receipt["releaseEligibleEnvironment"])
        self.assertTrue(receipt["eligibleForHumanGateReview"])
        self.assertEqual(receipt["minimumDurationSeconds"], 24 * 60 * 60)

    def test_rejects_load_below_forecast_headroom(self) -> None:
        self.payload["load"]["peakSSEConnections"] = 239
        self.write_manifest()
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("forecast plus required headroom", result.stderr)

    def test_rejects_short_environment_run(self) -> None:
        self.payload["environmentClass"] = "production-like"
        self.write_manifest()
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("run duration is below", result.stderr)

    def test_rejects_tampered_or_reused_evidence(self) -> None:
        (self.evidence / "databaseEvidence.json").write_text("tampered\n", encoding="utf-8")
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("sha256 does not match", result.stderr)

    def test_rejects_secret_evidence_without_echoing_secret(self) -> None:
        secret = "Authorization: Bearer capacity-test-secret"
        evidence_path = self.evidence / "applicationLogEvidence.json"
        evidence_path.write_text(secret + "\n", encoding="utf-8")
        self.references["applicationLogEvidence"]["sha256"] = (
            "sha256:" + hashlib.sha256(evidence_path.read_bytes()).hexdigest()
        )
        self.write_manifest()
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("prohibited bearer credential material", result.stderr)
        self.assertNotIn(secret, result.stderr)

    def test_rejects_duplicate_manifest_field(self) -> None:
        encoded = json.dumps(self.payload)
        duplicate = encoded.replace(
            '"schemaVersion": "synara.capacity-soak-evidence.v1",',
            '"schemaVersion": "synara.capacity-soak-evidence.v1", '
            '"schemaVersion": "synara.capacity-soak-evidence.v1",',
            1,
        )
        self.manifest.write_text(duplicate, encoding="utf-8")
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("duplicate JSON field", result.stderr)


if __name__ == "__main__":
    unittest.main()
