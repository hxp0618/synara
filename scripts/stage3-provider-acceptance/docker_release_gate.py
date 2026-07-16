#!/usr/bin/env python3
"""Run the clean-SHA real Codex/Claude Docker product and failure release gate."""

from __future__ import annotations

import argparse
import dataclasses
import pathlib
import re
import subprocess
import sys
from collections.abc import Mapping, Sequence
from typing import Any

import acceptance_runner as acceptance
import controlled_remote_release_gate as remote
import release_gate_common as common


SCHEMA_VERSION = "synara.provider-docker-release-gate.v1"
JSON_REPORT_NAME = "docker-release-gate.json"
MARKDOWN_REPORT_NAME = "docker-release-gate.md"
EXPECTED_UNSUPPORTED: Mapping[tuple[str, str], frozenset[str]] = {
    ("codex", "product"): frozenset({"real-provider.terminal-large-log"}),
    ("claudeAgent", "product"): frozenset({"real-provider.compact-boundary"}),
    ("codex", "failure"): frozenset(),
    ("claudeAgent", "failure"): frozenset(),
}

CredentialSource = remote.CredentialSource
GateWorkerImage = remote.GateWorkerImage
ReleaseGateError = remote.ReleaseGateError
IMAGE_ACCEPTANCE_LABEL = remote.IMAGE_ACCEPTANCE_LABEL
IMAGE_GATE_LABEL = remote.IMAGE_GATE_LABEL
IMAGE_OWNER_LABEL = remote.IMAGE_OWNER_LABEL
IMAGE_TARGET_LABEL = remote.IMAGE_TARGET_LABEL


@dataclasses.dataclass(frozen=True)
class DockerReleaseGateOptions:
    repo_root: pathlib.Path
    output_dir: pathlib.Path
    product_timeout_seconds: float
    failure_timeout_seconds: float
    codex_credential: CredentialSource
    claude_credential: CredentialSource
    docker_socket_path: pathlib.Path
    docker_control_plane_host: str
    docker_memory_bytes: int
    docker_nano_cpus: int


def parse_args(argv: Sequence[str]) -> DockerReleaseGateOptions:
    repo_root = pathlib.Path(__file__).resolve().parents[2]
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--codex-credential-env", required=True)
    parser.add_argument("--codex-base-url-env")
    parser.add_argument("--claude-credential-env", required=True)
    parser.add_argument(
        "--claude-credential-field",
        choices=acceptance.REAL_PROVIDER_CREDENTIAL_FIELDS,
        default="apiKey",
    )
    parser.add_argument("--claude-base-url-env")
    parser.add_argument("--output-dir", type=pathlib.Path)
    parser.add_argument("--product-timeout", type=float, default=2400.0)
    parser.add_argument("--failure-timeout", type=float, default=900.0)
    parser.add_argument(
        "--docker-socket-path",
        type=pathlib.Path,
        default=pathlib.Path("/var/run/docker.sock"),
    )
    parser.add_argument("--docker-control-plane-host", default="host.docker.internal")
    parser.add_argument("--docker-memory-bytes", type=int, default=2 << 30)
    parser.add_argument("--docker-nano-cpus", type=int, default=1_000_000_000)
    parsed = parser.parse_args(argv)
    if parsed.product_timeout <= 0 or parsed.failure_timeout <= 0:
        parser.error("matrix timeouts must be positive")
    if parsed.docker_memory_bytes < 64 << 20:
        parser.error("--docker-memory-bytes must be at least 67108864")
    if parsed.docker_nano_cpus <= 0:
        parser.error("--docker-nano-cpus must be positive")
    docker_socket_path = parsed.docker_socket_path.expanduser().resolve()
    if not docker_socket_path.is_absolute():
        parser.error("--docker-socket-path must be absolute")
    docker_host = parsed.docker_control_plane_host.strip()
    if not docker_host or re.fullmatch(r"[A-Za-z0-9._-]+", docker_host) is None:
        parser.error("--docker-control-plane-host must be a hostname or address without scheme or port")
    try:
        codex_credential = parse_credential_source(
            parsed.codex_credential_env,
            "apiKey",
            parsed.codex_base_url_env,
            "Codex",
        )
        claude_credential = parse_credential_source(
            parsed.claude_credential_env,
            parsed.claude_credential_field,
            parsed.claude_base_url_env,
            "Claude",
        )
    except ValueError as error:
        parser.error(str(error))
    output_dir = parsed.output_dir or remote.default_output_dir(repo_root, "docker")
    return DockerReleaseGateOptions(
        repo_root=repo_root,
        output_dir=output_dir.expanduser().resolve(),
        product_timeout_seconds=parsed.product_timeout,
        failure_timeout_seconds=parsed.failure_timeout,
        codex_credential=codex_credential,
        claude_credential=claude_credential,
        docker_socket_path=docker_socket_path,
        docker_control_plane_host=docker_host,
        docker_memory_bytes=parsed.docker_memory_bytes,
        docker_nano_cpus=parsed.docker_nano_cpus,
    )


parse_credential_source = remote.parse_credential_source
credential_source = remote.credential_source
tool_environment = remote.tool_environment
docker_environment = remote.docker_environment
docker_completed = remote.docker_completed
docker_result_not_found = remote.docker_result_not_found
inspect_gate_worker_image = remote.inspect_gate_worker_image
required_gate_worker_image_labels = remote.required_gate_worker_image_labels
build_gate_worker_image = remote.build_gate_worker_image
cleanup_gate_worker_image = remote.cleanup_gate_worker_image
child_environment = remote.child_environment
credential_redactor = remote.credential_redactor
scan_child_outputs = remote.scan_child_outputs
credential_environment_name_findings = remote.credential_environment_name_findings
worker_image_reference_errors = remote.worker_image_reference_errors


def child_policy(
    options: DockerReleaseGateOptions,
    worker_image_name: str,
) -> common.ChildReportPolicy:
    return remote.child_policy(options, target_spec(), worker_image_name)


def child_command(
    options: DockerReleaseGateOptions,
    provider: str,
    matrix: str,
    output_dir: pathlib.Path,
    worker_image_name: str,
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
        "docker",
        "--provider",
        provider,
        "--runner-command-json",
        '["/usr/local/bin/provider-host"]',
        "--real-provider-credential-env",
        source.environment_name,
        "--real-provider-credential-field",
        source.field,
        matrix_flag,
        "--output-dir",
        str(output_dir),
        "--timeout",
        str(timeout),
        "--docker-socket-path",
        str(options.docker_socket_path),
        "--docker-control-plane-host",
        options.docker_control_plane_host,
        "--docker-memory-bytes",
        str(options.docker_memory_bytes),
        "--docker-nano-cpus",
        str(options.docker_nano_cpus),
        "--docker-worker-image",
        worker_image_name,
        "--docker-skip-worker-build",
    ]
    if source.base_url_environment_name is not None:
        command.extend(["--real-provider-base-url-env", source.base_url_environment_name])
    return command


def inspect_docker_runtime(options: DockerReleaseGateOptions) -> dict[str, Any]:
    environment = docker_environment(options)
    version = subprocess.run(
        ["docker", "version", "--format", "{{.Server.Version}}"],
        cwd=options.repo_root,
        env=environment,
        check=False,
        capture_output=True,
        text=True,
        timeout=15.0,
    )
    platform = subprocess.run(
        ["docker", "info", "--format", "{{.OSType}}/{{.Architecture}}"],
        cwd=options.repo_root,
        env=environment,
        check=False,
        capture_output=True,
        text=True,
        timeout=15.0,
    )
    if version.returncode != 0 or not version.stdout.strip() or platform.returncode != 0:
        raise ReleaseGateError(
            "release.docker_runtime_unavailable",
            "Docker Engine runtime metadata could not be inspected.",
            {"versionReturnCode": version.returncode, "infoReturnCode": platform.returncode},
        )
    return {
        "serverVersion": version.stdout.strip(),
        "platform": platform.stdout.strip(),
        "socketPath": str(options.docker_socket_path),
    }


def inspect_docker_runtime_report(options: DockerReleaseGateOptions) -> dict[str, Any]:
    return {"docker": inspect_docker_runtime(options)}


def target_configuration(options: DockerReleaseGateOptions) -> dict[str, Any]:
    return {
        "socketPath": str(options.docker_socket_path),
        "controlPlaneHost": options.docker_control_plane_host,
        "memoryBytes": options.docker_memory_bytes,
        "nanoCpus": options.docker_nano_cpus,
    }


def target_spec() -> remote.RemoteReleaseTargetSpec:
    return remote.RemoteReleaseTargetSpec(
        target="docker",
        display_name="Docker",
        schema_version=SCHEMA_VERSION,
        json_report_name=JSON_REPORT_NAME,
        markdown_report_name=MARKDOWN_REPORT_NAME,
        mode="real-provider-docker-release-gate",
        worker_image_repository="synara-stage3-provider-release-gate",
        expected_unsupported=EXPECTED_UNSUPPORTED,
        cleanup_true_fields=(
            "managedWorkerContainersRemoved",
            "workspaceVolumeRemoved",
            "ownedNetworkRemoved",
            "stateRemoved",
        ),
        cleanup_false_fields=("broadCleanupUsed", "ownedImageRemoved"),
        worker_image_evidence_path=("docker",),
        child_command=child_command,
        inspect_runtime=inspect_docker_runtime_report,
        target_configuration=target_configuration,
        evidence_boundary=(
            "A pass closes the implemented real Codex/Claude Docker product and controlled-failure release slice.",
            "It does not close SSH, Kubernetes, registry-pushed multi-arch rollout, concurrency, or soak gates.",
        ),
    )


def gate_worker_image(git_sha: str, owner: str) -> GateWorkerImage:
    return remote.gate_worker_image(git_sha, owner, target_spec())


def markdown_from_report(report: Mapping[str, Any]) -> str:
    return remote.markdown_from_report(report, target_spec())


def write_report(
    report: Mapping[str, Any],
    output_dir: pathlib.Path,
    redactor: acceptance.SecretRedactor,
) -> tuple[pathlib.Path, pathlib.Path]:
    return remote.write_report(report, output_dir, redactor, target_spec())


def failure_report(
    *,
    run_id: str,
    started_at: str,
    started: float,
    options: DockerReleaseGateOptions,
    error: ReleaseGateError,
) -> dict[str, Any]:
    return remote.failure_report(
        run_id=run_id,
        started_at=started_at,
        started=started,
        options=options,
        spec=target_spec(),
        error=error,
    )


def configuration_evidence(options: DockerReleaseGateOptions) -> dict[str, Any]:
    return remote.configuration_evidence(options, target_spec())


def main(argv: Sequence[str] | None = None) -> int:
    options = parse_args(argv if argv is not None else sys.argv[1:])
    return remote.run_remote_release_gate(
        options,
        target_spec(),
        build_image=build_gate_worker_image,
        cleanup_image=cleanup_gate_worker_image,
        repository_state=common.repository_state,
        run_child=common.run_child_report,
    )


if __name__ == "__main__":
    raise SystemExit(main())
