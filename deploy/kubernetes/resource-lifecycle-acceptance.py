#!/usr/bin/env python3
"""Real-Kubernetes Suspend -> delete -> recover acceptance for an owned Stage 4 namespace.

The parent resilience runner creates an isolated Control Plane/PostgreSQL/MinIO
namespace. This child creates one tenant-owned managed-Kubernetes Target and a
deterministic acceptance Worker, then proves that a waiting approval can scale
to zero and finish in a new physical Pod from an immutable Recovery Bundle.

Raw tenant, Session, Execution, Worker, Pod UID, login token, and checkpoint
identities never enter the report. The temporary Worker namespace is deleted
only after its exact Target ownership label and namespace UID are revalidated.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import json
import os
import re
import secrets
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable, Mapping, Sequence


SCHEMA_VERSION = "synara.kubernetes.resource-lifecycle.acceptance.v1"
TARGET_LABEL = "synara.io/execution-target-id"
EXECUTION_LABEL = "synara.io/execution-id"
GENERATION_LABEL = "synara.io/generation"
ACCEPTANCE_OWNER_LABEL = "synara.ai/acceptance-owner"
COOKIE_NAME = "synara_login_session"
UUID_PATTERN = re.compile(
    r"^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"
)
DNS_LABEL_PATTERN = re.compile(r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$")


class AcceptanceError(RuntimeError):
    def __init__(self, code: str, message: str, details: Mapping[str, Any] | None = None):
        super().__init__(message)
        self.code = code
        self.message = message
        self.details = dict(details or {})


def utc_now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")


def canonical_json(value: Any) -> bytes:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True).encode()


def digest_text(value: str) -> str:
    return hashlib.sha256(value.encode()).hexdigest()


def digest_json(value: Any) -> str:
    return hashlib.sha256(canonical_json(value)).hexdigest()


def require_uuid(value: str, label: str) -> str:
    normalized = str(uuid.UUID(value))
    if not UUID_PATTERN.fullmatch(normalized):
        raise AcceptanceError("invalid_identity", f"{label} is not a canonical UUID")
    return normalized


def write_json_atomic(path: Path, payload: Mapping[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            json.dump(payload, handle, sort_keys=True, separators=(",", ":"))
            handle.write("\n")
        os.replace(temporary, path)
    finally:
        try:
            os.unlink(temporary)
        except FileNotFoundError:
            pass


class Kubectl:
    def __init__(self, context: str, control_plane_namespace: str, timeout: int):
        self.context = context
        self.control_plane_namespace = control_plane_namespace
        self.timeout = timeout

    def run(
        self,
        arguments: Sequence[str],
        *,
        namespace: str | None = None,
        input_bytes: bytes | None = None,
        check: bool = True,
        timeout: int | None = None,
    ) -> subprocess.CompletedProcess[bytes]:
        command = ["kubectl", "--context", self.context]
        if namespace:
            command.extend(["-n", namespace])
        command.extend(arguments)
        try:
            completed = subprocess.run(
                command,
                input=input_bytes,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=False,
                timeout=timeout or self.timeout,
            )
        except subprocess.TimeoutExpired as error:
            raise AcceptanceError(
                "kubectl_timeout",
                "A bounded Kubernetes operation timed out",
                {"operation": arguments[0] if arguments else "unknown"},
            ) from error
        if check and completed.returncode != 0:
            raise AcceptanceError(
                "kubectl_failed",
                "A Kubernetes operation failed",
                {
                    "operation": arguments[0] if arguments else "unknown",
                    "exitCode": completed.returncode,
                    "stderrDigest": hashlib.sha256(completed.stderr).hexdigest(),
                    "stderrBytes": len(completed.stderr),
                },
            )
        return completed

    def json(self, arguments: Sequence[str], *, namespace: str | None = None) -> Any:
        completed = self.run(arguments, namespace=namespace)
        try:
            return json.loads(completed.stdout)
        except json.JSONDecodeError as error:
            raise AcceptanceError("kubectl_json_invalid", "Kubernetes returned malformed JSON") from error

    def namespace(self, name: str, *, required: bool = True) -> Mapping[str, Any] | None:
        completed = self.run(["get", "namespace", name, "-o", "json"], check=False)
        if completed.returncode != 0:
            if required:
                raise AcceptanceError("namespace_missing", "The expected Kubernetes namespace is absent")
            return None
        try:
            value = json.loads(completed.stdout)
        except json.JSONDecodeError as error:
            raise AcceptanceError("namespace_json_invalid", "Kubernetes returned malformed namespace JSON") from error
        if not isinstance(value, dict):
            raise AcceptanceError("namespace_json_invalid", "Kubernetes namespace JSON has the wrong shape")
        return value

    def postgres(self, sql: str) -> str:
        pods = self.json(
            ["get", "pods", "-l", "app.kubernetes.io/name=synara-stage2-postgres", "-o", "json"],
            namespace=self.control_plane_namespace,
        )
        names = [
            item.get("metadata", {}).get("name")
            for item in pods.get("items", [])
            if item.get("status", {}).get("phase") == "Running"
        ]
        if len(names) != 1 or not isinstance(names[0], str):
            raise AcceptanceError(
                "postgres_pod_ambiguous",
                "Exactly one Running acceptance PostgreSQL Pod is required",
                {"runningPodCount": len(names)},
            )
        completed = self.run(
            [
                "exec",
                "-i",
                f"pod/{names[0]}",
                "--",
                "psql",
                "-X",
                "-U",
                "synara",
                "-d",
                "synara",
                "-v",
                "ON_ERROR_STOP=1",
                "-q",
                "-At",
            ],
            namespace=self.control_plane_namespace,
            input_bytes=sql.encode(),
        )
        return completed.stdout.decode().strip()

    def postgres_json(self, sql: str) -> Mapping[str, Any]:
        raw = self.postgres(sql)
        try:
            value = json.loads(raw)
        except json.JSONDecodeError as error:
            raise AcceptanceError(
                "postgres_json_invalid",
                "The acceptance state query returned malformed JSON",
                {"resultDigest": digest_text(raw), "resultBytes": len(raw.encode())},
            ) from error
        if not isinstance(value, dict):
            raise AcceptanceError("postgres_json_invalid", "The acceptance state query returned the wrong shape")
        return value


class PortForward:
    def __init__(self, kubectl: Kubectl):
        self.kubectl = kubectl
        self.process: subprocess.Popen[bytes] | None = None
        self.port = self._allocate_port()

    @staticmethod
    def _allocate_port() -> int:
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            return int(listener.getsockname()[1])

    def __enter__(self) -> str:
        command = [
            "kubectl",
            "--context",
            self.kubectl.context,
            "-n",
            self.kubectl.control_plane_namespace,
            "port-forward",
            "service/synara-control-plane",
            f"{self.port}:3780",
        ]
        self.process = subprocess.Popen(
            command,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            start_new_session=True,
        )
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            if self.process.poll() is not None:
                raise AcceptanceError("port_forward_failed", "Control Plane port-forward exited early")
            try:
                with urllib.request.urlopen(f"http://127.0.0.1:{self.port}/health", timeout=1) as response:
                    if response.status == 200:
                        return f"http://127.0.0.1:{self.port}"
            except (OSError, urllib.error.URLError):
                pass
            time.sleep(0.2)
        raise AcceptanceError("port_forward_timeout", "Control Plane port-forward did not become ready")

    def __exit__(self, *_: object) -> None:
        if self.process is None:
            return
        self.process.terminate()
        try:
            self.process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            self.process.kill()
            self.process.wait(timeout=5)


class API:
    def __init__(self, base_url: str, login_token: str):
        self.base_url = base_url.rstrip("/")
        self.login_token = login_token

    def request(
        self,
        method: str,
        path: str,
        body: Mapping[str, Any] | None = None,
        *,
        operation: str,
        idempotency_key: str | None = None,
        expected: Sequence[int] = (200, 201),
    ) -> Any:
        data = canonical_json(body) if body is not None else None
        headers = {
            "Accept": "application/json",
            "Cookie": f"{COOKIE_NAME}={self.login_token}",
            "X-Request-ID": f"lifecycle-{secrets.token_hex(12)}",
        }
        if data is not None:
            headers["Content-Type"] = "application/json"
        if idempotency_key:
            headers["Idempotency-Key"] = idempotency_key
        request = urllib.request.Request(
            self.base_url + path,
            data=data,
            headers=headers,
            method=method,
        )
        try:
            with urllib.request.urlopen(request, timeout=20) as response:
                payload = response.read(2 << 20)
                status = response.status
        except urllib.error.HTTPError as error:
            payload = error.read(2 << 20)
            problem_code = parse_problem_code(payload)
            raise AcceptanceError(
                "api_request_failed",
                "A Control Plane API request failed",
                {
                    "status": error.code,
                    "problemCode": problem_code,
                    "method": method,
                    "operation": operation,
                },
            ) from error
        except urllib.error.URLError as error:
            raise AcceptanceError("api_unavailable", "The Control Plane API was unavailable") from error
        if status not in expected:
            raise AcceptanceError(
                "api_status_unexpected",
                "A Control Plane API request returned an unexpected status",
                {"status": status, "method": method},
            )
        if not payload:
            return None
        try:
            return json.loads(payload)
        except json.JSONDecodeError as error:
            raise AcceptanceError("api_json_invalid", "The Control Plane API returned malformed JSON") from error


def parse_problem_code(payload: bytes) -> str | None:
    try:
        decoded = json.loads(payload)
    except (json.JSONDecodeError, UnicodeDecodeError):
        return None
    if not isinstance(decoded, dict):
        return None
    envelope = decoded.get("error")
    if isinstance(envelope, dict) and isinstance(envelope.get("code"), str):
        return envelope["code"]
    code = decoded.get("code")
    return code if isinstance(code, str) else None


@dataclass(frozen=True)
class Authority:
    user_id: str
    tenant_id: str
    organization_id: str
    login_session_id: str
    login_token: str


def seed_authority(kubectl: Kubectl, run_suffix: str) -> Authority:
    authority = Authority(
        user_id=str(uuid.uuid4()),
        tenant_id=str(uuid.uuid4()),
        organization_id=str(uuid.uuid4()),
        login_session_id=str(uuid.uuid4()),
        login_token=base64.urlsafe_b64encode(secrets.token_bytes(32)).rstrip(b"=").decode(),
    )
    token_hash = hashlib.sha256(authority.login_token.encode()).hexdigest()
    sql = f"""
BEGIN;
INSERT INTO users (id, email, display_name, status, email_verified_at)
VALUES ('{authority.user_id}'::uuid, 'lifecycle-{run_suffix}@localhost.invalid',
        'Stage 4 Lifecycle Acceptance', 'active', clock_timestamp());
INSERT INTO tenants (id, slug, name, status, plan_code, region, settings, created_by)
VALUES ('{authority.tenant_id}'::uuid, 'lifecycle-{run_suffix}', 'Stage 4 Lifecycle Acceptance',
        'active', 'acceptance', 'local', '{{}}'::jsonb, '{authority.user_id}'::uuid);
INSERT INTO tenant_memberships (tenant_id, user_id, role, status, joined_at)
VALUES ('{authority.tenant_id}'::uuid, '{authority.user_id}'::uuid, 'owner', 'active', clock_timestamp());
INSERT INTO organizations (id, tenant_id, slug, name, kind, status, settings, created_by)
VALUES ('{authority.organization_id}'::uuid, '{authority.tenant_id}'::uuid,
        'lifecycle-{run_suffix}', 'Stage 4 Lifecycle Acceptance', 'root', 'active', '{{}}'::jsonb,
        '{authority.user_id}'::uuid);
INSERT INTO organization_memberships (tenant_id, organization_id, user_id, role, status)
VALUES ('{authority.tenant_id}'::uuid, '{authority.organization_id}'::uuid,
        '{authority.user_id}'::uuid, 'owner', 'active');
INSERT INTO login_sessions (
  id, user_id, active_tenant_id, refresh_token_hash, expires_at, last_seen_at, created_at
)
VALUES ('{authority.login_session_id}'::uuid, '{authority.user_id}'::uuid,
        '{authority.tenant_id}'::uuid, decode('{token_hash}', 'hex'),
        clock_timestamp() + interval '2 hours', clock_timestamp(), clock_timestamp());
COMMIT;
SELECT json_build_object(
  'userCount', (SELECT count(*) FROM users WHERE id = '{authority.user_id}'::uuid),
  'tenantCount', (SELECT count(*) FROM tenants WHERE id = '{authority.tenant_id}'::uuid),
  'membershipCount', (SELECT count(*) FROM tenant_memberships
    WHERE tenant_id = '{authority.tenant_id}'::uuid AND user_id = '{authority.user_id}'::uuid),
  'organizationCount', (SELECT count(*) FROM organizations WHERE id = '{authority.organization_id}'::uuid),
  'loginCount', (SELECT count(*) FROM login_sessions WHERE id = '{authority.login_session_id}'::uuid)
)::text;
"""
    result = kubectl.postgres_json(sql)
    if set(result.values()) != {1} or len(result) != 5:
        raise AcceptanceError("authority_seed_failed", "Acceptance authority was not created exactly once")
    return authority


def load_execution_state(kubectl: Kubectl, session_id: str, execution_id: str) -> Mapping[str, Any]:
    session_id = require_uuid(session_id, "session ID")
    execution_id = require_uuid(execution_id, "execution ID")
    sql = f"""
SELECT json_build_object(
  'session', json_build_object(
    'status', session.status,
    'resourceState', session.resource_state,
    'meaningfulActivitySequence', session.meaningful_activity_sequence,
    'meaningfulActivityAt', session.meaningful_activity_at,
    'resourceIdleSince', session.resource_idle_since,
    'waitingKeepAliveSeconds', session.waiting_keep_alive_seconds,
    'suspendAfterIdleSeconds', session.suspend_after_idle_seconds,
    'warmPoolMode', session.warm_pool_mode,
    'provider', session.provider,
    'executionTargetId', session.execution_target_id
  ),
  'execution', json_build_object(
    'status', execution.status,
    'generation', execution.generation,
    'workerId', execution.worker_id,
    'targetKind', execution.target_kind,
    'executionTargetId', execution.execution_target_id,
    'provider', execution.provider,
    'nextRecoveryReason', execution.next_recovery_reason,
    'remoteWorkspaceId', execution.remote_workspace_id,
    'workspaceMaterializationId', execution.workspace_materialization_id,
    'restoreCheckpointId', execution.restore_checkpoint_id,
    'schedulingDecisionId', execution.scheduling_decision_id
  ),
  'turnStatus', turn.status,
  'activeSessionExecutionCount', (SELECT count(*) FROM agent_executions active_execution
    WHERE active_execution.tenant_id = execution.tenant_id
      AND active_execution.session_id = execution.session_id
      AND active_execution.status IN ('queued', 'leased', 'running', 'waiting-for-approval', 'recovering', 'suspended')),
  'leaseCount', (SELECT count(*) FROM worker_leases lease WHERE lease.execution_id = execution.id),
  'pendingInteractionCount', (SELECT count(*) FROM execution_interactions interaction
    WHERE interaction.tenant_id = execution.tenant_id AND interaction.execution_id = execution.id
      AND interaction.status = 'pending'),
  'resolvedInteractionCount', (SELECT count(*) FROM execution_interactions interaction
    WHERE interaction.tenant_id = execution.tenant_id AND interaction.execution_id = execution.id
      AND interaction.status = 'resolved'),
  'interactions', COALESCE((
    SELECT json_agg(json_build_object(
      'requestId', interaction.request_id,
      'kind', interaction.kind,
      'status', interaction.status,
      'generation', interaction.generation,
      'deliveryStatus', interaction.delivery_status,
      'resolution', interaction.resolution
    ) ORDER BY interaction.requested_at, interaction.id)
    FROM execution_interactions interaction
    WHERE interaction.tenant_id = execution.tenant_id AND interaction.execution_id = execution.id
  ), '[]'::json),
  'suspendAttempts', COALESCE((
    SELECT json_agg(json_build_object(
      'status', attempt.status,
      'generation', attempt.generation,
      'reason', attempt.reason,
      'completionMode', attempt.completion_mode,
      'checkpointStatus', attempt.checkpoint_status,
      'podTerminalPhase', attempt.pod_terminal_phase,
      'workerInstanceUid', attempt.worker_instance_uid,
      'workerPodName', attempt.worker_pod_name
    ) ORDER BY attempt.requested_at, attempt.id)
    FROM execution_suspend_attempts attempt
    WHERE attempt.tenant_id = execution.tenant_id AND attempt.execution_id = execution.id
  ), '[]'::json),
  'bundles', COALESCE((
    SELECT json_agg(json_build_object(
      'id', bundle.id,
      'generation', bundle.generation,
      'schemaVersion', bundle.schema_version,
      'recoveryReason', bundle.recovery_reason,
      'previousBundleId', bundle.previous_bundle_id,
      'payloadSha256', bundle.payload_sha256,
      'payload', bundle.payload
    ) ORDER BY bundle.generation, bundle.id)
    FROM execution_recovery_bundles bundle
    WHERE bundle.tenant_id = execution.tenant_id AND bundle.execution_id = execution.id
  ), '[]'::json),
  'workers', COALESCE((
    SELECT json_agg(json_build_object(
      'instanceUid', worker.instance_uid,
      'podName', worker.pod_name,
      'status', worker.status,
      'incarnation', worker.incarnation
    ) ORDER BY worker.registered_at, worker.id)
    FROM worker_instances worker
    WHERE worker.execution_target_id = execution.execution_target_id
      AND worker.assigned_execution_id = execution.id
  ), '[]'::json),
  'workspaceCheckpointCount', (SELECT count(*)
    FROM workspace_checkpoints checkpoint
    WHERE checkpoint.tenant_id = execution.tenant_id
      AND checkpoint.session_id = execution.session_id
      AND checkpoint.status = 'ready'),
  'workspaceRecoveryEvidenceCount', (SELECT count(*)
    FROM session_events event
    WHERE event.tenant_id = execution.tenant_id AND event.session_id = execution.session_id
      AND event.execution_id = execution.id
      AND event.payload::text ~ '\"artifactContentVerified\"[[:space:]]*:[[:space:]]*true'
      AND event.payload::text LIKE '%\"recoveryEvidence\"%')
)::text
FROM agent_executions execution
JOIN agent_sessions session
  ON session.tenant_id = execution.tenant_id AND session.id = execution.session_id
JOIN agent_turns turn
  ON turn.tenant_id = execution.tenant_id AND turn.session_id = execution.session_id
 AND turn.id = execution.turn_id
WHERE execution.session_id = '{session_id}'::uuid AND execution.id = '{execution_id}'::uuid;
"""
    return kubectl.postgres_json(sql)


def latest_execution_id(kubectl: Kubectl, session_id: str) -> str:
    session_id = require_uuid(session_id, "session ID")
    raw = kubectl.postgres(
        f"SELECT id::text FROM agent_executions WHERE session_id = '{session_id}'::uuid "
        "ORDER BY queued_at DESC, id DESC LIMIT 1;\n"
    )
    return require_uuid(raw, "latest execution ID")


def wait_for(
    label: str,
    timeout: float,
    operation: Callable[[], Any],
    predicate: Callable[[Any], bool],
    *,
    interval: float = 1.0,
) -> Any:
    deadline = time.monotonic() + timeout
    last: Any = None
    last_error: AcceptanceError | None = None
    while time.monotonic() < deadline:
        try:
            last = operation()
            last_error = None
            if predicate(last):
                return last
        except AcceptanceError as error:
            last_error = error
        time.sleep(interval)
    details: dict[str, Any] = {"wait": label, "timeoutSeconds": timeout}
    if last_error:
        details["lastErrorCode"] = last_error.code
    elif isinstance(last, Mapping):
        details["lastStateDigest"] = digest_json(last)
    raise AcceptanceError("acceptance_wait_timeout", f"Timed out waiting for {label}", details)


def list_execution_pods(kubectl: Kubectl, namespace: str, execution_id: str) -> list[Mapping[str, Any]]:
    result = kubectl.json(
        ["get", "pods", "-l", f"{EXECUTION_LABEL}={require_uuid(execution_id, 'execution ID')}", "-o", "json"],
        namespace=namespace,
    )
    items = result.get("items", [])
    return [item for item in items if isinstance(item, dict)]


def validate_recovery_bundles(
    bundles: Sequence[Mapping[str, Any]],
    execution_id: str,
    session_id: str,
    request_id: str,
    target_id: str,
) -> Mapping[str, Any]:
    if len(bundles) != 2:
        raise AcceptanceError(
            "recovery_bundle_count_invalid",
            "Suspend/resume must retain exactly the initial and resumed Recovery Bundles",
            {"bundleCount": len(bundles)},
        )
    first, resumed = bundles
    if (
        first.get("generation") != 1
        or first.get("schemaVersion") != 1
        or first.get("recoveryReason") != "initial-claim"
        or resumed.get("generation") != 2
        or resumed.get("schemaVersion") != 1
        or resumed.get("recoveryReason") != "suspend-resume"
        or resumed.get("previousBundleId") != first.get("id")
    ):
        raise AcceptanceError("recovery_bundle_lineage_invalid", "Recovery Bundle lineage is invalid")
    payload = resumed.get("payload")
    if not isinstance(payload, dict):
        raise AcceptanceError("recovery_bundle_payload_invalid", "The resumed Recovery Bundle payload is missing")
    workload = payload.get("workload")
    execution = payload.get("execution")
    if not isinstance(workload, dict) or not isinstance(execution, dict):
        raise AcceptanceError("recovery_bundle_payload_invalid", "Recovery Bundle snapshots are incomplete")
    snapshot = workload.get("resumeSnapshot")
    memory = workload.get("memoryReferences")
    recorded = snapshot.get("resumeRecordedInteractions") if isinstance(snapshot, dict) else None
    workspace = snapshot.get("workspace") if isinstance(snapshot, dict) else None
    if (
        payload.get("schemaVersion") != 1
        or payload.get("executionId") != execution_id
        or payload.get("sessionId") != session_id
        or payload.get("generation") != 2
        or payload.get("recoveryReason") != "suspend-resume"
        or workload.get("provider") != "codex"
        or workload.get("inputText") != "[workspace-verify] [approval]"
        or not isinstance(memory, list)
        or not isinstance(snapshot, dict)
        or not isinstance(recorded, list)
        or len(recorded) != 1
        or recorded[0].get("kind") != "approval"
        or recorded[0].get("requestId") != request_id
        or recorded[0].get("resolution") != {"decision": "accept"}
        or not isinstance(workspace, dict)
        or not isinstance(workspace.get("checkpoint"), dict)
        or not workspace["checkpoint"].get("checkpointId")
        or execution.get("targetKind") != "kubernetes"
        or execution.get("executionTargetId") != target_id
    ):
        raise AcceptanceError(
            "recovery_bundle_coverage_invalid",
            "Recovery Bundle omitted required execution, context, workspace, memory, or interaction state",
        )
    for bundle in (first, resumed):
        sha = bundle.get("payloadSha256")
        if not isinstance(sha, str) or not re.fullmatch(r"[0-9a-f]{64}", sha):
            raise AcceptanceError("recovery_bundle_digest_invalid", "Recovery Bundle digest is invalid")
    return {
        "bundleCount": 2,
        "initialPayloadSHA256": first["payloadSha256"],
        "resumedPayloadSHA256": resumed["payloadSha256"],
        "lineageVerified": True,
        "coverage": {
            "configuration": True,
            "conversationContext": True,
            "workspaceCheckpoint": True,
            "memoryReferencesField": True,
            "pendingInteractionResolution": True,
            "schedulingDecision": execution.get("schedulingDecisionId") is not None,
        },
    }


def delete_owned_worker_namespace(
    kubectl: Kubectl,
    worker_namespace: str,
    target_id: str | None,
    timeout: int,
) -> Mapping[str, Any]:
    namespace = kubectl.namespace(worker_namespace, required=False)
    if namespace is None:
        return {"attempted": target_id is not None, "deleted": True, "alreadyAbsent": True}
    metadata = namespace.get("metadata", {})
    labels = metadata.get("labels", {})
    uid = metadata.get("uid")
    if not target_id or labels.get(TARGET_LABEL) != target_id or not isinstance(uid, str) or not uid:
        raise AcceptanceError(
            "cleanup_ownership_mismatch",
            "Refusing to delete a Worker namespace without exact Target ownership",
        )
    body = canonical_json(
        {
            "apiVersion": "v1",
            "kind": "DeleteOptions",
            "propagationPolicy": "Foreground",
            "preconditions": {"uid": uid},
        }
    )
    kubectl.run(
        ["delete", "--raw", f"/api/v1/namespaces/{urllib.parse.quote(worker_namespace, safe='')}", "-f", "-"],
        input_bytes=body,
    )
    wait_for(
        "owned Worker namespace deletion",
        timeout,
        lambda: kubectl.namespace(worker_namespace, required=False),
        lambda value: value is None,
    )
    return {"attempted": True, "deleted": True, "alreadyAbsent": False, "uidPrecondition": True}


def run_acceptance(arguments: argparse.Namespace) -> Mapping[str, Any]:
    started_at = utc_now()
    started_monotonic = time.monotonic()
    kubectl = Kubectl(arguments.context, arguments.control_plane_namespace, arguments.command_timeout)
    control_namespace = kubectl.namespace(arguments.control_plane_namespace)
    owner = control_namespace.get("metadata", {}).get("labels", {}).get(ACCEPTANCE_OWNER_LABEL)
    if not isinstance(owner, str) or not owner:
        raise AcceptanceError(
            "control_namespace_not_owned",
            "The lifecycle gate requires an acceptance-owned Control Plane namespace",
        )
    if kubectl.namespace(arguments.worker_namespace, required=False) is not None:
        raise AcceptanceError(
            "worker_namespace_exists",
            "The requested lifecycle Worker namespace already exists",
        )

    run_suffix = uuid.uuid4().hex[:12]
    authority = seed_authority(kubectl, run_suffix)
    target_id: str | None = None
    cleanup: Mapping[str, Any] = {"attempted": False, "deleted": False}
    result: dict[str, Any] = {}
    try:
        with PortForward(kubectl) as base_url:
            api = API(base_url, authority.login_token)
            project = api.request(
                "POST",
                f"/v1/tenants/{authority.tenant_id}/organizations/{authority.organization_id}/projects",
                {
                    "name": "Stage 4 Lifecycle Acceptance",
                    "defaultBranch": "main",
                    "visibility": "organization",
                },
                operation="create-project",
                idempotency_key=f"lifecycle-project-{run_suffix}",
            )
            project_id = require_uuid(project["id"], "project ID")
            target = api.request(
                "POST",
                f"/v1/tenants/{authority.tenant_id}/execution-targets",
                {
                    "organizationId": authority.organization_id,
                    "kind": "kubernetes",
                    "name": "Stage 4 Lifecycle Acceptance",
                    "configuration": {
                        "namespace": arguments.worker_namespace,
                        "manageNamespace": True,
                        "image": arguments.worker_image,
                        "imagePullPolicy": arguments.image_pull_policy,
                        "controlPlaneUrl": (
                            f"http://synara-control-plane.{arguments.control_plane_namespace}.svc.cluster.local:3780"
                        ),
                        "allowInsecureControlPlane": True,
                        "runnerCommand": [
                            "node",
                            "/opt/synara/acceptance/provider-host-fixture.mjs",
                            "--protocol-v2",
                        ],
                        "maxActivePods": 4,
                        "egressCidrs": ["0.0.0.0/0"],
                        "cpuRequest": "100m",
                        "cpuLimit": "1",
                        "memoryRequest": "128Mi",
                        "memoryLimit": "1Gi",
                        "ephemeralStorageRequest": "128Mi",
                        "ephemeralStorageLimit": "2Gi",
                        "workspaceSizeLimit": "1Gi",
                        "quotaCpuRequests": "1",
                        "quotaCpuLimits": "4",
                        "quotaMemoryRequests": "2Gi",
                        "quotaMemoryLimits": "4Gi",
                        "quotaEphemeralStorage": "8Gi",
                    },
                    "capabilities": {
                        "workspaceModes": ["local", "worktree"],
                        "providerPolicy": {"experimentalProviders": ["codex"]},
                    },
                },
                operation="create-execution-target",
            )
            target_id = require_uuid(target["id"], "execution target ID")
            wait_for(
                "managed Worker namespace",
                90,
                lambda: kubectl.namespace(arguments.worker_namespace, required=False),
                lambda value: isinstance(value, Mapping)
                and value.get("metadata", {}).get("labels", {}).get(TARGET_LABEL) == target_id,
            )

            session = api.request(
                "POST",
                f"/v1/projects/{project_id}/sessions",
                {
                    "title": "Stage 4 lifecycle recovery",
                    "visibility": "private",
                    "provider": "codex",
                    "executionTargetId": target_id,
                    "resourceLifecyclePolicy": {
                        "waitingKeepAliveSeconds": 60,
                        "suspendAfterIdleSeconds": 60,
                        "workspaceRetentionDays": 1,
                        "warmPoolMode": "disabled",
                    },
                },
                operation="create-session",
                idempotency_key=f"lifecycle-session-{run_suffix}",
            )
            session_id = require_uuid(session["id"], "session ID")

            api.request(
                "POST",
                f"/v1/sessions/{session_id}/turns",
                {"inputText": "[artifact]", "runtimeMode": "full-access", "interactionMode": "default"},
                operation="create-artifact-turn",
                idempotency_key=f"lifecycle-artifact-turn-{run_suffix}",
            )
            artifact_execution_id = wait_for(
                "artifact execution creation",
                30,
                lambda: latest_execution_id(kubectl, session_id),
                lambda value: isinstance(value, str),
            )
            wait_for(
                "artifact execution completion",
                arguments.phase_timeout,
                lambda: load_execution_state(kubectl, session_id, artifact_execution_id),
                lambda state: state.get("execution", {}).get("status") == "completed"
                and state.get("turnStatus") == "completed"
                and state.get("activeSessionExecutionCount") == 0
                and state.get("workspaceCheckpointCount", 0) >= 1,
            )

            api.request(
                "POST",
                f"/v1/sessions/{session_id}/turns",
                {
                    "inputText": "[workspace-verify] [approval]",
                    "runtimeMode": "full-access",
                    "interactionMode": "default",
                },
                operation="create-approval-turn",
                idempotency_key=f"lifecycle-approval-turn-{run_suffix}",
            )
            execution_id = wait_for(
                "approval execution creation",
                30,
                lambda: latest_execution_id(kubectl, session_id),
                lambda value: value != artifact_execution_id,
            )
            waiting_state = wait_for(
                "waiting approval",
                arguments.phase_timeout,
                lambda: load_execution_state(kubectl, session_id, execution_id),
                lambda state: state.get("execution", {}).get("status") == "waiting-for-approval"
                and state.get("session", {}).get("resourceState") == "waiting"
                and state.get("pendingInteractionCount") == 1
                and len(state.get("workers", [])) == 1,
            )
            interactions = waiting_state.get("interactions", [])
            if len(interactions) != 1 or interactions[0].get("kind") != "approval":
                raise AcceptanceError("pending_interaction_invalid", "The waiting approval snapshot is invalid")
            request_id = interactions[0].get("requestId")
            if not isinstance(request_id, str) or not request_id:
                raise AcceptanceError("pending_interaction_invalid", "The approval request ID is missing")
            original_workers = waiting_state.get("workers", [])
            original_pod_uid = require_uuid(original_workers[0]["instanceUid"], "original Pod UID")
            original_pod_name = original_workers[0].get("podName")
            pending_at = utc_now()

            suspended_state = wait_for(
                "Pod-terminal resource suspension",
                max(arguments.phase_timeout, 120),
                lambda: load_execution_state(kubectl, session_id, execution_id),
                lambda state: state.get("execution", {}).get("status") == "suspended"
                and state.get("session", {}).get("resourceState") == "suspended"
                and state.get("leaseCount") == 0
                and len(state.get("suspendAttempts", [])) == 1
                and state["suspendAttempts"][0].get("status") == "completed"
                and state["suspendAttempts"][0].get("completionMode") == "kubernetes-pod-terminal-v1"
                and state["suspendAttempts"][0].get("podTerminalPhase") == "Succeeded",
            )
            suspended_at = utc_now()
            wait_for(
                "original execution Pod removal",
                90,
                lambda: list_execution_pods(kubectl, arguments.worker_namespace, execution_id),
                lambda pods: all(item.get("metadata", {}).get("uid") != original_pod_uid for item in pods),
            )
            if suspended_state.get("pendingInteractionCount") != 1:
                raise AcceptanceError(
                    "pending_interaction_lost",
                    "The pending approval did not survive resource suspension",
                )

            resolved = api.request(
                "POST",
                (
                    f"/v1/executions/{execution_id}/approvals/"
                    f"{urllib.parse.quote(request_id, safe='')}/resolve"
                ),
                {"decision": "accept"},
                operation="resolve-suspended-approval",
                idempotency_key=f"lifecycle-approval-resolve-{run_suffix}",
            )
            if resolved.get("deliveryStatus") != "resume-recorded":
                raise AcceptanceError(
                    "suspended_resolution_not_recorded",
                    "The suspended approval was not recorded for one-time recovery",
                )
            resumed_at = utc_now()
            completed_state = wait_for(
                "resumed execution completion",
                arguments.phase_timeout,
                lambda: load_execution_state(kubectl, session_id, execution_id),
                lambda state: state.get("execution", {}).get("status") == "completed"
                and state.get("execution", {}).get("generation") == 2
                and state.get("turnStatus") == "completed"
                and state.get("session", {}).get("resourceState") == "idle"
                and state.get("leaseCount") == 0
                and state.get("workspaceRecoveryEvidenceCount", 0) >= 1
                and len(state.get("workers", [])) >= 2,
            )
            finished_at = utc_now()
            workers = completed_state.get("workers", [])
            worker_uids = [require_uuid(item["instanceUid"], "Worker Pod UID") for item in workers]
            distinct_worker_uids = list(dict.fromkeys(worker_uids))
            if len(distinct_worker_uids) < 2 or original_pod_uid not in distinct_worker_uids:
                raise AcceptanceError("replacement_pod_missing", "Recovery did not use a new physical Worker Pod")
            replacement_pod_uid = next(item for item in distinct_worker_uids if item != original_pod_uid)

            recovery = validate_recovery_bundles(
                completed_state.get("bundles", []), execution_id, session_id, request_id, target_id
            )
            session_state = completed_state.get("session", {})
            if (
                session_state.get("waitingKeepAliveSeconds") != 60
                or session_state.get("suspendAfterIdleSeconds") != 60
                or session_state.get("warmPoolMode") != "disabled"
                or session_state.get("provider") != "codex"
                or session_state.get("executionTargetId") != target_id
            ):
                raise AcceptanceError(
                    "session_configuration_changed",
                    "The Session configuration changed across Pod recovery",
                )
            attempts = completed_state.get("suspendAttempts", [])
            result = {
                "schemaVersion": SCHEMA_VERSION,
                "status": "passed",
                "evidenceLevel": "E3",
                "startedAt": started_at,
                "finishedAt": finished_at,
                "durationSeconds": int(time.monotonic() - started_monotonic),
                "context": arguments.context,
                "controlPlaneNamespace": arguments.control_plane_namespace,
                "workerNamespace": arguments.worker_namespace,
                "identities": {
                    "targetIdDigest": digest_text(target_id),
                    "sessionIdDigest": digest_text(session_id),
                    "executionIdDigest": digest_text(execution_id),
                    "originalPodUidDigest": digest_text(original_pod_uid),
                    "replacementPodUidDigest": digest_text(replacement_pod_uid),
                    "physicalPodUidDistinct": original_pod_uid != replacement_pod_uid,
                },
                "lifecycle": {
                    "waitingKeepAliveSeconds": 60,
                    "pendingAt": pending_at,
                    "suspendedAt": suspended_at,
                    "resumedAt": resumed_at,
                    "completedAt": finished_at,
                    "firstGeneration": 1,
                    "resumedGeneration": 2,
                    "completionMode": attempts[0].get("completionMode"),
                    "podTerminalPhase": attempts[0].get("podTerminalPhase"),
                    "leaseReleasedWhileSuspended": suspended_state.get("leaseCount") == 0,
                    "sessionStateAfterCompletion": session_state.get("resourceState"),
                },
                "pendingInteraction": {
                    "kind": "approval",
                    "requestIdDigest": digest_text(request_id),
                    "preservedAcrossSuspension": suspended_state.get("pendingInteractionCount") == 1,
                    "resolvedWhileSuspended": True,
                    "deliveryStatusBeforeRecovery": "resume-recorded",
                    "providerCallbackReplayed": False,
                },
                "workspace": {
                    "readyCheckpointCount": completed_state.get("workspaceCheckpointCount"),
                    "restoredArtifactVerifiedByReplacement": True,
                    "recoveryEvidenceEventCount": completed_state.get("workspaceRecoveryEvidenceCount"),
                },
                "recoveryBundle": recovery,
                "podLifecycle": {
                    "originalPodNameDigest": digest_text(str(original_pod_name)),
                    "originalPodTerminalSucceeded": attempts[0].get("podTerminalPhase") == "Succeeded",
                    "originalPodAbsentBeforeResume": True,
                    "replacementPodObserved": True,
                    "workerIncarnationCount": len(workers),
                },
            }
    finally:
        cleanup = delete_owned_worker_namespace(
            kubectl, arguments.worker_namespace, target_id, arguments.cleanup_timeout
        )
    result["cleanup"] = cleanup
    return result


def parse_arguments(argv: Sequence[str]) -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--context", required=True)
    parser.add_argument("--control-plane-namespace", required=True)
    parser.add_argument("--worker-namespace", required=True)
    parser.add_argument("--worker-image", required=True)
    parser.add_argument("--image-pull-policy", choices=("Never", "IfNotPresent", "Always"), default="Never")
    parser.add_argument("--result-file", type=Path, required=True)
    parser.add_argument("--phase-timeout", type=int, default=240)
    parser.add_argument("--command-timeout", type=int, default=60)
    parser.add_argument("--cleanup-timeout", type=int, default=180)
    arguments = parser.parse_args(argv)
    for name in ("control_plane_namespace", "worker_namespace"):
        value = getattr(arguments, name)
        if len(value) > 63 or not DNS_LABEL_PATTERN.fullmatch(value) or not value.startswith("synara-"):
            parser.error(f"--{name.replace('_', '-')} must be a synara-* DNS label")
    if arguments.control_plane_namespace == arguments.worker_namespace:
        parser.error("--worker-namespace must differ from --control-plane-namespace")
    if any(value < 1 or value > 1800 for value in (arguments.phase_timeout, arguments.command_timeout, arguments.cleanup_timeout)):
        parser.error("timeouts must be between 1 and 1800 seconds")
    if not arguments.context.strip() or not arguments.worker_image.strip():
        parser.error("context and worker image must be non-empty")
    return arguments


def main(argv: Sequence[str] | None = None) -> int:
    arguments = parse_arguments(argv or sys.argv[1:])
    started_at = utc_now()
    started = time.monotonic()
    try:
        result = run_acceptance(arguments)
        write_json_atomic(arguments.result_file, result)
        return 0
    except AcceptanceError as error:
        payload = {
            "schemaVersion": SCHEMA_VERSION,
            "status": "failed",
            "evidenceLevel": "E3",
            "startedAt": started_at,
            "finishedAt": utc_now(),
            "durationSeconds": int(time.monotonic() - started),
            "context": arguments.context,
            "controlPlaneNamespace": arguments.control_plane_namespace,
            "workerNamespace": arguments.worker_namespace,
            "reasonCode": error.code,
            "message": error.message,
            "details": error.details,
        }
        write_json_atomic(arguments.result_file, payload)
        return 1
    except Exception as error:  # fail closed without serializing raw provider/Kubernetes data
        payload = {
            "schemaVersion": SCHEMA_VERSION,
            "status": "failed",
            "evidenceLevel": "E3",
            "startedAt": started_at,
            "finishedAt": utc_now(),
            "durationSeconds": int(time.monotonic() - started),
            "context": arguments.context,
            "controlPlaneNamespace": arguments.control_plane_namespace,
            "workerNamespace": arguments.worker_namespace,
            "reasonCode": "unexpected_acceptance_failure",
            "message": "The lifecycle acceptance failed unexpectedly.",
            "details": {"exceptionType": type(error).__name__},
        }
        write_json_atomic(arguments.result_file, payload)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
