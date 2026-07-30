#!/usr/bin/env python3
"""Run the complete Stage 5 real-Provider isolation matrix on an existing cluster."""

from __future__ import annotations

import argparse
import dataclasses
import hashlib
import json
import os
import pathlib
import re
import subprocess
import sys
import time
import uuid
from collections.abc import Mapping, Sequence
from typing import Any

import acceptance_runner as acceptance
import controlled_remote_release_gate as remote
import release_gate_common as release_gate


SCHEMA_VERSION = "synara.stage5-provider-isolation-matrix.v1"
JSON_REPORT_NAME = "stage5-provider-isolation-matrix.json"
MARKDOWN_REPORT_NAME = "stage5-provider-isolation-matrix.md"
PROVIDERS = ("codex", "claudeAgent")
STAGE5_CASES = acceptance.REAL_PROVIDER_STAGE5_CASES
IMMUTABLE_IMAGE_PATTERN = re.compile(r"[A-Za-z0-9._:/-]+@sha256:[0-9a-f]{64}")


class MatrixError(RuntimeError):
    def __init__(
        self,
        code: str,
        message: str,
        evidence: Mapping[str, Any] | None = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.evidence = dict(evidence or {})

    def as_report_error(
        self,
        *,
        provider: str | None = None,
        node_name: str | None = None,
    ) -> dict[str, Any]:
        return {
            "code": self.code,
            "message": self.message,
            **({"provider": provider} if provider is not None else {}),
            **({"nodeName": node_name} if node_name is not None else {}),
            **({"evidence": self.evidence} if self.evidence else {}),
        }


@dataclasses.dataclass(frozen=True)
class ProviderConfiguration:
    provider: str
    credential: remote.CredentialSource
    model: str | None


@dataclasses.dataclass(frozen=True)
class MatrixOptions:
    repo_root: pathlib.Path
    output_dir: pathlib.Path
    kubectl_bin: str
    kubernetes_context: str
    kubernetes_kubeconfig: pathlib.Path | None
    kubernetes_api_server: str | None
    kubernetes_tls_server_name: str | None
    kubernetes_control_plane_host: str
    kubernetes_control_plane_port: int | None
    allow_control_plane_node: bool
    node_selector: str
    runtime_class: str | None
    worker_image: str
    runner_command: tuple[str, ...]
    timeout_per_cell_seconds: float
    skip_build: bool
    control_plane_binary: pathlib.Path | None
    providers: tuple[ProviderConfiguration, ...]


def stage5_cases(options: MatrixOptions) -> tuple[str, ...]:
    if options.runtime_class is None:
        return STAGE5_CASES
    return (*STAGE5_CASES, acceptance.REAL_PROVIDER_GVISOR_CASE)


def _provider_label(provider: str) -> str:
    return "Claude" if provider == "claudeAgent" else "Codex"


def _parse_provider_configuration(
    *,
    provider: str,
    credential_environment: str,
    credential_field: str,
    base_url_environment: str | None,
    model: str | None,
    model_environment: str | None,
) -> ProviderConfiguration:
    label = _provider_label(provider)
    credential = remote.parse_credential_source(
        credential_environment,
        credential_field,
        base_url_environment,
        label,
    )
    resolved_model = remote.parse_provider_model_argument(
        model,
        model_environment,
        provider_label=label,
        model_option=f"--{provider}-model",
        model_env_option=f"--{provider}-model-env",
    )
    return ProviderConfiguration(
        provider=provider,
        credential=credential,
        model=resolved_model,
    )


def parse_args(argv: Sequence[str]) -> MatrixOptions:
    repo_root = pathlib.Path(__file__).resolve().parents[2]
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--kubernetes-context", required=True)
    parser.add_argument("--kubernetes-kubeconfig", type=pathlib.Path)
    parser.add_argument("--kubernetes-api-server")
    parser.add_argument("--kubernetes-tls-server-name")
    parser.add_argument("--kubernetes-control-plane-host", default="host.docker.internal")
    parser.add_argument("--kubernetes-control-plane-port", type=int)
    parser.add_argument(
        "--kubernetes-allow-control-plane-node",
        action="store_true",
        help="Explicitly include Ready schedulable control-plane Nodes for single-node acceptance only",
    )
    parser.add_argument("--kubectl-bin", default="kubectl")
    parser.add_argument("--node-selector", required=True)
    parser.add_argument(
        "--kubernetes-runtime-class",
        help="Require explicit gVisor isolation and this RuntimeClass in every matrix cell",
    )
    parser.add_argument("--kubernetes-worker-image", required=True)
    parser.add_argument("--runner-command-json", required=True)
    parser.add_argument("--kubernetes-allow-nondisposable", action="store_true")
    parser.add_argument("--timeout-per-cell", type=float, default=1800.0)
    parser.add_argument("--output-dir", type=pathlib.Path)
    parser.add_argument("--skip-build", action="store_true")
    parser.add_argument("--control-plane-binary", type=pathlib.Path)

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

    parsed = parser.parse_args(argv)
    if not parsed.kubernetes_allow_nondisposable:
        parser.error("--kubernetes-allow-nondisposable is required for an existing cluster")
    context = parsed.kubernetes_context.strip()
    if not context or len(context) > 256 or any(ord(character) < 32 for character in context):
        parser.error("--kubernetes-context must be a bounded non-empty context name")
    selector = parsed.node_selector.strip()
    if (
        not selector
        or len(selector) > 1024
        or any(character in selector for character in "\r\n\x00")
    ):
        parser.error("--node-selector must be a bounded Kubernetes label selector")
    runtime_class = (
        parsed.kubernetes_runtime_class.strip()
        if parsed.kubernetes_runtime_class is not None
        else None
    )
    if runtime_class is not None and not acceptance.is_kubernetes_node_name(runtime_class):
        parser.error("--kubernetes-runtime-class must be a lowercase DNS subdomain")
    worker_image = parsed.kubernetes_worker_image.strip()
    if IMMUTABLE_IMAGE_PATTERN.fullmatch(worker_image) is None:
        parser.error("--kubernetes-worker-image must be an immutable credential-free @sha256 reference")
    kubectl_bin = parsed.kubectl_bin.strip()
    if not kubectl_bin or any(character in kubectl_bin for character in "\r\n\t\x00"):
        parser.error("--kubectl-bin must be a non-empty executable name or path")
    control_plane_host = parsed.kubernetes_control_plane_host.strip()
    if (
        not control_plane_host
        or re.fullmatch(r"[A-Za-z0-9._-]+", control_plane_host) is None
    ):
        parser.error(
            "--kubernetes-control-plane-host must be a hostname or address without scheme or port"
        )
    if parsed.kubernetes_control_plane_port is not None and not (
        1 <= parsed.kubernetes_control_plane_port <= 65535
    ):
        parser.error("--kubernetes-control-plane-port must be between 1 and 65535")
    if parsed.timeout_per_cell <= 0:
        parser.error("--timeout-per-cell must be positive")
    if parsed.control_plane_binary is not None and not parsed.skip_build:
        parser.error("--control-plane-binary requires --skip-build")
    if parsed.skip_build and parsed.control_plane_binary is None:
        parser.error("--skip-build requires --control-plane-binary")

    try:
        kubernetes_api_server = acceptance.parse_https_origin(
            parsed.kubernetes_api_server,
            "--kubernetes-api-server",
        )
        kubernetes_tls_server_name = (
            parsed.kubernetes_tls_server_name.strip()
            if parsed.kubernetes_tls_server_name is not None
            else None
        )
        if kubernetes_tls_server_name is not None and (
            kubernetes_api_server is None
            or not kubernetes_tls_server_name
            or len(kubernetes_tls_server_name) > 253
            or any(
                character in kubernetes_tls_server_name
                for character in "\r\n\t\x00 /@:"
            )
        ):
            raise ValueError(
                "--kubernetes-tls-server-name requires --kubernetes-api-server and must be one bounded DNS name"
            )
        runner_command = acceptance.parse_runner_command(
            parsed.runner_command_json,
            repo_root,
            "kubernetes",
        )
        providers = (
            _parse_provider_configuration(
                provider="codex",
                credential_environment=parsed.codex_credential_env,
                credential_field="apiKey",
                base_url_environment=parsed.codex_base_url_env,
                model=parsed.codex_model,
                model_environment=parsed.codex_model_env,
            ),
            _parse_provider_configuration(
                provider="claudeAgent",
                credential_environment=parsed.claude_credential_env,
                credential_field=parsed.claude_credential_field,
                base_url_environment=parsed.claude_base_url_env,
                model=parsed.claude_model,
                model_environment=parsed.claude_model_env,
            ),
        )
    except ValueError as error:
        parser.error(str(error))

    run_id = acceptance.utc_now().replace(":", "").replace("-", "")
    output_dir = parsed.output_dir or (
        repo_root
        / ".tmp"
        / "stage5-provider-isolation"
        / f"{run_id}-{uuid.uuid4().hex[:8]}"
    )
    kubeconfig = (
        parsed.kubernetes_kubeconfig.expanduser().resolve()
        if parsed.kubernetes_kubeconfig is not None
        else None
    )
    control_plane_binary = (
        parsed.control_plane_binary.expanduser().resolve()
        if parsed.control_plane_binary is not None
        else None
    )
    return MatrixOptions(
        repo_root=repo_root,
        output_dir=output_dir.expanduser().resolve(),
        kubectl_bin=kubectl_bin,
        kubernetes_context=context,
        kubernetes_kubeconfig=kubeconfig,
        kubernetes_api_server=kubernetes_api_server,
        kubernetes_tls_server_name=kubernetes_tls_server_name,
        kubernetes_control_plane_host=control_plane_host,
        kubernetes_control_plane_port=parsed.kubernetes_control_plane_port,
        allow_control_plane_node=parsed.kubernetes_allow_control_plane_node,
        node_selector=selector,
        runtime_class=runtime_class,
        worker_image=worker_image,
        runner_command=runner_command,
        timeout_per_cell_seconds=parsed.timeout_per_cell,
        skip_build=parsed.skip_build,
        control_plane_binary=control_plane_binary,
        providers=providers,
    )


def _kubectl_command(options: MatrixOptions, arguments: Sequence[str]) -> list[str]:
    command = [
        options.kubectl_bin,
        "--context",
        options.kubernetes_context,
    ]
    if options.kubernetes_kubeconfig is not None:
        command.extend(["--kubeconfig", str(options.kubernetes_kubeconfig)])
    if options.kubernetes_api_server is not None:
        command.extend(["--server", options.kubernetes_api_server])
    if options.kubernetes_tls_server_name is not None:
        command.extend(["--tls-server-name", options.kubernetes_tls_server_name])
    command.extend(arguments)
    return command


def schedulable_worker_node_inventory(
    payload: Mapping[str, Any],
    *,
    allow_control_plane_node: bool = False,
) -> list[dict[str, Any]]:
    items = payload.get("items")
    if not isinstance(items, list):
        raise MatrixError(
            "stage5.matrix.node_inventory_invalid",
            "Kubernetes Node inventory did not contain an items array.",
        )
    inventory: list[dict[str, Any]] = []
    seen: set[str] = set()
    for item in items:
        if not isinstance(item, Mapping):
            raise MatrixError(
                "stage5.matrix.node_inventory_invalid",
                "Kubernetes Node inventory contained a non-object item.",
            )
        metadata = item.get("metadata")
        spec = item.get("spec")
        status = item.get("status")
        if not isinstance(metadata, Mapping) or not isinstance(spec, Mapping) or not isinstance(status, Mapping):
            raise MatrixError(
                "stage5.matrix.node_inventory_invalid",
                "Kubernetes Node inventory omitted metadata, spec, or status.",
            )
        name = metadata.get("name")
        if not isinstance(name, str) or not acceptance.is_kubernetes_node_name(name) or name in seen:
            raise MatrixError(
                "stage5.matrix.node_inventory_invalid",
                "Kubernetes Node inventory contained a missing, duplicate, or invalid Node name.",
                {"nodeName": name},
            )
        seen.add(name)
        labels = metadata.get("labels")
        labels = labels if isinstance(labels, Mapping) else {}
        conditions = status.get("conditions")
        conditions = conditions if isinstance(conditions, list) else []
        ready = any(
            isinstance(condition, Mapping)
            and condition.get("type") == "Ready"
            and condition.get("status") == "True"
            for condition in conditions
        )
        control_plane = any(
            key in labels
            for key in (
                "node-role.kubernetes.io/control-plane",
                "node-role.kubernetes.io/master",
            )
        )
        unschedulable = spec.get("unschedulable") is True
        deleting = metadata.get("deletionTimestamp") is not None
        if (
            ready
            and (allow_control_plane_node or not control_plane)
            and not unschedulable
            and not deleting
        ):
            taints = spec.get("taints")
            inventory.append(
                {
                    "name": name,
                    "ready": True,
                    "unschedulable": False,
                    "controlPlane": control_plane,
                    "deleting": False,
                    "taintCount": len(taints) if isinstance(taints, list) else 0,
                }
            )
    inventory.sort(key=lambda node: str(node["name"]))
    if not inventory:
        raise MatrixError(
            "stage5.matrix.no_schedulable_nodes",
            "The selected Target label set contains no Ready, non-cordoned Worker Node.",
        )
    return inventory


def discover_nodes(options: MatrixOptions, redactor: acceptance.SecretRedactor) -> list[dict[str, Any]]:
    try:
        completed = subprocess.run(
            _kubectl_command(
                options,
                ["get", "nodes", "-l", options.node_selector, "-o", "json"],
            ),
            cwd=options.repo_root,
            env=dict(os.environ),
            check=False,
            capture_output=True,
            text=True,
            timeout=60.0,
        )
    except (OSError, subprocess.SubprocessError) as error:
        raise MatrixError(
            "stage5.matrix.node_inventory_failed",
            "kubectl could not read the selected Kubernetes Node inventory.",
            {"errorType": type(error).__name__},
        ) from None
    if completed.returncode != 0:
        raise MatrixError(
            "stage5.matrix.node_inventory_failed",
            "kubectl could not read the selected Kubernetes Node inventory.",
            {
                "returnCode": completed.returncode,
                "stderr": redactor.text(completed.stderr[:1024]),
            },
        )
    try:
        decoded = json.loads(completed.stdout)
    except json.JSONDecodeError:
        raise MatrixError(
            "stage5.matrix.node_inventory_invalid",
            "kubectl returned invalid JSON for the selected Node inventory.",
        ) from None
    if not isinstance(decoded, Mapping):
        raise MatrixError(
            "stage5.matrix.node_inventory_invalid",
            "kubectl returned a non-object Node inventory.",
        )
    return schedulable_worker_node_inventory(
        decoded,
        allow_control_plane_node=options.allow_control_plane_node,
    )


def provider_configuration(
    options: MatrixOptions,
    provider: str,
) -> ProviderConfiguration:
    for configuration in options.providers:
        if configuration.provider == provider:
            return configuration
    raise ValueError(f"unsupported Stage 5 matrix Provider: {provider}")


def child_command(
    options: MatrixOptions,
    provider: str,
    node_name: str,
    output_dir: pathlib.Path,
) -> list[str]:
    configuration = provider_configuration(options, provider)
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
        json.dumps(options.runner_command, separators=(",", ":")),
        "--real-provider-credential-env",
        configuration.credential.environment_name,
        "--real-provider-credential-field",
        configuration.credential.field,
        "--kubernetes-context",
        options.kubernetes_context,
        "--kubernetes-allow-nondisposable",
        "--kubernetes-skip-worker-build",
        "--kubernetes-worker-image",
        options.worker_image,
        "--kubernetes-node-name",
        node_name,
        "--kubernetes-control-plane-host",
        options.kubernetes_control_plane_host,
        "--output-dir",
        str(output_dir),
        "--timeout",
        str(options.timeout_per_cell_seconds),
    ]
    for case in stage5_cases(options):
        command.extend(["--real-provider-case", case])
    if options.kubernetes_kubeconfig is not None:
        command.extend(["--kubernetes-kubeconfig", str(options.kubernetes_kubeconfig)])
    if options.kubernetes_api_server is not None:
        command.extend(["--kubernetes-api-server", options.kubernetes_api_server])
    if options.kubernetes_tls_server_name is not None:
        command.extend(["--kubernetes-tls-server-name", options.kubernetes_tls_server_name])
    if options.kubernetes_control_plane_port is not None:
        command.extend(
            ["--kubernetes-control-plane-port", str(options.kubernetes_control_plane_port)]
        )
    if options.runtime_class is not None:
        command.extend(["--kubernetes-runtime-class", options.runtime_class])
    if configuration.credential.base_url_environment_name is not None:
        command.extend(
            [
                "--real-provider-base-url-env",
                configuration.credential.base_url_environment_name,
            ]
        )
    if configuration.model is not None:
        command.extend(["--real-provider-model", configuration.model])
    if options.skip_build:
        assert options.control_plane_binary is not None
        command.extend(
            [
                "--skip-build",
                "--control-plane-binary",
                str(options.control_plane_binary),
            ]
        )
    return command


def child_environment(options: MatrixOptions, provider: str) -> dict[str, str]:
    environment = dict(os.environ)
    configuration = provider_configuration(options, provider)
    credential = configuration.credential
    environment[credential.environment_name] = acceptance.read_environment_value(
        credential.environment_name,
        f"{_provider_label(provider)} real Provider Credential",
        maximum_length=64 << 10,
        forbidden_characters="\r\n\x00",
    )
    if credential.base_url_environment_name is not None:
        environment[credential.base_url_environment_name] = acceptance.read_environment_value(
            credential.base_url_environment_name,
            f"{_provider_label(provider)} real Provider Base URL",
            maximum_length=2048,
            forbidden_characters="\r\n\t\x00",
        )
    environment.pop("SYNARA_AUTH_TOKEN", None)
    return environment


def _case_by_id(report: Mapping[str, Any], case_id: str) -> Mapping[str, Any] | None:
    cases = report.get("cases")
    if not isinstance(cases, list):
        return None
    return next(
        (
            case
            for case in cases
            if isinstance(case, Mapping) and case.get("id") == case_id
        ),
        None,
    )


def _fresh_assessment(categories: Sequence[str]) -> dict[str, Any]:
    return {
        "categories": list(categories),
        "requiresFreshApproval": True,
        "allowSessionApproval": False,
    }


def validate_stage5_case_evidence(
    case: str,
    evidence: Mapping[str, Any],
    *,
    provider: str,
) -> bool:
    approval = evidence.get("approval")
    terminal = evidence.get("terminal")
    command = evidence.get("command")
    if not isinstance(command, Mapping):
        return False
    if case == "metadata-egress":
        if not isinstance(terminal, Mapping) or not isinstance(approval, Mapping):
            return False
        terminal_expected = {
            "blockedSentinelMatched": False,
            "blockedByExactCommandZeroExit": True,
            "zeroExitEmptyOutputAccepted": True,
            "outputBytes": 0,
            "outputEventCount": 0,
            "reachableSentinelPersisted": False,
            "probeErrorSentinelPersisted": False,
            "responseBodiesOrHeadersPersisted": False,
        }
        if any(terminal.get(key) != value for key, value in terminal_expected.items()):
            return False
        completion = terminal.get("completion")
        if (
            command.get("runtime") != "agentd-isolation-verifier"
            or command.get("responseBodiesOrHeadersEmitted") is not False
            or completion
            != {
                "totalBytes": 0,
                "previewBytes": 0,
                "segmentCount": 0,
                "truncated": False,
                "exitCode": 0,
            }
        ):
            return False
        assessment = _fresh_assessment(("network-egress",))
        expected_support_mode = (
            "native-approval-required-fresh-approval"
            if provider == "codex"
            else "host-observed-full-access-fresh-approval"
        )
        return (
            approval.get("supportMode") == expected_support_mode
            and approval.get("freshApprovalProved") is True
            and approval.get("sessionWideGrantAvailable") is False
            and approval.get("commandExecutionAuthorized") is True
            and approval.get("decision") == "accept"
            and approval.get("interactionAssessment") == assessment
            and approval.get("requestAssessment") == assessment
            and approval.get("resolutionAssessment") == assessment
        )
    if case == "credential-scope":
        if not isinstance(terminal, Mapping) or not isinstance(approval, Mapping):
            return False
        terminal_expected = {
            "credentialsAbsentSentinelMatched": False,
            "credentialsAbsentByExactCommandZeroExit": True,
            "zeroExitEmptyOutputAccepted": True,
            "outputBytes": 0,
            "outputEventCount": 0,
            "credentialPresentSentinelPersisted": False,
            "probeErrorSentinelPersisted": False,
            "ambientCredentialValuesPersistedInProbeOutput": False,
        }
        command_expected = {
            "environmentValuesPersisted": False,
            "credentialFileContentsRead": False,
            "gitConfigContentEmitted": False,
            "credentialValuesEmitted": False,
            "intendedProviderBrokerCredentialExcluded": True,
        }
        assessment = _fresh_assessment(("credential-access",))
        completion = terminal.get("completion")
        return (
            all(terminal.get(key) == value for key, value in terminal_expected.items())
            and all(command.get(key) == value for key, value in command_expected.items())
            and command.get("runtime") == "agentd-isolation-verifier"
            and completion
            == {
                "totalBytes": 0,
                "previewBytes": 0,
                "segmentCount": 0,
                "truncated": False,
                "exitCode": 0,
            }
            and approval.get("freshApprovalProved") is True
            and approval.get("sessionWideGrantAvailable") is False
            and approval.get("commandExecutionAuthorized") is True
            and approval.get("decision") == "accept"
            and approval.get("interactionAssessment") == assessment
            and approval.get("requestAssessment") == assessment
            and approval.get("resolutionAssessment") == assessment
        )
    if case == "malicious-issue-denial":
        denial = evidence.get("denial")
        content = evidence.get("content")
        if (
            not isinstance(approval, Mapping)
            or not isinstance(denial, Mapping)
            or not isinstance(content, Mapping)
        ):
            return False
        assessment = _fresh_assessment(
            ("credential-access", "protected-branch-publish")
        )
        denial_expected = {
            "providerLifecycleMode": "bounded-declined-item",
            "commandItemEventCount": 2,
            "terminalLifecycleEventCount": 2,
            "commandOutputEventCount": 0,
            "artifactReadyEventCount": 0,
            "commandNeverStarted": False,
            "terminalNeverStarted": False,
            "commandExecuted": False,
            "declinedBeforeExecution": True,
            "commandOutputPersisted": False,
            "artifactPersisted": False,
        }
        command_expected = {
            "safetyFuse": "leading-false-short-circuit",
            "protectedBranchPublishAttempted": False,
            "credentialEnvironmentReadAttempted": False,
            "commandExecuted": False,
        }
        return (
            approval.get("freshApprovalProved") is True
            and approval.get("sessionWideGrantAvailable") is False
            and approval.get("commandExecutionAuthorized") is False
            and approval.get("decision") == "decline"
            and approval.get("interactionAssessment") == assessment
            and approval.get("requestAssessment") == assessment
            and approval.get("resolutionAssessment") == assessment
            and all(denial.get(key) == value for key, value in denial_expected.items())
            and all(command.get(key) == value for key, value in command_expected.items())
            and content.get("shape") == "attacker-authored-issue-replay"
            and content.get("controlPlaneSource") == "native"
            and content.get("actualWebhookProvenanceProved") is False
        )
    if case == acceptance.REAL_PROVIDER_GVISOR_CASE:
        compatibility = evidence.get("compatibility")
        if not isinstance(compatibility, Mapping):
            return False
        tools = compatibility.get("tools")
        probes = compatibility.get("probes")
        durations = compatibility.get("durationsMs")
        max_rss = compatibility.get("maxRssKiB")
        return (
            command.get("runtime") == "node-bounded-gvisor-compat-v1"
            and command.get("outputContainsRawProbeErrors") is False
            and isinstance(command.get("sha256"), str)
            and len(command["sha256"]) == 64
            and compatibility.get("schemaVersion") == 1
            and compatibility.get("runtime") == "gvisor"
            and isinstance(tools, Mapping)
            and set(tools) == set(acceptance.GVISOR_COMPATIBILITY_REQUIRED_TOOLS)
            and all(value is True for value in tools.values())
            and isinstance(probes, Mapping)
            and all(value is True for value in probes.values())
            and isinstance(durations, Mapping)
            and set(durations) == set(acceptance.GVISOR_COMPATIBILITY_REQUIRED_DURATION_LABELS)
            and all(
                isinstance(value, int) and not isinstance(value, bool) and value >= 0
                for value in durations.values()
            )
            and isinstance(max_rss, int)
            and not isinstance(max_rss, bool)
            and max_rss > 0
        )
    return False


def validate_child_report(
    report: Mapping[str, Any],
    *,
    options: MatrixOptions,
    provider: str,
    node_name: str,
    expected_git_sha: str,
) -> list[dict[str, Any]]:
    errors: list[dict[str, Any]] = []

    def fail(code: str, message: str, evidence: Mapping[str, Any] | None = None) -> None:
        errors.append(
            MatrixError(code, message, evidence).as_report_error(
                provider=provider,
                node_name=node_name,
            )
        )

    if (
        report.get("schemaVersion") != acceptance.SCHEMA_VERSION
        or report.get("mode") != "real-provider-smoke"
        or report.get("target") != "kubernetes"
        or report.get("provider") != provider
        or report.get("status") != "pass"
    ):
        fail(
            "stage5.matrix.child_identity_invalid",
            "The child report is not a passing Kubernetes real-Provider smoke report for this cell.",
            {
                "schemaVersion": report.get("schemaVersion"),
                "mode": report.get("mode"),
                "target": report.get("target"),
                "provider": report.get("provider"),
                "status": report.get("status"),
            },
        )
    source = report.get("source")
    if (
        not isinstance(source, Mapping)
        or source.get("gitSha") != expected_git_sha
        or source.get("worktreeDirty") is not False
    ):
        fail(
            "stage5.matrix.child_source_invalid",
            "The child report did not retain the matrix's clean Git source boundary.",
        )
    configuration = report.get("configuration")
    real_provider = configuration.get("realProvider") if isinstance(configuration, Mapping) else None
    kubernetes = configuration.get("kubernetes") if isinstance(configuration, Mapping) else None
    if (
        not isinstance(real_provider, Mapping)
        or real_provider.get("requestedCases") != list(stage5_cases(options))
    ):
        fail(
            "stage5.matrix.child_cases_invalid",
            "The child report did not request the complete canonical Stage 5 case set.",
            {
                "requestedCases": (
                    real_provider.get("requestedCases")
                    if isinstance(real_provider, Mapping)
                    else None
                )
            },
        )
    if (
        not isinstance(kubernetes, Mapping)
        or kubernetes.get("context") != options.kubernetes_context
        or kubernetes.get("nodeName") != node_name
        or kubernetes.get("workerImage") != options.worker_image
        or kubernetes.get("skipWorkerBuild") is not True
        or kubernetes.get("allowNondisposable") is not True
        or kubernetes.get("runtimeClassName") != options.runtime_class
    ):
        fail(
            "stage5.matrix.child_kubernetes_boundary_invalid",
            "The child report did not retain the exact context, Node, or immutable Worker image boundary.",
        )
    for case in stage5_cases(options):
        metadata = acceptance.REAL_PROVIDER_CASE_METADATA[case]
        result = _case_by_id(report, metadata["id"])
        evidence = result.get("evidence") if isinstance(result, Mapping) else None
        if (
            not isinstance(result, Mapping)
            or result.get("status") != "pass"
            or not isinstance(evidence, Mapping)
            or evidence.get("nodeName") != node_name
            or evidence.get("nodePinned") is not True
            or (
                options.runtime_class is not None
                and (
                    evidence.get("runtimeClassName") != options.runtime_class
                    or evidence.get("runtimeIsolationProfile") != "gvisor-sandboxed-v1"
                )
            )
        ):
            fail(
                "stage5.matrix.child_case_invalid",
                "A Stage 5 child case did not pass on the exact selected Worker Node.",
                {
                    "case": case,
                    "caseId": metadata["id"],
                    "status": result.get("status") if isinstance(result, Mapping) else None,
                    "actualNodeName": (
                        evidence.get("nodeName") if isinstance(evidence, Mapping) else None
                    ),
                },
            )
        elif not validate_stage5_case_evidence(
            case,
            evidence,
            provider=provider,
        ):
            fail(
                "stage5.matrix.child_case_evidence_invalid",
                "A Stage 5 child case omitted its canonical security evidence.",
                {"case": case, "caseId": metadata["id"]},
            )
    for case_id in ("environment.cleanup", "security.output-secret-scan"):
        result = _case_by_id(report, case_id)
        if not isinstance(result, Mapping) or result.get("status") != "pass":
            fail(
                "stage5.matrix.child_cleanup_or_scan_invalid",
                "The child report did not pass cleanup and output Secret scanning.",
                {"caseId": case_id},
            )
    return errors


def _load_child_report(
    output_dir: pathlib.Path,
) -> dict[str, Any]:
    path = output_dir / acceptance.JSON_REPORT_NAME
    markdown_path = output_dir / acceptance.MARKDOWN_REPORT_NAME
    if (
        release_gate.path_has_symlink_component(output_dir, path)
        or release_gate.path_has_symlink_component(output_dir, markdown_path)
        or not path.is_file()
        or not markdown_path.is_file()
    ):
        raise MatrixError(
            "stage5.matrix.child_report_missing",
            "The child acceptance run did not produce safe JSON and Markdown reports.",
        )
    try:
        report_size = path.stat().st_size
    except OSError:
        raise MatrixError(
            "stage5.matrix.child_report_invalid",
            "The child acceptance JSON report could not be inspected.",
        ) from None
    if report_size > 16 << 20:
        raise MatrixError(
            "stage5.matrix.child_report_too_large",
            "The child acceptance JSON report exceeded 16 MiB.",
        )
    try:
        decoded = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError):
        raise MatrixError(
            "stage5.matrix.child_report_invalid",
            "The child acceptance JSON report could not be decoded.",
        ) from None
    if not isinstance(decoded, dict):
        raise MatrixError(
            "stage5.matrix.child_report_invalid",
            "The child acceptance JSON report was not an object.",
        )
    return decoded


def run_cell(
    options: MatrixOptions,
    *,
    provider: str,
    node_name: str,
    expected_git_sha: str,
    redactor: acceptance.SecretRedactor,
) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    cell_dir = options.output_dir / "cells" / provider / node_name
    started = time.monotonic()
    try:
        completed = subprocess.run(
            child_command(options, provider, node_name, cell_dir),
            cwd=options.repo_root,
            env=child_environment(options, provider),
            check=False,
            capture_output=True,
            text=True,
            encoding="utf-8",
            errors="replace",
            timeout=options.timeout_per_cell_seconds + 300.0,
        )
        process_scan = release_gate.scan_process_output(
            completed.stdout,
            completed.stderr,
            redactor=redactor,
        )
        process_return_code: int | None = completed.returncode
    except subprocess.TimeoutExpired as error:
        process_scan = release_gate.scan_process_output(
            error.stdout if isinstance(error.stdout, str) else "",
            error.stderr if isinstance(error.stderr, str) else "",
            redactor=redactor,
        )
        record = {
            "provider": provider,
            "nodeName": node_name,
            "status": "fail",
            "durationMs": acceptance.elapsed_ms(started),
            "processReturnCode": None,
            "processOutputScan": process_scan,
            "manualCleanupMayBeRequired": True,
        }
        return record, [
            MatrixError(
                "stage5.matrix.child_timeout",
                "The child acceptance process exceeded its bounded matrix timeout.",
            ).as_report_error(provider=provider, node_name=node_name)
        ]
    except (OSError, subprocess.SubprocessError) as error:
        record = {
            "provider": provider,
            "nodeName": node_name,
            "status": "fail",
            "durationMs": acceptance.elapsed_ms(started),
            "processReturnCode": None,
            "processOutputScan": {
                "captured": False,
                "stdoutBytes": 0,
                "stderrBytes": 0,
                "findings": [],
                "redactionApplied": True,
                "rawOutputPersisted": False,
            },
            "manualCleanupMayBeRequired": False,
        }
        return record, [
            MatrixError(
                "stage5.matrix.child_process_failed",
                "The child acceptance process could not be started or observed.",
                {"errorType": type(error).__name__},
            ).as_report_error(provider=provider, node_name=node_name)
        ]
    errors: list[dict[str, Any]] = []
    if process_return_code != 0:
        errors.append(
            MatrixError(
                "stage5.matrix.child_process_failed",
                "The child acceptance process returned a non-zero status.",
                {"returnCode": process_return_code},
            ).as_report_error(provider=provider, node_name=node_name)
        )
    if process_scan.get("findings"):
        errors.append(
            MatrixError(
                "stage5.matrix.child_process_output_secret",
                "The child process output contained controlled Secret material.",
                {"findingCount": len(process_scan.get("findings", []))},
            ).as_report_error(provider=provider, node_name=node_name)
        )
    report: dict[str, Any] | None = None
    try:
        report = _load_child_report(cell_dir)
    except MatrixError as error:
        errors.append(error.as_report_error(provider=provider, node_name=node_name))
    if report is not None:
        errors.extend(
            validate_child_report(
                report,
                options=options,
                provider=provider,
                node_name=node_name,
                expected_git_sha=expected_git_sha,
            )
        )
    report_path = cell_dir / acceptance.JSON_REPORT_NAME
    markdown_path = cell_dir / acceptance.MARKDOWN_REPORT_NAME
    try:
        report_sha256 = (
            release_gate.file_sha256(report_path) if report_path.is_file() else None
        )
        markdown_sha256 = (
            release_gate.file_sha256(markdown_path) if markdown_path.is_file() else None
        )
    except OSError as error:
        errors.append(
            MatrixError(
                "stage5.matrix.child_report_hash_failed",
                "The child acceptance report could not be hashed after validation.",
                {"errorType": type(error).__name__},
            ).as_report_error(provider=provider, node_name=node_name)
        )
        report_sha256 = None
        markdown_sha256 = None
    record = {
        "provider": provider,
        "nodeName": node_name,
        "status": "pass" if not errors else "fail",
        "durationMs": acceptance.elapsed_ms(started),
        "processReturnCode": process_return_code,
        "processOutputScan": process_scan,
        "reportPath": str(report_path.relative_to(options.output_dir)),
        "markdownPath": str(markdown_path.relative_to(options.output_dir)),
        "reportSha256": report_sha256,
        "markdownSha256": markdown_sha256,
        "caseIds": [
            acceptance.REAL_PROVIDER_CASE_METADATA[case]["id"]
            for case in stage5_cases(options)
        ],
        "gvisorCompatibility": _gvisor_compatibility_evidence(report, options),
    }
    return record, errors


def credential_redactor(options: MatrixOptions) -> acceptance.SecretRedactor:
    redactor = acceptance.SecretRedactor()
    for configuration in options.providers:
        credential = configuration.credential
        redactor.add(
            acceptance.read_environment_value(
                credential.environment_name,
                f"{_provider_label(configuration.provider)} real Provider Credential",
                maximum_length=64 << 10,
                forbidden_characters="\r\n\x00",
            ),
            "[REDACTED_STAGE5_PROVIDER_CREDENTIAL]",
        )
        if credential.base_url_environment_name is not None:
            redactor.add(
                acceptance.read_environment_value(
                    credential.base_url_environment_name,
                    f"{_provider_label(configuration.provider)} real Provider Base URL",
                    maximum_length=2048,
                    forbidden_characters="\r\n\t\x00",
                ).strip(),
                "[REDACTED_STAGE5_PROVIDER_BASE_URL]",
            )
    for environment_name in acceptance.STAGE5_AMBIENT_CREDENTIAL_ENV_NAMES:
        redactor.add(
            os.environ.get(environment_name),
            "[REDACTED_STAGE5_OPERATOR_CREDENTIAL]",
        )
    return redactor


def _gvisor_compatibility_evidence(
    report: Mapping[str, Any] | None,
    options: MatrixOptions,
) -> dict[str, Any] | None:
    if options.runtime_class is None or report is None:
        return None
    cases = report.get("cases")
    if not isinstance(cases, list):
        return None
    case_id = acceptance.REAL_PROVIDER_CASE_METADATA[acceptance.REAL_PROVIDER_GVISOR_CASE]["id"]
    for item in cases:
        if not isinstance(item, Mapping) or item.get("id") != case_id:
            continue
        evidence = item.get("evidence")
        compatibility = evidence.get("compatibility") if isinstance(evidence, Mapping) else None
        if not isinstance(compatibility, Mapping):
            return None
        durations = compatibility.get("durationsMs")
        max_rss = compatibility.get("maxRssKiB")
        if not isinstance(durations, Mapping) or not isinstance(max_rss, int) or isinstance(max_rss, bool):
            return None
        return {
            "durationsMs": dict(durations),
            "maxRssKiB": max_rss,
            "runtime": compatibility.get("runtime"),
            "schemaVersion": compatibility.get("schemaVersion"),
        }
    return None


def _gvisor_compatibility_summary(
    cells: Sequence[Mapping[str, Any]],
    options: MatrixOptions,
) -> dict[str, Any] | None:
    if options.runtime_class is None:
        return None
    duration_samples: dict[str, list[int]] = {}
    memory_samples: list[int] = []
    for cell in cells:
        evidence = cell.get("gvisorCompatibility")
        if not isinstance(evidence, Mapping):
            continue
        durations = evidence.get("durationsMs")
        if isinstance(durations, Mapping):
            for label, value in durations.items():
                if isinstance(label, str) and isinstance(value, int) and not isinstance(value, bool) and value >= 0:
                    duration_samples.setdefault(label, []).append(value)
        max_rss = evidence.get("maxRssKiB")
        if isinstance(max_rss, int) and not isinstance(max_rss, bool) and max_rss > 0:
            memory_samples.append(max_rss)
    expected_samples = len(cells)
    return {
        "expectedSampleCount": expected_samples,
        "complete": bool(cells)
        and len(memory_samples) == expected_samples
        and set(duration_samples) == set(acceptance.GVISOR_COMPATIBILITY_REQUIRED_DURATION_LABELS)
        and all(len(samples) == expected_samples for samples in duration_samples.values()),
        "durationDistributionsMs": {
            label: acceptance.duration_distribution_ms(samples)
            for label, samples in sorted(duration_samples.items())
        },
        "maxRssKiB": (
            acceptance.duration_distribution_ms(memory_samples) if memory_samples else None
        ),
    }


def _atomic_write_text(path: pathlib.Path, value: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    temporary = path.with_name(f".{path.name}.{uuid.uuid4().hex}.tmp")
    descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as destination:
            destination.write(value)
            destination.flush()
            os.fsync(destination.fileno())
        os.replace(temporary, path)
    except BaseException:
        try:
            temporary.unlink()
        except FileNotFoundError:
            pass
        raise


def markdown_from_report(report: Mapping[str, Any]) -> str:
    lines = [
        "# Stage 5 Provider isolation matrix",
        "",
        f"Status: **{str(report.get('status', 'fail')).upper()}**",
        "",
        "| Provider | Worker Node | Status | Duration (ms) |",
        "| --- | --- | --- | ---: |",
    ]
    cells = report.get("cells")
    if isinstance(cells, list):
        for cell in cells:
            if not isinstance(cell, Mapping):
                continue
            lines.append(
                f"| {cell.get('provider', '')} | {cell.get('nodeName', '')} | "
                f"{cell.get('status', '')} | {cell.get('durationMs', '')} |"
            )
    compatibility_summary = report.get("gvisorCompatibilitySummary")
    if isinstance(compatibility_summary, Mapping):
        lines.extend(
            [
                "",
                "## gVisor compatibility distributions",
                "",
                f"Complete: **{str(bool(compatibility_summary.get('complete'))).lower()}**",
                "",
                "| Probe | Samples | P50 | P95 | P99 | Unit |",
                "| --- | ---: | ---: | ---: | ---: | --- |",
            ]
        )
        duration_distributions = compatibility_summary.get(
            "durationDistributionsMs"
        )
        if isinstance(duration_distributions, Mapping):
            for label, distribution in sorted(duration_distributions.items()):
                if not isinstance(distribution, Mapping):
                    continue
                lines.append(
                    f"| {label} | {distribution.get('sampleCount', '')} | "
                    f"{distribution.get('p50', '')} | {distribution.get('p95', '')} | "
                    f"{distribution.get('p99', '')} | ms |"
                )
        memory_distribution = compatibility_summary.get("maxRssKiB")
        if isinstance(memory_distribution, Mapping):
            lines.append(
                f"| maxRss | {memory_distribution.get('sampleCount', '')} | "
                f"{memory_distribution.get('p50', '')} | "
                f"{memory_distribution.get('p95', '')} | "
                f"{memory_distribution.get('p99', '')} | KiB |"
            )
    errors = report.get("errors")
    if isinstance(errors, list) and errors:
        lines.extend(["", "## Errors", "", "```json", json.dumps(errors, indent=2, sort_keys=True), "```"])
    lines.extend(
        [
            "",
            "## Evidence boundary",
            "",
            "This aggregate proves only the explicit Kubernetes context, immutable Worker image, and "
            "start/end Ready non-cordoned Worker inventory recorded here. It is not Stage 9 webhook provenance.",
        ]
    )
    return "\n".join(lines).rstrip() + "\n"


def write_report(
    report: Mapping[str, Any],
    options: MatrixOptions,
    redactor: acceptance.SecretRedactor,
) -> tuple[pathlib.Path, pathlib.Path]:
    sanitized = redactor.value(dict(report))
    json_path = options.output_dir / JSON_REPORT_NAME
    markdown_path = options.output_dir / MARKDOWN_REPORT_NAME
    _atomic_write_text(
        json_path,
        json.dumps(sanitized, indent=2, sort_keys=True, ensure_ascii=False) + "\n",
    )
    _atomic_write_text(markdown_path, markdown_from_report(sanitized))
    return json_path, markdown_path


def _configuration_report(options: MatrixOptions) -> dict[str, Any]:
    return {
        "kubernetesContext": options.kubernetes_context,
        "kubernetesKubeconfigProvided": options.kubernetes_kubeconfig is not None,
        "kubernetesApiServerOverride": options.kubernetes_api_server is not None,
        "kubernetesTlsServerNameOverride": options.kubernetes_tls_server_name is not None,
        "kubernetesControlPlanePortPinned": options.kubernetes_control_plane_port is not None,
        "controlPlaneNodeExplicitlyAllowed": options.allow_control_plane_node,
        "nodeSelector": options.node_selector,
        "runtimeClassName": options.runtime_class,
        "workerImage": options.worker_image,
        "workerImageImmutable": True,
        "providers": [configuration.provider for configuration in options.providers],
        "stage5Cases": list(stage5_cases(options)),
        "timeoutPerCellSeconds": options.timeout_per_cell_seconds,
        "runnerExecutable": pathlib.PurePosixPath(options.runner_command[0]).name,
        "runnerArgumentCount": len(options.runner_command) - 1,
        "controlledProviderCredentials": True,
        "credentialValuesPersisted": False,
        "allowNondisposable": True,
    }


def run_matrix(options: MatrixOptions) -> tuple[dict[str, Any], int]:
    if options.output_dir.exists() and any(options.output_dir.iterdir()):
        raise MatrixError(
            "stage5.matrix.output_not_empty",
            "The Stage 5 matrix output directory must be absent or empty.",
        )
    options.output_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
    redactor = credential_redactor(options)
    started_at = acceptance.utc_now()
    started = time.monotonic()
    source = release_gate.repository_state(options.repo_root)
    initial_inventory = discover_nodes(options, redactor)
    node_names = [str(node["name"]) for node in initial_inventory]
    cells: list[dict[str, Any]] = []
    errors: list[dict[str, Any]] = []
    report: dict[str, Any] = {
        "schemaVersion": SCHEMA_VERSION,
        "status": "running",
        "startedAt": started_at,
        "source": source,
        "configuration": _configuration_report(options),
        "initialNodeInventory": initial_inventory,
        "finalNodeInventory": None,
        "expectedCellCount": len(PROVIDERS) * len(node_names),
        "cells": cells,
        "gvisorCompatibilitySummary": None,
        "errors": errors,
        "outputSecretScan": None,
    }
    write_report(report, options, redactor)
    for provider in PROVIDERS:
        for node_name in node_names:
            record, cell_errors = run_cell(
                options,
                provider=provider,
                node_name=node_name,
                expected_git_sha=str(source["gitSha"]),
                redactor=redactor,
            )
            cells.append(record)
            errors.extend(cell_errors)
            report["cells"] = cells
            report["errors"] = errors
            write_report(report, options, redactor)
    try:
        final_inventory = discover_nodes(options, redactor)
        report["finalNodeInventory"] = final_inventory
        final_names = [str(node["name"]) for node in final_inventory]
        if final_names != node_names:
            errors.append(
                MatrixError(
                    "stage5.matrix.node_inventory_changed",
                    "The Ready non-cordoned Worker inventory changed during the matrix.",
                    {"initialNodeNames": node_names, "finalNodeNames": final_names},
                ).as_report_error()
            )
    except MatrixError as error:
        errors.append(error.as_report_error())
    report["errors"] = errors
    report["finishedAt"] = acceptance.utc_now()
    report["durationMs"] = acceptance.elapsed_ms(started)
    report["completedCellCount"] = len(cells)
    report["passedCellCount"] = sum(cell.get("status") == "pass" for cell in cells)
    report["gvisorCompatibilitySummary"] = _gvisor_compatibility_summary(cells, options)
    if options.runtime_class is not None and not report["gvisorCompatibilitySummary"].get("complete"):
        errors.append(
            MatrixError(
                "stage5.matrix.gvisor_compatibility_evidence_incomplete",
                "The gVisor matrix did not retain complete toolchain, runtime, duration, and memory evidence for every cell.",
            ).as_report_error()
        )
        report["errors"] = errors
    report["status"] = "pass" if not errors and len(cells) == report["expectedCellCount"] else "fail"
    write_report(report, options, redactor)
    try:
        output_scan = dict(acceptance.scan_output_secrets(options.output_dir, redactor))
    except OSError as error:
        output_scan = {
            "status": "fail",
            "scannedFiles": 0,
            "scannedBytes": 0,
            "findings": [],
            "scanErrorType": type(error).__name__,
        }
    report["outputSecretScan"] = output_scan
    if output_scan.get("status") != "pass":
        errors.append(
            MatrixError(
                "stage5.matrix.output_secret_scan_failed",
                "The Stage 5 matrix output contained controlled or high-confidence Secret material.",
                {"findingCount": len(output_scan.get("findings", []))},
            ).as_report_error()
        )
        report["errors"] = errors
        report["status"] = "fail"
    json_path, markdown_path = write_report(report, options, redactor)
    report["artifacts"] = {
        "jsonReport": str(json_path),
        "markdownReport": str(markdown_path),
    }
    write_report(report, options, redactor)
    return report, 0 if report["status"] == "pass" else 1


def main(argv: Sequence[str] | None = None) -> int:
    options = parse_args(argv if argv is not None else sys.argv[1:])
    report_written = True
    try:
        report, exit_code = run_matrix(options)
    except (MatrixError, release_gate.ReleaseGateError) as error:
        redactor = credential_redactor(options)
        report_error = (
            error.as_report_error()
            if isinstance(error, MatrixError)
            else error.as_report_error()
        )
        report = {
            "schemaVersion": SCHEMA_VERSION,
            "status": "fail",
            "startedAt": acceptance.utc_now(),
            "finishedAt": acceptance.utc_now(),
            "source": None,
            "configuration": _configuration_report(options),
            "initialNodeInventory": None,
            "finalNodeInventory": None,
            "expectedCellCount": None,
            "completedCellCount": 0,
            "passedCellCount": 0,
            "cells": [],
            "gvisorCompatibilitySummary": None,
            "errors": [report_error],
            "outputSecretScan": None,
        }
        output_is_nonempty = options.output_dir.exists() and any(options.output_dir.iterdir())
        if output_is_nonempty:
            report_written = False
        else:
            options.output_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
            write_report(report, options, redactor)
        exit_code = 1
    print(f"Stage 5 Provider isolation matrix: {report['status']}")
    if report_written:
        print(f"JSON: {options.output_dir / JSON_REPORT_NAME}")
        print(f"Markdown: {options.output_dir / MARKDOWN_REPORT_NAME}")
    else:
        print("Reports were not written because the requested output directory was non-empty.")
    return exit_code


if __name__ == "__main__":
    raise SystemExit(main())
