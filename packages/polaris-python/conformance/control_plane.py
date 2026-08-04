from __future__ import annotations

import json
import hashlib
import os
import sys
import time
import uuid
from pathlib import Path
from urllib.parse import urljoin
from urllib.error import HTTPError
from urllib.request import Request, urlopen

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "src"))

from polaris_agents import Polaris  # noqa: E402


def required(name: str) -> str:
    value = os.environ.get(name, "").strip()
    if not value:
        raise RuntimeError(f"{name} is required")
    return value


base_url = required("POLARIS_CONFORMANCE_BASE_URL")
api_key = required("POLARIS_CONFORMANCE_API_KEY")
target_id = required("POLARIS_CONFORMANCE_EXECUTION_TARGET_ID")
approval_execution_id = required("POLARIS_CONFORMANCE_APPROVAL_EXECUTION_ID")
approval_session_id = required("POLARIS_CONFORMANCE_APPROVAL_SESSION_ID")
approval_request_id = required("POLARIS_CONFORMANCE_APPROVAL_REQUEST_ID")
user_input_request_id = required("POLARIS_CONFORMANCE_USER_INPUT_REQUEST_ID")
artifact_id = required("POLARIS_CONFORMANCE_ARTIFACT_ID")
target_api_key = required("POLARIS_CONFORMANCE_TARGET_API_KEY")
tenant_id = required("POLARIS_CONFORMANCE_TENANT_ID")
organization_id = required("POLARIS_CONFORMANCE_ORGANIZATION_ID")

polaris = Polaris(api_key=api_key, base_url=base_url, max_retries=0, timeout=5)
target_polaris = Polaris(api_key=target_api_key, base_url=base_url, max_retries=0, timeout=5)
project_key = f"python-conformance-project-{uuid.uuid4()}"
project_name = f"Python SDK conformance {uuid.uuid4()}"
project = target_polaris.projects.create(
    tenant_id=tenant_id, organization_id=organization_id, name=project_name,
    idempotency_key=project_key,
)
project_replay = target_polaris.projects.create(
    tenant_id=tenant_id, organization_id=organization_id, name=project_name,
    idempotency_key=project_key,
)
project_page = target_polaris.projects.list(
    tenant_id=tenant_id, organization_id=organization_id, limit=50,
)
project_read = target_polaris.projects.get(project_id=project.data["id"])
assert project_replay.idempotency_replayed
assert project_read.data["id"] == project.data["id"]
assert any(item["id"] == project.data["id"] for item in project_page.data["items"])
project_update_key = f"python-conformance-project-update-{uuid.uuid4()}"
updated_project = target_polaris.projects.update(
    project_id=project.data["id"], name=f"{project_name} updated", idempotency_key=project_update_key,
)
updated_project_replay = target_polaris.projects.update(
    project_id=project.data["id"], name=f"{project_name} updated", idempotency_key=project_update_key,
)
assert updated_project.data["name"].endswith(" updated")
assert updated_project_replay.idempotency_replayed
archive_candidate = target_polaris.projects.create(
    tenant_id=tenant_id, organization_id=organization_id,
    name=f"Python archive conformance {uuid.uuid4()}",
    idempotency_key=f"python-conformance-project-archive-create-{uuid.uuid4()}",
)
project_archive_key = f"python-conformance-project-archive-{uuid.uuid4()}"
target_polaris.projects.archive(project_id=archive_candidate.data["id"], idempotency_key=project_archive_key)
archived_replay = target_polaris.projects.archive(
    project_id=archive_candidate.data["id"], idempotency_key=project_archive_key,
)
assert archived_replay.idempotency_replayed
project_capabilities = target_polaris.projects.capabilities(
    project_id=project.data["id"], execution_target_id=target_id,
)
assert project_capabilities.data["executionTargetId"] == target_id
assert project_capabilities.data["basis"] == "target"
create_key = f"python-conformance-create-{uuid.uuid4()}"
session = polaris.sessions.create(
    project_id=project.data["id"],
    title="Polaris Python SDK conformance",
    provider="codex",
    execution_target_id=target_id,
    idempotency_key=create_key,
)
replayed = polaris.sessions.create(
    project_id=project.data["id"],
    title="Polaris Python SDK conformance",
    provider="codex",
    execution_target_id=target_id,
    idempotency_key=create_key,
)
assert replayed.id == session.id and replayed.response.idempotency_replayed
session_capabilities = session.capabilities()
assert session_capabilities.data["executionTargetId"] == target_id
assert session_capabilities.data["basis"] == "target"
session_usage = session.usage()
assert session_usage.data["sessionId"] == session.id
assert isinstance(session_usage.data["items"], list)
archive_session = polaris.sessions.create(
    project_id=project.data["id"], title="Polaris Python archive conformance",
    provider="codex", model="conformance-model", execution_target_id=target_id,
    idempotency_key=f"python-conformance-archive-session-create-{uuid.uuid4()}",
)
archive_session_key = f"python-conformance-session-archive-{uuid.uuid4()}"
switch_model_key = f"python-conformance-session-model-switch-{uuid.uuid4()}"
switched_model = archive_session.switch_model(
    model="conformance-model", expected_model="conformance-model", idempotency_key=switch_model_key,
)
switched_model_replay = archive_session.switch_model(
    model="conformance-model", expected_model="conformance-model", idempotency_key=switch_model_key,
)
assert switched_model.data["model"] == "conformance-model"
assert switched_model_replay.idempotency_replayed
suspend_key = f"python-conformance-session-suspend-{uuid.uuid4()}"
suspended_session = archive_session.suspend(idempotency_key=suspend_key)
suspended_session_replay = archive_session.suspend(idempotency_key=suspend_key)
assert suspended_session.data["status"] == "suspended"
assert suspended_session_replay.idempotency_replayed
resume_key = f"python-conformance-session-resume-{uuid.uuid4()}"
resumed_session = archive_session.resume(idempotency_key=resume_key)
resumed_session_replay = archive_session.resume(idempotency_key=resume_key)
assert resumed_session.data["status"] == "active"
assert resumed_session_replay.idempotency_replayed
archived_session = archive_session.archive(idempotency_key=archive_session_key)
archived_session_replay = archive_session.archive(idempotency_key=archive_session_key)
assert archived_session.data["status"] == "archived"
assert archived_session_replay.idempotency_replayed
cancel_session = polaris.sessions.create(
    project_id=project.data["id"], title="Polaris Python cancel conformance",
    provider="codex", execution_target_id=target_id,
    idempotency_key=f"python-conformance-cancel-session-create-{uuid.uuid4()}",
)
cancel_turn = cancel_session.send_turn(
    input_text="Cancel this queued execution.",
    idempotency_key=f"python-conformance-cancel-turn-create-{uuid.uuid4()}",
)
cancel_events = cancel_session.list_events(after_sequence=0, limit=50)
cancellable_event = next(item for item in cancel_events.data["items"] if item["executionId"] is not None)
cancel_key = f"python-conformance-execution-cancel-{uuid.uuid4()}"
cancelled_execution = polaris.executions.cancel(
    execution_id=cancellable_event["executionId"], idempotency_key=cancel_key,
)
cancelled_execution_replay = polaris.executions.cancel(
    execution_id=cancellable_event["executionId"], idempotency_key=cancel_key,
)
assert cancelled_execution.data["status"] == "cancelled"
assert cancelled_execution_replay.idempotency_replayed
for forbidden in ["workerId", "workerManifestId", "providerRuntimeBindingId", "remoteWorkspaceId", "generation"]:
    assert forbidden not in cancelled_execution.data
rollback_boundary = cancel_session.list_events(after_sequence=0, limit=50)
rollback_key = f"python-conformance-session-rollback-{uuid.uuid4()}"
rollback = cancel_session.rollback(
    expected_last_event_sequence=rollback_boundary.data["lastSequence"],
    from_turn_id=cancel_turn.data["id"], idempotency_key=rollback_key,
)
rollback_replay = cancel_session.rollback(
    expected_last_event_sequence=rollback_boundary.data["lastSequence"],
    from_turn_id=cancel_turn.data["id"], idempotency_key=rollback_key,
)
assert rollback.data["removedTurnCount"] == 1
assert rollback_replay.idempotency_replayed
fork_key = f"python-conformance-session-fork-{uuid.uuid4()}"
forked = cancel_session.fork(
    expected_last_event_sequence=rollback.data["eventSequence"],
    title="Python conformance fork", visibility="organization", idempotency_key=fork_key,
)
forked_replay = cancel_session.fork(
    expected_last_event_sequence=rollback.data["eventSequence"],
    title="Python conformance fork", visibility="organization", idempotency_key=fork_key,
)
assert forked.data["session"]["id"] != cancel_session.id
assert forked_replay.idempotency_replayed
interrupt_session = polaris.sessions.create(
    project_id=project.data["id"], title="Polaris Python interrupt conformance",
    provider="codex", execution_target_id=target_id,
    idempotency_key=f"python-conformance-interrupt-session-create-{uuid.uuid4()}",
)
interrupt_session.send_turn(
    input_text="Interrupt this queued execution.",
    idempotency_key=f"python-conformance-interrupt-turn-create-{uuid.uuid4()}",
)
interrupt_key = f"python-conformance-turn-interrupt-{uuid.uuid4()}"
interrupt_command = interrupt_session.interrupt(idempotency_key=interrupt_key)
interrupt_command_replay = interrupt_session.interrupt(idempotency_key=interrupt_key)
assert interrupt_command.data["commandType"] == "InterruptTurn"
assert interrupt_command_replay.idempotency_replayed
for forbidden in ["payload", "deliveryWorkerId", "deliveryGeneration", "deliveryAttempts", "deliveryError"]:
    assert forbidden not in interrupt_command.data
session_page = polaris.sessions.list(project_id=project.data["id"], limit=50)
session_read = polaris.sessions.get(session_id=session.id)
assert any(item["id"] == session.id for item in session_page.data["items"])
assert session_read.id == session.id

turn = session.send_turn(
    input_text="Verify the Python SDK against the real Control Plane.",
    idempotency_key=f"python-conformance-turn-{uuid.uuid4()}",
)
page = session.list_events(after_sequence=0, limit=50)
assert len(page.data["items"]) >= 2

stream = session.events(reconnect=False)
observed = []
try:
    for event in stream:
        observed.append(event["eventType"])
        if event["eventType"] == "turn.created":
            break
finally:
    stream.close()
assert "session.created" in observed and "turn.created" in observed

held = None
for _ in range(40):
    try:
        held = urlopen(
            Request(
                f"{base_url}/v1/sessions/{session.id}/events/stream?afterSequence=0",
                headers={"Authorization": f"Bearer {api_key}", "Accept": "text/event-stream"},
            ),
            timeout=5,
        )
        break
    except HTTPError as error:
        if error.code != 429:
            raise
        error.close()
        time.sleep(0.05)
assert held is not None, "Could not reserve the SSE connection pool after the prior stream closed."
fallback = session.events(reconnect=False, polling_interval=0)
fallback_types = []
try:
    for event in fallback:
        fallback_types.append(event["eventType"])
        if event["eventType"] == "turn.created":
            break
finally:
    fallback.close()
    held.close()
assert "turn.created" in fallback_types

approval_key = f"python-conformance-approval-{uuid.uuid4()}"
approval_session = polaris.sessions.get(session_id=approval_session_id)
artifact_page = polaris.artifacts.list(session_id=approval_session_id, limit=50)
artifact_read = polaris.artifacts.get(artifact_id=artifact_id)
artifact_download = polaris.artifacts.download(artifact_id=artifact_id)
assert any(item["id"] == artifact_id for item in artifact_page.data["items"])
assert artifact_read.data["id"] == artifact_id
assert artifact_download.data["artifact"]["id"] == artifact_id
assert artifact_download.data["url"]
assert "objectKey" not in json.dumps(artifact_read.data)
artifact_payload = b"Polaris Artifact conformance\n"
artifact_digest = hashlib.sha256(artifact_payload).hexdigest()
artifact_create_key = f"python-conformance-artifact-{uuid.uuid4()}"
artifact_created = polaris.artifacts.create(
    session_id=approval_session_id, kind="generated_file", original_name="polaris-conformance.txt",
    idempotency_key=artifact_create_key,
)
artifact_create_replay = polaris.artifacts.create(
    session_id=approval_session_id, kind="generated_file", original_name="polaris-conformance.txt",
    idempotency_key=artifact_create_key,
)
assert artifact_create_replay.data["artifact"]["id"] == artifact_created.data["artifact"]["id"]
assert artifact_create_replay.idempotency_replayed and artifact_create_replay.data["uploadRequired"]
try:
    stale_upload = urlopen(Request(
        urljoin(base_url + "/", artifact_created.data["url"]), data=artifact_payload, method="PUT",
        headers={**artifact_created.data.get("headers", {}), "Content-Type": "text/plain"},
    ), timeout=5)
    stale_upload.close()
    raise AssertionError("Artifact replay did not revoke the previous local upload token")
except HTTPError as error:
    assert error.code == 401
upload = urlopen(Request(
    urljoin(base_url + "/", artifact_create_replay.data["url"]), data=artifact_payload, method="PUT",
    headers={**artifact_create_replay.data.get("headers", {}), "Content-Type": "text/plain"},
), timeout=5)
upload.close()
completed_artifact = polaris.artifacts.complete(
    artifact_id=artifact_created.data["artifact"]["id"], size_bytes=len(artifact_payload),
    sha256=artifact_digest, content_type="text/plain", idempotency_key=artifact_create_key + ":complete",
)
completed_artifact_replay = polaris.artifacts.complete(
    artifact_id=artifact_created.data["artifact"]["id"], size_bytes=len(artifact_payload),
    sha256=artifact_digest, content_type="text/plain", idempotency_key=artifact_create_key + ":complete",
)
assert completed_artifact.data["status"] == "ready"
assert completed_artifact_replay.data["id"] == completed_artifact.data["id"]
polaris.artifacts.delete(
    artifact_id=completed_artifact.data["id"], idempotency_key=artifact_create_key + ":delete",
)
polaris.artifacts.delete(
    artifact_id=completed_artifact.data["id"], idempotency_key=artifact_create_key + ":delete",
)
pending_interactions = approval_session.pending_interactions(limit=50)
interaction_history = polaris.interactions.list(execution_id=approval_execution_id, limit=50)
assert any(item["requestId"] == approval_request_id for item in pending_interactions.data["items"])
assert any(item["requestId"] == approval_request_id for item in interaction_history.data["items"])
assert "deliveryStatus" not in json.dumps(interaction_history.data)
approval = polaris.approvals.resolve(
    approval_execution_id,
    approval_request_id,
    "accept",
    idempotency_key=approval_key,
)
approval_replay = polaris.approvals.resolve(
    approval_execution_id,
    approval_request_id,
    "accept",
    idempotency_key=approval_key,
)
assert approval.data["status"] == "resolved"
assert approval_replay.idempotency_replayed
user_input_key = f"python-conformance-user-input-{uuid.uuid4()}"
user_input = polaris.interactions.resolve_user_input(
    approval_execution_id, user_input_request_id, {"environment": "staging"},
    idempotency_key=user_input_key,
)
user_input_replay = polaris.interactions.resolve_user_input(
    approval_execution_id, user_input_request_id, {"environment": "staging"},
    idempotency_key=user_input_key,
)
assert user_input.data["status"] == "resolved"
assert user_input_replay.idempotency_replayed

target_key = f"python-conformance-target-{uuid.uuid4()}"
target_name = f"Python SDK conformance {uuid.uuid4()}"
target = target_polaris.targets.create(
    tenant_id=tenant_id,
    organization_id=organization_id,
    kind="ssh",
    name=target_name,
    configuration={},
    idempotency_key=target_key,
)
target_replay = target_polaris.targets.create(
    tenant_id=tenant_id,
    organization_id=organization_id,
    kind="ssh",
    name=target_name,
    configuration={},
    idempotency_key=target_key,
)
target_read = target_polaris.targets.get(tenant_id=tenant_id, execution_target_id=target.data["id"])
assert target_replay.idempotency_replayed
assert target_read.data["id"] == target.data["id"]
assert "configuration" not in target_read.data

provisioning = target_polaris.targets.provision(
    tenant_id=tenant_id,
    execution_target_id=target.data["id"],
    action="install",
    idempotency_key=f"python-conformance-provision-{uuid.uuid4()}",
)
provisioned = target_polaris.targets.wait_for_provisioning(
    tenant_id=tenant_id,
    execution_target_id=target.data["id"],
    operation_id=provisioning.data["id"],
    poll_interval=0.01,
    timeout=5,
)
assert provisioned.data["state"] == "succeeded"
assert "configuration" not in json.dumps(provisioned.data)

print(json.dumps({
    "sessionId": session.id,
    "projectId": project.data["id"],
    "archivedProjectId": archive_candidate.data["id"],
    "archivedSessionId": archived_session.data["id"],
    "cancelledExecutionId": cancelled_execution.data["id"],
    "rollbackEventId": rollback.data["eventId"],
    "forkedSessionId": forked.data["session"]["id"],
    "interruptedExecutionId": interrupt_command.data["executionId"],
    "turnId": turn.data["id"],
    "eventTypes": observed,
    "approvalId": approval.data["id"],
    "targetId": target.data["id"],
    "provisioningOperationId": provisioning.data["id"],
    "createdArtifactId": completed_artifact.data["id"],
}))
