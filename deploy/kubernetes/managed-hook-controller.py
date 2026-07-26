#!/usr/bin/env python3
"""Run managed resilience hooks inside an owned containment boundary.

The production backend is deliberately Linux/systemd-user-only.  It creates a
deterministically named transient service for one operation/phase and never
uses a shared signing secret.  The process-group backend exists only for
repository validation; its result explicitly says that it is not a security
boundary.

Input is one JSON document on stdin.  Output is one compact, sanitized JSON
document on stdout.  Hook stdout, stderr, argv, environment, scope names,
challenge values, check names, and process identifiers are never returned.
"""

from __future__ import annotations

import argparse
import ctypes
import datetime as dt
import hashlib
import json
import os
import pathlib
import re
import shutil
import signal
import stat
import subprocess
import sys
import time
from dataclasses import dataclass
from typing import Any, Iterable


REQUEST_SCHEMA = "synara.managed-hook-controller.request.v1"
RESULT_SCHEMA = "synara.managed-hook-controller.result.v1"
ARTIFACT_SCHEMA = "synara.managed-hook-transition.v1"
MAX_REQUEST_BYTES = 64 * 1024
MAX_ARTIFACT_BYTES = 64 * 1024
PHASE_TRANSITIONS = {
    "start": "terminal-applied",
    "verify": "applied",
    "stop": "terminal-healed",
}
IDENTIFIER = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:@/+\-]{0,255}$")
KUBE_NAME = re.compile(r"^[a-z0-9](?:[-a-z0-9.]*[a-z0-9])?$")
UID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:\-]{7,255}$")
CHALLENGE = re.compile(r"^[A-Za-z0-9_-]{32,256}$")
DIGEST = re.compile(r"^[0-9a-f]{64}$")
HANDLED_SIGNALS = tuple(
    item
    for item in (getattr(signal, "SIGHUP", None), signal.SIGINT, signal.SIGTERM)
    if item is not None
)


class ControllerError(Exception):
    """A sanitized controller failure."""

    def __init__(self, code: str, *, exit_code: int = 1) -> None:
        super().__init__(code)
        self.code = code
        self.exit_code = exit_code


class UnsupportedError(ControllerError):
    def __init__(self, code: str) -> None:
        super().__init__(code, exit_code=125)


def canonical(value: Any) -> bytes:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True).encode("ascii")


def digest(value: Any) -> str:
    return hashlib.sha256(canonical(value)).hexdigest()


def strict_json_loads(payload: bytes, error_code: str, *, exit_code: int = 1) -> Any:
    """Parse standards-compliant JSON and reject duplicate object members."""

    def object_from_pairs(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
        result: dict[str, Any] = {}
        for key, value in pairs:
            if key in result:
                raise ValueError("duplicate object member")
            result[key] = value
        return result

    def reject_constant(_value: str) -> None:
        raise ValueError("non-finite JSON number")

    try:
        return json.loads(
            payload.decode("utf-8"),
            object_pairs_hook=object_from_pairs,
            parse_constant=reject_constant,
        )
    except (UnicodeDecodeError, json.JSONDecodeError, ValueError) as error:
        raise ControllerError(error_code, exit_code=exit_code) from error


def require_object(value: Any, label: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise ControllerError(f"invalid-{label}", exit_code=125)
    return value


def require_exact_keys(value: dict[str, Any], allowed: set[str], required: set[str], label: str) -> None:
    if not required.issubset(value) or not set(value).issubset(allowed):
        raise ControllerError(f"invalid-{label}-fields", exit_code=125)


def require_string(value: Any, pattern: re.Pattern[str], label: str) -> str:
    if not isinstance(value, str) or not pattern.fullmatch(value):
        raise ControllerError(f"invalid-{label}", exit_code=125)
    return value


@dataclass(frozen=True)
class Scope:
    context: str
    namespace: str
    node: str
    node_uid: str
    pod: str
    pod_uid: str
    challenge: str

    @classmethod
    def parse(cls, value: Any) -> "Scope":
        data = require_object(value, "scope")
        keys = {"context", "namespace", "node", "nodeUid", "pod", "podUid", "challenge"}
        require_exact_keys(data, keys, keys, "scope")
        return cls(
            context=require_string(data["context"], IDENTIFIER, "context"),
            namespace=require_string(data["namespace"], KUBE_NAME, "namespace"),
            node=require_string(data["node"], IDENTIFIER, "node"),
            node_uid=require_string(data["nodeUid"], UID, "node-uid"),
            pod=require_string(data["pod"], KUBE_NAME, "pod"),
            pod_uid=require_string(data["podUid"], UID, "pod-uid"),
            challenge=require_string(data["challenge"], CHALLENGE, "challenge"),
        )

    def artifact_fields(self) -> dict[str, str]:
        return {
            "context": self.context,
            "namespace": self.namespace,
            "node": self.node,
            "nodeUid": self.node_uid,
            "pod": self.pod,
            "podUid": self.pod_uid,
            "challenge": self.challenge,
        }

    def digest_fields(self) -> dict[str, str]:
        return self.artifact_fields()


@dataclass(frozen=True)
class Request:
    action: str
    operation_id: str
    phase: str
    backend: str
    security_boundary: bool
    scope: Scope | None
    command: tuple[str, ...]
    artifact_path: str | None
    timeout_seconds: int
    stop_timeout_seconds: int
    freshness_seconds: int

    @classmethod
    def parse(cls, raw: Any) -> "Request":
        data = require_object(raw, "request")
        allowed = {
            "schemaVersion",
            "action",
            "operationId",
            "phase",
            "backend",
            "securityBoundary",
            "scope",
            "command",
            "artifactPath",
            "timeoutSeconds",
            "stopTimeoutSeconds",
            "freshnessSeconds",
        }
        common = {"schemaVersion", "action", "operationId", "phase", "backend", "securityBoundary"}
        require_exact_keys(data, allowed, common, "request")
        if data["schemaVersion"] != REQUEST_SCHEMA:
            raise ControllerError("unsupported-request-schema", exit_code=125)
        action = data["action"]
        if action not in ("execute", "recover"):
            raise ControllerError("invalid-action", exit_code=125)
        operation_id = require_string(data["operationId"], UID, "operation-id")
        phase = data["phase"]
        if phase not in PHASE_TRANSITIONS:
            raise ControllerError("invalid-phase", exit_code=125)
        backend = data["backend"]
        if backend not in ("systemd-user", "process-group-test"):
            raise ControllerError("invalid-backend", exit_code=125)
        security_boundary = data["securityBoundary"]
        if not isinstance(security_boundary, bool):
            raise ControllerError("invalid-security-boundary", exit_code=125)

        scope: Scope | None = None
        command: tuple[str, ...] = ()
        artifact_path: str | None = None
        timeout_seconds = 30
        stop_timeout_seconds = 5
        freshness_seconds = 60
        if action == "execute":
            required = common | {
                "scope",
                "command",
                "artifactPath",
                "timeoutSeconds",
                "stopTimeoutSeconds",
                "freshnessSeconds",
            }
            require_exact_keys(data, allowed, required, "execute-request")
            scope = Scope.parse(data["scope"])
            command_value = data["command"]
            if (
                not isinstance(command_value, list)
                or not command_value
                or len(command_value) > 128
                or any(not isinstance(arg, str) or "\x00" in arg or len(arg) > 16_384 for arg in command_value)
            ):
                raise ControllerError("invalid-command", exit_code=125)
            if not os.path.isabs(command_value[0]):
                raise ControllerError("command-executable-not-absolute", exit_code=125)
            command = tuple(command_value)
            artifact_path = data["artifactPath"]
            if not isinstance(artifact_path, str) or not os.path.isabs(artifact_path) or "\x00" in artifact_path:
                raise ControllerError("invalid-artifact-path", exit_code=125)
            timeout_seconds = bounded_integer(data["timeoutSeconds"], "timeout", 1, 3600)
            stop_timeout_seconds = bounded_integer(data["stopTimeoutSeconds"], "stop-timeout", 1, 120)
            freshness_seconds = bounded_integer(data["freshnessSeconds"], "freshness", 1, 300)
        else:
            require_exact_keys(data, common, common, "recover-request")

        if backend == "systemd-user" and not security_boundary:
            raise ControllerError("systemd-requires-security-boundary", exit_code=125)
        if backend == "process-group-test":
            if security_boundary:
                raise ControllerError("test-backend-is-not-security-boundary", exit_code=125)
            if scope is not None and scope.context != "managed-validation":
                raise ControllerError("test-backend-context-forbidden", exit_code=125)

        return cls(
            action=action,
            operation_id=operation_id,
            phase=phase,
            backend=backend,
            security_boundary=security_boundary,
            scope=scope,
            command=command,
            artifact_path=artifact_path,
            timeout_seconds=timeout_seconds,
            stop_timeout_seconds=stop_timeout_seconds,
            freshness_seconds=freshness_seconds,
        )

    @property
    def unit_name(self) -> str:
        suffix = digest({"operationId": self.operation_id, "phase": self.phase})[:32]
        return f"synara-managed-hook-{self.phase}-{suffix}.service"

    @property
    def scope_digest(self) -> str | None:
        return None if self.scope is None else digest(self.scope.digest_fields())


def bounded_integer(value: Any, label: str, minimum: int, maximum: int) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or not minimum <= value <= maximum:
        raise ControllerError(f"invalid-{label}-seconds", exit_code=125)
    return value


@dataclass(frozen=True)
class MemberSummary:
    executable: int = 0
    zombie: int = 0
    dead_upper: int = 0
    dead_lower: int = 0
    unreadable: int = 0

    @classmethod
    def from_states(cls, states: Iterable[str | None]) -> "MemberSummary":
        counts = {"executable": 0, "zombie": 0, "dead_upper": 0, "dead_lower": 0, "unreadable": 0}
        for state in states:
            if state == "Z":
                counts["zombie"] += 1
            elif state == "X":
                counts["dead_upper"] += 1
            elif state == "x":
                counts["dead_lower"] += 1
            elif state is None:
                counts["unreadable"] += 1
            else:
                counts["executable"] += 1
        return cls(**counts)

    def sanitized(self) -> dict[str, int]:
        return {
            "executable": self.executable,
            "zombie": self.zombie,
            "deadUpper": self.dead_upper,
            "deadLower": self.dead_lower,
            "unreadable": self.unreadable,
        }


@dataclass(frozen=True)
class BackendOutcome:
    hook_exit_code: int | None
    timed_out: bool
    cancelled: bool
    unit_found: bool
    termination_confirmed: bool
    members: MemberSummary


@dataclass(frozen=True)
class UnitState:
    found: bool
    cgroup: str | None = None
    result: str | None = None
    invocation_id: str | None = None


@dataclass
class CgroupHandle:
    path: str
    descriptor: int
    device: int
    inode: int

    def close(self) -> None:
        if self.descriptor >= 0:
            os.close(self.descriptor)
            self.descriptor = -1


class Cancellation:
    def __init__(self) -> None:
        self.signal_number: int | None = None

    def install(self) -> None:
        def requested(signum: int, _frame: Any) -> None:
            if self.signal_number is None:
                self.signal_number = signum

        for signum in HANDLED_SIGNALS:
            signal.signal(signum, requested)

    @property
    def requested(self) -> bool:
        return self.signal_number is not None


def process_state(pid: int) -> str | None:
    """Return the Linux /proc state field, correctly handling ')' in comm."""
    try:
        data = pathlib.Path(f"/proc/{pid}/stat").read_text(encoding="utf-8")
        close = data.rfind(")")
        if close < 0:
            return None
        fields = data[close + 2 :].split()
        return fields[0] if fields else None
    except (OSError, UnicodeDecodeError):
        return None


def process_identity(pid: int) -> str | None:
    if not sys.platform.startswith("linux") or pid <= 1:
        return None
    try:
        data = pathlib.Path(f"/proc/{pid}/stat").read_text(encoding="utf-8")
        close = data.rfind(")")
        fields = data[close + 2 :].split()
        return f"linux-proc-start:{fields[19]}"
    except (OSError, IndexError, UnicodeDecodeError):
        return None


def set_not_dumpable() -> None:
    """Best-effort Linux PR_SET_DUMPABLE=0; absence is not a secret boundary."""
    if not sys.platform.startswith("linux"):
        return
    try:
        libc = ctypes.CDLL(None, use_errno=True)
        if libc.prctl(4, 0, 0, 0, 0) != 0:  # PR_SET_DUMPABLE
            raise OSError(ctypes.get_errno(), "prctl(PR_SET_DUMPABLE) failed")
    except (AttributeError, OSError):
        # This controller deliberately has no signing material.  systemd/cgroup
        # ownership, not dumpability, is the production authority boundary.
        return


def child_exited_wnowait(process: subprocess.Popen[bytes]) -> bool:
    required = ("waitid", "P_PID", "WEXITED", "WNOHANG", "WNOWAIT")
    if not all(hasattr(os, name) for name in required):
        raise UnsupportedError("waitid-wnowait-unavailable")
    try:
        result = os.waitid(os.P_PID, process.pid, os.WEXITED | os.WNOHANG | os.WNOWAIT)
    except ChildProcessError as error:
        raise ControllerError("direct-child-reaped-before-cleanup", exit_code=125) from error
    return result is not None


def reap_after_cleanup(process: subprocess.Popen[bytes]) -> int:
    try:
        return process.wait(timeout=2)
    except (subprocess.TimeoutExpired, ChildProcessError) as error:
        raise ControllerError("direct-child-reap-failed", exit_code=125) from error


def cgroup_path(cgroup: str) -> pathlib.Path:
    if not cgroup.startswith("/") or ".." in pathlib.PurePosixPath(cgroup).parts:
        raise ControllerError("invalid-unit-cgroup", exit_code=125)
    root = pathlib.Path("/sys/fs/cgroup")
    return root.joinpath(*pathlib.PurePosixPath(cgroup).parts[1:])


def open_cgroup(cgroup: str) -> CgroupHandle:
    target = cgroup_path(cgroup)
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_DIRECTORY", 0) | getattr(os, "O_NOFOLLOW", 0)
    descriptor = -1
    try:
        descriptor = os.open(target, flags)
        identity = os.fstat(descriptor)
    except OSError as error:
        if descriptor >= 0:
            os.close(descriptor)
        raise ControllerError("unit-cgroup-unavailable", exit_code=125) from error
    return CgroupHandle(cgroup, descriptor, identity.st_dev, identity.st_ino)


def read_cgroup_populated(handle: CgroupHandle) -> bool:
    try:
        identity = os.fstat(handle.descriptor)
        if (identity.st_dev, identity.st_ino) != (handle.device, handle.inode):
            raise ControllerError("unit-cgroup-identity-changed", exit_code=125)
        try:
            live_identity = os.stat(cgroup_path(handle.path), follow_symlinks=False)
        except FileNotFoundError:
            # cgroupfs only removes an empty leaf.  Because the directory was
            # opened and identity-bound before stop, removal is terminal proof.
            return False
        if (live_identity.st_dev, live_identity.st_ino) != (handle.device, handle.inode):
            raise ControllerError("unit-cgroup-identity-changed", exit_code=125)
        descriptor = os.open(
            "cgroup.events",
            os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0),
            dir_fd=handle.descriptor,
        )
        try:
            data = os.read(descriptor, 4096).decode("ascii")
        finally:
            os.close(descriptor)
    except ControllerError:
        raise
    except (OSError, UnicodeDecodeError) as error:
        raise ControllerError("cgroup-events-unreadable", exit_code=125) from error
    values = dict(line.split(maxsplit=1) for line in data.splitlines() if len(line.split(maxsplit=1)) == 2)
    if values.get("populated") not in {"0", "1"}:
        raise ControllerError("cgroup-events-invalid", exit_code=125)
    return values["populated"] == "1"


def read_cgroup_pids(handle: CgroupHandle) -> list[int]:
    pids: set[int] = set()
    for filename in ("cgroup.procs", "cgroup.threads"):
        try:
            descriptor = os.open(
                filename,
                os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0),
                dir_fd=handle.descriptor,
            )
            try:
                data = os.read(descriptor, 1024 * 1024).decode("ascii")
            finally:
                os.close(descriptor)
            for line in data.splitlines():
                if line.isdigit():
                    pids.add(int(line))
        except FileNotFoundError:
            continue
        except (OSError, UnicodeDecodeError) as error:
            raise ControllerError("cgroup-members-unreadable", exit_code=125) from error
    return sorted(pids)


def classify_pids(pids: Iterable[int]) -> MemberSummary:
    return MemberSummary.from_states(process_state(pid) for pid in pids)


def clean_hook_environment(request: Request) -> dict[str, str]:
    assert request.scope is not None and request.artifact_path is not None
    scope = request.scope
    return {
        "PATH": os.defpath,
        "LANG": "C.UTF-8",
        "LC_ALL": "C.UTF-8",
        "SYNARA_MANAGED_HOOK_ARTIFACT": request.artifact_path,
        "SYNARA_MANAGED_HOOK_OPERATION_ID": request.operation_id,
        "SYNARA_MANAGED_HOOK_PHASE": request.phase,
        "SYNARA_MANAGED_HOOK_CONTEXT": scope.context,
        "SYNARA_MANAGED_HOOK_NAMESPACE": scope.namespace,
        "SYNARA_MANAGED_HOOK_NODE": scope.node,
        "SYNARA_MANAGED_HOOK_NODE_UID": scope.node_uid,
        "SYNARA_MANAGED_HOOK_POD": scope.pod,
        "SYNARA_MANAGED_HOOK_POD_UID": scope.pod_uid,
        "SYNARA_MANAGED_HOOK_CHALLENGE": scope.challenge,
    }


def prepare_artifact_path(path_text: str) -> None:
    """Require a private runner-owned parent and a fresh artifact name."""
    path = pathlib.Path(path_text)
    parent = path.parent
    try:
        parent_stat = parent.stat()
        parent_lstat = parent.lstat()
    except OSError as error:
        raise ControllerError("artifact-parent-unavailable", exit_code=125) from error
    if (
        not stat.S_ISDIR(parent_stat.st_mode)
        or stat.S_ISLNK(parent_lstat.st_mode)
        or parent_stat.st_uid != os.geteuid()
        or parent_stat.st_mode & 0o022
    ):
        raise ControllerError("artifact-parent-not-private", exit_code=125)
    if os.path.lexists(path_text):
        raise ControllerError("artifact-path-not-fresh", exit_code=125)


class ProcessGroupTestBackend:
    security_boundary = False

    def __init__(self, cancellation: Cancellation) -> None:
        self.cancellation = cancellation

    def preflight(self) -> None:
        if not hasattr(os, "killpg"):
            raise UnsupportedError("process-groups-unavailable")

    def recover(self, request: Request) -> BackendOutcome:
        # There is intentionally no persistent PID authority for this test-only
        # backend.  Claiming recovery would be unsafe and misleading.
        return BackendOutcome(None, False, False, False, False, MemberSummary())

    def execute(self, request: Request) -> BackendOutcome:
        if self.cancellation.requested:
            return BackendOutcome(None, False, True, False, True, MemberSummary())
        env = clean_hook_environment(request)
        try:
            process = subprocess.Popen(
                list(request.command),
                stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                env=env,
                start_new_session=True,
                close_fds=True,
            )
        except OSError as error:
            raise ControllerError("hook-exec-failed", exit_code=125) from error
        identity = process_identity(process.pid)
        deadline = time.monotonic() + request.timeout_seconds
        timed_out = False
        while not child_exited_wnowait(process):
            if self.cancellation.requested:
                break
            if time.monotonic() >= deadline:
                timed_out = True
                break
            time.sleep(0.02)

        self._terminate_group(process, identity, request.stop_timeout_seconds)
        summary = self._group_summary(process.pid)
        termination_confirmed = summary.executable == 0 and summary.unreadable == 0
        hook_exit_code = reap_after_cleanup(process)
        return BackendOutcome(
            hook_exit_code,
            timed_out,
            self.cancellation.requested,
            True,
            termination_confirmed,
            summary,
        )

    @staticmethod
    def _group_summary(pgid: int) -> MemberSummary:
        if not sys.platform.startswith("linux"):
            return MemberSummary()
        pids: list[int] = []
        try:
            entries = os.listdir("/proc")
        except OSError:
            return MemberSummary(unreadable=1)
        for entry in entries:
            if not entry.isdigit():
                continue
            try:
                data = pathlib.Path(f"/proc/{entry}/stat").read_text(encoding="utf-8")
                close = data.rfind(")")
                fields = data[close + 2 :].split()
                if int(fields[2]) == pgid:  # state, ppid, pgrp
                    pids.append(int(entry))
            except (OSError, IndexError, UnicodeDecodeError, ValueError):
                continue
        return classify_pids(pids)

    def _terminate_group(self, process: subprocess.Popen[bytes], identity: str | None, timeout: int) -> None:
        if identity is not None and process_identity(process.pid) != identity:
            raise ControllerError("direct-child-identity-changed", exit_code=125)
        for signum, seconds in ((signal.SIGTERM, min(1.0, timeout)), (signal.SIGKILL, timeout)):
            summary = self._group_summary(process.pid)
            if child_exited_wnowait(process) and summary.executable == 0 and summary.unreadable == 0:
                return
            try:
                os.killpg(process.pid, signum)
            except ProcessLookupError:
                pass
            except PermissionError as error:
                raise ControllerError("process-group-signal-denied", exit_code=125) from error
            deadline = time.monotonic() + seconds
            while time.monotonic() < deadline:
                summary = self._group_summary(process.pid)
                if child_exited_wnowait(process) and summary.executable == 0 and summary.unreadable == 0:
                    return
                time.sleep(0.02)


class SystemdUserBackend:
    security_boundary = True

    def __init__(self, cancellation: Cancellation) -> None:
        self.cancellation = cancellation
        self.systemctl = shutil.which("systemctl")
        self.systemd_run = shutil.which("systemd-run")

    def preflight(self) -> None:
        if not sys.platform.startswith("linux"):
            raise UnsupportedError("systemd-backend-requires-linux")
        if self.systemctl is None or self.systemd_run is None:
            raise UnsupportedError("systemd-user-tools-unavailable")
        if not pathlib.Path("/sys/fs/cgroup/cgroup.controllers").is_file():
            raise UnsupportedError("cgroup-v2-unavailable")
        try:
            version = subprocess.run(
                [self.systemd_run, "--version"],
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=subprocess.DEVNULL,
                check=False,
                timeout=2,
                close_fds=True,
            )
            first_line = version.stdout.decode("ascii", "replace").splitlines()[0]
            version_number = int(first_line.split()[1])
        except (OSError, subprocess.TimeoutExpired, IndexError, ValueError) as error:
            raise UnsupportedError("systemd-version-unavailable") from error
        # ExitType= was introduced in systemd 250.  Reject older managers
        # before asking them to launch the hook.
        if version.returncode != 0 or version_number < 250:
            raise UnsupportedError("systemd-version-unsupported")
        try:
            completed = subprocess.run(
                [self.systemctl, "--user", "show-environment"],
                stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                check=False,
                timeout=3,
                close_fds=True,
            )
        except (OSError, subprocess.TimeoutExpired) as error:
            raise UnsupportedError("systemd-user-manager-unavailable") from error
        if completed.returncode != 0:
            raise UnsupportedError("systemd-user-manager-unavailable")

    def recover(self, request: Request) -> BackendOutcome:
        errors: list[BaseException] = []
        identity_unknown = False
        try:
            state = self._unit_identity(request.unit_name)
        except BaseException as error:
            state = UnitState(False)
            identity_unknown = True
            errors.append(error)
        handle: CgroupHandle | None = None
        if state.cgroup is not None:
            try:
                handle = open_cgroup(state.cgroup)
            except BaseException as error:
                errors.append(error)
        summary = MemberSummary(unreadable=1) if errors else MemberSummary()
        try:
            if state.found or identity_unknown:
                try:
                    self._stop_unit(request.unit_name, 5)
                except BaseException as error:
                    errors.append(error)
            if handle is not None:
                try:
                    summary = self._wait_cgroup_empty(handle, 5)
                except BaseException as error:
                    errors.append(error)
            try:
                self._reset_unit(request.unit_name)
                self._wait_unit_absent(request.unit_name, 5)
            except BaseException as error:
                errors.append(error)
        finally:
            if handle is not None:
                handle.close()
        if errors:
            raise ControllerError("systemd-recovery-unconfirmed", exit_code=125) from errors[0]
        confirmed = summary.executable == 0 and summary.unreadable == 0
        return BackendOutcome(None, False, False, state.found, confirmed, summary)

    def execute(self, request: Request) -> BackendOutcome:
        assert self.systemd_run is not None
        if self.cancellation.requested:
            return BackendOutcome(None, False, True, False, True, MemberSummary())
        environment = clean_hook_environment(request)
        command = [
            self.systemd_run,
            "--user",
            "--quiet",
            "--wait",
            "--collect",
            f"--unit={request.unit_name}",
            "--property=Type=exec",
            "--property=ExitType=cgroup",
            "--property=KillMode=control-group",
            "--property=SendSIGKILL=yes",
            f"--property=TimeoutStopSec={request.stop_timeout_seconds}s",
            f"--property=RuntimeMaxSec={request.timeout_seconds}s",
            "--property=UMask=0077",
            "--property=NoNewPrivileges=yes",
            "--property=StandardInput=null",
            "--property=StandardOutput=null",
            "--property=StandardError=null",
        ]
        for name, value in sorted(environment.items()):
            command.append(f"--setenv={name}={value}")
        command.extend(("--", *request.command))
        try:
            process = subprocess.Popen(
                command,
                stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                close_fds=True,
            )
        except OSError as error:
            raise ControllerError("systemd-run-exec-failed", exit_code=125) from error
        direct_identity = process_identity(process.pid)
        handle: CgroupHandle | None = None
        invocation_id: str | None = None
        unit_found = False
        unit_result: str | None = None
        timed_out = False
        primary_error: BaseException | None = None
        summary = MemberSummary(unreadable=1)
        hook_exit_code: int | None = None
        try:
            if direct_identity is None:
                raise ControllerError("systemd-run-child-identity-unavailable", exit_code=125)
            deadline = time.monotonic() + request.timeout_seconds
            while True:
                state = self._unit_identity(request.unit_name)
                unit_found = unit_found or state.found
                unit_result = state.result or unit_result
                handle, invocation_id = self._bind_unit_identity(state, handle, invocation_id)
                if child_exited_wnowait(process):
                    break
                if self.cancellation.requested:
                    break
                if time.monotonic() >= deadline:
                    timed_out = True
                    break
                time.sleep(0.025)
            if process_identity(process.pid) != direct_identity:
                raise ControllerError("systemd-run-child-identity-changed", exit_code=125)
            state = self._unit_identity(request.unit_name)
            unit_found = unit_found or state.found
            unit_result = state.result or unit_result
            handle, invocation_id = self._bind_unit_identity(state, handle, invocation_id)
            timed_out = timed_out or unit_result == "timeout"
            if not unit_found or handle is None or invocation_id is None:
                raise ControllerError("systemd-unit-identity-unavailable", exit_code=125)
        except BaseException as error:
            primary_error = error
        try:
            summary, hook_exit_code = self._cleanup_spawned_unit(
                request.unit_name,
                process,
                handle,
                request.stop_timeout_seconds,
            )
        except BaseException as cleanup_error:
            if primary_error is not None:
                raise ControllerError("systemd-cleanup-unconfirmed", exit_code=125) from cleanup_error
            raise
        finally:
            if handle is not None:
                handle.close()
        if primary_error is not None:
            if summary.executable != 0 or summary.unreadable != 0:
                raise ControllerError("systemd-cleanup-unconfirmed", exit_code=125) from primary_error
            raise primary_error
        termination_confirmed = summary.executable == 0 and summary.unreadable == 0
        return BackendOutcome(
            hook_exit_code,
            timed_out,
            self.cancellation.requested,
            unit_found,
            termination_confirmed,
            summary,
        )

    @staticmethod
    def _bind_unit_identity(
        state: UnitState,
        handle: CgroupHandle | None,
        invocation_id: str | None,
    ) -> tuple[CgroupHandle | None, str | None]:
        if state.invocation_id is not None:
            if invocation_id is not None and state.invocation_id != invocation_id:
                raise ControllerError("systemd-unit-invocation-changed", exit_code=125)
            invocation_id = state.invocation_id
        if state.cgroup is not None:
            if handle is None:
                handle = open_cgroup(state.cgroup)
            elif state.cgroup != handle.path:
                raise ControllerError("systemd-unit-cgroup-changed", exit_code=125)
        return handle, invocation_id

    def _cleanup_spawned_unit(
        self,
        unit: str,
        process: subprocess.Popen[bytes],
        handle: CgroupHandle | None,
        timeout: int,
    ) -> tuple[MemberSummary, int]:
        cleanup_errors: list[BaseException] = []
        try:
            self._stop_unit(unit, timeout)
        except BaseException as error:
            cleanup_errors.append(error)
        summary = MemberSummary(unreadable=1)
        if handle is not None:
            try:
                summary = self._wait_cgroup_empty(handle, timeout)
            except BaseException as error:
                cleanup_errors.append(error)
        else:
            cleanup_errors.append(ControllerError("systemd-unit-cgroup-never-bound", exit_code=125))
        hook_exit_code: int | None = None
        try:
            hook_exit_code = reap_after_cleanup(process)
        except BaseException as error:
            cleanup_errors.append(error)
            try:
                process.terminate()
                process.wait(timeout=2)
            except BaseException as terminate_error:
                cleanup_errors.append(terminate_error)
        try:
            self._reset_unit(unit)
            self._wait_unit_absent(unit, timeout)
        except BaseException as error:
            cleanup_errors.append(error)
        if cleanup_errors:
            raise ControllerError("systemd-cleanup-unconfirmed", exit_code=125) from cleanup_errors[0]
        assert hook_exit_code is not None
        return summary, hook_exit_code

    def _run_systemctl(self, args: list[str], timeout: int) -> subprocess.CompletedProcess[bytes]:
        assert self.systemctl is not None
        try:
            return subprocess.run(
                [self.systemctl, "--user", *args],
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=subprocess.DEVNULL,
                check=False,
                timeout=timeout,
                close_fds=True,
            )
        except (OSError, subprocess.TimeoutExpired) as error:
            raise ControllerError("systemctl-failed", exit_code=125) from error

    def _unit_identity(self, unit: str) -> UnitState:
        completed = self._run_systemctl(
            [
                "show",
                unit,
                "--property=LoadState",
                "--property=ControlGroup",
                "--property=Result",
                "--property=InvocationID",
            ],
            2,
        )
        if completed.returncode != 0:
            raise ControllerError("systemctl-show-failed", exit_code=125)
        values: dict[str, str] = {}
        for line in completed.stdout.decode("utf-8", "replace").splitlines():
            key, separator, value = line.partition("=")
            if separator:
                values[key] = value
        if values.get("LoadState") == "not-found":
            return UnitState(False)
        cgroup = values.get("ControlGroup") or None
        unit_result = values.get("Result") or None
        invocation_id = values.get("InvocationID") or None
        return UnitState(True, cgroup, unit_result, invocation_id)

    def _stop_unit(self, unit: str, timeout: int) -> None:
        try:
            completed = self._run_systemctl(["stop", unit], timeout + 2)
        except ControllerError:
            completed = None
        if completed is None or completed.returncode not in (0, 5):
            # Escalate only through systemd's exact unit ownership; never signal
            # numeric PIDs learned from mutable state.
            self._run_systemctl(["kill", "--kill-whom=all", "--signal=KILL", unit], 2)
        self._run_systemctl(["kill", "--kill-whom=all", "--signal=KILL", unit], 2)

    def _wait_cgroup_empty(self, handle: CgroupHandle, timeout: int) -> MemberSummary:
        deadline = time.monotonic() + timeout
        last = MemberSummary(unreadable=1)
        while time.monotonic() < deadline:
            populated = read_cgroup_populated(handle)
            last = classify_pids(read_cgroup_pids(handle))
            if not populated:
                return last if last.executable == 0 else MemberSummary(unreadable=1)
            if last.executable == 0 and last.unreadable == 0:
                # cgroup.events is recursive.  A populated cgroup with no
                # top-level members therefore has executable nested members.
                last = MemberSummary(executable=1)
            time.sleep(0.025)
        return last

    def _reset_unit(self, unit: str) -> None:
        self._run_systemctl(["reset-failed", unit], 2)

    def _wait_unit_absent(self, unit: str, timeout: int) -> None:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if not self._unit_identity(unit).found:
                return
            time.sleep(0.025)
        raise ControllerError("systemd-unit-not-collected", exit_code=125)


def read_artifact_snapshot(path_text: str) -> tuple[dict[str, Any], str]:
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0)
    try:
        descriptor = os.open(path_text, flags)
    except OSError as error:
        raise ControllerError("transition-artifact-unavailable") from error
    try:
        before = os.fstat(descriptor)
        if (
            not stat.S_ISREG(before.st_mode)
            or before.st_nlink != 1
            or before.st_uid != os.geteuid()
            or before.st_size > MAX_ARTIFACT_BYTES
        ):
            raise ControllerError("invalid-transition-artifact-file")
        chunks: list[bytes] = []
        remaining = before.st_size + 1
        while remaining:
            chunk = os.read(descriptor, min(remaining, 8192))
            if not chunk:
                break
            chunks.append(chunk)
            remaining -= len(chunk)
        payload = b"".join(chunks)
        after = os.fstat(descriptor)
        try:
            current = os.lstat(path_text)
        except OSError as error:
            raise ControllerError("transition-artifact-replaced") from error
        identity_before = (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns)
        identity_after = (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns)
        if (
            len(payload) != before.st_size
            or identity_before != identity_after
            or (after.st_dev, after.st_ino) != (current.st_dev, current.st_ino)
            or not stat.S_ISREG(current.st_mode)
        ):
            raise ControllerError("transition-artifact-changed")
        parsed = strict_json_loads(payload, "invalid-transition-artifact-json")
        return require_object(parsed, "transition-artifact"), hashlib.sha256(payload).hexdigest()
    finally:
        os.close(descriptor)


def parse_observed_at(value: Any) -> dt.datetime:
    if not isinstance(value, str) or len(value) > 40:
        raise ControllerError("invalid-transition-observed-at")
    normalized = value[:-1] + "+00:00" if value.endswith("Z") else value
    try:
        observed = dt.datetime.fromisoformat(normalized)
    except ValueError as error:
        raise ControllerError("invalid-transition-observed-at") from error
    if observed.tzinfo is None:
        raise ControllerError("invalid-transition-observed-at")
    return observed.astimezone(dt.timezone.utc)


def validate_artifact(request: Request) -> tuple[str, tuple[str, ...], int]:
    assert request.scope is not None and request.artifact_path is not None
    artifact, artifact_digest = read_artifact_snapshot(request.artifact_path)
    allowed = {
        "schemaVersion",
        "operationId",
        "phase",
        "transition",
        "context",
        "namespace",
        "node",
        "nodeUid",
        "pod",
        "podUid",
        "challenge",
        "observedAt",
        "checks",
    }
    require_exact_keys(artifact, allowed, allowed, "transition-artifact")
    expected = {
        "schemaVersion": ARTIFACT_SCHEMA,
        "operationId": request.operation_id,
        "phase": request.phase,
        "transition": PHASE_TRANSITIONS[request.phase],
        **request.scope.artifact_fields(),
    }
    for key, expected_value in expected.items():
        if artifact.get(key) != expected_value:
            raise ControllerError("transition-artifact-scope-mismatch")
    observed = parse_observed_at(artifact["observedAt"])
    now = dt.datetime.now(dt.timezone.utc)
    age = (now - observed).total_seconds()
    if age > request.freshness_seconds or age < -5:
        raise ControllerError("transition-artifact-stale")
    checks = artifact["checks"]
    if not isinstance(checks, list) or not checks or len(checks) > 64:
        raise ControllerError("invalid-transition-checks")
    evidence_digests: list[str] = []
    for raw_check in checks:
        check = require_object(raw_check, "transition-check")
        keys = {"name", "status", "evidenceDigest"}
        require_exact_keys(check, keys, keys, "transition-check")
        require_string(check["name"], IDENTIFIER, "transition-check-name")
        if check["status"] != "passed":
            raise ControllerError("transition-check-failed")
        evidence_digests.append(require_string(check["evidenceDigest"], DIGEST, "evidence-digest"))
    return artifact_digest, tuple(evidence_digests), len(checks)


def sanitized_result(
    request: Request | None,
    *,
    status: str,
    reason: str,
    outcome: BackendOutcome | None = None,
    artifact_digest: str | None = None,
    evidence_digests: tuple[str, ...] = (),
    check_count: int = 0,
) -> dict[str, Any]:
    result: dict[str, Any] = {
        "schemaVersion": RESULT_SCHEMA,
        "status": status,
        "reason": reason,
        "securityBoundary": bool(request is not None and request.security_boundary),
    }
    if request is not None:
        result.update(
            {
                "operationIdDigest": hashlib.sha256(request.operation_id.encode("utf-8")).hexdigest(),
                "phase": request.phase,
                "backend": request.backend,
                "unitNameDigest": hashlib.sha256(request.unit_name.encode("ascii")).hexdigest(),
                "scopeDigest": request.scope_digest,
                "expectedTransition": PHASE_TRANSITIONS[request.phase] if request.action == "execute" else None,
            }
        )
    if outcome is not None:
        result.update(
            {
                "hookExitCode": outcome.hook_exit_code,
                "timedOut": outcome.timed_out,
                "cancelled": outcome.cancelled,
                "unitFound": outcome.unit_found,
                "terminationConfirmed": outcome.termination_confirmed,
                "members": outcome.members.sanitized(),
            }
        )
    if artifact_digest is not None:
        result.update(
            {
                "artifactDigest": artifact_digest,
                "evidenceDigests": list(evidence_digests),
                "checkCount": check_count,
            }
        )
    return result


def load_request() -> Request:
    raw = sys.stdin.buffer.read(MAX_REQUEST_BYTES + 1)
    if len(raw) > MAX_REQUEST_BYTES:
        raise ControllerError("request-too-large", exit_code=125)
    parsed = strict_json_loads(raw, "invalid-request-json", exit_code=125)
    return Request.parse(parsed)


def run(request: Request, cancellation: Cancellation) -> tuple[dict[str, Any], int]:
    backend: SystemdUserBackend | ProcessGroupTestBackend
    if request.backend == "systemd-user":
        backend = SystemdUserBackend(cancellation)
    else:
        backend = ProcessGroupTestBackend(cancellation)
    # Unsupported environments must be rejected before hook execution.
    backend.preflight()
    if request.action == "recover":
        outcome = backend.recover(request)
        if not outcome.termination_confirmed:
            return sanitized_result(request, status="failed", reason="recovery-unconfirmed", outcome=outcome), 1
        return sanitized_result(request, status="recovered", reason="unit-terminal", outcome=outcome), 0

    assert request.artifact_path is not None
    if cancellation.requested:
        outcome = BackendOutcome(None, False, True, False, True, MemberSummary())
        return sanitized_result(request, status="cancelled", reason="signal-cancelled", outcome=outcome), 128 + int(cancellation.signal_number or signal.SIGTERM)
    prepare_artifact_path(request.artifact_path)
    if cancellation.requested:
        outcome = BackendOutcome(None, False, True, False, True, MemberSummary())
        return sanitized_result(request, status="cancelled", reason="signal-cancelled", outcome=outcome), 128 + int(cancellation.signal_number or signal.SIGTERM)

    outcome = backend.execute(request)
    if not outcome.termination_confirmed:
        return sanitized_result(request, status="failed", reason="containment-not-empty", outcome=outcome), 1
    if outcome.cancelled:
        return sanitized_result(request, status="cancelled", reason="signal-cancelled", outcome=outcome), 128 + int(cancellation.signal_number or signal.SIGTERM)
    if outcome.timed_out:
        return sanitized_result(request, status="failed", reason="hook-timed-out", outcome=outcome), 1
    if outcome.hook_exit_code != 0:
        return sanitized_result(request, status="failed", reason="hook-exit-nonzero", outcome=outcome), 1
    try:
        artifact_digest, evidence_digests, check_count = validate_artifact(request)
    except ControllerError as error:
        return sanitized_result(request, status="failed", reason=error.code, outcome=outcome), 1
    return (
        sanitized_result(
            request,
            status="passed",
            reason="transition-proved",
            outcome=outcome,
            artifact_digest=artifact_digest,
            evidence_digests=evidence_digests,
            check_count=check_count,
        ),
        0,
    )


def main() -> int:
    parser = argparse.ArgumentParser(add_help=False)
    parser.add_argument("--help", action="store_true")
    arguments, extras = parser.parse_known_args()
    if arguments.help:
        sys.stdout.write(f"{REQUEST_SCHEMA} on stdin; {RESULT_SCHEMA} on stdout\n")
        return 0
    if extras:
        payload = sanitized_result(None, status="unsupported", reason="unexpected-argv")
        sys.stdout.write(json.dumps(payload, sort_keys=True, separators=(",", ":")) + "\n")
        return 125
    set_not_dumpable()
    cancellation = Cancellation()
    cancellation.install()
    request: Request | None = None
    try:
        request = load_request()
        payload, exit_code = run(request, cancellation)
    except ControllerError as error:
        status = "unsupported" if error.exit_code == 125 else "failed"
        payload = sanitized_result(request, status=status, reason=error.code)
        exit_code = error.exit_code
    except Exception:
        payload = sanitized_result(request, status="failed", reason="internal-controller-error")
        exit_code = 125
    sys.stdout.write(json.dumps(payload, sort_keys=True, separators=(",", ":")) + "\n")
    return exit_code


if __name__ == "__main__":
    raise SystemExit(main())
