#!/usr/bin/env python3
"""Build and verify reproducible registry-pushed multi-arch Worker images."""

from __future__ import annotations

import argparse
import dataclasses
import datetime as dt
import hashlib
import json
import pathlib
import re
import shutil
import subprocess
import sys
import time
import uuid
from collections.abc import Mapping, Sequence
from typing import Any

import acceptance_runner as acceptance
import controlled_remote_release_gate as remote
import release_gate_common as common


SCHEMA_VERSION = "synara.worker-registry-release-gate.v1"
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

ReleaseGateError = common.ReleaseGateError


@dataclasses.dataclass(frozen=True)
class RegistryReleaseGateOptions:
    repo_root: pathlib.Path
    output_dir: pathlib.Path
    image_repository: str
    builder: str
    build_timeout_seconds: float
    docker_bin: str
    go_proxy: str | None


def parse_args(argv: Sequence[str]) -> RegistryReleaseGateOptions:
    repo_root = pathlib.Path(__file__).resolve().parents[2]
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image-repository", required=True)
    parser.add_argument("--builder", required=True)
    parser.add_argument("--build-timeout", type=float, default=7200.0)
    parser.add_argument("--docker-bin", default="docker")
    parser.add_argument("--go-proxy")
    parser.add_argument("--output-dir", type=pathlib.Path)
    parsed = parser.parse_args(argv)
    if parsed.build_timeout <= 0:
        parser.error("--build-timeout must be positive")
    try:
        image_repository = normalize_image_repository(parsed.image_repository)
        builder = normalize_builder_name(parsed.builder)
        docker_bin = normalize_executable(parsed.docker_bin, "--docker-bin")
        go_proxy = normalize_go_proxy(parsed.go_proxy)
    except ValueError as error:
        parser.error(str(error))
    output_dir = parsed.output_dir or remote.default_output_dir(repo_root, "registry")
    return RegistryReleaseGateOptions(
        repo_root=repo_root,
        output_dir=output_dir.expanduser().resolve(),
        image_repository=image_repository,
        builder=builder,
        build_timeout_seconds=parsed.build_timeout,
        docker_bin=docker_bin,
        go_proxy=go_proxy,
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


def normalize_go_proxy(value: str | None) -> str | None:
    if value is None:
        return None
    proxy = value.strip()
    if (
        not proxy
        or len(proxy) > 2048
        or any(character.isspace() or ord(character) < 32 for character in proxy)
        or any(character in proxy for character in "@?#")
    ):
        raise ValueError(
            "--go-proxy must be a public credential-free GOPROXY list without whitespace, userinfo, query, or fragment data"
        )
    for entry in proxy.split(","):
        if entry in {"direct", "off"}:
            continue
        if re.fullmatch(r"https://[A-Za-z0-9.-]+(?::[0-9]+)?(?:/[A-Za-z0-9._~!$&'()*+,;=:@%/-]*)?", entry) is None:
            raise ValueError("--go-proxy entries must use https://, direct, or off")
    return proxy


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
            env=remote.tool_environment(),
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
            env=remote.tool_environment(),
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
) -> tuple[dict[str, Any], bytes]:
    try:
        completed = subprocess.run(
            [options.docker_bin, *arguments],
            cwd=options.repo_root,
            env=remote.tool_environment(),
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
    if not isinstance(payload, dict):
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
    if wrapper != expected_wrapper:
        raise ReleaseGateError(
            "release.registry_embedded_runtime_invalid",
            "Worker Registry image Provider Host wrapper was not the canonical executable.",
            {"platform": platform},
        )
    return {
        "manifestSha256": hashlib.sha256(manifest_bytes).hexdigest(),
        "sbomSha256": sbom_digest,
        "lockfileSha256": lock_hashes,
        "providerHostSha256": common.file_sha256(files["providerHost"]),
        "agentdSha256": common.file_sha256(files["agentd"]),
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
    lines.extend(
        [
            "",
            "## Evidence boundary",
            "",
            "A pass closes clean-SHA registry push, required multi-arch shape, reproducible platform content,",
            "embedded supply-chain inputs, and BuildKit SBOM/provenance attachment. It does not prove image",
            "signature policy, production Registry retention, real Provider four-Target rollout, or soak.",
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
    return {
        "imageRepository": options.image_repository,
        "builder": options.builder,
        "platforms": list(REQUIRED_PLATFORMS),
        "target": "worker",
        "cachedBuild": True,
        "independentNoCacheBuild": True,
        "buildKitSBOM": True,
        "buildKitProvenance": "mode=max",
        "sourceDateEpochLayerRewrite": True,
        "goProxyOverride": options.go_proxy is not None,
        "remoteImagesRetainedAsReleaseEvidence": True,
        "remoteBroadCleanupUsed": False,
    }


def run_registry_release_gate(
    options: RegistryReleaseGateOptions,
    *,
    repository_state: Any = common.repository_state,
    runtime_inspector: Any = inspect_runtime,
    build_runner: Any = build_and_inspect,
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
    source: dict[str, Any] = {}
    runtime: dict[str, Any] = {}
    builds: list[dict[str, Any]] = []
    errors: list[dict[str, Any]] = []
    version: str | None = None
    source_date_epoch: str | None = None
    try:
        source = dict(repository_state(options.repo_root))
        runtime = dict(runtime_inspector(options))
        version, source_date_epoch = source_metadata(options.repo_root, str(source["gitSha"]))
        source.update(
            {
                "version": version,
                "sourceDateEpoch": source_date_epoch,
                "buildKitSBOMGenerator": locked_sbom_generator(options.repo_root),
            }
        )
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

    shutil.rmtree(state_dir, ignore_errors=True)
    state_removed = not state_dir.exists()
    if not state_removed:
        errors.append(
            {
                "code": "release.registry_state_cleanup_failed",
                "message": "Worker Registry release gate isolated local state was not removed.",
            }
        )
    status = "pass" if not errors and len(builds) == 2 else "fail"
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
