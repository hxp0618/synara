#!/usr/bin/env python3
"""Run the protected-cgroup v2 supervisor against real systemd and cgroup2."""

from __future__ import annotations

import argparse
import contextlib
import dataclasses
import datetime as dt
import fcntl
import hashlib
import json
import os
import pathlib
import re
import secrets
import stat
import subprocess
import sys
import tempfile
import time
from collections.abc import Mapping, Sequence
from typing import Any


SCHEMA_VERSION = "synara.protected-cgroup-v2-live-gate.v1"
TEST_EVIDENCE_PREFIX = "SYNARA_CGROUP_V2_LIVE_EVIDENCE="
VM_PREFIX = "synara-cgroup-v2-live-"
DISPOSABLE_VM_USER = "bin"
UNIT_TOKEN_PATTERN = re.compile(r"^[a-z0-9]{8,32}$")
MAX_CAPTURE_BYTES = 48 * 1024
CREATION_MARKER_PATH = "/etc/synara-cgroup-v2-live-owner"
LATE_CREATE_RECONCILIATION_SECONDS = 20.0


@dataclasses.dataclass(frozen=True)
class Options:
    orbctl: str
    vm_name: str
    output: pathlib.Path
    timeout: int


class GateFailure(RuntimeError):
    pass


@dataclasses.dataclass(frozen=True)
class LateCreationReconciliation:
    identity: dict[str, Any] | None
    last_candidate: dict[str, Any] | None
    attempts: int
    last_error: str | None


def parse_args(argv: Sequence[str]) -> Options:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--allow-create-disposable-vm", action="store_true")
    parser.add_argument("--orbctl", default="orbctl")
    parser.add_argument("--vm-name")
    parser.add_argument("--output", type=pathlib.Path)
    parser.add_argument("--timeout", type=int, default=210)
    parsed = parser.parse_args(argv)
    if not parsed.allow_create_disposable_vm:
        parser.error("--allow-create-disposable-vm is required")
    now = dt.datetime.now(dt.UTC)
    token = now.strftime("%Y%m%d%H%M%S") + f"{os.getpid():x}"
    vm_name = parsed.vm_name or VM_PREFIX + token
    validate_vm_name(vm_name)
    if parsed.timeout < 60 or parsed.timeout > 600:
        parser.error("--timeout must be between 60 and 600 seconds")
    output = parsed.output or pathlib.Path(".tmp/stage3-protected-cgroup-live") / f"{token}.json"
    return Options(
        orbctl=parsed.orbctl,
        vm_name=vm_name,
        output=output.expanduser().resolve(),
        timeout=parsed.timeout,
    )


def validate_vm_name(name: str) -> None:
    if not name.startswith(VM_PREFIX):
        raise ValueError(f"VM name must start with {VM_PREFIX}")
    token = name.removeprefix(VM_PREFIX)
    if not UNIT_TOKEN_PATTERN.fullmatch(token):
        raise ValueError("VM name suffix must be 8-32 lowercase ASCII letters or digits")


def run_command(
    command: Sequence[str],
    *,
    cwd: pathlib.Path | None = None,
    env: Mapping[str, str] | None = None,
    timeout: int = 60,
    check: bool = True,
) -> subprocess.CompletedProcess[str]:
    result = subprocess.run(
        list(command),
        cwd=cwd,
        env=None if env is None else dict(env),
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        timeout=timeout,
        check=False,
    )
    if check and result.returncode != 0:
        raise GateFailure(
            f"command failed ({result.returncode}): {command[0]}: {bounded(result.stdout)}"
        )
    return result


def bounded(value: str, limit: int = MAX_CAPTURE_BYTES) -> str:
    encoded = value.encode("utf-8", errors="replace")
    if len(encoded) <= limit:
        return value
    return encoded[:limit].decode("utf-8", errors="replace") + "\n[output truncated]\n"


def load_inventory(orbctl: str) -> list[dict[str, Any]]:
    result = run_command([orbctl, "list", "--format", "json"], timeout=30)
    payload = json.loads(result.stdout)
    if not isinstance(payload, list) or not all(isinstance(item, dict) for item in payload):
        raise GateFailure("orbctl inventory was not a JSON object list")
    return payload


def inventory_machine(inventory: Sequence[Mapping[str, Any]], name: str) -> dict[str, Any] | None:
    for item in inventory:
        if item.get("name") == name:
            return dict(item)
    return None


def inventory_machine_by_id(inventory: Sequence[Mapping[str, Any]], machine_id: str) -> dict[str, Any] | None:
    for item in inventory:
        if item.get("id") == machine_id:
            return dict(item)
    return None


def stable_machine_identity(machine: Mapping[str, Any] | None) -> dict[str, Any] | None:
    if machine is None:
        return None
    image = machine.get("image") if isinstance(machine.get("image"), Mapping) else {}
    return {
        "id": machine.get("id"),
        "name": machine.get("name"),
        "state": machine.get("state"),
        "distro": image.get("distro"),
        "version": image.get("version"),
        "architecture": image.get("arch"),
    }


def validate_inventory_preflight(inventory: Sequence[Mapping[str, Any]], vm_name: str) -> dict[str, Any]:
    debian = stable_machine_identity(inventory_machine(inventory, "debian"))
    if debian is None:
        raise GateFailure("pre-existing debian VM was not present; refusing to run without the preservation oracle")
    if inventory_machine(inventory, vm_name) is not None:
        raise GateFailure(f"disposable VM already exists: {vm_name}")
    return debian


def validate_created_machine(machine: Mapping[str, Any] | None, vm_name: str) -> dict[str, Any]:
    identity = stable_machine_identity(machine)
    if (
        identity is None
        or identity["name"] != vm_name
        or not isinstance(identity["id"], str)
        or not re.fullmatch(r"[A-Z0-9]{10,64}", identity["id"])
        or identity["distro"] != "ubuntu"
        or identity["version"] != "noble"
        or identity["architecture"] != "arm64"
    ):
        raise GateFailure(f"created VM identity is unexpected: {identity}")
    return identity


def creation_marker_cloud_config(token: str) -> str:
    if not re.fullmatch(r"[0-9a-f]{64}", token):
        raise ValueError("creation marker token must be 32 random bytes in lowercase hexadecimal")
    return "\n".join(
        [
            "#cloud-config",
            "write_files:",
            f"  - path: {CREATION_MARKER_PATH}",
            "    owner: root:root",
            "    permissions: '0600'",
            f"    content: '{token}'",
            "",
        ]
    )


def verify_creation_marker(options: Options, expected_sha256: str) -> None:
    result = remote_run(
        options,
        "/bin/sh",
        "-c",
        f"test -f {CREATION_MARKER_PATH} && stat -c '%u:%g:%a' {CREATION_MARKER_PATH} && sha256sum {CREATION_MARKER_PATH} | cut -d' ' -f1",
        timeout=30,
    )
    lines = result.stdout.splitlines()
    if lines != ["0:0:600", expected_sha256]:
        raise GateFailure("created VM did not present the exact root-owned run creation marker")


def prove_created_vm_ownership(options: Options, expected_marker_sha256: str) -> dict[str, Any]:
    before_marker = load_inventory(options.orbctl)
    first_identity = validate_created_machine(
        inventory_machine(before_marker, options.vm_name),
        options.vm_name,
    )
    verify_creation_marker(options, expected_marker_sha256)
    after_marker = load_inventory(options.orbctl)
    second_identity = validate_created_machine(
        inventory_machine(after_marker, options.vm_name),
        options.vm_name,
    )
    if first_identity["id"] != second_identity["id"]:
        raise GateFailure("created VM identity changed across creation-marker verification")
    return second_identity


def observe_unproven_candidate(options: Options) -> dict[str, Any] | None:
    try:
        inventory = load_inventory(options.orbctl)
    except Exception:
        return None
    machine = inventory_machine(inventory, options.vm_name)
    if machine is None:
        return None
    return stable_machine_identity(machine)


def reconcile_late_creation(
    options: Options,
    expected_marker_sha256: str,
    *,
    timeout: float = LATE_CREATE_RECONCILIATION_SECONDS,
    clock: Any = time.monotonic,
    sleeper: Any = time.sleep,
    inventory_loader: Any | None = None,
    marker_verifier: Any | None = None,
) -> LateCreationReconciliation:
    if timeout <= 0:
        raise ValueError("late-create reconciliation timeout must be positive")
    inventory_loader = inventory_loader or (lambda: load_inventory(options.orbctl))
    marker_verifier = marker_verifier or (lambda: verify_creation_marker(options, expected_marker_sha256))
    deadline = clock() + timeout
    delay = 0.1
    attempts = 0
    last_candidate: dict[str, Any] | None = None
    last_error: str | None = None
    while True:
        attempts += 1
        try:
            before_marker = inventory_loader()
            candidate = inventory_machine(before_marker, options.vm_name)
            if candidate is None:
                last_error = "same-name candidate not present"
            else:
                last_candidate = stable_machine_identity(candidate)
                first_identity = validate_created_machine(candidate, options.vm_name)
                marker_verifier()
                after_marker = inventory_loader()
                second_identity = validate_created_machine(
                    inventory_machine(after_marker, options.vm_name),
                    options.vm_name,
                )
                if first_identity["id"] != second_identity["id"]:
                    raise GateFailure("candidate identity changed across late marker verification")
                return LateCreationReconciliation(
                    identity=second_identity,
                    last_candidate=second_identity,
                    attempts=attempts,
                    last_error=None,
                )
        except Exception as error:
            last_error = bounded(str(error), 1024)
        remaining = deadline - clock()
        if remaining <= 0:
            return LateCreationReconciliation(
                identity=None,
                last_candidate=last_candidate,
                attempts=attempts,
                last_error=last_error,
            )
        sleeper(min(delay, remaining))
        delay = min(delay * 2, 2.0)


@contextlib.contextmanager
def acquire_vm_name_lock(vm_name: str, lock_directory: pathlib.Path | None = None) -> Any:
    if lock_directory is None:
        lock_directory = pathlib.Path(tempfile.gettempdir()) / f"synara-cgroup-v2-live-locks-{os.getuid()}"
    try:
        os.mkdir(lock_directory, 0o700)
    except FileExistsError:
        pass
    try:
        directory_fd = os.open(
            lock_directory,
            os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC | os.O_NOFOLLOW,
        )
    except OSError as error:
        raise GateFailure("open private VM name lock directory without following symlinks") from error
    try:
        directory_stats = os.fstat(directory_fd)
        if (
            not stat.S_ISDIR(directory_stats.st_mode)
            or directory_stats.st_uid != os.getuid()
            or stat.S_IMODE(directory_stats.st_mode) & 0o077 != 0
        ):
            raise GateFailure("VM name lock directory must be a private owner-controlled directory")
        try:
            lock_fd = os.open(
                f"{vm_name}.lock",
                os.O_RDWR | os.O_CREAT | os.O_CLOEXEC | os.O_NOFOLLOW,
                0o600,
                dir_fd=directory_fd,
            )
        except OSError as error:
            raise GateFailure("open private regular VM name lock without following symlinks") from error
    finally:
        os.close(directory_fd)
    try:
        lock_stats = os.fstat(lock_fd)
        if (
            not stat.S_ISREG(lock_stats.st_mode)
            or lock_stats.st_uid != os.getuid()
            or stat.S_IMODE(lock_stats.st_mode) & 0o077 != 0
        ):
            raise GateFailure("VM name lock must be a private owner-controlled regular file")
        try:
            fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as error:
            raise GateFailure(f"another live gate owns the VM name lock: {vm_name}") from error
        yield lock_fd
    finally:
        try:
            fcntl.flock(lock_fd, fcntl.LOCK_UN)
        finally:
            os.close(lock_fd)


def initial_evidence_boundary() -> dict[str, Any]:
    return {
        "containmentScenariosProved": False,
        "hostPrerequisitesObserved": [],
        "notProved": "real Codex or Claude Provider execution, cloud target, signed Control Plane projection, deployment, or release",
    }


def repository_root() -> pathlib.Path:
    return pathlib.Path(__file__).resolve().parents[2]


def source_manifest(repo: pathlib.Path) -> tuple[str, list[dict[str, Any]]]:
    control_plane = repo / "services/control-plane"
    sources = sorted((control_plane / "internal/agentd").glob("*.go"))
    sources.extend([control_plane / "go.mod", control_plane / "go.sum", pathlib.Path(__file__).resolve()])
    aggregate = hashlib.sha256()
    entries: list[dict[str, Any]] = []
    for path in sources:
        data = path.read_bytes()
        relative = path.relative_to(repo).as_posix()
        digest = hashlib.sha256(data).hexdigest()
        aggregate.update(relative.encode("utf-8") + b"\0" + data + b"\0")
        entries.append({"path": relative, "sha256": digest, "bytes": len(data)})
    return aggregate.hexdigest(), entries


def file_sha256(path: pathlib.Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def parse_test_evidence(output: str) -> dict[str, Any]:
    matches = []
    for line in output.splitlines():
        marker = line.find(TEST_EVIDENCE_PREFIX)
        if marker >= 0:
            matches.append(line[marker + len(TEST_EVIDENCE_PREFIX) :])
    if len(matches) != 1:
        raise GateFailure(f"expected one live test evidence record, found {len(matches)}")
    payload = json.loads(matches[0])
    if not isinstance(payload, dict) or payload.get("schemaVersion") != "synara.protected-cgroup-v2-live-test.v1":
        raise GateFailure("live test evidence schema is invalid")
    scenarios = payload.get("scenarios")
    expected = {
        "runtimeDiagnosticOverlapAndFence",
        "parentExtraPidZeroMutation",
        "legacyV1ZeroMutation",
        "unknownChildRepairRecovery",
        "sigkillHolderOrphanRecovery",
    }
    if not isinstance(scenarios, dict) or set(scenarios) != expected:
        raise GateFailure("live test evidence scenario set is incomplete")
    if any(not isinstance(value, dict) or value.get("status") != "pass" for value in scenarios.values()):
        raise GateFailure("one or more live test scenarios did not pass")
    return payload


def remote_run(options: Options, *arguments: str, timeout: int = 60, check: bool = True) -> subprocess.CompletedProcess[str]:
    return run_command(
        [options.orbctl, "run", "--machine", options.vm_name, "--user", "root", *arguments],
        timeout=timeout,
        check=check,
    )


def stream_binary_to_remote(
    options: Options,
    binary: pathlib.Path,
    remote_directory: str,
    remote_binary: str,
    expected_sha256: str,
) -> None:
    payload = binary.read_bytes()
    upload_path = remote_binary + ".upload"
    script = "\n".join(
        [
            "set -eu",
            f"install -d -o root -g root -m 0755 {remote_directory}",
            "umask 077",
            f"cat > {upload_path}",
            f"test \"$(stat -c %s {upload_path})\" = {len(payload)}",
            f"printf '%s  %s\\n' {expected_sha256} {upload_path} | sha256sum -c -",
            f"install -o root -g root -m 0755 {upload_path} {remote_binary}",
            f"rm -f {upload_path}",
        ]
    )
    result = subprocess.run(
        [options.orbctl, "run", "--machine", options.vm_name, "--user", "root", "/bin/sh", "-c", script],
        input=payload,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        timeout=90,
        check=False,
    )
    output = result.stdout.decode("utf-8", errors="replace")
    if result.returncode != 0:
        raise GateFailure(f"binary stdin transfer failed ({result.returncode}): {bounded(output)}")


def collect_host_facts(options: Options) -> dict[str, str]:
    script = "\n".join(
        [
            "set -eu",
            "printf 'kernel=%s\\n' \"$(uname -srmo)\"",
            "printf 'architecture=%s\\n' \"$(uname -m)\"",
            "printf 'systemd=%s\\n' \"$(systemctl --version | head -n 1)\"",
            "printf 'cgroupFilesystem=%s\\n' \"$(stat -fc %T /sys/fs/cgroup)\"",
            "printf 'cgroupMode=%s\\n' \"$(test -f /sys/fs/cgroup/cgroup.controllers && printf unified)\"",
        ]
    )
    result = remote_run(options, "/bin/sh", "-c", script, timeout=30)
    facts: dict[str, str] = {}
    for line in result.stdout.splitlines():
        key, separator, value = line.partition("=")
        if separator:
            facts[key] = value
    required = {"kernel", "architecture", "systemd", "cgroupFilesystem", "cgroupMode"}
    if set(facts) != required or facts["architecture"] != "aarch64" or facts["cgroupFilesystem"] != "cgroup2fs" or facts["cgroupMode"] != "unified":
        raise GateFailure(f"live host prerequisites failed: {facts}")
    return facts


def execute_live_test(options: Options, remote_binary: str, token: str) -> tuple[dict[str, Any], str, dict[str, str]]:
    main_unit = f"synara-cgroup-v2-live-{token}.service"
    helper_unit = f"synara-cgroup-v2-live-{token}-helper.service"
    state_dir = f"/run/synara-cgroup-v2-live-{token}"
    cgroup_root = f"/sys/fs/cgroup/system.slice/{main_unit}"
    command = [
        "/usr/bin/timeout",
        "--signal=KILL",
        f"{options.timeout}s",
        "/usr/bin/systemd-run",
        f"--unit={main_unit}",
        "--wait",
        "--pipe",
        "--property=Type=exec",
        "--property=Delegate=yes",
        "--property=KillMode=process",
        "--property=Restart=no",
        f"--property=TimeoutStartSec={options.timeout}s",
        f"--setenv=SYNARA_CGROUP_V2_LIVE_ROOT={cgroup_root}",
        f"--setenv=SYNARA_CGROUP_V2_LIVE_BINARY={remote_binary}",
        f"--setenv=SYNARA_CGROUP_V2_LIVE_STATE_DIR={state_dir}",
        f"--setenv=SYNARA_CGROUP_V2_LIVE_HELPER_UNIT={helper_unit}",
        "--setenv=SYNARA_CGROUP_V2_LIVE=1",
        remote_binary,
        "-test.run=^TestProtectedCgroupV2LiveIntegration$",
        "-test.v",
        f"-test.timeout={options.timeout - 15}s",
    ]
    result = remote_run(options, *command, timeout=options.timeout + 30, check=False)
    cleanup = "\n".join(
        [
            "set +e",
            f"systemctl kill --kill-who=all --signal=KILL {helper_unit} {main_unit} >/dev/null 2>&1",
            f"systemctl stop {helper_unit} {main_unit} >/dev/null 2>&1",
            f"systemctl reset-failed {helper_unit} {main_unit} >/dev/null 2>&1",
            f"rm -f /run/systemd/system/{helper_unit}",
            "systemctl daemon-reload >/dev/null 2>&1",
            f"rm -rf -- {state_dir}",
        ]
    )
    remote_run(options, "/bin/sh", "-c", cleanup, timeout=20, check=False)
    if result.returncode != 0:
        raise GateFailure(f"live systemd test failed ({result.returncode}): {bounded(result.stdout)}")
    evidence = parse_test_evidence(result.stdout)
    service_facts = evidence.get("systemd")
    if service_facts != {
        "unit": main_unit,
        "delegate": "yes",
        "killMode": "process",
        "controlGroup": f"/system.slice/{main_unit}",
        "activeState": "active",
        "subState": "running",
    }:
        raise GateFailure(f"main service properties were not exact: {service_facts}")
    return evidence, bounded(result.stdout), service_facts


def write_report(path: pathlib.Path, report: Mapping[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(json.dumps(report, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    temporary.replace(path)


def reserve_fallback_evidence(path: pathlib.Path) -> tuple[int, pathlib.Path]:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, fallback_name = tempfile.mkstemp(
        prefix=path.name + ".fallback-",
        suffix=".json",
        dir=path.parent,
    )
    stats = os.fstat(descriptor)
    if not stat.S_ISREG(stats.st_mode) or stats.st_uid != os.getuid() or stat.S_IMODE(stats.st_mode) & 0o077 != 0:
        os.close(descriptor)
        raise GateFailure("fallback evidence channel is not a private owner-controlled regular file")
    return descriptor, pathlib.Path(fallback_name)


def write_fallback_evidence(descriptor: int, report: Mapping[str, Any]) -> None:
    payload = (json.dumps(report, indent=2, sort_keys=True) + "\n").encode("utf-8")
    os.ftruncate(descriptor, 0)
    os.lseek(descriptor, 0, os.SEEK_SET)
    offset = 0
    while offset < len(payload):
        offset += os.write(descriptor, payload[offset:])
    os.fsync(descriptor)


def cleanup_proven_owned_vm(
    options: Options,
    report: dict[str, Any],
    owned_vm_id: str | None,
    debian_before: Mapping[str, Any] | None,
) -> list[str]:
    cleanup_errors: list[str] = []
    cleanup_observations: list[str] = []
    should_delete = owned_vm_id is not None
    if owned_vm_id is not None:
        try:
            inventory = load_inventory(options.orbctl)
        except Exception as error:
            cleanup_observations.append(f"pre-delete inventory unavailable; deleting captured exact ID: {bounded(str(error), 1024)}")
        else:
            by_id = inventory_machine_by_id(inventory, owned_vm_id)
            by_name = inventory_machine(inventory, options.vm_name)
            if by_id is None:
                should_delete = False
                report["vm"]["deleted"] = True
                cleanup_observations.append("captured exact VM ID was already absent before delete")
            elif by_id.get("name") != options.vm_name or (
                by_name is not None and by_name.get("id") != owned_vm_id
            ):
                should_delete = False
                cleanup_errors.append("pre-delete inventory did not bind the captured VM ID to the owned name")
    if should_delete and owned_vm_id is not None:
        report["vm"]["deleteAttempted"] = True
        report["vm"]["deleteMode"] = "opaque-id"
        try:
            result = run_command(
                [options.orbctl, "delete", "--force", owned_vm_id],
                timeout=120,
                check=False,
            )
        except Exception as error:
            cleanup_errors.append(f"delete captured VM ID failed: {bounded(str(error), 1024)}")
        else:
            if result.returncode != 0:
                report["vm"]["idDeleteFailure"] = {
                    "returnCode": result.returncode,
                    "outputSha256": hashlib.sha256(result.stdout.encode("utf-8", errors="replace")).hexdigest(),
                    "observation": bounded(result.stdout, 1024),
                }
                report["vm"]["deleteMode"] = "opaque-id-failed-no-name-fallback"
                report["vm"]["manualCleanupRequired"] = {
                    "required": True,
                    "capturedId": owned_vm_id,
                    "capturedName": options.vm_name,
                    "reason": (
                        "OrbStack 2.2.1 opaque-ID delete failed; automatic name deletion is forbidden because "
                        "identity cannot be held atomically across a name-delete TOCTOU window"
                    ),
                }
                cleanup_errors.append(
                    "opaque-ID delete failed; manual operator cleanup requires fresh identity and marker verification"
                )

    try:
        final_inventory = load_inventory(options.orbctl)
    except Exception as error:
        cleanup_errors.append(f"final inventory unavailable: {bounded(str(error), 1024)}")
    else:
        if owned_vm_id is not None:
            report["vm"]["deleted"] = inventory_machine_by_id(final_inventory, owned_vm_id) is None
            if not report["vm"]["deleted"]:
                cleanup_errors.append("captured exact VM ID remained after cleanup")
        else:
            candidate = stable_machine_identity(inventory_machine(final_inventory, options.vm_name))
            if candidate is not None:
                report["vm"]["unprovenCandidate"] = candidate
                report["vm"]["orphanCleanup"] = "not attempted because run ownership was not proved"
                cleanup_errors.append("unproven same-name candidate remains and was not deleted")
        debian_after = stable_machine_identity(inventory_machine(final_inventory, "debian"))
        report["preservedVm"]["after"] = debian_after
        report["preservedVm"]["unchanged"] = (
            debian_before is not None and dict(debian_before) == debian_after
        )
        if debian_before is not None and not report["preservedVm"]["unchanged"]:
            cleanup_errors.append("debian preservation oracle changed")
    if cleanup_observations:
        report["cleanupObservations"] = cleanup_observations
    return cleanup_errors


def run_gate(options: Options) -> dict[str, Any]:
    started = dt.datetime.now(dt.UTC)
    repo = repository_root()
    report: dict[str, Any] = {
        "schemaVersion": SCHEMA_VERSION,
        "status": "fail",
        "startedAt": started.isoformat(),
        "vm": {"name": options.vm_name, "ownedByThisRun": False, "deleted": None},
        "preservedVm": {"before": None, "after": None, "unchanged": False},
        "evidenceBoundary": initial_evidence_boundary(),
    }
    primary_error: Exception | None = None
    owned_vm_id: str | None = None
    debian_before: dict[str, Any] | None = None
    cleanup_errors: list[str] = []
    late_creation_marker_sha256: str | None = None
    late_creation_reconciliation_required = False
    unresolved_late_create_error: str | None = None
    fallback_descriptor: int | None = None
    fallback_path: pathlib.Path | None = None
    lock_context = acquire_vm_name_lock(options.vm_name)
    lock_held = False
    try:
        fallback_descriptor, fallback_path = reserve_fallback_evidence(options.output)
        lock_context.__enter__()
        lock_held = True
        baseline = load_inventory(options.orbctl)
        debian_before = validate_inventory_preflight(baseline, options.vm_name)
        report["preservedVm"]["before"] = debian_before
        source_sha, source_entries = source_manifest(repo)
        git_head = run_command(["git", "rev-parse", "HEAD"], cwd=repo).stdout.strip()
        dirty = bool(run_command(["git", "status", "--porcelain"], cwd=repo).stdout.strip())
        report["repository"] = {"gitHead": git_head, "dirty": dirty}
        report["source"] = {"sha256": source_sha, "files": source_entries}
        creation_token = secrets.token_hex(32)
        marker_sha256 = hashlib.sha256(creation_token.encode("ascii")).hexdigest()
        user_data_fd, user_data_name = tempfile.mkstemp(prefix="synara-cgroup-v2-live-user-data-", suffix=".yaml")
        user_data_path = pathlib.Path(user_data_name)
        try:
            os.fchmod(user_data_fd, 0o600)
            user_data = creation_marker_cloud_config(creation_token).encode("utf-8")
            user_data_offset = 0
            while user_data_offset < len(user_data):
                user_data_offset += os.write(user_data_fd, user_data[user_data_offset:])
            os.fsync(user_data_fd)
        finally:
            os.close(user_data_fd)
        creation_error: Exception | None = None
        try:
            late_creation_marker_sha256 = marker_sha256
            late_creation_reconciliation_required = True
            try:
                run_command(
                    [
                        options.orbctl,
                        "create",
                        "--isolated",
                        "--user",
                        DISPOSABLE_VM_USER,
                        "--user-data",
                        str(user_data_path),
                        "--arch",
                        "arm64",
                        "ubuntu:24.04",
                        options.vm_name,
                    ],
                    timeout=180,
                )
            except Exception as error:
                creation_error = error
            try:
                identity = prove_created_vm_ownership(options, marker_sha256)
            except Exception as proof_error:
                report["vm"]["createOutcome"] = (
                    "ambiguous-marker-unproved" if creation_error is not None else "success-marker-unproved"
                )
                candidate = observe_unproven_candidate(options)
                if candidate is not None:
                    report["vm"]["unprovenCandidate"] = candidate
                    report["vm"]["orphanCleanup"] = "not attempted because run ownership was not proved"
                if creation_error is not None:
                    raise GateFailure(
                        f"VM create outcome was ambiguous and ownership proof failed: create={creation_error}; proof={proof_error}"
                    ) from creation_error
                raise
            owned_vm_id = identity["id"]
            report["vm"].update(identity)
            report["vm"]["ownedByThisRun"] = True
            late_creation_reconciliation_required = False
            if creation_error is not None:
                report["vm"]["createOutcome"] = "ambiguous-marker-proved"
                raise GateFailure(
                    f"VM create did not return success, but the exact run marker proved ownership for cleanup: {creation_error}"
                ) from creation_error
            report["vm"]["createOutcome"] = "success-marker-proved"
        finally:
            try:
                user_data_path.unlink()
            except FileNotFoundError:
                pass
        host = collect_host_facts(options)
        report["host"] = host
        report["evidenceBoundary"]["hostPrerequisitesObserved"] = [
            "Ubuntu 24.04 arm64 disposable VM",
            host["systemd"],
            "unified cgroup2fs",
        ]

        with tempfile.TemporaryDirectory(prefix="synara-cgroup-v2-live-build-") as build_dir_value:
            build_dir = pathlib.Path(build_dir_value)
            binary = build_dir / "agentd-live.test"
            env = os.environ.copy()
            env.update({"GOOS": "linux", "GOARCH": "arm64", "CGO_ENABLED": "0"})
            run_command(
                ["go", "test", "-c", "-o", str(binary), "./internal/agentd"],
                cwd=repo / "services/control-plane",
                env=env,
                timeout=180,
            )
            binary_sha = file_sha256(binary)
            report["binary"] = {"sha256": binary_sha, "bytes": binary.stat().st_size, "goos": "linux", "goarch": "arm64"}
            remote_dir = f"/opt/synara-cgroup-v2-live/{binary_sha[:16]}"
            remote_binary = remote_dir + "/agentd-live.test"
            stream_binary_to_remote(options, binary, remote_dir, remote_binary, binary_sha)
            report["binary"]["transfer"] = "stdin"
            token = options.vm_name.removeprefix(VM_PREFIX)
            test_evidence, test_output, service = execute_live_test(options, remote_binary, token)
            report["systemdService"] = service
            report["test"] = test_evidence
            report["testOutput"] = test_output
            remote_run(options, "/bin/sh", "-c", f"rm -rf -- {remote_dir}", timeout=20, check=False)
        report["evidenceBoundary"]["containmentScenariosProved"] = True
        report["evidenceBoundary"]["proved"] = [
            "daemon root flock exclusion and crash release",
            "standalone live preflight credential drop, UseCgroupFD, and setsid cleanup",
            "parent PID, legacy v1, and unknown child zero-mutation rejection",
            "same-fence exclusion and real cgroup.kill/cgroup.events orphan recovery",
        ]
        report["status"] = "pass"
    except Exception as error:
        primary_error = error
        report["error"] = bounded(str(error), 4096)
    finally:
        if lock_held:
            if late_creation_reconciliation_required and owned_vm_id is None and late_creation_marker_sha256 is not None:
                try:
                    reconciliation = reconcile_late_creation(options, late_creation_marker_sha256)
                except Exception as error:
                    reconciliation = LateCreationReconciliation(
                        identity=None,
                        last_candidate=observe_unproven_candidate(options),
                        attempts=0,
                        last_error=bounded(str(error), 1024),
                    )
                report["vm"]["lateCreateReconciliation"] = {
                    "attempts": reconciliation.attempts,
                    "lastCandidate": reconciliation.last_candidate,
                    "lastError": reconciliation.last_error,
                    "ownershipProved": reconciliation.identity is not None,
                }
                if reconciliation.identity is not None:
                    owned_vm_id = reconciliation.identity["id"]
                    report["vm"].update(reconciliation.identity)
                    report["vm"]["ownedByThisRun"] = True
                    report["vm"]["createOutcome"] = "late-marker-proved-for-cleanup"
                    report["vm"].pop("unprovenCandidate", None)
                    report["vm"].pop("orphanCleanup", None)
                else:
                    if reconciliation.last_candidate is not None:
                        report["vm"]["unprovenCandidate"] = reconciliation.last_candidate
                    report["vm"]["orphanCleanup"] = (
                        "unresolved late-create risk after bounded reconciliation; no deletion without marker proof"
                    )
                    unresolved_late_create_error = (
                        "unresolved late-create risk after bounded reconciliation: "
                        f"attempts={reconciliation.attempts}, lastError={reconciliation.last_error}, "
                        f"lastCandidate={reconciliation.last_candidate}"
                    )
                    cleanup_errors.append(unresolved_late_create_error)
            try:
                cleanup_errors.extend(
                    cleanup_proven_owned_vm(
                        options,
                        report,
                        owned_vm_id,
                        debian_before,
                    )
                )
            except Exception as error:
                cleanup_errors.append(f"unexpected cleanup failure: {bounded(str(error), 1024)}")
            try:
                lock_context.__exit__(None, None, None)
            except Exception as error:
                cleanup_errors.append(f"release VM name lock failed: {bounded(str(error), 1024)}")
        if unresolved_late_create_error is not None:
            primary_text = "none" if primary_error is None else bounded(str(primary_error), 2048)
            primary_error = GateFailure(
                f"primary={primary_text}; cleanup={unresolved_late_create_error}"
            )
        if cleanup_errors:
            report["status"] = "fail"
            report["cleanupErrors"] = cleanup_errors
        report["completedAt"] = dt.datetime.now(dt.UTC).isoformat()
        report_write_errors: list[str] = []
        report_written = False
        try:
            write_report(options.output, report)
        except Exception as error:
            report_write_errors.append(bounded(str(error), 1024))
            report["status"] = "fail"
            report.setdefault("cleanupErrors", []).append(
                f"write report failed: {bounded(str(error), 1024)}"
            )
            try:
                write_report(options.output, report)
            except Exception as retry_error:
                report_write_errors.append(bounded(str(retry_error), 1024))
            else:
                report_written = True
        else:
            report_written = True

        permanent_report_failure = not report_written
        if permanent_report_failure:
            report["reportWriteErrors"] = report_write_errors
            if fallback_path is not None:
                report["fallbackEvidencePath"] = str(fallback_path)
            if fallback_descriptor is not None:
                try:
                    write_fallback_evidence(fallback_descriptor, report)
                except Exception as fallback_error:
                    report_write_errors.append(f"fallback evidence write failed: {bounded(str(fallback_error), 1024)}")
            primary_text = "none" if primary_error is None else bounded(str(primary_error), 2048)
            cleanup_text = "none" if not cleanup_errors else "; ".join(cleanup_errors)
            fallback_text = "unavailable" if fallback_path is None else str(fallback_path)
            primary_error = GateFailure(
                "gate/report failure aggregate: "
                f"primary={primary_text}; cleanup={cleanup_text}; "
                f"report={'; '.join(report_write_errors)}; fallback={fallback_text}"
            )
        if fallback_descriptor is not None:
            try:
                os.close(fallback_descriptor)
            except OSError:
                pass
        if not permanent_report_failure and fallback_path is not None:
            try:
                fallback_path.unlink()
            except FileNotFoundError:
                pass
    if primary_error is not None:
        raise primary_error
    if report["status"] != "pass":
        raise GateFailure("live gate did not finish with complete cleanup")
    return report


def main(argv: Sequence[str] | None = None) -> int:
    try:
        options = parse_args(sys.argv[1:] if argv is None else argv)
        report = run_gate(options)
    except (GateFailure, OSError, ValueError, json.JSONDecodeError, subprocess.SubprocessError) as error:
        print(f"protected cgroup live gate failed: {error}", file=sys.stderr)
        return 1
    print(json.dumps({"status": report["status"], "output": str(options.output), "vmDeleted": report["vm"]["deleted"], "debianUnchanged": report["preservedVm"]["unchanged"]}, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
