from __future__ import annotations

import hashlib
import json
import pathlib
import stat
import subprocess
import sys
import tempfile
import unittest


SCRIPT = pathlib.Path(__file__).with_name("validate_incident_exercise_evidence.py")


class ValidateIncidentExerciseEvidenceTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temporary.name)
        self.evidence = self.root / "evidence"
        self.evidence.mkdir()
        self.references: dict[str, dict[str, str]] = {}
        for field in (
            "onCallRotaEvidence",
            "pagingProviderEvidence",
            "internalStatusBoardTimelineEvidence",
            "employeeNotificationDeliveryEvidence",
            "internalProbeEvidence",
            "internalUserPathEvidence",
            "exerciseReviewEvidence",
        ):
            path = self.evidence / f"{field}.json"
            path.write_text(f"{field} proof\n", encoding="utf-8")
            self.references[field] = {
                "path": path.name,
                "sha256": "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest(),
            }
        self.payload = {
            "schemaVersion": "synara.incident-communication-exercise-evidence.v2",
            "exerciseId": "1a80e516-a331-4c81-8c8b-3ab3b16b9ac7",
            "releaseCommit": "f" * 40,
            "environmentClass": "production-like",
            "environmentId": "prodlike/incident-1",
            "exerciseMode": "live-internal-communication",
            "severity": "SEV-1",
            "serviceOrigin": "https://synara.example.test",
            "internalStatusBoardOrigin": "https://status.example.test",
            "internalStatusBoardOperationallyIndependent": True,
            "regionPromiseEnabled": False,
            "startedAt": "2026-07-30T00:00:00Z",
            "completedAt": "2026-07-30T02:00:00Z",
            "roles": {
                "incidentCommander": "actor/ic",
                "operationsLead": "actor/operations",
                "communicationsLead": "actor/comms",
                "scribe": "actor/scribe",
                "securityPrivacyLead": "actor/security",
                "releaseObserver": "actor/release",
            },
            "paging": {
                "providerReference": "paging/provider-1",
                "primaryResponder": "actor/primary",
                "secondaryResponder": "actor/secondary",
                "pageTriggeredAt": "2026-07-30T00:11:00Z",
                "acknowledgedAt": "2026-07-30T00:14:00Z",
                "acknowledgedBy": "actor/primary",
                "escalationExercised": True,
                "pagingSucceeded": True,
            },
            "internalStatusBoardComponents": {
                name: {"published": True, "regionDetailPublished": False}
                for name in (
                    "control-plane-api",
                    "authentication-sso",
                    "execution-scheduling",
                    "worker-runtime",
                    "artifact-service",
                    "web-application",
                )
            },
            "internalTimeline": {
                "impactConfirmedAt": "2026-07-30T00:10:00Z",
                "incidentOpenedAt": "2026-07-30T00:12:00Z",
                "updates": [
                    {"kind": "initial", "publishedAt": "2026-07-30T00:20:00Z"},
                    {"kind": "progress", "publishedAt": "2026-07-30T00:45:00Z"},
                    {"kind": "progress", "publishedAt": "2026-07-30T01:10:00Z"},
                    {"kind": "resolved", "publishedAt": "2026-07-30T01:40:00Z"},
                ],
                "internalHistoryVisible": True,
            },
            "employeeNotificationDelivery": {
                "channelReference": "employee/email-canary",
                "initialNotificationReceivedAt": "2026-07-30T00:21:00Z",
                "resolutionNotificationReceivedAt": "2026-07-30T01:41:00Z",
                "deliverySucceeded": True,
            },
            "recoveryVerification": {
                "recoveredAt": "2026-07-30T01:20:00Z",
                "internalProbePassed": True,
                "internalUserPathPassed": True,
                "cleanupCompleted": True,
            },
            "review": {
                "incidentTimelineConsistent": True,
                "correctiveActionsOwned": True,
                "sensitiveDataAbsent": True,
                "operationsApproved": True,
                "communicationsApproved": True,
            },
            "evidence": self.references,
        }
        self.manifest = self.root / "manifest.json"
        self.output = self.root / "receipt.json"
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
            str(self.output),
            "--validated-at",
            "2026-07-30T03:00:00Z",
        ]

    def run_validator(self) -> subprocess.CompletedProcess[str]:
        self.write_manifest()
        return subprocess.run(self.command(), check=False, capture_output=True, text=True)

    def receipt(self) -> dict[str, object]:
        return json.loads(self.output.read_text(encoding="utf-8"))

    def test_validates_live_exercise_without_declaring_operations_ready(self) -> None:
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertEqual(receipt["assessment"], "evidence-validated-not-operations-ready")
        self.assertTrue(receipt["eligibleForHumanGateReview"])
        self.assertTrue(receipt["pagingExerciseComplete"])
        self.assertTrue(receipt["internalTimelineWithinTargets"])
        self.assertTrue((self.root / "receipt.json.sha256").is_file())
        self.assertEqual(stat.S_IMODE(self.output.stat().st_mode), 0o600)
        self.assertEqual(
            stat.S_IMODE(self.output.with_suffix(".json.sha256").stat().st_mode), 0o600
        )

    def test_preserves_missed_ack_and_public_deadline_as_failed_exercise(self) -> None:
        self.payload["paging"]["acknowledgedAt"] = "2026-07-30T00:17:00Z"
        self.payload["internalTimeline"]["updates"][0]["publishedAt"] = "2026-07-30T00:30:00Z"
        self.payload["employeeNotificationDelivery"]["initialNotificationReceivedAt"] = "2026-07-30T00:31:00Z"
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertFalse(receipt["pagingExerciseComplete"])
        self.assertFalse(receipt["internalTimelineWithinTargets"])
        self.assertFalse(receipt["eligibleForHumanGateReview"])

    def test_tabletop_and_delivery_failure_remain_ineligible(self) -> None:
        self.payload["exerciseMode"] = "tabletop"
        self.payload["employeeNotificationDelivery"]["deliverySucceeded"] = False
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertFalse(receipt["employeeNotificationDeliveryComplete"])
        self.assertFalse(receipt["eligibleForHumanGateReview"])

    def test_region_promise_requires_component_region_detail(self) -> None:
        self.payload["regionPromiseEnabled"] = True
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertFalse(receipt["internalStatusBoardComponentsComplete"])

        for component in self.payload["internalStatusBoardComponents"].values():
            component["regionDetailPublished"] = True
        self.output = self.root / "receipt-with-region-detail.json"
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(self.receipt()["internalStatusBoardComponentsComplete"])

    def test_rejects_unordered_public_updates(self) -> None:
        self.payload["internalTimeline"]["updates"][1]["publishedAt"] = "2026-07-30T00:19:00Z"
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("updates are unordered", result.stderr)

    def test_rejects_tampered_or_reused_evidence(self) -> None:
        (self.evidence / "internalStatusBoardTimelineEvidence.json").write_text("tampered\n", encoding="utf-8")
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("sha256 does not match", result.stderr)

        self.references["internalStatusBoardTimelineEvidence"] = self.references["pagingProviderEvidence"]
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("duplicates another evidence file", result.stderr)

    def test_rejects_secret_material_without_echoing_it(self) -> None:
        reference = self.references["pagingProviderEvidence"]
        path = self.evidence / reference["path"]
        secret = "Authorization: Bearer incident-direct-secret"
        data = (secret + "\n").encode("utf-8")
        path.write_bytes(data)
        reference["sha256"] = "sha256:" + hashlib.sha256(data).hexdigest()
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("prohibited bearer credential material", result.stderr)
        self.assertNotIn(secret, result.stderr)

    def test_rejects_duplicate_manifest_fields(self) -> None:
        self.write_manifest()
        encoded = self.manifest.read_text(encoding="utf-8")
        marker = '"schemaVersion": "synara.incident-communication-exercise-evidence.v2",'
        self.manifest.write_text(
            encoded.replace(marker, f"{marker}\n  {marker}", 1),
            encoding="utf-8",
        )
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("duplicate JSON field", result.stderr)


if __name__ == "__main__":
    unittest.main()
