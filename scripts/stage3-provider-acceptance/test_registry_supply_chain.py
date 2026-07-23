from __future__ import annotations

import base64
import dataclasses
import datetime as dt
import hashlib
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
PUBLIC_KEY_PEM = """-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEQVkNH8kvecKHNfCGpmOfb4W8GXV5
PFhf/AZNZRPMI+AF7b5TN/EM/TCPLLU1+7sP/ye/sDw53VdCJ1QjiAZVYg==
-----END PUBLIC KEY-----
"""
PRODUCTION_PROFILE_REQUIRED_PATHS = (
    supply.TOOLS_LOCK_PATH,
    supply.VULNERABILITY_POLICY_PATH,
    supply.PRODUCTION_SIGNING_POLICY_PATH,
    supply.PRODUCTION_SIGNING_PROFILE_PATH,
    supply.SECURITY_PRODUCTION_KUSTOMIZATION_PATH / "kustomization.yaml",
    supply.SECURITY_CLUSTER_KUSTOMIZATION_PATH / "kustomization.yaml",
    supply.SECURITY_CLUSTER_POLICY_PATH,
    supply.SECURITY_NAMESPACE_KUSTOMIZATION_PATH / "kustomization.yaml",
    supply.SECURITY_PUBLIC_KEY_CONFIGMAP_PATH,
    supply.SECURITY_REPOSITORY_CONFIGMAP_PATH,
)


def signing_policy(
    *,
    path: str = str(supply.SIGNING_POLICY_PATH),
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
        path=path,
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
                "identity": {"docker-reference": reference.rpartition("@")[0]},
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


def transparency_bundle_payload(
    *,
    include_inclusion_proof: bool = True,
    include_signed_entry_timestamp: bool = True,
) -> dict[str, Any]:
    entry: dict[str, Any] = {
        "logIndex": 7,
        "integratedTime": 1_720_000_000,
    }
    if include_inclusion_proof:
        entry["inclusionProof"] = {
            "logIndex": 7,
            "rootHash": "ab" * 32,
            "treeSize": 8,
            "hashes": ["cd" * 32],
        }
    if include_signed_entry_timestamp:
        entry["inclusionPromise"] = {
            "signedEntryTimestamp": "c2lnbmVkLWVudHJ5LXRpbWVzdGFtcA==",
        }
    return {
        "mediaType": "application/vnd.dev.sigstore.bundle+json;version=0.3",
        "verificationMaterial": {
            "tlogEntries": [entry],
        },
    }


def vault_token_lookup_payload(
    *,
    display_name: str = supply.VAULT_TRANSIT_AUTH_METHOD,
    role_name: str = "synara-worker-release-signer",
    token_type: str = "batch",
    orphan: bool = True,
    policies: list[str] | None = None,
) -> dict[str, Any]:
    return {
        "data": {
            "display_name": display_name,
            "meta": {"role_name": role_name},
            "type": token_type,
            "orphan": orphan,
            "policies": policies if policies is not None else [role_name],
        }
    }


def write_transparency_bundle(
    path: pathlib.Path,
    *,
    include_inclusion_proof: bool = True,
    include_signed_entry_timestamp: bool = True,
) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        json.dumps(
            transparency_bundle_payload(
                include_inclusion_proof=include_inclusion_proof,
                include_signed_entry_timestamp=include_signed_entry_timestamp,
            )
        ),
        encoding="utf-8",
    )


def write_classic_signing_outputs(state_dir: pathlib.Path, arguments: list[str]) -> None:
    reference = arguments[-1]
    repository, separator, digest = reference.rpartition("@")
    if separator != "@":
        raise AssertionError("classic signing output requires a digest reference")
    annotations = {
        arguments[index + 1].split("=", 1)[0]: arguments[index + 1].split("=", 1)[1]
        for index, argument in enumerate(arguments[:-1])
        if argument == "-a"
    }
    payload = json.dumps(
        {
            "critical": {
                "identity": {"docker-reference": repository},
                "image": {"docker-manifest-digest": digest},
                "type": supply.COSIGN_CLAIM_TYPE,
            },
            "optional": annotations,
        },
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")
    outputs = {
        "--output-signature": b"c2lnbmF0dXJl\n",
        "--output-payload": payload,
        "--output-certificate": b"-----BEGIN CERTIFICATE-----\nfixture\n-----END CERTIFICATE-----\n",
    }
    for flag, content in outputs.items():
        if flag not in arguments:
            continue
        path = state_dir / pathlib.Path(arguments[arguments.index(flag) + 1])
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(content)


def write_bundle_create_output(
    state_dir: pathlib.Path,
    arguments: list[str],
    *,
    include_inclusion_proof: bool = True,
    include_signed_entry_timestamp: bool = True,
) -> None:
    if arguments[:2] != ["bundle", "create"]:
        raise AssertionError("expected cosign bundle create")
    artifact_path = state_dir / pathlib.Path(arguments[arguments.index("--artifact") + 1])
    signature_path = state_dir / pathlib.Path(arguments[arguments.index("--signature") + 1])
    for flag, path in (("--artifact", artifact_path), ("--signature", signature_path)):
        if not path.is_file():
            raise AssertionError(f"missing bundle input: {flag}")
    if "--certificate" in arguments:
        certificate_path = state_dir / pathlib.Path(
            arguments[arguments.index("--certificate") + 1]
        )
        base64.b64decode(certificate_path.read_bytes().strip(), validate=True)
    elif "--key" not in arguments:
        raise AssertionError("bundle create omitted its verifier")
    output = state_dir / pathlib.Path(arguments[arguments.index("--out") + 1])
    bundle = transparency_bundle_payload(
        include_inclusion_proof=include_inclusion_proof,
        include_signed_entry_timestamp=include_signed_entry_timestamp,
    )
    payload_bytes = artifact_path.read_bytes()
    signature = signature_path.read_text(encoding="utf-8").strip()
    base64.b64decode(signature, validate=True)
    bundle["messageSignature"] = {
        "messageDigest": {
            "algorithm": "SHA2_256",
            "digest": base64.b64encode(hashlib.sha256(payload_bytes).digest()).decode("ascii"),
        },
        "signature": signature,
    }
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(json.dumps(bundle), encoding="utf-8")


def configuration_with(policy: supply.SigningPolicy) -> supply.SupplyChainConfiguration:
    return dataclasses.replace(supply.load_configuration(REPO_ROOT), signing_policy=policy)


def production_configuration_with(policy: supply.SigningPolicy) -> supply.SupplyChainConfiguration:
    return dataclasses.replace(
        supply.load_configuration(REPO_ROOT, signing_policy_profile="production"),
        signing_policy=policy,
    )


def supply_options(
    state_dir: pathlib.Path,
    *,
    insecure_registry: bool = False,
    registry_auth_username_environment: str | None = None,
    registry_auth_password_environment: str | None = None,
    registry_ca_cert_environment: str | None = None,
    tool_proxy_url: str | None = None,
    production_public_key_configmap_path: pathlib.Path | None = None,
    production_repository_configmap_path: pathlib.Path | None = None,
) -> supply.SupplyChainOptions:
    return supply.SupplyChainOptions(
        repo_root=REPO_ROOT,
        state_dir=state_dir,
        image_repository="registry.example.test/synara/worker",
        docker_bin="docker",
        timeout_seconds=60.0,
        insecure_registry=insecure_registry,
        registry_auth_username_environment=registry_auth_username_environment,
        registry_auth_password_environment=registry_auth_password_environment,
        registry_ca_cert_environment=registry_ca_cert_environment,
        tool_proxy_url=tool_proxy_url,
        production_public_key_configmap_path=production_public_key_configmap_path,
        production_repository_configmap_path=production_repository_configmap_path,
    )


def write_runtime_configmaps(
    directory: pathlib.Path,
    *,
    public_key: str = PUBLIC_KEY_PEM,
    repository_pattern: str = "registry.example.test/synara/worker*",
) -> tuple[pathlib.Path, pathlib.Path]:
    public_key_path = directory / "runtime-public-key-configmap.yaml"
    repository_path = directory / "runtime-repository-configmap.yaml"
    public_key_path.write_text(
        (
            "apiVersion: v1\n"
            "kind: ConfigMap\n"
            "metadata:\n"
            "  name: synara-worker-cosign-public-key\n"
            "  namespace: synara-system\n"
            "data:\n"
            "  cosignPublicKey: |\n"
            + "".join(f"    {line}\n" for line in public_key.strip().splitlines())
        ),
        encoding="utf-8",
    )
    repository_path.write_text(
        (
            "apiVersion: v1\n"
            "kind: ConfigMap\n"
            "metadata:\n"
            "  name: synara-worker-signing-settings\n"
            "  namespace: synara-system\n"
            "data:\n"
            f"  repositoryPattern: {repository_pattern}\n"
        ),
        encoding="utf-8",
    )
    return public_key_path, repository_path


def copy_required_paths(repo_root: pathlib.Path, paths: tuple[pathlib.Path, ...]) -> None:
    for relative_path in paths:
        target = repo_root / relative_path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes((REPO_ROOT / relative_path).read_bytes())


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
        disposable = supply.load_configuration(REPO_ROOT)
        production = supply.load_configuration(REPO_ROOT, signing_policy_profile="production")

        self.assertIn(":v3.1.1@sha256:", disposable.tools.cosign)
        self.assertIn(":0.72.0@sha256:", disposable.tools.trivy)
        self.assertEqual(disposable.signing_policy_profile, "disposable")
        self.assertEqual(disposable.signing_policy.path, "deploy/worker/signing-policy.json")
        self.assertEqual(disposable.signing_policy.mode, "ephemeral-key")
        self.assertFalse(disposable.signing_policy.production_policy)
        self.assertFalse(disposable.signing_policy.require_transparency_log)
        self.assertIsNone(disposable.production_signing_profile)
        self.assertEqual(
            disposable.vulnerability_policy.blocked_severities,
            ("HIGH", "CRITICAL"),
        )
        self.assertFalse(disposable.vulnerability_policy.ignore_unfixed)
        self.assertEqual(disposable.vulnerability_policy.exceptions, ())

        self.assertEqual(production.signing_policy_profile, "production")
        self.assertEqual(
            production.signing_policy.path,
            "deploy/worker/production-signing-policy.json",
        )
        self.assertEqual(production.signing_policy.mode, "kms-key")
        self.assertTrue(production.signing_policy.production_policy)
        self.assertTrue(production.signing_policy.require_transparency_log)
        self.assertEqual(
            production.signing_policy.key_reference,
            supply.VAULT_TRANSIT_KEY_REFERENCE,
        )
        self.assertEqual(
            production.signing_policy.credential_environment,
            supply.VAULT_TRANSIT_REQUIRED_ENVIRONMENT,
        )
        profile = production.production_signing_profile
        self.assertIsNotNone(profile)
        assert profile is not None
        self.assertEqual(profile.path, "deploy/worker/production-signing-profile.json")
        self.assertEqual(profile.signer_type, "vault-transit-kms")
        self.assertEqual(profile.key_reference, supply.VAULT_TRANSIT_KEY_REFERENCE)
        self.assertEqual(profile.auth_method, supply.VAULT_TRANSIT_AUTH_METHOD)
        self.assertEqual(profile.principal, supply.VAULT_TRANSIT_PRINCIPAL)
        self.assertEqual(profile.audit_request_path, supply.VAULT_TRANSIT_AUDIT_REQUEST_PATH)
        self.assertEqual(profile.credential_environment, supply.VAULT_TRANSIT_REQUIRED_ENVIRONMENT)
        self.assertEqual(
            profile.registry_username_environment,
            supply.PRODUCTION_REGISTRY_USERNAME_ENV,
        )
        self.assertEqual(
            profile.registry_password_environment,
            supply.PRODUCTION_REGISTRY_PASSWORD_ENV,
        )
        self.assertEqual(
            profile.registry_ca_cert_environment,
            supply.PRODUCTION_REGISTRY_CA_CERT_ENV,
        )
        self.assertEqual(profile.transparency_log_provider, supply.TRANSPARENCY_LOG_PROVIDER)
        self.assertEqual(profile.transparency_log_url, supply.TRANSPARENCY_LOG_URL)
        self.assertTrue(profile.transparency_log_upload)
        self.assertTrue(profile.transparency_log_required)
        self.assertTrue(profile.transparency_log_verify)
        self.assertTrue(profile.transparency_log_inclusion_proof_required)
        self.assertTrue(profile.transparency_log_signed_entry_timestamp_required)
        self.assertEqual(profile.admission_provider, "kyverno")
        self.assertTrue(profile.admission_required)
        self.assertEqual(profile.admission_failure_policy, supply.ADMISSION_FAILURE_POLICY)
        self.assertEqual(
            profile.admission_validation_failure_action,
            supply.ADMISSION_VALIDATION_FAILURE_ACTION,
        )
        self.assertTrue(profile.admission_mutate_digest)
        self.assertTrue(profile.admission_verify_digest)
        self.assertEqual(profile.public_key_configmap_name, "synara-worker-cosign-public-key")
        self.assertEqual(profile.repository_configmap_name, "synara-worker-signing-settings")
        report = production.source_evidence()["productionSigningProfile"]
        self.assertEqual(report["signer"]["authMethod"], "approle")
        self.assertEqual(
            report["signer"]["principal"],
            "auth/approle/role/synara-worker-release-signer",
        )
        self.assertEqual(
            report["signer"]["auditRequestPath"],
            "transit/sign/synara-worker-release",
        )
        self.assertEqual(
            report["registryAccess"],
            {
                "usernameEnvironment": supply.PRODUCTION_REGISTRY_USERNAME_ENV,
                "passwordEnvironment": supply.PRODUCTION_REGISTRY_PASSWORD_ENV,
                "caCertEnvironment": supply.PRODUCTION_REGISTRY_CA_CERT_ENV,
            },
        )
        self.assertEqual(report["transparencyLog"]["provider"], "public-rekor")
        self.assertEqual(report["transparencyLog"]["url"], "https://rekor.sigstore.dev")
        self.assertTrue(report["transparencyLog"]["upload"])
        self.assertTrue(report["transparencyLog"]["inclusionProofRequired"])
        self.assertTrue(report["transparencyLog"]["signedEntryTimestampRequired"])
        self.assertEqual(report["admission"]["failurePolicy"], "Fail")
        self.assertEqual(report["admission"]["validationFailureAction"], "Enforce")
        self.assertTrue(report["admission"]["mutateDigest"])
        self.assertTrue(report["admission"]["verifyDigest"])
        self.assertEqual(
            report["admission"]["clusterPolicyPath"],
            "deploy/kubernetes/security/cluster/verify-synara-worker-images.yaml",
        )
        self.assertIn(
            supply.SECURITY_CLUSTER_KUSTOMIZATION_PATH / "kustomization.yaml",
            supply.PRODUCTION_RELEASE_SOURCE_PATHS,
        )
        self.assertIn(
            supply.SECURITY_NAMESPACE_KUSTOMIZATION_PATH / "kustomization.yaml",
            supply.PRODUCTION_RELEASE_SOURCE_PATHS,
        )

    def test_rejects_production_signing_profile_drift(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            repo_root = pathlib.Path(directory)
            copy_required_paths(repo_root, PRODUCTION_PROFILE_REQUIRED_PATHS)
            profile_path = repo_root / supply.PRODUCTION_SIGNING_PROFILE_PATH
            payload = json.loads(profile_path.read_text(encoding="utf-8"))
            payload["signer"]["principal"] = "auth/approle/role/other"
            profile_path.write_text(json.dumps(payload), encoding="utf-8")
            with self.assertRaises(supply.common.ReleaseGateError) as caught:
                supply.load_configuration(repo_root, signing_policy_profile="production")

        self.assertEqual(
            caught.exception.code,
            "release.registry_production_signing_profile_invalid",
        )

    def test_rejects_production_registry_access_environment_name_drift(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            repo_root = pathlib.Path(directory)
            copy_required_paths(repo_root, PRODUCTION_PROFILE_REQUIRED_PATHS)
            profile_path = repo_root / supply.PRODUCTION_SIGNING_PROFILE_PATH
            payload = json.loads(profile_path.read_text(encoding="utf-8"))
            payload["registryAccess"]["passwordEnvironment"] = "REGISTRY_PASSWORD_ENV"
            profile_path.write_text(json.dumps(payload), encoding="utf-8")
            with self.assertRaises(supply.common.ReleaseGateError) as caught:
                supply.load_configuration(repo_root, signing_policy_profile="production")

        self.assertEqual(
            caught.exception.code,
            "release.registry_production_signing_profile_invalid",
        )

    def test_rejects_stale_production_cluster_policy_repository_placeholder_drift(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            repo_root = pathlib.Path(directory)
            copy_required_paths(repo_root, PRODUCTION_PROFILE_REQUIRED_PATHS)
            cluster_policy_path = repo_root / supply.SECURITY_CLUSTER_POLICY_PATH
            cluster_policy_path.write_text(
                cluster_policy_path.read_text(encoding="utf-8").replace(
                    "registry.invalid/synara/worker*",
                    "192.168.139.3:5443/synara/worker*",
                ),
                encoding="utf-8",
            )
            with self.assertRaises(supply.common.ReleaseGateError) as caught:
                supply.load_configuration(repo_root, signing_policy_profile="production")

        self.assertEqual(
            caught.exception.code,
            "release.registry_production_signing_profile_invalid",
        )

    def test_rejects_cluster_policy_transparency_log_boundary_drift(self) -> None:
        cases = (
            ("missing-rekor", "                    rekor:\n", ""),
            (
                "wrong-rekor-url",
                "                      url: https://rekor.sigstore.dev",
                "                      url: https://rekor.example.test",
            ),
            ("ignore-tlog", "                      ignoreTlog: false", "                      ignoreTlog: true"),
        )
        for label, original, replacement in cases:
            with self.subTest(label=label), tempfile.TemporaryDirectory() as directory:
                repo_root = pathlib.Path(directory)
                copy_required_paths(repo_root, PRODUCTION_PROFILE_REQUIRED_PATHS)
                cluster_policy_path = repo_root / supply.SECURITY_CLUSTER_POLICY_PATH
                cluster_policy_path.write_text(
                    cluster_policy_path.read_text(encoding="utf-8").replace(original, replacement),
                    encoding="utf-8",
                )
                with self.assertRaises(supply.common.ReleaseGateError) as caught:
                    supply.load_configuration(repo_root, signing_policy_profile="production")
                self.assertEqual(
                    caught.exception.code,
                    "release.registry_production_signing_profile_invalid",
                )

    def test_rejects_cluster_policy_registry_credential_boundary_drift(self) -> None:
        cases = (
            (
                "missing-secret",
                "          imageRegistryCredentials:\n"
                "            secrets:\n"
                "              - synara-worker-registry-pull\n",
                "",
            ),
            (
                "wrong-secret",
                "              - synara-worker-registry-pull",
                "              - unrelated-registry-pull",
            ),
        )
        for label, original, replacement in cases:
            with self.subTest(label=label), tempfile.TemporaryDirectory() as directory:
                repo_root = pathlib.Path(directory)
                copy_required_paths(repo_root, PRODUCTION_PROFILE_REQUIRED_PATHS)
                cluster_policy_path = repo_root / supply.SECURITY_CLUSTER_POLICY_PATH
                cluster_policy_path.write_text(
                    cluster_policy_path.read_text(encoding="utf-8").replace(
                        original,
                        replacement,
                    ),
                    encoding="utf-8",
                )
                with self.assertRaises(supply.common.ReleaseGateError) as caught:
                    supply.load_configuration(repo_root, signing_policy_profile="production")
                self.assertEqual(
                    caught.exception.code,
                    "release.registry_production_signing_profile_invalid",
                )

    def test_rejects_production_kustomization_target_drift(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            repo_root = pathlib.Path(directory)
            copy_required_paths(repo_root, PRODUCTION_PROFILE_REQUIRED_PATHS)
            kustomization_path = (
                repo_root / supply.SECURITY_PRODUCTION_KUSTOMIZATION_PATH / "kustomization.yaml"
            )
            kustomization_path.write_text(
                kustomization_path.read_text(encoding="utf-8").replace(
                    "spec.rules.0.verifyImages.0.imageReferences.0",
                    "spec.rules.0.verifyImages.0.imageReferences.1",
                ),
                encoding="utf-8",
            )
            with self.assertRaises(supply.common.ReleaseGateError) as caught:
                supply.load_configuration(repo_root, signing_policy_profile="production")

        self.assertEqual(
            caught.exception.code,
            "release.registry_production_signing_profile_invalid",
        )

    def test_rejects_checked_in_public_key_placeholder_and_non_spki_pem(self) -> None:
        cases = (
            (
                "placeholder",
                "-----BEGIN PUBLIC KEY-----\nREPLACE_WITH_COSIGN_PUBLIC_KEY_PEM\n-----END PUBLIC KEY-----\n",
            ),
            (
                "non-spki",
                "-----BEGIN PUBLIC KEY-----\n"
                + base64.b64encode(bytes(range(80))).decode("ascii")
                + "\n-----END PUBLIC KEY-----\n",
            ),
        )
        for label, public_key in cases:
            with self.subTest(label=label), tempfile.TemporaryDirectory() as directory:
                repo_root = pathlib.Path(directory)
                copy_required_paths(repo_root, PRODUCTION_PROFILE_REQUIRED_PATHS)
                configmap_path = repo_root / supply.SECURITY_PUBLIC_KEY_CONFIGMAP_PATH
                configmap_path.write_text(
                    (
                        "apiVersion: v1\n"
                        "kind: ConfigMap\n"
                        "metadata:\n"
                        "  name: synara-worker-cosign-public-key\n"
                        "  namespace: synara-system\n"
                        "  labels:\n"
                        '    cache.kyverno.io/enabled: "true"\n'
                        "data:\n"
                        "  cosignPublicKey: |\n"
                        + "".join(f"    {line}\n" for line in public_key.strip().splitlines())
                    ),
                    encoding="utf-8",
                )
                with self.assertRaises(supply.common.ReleaseGateError) as caught:
                    supply.load_configuration(repo_root, signing_policy_profile="production")
                self.assertEqual(
                    caught.exception.code,
                    "release.registry_production_signing_profile_invalid",
                )

    def test_rejects_checked_in_repository_pattern_placeholder_and_invalid_shape(self) -> None:
        cases = (
            ("placeholder", supply.PLACEHOLDER_REPOSITORY_PATTERN),
            ("invalid-shape", "192.168.139.3:5443/synara/worker**"),
        )
        for label, repository_pattern in cases:
            with self.subTest(label=label), tempfile.TemporaryDirectory() as directory:
                repo_root = pathlib.Path(directory)
                copy_required_paths(repo_root, PRODUCTION_PROFILE_REQUIRED_PATHS)
                configmap_path = repo_root / supply.SECURITY_REPOSITORY_CONFIGMAP_PATH
                configmap_path.write_text(
                    (
                        "apiVersion: v1\n"
                        "kind: ConfigMap\n"
                        "metadata:\n"
                        "  name: synara-worker-signing-settings\n"
                        "  namespace: synara-system\n"
                        "  labels:\n"
                        '    cache.kyverno.io/enabled: "true"\n'
                        "data:\n"
                        f"  repositoryPattern: {repository_pattern}\n"
                    ),
                    encoding="utf-8",
                )
                with self.assertRaises(supply.common.ReleaseGateError) as caught:
                    supply.load_configuration(repo_root, signing_policy_profile="production")
                self.assertEqual(
                    caught.exception.code,
                    "release.registry_production_signing_profile_invalid",
                )

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
    def test_proxy_value_is_passed_by_environment_and_registry_bypasses_it(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            proxy_url = "http://host.docker.internal:6152"
            options = supply_options(
                pathlib.Path(directory) / "state",
                insecure_registry=True,
                tool_proxy_url=proxy_url,
            )
            completed = subprocess.CompletedProcess(["docker"], 0, stdout="ok", stderr="")
            redactor = acceptance.SecretRedactor()
            with mock.patch.object(supply.subprocess, "run", return_value=completed) as run:
                supply._run_tool(
                    options,
                    image="example.test/trivy:v1.0.0@sha256:" + "d" * 64,
                    arguments=["image", "registry.example.test/synara/worker@sha256:" + "a" * 64],
                    tool="trivy",
                    deadline=supply.time.monotonic() + 60,
                    redactor=redactor,
                    secret_environment={
                        "VAULT_ADDR": "https://vault.internal.test:8200",
                    },
                )

        command = run.call_args.args[0]
        environment = run.call_args.kwargs["env"]
        self.assertNotIn(proxy_url, command)
        for name in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY"):
            self.assertIn(name, command)
            self.assertIn(name, environment)
        self.assertEqual(environment["HTTPS_PROXY"], proxy_url)
        self.assertIn("registry.example.test", environment["NO_PROXY"])
        self.assertIn("host.docker.internal", environment["NO_PROXY"])
        self.assertIn("vault.internal.test", environment["NO_PROXY"])
        self.assertIn("localhost", environment["NO_PROXY"])

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

    def test_registry_access_materializes_docker_config_and_uses_tool_native_ca_flags(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            state_dir = pathlib.Path(directory) / "state"
            registry_ca_path = pathlib.Path(directory) / "registry-ca.pem"
            registry_ca_bytes = b"-----BEGIN CERTIFICATE-----\nfixture\n-----END CERTIFICATE-----\n"
            registry_ca_path.write_bytes(registry_ca_bytes)
            options = supply_options(
                state_dir,
                registry_auth_username_environment="REGISTRY_USER_ENV",
                registry_auth_password_environment="REGISTRY_PASSWORD_ENV",
                registry_ca_cert_environment="REGISTRY_CA_ENV",
            )
            redactor = acceptance.SecretRedactor()
            with mock.patch.dict(
                supply.os.environ,
                {
                    "REGISTRY_USER_ENV": "registry-user",
                    "REGISTRY_PASSWORD_ENV": "registry-password",
                    "REGISTRY_CA_ENV": str(registry_ca_path),
                },
            ):
                prepared = supply._prepare_registry_access(options, redactor=redactor)
                completed = subprocess.CompletedProcess(["docker"], 0, stdout="ok", stderr="")
                with mock.patch.object(supply.subprocess, "run", return_value=completed) as run:
                    supply._run_tool(
                        options,
                        image="example.test/cosign:v1.0.0@sha256:" + "d" * 64,
                        arguments=supply._classic_cosign_sign_arguments("--yes", REFERENCE),
                        tool="cosign",
                        deadline=supply.time.monotonic() + 60,
                        redactor=redactor,
                    )
                    cosign_command = run.call_args.args[0]
                    cosign_environment = run.call_args.kwargs["env"]
                    supply._run_tool(
                        options,
                        image="example.test/cosign:v1.0.0@sha256:" + "d" * 64,
                        arguments=["verify", "--output", "json", REFERENCE],
                        tool="cosign",
                        deadline=supply.time.monotonic() + 60,
                        redactor=redactor,
                    )
                    cosign_verify_command = run.call_args.args[0]
                    supply._run_tool(
                        options,
                        image="example.test/trivy:1.0.0@sha256:" + "e" * 64,
                        arguments=["image", "--format", "json", REFERENCE],
                        tool="trivy",
                        deadline=supply.time.monotonic() + 60,
                        redactor=redactor,
                    )
                    trivy_command = run.call_args.args[0]
                    trivy_environment = run.call_args.kwargs["env"]

            docker_config_dir = state_dir / "registry-access/docker-config"
            self.assertEqual(
                prepared.environment,
                {"DOCKER_CONFIG": "/workspace/registry-access/docker-config"},
            )
            self.assertEqual(
                prepared.host_environment,
                {"DOCKER_CONFIG": str(docker_config_dir)},
            )
            self.assertEqual(
                prepared.registry_ca_container_path,
                "/workspace/registry-access/docker-config/certs.d/registry.example.test/ca.crt",
            )
            self.assertTrue((docker_config_dir / "config.json").is_file())
            self.assertTrue(
                (docker_config_dir / "certs.d/registry.example.test/ca.crt").is_file()
            )
            self.assertNotIn("SSL_CERT_FILE", cosign_environment)
            self.assertNotIn("SSL_CERT_FILE", trivy_environment)
            self.assertNotIn("--registry-ca-cert", cosign_command)
            self.assertIn("--registry-cacert", cosign_command)
            self.assertIn("--new-bundle-format=false", cosign_command)
            self.assertIn("--use-signing-config=false", cosign_command)
            self.assertIn("--registry-cacert", cosign_verify_command)
            self.assertNotIn("--new-bundle-format=false", cosign_verify_command)
            self.assertNotIn("--use-signing-config=false", cosign_verify_command)
            self.assertIn(
                "/workspace/registry-access/docker-config/certs.d/registry.example.test/ca.crt",
                cosign_command,
            )
            self.assertIn("--cacert", trivy_command)
            self.assertIn(
                "/workspace/registry-access/docker-config/certs.d/registry.example.test/ca.crt",
                trivy_command,
            )

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
        plain_eof = supply.common.ReleaseGateError(
            "release.registry_supply_chain_command_failed",
            "Trivy failed.",
            {
                "tool": "trivy",
                "returnCode": 1,
                "outputExcerpt": (
                    'failed to download vulnerability DB: Get "https://mirror.gcr.io/v2/": EOF'
                ),
            },
        )
        policy_failure = supply.common.ReleaseGateError(
            "release.registry_supply_chain_command_failed",
            "Trivy failed.",
            {"tool": "trivy", "returnCode": 1, "outputExcerpt": "policy failure"},
        )
        completed = subprocess.CompletedProcess(["docker"], 0, stdout="", stderr="")

        self.assertTrue(supply._retryable_trivy_database_download(plain_eof))

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


class EphemeralSigningTest(unittest.TestCase):
    def test_ephemeral_signing_uses_classic_layout_without_external_services(self) -> None:
        reference = f"registry.example.test/synara/worker@{DIGEST}"
        calls: list[list[str]] = []
        with tempfile.TemporaryDirectory() as directory:
            state_dir = pathlib.Path(directory) / "state"

            def run_tool(_options: Any, **kwargs: Any) -> subprocess.CompletedProcess[str]:
                arguments = list(kwargs["arguments"])
                calls.append(arguments)
                if arguments[0] == "generate-key-pair":
                    key_prefix = state_dir / pathlib.Path(
                        arguments[arguments.index("--output-key-prefix") + 1]
                    )
                    key_prefix.parent.mkdir(parents=True, exist_ok=True)
                    key_prefix.with_suffix(".key").write_text("private", encoding="utf-8")
                    key_prefix.with_suffix(".pub").write_text(PUBLIC_KEY_PEM, encoding="utf-8")
                stdout = (
                    json.dumps(verification_payload(reference=reference))
                    if arguments[0] == "verify"
                    else ""
                )
                return subprocess.CompletedProcess(arguments, 0, stdout=stdout, stderr="")

            with mock.patch.object(supply, "_run_tool", side_effect=run_tool):
                result = supply._sign_and_verify(
                    supply_options(state_dir),
                    configuration_with(signing_policy()),
                    builds=[{"slot": "cached", "registryDigest": DIGEST}],
                    git_sha="a" * 40,
                    version="0.5.4",
                    run_id="run-1",
                    deadline=supply.time.monotonic() + 60,
                    redactor=acceptance.SecretRedactor(),
                )

            self.assertFalse((state_dir / "cosign/ephemeral.key").exists())

        self.assertEqual([arguments[0] for arguments in calls], ["generate-key-pair", "sign", "verify"])
        sign_arguments = calls[1]
        verify_arguments = calls[2]
        self.assertIn("--new-bundle-format=false", sign_arguments)
        self.assertIn("--use-signing-config=false", sign_arguments)
        self.assertIn("--tlog-upload=false", sign_arguments)
        self.assertEqual(
            sign_arguments[sign_arguments.index("--key") + 1],
            "cosign/ephemeral.key",
        )
        for argument in ("--bundle", "--signing-config", "--identity-token"):
            self.assertNotIn(argument, sign_arguments)
        self.assertIn("--insecure-ignore-tlog=true", verify_arguments)
        self.assertEqual(
            verify_arguments[verify_arguments.index("--key") + 1],
            "cosign/ephemeral.pub",
        )
        self.assertNotIn("--insecure-ignore-tlog=false", verify_arguments)
        self.assertEqual(result["mode"], "ephemeral-key")
        self.assertFalse(result["transparencyLog"])
        self.assertFalse(result["productionSigningPolicySatisfied"])
        self.assertTrue(result["privateKeyRemoved"])

    def test_ephemeral_signing_failure_removes_private_key(self) -> None:
        failure = supply.common.ReleaseGateError("test.cosign_failed", "Cosign failed.")
        with tempfile.TemporaryDirectory() as directory:
            state_dir = pathlib.Path(directory) / "state"

            def run_tool(_options: Any, **kwargs: Any) -> subprocess.CompletedProcess[str]:
                arguments = list(kwargs["arguments"])
                if arguments[0] == "generate-key-pair":
                    key_prefix = state_dir / pathlib.Path(
                        arguments[arguments.index("--output-key-prefix") + 1]
                    )
                    key_prefix.parent.mkdir(parents=True, exist_ok=True)
                    key_prefix.with_suffix(".key").write_text("private", encoding="utf-8")
                    key_prefix.with_suffix(".pub").write_text(PUBLIC_KEY_PEM, encoding="utf-8")
                    return subprocess.CompletedProcess(arguments, 0, stdout="", stderr="")
                raise failure

            with mock.patch.object(supply, "_run_tool", side_effect=run_tool), self.assertRaises(
                supply.common.ReleaseGateError
            ) as caught:
                supply._sign_and_verify(
                    supply_options(state_dir),
                    configuration_with(signing_policy()),
                    builds=[{"slot": "cached", "registryDigest": DIGEST}],
                    git_sha="a" * 40,
                    version="0.5.4",
                    run_id="run-1",
                    deadline=supply.time.monotonic() + 60,
                    redactor=acceptance.SecretRedactor(),
                )

            self.assertIs(caught.exception, failure)
            self.assertFalse((state_dir / "cosign/ephemeral.key").exists())


class SignatureVerificationTest(unittest.TestCase):
    def verification_payload(self, *, digest: str = DIGEST) -> list[dict[str, Any]]:
        return [
            {
                "critical": {
                    "identity": {"docker-reference": REFERENCE.rpartition("@")[0]},
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

    def test_accepts_sigstore_v03_protobuf_json_uint64_fields(self) -> None:
        bundle = transparency_bundle_payload()
        bundle["mediaType"] = "application/vnd.dev.sigstore.bundle.v0.3+json"
        entry = bundle["verificationMaterial"]["tlogEntries"][0]
        entry["logIndex"] = "7"
        entry["integratedTime"] = "1720000000"
        entry["inclusionProof"]["logIndex"] = "7"
        entry["inclusionProof"]["treeSize"] = "8"

        media_type, entries = supply._bundle_transparency_entries(
            bundle,
            require_inclusion_proof=True,
            require_signed_entry_timestamp=True,
            code="test.invalid_bundle",
            message="invalid bundle",
        )

        self.assertEqual(media_type, "application/vnd.dev.sigstore.bundle.v0.3+json")
        self.assertEqual(entries[0]["logIndex"], 7)
        self.assertEqual(entries[0]["integratedTime"], 1_720_000_000)
        self.assertTrue(entries[0]["inclusionProofPresent"])
        self.assertTrue(entries[0]["signedEntryTimestampPresent"])

    def test_rejects_noncanonical_protobuf_json_uint64_fields(self) -> None:
        for invalid_value in (True, -1, "-1", "07", str(1 << 64)):
            with self.subTest(invalid_value=invalid_value):
                bundle = transparency_bundle_payload()
                bundle["verificationMaterial"]["tlogEntries"][0]["inclusionProof"][
                    "treeSize"
                ] = invalid_value
                with self.assertRaises(supply.common.ReleaseGateError):
                    supply._bundle_transparency_entries(
                        bundle,
                        require_inclusion_proof=True,
                        require_signed_entry_timestamp=True,
                        code="test.invalid_bundle",
                        message="invalid bundle",
                    )

    def test_rejects_bundle_material_binding_mismatch(self) -> None:
        payload = b"signed payload"
        signature = b"signature"
        bundle = transparency_bundle_payload()
        bundle["messageSignature"] = {
            "messageDigest": {
                "algorithm": "SHA2_256",
                "digest": base64.b64encode(hashlib.sha256(payload).digest()).decode("ascii"),
            },
            "signature": base64.b64encode(b"different signature").decode("ascii"),
        }
        with tempfile.TemporaryDirectory() as directory:
            bundle_path = pathlib.Path(directory) / "bundle.json"
            bundle_path.write_text(json.dumps(bundle), encoding="utf-8")
            with self.assertRaises(supply.common.ReleaseGateError) as caught:
                supply._validate_cosign_bundle_binding(
                    bundle_path,
                    payload_bytes=payload,
                    signature_bytes=signature,
                )

        self.assertEqual(caught.exception.code, "release.registry_transparency_log_invalid")


class ProductionSigningTest(unittest.TestCase):
    def _lookup_vault_identity(
        self,
        directory: str,
        *,
        payload: Any = None,
        response_body: bytes | None = None,
        status: int = 200,
        address: str = "https://vault.example.test:8200",
        context_error: Exception | None = None,
        request_error: Exception | None = None,
    ) -> tuple[dict[str, Any], mock.Mock, mock.Mock, mock.Mock, pathlib.Path]:
        state_dir = pathlib.Path(directory) / "state"
        materialized_cacert = state_dir / supply.MATERIALIZED_VAULT_CACERT_RELATIVE_PATH
        materialized_cacert.parent.mkdir(parents=True, exist_ok=True)
        materialized_cacert.write_bytes(b"materialized Vault CA fixture")
        policy = signing_policy(
            path=str(supply.PRODUCTION_SIGNING_POLICY_PATH),
            mode="kms-key",
            require_transparency_log=True,
            key_reference=supply.VAULT_TRANSIT_KEY_REFERENCE,
            credential_environment=supply.VAULT_TRANSIT_REQUIRED_ENVIRONMENT,
        )
        configuration = production_configuration_with(policy)
        prepared_environment = supply.PreparedKmsEnvironment(
            secret_environment={
                "VAULT_ADDR": address,
                "VAULT_TOKEN": "vault-token-value",
                "VAULT_CACERT": (
                    f"/workspace/{supply.MATERIALIZED_VAULT_CACERT_RELATIVE_PATH.as_posix()}"
                ),
            },
            vault_ca_materialized=True,
        )
        if response_body is None:
            response_body = json.dumps(
                vault_token_lookup_payload() if payload is None else payload
            ).encode("utf-8")
        response = mock.Mock(status=status)
        response.read.return_value = response_body
        connection = mock.Mock()
        connection.getresponse.return_value = response
        connection.request.side_effect = request_error
        context = object()
        context_patch = mock.patch.object(
            supply.ssl,
            "create_default_context",
            return_value=context,
            side_effect=context_error,
        )
        connection_patch = mock.patch.object(
            supply.http.client,
            "HTTPSConnection",
            return_value=connection,
        )
        with context_patch as create_default_context, connection_patch as https_connection:
            identity = supply._production_vault_signer_identity(
                supply_options(state_dir),
                configuration,
                prepared_environment=prepared_environment,
                deadline=supply.time.monotonic() + 60,
            )
        return (
            identity,
            connection,
            create_default_context,
            https_connection,
            materialized_cacert,
        )

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
                    write_classic_signing_outputs(state_dir, arguments)
                if arguments[:2] == ["bundle", "create"]:
                    write_bundle_create_output(state_dir, arguments)
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
        bundle_arguments = calls[2]["arguments"]
        self.assertIn("--tlog-upload=true", sign_arguments)
        self.assertIn("--new-bundle-format=false", sign_arguments)
        self.assertIn("--use-signing-config=false", sign_arguments)
        self.assertIn("--output-signature", sign_arguments)
        self.assertIn("--output-payload", sign_arguments)
        self.assertIn("--output-certificate", sign_arguments)
        self.assertNotIn("--bundle", sign_arguments)
        self.assertIn("--certificate-identity", verify_arguments)
        self.assertIn("--certificate-oidc-issuer", verify_arguments)
        self.assertIn("--insecure-ignore-tlog=false", verify_arguments)
        self.assertEqual(bundle_arguments[:2], ["bundle", "create"])
        self.assertIn("--certificate", bundle_arguments)
        self.assertNotIn("--key", bundle_arguments)
        self.assertEqual(
            bundle_arguments[bundle_arguments.index("--rekor-url") + 1],
            supply.TRANSPARENCY_LOG_URL,
        )
        self.assertEqual(result["mode"], "keyless")
        self.assertTrue(result["productionSigningPolicySatisfied"])
        self.assertTrue(result["transparencyLogVerified"])
        self.assertTrue(result["transparencyLogInclusionProofPresent"])
        self.assertTrue(result["transparencyLogSignedEntryTimestampPresent"])
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
            if arguments[0] == "sign":
                write_classic_signing_outputs(pathlib.Path(directory) / "state", arguments)
            if arguments[:2] == ["bundle", "create"]:
                write_bundle_create_output(pathlib.Path(directory) / "state", arguments)
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
        self.assertIn("--new-bundle-format=false", calls[0]["arguments"])
        self.assertIn("--use-signing-config=false", calls[0]["arguments"])
        self.assertIn("--output-signature", calls[0]["arguments"])
        self.assertIn("--output-payload", calls[0]["arguments"])
        self.assertNotIn("--bundle", calls[0]["arguments"])
        self.assertEqual(calls[0]["arguments"][calls[0]["arguments"].index("--key") + 1], key_reference)
        self.assertIn("--insecure-ignore-tlog=false", calls[1]["arguments"])
        self.assertEqual(calls[2]["arguments"][:2], ["bundle", "create"])
        self.assertEqual(
            calls[2]["arguments"][calls[2]["arguments"].index("--key") + 1],
            key_reference,
        )
        self.assertEqual(result["mode"], "kms-key")
        self.assertTrue(result["productionSigningPolicySatisfied"])
        self.assertTrue(result["transparencyLogVerified"])
        self.assertTrue(result["transparencyLogInclusionProofPresent"])
        self.assertTrue(result["transparencyLogSignedEntryTimestampPresent"])
        self.assertEqual(result["credentialEnvironmentCount"], 1)

    def test_vault_signer_identity_lookup_uses_materialized_ca_and_reports_only_safe_fields(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            identity, connection, create_context, https_connection, materialized_cacert = (
                self._lookup_vault_identity(directory)
            )

        expected_policy_hash = supply._sha256(
            json.dumps(
                ["synara-worker-release-signer"],
                separators=(",", ":"),
                sort_keys=True,
            ).encode("utf-8")
        )
        self.assertEqual(
            identity,
            {
                "verified": True,
                "displayName": "approle",
                "roleName": "synara-worker-release-signer",
                "type": "batch",
                "orphan": True,
                "policyCount": 1,
                "policiesSha256": expected_policy_hash,
            },
        )
        create_context.assert_called_once_with(cafile=str(materialized_cacert.resolve()))
        https_connection.assert_called_once_with(
            "vault.example.test:8200",
            timeout=mock.ANY,
            context=create_context.return_value,
        )
        connection.request.assert_called_once_with(
            "GET",
            supply.VAULT_TOKEN_LOOKUP_PATH,
            headers={
                "Accept": "application/json",
                "X-Vault-Token": "vault-token-value",
            },
        )
        connection.getresponse.return_value.read.assert_called_once_with(
            supply.VAULT_TOKEN_LOOKUP_MAX_RESPONSE_BYTES + 1
        )
        connection.close.assert_called_once_with()
        serialized = json.dumps(identity, sort_keys=True)
        self.assertNotIn("vault-token-value", serialized)
        self.assertNotIn(str(materialized_cacert), serialized)

    def test_vault_signer_identity_rejects_root_token_shape(self) -> None:
        root_payload = vault_token_lookup_payload(
            display_name="root",
            role_name="root",
            token_type="service",
            policies=["root"],
        )
        with tempfile.TemporaryDirectory() as directory, self.assertRaises(
            supply.common.ReleaseGateError
        ) as caught:
            self._lookup_vault_identity(directory, payload=root_payload)

        self.assertEqual(caught.exception.code, "release.registry_signing_credential_invalid")
        serialized = json.dumps(caught.exception.as_report_error(), sort_keys=True)
        self.assertNotIn("vault-token-value", serialized)
        self.assertNotIn(json.dumps(root_payload, sort_keys=True), serialized)

    def test_vault_signer_identity_rejects_wrong_approle(self) -> None:
        with tempfile.TemporaryDirectory() as directory, self.assertRaises(
            supply.common.ReleaseGateError
        ) as caught:
            self._lookup_vault_identity(
                directory,
                payload=vault_token_lookup_payload(
                    role_name="other-release-signer",
                    policies=["synara-worker-release-signer"],
                ),
            )

        self.assertEqual(caught.exception.code, "release.registry_signing_credential_invalid")

    def test_vault_signer_identity_rejects_default_and_extra_policies(self) -> None:
        policy_sets = (
            ["synara-worker-release-signer", "default"],
            ["synara-worker-release-signer", "other-policy"],
        )
        for policies in policy_sets:
            with self.subTest(policies=policies), tempfile.TemporaryDirectory() as directory:
                with self.assertRaises(supply.common.ReleaseGateError) as caught:
                    self._lookup_vault_identity(
                        directory,
                        payload=vault_token_lookup_payload(policies=policies),
                    )
                self.assertEqual(
                    caught.exception.code,
                    "release.registry_signing_credential_invalid",
                )

    def test_vault_signer_identity_requires_strict_https_authority(self) -> None:
        invalid_addresses = (
            "http://vault.example.test:8200",
            "https://root@vault.example.test:8200",
            "https://vault.example.test:8200/v1",
            "https://vault.example.test:8200?namespace=admin",
            "https://vault.example.test:8200?",
            "https://vault.example.test:8200#fragment",
            "https://vault.example.test:8200#",
            "https://vault.example.test:0",
        )
        for address in invalid_addresses:
            with self.subTest(address=address), tempfile.TemporaryDirectory() as directory:
                with self.assertRaises(supply.common.ReleaseGateError) as caught:
                    self._lookup_vault_identity(directory, address=address)
                self.assertEqual(
                    caught.exception.code,
                    "release.registry_signing_credential_invalid",
                )

    def test_vault_signer_identity_fails_closed_on_tls_http_network_and_malformed_responses(self) -> None:
        cases = (
            {"context_error": supply.ssl.SSLError("TLS fixture failure")},
            {"request_error": OSError("network fixture failure")},
            {"status": 403, "response_body": b"forbidden response fixture"},
            {"response_body": b"{not-json"},
            {"payload": {"data": []}},
            {"response_body": "not-bytes"},
            {
                "response_body": b"x"
                * (supply.VAULT_TOKEN_LOOKUP_MAX_RESPONSE_BYTES + 1)
            },
        )
        for case in cases:
            with self.subTest(case=tuple(case)), tempfile.TemporaryDirectory() as directory:
                with self.assertRaises(supply.common.ReleaseGateError) as caught:
                    self._lookup_vault_identity(directory, **case)
                self.assertEqual(
                    caught.exception.code,
                    "release.registry_signing_credential_invalid",
                )
                serialized = json.dumps(caught.exception.as_report_error(), sort_keys=True)
                self.assertNotIn("vault-token-value", serialized)
                self.assertNotIn("forbidden response fixture", serialized)
                self.assertNotIn("network fixture failure", serialized)
                self.assertNotIn("TLS fixture failure", serialized)

    def test_hashivault_signing_materializes_vault_ca_into_gate_state(self) -> None:
        token = "vault-token-value"
        address = "https://vault.example.test"
        policy = signing_policy(
            path=str(supply.PRODUCTION_SIGNING_POLICY_PATH),
            mode="kms-key",
            require_transparency_log=True,
            key_reference=supply.VAULT_TRANSIT_KEY_REFERENCE,
            credential_environment=supply.VAULT_TRANSIT_REQUIRED_ENVIRONMENT,
        )
        reference = f"registry.example.test/synara/worker@{DIGEST}"
        calls: list[dict[str, Any]] = []
        with tempfile.TemporaryDirectory() as directory:
            state_dir = pathlib.Path(directory) / "state"
            ca_path = pathlib.Path(directory) / "ca.pem"
            ca_bytes = (
                b"-----BEGIN CERTIFICATE-----\n"
                b"MIIBsjCCAVmgAwIBAgIUQ2hlY2tlZEluQ0EwCgYIKoZIzj0EAwIwEzERMA8GA1UE\n"
                b"AwwIdmF1bHQtY2EwHhcNMjYwNzE5MDAwMDAwWhcNMzYwNzE2MDAwMDAwWjATMREw\n"
                b"DwYDVQQDDAh2YXVsdC1jYTBZMBMGByqGSM49AgEGCCqGSM49AwEHA0IABH2sKp75\n"
                b"vVZ8jw2C4gP9b+Yr1Gg9c9m6dQH3v9wM9V5A7Q0YkDf6Jf4G1uW0n2c+uY3JY5U2\n"
                b"5VdB9rjN7s7X5wSjUzBRMB0GA1UdDgQWBBRDZXBsYXlPbkx5Q2hlY2s2MDBTMB8G\n"
                b"A1UdIwQYMBaAFENlcGxheU9uTHlDaGVjazYwMFQwDwYDVR0TAQH/BAUwAwEB/zAK\n"
                b"BggqhkjOPQQDAgNIADBFAiEA4d3rQIfvH5h5rQ4WQwY3mA9Vq1EJ9gZb1x0kN6d5\n"
                b"uCsCIFm+f36m7x9Y2nR2rG4sy5pT4h5zvK6A8M0w3Q9F6R9C\n"
                b"-----END CERTIFICATE-----\n"
            )
            ca_path.write_bytes(ca_bytes)

            def run_tool(_options: Any, **kwargs: Any) -> subprocess.CompletedProcess[str]:
                calls.append(kwargs)
                secret_environment = kwargs["secret_environment"]
                self.assertEqual(secret_environment["VAULT_ADDR"], address)
                self.assertEqual(secret_environment["VAULT_TOKEN"], token)
                self.assertEqual(
                    secret_environment["VAULT_CACERT"],
                    "/workspace/vault/ca-certificates/vault-ca.crt",
                )
                materialized = state_dir / supply.MATERIALIZED_VAULT_CACERT_RELATIVE_PATH
                self.assertTrue(materialized.is_file())
                self.assertEqual(materialized.read_bytes(), ca_bytes)
                self.assertEqual(materialized.stat().st_mode & 0o777, 0o600)
                self.assertNotIn(str(ca_path), json.dumps(secret_environment))
                arguments = kwargs["arguments"]
                if arguments[0] == "sign":
                    write_classic_signing_outputs(state_dir, arguments)
                if arguments[:2] == ["bundle", "create"]:
                    write_bundle_create_output(state_dir, arguments)
                stdout = json.dumps(verification_payload(reference=reference)) if arguments[0] == "verify" else ""
                return subprocess.CompletedProcess(arguments, 0, stdout=stdout, stderr="")

            with mock.patch.dict(
                supply.os.environ,
                {
                    "VAULT_ADDR": address,
                    "VAULT_TOKEN": token,
                    "VAULT_CACERT": str(ca_path),
                },
            ), mock.patch.object(supply, "_run_tool", side_effect=run_tool):
                result = supply._sign_and_verify(
                    supply_options(state_dir),
                    configuration_with(policy),
                    builds=[{"slot": "cached", "registryDigest": DIGEST}],
                    git_sha="a" * 40,
                    version="0.5.4",
                    run_id="run-1",
                    deadline=supply.time.monotonic() + 60,
                    redactor=acceptance.SecretRedactor(),
                )

        self.assertEqual(result["mode"], "kms-key")
        self.assertTrue(result["vaultCaMaterialized"])
        self.assertEqual(
            result["credentialEnvironmentNames"],
            list(supply.VAULT_TRANSIT_REQUIRED_ENVIRONMENT),
        )
        self.assertEqual(len(calls), 3)

    def test_production_kms_signing_validates_runtime_admission_inputs_against_kms_key(self) -> None:
        token = "vault-token-value"
        address = "https://vault.example.test"
        policy = signing_policy(
            path=str(supply.PRODUCTION_SIGNING_POLICY_PATH),
            mode="kms-key",
            require_transparency_log=True,
            key_reference=supply.VAULT_TRANSIT_KEY_REFERENCE,
            credential_environment=supply.VAULT_TRANSIT_REQUIRED_ENVIRONMENT,
        )
        configuration = production_configuration_with(policy)
        reference = f"registry.example.test/synara/worker@{DIGEST}"
        with tempfile.TemporaryDirectory() as directory:
            state_dir = pathlib.Path(directory) / "state"
            vault_ca_path = pathlib.Path(directory) / "vault-ca.pem"
            vault_ca_path.write_bytes(
                b"-----BEGIN CERTIFICATE-----\nfixture\n-----END CERTIFICATE-----\n"
            )
            public_key_path, repository_path = write_runtime_configmaps(pathlib.Path(directory))
            calls: list[list[str]] = []
            events: list[str] = []
            signer_identity = {
                "verified": True,
                "displayName": "approle",
                "roleName": "synara-worker-release-signer",
                "type": "batch",
                "orphan": True,
                "policyCount": 1,
                "policiesSha256": "c" * 64,
            }

            def run_tool(_options: Any, **kwargs: Any) -> subprocess.CompletedProcess[str]:
                arguments = kwargs["arguments"]
                calls.append(list(arguments))
                events.append(arguments[0])
                if arguments[0] == "public-key":
                    return subprocess.CompletedProcess(arguments, 0, stdout=PUBLIC_KEY_PEM, stderr="")
                if arguments[0] == "sign":
                    self.assertEqual(
                        arguments[arguments.index("--key") + 1],
                        supply.VAULT_TRANSIT_KEY_REFERENCE,
                    )
                    write_classic_signing_outputs(state_dir, arguments)
                if arguments[0] == "verify":
                    self.assertEqual(
                        arguments[arguments.index("--key") + 1],
                        supply.MATERIALIZED_KMS_PUBLIC_KEY_RELATIVE_PATH.as_posix(),
                    )
                    self.assertIsNone(kwargs["secret_environment"])
                    materialized_key = (
                        state_dir / supply.MATERIALIZED_KMS_PUBLIC_KEY_RELATIVE_PATH
                    )
                    self.assertEqual(materialized_key.read_text(encoding="utf-8"), PUBLIC_KEY_PEM)
                    self.assertEqual(materialized_key.stat().st_mode & 0o777, 0o600)
                if arguments[:2] == ["bundle", "create"]:
                    self.assertEqual(
                        arguments[arguments.index("--key") + 1],
                        supply.MATERIALIZED_KMS_PUBLIC_KEY_RELATIVE_PATH.as_posix(),
                    )
                    self.assertIsNone(kwargs["secret_environment"])
                    write_bundle_create_output(state_dir, arguments)
                stdout = json.dumps(verification_payload(reference=reference)) if arguments[0] == "verify" else ""
                return subprocess.CompletedProcess(arguments, 0, stdout=stdout, stderr="")

            def verify_signer_identity(*_args: Any, **_kwargs: Any) -> dict[str, Any]:
                events.append("vault-lookup-self")
                return signer_identity

            with mock.patch.dict(
                supply.os.environ,
                {
                    "VAULT_ADDR": address,
                    "VAULT_TOKEN": token,
                    "VAULT_CACERT": str(vault_ca_path),
                },
            ), mock.patch.object(
                supply,
                "_production_vault_signer_identity",
                side_effect=verify_signer_identity,
            ) as identity_lookup, mock.patch.object(supply, "_run_tool", side_effect=run_tool):
                result = supply._sign_and_verify(
                    supply_options(
                        state_dir,
                        registry_auth_username_environment=supply.PRODUCTION_REGISTRY_USERNAME_ENV,
                        registry_auth_password_environment=supply.PRODUCTION_REGISTRY_PASSWORD_ENV,
                        registry_ca_cert_environment=supply.PRODUCTION_REGISTRY_CA_CERT_ENV,
                        production_public_key_configmap_path=public_key_path,
                        production_repository_configmap_path=repository_path,
                    ),
                    configuration,
                    builds=[{"slot": "cached", "registryDigest": DIGEST}],
                    git_sha="a" * 40,
                    version="0.5.4",
                    run_id="run-1",
                    deadline=supply.time.monotonic() + 60,
                    redactor=acceptance.SecretRedactor(),
                )

        self.assertTrue(result["productionAdmissionValidated"])
        self.assertTrue(result["admission"]["runtimeValidated"])
        self.assertEqual(
            result["admission"]["repositoryConfigMap"]["pattern"],
            "registry.example.test/synara/worker*",
        )
        self.assertEqual(
            result["admission"]["publicKeyConfigMap"]["sha256"],
            result["admission"]["publicKeyConfigMap"]["kmsSha256"],
        )
        self.assertEqual(calls[0][0], "public-key")
        self.assertEqual(calls[1][0], "sign")
        self.assertEqual(calls[2][0], "verify")
        self.assertEqual(calls[3][:2], ["bundle", "create"])
        self.assertIn("--insecure-ignore-tlog=false", calls[2])
        self.assertEqual(
            events,
            ["public-key", "vault-lookup-self", "sign", "verify", "bundle"],
        )
        self.assertEqual(result["verificationKeyMode"], "kms-exported-public-key")
        identity_lookup.assert_called_once()
        self.assertEqual(result["signerIdentity"], signer_identity)

    def test_production_kms_signing_rejects_registry_access_environment_name_drift(self) -> None:
        policy = signing_policy(
            path=str(supply.PRODUCTION_SIGNING_POLICY_PATH),
            mode="kms-key",
            require_transparency_log=True,
            key_reference=supply.VAULT_TRANSIT_KEY_REFERENCE,
            credential_environment=supply.VAULT_TRANSIT_REQUIRED_ENVIRONMENT,
        )
        configuration = production_configuration_with(policy)
        with tempfile.TemporaryDirectory() as directory, mock.patch.dict(
            supply.os.environ,
            {
                "VAULT_ADDR": "https://vault.example.test",
                "VAULT_TOKEN": "vault-token-value",
                "VAULT_CACERT": str(pathlib.Path(directory) / "vault-ca.pem"),
            },
        ):
            state_dir = pathlib.Path(directory) / "state"
            vault_ca_path = pathlib.Path(directory) / "vault-ca.pem"
            vault_ca_path.write_bytes(
                b"-----BEGIN CERTIFICATE-----\nfixture\n-----END CERTIFICATE-----\n"
            )
            public_key_path, repository_path = write_runtime_configmaps(pathlib.Path(directory))
            with self.assertRaises(supply.common.ReleaseGateError) as caught:
                supply._sign_and_verify(
                    supply_options(
                        state_dir,
                        registry_auth_username_environment="REGISTRY_USER_ENV",
                        registry_auth_password_environment="REGISTRY_PASSWORD_ENV",
                        registry_ca_cert_environment="REGISTRY_CA_ENV",
                        production_public_key_configmap_path=public_key_path,
                        production_repository_configmap_path=repository_path,
                    ),
                    configuration,
                    builds=[{"slot": "cached", "registryDigest": DIGEST}],
                    git_sha="a" * 40,
                    version="0.5.4",
                    run_id="run-1",
                    deadline=supply.time.monotonic() + 60,
                    redactor=acceptance.SecretRedactor(),
                )

        self.assertEqual(caught.exception.code, "release.registry_signing_credential_invalid")

    def test_kms_signing_rejects_missing_bundle_inclusion_proof(self) -> None:
        credential = "kms-secret-value"
        key_reference = "awskms:///arn:aws:kms:us-east-1:123456789012:key/key-id"
        policy = signing_policy(
            mode="kms-key",
            require_transparency_log=True,
            key_reference=key_reference,
            credential_environment=("SYNARA_TEST_KMS_CREDENTIAL",),
        )
        with tempfile.TemporaryDirectory() as directory, mock.patch.dict(
            supply.os.environ,
            {"SYNARA_TEST_KMS_CREDENTIAL": credential},
        ):
            state_dir = pathlib.Path(directory) / "state"

            def run_tool(_options: Any, **kwargs: Any) -> subprocess.CompletedProcess[str]:
                arguments = kwargs["arguments"]
                if arguments[0] == "sign":
                    write_classic_signing_outputs(state_dir, arguments)
                if arguments[:2] == ["bundle", "create"]:
                    write_bundle_create_output(
                        state_dir,
                        arguments,
                        include_inclusion_proof=False,
                    )
                stdout = json.dumps(verification_payload(reference=f"registry.example.test/synara/worker@{DIGEST}")) if arguments[0] == "verify" else ""
                return subprocess.CompletedProcess(arguments, 0, stdout=stdout, stderr="")

            with mock.patch.object(supply, "_run_tool", side_effect=run_tool), self.assertRaises(
                supply.common.ReleaseGateError
            ) as caught:
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

        self.assertEqual(caught.exception.code, "release.registry_transparency_log_invalid")

    def test_kms_signing_rejects_missing_bundle_signed_entry_timestamp(self) -> None:
        credential = "kms-secret-value"
        key_reference = "awskms:///arn:aws:kms:us-east-1:123456789012:key/key-id"
        policy = signing_policy(
            mode="kms-key",
            require_transparency_log=True,
            key_reference=key_reference,
            credential_environment=("SYNARA_TEST_KMS_CREDENTIAL",),
        )
        with tempfile.TemporaryDirectory() as directory, mock.patch.dict(
            supply.os.environ,
            {"SYNARA_TEST_KMS_CREDENTIAL": credential},
        ):
            state_dir = pathlib.Path(directory) / "state"

            def run_tool(_options: Any, **kwargs: Any) -> subprocess.CompletedProcess[str]:
                arguments = kwargs["arguments"]
                if arguments[0] == "sign":
                    write_classic_signing_outputs(state_dir, arguments)
                if arguments[:2] == ["bundle", "create"]:
                    write_bundle_create_output(
                        state_dir,
                        arguments,
                        include_signed_entry_timestamp=False,
                    )
                stdout = json.dumps(verification_payload(reference=f"registry.example.test/synara/worker@{DIGEST}")) if arguments[0] == "verify" else ""
                return subprocess.CompletedProcess(arguments, 0, stdout=stdout, stderr="")

            with mock.patch.object(supply, "_run_tool", side_effect=run_tool), self.assertRaises(
                supply.common.ReleaseGateError
            ) as caught:
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

        self.assertEqual(caught.exception.code, "release.registry_transparency_log_invalid")

    def test_kms_signing_rejects_stale_unrefreshed_local_material(self) -> None:
        credential = "kms-secret-value"
        key_reference = "awskms:///arn:aws:kms:us-east-1:123456789012:key/key-id"
        policy = signing_policy(
            mode="kms-key",
            require_transparency_log=True,
            key_reference=key_reference,
            credential_environment=("SYNARA_TEST_KMS_CREDENTIAL",),
        )
        with tempfile.TemporaryDirectory() as directory, mock.patch.dict(
            supply.os.environ,
            {"SYNARA_TEST_KMS_CREDENTIAL": credential},
        ):
            state_dir = pathlib.Path(directory) / "state"
            paths = supply._cosign_signature_evidence_paths(
                "cached",
                include_certificate=False,
            )
            for relative_path in (paths.bundle, paths.signature, paths.payload):
                path = state_dir / relative_path
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("stale", encoding="utf-8")

            def run_tool(_options: Any, **kwargs: Any) -> subprocess.CompletedProcess[str]:
                arguments = kwargs["arguments"]
                if arguments[0] == "sign":
                    for relative_path in (paths.bundle, paths.signature, paths.payload):
                        self.assertFalse((state_dir / relative_path).exists())
                if arguments[:2] == ["bundle", "create"]:
                    self.fail("stale signing material reached bundle creation")
                stdout = (
                    json.dumps(
                        verification_payload(
                            reference=f"registry.example.test/synara/worker@{DIGEST}"
                        )
                    )
                    if arguments[0] == "verify"
                    else ""
                )
                return subprocess.CompletedProcess(arguments, 0, stdout=stdout, stderr="")

            with mock.patch.object(supply, "_run_tool", side_effect=run_tool), self.assertRaises(
                supply.common.ReleaseGateError
            ) as caught:
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

        self.assertEqual(caught.exception.code, "release.registry_transparency_log_invalid")

    def test_production_runtime_admission_rejects_placeholder_public_key_before_kms_export(self) -> None:
        policy = signing_policy(
            path=str(supply.PRODUCTION_SIGNING_POLICY_PATH),
            mode="kms-key",
            require_transparency_log=True,
            key_reference=supply.VAULT_TRANSIT_KEY_REFERENCE,
            credential_environment=supply.VAULT_TRANSIT_REQUIRED_ENVIRONMENT,
        )
        configuration = production_configuration_with(policy)
        with tempfile.TemporaryDirectory() as directory:
            state_dir = pathlib.Path(directory) / "state"
            vault_ca_path = pathlib.Path(directory) / "vault-ca.pem"
            vault_ca_path.write_bytes(
                b"-----BEGIN CERTIFICATE-----\nfixture\n-----END CERTIFICATE-----\n"
            )
            public_key_path, repository_path = write_runtime_configmaps(
                pathlib.Path(directory),
                public_key=(
                    "-----BEGIN PUBLIC KEY-----\n"
                    "REPLACE_WITH_COSIGN_PUBLIC_KEY_PEM\n"
                    "-----END PUBLIC KEY-----\n"
                ),
            )
            with mock.patch.dict(
                supply.os.environ,
                {
                    "VAULT_ADDR": "https://vault.example.test",
                    "VAULT_TOKEN": "vault-token-value",
                    "VAULT_CACERT": str(vault_ca_path),
                },
            ), mock.patch.object(supply, "_run_tool") as run_tool, self.assertRaises(
                supply.common.ReleaseGateError
            ) as caught:
                supply._sign_and_verify(
                    supply_options(
                        state_dir,
                        registry_auth_username_environment=supply.PRODUCTION_REGISTRY_USERNAME_ENV,
                        registry_auth_password_environment=supply.PRODUCTION_REGISTRY_PASSWORD_ENV,
                        registry_ca_cert_environment=supply.PRODUCTION_REGISTRY_CA_CERT_ENV,
                        production_public_key_configmap_path=public_key_path,
                        production_repository_configmap_path=repository_path,
                    ),
                    configuration,
                    builds=[{"slot": "cached", "registryDigest": DIGEST}],
                    git_sha="a" * 40,
                    version="0.5.4",
                    run_id="run-1",
                    deadline=supply.time.monotonic() + 60,
                    redactor=acceptance.SecretRedactor(),
                )

        self.assertEqual(
            caught.exception.code,
            "release.registry_production_admission_input_invalid",
        )
        self.assertEqual(run_tool.call_count, 0)

    def test_production_runtime_admission_rejects_invalid_public_key_pem(self) -> None:
        policy = signing_policy(
            path=str(supply.PRODUCTION_SIGNING_POLICY_PATH),
            mode="kms-key",
            require_transparency_log=True,
            key_reference=supply.VAULT_TRANSIT_KEY_REFERENCE,
            credential_environment=supply.VAULT_TRANSIT_REQUIRED_ENVIRONMENT,
        )
        configuration = production_configuration_with(policy)
        with tempfile.TemporaryDirectory() as directory:
            state_dir = pathlib.Path(directory) / "state"
            vault_ca_path = pathlib.Path(directory) / "vault-ca.pem"
            vault_ca_path.write_bytes(
                b"-----BEGIN CERTIFICATE-----\nfixture\n-----END CERTIFICATE-----\n"
            )
            public_key_path, repository_path = write_runtime_configmaps(
                pathlib.Path(directory),
                public_key="not-a-public-key",
            )
            with mock.patch.dict(
                supply.os.environ,
                {
                    "VAULT_ADDR": "https://vault.example.test",
                    "VAULT_TOKEN": "vault-token-value",
                    "VAULT_CACERT": str(vault_ca_path),
                },
            ), mock.patch.object(supply, "_run_tool") as run_tool, self.assertRaises(
                supply.common.ReleaseGateError
            ) as caught:
                supply._sign_and_verify(
                    supply_options(
                        state_dir,
                        registry_auth_username_environment=supply.PRODUCTION_REGISTRY_USERNAME_ENV,
                        registry_auth_password_environment=supply.PRODUCTION_REGISTRY_PASSWORD_ENV,
                        registry_ca_cert_environment=supply.PRODUCTION_REGISTRY_CA_CERT_ENV,
                        production_public_key_configmap_path=public_key_path,
                        production_repository_configmap_path=repository_path,
                    ),
                    configuration,
                    builds=[{"slot": "cached", "registryDigest": DIGEST}],
                    git_sha="a" * 40,
                    version="0.5.4",
                    run_id="run-1",
                    deadline=supply.time.monotonic() + 60,
                    redactor=acceptance.SecretRedactor(),
                )

        self.assertEqual(
            caught.exception.code,
            "release.registry_production_admission_input_invalid",
        )
        self.assertEqual(run_tool.call_count, 0)

    def test_production_runtime_admission_rejects_repository_pattern_drift(self) -> None:
        policy = signing_policy(
            path=str(supply.PRODUCTION_SIGNING_POLICY_PATH),
            mode="kms-key",
            require_transparency_log=True,
            key_reference=supply.VAULT_TRANSIT_KEY_REFERENCE,
            credential_environment=supply.VAULT_TRANSIT_REQUIRED_ENVIRONMENT,
        )
        configuration = production_configuration_with(policy)
        with tempfile.TemporaryDirectory() as directory:
            state_dir = pathlib.Path(directory) / "state"
            vault_ca_path = pathlib.Path(directory) / "vault-ca.pem"
            vault_ca_path.write_bytes(
                b"-----BEGIN CERTIFICATE-----\nfixture\n-----END CERTIFICATE-----\n"
            )
            public_key_path, repository_path = write_runtime_configmaps(
                pathlib.Path(directory),
                repository_pattern="registry.example.test/synara/other*",
            )

            def run_tool(_options: Any, **kwargs: Any) -> subprocess.CompletedProcess[str]:
                arguments = kwargs["arguments"]
                stdout = PUBLIC_KEY_PEM if arguments[0] == "public-key" else ""
                return subprocess.CompletedProcess(arguments, 0, stdout=stdout, stderr="")

            with mock.patch.dict(
                supply.os.environ,
                {
                    "VAULT_ADDR": "https://vault.example.test",
                    "VAULT_TOKEN": "vault-token-value",
                    "VAULT_CACERT": str(vault_ca_path),
                },
            ), mock.patch.object(supply, "_run_tool", side_effect=run_tool) as patched_run_tool, self.assertRaises(
                supply.common.ReleaseGateError
            ) as caught:
                supply._sign_and_verify(
                    supply_options(
                        state_dir,
                        registry_auth_username_environment=supply.PRODUCTION_REGISTRY_USERNAME_ENV,
                        registry_auth_password_environment=supply.PRODUCTION_REGISTRY_PASSWORD_ENV,
                        registry_ca_cert_environment=supply.PRODUCTION_REGISTRY_CA_CERT_ENV,
                        production_public_key_configmap_path=public_key_path,
                        production_repository_configmap_path=repository_path,
                    ),
                    configuration,
                    builds=[{"slot": "cached", "registryDigest": DIGEST}],
                    git_sha="a" * 40,
                    version="0.5.4",
                    run_id="run-1",
                    deadline=supply.time.monotonic() + 60,
                    redactor=acceptance.SecretRedactor(),
                )

        self.assertEqual(
            caught.exception.code,
            "release.registry_production_admission_input_invalid",
        )
        self.assertEqual(patched_run_tool.call_count, 1)

    def test_verify_supply_chain_cleans_registry_and_vault_ca_state_and_redacts_report(self) -> None:
        token = "vault-token-value"
        address = "https://vault.example.test"
        registry_username = "registry-user"
        registry_password = "registry-password"
        policy = signing_policy(
            path=str(supply.PRODUCTION_SIGNING_POLICY_PATH),
            mode="kms-key",
            require_transparency_log=True,
            key_reference=supply.VAULT_TRANSIT_KEY_REFERENCE,
            credential_environment=supply.VAULT_TRANSIT_REQUIRED_ENVIRONMENT,
        )
        configuration = configuration_with(policy)
        reference = f"registry.example.test/synara/worker@{DIGEST}"
        with tempfile.TemporaryDirectory() as directory:
            state_dir = pathlib.Path(directory) / "state"
            vault_ca_path = pathlib.Path(directory) / "vault-ca.pem"
            registry_ca_path = pathlib.Path(directory) / "registry-ca.pem"
            ca_bytes = b"-----BEGIN CERTIFICATE-----\nfixture\n-----END CERTIFICATE-----\n"
            vault_ca_path.write_bytes(ca_bytes)
            registry_ca_path.write_bytes(ca_bytes)

            def run_tool(_options: Any, **kwargs: Any) -> subprocess.CompletedProcess[str]:
                secret_environment = kwargs["secret_environment"]
                self.assertEqual(secret_environment["VAULT_CACERT"], "/workspace/vault/ca-certificates/vault-ca.crt")
                arguments = kwargs["arguments"]
                if arguments[0] == "sign":
                    write_classic_signing_outputs(state_dir, arguments)
                if arguments[:2] == ["bundle", "create"]:
                    write_bundle_create_output(state_dir, arguments)
                stdout = json.dumps(verification_payload(reference=reference)) if arguments[0] == "verify" else ""
                return subprocess.CompletedProcess(arguments, 0, stdout=stdout, stderr="")

            redactor = acceptance.SecretRedactor()
            with (
                mock.patch.dict(
                    supply.os.environ,
                    {
                        "VAULT_ADDR": address,
                        "VAULT_TOKEN": token,
                        "VAULT_CACERT": str(vault_ca_path),
                        "REGISTRY_USER_ENV": registry_username,
                        "REGISTRY_PASSWORD_ENV": registry_password,
                        "REGISTRY_CA_ENV": str(registry_ca_path),
                    },
                ),
                mock.patch.object(supply, "_run_tool", side_effect=run_tool),
                mock.patch.object(
                    supply,
                    "_tool_versions",
                    return_value={"cosign": "v3.1.1", "trivy": "0.72.0"},
                ),
                mock.patch.object(
                    supply,
                    "_scan_platforms",
                    return_value=({"scans": [], "database": {}, "policy": {}}, []),
                ),
            ):
                result = supply.verify_supply_chain(
                    supply_options(
                        state_dir,
                        registry_auth_username_environment="REGISTRY_USER_ENV",
                        registry_auth_password_environment="REGISTRY_PASSWORD_ENV",
                        registry_ca_cert_environment="REGISTRY_CA_ENV",
                    ),
                    configuration,
                    builds=[
                        {
                            "slot": "cached",
                            "registryDigest": DIGEST,
                            "platformDigests": {
                                "linux/amd64": DIGEST,
                                "linux/arm64": "sha256:" + "b" * 64,
                            },
                        }
                    ],
                    git_sha="a" * 40,
                    version="0.5.4",
                    run_id="run-1",
                    redactor=redactor,
                )

            self.assertFalse((state_dir / supply.MATERIALIZED_VAULT_CACERT_RELATIVE_PATH).exists())
            self.assertFalse((state_dir / "registry-access/docker-config/config.json").exists())
            self.assertFalse(
                (
                    state_dir
                    / "registry-access/docker-config/certs.d/registry.example.test/ca.crt"
                ).exists()
            )
            self.assertTrue(result["cleanup"]["vaultCaRemoved"])
            self.assertTrue(result["cleanup"]["registryAuthConfigRemoved"])
            self.assertTrue(result["cleanup"]["registryCaRemoved"])
            self.assertTrue(result["signing"]["vaultCaRemoved"])
            self.assertTrue(result["signing"]["registryAuthConfigRemoved"])
            self.assertTrue(result["signing"]["registryCaRemoved"])
            serialized = json.dumps(redactor.value(result), sort_keys=True)
            self.assertNotIn(str(vault_ca_path), serialized)
            self.assertNotIn(str(registry_ca_path), serialized)
            self.assertNotIn(registry_password, serialized)

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
