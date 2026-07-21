#!/usr/bin/env python3
"""Verify immutable Kubernetes Worker rollout with controlled real Provider Sessions."""

from __future__ import annotations

import argparse
import dataclasses
import datetime as dt
import json
import os
import pathlib
import re
import sys
import time
import urllib.parse
import uuid
from collections.abc import Mapping, Sequence
from typing import Any

import acceptance_runner as acceptance
import controlled_remote_release_gate as remote
import kubernetes_worker_release_rollout_gate as fixture_rollout
import release_gate_common as release_gate
import worker_release_rollout_common as rollout


SCHEMA_VERSION = "synara.kubernetes-real-provider-worker-release-rollout-gate.v1"
JSON_REPORT_NAME = "kubernetes-real-provider-worker-release-rollout-gate.json"
MARKDOWN_REPORT_NAME = "kubernetes-real-provider-worker-release-rollout-gate.md"
DEFAULT_REGISTRY_IMAGE = fixture_rollout.DEFAULT_REGISTRY_IMAGE
DEFAULT_LOAD_WAVES = fixture_rollout.DEFAULT_LOAD_WAVES
REAL_PROVIDER_ROLLOUT_WORKER_LEASE_TTL = release_gate.PRODUCTION_WORKER_LEASE_TTL
REAL_PROVIDER_ROLLOUT_WORKER_HEARTBEAT_TIMEOUT = (
    release_gate.PRODUCTION_WORKER_HEARTBEAT_TIMEOUT
)
DEFAULT_SLA_FILE = (
    pathlib.Path(__file__).resolve().parents[2]
    / "deploy"
    / "worker"
    / "production-load-sla.json"
)


@dataclasses.dataclass(frozen=True)
class GateOptions:
    repo_root: pathlib.Path
    output_dir: pathlib.Path
    timeout_seconds: float
    skip_build: bool
    control_plane_binary: pathlib.Path | None
    docker_socket_path: pathlib.Path
    kubernetes_control_plane_host: str
    kind_bin: str
    kind_node_image: str
    kind_worker_nodes: int
    load_waves: int
    registry_image: str
    go_proxy: str | None
    provider: str
    real_provider_load_sla_file: pathlib.Path
    real_provider_credential_env: str
    real_provider_credential_field: str
    real_provider_base_url_env: str | None
    real_provider_model: str | None
    real_provider_model_env: str | None


def _provider_label(provider: str) -> str:
    return "Claude" if provider == "claudeAgent" else provider.capitalize()


def real_provider_rollout_approval_prompt(marker: str) -> str:
    return (
        "This is a new acceptance Turn. Do not reuse any tool result or final answer from an earlier "
        "Turn. You must independently invoke this Turn's one permitted tool and execute this Turn's "
        "command exactly once before answering. "
        f"{acceptance.real_provider_approval_prompt(marker)}"
    )


def parse_args(argv: Sequence[str]) -> GateOptions:
    repo_root = pathlib.Path(__file__).resolve().parents[2]
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--provider",
        choices=tuple(sorted(acceptance.REAL_PROVIDER_SMOKE_PROVIDERS)),
        default="codex",
    )
    parser.add_argument("--real-provider-credential-env", required=True)
    parser.add_argument(
        "--real-provider-credential-field",
        choices=acceptance.REAL_PROVIDER_CREDENTIAL_FIELDS,
        default="apiKey",
    )
    parser.add_argument("--real-provider-base-url-env")
    model_group = parser.add_mutually_exclusive_group()
    model_group.add_argument("--real-provider-model")
    model_group.add_argument("--real-provider-model-env")
    parser.add_argument("--output-dir", type=pathlib.Path)
    parser.add_argument("--timeout", type=float, default=5400.0)
    parser.add_argument("--skip-build", action="store_true")
    parser.add_argument("--control-plane-binary", type=pathlib.Path)
    parser.add_argument(
        "--real-provider-load-sla-file",
        type=pathlib.Path,
        default=DEFAULT_SLA_FILE,
    )
    parser.add_argument(
        "--docker-socket-path",
        type=pathlib.Path,
        default=pathlib.Path("/var/run/docker.sock"),
    )
    parser.add_argument("--kubernetes-control-plane-host", default="host.docker.internal")
    parser.add_argument("--kind-bin", default="kind")
    parser.add_argument("--kind-node-image", default="kindest/node:v1.33.1")
    parser.add_argument("--kind-worker-nodes", type=int, default=2)
    parser.add_argument("--load-waves", type=int, default=DEFAULT_LOAD_WAVES)
    parser.add_argument("--registry-image", default=DEFAULT_REGISTRY_IMAGE)
    parser.add_argument("--go-proxy")
    parsed = parser.parse_args(argv)
    if parsed.timeout <= 0:
        parser.error("--timeout must be positive")
    if parsed.control_plane_binary is not None and not parsed.skip_build:
        parser.error("--control-plane-binary requires --skip-build")
    if parsed.skip_build and parsed.control_plane_binary is None:
        parser.error("--skip-build requires --control-plane-binary")
    if parsed.kind_worker_nodes < 2 or parsed.kind_worker_nodes > 8:
        parser.error("--kind-worker-nodes must be between 2 and 8")
    if not 1 <= parsed.load_waves <= acceptance.REAL_PROVIDER_LOAD_MAX_WAVES:
        parser.error(
            "--load-waves must be between 1 and "
            f"{acceptance.REAL_PROVIDER_LOAD_MAX_WAVES}"
        )
    if parsed.real_provider_credential_field == "authToken" and parsed.provider != "claudeAgent":
        parser.error("--real-provider-credential-field authToken is supported only for claudeAgent")
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
    registry_image = parsed.registry_image.strip()
    if (
        not registry_image
        or len(registry_image) > 512
        or any(character.isspace() or ord(character) < 32 for character in registry_image)
        or any(character in registry_image for character in "?#")
        or "://" in registry_image
    ):
        parser.error("--registry-image must be a credential-free Docker image reference")
    try:
        go_proxy = release_gate.normalize_go_proxy(parsed.go_proxy)
        real_provider_load_sla_file = parsed.real_provider_load_sla_file.expanduser().resolve()
        operator_approved_sla = acceptance.parse_operator_approved_sla_file(
            real_provider_load_sla_file,
            "real-provider-load",
            option="--real-provider-load-sla-file",
        )
        credential = remote.parse_credential_source(
            parsed.real_provider_credential_env,
            parsed.real_provider_credential_field,
            parsed.real_provider_base_url_env,
            _provider_label(parsed.provider),
        )
        real_provider_model = remote.parse_provider_model_argument(
            parsed.real_provider_model,
            parsed.real_provider_model_env,
            provider_label=_provider_label(parsed.provider),
            model_option="--real-provider-model",
            model_env_option="--real-provider-model-env",
        )
        real_provider_model_env = acceptance.parse_environment_variable_name(
            parsed.real_provider_model_env,
            "--real-provider-model-env",
        )
    except ValueError as error:
        parser.error(str(error))
    if (
        operator_approved_sla is not None
        and parsed.timeout < operator_approved_sla.minimum_duration_seconds + 60.0
    ):
        parser.error(
            "--timeout must allow at least 60 seconds beyond the operator-approved minimumDurationSeconds"
        )
    run_id = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    output_dir = parsed.output_dir or (
        repo_root
        / ".tmp"
        / "stage3-provider-acceptance-results"
        / f"{run_id}-{uuid.uuid4().hex[:8]}-kubernetes-real-provider-worker-release-rollout"
    )
    return GateOptions(
        repo_root=repo_root,
        output_dir=output_dir.expanduser().resolve(),
        timeout_seconds=parsed.timeout,
        skip_build=parsed.skip_build,
        control_plane_binary=(
            parsed.control_plane_binary.expanduser().resolve()
            if parsed.control_plane_binary is not None
            else None
        ),
        docker_socket_path=docker_socket_path,
        kubernetes_control_plane_host=control_plane_host,
        kind_bin=kind_bin,
        kind_node_image=kind_node_image,
        kind_worker_nodes=parsed.kind_worker_nodes,
        load_waves=parsed.load_waves,
        registry_image=registry_image,
        go_proxy=go_proxy,
        provider=parsed.provider,
        real_provider_load_sla_file=real_provider_load_sla_file,
        real_provider_credential_env=credential.environment_name,
        real_provider_credential_field=credential.field,
        real_provider_base_url_env=credential.base_url_environment_name,
        real_provider_model=real_provider_model,
        real_provider_model_env=real_provider_model_env,
    )


def runner_options(options: GateOptions) -> acceptance.RunnerOptions:
    arguments = [
        "--target",
        "kubernetes",
        "--provider",
        options.provider,
        "--suite",
        "real-provider-load",
        "--runner-command-json",
        '["/usr/local/bin/provider-host"]',
        "--output-dir",
        str(options.output_dir),
        "--timeout",
        str(options.timeout_seconds),
        "--load-waves",
        str(options.load_waves),
        "--operator-approved-sla-file",
        str(options.real_provider_load_sla_file),
        "--worker-lease-ttl",
        REAL_PROVIDER_ROLLOUT_WORKER_LEASE_TTL,
        "--worker-heartbeat-timeout",
        REAL_PROVIDER_ROLLOUT_WORKER_HEARTBEAT_TIMEOUT,
        "--real-provider-credential-env",
        options.real_provider_credential_env,
        "--real-provider-credential-field",
        options.real_provider_credential_field,
        "--docker-socket-path",
        str(options.docker_socket_path),
        "--kubernetes-control-plane-host",
        options.kubernetes_control_plane_host,
        "--kind-bin",
        options.kind_bin,
        "--kind-node-image",
        options.kind_node_image,
        "--kind-worker-nodes",
        str(options.kind_worker_nodes),
        "--kubernetes-worker-image",
        "localhost.invalid/synara/worker-rollout:placeholder",
        "--kubernetes-skip-worker-build",
    ]
    if options.skip_build:
        arguments.extend(
            ["--skip-build", "--control-plane-binary", str(options.control_plane_binary)]
        )
    if options.real_provider_base_url_env is not None:
        arguments.extend(["--real-provider-base-url-env", options.real_provider_base_url_env])
    if options.real_provider_model is not None:
        arguments.extend(["--real-provider-model", options.real_provider_model])
    return acceptance.parse_args(arguments)


class KubernetesRealProviderReleaseRolloutDriver(
    fixture_rollout.KubernetesWorkerReleaseRolloutDriver
):
    def schedulable_worker_node_inventory(self) -> list[dict[str, Any]]:
        try:
            payload = json.loads(self._kubectl_command(["get", "nodes", "-o", "json"]))
        except json.JSONDecodeError:
            raise acceptance.AcceptanceError(
                "runner.kubernetes_nodes_invalid",
                "Kubernetes Node inventory was invalid JSON.",
            ) from None
        items = payload.get("items") if isinstance(payload, dict) else None
        if not isinstance(items, list) or not all(isinstance(item, dict) for item in items):
            raise acceptance.AcceptanceError(
                "runner.kubernetes_nodes_invalid",
                "Kubernetes Node inventory was malformed.",
            )
        inventory: list[dict[str, Any]] = []
        for item in items:
            metadata = acceptance.json_object(item.get("metadata"), "Kubernetes Node metadata")
            spec = acceptance.json_object(item.get("spec"), "Kubernetes Node spec")
            status = acceptance.json_object(item.get("status"), "Kubernetes Node status")
            name = metadata.get("name")
            labels = metadata.get("labels")
            conditions = status.get("conditions")
            taints = spec.get("taints", [])
            if (
                not isinstance(name, str)
                or not name
                or not isinstance(labels, dict)
                or not isinstance(conditions, list)
                or not all(isinstance(condition, dict) for condition in conditions)
                or not isinstance(taints, list)
                or not all(isinstance(taint, dict) for taint in taints)
            ):
                raise acceptance.AcceptanceError(
                    "runner.kubernetes_nodes_invalid",
                    "A Kubernetes Node omitted its scheduling identity.",
                )
            ready = any(
                condition.get("type") == "Ready" and condition.get("status") == "True"
                for condition in conditions
            )
            control_plane = any(
                key in labels
                for key in (
                    "node-role.kubernetes.io/control-plane",
                    "node-role.kubernetes.io/master",
                )
            )
            taint_summaries = sorted(
                (
                    {
                        "key": str(taint.get("key") or ""),
                        "effect": str(taint.get("effect") or ""),
                    }
                    for taint in taints
                ),
                key=lambda taint: (taint["key"], taint["effect"]),
            )
            unschedulable = spec.get("unschedulable") is True
            inventory.append(
                {
                    "name": name,
                    "ready": ready,
                    "unschedulable": unschedulable,
                    "controlPlane": control_plane,
                    "taints": taint_summaries,
                    "schedulableWorker": (
                        ready and not unschedulable and not control_plane and not taint_summaries
                    ),
                }
            )
        names = [str(node["name"]) for node in inventory]
        if len(names) != len(set(names)):
            raise acceptance.AcceptanceError(
                "runner.kubernetes_nodes_invalid",
                "Kubernetes Node inventory contained duplicate Node identities.",
                {"nodeNames": sorted(names)},
            )
        return inventory

    def provision_rollout_targets(
        self,
        tenant_id: str,
        organization_id: str,
        provider: str,
    ) -> Mapping[str, Any]:
        baseline = self.images["baseline"]
        candidate = self.images["candidate"]
        enabled_providers = (provider,)
        main = self._create_kubernetes_target(
            tenant_id,
            organization_id,
            provider,
            name=self.target_name,
            namespace=self.target_namespace,
            service_account=self.worker_service_account,
            image=baseline.exact_reference,
            max_active_pods=2,
            require_node_spread=True,
            experimental_providers=enabled_providers,
        )
        observer = self._create_kubernetes_target(
            tenant_id,
            organization_id,
            provider,
            name=self.canary_target_name,
            namespace=self.canary_namespace,
            service_account=self.canary_service_account,
            image=candidate.exact_reference,
            max_active_pods=1,
            experimental_providers=enabled_providers,
        )
        main_id = rollout.required_string(main, "id", "main Kubernetes Target")
        observer_id = rollout.required_string(observer, "id", "observer Kubernetes Target")
        self.target_id = main_id
        self.canary_target_id = observer_id
        self._remember_target_runtime(
            main_id,
            namespace=self.target_namespace,
            service_account=self.worker_service_account,
            image=baseline.exact_reference,
        )
        self._remember_target_runtime(
            observer_id,
            namespace=self.canary_namespace,
            service_account=self.canary_service_account,
            image=candidate.exact_reference,
        )
        self._wait_and_label_namespace(self.target_namespace)
        self._wait_and_label_namespace(self.canary_namespace)
        return {
            "mainTarget": acceptance.AcceptanceSuite._target_summary(main),
            "observerTarget": acceptance.AcceptanceSuite._target_summary(observer),
            "mainNamespace": self.target_namespace,
            "observerNamespace": self.canary_namespace,
            "mainMaxActivePods": 2,
            "observerMaxActivePods": 1,
            "mainExperimentalProviders": list(enabled_providers),
            "observerExperimentalProviders": list(enabled_providers),
            "resourceProfile": dict(
                acceptance.KUBERNETES_ACCEPTANCE_RESOURCE_CONFIGURATION
            ),
        }


class KubernetesRealProviderReleaseRolloutSuite(
    fixture_rollout.KubernetesWorkerReleaseRolloutSuite
):
    driver: KubernetesRealProviderReleaseRolloutDriver
    release_description_prefix = "Stage 3 Kubernetes real-provider rollout"

    def __init__(
        self,
        options: acceptance.RunnerOptions,
        driver: KubernetesRealProviderReleaseRolloutDriver,
        deadline: acceptance.Deadline,
        redactor: acceptance.SecretRedactor,
    ) -> None:
        super().__init__(options, driver, deadline, redactor)
        self.load_phase_metrics: dict[str, Mapping[str, Any]] = {}
        self.release_load_worker_ids: set[str] = set()
        self.real_provider_load_completed_wave_count = 0
        self.real_provider_load_session_turn_counts: dict[str, int] = {}
        self.real_provider_load_session_continuity: dict[str, list[dict[str, Any]]] = {}

    def _create_rollout_resources(self) -> Mapping[str, Any]:
        tenant_id = self._required("tenant_id")
        organization_id = self._required("organization_id")
        self._required("target_id")
        credential = self._create_real_provider_credential(
            title="Stage 3 Real Provider Worker Rollout",
            payload=self._real_provider_product_credential_payload(),
        )
        credential_id = self._string_id(credential, "real Provider rollout Credential")
        project = acceptance.json_object(
            self.api.request(
                "POST",
                f"/v1/tenants/{tenant_id}/organizations/{organization_id}/projects",
                {
                    "name": "Stage 3 Real Provider Worker Rollout",
                    "repositoryUrl": None,
                    "defaultBranch": "main",
                    "visibility": "organization",
                },
                expected=(201,),
            ),
            "real Provider rollout project",
        )
        project_id = self._string_id(project, "real Provider rollout project")
        self.state.project_id = project_id
        observer_id = rollout.required_string(
            {"id": self.driver.canary_target_id}, "id", "observer Target"
        )
        session_targets = {
            "baseline-seed": self._required("target_id"),
            "candidate-seed": observer_id,
            "baseline-active": self._required("target_id"),
            "candidate-canary": self._required("target_id"),
            "candidate-promoted": self._required("target_id"),
            "baseline-rollback": self._required("target_id"),
            "load-primary": self._required("target_id"),
        }
        summaries: dict[str, Mapping[str, Any]] = {}
        for key, target_id in session_targets.items():
            session = self._create_rollout_session(
                title=f"Stage 3 Kubernetes real-provider rollout {key}",
                target_id=target_id,
                credential_id=credential_id,
            )
            session_id = rollout.required_string(session, "id", f"{key} Session")
            if key == "load-primary":
                self.load_primary_session_id = session_id
            else:
                self.sessions[key] = session_id
            summaries[key] = {
                "id": session_id,
                "provider": session.get("provider"),
                "model": session.get("model"),
                "executionTargetId": session.get("executionTargetId"),
                "providerCredentialId": session.get("providerCredentialId"),
            }
        self.state.target_id = self.driver.target_id
        self.state.credential_id = credential_id
        self.state.session_id = self.sessions["baseline-active"]
        self.state.last_sequence = 0
        return {
            "credential": {
                "id": credential_id,
                "provider": credential.get("provider"),
                "credentialType": credential.get("credentialType"),
                "version": credential.get("version"),
                "organizationId": credential.get("organizationId"),
                "delivery": "control-plane-provider-credential",
                "source": "operator-environment",
                "credentialField": self.options.real_provider_credential_field,
                "baseUrlConfigured": self.options.real_provider_base_url_env is not None,
                "environmentVariableNamePersisted": False,
                "environmentVariableNameReportRedactionEnforced": True,
                "environmentVariableNameOutputScanEnforced": True,
                "payloadPersistedInReport": False,
            },
            "project": {
                "id": project_id,
                "organizationId": project.get("organizationId"),
                "repositoryUrl": project.get("repositoryUrl"),
            },
            "sessions": summaries,
        }

    def _create_rollout_session(
        self,
        *,
        title: str,
        target_id: str,
        credential_id: str,
    ) -> dict[str, Any]:
        previous_target = self.state.target_id
        self.state.target_id = target_id
        try:
            return self._create_project_session(
                provider=self.options.provider,
                title=title,
                credential_id=credential_id,
                model=self.options.real_provider_model,
                description=f"{title} Session",
            )
        finally:
            self.state.target_id = previous_target

    def _start_pending_approval(self, session_key: str, target_id: str) -> dict[str, Any]:
        session_id = self.sessions[session_key]
        marker = self._real_provider_marker(
            f"{session_key}-release",
            session_id=session_id,
            visible_label=session_key.replace("-", "_"),
        )
        expected_command = acceptance.real_provider_approval_command(marker)
        turn = self._create_turn(
            real_provider_rollout_approval_prompt(marker),
            runtime_mode="approval-required",
            session_id=session_id,
        )
        turn_id = self._turn_id(turn, f"{session_key} Turn")
        interaction, execution_id, request_id, interaction_payload, command = (
            self._real_provider_approval_interaction(
                turn_id,
                expected_command=expected_command,
                session_id=session_id,
            )
        )
        active = self._active_approval_evidence(
            turn_id,
            interaction,
            session_id=session_id,
            provider=self.options.provider,
        )
        return {
            "sessionKey": session_key,
            "sessionId": session_id,
            "targetId": target_id,
            "turn": turn,
            "interaction": interaction,
            "turnId": turn_id,
            "executionId": execution_id,
            "requestId": request_id,
            "requestKind": interaction_payload.get("requestKind"),
            "commandSummary": self.redactor.text(command[:256]),
            "marker": marker,
            "active": active,
        }

    def _start_real_provider_load_turn(
        self,
        session: Mapping[str, Any],
        wave_index: int,
        position: int,
    ) -> dict[str, Any]:
        session_id = rollout.required_string(session, "sessionId", "real Provider load Session")
        session_events = self._all_events(session_id=session_id)
        session_sequence_before_turn = (
            int(session_events[-1]["sequence"])
            if session_events and isinstance(session_events[-1].get("sequence"), int)
            else 0
        )
        marker = self._real_provider_marker(
            f"load-wave-{wave_index + 1}-position-{position}",
            session_id=session_id,
            visible_label=f"load_wave_{wave_index + 1}_position_{position}",
        )
        expected_command = acceptance.real_provider_approval_command(marker)
        started = time.monotonic()
        turn, control_plane_admission_latency_ms = self._create_turn_with_admission_latency(
            real_provider_rollout_approval_prompt(marker),
            runtime_mode="approval-required",
            session_id=session_id,
        )
        turn_id = self._turn_id(turn, "real Provider rollout load Turn")
        interaction, execution_id, request_id, interaction_payload, command = (
            self._real_provider_approval_interaction(
                turn_id,
                expected_command=expected_command,
                session_id=session_id,
            )
        )
        active = self._active_approval_evidence(
            turn_id,
            interaction,
            session_id=session_id,
            provider=self.options.provider,
        )
        return {
            "sessionId": session_id,
            "provider": self.options.provider,
            "turn": turn,
            "interaction": interaction,
            "active": active,
            "marker": marker,
            "requestId": request_id,
            "requestKind": interaction_payload.get("requestKind"),
            "commandSummary": self.redactor.text(command[:256]),
            "controlPlaneAdmissionLatencyMs": control_plane_admission_latency_ms,
            "interactionReadyLatencyMs": acceptance.elapsed_ms(started),
            "sessionSequenceBeforeTurn": session_sequence_before_turn,
            "turnStartedMonotonic": started,
            "targetExecution": None,
            "executionId": execution_id,
        }

    def _resolve_pending_approval(self, pending: Mapping[str, Any]) -> Mapping[str, Any]:
        session_id = rollout.required_string(pending, "sessionId", "pending Session")
        target_id = rollout.required_string(pending, "targetId", "pending Target")
        turn = acceptance.json_object(pending.get("turn"), "pending Turn")
        interaction = acceptance.json_object(pending.get("interaction"), "pending interaction")
        marker = rollout.required_string(pending, "marker", "pending marker")
        expected_command = acceptance.real_provider_approval_command(marker)
        replacement_generation = self._pending_replacement_generation(pending)
        recovery_options = (
            {"max_lease_generations": replacement_generation}
            if replacement_generation is not None
            else {}
        )
        resolution = self._resolve_real_provider_command_turn(
            turn=turn,
            interaction=interaction,
            session_id=session_id,
            marker=marker,
            expected_command=expected_command,
            **recovery_options,
        )
        target_terminal = self.driver.observe_terminal_execution(
            target_id,
            rollout.required_string(resolution, "executionId", "resolved Execution"),
        )
        return {**dict(resolution), "targetTerminal": target_terminal}

    def _resolve_real_provider_command_turn(
        self,
        *,
        turn: Mapping[str, Any],
        interaction: Mapping[str, Any],
        session_id: str,
        marker: str,
        expected_command: str,
        expected_resume_strategy: str = "authoritative-history",
        expected_resume_reason: str = "cursor_absent",
        max_lease_generations: int = 1,
    ) -> dict[str, Any]:
        turn_id = self._turn_id(turn, "real Provider rollout approval Turn")
        interaction_payload = acceptance.json_object(
            interaction.get("payload"),
            "real Provider rollout Approval payload",
        )
        execution_id, request_id, payload, command = self._real_provider_approval_request_details(
            interaction,
            turn_id=turn_id,
            expected_command=expected_command,
        )
        approvals: list[dict[str, Any]] = []
        seen_request_ids: set[str] = set()
        seen_interaction_ids: set[str] = set()
        current_interaction = interaction
        current_execution_id = execution_id
        current_request_id = request_id
        current_payload = payload
        current_command = command
        terminal: dict[str, Any] | None = None
        events: list[dict[str, Any]] | None = None
        for _attempt in range(acceptance.REAL_PROVIDER_MAX_SEQUENTIAL_APPROVALS):
            current_interaction_id = rollout.required_string(
                {"id": current_interaction.get("id")},
                "id",
                "Approval interaction",
            )
            if current_interaction_id in seen_interaction_ids:
                raise acceptance.AcceptanceError(
                    "runner.real_provider_approval_interaction_reused",
                    "The rollout Approval Turn reused a resolved interaction ID.",
                    {
                        "turnId": turn_id,
                        "executionId": execution_id,
                        "interactionId": current_interaction_id,
                    },
                )
            if current_request_id in seen_request_ids:
                raise acceptance.AcceptanceError(
                    "runner.real_provider_approval_request_reused",
                    "The rollout Approval Turn reused a resolved Request identity.",
                    {
                        "turnId": turn_id,
                        "executionId": execution_id,
                        "requestId": current_request_id,
                    },
                )
            seen_interaction_ids.add(current_interaction_id)
            resolved = acceptance.json_object(
                self.api.request(
                    "POST",
                    "/v1/executions/"
                    + current_execution_id
                    + "/approvals/"
                    + urllib.parse.quote(current_request_id, safe="")
                    + "/resolve",
                    {"decision": "accept"},
                ),
                "real Provider rollout approval resolution",
            )
            approvals.append(
                {
                    "interactionId": current_interaction_id,
                    "requestId": current_request_id,
                    "requestKind": current_payload.get("requestKind"),
                    "commandSummary": self.redactor.text(current_command[:256]),
                    "resolutionStatus": resolved.get("status"),
                    "deliveryStatus": resolved.get("deliveryStatus"),
                }
            )
            seen_request_ids.add(current_request_id)
            outcome = self._wait_for_turn_terminal_or_follow_up_approval(
                turn_id,
                current_interaction_id,
                current_request_id,
                session_id=session_id,
            )
            terminal_candidate = outcome.get("terminal")
            if isinstance(terminal_candidate, dict):
                matching_events = outcome.get("events")
                if not isinstance(matching_events, list) or not all(
                    isinstance(event, dict) for event in matching_events
                ):
                    raise acceptance.AcceptanceError(
                        "runner.response_shape_invalid",
                        "The rollout Approval terminal snapshot omitted its Event list.",
                    )
                terminal = terminal_candidate
                events = matching_events
                break
            next_interaction = acceptance.json_object(
                outcome.get("interaction"),
                "real Provider rollout follow-up Approval interaction",
            )
            next_execution_id, next_request_id, next_payload, next_command = (
                self._real_provider_approval_request_details(
                    next_interaction,
                    turn_id=turn_id,
                    expected_command=expected_command,
                )
            )
            if next_execution_id != execution_id:
                raise acceptance.AcceptanceError(
                    "runner.real_provider_follow_up_approval_execution_mismatch",
                    "The rollout Approval Turn changed Execution identity between sequential Approvals.",
                    {
                        "turnId": turn_id,
                        "expectedExecutionId": execution_id,
                        "actualExecutionId": next_execution_id,
                    },
                )
            current_interaction = next_interaction
            current_execution_id = next_execution_id
            current_request_id = next_request_id
            current_payload = next_payload
            current_command = next_command
        else:
            raise acceptance.AcceptanceError(
                "runner.real_provider_approval_limit_exceeded",
                "The rollout Approval Turn required too many sequential Approval resolutions.",
                {
                    "turnId": turn_id,
                    "executionId": execution_id,
                    "maxSequentialApprovals": acceptance.REAL_PROVIDER_MAX_SEQUENTIAL_APPROVALS,
                    "resolvedApprovalCount": len(approvals),
                },
            )
        assert terminal is not None
        assert events is not None
        provider_turn = self._real_provider_turn_evidence(
            turn_id,
            terminal,
            events,
            marker,
            expected_resume_strategy=expected_resume_strategy,
            expected_resume_reason=expected_resume_reason,
            max_lease_generations=max_lease_generations,
        )
        terminal_worker_id, terminal_generation = self._event_worker_identity(terminal)
        command_item = self._approval_command_item_evidence(
            events,
            execution_id=execution_id,
            worker_id=terminal_worker_id,
            generation=terminal_generation,
            terminal_sequence=terminal.get("sequence"),
        )
        event_type_counts: dict[str, int] = {}
        for event in events:
            event_type = str(event.get("eventType") or "")
            event_type_counts[event_type] = event_type_counts.get(event_type, 0) + 1
        for index, approval_entry in enumerate(approvals, start=1):
            opened = self._interaction_request_event(
                events,
                execution_id,
                str(approval_entry["requestId"]),
                "request.opened",
                f"rollout approval request #{index}",
            )
            resolved_event = self._interaction_request_event(
                events,
                execution_id,
                str(approval_entry["requestId"]),
                "request.resolved",
                f"rollout approval resolution #{index}",
            )
            approval_entry["openedEvent"] = self._event_summary(opened)
            approval_entry["resolvedEvent"] = self._event_summary(resolved_event)
        self.state.last_real_marker = marker
        first_approval = approvals[0]
        last_approval = approvals[-1]
        return {
            "turnId": turn_id,
            "executionId": execution_id,
            "workerId": terminal_worker_id,
            "generation": terminal_generation,
            "interactionId": first_approval["interactionId"],
            "requestId": first_approval["requestId"],
            "requestKind": interaction_payload.get("requestKind"),
            "commandSummary": first_approval["commandSummary"],
            "resolutionStatus": last_approval["resolutionStatus"],
            "deliveryStatus": last_approval["deliveryStatus"],
            "approvalCount": len(approvals),
            "approvalResolutions": approvals,
            "markerMatched": provider_turn.get("markerMatched"),
            "assistantTextBytes": provider_turn.get("assistantTextBytes"),
            "assistantTextSha256": provider_turn.get("assistantTextSha256"),
            "providerResume": provider_turn.get("providerResume"),
            "providerTurn": provider_turn,
            "commandItem": command_item,
            "eventTypeCounts": dict(sorted(event_type_counts.items())),
            "sequenceRange": self._sequence_range(events),
            "singleTerminal": True,
        }

    def _canary_overlap(self) -> Mapping[str, Any]:
        evidence = dict(super()._canary_overlap())
        baseline = acceptance.json_object(evidence.get("baseline"), "baseline overlap evidence")
        candidate = acceptance.json_object(evidence.get("candidate"), "candidate overlap evidence")
        distinct_nodes = self._require_distinct_nodes(
            "canary-overlap",
            [
                acceptance.json_object(baseline.get("pod"), "baseline overlap Pod"),
                acceptance.json_object(candidate.get("pod"), "candidate overlap Pod"),
            ],
        )
        evidence["distinctNodeEvidence"] = distinct_nodes
        return evidence

    def _activate_load_primary_session(self) -> None:
        if getattr(self, "_real_provider_load_session_cache", None) is not None:
            return
        session_id = self.load_primary_session_id
        if not isinstance(session_id, str) or not session_id:
            raise acceptance.AcceptanceError(
                "runner.kubernetes_rollout_load_session_missing",
                "The bounded Kubernetes rollout load Session was not created.",
            )
        self.state.session_id = session_id
        self.state.last_sequence = 0

    def _release_load_waves(
        self,
        *,
        slot: str,
        channel: str,
        wave_start: int,
        wave_count: int,
    ) -> Mapping[str, Any]:
        self._activate_load_primary_session()
        revision_id = rollout.required_string(
            self.revisions[slot], "id", f"{slot} Release Revision"
        )
        manifest_id = rollout.required_string(
            self.manifests[slot], "manifestId", f"{slot} Worker Manifest"
        )
        phase = f"{slot}-{channel}"
        quota = self._set_fixture_execution_quota(
            acceptance.REAL_PROVIDER_LOAD_CONCURRENCY,
            "real Provider rollout load admission",
            "runner.real_provider_load_quota_mismatch",
        )
        sessions = self._real_provider_load_sessions()
        execution_ids: set[str] = set()
        worker_ids: set[str] = set()
        node_names: set[str] = set()
        provider_execution_counts = {self.options.provider: 0}
        session_execution_counts = {
            str(session["sessionId"]): 0 for session in sessions
        }
        event_type_counts: dict[str, int] = {}
        active_checks = 0
        terminal_checks = 0
        worker_binding_checks = 0
        runtime_checks = 0
        quota_rejections = 0
        overlap_observations = 0
        control_plane_admission_latency_ms: list[int] = []
        slot_reuse_admission_latency_ms: list[int] = []
        interaction_ready_latency_ms: list[int] = []
        turn_completion_latency_ms: list[int] = []
        wave_durations_ms: list[int] = []
        first_wave_samples: list[dict[str, Any]] = []
        last_wave_samples: list[dict[str, Any]] = []
        samples: list[dict[str, Any]] = []
        started = time.monotonic()
        minimum_duration_ms = round(self.options.load_min_duration_seconds * 1000)
        maximum_wave_count = self.options.load_max_waves
        actual_wave_start = self.real_provider_load_completed_wave_count

        def start(
            session: Mapping[str, Any],
            global_wave_index: int,
            phase_wave_index: int,
            position: int,
        ) -> dict[str, Any]:
            nonlocal active_checks, worker_binding_checks, runtime_checks
            session_id = rollout.required_string(
                session, "sessionId", "real Provider rollout load Session"
            )
            prior_session_turns = self.real_provider_load_session_turn_counts.get(session_id, 0)
            expected_resume_strategy = (
                "authoritative-history" if prior_session_turns == 0 else "native-cursor"
            )
            expected_resume_reason = (
                "cursor_absent" if prior_session_turns == 0 else "cursor_usable"
            )
            load_turn = self._start_real_provider_load_turn(
                session,
                global_wave_index,
                position,
            )
            turn = acceptance.json_object(load_turn.get("turn"), "rollout load Turn")
            active = acceptance.json_object(
                load_turn.get("active"), "rollout load active Execution"
            )
            turn_id = self._turn_id(turn, "rollout load Turn")
            release = self._wait_execution_release(
                turn_id,
                revision_id=revision_id,
                channel=channel,
                manifest_id=manifest_id,
                terminal=False,
                session_id=str(load_turn["sessionId"]),
            )
            rollout.validate_release_load_identity(active, release)
            worker = self._wait_managed_worker(
                release,
                revision_id=revision_id,
                channel=channel,
                manifest_id=manifest_id,
            )
            pod = self.driver.observe_release_execution(
                self._required("target_id"),
                rollout.required_string(release, "executionId", "rollout load Execution"),
                expected_image=self.driver.images[slot].exact_reference,
                expected_release_revision_id=revision_id,
                expected_release_channel=channel,
            )
            if worker.get("podName") != pod.get("podName"):
                raise acceptance.AcceptanceError(
                    "runner.kubernetes_rollout_worker_pod_mismatch",
                    "The managed Worker API and Kubernetes API identified different release Pods.",
                    {
                        "workerPodName": worker.get("podName"),
                        "kubernetesPodName": pod.get("podName"),
                    },
                )
            resources = self.driver.observe_release_resource_profile(
                self._required("target_id"),
                rollout.required_string(release, "executionId", "rollout load Execution"),
                max_active_pods=acceptance.REAL_PROVIDER_LOAD_CONCURRENCY,
            )
            active_checks += 1
            worker_binding_checks += 1
            runtime_checks += 1
            if len(samples) < 2:
                samples.append(
                    {
                        "stage": "active",
                        "sessionId": load_turn.get("sessionId"),
                        "execution": release,
                        "worker": worker,
                        "runtime": {"pod": pod, "resources": resources},
                    }
                )
            return {
                **dict(load_turn),
                "phase": phase,
                "revisionId": revision_id,
                "globalWave": global_wave_index + 1,
                "phaseWave": phase_wave_index + 1,
                "turnOrdinal": prior_session_turns + 1,
                "expectedResumeStrategy": expected_resume_strategy,
                "expectedResumeReason": expected_resume_reason,
                "release": release,
                "workerBinding": worker,
                "pod": pod,
                "resources": resources,
            }

        def observe_overlap(
            active_turns: Sequence[Mapping[str, Any]],
            stage: str,
        ) -> dict[str, Any]:
            base_overlap = self._real_provider_load_overlap(active_turns, stage)
            pods = [
                acceptance.json_object(item.get("pod"), "real Provider rollout active Pod")
                for item in active_turns
            ]
            distinct_nodes = self._require_distinct_nodes(stage, pods)
            node_names.update(str(node) for node in distinct_nodes["nodeNames"])
            return {
                **dict(base_overlap),
                "nodeNames": distinct_nodes["nodeNames"],
                "podNames": [pod.get("podName") for pod in pods],
                "activeCount": len(active_turns),
                "podCount": distinct_nodes["podCount"],
                "nodeCount": distinct_nodes["nodeCount"],
                "schedulableWorkerInventoryMatched": True,
                "resourceProfiles": [item.get("resources") for item in active_turns],
            }

        def complete(load_turn: Mapping[str, Any]) -> dict[str, Any]:
            nonlocal terminal_checks
            expected_resume_strategy = rollout.required_string(
                load_turn,
                "expectedResumeStrategy",
                "rollout load expected resume strategy",
            )
            expected_resume_reason = rollout.required_string(
                load_turn,
                "expectedResumeReason",
                "rollout load expected resume reason",
            )
            terminal = self._complete_real_provider_release_load_turn(
                load_turn,
                revision_id=revision_id,
                channel=channel,
                manifest_id=manifest_id,
                expected_resume_strategy=expected_resume_strategy,
                expected_resume_reason=expected_resume_reason,
            )
            execution_id = rollout.required_string(
                terminal, "executionId", "rollout load terminal Execution"
            )
            if execution_id in self.release_load_execution_ids or execution_id in execution_ids:
                raise acceptance.AcceptanceError(
                    "runner.worker_release_load_execution_reused",
                    "Worker Release load reused an Execution across rollout phases.",
                    {"phase": phase, "executionId": execution_id},
                )
            self.release_load_execution_ids.add(execution_id)
            execution_ids.add(execution_id)
            worker_id = rollout.required_string(
                terminal, "workerId", "rollout load terminal Worker"
            )
            if worker_id in self.release_load_worker_ids or worker_id in worker_ids:
                raise acceptance.AcceptanceError(
                    "runner.worker_release_load_worker_reused",
                    "Execution-pinned Kubernetes rollout load reused a Worker identity.",
                    {"phase": phase, "executionId": execution_id, "workerId": worker_id},
                )
            self.release_load_worker_ids.add(worker_id)
            worker_ids.add(worker_id)
            provider_execution_counts[self.options.provider] += 1
            session_id = rollout.required_string(
                terminal, "sessionId", "rollout load terminal Session"
            )
            turn_ordinal = terminal.get("turnOrdinal")
            prior_session_turns = self.real_provider_load_session_turn_counts.get(session_id, 0)
            if turn_ordinal != prior_session_turns + 1:
                raise acceptance.AcceptanceError(
                    "runner.kubernetes_real_provider_session_turn_ordinal_invalid",
                    "A rollout load Session did not advance by exactly one Turn.",
                    {
                        "sessionId": session_id,
                        "priorTurnCount": prior_session_turns,
                        "turnOrdinal": turn_ordinal,
                    },
                )
            provider_resume = acceptance.json_object(
                terminal.get("providerResume"),
                "rollout load Provider resume evidence",
            )
            expected_resume = {
                "requestedStrategy": "native-cursor",
                "selectedStrategy": expected_resume_strategy,
                "reasonCode": expected_resume_reason,
            }
            if {key: provider_resume.get(key) for key in expected_resume} != expected_resume:
                raise acceptance.AcceptanceError(
                    "runner.real_provider_resume_decision_mismatch",
                    "The rollout load Turn did not retain the expected Provider resume decision.",
                    {
                        "sessionId": session_id,
                        "turnOrdinal": turn_ordinal,
                        "expected": expected_resume,
                        "actual": {key: provider_resume.get(key) for key in expected_resume},
                    },
                )
            self.real_provider_load_session_turn_counts[session_id] = int(turn_ordinal)
            self.real_provider_load_session_continuity.setdefault(session_id, []).append(
                {
                    "sessionId": session_id,
                    "turnOrdinal": turn_ordinal,
                    "phase": phase,
                    "revisionId": revision_id,
                    "channel": channel,
                    "globalWave": load_turn.get("globalWave"),
                    "phaseWave": load_turn.get("phaseWave"),
                    "turnId": terminal.get("turnId"),
                    "executionId": execution_id,
                    "workerId": worker_id,
                    "providerResume": dict(provider_resume),
                    "sessionSequenceBeforeTurn": terminal.get("sessionSequenceBeforeTurn"),
                    "sessionSequenceAfterTurn": terminal.get("sessionSequenceAfterTurn"),
                    "turnSequenceRange": terminal.get("sequenceRange"),
                    "markerMatched": terminal.get("markerMatched") is True,
                    "commandItemVerified": terminal.get("commandItemVerified") is True,
                }
            )
            session_execution_counts[session_id] += 1
            turn_completion_latency_ms.append(
                int(terminal["turnCompletionLatencyMs"])
            )
            terminal_checks += 1
            counts = acceptance.json_object(
                terminal.get("eventTypeCounts"),
                "rollout load terminal event type counts",
            )
            for event_type, count in counts.items():
                if isinstance(count, int):
                    event_type_counts[event_type] = event_type_counts.get(event_type, 0) + count
            if len(samples) < 4:
                samples.append(
                    {
                        "stage": "terminal",
                        "sessionId": terminal.get("sessionId"),
                        "execution": {
                            key: terminal.get(key)
                            for key in (
                                "executionId",
                                "workerId",
                                "generation",
                                "sequenceRange",
                            )
                        },
                    }
                )
            return terminal

        completed_wave_count = 0
        while completed_wave_count < maximum_wave_count:
            phase_wave_index = completed_wave_count
            global_wave_index = self.real_provider_load_completed_wave_count
            wave_started = time.monotonic()
            offset = global_wave_index % len(sessions)
            ordered = sessions[offset:] + sessions[:offset]
            active = [
                start(ordered[0], global_wave_index, phase_wave_index, 1),
                start(ordered[1], global_wave_index, phase_wave_index, 2),
            ]
            wave_control_plane_admission_latency_ms: list[int] = []
            wave_slot_reuse_admission_latency_ms: list[int] = []
            wave_interaction_ready_latency_ms: list[int] = []
            for load_turn in active:
                admission_latency_ms, interaction_latency_ms = (
                    self._record_load_start_latencies(
                        load_turn,
                        control_plane_admission_latency_ms,
                        interaction_ready_latency_ms,
                    )
                )
                wave_control_plane_admission_latency_ms.append(admission_latency_ms)
                wave_interaction_ready_latency_ms.append(interaction_latency_ms)
            overlaps = [
                observe_overlap(active, f"wave-{global_wave_index + 1}-initial")
            ]
            overlap_observations += 1
            rejections: list[dict[str, Any]] = []
            wave_turn_completion_latency_ms: list[int] = []
            for position, session in enumerate(ordered[2:], start=3):
                rejections.append(
                    self._assert_real_provider_load_quota_rejected(
                        session,
                        global_wave_index,
                        position,
                    )
                )
                quota_rejections += 1
                wave_turn_completion_latency_ms.append(
                    int(complete(active.pop(0))["turnCompletionLatencyMs"])
                )
                self._assert_real_provider_load_turn_pending(
                    active[0],
                    f"wave-{global_wave_index + 1}-before-position-{position}-admission",
                )
                recovered = start(
                    session,
                    global_wave_index,
                    phase_wave_index,
                    position,
                )
                admission_latency_ms, interaction_latency_ms = (
                    self._record_load_start_latencies(
                        recovered,
                        control_plane_admission_latency_ms,
                        interaction_ready_latency_ms,
                    )
                )
                slot_reuse_admission_latency_ms.append(admission_latency_ms)
                wave_control_plane_admission_latency_ms.append(admission_latency_ms)
                wave_slot_reuse_admission_latency_ms.append(admission_latency_ms)
                wave_interaction_ready_latency_ms.append(interaction_latency_ms)
                active.append(recovered)
                overlaps.append(
                    observe_overlap(
                        active,
                        "wave-"
                        f"{global_wave_index + 1}-slot-reuse-"
                        f"{position - acceptance.REAL_PROVIDER_LOAD_CONCURRENCY}",
                    )
                )
                overlap_observations += 1
            wave_turn_completion_latency_ms.append(
                int(complete(active.pop(0))["turnCompletionLatencyMs"])
            )
            self._assert_real_provider_load_turn_pending(
                active[0],
                f"wave-{global_wave_index + 1}-before-final-terminal",
            )
            wave_turn_completion_latency_ms.append(
                int(complete(active.pop(0))["turnCompletionLatencyMs"])
            )
            for session in sessions:
                pending = self._pending_interactions(str(session["sessionId"]))
                if pending:
                    raise acceptance.AcceptanceError(
                        "runner.real_provider_load_interaction_leaked",
                        "The real Provider rollout load phase ended with a pending interaction.",
                        {
                            "wave": global_wave_index + 1,
                            "sessionId": session["sessionId"],
                            "interactionIds": [item.get("id") for item in pending],
                        },
                    )
            wave_duration_ms = acceptance.elapsed_ms(wave_started)
            wave_durations_ms.append(wave_duration_ms)
            sample = {
                "wave": global_wave_index + 1,
                "phaseWave": phase_wave_index + 1,
                "sessionOrder": [str(session["sessionId"]) for session in ordered],
                "providerOrder": [self.options.provider] * acceptance.REAL_PROVIDER_LOAD_SESSIONS,
                "nodeNames": [overlap["nodeNames"] for overlap in overlaps],
                "overlapActiveCounts": [overlap["activeCount"] for overlap in overlaps],
                "overlapNodeCounts": [overlap["nodeCount"] for overlap in overlaps],
                "overlapWorkerIds": [overlap["workerIds"] for overlap in overlaps],
                "targetExecutionIdentities": [
                    overlap["targetExecutionIdentities"] for overlap in overlaps
                ],
                "quotaRejections": rejections,
                "controlPlaneAdmissionLatencyMs": acceptance.duration_distribution_ms(
                    wave_control_plane_admission_latency_ms
                ),
                "slotReuseAdmissionLatencyMs": acceptance.duration_distribution_ms(
                    wave_slot_reuse_admission_latency_ms
                ),
                "interactionReadyLatencyMs": acceptance.duration_distribution_ms(
                    wave_interaction_ready_latency_ms
                ),
                "turnCompletionLatencyMs": acceptance.duration_distribution_ms(
                    wave_turn_completion_latency_ms
                ),
                "durationMs": wave_duration_ms,
            }
            if completed_wave_count < 2:
                first_wave_samples.append(sample)
            last_wave_samples = (last_wave_samples + [sample])[-2:]
            completed_wave_count += 1
            self.real_provider_load_completed_wave_count += 1
            if (
                completed_wave_count >= wave_count
                and acceptance.elapsed_ms(started) >= minimum_duration_ms
            ):
                break
        duration_ms = acceptance.elapsed_ms(started)
        duration_target_met = duration_ms >= minimum_duration_ms
        if completed_wave_count < wave_count or not duration_target_met:
            raise acceptance.AcceptanceError(
                "runner.real_provider_load_duration_not_reached",
                "The real Provider rollout load phase reached its maximum wave safety bound before satisfying the requested duration.",
                {
                    "phase": phase,
                    "minimumWaves": wave_count,
                    "maximumWaves": maximum_wave_count,
                    "wavesCompleted": completed_wave_count,
                    "minimumDurationSeconds": self.options.load_min_duration_seconds,
                    "durationMs": duration_ms,
                },
            )
        expected_executions = completed_wave_count * acceptance.REAL_PROVIDER_LOAD_SESSIONS
        expected_rejections = completed_wave_count * (
            acceptance.REAL_PROVIDER_LOAD_SESSIONS - acceptance.REAL_PROVIDER_LOAD_CONCURRENCY
        )
        expected_overlaps = completed_wave_count * (
            acceptance.REAL_PROVIDER_LOAD_SESSIONS - 1
        )
        if (
            len(execution_ids) != expected_executions
            or quota_rejections != expected_rejections
            or overlap_observations != expected_overlaps
            or len(worker_ids) != expected_executions
            or active_checks != expected_executions
            or terminal_checks != expected_executions
            or worker_binding_checks != expected_executions
            or runtime_checks != expected_executions
            or any(count != completed_wave_count for count in session_execution_counts.values())
            or provider_execution_counts[self.options.provider] != expected_executions
        ):
            raise acceptance.AcceptanceError(
                "runner.kubernetes_real_provider_release_load_invalid",
                "The rollout load phase did not retain canonical execution, release-pin, overlap, node, or distribution counts.",
                {
                    "phase": phase,
                    "expectedExecutions": expected_executions,
                    "distinctExecutionCount": len(execution_ids),
                    "expectedQuotaRejections": expected_rejections,
                    "quotaRejections": quota_rejections,
                    "expectedOverlapObservations": expected_overlaps,
                    "overlapObservations": overlap_observations,
                    "expectedDistinctWorkerCount": expected_executions,
                    "workerIds": sorted(worker_ids),
                    "nodeNames": sorted(node_names),
                    "activeReleasePinChecks": active_checks,
                    "terminalReleasePinChecks": terminal_checks,
                    "workerBindingChecks": worker_binding_checks,
                    "activeRuntimeChecks": runtime_checks,
                    "providerExecutionCounts": dict(sorted(provider_execution_counts.items())),
                    "sessionExecutionCounts": dict(sorted(session_execution_counts.items())),
                },
            )
        sampled_waves: dict[int, dict[str, Any]] = {}
        for sample in first_wave_samples + last_wave_samples:
            sampled_waves[int(sample["wave"])] = sample
        wave_samples = [sampled_waves[index] for index in sorted(sampled_waves)]
        admission_attempts = len(execution_ids) + quota_rejections
        resume_continuity = self._real_provider_load_resume_continuity(
            require_cross_revision=slot == "baseline"
        )
        evidence = {
            "phase": phase,
            "revisionId": revision_id,
            "channel": channel,
            "manifestId": manifest_id,
            "registryDigest": self.driver.images[slot].digest,
            "maxConcurrentExecutions": quota.get("maxConcurrentExecutions"),
            "workers": acceptance.REAL_PROVIDER_LOAD_CONCURRENCY,
            "sessions": acceptance.REAL_PROVIDER_LOAD_SESSIONS,
            "providers": [self.options.provider],
            "wavesRequested": wave_count,
            "nominalWaveStart": wave_start,
            "actualWaveStart": actual_wave_start,
            "minimumWavesRequested": wave_count,
            "maximumWaves": maximum_wave_count,
            "minimumDurationSeconds": self.options.load_min_duration_seconds,
            "durationTargetMet": duration_target_met,
            "stopReason": "minimum-waves-and-duration-satisfied",
            "wavesCompleted": completed_wave_count,
            "firstWave": actual_wave_start + 1,
            "lastWave": actual_wave_start + completed_wave_count,
            "executionsCompleted": len(execution_ids),
            "distinctExecutionCount": len(execution_ids),
            "expectedDistinctWorkerCount": expected_executions,
            "distinctWorkerCount": len(worker_ids),
            "globalDistinctWorkerCount": len(self.release_load_worker_ids),
            "distinctNodeCount": len(node_names),
            "distinctNodeNames": sorted(node_names),
            "quotaRejections": quota_rejections,
            "admissionRetriesSucceeded": len(slot_reuse_admission_latency_ms),
            "admissionAttempts": admission_attempts,
            "expectedQuotaRejectionRate": round(
                quota_rejections / max(admission_attempts, 1),
                6,
            ),
            "overlapObservations": overlap_observations,
            "effectiveConcurrency": acceptance.REAL_PROVIDER_LOAD_CONCURRENCY,
            "executionSuccessRate": round(
                len(execution_ids) / max(expected_executions, 1),
                6,
            ),
            "unexpectedFailureCount": 0,
            "unexpectedErrorRate": 0.0,
            "doubleExecution": False,
            "duplicateTerminal": False,
            "pendingInteractionCount": 0,
            "providerExecutionCounts": dict(sorted(provider_execution_counts.items())),
            "sessionExecutionCounts": dict(sorted(session_execution_counts.items())),
            "eventTypeCounts": dict(sorted(event_type_counts.items())),
            "resourceProfile": acceptance.fixture_load_resource_profile(self.options),
            "durationMs": duration_ms,
            "observedCompletedExecutionsPerSecond": round(
                len(execution_ids) / max(duration_ms / 1000.0, 0.001),
                3,
            ),
            "controlPlaneAdmissionLatencyMs": acceptance.duration_distribution_ms(
                control_plane_admission_latency_ms
            ),
            "slotReuseAdmissionLatencyMs": acceptance.duration_distribution_ms(
                slot_reuse_admission_latency_ms
            ),
            "interactionReadyLatencyMs": acceptance.duration_distribution_ms(
                interaction_ready_latency_ms
            ),
            "turnCompletionLatencyMs": acceptance.duration_distribution_ms(
                turn_completion_latency_ms
            ),
            "waveDurationMs": acceptance.duration_distribution_ms(wave_durations_ms),
            "sessionsEvidence": [dict(session) for session in sessions],
            "sessionResumeContinuity": resume_continuity,
            "waveSamples": wave_samples,
            "activeReleasePinChecks": active_checks,
            "terminalReleasePinChecks": terminal_checks,
            "workerBindingChecks": worker_binding_checks,
            "activeRuntimeChecks": runtime_checks,
            "releasePinSamples": samples,
        }
        enriched = self._with_operator_approved_sla(evidence)
        self.release_load_phase_counts[phase] = expected_executions
        self.load_phase_metrics[phase] = dict(enriched)
        return {
            **dict(enriched),
            "podResourceProfileChecks": runtime_checks,
            "resourceProfile": dict(
                acceptance.KUBERNETES_ACCEPTANCE_RESOURCE_CONFIGURATION
            ),
        }

    def _real_provider_load_resume_continuity(
        self,
        *,
        require_cross_revision: bool,
    ) -> dict[str, Any]:
        expected_revision_order = [
            rollout.required_string(
                self.revisions["candidate"],
                "id",
                "candidate Release Revision",
            ),
            rollout.required_string(
                self.revisions["baseline"],
                "id",
                "baseline Release Revision",
            ),
        ]
        session_ids = sorted(self.real_provider_load_session_continuity)
        if len(session_ids) != acceptance.REAL_PROVIDER_LOAD_SESSIONS:
            raise acceptance.AcceptanceError(
                "runner.kubernetes_real_provider_resume_session_count_invalid",
                "The rollout load continuity evidence did not cover every bounded Session.",
                {
                    "expectedSessionCount": acceptance.REAL_PROVIDER_LOAD_SESSIONS,
                    "actualSessionCount": len(session_ids),
                },
            )
        session_evidence: list[dict[str, Any]] = []
        cross_revision_sessions = 0
        for session_id in session_ids:
            turns = self.real_provider_load_session_continuity[session_id]
            ordinals = [turn.get("turnOrdinal") for turn in turns]
            expected_ordinals = list(range(1, len(turns) + 1))
            revision_order: list[str] = []
            for turn in turns:
                revision_id = rollout.required_string(
                    turn,
                    "revisionId",
                    "load continuity Release Revision",
                )
                if not revision_order or revision_order[-1] != revision_id:
                    revision_order.append(revision_id)
            first_resume = acceptance.json_object(
                turns[0].get("providerResume"),
                "first rollout load Provider resume",
            )
            later_resumes = [
                acceptance.json_object(
                    turn.get("providerResume"),
                    "subsequent rollout load Provider resume",
                )
                for turn in turns[1:]
            ]
            sequence_contiguous = all(
                turns[index].get("sessionSequenceBeforeTurn")
                == turns[index - 1].get("sessionSequenceAfterTurn")
                for index in range(1, len(turns))
            )
            first_resume_valid = {
                key: first_resume.get(key)
                for key in ("requestedStrategy", "selectedStrategy", "reasonCode")
            } == {
                "requestedStrategy": "native-cursor",
                "selectedStrategy": "authoritative-history",
                "reasonCode": "cursor_absent",
            }
            later_resumes_valid = all(
                {
                    key: resume.get(key)
                    for key in ("requestedStrategy", "selectedStrategy", "reasonCode")
                }
                == {
                    "requestedStrategy": "native-cursor",
                    "selectedStrategy": "native-cursor",
                    "reasonCode": "cursor_usable",
                }
                for resume in later_resumes
            )
            marker_and_command_verified = all(
                turn.get("markerMatched") is True
                and turn.get("commandItemVerified") is True
                for turn in turns
            )
            crossed_revision = revision_order == expected_revision_order
            if crossed_revision:
                cross_revision_sessions += 1
            if (
                ordinals != expected_ordinals
                or not sequence_contiguous
                or not first_resume_valid
                or not later_resumes_valid
                or not marker_and_command_verified
                or (require_cross_revision and not crossed_revision)
            ):
                raise acceptance.AcceptanceError(
                    "runner.kubernetes_real_provider_resume_continuity_invalid",
                    "A rollout load Session did not retain authoritative-first then native-cursor continuity across immutable Revisions.",
                    {
                        "sessionId": session_id,
                        "turnOrdinals": ordinals,
                        "expectedTurnOrdinals": expected_ordinals,
                        "revisionOrder": revision_order,
                        "expectedRevisionOrder": expected_revision_order,
                        "sequenceContiguous": sequence_contiguous,
                        "firstResumeValid": first_resume_valid,
                        "laterResumesValid": later_resumes_valid,
                        "markerAndCommandVerified": marker_and_command_verified,
                    },
                )
            session_evidence.append(
                {
                    "sessionId": session_id,
                    "turnCount": len(turns),
                    "turnOrdinals": ordinals,
                    "revisionOrder": revision_order,
                    "crossRevision": crossed_revision,
                    "sequenceContiguous": True,
                    "firstTurnProviderResume": dict(first_resume),
                    "subsequentTurnsNativeCursorVerified": later_resumes_valid,
                    "markerAndCommandVerified": True,
                    "sequenceRange": {
                        "first": turns[0].get("sessionSequenceBeforeTurn"),
                        "last": turns[-1].get("sessionSequenceAfterTurn"),
                    },
                }
            )
        if require_cross_revision and cross_revision_sessions != len(session_ids):
            raise acceptance.AcceptanceError(
                "runner.kubernetes_real_provider_cross_revision_continuity_missing",
                "Not every rollout load Session crossed candidate promotion into baseline rollback.",
                {
                    "sessionCount": len(session_ids),
                    "crossRevisionSessionCount": cross_revision_sessions,
                },
            )
        return {
            "sessionCount": len(session_ids),
            "expectedRevisionOrder": expected_revision_order,
            "crossRevisionSessionCount": cross_revision_sessions,
            "allSessionsCrossedRevision": cross_revision_sessions == len(session_ids),
            "firstTurnResume": {
                "selectedStrategy": "authoritative-history",
                "reasonCode": "cursor_absent",
            },
            "subsequentTurnResume": {
                "selectedStrategy": "native-cursor",
                "reasonCode": "cursor_usable",
            },
            "sessions": session_evidence,
        }

    def _complete_real_provider_release_load_turn(
        self,
        load_turn: Mapping[str, Any],
        *,
        revision_id: str,
        channel: str,
        manifest_id: str,
        expected_resume_strategy: str,
        expected_resume_reason: str,
    ) -> dict[str, Any]:
        session_id = str(load_turn["sessionId"])
        turn = acceptance.json_object(load_turn.get("turn"), "real Provider rollout load Turn")
        interaction = acceptance.json_object(
            load_turn.get("interaction"),
            "real Provider rollout load interaction",
        )
        active = acceptance.json_object(
            load_turn.get("active"),
            "real Provider rollout load active evidence",
        )
        marker = rollout.required_string(load_turn, "marker", "real Provider rollout load marker")
        expected_command = acceptance.real_provider_approval_command(marker)
        resolution = self._resolve_real_provider_command_turn(
            turn=turn,
            interaction=interaction,
            session_id=session_id,
            marker=marker,
            expected_command=expected_command,
            expected_resume_strategy=expected_resume_strategy,
            expected_resume_reason=expected_resume_reason,
        )
        turn_id = self._turn_id(turn, "real Provider rollout load Turn")
        release = self._wait_execution_release(
            turn_id,
            revision_id=revision_id,
            channel=channel,
            manifest_id=manifest_id,
            terminal=True,
            session_id=session_id,
        )
        rollout.validate_release_load_identity(active, release)
        if (
            release.get("executionId") != resolution.get("executionId")
            or active.get("workerId") != resolution.get("workerId")
            or active.get("generation") != resolution.get("generation")
        ):
            raise acceptance.AcceptanceError(
                "runner.kubernetes_rollout_terminal_identity_mismatch",
                "Approval resolution and terminal release evidence identified different Execution fencing.",
                {
                    "expectedExecutionId": resolution.get("executionId"),
                    "actualExecutionId": release.get("executionId"),
                    "activeWorkerId": active.get("workerId"),
                    "terminalWorkerId": resolution.get("workerId"),
                    "activeGeneration": active.get("generation"),
                    "terminalGeneration": resolution.get("generation"),
                },
            )
        session_sequence_before_turn = load_turn.get("sessionSequenceBeforeTurn")
        if not isinstance(session_sequence_before_turn, int) or session_sequence_before_turn < 0:
            raise acceptance.AcceptanceError(
                "runner.real_provider_load_sequence_anchor_missing",
                "A real Provider rollout load Turn did not retain its prior Session sequence anchor.",
                {"sessionId": session_id, "turnId": turn_id},
            )
        session_events = self._all_events(session_id=session_id)
        session_sequences = [
            int(event["sequence"])
            for event in session_events
            if isinstance(event.get("sequence"), int)
        ]
        expected_session_sequences = (
            list(range(1, session_sequences[-1] + 1)) if session_sequences else []
        )
        provider_turn = acceptance.json_object(
            resolution.get("providerTurn"),
            "real Provider rollout load provider Turn",
        )
        provider_turn_sequence_range = acceptance.json_object(
            provider_turn.get("sequenceRange"),
            "real Provider rollout load provider Turn sequence range",
        )
        if (
            not session_sequences
            or session_sequences != expected_session_sequences
            or provider_turn_sequence_range.get("first") != session_sequence_before_turn + 1
            or provider_turn_sequence_range.get("last") != session_sequences[-1]
        ):
            raise acceptance.AcceptanceError(
                "runner.real_provider_load_session_sequence_not_advanced",
                "The real Provider rollout load Turn did not advance Session history contiguously.",
                {
                    "sessionId": session_id,
                    "turnId": turn_id,
                    "sessionSequenceBeforeTurn": session_sequence_before_turn,
                    "turnSequenceRange": provider_turn_sequence_range,
                    "sessionSequenceAfterTurn": session_sequences[-1] if session_sequences else None,
                },
            )
        target_terminal = self.driver.observe_terminal_execution(
            self._required("target_id"),
            rollout.required_string(resolution, "executionId", "rollout load Execution"),
        )
        turn_started_monotonic = load_turn.get("turnStartedMonotonic")
        if not isinstance(turn_started_monotonic, (int, float)):
            raise acceptance.AcceptanceError(
                "runner.real_provider_load_latency_missing",
                "A real Provider rollout load Turn did not retain its monotonic start time for latency accounting.",
                {"sessionId": session_id, "turnId": turn_id},
            )
        return {
            **dict(resolution),
            "sessionId": session_id,
            "provider": self.options.provider,
            "turnOrdinal": load_turn.get("turnOrdinal"),
            "providerResume": resolution.get("providerResume"),
            "markerMatched": resolution.get("markerMatched") is True,
            "commandItemVerified": isinstance(resolution.get("commandItem"), Mapping),
            "sessionSequenceBeforeTurn": session_sequence_before_turn,
            "sessionSequenceAfterTurn": session_sequences[-1],
            "turnCompletionLatencyMs": acceptance.elapsed_ms(turn_started_monotonic),
            "targetTerminal": dict(target_terminal) if target_terminal else None,
            "release": release,
        }

    def _require_distinct_nodes(
        self,
        stage: str,
        pods: Sequence[Mapping[str, Any]],
    ) -> dict[str, Any]:
        pod_names = sorted(
            {
                str(pod.get("podName"))
                for pod in pods
                if isinstance(pod.get("podName"), str) and pod.get("podName")
            }
        )
        node_names = sorted(
            {
                str(pod.get("nodeName"))
                for pod in pods
                if isinstance(pod.get("nodeName"), str) and pod.get("nodeName")
            }
        )
        if (
            len(pods) != acceptance.REAL_PROVIDER_LOAD_CONCURRENCY
            or len(pod_names) != acceptance.REAL_PROVIDER_LOAD_CONCURRENCY
            or len(node_names) != acceptance.REAL_PROVIDER_LOAD_CONCURRENCY
        ):
            raise acceptance.AcceptanceError(
                "runner.kubernetes_real_provider_distinct_nodes_missing",
                "The Kubernetes rollout evidence did not hold two active release-pinned Pods on different Worker nodes.",
                {
                    "stage": stage,
                    "nodeNames": node_names,
                    "podNames": pod_names,
                    "activeCount": len(pods),
                },
            )
        inventory = self.driver.schedulable_worker_node_inventory()
        inventory_by_name = {str(node.get("name")): node for node in inventory}
        observed_nodes = [inventory_by_name.get(name) for name in node_names]
        invalid_nodes = [
            {
                "name": name,
                "present": node is not None,
                "ready": node.get("ready") if isinstance(node, Mapping) else None,
                "unschedulable": (
                    node.get("unschedulable") if isinstance(node, Mapping) else None
                ),
                "controlPlane": node.get("controlPlane") if isinstance(node, Mapping) else None,
                "taintCount": (
                    len(node.get("taints", []))
                    if isinstance(node, Mapping) and isinstance(node.get("taints"), list)
                    else None
                ),
                "schedulableWorker": (
                    node.get("schedulableWorker") if isinstance(node, Mapping) else None
                ),
            }
            for name, node in zip(node_names, observed_nodes, strict=True)
            if not isinstance(node, Mapping) or node.get("schedulableWorker") is not True
        ]
        if invalid_nodes:
            raise acceptance.AcceptanceError(
                "runner.kubernetes_real_provider_schedulable_worker_nodes_invalid",
                "A release-pinned Pod ran outside the Ready, untainted, non-control-plane Worker inventory.",
                {"stage": stage, "invalidNodes": invalid_nodes},
            )
        return {
            "stage": stage,
            "nodeNames": node_names,
            "podNames": pod_names,
            "podCount": len(pod_names),
            "nodeCount": len(node_names),
            "nodes": [
                {
                    "name": node.get("name"),
                    "ready": node.get("ready"),
                    "unschedulable": node.get("unschedulable"),
                    "controlPlane": node.get("controlPlane"),
                    "taintCount": len(node.get("taints", [])),
                    "schedulableWorker": node.get("schedulableWorker"),
                }
                for node in observed_nodes
                if isinstance(node, Mapping)
            ],
            "schedulableWorkerInventoryMatched": True,
        }

    def _history_audit_outbox(self) -> Mapping[str, Any]:
        tenant_id = self._required("tenant_id")
        target_id = self._required("target_id")
        overview = acceptance.json_object(
            self.api.request("GET", rollout.release_base_path(tenant_id, target_id)),
            "final Worker Release overview",
        )
        history = rollout.validate_release_overview(
            overview,
            baseline_revision_id=self.revisions["baseline"]["id"],
            candidate_revision_id=self.revisions["candidate"]["id"],
            baseline_digest=self.driver.images["baseline"].digest,
            candidate_digest=self.driver.images["candidate"].digest,
        )
        audits, audit_pagination = rollout.load_all_audit_logs(self.api, tenant_id)
        audit = rollout.validate_release_audit(
            audits,
            target_id=target_id,
            revision_ids={self.revisions["baseline"]["id"], self.revisions["candidate"]["id"]},
        )
        outbox_page = acceptance.json_object(
            self.api.request(
                "GET",
                f"/v1/tenants/{tenant_id}/outbox-messages"
                "?status=all&topicPrefix=worker.release.&limit=200",
            ),
            "Worker Release Outbox page",
        )
        outbox_items = outbox_page.get("items")
        if not isinstance(outbox_items, list) or not all(
            isinstance(item, dict) for item in outbox_items
        ):
            raise acceptance.AcceptanceError(
                "runner.worker_release_outbox_invalid",
                "Worker Release Outbox API returned an invalid item list.",
            )
        outbox = rollout.validate_release_outbox(
            outbox_items,
            target_id=target_id,
            revision_ids={self.revisions["baseline"]["id"], self.revisions["candidate"]["id"]},
        )
        sequence_ranges: dict[str, Mapping[str, Any]] = {}
        execution_ids: list[str] = []
        terminal_counts: dict[str, int] = {}
        for key, session_id in self.sessions.items():
            events = self._all_events(session_id=session_id)
            sequences = [int(event["sequence"]) for event in events]
            expected = list(range(1, sequences[-1] + 1)) if sequences else []
            if sequences != expected:
                raise acceptance.AcceptanceError(
                    "runner.worker_release_session_sequence_discontinuous",
                    "A Kubernetes rollout Session did not retain contiguous Event sequence.",
                    {"sessionKey": key, "sequences": sequences},
                )
            turns = [event for event in events if event.get("eventType") == "turn.created"]
            if len(turns) != 1:
                raise acceptance.AcceptanceError(
                    "runner.kubernetes_rollout_turn_count_invalid",
                    "Each Kubernetes rollout Session must contain exactly one Turn.",
                    {"sessionKey": key, "turnCount": len(turns)},
                )
            execution_id = self._event_execution_id(turns[0])
            terminals = [
                event
                for event in events
                if event.get("executionId") == execution_id
                and event.get("eventType") in acceptance.TERMINAL_EVENT_TYPES
            ]
            if len(terminals) != 1:
                raise acceptance.AcceptanceError(
                    "runner.worker_release_terminal_count_invalid",
                    "A Kubernetes rollout Execution emitted a missing or duplicate terminal event.",
                    {"sessionKey": key, "terminalCount": len(terminals)},
                )
            sequence_ranges[key] = self._sequence_range(events)
            execution_ids.append(execution_id)
            terminal_counts[execution_id] = len(terminals)
        if len(execution_ids) != 6 or len(set(execution_ids)) != 6:
            raise acceptance.AcceptanceError(
                "runner.kubernetes_rollout_execution_count_invalid",
                "The rollout did not retain six distinct seed and release Executions.",
                {"executionCount": len(execution_ids), "distinctCount": len(set(execution_ids))},
            )
        if len(self.completed_release_executions) != 4:
            raise acceptance.AcceptanceError(
                "runner.kubernetes_rollout_release_execution_count_invalid",
                "The rollout did not complete exactly four release-pinned Executions.",
                {"count": len(self.completed_release_executions)},
            )
        total_load_executions = sum(self.release_load_phase_counts.values())
        if (
            total_load_executions <= 0
            or len(self.release_load_execution_ids) != total_load_executions
            or len(self.release_load_worker_ids) != total_load_executions
            or any(count <= 0 for count in self.release_load_phase_counts.values())
        ):
            raise acceptance.AcceptanceError(
                "runner.kubernetes_rollout_load_history_invalid",
                "The Kubernetes rollout load history omitted or reused a release-pinned Execution.",
                {
                    "distinctExecutionCount": len(self.release_load_execution_ids),
                    "distinctWorkerCount": len(self.release_load_worker_ids),
                    "phaseCounts": dict(self.release_load_phase_counts),
                    "phaseMetrics": {
                        phase: {
                            "wavesCompleted": metric.get("wavesCompleted"),
                            "durationMs": metric.get("durationMs"),
                        }
                        for phase, metric in self.load_phase_metrics.items()
                    },
                },
            )
        resume_continuity = self._real_provider_load_resume_continuity(
            require_cross_revision=True
        )
        return {
            "overview": history,
            "audit": {**audit, "pagination": audit_pagination},
            "outbox": outbox,
            "sessionEvents": {
                "sequenceRanges": sequence_ranges,
                "executionCount": len(execution_ids),
                "distinctExecutionCount": len(set(execution_ids)),
                "terminalCounts": terminal_counts,
                "releaseExecutions": self.completed_release_executions,
                "loadExecutions": {
                    "requestedMinimumWaves": self.options.load_waves,
                    "minimumDurationSeconds": self.options.load_min_duration_seconds,
                    "distinctExecutionCount": len(self.release_load_execution_ids),
                    "distinctWorkerCount": len(self.release_load_worker_ids),
                    "phaseCounts": dict(self.release_load_phase_counts),
                    "phaseMetrics": self.load_phase_metrics,
                    "sessionResumeContinuity": resume_continuity,
                },
                "duplicateTerminal": False,
                "doubleExecution": False,
            },
        }


def sensitive_environment_name_specs(options: GateOptions) -> tuple[tuple[str, str], ...]:
    candidates = (
        ("credential", options.real_provider_credential_env),
        ("base-url", options.real_provider_base_url_env),
        ("model", options.real_provider_model_env),
    )
    categories_by_name: dict[str, list[str]] = {}
    for category, name in candidates:
        if isinstance(name, str) and name:
            categories_by_name.setdefault(name, []).append(category)
    return tuple(
        ("+".join(sorted(categories)), name)
        for name, categories in sorted(categories_by_name.items())
    )


def _redact_sensitive_environment_names(
    value: Any,
    specs: Sequence[tuple[str, str]],
) -> Any:
    patterns = [
        re.compile(rf"(?<![A-Za-z0-9_]){re.escape(name)}(?![A-Za-z0-9_])")
        for _category, name in sorted(specs, key=lambda item: len(item[1]), reverse=True)
    ]

    def redact_text(text: str) -> str:
        for pattern in patterns:
            text = pattern.sub("[REDACTED_ENVIRONMENT_NAME]", text)
        return text

    if isinstance(value, str):
        return redact_text(value)
    if isinstance(value, list):
        return [_redact_sensitive_environment_names(item, specs) for item in value]
    if isinstance(value, tuple):
        return [_redact_sensitive_environment_names(item, specs) for item in value]
    if isinstance(value, dict):
        return {
            redact_text(str(key)): _redact_sensitive_environment_names(item, specs)
            for key, item in value.items()
        }
    return value


def output_sensitive_environment_name_scan_case(
    output_dir: pathlib.Path,
    specs: Sequence[tuple[str, str]],
) -> dict[str, Any]:
    started_at = acceptance.utc_now()
    started = time.monotonic()
    allowed_suffixes = {".json", ".log", ".md", ".txt", ".yaml", ".yml"}
    compiled = [
        (
            category,
            re.compile(
                rb"(?<![A-Za-z0-9_])"
                + re.escape(name.encode("utf-8"))
                + rb"(?![A-Za-z0-9_])"
            ),
        )
        for category, name in specs
    ]
    overlap_bytes = max([64, *(len(name.encode("utf-8")) + 2 for _category, name in specs)])
    findings: list[dict[str, Any]] = []
    scanned_files = 0
    scanned_bytes = 0
    for path in sorted(output_dir.rglob("*")):
        if path.is_symlink() or not path.is_file() or path.suffix.lower() not in allowed_suffixes:
            continue
        scanned_files += 1
        scanned_bytes += path.stat().st_size
        seen: set[str] = set()
        carry = b""
        offset = 0
        with path.open("rb") as source:
            while True:
                chunk = source.read(1 << 20)
                if not chunk:
                    break
                window = carry + chunk
                window_offset = max(0, offset - len(carry))
                for category, pattern in compiled:
                    match = pattern.search(window)
                    if match is not None and category not in seen:
                        findings.append(
                            {
                                "file": str(path.relative_to(output_dir)),
                                "category": category,
                                "offset": window_offset + match.start(),
                            }
                        )
                        seen.add(category)
                carry = window[-overlap_bytes:]
                offset += len(chunk)
    passed = not findings
    case: dict[str, Any] = {
        "id": "security.output-sensitive-environment-name-scan",
        "name": "Scan final rollout outputs for controlled Credential, Base URL, and model environment variable names",
        "status": "pass" if passed else "fail",
        "startedAt": started_at,
        "finishedAt": acceptance.utc_now(),
        "durationMs": acceptance.elapsed_ms(started),
        "evidence": {
            "scannedFiles": scanned_files,
            "scannedBytes": scanned_bytes,
            "fileTypes": sorted(allowed_suffixes),
            "sensitiveEnvironmentNameCount": len(specs),
            "categories": sorted(category for category, _name in specs),
            "findings": findings,
            "rawEnvironmentNamesPersistedInEvidence": False,
        },
    }
    if not passed:
        case.update(
            {
                "reasonCode": "runner.output_sensitive_environment_name_detected",
                "message": "Acceptance output contained a controlled environment variable name.",
            }
        )
    return case


def markdown_from_report(report: Mapping[str, Any]) -> str:
    lines = [
        "# Stage 3 Kubernetes Real Provider Worker Release Rollout Gate",
        "",
        f"- Schema: `{report.get('schemaVersion')}`",
        f"- Run: `{report.get('runId')}`",
        f"- Status: **{report.get('status')}**",
        f"- Git SHA: `{report.get('source', {}).get('gitSha')}`",
        f"- Provider: `{report.get('provider')}`",
        f"- Model: `{report.get('configuration', {}).get('realProvider', {}).get('model')}`",
        f"- Duration: `{report.get('durationMs')} ms`",
        "",
        "## Evidence boundary",
        "",
        "This gate proves one clean SHA, two exact immutable Worker digests, real controlled Provider Credentials, "
        "multi-node Kind rollout canary/promotion/rollback, real pending Approval command turns, release-pinned "
        "Kubernetes Pod/Worker fencing, distinct-node overlap evidence, bounded real-provider quota rejection and "
        "slot reuse, Audit/Outbox history, exact cleanup, and output secret scan. It does not replace production "
        "Registry TLS/auth/retention, production admission/KMS/tlog policy, or cloud-provider eviction/CNI proof.",
        "",
        "## Cases",
        "",
        "| Case | Status | Duration | Reason |",
        "| --- | --- | ---: | --- |",
    ]
    for case in report.get("cases", []):
        if not isinstance(case, dict):
            continue
        reason = str(case.get("reasonCode") or case.get("message") or "").replace("|", "\\|").replace("\n", " ")
        lines.append(
            f"| `{case.get('id', '')}` | {case.get('status', '')} | {case.get('durationMs', 0)} ms | {reason} |"
        )
    lines.extend(["", "## Evidence", ""])
    for case in report.get("cases", []):
        if not isinstance(case, dict):
            continue
        lines.extend([f"### {case.get('id', '')}", ""])
        if case.get("message"):
            lines.extend([str(case["message"]), ""])
        if case.get("evidence"):
            lines.extend(
                [
                    "```json",
                    json.dumps(case["evidence"], indent=2, sort_keys=True, ensure_ascii=False),
                    "```",
                    "",
                ]
            )
    return "\n".join(lines).rstrip() + "\n"


def write_report(
    report: Mapping[str, Any],
    output_dir: pathlib.Path,
    redactor: acceptance.SecretRedactor,
    sensitive_environment_names: Sequence[tuple[str, str]],
) -> tuple[pathlib.Path, pathlib.Path]:
    output_dir.mkdir(parents=True, exist_ok=True)
    sanitized = _redact_sensitive_environment_names(
        redactor.value(dict(report)),
        sensitive_environment_names,
    )
    json_path = output_dir / JSON_REPORT_NAME
    markdown_path = output_dir / MARKDOWN_REPORT_NAME
    json_path.write_text(
        json.dumps(sanitized, indent=2, sort_keys=True, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )
    markdown_path.write_text(markdown_from_report(sanitized), encoding="utf-8")
    return json_path, markdown_path


def failure_case(error: Exception, *, started_at: str, started: float) -> dict[str, Any]:
    if isinstance(error, release_gate.ReleaseGateError):
        code = error.code
        evidence = error.evidence
        message = error.message
    elif isinstance(error, acceptance.AcceptanceError):
        code = error.code
        evidence = error.evidence
        message = str(error)
    else:
        code = "runner.internal_error"
        evidence = {"exceptionType": error.__class__.__name__}
        message = str(error) or error.__class__.__name__
    return {
        "id": "environment.preflight",
        "name": "Require a clean source and initialize the Kubernetes real Provider rollout gate",
        "status": "fail",
        "startedAt": started_at,
        "finishedAt": acceptance.utc_now(),
        "durationMs": acceptance.elapsed_ms(started),
        "reasonCode": code,
        "message": message,
        "evidence": evidence,
    }


def build_report(
    gate_options: GateOptions,
    options: acceptance.RunnerOptions,
    *,
    run_id: str,
    source: Mapping[str, Any],
    started_at: str,
    started: float,
    cases: Sequence[Mapping[str, Any]],
    driver: KubernetesRealProviderReleaseRolloutDriver | None,
) -> dict[str, Any]:
    operator_approved_sla_report = acceptance.operator_approved_sla_report_from_cases(
        options.suite,
        options.operator_approved_sla,
        cases,
    )
    operator_approved_sla_source = acceptance.operator_approved_sla_file_report(
        options.operator_approved_sla_file,
        options.operator_approved_sla,
    )
    return {
        "schemaVersion": SCHEMA_VERSION,
        "runId": run_id,
        "mode": "kubernetes-real-provider-worker-release-rollout",
        "target": "kubernetes",
        "provider": gate_options.provider,
        "source": source,
        "startedAt": started_at,
        "finishedAt": acceptance.utc_now(),
        "durationMs": acceptance.elapsed_ms(started),
        "status": acceptance.aggregate_status(cases),
        "configuration": {
            "timeoutSeconds": gate_options.timeout_seconds,
            "skipControlPlaneBuild": gate_options.skip_build,
            "dockerSocketPath": str(gate_options.docker_socket_path),
            "kubernetesControlPlaneHost": gate_options.kubernetes_control_plane_host,
            "kindBinary": gate_options.kind_bin,
            "kindNodeImage": gate_options.kind_node_image,
            "kindWorkerNodes": gate_options.kind_worker_nodes,
            "registryImage": gate_options.registry_image,
            "registryAuthentication": False,
            "registryTLS": False,
            "mainMaxActivePods": 2,
            "observerMaxActivePods": 1,
            "canaryPercent": 100,
            "resourceProfile": dict(
                acceptance.KUBERNETES_ACCEPTANCE_RESOURCE_CONFIGURATION
            ),
            "broadCleanupAllowed": False,
            "workerTiming": {
                "leaseTTL": options.worker_lease_ttl,
                "heartbeatTimeout": options.worker_heartbeat_timeout,
            },
            "realProvider": {
                "credentialField": gate_options.real_provider_credential_field,
                "baseUrlConfigured": gate_options.real_provider_base_url_env is not None,
                "model": gate_options.real_provider_model,
                "productCredentialEnvironmentNamePersisted": False,
                "sensitiveEnvironmentNameReportRedactionEnforced": True,
                "sensitiveEnvironmentNameOutputScanEnforced": True,
                "payloadPersistedInReport": False,
            },
            "realProviderLoad": {
                "workers": acceptance.REAL_PROVIDER_LOAD_CONCURRENCY,
                "sessions": acceptance.REAL_PROVIDER_LOAD_SESSIONS,
                "minimumWavesRequested": gate_options.load_waves,
                "minimumDurationSeconds": options.load_min_duration_seconds,
                "maximumWaves": options.load_max_waves,
                "latencyPercentiles": [50, 95, 99],
                "unexpectedErrorRateRecorded": True,
                "operatorApprovedSlaThresholdsEnforced": (
                    bool(operator_approved_sla_report.get("enforced"))
                    if isinstance(operator_approved_sla_report, Mapping)
                    else False
                ),
                "operatorApprovedSla": operator_approved_sla_report,
                "operatorApprovedSlaFile": operator_approved_sla_source,
            },
            "releaseImages": (
                {
                    slot: rollout.release_image_evidence(image)
                    for slot, image in driver.images.items()
                }
                if driver is not None and driver.images
                else None
            ),
        },
        "cases": list(cases),
        "artifacts": {
            "jsonReport": str(gate_options.output_dir / JSON_REPORT_NAME),
            "markdownReport": str(gate_options.output_dir / MARKDOWN_REPORT_NAME),
            "logsDirectory": str(gate_options.output_dir / "logs"),
        },
    }


def main(argv: Sequence[str] | None = None) -> int:
    gate_options = parse_args(argv if argv is not None else sys.argv[1:])
    gate_options.output_dir.mkdir(parents=True, exist_ok=True)
    redactor = acceptance.SecretRedactor()
    deadline = acceptance.Deadline(gate_options.timeout_seconds)
    started_at = acceptance.utc_now()
    started = time.monotonic()
    run_id = f"stage3-kubernetes-real-provider-worker-release-rollout-{uuid.uuid4()}"
    source: Mapping[str, Any] = acceptance.repository_metadata(gate_options.repo_root)
    cases: list[dict[str, Any]] = []
    driver: KubernetesRealProviderReleaseRolloutDriver | None = None
    suite: KubernetesRealProviderReleaseRolloutSuite | None = None
    child_options = runner_options(gate_options)
    sensitive_environment_names = sensitive_environment_name_specs(gate_options)
    try:
        release_gate.repository_state(gate_options.repo_root)
        if os.name != "posix":
            raise acceptance.AcceptanceError(
                "runner.platform_unsupported",
                "The Kubernetes real Provider Worker Release rollout gate requires POSIX process groups.",
            )
        driver = KubernetesRealProviderReleaseRolloutDriver(
            gate_options,
            child_options,
            deadline,
            redactor,
        )
        suite = KubernetesRealProviderReleaseRolloutSuite(
            child_options,
            driver,
            deadline,
            redactor,
        )
        driver.install_signal_handlers()
        try:
            suite.run()
        except acceptance.RunnerInterrupted as error:
            suite.record_interruption(error)
        finally:
            driver.suppress_signals_for_cleanup()
            try:
                cleanup = driver.cleanup()
                suite.record_cleanup_success(cleanup)
            except acceptance.AcceptanceError as error:
                suite.record_cleanup_failure(error)
            except Exception as error:
                suite.record_cleanup_failure(
                    acceptance.AcceptanceError(
                        "runner.kubernetes_real_provider_worker_release_rollout_cleanup_failed",
                        "Kubernetes real Provider Worker rollout cleanup raised an unexpected error.",
                        {"exceptionType": error.__class__.__name__},
                    )
                )
            finally:
                driver.restore_signal_handlers()
        cases = suite.cases
    except Exception as error:
        if driver is not None and suite is None:
            try:
                driver.cleanup()
            except Exception as cleanup_error:
                error = acceptance.AcceptanceError(
                    "runner.preflight_cleanup_failed",
                    "Kubernetes real Provider rollout initialization failed and its exact cleanup also failed.",
                    {
                        "initialExceptionType": error.__class__.__name__,
                        "cleanupExceptionType": cleanup_error.__class__.__name__,
                    },
                )
        cases = [failure_case(error, started_at=started_at, started=started)]
    report = build_report(
        gate_options,
        child_options,
        run_id=run_id,
        source=source,
        started_at=started_at,
        started=started,
        cases=cases,
        driver=driver,
    )
    write_report(
        report,
        gate_options.output_dir,
        redactor,
        sensitive_environment_names,
    )
    secret_scan = acceptance.output_secret_scan_case(gate_options.output_dir, redactor)
    environment_name_scan = output_sensitive_environment_name_scan_case(
        gate_options.output_dir,
        sensitive_environment_names,
    )
    cases.extend([secret_scan, environment_name_scan])
    report = build_report(
        gate_options,
        child_options,
        run_id=run_id,
        source=source,
        started_at=started_at,
        started=started,
        cases=cases,
        driver=driver,
    )
    json_path, markdown_path = write_report(
        report,
        gate_options.output_dir,
        redactor,
        sensitive_environment_names,
    )
    print(f"Stage 3 Kubernetes real Provider Worker Release rollout: {report['status']}")
    print(f"JSON: {json_path}")
    print(f"Markdown: {markdown_path}")
    return 0 if report["status"] == "pass" else 1


if __name__ == "__main__":
    raise SystemExit(main())
