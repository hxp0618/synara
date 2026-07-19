from __future__ import annotations

import contextlib
import dataclasses
import datetime as dt
import hashlib
import io
import json
import pathlib
import subprocess
import tempfile
import unittest
from collections.abc import Callable
from typing import Any
from unittest import mock

import registry_release_gate as gate


REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
GIT_SHA = "a" * 40
VERSION = "0.0.0"
SOURCE_DATE_EPOCH = "1700000000"
INDEX_DIGESTS = {
    "linux/amd64": "sha256:" + "a" * 64,
    "linux/arm64": "sha256:" + "b" * 64,
}
ATTESTATION_DIGESTS = {
    "linux/amd64": "sha256:" + "c" * 64,
    "linux/arm64": "sha256:" + "d" * 64,
}
EMBEDDED_LOCK_FILE_NAMES = {
    "provider-tools-npm": "providerToolsLock",
    "provider-host-bun": "providerHostLock",
    "worker-apk": "workerAPKLock",
}


def options(
    output_dir: pathlib.Path,
    *,
    repo_root: pathlib.Path = REPO_ROOT,
    signing_policy_profile: str = "disposable",
    insecure_registry: bool | None = None,
    registry_auth_username_environment: str | None = None,
    registry_auth_password_environment: str | None = None,
    registry_ca_cert_environment: str | None = None,
    production_public_key_configmap_path: pathlib.Path | None = None,
    production_repository_configmap_path: pathlib.Path | None = None,
    production_registry_config_path: pathlib.Path | None = None,
    production_registry_retention_policy_path: pathlib.Path | None = None,
    production_registry_container: str | None = None,
    production_registry_runtime_config_path: str | None = None,
) -> gate.RegistryReleaseGateOptions:
    if insecure_registry is None:
        insecure_registry = signing_policy_profile != "production"
    if signing_policy_profile == "production":
        registry_auth_username_environment = (
            registry_auth_username_environment or gate.supply_chain.PRODUCTION_REGISTRY_USERNAME_ENV
        )
        registry_auth_password_environment = (
            registry_auth_password_environment or gate.supply_chain.PRODUCTION_REGISTRY_PASSWORD_ENV
        )
        registry_ca_cert_environment = (
            registry_ca_cert_environment or gate.supply_chain.PRODUCTION_REGISTRY_CA_CERT_ENV
        )
        production_public_key_configmap_path = production_public_key_configmap_path or pathlib.Path(
            "/tmp/production-public-key-configmap.yaml"
        )
        production_repository_configmap_path = (
            production_repository_configmap_path
            or pathlib.Path("/tmp/production-repository-configmap.yaml")
        )
        production_registry_config_path = production_registry_config_path or pathlib.Path(
            "/tmp/production-registry-config.yaml"
        )
        production_registry_retention_policy_path = (
            production_registry_retention_policy_path
            or pathlib.Path("/tmp/production-registry-retention-policy.json")
        )
        production_registry_container = production_registry_container or "synara-production-registry"
        production_registry_runtime_config_path = (
            production_registry_runtime_config_path or "/etc/distribution/config.yml"
        )
    return gate.RegistryReleaseGateOptions(
        repo_root=repo_root,
        output_dir=output_dir,
        image_repository="localhost:55091/synara/worker",
        builder="synara-stage3-registry-builder",
        build_timeout_seconds=7200.0,
        supply_chain_timeout_seconds=1800.0,
        docker_bin="docker",
        go_proxy="https://goproxy.cn,direct",
        insecure_registry=insecure_registry,
        signing_policy_profile=signing_policy_profile,
        registry_auth_username_environment=registry_auth_username_environment,
        registry_auth_password_environment=registry_auth_password_environment,
        registry_ca_cert_environment=registry_ca_cert_environment,
        production_public_key_configmap_path=production_public_key_configmap_path,
        production_repository_configmap_path=production_repository_configmap_path,
        production_registry_config_path=production_registry_config_path,
        production_registry_retention_policy_path=production_registry_retention_policy_path,
        production_registry_container=production_registry_container,
        production_registry_runtime_config_path=production_registry_runtime_config_path,
    )


def live_runtime_evidence(
    gate_options: gate.RegistryReleaseGateOptions,
    *,
    exported_config_sha256: str,
    live_policy_sha256: str,
    checked_in_policy_sha256: str,
    collected_at: str | None = None,
    registry_host: str | None = None,
    registry_authority: str | None = None,
    repository_authority: str | None = None,
    tls_peer_certificate_sha256: str = "f" * 64,
    repository_probe_status: int = 404,
    runtime_config_path: str | None = None,
    runtime_config_sha256: str | None = None,
    exported_runtime_config_sha256: str | None = None,
    container_name: str = "synara-production-registry",
    container_id: str = "a" * 64,
    expected_image_reference: str | None = None,
    expected_image_digest: str | None = None,
    config_image_reference: str | None = None,
    runtime_image_id: str = "sha256:" + "b" * 64,
    matched_repo_digest: str | None = None,
    container_started_at: str | None = None,
) -> dict[str, Any]:
    registry_host = registry_host or gate_options.image_repository.split("/", 1)[0]
    registry_authority = registry_authority or f"https://{registry_host}"
    repository_authority = repository_authority or gate_options.image_repository
    runtime_config_path = runtime_config_path or gate_options.production_registry_runtime_config_path
    runtime_config_sha256 = runtime_config_sha256 or exported_config_sha256
    exported_runtime_config_sha256 = exported_runtime_config_sha256 or exported_config_sha256
    collected_at = collected_at or dt.datetime.now(dt.timezone.utc).isoformat()
    container_started_at = container_started_at or dt.datetime.now(dt.timezone.utc).isoformat()
    image_contract = gate._production_registry_runtime_image_contract(
        gate.PRODUCTION_REGISTRY_RUNTIME_IMAGE_REFERENCE
    )
    expected_image_reference = expected_image_reference or image_contract["reference"]
    expected_image_digest = expected_image_digest or image_contract["digest"]
    config_image_reference = config_image_reference or image_contract["reference"]
    matched_repo_digest = matched_repo_digest or image_contract["repoDigest"]
    evidence = {
        "collectedAt": collected_at,
        "registryHost": registry_host,
        "registryAuthority": registry_authority,
        "repositoryAuthority": repository_authority,
        "repositoryProbeStatus": repository_probe_status,
        "tlsPeerCertificateSha256": tls_peer_certificate_sha256,
        "runtimeConfigPath": runtime_config_path,
        "runtimeConfigSha256": runtime_config_sha256,
        "exportedConfigSha256": exported_runtime_config_sha256,
        "retentionPolicySha256": live_policy_sha256,
        "checkedInRetentionPolicySha256": checked_in_policy_sha256,
        "container": {
            "name": container_name,
            "id": container_id,
            "image": {
                "expectedReference": expected_image_reference,
                "expectedDigest": expected_image_digest,
                "configReference": config_image_reference,
                "runtimeId": runtime_image_id,
                "matchedRepoDigest": matched_repo_digest,
            },
            "startedAt": container_started_at,
        },
    }
    evidence["runtimeEvidenceSha256"] = gate._stable_json_sha256(evidence)
    return evidence


def image_config(platform: str, *, extra_environment: list[str] | None = None) -> dict[str, Any]:
    os_name, architecture = platform.split("/", 1)
    environment = [
        f"{name}={value}" for name, value in gate.EXPECTED_ENVIRONMENT.items()
    ]
    environment.extend(extra_environment or [])
    return {
        "os": os_name,
        "architecture": architecture,
        "created": gate.expected_created_at(SOURCE_DATE_EPOCH),
        "config": {
            "User": gate.EXPECTED_USER,
            "Entrypoint": list(gate.EXPECTED_ENTRYPOINT),
            "Cmd": None,
            "WorkingDir": gate.EXPECTED_WORKING_DIRECTORY,
            "Env": environment,
            "Labels": {
                "org.opencontainers.image.title": "Synara Worker",
                "org.opencontainers.image.version": VERSION,
                "org.opencontainers.image.revision": GIT_SHA,
            },
        },
        "rootfs": {"diff_ids": ["sha256:" + "e" * 64]},
    }


def attestation_manifest() -> dict[str, Any]:
    return {
        "schemaVersion": 2,
        "mediaType": gate.OCI_MANIFEST_MEDIA_TYPE,
        "layers": [
            {
                "annotations": {
                    "in-toto.io/predicate-type": gate.SPDX_PREDICATE,
                }
            },
            {
                "annotations": {
                    "in-toto.io/predicate-type": f"{gate.SLSA_PREDICATE_PREFIX}v0.2",
                }
            },
        ],
    }


def index_payload(*, attestation_mode: str = "complete") -> dict[str, Any]:
    manifests: list[dict[str, Any]] = [
        {
            "mediaType": gate.OCI_MANIFEST_MEDIA_TYPE,
            "digest": digest,
            "platform": {
                "os": platform.split("/", 1)[0],
                "architecture": platform.split("/", 1)[1],
            },
        }
        for platform, digest in INDEX_DIGESTS.items()
    ]
    attestations = [
        {
            "mediaType": gate.OCI_MANIFEST_MEDIA_TYPE,
            "digest": ATTESTATION_DIGESTS[platform],
            "platform": {"os": "unknown", "architecture": "unknown"},
            "annotations": {
                "vnd.docker.reference.type": gate.ATTESTATION_TYPE,
                "vnd.docker.reference.digest": INDEX_DIGESTS[platform],
            },
        }
        for platform in gate.REQUIRED_PLATFORMS
    ]
    if attestation_mode == "missing":
        attestations.pop()
    elif attestation_mode == "duplicate":
        attestations.append(dict(attestations[0]))
    manifests.extend(attestations)
    return {
        "schemaVersion": 2,
        "mediaType": gate.OCI_INDEX_MEDIA_TYPE,
        "manifests": manifests,
    }


def registry_inspector(
    *,
    attestation_mode: str = "complete",
    mutate_attestation: Callable[[dict[str, Any]], None] | None = None,
) -> tuple[Callable[..., tuple[dict[str, Any], bytes]], str]:
    index = index_payload(attestation_mode=attestation_mode)
    raw_index = json.dumps(index, separators=(",", ":")).encode("utf-8")
    expected_digest = "sha256:" + hashlib.sha256(raw_index).hexdigest()

    def inspect(
        _options: gate.RegistryReleaseGateOptions,
        arguments: list[str],
        **_kwargs: Any,
    ) -> tuple[dict[str, Any], bytes]:
        reference = arguments[-1]
        if arguments[-2:] == ["--raw", "example.invalid/synara/worker:tag"]:
            return index, raw_index
        if reference.startswith("localhost:55091/synara/worker@"):
            payload = attestation_manifest()
            if mutate_attestation is not None:
                mutate_attestation(payload)
            return payload, json.dumps(payload).encode("utf-8")
        if arguments[-2:] == ["--format", "{{json .Image}}"]:
            payload = {
                platform: image_config(platform) for platform in gate.REQUIRED_PLATFORMS
            }
            return payload, json.dumps(payload).encode("utf-8")
        raise AssertionError(f"unexpected inspect arguments: {arguments}")

    return inspect, expected_digest


def write_embedded_fixture(
    root: pathlib.Path,
    *,
    mutate_manifest: Callable[[dict[str, Any]], None] | None = None,
    mutate_sbom: Callable[[dict[str, Any]], None] | None = None,
) -> dict[str, pathlib.Path]:
    paths = {name: root / name for name in gate.EMBEDDED_PATHS}
    for path in paths.values():
        path.parent.mkdir(parents=True, exist_ok=True)

    runtimes = gate._expected_provider_runtimes(REPO_ROOT)
    for name, local_path in gate.LOCAL_LOCK_PATHS.items():
        embedded_name = EMBEDDED_LOCK_FILE_NAMES[name]
        paths[embedded_name].write_bytes((REPO_ROOT / local_path).read_bytes())

    sbom = {
        "spdxVersion": "SPDX-2.3",
        "creationInfo": {"created": gate.expected_sbom_created_at(SOURCE_DATE_EPOCH)},
        "packages": [
            {"name": runtime["package"], "versionInfo": runtime["version"]}
            for runtime in runtimes
            if runtime["kind"] == "cli"
        ],
    }
    if mutate_sbom is not None:
        mutate_sbom(sbom)
    sbom_bytes = (json.dumps(sbom, sort_keys=True) + "\n").encode("utf-8")
    paths["sbom"].write_bytes(sbom_bytes)

    lockfiles = []
    for name, local_path in gate.LOCAL_LOCK_PATHS.items():
        embedded_name = EMBEDDED_LOCK_FILE_NAMES[name]
        lockfiles.append(
            {
                "name": name,
                "path": gate.EMBEDDED_PATHS[embedded_name],
                "sha256": hashlib.sha256(paths[embedded_name].read_bytes()).hexdigest(),
            }
        )
    manifest = {
        "schemaVersion": 1,
        "source": {"version": VERSION, "gitSha": GIT_SHA},
        "platform": {"os": "linux", "architecture": "amd64"},
        "baseImages": [
            {"name": name, "reference": f"example.invalid/{name}@sha256:{index * 64}"}
            for index, name in zip("123", ("agentd-build", "provider-host-build", "worker-runtime"))
        ],
        "lockfiles": lockfiles,
        "providerRuntimes": runtimes,
        "sboms": [
            {
                "name": "provider-tools",
                "format": "spdx-json",
                "path": gate.EMBEDDED_PATHS["sbom"],
                "sha256": hashlib.sha256(sbom_bytes).hexdigest(),
            }
        ],
    }
    if mutate_manifest is not None:
        mutate_manifest(manifest)
    paths["manifest"].write_text(
        json.dumps(manifest, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    paths["providerHost"].write_bytes(b"provider-host-bundle")
    paths["agentd"].write_bytes(b"agentd-binary")
    paths["buildRevision"].write_text(f"{GIT_SHA}\n", encoding="utf-8")
    paths["providerHostWrapper"].write_text(
        '#!/bin/sh\nexec node /opt/synara/provider-host/index.mjs "$@"\n',
        encoding="utf-8",
    )
    return paths


def sample_build(slot: str, no_cache: bool) -> dict[str, Any]:
    return {
        "slot": slot,
        "noCache": no_cache,
        "registryDigest": "sha256:" + ("1" if no_cache else "0") * 64,
        "platformDigests": dict(INDEX_DIGESTS),
        "embedded": {
            platform: {"providerHostSha256": "f" * 64}
            for platform in gate.REQUIRED_PLATFORMS
        },
    }


def sample_supply_chain() -> dict[str, Any]:
    return {
        "status": "pass",
        "mode": "registry-supply-chain",
        "tools": {"versions": {"cosign": "v3.1.1", "trivy": "0.72.0"}},
        "signing": {
            "mode": "ephemeral-key",
            "productionSigningPolicySatisfied": False,
            "signatures": [{"slot": "cached"}, {"slot": "no-cache"}],
        },
        "vulnerability": {
            "scans": [
                {
                    "platform": platform,
                    "vulnerabilities": {
                        "total": 0,
                        "bySeverity": {
                            severity: 0 for severity in gate.supply_chain.SUPPORTED_SEVERITIES
                        },
                    },
                    "secretFindingCount": 0,
                    "os": {"EOSL": False},
                }
                for platform in gate.REQUIRED_PLATFORMS
            ]
        },
        "cleanup": {"ephemeralPrivateKeyRemoved": True, "broadCleanupUsed": False},
        "errors": [],
    }


def production_supply_chain() -> dict[str, Any]:
    report = sample_supply_chain()
    report["signing"] = {
        **report["signing"],
        "mode": "kms-key",
        "productionSigningPolicySatisfied": True,
        "signerIdentity": gate._expected_production_signer_identity(),
    }
    return report


class InputValidationTest(unittest.TestCase):
    def test_configuration_requires_checked_in_signing_and_vulnerability_policies(self) -> None:
        evidence = gate.configuration_evidence(options(pathlib.Path("/tmp/output")))

        self.assertTrue(evidence["signingPolicyRequired"])
        self.assertEqual(evidence["signingPolicyProfile"], "disposable")
        self.assertEqual(evidence["signingPolicy"], "deploy/worker/signing-policy.json")
        self.assertEqual(evidence["vulnerabilityPolicy"], "deploy/worker/vulnerability-policy.json")
        self.assertNotIn("ephemeralDigestSigning", evidence)

    def test_configuration_can_select_production_signing_profile(self) -> None:
        evidence = gate.configuration_evidence(
            options(pathlib.Path("/tmp/output"), signing_policy_profile="production")
        )

        self.assertEqual(evidence["signingPolicyProfile"], "production")
        self.assertEqual(
            evidence["signingPolicy"],
            "deploy/worker/production-signing-policy.json",
        )
        self.assertEqual(
            evidence["productionSigningProfile"],
            "deploy/worker/production-signing-profile.json",
        )
        self.assertEqual(
            evidence["productionAdmissionInputs"],
            {
                "publicKeyConfigMap": "/tmp/production-public-key-configmap.yaml",
                "repositoryConfigMap": "/tmp/production-repository-configmap.yaml",
            },
        )
        self.assertEqual(
            evidence["productionRegistryBoundaryInputs"],
            {
                "registryConfig": "/tmp/production-registry-config.yaml",
                "retentionPolicy": "/tmp/production-registry-retention-policy.json",
            },
        )
        self.assertEqual(
            evidence["productionRegistryRuntimeEvidenceInputs"],
            {
                "container": "synara-production-registry",
                "runtimeConfigPath": "/etc/distribution/config.yml",
            },
        )
        self.assertEqual(
            evidence["registryAccess"],
            {
                "usernameEnvironment": gate.supply_chain.PRODUCTION_REGISTRY_USERNAME_ENV,
                "passwordEnvironment": gate.supply_chain.PRODUCTION_REGISTRY_PASSWORD_ENV,
                "caCertEnvironment": gate.supply_chain.PRODUCTION_REGISTRY_CA_CERT_ENV,
            },
        )

    def test_parse_args_requires_production_runtime_configmaps(self) -> None:
        with self.assertRaises(SystemExit):
            gate.parse_args(
                [
                    "--image-repository",
                    "localhost:55091/synara/worker",
                    "--builder",
                    "synara-stage3-registry-builder",
                    "--signing-policy-profile",
                    "production",
                    "--production-registry-container",
                    "synara-production-registry",
                    "--production-registry-runtime-config-path",
                    "/etc/distribution/config.yml",
                ]
            )

    def test_parse_args_requires_production_registry_boundary_inputs(self) -> None:
        with self.assertRaises(SystemExit):
            gate.parse_args(
                [
                    "--image-repository",
                    "localhost:55091/synara/worker",
                    "--builder",
                    "synara-stage3-registry-builder",
                    "--signing-policy-profile",
                    "production",
                    "--production-public-key-configmap",
                    "/tmp/public-key.yaml",
                    "--production-repository-configmap",
                    "/tmp/repository.yaml",
                    "--production-registry-container",
                    "synara-production-registry",
                    "--production-registry-runtime-config-path",
                    "/etc/distribution/config.yml",
                ]
            )

    def test_parse_args_rejects_production_insecure_registry(self) -> None:
        with self.assertRaises(SystemExit):
            gate.parse_args(
                [
                    "--image-repository",
                    "localhost:55091/synara/worker",
                    "--builder",
                    "synara-stage3-registry-builder",
                    "--signing-policy-profile",
                    "production",
                    "--insecure-registry",
                    "--production-public-key-configmap",
                    "/tmp/public-key.yaml",
                    "--production-repository-configmap",
                    "/tmp/repository.yaml",
                    "--production-registry-container",
                    "synara-production-registry",
                    "--production-registry-runtime-config-path",
                    "/etc/distribution/config.yml",
                ]
            )

    def test_parse_args_rejects_production_runtime_configmaps_for_disposable_profile(self) -> None:
        with self.assertRaises(SystemExit):
            gate.parse_args(
                [
                    "--image-repository",
                    "localhost:55091/synara/worker",
                    "--builder",
                    "synara-stage3-registry-builder",
                    "--production-public-key-configmap",
                    "/tmp/public-key.yaml",
                    "--production-repository-configmap",
                    "/tmp/repository.yaml",
                ]
            )

    def test_parse_args_rejects_production_registry_boundary_inputs_for_disposable_profile(self) -> None:
        with self.assertRaises(SystemExit):
            gate.parse_args(
                [
                    "--image-repository",
                    "localhost:55091/synara/worker",
                    "--builder",
                    "synara-stage3-registry-builder",
                    "--production-registry-config",
                    "/tmp/registry.yaml",
                    "--production-registry-retention-policy",
                    "/tmp/retention-policy.json",
                ]
            )

    def test_parse_args_requires_production_runtime_container_inputs(self) -> None:
        with self.assertRaises(SystemExit):
            gate.parse_args(
                [
                    "--image-repository",
                    "localhost:55091/synara/worker",
                    "--builder",
                    "synara-stage3-registry-builder",
                    "--signing-policy-profile",
                    "production",
                    "--production-public-key-configmap",
                    "/tmp/public-key.yaml",
                    "--production-repository-configmap",
                    "/tmp/repository.yaml",
                    "--production-registry-config",
                    "/tmp/registry.yaml",
                    "--production-registry-retention-policy",
                    "/tmp/retention-policy.json",
                ]
            )

    def test_parse_args_defaults_production_registry_access_environment_names(self) -> None:
        parsed = gate.parse_args(
            [
                "--image-repository",
                "localhost:55091/synara/worker",
                "--builder",
                "synara-stage3-registry-builder",
                "--signing-policy-profile",
                "production",
                "--production-public-key-configmap",
                "/tmp/public-key.yaml",
                "--production-repository-configmap",
                "/tmp/repository.yaml",
                "--production-registry-config",
                "/tmp/registry.yaml",
                "--production-registry-retention-policy",
                "/tmp/retention-policy.json",
                "--production-registry-container",
                "synara-production-registry",
                "--production-registry-runtime-config-path",
                "/etc/distribution/config.yml",
            ]
        )

        self.assertEqual(
            (
                parsed.registry_auth_username_environment,
                parsed.registry_auth_password_environment,
                parsed.registry_ca_cert_environment,
            ),
            gate.supply_chain.PRODUCTION_REGISTRY_ACCESS_ENVIRONMENT,
        )

    def test_parse_args_rejects_production_registry_access_environment_drift(self) -> None:
        with self.assertRaises(SystemExit):
            gate.parse_args(
                [
                    "--image-repository",
                    "localhost:55091/synara/worker",
                    "--builder",
                    "synara-stage3-registry-builder",
                    "--signing-policy-profile",
                    "production",
                    "--registry-auth-username-env",
                    "REGISTRY_USER_ENV",
                    "--production-public-key-configmap",
                    "/tmp/public-key.yaml",
                    "--production-repository-configmap",
                    "/tmp/repository.yaml",
                    "--production-registry-config",
                    "/tmp/registry.yaml",
                    "--production-registry-retention-policy",
                    "/tmp/retention-policy.json",
                    "--production-registry-container",
                    "synara-production-registry",
                    "--production-registry-runtime-config-path",
                    "/etc/distribution/config.yml",
                ]
            )

    def test_production_boundary_rejects_programmatic_registry_access_environment_drift(self) -> None:
        with self.assertRaises(gate.ReleaseGateError) as caught:
            gate._validate_production_boundary(
                options(
                    pathlib.Path("/tmp/output"),
                    signing_policy_profile="production",
                    registry_auth_username_environment="REGISTRY_USER_ENV",
                    registry_auth_password_environment="REGISTRY_PASSWORD_ENV",
                    registry_ca_cert_environment="REGISTRY_CA_ENV",
                )
            )

        self.assertEqual(caught.exception.code, "release.registry_production_boundary_invalid")

    def test_host_registry_environment_uses_isolated_docker_config_and_ca_layout(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            state_dir = pathlib.Path(directory) / "state"
            ca_path = pathlib.Path(directory) / "registry-ca.pem"
            ca_bytes = b"-----BEGIN CERTIFICATE-----\nfixture\n-----END CERTIFICATE-----\n"
            ca_path.write_bytes(ca_bytes)
            redactor = gate.acceptance.SecretRedactor()
            with mock.patch.dict(
                gate.supply_chain.os.environ,
                {
                    gate.supply_chain.PRODUCTION_REGISTRY_USERNAME_ENV: "registry-user",
                    gate.supply_chain.PRODUCTION_REGISTRY_PASSWORD_ENV: "registry-password",
                    gate.supply_chain.PRODUCTION_REGISTRY_CA_CERT_ENV: str(ca_path),
                },
            ):
                environment = gate._prepare_host_registry_environment(
                    options(
                        pathlib.Path(directory) / "output",
                        signing_policy_profile="production",
                        registry_auth_username_environment=gate.supply_chain.PRODUCTION_REGISTRY_USERNAME_ENV,
                        registry_auth_password_environment=gate.supply_chain.PRODUCTION_REGISTRY_PASSWORD_ENV,
                        registry_ca_cert_environment=gate.supply_chain.PRODUCTION_REGISTRY_CA_CERT_ENV,
                    ),
                    state_dir=state_dir,
                    redactor=redactor,
                )
            docker_config_dir = state_dir / "host-registry-access/registry-access/docker-config"
            self.assertEqual(environment, {"DOCKER_CONFIG": str(docker_config_dir)})
            self.assertTrue((docker_config_dir / "config.json").is_file())
            self.assertTrue((docker_config_dir / "certs.d/localhost:55091/ca.crt").is_file())

    def test_host_tool_helpers_apply_environment_overrides(self) -> None:
        completed = subprocess.CompletedProcess(["docker"], 0, stdout=b"{}", stderr=b"")
        with mock.patch.object(gate.subprocess, "run", return_value=completed) as run:
            gate._json_tool_output(
                dataclasses.replace(
                    options(pathlib.Path("/tmp/output")),
                    tool_environment_overrides={"DOCKER_CONFIG": "/tmp/docker-config"},
                ),
                ["buildx", "imagetools", "inspect", "--raw", "example.invalid/synara/worker:tag"],
                code="release.registry_index_invalid",
                message="inspect failed",
            )

        self.assertEqual(run.call_args.kwargs["env"]["DOCKER_CONFIG"], "/tmp/docker-config")

    def test_validates_production_registry_retention_boundary(self) -> None:
        checked_in_policy = json.loads(
            (REPO_ROOT / gate.PRODUCTION_REGISTRY_RETENTION_POLICY_PATH).read_text(encoding="utf-8")
        )
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            live_config = root / "registry-config.yml"
            live_policy = root / "retention-policy.json"
            live_config.write_text(
                "\n".join(
                    [
                        "storage:",
                        "  filesystem:",
                        "    rootdirectory: /var/lib/registry",
                        "  delete:",
                        "    enabled: false",
                        "http:",
                        "  host: https://localhost:55091",
                        "  tls:",
                        "    certificate: /certs/tls.crt",
                    ]
                )
                + "\n",
                encoding="utf-8",
            )
            live_policy.write_text(json.dumps(checked_in_policy), encoding="utf-8")

            exported_config_sha256 = hashlib.sha256(live_config.read_bytes()).hexdigest()
            checked_in_policy_sha256 = gate._stable_json_sha256(checked_in_policy)
            evidence = gate.validate_production_registry_boundary(
                options(
                    root / "output",
                    signing_policy_profile="production",
                    production_registry_config_path=live_config,
                    production_registry_retention_policy_path=live_policy,
                ),
                runtime_evidence=live_runtime_evidence(
                    options(
                        root / "output",
                        signing_policy_profile="production",
                        production_registry_config_path=live_config,
                        production_registry_retention_policy_path=live_policy,
                    ),
                    exported_config_sha256=exported_config_sha256,
                    live_policy_sha256=checked_in_policy_sha256,
                    checked_in_policy_sha256=checked_in_policy_sha256,
                ),
            )

        self.assertEqual(evidence["deleteEnabled"], False)
        self.assertEqual(evidence["promotionBoundary"], "digest-only")
        self.assertEqual(
            evidence["liveRuntimeEvidence"]["repositoryAuthority"],
            "localhost:55091/synara/worker",
        )
        self.assertEqual(
            evidence["liveRuntimeEvidence"]["container"]["image"],
            {
                "expectedReference": gate.PRODUCTION_REGISTRY_RUNTIME_IMAGE_REFERENCE,
                "expectedDigest": "sha256:"
                "a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373",
                "configReference": gate.PRODUCTION_REGISTRY_RUNTIME_IMAGE_REFERENCE,
                "runtimeId": "sha256:" + "b" * 64,
                "matchedRepoDigest": "registry@sha256:"
                "a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373",
            },
        )

    def test_live_registry_runtime_image_requires_exact_config_reference(self) -> None:
        runtime_image_id = "sha256:" + "b" * 64
        for config_reference in (
            "registry:2.8.3",
            "example.invalid/registry:2.8.3@sha256:"
            "a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373",
            "registry:2.8.3@sha256:" + "c" * 64,
        ):
            with self.subTest(config_reference=config_reference), self.assertRaises(
                gate.ReleaseGateError
            ) as caught:
                gate._inspect_live_registry_runtime_image(
                    options(pathlib.Path("/tmp/output"), signing_policy_profile="production"),
                    {
                        "Config": {"Image": config_reference},
                        "Image": runtime_image_id,
                    },
                    expected_reference=gate.PRODUCTION_REGISTRY_RUNTIME_IMAGE_REFERENCE,
                )

            self.assertEqual(
                caught.exception.code,
                "release.registry_production_boundary_invalid",
            )

    def test_live_registry_runtime_image_binds_image_id_and_repo_digest(self) -> None:
        runtime_image_id = "sha256:" + "b" * 64
        image_contract = gate._production_registry_runtime_image_contract(
            gate.PRODUCTION_REGISTRY_RUNTIME_IMAGE_REFERENCE
        )
        container_payload = {
            "Config": {"Image": gate.PRODUCTION_REGISTRY_RUNTIME_IMAGE_REFERENCE},
            "Image": runtime_image_id,
        }
        gate_options = options(
            pathlib.Path("/tmp/output"),
            signing_policy_profile="production",
        )
        with mock.patch.object(
            gate,
            "_json_tool_output",
            return_value=(
                [
                    {
                        "Id": runtime_image_id,
                        "RepoDigests": [image_contract["repoDigest"]],
                    }
                ],
                b"[]",
            ),
        ):
            evidence = gate._inspect_live_registry_runtime_image(
                gate_options,
                container_payload,
                expected_reference=gate.PRODUCTION_REGISTRY_RUNTIME_IMAGE_REFERENCE,
            )

        self.assertEqual(
            evidence,
            {
                "expectedReference": image_contract["reference"],
                "expectedDigest": image_contract["digest"],
                "configReference": image_contract["reference"],
                "runtimeId": runtime_image_id,
                "matchedRepoDigest": image_contract["repoDigest"],
            },
        )

    def test_live_registry_runtime_image_rejects_id_or_repo_digest_mismatch(self) -> None:
        runtime_image_id = "sha256:" + "b" * 64
        image_contract = gate._production_registry_runtime_image_contract(
            gate.PRODUCTION_REGISTRY_RUNTIME_IMAGE_REFERENCE
        )
        container_payload = {
            "Config": {"Image": image_contract["reference"]},
            "Image": runtime_image_id,
        }
        mismatches = (
            {
                "Id": "sha256:" + "c" * 64,
                "RepoDigests": [image_contract["repoDigest"]],
            },
            {
                "Id": runtime_image_id,
                "RepoDigests": ["registry@sha256:" + "d" * 64],
            },
            {
                "Id": runtime_image_id,
                "RepoDigests": [
                    "example.invalid/registry@" + image_contract["digest"]
                ],
            },
        )
        for image_payload in mismatches:
            with (
                self.subTest(image_payload=image_payload),
                mock.patch.object(
                    gate,
                    "_json_tool_output",
                    return_value=([image_payload], b"[]"),
                ),
                self.assertRaises(gate.ReleaseGateError) as caught,
            ):
                gate._inspect_live_registry_runtime_image(
                    options(
                        pathlib.Path("/tmp/output"),
                        signing_policy_profile="production",
                    ),
                    container_payload,
                    expected_reference=image_contract["reference"],
                )

            self.assertEqual(
                caught.exception.code,
                "release.registry_production_boundary_invalid",
            )

    def test_live_registry_runtime_evidence_rejects_tampered_image_identity(self) -> None:
        checked_in_policy = json.loads(
            (REPO_ROOT / gate.PRODUCTION_REGISTRY_RETENTION_POLICY_PATH).read_text(encoding="utf-8")
        )
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            live_config = root / "registry-config.yml"
            live_policy = root / "retention-policy.json"
            live_config.write_text(
                (
                    "storage:\n"
                    "  delete:\n"
                    "    enabled: false\n"
                    "http:\n"
                    "  host: https://localhost:55091\n"
                    "  tls:\n"
                    "    certificate: /certs/tls.crt\n"
                ),
                encoding="utf-8",
            )
            live_policy.write_text(json.dumps(checked_in_policy), encoding="utf-8")
            gate_options = options(
                root / "output",
                signing_policy_profile="production",
                production_registry_config_path=live_config,
                production_registry_retention_policy_path=live_policy,
            )
            exported_config_sha256 = hashlib.sha256(live_config.read_bytes()).hexdigest()
            checked_in_policy_sha256 = gate._stable_json_sha256(checked_in_policy)
            for overrides in (
                {"config_image_reference": "registry:2.8.3"},
                {"expected_image_digest": "sha256:" + "c" * 64},
                {"matched_repo_digest": "registry@sha256:" + "e" * 64},
            ):
                with self.subTest(overrides=overrides), self.assertRaises(
                    gate.ReleaseGateError
                ) as caught:
                    gate.validate_production_registry_boundary(
                        gate_options,
                        runtime_evidence=live_runtime_evidence(
                            gate_options,
                            exported_config_sha256=exported_config_sha256,
                            live_policy_sha256=checked_in_policy_sha256,
                            checked_in_policy_sha256=checked_in_policy_sha256,
                            **overrides,
                        ),
                    )

                self.assertEqual(
                    caught.exception.code,
                    "release.registry_production_boundary_invalid",
                )

            tampered_evidence = live_runtime_evidence(
                gate_options,
                exported_config_sha256=exported_config_sha256,
                live_policy_sha256=checked_in_policy_sha256,
                checked_in_policy_sha256=checked_in_policy_sha256,
            )
            tampered_evidence["container"]["image"]["runtimeId"] = "sha256:" + "d" * 64
            with self.assertRaises(gate.ReleaseGateError) as caught:
                gate.validate_production_registry_boundary(
                    gate_options,
                    runtime_evidence=tampered_evidence,
                )

            self.assertEqual(
                caught.exception.code,
                "release.registry_production_boundary_invalid",
            )

    def test_rejects_registry_boundary_when_live_config_allows_delete(self) -> None:
        checked_in_policy = json.loads(
            (REPO_ROOT / gate.PRODUCTION_REGISTRY_RETENTION_POLICY_PATH).read_text(encoding="utf-8")
        )
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            live_config = root / "registry-config.yml"
            live_policy = root / "retention-policy.json"
            live_config.write_text(
                "\n".join(
                    [
                        "storage:",
                        "  delete:",
                        "    enabled: true",
                    ]
                )
                + "\n",
                encoding="utf-8",
            )
            live_policy.write_text(json.dumps(checked_in_policy), encoding="utf-8")

            with self.assertRaises(gate.ReleaseGateError) as caught:
                gate.validate_production_registry_boundary(
                    options(
                        root / "output",
                        signing_policy_profile="production",
                        production_registry_config_path=live_config,
                        production_registry_retention_policy_path=live_policy,
                    )
                )

        self.assertEqual(caught.exception.code, "release.registry_production_boundary_invalid")

    def test_rejects_registry_boundary_when_live_config_uses_commented_false_only(self) -> None:
        checked_in_policy = json.loads(
            (REPO_ROOT / gate.PRODUCTION_REGISTRY_RETENTION_POLICY_PATH).read_text(encoding="utf-8")
        )
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            live_config = root / "registry-config.yml"
            live_policy = root / "retention-policy.json"
            live_config.write_text(
                "\n".join(
                    [
                        "storage:",
                        "  delete:",
                        "    # enabled: false",
                        "    enabled: true",
                    ]
                )
                + "\n",
                encoding="utf-8",
            )
            live_policy.write_text(json.dumps(checked_in_policy), encoding="utf-8")

            with self.assertRaises(gate.ReleaseGateError) as caught:
                gate.validate_production_registry_boundary(
                    options(
                        root / "output",
                        signing_policy_profile="production",
                        production_registry_config_path=live_config,
                        production_registry_retention_policy_path=live_policy,
                    )
                )

        self.assertEqual(caught.exception.code, "release.registry_production_boundary_invalid")

    def test_rejects_registry_boundary_when_live_config_duplicates_delete_enabled(self) -> None:
        checked_in_policy = json.loads(
            (REPO_ROOT / gate.PRODUCTION_REGISTRY_RETENTION_POLICY_PATH).read_text(encoding="utf-8")
        )
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            live_config = root / "registry-config.yml"
            live_policy = root / "retention-policy.json"
            live_config.write_text(
                "\n".join(
                    [
                        "storage:",
                        "  delete:",
                        "    enabled: false",
                        "    enabled: true",
                    ]
                )
                + "\n",
                encoding="utf-8",
            )
            live_policy.write_text(json.dumps(checked_in_policy), encoding="utf-8")

            with self.assertRaises(gate.ReleaseGateError) as caught:
                gate.validate_production_registry_boundary(
                    options(
                        root / "output",
                        signing_policy_profile="production",
                        production_registry_config_path=live_config,
                        production_registry_retention_policy_path=live_policy,
                    )
                )

        self.assertEqual(caught.exception.code, "release.registry_production_boundary_invalid")

    def test_rejects_registry_boundary_when_retention_policy_runtime_config_path_drifts(self) -> None:
        checked_in_policy = json.loads(
            (REPO_ROOT / gate.PRODUCTION_REGISTRY_RETENTION_POLICY_PATH).read_text(encoding="utf-8")
        )
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            live_config = root / "registry-config.yml"
            live_policy = root / "retention-policy.json"
            live_config.write_text(
                (
                    "storage:\n"
                    "  delete:\n"
                    "    enabled: false\n"
                    "http:\n"
                    "  host: https://localhost:55091\n"
                    "  tls:\n"
                    "    certificate: /certs/tls.crt\n"
                ),
                encoding="utf-8",
            )
            drifted_policy = dict(checked_in_policy)
            drifted_policy["registryConfigPath"] = "/etc/distribution/other-config.yml"
            live_policy.write_text(json.dumps(drifted_policy), encoding="utf-8")

            with self.assertRaises(gate.ReleaseGateError) as caught:
                gate.validate_production_registry_boundary(
                    options(
                        root / "output",
                        signing_policy_profile="production",
                        production_registry_config_path=live_config,
                        production_registry_retention_policy_path=live_policy,
                    )
                )

        self.assertEqual(caught.exception.code, "release.registry_production_boundary_invalid")

    def test_rejects_registry_boundary_when_runtime_evidence_is_stale(self) -> None:
        checked_in_policy = json.loads(
            (REPO_ROOT / gate.PRODUCTION_REGISTRY_RETENTION_POLICY_PATH).read_text(encoding="utf-8")
        )
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            live_config = root / "registry-config.yml"
            live_policy = root / "retention-policy.json"
            live_config.write_text(
                (
                    "storage:\n"
                    "  delete:\n"
                    "    enabled: false\n"
                    "http:\n"
                    "  host: https://localhost:55091\n"
                    "  tls:\n"
                    "    certificate: /certs/tls.crt\n"
                ),
                encoding="utf-8",
            )
            live_policy.write_text(json.dumps(checked_in_policy), encoding="utf-8")
            gate_options = options(
                root / "output",
                signing_policy_profile="production",
                production_registry_config_path=live_config,
                production_registry_retention_policy_path=live_policy,
            )
            exported_config_sha256 = hashlib.sha256(live_config.read_bytes()).hexdigest()
            checked_in_policy_sha256 = gate._stable_json_sha256(checked_in_policy)
            stale_collected_at = (
                dt.datetime.now(dt.timezone.utc)
                - dt.timedelta(seconds=gate.PRODUCTION_REGISTRY_RUNTIME_EVIDENCE_MAX_AGE_SECONDS + 1)
            ).isoformat()

            with self.assertRaises(gate.ReleaseGateError) as caught:
                gate.validate_production_registry_boundary(
                    gate_options,
                    runtime_evidence=live_runtime_evidence(
                        gate_options,
                        exported_config_sha256=exported_config_sha256,
                        live_policy_sha256=checked_in_policy_sha256,
                        checked_in_policy_sha256=checked_in_policy_sha256,
                        collected_at=stale_collected_at,
                    ),
                )

        self.assertEqual(caught.exception.code, "release.registry_production_boundary_invalid")

    def test_rejects_registry_boundary_when_runtime_evidence_authority_drifts(self) -> None:
        checked_in_policy = json.loads(
            (REPO_ROOT / gate.PRODUCTION_REGISTRY_RETENTION_POLICY_PATH).read_text(encoding="utf-8")
        )
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            live_config = root / "registry-config.yml"
            live_policy = root / "retention-policy.json"
            live_config.write_text(
                (
                    "storage:\n"
                    "  delete:\n"
                    "    enabled: false\n"
                    "http:\n"
                    "  host: https://localhost:55091\n"
                    "  tls:\n"
                    "    certificate: /certs/tls.crt\n"
                ),
                encoding="utf-8",
            )
            live_policy.write_text(json.dumps(checked_in_policy), encoding="utf-8")
            gate_options = options(
                root / "output",
                signing_policy_profile="production",
                production_registry_config_path=live_config,
                production_registry_retention_policy_path=live_policy,
            )
            exported_config_sha256 = hashlib.sha256(live_config.read_bytes()).hexdigest()
            checked_in_policy_sha256 = gate._stable_json_sha256(checked_in_policy)

            with self.assertRaises(gate.ReleaseGateError) as caught:
                gate.validate_production_registry_boundary(
                    gate_options,
                    runtime_evidence=live_runtime_evidence(
                        gate_options,
                        exported_config_sha256=exported_config_sha256,
                        live_policy_sha256=checked_in_policy_sha256,
                        checked_in_policy_sha256=checked_in_policy_sha256,
                        registry_authority="https://other.example.test",
                    ),
                )

        self.assertEqual(caught.exception.code, "release.registry_production_boundary_invalid")

    def test_rejects_registry_boundary_when_runtime_evidence_certificate_drifts(self) -> None:
        checked_in_policy = json.loads(
            (REPO_ROOT / gate.PRODUCTION_REGISTRY_RETENTION_POLICY_PATH).read_text(encoding="utf-8")
        )
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            live_config = root / "registry-config.yml"
            live_policy = root / "retention-policy.json"
            live_config.write_text(
                (
                    "storage:\n"
                    "  delete:\n"
                    "    enabled: false\n"
                    "http:\n"
                    "  host: https://localhost:55091\n"
                    "  tls:\n"
                    "    certificate: /certs/tls.crt\n"
                ),
                encoding="utf-8",
            )
            live_policy.write_text(json.dumps(checked_in_policy), encoding="utf-8")
            gate_options = options(
                root / "output",
                signing_policy_profile="production",
                production_registry_config_path=live_config,
                production_registry_retention_policy_path=live_policy,
            )
            prepared_registry_access = gate.supply_chain.PreparedRegistryAccess(
                environment={},
                host_environment={"DOCKER_CONFIG": str(root / "docker-config")},
                registry_host="localhost:55091",
                auth_configured=True,
                ca_materialized=True,
                registry_ca_container_path="/workspace/registry-access/docker-config/certs.d/localhost:55091/ca.crt",
            )
            inspect_payload = [
                {
                    "Id": "a" * 64,
                    "Image": "sha256:" + "b" * 64,
                    "Name": "/synara-production-registry",
                    "State": {
                        "Running": True,
                        "StartedAt": dt.datetime.now(dt.timezone.utc).isoformat(),
                    },
                    "Config": {"Image": gate.PRODUCTION_REGISTRY_RUNTIME_IMAGE_REFERENCE},
                }
            ]
            image_inspect_payload = [
                {
                    "Id": "sha256:" + "b" * 64,
                    "RepoDigests": [
                        gate._production_registry_runtime_image_contract(
                            gate.PRODUCTION_REGISTRY_RUNTIME_IMAGE_REFERENCE
                        )["repoDigest"]
                    ],
                }
            ]

            with (
                mock.patch.object(
                    gate,
                    "_prepare_host_registry_access",
                    return_value=prepared_registry_access,
                ),
                mock.patch.object(
                    gate,
                    "_json_tool_output",
                    side_effect=[
                        (inspect_payload, b"[]"),
                        (image_inspect_payload, b"[]"),
                    ],
                ),
                mock.patch.object(
                    gate,
                    "_production_boundary_docker_output",
                    side_effect=[
                        (
                            "storage:\n"
                            "  delete:\n"
                            "    enabled: false\n"
                            "http:\n"
                            "  host: https://localhost:55091\n"
                            "  tls:\n"
                            "    certificate: /certs/tls.crt\n"
                        ),
                        "-----BEGIN CERTIFICATE-----\nfixture\n-----END CERTIFICATE-----\n",
                    ],
                ),
                mock.patch.object(
                    gate,
                    "_load_host_registry_client_inputs",
                    return_value=gate.HostRegistryClientInputs(
                        registry_host="localhost:55091",
                        username="registry-user",
                        password="registry-password",
                        ca_path=root / "registry-ca.pem",
                    ),
                ),
                mock.patch.object(gate, "_pem_certificate_sha256", return_value="a" * 64),
                mock.patch.object(
                    gate,
                    "_probe_live_registry_boundary",
                    return_value={
                        "registryHost": "localhost:55091",
                        "repositoryProbeStatus": 404,
                        "tlsPeerCertificateSha256": "b" * 64,
                    },
                ),
                self.assertRaises(gate.ReleaseGateError) as caught,
            ):
                gate.validate_production_registry_boundary(
                    gate_options,
                    state_dir=root / "_state",
                    redactor=gate.acceptance.SecretRedactor(),
                )

        self.assertEqual(caught.exception.code, "release.registry_production_boundary_invalid")

    def test_rejects_registry_boundary_when_runtime_evidence_retention_hash_drifts(self) -> None:
        checked_in_policy = json.loads(
            (REPO_ROOT / gate.PRODUCTION_REGISTRY_RETENTION_POLICY_PATH).read_text(encoding="utf-8")
        )
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            live_config = root / "registry-config.yml"
            live_policy = root / "retention-policy.json"
            live_config.write_text(
                (
                    "storage:\n"
                    "  delete:\n"
                    "    enabled: false\n"
                    "http:\n"
                    "  host: https://localhost:55091\n"
                    "  tls:\n"
                    "    certificate: /certs/tls.crt\n"
                ),
                encoding="utf-8",
            )
            live_policy.write_text(json.dumps(checked_in_policy), encoding="utf-8")
            gate_options = options(
                root / "output",
                signing_policy_profile="production",
                production_registry_config_path=live_config,
                production_registry_retention_policy_path=live_policy,
            )
            exported_config_sha256 = hashlib.sha256(live_config.read_bytes()).hexdigest()
            checked_in_policy_sha256 = gate._stable_json_sha256(checked_in_policy)

            with self.assertRaises(gate.ReleaseGateError) as caught:
                gate.validate_production_registry_boundary(
                    gate_options,
                    runtime_evidence=live_runtime_evidence(
                        gate_options,
                        exported_config_sha256=exported_config_sha256,
                        live_policy_sha256="c" * 64,
                        checked_in_policy_sha256=checked_in_policy_sha256,
                    ),
                )

        self.assertEqual(caught.exception.code, "release.registry_production_boundary_invalid")

    def test_rejects_registry_boundary_when_runtime_evidence_config_hash_drifts(self) -> None:
        checked_in_policy = json.loads(
            (REPO_ROOT / gate.PRODUCTION_REGISTRY_RETENTION_POLICY_PATH).read_text(encoding="utf-8")
        )
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            live_config = root / "registry-config.yml"
            live_policy = root / "retention-policy.json"
            live_config.write_text(
                (
                    "storage:\n"
                    "  delete:\n"
                    "    enabled: false\n"
                    "http:\n"
                    "  host: https://localhost:55091\n"
                    "  tls:\n"
                    "    certificate: /certs/tls.crt\n"
                ),
                encoding="utf-8",
            )
            live_policy.write_text(json.dumps(checked_in_policy), encoding="utf-8")
            gate_options = options(
                root / "output",
                signing_policy_profile="production",
                production_registry_config_path=live_config,
                production_registry_retention_policy_path=live_policy,
            )
            exported_config_sha256 = hashlib.sha256(live_config.read_bytes()).hexdigest()
            checked_in_policy_sha256 = gate._stable_json_sha256(checked_in_policy)

            with self.assertRaises(gate.ReleaseGateError) as caught:
                gate.validate_production_registry_boundary(
                    gate_options,
                    runtime_evidence=live_runtime_evidence(
                        gate_options,
                        exported_config_sha256=exported_config_sha256,
                        live_policy_sha256=checked_in_policy_sha256,
                        checked_in_policy_sha256=checked_in_policy_sha256,
                        runtime_config_sha256="b" * 64,
                    ),
                )

        self.assertEqual(caught.exception.code, "release.registry_production_boundary_invalid")

    def test_accepts_registry_port_nested_repository_and_public_go_proxy(self) -> None:
        self.assertEqual(
            gate.normalize_image_repository("localhost:55091/synara/worker-image"),
            "localhost:55091/synara/worker-image",
        )
        self.assertEqual(
            gate.normalize_go_proxy("https://goproxy.cn,direct"),
            "https://goproxy.cn,direct",
        )

    def test_rejects_tag_digest_credentials_query_and_invalid_repository_shape(self) -> None:
        invalid = [
            "registry.example.test/team/worker:latest",
            "registry.example.test/team/worker@sha256:" + "a" * 64,
            "user@registry.example.test/team/worker",
            "registry.example.test/team/worker?debug=1",
            "registry.example.test/Team/worker",
            "registry.example.test/team/worker/",
            "localhost:70000/team/worker",
        ]
        for value in invalid:
            with self.subTest(value=value), self.assertRaises(ValueError):
                gate.normalize_image_repository(value)

    def test_rejects_non_https_or_credential_like_go_proxy_values(self) -> None:
        invalid = [
            "http://proxy.example.test,direct",
            "https://user:password@proxy.example.test,direct",
            "https://proxy.example.test?token=value,direct",
            "https://proxy.example.test, direct",
            "https://proxy.example.test,,direct",
        ]
        for value in invalid:
            with self.subTest(value=value), self.assertRaises(ValueError):
                gate.normalize_go_proxy(value)

    def test_build_inputs_keep_proxy_no_cache_and_pinned_sbom_generator(self) -> None:
        command = gate.build_command(
            options(pathlib.Path("/tmp/output")),
            image="localhost:55091/synara/worker:tag",
            git_sha=GIT_SHA,
            version=VERSION,
            source_date_epoch=SOURCE_DATE_EPOCH,
            metadata_path=pathlib.Path("/tmp/metadata.json"),
            no_cache=True,
        )

        self.assertIn("--push", command)
        self.assertIn("--no-cache", command)
        self.assertEqual(command[command.index("--go-proxy") + 1], "https://goproxy.cn,direct")
        self.assertEqual(
            gate.locked_sbom_generator(REPO_ROOT),
            "docker.io/docker/buildkit-syft-scanner@sha256:"
            "79e7b013cbec16bbb436f312819a49a4a57752b2270c1a9332ae1a10fcc82a68",
        )
        build_script = (REPO_ROOT / "deploy/worker/build.sh").read_text(encoding="utf-8")
        dockerfile = (REPO_ROOT / "Dockerfile").read_text(encoding="utf-8")
        self.assertIn("type=image,push=true,rewrite-timestamp=true", build_script)
        self.assertIn("--sbom=generator=$sbom_generator", build_script)
        self.assertIn("rm -f /var/log/apk.log", dockerfile)
        self.assertIn("--mount=from=worker-provider-tools", dockerfile)
        self.assertIn('/opt/synara/.build-revision', dockerfile)
        self.assertIn("COPY packages/shared/src ./packages/shared/src", dockerfile)
        self.assertIn('touch -d "@${SOURCE_DATE_EPOCH}" /out/synara-agentd', dockerfile)
        self.assertIn('touch -d "@${SOURCE_DATE_EPOCH}" /out/provider-host.mjs', dockerfile)
        self.assertNotIn(
            "COPY --from=worker-provider-tools /tmp/provider-tools.raw.spdx.json",
            dockerfile,
        )

        with tempfile.TemporaryDirectory() as directory:
            repo_root = pathlib.Path(directory)
            lock_path = repo_root / gate.SBOM_GENERATOR_LOCK_PATH
            lock_path.parent.mkdir(parents=True)
            lock_path.write_text("docker.io/docker/buildkit-syft-scanner:stable-1\n")
            with self.assertRaises(gate.ReleaseGateError) as caught:
                gate.locked_sbom_generator(repo_root)
        self.assertEqual(caught.exception.code, "release.registry_source_metadata_invalid")

        generator = gate.locked_sbom_generator(REPO_ROOT)
        generator_digest = generator.rsplit("@sha256:", 1)[-1]
        build_digest = "sha256:" + "8" * 64
        metadata = {
            "containerimage.digest": build_digest,
            "containerimage.descriptor": {
                "mediaType": gate.OCI_INDEX_MEDIA_TYPE,
                "digest": build_digest,
                "size": 123,
            },
            **{
                f"buildx.build.provenance/{platform}": {
                    "materials": [
                        {
                            "uri": "pkg:docker/docker/buildkit-syft-scanner"
                            f"?digest=sha256:{generator_digest}",
                            "digest": {"sha256": generator_digest},
                        }
                    ]
                }
                for platform in gate.REQUIRED_PLATFORMS
            },
        }
        with tempfile.TemporaryDirectory() as directory:
            metadata_path = pathlib.Path(directory) / "metadata.json"
            metadata_path.write_text(json.dumps(metadata), encoding="utf-8")

            evidence = gate._load_build_metadata(
                metadata_path,
                slot="cached",
                expected_sbom_generator=generator,
            )
            self.assertEqual(evidence["sbomGenerator"], generator)

            metadata["buildx.build.provenance/linux/arm64"]["materials"][0]["digest"] = {
                "sha256": "0" * 64
            }
            metadata_path.write_text(json.dumps(metadata), encoding="utf-8")
            with self.assertRaises(gate.ReleaseGateError) as metadata_error:
                gate._load_build_metadata(
                    metadata_path,
                    slot="no-cache",
                    expected_sbom_generator=generator,
                )
            self.assertEqual(
                metadata_error.exception.code,
                "release.registry_build_metadata_invalid",
            )

    def test_worker_runtime_replaces_base_npm_with_locked_high_free_copy(self) -> None:
        package_path = REPO_ROOT / "deploy/worker/provider-tools/package.json"
        lock_path = REPO_ROOT / "deploy/worker/provider-tools/package-lock.json"
        package = json.loads(package_path.read_text(encoding="utf-8"))
        lock_text = lock_path.read_text(encoding="utf-8")
        lock = json.loads(lock_text)
        packages = lock["packages"]

        expected_npm = package["dependencies"]["npm"]
        self.assertEqual(expected_npm, "12.0.1")
        self.assertEqual(packages[""]["dependencies"]["npm"], expected_npm)
        self.assertEqual(packages["node_modules/npm"]["version"], expected_npm)
        self.assertEqual(
            packages["node_modules/npm"]["resolved"],
            "https://registry.npmjs.org/npm/-/npm-12.0.1.tgz",
        )
        self.assertEqual(
            packages["node_modules/npm/node_modules/undici"]["version"],
            "6.27.0",
        )
        self.assertNotIn("registry.npmmirror.com", lock_text)

        dockerfile = (REPO_ROOT / "Dockerfile").read_text(encoding="utf-8")
        self.assertIn("rm -rf /usr/local/lib/node_modules/npm", dockerfile)
        self.assertIn(
            "node_modules/npm/bin/npm-cli.js /usr/local/bin/npm",
            dockerfile,
        )
        self.assertIn(
            "node_modules/npm/bin/npx-cli.js /usr/local/bin/npx",
            dockerfile,
        )
        self.assertIn(
            "node_modules/npm/node_modules/node-gyp/bin/node-gyp.js",
            dockerfile,
        )
        self.assertIn('test "$(npm --version)" = "$expected_npm"', dockerfile)
        self.assertIn("rm -rf /tmp/node-compile-cache", dockerfile)

    def test_worker_build_script_rejects_non_https_proxy_before_docker(self) -> None:
        completed = subprocess.run(
            [
                str(REPO_ROOT / "deploy/worker/build.sh"),
                "--git-sha",
                GIT_SHA,
                "--go-proxy",
                "http://proxy.example.test,direct",
            ],
            cwd=REPO_ROOT,
            check=False,
            capture_output=True,
            text=True,
        )

        self.assertEqual(completed.returncode, 2)
        self.assertIn("entries must use https://", completed.stderr)

        valid_proxy = subprocess.run(
            [
                str(REPO_ROOT / "deploy/worker/build.sh"),
                "--git-sha",
                GIT_SHA,
                "--source-date-epoch",
                "invalid",
                "--go-proxy",
                "https://goproxy.cn,direct",
            ],
            cwd=REPO_ROOT,
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(valid_proxy.returncode, 2)
        self.assertIn("--source-date-epoch", valid_proxy.stderr)


class ImageConfigTest(unittest.TestCase):
    def test_preserves_distinct_oci_and_spdx_timestamp_shapes(self) -> None:
        self.assertEqual(gate.expected_created_at(SOURCE_DATE_EPOCH), "2023-11-14T22:13:20Z")
        self.assertEqual(
            gate.expected_sbom_created_at(SOURCE_DATE_EPOCH),
            "2023-11-14T22:13:20.000Z",
        )

    def test_accepts_non_root_config_with_null_or_empty_cmd(self) -> None:
        for command in (None, []):
            payload = image_config("linux/amd64")
            payload["config"]["Cmd"] = command

            evidence = gate._validate_image_config(
                payload,
                platform="linux/amd64",
                git_sha=GIT_SHA,
                version=VERSION,
                created_at=gate.expected_created_at(SOURCE_DATE_EPOCH),
            )

            self.assertEqual(evidence["user"], gate.EXPECTED_USER)

    def test_rejects_runtime_cmd_and_credential_environment(self) -> None:
        invalid_cmd = image_config("linux/amd64")
        invalid_cmd["config"]["Cmd"] = ["sh"]
        credential_env = image_config(
            "linux/amd64", extra_environment=["SYNARA_API_TOKEN=must-not-be-here"]
        )
        for payload in (invalid_cmd, credential_env):
            with self.assertRaises(gate.ReleaseGateError) as caught:
                gate._validate_image_config(
                    payload,
                    platform="linux/amd64",
                    git_sha=GIT_SHA,
                    version=VERSION,
                    created_at=gate.expected_created_at(SOURCE_DATE_EPOCH),
                )
            self.assertEqual(caught.exception.code, "release.registry_image_config_invalid")


class RegistryInspectionTest(unittest.TestCase):
    def test_accepts_exact_dual_platform_index_attestations_and_configs(self) -> None:
        inspector, expected_digest = registry_inspector()
        with mock.patch.object(gate, "_json_tool_output", side_effect=inspector):
            evidence = gate.inspect_registry_image(
                options(pathlib.Path("/tmp/output")),
                image="example.invalid/synara/worker:tag",
                expected_digest=expected_digest,
                git_sha=GIT_SHA,
                version=VERSION,
                source_date_epoch=SOURCE_DATE_EPOCH,
            )

        self.assertEqual(evidence["indexDigest"], expected_digest)
        self.assertEqual(evidence["platformDigests"], INDEX_DIGESTS)
        self.assertEqual(set(evidence["attestationPredicates"]), set(gate.REQUIRED_PLATFORMS))

    def test_rejects_missing_or_duplicate_attestation_descriptor(self) -> None:
        for mode in ("missing", "duplicate"):
            inspector, expected_digest = registry_inspector(attestation_mode=mode)
            with self.subTest(mode=mode), mock.patch.object(
                gate, "_json_tool_output", side_effect=inspector
            ), self.assertRaises(gate.ReleaseGateError) as caught:
                gate.inspect_registry_image(
                    options(pathlib.Path("/tmp/output")),
                    image="example.invalid/synara/worker:tag",
                    expected_digest=expected_digest,
                    git_sha=GIT_SHA,
                    version=VERSION,
                    source_date_epoch=SOURCE_DATE_EPOCH,
                )
            self.assertEqual(caught.exception.code, "release.registry_attestation_invalid")

    def test_malformed_attestation_layers_raise_stable_gate_error(self) -> None:
        inspector, expected_digest = registry_inspector(
            mutate_attestation=lambda payload: payload.update({"layers": None})
        )
        with mock.patch.object(
            gate, "_json_tool_output", side_effect=inspector
        ), self.assertRaises(gate.ReleaseGateError) as caught:
            gate.inspect_registry_image(
                options(pathlib.Path("/tmp/output")),
                image="example.invalid/synara/worker:tag",
                expected_digest=expected_digest,
                git_sha=GIT_SHA,
                version=VERSION,
                source_date_epoch=SOURCE_DATE_EPOCH,
            )

        self.assertEqual(caught.exception.code, "release.registry_attestation_invalid")


class EmbeddedArtifactTest(unittest.TestCase):
    def test_accepts_manifest_sbom_lockfiles_and_runtime_files_from_source(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            files = write_embedded_fixture(pathlib.Path(directory))
            evidence = gate.validate_embedded_artifacts(
                options(pathlib.Path(directory) / "output"),
                files,
                platform="linux/amd64",
                git_sha=GIT_SHA,
                version=VERSION,
                source_date_epoch=SOURCE_DATE_EPOCH,
            )

        self.assertEqual(set(evidence["lockfileSha256"]), set(gate.LOCAL_LOCK_PATHS))
        self.assertEqual(evidence["providerRuntimes"], gate._expected_provider_runtimes(REPO_ROOT))

    def test_rejects_embedded_lockfile_that_differs_from_source(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            files = write_embedded_fixture(pathlib.Path(directory))
            files["providerHostLock"].write_bytes(b"different lock")
            with self.assertRaises(gate.ReleaseGateError) as caught:
                gate.validate_embedded_artifacts(
                    options(pathlib.Path(directory) / "output"),
                    files,
                    platform="linux/amd64",
                    git_sha=GIT_SHA,
                    version=VERSION,
                    source_date_epoch=SOURCE_DATE_EPOCH,
                )

        self.assertEqual(caught.exception.code, "release.registry_embedded_lock_invalid")

    def test_malformed_lockfiles_packages_and_creation_info_raise_gate_error(self) -> None:
        mutations = [
            (lambda manifest: manifest.update({"lockfiles": None}), None),
            (None, lambda sbom: sbom.update({"packages": None})),
            (None, lambda sbom: sbom.update({"creationInfo": None})),
        ]
        for manifest_mutation, sbom_mutation in mutations:
            with self.subTest(
                manifest=manifest_mutation is not None,
                sbom=sbom_mutation is not None,
            ), tempfile.TemporaryDirectory() as directory:
                files = write_embedded_fixture(
                    pathlib.Path(directory),
                    mutate_manifest=manifest_mutation,
                    mutate_sbom=sbom_mutation,
                )
                with self.assertRaises(gate.ReleaseGateError) as caught:
                    gate.validate_embedded_artifacts(
                        options(pathlib.Path(directory) / "output"),
                        files,
                        platform="linux/amd64",
                        git_sha=GIT_SHA,
                        version=VERSION,
                        source_date_epoch=SOURCE_DATE_EPOCH,
                    )
                self.assertEqual(
                    caught.exception.code,
                    "release.registry_embedded_manifest_invalid",
                )


class ReproducibilityTest(unittest.TestCase):
    def test_requires_two_builds_with_identical_platform_digests(self) -> None:
        cached = sample_build("cached", False)
        no_cache = sample_build("no-cache", True)

        self.assertEqual(gate.reproducibility_errors([cached, no_cache]), [])

        no_cache["platformDigests"] = {
            **INDEX_DIGESTS,
            "linux/arm64": "sha256:" + "9" * 64,
        }
        errors = gate.reproducibility_errors([cached, no_cache])
        self.assertEqual(errors[0]["code"], "release.registry_platform_digest_mismatch")

    def test_rejects_incomplete_build_coverage(self) -> None:
        errors = gate.reproducibility_errors([sample_build("cached", False)])
        self.assertEqual(errors[0]["code"], "release.registry_build_coverage_incomplete")


class AggregateGateTest(unittest.TestCase):
    def test_production_signer_identity_requires_exact_approle_boundary(self) -> None:
        gate_options = options(
            pathlib.Path("/tmp/output"),
            signing_policy_profile="production",
        )
        report = production_supply_chain()

        self.assertEqual(
            gate._validate_production_signer_identity(gate_options, report),
            gate._expected_production_signer_identity(),
        )

    def test_production_signer_identity_rejects_missing_or_tampered_evidence(self) -> None:
        gate_options = options(
            pathlib.Path("/tmp/output"),
            signing_policy_profile="production",
        )
        mutations = (
            lambda report: report["signing"].pop("signerIdentity"),
            lambda report: report["signing"]["signerIdentity"].update(
                {"roleName": "other-release-signer"}
            ),
            lambda report: report["signing"]["signerIdentity"].update(
                {"type": "service"}
            ),
            lambda report: report["signing"]["signerIdentity"].update(
                {"orphan": False}
            ),
            lambda report: report["signing"]["signerIdentity"].update(
                {"policiesSha256": "c" * 64}
            ),
        )
        for mutate in mutations:
            report = production_supply_chain()
            mutate(report)
            with self.subTest(mutate=mutate), self.assertRaises(
                gate.ReleaseGateError
            ) as caught:
                gate._validate_production_signer_identity(gate_options, report)

            self.assertEqual(
                caught.exception.code,
                "release.registry_supply_chain_signer_identity_invalid",
            )

    def test_markdown_distinguishes_production_signing_from_ephemeral_mechanics(self) -> None:
        supply_chain_report = production_supply_chain()
        markdown = gate.markdown_from_report(
            {
                "schemaVersion": gate.SCHEMA_VERSION,
                "runId": "run-1",
                "status": "pass",
                "source": {"gitSha": GIT_SHA, "version": VERSION},
                "durationMs": 1,
                "builds": [],
                "supplyChain": supply_chain_report,
                "errors": [],
            }
        )

        self.assertIn("also enforced the checked-in production KMS/keyless identity", markdown)
        self.assertIn("digest-only Registry retention and archive-first GC boundary", markdown)
        self.assertIn("Vault signer AppRole: `synara-worker-release-signer`", markdown)
        self.assertIn("Vault signer token: type `batch`, orphan `True`", markdown)
        self.assertNotIn("Ephemeral signing does not prove", markdown)

    def test_emits_pass_report_for_cached_and_no_cache_consensus(self) -> None:
        sha = subprocess.run(
            ["git", "rev-parse", "HEAD"],
            cwd=REPO_ROOT,
            check=True,
            capture_output=True,
            text=True,
        ).stdout.strip()
        with tempfile.TemporaryDirectory() as directory:
            output_dir = pathlib.Path(directory) / "gate"
            gate_options = options(output_dir)

            def build_runner(_options: Any, **kwargs: Any) -> dict[str, Any]:
                return sample_build(kwargs["slot"], kwargs["no_cache"])

            with contextlib.redirect_stdout(io.StringIO()):
                exit_code = gate.run_registry_release_gate(
                    gate_options,
                    repository_state=lambda _root: {"gitSha": sha, "worktreeDirty": False},
                    runtime_inspector=lambda _options: {"builder": gate_options.builder},
                    build_runner=build_runner,
                    supply_chain_runner=lambda *_args, **_kwargs: sample_supply_chain(),
                )
            report = json.loads((output_dir / gate.JSON_REPORT_NAME).read_text(encoding="utf-8"))

        self.assertEqual(exit_code, 0)
        self.assertEqual(report["status"], "pass")
        self.assertEqual(len(report["builds"]), 2)
        self.assertEqual(report["supplyChain"]["status"], "pass")
        self.assertEqual(report["security"]["outputSecretScan"]["findings"], [])
        self.assertIn(
            str(gate.PRODUCTION_REGISTRY_RETENTION_POLICY_PATH),
            report["source"]["sourceHashes"],
        )

    def test_emits_fail_report_when_supply_chain_policy_fails(self) -> None:
        sha = subprocess.run(
            ["git", "rev-parse", "HEAD"],
            cwd=REPO_ROOT,
            check=True,
            capture_output=True,
            text=True,
        ).stdout.strip()
        with tempfile.TemporaryDirectory() as directory:
            output_dir = pathlib.Path(directory) / "gate"
            gate_options = options(output_dir)

            def build_runner(_options: Any, **kwargs: Any) -> dict[str, Any]:
                return sample_build(kwargs["slot"], kwargs["no_cache"])

            supply_chain_report = sample_supply_chain()
            supply_chain_report["status"] = "fail"
            supply_chain_report["errors"] = [
                {
                    "code": "release.registry_vulnerability_policy_blocked",
                    "message": "critical vulnerability",
                }
            ]
            with contextlib.redirect_stdout(io.StringIO()):
                exit_code = gate.run_registry_release_gate(
                    gate_options,
                    repository_state=lambda _root: {"gitSha": sha, "worktreeDirty": False},
                    runtime_inspector=lambda _options: {"builder": gate_options.builder},
                    build_runner=build_runner,
                    supply_chain_runner=lambda *_args, **_kwargs: supply_chain_report,
                )
            report = json.loads((output_dir / gate.JSON_REPORT_NAME).read_text(encoding="utf-8"))

        self.assertEqual(exit_code, 1)
        self.assertEqual(report["status"], "fail")
        self.assertEqual(
            {error["code"] for error in report["errors"]},
            {"release.registry_vulnerability_policy_blocked"},
        )

    def test_production_gate_rejects_passing_report_without_signer_identity(self) -> None:
        sha = subprocess.run(
            ["git", "rev-parse", "HEAD"],
            cwd=REPO_ROOT,
            check=True,
            capture_output=True,
            text=True,
        ).stdout.strip()
        with tempfile.TemporaryDirectory() as directory:
            output_dir = pathlib.Path(directory) / "gate"
            gate_options = options(
                output_dir,
                signing_policy_profile="production",
            )
            supply_chain_report = production_supply_chain()
            supply_chain_report["signing"].pop("signerIdentity")

            def build_runner(_options: Any, **kwargs: Any) -> dict[str, Any]:
                return sample_build(kwargs["slot"], kwargs["no_cache"])

            with (
                mock.patch.object(gate, "_validate_production_boundary"),
                mock.patch.object(
                    gate,
                    "_prepare_host_registry_environment",
                    return_value={},
                ),
                mock.patch.object(
                    gate,
                    "validate_production_registry_boundary",
                    return_value={},
                ),
                contextlib.redirect_stdout(io.StringIO()),
            ):
                exit_code = gate.run_registry_release_gate(
                    gate_options,
                    repository_state=lambda _root: {"gitSha": sha, "worktreeDirty": False},
                    runtime_inspector=lambda _options: {"builder": gate_options.builder},
                    build_runner=build_runner,
                    supply_chain_runner=lambda *_args, **_kwargs: supply_chain_report,
                )
            report = json.loads((output_dir / gate.JSON_REPORT_NAME).read_text(encoding="utf-8"))

        self.assertEqual(exit_code, 1)
        self.assertEqual(report["status"], "fail")
        self.assertIn(
            "release.registry_supply_chain_signer_identity_invalid",
            {error["code"] for error in report["errors"]},
        )

    def test_emits_fail_report_when_no_cache_build_fails(self) -> None:
        sha = subprocess.run(
            ["git", "rev-parse", "HEAD"],
            cwd=REPO_ROOT,
            check=True,
            capture_output=True,
            text=True,
        ).stdout.strip()
        with tempfile.TemporaryDirectory() as directory:
            output_dir = pathlib.Path(directory) / "gate"
            gate_options = options(output_dir)

            def build_runner(_options: Any, **kwargs: Any) -> dict[str, Any]:
                if kwargs["no_cache"]:
                    raise gate.ReleaseGateError(
                        "release.registry_build_failed",
                        "no-cache build failed",
                    )
                return sample_build(kwargs["slot"], kwargs["no_cache"])

            with contextlib.redirect_stdout(io.StringIO()):
                exit_code = gate.run_registry_release_gate(
                    gate_options,
                    repository_state=lambda _root: {"gitSha": sha, "worktreeDirty": False},
                    runtime_inspector=lambda _options: {"builder": gate_options.builder},
                    build_runner=build_runner,
                )
            report = json.loads((output_dir / gate.JSON_REPORT_NAME).read_text(encoding="utf-8"))

        self.assertEqual(exit_code, 1)
        self.assertEqual(report["status"], "fail")
        self.assertEqual(
            {error["code"] for error in report["errors"]},
            {
                "release.registry_build_failed",
                "release.registry_build_coverage_incomplete",
            },
        )

    def test_production_insecure_registry_fails_before_build_execution(self) -> None:
        sha = subprocess.run(
            ["git", "rev-parse", "HEAD"],
            cwd=REPO_ROOT,
            check=True,
            capture_output=True,
            text=True,
        ).stdout.strip()
        with tempfile.TemporaryDirectory() as directory:
            output_dir = pathlib.Path(directory) / "gate"
            gate_options = options(
                output_dir,
                signing_policy_profile="production",
                insecure_registry=True,
            )

            build_runner = mock.Mock()
            with contextlib.redirect_stdout(io.StringIO()):
                exit_code = gate.run_registry_release_gate(
                    gate_options,
                    repository_state=lambda _root: {"gitSha": sha, "worktreeDirty": False},
                    runtime_inspector=lambda _options: {"builder": gate_options.builder},
                    build_runner=build_runner,
                    supply_chain_runner=lambda *_args, **_kwargs: sample_supply_chain(),
                )
            report = json.loads((output_dir / gate.JSON_REPORT_NAME).read_text(encoding="utf-8"))

        self.assertEqual(exit_code, 1)
        self.assertFalse(build_runner.called)
        self.assertEqual(report["errors"][0]["code"], "release.registry_production_signing_insecure_registry")


if __name__ == "__main__":
    unittest.main()
