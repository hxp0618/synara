from __future__ import annotations

import hashlib
import json
import pathlib
import stat
import subprocess
import sys
import tempfile
import unittest


SCRIPT = pathlib.Path(__file__).with_name("validate_penetration_evidence.py")


class ValidatePenetrationEvidenceTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temporary.name)
        self.evidence = self.root / "evidence"
        self.evidence.mkdir()
        self.references: dict[str, dict[str, str]] = {}
        for field in (
            "stage5CompletionEvidence",
            "targetRevalidationEvidence",
            "scopeStatementEvidence",
            "assessorIndependenceEvidence",
            "executionEvidence",
            "finalReportEvidence",
            "findingRegisterEvidence",
            "remediationRetestEvidence",
            "riskAcceptanceEvidence",
        ):
            path = self.evidence / f"{field}.json"
            path.write_text(f"{field} proof\n", encoding="utf-8")
            self.references[field] = {
                "path": path.name,
                "sha256": "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest(),
            }
        self.payload = {
            "schemaVersion": "synara.third-party-penetration-evidence.v1",
            "engagementId": "1dbad749-d05f-43fa-9971-766478435c98",
            "releaseCommit": "c" * 40,
            "environmentClass": "production-like",
            "environmentId": "prodlike/security-1",
            "deploymentProfile": "self-managed-kubernetes",
            "startedAt": "2026-07-30T00:00:00Z",
            "completedAt": "2026-07-30T03:00:00Z",
            "reportIssuedAt": "2026-07-30T04:00:00Z",
            "assessor": {
                "organizationReference": "assessor/vendor-1",
                "engagementReference": "contract/pen-2026-01",
                "thirdParty": True,
                "independent": True,
                "noConflictDeclared": True,
            },
            "stage5Dependency": {
                "status": "accepted-current-supported-surface",
                "acceptedCommit": "c" * 40,
                "scopeProfile": "self-managed-kubernetes",
                "targetRevalidated": True,
            },
            "assets": [
                {
                    "assetType": asset_type,
                    "assetId": f"release/{asset_type}",
                    "artifactDigest": "sha256:" + str(index + 1) * 64,
                    "tested": True,
                }
                for index, asset_type in enumerate(
                    ("web", "control-plane-api", "worker-runtime", "provider-host")
                )
            ],
            "scopeCoverage": {
                name: {"tested": True, "result": "no-finding"}
                for name in (
                    "crossTenantAuthorization",
                    "ssrf",
                    "commandInjection",
                    "pathTraversal",
                    "supplyChain",
                    "containerEscape",
                )
            },
            "methodologies": [
                "manual-business-logic",
                "owasp-web-api",
                "cloud-runtime",
            ],
            "findings": [],
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
            "2026-07-30T05:00:00Z",
        ]

    def run_validator(self) -> subprocess.CompletedProcess[str]:
        self.write_manifest()
        return subprocess.run(self.command(), check=False, capture_output=True, text=True)

    def receipt(self) -> dict[str, object]:
        return json.loads((self.root / "receipt.json").read_text(encoding="utf-8"))

    def test_validates_complete_bundle_without_declaring_penetration_passed(self) -> None:
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertEqual(receipt["assessment"], "evidence-validated-not-penetration-passed")
        self.assertTrue(receipt["eligibleForHumanGateReview"])
        self.assertTrue(receipt["declaredNoUnacceptedHighOrCriticalFindings"])
        self.assertTrue((self.root / "receipt.json.sha256").is_file())
        self.assertEqual(stat.S_IMODE((self.root / "receipt.json").stat().st_mode), 0o600)
        self.assertEqual(
            stat.S_IMODE((self.root / "receipt.json.sha256").stat().st_mode), 0o600
        )

    def test_records_open_high_finding_as_a_failed_gate_without_discarding_report(self) -> None:
        self.payload["findings"] = [
            {
                "findingId": "PEN-001",
                "severity": "high",
                "status": "remediation-in-progress",
                "affectedAssetTypes": ["control-plane-api"],
                "discoveredAt": "2026-07-30T01:00:00Z",
                "lastReviewedAt": "2026-07-30T02:00:00Z",
                "retestPassed": False,
                "riskAcceptance": None,
            }
        ]
        self.payload["scopeCoverage"]["crossTenantAuthorization"]["result"] = "findings-recorded"
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertFalse(receipt["declaredNoUnacceptedHighOrCriticalFindings"])
        self.assertFalse(receipt["eligibleForHumanGateReview"])
        self.assertEqual(receipt["findingCounts"]["bySeverity"]["high"], 1)

    def test_accepts_time_bounded_dual_approval_for_a_high_finding(self) -> None:
        self.payload["findings"] = [
            {
                "findingId": "PEN-002",
                "severity": "high",
                "status": "risk-accepted",
                "affectedAssetTypes": ["provider-host"],
                "discoveredAt": "2026-07-30T00:30:00Z",
                "lastReviewedAt": "2026-07-30T01:00:00Z",
                "retestPassed": False,
                "riskAcceptance": {
                    "acceptanceId": "risk/PEN-002",
                    "securityApprover": "actor/security-lead",
                    "businessApprover": "actor/product-owner",
                    "approvedAt": "2026-07-30T02:00:00Z",
                    "expiresAt": "2026-08-15T02:00:00Z",
                    "compensatingControlIds": ["control/egress-deny"],
                },
            }
        ]
        self.payload["scopeCoverage"]["ssrf"]["result"] = "findings-recorded"
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertTrue(receipt["declaredNoUnacceptedHighOrCriticalFindings"])
        self.assertTrue(receipt["eligibleForHumanGateReview"])

    def test_internal_or_incomplete_runs_remain_valid_but_ineligible(self) -> None:
        self.payload["assessor"]["thirdParty"] = False
        self.payload["scopeCoverage"]["containerEscape"] = {
            "tested": False,
            "result": "not-tested",
        }
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertFalse(receipt["thirdPartyIndependenceDeclared"])
        self.assertFalse(receipt["scopeCoverageComplete"])
        self.assertFalse(receipt["eligibleForHumanGateReview"])

    def test_rejects_schema_that_omits_a_required_attack_area(self) -> None:
        del self.payload["scopeCoverage"]["ssrf"]
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("every required attack area", result.stderr)

    def test_rejects_tampered_or_reused_evidence(self) -> None:
        (self.evidence / "finalReportEvidence.json").write_text("tampered\n", encoding="utf-8")
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("sha256 does not match", result.stderr)

        self.references["finalReportEvidence"] = self.references["executionEvidence"]
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("duplicates another evidence file", result.stderr)

    def test_rejects_secret_evidence_without_echoing_secret(self) -> None:
        secret = "Authorization: Bearer penetration-validator-secret"
        path = self.evidence / "finalReportEvidence.json"
        path.write_text(secret + "\n", encoding="utf-8")
        self.references["finalReportEvidence"] = {
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
            '  "schemaVersion": "synara.third-party-penetration-evidence.v1",',
            '  "schemaVersion": "synara.third-party-penetration-evidence.v1",\n'
            '  "schemaVersion": "synara.third-party-penetration-evidence.v1",',
            1,
        )
        self.manifest.write_text(duplicated, encoding="utf-8")
        result = subprocess.run(
            self.command(), check=False, capture_output=True, text=True
        )
        self.assertEqual(result.returncode, 2)
        self.assertIn("duplicate JSON field", result.stderr)


if __name__ == "__main__":
    unittest.main()
