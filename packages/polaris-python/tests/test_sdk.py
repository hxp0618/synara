from __future__ import annotations

import io
import json
import unittest
from email.message import Message
from typing import Any
from urllib.error import URLError

from polaris_agents import Polaris, PolarisSequenceGapError
from polaris_agents._generated import OPERATIONS
from polaris_agents._transport import PolarisTransport


class FakeResponse:
    def __init__(self, value: dict[str, Any] | None = None, *, stream: bytes = b"", status: int = 200) -> None:
        self.headers = Message()
        self.headers["X-Request-ID"] = "request-1"
        self.headers["RateLimit-Limit"] = "600"
        self.headers["RateLimit-Remaining"] = "599"
        self._body = io.BytesIO(stream or json.dumps(value or {}).encode())
        self.closed = False
        self.status = status

    def getcode(self) -> int:
        return self.status

    def read(self) -> bytes:
        return self._body.read()

    def readline(self) -> bytes:
        return self._body.readline()

    def close(self) -> None:
        self.closed = True


class PolarisSDKTest(unittest.TestCase):
    def test_generated_surface_contains_only_codegen_ready_operations(self) -> None:
        self.assertEqual(
            set(OPERATIONS),
            {
                "createSession",
                "createTurn",
                "listSessionEvents",
                "resolveExecutionApproval",
                "streamSessionEvents",
                "createExecutionTarget",
                "createExecutionTargetProvisioningOperation",
                "getExecutionTarget",
                "getExecutionTargetProvisioningOperation",
                "createProject",
                "updateProject",
                "archiveProject",
                "getProject",
                "listProjects",
                "projectProviderCapabilities",
                "getAgentSession",
                "listProjectSessions",
                "archiveSession",
                "sessionProviderCapabilities",
                "switchSessionModel",
                "suspendSession",
                "resumeSession",
                "getSessionUsage",
                "cancelExecution",
                "interruptActiveTurn",
                "resumeActiveTurnExecution",
                "resumeActiveTurn",
                "steerActiveTurn",
                "compactSession",
                "startSessionReview",
                "rollbackSession",
                "forkSession",
                "listExecutionInteractions",
                "listPendingSessionInteractions",
                "resolveExecutionUserInput",
                "getArtifact",
                "listArtifacts",
                "downloadArtifact",
                "createArtifact",
                "completeArtifact",
                "deleteArtifact",
            },
        )

    def test_artifact_create_and_complete_preserve_retry_keys(self) -> None:
        calls: list[Any] = []
        responses = [
            FakeResponse({"artifact": {"id": "artifact-1", "status": "pending"}, "uploadRequired": True}),
            FakeResponse({"id": "artifact-1", "status": "ready"}),
            FakeResponse(status=204),
            FakeResponse(status=204),
        ]

        def opener(request: Any, **_kwargs: Any) -> FakeResponse:
            calls.append(request)
            return responses.pop(0)

        transport = PolarisTransport(
            api_key="syna_sa_test", base_url="https://api.example.com", opener=opener,
        )
        sdk = Polaris(
            api_key="syna_sa_test", base_url="https://api.example.com", _transport=transport,
        )
        created = sdk.artifacts.create(
            session_id="session-1", kind="generated_file", original_name="result.txt",
            idempotency_key="artifact-create",
        )
        completed = sdk.artifacts.complete(
            artifact_id=created.data["artifact"]["id"], size_bytes=17, sha256="a" * 64,
            content_type="text/plain", idempotency_key="artifact-complete",
        )
        sdk.artifacts.delete(artifact_id=completed.data["id"], idempotency_key="artifact-delete")
        sdk.artifacts.delete(artifact_id=completed.data["id"], idempotency_key="artifact-delete")

        self.assertEqual(completed.data["status"], "ready")
        self.assertEqual(calls[0].headers["Idempotency-key"], "artifact-create")
        self.assertEqual(calls[1].headers["Idempotency-key"], "artifact-complete")
        self.assertEqual(calls[2].headers["Idempotency-key"], "artifact-delete")

    def test_artifacts_resource_reads_payload_free_metadata(self) -> None:
        calls: list[Any] = []
        artifact = {"id": "artifact/one", "kind": "generated_file", "status": "ready"}
        responses = [
            FakeResponse({"items": [artifact], "nextCursor": None}),
            FakeResponse(artifact),
            FakeResponse({"artifact": artifact, "url": "/v1/artifact-content/token", "expiresAt": "2026-08-04T00:15:00Z"}),
        ]

        def opener(request: Any, **_kwargs: Any) -> FakeResponse:
            calls.append(request)
            return responses.pop(0)

        transport = PolarisTransport(
            api_key="syna_sa_test", base_url="https://api.example.com", opener=opener,
        )
        sdk = Polaris(
            api_key="syna_sa_test", base_url="https://api.example.com", _transport=transport,
        )
        page = sdk.artifacts.list(session_id="session/one", limit=25)
        read = sdk.artifacts.get(artifact_id=page.data["items"][0]["id"])
        grant = sdk.artifacts.download(artifact_id=read.data["id"])

        self.assertEqual(read.data["id"], "artifact/one")
        self.assertEqual(grant.data["url"], "/v1/artifact-content/token")
        self.assertEqual(calls[0].full_url, "https://api.example.com/v1/sessions/session%2Fone/artifacts?limit=25")
        self.assertEqual(calls[1].full_url, "https://api.example.com/v1/artifacts/artifact%2Fone")
        self.assertEqual(calls[2].full_url, "https://api.example.com/v1/artifacts/artifact%2Fone/download")
        self.assertIsNone(calls[2].data)
        self.assertNotIn("Idempotency-key", calls[2].headers)

    def test_interactions_resources_use_bounded_pages(self) -> None:
        calls: list[Any] = []
        pending = {"id": "interaction-1", "requestId": "approval-1"}
        responses = [
            FakeResponse({"id": "session-1"}),
            FakeResponse({"items": [pending], "nextCursor": None, "snapshotSequence": 7}),
            FakeResponse({"items": [pending], "nextCursor": None}),
            FakeResponse({"id": "interaction-1", "status": "resolved"}),
        ]

        def opener(request: Any, **_kwargs: Any) -> FakeResponse:
            calls.append(request)
            return responses.pop(0)

        transport = PolarisTransport(
            api_key="syna_sa_test", base_url="https://api.example.com", opener=opener,
        )
        sdk = Polaris(
            api_key="syna_sa_test", base_url="https://api.example.com", _transport=transport,
        )
        session = sdk.sessions.get(session_id="session-1")
        pending_page = session.pending_interactions(limit=25)
        history_page = sdk.interactions.list(execution_id="execution/one", limit=25)
        resolved = sdk.interactions.resolve_user_input(
            "execution/one", "input/one", {"environment": "staging"},
            idempotency_key="input-key",
        )

        self.assertEqual(pending_page.data["snapshotSequence"], 7)
        self.assertEqual(history_page.data["items"][0]["requestId"], "approval-1")
        self.assertEqual(resolved.data["status"], "resolved")
        self.assertEqual(calls[1].full_url, "https://api.example.com/v1/sessions/session-1/interactions?limit=25")
        self.assertEqual(calls[2].full_url, "https://api.example.com/v1/executions/execution%2Fone/interactions?limit=25")
        self.assertEqual(calls[3].full_url, "https://api.example.com/v1/executions/execution%2Fone/user-input/input%2Fone/resolve")
        self.assertEqual(calls[3].headers["Idempotency-key"], "input-key")

    def test_sessions_resource_lists_and_reads_bounded_handles(self) -> None:
        calls: list[Any] = []
        session = {"id": "session/one", "title": "SDK Session"}
        responses = [
            FakeResponse({"items": [session], "nextCursor": "next-page"}),
            FakeResponse(session),
            FakeResponse({"executionTargetId": "target-1", "targetKind": "ssh", "basis": "target", "items": []}),
            FakeResponse({"tenantId": "tenant-1", "sessionId": "session/one", "items": []}),
            FakeResponse({**session, "model": "model-next"}),
            FakeResponse({**session, "status": "suspended"}),
            FakeResponse({**session, "status": "active"}),
            FakeResponse({"id": "execution-1", "status": "recovering"}),
            FakeResponse({"id": "command-1", "commandType": "SteerTurn", "status": "pending"}),
            FakeResponse({"id": "command-2", "commandType": "InterruptTurn", "status": "pending"}),
            FakeResponse({**session, "status": "archived"}),
        ]

        def opener(request: Any, **_kwargs: Any) -> FakeResponse:
            calls.append(request)
            return responses.pop(0)

        transport = PolarisTransport(
            api_key="syna_sa_test", base_url="https://api.example.com", opener=opener,
        )
        sdk = Polaris(
            api_key="syna_sa_test", base_url="https://api.example.com", _transport=transport,
        )
        page = sdk.sessions.list(project_id="project/one", limit=25, cursor="cursor+/=")
        handle = sdk.sessions.get(session_id=page.data["items"][0]["id"])
        capabilities = handle.capabilities()
        usage = handle.usage()
        switched = handle.switch_model(
            model="model-next", expected_model=None, idempotency_key="session-model-switch",
        )
        suspended = handle.suspend(idempotency_key="session-suspend")
        resumed = handle.resume(idempotency_key="session-resume")
        resumed_turn = handle.resume_active_turn(idempotency_key="turn-resume")
        steered = handle.steer(input_text="Continue with the narrower fix.", idempotency_key="turn-steer")
        interrupted = handle.interrupt(idempotency_key="turn-interrupt")
        archived = handle.archive(idempotency_key="session-archive")

        self.assertEqual(handle.id, "session/one")
        self.assertEqual(capabilities.data["basis"], "target")
        self.assertEqual(usage.data["items"], [])
        self.assertEqual(switched.data["model"], "model-next")
        self.assertEqual(suspended.data["status"], "suspended")
        self.assertEqual(resumed.data["status"], "active")
        self.assertEqual(resumed_turn.data["status"], "recovering")
        self.assertEqual(steered.data["commandType"], "SteerTurn")
        self.assertEqual(interrupted.data["commandType"], "InterruptTurn")
        self.assertEqual(archived.data["status"], "archived")
        self.assertEqual(calls[0].full_url, "https://api.example.com/v1/projects/project%2Fone/sessions?limit=25&cursor=cursor%2B%2F%3D")
        self.assertEqual(calls[1].full_url, "https://api.example.com/v1/sessions/session%2Fone")
        self.assertEqual(calls[2].full_url, "https://api.example.com/v1/sessions/session%2Fone/provider-capabilities")
        self.assertEqual(calls[3].full_url, "https://api.example.com/v1/sessions/session%2Fone/usage")
        self.assertEqual(calls[4].full_url, "https://api.example.com/v1/sessions/session%2Fone/model-switch")
        self.assertEqual(calls[4].headers["Idempotency-key"], "session-model-switch")
        self.assertEqual(calls[5].full_url, "https://api.example.com/v1/sessions/session%2Fone/suspend")
        self.assertEqual(calls[5].headers["Idempotency-key"], "session-suspend")
        self.assertEqual(calls[6].full_url, "https://api.example.com/v1/sessions/session%2Fone/resume")
        self.assertEqual(calls[6].headers["Idempotency-key"], "session-resume")
        self.assertEqual(calls[7].full_url, "https://api.example.com/v1/sessions/session%2Fone/turns/active/resume")
        self.assertEqual(calls[7].headers["Idempotency-key"], "turn-resume")
        self.assertEqual(calls[8].full_url, "https://api.example.com/v1/sessions/session%2Fone/turns/active/steer")
        self.assertEqual(calls[8].headers["Idempotency-key"], "turn-steer")
        self.assertEqual(calls[9].full_url, "https://api.example.com/v1/sessions/session%2Fone/turns/active/interrupt")
        self.assertEqual(calls[9].headers["Idempotency-key"], "turn-interrupt")
        self.assertEqual(calls[10].full_url, "https://api.example.com/v1/sessions/session%2Fone/archive")
        self.assertEqual(calls[10].headers["Idempotency-key"], "session-archive")

    def test_projects_resource_encodes_bounded_public_surface(self) -> None:
        calls: list[Any] = []
        project = {"id": "project/one", "name": "SDK Project"}
        responses = [
            FakeResponse(project),
            FakeResponse({"items": [project], "nextCursor": None}),
            FakeResponse(project),
            FakeResponse({**project, "name": "Updated Project"}),
            FakeResponse(status=204),
            FakeResponse({"executionTargetId": "target/one", "targetKind": "ssh", "basis": "target", "items": []}),
        ]

        def opener(request: Any, **_kwargs: Any) -> FakeResponse:
            calls.append(request)
            return responses.pop(0)

        transport = PolarisTransport(
            api_key="syna_sa_test", base_url="https://api.example.com", opener=opener,
        )
        sdk = Polaris(
            api_key="syna_sa_test", base_url="https://api.example.com", _transport=transport,
        )
        created = sdk.projects.create(
            tenant_id="tenant/one", organization_id="organization/one", name="SDK Project",
            idempotency_key="project-key",
        )
        listed = sdk.projects.list(
            tenant_id="tenant/one", organization_id="organization/one", limit=25,
        )
        read = sdk.projects.get(project_id=created.data["id"])
        updated = sdk.projects.update(
            project_id=created.data["id"], name="Updated Project", idempotency_key="project-update",
        )
        sdk.projects.archive(project_id=created.data["id"], idempotency_key="project-archive")
        capabilities = sdk.projects.capabilities(
            project_id=created.data["id"], execution_target_id="target/one",
        )

        self.assertEqual(listed.data["items"][0]["id"], "project/one")
        self.assertEqual(read.data["id"], "project/one")
        self.assertEqual(updated.data["name"], "Updated Project")
        self.assertEqual(capabilities.data["basis"], "target")
        self.assertEqual(calls[0].full_url, "https://api.example.com/v1/tenants/tenant%2Fone/organizations/organization%2Fone/projects")
        self.assertEqual(calls[0].headers["Idempotency-key"], "project-key")
        self.assertEqual(calls[1].full_url, "https://api.example.com/v1/tenants/tenant%2Fone/organizations/organization%2Fone/projects?limit=25")
        self.assertEqual(calls[2].full_url, "https://api.example.com/v1/projects/project%2Fone")
        self.assertEqual(calls[3].full_url, "https://api.example.com/v1/projects/project%2Fone")
        self.assertEqual(calls[3].headers["Idempotency-key"], "project-update")
        self.assertEqual(calls[4].full_url, "https://api.example.com/v1/projects/project%2Fone")
        self.assertEqual(calls[4].headers["Idempotency-key"], "project-archive")
        self.assertEqual(calls[5].full_url, "https://api.example.com/v1/projects/project%2Fone/provider-capabilities?executionTargetId=target%2Fone")

    def test_domain_client_encodes_paths_and_preserves_idempotency(self) -> None:
        calls: list[Any] = []
        responses = [
            FakeResponse({"id": "session/one"}),
            FakeResponse({"id": "turn-1", "sessionId": "session/one"}),
            FakeResponse({"id": "target/one", "kind": "ssh"}),
            FakeResponse({"id": "target/one", "kind": "ssh"}),
            FakeResponse({"id": "operation/one", "targetId": "target/one", "action": "install", "state": "accepted", "attempt": 0}),
            FakeResponse({"id": "operation/one", "targetId": "target/one", "action": "install", "state": "succeeded", "attempt": 1}),
        ]

        def opener(request: Any, **_kwargs: Any) -> FakeResponse:
            calls.append(request)
            return responses.pop(0)

        transport = PolarisTransport(
            api_key="syna_sa_test",
            base_url="https://api.example.com/",
            opener=opener,
        )
        sdk = Polaris(
            api_key="syna_sa_test",
            base_url="https://api.example.com",
            _transport=transport,
        )
        session = sdk.sessions.create(
            project_id="project/one",
            title="Fix CI",
            provider="codex",
            idempotency_key="create-key",
        )
        turn = session.send_turn(input_text="Fix tests", idempotency_key="turn-key")
        target = sdk.targets.create(
            tenant_id="tenant/one",
            organization_id="organization-1",
            kind="ssh",
            name="build host",
            configuration={},
            idempotency_key="target-key",
        )
        read_target = sdk.targets.get(tenant_id="tenant/one", execution_target_id=target.data["id"])
        operation = sdk.targets.provision(
            tenant_id="tenant/one", execution_target_id=target.data["id"], action="install",
            idempotency_key="provision-key",
        )
        completed = sdk.targets.wait_for_provisioning(
            tenant_id="tenant/one", execution_target_id=target.data["id"],
            operation_id=operation.data["id"], poll_interval=0,
        )

        self.assertEqual(session.id, "session/one")
        self.assertEqual(turn.data["id"], "turn-1")
        self.assertEqual(read_target.data["id"], "target/one")
        self.assertEqual(completed.data["state"], "succeeded")
        self.assertEqual(calls[0].full_url, "https://api.example.com/v1/projects/project%2Fone/sessions")
        self.assertEqual(calls[0].headers["Idempotency-key"], "create-key")
        self.assertEqual(calls[1].full_url, "https://api.example.com/v1/sessions/session%2Fone/turns")
        self.assertEqual(json.loads(calls[1].data), {
            "inputText": "Fix tests",
            "runtimeMode": "full-access",
            "interactionMode": "default",
        })
        self.assertEqual(calls[2].full_url, "https://api.example.com/v1/tenants/tenant%2Fone/execution-targets")
        self.assertEqual(calls[2].headers["Idempotency-key"], "target-key")
        self.assertEqual(calls[3].full_url, "https://api.example.com/v1/tenants/tenant%2Fone/execution-targets/target%2Fone")
        self.assertEqual(calls[4].full_url, "https://api.example.com/v1/tenants/tenant%2Fone/execution-targets/target%2Fone/provisioning-operations")
        self.assertEqual(calls[4].headers["Idempotency-key"], "provision-key")
        self.assertEqual(calls[5].full_url, "https://api.example.com/v1/tenants/tenant%2Fone/execution-targets/target%2Fone/provisioning-operations/operation%2Fone")

    def test_mutation_retries_network_failure_with_same_body_and_key(self) -> None:
        calls: list[Any] = []

        def opener(request: Any, **_kwargs: Any) -> FakeResponse:
            calls.append(request)
            if len(calls) == 1:
                raise URLError("temporary")
            return FakeResponse({"id": "session-1"})

        transport = PolarisTransport(
            api_key="syna_sa_test",
            base_url="https://api.example.com",
            opener=opener,
            sleep=lambda _seconds: None,
        )
        sdk = Polaris(
            api_key="syna_sa_test",
            base_url="https://api.example.com",
            _transport=transport,
        )
        sdk.sessions.create(
            project_id="project-1",
            title="Retry",
            provider="codex",
            idempotency_key="stable-key",
        )
        self.assertEqual(len(calls), 2)
        self.assertEqual(calls[0].data, calls[1].data)
        self.assertEqual(calls[0].headers["Idempotency-key"], calls[1].headers["Idempotency-key"])

    def test_execution_cancel_uses_sanitized_public_route(self) -> None:
        calls: list[Any] = []

        def opener(request: Any, **_kwargs: Any) -> FakeResponse:
            calls.append(request)
            status = "cancelled" if len(calls) == 1 else "recovering"
            return FakeResponse({"id": "execution/one", "status": status})

        sdk = Polaris(
            api_key="syna_sa_test", base_url="https://api.example.com",
            _transport=PolarisTransport(
                api_key="syna_sa_test", base_url="https://api.example.com", opener=opener,
            ),
        )
        cancelled = sdk.executions.cancel(
            execution_id="execution/one", idempotency_key="execution-cancel",
        )
        resumed = sdk.executions.resume_active_turn(
            execution_id="execution/one", idempotency_key="execution-resume",
        )

        self.assertEqual(cancelled.data["status"], "cancelled")
        self.assertEqual(resumed.data["status"], "recovering")
        self.assertEqual(calls[0].full_url, "https://api.example.com/v1/executions/execution%2Fone/cancel")
        self.assertEqual(calls[0].headers["Idempotency-key"], "execution-cancel")
        self.assertEqual(calls[1].full_url, "https://api.example.com/v1/executions/execution%2Fone/resume")
        self.assertEqual(calls[1].headers["Idempotency-key"], "execution-resume")

    def test_sequence_guarded_session_operations_encode_stable_inputs(self) -> None:
        calls: list[Any] = []
        session = {"id": "session-1"}
        responses = [
            FakeResponse(session),
            FakeResponse({"type": "compact", "executionId": "execution-1"}),
            FakeResponse({"type": "review", "executionId": "execution-2"}),
            FakeResponse({"sessionId": "session-1", "removedTurnCount": 1}),
            FakeResponse({"session": {"id": "session-fork"}, "sourceSessionId": "session-1"}),
        ]

        def opener(request: Any, **_kwargs: Any) -> FakeResponse:
            calls.append(request)
            return responses.pop(0)

        sdk = Polaris(
            api_key="syna_sa_test", base_url="https://api.example.com",
            _transport=PolarisTransport(
                api_key="syna_sa_test", base_url="https://api.example.com", opener=opener,
            ),
        )
        handle = sdk.sessions.get(session_id="session-1")
        handle.compact(expected_last_event_sequence=7, idempotency_key="compact-key")
        handle.start_review(
            expected_last_event_sequence=7, target_type="baseBranch", branch="main",
            idempotency_key="review-key",
        )
        handle.rollback(
            expected_last_event_sequence=7, from_turn_id="turn-1", idempotency_key="rollback-key",
        )
        handle.fork(
            expected_last_event_sequence=7, title="Forked", visibility="organization",
            idempotency_key="fork-key",
        )

        self.assertEqual([call.full_url for call in calls[1:]], [
            "https://api.example.com/v1/sessions/session-1/compact",
            "https://api.example.com/v1/sessions/session-1/reviews",
            "https://api.example.com/v1/sessions/session-1/rollback",
            "https://api.example.com/v1/sessions/session-1/fork",
        ])
        self.assertEqual([call.headers["Idempotency-key"] for call in calls[1:]], [
            "compact-key", "review-key", "rollback-key", "fork-key",
        ])
        self.assertEqual(json.loads(calls[2].data)["target"], {"type": "baseBranch", "branch": "main"})

    def test_event_stream_suppresses_replay_and_fails_closed_on_gap(self) -> None:
        stream_payload = _sse(1, "session.created") + _sse(1, "session.created") + _sse(2, "turn.created")

        def opener(request: Any, **_kwargs: Any) -> FakeResponse:
            if request.method == "POST":
                return FakeResponse({"id": "session-1"})
            return FakeResponse(stream=stream_payload)

        transport = PolarisTransport(
            api_key="syna_sa_test",
            base_url="https://api.example.com",
            opener=opener,
        )
        session = Polaris(
            api_key="syna_sa_test",
            base_url="https://api.example.com",
            _transport=transport,
        ).sessions.create(project_id="project-1", title="SSE", provider="codex")
        events = session.events(reconnect=False)
        self.assertEqual([event["sequence"] for event in events], [1, 2])

        transport._opener = lambda *_args, **_kwargs: FakeResponse(stream=_sse(1, "one") + _sse(3, "three"))  # type: ignore[method-assign]
        with self.assertRaises(PolarisSequenceGapError):
            list(session.events(reconnect=False))

        transport._opener = lambda *_args, **_kwargs: FakeResponse(  # type: ignore[method-assign]
            stream=_sse(1, "started", "execution-other") + _sse(2, "completed", "execution-wanted")
        )
        filtered = list(session.events_for_execution("execution-wanted", reconnect=False))
        self.assertEqual([(event["sequence"], event["executionId"]) for event in filtered], [(2, "execution-wanted")])


def _sse(sequence: int, event_type: str, execution_id: str | None = None) -> bytes:
    value = {"sequence": sequence, "eventType": event_type, "executionId": execution_id}
    return f"event: session-event\ndata: {json.dumps(value)}\n\n".encode()


if __name__ == "__main__":
    unittest.main()
