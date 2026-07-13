#!/usr/bin/env python3
"""Stage 3 Provider Runtime fixture acceptance runner.

Target drivers exercise production Control Plane, Worker, and agentd paths.
The runner never registers, heartbeats, or claims a Worker on behalf of
agentd. The Local driver uses LocalSupervisor; the Docker driver provisions a
managed Docker Execution Target through the user API and reconciler.
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
import json
import os
import pathlib
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
DOCKER_VOLUME_SENTINEL_PATH = "/data/.synara-stage3-provider-acceptance-volume"
DOCKER_VOLUME_SENTINEL_VALUE = "synara-stage3-named-volume-continuity-v1"
WORKER_PROXY_ALLOWED_PATH_PREFIXES = ("/v1/workers/", "/v1/artifact-content/")
WORKER_PROXY_MAX_REQUEST_BYTES = 64 << 20
CASE_STATUSES = frozenset({"pass", "unsupported", "skipped", "fail"})
TERMINAL_EVENT_TYPES = frozenset({"execution.completed", "execution.failed", "execution.cancelled"})
JSON_REPORT_NAME = "acceptance-report.json"
MARKDOWN_REPORT_NAME = "acceptance-report.md"
PROVIDERS = ("codex", "claudeAgent", "cursor", "gemini", "grok", "kilo", "opencode", "pi")
FIXTURE_SUPPORTED_PROVIDERS = frozenset({"codex", "claudeAgent"})

T = TypeVar("T")


def utc_now() -> str:
    return dt.datetime.now(dt.timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def elapsed_ms(started: float) -> int:
    return max(0, round((time.monotonic() - started) * 1000))


def random_key() -> str:
    return base64.urlsafe_b64encode(os.urandom(32)).decode("ascii").rstrip("=")


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


class AcceptanceError(RuntimeError):
    def __init__(self, code: str, message: str, evidence: Mapping[str, Any] | None = None) -> None:
        super().__init__(message)
        self.code = code
        self.evidence = dict(evidence or {})


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
    output_dir: pathlib.Path
    timeout_seconds: float
    runner_command: tuple[str, ...]
    skip_build: bool
    control_plane_binary: pathlib.Path | None
    keep: bool
    restart_control_plane: bool
    docker_socket_path: pathlib.Path
    docker_worker_image: str | None
    docker_skip_worker_build: bool
    docker_control_plane_host: str
    docker_network_mode: str | None
    docker_memory_bytes: int
    docker_nano_cpus: int


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
            with self.opener.open(request, timeout=self.deadline.request_timeout()) as response:
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

    def stop(self) -> None: ...

    def cleanup(self) -> None: ...


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
        self.thread = threading.Thread(
            target=self.server.serve_forever,
            kwargs={"poll_interval": 0.1},
            name="stage3-worker-only-proxy",
            daemon=True,
        )

    def start(self) -> None:
        self.thread.start()

    def stop(self) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5.0)
        if self.thread.is_alive():
            raise AcceptanceError(
                "runner.worker_proxy_stop_failed",
                "Worker-only proxy did not stop within five seconds.",
            )

    def evidence(self, advertised_host: str) -> Mapping[str, Any]:
        return {
            "listenAddress": "0.0.0.0",
            "advertisedHost": advertised_host,
            "port": self.port,
            "upstreamAddress": f"127.0.0.1:{self.upstream_port}",
            "allowedPathPrefixes": list(WORKER_PROXY_ALLOWED_PATH_PREFIXES),
        }


class LocalDriver:
    name = "local"

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
            return {"build": "skipped", "binary": str(self.binary_path)}

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

    def cleanup(self) -> None:
        self.stop()
        self._release_state()

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


class DockerDriver(LocalDriver):
    name = "docker"

    def __init__(
        self,
        repo_root: pathlib.Path,
        options: RunnerOptions,
        deadline: Deadline,
        redactor: SecretRedactor,
    ) -> None:
        super().__init__(repo_root, options, deadline, redactor)
        suffix = uuid.uuid4().hex[:12]
        self.registration_token = random_key()
        self.redactor.add(self.registration_token, "[REDACTED_WORKER_REGISTRATION_TOKEN]")
        self.target_name = f"stage3-docker-{suffix}"
        self.volume_name = f"synara-stage3-{suffix}"
        self.network_name = options.docker_network_mode or f"synara-stage3-{suffix}"
        self.owns_network = options.docker_network_mode is None
        self.owns_image = not options.docker_skip_worker_build
        self.head_sha = self._head_sha()
        self.image = options.docker_worker_image or f"synara-stage3-provider-acceptance:{self.head_sha}-{suffix}"
        self.target_id: str | None = None
        self.container_name: str | None = None
        self.worker_proxy: _WorkerOnlyProxy | None = None

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
            "workerProxy": self.worker_proxy.evidence(self.options.docker_control_plane_host),
        }

    def prepare(self) -> Mapping[str, Any]:
        control_plane = super().prepare()
        socket_evidence = self._ping_socket()
        version = self._docker_command(
            ["version", "--format", "{{.Server.Version}}"],
            log_path=self.logs_dir / "docker-version.log",
        ).strip()
        if not version:
            raise AcceptanceError("runner.docker_engine_unavailable", "Docker did not report a Server version.")

        build_started = time.monotonic()
        if self.options.docker_skip_worker_build:
            self._docker_command(
                ["image", "inspect", "--format", "{{.Id}}", self.image],
                log_path=self.logs_dir / "docker-image-inspect.log",
            )
            build_evidence: dict[str, Any] = {"build": "skipped"}
        else:
            self._docker_command(
                [
                    "build",
                    "--target",
                    "worker-acceptance",
                    "--tag",
                    self.image,
                    "--label",
                    "synara.io/stage3-provider-acceptance=true",
                    "--label",
                    f"org.opencontainers.image.revision={self.head_sha}",
                    str(self.repo_root),
                ],
                log_path=self.logs_dir / "docker-worker-build.log",
                maximum_timeout=max(60.0, self.deadline.remaining()),
            )
            build_evidence = {"build": "completed", "durationMs": elapsed_ms(build_started)}

        self._docker_command(
            [
                "run",
                "--rm",
                "--entrypoint",
                "sh",
                self.image,
                "-lc",
                "test -x /usr/local/bin/synara-agentd && "
                "test -x /usr/local/bin/provider-host && "
                "test -r /opt/synara/acceptance/provider-host-fixture.mjs && "
                "node --version",
            ],
            log_path=self.logs_dir / "docker-worker-smoke.log",
        )
        image_id = self._docker_command(
            ["image", "inspect", "--format", "{{.Id}}", self.image]
        ).strip()
        if self.owns_network:
            self._docker_command(
                [
                    "network",
                    "create",
                    "--label",
                    "synara.io/stage3-provider-acceptance=true",
                    self.network_name,
                ],
                log_path=self.logs_dir / "docker-network-create.log",
            )
        else:
            self._docker_command(["network", "inspect", self.network_name])
        return {
            "controlPlane": control_plane,
            "docker": {
                "serverVersion": version,
                "socket": socket_evidence,
                "workerImage": self.image,
                "workerImageId": image_id,
                "networkMode": self.network_name,
                **build_evidence,
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

    def cleanup(self) -> None:
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
            docker_cleanup(
                "remove named Workspace volume",
                ["volume", "rm", "-f", self.volume_name],
                20.0,
                ("no such volume",),
            )
            if self.owns_network:
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
                "SYNARA_DOCKER_RECONCILE_INTERVAL": "250ms",
            }
        )
        return environment

    def _worker_proxy_url(self) -> str:
        if self.worker_proxy is None or not self.worker_proxy.thread.is_alive():
            raise AcceptanceError(
                "runner.worker_proxy_unavailable",
                "Worker-only proxy was unavailable while provisioning the Docker Target.",
            )
        return f"http://{self.options.docker_control_plane_host}:{self.worker_proxy.port}"

    def _stop_worker_proxy(self) -> None:
        proxy = self.worker_proxy
        self.worker_proxy = None
        if proxy is not None:
            proxy.stop()

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

    def _head_sha(self) -> str:
        completed = subprocess.run(
            ["git", "rev-parse", "--short=12", "HEAD"],
            cwd=self.repo_root,
            env=self._tool_environment(),
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            timeout=10.0,
            check=False,
        )
        value = completed.stdout.strip().lower()
        return value if completed.returncode == 0 and value else "unknown"

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
            raise AcceptanceError("runner.worker_identity_missing", "The managed Docker Worker identity was missing.")
        return {
            "id": str(row[0]),
            "incarnation": int(row[1]),
            "instanceUid": str(row[2]),
            "status": str(row[3]),
            "podName": str(row[4]),
        }


class MissingTargetDriver:
    def __init__(self, name: str) -> None:
        self.name = name
        self.api: APIClient | None = None

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
        self._case(
            "runtime.worker-discovery",
            "Provision the exact Target and discover a real compatible Worker manifest",
            self._discover_worker,
            requires=("identity.dev-login",),
        )
        self._case(
            "resources.credential-project-session",
            "Create bound Credential, empty Repository Project, and Session",
            self._create_resources,
            requires=("runtime.worker-discovery",),
        )
        scenario_requirement = ("resources.credential-project-session",)
        self._case(
            "fixture.text-tool-usage-artifact",
            "Run text, tool, usage, Artifact, and Credential fixture flow",
            self._text_tool_usage_artifact,
            requires=scenario_requirement,
        )
        self._case(
            "fixture.approval-resolution",
            "Resolve Provider approval through the user API",
            self._approval_resolution,
            requires=("fixture.text-tool-usage-artifact",),
        )
        self._case(
            "fixture.user-input-resolution",
            "Resolve Provider user input through the user API",
            self._user_input_resolution,
            requires=("fixture.approval-resolution",),
        )
        self._case(
            "fixture.provider-error",
            "Persist deterministic Provider failure",
            self._provider_error,
            requires=("fixture.user-input-resolution",),
        )
        recovery_requirement = "fixture.provider-error"
        if self.driver.name == "docker":
            self._case(
                "recovery.worker-replacement",
                "Replace the managed Docker Worker and verify a new agentd incarnation",
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

    def record_cleanup_failure(self, error: AcceptanceError) -> None:
        self._fail_case(
            "environment.cleanup",
            "Clean isolated Target resources",
            error.code,
            str(error),
            error.evidence,
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
        except AcceptanceError as error:
            case.update(
                {
                    "status": "fail",
                    "finishedAt": utc_now(),
                    "durationMs": elapsed_ms(started),
                    "reasonCode": error.code,
                    "message": self.redactor.text(str(error)),
                    "evidence": self.redactor.value(error.evidence),
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

    def _discover_worker(self) -> Mapping[str, Any]:
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
        discovered = self._wait_compatible_manifest(target_id)
        self.state.target_id = target_id
        manifest = discovered["manifest"]
        provider = discovered["provider"]
        return {
            "target": self._target_summary(target),
            "driverEvidence": target.get("driverEvidence"),
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
        session = json_object(
            self.api.request(
                "POST",
                f"/v1/projects/{project_id}/sessions",
                {
                    "title": "Stage 3 Provider Acceptance",
                    "visibility": "project",
                    "provider": self.options.provider,
                    "model": "stage3-acceptance-fixture",
                    "providerCredentialId": credential_id,
                    "executionTargetId": target_id,
                },
                expected=(201,),
            ),
            "session",
        )
        session_id = session.get("id")
        if not isinstance(session_id, str):
            raise AcceptanceError("runner.session_id_missing", "Session API did not return an ID.")
        if session.get("executionTargetId") != target_id or session.get("providerCredentialId") != credential_id:
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
            "credential": {
                "id": credential_id,
                "provider": credential.get("provider"),
                "credentialType": credential.get("credentialType"),
                "version": credential.get("version"),
                "organizationId": credential.get("organizationId"),
            },
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

    def _approval_resolution(self) -> Mapping[str, Any]:
        turn = self._create_turn("[approval]")
        interaction = self._wait_for_interaction(str(turn["id"]), "approval")
        execution_id = str(interaction["executionId"])
        request_id = str(interaction["requestId"])
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
        return {
            "turnId": turn.get("id"),
            "executionId": terminal.get("executionId"),
            "requestId": request_id,
            "interactionId": interaction.get("id"),
            "resolutionStatus": resolved.get("status"),
            "deliveryStatus": resolved.get("deliveryStatus"),
            "sequenceRange": self._sequence_range(events),
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
                "Docker replacement evidence omitted the replacement Worker ID.",
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
            "semantics": "persisted named-volume Workspace content; not Workspace Checkpoint restore",
        }

    def _restart_control_plane(self) -> Mapping[str, Any]:
        events = self._all_events()
        if not events:
            raise AcceptanceError("runner.restart_without_events", "No Session events existed before restart.")
        self.state.pre_restart_sequence = int(events[-1]["sequence"])
        restarted = self.driver.restart()
        self.state.restarted = True
        tenant_id = self._required("tenant_id")
        target_id = self._required("target_id")

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

        manifest = self.api.wait_until("a post-restart online Worker", worker_probe)
        return {
            **restarted,
            "preRestartSequence": self.state.pre_restart_sequence,
            "postRestartManifestId": manifest.get("manifestId"),
            "workerStatusCounts": manifest.get("workerStatusCounts"),
        }

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
            "workerIdSemantics": "stable registration slot; a restarted agentd registration may reuse the Worker ID",
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
            execution_id = created.get("executionId")
            if not isinstance(execution_id, str):
                created_payload = created.get("payload")
                if isinstance(created_payload, dict):
                    execution_id = created_payload.get("executionId")
            if not isinstance(execution_id, str) or not execution_id:
                raise AcceptanceError(
                    "runner.turn_execution_id_missing",
                    "turn.created did not identify its Execution.",
                    {"turnId": turn_id, "event": self._event_summary(created)},
                )
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
    def _event_turn_id(event: Mapping[str, Any]) -> str | None:
        payload = event.get("payload")
        if isinstance(payload, dict) and isinstance(payload.get("turnId"), str):
            return str(payload["turnId"])
        return None

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
    lines = [
        "# Stage 3 Provider Fixture Acceptance",
        "",
        f"- Schema: `{report['schemaVersion']}`",
        f"- Run: `{report['runId']}`",
        f"- Target: `{report['target']}`",
        f"- Provider: `{report['provider']}`",
        f"- Status: **{report['status']}**",
        f"- Started: `{report['startedAt']}`",
        f"- Finished: `{report['finishedAt']}`",
        f"- Duration: `{report['durationMs']} ms`",
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
    parser.add_argument("--output-dir", type=pathlib.Path)
    parser.add_argument("--timeout", type=float, help="Overall timeout in seconds (default: Local 180, Docker 900)")
    parser.add_argument("--runner-command-json", help="Provider Host command as a JSON string array")
    parser.add_argument("--skip-build", action="store_true")
    parser.add_argument("--control-plane-binary", type=pathlib.Path)
    parser.add_argument("--keep", action="store_true", help="Keep SQLite, workspace, cache, and built binary")
    parser.add_argument("--docker-socket-path", type=pathlib.Path, default=pathlib.Path("/var/run/docker.sock"))
    parser.add_argument("--docker-worker-image", help="Existing worker-acceptance image used with --docker-skip-worker-build")
    parser.add_argument("--docker-skip-worker-build", action="store_true")
    parser.add_argument("--docker-control-plane-host", default="host.docker.internal")
    parser.add_argument("--docker-network-mode", help="Existing Docker network; the runner creates an isolated network by default")
    parser.add_argument("--docker-memory-bytes", type=int, default=2 << 30)
    parser.add_argument("--docker-nano-cpus", type=int, default=1_000_000_000)
    parser.add_argument(
        "--no-restart-control-plane",
        action="store_true",
        help="Run the second Turn without restarting the Control Plane",
    )
    parsed = parser.parse_args(argv)
    timeout_seconds = parsed.timeout if parsed.timeout is not None else (900.0 if parsed.target == "docker" else 180.0)
    if timeout_seconds <= 0:
        parser.error("--timeout must be positive")
    if parsed.control_plane_binary is not None and not parsed.skip_build:
        parser.error("--control-plane-binary requires --skip-build to prevent overwriting the configured binary")
    if parsed.skip_build and parsed.control_plane_binary is None:
        parser.error("--skip-build requires --control-plane-binary")
    if parsed.docker_skip_worker_build and not parsed.docker_worker_image:
        parser.error("--docker-skip-worker-build requires --docker-worker-image")
    if parsed.docker_worker_image and not parsed.docker_skip_worker_build:
        parser.error("--docker-worker-image requires --docker-skip-worker-build to avoid overwriting an operator image")
    if parsed.docker_memory_bytes < 64 << 20:
        parser.error("--docker-memory-bytes must be at least 67108864")
    if parsed.docker_nano_cpus <= 0:
        parser.error("--docker-nano-cpus must be positive")
    docker_socket_path = parsed.docker_socket_path.expanduser()
    if not docker_socket_path.is_absolute():
        parser.error("--docker-socket-path must be absolute")
    if not parsed.docker_control_plane_host.strip() or any(
        character in parsed.docker_control_plane_host for character in "\r\n\t\x00/:"
    ):
        parser.error("--docker-control-plane-host must be a hostname or address without scheme or port")
    try:
        runner_command = parse_runner_command(parsed.runner_command_json, repo_root, parsed.target)
    except ValueError as error:
        parser.error(str(error))
    run_id = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    output_dir = parsed.output_dir or repo_root / ".tmp" / "stage3-provider-acceptance-results" / run_id
    return RunnerOptions(
        target=parsed.target,
        provider=parsed.provider,
        output_dir=output_dir.resolve(),
        timeout_seconds=timeout_seconds,
        runner_command=runner_command,
        skip_build=parsed.skip_build,
        control_plane_binary=parsed.control_plane_binary.resolve() if parsed.control_plane_binary else None,
        keep=parsed.keep,
        restart_control_plane=not parsed.no_restart_control_plane,
        docker_socket_path=docker_socket_path,
        docker_worker_image=parsed.docker_worker_image,
        docker_skip_worker_build=parsed.docker_skip_worker_build,
        docker_control_plane_host=parsed.docker_control_plane_host.strip(),
        docker_network_mode=parsed.docker_network_mode,
        docker_memory_bytes=parsed.docker_memory_bytes,
        docker_nano_cpus=parsed.docker_nano_cpus,
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
    if options.target in {"local", "docker"} and os.name != "posix":
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
    elif options.provider not in FIXTURE_SUPPORTED_PROVIDERS:
        cases = [
            explicit_unsupported_case(
                "provider.explicit-unsupported",
                started_at,
                started,
                "runner.fixture_provider_unsupported",
                f"The deterministic fixture does not implement Provider {options.provider}.",
                {"fixtureSupportedProviders": sorted(FIXTURE_SUPPORTED_PROVIDERS)},
            )
        ]
    elif options.target in {"local", "docker"}:
        driver: LocalDriver = (
            LocalDriver(repo_root, options, deadline, redactor)
            if options.target == "local"
            else DockerDriver(repo_root, options, deadline, redactor)
        )
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
                    driver.cleanup()
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
            driver.cleanup()
        cases = suite.cases
    report: dict[str, Any] = {
        "schemaVersion": SCHEMA_VERSION,
        "runId": run_id,
        "target": options.target,
        "provider": options.provider,
        "mode": "fixture",
        "source": repository_metadata(repo_root),
        "startedAt": started_at,
        "finishedAt": utc_now(),
        "durationMs": elapsed_ms(started),
        "status": aggregate_status(cases),
        "configuration": {
            "timeoutSeconds": options.timeout_seconds,
            "restartControlPlane": options.restart_control_plane,
            "skipBuild": options.skip_build,
            "keepState": options.keep,
            "runnerCommand": {
                "executable": pathlib.Path(options.runner_command[0]).name,
                "argumentCount": len(options.runner_command) - 1,
            },
            "docker": {
                "socketPath": str(options.docker_socket_path),
                "workerImage": options.docker_worker_image,
                "skipWorkerBuild": options.docker_skip_worker_build,
                "controlPlaneHost": options.docker_control_plane_host,
                "networkMode": options.docker_network_mode or "isolated-per-run",
                "memoryBytes": options.docker_memory_bytes,
                "nanoCpus": options.docker_nano_cpus,
            }
            if options.target == "docker"
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
    print(f"Stage 3 Provider fixture acceptance: {report['status']}")
    print(f"JSON: {json_path}")
    print(f"Markdown: {markdown_path}")
    return 0 if report["status"] == "pass" else 1


if __name__ == "__main__":
    raise SystemExit(main())
