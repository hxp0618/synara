#!/usr/bin/env python3
"""Verify explicit SSH protected-cgroup host wiring and signed manifest trust."""

from __future__ import annotations

import argparse
import base64
import binascii
import dataclasses
import json
import pathlib
import re
import shlex
import subprocess
import sys
import tempfile
import uuid as uuidlib
from hashlib import sha256
from collections.abc import Callable, Mapping, Sequence
from typing import Any


SCHEMA_VERSION = "synara.ssh-protected-cgroup-gate.v2"
SUPPORTED_PROTECTED_CGROUP_SUPERVISOR = "agentd-protected-cgroup-supervisor-v3"
SUPPORTED_PROTECTED_CGROUP_PROBE_VERSION = 2
PROTECTED_CGROUP_SUPERVISOR_SUBGROUP = "synara-agentd"
SIGNATURE_DOMAIN = b"synara.process-containment-attestation.v1\n"
IDENTITY_PATTERN = re.compile(r"^uid:(\d+) gid:(\d+)$")
SHA256_PATTERN = re.compile(r"^[0-9a-f]{64}$")
SSH_HOST_PATTERN = re.compile(r"^[A-Za-z0-9._:-]{1,253}$")
SSH_ACCOUNT_PATTERN = re.compile(r"^[a-z_][a-z0-9_-]*[$]?$")
SYSTEMD_SERVICE_PATTERN = re.compile(r"^[A-Za-z0-9_.@:-]+\.service$")
REMOTE_PATH_PATTERN = re.compile(r"^/[A-Za-z0-9._/-]+$")

REQUIRED_REMOTE_ENV_KEYS = (
    "SYNARA_EXECUTION_TARGET_ID",
    "SYNARA_EXECUTION_TARGET_KIND",
    "SYNARA_AGENTD_CLUSTER_ID",
    "SYNARA_AGENTD_NAMESPACE",
    "SYNARA_AGENTD_INSTANCE_ID",
    "SYNARA_AGENTD_INSTANCE_UID",
    "SYNARA_AGENTD_CGROUP_V2_ROOT",
    "SYNARA_AGENTD_CGROUP_V2_PROVIDER_UID",
    "SYNARA_AGENTD_CGROUP_V2_PROVIDER_GID",
    "SYNARA_AGENTD_CGROUP_V2_PROVIDER_PIDS_MAX",
    "SYNARA_AGENTD_CGROUP_V2_PROVIDER_MEMORY_MAX_BYTES",
    "SYNARA_AGENTD_CGROUP_V2_PROVIDER_CPU_QUOTA_MICROS",
    "SYNARA_AGENTD_CGROUP_V2_PROVIDER_CPU_PERIOD_MICROS",
    "SYNARA_AGENTD_CGROUP_V2_ATTESTATION_KEY_ID",
    "SYNARA_AGENTD_CGROUP_V2_ATTESTATION_PRIVATE_KEY_FILE",
    "SYNARA_AGENTD_BUILD_GIT_SHA",
    "SYNARA_AGENTD_IMAGE_DIGEST",
    "SYNARA_AGENTD_VERSION",
)


@dataclasses.dataclass(frozen=True)
class SSHProtectedCgroupGateOptions:
    ssh_host: str
    ssh_port: int
    ssh_user: str
    ssh_identity_file: pathlib.Path
    ssh_host_key_file: pathlib.Path
    service_name: str
    remote_agentd_binary: str
    remote_env_file: str
    target_capabilities_file: pathlib.Path
    worker_manifest_file: pathlib.Path
    registration_context_file: pathlib.Path


@dataclasses.dataclass(frozen=True)
class ProcessContainmentPolicy:
    trust_mode: str
    key_id: str
    public_key: bytes
    public_key_base64: str


def parse_args(argv: Sequence[str]) -> SSHProtectedCgroupGateOptions:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--allow-remote-host", action="store_true")
    parser.add_argument("--ssh-host")
    parser.add_argument("--ssh-port", type=int, default=22)
    parser.add_argument("--ssh-user")
    parser.add_argument("--ssh-identity-file", type=pathlib.Path)
    parser.add_argument("--ssh-host-key-file", type=pathlib.Path)
    parser.add_argument("--service-name")
    parser.add_argument("--remote-agentd-binary")
    parser.add_argument("--remote-env-file")
    parser.add_argument("--target-capabilities-file", type=pathlib.Path)
    parser.add_argument("--worker-manifest-file", type=pathlib.Path)
    parser.add_argument("--registration-context-file", type=pathlib.Path)
    parsed = parser.parse_args(argv)
    if not parsed.allow_remote_host:
        parser.error("--allow-remote-host is required before this gate will contact a host")
    required = {
        "--ssh-host": parsed.ssh_host,
        "--ssh-user": parsed.ssh_user,
        "--ssh-identity-file": parsed.ssh_identity_file,
        "--ssh-host-key-file": parsed.ssh_host_key_file,
        "--service-name": parsed.service_name,
        "--remote-agentd-binary": parsed.remote_agentd_binary,
        "--remote-env-file": parsed.remote_env_file,
        "--target-capabilities-file": parsed.target_capabilities_file,
        "--worker-manifest-file": parsed.worker_manifest_file,
        "--registration-context-file": parsed.registration_context_file,
    }
    missing = [option for option, value in required.items() if value in (None, "")]
    if missing:
        parser.error("missing required explicit inputs: " + ", ".join(missing))
    if parsed.ssh_port < 1 or parsed.ssh_port > 65535:
        parser.error("--ssh-port must be between 1 and 65535")
    for option, path in {
        "--ssh-identity-file": parsed.ssh_identity_file,
        "--ssh-host-key-file": parsed.ssh_host_key_file,
        "--target-capabilities-file": parsed.target_capabilities_file,
        "--worker-manifest-file": parsed.worker_manifest_file,
        "--registration-context-file": parsed.registration_context_file,
    }.items():
        if path is None or not path.is_file():
            parser.error(f"{option} must point to an existing file")
    try:
        ssh_host = validate_ssh_host(parsed.ssh_host)
        ssh_user = validate_ssh_account(parsed.ssh_user, "--ssh-user")
        service_name = validate_systemd_service_name(parsed.service_name)
        remote_agentd_binary = validate_remote_path(parsed.remote_agentd_binary, "--remote-agentd-binary")
        remote_env_file = validate_remote_path(parsed.remote_env_file, "--remote-env-file")
    except ValueError as error:
        parser.error(str(error))
    return SSHProtectedCgroupGateOptions(
        ssh_host=ssh_host,
        ssh_port=parsed.ssh_port,
        ssh_user=ssh_user,
        ssh_identity_file=parsed.ssh_identity_file.expanduser().resolve(),
        ssh_host_key_file=parsed.ssh_host_key_file.expanduser().resolve(),
        service_name=service_name,
        remote_agentd_binary=remote_agentd_binary,
        remote_env_file=remote_env_file,
        target_capabilities_file=parsed.target_capabilities_file.expanduser().resolve(),
        worker_manifest_file=parsed.worker_manifest_file.expanduser().resolve(),
        registration_context_file=parsed.registration_context_file.expanduser().resolve(),
    )


def main(argv: Sequence[str] | None = None) -> int:
    options = parse_args(sys.argv[1:] if argv is None else argv)
    try:
        result = run_gate(options)
    except (OSError, ValueError, binascii.Error, subprocess.SubprocessError) as error:
        print(f"SSH protected cgroup gate failed: {error}", file=sys.stderr)
        return 1
    print(json.dumps(result, indent=2, sort_keys=True))
    return 0


def run_gate(
    options: SSHProtectedCgroupGateOptions,
    *,
    remote_runner: Callable[[SSHProtectedCgroupGateOptions, str], str] | None = None,
    attestation_verifier: Callable[[bytes, Mapping[str, Any], Mapping[str, Any]], None] | None = None,
) -> dict[str, Any]:
    remote_runner = remote_runner or run_remote_command
    attestation_verifier = attestation_verifier or verify_ed25519_attestation
    target_capabilities = load_json_object(options.target_capabilities_file, "target capabilities")
    worker_manifest = load_json_object(options.worker_manifest_file, "worker manifest")
    registration_context = load_json_object(options.registration_context_file, "registration context")
    policy = parse_process_containment_policy(capability_object(target_capabilities))
    live = collect_live_host_snapshot(options, remote_runner)
    preflight = run_live_preflight(options, remote_runner, live["env"])
    validate_registration_context_matches_live_env(registration_context, live["env"])
    validate_projected_manifest_trust(worker_manifest)
    validate_live_preflight(
        live["serviceUser"], live["delegate"], live["delegateSubgroup"], live["env"], preflight
    )
    validate_manifest_trusted(
        capability_object(worker_manifest),
        registration_context,
        policy,
        preflight,
        verifier=attestation_verifier,
    )
    final_live = collect_live_host_snapshot(options, remote_runner)
    validate_same_live_host_incarnation(live, final_live)
    return {
        "schemaVersion": SCHEMA_VERSION,
        "status": "pass",
        "service": {
            "name": options.service_name,
            "user": live["serviceUser"],
            "delegate": live["delegate"],
            "delegateSubgroup": live["delegateSubgroup"],
            "activeState": live["activeState"],
            "subState": live["subState"],
            "mainPid": live["mainPid"],
            "processStartTime": live["processStartTime"],
            "controlGroup": live["controlGroup"],
        },
        "remote": {
            "host": options.ssh_host,
            "port": options.ssh_port,
            "user": options.ssh_user,
            "agentdBinary": options.remote_agentd_binary,
            "envFile": options.remote_env_file,
        },
        "protectedCgroup": {
            "cgroupRoot": live["env"]["SYNARA_AGENTD_CGROUP_V2_ROOT"],
            "cgroupFilesystem": live["cgroupFilesystem"],
            "preflight": preflight,
            "policyKeyId": policy.key_id,
        },
        "manifestTrusted": True,
    }


def collect_live_host_snapshot(
    options: SSHProtectedCgroupGateOptions,
    remote_runner: Callable[[SSHProtectedCgroupGateOptions, str], str],
) -> dict[str, Any]:
    initial = _collect_live_host_snapshot_once(options, remote_runner)
    final = _collect_live_host_snapshot_once(options, remote_runner)
    validate_same_live_host_incarnation(initial, final, boundary="within one live-host snapshot")
    return initial


def _collect_live_host_snapshot_once(
    options: SSHProtectedCgroupGateOptions,
    remote_runner: Callable[[SSHProtectedCgroupGateOptions, str], str],
) -> dict[str, Any]:
    pattern = "^(" + "|".join(REQUIRED_REMOTE_ENV_KEYS) + ")="
    env_payload = remote_runner(
        options,
        "grep -E " + shlex.quote(pattern) + " " + shlex.quote(options.remote_env_file),
    )
    env_values = parse_shell_environment(env_payload)
    for name in REQUIRED_REMOTE_ENV_KEYS:
        if name not in env_values or not env_values[name]:
            raise ValueError(f"remote env file omitted required protected-cgroup variable {name}")
    if not valid_build_git_sha(env_values["SYNARA_AGENTD_BUILD_GIT_SHA"]):
        raise ValueError("remote env file build git SHA is invalid")
    if not valid_image_digest(env_values["SYNARA_AGENTD_IMAGE_DIGEST"]):
        raise ValueError("remote env file image digest is invalid")
    service_user = remote_runner(
        options,
        "systemctl show " + shlex.quote(options.service_name) + " --property=User --value",
    ).strip()
    delegate = remote_runner(
        options,
        "systemctl show " + shlex.quote(options.service_name) + " --property=Delegate --value",
    ).strip().lower()
    delegate_subgroup = remote_runner(
        options,
        "systemctl show " + shlex.quote(options.service_name) + " --property=DelegateSubgroup --value",
    ).strip()
    active_state = remote_runner(
        options,
        "systemctl show " + shlex.quote(options.service_name) + " --property=ActiveState --value",
    ).strip().lower()
    sub_state = remote_runner(
        options,
        "systemctl show " + shlex.quote(options.service_name) + " --property=SubState --value",
    ).strip().lower()
    main_pid_text = remote_runner(
        options,
        "systemctl show " + shlex.quote(options.service_name) + " --property=MainPID --value",
    ).strip()
    if active_state != "active" or sub_state != "running":
        raise ValueError("systemd service is not active/running")
    if delegate_subgroup != PROTECTED_CGROUP_SUPERVISOR_SUBGROUP:
        raise ValueError("systemd service DelegateSubgroup is not the protected supervisor subgroup")
    if not main_pid_text.isdigit() or int(main_pid_text) <= 0:
        raise ValueError("systemd service does not have a running MainPID")
    main_pid = int(main_pid_text)
    process_stat = remote_runner(
        options,
        "cat -- " + shlex.quote(f"/proc/{main_pid}/stat"),
    )
    process_start_time = parse_proc_stat_starttime(process_stat, main_pid)
    control_group = remote_runner(
        options,
        "systemctl show " + shlex.quote(options.service_name) + " --property=ControlGroup --value",
    ).strip()
    expected_cgroup_root = systemd_control_group_root(control_group)
    environment_files = remote_runner(
        options,
        "systemctl show " + shlex.quote(options.service_name) + " --property=EnvironmentFiles --value",
    ).strip()
    validate_systemd_environment_file_binding(environment_files, options.remote_env_file)
    process_executable = remote_runner(
        options,
        "readlink -f " + shlex.quote(f"/proc/{main_pid}/exe"),
    ).strip()
    if process_executable != options.remote_agentd_binary:
        raise ValueError("systemd service MainPID does not execute the expected agentd binary")
    process_control_group = remote_runner(
        options,
        "awk -F: '$1 == \"0\" {print $3}' " + shlex.quote(f"/proc/{main_pid}/cgroup"),
    ).strip()
    expected_process_control_group = control_group + "/" + PROTECTED_CGROUP_SUPERVISOR_SUBGROUP
    if process_control_group != expected_process_control_group:
        raise ValueError("systemd service MainPID is outside the protected supervisor subgroup")
    process_env_payload = remote_runner(
        options,
        "tr '\\000' '\\n' < " + shlex.quote(f"/proc/{main_pid}/environ") +
        " | grep -E " + shlex.quote(pattern),
    )
    process_env_values = parse_shell_environment(process_env_payload)
    for name in REQUIRED_REMOTE_ENV_KEYS:
        if process_env_values.get(name) != env_values[name]:
            raise ValueError(f"systemd service process environment does not match {name} in the bound env file")
    cgroup_root = env_values["SYNARA_AGENTD_CGROUP_V2_ROOT"]
    if cgroup_root != expected_cgroup_root:
        raise ValueError("protected cgroup root is not the systemd service delegated ControlGroup")
    key_path = env_values["SYNARA_AGENTD_CGROUP_V2_ATTESTATION_PRIVATE_KEY_FILE"]
    cgroup_filesystem = remote_runner(
        options,
        "stat -fc %T " + shlex.quote(cgroup_root),
    ).strip()
    if cgroup_filesystem != "cgroup2fs":
        raise ValueError("protected cgroup root must be mounted on cgroup2fs")
    remote_runner(options, "test -f " + shlex.quote(cgroup_root + "/cgroup.controllers"))
    parent_processes = remote_runner(
        options,
        "cat -- " + shlex.quote(cgroup_root + "/cgroup.procs"),
    ).split()
    if parent_processes:
        raise ValueError("protected cgroup service parent must be process-free")
    supervisor_cgroup = cgroup_root + "/" + PROTECTED_CGROUP_SUPERVISOR_SUBGROUP
    remote_runner(options, "test -d " + shlex.quote(supervisor_cgroup))
    supervisor_processes = remote_runner(
        options,
        "cat -- " + shlex.quote(supervisor_cgroup + "/cgroup.procs"),
    ).split()
    if supervisor_processes != [str(main_pid)]:
        raise ValueError("protected cgroup supervisor subgroup must contain only the systemd MainPID")
    enabled_controllers = set(remote_runner(
        options,
        "cat -- " + shlex.quote(cgroup_root + "/cgroup.subtree_control"),
    ).split())
    if not {"cpu", "memory", "pids"}.issubset(enabled_controllers):
        raise ValueError("protected cgroup service parent omitted required cpu/memory/pids controllers")
    remote_runner(options, "test \"$(stat -c %u " + shlex.quote(key_path) + ")\" = 0")
    remote_runner(options, "test \"$(stat -c %F " + shlex.quote(key_path) + ")\" = " + shlex.quote("regular file"))
    remote_runner(
        options,
        "case \"$(stat -c %a " + shlex.quote(key_path) + ")\" in 000|?00|??00) ;; *) exit 1 ;; esac",
    )
    cgroup_root_identity = remote_runner(
        options,
        "stat -Lc '%d:%i' " + shlex.quote(cgroup_root),
    ).strip()
    if re.fullmatch(r"[1-9][0-9]*:[1-9][0-9]*", cgroup_root_identity) is None:
        raise ValueError("protected cgroup root device/inode identity is invalid")
    snapshot = {
        "serviceUser": service_user,
        "delegate": delegate,
        "delegateSubgroup": delegate_subgroup,
        "activeState": active_state,
        "subState": sub_state,
        "mainPid": main_pid,
        "processStartTime": process_start_time,
        "controlGroup": control_group,
        "environmentFiles": environment_files,
        "processExecutable": process_executable,
        "processControlGroup": process_control_group,
        "env": env_values,
        "processEnv": process_env_values,
        "parentProcesses": parent_processes,
        "supervisorProcesses": supervisor_processes,
        "enabledControllers": sorted(enabled_controllers),
        "cgroupFilesystem": cgroup_filesystem,
        "cgroupRootIdentity": cgroup_root_identity,
    }
    tail_active_state = remote_runner(
        options,
        "systemctl show " + shlex.quote(options.service_name) + " --property=ActiveState --value",
    ).strip().lower()
    tail_service_user = remote_runner(
        options,
        "systemctl show " + shlex.quote(options.service_name) + " --property=User --value",
    ).strip()
    tail_delegate = remote_runner(
        options,
        "systemctl show " + shlex.quote(options.service_name) + " --property=Delegate --value",
    ).strip().lower()
    tail_delegate_subgroup = remote_runner(
        options,
        "systemctl show " + shlex.quote(options.service_name) + " --property=DelegateSubgroup --value",
    ).strip()
    tail_sub_state = remote_runner(
        options,
        "systemctl show " + shlex.quote(options.service_name) + " --property=SubState --value",
    ).strip().lower()
    tail_main_pid_text = remote_runner(
        options,
        "systemctl show " + shlex.quote(options.service_name) + " --property=MainPID --value",
    ).strip()
    if not tail_main_pid_text.isdigit() or int(tail_main_pid_text) <= 0:
        raise ValueError("systemd service lost its running MainPID during live-host snapshot")
    tail_main_pid = int(tail_main_pid_text)
    tail_process_stat = remote_runner(
        options,
        "cat -- " + shlex.quote(f"/proc/{tail_main_pid}/stat"),
    )
    tail_start_time = parse_proc_stat_starttime(tail_process_stat, tail_main_pid)
    tail_control_group = remote_runner(
        options,
        "systemctl show " + shlex.quote(options.service_name) + " --property=ControlGroup --value",
    ).strip()
    tail_parent_processes = remote_runner(
        options,
        "cat -- " + shlex.quote(cgroup_root + "/cgroup.procs"),
    ).split()
    tail_supervisor_processes = remote_runner(
        options,
        "cat -- " + shlex.quote(supervisor_cgroup + "/cgroup.procs"),
    ).split()
    tail_enabled_controllers = sorted(set(remote_runner(
        options,
        "cat -- " + shlex.quote(cgroup_root + "/cgroup.subtree_control"),
    ).split()))
    tail_root_identity = remote_runner(
        options,
        "stat -Lc '%d:%i' " + shlex.quote(cgroup_root),
    ).strip()
    tail_cgroup_filesystem = remote_runner(
        options,
        "stat -fc %T " + shlex.quote(cgroup_root),
    ).strip()
    tail = {
        "serviceUser": tail_service_user,
        "delegate": tail_delegate,
        "delegateSubgroup": tail_delegate_subgroup,
        "activeState": tail_active_state,
        "subState": tail_sub_state,
        "mainPid": tail_main_pid,
        "processStartTime": tail_start_time,
        "controlGroup": tail_control_group,
        "parentProcesses": tail_parent_processes,
        "supervisorProcesses": tail_supervisor_processes,
        "enabledControllers": tail_enabled_controllers,
        "cgroupRootIdentity": tail_root_identity,
        "cgroupFilesystem": tail_cgroup_filesystem,
    }
    for field, value in tail.items():
        if snapshot[field] != value:
            raise ValueError(f"live agentd incarnation changed within core snapshot: {field}")
    return snapshot


def run_live_preflight(
    options: SSHProtectedCgroupGateOptions,
    remote_runner: Callable[[SSHProtectedCgroupGateOptions, str], str],
    env_values: Mapping[str, str],
) -> dict[str, Any]:
    command = build_live_preflight_command(options, env_values)
    preflight_payload = remote_runner(
        options,
        command,
    )
    preflight = json.loads(preflight_payload)
    if not isinstance(preflight, dict):
        raise ValueError("protected-cgroup-preflight did not return a JSON object")
    return preflight


def build_live_preflight_command(
    options: SSHProtectedCgroupGateOptions,
    env_values: Mapping[str, str],
) -> str:
    missing = [name for name in REQUIRED_REMOTE_ENV_KEYS if not env_values.get(name)]
    if missing:
        raise ValueError("live preflight is missing allowlisted environment values")
    minimal_environment = {
        name: expect_string(env_values.get(name), "live preflight env " + name)
        for name in REQUIRED_REMOTE_ENV_KEYS
    }
    minimal_environment.update({
        "SYNARA_CONTROL_PLANE_URL": "http://127.0.0.1:1",
        "SYNARA_WORKER_REGISTRATION_TOKEN": "ssh-protected-cgroup-preflight-dummy",
        "SYNARA_AGENTD_RUNNER_COMMAND_JSON": '["/bin/false"]',
        "SYNARA_AGENTD_SSH_BOOTSTRAP_GENERATION": "1",
    })
    assignments = [
        shlex.quote(name + "=" + value)
        for name, value in sorted(minimal_environment.items())
    ]
    return " ".join([
        "/usr/bin/env",
        "-i",
        *assignments,
        shlex.quote(options.remote_agentd_binary),
        "protected-cgroup-preflight",
    ])


def parse_proc_stat_starttime(payload: str, expected_pid: int) -> int:
    line = payload.strip()
    open_parenthesis = line.find("(")
    close_parenthesis = line.rfind(")")
    if open_parenthesis <= 0 or close_parenthesis <= open_parenthesis:
        raise ValueError("systemd service MainPID /proc stat is malformed")
    if line[:open_parenthesis].strip() != str(expected_pid):
        raise ValueError("systemd service MainPID /proc stat PID does not match")
    # Fields after the final ')' start with field 3 (state). starttime is
    # field 22, therefore index 19 in this suffix. rfind handles spaces and ')'
    # characters inside the process comm field.
    suffix = line[close_parenthesis + 1:].strip().split()
    if len(suffix) <= 19 or not suffix[19].isdigit() or int(suffix[19]) <= 0:
        raise ValueError("systemd service MainPID /proc stat starttime is invalid")
    return int(suffix[19])


def validate_same_live_host_incarnation(
    initial: Mapping[str, Any],
    final: Mapping[str, Any],
    *,
    boundary: str = "during protected-cgroup gate",
) -> None:
    fields = (
        "activeState",
        "subState",
        "serviceUser",
        "delegate",
        "delegateSubgroup",
        "mainPid",
        "processStartTime",
        "controlGroup",
        "environmentFiles",
        "processExecutable",
        "processControlGroup",
        "env",
        "processEnv",
        "parentProcesses",
        "supervisorProcesses",
        "enabledControllers",
        "cgroupFilesystem",
        "cgroupRootIdentity",
    )
    for field in fields:
        if initial.get(field) != final.get(field):
            raise ValueError(f"live agentd incarnation changed {boundary}: {field}")


def run_remote_command(options: SSHProtectedCgroupGateOptions, command: str) -> str:
    ssh_command = [
        "ssh",
        "-i",
        str(options.ssh_identity_file),
        "-o",
        "IdentitiesOnly=yes",
        "-o",
        "StrictHostKeyChecking=yes",
        "-o",
        "UserKnownHostsFile=" + str(options.ssh_host_key_file),
        "-p",
        str(options.ssh_port),
        f"{options.ssh_user}@{options.ssh_host}",
        "sh -ceu " + shlex.quote(command),
    ]
    result = subprocess.run(ssh_command, check=True, capture_output=True, text=True)
    return result.stdout


def load_json_object(path: pathlib.Path, label: str) -> dict[str, Any]:
    value = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(value, dict):
        raise ValueError(f"{label} must be a JSON object")
    return value


def capability_object(payload: Mapping[str, Any]) -> Mapping[str, Any]:
    raw = payload.get("capabilities")
    if isinstance(raw, Mapping):
        return raw
    return payload


def parse_shell_environment(payload: str) -> dict[str, str]:
    values: dict[str, str] = {}
    for raw_line in payload.splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#"):
            continue
        name, separator, encoded = line.partition("=")
        if separator != "=" or not name.strip():
            raise ValueError("remote env file contains an invalid assignment")
        decoded = shlex.split(encoded, posix=True)
        if len(decoded) != 1:
            raise ValueError(f"remote env assignment {name.strip()} is invalid")
        values[name.strip()] = decoded[0]
    return values


def systemd_control_group_root(control_group: str) -> str:
    if not control_group.startswith("/") or control_group == "/" or ".." in control_group.split("/"):
        raise ValueError("systemd service ControlGroup is invalid")
    normalized = pathlib.PurePosixPath(control_group)
    if str(normalized) != control_group:
        raise ValueError("systemd service ControlGroup is not canonical")
    return str(pathlib.PurePosixPath("/sys/fs/cgroup") / control_group.lstrip("/"))


def validate_systemd_environment_file_binding(environment_files: str, expected_path: str) -> None:
    pattern = re.compile(r"(?:^|\s)-?" + re.escape(expected_path) + r"(?=\s|$)")
    if pattern.search(environment_files) is None:
        raise ValueError("systemd service is not bound to the expected EnvironmentFile")


def validate_registration_context_matches_live_env(
    registration_context: Mapping[str, Any],
    env_values: Mapping[str, str],
) -> None:
    mappings = {
        "executionTargetId": "SYNARA_EXECUTION_TARGET_ID",
        "targetKind": "SYNARA_EXECUTION_TARGET_KIND",
        "instanceUid": "SYNARA_AGENTD_INSTANCE_UID",
        "clusterId": "SYNARA_AGENTD_CLUSTER_ID",
        "namespace": "SYNARA_AGENTD_NAMESPACE",
        "podName": "SYNARA_AGENTD_INSTANCE_ID",
    }
    for context_name, env_name in mappings.items():
        context_value = expect_string(registration_context.get(context_name), "registrationContext." + context_name)
        env_value = expect_string(env_values.get(env_name), "env " + env_name)
        if context_value != env_value:
            raise ValueError(f"registration context {context_name} does not match the live service environment")
    if registration_context["targetKind"] != "ssh":
        raise ValueError("registration context targetKind is not ssh")
    for field in ("executionTargetId", "instanceUid"):
        try:
            parsed = uuidlib.UUID(str(registration_context[field]))
        except (ValueError, AttributeError) as error:
            raise ValueError(f"registration context {field} is not a UUID") from error
        if parsed.int == 0 or str(parsed) != registration_context[field]:
            raise ValueError(f"registration context {field} is not a canonical non-zero UUID")


def parse_process_containment_policy(capabilities: Mapping[str, Any]) -> ProcessContainmentPolicy:
    raw_policy = capabilities.get("processContainmentPolicy")
    if not isinstance(raw_policy, Mapping):
        raise ValueError("target processContainmentPolicy is missing")
    trust_mode = expect_string(raw_policy.get("trustMode"), "processContainmentPolicy.trustMode")
    if trust_mode != "signed-v1":
        raise ValueError("target processContainmentPolicy must use signed-v1")
    key_id = expect_string(raw_policy.get("keyId"), "processContainmentPolicy.keyId")
    public_key_base64 = expect_string(raw_policy.get("ed25519PublicKey"), "processContainmentPolicy.ed25519PublicKey")
    public_key = decode_standard_or_raw_standard_base64(
        public_key_base64,
        "processContainmentPolicy.ed25519PublicKey",
    )
    if len(public_key) != 32:
        raise ValueError("target processContainmentPolicy public key must decode to 32 bytes")
    return ProcessContainmentPolicy(
        trust_mode=trust_mode,
        key_id=key_id,
        public_key=public_key,
        public_key_base64=base64.b64encode(public_key).decode("ascii"),
    )


def validate_projected_manifest_trust(worker_manifest: Mapping[str, Any]) -> None:
    projection = projected_process_containment(worker_manifest)
    if projection is None:
        raise ValueError("worker manifest omitted projected processContainment trustState")
    if expect_string(projection.get("mode"), "worker manifest processContainment.mode") != "cgroup-v2":
        raise ValueError("worker manifest projected processContainment mode is invalid")
    if expect_string(projection.get("trustState"), "worker manifest processContainment.trustState") != "verified":
        raise ValueError("worker manifest projected processContainment trustState is not verified")


def validate_live_preflight(
    service_user: str,
    delegate: str,
    delegate_subgroup: str,
    env_values: Mapping[str, str],
    preflight: Mapping[str, Any],
) -> None:
    if service_user != "root":
        raise ValueError("systemd service user is not root")
    if delegate not in {"yes", "true", "1"}:
        raise ValueError("systemd service is missing Delegate=yes")
    if delegate_subgroup != PROTECTED_CGROUP_SUPERVISOR_SUBGROUP:
        raise ValueError("systemd service is missing the exact protected DelegateSubgroup")
    if not expect_bool(preflight.get("enabled"), "preflight.enabled"):
        raise ValueError("protected cgroup preflight is not enabled")
    if preflight.get("mode") != "cgroup-v2":
        raise ValueError("protected cgroup preflight mode is invalid")
    if preflight.get("supervisorVersion") != SUPPORTED_PROTECTED_CGROUP_SUPERVISOR:
        raise ValueError("protected cgroup preflight supervisorVersion is unsupported")
    if expect_int(preflight.get("probeVersion"), "preflight.probeVersion") != SUPPORTED_PROTECTED_CGROUP_PROBE_VERSION:
        raise ValueError("protected cgroup preflight probeVersion is unsupported")
    if not expect_bool(preflight.get("useCgroupFD"), "preflight.useCgroupFD"):
        raise ValueError("protected cgroup preflight did not prove UseCgroupFD")
    if not expect_bool(preflight.get("setsidDescendantKilled"), "preflight.setsidDescendantKilled"):
        raise ValueError("protected cgroup preflight did not prove setsid descendant cleanup")
    probe_sha = expect_string(preflight.get("probeSha256"), "preflight.probeSha256")
    if SHA256_PATTERN.fullmatch(probe_sha) is None:
        raise ValueError("protected cgroup preflight probeSha256 is invalid")
    supervisor_identity = parse_identity(expect_string(preflight.get("supervisorIdentity"), "preflight.supervisorIdentity"))
    provider_identity = parse_identity(expect_string(preflight.get("providerIdentity"), "preflight.providerIdentity"))
    if supervisor_identity["uid"] != 0:
        raise ValueError("protected cgroup preflight supervisor is not root")
    if supervisor_identity["uid"] == provider_identity["uid"]:
        raise ValueError("protected cgroup preflight supervisor/provider share a UID")
    provider_uid = int(expect_string(env_values.get("SYNARA_AGENTD_CGROUP_V2_PROVIDER_UID"), "env provider uid"))
    provider_gid = int(expect_string(env_values.get("SYNARA_AGENTD_CGROUP_V2_PROVIDER_GID"), "env provider gid"))
    if provider_identity["uid"] != provider_uid or provider_identity["gid"] != provider_gid:
        raise ValueError("protected cgroup preflight provider identity does not match env configuration")
    if expect_int(preflight.get("providerUid"), "preflight.providerUid") != provider_uid or expect_int(
        preflight.get("providerGid"),
        "preflight.providerGid",
    ) != provider_gid:
        raise ValueError("protected cgroup preflight observed provider uid/gid do not match env configuration")
    if not expect_bool(preflight.get("resourceLimitsApplied"), "preflight.resourceLimitsApplied"):
        raise ValueError("protected cgroup preflight did not prove finite Provider resource limits")
    provider_limits = preflight.get("providerLimits")
    if not isinstance(provider_limits, Mapping):
        raise ValueError("protected cgroup preflight omitted Provider resource limits")
    expected_limits = {
        "pidsMax": int(expect_string(env_values.get("SYNARA_AGENTD_CGROUP_V2_PROVIDER_PIDS_MAX"), "env pids max")),
        "memoryMaxBytes": int(expect_string(
            env_values.get("SYNARA_AGENTD_CGROUP_V2_PROVIDER_MEMORY_MAX_BYTES"), "env memory max"
        )),
        "cpuQuotaMicros": int(expect_string(
            env_values.get("SYNARA_AGENTD_CGROUP_V2_PROVIDER_CPU_QUOTA_MICROS"), "env cpu quota"
        )),
        "cpuPeriodMicros": int(expect_string(
            env_values.get("SYNARA_AGENTD_CGROUP_V2_PROVIDER_CPU_PERIOD_MICROS"), "env cpu period"
        )),
    }
    if any(value <= 0 for value in expected_limits.values()):
        raise ValueError("protected cgroup environment contains non-positive Provider resource limits")
    actual_limits = {
        name: expect_int(provider_limits.get(name), "preflight.providerLimits." + name)
        for name in expected_limits
    }
    if actual_limits != expected_limits:
        raise ValueError("protected cgroup preflight Provider limits do not match env configuration")
    if expect_string(preflight.get("attestationKeyId"), "preflight.attestationKeyId") != expect_string(
        env_values.get("SYNARA_AGENTD_CGROUP_V2_ATTESTATION_KEY_ID"), "env attestation key id"
    ):
        raise ValueError("protected cgroup preflight attestation key id does not match env configuration")
    public_key_base64 = expect_string(
        preflight.get("attestationPublicKeyBase64"), "preflight.attestationPublicKeyBase64"
    )
    public_key = decode_canonical_base64(public_key_base64, "preflight.attestationPublicKeyBase64")
    if len(public_key) != 32:
        raise ValueError("protected cgroup preflight public key must decode to 32 bytes")
    if base64.b64encode(public_key).decode("ascii") != public_key_base64:
        raise ValueError("protected cgroup preflight public key is not canonical base64")
    public_key_sha256 = expect_string(preflight.get("attestationPublicKeySha256"), "preflight.attestationPublicKeySha256")
    if SHA256_PATTERN.fullmatch(public_key_sha256) is None:
        raise ValueError("protected cgroup preflight public key digest is invalid")
    if sha256(public_key).hexdigest() != public_key_sha256:
        raise ValueError("protected cgroup preflight public key digest does not match the reported key")
    if preflight.get("attestation") is None or not isinstance(preflight.get("attestation"), Mapping):
        raise ValueError("protected cgroup preflight did not include a signed attestation")


def validate_manifest_trusted(
    worker_manifest: Mapping[str, Any],
    registration_context: Mapping[str, Any],
    policy: ProcessContainmentPolicy,
    preflight: Mapping[str, Any],
    *,
    verifier: Callable[[bytes, Mapping[str, Any], Mapping[str, Any]], None],
) -> None:
    worker_runtime = worker_manifest.get("workerRuntime")
    if not isinstance(worker_runtime, Mapping):
        raise ValueError("worker manifest omitted workerRuntime")
    containment = worker_runtime.get("processContainment")
    if not isinstance(containment, Mapping):
        raise ValueError("worker manifest omitted workerRuntime.processContainment")
    if expect_string(worker_runtime.get("operatingSystem"), "workerRuntime.operatingSystem") != "linux":
        raise ValueError("worker manifest operatingSystem is not linux")
    if not expect_string(worker_runtime.get("imageDigest"), "workerRuntime.imageDigest"):
        raise ValueError("worker manifest omitted imageDigest")
    for field in ("mode", "supervisorVersion", "probeVersion", "probeSha256", "supervisorIdentity", "providerIdentity"):
        manifest_value = containment.get(field)
        preflight_value = preflight.get(field)
        if field == "probeVersion":
            if expect_int(manifest_value, "workerRuntime.processContainment.probeVersion") != expect_int(
                preflight_value,
                "preflight.probeVersion",
            ):
                raise ValueError("worker manifest probeVersion does not match live preflight")
        elif expect_string(manifest_value, "workerRuntime.processContainment." + field) != expect_string(
            preflight_value,
            "preflight." + field,
        ):
            raise ValueError(f"worker manifest {field} does not match live preflight")
    if (
        expect_string(
            containment.get("supervisorVersion"),
            "workerRuntime.processContainment.supervisorVersion",
        )
        != SUPPORTED_PROTECTED_CGROUP_SUPERVISOR
    ):
        raise ValueError("worker manifest protected cgroup supervisorVersion is unsupported")
    if expect_int(containment.get("probeVersion"), "workerRuntime.processContainment.probeVersion") != SUPPORTED_PROTECTED_CGROUP_PROBE_VERSION:
        raise ValueError("worker manifest protected cgroup probeVersion is unsupported")
    attestation = containment.get("attestation")
    if not isinstance(attestation, Mapping):
        raise ValueError("worker manifest omitted process containment attestation")
    if attestation != preflight.get("attestation"):
        raise ValueError("worker manifest attestation does not match live preflight")
    if expect_string(attestation.get("keyId"), "worker attestation keyId") != policy.key_id:
        raise ValueError("worker manifest attestation keyId does not match the trusted policy")
    preflight_public_key = expect_string(
        preflight.get("attestationPublicKeyBase64"), "preflight.attestationPublicKeyBase64"
    )
    if preflight_public_key != policy.public_key_base64:
        raise ValueError("live preflight public key does not match the trusted processContainmentPolicy")
    statement = build_attestation_statement(worker_runtime, containment, registration_context)
    verifier(policy.public_key, attestation, statement)


def build_attestation_statement(
    worker_runtime: Mapping[str, Any],
    containment: Mapping[str, Any],
    registration_context: Mapping[str, Any],
) -> dict[str, Any]:
    return {
        "executionTargetId": expect_string(registration_context.get("executionTargetId"), "registrationContext.executionTargetId"),
        "targetKind": expect_string(registration_context.get("targetKind"), "registrationContext.targetKind"),
        "instanceUid": expect_string(registration_context.get("instanceUid"), "registrationContext.instanceUid"),
        "clusterId": expect_string(registration_context.get("clusterId"), "registrationContext.clusterId"),
        "namespace": expect_string(registration_context.get("namespace"), "registrationContext.namespace"),
        "podName": expect_string(registration_context.get("podName"), "registrationContext.podName"),
        "workerBuildVersion": expect_string(worker_runtime.get("workerBuildVersion"), "workerRuntime.workerBuildVersion"),
        "workerBuildGitSha": expect_string(worker_runtime.get("workerBuildGitSha"), "workerRuntime.workerBuildGitSha"),
        "imageDigest": expect_string(worker_runtime.get("imageDigest"), "workerRuntime.imageDigest"),
        "operatingSystem": expect_string(worker_runtime.get("operatingSystem"), "workerRuntime.operatingSystem"),
        "architecture": expect_string(worker_runtime.get("architecture"), "workerRuntime.architecture"),
        "mode": expect_string(containment.get("mode"), "workerRuntime.processContainment.mode"),
        "supervisorVersion": expect_string(
            containment.get("supervisorVersion"),
            "workerRuntime.processContainment.supervisorVersion",
        ),
        "probeVersion": expect_int(containment.get("probeVersion"), "workerRuntime.processContainment.probeVersion"),
        "probeSha256": expect_string(containment.get("probeSha256"), "workerRuntime.processContainment.probeSha256"),
        "supervisorIdentity": expect_string(
            containment.get("supervisorIdentity"),
            "workerRuntime.processContainment.supervisorIdentity",
        ),
        "providerIdentity": expect_string(
            containment.get("providerIdentity"),
            "workerRuntime.processContainment.providerIdentity",
        ),
    }


def verify_ed25519_attestation(
    public_key: bytes,
    envelope: Mapping[str, Any],
    statement: Mapping[str, Any],
) -> None:
    if len(public_key) != 32:
        raise ValueError("trusted process containment public key must be 32 bytes")
    if expect_int(envelope.get("schemaVersion"), "worker attestation schemaVersion") != 1:
        raise ValueError("worker manifest attestation schemaVersion is invalid")
    key_id = expect_string(envelope.get("keyId"), "worker attestation keyId")
    signature_base64 = expect_string(envelope.get("signature"), "worker attestation signature")
    signature = decode_canonical_base64(signature_base64, "worker attestation signature")
    payload = SIGNATURE_DOMAIN + json.dumps(
        {
            "schemaVersion": 1,
            "keyId": key_id,
            "statement": statement,
        },
        separators=(",", ":"),
    ).encode("utf-8")
    public_key_pem = pem_encode_public_key(ed25519_subject_public_key_info(public_key))
    with tempfile.TemporaryDirectory(prefix="synara-protected-cgroup-gate-") as temporary_directory:
        temporary_path = pathlib.Path(temporary_directory)
        public_key_path = temporary_path / "public-key.pem"
        payload_path = temporary_path / "payload.bin"
        signature_path = temporary_path / "signature.bin"
        public_key_path.write_text(public_key_pem, encoding="utf-8")
        payload_path.write_bytes(payload)
        signature_path.write_bytes(signature)
        subprocess.run(
            [
                "openssl",
                "pkeyutl",
                "-verify",
                "-pubin",
                "-inkey",
                str(public_key_path),
                "-rawin",
                "-in",
                str(payload_path),
                "-sigfile",
                str(signature_path),
            ],
            check=True,
            capture_output=True,
            text=True,
        )


def ed25519_subject_public_key_info(public_key: bytes) -> bytes:
    return bytes.fromhex("302a300506032b6570032100") + public_key


def pem_encode_public_key(der_bytes: bytes) -> str:
    encoded = base64.b64encode(der_bytes).decode("ascii")
    lines = [encoded[index:index + 64] for index in range(0, len(encoded), 64)]
    return "-----BEGIN PUBLIC KEY-----\n" + "\n".join(lines) + "\n-----END PUBLIC KEY-----\n"


def parse_identity(value: str) -> dict[str, int]:
    match = IDENTITY_PATTERN.fullmatch(value)
    if match is None:
        raise ValueError(f"invalid protected-cgroup identity {value!r}")
    return {"uid": int(match.group(1)), "gid": int(match.group(2))}


def projected_process_containment(payload: Mapping[str, Any]) -> Mapping[str, Any] | None:
    candidates = [payload.get("processContainment")]
    for key in ("projection", "workerManifestProjection"):
        nested = payload.get(key)
        if isinstance(nested, Mapping):
            candidates.append(nested.get("processContainment"))
    for candidate in candidates:
        if isinstance(candidate, Mapping):
            return candidate
    return None


def decode_standard_or_raw_standard_base64(value: str, label: str) -> bytes:
    try:
        return base64.b64decode(value, validate=True)
    except (ValueError, binascii.Error):
        if "-" in value or "_" in value:
            raise ValueError(f"{label} must use standard base64 encoding")
        padding = "=" * ((4 - len(value) % 4) % 4)
        try:
            return base64.b64decode(value + padding, validate=True)
        except (ValueError, binascii.Error) as error:
            raise ValueError(f"{label} must be valid base64") from error


def decode_canonical_base64(value: str, label: str) -> bytes:
    try:
        return base64.b64decode(value, validate=True)
    except (ValueError, binascii.Error) as error:
        raise ValueError(f"{label} must be canonical base64") from error


def expect_bool(value: Any, label: str) -> bool:
    if not isinstance(value, bool):
        raise ValueError(f"{label} must be a boolean")
    return value


def validate_ssh_host(value: str) -> str:
    host = value.strip()
    if not SSH_HOST_PATTERN.fullmatch(host) or host.startswith("-"):
        raise ValueError("--ssh-host must be a safe hostname or IP literal without shell metacharacters")
    return host


def validate_ssh_account(value: str, option: str) -> str:
    account = value.strip()
    if not SSH_ACCOUNT_PATTERN.fullmatch(account):
        raise ValueError(f"{option} must be a safe SSH account name")
    return account


def validate_systemd_service_name(value: str) -> str:
    service = value.strip()
    if not SYSTEMD_SERVICE_PATTERN.fullmatch(service):
        raise ValueError("--service-name must be a safe .service unit name")
    return service


def validate_remote_path(value: str, option: str) -> str:
    path = value.strip()
    if (
        not REMOTE_PATH_PATTERN.fullmatch(path)
        or "//" in path
        or "/../" in path
        or path.endswith("/..")
        or path == "/.."
    ):
        raise ValueError(f"{option} must be a safe absolute path")
    return path


def valid_build_git_sha(value: str) -> bool:
    return 7 <= len(value) <= 64 and all(character in "0123456789abcdef" for character in value)


def valid_image_digest(value: str) -> bool:
    return value.startswith("sha256:") and len(value) == len("sha256:") + 64 and all(
        character in "0123456789abcdef" for character in value[len("sha256:"):]
    )


def expect_int(value: Any, label: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise ValueError(f"{label} must be an integer")
    return value


def expect_string(value: Any, label: str) -> str:
    if not isinstance(value, str) or not value.strip():
        raise ValueError(f"{label} must be a non-empty string")
    return value.strip()


if __name__ == "__main__":
    raise SystemExit(main())
