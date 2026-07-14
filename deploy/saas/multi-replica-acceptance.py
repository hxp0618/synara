#!/usr/bin/env python3
"""In-network Stage 2 multi-replica acceptance client using only Python stdlib."""

from __future__ import annotations

import argparse
import concurrent.futures
import http.client
import http.cookies
import importlib.util
import json
import os
import shutil
import sys
import threading
import time
import uuid
from pathlib import Path
from typing import Any


LOGICAL_HOST = "synara-control-plane.test"
CONTROL_PLANE_PORT = 3780
WORKER_PROTOCOL_VERSION = 2
WORKER_VERSION = "acceptance"


class AcceptanceError(RuntimeError):
    pass


def require(condition: bool, message: str) -> None:
    if not condition:
        raise AcceptanceError(message)


def build_worker_capabilities(
    generator_path: Path,
    catalog_path: Path,
    target_capabilities: Any,
    worker_version: str,
) -> dict[str, Any]:
    require(isinstance(target_capabilities, dict), "Execution Target capabilities must be a JSON object")
    specification = importlib.util.spec_from_file_location("acceptance_worker_manifest", generator_path)
    require(
        specification is not None and specification.loader is not None,
        "Worker Manifest generator could not be loaded",
    )
    module = importlib.util.module_from_spec(specification)
    specification.loader.exec_module(module)
    catalog = json.loads(catalog_path.read_text())
    require(isinstance(catalog, dict), "Provider capability catalog must be a JSON object")
    capabilities = module.build_worker_capabilities(
        catalog,
        target_capabilities,
        worker_version=worker_version,
    )
    require(isinstance(capabilities, dict), "Worker Manifest generator did not return a JSON object")
    return capabilities


class CookieClient:
    def __init__(self, cookie_path: Path | None = None) -> None:
        self.cookie_path = cookie_path
        self.cookies: dict[str, str] = {}
        self.lock = threading.Lock()
        if cookie_path is not None and cookie_path.exists():
            self.cookies = json.loads(cookie_path.read_text())

    def request(
        self,
        address: str,
        method: str,
        path: str,
        body: dict[str, Any] | None = None,
        headers: dict[str, str] | None = None,
    ) -> tuple[int, bytes, http.client.HTTPMessage]:
        payload = None if body is None else json.dumps(body, separators=(",", ":")).encode()
        request_headers = {
            "Host": LOGICAL_HOST,
            "User-Agent": "synara-multi-replica-acceptance",
            "Accept": "application/json",
            "Connection": "close",
        }
        if payload is not None:
            request_headers["Content-Type"] = "application/json"
        with self.lock:
            if self.cookies:
                request_headers["Cookie"] = "; ".join(
                    f"{name}={value}" for name, value in sorted(self.cookies.items())
                )
        if headers:
            request_headers.update(headers)

        connection = http.client.HTTPConnection(address, CONTROL_PLANE_PORT, timeout=15)
        try:
            connection.request(method, path, body=payload, headers=request_headers)
            response = connection.getresponse()
            response_body = response.read()
            self._update_cookies(response.headers)
            return response.status, response_body, response.headers
        finally:
            connection.close()

    def json(
        self,
        address: str,
        method: str,
        path: str,
        body: dict[str, Any] | None = None,
        headers: dict[str, str] | None = None,
    ) -> dict[str, Any]:
        decoded, _ = self.json_with_headers(address, method, path, body, headers)
        return decoded

    def json_with_headers(
        self,
        address: str,
        method: str,
        path: str,
        body: dict[str, Any] | None = None,
        headers: dict[str, str] | None = None,
    ) -> tuple[dict[str, Any], http.client.HTTPMessage]:
        status, response_body, response_headers = self.request(address, method, path, body, headers)
        if not 200 <= status < 300:
            raise AcceptanceError(
                f"{method} {path} through {address} returned {status}: {response_body.decode(errors='replace')}"
            )
        if not response_body:
            return {}
        try:
            decoded = json.loads(response_body)
        except json.JSONDecodeError as error:
            raise AcceptanceError(f"{method} {path} returned invalid JSON: {error}") from error
        require(isinstance(decoded, dict), f"{method} {path} did not return a JSON object")
        return decoded, response_headers

    def cookie_header(self) -> str:
        with self.lock:
            return "; ".join(f"{name}={value}" for name, value in sorted(self.cookies.items()))

    def _update_cookies(self, headers: http.client.HTTPMessage) -> None:
        changed = False
        with self.lock:
            for raw_cookie in headers.get_all("Set-Cookie", []):
                parsed = http.cookies.SimpleCookie()
                parsed.load(raw_cookie)
                for name, morsel in parsed.items():
                    if morsel.value == "" or morsel["max-age"].startswith("-"):
                        changed = self.cookies.pop(name, None) is not None or changed
                    else:
                        self.cookies[name] = morsel.value
                        changed = True
            if changed and self.cookie_path is not None:
                self.cookie_path.write_text(json.dumps(self.cookies, sort_keys=True))


class SSECollector:
    def __init__(
        self,
        address: str,
        path: str,
        cookie_header: str,
        stop_after: set[int],
        headers: dict[str, str] | None = None,
    ) -> None:
        self.address = address
        self.path = path
        self.cookie_header = cookie_header
        self.stop_after = stop_after
        self.headers = headers or {}
        self.observed: set[int] = set()
        self.retry_seen = False
        self.error: BaseException | None = None
        self.ready = threading.Event()
        self.condition = threading.Condition()
        self.thread = threading.Thread(target=self._run, daemon=True)

    def start(self) -> None:
        self.thread.start()
        require(self.ready.wait(15), f"SSE connection to {self.address} did not become ready")
        if self.error is not None:
            raise AcceptanceError(f"SSE connection failed: {self.error}")

    def wait_for_retry(self) -> None:
        self._wait(lambda: self.retry_seen, "SSE retry directive was not received")

    def wait_for(self, event_ids: set[int]) -> None:
        self._wait(lambda: event_ids.issubset(self.observed), f"SSE events {sorted(event_ids)} were not received")

    def finish(self) -> None:
        self.thread.join(15)
        require(not self.thread.is_alive(), "SSE collector did not stop after receiving the expected events")
        if self.error is not None:
            raise AcceptanceError(f"SSE collection failed: {self.error}")

    def _wait(self, predicate: Any, message: str) -> None:
        deadline = time.monotonic() + 15
        with self.condition:
            while not predicate():
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise AcceptanceError(message)
                self.condition.wait(remaining)
                if self.error is not None:
                    raise AcceptanceError(f"SSE collection failed: {self.error}")

    def _run(self) -> None:
        connection = http.client.HTTPConnection(self.address, CONTROL_PLANE_PORT, timeout=20)
        request_headers = {
            "Host": LOGICAL_HOST,
            "Accept": "text/event-stream",
            "Cache-Control": "no-cache",
            "Connection": "close",
        }
        if self.cookie_header:
            request_headers["Cookie"] = self.cookie_header
        request_headers.update(self.headers)
        try:
            connection.request("GET", self.path, headers=request_headers)
            response = connection.getresponse()
            if response.status != 200:
                raise AcceptanceError(f"SSE returned HTTP {response.status}: {response.read().decode(errors='replace')}")
            self.ready.set()
            while True:
                raw_line = response.readline()
                if not raw_line:
                    raise AcceptanceError("SSE connection ended before expected events arrived")
                line = raw_line.decode(errors="replace").strip()
                with self.condition:
                    if line == "retry: 2000":
                        self.retry_seen = True
                    elif line.startswith("id: "):
                        self.observed.add(int(line.removeprefix("id: ")))
                    self.condition.notify_all()
                    if self.stop_after.issubset(self.observed):
                        return
        except BaseException as error:  # propagate network and assertion failures to the owner thread
            self.error = error
        finally:
            self.ready.set()
            with self.condition:
                self.condition.notify_all()
            connection.close()


def assert_ready(client: CookieClient, replica: str, expected_schema_version: int) -> None:
    ready = client.json(replica, "GET", "/ready")
    schema = ready.get("checks", {}).get("schema", {})
    require(ready.get("status") == "ready", f"{replica} did not report ready")
    require(schema.get("status") == "ready", f"{replica} schema was not ready")
    require(
        schema.get("expectedVersion") == expected_schema_version,
        f"{replica} expected schema version did not match the checked-out migrations",
    )
    require(
        schema.get("appliedVersion") == expected_schema_version,
        f"{replica} had unapplied migrations",
    )


def phase_one(
    replica_a: str,
    replica_b: str,
    registration_token: str,
    worker_manifest_generator: Path,
    provider_capability_catalog: Path,
    state_dir: Path,
    expected_schema_version: int,
) -> None:
    if state_dir.exists():
        shutil.rmtree(state_dir)
    state_dir.mkdir(parents=True)
    anonymous = CookieClient()
    assert_ready(anonymous, replica_a, expected_schema_version)
    assert_ready(anonymous, replica_b, expected_schema_version)

    run_id = f"{int(time.time())}-{os.getpid()}-{uuid.uuid4().hex[:8]}"
    revoke_client = CookieClient(state_dir / "revoke-cookie.json")
    revoke_client.json(
        replica_a,
        "POST",
        "/v1/auth/dev-login",
        {"email": f"revoke-{run_id}@example.com", "displayName": "Revoke Test"},
    )
    status, _, _ = revoke_client.request(replica_b, "POST", "/v1/auth/logout")
    require(status == 204, f"cross-replica logout returned {status}")
    status, _, _ = revoke_client.request(replica_a, "GET", "/v1/auth/session")
    require(status == 401, f"revoked login session returned {status} through the other replica")

    owner = CookieClient(state_dir / "owner-cookie.json")
    login = owner.json(
        replica_a,
        "POST",
        "/v1/auth/dev-login",
        {"email": f"multi-{run_id}@example.com", "displayName": "Multi Replica Owner"},
    )
    tenant_id = login["user"]["activeTenantId"]
    organization = owner.json(
        replica_a,
        "POST",
        f"/v1/tenants/{tenant_id}/organizations",
        {"slug": f"multi-{run_id}", "name": "Multi Replica", "kind": "department", "settings": {}},
    )
    project_body = {"name": "Multi Replica Project", "defaultBranch": "main", "visibility": "organization"}
    project_key = f"project-{run_id}"
    project, _ = owner.json_with_headers(
        replica_a,
        "POST",
        f"/v1/tenants/{tenant_id}/organizations/{organization['id']}/projects",
        project_body,
        {"Idempotency-Key": project_key},
    )
    replayed_project, project_headers = owner.json_with_headers(
        replica_b,
        "POST",
        f"/v1/tenants/{tenant_id}/organizations/{organization['id']}/projects",
        project_body,
        {"Idempotency-Key": project_key},
    )
    require(replayed_project["id"] == project["id"], "Project idempotency replay returned a different resource")
    require(project_headers.get("Idempotency-Replayed") == "true", "Project replay header was missing")

    session_body = {"title": "Multi Replica Session", "visibility": "project", "provider": "codex"}
    session_key = f"session-{run_id}"
    session, _ = owner.json_with_headers(
        replica_a,
        "POST",
        f"/v1/projects/{project['id']}/sessions",
        session_body,
        {"Idempotency-Key": session_key},
    )
    replayed_session, session_headers = owner.json_with_headers(
        replica_b,
        "POST",
        f"/v1/projects/{project['id']}/sessions",
        session_body,
        {"Idempotency-Key": session_key},
    )
    require(replayed_session["id"] == session["id"], "Session idempotency replay returned a different resource")
    require(session_headers.get("Idempotency-Replayed") == "true", "Session replay header was missing")
    session_id = session["id"]
    target_id = session["executionTargetId"]
    target = owner.json(replica_a, "GET", f"/v1/tenants/{tenant_id}/execution-targets/{target_id}")
    worker_capabilities = build_worker_capabilities(
        worker_manifest_generator,
        provider_capability_catalog,
        target.get("capabilities"),
        WORKER_VERSION,
    )

    cross_replica = SSECollector(
        replica_a,
        f"/v1/sessions/{session_id}/events/stream?afterSequence=1",
        owner.cookie_header(),
        {2},
    )
    cross_replica.start()
    cross_replica.wait_for_retry()
    limited_status, limited_body, limited_headers = owner.request(
        replica_b,
        "GET",
        f"/v1/sessions/{session_id}/events/stream?afterSequence=1",
        headers={"Accept": "text/event-stream"},
    )
    require(limited_status == 429, f"cross-replica SSE user limit returned {limited_status}")
    require(b"sse_user_connection_limit" in limited_body, "SSE user limit returned the wrong error")
    require(limited_headers.get("Retry-After") == "2", "SSE user limit omitted Retry-After")
    owner.json(
        replica_b,
        "POST",
        f"/v1/sessions/{session_id}/turns",
        {"inputText": "written through replica B"},
    )
    cross_replica.wait_for({2})
    cross_replica.finish()

    metrics_status, metrics_body, _ = anonymous.request(replica_b, "GET", "/metrics")
    require(metrics_status == 200, f"metrics endpoint returned {metrics_status}")
    for metric_name in (
        b"synara_sse_connections",
        b"synara_sse_catchup_duration_seconds",
        b"synara_sse_connection_rejections_total",
        b"synara_artifact_ready_bytes",
        b"synara_database_connections",
    ):
        require(metric_name in metrics_body, f"metrics endpoint omitted {metric_name.decode()}")

    events = owner.json(replica_a, "GET", f"/v1/sessions/{session_id}/events?afterSequence=1&limit=10")
    sequence_two = next(item for item in events["items"] if item["sequence"] == 2)
    execution_id = sequence_two["payload"]["executionId"]

    worker = CookieClient()
    registration = worker.json(
        replica_a,
        "POST",
        "/v1/workers/register",
        {
            "executionTargetId": target_id,
            "targetKind": target["kind"],
            "instanceUid": str(uuid.uuid4()),
            "clusterId": "multi",
            "namespace": "default",
            "podName": f"multi-worker-{run_id}",
            "version": WORKER_VERSION,
            "protocolVersion": WORKER_PROTOCOL_VERSION,
            "capabilities": worker_capabilities,
            "leaseSupported": True,
            "fencingSupported": True,
        },
        {"Authorization": f"Bearer {registration_token}"},
    )
    worker_headers = {"Authorization": f"Bearer {registration['token']}"}
    claim_body = {
        "executionTargetId": target_id,
        "targetKind": target["kind"],
        "executionId": execution_id,
    }
    with concurrent.futures.ThreadPoolExecutor(max_workers=2) as executor:
        claims = list(
            executor.map(
                lambda request: worker.json(
                    request[0],
                    "POST",
                    "/v1/workers/executions/claim",
                    claim_body,
                    worker_headers | {"X-Request-ID": request[1]},
                ),
                [(replica_a, f"claim-a-{run_id}"), (replica_b, f"claim-b-{run_id}")],
            )
        )
    require(
        sum(1 for claim in claims if claim.get("execution") is not None) == 1,
        "Execution was not claimed exactly once",
    )
    winning_index = next(index for index, claim in enumerate(claims) if claim.get("execution") is not None)
    winning_claim = claims[winning_index]
    winning_replica = (replica_a, replica_b)[winning_index]
    other_replica = (replica_b, replica_a)[winning_index]

    cancelled = owner.json(
        other_replica,
        "POST",
        f"/v1/executions/{execution_id}/cancel",
        headers={"Idempotency-Key": f"cancel-claimed-{run_id}"},
    )
    require(
        cancelled.get("status") == "cancelled",
        "cross-replica cancellation did not terminate the claimed Execution",
    )
    stale_renew_status, stale_renew_body, _ = worker.request(
        winning_replica,
        "POST",
        f"/v1/workers/executions/{execution_id}/renew",
        {
            "tenantId": tenant_id,
            "generation": winning_claim["lease"]["generation"],
            "leaseToken": winning_claim["lease"]["leaseToken"],
        },
        worker_headers | {"X-Request-ID": f"renew-cancelled-{run_id}"},
    )
    require(
        stale_renew_status == 409,
        f"cancelled cross-replica Lease renewed with status {stale_renew_status}",
    )
    stale_renew_error = json.loads(stale_renew_body)
    require(
        stale_renew_error.get("error", {}).get("code") == "lease_not_current",
        "cancelled cross-replica Lease returned the wrong fencing error",
    )

    concurrent_turn_body = {"inputText": "same idempotent Turn through both replicas"}
    concurrent_turn_key = f"turn-{run_id}"
    with concurrent.futures.ThreadPoolExecutor(max_workers=2) as executor:
        turns = list(
            executor.map(
                lambda address: owner.json_with_headers(
                    address,
                    "POST",
                    f"/v1/sessions/{session_id}/turns",
                    concurrent_turn_body,
                    {"Idempotency-Key": concurrent_turn_key},
                ),
                [replica_a, replica_b],
            )
        )
    require(turns[0][0]["id"] == turns[1][0]["id"], "same-key concurrent Turns returned different resources")
    require(
        sum(1 for _, headers in turns if headers.get("Idempotency-Replayed") == "true") == 1,
        "same-key concurrent Turns did not produce exactly one replay",
    )

    same_key_turn_id = turns[0][0]["id"]
    events = owner.json(replica_a, "GET", f"/v1/sessions/{session_id}/events?afterSequence=4&limit=10")
    same_key_event = next(
        item
        for item in events["items"]
        if item["eventType"] == "turn.created" and item["payload"].get("turnId") == same_key_turn_id
    )
    same_key_execution_id = same_key_event["executionId"]
    cancelled_same_key = owner.json(
        replica_b,
        "POST",
        f"/v1/executions/{same_key_execution_id}/cancel",
        headers={"Idempotency-Key": f"cancel-same-key-{run_id}"},
    )
    require(
        cancelled_same_key.get("status") == "cancelled",
        "same-key Turn did not reach a legal terminal state",
    )

    independent_turn = owner.json(
        replica_b,
        "POST",
        f"/v1/sessions/{session_id}/turns",
        {"inputText": "independent Turn after idempotency race"},
        {"Idempotency-Key": f"turn-independent-{run_id}"},
    )
    events = owner.json(replica_b, "GET", f"/v1/sessions/{session_id}/events?afterSequence=2&limit=10")
    require(
        [item["sequence"] for item in events["items"]] == [3, 4, 5, 6, 7],
        "cross-replica terminal and Turn sequences were not contiguous",
    )
    require(
        [item["eventType"] for item in events["items"]]
        == ["execution.leased", "execution.cancelled", "turn.created", "execution.cancelled", "turn.created"],
        "cross-replica terminal and Turn events were not ordered as expected",
    )
    independent_event = next(
        item
        for item in events["items"]
        if item["eventType"] == "turn.created" and item["payload"].get("turnId") == independent_turn["id"]
    )

    (state_dir / "session-id").write_text(session_id)
    (state_dir / "active-execution-id").write_text(independent_event["executionId"])
    print(f"Cross-replica SSE, unique Claim, and legal sequential Turns passed: session={session_id}", flush=True)


def phase_two(replica_b: str, state_dir: Path, expected_schema_version: int) -> None:
    session_id = (state_dir / "session-id").read_text().strip()
    active_execution_id = (state_dir / "active-execution-id").read_text().strip()
    owner = CookieClient(state_dir / "owner-cookie.json")
    assert_ready(CookieClient(), replica_b, expected_schema_version)

    failover = SSECollector(
        replica_b,
        f"/v1/sessions/{session_id}/events/stream",
        owner.cookie_header(),
        {5, 6, 7, 8, 9},
        {"Last-Event-ID": "4"},
    )
    failover.start()
    failover.wait_for({5, 6, 7})
    cancelled = owner.json(
        replica_b,
        "POST",
        f"/v1/executions/{active_execution_id}/cancel",
        headers={"Idempotency-Key": f"cancel-before-failover-turn-{uuid.uuid4().hex}"},
    )
    require(cancelled.get("status") == "cancelled", "failover replica did not terminate the active Execution")
    owner.json(
        replica_b,
        "POST",
        f"/v1/sessions/{session_id}/turns",
        {"inputText": "continued after replica A stopped"},
    )
    failover.wait_for({8, 9})
    failover.finish()
    require(4 not in failover.observed, "failover SSE replayed acknowledged Event 4")
    print(f"Replica failover and Last-Event-ID catch-up passed: session={session_id}", flush=True)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("phase", choices=("phase-one", "phase-two"))
    parser.add_argument("--replica-a")
    parser.add_argument("--replica-b", required=True)
    parser.add_argument("--registration-token")
    parser.add_argument("--worker-manifest-generator", type=Path)
    parser.add_argument("--provider-capability-catalog", type=Path)
    parser.add_argument("--expected-schema-version", type=int, required=True)
    parser.add_argument("--state-dir", type=Path, default=Path("/state/multi-replica-acceptance"))
    arguments = parser.parse_args()

    if arguments.phase == "phase-one":
        require(arguments.replica_a is not None, "phase one requires --replica-a")
        require(arguments.registration_token is not None, "phase one requires --registration-token")
        require(
            arguments.worker_manifest_generator is not None,
            "phase one requires --worker-manifest-generator",
        )
        require(
            arguments.provider_capability_catalog is not None,
            "phase one requires --provider-capability-catalog",
        )
        phase_one(
            arguments.replica_a,
            arguments.replica_b,
            arguments.registration_token,
            arguments.worker_manifest_generator,
            arguments.provider_capability_catalog,
            arguments.state_dir,
            arguments.expected_schema_version,
        )
    else:
        phase_two(arguments.replica_b, arguments.state_dir, arguments.expected_schema_version)


if __name__ == "__main__":
    try:
        main()
    except (AcceptanceError, KeyError, StopIteration, OSError, ValueError) as error:
        print(f"Multi-replica acceptance failed: {error}", file=sys.stderr)
        raise SystemExit(1) from error
