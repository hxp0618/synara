from __future__ import annotations

import dataclasses
import datetime as dt
import json
import pathlib
import subprocess
import tempfile
import unittest
from typing import Any
from unittest import mock

import acceptance_runner as acceptance
import registry_supply_chain as supply


REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
PLATFORM = "linux/amd64"
DIGEST = "sha256:" + "a" * 64
REFERENCE = f"localhost:55091/synara/worker@{DIGEST}"
NOW = dt.datetime(2026, 7, 17, tzinfo=dt.timezone.utc)


def policy(
    *,
    exceptions: tuple[supply.VulnerabilityException, ...] = (),
    ignore_unfixed: bool = False,
) -> supply.VulnerabilityPolicy:
    return supply.VulnerabilityPolicy(
        blocked_severities=("CRITICAL",),
        ignore_unfixed=ignore_unfixed,
        fail_on_end_of_life_os=True,
        maximum_database_age_hours=24,
        exceptions=exceptions,
        sha256="b" * 64,
    )


def vulnerability(
    *,
    vulnerability_id: str = "CVE-2026-12345",
    package: str = "openssl",
    severity: str = "CRITICAL",
    fixed_version: str = "3.0.2",
) -> dict[str, Any]:
    return {
        "VulnerabilityID": vulnerability_id,
        "PkgName": package,
        "InstalledVersion": "3.0.1",
        "FixedVersion": fixed_version,
        "Severity": severity,
        "Status": "fixed" if fixed_version else "affected",
        "PrimaryURL": f"https://example.test/{vulnerability_id}",
    }


def trivy_report(
    *,
    vulnerabilities: list[dict[str, Any]] | None = None,
    secrets: list[dict[str, Any]] | None = None,
    eol: bool = False,
) -> dict[str, Any]:
    return {
        "SchemaVersion": 2,
        "ArtifactName": REFERENCE,
        "ArtifactType": "container_image",
        "Metadata": {
            "ImageID": "sha256:" + "c" * 64,
            "RepoDigests": [REFERENCE],
            "OS": {"Family": "alpine", "Name": "3.23", "EOSL": eol},
        },
        "Results": [
            {
                "Target": "alpine",
                "Vulnerabilities": vulnerabilities or [],
                "Secrets": secrets or [],
            }
        ],
    }


class ConfigurationTest(unittest.TestCase):
    def test_checked_in_tool_and_policy_locks_are_valid(self) -> None:
        configuration = supply.load_configuration(REPO_ROOT)

        self.assertIn(":v3.1.1@sha256:", configuration.tools.cosign)
        self.assertIn(":0.72.0@sha256:", configuration.tools.trivy)
        self.assertEqual(configuration.vulnerability_policy.blocked_severities, ("CRITICAL",))
        self.assertFalse(configuration.vulnerability_policy.ignore_unfixed)
        self.assertEqual(configuration.vulnerability_policy.exceptions, ())

    def test_rejects_mutable_or_incomplete_tool_lock(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            repo_root = pathlib.Path(directory)
            lock_path = repo_root / supply.TOOLS_LOCK_PATH
            lock_path.parent.mkdir(parents=True)
            lock_path.write_text(
                "cosign=gcr.io/projectsigstore/cosign:v3.1.1\n"
                "trivy=ghcr.io/aquasecurity/trivy:0.72.0@sha256:" + "a" * 64 + "\n",
                encoding="utf-8",
            )
            with self.assertRaises(supply.common.ReleaseGateError) as caught:
                supply.load_tool_images(repo_root)

        self.assertEqual(caught.exception.code, "release.registry_supply_chain_source_invalid")

    def test_rejects_unknown_policy_fields_and_duplicate_exceptions(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            repo_root = pathlib.Path(directory)
            policy_path = repo_root / supply.VULNERABILITY_POLICY_PATH
            policy_path.parent.mkdir(parents=True)
            payload = {
                "schemaVersion": 1,
                "blockedSeverities": ["CRITICAL"],
                "ignoreUnfixed": False,
                "failOnEndOfLifeOS": True,
                "maximumDatabaseAgeHours": 24,
                "exceptions": [],
                "unexpected": True,
            }
            policy_path.write_text(json.dumps(payload), encoding="utf-8")
            with self.assertRaises(supply.common.ReleaseGateError) as caught:
                supply.load_vulnerability_policy(repo_root)

        self.assertEqual(caught.exception.code, "release.registry_supply_chain_policy_invalid")


class CommandBoundaryTest(unittest.TestCase):
    def test_secret_environment_value_is_not_written_to_docker_arguments(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            state_dir = pathlib.Path(directory) / "state"
            options = supply.SupplyChainOptions(
                repo_root=REPO_ROOT,
                state_dir=state_dir,
                image_repository="localhost:55091/synara/worker",
                docker_bin="docker",
                timeout_seconds=60.0,
                insecure_registry=True,
            )
            redactor = acceptance.SecretRedactor()
            redactor.add("secret-value")
            completed = subprocess.CompletedProcess(["docker"], 0, stdout="ok", stderr="")
            with mock.patch.object(supply.subprocess, "run", return_value=completed) as run:
                supply._run_tool(
                    options,
                    image="example.test/tool:v1.0.0@sha256:" + "d" * 64,
                    arguments=["version"],
                    tool="probe",
                    deadline=supply.time.monotonic() + 60,
                    redactor=redactor,
                    secret_environment={"COSIGN_PASSWORD": "secret-value"},
                )

        command = run.call_args.args[0]
        environment = run.call_args.kwargs["env"]
        self.assertNotIn("secret-value", command)
        self.assertIn("COSIGN_PASSWORD", command)
        self.assertEqual(environment["COSIGN_PASSWORD"], "secret-value")
        self.assertIn("no-new-privileges", command)
        self.assertIn("ALL", command)


class SignatureVerificationTest(unittest.TestCase):
    def verification_payload(self, *, digest: str = DIGEST) -> list[dict[str, Any]]:
        return [
            {
                "critical": {
                    "identity": {"docker-reference": REFERENCE},
                    "image": {"docker-manifest-digest": digest},
                    "type": supply.COSIGN_CLAIM_TYPE,
                },
                "optional": {
                    "synara.git-sha": "a" * 40,
                    "synara.run-id": "run-1",
                    "synara.slot": "cached",
                    "synara.version": "0.5.4",
                },
            }
        ]

    def test_accepts_exact_digest_identity_and_annotations(self) -> None:
        evidence = supply.validate_cosign_verification(
            self.verification_payload(),
            reference=REFERENCE,
            digest=DIGEST,
            annotations={
                "synara.git-sha": "a" * 40,
                "synara.run-id": "run-1",
                "synara.slot": "cached",
                "synara.version": "0.5.4",
            },
        )

        self.assertEqual(evidence["verifiedSignatureCount"], 1)
        self.assertRegex(evidence["verificationPayloadSha256"], r"^[0-9a-f]{64}$")

    def test_rejects_wrong_digest_or_missing_annotation(self) -> None:
        with self.assertRaises(supply.common.ReleaseGateError) as caught:
            supply.validate_cosign_verification(
                self.verification_payload(digest="sha256:" + "e" * 64),
                reference=REFERENCE,
                digest=DIGEST,
                annotations={"synara.slot": "cached"},
            )

        self.assertEqual(caught.exception.code, "release.registry_signature_verification_invalid")


class VulnerabilityPolicyTest(unittest.TestCase):
    def test_accepts_supported_os_without_blocked_vulnerabilities_or_secrets(self) -> None:
        evidence, errors, used = supply.evaluate_trivy_report(
            trivy_report(vulnerabilities=[vulnerability(severity="HIGH")]),
            platform=PLATFORM,
            reference=REFERENCE,
            policy=policy(),
            now=NOW,
        )

        self.assertEqual(errors, [])
        self.assertEqual(used, set())
        self.assertEqual(evidence["vulnerabilities"]["bySeverity"]["HIGH"], 1)
        self.assertEqual(evidence["blockedFindings"], [])

    def test_blocks_critical_eol_and_secret_findings_without_persisting_secret_value(self) -> None:
        evidence, errors, _used = supply.evaluate_trivy_report(
            trivy_report(
                vulnerabilities=[vulnerability()],
                secrets=[
                    {
                        "RuleID": "private-key",
                        "Category": "secret",
                        "Title": "Private Key",
                        "Target": "layer.tar",
                        "StartLine": 10,
                        "EndLine": 12,
                        "Match": "do-not-persist-this-value",
                    }
                ],
                eol=True,
            ),
            platform=PLATFORM,
            reference=REFERENCE,
            policy=policy(),
            now=NOW,
        )

        self.assertEqual(
            {error["code"] for error in errors},
            {
                "release.registry_vulnerability_os_eol",
                "release.registry_image_secret_detected",
                "release.registry_vulnerability_policy_blocked",
            },
        )
        self.assertEqual(evidence["secretFindingCount"], 1)
        serialized = json.dumps(errors)
        self.assertNotIn("do-not-persist-this-value", serialized)

    def test_active_exception_waives_exact_platform_package_and_expired_one_fails(self) -> None:
        active = supply.VulnerabilityException(
            vulnerability_id="CVE-2026-12345",
            package="openssl",
            platform=PLATFORM,
            expires_at=NOW + dt.timedelta(days=7),
            owner="security@example.test",
            reason="Temporary exception while the fixed base image is deployed.",
        )
        evidence, errors, used = supply.evaluate_trivy_report(
            trivy_report(vulnerabilities=[vulnerability()]),
            platform=PLATFORM,
            reference=REFERENCE,
            policy=policy(exceptions=(active,)),
            now=NOW,
        )

        self.assertEqual(errors, [])
        self.assertEqual(used, {active.identity})
        self.assertEqual(len(evidence["waivedFindings"]), 1)

        expired = dataclasses.replace(active, expires_at=NOW - dt.timedelta(seconds=1))
        _evidence, expired_errors, _used = supply.evaluate_trivy_report(
            trivy_report(vulnerabilities=[vulnerability()]),
            platform=PLATFORM,
            reference=REFERENCE,
            policy=policy(exceptions=(expired,)),
            now=NOW,
        )
        self.assertEqual(
            {error["code"] for error in expired_errors},
            {
                "release.registry_vulnerability_exception_expired",
                "release.registry_vulnerability_policy_blocked",
            },
        )

    def test_ignore_unfixed_excludes_only_unfixed_blocked_findings(self) -> None:
        evidence, errors, _used = supply.evaluate_trivy_report(
            trivy_report(vulnerabilities=[vulnerability(fixed_version="")]),
            platform=PLATFORM,
            reference=REFERENCE,
            policy=policy(ignore_unfixed=True),
            now=NOW,
        )

        self.assertEqual(errors, [])
        self.assertEqual(evidence["vulnerabilities"]["unfixed"], 1)
        self.assertEqual(evidence["blockedFindings"], [])

    def test_database_freshness_is_enforced(self) -> None:
        payload = {
            "Version": "0.72.0",
            "VulnerabilityDB": {
                "Version": 2,
                "UpdatedAt": (NOW - dt.timedelta(hours=2)).isoformat().replace("+00:00", "Z"),
                "NextUpdate": (NOW + dt.timedelta(hours=4)).isoformat().replace("+00:00", "Z"),
                "DownloadedAt": NOW.isoformat().replace("+00:00", "Z"),
            },
        }
        evidence, errors = supply.evaluate_trivy_database(
            payload,
            expected_version="0.72.0",
            policy=policy(),
            now=NOW,
        )
        self.assertEqual(errors, [])
        self.assertEqual(evidence["ageSeconds"], 7200)

        payload["VulnerabilityDB"]["UpdatedAt"] = (
            NOW - dt.timedelta(hours=25)
        ).isoformat().replace("+00:00", "Z")
        _evidence, stale_errors = supply.evaluate_trivy_database(
            payload,
            expected_version="0.72.0",
            policy=policy(),
            now=NOW,
        )
        self.assertEqual(
            {error["code"] for error in stale_errors},
            {"release.registry_vulnerability_database_stale"},
        )


if __name__ == "__main__":
    unittest.main()
