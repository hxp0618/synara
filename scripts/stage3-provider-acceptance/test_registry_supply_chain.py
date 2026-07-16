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


def signing_policy(
    *,
    mode: str = "ephemeral-key",
    require_transparency_log: bool = False,
    key_reference: str | None = None,
    credential_environment: tuple[str, ...] = (),
    identity_token_environment: str | None = None,
    certificate_identity: str | None = None,
    certificate_identity_regexp: str | None = None,
    certificate_oidc_issuer: str | None = None,
    certificate_oidc_issuer_regexp: str | None = None,
) -> supply.SigningPolicy:
    return supply.SigningPolicy(
        mode=mode,
        require_transparency_log=require_transparency_log,
        key_reference=key_reference,
        credential_environment=credential_environment,
        identity_token_environment=identity_token_environment,
        certificate_identity=certificate_identity,
        certificate_identity_regexp=certificate_identity_regexp,
        certificate_oidc_issuer=certificate_oidc_issuer,
        certificate_oidc_issuer_regexp=certificate_oidc_issuer_regexp,
        sha256="c" * 64,
    )


def signing_payload(**overrides: Any) -> dict[str, Any]:
    payload: dict[str, Any] = {
        "schemaVersion": 1,
        "mode": "ephemeral-key",
        "requireTransparencyLog": False,
        "keyReference": None,
        "credentialEnvironment": [],
        "identityTokenEnvironment": None,
        "certificateIdentity": None,
        "certificateIdentityRegexp": None,
        "certificateOidcIssuer": None,
        "certificateOidcIssuerRegexp": None,
    }
    payload.update(overrides)
    return payload


def verification_payload(
    *,
    reference: str = REFERENCE,
    digest: str = DIGEST,
) -> list[dict[str, Any]]:
    return [
        {
            "critical": {
                "identity": {"docker-reference": reference},
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


def configuration_with(policy: supply.SigningPolicy) -> supply.SupplyChainConfiguration:
    return dataclasses.replace(supply.load_configuration(REPO_ROOT), signing_policy=policy)


def supply_options(
    state_dir: pathlib.Path,
    *,
    insecure_registry: bool = False,
) -> supply.SupplyChainOptions:
    return supply.SupplyChainOptions(
        repo_root=REPO_ROOT,
        state_dir=state_dir,
        image_repository="registry.example.test/synara/worker",
        docker_bin="docker",
        timeout_seconds=60.0,
        insecure_registry=insecure_registry,
    )


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
        self.assertEqual(configuration.signing_policy.mode, "ephemeral-key")
        self.assertFalse(configuration.signing_policy.production_policy)
        self.assertFalse(configuration.signing_policy.require_transparency_log)
        self.assertEqual(
            configuration.vulnerability_policy.blocked_severities,
            ("HIGH", "CRITICAL"),
        )
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

    def test_accepts_keyless_and_kms_production_signing_policies(self) -> None:
        policies = [
            signing_payload(
                mode="keyless",
                requireTransparencyLog=True,
                identityTokenEnvironment="SYNARA_COSIGN_IDENTITY_TOKEN",
                certificateIdentity="https://github.com/example/synara/.github/workflows/release.yml@refs/tags/v1",
                certificateOidcIssuer="https://token.actions.githubusercontent.com",
            ),
            signing_payload(
                mode="keyless",
                requireTransparencyLog=True,
                identityTokenEnvironment="SYNARA_COSIGN_IDENTITY_TOKEN",
                certificateIdentityRegexp=r"^https://github\.com/example/synara/.*$",
                certificateOidcIssuerRegexp=r"^https://token\.actions\.githubusercontent\.com$",
            ),
            signing_payload(
                mode="kms-key",
                requireTransparencyLog=True,
                keyReference="awskms:///arn:aws:kms:us-east-1:123456789012:key/key-id",
                credentialEnvironment=["AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"],
            ),
        ]
        for payload in policies:
            with self.subTest(mode=payload["mode"]), tempfile.TemporaryDirectory() as directory:
                repo_root = pathlib.Path(directory)
                policy_path = repo_root / supply.SIGNING_POLICY_PATH
                policy_path.parent.mkdir(parents=True)
                policy_path.write_text(json.dumps(payload), encoding="utf-8")
                policy = supply.load_signing_policy(repo_root)

                self.assertTrue(policy.production_policy)
                self.assertTrue(policy.require_transparency_log)
                report = policy.as_report()
                self.assertNotIn("identityTokenEnvironment", report)
                self.assertNotIn("credentialEnvironment", report)

    def test_rejects_signing_policy_mode_mismatches_and_unsafe_values(self) -> None:
        invalid = [
            signing_payload(requireTransparencyLog=True),
            signing_payload(
                mode="keyless",
                requireTransparencyLog=False,
                identityTokenEnvironment="SYNARA_COSIGN_IDENTITY_TOKEN",
                certificateIdentity="release@example.test",
                certificateOidcIssuer="https://issuer.example.test",
            ),
            signing_payload(
                mode="keyless",
                requireTransparencyLog=True,
                identityTokenEnvironment="lowercase-token",
                certificateIdentity="release@example.test",
                certificateOidcIssuer="https://issuer.example.test",
            ),
            signing_payload(
                mode="keyless",
                requireTransparencyLog=True,
                identityTokenEnvironment="SYNARA_COSIGN_IDENTITY",
                certificateIdentity="release@example.test",
                certificateOidcIssuer="https://issuer.example.test",
            ),
            signing_payload(
                mode="keyless",
                requireTransparencyLog=True,
                identityTokenEnvironment="SYNARA_COSIGN_IDENTITY_TOKEN",
                certificateIdentity="release@example.test",
                certificateIdentityRegexp=".*",
                certificateOidcIssuer="https://issuer.example.test",
            ),
            signing_payload(
                mode="keyless",
                requireTransparencyLog=True,
                identityTokenEnvironment="SYNARA_COSIGN_IDENTITY_TOKEN",
                certificateIdentityRegexp=r"https://github\.com/example/.*",
                certificateOidcIssuer="https://issuer.example.test",
            ),
            signing_payload(
                mode="keyless",
                requireTransparencyLog=True,
                identityTokenEnvironment="SYNARA_COSIGN_IDENTITY_TOKEN",
                certificateIdentityRegexp=r"(?=unsafe)",
                certificateOidcIssuer="https://issuer.example.test",
            ),
            signing_payload(
                mode="keyless",
                requireTransparencyLog=True,
                identityTokenEnvironment="SYNARA_COSIGN_IDENTITY_TOKEN",
                certificateIdentity="release@example.test",
                certificateOidcIssuer="http://issuer.example.test",
            ),
            signing_payload(
                mode="keyless",
                requireTransparencyLog=True,
                identityTokenEnvironment="SYNARA_COSIGN_IDENTITY_TOKEN",
                certificateIdentity="release@example.test",
                certificateOidcIssuerRegexp=r"^http://issuer\.example\.test$",
            ),
            signing_payload(
                mode="kms-key",
                requireTransparencyLog=True,
                keyReference="https://kms.example.test/key",
            ),
            {**signing_payload(), "unexpected": True},
        ]
        for payload in invalid:
            with self.subTest(payload=payload), tempfile.TemporaryDirectory() as directory:
                repo_root = pathlib.Path(directory)
                policy_path = repo_root / supply.SIGNING_POLICY_PATH
                policy_path.parent.mkdir(parents=True)
                policy_path.write_text(json.dumps(payload), encoding="utf-8")
                with self.assertRaises(supply.common.ReleaseGateError) as caught:
                    supply.load_signing_policy(repo_root)

                self.assertEqual(caught.exception.code, "release.registry_signing_policy_invalid")

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

    def test_retries_only_transient_trivy_database_download_failures(self) -> None:
        transient = supply.common.ReleaseGateError(
            "release.registry_supply_chain_command_failed",
            "Trivy failed.",
            {
                "tool": "trivy",
                "returnCode": 1,
                "outputExcerpt": (
                    "failed to download vulnerability DB: OCI artifact error: unexpected EOF"
                ),
            },
        )
        policy_failure = supply.common.ReleaseGateError(
            "release.registry_supply_chain_command_failed",
            "Trivy failed.",
            {"tool": "trivy", "returnCode": 1, "outputExcerpt": "policy failure"},
        )
        completed = subprocess.CompletedProcess(["docker"], 0, stdout="", stderr="")

        with tempfile.TemporaryDirectory() as directory:
            state_dir = pathlib.Path(directory) / "state"
            state_dir.mkdir()
            report_path = state_dir / "trivy-linux-amd64.json"
            report_path.write_text("partial", encoding="utf-8")
            options = supply_options(state_dir)
            configuration = supply.load_configuration(REPO_ROOT)
            redactor = acceptance.SecretRedactor()
            with (
                mock.patch.object(
                    supply,
                    "_run_tool",
                    side_effect=[transient, completed],
                ) as run,
                mock.patch.object(supply.time, "sleep") as sleep,
            ):
                retries = supply._run_trivy_scan(
                    options,
                    configuration,
                    arguments=["image", REFERENCE],
                    report_path=report_path,
                    deadline=supply.time.monotonic() + 60,
                    redactor=redactor,
                )

            self.assertEqual(retries, 1)
            self.assertEqual(run.call_count, 2)
            sleep.assert_called_once_with(supply.TRIVY_DATABASE_DOWNLOAD_RETRY_DELAY_SECONDS)
            self.assertFalse(report_path.exists())

            with mock.patch.object(supply, "_run_tool", side_effect=policy_failure) as run:
                with self.assertRaises(supply.common.ReleaseGateError) as caught:
                    supply._run_trivy_scan(
                        options,
                        configuration,
                        arguments=["image", REFERENCE],
                        report_path=report_path,
                        deadline=supply.time.monotonic() + 60,
                        redactor=redactor,
                    )

            self.assertIs(caught.exception, policy_failure)
            self.assertEqual(run.call_count, 1)


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


class ProductionSigningTest(unittest.TestCase):
    def test_keyless_signing_keeps_token_out_of_arguments_and_requires_identity_and_tlog(self) -> None:
        token = "header.payload.signature"
        policy = signing_policy(
            mode="keyless",
            require_transparency_log=True,
            identity_token_environment="SYNARA_TEST_COSIGN_IDENTITY_TOKEN",
            certificate_identity="https://github.com/example/synara/.github/workflows/release.yml@refs/tags/v1",
            certificate_oidc_issuer="https://token.actions.githubusercontent.com",
        )
        builds = [{"slot": "cached", "registryDigest": DIGEST}]
        reference = f"registry.example.test/synara/worker@{DIGEST}"
        calls: list[dict[str, Any]] = []
        with tempfile.TemporaryDirectory() as directory:
            state_dir = pathlib.Path(directory) / "state"

            def run_tool(_options: Any, **kwargs: Any) -> subprocess.CompletedProcess[str]:
                calls.append(kwargs)
                arguments = kwargs["arguments"]
                self.assertNotIn(token, arguments)
                if arguments[0] == "sign":
                    token_relative = pathlib.Path(arguments[arguments.index("--identity-token") + 1])
                    token_path = state_dir / token_relative
                    self.assertEqual(token_path.read_text(encoding="utf-8"), token)
                    self.assertEqual(token_path.stat().st_mode & 0o777, 0o600)
                stdout = json.dumps(verification_payload(reference=reference)) if arguments[0] == "verify" else ""
                return subprocess.CompletedProcess(arguments, 0, stdout=stdout, stderr="")

            with mock.patch.dict(
                supply.os.environ,
                {"SYNARA_TEST_COSIGN_IDENTITY_TOKEN": token},
            ), mock.patch.object(supply, "_run_tool", side_effect=run_tool):
                result = supply._sign_and_verify(
                    supply_options(state_dir),
                    configuration_with(policy),
                    builds=builds,
                    git_sha="a" * 40,
                    version="0.5.4",
                    run_id="run-1",
                    deadline=supply.time.monotonic() + 60,
                    redactor=acceptance.SecretRedactor(),
                )

            self.assertFalse((state_dir / "cosign/identity-token").exists())

        sign_arguments = calls[0]["arguments"]
        verify_arguments = calls[1]["arguments"]
        self.assertIn("--tlog-upload=true", sign_arguments)
        self.assertIn("--certificate-identity", verify_arguments)
        self.assertIn("--certificate-oidc-issuer", verify_arguments)
        self.assertNotIn("--insecure-ignore-tlog=true", verify_arguments)
        self.assertEqual(result["mode"], "keyless")
        self.assertTrue(result["productionSigningPolicySatisfied"])
        self.assertTrue(result["identityTokenRemoved"])

    def test_kms_signing_passes_only_named_environment_and_verifies_tlog(self) -> None:
        credential = "kms-secret-value"
        key_reference = "awskms:///arn:aws:kms:us-east-1:123456789012:key/key-id"
        policy = signing_policy(
            mode="kms-key",
            require_transparency_log=True,
            key_reference=key_reference,
            credential_environment=("SYNARA_TEST_KMS_CREDENTIAL",),
        )
        reference = f"registry.example.test/synara/worker@{DIGEST}"
        calls: list[dict[str, Any]] = []

        def run_tool(_options: Any, **kwargs: Any) -> subprocess.CompletedProcess[str]:
            calls.append(kwargs)
            arguments = kwargs["arguments"]
            self.assertNotIn(credential, arguments)
            self.assertEqual(kwargs["secret_environment"], {"SYNARA_TEST_KMS_CREDENTIAL": credential})
            stdout = json.dumps(verification_payload(reference=reference)) if arguments[0] == "verify" else ""
            return subprocess.CompletedProcess(arguments, 0, stdout=stdout, stderr="")

        with tempfile.TemporaryDirectory() as directory, mock.patch.dict(
            supply.os.environ,
            {"SYNARA_TEST_KMS_CREDENTIAL": credential},
        ), mock.patch.object(supply, "_run_tool", side_effect=run_tool):
            result = supply._sign_and_verify(
                supply_options(pathlib.Path(directory) / "state"),
                configuration_with(policy),
                builds=[{"slot": "cached", "registryDigest": DIGEST}],
                git_sha="a" * 40,
                version="0.5.4",
                run_id="run-1",
                deadline=supply.time.monotonic() + 60,
                redactor=acceptance.SecretRedactor(),
            )

        self.assertIn("--tlog-upload=true", calls[0]["arguments"])
        self.assertEqual(calls[0]["arguments"][calls[0]["arguments"].index("--key") + 1], key_reference)
        self.assertNotIn("--insecure-ignore-tlog=true", calls[1]["arguments"])
        self.assertEqual(result["mode"], "kms-key")
        self.assertTrue(result["productionSigningPolicySatisfied"])
        self.assertEqual(result["credentialEnvironmentCount"], 1)

    def test_keyless_identity_token_is_removed_when_cosign_fails(self) -> None:
        token = "header.payload.signature"
        policy = signing_policy(
            mode="keyless",
            require_transparency_log=True,
            identity_token_environment="SYNARA_TEST_COSIGN_IDENTITY_TOKEN",
            certificate_identity="release@example.test",
            certificate_oidc_issuer="https://issuer.example.test",
        )
        with tempfile.TemporaryDirectory() as directory:
            state_dir = pathlib.Path(directory) / "state"
            with mock.patch.dict(
                supply.os.environ,
                {"SYNARA_TEST_COSIGN_IDENTITY_TOKEN": token},
            ), mock.patch.object(
                supply,
                "_run_tool",
                side_effect=supply.common.ReleaseGateError(
                    "release.registry_supply_chain_command_failed",
                    "Cosign failed.",
                ),
            ), self.assertRaises(supply.common.ReleaseGateError):
                supply._sign_and_verify(
                    supply_options(state_dir),
                    configuration_with(policy),
                    builds=[{"slot": "cached", "registryDigest": DIGEST}],
                    git_sha="a" * 40,
                    version="0.5.4",
                    run_id="run-1",
                    deadline=supply.time.monotonic() + 60,
                    redactor=acceptance.SecretRedactor(),
                )

            self.assertFalse((state_dir / "cosign/identity-token").exists())

    def test_production_signing_rejects_insecure_registry_and_missing_credentials(self) -> None:
        keyless = signing_policy(
            mode="keyless",
            require_transparency_log=True,
            identity_token_environment="SYNARA_TEST_COSIGN_IDENTITY_TOKEN",
            certificate_identity="release@example.test",
            certificate_oidc_issuer="https://issuer.example.test",
        )
        with tempfile.TemporaryDirectory() as directory, self.assertRaises(
            supply.common.ReleaseGateError
        ) as insecure:
            supply._sign_and_verify(
                supply_options(pathlib.Path(directory) / "state", insecure_registry=True),
                configuration_with(keyless),
                builds=[{"slot": "cached", "registryDigest": DIGEST}],
                git_sha="a" * 40,
                version="0.5.4",
                run_id="run-1",
                deadline=supply.time.monotonic() + 60,
                redactor=acceptance.SecretRedactor(),
            )
        self.assertEqual(
            insecure.exception.code,
            "release.registry_production_signing_insecure_registry",
        )

        kms = signing_policy(
            mode="kms-key",
            require_transparency_log=True,
            key_reference="awskms:///arn:aws:kms:us-east-1:123456789012:key/key-id",
            credential_environment=("SYNARA_TEST_MISSING_KMS_CREDENTIAL",),
        )
        with tempfile.TemporaryDirectory() as directory, mock.patch.dict(
            supply.os.environ,
            {},
            clear=True,
        ), self.assertRaises(supply.common.ReleaseGateError) as missing:
            supply._sign_and_verify(
                supply_options(pathlib.Path(directory) / "state"),
                configuration_with(kms),
                builds=[{"slot": "cached", "registryDigest": DIGEST}],
                git_sha="a" * 40,
                version="0.5.4",
                run_id="run-1",
                deadline=supply.time.monotonic() + 60,
                redactor=acceptance.SecretRedactor(),
            )
        self.assertEqual(missing.exception.code, "release.registry_signing_credential_invalid")


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
        self.assertEqual(evidence["reviewFindingCount"], 1)
        self.assertEqual(evidence["reviewFindings"][0]["vulnerabilityId"], "CVE-2026-12345")
        self.assertEqual(evidence["reviewFindings"][0]["target"], "alpine")
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
