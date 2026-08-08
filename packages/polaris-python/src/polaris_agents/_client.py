from __future__ import annotations

import json
import time
import uuid
from collections.abc import Iterator
from typing import Any, Literal, cast
from urllib.parse import quote

from ._errors import PolarisError, PolarisSequenceGapError, PolarisTransportError
from ._generated import Artifact, ArtifactDownloadGrant, ArtifactPage, ArtifactUploadGrant, DeveloperControlCommand, DeveloperExecution, DeveloperQueuedSessionOperation, ExecutionTarget, ExecutionTargetProvisioningOperation, ForkSessionResult, Interaction, InteractionPage, PendingInteractionPage, Project, ProjectPage, ProviderCapabilityProjection, RollbackSessionResult, Session, SessionEvent, SessionEventPage, SessionPage, SessionUsage, Turn
from ._transport import PolarisTransport, ResponseMetadata, SDKResponse


class Polaris:
    def __init__(
        self,
        *,
        api_key: str,
        base_url: str,
        max_retries: int = 2,
        timeout: float = 30.0,
        _transport: PolarisTransport | None = None,
    ) -> None:
        transport = _transport or PolarisTransport(
            api_key=api_key,
            base_url=base_url,
            max_retries=max_retries,
            timeout=timeout,
        )
        self.artifacts = ArtifactsResource(transport)
        self.projects = ProjectsResource(transport)
        self.sessions = SessionsResource(transport)
        self.interactions = InteractionsResource(transport)
        self.executions = ExecutionsResource(transport)
        self.approvals = ApprovalsResource(transport)
        self.targets = TargetsResource(transport)


class ExecutionsResource:
    def __init__(self, transport: PolarisTransport) -> None:
        self._transport = transport

    def cancel(
        self, *, execution_id: str, idempotency_key: str | None = None,
    ) -> SDKResponse[DeveloperExecution]:
        response = self._transport.request_json(
            "POST", f"/v1/executions/{_segment(execution_id)}/cancel",
            idempotency_key=idempotency_key or str(uuid.uuid4()),
        )
        return SDKResponse(cast(DeveloperExecution, response.data), response.metadata)

    def resume_active_turn(
        self, *, execution_id: str, idempotency_key: str | None = None,
    ) -> SDKResponse[DeveloperExecution]:
        response = self._transport.request_json(
            "POST", f"/v1/executions/{_segment(execution_id)}/resume",
            idempotency_key=idempotency_key or str(uuid.uuid4()),
        )
        return SDKResponse(cast(DeveloperExecution, response.data), response.metadata)


class ArtifactsResource:
    def __init__(self, transport: PolarisTransport) -> None:
        self._transport = transport

    def create(
        self,
        *,
        session_id: str,
        kind: Literal["attachment", "generated_file", "terminal_log", "diff", "workspace_snapshot"],
        original_name: str | None = None,
        execution_id: str | None = None,
        expires_at: str | None = None,
        idempotency_key: str | None = None,
    ) -> SDKResponse[ArtifactUploadGrant]:
        body: dict[str, Any] = {"kind": kind}
        _optional(body, "originalName", original_name)
        _optional(body, "executionId", execution_id)
        _optional(body, "expiresAt", expires_at)
        response = self._transport.request_json(
            "POST", f"/v1/sessions/{_segment(session_id)}/artifacts", body=body,
            idempotency_key=idempotency_key or str(uuid.uuid4()),
        )
        return SDKResponse(cast(ArtifactUploadGrant, response.data), response.metadata)

    def list(
        self, *, session_id: str, limit: int = 50, cursor: str | None = None,
    ) -> SDKResponse[ArtifactPage]:
        query = _bounded_page_query(limit, cursor)
        response = self._transport.request_json(
            "GET", f"/v1/sessions/{_segment(session_id)}/artifacts", query=query,
        )
        return SDKResponse(cast(ArtifactPage, response.data), response.metadata)

    def get(self, *, artifact_id: str) -> SDKResponse[Artifact]:
        response = self._transport.request_json("GET", f"/v1/artifacts/{_segment(artifact_id)}")
        return SDKResponse(cast(Artifact, response.data), response.metadata)

    def download(self, *, artifact_id: str) -> SDKResponse[ArtifactDownloadGrant]:
        response = self._transport.request_json(
            "POST", f"/v1/artifacts/{_segment(artifact_id)}/download",
        )
        return SDKResponse(cast(ArtifactDownloadGrant, response.data), response.metadata)

    def complete(
        self,
        *,
        artifact_id: str,
        size_bytes: int,
        sha256: str,
        content_type: str,
        idempotency_key: str | None = None,
    ) -> SDKResponse[Artifact]:
        response = self._transport.request_json(
            "POST", f"/v1/artifacts/{_segment(artifact_id)}/complete",
            body={"sizeBytes": size_bytes, "sha256": sha256, "contentType": content_type},
            idempotency_key=idempotency_key or str(uuid.uuid4()),
        )
        return SDKResponse(cast(Artifact, response.data), response.metadata)

    def delete(self, *, artifact_id: str, idempotency_key: str | None = None) -> ResponseMetadata:
        response = self._transport.request_json(
            "DELETE", f"/v1/artifacts/{_segment(artifact_id)}",
            idempotency_key=idempotency_key or str(uuid.uuid4()),
        )
        return response.metadata


class InteractionsResource:
    def __init__(self, transport: PolarisTransport) -> None:
        self._transport = transport

    def list(
        self,
        *,
        execution_id: str,
        limit: int = 50,
        cursor: str | None = None,
    ) -> SDKResponse[InteractionPage]:
        query = _bounded_page_query(limit, cursor)
        response = self._transport.request_json(
            "GET", f"/v1/executions/{_segment(execution_id)}/interactions", query=query,
        )
        return SDKResponse(cast(InteractionPage, response.data), response.metadata)

    def resolve_user_input(
        self,
        execution_id: str,
        request_id: str,
        answers: dict[str, str | list[str]],
        *,
        idempotency_key: str | None = None,
    ) -> SDKResponse[Interaction]:
        if not answers:
            raise ValueError("answers must not be empty.")
        response = self._transport.request_json(
            "POST",
            f"/v1/executions/{_segment(execution_id)}/user-input/{_segment(request_id)}/resolve",
            body={"answers": answers},
            idempotency_key=idempotency_key or str(uuid.uuid4()),
        )
        return SDKResponse(cast(Interaction, response.data), response.metadata)


class ProjectsResource:
    def __init__(self, transport: PolarisTransport) -> None:
        self._transport = transport

    def list(
        self,
        *,
        tenant_id: str,
        organization_id: str,
        limit: int = 50,
        cursor: str | None = None,
    ) -> SDKResponse[ProjectPage]:
        _validate_limit(limit)
        if cursor is not None and not cursor.strip():
            raise ValueError("cursor must not be empty.")
        query: dict[str, Any] = {"limit": limit}
        _optional(query, "cursor", cursor)
        response = self._transport.request_json(
            "GET",
            self._path(tenant_id, organization_id),
            query=query,
        )
        return SDKResponse(cast(ProjectPage, response.data), response.metadata)

    def create(
        self,
        *,
        tenant_id: str,
        organization_id: str,
        name: str,
        repository_url: str | None = None,
        default_branch: str = "main",
        visibility: Literal["organization", "tenant"] = "organization",
        idempotency_key: str | None = None,
    ) -> SDKResponse[Project]:
        if visibility not in {"organization", "tenant"}:
            raise ValueError("visibility must be organization or tenant.")
        body: dict[str, Any] = {
            "name": name,
            "defaultBranch": default_branch,
            "visibility": visibility,
        }
        _optional(body, "repositoryUrl", repository_url)
        response = self._transport.request_json(
            "POST",
            self._path(tenant_id, organization_id),
            body=body,
            idempotency_key=idempotency_key or str(uuid.uuid4()),
        )
        return SDKResponse(cast(Project, response.data), response.metadata)

    def get(self, *, project_id: str) -> SDKResponse[Project]:
        response = self._transport.request_json("GET", f"/v1/projects/{_segment(project_id)}")
        return SDKResponse(cast(Project, response.data), response.metadata)

    def update(
        self,
        *,
        project_id: str,
        name: str | None = None,
        repository_url: str | None = None,
        default_branch: str | None = None,
        visibility: Literal["organization", "tenant"] | None = None,
        idempotency_key: str | None = None,
    ) -> SDKResponse[Project]:
        if visibility is not None and visibility not in {"organization", "tenant"}:
            raise ValueError("visibility must be organization or tenant.")
        body: dict[str, Any] = {}
        _optional(body, "name", name)
        _optional(body, "repositoryUrl", repository_url)
        _optional(body, "defaultBranch", default_branch)
        _optional(body, "visibility", visibility)
        if not body:
            raise ValueError("provide at least one project field to update.")
        response = self._transport.request_json(
            "PATCH", f"/v1/projects/{_segment(project_id)}", body=body,
            idempotency_key=idempotency_key or str(uuid.uuid4()),
        )
        return SDKResponse(cast(Project, response.data), response.metadata)

    def archive(self, *, project_id: str, idempotency_key: str | None = None) -> ResponseMetadata:
        response = self._transport.request_json(
            "DELETE", f"/v1/projects/{_segment(project_id)}",
            idempotency_key=idempotency_key or str(uuid.uuid4()),
        )
        return response.metadata

    def capabilities(
        self, *, project_id: str, execution_target_id: str | None = None,
    ) -> SDKResponse[ProviderCapabilityProjection]:
        query: dict[str, Any] = {}
        _optional(query, "executionTargetId", execution_target_id)
        response = self._transport.request_json(
            "GET", f"/v1/projects/{_segment(project_id)}/provider-capabilities", query=query,
        )
        return SDKResponse(cast(ProviderCapabilityProjection, response.data), response.metadata)

    @staticmethod
    def _path(tenant_id: str, organization_id: str) -> str:
        return f"/v1/tenants/{_segment(tenant_id)}/organizations/{_segment(organization_id)}/projects"


class SessionsResource:
    def __init__(self, transport: PolarisTransport) -> None:
        self._transport = transport

    def list(
        self,
        *,
        project_id: str,
        limit: int = 50,
        cursor: str | None = None,
    ) -> SDKResponse[SessionPage]:
        _validate_limit(limit)
        if cursor is not None and not cursor.strip():
            raise ValueError("cursor must not be empty.")
        query: dict[str, Any] = {"limit": limit}
        _optional(query, "cursor", cursor)
        response = self._transport.request_json(
            "GET", f"/v1/projects/{_segment(project_id)}/sessions", query=query,
        )
        return SDKResponse(cast(SessionPage, response.data), response.metadata)

    def get(self, *, session_id: str) -> SessionHandle:
        response = self._transport.request_json("GET", f"/v1/sessions/{_segment(session_id)}")
        return SessionHandle(self._transport, cast(Session, response.data), response.metadata)

    def create(
        self,
        *,
        project_id: str,
        title: str,
        provider: str,
        visibility: Literal["project", "organization"] = "project",
        model: str | None = None,
        execution_target_id: str | None = None,
        execution_target_group_id: str | None = None,
        preferred_execution_region: str | None = None,
        idempotency_key: str | None = None,
    ) -> SessionHandle:
        body: dict[str, Any] = {"title": title, "provider": provider, "visibility": visibility}
        _optional(body, "model", model)
        _optional(body, "executionTargetId", execution_target_id)
        _optional(body, "executionTargetGroupId", execution_target_group_id)
        _optional(body, "preferredExecutionRegion", preferred_execution_region)
        response = self._transport.request_json(
            "POST",
            f"/v1/projects/{_segment(project_id)}/sessions",
            body=body,
            idempotency_key=idempotency_key or str(uuid.uuid4()),
        )
        return SessionHandle(self._transport, cast(Session, response.data), response.metadata)


class TargetsResource:
    def __init__(self, transport: PolarisTransport) -> None:
        self._transport = transport

    def create(
        self,
        *,
        tenant_id: str,
        organization_id: str,
        kind: Literal["ssh", "docker", "kubernetes"],
        name: str,
        configuration: dict[str, Any],
        capabilities: dict[str, Any] | None = None,
        idempotency_key: str | None = None,
    ) -> SDKResponse[ExecutionTarget]:
        if kind not in {"ssh", "docker", "kubernetes"}:
            raise ValueError("kind must be ssh, docker, or kubernetes.")
        response = self._transport.request_json(
            "POST",
            f"/v1/tenants/{_segment(tenant_id)}/execution-targets",
            body={
                "organizationId": organization_id,
                "kind": kind,
                "name": name,
                "configuration": configuration,
                "capabilities": capabilities or {},
            },
            idempotency_key=idempotency_key or str(uuid.uuid4()),
        )
        return SDKResponse(cast(ExecutionTarget, response.data), response.metadata)

    def get(self, *, tenant_id: str, execution_target_id: str) -> SDKResponse[ExecutionTarget]:
        response = self._transport.request_json(
            "GET",
            f"/v1/tenants/{_segment(tenant_id)}/execution-targets/{_segment(execution_target_id)}",
        )
        return SDKResponse(cast(ExecutionTarget, response.data), response.metadata)

    def provision(
        self,
        *,
        tenant_id: str,
        execution_target_id: str,
        action: Literal["install", "upgrade", "revoke"],
        idempotency_key: str | None = None,
    ) -> SDKResponse[ExecutionTargetProvisioningOperation]:
        if action not in {"install", "upgrade", "revoke"}:
            raise ValueError("action must be install, upgrade, or revoke.")
        response = self._transport.request_json(
            "POST",
            self._provisioning_path(tenant_id, execution_target_id),
            body={"action": action},
            idempotency_key=idempotency_key or str(uuid.uuid4()),
        )
        return SDKResponse(cast(ExecutionTargetProvisioningOperation, response.data), response.metadata)

    def get_provisioning_operation(
        self, *, tenant_id: str, execution_target_id: str, operation_id: str,
    ) -> SDKResponse[ExecutionTargetProvisioningOperation]:
        response = self._transport.request_json(
            "GET",
            f"{self._provisioning_path(tenant_id, execution_target_id)}/{_segment(operation_id)}",
        )
        return SDKResponse(cast(ExecutionTargetProvisioningOperation, response.data), response.metadata)

    def wait_for_provisioning(
        self,
        *,
        tenant_id: str,
        execution_target_id: str,
        operation_id: str,
        poll_interval: float = 1.0,
        timeout: float = 600.0,
    ) -> SDKResponse[ExecutionTargetProvisioningOperation]:
        if poll_interval < 0 or poll_interval > 60:
            raise ValueError("poll_interval must be between 0 and 60 seconds.")
        if timeout <= 0 or timeout > 3600:
            raise ValueError("timeout must be greater than 0 and at most 3600 seconds.")
        deadline = time.monotonic() + timeout
        while True:
            response = self.get_provisioning_operation(
                tenant_id=tenant_id,
                execution_target_id=execution_target_id,
                operation_id=operation_id,
            )
            if response.data["state"] in {"succeeded", "failed"}:
                return response
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise PolarisTransportError(
                    f"Provisioning operation {operation_id} did not finish within {timeout} seconds."
                )
            time.sleep(min(poll_interval, remaining))

    @staticmethod
    def _provisioning_path(tenant_id: str, execution_target_id: str) -> str:
        return f"/v1/tenants/{_segment(tenant_id)}/execution-targets/{_segment(execution_target_id)}/provisioning-operations"


class SessionHandle:
    def __init__(
        self,
        transport: PolarisTransport,
        data: Session,
        response: ResponseMetadata,
    ) -> None:
        self._transport = transport
        self.data = data
        self.response = response
        self.id = data["id"]

    def send_turn(
        self,
        *,
        input_text: str,
        runtime_mode: str = "full-access",
        interaction_mode: str = "default",
        source_proposed_plan: dict[str, str] | None = None,
        idempotency_key: str | None = None,
    ) -> SDKResponse[Turn]:
        body: dict[str, Any] = {
            "inputText": input_text,
            "runtimeMode": runtime_mode,
            "interactionMode": interaction_mode,
        }
        _optional(body, "sourceProposedPlan", source_proposed_plan)
        response = self._transport.request_json(
            "POST",
            f"/v1/sessions/{_segment(self.id)}/turns",
            body=body,
            idempotency_key=idempotency_key or str(uuid.uuid4()),
        )
        return SDKResponse(cast(Turn, response.data), response.metadata)

    def archive(self, *, idempotency_key: str | None = None) -> SDKResponse[Session]:
        response = self._transport.request_json(
            "POST", f"/v1/sessions/{_segment(self.id)}/archive",
            idempotency_key=idempotency_key or str(uuid.uuid4()),
        )
        return SDKResponse(cast(Session, response.data), response.metadata)

    def capabilities(self) -> SDKResponse[ProviderCapabilityProjection]:
        response = self._transport.request_json(
            "GET", f"/v1/sessions/{_segment(self.id)}/provider-capabilities",
        )
        return SDKResponse(cast(ProviderCapabilityProjection, response.data), response.metadata)

    def switch_model(
        self, *, model: str, expected_model: str | None, idempotency_key: str | None = None,
    ) -> SDKResponse[Session]:
        response = self._transport.request_json(
            "POST", f"/v1/sessions/{_segment(self.id)}/model-switch",
            body={"model": model, "expectedModel": expected_model},
            idempotency_key=idempotency_key or str(uuid.uuid4()),
        )
        return SDKResponse(cast(Session, response.data), response.metadata)

    def suspend(self, *, idempotency_key: str | None = None) -> SDKResponse[Session]:
        response = self._transport.request_json(
            "POST", f"/v1/sessions/{_segment(self.id)}/suspend",
            idempotency_key=idempotency_key or str(uuid.uuid4()),
        )
        return SDKResponse(cast(Session, response.data), response.metadata)

    def resume(self, *, idempotency_key: str | None = None) -> SDKResponse[Session]:
        response = self._transport.request_json(
            "POST", f"/v1/sessions/{_segment(self.id)}/resume",
            idempotency_key=idempotency_key or str(uuid.uuid4()),
        )
        return SDKResponse(cast(Session, response.data), response.metadata)

    def usage(self) -> SDKResponse[SessionUsage]:
        response = self._transport.request_json(
            "GET", f"/v1/sessions/{_segment(self.id)}/usage",
        )
        return SDKResponse(cast(SessionUsage, response.data), response.metadata)

    def interrupt(self, *, idempotency_key: str | None = None) -> SDKResponse[DeveloperControlCommand]:
        response = self._transport.request_json(
            "POST", f"/v1/sessions/{_segment(self.id)}/turns/active/interrupt",
            idempotency_key=idempotency_key or str(uuid.uuid4()),
        )
        return SDKResponse(cast(DeveloperControlCommand, response.data), response.metadata)

    def resume_active_turn(self, *, idempotency_key: str | None = None) -> SDKResponse[DeveloperExecution]:
        response = self._transport.request_json(
            "POST", f"/v1/sessions/{_segment(self.id)}/turns/active/resume",
            idempotency_key=idempotency_key or str(uuid.uuid4()),
        )
        return SDKResponse(cast(DeveloperExecution, response.data), response.metadata)

    def steer(
        self, *, input_text: str, idempotency_key: str | None = None,
    ) -> SDKResponse[DeveloperControlCommand]:
        response = self._transport.request_json(
            "POST", f"/v1/sessions/{_segment(self.id)}/turns/active/steer",
            body={"inputText": input_text},
            idempotency_key=idempotency_key or str(uuid.uuid4()),
        )
        return SDKResponse(cast(DeveloperControlCommand, response.data), response.metadata)

    def compact(
        self, *, expected_last_event_sequence: int, idempotency_key: str | None = None,
    ) -> SDKResponse[DeveloperQueuedSessionOperation]:
        response = self._transport.request_json(
            "POST", f"/v1/sessions/{_segment(self.id)}/compact",
            body={"expectedLastEventSequence": expected_last_event_sequence},
            idempotency_key=idempotency_key or str(uuid.uuid4()),
        )
        return SDKResponse(cast(DeveloperQueuedSessionOperation, response.data), response.metadata)

    def start_review(
        self,
        *,
        expected_last_event_sequence: int,
        target_type: Literal["uncommittedChanges", "baseBranch"],
        branch: str | None = None,
        runtime_mode: Literal["approval-required", "full-access"] = "approval-required",
        idempotency_key: str | None = None,
    ) -> SDKResponse[DeveloperQueuedSessionOperation]:
        target: dict[str, Any] = {"type": target_type}
        _optional(target, "branch", branch)
        response = self._transport.request_json(
            "POST", f"/v1/sessions/{_segment(self.id)}/reviews",
            body={
                "expectedLastEventSequence": expected_last_event_sequence,
                "target": target,
                "runtimeMode": runtime_mode,
            },
            idempotency_key=idempotency_key or str(uuid.uuid4()),
        )
        return SDKResponse(cast(DeveloperQueuedSessionOperation, response.data), response.metadata)

    def rollback(
        self,
        *,
        expected_last_event_sequence: int,
        from_turn_id: str,
        idempotency_key: str | None = None,
    ) -> SDKResponse[RollbackSessionResult]:
        response = self._transport.request_json(
            "POST", f"/v1/sessions/{_segment(self.id)}/rollback",
            body={"expectedLastEventSequence": expected_last_event_sequence, "fromTurnId": from_turn_id},
            idempotency_key=idempotency_key or str(uuid.uuid4()),
        )
        return SDKResponse(cast(RollbackSessionResult, response.data), response.metadata)

    def fork(
        self,
        *,
        expected_last_event_sequence: int,
        title: str | None = None,
        visibility: Literal["project", "organization"] | None = None,
        provider_credential_id: str | None = None,
        execution_target_id: str | None = None,
        idempotency_key: str | None = None,
    ) -> SDKResponse[ForkSessionResult]:
        body: dict[str, Any] = {"expectedLastEventSequence": expected_last_event_sequence}
        _optional(body, "title", title)
        _optional(body, "visibility", visibility)
        _optional(body, "providerCredentialId", provider_credential_id)
        _optional(body, "executionTargetId", execution_target_id)
        response = self._transport.request_json(
            "POST", f"/v1/sessions/{_segment(self.id)}/fork", body=body,
            idempotency_key=idempotency_key or str(uuid.uuid4()),
        )
        return SDKResponse(cast(ForkSessionResult, response.data), response.metadata)

    def list_events(self, *, after_sequence: int = 0, limit: int = 50) -> SDKResponse[SessionEventPage]:
        _validate_sequence(after_sequence)
        _validate_limit(limit)
        response = self._transport.request_json(
            "GET",
            f"/v1/sessions/{_segment(self.id)}/events",
            query={"afterSequence": after_sequence, "limit": limit},
        )
        return SDKResponse(cast(SessionEventPage, response.data), response.metadata)

    def pending_interactions(
        self, *, limit: int = 50, cursor: str | None = None,
    ) -> SDKResponse[PendingInteractionPage]:
        query = _bounded_page_query(limit, cursor)
        response = self._transport.request_json(
            "GET", f"/v1/sessions/{_segment(self.id)}/interactions", query=query,
        )
        return SDKResponse(cast(PendingInteractionPage, response.data), response.metadata)

    def events(
        self,
        *,
        after_sequence: int = 0,
        reconnect: bool = True,
        reconnect_delay: float = 0.5,
        fallback_to_polling: bool = True,
        polling_interval: float = 1.0,
        polling_limit: int = 50,
    ) -> Iterator[SessionEvent]:
        _validate_sequence(after_sequence)
        _validate_limit(polling_limit)
        if reconnect_delay < 0 or reconnect_delay > 60 or polling_interval < 0 or polling_interval > 60:
            raise ValueError("Event reconnect and polling delays must be between 0 and 60 seconds.")
        cursor = after_sequence
        failures = 0
        while True:
            try:
                response = self._transport.open_event_stream(
                    f"/v1/sessions/{_segment(self.id)}/events/stream",
                    after_sequence=cursor,
                )
            except PolarisError as error:
                if fallback_to_polling and error.status == 429 and error.code in {
                    "sse_user_connection_limit",
                    "sse_tenant_connection_limit",
                }:
                    yield from self._poll_events(cursor, polling_interval, polling_limit)
                    return
                if error.status not in {429, 500, 502, 503, 504} or failures >= self._transport.max_retries:
                    raise
                failures += 1
                time.sleep(float(error.retry_after_seconds or reconnect_delay))
                continue
            except PolarisTransportError:
                if failures >= self._transport.max_retries:
                    raise
                failures += 1
                time.sleep(reconnect_delay)
                continue
            failures = 0
            try:
                for event in _decode_event_stream(response):
                    sequence = _event_sequence(event)
                    if sequence <= cursor:
                        continue
                    if sequence != cursor + 1:
                        raise PolarisSequenceGapError(cursor + 1, sequence)
                    cursor = sequence
                    yield event
            finally:
                response.close()
            if not reconnect:
                return
            time.sleep(reconnect_delay)

    def events_for_execution(
        self,
        execution_id: str,
        **options: Any,
    ) -> Iterator[SessionEvent]:
        if not execution_id.strip():
            raise ValueError("execution_id is required.")
        for event in self.events(**options):
            if event.get("executionId") == execution_id:
                yield event

    def _poll_events(self, cursor: int, interval: float, limit: int) -> Iterator[SessionEvent]:
        while True:
            page = self.list_events(after_sequence=cursor, limit=limit).data
            items = page["items"]
            for event in items:
                sequence = _event_sequence(event)
                if sequence <= cursor:
                    continue
                if sequence != cursor + 1:
                    raise PolarisSequenceGapError(cursor + 1, sequence)
                cursor = sequence
                yield event
            if len(items) < limit:
                time.sleep(interval)


class ApprovalsResource:
    def __init__(self, transport: PolarisTransport) -> None:
        self._transport = transport

    def resolve(
        self,
        execution_id: str,
        request_id: str,
        decision: Literal["accept", "decline"],
        *,
        idempotency_key: str | None = None,
    ) -> SDKResponse[Interaction]:
        if decision not in {"accept", "decline"}:
            raise ValueError("decision must be accept or decline.")
        response = self._transport.request_json(
            "POST",
            f"/v1/executions/{_segment(execution_id)}/approvals/{_segment(request_id)}/resolve",
            body={"decision": decision},
            idempotency_key=idempotency_key or str(uuid.uuid4()),
        )
        return SDKResponse(cast(Interaction, response.data), response.metadata)


def _decode_event_stream(response: Any) -> Iterator[SessionEvent]:
    event_type = "message"
    data: list[str] = []
    while True:
        raw = response.readline()
        if raw == b"" or raw == "":
            return
        line = raw.decode("utf-8") if isinstance(raw, bytes) else raw
        line = line.rstrip("\r\n")
        if line == "":
            if event_type == "session-event" and data:
                try:
                    value = json.loads("\n".join(data))
                except json.JSONDecodeError as error:
                    raise PolarisTransportError("Polaris Event stream contained invalid JSON.") from error
                if not isinstance(value, dict):
                    raise PolarisTransportError("Polaris Event stream contained an invalid Session Event.")
                yield cast(SessionEvent, value)
            event_type, data = "message", []
            continue
        if line.startswith(":"):
            continue
        field, separator, value = line.partition(":")
        if separator and value.startswith(" "):
            value = value[1:]
        if field == "event":
            event_type = value
        elif field == "data":
            data.append(value)


def _event_sequence(event: SessionEvent) -> int:
    sequence = event.get("sequence")
    if not isinstance(sequence, int) or isinstance(sequence, bool) or sequence < 1:
        raise PolarisTransportError("Polaris Event stream contained an invalid Session Event.")
    return sequence


def _segment(value: str) -> str:
    return quote(value, safe="")


def _optional(body: dict[str, Any], key: str, value: Any) -> None:
    if value is not None:
        body[key] = value


def _bounded_page_query(limit: int, cursor: str | None) -> dict[str, Any]:
    _validate_limit(limit)
    if cursor is not None and not cursor.strip():
        raise ValueError("cursor must not be empty.")
    query: dict[str, Any] = {"limit": limit}
    _optional(query, "cursor", cursor)
    return query


def _validate_sequence(value: int) -> None:
    if not isinstance(value, int) or isinstance(value, bool) or value < 0:
        raise ValueError("after_sequence must be a non-negative integer.")


def _validate_limit(value: int) -> None:
    if not isinstance(value, int) or isinstance(value, bool) or not 1 <= value <= 200:
        raise ValueError("limit must be an integer between 1 and 200.")
