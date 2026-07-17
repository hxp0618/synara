from __future__ import annotations

import contextlib
import base64
import dataclasses
import hashlib
import io
import json
import os
import pathlib
import shutil
import sqlite3
import subprocess
import tarfile
import tempfile
import unittest
import urllib.error
import urllib.request
from collections.abc import Callable, Mapping, Sequence
from typing import Any
from unittest import mock

import acceptance_runner as acceptance


REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]


def runner_options(*, restart_control_plane: bool = True) -> acceptance.RunnerOptions:
    return acceptance.RunnerOptions(
        target="fake",
        provider="codex",
        suite="fixture",
        output_dir=pathlib.Path(tempfile.gettempdir()) / "synara-stage3-acceptance-runner-tests",
        timeout_seconds=30.0,
        runner_command=("fixture",),
        skip_build=True,
        control_plane_binary=pathlib.Path("/tmp/fake-control-plane"),
        keep=False,
        restart_control_plane=restart_control_plane,
        soak_turns=0,
        soak_restart_every=0,
        load_waves=0,
        ssh_orbctl_bin="orbctl",
        ssh_machine_name=None,
        ssh_machine_arch="arm64",
        ssh_machine_image="ubuntu:24.04",
        ssh_node_version="24.13.1",
        ssh_external_host=None,
        ssh_external_port=22,
        ssh_external_user=None,
        ssh_external_identity_file=None,
        ssh_external_host_key_file=None,
        ssh_external_service_user=acceptance.SSH_SERVICE_USER,
        ssh_external_use_sudo=False,
        ssh_allow_external_host=False,
        docker_socket_path=pathlib.Path("/var/run/docker.sock"),
        docker_worker_image=None,
        docker_skip_worker_build=False,
        docker_control_plane_host="host.docker.internal",
        docker_network_mode=None,
        docker_memory_bytes=2 << 30,
        docker_nano_cpus=1_000_000_000,
        kubernetes_context=None,
        kubernetes_kubeconfig=None,
        kubernetes_allow_nondisposable=False,
        kubernetes_shared_local_image_store=False,
        kubernetes_worker_image=None,
        kubernetes_skip_worker_build=False,
        kubernetes_control_plane_host="host.docker.internal",
        kind_bin="kind",
        kind_cluster_name=None,
        kind_node_image="kindest/node:v1.33.1",
        failure_cases=(),
        network_outage_seconds=8.0,
        docker_allow_network_interruption=False,
        kubernetes_allow_node_drain=False,
        failure_only=False,
        real_provider_cases=(),
        real_provider_failure_cases=(),
        real_provider_credential_env=None,
        real_provider_credential_field="apiKey",
        real_provider_base_url_env=None,
    )


def real_provider_turn_events(
    assistant_text: str,
    *,
    terminal_text: str | None = None,
    selected_strategy: str,
    reason_code: str,
) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    terminal = {
        "sequence": 5,
        "eventType": "execution.completed",
        "executionId": "execution-1",
        "workerId": "worker-1",
        "generation": 1,
        **({"payload": {"output": {"text": terminal_text}}} if terminal_text is not None else {}),
    }
    return terminal, [
        {
            "sequence": 1,
            "eventType": "turn.created",
            "executionId": "execution-1",
            "payload": {"turnId": "turn-1", "executionId": "execution-1"},
        },
        {
            "sequence": 2,
            "eventType": "execution.leased",
            "executionId": "execution-1",
            "payload": {
                "providerResume": {
                    "requestedStrategy": "native-cursor",
                    "selectedStrategy": selected_strategy,
                    "reasonCode": reason_code,
                }
            },
        },
        {"sequence": 3, "eventType": "execution.started", "executionId": "execution-1"},
        {
            "sequence": 4,
            "eventVersion": 2,
            "eventType": "content.delta",
            "executionId": "execution-1",
            "payload": {"streamKind": "assistant_text", "delta": assistant_text},
        },
        terminal,
    ]


class FakeAPI:
    def __init__(self) -> None:
        self.requests: list[tuple[str, str, Mapping[str, Any] | None]] = []

    def request(
        self,
        method: str,
        path: str,
        payload: Mapping[str, Any] | None = None,
        expected: Sequence[int] = (200,),
        *,
        maximum_timeout: float = 10.0,
    ) -> Any:
        del expected, maximum_timeout
        self.requests.append((method, path, payload))
        return {"status": "resolved", "deliveryStatus": "delivered"}

    def wait_until(
        self,
        _description: str,
        probe: Callable[[], Any | None],
        interval: float = 0.25,
    ) -> Any:
        del interval
        value = probe()
        if value is None:
            raise AssertionError("probe did not become ready in fake API wait")
        return value


class FakeDriver:
    def __init__(self, lifecycle: acceptance.TargetLifecycle, *, name: str = "fake") -> None:
        self.name = name
        self.lifecycle = lifecycle
        self.api = FakeAPI()
        self.restart_calls = 0
        self.pending_interaction_recovery: str | None = None

    def prepare(self) -> Mapping[str, Any]:
        return {}

    def start(self) -> Mapping[str, Any]:
        return {}

    def restart(self) -> Mapping[str, Any]:
        self.restart_calls += 1
        return {"processGeneration": self.restart_calls + 1}

    def provision_target(
        self,
        tenant_id: str,
        organization_id: str,
        provider: str,
    ) -> Mapping[str, Any]:
        del tenant_id, provider
        return {"id": "target-id", "organizationId": organization_id, "kind": self.name}

    def replace_worker(self, tenant_id: str, target_id: str, provider: str) -> Mapping[str, Any]:
        del tenant_id, target_id, provider
        return {"replacementWorkerId": "worker-replacement"}

    def observe_execution(self, target_id: str, execution_id: str) -> Mapping[str, Any]:
        return {"targetId": target_id, "executionId": execution_id}

    def observe_terminal_execution(self, target_id: str, execution_id: str) -> Mapping[str, Any]:
        return {"targetId": target_id, "executionId": execution_id, "terminal": True}

    def recover_pending_interaction(self, target_id: str, execution_id: str) -> Mapping[str, Any]:
        del target_id, execution_id
        return {"recoveryMode": "unsupported"}

    def stop(self) -> None:
        return None

    def cleanup(self) -> None:
        return None


class CaseOrderSuite(acceptance.AcceptanceSuite):
    def __init__(
        self,
        driver: FakeDriver,
        options: acceptance.RunnerOptions | None = None,
    ) -> None:
        super().__init__(
            options or runner_options(),
            driver,
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        self.case_order: list[str] = []

    def _case(
        self,
        case_id: str,
        name: str,
        operation: Callable[[], Mapping[str, Any] | None],
        requires: Sequence[str] = (),
    ) -> None:
        del name, operation, requires
        self.case_order.append(case_id)


class FixtureSoakSuite(acceptance.AcceptanceSuite):
    def __init__(self, *, turn_count: int = 3, duplicate_execution: bool = False) -> None:
        driver = FakeDriver(acceptance.STANDING_WORKER)
        super().__init__(
            dataclasses.replace(
                runner_options(),
                suite="fixture-soak",
                soak_turns=turn_count,
                soak_restart_every=2 if turn_count > 2 else 0,
            ),
            driver,
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        self.state.session_id = "session-id"
        self.state.target_id = "target-id"
        self.events: list[dict[str, Any]] = [
            {"sequence": 1, "eventType": "session.created"}
        ]
        self.executions: dict[str, str] = {}
        self.turn_start_indexes: dict[str, int] = {}
        self.duplicate_execution = duplicate_execution
        self.restart_calls = 0

    def _create_turn(self, input_text: str) -> dict[str, Any]:
        del input_text
        turn_number = len(self.executions) + 1
        turn_id = f"turn-{turn_number}"
        execution_id = "execution-1" if self.duplicate_execution else f"execution-{turn_number}"
        self.executions[turn_id] = execution_id
        self.turn_start_indexes[turn_id] = len(self.events)
        self.events.append(
            {
                "sequence": len(self.events) + 1,
                "eventType": "turn.created",
                "executionId": execution_id,
                "payload": {"turnId": turn_id, "executionId": execution_id},
            }
        )
        return {"id": turn_id}

    def _wait_for_turn_terminal(
        self,
        turn_id: str,
        expected_event_type: str,
    ) -> tuple[dict[str, Any], list[dict[str, Any]]]:
        execution_id = self.executions[turn_id]
        ordered_event_types = [
            "execution.leased",
            "workspace.ready",
            "execution.started",
            "content.delta",
            "item.started",
            "item.completed",
            "thread.token-usage.updated",
            "workspace.dirty",
            "checkpoint.created",
            "artifact.ready",
            "checkpoint.ready",
            expected_event_type,
        ]
        for event_type in ordered_event_types:
            self.events.append(
                {
                    "sequence": len(self.events) + 1,
                    "eventType": event_type,
                    "executionId": execution_id,
                    "workerId": "worker-1",
                    "generation": 1,
                }
            )
        return self.events[-1], self.events[self.turn_start_indexes[turn_id] :]

    def _all_events(self) -> list[dict[str, Any]]:
        return list(self.events)

    def _restart_control_plane(self) -> Mapping[str, Any]:
        self.restart_calls += 1
        return {
            "processGeneration": self.restart_calls + 1,
            "previousPid": 100 + self.restart_calls,
            "preRestartSequence": len(self.events),
        }


class FixtureConcurrencyAPI(FakeAPI):
    def request(
        self,
        method: str,
        path: str,
        payload: Mapping[str, Any] | None = None,
        expected: Sequence[int] = (200,),
        *,
        maximum_timeout: float = 10.0,
    ) -> Any:
        if method == "PUT" and path.endswith("/quota"):
            self.requests.append((method, path, payload))
            return {
                "tenantId": "tenant-id",
                "maxConcurrentExecutions": acceptance.FIXTURE_CONCURRENCY_WORKERS,
                "maxArtifactBytes": None,
            }
        return super().request(
            method,
            path,
            payload,
            expected,
            maximum_timeout=maximum_timeout,
        )


class FixtureConcurrencySuite(acceptance.AcceptanceSuite):
    def __init__(
        self,
        *,
        duplicate_worker: bool = False,
        primary_provider: str = "codex",
    ) -> None:
        driver = FakeDriver(acceptance.STANDING_WORKER, name="docker")
        driver.api = FixtureConcurrencyAPI()
        super().__init__(
            dataclasses.replace(
                runner_options(),
                target="docker",
                provider=primary_provider,
                suite="fixture-concurrency",
                restart_control_plane=False,
            ),
            driver,
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        self.state.tenant_id = "tenant-id"
        self.state.organization_id = "organization-id"
        self.state.target_id = "target-id"
        self.state.project_id = "project-id"
        self.state.session_id = f"session-{primary_provider}"
        self.state.credential_id = f"credential-{primary_provider}"
        self.duplicate_worker = duplicate_worker
        self.events: dict[str, list[dict[str, Any]]] = {
            f"session-{primary_provider}": []
        }
        self.turn_sessions: dict[str, str] = {}
        self.pending: dict[str, dict[str, Any]] = {}

    def _create_fixture_credential(self, provider: str, title: str) -> dict[str, Any]:
        del title
        return {
            "id": f"credential-{provider}",
            "provider": provider,
            "credentialType": "acceptance_fixture",
        }

    def _create_project_session(
        self,
        *,
        provider: str,
        title: str,
        credential_id: str | None,
        model: str | None = None,
        description: str,
    ) -> dict[str, Any]:
        del title, model, description
        session_id = f"session-{provider}"
        self.events[session_id] = []
        return {
            "id": session_id,
            "provider": provider,
            "executionTargetId": "target-id",
            "providerCredentialId": credential_id,
        }

    def _create_turn(
        self,
        input_text: str,
        *,
        runtime_mode: str = "full-access",
        interaction_mode: str = "default",
        session_id: str | None = None,
    ) -> dict[str, Any]:
        del input_text, runtime_mode, interaction_mode
        resolved_session = session_id or self.state.session_id
        if resolved_session is None:
            raise AssertionError("missing fake Session")
        turn_id = f"turn-{len(self.turn_sessions) + 1}"
        execution_id = f"execution-{len(self.turn_sessions) + 1}"
        self.turn_sessions[turn_id] = resolved_session
        events = self.events[resolved_session]
        events.append(
            {
                "sequence": len(events) + 1,
                "eventType": "turn.created",
                "executionId": execution_id,
                "payload": {"turnId": turn_id, "executionId": execution_id},
            }
        )
        return {"id": turn_id}

    def _wait_for_interaction(
        self,
        turn_id: str,
        kind: str,
        *,
        session_id: str | None = None,
    ) -> dict[str, Any]:
        resolved_session = session_id or self.turn_sessions[turn_id]
        events = self.events[resolved_session]
        execution_id = str(events[0]["executionId"])
        worker_number = (
            1
            if self.duplicate_worker or resolved_session == self.state.session_id
            else 2
        )
        worker_id = f"worker-{worker_number}"
        for event_type in (
            "execution.leased",
            "workspace.ready",
            "execution.started",
            "request.opened",
        ):
            events.append(
                {
                    "sequence": len(events) + 1,
                    "eventType": event_type,
                    "executionId": execution_id,
                    "workerId": worker_id,
                    "generation": 1,
                }
            )
        interaction = {
            "id": f"interaction-{execution_id}",
            "turnId": turn_id,
            "kind": kind,
            "executionId": execution_id,
            "requestId": f"request-{execution_id}",
        }
        self.pending[str(interaction["id"])] = interaction
        return interaction

    def _all_events(self, *, session_id: str | None = None) -> list[dict[str, Any]]:
        resolved_session = session_id or self.state.session_id
        if resolved_session is None:
            raise AssertionError("missing fake Session")
        return list(self.events[resolved_session])

    def _interaction_pending(
        self,
        session_id: str,
        interaction: Mapping[str, Any],
    ) -> bool:
        del session_id
        return str(interaction.get("id")) in self.pending

    def _resolve_approval_turn(
        self,
        turn: Mapping[str, Any],
        interaction: Mapping[str, Any],
        *,
        session_id: str,
    ) -> dict[str, Any]:
        interaction_id = str(interaction["id"])
        if interaction_id not in self.pending:
            raise AssertionError("fake interaction was not pending")
        del self.pending[interaction_id]
        events = self.events[session_id]
        execution_id = str(interaction["executionId"])
        for event_type in ("request.resolved", "execution.completed"):
            events.append(
                {
                    "sequence": len(events) + 1,
                    "eventType": event_type,
                    "executionId": execution_id,
                    "workerId": events[-1]["workerId"],
                    "generation": 1,
                }
            )
        return {
            "turnId": turn["id"],
            "executionId": execution_id,
            "requestId": interaction["requestId"],
            "interactionId": interaction_id,
            "resolutionStatus": "resolved",
            "deliveryStatus": "delivered",
            "sequenceRange": self._sequence_range(events),
        }


class FixtureLoadSuite(FixtureConcurrencySuite):
    def __init__(
        self,
        *,
        wave_count: int = 2,
        enforce_quota: bool = True,
        duplicate_worker: bool = False,
    ) -> None:
        super().__init__(duplicate_worker=duplicate_worker)
        self.options = dataclasses.replace(
            self.options,
            suite="fixture-load",
            load_waves=wave_count,
        )
        self.enforce_quota = enforce_quota
        self.pending_workers: dict[str, str] = {}
        self.pending_generations: dict[str, int] = {}

    def _create_project_session(
        self,
        *,
        provider: str,
        title: str,
        credential_id: str | None,
        model: str | None = None,
        description: str,
    ) -> dict[str, Any]:
        del title, model, description
        base = f"session-{provider}"
        suffix = 1
        session_id = base
        while session_id in self.events:
            suffix += 1
            session_id = f"{base}-{suffix}"
        self.events[session_id] = []
        return {
            "id": session_id,
            "provider": provider,
            "executionTargetId": "target-id",
            "providerCredentialId": credential_id,
        }

    def _create_turn(
        self,
        input_text: str,
        *,
        runtime_mode: str = "full-access",
        interaction_mode: str = "default",
        session_id: str | None = None,
    ) -> dict[str, Any]:
        del input_text, runtime_mode, interaction_mode
        if self.enforce_quota and len(self.pending) >= acceptance.FIXTURE_CONCURRENCY_WORKERS:
            raise acceptance.AcceptanceError(
                "execution_quota_exceeded",
                "The tenant concurrent execution quota has been reached.",
            )
        resolved_session = session_id or self.state.session_id
        if resolved_session is None:
            raise AssertionError("missing fake load Session")
        turn_id = f"turn-{len(self.turn_sessions) + 1}"
        execution_id = f"execution-{len(self.turn_sessions) + 1}"
        self.turn_sessions[turn_id] = resolved_session
        events = self.events[resolved_session]
        events.append(
            {
                "sequence": len(events) + 1,
                "eventType": "turn.created",
                "executionId": execution_id,
                "payload": {"turnId": turn_id, "executionId": execution_id},
            }
        )
        return {"id": turn_id}

    def _wait_for_interaction(
        self,
        turn_id: str,
        kind: str,
        *,
        session_id: str | None = None,
    ) -> dict[str, Any]:
        resolved_session = session_id or self.turn_sessions[turn_id]
        events = self.events[resolved_session]
        execution_id = str(events[-1]["executionId"])
        used_workers = set(self.pending_workers.values())
        if self.duplicate_worker:
            worker_id = "worker-1"
        else:
            worker_id = next(
                (
                    candidate
                    for candidate in ("worker-1", "worker-2")
                    if candidate not in used_workers
                ),
                "worker-missing",
            )
        for event_type in (
            "execution.leased",
            "workspace.ready",
            "execution.started",
            "content.delta",
            "item.started",
            "item.completed",
            "thread.token-usage.updated",
            "artifact.ready",
            "request.opened",
        ):
            payload = (
                {"requestId": f"request-{execution_id}"}
                if event_type == "request.opened"
                else None
            )
            events.append(
                {
                    "sequence": len(events) + 1,
                    "eventType": event_type,
                    "executionId": execution_id,
                    "workerId": worker_id,
                    "generation": 1,
                    **({"payload": payload} if payload is not None else {}),
                }
            )
        interaction = {
            "id": f"interaction-{execution_id}",
            "turnId": turn_id,
            "kind": kind,
            "executionId": execution_id,
            "requestId": f"request-{execution_id}",
        }
        interaction_id = str(interaction["id"])
        self.pending[interaction_id] = interaction
        self.pending_workers[interaction_id] = worker_id
        self.pending_generations[interaction_id] = 1
        return interaction

    def _all_events(self, *, session_id: str | None = None) -> list[dict[str, Any]]:
        resolved_session = session_id or self.state.session_id
        if resolved_session is None:
            raise AssertionError("missing fake load Session")
        return [dict(event) for event in self.events[resolved_session]]

    def _pending_interactions(self, session_id: str) -> list[dict[str, Any]]:
        turn_ids = {
            turn_id
            for turn_id, resolved_session in self.turn_sessions.items()
            if resolved_session == session_id
        }
        return [
            dict(interaction)
            for interaction in self.pending.values()
            if interaction.get("turnId") in turn_ids
        ]

    def _resolve_approval_turn(
        self,
        turn: Mapping[str, Any],
        interaction: Mapping[str, Any],
        *,
        session_id: str,
    ) -> dict[str, Any]:
        interaction_id = str(interaction["id"])
        worker_id = self.pending_workers.pop(interaction_id)
        generation = self.pending_generations.pop(interaction_id)
        del self.pending[interaction_id]
        events = self.events[session_id]
        execution_id = str(interaction["executionId"])
        for event_type in (
            "workspace.dirty",
            "checkpoint.created",
            "artifact.ready",
            "checkpoint.ready",
            "request.resolved",
        ):
            events.append(
                {
                    "sequence": len(events) + 1,
                    "eventType": event_type,
                    "executionId": execution_id,
                    "workerId": worker_id,
                    "generation": generation,
                }
            )
        events.append(
            {
                "sequence": len(events) + 1,
                "eventType": "execution.completed",
                "executionId": execution_id,
                "workerId": worker_id,
                "generation": generation,
                "payload": {
                    "output": {
                        "credentialEvidence": {
                            "credentialPayloadKeys": ["acceptanceToken"],
                            "credentialVerified": True,
                        }
                    }
                },
            }
        )
        return {
            "turnId": turn["id"],
            "executionId": execution_id,
            "requestId": interaction["requestId"],
            "interactionId": interaction_id,
            "resolutionStatus": "resolved",
            "deliveryStatus": "delivered",
            "sequenceRange": self._sequence_range(events),
        }


class FixtureLoadFailureSuite(FixtureLoadSuite):
    def __init__(self, *, wave_count: int = 2) -> None:
        super().__init__(wave_count=wave_count)
        self.options = dataclasses.replace(self.options, suite="fixture-load-failure")
        self.driver.validate_failure = lambda fault: None  # type: ignore[attr-defined]
        self.driver.inject_failure = self._inject_worker_failure  # type: ignore[attr-defined]

    def _inject_worker_failure(
        self,
        fault: str,
        target_id: str,
        execution_id: str,
    ) -> Mapping[str, Any]:
        if fault not in acceptance.FIXTURE_LOAD_FAILURE_CASES or target_id != "target-id":
            raise AssertionError((fault, target_id))
        interaction_id, interaction = next(
            (
                interaction_id,
                interaction,
            )
            for interaction_id, interaction in self.pending.items()
            if interaction.get("executionId") == execution_id
        )
        turn_id = str(interaction["turnId"])
        session_id = self.turn_sessions[turn_id]
        worker_id = self.pending_workers.pop(interaction_id)
        self.pending_generations.pop(interaction_id)
        del self.pending[interaction_id]
        events = self.events[session_id]
        if fault == "provider-host-process-crash":
            events.append(
                {
                    "sequence": len(events) + 1,
                    "eventType": "execution.failed",
                    "executionId": execution_id,
                    "workerId": worker_id,
                    "generation": 1,
                    "payload": {"failureCode": "provider_unavailable"},
                }
            )
            return {
                "fault": fault,
                "executionId": execution_id,
                "executionGeneration": 1,
                "workerId": worker_id,
                "containerId": "container-1",
                "containerName": f"container-{worker_id}",
                "exactExecutionWorkerMatch": True,
                "scopedToManagedContainer": True,
                "scopedToAgentdDescendants": True,
                "broadProcessMatchUsed": False,
                "providerHostPid": 42,
            }
        events.append(
            {
                "sequence": len(events) + 1,
                "eventType": "execution.recovering",
                "executionId": execution_id,
                "workerId": worker_id,
                "generation": 1,
            }
        )
        replacement_request_id = f"request-{execution_id}-generation-2"
        for event_type in (
            "execution.leased",
            "workspace.ready",
            "execution.started",
            "request.opened",
        ):
            events.append(
                {
                    "sequence": len(events) + 1,
                    "eventType": event_type,
                    "executionId": execution_id,
                    "workerId": worker_id,
                    "generation": 2,
                    **(
                        {"payload": {"requestId": replacement_request_id}}
                        if event_type == "request.opened"
                        else {}
                    ),
                }
            )
        replacement = {
            "id": f"interaction-{execution_id}-generation-2",
            "turnId": turn_id,
            "kind": "approval",
            "executionId": execution_id,
            "requestId": replacement_request_id,
        }
        replacement_id = str(replacement["id"])
        self.pending[replacement_id] = replacement
        self.pending_workers[replacement_id] = worker_id
        self.pending_generations[replacement_id] = 2
        evidence: dict[str, Any] = {
            "fault": fault,
            "executionId": execution_id,
            "executionGeneration": 1,
            "workerId": worker_id,
            "containerId": "container-1",
            "containerName": f"container-{worker_id}",
            "exactExecutionWorkerMatch": True,
        }
        if fault == "worker-network":
            evidence["restored"] = True
        elif fault == "worker-container-loss":
            evidence.update(
                {
                    "removedContainerId": "container-old",
                    "replacementContainerId": "container-new",
                    "containerIdChanged": True,
                    "workerIdStable": True,
                    "previousWorkerIncarnation": 1,
                    "replacementWorkerIncarnation": 2,
                    "workerIncarnationAdvanced": True,
                    "instanceUidChanged": True,
                    "replacementReady": True,
                    "namedVolumeContinuity": {
                        "preservedAcrossReplacement": True,
                    },
                }
            )
        return evidence

    def _wait_for_replacement_interaction(
        self,
        turn_id: str,
        kind: str,
        previous_interaction_id: str,
        *,
        session_id: str | None = None,
    ) -> dict[str, Any]:
        resolved_session_id = session_id or self.state.session_id
        if resolved_session_id is None:
            raise AssertionError("missing fake Session")
        if previous_interaction_id in self.pending:
            raise AssertionError("stale interaction remained pending")
        matches = [
            interaction
            for interaction in self._pending_interactions(resolved_session_id)
            if interaction.get("turnId") == turn_id
            and interaction.get("kind") == kind
            and interaction.get("id") != previous_interaction_id
        ]
        if len(matches) != 1:
            raise AssertionError(matches)
        return matches[0]


class BarrierSuite(acceptance.AcceptanceSuite):
    def __init__(self, lifecycle: acceptance.TargetLifecycle) -> None:
        self.fake_driver = FakeDriver(lifecycle)
        super().__init__(
            runner_options(),
            self.fake_driver,
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        self.state.tenant_id = "tenant-id"
        self.state.organization_id = "organization-id"
        self.state.target_id = "target-id"
        self.state.session_id = "session-id"
        self.created_turns: list[str] = []
        self.interaction_waits = 0
        self.post_restart_waits = 0

    def _create_turn(self, input_text: str) -> dict[str, Any]:
        self.created_turns.append(input_text)
        return {"id": f"turn-{len(self.created_turns)}"}

    def _wait_for_interaction(self, turn_id: str, kind: str) -> dict[str, Any]:
        self.interaction_waits += 1
        return {
            "id": "interaction-1",
            "turnId": turn_id,
            "kind": kind,
            "executionId": "execution-1",
            "requestId": "approval-request-1",
        }

    def _wait_compatible_manifest(self, target_id: str) -> dict[str, Any]:
        if target_id != "target-id":
            raise AssertionError(f"unexpected target ID: {target_id}")
        return {
            "manifest": {
                "manifestId": "manifest-1",
                "workerStatusCounts": {"online": 1},
                "workerProtocol": {"minimum": 2, "maximum": 2},
                "runtimeEvent": {"minimum": 2, "maximum": 2},
                "workerBuild": {"gitSha": "abcdef0"},
            },
            "provider": {
                "provider": "codex",
                "supportTier": "experimental",
                "compatibilityStatus": "compatible",
                "runtime": {"available": True, "compatible": True},
                "releasePolicy": {"enabled": True},
            },
        }

    def _wait_for_turn_terminal(
        self,
        turn_id: str,
        expected_event_type: str,
    ) -> tuple[dict[str, Any], list[dict[str, Any]]]:
        terminal = {
            "sequence": 2,
            "eventType": expected_event_type,
            "executionId": "execution-1",
            "workerId": "worker-1",
            "generation": 1,
        }
        return terminal, [
            {"sequence": 1, "eventType": "request.resolved", "executionId": "execution-1"},
            terminal,
        ]

    def _all_events(self) -> list[dict[str, Any]]:
        return [{"sequence": 1, "eventType": "execution.completed"}]

    def _wait_post_restart_online_worker(self, target_id: str) -> dict[str, Any]:
        self.post_restart_waits += 1
        if target_id != "target-id":
            raise AssertionError(f"unexpected target ID: {target_id}")
        return {"manifestId": "manifest-after-restart", "workerStatusCounts": {"online": 1}}


class PendingApprovalRecoverySuite(BarrierSuite):
    def __init__(self, *, replacement_request_id: str = "approval-request-2") -> None:
        super().__init__(acceptance.EXECUTION_PINNED_WORKER)
        self.fake_driver.pending_interaction_recovery = "delete-pod"
        self.fake_driver.recover_pending_interaction = self._recover_pending_interaction  # type: ignore[method-assign]
        self.fake_driver.observe_execution = self._observe_execution  # type: ignore[method-assign]
        self.replacement_request_id = replacement_request_id
        self.recovered = False

    def _recover_pending_interaction(self, target_id: str, execution_id: str) -> Mapping[str, Any]:
        if target_id != "target-id":
            raise AssertionError(f"unexpected target ID: {target_id}")
        if execution_id != "execution-1":
            raise AssertionError(f"unexpected execution ID: {execution_id}")
        self.recovered = True
        return {"deletedPodUid": "pod-uid-1", "recoveryMode": "delete-pod"}

    def _wait_for_interaction(self, turn_id: str, kind: str) -> dict[str, Any]:
        self.interaction_waits += 1
        if not self.recovered:
            return {
                "id": "interaction-1",
                "turnId": turn_id,
                "kind": kind,
                "executionId": "execution-1",
                "requestId": "approval-request-1",
            }
        return {
            "id": "interaction-2",
            "turnId": turn_id,
            "kind": kind,
            "executionId": "execution-1",
            "requestId": self.replacement_request_id,
        }

    def _wait_for_replacement_interaction(
        self,
        turn_id: str,
        kind: str,
        previous_interaction_id: str,
        *,
        session_id: str | None = None,
    ) -> dict[str, Any]:
        del session_id
        if not self.recovered:
            raise AssertionError("pending interaction recovery did not run before replacement wait")
        if previous_interaction_id != "interaction-1":
            raise AssertionError(f"unexpected prior interaction ID: {previous_interaction_id}")
        return self._wait_for_interaction(turn_id, kind)

    def _all_events(self) -> list[dict[str, Any]]:
        events = [
            {
                "sequence": 1,
                "eventType": "turn.created",
                "executionId": "execution-1",
                "payload": {"turnId": "turn-1", "executionId": "execution-1"},
            },
            {
                "sequence": 2,
                "eventType": "request.opened",
                "executionId": "execution-1",
                "workerId": "worker-1",
                "generation": 1,
                "payload": {"requestId": "approval-request-1"},
            },
        ]
        if self.recovered:
            events.extend(
                [
                    {
                        "sequence": 3,
                        "eventType": "execution.recovering",
                        "executionId": "execution-1",
                        "workerId": "worker-1",
                        "generation": 1,
                        "payload": {"turnId": "turn-1", "reason": "lease_expired"},
                    },
                    {
                        "sequence": 4,
                        "eventType": "request.opened",
                        "executionId": "execution-1",
                        "workerId": "worker-2",
                        "generation": 2,
                        "payload": {"requestId": self.replacement_request_id},
                    },
                ]
            )
        return events

    def _wait_for_turn_terminal(
        self,
        turn_id: str,
        expected_event_type: str,
    ) -> tuple[dict[str, Any], list[dict[str, Any]]]:
        terminal = {
            "sequence": 3,
            "eventType": expected_event_type,
            "executionId": "execution-1",
            "workerId": "worker-2",
            "generation": 2,
        }
        return terminal, [
            {
                "sequence": 1,
                "eventType": "turn.created",
                "executionId": "execution-1",
                "payload": {"turnId": turn_id, "executionId": "execution-1"},
            },
            {"sequence": 2, "eventType": "request.resolved", "executionId": "execution-1"},
            terminal,
        ]

    def _observe_execution(self, target_id: str, execution_id: str) -> Mapping[str, Any]:
        del target_id, execution_id
        return {"podUid": "pod-uid-2", "generation": "2"}


class ProviderFailureSuite(BarrierSuite):
    def __init__(self, actual_failure_code: str) -> None:
        super().__init__(acceptance.STANDING_WORKER)
        self.actual_failure_code = actual_failure_code

    def _wait_for_turn_terminal(
        self,
        turn_id: str,
        expected_event_type: str,
    ) -> tuple[dict[str, Any], list[dict[str, Any]]]:
        execution_id = f"execution-{turn_id}"
        terminal = {
            "sequence": 2,
            "eventType": expected_event_type,
            "executionId": execution_id,
            "workerId": "worker-1",
            "generation": 1,
            "payload": {
                "failureCode": self.actual_failure_code,
            }
            if expected_event_type == "execution.failed"
            else {},
        }
        return terminal, [
            {
                "sequence": 1,
                "eventType": "turn.created",
                "executionId": execution_id,
                "payload": {"turnId": turn_id},
            },
            terminal,
        ]


class TerminalLargeAPI(FakeAPI):
    def __init__(self, artifacts: Sequence[Mapping[str, Any]]) -> None:
        super().__init__()
        self.artifacts = list(artifacts)

    def request(
        self,
        method: str,
        path: str,
        payload: Mapping[str, Any] | None = None,
        expected: Sequence[int] = (200,),
        *,
        maximum_timeout: float = 10.0,
    ) -> Any:
        if method == "GET" and path == "/v1/sessions/session-id/artifacts":
            return {"items": self.artifacts}
        return super().request(method, path, payload, expected, maximum_timeout=maximum_timeout)


class TerminalLargeSuite(acceptance.AcceptanceSuite):
    def __init__(self, *, leak_runtime_path: bool = False, corrupt_artifact: bool = False) -> None:
        expected_segments = acceptance.terminal_large_expected_segments()
        artifact_ids = [
            f"00000000-0000-4000-8000-{segment['segmentIndex'] + 1:012d}"
            for segment in expected_segments
        ]
        artifacts = [
            {
                "id": artifact_id,
                "executionId": "execution-terminal-large",
                "kind": "terminal_log",
                "status": "ready",
                "originalName": f"terminal-log-{segment['segmentIndex'] + 1:06d}.log",
                "contentType": "text/plain; charset=utf-8",
                "sizeBytes": segment["length"],
                "sha256": "0" * 64 if corrupt_artifact and index == 0 else segment["sha256"],
            }
            for index, (artifact_id, segment) in enumerate(
                zip(artifact_ids, expected_segments, strict=True)
            )
        ]
        driver = FakeDriver(acceptance.STANDING_WORKER)
        driver.api = TerminalLargeAPI(artifacts)
        super().__init__(
            runner_options(),
            driver,
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        self.state.session_id = "session-id"
        terminal_id = "fixture-terminal-large-1"
        events: list[dict[str, Any]] = [
            {
                "sequence": 1,
                "eventType": "item.started",
                "executionId": "execution-terminal-large",
                "payload": {
                    "itemType": "command_execution",
                    "status": "inProgress",
                    "data": {
                        "provider": "codex",
                        "terminal": {
                            "terminalId": terminal_id,
                            "eventType": "terminal.started",
                            "commandSummary": "fixture-terminal-large --bytes=2097409",
                            "cwdLabel": ".",
                        },
                    },
                },
            },
            {
                "sequence": 2,
                "eventType": "content.delta",
                "executionId": "execution-terminal-large",
                "payload": {
                    "streamKind": "command_output",
                    "terminalId": terminal_id,
                    "encoding": "utf-8",
                    "delta": acceptance.terminal_large_bytes(
                        0, acceptance.TERMINAL_LOG_PREVIEW_BYTES
                    ).decode("ascii"),
                    "byteOffset": 0,
                    "byteLength": acceptance.TERMINAL_LOG_PREVIEW_BYTES,
                    "truncated": True,
                },
            },
        ]
        for artifact_id, segment in zip(artifact_ids, expected_segments, strict=True):
            events.extend(
                [
                    {
                        "sequence": len(events) + 1,
                        "eventType": "artifact.ready",
                        "executionId": "execution-terminal-large",
                        "payload": {
                            "artifactId": artifact_id,
                            "kind": "terminal_log",
                            "sizeBytes": segment["length"],
                            "contentType": "text/plain; charset=utf-8",
                        },
                    },
                    {
                        "sequence": len(events) + 2,
                        "eventType": "item.updated",
                        "executionId": "execution-terminal-large",
                        "payload": {
                            "itemType": "command_execution",
                            "status": "inProgress",
                            "title": "Terminal log",
                            "data": {
                                "provider": "codex",
                                "terminal": {
                                    "terminalId": terminal_id,
                                    "eventType": "terminal.output.reference",
                                    "artifactId": artifact_id,
                                    "offset": segment["offset"],
                                    "length": segment["length"],
                                    "segmentIndex": segment["segmentIndex"],
                                    "encoding": segment["encoding"],
                                },
                            },
                        },
                    },
                ]
            )
        events.append(
            {
                "sequence": len(events) + 1,
                "eventType": "artifact.ready",
                "executionId": "execution-terminal-large",
                "payload": {
                    "artifactId": "00000000-0000-4000-8000-000000000099",
                    "kind": "checkpoint",
                    "sizeBytes": 128,
                    "contentType": "application/gzip",
                },
            }
        )
        completion_terminal: dict[str, Any] = {
            "terminalId": terminal_id,
            "eventType": "terminal.exited",
            "totalBytes": acceptance.TERMINAL_LARGE_TOTAL_BYTES,
            "previewBytes": acceptance.TERMINAL_LOG_PREVIEW_BYTES,
            "segmentCount": len(expected_segments),
            "truncated": True,
            "exitCode": 0,
        }
        if leak_runtime_path:
            completion_terminal["runtimeOutputDirectory"] = "/tmp/synara-runtime-output"
        events.append(
            {
                "sequence": len(events) + 1,
                "eventType": "item.completed",
                "executionId": "execution-terminal-large",
                "payload": {
                    "itemType": "command_execution",
                    "status": "completed",
                    "data": {"provider": "codex", "terminal": completion_terminal},
                },
            }
        )
        self.execution_terminal = {
            "sequence": len(events) + 1,
            "eventType": "execution.completed",
            "executionId": "execution-terminal-large",
            "workerId": "worker-1",
            "generation": 1,
        }
        events.append(self.execution_terminal)
        self.events = events

    def _create_turn(self, input_text: str) -> dict[str, Any]:
        if input_text != "[terminal-large]":
            raise AssertionError(f"unexpected fixture directive: {input_text}")
        return {"id": "turn-terminal-large"}

    def _wait_for_turn_terminal(
        self,
        turn_id: str,
        expected_event_type: str,
    ) -> tuple[dict[str, Any], list[dict[str, Any]]]:
        if turn_id != "turn-terminal-large" or expected_event_type != "execution.completed":
            raise AssertionError("unexpected large Terminal wait")
        return self.execution_terminal, self.events


class RealProviderTerminalLargeSuite(TerminalLargeSuite):
    def __init__(self) -> None:
        super().__init__()
        self.options = dataclasses.replace(
            self.options,
            suite="real-provider-smoke",
            provider="claudeAgent",
        )
        self.state.credential_id = "controlled-provider-credential-id"
        marker = self._real_provider_marker("terminal-large")
        self.assistant_text = f"Running the requested command once.{marker}"
        execution_terminal = self.events.pop()
        execution_terminal["payload"] = {"output": {"text": self.assistant_text}}
        self.events = [
            {
                "eventType": "turn.created",
                "executionId": "execution-terminal-large",
                "payload": {
                    "turnId": "turn-terminal-large",
                    "executionId": "execution-terminal-large",
                },
            },
            {
                "eventType": "execution.leased",
                "executionId": "execution-terminal-large",
                "payload": {
                    "providerResume": {
                        "requestedStrategy": "native-cursor",
                        "selectedStrategy": "native-cursor",
                        "reasonCode": "cursor_usable",
                    }
                },
            },
            {
                "eventType": "execution.started",
                "executionId": "execution-terminal-large",
            },
            *self.events,
            {
                "eventVersion": 2,
                "eventType": "content.delta",
                "executionId": "execution-terminal-large",
                "payload": {"streamKind": "assistant_text", "delta": self.assistant_text},
            },
            execution_terminal,
        ]
        for sequence, event in enumerate(self.events, start=1):
            event["sequence"] = sequence
        self.execution_terminal = execution_terminal
        self.created_input: str | None = None

    def _create_turn(
        self,
        input_text: str,
        *,
        runtime_mode: str = "full-access",
        interaction_mode: str = "default",
    ) -> dict[str, Any]:
        if runtime_mode != "full-access" or interaction_mode != "default":
            raise AssertionError("unexpected real Provider terminal-large Turn mode")
        command = acceptance.terminal_large_node_command()
        marker = self._real_provider_marker("terminal-large")
        if input_text.count(command) != 1 or input_text.count(marker) != 1:
            raise AssertionError("real Provider terminal-large prompt omitted its command or marker")
        self.created_input = input_text
        return {"id": "turn-terminal-large"}


def generated_file_snapshot_bytes(
    member_name: str = acceptance.GENERATED_FILE_RELATIVE_PATH,
    *,
    include_approval_sentinel: bool = False,
    include_steer_sentinel: bool = False,
) -> bytes:
    content = acceptance.generated_file_bytes()
    buffer = io.BytesIO()
    with tarfile.open(fileobj=buffer, mode="w") as archive:
        member = tarfile.TarInfo(member_name)
        member.size = len(content)
        member.mode = 0o644
        member.mtime = 0
        archive.addfile(member, io.BytesIO(content))
        standalone = tarfile.TarInfo(acceptance.STANDALONE_GENERATED_FILE_RELATIVE_PATH)
        standalone.size = len(acceptance.STANDALONE_GENERATED_FILE_CONTENT)
        standalone.mode = 0o644
        standalone.mtime = 0
        archive.addfile(standalone, io.BytesIO(acceptance.STANDALONE_GENERATED_FILE_CONTENT))
        if include_approval_sentinel:
            approval = tarfile.TarInfo(acceptance.REAL_PROVIDER_APPROVAL_RELATIVE_PATH)
            approval.size = len(acceptance.REAL_PROVIDER_APPROVAL_CONTENT)
            approval.mode = 0o644
            approval.mtime = 0
            archive.addfile(approval, io.BytesIO(acceptance.REAL_PROVIDER_APPROVAL_CONTENT))
        if include_steer_sentinel:
            steer = tarfile.TarInfo(acceptance.REAL_PROVIDER_STEER_RELATIVE_PATH)
            steer.size = len(acceptance.REAL_PROVIDER_STEER_CONTENT)
            steer.mode = 0o644
            steer.mtime = 0
            archive.addfile(steer, io.BytesIO(acceptance.REAL_PROVIDER_STEER_CONTENT))
    return buffer.getvalue()


class GeneratedFileAPI(FakeAPI):
    def __init__(
        self,
        artifacts: Sequence[Mapping[str, Any]],
        payloads: Mapping[str, bytes],
    ) -> None:
        super().__init__()
        self.artifacts = [dict(artifact) for artifact in artifacts]
        self.payloads = dict(payloads)
        self.list_calls = 0

    def request(
        self,
        method: str,
        path: str,
        payload: Mapping[str, Any] | None = None,
        expected: Sequence[int] = (200,),
        *,
        maximum_timeout: float = 10.0,
    ) -> Any:
        del payload, expected, maximum_timeout
        if method == "GET" and path == "/v1/sessions/session-id/artifacts":
            self.list_calls += 1
            prior = {
                "id": "00000000-0000-4000-8000-000000000099",
                "kind": "workspace_snapshot",
                "status": "ready",
            }
            return {"items": [prior] if self.list_calls == 1 else [prior, *self.artifacts]}
        if method == "POST" and path.startswith("/v1/artifacts/") and path.endswith("/download"):
            artifact_id = path.removeprefix("/v1/artifacts/").removesuffix("/download")
            artifact = next(
                (item for item in self.artifacts if item.get("id") == artifact_id),
                None,
            )
            if artifact is not None:
                return {"artifact": artifact, "url": f"/downloads/{artifact_id}"}
        return super().request(method, path)

    def download_bytes(
        self,
        url: str,
        *,
        maximum_bytes: int,
        maximum_timeout: float = 30.0,
    ) -> bytes:
        del maximum_timeout
        if not url.startswith("/downloads/"):
            raise AssertionError(f"unexpected download URL: {url}")
        artifact_id = url.removeprefix("/downloads/")
        payload = self.payloads.get(artifact_id)
        if payload is None:
            raise AssertionError(f"missing fake Artifact payload: {artifact_id}")
        if len(payload) > maximum_bytes:
            raise AssertionError("fake generated-file Artifact exceeded the caller limit")
        return payload


class RealProviderGeneratedFileSuite(acceptance.AcceptanceSuite):
    def __init__(
        self,
        *,
        duplicate_ready: bool = False,
        duplicate_generated_ready: bool = False,
        snapshot_member_name: str = acceptance.GENERATED_FILE_RELATIVE_PATH,
        corrupt_sha256: bool = False,
        corrupt_generated_file: bool = False,
    ) -> None:
        snapshot = generated_file_snapshot_bytes(snapshot_member_name)
        snapshot_sha256 = "0" * 64 if corrupt_sha256 else hashlib.sha256(snapshot).hexdigest()
        generated_content = acceptance.STANDALONE_GENERATED_FILE_CONTENT
        generated_artifact_id = "00000000-0000-4000-8000-000000000100"
        generated_artifact = {
            "id": generated_artifact_id,
            "executionId": "execution-generated-file",
            "kind": "generated_file",
            "status": "ready",
            "originalName": pathlib.PurePosixPath(
                acceptance.STANDALONE_GENERATED_FILE_RELATIVE_PATH
            ).name,
            "contentType": "application/octet-stream",
            "sizeBytes": len(generated_content),
            "sha256": hashlib.sha256(generated_content).hexdigest(),
        }
        artifact_id = "00000000-0000-4000-8000-000000000101"
        artifact = {
            "id": artifact_id,
            "executionId": "execution-generated-file",
            "kind": "workspace_snapshot",
            "status": "ready",
            "originalName": "workspace-execution-generated-file-generation-1.tar",
            "contentType": "application/x-tar",
            "sizeBytes": len(snapshot),
            "sha256": snapshot_sha256,
        }
        driver = FakeDriver(acceptance.STANDING_WORKER)
        driver.api = GeneratedFileAPI(
            [generated_artifact, artifact],
            {
                generated_artifact_id: (
                    b"X" + generated_content[1:] if corrupt_generated_file else generated_content
                ),
                artifact_id: snapshot,
            },
        )
        super().__init__(
            dataclasses.replace(
                runner_options(),
                suite="real-provider-smoke",
                real_provider_cases=("generated-file-checkpoint",),
            ),
            driver,
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        self.state.session_id = "session-id"
        marker = self._real_provider_marker("generated-file-checkpoint")
        generated_ready_events = [
            {
                "eventType": "artifact.ready",
                "executionId": "execution-generated-file",
                "workerId": "worker-1",
                "generation": 1,
                "payload": {
                    "artifactId": generated_artifact_id,
                    "kind": "generated_file",
                    "contentType": "application/octet-stream",
                    "sizeBytes": len(generated_content),
                },
            }
        ]
        if duplicate_generated_ready:
            generated_ready_events.append(dict(generated_ready_events[0]))
        artifact_ready_events = [
            {
                "eventType": "artifact.ready",
                "executionId": "execution-generated-file",
                "workerId": "worker-1",
                "generation": 1,
                "payload": {
                    "artifactId": artifact_id,
                    "kind": "workspace_snapshot",
                    "contentType": "application/x-tar",
                    "sizeBytes": len(snapshot),
                },
            }
        ]
        if duplicate_ready:
            artifact_ready_events.append(dict(artifact_ready_events[0]))
        self.events: list[dict[str, Any]] = [
            {
                "eventType": "turn.created",
                "executionId": "execution-generated-file",
                "payload": {
                    "turnId": "turn-generated-file",
                    "executionId": "execution-generated-file",
                },
            },
            {
                "eventType": "execution.leased",
                "executionId": "execution-generated-file",
                "payload": {
                    "providerResume": {
                        "requestedStrategy": "native-cursor",
                        "selectedStrategy": "native-cursor",
                        "reasonCode": "cursor_usable",
                    }
                },
            },
            {
                "eventType": "execution.started",
                "executionId": "execution-generated-file",
            },
            {
                "eventVersion": 2,
                "eventType": "content.delta",
                "executionId": "execution-generated-file",
                "payload": {"streamKind": "assistant_text", "delta": marker},
            },
            *generated_ready_events,
            {
                "eventType": "workspace.dirty",
                "executionId": "execution-generated-file",
                "workerId": "worker-1",
                "generation": 1,
                "payload": {"turnId": "turn-generated-file", "workspaceId": "workspace-1"},
            },
            {
                "eventType": "checkpoint.created",
                "executionId": "execution-generated-file",
                "workerId": "worker-1",
                "generation": 1,
                "payload": {
                    "checkpointId": "checkpoint-generated-file",
                    "strategy": "snapshot",
                    "turnId": "turn-generated-file",
                    "workspaceId": "workspace-1",
                },
            },
            *artifact_ready_events,
            {
                "eventType": "checkpoint.ready",
                "executionId": "execution-generated-file",
                "workerId": "worker-1",
                "generation": 1,
                "payload": {
                    "artifactId": artifact_id,
                    "checkpointId": "checkpoint-generated-file",
                    "sha256": snapshot_sha256,
                    "strategy": "snapshot",
                    "turnId": "turn-generated-file",
                    "workspaceId": "workspace-1",
                },
            },
        ]
        self.execution_terminal = {
            "eventType": "execution.completed",
            "executionId": "execution-generated-file",
            "workerId": "worker-1",
            "generation": 1,
            "payload": {"output": {"text": marker}},
        }
        self.events.append(self.execution_terminal)
        for sequence, event in enumerate(self.events, start=1):
            event["sequence"] = sequence
        self.created_input: str | None = None

    def _create_turn(
        self,
        input_text: str,
        *,
        runtime_mode: str = "full-access",
        interaction_mode: str = "default",
    ) -> dict[str, Any]:
        if runtime_mode != "full-access" or interaction_mode != "default":
            raise AssertionError("unexpected generated-file Turn mode")
        command = acceptance.generated_file_node_command()
        marker = self._real_provider_marker("generated-file-checkpoint")
        standalone_text = acceptance.STANDALONE_GENERATED_FILE_CONTENT.decode("ascii").rstrip("\n")
        if (
            input_text.count(command) != 1
            or input_text.count(marker) != 1
            or input_text.count(acceptance.STANDALONE_GENERATED_FILE_RELATIVE_PATH) != 1
            or input_text.count(standalone_text) != 1
            or "native apply_patch file-change tool" not in input_text
        ):
            raise AssertionError("generated-file prompt omitted its command or marker")
        self.created_input = input_text
        return {"id": "turn-generated-file"}

    def _wait_for_turn_terminal(
        self,
        turn_id: str,
        expected_event_type: str,
    ) -> tuple[dict[str, Any], list[dict[str, Any]]]:
        if turn_id != "turn-generated-file" or expected_event_type != "execution.completed":
            raise AssertionError("unexpected generated-file wait")
        return self.execution_terminal, self.events


class ProviderFaultServerTest(unittest.TestCase):
    def test_rate_limit_endpoint_records_only_bounded_request_metadata(self) -> None:
        server = acceptance._ProviderFaultServer("codex", "rate-limit")
        credential = "provider-fault-secret-value"
        server.start()
        try:
            with self.assertRaises(urllib.error.HTTPError) as unscoped:
                urllib.request.urlopen(
                    f"http://127.0.0.1:{server.port}/not-the-owned-route",
                    timeout=2.0,
                )
            unscoped.exception.close()
            request = urllib.request.Request(
                f"{server.credential_base_url}/responses?stream=true",
                data=b'{"model":"test"}',
                headers={
                    "Authorization": f"Bearer {credential}",
                    "Content-Type": "application/json",
                },
                method="POST",
            )
            with self.assertRaises(urllib.error.HTTPError) as raised:
                urllib.request.urlopen(request, timeout=2.0)
            body = raised.exception.read().decode("utf-8")
            raised.exception.close()
        finally:
            server.stop()

        evidence = server.evidence()
        self.assertEqual(raised.exception.code, 429)
        self.assertIn("rate_limit_error", body)
        self.assertEqual(evidence["requestCount"], 1)
        self.assertEqual(evidence["paths"], ["/v1/responses"])
        self.assertEqual(evidence["credentialHeaderNames"], ["authorization"])
        self.assertEqual(evidence["unscopedRequestCount"], 1)
        self.assertFalse(evidence["routeTokenPersisted"])
        self.assertFalse(evidence["requestBodiesRetained"])
        self.assertNotIn(credential, json.dumps(evidence))
        self.assertNotIn(server.route_token, json.dumps(evidence))


class DockerDriverRealProviderFaultTest(unittest.TestCase):
    @staticmethod
    def _driver() -> acceptance.DockerDriver:
        driver = object.__new__(acceptance.DockerDriver)
        driver.options = runner_options()
        driver.redactor = acceptance.SecretRedactor()
        driver.target_id = "target-id"
        return driver

    def test_fault_server_uses_host_gateway_and_unscoped_bind(self) -> None:
        driver = self._driver()
        server = driver.create_provider_fault_server("codex", "authentication")
        server.start()
        try:
            self.assertEqual(server.listen_host, "0.0.0.0")
            self.assertEqual(server.advertised_host, "host.docker.internal")
            self.assertTrue(server.endpoint.startswith("http://host.docker.internal:"))
            self.assertIn(server.route_token, server.endpoint)
        finally:
            server.stop()

    def test_execution_worker_identity_joins_execution_to_exact_worker_pod_name(self) -> None:
        driver = self._driver()
        with tempfile.TemporaryDirectory() as directory:
            driver.state_dir = pathlib.Path(directory)
            with sqlite3.connect(driver.state_dir / "metadata.sqlite") as connection:
                connection.executescript(
                    """
                    CREATE TABLE agent_executions (
                      id TEXT PRIMARY KEY,
                      execution_target_id TEXT NOT NULL,
                      worker_id TEXT,
                      generation INTEGER NOT NULL
                    );
                    CREATE TABLE worker_instances (
                      id TEXT PRIMARY KEY,
                      execution_target_id TEXT NOT NULL,
                      incarnation INTEGER NOT NULL,
                      instance_uid TEXT NOT NULL,
                      status TEXT NOT NULL,
                      pod_name TEXT NOT NULL
                    );
                    INSERT INTO worker_instances VALUES (
                      'worker-2', 'target-id', 3, 'instance-2', 'online', 'synara-worker-1'
                    );
                    INSERT INTO agent_executions VALUES (
                      'execution-2', 'target-id', 'worker-2', 1
                    );
                    """
                )

            identity = driver._execution_worker_identity("target-id", "execution-2")

        self.assertEqual(
            identity,
            {
                "id": "worker-2",
                "generation": 1,
                "incarnation": 3,
                "instanceUid": "instance-2",
                "status": "online",
                "podName": "synara-worker-1",
            },
        )

    def test_fault_server_probe_runs_inside_exact_managed_container(self) -> None:
        driver = self._driver()
        driver._wait_container = mock.Mock(return_value={"Id": "abcdef1234567890"})  # type: ignore[method-assign]
        driver._docker_command = mock.Mock(return_value="")  # type: ignore[method-assign]
        server = acceptance._ProviderFaultServer(
            "claudeAgent",
            "rate-limit",
            listen_host="0.0.0.0",
            advertised_host="host.docker.internal",
        )
        server.start()
        try:
            evidence = driver.probe_provider_fault_server(server)
        finally:
            server.stop()

        arguments = driver._docker_command.call_args.args[0]
        self.assertEqual(arguments[:3], ["exec", "abcdef1234567890", "node"])
        self.assertEqual(arguments[-2], "429")
        self.assertEqual(arguments[-1], server.credential_base_url)
        self.assertTrue(evidence["probedFromWorker"])
        self.assertEqual(evidence["containerId"], "abcdef123456")
        self.assertFalse(evidence["endpointPersisted"])
        self.assertNotIn(server.route_token, json.dumps(evidence))

    def test_host_crash_kills_one_agentd_descendant_inside_exact_container(self) -> None:
        driver = self._driver()
        driver._wait_container = mock.Mock(return_value={"Id": "abcdef1234567890"})  # type: ignore[method-assign]
        driver._docker_command = mock.Mock(  # type: ignore[method-assign]
            return_value=json.dumps(
                {
                    "rootPid": 1,
                    "candidateCount": 1,
                    "descendantCount": 4,
                    "providerHostPid": 42,
                    "killed": True,
                }
            )
        )

        evidence = driver.crash_provider_host()

        arguments = driver._docker_command.call_args.args[0]
        self.assertEqual(arguments[:4], ["exec", "abcdef1234567890", "node", "-e"])
        self.assertNotIn("--protocol-v2", arguments[4])
        self.assertEqual(arguments[-1], "1")
        self.assertEqual(evidence["providerHostPid"], 42)
        self.assertTrue(evidence["scopedToManagedContainer"])
        self.assertTrue(evidence["scopedToAgentdDescendants"])
        self.assertFalse(evidence["broadProcessMatchUsed"])

    def test_host_crash_fails_closed_on_ambiguous_container_processes(self) -> None:
        driver = self._driver()
        driver._wait_container = mock.Mock(return_value={"Id": "abcdef1234567890"})  # type: ignore[method-assign]
        driver._docker_command = mock.Mock(  # type: ignore[method-assign]
            return_value=json.dumps({"rootPid": 1, "candidateCount": 2, "descendantCount": 5})
        )

        with self.assertRaises(acceptance.AcceptanceError) as caught:
            driver.crash_provider_host()

        self.assertEqual(caught.exception.code, "runner.provider_host_process_ambiguous")
        self.assertEqual(caught.exception.evidence["candidateCount"], 2)

    def test_host_crash_rejects_invalid_process_scan_payload(self) -> None:
        driver = self._driver()
        driver._wait_container = mock.Mock(return_value={"Id": "abcdef1234567890"})  # type: ignore[method-assign]
        driver._docker_command = mock.Mock(  # type: ignore[method-assign]
            return_value=json.dumps(
                {"rootPid": 1, "candidateCount": "one", "descendantCount": 4}
            )
        )

        with self.assertRaises(acceptance.AcceptanceError) as caught:
            driver.crash_provider_host()

        self.assertEqual(caught.exception.code, "runner.provider_host_process_scan_failed")

    def test_worker_network_fault_disconnects_exact_execution_worker_container(self) -> None:
        driver = self._driver()
        driver.options = dataclasses.replace(
            runner_options(),
            target="docker",
            suite="fixture-load-failure",
        )
        driver.network_name = "owned-network"
        driver.owns_network = True
        driver.desired_workers = 2
        driver.deadline = mock.Mock()
        driver._execution_worker_identity = mock.Mock(  # type: ignore[method-assign]
            return_value={
                "id": "worker-2",
                "generation": 1,
                "incarnation": 3,
                "instanceUid": "instance-2",
                "status": "online",
                "podName": "synara-worker-1",
            }
        )
        driver._wait_containers = mock.Mock(  # type: ignore[method-assign]
            return_value=[
                {
                    "Id": "container-0-full",
                    "Name": "/synara-worker-0",
                    "Config": {
                        "Labels": {
                            "synara.io/managed": "true",
                            "synara.io/execution-target-id": "target-id",
                            "synara.io/worker-index": "0",
                        }
                    },
                },
                {
                    "Id": "container-1-full",
                    "Name": "/synara-worker-1",
                    "Config": {
                        "Labels": {
                            "synara.io/managed": "true",
                            "synara.io/execution-target-id": "target-id",
                            "synara.io/worker-index": "1",
                        }
                    },
                },
            ]
        )
        driver._docker_command = mock.Mock(return_value="")  # type: ignore[method-assign]
        driver._docker_completed = mock.Mock(  # type: ignore[method-assign]
            return_value=subprocess.CompletedProcess([], 0, "", "")
        )

        evidence = driver.inject_failure("worker-network", "target-id", "execution-2")

        driver._docker_command.assert_called_once_with(
            ["network", "disconnect", "owned-network", "container-1-full"]
        )
        driver._docker_completed.assert_called_once_with(
            ["network", "connect", "owned-network", "container-1-full"],
            cleanup_timeout=15.0,
        )
        self.assertEqual(evidence["executionId"], "execution-2")
        self.assertEqual(evidence["workerId"], "worker-2")
        self.assertEqual(evidence["containerName"], "synara-worker-1")
        self.assertEqual(evidence["workerIndex"], 1)
        self.assertTrue(evidence["exactExecutionWorkerMatch"])

    def test_provider_host_crash_targets_exact_execution_worker_container(self) -> None:
        driver = self._driver()
        driver._execution_worker_container = mock.Mock(  # type: ignore[method-assign]
            return_value=(
                {"Id": "abcdef1234567890", "Name": "/synara-worker-1"},
                {
                    "id": "worker-2",
                    "generation": 1,
                    "incarnation": 3,
                    "instanceUid": "instance-2",
                    "status": "online",
                    "podName": "synara-worker-1",
                },
                "1",
            )
        )
        driver._docker_command = mock.Mock(  # type: ignore[method-assign]
            return_value=json.dumps(
                {
                    "rootPid": 1,
                    "candidateCount": 1,
                    "descendantCount": 4,
                    "providerHostPid": 42,
                    "killed": True,
                }
            )
        )

        evidence = driver.inject_failure(
            "provider-host-process-crash",
            "target-id",
            "execution-2",
        )

        arguments = driver._docker_command.call_args.args[0]
        self.assertEqual(arguments[:4], ["exec", "abcdef1234567890", "node", "-e"])
        self.assertEqual(evidence["executionId"], "execution-2")
        self.assertEqual(evidence["workerId"], "worker-2")
        self.assertEqual(evidence["workerIndex"], 1)
        self.assertEqual(evidence["providerHostPid"], 42)
        self.assertTrue(evidence["exactExecutionWorkerMatch"])
        self.assertTrue(evidence["scopedToManagedContainer"])
        self.assertTrue(evidence["scopedToAgentdDescendants"])
        self.assertFalse(evidence["broadProcessMatchUsed"])

    def test_worker_container_loss_waits_for_same_logical_worker_replacement(self) -> None:
        driver = self._driver()
        driver.options = dataclasses.replace(
            runner_options(),
            target="docker",
            suite="fixture-load-failure",
        )
        driver.api = FakeAPI()  # type: ignore[assignment]
        before_worker = {
            "id": "worker-2",
            "generation": 1,
            "incarnation": 3,
            "instanceUid": "instance-old",
            "status": "online",
            "podName": "synara-worker-1",
        }
        driver._execution_worker_container = mock.Mock(  # type: ignore[method-assign]
            return_value=(
                {"Id": "aaaaaaaaaaaaaaaa", "Name": "/synara-worker-1"},
                before_worker,
                "1",
            )
        )
        driver._container_snapshots = mock.Mock(  # type: ignore[method-assign]
            return_value=[
                {
                    "Id": "bbbbbbbbbbbbbbbb",
                    "Name": "/synara-worker-1",
                    "State": {"Running": True},
                }
            ]
        )
        driver._execution_worker_identity = mock.Mock(  # type: ignore[method-assign]
            return_value={
                **before_worker,
                "incarnation": 4,
                "instanceUid": "instance-new",
            }
        )
        driver._write_volume_sentinel = mock.Mock()  # type: ignore[method-assign]
        driver._verify_volume_sentinel = mock.Mock()  # type: ignore[method-assign]
        driver._docker_command = mock.Mock(return_value="")  # type: ignore[method-assign]

        evidence = driver.inject_failure(
            "worker-container-loss",
            "target-id",
            "execution-2",
        )

        driver._docker_command.assert_called_once_with(
            ["rm", "-f", "aaaaaaaaaaaaaaaa"],
            maximum_timeout=20.0,
        )
        driver._write_volume_sentinel.assert_called_once_with("aaaaaaaaaaaaaaaa")
        driver._verify_volume_sentinel.assert_called_once_with("bbbbbbbbbbbbbbbb")
        self.assertEqual(evidence["removedContainerId"], "aaaaaaaaaaaa")
        self.assertEqual(evidence["replacementContainerId"], "bbbbbbbbbbbb")
        self.assertTrue(evidence["containerIdChanged"])
        self.assertTrue(evidence["workerIdStable"])
        self.assertTrue(evidence["workerIncarnationAdvanced"])
        self.assertTrue(evidence["instanceUidChanged"])
        self.assertTrue(evidence["namedVolumeContinuity"]["preservedAcrossReplacement"])


class KubernetesDriverRealProviderFaultTest(unittest.TestCase):
    @staticmethod
    def _driver() -> acceptance.KubernetesDriver:
        driver = object.__new__(acceptance.KubernetesDriver)
        driver.options = runner_options()
        driver.redactor = acceptance.SecretRedactor()
        driver.target_id = "target-id"
        driver.target_namespace = "synara-stage3-worker-test"
        driver.worker_service_account = "synara-worker-test"
        driver.image = "synara-worker:test"
        driver.target_runtimes = {
            "target-id": {
                "namespace": driver.target_namespace,
                "serviceAccount": driver.worker_service_account,
                "image": driver.image,
            }
        }
        return driver

    @staticmethod
    def _running_pod() -> dict[str, Any]:
        return {
            "metadata": {
                "name": "synara-agentd-execution",
                "uid": "pod-uid",
                "labels": {
                    "synara.io/execution-target-id": "target-id",
                    "synara.io/execution-id": "execution-id",
                },
            },
            "spec": {"containers": [{"name": "agentd"}]},
            "status": {"phase": "Running"},
        }

    def test_fault_server_uses_kubernetes_host_gateway_without_persisting_endpoint(self) -> None:
        driver = self._driver()
        server = driver.create_provider_fault_server("claudeAgent", "rate-limit")
        server.start()
        try:
            evidence = driver.probe_provider_fault_server(server)
        finally:
            server.stop()

        self.assertEqual(server.listen_host, "0.0.0.0")
        self.assertEqual(server.advertised_host, "host.docker.internal")
        self.assertEqual(evidence["validationMode"], "controlled-provider-request")
        self.assertFalse(evidence["probedFromWorker"])
        self.assertFalse(evidence["endpointPersisted"])
        self.assertNotIn(server.route_token, json.dumps(evidence))

        completed = acceptance.finalize_provider_fault_reachability(
            evidence,
            {"requestCount": 1},
        )
        self.assertTrue(completed["probedFromWorker"])
        self.assertEqual(completed["observedProviderRequestCount"], 1)

    def test_fault_server_reachability_requires_an_observed_provider_request(self) -> None:
        driver = self._driver()
        server = driver.create_provider_fault_server("codex", "authentication")
        reachability = driver.probe_provider_fault_server(server)

        with self.assertRaises(acceptance.AcceptanceError) as caught:
            acceptance.finalize_provider_fault_reachability(
                reachability,
                {"requestCount": 0},
            )

        server.server.server_close()
        self.assertEqual(caught.exception.code, "runner.provider_fault_reachability_unproven")

    def test_host_crash_kills_one_agentd_descendant_inside_exact_execution_pod(self) -> None:
        driver = self._driver()
        driver._kubectl_command = mock.Mock(  # type: ignore[method-assign]
            side_effect=[
                json.dumps({"items": [self._running_pod()]}),
                json.dumps(
                    {
                        "rootPid": 1,
                        "candidateCount": 1,
                        "descendantCount": 4,
                        "providerHostPid": 52,
                        "killed": True,
                    }
                ),
            ]
        )

        evidence = driver.crash_provider_host()

        inventory_arguments = driver._kubectl_command.call_args_list[0].args[0]
        crash_arguments = driver._kubectl_command.call_args_list[1].args[0]
        self.assertEqual(
            inventory_arguments,
            [
                "-n",
                "synara-stage3-worker-test",
                "get",
                "pods",
                "-l",
                "synara.io/execution-target-id=target-id",
                "-o",
                "json",
            ],
        )
        self.assertEqual(
            crash_arguments[:8],
            [
                "-n",
                "synara-stage3-worker-test",
                "exec",
                "synara-agentd-execution",
                "-c",
                "agentd",
                "--",
                "node",
            ],
        )
        self.assertNotIn("--protocol-v2", crash_arguments[-2])
        self.assertEqual(crash_arguments[-1], "1")
        self.assertEqual(evidence["providerHostPid"], 52)
        self.assertEqual(evidence["podUid"], "pod-uid")
        self.assertEqual(evidence["executionId"], "execution-id")
        self.assertTrue(evidence["scopedToExecutionPod"])
        self.assertTrue(evidence["scopedToAgentdDescendants"])
        self.assertFalse(evidence["broadProcessMatchUsed"])

    def test_host_crash_fails_closed_when_multiple_target_pods_are_running(self) -> None:
        driver = self._driver()
        driver._kubectl_command = mock.Mock(  # type: ignore[method-assign]
            return_value=json.dumps({"items": [self._running_pod(), self._running_pod()]})
        )

        with self.assertRaises(acceptance.AcceptanceError) as caught:
            driver.crash_provider_host()

        self.assertEqual(caught.exception.code, "runner.kubernetes_active_pod_ambiguous")
        self.assertEqual(caught.exception.evidence["runningPodCount"], 2)
        self.assertEqual(driver._kubectl_command.call_count, 1)


class APIClientTimeoutTest(unittest.TestCase):
    def test_request_keeps_short_default_and_allows_explicit_long_operation_timeout(self) -> None:
        timeouts: list[float] = []

        class Response:
            status = 200

            def __enter__(self) -> Response:
                return self

            def __exit__(self, *_args: Any) -> None:
                return None

            @staticmethod
            def read() -> bytes:
                return b"{}"

        class Opener:
            @staticmethod
            def open(_request: Any, timeout: float) -> Response:
                timeouts.append(timeout)
                return Response()

        client = acceptance.APIClient(
            "http://127.0.0.1:3780",
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        client.opener = Opener()  # type: ignore[assignment]
        client.request("GET", "/default")
        client.request("GET", "/long", maximum_timeout=25.0)

        self.assertAlmostEqual(timeouts[0], 10.0, delta=0.1)
        self.assertAlmostEqual(timeouts[1], 25.0, delta=0.1)


class LocalRetentionHarnessTest(unittest.TestCase):
    def test_local_driver_uses_short_sweep_only_for_retention_concurrency(self) -> None:
        drivers = [
            acceptance.LocalDriver(
                REPO_ROOT,
                dataclasses.replace(
                    runner_options(),
                    target="local",
                    suite=suite,
                ),
                acceptance.Deadline(30.0),
                acceptance.SecretRedactor(),
            )
            for suite in ("fixture", "fixture-retention-concurrency")
        ]
        try:
            self.assertEqual(
                drivers[0]._control_plane_environment()["SYNARA_RETENTION_SWEEP_INTERVAL"],
                "24h",
            )
            self.assertEqual(
                drivers[1]._control_plane_environment()["SYNARA_RETENTION_SWEEP_INTERVAL"],
                acceptance.FIXTURE_RETENTION_SWEEP_INTERVAL,
            )
        finally:
            for driver in drivers:
                driver._release_state()

    def test_harness_stages_and_validates_active_then_terminal_cleanup(self) -> None:
        with tempfile.TemporaryDirectory() as raw_state:
            state_dir = pathlib.Path(raw_state)
            database_path = state_dir / "metadata.sqlite"
            tenant_id = "tenant-id"
            organization_id = "organization-id"
            project_id = "project-id"
            session_id = "session-id"
            target_id = "target-id"
            workspace_id = "workspace-id"
            materialization_id = "materialization-id"
            incarnation_id = "incarnation-id"
            checkpoint_id = "checkpoint-id"
            checkpoint_artifact_id = "checkpoint-artifact-id"
            generated_artifact_id = "generated-artifact-id"
            generated_execution_id = "generated-execution-id"
            active_execution_id = "active-execution-id"
            interaction_id = "interaction-id"
            generated_key = f"{tenant_id}/generated.txt"
            checkpoint_key = f"{tenant_id}/checkpoint.tar"
            generated_path = state_dir / "artifacts" / generated_key
            checkpoint_path = state_dir / "artifacts" / checkpoint_key
            generated_path.parent.mkdir(parents=True)
            generated_path.write_text("generated", encoding="utf-8")
            checkpoint_path.write_text("checkpoint", encoding="utf-8")
            workspace_path = state_dir.joinpath(
                "workspaces",
                "v3",
                target_id,
                tenant_id,
                project_id,
                session_id,
                workspace_id,
                incarnation_id,
            )
            workspace_path.mkdir(parents=True)
            (workspace_path / "manifest.json").write_text("{}\n", encoding="utf-8")
            now = acceptance.dt.datetime.now(acceptance.dt.timezone.utc)
            timestamp = now.isoformat(sep=" ", timespec="microseconds")
            with sqlite3.connect(database_path) as connection:
                connection.executescript(
                    """
                    CREATE TABLE agent_sessions (
                        tenant_id TEXT, id TEXT, status TEXT, updated_at TEXT, archived_at TEXT
                    );
                    CREATE TABLE remote_workspaces (
                        tenant_id TEXT, organization_id TEXT, project_id TEXT, session_id TEXT,
                        execution_target_id TEXT, id TEXT, state TEXT, current_checkpoint_id TEXT,
                        current_materialization_id TEXT, retention_until TEXT, updated_at TEXT, cleaned_at TEXT
                    );
                    CREATE TABLE workspace_materializations (
                        tenant_id TEXT, id TEXT, workspace_id TEXT, incarnation_id TEXT, layout_version INTEGER,
                        state TEXT, cleanup_reason TEXT, cleanup_requested_at TEXT, updated_at TEXT, cleaned_at TEXT
                    );
                    CREATE TABLE workspace_checkpoints (
                        tenant_id TEXT, id TEXT, artifact_id TEXT, status TEXT
                    );
                    CREATE TABLE artifacts (
                        tenant_id TEXT, id TEXT, session_id TEXT, execution_id TEXT, kind TEXT,
                        original_name TEXT, object_key TEXT, workspace_checkpoint_id TEXT,
                        status TEXT, ready_at TEXT, expires_at TEXT, deleted_at TEXT
                    );
                    CREATE TABLE agent_executions (
                        tenant_id TEXT, id TEXT, status TEXT, workspace_materialization_id TEXT
                    );
                    CREATE TABLE execution_interactions (
                        tenant_id TEXT, id TEXT, status TEXT
                    );
                    CREATE TABLE worker_leases (
                        tenant_id TEXT, execution_id TEXT
                    );
                    CREATE TABLE workspace_cleanup_commands (
                        tenant_id TEXT, materialization_id TEXT, id TEXT, status TEXT, reason TEXT,
                        dispatch_generation INTEGER, delivery_attempts INTEGER,
                        acknowledged_at TEXT, created_at TEXT
                    );
                    """
                )
                connection.execute(
                    "INSERT INTO agent_sessions VALUES (?, ?, 'active', ?, NULL)",
                    (tenant_id, session_id, timestamp),
                )
                connection.execute(
                    "INSERT INTO remote_workspaces VALUES (?, ?, ?, ?, ?, ?, 'ready', ?, ?, NULL, ?, NULL)",
                    (
                        tenant_id,
                        organization_id,
                        project_id,
                        session_id,
                        target_id,
                        workspace_id,
                        checkpoint_id,
                        materialization_id,
                        timestamp,
                    ),
                )
                connection.execute(
                    "INSERT INTO workspace_materializations VALUES (?, ?, ?, ?, 3, 'active', NULL, NULL, ?, NULL)",
                    (tenant_id, materialization_id, workspace_id, incarnation_id, timestamp),
                )
                connection.execute(
                    "INSERT INTO workspace_checkpoints VALUES (?, ?, ?, 'ready')",
                    (tenant_id, checkpoint_id, checkpoint_artifact_id),
                )
                connection.execute(
                    "INSERT INTO artifacts VALUES (?, ?, ?, ?, 'generated_file', 'artifact.txt', ?, NULL, 'ready', ?, NULL, NULL)",
                    (
                        tenant_id,
                        generated_artifact_id,
                        session_id,
                        generated_execution_id,
                        generated_key,
                        timestamp,
                    ),
                )
                connection.execute(
                    "INSERT INTO artifacts VALUES (?, ?, ?, ?, 'workspace_snapshot', 'checkpoint.tar', ?, ?, 'ready', ?, NULL, NULL)",
                    (
                        tenant_id,
                        checkpoint_artifact_id,
                        session_id,
                        generated_execution_id,
                        checkpoint_key,
                        checkpoint_id,
                        timestamp,
                    ),
                )
                connection.execute(
                    "INSERT INTO agent_executions VALUES (?, ?, 'waiting-for-approval', ?)",
                    (tenant_id, active_execution_id, materialization_id),
                )
                connection.execute(
                    "INSERT INTO execution_interactions VALUES (?, ?, 'pending')",
                    (tenant_id, interaction_id),
                )
                connection.execute(
                    "INSERT INTO worker_leases VALUES (?, ?)",
                    (tenant_id, active_execution_id),
                )

            harness = acceptance.LocalRetentionHarness(state_dir)
            seed = harness.load_seed(session_id)
            self.assertEqual(seed.evidence()["workspaceId"], workspace_id)
            expired_at = now - acceptance.dt.timedelta(days=2)
            harness.stage_active_retention(seed, expired_at)
            generated_path.unlink()
            with sqlite3.connect(database_path) as connection:
                connection.execute(
                    "UPDATE artifacts SET status = 'deleted', deleted_at = ? WHERE id = ?",
                    (timestamp, generated_artifact_id),
                )
            active = harness.snapshot(seed, active_execution_id, interaction_id)
            acceptance.AcceptanceSuite._validate_retention_active_snapshot(seed, active)

            harness.age_session(seed, expired_at)
            shutil.rmtree(workspace_path)
            with sqlite3.connect(database_path) as connection:
                connection.execute(
                    "UPDATE agent_sessions SET status = 'archived', archived_at = ? WHERE id = ?",
                    (timestamp, session_id),
                )
                connection.execute(
                    "UPDATE agent_executions SET status = 'completed' WHERE id = ?",
                    (active_execution_id,),
                )
                connection.execute(
                    "UPDATE execution_interactions SET status = 'resolved' WHERE id = ?",
                    (interaction_id,),
                )
                connection.execute(
                    "DELETE FROM worker_leases WHERE execution_id = ?",
                    (active_execution_id,),
                )
                connection.execute(
                    "UPDATE remote_workspaces SET state = 'cleaned', cleaned_at = ? WHERE id = ?",
                    (timestamp, workspace_id),
                )
                connection.execute(
                    "UPDATE workspace_materializations SET state = 'cleaned', cleaned_at = ? WHERE id = ?",
                    (timestamp, materialization_id),
                )
                connection.execute(
                    "INSERT INTO workspace_cleanup_commands VALUES (?, ?, 'cleanup-id', 'acknowledged', 'retention-session-archive', 1, 1, ?, ?)",
                    (tenant_id, materialization_id, timestamp, timestamp),
                )
            terminal = harness.snapshot(seed, active_execution_id, interaction_id)
            acceptance.AcceptanceSuite._validate_retention_terminal_snapshot(seed, terminal)


class ManagedWorkerImageTest(unittest.TestCase):
    def test_fixture_concurrency_requests_two_managed_docker_workers(self) -> None:
        driver = acceptance.DockerDriver(
            REPO_ROOT,
            dataclasses.replace(
                runner_options(),
                target="docker",
                suite="fixture-concurrency",
            ),
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        try:
            self.assertEqual(
                driver.desired_workers,
                acceptance.FIXTURE_CONCURRENCY_WORKERS,
            )
        finally:
            driver._release_state()

    def test_fixture_load_reuses_two_managed_docker_workers(self) -> None:
        driver = acceptance.DockerDriver(
            REPO_ROOT,
            dataclasses.replace(
                runner_options(),
                target="docker",
                suite="fixture-load",
                load_waves=acceptance.FIXTURE_LOAD_DEFAULT_WAVES,
            ),
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        try:
            self.assertEqual(
                driver.desired_workers,
                acceptance.FIXTURE_CONCURRENCY_WORKERS,
            )
        finally:
            driver._release_state()

    def test_fixture_load_failure_reuses_two_managed_docker_workers(self) -> None:
        driver = acceptance.DockerDriver(
            REPO_ROOT,
            dataclasses.replace(
                runner_options(),
                target="docker",
                suite="fixture-load-failure",
                load_waves=acceptance.FIXTURE_LOAD_DEFAULT_WAVES,
            ),
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        try:
            self.assertEqual(
                driver.desired_workers,
                acceptance.FIXTURE_CONCURRENCY_WORKERS,
            )
        finally:
            driver._release_state()

    def test_worker_smoke_preserves_image_path_with_non_login_shell(self) -> None:
        driver = object.__new__(acceptance.ManagedWorkerDriver)
        driver.logs_dir = pathlib.Path(tempfile.gettempdir()) / "synara-stage3-worker-smoke-test"
        driver.deadline = acceptance.Deadline(30.0)
        driver._ping_socket = mock.Mock(return_value={"path": "/var/run/docker.sock", "ping": "OK"})
        commands: list[list[str]] = []

        def docker_command(arguments: Sequence[str], **_kwargs: Any) -> str:
            commands.append(list(arguments))
            if arguments[:2] == ["version", "--format"]:
                return "29.4.0\n"
            if arguments[:2] == ["image", "inspect"]:
                return "sha256:image-id\n"
            return ""

        driver._docker_command = docker_command

        driver._prepare_worker_image("fixture-image", skip_build=True, log_prefix="fixture")

        smoke = next(command for command in commands if command[:2] == ["run", "--rm"])
        self.assertIn("-c", smoke)
        self.assertNotIn("-lc", smoke)
        self.assertIn("codex --version", smoke[-1])


class AcceptanceSuiteLifecycleTest(unittest.TestCase):
    def test_fixture_concurrency_requires_two_sessions_executions_and_workers(self) -> None:
        evidence = FixtureConcurrencySuite()._fixture_multi_provider_concurrency()

        self.assertEqual(evidence["providers"], ["codex", "claudeAgent"])
        self.assertEqual(evidence["distinctSessionCount"], 2)
        self.assertEqual(evidence["distinctExecutionCount"], 2)
        self.assertEqual(evidence["distinctWorkerCount"], 2)
        self.assertTrue(evidence["simultaneousPendingApprovals"])
        self.assertTrue(evidence["primaryRemainedPendingAfterSecondaryResolution"])

    def test_fixture_concurrency_rejects_one_worker_for_two_active_executions(self) -> None:
        suite = FixtureConcurrencySuite(duplicate_worker=True)

        with self.assertRaises(acceptance.AcceptanceError) as caught:
            suite._fixture_multi_provider_concurrency()

        self.assertEqual(caught.exception.code, "runner.concurrency_worker_reused")

    def test_fixture_concurrency_accepts_claude_as_primary_provider(self) -> None:
        evidence = FixtureConcurrencySuite(
            primary_provider="claudeAgent"
        )._fixture_multi_provider_concurrency()

        self.assertEqual(evidence["providers"], ["claudeAgent", "codex"])
        self.assertEqual(evidence["distinctWorkerCount"], 2)

    def test_fixture_load_validates_quota_recovery_and_durable_turns(self) -> None:
        evidence = FixtureLoadSuite(wave_count=2)._fixture_load_admission_waves()

        self.assertEqual(evidence["wavesCompleted"], 2)
        self.assertEqual(evidence["executionsCompleted"], 8)
        self.assertEqual(evidence["quotaRejections"], 4)
        self.assertEqual(evidence["admissionRetriesSucceeded"], 4)
        self.assertEqual(evidence["overlapObservations"], 6)
        self.assertEqual(evidence["distinctWorkerCount"], 2)
        self.assertEqual(evidence["providerExecutionCounts"], {"claudeAgent": 4, "codex": 4})
        self.assertEqual(set(evidence["sessionExecutionCounts"].values()), {2})
        self.assertEqual(evidence["eventTypeCounts"]["execution.completed"], 8)
        self.assertEqual(evidence["eventTypeCounts"]["checkpoint.ready"], 8)
        self.assertEqual(evidence["eventTypeCounts"]["artifact.ready"], 16)
        self.assertFalse(evidence["doubleExecution"])
        self.assertFalse(evidence["duplicateTerminal"])

    def test_fixture_load_allows_one_explicit_rollout_segment_with_hooks(self) -> None:
        suite = FixtureLoadSuite(wave_count=2)
        active: list[str] = []
        terminal: list[str] = []

        evidence = suite._fixture_load_admission_waves(
            wave_start=1,
            wave_count=1,
            active_validator=lambda load_turn: active.append(
                str(load_turn["active"]["executionId"])
            ),
            terminal_validator=lambda _load_turn, completed: terminal.append(
                str(completed["executionId"])
            ),
        )

        self.assertEqual(evidence["firstWave"], 2)
        self.assertEqual(evidence["lastWave"], 2)
        self.assertEqual(evidence["executionsCompleted"], 4)
        self.assertEqual(len(active), 4)
        self.assertEqual(terminal, active)

    def test_fixture_load_failure_targets_one_worker_and_reuses_sessions_after_recovery(self) -> None:
        suite = FixtureLoadFailureSuite(wave_count=2)

        network_failure = suite._fixture_load_failure_isolation(
            "worker-network",
            session_offset=0,
            affected_index=0,
        )
        container_failure = suite._fixture_load_failure_isolation(
            "worker-container-loss",
            session_offset=2,
            affected_index=1,
        )
        provider_crash = suite._fixture_load_provider_host_crash_isolation(
            session_offset=1,
            affected_index=0,
        )
        load = suite._fixture_load_admission_waves()

        self.assertTrue(network_failure["peerSessionEventsUnchanged"])
        self.assertTrue(network_failure["peerInteractionIdentityUnchanged"])
        self.assertTrue(network_failure["peerWorkerAndGenerationUnchanged"])
        self.assertTrue(network_failure["targetedGenerationFenced"])
        self.assertEqual(network_failure["recovery"]["staleGeneration"], 1)
        self.assertEqual(network_failure["recovery"]["replacementGeneration"], 2)
        self.assertEqual(
            network_failure["recovery"]["targetRecovery"]["workerId"],
            network_failure["affected"]["workerId"],
        )
        self.assertEqual(network_failure["terminalCount"], 2)
        self.assertEqual(network_failure["pendingInteractionCount"], 0)
        self.assertEqual(container_failure["failureCase"], "worker-container-loss")
        self.assertTrue(
            container_failure["recovery"]["targetRecovery"]["containerIdChanged"]
        )
        self.assertTrue(
            container_failure["recovery"]["targetRecovery"]["workerIncarnationAdvanced"]
        )
        self.assertEqual(container_failure["recovery"]["replacementGeneration"], 2)
        self.assertEqual(provider_crash["failureTerminal"]["failureCode"], "provider_unavailable")
        self.assertTrue(provider_crash["newExecutionRecovery"])
        self.assertNotEqual(
            provider_crash["affected"]["executionId"],
            provider_crash["retry"]["executionId"],
        )
        self.assertEqual(
            provider_crash["affected"]["workerId"],
            provider_crash["retry"]["workerId"],
        )
        self.assertTrue(provider_crash["peerSessionEventsUnchanged"])
        self.assertEqual(provider_crash["failedTerminalCount"], 1)
        self.assertEqual(provider_crash["completedTerminalCount"], 2)
        self.assertEqual(load["wavesCompleted"], 2)
        self.assertEqual(load["executionsCompleted"], 8)
        self.assertEqual(len(suite.events), acceptance.FIXTURE_LOAD_SESSIONS)

    def test_fixture_load_rejects_missing_quota_enforcement(self) -> None:
        suite = FixtureLoadSuite(wave_count=2, enforce_quota=False)

        with self.assertRaises(acceptance.AcceptanceError) as caught:
            suite._fixture_load_admission_waves()

        self.assertEqual(caught.exception.code, "runner.load_quota_not_enforced")

    def test_fixture_load_rejects_duplicate_worker_overlap(self) -> None:
        suite = FixtureLoadSuite(wave_count=2, duplicate_worker=True)

        with self.assertRaises(acceptance.AcceptanceError) as caught:
            suite._fixture_load_admission_waves()

        self.assertEqual(caught.exception.code, "runner.load_worker_overlap_invalid")

    def test_fixture_soak_validates_unique_executions_and_restart_history(self) -> None:
        suite = FixtureSoakSuite()

        evidence = suite._fixture_long_session_soak()

        self.assertEqual(evidence["turnsCompleted"], 3)
        self.assertEqual(evidence["distinctExecutionCount"], 3)
        self.assertEqual(evidence["controlPlaneRestartCount"], 1)
        self.assertEqual(evidence["sessionSequenceRange"], {"first": 1, "last": 40, "count": 40})
        self.assertFalse(evidence["eventPagination"]["required"])
        self.assertFalse(evidence["doubleExecution"])
        self.assertFalse(evidence["duplicateTerminal"])

    def test_fixture_soak_rejects_execution_reuse(self) -> None:
        suite = FixtureSoakSuite(duplicate_execution=True)

        with self.assertRaises(acceptance.AcceptanceError) as caught:
            suite._fixture_long_session_soak()

        self.assertEqual(caught.exception.code, "runner.soak_execution_reused")

    def test_fixture_soak_crosses_pagination_for_canonical_turn_count(self) -> None:
        suite = FixtureSoakSuite(turn_count=100)

        evidence = suite._fixture_long_session_soak()

        self.assertTrue(evidence["eventPagination"]["required"])
        self.assertTrue(evidence["eventPagination"]["exercised"])
        self.assertGreater(evidence["sessionSequenceRange"]["count"], 500)

    def test_terminal_large_pattern_has_stable_segment_hashes(self) -> None:
        self.assertEqual(
            acceptance.terminal_large_expected_segments(),
            [
                {
                    "offset": 0,
                    "length": 1 << 20,
                    "segmentIndex": 0,
                    "encoding": "utf-8",
                    "sha256": "f22d03ccbcfd9f40f8a8adb9deaa74e9c4fddc6f0325158a260021c698f0c869",
                },
                {
                    "offset": 1 << 20,
                    "length": 1 << 20,
                    "segmentIndex": 1,
                    "encoding": "utf-8",
                    "sha256": "eb149a408fa80e2faf39670f5e8e357a61d723d5d5b5d3620a9ca05105b636be",
                },
                {
                    "offset": 2 << 20,
                    "length": 257,
                    "segmentIndex": 2,
                    "encoding": "utf-8",
                    "sha256": "5fa2911d4a2a4821ba301f5256983895d62da71a9a5c4e8237e6a8900d4c09c1",
                },
            ],
        )

    def test_terminal_large_case_validates_preview_references_and_artifacts(self) -> None:
        evidence = TerminalLargeSuite()._terminal_large_log()

        self.assertEqual(evidence["terminalId"], "fixture-terminal-large-1")
        self.assertEqual(evidence["preview"]["bytes"], 32 << 10)
        self.assertTrue(evidence["preview"]["truncated"])
        self.assertEqual(evidence["completion"]["totalBytes"], 2 * (1 << 20) + 257)
        self.assertEqual(evidence["completion"]["segmentCount"], 3)
        self.assertEqual(
            [segment["length"] for segment in evidence["segments"]],
            [1 << 20, 1 << 20, 257],
        )
        self.assertFalse(evidence["runtimePhysicalPathLeak"])

    def test_generated_file_node_command_writes_exact_workspace_payload(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            completed = subprocess.run(
                ["bash", "-c", acceptance.generated_file_node_command()],
                cwd=directory,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=True,
            )
            generated = pathlib.Path(directory, acceptance.GENERATED_FILE_RELATIVE_PATH).read_bytes()

        self.assertEqual(completed.stdout, b"")
        self.assertEqual(completed.stderr, b"")
        self.assertEqual(generated, acceptance.generated_file_bytes())
        self.assertEqual(len(generated), (1 << 20) + 257)

    def test_large_diff_seed_command_writes_exact_workspace_payload(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            completed = subprocess.run(
                ["bash", "-c", acceptance.large_diff_seed_node_command()],
                cwd=directory,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=True,
            )
            generated = pathlib.Path(
                directory, acceptance.LARGE_DIFF_RELATIVE_PATH
            ).read_bytes()

        self.assertEqual(completed.stdout, b"")
        self.assertEqual(completed.stderr, b"")
        self.assertEqual(generated, acceptance.large_diff_seed_bytes())
        self.assertGreater(len(generated), 64 << 10)

    def test_real_provider_generated_file_checkpoint_validates_ready_snapshot_content(self) -> None:
        suite = RealProviderGeneratedFileSuite()

        evidence = suite._real_provider_generated_file_checkpoint()

        self.assertEqual(evidence["command"]["relativePath"], acceptance.GENERATED_FILE_RELATIVE_PATH)
        self.assertEqual(evidence["command"]["totalBytes"], (1 << 20) + 257)
        self.assertEqual(
            evidence["standaloneFile"]["relativePath"],
            acceptance.STANDALONE_GENERATED_FILE_RELATIVE_PATH,
        )
        self.assertEqual(evidence["checkpoint"]["strategy"], "snapshot")
        self.assertEqual(
            evidence["checkpoint"]["snapshot"]["file"],
            {
                "path": acceptance.GENERATED_FILE_RELATIVE_PATH,
                "sizeBytes": (1 << 20) + 257,
                "sha256": hashlib.sha256(acceptance.generated_file_bytes()).hexdigest(),
            },
        )
        self.assertFalse(evidence["checkpoint"]["runtimePhysicalPathLeak"])
        self.assertFalse(evidence["checkpoint"]["duplicateReadyArtifact"])
        self.assertEqual(
            evidence["checkpoint"]["generatedFileArtifact"]["artifact"],
            {
                "id": "00000000-0000-4000-8000-000000000100",
                "kind": "generated_file",
                "status": "ready",
                "originalName": pathlib.PurePosixPath(
                    acceptance.STANDALONE_GENERATED_FILE_RELATIVE_PATH
                ).name,
                "contentType": "application/octet-stream",
                "sizeBytes": len(acceptance.STANDALONE_GENERATED_FILE_CONTENT),
                "sha256": hashlib.sha256(
                    acceptance.STANDALONE_GENERATED_FILE_CONTENT
                ).hexdigest(),
            },
        )
        self.assertEqual(
            evidence["checkpoint"]["generatedFileArtifact"]["download"],
            {
                "sizeBytes": len(acceptance.STANDALONE_GENERATED_FILE_CONTENT),
                "sha256": hashlib.sha256(
                    acceptance.STANDALONE_GENERATED_FILE_CONTENT
                ).hexdigest(),
            },
        )
        self.assertLess(
            evidence["checkpoint"]["sequenceRange"]["generatedArtifactReady"],
            evidence["checkpoint"]["sequenceRange"]["workspaceDirty"],
        )
        self.assertTrue(evidence["providerTurn"]["markerMatched"])
        self.assertEqual(evidence["providerTurn"]["markerMatchMode"], "contains-once")
        self.assertIsNotNone(suite.created_input)

    def test_real_provider_generated_file_checkpoint_rejects_duplicate_ready_artifact(self) -> None:
        with self.assertRaises(acceptance.AcceptanceError) as caught:
            RealProviderGeneratedFileSuite(duplicate_ready=True)._real_provider_generated_file_checkpoint()

        self.assertEqual(caught.exception.code, "runner.generated_file_checkpoint_event_invalid")

    def test_real_provider_generated_file_checkpoint_rejects_duplicate_standalone_ready_artifact(
        self,
    ) -> None:
        with self.assertRaises(acceptance.AcceptanceError) as caught:
            RealProviderGeneratedFileSuite(
                duplicate_generated_ready=True
            )._real_provider_generated_file_checkpoint()

        self.assertEqual(caught.exception.code, "runner.generated_file_artifact_event_invalid")

    def test_real_provider_generated_file_checkpoint_rejects_standalone_download_mismatch(
        self,
    ) -> None:
        with self.assertRaises(acceptance.AcceptanceError) as caught:
            RealProviderGeneratedFileSuite(
                corrupt_generated_file=True
            )._real_provider_generated_file_checkpoint()

        self.assertEqual(caught.exception.code, "runner.generated_file_artifact_download_mismatch")

    def test_generated_file_snapshot_preserves_known_approval_sentinel(self) -> None:
        evidence = acceptance.generated_file_snapshot_evidence(
            generated_file_snapshot_bytes(
                include_approval_sentinel=True,
                include_steer_sentinel=True,
            )
        )

        self.assertEqual(evidence["regularFileCount"], 4)
        self.assertEqual(
            evidence["preservedKnownFiles"],
            [
                acceptance.STANDALONE_GENERATED_FILE_RELATIVE_PATH,
                acceptance.REAL_PROVIDER_APPROVAL_RELATIVE_PATH,
                acceptance.REAL_PROVIDER_STEER_RELATIVE_PATH,
            ],
        )

    def test_real_provider_generated_file_checkpoint_rejects_unsafe_snapshot_member(self) -> None:
        with self.assertRaises(acceptance.AcceptanceError) as caught:
            RealProviderGeneratedFileSuite(
                snapshot_member_name="../generated-file.txt"
            )._real_provider_generated_file_checkpoint()

        self.assertEqual(caught.exception.code, "runner.generated_file_snapshot_unsafe")

    def test_real_provider_generated_file_checkpoint_rejects_download_hash_mismatch(self) -> None:
        with self.assertRaises(acceptance.AcceptanceError) as caught:
            RealProviderGeneratedFileSuite(corrupt_sha256=True)._real_provider_generated_file_checkpoint()

        self.assertEqual(caught.exception.code, "runner.generated_file_checkpoint_download_mismatch")

    def test_terminal_large_node_command_emits_exact_fixture_bytes_without_newline(self) -> None:
        completed = subprocess.run(
            ["bash", "-c", acceptance.terminal_large_node_command()],
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=True,
        )

        self.assertEqual(len(completed.stdout), acceptance.TERMINAL_LARGE_TOTAL_BYTES)
        self.assertEqual(
            completed.stdout,
            acceptance.terminal_large_bytes(0, acceptance.TERMINAL_LARGE_TOTAL_BYTES),
        )
        self.assertNotEqual(completed.stdout[-1:], b"\n")

    def test_controlled_claude_terminal_large_reuses_strict_terminal_and_turn_evidence(self) -> None:
        suite = RealProviderTerminalLargeSuite()

        evidence = suite._real_provider_terminal_large_log()

        marker = suite._real_provider_marker("terminal-large")
        self.assertEqual(evidence["command"]["runtime"], "node")
        self.assertEqual(evidence["terminal"]["completion"]["totalBytes"], 2 * (1 << 20) + 257)
        self.assertEqual(evidence["terminal"]["completion"]["segmentCount"], 3)
        self.assertFalse(evidence["terminal"]["runtimePhysicalPathLeak"])
        self.assertTrue(evidence["providerTurn"]["markerMatched"])
        self.assertEqual(evidence["providerTurn"]["markerMatchMode"], "contains-once")
        self.assertEqual(
            evidence["providerTurn"]["providerResume"]["selectedStrategy"],
            "native-cursor",
        )
        self.assertEqual(suite.state.last_real_marker, marker)
        self.assertIsNotNone(suite.created_input)
        self.assertIn(acceptance.terminal_large_node_command(), suite.created_input or "")

    def test_codex_terminal_large_reports_lossless_output_unsupported(self) -> None:
        suite = RealProviderTerminalLargeSuite()
        suite.options = dataclasses.replace(suite.options, provider="codex")

        with self.assertRaises(acceptance.AcceptanceUnsupported) as caught:
            suite._real_provider_terminal_large_log()

        self.assertEqual(
            caught.exception.code,
            "runner.real_provider_terminal_large_lossless_output_unsupported",
        )
        self.assertEqual(
            caught.exception.evidence,
            {
                "provider": "codex",
                "supportMode": "unsupported",
                "providerBoundary": "unified-exec-1MiB-head-tail",
                "requestedBytes": 2 * (1 << 20) + 257,
                "retainedBytes": 1 << 20,
                "lossless": False,
                "compatibleProviderVersionRange": "0.144.x",
            },
        )
        self.assertIsNone(suite.created_input)

    def test_claude_ambient_terminal_large_requires_controlled_credential(self) -> None:
        suite = RealProviderTerminalLargeSuite()
        suite.state.credential_id = None

        with self.assertRaises(acceptance.AcceptanceUnsupported) as caught:
            suite._real_provider_terminal_large_log()

        self.assertEqual(
            caught.exception.code,
            "runner.real_provider_terminal_large_controlled_credential_required",
        )
        self.assertEqual(caught.exception.evidence["authentication"], "ambient-auth")
        self.assertFalse(caught.exception.evidence["lossless"])
        self.assertIsNone(suite.created_input)

    def test_runtime_path_detection_allows_absolute_command_executables(self) -> None:
        self.assertFalse(
            acceptance.contains_runtime_physical_path(
                {
                    "data": {
                        "terminal": {
                            "commandSummary": "/bin/zsh -lc 'node -e test'",
                            "cwdLabel": ".",
                        }
                    }
                }
            )
        )
        self.assertTrue(
            acceptance.contains_runtime_physical_path(
                {"runtimeOutputDirectory": "/tmp/.synara-runtime/execution"}
            )
        )

    def test_terminal_large_case_rejects_runtime_output_path_leak(self) -> None:
        with self.assertRaises(acceptance.AcceptanceError) as caught:
            TerminalLargeSuite(leak_runtime_path=True)._terminal_large_log()

        self.assertEqual(caught.exception.code, "runner.terminal_runtime_path_leaked")

    def test_terminal_large_case_rejects_artifact_hash_mismatch(self) -> None:
        with self.assertRaises(acceptance.AcceptanceError) as caught:
            TerminalLargeSuite(corrupt_artifact=True)._terminal_large_log()

        self.assertEqual(caught.exception.code, "runner.terminal_artifact_mismatch")

    def test_interaction_waits_distinguish_initial_and_replacement_snapshots(self) -> None:
        suite = BarrierSuite(acceptance.EXECUTION_PINNED_WORKER)

        class InteractionAPI(FakeAPI):
            def __init__(self) -> None:
                super().__init__()
                self.snapshots = [
                    {
                        "items": [
                            {
                                "id": "interaction-1",
                                "turnId": "turn-1",
                                "kind": "approval",
                            }
                        ]
                    },
                    {
                        "items": [
                            {
                                "id": "interaction-1",
                                "turnId": "turn-1",
                                "kind": "approval",
                            },
                            {
                                "id": "interaction-2",
                                "turnId": "turn-1",
                                "kind": "approval",
                            },
                        ]
                    },
                    {
                        "items": [
                            {
                                "id": "interaction-2",
                                "turnId": "turn-1",
                                "kind": "approval",
                            }
                        ]
                    },
                ]

            def request(
                self,
                method: str,
                path: str,
                payload: Mapping[str, Any] | None = None,
                expected: Sequence[int] = (200,),
                *,
                maximum_timeout: float = 10.0,
            ) -> Any:
                del method, path, payload, expected, maximum_timeout
                return self.snapshots.pop(0)

            def wait_until(
                self,
                _description: str,
                probe: Callable[[], Any | None],
                interval: float = 0.25,
            ) -> Any:
                del interval
                while self.snapshots:
                    value = probe()
                    if value is not None:
                        return value
                raise AssertionError("interaction probe did not become ready")

        api = InteractionAPI()
        suite.fake_driver.api = api  # type: ignore[assignment]

        initial = acceptance.AcceptanceSuite._wait_for_interaction(suite, "turn-1", "approval")
        replacement = acceptance.AcceptanceSuite._wait_for_replacement_interaction(
            suite,
            "turn-1",
            "approval",
            "interaction-1",
        )

        self.assertEqual(initial["id"], "interaction-1")
        self.assertEqual(replacement["id"], "interaction-2")

    def test_interaction_wait_fails_when_turn_terminates_without_request(self) -> None:
        suite = BarrierSuite(acceptance.EXECUTION_PINNED_WORKER)

        class InteractionAPI(FakeAPI):
            def request(
                self,
                method: str,
                path: str,
                payload: Mapping[str, Any] | None = None,
                expected: Sequence[int] = (200,),
                *,
                maximum_timeout: float = 10.0,
            ) -> Any:
                del method, path, payload, expected, maximum_timeout
                return {"items": []}

            def wait_until(
                self,
                _description: str,
                probe: Callable[[], Any | None],
                interval: float = 0.25,
            ) -> Any:
                del interval
                return probe()

        suite.fake_driver.api = InteractionAPI()  # type: ignore[assignment]
        suite._all_events = mock.Mock(  # type: ignore[method-assign]
            return_value=[
                {
                    "sequence": 1,
                    "eventType": "turn.created",
                    "executionId": "execution-1",
                    "payload": {"turnId": "turn-1"},
                },
                {
                    "sequence": 2,
                    "eventType": "execution.completed",
                    "executionId": "execution-1",
                    "payload": {"turnId": "turn-1"},
                },
            ]
        )

        with self.assertRaises(acceptance.AcceptanceError) as caught:
            acceptance.AcceptanceSuite._wait_for_interaction(suite, "turn-1", "approval")

        self.assertEqual(caught.exception.code, "runner.interaction_missing_after_terminal")
        self.assertEqual(caught.exception.evidence["expectedInteractionKind"], "approval")
        self.assertEqual(caught.exception.evidence["terminal"]["eventType"], "execution.completed")

    def test_standing_worker_preserves_existing_case_order(self) -> None:
        suite = CaseOrderSuite(FakeDriver(acceptance.STANDING_WORKER))

        suite.run()

        self.assertEqual(
            suite.case_order,
            [
                "environment.target-prepare",
                "environment.control-plane-start",
                "identity.dev-login",
                "runtime.worker-discovery",
                "resources.credential-project-session",
                "fixture.text-tool-usage-artifact",
                "fixture.approval-resolution",
                "fixture.terminal-large-log",
                "fixture.user-input-resolution",
                "fixture.provider-error",
                "recovery.control-plane-restart",
                "fixture.second-turn-continuity",
            ],
        )

    def test_fixture_soak_appends_long_session_case_after_restart_continuity(self) -> None:
        suite = CaseOrderSuite(
            FakeDriver(acceptance.STANDING_WORKER),
            dataclasses.replace(
                runner_options(),
                suite="fixture-soak",
                soak_turns=10,
                soak_restart_every=5,
            ),
        )

        suite.run()

        self.assertEqual(
            suite.case_order[-2:],
            ["fixture.second-turn-continuity", "soak.multi-turn-restart-continuity"],
        )

    def test_fixture_concurrency_uses_dedicated_two_worker_case_order(self) -> None:
        suite = CaseOrderSuite(
            FakeDriver(acceptance.STANDING_MANAGED_WORKER, name="docker"),
            dataclasses.replace(
                runner_options(),
                target="docker",
                suite="fixture-concurrency",
                restart_control_plane=False,
            ),
        )

        suite.run()

        self.assertEqual(
            suite.case_order,
            [
                "environment.target-prepare",
                "environment.control-plane-start",
                "identity.dev-login",
                "runtime.target-provision",
                "runtime.concurrent-worker-discovery",
                "resources.credential-project-session",
                "concurrency.multi-provider-multi-session",
            ],
        )

    def test_fixture_load_reuses_parallel_worker_setup_with_one_load_case(self) -> None:
        suite = CaseOrderSuite(
            FakeDriver(acceptance.STANDING_MANAGED_WORKER, name="docker"),
            dataclasses.replace(
                runner_options(),
                target="docker",
                suite="fixture-load",
                load_waves=acceptance.FIXTURE_LOAD_DEFAULT_WAVES,
                restart_control_plane=False,
            ),
        )

        suite.run()

        self.assertEqual(
            suite.case_order,
            [
                "environment.target-prepare",
                "environment.control-plane-start",
                "identity.dev-login",
                "runtime.target-provision",
                "runtime.concurrent-worker-discovery",
                "resources.credential-project-session",
                "load.multi-session-admission-waves",
            ],
        )

    def test_fixture_load_failure_runs_targeted_recovery_before_post_failure_load(self) -> None:
        suite = CaseOrderSuite(
            FakeDriver(acceptance.STANDING_MANAGED_WORKER, name="docker"),
            dataclasses.replace(
                runner_options(),
                target="docker",
                suite="fixture-load-failure",
                load_waves=acceptance.FIXTURE_LOAD_DEFAULT_WAVES,
                restart_control_plane=False,
            ),
        )

        suite.run()

        self.assertEqual(
            suite.case_order,
            [
                "environment.target-prepare",
                "environment.control-plane-start",
                "identity.dev-login",
                "runtime.target-provision",
                "runtime.concurrent-worker-discovery",
                "resources.credential-project-session",
                "load.targeted-worker-network-recovery",
                "load.targeted-worker-container-loss-recovery",
                "load.targeted-provider-host-process-crash",
                "load.post-failure-admission-waves",
            ],
        )

    def test_fixture_retention_concurrency_uses_local_fencing_case_order(self) -> None:
        suite = CaseOrderSuite(
            FakeDriver(acceptance.STANDING_WORKER, name="local"),
            dataclasses.replace(
                runner_options(),
                target="local",
                suite="fixture-retention-concurrency",
                restart_control_plane=False,
            ),
        )

        suite.run()

        self.assertEqual(
            suite.case_order,
            [
                "environment.target-prepare",
                "environment.control-plane-start",
                "identity.dev-login",
                "runtime.worker-discovery",
                "resources.credential-project-session",
                "retention.seed-artifact-checkpoint",
                "retention.active-execution-cleanup-fencing",
            ],
        )

    def test_real_provider_smoke_uses_two_turn_restart_case_order(self) -> None:
        suite = CaseOrderSuite(
            FakeDriver(acceptance.EXECUTION_PINNED_WORKER),
            dataclasses.replace(runner_options(), suite="real-provider-smoke"),
        )

        suite.run()

        self.assertEqual(
            suite.case_order,
            [
                "environment.target-prepare",
                "environment.control-plane-start",
                "identity.dev-login",
                "runtime.target-provision",
                "resources.real-provider-project-session",
                "real-provider.turn-1-start",
                "runtime.real-provider-worker-discovery",
                "real-provider.turn-1",
                "recovery.control-plane-restart",
                "real-provider.turn-2-continuity",
            ],
        )

    def test_real_provider_cases_are_canonical_and_run_before_restart(self) -> None:
        suite = CaseOrderSuite(
            FakeDriver(acceptance.EXECUTION_PINNED_WORKER),
            dataclasses.replace(
                runner_options(),
                suite="real-provider-smoke",
                real_provider_cases=(
                    "approval",
                    "user-input",
                    "generated-file-checkpoint",
                    "terminal-large",
                ),
            ),
        )

        suite.run()

        self.assertEqual(
            suite.case_order,
            [
                "environment.target-prepare",
                "environment.control-plane-start",
                "identity.dev-login",
                "runtime.target-provision",
                "resources.real-provider-project-session",
                "real-provider.turn-1-start",
                "runtime.real-provider-worker-discovery",
                "real-provider.turn-1",
                "real-provider.approval-resolution",
                "real-provider.user-input-resolution",
                "real-provider.generated-file-checkpoint",
                "real-provider.terminal-large-log",
                "recovery.control-plane-restart",
                "real-provider.turn-2-continuity",
            ],
        )

    def test_real_provider_terminal_large_selected_alone_precedes_restart_continuity(self) -> None:
        suite = CaseOrderSuite(
            FakeDriver(acceptance.EXECUTION_PINNED_WORKER),
            dataclasses.replace(
                runner_options(),
                suite="real-provider-smoke",
                real_provider_cases=("terminal-large",),
            ),
        )

        suite.run()

        self.assertEqual(
            suite.case_order[-3:],
            [
                "real-provider.terminal-large-log",
                "recovery.control-plane-restart",
                "real-provider.turn-2-continuity",
            ],
        )

    def test_real_provider_advanced_cases_run_after_restart_continuity(self) -> None:
        suite = CaseOrderSuite(
            FakeDriver(acceptance.EXECUTION_PINNED_WORKER),
            dataclasses.replace(
                runner_options(),
                suite="real-provider-smoke",
                real_provider_cases=(
                    "generated-file-checkpoint",
                    "terminal-large",
                    "review",
                    "compact",
                    "rollback",
                    "fork",
                ),
            ),
        )

        suite.run()

        self.assertEqual(
            suite.case_order,
            [
                "environment.target-prepare",
                "environment.control-plane-start",
                "identity.dev-login",
                "runtime.target-provision",
                "resources.real-provider-project-session",
                "real-provider.turn-1-start",
                "runtime.real-provider-worker-discovery",
                "real-provider.turn-1",
                "real-provider.generated-file-checkpoint",
                "real-provider.terminal-large-log",
                "recovery.control-plane-restart",
                "real-provider.turn-2-continuity",
                "real-provider.review",
                "real-provider.compact-boundary",
                "real-provider.rollback-emulation",
                "real-provider.fork-emulation",
            ],
        )

    def test_real_provider_failure_matrix_runs_before_restart_and_expiry_continuity(self) -> None:
        suite = CaseOrderSuite(
            FakeDriver(acceptance.STANDING_WORKER, name="local"),
            dataclasses.replace(
                runner_options(),
                suite="real-provider-smoke",
                real_provider_failure_cases=acceptance.REAL_PROVIDER_FAILURE_CASES,
            ),
        )

        suite.run()

        self.assertEqual(
            suite.case_order,
            [
                "environment.target-prepare",
                "environment.control-plane-start",
                "identity.dev-login",
                "runtime.target-provision",
                "resources.real-provider-project-session",
                "real-provider.turn-1-start",
                "runtime.real-provider-worker-discovery",
                "real-provider.turn-1",
                "real-provider.failure-authentication",
                "real-provider.failure-rate-limit-retry",
                "real-provider.failure-host-crash-retry",
                "real-provider.failure-cursor-expiry",
                "recovery.control-plane-restart",
                "real-provider.turn-2-continuity",
            ],
        )

    def test_real_provider_marker_and_native_resume_evidence_pass(self) -> None:
        suite = BarrierSuite(acceptance.EXECUTION_PINNED_WORKER)
        marker = "SYNARA_REAL_PROVIDER_SMOKE_CODEX_0123456789ABCDEF"
        terminal, events = real_provider_turn_events(
            marker + "\n",
            terminal_text=marker,
            selected_strategy="native-cursor",
            reason_code="cursor_usable",
        )

        evidence = suite._real_provider_turn_evidence(
            "turn-1",
            terminal,
            events,
            marker,
            expected_resume_strategy="native-cursor",
            expected_resume_reason="cursor_usable",
        )

        self.assertTrue(evidence["markerMatched"])
        self.assertTrue(evidence["terminalOutputMatched"])
        self.assertEqual(evidence["providerResume"]["selectedStrategy"], "native-cursor")

    def test_real_provider_cursor_expiry_continuity_requires_authoritative_history(self) -> None:
        suite = BarrierSuite(acceptance.STANDING_WORKER)
        suite.options = dataclasses.replace(
            suite.options,
            suite="real-provider-smoke",
            real_provider_failure_cases=("cursor-expiry",),
        )
        suite.state.pre_restart_sequence = 1
        suite.state.last_real_marker = "SYNARA_REAL_PROVIDER_CONTINUITY_CODEX_EXPIRED"
        suite.state.first_worker_id = "worker-1"
        suite.state.first_generation = 1
        terminal = {
            "sequence": 2,
            "eventType": "execution.completed",
            "executionId": "execution-2",
            "workerId": "worker-1",
            "generation": 1,
        }
        turn_events = [{"sequence": 2, "eventType": "execution.completed"}]
        suite._wait_for_turn_terminal = mock.Mock(return_value=(terminal, turn_events))  # type: ignore[method-assign]
        suite._real_provider_turn_evidence = mock.Mock(return_value={})  # type: ignore[method-assign]
        suite._all_events = mock.Mock(  # type: ignore[method-assign]
            return_value=[
                {"sequence": 1, "eventType": "execution.completed"},
                terminal,
            ]
        )

        evidence = suite._real_provider_second_turn_continuity()

        suite._real_provider_turn_evidence.assert_called_once_with(
            "turn-1",
            terminal,
            turn_events,
            "SYNARA_REAL_PROVIDER_CONTINUITY_CODEX_EXPIRED",
            expected_resume_strategy="authoritative-history",
            expected_resume_reason="cursor_expired",
        )
        self.assertIn("authoritative history", evidence["continuityAssertion"])

    def test_real_provider_marker_mismatch_fails_closed(self) -> None:
        suite = BarrierSuite(acceptance.EXECUTION_PINNED_WORKER)
        terminal, events = real_provider_turn_events(
            "wrong marker",
            selected_strategy="authoritative-history",
            reason_code="cursor_absent",
        )

        with self.assertRaises(acceptance.AcceptanceError) as raised:
            suite._real_provider_turn_evidence(
                "turn-1",
                terminal,
                events,
                "expected marker",
                expected_resume_strategy="authoritative-history",
                expected_resume_reason="cursor_absent",
            )

        self.assertEqual(raised.exception.code, "runner.real_provider_marker_mismatch")

    def test_real_provider_generated_file_dispatches_to_canonical_handler(self) -> None:
        suite = BarrierSuite(acceptance.EXECUTION_PINNED_WORKER)
        expected = {"checkpoint": {"strategy": "snapshot"}}
        suite._real_provider_generated_file_checkpoint = mock.Mock(  # type: ignore[method-assign]
            return_value=expected
        )

        actual = suite._execute_real_provider_case("generated-file-checkpoint")

        self.assertEqual(actual, expected)
        suite._real_provider_generated_file_checkpoint.assert_called_once_with()

    def test_real_provider_large_diff_dispatches_to_canonical_handler(self) -> None:
        suite = BarrierSuite(acceptance.EXECUTION_PINNED_WORKER)
        expected = {"diff": {"artifact": {"kind": "diff"}}}
        suite._real_provider_large_diff_artifact = mock.Mock(return_value=expected)  # type: ignore[method-assign]

        actual = suite._execute_real_provider_case("large-diff")

        self.assertEqual(actual, expected)
        suite._real_provider_large_diff_artifact.assert_called_once_with()

    def test_real_provider_terminal_large_dispatches_to_canonical_handler(self) -> None:
        suite = BarrierSuite(acceptance.EXECUTION_PINNED_WORKER)
        expected = {"terminal": {"completion": {"totalBytes": 2 * (1 << 20) + 257}}}
        suite._real_provider_terminal_large_log = mock.Mock(return_value=expected)  # type: ignore[method-assign]

        actual = suite._execute_real_provider_case("terminal-large")

        self.assertEqual(actual, expected)
        suite._real_provider_terminal_large_log.assert_called_once_with()

    def test_execution_pinned_worker_provisions_resources_before_barrier(self) -> None:
        suite = CaseOrderSuite(FakeDriver(acceptance.EXECUTION_PINNED_WORKER))

        suite.run()

        self.assertEqual(
            suite.case_order,
            [
                "environment.target-prepare",
                "environment.control-plane-start",
                "identity.dev-login",
                "runtime.target-provision",
                "resources.credential-project-session",
                "runtime.worker-discovery",
                "fixture.approval-resolution",
                "fixture.text-tool-usage-artifact",
                "fixture.terminal-large-log",
                "fixture.user-input-resolution",
                "fixture.provider-error",
                "recovery.control-plane-restart",
                "fixture.second-turn-continuity",
            ],
        )

    def test_replacement_cases_follow_capability_not_driver_name(self) -> None:
        managed = CaseOrderSuite(FakeDriver(acceptance.STANDING_MANAGED_WORKER, name="not-docker"))
        unmanaged_docker = CaseOrderSuite(FakeDriver(acceptance.STANDING_WORKER, name="docker"))

        managed.run()
        unmanaged_docker.run()

        self.assertIn("recovery.worker-replacement", managed.case_order)
        self.assertIn("recovery.post-replacement-workspace-turn", managed.case_order)
        self.assertNotIn("recovery.worker-replacement", unmanaged_docker.case_order)
        self.assertNotIn("recovery.post-replacement-workspace-turn", unmanaged_docker.case_order)

    def test_selected_failure_matrix_is_ordered_before_replacement_and_restart(self) -> None:
        suite = CaseOrderSuite(
            FakeDriver(acceptance.STANDING_MANAGED_WORKER, name="docker"),
            dataclasses.replace(
                runner_options(),
                failure_cases=("provider-malformed", "worker-network"),
            ),
        )

        suite.run()

        malformed = suite.case_order.index("failure.provider-host-malformed")
        network = suite.case_order.index("failure.worker-network-interruption")
        replacement = suite.case_order.index("recovery.worker-replacement")
        restart = suite.case_order.index("recovery.control-plane-restart")
        self.assertLess(malformed, network)
        self.assertLess(network, replacement)
        self.assertLess(replacement, restart)

    def test_failure_only_uses_minimal_setup_and_continuity_smoke(self) -> None:
        suite = CaseOrderSuite(
            FakeDriver(acceptance.STANDING_WORKER, name="local"),
            dataclasses.replace(
                runner_options(),
                failure_cases=("provider-malformed",),
                failure_only=True,
            ),
        )

        suite.run()

        self.assertEqual(
            suite.case_order,
            [
                "environment.target-prepare",
                "environment.control-plane-start",
                "identity.dev-login",
                "runtime.worker-discovery",
                "resources.credential-project-session",
                "fixture.baseline-smoke",
                "failure.provider-host-malformed",
                "fixture.post-failure-continuity",
            ],
        )

    def test_pending_interaction_recovery_cases_follow_driver_capability(self) -> None:
        recovering = FakeDriver(acceptance.EXECUTION_PINNED_WORKER, name="kubernetes-like")
        recovering.pending_interaction_recovery = "delete-pod"
        suite = CaseOrderSuite(recovering)

        suite.run()

        self.assertEqual(
            suite.case_order,
            [
                "environment.target-prepare",
                "environment.control-plane-start",
                "identity.dev-login",
                "runtime.target-provision",
                "resources.credential-project-session",
                "runtime.worker-discovery",
                "recovery.pending-approval-runtime-loss",
                "fixture.approval-resolution",
                "fixture.text-tool-usage-artifact",
                "fixture.terminal-large-log",
                "fixture.user-input-resolution",
                "fixture.provider-error",
                "recovery.control-plane-restart",
                "fixture.second-turn-continuity",
            ],
        )

    def test_execution_pinned_approval_barrier_is_resolved_without_second_turn(self) -> None:
        suite = BarrierSuite(acceptance.EXECUTION_PINNED_WORKER)

        discovery = suite._discover_worker()
        resolution = suite._approval_resolution()

        self.assertEqual(suite.created_turns, ["[approval]"])
        self.assertEqual(suite.interaction_waits, 1)
        self.assertIsNone(suite.state.pending_approval)
        self.assertEqual(discovery["readinessBarrier"]["executionId"], "execution-1")
        self.assertEqual(resolution["turnId"], "turn-1")
        self.assertEqual(
            suite.fake_driver.api.requests,
            [
                (
                    "POST",
                    "/v1/executions/execution-1/approvals/approval-request-1/resolve",
                    {"decision": "accept"},
                )
            ],
        )

    def test_execution_pinned_restart_defers_worker_wait_until_next_execution(self) -> None:
        suite = BarrierSuite(acceptance.EXECUTION_PINNED_WORKER)

        evidence = suite._restart_control_plane()

        self.assertEqual(suite.post_restart_waits, 0)
        self.assertEqual(evidence["workerAllocation"], "execution-pinned")
        self.assertEqual(evidence["postRestartWorkerExpectation"], "deferred-until-next-execution")

    def test_pending_approval_runtime_recovery_rebinds_the_barrier(self) -> None:
        suite = PendingApprovalRecoverySuite()

        discovery = suite._discover_worker()
        recovery = suite._recover_pending_approval_runtime()
        resolution = suite._approval_resolution()

        self.assertEqual(discovery["readinessBarrier"]["requestId"], "approval-request-1")
        self.assertTrue(suite.recovered)
        self.assertEqual(recovery["staleInteractionId"], "interaction-1")
        self.assertEqual(recovery["replacementInteractionId"], "interaction-2")
        self.assertEqual(recovery["replacementRequestId"], "approval-request-2")
        self.assertEqual(recovery["targetRuntime"]["podUid"], "pod-uid-2")
        self.assertEqual(resolution["requestId"], "approval-request-2")
        self.assertEqual(
            suite.fake_driver.api.requests,
            [
                (
                    "POST",
                    "/v1/executions/execution-1/approvals/approval-request-2/resolve",
                    {"decision": "accept"},
                )
            ],
        )

    def test_pending_approval_runtime_recovery_rejects_reused_request_identity(self) -> None:
        suite = PendingApprovalRecoverySuite(replacement_request_id="approval-request-1")

        suite._discover_worker()
        with self.assertRaises(acceptance.AcceptanceError) as raised:
            suite._recover_pending_approval_runtime()

        self.assertEqual(raised.exception.code, "runner.pending_interaction_request_not_replaced")

    def test_provider_host_fault_requires_stable_code_and_next_turn_recovery(self) -> None:
        suite = ProviderFailureSuite("protocol_violation")

        evidence = suite._provider_host_failure(
            "provider-malformed",
            "[provider-malformed]",
            "protocol_violation",
        )

        self.assertEqual(suite.created_turns, ["[provider-malformed]", "[text]"])
        self.assertEqual(evidence["failureCode"], "protocol_violation")
        self.assertTrue(evidence["hostRecoveredForNextTurn"])

    def test_provider_host_fault_rejects_unexpected_code(self) -> None:
        suite = ProviderFailureSuite("provider_unavailable")

        with self.assertRaises(acceptance.AcceptanceError) as raised:
            suite._provider_host_failure(
                "provider-malformed",
                "[provider-malformed]",
                "protocol_violation",
            )

        self.assertEqual(raised.exception.code, "runner.provider_fault_code_mismatch")

    def test_worker_network_failure_recovers_and_resolves_new_generation(self) -> None:
        suite = PendingApprovalRecoverySuite()
        suite.fake_driver.validate_failure = lambda fault: None  # type: ignore[attr-defined]
        suite.fake_driver.inject_failure = (  # type: ignore[attr-defined]
            lambda fault, target_id, execution_id: suite._recover_pending_interaction(
                target_id, execution_id
            )
        )

        evidence = suite._pending_approval_failure("worker-network")

        self.assertEqual(evidence["recovery"]["replacementRequestId"], "approval-request-2")
        self.assertTrue(evidence["generationFenced"])
        self.assertEqual(evidence["resolution"]["executionId"], "execution-1")

    def test_real_provider_recovery_reports_controlled_product_credential(self) -> None:
        suite = BarrierSuite(acceptance.STANDING_WORKER)
        suite.state.credential_id = "credential-1"
        suite._create_real_provider_session = mock.Mock(  # type: ignore[method-assign]
            return_value={"id": "session-recovery"}
        )
        suite._create_turn = mock.Mock(return_value={"id": "turn-recovery"})  # type: ignore[method-assign]
        terminal = {"sequence": 1, "eventType": "execution.completed"}
        suite._wait_for_turn_terminal = mock.Mock(  # type: ignore[method-assign]
            return_value=(terminal, [terminal])
        )
        suite._real_provider_turn_evidence = mock.Mock(  # type: ignore[method-assign]
            return_value={"markerMatched": True}
        )

        evidence = suite._real_provider_recovery_turn("provider-host-crash-retry")

        self.assertFalse(evidence["ambientAuthentication"])
        suite._create_real_provider_session.assert_called_once_with(
            title="Stage 3 Real Provider provider-host-crash-retry Recovery"
        )

    def test_standing_restart_waits_for_online_worker(self) -> None:
        suite = BarrierSuite(acceptance.STANDING_WORKER)

        evidence = suite._restart_control_plane()

        self.assertEqual(suite.post_restart_waits, 1)
        self.assertEqual(evidence["postRestartManifestId"], "manifest-after-restart")

    def test_real_provider_resources_bind_controlled_environment_credential(self) -> None:
        secret = "stage3-real-provider-product-secret"
        base_url = "https://provider.example.test/v1"
        options = dataclasses.replace(
            runner_options(),
            target="docker",
            provider="codex",
            suite="real-provider-smoke",
            real_provider_credential_env="SYNARA_ACCEPTANCE_PROVIDER_KEY",
            real_provider_base_url_env="SYNARA_ACCEPTANCE_PROVIDER_BASE_URL",
        )
        driver = FakeDriver(acceptance.STANDING_MANAGED_WORKER, name="docker")

        class ResourceAPI(FakeAPI):
            def request(
                self,
                method: str,
                path: str,
                payload: Mapping[str, Any] | None = None,
                expected: Sequence[int] = (200,),
                *,
                maximum_timeout: float = 10.0,
            ) -> Any:
                del expected, maximum_timeout
                self.requests.append((method, path, payload))
                if path.endswith("/credentials"):
                    return {
                        "id": "credential-1",
                        "organizationId": "organization-id",
                        "provider": "codex",
                        "credentialType": "api_key",
                        "version": 1,
                    }
                if path.endswith("/projects"):
                    return {
                        "id": "project-1",
                        "organizationId": "organization-id",
                        "repositoryUrl": None,
                    }
                if path.endswith("/sessions"):
                    assert payload is not None
                    return {
                        "id": "session-1",
                        "provider": payload.get("provider"),
                        "executionTargetId": payload.get("executionTargetId"),
                        "providerCredentialId": payload.get("providerCredentialId"),
                        "lastEventSequence": 1,
                    }
                raise AssertionError(f"unexpected resource request: {method} {path}")

        api = ResourceAPI()
        driver.api = api
        redactor = acceptance.SecretRedactor()
        suite = acceptance.AcceptanceSuite(
            options,
            driver,
            acceptance.Deadline(30.0),
            redactor,
        )
        suite.state.tenant_id = "tenant-id"
        suite.state.organization_id = "organization-id"
        suite.state.target_id = "target-id"

        with mock.patch.dict(
            os.environ,
            {
                "SYNARA_ACCEPTANCE_PROVIDER_KEY": secret,
                "SYNARA_ACCEPTANCE_PROVIDER_BASE_URL": base_url,
            },
        ):
            evidence = suite._create_resources()

        credential_request = next(request for request in api.requests if request[1].endswith("/credentials"))
        session_request = next(request for request in api.requests if request[1].endswith("/sessions"))
        self.assertEqual(
            credential_request[2],
            {
                "organizationId": "organization-id",
                "name": "Stage 3 Real Provider Acceptance",
                "purpose": "provider",
                "provider": "codex",
                "credentialType": "api_key",
                "payload": {"apiKey": secret, "baseUrl": base_url},
            },
        )
        self.assertEqual(session_request[2]["providerCredentialId"], "credential-1")  # type: ignore[index]
        self.assertNotIn("model", session_request[2])  # type: ignore[operator]
        self.assertEqual(evidence["credential"]["source"], "operator-environment")
        self.assertFalse(evidence["credential"]["environmentVariableNamePersisted"])
        self.assertNotIn(secret, json.dumps(evidence))
        self.assertNotIn(base_url, json.dumps(evidence))
        self.assertEqual(redactor.text(secret), "[REDACTED_REAL_PROVIDER_CREDENTIAL]")
        self.assertEqual(redactor.text(base_url), "[REDACTED_REAL_PROVIDER_BASE_URL]")

    def test_real_provider_resources_fail_closed_when_credential_env_is_missing(self) -> None:
        options = dataclasses.replace(
            runner_options(),
            target="docker",
            suite="real-provider-smoke",
            real_provider_credential_env="SYNARA_ACCEPTANCE_MISSING_KEY",
        )
        driver = FakeDriver(acceptance.STANDING_MANAGED_WORKER, name="docker")
        suite = acceptance.AcceptanceSuite(
            options,
            driver,
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        suite.state.tenant_id = "tenant-id"
        suite.state.organization_id = "organization-id"
        suite.state.target_id = "target-id"

        with mock.patch.dict(os.environ, {"SYNARA_ACCEPTANCE_MISSING_KEY": ""}):
            with self.assertRaises(acceptance.AcceptanceError) as caught:
                suite._create_resources()

        self.assertEqual(caught.exception.code, "runner.real_provider_credential_env_missing")
        self.assertFalse(caught.exception.evidence["environmentVariableNamePersisted"])
        self.assertEqual(driver.api.requests, [])

        with mock.patch.dict(os.environ, {"SYNARA_ACCEPTANCE_MISSING_KEY": "unsafe\nvalue"}):
            with self.assertRaises(acceptance.AcceptanceError) as invalid:
                suite._create_resources()

        self.assertEqual(invalid.exception.code, "runner.real_provider_credential_env_invalid")
        self.assertEqual(driver.api.requests, [])


class MarkdownReportTest(unittest.TestCase):
    def test_fixture_load_report_renders_boundary_and_requested_shape(self) -> None:
        report = {
            "schemaVersion": acceptance.SCHEMA_VERSION,
            "runId": "run-load",
            "mode": "fixture-load",
            "target": "docker",
            "provider": "codex",
            "status": "pass",
            "startedAt": "2026-07-17T00:00:00Z",
            "finishedAt": "2026-07-17T00:01:00Z",
            "durationMs": 60_000,
            "configuration": {
                "load": {
                    "workers": 2,
                    "sessions": 4,
                    "waves": 25,
                    "boundary": "bounded deterministic load only",
                },
                "failureMatrix": {"requestedCases": []},
                "realProvider": {"requestedCases": [], "requestedFailureCases": []},
            },
            "cases": [],
        }

        rendered = acceptance.markdown_from_report(report)

        self.assertIn("# Stage 3 Provider Fixture Load Acceptance", rendered)
        self.assertIn("bounded deterministic load only", rendered)
        self.assertIn("## Requested fixture load", rendered)
        self.assertIn('"waves": 25', rendered)

    def test_fixture_load_failure_report_renders_targeting_and_post_recovery_load(self) -> None:
        report = {
            "schemaVersion": acceptance.SCHEMA_VERSION,
            "runId": "run-load-failure",
            "mode": "fixture-load-failure",
            "target": "docker",
            "provider": "codex",
            "status": "pass",
            "startedAt": "2026-07-17T00:00:00Z",
            "finishedAt": "2026-07-17T00:01:00Z",
            "durationMs": 60_000,
            "configuration": {
                "load": {
                    "workers": 2,
                    "sessions": 4,
                    "waves": 25,
                    "boundary": "targeted deterministic failure and bounded load only",
                },
                "loadFailure": {
                    "faults": [
                        "worker-network",
                        "worker-container-loss",
                        "provider-host-process-crash",
                    ],
                    "targeting": "execution to exact container",
                },
                "failureMatrix": {"requestedCases": []},
                "realProvider": {"requestedCases": [], "requestedFailureCases": []},
            },
            "cases": [],
        }

        rendered = acceptance.markdown_from_report(report)

        self.assertIn("# Stage 3 Provider Fixture Load Failure Acceptance", rendered)
        self.assertIn("## Requested fixture load failure", rendered)
        self.assertIn('"worker-container-loss"', rendered)
        self.assertIn('"provider-host-process-crash"', rendered)
        self.assertIn("## Requested fixture load", rendered)


class RunnerOptionsTest(unittest.TestCase):
    def test_provider_choices_use_canonical_antigravity_name(self) -> None:
        options = acceptance.parse_args(["--provider", "antigravity"])

        self.assertEqual(options.provider, "antigravity")
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            acceptance.parse_args(["--provider", "gemini"])

    def test_fixture_soak_uses_canonical_defaults(self) -> None:
        options = acceptance.parse_args(["--suite", "fixture-soak"])

        self.assertEqual(options.soak_turns, 100)
        self.assertEqual(options.soak_restart_every, 10)
        self.assertTrue(options.restart_control_plane)

    def test_fixture_soak_parses_explicit_bounds(self) -> None:
        options = acceptance.parse_args(
            [
                "--suite",
                "fixture-soak",
                "--soak-turns",
                "40",
                "--soak-restart-every",
                "8",
            ]
        )

        self.assertEqual(options.soak_turns, 40)
        self.assertEqual(options.soak_restart_every, 8)

    def test_fixture_soak_rejects_noncanonical_combinations(self) -> None:
        invalid_arguments = (
            ["--soak-turns", "10"],
            ["--suite", "fixture-soak", "--soak-turns", "9"],
            ["--suite", "fixture-soak", "--soak-turns", "20", "--soak-restart-every", "20"],
            ["--suite", "fixture-soak", "--no-restart-control-plane"],
            ["--suite", "fixture-soak", "--failure-only", "--failure-case", "provider-crash"],
        )
        for arguments in invalid_arguments:
            with self.subTest(arguments=arguments):
                with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                    acceptance.parse_args(arguments)

    def test_fixture_concurrency_uses_canonical_docker_shape(self) -> None:
        options = acceptance.parse_args(
            ["--suite", "fixture-concurrency", "--target", "docker"]
        )

        self.assertEqual(options.suite, "fixture-concurrency")
        self.assertEqual(options.target, "docker")
        self.assertFalse(options.restart_control_plane)
        self.assertEqual(options.timeout_seconds, 900.0)

    def test_fixture_concurrency_rejects_other_targets_and_failure_matrix(self) -> None:
        invalid_arguments = (
            ["--suite", "fixture-concurrency"],
            [
                "--suite",
                "fixture-concurrency",
                "--target",
                "docker",
                "--failure-case",
                "provider-crash",
            ],
        )
        for arguments in invalid_arguments:
            with self.subTest(arguments=arguments):
                with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                    acceptance.parse_args(arguments)

    def test_fixture_load_uses_canonical_docker_shape(self) -> None:
        options = acceptance.parse_args(
            ["--suite", "fixture-load", "--target", "docker"]
        )

        self.assertEqual(options.suite, "fixture-load")
        self.assertEqual(options.target, "docker")
        self.assertEqual(options.load_waves, acceptance.FIXTURE_LOAD_DEFAULT_WAVES)
        self.assertFalse(options.restart_control_plane)
        self.assertEqual(options.timeout_seconds, 900.0)

    def test_fixture_load_parses_bounds_and_rejects_noncanonical_combinations(self) -> None:
        options = acceptance.parse_args(
            [
                "--suite",
                "fixture-load",
                "--target",
                "docker",
                "--load-waves",
                "8",
            ]
        )
        self.assertEqual(options.load_waves, 8)

        invalid_arguments = (
            ["--load-waves", "8"],
            ["--suite", "fixture-load"],
            [
                "--suite",
                "fixture-load",
                "--target",
                "docker",
                "--load-waves",
                str(acceptance.FIXTURE_LOAD_MIN_WAVES - 1),
            ],
            [
                "--suite",
                "fixture-load",
                "--target",
                "docker",
                "--load-waves",
                str(acceptance.FIXTURE_LOAD_MAX_WAVES + 1),
            ],
            [
                "--suite",
                "fixture-load",
                "--target",
                "docker",
                "--failure-case",
                "provider-crash",
            ],
        )
        for arguments in invalid_arguments:
            with self.subTest(arguments=arguments):
                with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                    acceptance.parse_args(arguments)

    def test_fixture_load_failure_uses_canonical_docker_shape(self) -> None:
        options = acceptance.parse_args(
            [
                "--suite",
                "fixture-load-failure",
                "--target",
                "docker",
                "--load-waves",
                "8",
            ]
        )

        self.assertEqual(options.suite, "fixture-load-failure")
        self.assertEqual(options.target, "docker")
        self.assertEqual(options.load_waves, 8)
        self.assertFalse(options.restart_control_plane)
        self.assertEqual(options.timeout_seconds, 900.0)

        invalid_arguments = (
            ["--suite", "fixture-load-failure"],
            [
                "--suite",
                "fixture-load-failure",
                "--target",
                "docker",
                "--failure-case",
                "worker-network",
            ],
        )
        for arguments in invalid_arguments:
            with self.subTest(arguments=arguments):
                with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                    acceptance.parse_args(arguments)

    def test_fixture_retention_concurrency_uses_canonical_local_shape(self) -> None:
        options = acceptance.parse_args(
            ["--suite", "fixture-retention-concurrency", "--target", "local"]
        )

        self.assertEqual(options.suite, "fixture-retention-concurrency")
        self.assertEqual(options.target, "local")
        self.assertFalse(options.restart_control_plane)
        self.assertEqual(options.timeout_seconds, 180.0)

    def test_fixture_retention_concurrency_rejects_other_targets_and_failure_matrix(self) -> None:
        invalid_arguments = (
            ["--suite", "fixture-retention-concurrency", "--target", "docker"],
            [
                "--suite",
                "fixture-retention-concurrency",
                "--target",
                "local",
                "--failure-case",
                "provider-crash",
            ],
        )
        for arguments in invalid_arguments:
            with self.subTest(arguments=arguments):
                with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                    acceptance.parse_args(arguments)

    def test_real_provider_smoke_requires_explicit_runner_command(self) -> None:
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            acceptance.parse_args(["--suite", "real-provider-smoke"])

    def test_real_provider_smoke_rejects_fixture_failure_matrix(self) -> None:
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            acceptance.parse_args(
                [
                    "--suite",
                    "real-provider-smoke",
                    "--runner-command-json",
                    '["node","/tmp/provider-host.mjs"]',
                    "--failure-matrix",
                ]
            )

    def test_real_provider_smoke_parses_explicit_runner_command(self) -> None:
        options = acceptance.parse_args(
            [
                "--suite",
                "real-provider-smoke",
                "--provider",
                "claudeAgent",
                "--runner-command-json",
                '["node","/tmp/provider-host.mjs"]',
            ]
        )

        self.assertEqual(options.suite, "real-provider-smoke")
        self.assertEqual(options.provider, "claudeAgent")
        self.assertEqual(options.runner_command, ("node", "/tmp/provider-host.mjs"))
        self.assertEqual(options.failure_cases, ())
        self.assertEqual(options.real_provider_cases, ())
        self.assertEqual(options.real_provider_failure_cases, ())
        self.assertIsNone(options.real_provider_credential_env)
        self.assertEqual(options.real_provider_credential_field, "apiKey")
        self.assertIsNone(options.real_provider_base_url_env)

    def test_remote_real_provider_requires_controlled_credential_source(self) -> None:
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            acceptance.parse_args(
                [
                    "--suite",
                    "real-provider-smoke",
                    "--target",
                    "docker",
                    "--runner-command-json",
                    '["node","/opt/synara/provider-host/index.mjs"]',
                ]
            )

    def test_remote_real_provider_parses_controlled_credential_source(self) -> None:
        with mock.patch.dict(
            os.environ,
            {
                "SYNARA_ACCEPTANCE_CLAUDE_KEY": "controlled-claude-key",
                "SYNARA_ACCEPTANCE_CLAUDE_BASE_URL": "https://claude.example.test",
            },
        ):
            options = acceptance.parse_args(
                [
                    "--suite",
                    "real-provider-smoke",
                    "--target",
                    "docker",
                    "--provider",
                    "claudeAgent",
                    "--runner-command-json",
                    '["node","/opt/synara/provider-host/index.mjs"]',
                    "--real-provider-credential-env",
                    "SYNARA_ACCEPTANCE_CLAUDE_KEY",
                    "--real-provider-base-url-env",
                    "SYNARA_ACCEPTANCE_CLAUDE_BASE_URL",
                ]
            )

        self.assertEqual(options.real_provider_credential_env, "SYNARA_ACCEPTANCE_CLAUDE_KEY")
        self.assertEqual(options.real_provider_credential_field, "apiKey")
        self.assertEqual(
            options.real_provider_base_url_env,
            "SYNARA_ACCEPTANCE_CLAUDE_BASE_URL",
        )

    def test_real_provider_credential_options_reject_unsafe_names_and_fields(self) -> None:
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            acceptance.parse_args(
                [
                    "--suite",
                    "real-provider-smoke",
                    "--runner-command-json",
                    '["node","/tmp/provider-host.mjs"]',
                    "--real-provider-credential-env",
                    "../../secret",
                ]
            )
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            acceptance.parse_args(
                [
                    "--suite",
                    "real-provider-smoke",
                    "--provider",
                    "codex",
                    "--runner-command-json",
                    '["node","/tmp/provider-host.mjs"]',
                    "--real-provider-credential-env",
                    "SYNARA_ACCEPTANCE_CODEX_KEY",
                    "--real-provider-credential-field",
                    "authToken",
                ]
            )

    def test_real_provider_matrix_expands_in_canonical_order(self) -> None:
        options = acceptance.parse_args(
            [
                "--suite",
                "real-provider-smoke",
                "--runner-command-json",
                '["node","/tmp/provider-host.mjs"]',
                "--real-provider-case",
                "user-input",
                "--real-provider-matrix",
            ]
        )

        self.assertEqual(
            acceptance.REAL_PROVIDER_CASES,
            (
                "approval",
                "user-input",
                "steer",
                "interrupt",
                "generated-file-checkpoint",
                "large-diff",
                "terminal-large",
                "review",
                "compact",
                "rollback",
                "fork",
            ),
        )
        self.assertEqual(options.real_provider_cases, acceptance.REAL_PROVIDER_CASES)

    def test_real_provider_failure_matrix_expands_in_canonical_order(self) -> None:
        options = acceptance.parse_args(
            [
                "--suite",
                "real-provider-smoke",
                "--runner-command-json",
                '["node","/tmp/provider-host.mjs"]',
                "--real-provider-failure-case",
                "rate-limit-retry",
                "--real-provider-failure-matrix",
            ]
        )

        self.assertEqual(
            acceptance.REAL_PROVIDER_FAILURE_CASES,
            (
                "authentication",
                "rate-limit-retry",
                "provider-host-crash-retry",
                "cursor-expiry",
            ),
        )
        self.assertEqual(
            options.real_provider_failure_cases,
            acceptance.REAL_PROVIDER_FAILURE_CASES,
        )
        self.assertEqual(options.timeout_seconds, 420.0)

    def test_real_provider_failure_matrix_requires_a_separate_canonical_run(self) -> None:
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            acceptance.parse_args(
                [
                    "--suite",
                    "real-provider-smoke",
                    "--runner-command-json",
                    '["node","/tmp/provider-host.mjs"]',
                    "--real-provider-case",
                    "approval",
                    "--real-provider-failure-matrix",
                ]
            )

    def test_fixture_suite_rejects_real_provider_cases(self) -> None:
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            acceptance.parse_args(["--real-provider-case", "approval"])

    def test_failure_matrix_expands_target_cases_without_duplicates(self) -> None:
        options = acceptance.parse_args(
            [
                "--target",
                "kubernetes",
                "--failure-case",
                "provider-crash",
                "--failure-matrix",
            ]
        )

        self.assertEqual(options.failure_cases, acceptance.TARGET_FAILURE_CASES["kubernetes"])
        self.assertEqual(options.network_outage_seconds, 8.0)

    def test_network_outage_must_cross_acceptance_lease_ttl(self) -> None:
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            acceptance.parse_args(["--network-outage-seconds", "6.5"])

    def test_failure_only_requires_at_least_one_case(self) -> None:
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            acceptance.parse_args(["--failure-only"])

    def test_ssh_options_use_owned_orbstack_defaults_and_release_timeout(self) -> None:
        options = acceptance.parse_args(["--target", "ssh", "--ssh-machine-name", "synara-stage3-test"])

        self.assertEqual(options.timeout_seconds, 900.0)
        self.assertEqual(options.ssh_orbctl_bin, "orbctl")
        self.assertEqual(options.ssh_machine_name, "synara-stage3-test")
        self.assertEqual(options.ssh_machine_arch, "arm64")
        self.assertEqual(options.ssh_machine_image, "ubuntu:24.04")
        self.assertEqual(options.ssh_node_version, "24.13.1")

    def test_ssh_machine_name_rejects_non_dns_input(self) -> None:
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            acceptance.parse_args(["--target", "ssh", "--ssh-machine-name", "../../user-host"])

    def test_ssh_external_host_requires_explicit_authorization_and_repository_external_files(
        self,
    ) -> None:
        with tempfile.TemporaryDirectory() as directory:
            source_root = pathlib.Path(directory)
            identity = source_root / "id_ed25519"
            host_key = source_root / "known-host.pub"
            identity.write_text("private-key-placeholder\n", encoding="utf-8")
            identity.chmod(0o600)
            host_key.write_text("ssh-ed25519 Zml4dHVyZS1ob3N0LWtleQ==\n", encoding="utf-8")

            options = acceptance.parse_args(
                [
                    "--target",
                    "ssh",
                    "--ssh-external-host",
                    "192.0.2.10",
                    "--ssh-external-port",
                    "2222",
                    "--ssh-external-user",
                    "root",
                    "--ssh-external-identity-file",
                    str(identity),
                    "--ssh-external-host-key-file",
                    str(host_key),
                    "--ssh-external-service-user",
                    "root",
                    "--ssh-allow-external-host",
                    "--ssh-machine-arch",
                    "amd64",
                ]
            )

            self.assertEqual(options.ssh_external_host, "192.0.2.10")
            self.assertEqual(options.ssh_external_port, 2222)
            self.assertEqual(options.ssh_external_user, "root")
            self.assertEqual(options.ssh_external_identity_file, identity.resolve())
            self.assertEqual(options.ssh_external_host_key_file, host_key.resolve())
            self.assertTrue(options.ssh_allow_external_host)

            with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                acceptance.parse_args(
                    [
                        "--target",
                        "ssh",
                        "--ssh-external-host",
                        "192.0.2.10",
                        "--ssh-external-user",
                        "root",
                        "--ssh-external-identity-file",
                        str(identity),
                        "--ssh-external-host-key-file",
                        str(host_key),
                    ]
                )

    def test_ssh_external_host_rejects_repository_identity_source(self) -> None:
        identity = REPO_ROOT / ".stage3-test-identity"
        host_key = REPO_ROOT / ".stage3-test-host-key"
        try:
            identity.write_text("private-key-placeholder\n", encoding="utf-8")
            identity.chmod(0o600)
            host_key.write_text("ssh-ed25519 Zml4dHVyZS1ob3N0LWtleQ==\n", encoding="utf-8")
            with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                acceptance.parse_args(
                    [
                        "--target",
                        "ssh",
                        "--ssh-external-host",
                        "192.0.2.10",
                        "--ssh-external-user",
                        "root",
                        "--ssh-external-identity-file",
                        str(identity),
                        "--ssh-external-host-key-file",
                        str(host_key),
                        "--ssh-allow-external-host",
                    ]
                )
        finally:
            identity.unlink(missing_ok=True)
            host_key.unlink(missing_ok=True)

    def test_ssh_rejects_global_skip_build_without_separate_linux_runtime_inputs(self) -> None:
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            acceptance.parse_args(
                [
                    "--target",
                    "ssh",
                    "--skip-build",
                    "--control-plane-binary",
                    "/tmp/synara-control-plane",
                ]
            )

    def test_kubernetes_options_use_release_timeout_and_explicit_kind_context(self) -> None:
        options = acceptance.parse_args(
            [
                "--target",
                "kubernetes",
                "--kubernetes-context",
                "kind-fixture",
                "--kubernetes-kubeconfig",
                "/tmp/kind-fixture-kubeconfig",
                "--kubernetes-worker-image",
                "synara-worker:test",
                "--kubernetes-skip-worker-build",
            ]
        )

        self.assertEqual(options.timeout_seconds, 1200.0)
        self.assertEqual(options.kubernetes_context, "kind-fixture")
        self.assertEqual(
            options.kubernetes_kubeconfig,
            pathlib.Path("/tmp/kind-fixture-kubeconfig").resolve(),
        )
        self.assertEqual(options.kubernetes_worker_image, "synara-worker:test")
        self.assertTrue(options.kubernetes_skip_worker_build)

    def test_kubernetes_existing_image_requires_skip_build(self) -> None:
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            acceptance.parse_args(
                ["--target", "kubernetes", "--kubernetes-worker-image", "operator-owned:image"]
            )

    def test_kubernetes_shared_local_image_store_requires_explicit_reused_context_authorization(
        self,
    ) -> None:
        options = acceptance.parse_args(
            [
                "--target",
                "kubernetes",
                "--kubernetes-context",
                "orbstack",
                "--kubernetes-allow-nondisposable",
                "--kubernetes-shared-local-image-store",
            ]
        )

        self.assertTrue(options.kubernetes_shared_local_image_store)
        invalid_arguments = (
            ["--target", "kubernetes", "--kubernetes-shared-local-image-store"],
            [
                "--target",
                "kubernetes",
                "--kubernetes-context",
                "orbstack",
                "--kubernetes-shared-local-image-store",
            ],
            [
                "--target",
                "kubernetes",
                "--kubernetes-context",
                "orbstack",
                "--kubernetes-allow-nondisposable",
                "--kubernetes-shared-local-image-store",
                "--kubernetes-worker-image",
                "operator-owned:image",
                "--kubernetes-skip-worker-build",
            ],
        )
        for arguments in invalid_arguments:
            with self.subTest(arguments=arguments):
                with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
                    acceptance.parse_args(arguments)


class OutputSecretScanTest(unittest.TestCase):
    def test_scan_fails_closed_without_echoing_secret_material(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            output_dir = pathlib.Path(directory)
            redactor = acceptance.SecretRedactor()
            secret = "stage3-dynamic-secret-value"
            redactor.add(secret)
            (output_dir / "control-plane.log").write_text(
                f"unexpected payload {secret}\n-----BEGIN OPENSSH PRIVATE KEY-----\n",
                encoding="utf-8",
            )

            evidence = acceptance.scan_output_secrets(output_dir, redactor)

            self.assertEqual(evidence["status"], "fail")
            self.assertEqual(
                {finding["kind"] for finding in evidence["findings"]},
                {"known-secret-2", "private-key-pem"},
            )
            self.assertNotIn(secret, str(evidence))

    def test_scan_passes_redacted_reports_and_logs(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            output_dir = pathlib.Path(directory)
            redactor = acceptance.SecretRedactor()
            redactor.add("stage3-dynamic-secret-value")
            (output_dir / "acceptance-report.json").write_text(
                '{"credential":"[REDACTED]"}\n',
                encoding="utf-8",
            )
            (output_dir / "control-plane.log").write_text("safe output\n", encoding="utf-8")

            evidence = acceptance.scan_output_secrets(output_dir, redactor)

            self.assertEqual(evidence["status"], "pass")
            self.assertEqual(evidence["findings"], [])


class SSHDriverTest(unittest.TestCase):
    @staticmethod
    def _key(label: bytes) -> str:
        return "ssh-ed25519 " + base64.b64encode(label).decode("ascii")

    @staticmethod
    def _external_options(
        identity: pathlib.Path,
        host_key: pathlib.Path,
    ) -> acceptance.RunnerOptions:
        return dataclasses.replace(
            runner_options(),
            target="ssh",
            ssh_machine_arch="amd64",
            ssh_external_host="192.0.2.10",
            ssh_external_port=2222,
            ssh_external_user="root",
            ssh_external_identity_file=identity,
            ssh_external_host_key_file=host_key,
            ssh_external_service_user="root",
            ssh_allow_external_host=True,
        )

    def test_external_host_key_source_pins_exact_endpoint_and_rejects_mismatch(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            identity = root / "id_ed25519"
            host_key_source = root / "known-host"
            identity.write_text("private-key-placeholder\n", encoding="utf-8")
            identity.chmod(0o600)
            host_key = self._key(b"trusted-external-host")
            host_key_source.write_text(f"[192.0.2.10]:2222 {host_key}\n", encoding="utf-8")
            redactor = acceptance.SecretRedactor()
            driver = acceptance.SSHDriver(
                pathlib.Path.cwd(),
                self._external_options(identity, host_key_source),
                acceptance.Deadline(30.0),
                redactor,
            )
            self.addCleanup(driver._release_state)

            self.assertEqual(driver._load_external_host_key(), host_key)

            host_key_source.write_text(f"[192.0.2.11]:2222 {host_key}\n", encoding="utf-8")
            with self.assertRaises(acceptance.AcceptanceError) as caught:
                driver._load_external_host_key()
            self.assertEqual(caught.exception.code, "runner.ssh_external_host_key_invalid")

    def test_external_identity_is_loaded_without_copy_and_operator_source_is_preserved(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            identity = root / "id_ed25519"
            host_key_source = root / "host-key"
            subprocess.run(
                ["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(identity)],
                check=True,
            )
            identity.chmod(0o600)
            host_key_source.write_text(self._key(b"trusted-external-host") + "\n", encoding="utf-8")
            redactor = acceptance.SecretRedactor()
            driver = acceptance.SSHDriver(
                pathlib.Path.cwd(),
                self._external_options(identity, host_key_source),
                acceptance.Deadline(30.0),
                redactor,
            )
            self.addCleanup(driver._release_state)

            evidence = driver._load_external_identity()
            self.assertTrue(driver.client_private_key.startswith("-----BEGIN OPENSSH PRIVATE KEY-----"))
            self.assertTrue(driver.client_public_key.startswith("ssh-ed25519 "))
            self.assertNotIn(str(identity), json.dumps(evidence))
            driver._discard_local_private_key()

            self.assertEqual(driver.client_private_key, "")
            self.assertTrue(identity.is_file())
            self.assertTrue(evidence["operatorIdentitySourcePreserved"])
            self.assertTrue(evidence["driverPrivateKeyReferenceClearedAfterProvision"])
            self.assertTrue(
                any(value.startswith("-----BEGIN OPENSSH PRIVATE KEY-----") for value in redactor.secret_values())
            )

    def test_external_preflight_refuses_existing_runtime_before_upload(self) -> None:
        events: list[str] = []

        class Connection:
            def __enter__(self) -> Connection:
                return self

            def __exit__(self, *_args: Any) -> None:
                return None

            def settimeout(self, _timeout: float) -> None:
                return None

            def recv(self, _size: int) -> bytes:
                return b"SSH-2.0-fixture\r\n"

        class ConflictDriver(acceptance.SSHDriver):
            def _load_external_host_key(self) -> str:
                return SSHDriverTest._key(b"trusted-external-host")

            def _remote_command(self, command: Sequence[str], **kwargs: Any) -> str:
                del kwargs
                events.append("remote:" + " ".join(command))
                if command == ["uname", "-m"]:
                    return "x86_64\n"
                raise AssertionError(command)

            def _remote_root_command(self, command: Sequence[str], **kwargs: Any) -> str:
                del kwargs
                events.append("root:" + " ".join(command[:2]))
                raise acceptance.AcceptanceError(
                    "runner.ssh_external_command_failed",
                    "owned runtime path exists",
                )

            def _remote_upload(self, source: pathlib.Path, destination: str, mode: str) -> None:
                del source, destination, mode
                events.append("unexpected-upload")

        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            identity = root / "id_ed25519"
            host_key_source = root / "host-key"
            identity.write_text("private-key-placeholder\n", encoding="utf-8")
            identity.chmod(0o600)
            host_key_source.write_text(self._key(b"trusted-external-host") + "\n", encoding="utf-8")
            driver = ConflictDriver(
                pathlib.Path.cwd(),
                self._external_options(identity, host_key_source),
                acceptance.Deadline(30.0),
                acceptance.SecretRedactor(),
            )
            self.addCleanup(driver._release_state)
            with mock.patch.object(acceptance.socket, "create_connection", return_value=Connection()):
                with self.assertRaises(acceptance.AcceptanceError) as caught:
                    driver._prepare_external_host()

        self.assertEqual(caught.exception.code, "runner.ssh_external_preflight_failed")
        self.assertFalse(driver.external_runtime_created)
        self.assertNotIn("unexpected-upload", events)

    def test_provision_uses_one_time_key_pinned_host_key_and_product_install(self) -> None:
        target_ids = [
            "11111111-1111-4111-8111-111111111111",
            "22222222-2222-4222-8222-222222222222",
        ]

        class ProvisionAPI:
            def __init__(self) -> None:
                self.requests: list[tuple[str, str, Mapping[str, Any] | None]] = []
                self.created: list[Mapping[str, Any]] = []

            def request(
                inner_self,
                method: str,
                path: str,
                payload: Mapping[str, Any] | None = None,
                expected: Sequence[int] = (200,),
                *,
                maximum_timeout: float = 10.0,
            ) -> Any:
                del expected
                inner_self.requests.append((method, path, payload))
                if path.endswith("/ssh/install"):
                    self.assertEqual(maximum_timeout, acceptance.SSH_CONTROL_PLANE_OPERATION_TIMEOUT)
                if method == "POST" and path.endswith("/execution-targets"):
                    assert payload is not None
                    inner_self.created.append(payload)
                    target_id = target_ids[len(inner_self.created) - 1]
                    return {
                        "id": target_id,
                        "organizationId": payload["organizationId"],
                        "kind": payload["kind"],
                        "name": payload["name"],
                        "status": "active",
                    }
                if path.endswith(f"/{target_ids[0]}/ssh/install"):
                    raise acceptance.AcceptanceError(
                        "ssh_connection_failed",
                        "The SSH execution target could not be reached.",
                    )
                if path.endswith(f"/{target_ids[1]}/ssh/install"):
                    return {
                        "targetId": target_ids[1],
                        "operation": "install",
                        "status": "active",
                        "serviceName": f"synara-agentd-{target_ids[1]}.service",
                        "binarySha256": "a" * 64,
                    }
                raise AssertionError(f"unexpected request: {method} {path}")

        class ProvisionDriver(acceptance.SSHDriver):
            def _worker_proxy_url(self) -> str:
                return "http://127.0.0.1:41234"

            def _worker_proxy_relay_evidence(self) -> Mapping[str, Any]:
                return {
                    "mode": "reverse-ssh-loopback",
                    "vmListenHost": "127.0.0.1",
                    "vmListenPort": 41234,
                    "upstreamAddress": "127.0.0.1:49999",
                    "readsUserSSHConfiguration": False,
                    "log": "/tmp/relay.log",
                }

            def _assert_remote_target_absent(
                self,
                target_id: str,
                *,
                cleanup_timeout: float | None = None,
            ) -> None:
                del cleanup_timeout
                self.absent_targets = [*getattr(self, "absent_targets", []), target_id]

            def _require_service_active(self, service_name: str) -> dict[str, Any]:
                return {
                    "serviceName": service_name,
                    "activeState": "active",
                    "subState": "running",
                    "unitFileState": "enabled",
                    "mainPid": 42,
                    "restartCount": 0,
                }

        redactor = acceptance.SecretRedactor()
        options = dataclasses.replace(runner_options(), target="ssh")
        driver = ProvisionDriver(pathlib.Path.cwd(), options, acceptance.Deadline(30.0), redactor)
        self.addCleanup(driver._release_state)
        api = ProvisionAPI()
        driver.api = api  # type: ignore[assignment]
        driver.machine_ip = "192.0.2.10"
        driver.host_key = self._key(b"trusted-host-key")
        driver.client_public_key = self._key(b"wrong-host-key")
        driver.client_private_key = "-----BEGIN OPENSSH PRIVATE KEY-----\none-time-private-secret\n-----END OPENSSH PRIVATE KEY-----\n"
        redactor.add(driver.client_private_key, "[REDACTED_SSH_PRIVATE_KEY]")

        target = driver.provision_target("tenant-id", "organization-id", "codex")

        self.assertEqual(target["id"], target_ids[1])
        self.assertEqual(driver.absent_targets, target_ids)
        self.assertEqual(driver.client_private_key, "")
        self.assertEqual(len(api.created), 2)
        negative_configuration = api.created[0]["configuration"]
        configuration = api.created[1]["configuration"]
        self.assertEqual(negative_configuration["hostKey"], driver.client_public_key)
        self.assertEqual(configuration["hostKey"], driver.host_key)
        self.assertEqual(configuration["privateKey"], "-----BEGIN OPENSSH PRIVATE KEY-----\none-time-private-secret\n-----END OPENSSH PRIVATE KEY-----\n")
        self.assertEqual(configuration["controlPlaneUrl"], "http://127.0.0.1:41234")
        self.assertEqual(configuration["serviceUser"], acceptance.SSH_SERVICE_USER)
        self.assertFalse(configuration["useSudo"])
        self.assertEqual(
            target["driverEvidence"]["controlPlaneCredentialLifecycle"],
            acceptance.SSH_CREDENTIAL_LIFECYCLE,
        )
        self.assertNotIn("one-time-private-secret", str(redactor.value(target)))

    def test_external_target_configuration_uses_unique_scoped_paths_without_source_metadata(
        self,
    ) -> None:
        class TargetAPI:
            def __init__(self) -> None:
                self.payload: Mapping[str, Any] | None = None

            def request(
                inner_self,
                method: str,
                path: str,
                payload: Mapping[str, Any] | None = None,
                expected: Sequence[int] = (200,),
                *,
                maximum_timeout: float = 10.0,
            ) -> Any:
                del method, path, expected, maximum_timeout
                inner_self.payload = payload
                return {"id": "target-id"}

        class TargetDriver(acceptance.SSHDriver):
            def _worker_proxy_url(self) -> str:
                return "http://127.0.0.1:41234"

        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            identity = root / "id_ed25519"
            host_key_source = root / "host-key"
            identity.write_text("private-key-placeholder\n", encoding="utf-8")
            identity.chmod(0o600)
            host_key_source.write_text(self._key(b"trusted-external-host") + "\n", encoding="utf-8")
            driver = TargetDriver(
                pathlib.Path.cwd(),
                self._external_options(identity, host_key_source),
                acceptance.Deadline(30.0),
                acceptance.SecretRedactor(),
            )
            self.addCleanup(driver._release_state)
            api = TargetAPI()
            driver.api = api  # type: ignore[assignment]
            driver.client_private_key = "private-key-secret"
            driver.machine_ip = "192.0.2.10"

            driver._create_ssh_target(
                "tenant-id",
                "organization-id",
                "external-target",
                self._key(b"trusted-external-host"),
                "codex",
            )

        assert api.payload is not None
        configuration = api.payload["configuration"]
        self.assertEqual(configuration["port"], 2222)
        self.assertEqual(configuration["user"], "root")
        self.assertEqual(configuration["serviceUser"], "root")
        self.assertEqual(configuration["runnerCommand"], [driver.remote_node_path, driver.remote_fixture_path, "--protocol-v2"])
        self.assertEqual(configuration["installRoot"], driver.external_install_root)
        self.assertEqual(configuration["workspaceRoot"], driver.external_workspace_root)
        self.assertEqual(configuration["gitCacheRoot"], driver.external_git_cache_root)
        encoded = json.dumps(api.payload)
        self.assertNotIn(str(identity), encoded)
        self.assertNotIn(str(host_key_source), encoded)

    def test_external_replacement_never_restarts_sshd_or_host(self) -> None:
        events: list[str] = []

        class ReplacementAPI:
            def request(
                self,
                method: str,
                path: str,
                payload: Mapping[str, Any] | None = None,
                expected: Sequence[int] = (200,),
                *,
                maximum_timeout: float = 10.0,
            ) -> Any:
                del payload, expected, maximum_timeout
                events.append(f"api:{method}:{path}")
                if path.endswith("/provider-policy"):
                    return {"status": "active"}
                if path.endswith("/ssh/upgrade"):
                    return {
                        "targetId": "target-id",
                        "operation": "upgrade",
                        "status": "active",
                        "serviceName": "synara-agentd-target-id.service",
                    }
                raise AssertionError(path)

            def wait_until(
                self,
                description: str,
                probe: Callable[[], Any],
                interval: float = 0.25,
            ) -> Any:
                del description, interval
                return probe()

        class ReplacementDriver(acceptance.SSHDriver):
            def __init__(self, *args: Any, **kwargs: Any) -> None:
                super().__init__(*args, **kwargs)
                self.worker_calls = 0
                self.service_calls = 0

            def _worker_identity(self, target_id: str, *, required: bool = True) -> dict[str, Any] | None:
                del target_id, required
                self.worker_calls += 1
                if self.worker_calls == 1:
                    return {"id": "worker-id", "incarnation": 1, "instanceUid": "old", "status": "online"}
                return {"id": "worker-id", "incarnation": 2, "instanceUid": "new", "status": "online"}

            def _require_service_active(self, service_name: str) -> dict[str, Any]:
                self.service_calls += 1
                return {
                    "serviceName": service_name,
                    "activeState": "active",
                    "subState": "running",
                    "unitFileState": "enabled",
                    "mainPid": 100 if self.service_calls == 1 else 200,
                    "restartCount": 0,
                }

            def _remote_command(self, command: Sequence[str], **kwargs: Any) -> str:
                del command, kwargs
                raise AssertionError("external replacement must not restart sshd")

        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            identity = root / "id_ed25519"
            host_key_source = root / "host-key"
            identity.write_text("private-key-placeholder\n", encoding="utf-8")
            identity.chmod(0o600)
            host_key_source.write_text(self._key(b"trusted-external-host") + "\n", encoding="utf-8")
            driver = ReplacementDriver(
                pathlib.Path.cwd(),
                self._external_options(identity, host_key_source),
                acceptance.Deadline(30.0),
                acceptance.SecretRedactor(),
            )
            self.addCleanup(driver._release_state)
            driver.api = ReplacementAPI()  # type: ignore[assignment]
            driver.service_name = "synara-agentd-target-id.service"
            driver.host_key = self._key(b"trusted-external-host")

            evidence = driver.replace_worker("tenant-id", "target-id", "codex")

        self.assertFalse(evidence["sshdRestarted"])
        self.assertFalse(evidence["externalHostRestarted"])
        self.assertTrue(evidence["instanceUidChanged"])
        self.assertTrue(any(event.endswith("/ssh/upgrade") for event in events))

    def test_replace_restarts_sshd_and_systemd_then_waits_for_new_incarnation(self) -> None:
        events: list[str] = []

        class ReplacementAPI:
            def request(
                self,
                method: str,
                path: str,
                payload: Mapping[str, Any] | None = None,
                expected: Sequence[int] = (200,),
                *,
                maximum_timeout: float = 10.0,
            ) -> Any:
                del payload, expected
                if path.endswith("/ssh/upgrade"):
                    events.append(f"timeout:{maximum_timeout}")
                events.append(f"api:{method}:{path}")
                if path.endswith("/provider-policy"):
                    return {"status": "active"}
                if path.endswith("/ssh/upgrade"):
                    return {
                        "targetId": "target-id",
                        "operation": "upgrade",
                        "status": "active",
                        "serviceName": "synara-agentd-target-id.service",
                    }
                raise AssertionError(path)

            def wait_until(
                self,
                description: str,
                probe: Callable[[], Any],
                interval: float = 0.25,
            ) -> Any:
                del description, interval
                value = probe()
                if value is None:
                    raise AssertionError("replacement probe did not complete")
                return value

        class ReplacementDriver(acceptance.SSHDriver):
            def __init__(self, *args: Any, **kwargs: Any) -> None:
                super().__init__(*args, **kwargs)
                self.worker_calls = 0
                self.service_calls = 0

            def _worker_identity(self, target_id: str, *, required: bool = True) -> dict[str, Any] | None:
                del target_id, required
                self.worker_calls += 1
                if self.worker_calls == 1:
                    return {"id": "worker-id", "incarnation": 1, "instanceUid": "old", "status": "online", "podName": "ssh"}
                return {"id": "worker-id", "incarnation": 2, "instanceUid": "new", "status": "online", "podName": "ssh"}

            def _require_service_active(self, service_name: str) -> dict[str, Any]:
                self.service_calls += 1
                return {
                    "serviceName": service_name,
                    "activeState": "active",
                    "subState": "running",
                    "unitFileState": "enabled",
                    "mainPid": 100 if self.service_calls == 1 else 200,
                    "restartCount": 0,
                }

            def _remote_command(self, command: Sequence[str], **kwargs: Any) -> str:
                del kwargs
                events.append("remote:" + " ".join(command))
                return "active\n" if command[:2] == ["systemctl", "is-active"] else ""

        options = dataclasses.replace(runner_options(), target="ssh")
        driver = ReplacementDriver(
            pathlib.Path.cwd(), options, acceptance.Deadline(30.0), acceptance.SecretRedactor()
        )
        self.addCleanup(driver._release_state)
        driver.api = ReplacementAPI()  # type: ignore[assignment]
        driver.service_name = "synara-agentd-target-id.service"
        driver.host_key = self._key(b"trusted-host-key")

        evidence = driver.replace_worker("tenant-id", "target-id", "codex")

        self.assertTrue(evidence["sshdRestarted"])
        self.assertTrue(evidence["workerIdStable"])
        self.assertTrue(evidence["instanceUidChanged"])
        self.assertEqual(evidence["previousMainPid"], 100)
        self.assertEqual(evidence["replacementMainPid"], 200)
        self.assertIn("remote:systemctl restart ssh", events)
        self.assertTrue(any(event.endswith("/ssh/upgrade") for event in events))
        self.assertIn(f"timeout:{acceptance.SSH_CONTROL_PLANE_OPERATION_TIMEOUT}", events)

    def test_cleanup_revokes_before_stopping_and_deletes_only_owned_machine(self) -> None:
        events: list[str] = []

        class CleanupAPI:
            def request(
                self,
                method: str,
                path: str,
                payload: Mapping[str, Any] | None = None,
                expected: Sequence[int] = (200,),
                *,
                maximum_timeout: float = 10.0,
            ) -> Any:
                del payload, expected
                if path.endswith("/ssh/revoke"):
                    events.append(f"timeout:{maximum_timeout}")
                events.append(f"api:{method}:{path}")
                return {"operation": "revoke", "status": "disabled"}

        class RunningProcess:
            @staticmethod
            def poll() -> None:
                return None

        class CleanupDriver(acceptance.SSHDriver):
            def _remote_command(self, command: Sequence[str], **kwargs: Any) -> str:
                del kwargs
                events.append("remote:" + " ".join(command))
                return ""

            def _assert_remote_target_absent(self, target_id: str, **kwargs: Any) -> None:
                del kwargs
                events.append(f"absent:{target_id}")

            def _orbctl_completed(self, arguments: Sequence[str], **kwargs: Any) -> subprocess.CompletedProcess[str]:
                del kwargs
                events.append("orbctl:" + " ".join(arguments))
                return subprocess.CompletedProcess(list(arguments), 0, "deleted")

            def stop(self) -> None:
                events.append("stop-control-plane")
                self.process = None

            def _stop_worker_proxy_relay(self) -> None:
                events.append("stop-worker-proxy-relay")

            def _stop_worker_proxy(self) -> None:
                events.append("stop-worker-proxy")

            def _release_state(self) -> None:
                events.append("release-state")

        options = dataclasses.replace(
            runner_options(), target="ssh", ssh_machine_name="synara-stage3-owned"
        )
        driver = CleanupDriver(
            pathlib.Path.cwd(), options, acceptance.Deadline(30.0), acceptance.SecretRedactor()
        )
        driver.api = CleanupAPI()  # type: ignore[assignment]
        driver.process = RunningProcess()  # type: ignore[assignment]
        driver.machine_create_attempted = True
        driver.machine_created = True
        driver.tenant_id = "tenant-id"
        driver.target_id = "target-id"
        driver.service_name = "synara-agentd-target-id.service"

        driver.cleanup()

        revoke_index = next(index for index, event in enumerate(events) if event.endswith("/ssh/revoke"))
        self.assertIn(f"timeout:{acceptance.SSH_CONTROL_PLANE_OPERATION_TIMEOUT}", events)
        relay_stop_index = events.index("stop-worker-proxy-relay")
        stop_index = events.index("stop-control-plane")
        delete = next(event for event in events if event.startswith("orbctl:delete"))
        self.assertLess(revoke_index, stop_index)
        self.assertLess(relay_stop_index, stop_index)
        self.assertEqual(delete, "orbctl:delete --force synara-stage3-owned")
        self.assertNotIn("--all", delete)

    def test_external_cleanup_revokes_and_removes_only_owned_runtime_while_preserving_host_identity(
        self,
    ) -> None:
        events: list[str] = []

        class CleanupAPI:
            def request(
                self,
                method: str,
                path: str,
                payload: Mapping[str, Any] | None = None,
                expected: Sequence[int] = (200,),
                *,
                maximum_timeout: float = 10.0,
            ) -> Any:
                del payload, expected, maximum_timeout
                events.append(f"api:{method}:{path}")
                return {"operation": "revoke", "status": "disabled"}

        class RunningProcess:
            @staticmethod
            def poll() -> None:
                return None

        class CleanupDriver(acceptance.SSHDriver):
            def _remote_root_command(self, command: Sequence[str], **kwargs: Any) -> str:
                del kwargs
                events.append("root:" + " ".join(command[:2]))
                return ""

            def _assert_remote_target_absent(self, target_id: str, **kwargs: Any) -> None:
                del kwargs
                events.append(f"absent:{target_id}")

            def _remove_external_runtime(self) -> None:
                events.append("remove-owned-runtime")
                self.external_runtime_created = False
                self.machine_created = False

            def _orbctl_completed(self, arguments: Sequence[str], **kwargs: Any) -> subprocess.CompletedProcess[str]:
                del arguments, kwargs
                raise AssertionError("external cleanup must not invoke OrbStack")

            def stop(self) -> None:
                events.append("stop-control-plane")
                self.process = None

            def _stop_worker_proxy_relay(self) -> None:
                events.append("stop-worker-proxy-relay")

            def _stop_worker_proxy(self) -> None:
                events.append("stop-worker-proxy")

            def _release_state(self) -> None:
                events.append("release-state")

        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            identity = root / "id_ed25519"
            host_key_source = root / "host-key"
            identity.write_text("private-key-placeholder\n", encoding="utf-8")
            identity.chmod(0o600)
            host_key_source.write_text(self._key(b"trusted-external-host") + "\n", encoding="utf-8")
            driver = CleanupDriver(
                pathlib.Path.cwd(),
                self._external_options(identity, host_key_source),
                acceptance.Deadline(30.0),
                acceptance.SecretRedactor(),
            )
            driver.api = CleanupAPI()  # type: ignore[assignment]
            driver.process = RunningProcess()  # type: ignore[assignment]
            driver.machine_created = True
            driver.external_runtime_created = True
            driver.tenant_id = "tenant-id"
            driver.target_id = "target-id"
            driver.service_name = "synara-agentd-target-id.service"

            evidence = driver.cleanup()

            self.assertTrue(identity.is_file())

        revoke_index = next(index for index, event in enumerate(events) if event.endswith("/ssh/revoke"))
        verify_index = events.index("absent:target-id")
        runtime_index = events.index("remove-owned-runtime")
        self.assertLess(revoke_index, verify_index)
        self.assertLess(verify_index, runtime_index)
        self.assertFalse(any("authorized_keys" in event for event in events))
        self.assertTrue(evidence["externalHostPreserved"])
        self.assertFalse(evidence["externalHostRestarted"])
        self.assertTrue(evidence["ownedRuntimeRemoved"])
        self.assertTrue(evidence["operatorIdentitySourcePreserved"])

    def test_external_runtime_cleanup_requires_exact_ownership_marker(self) -> None:
        scripts: list[str] = []

        class OwnershipDriver(acceptance.SSHDriver):
            def _remote_root_command(self, command: Sequence[str], **kwargs: Any) -> str:
                del kwargs
                scripts.append(command[-1])
                return ""

        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            identity = root / "id_ed25519"
            host_key_source = root / "host-key"
            identity.write_text("private-key-placeholder\n", encoding="utf-8")
            identity.chmod(0o600)
            host_key_source.write_text(self._key(b"trusted-external-host") + "\n", encoding="utf-8")
            driver = OwnershipDriver(
                pathlib.Path.cwd(),
                self._external_options(identity, host_key_source),
                acceptance.Deadline(30.0),
                acceptance.SecretRedactor(),
            )
            self.addCleanup(driver._release_state)
            driver.external_runtime_created = True
            driver.machine_created = True

            driver._remove_external_runtime()

        self.assertEqual(len(scripts), 1)
        script = scripts[0]
        self.assertIn(driver.external_runtime_root + "/.synara-owner", script)
        self.assertIn(driver.installation_id, script)
        self.assertIn("rm -rf -- " + driver.external_runtime_root, script)
        self.assertIn("rm -rf -- " + driver.external_stage_root, script)
        self.assertNotIn("rm -rf -- /opt/synara\n", script)
        self.assertFalse(driver.external_runtime_created)

    def test_cleanup_reports_actual_isolated_state_removal(self) -> None:
        driver = acceptance.SSHDriver(
            pathlib.Path.cwd(),
            dataclasses.replace(runner_options(), target="ssh"),
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        state_dir = driver.state_dir

        evidence = driver.cleanup()

        self.assertTrue(evidence["stateRemoved"])
        self.assertFalse(state_dir.exists())
        self.assertTrue(evidence["machineRemoved"])
        self.assertTrue(evidence["localKeyMaterialRemoved"])

    def test_failed_machine_create_still_triggers_exact_cleanup_without_remote_mutation(self) -> None:
        events: list[str] = []

        class CreateFailureDriver(acceptance.SSHDriver):
            def _orbctl_command(self, arguments: Sequence[str], **kwargs: Any) -> str:
                del kwargs
                events.append("command:" + " ".join(arguments))
                if arguments[:2] == ["list", "--format"]:
                    return "[]"
                if arguments and arguments[0] == "create":
                    raise acceptance.AcceptanceError("runner.orbstack_command_failed", "create failed")
                raise AssertionError(arguments)

            def _remote_command(self, command: Sequence[str], **kwargs: Any) -> str:
                del kwargs
                events.append("unexpected-remote:" + " ".join(command))
                return ""

        options = dataclasses.replace(
            runner_options(), target="ssh", ssh_machine_name="synara-stage3-create-failed"
        )
        driver = CreateFailureDriver(
            pathlib.Path.cwd(), options, acceptance.Deadline(30.0), acceptance.SecretRedactor()
        )

        with self.assertRaises(acceptance.AcceptanceError):
            driver._prepare_machine()

        self.assertTrue(driver.machine_create_attempted)
        self.assertFalse(driver.machine_created)
        create = next(event for event in events if event.startswith("command:create"))
        self.assertIn("--arch arm64", create)
        self.assertIn(f"--user {acceptance.SSH_SERVICE_USER}", create)
        self.assertIn("--isolated ubuntu:24.04 synara-stage3-create-failed", create)
        driver.cleanup()
        self.assertFalse(any(event.startswith("cleanup:delete") for event in events))
        self.assertFalse(any(event.startswith("unexpected-remote:") for event in events))

    def test_worker_proxy_relay_uses_runner_owned_ssh_args_and_stops_cleanly(self) -> None:
        events: list[str] = []

        class FakeThread:
            @staticmethod
            def is_alive() -> bool:
                return True

        class FakeWorkerProxy:
            port = 43123
            thread = FakeThread()

        class RelayProcess:
            def __init__(self) -> None:
                self.pid = 4321
                self.returncode: int | None = None

            def poll(self) -> int | None:
                return self.returncode

            def wait(self, timeout: float | None = None) -> int:
                del timeout
                self.returncode = 0
                events.append("relay-wait")
                return 0

        class RelayDriver(acceptance.SSHDriver):
            def _spawn_worker_proxy_relay(
                self,
                command: Sequence[str],
                log_handle: Any,
            ) -> RelayProcess:
                del log_handle
                events.append("spawn:" + " ".join(command))
                return RelayProcess()

        options = dataclasses.replace(runner_options(), target="ssh")
        driver = RelayDriver(pathlib.Path.cwd(), options, acceptance.Deadline(30.0), acceptance.SecretRedactor())
        self.addCleanup(driver._release_state)
        driver.credentials_dir.mkdir(parents=True, exist_ok=True)
        driver.client_key_path.write_text("private-key", encoding="utf-8")
        driver.machine_created = True
        driver.machine_ip = "192.0.2.10"
        driver.host_key = self._key(b"trusted-host-key")
        driver.worker_proxy = FakeWorkerProxy()  # type: ignore[assignment]

        with mock.patch.object(acceptance, "reserve_loopback_port", return_value=41234), mock.patch.object(
            acceptance.os,
            "killpg",
            side_effect=lambda pid, sig: events.append(f"killpg:{pid}:{sig}"),
        ):
            driver._start_worker_proxy_relay()
            evidence = driver._worker_proxy_relay_evidence()
            known_hosts = driver.known_hosts_path.read_text(encoding="utf-8")
            driver._stop_worker_proxy_relay()

        spawn = next(event for event in events if event.startswith("spawn:"))
        self.assertIn("ssh -F /dev/null", spawn)
        self.assertIn("IdentityAgent=none", spawn)
        self.assertIn("StrictHostKeyChecking=yes", spawn)
        self.assertIn("GlobalKnownHostsFile=/dev/null", spawn)
        self.assertIn(f"UserKnownHostsFile={driver.known_hosts_path}", spawn)
        self.assertIn("-R 127.0.0.1:41234:127.0.0.1:43123", spawn)
        self.assertIn("root@192.0.2.10", spawn)
        self.assertEqual(known_hosts, f"192.0.2.10 {driver.host_key}\n")
        self.assertEqual(evidence["mode"], "reverse-ssh-loopback")
        self.assertEqual(evidence["vmListenPort"], 41234)
        self.assertTrue(any(event.startswith("killpg:4321:") for event in events))
        self.assertIn("relay-wait", events)
        self.assertFalse(driver.known_hosts_path.exists())

    def test_provider_fault_server_uses_owned_reverse_relay_and_request_proof(self) -> None:
        class RelayProcess:
            pid = 4321
            returncode: int | None = None

            @staticmethod
            def poll() -> None:
                return None

        driver = acceptance.SSHDriver(
            pathlib.Path.cwd(),
            dataclasses.replace(runner_options(), target="ssh"),
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        self.addCleanup(driver._release_state)
        worker_proxy = acceptance._WorkerOnlyProxy(9)
        worker_proxy.start()
        self.addCleanup(worker_proxy.stop)
        driver.worker_proxy = worker_proxy
        driver.worker_proxy_relay_process = RelayProcess()  # type: ignore[assignment]
        driver.worker_proxy_relay_port = 41235
        server = driver.create_provider_fault_server("claudeAgent", "authentication")
        server.start()
        try:
            evidence = driver.probe_provider_fault_server(server)
            with self.assertRaises(acceptance.AcceptanceError) as duplicate:
                worker_proxy.register_provider_fault_route(server.route_prefix, server.port)
            request = urllib.request.Request(
                f"http://127.0.0.1:{worker_proxy.port}{server.route_prefix}/v1/messages",
                data=b'{}',
                headers={"Content-Type": "application/json", "X-Api-Key": "fault-secret"},
                method="POST",
            )
            with self.assertRaises(urllib.error.HTTPError) as raised:
                urllib.request.urlopen(request, timeout=2.0)
            raised.exception.close()
            fault_evidence = server.evidence()
        finally:
            server.stop()

        self.assertTrue(server.endpoint.startswith("http://127.0.0.1:41235/"))
        self.assertEqual(duplicate.exception.code, "runner.provider_fault_route_duplicate")
        self.assertEqual(raised.exception.code, 401)
        self.assertEqual(fault_evidence["advertisedPort"], 41235)
        self.assertEqual(fault_evidence["requestCount"], 1)
        self.assertEqual(fault_evidence["paths"], ["/v1/messages"])
        self.assertEqual(fault_evidence["credentialHeaderNames"], ["x-api-key"])
        self.assertEqual(evidence["transport"], "reverse-ssh-loopback")
        self.assertEqual(evidence["validationMode"], "controlled-provider-request")
        self.assertFalse(evidence["probedFromWorker"])
        self.assertFalse(evidence["endpointPersisted"])
        self.assertNotIn(server.route_token, json.dumps(evidence))
        self.assertIsNone(worker_proxy.provider_fault_upstream_port(server.route_prefix))

        completed = acceptance.finalize_provider_fault_reachability(
            evidence,
            {"requestCount": 1},
        )
        self.assertTrue(completed["probedFromWorker"])

    def test_host_crash_kills_one_protocol_v2_descendant_of_systemd_agentd(self) -> None:
        driver = acceptance.SSHDriver(
            pathlib.Path.cwd(),
            dataclasses.replace(runner_options(), target="ssh"),
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        self.addCleanup(driver._release_state)
        driver.machine_created = True
        driver.machine_name = "synara-stage3-owned"
        driver.service_name = "synara-agentd-target-id.service"
        driver._require_service_active = mock.Mock(  # type: ignore[method-assign]
            return_value={"mainPid": 321}
        )
        driver._remote_command = mock.Mock(  # type: ignore[method-assign]
            return_value=json.dumps(
                {
                    "rootPid": 321,
                    "candidateCount": 1,
                    "descendantCount": 4,
                    "providerHostPid": 654,
                    "killed": True,
                }
            )
        )

        evidence = driver.crash_provider_host()

        command = driver._remote_command.call_args.args[0]
        self.assertEqual(command[:2], ["node", "-e"])
        self.assertNotIn("--protocol-v2", command[2])
        self.assertEqual(command[-1], "321")
        self.assertEqual(evidence["providerHostPid"], 654)
        self.assertTrue(evidence["scopedToDisposableMachine"])
        self.assertTrue(evidence["scopedToSystemdService"])
        self.assertTrue(evidence["scopedToAgentdDescendants"])
        self.assertFalse(evidence["broadProcessMatchUsed"])

    def test_host_crash_fails_closed_on_ambiguous_systemd_descendants(self) -> None:
        driver = acceptance.SSHDriver(
            pathlib.Path.cwd(),
            dataclasses.replace(runner_options(), target="ssh"),
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        self.addCleanup(driver._release_state)
        driver.machine_created = True
        driver.service_name = "synara-agentd-target-id.service"
        driver._require_service_active = mock.Mock(  # type: ignore[method-assign]
            return_value={"mainPid": 321}
        )
        driver._remote_command = mock.Mock(  # type: ignore[method-assign]
            return_value=json.dumps(
                {"rootPid": 321, "candidateCount": 2, "descendantCount": 5}
            )
        )

        with self.assertRaises(acceptance.AcceptanceError) as caught:
            driver.crash_provider_host()

        self.assertEqual(caught.exception.code, "runner.provider_host_process_ambiguous")
        self.assertEqual(caught.exception.evidence["candidateCount"], 2)

    def test_generated_key_evidence_describes_local_deletion_and_encrypted_lifecycle(self) -> None:
        class KeyEvidenceDriver(acceptance.SSHDriver):
            def _local_command(
                self,
                arguments: Sequence[str],
                *,
                cwd: pathlib.Path,
                environment: Mapping[str, str],
                log_path: pathlib.Path,
                maximum_timeout: float,
                error_code: str,
                description: str,
            ) -> None:
                del arguments, cwd, environment, log_path, maximum_timeout, error_code, description
                self.credentials_dir.mkdir(parents=True, exist_ok=True)
                self.client_key_path.write_text(
                    "-----BEGIN OPENSSH PRIVATE KEY-----\nfixture-private-key\n-----END OPENSSH PRIVATE KEY-----\n",
                    encoding="utf-8",
                )
                self.client_public_key_path.write_text(SSHDriverTest._key(b"generated-host-key"), encoding="utf-8")

        driver = KeyEvidenceDriver(
            pathlib.Path.cwd(),
            dataclasses.replace(runner_options(), target="ssh"),
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        self.addCleanup(driver._discard_local_key_material)
        self.addCleanup(driver._release_state)

        evidence = driver._generate_client_key()

        self.assertTrue(evidence["localPrivateKeyPlaintextDeletedAfterProvision"])
        self.assertEqual(evidence["controlPlaneCredentialLifecycle"], acceptance.SSH_CREDENTIAL_LIFECYCLE)
        self.assertNotIn("privateKeyPersistedAfterProvision", evidence)

    def test_machine_setup_script_creates_run_sshd_before_validation_and_restart(self) -> None:
        driver = acceptance.SSHDriver(
            pathlib.Path.cwd(),
            dataclasses.replace(runner_options(), target="ssh"),
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        self.addCleanup(driver._release_state)

        script_lines = driver._machine_setup_script().splitlines()

        run_sshd_index = script_lines.index("install -d -o root -g root -m 0755 /run/sshd")
        validate_index = script_lines.index("sshd -t")
        restart_index = script_lines.index("systemctl restart ssh")
        self.assertLess(run_sshd_index, validate_index)
        self.assertLess(validate_index, restart_index)

    def test_real_provider_setup_installs_locked_host_and_provider_tools(self) -> None:
        driver = acceptance.SSHDriver(
            pathlib.Path.cwd(),
            dataclasses.replace(
                runner_options(),
                target="ssh",
                suite="real-provider-smoke",
                runner_command=(acceptance.SSH_PROVIDER_HOST_COMMAND_PATH,),
            ),
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        self.addCleanup(driver._release_state)

        script = driver._machine_setup_script()

        self.assertIn(acceptance.SSH_REMOTE_PROVIDER_HOST_PATH, script)
        self.assertIn(acceptance.SSH_REMOTE_PROVIDER_TOOLS_ROOT, script)
        self.assertIn("npm ci --omit=dev --ignore-scripts --no-audit --no-fund", script)
        self.assertIn("node node_modules/@anthropic-ai/claude-code/install.cjs", script)
        self.assertIn(acceptance.SSH_PROVIDER_HOST_COMMAND_PATH, script)
        self.assertIn("node_modules/.bin/codex", script)
        self.assertIn("node_modules/.bin/claude", script)
        self.assertNotIn(acceptance.SSH_REMOTE_FIXTURE_PATH, script)

    def test_real_provider_artifacts_build_host_instead_of_fixture(self) -> None:
        calls: list[list[str]] = []

        class BuildDriver(acceptance.SSHDriver):
            def _local_command(
                self,
                arguments: Sequence[str],
                *,
                cwd: pathlib.Path,
                environment: Mapping[str, str],
                log_path: pathlib.Path,
                maximum_timeout: float,
                error_code: str,
                description: str,
            ) -> None:
                del cwd, environment, log_path, maximum_timeout, error_code, description
                calls.append(list(arguments))
                if arguments[0] == "go":
                    output_path = pathlib.Path(arguments[arguments.index("-o") + 1])
                else:
                    output_path = pathlib.Path(arguments[arguments.index("--outfile") + 1])
                output_path.parent.mkdir(parents=True, exist_ok=True)
                output_path.write_bytes(b"runtime-binary")

        driver = BuildDriver(
            REPO_ROOT,
            dataclasses.replace(
                runner_options(),
                target="ssh",
                suite="real-provider-smoke",
                runner_command=(acceptance.SSH_PROVIDER_HOST_COMMAND_PATH,),
            ),
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        self.addCleanup(driver._release_state)

        evidence = driver._prepare_ssh_artifacts()

        bun_command = next(command for command in calls if command[0] == "bun")
        self.assertIn(str(REPO_ROOT / "apps/provider-host/src/index.ts"), bun_command)
        self.assertNotIn("provider-host-fixture.ts", " ".join(bun_command))
        self.assertIn("providerHost", evidence)
        self.assertNotIn("providerHostFixture", evidence)
        self.assertEqual(evidence["providerHost"]["runtime"], "real-provider")
        self.assertIn("providerTools", evidence)

    def test_real_provider_runtime_verifies_locked_versions_and_bundle_digest(self) -> None:
        driver = acceptance.SSHDriver(
            REPO_ROOT,
            dataclasses.replace(
                runner_options(),
                target="ssh",
                suite="real-provider-smoke",
                runner_command=(acceptance.SSH_PROVIDER_HOST_COMMAND_PATH,),
            ),
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        self.addCleanup(driver._release_state)
        driver.provider_host_bundle_path.parent.mkdir(parents=True, exist_ok=True)
        driver.provider_host_bundle_path.write_bytes(b"provider-host-bundle")
        expected_sha = hashlib.sha256(b"provider-host-bundle").hexdigest()

        def remote(command: Sequence[str], **_kwargs: Any) -> str:
            executable = command[0]
            if executable.endswith("/codex"):
                return "codex-cli 0.144.1\n"
            if executable.endswith("/claude"):
                return "2.1.197 (Claude Code)\n"
            if executable == "sha256sum":
                return f"{expected_sha}  {acceptance.SSH_REMOTE_PROVIDER_HOST_PATH}\n"
            raise AssertionError(command)

        driver._remote_command = mock.Mock(side_effect=remote)  # type: ignore[method-assign]

        evidence = driver._inspect_ssh_provider_runtime()

        self.assertEqual(evidence["kind"], "real-provider")
        self.assertEqual(evidence["providerHost"]["sha256"], expected_sha)
        self.assertEqual(evidence["providerTools"]["codex"]["version"], "0.144.1")
        self.assertEqual(evidence["providerTools"]["claudeAgent"]["version"], "2.1.197")


class KubernetesDriverObservationTest(unittest.TestCase):
    def test_cleanup_retries_only_transient_idempotent_kubectl_operations(self) -> None:
        options = dataclasses.replace(
            runner_options(),
            target="kubernetes",
            kubernetes_context="orbstack",
            kubernetes_allow_nondisposable=True,
            kubernetes_shared_local_image_store=True,
        )
        driver = acceptance.KubernetesDriver(
            pathlib.Path.cwd(),
            options,
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        self.addCleanup(driver._release_state)
        owned_resource = json.dumps(
            {
                "metadata": {
                    "labels": {
                        "synara.io/stage3-provider-acceptance-owner": driver.resource_owner,
                    }
                }
            }
        )

        with (
            mock.patch.object(
                driver,
                "_kubectl_completed",
                side_effect=[
                    subprocess.CompletedProcess(
                        ["kubectl"],
                        1,
                        stdout="Unable to connect to the server: unexpected EOF",
                    ),
                    subprocess.CompletedProcess(["kubectl"], 0, stdout=owned_resource),
                ],
            ) as ownership_command,
            mock.patch.object(acceptance.time, "sleep") as sleep,
        ):
            self.assertTrue(driver._kubernetes_resource_is_owned("namespace", "owned"))

        self.assertEqual(ownership_command.call_count, 2)
        sleep.assert_called_once_with(1.0)

        with (
            mock.patch.object(
                driver,
                "_kubectl_completed",
                side_effect=[
                    subprocess.CompletedProcess(
                        ["kubectl"],
                        1,
                        stdout="context deadline exceeded while awaiting headers",
                    ),
                    subprocess.CompletedProcess(["kubectl"], 0, stdout="namespace/owned deleted"),
                ],
            ) as delete_command,
            mock.patch.object(acceptance.time, "sleep") as sleep,
        ):
            output = driver._kubectl_cleanup_command(
                ["delete", "namespace", "owned", "--ignore-not-found"],
                cleanup_timeout=20.0,
            )

        self.assertEqual(output, "namespace/owned deleted")
        self.assertEqual(delete_command.call_count, 2)
        sleep.assert_called_once_with(1.0)

    def test_cleanup_does_not_retry_nontransient_authorization_failure(self) -> None:
        options = dataclasses.replace(
            runner_options(),
            target="kubernetes",
            kubernetes_context="orbstack",
            kubernetes_allow_nondisposable=True,
            kubernetes_shared_local_image_store=True,
        )
        driver = acceptance.KubernetesDriver(
            pathlib.Path.cwd(),
            options,
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        self.addCleanup(driver._release_state)

        with (
            mock.patch.object(
                driver,
                "_kubectl_completed",
                return_value=subprocess.CompletedProcess(
                    ["kubectl"],
                    1,
                    stdout="Error from server (Forbidden): access denied",
                ),
            ) as command,
            mock.patch.object(acceptance.time, "sleep") as sleep,
            self.assertRaises(acceptance.AcceptanceError) as caught,
        ):
            driver._kubernetes_resource_is_owned("namespace", "not-owned")

        self.assertEqual(caught.exception.code, "runner.kubernetes_ownership_check_failed")
        command.assert_called_once()
        sleep.assert_not_called()

    def test_shared_local_image_store_builds_without_kind_load_and_uses_never(self) -> None:
        options = dataclasses.replace(
            runner_options(),
            target="kubernetes",
            kubernetes_context="orbstack",
            kubernetes_allow_nondisposable=True,
            kubernetes_shared_local_image_store=True,
        )

        class SharedStoreDriver(acceptance.KubernetesDriver):
            def _prepare_cluster(self) -> Mapping[str, Any]:
                return {"context": self.context, "ownedCluster": False}

            def _prepare_worker_image(
                self,
                image: str,
                *,
                skip_build: bool,
                log_prefix: str,
            ) -> Mapping[str, Any]:
                self.prepared_image = (image, skip_build, log_prefix)
                return {"workerImage": image, "workerImageId": "sha256:image"}

            def _prepare_cluster_access(self) -> Mapping[str, Any]:
                return {"bootstrapNamespace": self.bootstrap_namespace}

            def _kind_command(self, *_args: Any, **_kwargs: Any) -> str:
                raise AssertionError("shared local image stores must not invoke Kind image loading")

        driver = SharedStoreDriver(
            pathlib.Path.cwd(),
            options,
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        self.addCleanup(driver._release_state)

        with mock.patch.object(acceptance.ManagedWorkerDriver, "prepare", return_value={}):
            evidence = driver.prepare()

        self.assertEqual(driver.prepared_image, (driver.image, False, "kubernetes"))
        self.assertEqual(driver.image_pull_policy, "Never")
        self.assertEqual(
            evidence["kubernetes"]["containerEngine"]["clusterImageTransport"],
            "shared-local-container-engine",
        )

    def test_shared_local_image_store_prepares_canary_without_kind_load(self) -> None:
        options = dataclasses.replace(
            runner_options(),
            target="kubernetes",
            kubernetes_context="orbstack",
            kubernetes_allow_nondisposable=True,
            kubernetes_shared_local_image_store=True,
        )

        class SharedStoreDriver(acceptance.KubernetesDriver):
            def __init__(self, *args: Any, **kwargs: Any) -> None:
                super().__init__(*args, **kwargs)
                self.docker_commands: list[list[str]] = []

            def _docker_command(self, arguments: Sequence[str], **_kwargs: Any) -> str:
                self.docker_commands.append(list(arguments))
                return "sha256:canary" if arguments[:2] == ["image", "inspect"] else ""

            def _kind_command(self, *_args: Any, **_kwargs: Any) -> str:
                raise AssertionError("shared local image stores must not invoke Kind image loading")

        driver = SharedStoreDriver(
            pathlib.Path.cwd(),
            options,
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        self.addCleanup(driver._release_state)

        evidence = driver._prepare_canary_image()

        self.assertEqual(driver.docker_commands[0][:2], ["image", "tag"])
        self.assertEqual(evidence["clusterImageTransport"], "shared-local-container-engine")
        self.assertEqual(evidence["imageId"], "sha256:canary")

    def test_wait_execution_pod_ignores_terminating_runtime_during_replacement(self) -> None:
        options = dataclasses.replace(
            runner_options(),
            target="kubernetes",
            kubernetes_context="kind-fixture",
            kubernetes_kubeconfig=pathlib.Path("/tmp/kind-fixture-kubeconfig"),
        )

        class ReplacementDriver(acceptance.KubernetesDriver):
            def __init__(self, *args: Any, **kwargs: Any) -> None:
                super().__init__(*args, **kwargs)
                self.observations = 0

            def _execution_pods(self, target_id: str, execution_id: str) -> list[dict[str, Any]]:
                del target_id, execution_id
                self.observations += 1
                replacement_phase = "Pending" if self.observations == 1 else "Running"
                return [
                    {
                        "metadata": {
                            "name": "synara-exec-stale",
                            "uid": "pod-uid-stale",
                            "deletionTimestamp": "2026-07-14T00:00:00Z",
                        },
                        "status": {"phase": "Running"},
                    },
                    {
                        "metadata": {"name": "synara-exec-replacement", "uid": "pod-uid-replacement"},
                        "status": {"phase": replacement_phase},
                    },
                ]

        driver = ReplacementDriver(
            pathlib.Path.cwd(),
            options,
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        self.addCleanup(driver._release_state)

        with mock.patch.object(driver.deadline, "sleep", return_value=None):
            pod = driver._wait_execution_pod("target-id", "execution-id")

        self.assertEqual(pod["metadata"]["uid"], "pod-uid-replacement")
        self.assertEqual(driver.observations, 2)

    def test_observe_execution_validates_real_pod_contract(self) -> None:
        options = dataclasses.replace(
            runner_options(),
            target="kubernetes",
            kubernetes_context="kind-fixture",
            kubernetes_kubeconfig=pathlib.Path("/tmp/kind-fixture-kubeconfig"),
        )

        class ObservationDriver(acceptance.KubernetesDriver):
            def _wait_execution_pod(self, target_id: str, execution_id: str) -> dict[str, Any]:
                compact = target_id.replace("-", "")[:12]
                return {
                    "metadata": {
                        "name": "synara-exec-fixture",
                        "uid": "pod-uid",
                        "labels": {
                            "synara.io/managed": "true",
                            "synara.io/execution-target-id": target_id,
                            "synara.io/execution-id": execution_id,
                            "synara.io/generation": "1",
                        },
                    },
                    "spec": {
                        "serviceAccountName": self.worker_service_account,
                        "automountServiceAccountToken": False,
                        "restartPolicy": "Never",
                        "securityContext": {"runAsNonRoot": True, "fsGroup": 10001},
                        "volumes": [
                            {"name": "workspace", "emptyDir": {}},
                            {"name": "tmp", "emptyDir": {}},
                            {"name": "home", "emptyDir": {}},
                        ],
                        "containers": [
                            {
                                "name": "agentd",
                                "image": self.image,
                                "imagePullPolicy": "Never",
                                "securityContext": {
                                    "allowPrivilegeEscalation": False,
                                    "readOnlyRootFilesystem": True,
                                    "runAsNonRoot": True,
                                    "runAsUser": 10001,
                                    "runAsGroup": 10001,
                                    "capabilities": {"drop": ["ALL"]},
                                },
                                "env": [
                                    {
                                        "name": "SYNARA_AGENTD_ASSIGNED_EXECUTION_ID",
                                        "value": execution_id,
                                    },
                                    {
                                        "name": "SYNARA_WORKER_REGISTRATION_TOKEN",
                                        "valueFrom": {
                                            "secretKeyRef": {
                                                "name": f"synara-agentd-{compact}",
                                                "key": "registration-token",
                                            }
                                        },
                                    },
                                ],
                            }
                        ],
                    },
                    "status": {"phase": "Running"},
                }

            def _foundation_evidence(
                self,
                target_id: str,
                secret_name: Any,
                **_kwargs: Any,
            ) -> Mapping[str, Any]:
                del target_id, secret_name
                return {"serviceAccount": self.worker_service_account}

        driver = ObservationDriver(
            pathlib.Path.cwd(),
            options,
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        self.addCleanup(driver._release_state)
        target_id = "11111111-2222-3333-4444-555555555555"
        evidence = driver.observe_execution(target_id, "execution-id")

        self.assertEqual(evidence["phase"], "Running")
        self.assertEqual(evidence["volumes"], ["home", "tmp", "workspace"])
        self.assertEqual(evidence["foundation"]["serviceAccount"], driver.worker_service_account)

    def test_recover_pending_interaction_force_deletes_exact_execution_pod(self) -> None:
        options = dataclasses.replace(
            runner_options(),
            target="kubernetes",
            kubernetes_context="kind-fixture",
            kubernetes_kubeconfig=pathlib.Path("/tmp/kind-fixture-kubeconfig"),
        )

        class RecoveryDriver(acceptance.KubernetesDriver):
            def __init__(self, *args: Any, **kwargs: Any) -> None:
                super().__init__(*args, **kwargs)
                self.commands: list[list[str]] = []

            def _wait_execution_pod(self, target_id: str, execution_id: str) -> dict[str, Any]:
                del target_id, execution_id
                return {
                    "metadata": {
                        "name": "synara-exec-fixture",
                        "uid": "pod-uid",
                        "labels": {"synara.io/generation": "2"},
                    },
                    "status": {"phase": "Running"},
                }

            def _kubectl_command(
                self,
                arguments: Sequence[str],
                *,
                input_text: str | None = None,
                cleanup_timeout: float | None = None,
            ) -> str:
                del input_text, cleanup_timeout
                self.commands.append(list(arguments))
                return ""

        driver = RecoveryDriver(
            pathlib.Path.cwd(),
            options,
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        self.addCleanup(driver._release_state)

        evidence = driver.recover_pending_interaction("target-id", "execution-id")

        self.assertEqual(evidence["deletedPodName"], "synara-exec-fixture")
        self.assertEqual(evidence["deletedPodUid"], "pod-uid")
        self.assertEqual(evidence["deletedPodGeneration"], "2")
        self.assertEqual(
            driver.commands,
            [
                [
                    "-n",
                    driver.target_namespace,
                    "delete",
                    "pod",
                    "synara-exec-fixture",
                    "--grace-period=0",
                    "--force",
                    "--wait=false",
                ]
            ],
        )

    def test_eviction_uses_policy_v1_uid_precondition_for_exact_pod(self) -> None:
        options = dataclasses.replace(
            runner_options(),
            target="kubernetes",
            kubernetes_context="kind-fixture",
            kubernetes_kubeconfig=pathlib.Path("/tmp/kind-fixture-kubeconfig"),
        )

        class EvictionDriver(acceptance.KubernetesDriver):
            def __init__(self, *args: Any, **kwargs: Any) -> None:
                super().__init__(*args, **kwargs)
                self.commands: list[tuple[list[str], str | None]] = []

            def _wait_execution_pod(self, target_id: str, execution_id: str) -> dict[str, Any]:
                del target_id, execution_id
                return {
                    "metadata": {
                        "name": "synara-exec-fixture",
                        "uid": "pod-uid",
                        "labels": {"synara.io/generation": "3"},
                    },
                    "spec": {"nodeName": "kind-control-plane"},
                    "status": {"phase": "Running"},
                }

            def _kubectl_command(
                self,
                arguments: Sequence[str],
                *,
                input_text: str | None = None,
                cleanup_timeout: float | None = None,
            ) -> str:
                del cleanup_timeout
                self.commands.append((list(arguments), input_text))
                return ""

        driver = EvictionDriver(
            pathlib.Path.cwd(),
            options,
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        self.addCleanup(driver._release_state)

        evidence = driver.inject_failure("kubernetes-eviction", "target-id", "execution-id")

        command, payload = driver.commands[0]
        self.assertEqual(command[:2], ["create", "--raw"])
        self.assertTrue(command[2].endswith("/pods/synara-exec-fixture/eviction"))
        self.assertEqual(
            json.loads(payload or "{}")["deleteOptions"]["preconditions"]["uid"],
            "pod-uid",
        )
        self.assertTrue(evidence["uidPrecondition"])

    def test_node_drain_is_scoped_and_always_uncordons_owned_node(self) -> None:
        options = dataclasses.replace(
            runner_options(),
            target="kubernetes",
            kubernetes_context="kind-fixture",
            kubernetes_kubeconfig=pathlib.Path("/tmp/kind-fixture-kubeconfig"),
            kubernetes_allow_node_drain=True,
        )

        class DrainDriver(acceptance.KubernetesDriver):
            def __init__(self, *args: Any, **kwargs: Any) -> None:
                super().__init__(*args, **kwargs)
                self.commands: list[list[str]] = []

            def _wait_execution_pod(self, target_id: str, execution_id: str) -> dict[str, Any]:
                del target_id, execution_id
                return {
                    "metadata": {
                        "name": "synara-exec-fixture",
                        "uid": "pod-uid",
                        "labels": {"synara.io/generation": "4"},
                    },
                    "spec": {"nodeName": "kind-control-plane"},
                    "status": {"phase": "Running"},
                }

            def _kubectl_command(
                self,
                arguments: Sequence[str],
                *,
                input_text: str | None = None,
                cleanup_timeout: float | None = None,
            ) -> str:
                del input_text, cleanup_timeout
                self.commands.append(list(arguments))
                return ""

        driver = DrainDriver(
            pathlib.Path.cwd(),
            options,
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        self.addCleanup(driver._release_state)

        evidence = driver.inject_failure("kubernetes-drain", "target-id", "execution-id")

        self.assertEqual(driver.commands[0], ["cordon", "kind-control-plane"])
        self.assertEqual(driver.commands[-1], ["uncordon", "kind-control-plane"])
        drain = driver.commands[1]
        self.assertEqual(drain[:2], ["drain", "kind-control-plane"])
        self.assertIn(
            "--pod-selector=synara.io/execution-target-id=target-id,synara.io/execution-id=execution-id",
            drain,
        )
        self.assertIn("--disable-eviction", drain)
        self.assertTrue(evidence["uncordoned"])


if __name__ == "__main__":
    unittest.main()
