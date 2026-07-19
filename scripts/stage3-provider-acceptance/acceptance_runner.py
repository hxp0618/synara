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
import contextlib
import dataclasses
import datetime as dt
import hashlib
import http.client
import http.cookiejar
import http.server
import io
import ipaddress
import json
import os
import pathlib
import re
import shlex
import shutil
import signal
import socket
import sqlite3
import stat
import subprocess
import sys
import tempfile
import threading
import time
import tarfile
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
STANDALONE_GENERATED_FILE_RELATIVE_PATH = (
    ".synara-stage3-standalone-generated-file.txt"
)
STANDALONE_GENERATED_FILE_CONTENT = b"SYNARA_STAGE3_STANDALONE_GENERATED_FILE_V1\n"
STANDALONE_GENERATED_FILE_DOWNLOAD_MAX_BYTES = len(STANDALONE_GENERATED_FILE_CONTENT) + 1
GENERATED_FILE_RELATIVE_PATH = ".synara-stage3-acceptance/generated-file.txt"
GENERATED_FILE_TOTAL_BYTES = (1 << 20) + 257
GENERATED_FILE_SNAPSHOT_MAX_BYTES = 4 << 20
LARGE_DIFF_RELATIVE_PATH = ".synara-stage3-large-diff.txt"
LARGE_DIFF_LINE_COUNT = 5_000
LARGE_DIFF_DOWNLOAD_MAX_BYTES = 4 << 20
REAL_PROVIDER_APPROVAL_RELATIVE_PATH = ".synara-real-provider-approval.txt"
REAL_PROVIDER_APPROVAL_CONTENT = b"SYNARA_REAL_PROVIDER_APPROVAL_TOOL_OK\n"
REAL_PROVIDER_STEER_RELATIVE_PATH = ".synara-real-provider-steer.txt"
REAL_PROVIDER_STEER_CONTENT = b"SYNARA_REAL_PROVIDER_STEER_TOOL_OK\n"
REAL_PROVIDER_INTERRUPT_CONTENT = b"SYNARA_REAL_PROVIDER_INTERRUPT_TOOL_SHOULD_NOT_RUN\n"
TERMINAL_LARGE_TOTAL_BYTES = 2 * (1 << 20) + 257
TERMINAL_LARGE_CHUNK_BYTES = 63 << 10
TERMINAL_LOG_PREVIEW_BYTES = 32 << 10
TERMINAL_LOG_SEGMENT_BYTES = 1 << 20
TERMINAL_LARGE_PATTERN = b"0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ._-"
DOCKER_VOLUME_SENTINEL_PATH = "/data/.synara-stage3-provider-acceptance-volume"
DOCKER_VOLUME_SENTINEL_VALUE = "synara-stage3-named-volume-continuity-v1"
SSH_REMOTE_FIXTURE_PATH = "/opt/synara/acceptance/provider-host-fixture.mjs"
SSH_REMOTE_PROVIDER_HOST_PATH = "/opt/synara/provider-host/index.mjs"
SSH_PROVIDER_HOST_COMMAND_PATH = "/usr/local/bin/provider-host"
SSH_REMOTE_PROVIDER_TOOLS_ROOT = "/opt/synara/provider-tools"
SSH_SERVICE_USER = "synara"
SSH_RELAY_LOOPBACK_HOST = "127.0.0.1"
SSH_RELAY_TRANSPORT = "runner-owned reverse SSH relay to the local Worker-only proxy"
SSH_CONTROL_PLANE_OPERATION_TIMEOUT = 180.0
SSH_EXTERNAL_RECOVERY_OPERATION_TIMEOUT = 60.0
SSH_EXTERNAL_NODE_DOWNLOAD_CURL_ARGUMENTS = (
    "--fail",
    "--location",
    "--silent",
    "--show-error",
    "--remote-name",
    "--ipv4",
    "--http1.1",
    "--retry",
    "5",
    "--retry-all-errors",
    "--retry-delay",
    "2",
    "--connect-timeout",
    "15",
    "--max-time",
    "300",
    "--retry-max-time",
    "600",
)
SSH_CREDENTIAL_LIFECYCLE = (
    "runner posts the one-time private key once during Target creation, deletes the local plaintext copy after "
    "provisioning, and relies on the Control Plane encrypted credential until ssh/revoke"
)
SSH_EXTERNAL_CREDENTIAL_LIFECYCLE = (
    "runner reads one repository-external identity only for strict pinned-Host-Key transport and Target creation, "
    "clears the active driver reference after provisioning, retains a run-scoped redaction copy until process exit "
    "to scrub later evidence, preserves the operator source, and relies on the Control Plane encrypted credential "
    "until ssh/revoke"
)
DEFAULT_ACCEPTANCE_WORKER_LEASE_TTL = "6s"
DEFAULT_ACCEPTANCE_WORKER_HEARTBEAT_TIMEOUT = "18s"
WORKER_PROXY_ALLOWED_PATH_PREFIXES = ("/v1/workers/", "/v1/artifact-content/")
WORKER_PROXY_MAX_REQUEST_BYTES = 64 << 20
CASE_STATUSES = frozenset({"pass", "unsupported", "skipped", "fail"})
TERMINAL_EVENT_TYPES = frozenset(
    {"execution.completed", "execution.failed", "execution.cancelled", "execution.interrupted"}
)
FIXTURE_SOAK_REQUIRED_EVENT_TYPES = frozenset(
    {
        "turn.created",
        "execution.leased",
        "workspace.ready",
        "execution.started",
        "content.delta",
        "item.started",
        "item.completed",
        "thread.token-usage.updated",
        "workspace.dirty",
        "checkpoint.created",
        "artifact.ready",
        "checkpoint.ready",
        "execution.completed",
    }
)
JSON_REPORT_NAME = "acceptance-report.json"
MARKDOWN_REPORT_NAME = "acceptance-report.md"
PROVIDERS = ("codex", "claudeAgent", "cursor", "antigravity", "grok", "kilo", "opencode", "pi")
FIXTURE_SUPPORTED_PROVIDERS = frozenset({"codex", "claudeAgent"})
REAL_PROVIDER_SMOKE_PROVIDERS = frozenset({"codex", "claudeAgent"})
REAL_PROVIDER_CREDENTIAL_FIELDS = ("apiKey", "authToken")
FIXTURE_CONCURRENCY_PROVIDERS = ("codex", "claudeAgent")
FIXTURE_CONCURRENCY_WORKERS = 2
FIXTURE_LOAD_SESSIONS = 4
FIXTURE_LOAD_DEFAULT_WAVES = 25
FIXTURE_LOAD_MIN_WAVES = 2
FIXTURE_LOAD_MAX_WAVES = 100
FIXTURE_LOAD_DURATION_MAX_WAVES = 10_000
FIXTURE_LOAD_DURATION_MAX_SECONDS = 86_400.0
FIXTURE_LOAD_SUITES = frozenset({"fixture-load", "fixture-load-failure"})
FIXTURE_PARALLEL_DOCKER_SUITES = FIXTURE_LOAD_SUITES | {"fixture-concurrency"}
KUBERNETES_ACCEPTANCE_RESOURCE_CONFIGURATION: Mapping[str, str] = {
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
}
FIXTURE_LOAD_GENERATION_RECOVERY_CASES = ("worker-network", "worker-container-loss")
FIXTURE_LOAD_FAILURE_CASES = (
    *FIXTURE_LOAD_GENERATION_RECOVERY_CASES,
    "provider-host-process-crash",
)
FIXTURE_RETENTION_SWEEP_INTERVAL = "250ms"
FIXTURE_RETENTION_DAYS = 1
FIXTURE_RETENTION_AGE_DAYS = 2
KUBERNETES_CLEANUP_MAX_ATTEMPTS = 3
KUBERNETES_CLEANUP_RETRY_DELAYS_SECONDS = (1.0, 2.0)
SUITES = (
    "fixture",
    "fixture-soak",
    "fixture-concurrency",
    "fixture-load",
    "fixture-load-failure",
    "fixture-retention-concurrency",
    "real-provider-smoke",
)
REAL_PROVIDER_PRE_RESTART_CASES = (
    "approval",
    "user-input",
    "steer",
    "interrupt",
    "generated-file-checkpoint",
    "large-diff",
    "terminal-large",
)
REAL_PROVIDER_POST_RESTART_CASES = ("review", "compact", "rollback", "fork")
REAL_PROVIDER_CASES = REAL_PROVIDER_PRE_RESTART_CASES + REAL_PROVIDER_POST_RESTART_CASES
REAL_PROVIDER_FAILURE_CASES = (
    "authentication",
    "rate-limit-retry",
    "provider-host-crash-retry",
    "cursor-expiry",
)
REAL_PROVIDER_HTTP_FAULT_TARGETS = ("local", "ssh", "docker", "kubernetes")
REAL_PROVIDER_HOST_CRASH_TARGETS = ("local", "ssh", "docker", "kubernetes")
REAL_PROVIDER_CURSOR_MAX_AGE = "1s"
REAL_PROVIDER_CURSOR_EXPIRY_WAIT_SECONDS = 1.25
REAL_PROVIDER_MAX_SEQUENTIAL_APPROVALS = 4
REAL_PROVIDER_MAX_RECOVERY_LEASE_GENERATIONS = 3
REAL_PROVIDER_CASE_METADATA: Mapping[str, Mapping[str, str]] = {
    "generated-file-checkpoint": {
        "id": "real-provider.generated-file-checkpoint",
        "name": "Capture a real Provider generated file as a standalone Artifact and ready Workspace Checkpoint",
    },
    "large-diff": {
        "id": "real-provider.large-diff-artifact",
        "name": "Persist a real Provider large Turn Diff as a Ready Artifact reference",
    },
    "approval": {
        "id": "real-provider.approval-resolution",
        "name": "Resolve a real Provider tool Approval through the user API",
    },
    "user-input": {
        "id": "real-provider.user-input-resolution",
        "name": "Resolve real Provider structured user input through the user API",
    },
    "steer": {
        "id": "real-provider.steer-active-turn",
        "name": "Steer a real Provider Turn while Provider work is active",
    },
    "interrupt": {
        "id": "real-provider.interrupt-active-turn",
        "name": "Interrupt a real Provider Turn and verify immediate recovery",
    },
    "terminal-large": {
        "id": "real-provider.terminal-large-log",
        "name": "Persist a real Provider large Terminal stream as preview and segmented Artifacts",
    },
    "review": {
        "id": "real-provider.review",
        "name": "Run the real Provider Review operation through the queued Execution path",
    },
    "compact": {
        "id": "real-provider.compact-boundary",
        "name": "Verify native Compact or the explicit Provider unsupported boundary",
    },
    "rollback": {
        "id": "real-provider.rollback-emulation",
        "name": "Rollback logical Session history without claiming a Worker",
    },
    "fork": {
        "id": "real-provider.fork-emulation",
        "name": "Fork logical Session history and continue through authoritative reconstruction",
    },
}
REAL_PROVIDER_FAILURE_CASE_METADATA: Mapping[str, Mapping[str, str]] = {
    "authentication": {
        "id": "real-provider.failure-authentication",
        "name": "Classify a real Provider HTTP authentication failure without leaking its Credential",
    },
    "rate-limit-retry": {
        "id": "real-provider.failure-rate-limit-retry",
        "name": "Classify real Provider HTTP rate limiting and recover through a new Execution",
    },
    "provider-host-crash-retry": {
        "id": "real-provider.failure-host-crash-retry",
        "name": "Kill the scoped real Provider Host mid-Turn and recover through a new Execution",
    },
    "cursor-expiry": {
        "id": "real-provider.failure-cursor-expiry",
        "name": "Expire the authenticated Provider Cursor before restart continuity",
    },
}
FAILURE_CASES = (
    "provider-malformed",
    "provider-oversized",
    "provider-crash",
    "worker-network",
    "kubernetes-drain",
    "kubernetes-pdb-drain",
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
        "kubernetes-pdb-drain",
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
    "kubernetes-pdb-drain": {
        "id": "failure.kubernetes-pdb-node-drain",
        "name": "Honor an exact PodDisruptionBudget before multi-node drain recovery",
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


def duration_distribution_ms(values: Sequence[int]) -> dict[str, int | float]:
    if not values:
        raise ValueError("duration distribution requires at least one sample")
    if any(value < 0 for value in values):
        raise ValueError("duration distribution samples must be non-negative")
    ordered = sorted(values)

    def nearest_rank(percentile: int) -> int:
        index = max(0, ((len(ordered) * percentile + 99) // 100) - 1)
        return ordered[index]

    return {
        "sampleCount": len(ordered),
        "minimum": ordered[0],
        "maximum": ordered[-1],
        "average": round(sum(ordered) / len(ordered), 3),
        "p50": nearest_rank(50),
        "p95": nearest_rank(95),
        "p99": nearest_rank(99),
    }


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


def node_eval_command_prefix(node_executable: str) -> str:
    executable = node_executable.strip()
    if not executable or any(character in executable for character in "\r\n\x00"):
        raise ValueError("Node executable must be a non-empty command or path")
    return f"{shlex.quote(executable)} -e '"


def terminal_large_node_command(node_executable: str = "node") -> str:
    pattern = TERMINAL_LARGE_PATTERN.decode("ascii")
    return node_eval_command_prefix(node_executable) + (
        f'const p=Buffer.from("{pattern}");'
        f"const n={TERMINAL_LARGE_TOTAL_BYTES};"
        "const b=Buffer.allocUnsafe(n);"
        "for(let i=0;i<n;i++)b[i]=p[i%p.length];"
        "process.stdout.write(b)'"
    )


def real_provider_read_only_output_command(content: bytes) -> str:
    output = json.dumps(content.decode("ascii"))
    return f"node -e 'process.stdout.write({output})'"


def real_provider_approval_command() -> str:
    return real_provider_read_only_output_command(REAL_PROVIDER_APPROVAL_CONTENT)


def real_provider_host_crash_command() -> str:
    return real_provider_approval_command()


def real_provider_approval_tool_prompt(command: str) -> str:
    return (
        "Use the Bash or shell tool exactly once. Do not emit any assistant text before the tool call. "
        "Run this exact read-only Node command as the sole shell command:\n"
        f"{command}\n"
        "Do not add redirection, pipes, wrappers, or file changes."
    )


def real_provider_host_crash_approval_prompt() -> str:
    return real_provider_approval_tool_prompt(real_provider_host_crash_command())


def real_provider_approval_command_matches(candidate: str, expected: str) -> bool:
    if candidate == expected:
        return True
    try:
        command = shlex.split(candidate)
    except ValueError:
        return False
    return (
        len(command) == 3
        and pathlib.PurePosixPath(command[0]).name in {"bash", "sh", "zsh"}
        and command[1] in {"-c", "-lc"}
        and command[2] == expected
    )


def real_provider_steer_command() -> str:
    return real_provider_read_only_output_command(REAL_PROVIDER_STEER_CONTENT)


def real_provider_interrupt_command() -> str:
    return real_provider_read_only_output_command(REAL_PROVIDER_INTERRUPT_CONTENT)


def real_provider_approval_gated_prompt(command: str, marker: str) -> str:
    return (
        f"{real_provider_approval_tool_prompt(command)} "
        "After the tool finishes, the complete "
        f"assistant text for this Turn must be exactly {marker} and no other text."
    )


def real_provider_approval_prompt(marker: str) -> str:
    return real_provider_approval_gated_prompt(real_provider_approval_command(), marker)


def real_provider_steer_prompt(marker: str) -> str:
    return real_provider_approval_gated_prompt(real_provider_steer_command(), marker)


def real_provider_interrupt_prompt(marker: str) -> str:
    return real_provider_approval_gated_prompt(real_provider_interrupt_command(), marker)


def real_provider_user_input_prompt(provider: str, marker: str) -> str:
    tool_name = {
        "codex": "request_user_input",
        "claudeAgent": "AskUserQuestion",
    }.get(provider)
    if tool_name is None:
        raise ValueError(f"unsupported real Provider user-input tool: {provider}")
    return (
        f"Call the {tool_name} tool as your very next action and call no other tool. "
        "Do not emit any assistant text before the tool call and do not answer the question yourself. "
        "Ask exactly one question with header 'Environment', question "
        "'Which environment should this acceptance use?', and exactly two options labeled 'Staging' and "
        "'Production'. Do not call ExitPlanMode. After the tool returns the user's answer, the complete "
        f"assistant text after the tool call must be exactly {marker} and no other text."
    )


def generated_file_bytes() -> bytes:
    return terminal_large_bytes(0, GENERATED_FILE_TOTAL_BYTES)


def generated_file_node_command(node_executable: str = "node") -> str:
    pattern = TERMINAL_LARGE_PATTERN.decode("ascii")
    return node_eval_command_prefix(node_executable) + (
        'const f=require("node:fs");'
        f'const p="{GENERATED_FILE_RELATIVE_PATH}";'
        'f.mkdirSync(".synara-stage3-acceptance",{recursive:true});'
        f'const s=Buffer.from("{pattern}");'
        f"const n={GENERATED_FILE_TOTAL_BYTES};"
        "const b=Buffer.allocUnsafe(n);"
        "for(let i=0;i<n;i++)b[i]=s[i%s.length];"
        "f.writeFileSync(p,b)'"
    )


def large_diff_seed_bytes() -> bytes:
    return (
        "".join(
            f"SYNARA_STAGE3_LARGE_DIFF_BEFORE_{index:05d}_{'x' * 24}\n"
            for index in range(LARGE_DIFF_LINE_COUNT)
        )
    ).encode("ascii")


def large_diff_seed_node_command(node_executable: str = "node") -> str:
    return node_eval_command_prefix(node_executable) + (
        'const f=require("node:fs");'
        f'const p="{LARGE_DIFF_RELATIVE_PATH}";'
        f"const n={LARGE_DIFF_LINE_COUNT};"
        'const l=[];for(let i=0;i<n;i++)l.push("SYNARA_STAGE3_LARGE_DIFF_BEFORE_"+'
        'String(i).padStart(5,"0")+"_"+"x".repeat(24));'
        'f.writeFileSync(p,l.join("\\n")+"\\n")'
        "'"
    )


def generated_file_snapshot_evidence(snapshot: bytes) -> dict[str, Any]:
    if len(snapshot) > GENERATED_FILE_SNAPSHOT_MAX_BYTES:
        raise AcceptanceError(
            "runner.generated_file_snapshot_oversized",
            "The generated-file Workspace Snapshot exceeded the acceptance download limit.",
            {"snapshotBytes": len(snapshot), "maximumBytes": GENERATED_FILE_SNAPSHOT_MAX_BYTES},
        )
    try:
        archive = tarfile.open(fileobj=io.BytesIO(snapshot), mode="r:*")
    except (tarfile.TarError, OSError):
        raise AcceptanceError(
            "runner.generated_file_snapshot_invalid",
            "The generated-file Workspace Snapshot was not a valid Tar archive.",
        ) from None

    with archive:
        members = archive.getmembers()
        regular_members: list[tarfile.TarInfo] = []
        normalized_names: set[str] = set()
        allowed_directory_names = {
            pathlib.PurePosixPath(GENERATED_FILE_RELATIVE_PATH).parent.as_posix(),
        }
        for member in members:
            path = pathlib.PurePosixPath(member.name)
            normalized = path.as_posix()
            if (
                path.is_absolute()
                or "\\" in member.name
                or re.match(r"^[A-Za-z]:", member.name) is not None
                or ".." in path.parts
                or normalized in {"", "."}
                or normalized in normalized_names
                or member.issym()
                or member.islnk()
                or not (member.isfile() or member.isdir())
                or (member.isdir() and normalized not in allowed_directory_names)
            ):
                raise AcceptanceError(
                    "runner.generated_file_snapshot_unsafe",
                    "The generated-file Workspace Snapshot contained an unsafe or duplicate member.",
                    {"memberName": member.name, "memberType": member.type.decode("ascii", errors="replace")},
                )
            normalized_names.add(normalized)
            if member.isfile():
                regular_members.append(member)

        regular_by_name = {
            pathlib.PurePosixPath(member.name).as_posix(): member for member in regular_members
        }
        known_runner_files = {
            STANDALONE_GENERATED_FILE_RELATIVE_PATH: STANDALONE_GENERATED_FILE_CONTENT,
            REAL_PROVIDER_APPROVAL_RELATIVE_PATH: REAL_PROVIDER_APPROVAL_CONTENT,
            REAL_PROVIDER_STEER_RELATIVE_PATH: REAL_PROVIDER_STEER_CONTENT,
        }
        allowed_regular_files = {GENERATED_FILE_RELATIVE_PATH, *known_runner_files}
        required_regular_files = {
            GENERATED_FILE_RELATIVE_PATH,
            STANDALONE_GENERATED_FILE_RELATIVE_PATH,
        }
        unexpected_regular_files = sorted(set(regular_by_name).difference(allowed_regular_files))
        missing_regular_files = sorted(required_regular_files.difference(regular_by_name))
        if missing_regular_files or unexpected_regular_files:
            raise AcceptanceError(
                "runner.generated_file_snapshot_shape_invalid",
                "The generated-file Workspace Snapshot omitted its payload or contained an unexpected file.",
                {
                    "memberCount": len(members),
                    "regularFileCount": len(regular_members),
                    "regularFileNames": sorted(regular_by_name),
                    "missingRegularFileNames": missing_regular_files,
                    "unexpectedRegularFileNames": unexpected_regular_files,
                },
            )
        member = regular_by_name[GENERATED_FILE_RELATIVE_PATH]
        if member.size != GENERATED_FILE_TOTAL_BYTES:
            raise AcceptanceError(
                "runner.generated_file_snapshot_member_invalid",
                "The Workspace Snapshot generated file had the wrong size.",
                {
                    "actualBytes": member.size,
                    "expectedBytes": GENERATED_FILE_TOTAL_BYTES,
                },
            )
        reader = archive.extractfile(member)
        if reader is None:
            raise AcceptanceError(
                "runner.generated_file_snapshot_content_missing",
                "The generated file could not be read from the Workspace Snapshot.",
            )
        content = reader.read(GENERATED_FILE_TOTAL_BYTES + 1)

        preserved_known_files: list[str] = []
        for known_path, expected_content in known_runner_files.items():
            known_member = regular_by_name.get(known_path)
            if known_member is None:
                continue
            known_reader = archive.extractfile(known_member)
            actual_content = (
                known_reader.read(len(expected_content) + 1) if known_reader is not None else b""
            )
            if actual_content != expected_content:
                raise AcceptanceError(
                    "runner.generated_file_snapshot_prior_file_invalid",
                    "The generated-file Snapshot did not preserve a known runner sentinel exactly.",
                    {
                        "path": known_path,
                        "actualBytes": len(actual_content),
                        "expectedBytes": len(expected_content),
                    },
                )
            preserved_known_files.append(known_path)

    expected = generated_file_bytes()
    if content != expected:
        raise AcceptanceError(
            "runner.generated_file_snapshot_content_mismatch",
            "The generated file content did not match the deterministic acceptance payload.",
            {
                "actualBytes": len(content),
                "expectedBytes": len(expected),
                "actualSha256": hashlib.sha256(content).hexdigest(),
                "expectedSha256": hashlib.sha256(expected).hexdigest(),
            },
        )
    return {
        "archiveBytes": len(snapshot),
        "archiveSha256": hashlib.sha256(snapshot).hexdigest(),
        "memberCount": len(members),
        "regularFileCount": len(regular_members),
        "preservedKnownFiles": preserved_known_files,
        "file": {
            "path": GENERATED_FILE_RELATIVE_PATH,
            "sizeBytes": len(content),
            "sha256": hashlib.sha256(content).hexdigest(),
        },
    }


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
        "runtime-output" in normalized.casefold()
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


def json_object_array(value: Any, label: str) -> list[dict[str, Any]]:
    if not isinstance(value, list) or not all(isinstance(item, dict) for item in value):
        raise AcceptanceError("runner.response_shape_invalid", f"{label} was not an object array.")
    return value


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
    soak_turns: int
    soak_restart_every: int
    load_waves: int
    load_min_duration_seconds: float
    load_max_waves: int
    worker_lease_ttl: str
    worker_heartbeat_timeout: str
    ssh_orbctl_bin: str
    ssh_machine_name: str | None
    ssh_machine_arch: str
    ssh_machine_image: str
    ssh_node_version: str
    ssh_external_host: str | None
    ssh_external_port: int
    ssh_external_user: str | None
    ssh_external_identity_file: pathlib.Path | None
    ssh_external_host_key_file: pathlib.Path | None
    ssh_external_service_user: str
    ssh_external_use_sudo: bool
    ssh_allow_external_host: bool
    docker_socket_path: pathlib.Path
    docker_worker_image: str | None
    docker_skip_worker_build: bool
    docker_control_plane_host: str
    docker_network_mode: str | None
    docker_memory_bytes: int
    docker_nano_cpus: int
    kubernetes_context: str | None
    kubernetes_kubeconfig: pathlib.Path | None
    kubernetes_api_server: str | None
    kubernetes_tls_server_name: str | None
    kubernetes_allow_nondisposable: bool
    kubernetes_shared_local_image_store: bool
    kubernetes_worker_image: str | None
    kubernetes_skip_worker_build: bool
    kubernetes_control_plane_host: str
    kind_bin: str
    kind_cluster_name: str | None
    kind_node_image: str
    kind_worker_nodes: int
    failure_cases: tuple[str, ...]
    network_outage_seconds: float
    docker_allow_network_interruption: bool
    kubernetes_allow_node_drain: bool
    failure_only: bool
    real_provider_cases: tuple[str, ...]
    real_provider_failure_cases: tuple[str, ...]
    real_provider_credential_env: str | None
    real_provider_credential_field: str
    real_provider_base_url_env: str | None
    real_provider_model: str | None


def fixture_load_resource_profile(options: RunnerOptions) -> dict[str, Any]:
    profile: dict[str, Any] = {
        "target": options.target,
        "tenantMaxConcurrentExecutions": FIXTURE_CONCURRENCY_WORKERS,
        "workers": FIXTURE_CONCURRENCY_WORKERS,
        "activeExecutionSlotsPerWorker": 1,
    }
    if options.target == "docker":
        profile["dockerWorker"] = {
            "memoryBytes": options.docker_memory_bytes,
            "nanoCpus": options.docker_nano_cpus,
        }
    elif options.target == "kubernetes":
        profile["kubernetesWorker"] = dict(KUBERNETES_ACCEPTANCE_RESOURCE_CONFIGURATION)
    return profile


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
    pending_user_input: dict[str, Any] | None = None
    pending_real_turn_id: str | None = None
    last_real_marker: str | None = None
    rollback_anchor_turn_id: str | None = None


@dataclasses.dataclass(frozen=True)
class LocalRetentionSeed:
    tenant_id: str
    organization_id: str
    project_id: str
    session_id: str
    target_id: str
    workspace_id: str
    materialization_id: str
    incarnation_id: str
    layout_version: int
    current_checkpoint_id: str
    checkpoint_artifact_id: str
    generated_artifact_id: str
    generated_artifact_execution_id: str
    generated_object_path: pathlib.Path
    checkpoint_object_path: pathlib.Path
    workspace_generation_path: pathlib.Path

    def evidence(self) -> dict[str, Any]:
        return {
            "tenantId": self.tenant_id,
            "organizationId": self.organization_id,
            "projectId": self.project_id,
            "sessionId": self.session_id,
            "executionTargetId": self.target_id,
            "workspaceId": self.workspace_id,
            "materializationId": self.materialization_id,
            "incarnationId": self.incarnation_id,
            "layoutVersion": self.layout_version,
            "currentCheckpointId": self.current_checkpoint_id,
            "checkpointArtifactId": self.checkpoint_artifact_id,
            "generatedArtifactId": self.generated_artifact_id,
            "generatedArtifactExecutionId": self.generated_artifact_execution_id,
            "physicalPathsPersisted": False,
        }


class LocalRetentionHarness:
    def __init__(self, state_dir: pathlib.Path) -> None:
        self.state_dir = state_dir
        self.database_path = state_dir / "metadata.sqlite"
        self.artifact_root = state_dir / "artifacts"
        self.workspace_root = state_dir / "workspaces"

    def _connect(self) -> sqlite3.Connection:
        if not self.database_path.is_file():
            raise AcceptanceError(
                "runner.retention_database_missing",
                "The isolated Local metadata database was unavailable for retention staging.",
            )
        connection = sqlite3.connect(self.database_path, timeout=5.0)
        connection.row_factory = sqlite3.Row
        connection.execute("PRAGMA busy_timeout = 5000")
        return connection

    @staticmethod
    def _required_text(row: sqlite3.Row, key: str, label: str) -> str:
        value = row[key]
        if not isinstance(value, str) or not value:
            raise AcceptanceError(
                "runner.retention_seed_invalid",
                f"The retention {label} omitted {key}.",
            )
        return value

    @staticmethod
    def _single_row(
        rows: Sequence[sqlite3.Row],
        label: str,
    ) -> sqlite3.Row:
        if len(rows) != 1:
            raise AcceptanceError(
                "runner.retention_seed_ambiguous",
                f"Expected exactly one {label} for the retention fixture.",
                {"count": len(rows)},
            )
        return rows[0]

    @staticmethod
    def _scoped_local_path(root: pathlib.Path, object_key: str, label: str) -> pathlib.Path:
        pure = pathlib.PurePosixPath(object_key)
        if pure.is_absolute() or object_key != pure.as_posix() or ".." in pure.parts:
            raise AcceptanceError(
                "runner.retention_object_key_invalid",
                f"The retention {label} object key was unsafe.",
            )
        resolved_root = root.resolve()
        resolved = (resolved_root / pathlib.Path(*pure.parts)).resolve()
        if resolved == resolved_root or resolved_root not in resolved.parents:
            raise AcceptanceError(
                "runner.retention_object_key_invalid",
                f"The retention {label} object key escaped the isolated store.",
            )
        return resolved

    def load_seed(self, session_id: str) -> LocalRetentionSeed:
        try:
            with contextlib.closing(self._connect()) as connection:
                scope_rows = connection.execute(
                    """
                    SELECT
                        workspace.tenant_id,
                        workspace.organization_id,
                        workspace.project_id,
                        workspace.session_id,
                        workspace.execution_target_id,
                        workspace.id AS workspace_id,
                        materialization.id AS materialization_id,
                        materialization.incarnation_id,
                        materialization.layout_version,
                        checkpoint.id AS checkpoint_id,
                        checkpoint.artifact_id AS checkpoint_artifact_id
                    FROM remote_workspaces AS workspace
                    JOIN workspace_materializations AS materialization
                      ON materialization.tenant_id = workspace.tenant_id
                     AND materialization.id = workspace.current_materialization_id
                    JOIN workspace_checkpoints AS checkpoint
                      ON checkpoint.tenant_id = workspace.tenant_id
                     AND checkpoint.id = workspace.current_checkpoint_id
                    WHERE workspace.session_id = ?
                    """,
                    (session_id,),
                ).fetchall()
                scope = self._single_row(scope_rows, "current Workspace materialization and Checkpoint")
                generated_rows = connection.execute(
                    """
                    SELECT id, execution_id, object_key
                    FROM artifacts
                    WHERE session_id = ?
                      AND kind = 'generated_file'
                      AND original_name = 'artifact.txt'
                      AND status = 'ready'
                      AND deleted_at IS NULL
                    ORDER BY ready_at, id
                    """,
                    (session_id,),
                ).fetchall()
                generated = self._single_row(generated_rows, "ready fixture generated Artifact")
                checkpoint_artifact_id = self._required_text(
                    scope,
                    "checkpoint_artifact_id",
                    "current Checkpoint",
                )
                checkpoint_rows = connection.execute(
                    """
                    SELECT id, object_key
                    FROM artifacts
                    WHERE id = ?
                      AND workspace_checkpoint_id = ?
                      AND status = 'ready'
                      AND deleted_at IS NULL
                    """,
                    (checkpoint_artifact_id, self._required_text(scope, "checkpoint_id", "current Checkpoint")),
                ).fetchall()
                checkpoint_artifact = self._single_row(
                    checkpoint_rows,
                    "ready current Checkpoint Artifact",
                )
        except sqlite3.Error as error:
            raise AcceptanceError(
                "runner.retention_seed_load_failed",
                f"The isolated retention seed could not be loaded: {error}",
            ) from None

        layout_version = scope["layout_version"]
        if type(layout_version) is not int or layout_version != 3:
            raise AcceptanceError(
                "runner.retention_layout_invalid",
                "The retention fixture requires the current Workspace layout version 3.",
                {"layoutVersion": layout_version},
            )
        tenant_id = self._required_text(scope, "tenant_id", "Workspace")
        project_id = self._required_text(scope, "project_id", "Workspace")
        target_id = self._required_text(scope, "execution_target_id", "Workspace")
        workspace_id = self._required_text(scope, "workspace_id", "Workspace")
        incarnation_id = self._required_text(scope, "incarnation_id", "materialization")
        generated_object_path = self._scoped_local_path(
            self.artifact_root,
            self._required_text(generated, "object_key", "generated Artifact"),
            "generated Artifact",
        )
        checkpoint_object_path = self._scoped_local_path(
            self.artifact_root,
            self._required_text(checkpoint_artifact, "object_key", "Checkpoint Artifact"),
            "Checkpoint Artifact",
        )
        workspace_generation_path = self.workspace_root.joinpath(
            "v3",
            target_id,
            tenant_id,
            project_id,
            session_id,
            workspace_id,
            incarnation_id,
        )
        for label, path in (
            ("generated Artifact payload", generated_object_path),
            ("Checkpoint Artifact payload", checkpoint_object_path),
            ("Workspace generation", workspace_generation_path),
        ):
            if not path.exists():
                raise AcceptanceError(
                    "runner.retention_seed_payload_missing",
                    f"The retention seed {label} was unavailable before staging.",
                )
        return LocalRetentionSeed(
            tenant_id=tenant_id,
            organization_id=self._required_text(scope, "organization_id", "Workspace"),
            project_id=project_id,
            session_id=self._required_text(scope, "session_id", "Workspace"),
            target_id=target_id,
            workspace_id=workspace_id,
            materialization_id=self._required_text(scope, "materialization_id", "materialization"),
            incarnation_id=incarnation_id,
            layout_version=layout_version,
            current_checkpoint_id=self._required_text(scope, "checkpoint_id", "current Checkpoint"),
            checkpoint_artifact_id=checkpoint_artifact_id,
            generated_artifact_id=self._required_text(generated, "id", "generated Artifact"),
            generated_artifact_execution_id=self._required_text(
                generated,
                "execution_id",
                "generated Artifact",
            ),
            generated_object_path=generated_object_path,
            checkpoint_object_path=checkpoint_object_path,
            workspace_generation_path=workspace_generation_path,
        )

    @staticmethod
    def _database_time(value: dt.datetime) -> str:
        if value.tzinfo is None or value.utcoffset() is None:
            raise ValueError("retention staging time must be timezone-aware")
        return value.astimezone(dt.timezone.utc).isoformat(sep=" ", timespec="microseconds")

    @staticmethod
    def _expect_update(cursor: sqlite3.Cursor, label: str) -> None:
        if cursor.rowcount != 1:
            raise AcceptanceError(
                "runner.retention_stage_scope_changed",
                f"Retention staging did not update exactly one {label}.",
                {"rowsAffected": cursor.rowcount},
            )

    def stage_active_retention(self, seed: LocalRetentionSeed, expired_at: dt.datetime) -> None:
        value = self._database_time(expired_at)
        try:
            with contextlib.closing(self._connect()) as connection:
                connection.execute("BEGIN IMMEDIATE")
                self._expect_update(
                    connection.execute(
                        "UPDATE agent_sessions SET updated_at = ? WHERE tenant_id = ? AND id = ? AND status = 'active'",
                        (value, seed.tenant_id, seed.session_id),
                    ),
                    "active Session",
                )
                self._expect_update(
                    connection.execute(
                        "UPDATE artifacts SET expires_at = ? WHERE tenant_id = ? AND id = ? AND status = 'ready' AND deleted_at IS NULL",
                        (value, seed.tenant_id, seed.generated_artifact_id),
                    ),
                    "ready generated Artifact",
                )
                self._expect_update(
                    connection.execute(
                        """
                        UPDATE workspace_materializations
                        SET cleanup_reason = 'retention-session-archive', cleanup_requested_at = ?, updated_at = ?
                        WHERE tenant_id = ? AND id = ? AND incarnation_id = ? AND state = 'active'
                        """,
                        (
                            value,
                            value,
                            seed.tenant_id,
                            seed.materialization_id,
                            seed.incarnation_id,
                        ),
                    ),
                    "active Workspace materialization",
                )
                self._expect_update(
                    connection.execute(
                        "UPDATE remote_workspaces SET retention_until = ?, updated_at = ? WHERE tenant_id = ? AND id = ?",
                        (value, value, seed.tenant_id, seed.workspace_id),
                    ),
                    "logical Workspace",
                )
                connection.commit()
        except sqlite3.Error as error:
            raise AcceptanceError(
                "runner.retention_stage_failed",
                f"The isolated retention concurrency rows could not be staged: {error}",
            ) from None

    def age_session(self, seed: LocalRetentionSeed, expired_at: dt.datetime) -> None:
        value = self._database_time(expired_at)
        try:
            with contextlib.closing(self._connect()) as connection:
                self._expect_update(
                    connection.execute(
                        "UPDATE agent_sessions SET updated_at = ? WHERE tenant_id = ? AND id = ? AND status = 'active'",
                        (value, seed.tenant_id, seed.session_id),
                    ),
                    "post-terminal active Session",
                )
                connection.commit()
        except sqlite3.Error as error:
            raise AcceptanceError(
                "runner.retention_session_age_failed",
                f"The post-terminal Session could not be aged for retention: {error}",
            ) from None

    def snapshot(
        self,
        seed: LocalRetentionSeed,
        active_execution_id: str,
        interaction_id: str,
    ) -> dict[str, Any]:
        try:
            with contextlib.closing(self._connect()) as connection:
                session = connection.execute(
                    "SELECT status, archived_at FROM agent_sessions WHERE tenant_id = ? AND id = ?",
                    (seed.tenant_id, seed.session_id),
                ).fetchone()
                execution = connection.execute(
                    "SELECT status FROM agent_executions WHERE tenant_id = ? AND id = ?",
                    (seed.tenant_id, active_execution_id),
                ).fetchone()
                interaction = connection.execute(
                    "SELECT status FROM execution_interactions WHERE tenant_id = ? AND id = ?",
                    (seed.tenant_id, interaction_id),
                ).fetchone()
                workspace = connection.execute(
                    "SELECT state, current_checkpoint_id, current_materialization_id, cleaned_at FROM remote_workspaces WHERE tenant_id = ? AND id = ?",
                    (seed.tenant_id, seed.workspace_id),
                ).fetchone()
                materialization = connection.execute(
                    "SELECT state, cleanup_reason, cleanup_requested_at, cleaned_at FROM workspace_materializations WHERE tenant_id = ? AND id = ? AND incarnation_id = ?",
                    (seed.tenant_id, seed.materialization_id, seed.incarnation_id),
                ).fetchone()
                generated = connection.execute(
                    "SELECT status, deleted_at FROM artifacts WHERE tenant_id = ? AND id = ?",
                    (seed.tenant_id, seed.generated_artifact_id),
                ).fetchone()
                seed_checkpoint = connection.execute(
                    "SELECT status, artifact_id FROM workspace_checkpoints WHERE tenant_id = ? AND id = ?",
                    (seed.tenant_id, seed.current_checkpoint_id),
                ).fetchone()
                seed_checkpoint_artifact = connection.execute(
                    "SELECT status, deleted_at FROM artifacts WHERE tenant_id = ? AND id = ?",
                    (seed.tenant_id, seed.checkpoint_artifact_id),
                ).fetchone()
                current_checkpoint = connection.execute(
                    "SELECT id, status, artifact_id FROM workspace_checkpoints WHERE tenant_id = ? AND id = ?",
                    (seed.tenant_id, workspace["current_checkpoint_id"] if workspace is not None else None),
                ).fetchone()
                current_checkpoint_artifact = connection.execute(
                    "SELECT id, status, deleted_at, object_key FROM artifacts WHERE tenant_id = ? AND id = ?",
                    (
                        seed.tenant_id,
                        current_checkpoint["artifact_id"] if current_checkpoint is not None else None,
                    ),
                ).fetchone()
                lease_count = int(
                    connection.execute(
                        "SELECT COUNT(*) FROM worker_leases WHERE tenant_id = ? AND execution_id = ?",
                        (seed.tenant_id, active_execution_id),
                    ).fetchone()[0]
                )
                active_execution_count = int(
                    connection.execute(
                        """
                        SELECT COUNT(*)
                        FROM agent_executions
                        WHERE tenant_id = ?
                          AND workspace_materialization_id = ?
                          AND status NOT IN ('completed', 'failed', 'cancelled', 'interrupted')
                        """,
                        (seed.tenant_id, seed.materialization_id),
                    ).fetchone()[0]
                )
                cleanup_rows = connection.execute(
                    """
                    SELECT id, status, reason, dispatch_generation, delivery_attempts, acknowledged_at
                    FROM workspace_cleanup_commands
                    WHERE tenant_id = ? AND materialization_id = ?
                    ORDER BY created_at, id
                    """,
                    (seed.tenant_id, seed.materialization_id),
                ).fetchall()
        except sqlite3.Error as error:
            raise AcceptanceError(
                "runner.retention_snapshot_failed",
                f"The isolated retention state could not be observed: {error}",
            ) from None
        required = {
            "session": session,
            "execution": execution,
            "interaction": interaction,
            "workspace": workspace,
            "materialization": materialization,
            "generatedArtifact": generated,
            "seedCheckpoint": seed_checkpoint,
            "seedCheckpointArtifact": seed_checkpoint_artifact,
            "currentCheckpoint": current_checkpoint,
            "currentCheckpointArtifact": current_checkpoint_artifact,
        }
        missing = sorted(label for label, row in required.items() if row is None)
        if missing:
            raise AcceptanceError(
                "runner.retention_snapshot_incomplete",
                "The isolated retention state lost required rows.",
                {"missing": missing},
            )
        return {
            "session": {
                "id": seed.session_id,
                "status": session["status"],
                "archived": session["archived_at"] is not None,
            },
            "execution": {
                "id": active_execution_id,
                "status": execution["status"],
                "leaseCount": lease_count,
            },
            "interaction": {
                "id": interaction_id,
                "status": interaction["status"],
            },
            "workspace": {
                "id": seed.workspace_id,
                "state": workspace["state"],
                "currentCheckpointId": workspace["current_checkpoint_id"],
                "currentMaterializationId": workspace["current_materialization_id"],
                "cleaned": workspace["cleaned_at"] is not None,
                "generationPresent": seed.workspace_generation_path.exists(),
            },
            "materialization": {
                "id": seed.materialization_id,
                "state": materialization["state"],
                "cleanupReason": materialization["cleanup_reason"],
                "cleanupRequested": materialization["cleanup_requested_at"] is not None,
                "cleaned": materialization["cleaned_at"] is not None,
                "activeExecutionCount": active_execution_count,
            },
            "generatedArtifact": {
                "id": seed.generated_artifact_id,
                "status": generated["status"],
                "deleted": generated["deleted_at"] is not None,
                "payloadPresent": seed.generated_object_path.is_file(),
            },
            "seedCheckpoint": {
                "id": seed.current_checkpoint_id,
                "status": seed_checkpoint["status"],
                "artifactId": seed_checkpoint["artifact_id"],
            },
            "seedCheckpointArtifact": {
                "id": seed.checkpoint_artifact_id,
                "status": seed_checkpoint_artifact["status"],
                "deleted": seed_checkpoint_artifact["deleted_at"] is not None,
                "payloadPresent": seed.checkpoint_object_path.is_file(),
            },
            "currentCheckpoint": {
                "id": current_checkpoint["id"],
                "status": current_checkpoint["status"],
                "artifactId": current_checkpoint["artifact_id"],
            },
            "currentCheckpointArtifact": {
                "id": current_checkpoint_artifact["id"],
                "status": current_checkpoint_artifact["status"],
                "deleted": current_checkpoint_artifact["deleted_at"] is not None,
                "payloadPresent": self._scoped_local_path(
                    self.artifact_root,
                    self._required_text(
                        current_checkpoint_artifact,
                        "object_key",
                        "current Checkpoint Artifact",
                    ),
                    "current Checkpoint Artifact",
                ).is_file(),
            },
            "cleanupCommands": [
                {
                    "id": row["id"],
                    "status": row["status"],
                    "reason": row["reason"],
                    "dispatchGeneration": row["dispatch_generation"],
                    "deliveryAttempts": row["delivery_attempts"],
                    "acknowledged": row["acknowledged_at"] is not None,
                }
                for row in cleanup_rows
            ],
            "physicalPathsPersisted": False,
        }


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

    def download_bytes(
        self,
        url: str,
        *,
        maximum_bytes: int,
        maximum_timeout: float = 30.0,
    ) -> bytes:
        if maximum_bytes <= 0:
            raise ValueError("maximum_bytes must be positive")
        parsed = urllib.parse.urlsplit(url)
        if parsed.scheme:
            if parsed.scheme not in {"http", "https"} or not parsed.netloc:
                raise AcceptanceError(
                    "runner.artifact_download_url_invalid",
                    "The Artifact download grant returned an unsupported URL.",
                )
            request_url = url
        elif url.startswith("/"):
            request_url = self.base_url + url
        else:
            raise AcceptanceError(
                "runner.artifact_download_url_invalid",
                "The Artifact download grant returned an invalid relative URL.",
            )
        for values in urllib.parse.parse_qs(parsed.query).values():
            for value in values:
                self.redactor.add(value, "[REDACTED_ARTIFACT_URL_VALUE]")
        request = urllib.request.Request(
            request_url,
            headers={
                "Accept": "application/octet-stream",
                "X-Request-ID": f"stage3-acceptance-{uuid.uuid4()}",
            },
            method="GET",
        )
        safe_path = parsed.path or "/"
        try:
            with self.opener.open(
                request,
                timeout=self.deadline.request_timeout(maximum=maximum_timeout),
            ) as response:
                status = int(response.status)
                content_length = response.headers.get("Content-Length")
                if content_length is not None:
                    try:
                        declared_length = int(content_length)
                    except ValueError:
                        raise AcceptanceError(
                            "runner.artifact_download_length_invalid",
                            "The Artifact download returned an invalid Content-Length.",
                            {"path": safe_path},
                        ) from None
                    if declared_length < 0 or declared_length > maximum_bytes:
                        raise AcceptanceError(
                            "runner.artifact_download_oversized",
                            "The Artifact download exceeded the acceptance size limit.",
                            {
                                "path": safe_path,
                                "declaredBytes": declared_length,
                                "maximumBytes": maximum_bytes,
                            },
                        )
                body = response.read(maximum_bytes + 1)
        except urllib.error.HTTPError as error:
            raise AcceptanceError(
                "runner.artifact_download_failed",
                f"GET {safe_path} returned HTTP {int(error.code)}.",
                {"path": safe_path, "status": int(error.code)},
            ) from None
        except AcceptanceError:
            raise
        except (urllib.error.URLError, TimeoutError, OSError) as error:
            raise AcceptanceError(
                "runner.artifact_download_transport_failed",
                f"GET {safe_path} failed: {self.redactor.text(str(error))}",
                {"path": safe_path},
            ) from None
        if status != 200:
            raise AcceptanceError(
                "runner.artifact_download_failed",
                f"GET {safe_path} returned HTTP {status}.",
                {"path": safe_path, "status": status},
            )
        if len(body) > maximum_bytes:
            raise AcceptanceError(
                "runner.artifact_download_oversized",
                "The Artifact download exceeded the acceptance size limit.",
                {"path": safe_path, "maximumBytes": maximum_bytes},
            )
        if content_length is not None and len(body) != int(content_length):
            raise AcceptanceError(
                "runner.artifact_download_incomplete",
                "The Artifact download body did not match Content-Length.",
                {"path": safe_path, "declaredBytes": int(content_length), "actualBytes": len(body)},
            )
        for cookie in self.cookies:
            self.redactor.add(cookie.value, "[REDACTED_SESSION_COOKIE]")
        return body

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

    def real_provider_node_executable(self) -> str: ...

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
        self._provider_fault_routes: dict[str, int] = {}
        self._provider_fault_routes_lock = threading.Lock()
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
                provider_fault_upstream_port = proxy.provider_fault_upstream_port(path)
                if (
                    parsed.scheme
                    or parsed.netloc
                    or len(self.path) > 16_384
                    or "%" in path
                    or "\\" in path
                    or "//" in path
                    or any(segment in {".", ".."} for segment in segments)
                    or (
                        provider_fault_upstream_port is None
                        and not any(path.startswith(prefix) for prefix in WORKER_PROXY_ALLOWED_PATH_PREFIXES)
                    )
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

                allowed_request_headers = {
                    "accept",
                    "authorization",
                    "content-type",
                    "idempotency-key",
                    "user-agent",
                    "x-request-id",
                }
                if provider_fault_upstream_port is not None:
                    allowed_request_headers.update(
                        {
                            "anthropic-beta",
                            "anthropic-version",
                            "openai-beta",
                            "x-api-key",
                        }
                    )
                headers = {
                    name: value
                    for name, value in self.headers.items()
                    if name.lower() in allowed_request_headers
                }
                upstream = http.client.HTTPConnection(
                    "127.0.0.1",
                    provider_fault_upstream_port or proxy.upstream_port,
                    timeout=30.0,
                )
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
                    if provider_fault_upstream_port is not None:
                        allowed_response_headers.update({"retry-after", "x-request-id"})
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
                        self._json_error(
                            502,
                            "provider_fault_unavailable"
                            if provider_fault_upstream_port is not None
                            else "control_plane_unavailable",
                        )
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

    def register_provider_fault_route(self, route_prefix: str, upstream_port: int) -> None:
        if re.fullmatch(r"/[0-9a-f]{32}", route_prefix) is None:
            raise AcceptanceError(
                "runner.provider_fault_route_invalid",
                "Provider fault route registration received an invalid route prefix.",
            )
        if not (1 <= upstream_port <= 65535):
            raise AcceptanceError(
                "runner.provider_fault_route_invalid",
                "Provider fault route registration received an invalid upstream port.",
            )
        with self._provider_fault_routes_lock:
            if route_prefix in self._provider_fault_routes:
                raise AcceptanceError(
                    "runner.provider_fault_route_duplicate",
                    "Provider fault route registration collided with an active route.",
                )
            self._provider_fault_routes[route_prefix] = upstream_port

    def unregister_provider_fault_route(self, route_prefix: str, upstream_port: int) -> None:
        with self._provider_fault_routes_lock:
            if self._provider_fault_routes.get(route_prefix) == upstream_port:
                del self._provider_fault_routes[route_prefix]

    def provider_fault_upstream_port(self, path: str) -> int | None:
        with self._provider_fault_routes_lock:
            routes = tuple(self._provider_fault_routes.items())
        for route_prefix, upstream_port in routes:
            if path == route_prefix or path.startswith(route_prefix + "/"):
                return upstream_port
        return None

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


class _ProviderFaultServer:
    """Return bounded Provider-shaped HTTP failures without retaining request bodies or Secrets."""

    def __init__(
        self,
        provider: str,
        fault: str,
        *,
        listen_host: str = "127.0.0.1",
        advertised_host: str | None = None,
    ) -> None:
        if fault not in {"authentication", "rate-limit"}:
            raise ValueError(f"unsupported Provider HTTP fault: {fault}")
        advertised_host = advertised_host or listen_host
        if listen_host not in {"127.0.0.1", "0.0.0.0"}:
            raise ValueError("Provider fault server listen host must be loopback or all IPv4 interfaces")
        if re.fullmatch(r"[A-Za-z0-9._-]+", advertised_host) is None:
            raise ValueError("Provider fault server advertised host is invalid")
        self.provider = provider
        self.fault = fault
        self.listen_host = listen_host
        self.advertised_host = advertised_host
        self.route_token = uuid.uuid4().hex
        self.route_prefix = f"/{self.route_token}"
        self._requests: list[dict[str, Any]] = []
        self._unscoped_request_count = 0
        self._lock = threading.Lock()
        fault_server = self

        class Handler(http.server.BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.0"
            server_version = "SynaraStage3ProviderFault/1"
            sys_version = ""

            def do_GET(self) -> None:
                self._respond()

            def do_POST(self) -> None:
                self._respond()

            def do_PUT(self) -> None:
                self._respond()

            def do_PATCH(self) -> None:
                self._respond()

            def do_DELETE(self) -> None:
                self._respond()

            def do_OPTIONS(self) -> None:
                self._respond()

            def do_HEAD(self) -> None:
                self._respond()

            def log_message(self, _format: str, *_args: Any) -> None:
                return None

            def _respond(self) -> None:
                parsed = urllib.parse.urlsplit(self.path)
                if not (
                    parsed.path == fault_server.route_prefix
                    or parsed.path.startswith(fault_server.route_prefix + "/")
                ):
                    with fault_server._lock:
                        fault_server._unscoped_request_count += 1
                    self._write_response(
                        404,
                        {"error": {"type": "not_found", "message": "Not found."}},
                    )
                    return
                credential_headers = sorted(
                    name.lower()
                    for name in ("Authorization", "X-Api-Key")
                    if self.headers.get(name)
                )
                try:
                    content_length = max(0, int(self.headers.get("Content-Length", "0")))
                except ValueError:
                    content_length = 0
                with fault_server._lock:
                    fault_server._requests.append(
                        {
                            "method": self.command,
                            "path": (parsed.path[len(fault_server.route_prefix) :] or "/")[:500],
                            "contentLength": min(content_length, 16 << 20),
                            "credentialHeaderNames": credential_headers,
                        }
                    )
                status, payload = fault_server._response()
                self._write_response(status, payload)

            def _write_response(self, status: int, payload: Mapping[str, Any]) -> None:
                encoded = json.dumps(payload, separators=(",", ":")).encode("utf-8")
                try:
                    self.send_response(status)
                    self.send_header("Content-Type", "application/json")
                    self.send_header("Content-Length", str(len(encoded)))
                    self.send_header("Connection", "close")
                    self.send_header("X-Request-ID", f"stage3-provider-fault-{fault_server.fault}")
                    if status == 429:
                        self.send_header("Retry-After", "0")
                    self.end_headers()
                    if self.command != "HEAD":
                        self.wfile.write(encoded)
                except OSError:
                    return None

        try:
            self.server = http.server.ThreadingHTTPServer((self.listen_host, 0), Handler)
        except OSError as error:
            raise AcceptanceError(
                "runner.provider_fault_server_start_failed",
                f"Provider fault server could not bind: {error}",
            ) from None
        self.server.daemon_threads = True
        self.port = int(self.server.server_address[1])
        self.advertised_port = self.port
        self.thread = threading.Thread(
            target=self.server.serve_forever,
            kwargs={"poll_interval": 0.1},
            name=f"stage3-provider-{fault}-fault",
            daemon=True,
        )

    @property
    def endpoint(self) -> str:
        return f"http://{self.advertised_host}:{self.advertised_port}{self.route_prefix}"

    @property
    def credential_base_url(self) -> str:
        return f"{self.endpoint}/v1" if self.provider == "codex" else self.endpoint

    def start(self) -> None:
        self.thread.start()

    def stop(self) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5.0)
        if self.thread.is_alive():
            raise AcceptanceError(
                "runner.provider_fault_server_stop_failed",
                "Provider fault server did not stop within five seconds.",
            )

    def evidence(self) -> Mapping[str, Any]:
        with self._lock:
            requests = tuple(dict(request) for request in self._requests)
            unscoped_request_count = self._unscoped_request_count
        credential_headers = sorted(
            {
                str(header)
                for request in requests
                for header in request.get("credentialHeaderNames", [])
            }
        )
        return {
            "fault": self.fault,
            "listenAddress": self.listen_host,
            "advertisedHost": self.advertised_host,
            "port": self.port,
            "advertisedPort": self.advertised_port,
            "routeTokenPersisted": False,
            "unscopedRequestCount": unscoped_request_count,
            "responseStatus": 401 if self.fault == "authentication" else 429,
            "requestCount": len(requests),
            "methods": sorted({str(request.get("method")) for request in requests}),
            "paths": sorted({str(request.get("path")) for request in requests})[:20],
            "credentialHeaderNames": credential_headers,
            "requestBodiesRetained": False,
            "credentialValuesRetained": False,
        }

    def _response(self) -> tuple[int, Mapping[str, Any]]:
        if self.fault == "authentication":
            status = 401
            error_type = "authentication_error"
            error_code = "invalid_api_key"
            message = "Authentication required: invalid API key for the Stage 3 Provider fault matrix."
        else:
            status = 429
            error_type = "rate_limit_error"
            error_code = "rate_limit_exceeded"
            message = "Rate limit exceeded for the Stage 3 Provider fault matrix."
        if self.provider == "claudeAgent":
            return status, {
                "type": "error",
                "error": {"type": error_type, "message": message},
            }
        return status, {
            "error": {"type": error_type, "code": error_code, "message": message},
        }


class _SSHProviderFaultServer(_ProviderFaultServer):
    def __init__(self, driver: "SSHDriver", provider: str, fault: str) -> None:
        super().__init__(provider, fault)
        self.driver = driver
        self.route_registered = False

    def start(self) -> None:
        self.driver._ensure_worker_proxy_relay()
        worker_proxy = self.driver.worker_proxy
        if worker_proxy is None:
            raise AcceptanceError(
                "runner.ssh_provider_fault_relay_unavailable",
                "The SSH Worker-only proxy was unavailable for Provider fault routing.",
            )
        self.advertised_port = self.driver.worker_proxy_relay_port
        super().start()
        try:
            worker_proxy.register_provider_fault_route(self.route_prefix, self.port)
        except Exception:
            super().stop()
            raise
        self.route_registered = True

    def stop(self) -> None:
        errors: list[str] = []
        worker_proxy = self.driver.worker_proxy
        if self.route_registered and worker_proxy is not None:
            try:
                worker_proxy.unregister_provider_fault_route(self.route_prefix, self.port)
            except Exception as error:
                errors.append(self.driver.redactor.text(str(error)))
        self.route_registered = False
        try:
            super().stop()
        except Exception as error:
            errors.append(self.driver.redactor.text(str(error)))
        if errors:
            raise AcceptanceError(
                "runner.ssh_provider_fault_cleanup_failed",
                "The SSH Provider fault endpoint could not be cleaned completely.",
                {"errors": errors},
            )


def provider_host_crash_script() -> str:
    """Kill one protocol-v2 Provider Host under an explicitly scoped agentd root PID."""

    return (
        "const fs=require('node:fs');"
        "const needle=['--protocol','-v2'].join('');"
        "const rootPid=Number(process.argv[1]);"
        "const processes=new Map();"
        "for(const entry of fs.readdirSync('/proc',{withFileTypes:true})){"
        "if(!entry.isDirectory()||!/^[0-9]+$/.test(entry.name))continue;"
        "const pid=Number(entry.name);"
        "try{"
        "const stat=fs.readFileSync('/proc/'+pid+'/stat','utf8');"
        "const close=stat.lastIndexOf(')');"
        "const fields=stat.slice(close+2).trim().split(/\\s+/);"
        "const ppid=Number(fields[1]);"
        "const command=fs.readFileSync('/proc/'+pid+'/cmdline').toString('utf8').replace(/\\0/g,' ');"
        "processes.set(pid,{ppid,command});"
        "}catch{}"
        "}"
        "const descendants=new Set(Number.isInteger(rootPid)&&rootPid>0?[rootPid]:[]);"
        "let changed=true;"
        "while(changed){changed=false;for(const [pid,value] of processes){"
        "if(!descendants.has(pid)&&descendants.has(value.ppid)){descendants.add(pid);changed=true;}"
        "}}"
        "const candidates=[];"
        "for(const [pid,value] of processes){"
        "if(pid!==rootPid&&descendants.has(pid)&&value.command.includes(needle))candidates.push(pid);"
        "}"
        "candidates.sort((left,right)=>left-right);"
        "const result={rootPid,candidateCount:candidates.length,"
        "descendantCount:Math.max(0,descendants.size-1)};"
        "if(candidates.length===1){"
        "result.providerHostPid=candidates[0];"
        "try{process.kill(candidates[0],'SIGKILL');result.killed=true;}"
        "catch(error){result.killed=false;result.killError=error&&error.code?String(error.code):'unknown';}"
        "}"
        "process.stdout.write(JSON.stringify(result));"
    )


def provider_host_crash_evidence(
    output: str,
    *,
    target: str,
    scope: Mapping[str, Any],
) -> dict[str, Any]:
    try:
        result = json.loads(output)
    except json.JSONDecodeError:
        raise AcceptanceError(
            "runner.provider_host_process_scan_failed",
            f"{target.title()} Provider Host process scan returned invalid JSON.",
        ) from None
    if not isinstance(result, dict):
        raise AcceptanceError(
            "runner.provider_host_process_scan_failed",
            f"{target.title()} Provider Host process scan returned an invalid payload.",
        )
    root_pid = result.get("rootPid")
    candidate_count = result.get("candidateCount")
    descendant_count = result.get("descendantCount")
    if (
        type(root_pid) is not int
        or root_pid < 1
        or type(candidate_count) is not int
        or candidate_count < 0
        or type(descendant_count) is not int
        or descendant_count < 0
    ):
        raise AcceptanceError(
            "runner.provider_host_process_scan_failed",
            f"{target.title()} Provider Host process scan returned invalid process counts.",
        )
    if candidate_count != 1:
        raise AcceptanceError(
            "runner.provider_host_process_ambiguous",
            f"Expected exactly one Provider Host process in the scoped {target} Worker runtime.",
            {
                "target": target,
                "candidateCount": candidate_count,
                "descendantCount": descendant_count,
                **scope,
            },
        )
    provider_host_pid = result.get("providerHostPid")
    if type(provider_host_pid) is not int or provider_host_pid < 2 or result.get("killed") is not True:
        raise AcceptanceError(
            "runner.provider_host_process_disappeared",
            f"The scoped {target} Provider Host exited before crash injection completed.",
            {
                "target": target,
                "providerHostPid": provider_host_pid,
                "killError": result.get("killError"),
                **scope,
            },
        )
    return {
        "target": target,
        "agentdRootPid": root_pid,
        "providerHostPid": provider_host_pid,
        "signal": signal.Signals(signal.SIGKILL).name,
        "scopedToAgentdDescendants": True,
        "broadProcessMatchUsed": False,
        **scope,
    }


def finalize_provider_fault_reachability(
    reachability: Mapping[str, Any],
    fault_evidence: Mapping[str, Any],
) -> dict[str, Any]:
    result = dict(reachability)
    if result.get("validationMode") != "controlled-provider-request":
        return result
    request_count = fault_evidence.get("requestCount")
    if type(request_count) is not int or request_count < 1:
        raise AcceptanceError(
            "runner.provider_fault_reachability_unproven",
            "The execution-pinned Provider request did not prove fault endpoint reachability.",
        )
    return {
        **result,
        "probedFromWorker": True,
        "observedProviderRequestCount": request_count,
    }


class LocalDriver:
    name = "local"
    lifecycle = STANDING_WORKER
    replacement_workspace_semantics = "no managed Worker replacement"
    pending_interaction_recovery: str | None = None

    def real_provider_node_executable(self) -> str:
        return "node"

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

    def create_provider_fault_server(self, provider: str, fault: str) -> _ProviderFaultServer:
        if self.name != "local":
            raise AcceptanceUnsupported(
                "runner.real_provider_http_fault_target_unsupported",
                "The selected Target does not implement a scoped Provider HTTP fault endpoint.",
                {"target": self.name, "requiredTargets": list(REAL_PROVIDER_HTTP_FAULT_TARGETS)},
            )
        return _ProviderFaultServer(provider, fault)

    def probe_provider_fault_server(
        self,
        _server: _ProviderFaultServer,
    ) -> Mapping[str, Any]:
        if self.name != "local":
            raise AcceptanceUnsupported(
                "runner.real_provider_http_fault_target_unsupported",
                "The selected Target does not implement Provider fault endpoint reachability validation.",
                {"target": self.name, "requiredTargets": list(REAL_PROVIDER_HTTP_FAULT_TARGETS)},
            )
        return {
            "target": self.name,
            "transport": "host-loopback",
            "probedFromWorker": False,
        }

    def crash_provider_host(self) -> Mapping[str, Any]:
        if self.name != "local" or os.name != "posix":
            raise AcceptanceUnsupported(
                "runner.real_provider_host_crash_unsupported",
                "The selected Target does not implement scoped real Provider Host crash injection.",
                {"target": self.name, "requiredTargets": list(REAL_PROVIDER_HOST_CRASH_TARGETS)},
            )
        process = self.process
        if process is None or process.poll() is not None:
            raise AcceptanceError(
                "runner.control_plane_not_running",
                "Control Plane was not running during Provider Host crash injection.",
            )
        try:
            completed = subprocess.run(
                ["ps", "-axo", "pid=,ppid=,command="],
                cwd=self.repo_root,
                env=self._tool_environment(),
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                text=True,
                timeout=10.0,
                check=False,
            )
        except (OSError, subprocess.TimeoutExpired) as error:
            raise AcceptanceError(
                "runner.provider_host_process_scan_failed",
                f"Provider Host process tree could not be inspected: {self.redactor.text(str(error))}",
            ) from None
        if completed.returncode != 0:
            raise AcceptanceError(
                "runner.provider_host_process_scan_failed",
                f"Process tree inspection exited with status {completed.returncode}.",
            )
        processes: dict[int, tuple[int, str]] = {}
        for line in completed.stdout.splitlines():
            match = re.match(r"\s*(\d+)\s+(\d+)\s+(.*)$", line)
            if match is None:
                continue
            processes[int(match.group(1))] = (int(match.group(2)), match.group(3))
        descendants = {process.pid}
        changed = True
        while changed:
            changed = False
            for pid, (parent_pid, _command) in processes.items():
                if pid not in descendants and parent_pid in descendants:
                    descendants.add(pid)
                    changed = True
        candidates = sorted(
            pid
            for pid in descendants
            if pid != process.pid and "--protocol-v2" in processes.get(pid, (0, ""))[1]
        )
        if len(candidates) != 1:
            raise AcceptanceError(
                "runner.provider_host_process_ambiguous",
                "Expected exactly one scoped Provider Host process during crash injection.",
                {
                    "controlPlanePid": process.pid,
                    "candidateCount": len(candidates),
                    "descendantCount": max(0, len(descendants) - 1),
                },
            )
        provider_host_pid = candidates[0]
        try:
            os.kill(provider_host_pid, signal.SIGKILL)
        except ProcessLookupError:
            raise AcceptanceError(
                "runner.provider_host_process_disappeared",
                "The scoped Provider Host exited before crash injection completed.",
                {"providerHostPid": provider_host_pid},
            ) from None
        return {
            "target": self.name,
            "controlPlanePid": process.pid,
            "providerHostPid": provider_host_pid,
            "signal": signal.Signals(signal.SIGKILL).name,
            "scopedToControlPlaneDescendants": True,
            "broadProcessMatchUsed": False,
        }

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
                "SYNARA_WORKER_LEASE_TTL": self.options.worker_lease_ttl,
                "SYNARA_WORKER_HEARTBEAT_TIMEOUT": self.options.worker_heartbeat_timeout,
                "SYNARA_OUTBOX_POLL_INTERVAL": "100ms",
                "SYNARA_OUTBOX_CLAIM_TTL": "5s",
                "SYNARA_RETENTION_SWEEP_INTERVAL": (
                    FIXTURE_RETENTION_SWEEP_INTERVAL
                    if self.options.suite == "fixture-retention-concurrency"
                    else "24h"
                ),
            }
        )
        if "cursor-expiry" in self.options.real_provider_failure_cases:
            environment["SYNARA_PROVIDER_CURSOR_MAX_AGE"] = REAL_PROVIDER_CURSOR_MAX_AGE
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

    def _execution_worker_identity(
        self,
        target_id: str,
        execution_id: str,
    ) -> dict[str, Any]:
        database_path = self.state_dir / "metadata.sqlite"
        try:
            connection = sqlite3.connect(f"file:{database_path}?mode=ro", uri=True, timeout=2.0)
            try:
                row = connection.execute(
                    """
                    SELECT execution.worker_id,
                           execution.generation,
                           worker.incarnation,
                           worker.instance_uid,
                           worker.status,
                           worker.pod_name
                    FROM agent_executions AS execution
                    JOIN worker_instances AS worker ON worker.id = execution.worker_id
                    WHERE execution.id = ?
                      AND execution.execution_target_id = ?
                      AND worker.execution_target_id = ?
                    """,
                    (execution_id, target_id, target_id),
                ).fetchone()
            finally:
                connection.close()
        except sqlite3.Error as error:
            raise AcceptanceError(
                "runner.execution_worker_identity_query_failed",
                f"Execution Worker identity could not be read from the isolated metadata store: {error}",
                {"targetId": target_id, "executionId": execution_id},
            ) from None
        if row is None:
            raise AcceptanceError(
                "runner.execution_worker_identity_missing",
                "The Execution did not retain an exact managed Worker identity.",
                {"targetId": target_id, "executionId": execution_id},
            )
        return {
            "id": str(row[0]),
            "generation": int(row[1]),
            "incarnation": int(row[2]),
            "instanceUid": str(row[3]),
            "status": str(row[4]),
            "podName": str(row[5]),
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
        self.desired_workers = (
            FIXTURE_CONCURRENCY_WORKERS
            if options.suite in FIXTURE_PARALLEL_DOCKER_SUITES
            else 1
        )
        self.target_id: str | None = None

    @property
    def worker_proxy_host(self) -> str:
        return self.options.docker_control_plane_host

    def create_provider_fault_server(self, provider: str, fault: str) -> _ProviderFaultServer:
        return _ProviderFaultServer(
            provider,
            fault,
            listen_host="0.0.0.0",
            advertised_host=self.worker_proxy_host,
        )

    def _required_managed_container_id(self, operation: str) -> str:
        target_id = self.target_id
        if target_id is None:
            raise AcceptanceError(
                "runner.docker_target_id_missing",
                f"Docker {operation} was requested before Target provisioning.",
            )
        snapshot = self._wait_container(target_id)
        container_id = str(snapshot.get("Id") or "")
        if not container_id:
            raise AcceptanceError(
                "runner.docker_container_id_missing",
                f"Managed Docker Worker inspect omitted its container ID during {operation}.",
            )
        return container_id

    def probe_provider_fault_server(
        self,
        server: _ProviderFaultServer,
    ) -> Mapping[str, Any]:
        container_id = self._required_managed_container_id(
            "Provider fault endpoint reachability validation"
        )
        expected_status = 401 if server.fault == "authentication" else 429
        script = (
            "const expected=Number(process.argv[1]);"
            "fetch(process.argv[2],{method:'HEAD'}).then((response)=>{"
            "if(response.status!==expected){process.exitCode=2;}"
            "}).catch(()=>{process.exitCode=3;});"
        )
        self._docker_command(
            [
                "exec",
                container_id,
                "node",
                "-e",
                script,
                str(expected_status),
                server.credential_base_url,
            ],
            maximum_timeout=15.0,
        )
        return {
            "target": self.name,
            "transport": "host-gateway",
            "advertisedHost": self.worker_proxy_host,
            "containerId": container_id[:12],
            "expectedStatus": expected_status,
            "probedFromWorker": True,
            "endpointPersisted": False,
        }

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
        enabled_providers = (
            list(FIXTURE_CONCURRENCY_PROVIDERS)
            if self.options.suite in FIXTURE_PARALLEL_DOCKER_SUITES
            else [provider]
        )
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
                        "desiredWorkers": self.desired_workers,
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
                        "providerPolicy": {"experimentalProviders": enabled_providers},
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
        snapshots = self._wait_containers(target_id, self.desired_workers)
        validated = [self._validate_container(snapshot) for snapshot in snapshots]
        evidence: Mapping[str, Any]
        if self.desired_workers == 1:
            evidence = validated[0]
        else:
            evidence = {
                "desiredWorkers": self.desired_workers,
                "runningWorkers": len(validated),
                "containers": validated,
            }
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

    def _execution_worker_container(
        self,
        target_id: str,
        execution_id: str,
    ) -> tuple[dict[str, Any], dict[str, Any], str]:
        worker = self._execution_worker_identity(target_id, execution_id)
        pod_name = str(worker.get("podName") or "")
        snapshots = self._wait_containers(target_id, self.desired_workers)
        matches = [
            snapshot
            for snapshot in snapshots
            if str(snapshot.get("Name") or "").lstrip("/") == pod_name
        ]
        if len(matches) != 1:
            raise AcceptanceError(
                "runner.docker_execution_worker_container_missing",
                "The exact Execution Worker did not map to one managed Docker container.",
                {
                    "targetId": target_id,
                    "executionId": execution_id,
                    "workerId": worker.get("id"),
                    "podName": pod_name,
                    "candidateContainerNames": [
                        str(snapshot.get("Name") or "").lstrip("/")
                        for snapshot in snapshots
                    ],
                },
            )
        snapshot = matches[0]
        config = json_object(snapshot.get("Config"), "docker inspect Config")
        labels = json_object(config.get("Labels"), "docker inspect Config.Labels")
        worker_index = labels.get("synara.io/worker-index")
        if (
            labels.get("synara.io/managed") != "true"
            or labels.get("synara.io/execution-target-id") != target_id
            or not isinstance(worker_index, str)
            or re.fullmatch(r"[0-9]+", worker_index) is None
        ):
            raise AcceptanceError(
                "runner.docker_execution_worker_container_contract_mismatch",
                "The exact Execution Worker container omitted its managed Target labels.",
                {
                    "targetId": target_id,
                    "executionId": execution_id,
                    "workerId": worker.get("id"),
                    "podName": pod_name,
                    "managed": labels.get("synara.io/managed"),
                    "labeledTargetId": labels.get("synara.io/execution-target-id"),
                    "workerIndex": worker_index,
                },
            )
        return snapshot, worker, worker_index

    def _wait_replacement_execution_worker_container(
        self,
        target_id: str,
        execution_id: str,
        container_name: str,
        previous_container_id: str,
        previous_worker: Mapping[str, Any],
    ) -> tuple[dict[str, Any], dict[str, Any]]:
        def replacement_probe() -> tuple[dict[str, Any], dict[str, Any]] | None:
            matches = [
                snapshot
                for snapshot in self._container_snapshots(target_id)
                if str(snapshot.get("Name") or "").lstrip("/") == container_name
                and snapshot.get("Id") != previous_container_id
                and isinstance(snapshot.get("State"), dict)
                and json_object(snapshot.get("State"), "docker inspect State").get("Running") is True
            ]
            if len(matches) != 1:
                return None
            worker = self._execution_worker_identity(target_id, execution_id)
            if (
                worker.get("id") != previous_worker.get("id")
                or int(worker.get("incarnation") or 0)
                <= int(previous_worker.get("incarnation") or 0)
                or worker.get("instanceUid") == previous_worker.get("instanceUid")
                or worker.get("status") != "online"
                or worker.get("podName") != container_name
            ):
                return None
            return matches[0], worker

        return self.api.wait_until(
            f"replacement managed Docker Worker container {container_name}",
            replacement_probe,
        )

    def _inject_worker_container_loss(
        self,
        target_id: str,
        execution_id: str,
    ) -> Mapping[str, Any]:
        snapshot, before_worker, worker_index = self._execution_worker_container(
            target_id,
            execution_id,
        )
        container_id = str(snapshot.get("Id") or "")
        container_name = str(snapshot.get("Name") or "").lstrip("/")
        if not container_id or not container_name:
            raise AcceptanceError(
                "runner.docker_container_identity_missing",
                "The managed Docker Worker omitted its container identity before exact loss injection.",
            )
        self._write_volume_sentinel(container_id)
        started = time.monotonic()
        self._docker_command(["rm", "-f", container_id], maximum_timeout=20.0)
        replacement, after_worker = self._wait_replacement_execution_worker_container(
            target_id,
            execution_id,
            container_name,
            container_id,
            before_worker,
        )
        replacement_container_id = str(replacement.get("Id") or "")
        if not replacement_container_id:
            raise AcceptanceError(
                "runner.docker_replacement_container_id_missing",
                "The replacement managed Docker Worker omitted its container ID.",
            )
        self._verify_volume_sentinel(replacement_container_id)
        return {
            "fault": "worker-container-loss",
            "executionId": execution_id,
            "executionGeneration": before_worker.get("generation"),
            "workerId": before_worker.get("id"),
            "workerPodName": before_worker.get("podName"),
            "workerIndex": int(worker_index),
            "removedContainerId": container_id[:12],
            "replacementContainerId": replacement_container_id[:12],
            "containerName": container_name,
            "containerIdChanged": replacement_container_id != container_id,
            "exactExecutionWorkerMatch": container_name == before_worker.get("podName"),
            "workerIdStable": after_worker.get("id") == before_worker.get("id"),
            "previousWorkerIncarnation": before_worker.get("incarnation"),
            "replacementWorkerIncarnation": after_worker.get("incarnation"),
            "workerIncarnationAdvanced": int(after_worker.get("incarnation") or 0)
            > int(before_worker.get("incarnation") or 0),
            "instanceUidChanged": after_worker.get("instanceUid") != before_worker.get("instanceUid"),
            "namedVolumeContinuity": {
                "sentinelPath": DOCKER_VOLUME_SENTINEL_PATH,
                "preservedAcrossReplacement": True,
                "semantics": "named-volume content continuity; not Workspace Checkpoint restore",
            },
            "durationMs": elapsed_ms(started),
            "replacementReady": True,
        }

    def inject_failure(
        self,
        fault: str,
        target_id: str,
        execution_id: str,
    ) -> Mapping[str, Any]:
        if fault == "provider-host-process-crash":
            snapshot, worker, worker_index = self._execution_worker_container(
                target_id,
                execution_id,
            )
            container_id = str(snapshot.get("Id") or "")
            container_name = str(snapshot.get("Name") or "").lstrip("/")
            if not container_id or not container_name:
                raise AcceptanceError(
                    "runner.docker_container_identity_missing",
                    "The managed Docker Worker omitted its container identity before Provider Host crash.",
                )
            evidence = self._crash_provider_host_in_container(
                container_id,
                {
                    "containerId": container_id[:12],
                    "containerName": container_name,
                    "scopedToManagedContainer": True,
                },
            )
            return {
                "fault": fault,
                "executionId": execution_id,
                "executionGeneration": worker.get("generation"),
                "workerId": worker.get("id"),
                "workerIncarnation": worker.get("incarnation"),
                "workerPodName": worker.get("podName"),
                "workerIndex": int(worker_index),
                "exactExecutionWorkerMatch": container_name == worker.get("podName"),
                **evidence,
            }
        if fault == "worker-container-loss":
            return self._inject_worker_container_loss(target_id, execution_id)
        if fault != "worker-network":
            return super().inject_failure(fault, target_id, execution_id)
        if not self.owns_network and not self.options.docker_allow_network_interruption:
            raise AcceptanceUnsupported(
                "runner.docker_network_fault_not_authorized",
                "Docker network interruption is disabled for an operator-owned network.",
                {
                    "network": self.network_name,
                    "requiredInputs": ["--docker-allow-network-interruption"],
                },
            )
        snapshot, before_worker, worker_index = self._execution_worker_container(
            target_id,
            execution_id,
        )
        container_id = str(snapshot.get("Id") or "")
        if not container_id:
            raise AcceptanceError(
                "runner.docker_container_id_missing",
                "The managed Docker Worker omitted its container ID before network interruption.",
            )
        container_name = str(snapshot.get("Name") or "").lstrip("/")
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
            "containerName": container_name,
            "workerIndex": int(worker_index),
            "executionId": execution_id,
            "executionGeneration": before_worker.get("generation"),
            "workerId": before_worker.get("id"),
            "workerIncarnation": before_worker.get("incarnation"),
            "workerPodName": before_worker.get("podName"),
            "exactExecutionWorkerMatch": container_name == before_worker.get("podName"),
            "durationMs": elapsed_ms(started),
            "restored": True,
        }

    def validate_failure(self, fault: str) -> None:
        if fault == "provider-host-process-crash":
            return
        if fault == "worker-container-loss":
            return
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

    def _crash_provider_host_in_container(
        self,
        container_id: str,
        scope: Mapping[str, Any],
    ) -> Mapping[str, Any]:
        output = self._docker_command(
            ["exec", container_id, "node", "-e", provider_host_crash_script(), "1"],
            maximum_timeout=15.0,
        )
        return provider_host_crash_evidence(
            output,
            target=self.name,
            scope=scope,
        )

    def crash_provider_host(self) -> Mapping[str, Any]:
        container_id = self._required_managed_container_id("Provider Host crash injection")
        return self._crash_provider_host_in_container(
            container_id,
            {
                "containerId": container_id[:12],
                "scopedToManagedContainer": True,
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
            "stateRemoved": self._temporary_state and not self.state_dir.exists(),
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

    def _container_snapshots(self, target_id: str) -> list[dict[str, Any]]:
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
            return []
        completed = self._docker_completed(["inspect", *container_ids])
        output = self.redactor.text(completed.stdout)
        if completed.returncode != 0:
            if "no such object" in output.lower() or "no such container" in output.lower():
                return []
            raise AcceptanceError(
                "runner.docker_command_failed",
                f"Docker inspect exited with status {completed.returncode}.",
                {
                    "command": ["docker", "inspect", *container_ids],
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
        if (
            not isinstance(inspected, list)
            or len(inspected) != len(container_ids)
            or not all(isinstance(item, dict) for item in inspected)
        ):
            raise AcceptanceError("runner.docker_inspect_invalid", "Docker inspect returned an invalid payload.")
        return sorted(
            inspected,
            key=lambda item: str(item.get("Name") or ""),
        )

    def _container_snapshot(self, target_id: str) -> dict[str, Any] | None:
        snapshots = self._container_snapshots(target_id)
        if not snapshots:
            return None
        if len(snapshots) != 1:
            raise AcceptanceError(
                "runner.docker_container_count_invalid",
                "Expected exactly one managed Docker Worker container for this operation.",
                {"targetId": target_id, "containerCount": len(snapshots)},
            )
        return snapshots[0]

    def _wait_containers(self, target_id: str, expected_count: int) -> list[dict[str, Any]]:
        def probe() -> list[dict[str, Any]] | None:
            snapshots = self._container_snapshots(target_id)
            if len(snapshots) != expected_count:
                return None
            for snapshot in snapshots:
                state = snapshot.get("State")
                if not isinstance(state, dict) or state.get("Running") is not True:
                    return None
            return snapshots

        return self.api.wait_until(
            f"{expected_count} running managed Docker Worker containers",
            probe,
        )

    def _wait_container(self, target_id: str) -> dict[str, Any]:
        return self._wait_containers(target_id, 1)[0]

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
        self.external_host = options.ssh_external_host is not None
        self.owns_machine = not self.external_host
        if options.ssh_external_host is not None:
            self.redactor.add(
                options.ssh_external_host,
                "[REDACTED_SSH_EXTERNAL_HOST]",
            )
        if options.ssh_external_identity_file is not None:
            self.redactor.add(
                str(options.ssh_external_identity_file),
                "[REDACTED_SSH_IDENTITY_SOURCE]",
            )
        if options.ssh_external_host_key_file is not None:
            self.redactor.add(
                str(options.ssh_external_host_key_file),
                "[REDACTED_SSH_HOST_KEY_SOURCE]",
            )
        self.machine_name = options.ssh_machine_name or f"synara-stage3-{suffix}"
        self.machine_create_attempted = False
        self.machine_created = False
        self.external_runtime_created = False
        self.tenant_id: str | None = None
        self.target_id: str | None = None
        self.service_name: str | None = None
        self.machine_ip = options.ssh_external_host or ""
        self.host_key = ""
        self.client_private_key = ""
        self.client_public_key = ""
        self.credentials_dir = self.state_dir / "ssh-credentials"
        self.client_key_path = self.credentials_dir / "id_ed25519"
        self.client_public_key_path = self.credentials_dir / "id_ed25519.pub"
        self.known_hosts_path = self.credentials_dir / "known_hosts"
        self.external_runtime_root = f"/opt/synara/acceptance/{self.resource_owner}"
        self.external_stage_root = f"/tmp/synara-stage3-{self.resource_owner}"
        self.remote_node_path = (
            f"{self.external_runtime_root}/node/bin/node" if self.external_host else "node"
        )
        self.remote_provider_host_path = (
            f"{self.external_runtime_root}/provider-host/index.mjs"
            if self.external_host
            else SSH_REMOTE_PROVIDER_HOST_PATH
        )
        self.remote_fixture_path = (
            f"{self.external_runtime_root}/provider-host-fixture.mjs"
            if self.external_host
            else SSH_REMOTE_FIXTURE_PATH
        )
        self.remote_provider_tools_root = (
            f"{self.external_runtime_root}/provider-tools"
            if self.external_host
            else SSH_REMOTE_PROVIDER_TOOLS_ROOT
        )
        self.remote_provider_host_command_path = (
            f"{self.external_runtime_root}/bin/provider-host"
            if self.external_host
            else SSH_PROVIDER_HOST_COMMAND_PATH
        )
        self.external_managed_root = f"{self.external_runtime_root}/managed"
        self.external_install_root = f"{self.external_managed_root}/install"
        self.external_workspace_root = f"{self.external_managed_root}/workspaces"
        self.external_git_cache_root = f"{self.external_managed_root}/git-cache"
        self.agentd_binary_path = self.state_dir / "bin" / (
            f"synara-agentd-linux-{options.ssh_machine_arch}"
        )
        self.fixture_bundle_path = self.state_dir / "bin" / "provider-host-fixture.mjs"
        self.provider_host_bundle_path = self.state_dir / "bin" / "provider-host.mjs"
        self.provider_tools_package_path = (
            self.repo_root / "deploy" / "worker" / "provider-tools" / "package.json"
        )
        self.provider_tools_lock_path = (
            self.repo_root / "deploy" / "worker" / "provider-tools" / "package-lock.json"
        )
        self.worker_proxy_relay_process: subprocess.Popen[str] | None = None
        self.worker_proxy_relay_log_handle: Any | None = None
        self.worker_proxy_relay_log_path = self.logs_dir / "ssh-worker-proxy-relay.log"
        self.worker_proxy_relay_port = 0

    @property
    def credential_lifecycle(self) -> str:
        return (
            SSH_EXTERNAL_CREDENTIAL_LIFECYCLE
            if self.external_host
            else SSH_CREDENTIAL_LIFECYCLE
        )

    def real_provider_node_executable(self) -> str:
        return self.remote_node_path

    @property
    def identity_path(self) -> pathlib.Path:
        if self.external_host:
            path = self.options.ssh_external_identity_file
            if path is None:
                raise AcceptanceError(
                    "runner.ssh_external_identity_missing",
                    "The authorized external SSH identity source was unavailable.",
                )
            return path
        return self.client_key_path

    def _runner_command(self) -> list[str]:
        if not self.external_host:
            return list(self.options.runner_command)
        if self.options.suite == "real-provider-smoke":
            return [self.remote_provider_host_command_path]
        return [self.remote_node_path, self.remote_fixture_path, "--protocol-v2"]

    @property
    def worker_proxy_host(self) -> str:
        return SSH_RELAY_LOOPBACK_HOST

    def create_provider_fault_server(self, provider: str, fault: str) -> _ProviderFaultServer:
        return _SSHProviderFaultServer(self, provider, fault)

    def probe_provider_fault_server(
        self,
        server: _ProviderFaultServer,
    ) -> Mapping[str, Any]:
        process = self.worker_proxy_relay_process
        if (
            not isinstance(server, _SSHProviderFaultServer)
            or not server.route_registered
            or self.worker_proxy is None
            or not self.worker_proxy.thread.is_alive()
            or process is None
            or process.poll() is not None
        ):
            raise AcceptanceError(
                "runner.ssh_provider_fault_relay_unavailable",
                "The SSH Provider fault reverse relay was unavailable.",
            )
        return {
            "target": self.name,
            "transport": "reverse-ssh-loopback",
            "vmListenHost": SSH_RELAY_LOOPBACK_HOST,
            "vmListenPort": self.worker_proxy_relay_port,
            "validationMode": "controlled-provider-request",
            "probedFromWorker": False,
            "endpointPersisted": False,
            "readsUserSSHConfiguration": False,
        }

    def crash_provider_host(self) -> Mapping[str, Any]:
        service_name = self.service_name
        if not service_name or not self.machine_created:
            raise AcceptanceError(
                "runner.ssh_service_missing",
                "The managed SSH service was unavailable during Provider Host crash injection.",
            )
        service = self._require_service_active(service_name)
        main_pid = int(service["mainPid"])
        output = self._remote_root_command(
            [self.remote_node_path, "-e", provider_host_crash_script(), str(main_pid)],
            maximum_timeout=SSH_EXTERNAL_RECOVERY_OPERATION_TIMEOUT,
        )
        return provider_host_crash_evidence(
            output,
            target=self.name,
            scope={
                **(
                    {"installationId": self.installation_id}
                    if self.external_host
                    else {"machineName": self.machine_name}
                ),
                "serviceName": service_name,
                "systemdMainPid": main_pid,
                "scopedToDisposableMachine": self.owns_machine,
                "scopedToExternalHost": self.external_host,
                "scopedToSystemdService": True,
            },
        )

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
                "The SSH runtime was unavailable while provisioning the Target.",
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
            self._assert_remote_target_absent(target_id)
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
            runtime_identity = {
                "runtime": (
                    "authorized-external-host"
                    if self.external_host
                    else "owned-disposable-orbstack"
                ),
                "ownedMachine": self.owns_machine,
                "installationId": self.installation_id,
            }
            return {
                **target,
                "driverEvidence": {
                    **runtime_identity,
                    **({"machineName": self.machine_name, "machineAddress": self.machine_ip} if self.owns_machine else {}),
                    "hostKeyAlgorithm": self.host_key.split()[0],
                    "hostKeyFingerprint": self._host_key_fingerprint(self.host_key),
                    "hostKeyMismatch": negative_evidence,
                    "service": service,
                    "binarySha256": installed.get("binarySha256"),
                    "credentialSource": (
                        "repository-external operator identity"
                        if self.external_host
                        else "runner-generated one-time Ed25519 key"
                    ),
                    "controlPlaneTransport": self._worker_proxy_relay_evidence(),
                    "controlPlaneCredentialLifecycle": self.credential_lifecycle,
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
        sshd_restarted = False
        if self.owns_machine:
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
            sshd_restarted = True
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
            "strategy": (
                "pinned-Host-Key SSH upgrade of the scoped managed service"
                if self.external_host
                else "pinned-Host-Key SSH upgrade with systemd restart"
            ),
            "sshdRestarted": sshd_restarted,
            "externalHostRestarted": False,
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
        revoke_required = bool(self.target_id and self.tenant_id)
        revoke_succeeded = not revoke_required
        managed_cleanup_verified = not bool(self.target_id)

        def collect(operation: str, action: Callable[[], Any]) -> Any:
            try:
                return action()
            except Exception as error:
                errors.append(f"{operation}: {self.redactor.text(str(error))}")
                return None

        if self.machine_created and self.service_name:
            collect(
                "capture SSH Worker journal",
                lambda: self._remote_root_command(
                    ["journalctl", "--no-pager", "-u", self.service_name, "-n", "500"],
                    log_path=self.logs_dir / "ssh-agentd-journal.log",
                    cleanup_timeout=SSH_EXTERNAL_RECOVERY_OPERATION_TIMEOUT,
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
            elif isinstance(result, dict):
                revoke_succeeded = True
        elif revoke_required:
            errors.append("revoke managed SSH Target: Control Plane was unavailable")
        if self.machine_created and self.target_id and revoke_succeeded:
            verified = collect(
                "verify revoked SSH Target files",
                lambda: (
                    self._assert_remote_target_absent(self.target_id, cleanup_timeout=20.0),
                    True,
                )[1],
            )
            managed_cleanup_verified = verified is True
        if self.machine_created and self.owns_machine:
            collect(
                "remove disposable SSH authorization",
                lambda: self._remote_command(
                    ["rm", "-f", "/root/.ssh/authorized_keys"],
                    cleanup_timeout=10.0,
                ),
            )
        if (
            self.external_host
            and self.external_runtime_created
            and revoke_succeeded
            and managed_cleanup_verified
        ):
            collect("remove owned external SSH runtime", self._remove_external_runtime)
        collect("stop Worker proxy relay", self._stop_worker_proxy_relay)
        collect("stop Control Plane", self.stop)
        if self.machine_created and self.owns_machine and not self.options.keep:
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
            "runtime": (
                "authorized-external-host"
                if self.external_host
                else "owned-disposable-orbstack"
            ),
            "installationId": self.installation_id,
            **({"machineName": self.machine_name} if self.owns_machine else {}),
            "machineRemoved": self.owns_machine and not self.options.keep and not self.machine_created,
            "machinePreservedByRequest": self.owns_machine and self.options.keep,
            "externalHostPreserved": self.external_host,
            "externalHostRestarted": False,
            "ownedRuntimeRemoved": not self.external_runtime_created,
            "operatorIdentitySourcePreserved": (
                self.external_host
                and self.options.ssh_external_identity_file is not None
                and self.options.ssh_external_identity_file.is_file()
            ),
            "productRevokeRequested": bool(self.target_id and self.tenant_id),
            "machineLifecycleCompleted": (
                not self.external_runtime_created
                if self.external_host
                else self.options.keep or not self.machine_created
            ),
            "localKeyMaterialRemoved": not self.credentials_dir.exists(),
            "stateRemoved": self._temporary_state and not self.state_dir.exists(),
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

    def _ssh_port(self) -> int:
        return self.options.ssh_external_port if self.external_host else 22

    def _ssh_user(self) -> str:
        return self.options.ssh_external_user or "root"

    def _known_hosts_token(self) -> str:
        host = self.machine_ip
        if self._ssh_port() != 22 or ":" in host:
            return f"[{host}]:{self._ssh_port()}"
        return host

    def _ssh_destination(self) -> str:
        host = f"[{self.machine_ip}]" if ":" in self.machine_ip else self.machine_ip
        return f"{self._ssh_user()}@{host}"

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
                "The SSH runtime was unavailable while starting the reverse relay.",
            )
        if self.worker_proxy is None or not self.worker_proxy.thread.is_alive():
            raise AcceptanceError(
                "runner.worker_proxy_unavailable",
                "Worker-only proxy was unavailable while starting the SSH relay.",
            )
        identity_path = self.identity_path
        if not identity_path.is_file():
            raise AcceptanceError(
                "runner.ssh_private_key_missing",
                "The authorized SSH identity was unavailable while starting the reverse relay.",
            )
        self.credentials_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
        # OpenSSH canonicalizes the default port as the bare host token. The
        # bracketed ``[host]:port`` form is reserved for non-default ports and
        # would make strict checking reject this otherwise pinned key.
        self.known_hosts_path.write_text(
            f"{self._known_hosts_token()} {self.host_key}\n",
            encoding="utf-8",
        )
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
                "-p",
                str(self._ssh_port()),
                "-i",
                str(identity_path),
                "-N",
                "-T",
                "-R",
                f"{SSH_RELAY_LOOPBACK_HOST}:{relay_port}:127.0.0.1:{self.worker_proxy.port}",
                self._ssh_destination(),
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
            "The runner-owned reverse SSH relay could not establish the remote loopback listener.",
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
        real_provider_runtime = self.options.suite == "real-provider-smoke"
        provider_host_bundle_path = (
            self.provider_host_bundle_path if real_provider_runtime else self.fixture_bundle_path
        )
        provider_host_source = (
            self.repo_root / "apps" / "provider-host" / "src" / "index.ts"
            if real_provider_runtime
            else self.repo_root
            / "scripts"
            / "stage3-provider-acceptance"
            / "provider-host-fixture.ts"
        )
        provider_host_started = time.monotonic()
        self._local_command(
            [
                "bun",
                "build",
                str(provider_host_source),
                "--target=node",
                "--outfile",
                str(provider_host_bundle_path),
            ],
            cwd=self.repo_root,
            environment=self._tool_environment(),
            log_path=self.logs_dir
            / (
                "ssh-provider-host-build.log"
                if real_provider_runtime
                else "ssh-provider-host-fixture-build.log"
            ),
            maximum_timeout=max(60.0, self.deadline.remaining()),
            error_code=(
                "runner.ssh_provider_host_build_failed"
                if real_provider_runtime
                else "runner.ssh_fixture_build_failed"
            ),
            description=(
                "real Provider Host build"
                if real_provider_runtime
                else "Provider Host fixture build"
            ),
        )
        provider_host_duration = elapsed_ms(provider_host_started)
        if not self.agentd_binary_path.is_file() or not provider_host_bundle_path.is_file():
            raise AcceptanceError(
                "runner.ssh_artifact_missing",
                "SSH acceptance build did not produce the required runtime artifacts.",
            )
        if real_provider_runtime and (
            not self.provider_tools_package_path.is_file()
            or not self.provider_tools_lock_path.is_file()
        ):
            raise AcceptanceError(
                "runner.ssh_provider_tools_lock_missing",
                "SSH real Provider runtime omitted its locked Provider tools package inputs.",
            )
        provider_host_evidence = {
            "path": str(provider_host_bundle_path),
            "remotePath": (
                self.remote_provider_host_path
                if real_provider_runtime
                else self.remote_fixture_path
            ),
            "sha256": hashlib.sha256(provider_host_bundle_path.read_bytes()).hexdigest(),
            "durationMs": provider_host_duration,
            "runtime": "real-provider" if real_provider_runtime else "deterministic-fixture",
        }
        return {
            "agentd": {
                "path": str(self.agentd_binary_path),
                "goos": "linux",
                "goarch": self.options.ssh_machine_arch,
                "sha256": hashlib.sha256(self.agentd_binary_path.read_bytes()).hexdigest(),
                "durationMs": agentd_duration,
            },
            (
                "providerHost"
                if real_provider_runtime
                else "providerHostFixture"
            ): provider_host_evidence,
            **(
                {
                    "providerTools": {
                        "packageSha256": hashlib.sha256(
                            self.provider_tools_package_path.read_bytes()
                        ).hexdigest(),
                        "lockSha256": hashlib.sha256(
                            self.provider_tools_lock_path.read_bytes()
                        ).hexdigest(),
                        "remoteRoot": self.remote_provider_tools_root,
                    }
                }
                if real_provider_runtime
                else {}
            ),
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
        if self.external_host:
            return self._load_external_identity()
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

    def _load_external_identity(self) -> Mapping[str, Any]:
        identity_path = self.identity_path
        try:
            mode = stat.S_IMODE(identity_path.stat().st_mode)
            private_key = identity_path.read_text(encoding="utf-8")
        except (OSError, UnicodeError):
            raise AcceptanceError(
                "runner.ssh_external_identity_unavailable",
                "The repository-external SSH identity could not be read.",
            ) from None
        if mode & (stat.S_IRWXG | stat.S_IRWXO) or not private_key.strip() or len(private_key) > 1 << 20:
            raise AcceptanceError(
                "runner.ssh_external_identity_invalid",
                "The repository-external SSH identity was empty, oversized, or had unsafe permissions.",
            )
        environment = self._tool_environment()
        environment.update({"SSH_ASKPASS": "/bin/false", "SSH_ASKPASS_REQUIRE": "force"})
        try:
            completed = subprocess.run(
                ["ssh-keygen", "-y", "-f", str(identity_path)],
                cwd=self.repo_root,
                env=environment,
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=subprocess.DEVNULL,
                text=True,
                timeout=self.deadline.request_timeout(maximum=10.0),
                check=False,
            )
        except (OSError, subprocess.TimeoutExpired):
            completed = None
        public_fields = completed.stdout.strip().split() if completed is not None else []
        if completed is None or completed.returncode != 0 or len(public_fields) < 2:
            raise AcceptanceError(
                "runner.ssh_external_identity_invalid",
                "The repository-external SSH identity must be a usable non-interactive private key.",
            )
        self.client_private_key = private_key
        self.client_public_key = " ".join(public_fields[:2])
        self.redactor.add(self.client_private_key, "[REDACTED_SSH_PRIVATE_KEY]")
        for line in self.client_private_key.splitlines():
            if len(line) >= 32 and not line.startswith("-----"):
                self.redactor.add(line, "[REDACTED_SSH_PRIVATE_KEY_DATA]")
        return {
            "credentialSource": "repository-external operator identity",
            "algorithm": public_fields[0],
            "driverPrivateKeyReferenceClearedAfterProvision": True,
            "operatorIdentitySourcePreserved": True,
            "controlPlaneCredentialLifecycle": self.credential_lifecycle,
        }

    def _prepare_machine(self) -> Mapping[str, Any]:
        if self.external_host:
            return self._prepare_external_host()
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
        if self.options.suite == "real-provider-smoke":
            for source in (
                self.provider_host_bundle_path,
                self.provider_tools_package_path,
                self.provider_tools_lock_path,
            ):
                self._remote_upload(source, f"{stage}/{source.name}", "0600")
        else:
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
        provider_runtime = (
            self._inspect_ssh_provider_runtime()
            if self.options.suite == "real-provider-smoke"
            else {"kind": "deterministic-fixture"}
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
            "providerRuntime": provider_runtime,
            "sshd": "active",
            "initSystem": "systemd",
            "hostKeyFingerprint": self._host_key_fingerprint(self.host_key),
        }

    def _prepare_external_host(self) -> Mapping[str, Any]:
        self.host_key = self._load_external_host_key()
        try:
            with socket.create_connection(
                (self.machine_ip, self._ssh_port()),
                timeout=self.deadline.request_timeout(maximum=5.0),
            ) as connection:
                connection.settimeout(self.deadline.request_timeout(maximum=5.0))
                banner = connection.recv(128)
        except OSError:
            raise AcceptanceError(
                "runner.ssh_external_unreachable",
                "The authorized external SSH endpoint was unreachable.",
            ) from None
        if not banner.startswith(b"SSH-"):
            raise AcceptanceError(
                "runner.sshd_banner_invalid",
                "The authorized external endpoint did not return an SSH protocol banner.",
            )
        architecture = self._remote_command(["uname", "-m"]).strip()
        normalized_architecture = {"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64"}.get(
            architecture
        )
        if normalized_architecture != self.options.ssh_machine_arch:
            raise AcceptanceError(
                "runner.ssh_external_arch_mismatch",
                "The external SSH host architecture did not match --ssh-machine-arch.",
                {
                    "expected": self.options.ssh_machine_arch,
                    "actual": normalized_architecture or "unsupported",
                },
            )
        if self.options.ssh_external_use_sudo:
            self._remote_command(["sudo", "-n", "true"])
        preflight_script = "\n".join(
            [
                "test \"$(cat /proc/1/comm)\" = systemd",
                f"id -u {shlex.quote(self.options.ssh_external_service_user)} >/dev/null",
                "for tool in sh env curl tar xz sha256sum git systemctl install base64; do command -v \"$tool\" >/dev/null; done",
                "systemctl is-active --quiet ssh || systemctl is-active --quiet sshd",
                f"test ! -e {shlex.quote(self.external_runtime_root)}",
                f"test ! -e {shlex.quote(self.external_stage_root)}",
            ]
        )
        try:
            self._remote_root_command(["sh", "-ceu", preflight_script])
        except AcceptanceError as error:
            raise AcceptanceError(
                "runner.ssh_external_preflight_failed",
                "The external SSH host failed the systemd, tool, service-user, or path-conflict preflight.",
                {"causeCode": error.code},
            ) from None
        self.external_runtime_created = True
        self._remote_command(["install", "-d", "-m", "0700", self.external_stage_root])
        if self.options.suite == "real-provider-smoke":
            sources = (
                self.provider_host_bundle_path,
                self.provider_tools_package_path,
                self.provider_tools_lock_path,
            )
        else:
            sources = (self.fixture_bundle_path,)
        for source in sources:
            self._remote_upload(source, f"{self.external_stage_root}/{source.name}", "0600")
        self._remote_root_command(
            ["sh", "-ceu", self._external_setup_script()],
            log_path=self.logs_dir / "ssh-external-runtime-setup.log",
            maximum_timeout=max(240.0, self.deadline.remaining()),
        )
        self.machine_created = True
        provider_runtime = (
            self._inspect_ssh_provider_runtime()
            if self.options.suite == "real-provider-smoke"
            else {"kind": "deterministic-fixture"}
        )
        return {
            "runtime": "authorized-external-host",
            "ownedMachine": False,
            "externalHostAuthorized": self.options.ssh_allow_external_host,
            "externalHostAddressPersisted": False,
            "installationId": self.installation_id,
            "machineArch": self.options.ssh_machine_arch,
            "nodeVersion": self.options.ssh_node_version,
            "providerRuntime": provider_runtime,
            "sshd": "active-not-restarted",
            "initSystem": "systemd",
            "hostKeyFingerprint": self._host_key_fingerprint(self.host_key),
            "credentialSource": "repository-external operator identity",
            "operatorIdentitySourcePreserved": True,
            "driverPrivateKeyReferenceClearedAfterProvision": True,
            "controlPlaneCredentialLifecycle": self.credential_lifecycle,
            "controlPlaneTransport": {
                "mode": "reverse-ssh-loopback",
                "description": SSH_RELAY_TRANSPORT,
                "vmListenHost": SSH_RELAY_LOOPBACK_HOST,
            },
        }

    def _load_external_host_key(self) -> str:
        source = self.options.ssh_external_host_key_file
        if source is None:
            raise AcceptanceError(
                "runner.ssh_external_host_key_missing",
                "The external SSH Host Key source was unavailable.",
            )
        try:
            lines = [
                line.strip()
                for line in source.read_text(encoding="utf-8").splitlines()
                if line.strip() and not line.lstrip().startswith("#")
            ]
        except (OSError, UnicodeError):
            lines = []
        if len(lines) != 1:
            raise AcceptanceError(
                "runner.ssh_external_host_key_invalid",
                "The external SSH Host Key source must contain exactly one pinned key.",
            )
        fields = lines[0].split()
        key_types = ("ssh-ed25519", "ssh-rsa", "ecdsa-sha2-")
        if len(fields) >= 2 and fields[0].startswith(key_types):
            host_key = " ".join(fields[:2])
        elif len(fields) >= 3 and fields[1].startswith(key_types):
            host_tokens = fields[0].split(",")
            if (
                self._known_hosts_token() not in host_tokens
                or any(token.startswith("|") or "*" in token or "?" in token for token in host_tokens)
            ):
                raise AcceptanceError(
                    "runner.ssh_external_host_key_invalid",
                    "The approved known-host source did not pin the explicit external endpoint.",
                )
            host_key = " ".join(fields[1:3])
        else:
            raise AcceptanceError(
                "runner.ssh_external_host_key_invalid",
                "The external SSH Host Key source contained an unsupported entry.",
            )
        self._host_key_fingerprint(host_key)
        return host_key

    def _external_setup_script(self) -> str:
        node_arch = "x64" if self.options.ssh_machine_arch == "amd64" else "arm64"
        version = self.options.ssh_node_version
        archive = f"node-v{version}-linux-{node_arch}.tar.xz"
        root = self.external_runtime_root
        stage = self.external_stage_root
        node_root = f"{root}/node"
        build_home = f"{root}/build-home"
        npm_cache = f"{root}/npm-cache"
        provider_home = f"{root}/provider-home"
        node_download = shlex.join(("curl", *SSH_EXTERNAL_NODE_DOWNLOAD_CURL_ARGUMENTS))
        if self.options.suite == "real-provider-smoke":
            runtime_install = [
                f"install -d -m 0755 {shlex.quote(pathlib.PurePosixPath(self.remote_provider_host_path).parent.as_posix())}",
                f"install -d -m 0755 {shlex.quote(self.remote_provider_tools_root)} {shlex.quote(pathlib.PurePosixPath(self.remote_provider_host_command_path).parent.as_posix())}",
                f"install -m 0644 {shlex.quote(stage + '/' + self.provider_host_bundle_path.name)} {shlex.quote(self.remote_provider_host_path)}",
                f"install -m 0644 {shlex.quote(stage + '/' + self.provider_tools_package_path.name)} {shlex.quote(self.remote_provider_tools_root + '/package.json')}",
                f"install -m 0644 {shlex.quote(stage + '/' + self.provider_tools_lock_path.name)} {shlex.quote(self.remote_provider_tools_root + '/package-lock.json')}",
                f"cd {shlex.quote(self.remote_provider_tools_root)}",
                f"HOME={shlex.quote(build_home)} npm_config_cache={shlex.quote(npm_cache)} PATH={shlex.quote(node_root + '/bin')}:$PATH npm ci --omit=dev --ignore-scripts --no-audit --no-fund",
                f"HOME={shlex.quote(build_home)} {shlex.quote(self.remote_node_path)} node_modules/@anthropic-ai/claude-code/install.cjs",
                f"HOME={shlex.quote(build_home)} npm_config_cache={shlex.quote(npm_cache)} PATH={shlex.quote(node_root + '/bin')}:$PATH npm cache clean --force",
                f"cat > {shlex.quote(self.remote_provider_host_command_path)} <<'EOF'",
                "#!/bin/sh",
                f"export HOME={provider_home}",
                f"export XDG_CONFIG_HOME={provider_home}/.config",
                f"export XDG_CACHE_HOME={provider_home}/.cache",
                f"export XDG_STATE_HOME={provider_home}/.local/state",
                f"export CODEX_HOME={provider_home}/.codex",
                f"export CLAUDE_CONFIG_DIR={provider_home}/.claude",
                f"export PATH={self._external_provider_runtime_path()}",
                f'exec {self.remote_node_path} {self.remote_provider_host_path} "$@"',
                "EOF",
                f"chmod 0755 {shlex.quote(self.remote_provider_host_command_path)}",
                f"test -x {shlex.quote(self.remote_provider_tools_root + '/node_modules/.bin/codex')}",
                f"test -x {shlex.quote(self.remote_provider_tools_root + '/node_modules/.bin/claude')}",
            ]
        else:
            runtime_install = [
                f"install -m 0644 {shlex.quote(stage + '/' + self.fixture_bundle_path.name)} {shlex.quote(self.remote_fixture_path)}",
            ]
        return "\n".join(
            [
                f"test ! -e {shlex.quote(root)}",
                f"install -d -m 0755 {shlex.quote(root)}",
                f"printf '%s\\n' {shlex.quote(self.installation_id)} > {shlex.quote(root + '/.synara-owner')}",
                f"chmod 0600 {shlex.quote(root + '/.synara-owner')}",
                f"install -d -m 0700 {shlex.quote(build_home)} {shlex.quote(npm_cache)} {shlex.quote(provider_home)}",
                f"chown -R {shlex.quote(self.options.ssh_external_service_user + ':')} {shlex.quote(provider_home)}",
                f"workdir=$(mktemp -d {shlex.quote(stage + '/node.XXXXXX')})",
                "trap 'rm -rf \"$workdir\"' EXIT",
                "cd \"$workdir\"",
                f"{node_download} https://nodejs.org/dist/v{version}/{archive}",
                f"{node_download} https://nodejs.org/dist/v{version}/SHASUMS256.txt",
                f"grep '  {archive}$' SHASUMS256.txt | sha256sum -c -",
                f"install -d -m 0755 {shlex.quote(node_root)}",
                f"tar -xJf {shlex.quote(archive)} -C {shlex.quote(node_root)} --strip-components=1",
                f"test \"$({shlex.quote(self.remote_node_path)} --version)\" = v{version}",
                *runtime_install,
                f"rm -rf {shlex.quote(stage)}",
            ]
        )

    def _external_provider_runtime_path(self) -> str:
        if not self.external_host:
            raise AssertionError("external Provider runtime PATH requires an external SSH host")
        return ":".join(
            (
                f"{self.remote_provider_tools_root}/node_modules/.bin",
                pathlib.PurePosixPath(self.remote_node_path).parent.as_posix(),
                "/usr/local/sbin",
                "/usr/local/bin",
                "/usr/sbin",
                "/usr/bin",
                "/sbin",
                "/bin",
            )
        )

    def _remote_provider_cli_version(self, executable: str) -> str:
        command = [executable, "--version"]
        if self.external_host:
            command = ["env", f"PATH={self._external_provider_runtime_path()}", *command]
        return self._remote_command(command).strip()

    def _remove_external_runtime(self) -> None:
        if not self.external_host or not self.external_runtime_created:
            return
        root = self.external_runtime_root
        stage = self.external_stage_root
        marker = f"{root}/.synara-owner"
        script = "\n".join(
            [
                f"if test -e {shlex.quote(root)}; then",
                f"  test -f {shlex.quote(marker)}",
                f"  test \"$(cat {shlex.quote(marker)})\" = {shlex.quote(self.installation_id)}",
                f"  rm -rf -- {shlex.quote(root)}",
                "fi",
                f"rm -rf -- {shlex.quote(stage)}",
                f"test ! -e {shlex.quote(root)}",
                f"test ! -e {shlex.quote(stage)}",
            ]
        )
        self._remote_root_command(["sh", "-ceu", script], cleanup_timeout=30.0)
        self.external_runtime_created = False
        self.machine_created = False

    def _inspect_ssh_provider_runtime(self) -> Mapping[str, Any]:
        try:
            package = json.loads(self.provider_tools_package_path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as error:
            raise AcceptanceError(
                "runner.ssh_provider_tools_lock_invalid",
                f"SSH Provider tools package metadata was invalid: {error}",
            ) from None
        dependencies = package.get("dependencies") if isinstance(package, dict) else None
        codex_version = dependencies.get("@openai/codex") if isinstance(dependencies, dict) else None
        claude_version = (
            dependencies.get("@anthropic-ai/claude-code")
            if isinstance(dependencies, dict)
            else None
        )
        if not isinstance(codex_version, str) or not isinstance(claude_version, str):
            raise AcceptanceError(
                "runner.ssh_provider_tools_lock_invalid",
                "SSH Provider tools package metadata omitted locked Codex or Claude versions.",
            )
        codex_output = self._remote_provider_cli_version(
            f"{self.remote_provider_tools_root}/node_modules/.bin/codex"
        )
        claude_output = self._remote_provider_cli_version(
            f"{self.remote_provider_tools_root}/node_modules/.bin/claude"
        )
        if codex_version not in codex_output or claude_version not in claude_output:
            raise AcceptanceError(
                "runner.ssh_provider_tools_version_mismatch",
                "SSH Provider CLI versions did not match the locked package inputs.",
                {
                    "expectedCodexVersion": codex_version,
                    "expectedClaudeVersion": claude_version,
                },
            )
        provider_host_sha = self._remote_command(
            ["sha256sum", self.remote_provider_host_path]
        ).split(maxsplit=1)[0]
        expected_provider_host_sha = hashlib.sha256(
            self.provider_host_bundle_path.read_bytes()
        ).hexdigest()
        if provider_host_sha != expected_provider_host_sha:
            raise AcceptanceError(
                "runner.ssh_provider_host_digest_mismatch",
                "SSH Provider Host bundle digest did not match the local build.",
            )
        return {
            "kind": "real-provider",
            "providerHost": {
                "command": self.remote_provider_host_command_path,
                "remotePath": self.remote_provider_host_path,
                "sha256": provider_host_sha,
            },
            "providerTools": {
                "remoteRoot": self.remote_provider_tools_root,
                "lockedInstall": True,
                "codex": {"version": codex_version, "versionOutput": codex_output[:500]},
                "claudeAgent": {
                    "version": claude_version,
                    "versionOutput": claude_output[:500],
                },
            },
        }

    def _machine_setup_script(self) -> str:
        node_arch = "x64" if self.options.ssh_machine_arch == "amd64" else "arm64"
        version = self.options.ssh_node_version
        archive = f"node-v{version}-linux-{node_arch}.tar.xz"
        stage = "/tmp/synara-stage3-acceptance"
        if self.options.suite == "real-provider-smoke":
            runtime_install = [
                f"install -d -m 0755 {pathlib.PurePosixPath(SSH_REMOTE_PROVIDER_HOST_PATH).parent}",
                f"install -d -m 0755 {SSH_REMOTE_PROVIDER_TOOLS_ROOT}",
                f"install -m 0644 {stage}/{self.provider_host_bundle_path.name} {SSH_REMOTE_PROVIDER_HOST_PATH}",
                f"install -m 0644 {stage}/{self.provider_tools_package_path.name} {SSH_REMOTE_PROVIDER_TOOLS_ROOT}/package.json",
                f"install -m 0644 {stage}/{self.provider_tools_lock_path.name} {SSH_REMOTE_PROVIDER_TOOLS_ROOT}/package-lock.json",
                f"cd {SSH_REMOTE_PROVIDER_TOOLS_ROOT}",
                "npm ci --omit=dev --ignore-scripts --no-audit --no-fund",
                "node node_modules/@anthropic-ai/claude-code/install.cjs",
                "npm cache clean --force",
                f"cat > {SSH_PROVIDER_HOST_COMMAND_PATH} <<'EOF'",
                "#!/bin/sh",
                f"export PATH={SSH_REMOTE_PROVIDER_TOOLS_ROOT}/node_modules/.bin:$PATH",
                f'exec node {SSH_REMOTE_PROVIDER_HOST_PATH} "$@"',
                "EOF",
                f"chmod 0755 {SSH_PROVIDER_HOST_COMMAND_PATH}",
                f"test -x {SSH_REMOTE_PROVIDER_TOOLS_ROOT}/node_modules/.bin/codex",
                f"test -x {SSH_REMOTE_PROVIDER_TOOLS_ROOT}/node_modules/.bin/claude",
            ]
        else:
            runtime_install = [
                f"install -d -m 0755 {pathlib.PurePosixPath(SSH_REMOTE_FIXTURE_PATH).parent}",
                f"install -m 0644 {stage}/{self.fixture_bundle_path.name} {SSH_REMOTE_FIXTURE_PATH}",
            ]
        return "\n".join(
            [
                "export DEBIAN_FRONTEND=noninteractive",
                "apt-get update",
                "apt-get install -y --no-install-recommends ca-certificates curl git openssh-client openssh-server xz-utils",
                f"id -u {SSH_SERVICE_USER} >/dev/null",
                "install -d -m 0700 /root/.ssh",
                f"install -m 0600 {stage}/{self.client_public_key_path.name} /root/.ssh/authorized_keys",
                "workdir=$(mktemp -d /tmp/synara-node.XXXXXX)",
                "trap 'rm -rf \"$workdir\"' EXIT",
                "cd \"$workdir\"",
                f"curl -fsSLO https://nodejs.org/dist/v{version}/{archive}",
                f"curl -fsSLO https://nodejs.org/dist/v{version}/SHASUMS256.txt",
                f"grep '  {archive}$' SHASUMS256.txt | sha256sum -c -",
                f"tar -xJf {archive} -C /usr/local --strip-components=1",
                f"test \"$(node --version)\" = v{version}",
                *runtime_install,
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
                        "port": self._ssh_port(),
                        "user": self._ssh_user(),
                        "privateKey": self.client_private_key,
                        "hostKey": host_key,
                        "controlPlaneUrl": self._worker_proxy_url(),
                        "allowInsecureControlPlane": True,
                        "runnerCommand": self._runner_command(),
                        "serviceUser": (
                            self.options.ssh_external_service_user
                            if self.external_host
                            else SSH_SERVICE_USER
                        ),
                        "useSudo": (
                            self.options.ssh_external_use_sudo
                            if self.external_host
                            else False
                        ),
                        **(
                            {
                                "installRoot": self.external_install_root,
                                "workspaceRoot": self.external_workspace_root,
                                "gitCacheRoot": self.external_git_cache_root,
                            }
                            if self.external_host
                            else {}
                        ),
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
        install_root = (
            self.external_install_root
            if self.external_host
            else f"/opt/synara/targets/{target_id}"
        )
        temporary = f"/tmp/synara-agentd-{target_id}"
        unit_path = f"/etc/systemd/system/{service_name}"
        script = "\n".join(
            [
                f"if systemctl cat {service_name} >/dev/null 2>&1; then exit 1; fi",
                f"test ! -e {unit_path}",
                f"test ! -e {install_root}/synara-agentd",
                f"test ! -e {install_root}/agentd.env",
                f"test ! -e {temporary}",
                f"test ! -e {temporary}.env",
                f"test ! -e {temporary}.service",
            ]
        )
        self._remote_root_command(["sh", "-ceu", script], cleanup_timeout=cleanup_timeout)

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
        if self.external_host:
            del user
            return self._external_ssh_command(
                command,
                log_path=log_path,
                maximum_timeout=maximum_timeout,
                cleanup_timeout=cleanup_timeout,
                input_text=input_text,
            )
        return self._orbctl_command(
            ["run", "--machine", self.machine_name, "--user", user, *command],
            log_path=log_path,
            maximum_timeout=maximum_timeout,
            cleanup_timeout=cleanup_timeout,
            input_text=input_text,
        )

    def _external_ssh_command(
        self,
        command: Sequence[str],
        *,
        log_path: pathlib.Path | None = None,
        maximum_timeout: float | None = None,
        cleanup_timeout: float | None = None,
        input_text: str | None = None,
    ) -> str:
        if not command or not self.machine_ip or not self.host_key:
            raise AcceptanceError(
                "runner.ssh_external_command_invalid",
                "The external SSH command boundary was incomplete.",
            )
        self._write_known_hosts_file()
        timeout = cleanup_timeout
        if timeout is None:
            timeout = self.deadline.request_timeout(maximum=maximum_timeout or 30.0)
        arguments = [
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
            "UpdateHostKeys=no",
            "-o",
            "LogLevel=ERROR",
            "-o",
            "ConnectTimeout=10",
            "-p",
            str(self._ssh_port()),
            "-i",
            str(self.identity_path),
            self._ssh_destination(),
            shlex.join(list(command)),
        ]
        try:
            completed = subprocess.run(
                arguments,
                cwd=self.repo_root,
                env=self._tool_environment(),
                input=input_text,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                text=True,
                timeout=timeout,
                check=False,
            )
        except subprocess.TimeoutExpired:
            raise AcceptanceError(
                "runner.ssh_external_command_failed",
                "The pinned external SSH command could not run.",
                {
                    "failureKind": "timeout",
                    "remoteExecutable": pathlib.PurePosixPath(command[0]).name,
                    "timeoutSeconds": timeout,
                },
            ) from None
        except OSError:
            raise AcceptanceError(
                "runner.ssh_external_command_failed",
                "The pinned external SSH command could not run.",
                {
                    "failureKind": "process-start",
                    "remoteExecutable": pathlib.PurePosixPath(command[0]).name,
                },
            ) from None
        output = self.redactor.text(completed.stdout)
        if log_path is not None:
            log_path.parent.mkdir(parents=True, exist_ok=True)
            log_path.write_text(output, encoding="utf-8")
        if completed.returncode != 0:
            raise AcceptanceError(
                "runner.ssh_external_command_failed",
                "The pinned external SSH command failed.",
                {
                    "remoteExecutable": pathlib.PurePosixPath(command[0]).name,
                    "exitCode": completed.returncode,
                    "log": str(log_path) if log_path else None,
                    "outputExcerpt": output[-1000:],
                },
            )
        return output

    def _remote_root_command(
        self,
        command: Sequence[str],
        **kwargs: Any,
    ) -> str:
        if not self.external_host or not self.options.ssh_external_use_sudo:
            return self._remote_command(command, **kwargs)
        return self._remote_command(["sudo", "-n", *command], **kwargs)

    def _write_known_hosts_file(self) -> None:
        if not self.host_key:
            raise AcceptanceError(
                "runner.ssh_host_key_invalid",
                "A pinned SSH Host Key is required before opening external transport.",
            )
        self.credentials_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
        os.chmod(self.credentials_dir, 0o700)
        self.known_hosts_path.write_text(
            f"{self._known_hosts_token()} {self.host_key}\n",
            encoding="utf-8",
        )
        os.chmod(self.known_hosts_path, 0o600)

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
        if self.external_host:
            return
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
        self.api_server_override = options.kubernetes_api_server
        self.tls_server_name_override = options.kubernetes_tls_server_name
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

    @property
    def uses_local_image_store(self) -> bool:
        return self.context.startswith("kind-") or self.options.kubernetes_shared_local_image_store

    @property
    def image_pull_policy(self) -> str:
        return "Never" if self.uses_local_image_store else "IfNotPresent"

    def create_provider_fault_server(self, provider: str, fault: str) -> _ProviderFaultServer:
        return _ProviderFaultServer(
            provider,
            fault,
            listen_host="0.0.0.0",
            advertised_host=self.worker_proxy_host,
        )

    def probe_provider_fault_server(
        self,
        server: _ProviderFaultServer,
    ) -> Mapping[str, Any]:
        return {
            "target": self.name,
            "transport": "host-gateway",
            "advertisedHost": self.worker_proxy_host,
            "expectedStatus": 401 if server.fault == "authentication" else 429,
            "validationMode": "controlled-provider-request",
            "probedFromWorker": False,
            "endpointPersisted": False,
        }

    def crash_provider_host(self) -> Mapping[str, Any]:
        pod = self._required_active_target_pod("Provider Host crash injection")
        output = self._kubectl_command(
            [
                "-n",
                str(pod["namespace"]),
                "exec",
                str(pod["name"]),
                "-c",
                "agentd",
                "--",
                "node",
                "-e",
                provider_host_crash_script(),
                "1",
            ],
            cleanup_timeout=15.0,
        )
        return provider_host_crash_evidence(
            output,
            target=self.name,
            scope={
                "namespace": pod["namespace"],
                "podName": pod["name"],
                "podUid": pod["uid"],
                "executionId": pod["executionId"],
                "scopedToExecutionPod": True,
            },
        )

    def _required_active_target_pod(self, operation: str) -> dict[str, str]:
        target_id = self.target_id
        if target_id is None:
            raise AcceptanceError(
                "runner.kubernetes_target_id_missing",
                f"Kubernetes {operation} was requested before Target provisioning.",
            )
        runtime = self._target_runtime(target_id)
        namespace = runtime["namespace"]
        try:
            payload = json.loads(
                self._kubectl_command(
                    [
                        "-n",
                        namespace,
                        "get",
                        "pods",
                        "-l",
                        f"synara.io/execution-target-id={target_id}",
                        "-o",
                        "json",
                    ]
                )
            )
        except json.JSONDecodeError:
            raise AcceptanceError(
                "runner.kubernetes_pods_invalid",
                f"Kubernetes Pod inventory was invalid during {operation}.",
            ) from None
        items = payload.get("items") if isinstance(payload, dict) else None
        if not isinstance(items, list) or not all(isinstance(item, dict) for item in items):
            raise AcceptanceError(
                "runner.kubernetes_pods_invalid",
                f"Kubernetes Pod inventory was malformed during {operation}.",
            )
        running = [
            item
            for item in items
            if isinstance(item.get("metadata"), dict)
            and item["metadata"].get("deletionTimestamp") is None
            and isinstance(item.get("status"), dict)
            and item["status"].get("phase") == "Running"
        ]
        if len(running) != 1:
            raise AcceptanceError(
                "runner.kubernetes_active_pod_ambiguous",
                f"Expected exactly one running execution-pinned Pod during {operation}.",
                {
                    "targetId": target_id,
                    "namespace": namespace,
                    "runningPodCount": len(running),
                    "totalPodCount": len(items),
                },
            )
        pod = running[0]
        metadata = json_object(pod.get("metadata"), "Kubernetes active Pod metadata")
        labels = json_object(metadata.get("labels"), "Kubernetes active Pod labels")
        spec = json_object(pod.get("spec"), "Kubernetes active Pod spec")
        containers = spec.get("containers")
        name = metadata.get("name")
        uid = metadata.get("uid")
        execution_id = labels.get("synara.io/execution-id")
        if (
            not isinstance(name, str)
            or not name
            or not isinstance(uid, str)
            or not uid
            or not isinstance(execution_id, str)
            or not execution_id
            or not isinstance(containers, list)
            or len(containers) != 1
            or not isinstance(containers[0], dict)
            or containers[0].get("name") != "agentd"
        ):
            raise AcceptanceError(
                "runner.kubernetes_pod_contract_mismatch",
                f"The active Kubernetes Pod omitted its scoped identity during {operation}.",
                {"targetId": target_id, "namespace": namespace},
            )
        return {
            "namespace": namespace,
            "name": name,
            "uid": uid,
            "executionId": execution_id,
        }

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
            image_transport = "kind-load"
        elif self.options.kubernetes_shared_local_image_store:
            image_transport = "shared-local-container-engine"
        elif not self.options.kubernetes_skip_worker_build:
            raise AcceptanceError(
                "runner.kubernetes_image_load_unsupported",
                "A locally built Kubernetes Worker image requires Kind or an explicitly shared local image store.",
                {
                    "context": self.context,
                    "requiredInputs": [
                        "--kubernetes-shared-local-image-store",
                        "or --kubernetes-skip-worker-build",
                    ],
                },
            )
        else:
            image_transport = "operator-provided-image"
        access_evidence = self._prepare_cluster_access()
        return {
            "controlPlane": control_plane,
            "kubernetes": {
                **cluster_evidence,
                **access_evidence,
                "resourceOwner": self.resource_owner,
                "containerEngine": {
                    **image_evidence,
                    "clusterImageTransport": image_transport,
                    "imagePullPolicy": self.image_pull_policy,
                },
            },
        }

    def _prepare_canary_image(self) -> Mapping[str, Any]:
        if self.canary_image_prepared:
            return {"image": self.canary_image, "prepared": True}
        if not self.uses_local_image_store:
            raise AcceptanceUnsupported(
                "runner.kubernetes_canary_registry_required",
                "A Kubernetes canary requires a local image store or a caller-published immutable image.",
                {
                    "context": self.context,
                    "requiredInputs": [
                        "--kubernetes-shared-local-image-store",
                        "or published immutable canary image and product release revision",
                    ],
                },
            )
        self._docker_command(
            ["image", "tag", self.image, self.canary_image],
            log_path=self.logs_dir / "kubernetes-canary-tag.log",
        )
        if self.context.startswith("kind-"):
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
            "clusterImageTransport": (
                "kind-load" if self.context.startswith("kind-") else "shared-local-container-engine"
            ),
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
                "imagePullPolicy": self.image_pull_policy,
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
                "imagePullPolicy": self.image_pull_policy,
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
        max_active_pods: int = 1,
        experimental_providers: Sequence[str] | None = None,
    ) -> dict[str, Any]:
        if not self.api_server or not self.ca_certificate or not self.kubernetes_token:
            raise AcceptanceError(
                "runner.kubernetes_access_unavailable",
                "Kubernetes API access was unavailable while provisioning the Target.",
            )
        enabled_providers = list(
            dict.fromkeys(experimental_providers or (provider,))
        )
        if provider not in enabled_providers or any(
            enabled_provider not in PROVIDERS for enabled_provider in enabled_providers
        ):
            raise AcceptanceError(
                "runner.kubernetes_provider_policy_invalid",
                "The Kubernetes Target Provider policy omitted its primary Provider or contained an unsupported Provider.",
                {
                    "provider": provider,
                    "experimentalProviders": enabled_providers,
                },
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
                        "imagePullPolicy": self.image_pull_policy,
                        "controlPlaneUrl": self._worker_proxy_url(),
                        "allowInsecureControlPlane": True,
                        "runnerCommand": list(self.options.runner_command),
                        "maxActivePods": max_active_pods,
                        "egressCidrs": ["0.0.0.0/0"],
                        **dict(KUBERNETES_ACCEPTANCE_RESOURCE_CONFIGURATION),
                    },
                    "capabilities": {
                        "workspaceModes": ["local", "worktree"],
                        "providerPolicy": {
                            "experimentalProviders": enabled_providers
                        },
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
        if fault not in {
            "kubernetes-drain",
            "kubernetes-pdb-drain",
            "kubernetes-eviction",
        }:
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

        self._require_node_drain_authorization()
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
        if fault == "kubernetes-pdb-drain":
            return self._inject_pdb_drain(
                target_id=target_id,
                execution_id=execution_id,
                namespace=namespace,
                pod_name=name,
                pod_uid=uid,
                node_name=node_name,
                selector=selector,
                generation=labels.get("synara.io/generation"),
            )
        self._kubectl_command(["cordon", node_name], cleanup_timeout=15.0)
        started = time.monotonic()
        try:
            self._kubectl_command(
                self._node_drain_arguments(
                    node_name,
                    selector,
                    disable_eviction=True,
                    timeout="45s",
                ),
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

    def _inject_pdb_drain(
        self,
        *,
        target_id: str,
        execution_id: str,
        namespace: str,
        pod_name: str,
        pod_uid: str,
        node_name: str,
        selector: str,
        generation: Any,
    ) -> Mapping[str, Any]:
        alternate_nodes = self._require_pdb_drain_nodes(source_node=node_name)
        compact_execution_id = re.sub(r"[^a-z0-9]", "", execution_id.lower())[:16]
        pdb_name = f"synara-drain-{compact_execution_id or uuid.uuid4().hex[:12]}"
        match_labels = {
            "synara.io/execution-target-id": target_id,
            "synara.io/execution-id": execution_id,
        }
        manifest = {
            "apiVersion": "policy/v1",
            "kind": "PodDisruptionBudget",
            "metadata": {
                "name": pdb_name,
                "namespace": namespace,
                "labels": self._ownership_labels(),
            },
            "spec": {
                "minAvailable": 1,
                "selector": {"matchLabels": match_labels},
            },
        }
        pdb_created = False
        started = time.monotonic()
        blocked_duration_ms = 0
        pdb_evidence: Mapping[str, Any] = {}
        replacement: Mapping[str, Any] = {}
        self._kubectl_command(["cordon", node_name], cleanup_timeout=15.0)
        try:
            pdb_created = True
            self._kubectl_command(
                ["-n", namespace, "apply", "-f", "-"],
                input_text=json.dumps(manifest, separators=(",", ":")),
                cleanup_timeout=20.0,
            )
            pdb_evidence = self._wait_pdb_blocking(namespace, pdb_name)
            blocked_started = time.monotonic()
            blocked = self._kubectl_completed(
                self._node_drain_arguments(
                    node_name,
                    selector,
                    disable_eviction=False,
                    timeout="8s",
                ),
                cleanup_timeout=15.0,
            )
            blocked_duration_ms = elapsed_ms(blocked_started)
            blocked_output = self.redactor.text(blocked.stdout).lower()
            if blocked.returncode == 0 or not any(
                marker in blocked_output
                for marker in ("disruption budget", "cannot evict pod")
            ):
                raise AcceptanceError(
                    "runner.kubernetes_pdb_drain_not_blocked",
                    "The exact PodDisruptionBudget did not block the first Node drain attempt.",
                    {
                        "node": node_name,
                        "podName": pod_name,
                        "pdbName": pdb_name,
                        "exitCode": blocked.returncode,
                    },
                )
            blocked_pod = self._wait_execution_pod(target_id, execution_id)
            blocked_metadata = json_object(
                blocked_pod.get("metadata"),
                "Kubernetes PDB-blocked Pod metadata",
            )
            blocked_spec = json_object(
                blocked_pod.get("spec"),
                "Kubernetes PDB-blocked Pod spec",
            )
            if blocked_metadata.get("uid") != pod_uid or blocked_spec.get("nodeName") != node_name:
                raise AcceptanceError(
                    "runner.kubernetes_pdb_drain_pod_changed",
                    "The PDB-blocked drain did not preserve the exact running Pod on its source Node.",
                    {
                        "expectedPodUid": pod_uid,
                        "actualPodUid": blocked_metadata.get("uid"),
                        "expectedNode": node_name,
                        "actualNode": blocked_spec.get("nodeName"),
                    },
                )
            self._kubectl_command(
                ["-n", namespace, "delete", "poddisruptionbudget", pdb_name, "--wait=true"],
                cleanup_timeout=20.0,
            )
            pdb_created = False
            self._kubectl_command(
                self._node_drain_arguments(
                    node_name,
                    selector,
                    disable_eviction=True,
                    timeout="45s",
                ),
                cleanup_timeout=55.0,
            )
            replacement = self._wait_replacement_execution_pod(
                target_id,
                execution_id,
                stale_uid=pod_uid,
                stale_node=node_name,
            )
        finally:
            try:
                if pdb_created:
                    self._kubectl_command(
                        [
                            "-n",
                            namespace,
                            "delete",
                            "poddisruptionbudget",
                            pdb_name,
                            "--ignore-not-found",
                            "--wait=true",
                        ],
                        cleanup_timeout=20.0,
                    )
            finally:
                self._kubectl_command(["uncordon", node_name], cleanup_timeout=15.0)
        return {
            "fault": "kubernetes-pdb-drain",
            "node": node_name,
            "selector": selector,
            "deletedPodName": pod_name,
            "deletedPodUid": pod_uid,
            "deletedPodGeneration": generation,
            "namespace": namespace,
            "pdbName": pdb_name,
            "pdb": pdb_evidence,
            "pdbBlockedDrain": True,
            "pdbRemovedBeforeDrain": True,
            "blockedDurationMs": blocked_duration_ms,
            "durationMs": elapsed_ms(started),
            "uncordoned": True,
            "alternateReadyNodeCount": len(alternate_nodes),
            "replacementPodName": replacement.get("name"),
            "replacementPodUid": replacement.get("uid"),
            "replacementPodGeneration": replacement.get("generation"),
            "replacementNode": replacement.get("node"),
            "multiNodeRescheduled": replacement.get("node") != node_name,
            "deleteMechanism": "PDB-blocked Eviction attempt followed by exact graceful Pod DELETE",
        }

    @staticmethod
    def _node_drain_arguments(
        node_name: str,
        selector: str,
        *,
        disable_eviction: bool,
        timeout: str,
    ) -> list[str]:
        arguments = [
            "drain",
            node_name,
            f"--pod-selector={selector}",
            "--ignore-daemonsets",
            "--delete-emptydir-data",
            "--force",
        ]
        if disable_eviction:
            arguments.append("--disable-eviction")
        arguments.extend(["--grace-period=20", f"--timeout={timeout}"])
        return arguments

    def _require_node_drain_authorization(self) -> None:
        if self.owns_cluster or self.options.kubernetes_allow_node_drain:
            return
        raise AcceptanceUnsupported(
            "runner.kubernetes_node_drain_not_authorized",
            "Node drain is disabled for an operator-owned Kubernetes context.",
            {
                "context": self.context,
                "requiredInputs": ["--kubernetes-allow-node-drain"],
            },
        )

    def _require_pdb_drain_nodes(self, *, source_node: str | None = None) -> tuple[str, ...]:
        schedulable_nodes = self._ready_schedulable_node_names()
        candidates = tuple(
            node for node in schedulable_nodes if source_node is None or node != source_node
        )
        if len(schedulable_nodes) >= 2 and candidates:
            return candidates
        evidence: dict[str, Any] = {
            "schedulableNodeCount": len(schedulable_nodes),
            "requiredInputs": (
                ["--kind-worker-nodes", "2"]
                if self.owns_cluster
                else ["at least two Ready schedulable Nodes"]
            ),
        }
        if source_node is not None:
            evidence["sourceNode"] = source_node
        raise AcceptanceUnsupported(
            "runner.kubernetes_pdb_drain_requires_multi_node",
            "PDB drain recovery requires at least two Ready schedulable Kubernetes Nodes.",
            evidence,
        )

    def _wait_pdb_blocking(self, namespace: str, pdb_name: str) -> Mapping[str, Any]:
        def pdb_probe() -> Mapping[str, Any] | None:
            completed = self._kubectl_completed(
                ["-n", namespace, "get", "poddisruptionbudget", pdb_name, "-o", "json"],
                cleanup_timeout=5.0,
            )
            if completed.returncode != 0:
                return None
            try:
                payload = json.loads(completed.stdout)
                metadata = json_object(payload.get("metadata"), "PodDisruptionBudget metadata")
                status = json_object(payload.get("status"), "PodDisruptionBudget status")
            except (AcceptanceError, json.JSONDecodeError):
                return None
            generation = metadata.get("generation")
            if (
                not isinstance(generation, int)
                or status.get("observedGeneration") != generation
                or status.get("currentHealthy") != 1
                or status.get("desiredHealthy") != 1
                or status.get("disruptionsAllowed") != 0
            ):
                return None
            return {
                "minAvailable": 1,
                "currentHealthy": status.get("currentHealthy"),
                "desiredHealthy": status.get("desiredHealthy"),
                "expectedPods": status.get("expectedPods"),
                "disruptionsAllowed": status.get("disruptionsAllowed"),
                "observedGeneration": status.get("observedGeneration"),
            }

        return self.api.wait_until(
            f"PodDisruptionBudget {namespace}/{pdb_name} to block eviction",
            pdb_probe,
            interval=0.2,
        )

    def _wait_replacement_execution_pod(
        self,
        target_id: str,
        execution_id: str,
        *,
        stale_uid: str,
        stale_node: str,
    ) -> Mapping[str, Any]:
        while True:
            candidates: list[dict[str, Any]] = []
            for pod in self._execution_pods(target_id, execution_id):
                metadata = pod.get("metadata")
                status = pod.get("status")
                if (
                    isinstance(metadata, dict)
                    and metadata.get("uid") != stale_uid
                    and metadata.get("deletionTimestamp") is None
                    and isinstance(status, dict)
                    and status.get("phase") == "Running"
                ):
                    candidates.append(pod)
            if len(candidates) > 1:
                raise AcceptanceError(
                    "runner.kubernetes_replacement_pod_ambiguous",
                    "More than one running replacement Pod existed for the drained Execution.",
                    {
                        "targetId": target_id,
                        "executionId": execution_id,
                        "replacementPodCount": len(candidates),
                    },
                )
            if len(candidates) == 1:
                pod = candidates[0]
                metadata = json_object(pod.get("metadata"), "Kubernetes replacement Pod metadata")
                spec = json_object(pod.get("spec"), "Kubernetes replacement Pod spec")
                labels = json_object(metadata.get("labels"), "Kubernetes replacement Pod labels")
                name = metadata.get("name")
                uid = metadata.get("uid")
                node = spec.get("nodeName")
                if (
                    not isinstance(name, str)
                    or not name
                    or not isinstance(uid, str)
                    or not uid
                    or not isinstance(node, str)
                    or not node
                ):
                    raise AcceptanceError(
                        "runner.kubernetes_replacement_pod_invalid",
                        "The drained Execution replacement Pod omitted its stable identity.",
                    )
                if node == stale_node:
                    raise AcceptanceError(
                        "runner.kubernetes_replacement_node_not_changed",
                        "The replacement Pod scheduled onto the source Node while it was still cordoned.",
                        {"sourceNode": stale_node, "replacementNode": node},
                    )
                return {
                    "name": name,
                    "uid": uid,
                    "node": node,
                    "generation": labels.get("synara.io/generation"),
                }
            self.deadline.sleep(0.2)

    def validate_failure(self, fault: str) -> None:
        if fault == "worker-network" or fault == "kubernetes-eviction":
            return
        if fault in {"kubernetes-drain", "kubernetes-pdb-drain"}:
            self._require_node_drain_authorization()
            if fault == "kubernetes-pdb-drain":
                self._require_pdb_drain_nodes()
            return
        return super().validate_failure(fault)

    def observe_execution(self, target_id: str, execution_id: str) -> Mapping[str, Any]:
        runtime = self._target_runtime(target_id)
        return self._observe_execution_pod_contract(
            target_id,
            execution_id,
            expected_image=runtime["image"],
            expected_release_revision_id=None,
            expected_release_channel=None,
        )

    def observe_release_execution(
        self,
        target_id: str,
        execution_id: str,
        *,
        expected_image: str,
        expected_release_revision_id: str,
        expected_release_channel: str,
    ) -> Mapping[str, Any]:
        return self._observe_execution_pod_contract(
            target_id,
            execution_id,
            expected_image=expected_image,
            expected_release_revision_id=expected_release_revision_id,
            expected_release_channel=expected_release_channel,
        )

    def _observe_execution_pod_contract(
        self,
        target_id: str,
        execution_id: str,
        *,
        expected_image: str,
        expected_release_revision_id: str | None,
        expected_release_channel: str | None,
    ) -> Mapping[str, Any]:
        runtime = self._target_runtime(target_id)
        expected_namespace = runtime["namespace"]
        expected_service_account = runtime["serviceAccount"]
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
            "synara.io/worker-release-revision-id": expected_release_revision_id,
            "synara.io/worker-release-channel": expected_release_channel,
        }
        actual_labels = {key: labels.get(key) for key in expected_labels}
        assigned_execution = json_object(
            environment.get("SYNARA_AGENTD_ASSIGNED_EXECUTION_ID"),
            "assigned execution environment",
        ).get("value")
        expected_digest_match = re.search(r"@(sha256:[0-9a-f]{64})$", expected_image)
        expected_digest = expected_digest_match.group(1) if expected_digest_match is not None else None
        digest_environment = environment.get("SYNARA_AGENTD_IMAGE_DIGEST")
        actual_digest = digest_environment.get("value") if isinstance(digest_environment, dict) else None
        container_statuses = status.get("containerStatuses")
        container_image_id: str | None = None
        if isinstance(container_statuses, list):
            matching_statuses = [
                item
                for item in container_statuses
                if isinstance(item, dict) and item.get("name") == "agentd"
            ]
            if len(matching_statuses) == 1 and isinstance(matching_statuses[0].get("imageID"), str):
                container_image_id = str(matching_statuses[0]["imageID"])
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
            or container.get("imagePullPolicy") != self.image_pull_policy
            or assigned_execution != execution_id
            or actual_digest != expected_digest
            or (
                expected_digest is not None
                and (
                    container_image_id is None
                    or not container_image_id.endswith("@" + expected_digest)
                )
            )
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
                    "expectedImage": expected_image,
                    "actualImage": container.get("image"),
                    "expectedImageDigest": expected_digest,
                    "actualImageDigest": actual_digest,
                    "containerImageId": container_image_id,
                    "imagePullPolicy": container.get("imagePullPolicy"),
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
            "imageDigest": actual_digest,
            "containerImageId": container_image_id,
            "imagePullPolicy": container.get("imagePullPolicy"),
            "workerReleaseRevisionId": labels.get("synara.io/worker-release-revision-id"),
            "workerReleaseChannel": labels.get("synara.io/worker-release-channel"),
            "nodeName": spec.get("nodeName"),
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
            "stateRemoved": self._temporary_state and not self.state_dir.exists(),
            "broadCleanupUsed": False,
        }

    def _kind_cluster_configuration(self) -> Mapping[str, Any] | None:
        if not self.options.kind_worker_nodes:
            return None
        return {
            "kind": "Cluster",
            "apiVersion": "kind.x-k8s.io/v1alpha4",
            "nodes": [
                {"role": "control-plane"},
                *(
                    {"role": "worker"}
                    for _ in range(self.options.kind_worker_nodes)
                ),
            ],
        }

    def _prepare_cluster(self) -> Mapping[str, Any]:
        if self.owns_cluster:
            if self.kubeconfig is None:
                raise AcceptanceError("runner.kubernetes_kubeconfig_missing", "Owned Kind cluster omitted kubeconfig.")
            self.kubeconfig.parent.mkdir(parents=True, exist_ok=True)
            create_arguments = [
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
            ]
            cluster_configuration = self._kind_cluster_configuration()
            if cluster_configuration is not None:
                config_path = self.state_dir / "kind-cluster.json"
                config_path.write_text(
                    json.dumps(
                        cluster_configuration,
                        separators=(",", ":"),
                    )
                    + "\n",
                    encoding="utf-8",
                )
                create_arguments.extend(["--config", str(config_path)])
            self._kind_command(
                create_arguments,
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
            "requestedKindWorkerNodes": self.options.kind_worker_nodes if self.owns_cluster else None,
            "serverVersion": server.get("gitVersion"),
            "kubeconfig": str(self.kubeconfig) if self.kubeconfig is not None else "default",
            "topology": self._wait_cluster_topology_evidence(),
        }

    def _wait_cluster_topology_evidence(self) -> Mapping[str, Any]:
        if not self.owns_cluster:
            return self._cluster_topology_evidence()
        expected_node_count = self.options.kind_worker_nodes + 1
        while True:
            evidence = self._cluster_topology_evidence()
            if (
                evidence["nodeCount"] == expected_node_count
                and evidence["readyNodeCount"] == expected_node_count
                and evidence["controlPlaneNodeCount"] == 1
                and evidence["workerNodeCount"] == self.options.kind_worker_nodes
                and evidence["schedulableNodeCount"] >= self.options.kind_worker_nodes
            ):
                return evidence
            self.deadline.sleep(0.2)

    def _cluster_node_summaries(self) -> list[dict[str, Any]]:
        try:
            payload = json.loads(self._kubectl_command(["get", "nodes", "-o", "json"]))
        except json.JSONDecodeError:
            raise AcceptanceError(
                "runner.kubernetes_nodes_invalid",
                "Kubernetes Node inventory was invalid JSON.",
            ) from None
        items = payload.get("items") if isinstance(payload, dict) else None
        if not isinstance(items, list) or not all(isinstance(item, dict) for item in items):
            raise AcceptanceError(
                "runner.kubernetes_nodes_invalid",
                "Kubernetes Node inventory was malformed.",
            )
        summaries: list[dict[str, Any]] = []
        for item in items:
            metadata = json_object(item.get("metadata"), "Kubernetes Node metadata")
            spec = json_object(item.get("spec"), "Kubernetes Node spec")
            status = json_object(item.get("status"), "Kubernetes Node status")
            name = metadata.get("name")
            labels = metadata.get("labels")
            conditions = status.get("conditions")
            taints = spec.get("taints", [])
            if (
                not isinstance(name, str)
                or not name
                or not isinstance(labels, dict)
                or not isinstance(conditions, list)
                or not all(isinstance(condition, dict) for condition in conditions)
                or not isinstance(taints, list)
                or not all(isinstance(taint, dict) for taint in taints)
            ):
                raise AcceptanceError(
                    "runner.kubernetes_nodes_invalid",
                    "A Kubernetes Node omitted its scheduling identity.",
                )
            ready = any(
                condition.get("type") == "Ready" and condition.get("status") == "True"
                for condition in conditions
            )
            blocking_taints = sorted(
                str(taint.get("key"))
                for taint in taints
                if taint.get("effect") in {"NoSchedule", "NoExecute"}
                and isinstance(taint.get("key"), str)
            )
            summaries.append(
                {
                    "name": name,
                    "ready": ready,
                    "unschedulable": spec.get("unschedulable") is True,
                    "blockingTaints": blocking_taints,
                    "controlPlane": any(
                        key in labels
                        for key in (
                            "node-role.kubernetes.io/control-plane",
                            "node-role.kubernetes.io/master",
                        )
                    ),
                }
            )
        return summaries

    def _ready_schedulable_node_names(self) -> tuple[str, ...]:
        return tuple(
            str(node["name"])
            for node in self._cluster_node_summaries()
            if node["ready"] and not node["unschedulable"] and not node["blockingTaints"]
        )

    def _cluster_topology_evidence(self) -> Mapping[str, Any]:
        nodes = self._cluster_node_summaries()
        ready = [node for node in nodes if node["ready"]]
        schedulable = [
            node
            for node in ready
            if not node["unschedulable"] and not node["blockingTaints"]
        ]
        return {
            "nodeCount": len(nodes),
            "readyNodeCount": len(ready),
            "schedulableNodeCount": len(schedulable),
            "controlPlaneNodeCount": sum(1 for node in nodes if node["controlPlane"]),
            "workerNodeCount": sum(1 for node in nodes if not node["controlPlane"]),
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
        api_server = self.api_server_override or cluster.get("server")
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
        command = ["kubectl", "--context", self.context]
        if self.api_server_override is not None:
            command.extend(["--server", self.api_server_override])
        if self.tls_server_name_override is not None:
            command.extend(["--tls-server-name", self.tls_server_name_override])
        command.extend(arguments)
        try:
            return subprocess.run(
                command,
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

    @staticmethod
    def _kubectl_cleanup_failure_is_transient(output: str) -> bool:
        normalized = output.lower()
        return any(
            marker in normalized
            for marker in (
                "unexpected eof",
                "context deadline exceeded",
                "client.timeout exceeded",
                "request canceled",
                "connection reset by peer",
                "connection refused",
                "i/o timeout",
                "tls handshake timeout",
                "server is currently unable to handle the request",
                "server was unable to return a response in the time allotted",
                "service unavailable",
            )
        )

    def _kubectl_cleanup_completed(
        self,
        arguments: Sequence[str],
        *,
        cleanup_timeout: float,
    ) -> subprocess.CompletedProcess[str]:
        last_completed: subprocess.CompletedProcess[str] | None = None
        last_error: AcceptanceError | None = None
        for attempt in range(KUBERNETES_CLEANUP_MAX_ATTEMPTS):
            try:
                completed = self._kubectl_completed(
                    arguments,
                    cleanup_timeout=cleanup_timeout,
                )
            except AcceptanceError as error:
                last_error = error
            else:
                last_completed = completed
                output = self.redactor.text(completed.stdout)
                if completed.returncode == 0 or not self._kubectl_cleanup_failure_is_transient(output):
                    return completed
            if attempt + 1 < KUBERNETES_CLEANUP_MAX_ATTEMPTS:
                time.sleep(KUBERNETES_CLEANUP_RETRY_DELAYS_SECONDS[attempt])
        if last_completed is not None:
            return last_completed
        if last_error is not None:
            raise last_error
        raise AssertionError("Kubernetes cleanup retry loop produced no result")

    def _kubectl_cleanup_command(
        self,
        arguments: Sequence[str],
        *,
        cleanup_timeout: float,
    ) -> str:
        completed = self._kubectl_cleanup_completed(
            arguments,
            cleanup_timeout=cleanup_timeout,
        )
        output = self.redactor.text(completed.stdout)
        if completed.returncode != 0 and "not found" not in output.lower() and "notfound" not in output.lower():
            raise AcceptanceError(
                "runner.kubernetes_cleanup_command_failed",
                f"kubectl cleanup exited with status {completed.returncode}.",
                {
                    "command": ["kubectl", *arguments[:3]],
                    "exitCode": completed.returncode,
                    "maximumAttempts": KUBERNETES_CLEANUP_MAX_ATTEMPTS,
                    "outputExcerpt": output[-1000:],
                },
            )
        return output

    def _delete_cluster_resources(self) -> None:
        for namespace in sorted(self.owned_namespaces):
            if not self._kubernetes_resource_is_owned("namespace", namespace):
                continue
            self._kubectl_cleanup_command(
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
            self._kubectl_cleanup_command(
                ["delete", resource, self.bootstrap_role, "--ignore-not-found"],
                cleanup_timeout=20.0,
            )

    def _kubernetes_resource_is_owned(self, resource: str, name: str) -> bool:
        completed = self._kubectl_cleanup_completed(
            ["get", resource, name, "-o", "json"],
            cleanup_timeout=20.0,
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

    def real_provider_node_executable(self) -> str:
        return "node"

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
        self._fixture_load_session_cache: list[dict[str, Any]] | None = None

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
        if self.options.suite == "fixture-concurrency":
            self._run_fixture_concurrency()
            return self.cases
        if self.options.suite == "fixture-load":
            self._run_fixture_load()
            return self.cases
        if self.options.suite == "fixture-load-failure":
            self._run_fixture_load_failure()
            return self.cases
        if self.options.suite == "fixture-retention-concurrency":
            self._run_fixture_retention_concurrency()
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
        user_input_requirement = "fixture.terminal-large-log"
        if (
            self.driver.lifecycle.execution_pinned
            and getattr(self.driver, "pending_interaction_recovery", None) is not None
        ):
            self._case(
                "recovery.pending-user-input-runtime-loss",
                "Force pending structured user input runtime loss and verify recovery fencing",
                self._recover_pending_user_input_runtime,
                requires=("fixture.terminal-large-log",),
            )
            user_input_requirement = "recovery.pending-user-input-runtime-loss"
        self._case(
            "fixture.user-input-resolution",
            "Resolve Provider user input through the user API",
            self._user_input_resolution,
            requires=(user_input_requirement,),
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
        if self.options.suite == "fixture-soak":
            self._case(
                "soak.multi-turn-restart-continuity",
                "Run a deterministic long Session with repeated Control Plane restarts",
                self._fixture_long_session_soak,
                requires=("fixture.second-turn-continuity",),
            )
        return self.cases

    def _run_fixture_concurrency(self) -> None:
        self._case(
            "runtime.target-provision",
            "Provision a two-Worker managed Docker Target",
            self._provision_target,
            requires=("identity.dev-login",),
        )
        self._case(
            "runtime.concurrent-worker-discovery",
            "Discover two compatible Workers exposing both fixture Providers",
            self._discover_concurrency_workers,
            requires=("runtime.target-provision",),
        )
        self._case(
            "resources.credential-project-session",
            "Create the primary bound Credential, empty Repository Project, and Session",
            self._create_resources,
            requires=("runtime.concurrent-worker-discovery",),
        )
        self._case(
            "concurrency.multi-provider-multi-session",
            "Run overlapping Codex and Claude fixture Executions on distinct Workers",
            self._fixture_multi_provider_concurrency,
            requires=("resources.credential-project-session",),
        )

    def _run_fixture_load_setup(self) -> None:
        self._case(
            "runtime.target-provision",
            "Provision a two-Worker managed Docker Target",
            self._provision_target,
            requires=("identity.dev-login",),
        )
        self._case(
            "runtime.concurrent-worker-discovery",
            "Discover two compatible Workers exposing both fixture Providers",
            self._discover_concurrency_workers,
            requires=("runtime.target-provision",),
        )
        self._case(
            "resources.credential-project-session",
            "Create the primary bound Credential, empty Repository Project, and Session",
            self._create_resources,
            requires=("runtime.concurrent-worker-discovery",),
        )

    def _run_fixture_load(self) -> None:
        self._run_fixture_load_setup()
        self._case(
            "load.multi-session-admission-waves",
            "Run bounded multi-Session load with quota rejection, slot reuse, Artifacts, and Checkpoints",
            self._fixture_load_admission_waves,
            requires=("resources.credential-project-session",),
        )

    def _run_fixture_load_failure(self) -> None:
        self._run_fixture_load_setup()
        self._case(
            "load.targeted-worker-network-recovery",
            "Interrupt one exact busy Worker while its peer Session remains unchanged",
            lambda: self._fixture_load_failure_isolation(
                "worker-network",
                session_offset=0,
                affected_index=0,
            ),
            requires=("resources.credential-project-session",),
        )
        self._case(
            "load.targeted-worker-container-loss-recovery",
            "Remove one exact busy Worker container and verify managed replacement isolation",
            lambda: self._fixture_load_failure_isolation(
                "worker-container-loss",
                session_offset=2,
                affected_index=1,
            ),
            requires=("load.targeted-worker-network-recovery",),
        )
        self._case(
            "load.targeted-provider-host-process-crash",
            "Crash one exact busy Provider Host while its peer Session remains unchanged",
            lambda: self._fixture_load_provider_host_crash_isolation(
                session_offset=1,
                affected_index=0,
            ),
            requires=("load.targeted-worker-container-loss-recovery",),
        )
        self._case(
            "load.post-failure-admission-waves",
            "Run bounded post-recovery load with quota rejection, slot reuse, Artifacts, and Checkpoints",
            self._fixture_load_admission_waves,
            requires=("load.targeted-provider-host-process-crash",),
        )

    def _run_fixture_retention_concurrency(self) -> None:
        self._case(
            "runtime.worker-discovery",
            "Provision the Local Target and discover its real compatible Worker manifest",
            self._provision_standing_target_and_discover_worker,
            requires=("identity.dev-login",),
        )
        self._case(
            "resources.credential-project-session",
            "Create a bound Credential, empty Repository Project, and Session",
            self._create_resources,
            requires=("runtime.worker-discovery",),
        )
        self._case(
            "retention.seed-artifact-checkpoint",
            "Create a terminal Artifact and current ready Workspace Checkpoint",
            self._text_tool_usage_artifact,
            requires=("resources.credential-project-session",),
        )
        self._case(
            "retention.active-execution-cleanup-fencing",
            "Run retention while an Approval Execution is active, then complete exact Workspace cleanup",
            self._fixture_retention_concurrency,
            requires=("retention.seed-artifact-checkpoint",),
        )

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
        recovery_requirement = "real-provider.turn-1"
        for real_provider_case in self.options.real_provider_cases:
            if real_provider_case not in REAL_PROVIDER_PRE_RESTART_CASES:
                continue
            metadata = REAL_PROVIDER_CASE_METADATA[real_provider_case]
            case_id = metadata["id"]
            self._case(
                case_id,
                metadata["name"],
                lambda real_provider_case=real_provider_case: self._execute_real_provider_case(
                    real_provider_case
                ),
                requires=(recovery_requirement,),
            )
            recovery_requirement = case_id
        for failure_case in self.options.real_provider_failure_cases:
            metadata = REAL_PROVIDER_FAILURE_CASE_METADATA[failure_case]
            case_id = metadata["id"]
            self._case(
                case_id,
                metadata["name"],
                lambda failure_case=failure_case: self._execute_real_provider_failure_case(
                    failure_case
                ),
                requires=(recovery_requirement,),
            )
            recovery_requirement = case_id
        if self.options.restart_control_plane:
            self._case(
                "recovery.control-plane-restart",
                "Restart Control Plane with persisted real Provider state",
                self._restart_control_plane,
                requires=(recovery_requirement,),
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
        advanced_requirement = "real-provider.turn-2-continuity"
        for real_provider_case in self.options.real_provider_cases:
            if real_provider_case not in REAL_PROVIDER_POST_RESTART_CASES:
                continue
            metadata = REAL_PROVIDER_CASE_METADATA[real_provider_case]
            case_id = metadata["id"]
            self._case(
                case_id,
                metadata["name"],
                lambda real_provider_case=real_provider_case: self._execute_real_provider_case(
                    real_provider_case
                ),
                requires=(advanced_requirement,),
            )
            advanced_requirement = case_id

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

    def _begin_pending_interaction_barrier(
        self,
        *,
        state_field: str,
        kind: str,
        input_text: str,
        display_name: str,
        already_started_code: str,
        invalid_code: str,
    ) -> Mapping[str, Any]:
        if getattr(self.state, state_field) is not None:
            raise AcceptanceError(
                already_started_code,
                f"The execution-pinned {display_name} readiness barrier was already started.",
            )
        turn = self._create_turn(input_text)
        turn_id = turn.get("id")
        if not isinstance(turn_id, str) or not turn_id:
            raise AcceptanceError(
                "runner.turn_id_missing",
                f"{display_name} readiness Turn did not return an ID.",
            )
        interaction = self._wait_for_interaction(turn_id, kind)
        execution_id = interaction.get("executionId")
        request_id = interaction.get("requestId")
        if not isinstance(execution_id, str) or not execution_id or not isinstance(request_id, str) or not request_id:
            raise AcceptanceError(
                invalid_code,
                f"The execution-pinned {display_name} readiness barrier omitted its Execution or Request ID.",
                {"turnId": turn_id, "interactionId": interaction.get("id")},
            )
        setattr(self.state, state_field, {"turn": turn, "interaction": interaction})
        return {
            "kind": kind,
            "turnId": turn_id,
            "executionId": execution_id,
            "requestId": request_id,
            "interactionId": interaction.get("id"),
        }

    def _begin_approval_readiness_barrier(self) -> Mapping[str, Any]:
        return self._begin_pending_interaction_barrier(
            state_field="pending_approval",
            kind="approval",
            input_text="[approval]",
            display_name="Approval",
            already_started_code="runner.approval_barrier_already_started",
            invalid_code="runner.approval_barrier_invalid",
        )

    def _begin_user_input_readiness_barrier(self) -> Mapping[str, Any]:
        return self._begin_pending_interaction_barrier(
            state_field="pending_user_input",
            kind="user-input",
            input_text="[user-input]",
            display_name="Structured User Input",
            already_started_code="runner.user_input_barrier_already_started",
            invalid_code="runner.user_input_barrier_invalid",
        )

    def _wait_compatible_manifest(self, target_id: str) -> dict[str, Any]:
        discovered = self._wait_compatible_manifest_for_providers(
            target_id,
            (self.options.provider,),
            minimum_online=1,
        )
        providers = json_object_array(discovered.get("providers"), "Worker Provider discovery")
        return {
            "manifest": json_object(discovered.get("manifest"), "Worker manifest discovery"),
            "provider": providers[0],
        }

    def _wait_compatible_manifest_for_providers(
        self,
        target_id: str,
        providers: Sequence[str],
        *,
        minimum_online: int,
    ) -> dict[str, Any]:
        tenant_id = self._required("tenant_id")
        requested_providers = tuple(dict.fromkeys(providers))
        if not requested_providers or minimum_online <= 0:
            raise ValueError("Worker manifest discovery requires Providers and a positive online count")

        def manifest_probe() -> dict[str, Any] | None:
            manifests = json_items(
                self.api.request("GET", f"/v1/tenants/{tenant_id}/worker-manifests"),
                "worker-manifests",
            )
            for manifest in manifests:
                counts = manifest.get("workerStatusCounts")
                if manifest.get("executionTargetId") != target_id or not isinstance(counts, dict):
                    continue
                if (
                    not isinstance(counts.get("online"), int)
                    or counts["online"] < minimum_online
                ):
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
                manifest_providers = manifest.get("providers")
                if not isinstance(manifest_providers, list):
                    continue
                discovered_providers: list[dict[str, Any]] = []
                for requested_provider in requested_providers:
                    provider = next(
                        (
                            item
                            for item in manifest_providers
                            if isinstance(item, dict)
                            and str(item.get("provider", "")).lower()
                            == requested_provider.lower()
                        ),
                        None,
                    )
                    if provider is None:
                        break
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
                            "The real Worker manifest did not expose every requested compatible enabled Provider.",
                            {
                                "requestedProvider": requested_provider,
                                "provider": self.redactor.value(provider),
                                "manifestId": manifest.get("manifestId"),
                            },
                        )
                    discovered_providers.append(provider)
                if len(discovered_providers) == len(requested_providers):
                    return {"manifest": manifest, "providers": discovered_providers}
            return None

        return self.api.wait_until(
            f"an online Worker manifest with {minimum_online} Workers and Providers "
            + ", ".join(requested_providers),
            manifest_probe,
        )

    def _discover_concurrency_workers(self) -> Mapping[str, Any]:
        discovered = self._wait_compatible_manifest_for_providers(
            self._required("target_id"),
            FIXTURE_CONCURRENCY_PROVIDERS,
            minimum_online=FIXTURE_CONCURRENCY_WORKERS,
        )
        manifest = json_object(discovered.get("manifest"), "concurrency Worker manifest")
        providers = json_object_array(
            discovered.get("providers"),
            "concurrency Worker Providers",
        )
        return {
            "manifestId": manifest.get("manifestId"),
            "workerStatusCounts": manifest.get("workerStatusCounts"),
            "workerProtocol": manifest.get("workerProtocol"),
            "runtimeEvent": manifest.get("runtimeEvent"),
            "providers": [
                {
                    "provider": provider.get("provider"),
                    "supportTier": provider.get("supportTier"),
                    "compatibilityStatus": provider.get("compatibilityStatus"),
                    "runtime": provider.get("runtime"),
                    "releasePolicy": provider.get("releasePolicy"),
                }
                for provider in providers
            ],
        }

    @staticmethod
    def _version_range_contains(value: Mapping[str, Any], version: int) -> bool:
        minimum = value.get("minimum")
        maximum = value.get("maximum")
        return isinstance(minimum, int) and isinstance(maximum, int) and minimum <= version <= maximum

    def _create_resources(self) -> Mapping[str, Any]:
        tenant_id = self._required("tenant_id")
        organization_id = self._required("organization_id")
        self._required("target_id")
        real_provider_smoke = self.options.suite == "real-provider-smoke"
        credential: dict[str, Any] | None = None
        credential_id: str | None = None
        if not real_provider_smoke:
            credential = self._create_fixture_credential(
                self.options.provider,
                "Stage 3 Provider Acceptance Fixture",
            )
            credential_id = self._string_id(credential, "fixture Credential")
        elif self.options.real_provider_credential_env is not None:
            credential = self._create_real_provider_credential(
                title="Stage 3 Real Provider Acceptance",
                payload=self._real_provider_product_credential_payload(),
            )
            credential_id = self._string_id(credential, "real Provider product Credential")
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
        self.state.project_id = project_id
        session = self._create_project_session(
            provider=self.options.provider,
            title=(
                "Stage 3 Real Provider Smoke"
                if real_provider_smoke
                else "Stage 3 Provider Acceptance"
            ),
            credential_id=credential_id,
            model=(
                self.options.real_provider_model
                if real_provider_smoke
                else "stage3-acceptance-fixture"
            ),
            description="session",
        )
        session_id = session.get("id")
        if not isinstance(session_id, str):
            raise AcceptanceError("runner.session_id_missing", "Session API did not return an ID.")
        self.state.credential_id = credential_id
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
                    **(
                        {
                            "delivery": "control-plane-provider-credential",
                            "source": "operator-environment",
                            "credentialField": self.options.real_provider_credential_field,
                            "baseUrlConfigured": self.options.real_provider_base_url_env is not None,
                            "environmentVariableNamePersisted": False,
                        }
                        if real_provider_smoke
                        else {"delivery": "acceptance-fixture"}
                    ),
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

    def _create_fixture_credential(self, provider: str, title: str) -> dict[str, Any]:
        tenant_id = self._required("tenant_id")
        organization_id = self._required("organization_id")
        credential = json_object(
            self.api.request(
                "POST",
                f"/v1/tenants/{tenant_id}/credentials",
                {
                    "organizationId": organization_id,
                    "name": title,
                    "purpose": "provider",
                    "provider": provider,
                    "credentialType": "api_key",
                    "payload": {"apiKey": FIXTURE_CREDENTIAL_SENTINEL},
                },
                expected=(201,),
            ),
            "fixture Credential",
        )
        if (
            str(credential.get("provider") or "").lower() != provider.lower()
            or credential.get("credentialType") != "api_key"
        ):
            raise AcceptanceError(
                "runner.credential_binding_mismatch",
                "Fixture Credential did not retain the requested Provider and type.",
                {
                    "requestedProvider": provider,
                    "provider": credential.get("provider"),
                    "credentialType": credential.get("credentialType"),
                },
            )
        self._string_id(credential, "fixture Credential")
        return credential

    def _create_project_session(
        self,
        *,
        provider: str,
        title: str,
        credential_id: str | None,
        model: str | None = None,
        description: str,
    ) -> dict[str, Any]:
        project_id = self._required("project_id")
        target_id = self._required("target_id")
        session_input: dict[str, Any] = {
            "title": title,
            "visibility": "project",
            "provider": provider,
            "executionTargetId": target_id,
        }
        if credential_id is not None:
            session_input["providerCredentialId"] = credential_id
        if model is not None:
            session_input["model"] = model
        session = json_object(
            self.api.request(
                "POST",
                f"/v1/projects/{project_id}/sessions",
                session_input,
                expected=(201,),
            ),
            description,
        )
        if (
            str(session.get("provider") or "").lower() != provider.lower()
            or session.get("executionTargetId") != target_id
            or session.get("providerCredentialId") != credential_id
            or (model is not None and session.get("model") != model)
        ):
            raise AcceptanceError(
                "runner.session_binding_mismatch",
                f"The {description} did not retain the requested Provider, Target, Credential, and model bindings.",
                {
                    "requestedProvider": provider,
                    "provider": session.get("provider"),
                    "executionTargetId": session.get("executionTargetId"),
                    "providerCredentialId": session.get("providerCredentialId"),
                    "requestedModel": model,
                    "model": session.get("model"),
                },
            )
        self._string_id(session, description)
        return session

    def _real_provider_product_credential_payload(self) -> dict[str, Any]:
        environment_name = self.options.real_provider_credential_env
        if environment_name is None:
            raise AcceptanceError(
                "runner.real_provider_credential_not_configured",
                "The real Provider product Credential source was not configured.",
                {"provider": self.options.provider, "target": self.driver.name},
            )
        try:
            secret = read_environment_value(
                environment_name,
                "real Provider Credential",
                maximum_length=64 << 10,
                forbidden_characters="\r\n\x00",
            )
        except EnvironmentValueError as error:
            raise AcceptanceError(
                (
                    "runner.real_provider_credential_env_missing"
                    if error.reason == "missing"
                    else "runner.real_provider_credential_env_invalid"
                ),
                str(error),
                {
                    "provider": self.options.provider,
                    "target": self.driver.name,
                    "environmentVariableNamePersisted": False,
                },
            ) from None
        self.redactor.add(secret, "[REDACTED_REAL_PROVIDER_CREDENTIAL]")
        payload: dict[str, Any] = {
            self.options.real_provider_credential_field: secret,
        }
        base_url_environment = self.options.real_provider_base_url_env
        if base_url_environment is not None:
            try:
                base_url = read_environment_value(
                    base_url_environment,
                    "real Provider Base URL",
                    maximum_length=2048,
                    forbidden_characters="\r\n\t\x00",
                ).strip()
            except EnvironmentValueError as error:
                raise AcceptanceError(
                    (
                        "runner.real_provider_base_url_env_missing"
                        if error.reason == "missing"
                        else "runner.real_provider_base_url_env_invalid"
                    ),
                    str(error),
                    {
                        "provider": self.options.provider,
                        "target": self.driver.name,
                        "environmentVariableNamePersisted": False,
                    },
                ) from None
            self.redactor.add(base_url, "[REDACTED_REAL_PROVIDER_BASE_URL]")
            payload["baseUrl"] = base_url
        return payload

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
        self.state.last_real_marker = self._real_provider_marker()
        return evidence

    def _execute_real_provider_case(self, real_provider_case: str) -> Mapping[str, Any]:
        if real_provider_case == "generated-file-checkpoint":
            return self._real_provider_generated_file_checkpoint()
        if real_provider_case == "large-diff":
            return self._real_provider_large_diff_artifact()
        if real_provider_case == "approval":
            return self._real_provider_approval_resolution()
        if real_provider_case == "user-input":
            return self._real_provider_user_input_resolution()
        if real_provider_case == "steer":
            return self._real_provider_steer_active_turn()
        if real_provider_case == "interrupt":
            return self._real_provider_interrupt_active_turn()
        if real_provider_case == "terminal-large":
            return self._real_provider_terminal_large_log()
        if real_provider_case == "review":
            return self._real_provider_review()
        if real_provider_case == "compact":
            return self._real_provider_compact_boundary()
        if real_provider_case == "rollback":
            return self._real_provider_rollback_emulation()
        if real_provider_case == "fork":
            return self._real_provider_fork_emulation()
        raise AcceptanceError(
            "runner.real_provider_case_unknown",
            f"Unknown real Provider acceptance case {real_provider_case}.",
            {"case": real_provider_case},
        )

    def _execute_real_provider_failure_case(self, failure_case: str) -> Mapping[str, Any]:
        if failure_case == "authentication":
            return self._real_provider_http_failure(
                failure_case,
                fault="authentication",
                expected_failure_code="authentication_required",
            )
        if failure_case == "rate-limit-retry":
            return self._real_provider_http_failure(
                failure_case,
                fault="rate-limit",
                expected_failure_code="provider_rate_limited",
            )
        if failure_case == "provider-host-crash-retry":
            return self._real_provider_host_crash_retry()
        if failure_case == "cursor-expiry":
            return self._real_provider_cursor_expiry_barrier()
        raise AcceptanceError(
            "runner.real_provider_failure_case_unknown",
            f"Unknown real Provider failure acceptance case {failure_case}.",
            {"case": failure_case},
        )

    def _real_provider_http_failure(
        self,
        failure_case: str,
        *,
        fault: str,
        expected_failure_code: str,
    ) -> Mapping[str, Any]:
        create_fault_server = getattr(self.driver, "create_provider_fault_server", None)
        probe_fault_server = getattr(self.driver, "probe_provider_fault_server", None)
        if not callable(create_fault_server) or not callable(probe_fault_server):
            raise AcceptanceUnsupported(
                "runner.real_provider_http_fault_target_unsupported",
                "The selected Target does not implement a scoped Provider HTTP fault endpoint.",
                {
                    "target": self.driver.name,
                    "failureCase": failure_case,
                    "requiredTargets": list(REAL_PROVIDER_HTTP_FAULT_TARGETS),
                },
            )
        fault_server = create_fault_server(self.options.provider, fault)
        fault_secret = f"stage3-provider-fault-{uuid.uuid4()}"
        self.redactor.add(fault_secret, "[REDACTED_PROVIDER_FAULT_CREDENTIAL]")
        self.redactor.add(fault_server.route_token, "[REDACTED_PROVIDER_FAULT_ROUTE]")
        fault_server_started = False
        try:
            fault_server.start()
            fault_server_started = True
            self.redactor.add(fault_server.endpoint, "[REDACTED_PROVIDER_FAULT_ENDPOINT]")
            reachability = dict(probe_fault_server(fault_server))
            credential = self._create_real_provider_credential(
                title=f"Stage 3 Real Provider {failure_case}",
                payload={
                    "apiKey": fault_secret,
                    "baseUrl": fault_server.credential_base_url,
                },
            )
            credential_id = self._string_id(credential, "real Provider failure Credential")
            session = self._create_real_provider_session(
                title=f"Stage 3 Real Provider {failure_case}",
                credential_id=credential_id,
            )
            session_id = self._string_id(session, "real Provider failure Session")
            turn = self._create_turn(
                "This request is an automated Provider error-classification acceptance check. "
                "Reply with exactly SYNARA_PROVIDER_FAULT_SHOULD_NOT_COMPLETE and no other text.",
                session_id=session_id,
            )
            turn_id = self._turn_id(turn, "real Provider HTTP failure Turn")
            terminal, events = self._wait_for_turn_terminal(
                turn_id,
                "execution.failed",
                session_id=session_id,
            )
        finally:
            if fault_server_started:
                fault_server.stop()
        fault_evidence = fault_server.evidence()
        if int(fault_evidence.get("requestCount") or 0) < 1:
            raise AcceptanceError(
                "runner.real_provider_http_fault_not_observed",
                "The real Provider failed without reaching the runner-owned HTTP fault endpoint.",
                {"failureCase": failure_case, "provider": self.options.provider},
            )
        if not fault_evidence.get("credentialHeaderNames"):
            raise AcceptanceError(
                "runner.real_provider_http_credential_header_missing",
                "The real Provider request did not carry a Credential header to the controlled endpoint.",
                {"failureCase": failure_case, "provider": self.options.provider},
            )
        reachability = finalize_provider_fault_reachability(reachability, fault_evidence)
        payload = json_object(terminal.get("payload"), "real Provider execution.failed payload")
        actual_failure_code = payload.get("failureCode")
        if actual_failure_code != expected_failure_code:
            raise AcceptanceError(
                "runner.real_provider_failure_code_mismatch",
                "The real Provider HTTP failure did not preserve the expected stable failure code.",
                {
                    "failureCase": failure_case,
                    "expectedFailureCode": expected_failure_code,
                    "actualFailureCode": actual_failure_code,
                    "faultServer": fault_evidence,
                },
            )
        encoded_events = json.dumps(events, separators=(",", ":"), ensure_ascii=False)
        if (
            fault_secret in encoded_events
            or fault_server.endpoint in encoded_events
            or fault_server.route_token in encoded_events
        ):
            raise AcceptanceError(
                "runner.real_provider_failure_secret_leak",
                "The real Provider failure events exposed controlled Credential or endpoint material.",
                {"failureCase": failure_case},
            )
        recovery = self._real_provider_recovery_turn(failure_case)
        return {
            "failureCase": failure_case,
            "faultTurnId": turn_id,
            "faultExecutionId": terminal.get("executionId"),
            "failureCode": actual_failure_code,
            "faultSequenceRange": self._sequence_range(events),
            "controlledCredential": {
                "id": credential_id,
                "provider": credential.get("provider"),
                "credentialType": credential.get("credentialType"),
                "version": credential.get("version"),
                "payloadPersistedInReport": False,
            },
            "faultServer": {**fault_evidence, "reachability": reachability},
            "singleTerminal": True,
            "credentialLeak": False,
            "recovery": recovery,
        }

    def _real_provider_host_crash_retry(self) -> Mapping[str, Any]:
        crash = getattr(self.driver, "crash_provider_host", None)
        if not callable(crash):
            raise AcceptanceUnsupported(
                "runner.real_provider_host_crash_unsupported",
                "The selected Target does not implement scoped real Provider Host crash injection.",
                {"target": self.driver.name},
            )
        session = self._create_real_provider_session(
            title="Stage 3 Real Provider Host Crash",
        )
        session_id = self._string_id(session, "real Provider crash Session")
        approval_barrier = self._real_provider_host_crash_uses_pending_approval()
        turn = self._create_turn(
            (
                real_provider_host_crash_approval_prompt()
                if approval_barrier
                else (
                    "Immediately invoke the Bash or shell tool exactly once with the command `sleep 120`. "
                    "Do not wait before invoking the tool, do not answer in text, and do nothing else while the "
                    "command is running. This is an automated process-crash acceptance barrier."
                )
            ),
            runtime_mode="approval-required" if approval_barrier else "full-access",
            session_id=session_id,
        )
        turn_id = self._turn_id(turn, "real Provider Host crash Turn")
        created = self._wait_for_turn_created(turn_id, session_id=session_id)
        execution_id = self._event_execution_id(created)
        approval_evidence: dict[str, Any] | None = None
        approval_interaction: dict[str, Any] | None = None
        if approval_barrier:
            (
                approval_interaction,
                approval_execution_id,
                approval_request_id,
                approval_payload,
                approval_command,
            ) = self._real_provider_approval_interaction(
                turn_id,
                expected_command=real_provider_host_crash_command(),
                session_id=session_id,
            )
            if approval_execution_id != execution_id:
                raise AcceptanceError(
                    "runner.real_provider_host_crash_approval_execution_mismatch",
                    "The external SSH Host-crash Approval barrier changed Execution identity.",
                    {
                        "turnId": turn_id,
                        "expectedExecutionId": execution_id,
                        "actualExecutionId": approval_execution_id,
                    },
                )
            approval_evidence = {
                "interactionId": approval_interaction.get("id"),
                "requestId": approval_request_id,
                "requestKind": approval_payload.get("requestKind"),
                "commandSummary": self.redactor.text(approval_command[:256]),
            }
        started = self._wait_for_execution_event(
            execution_id,
            "item.started",
            after_sequence=int(created.get("sequence") or 0),
            session_id=session_id,
        )
        if approval_interaction is not None:
            if not self._interaction_pending(session_id, approval_interaction):
                raise AcceptanceError(
                    "runner.real_provider_host_crash_approval_not_pending",
                    "The external SSH Host-crash Approval barrier resolved before crash injection.",
                    {
                        "turnId": turn_id,
                        "interactionId": approval_interaction.get("id"),
                    },
                )
            assert approval_evidence is not None
            approval_evidence["pendingAtCrash"] = True
        crash_evidence = dict(crash())
        terminal, events = self._wait_for_turn_terminal(
            turn_id,
            "execution.failed",
            session_id=session_id,
        )
        payload = json_object(terminal.get("payload"), "real Provider crash execution.failed payload")
        if payload.get("failureCode") != "provider_unavailable":
            raise AcceptanceError(
                "runner.real_provider_failure_code_mismatch",
                "The scoped real Provider Host crash did not persist provider_unavailable.",
                {
                    "failureCase": "provider-host-crash-retry",
                    "actualFailureCode": payload.get("failureCode"),
                },
            )
        if approval_interaction is not None:
            pending = [
                item
                for item in self._pending_interactions(session_id)
                if item.get("turnId") == turn_id
            ]
            if pending:
                raise AcceptanceError(
                    "runner.real_provider_host_crash_interaction_leaked",
                    "The external SSH Provider Host crash left its Approval interaction pending.",
                    {
                        "turnId": turn_id,
                        "interactionIds": [item.get("id") for item in pending],
                    },
                )
            assert approval_evidence is not None
            approval_evidence["clearedAfterCrash"] = True
        recovery = self._real_provider_recovery_turn("provider-host-crash-retry")
        return {
            "failureCase": "provider-host-crash-retry",
            "faultTurnId": turn_id,
            "faultExecutionId": execution_id,
            "failureCode": payload.get("failureCode"),
            "activeWorkBarrier": {
                "eventId": started.get("eventId"),
                "eventType": started.get("eventType"),
                "sequence": started.get("sequence"),
            },
            **({"approvalBarrier": approval_evidence} if approval_evidence is not None else {}),
            "crash": crash_evidence,
            "faultSequenceRange": self._sequence_range(events),
            "singleTerminal": True,
            "recovery": recovery,
        }

    def _real_provider_host_crash_uses_pending_approval(self) -> bool:
        return getattr(self.driver, "name", None) == "ssh" and bool(
            getattr(self.driver, "external_host", False)
        )

    def _real_provider_cursor_expiry_barrier(self) -> Mapping[str, Any]:
        if "cursor-expiry" not in self.options.real_provider_failure_cases:
            raise AcceptanceError(
                "runner.real_provider_cursor_expiry_not_configured",
                "Cursor expiry barrier requires the canonical short maximum-age configuration.",
            )
        started = time.monotonic()
        self.deadline.sleep(REAL_PROVIDER_CURSOR_EXPIRY_WAIT_SECONDS)
        return {
            "configuredMaximumAge": REAL_PROVIDER_CURSOR_MAX_AGE,
            "waitedMs": elapsed_ms(started),
            "expectedPostRestartStrategy": "authoritative-history",
            "expectedPostRestartReason": "cursor_expired",
            "cursorBytesMutatedByRunner": False,
        }

    def _real_provider_recovery_turn(self, failure_case: str) -> Mapping[str, Any]:
        session = self._create_real_provider_session(
            title=f"Stage 3 Real Provider {failure_case} Recovery",
        )
        session_id = self._string_id(session, "real Provider recovery Session")
        marker = self._real_provider_marker(
            f"{failure_case}-recovery",
            session_id=session_id,
            visible_label="recovery",
        )
        turn = self._create_turn(
            f"Reply with exactly {marker} and no other text.",
            session_id=session_id,
        )
        turn_id = self._turn_id(turn, "real Provider failure recovery Turn")
        terminal, events = self._wait_for_turn_terminal(
            turn_id,
            "execution.completed",
            session_id=session_id,
        )
        evidence = self._real_provider_turn_evidence(
            turn_id,
            terminal,
            events,
            marker,
            expected_resume_strategy="authoritative-history",
            expected_resume_reason="cursor_absent",
            max_lease_generations=(
                REAL_PROVIDER_MAX_RECOVERY_LEASE_GENERATIONS
                if failure_case == "provider-host-crash-retry"
                else 1
            ),
        )
        return {
            **evidence,
            "sessionId": session_id,
            "newExecutionAfterFailure": True,
            "ambientAuthentication": self.state.credential_id is None,
        }

    def _create_real_provider_credential(
        self,
        *,
        title: str,
        payload: Mapping[str, Any],
    ) -> dict[str, Any]:
        tenant_id = self._required("tenant_id")
        organization_id = self._required("organization_id")
        return json_object(
            self.api.request(
                "POST",
                f"/v1/tenants/{tenant_id}/credentials",
                {
                    "organizationId": organization_id,
                    "name": title,
                    "purpose": "provider",
                    "provider": self.options.provider,
                    "credentialType": "api_key",
                    "payload": dict(payload),
                },
                expected=(201,),
            ),
            "real Provider Credential",
        )

    def _create_real_provider_session(
        self,
        *,
        title: str,
        credential_id: str | None = None,
    ) -> dict[str, Any]:
        credential_id = credential_id or self.state.credential_id
        return self._create_project_session(
            provider=self.options.provider,
            title=title,
            credential_id=credential_id,
            model=self.options.real_provider_model,
            description="real Provider failure Session",
        )

    @staticmethod
    def _string_id(value: Mapping[str, Any], description: str) -> str:
        identifier = value.get("id")
        if not isinstance(identifier, str) or not identifier:
            raise AcceptanceError("runner.resource_id_missing", f"The {description} omitted its ID.")
        return identifier

    def _real_provider_large_diff_artifact(self) -> Mapping[str, Any]:
        seed_marker = self._real_provider_marker("large-diff-seed")
        seed_command = large_diff_seed_node_command(
            self.driver.real_provider_node_executable()
        )
        seed_turn = self._create_turn(
            "Use the Bash or shell tool exactly once. Run this exact command as the sole shell command:\n"
            f"{seed_command}\n"
            "Do not add redirections, pipes, wrappers, or any other terminal command. Do not read the file "
            "back or print its contents. After the command succeeds, reply with exactly "
            f"{seed_marker} and no other text."
        )
        seed_turn_id = self._turn_id(seed_turn, "real Provider large-Diff seed Turn")
        seed_terminal, seed_events = self._wait_for_turn_terminal(
            seed_turn_id, "execution.completed"
        )
        seed_evidence = self._real_provider_turn_evidence(
            seed_turn_id,
            seed_terminal,
            seed_events,
            seed_marker,
            expected_resume_strategy="native-cursor",
            expected_resume_reason="cursor_usable",
            marker_match_mode="contains-once",
        )

        before_artifacts = json_items(
            self.api.request(
                "GET", f"/v1/sessions/{self._required('session_id')}/artifacts"
            ),
            "pre-large-Diff artifacts",
        )
        previous_artifact_ids = {
            str(item["id"])
            for item in before_artifacts
            if isinstance(item.get("id"), str) and item.get("id")
        }
        marker = self._real_provider_marker("large-diff")
        if self.options.provider == "codex":
            mutation = (
                "Use the native apply_patch file-change tool exactly once, never Bash or shell, to delete "
                f"{LARGE_DIFF_RELATIVE_PATH}. Do not read the file first."
            )
        else:
            mutation = (
                "Use the native Read tool exactly once on "
                f"{LARGE_DIFF_RELATIVE_PATH} with offset 1 and limit 1. Do not print the line or read "
                "any other part of the file. Then use the native Write tool exactly once, never Bash or "
                f"shell, to replace {LARGE_DIFF_RELATIVE_PATH} with exactly this UTF-8 line and one "
                "trailing newline:\nSYNARA_STAGE3_LARGE_DIFF_AFTER_V1\n"
            )
        turn = self._create_turn(
            f"{mutation} After the native file tool succeeds, reply with exactly {marker} and no other text."
        )
        turn_id = self._turn_id(turn, "real Provider large-Diff Turn")
        terminal, events = self._wait_for_turn_terminal(turn_id, "execution.completed")
        provider_evidence = self._real_provider_turn_evidence(
            turn_id,
            terminal,
            events,
            marker,
            expected_resume_strategy="native-cursor",
            expected_resume_reason="cursor_usable",
            marker_match_mode="contains-once",
        )
        artifact_evidence = self._validate_large_diff_artifact(
            terminal, events, previous_artifact_ids
        )
        self.state.last_real_marker = marker
        return {
            "seed": {
                "turnId": seed_turn_id,
                "relativePath": LARGE_DIFF_RELATIVE_PATH,
                "sizeBytes": len(large_diff_seed_bytes()),
                "sha256": hashlib.sha256(large_diff_seed_bytes()).hexdigest(),
                "commandSha256": hashlib.sha256(seed_command.encode("utf-8")).hexdigest(),
                "providerTurn": seed_evidence,
            },
            "diff": artifact_evidence,
            "providerTurn": provider_evidence,
        }

    def _validate_large_diff_artifact(
        self,
        execution_terminal: Mapping[str, Any],
        events: Sequence[Mapping[str, Any]],
        previous_artifact_ids: set[str],
    ) -> Mapping[str, Any]:
        execution_id = execution_terminal.get("executionId")
        if not isinstance(execution_id, str) or not execution_id:
            raise AcceptanceError(
                "runner.large_diff_execution_id_missing",
                "The large-Diff Turn omitted its Execution ID.",
            )
        ready_events = [
            event
            for event in events
            if event.get("eventType") == "artifact.ready"
            and event.get("executionId") == execution_id
            and isinstance(event.get("payload"), dict)
            and event["payload"].get("kind") == "diff"
        ]
        reference_events = [
            event
            for event in events
            if event.get("eventType") == "turn.diff.updated"
            and event.get("executionId") == execution_id
            and isinstance(event.get("payload"), dict)
            and isinstance(event["payload"].get("artifact"), dict)
        ]
        if len(ready_events) != 1 or len(reference_events) != 1:
            completed_tools = [
                event["payload"].get("title")
                for event in events
                if event.get("eventType") == "item.completed"
                and event.get("executionId") == execution_id
                and isinstance(event.get("payload"), dict)
                and isinstance(event["payload"].get("title"), str)
            ]
            provider_warnings = [
                event["payload"].get("message")
                for event in events
                if event.get("eventType") == "runtime.warning"
                and event.get("executionId") == execution_id
                and isinstance(event.get("payload"), dict)
                and isinstance(event["payload"].get("message"), str)
            ]
            inline_diff_bytes = [
                len(event["payload"]["unifiedDiff"].encode("utf-8"))
                for event in events
                if event.get("eventType") == "turn.diff.updated"
                and event.get("executionId") == execution_id
                and isinstance(event.get("payload"), dict)
                and isinstance(event["payload"].get("unifiedDiff"), str)
            ]
            ready_artifact_kinds = [
                event["payload"].get("kind")
                for event in events
                if event.get("eventType") == "artifact.ready"
                and event.get("executionId") == execution_id
                and isinstance(event.get("payload"), dict)
                and isinstance(event["payload"].get("kind"), str)
            ]
            raise AcceptanceError(
                "runner.large_diff_event_boundary_invalid",
                "Expected one Diff artifact.ready and one Artifact-backed turn.diff.updated Event.",
                {
                    "executionId": execution_id,
                    "readyCount": len(ready_events),
                    "referenceCount": len(reference_events),
                    "completedTools": completed_tools,
                    "providerWarnings": provider_warnings[-4:],
                    "inlineDiffBytes": inline_diff_bytes,
                    "readyArtifactKinds": ready_artifact_kinds,
                },
            )
        ready = ready_events[0]
        reference = reference_events[0]
        ready_payload = json_object(ready.get("payload"), "large-Diff artifact.ready payload")
        reference_payload = json_object(
            reference.get("payload"), "large-Diff turn.diff.updated payload"
        )
        artifact_reference = json_object(
            reference_payload.get("artifact"), "large-Diff Artifact reference"
        )
        artifact_id = ready_payload.get("artifactId")
        sequences = [
            ready.get("sequence"),
            reference.get("sequence"),
            execution_terminal.get("sequence"),
        ]
        if (
            not isinstance(artifact_id, str)
            or not artifact_id
            or artifact_id in previous_artifact_ids
            or artifact_reference.get("artifactId") != artifact_id
            or ready_payload.get("contentType") != "text/x-diff; charset=utf-8"
            or artifact_reference.get("contentType") != ready_payload.get("contentType")
            or not all(isinstance(sequence, int) for sequence in sequences)
            or sequences != sorted(sequences)
            or len(set(sequences)) != len(sequences)
            or "unifiedDiff" in reference_payload
            or contains_runtime_physical_path(ready_payload)
            or contains_runtime_physical_path(reference_payload)
        ):
            raise AcceptanceError(
                "runner.large_diff_reference_invalid",
                "The large-Diff Artifact reference did not form one ordered Ready boundary.",
                {
                    "artifactReady": ready_payload,
                    "diffReference": reference_payload,
                    "sequences": sequences,
                },
            )

        artifacts = json_items(
            self.api.request(
                "GET", f"/v1/sessions/{self._required('session_id')}/artifacts"
            ),
            "large-Diff artifacts",
        )
        matching = [
            item
            for item in artifacts
            if item.get("id") == artifact_id
            and item.get("kind") == "diff"
            and item.get("executionId") == execution_id
        ]
        if len(matching) != 1:
            raise AcceptanceError(
                "runner.large_diff_artifact_invalid",
                "The Ready Diff Artifact was missing or ambiguous.",
                {"artifactId": artifact_id, "count": len(matching)},
            )
        artifact = matching[0]
        grant = json_object(
            self.api.request("POST", f"/v1/artifacts/{artifact_id}/download"),
            "large-Diff Artifact download grant",
        )
        url = grant.get("url")
        if not isinstance(url, str) or not url:
            raise AcceptanceError(
                "runner.large_diff_download_grant_invalid",
                "The large-Diff Artifact download grant was invalid.",
            )
        content = self.api.download_bytes(url, maximum_bytes=LARGE_DIFF_DOWNLOAD_MAX_BYTES)
        digest = hashlib.sha256(content).hexdigest()
        try:
            text = content.decode("utf-8")
        except UnicodeDecodeError as error:
            raise AcceptanceError(
                "runner.large_diff_encoding_invalid",
                "The large-Diff Artifact was not valid UTF-8.",
            ) from error
        expected_first = "SYNARA_STAGE3_LARGE_DIFF_BEFORE_00000_"
        expected_last = f"SYNARA_STAGE3_LARGE_DIFF_BEFORE_{LARGE_DIFF_LINE_COUNT - 1:05d}_"
        if (
            artifact.get("status") != "ready"
            or artifact.get("originalName") != "turn.diff"
            or artifact.get("contentType") != "text/x-diff; charset=utf-8"
            or len(content) <= 64 << 10
            or len(content) != artifact.get("sizeBytes")
            or digest != artifact.get("sha256")
            or artifact_reference.get("sizeBytes") != len(content)
            or artifact_reference.get("sha256") != digest
            or artifact_reference.get("fileCount") != 1
            or not isinstance(artifact_reference.get("additions"), int)
            or not isinstance(artifact_reference.get("deletions"), int)
            or int(artifact_reference["deletions"]) < LARGE_DIFF_LINE_COUNT
            or LARGE_DIFF_RELATIVE_PATH not in text
            or expected_first not in text
            or expected_last not in text
            or contains_runtime_physical_path(artifact)
        ):
            raise AcceptanceError(
                "runner.large_diff_download_mismatch",
                "The downloaded large-Diff Artifact did not match its Ready reference or deterministic seed.",
                {
                    "artifact": self._artifact_summary(artifact),
                    "reference": artifact_reference,
                    "actualBytes": len(content),
                    "actualSha256": digest,
                },
            )
        return {
            "artifact": self._artifact_summary(artifact),
            "download": {"sizeBytes": len(content), "sha256": digest},
            "summary": {
                "fileCount": artifact_reference.get("fileCount"),
                "additions": artifact_reference.get("additions"),
                "deletions": artifact_reference.get("deletions"),
            },
            "sequenceRange": {
                "artifactReady": ready.get("sequence"),
                "diffReference": reference.get("sequence"),
                "executionCompleted": execution_terminal.get("sequence"),
            },
            "inlinePayloadPersisted": False,
            "runtimePhysicalPathLeak": False,
        }

    def _real_provider_generated_file_checkpoint(self) -> Mapping[str, Any]:
        before_artifacts = json_items(
            self.api.request("GET", f"/v1/sessions/{self._required('session_id')}/artifacts"),
            "pre-generated-file artifacts",
        )
        previous_artifact_ids = {
            str(item["id"])
            for item in before_artifacts
            if isinstance(item.get("id"), str) and item.get("id")
        }
        marker = self._real_provider_marker("generated-file-checkpoint")
        command = generated_file_node_command(self.driver.real_provider_node_executable())
        native_file_tool = (
            "the native apply_patch file-change tool"
            if self.options.provider == "codex"
            else "the native Write tool"
        )
        standalone_text = STANDALONE_GENERATED_FILE_CONTENT.decode("ascii").rstrip("\n")
        turn = self._create_turn(
            f"First use {native_file_tool} exactly once, never Bash or shell, to create "
            f"{STANDALONE_GENERATED_FILE_RELATIVE_PATH} with exactly this UTF-8 line and one trailing newline:\n"
            f"{standalone_text}\n"
            "Do not read that file back. After the native file tool succeeds, use the Bash or shell tool "
            "exactly once. Run this exact command as the sole shell command:\n"
            f"{command}\n"
            "Do not add redirections, pipes, wrappers, or any other terminal command. Do not read the file "
            "back or print its contents. After the command succeeds, reply with exactly "
            f"{marker} and no other text."
        )
        turn_id = self._turn_id(turn, "real Provider generated-file Turn")
        terminal, events = self._wait_for_turn_terminal(turn_id, "execution.completed")
        provider_evidence = self._real_provider_turn_evidence(
            turn_id,
            terminal,
            events,
            marker,
            expected_resume_strategy="native-cursor",
            expected_resume_reason="cursor_usable",
            marker_match_mode="contains-once",
        )
        checkpoint_evidence = self._validate_generated_file_checkpoint(
            turn_id,
            terminal,
            events,
            previous_artifact_ids,
        )
        self.state.last_real_marker = marker
        return {
            "command": {
                "runtime": "node",
                "relativePath": GENERATED_FILE_RELATIVE_PATH,
                "totalBytes": GENERATED_FILE_TOTAL_BYTES,
                "commandSha256": hashlib.sha256(command.encode("utf-8")).hexdigest(),
            },
            "standaloneFile": {
                "providerTool": native_file_tool,
                "relativePath": STANDALONE_GENERATED_FILE_RELATIVE_PATH,
                "sizeBytes": len(STANDALONE_GENERATED_FILE_CONTENT),
                "sha256": hashlib.sha256(STANDALONE_GENERATED_FILE_CONTENT).hexdigest(),
            },
            "checkpoint": checkpoint_evidence,
            "providerTurn": provider_evidence,
        }

    def _validate_generated_file_checkpoint(
        self,
        turn_id: str,
        execution_terminal: Mapping[str, Any],
        events: Sequence[Mapping[str, Any]],
        previous_artifact_ids: set[str],
    ) -> Mapping[str, Any]:
        execution_id = execution_terminal.get("executionId")
        if not isinstance(execution_id, str) or not execution_id:
            raise AcceptanceError(
                "runner.generated_file_execution_id_missing",
                "The generated-file Turn omitted its Execution ID.",
            )

        def single_event(event_type: str) -> Mapping[str, Any]:
            matching = [
                event
                for event in events
                if event.get("eventType") == event_type and event.get("executionId") == execution_id
            ]
            if len(matching) != 1:
                raise AcceptanceError(
                    "runner.generated_file_checkpoint_event_invalid",
                    f"Expected exactly one {event_type} event for the generated-file Execution.",
                    {"eventType": event_type, "count": len(matching), "executionId": execution_id},
                )
            return matching[0]

        dirty = single_event("workspace.dirty")
        checkpoint_created = single_event("checkpoint.created")
        checkpoint_ready = single_event("checkpoint.ready")
        generated_ready_events = [
            event
            for event in events
            if event.get("eventType") == "artifact.ready"
            and event.get("executionId") == execution_id
            and isinstance(event.get("payload"), dict)
            and event["payload"].get("kind") == "generated_file"
        ]
        if len(generated_ready_events) != 1:
            raise AcceptanceError(
                "runner.generated_file_artifact_event_invalid",
                "Expected exactly one standalone generated_file artifact.ready event for the generated-file Execution.",
                {
                    "eventType": "artifact.ready",
                    "artifactKind": "generated_file",
                    "count": len(generated_ready_events),
                    "executionId": execution_id,
                },
            )
        generated_ready = generated_ready_events[0]
        snapshot_ready_events = [
            event
            for event in events
            if event.get("eventType") == "artifact.ready"
            and event.get("executionId") == execution_id
            and isinstance(event.get("payload"), dict)
            and event["payload"].get("kind") == "workspace_snapshot"
        ]
        if len(snapshot_ready_events) != 1:
            raise AcceptanceError(
                "runner.generated_file_checkpoint_event_invalid",
                "Expected exactly one workspace_snapshot artifact.ready event for the generated-file Execution.",
                {
                    "eventType": "artifact.ready",
                    "artifactKind": "workspace_snapshot",
                    "count": len(snapshot_ready_events),
                    "executionId": execution_id,
                },
            )
        artifact_ready = snapshot_ready_events[0]
        dirty_payload = json_object(dirty.get("payload"), "generated-file workspace.dirty payload")
        created_payload = json_object(
            checkpoint_created.get("payload"),
            "generated-file checkpoint.created payload",
        )
        generated_payload = json_object(
            generated_ready.get("payload"),
            "generated-file standalone artifact.ready payload",
        )
        artifact_payload = json_object(
            artifact_ready.get("payload"),
            "generated-file artifact.ready payload",
        )
        ready_payload = json_object(
            checkpoint_ready.get("payload"),
            "generated-file checkpoint.ready payload",
        )
        checkpoint_id = created_payload.get("checkpointId")
        generated_artifact_id = generated_payload.get("artifactId")
        artifact_id = ready_payload.get("artifactId")
        workspace_id = created_payload.get("workspaceId")
        checkpoint_sha256 = ready_payload.get("sha256")
        if (
            dirty_payload.get("turnId") != turn_id
            or created_payload.get("turnId") != turn_id
            or ready_payload.get("turnId") != turn_id
            or not isinstance(checkpoint_id, str)
            or not checkpoint_id
            or ready_payload.get("checkpointId") != checkpoint_id
            or not isinstance(workspace_id, str)
            or not workspace_id
            or ready_payload.get("workspaceId") != workspace_id
            or not isinstance(generated_artifact_id, str)
            or not generated_artifact_id
            or generated_artifact_id in previous_artifact_ids
            or generated_payload.get("kind") != "generated_file"
            or generated_payload.get("contentType") != "application/octet-stream"
            or generated_payload.get("sizeBytes") != len(STANDALONE_GENERATED_FILE_CONTENT)
            or created_payload.get("strategy") != "snapshot"
            or ready_payload.get("strategy") != "snapshot"
            or not isinstance(artifact_id, str)
            or not artifact_id
            or artifact_id in previous_artifact_ids
            or artifact_id == generated_artifact_id
            or artifact_payload.get("artifactId") != artifact_id
            or artifact_payload.get("kind") != "workspace_snapshot"
            or artifact_payload.get("contentType") != "application/x-tar"
            or not isinstance(artifact_payload.get("sizeBytes"), int)
            or int(artifact_payload["sizeBytes"]) <= GENERATED_FILE_TOTAL_BYTES
            or not isinstance(checkpoint_sha256, str)
            or re.fullmatch(r"[0-9a-f]{64}", checkpoint_sha256) is None
        ):
            raise AcceptanceError(
                "runner.generated_file_checkpoint_boundary_invalid",
                "The generated-file Checkpoint events did not form one ready Snapshot boundary.",
                {
                    "turnId": turn_id,
                    "dirty": dirty_payload,
                    "generatedArtifactReady": generated_payload,
                    "checkpointCreated": created_payload,
                    "artifactReady": artifact_payload,
                    "checkpointReady": ready_payload,
                },
            )
        ordered_events = [
            generated_ready,
            dirty,
            checkpoint_created,
            artifact_ready,
            checkpoint_ready,
            execution_terminal,
        ]
        sequences = [event.get("sequence") for event in ordered_events]
        if (
            not all(isinstance(sequence, int) for sequence in sequences)
            or sequences != sorted(sequences)
            or len(set(sequences)) != len(sequences)
        ):
            raise AcceptanceError(
                "runner.generated_file_checkpoint_order_invalid",
                "The generated-file Checkpoint lifecycle was not ordered before Execution completion.",
                {"sequences": sequences},
            )
        if any(
            contains_runtime_physical_path(event.get("payload"))
            for event in (
                generated_ready,
                dirty,
                checkpoint_created,
                artifact_ready,
                checkpoint_ready,
            )
        ):
            raise AcceptanceError(
                "runner.generated_file_checkpoint_path_leaked",
                "A generated-file Checkpoint Event exposed a physical Workspace or Artifact path.",
            )

        artifacts = json_items(
            self.api.request("GET", f"/v1/sessions/{self._required('session_id')}/artifacts"),
            "generated-file artifacts",
        )
        generated_artifacts = [
            item
            for item in artifacts
            if item.get("kind") == "generated_file" and item.get("executionId") == execution_id
        ]
        expected_generated_sha256 = hashlib.sha256(STANDALONE_GENERATED_FILE_CONTENT).hexdigest()
        expected_generated_artifact = {
            "id": generated_artifact_id,
            "kind": "generated_file",
            "status": "ready",
            "originalName": pathlib.PurePosixPath(
                STANDALONE_GENERATED_FILE_RELATIVE_PATH
            ).name,
            "contentType": "application/octet-stream",
            "sizeBytes": len(STANDALONE_GENERATED_FILE_CONTENT),
            "sha256": expected_generated_sha256,
            "executionId": execution_id,
        }
        if (
            len(generated_artifacts) != 1
            or {
                key: generated_artifacts[0].get(key)
                for key in expected_generated_artifact
            }
            != expected_generated_artifact
            or contains_runtime_physical_path(generated_artifacts[0])
        ):
            raise AcceptanceError(
                "runner.generated_file_artifact_invalid",
                "The standalone generated_file Artifact metadata did not match its ready Event and Workspace payload.",
                {
                    "expected": expected_generated_artifact,
                    "actual": [self._artifact_summary(item) for item in generated_artifacts],
                },
            )
        generated_artifact = generated_artifacts[0]
        generated_grant = json_object(
            self.api.request("POST", f"/v1/artifacts/{generated_artifact_id}/download"),
            "standalone generated-file Artifact download grant",
        )
        generated_grant_artifact = json_object(
            generated_grant.get("artifact"),
            "standalone generated-file Artifact download grant.artifact",
        )
        generated_download_url = generated_grant.get("url")
        if (
            generated_grant_artifact.get("id") != generated_artifact_id
            or not isinstance(generated_download_url, str)
            or not generated_download_url
        ):
            raise AcceptanceError(
                "runner.generated_file_artifact_download_grant_invalid",
                "The standalone generated_file Artifact download grant was invalid.",
                {"artifactId": generated_artifact_id},
            )
        generated_content = self.api.download_bytes(
            generated_download_url,
            maximum_bytes=STANDALONE_GENERATED_FILE_DOWNLOAD_MAX_BYTES,
        )
        generated_sha256 = hashlib.sha256(generated_content).hexdigest()
        if (
            generated_content != STANDALONE_GENERATED_FILE_CONTENT
            or len(generated_content) != generated_artifact.get("sizeBytes")
            or generated_sha256 != generated_artifact.get("sha256")
        ):
            raise AcceptanceError(
                "runner.generated_file_artifact_download_mismatch",
                "The downloaded standalone generated_file Artifact did not match its ready metadata or deterministic payload.",
                {
                    "artifactId": generated_artifact_id,
                    "actualBytes": len(generated_content),
                    "expectedBytes": generated_artifact.get("sizeBytes"),
                    "actualSha256": generated_sha256,
                    "expectedSha256": generated_artifact.get("sha256"),
                },
            )
        artifact = next((item for item in artifacts if item.get("id") == artifact_id), None)
        expected_artifact = {
            "kind": "workspace_snapshot",
            "status": "ready",
            "contentType": "application/x-tar",
            "sizeBytes": artifact_payload.get("sizeBytes"),
            "sha256": checkpoint_sha256,
            "executionId": execution_id,
        }
        if artifact is None or {key: artifact.get(key) for key in expected_artifact} != expected_artifact:
            raise AcceptanceError(
                "runner.generated_file_checkpoint_artifact_invalid",
                "The generated-file Checkpoint Artifact metadata did not match its ready Events.",
                {
                    "artifactId": artifact_id,
                    "expected": expected_artifact,
                    "actual": self._artifact_summary(artifact) if artifact is not None else None,
                },
            )
        original_name = artifact.get("originalName")
        if (
            not isinstance(original_name, str)
            or not original_name.startswith("workspace-")
            or not original_name.endswith(".tar")
        ):
            raise AcceptanceError(
                "runner.generated_file_checkpoint_artifact_name_invalid",
                "The generated-file Workspace Snapshot used an invalid logical Artifact name.",
                {"artifactId": artifact_id, "originalName": original_name},
            )
        grant = json_object(
            self.api.request("POST", f"/v1/artifacts/{artifact_id}/download"),
            "generated-file Artifact download grant",
        )
        grant_artifact = json_object(
            grant.get("artifact"),
            "generated-file Artifact download grant.artifact",
        )
        download_url = grant.get("url")
        if grant_artifact.get("id") != artifact_id or not isinstance(download_url, str) or not download_url:
            raise AcceptanceError(
                "runner.generated_file_checkpoint_download_grant_invalid",
                "The generated-file Workspace Snapshot download grant was invalid.",
                {"artifactId": artifact_id},
            )
        snapshot = self.api.download_bytes(
            download_url,
            maximum_bytes=GENERATED_FILE_SNAPSHOT_MAX_BYTES,
        )
        snapshot_sha256 = hashlib.sha256(snapshot).hexdigest()
        if len(snapshot) != artifact.get("sizeBytes") or snapshot_sha256 != checkpoint_sha256:
            raise AcceptanceError(
                "runner.generated_file_checkpoint_download_mismatch",
                "The downloaded Workspace Snapshot did not match its ready Artifact metadata.",
                {
                    "artifactId": artifact_id,
                    "actualBytes": len(snapshot),
                    "expectedBytes": artifact.get("sizeBytes"),
                    "actualSha256": snapshot_sha256,
                    "expectedSha256": checkpoint_sha256,
                },
            )
        snapshot_evidence = generated_file_snapshot_evidence(snapshot)
        return {
            "turnId": turn_id,
            "executionId": execution_id,
            "workspaceId": workspace_id,
            "checkpointId": checkpoint_id,
            "strategy": "snapshot",
            "generatedFileArtifact": {
                "artifact": self._artifact_summary(generated_artifact),
                "download": {
                    "sizeBytes": len(generated_content),
                    "sha256": generated_sha256,
                },
            },
            "artifact": self._artifact_summary(artifact),
            "snapshot": snapshot_evidence,
            "sequenceRange": {
                "generatedArtifactReady": generated_ready.get("sequence"),
                "workspaceDirty": dirty.get("sequence"),
                "checkpointCreated": checkpoint_created.get("sequence"),
                "artifactReady": artifact_ready.get("sequence"),
                "checkpointReady": checkpoint_ready.get("sequence"),
                "executionCompleted": execution_terminal.get("sequence"),
            },
            "runtimePhysicalPathLeak": False,
            "duplicateReadyArtifact": False,
            "releaseBoundary": (
                "standalone Provider generated_file ArtifactCandidate and ready workspace_snapshot "
                "Checkpoint are both proven; large Diff remains a separate gate"
            ),
        }

    def _real_provider_approval_request_details(
        self,
        interaction: Mapping[str, Any],
        *,
        turn_id: str,
        expected_command: str,
    ) -> tuple[str, str, dict[str, Any], str]:
        interaction_id = interaction.get("id")
        if not isinstance(interaction_id, str) or not interaction_id:
            raise AcceptanceError(
                "runner.real_provider_approval_interaction_id_invalid",
                "The real Provider Approval interaction omitted a valid interaction ID.",
                {"turnId": turn_id, "interactionId": interaction_id},
            )
        execution_id, request_id = self._interaction_identity(
            interaction,
            "real Provider Approval interaction",
        )
        if interaction.get("turnId") != turn_id:
            raise AcceptanceError(
                "runner.real_provider_approval_turn_mismatch",
                "The real Provider Approval interaction did not stay bound to its Turn.",
                {
                    "expectedTurnId": turn_id,
                    "interactionId": interaction.get("id"),
                    "actualTurnId": interaction.get("turnId"),
                },
            )
        interaction_payload = json_object(
            interaction.get("payload"),
            "real Provider Approval interaction payload",
        )
        command = interaction_payload.get("command")
        if interaction_payload.get("requestKind") != "command" or not isinstance(command, str) or not command:
            raise AcceptanceError(
                "runner.real_provider_approval_payload_invalid",
                "The real Provider Approval interaction did not describe a command request.",
                {
                    "turnId": turn_id,
                    "interactionId": interaction.get("id"),
                    "requestKind": interaction_payload.get("requestKind"),
                },
            )
        if not real_provider_approval_command_matches(command, expected_command):
            raise AcceptanceError(
                "runner.real_provider_approval_command_invalid",
                "The real Provider Approval interaction requested a non-canonical command.",
                {
                    "turnId": turn_id,
                    "interactionId": interaction_id,
                    "requestId": request_id,
                    "actualCommand": self.redactor.text(command[:256]),
                },
            )
        return execution_id, request_id, interaction_payload, command

    def _real_provider_approval_interaction(
        self,
        turn_id: str,
        *,
        expected_command: str,
        session_id: str | None = None,
    ) -> tuple[dict[str, Any], str, str, dict[str, Any], str]:
        interaction = self._wait_for_interaction(turn_id, "approval", session_id=session_id)
        execution_id, request_id, interaction_payload, command = (
            self._real_provider_approval_request_details(
                interaction,
                turn_id=turn_id,
                expected_command=expected_command,
            )
        )
        return interaction, execution_id, request_id, interaction_payload, command

    def _wait_for_turn_terminal_or_follow_up_approval(
        self,
        turn_id: str,
        previous_interaction_id: str,
        previous_request_id: str,
        *,
        session_id: str | None = None,
    ) -> dict[str, Any]:
        resolved_session_id = session_id or self._required("session_id")

        def approval_probe() -> dict[str, Any] | None:
            interactions = json_object(
                self.api.request("GET", f"/v1/sessions/{resolved_session_id}/interactions"),
                "pending interactions",
            )
            items = interactions.get("items")
            if not isinstance(items, list):
                raise AcceptanceError(
                    "runner.response_shape_invalid",
                    "pending interactions.items was not an array.",
                )
            same_turn_pending = [
                item for item in items if isinstance(item, dict) and item.get("turnId") == turn_id
            ]
            invalid_kind = [
                item
                for item in same_turn_pending
                if item.get("kind") != "approval"
            ]
            if invalid_kind:
                raise AcceptanceError(
                    "runner.real_provider_follow_up_interaction_kind_invalid",
                    "The real Provider Approval Turn exposed a non-approval pending interaction.",
                    {
                        "turnId": turn_id,
                        "previousInteractionId": previous_interaction_id,
                        "pendingInteractions": [
                            self._interaction_record_summary(item) for item in invalid_kind
                        ],
                    },
                )
            snapshot = self._turn_terminal_snapshot(
                turn_id,
                session_id=resolved_session_id if session_id is not None else None,
            )
            if snapshot is not None:
                terminal, matching = snapshot
                if same_turn_pending:
                    raise AcceptanceError(
                        "runner.real_provider_terminal_pending_interaction",
                        "The real Provider Approval Turn reached terminal state while pending interactions remained.",
                        {
                            "turnId": turn_id,
                            "terminal": self._terminal_event_summary(terminal),
                            "pendingInteractions": [
                                self._interaction_record_summary(item) for item in same_turn_pending
                            ],
                        },
                    )
                if terminal.get("eventType") != "execution.completed":
                    raise AcceptanceError(
                        "runner.turn_terminal_mismatch",
                        "Real Provider Approval Turn terminated unexpectedly.",
                        {
                            "turnId": turn_id,
                            "terminal": self._terminal_event_summary(terminal),
                            "eventTypes": [event.get("eventType") for event in matching],
                            "runtimeWarnings": self._runtime_warning_summaries(matching),
                        },
                    )
                if resolved_session_id == self.state.session_id:
                    self.state.last_sequence = max(self.state.last_sequence, int(terminal["sequence"]))
                return {
                    "terminal": terminal,
                    "events": matching,
                }
            previous_pending = [
                item
                for item in same_turn_pending
                if item.get("id") == previous_interaction_id
            ]
            if previous_pending:
                if any(item.get("requestId") != previous_request_id for item in previous_pending):
                    raise AcceptanceError(
                        "runner.real_provider_approval_interaction_reused",
                        "The real Provider Approval Turn reused an existing interaction ID for a new request.",
                        {
                            "turnId": turn_id,
                            "interactionId": previous_interaction_id,
                            "previousRequestId": previous_request_id,
                            "pendingInteractions": [
                                self._interaction_record_summary(item) for item in previous_pending
                            ],
                        },
                    )
                return None
            matches = [item for item in same_turn_pending if item.get("id") != previous_interaction_id]
            if len(matches) > 1:
                raise AcceptanceError(
                    "runner.real_provider_follow_up_approval_ambiguous",
                    "The real Provider Approval Turn exposed more than one follow-up Approval at once.",
                    {
                        "turnId": turn_id,
                        "previousInteractionId": previous_interaction_id,
                        "interactionIds": [item.get("id") for item in matches],
                    },
                )
            if matches:
                return {"interaction": matches[0]}
            return None

        return self.api.wait_until(
            f"terminal event or follow-up approval for Turn {turn_id}",
            approval_probe,
        )

    def _real_provider_approval_resolution(self) -> Mapping[str, Any]:
        marker = self._real_provider_marker("approval")
        turn = self._create_turn(
            real_provider_approval_prompt(marker),
            runtime_mode="approval-required",
        )
        turn_id = self._turn_id(turn, "real Provider Approval Turn")
        interaction, execution_id, request_id, interaction_payload, command = (
            self._real_provider_approval_interaction(
                turn_id,
                expected_command=real_provider_approval_command(),
            )
        )
        approvals: list[dict[str, Any]] = []
        seen_request_ids: set[str] = set()
        seen_interaction_ids: set[str] = set()
        terminal: dict[str, Any] | None = None
        events: list[dict[str, Any]] | None = None
        current_interaction = interaction
        current_execution_id = execution_id
        current_request_id = request_id
        current_payload = interaction_payload
        current_command = command
        for _attempt in range(REAL_PROVIDER_MAX_SEQUENTIAL_APPROVALS):
            current_interaction_id = str(current_interaction["id"])
            if current_interaction_id in seen_interaction_ids:
                raise AcceptanceError(
                    "runner.real_provider_approval_interaction_reused",
                    "The real Provider Approval Turn reused a resolved interaction ID.",
                    {
                        "turnId": turn_id,
                        "executionId": execution_id,
                        "interactionId": current_interaction_id,
                        "resolvedInteractionIds": [entry["interactionId"] for entry in approvals],
                    },
                )
            if current_request_id in seen_request_ids:
                raise AcceptanceError(
                    "runner.real_provider_approval_request_reused",
                    "The real Provider Approval Turn reused a resolved Request identity.",
                    {
                        "turnId": turn_id,
                        "executionId": execution_id,
                        "requestId": current_request_id,
                        "interactionId": current_interaction.get("id"),
                        "resolvedRequestIds": [entry["requestId"] for entry in approvals],
                    },
                )
            seen_interaction_ids.add(current_interaction_id)
            resolved = json_object(
                self.api.request(
                    "POST",
                    f"/v1/executions/{current_execution_id}/approvals/{urllib.parse.quote(current_request_id, safe='')}/resolve",
                    {"decision": "accept"},
                ),
                "real Provider Approval resolution",
            )
            approval_entry = {
                "interactionId": current_interaction_id,
                "requestId": current_request_id,
                "requestKind": current_payload.get("requestKind"),
                "commandSummary": self.redactor.text(current_command[:256]),
                "resolutionStatus": resolved.get("status"),
                "deliveryStatus": resolved.get("deliveryStatus"),
            }
            approvals.append(approval_entry)
            seen_request_ids.add(current_request_id)
            outcome = self._wait_for_turn_terminal_or_follow_up_approval(
                turn_id,
                current_interaction_id,
                current_request_id,
            )
            terminal_candidate = outcome.get("terminal")
            if isinstance(terminal_candidate, dict):
                matching_events = outcome.get("events")
                if not isinstance(matching_events, list) or not all(
                    isinstance(event, dict) for event in matching_events
                ):
                    raise AcceptanceError(
                        "runner.response_shape_invalid",
                        "The real Provider Approval terminal snapshot omitted its Event list.",
                    )
                terminal = terminal_candidate
                events = matching_events
                break
            next_interaction = json_object(
                outcome.get("interaction"),
                "real Provider follow-up Approval interaction",
            )
            next_execution_id, next_request_id, next_payload, next_command = (
                self._real_provider_approval_request_details(
                    next_interaction,
                    turn_id=turn_id,
                    expected_command=real_provider_approval_command(),
                )
            )
            if next_execution_id != execution_id:
                raise AcceptanceError(
                    "runner.real_provider_follow_up_approval_execution_mismatch",
                    "The real Provider Approval Turn changed Execution identity between sequential Approvals.",
                    {
                        "turnId": turn_id,
                        "expectedExecutionId": execution_id,
                        "actualExecutionId": next_execution_id,
                        "interactionId": next_interaction.get("id"),
                    },
                )
            current_interaction = next_interaction
            current_execution_id = next_execution_id
            current_request_id = next_request_id
            current_payload = next_payload
            current_command = next_command
        else:
            raise AcceptanceError(
                "runner.real_provider_approval_limit_exceeded",
                "The real Provider Approval Turn required too many sequential Approval resolutions.",
                {
                    "turnId": turn_id,
                    "executionId": execution_id,
                    "maxSequentialApprovals": REAL_PROVIDER_MAX_SEQUENTIAL_APPROVALS,
                    "resolvedApprovalCount": len(approvals),
                    "resolvedRequestIds": [entry["requestId"] for entry in approvals],
                    "nextInteraction": self._interaction_record_summary(current_interaction),
                },
            )

        assert terminal is not None
        assert events is not None
        event_types = [str(event.get("eventType")) for event in events]
        for required_event_type in ("item.started", "item.completed"):
            if required_event_type not in event_types:
                raise AcceptanceError(
                    "runner.real_provider_approval_events_missing",
                    "The real Provider Approval Turn omitted a required durable or tool event.",
                    {
                        "turnId": turn_id,
                        "missingEventType": required_event_type,
                        "eventTypes": event_types,
                    },
                )
        for index, approval_entry in enumerate(approvals, start=1):
            resolved_event = self._interaction_request_event(
                events,
                execution_id,
                str(approval_entry["requestId"]),
                "request.resolved",
                f"real Provider Approval resolution #{index}",
            )
            approval_entry["resolvedEvent"] = self._event_summary(resolved_event)
        evidence = self._real_provider_turn_evidence(
            turn_id,
            terminal,
            events,
            marker,
            expected_resume_strategy="native-cursor",
            expected_resume_reason="cursor_usable",
        )
        self.state.last_real_marker = marker
        first_approval = approvals[0]
        last_approval = approvals[-1]
        return {
            **evidence,
            "interactionId": first_approval["interactionId"],
            "requestId": first_approval["requestId"],
            "requestKind": first_approval["requestKind"],
            "commandSummary": first_approval["commandSummary"],
            "resolutionStatus": last_approval["resolutionStatus"],
            "deliveryStatus": last_approval["deliveryStatus"],
            "approvalCount": len(approvals),
            "approvalResolutions": approvals,
        }

    def _real_provider_user_input_resolution(self) -> Mapping[str, Any]:
        marker = self._real_provider_marker("user-input")
        turn = self._create_turn(
            real_provider_user_input_prompt(self.options.provider, marker),
            runtime_mode="approval-required",
            interaction_mode="plan",
        )
        turn_id = self._turn_id(turn, "real Provider user-input Turn")
        interaction = self._wait_for_interaction(turn_id, "user-input")
        execution_id, request_id = self._interaction_identity(
            interaction,
            "real Provider user-input interaction",
        )
        interaction_payload = json_object(
            interaction.get("payload"),
            "real Provider user-input interaction payload",
        )
        questions = interaction_payload.get("questions")
        if not isinstance(questions, list) or len(questions) != 1 or not isinstance(questions[0], dict):
            raise AcceptanceError(
                "runner.real_provider_user_input_questions_invalid",
                "The real Provider did not request exactly one structured question.",
                {
                    "turnId": turn_id,
                    "interactionId": interaction.get("id"),
                    "questionCount": len(questions) if isinstance(questions, list) else None,
                },
            )
        question = questions[0]
        question_id = question.get("id")
        if not isinstance(question_id, str) or not question_id:
            raise AcceptanceError(
                "runner.real_provider_user_input_question_id_missing",
                "The real Provider structured question omitted its stable ID.",
                {"turnId": turn_id, "interactionId": interaction.get("id")},
            )
        resolved = json_object(
            self.api.request(
                "POST",
                f"/v1/executions/{execution_id}/user-input/{urllib.parse.quote(request_id, safe='')}/resolve",
                {"answers": {question_id: "Staging"}},
            ),
            "real Provider user-input resolution",
        )
        terminal, events = self._wait_for_turn_terminal(turn_id, "execution.completed")
        event_types = [str(event.get("eventType")) for event in events]
        if "user-input.resolved" not in event_types:
            raise AcceptanceError(
                "runner.real_provider_user_input_event_missing",
                "The real Provider user-input Turn completed without user-input.resolved.",
                {"turnId": turn_id, "eventTypes": event_types},
            )
        evidence = self._real_provider_turn_evidence(
            turn_id,
            terminal,
            events,
            marker,
            expected_resume_strategy="native-cursor",
            expected_resume_reason="cursor_usable",
        )
        self.state.last_real_marker = marker
        return {
            **evidence,
            "interactionId": interaction.get("id"),
            "requestId": request_id,
            "question": {
                "id": question_id,
                "header": question.get("header"),
                "question": question.get("question"),
                "optionCount": len(question.get("options"))
                if isinstance(question.get("options"), list)
                else None,
            },
            "answer": "Staging",
            "resolutionStatus": resolved.get("status"),
            "deliveryStatus": resolved.get("deliveryStatus"),
        }

    def _real_provider_steer_active_turn(self) -> Mapping[str, Any]:
        original_marker = self._real_provider_marker("steer-original")
        steered_marker = self._real_provider_marker("steer")
        if self.options.provider == "claudeAgent":
            return self._real_claude_provider_steer_active_turn(original_marker, steered_marker)
        turn = self._create_turn(
            real_provider_steer_prompt(original_marker),
            runtime_mode="approval-required",
        )
        turn_id = self._turn_id(turn, "real Provider Steer Turn")
        interaction, execution_id, request_id, interaction_payload, command = (
            self._real_provider_approval_interaction(
                turn_id,
                expected_command=real_provider_steer_command(),
            )
        )
        before_sequence = self.state.last_sequence
        steer = json_object(
            self.api.request(
                "POST",
                f"/v1/sessions/{self._required('session_id')}/turns/active/steer",
                {
                    "inputText": (
                        "Change the final answer for this active Turn. After the approved command finishes, "
                        f"reply with exactly {steered_marker} and no other text."
                    )
                },
                expected=(200, 201, 202),
            ),
            "real Provider Steer command",
        )
        steer_acknowledged = self._wait_for_execution_event(
            execution_id,
            "turn.steered",
            after_sequence=before_sequence,
        )
        resolved = json_object(
            self.api.request(
                "POST",
                f"/v1/executions/{execution_id}/approvals/{urllib.parse.quote(request_id, safe='')}/resolve",
                {"decision": "accept"},
            ),
            "real Provider Steer Approval resolution",
        )
        terminal, events = self._wait_for_turn_terminal(turn_id, "execution.completed")
        event_types = [str(event.get("eventType")) for event in events]
        for required_event_type in ("turn.steer-requested", "turn.steered", "request.resolved"):
            if required_event_type not in event_types:
                raise AcceptanceError(
                    "runner.real_provider_steer_events_missing",
                    "The real Provider Steer Turn omitted a required control or interaction event.",
                    {
                        "turnId": turn_id,
                        "missingEventType": required_event_type,
                        "eventTypes": event_types,
                    },
                )
        evidence = self._real_provider_turn_evidence(
            turn_id,
            terminal,
            events,
            steered_marker,
            expected_resume_strategy="native-cursor",
            expected_resume_reason="cursor_usable",
        )
        self.state.last_real_marker = steered_marker
        return {
            **evidence,
            "originalMarkerRejected": original_marker != steered_marker,
            "interactionId": interaction.get("id"),
            "requestId": request_id,
            "requestKind": interaction_payload.get("requestKind"),
            "commandSummary": self.redactor.text(command[:256]),
            "approvalStatus": resolved.get("status"),
            "steerControlCommand": {
                "id": steer.get("id"),
                "commandType": steer.get("commandType"),
                "statusAtRequest": steer.get("status"),
            },
            "steerAcknowledgedBeforeApprovalResolution": self._event_summary(
                steer_acknowledged
            ),
            "requestedAfterSequence": before_sequence,
        }

    def _real_claude_provider_steer_active_turn(
        self,
        original_marker: str,
        steered_marker: str,
    ) -> Mapping[str, Any]:
        steer_text = REAL_PROVIDER_STEER_CONTENT.decode("ascii").rstrip("\n")
        steer_command = (
            f"sleep 8 && printf '{steer_text}\\n' > {REAL_PROVIDER_STEER_RELATIVE_PATH}"
        )
        turn = self._create_turn(
            "Use the Bash tool exactly once to run this command: "
            f"{steer_command}. After it succeeds, reply with exactly "
            f"{original_marker} and no other text.",
            runtime_mode="full-access",
        )
        turn_id = self._turn_id(turn, "real Claude Provider Steer Turn")
        created = self._wait_for_turn_created(turn_id)
        execution_id = self._event_execution_id(created)
        started = self._wait_for_execution_event(
            execution_id,
            "item.started",
            after_sequence=int(created.get("sequence") or 0),
        )
        before_sequence = int(started.get("sequence") or self.state.last_sequence)
        steer = json_object(
            self.api.request(
                "POST",
                f"/v1/sessions/{self._required('session_id')}/turns/active/steer",
                {
                    "inputText": (
                        "Change the final answer for this active Turn. After the running command finishes, "
                        f"reply with exactly {steered_marker} and no other text."
                    )
                },
                expected=(200, 201, 202),
            ),
            "real Claude Provider Steer command",
        )
        terminal, events = self._wait_for_turn_terminal(turn_id, "execution.completed")
        event_types = [str(event.get("eventType")) for event in events]
        for required_event_type in (
            "item.started",
            "item.completed",
            "turn.steer-requested",
            "turn.steered",
        ):
            if required_event_type not in event_types:
                raise AcceptanceError(
                    "runner.real_provider_steer_events_missing",
                    "The real Claude Provider Steer Turn omitted a required control or tool event.",
                    {
                        "turnId": turn_id,
                        "missingEventType": required_event_type,
                        "eventTypes": event_types,
                    },
                )
        evidence = self._real_provider_turn_evidence(
            turn_id,
            terminal,
            events,
            steered_marker,
            expected_resume_strategy="native-cursor",
            expected_resume_reason="cursor_usable",
        )
        self.state.last_real_marker = steered_marker
        return {
            **evidence,
            "originalMarkerRejected": original_marker != steered_marker,
            "activeWorkBarrier": {
                "eventId": started.get("eventId"),
                "eventType": started.get("eventType"),
                "sequence": started.get("sequence"),
            },
            "steerControlCommand": {
                "id": steer.get("id"),
                "commandType": steer.get("commandType"),
                "statusAtRequest": steer.get("status"),
            },
            "requestedAfterSequence": before_sequence,
        }

    def _real_provider_interrupt_active_turn(self) -> Mapping[str, Any]:
        unreachable_marker = self._real_provider_marker("interrupt-should-not-complete")
        turn = self._create_turn(
            real_provider_interrupt_prompt(unreachable_marker),
            runtime_mode="approval-required",
        )
        turn_id = self._turn_id(turn, "real Provider Interrupt Turn")
        interaction, execution_id, request_id, interaction_payload, command = (
            self._real_provider_approval_interaction(
                turn_id,
                expected_command=real_provider_interrupt_command(),
            )
        )
        interrupt = json_object(
            self.api.request(
                "POST",
                f"/v1/sessions/{self._required('session_id')}/turns/active/interrupt",
                expected=(200, 201, 202),
            ),
            "real Provider Interrupt command",
        )
        terminal, events = self._wait_for_turn_terminal(turn_id, "execution.interrupted")
        event_types = [str(event.get("eventType")) for event in events]
        for required_event_type in ("request.opened", "turn.interrupt-requested", "execution.interrupted"):
            if required_event_type not in event_types:
                raise AcceptanceError(
                    "runner.real_provider_interrupt_events_missing",
                    "The real Provider Interrupt Turn omitted a required interaction or control event.",
                    {
                        "turnId": turn_id,
                        "missingEventType": required_event_type,
                        "eventTypes": event_types,
                    },
                )
        pending = json_object(
            self.api.request("GET", f"/v1/sessions/{self._required('session_id')}/interactions"),
            "post-interrupt pending interactions",
        )
        pending_items = pending.get("items")
        if not isinstance(pending_items, list):
            raise AcceptanceError(
                "runner.response_shape_invalid",
                "post-interrupt pending interactions.items was not an array.",
            )
        if any(
            isinstance(item, dict)
            and (item.get("id") == interaction.get("id") or item.get("requestId") == request_id)
            for item in pending_items
        ):
            raise AcceptanceError(
                "runner.real_provider_interrupt_interaction_stale",
                "The interrupted real Provider Turn retained its stale Approval interaction.",
                {"turnId": turn_id, "interactionId": interaction.get("id")},
            )

        recovery_marker = self._real_provider_marker("interrupt-recovery")
        recovery_turn = self._create_turn(
            f"Reply with exactly {recovery_marker} and no other text."
        )
        recovery_turn_id = self._turn_id(recovery_turn, "post-interrupt recovery Turn")
        recovery_terminal, recovery_events = self._wait_for_turn_terminal(
            recovery_turn_id,
            "execution.completed",
        )
        recovery_evidence = self._real_provider_turn_evidence(
            recovery_turn_id,
            recovery_terminal,
            recovery_events,
            recovery_marker,
            expected_resume_strategy="native-cursor",
            expected_resume_reason="cursor_usable",
        )
        self.state.last_real_marker = recovery_marker
        worker_id, generation = self._event_worker_identity(terminal)
        return {
            "turnId": turn_id,
            "executionId": execution_id,
            "workerId": worker_id,
            "generation": generation,
            "interactionId": interaction.get("id"),
            "requestId": request_id,
            "requestKind": interaction_payload.get("requestKind"),
            "commandSummary": self.redactor.text(command[:256]),
            "interruptControlCommand": {
                "id": interrupt.get("id"),
                "commandType": interrupt.get("commandType"),
                "statusAtRequest": interrupt.get("status"),
            },
            "interruptedSequenceRange": self._sequence_range(events),
            "staleInteractionRemoved": True,
            "recovery": recovery_evidence,
        }

    def _real_provider_terminal_large_log(self) -> Mapping[str, Any]:
        if self.options.provider == "codex":
            raise AcceptanceUnsupported(
                "runner.real_provider_terminal_large_lossless_output_unsupported",
                "Codex 0.144.x Unified Exec does not expose a lossless stream for output larger than 1 MiB.",
                {
                    "provider": self.options.provider,
                    "supportMode": "unsupported",
                    "providerBoundary": "unified-exec-1MiB-head-tail",
                    "requestedBytes": TERMINAL_LARGE_TOTAL_BYTES,
                    "retainedBytes": 1 << 20,
                    "lossless": False,
                    "compatibleProviderVersionRange": "0.144.x",
                },
            )
        if self.options.provider == "claudeAgent" and self.state.credential_id is None:
            raise AcceptanceUnsupported(
                "runner.real_provider_terminal_large_controlled_credential_required",
                "Claude ambient authentication cannot bind CLAUDE_CONFIG_DIR to the controlled Runtime Output Root.",
                {
                    "provider": self.options.provider,
                    "authentication": "ambient-auth",
                    "supportMode": "unsupported",
                    "requiredAuthentication": "controlled-provider-credential",
                    "requestedBytes": TERMINAL_LARGE_TOTAL_BYTES,
                    "lossless": False,
                    "securityBoundary": (
                        "Provider-retained output paths remain rejected unless they are inside the "
                        "agentd-owned Runtime Output Root"
                    ),
                },
            )
        marker = self._real_provider_marker("terminal-large")
        command = terminal_large_node_command(self.driver.real_provider_node_executable())
        turn = self._create_turn(
            "Use the Bash or shell tool exactly once. Run this exact command as the sole shell command:\n"
            f"{command}\n"
            "Do not add redirections, pipes, wrappers, or any other terminal command. Leave stdout "
            "unmodified and do not append a newline. After the command succeeds, reply with exactly "
            f"{marker} and no other text."
        )
        turn_id = self._turn_id(turn, "real Provider large Terminal Turn")
        terminal, events = self._wait_for_turn_terminal(turn_id, "execution.completed")
        terminal_evidence = self._validate_terminal_large_log(turn_id, terminal, events)
        provider_evidence = self._real_provider_turn_evidence(
            turn_id,
            terminal,
            events,
            marker,
            expected_resume_strategy="native-cursor",
            expected_resume_reason="cursor_usable",
            marker_match_mode="contains-once",
        )
        self.state.last_real_marker = marker
        return {
            "command": {
                "runtime": "node",
                "totalBytes": TERMINAL_LARGE_TOTAL_BYTES,
                "patternBytes": len(TERMINAL_LARGE_PATTERN),
                "commandSha256": hashlib.sha256(command.encode("utf-8")).hexdigest(),
            },
            "terminal": terminal_evidence,
            "providerTurn": provider_evidence,
        }

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
            "Repeat only the unique SYNARA marker from your immediately previous answer. "
            "Output no additional text."
        )
        turn_id = turn.get("id")
        if not isinstance(turn_id, str) or not turn_id:
            raise AcceptanceError(
                "runner.turn_id_missing",
                "The second real Provider smoke Turn did not return an ID.",
            )
        terminal, turn_events = self._wait_for_turn_terminal(turn_id, "execution.completed")
        cursor_expiry_selected = "cursor-expiry" in self.options.real_provider_failure_cases
        evidence = self._real_provider_turn_evidence(
            turn_id,
            terminal,
            turn_events,
            self._last_real_marker(),
            expected_resume_strategy=(
                "authoritative-history" if cursor_expiry_selected else "native-cursor"
            ),
            expected_resume_reason="cursor_expired" if cursor_expiry_selected else "cursor_usable",
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
                "continuityAssertion": (
                    "expired Provider cursor was quarantined and authoritative history restored the immediately "
                    "previous answer"
                    if cursor_expiry_selected
                    else "native Provider cursor restored the immediately previous answer"
                ),
            }
        )
        self.state.rollback_anchor_turn_id = turn_id
        return evidence

    def _real_provider_review(self) -> Mapping[str, Any]:
        session = self._current_session()
        expected_sequence = self._session_last_event_sequence(session)
        operation = json_object(
            self.api.request(
                "POST",
                f"/v1/sessions/{self._required('session_id')}/reviews",
                {
                    "expectedLastEventSequence": expected_sequence,
                    "target": {"type": "uncommittedChanges"},
                    "runtimeMode": "approval-required",
                },
                expected=(202,),
            ),
            "real Provider Review operation",
        )
        turn_id, execution_id, control_command = self._queued_operation_identity(
            operation,
            operation_type="review",
            turn_kind="review",
            command_type="StartReview",
        )
        terminal, events = self._wait_for_turn_terminal(turn_id, "execution.completed")
        if terminal.get("executionId") != execution_id:
            raise AcceptanceError(
                "runner.real_provider_operation_execution_mismatch",
                "The real Provider Review completed a different Execution.",
                {
                    "queuedExecutionId": execution_id,
                    "terminalExecutionId": terminal.get("executionId"),
                },
            )
        completed_item_types = {
            payload.get("itemType")
            for event in events
            if event.get("eventType") == "item.completed"
            and isinstance((payload := event.get("payload")), dict)
        }
        required_item_types = {"review_entered", "review_exited"}
        if not required_item_types.issubset(completed_item_types):
            raise AcceptanceError(
                "runner.real_provider_review_boundary_missing",
                "The real Provider Review omitted its entered or exited boundary.",
                {
                    "turnId": turn_id,
                    "completedItemTypes": sorted(str(value) for value in completed_item_types),
                },
            )
        semantic = self._single_execution_event(events, "session.review.completed")
        semantic_payload = json_object(semantic.get("payload"), "Review semantic payload")
        expected_support_mode = "native" if self.options.provider == "codex" else "emulated"
        if semantic_payload.get("supportMode") != expected_support_mode:
            raise AcceptanceError(
                "runner.real_provider_review_support_mode_mismatch",
                "The real Provider Review persisted the wrong support mode.",
                {
                    "provider": self.options.provider,
                    "expectedSupportMode": expected_support_mode,
                    "actualSupportMode": semantic_payload.get("supportMode"),
                },
            )
        assistant = self._assistant_text_summary(events, "real Provider Review")
        worker_id, generation = self._event_worker_identity(terminal)
        return {
            "turnId": turn_id,
            "executionId": execution_id,
            "workerId": worker_id,
            "generation": generation,
            "supportMode": expected_support_mode,
            "target": {"type": "uncommittedChanges"},
            "controlCommand": {
                "id": control_command.get("id"),
                "commandType": control_command.get("commandType"),
                "statusAtRequest": control_command.get("status"),
            },
            "assistant": assistant,
            "eventTypes": [str(event.get("eventType")) for event in events],
            "sequenceRange": self._sequence_range(events),
            "semanticEvent": self._event_summary(semantic),
        }

    def _real_provider_compact_boundary(self) -> Mapping[str, Any]:
        session = self._current_session()
        expected_sequence = self._session_last_event_sequence(session)
        path = f"/v1/sessions/{self._required('session_id')}/compact"
        if self.options.provider == "claudeAgent":
            try:
                self.api.request(
                    "POST",
                    path,
                    {"expectedLastEventSequence": expected_sequence},
                    expected=(202,),
                )
            except HTTPFailure as error:
                status = error.evidence.get("status")
                if error.code != "capability_unsupported" or status != 409:
                    raise
                after = self._current_session()
                actual_sequence = self._session_last_event_sequence(after)
                if actual_sequence != expected_sequence:
                    raise AcceptanceError(
                        "runner.real_provider_compact_unsupported_mutated",
                        "The explicitly unsupported Claude Compact request mutated Session history.",
                        {
                            "beforeSequence": expected_sequence,
                            "afterSequence": actual_sequence,
                        },
                    )
                raise AcceptanceUnsupported(
                    "capability_unsupported",
                    "Claude Agent SDK Compact is explicitly unsupported.",
                    {
                        "provider": self.options.provider,
                        "supportMode": "unsupported",
                        "httpStatus": status,
                        "sessionSequenceUnchanged": True,
                        "lastEventSequence": actual_sequence,
                    },
                ) from None
            raise AcceptanceError(
                "runner.real_provider_compact_unexpectedly_supported",
                "Claude Compact was accepted even though the Provider capability is explicitly unsupported.",
                {"provider": self.options.provider},
            )

        operation = json_object(
            self.api.request(
                "POST",
                path,
                {"expectedLastEventSequence": expected_sequence},
                expected=(202,),
            ),
            "real Provider Compact operation",
        )
        turn_id, execution_id, control_command = self._queued_operation_identity(
            operation,
            operation_type="compact",
            turn_kind="compact",
            command_type="CompactSession",
        )
        terminal, events = self._wait_for_turn_terminal(turn_id, "execution.completed")
        compact_items = [
            event
            for event in events
            if event.get("eventType") == "item.completed"
            and isinstance(event.get("payload"), dict)
            and event["payload"].get("itemType") == "context_compaction"
        ]
        if len(compact_items) != 1:
            raise AcceptanceError(
                "runner.real_provider_compact_boundary_missing",
                "Codex Compact did not persist exactly one completed context-compaction boundary.",
                {"turnId": turn_id, "boundaryCount": len(compact_items)},
            )
        semantic = self._single_execution_event(events, "thread.state.changed")
        semantic_payload = json_object(semantic.get("payload"), "Compact semantic payload")
        if semantic_payload.get("state") != "compacted" or semantic_payload.get("supportMode") != "native":
            raise AcceptanceError(
                "runner.real_provider_compact_semantic_invalid",
                "Codex Compact persisted an invalid semantic terminal.",
                {
                    "state": semantic_payload.get("state"),
                    "supportMode": semantic_payload.get("supportMode"),
                },
            )
        worker_id, generation = self._event_worker_identity(terminal)
        return {
            "turnId": turn_id,
            "executionId": execution_id,
            "workerId": worker_id,
            "generation": generation,
            "supportMode": "native",
            "controlCommand": {
                "id": control_command.get("id"),
                "commandType": control_command.get("commandType"),
                "statusAtRequest": control_command.get("status"),
            },
            "boundaryEvent": self._event_summary(compact_items[0]),
            "semanticEvent": self._event_summary(semantic),
            "eventTypes": [str(event.get("eventType")) for event in events],
            "sequenceRange": self._sequence_range(events),
        }

    def _real_provider_rollback_emulation(self) -> Mapping[str, Any]:
        session_id = self._required("session_id")
        anchor_turn_id = self._required("rollback_anchor_turn_id")
        session = self._current_session()
        expected_sequence = self._session_last_event_sequence(session)
        result = json_object(
            self.api.request(
                "POST",
                f"/v1/sessions/{session_id}/rollback",
                {
                    "expectedLastEventSequence": expected_sequence,
                    "fromTurnId": anchor_turn_id,
                },
            ),
            "real Provider Rollback emulation",
        )
        if (
            result.get("sessionId") != session_id
            or result.get("fromTurnId") != anchor_turn_id
            or result.get("supportMode") != "emulated"
            or result.get("workspaceDisposition") != "unchanged"
            or result.get("externalSideEffectsReverted") is not False
            or not isinstance(result.get("removedTurnCount"), int)
            or int(result["removedTurnCount"]) < 1
        ):
            raise AcceptanceError(
                "runner.real_provider_rollback_result_invalid",
                "The Control Plane Rollback result omitted its required emulation boundary.",
                {"result": result},
            )
        event_sequence = result.get("eventSequence")
        if event_sequence != expected_sequence + 1:
            raise AcceptanceError(
                "runner.real_provider_rollback_sequence_invalid",
                "The emulated Rollback did not append exactly one authoritative Session Event.",
                {"beforeSequence": expected_sequence, "eventSequence": event_sequence},
            )
        events = self._all_events()
        rollback_event = next(
            (
                event
                for event in events
                if event.get("sequence") == event_sequence
                and event.get("eventType") == "session.history.rolled-back"
            ),
            None,
        )
        if rollback_event is None or any(
            rollback_event.get(key) is not None for key in ("executionId", "workerId", "generation")
        ):
            raise AcceptanceError(
                "runner.real_provider_rollback_event_invalid",
                "Rollback was not persisted as a Worker-free logical history event.",
                {"eventSequence": event_sequence},
            )
        return {
            "sessionId": session_id,
            "fromTurnId": anchor_turn_id,
            "fromSequence": result.get("fromSequence"),
            "removedTurnCount": result.get("removedTurnCount"),
            "supportMode": "emulated",
            "workspaceDisposition": "unchanged",
            "externalSideEffectsReverted": False,
            "workerClaimed": False,
            "event": self._event_summary(rollback_event),
            "sessionSequenceRange": self._sequence_range(events),
        }

    def _real_provider_fork_emulation(self) -> Mapping[str, Any]:
        source_session_id = self._required("session_id")
        source_marker = self._real_provider_marker("fork-source")
        anchor_turn = self._create_turn(
            f"Reply with exactly {source_marker} and no other text."
        )
        anchor_turn_id = self._turn_id(anchor_turn, "pre-Fork marker Turn")
        anchor_terminal, anchor_events = self._wait_for_turn_terminal(
            anchor_turn_id,
            "execution.completed",
        )
        rollback_selected = "rollback" in self.options.real_provider_cases
        anchor_evidence = self._real_provider_turn_evidence(
            anchor_turn_id,
            anchor_terminal,
            anchor_events,
            source_marker,
            expected_resume_strategy=(
                "authoritative-history" if rollback_selected else "native-cursor"
            ),
            expected_resume_reason="cursor_absent" if rollback_selected else "cursor_usable",
        )
        self.state.last_real_marker = source_marker
        source = self._current_session()
        expected_sequence = self._session_last_event_sequence(source)
        fork_input = self._real_provider_fork_input(source, expected_sequence)
        result = json_object(
            self.api.request(
                "POST",
                f"/v1/sessions/{source_session_id}/fork",
                fork_input,
                expected=(201,),
            ),
            "real Provider Fork emulation",
        )
        forked = json_object(result.get("session"), "forked Session")
        forked_session_id = forked.get("id")
        if (
            not isinstance(forked_session_id, str)
            or not forked_session_id
            or forked_session_id == source_session_id
            or result.get("sourceSessionId") != source_session_id
            or result.get("sourceEventSequence") != expected_sequence
            or result.get("supportMode") != "emulated"
            or forked.get("forkSourceSessionId") != source_session_id
            or forked.get("forkStrategy") != "emulated"
        ):
            raise AcceptanceError(
                "runner.real_provider_fork_result_invalid",
                "The Control Plane Fork result omitted its logical lineage boundary.",
                {"result": result},
            )
        self.state.session_id = forked_session_id
        turn = self._create_turn(
            "Repeat your immediately previous answer exactly. Output no additional text."
        )
        turn_id = self._turn_id(turn, "fork continuity Turn")
        terminal, events = self._wait_for_turn_terminal(turn_id, "execution.completed")
        continuity = self._real_provider_turn_evidence(
            turn_id,
            terminal,
            events,
            self._last_real_marker(),
            expected_resume_strategy="authoritative-history",
            expected_resume_reason="cursor_absent",
        )
        return {
            "sourceSessionId": source_session_id,
            "sourceEventSequence": expected_sequence,
            "forkedSessionId": forked_session_id,
            "forkSourceTurnId": forked.get("forkSourceTurnId"),
            "supportMode": "emulated",
            "providerCursorCopied": False,
            "sourceAnchor": anchor_evidence,
            "continuity": continuity,
        }

    @staticmethod
    def _real_provider_fork_input(
        source: Mapping[str, Any], expected_sequence: int
    ) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "expectedLastEventSequence": expected_sequence,
            "title": "Stage 3 real Provider acceptance fork",
        }
        source_credential_id = source.get("providerCredentialId")
        if source_credential_id is None:
            return payload
        if not isinstance(source_credential_id, str) or not source_credential_id:
            raise AcceptanceError(
                "runner.real_provider_fork_credential_invalid",
                "The source Session returned an invalid Provider Credential binding.",
            )
        payload["providerCredentialId"] = source_credential_id
        return payload

    def _current_session(self, session_id: str | None = None) -> dict[str, Any]:
        resolved_session_id = session_id or self._required("session_id")
        return json_object(
            self.api.request("GET", f"/v1/sessions/{resolved_session_id}"),
            "Agent Session",
        )

    @staticmethod
    def _session_last_event_sequence(session: Mapping[str, Any]) -> int:
        sequence = session.get("lastEventSequence")
        if not isinstance(sequence, int) or sequence < 0:
            raise AcceptanceError(
                "runner.session_sequence_missing",
                "Agent Session omitted lastEventSequence.",
            )
        return sequence

    @staticmethod
    def _queued_operation_identity(
        operation: Mapping[str, Any],
        *,
        operation_type: str,
        turn_kind: str,
        command_type: str,
    ) -> tuple[str, str, dict[str, Any]]:
        turn = json_object(operation.get("turn"), f"queued {operation_type} Turn")
        control_command = json_object(
            operation.get("controlCommand"),
            f"queued {operation_type} Control Command",
        )
        turn_id = turn.get("id")
        execution_id = operation.get("executionId")
        if (
            operation.get("type") != operation_type
            or turn.get("turnKind") != turn_kind
            or not isinstance(turn_id, str)
            or not turn_id
            or not isinstance(execution_id, str)
            or not execution_id
            or control_command.get("commandType") != command_type
            or control_command.get("status") != "pending"
        ):
            raise AcceptanceError(
                "runner.real_provider_operation_queue_invalid",
                f"The queued {operation_type} operation returned an invalid identity.",
                {"operation": operation},
            )
        return turn_id, execution_id, control_command

    @staticmethod
    def _single_execution_event(
        events: Sequence[Mapping[str, Any]],
        event_type: str,
    ) -> Mapping[str, Any]:
        matching = [event for event in events if event.get("eventType") == event_type]
        if len(matching) != 1:
            raise AcceptanceError(
                "runner.real_provider_semantic_event_invalid",
                f"Expected exactly one {event_type} event.",
                {"eventType": event_type, "count": len(matching)},
            )
        return matching[0]

    @staticmethod
    def _assistant_text_summary(
        events: Sequence[Mapping[str, Any]],
        description: str,
    ) -> dict[str, Any]:
        deltas: list[str] = []
        sequences: list[int] = []
        for event in events:
            if event.get("eventType") != "content.delta":
                continue
            payload = event.get("payload")
            if not isinstance(payload, dict) or payload.get("streamKind") != "assistant_text":
                continue
            if event.get("eventVersion") != 2 or not isinstance(payload.get("delta"), str):
                raise AcceptanceError(
                    "runner.real_provider_assistant_delta_invalid",
                    f"The {description} emitted an invalid assistant Runtime Event.",
                    {"event": AcceptanceSuite._event_summary(event)},
                )
            deltas.append(str(payload["delta"]))
            if isinstance(event.get("sequence"), int):
                sequences.append(int(event["sequence"]))
        text = "".join(deltas)
        if not text.strip():
            raise AcceptanceError(
                "runner.real_provider_assistant_text_missing",
                f"The {description} completed without canonical assistant text.",
            )
        return {
            "deltaCount": len(deltas),
            "textBytes": len(text.encode("utf-8")),
            "textSha256": hashlib.sha256(text.encode("utf-8")).hexdigest(),
            "sequenceRange": {
                "first": min(sequences) if sequences else None,
                "last": max(sequences) if sequences else None,
            },
        }

    def _real_provider_marker(
        self,
        case: str = "continuity",
        *,
        session_id: str | None = None,
        visible_label: str | None = None,
    ) -> str:
        session_id = session_id or self._required("session_id")
        provider = re.sub(r"[^A-Za-z0-9]+", "_", self.options.provider).strip("_").upper()
        digest = hashlib.sha256(
            f"synara-real-provider-smoke-v1\0{session_id}\0{self.options.provider}\0{case}".encode(
                "utf-8"
            )
        ).hexdigest()[:16].upper()
        label = re.sub(r"[^A-Za-z0-9]+", "_", visible_label or case).strip("_").upper()
        return f"SYNARA_REAL_PROVIDER_{label}_{provider}_{digest}"

    def _last_real_marker(self) -> str:
        marker = self.state.last_real_marker
        if not isinstance(marker, str) or not marker:
            raise AcceptanceError(
                "runner.real_provider_marker_missing",
                "The latest real Provider marker was unavailable before continuity verification.",
            )
        return marker

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
        marker_match_mode: str = "exact",
        max_lease_generations: int = 1,
    ) -> dict[str, Any]:
        if marker_match_mode not in {"exact", "contains-once"}:
            raise ValueError(f"unsupported marker match mode: {marker_match_mode}")
        if max_lease_generations < 1:
            raise ValueError("max_lease_generations must be positive")
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
        marker_matches = (
            normalized_text == expected_marker
            if marker_match_mode == "exact"
            else normalized_text.count(expected_marker) == 1
        )
        if not marker_matches:
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

        worker_id, generation = self._event_worker_identity(terminal)
        leased_events = [event for event in events if event.get("eventType") == "execution.leased"]
        if not 1 <= len(leased_events) <= max_lease_generations:
            raise AcceptanceError(
                "runner.real_provider_resume_decision_missing",
                "The real Provider Turn did not contain the allowed number of Provider resume decisions.",
                {
                    "turnId": turn_id,
                    "executionLeasedEvents": len(leased_events),
                    "maxLeaseGenerations": max_lease_generations,
                },
            )
        leased_generations: list[int] = []
        if max_lease_generations > 1:
            leased_generations = [
                event.get("generation")
                for event in leased_events
                if isinstance(event.get("generation"), int)
            ]
            expected_generations = list(range(1, generation + 1))
            if leased_generations != expected_generations:
                raise AcceptanceError(
                    "runner.real_provider_recovery_generation_invalid",
                    "The real Provider recovery Turn did not use one contiguous lease decision per Generation.",
                    {
                        "turnId": turn_id,
                        "leasedGenerations": leased_generations,
                        "terminalGeneration": generation,
                        "maxLeaseGenerations": max_lease_generations,
                    },
                )
        expected_resume = {
            "requestedStrategy": "native-cursor",
            "selectedStrategy": expected_resume_strategy,
            "reasonCode": expected_resume_reason,
        }
        actual_resume: dict[str, Any] | None = None
        for leased_event in leased_events:
            leased_payload = json_object(leased_event.get("payload"), "execution.leased payload")
            provider_resume = json_object(leased_payload.get("providerResume"), "Provider resume decision")
            leased_resume = {key: provider_resume.get(key) for key in expected_resume}
            if leased_resume != expected_resume:
                raise AcceptanceError(
                    "runner.real_provider_resume_decision_mismatch",
                    "The real Provider Turn used an unexpected resume strategy.",
                    {
                        "turnId": turn_id,
                        "expected": expected_resume,
                        "actual": leased_resume,
                        "leasedGeneration": leased_event.get("generation"),
                    },
                )
            actual_resume = leased_resume
        if actual_resume is None:
            raise AcceptanceError(
                "runner.real_provider_resume_decision_missing",
                "The real Provider Turn did not retain a Provider resume decision.",
                {"turnId": turn_id},
            )

        terminal_payload = terminal.get("payload")
        terminal_output_matches: bool | None = None
        if isinstance(terminal_payload, dict) and isinstance(terminal_payload.get("output"), dict):
            output_text = terminal_payload["output"].get("text")
            if isinstance(output_text, str):
                normalized_output = output_text.strip()
                terminal_output_matches = (
                    normalized_output == expected_marker
                    if marker_match_mode == "exact"
                    else normalized_output.count(expected_marker) == 1
                )
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

        return {
            "turnId": turn_id,
            "executionId": terminal.get("executionId"),
            "workerId": worker_id,
            "generation": generation,
            "expectedMarker": expected_marker,
            "markerMatched": True,
            "markerMatchMode": marker_match_mode,
            "assistantDeltaCount": len(assistant_deltas),
            "assistantTextBytes": len(assistant_text.encode("utf-8")),
            "assistantTextSha256": hashlib.sha256(assistant_text.encode("utf-8")).hexdigest(),
            "assistantSequenceRange": {
                "first": min(assistant_sequences) if assistant_sequences else None,
                "last": max(assistant_sequences) if assistant_sequences else None,
            },
            "terminalOutputMatched": terminal_output_matches,
            "providerResume": actual_resume,
            "executionLeasedEvents": len(leased_events),
            "leasedGenerations": leased_generations,
            "maxLeaseGenerations": max_lease_generations,
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
            "credentialPayloadKeys": ["apiKey"],
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
        turn_id = self._turn_id(turn, "large Terminal fixture Turn")
        execution_terminal, events = self._wait_for_turn_terminal(
            turn_id, "execution.completed"
        )
        return self._validate_terminal_large_log(turn_id, execution_terminal, events)

    def _validate_terminal_large_log(
        self,
        turn_id: str,
        execution_terminal: Mapping[str, Any],
        events: Sequence[Mapping[str, Any]],
    ) -> Mapping[str, Any]:
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
            "turnId": turn_id,
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

    def _fixture_retention_concurrency(self) -> Mapping[str, Any]:
        if not isinstance(self.driver, LocalDriver) or self.driver.name != "local":
            raise AcceptanceError(
                "runner.retention_target_invalid",
                "The deterministic retention concurrency suite requires the isolated Local driver.",
            )
        session_id = self._required("session_id")
        tenant_id = self._required("tenant_id")
        harness = LocalRetentionHarness(self.driver.state_dir)
        seed = harness.load_seed(session_id)
        if seed.tenant_id != tenant_id or seed.target_id != self._required("target_id"):
            raise AcceptanceError(
                "runner.retention_seed_scope_mismatch",
                "The retention seed did not remain inside the active Tenant and Target.",
                {"seed": seed.evidence()},
            )

        approval_turn = self._create_turn("[approval]")
        turn_id = self._turn_id(approval_turn, "retention concurrency Approval Turn")
        interaction = self._wait_for_interaction(turn_id, "approval")
        interaction_id = interaction.get("id")
        if not isinstance(interaction_id, str) or not interaction_id:
            raise AcceptanceError(
                "runner.retention_interaction_id_missing",
                "The retention Approval interaction omitted its ID.",
            )
        active = self._active_approval_evidence(
            turn_id,
            interaction,
            session_id=session_id,
            provider=self.options.provider,
        )
        active_execution_id = str(active["executionId"])

        policy = json_object(
            self.api.request(
                "PUT",
                f"/v1/tenants/{tenant_id}/retention-policy",
                {
                    "sessionArchiveAfterDays": FIXTURE_RETENTION_DAYS,
                    "artifactDeleteAfterDays": FIXTURE_RETENTION_DAYS,
                    "workspaceCleanupAfterDays": FIXTURE_RETENTION_DAYS,
                },
            ),
            "retention policy",
        )
        expected_policy = {
            "sessionArchiveAfterDays": FIXTURE_RETENTION_DAYS,
            "artifactDeleteAfterDays": FIXTURE_RETENTION_DAYS,
            "workspaceCleanupAfterDays": FIXTURE_RETENTION_DAYS,
        }
        actual_policy = {key: policy.get(key) for key in expected_policy}
        if actual_policy != expected_policy:
            raise AcceptanceError(
                "runner.retention_policy_mismatch",
                "The Tenant did not retain the deterministic retention policy.",
                {"expected": expected_policy, "actual": actual_policy},
            )

        expired_at = dt.datetime.now(dt.timezone.utc) - dt.timedelta(
            days=FIXTURE_RETENTION_AGE_DAYS
        )
        harness.stage_active_retention(seed, expired_at)
        active_started = time.monotonic()

        def active_retention_probe() -> dict[str, Any] | None:
            snapshot = harness.snapshot(
                seed,
                active_execution_id,
                interaction_id,
            )
            generated = json_object(
                snapshot.get("generatedArtifact"),
                "active retention generated Artifact",
            )
            if generated.get("status") != "deleted" or generated.get("payloadPresent") is not False:
                return None
            return snapshot

        active_snapshot = self.api.wait_until(
            "retention sweep while the Approval Execution remains active",
            active_retention_probe,
            interval=0.1,
        )
        self._validate_retention_active_snapshot(seed, active_snapshot)
        if not self._interaction_pending(session_id, interaction):
            raise AcceptanceError(
                "runner.retention_interaction_lost",
                "The retention sweep removed the active Approval interaction.",
            )

        resolution = self._resolve_approval_turn(
            approval_turn,
            interaction,
            session_id=session_id,
        )
        harness.age_session(seed, expired_at)
        terminal_started = time.monotonic()

        def terminal_retention_probe() -> dict[str, Any] | None:
            snapshot = harness.snapshot(
                seed,
                active_execution_id,
                interaction_id,
            )
            session = json_object(snapshot.get("session"), "terminal retention Session")
            workspace = json_object(snapshot.get("workspace"), "terminal retention Workspace")
            materialization = json_object(
                snapshot.get("materialization"),
                "terminal retention materialization",
            )
            commands = snapshot.get("cleanupCommands")
            if (
                session.get("status") != "archived"
                or workspace.get("state") != "cleaned"
                or materialization.get("state") != "cleaned"
                or not isinstance(commands, list)
                or len(commands) != 1
                or not isinstance(commands[0], dict)
                or commands[0].get("status") != "acknowledged"
            ):
                return None
            return snapshot

        terminal_snapshot = self.api.wait_until(
            "post-terminal Session archive and physical Workspace cleanup",
            terminal_retention_probe,
            interval=0.1,
        )
        self._validate_retention_terminal_snapshot(seed, terminal_snapshot)
        return {
            "policy": actual_policy,
            "clockSetup": {
                "agedDays": FIXTURE_RETENTION_AGE_DAYS,
                "scope": "runner-owned isolated SQLite rows only",
                "productionClockChanged": False,
            },
            "seed": seed.evidence(),
            "activeApproval": active,
            "duringActiveExecution": {
                "sweepObservedMs": elapsed_ms(active_started),
                "interactionRemainedPending": True,
                **active_snapshot,
            },
            "resolution": resolution,
            "afterTerminal": {
                "cleanupObservedMs": elapsed_ms(terminal_started),
                **terminal_snapshot,
            },
            "sessionArchiveFencedWhileActive": True,
            "workspaceCleanupFencedWhileActive": True,
            "unreferencedArtifactDeletedDuringExecution": True,
            "seedCheckpointRetained": True,
            "postTerminalCurrentCheckpointReady": True,
            "singleTerminal": True,
            "physicalPathsPersisted": False,
        }

    @staticmethod
    def _validate_retention_active_snapshot(
        seed: LocalRetentionSeed,
        snapshot: Mapping[str, Any],
    ) -> None:
        session = json_object(snapshot.get("session"), "active retention Session")
        execution = json_object(snapshot.get("execution"), "active retention Execution")
        interaction = json_object(snapshot.get("interaction"), "active retention interaction")
        workspace = json_object(snapshot.get("workspace"), "active retention Workspace")
        materialization = json_object(
            snapshot.get("materialization"),
            "active retention materialization",
        )
        generated = json_object(
            snapshot.get("generatedArtifact"),
            "active retention generated Artifact",
        )
        seed_checkpoint = json_object(
            snapshot.get("seedCheckpoint"),
            "active retention seed Checkpoint",
        )
        seed_checkpoint_artifact = json_object(
            snapshot.get("seedCheckpointArtifact"),
            "active retention seed Checkpoint Artifact",
        )
        current_checkpoint = json_object(
            snapshot.get("currentCheckpoint"),
            "active retention current Checkpoint",
        )
        current_checkpoint_artifact = json_object(
            snapshot.get("currentCheckpointArtifact"),
            "active retention current Checkpoint Artifact",
        )
        commands = snapshot.get("cleanupCommands")
        valid = (
            session.get("status") == "active"
            and session.get("archived") is False
            and execution.get("status") == "waiting-for-approval"
            and execution.get("leaseCount") == 1
            and interaction.get("status") == "pending"
            and workspace.get("state") != "cleaned"
            and workspace.get("currentCheckpointId") == seed.current_checkpoint_id
            and workspace.get("currentMaterializationId") == seed.materialization_id
            and workspace.get("cleaned") is False
            and workspace.get("generationPresent") is True
            and materialization.get("state") == "active"
            and materialization.get("cleanupReason") == "retention-session-archive"
            and materialization.get("cleanupRequested") is True
            and materialization.get("cleaned") is False
            and materialization.get("activeExecutionCount") == 1
            and generated.get("status") == "deleted"
            and generated.get("deleted") is True
            and generated.get("payloadPresent") is False
            and seed_checkpoint.get("status") == "ready"
            and seed_checkpoint.get("artifactId") == seed.checkpoint_artifact_id
            and seed_checkpoint_artifact.get("status") == "ready"
            and seed_checkpoint_artifact.get("deleted") is False
            and seed_checkpoint_artifact.get("payloadPresent") is True
            and current_checkpoint.get("id") == seed.current_checkpoint_id
            and current_checkpoint.get("status") == "ready"
            and current_checkpoint.get("artifactId") == seed.checkpoint_artifact_id
            and current_checkpoint_artifact.get("id") == seed.checkpoint_artifact_id
            and current_checkpoint_artifact.get("status") == "ready"
            and current_checkpoint_artifact.get("deleted") is False
            and current_checkpoint_artifact.get("payloadPresent") is True
            and commands == []
        )
        if not valid:
            raise AcceptanceError(
                "runner.retention_active_fencing_invalid",
                "Retention did not preserve the active Session, Lease, Checkpoint, and Workspace cleanup fence.",
                {"snapshot": dict(snapshot)},
            )

    @staticmethod
    def _validate_retention_terminal_snapshot(
        seed: LocalRetentionSeed,
        snapshot: Mapping[str, Any],
    ) -> None:
        session = json_object(snapshot.get("session"), "terminal retention Session")
        execution = json_object(snapshot.get("execution"), "terminal retention Execution")
        interaction = json_object(snapshot.get("interaction"), "terminal retention interaction")
        workspace = json_object(snapshot.get("workspace"), "terminal retention Workspace")
        materialization = json_object(
            snapshot.get("materialization"),
            "terminal retention materialization",
        )
        generated = json_object(
            snapshot.get("generatedArtifact"),
            "terminal retention generated Artifact",
        )
        seed_checkpoint = json_object(
            snapshot.get("seedCheckpoint"),
            "terminal retention seed Checkpoint",
        )
        seed_checkpoint_artifact = json_object(
            snapshot.get("seedCheckpointArtifact"),
            "terminal retention seed Checkpoint Artifact",
        )
        current_checkpoint = json_object(
            snapshot.get("currentCheckpoint"),
            "terminal retention current Checkpoint",
        )
        current_checkpoint_artifact = json_object(
            snapshot.get("currentCheckpointArtifact"),
            "terminal retention current Checkpoint Artifact",
        )
        commands = snapshot.get("cleanupCommands")
        command = commands[0] if isinstance(commands, list) and len(commands) == 1 else None
        valid = (
            session.get("status") == "archived"
            and session.get("archived") is True
            and execution.get("status") == "completed"
            and execution.get("leaseCount") == 0
            and interaction.get("status") == "resolved"
            and workspace.get("state") == "cleaned"
            and workspace.get("currentCheckpointId") == current_checkpoint.get("id")
            and workspace.get("currentMaterializationId") == seed.materialization_id
            and workspace.get("cleaned") is True
            and workspace.get("generationPresent") is False
            and materialization.get("state") == "cleaned"
            and materialization.get("cleanupReason") == "retention-session-archive"
            and materialization.get("cleaned") is True
            and materialization.get("activeExecutionCount") == 0
            and generated.get("status") == "deleted"
            and generated.get("deleted") is True
            and generated.get("payloadPresent") is False
            and seed_checkpoint.get("status") == "ready"
            and seed_checkpoint.get("artifactId") == seed.checkpoint_artifact_id
            and seed_checkpoint_artifact.get("status") == "ready"
            and seed_checkpoint_artifact.get("deleted") is False
            and seed_checkpoint_artifact.get("payloadPresent") is True
            and isinstance(current_checkpoint.get("id"), str)
            and current_checkpoint.get("status") == "ready"
            and current_checkpoint.get("artifactId") == current_checkpoint_artifact.get("id")
            and current_checkpoint_artifact.get("status") == "ready"
            and current_checkpoint_artifact.get("deleted") is False
            and current_checkpoint_artifact.get("payloadPresent") is True
            and isinstance(command, dict)
            and command.get("status") == "acknowledged"
            and command.get("reason") == "retention-session-archive"
            and command.get("dispatchGeneration") == 1
            and command.get("deliveryAttempts") == 1
            and command.get("acknowledged") is True
        )
        if not valid:
            raise AcceptanceError(
                "runner.retention_terminal_cleanup_invalid",
                "Post-terminal retention did not archive the Session and acknowledge exactly one physical Workspace cleanup.",
                {"snapshot": dict(snapshot)},
            )

    def _fixture_multi_provider_concurrency(self) -> Mapping[str, Any]:
        primary_session_id = self._required("session_id")
        primary_provider = self.options.provider
        secondary_provider = next(
            provider
            for provider in FIXTURE_CONCURRENCY_PROVIDERS
            if provider != primary_provider
        )
        quota = self._set_fixture_execution_quota(
            FIXTURE_CONCURRENCY_WORKERS,
            "concurrency",
            "runner.concurrency_quota_mismatch",
        )

        secondary_credential = self._create_fixture_credential(
            secondary_provider,
            "Stage 3 Provider Concurrency Fixture",
        )
        secondary_session = self._create_project_session(
            provider=secondary_provider,
            title="Stage 3 Provider Concurrency Secondary",
            credential_id=self._string_id(secondary_credential, "secondary fixture Credential"),
            model="stage3-acceptance-fixture",
            description="secondary concurrency Session",
        )
        secondary_session_id = self._string_id(
            secondary_session,
            "secondary concurrency Session",
        )
        if secondary_session_id == primary_session_id:
            raise AcceptanceError(
                "runner.concurrency_session_reused",
                "The concurrency gate reused one Session for both Providers.",
            )

        primary_turn = self._create_turn("[approval]", session_id=primary_session_id)
        primary_turn_id = self._turn_id(primary_turn, "primary concurrency Turn")
        primary_interaction = self._wait_for_interaction(
            primary_turn_id,
            "approval",
            session_id=primary_session_id,
        )
        secondary_turn = self._create_turn("[approval]", session_id=secondary_session_id)
        secondary_turn_id = self._turn_id(secondary_turn, "secondary concurrency Turn")
        secondary_interaction = self._wait_for_interaction(
            secondary_turn_id,
            "approval",
            session_id=secondary_session_id,
        )

        primary_active = self._active_approval_evidence(
            primary_turn_id,
            primary_interaction,
            session_id=primary_session_id,
            provider=primary_provider,
        )
        secondary_active = self._active_approval_evidence(
            secondary_turn_id,
            secondary_interaction,
            session_id=secondary_session_id,
            provider=secondary_provider,
        )
        execution_ids = {str(primary_active["executionId"]), str(secondary_active["executionId"])}
        worker_ids = {str(primary_active["workerId"]), str(secondary_active["workerId"])}
        if len(execution_ids) != 2:
            raise AcceptanceError(
                "runner.concurrency_execution_reused",
                "The two concurrent Sessions reused one Execution.",
                {"executionIds": sorted(execution_ids)},
            )
        if len(worker_ids) != FIXTURE_CONCURRENCY_WORKERS:
            raise AcceptanceError(
                "runner.concurrency_worker_reused",
                "The two active Executions did not overlap on distinct Workers.",
                {"workerIds": sorted(worker_ids)},
            )
        if not self._interaction_pending(primary_session_id, primary_interaction) or not self._interaction_pending(
            secondary_session_id,
            secondary_interaction,
        ):
            raise AcceptanceError(
                "runner.concurrency_overlap_missing",
                "Both Provider approval barriers were not pending at the same observation point.",
            )

        secondary_resolution = self._resolve_approval_turn(
            secondary_turn,
            secondary_interaction,
            session_id=secondary_session_id,
        )
        if not self._interaction_pending(primary_session_id, primary_interaction):
            raise AcceptanceError(
                "runner.concurrency_isolation_lost",
                "Resolving the secondary Provider interaction changed the primary pending interaction.",
            )
        primary_resolution = self._resolve_approval_turn(
            primary_turn,
            primary_interaction,
            session_id=primary_session_id,
        )
        return {
            "maxConcurrentExecutions": quota.get("maxConcurrentExecutions"),
            "providers": [primary_provider, secondary_provider],
            "distinctSessionCount": 2,
            "distinctExecutionCount": len(execution_ids),
            "distinctWorkerCount": len(worker_ids),
            "simultaneousPendingApprovals": True,
            "primaryRemainedPendingAfterSecondaryResolution": True,
            "doubleExecution": False,
            "duplicateTerminal": False,
            "active": [primary_active, secondary_active],
            "resolved": [primary_resolution, secondary_resolution],
        }

    def _set_fixture_execution_quota(
        self,
        maximum: int,
        description: str,
        mismatch_code: str,
    ) -> dict[str, Any]:
        tenant_id = self._required("tenant_id")
        quota = json_object(
            self.api.request(
                "PUT",
                f"/v1/tenants/{tenant_id}/quota",
                {
                    "maxConcurrentExecutions": maximum,
                    "maxArtifactBytes": None,
                },
            ),
            f"{description} quota",
        )
        if quota.get("maxConcurrentExecutions") != maximum:
            raise AcceptanceError(
                mismatch_code,
                "The Tenant did not retain the requested concurrent Execution quota.",
                {
                    "description": description,
                    "expected": maximum,
                    "actual": quota.get("maxConcurrentExecutions"),
                },
            )
        return quota

    def _start_fixture_load_failure_turn(
        self,
        session: Mapping[str, Any],
        failure_case: str,
        position: int,
    ) -> dict[str, Any]:
        session_id = str(session["sessionId"])
        provider = str(session["provider"])
        turn = self._create_turn(
            f"[approval] fixture-load-{failure_case}-position-{position}",
            session_id=session_id,
        )
        turn_id = self._turn_id(turn, "fixture load failure Turn")
        interaction = self._wait_for_interaction(
            turn_id,
            "approval",
            session_id=session_id,
        )
        active = self._active_approval_evidence(
            turn_id,
            interaction,
            session_id=session_id,
            provider=provider,
        )
        return {
            "sessionId": session_id,
            "provider": provider,
            "turn": turn,
            "interaction": interaction,
            "active": active,
        }

    def _assert_fixture_load_session_unchanged(
        self,
        load_turn: Mapping[str, Any],
        before_events: Sequence[Mapping[str, Any]],
        before_pending: Sequence[Mapping[str, Any]],
        stage: str,
    ) -> None:
        session_id = str(load_turn["sessionId"])
        interaction = json_object(
            load_turn.get("interaction"),
            "load failure unaffected interaction",
        )
        after_events = self._all_events(session_id=session_id)
        after_pending = self._pending_interactions(session_id)
        if (
            after_events != list(before_events)
            or after_pending != list(before_pending)
            or not self._interaction_pending(session_id, interaction)
        ):
            raise AcceptanceError(
                "runner.load_failure_peer_session_mutated",
                "The non-targeted load Session changed while its peer Worker recovered.",
                {
                    "stage": stage,
                    "sessionId": session_id,
                    "interactionId": interaction.get("id"),
                    "beforeEventCount": len(before_events),
                    "afterEventCount": len(after_events),
                    "beforeInteractionCount": len(before_pending),
                    "afterInteractionCount": len(after_pending),
                },
            )

    def _complete_fixture_failure_turn(
        self,
        load_turn: Mapping[str, Any],
        *,
        expected_worker_id: str,
        expected_generation: int,
        expected_request_count: int,
    ) -> dict[str, Any]:
        session_id = str(load_turn["sessionId"])
        turn = json_object(load_turn.get("turn"), "load failure Turn")
        interaction = json_object(load_turn.get("interaction"), "load failure interaction")
        resolution = self._resolve_approval_turn(
            turn,
            interaction,
            session_id=session_id,
        )
        turn_id = self._turn_id(turn, "load failure Turn")
        snapshot = self._turn_terminal_snapshot(turn_id, session_id=session_id)
        if snapshot is None:
            raise AcceptanceError(
                "runner.load_failure_terminal_missing",
                "A resolved load failure Turn had no terminal snapshot.",
                {"sessionId": session_id, "turnId": turn_id},
            )
        terminal, events = snapshot
        event_type_counts: dict[str, int] = {}
        for event in events:
            event_type = str(event.get("eventType") or "")
            event_type_counts[event_type] = event_type_counts.get(event_type, 0) + 1
        terminal_worker_id, terminal_generation = self._event_worker_identity(terminal)
        invalid_terminal_types = [
            event.get("eventType")
            for event in events
            if event.get("eventType") in TERMINAL_EVENT_TYPES
            and event.get("eventType") != "execution.completed"
        ]
        if (
            terminal.get("eventType") != "execution.completed"
            or terminal_worker_id != expected_worker_id
            or terminal_generation != expected_generation
            or event_type_counts.get("request.opened", 0) != expected_request_count
            or event_type_counts.get("request.resolved", 0) != 1
            or event_type_counts.get("execution.completed", 0) != 1
            or invalid_terminal_types
        ):
            raise AcceptanceError(
                "runner.load_failure_terminal_invalid",
                "A load failure Turn did not retain one Generation-fenced terminal path.",
                {
                    "sessionId": session_id,
                    "turnId": turn_id,
                    "expectedWorkerId": expected_worker_id,
                    "terminalWorkerId": terminal_worker_id,
                    "expectedGeneration": expected_generation,
                    "terminalGeneration": terminal_generation,
                    "expectedRequestCount": expected_request_count,
                    "eventTypeCounts": dict(sorted(event_type_counts.items())),
                    "invalidTerminalTypes": invalid_terminal_types,
                },
            )
        return {
            "sessionId": session_id,
            "provider": load_turn.get("provider"),
            "turnId": turn_id,
            "executionId": terminal.get("executionId"),
            "workerId": terminal_worker_id,
            "generation": terminal_generation,
            "resolution": resolution,
            "eventTypeCounts": dict(sorted(event_type_counts.items())),
            "sequenceRange": self._sequence_range(events),
        }

    def _fixture_load_failure_isolation(
        self,
        failure_case: str,
        *,
        session_offset: int,
        affected_index: int,
    ) -> Mapping[str, Any]:
        if failure_case not in FIXTURE_LOAD_GENERATION_RECOVERY_CASES:
            raise AcceptanceError(
                "runner.load_failure_case_invalid",
                "The fixture load failure case was not canonical.",
                {"failureCase": failure_case},
            )
        if affected_index not in {0, 1}:
            raise AcceptanceError(
                "runner.load_failure_affected_index_invalid",
                "The fixture load failure affected index must select one active Execution.",
                {"affectedIndex": affected_index},
            )
        inject = getattr(self.driver, "inject_failure", None)
        if not callable(inject):
            raise AcceptanceUnsupported(
                "runner.load_failure_injection_unsupported",
                "The selected Target cannot inject the exact Worker network failure.",
                {"target": self.driver.name},
            )
        preflight = getattr(self.driver, "validate_failure", None)
        if callable(preflight):
            preflight(failure_case)
        quota = self._set_fixture_execution_quota(
            FIXTURE_CONCURRENCY_WORKERS,
            "load failure",
            "runner.load_failure_quota_mismatch",
        )
        sessions = self._fixture_load_sessions()
        offset = session_offset % len(sessions)
        ordered = sessions[offset:] + sessions[:offset]
        active = [
            self._start_fixture_load_failure_turn(ordered[0], failure_case, 1),
            self._start_fixture_load_failure_turn(ordered[1], failure_case, 2),
        ]
        overlap = self._fixture_load_overlap(active, 0, f"pre-{failure_case}")
        quota_rejections = [
            self._assert_fixture_load_quota_rejected(session, 0, position)
            for position, session in enumerate(ordered[2:], start=3)
        ]
        affected = active[affected_index]
        unaffected = active[1 - affected_index]
        affected_active = json_object(affected.get("active"), "affected load Execution")
        unaffected_active = json_object(unaffected.get("active"), "unaffected load Execution")
        unaffected_session_id = str(unaffected["sessionId"])
        unaffected_events = self._all_events(session_id=unaffected_session_id)
        unaffected_pending = self._pending_interactions(unaffected_session_id)
        recovery, replacement = self._recover_pending_approval_context(
            {
                "turn": json_object(affected.get("turn"), "affected load Turn"),
                "interaction": json_object(
                    affected.get("interaction"),
                    "affected load interaction",
                ),
            },
            session_id=str(affected["sessionId"]),
            recover=lambda target_id, execution_id: inject(
                failure_case,
                target_id,
                execution_id,
            ),
        )
        target_recovery = json_object(
            recovery.get("targetRecovery"),
            "targeted load failure recovery",
        )
        if (
            target_recovery.get("fault") != failure_case
            or target_recovery.get("executionId") != affected_active.get("executionId")
            or target_recovery.get("workerId") != affected_active.get("workerId")
            or target_recovery.get("executionGeneration") != affected_active.get("generation")
            or target_recovery.get("exactExecutionWorkerMatch") is not True
        ):
            raise AcceptanceError(
                "runner.load_failure_target_mismatch",
                "The Worker failure injection did not prove an exact Execution-to-container match.",
                {
                    "affected": affected_active,
                    "targetRecovery": target_recovery,
                },
            )
        if failure_case == "worker-network" and target_recovery.get("restored") is not True:
            raise AcceptanceError(
                "runner.load_failure_network_not_restored",
                "The exact Worker network interruption was not restored.",
                {"targetRecovery": target_recovery},
            )
        if failure_case == "worker-container-loss" and not (
            target_recovery.get("containerIdChanged") is True
            and target_recovery.get("workerIdStable") is True
            and target_recovery.get("workerIncarnationAdvanced") is True
            and target_recovery.get("instanceUidChanged") is True
            and target_recovery.get("replacementReady") is True
            and isinstance(target_recovery.get("namedVolumeContinuity"), Mapping)
            and json_object(
                target_recovery.get("namedVolumeContinuity"),
                "load container replacement volume continuity",
            ).get("preservedAcrossReplacement")
            is True
        ):
            raise AcceptanceError(
                "runner.load_failure_container_replacement_invalid",
                "The exact Worker container loss did not produce one healthy fenced replacement.",
                {"targetRecovery": target_recovery},
            )
        self._assert_fixture_load_session_unchanged(
            unaffected,
            unaffected_events,
            unaffected_pending,
            "after-target-recovery",
        )
        recovered_affected = {**affected, "interaction": replacement}
        affected_terminal = self._complete_fixture_failure_turn(
            recovered_affected,
            expected_worker_id=str(recovery["replacementWorkerId"]),
            expected_generation=int(recovery["replacementGeneration"]),
            expected_request_count=2,
        )
        self._assert_fixture_load_session_unchanged(
            unaffected,
            unaffected_events,
            unaffected_pending,
            "after-target-terminal",
        )
        unaffected_terminal = self._complete_fixture_failure_turn(
            unaffected,
            expected_worker_id=str(unaffected_active["workerId"]),
            expected_generation=int(unaffected_active["generation"]),
            expected_request_count=1,
        )
        leaked = {
            str(session["sessionId"]): self._pending_interactions(str(session["sessionId"]))
            for session in sessions
        }
        leaked = {session_id: items for session_id, items in leaked.items() if items}
        if leaked:
            raise AcceptanceError(
                "runner.load_failure_interaction_leaked",
                "The targeted Worker recovery left pending interactions behind.",
                {
                    "sessions": {
                        session_id: [item.get("id") for item in items]
                        for session_id, items in leaked.items()
                    }
                },
            )
        return {
            "failureCase": failure_case,
            "maxConcurrentExecutions": quota.get("maxConcurrentExecutions"),
            "workers": FIXTURE_CONCURRENCY_WORKERS,
            "sessions": FIXTURE_LOAD_SESSIONS,
            "providers": list(FIXTURE_CONCURRENCY_PROVIDERS),
            "sessionOrder": [str(session["sessionId"]) for session in ordered],
            "affectedIndex": affected_index,
            "overlap": overlap,
            "quotaRejections": quota_rejections,
            "affected": affected_active,
            "unaffected": unaffected_active,
            "recovery": recovery,
            "affectedTerminal": affected_terminal,
            "unaffectedTerminal": unaffected_terminal,
            "peerSessionEventsUnchanged": True,
            "peerInteractionIdentityUnchanged": True,
            "peerWorkerAndGenerationUnchanged": True,
            "targetedGenerationFenced": True,
            "terminalCount": 2,
            "duplicateTerminal": False,
            "pendingInteractionCount": 0,
        }

    def _fixture_load_provider_host_crash_isolation(
        self,
        *,
        session_offset: int,
        affected_index: int,
    ) -> Mapping[str, Any]:
        failure_case = "provider-host-process-crash"
        if affected_index not in {0, 1}:
            raise AcceptanceError(
                "runner.load_failure_affected_index_invalid",
                "The fixture load failure affected index must select one active Execution.",
                {"affectedIndex": affected_index},
            )
        inject = getattr(self.driver, "inject_failure", None)
        if not callable(inject):
            raise AcceptanceUnsupported(
                "runner.load_failure_injection_unsupported",
                "The selected Target cannot inject the exact Provider Host process crash.",
                {"target": self.driver.name},
            )
        preflight = getattr(self.driver, "validate_failure", None)
        if callable(preflight):
            preflight(failure_case)
        quota = self._set_fixture_execution_quota(
            FIXTURE_CONCURRENCY_WORKERS,
            "load Provider Host crash",
            "runner.load_failure_quota_mismatch",
        )
        sessions = self._fixture_load_sessions()
        offset = session_offset % len(sessions)
        ordered = sessions[offset:] + sessions[:offset]
        active = [
            self._start_fixture_load_failure_turn(ordered[0], failure_case, 1),
            self._start_fixture_load_failure_turn(ordered[1], failure_case, 2),
        ]
        overlap = self._fixture_load_overlap(active, 0, "pre-provider-host-process-crash")
        quota_rejections = [
            self._assert_fixture_load_quota_rejected(session, 0, position)
            for position, session in enumerate(ordered[2:], start=3)
        ]
        affected = active[affected_index]
        unaffected = active[1 - affected_index]
        affected_active = json_object(affected.get("active"), "affected Provider Host crash Execution")
        unaffected_active = json_object(unaffected.get("active"), "unaffected Provider Host crash Execution")
        unaffected_session_id = str(unaffected["sessionId"])
        unaffected_events = self._all_events(session_id=unaffected_session_id)
        unaffected_pending = self._pending_interactions(unaffected_session_id)
        crash = dict(
            inject(
                failure_case,
                self._required("target_id"),
                str(affected_active["executionId"]),
            )
        )
        if (
            crash.get("fault") != failure_case
            or crash.get("executionId") != affected_active.get("executionId")
            or crash.get("workerId") != affected_active.get("workerId")
            or crash.get("executionGeneration") != affected_active.get("generation")
            or crash.get("exactExecutionWorkerMatch") is not True
            or crash.get("scopedToManagedContainer") is not True
            or crash.get("scopedToAgentdDescendants") is not True
            or crash.get("broadProcessMatchUsed") is not False
        ):
            raise AcceptanceError(
                "runner.load_provider_host_crash_target_invalid",
                "The Provider Host crash did not remain scoped to the exact active Execution Worker.",
                {"affected": affected_active, "crash": crash},
            )
        affected_turn = json_object(affected.get("turn"), "affected Provider Host crash Turn")
        affected_turn_id = self._turn_id(affected_turn, "affected Provider Host crash Turn")
        terminal, events = self._wait_for_turn_terminal(
            affected_turn_id,
            "execution.failed",
            session_id=str(affected["sessionId"]),
        )
        payload = json_object(terminal.get("payload"), "Provider Host crash execution.failed payload")
        terminal_worker_id, terminal_generation = self._event_worker_identity(terminal)
        terminal_count = sum(
            1 for event in events if event.get("eventType") in TERMINAL_EVENT_TYPES
        )
        if (
            payload.get("failureCode") != "provider_unavailable"
            or terminal.get("executionId") != affected_active.get("executionId")
            or terminal_worker_id != affected_active.get("workerId")
            or terminal_generation != affected_active.get("generation")
            or terminal_count != 1
            or any(event.get("eventType") == "execution.recovering" for event in events)
        ):
            raise AcceptanceError(
                "runner.load_provider_host_crash_terminal_invalid",
                "The exact Provider Host crash did not produce one fenced provider_unavailable terminal.",
                {
                    "affected": affected_active,
                    "terminal": self._event_summary(terminal),
                    "failureCode": payload.get("failureCode"),
                    "terminalCount": terminal_count,
                    "eventTypes": [event.get("eventType") for event in events],
                },
            )
        if self._pending_interactions(str(affected["sessionId"])):
            raise AcceptanceError(
                "runner.load_provider_host_crash_interaction_leaked",
                "The failed Provider Host Generation left a pending interaction behind.",
                {"sessionId": affected["sessionId"]},
            )
        self._assert_fixture_load_session_unchanged(
            unaffected,
            unaffected_events,
            unaffected_pending,
            "after-provider-host-crash-terminal",
        )
        retry_started = time.monotonic()
        retry = self._start_fixture_load_turn(
            ordered[affected_index],
            0,
            5,
        )
        retry_active = json_object(retry.get("active"), "Provider Host crash retry Execution")
        if (
            retry_active.get("executionId") == affected_active.get("executionId")
            or retry_active.get("workerId") != affected_active.get("workerId")
        ):
            raise AcceptanceError(
                "runner.load_provider_host_crash_retry_invalid",
                "Provider Host crash recovery did not use a new Execution on the freed logical Worker.",
                {"failed": affected_active, "retry": retry_active},
            )
        retry_overlap = self._fixture_load_overlap(
            [unaffected, retry],
            0,
            "provider-host-crash-retry",
        )
        retry_terminal = self._complete_fixture_load_turn(retry)
        self._assert_fixture_load_session_unchanged(
            unaffected,
            unaffected_events,
            unaffected_pending,
            "after-provider-host-crash-retry-terminal",
        )
        peer_terminal = self._complete_fixture_failure_turn(
            unaffected,
            expected_worker_id=str(unaffected_active["workerId"]),
            expected_generation=int(unaffected_active["generation"]),
            expected_request_count=1,
        )
        leaked = {
            str(session["sessionId"]): self._pending_interactions(str(session["sessionId"]))
            for session in sessions
        }
        leaked = {session_id: items for session_id, items in leaked.items() if items}
        if leaked:
            raise AcceptanceError(
                "runner.load_failure_interaction_leaked",
                "The Provider Host crash recovery left pending interactions behind.",
                {
                    "sessions": {
                        session_id: [item.get("id") for item in items]
                        for session_id, items in leaked.items()
                    }
                },
            )
        return {
            "failureCase": failure_case,
            "maxConcurrentExecutions": quota.get("maxConcurrentExecutions"),
            "workers": FIXTURE_CONCURRENCY_WORKERS,
            "sessions": FIXTURE_LOAD_SESSIONS,
            "providers": list(FIXTURE_CONCURRENCY_PROVIDERS),
            "sessionOrder": [str(session["sessionId"]) for session in ordered],
            "affectedIndex": affected_index,
            "overlap": overlap,
            "quotaRejections": quota_rejections,
            "affected": affected_active,
            "unaffected": unaffected_active,
            "crash": crash,
            "failureTerminal": {
                "event": self._event_summary(terminal),
                "failureCode": payload.get("failureCode"),
                "sequenceRange": self._sequence_range(events),
            },
            "retry": retry_active,
            "retryOverlap": retry_overlap,
            "retryTerminal": retry_terminal,
            "peerTerminal": peer_terminal,
            "recoveryAdmissionMs": elapsed_ms(retry_started),
            "peerSessionEventsUnchanged": True,
            "peerInteractionIdentityUnchanged": True,
            "peerWorkerAndGenerationUnchanged": True,
            "newExecutionRecovery": True,
            "failedTerminalCount": 1,
            "completedTerminalCount": 2,
            "duplicateTerminal": False,
            "pendingInteractionCount": 0,
        }

    def _fixture_load_admission_waves(
        self,
        *,
        wave_start: int = 0,
        wave_count: int | None = None,
        expected_distinct_workers: int = FIXTURE_CONCURRENCY_WORKERS,
        active_validator: Callable[[Mapping[str, Any]], None] | None = None,
        terminal_validator: (
            Callable[[Mapping[str, Any], Mapping[str, Any]], None] | None
        ) = None,
    ) -> Mapping[str, Any]:
        segmented = wave_count is not None
        wave_count = self.options.load_waves if wave_count is None else wave_count
        minimum_duration_seconds = (
            0.0 if segmented else self.options.load_min_duration_seconds
        )
        maximum_wave_count = (
            wave_count
            if segmented or minimum_duration_seconds <= 0
            else self.options.load_max_waves
        )
        minimum_waves = 1 if segmented else FIXTURE_LOAD_MIN_WAVES
        if not minimum_waves <= wave_count <= FIXTURE_LOAD_MAX_WAVES:
            raise AcceptanceError(
                "runner.load_waves_invalid",
                "The fixture load wave count was outside the accepted range.",
                {
                    "minimum": minimum_waves,
                    "maximum": FIXTURE_LOAD_MAX_WAVES,
                    "actual": wave_count,
                },
            )
        wave_range_limit = (
            FIXTURE_LOAD_MAX_WAVES
            if segmented
            else FIXTURE_LOAD_DURATION_MAX_WAVES
        )
        if (
            wave_start < 0
            or maximum_wave_count < wave_count
            or wave_start + maximum_wave_count > wave_range_limit
        ):
            raise AcceptanceError(
                "runner.load_wave_range_invalid",
                "The fixture load wave range was outside the accepted boundary.",
                {
                    "start": wave_start,
                    "minimumCount": wave_count,
                    "maximumCount": maximum_wave_count,
                    "maximum": wave_range_limit,
                },
            )
        quota = self._set_fixture_execution_quota(
            FIXTURE_CONCURRENCY_WORKERS,
            "load admission",
            "runner.load_quota_mismatch",
        )
        sessions = self._fixture_load_sessions()
        execution_ids: set[str] = set()
        worker_ids: set[str] = set()
        provider_execution_counts = {
            provider: 0 for provider in FIXTURE_CONCURRENCY_PROVIDERS
        }
        session_execution_counts = {
            str(session["sessionId"]): 0 for session in sessions
        }
        event_type_counts: dict[str, int] = {}
        quota_rejections = 0
        overlap_observations = 0
        admission_recovery_ms: list[int] = []
        wave_durations_ms: list[int] = []
        first_wave_samples: list[dict[str, Any]] = []
        last_wave_samples: list[dict[str, Any]] = []
        started = time.monotonic()
        minimum_duration_ms = round(minimum_duration_seconds * 1000)

        def complete(load_turn: Mapping[str, Any]) -> None:
            terminal = self._complete_fixture_load_turn(load_turn)
            if terminal_validator is not None:
                terminal_validator(load_turn, terminal)
            self._accumulate_fixture_load_terminal(
                terminal,
                execution_ids,
                worker_ids,
                provider_execution_counts,
                session_execution_counts,
                event_type_counts,
            )

        def start(
            session: Mapping[str, Any],
            wave_index: int,
            position: int,
        ) -> dict[str, Any]:
            load_turn = self._start_fixture_load_turn(
                session,
                wave_index,
                position,
            )
            if active_validator is not None:
                active_validator(load_turn)
            return load_turn

        local_wave_index = 0
        while local_wave_index < maximum_wave_count:
            wave_index = wave_start + local_wave_index
            wave_started = time.monotonic()
            offset = wave_index % len(sessions)
            ordered = sessions[offset:] + sessions[:offset]
            active = [
                start(ordered[0], wave_index, 1),
                start(ordered[1], wave_index, 2),
            ]
            overlaps = [self._fixture_load_overlap(active, wave_index, "initial")]
            overlap_observations += 1
            worker_ids.update(overlaps[-1]["workerIds"])
            rejections: list[dict[str, Any]] = []

            for position, session in enumerate(ordered[2:], start=3):
                rejections.append(
                    self._assert_fixture_load_quota_rejected(
                        session,
                        wave_index,
                        position,
                    )
                )
                quota_rejections += 1
                complete(active.pop(0))
                self._assert_fixture_load_turn_pending(
                    active[0],
                    wave_index,
                    f"before-position-{position}-admission",
                )
                recovery_started = time.monotonic()
                active.append(
                    start(
                        session,
                        wave_index,
                        position,
                    )
                )
                admission_recovery_ms.append(elapsed_ms(recovery_started))
                overlaps.append(
                    self._fixture_load_overlap(
                        active,
                        wave_index,
                        f"slot-reuse-{position - 2}",
                    )
                )
                overlap_observations += 1
                worker_ids.update(overlaps[-1]["workerIds"])

            complete(active.pop(0))
            self._assert_fixture_load_turn_pending(
                active[0],
                wave_index,
                "before-final-terminal",
            )
            complete(active.pop(0))

            for session in sessions:
                pending = self._pending_interactions(str(session["sessionId"]))
                if pending:
                    raise AcceptanceError(
                        "runner.load_interaction_leaked",
                        "A fixture load wave ended with a pending interaction.",
                        {
                            "wave": wave_index + 1,
                            "sessionId": session["sessionId"],
                            "interactionIds": [item.get("id") for item in pending],
                        },
                    )

            wave_duration = elapsed_ms(wave_started)
            wave_durations_ms.append(wave_duration)
            sample = {
                "wave": wave_index + 1,
                "sessionOrder": [str(session["sessionId"]) for session in ordered],
                "providerOrder": [str(session["provider"]) for session in ordered],
                "overlapWorkerIds": [overlap["workerIds"] for overlap in overlaps],
                "quotaRejections": rejections,
                "durationMs": wave_duration,
            }
            if local_wave_index < 2:
                first_wave_samples.append(sample)
            last_wave_samples = (last_wave_samples + [sample])[-2:]
            local_wave_index += 1
            if (
                local_wave_index >= wave_count
                and elapsed_ms(started) >= minimum_duration_ms
            ):
                break

        completed_wave_count = local_wave_index
        duration_ms = elapsed_ms(started)
        duration_target_met = duration_ms >= minimum_duration_ms
        if completed_wave_count < wave_count or not duration_target_met:
            raise AcceptanceError(
                "runner.load_duration_not_reached",
                "The fixture load run reached its maximum wave safety bound before satisfying the requested duration.",
                {
                    "minimumWaves": wave_count,
                    "maximumWaves": maximum_wave_count,
                    "wavesCompleted": completed_wave_count,
                    "minimumDurationSeconds": minimum_duration_seconds,
                    "durationMs": duration_ms,
                },
            )

        sampled_waves: dict[int, dict[str, Any]] = {}
        for sample in first_wave_samples + last_wave_samples:
            sampled_waves[int(sample["wave"])] = sample
        wave_samples = [sampled_waves[index] for index in sorted(sampled_waves)]

        expected_executions = completed_wave_count * FIXTURE_LOAD_SESSIONS
        expected_rejections = completed_wave_count * (
            FIXTURE_LOAD_SESSIONS - FIXTURE_CONCURRENCY_WORKERS
        )
        expected_overlaps = completed_wave_count * (FIXTURE_LOAD_SESSIONS - 1)
        if (
            len(execution_ids) != expected_executions
            or quota_rejections != expected_rejections
            or overlap_observations != expected_overlaps
            or len(worker_ids) != expected_distinct_workers
            or any(
                count != completed_wave_count
                for count in session_execution_counts.values()
            )
            or any(count != expected_executions // 2 for count in provider_execution_counts.values())
        ):
            raise AcceptanceError(
                "runner.load_aggregate_invalid",
                "The fixture load run did not retain its canonical execution, rejection, overlap, or distribution counts.",
                {
                    "expectedExecutions": expected_executions,
                    "distinctExecutionCount": len(execution_ids),
                    "expectedQuotaRejections": expected_rejections,
                    "quotaRejections": quota_rejections,
                    "expectedOverlapObservations": expected_overlaps,
                    "overlapObservations": overlap_observations,
                    "expectedDistinctWorkerCount": expected_distinct_workers,
                    "workerIds": sorted(worker_ids),
                    "providerExecutionCounts": dict(sorted(provider_execution_counts.items())),
                    "sessionExecutionCounts": dict(sorted(session_execution_counts.items())),
                },
            )

        admission_attempts = len(execution_ids) + quota_rejections
        return {
            "maxConcurrentExecutions": quota.get("maxConcurrentExecutions"),
            "workers": FIXTURE_CONCURRENCY_WORKERS,
            "sessions": FIXTURE_LOAD_SESSIONS,
            "providers": list(FIXTURE_CONCURRENCY_PROVIDERS),
            "wavesRequested": wave_count,
            "minimumWavesRequested": wave_count,
            "maximumWaves": maximum_wave_count,
            "minimumDurationSeconds": minimum_duration_seconds,
            "durationTargetMet": duration_target_met,
            "stopReason": "minimum-waves-and-duration-satisfied",
            "wavesCompleted": completed_wave_count,
            "firstWave": wave_start + 1,
            "lastWave": wave_start + completed_wave_count,
            "executionsCompleted": len(execution_ids),
            "distinctExecutionCount": len(execution_ids),
            "expectedDistinctWorkerCount": expected_distinct_workers,
            "distinctWorkerCount": len(worker_ids),
            "quotaRejections": quota_rejections,
            "admissionRetriesSucceeded": len(admission_recovery_ms),
            "admissionAttempts": admission_attempts,
            "expectedQuotaRejectionRate": round(
                quota_rejections / max(admission_attempts, 1),
                6,
            ),
            "overlapObservations": overlap_observations,
            "effectiveConcurrency": FIXTURE_CONCURRENCY_WORKERS,
            "executionSuccessRate": round(
                len(execution_ids) / max(expected_executions, 1),
                6,
            ),
            "unexpectedFailureCount": 0,
            "unexpectedErrorRate": 0.0,
            "doubleExecution": False,
            "duplicateTerminal": False,
            "pendingInteractionCount": 0,
            "providerExecutionCounts": dict(sorted(provider_execution_counts.items())),
            "sessionExecutionCounts": dict(sorted(session_execution_counts.items())),
            "eventTypeCounts": dict(sorted(event_type_counts.items())),
            "resourceProfile": fixture_load_resource_profile(self.options),
            "durationMs": duration_ms,
            "observedCompletedExecutionsPerSecond": round(
                len(execution_ids) / max(duration_ms / 1000.0, 0.001),
                3,
            ),
            "waveDurationMs": duration_distribution_ms(wave_durations_ms),
            "admissionRecoveryMs": duration_distribution_ms(admission_recovery_ms),
            "sessionsEvidence": [
                {
                    "sessionId": session["sessionId"],
                    "provider": session["provider"],
                }
                for session in sessions
            ],
            "waveSamples": wave_samples,
        }

    def _fixture_load_sessions(self) -> list[dict[str, Any]]:
        if self._fixture_load_session_cache is not None:
            return [dict(session) for session in self._fixture_load_session_cache]
        primary_provider = self.options.provider
        if primary_provider not in FIXTURE_CONCURRENCY_PROVIDERS:
            raise AcceptanceError(
                "runner.load_provider_unsupported",
                "The fixture load suite requires Codex or Claude Agent as its primary Provider.",
                {"provider": primary_provider},
            )
        secondary_provider = next(
            provider
            for provider in FIXTURE_CONCURRENCY_PROVIDERS
            if provider != primary_provider
        )
        credentials = {
            primary_provider: self._required("credential_id"),
        }
        secondary_credential = self._create_fixture_credential(
            secondary_provider,
            "Stage 3 Provider Load Fixture",
        )
        credentials[secondary_provider] = self._string_id(
            secondary_credential,
            "load fixture secondary Credential",
        )
        providers = [
            primary_provider,
            secondary_provider,
            primary_provider,
            secondary_provider,
        ]
        sessions = [
            {
                "sessionId": self._required("session_id"),
                "provider": primary_provider,
                "credentialId": credentials[primary_provider],
            }
        ]
        for index, provider in enumerate(providers[1:], start=2):
            session = self._create_project_session(
                provider=provider,
                title=f"Stage 3 Provider Load Session {index}",
                credential_id=credentials[provider],
                model="stage3-acceptance-fixture",
                description=f"load Session {index}",
            )
            sessions.append(
                {
                    "sessionId": self._string_id(session, f"load Session {index}"),
                    "provider": provider,
                    "credentialId": credentials[provider],
                }
            )
        session_ids = {str(session["sessionId"]) for session in sessions}
        if len(session_ids) != FIXTURE_LOAD_SESSIONS:
            raise AcceptanceError(
                "runner.load_session_reused",
                "The fixture load suite did not create four distinct Sessions.",
                {"sessionIds": sorted(session_ids)},
            )
        self._fixture_load_session_cache = [dict(session) for session in sessions]
        return [dict(session) for session in sessions]

    def _start_fixture_load_turn(
        self,
        session: Mapping[str, Any],
        wave_index: int,
        position: int,
    ) -> dict[str, Any]:
        session_id = str(session["sessionId"])
        provider = str(session["provider"])
        started = time.monotonic()
        turn = self._create_turn(
            "[text] [tool] [usage] [artifact] [credential] [approval] "
            f"fixture-load-wave-{wave_index + 1}-position-{position}",
            session_id=session_id,
        )
        turn_id = self._turn_id(turn, "fixture load Turn")
        interaction = self._wait_for_interaction(
            turn_id,
            "approval",
            session_id=session_id,
        )
        active = self._active_approval_evidence(
            turn_id,
            interaction,
            session_id=session_id,
            provider=provider,
        )
        return {
            "sessionId": session_id,
            "provider": provider,
            "turn": turn,
            "interaction": interaction,
            "active": active,
            "admissionMs": elapsed_ms(started),
        }

    def _fixture_load_overlap(
        self,
        active_turns: Sequence[Mapping[str, Any]],
        wave_index: int,
        stage: str,
    ) -> dict[str, Any]:
        active = [json_object(item.get("active"), "load active evidence") for item in active_turns]
        session_ids = {str(item.get("sessionId")) for item in active}
        execution_ids = {str(item.get("executionId")) for item in active}
        worker_ids = {str(item.get("workerId")) for item in active}
        pending = all(
            self._interaction_pending(
                str(item["sessionId"]),
                json_object(item.get("interaction"), "load overlap interaction"),
            )
            for item in active_turns
        )
        if (
            len(active) != FIXTURE_CONCURRENCY_WORKERS
            or len(session_ids) != FIXTURE_CONCURRENCY_WORKERS
            or len(execution_ids) != FIXTURE_CONCURRENCY_WORKERS
            or len(worker_ids) != FIXTURE_CONCURRENCY_WORKERS
            or not pending
        ):
            raise AcceptanceError(
                "runner.load_worker_overlap_invalid",
                "The fixture load wave did not hold two pending Executions on distinct Workers.",
                {
                    "wave": wave_index + 1,
                    "stage": stage,
                    "sessionIds": sorted(session_ids),
                    "executionIds": sorted(execution_ids),
                    "workerIds": sorted(worker_ids),
                    "allInteractionsPending": pending,
                },
            )
        return {
            "stage": stage,
            "sessionIds": sorted(session_ids),
            "executionIds": sorted(execution_ids),
            "workerIds": sorted(worker_ids),
        }

    def _assert_fixture_load_turn_pending(
        self,
        load_turn: Mapping[str, Any],
        wave_index: int,
        stage: str,
    ) -> None:
        session_id = str(load_turn["sessionId"])
        interaction = json_object(
            load_turn.get("interaction"),
            "load pending interaction",
        )
        if not self._interaction_pending(session_id, interaction):
            raise AcceptanceError(
                "runner.load_overlap_isolation_lost",
                "Completing one load Execution changed another pending Approval.",
                {
                    "wave": wave_index + 1,
                    "stage": stage,
                    "sessionId": session_id,
                    "interactionId": interaction.get("id"),
                },
            )

    def _assert_fixture_load_quota_rejected(
        self,
        session: Mapping[str, Any],
        wave_index: int,
        position: int,
    ) -> dict[str, Any]:
        session_id = str(session["sessionId"])
        before_events = self._all_events(session_id=session_id)
        before_pending = self._pending_interactions(session_id)
        started = time.monotonic()
        try:
            self._create_turn(
                "[approval] "
                f"fixture-load-quota-rejection-wave-{wave_index + 1}-position-{position}",
                session_id=session_id,
            )
        except AcceptanceError as error:
            if error.code != "execution_quota_exceeded":
                raise
            reason_code = error.code
        else:
            raise AcceptanceError(
                "runner.load_quota_not_enforced",
                "The Tenant admitted a third Execution while the two-slot load quota was occupied.",
                {
                    "wave": wave_index + 1,
                    "position": position,
                    "sessionId": session_id,
                },
            )
        after_events = self._all_events(session_id=session_id)
        after_pending = self._pending_interactions(session_id)
        if after_events != before_events or after_pending != before_pending:
            raise AcceptanceError(
                "runner.load_quota_rejection_mutated_session",
                "A rejected load admission mutated the Session Event or interaction state.",
                {
                    "wave": wave_index + 1,
                    "position": position,
                    "sessionId": session_id,
                    "beforeEventCount": len(before_events),
                    "afterEventCount": len(after_events),
                    "beforeInteractionCount": len(before_pending),
                    "afterInteractionCount": len(after_pending),
                },
            )
        return {
            "sessionId": session_id,
            "reasonCode": reason_code,
            "stateMutated": False,
            "durationMs": elapsed_ms(started),
        }

    def _complete_fixture_load_turn(
        self,
        load_turn: Mapping[str, Any],
    ) -> dict[str, Any]:
        session_id = str(load_turn["sessionId"])
        provider = str(load_turn["provider"])
        turn = json_object(load_turn.get("turn"), "load Turn")
        interaction = json_object(load_turn.get("interaction"), "load interaction")
        active = json_object(load_turn.get("active"), "load active evidence")
        resolution = self._resolve_approval_turn(
            turn,
            interaction,
            session_id=session_id,
        )
        turn_id = self._turn_id(turn, "load Turn")
        snapshot = self._turn_terminal_snapshot(turn_id, session_id=session_id)
        if snapshot is None:
            raise AcceptanceError(
                "runner.load_terminal_missing",
                "A resolved fixture load Turn had no terminal snapshot.",
                {"sessionId": session_id, "turnId": turn_id},
            )
        terminal, events = snapshot
        event_type_counts: dict[str, int] = {}
        for event in events:
            event_type = str(event.get("eventType") or "")
            event_type_counts[event_type] = event_type_counts.get(event_type, 0) + 1
        exact_counts = {
            "turn.created": 1,
            "execution.leased": 1,
            "workspace.ready": 1,
            "execution.started": 1,
            "content.delta": 1,
            "item.started": 1,
            "item.completed": 1,
            "thread.token-usage.updated": 1,
            "request.opened": 1,
            "request.resolved": 1,
            "workspace.dirty": 1,
            "checkpoint.created": 1,
            "checkpoint.ready": 1,
            "execution.completed": 1,
        }
        invalid_counts = {
            event_type: {
                "expected": expected,
                "actual": event_type_counts.get(event_type, 0),
            }
            for event_type, expected in exact_counts.items()
            if event_type_counts.get(event_type, 0) != expected
        }
        if event_type_counts.get("artifact.ready", 0) < 2:
            invalid_counts["artifact.ready"] = {
                "expectedMinimum": 2,
                "actual": event_type_counts.get("artifact.ready", 0),
            }
        terminal_payload = json_object(terminal.get("payload"), "load terminal payload")
        output = json_object(terminal_payload.get("output"), "load terminal output")
        credential_evidence = json_object(
            output.get("credentialEvidence"),
            "load credential evidence",
        )
        expected_credential_evidence = {
            "credentialPayloadKeys": ["apiKey"],
            "credentialVerified": True,
        }
        terminal_worker_id, terminal_generation = self._event_worker_identity(terminal)
        if (
            invalid_counts
            or credential_evidence != expected_credential_evidence
            or terminal.get("executionId") != active.get("executionId")
            or terminal_worker_id != active.get("workerId")
            or terminal_generation != active.get("generation")
        ):
            raise AcceptanceError(
                "runner.load_terminal_evidence_invalid",
                "A fixture load Turn omitted required durable events, Credential evidence, or Worker fencing.",
                {
                    "sessionId": session_id,
                    "provider": provider,
                    "turnId": turn_id,
                    "executionId": terminal.get("executionId"),
                    "invalidEventTypeCounts": invalid_counts,
                    "credentialEvidence": credential_evidence,
                    "activeWorkerId": active.get("workerId"),
                    "terminalWorkerId": terminal_worker_id,
                    "activeGeneration": active.get("generation"),
                    "terminalGeneration": terminal_generation,
                },
            )
        return {
            "sessionId": session_id,
            "provider": provider,
            "turnId": turn_id,
            "executionId": terminal.get("executionId"),
            "workerId": terminal_worker_id,
            "generation": terminal_generation,
            "resolution": resolution,
            "eventTypeCounts": dict(sorted(event_type_counts.items())),
            "sequenceRange": self._sequence_range(events),
        }

    @staticmethod
    def _accumulate_fixture_load_terminal(
        terminal: Mapping[str, Any],
        execution_ids: set[str],
        worker_ids: set[str],
        provider_execution_counts: dict[str, int],
        session_execution_counts: dict[str, int],
        event_type_counts: dict[str, int],
    ) -> None:
        execution_id = str(terminal.get("executionId") or "")
        if not execution_id or execution_id in execution_ids:
            raise AcceptanceError(
                "runner.load_execution_reused",
                "The fixture load run reused or omitted an Execution identity.",
                {"executionId": execution_id},
            )
        execution_ids.add(execution_id)
        worker_ids.add(str(terminal["workerId"]))
        provider = str(terminal["provider"])
        session_id = str(terminal["sessionId"])
        provider_execution_counts[provider] += 1
        session_execution_counts[session_id] += 1
        terminal_counts = json_object(
            terminal.get("eventTypeCounts"),
            "load terminal event type counts",
        )
        for event_type, count in terminal_counts.items():
            if isinstance(count, int):
                event_type_counts[event_type] = event_type_counts.get(event_type, 0) + count

    def _active_approval_evidence(
        self,
        turn_id: str,
        interaction: Mapping[str, Any],
        *,
        session_id: str,
        provider: str,
    ) -> dict[str, Any]:
        execution_id, request_id = self._interaction_identity(
            interaction,
            f"{provider} concurrency approval",
        )
        if interaction.get("turnId") != turn_id or interaction.get("kind") != "approval":
            raise AcceptanceError(
                "runner.concurrency_interaction_invalid",
                "The concurrency interaction did not retain its Turn and approval kind.",
                {
                    "provider": provider,
                    "turnId": turn_id,
                    "interactionTurnId": interaction.get("turnId"),
                    "kind": interaction.get("kind"),
                },
            )
        events = self._all_events(session_id=session_id)
        matching = [event for event in events if event.get("executionId") == execution_id]
        event_types = {str(event.get("eventType") or "") for event in matching}
        event_type_counts = {
            event_type: sum(1 for event in matching if event.get("eventType") == event_type)
            for event_type in event_types
        }
        required = {
            "turn.created",
            "execution.leased",
            "workspace.ready",
            "execution.started",
            "request.opened",
        }
        terminals = [event for event in matching if event.get("eventType") in TERMINAL_EVENT_TYPES]
        started = [event for event in matching if event.get("eventType") == "execution.started"]
        invalid_counts = {
            event_type: event_type_counts.get(event_type, 0)
            for event_type in required
            if event_type_counts.get(event_type, 0) != 1
        }
        if invalid_counts or terminals:
            raise AcceptanceError(
                "runner.concurrency_active_execution_invalid",
                "A concurrency barrier was not one live, non-terminal Execution.",
                {
                    "provider": provider,
                    "executionId": execution_id,
                    "invalidEventTypeCounts": dict(sorted(invalid_counts.items())),
                    "terminalCount": len(terminals),
                },
            )
        worker_id, generation = self._event_worker_identity(started[0])
        return {
            "provider": provider,
            "sessionId": session_id,
            "turnId": turn_id,
            "executionId": execution_id,
            "workerId": worker_id,
            "generation": generation,
            "requestId": request_id,
            "interactionId": interaction.get("id"),
            "sequenceRange": self._sequence_range(matching),
            "eventTypes": sorted(event_types),
            "eventTypeCounts": dict(sorted(event_type_counts.items())),
            "terminalBeforeResolution": False,
        }

    def _interaction_pending(
        self,
        session_id: str,
        interaction: Mapping[str, Any],
    ) -> bool:
        interaction_id = interaction.get("id")
        if not isinstance(interaction_id, str) or not interaction_id:
            raise AcceptanceError(
                "runner.interaction_identity_missing",
                "The pending interaction omitted its ID.",
            )
        return any(item.get("id") == interaction_id for item in self._pending_interactions(session_id))

    def _pending_interactions(self, session_id: str) -> list[dict[str, Any]]:
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
        if not all(isinstance(item, dict) for item in items):
            raise AcceptanceError(
                "runner.response_shape_invalid",
                "pending interactions.items contained a non-object value.",
            )
        return [dict(item) for item in items]

    def _execution_interactions(self, execution_id: str) -> list[dict[str, Any]]:
        snapshot = json_object(
            self.api.request(
                "GET",
                f"/v1/executions/{urllib.parse.quote(execution_id, safe='')}/interactions",
            ),
            "execution interactions",
        )
        items = snapshot.get("items")
        if not isinstance(items, list):
            raise AcceptanceError(
                "runner.response_shape_invalid",
                "execution interactions.items was not an array.",
            )
        if not all(isinstance(item, dict) for item in items):
            raise AcceptanceError(
                "runner.response_shape_invalid",
                "execution interactions.items contained a non-object value.",
            )
        return [dict(item) for item in items]

    def _find_execution_interaction_record(
        self,
        execution_id: str,
        interaction_id: str,
    ) -> dict[str, Any] | None:
        matches = [
            item
            for item in self._execution_interactions(execution_id)
            if item.get("id") == interaction_id
        ]
        if len(matches) != 1:
            if not matches:
                return None
            raise AcceptanceError(
                "runner.execution_interaction_missing",
                "Execution interaction history contained duplicate interaction records.",
                {
                    "executionId": execution_id,
                    "interactionId": interaction_id,
                    "matchCount": len(matches),
                },
            )
        return matches[0]

    def _interaction_record_summary(self, interaction: Mapping[str, Any]) -> dict[str, Any]:
        summary = {
            key: interaction.get(key)
            for key in (
                "id",
                "turnId",
                "executionId",
                "workerId",
                "generation",
                "requestId",
                "kind",
                "status",
                "deliveryStatus",
                "deliveryAttempts",
                "resolutionKind",
            )
        }
        delivery_error = interaction.get("deliveryError")
        if isinstance(delivery_error, str) and delivery_error:
            summary["deliveryError"] = self.redactor.text(delivery_error)
        return summary

    def _wait_for_execution_interaction_state(
        self,
        execution_id: str,
        interaction_id: str,
        *,
        status: str,
        delivery_status: str,
        description: str,
    ) -> dict[str, Any]:
        last_seen: dict[str, Any] | None = None

        def probe() -> dict[str, Any] | None:
            nonlocal last_seen
            interaction = self._find_execution_interaction_record(execution_id, interaction_id)
            if interaction is None:
                return None
            last_seen = interaction
            if (
                interaction.get("status") == status
                and interaction.get("deliveryStatus") == delivery_status
            ):
                return interaction
            return None

        try:
            return self.api.wait_until(description, probe)
        except AcceptanceError as error:
            if error.code != "runner.wait_timeout" or last_seen is None:
                raise
            raise AcceptanceError(
                "runner.pending_interaction_not_superseded",
                "The stale Structured User Input did not become expired with superseded delivery after runtime recovery.",
                {
                    "expectedStatus": status,
                    "expectedDeliveryStatus": delivery_status,
                    "interaction": self._interaction_record_summary(last_seen),
                },
            ) from None

    def _resolve_pending_interaction_turn(
        self,
        turn: Mapping[str, Any],
        interaction: Mapping[str, Any],
        *,
        session_id: str,
        request_path: str,
        resolution_payload: Mapping[str, Any],
        interaction_name: str,
        resolution_name: str,
        resolved_event_type: str,
        interaction_invalid_code: str,
        resolution_event_missing_code: str,
        terminal_execution_mismatch_code: str,
    ) -> dict[str, Any]:
        execution_id = interaction.get("executionId")
        request_id = interaction.get("requestId")
        if not isinstance(execution_id, str) or not execution_id or not isinstance(request_id, str) or not request_id:
            raise AcceptanceError(
                interaction_invalid_code,
                f"The {interaction_name} omitted its Execution or Request ID.",
                {"turnId": turn.get("id"), "interactionId": interaction.get("id")},
            )
        resolved = json_object(
            self.api.request(
                "POST",
                f"/v1/executions/{execution_id}/{request_path}/{urllib.parse.quote(request_id, safe='')}/resolve",
                dict(resolution_payload),
            ),
            resolution_name,
        )
        turn_id = self._turn_id(turn, f"{interaction_name} Turn")
        if session_id == self.state.session_id:
            terminal, events = self._wait_for_turn_terminal(
                turn_id,
                "execution.completed",
            )
        else:
            terminal, events = self._wait_for_turn_terminal(
                turn_id,
                "execution.completed",
                session_id=session_id,
            )
        if not any(event.get("eventType") == resolved_event_type for event in events):
            raise AcceptanceError(
                resolution_event_missing_code,
                f"{interaction_name} completed without a {resolved_event_type} event.",
                {"eventTypes": [event.get("eventType") for event in events]},
            )
        terminal_execution_id = self._event_execution_id(terminal)
        if terminal_execution_id != execution_id:
            raise AcceptanceError(
                terminal_execution_mismatch_code,
                f"{interaction_name} resolution completed a different Execution.",
                {
                    "expectedExecutionId": execution_id,
                    "terminalExecutionId": terminal_execution_id,
                },
            )
        return {
            "turnId": turn_id,
            "executionId": terminal_execution_id,
            "requestId": request_id,
            "interactionId": interaction.get("id"),
            "resolutionStatus": resolved.get("status"),
            "deliveryStatus": resolved.get("deliveryStatus"),
            "sequenceRange": self._sequence_range(events),
            "singleTerminal": True,
        }

    def _resolve_approval_turn(
        self,
        turn: Mapping[str, Any],
        interaction: Mapping[str, Any],
        *,
        session_id: str,
    ) -> dict[str, Any]:
        return self._resolve_pending_interaction_turn(
            turn,
            interaction,
            session_id=session_id,
            request_path="approvals",
            resolution_payload={"decision": "accept"},
            interaction_name="Approval interaction",
            resolution_name="approval resolution",
            resolved_event_type="request.resolved",
            interaction_invalid_code="runner.approval_interaction_invalid",
            resolution_event_missing_code="runner.approval_resolution_event_missing",
            terminal_execution_mismatch_code="runner.approval_terminal_execution_mismatch",
        )

    def _approval_resolution(self) -> Mapping[str, Any]:
        pending = self.state.pending_approval
        if pending is None:
            turn = self._create_turn("[approval]")
            interaction = self._wait_for_interaction(str(turn["id"]), "approval")
        else:
            turn = json_object(pending.get("turn"), "pending approval turn")
            interaction = json_object(pending.get("interaction"), "pending approval interaction")
            self.state.pending_approval = None
        result = self._resolve_approval_turn(
            turn,
            interaction,
            session_id=self._required("session_id"),
        )
        execution_id = str(result["executionId"])
        target_evidence = self.driver.observe_terminal_execution(
            self._required("target_id"),
            execution_id,
        )
        return {
            **result,
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

    def _interaction_request_event(
        self,
        events: Sequence[Mapping[str, Any]],
        execution_id: str,
        request_id: str,
        event_type: str,
        description: str,
    ) -> Mapping[str, Any]:
        matches = []
        for event in events:
            if event.get("executionId") != execution_id or event.get("eventType") != event_type:
                continue
            payload = event.get("payload")
            if isinstance(payload, Mapping) and payload.get("requestId") == request_id:
                matches.append(event)
        if len(matches) != 1:
            raise AcceptanceError(
                "runner.pending_interaction_request_event_invalid",
                f"The {description} did not map to exactly one {event_type} Event.",
                {
                    "executionId": execution_id,
                    "requestId": request_id,
                    "eventType": event_type,
                    "matchCount": len(matches),
                },
            )
        return matches[0]

    def _interaction_request_identity(
        self,
        events: Sequence[Mapping[str, Any]],
        execution_id: str,
        request_id: str,
        event_type: str,
        description: str,
    ) -> tuple[str, int]:
        return self._event_worker_identity(
            self._interaction_request_event(
                events,
                execution_id,
                request_id,
                event_type,
                description,
            )
        )

    def _recover_pending_interaction_context(
        self,
        pending: Mapping[str, Any],
        *,
        session_id: str,
        recover: Callable[[str, str], Mapping[str, Any]],
        kind: str,
        label: str,
        recovering_validator: (
            Callable[[Mapping[str, Any], Mapping[str, Any]], None] | None
        ) = None,
        observe_replacement: (
            Callable[[str, str], Mapping[str, Any]] | None
        ) = None,
        verify_stale_superseded: bool = False,
    ) -> tuple[dict[str, Any], dict[str, Any]]:
        turn = json_object(pending.get("turn"), f"pending {label} turn")
        interaction = json_object(pending.get("interaction"), f"pending {label} interaction")
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
                "runner.pending_interaction_invalid",
                f"The pending {label} barrier omitted its Turn, Interaction, Execution, or Request identity.",
                {"turn": turn, "interaction": interaction},
            )
        primary_session = session_id == self.state.session_id
        request_event_type = (
            "request.opened" if kind == "approval" else "user-input.requested"
        )
        before_events = (
            self._all_events()
            if primary_session
            else self._all_events(session_id=session_id)
        )
        stale_worker_id, stale_generation = self._interaction_request_identity(
            before_events,
            execution_id,
            request_id,
            request_event_type,
            f"pending {label}",
        )
        before_sequence = max(
            max((int(event.get("sequence") or 0) for event in before_events), default=0),
            self.state.last_sequence if primary_session else 0,
        )
        target_evidence = recover(self._required("target_id"), execution_id)
        recovery_event = self._wait_for_execution_event(
            execution_id,
            "execution.recovering",
            after_sequence=before_sequence,
            session_id=None if primary_session else session_id,
        )
        recovery_worker_id, recovery_generation = self._event_worker_identity(recovery_event)
        if recovery_worker_id != stale_worker_id or recovery_generation != stale_generation:
            raise AcceptanceError(
                "runner.pending_interaction_recovery_event_identity_invalid",
                "The recovery Event did not fence the stale Worker Generation.",
                {
                    "staleWorkerId": stale_worker_id,
                    "recoveryWorkerId": recovery_worker_id,
                    "staleGeneration": stale_generation,
                    "recoveryGeneration": recovery_generation,
                },
            )
        if recovering_validator is not None:
            recovering_validator(target_evidence, recovery_event)
        replacement = self._wait_for_replacement_interaction(
            turn_id,
            kind,
            previous_interaction_id,
            session_id=None if primary_session else session_id,
        )
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
                f"The recovered {label} omitted its Execution or Request identity.",
                {"interaction": replacement},
            )
        if replacement_request_id == request_id:
            raise AcceptanceError(
                "runner.pending_interaction_request_not_replaced",
                f"Pending {label} recovery reused the obsolete Generation's Request identity.",
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
                f"Pending {label} recovery created a different Execution instead of advancing its Generation.",
                {
                    "staleExecutionId": execution_id,
                    "replacementExecutionId": replacement_execution_id,
                },
            )
        after_events = (
            self._all_events()
            if primary_session
            else self._all_events(session_id=session_id)
        )
        replacement_worker_id, replacement_generation = self._interaction_request_identity(
            after_events,
            replacement_execution_id,
            replacement_request_id,
            request_event_type,
            f"replacement {label}",
        )
        if replacement_generation != stale_generation + 1:
            raise AcceptanceError(
                "runner.pending_interaction_generation_not_advanced",
                f"Pending {label} recovery did not advance exactly one Execution Generation.",
                {
                    "staleGeneration": stale_generation,
                    "replacementGeneration": replacement_generation,
                    "staleWorkerId": stale_worker_id,
                    "replacementWorkerId": replacement_worker_id,
                },
            )
        stale_interaction: dict[str, Any] | None = None
        if verify_stale_superseded:
            stale_interaction = self._wait_for_execution_interaction_state(
                execution_id,
                previous_interaction_id,
                status="expired",
                delivery_status="superseded",
                description=f"stale {label} superseded interaction",
            )
        observer = observe_replacement or self.driver.observe_execution
        target_runtime = observer(
            self._required("target_id"),
            replacement_execution_id,
        )
        deleted_uid = target_evidence.get("deletedPodUid") if isinstance(target_evidence, Mapping) else None
        replacement_uid = target_runtime.get("podUid") if isinstance(target_runtime, Mapping) else None
        if isinstance(deleted_uid, str) and isinstance(replacement_uid, str) and deleted_uid == replacement_uid:
            raise AcceptanceError(
                "runner.pending_interaction_recovery_not_replaced",
                f"Pending {label} recovery reused the deleted execution-pinned runtime identity.",
                {"deletedPodUid": deleted_uid, "replacementPodUid": replacement_uid},
            )
        evidence = {
            "turnId": turn_id,
            "staleInteractionId": previous_interaction_id,
            "staleRequestId": request_id,
            "staleExecutionId": execution_id,
            "staleWorkerId": stale_worker_id,
            "staleGeneration": stale_generation,
            "recoveryEvent": self._event_summary(recovery_event),
            "replacementInteractionId": replacement.get("id"),
            "replacementRequestId": replacement_request_id,
            "replacementExecutionId": replacement_execution_id,
            "replacementWorkerId": replacement_worker_id,
            "replacementGeneration": replacement_generation,
            "targetRecovery": dict(target_evidence),
            "targetRuntime": dict(target_runtime),
        }
        if stale_interaction is not None:
            evidence["staleInteraction"] = self._interaction_record_summary(stale_interaction)
        return evidence, replacement

    def _recover_pending_approval_context(
        self,
        pending: Mapping[str, Any],
        *,
        session_id: str,
        recover: Callable[[str, str], Mapping[str, Any]],
        recovering_validator: (
            Callable[[Mapping[str, Any], Mapping[str, Any]], None] | None
        ) = None,
        observe_replacement: (
            Callable[[str, str], Mapping[str, Any]] | None
        ) = None,
    ) -> tuple[dict[str, Any], dict[str, Any]]:
        return self._recover_pending_interaction_context(
            pending,
            session_id=session_id,
            recover=recover,
            kind="approval",
            label="Approval",
            recovering_validator=recovering_validator,
            observe_replacement=observe_replacement,
        )

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
        evidence, replacement = self._recover_pending_approval_context(
            pending,
            session_id=self._required("session_id"),
            recover=recover,
        )
        self.state.pending_approval = {
            "turn": json_object(pending.get("turn"), "pending approval turn"),
            "interaction": replacement,
        }
        return evidence

    def _recover_pending_user_input_context(
        self,
        pending: Mapping[str, Any],
        *,
        session_id: str,
        recover: Callable[[str, str], Mapping[str, Any]],
        recovering_validator: (
            Callable[[Mapping[str, Any], Mapping[str, Any]], None] | None
        ) = None,
        observe_replacement: (
            Callable[[str, str], Mapping[str, Any]] | None
        ) = None,
    ) -> tuple[dict[str, Any], dict[str, Any]]:
        return self._recover_pending_interaction_context(
            pending,
            session_id=session_id,
            recover=recover,
            kind="user-input",
            label="Structured User Input",
            recovering_validator=recovering_validator,
            observe_replacement=observe_replacement,
            verify_stale_superseded=True,
        )

    def _recover_pending_user_input_with(
        self,
        recover: Callable[[str, str], Mapping[str, Any]],
    ) -> Mapping[str, Any]:
        pending = self.state.pending_user_input
        if pending is None:
            raise AcceptanceError(
                "runner.pending_user_input_missing",
                "The pending structured user input barrier was unavailable for runtime recovery.",
            )
        evidence, replacement = self._recover_pending_user_input_context(
            pending,
            session_id=self._required("session_id"),
            recover=recover,
        )
        self.state.pending_user_input = {
            "turn": json_object(pending.get("turn"), "pending user input turn"),
            "interaction": replacement,
        }
        return evidence

    def _recover_pending_user_input_runtime(self) -> Mapping[str, Any]:
        recover = getattr(self.driver, "recover_pending_interaction", None)
        if not callable(recover):
            raise AcceptanceError(
                "runner.pending_interaction_recovery_unsupported",
                "The TargetDriver cannot recover a pending interaction runtime.",
                {"target": self.driver.name},
            )
        if self.state.pending_user_input is None:
            self._begin_user_input_readiness_barrier()
        return self._recover_pending_user_input_with(recover)

    def _fixture_user_input_answers(
        self,
        interaction: Mapping[str, Any],
    ) -> dict[str, Any]:
        payload = interaction.get("payload")
        if not isinstance(payload, Mapping):
            raise AcceptanceError(
                "runner.user_input_payload_invalid",
                "The deterministic user input interaction omitted its structured payload.",
                {"interactionId": interaction.get("id")},
            )
        questions = payload.get("questions")
        if not isinstance(questions, list) or len(questions) != 1:
            raise AcceptanceError(
                "runner.user_input_payload_invalid",
                "The deterministic user input interaction did not contain exactly one structured question.",
                {
                    "interactionId": interaction.get("id"),
                    "questionCount": len(questions) if isinstance(questions, list) else None,
                },
            )
        question = questions[0]
        if not isinstance(question, Mapping):
            raise AcceptanceError(
                "runner.user_input_payload_invalid",
                "The deterministic user input question was not an object.",
                {"interactionId": interaction.get("id")},
            )
        options = question.get("options")
        option_labels = (
            [
                option.get("label")
                for option in options
                if isinstance(option, Mapping)
            ]
            if isinstance(options, list)
            else None
        )
        expected = {
            "id": "fixture-choice",
            "header": "Fixture",
            "question": "Choose the deterministic acceptance answer.",
            "multiSelect": False,
            "optionLabels": ["Continue", "Stop"],
        }
        actual = {
            "id": question.get("id"),
            "header": question.get("header"),
            "question": question.get("question"),
            "multiSelect": question.get("multiSelect"),
            "optionLabels": option_labels,
        }
        if actual != expected:
            raise AcceptanceError(
                "runner.user_input_payload_invalid",
                "The deterministic user input question changed across runtime recovery.",
                {
                    "interactionId": interaction.get("id"),
                    "expected": expected,
                    "actual": actual,
                },
            )
        return {"fixture-choice": "Continue"}

    def _resolve_user_input_turn(
        self,
        turn: Mapping[str, Any],
        interaction: Mapping[str, Any],
        *,
        session_id: str,
    ) -> dict[str, Any]:
        return self._resolve_pending_interaction_turn(
            turn,
            interaction,
            session_id=session_id,
            request_path="user-input",
            resolution_payload={"answers": self._fixture_user_input_answers(interaction)},
            interaction_name="Structured user input interaction",
            resolution_name="user-input resolution",
            resolved_event_type="user-input.resolved",
            interaction_invalid_code="runner.user_input_interaction_invalid",
            resolution_event_missing_code="runner.user_input_resolution_event_missing",
            terminal_execution_mismatch_code="runner.user_input_terminal_execution_mismatch",
        )

    def _user_input_resolution(self) -> Mapping[str, Any]:
        pending = self.state.pending_user_input
        if pending is None:
            turn = self._create_turn("[user-input]")
            interaction = self._wait_for_interaction(str(turn["id"]), "user-input")
        else:
            turn = json_object(pending.get("turn"), "pending user input turn")
            interaction = json_object(pending.get("interaction"), "pending user input interaction")
            self.state.pending_user_input = None
        result = self._resolve_user_input_turn(
            turn,
            interaction,
            session_id=self._required("session_id"),
        )
        execution_id = str(result["executionId"])
        target_evidence = self.driver.observe_terminal_execution(
            self._required("target_id"),
            execution_id,
        )
        return {
            **result,
            "targetTerminal": dict(target_evidence) if target_evidence else None,
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

    def _fixture_long_session_soak(self) -> Mapping[str, Any]:
        turn_count = self.options.soak_turns
        restart_every = self.options.soak_restart_every
        if turn_count <= 0:
            raise AcceptanceError(
                "runner.soak_turns_invalid",
                "The fixture soak requires at least one configured Turn.",
                {"soakTurns": turn_count},
            )

        before_events = self._all_events()
        before_sequence = int(before_events[-1]["sequence"]) if before_events else 0
        turn_evidence: list[dict[str, Any]] = []
        restart_evidence: list[dict[str, Any]] = []
        execution_ids: set[str] = set()
        worker_ids: set[str] = set()

        for turn_index in range(1, turn_count + 1):
            turn = self._create_turn(f"[text] [tool] [usage] fixture-soak-turn-{turn_index}")
            turn_id = self._turn_id(turn, f"fixture soak Turn {turn_index}")
            terminal, events = self._wait_for_turn_terminal(turn_id, "execution.completed")
            execution_id = self._event_execution_id(terminal)
            if execution_id in execution_ids:
                raise AcceptanceError(
                    "runner.soak_execution_reused",
                    "The fixture soak reused one Execution for multiple Turns.",
                    {"turnIndex": turn_index, "turnId": turn_id, "executionId": execution_id},
                )
            execution_ids.add(execution_id)
            worker_id, generation = self._event_worker_identity(terminal)
            worker_ids.add(worker_id)
            event_types = {str(event.get("eventType") or "") for event in events}
            missing_event_types = sorted(FIXTURE_SOAK_REQUIRED_EVENT_TYPES - event_types)
            if missing_event_types:
                raise AcceptanceError(
                    "runner.soak_turn_evidence_missing",
                    "A fixture soak Turn omitted required text, tool, usage, Workspace or Checkpoint evidence.",
                    {
                        "turnIndex": turn_index,
                        "turnId": turn_id,
                        "executionId": execution_id,
                        "missingEventTypes": missing_event_types,
                    },
                )
            turn_evidence.append(
                {
                    "turnIndex": turn_index,
                    "turnId": turn_id,
                    "executionId": execution_id,
                    "workerId": worker_id,
                    "generation": generation,
                    "terminalSequence": terminal.get("sequence"),
                    "sequenceRange": self._sequence_range(events),
                    "eventTypes": sorted(event_types),
                }
            )

            if restart_every > 0 and turn_index % restart_every == 0 and turn_index < turn_count:
                restarted = self._restart_control_plane()
                restart_evidence.append(
                    {
                        "afterTurn": turn_index,
                        "processGeneration": restarted.get("processGeneration"),
                        "previousPid": restarted.get("previousPid"),
                        "preRestartSequence": restarted.get("preRestartSequence"),
                    }
                )

        all_events = self._all_events()
        terminal_counts = {execution_id: 0 for execution_id in execution_ids}
        created_turn_ids: set[str] = set()
        event_type_counts: dict[str, int] = {}
        for event in all_events:
            event_type = str(event.get("eventType") or "")
            event_type_counts[event_type] = event_type_counts.get(event_type, 0) + 1
            execution_id = event.get("executionId")
            if execution_id in terminal_counts and event_type in TERMINAL_EVENT_TYPES:
                terminal_counts[str(execution_id)] += 1
            turn_id = self._event_turn_id(event)
            if event_type == "turn.created" and isinstance(turn_id, str):
                created_turn_ids.add(turn_id)

        invalid_terminal_counts = {
            execution_id: count for execution_id, count in terminal_counts.items() if count != 1
        }
        expected_turn_ids = {str(item["turnId"]) for item in turn_evidence}
        missing_turns = sorted(expected_turn_ids - created_turn_ids)
        if invalid_terminal_counts or missing_turns:
            raise AcceptanceError(
                "runner.soak_history_invalid",
                "The fixture soak history did not preserve one created Turn and one terminal per Execution.",
                {
                    "invalidTerminalCounts": invalid_terminal_counts,
                    "missingTurnIds": missing_turns,
                },
            )

        pagination_required = turn_count >= 100
        pagination_exercised = len(all_events) > 500
        if pagination_required and not pagination_exercised:
            raise AcceptanceError(
                "runner.soak_event_pagination_not_exercised",
                "The canonical fixture soak did not cross the 500-event pagination boundary.",
                {"soakTurns": turn_count, "eventCount": len(all_events)},
            )

        encoded_identity = json.dumps(
            turn_evidence,
            ensure_ascii=False,
            separators=(",", ":"),
            sort_keys=True,
        ).encode("utf-8")
        sample_size = min(3, len(turn_evidence))
        return {
            "turnsRequested": turn_count,
            "turnsCompleted": len(turn_evidence),
            "distinctExecutionCount": len(execution_ids),
            "distinctWorkerCount": len(worker_ids),
            "controlPlaneRestartEvery": restart_every,
            "controlPlaneRestartCount": len(restart_evidence),
            "controlPlaneRestarts": restart_evidence,
            "sequenceBeforeSoak": before_sequence,
            "sessionSequenceRange": self._sequence_range(all_events),
            "eventsAdded": max(0, len(all_events) - len(before_events)),
            "eventTypeCounts": dict(sorted(event_type_counts.items())),
            "eventPagination": {
                "pageSize": 500,
                "required": pagination_required,
                "exercised": pagination_exercised,
            },
            "doubleExecution": False,
            "duplicateTerminal": False,
            "identitySha256": hashlib.sha256(encoded_identity).hexdigest(),
            "turnSamples": {
                "first": turn_evidence[:sample_size],
                "last": turn_evidence[-sample_size:],
            },
        }

    def _create_turn(
        self,
        input_text: str,
        *,
        runtime_mode: str = "full-access",
        interaction_mode: str = "default",
        session_id: str | None = None,
    ) -> dict[str, Any]:
        session_id = session_id or self._required("session_id")
        return json_object(
            self.api.request(
                "POST",
                f"/v1/sessions/{session_id}/turns",
                {
                    "inputText": input_text,
                    "runtimeMode": runtime_mode,
                    "interactionMode": interaction_mode,
                },
                expected=(201,),
            ),
            "turn",
        )

    @staticmethod
    def _turn_id(turn: Mapping[str, Any], description: str) -> str:
        turn_id = turn.get("id")
        if not isinstance(turn_id, str) or not turn_id:
            raise AcceptanceError(
                "runner.turn_id_missing",
                f"The {description} did not return an ID.",
            )
        return turn_id

    @staticmethod
    def _interaction_identity(interaction: Mapping[str, Any], description: str) -> tuple[str, str]:
        execution_id = interaction.get("executionId")
        request_id = interaction.get("requestId")
        if (
            not isinstance(execution_id, str)
            or not execution_id
            or not isinstance(request_id, str)
            or not request_id
        ):
            raise AcceptanceError(
                "runner.interaction_identity_missing",
                f"The {description} omitted its Execution or Request ID.",
                {"interactionId": interaction.get("id")},
            )
        return execution_id, request_id

    def _wait_for_turn_created(
        self,
        turn_id: str,
        *,
        session_id: str | None = None,
    ) -> dict[str, Any]:
        resolved_session_id = session_id or self._required("session_id")

        def created_probe() -> dict[str, Any] | None:
            events = (
                self._all_events(session_id=resolved_session_id)
                if session_id is not None
                else self._all_events()
            )
            for event in events:
                if event.get("eventType") != "turn.created" or self._event_turn_id(event) != turn_id:
                    continue
                self._event_execution_id(event)
                sequence = event.get("sequence")
                if isinstance(sequence, int) and resolved_session_id == self.state.session_id:
                    self.state.last_sequence = max(self.state.last_sequence, sequence)
                return event
            return None

        return self.api.wait_until(f"turn.created for Turn {turn_id}", created_probe)

    def _wait_for_turn_terminal(
        self,
        turn_id: str,
        expected_event_type: str,
        *,
        session_id: str | None = None,
    ) -> tuple[dict[str, Any], list[dict[str, Any]]]:
        resolved_session_id = session_id or self._required("session_id")

        def terminal_probe() -> tuple[dict[str, Any], list[dict[str, Any]]] | None:
            snapshot = self._turn_terminal_snapshot(
                turn_id,
                session_id=resolved_session_id if session_id is not None else None,
            )
            if snapshot is None:
                return None
            terminal, matching = snapshot
            if terminal.get("eventType") != expected_event_type:
                raise AcceptanceError(
                    "runner.turn_terminal_mismatch",
                    f"Turn terminated as {terminal.get('eventType')} instead of {expected_event_type}.",
                    {
                        "turnId": turn_id,
                        "terminal": self._terminal_event_summary(terminal),
                        "eventTypes": [event.get("eventType") for event in matching],
                        "runtimeWarnings": self._runtime_warning_summaries(matching),
                    },
                )
            if resolved_session_id == self.state.session_id:
                self.state.last_sequence = max(self.state.last_sequence, int(terminal["sequence"]))
            return terminal, matching

        return self.api.wait_until(f"Turn {turn_id} terminal event", terminal_probe)

    def _turn_terminal_snapshot(
        self,
        turn_id: str,
        *,
        session_id: str | None = None,
    ) -> tuple[dict[str, Any], list[dict[str, Any]]] | None:
        events = self._all_events(session_id=session_id) if session_id is not None else self._all_events()
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
        return terminals[0], matching

    def _raise_if_turn_terminated_without_interaction(
        self,
        turn_id: str,
        kind: str,
        *,
        session_id: str,
    ) -> None:
        snapshot = self._turn_terminal_snapshot(
            turn_id,
            session_id=session_id if session_id != self.state.session_id else None,
        )
        if snapshot is None:
            return
        terminal, matching = snapshot
        raise AcceptanceError(
            "runner.interaction_missing_after_terminal",
            f"Turn terminated before producing the required {kind} interaction.",
            {
                "turnId": turn_id,
                "expectedInteractionKind": kind,
                "terminal": self._terminal_event_summary(terminal),
                "eventTypes": [event.get("eventType") for event in matching],
                "runtimeWarnings": self._runtime_warning_summaries(matching),
                "providerItems": self._provider_item_summaries(matching),
            },
        )

    def _wait_for_interaction(
        self,
        turn_id: str,
        kind: str,
        *,
        session_id: str | None = None,
    ) -> dict[str, Any]:
        session_id = session_id or self._required("session_id")

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
            self._raise_if_turn_terminated_without_interaction(
                turn_id,
                kind,
                session_id=session_id,
            )
            return None

        return self.api.wait_until(f"{kind} interaction for Turn {turn_id}", interaction_probe)

    def _wait_for_replacement_interaction(
        self,
        turn_id: str,
        kind: str,
        previous_interaction_id: str,
        *,
        session_id: str | None = None,
    ) -> dict[str, Any]:
        session_id = session_id or self._required("session_id")

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
            previous_pending = any(
                isinstance(item, dict) and item.get("id") == previous_interaction_id for item in items
            )
            if not previous_pending:
                for item in items:
                    if (
                        isinstance(item, dict)
                        and item.get("turnId") == turn_id
                        and item.get("kind") == kind
                        and item.get("id") != previous_interaction_id
                    ):
                        return item
            self._raise_if_turn_terminated_without_interaction(
                turn_id,
                kind,
                session_id=session_id,
            )
            return None

        return self.api.wait_until(f"replacement {kind} interaction for Turn {turn_id}", interaction_probe)

    def _wait_for_execution_event(
        self,
        execution_id: str,
        event_type: str,
        *,
        after_sequence: int,
        session_id: str | None = None,
    ) -> dict[str, Any]:
        resolved_session_id = session_id or self._required("session_id")

        def event_probe() -> dict[str, Any] | None:
            events = (
                self._all_events(session_id=resolved_session_id)
                if session_id is not None
                else self._all_events()
            )
            for event in events:
                sequence = int(event.get("sequence") or 0)
                if sequence <= after_sequence:
                    continue
                if event.get("executionId") == execution_id and event.get("eventType") == event_type:
                    if resolved_session_id == self.state.session_id:
                        self.state.last_sequence = max(self.state.last_sequence, sequence)
                    return event
            return None

        return self.api.wait_until(f"{event_type} for Execution {execution_id}", event_probe)

    def _all_events(self, *, session_id: str | None = None) -> list[dict[str, Any]]:
        session_id = session_id or self._required("session_id")
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

    def _redacted_message_summary(self, value: Any) -> dict[str, Any] | None:
        if not isinstance(value, str) or not value:
            return None
        redacted = self.redactor.text(value)
        encoded = redacted.encode("utf-8")
        return {
            "bytes": len(encoded),
            "sha256": hashlib.sha256(encoded).hexdigest(),
            "preview": redacted[:512],
        }

    def _terminal_event_summary(self, event: Mapping[str, Any]) -> dict[str, Any]:
        summary = self._event_summary(event)
        if event.get("eventType") != "execution.failed":
            return summary
        payload = event.get("payload")
        if not isinstance(payload, dict):
            return summary
        failure: dict[str, Any] = {}
        failure_code = payload.get("failureCode")
        if isinstance(failure_code, str) and failure_code:
            failure["code"] = failure_code
        message = self._redacted_message_summary(payload.get("failureMessage"))
        if message is not None:
            failure["message"] = message
        if failure:
            summary["failure"] = failure
        return summary

    def _runtime_warning_summaries(
        self,
        events: Sequence[Mapping[str, Any]],
    ) -> list[dict[str, Any]]:
        summaries: list[dict[str, Any]] = []
        for event in events:
            if event.get("eventType") != "runtime.warning":
                continue
            payload = event.get("payload")
            if not isinstance(payload, dict):
                continue
            summary = self._event_summary(event)
            message = self._redacted_message_summary(payload.get("message"))
            if message is not None:
                summary["message"] = message
            detail = payload.get("detail")
            if isinstance(detail, dict):
                safe_detail = {
                    key: detail.get(key)
                    for key in (
                        "sourceEventType",
                        "reasonCode",
                        "attemptedStrategy",
                        "selectedStrategy",
                        "outcome",
                    )
                    if isinstance(detail.get(key), (str, bool, int, float))
                }
                if safe_detail:
                    summary["detail"] = safe_detail
            summaries.append(summary)
        return summaries[-4:]

    def _provider_item_summaries(
        self,
        events: Sequence[Mapping[str, Any]],
    ) -> list[dict[str, Any]]:
        summaries: list[dict[str, Any]] = []
        for event in events:
            if event.get("eventType") not in {"item.started", "item.updated", "item.completed"}:
                continue
            payload = event.get("payload")
            if not isinstance(payload, dict):
                continue
            summary = self._event_summary(event)
            for key in ("itemType", "status"):
                value = payload.get(key)
                if isinstance(value, str) and value:
                    summary[key] = value
            title = self._redacted_message_summary(payload.get("title"))
            if title is not None:
                summary["title"] = title
            summaries.append(summary)
        return summaries[-8:]

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
    fixture_soak = report.get("mode") == "fixture-soak"
    fixture_concurrency = report.get("mode") == "fixture-concurrency"
    fixture_load = report.get("mode") == "fixture-load"
    fixture_load_failure = report.get("mode") == "fixture-load-failure"
    fixture_retention_concurrency = report.get("mode") == "fixture-retention-concurrency"
    configuration = report.get("configuration")
    failure_matrix = configuration.get("failureMatrix") if isinstance(configuration, dict) else None
    real_provider = configuration.get("realProvider") if isinstance(configuration, dict) else None
    soak = configuration.get("soak") if isinstance(configuration, dict) else None
    concurrency = configuration.get("concurrency") if isinstance(configuration, dict) else None
    load = configuration.get("load") if isinstance(configuration, dict) else None
    load_failure = configuration.get("loadFailure") if isinstance(configuration, dict) else None
    retention_concurrency = (
        configuration.get("retentionConcurrency") if isinstance(configuration, dict) else None
    )
    real_provider_boundary = (
        real_provider.get("boundary")
        if isinstance(real_provider, dict) and isinstance(real_provider.get("boundary"), str)
        else None
    )
    lines = [
        (
            "# Stage 3 Real Provider Smoke Acceptance"
            if real_provider_smoke
            else "# Stage 3 Provider Fixture Retention Concurrency Acceptance"
            if fixture_retention_concurrency
            else "# Stage 3 Provider Fixture Load Failure Acceptance"
            if fixture_load_failure
            else "# Stage 3 Provider Fixture Load Acceptance"
            if fixture_load
            else "# Stage 3 Provider Fixture Concurrency Acceptance"
            if fixture_concurrency
            else "# Stage 3 Provider Fixture Soak Acceptance"
            if fixture_soak
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
            real_provider_boundary
            or "This report runs a real Codex App Server or Claude Agent SDK Provider through the real Control "
            "Plane, selected Target, agentd, Worker Protocol, Provider Host, Control Plane restart, and a "
            "native-cursor second Turn. It is a narrow two-Turn smoke, not the complete Local or four-Target "
            "Release Gate."
            if real_provider_smoke
            else str(retention_concurrency.get("boundary"))
            if fixture_retention_concurrency
            and isinstance(retention_concurrency, dict)
            and isinstance(retention_concurrency.get("boundary"), str)
            else str(load.get("boundary"))
            if (fixture_load or fixture_load_failure)
            and isinstance(load, dict)
            and isinstance(load.get("boundary"), str)
            else str(concurrency.get("boundary"))
            if fixture_concurrency
            and isinstance(concurrency, dict)
            and isinstance(concurrency.get("boundary"), str)
            else str(soak.get("boundary"))
            if fixture_soak and isinstance(soak, dict) and isinstance(soak.get("boundary"), str)
            else (
                "This report uses the deterministic Provider Host fixture through the real Control Plane, "
                "agentd, Worker Protocol, and selected Target lifecycle. It is not a real Codex App Server or "
                "Claude Agent SDK release gate."
            )
        ),
    ]
    if fixture_soak and isinstance(soak, dict):
        lines.extend(
            [
                "",
                "## Requested fixture soak",
                "",
                "```json",
                json.dumps(soak, indent=2, sort_keys=True, ensure_ascii=False),
                "```",
            ]
        )
    if fixture_concurrency and isinstance(concurrency, dict):
        lines.extend(
            [
                "",
                "## Requested fixture concurrency",
                "",
                "```json",
                json.dumps(concurrency, indent=2, sort_keys=True, ensure_ascii=False),
                "```",
            ]
        )
    if fixture_load_failure and isinstance(load_failure, dict):
        lines.extend(
            [
                "",
                "## Requested fixture load failure",
                "",
                "```json",
                json.dumps(load_failure, indent=2, sort_keys=True, ensure_ascii=False),
                "```",
            ]
        )
    if (fixture_load or fixture_load_failure) and isinstance(load, dict):
        lines.extend(
            [
                "",
                "## Requested fixture load",
                "",
                "```json",
                json.dumps(load, indent=2, sort_keys=True, ensure_ascii=False),
                "```",
            ]
        )
    if fixture_retention_concurrency and isinstance(retention_concurrency, dict):
        lines.extend(
            [
                "",
                "## Requested fixture retention concurrency",
                "",
                "```json",
                json.dumps(retention_concurrency, indent=2, sort_keys=True, ensure_ascii=False),
                "```",
            ]
        )
    if isinstance(real_provider, dict) and (
        real_provider.get("requestedCases") or real_provider.get("requestedFailureCases")
    ):
        lines.extend(
            [
                "",
                "## Requested real Provider cases",
                "",
                "```json",
                json.dumps(real_provider, indent=2, sort_keys=True, ensure_ascii=False),
                "```",
            ]
        )
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


def parse_environment_variable_name(raw: str | None, option: str) -> str | None:
    if raw is None:
        return None
    value = raw.strip()
    if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", value):
        raise ValueError(f"{option} must be a valid environment variable name")
    return value


def parse_provider_model(raw: str | None, option: str) -> str | None:
    if raw is None:
        return None
    value = raw.strip()
    if (
        not value
        or len(value) > 256
        or any(character.isspace() or ord(character) < 32 for character in value)
    ):
        raise ValueError(f"{option} must be one non-empty model identifier without whitespace")
    return value


def parse_https_origin(raw: str | None, option: str) -> str | None:
    if raw is None:
        return None
    value = raw.strip()
    parsed = urllib.parse.urlsplit(value)
    if (
        parsed.scheme != "https"
        or not parsed.hostname
        or parsed.username is not None
        or parsed.password is not None
        or parsed.path not in {"", "/"}
        or parsed.query
        or parsed.fragment
    ):
        raise ValueError(f"{option} must be a credential-free HTTPS origin")
    try:
        port = parsed.port
    except ValueError:
        raise ValueError(f"{option} contains an invalid port") from None
    host = f"[{parsed.hostname}]" if ":" in parsed.hostname else parsed.hostname
    return f"https://{host}{f':{port}' if port is not None else ''}"


def resolve_repository_external_file(
    raw: pathlib.Path,
    repo_root: pathlib.Path,
    option: str,
    *,
    private: bool,
) -> pathlib.Path:
    expanded = raw.expanduser()
    if not expanded.is_absolute():
        raise ValueError(f"{option} must be absolute")
    repo_root_resolved = repo_root.resolve()
    lexical = pathlib.Path(os.path.abspath(expanded))
    resolved = expanded.resolve()
    if (
        lexical == repo_root_resolved
        or repo_root_resolved in lexical.parents
        or resolved == repo_root_resolved
        or repo_root_resolved in resolved.parents
    ):
        raise ValueError(f"{option} must be outside the repository")
    if not resolved.is_file():
        raise ValueError(f"{option} must reference an existing regular file")
    if private and stat.S_IMODE(resolved.stat().st_mode) & (stat.S_IRWXG | stat.S_IRWXO):
        raise ValueError(f"{option} must not be accessible by group or other users")
    return resolved


class EnvironmentValueError(ValueError):
    def __init__(self, reason: str, message: str) -> None:
        self.reason = reason
        super().__init__(message)


def read_environment_value(
    environment_name: str,
    description: str,
    *,
    maximum_length: int,
    forbidden_characters: str,
) -> str:
    value = os.environ.get(environment_name)
    if value is None or not value.strip():
        raise EnvironmentValueError(
            "missing",
            f"The configured {description} environment variable was missing or empty.",
        )
    if len(value) > maximum_length or any(character in value for character in forbidden_characters):
        raise EnvironmentValueError(
            "invalid",
            f"The configured {description} environment value was invalid.",
        )
    return value


def parse_args(argv: Sequence[str]) -> RunnerOptions:
    repo_root = pathlib.Path(__file__).resolve().parents[2]
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--target", default="local", choices=("local", "docker", "ssh", "kubernetes"))
    parser.add_argument("--provider", default="codex", choices=PROVIDERS)
    parser.add_argument(
        "--suite",
        default="fixture",
        choices=SUITES,
        help=(
            "Acceptance suite: deterministic fixture, deterministic long-session soak, managed Docker "
            "multi-Provider concurrency, bounded Docker load/admission, targeted Docker failure under load, "
            "Local retention concurrency, or real Provider smoke"
        ),
    )
    parser.add_argument("--output-dir", type=pathlib.Path)
    parser.add_argument(
        "--timeout",
        type=float,
        help="Overall timeout in seconds (default: Local 180, SSH/Docker 900, Kubernetes 1200)",
    )
    parser.add_argument("--runner-command-json", help="Provider Host command as a JSON string array")
    parser.add_argument(
        "--real-provider-credential-env",
        help="Environment variable containing the real Provider secret; the name and value are not persisted",
    )
    parser.add_argument(
        "--real-provider-credential-field",
        choices=REAL_PROVIDER_CREDENTIAL_FIELDS,
        default="apiKey",
        help="Credential payload field populated from --real-provider-credential-env",
    )
    parser.add_argument(
        "--real-provider-base-url-env",
        help="Optional environment variable containing the controlled Provider Base URL",
    )
    parser.add_argument(
        "--real-provider-model",
        help="Explicit non-secret model identifier used by the controlled real Provider profile",
    )
    parser.add_argument(
        "--real-provider-model-env",
        help=(
            "Environment variable containing the controlled real Provider model identifier; its "
            "name is not persisted, while the resolved non-secret model may appear in reports"
        ),
    )
    parser.add_argument(
        "--real-provider-case",
        action="append",
        choices=REAL_PROVIDER_CASES,
        default=[],
        help="Add a real Provider product-path case to real-provider-smoke; repeat to select multiple cases",
    )
    parser.add_argument(
        "--real-provider-matrix",
        action="store_true",
        help="Run every implemented real Provider product-path case in its canonical restart position",
    )
    parser.add_argument(
        "--real-provider-failure-case",
        action="append",
        choices=REAL_PROVIDER_FAILURE_CASES,
        default=[],
        help="Add a real Provider failure/recovery case; repeat to select multiple cases",
    )
    parser.add_argument(
        "--real-provider-failure-matrix",
        action="store_true",
        help="Run the complete real Provider Local authentication/rate-limit/crash/Cursor-expiry matrix",
    )
    parser.add_argument("--skip-build", action="store_true")
    parser.add_argument("--control-plane-binary", type=pathlib.Path)
    parser.add_argument("--keep", action="store_true", help="Keep SQLite, workspace, cache, and built binary")
    parser.add_argument("--ssh-orbctl-bin", default="orbctl")
    parser.add_argument("--ssh-machine-name", help="Owned disposable OrbStack machine name")
    parser.add_argument("--ssh-machine-arch", choices=("arm64", "amd64"), default="arm64")
    parser.add_argument("--ssh-machine-image", default="ubuntu:24.04")
    parser.add_argument("--ssh-node-version", default="24.13.1")
    parser.add_argument("--ssh-external-host", help="Explicit non-disposable SSH host")
    parser.add_argument("--ssh-external-port", type=int, default=22)
    parser.add_argument("--ssh-external-user", help="SSH login user for --ssh-external-host")
    parser.add_argument(
        "--ssh-external-identity-file",
        type=pathlib.Path,
        help="Absolute repository-external OpenSSH private-key file",
    )
    parser.add_argument(
        "--ssh-external-host-key-file",
        type=pathlib.Path,
        help="Absolute repository-external file containing exactly one pinned SSH Host Key",
    )
    parser.add_argument("--ssh-external-service-user", default=SSH_SERVICE_USER)
    parser.add_argument("--ssh-external-use-sudo", action="store_true")
    parser.add_argument(
        "--ssh-allow-external-host",
        action="store_true",
        help="Authorize scoped installation and cleanup on the explicit non-disposable SSH host",
    )
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
    parser.add_argument(
        "--kubernetes-api-server",
        help="Override the reused Context API server with a credential-free HTTPS origin",
    )
    parser.add_argument(
        "--kubernetes-tls-server-name",
        help="TLS certificate server name used with --kubernetes-api-server",
    )
    parser.add_argument("--kubernetes-allow-nondisposable", action="store_true")
    parser.add_argument(
        "--kubernetes-shared-local-image-store",
        action="store_true",
        help=(
            "Allow a reused non-Kind context to consume and clean the runner-built local Docker image with "
            "imagePullPolicy=Never"
        ),
    )
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
        "--kind-worker-nodes",
        type=int,
        default=0,
        help="Worker Node count for an owned Kind cluster; use at least 2 for the PDB multi-node drain case",
    )
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
        "--worker-lease-ttl",
        default=DEFAULT_ACCEPTANCE_WORKER_LEASE_TTL,
        help=(
            "Acceptance Control Plane worker Lease TTL duration literal "
            f"(default: {DEFAULT_ACCEPTANCE_WORKER_LEASE_TTL})"
        ),
    )
    parser.add_argument(
        "--worker-heartbeat-timeout",
        default=DEFAULT_ACCEPTANCE_WORKER_HEARTBEAT_TIMEOUT,
        help=(
            "Acceptance Control Plane worker heartbeat timeout duration literal "
            f"(default: {DEFAULT_ACCEPTANCE_WORKER_HEARTBEAT_TIMEOUT})"
        ),
    )
    parser.add_argument(
        "--no-restart-control-plane",
        action="store_true",
        help="Run the second Turn without restarting the Control Plane",
    )
    parser.add_argument(
        "--soak-turns",
        type=int,
        help="Additional fixture-soak Turns (default: 100; allowed: 10..1000)",
    )
    parser.add_argument(
        "--soak-restart-every",
        type=int,
        help="Restart the Control Plane after each N completed soak Turns (default: 10; 0 disables)",
    )
    parser.add_argument(
        "--load-waves",
        type=int,
        help=(
            "Four-Session load waves for fixture-load or fixture-load-failure (default: 25; allowed: "
            f"{FIXTURE_LOAD_MIN_WAVES}..{FIXTURE_LOAD_MAX_WAVES})"
        ),
    )
    parser.add_argument(
        "--load-min-duration-seconds",
        type=float,
        help=(
            "Minimum measured load-case duration for fixture-load or fixture-load-failure; the runner adds "
            "whole waves until both this duration and --load-waves are satisfied"
        ),
    )
    parser.add_argument(
        "--load-max-waves",
        type=int,
        help=(
            "Safety bound used with --load-min-duration-seconds (default: 100; maximum: "
            f"{FIXTURE_LOAD_DURATION_MAX_WAVES})"
        ),
    )
    parsed = parser.parse_args(argv)
    soak_turns = parsed.soak_turns if parsed.soak_turns is not None else 100 if parsed.suite == "fixture-soak" else 0
    soak_restart_every = (
        parsed.soak_restart_every
        if parsed.soak_restart_every is not None
        else 10
        if parsed.suite == "fixture-soak"
        else 0
    )
    load_waves = (
        parsed.load_waves
        if parsed.load_waves is not None
        else FIXTURE_LOAD_DEFAULT_WAVES
        if parsed.suite in FIXTURE_LOAD_SUITES
        else 0
    )
    load_min_duration_seconds = (
        parsed.load_min_duration_seconds
        if parsed.load_min_duration_seconds is not None
        else 0.0
    )
    load_max_waves = (
        parsed.load_max_waves
        if parsed.load_max_waves is not None
        else max(load_waves, FIXTURE_LOAD_MAX_WAVES)
        if parsed.suite in FIXTURE_LOAD_SUITES and load_min_duration_seconds > 0
        else load_waves
    )
    base_default_timeout = (
        1200.0
        if parsed.target == "kubernetes"
        else 900.0
        if parsed.target in {"docker", "ssh"} or parsed.suite == "fixture-soak"
        else 420.0
        if parsed.real_provider_failure_case or parsed.real_provider_failure_matrix
        else 180.0
    )
    default_timeout = (
        max(base_default_timeout, load_min_duration_seconds + 300.0)
        if parsed.suite in FIXTURE_LOAD_SUITES and load_min_duration_seconds > 0
        else base_default_timeout
    )
    worker_lease_ttl = parsed.worker_lease_ttl.strip()
    worker_heartbeat_timeout = parsed.worker_heartbeat_timeout.strip()
    duration_match = re.compile(r"([1-9][0-9]*)(ms|s|m|h)$")

    def duration_seconds(value: str, option: str) -> float:
        match = duration_match.fullmatch(value)
        if match is None:
            parser.error(f"{option} must be a valid duration like 250ms, 6s, 5m, or 1h")
        amount = int(match.group(1))
        unit = match.group(2)
        multiplier = {"ms": 0.001, "s": 1.0, "m": 60.0, "h": 3600.0}[unit]
        return amount * multiplier

    worker_lease_ttl_seconds = duration_seconds(worker_lease_ttl, "--worker-lease-ttl")
    worker_heartbeat_timeout_seconds = duration_seconds(
        worker_heartbeat_timeout,
        "--worker-heartbeat-timeout",
    )
    if worker_heartbeat_timeout_seconds <= worker_lease_ttl_seconds:
        parser.error("--worker-heartbeat-timeout must be greater than --worker-lease-ttl")
    if (
        parsed.suite != "real-provider-smoke"
        and (
            worker_lease_ttl != DEFAULT_ACCEPTANCE_WORKER_LEASE_TTL
            or worker_heartbeat_timeout != DEFAULT_ACCEPTANCE_WORKER_HEARTBEAT_TIMEOUT
        )
    ):
        parser.error("custom worker lease timing is only supported by --suite real-provider-smoke")
    timeout_seconds = parsed.timeout if parsed.timeout is not None else default_timeout
    if timeout_seconds <= 0:
        parser.error("--timeout must be positive")
    if parsed.suite != "fixture-soak" and (
        parsed.soak_turns is not None or parsed.soak_restart_every is not None
    ):
        parser.error("--soak-turns and --soak-restart-every require --suite fixture-soak")
    if parsed.suite == "fixture-soak":
        if not 10 <= soak_turns <= 1000:
            parser.error("--soak-turns must be between 10 and 1000")
        if soak_restart_every < 0 or soak_restart_every >= soak_turns:
            parser.error("--soak-restart-every must be 0 or less than --soak-turns")
        if parsed.no_restart_control_plane:
            parser.error("--suite fixture-soak requires Control Plane restart continuity")
        if parsed.failure_only:
            parser.error("--suite fixture-soak cannot be combined with --failure-only")
    if parsed.suite not in FIXTURE_LOAD_SUITES and (
        parsed.load_waves is not None
        or parsed.load_min_duration_seconds is not None
        or parsed.load_max_waves is not None
    ):
        parser.error(
            "load wave and duration options require --suite fixture-load or fixture-load-failure"
        )
    if parsed.suite in FIXTURE_LOAD_SUITES and not (
        FIXTURE_LOAD_MIN_WAVES <= load_waves <= FIXTURE_LOAD_MAX_WAVES
    ):
        parser.error(
            f"--load-waves must be between {FIXTURE_LOAD_MIN_WAVES} and {FIXTURE_LOAD_MAX_WAVES}"
        )
    if parsed.suite in FIXTURE_LOAD_SUITES:
        if not 0 <= load_min_duration_seconds <= FIXTURE_LOAD_DURATION_MAX_SECONDS:
            parser.error(
                "--load-min-duration-seconds must be between 0 and "
                f"{int(FIXTURE_LOAD_DURATION_MAX_SECONDS)}"
            )
        if parsed.load_max_waves is not None and load_min_duration_seconds <= 0:
            parser.error(
                "--load-max-waves requires a positive --load-min-duration-seconds"
            )
        if not load_waves <= load_max_waves <= FIXTURE_LOAD_DURATION_MAX_WAVES:
            parser.error(
                "--load-max-waves must be at least --load-waves and no greater than "
                f"{FIXTURE_LOAD_DURATION_MAX_WAVES}"
            )
        if load_min_duration_seconds > 0 and timeout_seconds < load_min_duration_seconds + 60.0:
            parser.error(
                "--timeout must allow at least 60 seconds beyond --load-min-duration-seconds for setup and cleanup"
            )
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
    if parsed.kubernetes_api_server is not None and not parsed.kubernetes_context:
        parser.error("--kubernetes-api-server requires --kubernetes-context")
    if parsed.kubernetes_tls_server_name is not None and parsed.kubernetes_api_server is None:
        parser.error("--kubernetes-tls-server-name requires --kubernetes-api-server")
    if parsed.kubernetes_shared_local_image_store:
        if not parsed.kubernetes_context:
            parser.error("--kubernetes-shared-local-image-store requires --kubernetes-context")
        if not parsed.kubernetes_allow_nondisposable:
            parser.error(
                "--kubernetes-shared-local-image-store requires --kubernetes-allow-nondisposable"
            )
        if parsed.kubernetes_skip_worker_build or parsed.kubernetes_worker_image:
            parser.error(
                "--kubernetes-shared-local-image-store cannot be combined with an operator-provided Worker image"
            )
    if parsed.kind_cluster_name and parsed.kubernetes_context:
        parser.error("--kind-cluster-name cannot be combined with --kubernetes-context")
    if parsed.kind_worker_nodes and parsed.target != "kubernetes":
        parser.error("--kind-worker-nodes requires --target kubernetes")
    if parsed.kind_worker_nodes and parsed.kubernetes_context:
        parser.error("--kind-worker-nodes cannot be combined with --kubernetes-context")
    if parsed.kind_worker_nodes < 0 or parsed.kind_worker_nodes > 8:
        parser.error("--kind-worker-nodes must be between 0 and 8")
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
    external_option_used = bool(
        parsed.ssh_external_host
        or parsed.ssh_external_user
        or parsed.ssh_external_identity_file
        or parsed.ssh_external_host_key_file
        or parsed.ssh_allow_external_host
        or parsed.ssh_external_use_sudo
        or parsed.ssh_external_port != 22
        or parsed.ssh_external_service_user != SSH_SERVICE_USER
    )
    if external_option_used and parsed.target != "ssh":
        parser.error("SSH external-host options require --target ssh")
    ssh_external_host = parsed.ssh_external_host.strip() if parsed.ssh_external_host else None
    ssh_external_user = parsed.ssh_external_user.strip() if parsed.ssh_external_user else None
    ssh_external_service_user = parsed.ssh_external_service_user.strip()
    ssh_external_identity_file: pathlib.Path | None = None
    ssh_external_host_key_file: pathlib.Path | None = None
    if ssh_external_host is not None:
        if not parsed.ssh_allow_external_host:
            parser.error("--ssh-external-host requires --ssh-allow-external-host")
        if parsed.ssh_machine_name is not None:
            parser.error("--ssh-external-host cannot be combined with --ssh-machine-name")
        if (
            ssh_external_user is None
            or parsed.ssh_external_identity_file is None
            or parsed.ssh_external_host_key_file is None
        ):
            parser.error(
                "--ssh-external-host requires --ssh-external-user, --ssh-external-identity-file, "
                "and --ssh-external-host-key-file"
            )
        if (
            len(ssh_external_host) > 253
            or any(character in ssh_external_host for character in "\r\n\t\x00 /@")
        ):
            parser.error("--ssh-external-host must be one hostname or address")
        if parsed.ssh_external_port < 1 or parsed.ssh_external_port > 65535:
            parser.error("--ssh-external-port must be between 1 and 65535")
        if not re.fullmatch(r"[a-z_][a-z0-9_-]*[$]?", ssh_external_user):
            parser.error("--ssh-external-user must be a valid Unix user")
        if not re.fullmatch(r"[a-z_][a-z0-9_-]*[$]?", ssh_external_service_user):
            parser.error("--ssh-external-service-user must be a valid Unix user")
        if ssh_external_user != "root" and not parsed.ssh_external_use_sudo:
            parser.error("a non-root --ssh-external-user requires --ssh-external-use-sudo")
        try:
            ssh_external_identity_file = resolve_repository_external_file(
                parsed.ssh_external_identity_file,
                repo_root,
                "--ssh-external-identity-file",
                private=True,
            )
            ssh_external_host_key_file = resolve_repository_external_file(
                parsed.ssh_external_host_key_file,
                repo_root,
                "--ssh-external-host-key-file",
                private=False,
            )
        except ValueError as error:
            parser.error(str(error))
    elif external_option_used:
        parser.error("SSH external-host options require --ssh-external-host")
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
    try:
        kubernetes_api_server = parse_https_origin(
            parsed.kubernetes_api_server,
            "--kubernetes-api-server",
        )
    except ValueError as error:
        parser.error(str(error))
    kubernetes_tls_server_name = (
        parsed.kubernetes_tls_server_name.strip()
        if parsed.kubernetes_tls_server_name is not None
        else None
    )
    if kubernetes_tls_server_name is not None and (
        not kubernetes_tls_server_name
        or len(kubernetes_tls_server_name) > 253
        or any(character in kubernetes_tls_server_name for character in "\r\n\t\x00 /@:")
    ):
        parser.error("--kubernetes-tls-server-name must be one DNS name without scheme or port")
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
    try:
        real_provider_credential_env = parse_environment_variable_name(
            parsed.real_provider_credential_env,
            "--real-provider-credential-env",
        )
        real_provider_base_url_env = parse_environment_variable_name(
            parsed.real_provider_base_url_env,
            "--real-provider-base-url-env",
        )
        real_provider_model_env = parse_environment_variable_name(
            parsed.real_provider_model_env,
            "--real-provider-model-env",
        )
        real_provider_model = parse_provider_model(
            parsed.real_provider_model,
            "--real-provider-model",
        )
    except ValueError as error:
        parser.error(str(error))
    if real_provider_model is not None and real_provider_model_env is not None:
        parser.error("--real-provider-model and --real-provider-model-env are mutually exclusive")
    run_id = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    output_dir = parsed.output_dir or repo_root / ".tmp" / "stage3-provider-acceptance-results" / run_id
    kubernetes_kubeconfig = parsed.kubernetes_kubeconfig.expanduser().resolve() if parsed.kubernetes_kubeconfig else None
    requested_failure_cases = list(parsed.failure_case)
    if parsed.failure_matrix:
        requested_failure_cases.extend(TARGET_FAILURE_CASES[parsed.target])
    requested_failure_case_set = set(requested_failure_cases)
    failure_cases = tuple(case for case in FAILURE_CASES if case in requested_failure_case_set)
    if (
        parsed.target == "kubernetes"
        and parsed.kubernetes_context is None
        and "kubernetes-pdb-drain" in failure_cases
        and parsed.kind_worker_nodes < 2
    ):
        parser.error(
            "owned Kind kubernetes-pdb-drain requires --kind-worker-nodes 2 or greater"
        )
    requested_real_provider_cases = list(parsed.real_provider_case)
    if parsed.real_provider_matrix:
        requested_real_provider_cases.extend(REAL_PROVIDER_CASES)
    requested_real_provider_case_set = set(requested_real_provider_cases)
    real_provider_cases = tuple(
        case for case in REAL_PROVIDER_CASES if case in requested_real_provider_case_set
    )
    requested_real_provider_failure_cases = list(parsed.real_provider_failure_case)
    if parsed.real_provider_failure_matrix:
        requested_real_provider_failure_cases.extend(REAL_PROVIDER_FAILURE_CASES)
    requested_real_provider_failure_case_set = set(requested_real_provider_failure_cases)
    real_provider_failure_cases = tuple(
        case
        for case in REAL_PROVIDER_FAILURE_CASES
        if case in requested_real_provider_failure_case_set
    )
    if parsed.failure_only and not failure_cases:
        parser.error("--failure-only requires --failure-matrix or at least one --failure-case")
    if parsed.suite == "fixture-concurrency":
        if parsed.target != "docker":
            parser.error("--suite fixture-concurrency currently requires --target docker")
        if failure_cases or parsed.failure_only:
            parser.error(
                "--suite fixture-concurrency cannot be combined with fixture failure/canary options"
            )
    if parsed.suite == "fixture-load":
        if parsed.target != "docker":
            parser.error("--suite fixture-load currently requires --target docker")
        if failure_cases or parsed.failure_only:
            parser.error(
                "--suite fixture-load cannot be combined with fixture failure/canary options"
            )
    if parsed.suite == "fixture-load-failure":
        if parsed.target != "docker":
            parser.error("--suite fixture-load-failure currently requires --target docker")
        if failure_cases or parsed.failure_only:
            parser.error(
                f"--suite {parsed.suite} cannot be combined with fixture failure/canary options"
            )
    if parsed.suite == "fixture-retention-concurrency":
        if parsed.target != "local":
            parser.error("--suite fixture-retention-concurrency requires --target local")
        if failure_cases or parsed.failure_only:
            parser.error(
                "--suite fixture-retention-concurrency cannot be combined with fixture failure/canary options"
            )
    if parsed.suite == "real-provider-smoke":
        if parsed.runner_command_json is None:
            parser.error("--suite real-provider-smoke requires an explicit --runner-command-json")
        if failure_cases or parsed.failure_only:
            parser.error(
                "--suite real-provider-smoke cannot be combined with fixture failure/canary options"
            )
        if real_provider_cases and real_provider_failure_cases:
            parser.error(
                "real Provider product-path cases and failure cases require separate canonical runs"
            )
        if parsed.target != "local" and real_provider_credential_env is None:
            parser.error(
                "remote real Provider acceptance requires --real-provider-credential-env"
            )
        if real_provider_base_url_env is not None and real_provider_credential_env is None:
            parser.error(
                "--real-provider-base-url-env requires --real-provider-credential-env"
            )
        if parsed.real_provider_credential_field == "authToken" and parsed.provider != "claudeAgent":
            parser.error("--real-provider-credential-field authToken is supported only for claudeAgent")
    elif real_provider_cases or real_provider_failure_cases:
        parser.error(
            "real Provider case options require --suite real-provider-smoke"
        )
    elif real_provider_credential_env is not None or real_provider_base_url_env is not None:
        parser.error("real Provider Credential options require --suite real-provider-smoke")
    elif real_provider_model is not None:
        parser.error("--real-provider-model requires --suite real-provider-smoke")
    elif real_provider_model_env is not None:
        parser.error("--real-provider-model-env requires --suite real-provider-smoke")
    try:
        if real_provider_credential_env is not None:
            read_environment_value(
                real_provider_credential_env,
                "real Provider Credential",
                maximum_length=64 << 10,
                forbidden_characters="\r\n\x00",
            )
        if real_provider_base_url_env is not None:
            read_environment_value(
                real_provider_base_url_env,
                "real Provider Base URL",
                maximum_length=2048,
                forbidden_characters="\r\n\t\x00",
            )
        if real_provider_model_env is not None:
            real_provider_model = parse_provider_model(
                read_environment_value(
                    real_provider_model_env,
                    "real Provider model",
                    maximum_length=256,
                    forbidden_characters="\r\n\t\x00",
                ),
                "--real-provider-model-env",
            )
    except ValueError as error:
        parser.error(str(error))
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
        restart_control_plane=(
            not parsed.no_restart_control_plane
            and parsed.suite
            not in {
                "fixture-concurrency",
                "fixture-load",
                "fixture-load-failure",
                "fixture-retention-concurrency",
            }
        ),
        soak_turns=soak_turns,
        soak_restart_every=soak_restart_every,
        load_waves=load_waves,
        load_min_duration_seconds=load_min_duration_seconds,
        load_max_waves=load_max_waves,
        worker_lease_ttl=worker_lease_ttl,
        worker_heartbeat_timeout=worker_heartbeat_timeout,
        ssh_orbctl_bin=parsed.ssh_orbctl_bin.strip(),
        ssh_machine_name=ssh_machine_name,
        ssh_machine_arch=parsed.ssh_machine_arch,
        ssh_machine_image=parsed.ssh_machine_image.strip(),
        ssh_node_version=parsed.ssh_node_version.strip(),
        ssh_external_host=ssh_external_host,
        ssh_external_port=parsed.ssh_external_port,
        ssh_external_user=ssh_external_user,
        ssh_external_identity_file=ssh_external_identity_file,
        ssh_external_host_key_file=ssh_external_host_key_file,
        ssh_external_service_user=ssh_external_service_user,
        ssh_external_use_sudo=parsed.ssh_external_use_sudo,
        ssh_allow_external_host=parsed.ssh_allow_external_host,
        docker_socket_path=docker_socket_path,
        docker_worker_image=parsed.docker_worker_image,
        docker_skip_worker_build=parsed.docker_skip_worker_build,
        docker_control_plane_host=parsed.docker_control_plane_host.strip(),
        docker_network_mode=parsed.docker_network_mode,
        docker_memory_bytes=parsed.docker_memory_bytes,
        docker_nano_cpus=parsed.docker_nano_cpus,
        kubernetes_context=kubernetes_context,
        kubernetes_kubeconfig=kubernetes_kubeconfig,
        kubernetes_api_server=kubernetes_api_server,
        kubernetes_tls_server_name=kubernetes_tls_server_name,
        kubernetes_allow_nondisposable=parsed.kubernetes_allow_nondisposable,
        kubernetes_shared_local_image_store=parsed.kubernetes_shared_local_image_store,
        kubernetes_worker_image=parsed.kubernetes_worker_image,
        kubernetes_skip_worker_build=parsed.kubernetes_skip_worker_build,
        kubernetes_control_plane_host=parsed.kubernetes_control_plane_host.strip(),
        kind_bin=parsed.kind_bin.strip(),
        kind_cluster_name=kind_cluster_name,
        kind_node_image=parsed.kind_node_image.strip(),
        kind_worker_nodes=parsed.kind_worker_nodes,
        failure_cases=failure_cases,
        network_outage_seconds=parsed.network_outage_seconds,
        docker_allow_network_interruption=parsed.docker_allow_network_interruption,
        kubernetes_allow_node_drain=parsed.kubernetes_allow_node_drain,
        failure_only=parsed.failure_only,
        real_provider_cases=real_provider_cases,
        real_provider_failure_cases=real_provider_failure_cases,
        real_provider_credential_env=real_provider_credential_env,
        real_provider_credential_field=parsed.real_provider_credential_field,
        real_provider_base_url_env=real_provider_base_url_env,
        real_provider_model=real_provider_model,
    )


def main(argv: Sequence[str] | None = None) -> int:
    options = parse_args(argv if argv is not None else sys.argv[1:])
    repo_root = pathlib.Path(__file__).resolve().parents[2]
    options.output_dir.mkdir(parents=True, exist_ok=True)
    redactor = SecretRedactor()
    if options.real_provider_credential_env is not None:
        redactor.add(
            read_environment_value(
                options.real_provider_credential_env,
                "real Provider Credential",
                maximum_length=64 << 10,
                forbidden_characters="\r\n\x00",
            ),
            "[REDACTED_REAL_PROVIDER_CREDENTIAL]",
        )
    if options.real_provider_base_url_env is not None:
        redactor.add(
            read_environment_value(
                options.real_provider_base_url_env,
                "real Provider Base URL",
                maximum_length=2048,
                forbidden_characters="\r\n\t\x00",
            ).strip(),
            "[REDACTED_REAL_PROVIDER_BASE_URL]",
        )
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
    fixture_concurrency = options.suite == "fixture-concurrency"
    fixture_load = options.suite == "fixture-load"
    fixture_load_failure = options.suite == "fixture-load-failure"
    fixture_load_suite = options.suite in FIXTURE_LOAD_SUITES
    fixture_retention_concurrency = options.suite == "fixture-retention-concurrency"
    mode = (
        "real-provider-smoke"
        if real_provider_smoke
        else "fixture-retention-concurrency"
        if fixture_retention_concurrency
        else "fixture-load-failure"
        if fixture_load_failure
        else "fixture-load"
        if fixture_load
        else "fixture-concurrency"
        if fixture_concurrency
        else "fixture-soak"
        if options.suite == "fixture-soak"
        else "fixture+failure-matrix"
        if options.failure_cases
        else "fixture"
    )
    evidence_boundary = (
        (
            "real Codex/Claude through the real Control Plane, Local agentd, Worker Protocol and Provider Host; "
            "runner-owned 401/429 endpoints, scoped Host crash, new-Execution recovery and audited Cursor-expiry "
            "history reconstruction; Local failure evidence only, not a four-Target Release Gate"
            if options.real_provider_failure_cases
            else "real Codex/Claude through the real Control Plane, selected Target, agentd, Worker Protocol, "
            "Provider Host, Control Plane restart, and native-cursor second Turn; narrow smoke only, not a "
            "complete Local or four-Target Release Gate"
        )
        if real_provider_smoke
        else (
            "deterministic Provider Host fixture over the real Local Control Plane, Local agentd and background "
            "retention sweeper; runner-scoped metadata aging proves active Execution fencing, concurrent deletion "
            "of an unreferenced Artifact, protected seed/current Checkpoint lineages and post-terminal physical "
            "Workspace cleanup only, not real Provider, remote Target, load or production-duration retention"
            if fixture_retention_concurrency
            else (
                "deterministic Codex and Claude Provider Host fixtures over one real Control Plane and two managed "
                "Docker agentd Workers; four Sessions prove exact Execution-to-Worker-to-container network and "
                "container-loss targeting, managed replacement, exact Provider Host process crash classification, "
                "peer-Session isolation, Generation fencing or new-Execution recovery, unique terminal paths, then "
                "resource-profiled quota rejection, slot reuse, optional minimum-duration load and diagnostic "
                "latency/error measurements; deterministic single-host mechanics only, not real Provider, "
                "multi-host, multi-node, rollout failure or an operator-approved production SLA"
                if fixture_load_failure
                else (
                "deterministic Codex and Claude Provider Host fixtures over one real Control Plane and two managed "
                "Docker agentd Workers; four Sessions run repeated quota rejection, immediate slot reuse, three "
                "overlap observations per wave, durable Artifact/Checkpoint completion, optional minimum-duration "
                "load and diagnostic P50/P95/P99/error measurements; resource-profiled mechanics only, not real "
                "Provider, multi-node or an operator-approved production performance SLA"
                if fixture_load
                else (
                    "deterministic Codex and Claude Provider Host fixtures over one real Control Plane and two managed "
                    "Docker agentd Workers; simultaneous pending Approval barriers prove multi-Session execution "
                    "overlap, not real Provider, load, remote Target, or production concurrency"
                    if fixture_concurrency
                    else "deterministic Provider Host fixture over real Control Plane, agentd, Worker Protocol, and Target paths"
                )
                )
            )
        )
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
            "workerTiming": {
                "leaseTTL": options.worker_lease_ttl,
                "heartbeatTimeout": options.worker_heartbeat_timeout,
            },
            "soak": {
                "turns": options.soak_turns,
                "restartEvery": options.soak_restart_every,
                "boundary": (
                    "deterministic long-session, repeated Control Plane restart, event pagination and terminal "
                    "integrity, plus repeated Tool/Usage/Checkpoint mechanics only; not real Provider, multi-node, "
                    "retention-concurrency or production soak evidence"
                    if options.suite == "fixture-soak"
                    else None
                ),
            },
            "concurrency": {
                "workers": FIXTURE_CONCURRENCY_WORKERS if fixture_concurrency else 0,
                "sessions": 2 if fixture_concurrency else 0,
                "providers": list(FIXTURE_CONCURRENCY_PROVIDERS) if fixture_concurrency else [],
                "barrier": "simultaneous-pending-approval" if fixture_concurrency else None,
                "boundary": evidence_boundary if fixture_concurrency else None,
            },
            "load": {
                "workers": FIXTURE_CONCURRENCY_WORKERS if fixture_load_suite else 0,
                "sessions": FIXTURE_LOAD_SESSIONS if fixture_load_suite else 0,
                "waves": options.load_waves if fixture_load_suite else 0,
                "minimumDurationSeconds": (
                    options.load_min_duration_seconds if fixture_load_suite else 0
                ),
                "maximumWaves": options.load_max_waves if fixture_load_suite else 0,
                "plannedExecutions": (
                    FIXTURE_LOAD_SESSIONS * options.load_waves if fixture_load_suite else 0
                ),
                "plannedQuotaRejections": (
                    (FIXTURE_LOAD_SESSIONS - FIXTURE_CONCURRENCY_WORKERS)
                    * options.load_waves
                    if fixture_load_suite
                    else 0
                ),
                "providers": list(FIXTURE_CONCURRENCY_PROVIDERS) if fixture_load_suite else [],
                "maxConcurrentExecutions": (
                    FIXTURE_CONCURRENCY_WORKERS if fixture_load_suite else None
                ),
                "resourceProfile": (
                    fixture_load_resource_profile(options) if fixture_load_suite else None
                ),
                "measurement": {
                    "durationTargetEnforced": (
                        options.load_min_duration_seconds > 0
                        if fixture_load_suite
                        else False
                    ),
                    "latencyPercentiles": [50, 95, 99] if fixture_load_suite else [],
                    "unexpectedErrorRateRecorded": fixture_load_suite,
                    "operatorApprovedSlaThresholdsEnforced": False,
                },
                "boundary": evidence_boundary if fixture_load_suite else None,
            },
            "loadFailure": {
                "faults": list(FIXTURE_LOAD_FAILURE_CASES) if fixture_load_failure else [],
                "targeting": (
                    "agent_executions.worker_id -> worker_instances.pod_name -> exact managed container"
                    if fixture_load_failure
                    else None
                ),
                "networkOutageSeconds": (
                    options.network_outage_seconds if fixture_load_failure else 0
                ),
                "activeSessions": 2 if fixture_load_failure else 0,
                "quotaRejectedSessions": 2 if fixture_load_failure else 0,
                "faultCases": len(FIXTURE_LOAD_FAILURE_CASES) if fixture_load_failure else 0,
                "postRecoveryWaves": options.load_waves if fixture_load_failure else 0,
                "boundary": evidence_boundary if fixture_load_failure else None,
            },
            "retentionConcurrency": {
                "target": "local" if fixture_retention_concurrency else None,
                "sweepInterval": (
                    FIXTURE_RETENTION_SWEEP_INTERVAL
                    if fixture_retention_concurrency
                    else None
                ),
                "sessionArchiveAfterDays": (
                    FIXTURE_RETENTION_DAYS if fixture_retention_concurrency else None
                ),
                "artifactDeleteAfterDays": (
                    FIXTURE_RETENTION_DAYS if fixture_retention_concurrency else None
                ),
                "workspaceCleanupAfterDays": (
                    FIXTURE_RETENTION_DAYS if fixture_retention_concurrency else None
                ),
                "agedDays": (
                    FIXTURE_RETENTION_AGE_DAYS if fixture_retention_concurrency else None
                ),
                "barrier": (
                    "pending-approval-active-execution"
                    if fixture_retention_concurrency
                    else None
                ),
                "boundary": evidence_boundary if fixture_retention_concurrency else None,
            },
            "skipBuild": options.skip_build,
            "keepState": options.keep,
            "failureMatrix": {
                "requestedCases": list(options.failure_cases),
                "failureOnly": options.failure_only,
                "networkOutageSeconds": options.network_outage_seconds,
                "realProviderReleaseGate": False,
                "boundary": evidence_boundary,
            },
            "realProvider": {
                "requestedCases": list(options.real_provider_cases),
                "requestedFailureCases": list(options.real_provider_failure_cases),
                "ambientAuthentication": (
                    real_provider_smoke and options.real_provider_credential_env is None
                ),
                "controlledProductCredential": options.real_provider_credential_env is not None,
                "controlledProductCredentialField": (
                    options.real_provider_credential_field
                    if options.real_provider_credential_env is not None
                    else None
                ),
                "productCredentialEnvironmentNamePersisted": False,
                "controlledBaseUrl": options.real_provider_base_url_env is not None,
                "model": options.real_provider_model,
                "controlledFaultCredentials": bool(options.real_provider_failure_cases),
                "cursorMaximumAge": (
                    REAL_PROVIDER_CURSOR_MAX_AGE
                    if "cursor-expiry" in options.real_provider_failure_cases
                    else None
                ),
                "boundary": (
                    evidence_boundary
                    if options.real_provider_failure_cases
                    else "selected real Provider cases run in canonical pre/post-restart positions around the "
                    "two-Turn continuity baseline"
                    if options.real_provider_cases
                    else "two-Turn marker and native-cursor continuity baseline"
                ),
            },
            "runnerCommand": {
                "executable": pathlib.Path(options.runner_command[0]).name,
                "argumentCount": len(options.runner_command) - 1,
            },
            "ssh": {
                "runtime": (
                    "authorized-external-host"
                    if options.ssh_external_host is not None
                    else "owned-disposable-orbstack"
                ),
                **(
                    {
                        "orbctlBinary": options.ssh_orbctl_bin,
                        "machineName": options.ssh_machine_name or "generated-per-run",
                        "machineImage": options.ssh_machine_image,
                    }
                    if options.ssh_external_host is None
                    else {
                        "externalHostAuthorized": options.ssh_allow_external_host,
                        "externalHostAddressPersisted": False,
                        "operatorIdentitySourcePersisted": False,
                        "operatorHostKeySourcePersisted": False,
                    }
                ),
                "machineArch": options.ssh_machine_arch,
                "controlPlaneTransport": {
                    "mode": "reverse-ssh-loopback",
                    "description": SSH_RELAY_TRANSPORT,
                    "vmListenHost": SSH_RELAY_LOOPBACK_HOST,
                },
                "nodeVersion": options.ssh_node_version,
                "credentialSource": (
                    "repository-external operator identity"
                    if options.ssh_external_host is not None
                    else "runner-generated one-time Ed25519 key"
                ),
                "localPrivateKeyPlaintextDeletedAfterProvision": (
                    options.ssh_external_host is None
                ),
                "driverPrivateKeyReferenceClearedAfterProvision": (
                    options.ssh_external_host is not None
                ),
                "operatorIdentitySourcePreserved": options.ssh_external_host is not None,
                "controlPlaneCredentialLifecycle": (
                    SSH_EXTERNAL_CREDENTIAL_LIFECYCLE
                    if options.ssh_external_host is not None
                    else SSH_CREDENTIAL_LIFECYCLE
                ),
                "readsUserSSHConfiguration": False,
                "runtimeBuild": (
                    "real-provider-host-plus-locked-tools-per-run"
                    if real_provider_smoke
                    else "deterministic-fixture-cross-built-per-run"
                ),
                "cleanupSemantics": (
                    "ssh/revoke removes only the target-scoped systemd unit, environment, and agentd binary; the "
                    "Runner then removes only its ownership-marked external runtime and preserves the host"
                    if options.ssh_external_host is not None
                    else "ssh/revoke removes the systemd unit, environment, and agentd binary; deletion of the "
                    "owned OrbStack machine is infrastructure cleanup and does not prove product-level Workspace purge"
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
                "desiredWorkers": (
                    FIXTURE_CONCURRENCY_WORKERS
                    if fixture_concurrency or fixture_load_suite
                    else 1
                ),
                "allowOperatorNetworkInterruption": options.docker_allow_network_interruption,
            }
            if options.target == "docker"
            else None,
            "kubernetes": {
                "context": options.kubernetes_context or "owned-kind-cluster",
                "kubeconfig": str(options.kubernetes_kubeconfig) if options.kubernetes_kubeconfig else None,
                "apiServerOverride": options.kubernetes_api_server,
                "tlsServerNameOverride": options.kubernetes_tls_server_name,
                "allowNondisposable": options.kubernetes_allow_nondisposable,
                "sharedLocalImageStore": options.kubernetes_shared_local_image_store,
                "workerImage": options.kubernetes_worker_image,
                "skipWorkerBuild": options.kubernetes_skip_worker_build,
                "controlPlaneHost": options.kubernetes_control_plane_host,
                "kindBinary": options.kind_bin,
                "kindClusterName": options.kind_cluster_name,
                "kindNodeImage": options.kind_node_image,
                "kindWorkerNodes": options.kind_worker_nodes,
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
