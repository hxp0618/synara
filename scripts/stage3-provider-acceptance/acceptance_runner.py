#!/usr/bin/env python3
"""Stage 3 Provider Runtime fixture acceptance runner.

The Local driver deliberately exercises the production HTTP and embedded
LocalSupervisor path.  It never registers, heartbeats, or claims a Worker on
behalf of agentd.
"""

from __future__ import annotations

import argparse
import base64
import dataclasses
import datetime as dt
import http.cookiejar
import json
import os
import pathlib
import shutil
import signal
import socket
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
        self.cursor_key = ""
        self.credential_key = ""
        if self._temporary_state:
            shutil.rmtree(self.state_dir, ignore_errors=True)

    def _tool_environment(self) -> dict[str, str]:
        allowed = ("PATH", "HOME", "TMPDIR", "GOCACHE", "GOMODCACHE", "GOPATH", "GOROOT")
        environment = {key: os.environ[key] for key in allowed if key in os.environ}
        environment.setdefault("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")
        return environment

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
        if self.driver.name != "local":
            self._case("runner.target-driver", "Resolve Target driver", self.driver.prepare)
            return self.cases

        self._case("environment.control-plane-build", "Build Control Plane", self.driver.prepare)
        self._case(
            "environment.control-plane-start",
            "Start isolated Personal Control Plane and embedded agentd",
            self.driver.start,
            requires=("environment.control-plane-build",),
        )
        self._case(
            "identity.dev-login",
            "Authenticate through dev-login",
            self._dev_login,
            requires=("environment.control-plane-start",),
        )
        self._case(
            "runtime.worker-discovery",
            "Discover local-default Target and real Worker manifest",
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
        if self.options.restart_control_plane:
            self._case(
                "recovery.control-plane-restart",
                "Restart Control Plane with persisted state",
                self._restart_control_plane,
                requires=("fixture.provider-error",),
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
        self.state.tenant_id = tenant_id
        return {"tenantId": tenant_id, "userId": user.get("userId"), "authenticated": response.get("authenticated")}

    def _discover_worker(self) -> Mapping[str, Any]:
        tenant_id = self._required("tenant_id")
        targets = json_items(
            self.api.request("GET", f"/v1/tenants/{tenant_id}/execution-targets"),
            "execution-targets",
        )
        candidates = [
            target
            for target in targets
            if target.get("kind") == "local" and target.get("name") == "local-default" and target.get("status") == "active"
        ]
        if len(candidates) != 1:
            raise AcceptanceError(
                "runner.local_default_target_missing",
                "Expected exactly one active local-default Target.",
                {"candidateCount": len(candidates), "targets": [self._target_summary(target) for target in targets]},
            )
        target = candidates[0]
        target_id = target.get("id")
        organization_id = target.get("organizationId")
        if not isinstance(target_id, str) or not isinstance(organization_id, str):
            raise AcceptanceError(
                "runner.local_default_target_scope_invalid",
                "local-default Target did not have Tenant and Organization scope.",
            )
        organizations = json_items(
            self.api.request("GET", f"/v1/tenants/{tenant_id}/organizations"),
            "organizations",
        )
        organization = next((item for item in organizations if item.get("id") == organization_id), None)
        if organization is None:
            raise AcceptanceError(
                "runner.target_organization_missing",
                "local-default Target Organization was not visible through the user API.",
                {"organizationId": organization_id},
            )

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

        discovered = self.api.wait_until("an online compatible Worker manifest", manifest_probe)
        self.state.target_id = target_id
        self.state.organization_id = organization_id
        manifest = discovered["manifest"]
        provider = discovered["provider"]
        return {
            "target": self._target_summary(target),
            "organization": {
                "id": organization.get("id"),
                "slug": organization.get("slug"),
                "kind": organization.get("kind"),
            },
            "manifestId": manifest.get("manifestId"),
            "workerStatusCounts": manifest.get("workerStatusCounts"),
            "workerProtocol": manifest.get("workerProtocol"),
            "runtimeEvent": manifest.get("runtimeEvent"),
            "provider": {
                "provider": provider.get("provider"),
                "supportTier": provider.get("supportTier"),
                "compatibilityStatus": provider.get("compatibilityStatus"),
                "runtime": provider.get("runtime"),
                "releasePolicy": provider.get("releasePolicy"),
            },
        }

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


def parse_runner_command(raw: str | None, repo_root: pathlib.Path) -> tuple[str, ...]:
    if raw is None:
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
    parser.add_argument("--timeout", type=float, default=180.0, help="Overall timeout in seconds")
    parser.add_argument("--runner-command-json", help="Provider Host command as a JSON string array")
    parser.add_argument("--skip-build", action="store_true")
    parser.add_argument("--control-plane-binary", type=pathlib.Path)
    parser.add_argument("--keep", action="store_true", help="Keep SQLite, workspace, cache, and built binary")
    parser.add_argument(
        "--no-restart-control-plane",
        action="store_true",
        help="Run the second Turn without restarting the Control Plane",
    )
    parsed = parser.parse_args(argv)
    if parsed.timeout <= 0:
        parser.error("--timeout must be positive")
    if parsed.control_plane_binary is not None and not parsed.skip_build:
        parser.error("--control-plane-binary requires --skip-build to prevent overwriting the configured binary")
    if parsed.skip_build and parsed.control_plane_binary is None:
        parser.error("--skip-build requires --control-plane-binary")
    try:
        runner_command = parse_runner_command(parsed.runner_command_json, repo_root)
    except ValueError as error:
        parser.error(str(error))
    run_id = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    output_dir = parsed.output_dir or repo_root / ".tmp" / "stage3-provider-acceptance-results" / run_id
    return RunnerOptions(
        target=parsed.target,
        provider=parsed.provider,
        output_dir=output_dir.resolve(),
        timeout_seconds=parsed.timeout,
        runner_command=runner_command,
        skip_build=parsed.skip_build,
        control_plane_binary=parsed.control_plane_binary.resolve() if parsed.control_plane_binary else None,
        keep=parsed.keep,
        restart_control_plane=not parsed.no_restart_control_plane,
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
    if options.target == "local" and os.name != "posix":
        cases = [
            explicit_unsupported_case(
                "environment.platform-unsupported",
                started_at,
                started,
                "runner.platform_unsupported",
                "The Local TargetDriver requires a POSIX process-group implementation.",
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
    elif options.target == "local":
        driver = LocalDriver(repo_root, options, deadline, redactor)
        suite = AcceptanceSuite(options, driver, deadline, redactor)
        driver.install_signal_handlers()
        try:
            suite.run()
        except RunnerInterrupted as error:
            suite.record_interruption(error)
        finally:
            try:
                driver.cleanup()
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
