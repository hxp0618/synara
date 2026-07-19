#!/usr/bin/env python3
"""Run the clean-SHA real Codex/Claude SSH product and failure release gate."""

from __future__ import annotations

import argparse
import dataclasses
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


SCHEMA_VERSION = "synara.provider-ssh-release-gate.v1"
JSON_REPORT_NAME = "ssh-release-gate.json"
MARKDOWN_REPORT_NAME = "ssh-release-gate.md"
EXPECTED_UNSUPPORTED: Mapping[tuple[str, str], frozenset[str]] = {
    ("codex", "product"): frozenset({"real-provider.terminal-large-log"}),
    ("claudeAgent", "product"): frozenset({"real-provider.compact-boundary"}),
    ("codex", "failure"): frozenset(),
    ("claudeAgent", "failure"): frozenset(),
}
SSH_REAL_PROVIDER_WORKER_LEASE_TTL = "60s"
SSH_REAL_PROVIDER_WORKER_HEARTBEAT_TIMEOUT = "180s"

CredentialSource = remote.CredentialSource
ReleaseGateError = remote.ReleaseGateError


@dataclasses.dataclass(frozen=True)
class SSHReleaseGateOptions:
    repo_root: pathlib.Path
    output_dir: pathlib.Path
    product_timeout_seconds: float
    failure_timeout_seconds: float
    codex_credential: CredentialSource
    claude_credential: CredentialSource
    ssh_orbctl_bin: str
    ssh_machine_arch: str
    ssh_machine_image: str
    ssh_node_version: str
    ssh_external_host: str | None
    ssh_external_port: int
    ssh_external_user: str | None
    ssh_external_identity_file: pathlib.Path | None
    ssh_external_host_key_file: pathlib.Path | None
    ssh_external_service_user: str
    ssh_external_use_sudo: bool
    ssh_allow_external_host: bool
    codex_model: str | None = None
    claude_model: str | None = None


def parse_args(argv: Sequence[str]) -> SSHReleaseGateOptions:
    repo_root = pathlib.Path(__file__).resolve().parents[2]
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--codex-credential-env", required=True)
    parser.add_argument("--codex-base-url-env")
    remote.add_provider_model_arguments(parser, "codex")
    parser.add_argument("--claude-credential-env", required=True)
    parser.add_argument(
        "--claude-credential-field",
        choices=acceptance.REAL_PROVIDER_CREDENTIAL_FIELDS,
        default="apiKey",
    )
    parser.add_argument("--claude-base-url-env")
    remote.add_provider_model_arguments(parser, "claude")
    parser.add_argument("--output-dir", type=pathlib.Path)
    parser.add_argument("--product-timeout", type=float, default=3600.0)
    parser.add_argument("--failure-timeout", type=float, default=2400.0)
    parser.add_argument("--ssh-orbctl-bin", default="orbctl")
    parser.add_argument("--ssh-machine-arch", choices=("arm64", "amd64"), default="arm64")
    parser.add_argument("--ssh-machine-image", default="ubuntu:24.04")
    parser.add_argument("--ssh-node-version", default="24.13.1")
    parser.add_argument("--ssh-external-host")
    parser.add_argument("--ssh-external-port", type=int, default=22)
    parser.add_argument("--ssh-external-user")
    parser.add_argument("--ssh-external-identity-file", type=pathlib.Path)
    parser.add_argument("--ssh-external-host-key-file", type=pathlib.Path)
    parser.add_argument("--ssh-external-service-user", default=acceptance.SSH_SERVICE_USER)
    parser.add_argument("--ssh-external-use-sudo", action="store_true")
    parser.add_argument("--ssh-allow-external-host", action="store_true")
    parsed = parser.parse_args(argv)
    if parsed.product_timeout <= 0 or parsed.failure_timeout <= 0:
        parser.error("matrix timeouts must be positive")
    orbctl_bin = parsed.ssh_orbctl_bin.strip()
    machine_image = parsed.ssh_machine_image.strip()
    node_version = parsed.ssh_node_version.strip()
    if not orbctl_bin or any(character in orbctl_bin for character in "\r\n\t\x00"):
        parser.error("--ssh-orbctl-bin must be a command or executable path")
    if not machine_image or len(machine_image) > 128 or any(
        character in machine_image for character in "\r\n\t\x00"
    ):
        parser.error("--ssh-machine-image must be a non-empty OrbStack distro reference")
    if re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", node_version) is None:
        parser.error("--ssh-node-version must be a three-component numeric version")
    external_host = parsed.ssh_external_host.strip() if parsed.ssh_external_host else None
    external_user = parsed.ssh_external_user.strip() if parsed.ssh_external_user else None
    external_service_user = parsed.ssh_external_service_user.strip()
    external_identity_file: pathlib.Path | None = None
    external_host_key_file: pathlib.Path | None = None
    external_option_used = bool(
        external_host
        or external_user
        or parsed.ssh_external_identity_file
        or parsed.ssh_external_host_key_file
        or parsed.ssh_allow_external_host
        or parsed.ssh_external_use_sudo
        or parsed.ssh_external_port != 22
        or external_service_user != acceptance.SSH_SERVICE_USER
    )
    if external_host is not None:
        if not parsed.ssh_allow_external_host:
            parser.error("--ssh-external-host requires --ssh-allow-external-host")
        if external_user is None or parsed.ssh_external_identity_file is None or parsed.ssh_external_host_key_file is None:
            parser.error(
                "--ssh-external-host requires --ssh-external-user, --ssh-external-identity-file, "
                "and --ssh-external-host-key-file"
            )
        if len(external_host) > 253 or any(
            character in external_host for character in "\r\n\t\x00 /@"
        ):
            parser.error("--ssh-external-host must be one hostname or address")
        if parsed.ssh_external_port < 1 or parsed.ssh_external_port > 65535:
            parser.error("--ssh-external-port must be between 1 and 65535")
        if not re.fullmatch(r"[a-z_][a-z0-9_-]*[$]?", external_user):
            parser.error("--ssh-external-user must be a valid Unix user")
        if not re.fullmatch(r"[a-z_][a-z0-9_-]*[$]?", external_service_user):
            parser.error("--ssh-external-service-user must be a valid Unix user")
        if external_user != "root" and not parsed.ssh_external_use_sudo:
            parser.error("a non-root --ssh-external-user requires --ssh-external-use-sudo")
        try:
            external_identity_file = acceptance.resolve_repository_external_file(
                parsed.ssh_external_identity_file,
                repo_root,
                "--ssh-external-identity-file",
                private=True,
            )
            external_host_key_file = acceptance.resolve_repository_external_file(
                parsed.ssh_external_host_key_file,
                repo_root,
                "--ssh-external-host-key-file",
                private=False,
            )
        except ValueError as error:
            parser.error(str(error))
    elif external_option_used:
        parser.error("SSH external-host options require --ssh-external-host")
    try:
        codex_credential = remote.parse_credential_source(
            parsed.codex_credential_env,
            "apiKey",
            parsed.codex_base_url_env,
            "Codex",
        )
        claude_credential = remote.parse_credential_source(
            parsed.claude_credential_env,
            parsed.claude_credential_field,
            parsed.claude_base_url_env,
            "Claude",
        )
        codex_model = remote.parse_provider_model_argument(
            parsed.codex_model,
            parsed.codex_model_env,
            provider_label="Codex",
            model_option="--codex-model",
            model_env_option="--codex-model-env",
        )
        claude_model = remote.parse_provider_model_argument(
            parsed.claude_model,
            parsed.claude_model_env,
            provider_label="Claude",
            model_option="--claude-model",
            model_env_option="--claude-model-env",
        )
    except ValueError as error:
        parser.error(str(error))
    output_dir = parsed.output_dir or remote.default_output_dir(repo_root, "ssh")
    return SSHReleaseGateOptions(
        repo_root=repo_root,
        output_dir=output_dir.expanduser().resolve(),
        product_timeout_seconds=parsed.product_timeout,
        failure_timeout_seconds=parsed.failure_timeout,
        codex_credential=codex_credential,
        claude_credential=claude_credential,
        ssh_orbctl_bin=orbctl_bin,
        ssh_machine_arch=parsed.ssh_machine_arch,
        ssh_machine_image=machine_image,
        ssh_node_version=node_version,
        ssh_external_host=external_host,
        ssh_external_port=parsed.ssh_external_port,
        ssh_external_user=external_user,
        ssh_external_identity_file=external_identity_file,
        ssh_external_host_key_file=external_host_key_file,
        ssh_external_service_user=external_service_user,
        ssh_external_use_sudo=parsed.ssh_external_use_sudo,
        ssh_allow_external_host=parsed.ssh_allow_external_host,
        codex_model=codex_model,
        claude_model=claude_model,
    )


def credential_source(options: SSHReleaseGateOptions, provider: str) -> CredentialSource:
    return remote.credential_source(options, provider)


provider_model = remote.provider_model


def uses_external_host(options: SSHReleaseGateOptions) -> bool:
    return options.ssh_external_host is not None


def expected_provider_tool_versions(repo_root: pathlib.Path) -> dict[str, str]:
    provider_tools_root = repo_root / "deploy" / "worker" / "provider-tools"
    package_path = provider_tools_root / "package.json"
    lock_path = provider_tools_root / "package-lock.json"
    try:
        package_payload = json.loads(package_path.read_text(encoding="utf-8"))
        lock_payload = json.loads(lock_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        raise ReleaseGateError(
            "release.ssh_provider_tools_lock_invalid",
            "SSH release gate could not read the Provider tools package and lock metadata.",
        ) from None

    dependencies = (
        package_payload.get("dependencies") if isinstance(package_payload, dict) else None
    )
    codex = dependencies.get("@openai/codex") if isinstance(dependencies, dict) else None
    claude = (
        dependencies.get("@anthropic-ai/claude-code")
        if isinstance(dependencies, dict)
        else None
    )
    if not isinstance(codex, str) or not isinstance(claude, str):
        raise ReleaseGateError(
            "release.ssh_provider_tools_lock_invalid",
            "SSH release gate Provider tools metadata omitted locked Codex or Claude versions.",
        )

    lock_packages = lock_payload.get("packages") if isinstance(lock_payload, dict) else None
    lock_root = lock_packages.get("") if isinstance(lock_packages, dict) else None
    lock_dependencies = (
        lock_root.get("dependencies") if isinstance(lock_root, dict) else None
    )
    locked_versions = {
        "@openai/codex": codex,
        "@anthropic-ai/claude-code": claude,
    }
    if not isinstance(lock_dependencies, dict) or any(
        lock_dependencies.get(package_name) != version
        or not isinstance((entry := lock_packages.get(f"node_modules/{package_name}")), dict)
        or entry.get("version") != version
        for package_name, version in locked_versions.items()
    ):
        raise ReleaseGateError(
            "release.ssh_provider_tools_lock_invalid",
            "SSH release gate Provider tools package and lock versions did not match.",
        )
    return {"codex": codex, "claudeAgent": claude}


def child_policy(options: SSHReleaseGateOptions) -> common.ChildReportPolicy:
    if uses_external_host(options):
        cleanup_true_fields = (
            "externalHostPreserved",
            "ownedRuntimeRemoved",
            "operatorIdentitySourcePreserved",
            "productRevokeRequested",
            "machineLifecycleCompleted",
            "localKeyMaterialRemoved",
            "stateRemoved",
        )
        cleanup_false_fields = (
            "machineRemoved",
            "machinePreservedByRequest",
            "externalHostRestarted",
            "broadCleanupUsed",
        )
    else:
        cleanup_true_fields = (
            "machineRemoved",
            "productRevokeRequested",
            "machineLifecycleCompleted",
            "localKeyMaterialRemoved",
            "stateRemoved",
        )
        cleanup_false_fields = ("machinePreservedByRequest", "broadCleanupUsed")
    return common.ChildReportPolicy(
        target="ssh",
        runner_executable="provider-host",
        expected_unsupported=EXPECTED_UNSUPPORTED,
        authentication="controlled",
        credential_fields={
            provider: credential_source(options, provider).field for provider in common.PROVIDERS
        },
        controlled_base_urls={
            provider: credential_source(options, provider).base_url_environment_name is not None
            for provider in common.PROVIDERS
        },
        provider_models={
            provider: remote.provider_model(options, provider) for provider in common.PROVIDERS
        },
        cleanup_true_fields=cleanup_true_fields,
        cleanup_false_fields=cleanup_false_fields,
    )


def child_command(
    options: SSHReleaseGateOptions,
    provider: str,
    matrix: str,
    output_dir: pathlib.Path,
) -> list[str]:
    source = credential_source(options, provider)
    timeout = (
        options.product_timeout_seconds
        if matrix == "product"
        else options.failure_timeout_seconds
    )
    matrix_flag = "--real-provider-matrix" if matrix == "product" else "--real-provider-failure-matrix"
    command = [
        sys.executable,
        str(options.repo_root / "scripts" / "stage3-provider-acceptance" / "acceptance_runner.py"),
        "--suite",
        "real-provider-smoke",
        "--target",
        "ssh",
        "--provider",
        provider,
        "--runner-command-json",
        '["/usr/local/bin/provider-host"]',
        "--real-provider-credential-env",
        source.environment_name,
        "--real-provider-credential-field",
        source.field,
        "--worker-lease-ttl",
        SSH_REAL_PROVIDER_WORKER_LEASE_TTL,
        "--worker-heartbeat-timeout",
        SSH_REAL_PROVIDER_WORKER_HEARTBEAT_TIMEOUT,
        matrix_flag,
        "--output-dir",
        str(output_dir),
        "--timeout",
        str(timeout),
        "--ssh-machine-arch",
        options.ssh_machine_arch,
        "--ssh-node-version",
        options.ssh_node_version,
    ]
    if uses_external_host(options):
        assert options.ssh_external_host is not None
        assert options.ssh_external_user is not None
        assert options.ssh_external_identity_file is not None
        assert options.ssh_external_host_key_file is not None
        command.extend(
            [
                "--ssh-external-host",
                options.ssh_external_host,
                "--ssh-external-port",
                str(options.ssh_external_port),
                "--ssh-external-user",
                options.ssh_external_user,
                "--ssh-external-identity-file",
                str(options.ssh_external_identity_file),
                "--ssh-external-host-key-file",
                str(options.ssh_external_host_key_file),
                "--ssh-external-service-user",
                options.ssh_external_service_user,
                "--ssh-allow-external-host",
            ]
        )
        if options.ssh_external_use_sudo:
            command.append("--ssh-external-use-sudo")
    else:
        command.extend(
            [
                "--ssh-orbctl-bin",
                options.ssh_orbctl_bin,
                "--ssh-machine-image",
                options.ssh_machine_image,
            ]
        )
    if source.base_url_environment_name is not None:
        command.extend(["--real-provider-base-url-env", source.base_url_environment_name])
    model = remote.provider_model(options, provider)
    if model is not None:
        command.extend(["--real-provider-model", model])
    return command


def child_environment(options: SSHReleaseGateOptions, provider: str) -> dict[str, str]:
    return remote.controlled_child_environment(
        options,
        provider,
        remote.tool_environment(),
    )


def _run_metadata_command(
    options: SSHReleaseGateOptions,
    command: Sequence[str],
    *,
    timeout: float = 15.0,
) -> subprocess.CompletedProcess[str]:
    try:
        return subprocess.run(
            list(command),
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
            "release.ssh_runtime_command_failed",
            "A required SSH release-gate runtime command could not complete.",
            {"executable": pathlib.Path(command[0]).name if command else "unknown"},
        ) from None


def inspect_ssh_runtime(options: SSHReleaseGateOptions) -> dict[str, Any]:
    commands: dict[str, list[str]] = {
        "go": ["go", "version"],
        "bun": ["bun", "--version"],
        "ssh": ["ssh", "-V"],
    }
    if not uses_external_host(options):
        commands.update(
            {
                "orbctl": [options.ssh_orbctl_bin, "version"],
                "inventory": [options.ssh_orbctl_bin, "list", "--format", "json"],
            }
        )
    results = {name: _run_metadata_command(options, command) for name, command in commands.items()}
    failed = {
        name: result.returncode
        for name, result in results.items()
        if result.returncode != 0 or not result.stdout.strip()
    }
    environment = remote.tool_environment()
    if shutil.which("ssh-keygen", path=environment.get("PATH")) is None:
        failed["ssh-keygen"] = 127
    if failed:
        raise ReleaseGateError(
            "release.ssh_runtime_unavailable",
            "Required Go, Bun, OpenSSH, ssh-keygen, or disposable-host runtime metadata could not be inspected.",
            {"returnCodes": failed},
        )
    versions = expected_provider_tool_versions(options.repo_root)
    evidence: dict[str, Any] = {
        "hostTools": {
            "go": results["go"].stdout.strip()[:500],
            "bun": results["bun"].stdout.strip()[:500],
            "ssh": results["ssh"].stdout.strip()[:500],
            "sshKeygenAvailable": True,
        },
        "remoteRuntime": {
            "machineArch": options.ssh_machine_arch,
            "machineImage": options.ssh_machine_image,
            "nodeVersion": options.ssh_node_version,
            "providerToolVersions": versions,
        },
    }
    if uses_external_host(options):
        evidence["externalHost"] = {
            "explicitlyAuthorized": options.ssh_allow_external_host,
            "addressPersisted": False,
            "identitySourcePersisted": False,
            "hostKeySourcePersisted": False,
            "lifecycle": "operator-owned-nondisposable",
        }
        return evidence
    try:
        inventory_payload = json.loads(results["inventory"].stdout)
    except json.JSONDecodeError:
        inventory_payload = None
    machines = (
        inventory_payload
        if isinstance(inventory_payload, list)
        else inventory_payload.get("machines")
        if isinstance(inventory_payload, dict)
        else None
    )
    if not isinstance(machines, list):
        raise ReleaseGateError(
            "release.ssh_runtime_invalid",
            "OrbStack machine inventory was not valid JSON array evidence.",
        )
    evidence["orbctl"] = {
        "binary": options.ssh_orbctl_bin,
        "version": results["orbctl"].stdout.strip()[:500],
        "existingMachineCount": len(machines),
    }
    return evidence


def _case_evidence(report: Mapping[str, Any], case_id: str) -> Mapping[str, Any] | None:
    cases = report.get("cases")
    if not isinstance(cases, list):
        return None
    case = next(
        (
            item
            for item in cases
            if isinstance(item, dict) and item.get("id") == case_id
        ),
        None,
    )
    evidence = case.get("evidence") if isinstance(case, dict) else None
    return evidence if isinstance(evidence, dict) else None


def validate_ssh_child_runtime(
    report: Mapping[str, Any],
    *,
    provider: str,
    matrix: str,
    options: SSHReleaseGateOptions,
    expected_versions: Mapping[str, str],
) -> tuple[list[dict[str, Any]], dict[str, Any] | None]:
    errors: list[dict[str, Any]] = []
    external = uses_external_host(options)

    def fail(message: str, evidence: Mapping[str, Any] | None = None) -> None:
        errors.append(
            {
                "code": "release.child_ssh_runtime_invalid",
                "message": message,
                "provider": provider,
                "matrix": matrix,
                **({"evidence": dict(evidence)} if evidence else {}),
            }
        )

    configuration = report.get("configuration")
    worker_timing = configuration.get("workerTiming") if isinstance(configuration, dict) else None
    ssh_configuration = configuration.get("ssh") if isinstance(configuration, dict) else None
    control_plane_transport = (
        ssh_configuration.get("controlPlaneTransport")
        if isinstance(ssh_configuration, dict)
        else None
    )
    configuration_invalid = (
        not isinstance(worker_timing, dict)
        or worker_timing.get("leaseTTL") != SSH_REAL_PROVIDER_WORKER_LEASE_TTL
        or worker_timing.get("heartbeatTimeout")
        != SSH_REAL_PROVIDER_WORKER_HEARTBEAT_TIMEOUT
        or not isinstance(ssh_configuration, dict)
        or ssh_configuration.get("runtime")
        != ("authorized-external-host" if external else "owned-disposable-orbstack")
        or ssh_configuration.get("machineArch") != options.ssh_machine_arch
        or ssh_configuration.get("nodeVersion") != options.ssh_node_version
        or ssh_configuration.get("readsUserSSHConfiguration") is not False
        or not isinstance(control_plane_transport, dict)
        or control_plane_transport.get("mode") != "reverse-ssh-loopback"
        or control_plane_transport.get("vmListenHost") != acceptance.SSH_RELAY_LOOPBACK_HOST
        or ssh_configuration.get("runtimeBuild")
        != "real-provider-host-plus-locked-tools-per-run"
    )
    if external:
        configuration_invalid = configuration_invalid or (
            not isinstance(ssh_configuration, dict)
            or ssh_configuration.get("externalHostAuthorized") is not True
            or ssh_configuration.get("externalHostAddressPersisted") is not False
            or ssh_configuration.get("operatorIdentitySourcePersisted") is not False
            or ssh_configuration.get("operatorHostKeySourcePersisted") is not False
            or ssh_configuration.get("driverPrivateKeyReferenceClearedAfterProvision") is not True
            or ssh_configuration.get("operatorIdentitySourcePreserved") is not True
            or ssh_configuration.get("controlPlaneCredentialLifecycle")
            != acceptance.SSH_EXTERNAL_CREDENTIAL_LIFECYCLE
            or "orbctlBinary" in ssh_configuration
            or "machineName" in ssh_configuration
            or "machineImage" in ssh_configuration
        )
    else:
        configuration_invalid = configuration_invalid or (
            not isinstance(ssh_configuration, dict)
            or ssh_configuration.get("orbctlBinary") != options.ssh_orbctl_bin
            or ssh_configuration.get("machineName") != "generated-per-run"
            or ssh_configuration.get("machineImage") != options.ssh_machine_image
            or ssh_configuration.get("localPrivateKeyPlaintextDeletedAfterProvision") is not True
            or ssh_configuration.get("controlPlaneCredentialLifecycle")
            != acceptance.SSH_CREDENTIAL_LIFECYCLE
        )
    if configuration_invalid:
        fail("SSH child configuration did not preserve the authorized runtime boundary.")

    target_prepare = _case_evidence(report, "environment.target-prepare")
    target_provision = _case_evidence(report, "runtime.target-provision")
    cleanup = _case_evidence(report, "environment.cleanup")
    ssh = target_prepare.get("ssh") if isinstance(target_prepare, dict) else None
    if not isinstance(ssh, dict):
        fail("SSH child target-prepare evidence was missing.")
        return errors, None

    machine_name = ssh.get("machineName")
    installation_id = ssh.get("installationId")
    agentd = ssh.get("agentd")
    provider_host = ssh.get("providerHost")
    provider_tools = ssh.get("providerTools")
    provider_runtime = ssh.get("providerRuntime")
    runtime_host = (
        provider_runtime.get("providerHost") if isinstance(provider_runtime, dict) else None
    )
    runtime_tools = (
        provider_runtime.get("providerTools") if isinstance(provider_runtime, dict) else None
    )
    codex_runtime = runtime_tools.get("codex") if isinstance(runtime_tools, dict) else None
    claude_runtime = (
        runtime_tools.get("claudeAgent") if isinstance(runtime_tools, dict) else None
    )
    agentd_sha = agentd.get("sha256") if isinstance(agentd, dict) else None
    provider_host_sha = (
        provider_host.get("sha256") if isinstance(provider_host, dict) else None
    )
    driver_evidence = (
        target_provision.get("driverEvidence")
        if isinstance(target_provision, dict)
        else None
    )
    driver = driver_evidence if isinstance(driver_evidence, dict) else {}
    cleanup_evidence = cleanup if isinstance(cleanup, dict) else {}
    provision_transport = (
        driver_evidence.get("controlPlaneTransport")
        if isinstance(driver_evidence, dict)
        else None
    )
    host_key_mismatch = (
        driver_evidence.get("hostKeyMismatch")
        if isinstance(driver_evidence, dict)
        else None
    )
    service = driver_evidence.get("service") if isinstance(driver_evidence, dict) else None
    host_key_fingerprint = ssh.get("hostKeyFingerprint")
    driver_host_key_fingerprint = (
        driver_evidence.get("hostKeyFingerprint")
        if isinstance(driver_evidence, dict)
        else None
    )
    expected_package_sha = common.file_sha256(
        options.repo_root / "deploy" / "worker" / "provider-tools" / "package.json"
    )
    expected_lock_sha = common.file_sha256(
        options.repo_root / "deploy" / "worker" / "provider-tools" / "package-lock.json"
    )
    provider_tools_root = provider_tools.get("remoteRoot") if isinstance(provider_tools, dict) else None
    if external:
        root_match = (
            re.fullmatch(r"(/opt/synara/acceptance/[0-9a-f]{20})/provider-tools", provider_tools_root)
            if isinstance(provider_tools_root, str)
            else None
        )
        runtime_root = root_match.group(1) if root_match is not None else None
        expected_provider_host_path = (
            f"{runtime_root}/provider-host/index.mjs" if runtime_root is not None else None
        )
        expected_provider_command = (
            f"{runtime_root}/bin/provider-host" if runtime_root is not None else None
        )
    else:
        runtime_root = None
        expected_provider_host_path = acceptance.SSH_REMOTE_PROVIDER_HOST_PATH
        expected_provider_command = acceptance.SSH_PROVIDER_HOST_COMMAND_PATH
    common_invalid = (
        ssh.get("machineArch") != options.ssh_machine_arch
        or ssh.get("nodeVersion") != options.ssh_node_version
        or ssh.get("initSystem") != "systemd"
        or not isinstance(host_key_fingerprint, str)
        or not host_key_fingerprint.startswith("SHA256:")
        or driver_host_key_fingerprint != host_key_fingerprint
        or not isinstance(agentd_sha, str)
        or re.fullmatch(r"[0-9a-f]{64}", agentd_sha) is None
        or not isinstance(agentd, dict)
        or agentd.get("goos") != "linux"
        or agentd.get("goarch") != options.ssh_machine_arch
        or not isinstance(provider_host_sha, str)
        or re.fullmatch(r"[0-9a-f]{64}", provider_host_sha) is None
        or not isinstance(provider_host, dict)
        or provider_host.get("remotePath") != expected_provider_host_path
        or provider_host.get("runtime") != "real-provider"
        or not isinstance(provider_tools, dict)
        or provider_tools_root
        != (runtime_root + "/provider-tools" if runtime_root is not None else acceptance.SSH_REMOTE_PROVIDER_TOOLS_ROOT)
        or provider_tools.get("packageSha256") != expected_package_sha
        or provider_tools.get("lockSha256") != expected_lock_sha
        or not isinstance(provider_runtime, dict)
        or provider_runtime.get("kind") != "real-provider"
        or not isinstance(runtime_host, dict)
        or runtime_host.get("command") != expected_provider_command
        or runtime_host.get("remotePath") != expected_provider_host_path
        or runtime_host.get("sha256") != provider_host_sha
        or not isinstance(runtime_tools, dict)
        or runtime_tools.get("lockedInstall") is not True
        or runtime_tools.get("remoteRoot") != provider_tools_root
        or not isinstance(codex_runtime, dict)
        or codex_runtime.get("version") != expected_versions["codex"]
        or not isinstance(claude_runtime, dict)
        or claude_runtime.get("version") != expected_versions["claudeAgent"]
        or not isinstance(driver_evidence, dict)
        or driver_evidence.get("binarySha256") != agentd_sha
        or not isinstance(host_key_mismatch, dict)
        or host_key_mismatch.get("rejected") is not True
        or host_key_mismatch.get("errorCode") != "ssh_connection_failed"
        or not isinstance(service, dict)
        or service.get("activeState") != "active"
        or service.get("subState") != "running"
        or not isinstance(provision_transport, dict)
        or provision_transport.get("mode") != "reverse-ssh-loopback"
        or provision_transport.get("readsUserSSHConfiguration") is not False
        or not isinstance(cleanup, dict)
    )
    if external:
        mode_invalid = (
            runtime_root is None
            or not isinstance(installation_id, str)
            or re.fullmatch(r"stage3-provider-acceptance-[0-9a-f-]{36}", installation_id) is None
            or ssh.get("runtime") != "authorized-external-host"
            or ssh.get("ownedMachine") is not False
            or ssh.get("externalHostAuthorized") is not True
            or ssh.get("externalHostAddressPersisted") is not False
            or ssh.get("sshd") != "active-not-restarted"
            or ssh.get("operatorIdentitySourcePreserved") is not True
            or ssh.get("driverPrivateKeyReferenceClearedAfterProvision") is not True
            or ssh.get("controlPlaneCredentialLifecycle")
            != acceptance.SSH_EXTERNAL_CREDENTIAL_LIFECYCLE
            or driver.get("runtime") != "authorized-external-host"
            or driver.get("ownedMachine") is not False
            or driver.get("installationId") != installation_id
            or driver.get("controlPlaneCredentialLifecycle")
            != acceptance.SSH_EXTERNAL_CREDENTIAL_LIFECYCLE
            or cleanup_evidence.get("runtime") != "authorized-external-host"
            or cleanup_evidence.get("installationId") != installation_id
            or "machineName" in ssh
            or "machineAddress" in driver
        )
        runtime_identity = installation_id
    else:
        mode_invalid = (
            not isinstance(machine_name, str)
            or re.fullmatch(r"synara-stage3-[0-9a-f]{12}", machine_name) is None
            or ssh.get("ownedMachine") is not True
            or ssh.get("machineImage") != options.ssh_machine_image
            or ssh.get("sshd") != "active"
            or ssh.get("algorithm") != "ssh-ed25519"
            or ssh.get("localPrivateKeyPlaintextDeletedAfterProvision") is not True
            or driver.get("machineName") != machine_name
            or driver.get("controlPlaneCredentialLifecycle")
            != acceptance.SSH_CREDENTIAL_LIFECYCLE
            or cleanup_evidence.get("machineName") != machine_name
        )
        runtime_identity = machine_name
    if common_invalid or mode_invalid:
        fail("SSH child runtime evidence did not prove the locked authorized runtime boundary.")
        return errors, None

    return (
        errors,
        {
            "runtime": "authorized-external-host" if external else "owned-disposable-orbstack",
            "runtimeIdentity": runtime_identity,
            **({"installationId": installation_id} if external else {"machineName": machine_name}),
            "hostKeyFingerprint": host_key_fingerprint,
            "agentdSha256": agentd_sha,
            "providerHostSha256": provider_host_sha,
            "codexVersion": codex_runtime["version"],
            "claudeVersion": claude_runtime["version"],
        },
    )


def run_ssh_child_report(
    *,
    options: SSHReleaseGateOptions,
    provider: str,
    matrix: str,
    expected_git_sha: str,
    policy: common.ChildReportPolicy,
    expected_versions: Mapping[str, str],
) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    child_dir = options.output_dir / provider / matrix
    record, errors = common.run_child_report(
        repo_root=options.repo_root,
        output_dir=options.output_dir,
        provider=provider,
        matrix=matrix,
        expected_git_sha=expected_git_sha,
        command=child_command(options, provider, matrix, child_dir),
        policy=policy,
        environment=child_environment(options, provider),
    )
    json_path = child_dir / acceptance.JSON_REPORT_NAME
    if not json_path.is_file():
        return record, errors
    try:
        report = json.loads(json_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        return record, errors
    if not isinstance(report, dict):
        return record, errors
    runtime_errors, runtime = validate_ssh_child_runtime(
        report,
        provider=provider,
        matrix=matrix,
        options=options,
        expected_versions=expected_versions,
    )
    errors.extend(runtime_errors)
    if runtime is not None:
        record["sshRuntime"] = runtime
    if runtime_errors:
        record["status"] = "fail"
    return record, errors


def configuration_evidence(options: SSHReleaseGateOptions) -> dict[str, Any]:
    return {
        "runnerCommand": {"executable": "provider-host", "argumentCount": 0},
        "productTimeoutSeconds": options.product_timeout_seconds,
        "failureTimeoutSeconds": options.failure_timeout_seconds,
        "separateChildBoundaries": True,
        "credentialEnvironmentNamesRecordedByGate": False,
        "workerTiming": {
            "leaseTTL": SSH_REAL_PROVIDER_WORKER_LEASE_TTL,
            "heartbeatTimeout": SSH_REAL_PROVIDER_WORKER_HEARTBEAT_TIMEOUT,
        },
        "credentials": {
            provider: {
                "field": credential_source(options, provider).field,
                "controlledBaseUrl": (
                    credential_source(options, provider).base_url_environment_name is not None
                ),
                "model": provider_model(options, provider),
            }
            for provider in common.PROVIDERS
        },
        "ssh": {
            "machineArch": options.ssh_machine_arch,
            "nodeVersion": options.ssh_node_version,
            "runtime": (
                "authorized-external-host"
                if uses_external_host(options)
                else "owned-disposable-orbstack"
            ),
            **(
                {
                    "externalHostExplicitlyAuthorized": options.ssh_allow_external_host,
                    "externalHostAddressPersisted": False,
                    "identitySourcePersisted": False,
                    "hostKeySourcePersisted": False,
                    "machineLifecycle": "operator-owned-nondisposable",
                }
                if uses_external_host(options)
                else {
                    "orbctlBinary": options.ssh_orbctl_bin,
                    "machineImage": options.ssh_machine_image,
                    "machineLifecycle": "owned-disposable-per-child",
                }
            ),
            "runtimeBuild": "real-provider-host-plus-locked-tools-per-child",
        },
    }


def _runtime_consensus_errors(runs: Sequence[Mapping[str, Any]]) -> list[dict[str, Any]]:
    fields = {
        "agentdSha256": "release.ssh_agentd_digest_mismatch",
        "providerHostSha256": "release.ssh_provider_host_digest_mismatch",
        "codexVersion": "release.ssh_codex_version_mismatch",
        "claudeVersion": "release.ssh_claude_version_mismatch",
    }
    errors: list[dict[str, Any]] = []
    for field, code in fields.items():
        values = {
            runtime.get(field)
            for run in runs
            if isinstance((runtime := run.get("sshRuntime")), dict)
            and isinstance(runtime.get(field), str)
        }
        if len(values) != 1 or len(runs) == 0 or any(
            not isinstance(run.get("sshRuntime"), dict)
            or not isinstance(run["sshRuntime"].get(field), str)
            for run in runs
        ):
            errors.append(
                {
                    "code": code,
                    "message": f"SSH child reports did not share one {field} value.",
                    "evidence": {"distinctValueCount": len(values), "runCount": len(runs)},
                }
            )
    runtimes = [
        runtime
        for run in runs
        if isinstance((runtime := run.get("sshRuntime")), dict)
    ]
    runtime_modes = {
        runtime.get("runtime")
        for runtime in runtimes
        if isinstance(runtime.get("runtime"), str)
    }
    host_key_fingerprints = {
        runtime.get("hostKeyFingerprint")
        for runtime in runtimes
        if isinstance(runtime.get("hostKeyFingerprint"), str)
    }
    every_run_has_host_key = len(runtimes) == len(runs) and all(
        isinstance(runtime.get("hostKeyFingerprint"), str) for runtime in runtimes
    )
    if runtime_modes == {"authorized-external-host"}:
        if not every_run_has_host_key or len(host_key_fingerprints) != 1:
            errors.append(
                {
                    "code": "release.ssh_host_key_mismatch",
                    "message": "External SSH child reports did not share one pinned Host Key fingerprint.",
                    "evidence": {
                        "distinctValueCount": len(host_key_fingerprints),
                        "runCount": len(runs),
                    },
                }
            )
    elif runtime_modes == {"owned-disposable-orbstack"}:
        if not every_run_has_host_key or len(host_key_fingerprints) != len(runs):
            errors.append(
                {
                    "code": "release.ssh_host_key_isolation_invalid",
                    "message": "Disposable SSH child reports did not use one distinct Host Key per owned machine.",
                    "evidence": {
                        "distinctValueCount": len(host_key_fingerprints),
                        "runCount": len(runs),
                    },
                }
            )
    runtime_identities = {
        runtime.get("runtimeIdentity")
        for run in runs
        if isinstance((runtime := run.get("sshRuntime")), dict)
        and isinstance(runtime.get("runtimeIdentity"), str)
    }
    if len(runtime_identities) != len(runs) or len(runs) == 0:
        errors.append(
            {
                "code": (
                    "release.ssh_installation_isolation_invalid"
                    if runs and any(
                        isinstance(run.get("sshRuntime"), dict)
                        and run["sshRuntime"].get("runtime") == "authorized-external-host"
                        for run in runs
                    )
                    else "release.ssh_machine_isolation_invalid"
                ),
                "message": "SSH child reports did not use one distinct owned installation boundary per matrix.",
                "evidence": {
                    "distinctRuntimeIdentityCount": len(runtime_identities),
                    "runCount": len(runs),
                },
            }
        )
    return errors


def markdown_from_report(report: Mapping[str, Any]) -> str:
    configuration = report.get("configuration")
    ssh_configuration = configuration.get("ssh") if isinstance(configuration, dict) else None
    external = (
        isinstance(ssh_configuration, dict)
        and ssh_configuration.get("runtime") == "authorized-external-host"
    )
    lines = [
        "# Stage 3 Real Provider SSH Release Gate",
        "",
        f"- Schema: `{report['schemaVersion']}`",
        f"- Run: `{report['runId']}`",
        f"- Status: **{report['status']}**",
        f"- Git SHA: `{report.get('source', {}).get('gitSha', '')}`",
        f"- Provider Host SHA: `{report.get('runtimeArtifacts', {}).get('providerHostSha256', '')}`",
        f"- Duration: `{report['durationMs']} ms`",
        "",
        (
            "Each child uses the explicitly authorized non-disposable host through one repository-external identity,"
            if external
            else "Each child owns one disposable OrbStack VM, one generated SSH key, one Control Plane state boundary,"
        ),
        (
            "one pinned Host Key and one unique ownership-marked runtime. Product revoke and exact owned-runtime cleanup"
            if external
            else "and one real Provider Host/tools installation. The aggregate passes only when all four children share"
        ),
        (
            "must preserve the host and identity source; all four children must share one clean SHA, runtime digest,"
            if external
            else "one clean SHA, Capability Catalog, agentd/Host digest and locked Provider versions, while using distinct"
        ),
        (
            "locked Provider versions and Host Key fingerprint with empty Secret scans."
            if external
            else "machines with exact cleanup and empty Secret scans."
        ),
        "",
        "## Child matrices",
        "",
        "| Provider | Matrix | Status | Cases | Unsupported | Runtime ID | Host SHA-256 | JSON SHA-256 |",
        "| --- | --- | --- | --- | --- | --- | --- | --- |",
    ]
    for run in report.get("runs", []):
        if not isinstance(run, dict):
            continue
        counts = run.get("caseCounts") if isinstance(run.get("caseCounts"), dict) else {}
        runtime = run.get("sshRuntime") if isinstance(run.get("sshRuntime"), dict) else {}
        case_summary = ", ".join(
            f"{status}={counts.get(status, 0)}"
            for status in ("pass", "unsupported", "skipped", "fail")
        )
        unsupported = ", ".join(run.get("unsupportedCaseIds", [])) or "none"
        lines.append(
            f"| `{run.get('provider', '')}` | `{run.get('matrix', '')}` | {run.get('status', '')} | "
            f"{case_summary} | {unsupported} | `{runtime.get('runtimeIdentity', '')}` | "
            f"`{runtime.get('providerHostSha256', '')}` | `{run.get('reportSha256', '')}` |"
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
            (
                "A pass closes the implemented real Codex/Claude authorized external-SSH product and controlled-failure slice."
                if external
                else "A pass closes the implemented real Codex/Claude disposable-SSH product and controlled-failure slice."
            ),
            "It does not close Docker, Kubernetes, registry rollout, concurrency, broad production SSH policy, or soak gates.",
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


def run_ssh_release_gate(
    options: SSHReleaseGateOptions,
    *,
    repository_state: Any = common.repository_state,
    runtime_inspector: Any = inspect_ssh_runtime,
    child_runner: Any = run_ssh_child_report,
) -> int:
    if options.output_dir.exists() and (
        not options.output_dir.is_dir() or any(options.output_dir.iterdir())
    ):
        print("SSH release gate output directory must be empty or absent.", file=sys.stderr)
        return 2
    options.output_dir.mkdir(parents=True, exist_ok=True)
    redactor = remote.credential_redactor(options)
    if options.ssh_external_host is not None:
        redactor.add(options.ssh_external_host, "[REDACTED_SSH_EXTERNAL_HOST]")
    if options.ssh_external_identity_file is not None:
        redactor.add(
            str(options.ssh_external_identity_file),
            "[REDACTED_SSH_IDENTITY_SOURCE]",
        )
    if options.ssh_external_host_key_file is not None:
        redactor.add(
            str(options.ssh_external_host_key_file),
            "[REDACTED_SSH_HOST_KEY_SOURCE]",
        )
    started_at = acceptance.utc_now()
    started = time.monotonic()
    run_id = f"stage3-provider-ssh-release-{uuid.uuid4()}"
    try:
        source = repository_state(options.repo_root)
        runtime = dict(runtime_inspector(options))
        expected_versions = expected_provider_tool_versions(options.repo_root)
    except (OSError, subprocess.SubprocessError, ReleaseGateError) as raw_error:
        error = (
            raw_error
            if isinstance(raw_error, ReleaseGateError)
            else ReleaseGateError(
                "release.preflight_failed",
                "SSH release gate preflight failed.",
            )
        )
        report = {
            "schemaVersion": SCHEMA_VERSION,
            "runId": run_id,
            "mode": "real-provider-ssh-release-gate",
            "target": "ssh",
            "status": "fail",
            "startedAt": started_at,
            "finishedAt": acceptance.utc_now(),
            "durationMs": acceptance.elapsed_ms(started),
            "configuration": configuration_evidence(options),
            "runs": [],
            "errors": [error.as_report_error()],
        }
        json_path, markdown_path = write_report(report, options.output_dir, redactor)
        print("Stage 3 real Provider SSH release gate: fail")
        print(f"JSON: {json_path}")
        print(f"Markdown: {markdown_path}")
        return 1

    policy = child_policy(options)
    runs: list[dict[str, Any]] = []
    errors: list[dict[str, Any]] = []
    try:
        for provider in common.PROVIDERS:
            for matrix in common.MATRICES:
                run, child_errors = child_runner(
                    options=options,
                    provider=provider,
                    matrix=matrix,
                    expected_git_sha=str(source["gitSha"]),
                    policy=policy,
                    expected_versions=expected_versions,
                )
                runs.append(run)
                errors.extend(child_errors)
    except Exception as raw_error:
        errors.append(
            (
                raw_error
                if isinstance(raw_error, ReleaseGateError)
                else ReleaseGateError(
                    "release.execution_failed",
                    "The SSH release gate could not complete its child execution phase.",
                )
            ).as_report_error()
        )

    required_runs = len(common.PROVIDERS) * len(common.MATRICES)
    if len(runs) != required_runs:
        errors.append(
            {
                "code": "release.child_coverage_incomplete",
                "message": "The SSH release gate did not complete all required child matrices.",
                "evidence": {"requiredRuns": required_runs, "completedRuns": len(runs)},
            }
        )
    if runs:
        errors.extend(common.catalog_consensus_errors(runs))
        errors.extend(_runtime_consensus_errors(runs))
    output_secret_scan = remote.scan_child_outputs(options, redactor)
    if output_secret_scan["findings"]:
        errors.append(
            {
                "code": "release.aggregate_secret_scan_failed",
                "message": "Aggregate SSH child output scan found controlled Credential material.",
                "evidence": {"findingCount": len(output_secret_scan["findings"])},
            }
        )
    environment_name_findings = remote.credential_environment_name_findings(options)
    if environment_name_findings:
        errors.append(
            {
                "code": "release.credential_environment_name_persisted",
                "message": "SSH child output persisted an operator Credential environment-variable name.",
                "evidence": {"files": environment_name_findings},
            }
        )
    status = (
        "pass"
        if not errors
        and len(runs) == required_runs
        and all(run.get("status") == "pass" for run in runs)
        else "fail"
    )
    catalog_hashes = {
        source_value.get("providerCapabilityCatalogSha256")
        for run in runs
        if isinstance((source_value := run.get("source")), dict)
        and isinstance(source_value.get("providerCapabilityCatalogSha256"), str)
    }
    runtime_fields = {
        field: {
            runtime_value.get(field)
            for run in runs
            if isinstance((runtime_value := run.get("sshRuntime")), dict)
            and isinstance(runtime_value.get(field), str)
        }
        for field in (
            "agentdSha256",
            "providerHostSha256",
            "codexVersion",
            "claudeVersion",
            "hostKeyFingerprint",
        )
    }
    runtime_identities = sorted(
        runtime_value["runtimeIdentity"]
        for run in runs
        if isinstance((runtime_value := run.get("sshRuntime")), dict)
        and isinstance(runtime_value.get("runtimeIdentity"), str)
    )
    report = {
        "schemaVersion": SCHEMA_VERSION,
        "runId": run_id,
        "mode": "real-provider-ssh-release-gate",
        "target": "ssh",
        "status": status,
        "source": {
            **source,
            "providerCapabilityCatalogSha256": (
                next(iter(catalog_hashes)) if len(catalog_hashes) == 1 else None
            ),
        },
        "runtime": runtime,
        "runtimeArtifacts": {
            field: next(iter(values)) if len(values) == 1 else None
            for field, values in runtime_fields.items()
        },
        "isolation": {
            "runtime": (
                "authorized-external-host"
                if uses_external_host(options)
                else "owned-disposable-orbstack"
            ),
            "ownedDisposableMachinePerRun": not uses_external_host(options),
            "externalHostPreserved": uses_external_host(options),
            "requiredRuntimeIdentityCount": required_runs,
            "distinctRuntimeIdentityCount": len(set(runtime_identities)),
            "runtimeIdentities": runtime_identities,
            "generatedPrivateKeyPerRun": not uses_external_host(options),
            "operatorIdentitySourceReusedAcrossRuns": uses_external_host(options),
        },
        "startedAt": started_at,
        "finishedAt": acceptance.utc_now(),
        "durationMs": acceptance.elapsed_ms(started),
        "configuration": configuration_evidence(options),
        "coverage": {
            "requiredRuns": required_runs,
            "completedRuns": len(runs),
            "providers": list(common.PROVIDERS),
            "productCases": list(acceptance.REAL_PROVIDER_CASES),
            "failureCases": list(acceptance.REAL_PROVIDER_FAILURE_CASES),
        },
        "security": {
            "rawChildOutputPersisted": False,
            "childSecretScansRequired": True,
            "childCleanupRequired": True,
            "credentialEnvironmentNamesPersisted": bool(environment_name_findings),
            "aggregateChildOutputScan": output_secret_scan,
        },
        "runs": runs,
        "errors": errors,
    }
    json_path, markdown_path = write_report(report, options.output_dir, redactor)
    print(f"Stage 3 real Provider SSH release gate: {status}")
    print(f"JSON: {json_path}")
    print(f"Markdown: {markdown_path}")
    return 0 if status == "pass" else 1


def main(argv: Sequence[str] | None = None) -> int:
    options = parse_args(argv if argv is not None else sys.argv[1:])
    return run_ssh_release_gate(options)


if __name__ == "__main__":
    raise SystemExit(main())
