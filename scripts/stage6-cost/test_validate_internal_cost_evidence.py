from __future__ import annotations

import copy
import hashlib
import importlib.util
import json
import pathlib
import tempfile
import unittest

MODULE_PATH = pathlib.Path(__file__).with_name("validate_internal_cost_evidence.py")
SPEC = importlib.util.spec_from_file_location("validate_internal_cost_evidence", MODULE_PATH)
assert SPEC and SPEC.loader
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class InternalCostEvidenceTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.root = pathlib.Path(self.temporary.name)
        self.evidence_root = self.root / "evidence"
        self.evidence_root.mkdir()
        evidence = []
        for evidence_id in sorted(MODULE.EVIDENCE_IDS):
            path = self.evidence_root / f"{evidence_id}.json"
            data = (json.dumps({"evidence": evidence_id}, sort_keys=True) + "\n").encode()
            path.write_bytes(data)
            evidence.append(
                {
                    "id": evidence_id,
                    "path": path.name,
                    "sha256": "sha256:" + hashlib.sha256(data).hexdigest(),
                }
            )
        self.manifest = {
            "schemaVersion": MODULE.MANIFEST_SCHEMA,
            "candidate": {
                "candidateId": "v0.6.3-stage6.internal-cost",
                "sourceCommit": "a" * 40,
                "environment": "production-like",
                "environmentId": "stage6-internal-cost-01",
                "controlPlaneBaseUrl": "https://control.example.test/v1",
                "migrationTail": {
                    "name": "000152_stage6_internal_cost_governance.sql",
                    "sha256": "sha256:" + "b" * 64,
                },
            },
            "period": {"start": "2026-07-01T00:00:00Z", "end": "2026-08-01T00:00:00Z"},
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
            "evidence": evidence,
        }

    def write_manifest(self, manifest: dict | None = None) -> pathlib.Path:
        path = self.root / "manifest.json"
        path.write_text(json.dumps(manifest or self.manifest), encoding="utf-8")
        return path

    def test_validates_internal_usage_cost_and_token_receipt(self) -> None:
        receipt = MODULE.validate(self.write_manifest(), self.evidence_root)
        self.assertEqual(receipt["schemaVersion"], MODULE.RECEIPT_SCHEMA)
        self.assertEqual(receipt["assessment"], MODULE.ASSESSMENT)
        self.assertEqual(receipt["costs"]["knownByCurrency"], {"CNY": 3200, "USD": 24000})
        self.assertEqual(receipt["evidenceFileCount"], 4)
        self.assertTrue(receipt["eligibleForHumanGateReview"])

    def test_rejects_provider_coverage_drift(self) -> None:
        manifest = copy.deepcopy(self.manifest)
        manifest["usage"]["providerCostUnavailableExecutionCount"] = 0
        with self.assertRaisesRegex(MODULE.CostEvidenceError, "classify every execution"):
            MODULE.validate(self.write_manifest(manifest), self.evidence_root)

    def test_rejects_platform_double_counting(self) -> None:
        manifest = copy.deepcopy(self.manifest)
        manifest["usage"]["actualPlatformAllocationCount"] = 2
        with self.assertRaisesRegex(MODULE.CostEvidenceError, "classify every execution"):
            MODULE.validate(self.write_manifest(manifest), self.evidence_root)

    def test_rejects_known_cost_arithmetic_drift(self) -> None:
        manifest = copy.deepcopy(self.manifest)
        manifest["costs"]["knownByCurrency"]["USD"] = 23999
        with self.assertRaisesRegex(MODULE.CostEvidenceError, "exactly equal"):
            MODULE.validate(self.write_manifest(manifest), self.evidence_root)

    def test_rejects_payment_semantics(self) -> None:
        manifest = copy.deepcopy(self.manifest)
        manifest["candidate"]["stripeMode"] = "disabled"
        with self.assertRaisesRegex(MODULE.CostEvidenceError, "forbidden payment semantics"):
            MODULE.validate(self.write_manifest(manifest), self.evidence_root)

    def test_rejects_control_plane_url_query(self) -> None:
        manifest = copy.deepcopy(self.manifest)
        manifest["candidate"]["controlPlaneBaseUrl"] = (
            "https://control.example.test/v1?access=not-allowed"
        )
        with self.assertRaisesRegex(MODULE.CostEvidenceError, "credential-free HTTPS URL"):
            MODULE.validate(self.write_manifest(manifest), self.evidence_root)

    def test_rejects_evidence_digest_drift(self) -> None:
        manifest = copy.deepcopy(self.manifest)
        manifest["evidence"][0]["sha256"] = "sha256:" + "c" * 64
        with self.assertRaisesRegex(MODULE.CostEvidenceError, "digest mismatch"):
            MODULE.validate(self.write_manifest(manifest), self.evidence_root)

    def test_rejects_secret_material_without_echoing_it(self) -> None:
        manifest = copy.deepcopy(self.manifest)
        row = manifest["evidence"][0]
        secret = "Authorization: Bearer internal-cost-direct-secret"
        evidence_path = self.evidence_root / row["path"]
        data = (secret + "\n").encode("utf-8")
        evidence_path.write_bytes(data)
        row["sha256"] = "sha256:" + hashlib.sha256(data).hexdigest()
        with self.assertRaisesRegex(
            MODULE.CostEvidenceError,
            "prohibited bearer credential material",
        ) as caught:
            MODULE.validate(self.write_manifest(manifest), self.evidence_root)
        self.assertNotIn(secret, str(caught.exception))

    def test_rejects_symlinked_evidence(self) -> None:
        manifest = copy.deepcopy(self.manifest)
        row = manifest["evidence"][0]
        target = self.evidence_root / row["path"]
        link = self.evidence_root / "linked-evidence.json"
        link.symlink_to(target.name)
        row["path"] = link.name
        with self.assertRaisesRegex(MODULE.CostEvidenceError, "must not traverse a symlink"):
            MODULE.validate(self.write_manifest(manifest), self.evidence_root)

    def test_rejects_duplicate_manifest_fields(self) -> None:
        path = self.write_manifest()
        encoded = path.read_text(encoding="utf-8")
        marker = f'"schemaVersion": "{MODULE.MANIFEST_SCHEMA}"'
        path.write_text(
            encoded.replace(marker, f'{marker}, "schemaVersion": "{MODULE.MANIFEST_SCHEMA}"', 1),
            encoding="utf-8",
        )
        with self.assertRaisesRegex(MODULE.CostEvidenceError, "duplicate JSON field"):
            MODULE.validate(path, self.evidence_root)


if __name__ == "__main__":
    unittest.main()
