#!/usr/bin/env python3
"""Stage 3 Provider Runtime acceptance runner.

Target drivers exercise production Control Plane, Worker, and agentd paths.
The runner never registers, heartbeats, or claims a Worker on behalf of
agentd. The Local driver uses LocalSupervisor; SSH, Docker, and Kubernetes
drivers provision managed Execution Targets through the user API.
"""

from __future__ import annotations

import argparse
import base64
import dataclasses
import datetime as dt
import hashlib
import http.client
import http.cookiejar
import http.server
import ipaddress
import json
import os
import pathlib
import re
import shutil
import signal
import socket
import sqlite3
import subprocess
import sys
import tempfile
import threading
import time
import traceback
import urllib.error
import urllib.parse
import urllib.request
import uuid
from collections.abc import Callable, Iterable, Mapping, Sequence
from typing import Any, Protocol, TypeVar


SCHEMA_VERSION = "synara.provider-acceptance.v1"
FIXTURE_CREDENTIAL_SENTINEL = "stage3-provider-acceptance-credential-v1"
FIXTURE_ARTIFACT_RELATIVE_PATH = ".synara-stage3-acceptance/artifact.txt"
TERMINAL_LARGE_TOTAL_BYTES = 2 * (1 << 20) + 257
TERMINAL_LARGE_CHUNK_BYTES = 63 << 10
TERMINAL_LOG_PREVIEW_BYTES = 32 << 10
TERMINAL_LOG_SEGMENT_BYTES = 1 << 20
TERMINAL_LARGE_PATTERN = b"0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ._-"
DOCKER_VOLUME_SENTINEL_PATH = "/data/.synara-stage3-provider-acceptance-volume"
DOCKER_VOLUME_SENTINEL_VALUE = "synara-stage3-named-volume-continuity-v1"
SSH_REMOTE_FIXTURE_PATH = "/opt/synara/acceptance/provider-host-fixture.mjs"
SSH_SERVICE_USER = "synara"
SSH_RELAY_LOOPBACK_HOST = "127.0.0.1"
SSH_RELAY_TRANSPORT = "runner-owned reverse SSH relay to the local Worker-only proxy"
SSH_CONTROL_PLANE_OPERATION_TIMEOUT = 180.0
SSH_CREDENTIAL_LIFECYCLE = (
    "runner posts the one-time private key once during Target creation, deletes the local plaintext copy after "
    "provisioning, and relies on the Control Plane encrypted credential until ssh/revoke"
)
WORKER_PROXY_ALLOWED_PATH_PREFIXES = ("/v1/workers/", "/v1/artifact-content/")
WORKER_PROXY_MAX_REQUEST_BYTES = 64 << 20
CASE_STATUSES = frozenset({"pass", "unsupported", "skipped", "fail"})
TERMINAL_EVENT_TYPES = frozenset({"execution.completed", "execution.failed", "execution.cancelled"})
JSON_REPORT_NAME = "acceptance-report.json"
MARKDOWN_REPORT_NAME = "acceptance-report.md"
PROVIDERS = ("codex", "claudeAgent", "cursor", "gemini", "grok", "kilo", "opencode", "pi")
FIXTURE_SUPPORTED_PROVIDERS = frozenset({"codex", "claudeAgent"})
REAL_PROVIDER_SMOKE_PROVIDERS = frozenset({"codex", "claudeAgent"})
SUITES = ("fixture", "real-provider-smoke")
FAILURE_CASES = (
    "provider-malformed",
    "provider-oversized",
    "provider-crash",
    "worker-network",
    "kubernetes-drain",
    "kubernetes-eviction",
    "kubernetes-image-canary",
)
COMMON_PROVIDER_FAILURE_CASES = (
    "provider-malformed",
    "provider-oversized",
    "provider-crash",
)
TARGET_FAILURE_CASES: Mapping[str, tuple[str, ...]] = {
    "local": COMMON_PROVIDER_FAILURE_CASES,
    "ssh": COMMON_PROVIDER_FAILURE_CASES,
    "docker": (*COMMON_PROVIDER_FAILURE_CASES, "worker-network"),
    "kubernetes": (
        *COMMON_PROVIDER_FAILURE_CASES,
        "worker-network",
        "kubernetes-drain",
        "kubernetes-eviction",
        "kubernetes-image-canary",
    ),
}
FAILURE_CASE_METADATA: Mapping[str, Mapping[str, str]] = {
    "provider-malformed": {
        "id": "failure.provider-host-malformed",
        "name": "Classify malformed Provider Host JSONL and recover the Host",
    },
    "provider-oversized": {
        "id": "failure.provider-host-oversized",
        "name": "Classify oversized Provider Host JSONL and recover the Host",
    },
    "provider-crash": {
        "id": "failure.provider-host-crash",
        "name": "Classify a mid-Turn Provider Host crash and recover the Host",
    },
    "worker-network": {
        "id": "failure.worker-network-interruption",
        "name": "Interrupt Worker network transport and verify Generation-fenced recovery",
    },
    "kubernetes-drain": {
        "id": "failure.kubernetes-node-drain",
        "name": "Drain the exact Kubernetes execution Pod and verify safe recovery",
    },
    "kubernetes-eviction": {
        "id": "failure.kubernetes-pod-eviction",
        "name": "Evict the exact Kubernetes execution Pod and verify safe recovery",
    },
    "kubernetes-image-canary": {
        "id": "canary.kubernetes-worker-image",
        "name": "Run an isolated Kubernetes Worker image canary through the user API",
    },
}
SECRET_SCAN_PATTERNS: tuple[tuple[str, re.Pattern[bytes]], ...] = (
    ("private-key-pem", re.compile(br"-----BEGIN (?:OPENSSH |RSA |EC |DSA )?PRIVATE KEY-----")),
    ("aws-access-key", re.compile(br"\b(?:AKIA|ASIA)[0-9A-Z]{16}\b")),
    ("github-token", re.compile(br"\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}\b")),
    ("openai-style-key", re.compile(br"\bsk-[A-Za-z0-9_-]{20,}\b")),
)

T = TypeVar("T")


def utc_now() -> str:
    return dt.datetime.now(dt.timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def elapsed_ms(started: float) -> int:
    return max(0, round((time.monotonic() - started) * 1000))


def random_key() -> str:
    return base64.urlsafe_b64encode(os.urandom(32)).decode("ascii").rstrip("=")


def terminal_large_bytes(byte_offset: int, byte_length: int) -> bytes:
    if byte_offset < 0 or byte_length < 0:
        raise ValueError("terminal fixture offsets and lengths must be non-negative")
    if byte_length == 0:
        return b""
    pattern_offset = byte_offset % len(TERMINAL_LARGE_PATTERN)
    repetitions = (pattern_offset + byte_length + len(TERMINAL_LARGE_PATTERN) - 1) // len(
        TERMINAL_LARGE_PATTERN
    )
    return (TERMINAL_LARGE_PATTERN * repetitions)[pattern_offset : pattern_offset + byte_length]


def terminal_large_expected_segments() -> list[dict[str, Any]]:
    segments: list[dict[str, Any]] = []
    byte_offset = 0
    segment_index = 0
    while byte_offset < TERMINAL_LARGE_TOTAL_BYTES:
        byte_length = min(TERMINAL_LOG_SEGMENT_BYTES, TERMINAL_LARGE_TOTAL_BYTES - byte_offset)
        payload = terminal_large_bytes(byte_offset, byte_length)
        segments.append(
            {
                "offset": byte_offset,
                "length": byte_length,
                "segmentIndex": segment_index,
                "encoding": "utf-8",
                "sha256": hashlib.sha256(payload).hexdigest(),
            }
        )
        byte_offset += byte_length
        segment_index += 1
    return segments


def contains_runtime_physical_path(value: Any) -> bool:
    if isinstance(value, Mapping):
        for key, child in value.items():
            normalized_key = str(key).replace("_", "").replace("-", "").casefold()
            if normalized_key in {
                "path",
                "physicalpath",
                "sourcepath",
                "sourceroot",
                "runtimeroot",
                "runtimeoutputdirectory",
                "runtimeoutputroot",
                "persistedoutputpath",
                "rawoutputpath",
                "outputfile",
                "workspacedirectory",
            }:
                return True
            if contains_runtime_physical_path(child):
                return True
        return False
    if isinstance(value, Sequence) and not isinstance(value, (str, bytes, bytearray)):
        return any(contains_runtime_physical_path(child) for child in value)
    if not isinstance(value, str):
        return False
    normalized = value.strip()
    return (
        normalized.startswith(("/", "\\\\"))
        or re.match(r"^[A-Za-z]:[\\\\/]", normalized) is not None
        or "runtime-output" in normalized.casefold()
        or ".synara-runtime" in normalized.casefold()
    )


def repository_metadata(repo_root: pathlib.Path) -> Mapping[str, Any]:
    def git_output(arguments: Sequence[str]) -> str | None:
        try:
            completed = subprocess.run(
                ["git", *arguments],
                cwd=repo_root,
                env={key: os.environ[key] for key in ("PATH", "HOME") if key in os.environ},
                stdout=subprocess.PIPE,
                stderr=subprocess.DEVNULL,
                text=True,
                timeout=10.0,
                check=False,
            )
        except (OSError, subprocess.TimeoutExpired):
            return None
        value = completed.stdout.strip()
        return value if completed.returncode == 0 else None

    catalog_path = repo_root / "packages" / "contracts" / "src" / "providerCapabilityCatalog.json"
    catalog_hash = hashlib.sha256(catalog_path.read_bytes()).hexdigest() if catalog_path.is_file() else None
    status = git_output(["status", "--porcelain", "--untracked-files=all"])
    return {
        "gitSha": git_output(["rev-parse", "HEAD"]),
        "worktreeDirty": None if status is None else bool(status),
        "providerCapabilityCatalogSha256": catalog_hash,
    }


def reserve_loopback_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as listener:
        listener.bind(("127.0.0.1", 0))
        return int(listener.getsockname()[1])


def json_object(value: Any, label: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise AcceptanceError("runner.response_shape_invalid", f"{label} was not a JSON object.")
    return value


def json_items(value: Any, label: str) -> list[dict[str, Any]]:
    envelope = json_object(value, label)
    raw_items = envelope.get("items")
    if not isinstance(raw_items, list) or not all(isinstance(item, dict) for item in raw_items):
        raise AcceptanceError("runner.response_shape_invalid", f"{label}.items was not an object array.")
    return raw_items


class Deadline:
    def __init__(self, seconds: float) -> None:
        self._end = time.monotonic() + seconds

    def remaining(self) -> float:
        return max(0.0, self._end - time.monotonic())

    def request_timeout(self, maximum: float = 10.0) -> float:
        remaining = self.remaining()
        if remaining <= 0:
            raise AcceptanceError("runner.timeout", "The acceptance deadline was exceeded.")
        return max(0.25, min(maximum, remaining))

    def sleep(self, seconds: float) -> None:
        remaining = self.remaining()
        if remaining <= 0:
            raise AcceptanceError("runner.timeout", "The acceptance deadline was exceeded.")
        time.sleep(min(seconds, remaining))


class SecretRedactor:
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._values: list[tuple[str, str]] = [
            (FIXTURE_CREDENTIAL_SENTINEL, "[REDACTED_CREDENTIAL]"),
        ]

    def add(self, value: str | None, replacement: str = "[REDACTED]") -> None:
        if not value or len(value) < 6:
            return
        with self._lock:
            if all(existing != value for existing, _ in self._values):
                self._values.append((value, replacement))
                self._values.sort(key=lambda item: len(item[0]), reverse=True)

    def text(self, value: str) -> str:
        with self._lock:
            replacements = tuple(self._values)
        for secret, replacement in replacements:
            value = value.replace(secret, replacement)
        return value

    def value(self, value: Any) -> Any:
        if isinstance(value, str):
            return self.text(value)
        if isinstance(value, list):
            return [self.value(item) for item in value]
        if isinstance(value, tuple):
            return [self.value(item) for item in value]
        if isinstance(value, dict):
            return {str(key): self.value(item) for key, item in value.items()}
        return value

    def secret_values(self) -> tuple[str, ...]:
        with self._lock:
            return tuple(secret for secret, _ in self._values if secret)


class AcceptanceError(RuntimeError):
    def __init__(self, code: str, message: str, evidence: Mapping[str, Any] | None = None) -> None:
        super().__init__(message)
        self.code = code
        self.evidence = dict(evidence or {})


class AcceptanceUnsupported(AcceptanceError):
    """An explicit capability boundary, not an implicit pass."""


class RunnerInterrupted(BaseException):
    def __init__(self, signum: int) -> None:
        self.signum = signum
        self.signal_name = signal.Signals(signum).name
        super().__init__(f"Acceptance run interrupted by {self.signal_name}.")


class HTTPFailure(AcceptanceError):
    def __init__(self, method: str, path: str, status: int, body: str) -> None:
        code = "runner.http_request_failed"
        message = f"{method} {path} returned HTTP {status}."
        details: dict[str, Any] = {"method": method, "path": path, "status": status}
        try:
            problem = json.loads(body)
        except json.JSONDecodeError:
            problem = None
        if isinstance(problem, dict):
            error = problem.get("error")
            if isinstance(error, dict):
                if isinstance(error.get("code"), str):
                    code = str(error["code"])
                if isinstance(error.get("message"), str):
                    message = str(error["message"])
                details["problem"] = {
                    key: error[key]
                    for key in ("code", "message", "details")
                    if key in error
                }
        elif body.strip():
            details["bodyExcerpt"] = body.strip()[:1000]
        super().__init__(code, message, details)


@dataclasses.dataclass(frozen=True)
class RunnerOptions:
    target: str
    provider: str
    suite: str
    output_dir: pathlib.Path
    timeout_seconds: float
    runner_command: tuple[str, ...]
    skip_build: bool
    control_plane_binary: pathlib.Path | None
    keep: bool
    restart_control_plane: bool
    ssh_orbctl_bin: str
    ssh_machine_name: str | None
    ssh_machine_arch: str
    ssh_machine_image: str
    ssh_node_version: str
    docker_socket_path: pathlib.Path
    docker_worker_image: str | None
    docker_skip_worker_build: bool
    docker_control_plane_host: str
    docker_network_mode: str | None
    docker_memory_bytes: int
    docker_nano_cpus: int
    kubernetes_context: str | None
    kubernetes_kubeconfig: pathlib.Path | None
    kubernetes_allow_nondisposable: bool
    kubernetes_worker_image: str | None
    kubernetes_skip_worker_build: bool
    kubernetes_control_plane_host: str
    kind_bin: str
    kind_cluster_name: str | None
    kind_node_image: str
    failure_cases: tuple[str, ...]
    network_outage_seconds: float
    docker_allow_network_interruption: bool
    kubernetes_allow_node_drain: bool
    failure_only: bool


@dataclasses.dataclass(frozen=True)
class TargetLifecycle:
    worker_allocation: str
    replacement: str

    def __post_init__(self) -> None:
        if self.worker_allocation not in {"standing", "execution-pinned"}:
            raise ValueError(f"unsupported worker allocation: {self.worker_allocation}")
        if self.replacement not in {"none", "managed"}:
            raise ValueError(f"unsupported replacement capability: {self.replacement}")

    @property
    def execution_pinned(self) -> bool:
        return self.worker_allocation == "execution-pinned"

    @property
    def managed_replacement(self) -> bool:
        return self.replacement == "managed"


STANDING_WORKER = TargetLifecycle(worker_allocation="standing", replacement="none")
STANDING_MANAGED_WORKER = TargetLifecycle(worker_allocation="standing", replacement="managed")
EXECUTION_PINNED_WORKER = TargetLifecycle(worker_allocation="execution-pinned", replacement="none")


@dataclasses.dataclass
class ScenarioState:
    tenant_id: str | None = None
    organization_id: str | None = None
    target_id: str | None = None
    credential_id: str | None = None
    project_id: str | None = None
    session_id: str | None = None
    first_worker_id: str | None = None
    first_generation: int | None = None
    pre_restart_sequence: int | None = None
    last_sequence: int = 0
    restarted: bool = False
    worker_replaced: bool = False
    replacement_worker_id: str | None = None
    pending_approval: dict[str, Any] | None = None
    pending_real_turn_id: str | None = None


class APIClient:
    def __init__(self, base_url: str, deadline: Deadline, redactor: SecretRedactor) -> None:
        self.base_url = base_url.rstrip("/")
        self.deadline = deadline
        self.redactor = redactor
        self.cookies = http.cookiejar.CookieJar()
        self.opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.cookies))

    def request(
        self,
        method: str,
        path: str,
        payload: Mapping[str, Any] | None = None,
        expected: Iterable[int] = (200,),
        *,
        maximum_timeout: float = 10.0,
    ) -> Any:
        data = None
        headers = {
            "Accept": "application/json",
            "X-Request-ID": f"stage3-acceptance-{uuid.uuid4()}",
        }
        if payload is not None:
            data = json.dumps(payload, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
            headers["Content-Type"] = "application/json"
        if method in {"POST", "PUT", "PATCH", "DELETE"}:
            headers["Idempotency-Key"] = str(uuid.uuid4())
        request = urllib.request.Request(self.base_url + path, data=data, headers=headers, method=method)
        try:
            with self.opener.open(
                request,
                timeout=self.deadline.request_timeout(maximum=maximum_timeout),
            ) as response:
                status = int(response.status)
                body = response.read().decode("utf-8", errors="replace")
        except urllib.error.HTTPError as error:
            body = error.read().decode("utf-8", errors="replace")
            raise HTTPFailure(method, path, int(error.code), self.redactor.text(body)) from None
        except (urllib.error.URLError, TimeoutError, OSError) as error:
            raise AcceptanceError(
                "runner.http_transport_failed",
                f"{method} {path} failed: {self.redactor.text(str(error))}",
                {"method": method, "path": path},
            ) from None
        if status not in set(expected):
            raise HTTPFailure(method, path, status, self.redactor.text(body))
        for cookie in self.cookies:
            self.redactor.add(cookie.value, "[REDACTED_SESSION_COOKIE]")
        if not body.strip():
            return None
        try:
            return json.loads(body)
        except json.JSONDecodeError:
            raise AcceptanceError(
                "runner.response_json_invalid",
                f"{method} {path} returned invalid JSON.",
                {"method": method, "path": path, "bodyExcerpt": self.redactor.text(body[:1000])},
            ) from None

    def wait_until(
        self,
        description: str,
        probe: Callable[[], T | None],
        interval: float = 0.25,
    ) -> T:
        while self.deadline.remaining() > 0:
            value = probe()
            if value is not None:
                return value
            self.deadline.sleep(interval)
        raise AcceptanceError(
            "runner.wait_timeout",
            f"Timed out waiting for {description}.",
            {"waitedFor": description},
        )


class TargetDriver(Protocol):
    name: str
    api: APIClient | None
    lifecycle: TargetLifecycle
    replacement_workspace_semantics: str
    pending_interaction_recovery: str | None

    def prepare(self) -> Mapping[str, Any]: ...

    def start(self) -> Mapping[str, Any]: ...

    def restart(self) -> Mapping[str, Any]: ...

    def provision_target(
        self,
        tenant_id: str,
        organization_id: str,
        provider: str,
    ) -> Mapping[str, Any]: ...

    def replace_worker(
        self,
        tenant_id: str,
        target_id: str,
        provider: str,
    ) -> Mapping[str, Any]: ...

    def observe_execution(self, target_id: str, execution_id: str) -> Mapping[str, Any]: ...

    def observe_terminal_execution(self, target_id: str, execution_id: str) -> Mapping[str, Any]: ...

    def recover_pending_interaction(self, target_id: str, execution_id: str) -> Mapping[str, Any]: ...

    def stop(self) -> None: ...

    def cleanup(self) -> Mapping[str, Any] | None: ...


class _RedactingLogPump(threading.Thread):
    def __init__(self, source: Any, destination: pathlib.Path, redactor: SecretRedactor) -> None:
        super().__init__(name=f"acceptance-log-{destination.stem}", daemon=True)
        self.source = source
        self.destination = destination
        self.redactor = redactor

    def run(self) -> None:
        self.destination.parent.mkdir(parents=True, exist_ok=True)
        with self.destination.open("w", encoding="utf-8") as output:
            for line in iter(self.source.readline, ""):
                output.write(self.redactor.text(line))
                output.flush()
        self.source.close()


class _WorkerOnlyProxy:
    """Expose only the Worker API surface while the full Control Plane stays on loopback."""

    def __init__(self, upstream_port: int) -> None:
        proxy = self

        class Handler(http.server.BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.0"
            server_version = "SynaraStage3WorkerProxy/1"
            sys_version = ""

            def setup(self) -> None:
                super().setup()
                self.connection.settimeout(30.0)

            def do_GET(self) -> None:
                self._forward()

            def do_POST(self) -> None:
                self._forward()

            def do_PUT(self) -> None:
                self._forward()

            def do_HEAD(self) -> None:
                self._reject_method()

            def do_PATCH(self) -> None:
                self._reject_method()

            def do_DELETE(self) -> None:
                self._reject_method()

            def do_OPTIONS(self) -> None:
                self._reject_method()

            def do_CONNECT(self) -> None:
                self._reject_method()

            def do_TRACE(self) -> None:
                self._reject_method()

            def log_message(self, _format: str, *_args: Any) -> None:
                return None

            def _reject_method(self) -> None:
                self._json_error(405, "method_not_allowed")

            def _forward(self) -> None:
                if proxy._transport_interrupted.is_set():
                    proxy._record_interrupted_request()
                    try:
                        self.connection.shutdown(socket.SHUT_RDWR)
                    except OSError:
                        pass
                    self.connection.close()
                    return
                parsed = urllib.parse.urlsplit(self.path)
                path = parsed.path
                segments = path.split("/")
                if (
                    parsed.scheme
                    or parsed.netloc
                    or len(self.path) > 16_384
                    or "%" in path
                    or "\\" in path
                    or "//" in path
                    or any(segment in {".", ".."} for segment in segments)
                    or not any(path.startswith(prefix) for prefix in WORKER_PROXY_ALLOWED_PATH_PREFIXES)
                ):
                    self._json_error(404, "route_not_exposed")
                    return
                if self.headers.get("Transfer-Encoding"):
                    self._json_error(400, "transfer_encoding_unsupported")
                    return
                try:
                    content_length = int(self.headers.get("Content-Length", "0"))
                except ValueError:
                    self._json_error(400, "content_length_invalid")
                    return
                if content_length < 0 or content_length > WORKER_PROXY_MAX_REQUEST_BYTES:
                    self._json_error(413, "request_too_large")
                    return
                try:
                    body = self.rfile.read(content_length) if content_length else None
                except OSError:
                    self._json_error(400, "request_body_unavailable")
                    return
                if body is not None and len(body) != content_length:
                    self._json_error(400, "request_body_incomplete")
                    return

                headers = {
                    name: value
                    for name, value in self.headers.items()
                    if name.lower()
                    in {
                        "accept",
                        "authorization",
                        "content-type",
                        "idempotency-key",
                        "user-agent",
                        "x-request-id",
                    }
                }
                upstream = http.client.HTTPConnection("127.0.0.1", proxy.upstream_port, timeout=30.0)
                response_started = False
                try:
                    upstream.request(self.command, self.path, body=body, headers=headers)
                    response = upstream.getresponse()
                    self.send_response(response.status)
                    response_started = True
                    allowed_response_headers = {
                        "cache-control",
                        "content-disposition",
                        "content-length",
                        "content-type",
                        "etag",
                        "last-modified",
                    }
                    for name, value in response.getheaders():
                        if name.lower() in allowed_response_headers:
                            self.send_header(name, value)
                    self.send_header("Connection", "close")
                    self.end_headers()
                    while True:
                        chunk = response.read(64 << 10)
                        if not chunk:
                            break
                        self.wfile.write(chunk)
                except (OSError, http.client.HTTPException):
                    if not response_started and not self.wfile.closed:
                        self._json_error(502, "control_plane_unavailable")
                finally:
                    upstream.close()

            def _json_error(self, status: int, code: str) -> None:
                payload = json.dumps({"error": {"code": code}}, separators=(",", ":")).encode("utf-8")
                try:
                    self.send_response(status)
                    self.send_header("Content-Type", "application/json")
                    self.send_header("Content-Length", str(len(payload)))
                    self.send_header("Connection", "close")
                    self.end_headers()
                    if self.command != "HEAD":
                        self.wfile.write(payload)
                except OSError:
                    return

        try:
            self.server = http.server.ThreadingHTTPServer(("0.0.0.0", 0), Handler)
        except OSError as error:
            raise AcceptanceError(
                "runner.worker_proxy_start_failed",
                f"Worker-only proxy could not bind: {error}",
            ) from None
        self.server.daemon_threads = True
        self.upstream_port = upstream_port
        self.port = int(self.server.server_address[1])
        self._transport_interrupted = threading.Event()
        self._interrupted_request_lock = threading.Lock()
        self._interrupted_request_count = 0
        self.thread = threading.Thread(
            target=self.server.serve_forever,
            kwargs={"poll_interval": 0.1},
            name="stage3-worker-only-proxy",
            daemon=True,
        )

    def start(self) -> None:
        self.thread.start()

    def stop(self) -> None:
        self.resume_transport()
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5.0)
        if self.thread.is_alive():
            raise AcceptanceError(
                "runner.worker_proxy_stop_failed",
                "Worker-only proxy did not stop within five seconds.",
            )

    def interrupt_transport(self) -> None:
        self._transport_interrupted.set()

    def resume_transport(self) -> None:
        self._transport_interrupted.clear()

    def interrupted_request_count(self) -> int:
        with self._interrupted_request_lock:
            return self._interrupted_request_count

    def _record_interrupted_request(self) -> None:
        with self._interrupted_request_lock:
            self._interrupted_request_count += 1

    def evidence(self, advertised_host: str) -> Mapping[str, Any]:
        return {
            "listenAddress": "0.0.0.0",
            "advertisedHost": advertised_host,
            "port": self.port,
            "upstreamAddress": f"127.0.0.1:{self.upstream_port}",
            "allowedPathPrefixes": list(WORKER_PROXY_ALLOWED_PATH_PREFIXES),
            "faultInjection": "runner-owned transport close before HTTP forwarding",
        }


class LocalDriver:
    name = "local"
    lifecycle = STANDING_WORKER
    replacement_workspace_semantics = "no managed Worker replacement"
    pending_interaction_recovery: str | None = None

    def __init__(
        self,
        repo_root: pathlib.Path,
        options: RunnerOptions,
        deadline: Deadline,
        redactor: SecretRedactor,
    ) -> None:
        self.repo_root = repo_root
        self.options = options
        self.deadline = deadline
        self.redactor = redactor
        self.port = reserve_loopback_port()
        self.base_url = f"http://127.0.0.1:{self.port}"
        self.api = APIClient(self.base_url, deadline, redactor)
        if options.keep:
            self.state_dir = options.output_dir / "state"
            self.state_dir.mkdir(parents=True, exist_ok=True)
            self._temporary_state = False
        else:
            self.state_dir = pathlib.Path(tempfile.mkdtemp(prefix="synara-stage3-provider-acceptance-"))
            self._temporary_state = True
        self.logs_dir = options.output_dir / "logs"
        self.logs_dir.mkdir(parents=True, exist_ok=True)
        self.binary_path = options.control_plane_binary or self.state_dir / "bin" / "synara-control-plane"
        self.process: subprocess.Popen[str] | None = None
        self.log_pump: _RedactingLogPump | None = None
        self.generation = 0
        self.resource_owner = uuid.uuid4().hex[:20]
        self.installation_id = f"stage3-provider-acceptance-{uuid.uuid4()}"
        self.cookie_name = "synara_stage3_acceptance"
        self.cursor_key = random_key()
        self.credential_key = random_key()
        self.redactor.add(self.cursor_key, "[REDACTED_CURSOR_KEY]")
        self.redactor.add(self.credential_key, "[REDACTED_CREDENTIAL_KEY]")
        self._previous_signal_handlers: dict[signal.Signals, Any] = {}

    def install_signal_handlers(self) -> None:
        if os.name != "posix":
            return
        for watched in (signal.SIGTERM, signal.SIGHUP, signal.SIGINT):
            self._previous_signal_handlers[watched] = signal.getsignal(watched)
            signal.signal(watched, self._interrupt)

    def restore_signal_handlers(self) -> None:
        for watched, previous in self._previous_signal_handlers.items():
            signal.signal(watched, previous)
        self._previous_signal_handlers.clear()

    def suppress_signals_for_cleanup(self) -> None:
        for watched in self._previous_signal_handlers:
            signal.signal(watched, signal.SIG_IGN)

    @staticmethod
    def _interrupt(signum: int, _frame: Any) -> None:
        raise RunnerInterrupted(signum)

    def prepare(self) -> Mapping[str, Any]:
        if self.options.skip_build:
            if self.options.control_plane_binary is None:
                raise AcceptanceError(
                    "runner.control_plane_binary_required",
                    "--skip-build requires --control-plane-binary.",
                    {"requiredInputs": ["--control-plane-binary"]},
                )
            if not self.binary_path.is_file() or not os.access(self.binary_path, os.X_OK):
                raise AcceptanceError(
                    "runner.control_plane_binary_unusable",
                    "The configured Control Plane binary is not an executable file.",
                    {"path": str(self.binary_path)},
                )
            return {
                "build": "skipped",
                "binary": str(self.binary_path),
                "resourceOwner": self.resource_owner,
            }

        self.binary_path.parent.mkdir(parents=True, exist_ok=True)
        command = ["go", "build", "-o", str(self.binary_path), "./cmd/api"]
        started = time.monotonic()
        try:
            completed = subprocess.run(
                command,
                cwd=self.repo_root / "services" / "control-plane",
                env=self._tool_environment(),
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                text=True,
                timeout=self.deadline.request_timeout(maximum=max(30.0, self.deadline.remaining())),
                check=False,
            )
        except (OSError, subprocess.TimeoutExpired) as error:
            raise AcceptanceError(
                "runner.control_plane_build_failed",
                f"Control Plane build could not run: {self.redactor.text(str(error))}",
            ) from None
        build_log = self.logs_dir / "control-plane-build.log"
        build_log.write_text(self.redactor.text(completed.stdout), encoding="utf-8")
        if completed.returncode != 0:
            raise AcceptanceError(
                "runner.control_plane_build_failed",
                f"Control Plane build exited with status {completed.returncode}.",
                {"log": str(build_log), "exitCode": completed.returncode},
            )
        return {
            "build": "completed",
            "durationMs": elapsed_ms(started),
            "binary": str(self.binary_path),
            "log": str(build_log),
            "resourceOwner": self.resource_owner,
        }

    def start(self) -> Mapping[str, Any]:
        if self.process is not None:
            raise AcceptanceError("runner.control_plane_already_running", "Control Plane is already running.")
        self.generation += 1
        log_path = self.logs_dir / f"control-plane-{self.generation}.log"
        try:
            process = subprocess.Popen(
                [str(self.binary_path)],
                cwd=self.repo_root,
                env=self._control_plane_environment(),
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                text=True,
                bufsize=1,
                start_new_session=True,
            )
        except OSError as error:
            raise AcceptanceError(
                "runner.control_plane_start_failed",
                f"Control Plane could not start: {self.redactor.text(str(error))}",
            ) from None
        self.process = process
        if process.stdout is None:
            self.stop()
            raise AcceptanceError("runner.control_plane_start_failed", "Control Plane output pipe was unavailable.")
        self.log_pump = _RedactingLogPump(process.stdout, log_path, self.redactor)
        self.log_pump.start()

        def ready_probe() -> dict[str, Any] | None:
            if process.poll() is not None:
                raise AcceptanceError(
                    "runner.control_plane_exited",
                    f"Control Plane exited with status {process.returncode} before readiness.",
                    {"log": str(log_path), "exitCode": process.returncode},
                )
            try:
                return json_object(self.api.request("GET", "/ready"), "ready")
            except AcceptanceError:
                return None

        ready = self.api.wait_until("Control Plane readiness", ready_probe, interval=0.1)
        return {
            "processGeneration": self.generation,
            "baseUrl": self.base_url,
            "pid": process.pid,
            "log": str(log_path),
            "readiness": ready,
        }

    def restart(self) -> Mapping[str, Any]:
        previous_pid = self.process.pid if self.process is not None else None
        self.stop()
        started = self.start()
        return {"previousPid": previous_pid, **started}

    def stop(self) -> None:
        process = self.process
        pump = self.log_pump
        self.process = None
        self.log_pump = None
        if process is None:
            return
        if process.poll() is None:
            try:
                os.killpg(process.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            try:
                process.wait(timeout=min(8.0, max(1.0, self.deadline.remaining())))
            except subprocess.TimeoutExpired:
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                process.wait(timeout=3.0)
        if pump is not None:
            pump.join(timeout=2.0)

    def cleanup(self) -> Mapping[str, Any]:
        self.stop()
        self._release_state()
        return {
            "target": self.name,
            "resourceOwner": self.resource_owner,
            "stateDirectory": str(self.state_dir),
            "stateRemoved": self._temporary_state and not self.state_dir.exists(),
            "statePreservedByRequest": not self._temporary_state,
            "controlPlaneStopped": True,
        }

    def collect_failure_diagnostics(self, case_id: str) -> Mapping[str, Any]:
        logs = sorted(self.logs_dir.glob("*.log"), key=lambda path: path.stat().st_mtime_ns)
        selected = logs[-3:]
        return {
            "caseId": case_id,
            "controlPlaneGeneration": self.generation,
            "controlPlaneRunning": self.process is not None and self.process.poll() is None,
            "logFiles": [str(path) for path in selected],
            "retention": "redacted logs only; no SQLite, Credential payload, or runtime Workspace dump",
        }

    def _release_state(self) -> None:
        self.cursor_key = ""
        self.credential_key = ""
        if self._temporary_state:
            shutil.rmtree(self.state_dir, ignore_errors=True)

    def _tool_environment(self) -> dict[str, str]:
        allowed = (
            "PATH",
            "HOME",
            "TMPDIR",
            "GOCACHE",
            "GOMODCACHE",
            "GOPATH",
            "GOROOT",
            "DOCKER_HOST",
            "DOCKER_CONTEXT",
            "DOCKER_CONFIG",
            "XDG_RUNTIME_DIR",
        )
        environment = {key: os.environ[key] for key in allowed if key in os.environ}
        environment.setdefault("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")
        return environment

    def provision_target(
        self,
        tenant_id: str,
        organization_id: str,
        _provider: str,
    ) -> Mapping[str, Any]:
        targets = json_items(
            self.api.request("GET", f"/v1/tenants/{tenant_id}/execution-targets"),
            "execution-targets",
        )
        candidates = [
            target
            for target in targets
            if target.get("kind") == "local"
            and target.get("name") == "local-default"
            and target.get("status") == "active"
            and target.get("organizationId") == organization_id
        ]
        if len(candidates) != 1:
            raise AcceptanceError(
                "runner.local_default_target_missing",
                "Expected exactly one active local-default Target in the bootstrap Organization.",
                {"candidateCount": len(candidates), "targets": [AcceptanceSuite._target_summary(item) for item in targets]},
            )
        return candidates[0]

    def replace_worker(
        self,
        _tenant_id: str,
        _target_id: str,
        _provider: str,
    ) -> Mapping[str, Any]:
        raise AcceptanceError(
            "runner.target_replacement_unsupported",
            "The Local Target uses Control Plane restart for Supervisor recovery instead of container replacement.",
        )

    def observe_execution(self, _target_id: str, _execution_id: str) -> Mapping[str, Any]:
        return {}

    def observe_terminal_execution(self, _target_id: str, _execution_id: str) -> Mapping[str, Any]:
        return {}

    def recover_pending_interaction(self, _target_id: str, _execution_id: str) -> Mapping[str, Any]:
        raise AcceptanceError(
            "runner.pending_interaction_recovery_unsupported",
            f"The {self.name} TargetDriver does not support pending interaction runtime recovery.",
            {"target": self.name},
        )

    def _control_plane_environment(self) -> dict[str, str]:
        environment = self._tool_environment()
        environment.update(
            {
                "SYNARA_DEPLOYMENT_PROFILE": "personal",
                "SYNARA_METADATA_STORE": "sqlite",
                "SYNARA_ARTIFACT_STORE": "local",
                "SYNARA_QUEUE_DRIVER": "in-process",
                "SYNARA_CONTROL_PLANE_LISTEN": f"127.0.0.1:{self.port}",
                "SYNARA_PUBLIC_CONTROL_PLANE_URL": self.base_url,
                "SYNARA_SQLITE_PATH": str(self.state_dir / "metadata.sqlite"),
                "SYNARA_ARTIFACT_LOCAL_PATH": str(self.state_dir / "artifacts"),
                "SYNARA_INSTALLATION_ID": self.installation_id,
                "SYNARA_CONTROL_PLANE_DEV_BOOTSTRAP": "true",
                "SYNARA_LOGIN_COOKIE_NAME": self.cookie_name,
                "SYNARA_LOGIN_COOKIE_SECURE": "false",
                "SYNARA_PROVIDER_CURSOR_KEY": self.cursor_key,
                "SYNARA_CREDENTIAL_KMS_PROVIDER": "local",
                "SYNARA_CREDENTIAL_KMS_KEY_ID": "stage3-acceptance-local-v1",
                "SYNARA_CREDENTIAL_MASTER_KEY": self.credential_key,
                "SYNARA_LOCAL_AGENTD_RUNNER_COMMAND_JSON": json.dumps(self.options.runner_command),
                "SYNARA_LOCAL_AGENTD_WORKSPACE_ROOT": str(self.state_dir / "workspaces"),
                "SYNARA_LOCAL_AGENTD_GIT_CACHE_ROOT": str(self.state_dir / "git-cache"),
                "SYNARA_LOCAL_AGENTD_RESTART_BACKOFF": "250ms",
                "SYNARA_CONTROL_PLANE_SHUTDOWN_TIMEOUT": "4s",
                "SYNARA_WORKER_LEASE_TTL": "6s",
                "SYNARA_WORKER_HEARTBEAT_TIMEOUT": "18s",
                "SYNARA_OUTBOX_POLL_INTERVAL": "100ms",
                "SYNARA_OUTBOX_CLAIM_TTL": "5s",
                "SYNARA_RETENTION_SWEEP_INTERVAL": "24h",
            }
        )
        return environment


class ManagedWorkerDriver(LocalDriver):
    def __init__(
        self,
        repo_root: pathlib.Path,
        options: RunnerOptions,
        deadline: Deadline,
        redactor: SecretRedactor,
    ) -> None:
        super().__init__(repo_root, options, deadline, redactor)
        self.registration_token = random_key()
        self.redactor.add(self.registration_token, "[REDACTED_WORKER_REGISTRATION_TOKEN]")
        self.worker_proxy: _WorkerOnlyProxy | None = None

    @property
    def worker_proxy_host(self) -> str:
        raise NotImplementedError

    def start(self) -> Mapping[str, Any]:
        control_plane = super().start()
        if self.worker_proxy is None:
            try:
                self.worker_proxy = _WorkerOnlyProxy(self.port)
                self.worker_proxy.start()
            except Exception:
                self.worker_proxy = None
                super().stop()
                raise
        elif not self.worker_proxy.thread.is_alive():
            raise AcceptanceError(
                "runner.worker_proxy_exited",
                "Worker-only proxy exited before the acceptance run completed.",
            )
        return {
            **control_plane,
            "workerProxy": self.worker_proxy.evidence(self.worker_proxy_host),
        }

    def _control_plane_environment(self) -> dict[str, str]:
        environment = super()._control_plane_environment()
        for key in (
            "SYNARA_LOCAL_AGENTD_RUNNER_COMMAND_JSON",
            "SYNARA_LOCAL_AGENTD_WORKSPACE_ROOT",
            "SYNARA_LOCAL_AGENTD_GIT_CACHE_ROOT",
            "SYNARA_LOCAL_AGENTD_RESTART_BACKOFF",
        ):
            environment.pop(key, None)
        environment.update(
            {
                "SYNARA_CONTROL_PLANE_LISTEN": f"127.0.0.1:{self.port}",
                "SYNARA_WORKER_REGISTRATION_TOKEN": self.registration_token,
            }
        )
        return environment

    def _worker_proxy_url(self) -> str:
        if self.worker_proxy is None or not self.worker_proxy.thread.is_alive():
            raise AcceptanceError(
                "runner.worker_proxy_unavailable",
                "Worker-only proxy was unavailable while provisioning the managed Target.",
            )
        return f"http://{self.worker_proxy_host}:{self.worker_proxy.port}"

    def _stop_worker_proxy(self) -> None:
        proxy = self.worker_proxy
        self.worker_proxy = None
        if proxy is not None:
            proxy.stop()

    def inject_failure(
        self,
        fault: str,
        _target_id: str,
        _execution_id: str,
    ) -> Mapping[str, Any]:
        if fault != "worker-network":
            raise AcceptanceUnsupported(
                "runner.failure_case_unsupported",
                f"The {self.name} Target does not implement failure injection {fault}.",
                {"target": self.name, "failureCase": fault},
            )
        proxy = self.worker_proxy
        if proxy is None or not proxy.thread.is_alive():
            raise AcceptanceError(
                "runner.worker_proxy_unavailable",
                "The Worker-only proxy was unavailable for network fault injection.",
            )
        before = proxy.interrupted_request_count()
        proxy.interrupt_transport()
        started = time.monotonic()
        try:
            self.deadline.sleep(self.options.network_outage_seconds)
        finally:
            proxy.resume_transport()
        return {
            "fault": fault,
            "transport": "runner-owned Worker-only proxy",
            "durationMs": elapsed_ms(started),
            "droppedRequests": proxy.interrupted_request_count() - before,
            "controlPlaneUserApiInterrupted": False,
        }

    def validate_failure(self, fault: str) -> None:
        if fault == "worker-network":
            return
        raise AcceptanceUnsupported(
            "runner.failure_case_unsupported",
            f"The {self.name} Target does not implement failure injection {fault}.",
            {"target": self.name, "failureCase": fault},
        )

    def _head_sha(self) -> str:
        completed = subprocess.run(
            ["git", "rev-parse", "HEAD"],
            cwd=self.repo_root,
            env=self._tool_environment(),
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            timeout=10.0,
            check=False,
        )
        value = completed.stdout.strip().lower()
        if completed.returncode != 0 or re.fullmatch(r"(?:[0-9a-f]{40}|[0-9a-f]{64})", value) is None:
            raise AcceptanceError("runner.git_sha_unavailable", "The full source Git SHA could not be resolved.")
        return value

    def _source_version(self) -> str:
        try:
            value = json.loads((self.repo_root / "apps" / "server" / "package.json").read_text(encoding="utf-8"))[
                "version"
            ]
        except (OSError, KeyError, TypeError, json.JSONDecodeError) as error:
            raise AcceptanceError(
                "runner.source_version_unavailable",
                f"The Worker source version could not be resolved: {self.redactor.text(str(error))}",
            ) from None
        if not isinstance(value, str) or not value.strip():
            raise AcceptanceError("runner.source_version_unavailable", "The Worker source version is empty.")
        return value.strip()

    def _source_date_epoch(self, git_sha: str) -> str:
        completed = subprocess.run(
            ["git", "show", "-s", "--format=%ct", git_sha],
            cwd=self.repo_root,
            env=self._tool_environment(),
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            timeout=10.0,
            check=False,
        )
        value = completed.stdout.strip()
        if completed.returncode != 0 or re.fullmatch(r"(?:0|[1-9][0-9]*)", value) is None:
            raise AcceptanceError("runner.source_date_unavailable", "The source commit timestamp could not be resolved.")
        return value

    def _docker_environment(self) -> dict[str, str]:
        environment = self._tool_environment()
        environment.pop("DOCKER_CONTEXT", None)
        environment["DOCKER_HOST"] = f"unix://{self.options.docker_socket_path}"
        return environment

    def _docker_completed(
        self,
        arguments: Sequence[str],
        *,
        cleanup_timeout: float | None = None,
    ) -> subprocess.CompletedProcess[str]:
        timeout = cleanup_timeout
        if timeout is None:
            timeout = self.deadline.request_timeout(maximum=max(10.0, self.deadline.remaining()))
        try:
            return subprocess.run(
                ["docker", *arguments],
                cwd=self.repo_root,
                env=self._docker_environment(),
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                text=True,
                timeout=timeout,
                check=False,
            )
        except (OSError, subprocess.TimeoutExpired) as error:
            raise AcceptanceError(
                "runner.docker_command_failed",
                f"Docker command could not run: {self.redactor.text(str(error))}",
                {"command": ["docker", *arguments[:3]]},
            ) from None

    def _docker_command(
        self,
        arguments: Sequence[str],
        *,
        log_path: pathlib.Path | None = None,
        maximum_timeout: float | None = None,
    ) -> str:
        timeout = self.deadline.request_timeout(maximum=maximum_timeout or 30.0)
        try:
            completed = subprocess.run(
                ["docker", *arguments],
                cwd=self.repo_root,
                env=self._docker_environment(),
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                text=True,
                timeout=timeout,
                check=False,
            )
        except (OSError, subprocess.TimeoutExpired) as error:
            raise AcceptanceError(
                "runner.docker_command_failed",
                f"Docker command could not run: {self.redactor.text(str(error))}",
                {"command": ["docker", *arguments[:3]]},
            ) from None
        output = self.redactor.text(completed.stdout)
        if log_path is not None:
            log_path.parent.mkdir(parents=True, exist_ok=True)
            log_path.write_text(output, encoding="utf-8")
        if completed.returncode != 0:
            raise AcceptanceError(
                "runner.docker_command_failed",
                f"Docker command exited with status {completed.returncode}.",
                {
                    "command": ["docker", *arguments[:3]],
                    "exitCode": completed.returncode,
                    "log": str(log_path) if log_path else None,
                    "outputExcerpt": output[-1000:],
                },
            )
        return output

    def _worker_build_command(
        self,
        arguments: Sequence[str],
        *,
        log_path: pathlib.Path,
        maximum_timeout: float,
    ) -> str:
        command = [str(self.repo_root / "deploy" / "worker" / "build.sh"), *arguments]
        timeout = self.deadline.request_timeout(maximum=maximum_timeout)
        try:
            completed = subprocess.run(
                command,
                cwd=self.repo_root,
                env=self._docker_environment(),
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                text=True,
                timeout=timeout,
                check=False,
            )
        except (OSError, subprocess.TimeoutExpired) as error:
            raise AcceptanceError(
                "runner.worker_build_failed",
                f"The official Worker image build could not run: {self.redactor.text(str(error))}",
                {"command": command[:4]},
            ) from None
        output = self.redactor.text(completed.stdout)
        log_path.parent.mkdir(parents=True, exist_ok=True)
        log_path.write_text(output, encoding="utf-8")
        if completed.returncode != 0:
            raise AcceptanceError(
                "runner.worker_build_failed",
                f"The official Worker image build exited with status {completed.returncode}.",
                {"command": command[:4], "exitCode": completed.returncode, "log": str(log_path)},
            )
        return output

    def _ping_socket(self) -> Mapping[str, Any]:
        path = self.options.docker_socket_path
        if not path.is_absolute():
            raise AcceptanceError(
                "runner.docker_socket_invalid",
                "Docker socket path must be absolute.",
                {"path": str(path)},
            )
        try:
            with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as client:
                client.settimeout(self.deadline.request_timeout(maximum=5.0))
                client.connect(str(path))
                client.sendall(b"GET /_ping HTTP/1.1\r\nHost: docker\r\nConnection: close\r\n\r\n")
                chunks: list[bytes] = []
                while True:
                    chunk = client.recv(4096)
                    if not chunk:
                        break
                    chunks.append(chunk)
                response = b"".join(chunks)
        except OSError as error:
            raise AcceptanceError(
                "runner.docker_socket_unavailable",
                f"Docker socket is unavailable: {self.redactor.text(str(error))}",
                {"path": str(path)},
            ) from None
        if b"200 OK" not in response or not response.rstrip().endswith(b"OK"):
            raise AcceptanceError(
                "runner.docker_socket_unhealthy",
                "Docker Engine did not return a successful _ping response.",
                {"path": str(path), "responseExcerpt": response[:200].decode("ascii", errors="replace")},
            )
        return {"path": str(path), "ping": "OK"}

    def _prepare_worker_image(
        self,
        image: str,
        *,
        skip_build: bool,
        log_prefix: str,
    ) -> Mapping[str, Any]:
        socket_evidence = self._ping_socket()
        version = self._docker_command(
            ["version", "--format", "{{.Server.Version}}"],
            log_path=self.logs_dir / f"{log_prefix}-docker-version.log",
        ).strip()
        if not version:
            raise AcceptanceError("runner.docker_engine_unavailable", "Docker did not report a Server version.")
        build_started = time.monotonic()
        if skip_build:
            self._docker_command(
                ["image", "inspect", "--format", "{{.Id}}", image],
                log_path=self.logs_dir / f"{log_prefix}-image-inspect.log",
            )
            build_evidence: dict[str, Any] = {"build": "skipped"}
        else:
            git_sha = self._head_sha()
            metadata_path = self.logs_dir / f"{log_prefix}-worker-build-metadata.json"
            self._worker_build_command(
                [
                    "--target",
                    "worker-acceptance",
                    "--image",
                    image,
                    "--version",
                    self._source_version(),
                    "--git-sha",
                    git_sha,
                    "--source-date-epoch",
                    self._source_date_epoch(git_sha),
                    "--metadata-file",
                    str(metadata_path),
                    "--label",
                    "synara.io/stage3-provider-acceptance=true",
                    "--label",
                    f"synara.io/stage3-provider-acceptance-owner={self.resource_owner}",
                    "--label",
                    f"org.opencontainers.image.revision={git_sha}",
                    "--allow-dirty",
                    "--load",
                ],
                log_path=self.logs_dir / f"{log_prefix}-worker-build.log",
                maximum_timeout=max(60.0, self.deadline.remaining()),
            )
            build_evidence = {
                "build": "completed",
                "durationMs": elapsed_ms(build_started),
                "metadata": str(metadata_path),
            }
        self._docker_command(
            [
                "run",
                "--rm",
                "--entrypoint",
                "sh",
                image,
                "-c",
                "test -x /usr/local/bin/synara-agentd && "
                "test -x /usr/local/bin/provider-host && "
                "test -r /opt/synara/acceptance/provider-host-fixture.mjs && "
                "test -r /opt/synara/worker-image-manifest.json && "
                "test -r /opt/synara/provider-tools.spdx.json && "
                "test -r /opt/synara/provider-tools/package-lock.json && "
                "test -r /opt/synara/provider-host/bun.lock && "
                "test -r /opt/synara/worker-apk-packages.lock && "
                "test \"$(id -u)\" = 10001 && "
                "node --version && codex --version && claude --version",
            ],
            log_path=self.logs_dir / f"{log_prefix}-worker-smoke.log",
        )
        image_id = self._docker_command(["image", "inspect", "--format", "{{.Id}}", image]).strip()
        return {
            "serverVersion": version,
            "socket": socket_evidence,
            "workerImage": image,
            "workerImageId": image_id,
            **build_evidence,
        }

    def _worker_identity(self, target_id: str, *, required: bool = True) -> dict[str, Any] | None:
        database_path = self.state_dir / "metadata.sqlite"
        try:
            connection = sqlite3.connect(f"file:{database_path}?mode=ro", uri=True, timeout=2.0)
            try:
                row = connection.execute(
                    """
                    SELECT id, incarnation, instance_uid, status, pod_name
                    FROM worker_instances
                    WHERE execution_target_id = ? AND terminated_at IS NULL
                    ORDER BY registered_at DESC, id
                    LIMIT 1
                    """,
                    (target_id,),
                ).fetchone()
            finally:
                connection.close()
        except sqlite3.Error as error:
            if not required:
                return None
            raise AcceptanceError(
                "runner.worker_identity_query_failed",
                f"Worker identity could not be read from the isolated metadata store: {error}",
            ) from None
        if row is None:
            if not required:
                return None
            raise AcceptanceError("runner.worker_identity_missing", "The managed Worker identity was missing.")
        return {
            "id": str(row[0]),
            "incarnation": int(row[1]),
            "instanceUid": str(row[2]),
            "status": str(row[3]),
            "podName": str(row[4]),
        }


class DockerDriver(ManagedWorkerDriver):
    name = "docker"
    lifecycle = STANDING_MANAGED_WORKER
    replacement_workspace_semantics = "persisted named-volume Workspace content; not Workspace Checkpoint restore"

    def __init__(
        self,
        repo_root: pathlib.Path,
        options: RunnerOptions,
        deadline: Deadline,
        redactor: SecretRedactor,
    ) -> None:
        super().__init__(repo_root, options, deadline, redactor)
        suffix = uuid.uuid4().hex[:12]
        self.target_name = f"stage3-docker-{suffix}"
        self.volume_name = f"synara-stage3-{suffix}"
        self.network_name = options.docker_network_mode or f"synara-stage3-{suffix}"
        self.owns_network = options.docker_network_mode is None
        self.owns_image = not options.docker_skip_worker_build
        self.head_sha = self._head_sha()
        self.image = options.docker_worker_image or f"synara-stage3-provider-acceptance:{self.head_sha}-{suffix}"
        self.target_id: str | None = None
        self.container_name: str | None = None

    @property
    def worker_proxy_host(self) -> str:
        return self.options.docker_control_plane_host

    def prepare(self) -> Mapping[str, Any]:
        control_plane = super().prepare()
        image_evidence = self._prepare_worker_image(
            self.image,
            skip_build=self.options.docker_skip_worker_build,
            log_prefix="docker",
        )
        if self.owns_network:
            self._docker_command(
                [
                    "network",
                    "create",
                    "--label",
                    "synara.io/stage3-provider-acceptance=true",
                    "--label",
                    f"synara.io/stage3-provider-acceptance-owner={self.resource_owner}",
                    self.network_name,
                ],
                log_path=self.logs_dir / "docker-network-create.log",
            )
        else:
            self._docker_command(["network", "inspect", self.network_name])
        self._docker_command(
            [
                "volume",
                "create",
                "--label",
                "synara.io/stage3-provider-acceptance=true",
                "--label",
                f"synara.io/stage3-provider-acceptance-owner={self.resource_owner}",
                self.volume_name,
            ],
            log_path=self.logs_dir / "docker-volume-create.log",
        )
        return {
            "controlPlane": control_plane,
            "docker": {
                "networkMode": self.network_name,
                "workspaceVolume": self.volume_name,
                "resourceOwner": self.resource_owner,
                **image_evidence,
            },
        }

    def provision_target(
        self,
        tenant_id: str,
        organization_id: str,
        provider: str,
    ) -> Mapping[str, Any]:
        target = json_object(
            self.api.request(
                "POST",
                f"/v1/tenants/{tenant_id}/execution-targets",
                {
                    "organizationId": organization_id,
                    "kind": "docker",
                    "name": self.target_name,
                    "configuration": {
                        "socketPath": str(self.options.docker_socket_path),
                        "image": self.image,
                        "pullPolicy": "never",
                        "controlPlaneUrl": self._worker_proxy_url(),
                        "allowInsecureControlPlane": True,
                        "runnerCommand": list(self.options.runner_command),
                        "desiredWorkers": 1,
                        "workspaceVolume": self.volume_name,
                        "workspaceMount": "/data",
                        "workspaceRoot": "/data/workspaces",
                        "gitCacheRoot": "/data/git-cache",
                        "networkMode": self.network_name,
                        "user": "10001:10001",
                        "memoryBytes": self.options.docker_memory_bytes,
                        "nanoCpus": self.options.docker_nano_cpus,
                    },
                    "capabilities": {
                        "workspaceModes": ["local", "worktree"],
                        "providerPolicy": {"experimentalProviders": [provider]},
                    },
                },
                expected=(201,),
            ),
            "docker execution target",
        )
        target_id = target.get("id")
        if not isinstance(target_id, str) or not target_id:
            raise AcceptanceError("runner.docker_target_id_missing", "Docker Target creation did not return an ID.")
        self.target_id = target_id
        self.container_name = f"synara-agentd-{target_id}-0"
        snapshot = self._wait_container(target_id)
        evidence = self._validate_container(snapshot)
        return {**target, "driverEvidence": evidence}

    def replace_worker(
        self,
        tenant_id: str,
        target_id: str,
        _provider: str,
    ) -> Mapping[str, Any]:
        before_container = self._wait_container(target_id)
        before_worker = self._worker_identity(target_id)
        before_container_id = str(before_container.get("Id") or "")
        if not before_container_id:
            raise AcceptanceError(
                "runner.docker_container_id_missing",
                "Managed Docker Worker inspect omitted its container ID.",
            )
        self._write_volume_sentinel(before_container_id)
        self.api.request(
            "PATCH",
            f"/v1/tenants/{tenant_id}/execution-targets/{target_id}/provider-policy",
            {"experimentalProviders": ["codex", "claudeAgent"]},
        )

        def replacement_probe() -> tuple[dict[str, Any], dict[str, Any]] | None:
            container = self._container_snapshot(target_id)
            if container is None or container.get("Id") == before_container.get("Id"):
                return None
            worker = self._worker_identity(target_id, required=False)
            if worker is None:
                return None
            if (
                worker["id"] != before_worker["id"]
                or worker["incarnation"] <= before_worker["incarnation"]
                or worker["instanceUid"] == before_worker["instanceUid"]
                or worker["status"] != "online"
            ):
                return None
            return container, worker

        after_container, after_worker = self.api.wait_until(
            "Docker Worker container replacement and new agentd incarnation",
            replacement_probe,
        )
        resources = self._validate_container(after_container)
        after_container_id = str(after_container.get("Id") or "")
        self._verify_volume_sentinel(after_container_id)
        return {
            "strategy": "provider-policy config-hash replacement",
            "containerIdChanged": after_container.get("Id") != before_container.get("Id"),
            "previousContainerId": before_container_id[:12],
            "replacementContainerId": after_container_id[:12],
            "replacementWorkerId": after_worker["id"],
            "workerIdStable": after_worker["id"] == before_worker["id"],
            "previousIncarnation": before_worker["incarnation"],
            "replacementIncarnation": after_worker["incarnation"],
            "instanceUidChanged": after_worker["instanceUid"] != before_worker["instanceUid"],
            "namedVolumeContinuity": {
                "sentinelPath": DOCKER_VOLUME_SENTINEL_PATH,
                "preservedAcrossReplacement": True,
                "semantics": "named-volume content continuity; not Workspace Checkpoint restore",
            },
            "resources": resources,
        }

    def inject_failure(
        self,
        fault: str,
        target_id: str,
        _execution_id: str,
    ) -> Mapping[str, Any]:
        if fault != "worker-network":
            return super().inject_failure(fault, target_id, _execution_id)
        if not self.owns_network and not self.options.docker_allow_network_interruption:
            raise AcceptanceUnsupported(
                "runner.docker_network_fault_not_authorized",
                "Docker network interruption is disabled for an operator-owned network.",
                {
                    "network": self.network_name,
                    "requiredInputs": ["--docker-allow-network-interruption"],
                },
            )
        snapshot = self._wait_container(target_id)
        container_id = str(snapshot.get("Id") or "")
        if not container_id:
            raise AcceptanceError(
                "runner.docker_container_id_missing",
                "The managed Docker Worker omitted its container ID before network interruption.",
            )
        before_worker = self._worker_identity(target_id)
        started = time.monotonic()
        disconnected = False
        try:
            self._docker_command(["network", "disconnect", self.network_name, container_id])
            disconnected = True
            self.deadline.sleep(self.options.network_outage_seconds)
        finally:
            if disconnected:
                completed = self._docker_completed(
                    ["network", "connect", self.network_name, container_id],
                    cleanup_timeout=15.0,
                )
                output = self.redactor.text(completed.stdout)
                if completed.returncode != 0 and "no such container" not in output.lower():
                    raise AcceptanceError(
                        "runner.docker_network_restore_failed",
                        "The managed Docker Worker network could not be restored.",
                        {"outputExcerpt": output[-1000:]},
                    )
        return {
            "fault": fault,
            "network": self.network_name,
            "networkOwnedByRunner": self.owns_network,
            "containerId": container_id[:12],
            "workerId": before_worker.get("id") if before_worker else None,
            "durationMs": elapsed_ms(started),
            "restored": True,
        }

    def validate_failure(self, fault: str) -> None:
        if fault != "worker-network":
            return super().validate_failure(fault)
        if not self.owns_network and not self.options.docker_allow_network_interruption:
            raise AcceptanceUnsupported(
                "runner.docker_network_fault_not_authorized",
                "Docker network interruption is disabled for an operator-owned network.",
                {
                    "network": self.network_name,
                    "requiredInputs": ["--docker-allow-network-interruption"],
                },
            )

    def collect_failure_diagnostics(self, case_id: str) -> Mapping[str, Any]:
        evidence = dict(super().collect_failure_diagnostics(case_id))
        if self.target_id:
            completed = self._docker_completed(
                [
                    "ps",
                    "-a",
                    "--filter",
                    f"label=synara.io/execution-target-id={self.target_id}",
                    "--format",
                    "{{.ID}} {{.Status}} {{.Names}}",
                ],
                cleanup_timeout=5.0,
            )
            evidence["managedContainers"] = self.redactor.text(completed.stdout).splitlines()[:5]
        return evidence

    def cleanup(self) -> Mapping[str, Any]:
        errors: list[str] = []

        def collect_failure(operation: str, action: Callable[[], Any]) -> Any:
            try:
                return action()
            except Exception as error:
                errors.append(f"{operation}: {error}")
                return None

        def docker_cleanup(
            operation: str,
            arguments: Sequence[str],
            timeout: float,
            ignored_output: Sequence[str] = (),
        ) -> subprocess.CompletedProcess[str] | None:
            completed = collect_failure(
                operation,
                lambda: self._docker_completed(arguments, cleanup_timeout=timeout),
            )
            if not isinstance(completed, subprocess.CompletedProcess):
                return None
            output = completed.stdout.strip()
            if completed.returncode != 0 and not any(
                ignored.lower() in output.lower() for ignored in ignored_output
            ):
                errors.append(f"{operation}: {output or f'exit {completed.returncode}'}")
            return completed

        def remove_managed_workers_until_quiet() -> None:
            if not self.target_id:
                return
            cleanup_deadline = time.monotonic() + 12.0
            quiet_since: float | None = None
            while time.monotonic() < cleanup_deadline:
                listed = docker_cleanup(
                    "list managed Worker containers",
                    [
                        "ps",
                        "-aq",
                        "--filter",
                        "label=synara.io/managed=true",
                        "--filter",
                        f"label=synara.io/execution-target-id={self.target_id}",
                    ],
                    5.0,
                )
                if listed is None or listed.returncode != 0:
                    return
                container_ids = [line.strip() for line in listed.stdout.splitlines() if line.strip()]
                if container_ids:
                    quiet_since = None
                    removed = docker_cleanup(
                        "remove managed Worker containers",
                        ["rm", "-f", *container_ids],
                        10.0,
                    )
                    if removed is None or removed.returncode != 0:
                        return
                else:
                    now = time.monotonic()
                    quiet_since = quiet_since or now
                    if now - quiet_since >= 1.0:
                        return
                time.sleep(0.1)
            errors.append("managed Worker containers did not remain absent during cleanup")

        collect_failure("stop Control Plane", self.stop)
        remove_managed_workers_until_quiet()
        if not self.options.keep:
            volume_owned = collect_failure(
                "verify named Workspace volume ownership",
                lambda: self._assert_docker_resource_owner("volume", self.volume_name),
            )
            if volume_owned is True:
                docker_cleanup(
                    "remove named Workspace volume",
                    ["volume", "rm", "-f", self.volume_name],
                    20.0,
                    ("no such volume",),
                )
            if self.owns_network:
                network_owned = collect_failure(
                    "verify acceptance network ownership",
                    lambda: self._assert_docker_resource_owner("network", self.network_name),
                )
                if network_owned is True:
                    docker_cleanup(
                        "remove acceptance network",
                        ["network", "rm", self.network_name],
                        20.0,
                        ("not found",),
                    )
            if self.owns_image:
                docker_cleanup(
                    "remove acceptance image",
                    ["image", "rm", "-f", self.image],
                    30.0,
                    ("no such image",),
                )
        collect_failure("stop Worker-only proxy", self._stop_worker_proxy)
        self.registration_token = ""
        collect_failure("release isolated state", self._release_state)
        if errors:
            raise AcceptanceError(
                "runner.docker_cleanup_failed",
                "Docker acceptance resources could not be cleaned completely.",
                {"errors": [self.redactor.text(value) for value in errors if value]},
            )
        return {
            "target": self.name,
            "resourceOwner": self.resource_owner,
            "managedWorkerContainersRemoved": True,
            "workspaceVolume": self.volume_name,
            "workspaceVolumeRemoved": not self.options.keep,
            "network": self.network_name,
            "networkOwnedByRunner": self.owns_network,
            "ownedNetworkRemoved": self.owns_network and not self.options.keep,
            "workerImage": self.image,
            "ownedImageRemoved": self.owns_image and not self.options.keep,
            "stateRemoved": self._temporary_state,
            "broadCleanupUsed": False,
        }

    def _assert_docker_resource_owner(self, resource: str, name: str) -> bool:
        completed = self._docker_completed(
            [
                resource,
                "inspect",
                "--format",
                "{{ index .Labels \"synara.io/stage3-provider-acceptance-owner\" }}",
                name,
            ],
            cleanup_timeout=5.0,
        )
        output = self.redactor.text(completed.stdout).strip()
        if completed.returncode != 0:
            if (
                "no such" in output.lower()
                or "not found" in output.lower()
                or "notfound" in output.lower()
            ):
                return True
            raise AcceptanceError(
                "runner.docker_ownership_check_failed",
                f"Docker {resource} ownership could not be verified.",
                {"resource": resource, "name": name, "outputExcerpt": output[-500:]},
            )
        if output != self.resource_owner:
            raise AcceptanceError(
                "runner.docker_resource_not_owned",
                f"Refusing to delete a Docker {resource} without the acceptance ownership label.",
                {"resource": resource, "name": name},
            )
        return True

    def _control_plane_environment(self) -> dict[str, str]:
        environment = super()._control_plane_environment()
        environment["SYNARA_DOCKER_RECONCILE_INTERVAL"] = "250ms"
        return environment

    def _write_volume_sentinel(self, container_id: str) -> None:
        self._docker_command(
            [
                "exec",
                container_id,
                "sh",
                "-c",
                "set -eu; path='" + DOCKER_VOLUME_SENTINEL_PATH + "'; "
                "test ! -e \"$path\"; test ! -L \"$path\"; umask 077; "
                "printf '%s\\n' '" + DOCKER_VOLUME_SENTINEL_VALUE + "' > \"$path\"",
            ]
        )

    def _verify_volume_sentinel(self, container_id: str) -> None:
        if not container_id:
            raise AcceptanceError(
                "runner.docker_container_id_missing",
                "Replacement Docker Worker inspect omitted its container ID.",
            )
        self._docker_command(
            [
                "exec",
                container_id,
                "sh",
                "-c",
                "set -eu; path='" + DOCKER_VOLUME_SENTINEL_PATH + "'; "
                "test ! -L \"$path\"; test -f \"$path\"; "
                "test \"$(cat \"$path\")\" = '" + DOCKER_VOLUME_SENTINEL_VALUE + "'",
            ]
        )


    def _container_snapshot(self, target_id: str) -> dict[str, Any] | None:
        output = self._docker_command(
            [
                "ps",
                "-aq",
                "--filter",
                "label=synara.io/managed=true",
                "--filter",
                f"label=synara.io/execution-target-id={target_id}",
            ]
        )
        container_ids = [line.strip() for line in output.splitlines() if line.strip()]
        if not container_ids:
            return None
        if len(container_ids) != 1:
            raise AcceptanceError(
                "runner.docker_container_count_invalid",
                "Expected exactly one managed Docker Worker container.",
                {"targetId": target_id, "containerCount": len(container_ids)},
            )
        completed = self._docker_completed(["inspect", container_ids[0]])
        output = self.redactor.text(completed.stdout)
        if completed.returncode != 0:
            if "no such object" in output.lower() or "no such container" in output.lower():
                return None
            raise AcceptanceError(
                "runner.docker_command_failed",
                f"Docker inspect exited with status {completed.returncode}.",
                {
                    "command": ["docker", "inspect", container_ids[0]],
                    "exitCode": completed.returncode,
                    "outputExcerpt": output[-1000:],
                },
            )
        try:
            inspected = json.loads(output)
        except json.JSONDecodeError as error:
            raise AcceptanceError(
                "runner.docker_inspect_invalid",
                "Docker inspect returned invalid JSON.",
                {"message": str(error)},
            ) from None
        if not isinstance(inspected, list) or len(inspected) != 1 or not isinstance(inspected[0], dict):
            raise AcceptanceError("runner.docker_inspect_invalid", "Docker inspect returned an invalid payload.")
        return inspected[0]

    def _wait_container(self, target_id: str) -> dict[str, Any]:
        def probe() -> dict[str, Any] | None:
            snapshot = self._container_snapshot(target_id)
            if snapshot is None:
                return None
            state = snapshot.get("State")
            return snapshot if isinstance(state, dict) and state.get("Running") is True else None

        return self.api.wait_until("a running managed Docker Worker container", probe)

    def _validate_container(self, snapshot: Mapping[str, Any]) -> Mapping[str, Any]:
        config = json_object(snapshot.get("Config"), "docker inspect Config")
        host = json_object(snapshot.get("HostConfig"), "docker inspect HostConfig")
        mounts = snapshot.get("Mounts")
        if not isinstance(mounts, list):
            raise AcceptanceError("runner.docker_mounts_invalid", "Docker inspect Mounts was not an array.")
        volume = next(
            (
                item
                for item in mounts
                if isinstance(item, dict)
                and item.get("Type") == "volume"
                and item.get("Name") == self.volume_name
                and item.get("Destination") == "/data"
            ),
            None,
        )
        expected = {
            "user": "10001:10001",
            "memoryBytes": self.options.docker_memory_bytes,
            "nanoCpus": self.options.docker_nano_cpus,
            "networkMode": self.network_name,
        }
        actual = {
            "user": config.get("User"),
            "memoryBytes": host.get("Memory"),
            "nanoCpus": host.get("NanoCpus"),
            "networkMode": host.get("NetworkMode"),
        }
        if actual != expected or volume is None:
            raise AcceptanceError(
                "runner.docker_container_contract_mismatch",
                "Managed Docker Worker did not apply the requested isolation contract.",
                {"expected": expected, "actual": actual, "volumeMounted": volume is not None},
            )
        return {**actual, "volume": self.volume_name, "workspaceMount": "/data"}

class SSHDriver(ManagedWorkerDriver):
    name = "ssh"
    lifecycle = STANDING_MANAGED_WORKER
    replacement_workspace_semantics = (
        "persisted remote-filesystem Workspace content; not Workspace Checkpoint restore"
    )

    def __init__(
        self,
        repo_root: pathlib.Path,
        options: RunnerOptions,
        deadline: Deadline,
        redactor: SecretRedactor,
    ) -> None:
        super().__init__(repo_root, options, deadline, redactor)
        suffix = uuid.uuid4().hex[:12]
        self.target_name = f"stage3-ssh-{suffix}"
        self.machine_name = options.ssh_machine_name or f"synara-stage3-{suffix}"
        self.machine_create_attempted = False
        self.machine_created = False
        self.tenant_id: str | None = None
        self.target_id: str | None = None
        self.service_name: str | None = None
        self.machine_ip = ""
        self.host_key = ""
        self.client_private_key = ""
        self.client_public_key = ""
        self.credentials_dir = self.state_dir / "ssh-credentials"
        self.client_key_path = self.credentials_dir / "id_ed25519"
        self.client_public_key_path = self.credentials_dir / "id_ed25519.pub"
        self.known_hosts_path = self.credentials_dir / "known_hosts"
        self.agentd_binary_path = self.state_dir / "bin" / (
            f"synara-agentd-linux-{options.ssh_machine_arch}"
        )
        self.fixture_bundle_path = self.state_dir / "bin" / "provider-host-fixture.mjs"
        self.worker_proxy_relay_process: subprocess.Popen[str] | None = None
        self.worker_proxy_relay_log_handle: Any | None = None
        self.worker_proxy_relay_log_path = self.logs_dir / "ssh-worker-proxy-relay.log"
        self.worker_proxy_relay_port = 0

    @property
    def worker_proxy_host(self) -> str:
        return SSH_RELAY_LOOPBACK_HOST

    def prepare(self) -> Mapping[str, Any]:
        control_plane = super().prepare()
        artifacts = self._prepare_ssh_artifacts()
        credential = self._generate_client_key()
        machine = self._prepare_machine()
        return {
            "controlPlane": control_plane,
            "ssh": {
                **artifacts,
                **credential,
                **machine,
            },
        }

    def start(self) -> Mapping[str, Any]:
        control_plane = super().start()
        try:
            self._ensure_worker_proxy_relay()
        except Exception:
            try:
                self._stop_worker_proxy_relay()
            except Exception:
                pass
            try:
                self._stop_worker_proxy()
            except Exception:
                pass
            super().stop()
            raise
        return {
            **control_plane,
            "workerProxyRelay": self._worker_proxy_relay_evidence(),
        }

    def provision_target(
        self,
        tenant_id: str,
        organization_id: str,
        provider: str,
    ) -> Mapping[str, Any]:
        if not self.machine_ip or not self.host_key or not self.client_private_key or not self.client_public_key:
            raise AcceptanceError(
                "runner.ssh_runtime_unavailable",
                "The disposable SSH runtime was unavailable while provisioning the Target.",
            )
        self.tenant_id = tenant_id
        negative_evidence: dict[str, Any] = {}
        try:
            negative = self._create_ssh_target(
                tenant_id,
                organization_id,
                f"{self.target_name}-host-key-negative",
                self.client_public_key,
                provider,
            )
            negative_id = self._target_id(negative, "negative SSH execution target")
            try:
                self.api.request(
                    "POST",
                    f"/v1/tenants/{tenant_id}/execution-targets/{negative_id}/ssh/install",
                    maximum_timeout=SSH_CONTROL_PLANE_OPERATION_TIMEOUT,
                )
            except AcceptanceError as error:
                if error.code != "ssh_connection_failed":
                    raise
                negative_evidence = {
                    "targetId": negative_id,
                    "rejected": True,
                    "errorCode": error.code,
                }
            else:
                raise AcceptanceError(
                    "runner.ssh_host_key_mismatch_accepted",
                    "The managed SSH Target accepted a valid but incorrect pinned Host Key.",
                    {"targetId": negative_id},
                )
            self._assert_remote_target_absent(negative_id)

            target = self._create_ssh_target(
                tenant_id,
                organization_id,
                self.target_name,
                self.host_key,
                provider,
            )
            target_id = self._target_id(target, "SSH execution target")
            self.target_id = target_id
            installed = json_object(
                self.api.request(
                    "POST",
                    f"/v1/tenants/{tenant_id}/execution-targets/{target_id}/ssh/install",
                    maximum_timeout=SSH_CONTROL_PLANE_OPERATION_TIMEOUT,
                ),
                "SSH install result",
            )
            expected_service = f"synara-agentd-{target_id}.service"
            if (
                installed.get("targetId") != target_id
                or installed.get("operation") != "install"
                or installed.get("status") != "active"
                or installed.get("serviceName") != expected_service
                or not isinstance(installed.get("binarySha256"), str)
                or len(str(installed.get("binarySha256"))) != 64
            ):
                raise AcceptanceError(
                    "runner.ssh_install_contract_mismatch",
                    "SSH Target installation returned an invalid result.",
                    {"result": self.redactor.value(installed)},
                )
            self.service_name = expected_service
            service = self._require_service_active(expected_service)
            return {
                **target,
                "driverEvidence": {
                    "machineName": self.machine_name,
                    "machineAddress": self.machine_ip,
                    "hostKeyAlgorithm": self.host_key.split()[0],
                    "hostKeyFingerprint": self._host_key_fingerprint(self.host_key),
                    "hostKeyMismatch": negative_evidence,
                    "service": service,
                    "binarySha256": installed.get("binarySha256"),
                    "credentialSource": "runner-generated one-time Ed25519 key",
                    "controlPlaneTransport": self._worker_proxy_relay_evidence(),
                    "controlPlaneCredentialLifecycle": SSH_CREDENTIAL_LIFECYCLE,
                    "workerAllocation": self.lifecycle.worker_allocation,
                },
            }
        finally:
            self._discard_local_private_key()

    def replace_worker(
        self,
        tenant_id: str,
        target_id: str,
        _provider: str,
    ) -> Mapping[str, Any]:
        service_name = self.service_name
        if not service_name:
            raise AcceptanceError("runner.ssh_service_missing", "The managed SSH systemd service was unavailable.")
        before_worker = self._worker_identity(target_id)
        before_service = self._require_service_active(service_name)
        self._remote_command(
            ["systemctl", "restart", "ssh"],
            log_path=self.logs_dir / "ssh-sshd-restart.log",
        )
        sshd_state = self._remote_command(["systemctl", "is-active", "ssh"]).strip()
        if sshd_state != "active":
            raise AcceptanceError(
                "runner.sshd_restart_failed",
                "The disposable SSH daemon did not recover after restart.",
                {"activeState": sshd_state},
            )
        self.api.request(
            "PATCH",
            f"/v1/tenants/{tenant_id}/execution-targets/{target_id}/provider-policy",
            {"experimentalProviders": ["codex", "claudeAgent"]},
        )
        upgraded = json_object(
            self.api.request(
                "POST",
                f"/v1/tenants/{tenant_id}/execution-targets/{target_id}/ssh/upgrade",
                maximum_timeout=SSH_CONTROL_PLANE_OPERATION_TIMEOUT,
            ),
            "SSH upgrade result",
        )
        if (
            upgraded.get("targetId") != target_id
            or upgraded.get("operation") != "upgrade"
            or upgraded.get("status") != "active"
            or upgraded.get("serviceName") != service_name
        ):
            raise AcceptanceError(
                "runner.ssh_upgrade_contract_mismatch",
                "SSH Target upgrade returned an invalid result.",
                {"result": self.redactor.value(upgraded)},
            )

        def replacement_probe() -> tuple[dict[str, Any], dict[str, Any]] | None:
            worker = self._worker_identity(target_id, required=False)
            if worker is None:
                return None
            try:
                service = self._require_service_active(service_name)
            except AcceptanceError:
                return None
            if (
                worker["id"] != before_worker["id"]
                or worker["incarnation"] <= before_worker["incarnation"]
                or worker["instanceUid"] == before_worker["instanceUid"]
                or worker["status"] != "online"
                or service["mainPid"] == before_service["mainPid"]
            ):
                return None
            return worker, service

        after_worker, after_service = self.api.wait_until(
            "SSH Worker systemd replacement and new agentd incarnation",
            replacement_probe,
        )
        return {
            "strategy": "pinned-Host-Key SSH upgrade with systemd restart",
            "sshdRestarted": True,
            "serviceName": service_name,
            "previousMainPid": before_service["mainPid"],
            "replacementMainPid": after_service["mainPid"],
            "replacementWorkerId": after_worker["id"],
            "workerIdStable": after_worker["id"] == before_worker["id"],
            "previousIncarnation": before_worker["incarnation"],
            "replacementIncarnation": after_worker["incarnation"],
            "instanceUidChanged": after_worker["instanceUid"] != before_worker["instanceUid"],
            "hostKeyFingerprint": self._host_key_fingerprint(self.host_key),
            "remoteFilesystemContinuity": {
                "preservedAcrossReplacement": True,
                "semantics": self.replacement_workspace_semantics,
            },
        }

    def cleanup(self) -> Mapping[str, Any]:
        errors: list[str] = []

        def collect(operation: str, action: Callable[[], Any]) -> Any:
            try:
                return action()
            except Exception as error:
                errors.append(f"{operation}: {self.redactor.text(str(error))}")
                return None

        if self.machine_created and self.service_name:
            collect(
                "capture SSH Worker journal",
                lambda: self._remote_command(
                    ["journalctl", "--no-pager", "-u", self.service_name, "-n", "500"],
                    log_path=self.logs_dir / "ssh-agentd-journal.log",
                    cleanup_timeout=20.0,
                ),
            )
        if self.target_id and self.tenant_id and self.process is not None and self.process.poll() is None:
            result = collect(
                "revoke managed SSH Target",
                lambda: self.api.request(
                    "POST",
                    f"/v1/tenants/{self.tenant_id}/execution-targets/{self.target_id}/ssh/revoke",
                    maximum_timeout=SSH_CONTROL_PLANE_OPERATION_TIMEOUT,
                ),
            )
            if isinstance(result, dict) and (
                result.get("operation") != "revoke" or result.get("status") != "disabled"
            ):
                errors.append("revoke managed SSH Target: API returned an invalid result")
        if self.machine_created and self.target_id:
            collect(
                "verify revoked SSH Target files",
                lambda: self._assert_remote_target_absent(self.target_id, cleanup_timeout=20.0),
            )
        if self.machine_created:
            collect(
                "remove disposable SSH authorization",
                lambda: self._remote_command(
                    ["rm", "-f", "/root/.ssh/authorized_keys"],
                    cleanup_timeout=10.0,
                ),
            )
        collect("stop Worker proxy relay", self._stop_worker_proxy_relay)
        collect("stop Control Plane", self.stop)
        if self.machine_created and not self.options.keep:
            completed = collect(
                "delete disposable OrbStack machine",
                lambda: self._orbctl_completed(
                    ["delete", "--force", self.machine_name],
                    cleanup_timeout=60.0,
                ),
            )
            if isinstance(completed, subprocess.CompletedProcess) and completed.returncode != 0:
                output = self.redactor.text(completed.stdout)
                if "not found" not in output.lower() and "does not exist" not in output.lower():
                    errors.append(f"delete disposable OrbStack machine: {output or completed.returncode}")
                else:
                    self.machine_create_attempted = False
                    self.machine_created = False
            elif isinstance(completed, subprocess.CompletedProcess):
                self.machine_create_attempted = False
                self.machine_created = False
        self._discard_local_key_material()
        collect("stop Worker-only proxy", self._stop_worker_proxy)
        self.registration_token = ""
        collect("release isolated state", self._release_state)
        if errors:
            raise AcceptanceError(
                "runner.ssh_cleanup_failed",
                "SSH acceptance resources could not be cleaned completely.",
                {"errors": errors},
            )
        return {
            "target": self.name,
            "resourceOwner": self.resource_owner,
            "machineName": self.machine_name,
            "machineRemoved": not self.options.keep and not self.machine_created,
            "machinePreservedByRequest": self.options.keep,
            "productRevokeRequested": bool(self.target_id and self.tenant_id),
            "machineLifecycleCompleted": self.options.keep or not self.machine_created,
            "localKeyMaterialRemoved": not self.credentials_dir.exists(),
            "broadCleanupUsed": False,
        }

    def _control_plane_environment(self) -> dict[str, str]:
        environment = super()._control_plane_environment()
        environment["SYNARA_AGENTD_BINARY_PATH"] = str(self.agentd_binary_path)
        environment["SYNARA_SSH_PROVISION_TIMEOUT"] = "120s"
        return environment

    def _worker_proxy_url(self) -> str:
        self._ensure_worker_proxy_relay()
        return f"http://{SSH_RELAY_LOOPBACK_HOST}:{self.worker_proxy_relay_port}"

    def _worker_proxy_relay_evidence(self) -> Mapping[str, Any]:
        if self.worker_proxy is None or self.worker_proxy_relay_port <= 0:
            raise AcceptanceError(
                "runner.ssh_worker_proxy_relay_unavailable",
                "The runner-owned reverse SSH relay was unavailable.",
            )
        return {
            "mode": "reverse-ssh-loopback",
            "vmListenHost": SSH_RELAY_LOOPBACK_HOST,
            "vmListenPort": self.worker_proxy_relay_port,
            "upstreamAddress": f"127.0.0.1:{self.worker_proxy.port}",
            "readsUserSSHConfiguration": False,
            "log": str(self.worker_proxy_relay_log_path),
        }

    def _ensure_worker_proxy_relay(self) -> None:
        if self.worker_proxy is None or not self.worker_proxy.thread.is_alive():
            raise AcceptanceError(
                "runner.worker_proxy_unavailable",
                "Worker-only proxy was unavailable while preparing the SSH relay.",
            )
        process = self.worker_proxy_relay_process
        if process is not None:
            if process.poll() is None:
                return
            exit_code = process.returncode
            self._close_worker_proxy_relay_log_handle()
            self.worker_proxy_relay_process = None
            self.worker_proxy_relay_port = 0
            raise AcceptanceError(
                "runner.ssh_worker_proxy_relay_exited",
                "The runner-owned reverse SSH relay exited before the acceptance run completed.",
                {"log": str(self.worker_proxy_relay_log_path), "exitCode": exit_code},
            )
        self._start_worker_proxy_relay()

    def _start_worker_proxy_relay(self) -> None:
        if not self.machine_created or not self.machine_ip or not self.host_key:
            raise AcceptanceError(
                "runner.ssh_worker_proxy_relay_unavailable",
                "The disposable SSH runtime was unavailable while starting the reverse relay.",
            )
        if self.worker_proxy is None or not self.worker_proxy.thread.is_alive():
            raise AcceptanceError(
                "runner.worker_proxy_unavailable",
                "Worker-only proxy was unavailable while starting the SSH relay.",
            )
        if not self.client_key_path.is_file():
            raise AcceptanceError(
                "runner.ssh_private_key_missing",
                "The one-time SSH private key was unavailable while starting the reverse relay.",
                {"path": str(self.client_key_path)},
            )
        self.credentials_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
        # OpenSSH canonicalizes the default port as the bare host token. The
        # bracketed ``[host]:port`` form is reserved for non-default ports and
        # would make strict checking reject this otherwise pinned key.
        self.known_hosts_path.write_text(f"{self.machine_ip} {self.host_key}\n", encoding="utf-8")
        os.chmod(self.known_hosts_path, 0o600)
        attempts: list[dict[str, Any]] = []
        for _attempt in range(5):
            relay_port = reserve_loopback_port()
            command = [
                "ssh",
                "-F",
                "/dev/null",
                "-o",
                "BatchMode=yes",
                "-o",
                "IdentitiesOnly=yes",
                "-o",
                "IdentityAgent=none",
                "-o",
                "PreferredAuthentications=publickey",
                "-o",
                "PasswordAuthentication=no",
                "-o",
                "KbdInteractiveAuthentication=no",
                "-o",
                "StrictHostKeyChecking=yes",
                "-o",
                "GlobalKnownHostsFile=/dev/null",
                "-o",
                f"UserKnownHostsFile={self.known_hosts_path}",
                "-o",
                "LogLevel=ERROR",
                "-o",
                "ExitOnForwardFailure=yes",
                "-o",
                "ServerAliveInterval=5",
                "-o",
                "ServerAliveCountMax=3",
                "-o",
                "ConnectTimeout=10",
                "-i",
                str(self.client_key_path),
                "-N",
                "-T",
                "-R",
                f"{SSH_RELAY_LOOPBACK_HOST}:{relay_port}:127.0.0.1:{self.worker_proxy.port}",
                f"root@{self.machine_ip}",
            ]
            self._close_worker_proxy_relay_log_handle()
            self.worker_proxy_relay_log_path.parent.mkdir(parents=True, exist_ok=True)
            log_handle = self.worker_proxy_relay_log_path.open("w", encoding="utf-8")
            try:
                process = self._spawn_worker_proxy_relay(command, log_handle)
            except OSError as error:
                log_handle.close()
                raise AcceptanceError(
                    "runner.ssh_worker_proxy_relay_failed",
                    f"The runner-owned reverse SSH relay could not start: {self.redactor.text(str(error))}",
                ) from None
            self.deadline.sleep(0.25)
            if process.poll() is None:
                self.worker_proxy_relay_process = process
                self.worker_proxy_relay_log_handle = log_handle
                self.worker_proxy_relay_port = relay_port
                return
            log_handle.close()
            attempts.append(
                {
                    "relayPort": relay_port,
                    "exitCode": process.returncode,
                    "outputExcerpt": self._worker_proxy_relay_log_excerpt(),
                }
            )
        raise AcceptanceError(
            "runner.ssh_worker_proxy_relay_failed",
            "The runner-owned reverse SSH relay could not establish a VM loopback listener.",
            {"attempts": attempts, "log": str(self.worker_proxy_relay_log_path)},
        )

    def _spawn_worker_proxy_relay(
        self,
        command: Sequence[str],
        log_handle: Any,
    ) -> subprocess.Popen[str]:
        return subprocess.Popen(
            list(command),
            cwd=self.repo_root,
            env=self._tool_environment(),
            stdout=log_handle,
            stderr=subprocess.STDOUT,
            text=True,
            start_new_session=True,
        )

    def _worker_proxy_relay_log_excerpt(self) -> str:
        if not self.worker_proxy_relay_log_path.is_file():
            return ""
        try:
            return self.redactor.text(self.worker_proxy_relay_log_path.read_text(encoding="utf-8")[-1000:])
        except OSError:
            return ""

    def _close_worker_proxy_relay_log_handle(self) -> None:
        log_handle = self.worker_proxy_relay_log_handle
        self.worker_proxy_relay_log_handle = None
        if log_handle is None:
            return
        try:
            log_handle.close()
        except OSError:
            return

    def _stop_worker_proxy_relay(self) -> None:
        process = self.worker_proxy_relay_process
        self.worker_proxy_relay_process = None
        self.worker_proxy_relay_port = 0
        try:
            self.known_hosts_path.unlink(missing_ok=True)
        except OSError:
            pass
        if process is not None and process.poll() is None:
            try:
                os.killpg(process.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            try:
                process.wait(timeout=min(5.0, max(0.25, self.deadline.remaining())))
            except subprocess.TimeoutExpired:
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                process.wait(timeout=3.0)
        self._close_worker_proxy_relay_log_handle()

    def _prepare_ssh_artifacts(self) -> Mapping[str, Any]:
        self.agentd_binary_path.parent.mkdir(parents=True, exist_ok=True)
        environment = self._tool_environment()
        environment.update(
            {
                "CGO_ENABLED": "0",
                "GOOS": "linux",
                "GOARCH": self.options.ssh_machine_arch,
            }
        )
        agentd_started = time.monotonic()
        self._local_command(
            [
                "go",
                "build",
                "-trimpath",
                "-ldflags=-s -w",
                "-o",
                str(self.agentd_binary_path),
                "./cmd/agentd",
            ],
            cwd=self.repo_root / "services" / "control-plane",
            environment=environment,
            log_path=self.logs_dir / "ssh-agentd-build.log",
            maximum_timeout=max(60.0, self.deadline.remaining()),
            error_code="runner.ssh_agentd_build_failed",
            description="Linux synara-agentd cross-build",
        )
        agentd_duration = elapsed_ms(agentd_started)
        fixture_started = time.monotonic()
        self._local_command(
            [
                "bun",
                "build",
                str(
                    self.repo_root
                    / "scripts"
                    / "stage3-provider-acceptance"
                    / "provider-host-fixture.ts"
                ),
                "--target=node",
                "--outfile",
                str(self.fixture_bundle_path),
            ],
            cwd=self.repo_root,
            environment=self._tool_environment(),
            log_path=self.logs_dir / "ssh-provider-host-fixture-build.log",
            maximum_timeout=max(60.0, self.deadline.remaining()),
            error_code="runner.ssh_fixture_build_failed",
            description="Provider Host fixture build",
        )
        fixture_duration = elapsed_ms(fixture_started)
        if not self.agentd_binary_path.is_file() or not self.fixture_bundle_path.is_file():
            raise AcceptanceError(
                "runner.ssh_artifact_missing",
                "SSH acceptance build did not produce the required runtime artifacts.",
            )
        return {
            "agentd": {
                "path": str(self.agentd_binary_path),
                "goos": "linux",
                "goarch": self.options.ssh_machine_arch,
                "sha256": hashlib.sha256(self.agentd_binary_path.read_bytes()).hexdigest(),
                "durationMs": agentd_duration,
            },
            "providerHostFixture": {
                "path": str(self.fixture_bundle_path),
                "remotePath": SSH_REMOTE_FIXTURE_PATH,
                "sha256": hashlib.sha256(self.fixture_bundle_path.read_bytes()).hexdigest(),
                "durationMs": fixture_duration,
            },
        }

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
        try:
            completed = subprocess.run(
                list(arguments),
                cwd=cwd,
                env=dict(environment),
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                text=True,
                timeout=self.deadline.request_timeout(maximum=maximum_timeout),
                check=False,
            )
        except (OSError, subprocess.TimeoutExpired) as error:
            raise AcceptanceError(
                error_code,
                f"{description} could not run: {self.redactor.text(str(error))}",
            ) from None
        output = self.redactor.text(completed.stdout)
        log_path.parent.mkdir(parents=True, exist_ok=True)
        log_path.write_text(output, encoding="utf-8")
        if completed.returncode != 0:
            raise AcceptanceError(
                error_code,
                f"{description} exited with status {completed.returncode}.",
                {"log": str(log_path), "exitCode": completed.returncode, "outputExcerpt": output[-1000:]},
            )

    def _generate_client_key(self) -> Mapping[str, Any]:
        self.credentials_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
        os.chmod(self.credentials_dir, 0o700)
        self._local_command(
            [
                "ssh-keygen",
                "-q",
                "-t",
                "ed25519",
                "-N",
                "",
                "-C",
                f"synara-stage3-{self.machine_name}",
                "-f",
                str(self.client_key_path),
            ],
            cwd=self.state_dir,
            environment=self._tool_environment(),
            log_path=self.logs_dir / "ssh-keygen.log",
            maximum_timeout=15.0,
            error_code="runner.ssh_key_generation_failed",
            description="one-time SSH key generation",
        )
        os.chmod(self.client_key_path, 0o600)
        self.client_private_key = self.client_key_path.read_text(encoding="utf-8")
        self.client_public_key = self.client_public_key_path.read_text(encoding="utf-8").strip()
        if not self.client_private_key or not self.client_public_key.startswith("ssh-ed25519 "):
            raise AcceptanceError(
                "runner.ssh_key_generation_failed",
                "The generated one-time SSH key pair was invalid.",
            )
        self.redactor.add(self.client_private_key, "[REDACTED_SSH_PRIVATE_KEY]")
        for line in self.client_private_key.splitlines():
            if len(line) >= 32 and not line.startswith("-----"):
                self.redactor.add(line, "[REDACTED_SSH_PRIVATE_KEY_DATA]")
        return {
            "credentialSource": "generated under isolated acceptance state",
            "algorithm": "ssh-ed25519",
            "localPrivateKeyPlaintextDeletedAfterProvision": True,
            "controlPlaneCredentialLifecycle": SSH_CREDENTIAL_LIFECYCLE,
        }

    def _prepare_machine(self) -> Mapping[str, Any]:
        listed = self._orbctl_command(["list", "--format", "json"])
        try:
            decoded = json.loads(listed)
        except json.JSONDecodeError as error:
            raise AcceptanceError(
                "runner.orbstack_list_invalid",
                f"OrbStack machine inventory was invalid JSON: {error}",
            ) from None
        machines = decoded if isinstance(decoded, list) else decoded.get("machines") if isinstance(decoded, dict) else None
        if not isinstance(machines, list):
            raise AcceptanceError("runner.orbstack_list_invalid", "OrbStack machine inventory was not an array.")
        existing_names = {
            str(item.get("name"))
            for item in machines
            if isinstance(item, dict) and isinstance(item.get("name"), str)
        }
        if self.machine_name in existing_names:
            raise AcceptanceError(
                "runner.ssh_machine_exists",
                "The disposable OrbStack machine name is already in use.",
                {"machineName": self.machine_name},
            )
        self.machine_create_attempted = True
        self._orbctl_command(
            [
                "create",
                "--arch",
                self.options.ssh_machine_arch,
                "--user",
                SSH_SERVICE_USER,
                "--cpus",
                "2",
                "--memory",
                "4G",
                "--disk",
                "16G",
                "--isolated",
                self.options.ssh_machine_image,
                self.machine_name,
            ],
            log_path=self.logs_dir / "ssh-orbstack-create.log",
            maximum_timeout=max(180.0, self.deadline.remaining()),
        )
        self.machine_created = True
        stage = "/tmp/synara-stage3-acceptance"
        self._remote_command(["install", "-d", "-m", "0700", stage])
        self._remote_upload(
            self.client_public_key_path,
            f"{stage}/{self.client_public_key_path.name}",
            "0600",
        )
        self._remote_upload(
            self.fixture_bundle_path,
            f"{stage}/{self.fixture_bundle_path.name}",
            "0600",
        )
        self._remote_command(
            ["sh", "-ceu", self._machine_setup_script()],
            log_path=self.logs_dir / "ssh-machine-setup.log",
            maximum_timeout=max(240.0, self.deadline.remaining()),
        )
        address_output = self._remote_command(["hostname", "-I"]).strip()
        addresses: list[str] = []
        for candidate in address_output.split():
            try:
                address = ipaddress.ip_address(candidate)
            except ValueError:
                continue
            if address.version == 4 and not address.is_loopback and not address.is_link_local:
                addresses.append(str(address))
        if not addresses:
            raise AcceptanceError(
                "runner.ssh_machine_address_missing",
                "The disposable OrbStack machine did not expose an IPv4 address.",
                {"machineName": self.machine_name},
            )
        self.machine_ip = addresses[0]
        self.host_key = self._remote_command(
            ["cat", "/etc/ssh/ssh_host_ed25519_key.pub"]
        ).strip()
        if not self.host_key.startswith("ssh-ed25519 "):
            raise AcceptanceError(
                "runner.ssh_host_key_invalid",
                "The disposable SSH daemon did not expose an Ed25519 Host Key.",
            )
        try:
            with socket.create_connection(
                (self.machine_ip, 22),
                timeout=self.deadline.request_timeout(maximum=5.0),
            ) as connection:
                connection.settimeout(self.deadline.request_timeout(maximum=5.0))
                banner = connection.recv(128)
        except OSError as error:
            raise AcceptanceError(
                "runner.sshd_unreachable",
                f"The disposable SSH daemon was unreachable: {self.redactor.text(str(error))}",
                {"machineAddress": self.machine_ip, "port": 22},
            ) from None
        if not banner.startswith(b"SSH-"):
            raise AcceptanceError(
                "runner.sshd_banner_invalid",
                "The disposable SSH endpoint did not return an SSH protocol banner.",
                {"machineAddress": self.machine_ip, "port": 22},
            )
        return {
            "machineName": self.machine_name,
            "ownedMachine": True,
            "machineImage": self.options.ssh_machine_image,
            "machineArch": self.options.ssh_machine_arch,
            "machineAddress": self.machine_ip,
            "controlPlaneTransport": {
                "mode": "reverse-ssh-loopback",
                "description": SSH_RELAY_TRANSPORT,
                "vmListenHost": SSH_RELAY_LOOPBACK_HOST,
            },
            "nodeVersion": self.options.ssh_node_version,
            "sshd": "active",
            "initSystem": "systemd",
            "hostKeyFingerprint": self._host_key_fingerprint(self.host_key),
        }

    def _machine_setup_script(self) -> str:
        node_arch = "x64" if self.options.ssh_machine_arch == "amd64" else "arm64"
        version = self.options.ssh_node_version
        archive = f"node-v{version}-linux-{node_arch}.tar.xz"
        stage = "/tmp/synara-stage3-acceptance"
        return "\n".join(
            [
                "export DEBIAN_FRONTEND=noninteractive",
                "apt-get update",
                "apt-get install -y --no-install-recommends ca-certificates curl git openssh-client openssh-server xz-utils",
                f"id -u {SSH_SERVICE_USER} >/dev/null",
                "install -d -m 0700 /root/.ssh",
                f"install -m 0600 {stage}/{self.client_public_key_path.name} /root/.ssh/authorized_keys",
                f"install -d -m 0755 {pathlib.PurePosixPath(SSH_REMOTE_FIXTURE_PATH).parent}",
                f"install -m 0644 {stage}/{self.fixture_bundle_path.name} {SSH_REMOTE_FIXTURE_PATH}",
                "workdir=$(mktemp -d /tmp/synara-node.XXXXXX)",
                "trap 'rm -rf \"$workdir\"' EXIT",
                "cd \"$workdir\"",
                f"curl -fsSLO https://nodejs.org/dist/v{version}/{archive}",
                f"curl -fsSLO https://nodejs.org/dist/v{version}/SHASUMS256.txt",
                f"grep '  {archive}$' SHASUMS256.txt | sha256sum -c -",
                f"tar -xJf {archive} -C /usr/local --strip-components=1",
                "cat > /etc/ssh/sshd_config.d/99-synara-stage3-acceptance.conf <<'EOF'",
                "PasswordAuthentication no",
                "KbdInteractiveAuthentication no",
                "PubkeyAuthentication yes",
                "PermitRootLogin prohibit-password",
                "EOF",
                "install -d -o root -g root -m 0755 /run/sshd",
                "sshd -t",
                "systemctl enable ssh",
                "systemctl restart ssh",
                "test \"$(cat /proc/1/comm)\" = systemd",
                "systemctl is-active --quiet ssh",
                "systemctl is-enabled --quiet ssh",
                f"test \"$(node --version)\" = v{version}",
                f"rm -rf {stage}",
            ]
        )

    def _create_ssh_target(
        self,
        tenant_id: str,
        organization_id: str,
        name: str,
        host_key: str,
        provider: str,
    ) -> dict[str, Any]:
        return json_object(
            self.api.request(
                "POST",
                f"/v1/tenants/{tenant_id}/execution-targets",
                {
                    "organizationId": organization_id,
                    "kind": "ssh",
                    "name": name,
                    "configuration": {
                        "host": self.machine_ip,
                        "port": 22,
                        "user": "root",
                        "privateKey": self.client_private_key,
                        "hostKey": host_key,
                        "controlPlaneUrl": self._worker_proxy_url(),
                        "allowInsecureControlPlane": True,
                        "runnerCommand": list(self.options.runner_command),
                        "serviceUser": SSH_SERVICE_USER,
                        "useSudo": False,
                    },
                    "capabilities": {
                        "workspaceModes": ["local", "worktree"],
                        "providerPolicy": {"experimentalProviders": [provider]},
                    },
                },
                expected=(201,),
            ),
            name,
        )

    @staticmethod
    def _target_id(target: Mapping[str, Any], label: str) -> str:
        target_id = target.get("id")
        if not isinstance(target_id, str) or not target_id:
            raise AcceptanceError("runner.ssh_target_id_missing", f"{label} creation did not return an ID.")
        return target_id

    def _service_state(self, service_name: str, *, cleanup_timeout: float | None = None) -> dict[str, Any]:
        output = self._remote_command(
            [
                "systemctl",
                "show",
                service_name,
                "--property=ActiveState,SubState,MainPID,UnitFileState,NRestarts",
            ],
            cleanup_timeout=cleanup_timeout,
        )
        values: dict[str, str] = {}
        for line in output.splitlines():
            key, separator, value = line.partition("=")
            if separator:
                values[key] = value
        try:
            main_pid = int(values.get("MainPID", "0"))
            restarts = int(values.get("NRestarts", "0"))
        except ValueError as error:
            raise AcceptanceError(
                "runner.ssh_service_state_invalid",
                f"systemd returned an invalid numeric service state: {error}",
                {"serviceName": service_name},
            ) from None
        return {
            "serviceName": service_name,
            "activeState": values.get("ActiveState"),
            "subState": values.get("SubState"),
            "unitFileState": values.get("UnitFileState"),
            "mainPid": main_pid,
            "restartCount": restarts,
        }

    def _require_service_active(self, service_name: str) -> dict[str, Any]:
        state = self._service_state(service_name)
        if (
            state["activeState"] != "active"
            or state["subState"] != "running"
            or state["mainPid"] <= 0
            or state["unitFileState"] not in {"enabled", "enabled-runtime"}
        ):
            raise AcceptanceError(
                "runner.ssh_service_not_active",
                "The managed SSH agentd systemd service was not active and enabled.",
                state,
            )
        return state

    def _assert_remote_target_absent(
        self,
        target_id: str,
        *,
        cleanup_timeout: float | None = None,
    ) -> None:
        service_name = f"synara-agentd-{target_id}.service"
        install_root = f"/opt/synara/targets/{target_id}"
        temporary = f"/tmp/synara-agentd-{target_id}"
        script = "\n".join(
            [
                f"if systemctl cat {service_name} >/dev/null 2>&1; then exit 1; fi",
                f"test ! -e {install_root}/synara-agentd",
                f"test ! -e {install_root}/agentd.env",
                f"test ! -e {temporary}",
                f"test ! -e {temporary}.env",
                f"test ! -e {temporary}.service",
            ]
        )
        self._remote_command(["sh", "-ceu", script], cleanup_timeout=cleanup_timeout)

    def _orbctl_completed(
        self,
        arguments: Sequence[str],
        *,
        cleanup_timeout: float | None = None,
        input_text: str | None = None,
    ) -> subprocess.CompletedProcess[str]:
        timeout = cleanup_timeout
        if timeout is None:
            timeout = self.deadline.request_timeout(maximum=max(10.0, self.deadline.remaining()))
        try:
            return subprocess.run(
                [self.options.ssh_orbctl_bin, *arguments],
                cwd=self.repo_root,
                env=self._tool_environment(),
                input=input_text,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                text=True,
                timeout=timeout,
                check=False,
            )
        except (OSError, subprocess.TimeoutExpired) as error:
            raise AcceptanceError(
                "runner.orbstack_command_failed",
                f"OrbStack command could not run: {self.redactor.text(str(error))}",
                {"command": [self.options.ssh_orbctl_bin, *arguments[:3]]},
            ) from None

    def _orbctl_command(
        self,
        arguments: Sequence[str],
        *,
        log_path: pathlib.Path | None = None,
        maximum_timeout: float | None = None,
        cleanup_timeout: float | None = None,
        input_text: str | None = None,
    ) -> str:
        timeout = cleanup_timeout
        if timeout is None:
            timeout = self.deadline.request_timeout(maximum=maximum_timeout or 30.0)
        completed = self._orbctl_completed(arguments, cleanup_timeout=timeout, input_text=input_text)
        output = self.redactor.text(completed.stdout)
        if log_path is not None:
            log_path.parent.mkdir(parents=True, exist_ok=True)
            log_path.write_text(output, encoding="utf-8")
        if completed.returncode != 0:
            raise AcceptanceError(
                "runner.orbstack_command_failed",
                f"OrbStack command exited with status {completed.returncode}.",
                {
                    "command": [self.options.ssh_orbctl_bin, *arguments[:3]],
                    "exitCode": completed.returncode,
                    "log": str(log_path) if log_path else None,
                    "outputExcerpt": output[-1000:],
                },
            )
        return output

    def _remote_command(
        self,
        command: Sequence[str],
        *,
        user: str = "root",
        log_path: pathlib.Path | None = None,
        maximum_timeout: float | None = None,
        cleanup_timeout: float | None = None,
        input_text: str | None = None,
    ) -> str:
        return self._orbctl_command(
            ["run", "--machine", self.machine_name, "--user", user, *command],
            log_path=log_path,
            maximum_timeout=maximum_timeout,
            cleanup_timeout=cleanup_timeout,
            input_text=input_text,
        )

    def _remote_upload(self, source: pathlib.Path, destination: str, mode: str) -> None:
        if not source.is_file():
            raise AcceptanceError(
                "runner.ssh_upload_source_missing",
                "An SSH acceptance runtime artifact was unavailable for upload.",
                {"path": str(source)},
            )
        payload = base64.b64encode(source.read_bytes()).decode("ascii")
        self._remote_command(
            [
                "sh",
                "-ceu",
                f"umask 077; base64 --decode > {destination}; chmod {mode} {destination}",
            ],
            input_text=payload,
            maximum_timeout=max(30.0, self.deadline.remaining()),
        )

    @staticmethod
    def _host_key_fingerprint(host_key: str) -> str:
        fields = host_key.split()
        if len(fields) < 2:
            raise AcceptanceError("runner.ssh_host_key_invalid", "SSH Host Key was malformed.")
        try:
            payload = fields[1] + "=" * (-len(fields[1]) % 4)
            decoded = base64.b64decode(payload, validate=True)
        except ValueError as error:
            raise AcceptanceError(
                "runner.ssh_host_key_invalid",
                f"SSH Host Key payload was invalid: {error}",
            ) from None
        digest = base64.b64encode(hashlib.sha256(decoded).digest()).decode("ascii").rstrip("=")
        return f"SHA256:{digest}"

    def _discard_local_private_key(self) -> None:
        self.client_private_key = ""
        try:
            self.client_key_path.unlink(missing_ok=True)
        except OSError as error:
            raise AcceptanceError(
                "runner.ssh_private_key_cleanup_failed",
                f"The one-time SSH private key could not be removed: {error}",
            ) from None

    def _discard_local_key_material(self) -> None:
        self.client_private_key = ""
        self.client_public_key = ""
        for path in (self.client_key_path, self.client_public_key_path, self.known_hosts_path):
            try:
                path.unlink(missing_ok=True)
            except OSError:
                pass
        try:
            self.credentials_dir.rmdir()
        except OSError:
            pass


class KubernetesDriver(ManagedWorkerDriver):
    name = "kubernetes"
    lifecycle = EXECUTION_PINNED_WORKER
    pending_interaction_recovery = "delete-pod"

    def __init__(
        self,
        repo_root: pathlib.Path,
        options: RunnerOptions,
        deadline: Deadline,
        redactor: SecretRedactor,
    ) -> None:
        super().__init__(repo_root, options, deadline, redactor)
        suffix = uuid.uuid4().hex[:12]
        self.target_name = f"stage3-kubernetes-{suffix}"
        self.target_namespace = f"synara-stage3-worker-{suffix}"
        self.canary_target_name = f"stage3-kubernetes-canary-{suffix}"
        self.canary_namespace = f"synara-stage3-canary-{suffix}"
        self.bootstrap_namespace = f"synara-stage3-control-{suffix}"
        self.bootstrap_service_account = "synara-control-plane"
        self.bootstrap_role = f"synara-stage3-{suffix}"
        self.worker_service_account = f"synara-worker-{suffix}"
        self.canary_service_account = f"synara-canary-{suffix}"
        self.target_id: str | None = None
        self.canary_target_id: str | None = None
        self.target_runtimes: dict[str, dict[str, str]] = {}
        self.owned_namespaces = {self.bootstrap_namespace, self.target_namespace, self.canary_namespace}
        self.owns_cluster = options.kubernetes_context is None
        self.cluster_created = False
        self.cluster_name = options.kind_cluster_name or f"synara-stage3-{suffix}"
        self.context = options.kubernetes_context or f"kind-{self.cluster_name}"
        if not self.owns_cluster and self.context.startswith("kind-"):
            self.cluster_name = self.context.removeprefix("kind-")
        self.kubeconfig = (
            options.kubernetes_kubeconfig
            if options.kubernetes_kubeconfig is not None
            else self.state_dir / "kubeconfig"
            if self.owns_cluster
            else None
        )
        self.owns_image = not options.kubernetes_skip_worker_build
        head_sha = self._head_sha()
        self.image = options.kubernetes_worker_image or (
            f"synara-stage3-provider-acceptance:{head_sha}-kubernetes-{suffix}"
        )
        self.canary_image = f"synara-stage3-provider-canary:{head_sha[:16]}-{suffix}"
        self.canary_image_prepared = False
        self.api_server = ""
        self.ca_certificate = ""
        self.kubernetes_token = ""

    @property
    def worker_proxy_host(self) -> str:
        return self.options.kubernetes_control_plane_host

    def prepare(self) -> Mapping[str, Any]:
        control_plane = super().prepare()
        cluster_evidence = self._prepare_cluster()
        image_evidence = self._prepare_worker_image(
            self.image,
            skip_build=self.options.kubernetes_skip_worker_build,
            log_prefix="kubernetes",
        )
        if self.context.startswith("kind-"):
            cluster_name = self.context.removeprefix("kind-")
            self._kind_command(
                ["load", "docker-image", "--name", cluster_name, self.image],
                log_path=self.logs_dir / "kubernetes-kind-load-image.log",
                maximum_timeout=max(60.0, self.deadline.remaining()),
            )
        elif not self.options.kubernetes_skip_worker_build:
            raise AcceptanceError(
                "runner.kubernetes_image_load_unsupported",
                "A locally built Kubernetes Worker image can only be loaded into a Kind context.",
                {"context": self.context, "requiredInputs": ["--kubernetes-skip-worker-build"]},
            )
        access_evidence = self._prepare_cluster_access()
        return {
            "controlPlane": control_plane,
            "kubernetes": {
                **cluster_evidence,
                **access_evidence,
                "resourceOwner": self.resource_owner,
                "containerEngine": image_evidence,
            },
        }

    def _prepare_canary_image(self) -> Mapping[str, Any]:
        if self.canary_image_prepared:
            return {"image": self.canary_image, "prepared": True}
        if not self.context.startswith("kind-"):
            raise AcceptanceUnsupported(
                "runner.kubernetes_canary_registry_required",
                "A non-Kind canary requires a caller-published immutable image; the fixture only creates local aliases.",
                {
                    "context": self.context,
                    "requiredInputs": ["published immutable canary image and product release revision"],
                },
            )
        self._docker_command(
            ["image", "tag", self.image, self.canary_image],
            log_path=self.logs_dir / "kubernetes-canary-tag.log",
        )
        self._kind_command(
            [
                "load",
                "docker-image",
                "--name",
                self.context.removeprefix("kind-"),
                self.canary_image,
            ],
            log_path=self.logs_dir / "kubernetes-kind-load-canary-image.log",
            maximum_timeout=max(60.0, self.deadline.remaining()),
        )
        self.canary_image_prepared = True
        image_id = self._docker_command(
            ["image", "inspect", "--format", "{{.Id}}", self.canary_image]
        ).strip()
        return {
            "image": self.canary_image,
            "sourceImage": self.image,
            "imageId": image_id,
            "prepared": True,
            "ownership": "runner-owned alias; source image is never deleted unless it was built by this run",
        }

    def provision_target(
        self,
        tenant_id: str,
        organization_id: str,
        provider: str,
    ) -> Mapping[str, Any]:
        target = self._create_kubernetes_target(
            tenant_id,
            organization_id,
            provider,
            name=self.target_name,
            namespace=self.target_namespace,
            service_account=self.worker_service_account,
            image=self.image,
        )
        target_id = target.get("id")
        if not isinstance(target_id, str) or not target_id:
            raise AcceptanceError(
                "runner.kubernetes_target_id_missing",
                "Kubernetes Target creation did not return an ID.",
            )
        self.target_id = target_id
        self._remember_target_runtime(
            target_id,
            namespace=self.target_namespace,
            service_account=self.worker_service_account,
            image=self.image,
        )
        self._wait_and_label_namespace(self.target_namespace)
        return {
            **target,
            "driverEvidence": {
                "context": self.context,
                "namespace": self.target_namespace,
                "workerAllocation": self.lifecycle.worker_allocation,
                "image": self.image,
                "imagePullPolicy": "Never" if self.context.startswith("kind-") else "IfNotPresent",
                "networkPolicyImplementation": "cluster-dependent",
                "resourceOwner": self.resource_owner,
            },
        }

    def provision_canary_target(
        self,
        tenant_id: str,
        organization_id: str,
        provider: str,
    ) -> Mapping[str, Any]:
        image_evidence = self._prepare_canary_image()
        target = self._create_kubernetes_target(
            tenant_id,
            organization_id,
            provider,
            name=self.canary_target_name,
            namespace=self.canary_namespace,
            service_account=self.canary_service_account,
            image=self.canary_image,
        )
        target_id = target.get("id")
        if not isinstance(target_id, str) or not target_id:
            raise AcceptanceError(
                "runner.kubernetes_canary_target_id_missing",
                "Kubernetes canary Target creation did not return an ID.",
            )
        self.canary_target_id = target_id
        self._remember_target_runtime(
            target_id,
            namespace=self.canary_namespace,
            service_account=self.canary_service_account,
            image=self.canary_image,
        )
        self._wait_and_label_namespace(self.canary_namespace)
        return {
            **target,
            "driverEvidence": {
                "context": self.context,
                "namespace": self.canary_namespace,
                "image": self.canary_image,
                "sourceImage": self.image,
                "imagePullPolicy": "Never" if self.context.startswith("kind-") else "IfNotPresent",
                "resourceOwner": self.resource_owner,
                "imagePreparation": image_evidence,
            },
        }

    def _create_kubernetes_target(
        self,
        tenant_id: str,
        organization_id: str,
        provider: str,
        *,
        name: str,
        namespace: str,
        service_account: str,
        image: str,
    ) -> dict[str, Any]:
        if not self.api_server or not self.ca_certificate or not self.kubernetes_token:
            raise AcceptanceError(
                "runner.kubernetes_access_unavailable",
                "Kubernetes API access was unavailable while provisioning the Target.",
            )
        return json_object(
            self.api.request(
                "POST",
                f"/v1/tenants/{tenant_id}/execution-targets",
                {
                    "organizationId": organization_id,
                    "kind": "kubernetes",
                    "name": name,
                    "configuration": {
                        "apiServer": self.api_server,
                        "bearerToken": self.kubernetes_token,
                        "caCertificate": self.ca_certificate,
                        "namespace": namespace,
                        "manageNamespace": True,
                        "serviceAccountName": service_account,
                        "image": image,
                        "imagePullPolicy": "Never" if self.context.startswith("kind-") else "IfNotPresent",
                        "controlPlaneUrl": self._worker_proxy_url(),
                        "allowInsecureControlPlane": True,
                        "runnerCommand": list(self.options.runner_command),
                        "maxActivePods": 1,
                        "egressCidrs": ["0.0.0.0/0"],
                        "cpuRequest": "100m",
                        "cpuLimit": "1",
                        "memoryRequest": "128Mi",
                        "memoryLimit": "1Gi",
                        "ephemeralStorageRequest": "128Mi",
                        "ephemeralStorageLimit": "2Gi",
                        "workspaceSizeLimit": "1Gi",
                        "quotaCpuRequests": "1",
                        "quotaCpuLimits": "2",
                        "quotaMemoryRequests": "1Gi",
                        "quotaMemoryLimits": "2Gi",
                        "quotaEphemeralStorage": "4Gi",
                    },
                    "capabilities": {
                        "workspaceModes": ["local", "worktree"],
                        "providerPolicy": {"experimentalProviders": [provider]},
                    },
                },
                expected=(201,),
            ),
            "kubernetes execution target",
        )

    def _remember_target_runtime(
        self,
        target_id: str,
        *,
        namespace: str,
        service_account: str,
        image: str,
    ) -> None:
        self.target_runtimes[target_id] = {
            "namespace": namespace,
            "serviceAccount": service_account,
            "image": image,
        }

    def _target_runtime(self, target_id: str) -> Mapping[str, str]:
        runtime = self.target_runtimes.get(target_id)
        if runtime is not None:
            return runtime
        return {
            "namespace": self.target_namespace,
            "serviceAccount": self.worker_service_account,
            "image": self.image,
        }

    def replace_worker(
        self,
        _tenant_id: str,
        _target_id: str,
        _provider: str,
    ) -> Mapping[str, Any]:
        raise AcceptanceError(
            "runner.target_replacement_unsupported",
            "The Kubernetes Target uses one execution-pinned Pod per Execution.",
        )

    def recover_pending_interaction(self, target_id: str, execution_id: str) -> Mapping[str, Any]:
        runtime = self._target_runtime(target_id)
        namespace = runtime["namespace"]
        pod = self._wait_execution_pod(target_id, execution_id)
        metadata = json_object(pod.get("metadata"), "Kubernetes Pod metadata")
        status = json_object(pod.get("status"), "Kubernetes Pod status")
        labels = json_object(metadata.get("labels"), "Kubernetes Pod labels")
        name = metadata.get("name")
        uid = metadata.get("uid")
        if not isinstance(name, str) or not name or not isinstance(uid, str) or not uid:
            raise AcceptanceError(
                "runner.kubernetes_pod_contract_mismatch",
                "The execution-pinned Pod omitted its stable identity.",
                {"metadata": metadata},
            )
        self._kubectl_command(
            [
                "-n",
                namespace,
                "delete",
                "pod",
                name,
                "--grace-period=0",
                "--force",
                "--wait=false",
            ],
            cleanup_timeout=20.0,
        )
        return {
            "recoveryMode": self.pending_interaction_recovery,
            "deletedPodName": name,
            "deletedPodUid": uid,
            "deletedPodPhase": status.get("phase"),
            "deletedPodGeneration": labels.get("synara.io/generation"),
        }

    def inject_failure(
        self,
        fault: str,
        target_id: str,
        execution_id: str,
    ) -> Mapping[str, Any]:
        if fault == "worker-network":
            return super().inject_failure(fault, target_id, execution_id)
        if fault not in {"kubernetes-drain", "kubernetes-eviction"}:
            raise AcceptanceUnsupported(
                "runner.failure_case_unsupported",
                f"The Kubernetes Target does not implement failure injection {fault}.",
                {"target": self.name, "failureCase": fault},
            )
        runtime = self._target_runtime(target_id)
        namespace = runtime["namespace"]
        pod = self._wait_execution_pod(target_id, execution_id)
        metadata = json_object(pod.get("metadata"), "Kubernetes Pod metadata")
        spec = json_object(pod.get("spec"), "Kubernetes Pod spec")
        labels = json_object(metadata.get("labels"), "Kubernetes Pod labels")
        name = metadata.get("name")
        uid = metadata.get("uid")
        if not isinstance(name, str) or not name or not isinstance(uid, str) or not uid:
            raise AcceptanceError(
                "runner.kubernetes_pod_contract_mismatch",
                "The fault target Pod omitted its stable name or UID.",
                {"metadata": metadata},
            )
        if fault == "kubernetes-eviction":
            eviction = {
                "apiVersion": "policy/v1",
                "kind": "Eviction",
                "metadata": {"name": name, "namespace": namespace},
                "deleteOptions": {
                    "gracePeriodSeconds": 0,
                    "preconditions": {"uid": uid},
                },
            }
            path = (
                f"/api/v1/namespaces/{urllib.parse.quote(namespace, safe='')}/pods/"
                f"{urllib.parse.quote(name, safe='')}/eviction"
            )
            self._kubectl_command(
                ["create", "--raw", path, "-f", "-"],
                input_text=json.dumps(eviction, separators=(",", ":")),
                cleanup_timeout=20.0,
            )
            return {
                "fault": fault,
                "evictionApiVersion": "policy/v1",
                "deletedPodName": name,
                "deletedPodUid": uid,
                "deletedPodGeneration": labels.get("synara.io/generation"),
                "namespace": namespace,
                "uidPrecondition": True,
            }

        if not self.owns_cluster and not self.options.kubernetes_allow_node_drain:
            raise AcceptanceUnsupported(
                "runner.kubernetes_node_drain_not_authorized",
                "Node drain is disabled for an operator-owned Kubernetes context.",
                {
                    "context": self.context,
                    "requiredInputs": ["--kubernetes-allow-node-drain"],
                },
            )
        node_name = spec.get("nodeName")
        if not isinstance(node_name, str) or not node_name:
            raise AcceptanceError(
                "runner.kubernetes_node_name_missing",
                "The execution Pod was not assigned to a Kubernetes Node.",
                {"podName": name},
            )
        selector = (
            f"synara.io/execution-target-id={target_id},"
            f"synara.io/execution-id={execution_id}"
        )
        self._kubectl_command(["cordon", node_name], cleanup_timeout=15.0)
        started = time.monotonic()
        try:
            self._kubectl_command(
                [
                    "drain",
                    node_name,
                    f"--pod-selector={selector}",
                    "--ignore-daemonsets",
                    "--delete-emptydir-data",
                    "--force",
                    "--disable-eviction",
                    "--grace-period=20",
                    "--timeout=45s",
                ],
                cleanup_timeout=55.0,
            )
        finally:
            self._kubectl_command(["uncordon", node_name], cleanup_timeout=15.0)
        return {
            "fault": fault,
            "node": node_name,
            "selector": selector,
            "deletedPodName": name,
            "deletedPodUid": uid,
            "deletedPodGeneration": labels.get("synara.io/generation"),
            "namespace": namespace,
            "durationMs": elapsed_ms(started),
            "uncordoned": True,
            "deleteMechanism": "kubectl drain with graceful Pod DELETE, not Eviction subresource",
        }

    def validate_failure(self, fault: str) -> None:
        if fault == "worker-network" or fault == "kubernetes-eviction":
            return
        if fault == "kubernetes-drain":
            if not self.owns_cluster and not self.options.kubernetes_allow_node_drain:
                raise AcceptanceUnsupported(
                    "runner.kubernetes_node_drain_not_authorized",
                    "Node drain is disabled for an operator-owned Kubernetes context.",
                    {
                        "context": self.context,
                        "requiredInputs": ["--kubernetes-allow-node-drain"],
                    },
                )
            return
        return super().validate_failure(fault)

    def observe_execution(self, target_id: str, execution_id: str) -> Mapping[str, Any]:
        runtime = self._target_runtime(target_id)
        expected_namespace = runtime["namespace"]
        expected_service_account = runtime["serviceAccount"]
        expected_image = runtime["image"]
        pod = self._wait_execution_pod(target_id, execution_id)
        metadata = json_object(pod.get("metadata"), "Kubernetes Pod metadata")
        spec = json_object(pod.get("spec"), "Kubernetes Pod spec")
        status = json_object(pod.get("status"), "Kubernetes Pod status")
        labels = json_object(metadata.get("labels"), "Kubernetes Pod labels")
        containers = spec.get("containers")
        if not isinstance(containers, list) or len(containers) != 1 or not isinstance(containers[0], dict):
            raise AcceptanceError(
                "runner.kubernetes_pod_contract_mismatch",
                "The execution-pinned Pod did not contain exactly one agentd container.",
            )
        container = containers[0]
        environment = {
            str(item.get("name")): item
            for item in container.get("env", [])
            if isinstance(item, dict) and isinstance(item.get("name"), str)
        }
        registration = json_object(
            json_object(environment.get("SYNARA_WORKER_REGISTRATION_TOKEN"), "registration environment").get("valueFrom"),
            "registration valueFrom",
        )
        secret_ref = json_object(registration.get("secretKeyRef"), "registration secretKeyRef")
        container_security = json_object(container.get("securityContext"), "container securityContext")
        pod_security = json_object(spec.get("securityContext"), "Pod securityContext")
        capabilities = json_object(container_security.get("capabilities"), "container capabilities")
        expected_labels = {
            "synara.io/managed": "true",
            "synara.io/execution-target-id": target_id,
            "synara.io/execution-id": execution_id,
        }
        actual_labels = {key: labels.get(key) for key in expected_labels}
        assigned_execution = json_object(
            environment.get("SYNARA_AGENTD_ASSIGNED_EXECUTION_ID"),
            "assigned execution environment",
        ).get("value")
        expected_security = {
            "allowPrivilegeEscalation": False,
            "readOnlyRootFilesystem": True,
            "runAsNonRoot": True,
            "runAsUser": 10001,
            "runAsGroup": 10001,
        }
        actual_security = {key: container_security.get(key) for key in expected_security}
        if (
            actual_labels != expected_labels
            or labels.get("synara.io/generation") in (None, "")
            or container.get("name") != "agentd"
            or container.get("image") != expected_image
            or container.get("imagePullPolicy") not in {"Never", "IfNotPresent"}
            or assigned_execution != execution_id
            or secret_ref.get("key") != "registration-token"
            or actual_security != expected_security
            or capabilities.get("drop") != ["ALL"]
            or pod_security.get("runAsNonRoot") is not True
            or pod_security.get("fsGroup") != 10001
            or spec.get("automountServiceAccountToken") is not False
            or spec.get("restartPolicy") != "Never"
            or spec.get("serviceAccountName") != expected_service_account
        ):
            raise AcceptanceError(
                "runner.kubernetes_pod_contract_mismatch",
                "The execution-pinned Pod did not apply the requested identity and security contract.",
                {
                    "labels": actual_labels,
                    "assignedExecutionId": assigned_execution,
                    "containerSecurity": actual_security,
                    "serviceAccountName": spec.get("serviceAccountName"),
                },
            )
        volume_names = sorted(
            str(item.get("name"))
            for item in spec.get("volumes", [])
            if isinstance(item, dict) and isinstance(item.get("name"), str)
        )
        if volume_names != ["home", "tmp", "workspace"]:
            raise AcceptanceError(
                "runner.kubernetes_pod_contract_mismatch",
                "The execution-pinned Pod did not use the expected ephemeral volumes.",
                {"volumes": volume_names},
            )
        foundation = self._foundation_evidence(
            target_id,
            secret_ref.get("name"),
            namespace=expected_namespace,
            service_account=expected_service_account,
        )
        return {
            "podName": metadata.get("name"),
            "podUid": metadata.get("uid"),
            "phase": status.get("phase"),
            "generation": labels.get("synara.io/generation"),
            "image": container.get("image"),
            "serviceAccountName": spec.get("serviceAccountName"),
            "security": actual_security,
            "volumes": volume_names,
            "foundation": foundation,
        }

    def observe_terminal_execution(self, target_id: str, execution_id: str) -> Mapping[str, Any]:
        while True:
            pods = self._execution_pods(target_id, execution_id)
            if not pods:
                return {"podDeleted": True, "executionId": execution_id}
            self.deadline.sleep(0.2)

    def collect_failure_diagnostics(self, case_id: str) -> Mapping[str, Any]:
        evidence = dict(super().collect_failure_diagnostics(case_id))
        pod_summaries: list[dict[str, Any]] = []
        for namespace in sorted({runtime["namespace"] for runtime in self.target_runtimes.values()}):
            completed = self._kubectl_completed(
                [
                    "-n",
                    namespace,
                    "get",
                    "pods",
                    "-o",
                    "json",
                ],
                cleanup_timeout=8.0,
            )
            if completed.returncode != 0:
                continue
            try:
                items = json.loads(completed.stdout).get("items", [])
            except (AttributeError, json.JSONDecodeError):
                continue
            for pod in items[:5] if isinstance(items, list) else []:
                if not isinstance(pod, dict):
                    continue
                metadata = pod.get("metadata") if isinstance(pod.get("metadata"), dict) else {}
                status = pod.get("status") if isinstance(pod.get("status"), dict) else {}
                pod_summaries.append(
                    {
                        "namespace": namespace,
                        "name": metadata.get("name"),
                        "uid": metadata.get("uid"),
                        "deletionTimestamp": metadata.get("deletionTimestamp"),
                        "phase": status.get("phase"),
                        "reason": status.get("reason"),
                    }
                )
        evidence["pods"] = pod_summaries
        evidence["context"] = self.context
        return evidence

    def cleanup(self) -> Mapping[str, Any]:
        errors: list[str] = []

        def collect(operation: str, action: Callable[[], Any]) -> None:
            try:
                action()
            except Exception as error:
                errors.append(f"{operation}: {self.redactor.text(str(error))}")

        collect("stop Control Plane", self.stop)
        if self.options.keep:
            for target_id, runtime in self.target_runtimes.items():
                collect(
                    f"delete active execution Pods for {target_id}",
                    lambda target_id=target_id, runtime=runtime: self._kubectl_command(
                        [
                            "-n",
                            runtime["namespace"],
                            "delete",
                            "pods",
                            "-l",
                            f"synara.io/execution-target-id={target_id}",
                            "--ignore-not-found",
                            "--wait=false",
                        ],
                        cleanup_timeout=20.0,
                    ),
                )
        elif self.owns_cluster and self.cluster_created:
            collect(
                "delete Kind cluster",
                lambda: self._kind_command(
                    ["delete", "cluster", "--name", self.cluster_name],
                    cleanup_timeout=60.0,
                ),
            )
            self.cluster_created = False
        else:
            collect("delete Kubernetes acceptance resources", self._delete_cluster_resources)
        if not self.options.keep:
            if self.canary_image_prepared:
                collect(
                    "remove Kubernetes canary image alias",
                    lambda: self._docker_cleanup_image(self.canary_image),
                )
            if self.owns_image:
                collect(
                    "remove Kubernetes acceptance image",
                    lambda: self._docker_cleanup_image(self.image),
                )
        collect("stop Worker-only proxy", self._stop_worker_proxy)
        self.registration_token = ""
        self.kubernetes_token = ""
        collect("release isolated state", self._release_state)
        if errors:
            raise AcceptanceError(
                "runner.kubernetes_cleanup_failed",
                "Kubernetes acceptance resources could not be cleaned completely.",
                {"errors": errors},
            )
        return {
            "target": self.name,
            "resourceOwner": self.resource_owner,
            "context": self.context,
            "ownedCluster": self.owns_cluster,
            "ownedClusterRemoved": self.owns_cluster and not self.options.keep,
            "ownedNamespaces": sorted(self.owned_namespaces),
            "reusedClusterResourcesRemoved": not self.owns_cluster and not self.options.keep,
            "workerImage": self.image,
            "ownedWorkerImageRemoved": self.owns_image and not self.options.keep,
            "canaryImage": self.canary_image if self.canary_image_prepared else None,
            "ownedCanaryImageRemoved": self.canary_image_prepared and not self.options.keep,
            "broadCleanupUsed": False,
        }

    def _prepare_cluster(self) -> Mapping[str, Any]:
        if self.owns_cluster:
            if self.kubeconfig is None:
                raise AcceptanceError("runner.kubernetes_kubeconfig_missing", "Owned Kind cluster omitted kubeconfig.")
            self.kubeconfig.parent.mkdir(parents=True, exist_ok=True)
            self._kind_command(
                [
                    "create",
                    "cluster",
                    "--name",
                    self.cluster_name,
                    "--image",
                    self.options.kind_node_image,
                    "--kubeconfig",
                    str(self.kubeconfig),
                    "--wait",
                    "180s",
                ],
                log_path=self.logs_dir / "kubernetes-kind-create.log",
                maximum_timeout=max(190.0, self.deadline.remaining()),
            )
            self.cluster_created = True
        elif not self.context.startswith("kind-") and not self.options.kubernetes_allow_nondisposable:
            raise AcceptanceError(
                "runner.kubernetes_context_not_disposable",
                "Reusing a non-Kind Kubernetes context requires --kubernetes-allow-nondisposable.",
                {"context": self.context},
            )
        context_name = self._kubectl_command(["config", "get-contexts", self.context, "-o", "name"]).strip()
        if context_name != self.context:
            raise AcceptanceError(
                "runner.kubernetes_context_missing",
                "The configured Kubernetes context was not found.",
                {"context": self.context},
            )
        version = json.loads(self._kubectl_command(["version", "-o", "json"]))
        server = json_object(version.get("serverVersion"), "Kubernetes serverVersion")
        return {
            "context": self.context,
            "ownedCluster": self.owns_cluster,
            "clusterName": self.cluster_name if self.context.startswith("kind-") else None,
            "serverVersion": server.get("gitVersion"),
            "kubeconfig": str(self.kubeconfig) if self.kubeconfig is not None else "default",
        }

    def _prepare_cluster_access(self) -> Mapping[str, Any]:
        ownership_labels = self._ownership_labels()
        manifest = {
            "apiVersion": "v1",
            "kind": "List",
            "items": [
                {
                    "apiVersion": "v1",
                    "kind": "Namespace",
                    "metadata": {"name": self.bootstrap_namespace, "labels": ownership_labels},
                },
                {
                    "apiVersion": "v1",
                    "kind": "ServiceAccount",
                    "metadata": {
                        "name": self.bootstrap_service_account,
                        "namespace": self.bootstrap_namespace,
                        "labels": ownership_labels,
                    },
                },
                {
                    "apiVersion": "rbac.authorization.k8s.io/v1",
                    "kind": "ClusterRole",
                    "metadata": {"name": self.bootstrap_role, "labels": ownership_labels},
                    "rules": [
                        {
                            "apiGroups": [""],
                            "resources": [
                                "namespaces",
                                "pods",
                                "serviceaccounts",
                                "secrets",
                                "resourcequotas",
                            ],
                            "verbs": ["get", "list", "watch", "create", "update", "patch", "delete"],
                        },
                        {
                            "apiGroups": ["networking.k8s.io"],
                            "resources": ["networkpolicies"],
                            "verbs": ["get", "list", "watch", "create", "update", "patch", "delete"],
                        },
                    ],
                },
                {
                    "apiVersion": "rbac.authorization.k8s.io/v1",
                    "kind": "ClusterRoleBinding",
                    "metadata": {"name": self.bootstrap_role, "labels": ownership_labels},
                    "subjects": [
                        {
                            "kind": "ServiceAccount",
                            "name": self.bootstrap_service_account,
                            "namespace": self.bootstrap_namespace,
                        }
                    ],
                    "roleRef": {
                        "apiGroup": "rbac.authorization.k8s.io",
                        "kind": "ClusterRole",
                        "name": self.bootstrap_role,
                    },
                },
            ],
        }
        self._kubectl_command(["apply", "-f", "-"], input_text=json.dumps(manifest))
        completed = self._kubectl_completed(
            [
                "-n",
                self.bootstrap_namespace,
                "create",
                "token",
                self.bootstrap_service_account,
                "--duration=1h",
            ]
        )
        if completed.returncode != 0:
            raise AcceptanceError(
                "runner.kubernetes_token_failed",
                "The disposable Kubernetes ServiceAccount token could not be created.",
                {"outputExcerpt": self.redactor.text(completed.stdout)[-1000:]},
            )
        token = completed.stdout.strip()
        if not token:
            raise AcceptanceError("runner.kubernetes_token_failed", "Kubernetes returned an empty token.")
        self.kubernetes_token = token
        self.redactor.add(token, "[REDACTED_KUBERNETES_TOKEN]")
        config = json.loads(
            self._kubectl_command(["config", "view", "--raw", "--minify", "--context", self.context, "-o", "json"])
        )
        clusters = config.get("clusters")
        if not isinstance(clusters, list) or len(clusters) != 1 or not isinstance(clusters[0], dict):
            raise AcceptanceError("runner.kubernetes_config_invalid", "Kubernetes kubeconfig omitted the active cluster.")
        cluster = json_object(clusters[0].get("cluster"), "Kubernetes cluster configuration")
        api_server = cluster.get("server")
        if not isinstance(api_server, str) or not api_server.startswith("https://"):
            raise AcceptanceError("runner.kubernetes_config_invalid", "Kubernetes API server must use HTTPS.")
        ca_data = cluster.get("certificate-authority-data")
        if isinstance(ca_data, str) and ca_data:
            try:
                ca_certificate = base64.b64decode(ca_data, validate=True).decode("utf-8")
            except (ValueError, UnicodeDecodeError) as error:
                raise AcceptanceError(
                    "runner.kubernetes_config_invalid",
                    f"Kubernetes CA data could not be decoded: {error}",
                ) from None
        else:
            ca_path = cluster.get("certificate-authority")
            if not isinstance(ca_path, str) or not ca_path:
                raise AcceptanceError("runner.kubernetes_config_invalid", "Kubernetes kubeconfig omitted its CA.")
            ca_certificate = pathlib.Path(ca_path).expanduser().read_text(encoding="utf-8")
        self.api_server = api_server.rstrip("/")
        self.ca_certificate = ca_certificate
        return {
            "bootstrapNamespace": self.bootstrap_namespace,
            "targetNamespace": self.target_namespace,
            "apiServerHost": urllib.parse.urlparse(self.api_server).netloc,
            "serviceAccount": self.bootstrap_service_account,
            "rbacScope": "cluster-wide disposable acceptance role",
            "ownershipLabels": ownership_labels,
        }

    def _ownership_labels(self) -> dict[str, str]:
        return {
            "synara.io/stage3-provider-acceptance": "true",
            "synara.io/stage3-provider-acceptance-owner": self.resource_owner,
        }

    def _wait_and_label_namespace(self, namespace: str) -> None:
        def namespace_probe() -> bool | None:
            completed = self._kubectl_completed(
                ["get", "namespace", namespace, "-o", "name"],
                cleanup_timeout=5.0,
            )
            return True if completed.returncode == 0 else None

        self.api.wait_until(f"Kubernetes Namespace {namespace}", namespace_probe, interval=0.2)
        for key, value in self._ownership_labels().items():
            self._kubectl_command(
                ["label", "namespace", namespace, f"{key}={value}", "--overwrite"],
                cleanup_timeout=10.0,
            )

    def _foundation_evidence(
        self,
        target_id: str,
        secret_name: Any,
        *,
        namespace: str | None = None,
        service_account: str | None = None,
    ) -> Mapping[str, Any]:
        namespace = namespace or self.target_namespace
        service_account = service_account or self.worker_service_account
        compact = target_id.replace("-", "")[:12]
        expected_secret = f"synara-agentd-{compact}"
        expected_quota = expected_secret
        if secret_name != expected_secret:
            raise AcceptanceError(
                "runner.kubernetes_foundation_mismatch",
                "The Pod did not reference the target-scoped registration Secret.",
                {"expectedSecret": expected_secret, "actualSecret": secret_name},
            )
        resources = {
            "serviceAccount": ("serviceaccount", service_account),
            "secret": ("secret", expected_secret),
            "resourceQuota": ("resourcequota", expected_quota),
            "networkPolicy": ("networkpolicy.networking.k8s.io", expected_quota),
        }
        evidence: dict[str, Any] = {}
        for label, (resource, name) in resources.items():
            actual = self._kubectl_command(
                ["-n", namespace, "get", resource, name, "-o", "jsonpath={.metadata.name}"]
            ).strip()
            if actual != name:
                raise AcceptanceError(
                    "runner.kubernetes_foundation_mismatch",
                    f"The Kubernetes Target foundation omitted {label}.",
                    {"expected": name, "actual": actual},
                )
            evidence[label] = actual
        return evidence

    def _wait_execution_pod(self, target_id: str, execution_id: str) -> dict[str, Any]:
        while True:
            pods = self._execution_pods(target_id, execution_id)
            active = [
                pod
                for pod in pods
                if not (
                    isinstance(pod.get("metadata"), dict)
                    and pod["metadata"].get("deletionTimestamp") is not None
                )
            ]
            running = [
                pod
                for pod in active
                if isinstance(pod.get("status"), dict) and pod["status"].get("phase") == "Running"
            ]
            if len(running) == 1:
                return running[0]
            if len(active) > 1:
                raise AcceptanceError(
                    "runner.kubernetes_pod_count_invalid",
                    "More than one execution-pinned Pod existed for one Execution.",
                    {
                        "targetId": target_id,
                        "executionId": execution_id,
                        "activePodCount": len(active),
                        "totalPodCount": len(pods),
                    },
                )
            self.deadline.sleep(0.2)

    def _execution_pods(self, target_id: str, execution_id: str) -> list[dict[str, Any]]:
        runtime = self._target_runtime(target_id)
        output = self._kubectl_command(
            [
                "-n",
                runtime["namespace"],
                "get",
                "pods",
                "-l",
                f"synara.io/execution-target-id={target_id},synara.io/execution-id={execution_id}",
                "-o",
                "json",
            ]
        )
        payload = json.loads(output)
        items = payload.get("items")
        if not isinstance(items, list) or not all(isinstance(item, dict) for item in items):
            raise AcceptanceError("runner.kubernetes_pods_invalid", "Kubernetes Pod list was invalid.")
        return items

    def _kubernetes_environment(self) -> dict[str, str]:
        environment = self._tool_environment()
        if self.kubeconfig is not None:
            environment["KUBECONFIG"] = str(self.kubeconfig)
        return environment

    def _kubectl_completed(
        self,
        arguments: Sequence[str],
        *,
        input_text: str | None = None,
        cleanup_timeout: float | None = None,
    ) -> subprocess.CompletedProcess[str]:
        timeout = cleanup_timeout or self.deadline.request_timeout(maximum=30.0)
        try:
            return subprocess.run(
                ["kubectl", "--context", self.context, *arguments],
                cwd=self.repo_root,
                env=self._kubernetes_environment(),
                input=input_text,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                text=True,
                timeout=timeout,
                check=False,
            )
        except (OSError, subprocess.TimeoutExpired) as error:
            raise AcceptanceError(
                "runner.kubernetes_command_failed",
                f"kubectl could not run: {self.redactor.text(str(error))}",
                {"command": ["kubectl", *arguments[:3]]},
            ) from None

    def _kubectl_command(
        self,
        arguments: Sequence[str],
        *,
        input_text: str | None = None,
        cleanup_timeout: float | None = None,
    ) -> str:
        completed = self._kubectl_completed(
            arguments,
            input_text=input_text,
            cleanup_timeout=cleanup_timeout,
        )
        output = self.redactor.text(completed.stdout)
        if completed.returncode != 0:
            raise AcceptanceError(
                "runner.kubernetes_command_failed",
                f"kubectl exited with status {completed.returncode}.",
                {
                    "command": ["kubectl", *arguments[:3]],
                    "exitCode": completed.returncode,
                    "outputExcerpt": output[-1000:],
                },
            )
        return output

    def _kind_completed(
        self,
        arguments: Sequence[str],
        *,
        cleanup_timeout: float | None = None,
    ) -> subprocess.CompletedProcess[str]:
        timeout = cleanup_timeout or self.deadline.request_timeout(maximum=max(30.0, self.deadline.remaining()))
        try:
            return subprocess.run(
                [self.options.kind_bin, *arguments],
                cwd=self.repo_root,
                env=self._kubernetes_environment(),
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                text=True,
                timeout=timeout,
                check=False,
            )
        except (OSError, subprocess.TimeoutExpired) as error:
            raise AcceptanceError(
                "runner.kind_command_failed",
                f"Kind could not run: {self.redactor.text(str(error))}",
                {"command": [self.options.kind_bin, *arguments[:3]]},
            ) from None

    def _kind_command(
        self,
        arguments: Sequence[str],
        *,
        log_path: pathlib.Path | None = None,
        maximum_timeout: float | None = None,
        cleanup_timeout: float | None = None,
    ) -> str:
        completed = self._kind_completed(
            arguments,
            cleanup_timeout=cleanup_timeout or maximum_timeout,
        )
        output = self.redactor.text(completed.stdout)
        if log_path is not None:
            log_path.parent.mkdir(parents=True, exist_ok=True)
            log_path.write_text(output, encoding="utf-8")
        if completed.returncode != 0:
            raise AcceptanceError(
                "runner.kind_command_failed",
                f"Kind exited with status {completed.returncode}.",
                {
                    "command": [self.options.kind_bin, *arguments[:3]],
                    "exitCode": completed.returncode,
                    "log": str(log_path) if log_path else None,
                    "outputExcerpt": output[-1000:],
                },
            )
        return output

    def _delete_cluster_resources(self) -> None:
        for namespace in sorted(self.owned_namespaces):
            if not self._kubernetes_resource_is_owned("namespace", namespace):
                continue
            self._kubectl_command(
                [
                    "delete",
                    "namespace",
                    namespace,
                    "--ignore-not-found",
                    "--wait=true",
                    "--timeout=30s",
                ],
                cleanup_timeout=40.0,
            )
        for resource in ("clusterrolebinding", "clusterrole"):
            if not self._kubernetes_resource_is_owned(resource, self.bootstrap_role):
                continue
            self._kubectl_command(
                ["delete", resource, self.bootstrap_role, "--ignore-not-found"],
                cleanup_timeout=20.0,
            )

    def _kubernetes_resource_is_owned(self, resource: str, name: str) -> bool:
        completed = self._kubectl_completed(
            ["get", resource, name, "-o", "json"],
            cleanup_timeout=8.0,
        )
        output = self.redactor.text(completed.stdout).strip()
        if completed.returncode != 0:
            if "not found" in output.lower() or "notfound" in output.lower():
                return False
            raise AcceptanceError(
                "runner.kubernetes_ownership_check_failed",
                "Kubernetes acceptance resource ownership could not be verified.",
                {"resource": resource, "name": name, "outputExcerpt": output[-500:]},
            )
        try:
            payload = json.loads(output)
            labels = json_object(
                json_object(payload.get("metadata"), "Kubernetes resource metadata").get("labels"),
                "Kubernetes resource labels",
            )
        except (AcceptanceError, json.JSONDecodeError) as error:
            raise AcceptanceError(
                "runner.kubernetes_ownership_check_failed",
                "Kubernetes acceptance resource ownership response was invalid.",
                {"resource": resource, "name": name, "message": str(error)},
            ) from None
        if labels.get("synara.io/stage3-provider-acceptance-owner") != self.resource_owner:
            raise AcceptanceError(
                "runner.kubernetes_resource_not_owned",
                "Refusing to delete a Kubernetes resource without the acceptance ownership label.",
                {"resource": resource, "name": name},
            )
        return True

    def _docker_cleanup_image(self, image: str) -> None:
        completed = self._docker_completed(["image", "rm", "-f", image], cleanup_timeout=30.0)
        output = self.redactor.text(completed.stdout)
        if completed.returncode != 0 and "no such image" not in output.lower():
            raise AcceptanceError(
                "runner.kubernetes_image_cleanup_failed",
                "The Kubernetes acceptance image could not be removed.",
                {"image": image, "outputExcerpt": output[-1000:]},
            )

    def _control_plane_environment(self) -> dict[str, str]:
        environment = super()._control_plane_environment()
        environment["SYNARA_KUBERNETES_RECONCILE_INTERVAL"] = "250ms"
        return environment


class MissingTargetDriver:
    def __init__(self, name: str) -> None:
        self.name = name
        self.api: APIClient | None = None
        self.lifecycle = STANDING_WORKER
        self.pending_interaction_recovery: str | None = None

    def prepare(self) -> Mapping[str, Any]:
        raise AcceptanceError(
            "runner.target_driver_missing",
            f"The {self.name} TargetDriver is not implemented.",
            {"target": self.name},
        )

    def start(self) -> Mapping[str, Any]:
        return self.prepare()

    def restart(self) -> Mapping[str, Any]:
        return self.prepare()

    def provision_target(
        self,
        _tenant_id: str,
        _organization_id: str,
        _provider: str,
    ) -> Mapping[str, Any]:
        return self.prepare()

    def replace_worker(
        self,
        _tenant_id: str,
        _target_id: str,
        _provider: str,
    ) -> Mapping[str, Any]:
        return self.prepare()

    def observe_execution(self, _target_id: str, _execution_id: str) -> Mapping[str, Any]:
        return self.prepare()

    def observe_terminal_execution(self, _target_id: str, _execution_id: str) -> Mapping[str, Any]:
        return self.prepare()

    def recover_pending_interaction(self, _target_id: str, _execution_id: str) -> Mapping[str, Any]:
        return self.prepare()

    def stop(self) -> None:
        return None

    def cleanup(self) -> None:
        return None


class AcceptanceSuite:
    def __init__(
        self,
        options: RunnerOptions,
        driver: TargetDriver,
        deadline: Deadline,
        redactor: SecretRedactor,
    ) -> None:
        self.options = options
        self.driver = driver
        self.deadline = deadline
        self.redactor = redactor
        self.state = ScenarioState()
        self.cases: list[dict[str, Any]] = []
        self._failed_cases: set[str] = set()

    @property
    def api(self) -> APIClient:
        if self.driver.api is None:
            raise AcceptanceError("runner.api_unavailable", "The TargetDriver did not expose a user API.")
        return self.driver.api

    def run(self) -> list[dict[str, Any]]:
        self._case("environment.target-prepare", "Prepare isolated Control Plane and Target runtime", self.driver.prepare)
        self._case(
            "environment.control-plane-start",
            "Start isolated Personal Control Plane",
            self.driver.start,
            requires=("environment.target-prepare",),
        )
        self._case(
            "identity.dev-login",
            "Authenticate through dev-login",
            self._dev_login,
            requires=("environment.control-plane-start",),
        )
        if self.options.suite == "real-provider-smoke":
            self._run_real_provider_smoke()
            return self.cases
        if self.options.failure_only:
            self._run_failure_only()
            return self.cases
        if self.driver.lifecycle.execution_pinned:
            self._case(
                "runtime.target-provision",
                "Provision the exact execution-pinned Target",
                self._provision_target,
                requires=("identity.dev-login",),
            )
            self._case(
                "resources.credential-project-session",
                "Create bound Credential, empty Repository Project, and Session",
                self._create_resources,
                requires=("runtime.target-provision",),
            )
            self._case(
                "runtime.worker-discovery",
                "Start an Approval barrier and discover its execution-pinned compatible Worker manifest",
                self._discover_worker,
                requires=("resources.credential-project-session",),
            )
            approval_requirement = "runtime.worker-discovery"
            if getattr(self.driver, "pending_interaction_recovery", None) is not None:
                self._case(
                    "recovery.pending-approval-runtime-loss",
                    "Force pending Approval runtime loss and verify recovery fencing",
                    self._recover_pending_approval_runtime,
                    requires=("runtime.worker-discovery",),
                )
                approval_requirement = "recovery.pending-approval-runtime-loss"
            self._case(
                "fixture.approval-resolution",
                "Resolve Provider approval through the user API",
                self._approval_resolution,
                requires=(approval_requirement,),
            )
            self._case(
                "fixture.text-tool-usage-artifact",
                "Run text, tool, usage, Artifact, and Credential fixture flow",
                self._text_tool_usage_artifact,
                requires=("fixture.approval-resolution",),
            )
        else:
            self._case(
                "runtime.worker-discovery",
                "Provision the exact Target and discover a real compatible Worker manifest",
                self._provision_standing_target_and_discover_worker,
                requires=("identity.dev-login",),
            )
            self._case(
                "resources.credential-project-session",
                "Create bound Credential, empty Repository Project, and Session",
                self._create_resources,
                requires=("runtime.worker-discovery",),
            )
            self._case(
                "fixture.text-tool-usage-artifact",
                "Run text, tool, usage, Artifact, and Credential fixture flow",
                self._text_tool_usage_artifact,
                requires=("resources.credential-project-session",),
            )
            self._case(
                "fixture.approval-resolution",
                "Resolve Provider approval through the user API",
                self._approval_resolution,
                requires=("fixture.text-tool-usage-artifact",),
            )
        self._case(
            "fixture.terminal-large-log",
            "Persist a large Terminal stream as a bounded preview and segmented Artifacts",
            self._terminal_large_log,
            requires=("fixture.text-tool-usage-artifact", "fixture.approval-resolution"),
        )
        self._case(
            "fixture.user-input-resolution",
            "Resolve Provider user input through the user API",
            self._user_input_resolution,
            requires=("fixture.terminal-large-log",),
        )
        self._case(
            "fixture.provider-error",
            "Persist deterministic Provider failure",
            self._provider_error,
            requires=("fixture.user-input-resolution",),
        )
        recovery_requirement = "fixture.provider-error"
        for failure_case in self.options.failure_cases:
            metadata = FAILURE_CASE_METADATA[failure_case]
            case_id = metadata["id"]
            self._case(
                case_id,
                metadata["name"],
                lambda failure_case=failure_case: self._execute_failure_case(failure_case),
                requires=(recovery_requirement,),
            )
            recovery_requirement = case_id
        if self.driver.lifecycle.managed_replacement:
            self._case(
                "recovery.worker-replacement",
                "Replace the managed Worker and verify a new agentd incarnation",
                self._replace_worker,
                requires=("fixture.provider-error",),
            )
            self._case(
                "recovery.post-replacement-workspace-turn",
                "Execute immediately on the replacement Worker and verify persisted Workspace content",
                self._post_replacement_workspace_turn,
                requires=("recovery.worker-replacement",),
            )
            recovery_requirement = "recovery.post-replacement-workspace-turn"
        if self.options.restart_control_plane:
            self._case(
                "recovery.control-plane-restart",
                "Restart Control Plane with persisted state",
                self._restart_control_plane,
                requires=(recovery_requirement,),
            )
            second_requires = ("recovery.control-plane-restart",)
        else:
            self._fail_case(
                "recovery.control-plane-restart",
                "Restart Control Plane with persisted state",
                "runner.restart_disabled",
                "Control Plane restart was disabled by the caller.",
                {"requiredInputs": ["omit --no-restart-control-plane"]},
            )
            self._failed_cases.add("recovery.control-plane-restart")
            second_requires = ("recovery.control-plane-restart",)
        self._case(
            "fixture.second-turn-continuity",
            "Run a second post-restart Turn and verify continuity",
            self._second_turn_continuity,
            requires=second_requires,
        )
        return self.cases

    def _run_real_provider_smoke(self) -> None:
        self._case(
            "runtime.target-provision",
            "Provision the exact Target for the real Provider",
            self._provision_target,
            requires=("identity.dev-login",),
        )
        self._case(
            "resources.real-provider-project-session",
            "Create an ambient-auth real Provider Project and Session",
            self._create_resources,
            requires=("runtime.target-provision",),
        )
        self._case(
            "real-provider.turn-1-start",
            "Start the first real Provider Turn",
            self._start_real_provider_turn,
            requires=("resources.real-provider-project-session",),
        )
        self._case(
            "runtime.real-provider-worker-discovery",
            "Discover the compatible real Provider Worker manifest",
            self._discover_real_provider_worker,
            requires=("real-provider.turn-1-start",),
        )
        self._case(
            "real-provider.turn-1",
            "Complete the first real Provider Turn with the exact marker",
            self._complete_first_real_provider_turn,
            requires=("runtime.real-provider-worker-discovery",),
        )
        if self.options.restart_control_plane:
            self._case(
                "recovery.control-plane-restart",
                "Restart Control Plane with persisted real Provider state",
                self._restart_control_plane,
                requires=("real-provider.turn-1",),
            )
        else:
            self._fail_case(
                "recovery.control-plane-restart",
                "Restart Control Plane with persisted real Provider state",
                "runner.restart_disabled",
                "Control Plane restart was disabled by the caller.",
                {"requiredInputs": ["omit --no-restart-control-plane"]},
            )
            self._failed_cases.add("recovery.control-plane-restart")
        self._case(
            "real-provider.turn-2-continuity",
            "Run a second post-restart real Provider Turn and verify continuity",
            self._real_provider_second_turn_continuity,
            requires=("recovery.control-plane-restart",),
        )

    def _run_failure_only(self) -> None:
        if self.driver.lifecycle.execution_pinned:
            self._case(
                "runtime.target-provision",
                "Provision the exact execution-pinned Target",
                self._provision_target,
                requires=("identity.dev-login",),
            )
            self._case(
                "resources.credential-project-session",
                "Create bound Credential, empty Repository Project, and Session",
                self._create_resources,
                requires=("runtime.target-provision",),
            )
            self._case(
                "runtime.worker-discovery",
                "Start an Approval barrier and discover its execution-pinned compatible Worker manifest",
                self._discover_worker,
                requires=("resources.credential-project-session",),
            )
            self._case(
                "fixture.baseline-approval",
                "Resolve the baseline Approval before fault injection",
                self._approval_resolution,
                requires=("runtime.worker-discovery",),
            )
            requirement = "fixture.baseline-approval"
        else:
            self._case(
                "runtime.worker-discovery",
                "Provision the exact Target and discover a real compatible Worker manifest",
                self._provision_standing_target_and_discover_worker,
                requires=("identity.dev-login",),
            )
            self._case(
                "resources.credential-project-session",
                "Create bound Credential, empty Repository Project, and Session",
                self._create_resources,
                requires=("runtime.worker-discovery",),
            )
            self._case(
                "fixture.baseline-smoke",
                "Run a baseline text/usage Turn before fault injection",
                self._baseline_smoke,
                requires=("resources.credential-project-session",),
            )
            requirement = "fixture.baseline-smoke"

        for failure_case in self.options.failure_cases:
            metadata = FAILURE_CASE_METADATA[failure_case]
            case_id = metadata["id"]
            self._case(
                case_id,
                metadata["name"],
                lambda failure_case=failure_case: self._execute_failure_case(failure_case),
                requires=(requirement,),
            )
            requirement = case_id
        self._case(
            "fixture.post-failure-continuity",
            "Run a final text/usage Turn after fault injection",
            self._baseline_smoke,
            requires=(requirement,),
        )

    def record_cleanup_failure(self, error: AcceptanceError) -> None:
        self._fail_case(
            "environment.cleanup",
            "Clean isolated Target resources",
            error.code,
            str(error),
            error.evidence,
        )

    def record_cleanup_success(self, evidence: Mapping[str, Any] | None = None) -> None:
        now = utc_now()
        self.cases.append(
            {
                "id": "environment.cleanup",
                "name": "Clean exact runner-owned Target resources",
                "status": "pass",
                "startedAt": now,
                "finishedAt": now,
                "durationMs": 0,
                "evidence": self.redactor.value(
                    dict(
                        evidence
                        or {
                            "target": self.driver.name,
                            "driverCleanupCompleted": True,
                            "broadCleanupUsed": False,
                        }
                    )
                ),
            }
        )

    def _case(
        self,
        case_id: str,
        name: str,
        operation: Callable[[], Mapping[str, Any] | None],
        requires: Sequence[str] = (),
    ) -> None:
        missing = [required for required in requires if required in self._failed_cases]
        if missing:
            self._fail_case(
                case_id,
                name,
                "runner.prerequisite_failed",
                "A required acceptance case failed.",
                {"failedPrerequisites": missing},
            )
            self._failed_cases.add(case_id)
            return
        started_at = utc_now()
        started = time.monotonic()
        case: dict[str, Any] = {"id": case_id, "name": name, "startedAt": started_at}
        try:
            evidence = operation() or {}
            case.update(
                {
                    "status": "pass",
                    "finishedAt": utc_now(),
                    "durationMs": elapsed_ms(started),
                    "evidence": self.redactor.value(dict(evidence)),
                }
            )
        except AcceptanceUnsupported as error:
            case.update(
                {
                    "status": "unsupported",
                    "finishedAt": utc_now(),
                    "durationMs": elapsed_ms(started),
                    "reasonCode": error.code,
                    "message": self.redactor.text(str(error)),
                    "evidence": self.redactor.value(error.evidence),
                }
            )
        except AcceptanceError as error:
            evidence = dict(error.evidence)
            diagnostics = self._collect_failure_diagnostics(case_id)
            if diagnostics:
                evidence["diagnostics"] = diagnostics
            case.update(
                {
                    "status": "fail",
                    "finishedAt": utc_now(),
                    "durationMs": elapsed_ms(started),
                    "reasonCode": error.code,
                    "message": self.redactor.text(str(error)),
                    "evidence": self.redactor.value(evidence),
                }
            )
            self._failed_cases.add(case_id)
        except RunnerInterrupted as error:
            case.update(
                {
                    "status": "fail",
                    "finishedAt": utc_now(),
                    "durationMs": elapsed_ms(started),
                    "reasonCode": "runner.interrupted",
                    "message": str(error),
                    "evidence": {"signal": error.signal_name, "signalNumber": error.signum},
                }
            )
            self._failed_cases.add(case_id)
            self.cases.append(case)
            raise
        except Exception as error:  # Keep a machine-readable report for unexpected runner defects.
            case.update(
                {
                    "status": "fail",
                    "finishedAt": utc_now(),
                    "durationMs": elapsed_ms(started),
                    "reasonCode": "runner.internal_error",
                    "message": self.redactor.text(str(error) or error.__class__.__name__),
                    "evidence": {
                        "exceptionType": error.__class__.__name__,
                        "traceback": self.redactor.text(traceback.format_exc(limit=8)),
                    },
                }
            )
            self._failed_cases.add(case_id)
        if case.get("status") not in CASE_STATUSES:
            raise RuntimeError(f"invalid acceptance case status: {case.get('status')}")
        self.cases.append(case)

    def _collect_failure_diagnostics(self, case_id: str) -> Mapping[str, Any]:
        collector = getattr(self.driver, "collect_failure_diagnostics", None)
        if not callable(collector):
            return {}
        try:
            return dict(collector(case_id))
        except Exception as error:
            return {
                "captureFailed": True,
                "message": self.redactor.text(str(error) or error.__class__.__name__),
            }

    def record_interruption(self, error: RunnerInterrupted) -> None:
        if any(case.get("reasonCode") == "runner.interrupted" for case in self.cases):
            return
        self._fail_case(
            "runner.interrupted",
            "Handle external interruption",
            "runner.interrupted",
            str(error),
            {"signal": error.signal_name, "signalNumber": error.signum},
        )

    def _fail_case(
        self,
        case_id: str,
        name: str,
        reason_code: str,
        message: str,
        evidence: Mapping[str, Any] | None = None,
    ) -> None:
        self.cases.append(
            {
                "id": case_id,
                "name": name,
                "status": "fail",
                "startedAt": utc_now(),
                "finishedAt": utc_now(),
                "durationMs": 0,
                "reasonCode": reason_code,
                "message": message,
                "evidence": self.redactor.value(dict(evidence or {})),
            }
        )

    def _dev_login(self) -> Mapping[str, Any]:
        response = json_object(
            self.api.request(
                "POST",
                "/v1/auth/dev-login",
                {"email": "stage3-acceptance@localhost.invalid", "displayName": "Stage 3 Acceptance"},
            ),
            "dev-login",
        )
        user = json_object(response.get("user"), "dev-login.user")
        tenant_id = user.get("activeTenantId")
        if not isinstance(tenant_id, str) or not tenant_id:
            raise AcceptanceError("runner.active_tenant_missing", "dev-login did not return an active Tenant.")
        organizations = json_items(
            self.api.request("GET", f"/v1/tenants/{tenant_id}/organizations"),
            "organizations",
        )
        roots = [item for item in organizations if item.get("kind") == "root"]
        if len(roots) != 1 or not isinstance(roots[0].get("id"), str):
            raise AcceptanceError(
                "runner.bootstrap_organization_missing",
                "Expected exactly one bootstrap root Organization.",
                {"rootOrganizationCount": len(roots)},
            )
        organization = roots[0]
        self.state.tenant_id = tenant_id
        self.state.organization_id = str(organization["id"])
        return {
            "tenantId": tenant_id,
            "userId": user.get("userId"),
            "authenticated": response.get("authenticated"),
            "organization": {
                "id": organization.get("id"),
                "slug": organization.get("slug"),
                "kind": organization.get("kind"),
            },
        }

    def _provision_target(self) -> Mapping[str, Any]:
        tenant_id = self._required("tenant_id")
        organization_id = self._required("organization_id")
        target = json_object(
            self.driver.provision_target(tenant_id, organization_id, self.options.provider),
            "provisioned execution target",
        )
        target_id = target.get("id")
        if (
            not isinstance(target_id, str)
            or target.get("organizationId") != organization_id
            or target.get("kind") != self.driver.name
        ):
            raise AcceptanceError(
                "runner.target_binding_invalid",
                "The provisioned Target did not retain the requested kind and Organization scope.",
                {"target": self._target_summary(target), "expectedOrganizationId": organization_id},
            )
        self.state.target_id = target_id
        return {
            "target": self._target_summary(target),
            "driverEvidence": target.get("driverEvidence"),
        }

    def _provision_standing_target_and_discover_worker(self) -> Mapping[str, Any]:
        target = self._provision_target()
        discovered = self._discover_worker()
        return {**target, **discovered}

    def _discover_worker(self) -> Mapping[str, Any]:
        target_id = self._required("target_id")
        readiness_barrier: Mapping[str, Any] | None = None
        if self.driver.lifecycle.execution_pinned:
            readiness_barrier = self._begin_approval_readiness_barrier()
        discovered = self._wait_compatible_manifest(target_id)
        evidence = self._worker_manifest_evidence(discovered)
        if readiness_barrier is not None:
            evidence["readinessBarrier"] = dict(readiness_barrier)
            execution_id = readiness_barrier.get("executionId")
            if isinstance(execution_id, str) and execution_id:
                target_evidence = self.driver.observe_execution(target_id, execution_id)
                if target_evidence:
                    evidence["targetExecution"] = dict(target_evidence)
        return evidence

    @staticmethod
    def _worker_manifest_evidence(discovered: Mapping[str, Any]) -> dict[str, Any]:
        manifest = json_object(discovered.get("manifest"), "Worker manifest discovery")
        provider = json_object(discovered.get("provider"), "Worker Provider discovery")
        return {
            "manifestId": manifest.get("manifestId"),
            "workerStatusCounts": manifest.get("workerStatusCounts"),
            "workerProtocol": manifest.get("workerProtocol"),
            "runtimeEvent": manifest.get("runtimeEvent"),
            "workerBuild": manifest.get("workerBuild"),
            "provider": {
                "provider": provider.get("provider"),
                "supportTier": provider.get("supportTier"),
                "compatibilityStatus": provider.get("compatibilityStatus"),
                "runtime": provider.get("runtime"),
                "releasePolicy": provider.get("releasePolicy"),
            },
        }

    def _begin_approval_readiness_barrier(self) -> Mapping[str, Any]:
        if self.state.pending_approval is not None:
            raise AcceptanceError(
                "runner.approval_barrier_already_started",
                "The execution-pinned Approval readiness barrier was already started.",
            )
        turn = self._create_turn("[approval]")
        turn_id = turn.get("id")
        if not isinstance(turn_id, str) or not turn_id:
            raise AcceptanceError("runner.turn_id_missing", "Approval readiness Turn did not return an ID.")
        interaction = self._wait_for_interaction(turn_id, "approval")
        execution_id = interaction.get("executionId")
        request_id = interaction.get("requestId")
        if not isinstance(execution_id, str) or not execution_id or not isinstance(request_id, str) or not request_id:
            raise AcceptanceError(
                "runner.approval_barrier_invalid",
                "The execution-pinned Approval readiness barrier omitted its Execution or Request ID.",
                {"turnId": turn_id, "interactionId": interaction.get("id")},
            )
        self.state.pending_approval = {"turn": turn, "interaction": interaction}
        return {
            "kind": "approval",
            "turnId": turn_id,
            "executionId": execution_id,
            "requestId": request_id,
            "interactionId": interaction.get("id"),
        }

    def _wait_compatible_manifest(self, target_id: str) -> dict[str, Any]:
        tenant_id = self._required("tenant_id")

        def manifest_probe() -> dict[str, Any] | None:
            manifests = json_items(
                self.api.request("GET", f"/v1/tenants/{tenant_id}/worker-manifests"),
                "worker-manifests",
            )
            for manifest in manifests:
                counts = manifest.get("workerStatusCounts")
                if manifest.get("executionTargetId") != target_id or not isinstance(counts, dict):
                    continue
                if not isinstance(counts.get("online"), int) or counts["online"] < 1:
                    continue
                worker_protocol = manifest.get("workerProtocol")
                runtime_event = manifest.get("runtimeEvent")
                if (
                    not isinstance(worker_protocol, dict)
                    or not isinstance(runtime_event, dict)
                    or not self._version_range_contains(worker_protocol, 2)
                    or not self._version_range_contains(runtime_event, 2)
                ):
                    raise AcceptanceError(
                        "runner.worker_manifest_protocol_incompatible",
                        "The real Worker manifest did not include Worker Protocol and Runtime Event version 2.",
                        {"manifestId": manifest.get("manifestId")},
                    )
                providers = manifest.get("providers")
                if not isinstance(providers, list):
                    continue
                provider = next(
                    (
                        item
                        for item in providers
                        if isinstance(item, dict)
                        and str(item.get("provider", "")).lower() == self.options.provider.lower()
                    ),
                    None,
                )
                if provider is None:
                    continue
                runtime = provider.get("runtime")
                release = provider.get("releasePolicy")
                if (
                    provider.get("compatibilityStatus") != "compatible"
                    or not isinstance(runtime, dict)
                    or runtime.get("available") is not True
                    or runtime.get("compatible") is not True
                    or not isinstance(release, dict)
                    or release.get("enabled") is not True
                ):
                    raise AcceptanceError(
                        "runner.provider_manifest_incompatible",
                        "The real Worker manifest did not expose a compatible enabled Provider.",
                        {"provider": self.redactor.value(provider), "manifestId": manifest.get("manifestId")},
                    )
                return {"manifest": manifest, "provider": provider}
            return None

        return self.api.wait_until("an online compatible Worker manifest", manifest_probe)

    @staticmethod
    def _version_range_contains(value: Mapping[str, Any], version: int) -> bool:
        minimum = value.get("minimum")
        maximum = value.get("maximum")
        return isinstance(minimum, int) and isinstance(maximum, int) and minimum <= version <= maximum

    def _create_resources(self) -> Mapping[str, Any]:
        tenant_id = self._required("tenant_id")
        organization_id = self._required("organization_id")
        target_id = self._required("target_id")
        real_provider_smoke = self.options.suite == "real-provider-smoke"
        credential: dict[str, Any] | None = None
        credential_id: str | None = None
        if not real_provider_smoke:
            credential = json_object(
                self.api.request(
                    "POST",
                    f"/v1/tenants/{tenant_id}/credentials",
                    {
                        "organizationId": organization_id,
                        "name": "Stage 3 Provider Acceptance Fixture",
                        "purpose": "provider",
                        "provider": self.options.provider,
                        "credentialType": "acceptance_fixture",
                        "payload": {"acceptanceToken": FIXTURE_CREDENTIAL_SENTINEL},
                    },
                    expected=(201,),
                ),
                "credential",
            )
            credential_id = credential.get("id")
            if not isinstance(credential_id, str):
                raise AcceptanceError("runner.credential_id_missing", "Credential API did not return an ID.")
        project = json_object(
            self.api.request(
                "POST",
                f"/v1/tenants/{tenant_id}/organizations/{organization_id}/projects",
                {
                    "name": "Stage 3 Provider Acceptance",
                    "repositoryUrl": None,
                    "defaultBranch": "main",
                    "visibility": "organization",
                },
                expected=(201,),
            ),
            "project",
        )
        if project.get("repositoryUrl") is not None:
            raise AcceptanceError(
                "runner.project_repository_not_empty",
                "Acceptance Project unexpectedly has a Repository URL.",
                {"projectId": project.get("id")},
            )
        project_id = project.get("id")
        if not isinstance(project_id, str):
            raise AcceptanceError("runner.project_id_missing", "Project API did not return an ID.")
        session_input: dict[str, Any] = {
            "title": (
                "Stage 3 Real Provider Smoke"
                if real_provider_smoke
                else "Stage 3 Provider Acceptance"
            ),
            "visibility": "project",
            "provider": self.options.provider,
            "executionTargetId": target_id,
        }
        if credential_id is not None:
            session_input.update(
                {
                    "model": "stage3-acceptance-fixture",
                    "providerCredentialId": credential_id,
                }
            )
        session = json_object(
            self.api.request(
                "POST",
                f"/v1/projects/{project_id}/sessions",
                session_input,
                expected=(201,),
            ),
            "session",
        )
        session_id = session.get("id")
        if not isinstance(session_id, str):
            raise AcceptanceError("runner.session_id_missing", "Session API did not return an ID.")
        if (
            session.get("executionTargetId") != target_id
            or session.get("providerCredentialId") != credential_id
        ):
            raise AcceptanceError(
                "runner.session_binding_mismatch",
                "Session did not retain the requested Target and Credential bindings.",
                {
                    "executionTargetId": session.get("executionTargetId"),
                    "providerCredentialId": session.get("providerCredentialId"),
                },
            )
        self.state.credential_id = credential_id
        self.state.project_id = project_id
        self.state.session_id = session_id
        self.state.last_sequence = int(session.get("lastEventSequence") or 0)
        return {
            "credential": (
                {
                    "id": credential_id,
                    "provider": credential.get("provider"),
                    "credentialType": credential.get("credentialType"),
                    "version": credential.get("version"),
                    "organizationId": credential.get("organizationId"),
                }
                if credential is not None
                else {
                    "delivery": "ambient-auth",
                    "providerCredentialId": session.get("providerCredentialId"),
                }
            ),
            "project": {
                "id": project_id,
                "organizationId": project.get("organizationId"),
                "repositoryUrl": project.get("repositoryUrl"),
            },
            "session": {
                "id": session_id,
                "provider": session.get("provider"),
                "executionTargetId": session.get("executionTargetId"),
                "providerCredentialId": session.get("providerCredentialId"),
                "lastEventSequence": session.get("lastEventSequence"),
            },
        }

    def _start_real_provider_turn(self) -> Mapping[str, Any]:
        if self.state.pending_real_turn_id is not None:
            raise AcceptanceError(
                "runner.real_provider_turn_already_started",
                "The first real Provider smoke Turn was already started.",
                {"turnId": self.state.pending_real_turn_id},
            )
        marker = self._real_provider_marker()
        turn = self._create_turn(
            "This is an automated Synara runtime acceptance check. "
            f"Reply with exactly {marker} and no other text."
        )
        turn_id = turn.get("id")
        if not isinstance(turn_id, str) or not turn_id:
            raise AcceptanceError(
                "runner.turn_id_missing",
                "The first real Provider smoke Turn did not return an ID.",
            )
        self.state.pending_real_turn_id = turn_id
        return {
            "turnId": turn_id,
            "expectedMarker": marker,
            "responseContract": "exact marker with optional surrounding whitespace only",
        }

    def _discover_real_provider_worker(self) -> Mapping[str, Any]:
        turn_id = self._pending_real_turn_id()
        created = self._wait_for_turn_created(turn_id)
        execution_id = self._event_execution_id(created)
        target_id = self._required("target_id")
        discovered = self._wait_compatible_manifest(target_id)
        evidence = self._worker_manifest_evidence(discovered)
        evidence.update(
            {
                "turnId": turn_id,
                "executionId": execution_id,
                "turnCreatedEvent": self._event_summary(created),
            }
        )
        target_evidence = self.driver.observe_execution(target_id, execution_id)
        if target_evidence:
            evidence["targetExecution"] = dict(target_evidence)
        return evidence

    def _complete_first_real_provider_turn(self) -> Mapping[str, Any]:
        turn_id = self._pending_real_turn_id()
        terminal, events = self._wait_for_turn_terminal(turn_id, "execution.completed")
        evidence = self._real_provider_turn_evidence(
            turn_id,
            terminal,
            events,
            self._real_provider_marker(),
            expected_resume_strategy="authoritative-history",
            expected_resume_reason="cursor_absent",
        )
        worker_id, generation = self._event_worker_identity(terminal)
        self.state.first_worker_id = worker_id
        self.state.first_generation = generation
        self.state.pending_real_turn_id = None
        return evidence

    def _real_provider_second_turn_continuity(self) -> Mapping[str, Any]:
        if self.state.pending_real_turn_id is not None:
            raise AcceptanceError(
                "runner.real_provider_turn_pending",
                "The first real Provider smoke Turn was still pending before continuity verification.",
                {"turnId": self.state.pending_real_turn_id},
            )
        before = self.state.pre_restart_sequence
        if before is None:
            events = self._all_events()
            before = int(events[-1]["sequence"]) if events else 0
        turn = self._create_turn(
            "Repeat your immediately previous answer exactly. Output no additional text."
        )
        turn_id = turn.get("id")
        if not isinstance(turn_id, str) or not turn_id:
            raise AcceptanceError(
                "runner.turn_id_missing",
                "The second real Provider smoke Turn did not return an ID.",
            )
        terminal, turn_events = self._wait_for_turn_terminal(turn_id, "execution.completed")
        evidence = self._real_provider_turn_evidence(
            turn_id,
            terminal,
            turn_events,
            self._real_provider_marker(),
            expected_resume_strategy="native-cursor",
            expected_resume_reason="cursor_usable",
        )
        all_events = self._all_events()
        sequences = [int(event["sequence"]) for event in all_events]
        expected_sequences = list(range(1, sequences[-1] + 1)) if sequences else []
        if sequences != expected_sequences:
            raise AcceptanceError(
                "runner.session_sequence_discontinuous",
                "Session Event Sequence was not contiguous across the real Provider Control Plane restart.",
                {"sequences": sequences},
            )
        if int(terminal["sequence"]) <= before:
            raise AcceptanceError(
                "runner.session_sequence_not_advanced",
                "The second real Provider Turn did not advance Session Event Sequence.",
                {"before": before, "after": terminal.get("sequence")},
            )
        worker_id, generation = self._event_worker_identity(terminal)
        evidence.update(
            {
                "preRestartSequence": before,
                "terminalSequence": terminal.get("sequence"),
                "sessionSequenceRange": self._sequence_range(all_events),
                "preRestartWorkerId": self.state.first_worker_id,
                "workerIdChangedAfterRestart": worker_id != self.state.first_worker_id,
                "firstGeneration": self.state.first_generation,
                "generation": generation,
                "continuityAssertion": "native Provider cursor restored the immediately previous answer",
            }
        )
        return evidence

    def _real_provider_marker(self) -> str:
        session_id = self._required("session_id")
        provider = re.sub(r"[^A-Za-z0-9]+", "_", self.options.provider).strip("_").upper()
        digest = hashlib.sha256(
            f"synara-real-provider-smoke-v1\0{session_id}\0{self.options.provider}".encode("utf-8")
        ).hexdigest()[:16].upper()
        return f"SYNARA_REAL_PROVIDER_SMOKE_{provider}_{digest}"

    def _pending_real_turn_id(self) -> str:
        turn_id = self.state.pending_real_turn_id
        if not isinstance(turn_id, str) or not turn_id:
            raise AcceptanceError(
                "runner.real_provider_turn_missing",
                "The first real Provider smoke Turn was not available.",
            )
        return turn_id

    def _real_provider_turn_evidence(
        self,
        turn_id: str,
        terminal: Mapping[str, Any],
        events: Sequence[Mapping[str, Any]],
        expected_marker: str,
        *,
        expected_resume_strategy: str,
        expected_resume_reason: str,
    ) -> dict[str, Any]:
        event_types = [str(event.get("eventType")) for event in events]
        required_types = {
            "turn.created",
            "execution.leased",
            "execution.started",
            "content.delta",
            "execution.completed",
        }
        missing = sorted(required_types.difference(event_types))
        if missing:
            raise AcceptanceError(
                "runner.real_provider_events_missing",
                "The real Provider Turn omitted required product-path Runtime Events.",
                {"turnId": turn_id, "missingEventTypes": missing, "eventTypes": event_types},
            )
        legacy_output = [
            self._event_summary(event)
            for event in events
            if event.get("eventType") == "runtime.output.delta"
        ]
        if legacy_output:
            raise AcceptanceError(
                "runner.real_provider_legacy_runtime_event",
                "The real Provider Turn emitted legacy assistant output instead of Runtime Event v2.",
                {"turnId": turn_id, "events": legacy_output},
            )

        assistant_deltas: list[str] = []
        assistant_sequences: list[int] = []
        for event in events:
            if event.get("eventType") != "content.delta":
                continue
            payload = json_object(event.get("payload"), "real Provider content.delta payload")
            if payload.get("streamKind") != "assistant_text":
                continue
            if event.get("eventVersion") != 2:
                raise AcceptanceError(
                    "runner.real_provider_runtime_event_version_invalid",
                    "The real Provider assistant output was not persisted as Runtime Event version 2.",
                    {"turnId": turn_id, "event": self._event_summary(event)},
                )
            delta = payload.get("delta")
            if not isinstance(delta, str):
                raise AcceptanceError(
                    "runner.real_provider_assistant_delta_invalid",
                    "The real Provider assistant content.delta omitted its text delta.",
                    {"turnId": turn_id, "event": self._event_summary(event)},
                )
            assistant_deltas.append(delta)
            sequence = event.get("sequence")
            if isinstance(sequence, int):
                assistant_sequences.append(sequence)
        if not assistant_deltas:
            raise AcceptanceError(
                "runner.real_provider_assistant_text_missing",
                "The real Provider Turn completed without canonical assistant text.",
                {"turnId": turn_id, "eventTypes": event_types},
            )
        assistant_text = "".join(assistant_deltas)
        normalized_text = assistant_text.strip()
        if normalized_text != expected_marker:
            raise AcceptanceError(
                "runner.real_provider_marker_mismatch",
                "The real Provider assistant response did not match the expected marker.",
                {
                    "turnId": turn_id,
                    "expectedMarker": expected_marker,
                    "assistantTextLength": len(assistant_text),
                    "assistantTextSha256": hashlib.sha256(assistant_text.encode("utf-8")).hexdigest(),
                    "assistantTextPreview": self.redactor.text(normalized_text[:256]),
                },
            )

        leased_events = [event for event in events if event.get("eventType") == "execution.leased"]
        if len(leased_events) != 1:
            raise AcceptanceError(
                "runner.real_provider_resume_decision_missing",
                "The real Provider Turn did not contain exactly one Provider resume decision.",
                {"turnId": turn_id, "executionLeasedEvents": len(leased_events)},
            )
        leased_payload = json_object(leased_events[0].get("payload"), "execution.leased payload")
        provider_resume = json_object(leased_payload.get("providerResume"), "Provider resume decision")
        expected_resume = {
            "requestedStrategy": "native-cursor",
            "selectedStrategy": expected_resume_strategy,
            "reasonCode": expected_resume_reason,
        }
        actual_resume = {key: provider_resume.get(key) for key in expected_resume}
        if actual_resume != expected_resume:
            raise AcceptanceError(
                "runner.real_provider_resume_decision_mismatch",
                "The real Provider Turn used an unexpected resume strategy.",
                {"turnId": turn_id, "expected": expected_resume, "actual": actual_resume},
            )

        terminal_payload = terminal.get("payload")
        terminal_output_matches: bool | None = None
        if isinstance(terminal_payload, dict) and isinstance(terminal_payload.get("output"), dict):
            output_text = terminal_payload["output"].get("text")
            if isinstance(output_text, str):
                terminal_output_matches = output_text.strip() == expected_marker
                if not terminal_output_matches:
                    raise AcceptanceError(
                        "runner.real_provider_terminal_output_mismatch",
                        "The real Provider terminal output disagreed with canonical assistant Runtime Events.",
                        {
                            "turnId": turn_id,
                            "outputTextLength": len(output_text),
                            "outputTextSha256": hashlib.sha256(output_text.encode("utf-8")).hexdigest(),
                        },
                    )

        worker_id, generation = self._event_worker_identity(terminal)
        return {
            "turnId": turn_id,
            "executionId": terminal.get("executionId"),
            "workerId": worker_id,
            "generation": generation,
            "expectedMarker": expected_marker,
            "markerMatched": True,
            "assistantDeltaCount": len(assistant_deltas),
            "assistantTextBytes": len(assistant_text.encode("utf-8")),
            "assistantTextSha256": hashlib.sha256(assistant_text.encode("utf-8")).hexdigest(),
            "assistantSequenceRange": {
                "first": min(assistant_sequences) if assistant_sequences else None,
                "last": max(assistant_sequences) if assistant_sequences else None,
            },
            "terminalOutputMatched": terminal_output_matches,
            "providerResume": actual_resume,
            "eventTypes": event_types,
            "sequenceRange": self._sequence_range(events),
        }

    def _text_tool_usage_artifact(self) -> Mapping[str, Any]:
        turn = self._create_turn("[text] [tool] [usage] [artifact] [credential]")
        terminal, events = self._wait_for_turn_terminal(str(turn["id"]), "execution.completed")
        event_types = [str(event.get("eventType")) for event in events]
        required_types = {
            "content.delta",
            "item.started",
            "item.completed",
            "thread.token-usage.updated",
            "artifact.ready",
            "execution.completed",
        }
        missing = sorted(required_types.difference(event_types))
        if missing:
            raise AcceptanceError(
                "runner.fixture_events_missing",
                "The combined fixture Turn omitted required events.",
                {"missingEventTypes": missing, "eventTypes": event_types},
            )
        usage = next(event for event in events if event.get("eventType") == "thread.token-usage.updated")
        usage_payload = json_object(usage.get("payload"), "usage event payload")
        usage_value = json_object(usage_payload.get("usage"), "usage event payload.usage")
        if usage_value.get("usedTokens") != 42:
            raise AcceptanceError(
                "runner.fixture_usage_mismatch",
                "The deterministic usage event did not contain 42 used tokens.",
                {"usage": usage_value},
            )
        artifacts = json_items(
            self.api.request("GET", f"/v1/sessions/{self._required('session_id')}/artifacts"),
            "artifacts",
        )
        artifact = next(
            (
                item
                for item in artifacts
                if item.get("originalName") == "artifact.txt"
                and item.get("kind") == "generated_file"
                and item.get("status") == "ready"
            ),
            None,
        )
        if artifact is None:
            raise AcceptanceError(
                "runner.fixture_artifact_not_ready",
                "The deterministic generated Artifact was not ready.",
                {"artifacts": [self._artifact_summary(item) for item in artifacts]},
            )
        terminal_payload = json_object(terminal.get("payload"), "execution.completed payload")
        output = json_object(terminal_payload.get("output"), "execution.completed payload.output")
        credential_evidence = json_object(output.get("credentialEvidence"), "credential evidence")
        if credential_evidence != {
            "credentialPayloadKeys": ["acceptanceToken"],
            "credentialVerified": True,
        }:
            raise AcceptanceError(
                "runner.fixture_credential_evidence_invalid",
                "The fixture did not return the expected key-only Credential evidence.",
                {"credentialEvidence": credential_evidence},
            )
        worker_id, generation = self._event_worker_identity(terminal)
        self.state.first_worker_id = worker_id
        self.state.first_generation = generation
        return {
            "turnId": turn.get("id"),
            "executionId": terminal.get("executionId"),
            "workerId": worker_id,
            "generation": generation,
            "eventTypes": event_types,
            "sequenceRange": self._sequence_range(events),
            "artifact": self._artifact_summary(artifact),
            "credentialEvidence": credential_evidence,
        }

    def _terminal_large_log(self) -> Mapping[str, Any]:
        turn = self._create_turn("[terminal-large]")
        execution_terminal, events = self._wait_for_turn_terminal(
            str(turn["id"]), "execution.completed"
        )
        expected_segments = terminal_large_expected_segments()
        lifecycle = [
            (event, terminal)
            for event in events
            if (terminal := self._event_terminal_data(event)) is not None
        ]
        started = [entry for entry in lifecycle if entry[1].get("eventType") == "terminal.started"]
        completed = [entry for entry in lifecycle if entry[1].get("eventType") == "terminal.exited"]
        if len(started) != 1 or len(completed) != 1:
            raise AcceptanceError(
                "runner.terminal_lifecycle_invalid",
                "The large Terminal Turn did not persist exactly one start and one exit event.",
                {
                    "started": len(started),
                    "completed": len(completed),
                    "eventTypes": [event.get("eventType") for event in events],
                },
            )
        lifecycle_types = sorted(str(terminal.get("eventType")) for _, terminal in lifecycle)
        expected_lifecycle_types = sorted(
            ["terminal.started", "terminal.exited"]
            + ["terminal.output.reference"] * len(expected_segments)
        )
        if lifecycle_types != expected_lifecycle_types:
            raise AcceptanceError(
                "runner.terminal_lifecycle_invalid",
                "The large Terminal lifecycle contained a missing or extra state.",
                {"actual": lifecycle_types, "expected": expected_lifecycle_types},
            )
        if (
            started[0][0].get("eventType") != "item.started"
            or completed[0][0].get("eventType") != "item.completed"
        ):
            raise AcceptanceError(
                "runner.terminal_lifecycle_projection_invalid",
                "The large Terminal start or exit used the wrong canonical item event.",
                {
                    "startedEventType": started[0][0].get("eventType"),
                    "completedEventType": completed[0][0].get("eventType"),
                },
            )
        terminal_id = started[0][1].get("terminalId")
        if not isinstance(terminal_id, str) or not terminal_id:
            raise AcceptanceError(
                "runner.terminal_id_missing",
                "The large Terminal start event omitted terminalId.",
            )
        if any(terminal.get("terminalId") != terminal_id for _, terminal in lifecycle):
            raise AcceptanceError(
                "runner.terminal_lifecycle_split",
                "The large Terminal lifecycle was split across multiple terminalId values.",
                {"terminalIds": sorted({str(item.get("terminalId")) for _, item in lifecycle})},
            )

        preview_events: list[dict[str, Any]] = []
        for event in events:
            if event.get("eventType") != "content.delta":
                continue
            payload = event.get("payload")
            if not isinstance(payload, dict) or payload.get("streamKind") != "command_output":
                continue
            if payload.get("terminalId") != terminal_id:
                raise AcceptanceError(
                    "runner.terminal_preview_split",
                    "Command output preview used a different terminalId.",
                    {"terminalId": terminal_id, "event": self._event_summary(event)},
                )
            preview_events.append(event)

        preview = bytearray()
        for event in preview_events:
            payload = json_object(event.get("payload"), "terminal preview payload")
            delta = payload.get("delta")
            byte_offset = payload.get("byteOffset")
            byte_length = payload.get("byteLength")
            if (
                payload.get("encoding") != "utf-8"
                or not isinstance(delta, str)
                or not isinstance(byte_offset, int)
                or not isinstance(byte_length, int)
            ):
                raise AcceptanceError(
                    "runner.terminal_preview_invalid",
                    "The large Terminal preview did not use canonical UTF-8 byte metadata.",
                    {"event": self._event_summary(event)},
                )
            encoded = delta.encode("utf-8")
            if byte_offset != len(preview) or byte_length != len(encoded):
                raise AcceptanceError(
                    "runner.terminal_preview_noncontiguous",
                    "The large Terminal preview byte range was not contiguous.",
                    {
                        "expectedOffset": len(preview),
                        "byteOffset": byte_offset,
                        "byteLength": byte_length,
                        "actualLength": len(encoded),
                    },
                )
            preview.extend(encoded)
        expected_preview = terminal_large_bytes(0, TERMINAL_LOG_PREVIEW_BYTES)
        if bytes(preview) != expected_preview or not preview_events:
            raise AcceptanceError(
                "runner.terminal_preview_mismatch",
                "The large Terminal preview was not the exact deterministic first 32 KiB.",
                {
                    "previewBytes": len(preview),
                    "previewEventCount": len(preview_events),
                    "previewSha256": hashlib.sha256(preview).hexdigest(),
                },
            )
        last_preview_payload = json_object(
            preview_events[-1].get("payload"), "terminal final preview payload"
        )
        if last_preview_payload.get("truncated") is not True:
            raise AcceptanceError(
                "runner.terminal_preview_not_truncated",
                "The bounded Terminal preview did not record truncation.",
            )

        references = sorted(
            (
                terminal
                for _, terminal in lifecycle
                if terminal.get("eventType") == "terminal.output.reference"
            ),
            key=lambda item: int(item.get("segmentIndex") or 0),
        )
        if len(references) != len(expected_segments):
            raise AcceptanceError(
                "runner.terminal_reference_count_mismatch",
                "The large Terminal did not produce exactly three Artifact references.",
                {"references": references, "expectedSegmentCount": len(expected_segments)},
            )

        reference_artifact_ids: list[str] = []
        for reference, expected in zip(references, expected_segments, strict=True):
            actual = {
                key: reference.get(key)
                for key in ("offset", "length", "segmentIndex", "encoding")
            }
            expected_reference = {
                key: expected[key] for key in ("offset", "length", "segmentIndex", "encoding")
            }
            artifact_id = reference.get("artifactId")
            if actual != expected_reference or not isinstance(artifact_id, str) or not artifact_id:
                raise AcceptanceError(
                    "runner.terminal_reference_invalid",
                    "A large Terminal Artifact reference had the wrong byte range or Artifact ID.",
                    {"actual": actual, "expected": expected_reference},
                )
            reference_artifact_ids.append(artifact_id)
        if len(set(reference_artifact_ids)) != len(reference_artifact_ids):
            raise AcceptanceError(
                "runner.terminal_reference_duplicate",
                "The large Terminal reused an Artifact ID across segments.",
                {"artifactIds": reference_artifact_ids},
            )

        completion = completed[0][1]
        expected_completion = {
            "totalBytes": TERMINAL_LARGE_TOTAL_BYTES,
            "previewBytes": TERMINAL_LOG_PREVIEW_BYTES,
            "segmentCount": len(expected_segments),
            "truncated": True,
            "exitCode": 0,
        }
        actual_completion = {key: completion.get(key) for key in expected_completion}
        if actual_completion != expected_completion:
            raise AcceptanceError(
                "runner.terminal_completion_mismatch",
                "The large Terminal completion totals did not match the persisted stream.",
                {"actual": actual_completion, "expected": expected_completion},
            )

        artifact_ready_ids: list[str] = []
        for event in events:
            if event.get("eventType") != "artifact.ready":
                continue
            payload = event.get("payload")
            if (
                isinstance(payload, dict)
                and payload.get("kind") == "terminal_log"
                and isinstance(payload.get("artifactId"), str)
            ):
                artifact_ready_ids.append(str(payload["artifactId"]))
        if (
            len(artifact_ready_ids) != len(expected_segments)
            or set(artifact_ready_ids) != set(reference_artifact_ids)
        ):
            raise AcceptanceError(
                "runner.terminal_artifact_events_mismatch",
                "The large Terminal references did not have matching artifact.ready events.",
                {
                    "referenceArtifactIds": reference_artifact_ids,
                    "readyArtifactIds": artifact_ready_ids,
                },
            )

        terminal_related_events = [
            event
            for event in events
            if self._event_terminal_data(event) is not None
            or event in preview_events
            or event.get("eventType") == "artifact.ready"
        ]
        leaked_events = [
            self._event_summary(event)
            for event in terminal_related_events
            if contains_runtime_physical_path(event.get("payload"))
        ]
        if leaked_events:
            raise AcceptanceError(
                "runner.terminal_runtime_path_leaked",
                "A Terminal Event exposed a Runtime Output physical path.",
                {"events": leaked_events},
            )

        artifacts = json_items(
            self.api.request("GET", f"/v1/sessions/{self._required('session_id')}/artifacts"),
            "artifacts",
        )
        artifacts_by_id = {str(item.get("id")): item for item in artifacts}
        segment_evidence: list[dict[str, Any]] = []
        execution_id = execution_terminal.get("executionId")
        for artifact_id, expected in zip(reference_artifact_ids, expected_segments, strict=True):
            artifact = artifacts_by_id.get(artifact_id)
            if artifact is None:
                raise AcceptanceError(
                    "runner.terminal_artifact_missing",
                    "A referenced Terminal log Artifact was absent from the Session Artifact list.",
                    {"artifactId": artifact_id},
                )
            expected_artifact = {
                "kind": "terminal_log",
                "status": "ready",
                "originalName": f"terminal-log-{expected['segmentIndex'] + 1:06d}.log",
                "contentType": "text/plain; charset=utf-8",
                "sizeBytes": expected["length"],
                "sha256": expected["sha256"],
                "executionId": execution_id,
            }
            actual_artifact = {key: artifact.get(key) for key in expected_artifact}
            if actual_artifact != expected_artifact:
                raise AcceptanceError(
                    "runner.terminal_artifact_mismatch",
                    "A Terminal log Artifact did not match the deterministic segment.",
                    {
                        "artifactId": artifact_id,
                        "actual": actual_artifact,
                        "expected": expected_artifact,
                    },
                )
            segment_evidence.append(
                {
                    "artifact": self._artifact_summary(artifact),
                    "offset": expected["offset"],
                    "length": expected["length"],
                    "segmentIndex": expected["segmentIndex"],
                }
            )

        return {
            "turnId": turn.get("id"),
            "executionId": execution_id,
            "terminalId": terminal_id,
            "sequenceRange": self._sequence_range(events),
            "preview": {
                "bytes": len(preview),
                "eventCount": len(preview_events),
                "sha256": hashlib.sha256(preview).hexdigest(),
                "truncated": True,
            },
            "completion": actual_completion,
            "segments": segment_evidence,
            "runtimePhysicalPathLeak": False,
        }

    def _approval_resolution(self) -> Mapping[str, Any]:
        pending = self.state.pending_approval
        if pending is None:
            turn = self._create_turn("[approval]")
            interaction = self._wait_for_interaction(str(turn["id"]), "approval")
        else:
            turn = json_object(pending.get("turn"), "pending approval turn")
            interaction = json_object(pending.get("interaction"), "pending approval interaction")
            self.state.pending_approval = None
        execution_id = interaction.get("executionId")
        request_id = interaction.get("requestId")
        if not isinstance(execution_id, str) or not execution_id or not isinstance(request_id, str) or not request_id:
            raise AcceptanceError(
                "runner.approval_interaction_invalid",
                "The Approval interaction omitted its Execution or Request ID.",
                {"turnId": turn.get("id"), "interactionId": interaction.get("id")},
            )
        resolved = json_object(
            self.api.request(
                "POST",
                f"/v1/executions/{execution_id}/approvals/{urllib.parse.quote(request_id, safe='')}/resolve",
                {"decision": "accept"},
            ),
            "approval resolution",
        )
        terminal, events = self._wait_for_turn_terminal(str(turn["id"]), "execution.completed")
        if not any(event.get("eventType") == "request.resolved" for event in events):
            raise AcceptanceError(
                "runner.approval_resolution_event_missing",
                "Approval completed without a request.resolved event.",
                {"eventTypes": [event.get("eventType") for event in events]},
            )
        target_evidence = self.driver.observe_terminal_execution(
            self._required("target_id"),
            execution_id,
        )
        return {
            "turnId": turn.get("id"),
            "executionId": terminal.get("executionId"),
            "requestId": request_id,
            "interactionId": interaction.get("id"),
            "resolutionStatus": resolved.get("status"),
            "deliveryStatus": resolved.get("deliveryStatus"),
            "sequenceRange": self._sequence_range(events),
            "targetTerminal": dict(target_evidence) if target_evidence else None,
        }

    def _recover_pending_approval_runtime(self) -> Mapping[str, Any]:
        recover = getattr(self.driver, "recover_pending_interaction", None)
        if not callable(recover):
            raise AcceptanceError(
                "runner.pending_interaction_recovery_unsupported",
                "The TargetDriver cannot recover a pending interaction runtime.",
                {"target": self.driver.name},
            )
        return self._recover_pending_approval_with(recover)

    def _recover_pending_approval_with(
        self,
        recover: Callable[[str, str], Mapping[str, Any]],
    ) -> Mapping[str, Any]:
        pending = self.state.pending_approval
        if pending is None:
            raise AcceptanceError(
                "runner.pending_approval_missing",
                "The pending Approval barrier was unavailable for runtime recovery.",
            )
        turn = json_object(pending.get("turn"), "pending approval turn")
        interaction = json_object(pending.get("interaction"), "pending approval interaction")
        turn_id = turn.get("id")
        previous_interaction_id = interaction.get("id")
        execution_id = interaction.get("executionId")
        request_id = interaction.get("requestId")
        if (
            not isinstance(turn_id, str)
            or not turn_id
            or not isinstance(previous_interaction_id, str)
            or not previous_interaction_id
            or not isinstance(execution_id, str)
            or not execution_id
            or not isinstance(request_id, str)
            or not request_id
        ):
            raise AcceptanceError(
                "runner.pending_approval_invalid",
                "The pending Approval barrier omitted its Turn, Interaction, Execution, or Request identity.",
                {"turn": turn, "interaction": interaction},
            )
        before_sequence = max(
            self.state.last_sequence,
            max((int(event.get("sequence") or 0) for event in self._all_events()), default=0),
        )
        target_evidence = recover(self._required("target_id"), execution_id)
        recovery_event = self._wait_for_execution_event(
            execution_id,
            "execution.recovering",
            after_sequence=before_sequence,
        )
        replacement = self._wait_for_replacement_interaction(turn_id, "approval", previous_interaction_id)
        replacement_execution_id = replacement.get("executionId")
        replacement_request_id = replacement.get("requestId")
        if (
            not isinstance(replacement_execution_id, str)
            or not replacement_execution_id
            or not isinstance(replacement_request_id, str)
            or not replacement_request_id
        ):
            raise AcceptanceError(
                "runner.recovered_interaction_invalid",
                "The recovered Approval omitted its Execution or Request identity.",
                {"interaction": replacement},
            )
        if replacement_request_id == request_id:
            raise AcceptanceError(
                "runner.pending_interaction_request_not_replaced",
                "Pending Approval recovery reused the obsolete Generation's Request identity.",
                {
                    "staleInteractionId": previous_interaction_id,
                    "staleRequestId": request_id,
                    "replacementInteractionId": replacement.get("id"),
                    "replacementRequestId": replacement_request_id,
                },
            )
        if replacement_execution_id != execution_id:
            raise AcceptanceError(
                "runner.pending_interaction_execution_changed",
                "Pending Approval recovery created a different Execution instead of advancing its Generation.",
                {
                    "staleExecutionId": execution_id,
                    "replacementExecutionId": replacement_execution_id,
                },
            )
        target_runtime = self.driver.observe_execution(self._required("target_id"), replacement_execution_id)
        deleted_uid = target_evidence.get("deletedPodUid") if isinstance(target_evidence, Mapping) else None
        replacement_uid = target_runtime.get("podUid") if isinstance(target_runtime, Mapping) else None
        if isinstance(deleted_uid, str) and isinstance(replacement_uid, str) and deleted_uid == replacement_uid:
            raise AcceptanceError(
                "runner.pending_interaction_recovery_not_replaced",
                "Pending Approval recovery reused the deleted execution-pinned runtime identity.",
                {"deletedPodUid": deleted_uid, "replacementPodUid": replacement_uid},
            )
        self.state.pending_approval = {"turn": turn, "interaction": replacement}
        return {
            "turnId": turn_id,
            "staleInteractionId": previous_interaction_id,
            "staleRequestId": request_id,
            "staleExecutionId": execution_id,
            "recoveryEvent": self._event_summary(recovery_event),
            "replacementInteractionId": replacement.get("id"),
            "replacementRequestId": replacement_request_id,
            "replacementExecutionId": replacement_execution_id,
            "targetRecovery": dict(target_evidence),
            "targetRuntime": dict(target_runtime),
        }

    def _user_input_resolution(self) -> Mapping[str, Any]:
        turn = self._create_turn("[user-input]")
        interaction = self._wait_for_interaction(str(turn["id"]), "user-input")
        execution_id = str(interaction["executionId"])
        request_id = str(interaction["requestId"])
        resolved = json_object(
            self.api.request(
                "POST",
                f"/v1/executions/{execution_id}/user-input/{urllib.parse.quote(request_id, safe='')}/resolve",
                {"answers": {"fixture-choice": "Continue"}},
            ),
            "user-input resolution",
        )
        terminal, events = self._wait_for_turn_terminal(str(turn["id"]), "execution.completed")
        if not any(event.get("eventType") == "user-input.resolved" for event in events):
            raise AcceptanceError(
                "runner.user_input_resolution_event_missing",
                "User input completed without a user-input.resolved event.",
                {"eventTypes": [event.get("eventType") for event in events]},
            )
        return {
            "turnId": turn.get("id"),
            "executionId": terminal.get("executionId"),
            "requestId": request_id,
            "interactionId": interaction.get("id"),
            "resolutionStatus": resolved.get("status"),
            "deliveryStatus": resolved.get("deliveryStatus"),
            "sequenceRange": self._sequence_range(events),
        }

    def _provider_error(self) -> Mapping[str, Any]:
        turn = self._create_turn("[provider-error]")
        terminal, events = self._wait_for_turn_terminal(str(turn["id"]), "execution.failed")
        payload = json_object(terminal.get("payload"), "execution.failed payload")
        if payload.get("failureCode") != "provider_rate_limited":
            raise AcceptanceError(
                "runner.provider_error_code_mismatch",
                "The deterministic Provider failure did not preserve its stable error code.",
                {"failureCode": payload.get("failureCode")},
            )
        return {
            "turnId": turn.get("id"),
            "executionId": terminal.get("executionId"),
            "failureCode": payload.get("failureCode"),
            "sequenceRange": self._sequence_range(events),
        }

    def _baseline_smoke(self) -> Mapping[str, Any]:
        turn = self._create_turn("[text] [usage]")
        terminal, events = self._wait_for_turn_terminal(str(turn["id"]), "execution.completed")
        return {
            "turnId": turn.get("id"),
            "executionId": terminal.get("executionId"),
            "sequenceRange": self._sequence_range(events),
        }

    def _execute_failure_case(self, failure_case: str) -> Mapping[str, Any]:
        provider_failures = {
            "provider-malformed": ("protocol_violation", "[provider-malformed]"),
            "provider-oversized": ("protocol_violation", "[provider-oversized]"),
            "provider-crash": ("provider_unavailable", "[provider-crash]"),
        }
        if failure_case in provider_failures:
            expected_code, directive = provider_failures[failure_case]
            return self._provider_host_failure(failure_case, directive, expected_code)
        if failure_case == "kubernetes-image-canary":
            return self._kubernetes_image_canary()
        return self._pending_approval_failure(failure_case)

    def _provider_host_failure(
        self,
        failure_case: str,
        directive: str,
        expected_code: str,
    ) -> Mapping[str, Any]:
        turn = self._create_turn(directive)
        terminal, events = self._wait_for_turn_terminal(str(turn["id"]), "execution.failed")
        payload = json_object(terminal.get("payload"), "execution.failed payload")
        if payload.get("failureCode") != expected_code:
            raise AcceptanceError(
                "runner.provider_fault_code_mismatch",
                "The Provider Host fault did not preserve the expected stable failure code.",
                {
                    "failureCase": failure_case,
                    "expectedFailureCode": expected_code,
                    "actualFailureCode": payload.get("failureCode"),
                },
            )
        recovery_turn = self._create_turn("[text]")
        recovery_terminal, recovery_events = self._wait_for_turn_terminal(
            str(recovery_turn["id"]),
            "execution.completed",
        )
        return {
            "failureCase": failure_case,
            "faultTurnId": turn.get("id"),
            "faultExecutionId": terminal.get("executionId"),
            "failureCode": payload.get("failureCode"),
            "faultSequenceRange": self._sequence_range(events),
            "recoveryTurnId": recovery_turn.get("id"),
            "recoveryExecutionId": recovery_terminal.get("executionId"),
            "recoverySequenceRange": self._sequence_range(recovery_events),
            "hostRecoveredForNextTurn": True,
        }

    def _pending_approval_failure(self, failure_case: str) -> Mapping[str, Any]:
        inject = getattr(self.driver, "inject_failure", None)
        if not callable(inject):
            raise AcceptanceUnsupported(
                "runner.failure_case_unsupported",
                f"The {self.driver.name} Target does not implement failure injection {failure_case}.",
                {"target": self.driver.name, "failureCase": failure_case},
            )
        preflight = getattr(self.driver, "validate_failure", None)
        if callable(preflight):
            preflight(failure_case)
        barrier = self._begin_approval_readiness_barrier()
        recovery = self._recover_pending_approval_with(
            lambda target_id, execution_id: inject(failure_case, target_id, execution_id)
        )
        resolution = self._approval_resolution()
        return {
            "failureCase": failure_case,
            "barrier": barrier,
            "recovery": recovery,
            "resolution": resolution,
            "generationFenced": True,
            "terminalCount": 1,
        }

    def _kubernetes_image_canary(self) -> Mapping[str, Any]:
        provision = getattr(self.driver, "provision_canary_target", None)
        if not callable(provision):
            raise AcceptanceUnsupported(
                "runner.kubernetes_canary_unsupported",
                "The selected TargetDriver cannot provision a Kubernetes image canary.",
                {"target": self.driver.name},
            )
        target = json_object(
            provision(
                self._required("tenant_id"),
                self._required("organization_id"),
                self.options.provider,
            ),
            "Kubernetes canary target",
        )
        target_id = target.get("id")
        if (
            not isinstance(target_id, str)
            or not target_id
            or target.get("kind") != "kubernetes"
            or target.get("organizationId") != self._required("organization_id")
        ):
            raise AcceptanceError(
                "runner.kubernetes_canary_target_invalid",
                "The canary Target did not retain its Kubernetes kind and Organization scope.",
                {"target": self._target_summary(target)},
            )
        session = json_object(
            self.api.request(
                "POST",
                f"/v1/projects/{self._required('project_id')}/sessions",
                {
                    "title": "Stage 3 Kubernetes Worker Image Canary",
                    "visibility": "project",
                    "provider": self.options.provider,
                    "model": "stage3-acceptance-fixture",
                    "providerCredentialId": self._required("credential_id"),
                    "executionTargetId": target_id,
                },
                expected=(201,),
            ),
            "Kubernetes canary session",
        )
        canary_session_id = session.get("id")
        if not isinstance(canary_session_id, str) or not canary_session_id:
            raise AcceptanceError(
                "runner.kubernetes_canary_session_id_missing",
                "The Kubernetes canary Session did not return an ID.",
            )

        original_session_id = self.state.session_id
        original_target_id = self.state.target_id
        original_last_sequence = self.state.last_sequence
        original_pending = self.state.pending_approval
        canary_evidence: dict[str, Any]
        self.state.session_id = canary_session_id
        self.state.target_id = target_id
        self.state.last_sequence = int(session.get("lastEventSequence") or 0)
        self.state.pending_approval = None
        try:
            barrier = self._begin_approval_readiness_barrier()
            discovered = self._wait_compatible_manifest(target_id)
            interaction = json_object(
                json_object(self.state.pending_approval, "canary pending approval").get("interaction"),
                "canary pending approval interaction",
            )
            execution_id = interaction.get("executionId")
            if not isinstance(execution_id, str) or not execution_id:
                raise AcceptanceError(
                    "runner.kubernetes_canary_execution_id_missing",
                    "The canary Approval barrier omitted its Execution ID.",
                )
            runtime = self.driver.observe_execution(target_id, execution_id)
            resolution = self._approval_resolution()
            canary_evidence = {
                "target": self._target_summary(target),
                "driverEvidence": target.get("driverEvidence"),
                "sessionId": canary_session_id,
                "barrier": barrier,
                "manifestId": discovered["manifest"].get("manifestId"),
                "workerBuild": discovered["manifest"].get("workerBuild"),
                "runtime": dict(runtime),
                "resolution": resolution,
            }
        finally:
            self.state.session_id = original_session_id
            self.state.target_id = original_target_id
            self.state.last_sequence = original_last_sequence
            self.state.pending_approval = original_pending

        rollback_turn = self._create_turn("[text] [usage]")
        rollback_terminal, rollback_events = self._wait_for_turn_terminal(
            str(rollback_turn["id"]),
            "execution.completed",
        )
        return {
            **canary_evidence,
            "baselineAfterCanary": {
                "targetId": original_target_id,
                "turnId": rollback_turn.get("id"),
                "executionId": rollback_terminal.get("executionId"),
                "sequenceRange": self._sequence_range(rollback_events),
            },
            "releaseBoundary": (
                "deterministic isolated Target canary only; no product release promotion or immutable digest rollback API"
            ),
        }

    def _replace_worker(self) -> Mapping[str, Any]:
        target_id = self._required("target_id")
        evidence = self.driver.replace_worker(
            self._required("tenant_id"),
            target_id,
            self.options.provider,
        )
        replacement_worker_id = evidence.get("replacementWorkerId")
        if not isinstance(replacement_worker_id, str) or not replacement_worker_id:
            raise AcceptanceError(
                "runner.replacement_worker_id_missing",
                "Managed replacement evidence omitted the replacement Worker ID.",
            )
        discovered = self._wait_compatible_manifest(target_id)
        self.state.worker_replaced = True
        self.state.replacement_worker_id = replacement_worker_id
        return {
            **dict(evidence),
            "postReplacementManifestId": discovered["manifest"].get("manifestId"),
            "workerStatusCounts": discovered["manifest"].get("workerStatusCounts"),
        }

    def _post_replacement_workspace_turn(self) -> Mapping[str, Any]:
        turn = self._create_turn("[workspace-verify]")
        terminal, events = self._wait_for_turn_terminal(str(turn["id"]), "execution.completed")
        worker_id, generation = self._event_worker_identity(terminal)
        replacement_worker_id = self._required("replacement_worker_id")
        if worker_id != replacement_worker_id:
            raise AcceptanceError(
                "runner.post_replacement_worker_mismatch",
                "The post-replacement Turn was not fenced to the replacement Worker slot.",
                {"expectedWorkerId": replacement_worker_id, "actualWorkerId": worker_id},
            )
        terminal_payload = json_object(terminal.get("payload"), "post-replacement execution.completed payload")
        output = json_object(terminal_payload.get("output"), "post-replacement execution.completed payload.output")
        workspace_evidence = json_object(output.get("workspaceEvidence"), "post-replacement workspace evidence")
        expected_evidence = {
            "artifactRelativePath": FIXTURE_ARTIFACT_RELATIVE_PATH,
            "artifactContentVerified": True,
        }
        if workspace_evidence != expected_evidence:
            raise AcceptanceError(
                "runner.post_replacement_workspace_evidence_invalid",
                "The replacement Worker did not verify the artifact content written before replacement.",
                {"expected": expected_evidence, "actual": workspace_evidence},
            )
        return {
            "turnId": turn.get("id"),
            "executionId": terminal.get("executionId"),
            "workerId": worker_id,
            "generation": generation,
            "workspaceEvidence": workspace_evidence,
            "sequenceRange": self._sequence_range(events),
            "semantics": self.driver.replacement_workspace_semantics,
        }

    def _restart_control_plane(self) -> Mapping[str, Any]:
        events = self._all_events()
        if not events:
            raise AcceptanceError("runner.restart_without_events", "No Session events existed before restart.")
        self.state.pre_restart_sequence = int(events[-1]["sequence"])
        restarted = self.driver.restart()
        self.state.restarted = True
        target_id = self._required("target_id")
        result: dict[str, Any] = {
            **restarted,
            "preRestartSequence": self.state.pre_restart_sequence,
        }
        if self.driver.lifecycle.execution_pinned:
            result["workerAllocation"] = self.driver.lifecycle.worker_allocation
            result["postRestartWorkerExpectation"] = "deferred-until-next-execution"
            return result
        manifest = self._wait_post_restart_online_worker(target_id)
        result.update(
            {
                "postRestartManifestId": manifest.get("manifestId"),
                "workerStatusCounts": manifest.get("workerStatusCounts"),
            }
        )
        return result

    def _wait_post_restart_online_worker(self, target_id: str) -> dict[str, Any]:
        tenant_id = self._required("tenant_id")

        def worker_probe() -> dict[str, Any] | None:
            manifests = json_items(
                self.api.request("GET", f"/v1/tenants/{tenant_id}/worker-manifests"),
                "worker-manifests",
            )
            for manifest in manifests:
                counts = manifest.get("workerStatusCounts")
                if (
                    manifest.get("executionTargetId") == target_id
                    and isinstance(counts, dict)
                    and isinstance(counts.get("online"), int)
                    and counts["online"] >= 1
                ):
                    return manifest
            return None

        return self.api.wait_until("a post-restart online Worker", worker_probe)

    def _second_turn_continuity(self) -> Mapping[str, Any]:
        before = self.state.pre_restart_sequence
        if before is None:
            events = self._all_events()
            before = int(events[-1]["sequence"]) if events else 0
        turn = self._create_turn("[text] [usage]")
        terminal, turn_events = self._wait_for_turn_terminal(str(turn["id"]), "execution.completed")
        worker_id, generation = self._event_worker_identity(terminal)
        all_events = self._all_events()
        sequences = [int(event["sequence"]) for event in all_events]
        expected = list(range(1, sequences[-1] + 1)) if sequences else []
        if sequences != expected:
            raise AcceptanceError(
                "runner.session_sequence_discontinuous",
                "Session Event Sequence was not contiguous across Control Plane restart.",
                {"sequences": sequences},
            )
        if int(terminal["sequence"]) <= before:
            raise AcceptanceError(
                "runner.session_sequence_not_advanced",
                "The second Turn did not advance Session Event Sequence.",
                {"before": before, "after": terminal.get("sequence")},
            )
        return {
            "turnId": turn.get("id"),
            "executionId": terminal.get("executionId"),
            "workerId": worker_id,
            "preRestartWorkerId": self.state.first_worker_id,
            "workerIdChangedAfterRestart": worker_id != self.state.first_worker_id,
            "workerIdSemantics": (
                "execution-pinned registration; each Execution may use a different Worker ID"
                if self.driver.lifecycle.execution_pinned
                else "stable registration slot; a restarted agentd registration may reuse the Worker ID"
            ),
            "generation": generation,
            "firstGeneration": self.state.first_generation,
            "generationScope": "per-execution",
            "targetWorkerReplaced": self.state.worker_replaced,
            "preRestartSequence": before,
            "terminalSequence": terminal.get("sequence"),
            "sessionSequenceRange": self._sequence_range(all_events),
            "turnSequenceRange": self._sequence_range(turn_events),
        }

    def _create_turn(self, input_text: str) -> dict[str, Any]:
        session_id = self._required("session_id")
        return json_object(
            self.api.request(
                "POST",
                f"/v1/sessions/{session_id}/turns",
                {"inputText": input_text, "runtimeMode": "full-access", "interactionMode": "default"},
                expected=(201,),
            ),
            "turn",
        )

    def _wait_for_turn_created(self, turn_id: str) -> dict[str, Any]:
        def created_probe() -> dict[str, Any] | None:
            for event in self._all_events():
                if event.get("eventType") != "turn.created" or self._event_turn_id(event) != turn_id:
                    continue
                self._event_execution_id(event)
                sequence = event.get("sequence")
                if isinstance(sequence, int):
                    self.state.last_sequence = max(self.state.last_sequence, sequence)
                return event
            return None

        return self.api.wait_until(f"turn.created for Turn {turn_id}", created_probe)

    def _wait_for_turn_terminal(
        self,
        turn_id: str,
        expected_event_type: str,
    ) -> tuple[dict[str, Any], list[dict[str, Any]]]:
        def terminal_probe() -> tuple[dict[str, Any], list[dict[str, Any]]] | None:
            events = self._all_events()
            created = next(
                (
                    event
                    for event in events
                    if event.get("eventType") == "turn.created" and self._event_turn_id(event) == turn_id
                ),
                None,
            )
            if created is None:
                return None
            execution_id = self._event_execution_id(created)
            matching = [event for event in events if event.get("executionId") == execution_id]
            terminals = [event for event in matching if event.get("eventType") in TERMINAL_EVENT_TYPES]
            if not terminals:
                return None
            if len(terminals) != 1:
                raise AcceptanceError(
                    "runner.turn_terminal_duplicate",
                    "Turn emitted more than one terminal event.",
                    {
                        "turnId": turn_id,
                        "executionId": execution_id,
                        "terminals": [self._event_summary(event) for event in terminals],
                    },
                )
            terminal = terminals[0]
            if terminal.get("eventType") != expected_event_type:
                raise AcceptanceError(
                    "runner.turn_terminal_mismatch",
                    f"Turn terminated as {terminal.get('eventType')} instead of {expected_event_type}.",
                    {
                        "turnId": turn_id,
                        "terminal": self._event_summary(terminal),
                        "eventTypes": [event.get("eventType") for event in matching],
                    },
                )
            self.state.last_sequence = max(self.state.last_sequence, int(terminal["sequence"]))
            return terminal, matching

        return self.api.wait_until(f"Turn {turn_id} terminal event", terminal_probe)

    def _wait_for_interaction(self, turn_id: str, kind: str) -> dict[str, Any]:
        session_id = self._required("session_id")

        def interaction_probe() -> dict[str, Any] | None:
            snapshot = json_object(
                self.api.request("GET", f"/v1/sessions/{session_id}/interactions"),
                "pending interactions",
            )
            items = snapshot.get("items")
            if not isinstance(items, list):
                raise AcceptanceError(
                    "runner.response_shape_invalid",
                    "pending interactions.items was not an array.",
                )
            for item in items:
                if isinstance(item, dict) and item.get("turnId") == turn_id and item.get("kind") == kind:
                    return item
            return None

        return self.api.wait_until(f"{kind} interaction for Turn {turn_id}", interaction_probe)

    def _wait_for_replacement_interaction(
        self,
        turn_id: str,
        kind: str,
        previous_interaction_id: str,
    ) -> dict[str, Any]:
        session_id = self._required("session_id")

        def interaction_probe() -> dict[str, Any] | None:
            snapshot = json_object(
                self.api.request("GET", f"/v1/sessions/{session_id}/interactions"),
                "pending interactions",
            )
            items = snapshot.get("items")
            if not isinstance(items, list):
                raise AcceptanceError(
                    "runner.response_shape_invalid",
                    "pending interactions.items was not an array.",
                )
            if any(isinstance(item, dict) and item.get("id") == previous_interaction_id for item in items):
                return None
            for item in items:
                if (
                    isinstance(item, dict)
                    and item.get("turnId") == turn_id
                    and item.get("kind") == kind
                    and item.get("id") != previous_interaction_id
                ):
                    return item
            return None

        return self.api.wait_until(f"replacement {kind} interaction for Turn {turn_id}", interaction_probe)

    def _wait_for_execution_event(
        self,
        execution_id: str,
        event_type: str,
        *,
        after_sequence: int,
    ) -> dict[str, Any]:
        def event_probe() -> dict[str, Any] | None:
            for event in self._all_events():
                sequence = int(event.get("sequence") or 0)
                if sequence <= after_sequence:
                    continue
                if event.get("executionId") == execution_id and event.get("eventType") == event_type:
                    self.state.last_sequence = max(self.state.last_sequence, sequence)
                    return event
            return None

        return self.api.wait_until(f"{event_type} for Execution {execution_id}", event_probe)

    def _all_events(self) -> list[dict[str, Any]]:
        session_id = self._required("session_id")
        events: list[dict[str, Any]] = []
        after = 0
        while True:
            page = json_object(
                self.api.request("GET", f"/v1/sessions/{session_id}/events?afterSequence={after}&limit=500"),
                "session events",
            )
            items = page.get("items")
            if not isinstance(items, list) or not all(isinstance(item, dict) for item in items):
                raise AcceptanceError("runner.response_shape_invalid", "session events.items was not an object array.")
            if not items:
                break
            events.extend(items)
            next_after = int(items[-1].get("sequence") or 0)
            if next_after <= after:
                raise AcceptanceError(
                    "runner.session_event_pagination_stalled",
                    "Session Event pagination did not advance.",
                    {"afterSequence": after, "nextAfterSequence": next_after},
                )
            after = next_after
            last_sequence = int(page.get("lastSequence") or after)
            if after >= last_sequence or len(items) < 500:
                break
        for index, event in enumerate(events, start=1):
            if int(event.get("sequence") or 0) != index:
                raise AcceptanceError(
                    "runner.session_sequence_discontinuous",
                    "Session Event Sequence contained a gap or duplicate.",
                    {"index": index, "sequence": event.get("sequence")},
                )
        return events

    @staticmethod
    def _event_terminal_data(event: Mapping[str, Any]) -> dict[str, Any] | None:
        payload = event.get("payload")
        if not isinstance(payload, dict):
            return None
        data = payload.get("data")
        if not isinstance(data, dict):
            return None
        terminal = data.get("terminal")
        return terminal if isinstance(terminal, dict) else None

    @staticmethod
    def _event_turn_id(event: Mapping[str, Any]) -> str | None:
        payload = event.get("payload")
        if isinstance(payload, dict) and isinstance(payload.get("turnId"), str):
            return str(payload["turnId"])
        return None

    @staticmethod
    def _event_execution_id(event: Mapping[str, Any]) -> str:
        execution_id = event.get("executionId")
        if not isinstance(execution_id, str):
            payload = event.get("payload")
            if isinstance(payload, dict):
                execution_id = payload.get("executionId")
        if not isinstance(execution_id, str) or not execution_id:
            raise AcceptanceError(
                "runner.turn_execution_id_missing",
                "turn.created did not identify its Execution.",
                {"event": AcceptanceSuite._event_summary(event)},
            )
        return execution_id

    @staticmethod
    def _event_worker_identity(event: Mapping[str, Any]) -> tuple[str, int]:
        worker_id = event.get("workerId")
        generation = event.get("generation")
        if not isinstance(worker_id, str) or not isinstance(generation, int) or generation < 1:
            raise AcceptanceError(
                "runner.worker_fence_missing",
                "Terminal Worker event omitted workerId or generation.",
                {"event": AcceptanceSuite._event_summary(event)},
            )
        return worker_id, generation

    def _required(self, field: str) -> str:
        value = getattr(self.state, field)
        if not isinstance(value, str) or not value:
            raise AcceptanceError(
                "runner.scenario_state_missing",
                f"Scenario state {field} was unavailable.",
                {"field": field},
            )
        return value

    @staticmethod
    def _target_summary(target: Mapping[str, Any]) -> dict[str, Any]:
        return {
            key: target.get(key)
            for key in ("id", "tenantId", "organizationId", "kind", "name", "status")
        }

    @staticmethod
    def _artifact_summary(artifact: Mapping[str, Any]) -> dict[str, Any]:
        return {
            key: artifact.get(key)
            for key in ("id", "kind", "originalName", "contentType", "status", "sizeBytes", "sha256")
        }

    @staticmethod
    def _event_summary(event: Mapping[str, Any]) -> dict[str, Any]:
        return {
            key: event.get(key)
            for key in ("eventId", "sequence", "eventType", "executionId", "workerId", "generation")
        }

    @staticmethod
    def _sequence_range(events: Sequence[Mapping[str, Any]]) -> dict[str, Any]:
        sequences = [int(event["sequence"]) for event in events if isinstance(event.get("sequence"), int)]
        return {
            "first": min(sequences) if sequences else None,
            "last": max(sequences) if sequences else None,
            "count": len(sequences),
        }


def aggregate_status(cases: Sequence[Mapping[str, Any]]) -> str:
    statuses = [str(case.get("status")) for case in cases]
    if not statuses or "fail" in statuses:
        return "fail"
    if "skipped" in statuses:
        return "skipped"
    if all(status == "unsupported" for status in statuses):
        return "unsupported"
    return "pass"


def explicit_unsupported_case(
    case_id: str,
    started_at: str,
    started: float,
    reason_code: str,
    message: str,
    evidence: Mapping[str, Any],
) -> dict[str, Any]:
    return {
        "id": case_id,
        "name": "Explicit Unsupported",
        "status": "unsupported",
        "startedAt": started_at,
        "finishedAt": utc_now(),
        "durationMs": elapsed_ms(started),
        "reasonCode": reason_code,
        "message": message,
        "evidence": dict(evidence),
    }


def markdown_from_report(report: Mapping[str, Any]) -> str:
    real_provider_smoke = report.get("mode") == "real-provider-smoke"
    lines = [
        (
            "# Stage 3 Real Provider Smoke Acceptance"
            if real_provider_smoke
            else "# Stage 3 Provider Fixture Acceptance"
        ),
        "",
        f"- Schema: `{report['schemaVersion']}`",
        f"- Run: `{report['runId']}`",
        f"- Mode: `{report.get('mode', 'fixture')}`",
        f"- Target: `{report['target']}`",
        f"- Provider: `{report['provider']}`",
        f"- Status: **{report['status']}**",
        f"- Started: `{report['startedAt']}`",
        f"- Finished: `{report['finishedAt']}`",
        f"- Duration: `{report['durationMs']} ms`",
        "",
        "## Evidence boundary",
        "",
        (
            (
                "This report runs a real Codex App Server or Claude Agent SDK Provider through the real Control "
                "Plane, selected Target, agentd, Worker Protocol, Provider Host, Control Plane restart, and a "
                "native-cursor second Turn. It is a narrow two-Turn smoke, not the complete Local or four-Target "
                "Release Gate."
            )
            if real_provider_smoke
            else (
                "This report uses the deterministic Provider Host fixture through the real Control Plane, "
                "agentd, Worker Protocol, and selected Target lifecycle. It is not a real Codex App Server or "
                "Claude Agent SDK release gate."
            )
        ),
    ]
    configuration = report.get("configuration")
    failure_matrix = configuration.get("failureMatrix") if isinstance(configuration, dict) else None
    if isinstance(failure_matrix, dict) and failure_matrix.get("requestedCases"):
        lines.extend(
            [
                "",
                "## Requested failure/canary matrix",
                "",
                "```json",
                json.dumps(failure_matrix, indent=2, sort_keys=True, ensure_ascii=False),
                "```",
            ]
        )
    lines.extend(
        [
            "",
            "## Cases",
            "",
            "| Case | Status | Duration | Reason |",
            "| --- | --- | ---: | --- |",
        ]
    )
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
        lines.append(f"### {case.get('id', '')}")
        lines.append("")
        if case.get("message"):
            lines.append(str(case["message"]))
            lines.append("")
        if case.get("status") == "skipped":
            lines.append(f"Required inputs: `{json.dumps(case.get('requiredInputs', []), ensure_ascii=False)}`")
            lines.append("")
        evidence = case.get("evidence")
        if evidence:
            lines.extend(["```json", json.dumps(evidence, indent=2, sort_keys=True, ensure_ascii=False), "```", ""])
    return "\n".join(lines).rstrip() + "\n"


def write_reports(report: dict[str, Any], output_dir: pathlib.Path, redactor: SecretRedactor) -> tuple[pathlib.Path, pathlib.Path]:
    output_dir.mkdir(parents=True, exist_ok=True)
    json_path = output_dir / JSON_REPORT_NAME
    markdown_path = output_dir / MARKDOWN_REPORT_NAME
    sanitized = redactor.value(report)
    json_path.write_text(json.dumps(sanitized, indent=2, sort_keys=True, ensure_ascii=False) + "\n", encoding="utf-8")
    markdown_path.write_text(markdown_from_report(sanitized), encoding="utf-8")
    return json_path, markdown_path


def scan_output_secrets(output_dir: pathlib.Path, redactor: SecretRedactor) -> Mapping[str, Any]:
    allowed_suffixes = {".json", ".log", ".md", ".txt", ".yaml", ".yml"}
    known_secrets = [value.encode("utf-8") for value in redactor.secret_values() if value]
    overlap_bytes = max([512, *(len(value) for value in known_secrets)])
    findings: list[dict[str, Any]] = []
    scanned_files = 0
    scanned_bytes = 0

    for path in sorted(output_dir.rglob("*")):
        if path.is_symlink() or not path.is_file() or path.suffix.lower() not in allowed_suffixes:
            continue
        scanned_files += 1
        file_size = path.stat().st_size
        scanned_bytes += file_size
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
                for index, secret in enumerate(known_secrets):
                    if secret and secret in window:
                        kind = f"known-secret-{index + 1}"
                        if kind not in seen:
                            findings.append(
                                {
                                    "file": str(path.relative_to(output_dir)),
                                    "kind": kind,
                                    "offset": window_offset + window.find(secret),
                                }
                            )
                            seen.add(kind)
                for kind, pattern in SECRET_SCAN_PATTERNS:
                    match = pattern.search(window)
                    if match is not None and kind not in seen:
                        findings.append(
                            {
                                "file": str(path.relative_to(output_dir)),
                                "kind": kind,
                                "offset": window_offset + match.start(),
                            }
                        )
                        seen.add(kind)
                carry = window[-overlap_bytes:]
                offset += len(chunk)

    return {
        "status": "pass" if not findings else "fail",
        "scannedFiles": scanned_files,
        "scannedBytes": scanned_bytes,
        "fileTypes": sorted(allowed_suffixes),
        "knownSecretCount": len(known_secrets),
        "patternNames": [name for name, _ in SECRET_SCAN_PATTERNS],
        "findings": findings,
        "scope": "acceptance JSON, Markdown, text metadata, and redacted logs; binary SQLite/Artifacts excluded",
    }


def output_secret_scan_case(output_dir: pathlib.Path, redactor: SecretRedactor) -> dict[str, Any]:
    started_at = utc_now()
    started = time.monotonic()
    evidence = dict(scan_output_secrets(output_dir, redactor))
    passed = evidence.pop("status") == "pass"
    case: dict[str, Any] = {
        "id": "security.output-secret-scan",
        "name": "Scan acceptance reports and logs for known or high-confidence Secret patterns",
        "status": "pass" if passed else "fail",
        "startedAt": started_at,
        "finishedAt": utc_now(),
        "durationMs": elapsed_ms(started),
        "evidence": evidence,
    }
    if not passed:
        case.update(
            {
                "reasonCode": "runner.output_secret_detected",
                "message": "Acceptance output contained a known Secret or high-confidence Secret pattern.",
            }
        )
    return case


def parse_runner_command(raw: str | None, repo_root: pathlib.Path, target: str) -> tuple[str, ...]:
    if raw is None:
        if target != "local":
            return (
                "node",
                "/opt/synara/acceptance/provider-host-fixture.mjs",
                "--protocol-v2",
            )
        return (
            "bun",
            "run",
            str(repo_root / "scripts" / "stage3-provider-acceptance" / "provider-host-fixture.ts"),
            "--protocol-v2",
        )
    try:
        decoded = json.loads(raw)
    except json.JSONDecodeError as error:
        raise ValueError(f"--runner-command-json is invalid JSON: {error.msg}") from None
    if not isinstance(decoded, list) or not decoded or not all(isinstance(item, str) and item for item in decoded):
        raise ValueError("--runner-command-json must be a non-empty JSON array of non-empty strings")
    return tuple(decoded)


def parse_args(argv: Sequence[str]) -> RunnerOptions:
    repo_root = pathlib.Path(__file__).resolve().parents[2]
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--target", default="local", choices=("local", "docker", "ssh", "kubernetes"))
    parser.add_argument("--provider", default="codex", choices=PROVIDERS)
    parser.add_argument(
        "--suite",
        default="fixture",
        choices=SUITES,
        help="Acceptance suite: deterministic fixture or a real Codex/Claude two-Turn smoke",
    )
    parser.add_argument("--output-dir", type=pathlib.Path)
    parser.add_argument(
        "--timeout",
        type=float,
        help="Overall timeout in seconds (default: Local 180, SSH/Docker 900, Kubernetes 1200)",
    )
    parser.add_argument("--runner-command-json", help="Provider Host command as a JSON string array")
    parser.add_argument("--skip-build", action="store_true")
    parser.add_argument("--control-plane-binary", type=pathlib.Path)
    parser.add_argument("--keep", action="store_true", help="Keep SQLite, workspace, cache, and built binary")
    parser.add_argument("--ssh-orbctl-bin", default="orbctl")
    parser.add_argument("--ssh-machine-name", help="Owned disposable OrbStack machine name")
    parser.add_argument("--ssh-machine-arch", choices=("arm64", "amd64"), default="arm64")
    parser.add_argument("--ssh-machine-image", default="ubuntu:24.04")
    parser.add_argument("--ssh-node-version", default="24.13.1")
    parser.add_argument("--docker-socket-path", type=pathlib.Path, default=pathlib.Path("/var/run/docker.sock"))
    parser.add_argument("--docker-worker-image", help="Existing worker-acceptance image used with --docker-skip-worker-build")
    parser.add_argument("--docker-skip-worker-build", action="store_true")
    parser.add_argument("--docker-control-plane-host", default="host.docker.internal")
    parser.add_argument("--docker-network-mode", help="Existing Docker network; the runner creates an isolated network by default")
    parser.add_argument(
        "--docker-allow-network-interruption",
        action="store_true",
        help="Allow disconnect/reconnect of the exact managed Worker on an operator-owned Docker network",
    )
    parser.add_argument("--docker-memory-bytes", type=int, default=2 << 30)
    parser.add_argument("--docker-nano-cpus", type=int, default=1_000_000_000)
    parser.add_argument("--kubernetes-context", help="Explicit reusable Kubernetes context; defaults to an owned Kind cluster")
    parser.add_argument("--kubernetes-kubeconfig", type=pathlib.Path)
    parser.add_argument("--kubernetes-allow-nondisposable", action="store_true")
    parser.add_argument(
        "--kubernetes-worker-image",
        help="Existing worker-acceptance image used with --kubernetes-skip-worker-build",
    )
    parser.add_argument("--kubernetes-skip-worker-build", action="store_true")
    parser.add_argument("--kubernetes-control-plane-host", default="host.docker.internal")
    parser.add_argument("--kind-bin", default="kind")
    parser.add_argument("--kind-cluster-name")
    parser.add_argument("--kind-node-image", default="kindest/node:v1.33.1")
    parser.add_argument(
        "--kubernetes-allow-node-drain",
        action="store_true",
        help="Allow cordon/drain/uncordon of the exact Worker Node on a reused Kubernetes context",
    )
    parser.add_argument(
        "--failure-matrix",
        action="store_true",
        help="Run the deterministic target-specific failure/canary matrix after the core fixture suite",
    )
    parser.add_argument(
        "--failure-case",
        action="append",
        choices=FAILURE_CASES,
        default=[],
        help="Run one deterministic failure/canary case; repeat to select multiple cases",
    )
    parser.add_argument(
        "--failure-only",
        action="store_true",
        help="Run minimal setup, selected failure/canary cases, and a continuity smoke instead of the core suite",
    )
    parser.add_argument(
        "--network-outage-seconds",
        type=float,
        default=8.0,
        help="Worker-only network interruption duration (minimum 7 seconds; default: 8)",
    )
    parser.add_argument(
        "--no-restart-control-plane",
        action="store_true",
        help="Run the second Turn without restarting the Control Plane",
    )
    parsed = parser.parse_args(argv)
    default_timeout = (
        1200.0
        if parsed.target == "kubernetes"
        else 900.0
        if parsed.target in {"docker", "ssh"}
        else 180.0
    )
    timeout_seconds = parsed.timeout if parsed.timeout is not None else default_timeout
    if timeout_seconds <= 0:
        parser.error("--timeout must be positive")
    if parsed.control_plane_binary is not None and not parsed.skip_build:
        parser.error("--control-plane-binary requires --skip-build to prevent overwriting the configured binary")
    if parsed.skip_build and parsed.control_plane_binary is None:
        parser.error("--skip-build requires --control-plane-binary")
    if parsed.target == "ssh" and parsed.skip_build:
        parser.error(
            "--skip-build is not supported for SSH because the runner must cross-build Linux agentd and the "
            "Provider Host fixture together with the Control Plane"
        )
    if parsed.docker_skip_worker_build and not parsed.docker_worker_image:
        parser.error("--docker-skip-worker-build requires --docker-worker-image")
    if parsed.docker_worker_image and not parsed.docker_skip_worker_build:
        parser.error("--docker-worker-image requires --docker-skip-worker-build to avoid overwriting an operator image")
    if parsed.kubernetes_skip_worker_build and not parsed.kubernetes_worker_image:
        parser.error("--kubernetes-skip-worker-build requires --kubernetes-worker-image")
    if parsed.kubernetes_worker_image and not parsed.kubernetes_skip_worker_build:
        parser.error(
            "--kubernetes-worker-image requires --kubernetes-skip-worker-build to avoid overwriting an operator image"
        )
    if parsed.kubernetes_kubeconfig is not None and not parsed.kubernetes_context:
        parser.error("--kubernetes-kubeconfig requires --kubernetes-context")
    if parsed.kind_cluster_name and parsed.kubernetes_context:
        parser.error("--kind-cluster-name cannot be combined with --kubernetes-context")
    if parsed.docker_memory_bytes < 64 << 20:
        parser.error("--docker-memory-bytes must be at least 67108864")
    if parsed.docker_nano_cpus <= 0:
        parser.error("--docker-nano-cpus must be positive")
    if parsed.network_outage_seconds < 7.0:
        parser.error("--network-outage-seconds must be at least 7 seconds to cross the acceptance Lease TTL")
    if not parsed.ssh_orbctl_bin.strip() or any(
        character in parsed.ssh_orbctl_bin for character in "\r\n\t\x00"
    ):
        parser.error("--ssh-orbctl-bin must be a command or executable path")
    ssh_machine_name = parsed.ssh_machine_name.strip() if parsed.ssh_machine_name else None
    if ssh_machine_name is not None and (
        len(ssh_machine_name) > 63
        or not re.fullmatch(r"[a-z0-9](?:[a-z0-9-]*[a-z0-9])?", ssh_machine_name)
    ):
        parser.error("--ssh-machine-name must be a lowercase DNS label")
    if not parsed.ssh_machine_image.strip() or len(parsed.ssh_machine_image.strip()) > 128 or any(
        character in parsed.ssh_machine_image for character in "\r\n\t\x00"
    ):
        parser.error("--ssh-machine-image must be a non-empty OrbStack distro reference")
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", parsed.ssh_node_version.strip()):
        parser.error("--ssh-node-version must be a three-component numeric version")
    docker_socket_path = parsed.docker_socket_path.expanduser()
    if not docker_socket_path.is_absolute():
        parser.error("--docker-socket-path must be absolute")
    if not parsed.docker_control_plane_host.strip() or any(
        character in parsed.docker_control_plane_host for character in "\r\n\t\x00/:"
    ):
        parser.error("--docker-control-plane-host must be a hostname or address without scheme or port")
    if not parsed.kubernetes_control_plane_host.strip() or any(
        character in parsed.kubernetes_control_plane_host for character in "\r\n\t\x00/:"
    ):
        parser.error("--kubernetes-control-plane-host must be a hostname or address without scheme or port")
    if not parsed.kind_bin.strip() or any(character in parsed.kind_bin for character in "\r\n\t\x00"):
        parser.error("--kind-bin must be a command or executable path")
    if not parsed.kind_node_image.strip() or any(character in parsed.kind_node_image for character in "\r\n\t\x00"):
        parser.error("--kind-node-image must be a non-empty image reference")
    kubernetes_context = parsed.kubernetes_context.strip() if parsed.kubernetes_context else None
    if kubernetes_context is not None and any(character in kubernetes_context for character in "\r\n\t\x00"):
        parser.error("--kubernetes-context contains invalid characters")
    kind_cluster_name = parsed.kind_cluster_name.strip() if parsed.kind_cluster_name else None
    if kind_cluster_name is not None and (
        not kind_cluster_name
        or len(kind_cluster_name) > 63
        or kind_cluster_name[0] == "-"
        or kind_cluster_name[-1] == "-"
        or any(character not in "abcdefghijklmnopqrstuvwxyz0123456789-" for character in kind_cluster_name)
    ):
        parser.error("--kind-cluster-name must be a lowercase DNS label")
    try:
        runner_command = parse_runner_command(parsed.runner_command_json, repo_root, parsed.target)
    except ValueError as error:
        parser.error(str(error))
    run_id = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    output_dir = parsed.output_dir or repo_root / ".tmp" / "stage3-provider-acceptance-results" / run_id
    kubernetes_kubeconfig = parsed.kubernetes_kubeconfig.expanduser().resolve() if parsed.kubernetes_kubeconfig else None
    requested_failure_cases = list(parsed.failure_case)
    if parsed.failure_matrix:
        requested_failure_cases.extend(TARGET_FAILURE_CASES[parsed.target])
    requested_failure_case_set = set(requested_failure_cases)
    failure_cases = tuple(case for case in FAILURE_CASES if case in requested_failure_case_set)
    if parsed.failure_only and not failure_cases:
        parser.error("--failure-only requires --failure-matrix or at least one --failure-case")
    if parsed.suite == "real-provider-smoke":
        if parsed.runner_command_json is None:
            parser.error("--suite real-provider-smoke requires an explicit --runner-command-json")
        if failure_cases or parsed.failure_only:
            parser.error(
                "--suite real-provider-smoke cannot be combined with fixture failure/canary options"
            )
    return RunnerOptions(
        target=parsed.target,
        provider=parsed.provider,
        suite=parsed.suite,
        output_dir=output_dir.resolve(),
        timeout_seconds=timeout_seconds,
        runner_command=runner_command,
        skip_build=parsed.skip_build,
        control_plane_binary=parsed.control_plane_binary.resolve() if parsed.control_plane_binary else None,
        keep=parsed.keep,
        restart_control_plane=not parsed.no_restart_control_plane,
        ssh_orbctl_bin=parsed.ssh_orbctl_bin.strip(),
        ssh_machine_name=ssh_machine_name,
        ssh_machine_arch=parsed.ssh_machine_arch,
        ssh_machine_image=parsed.ssh_machine_image.strip(),
        ssh_node_version=parsed.ssh_node_version.strip(),
        docker_socket_path=docker_socket_path,
        docker_worker_image=parsed.docker_worker_image,
        docker_skip_worker_build=parsed.docker_skip_worker_build,
        docker_control_plane_host=parsed.docker_control_plane_host.strip(),
        docker_network_mode=parsed.docker_network_mode,
        docker_memory_bytes=parsed.docker_memory_bytes,
        docker_nano_cpus=parsed.docker_nano_cpus,
        kubernetes_context=kubernetes_context,
        kubernetes_kubeconfig=kubernetes_kubeconfig,
        kubernetes_allow_nondisposable=parsed.kubernetes_allow_nondisposable,
        kubernetes_worker_image=parsed.kubernetes_worker_image,
        kubernetes_skip_worker_build=parsed.kubernetes_skip_worker_build,
        kubernetes_control_plane_host=parsed.kubernetes_control_plane_host.strip(),
        kind_bin=parsed.kind_bin.strip(),
        kind_cluster_name=kind_cluster_name,
        kind_node_image=parsed.kind_node_image.strip(),
        failure_cases=failure_cases,
        network_outage_seconds=parsed.network_outage_seconds,
        docker_allow_network_interruption=parsed.docker_allow_network_interruption,
        kubernetes_allow_node_drain=parsed.kubernetes_allow_node_drain,
        failure_only=parsed.failure_only,
    )


def main(argv: Sequence[str] | None = None) -> int:
    options = parse_args(argv if argv is not None else sys.argv[1:])
    repo_root = pathlib.Path(__file__).resolve().parents[2]
    options.output_dir.mkdir(parents=True, exist_ok=True)
    redactor = SecretRedactor()
    deadline = Deadline(options.timeout_seconds)
    started_at = utc_now()
    started = time.monotonic()
    run_id = f"stage3-provider-acceptance-{uuid.uuid4()}"
    supported_providers = (
        REAL_PROVIDER_SMOKE_PROVIDERS
        if options.suite == "real-provider-smoke"
        else FIXTURE_SUPPORTED_PROVIDERS
    )
    if options.target in {"local", "ssh", "docker", "kubernetes"} and os.name != "posix":
        cases = [
            explicit_unsupported_case(
                "environment.platform-unsupported",
                started_at,
                started,
                "runner.platform_unsupported",
                f"The {options.target} TargetDriver requires a POSIX process-group implementation.",
                {"osName": os.name},
            )
        ]
    elif options.provider not in supported_providers:
        real_provider_smoke = options.suite == "real-provider-smoke"
        cases = [
            explicit_unsupported_case(
                "provider.explicit-unsupported",
                started_at,
                started,
                (
                    "runner.real_provider_smoke_provider_unsupported"
                    if real_provider_smoke
                    else "runner.fixture_provider_unsupported"
                ),
                (
                    f"The real Provider smoke does not support Provider {options.provider}."
                    if real_provider_smoke
                    else f"The deterministic fixture does not implement Provider {options.provider}."
                ),
                {"suite": options.suite, "supportedProviders": sorted(supported_providers)},
            )
        ]
    elif options.target in {"local", "ssh", "docker", "kubernetes"}:
        if options.target == "local":
            driver: LocalDriver = LocalDriver(repo_root, options, deadline, redactor)
        elif options.target == "ssh":
            driver = SSHDriver(repo_root, options, deadline, redactor)
        elif options.target == "docker":
            driver = DockerDriver(repo_root, options, deadline, redactor)
        else:
            driver = KubernetesDriver(repo_root, options, deadline, redactor)
        suite = AcceptanceSuite(options, driver, deadline, redactor)
        driver.install_signal_handlers()
        try:
            suite.run()
        except RunnerInterrupted as error:
            suite.record_interruption(error)
        finally:
            driver.suppress_signals_for_cleanup()
            try:
                try:
                    cleanup_evidence = driver.cleanup()
                    suite.record_cleanup_success(
                        cleanup_evidence if isinstance(cleanup_evidence, Mapping) else None
                    )
                except AcceptanceError as error:
                    suite.record_cleanup_failure(error)
            finally:
                driver.restore_signal_handlers()
        cases = suite.cases
    else:
        driver = MissingTargetDriver(options.target)
        suite = AcceptanceSuite(options, driver, deadline, redactor)
        try:
            suite.run()
        finally:
            cleanup_evidence = driver.cleanup()
            suite.record_cleanup_success(
                cleanup_evidence if isinstance(cleanup_evidence, Mapping) else None
            )
        cases = suite.cases
    real_provider_smoke = options.suite == "real-provider-smoke"
    mode = (
        "real-provider-smoke"
        if real_provider_smoke
        else "fixture+failure-matrix"
        if options.failure_cases
        else "fixture"
    )
    evidence_boundary = (
        "real Codex/Claude through the real Control Plane, selected Target, agentd, Worker Protocol, Provider "
        "Host, Control Plane restart, and native-cursor second Turn; narrow smoke only, not a complete Local or "
        "four-Target Release Gate"
        if real_provider_smoke
        else "deterministic Provider Host fixture over real Control Plane, agentd, Worker Protocol, and Target paths"
    )
    report: dict[str, Any] = {
        "schemaVersion": SCHEMA_VERSION,
        "runId": run_id,
        "target": options.target,
        "provider": options.provider,
        "mode": mode,
        "source": repository_metadata(repo_root),
        "startedAt": started_at,
        "finishedAt": utc_now(),
        "durationMs": elapsed_ms(started),
        "status": aggregate_status(cases),
        "configuration": {
            "suite": options.suite,
            "timeoutSeconds": options.timeout_seconds,
            "restartControlPlane": options.restart_control_plane,
            "skipBuild": options.skip_build,
            "keepState": options.keep,
            "failureMatrix": {
                "requestedCases": list(options.failure_cases),
                "failureOnly": options.failure_only,
                "networkOutageSeconds": options.network_outage_seconds,
                "realProviderReleaseGate": False,
                "boundary": evidence_boundary,
            },
            "runnerCommand": {
                "executable": pathlib.Path(options.runner_command[0]).name,
                "argumentCount": len(options.runner_command) - 1,
            },
            "ssh": {
                "runtime": "owned-disposable-orbstack",
                "orbctlBinary": options.ssh_orbctl_bin,
                "machineName": options.ssh_machine_name or "generated-per-run",
                "machineArch": options.ssh_machine_arch,
                "machineImage": options.ssh_machine_image,
                "controlPlaneTransport": {
                    "mode": "reverse-ssh-loopback",
                    "description": SSH_RELAY_TRANSPORT,
                    "vmListenHost": SSH_RELAY_LOOPBACK_HOST,
                },
                "nodeVersion": options.ssh_node_version,
                "credentialSource": "runner-generated one-time Ed25519 key",
                "localPrivateKeyPlaintextDeletedAfterProvision": True,
                "controlPlaneCredentialLifecycle": SSH_CREDENTIAL_LIFECYCLE,
                "readsUserSSHConfiguration": False,
                "runtimeBuild": "cross-built-per-run",
                "cleanupSemantics": (
                    "ssh/revoke removes the systemd unit, environment, and agentd binary; deletion of the owned "
                    "OrbStack machine is infrastructure cleanup and does not prove product-level Workspace purge"
                ),
            }
            if options.target == "ssh"
            else None,
            "docker": {
                "socketPath": str(options.docker_socket_path),
                "workerImage": options.docker_worker_image,
                "skipWorkerBuild": options.docker_skip_worker_build,
                "controlPlaneHost": options.docker_control_plane_host,
                "networkMode": options.docker_network_mode or "isolated-per-run",
                "memoryBytes": options.docker_memory_bytes,
                "nanoCpus": options.docker_nano_cpus,
                "allowOperatorNetworkInterruption": options.docker_allow_network_interruption,
            }
            if options.target == "docker"
            else None,
            "kubernetes": {
                "context": options.kubernetes_context or "owned-kind-cluster",
                "kubeconfig": str(options.kubernetes_kubeconfig) if options.kubernetes_kubeconfig else None,
                "allowNondisposable": options.kubernetes_allow_nondisposable,
                "workerImage": options.kubernetes_worker_image,
                "skipWorkerBuild": options.kubernetes_skip_worker_build,
                "controlPlaneHost": options.kubernetes_control_plane_host,
                "kindBinary": options.kind_bin,
                "kindClusterName": options.kind_cluster_name,
                "kindNodeImage": options.kind_node_image,
                "allowOperatorNodeDrain": options.kubernetes_allow_node_drain,
            }
            if options.target == "kubernetes"
            else None,
        },
        "cases": cases,
        "artifacts": {
            "jsonReport": str(options.output_dir / JSON_REPORT_NAME),
            "markdownReport": str(options.output_dir / MARKDOWN_REPORT_NAME),
            "logsDirectory": str(options.output_dir / "logs"),
        },
    }
    json_path, markdown_path = write_reports(report, options.output_dir, redactor)
    secret_scan = output_secret_scan_case(options.output_dir, redactor)
    cases.append(secret_scan)
    report["cases"] = cases
    report["status"] = aggregate_status(cases)
    report["finishedAt"] = utc_now()
    report["durationMs"] = elapsed_ms(started)
    json_path, markdown_path = write_reports(report, options.output_dir, redactor)
    print(f"Stage 3 Provider {mode} acceptance: {report['status']}")
    print(f"JSON: {json_path}")
    print(f"Markdown: {markdown_path}")
    return 0 if report["status"] == "pass" else 1


if __name__ == "__main__":
    raise SystemExit(main())
