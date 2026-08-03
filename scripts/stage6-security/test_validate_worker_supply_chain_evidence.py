from __future__ import annotations

import copy
import hashlib
import importlib.util
import json
import pathlib
import stat
import subprocess
import sys
import tempfile
import unittest
from typing import Any, Callable


SCRIPT = pathlib.Path(__file__).with_name("validate_worker_supply_chain_evidence.py")
SPEC = importlib.util.spec_from_file_location("worker_supply_chain_validator", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
validator = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(validator)

COMMIT = "a" * 40
DIGEST = "sha256:" + "b" * 64
PLATFORM_DIGESTS = {
    "linux/amd64": "sha256:" + "c" * 64,
    "linux/arm64": "sha256:" + "d" * 64,
}


def release_manifest() -> dict[str, Any]:
    return {
        "schemaVersion": "synara-stage6-release-evidence-v2",
        "source": {"commit": COMMIT, "clean": True},
        "artifacts": {"workerImage": DIGEST},
        "assessment": "evidence-collected-not-control-passed",
    }


def build(slot: str, no_cache: bool) -> dict[str, Any]:
    return {
        "slot": slot,
        "noCache": no_cache,
        "registryDigest": DIGEST,
        "platformDigests": copy.deepcopy(PLATFORM_DIGESTS),
        "attestationPredicates": {
            platform: ["https://spdx.dev/Document", "https://slsa.dev/provenance/v1"]
            for platform in PLATFORM_DIGESTS
        },
    }


def registry_report() -> dict[str, Any]:
    return {
        "schemaVersion": "synara.worker-registry-release-gate.v2",
        "mode": "worker-registry-release-gate",
        "status": "pass",
        "configuration": {"signingPolicyProfile": "production"},
        "source": {
            "gitSha": COMMIT,
            "worktreeDirty": False,
            "supplyChain": {
                "signingPolicyProfile": "production",
                "signingPolicy": {
                    "mode": "kms-key",
                    "productionPolicy": True,
                    "sha256": "1" * 64,
                },
                "productionSigningProfile": {"sha256": "2" * 64},
            },
        },
        "builds": [build("cached", False), build("no-cache", True)],
        "supplyChain": {
            "status": "pass",
            "signing": {
                "mode": "kms-key",
                "productionSigningPolicySatisfied": True,
                "transparencyLogVerified": True,
                "transparencyLogInclusionProofPresent": True,
                "transparencyLogSignedEntryTimestampPresent": True,
                "signatures": [{"slot": "cached"}, {"slot": "no-cache"}],
            },
            "vulnerability": {
                "policy": {
                    "blockedSeverities": ["HIGH", "CRITICAL"],
                    "ignoreUnfixed": False,
                    "failOnEndOfLifeOS": True,
                    "maximumDatabaseAgeHours": 24,
                    "exceptionCount": 0,
                },
                "database": {"ageSeconds": 60, "maximumAgeHours": 24},
                "scans": [
                    {
                        "platform": platform,
                        "os": {"Family": "debian", "EOSL": False},
                        "blockedFindings": [],
                        "waivedFindings": [],
                        "secretFindingCount": 0,
                    }
                    for platform in PLATFORM_DIGESTS
                ],
                "staleExceptionCount": 0,
            },
            "cleanup": {
                "isolatedStateRemoved": True,
                "signingSecretStateRemoved": True,
                "broadCleanupUsed": False,
            },
            "errors": [],
        },
        "cleanup": {"isolatedStateRemoved": True, "broadCleanupUsed": False},
        "security": {"outputSecretScan": {"status": "pass", "findings": []}},
        "errors": [],
    }


def admission_report(registry_sha256: str) -> dict[str, Any]:
    return {
        "schemaVersion": "synara.vault-kms-admission-gate.v1",
        "mode": "vault-kms-admission-gate",
        "status": "pass",
        "source": {
            "gitSha": COMMIT,
            "registryReleaseGate": {
                "reportSha256": registry_sha256,
                "cachedSignedImage": f"registry.example.com/synara/worker@{DIGEST}",
                "transparencyLogVerified": True,
                "transparencyLogInclusionProofPresent": True,
                "transparencyLogSignedEntryTimestampPresent": True,
            },
        },
        "admission": {
            "probes": {
                "signed": {"status": "admitted"},
                "unsigned": {"status": "denied"},
                "wrong-key": {"status": "denied"},
                "tag-drift": {"status": "denied"},
            },
            "controllerProbes": {
                kind: {"status": "denied"}
                for kind in ("deployment", "statefulset", "job", "cronjob")
            },
        },
        "cleanup": {
            "isolatedStateRemoved": True,
            "exactOwnerCleanup": True,
            "broadCleanupUsed": False,
        },
        "security": {"outputSecretScan": {"status": "pass", "findings": []}},
        "errors": [],
    }


class ValidateWorkerSupplyChainEvidenceTest(unittest.TestCase):
    def write_bundle(
        self,
        *,
        mutate_release: Callable[[dict[str, Any]], None] | None = None,
        mutate_registry: Callable[[dict[str, Any]], None] | None = None,
        mutate_admission: Callable[[dict[str, Any]], None] | None = None,
    ) -> tuple[tempfile.TemporaryDirectory[str], pathlib.Path, pathlib.Path, pathlib.Path]:
        temporary = tempfile.TemporaryDirectory()
        root = pathlib.Path(temporary.name)
        release = release_manifest()
        registry = registry_report()
        if mutate_release:
            mutate_release(release)
        if mutate_registry:
            mutate_registry(registry)
        release_path = root / "release.json"
        registry_path = root / "registry.json"
        release_path.write_text(json.dumps(release, sort_keys=True) + "\n", encoding="utf-8")
        registry_path.write_text(json.dumps(registry, sort_keys=True) + "\n", encoding="utf-8")
        registry_sha = hashlib.sha256(registry_path.read_bytes()).hexdigest()
        admission = admission_report(registry_sha)
        if mutate_admission:
            mutate_admission(admission)
        admission_path = root / "admission.json"
        admission_path.write_text(json.dumps(admission, sort_keys=True) + "\n", encoding="utf-8")
        return temporary, release_path, registry_path, admission_path

    def validate(self, **kwargs: Any) -> dict[str, Any]:
        temporary, release, registry, admission = self.write_bundle(**kwargs)
        self.addCleanup(temporary.cleanup)
        return validator.validate_evidence(release, registry, admission)

    def assert_rejected(self, expected: str, **kwargs: Any) -> None:
        with self.assertRaisesRegex(validator.WorkerSupplyChainEvidenceError, expected):
            self.validate(**kwargs)

    def test_accepts_bound_production_evidence_and_cli_output(self) -> None:
        temporary, release, registry, admission = self.write_bundle()
        self.addCleanup(temporary.cleanup)
        output = pathlib.Path(temporary.name) / "receipt.json"
        result = subprocess.run(
            [
                sys.executable,
                str(SCRIPT),
                "--release-manifest",
                str(release),
                "--registry-report",
                str(registry),
                "--admission-report",
                str(admission),
                "--output",
                str(output),
                "--validated-at",
                "2026-08-01T00:00:00Z",
            ],
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = json.loads(output.read_text(encoding="utf-8"))
        self.assertEqual(receipt["source"]["workerImage"], DIGEST)
        self.assertEqual(receipt["controls"]["signedAndNegativeAdmissionProbes"], "pass")
        self.assertEqual(receipt["assessment"], "evidence-validated-not-worker-supply-chain-approved")
        self.assertEqual(receipt["validatedAt"], "2026-08-01T00:00:00Z")
        sidecar = output.with_suffix(".json.sha256")
        self.assertTrue(sidecar.is_file())
        self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(sidecar.stat().st_mode), 0o600)

    def test_rejects_worker_digest_mismatch(self) -> None:
        self.assert_rejected(
            "digest does not match",
            mutate_registry=lambda report: report["builds"][0].update({"registryDigest": "sha256:" + "e" * 64}),
        )

    def test_rejects_registry_report_hash_mismatch(self) -> None:
        self.assert_rejected(
            "exact registry report bytes",
            mutate_admission=lambda report: report["source"]["registryReleaseGate"].update({"reportSha256": "f" * 64}),
        )

    def test_rejects_non_production_signing(self) -> None:
        self.assert_rejected(
            "production KMS signing profile",
            mutate_registry=lambda report: report["configuration"].update({"signingPolicyProfile": "development"}),
        )

    def test_rejects_blocked_vulnerability(self) -> None:
        self.assert_rejected(
            "blocked, waived, or secret",
            mutate_registry=lambda report: report["supplyChain"]["vulnerability"]["scans"][0].update(
                {"blockedFindings": [{"vulnerabilityId": "CVE-test"}]}
            ),
        )

    def test_rejects_stale_database_eol_or_secret_finding(self) -> None:
        mutations = (
            lambda report: report["supplyChain"]["vulnerability"]["database"].update({"ageSeconds": 86401}),
            lambda report: report["supplyChain"]["vulnerability"]["scans"][0]["os"].update({"EOSL": True}),
            lambda report: report["supplyChain"]["vulnerability"]["scans"][0].update({"secretFindingCount": 1}),
        )
        for mutation in mutations:
            with self.subTest(mutation=mutation):
                with self.assertRaises(validator.WorkerSupplyChainEvidenceError):
                    self.validate(mutate_registry=mutation)

    def test_rejects_allowed_negative_admission_probe(self) -> None:
        self.assert_rejected(
            "unsigned probe must be denied",
            mutate_admission=lambda report: report["admission"]["probes"]["unsigned"].update({"status": "admitted"}),
        )

    def test_rejects_cleanup_failure_or_broad_cleanup(self) -> None:
        mutations = (
            lambda report: report["cleanup"].update({"isolatedStateRemoved": False}),
            lambda report: report["cleanup"].update({"broadCleanupUsed": True}),
        )
        for mutation in mutations:
            with self.subTest(mutation=mutation):
                self.assert_rejected("cleanup boundary", mutate_admission=mutation)

    def test_rejects_duplicate_json_field(self) -> None:
        temporary, release, registry, admission = self.write_bundle()
        self.addCleanup(temporary.cleanup)
        encoded = registry.read_text(encoding="utf-8")
        registry.write_text(
            encoded.replace(
                '"status": "pass"',
                '"status": "pass", "status": "pass"',
                1,
            ),
            encoding="utf-8",
        )
        with self.assertRaisesRegex(
            validator.WorkerSupplyChainEvidenceError, "duplicate field"
        ):
            validator.validate_evidence(release, registry, admission)

    def test_rejects_secret_input_without_echoing_secret(self) -> None:
        temporary, release, registry, admission = self.write_bundle()
        self.addCleanup(temporary.cleanup)
        secret = "Authorization: Bearer worker-supply-chain-secret"
        registry.write_text(
            registry.read_text(encoding="utf-8") + secret + "\n",
            encoding="utf-8",
        )
        result = subprocess.run(
            [
                sys.executable,
                str(SCRIPT),
                "--release-manifest",
                str(release),
                "--registry-report",
                str(registry),
                "--admission-report",
                str(admission),
            ],
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, 2)
        self.assertIn("prohibited bearer credential material", result.stderr)
        self.assertNotIn(secret, result.stderr)

    def test_rejects_symlinked_input(self) -> None:
        temporary, release, registry, admission = self.write_bundle()
        self.addCleanup(temporary.cleanup)
        linked_release = pathlib.Path(temporary.name) / "linked-release.json"
        linked_release.symlink_to(release.name)
        with self.assertRaisesRegex(
            validator.WorkerSupplyChainEvidenceError, "non-symlink file"
        ):
            validator.validate_evidence(linked_release, registry, admission)


if __name__ == "__main__":
    unittest.main()
