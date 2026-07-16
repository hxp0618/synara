#!/usr/bin/env python3
"""Run the clean-SHA real Codex/Claude Kubernetes product and failure release gate."""

from __future__ import annotations

import argparse
import dataclasses
import json
import pathlib
import re
import subprocess
import sys
from collections.abc import Mapping, Sequence
from typing import Any

import acceptance_runner as acceptance
import controlled_remote_release_gate as remote
import release_gate_common as common


SCHEMA_VERSION = "synara.provider-kubernetes-release-gate.v1"
JSON_REPORT_NAME = "kubernetes-release-gate.json"
MARKDOWN_REPORT_NAME = "kubernetes-release-gate.md"
EXPECTED_UNSUPPORTED: Mapping[tuple[str, str], frozenset[str]] = {
    ("codex", "product"): frozenset({"real-provider.terminal-large-log"}),
    ("claudeAgent", "product"): frozenset({"real-provider.compact-boundary"}),
    ("codex", "failure"): frozenset(),
    ("claudeAgent", "failure"): frozenset(),
}

CredentialSource = remote.CredentialSource
GateWorkerImage = remote.GateWorkerImage
ReleaseGateError = remote.ReleaseGateError


@dataclasses.dataclass(frozen=True)
class KubernetesReleaseGateOptions:
    repo_root: pathlib.Path
    output_dir: pathlib.Path
    product_timeout_seconds: float
    failure_timeout_seconds: float
    codex_credential: CredentialSource
    claude_credential: CredentialSource
    docker_socket_path: pathlib.Path
    kubernetes_control_plane_host: str
    kind_bin: str
    kind_node_image: str


def parse_args(argv: Sequence[str]) -> KubernetesReleaseGateOptions:
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
    parser.add_argument("--product-timeout", type=float, default=3600.0)
    parser.add_argument("--failure-timeout", type=float, default=1200.0)
    parser.add_argument(
        "--docker-socket-path",
        type=pathlib.Path,
        default=pathlib.Path("/var/run/docker.sock"),
    )
    parser.add_argument("--kubernetes-control-plane-host", default="host.docker.internal")
    parser.add_argument("--kind-bin", default="kind")
    parser.add_argument("--kind-node-image", default="kindest/node:v1.33.1")
    parsed = parser.parse_args(argv)
    if parsed.product_timeout <= 0 or parsed.failure_timeout <= 0:
        parser.error("matrix timeouts must be positive")
    docker_socket_path = parsed.docker_socket_path.expanduser().resolve()
    if not docker_socket_path.is_absolute():
        parser.error("--docker-socket-path must be absolute")
    control_plane_host = parsed.kubernetes_control_plane_host.strip()
    if not control_plane_host or re.fullmatch(r"[A-Za-z0-9._-]+", control_plane_host) is None:
        parser.error(
            "--kubernetes-control-plane-host must be a hostname or address without scheme or port"
        )
    kind_bin = parsed.kind_bin.strip()
    kind_node_image = parsed.kind_node_image.strip()
    if not kind_bin or any(character in kind_bin for character in "\r\n\t\x00"):
        parser.error("--kind-bin must be a non-empty executable name or path")
    if not kind_node_image or any(character in kind_node_image for character in "\r\n\t\x00"):
        parser.error("--kind-node-image must be a non-empty image reference")
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
    except ValueError as error:
        parser.error(str(error))
    output_dir = parsed.output_dir or remote.default_output_dir(repo_root, "kubernetes")
    return KubernetesReleaseGateOptions(
        repo_root=repo_root,
        output_dir=output_dir.expanduser().resolve(),
        product_timeout_seconds=parsed.product_timeout,
        failure_timeout_seconds=parsed.failure_timeout,
        codex_credential=codex_credential,
        claude_credential=claude_credential,
        docker_socket_path=docker_socket_path,
        kubernetes_control_plane_host=control_plane_host,
        kind_bin=kind_bin,
        kind_node_image=kind_node_image,
    )


def credential_source(options: KubernetesReleaseGateOptions, provider: str) -> CredentialSource:
    return remote.credential_source(options, provider)


def child_policy(
    options: KubernetesReleaseGateOptions,
    worker_image_name: str,
) -> common.ChildReportPolicy:
    return remote.child_policy(options, target_spec(), worker_image_name)


def child_command(
    options: KubernetesReleaseGateOptions,
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
        "kubernetes",
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
        "--kubernetes-control-plane-host",
        options.kubernetes_control_plane_host,
        "--kind-bin",
        options.kind_bin,
        "--kind-node-image",
        options.kind_node_image,
        "--kubernetes-worker-image",
        worker_image_name,
        "--kubernetes-skip-worker-build",
    ]
    if source.base_url_environment_name is not None:
        command.extend(["--real-provider-base-url-env", source.base_url_environment_name])
    return command


def _run_metadata_command(
    options: KubernetesReleaseGateOptions,
    command: Sequence[str],
    *,
    environment: Mapping[str, str],
    timeout: float = 15.0,
) -> subprocess.CompletedProcess[str]:
    try:
        return subprocess.run(
            list(command),
            cwd=options.repo_root,
            env=dict(environment),
            check=False,
            capture_output=True,
            text=True,
            timeout=timeout,
        )
    except (OSError, subprocess.TimeoutExpired):
        raise ReleaseGateError(
            "release.kubernetes_runtime_command_failed",
            "A required Kubernetes release-gate runtime command could not complete.",
            {"executable": pathlib.Path(command[0]).name if command else "unknown"},
        ) from None


def inspect_kubernetes_runtime(options: KubernetesReleaseGateOptions) -> dict[str, Any]:
    docker_environment = remote.docker_environment(options)
    tool_environment = remote.tool_environment()
    docker_version = _run_metadata_command(
        options,
        ["docker", "version", "--format", "{{.Server.Version}}"],
        environment=docker_environment,
    )
    docker_platform = _run_metadata_command(
        options,
        ["docker", "info", "--format", "{{.OSType}}/{{.Architecture}}"],
        environment=docker_environment,
    )
    kind_version = _run_metadata_command(
        options,
        [options.kind_bin, "version"],
        environment=docker_environment,
    )
    kubectl_version = _run_metadata_command(
        options,
        ["kubectl", "version", "--client", "-o", "json"],
        environment=tool_environment,
    )
    if (
        docker_version.returncode != 0
        or not docker_version.stdout.strip()
        or docker_platform.returncode != 0
        or kind_version.returncode != 0
        or not kind_version.stdout.strip()
        or kubectl_version.returncode != 0
    ):
        raise ReleaseGateError(
            "release.kubernetes_runtime_unavailable",
            "Docker, Kind, or kubectl runtime metadata could not be inspected.",
            {
                "dockerVersionReturnCode": docker_version.returncode,
                "dockerInfoReturnCode": docker_platform.returncode,
                "kindReturnCode": kind_version.returncode,
                "kubectlReturnCode": kubectl_version.returncode,
            },
        )
    try:
        kubectl_payload = json.loads(kubectl_version.stdout)
    except json.JSONDecodeError:
        kubectl_payload = None
    client_version = (
        kubectl_payload.get("clientVersion") if isinstance(kubectl_payload, dict) else None
    )
    if not isinstance(client_version, dict) or not isinstance(client_version.get("gitVersion"), str):
        raise ReleaseGateError(
            "release.kubernetes_runtime_invalid",
            "kubectl client metadata was not valid JSON version evidence.",
        )
    return {
        "docker": {
            "serverVersion": docker_version.stdout.strip(),
            "platform": docker_platform.stdout.strip(),
            "socketPath": str(options.docker_socket_path),
        },
        "kind": {
            "binary": options.kind_bin,
            "version": kind_version.stdout.strip()[:500],
            "nodeImage": options.kind_node_image,
        },
        "kubectl": {"clientVersion": client_version["gitVersion"]},
    }


def target_configuration(options: KubernetesReleaseGateOptions) -> dict[str, Any]:
    return {
        "dockerSocketPath": str(options.docker_socket_path),
        "controlPlaneHost": options.kubernetes_control_plane_host,
        "kindBinary": options.kind_bin,
        "kindNodeImage": options.kind_node_image,
        "clusterLifecycle": "owned-disposable-per-child",
    }


def target_spec() -> remote.RemoteReleaseTargetSpec:
    return remote.RemoteReleaseTargetSpec(
        target="kubernetes",
        display_name="Kubernetes",
        schema_version=SCHEMA_VERSION,
        json_report_name=JSON_REPORT_NAME,
        markdown_report_name=MARKDOWN_REPORT_NAME,
        mode="real-provider-kubernetes-release-gate",
        worker_image_repository="synara-stage3-provider-kubernetes-release-gate",
        expected_unsupported=EXPECTED_UNSUPPORTED,
        cleanup_true_fields=("ownedClusterRemoved", "stateRemoved"),
        cleanup_false_fields=(
            "broadCleanupUsed",
            "ownedWorkerImageRemoved",
            "ownedCanaryImageRemoved",
            "reusedClusterResourcesRemoved",
        ),
        worker_image_evidence_path=("kubernetes", "containerEngine"),
        child_command=child_command,
        inspect_runtime=inspect_kubernetes_runtime,
        target_configuration=target_configuration,
        evidence_boundary=(
            "A pass closes the implemented real Codex/Claude disposable-Kind product and controlled-failure slice.",
            "It does not close SSH, production multi-node Kubernetes, registry rollout, concurrency, or soak gates.",
        ),
    )


def main(argv: Sequence[str] | None = None) -> int:
    options = parse_args(argv if argv is not None else sys.argv[1:])
    return remote.run_remote_release_gate(
        options,
        target_spec(),
        build_image=remote.build_gate_worker_image,
        cleanup_image=remote.cleanup_gate_worker_image,
        repository_state=common.repository_state,
        run_child=common.run_child_report,
    )


if __name__ == "__main__":
    raise SystemExit(main())
