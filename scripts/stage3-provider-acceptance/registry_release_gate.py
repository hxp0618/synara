#!/usr/bin/env python3
"""Build and verify reproducible registry-pushed multi-arch Worker images."""

from __future__ import annotations

import argparse
import base64
import binascii
import dataclasses
import datetime as dt
import hashlib
import http.client
import json
import pathlib
import re
import shutil
import ssl
import subprocess
import sys
import time
import urllib.parse
import uuid
from collections.abc import Mapping, Sequence
from typing import Any

import acceptance_runner as acceptance
import controlled_remote_release_gate as remote
import release_gate_common as common
import registry_supply_chain as supply_chain


SCHEMA_VERSION = "synara.worker-registry-release-gate.v2"
JSON_REPORT_NAME = "worker-registry-release-gate.json"
MARKDOWN_REPORT_NAME = "worker-registry-release-gate.md"
REQUIRED_PLATFORMS = ("linux/amd64", "linux/arm64")
OCI_INDEX_MEDIA_TYPE = "application/vnd.oci.image.index.v1+json"
OCI_MANIFEST_MEDIA_TYPE = "application/vnd.oci.image.manifest.v1+json"
ATTESTATION_TYPE = "attestation-manifest"
SPDX_PREDICATE = "https://spdx.dev/Document"
SLSA_PREDICATE_PREFIX = "https://slsa.dev/provenance/"
EXPECTED_USER = "10001:10001"
EXPECTED_ENTRYPOINT = ["/usr/local/bin/synara-agentd"]
EXPECTED_WORKING_DIRECTORY = "/data"
EXPECTED_ENVIRONMENT = {
    "HOME": "/home/synara",
    "SYNARA_AGENTD_WORKSPACE_ROOT": "/data/workspaces",
    "SYNARA_AGENTD_WORKER_IMAGE_MANIFEST_PATH": "/opt/synara/worker-image-manifest.json",
    "NPM_CONFIG_UPDATE_NOTIFIER": "false",
}
EMBEDDED_PATHS = {
    "manifest": "/opt/synara/worker-image-manifest.json",
    "sbom": "/opt/synara/provider-tools.spdx.json",
    "providerToolsLock": "/opt/synara/provider-tools/package-lock.json",
    "providerHostLock": "/opt/synara/provider-host/bun.lock",
    "workerAPKLock": "/opt/synara/worker-apk-packages.lock",
    "providerHost": "/opt/synara/provider-host/index.mjs",
    "agentd": "/usr/local/bin/synara-agentd",
    "providerHostWrapper": "/usr/local/bin/provider-host",
    "buildRevision": "/opt/synara/.build-revision",
}
LOCAL_LOCK_PATHS = {
    "provider-tools-npm": pathlib.Path("deploy/worker/provider-tools/package-lock.json"),
    "provider-host-bun": pathlib.Path("bun.lock"),
    "worker-apk": pathlib.Path("deploy/worker/apk-packages.lock"),
}
SBOM_GENERATOR_LOCK_PATH = pathlib.Path("deploy/worker/buildkit-sbom-generator.lock")
EMBEDDED_LOCK_PATHS = {
    "provider-tools-npm": EMBEDDED_PATHS["providerToolsLock"],
    "provider-host-bun": EMBEDDED_PATHS["providerHostLock"],
    "worker-apk": EMBEDDED_PATHS["workerAPKLock"],
}
CREDENTIAL_ENVIRONMENT_PATTERN = re.compile(
    r"(?:TOKEN|SECRET|PASSWORD|PASSWD|CREDENTIAL|API_KEY|PRIVATE_KEY|ACCESS_KEY)",
    re.IGNORECASE,
)
PRODUCTION_REGISTRY_RETENTION_POLICY_PATH = supply_chain.PRODUCTION_REGISTRY_RETENTION_POLICY_PATH
PRODUCTION_REGISTRY_RUNTIME_EVIDENCE_MAX_AGE_SECONDS = 300.0
PRODUCTION_REGISTRY_RUNTIME_IMAGE_REFERENCE = (
    "registry:2.8.3@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"
)
SHA256_HEX_PATTERN = re.compile(r"[0-9a-f]{64}")
SHA256_DIGEST_PATTERN = re.compile(r"sha256:[0-9a-f]{64}")
PEM_CERTIFICATE_PATTERN = re.compile(
    r"-----BEGIN CERTIFICATE-----\n.+?\n-----END CERTIFICATE-----\n?",
    re.DOTALL,
)
CONTAINER_NAME_PATTERN = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,254}")

ReleaseGateError = common.ReleaseGateError


@dataclasses.dataclass(frozen=True)
class RegistryReleaseGateOptions:
    repo_root: pathlib.Path
    output_dir: pathlib.Path
    image_repository: str
    builder: str
    build_timeout_seconds: float
    supply_chain_timeout_seconds: float
    docker_bin: str
    go_proxy: str | None
    insecure_registry: bool
    signing_policy_profile: str
    registry_auth_username_environment: str | None
    registry_auth_password_environment: str | None
    registry_ca_cert_environment: str | None
    production_public_key_configmap_path: pathlib.Path | None = None
    production_repository_configmap_path: pathlib.Path | None = None
    production_registry_config_path: pathlib.Path | None = None
    production_registry_retention_policy_path: pathlib.Path | None = None
    production_registry_container: str | None = None
    production_registry_runtime_config_path: str | None = None
    tool_environment_overrides: dict[str, str] = dataclasses.field(default_factory=dict)


@dataclasses.dataclass(frozen=True)
class HostRegistryClientInputs:
    registry_host: str
    username: str
    password: str
    ca_path: pathlib.Path


def normalize_environment_name(value: str | None, flag: str) -> str | None:
    if value is None:
        return None
    name = value.strip()
    if not name or supply_chain.ENVIRONMENT_NAME_PATTERN.fullmatch(name) is None:
        raise ValueError(f"{flag} must be an uppercase environment variable name")
    return name


def normalize_container_name(value: str | None, flag: str) -> str | None:
    if value is None:
        return None
    name = value.strip()
    if not name or CONTAINER_NAME_PATTERN.fullmatch(name) is None:
        raise ValueError(f"{flag} must be a Docker container name or ID")
    return name


def normalize_container_path(value: str | None, flag: str) -> str | None:
    if value is None:
        return None
    path = value.strip()
    if (
        not path
        or not path.startswith("/")
        or len(path) > 4096
        or any(character in "\r\n\x00" for character in path)
    ):
        raise ValueError(f"{flag} must be an absolute in-container path")
    return path


def _resolve_production_registry_access_environment_names(
    username_environment: str | None,
    password_environment: str | None,
    ca_cert_environment: str | None,
) -> tuple[str, str, str]:
    resolved = (
        username_environment or supply_chain.PRODUCTION_REGISTRY_USERNAME_ENV,
        password_environment or supply_chain.PRODUCTION_REGISTRY_PASSWORD_ENV,
        ca_cert_environment or supply_chain.PRODUCTION_REGISTRY_CA_CERT_ENV,
    )
    if resolved != supply_chain.PRODUCTION_REGISTRY_ACCESS_ENVIRONMENT:
        expected = ", ".join(supply_chain.PRODUCTION_REGISTRY_ACCESS_ENVIRONMENT)
        raise ValueError(
            f"production registry access environment names must remain {expected}"
        )
    return resolved


def parse_args(argv: Sequence[str]) -> RegistryReleaseGateOptions:
    repo_root = pathlib.Path(__file__).resolve().parents[2]
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image-repository", required=True)
    parser.add_argument("--builder", required=True)
    parser.add_argument("--build-timeout", type=float, default=7200.0)
    parser.add_argument("--supply-chain-timeout", type=float, default=1800.0)
    parser.add_argument("--docker-bin", default="docker")
    parser.add_argument("--go-proxy")
    parser.add_argument("--insecure-registry", action="store_true")
    parser.add_argument(
        "--signing-policy-profile",
        choices=supply_chain.SUPPORTED_SIGNING_POLICY_PROFILES,
        default="disposable",
    )
    parser.add_argument("--registry-auth-username-env")
    parser.add_argument("--registry-auth-password-env")
    parser.add_argument("--registry-ca-cert-env")
    parser.add_argument("--production-public-key-configmap", type=pathlib.Path)
    parser.add_argument("--production-repository-configmap", type=pathlib.Path)
    parser.add_argument("--production-registry-config", type=pathlib.Path)
    parser.add_argument("--production-registry-retention-policy", type=pathlib.Path)
    parser.add_argument("--production-registry-container")
    parser.add_argument("--production-registry-runtime-config-path")
    parser.add_argument("--output-dir", type=pathlib.Path)
    parsed = parser.parse_args(argv)
    if parsed.build_timeout <= 0:
        parser.error("--build-timeout must be positive")
    if parsed.supply_chain_timeout <= 0:
        parser.error("--supply-chain-timeout must be positive")
    try:
        image_repository = normalize_image_repository(parsed.image_repository)
        builder = normalize_builder_name(parsed.builder)
        docker_bin = normalize_executable(parsed.docker_bin, "--docker-bin")
        go_proxy = normalize_go_proxy(parsed.go_proxy)
        registry_auth_username_environment = normalize_environment_name(
            parsed.registry_auth_username_env,
            "--registry-auth-username-env",
        )
        registry_auth_password_environment = normalize_environment_name(
            parsed.registry_auth_password_env,
            "--registry-auth-password-env",
        )
        registry_ca_cert_environment = normalize_environment_name(
            parsed.registry_ca_cert_env,
            "--registry-ca-cert-env",
        )
        production_registry_container = normalize_container_name(
            parsed.production_registry_container,
            "--production-registry-container",
        )
        production_registry_runtime_config_path = normalize_container_path(
            parsed.production_registry_runtime_config_path,
            "--production-registry-runtime-config-path",
        )
    except ValueError as error:
        parser.error(str(error))
    registry_env_names = (
        registry_auth_username_environment,
        registry_auth_password_environment,
        registry_ca_cert_environment,
    )
    production_admission_paths = (
        parsed.production_public_key_configmap,
        parsed.production_repository_configmap,
    )
    production_registry_boundary_paths = (
        parsed.production_registry_config,
        parsed.production_registry_retention_policy,
    )
    production_runtime_inputs = (
        production_registry_container,
        production_registry_runtime_config_path,
    )
    if parsed.signing_policy_profile == "production":
        if parsed.insecure_registry:
            parser.error("production signing requires a TLS registry; remove --insecure-registry")
        try:
            (
                registry_auth_username_environment,
                registry_auth_password_environment,
                registry_ca_cert_environment,
            ) = _resolve_production_registry_access_environment_names(
                registry_auth_username_environment,
                registry_auth_password_environment,
                registry_ca_cert_environment,
            )
        except ValueError as error:
            parser.error(str(error))
        if any(value is None for value in production_admission_paths):
            parser.error(
                "--production-public-key-configmap and --production-repository-configmap are required with --signing-policy-profile production"
            )
        if any(value is None for value in production_registry_boundary_paths):
            parser.error(
                "--production-registry-config and --production-registry-retention-policy are required with --signing-policy-profile production"
            )
        if any(value is None for value in production_runtime_inputs):
            parser.error(
                "--production-registry-container and --production-registry-runtime-config-path are required with --signing-policy-profile production"
            )
    elif any(value is not None for value in registry_env_names):
        parser.error(
            "production registry auth/CA environment names are only supported with --signing-policy-profile production"
        )
    elif any(value is not None for value in production_admission_paths):
        parser.error(
            "production admission ConfigMap paths are only supported with --signing-policy-profile production"
        )
    elif any(value is not None for value in production_registry_boundary_paths):
        parser.error(
            "production registry boundary inputs are only supported with --signing-policy-profile production"
        )
    elif any(value is not None for value in production_runtime_inputs):
        parser.error(
            "production registry runtime evidence inputs are only supported with --signing-policy-profile production"
        )
    output_dir = parsed.output_dir or remote.default_output_dir(repo_root, "registry")
    return RegistryReleaseGateOptions(
        repo_root=repo_root,
        output_dir=output_dir.expanduser().resolve(),
        image_repository=image_repository,
        builder=builder,
        build_timeout_seconds=parsed.build_timeout,
        supply_chain_timeout_seconds=parsed.supply_chain_timeout,
        docker_bin=docker_bin,
        go_proxy=go_proxy,
        insecure_registry=parsed.insecure_registry,
        signing_policy_profile=parsed.signing_policy_profile,
        registry_auth_username_environment=registry_auth_username_environment,
        registry_auth_password_environment=registry_auth_password_environment,
        registry_ca_cert_environment=registry_ca_cert_environment,
        production_public_key_configmap_path=(
            parsed.production_public_key_configmap.expanduser().resolve()
            if parsed.production_public_key_configmap is not None
            else None
        ),
        production_repository_configmap_path=(
            parsed.production_repository_configmap.expanduser().resolve()
            if parsed.production_repository_configmap is not None
            else None
        ),
        production_registry_config_path=(
            parsed.production_registry_config.expanduser().resolve()
            if parsed.production_registry_config is not None
            else None
        ),
        production_registry_retention_policy_path=(
            parsed.production_registry_retention_policy.expanduser().resolve()
            if parsed.production_registry_retention_policy is not None
            else None
        ),
        production_registry_container=production_registry_container,
        production_registry_runtime_config_path=production_registry_runtime_config_path,
    )


def normalize_image_repository(value: str) -> str:
    repository = value.strip()
    components = repository.split("/")
    registry = components[0] if components else ""
    repository_components = components[1:]
    registry_match = re.fullmatch(
        r"[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?(?::([0-9]{1,5}))?",
        registry,
    )
    repository_component_pattern = re.compile(
        r"[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*"
    )
    port = registry_match.group(1) if registry_match is not None else None
    if (
        not repository
        or len(repository) > 512
        or any(character.isspace() or ord(character) < 32 for character in repository)
        or any(character in repository for character in "@?#")
        or "://" in repository
        or repository.endswith("/")
        or registry_match is None
        or (port is not None and not 1 <= int(port) <= 65535)
        or not repository_components
        or any(
            repository_component_pattern.fullmatch(component) is None
            for component in repository_components
        )
    ):
        raise ValueError(
            "--image-repository must be a credential-free registry repository without a tag or digest"
        )
    return repository


def normalize_builder_name(value: str) -> str:
    builder = value.strip()
    if re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,127}", builder) is None:
        raise ValueError("--builder must be a valid existing Buildx builder name")
    return builder


def normalize_executable(value: str, flag: str) -> str:
    executable = value.strip()
    if not executable or any(character in executable for character in "\r\n\t\x00"):
        raise ValueError(f"{flag} must be a command or executable path")
    return executable


normalize_go_proxy = common.normalize_go_proxy


def _tool_environment(options: RegistryReleaseGateOptions) -> dict[str, str]:
    environment = remote.tool_environment()
    environment.update(options.tool_environment_overrides)
    return environment


def _tool_completed(
    options: RegistryReleaseGateOptions,
    arguments: Sequence[str],
    *,
    timeout: float = 30.0,
) -> subprocess.CompletedProcess[str]:
    try:
        return subprocess.run(
            [options.docker_bin, *arguments],
            cwd=options.repo_root,
            env=_tool_environment(options),
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            timeout=timeout,
        )
    except (OSError, subprocess.TimeoutExpired):
        raise ReleaseGateError(
            "release.registry_runtime_command_failed",
            "A required Worker Registry release-gate command could not complete.",
            {"executable": pathlib.Path(options.docker_bin).name},
        ) from None


def inspect_runtime(options: RegistryReleaseGateOptions) -> dict[str, Any]:
    docker = _tool_completed(options, ["version", "--format", "{{json .}}"])
    buildx = _tool_completed(options, ["buildx", "version"])
    builder = _tool_completed(options, ["buildx", "inspect", options.builder, "--bootstrap"])
    if any(result.returncode != 0 or not result.stdout.strip() for result in (docker, buildx, builder)):
        raise ReleaseGateError(
            "release.registry_runtime_unavailable",
            "Docker, Buildx, or the selected builder was unavailable.",
            {
                "dockerReturnCode": docker.returncode,
                "buildxReturnCode": buildx.returncode,
                "builderReturnCode": builder.returncode,
            },
        )
    try:
        docker_payload = json.loads(docker.stdout)
    except json.JSONDecodeError:
        docker_payload = None
    server = docker_payload.get("Server") if isinstance(docker_payload, dict) else None
    driver_match = re.search(r"(?m)^Driver:\s+(\S+)\s*$", builder.stdout)
    platform_match = re.search(r"(?m)^Platforms:\s+(.+?)\s*$", builder.stdout)
    status_values = re.findall(r"(?m)^Status:\s+(\S+)\s*$", builder.stdout)
    platforms = {
        item.strip().split("/v", 1)[0]
        for item in (platform_match.group(1).split(",") if platform_match else [])
        if item.strip()
    }
    if (
        not isinstance(server, dict)
        or driver_match is None
        or driver_match.group(1) != "docker-container"
        or not status_values
        or any(status != "running" for status in status_values)
        or not set(REQUIRED_PLATFORMS).issubset(platforms)
    ):
        raise ReleaseGateError(
            "release.registry_builder_invalid",
            "The selected Buildx builder did not provide a running docker-container multi-arch boundary.",
            {
                "driver": driver_match.group(1) if driver_match else None,
                "statuses": status_values,
                "platforms": sorted(platforms),
            },
        )
    return {
        "dockerServerVersion": server.get("Version"),
        "dockerServerArchitecture": server.get("Arch"),
        "buildxVersion": buildx.stdout.strip()[:500],
        "builder": options.builder,
        "builderDriver": driver_match.group(1),
        "platforms": sorted(platforms),
    }


def source_metadata(repo_root: pathlib.Path, git_sha: str) -> tuple[str, str]:
    package_path = repo_root / "apps" / "server" / "package.json"
    try:
        package = json.loads(package_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        raise ReleaseGateError(
            "release.registry_source_metadata_invalid",
            "Worker Registry gate could not read the source package version.",
        ) from None
    version = package.get("version") if isinstance(package, dict) else None
    if not isinstance(version, str) or not version.strip():
        raise ReleaseGateError(
            "release.registry_source_metadata_invalid",
            "Worker Registry gate source package version was missing.",
        )
    try:
        epoch = subprocess.run(
            ["git", "show", "-s", "--format=%ct", git_sha],
            cwd=repo_root,
            env=remote.tool_environment(),
            check=True,
            capture_output=True,
            text=True,
            timeout=15.0,
        ).stdout.strip()
    except (OSError, subprocess.SubprocessError):
        raise ReleaseGateError(
            "release.registry_source_metadata_invalid",
            "Worker Registry gate could not read the source commit timestamp.",
        ) from None
    if re.fullmatch(r"(?:0|[1-9][0-9]*)", epoch) is None:
        raise ReleaseGateError(
            "release.registry_source_metadata_invalid",
            "Worker Registry gate source commit timestamp was invalid.",
        )
    return version.strip(), epoch


def locked_sbom_generator(repo_root: pathlib.Path) -> str:
    try:
        reference = (repo_root / SBOM_GENERATOR_LOCK_PATH).read_text(encoding="utf-8").strip()
    except OSError:
        raise ReleaseGateError(
            "release.registry_source_metadata_invalid",
            "Worker Registry gate could not read the BuildKit SBOM generator lock.",
        ) from None
    if "@" not in reference:
        raise ReleaseGateError(
            "release.registry_source_metadata_invalid",
            "Worker Registry BuildKit SBOM generator was not digest-pinned.",
        )
    repository, digest = reference.rsplit("@", 1)
    try:
        normalize_image_repository(repository)
    except ValueError:
        raise ReleaseGateError(
            "release.registry_source_metadata_invalid",
            "Worker Registry BuildKit SBOM generator reference was invalid.",
        ) from None
    if re.fullmatch(r"sha256:[0-9a-f]{64}", digest) is None:
        raise ReleaseGateError(
            "release.registry_source_metadata_invalid",
            "Worker Registry BuildKit SBOM generator was not digest-pinned.",
        )
    return reference


def expected_created_at(source_date_epoch: str) -> str:
    return dt.datetime.fromtimestamp(int(source_date_epoch), tz=dt.timezone.utc).strftime(
        "%Y-%m-%dT%H:%M:%SZ"
    )


def expected_sbom_created_at(source_date_epoch: str) -> str:
    return dt.datetime.fromtimestamp(int(source_date_epoch), tz=dt.timezone.utc).strftime(
        "%Y-%m-%dT%H:%M:%S.000Z"
    )


def build_command(
    options: RegistryReleaseGateOptions,
    *,
    image: str,
    git_sha: str,
    version: str,
    source_date_epoch: str,
    metadata_path: pathlib.Path,
    no_cache: bool,
) -> list[str]:
    command = [
        str(options.repo_root / "deploy" / "worker" / "build.sh"),
        "--target",
        "worker",
        "--image",
        image,
        "--version",
        version,
        "--git-sha",
        git_sha,
        "--source-date-epoch",
        source_date_epoch,
        "--platform",
        ",".join(REQUIRED_PLATFORMS),
        "--metadata-file",
        str(metadata_path),
        "--builder",
        options.builder,
        "--push",
    ]
    if options.go_proxy is not None:
        command.extend(["--go-proxy", options.go_proxy])
    if no_cache:
        command.append("--no-cache")
    return command


def _run_build(
    options: RegistryReleaseGateOptions,
    command: Sequence[str],
    *,
    slot: str,
) -> None:
    try:
        completed = subprocess.run(
            list(command),
            cwd=options.repo_root,
            env=_tool_environment(options),
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            timeout=options.build_timeout_seconds,
        )
    except (OSError, subprocess.TimeoutExpired):
        raise ReleaseGateError(
            "release.registry_build_failed",
            "A Worker Registry reproducibility build could not complete.",
            {"slot": slot},
        ) from None
    if completed.returncode != 0:
        raise ReleaseGateError(
            "release.registry_build_failed",
            "A Worker Registry reproducibility build failed.",
            {"slot": slot, "returnCode": completed.returncode},
        )


def _load_build_metadata(
    path: pathlib.Path,
    *,
    slot: str,
    expected_sbom_generator: str,
) -> dict[str, Any]:
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        raise ReleaseGateError(
            "release.registry_build_metadata_invalid",
            "A Worker Registry build omitted valid Buildx metadata.",
            {"slot": slot},
        ) from None
    descriptor = payload.get("containerimage.descriptor") if isinstance(payload, dict) else None
    digest = payload.get("containerimage.digest") if isinstance(payload, dict) else None
    expected_generator_digest = expected_sbom_generator.rsplit("@", 1)[-1].removeprefix(
        "sha256:"
    )
    if (
        not isinstance(descriptor, dict)
        or descriptor.get("mediaType") != OCI_INDEX_MEDIA_TYPE
        or descriptor.get("digest") != digest
        or not isinstance(digest, str)
        or re.fullmatch(r"sha256:[0-9a-f]{64}", digest) is None
    ):
        raise ReleaseGateError(
            "release.registry_build_metadata_invalid",
            "A Worker Registry build did not return one immutable OCI index digest.",
            {"slot": slot},
        )
    for platform in REQUIRED_PLATFORMS:
        provenance = payload.get(f"buildx.build.provenance/{platform}")
        materials = provenance.get("materials") if isinstance(provenance, dict) else None
        material_items = materials if isinstance(materials, list) else []
        scanner_materials = [
            material
            for material in material_items
            if isinstance(material, dict)
            and isinstance(material.get("uri"), str)
            and material["uri"].startswith("pkg:docker/docker/buildkit-syft-scanner")
        ]
        scanner_digest = (
            scanner_materials[0].get("digest")
            if len(scanner_materials) == 1
            else None
        )
        if (
            not isinstance(materials, list)
            or len(scanner_materials) != 1
            or not isinstance(scanner_digest, dict)
            or scanner_digest.get("sha256") != expected_generator_digest
        ):
            raise ReleaseGateError(
                "release.registry_build_metadata_invalid",
                "A Worker Registry build did not use the locked BuildKit SBOM generator.",
                {"slot": slot, "platform": platform},
            )
    return {
        "digest": digest,
        "descriptorSize": descriptor.get("size"),
        "sbomGenerator": expected_sbom_generator,
    }


def _json_tool_output(
    options: RegistryReleaseGateOptions,
    arguments: Sequence[str],
    *,
    code: str,
    message: str,
    timeout: float = 60.0,
) -> tuple[dict[str, Any] | list[Any], bytes]:
    try:
        completed = subprocess.run(
            [options.docker_bin, *arguments],
            cwd=options.repo_root,
            env=_tool_environment(options),
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            timeout=timeout,
        )
    except (OSError, subprocess.TimeoutExpired):
        raise ReleaseGateError(code, message) from None
    if completed.returncode != 0:
        raise ReleaseGateError(code, message, {"returnCode": completed.returncode})
    try:
        payload = json.loads(completed.stdout)
    except json.JSONDecodeError:
        raise ReleaseGateError(code, message) from None
    if not isinstance(payload, (dict, list)):
        raise ReleaseGateError(code, message)
    return payload, completed.stdout


def _normalize_image_configs(payload: Mapping[str, Any]) -> dict[str, Mapping[str, Any]]:
    if isinstance(payload.get("architecture"), str) and isinstance(payload.get("os"), str):
        return {f"{payload['os']}/{payload['architecture']}": payload}
    return {
        str(platform): value
        for platform, value in payload.items()
        if isinstance(value, dict)
    }


def _environment_map(config: Mapping[str, Any]) -> dict[str, str]:
    values = config.get("Env")
    if not isinstance(values, list) or not all(isinstance(value, str) for value in values):
        raise ReleaseGateError(
            "release.registry_image_config_invalid",
            "A Worker image config omitted its environment array.",
        )
    result: dict[str, str] = {}
    for value in values:
        name, separator, item = value.partition("=")
        if not separator or not name or name in result:
            raise ReleaseGateError(
                "release.registry_image_config_invalid",
                "A Worker image config contained a malformed or duplicate environment entry.",
            )
        result[name] = item
    return result


def _validate_image_config(
    image: Mapping[str, Any],
    *,
    platform: str,
    git_sha: str,
    version: str,
    created_at: str,
) -> dict[str, Any]:
    os_name, architecture = platform.split("/", 1)
    config = image.get("config")
    rootfs = image.get("rootfs")
    labels = config.get("Labels") if isinstance(config, dict) else None
    environment = _environment_map(config) if isinstance(config, dict) else {}
    risky_names = sorted(
        name for name in environment if CREDENTIAL_ENVIRONMENT_PATTERN.search(name) is not None
    )
    diff_ids = rootfs.get("diff_ids") if isinstance(rootfs, dict) else None
    if (
        image.get("os") != os_name
        or image.get("architecture") != architecture
        or image.get("created") != created_at
        or not isinstance(config, dict)
        or config.get("User") != EXPECTED_USER
        or config.get("Entrypoint") != EXPECTED_ENTRYPOINT
        or config.get("WorkingDir") != EXPECTED_WORKING_DIRECTORY
        or config.get("Cmd") not in (None, [])
        or not isinstance(labels, dict)
        or labels.get("org.opencontainers.image.title") != "Synara Worker"
        or labels.get("org.opencontainers.image.version") != version
        or labels.get("org.opencontainers.image.revision") != git_sha
        or any(environment.get(name) != value for name, value in EXPECTED_ENVIRONMENT.items())
        or risky_names
        or not isinstance(diff_ids, list)
        or not diff_ids
        or any(
            not isinstance(value, str)
            or re.fullmatch(r"sha256:[0-9a-f]{64}", value) is None
            for value in diff_ids
        )
    ):
        raise ReleaseGateError(
            "release.registry_image_config_invalid",
            "A Worker platform image did not preserve the non-root immutable runtime configuration.",
            {"platform": platform, "credentialLikeEnvironmentNames": risky_names},
        )
    return {
        "created": image["created"],
        "user": config["User"],
        "entrypoint": config["Entrypoint"],
        "workingDirectory": config["WorkingDir"],
        "environmentNames": sorted(environment),
        "rootfsDiffIds": list(diff_ids),
    }


def inspect_registry_image(
    options: RegistryReleaseGateOptions,
    *,
    image: str,
    expected_digest: str,
    git_sha: str,
    version: str,
    source_date_epoch: str,
) -> dict[str, Any]:
    index, raw_index = _json_tool_output(
        options,
        ["buildx", "imagetools", "inspect", "--raw", image],
        code="release.registry_index_invalid",
        message="Worker Registry OCI index inspection failed.",
    )
    actual_digest = "sha256:" + hashlib.sha256(raw_index).hexdigest()
    manifests = index.get("manifests")
    if (
        index.get("mediaType") != OCI_INDEX_MEDIA_TYPE
        or actual_digest != expected_digest
        or not isinstance(manifests, list)
    ):
        raise ReleaseGateError(
            "release.registry_index_invalid",
            "Worker Registry metadata and the pushed OCI index digest did not match.",
            {"expectedDigest": expected_digest, "actualDigest": actual_digest},
        )

    platform_descriptors: dict[str, Mapping[str, Any]] = {}
    attestation_descriptors: list[Mapping[str, Any]] = []
    for descriptor in manifests:
        if not isinstance(descriptor, dict):
            raise ReleaseGateError(
                "release.registry_index_invalid",
                "Worker Registry OCI index contained a malformed descriptor.",
            )
        platform_value = descriptor.get("platform")
        os_name = platform_value.get("os") if isinstance(platform_value, dict) else None
        architecture = (
            platform_value.get("architecture") if isinstance(platform_value, dict) else None
        )
        if os_name == "linux" and architecture in {"amd64", "arm64"}:
            key = f"{os_name}/{architecture}"
            if key in platform_descriptors:
                raise ReleaseGateError(
                    "release.registry_platform_invalid",
                    "Worker Registry OCI index contained a duplicate platform descriptor.",
                    {"platform": key},
                )
            platform_descriptors[key] = descriptor
        elif os_name == "unknown" and architecture == "unknown":
            attestation_descriptors.append(descriptor)
        else:
            raise ReleaseGateError(
                "release.registry_platform_invalid",
                "Worker Registry OCI index contained an unexpected platform descriptor.",
                {"os": os_name, "architecture": architecture},
            )
    if set(platform_descriptors) != set(REQUIRED_PLATFORMS):
        raise ReleaseGateError(
            "release.registry_platform_invalid",
            "Worker Registry OCI index did not contain exactly linux/amd64 and linux/arm64.",
            {"platforms": sorted(platform_descriptors)},
        )
    platform_digests: dict[str, str] = {}
    for platform, descriptor in platform_descriptors.items():
        digest = descriptor.get("digest")
        if (
            descriptor.get("mediaType") != OCI_MANIFEST_MEDIA_TYPE
            or not isinstance(digest, str)
            or re.fullmatch(r"sha256:[0-9a-f]{64}", digest) is None
        ):
            raise ReleaseGateError(
                "release.registry_platform_invalid",
                "Worker Registry platform descriptor did not contain an immutable OCI digest.",
                {"platform": platform},
            )
        platform_digests[platform] = digest

    predicates: dict[str, list[str]] = {}
    for platform, subject_digest in platform_digests.items():
        matching = [
            descriptor
            for descriptor in attestation_descriptors
            if isinstance(descriptor.get("annotations"), dict)
            and descriptor["annotations"].get("vnd.docker.reference.type") == ATTESTATION_TYPE
            and descriptor["annotations"].get("vnd.docker.reference.digest") == subject_digest
        ]
        if len(matching) != 1:
            raise ReleaseGateError(
                "release.registry_attestation_invalid",
                "Worker Registry platform did not have exactly one attached attestation manifest.",
                {"platform": platform, "attestationCount": len(matching)},
            )
        attestation_digest = matching[0].get("digest")
        if (
            matching[0].get("mediaType") != OCI_MANIFEST_MEDIA_TYPE
            or not isinstance(attestation_digest, str)
            or re.fullmatch(r"sha256:[0-9a-f]{64}", attestation_digest) is None
        ):
            raise ReleaseGateError(
                "release.registry_attestation_invalid",
                "Worker Registry attestation descriptor was not an immutable OCI manifest.",
                {"platform": platform},
            )
        attestation, _ = _json_tool_output(
            options,
            [
                "buildx",
                "imagetools",
                "inspect",
                "--raw",
                f"{options.image_repository}@{attestation_digest}",
            ],
            code="release.registry_attestation_invalid",
            message="Worker Registry attestation manifest inspection failed.",
        )
        layers = attestation.get("layers")
        layer_items = layers if isinstance(layers, list) else []
        layer_predicates = sorted(
            {
                annotations.get("in-toto.io/predicate-type")
                for layer in layer_items
                if isinstance(layer, dict)
                if isinstance((annotations := layer.get("annotations")), dict)
                and isinstance(annotations.get("in-toto.io/predicate-type"), str)
            }
        )
        if (
            attestation.get("schemaVersion") != 2
            or attestation.get("mediaType") != OCI_MANIFEST_MEDIA_TYPE
            or not isinstance(layers, list)
            or SPDX_PREDICATE not in layer_predicates
            or not any(
                predicate.startswith(SLSA_PREDICATE_PREFIX)
                for predicate in layer_predicates
            )
        ):
            raise ReleaseGateError(
                "release.registry_attestation_invalid",
                "Worker Registry attestation omitted SPDX SBOM or SLSA provenance evidence.",
                {"platform": platform, "predicates": layer_predicates},
            )
        predicates[platform] = layer_predicates
    if len(attestation_descriptors) != len(REQUIRED_PLATFORMS):
        raise ReleaseGateError(
            "release.registry_attestation_invalid",
            "Worker Registry OCI index contained unattached or duplicate attestation descriptors.",
            {"attestationCount": len(attestation_descriptors)},
        )

    images, _ = _json_tool_output(
        options,
        ["buildx", "imagetools", "inspect", image, "--format", "{{json .Image}}"],
        code="release.registry_image_config_invalid",
        message="Worker Registry image-config inspection failed.",
    )
    image_configs = _normalize_image_configs(images)
    if set(image_configs) != set(REQUIRED_PLATFORMS):
        raise ReleaseGateError(
            "release.registry_image_config_invalid",
            "Worker Registry image-config inspection did not return both required platforms.",
            {"platforms": sorted(image_configs)},
        )
    created_at = expected_created_at(source_date_epoch)
    config_evidence = {
        platform: _validate_image_config(
            image_configs[platform],
            platform=platform,
            git_sha=git_sha,
            version=version,
            created_at=created_at,
        )
        for platform in REQUIRED_PLATFORMS
    }
    return {
        "indexDigest": actual_digest,
        "platformDigests": platform_digests,
        "attestationPredicates": predicates,
        "imageConfigs": config_evidence,
    }


def _docker_success(
    options: RegistryReleaseGateOptions,
    arguments: Sequence[str],
    *,
    timeout: float = 120.0,
) -> subprocess.CompletedProcess[str]:
    completed = _tool_completed(options, arguments, timeout=timeout)
    if completed.returncode != 0:
        raise ReleaseGateError(
            "release.registry_embedded_inspection_failed",
            "Worker Registry embedded image inspection command failed.",
            {"operation": arguments[0] if arguments else "unknown"},
        )
    return completed


def _production_boundary_docker_output(
    options: RegistryReleaseGateOptions,
    arguments: Sequence[str],
    *,
    timeout: float = 30.0,
    message: str,
) -> str:
    completed = _tool_completed(options, arguments, timeout=timeout)
    if completed.returncode != 0:
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            message,
            {"operation": arguments[0] if arguments else "unknown"},
        )
    return completed.stdout


def _copy_embedded_files(
    options: RegistryReleaseGateOptions,
    container_name: str,
    destination: pathlib.Path,
) -> dict[str, pathlib.Path]:
    destination.mkdir(parents=True, exist_ok=False)
    result: dict[str, pathlib.Path] = {}
    for name, container_path in EMBEDDED_PATHS.items():
        target = destination / name
        _docker_success(
            options,
            ["cp", f"{container_name}:{container_path}", str(target)],
            timeout=120.0,
        )
        result[name] = target
    return result


def _expected_provider_runtimes(repo_root: pathlib.Path) -> list[dict[str, str]]:
    try:
        provider_lock = json.loads(
            (repo_root / "deploy/worker/provider-tools/package-lock.json").read_text(
                encoding="utf-8"
            )
        )
        provider_package = json.loads(
            (repo_root / "apps/provider-host/package.json").read_text(encoding="utf-8")
        )
    except (OSError, json.JSONDecodeError):
        raise ReleaseGateError(
            "release.registry_embedded_manifest_invalid",
            "Worker Registry gate could not read locked Provider runtime metadata.",
        ) from None
    packages = provider_lock.get("packages") if isinstance(provider_lock, dict) else None
    dependencies = (
        provider_package.get("dependencies") if isinstance(provider_package, dict) else None
    )
    codex = packages.get("node_modules/@openai/codex") if isinstance(packages, dict) else None
    claude = (
        packages.get("node_modules/@anthropic-ai/claude-code")
        if isinstance(packages, dict)
        else None
    )
    sdk = dependencies.get("@anthropic-ai/claude-agent-sdk") if isinstance(dependencies, dict) else None
    versions = {
        "codex": codex.get("version") if isinstance(codex, dict) else None,
        "claude": claude.get("version") if isinstance(claude, dict) else None,
        "sdk": sdk,
    }
    if any(not isinstance(value, str) or not value for value in versions.values()):
        raise ReleaseGateError(
            "release.registry_embedded_manifest_invalid",
            "Worker Registry gate locked Provider runtime metadata was incomplete.",
        )
    return [
        {
            "provider": "claudeAgent",
            "kind": "cli",
            "package": "@anthropic-ai/claude-code",
            "version": str(versions["claude"]),
        },
        {
            "provider": "claudeAgent",
            "kind": "sdk",
            "package": "@anthropic-ai/claude-agent-sdk",
            "version": str(versions["sdk"]),
        },
        {
            "provider": "codex",
            "kind": "cli",
            "package": "@openai/codex",
            "version": str(versions["codex"]),
        },
    ]


def validate_embedded_artifacts(
    options: RegistryReleaseGateOptions,
    files: Mapping[str, pathlib.Path],
    *,
    platform: str,
    git_sha: str,
    version: str,
    source_date_epoch: str,
) -> dict[str, Any]:
    try:
        manifest_bytes = files["manifest"].read_bytes()
        manifest = json.loads(manifest_bytes)
        sbom_bytes = files["sbom"].read_bytes()
        sbom = json.loads(sbom_bytes)
    except (OSError, KeyError, json.JSONDecodeError):
        raise ReleaseGateError(
            "release.registry_embedded_manifest_invalid",
            "Worker Registry image omitted valid embedded Manifest or SBOM data.",
            {"platform": platform},
        ) from None
    if not isinstance(manifest, dict) or not isinstance(sbom, dict):
        raise ReleaseGateError(
            "release.registry_embedded_manifest_invalid",
            "Worker Registry image embedded Manifest and SBOM must be JSON objects.",
            {"platform": platform},
        )
    _, architecture = platform.split("/", 1)
    lockfiles = manifest.get("lockfiles")
    lock_items = lockfiles if isinstance(lockfiles, list) else []
    if (
        not isinstance(lockfiles, list)
        or len(lock_items) != len(LOCAL_LOCK_PATHS)
        or not all(isinstance(item, dict) for item in lock_items)
        or {item.get("name") for item in lock_items if isinstance(item, dict)}
        != set(LOCAL_LOCK_PATHS)
    ):
        raise ReleaseGateError(
            "release.registry_embedded_manifest_invalid",
            "Worker Registry image embedded Manifest lockfile descriptors were malformed.",
            {"platform": platform},
        )
    lock_by_name = {
        item.get("name"): item
        for item in lock_items
        if isinstance(item, dict)
    }
    embedded_lock_files = {
        "provider-tools-npm": files["providerToolsLock"],
        "provider-host-bun": files["providerHostLock"],
        "worker-apk": files["workerAPKLock"],
    }
    lock_hashes: dict[str, str] = {}
    for name, local_relative_path in LOCAL_LOCK_PATHS.items():
        embedded_path = embedded_lock_files[name]
        embedded_bytes = embedded_path.read_bytes()
        local_bytes = (options.repo_root / local_relative_path).read_bytes()
        digest = hashlib.sha256(embedded_bytes).hexdigest()
        descriptor = lock_by_name.get(name)
        if (
            embedded_bytes != local_bytes
            or not isinstance(descriptor, dict)
            or descriptor.get("path") != EMBEDDED_LOCK_PATHS[name]
            or descriptor.get("sha256") != digest
        ):
            raise ReleaseGateError(
                "release.registry_embedded_lock_invalid",
                "Worker Registry image embedded lockfile did not match the clean checkout.",
                {"platform": platform, "lockfile": name},
            )
        lock_hashes[name] = digest
    sboms = manifest.get("sboms")
    sbom_descriptor = sboms[0] if isinstance(sboms, list) and len(sboms) == 1 else None
    sbom_digest = hashlib.sha256(sbom_bytes).hexdigest()
    expected_runtimes = _expected_provider_runtimes(options.repo_root)
    packages = sbom.get("packages")
    package_items = packages if isinstance(packages, list) else []
    described_packages = {
        (item.get("name"), item.get("versionInfo"))
        for item in package_items
        if isinstance(item, dict)
    }
    creation_info = sbom.get("creationInfo")
    if (
        manifest.get("schemaVersion") != 1
        or manifest.get("source") != {"version": version, "gitSha": git_sha}
        or manifest.get("platform") != {"os": "linux", "architecture": architecture}
        or manifest.get("providerRuntimes") != expected_runtimes
        or not isinstance(sbom_descriptor, dict)
        or sbom_descriptor.get("name") != "provider-tools"
        or sbom_descriptor.get("format") != "spdx-json"
        or sbom_descriptor.get("path") != EMBEDDED_PATHS["sbom"]
        or sbom_descriptor.get("sha256") != sbom_digest
        or sbom.get("spdxVersion") != "SPDX-2.3"
        or not isinstance(packages, list)
        or not isinstance(creation_info, dict)
        or creation_info.get("created") != expected_sbom_created_at(source_date_epoch)
        or any(
            (runtime["package"], runtime["version"]) not in described_packages
            for runtime in expected_runtimes
            if runtime["kind"] == "cli"
        )
    ):
        raise ReleaseGateError(
            "release.registry_embedded_manifest_invalid",
            "Worker Registry image embedded Manifest/SBOM did not match the clean source boundary.",
            {"platform": platform},
        )
    base_images = manifest.get("baseImages")
    if (
        not isinstance(base_images, list)
        or len(base_images) != 3
        or any(
            not isinstance(item, dict)
            or not isinstance(item.get("reference"), str)
            or re.fullmatch(r"[^\s@]+@sha256:[0-9a-f]{64}", item["reference"]) is None
            for item in base_images
        )
    ):
        raise ReleaseGateError(
            "release.registry_embedded_manifest_invalid",
            "Worker Registry image did not record three immutable base-image digests.",
            {"platform": platform},
        )
    wrapper = files["providerHostWrapper"].read_text(encoding="utf-8")
    expected_wrapper = '#!/bin/sh\nexec node /opt/synara/provider-host/index.mjs "$@"\n'
    build_revision = files["buildRevision"].read_text(encoding="utf-8")
    if wrapper != expected_wrapper or build_revision != f"{git_sha}\n":
        raise ReleaseGateError(
            "release.registry_embedded_runtime_invalid",
            "Worker Registry image runtime wrapper or release cache identity was invalid.",
            {"platform": platform},
        )
    return {
        "manifestSha256": hashlib.sha256(manifest_bytes).hexdigest(),
        "sbomSha256": sbom_digest,
        "lockfileSha256": lock_hashes,
        "providerHostSha256": common.file_sha256(files["providerHost"]),
        "agentdSha256": common.file_sha256(files["agentd"]),
        "buildRevisionSha256": common.file_sha256(files["buildRevision"]),
        "providerRuntimes": expected_runtimes,
    }


def inspect_embedded_platform(
    options: RegistryReleaseGateOptions,
    *,
    image_digest: str,
    platform: str,
    state_dir: pathlib.Path,
    git_sha: str,
    version: str,
    source_date_epoch: str,
) -> dict[str, Any]:
    reference = f"{options.image_repository}@{image_digest}"
    architecture = platform.split("/", 1)[1]
    container_name = f"synara-stage3-registry-{uuid.uuid4().hex[:12]}-{architecture}"
    destination = state_dir / architecture
    existing = _tool_completed(options, ["image", "inspect", reference], timeout=30.0).returncode == 0
    container_removed = False
    state_removed = False
    image_removed = existing
    try:
        _docker_success(options, ["pull", "--platform", platform, reference], timeout=1800.0)
        _docker_success(
            options,
            ["create", "--platform", platform, "--name", container_name, reference],
            timeout=120.0,
        )
        files = _copy_embedded_files(options, container_name, destination)
        evidence = validate_embedded_artifacts(
            options,
            files,
            platform=platform,
            git_sha=git_sha,
            version=version,
            source_date_epoch=source_date_epoch,
        )
    finally:
        removal = _tool_completed(options, ["rm", "-f", container_name], timeout=60.0)
        container_removed = removal.returncode == 0 or "No such container" in removal.stdout
        shutil.rmtree(destination, ignore_errors=True)
        state_removed = not destination.exists()
        if not existing:
            image_removal = _tool_completed(options, ["image", "rm", reference], timeout=120.0)
            image_removed = image_removal.returncode == 0
    if not container_removed or not state_removed or not image_removed:
        raise ReleaseGateError(
            "release.registry_local_cleanup_failed",
            "Worker Registry embedded inspection did not clean its exact local resources.",
            {
                "platform": platform,
                "containerRemoved": container_removed,
                "stateRemoved": state_removed,
                "localImageRemovedOrPreserved": image_removed,
            },
        )
    return {
        **evidence,
        "cleanup": {
            "containerRemoved": container_removed,
            "stateRemoved": state_removed,
            "localImagePreexisting": existing,
            "localImageRemovedOrPreserved": image_removed,
            "broadCleanupUsed": False,
        },
    }


def build_and_inspect(
    options: RegistryReleaseGateOptions,
    *,
    slot: str,
    image: str,
    git_sha: str,
    version: str,
    source_date_epoch: str,
    state_dir: pathlib.Path,
    no_cache: bool,
) -> dict[str, Any]:
    metadata_path = state_dir / f"build-{slot}.json"
    command = build_command(
        options,
        image=image,
        git_sha=git_sha,
        version=version,
        source_date_epoch=source_date_epoch,
        metadata_path=metadata_path,
        no_cache=no_cache,
    )
    _run_build(options, command, slot=slot)
    metadata = _load_build_metadata(
        metadata_path,
        slot=slot,
        expected_sbom_generator=locked_sbom_generator(options.repo_root),
    )
    registry = inspect_registry_image(
        options,
        image=image,
        expected_digest=metadata["digest"],
        git_sha=git_sha,
        version=version,
        source_date_epoch=source_date_epoch,
    )
    embedded = {
        platform: inspect_embedded_platform(
            options,
            image_digest=registry["platformDigests"][platform],
            platform=platform,
            state_dir=state_dir / f"embedded-{slot}",
            git_sha=git_sha,
            version=version,
            source_date_epoch=source_date_epoch,
        )
        for platform in REQUIRED_PLATFORMS
    }
    return {
        "slot": slot,
        "image": image,
        "noCache": no_cache,
        "registryDigest": metadata["digest"],
        "descriptorSize": metadata["descriptorSize"],
        "sbomGenerator": metadata["sbomGenerator"],
        **registry,
        "embedded": embedded,
    }


def reproducibility_errors(builds: Sequence[Mapping[str, Any]]) -> list[dict[str, Any]]:
    if len(builds) != 2:
        return [
            {
                "code": "release.registry_build_coverage_incomplete",
                "message": "Worker Registry gate did not complete both reproducibility builds.",
                "evidence": {"completedBuilds": len(builds)},
            }
        ]
    platform_maps = [build.get("platformDigests") for build in builds]
    errors: list[dict[str, Any]] = []
    if not all(isinstance(value, dict) for value in platform_maps) or platform_maps[0] != platform_maps[1]:
        errors.append(
            {
                "code": "release.registry_platform_digest_mismatch",
                "message": "Cached and no-cache builds did not reproduce the same platform manifests.",
            }
        )
    for build in builds:
        embedded = build.get("embedded")
        if not isinstance(embedded, dict):
            errors.append(
                {
                    "code": "release.registry_embedded_coverage_incomplete",
                    "message": "A Worker Registry build omitted embedded per-platform evidence.",
                    "evidence": {"slot": build.get("slot")},
                }
            )
            continue
        host_hashes = {
            value.get("providerHostSha256")
            for value in embedded.values()
            if isinstance(value, dict)
        }
        if len(host_hashes) != 1 or None in host_hashes:
            errors.append(
                {
                    "code": "release.registry_provider_host_digest_mismatch",
                    "message": "Worker Registry platform images did not embed one Provider Host bundle.",
                    "evidence": {"slot": build.get("slot")},
                }
            )
    return errors


def markdown_from_report(report: Mapping[str, Any]) -> str:
    lines = [
        "# Stage 3 Worker Registry Release Gate",
        "",
        f"- Schema: `{report['schemaVersion']}`",
        f"- Run: `{report['runId']}`",
        f"- Status: **{report['status']}**",
        f"- Git SHA: `{report.get('source', {}).get('gitSha', '')}`",
        f"- Version: `{report.get('source', {}).get('version', '')}`",
        f"- Duration: `{report['durationMs']} ms`",
        "",
        "The gate performs one cached and one no-cache clean-SHA push. It requires identical linux/amd64 and",
        "linux/arm64 platform manifests, registry-returned OCI digests, per-platform SPDX/SLSA attestations,",
        "non-root runtime config, and embedded Manifest/SBOM/lockfile evidence.",
        "",
        "## Builds",
        "",
        "| Slot | No cache | Registry digest | amd64 manifest | arm64 manifest |",
        "| --- | --- | --- | --- | --- |",
    ]
    for build in report.get("builds", []):
        if not isinstance(build, dict):
            continue
        digests = build.get("platformDigests") if isinstance(build.get("platformDigests"), dict) else {}
        lines.append(
            f"| `{build.get('slot', '')}` | `{build.get('noCache', False)}` | "
            f"`{build.get('registryDigest', '')}` | `{digests.get('linux/amd64', '')}` | "
            f"`{digests.get('linux/arm64', '')}` |"
        )
    supply_chain_report = report.get("supplyChain")
    if isinstance(supply_chain_report, dict) and supply_chain_report:
        signing = supply_chain_report.get("signing")
        signer_identity = (
            signing.get("signerIdentity")
            if isinstance(signing, dict) and isinstance(signing.get("signerIdentity"), dict)
            else None
        )
        vulnerability = supply_chain_report.get("vulnerability")
        scans = vulnerability.get("scans") if isinstance(vulnerability, dict) else None
        lines.extend(
            [
                "",
                "## Supply chain",
                "",
                f"- Status: `{supply_chain_report.get('status', '')}`",
                f"- Signing mode: `{signing.get('mode', '') if isinstance(signing, dict) else ''}`",
                f"- Production signing policy satisfied: "
                f"`{signing.get('productionSigningPolicySatisfied', False) if isinstance(signing, dict) else False}`",
                *(
                    [
                        f"- Vault signer AppRole: `{signer_identity.get('roleName', '')}`",
                        f"- Vault signer token: type `{signer_identity.get('type', '')}`, "
                        f"orphan `{signer_identity.get('orphan', False)}`",
                        f"- Vault signer policies SHA256: `{signer_identity.get('policiesSha256', '')}`",
                    ]
                    if signer_identity is not None
                    else []
                ),
            ]
        )
        if isinstance(scans, list):
            lines.extend(
                [
                    "",
                    "| Platform | Vulnerabilities | Critical | High | Unknown | Secrets | EOSL |",
                    "| --- | --- | --- | --- | --- | --- | --- |",
                ]
            )
            for scan in scans:
                if not isinstance(scan, dict):
                    continue
                summary = scan.get("vulnerabilities")
                by_severity = summary.get("bySeverity") if isinstance(summary, dict) else None
                os_metadata = scan.get("os")
                eol = os_metadata.get("EOSL", False) if isinstance(os_metadata, dict) else None
                lines.append(
                    f"| `{scan.get('platform', '')}` | `{summary.get('total', '') if isinstance(summary, dict) else ''}` | "
                    f"`{by_severity.get('CRITICAL', '') if isinstance(by_severity, dict) else ''}` | "
                    f"`{by_severity.get('HIGH', '') if isinstance(by_severity, dict) else ''}` | "
                    f"`{by_severity.get('UNKNOWN', '') if isinstance(by_severity, dict) else ''}` | "
                    f"`{scan.get('secretFindingCount', '')}` | "
                    f"`{eol if eol is not None else ''}` |"
                )
    runtime = report.get("runtime")
    production_registry = (
        runtime.get("productionRegistryBoundary")
        if isinstance(runtime, dict) and isinstance(runtime.get("productionRegistryBoundary"), dict)
        else None
    )
    if isinstance(production_registry, dict):
        live_runtime_evidence = (
            production_registry.get("liveRuntimeEvidence")
            if isinstance(production_registry.get("liveRuntimeEvidence"), dict)
            else {}
        )
        container = (
            live_runtime_evidence.get("container")
            if isinstance(live_runtime_evidence.get("container"), dict)
            else {}
        )
        runtime_image = (
            container.get("image") if isinstance(container.get("image"), dict) else {}
        )
        lines.extend(
            [
                "",
                "## Production Registry boundary",
                "",
                f"- Delete enabled: `{production_registry.get('deleteEnabled', '')}`",
                f"- Promotion boundary: `{production_registry.get('promotionBoundary', '')}`",
                f"- Release evidence retention days: `{production_registry.get('releaseEvidenceDays', '')}`",
                f"- Garbage collection mode: `{production_registry.get('garbageCollectionMode', '')}`",
                f"- Registry host: `{live_runtime_evidence.get('registryHost', '')}`",
                f"- Registry authority: `{live_runtime_evidence.get('registryAuthority', '')}`",
                f"- Repository authority: `{live_runtime_evidence.get('repositoryAuthority', '')}`",
                f"- TLS peer certificate SHA256: `{live_runtime_evidence.get('tlsPeerCertificateSha256', '')}`",
                f"- Runtime config SHA256: `{live_runtime_evidence.get('runtimeConfigSha256', '')}`",
                f"- Retention policy SHA256: `{live_runtime_evidence.get('retentionPolicySha256', '')}`",
                f"- Runtime evidence collected: `{live_runtime_evidence.get('collectedAt', '')}`",
                f"- Runtime container: `{container.get('name', '')}`",
                f"- Expected Registry runtime image: `{runtime_image.get('expectedReference', '')}`",
                f"- Registry runtime image ID: `{runtime_image.get('runtimeId', '')}`",
                f"- Matched Registry RepoDigest: `{runtime_image.get('matchedRepoDigest', '')}`",
            ]
        )
    errors = report.get("errors")
    if isinstance(errors, list) and errors:
        lines.extend(
            [
                "",
                "## Errors",
                "",
                "```json",
                json.dumps(errors, indent=2, sort_keys=True, ensure_ascii=False),
                "```",
            ]
        )
    production_signing = (
        isinstance(supply_chain_report, dict)
        and isinstance(supply_chain_report.get("signing"), dict)
        and supply_chain_report["signing"].get("productionSigningPolicySatisfied") is True
    )
    lines.extend(
        [
            "",
            "## Evidence boundary",
            "",
            "A pass closes clean-SHA registry push, required multi-arch shape, reproducible platform content,",
            "embedded supply-chain inputs, BuildKit SBOM/provenance attachment, digest signing mechanics, and the",
            "checked-in vulnerability policy.",
            *(
                [
                    "This run also enforced the checked-in production KMS/keyless identity and transparency-log",
                    "policy, plus the checked-in delete-disabled, digest-only Registry retention and archive-first GC boundary.",
                ]
                if production_signing
                else [
                    "Ephemeral signing does not prove production KMS/keyless identity, transparency-log policy, or",
                    "production Registry retention/GC policy.",
                ]
            ),
            "Real Provider four-Target rollout and soak remain open.",
        ]
    )
    return "\n".join(lines) + "\n"


def write_report(
    report: Mapping[str, Any],
    output_dir: pathlib.Path,
    redactor: acceptance.SecretRedactor,
) -> tuple[pathlib.Path, pathlib.Path]:
    sanitized = redactor.value(report)
    json_path = output_dir / JSON_REPORT_NAME
    markdown_path = output_dir / MARKDOWN_REPORT_NAME
    json_path.write_text(
        json.dumps(sanitized, indent=2, sort_keys=True, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )
    markdown_path.write_text(markdown_from_report(sanitized), encoding="utf-8")
    return json_path, markdown_path


def configuration_evidence(options: RegistryReleaseGateOptions) -> dict[str, Any]:
    evidence = {
        "imageRepository": options.image_repository,
        "builder": options.builder,
        "platforms": list(REQUIRED_PLATFORMS),
        "target": "worker",
        "cachedBuild": True,
        "independentNoCacheBuild": True,
        "buildKitSBOM": True,
        "buildKitProvenance": "mode=max",
        "sourceDateEpochLayerRewrite": True,
        "supplyChainRequired": True,
        "signingPolicyProfile": options.signing_policy_profile,
        "signingPolicyRequired": True,
        "signingPolicy": str(supply_chain.signing_policy_path(options.signing_policy_profile)),
        "vulnerabilityPolicy": str(supply_chain.VULNERABILITY_POLICY_PATH),
        "insecureRegistry": options.insecure_registry,
        "goProxyOverride": options.go_proxy is not None,
        "remoteImagesRetainedAsReleaseEvidence": True,
        "remoteBroadCleanupUsed": False,
    }
    if options.signing_policy_profile == "production":
        evidence["productionSigningProfile"] = str(supply_chain.PRODUCTION_SIGNING_PROFILE_PATH)
        evidence["productionAdmissionInputs"] = {
            "publicKeyConfigMap": str(options.production_public_key_configmap_path),
            "repositoryConfigMap": str(options.production_repository_configmap_path),
        }
        evidence["productionRegistryBoundaryInputs"] = {
            "registryConfig": str(options.production_registry_config_path),
            "retentionPolicy": str(options.production_registry_retention_policy_path),
        }
        evidence["productionRegistryRuntimeEvidenceInputs"] = {
            "container": options.production_registry_container,
            "runtimeConfigPath": options.production_registry_runtime_config_path,
        }
    if options.registry_auth_username_environment is not None:
        evidence["registryAccess"] = {
            "usernameEnvironment": options.registry_auth_username_environment,
            "passwordEnvironment": options.registry_auth_password_environment,
            "caCertEnvironment": options.registry_ca_cert_environment,
        }
    return evidence


def _host_registry_access_options(
    options: RegistryReleaseGateOptions,
    *,
    state_dir: pathlib.Path,
) -> supply_chain.SupplyChainOptions:
    return supply_chain.SupplyChainOptions(
        repo_root=options.repo_root,
        state_dir=state_dir / "host-registry-access",
        image_repository=options.image_repository,
        docker_bin=options.docker_bin,
        timeout_seconds=options.supply_chain_timeout_seconds,
        insecure_registry=options.insecure_registry,
        registry_auth_username_environment=options.registry_auth_username_environment,
        registry_auth_password_environment=options.registry_auth_password_environment,
        registry_ca_cert_environment=options.registry_ca_cert_environment,
    )


def _prepare_host_registry_access(
    options: RegistryReleaseGateOptions,
    *,
    state_dir: pathlib.Path,
    redactor: acceptance.SecretRedactor,
) -> supply_chain.PreparedRegistryAccess:
    return supply_chain._prepare_registry_access(
        _host_registry_access_options(options, state_dir=state_dir),
        redactor=redactor,
    )


def _prepare_host_registry_environment(
    options: RegistryReleaseGateOptions,
    *,
    state_dir: pathlib.Path,
    redactor: acceptance.SecretRedactor,
) -> dict[str, str]:
    return _prepare_host_registry_access(
        options,
        state_dir=state_dir,
        redactor=redactor,
    ).host_environment


def _read_json_path(path: pathlib.Path, *, code: str, message: str) -> dict[str, Any]:
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except OSError:
        raise ReleaseGateError(code, message, {"path": str(path)}) from None
    except json.JSONDecodeError:
        raise ReleaseGateError(code, message, {"path": str(path)}) from None
    if not isinstance(payload, dict):
        raise ReleaseGateError(code, message, {"path": str(path)})
    return payload


def _production_registry_runtime_image_contract(reference: Any) -> dict[str, str]:
    if reference != PRODUCTION_REGISTRY_RUNTIME_IMAGE_REFERENCE:
        raise ValueError("production Registry runtime image must match the checked-in exact tag and digest")
    tagged_repository, separator, digest = reference.rpartition("@")
    last_slash = tagged_repository.rfind("/")
    tag_separator = tagged_repository.rfind(":")
    if (
        not separator
        or SHA256_DIGEST_PATTERN.fullmatch(digest) is None
        or tag_separator <= last_slash
        or tag_separator == len(tagged_repository) - 1
    ):
        raise ValueError("production Registry runtime image must use an exact version tag and sha256 digest")
    repository = tagged_repository[:tag_separator]
    tag = tagged_repository[tag_separator + 1 :]
    if not repository or tag == "latest":
        raise ValueError("production Registry runtime image must use an exact version tag and sha256 digest")
    return {
        "reference": reference,
        "digest": digest,
        "repoDigest": f"{repository}@{digest}",
    }


def _normalize_retention_policy(
    payload: Mapping[str, Any],
    *,
    code: str,
    runtime_config_path: str | None = None,
) -> dict[str, Any]:
    immutability = payload.get("immutability")
    retention = payload.get("retention")
    garbage_collection = payload.get("garbageCollection")
    release_days = retention.get("releaseEvidenceDays") if isinstance(retention, dict) else None
    registry_config_path = payload.get("registryConfigPath")
    try:
        runtime_image = _production_registry_runtime_image_contract(payload.get("runtimeImage"))
    except ValueError:
        runtime_image = None
    if (
        payload.get("schemaVersion") != 1
        or not isinstance(registry_config_path, str)
        or not registry_config_path.strip()
        or runtime_image is None
        or not isinstance(immutability, dict)
        or immutability.get("deleteEnabled") is not False
        or immutability.get("promotionBoundary") != "digest-only"
        or not isinstance(retention, dict)
        or not isinstance(release_days, int)
        or release_days <= 0
        or retention.get("appliesAfterArchive") is not True
        or not isinstance(garbage_collection, dict)
        or garbage_collection.get("mode") != "manual-after-archive"
        or garbage_collection.get("requiresReleaseEvidenceArchive") is not True
    ):
        raise ReleaseGateError(
            code,
            "Worker Registry retention policy did not preserve the required delete-disabled, digest-only, archive-first boundary.",
        )
    normalized_registry_config_path = registry_config_path.strip()
    if (
        runtime_config_path is not None
        and normalized_registry_config_path != runtime_config_path
    ):
        raise ReleaseGateError(
            code,
            "Worker Registry retention policy did not bind the declared runtime config path to the active production runtime path.",
            {
                "registryConfigPath": normalized_registry_config_path,
                "runtimeConfigPath": runtime_config_path,
            },
        )
    return {
        "schemaVersion": 1,
        "registryConfigPath": normalized_registry_config_path,
        "runtimeImage": runtime_image["reference"],
        "immutability": {
            "deleteEnabled": False,
            "promotionBoundary": "digest-only",
        },
        "retention": {
            "releaseEvidenceDays": release_days,
            "appliesAfterArchive": True,
        },
        "garbageCollection": {
            "mode": "manual-after-archive",
            "requiresReleaseEvidenceArchive": True,
        },
    }


def _stable_json_sha256(value: Mapping[str, Any]) -> str:
    return hashlib.sha256(
        json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode(
            "utf-8"
        )
    ).hexdigest()


def _normalize_text_content(text: str) -> str:
    normalized = text.replace("\r\n", "\n").replace("\r", "\n")
    return normalized if normalized.endswith("\n") else normalized + "\n"


def _parse_yaml_scalar(value: str) -> str:
    text = _strip_yaml_inline_comment(value).strip()
    if len(text) >= 2 and text[0] == text[-1] and text[0] in {'"', "'"}:
        return text[1:-1]
    lowered = text.lower()
    if lowered == "true":
        return True
    if lowered == "false":
        return False
    if re.fullmatch(r"-?[0-9]+", text) is not None:
        return int(text)
    return text


def _strip_yaml_inline_comment(value: str) -> str:
    result: list[str] = []
    quote: str | None = None
    escaped = False
    for character in value:
        if quote is None:
            if character == "#":
                break
            if character in {'"', "'"}:
                quote = character
        else:
            if quote == '"' and character == "\\" and not escaped:
                escaped = True
                result.append(character)
                continue
            if character == quote and not escaped:
                quote = None
            escaped = False
        result.append(character)
    return "".join(result).rstrip()


def _raise_registry_yaml_error(
    *,
    code: str,
    message: str,
    line: int | None = None,
    path: Sequence[str] | None = None,
) -> None:
    evidence: dict[str, Any] = {}
    if line is not None:
        evidence["line"] = line
    if path is not None:
        evidence["path"] = ".".join(path)
    raise ReleaseGateError(code, message, evidence or None)


def _yaml_tokens(
    text: str,
    *,
    code: str,
    message: str,
) -> list[tuple[int, str, int]]:
    tokens: list[tuple[int, str, int]] = []
    for line_number, raw_line in enumerate(_normalize_text_content(text).splitlines(), start=1):
        leading_width = len(raw_line) - len(raw_line.lstrip(" \t"))
        indentation = raw_line[:leading_width]
        if "\t" in indentation:
            _raise_registry_yaml_error(
                code=code,
                message=message,
                line=line_number,
            )
        without_comment = _strip_yaml_inline_comment(raw_line)
        if not without_comment.strip():
            continue
        indent = len(without_comment) - len(without_comment.lstrip(" "))
        if indent % 2 != 0:
            _raise_registry_yaml_error(
                code=code,
                message=message,
                line=line_number,
            )
        content = without_comment[indent:].rstrip()
        tokens.append((indent, content, line_number))
    return tokens


def _split_yaml_key_value(
    content: str,
    *,
    code: str,
    message: str,
    line: int,
) -> tuple[str, str]:
    key, separator, remainder = content.partition(":")
    if not separator or not key.strip():
        _raise_registry_yaml_error(
            code=code,
            message=message,
            line=line,
        )
    return key.strip(), remainder.lstrip()


def _parse_yaml_block(
    tokens: Sequence[tuple[int, str, int]],
    start_index: int,
    indent: int,
    *,
    code: str,
    message: str,
) -> tuple[Any, int]:
    mapping: dict[str, Any] = {}
    sequence: list[Any] | None = None
    index = start_index
    while index < len(tokens):
        token_indent, content, line_number = tokens[index]
        if token_indent < indent:
            break
        if token_indent > indent:
            _raise_registry_yaml_error(
                code=code,
                message=message,
                line=line_number,
            )
        if content.startswith("- "):
            if mapping:
                _raise_registry_yaml_error(
                    code=code,
                    message=message,
                    line=line_number,
                )
            if sequence is None:
                sequence = []
            item_text = content[2:].strip()
            index += 1
            if item_text:
                sequence.append(_parse_yaml_scalar(item_text))
                continue
            if index >= len(tokens) or tokens[index][0] <= token_indent:
                _raise_registry_yaml_error(
                    code=code,
                    message=message,
                    line=line_number,
                )
            child, index = _parse_yaml_block(
                tokens,
                index,
                tokens[index][0],
                code=code,
                message=message,
            )
            sequence.append(child)
            continue
        if sequence is not None:
            _raise_registry_yaml_error(
                code=code,
                message=message,
                line=line_number,
            )
        key, value = _split_yaml_key_value(
            content,
            code=code,
            message=message,
            line=line_number,
        )
        if key in mapping:
            _raise_registry_yaml_error(
                code=code,
                message=message,
                line=line_number,
            )
        index += 1
        if value:
            mapping[key] = _parse_yaml_scalar(value)
            continue
        if index < len(tokens) and tokens[index][0] > token_indent:
            child, index = _parse_yaml_block(
                tokens,
                index,
                tokens[index][0],
                code=code,
                message=message,
            )
            mapping[key] = child
        else:
            mapping[key] = None
    return (sequence if sequence is not None else mapping), index


def _parse_registry_runtime_config(
    text: str,
    *,
    code: str,
    message: str,
) -> dict[str, Any]:
    tokens = _yaml_tokens(text, code=code, message=message)
    if not tokens:
        raise ReleaseGateError(code, message)
    if tokens[0][0] != 0:
        _raise_registry_yaml_error(code=code, message=message, line=tokens[0][2])
    parsed, index = _parse_yaml_block(tokens, 0, 0, code=code, message=message)
    if index != len(tokens) or not isinstance(parsed, dict):
        raise ReleaseGateError(code, message)
    return parsed


def _yaml_mapping_at_path(
    root: Mapping[str, Any],
    path: Sequence[str],
    *,
    code: str,
    message: str,
) -> Mapping[str, Any]:
    current: Any = root
    traversed: list[str] = []
    for key in path:
        traversed.append(key)
        if not isinstance(current, Mapping) or key not in current:
            _raise_registry_yaml_error(
                code=code,
                message=message,
                path=traversed,
            )
        current = current[key]
    if not isinstance(current, Mapping):
        _raise_registry_yaml_error(
            code=code,
            message=message,
            path=path,
        )
    return current


def _yaml_string_at_path(
    root: Mapping[str, Any],
    path: Sequence[str],
    *,
    code: str,
    message: str,
) -> str:
    current: Any = root
    traversed: list[str] = []
    for key in path:
        traversed.append(key)
        if not isinstance(current, Mapping) or key not in current:
            _raise_registry_yaml_error(
                code=code,
                message=message,
                path=traversed,
            )
        current = current[key]
    if not isinstance(current, str) or not current.strip():
        _raise_registry_yaml_error(
            code=code,
            message=message,
            path=path,
        )
    return current.strip()


def _yaml_bool_at_path(
    root: Mapping[str, Any],
    path: Sequence[str],
    *,
    code: str,
    message: str,
) -> bool:
    current: Any = root
    traversed: list[str] = []
    for key in path:
        traversed.append(key)
        if not isinstance(current, Mapping) or key not in current:
            _raise_registry_yaml_error(
                code=code,
                message=message,
                path=traversed,
            )
        current = current[key]
    if not isinstance(current, bool):
        _raise_registry_yaml_error(
            code=code,
            message=message,
            path=path,
        )
    return current


def _inspect_runtime_registry_config(
    config_text: str,
    *,
    registry_host: str,
    code: str,
    invalid_message: str,
    authority_message: str,
) -> dict[str, Any]:
    parsed = _parse_registry_runtime_config(
        config_text,
        code=code,
        message=invalid_message,
    )
    delete_enabled = _yaml_bool_at_path(
        parsed,
        ("storage", "delete", "enabled"),
        code=code,
        message=invalid_message,
    )
    registry_authority = _yaml_string_at_path(
        parsed,
        ("http", "host"),
        code=code,
        message=authority_message,
    )
    _yaml_mapping_at_path(
        parsed,
        ("http", "tls"),
        code=code,
        message=authority_message,
    )
    certificate_path = _yaml_string_at_path(
        parsed,
        ("http", "tls", "certificate"),
        code=code,
        message=authority_message,
    )
    parsed_authority = urllib.parse.urlparse(registry_authority)
    if (
        parsed_authority.scheme != "https"
        or parsed_authority.netloc != registry_host
        or parsed_authority.path not in {"", "/"}
        or parsed_authority.params
        or parsed_authority.query
        or parsed_authority.fragment
    ):
        raise ReleaseGateError(
            code,
            authority_message,
            {
                "registryAuthority": registry_authority,
                "expectedRegistryHost": registry_host,
            },
        )
    try:
        normalized_certificate_path = normalize_container_path(
            certificate_path,
            "runtime tls certificate path",
        )
    except ValueError:
        raise ReleaseGateError(
            code,
            authority_message,
            {"registryAuthority": registry_authority},
        ) from None
    assert normalized_certificate_path is not None
    return {
        "deleteEnabled": delete_enabled,
        "registryAuthority": registry_authority,
        "certificatePath": normalized_certificate_path,
    }


def _parse_runtime_boundary_timestamp(value: Any, *, field: str) -> dt.datetime:
    if not isinstance(value, str) or not value.strip():
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "Worker Registry live runtime evidence was invalid.",
            {"field": field},
        )
    candidate = value.strip()
    if candidate.endswith("Z"):
        candidate = candidate[:-1] + "+00:00"
    try:
        parsed = dt.datetime.fromisoformat(candidate)
    except ValueError:
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "Worker Registry live runtime evidence was invalid.",
            {"field": field},
        ) from None
    if parsed.tzinfo is None:
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "Worker Registry live runtime evidence was invalid.",
            {"field": field},
        )
    return parsed.astimezone(dt.timezone.utc)


def _extract_runtime_registry_details(
    config_text: str,
    *,
    registry_host: str,
) -> tuple[str, str]:
    details = _inspect_runtime_registry_config(
        config_text,
        registry_host=registry_host,
        code="release.registry_production_boundary_invalid",
        invalid_message="The live Worker Registry configuration export was not valid strict YAML.",
        authority_message="The live Worker Registry configuration did not expose the exact TLS authority boundary.",
    )
    return details["registryAuthority"], details["certificatePath"]


def _pem_certificate_sha256(
    text: str,
    *,
    field: str,
) -> str:
    normalized = _normalize_text_content(text)
    match = PEM_CERTIFICATE_PATTERN.search(normalized)
    if match is None:
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "Worker Registry live runtime evidence was invalid.",
            {"field": field},
        )
    certificate_text = match.group(0)
    try:
        der = ssl.PEM_cert_to_DER_cert(
            certificate_text if certificate_text.endswith("\n") else certificate_text + "\n"
        )
    except ValueError:
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "Worker Registry live runtime evidence was invalid.",
            {"field": field},
        ) from None
    return hashlib.sha256(der).hexdigest()


def _load_host_registry_client_inputs(
    options: RegistryReleaseGateOptions,
    *,
    state_dir: pathlib.Path,
    prepared_registry_access: supply_chain.PreparedRegistryAccess,
) -> HostRegistryClientInputs:
    if not prepared_registry_access.auth_configured or not prepared_registry_access.ca_materialized:
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "Production Worker Registry signing omitted live registry credential evidence.",
        )
    registry_options = _host_registry_access_options(options, state_dir=state_dir)
    paths = supply_chain._registry_state_paths(registry_options)
    try:
        docker_config = json.loads(paths["docker_config"].read_text(encoding="utf-8"))
        auth_entry = (
            docker_config.get("auths", {}).get(prepared_registry_access.registry_host)
            if isinstance(docker_config, dict)
            else None
        )
        auth_value = auth_entry.get("auth") if isinstance(auth_entry, dict) else None
        if not isinstance(auth_value, str) or not auth_value:
            raise ValueError
        decoded = base64.b64decode(auth_value, validate=True).decode("utf-8")
        username, separator, password = decoded.partition(":")
        ca_path = paths["registry_ca"].resolve(strict=True)
    except (OSError, ValueError, binascii.Error, UnicodeDecodeError, json.JSONDecodeError):
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "Production Worker Registry signing omitted live registry credential evidence.",
        ) from None
    if not separator or not username or not password:
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "Production Worker Registry signing omitted live registry credential evidence.",
        )
    return HostRegistryClientInputs(
        registry_host=prepared_registry_access.registry_host,
        username=username,
        password=password,
        ca_path=ca_path,
    )


def _registry_https_request(
    inputs: HostRegistryClientInputs,
    method: str,
    path: str,
    *,
    authenticated: bool,
) -> tuple[int, Mapping[str, str], str]:
    context = ssl.create_default_context(cafile=str(inputs.ca_path))
    headers: dict[str, str] = {}
    if authenticated:
        token = base64.b64encode(f"{inputs.username}:{inputs.password}".encode("utf-8")).decode("ascii")
        headers["Authorization"] = f"Basic {token}"
    connection = http.client.HTTPSConnection(
        inputs.registry_host,
        timeout=20.0,
        context=context,
    )
    try:
        connection.request(method, path, headers=headers)
        response = connection.getresponse()
        response.read()
        certificate = connection.sock.getpeercert(binary_form=True) if connection.sock else None
        return (
            response.status,
            dict(response.getheaders()),
            hashlib.sha256(certificate or b"").hexdigest(),
        )
    except OSError:
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "The live Worker Registry TLS probe could not complete.",
            {"registryHost": inputs.registry_host},
        ) from None
    finally:
        connection.close()


def _probe_live_registry_boundary(
    inputs: HostRegistryClientInputs,
    *,
    image_repository: str,
) -> dict[str, Any]:
    unauth_status, unauth_headers, unauth_certificate_sha256 = _registry_https_request(
        inputs,
        "GET",
        "/v2/",
        authenticated=False,
    )
    auth_status, _auth_headers, auth_certificate_sha256 = _registry_https_request(
        inputs,
        "GET",
        "/v2/",
        authenticated=True,
    )
    challenge = unauth_headers.get("WWW-Authenticate", "")
    if unauth_status != 401 or "basic" not in challenge.lower():
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "The live Worker Registry did not enforce an unauthenticated Basic-auth TLS challenge.",
            {"status": unauth_status, "challenge": challenge[:200]},
        )
    if auth_status != 200:
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "The live Worker Registry did not accept authenticated TLS access.",
            {"status": auth_status},
        )
    if auth_certificate_sha256 != unauth_certificate_sha256:
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "The live Worker Registry TLS peer certificate drifted across probes.",
        )
    repository = image_repository.split("/", 1)[1]
    repository_path = "/".join(
        urllib.parse.quote(component, safe="")
        for component in repository.split("/")
    )
    repository_status, _repository_headers, repository_certificate_sha256 = _registry_https_request(
        inputs,
        "GET",
        f"/v2/{repository_path}/tags/list?n=1",
        authenticated=True,
    )
    if repository_certificate_sha256 != auth_certificate_sha256:
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "The live Worker Registry TLS peer certificate drifted across repository probes.",
        )
    if repository_status not in {200, 404}:
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "The live Worker Registry did not prove the exact repository authority boundary.",
            {"status": repository_status, "repositoryAuthority": image_repository},
        )
    return {
        "registryHost": inputs.registry_host,
        "repositoryProbeStatus": repository_status,
        "tlsPeerCertificateSha256": auth_certificate_sha256,
    }


def _inspect_live_registry_runtime_image(
    options: RegistryReleaseGateOptions,
    container_payload: Mapping[str, Any],
    *,
    expected_reference: str,
) -> dict[str, str]:
    contract = _production_registry_runtime_image_contract(expected_reference)
    config = container_payload.get("Config")
    config_reference = config.get("Image") if isinstance(config, Mapping) else None
    runtime_image_id = container_payload.get("Image")
    if (
        config_reference != contract["reference"]
        or not isinstance(runtime_image_id, str)
        or SHA256_DIGEST_PATTERN.fullmatch(runtime_image_id) is None
    ):
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "The live Worker Registry container did not use the checked-in exact runtime image.",
            {
                "expectedImageReference": contract["reference"],
                "configImageReference": config_reference,
                "runtimeImageId": runtime_image_id,
            },
        )
    inspect_payload, _inspect_raw = _json_tool_output(
        options,
        ["image", "inspect", runtime_image_id],
        code="release.registry_production_boundary_invalid",
        message="The live Worker Registry runtime image inspection could not complete.",
    )
    image_payload = (
        inspect_payload[0]
        if isinstance(inspect_payload, list) and len(inspect_payload) == 1
        else None
    )
    inspected_image_id = image_payload.get("Id") if isinstance(image_payload, Mapping) else None
    repo_digests = image_payload.get("RepoDigests") if isinstance(image_payload, Mapping) else None
    matched_repo_digest = (
        next(
            (
                item
                for item in repo_digests
                if isinstance(item, str) and item == contract["repoDigest"]
            ),
            None,
        )
        if isinstance(repo_digests, list)
        else None
    )
    if inspected_image_id != runtime_image_id or matched_repo_digest is None:
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "The live Worker Registry runtime image ID or RepoDigest did not match the checked-in image pin.",
            {
                "expectedImageReference": contract["reference"],
                "expectedRepoDigest": contract["repoDigest"],
                "containerRuntimeImageId": runtime_image_id,
                "inspectedRuntimeImageId": inspected_image_id,
            },
        )
    return {
        "expectedReference": contract["reference"],
        "expectedDigest": contract["digest"],
        "configReference": config_reference,
        "runtimeId": runtime_image_id,
        "matchedRepoDigest": matched_repo_digest,
    }


def _collect_live_registry_boundary_evidence(
    options: RegistryReleaseGateOptions,
    *,
    state_dir: pathlib.Path,
    prepared_registry_access: supply_chain.PreparedRegistryAccess,
    exported_config_sha256: str,
    live_policy_sha256: str,
    checked_in_policy_sha256: str,
    expected_runtime_image_reference: str,
) -> dict[str, Any]:
    if (
        options.production_registry_container is None
        or options.production_registry_runtime_config_path is None
    ):
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "Production Worker Registry signing omitted live runtime evidence inputs.",
        )
    inspect_payload, _inspect_raw = _json_tool_output(
        options,
        ["inspect", options.production_registry_container],
        code="release.registry_production_boundary_invalid",
        message="The live Worker Registry container inspection could not complete.",
    )
    container_payload = inspect_payload[0] if isinstance(inspect_payload, list) and inspect_payload else None
    state = container_payload.get("State") if isinstance(container_payload, dict) else None
    container_name = container_payload.get("Name") if isinstance(container_payload, dict) else None
    container_id = container_payload.get("Id") if isinstance(container_payload, dict) else None
    started_at = state.get("StartedAt") if isinstance(state, dict) else None
    if (
        not isinstance(container_payload, dict)
        or not isinstance(state, dict)
        or state.get("Running") is not True
        or not isinstance(container_name, str)
        or not isinstance(container_id, str)
        or SHA256_HEX_PATTERN.fullmatch(container_id[:64]) is None
        or not isinstance(started_at, str)
        or not started_at.strip()
    ):
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "The live Worker Registry container did not expose a running production runtime boundary.",
            {"container": options.production_registry_container},
        )
    runtime_image = _inspect_live_registry_runtime_image(
        options,
        container_payload,
        expected_reference=expected_runtime_image_reference,
    )
    live_config = _production_boundary_docker_output(
        options,
        ["exec", options.production_registry_container, "cat", options.production_registry_runtime_config_path],
        timeout=30.0,
        message="The live Worker Registry runtime configuration could not be read from the running container.",
    )
    runtime_config_sha256 = hashlib.sha256(
        _normalize_text_content(live_config).encode("utf-8")
    ).hexdigest()
    if runtime_config_sha256 != exported_config_sha256:
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "The exported Worker Registry configuration did not match the live container runtime configuration.",
            {
                "runtimeConfigSha256": runtime_config_sha256,
                "exportedConfigSha256": exported_config_sha256,
            },
        )
    client_inputs = _load_host_registry_client_inputs(
        options,
        state_dir=state_dir,
        prepared_registry_access=prepared_registry_access,
    )
    registry_authority, certificate_path = _extract_runtime_registry_details(
        live_config,
        registry_host=client_inputs.registry_host,
    )
    tls_certificate_text = _production_boundary_docker_output(
        options,
        ["exec", options.production_registry_container, "cat", certificate_path],
        timeout=30.0,
        message="The live Worker Registry TLS certificate could not be read from the running container.",
    )
    runtime_certificate_sha256 = _pem_certificate_sha256(
        tls_certificate_text,
        field="liveRuntimeEvidence.runtimeCertificate",
    )
    tls_probe = _probe_live_registry_boundary(
        client_inputs,
        image_repository=options.image_repository,
    )
    if tls_probe["tlsPeerCertificateSha256"] != runtime_certificate_sha256:
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "The live Worker Registry TLS peer certificate did not match the running container certificate.",
            {
                "runtimeCertificateSha256": runtime_certificate_sha256,
                "tlsPeerCertificateSha256": tls_probe["tlsPeerCertificateSha256"],
            },
        )
    evidence = {
        "collectedAt": acceptance.utc_now(),
        "registryHost": client_inputs.registry_host,
        "registryAuthority": registry_authority,
        "repositoryAuthority": options.image_repository,
        "repositoryProbeStatus": tls_probe["repositoryProbeStatus"],
        "tlsPeerCertificateSha256": tls_probe["tlsPeerCertificateSha256"],
        "runtimeConfigPath": options.production_registry_runtime_config_path,
        "runtimeConfigSha256": runtime_config_sha256,
        "exportedConfigSha256": exported_config_sha256,
        "retentionPolicySha256": live_policy_sha256,
        "checkedInRetentionPolicySha256": checked_in_policy_sha256,
        "container": {
            "name": container_name.lstrip("/"),
            "id": container_id,
            "image": runtime_image,
            "startedAt": started_at,
        },
    }
    evidence["runtimeEvidenceSha256"] = _stable_json_sha256(evidence)
    return evidence


def _validate_live_registry_boundary_evidence(
    evidence: Mapping[str, Any],
    *,
    registry_host: str,
    image_repository: str,
    runtime_config_path: str,
    exported_config_sha256: str,
    live_policy_sha256: str,
    checked_in_policy_sha256: str,
) -> dict[str, Any]:
    if not isinstance(evidence, Mapping):
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "Worker Registry live runtime evidence was invalid.",
        )
    required_fields = {
        "collectedAt",
        "registryHost",
        "registryAuthority",
        "repositoryAuthority",
        "repositoryProbeStatus",
        "tlsPeerCertificateSha256",
        "runtimeConfigPath",
        "runtimeConfigSha256",
        "exportedConfigSha256",
        "retentionPolicySha256",
        "checkedInRetentionPolicySha256",
        "runtimeEvidenceSha256",
        "container",
    }
    if set(evidence) != required_fields:
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "Worker Registry live runtime evidence was invalid.",
            {
                "missingFields": sorted(required_fields - set(evidence)),
                "unexpectedFields": sorted(set(evidence) - required_fields),
            },
        )
    collected_at = _parse_runtime_boundary_timestamp(
        evidence.get("collectedAt"),
        field="liveRuntimeEvidence.collectedAt",
    )
    age_seconds = (dt.datetime.now(dt.timezone.utc) - collected_at).total_seconds()
    if age_seconds > PRODUCTION_REGISTRY_RUNTIME_EVIDENCE_MAX_AGE_SECONDS:
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "Worker Registry live runtime evidence was stale.",
            {
                "ageSeconds": round(age_seconds, 3),
                "maximumAgeSeconds": PRODUCTION_REGISTRY_RUNTIME_EVIDENCE_MAX_AGE_SECONDS,
            },
        )
    container = evidence.get("container")
    if not isinstance(container, dict) or set(container) != {"name", "id", "image", "startedAt"}:
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "Worker Registry live runtime evidence was invalid.",
            {"field": "liveRuntimeEvidence.container"},
        )
    runtime_image = container.get("image")
    expected_runtime_image = _production_registry_runtime_image_contract(
        PRODUCTION_REGISTRY_RUNTIME_IMAGE_REFERENCE
    )
    if not isinstance(runtime_image, dict) or set(runtime_image) != {
        "expectedReference",
        "expectedDigest",
        "configReference",
        "runtimeId",
        "matchedRepoDigest",
    }:
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "Worker Registry live runtime image evidence was invalid.",
            {"field": "liveRuntimeEvidence.container.image"},
        )
    if (
        evidence.get("registryHost") != registry_host
        or evidence.get("repositoryAuthority") != image_repository
        or evidence.get("runtimeConfigPath") != runtime_config_path
        or evidence.get("runtimeConfigSha256") != exported_config_sha256
        or evidence.get("exportedConfigSha256") != exported_config_sha256
        or evidence.get("retentionPolicySha256") != live_policy_sha256
        or evidence.get("checkedInRetentionPolicySha256") != checked_in_policy_sha256
        or not isinstance(evidence.get("repositoryProbeStatus"), int)
        or evidence["repositoryProbeStatus"] not in {200, 404}
    ):
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "Worker Registry live runtime evidence did not match the active production registry boundary.",
        )
    registry_authority = evidence.get("registryAuthority")
    parsed_authority = urllib.parse.urlparse(registry_authority) if isinstance(registry_authority, str) else None
    if (
        not isinstance(registry_authority, str)
        or parsed_authority is None
        or parsed_authority.scheme != "https"
        or parsed_authority.netloc != registry_host
        or parsed_authority.path not in {"", "/"}
        or parsed_authority.params
        or parsed_authority.query
        or parsed_authority.fragment
    ):
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "Worker Registry live runtime evidence did not match the active production registry boundary.",
            {"field": "liveRuntimeEvidence.registryAuthority"},
        )
    tls_peer_certificate_sha256 = evidence.get("tlsPeerCertificateSha256")
    if (
        not isinstance(tls_peer_certificate_sha256, str)
        or SHA256_HEX_PATTERN.fullmatch(tls_peer_certificate_sha256) is None
    ):
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "Worker Registry live runtime evidence was invalid.",
            {"field": "liveRuntimeEvidence.tlsPeerCertificateSha256"},
        )
    if (
        not isinstance(container.get("name"), str)
        or not container["name"].strip()
        or not isinstance(container.get("id"), str)
        or SHA256_HEX_PATTERN.fullmatch(container["id"][:64]) is None
        or runtime_image.get("expectedReference") != expected_runtime_image["reference"]
        or runtime_image.get("expectedDigest") != expected_runtime_image["digest"]
        or runtime_image.get("configReference") != expected_runtime_image["reference"]
        or not isinstance(runtime_image.get("runtimeId"), str)
        or SHA256_DIGEST_PATTERN.fullmatch(runtime_image["runtimeId"]) is None
        or runtime_image.get("matchedRepoDigest") != expected_runtime_image["repoDigest"]
    ):
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "Worker Registry live runtime evidence was invalid.",
            {"field": "liveRuntimeEvidence.container"},
        )
    _parse_runtime_boundary_timestamp(
        container.get("startedAt"),
        field="liveRuntimeEvidence.container.startedAt",
    )
    normalized = {key: value for key, value in evidence.items() if key != "runtimeEvidenceSha256"}
    if evidence.get("runtimeEvidenceSha256") != _stable_json_sha256(normalized):
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "Worker Registry live runtime evidence was invalid.",
            {"field": "liveRuntimeEvidence.runtimeEvidenceSha256"},
        )
    return dict(evidence)


def validate_production_registry_boundary(
    options: RegistryReleaseGateOptions,
    *,
    state_dir: pathlib.Path | None = None,
    redactor: acceptance.SecretRedactor | None = None,
    runtime_evidence: Mapping[str, Any] | None = None,
) -> dict[str, Any]:
    if options.signing_policy_profile != "production":
        return {}
    if (
        options.production_registry_config_path is None
        or options.production_registry_retention_policy_path is None
        or options.production_registry_container is None
        or options.production_registry_runtime_config_path is None
    ):
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "Production Worker Registry signing omitted the live Registry runtime evidence inputs.",
        )
    checked_in_policy = _normalize_retention_policy(
        _read_json_path(
            options.repo_root / PRODUCTION_REGISTRY_RETENTION_POLICY_PATH,
            code="release.registry_source_metadata_invalid",
            message="The checked-in Worker Registry retention policy was unavailable or invalid.",
        ),
        code="release.registry_source_metadata_invalid",
        runtime_config_path=options.production_registry_runtime_config_path,
    )
    live_policy = _normalize_retention_policy(
        _read_json_path(
            options.production_registry_retention_policy_path,
            code="release.registry_production_boundary_invalid",
            message="The live Worker Registry retention policy export was unavailable or invalid.",
        ),
        code="release.registry_production_boundary_invalid",
        runtime_config_path=options.production_registry_runtime_config_path,
    )
    checked_in_policy_sha256 = _stable_json_sha256(checked_in_policy)
    live_policy_sha256 = _stable_json_sha256(live_policy)
    if live_policy != checked_in_policy:
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "The live Worker Registry retention policy did not match the checked-in production boundary.",
            {
                "checkedInPolicySha256": checked_in_policy_sha256,
                "livePolicySha256": live_policy_sha256,
            },
        )
    try:
        registry_config_text = options.production_registry_config_path.read_text(encoding="utf-8")
    except OSError:
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "The live Worker Registry configuration export was unavailable.",
            {"path": str(options.production_registry_config_path)},
        ) from None
    normalized_registry_config_text = _normalize_text_content(registry_config_text)
    exported_config_sha256 = hashlib.sha256(normalized_registry_config_text.encode("utf-8")).hexdigest()
    live_registry_host = options.image_repository.split("/", 1)[0]
    runtime_config_details = _inspect_runtime_registry_config(
        normalized_registry_config_text,
        registry_host=live_registry_host,
        code="release.registry_production_boundary_invalid",
        invalid_message="The live Worker Registry configuration export was not valid strict YAML.",
        authority_message="The live Worker Registry configuration did not expose the exact TLS authority boundary.",
    )
    if runtime_config_details["deleteEnabled"] is not False:
        raise ReleaseGateError(
            "release.registry_production_boundary_invalid",
            "The live Worker Registry configuration did not keep delete disabled.",
            {"path": str(options.production_registry_config_path)},
        )
    if runtime_evidence is None:
        if state_dir is None or redactor is None:
            raise ReleaseGateError(
                "release.registry_production_boundary_invalid",
                "Production Worker Registry signing omitted the live Registry runtime evidence inputs.",
            )
        prepared_registry_access = _prepare_host_registry_access(
            options,
            state_dir=state_dir,
            redactor=redactor,
        )
        runtime_evidence = _collect_live_registry_boundary_evidence(
            options,
            state_dir=state_dir,
            prepared_registry_access=prepared_registry_access,
            exported_config_sha256=exported_config_sha256,
            live_policy_sha256=live_policy_sha256,
            checked_in_policy_sha256=checked_in_policy_sha256,
            expected_runtime_image_reference=checked_in_policy["runtimeImage"],
        )
    live_runtime_evidence = _validate_live_registry_boundary_evidence(
        runtime_evidence,
        registry_host=live_registry_host,
        image_repository=options.image_repository,
        runtime_config_path=options.production_registry_runtime_config_path,
        exported_config_sha256=exported_config_sha256,
        live_policy_sha256=live_policy_sha256,
        checked_in_policy_sha256=checked_in_policy_sha256,
    )
    return {
        "registryConfigPath": str(options.production_registry_config_path),
        "retentionPolicyPath": str(options.production_registry_retention_policy_path),
        "deleteEnabled": False,
        "promotionBoundary": live_policy["immutability"]["promotionBoundary"],
        "releaseEvidenceDays": live_policy["retention"]["releaseEvidenceDays"],
        "garbageCollectionMode": live_policy["garbageCollection"]["mode"],
        "archiveRequiredBeforeGc": live_policy["garbageCollection"]["requiresReleaseEvidenceArchive"],
        "liveRuntimeEvidence": live_runtime_evidence,
    }


def _expected_production_signer_identity() -> dict[str, Any]:
    role_name = supply_chain.VAULT_TRANSIT_PRINCIPAL.removeprefix("auth/approle/role/")
    policies = [role_name]
    return {
        "verified": True,
        "displayName": supply_chain.VAULT_TRANSIT_AUTH_METHOD,
        "roleName": role_name,
        "type": "batch",
        "orphan": True,
        "policyCount": 1,
        "policiesSha256": hashlib.sha256(
            json.dumps(policies, separators=(",", ":"), sort_keys=True).encode("utf-8")
        ).hexdigest(),
    }


def _validate_production_signer_identity(
    options: RegistryReleaseGateOptions,
    supply_chain_report: Mapping[str, Any],
) -> dict[str, Any]:
    if options.signing_policy_profile != "production":
        return {}
    signing = supply_chain_report.get("signing")
    identity = signing.get("signerIdentity") if isinstance(signing, Mapping) else None
    expected = _expected_production_signer_identity()
    if not isinstance(identity, dict) or set(identity) != set(expected) or identity != expected:
        raise ReleaseGateError(
            "release.registry_supply_chain_signer_identity_invalid",
            "Worker Registry supply-chain evidence did not prove the exact production Vault AppRole signer identity.",
            {
                "expectedIdentitySha256": _stable_json_sha256(expected),
                "actualIdentitySha256": (
                    _stable_json_sha256(identity) if isinstance(identity, dict) else None
                ),
            },
        )
    return dict(expected)


def _validate_production_boundary(options: RegistryReleaseGateOptions) -> None:
    if options.signing_policy_profile == "production" and options.insecure_registry:
        raise ReleaseGateError(
            "release.registry_production_signing_insecure_registry",
            "Production Worker Registry signing requires a TLS registry before any push begins.",
        )
    if options.signing_policy_profile == "production":
        actual = (
            options.registry_auth_username_environment,
            options.registry_auth_password_environment,
            options.registry_ca_cert_environment,
        )
        if actual != supply_chain.PRODUCTION_REGISTRY_ACCESS_ENVIRONMENT:
            raise ReleaseGateError(
                "release.registry_production_boundary_invalid",
                "Production Worker Registry signing drifted from the checked-in Registry access environment names.",
                {
                    "expected": list(supply_chain.PRODUCTION_REGISTRY_ACCESS_ENVIRONMENT),
                    "actual": list(actual),
                },
            )


def run_registry_release_gate(
    options: RegistryReleaseGateOptions,
    *,
    repository_state: Any = common.repository_state,
    runtime_inspector: Any = inspect_runtime,
    build_runner: Any = build_and_inspect,
    supply_chain_runner: Any = supply_chain.verify_supply_chain,
) -> int:
    if options.output_dir.exists() and (
        not options.output_dir.is_dir() or any(options.output_dir.iterdir())
    ):
        print("Worker Registry release gate output directory must be empty or absent.", file=sys.stderr)
        return 2
    options.output_dir.mkdir(parents=True, exist_ok=True)
    state_dir = options.output_dir / "_state"
    state_dir.mkdir()
    started_at = acceptance.utc_now()
    started = time.monotonic()
    run_id = f"stage3-worker-registry-release-{uuid.uuid4()}"
    redactor = acceptance.SecretRedactor()
    try:
        _validate_production_boundary(options)
        options = dataclasses.replace(
            options,
            tool_environment_overrides=_prepare_host_registry_environment(
                options,
                state_dir=state_dir,
                redactor=redactor,
            ),
        )
    except ReleaseGateError as error:
        errors = [error.as_report_error()]
    else:
        errors: list[dict[str, Any]] = []
    source: dict[str, Any] = {}
    runtime: dict[str, Any] = {}
    builds: list[dict[str, Any]] = []
    supply_chain_report: dict[str, Any] = {}
    version: str | None = None
    source_date_epoch: str | None = None
    supply_chain_configuration: supply_chain.SupplyChainConfiguration | None = None
    try:
        source = dict(repository_state(options.repo_root))
        runtime = dict(runtime_inspector(options))
        version, source_date_epoch = source_metadata(options.repo_root, str(source["gitSha"]))
        supply_chain_configuration = supply_chain.load_configuration(
            options.repo_root,
            signing_policy_profile=options.signing_policy_profile,
        )
        source.update(
            {
                "version": version,
                "sourceDateEpoch": source_date_epoch,
                "buildKitSBOMGenerator": locked_sbom_generator(options.repo_root),
                "sourceHashes": supply_chain.checked_in_source_hashes(
                    options.repo_root,
                    supply_chain.PRODUCTION_RELEASE_SOURCE_PATHS,
                ),
                "supplyChain": supply_chain_configuration.source_evidence(),
            }
        )
        production_registry_boundary = validate_production_registry_boundary(
            options,
            state_dir=state_dir,
            redactor=redactor,
        )
        if production_registry_boundary:
            runtime["productionRegistryBoundary"] = production_registry_boundary
    except (OSError, subprocess.SubprocessError, ReleaseGateError) as raw_error:
        error = (
            raw_error
            if isinstance(raw_error, ReleaseGateError)
            else ReleaseGateError(
                "release.registry_preflight_failed",
                "Worker Registry release gate preflight failed.",
            )
        )
        errors.append(error.as_report_error())

    if not errors and version is not None and source_date_epoch is not None:
        suffix = uuid.uuid4().hex[:12]
        for slot, no_cache in (("cached", False), ("no-cache", True)):
            image = f"{options.image_repository}:stage3-{str(source['gitSha'])[:12]}-{suffix}-{slot}"
            try:
                builds.append(
                    build_runner(
                        options,
                        slot=slot,
                        image=image,
                        git_sha=str(source["gitSha"]),
                        version=version,
                        source_date_epoch=source_date_epoch,
                        state_dir=state_dir,
                        no_cache=no_cache,
                    )
                )
            except (OSError, subprocess.SubprocessError, ReleaseGateError) as raw_error:
                error = (
                    raw_error
                    if isinstance(raw_error, ReleaseGateError)
                    else ReleaseGateError(
                        "release.registry_execution_failed",
                        "Worker Registry release gate execution failed.",
                        {"slot": slot},
                    )
                )
                errors.append(error.as_report_error())
                break
        errors.extend(reproducibility_errors(builds))
        if not errors and supply_chain_configuration is not None:
            try:
                raw_supply_chain_report = supply_chain_runner(
                    supply_chain.SupplyChainOptions(
                        repo_root=options.repo_root,
                        state_dir=state_dir / "supply-chain",
                        image_repository=options.image_repository,
                        docker_bin=options.docker_bin,
                        timeout_seconds=options.supply_chain_timeout_seconds,
                        insecure_registry=options.insecure_registry,
                        registry_auth_username_environment=options.registry_auth_username_environment,
                        registry_auth_password_environment=options.registry_auth_password_environment,
                        registry_ca_cert_environment=options.registry_ca_cert_environment,
                        production_public_key_configmap_path=options.production_public_key_configmap_path,
                        production_repository_configmap_path=options.production_repository_configmap_path,
                    ),
                    supply_chain_configuration,
                    builds=builds,
                    git_sha=str(source["gitSha"]),
                    version=version,
                    run_id=run_id,
                    redactor=redactor,
                )
                if not isinstance(raw_supply_chain_report, dict):
                    raise ReleaseGateError(
                        "release.registry_supply_chain_report_invalid",
                        "Worker Registry supply-chain verifier returned an invalid report.",
                    )
                supply_chain_report = dict(raw_supply_chain_report)
                supply_chain_errors = supply_chain_report.get("errors")
                if not isinstance(supply_chain_errors, list) or not all(
                    isinstance(error, dict) for error in supply_chain_errors
                ):
                    raise ReleaseGateError(
                        "release.registry_supply_chain_report_invalid",
                        "Worker Registry supply-chain verifier returned invalid errors.",
                    )
                errors.extend(supply_chain_errors)
                if supply_chain_report.get("status") != "pass" and not supply_chain_errors:
                    errors.append(
                        {
                            "code": "release.registry_supply_chain_status_invalid",
                            "message": "Worker Registry supply-chain verifier did not pass.",
                        }
                    )
                if supply_chain_report.get("status") == "pass" and not supply_chain_errors:
                    _validate_production_signer_identity(options, supply_chain_report)
            except (OSError, subprocess.SubprocessError, ReleaseGateError) as raw_error:
                error = (
                    raw_error
                    if isinstance(raw_error, ReleaseGateError)
                    else ReleaseGateError(
                        "release.registry_supply_chain_failed",
                        "Worker Registry supply-chain verification failed.",
                    )
                )
                errors.append(error.as_report_error())

    shutil.rmtree(state_dir, ignore_errors=True)
    state_removed = not state_dir.exists()
    supply_chain_cleanup = supply_chain_report.get("cleanup")
    if isinstance(supply_chain_cleanup, dict):
        supply_chain_cleanup["isolatedStateRemoved"] = state_removed
    if not state_removed:
        errors.append(
            {
                "code": "release.registry_state_cleanup_failed",
                "message": "Worker Registry release gate isolated local state was not removed.",
            }
        )
    status = (
        "pass"
        if not errors
        and len(builds) == 2
        and supply_chain_report.get("status") == "pass"
        else "fail"
    )
    report = {
        "schemaVersion": SCHEMA_VERSION,
        "runId": run_id,
        "mode": "worker-registry-release-gate",
        "status": status,
        "source": source,
        "runtime": runtime,
        "configuration": configuration_evidence(options),
        "startedAt": started_at,
        "finishedAt": acceptance.utc_now(),
        "durationMs": acceptance.elapsed_ms(started),
        "cleanup": {
            "isolatedStateRemoved": state_removed,
            "remoteImagesRetainedAsReleaseEvidence": True,
            "broadCleanupUsed": False,
        },
        "builds": builds,
        "supplyChain": supply_chain_report,
        "errors": errors,
    }
    json_path, markdown_path = write_report(report, options.output_dir, redactor)
    output_scan = acceptance.scan_output_secrets(options.output_dir, redactor)
    if output_scan.get("findings"):
        report["status"] = "fail"
        report["errors"].append(
            {
                "code": "release.registry_output_secret_scan_failed",
                "message": "Worker Registry release-gate report contained Secret-like material.",
                "evidence": {"findingCount": len(output_scan["findings"])},
            }
        )
        status = "fail"
    report["security"] = {"outputSecretScan": output_scan}
    json_path, markdown_path = write_report(report, options.output_dir, redactor)
    print(f"Stage 3 Worker Registry release gate: {status}")
    print(f"JSON: {json_path}")
    print(f"Markdown: {markdown_path}")
    return 0 if status == "pass" else 1


def main(argv: Sequence[str] | None = None) -> int:
    options = parse_args(argv if argv is not None else sys.argv[1:])
    return run_registry_release_gate(options)


if __name__ == "__main__":
    raise SystemExit(main())
