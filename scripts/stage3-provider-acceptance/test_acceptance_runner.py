from __future__ import annotations

import contextlib
import base64
import dataclasses
import hashlib
import io
import json
import pathlib
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
        ssh_orbctl_bin="orbctl",
        ssh_machine_name=None,
        ssh_machine_arch="arm64",
        ssh_machine_image="ubuntu:24.04",
        ssh_node_version="24.13.1",
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
    ) -> dict[str, Any]:
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
            }
        ]
        if self.recovered:
            events.append(
                {
                    "sequence": 2,
                    "eventType": "execution.recovering",
                    "executionId": "execution-1",
                    "payload": {"turnId": "turn-1", "reason": "lease_expired"},
                }
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
        self.assertEqual(evidence["credentialHeaderNames"], ["authorization"])
        self.assertFalse(evidence["requestBodiesRetained"])
        self.assertNotIn(credential, json.dumps(evidence))


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


class ManagedWorkerImageTest(unittest.TestCase):
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

    def test_standing_restart_waits_for_online_worker(self) -> None:
        suite = BarrierSuite(acceptance.STANDING_WORKER)

        evidence = suite._restart_control_plane()

        self.assertEqual(suite.post_restart_waits, 1)
        self.assertEqual(evidence["postRestartManifestId"], "manifest-after-restart")


class RunnerOptionsTest(unittest.TestCase):
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
                self.absent_target = target_id

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
        self.assertEqual(driver.absent_target, target_ids[0])
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


class KubernetesDriverObservationTest(unittest.TestCase):
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
