#!/usr/bin/env python3
from __future__ import annotations

import json
import os
import pathlib
import re
import shlex
import signal
import subprocess
import sys
import tempfile
import textwrap
import time


SCRIPT_DIR = pathlib.Path(__file__).resolve().parent
REPO_ROOT = SCRIPT_DIR.parent.parent
CONTRACT_PATH = REPO_ROOT / "docs" / "contracts" / "kubernetes-resilience-acceptance-v1.md"


def fail(message: str) -> None:
    raise SystemExit(message)


def require_regex(text: str, pattern: str, label: str) -> re.Match[str]:
    match = re.search(pattern, text, re.MULTILINE)
    if match is None:
        fail(f"missing {label}")
    return match


def write_executable(path: pathlib.Path, text: str) -> None:
    path.write_text(textwrap.dedent(text).lstrip(), encoding="utf-8")
    path.chmod(0o755)


def make_fake_kubectl(path: pathlib.Path) -> None:
    write_executable(
        path,
        r"""
        #!/usr/bin/env python3
        import json
        import os
        import sys

        prehook_mode = os.environ.get("FAKE_MANAGED_PREHOOK_MODE", "")

        args = sys.argv[1:]
        namespace = None
        cleaned = []
        i = 0
        while i < len(args):
            if args[i] == "--context":
                i += 2
                continue
            if args[i] in ("-n", "--namespace"):
                namespace = args[i + 1]
                i += 2
                continue
            cleaned.append(args[i])
            i += 1

        control_plane_pods = [
            {
                "metadata": {"name": "synara-control-plane-a", "uid": "pod-uid-a", "deletionTimestamp": None},
                "spec": {"nodeName": "safe-worker"},
                "status": {
                    "phase": "Running",
                    "conditions": [{"type": "Ready", "status": "True"}],
                },
            },
            {
                "metadata": {"name": "synara-control-plane-b", "uid": "pod-uid-b", "deletionTimestamp": None},
                "spec": {"nodeName": "dep-worker"},
                "status": {
                    "phase": "Running",
                    "conditions": [{"type": "Ready", "status": "True"}],
                },
            },
        ]
        dependency_pods = [
            {
                "metadata": {
                    "name": "synara-stage2-postgres",
                    "labels": {"app.kubernetes.io/name": "synara-stage2-postgres"},
                },
                "spec": {"nodeName": "dep-worker"},
            },
            {
                "metadata": {
                    "name": "synara-stage2-minio",
                    "labels": {"app.kubernetes.io/name": "synara-stage2-minio"},
                },
                "spec": {"nodeName": "dep-worker"},
            },
        ]
        node_payload = {
            "metadata": {"name": "safe-worker", "uid": "node-uid-safe-worker"},
            "status": {
                "conditions": [
                    {
                        "type": "Ready",
                        "status": "True",
                        "reason": "KubeletReady",
                        "message": "fake node ready",
                    }
                ]
            },
        }
        if prehook_mode == "uid-missing":
            node_payload["metadata"].pop("uid", None)
        if prehook_mode == "malformed-target":
            control_plane_pods[0]["metadata"]["uid"] = {"rawTarget": "safe-worker-secret"}
        if prehook_mode == "empty-allowlist-skip":
            control_plane_pods[0]["spec"]["nodeName"] = {"rawTarget": "safe-worker-secret"}
        ready_payload = {
            "status": "ready",
            "checks": {"database": {"status": "ready"}, "schema": {"status": "ready"}},
        }

        if cleaned == ["cluster-info"]:
            sys.stdout.write("Kubernetes control plane is running\n")
            raise SystemExit(0)
        if cleaned[:2] == ["get", "--raw"]:
            sys.stdout.write(json.dumps(ready_payload))
            raise SystemExit(0)
        if cleaned[:2] == ["rollout", "status"]:
            raise SystemExit(0)
        if cleaned and cleaned[0] == "wait" and "--for=condition=Ready" in cleaned:
            raise SystemExit(0)
        if cleaned[:2] == ["get", "node/safe-worker"] and cleaned[-2:] == ["-o", "json"]:
            if prehook_mode == "get-node-failed":
                raise SystemExit(1)
            sys.stdout.write(json.dumps(node_payload))
            raise SystemExit(0)
        if cleaned[:2] == ["get", "pods"] and cleaned[-2:] == ["-o", "json"]:
            if "-l" in cleaned:
                sys.stdout.write(json.dumps({"items": control_plane_pods}))
            else:
                sys.stdout.write(json.dumps({"items": control_plane_pods + dependency_pods}))
            raise SystemExit(0)

        sys.stderr.write(f"unsupported fake kubectl invocation: namespace={namespace!r} args={cleaned!r}\n")
        raise SystemExit(1)
        """,
    )


def make_fake_leader_kubectl(path: pathlib.Path) -> None:
    write_executable(
        path,
        r"""
        #!/usr/bin/env python3
        import json
        import os
        import pathlib
        import re
        import subprocess
        import sys
        import time

        args = sys.argv[1:]
        cleaned = []
        i = 0
        while i < len(args):
            if args[i] == "--context":
                i += 2
                continue
            if args[i] in ("-n", "--namespace"):
                i += 2
                continue
            cleaned.append(args[i])
            i += 1

        state_dir = pathlib.Path(os.environ["FAKE_LEADER_STATE_DIR"])
        mode = os.environ["FAKE_LEADER_MODE"]
        log_path = state_dir / "kubectl.jsonl"
        descriptor = os.open(log_path, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
        try:
            os.write(descriptor, (json.dumps(cleaned, separators=(",", ":")) + "\n").encode())
        finally:
            os.close(descriptor)

        def record_event(name):
            event_path = state_dir / "events.log"
            event_descriptor = os.open(event_path, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
            try:
                os.write(event_descriptor, (name + "\n").encode())
            finally:
                os.close(event_descriptor)

        def wait_guard_terminal():
            pid_path = state_dir / "guard-exec.pid"
            deadline = time.monotonic() + 3
            while not pid_path.exists():
                if time.monotonic() >= deadline: raise SystemExit(2)
                time.sleep(0.01)
            pid = int(pid_path.read_text(encoding="utf-8"))
            while time.monotonic() < deadline:
                try:
                    if sys.platform.startswith("linux"):
                        data = pathlib.Path(f"/proc/{pid}/stat").read_text(encoding="utf-8")
                        state = data[data.rfind(")") + 2:].split()[0]
                    else:
                        result = subprocess.run(["ps", "-o", "stat=", "-p", str(pid)], capture_output=True, text=True)
                        if result.returncode != 0 or not result.stdout.strip(): return
                        state = result.stdout.strip()[0]
                    if state in {"Z", "X", "x"}: return
                except (FileNotFoundError, ProcessLookupError):
                    return
                time.sleep(0.01)
            raise SystemExit(2)

        postgres_name = "synara-stage2-postgres-0"
        postgres_uid = "postgres-uid-original"
        postgres_pod = {
            "metadata": {
                "name": postgres_name,
                "uid": postgres_uid,
                "deletionTimestamp": None,
                "labels": {"app.kubernetes.io/name": "synara-stage2-postgres"},
            },
            "status": {
                "phase": "Running",
                "conditions": [{"type": "Ready", "status": "True"}],
            },
        }
        control_plane_pods = [
            {
                "metadata": {"name": "synara-control-plane-a", "uid": "cp-uid-a", "deletionTimestamp": None},
                "status": {
                    "phase": "Running",
                    "conditions": [{"type": "Ready", "status": "True"}],
                },
            },
            {
                "metadata": {"name": "synara-control-plane-b", "uid": "cp-uid-b", "deletionTimestamp": None},
                "status": {
                    "phase": "Running",
                    "conditions": [{"type": "Ready", "status": "True"}],
                },
            },
        ]

        if cleaned == ["cluster-info"]:
            raise SystemExit(0)
        if cleaned[:2] == ["get", "--raw"]:
            sys.stdout.write('{"status":"ready","checks":{"database":{"status":"ready"},"schema":{"status":"ready"}}}')
            raise SystemExit(0)
        if cleaned[:2] == ["rollout", "status"]:
            raise SystemExit(0)
        if cleaned and cleaned[0] == "wait":
            raise SystemExit(0)
        if cleaned[:2] == ["get", f"pod/{postgres_name}"] and cleaned[-2:] == ["-o", "json"]:
            exact = dict(postgres_pod)
            exact["metadata"] = dict(postgres_pod["metadata"])
            count_path = state_dir / "postgres-get-count"
            count = int(count_path.read_text(encoding="utf-8")) + 1 if count_path.exists() else 1
            count_path.write_text(str(count), encoding="utf-8")
            if mode == "guard-exit-during-exact-get" and count == 2:
                (state_dir / "terminate-guard").write_text("now\n", encoding="utf-8")
                wait_guard_terminal()
            if mode == "eof-after-delete-before-unlock" and (state_dir / "leader-deleted").exists():
                time.sleep(0.2)
            if mode in ("replacement", "pg-replacement-before-ready"):
                exact["metadata"]["uid"] = "postgres-uid-replacement"
            elif mode == "pg-replacement-before-delete" and count >= 2:
                exact["metadata"]["uid"] = "postgres-uid-replacement"
            elif mode == "pg-replacement-after-delete" and (state_dir / "leader-deleted").exists():
                exact["metadata"]["uid"] = "postgres-uid-replacement"
            sys.stdout.write(json.dumps(exact))
            raise SystemExit(0)
        if cleaned[:2] == ["get", "pod/synara-control-plane-a"] and "--ignore-not-found" in cleaned:
            if mode == "delete-retry-guard-exit" and (state_dir / "delete-attempts").exists() and not (state_dir / "guard-exit-triggered").exists():
                (state_dir / "guard-exit-triggered").write_text("yes\n", encoding="utf-8")
                (state_dir / "terminate-guard").write_text("now\n", encoding="utf-8")
                wait_guard_terminal()
            deletion_state = (state_dir / "leader-deleted").read_text(encoding="utf-8").strip() if (
                state_dir / "leader-deleted"
            ).exists() else "active"
            if deletion_state == "absent":
                raise SystemExit(0)
            exact = dict(control_plane_pods[0])
            exact["metadata"] = dict(control_plane_pods[0]["metadata"])
            if deletion_state == "replacement":
                exact["metadata"]["uid"] = "cp-uid-a-replacement"
            elif deletion_state == "terminating":
                exact["metadata"]["deletionTimestamp"] = "2026-07-26T00:00:00Z"
            sys.stdout.write(json.dumps(exact))
            raise SystemExit(0)
        if cleaned[:2] == ["get", "pods"] and cleaned[-2:] == ["-o", "json"]:
            selector = cleaned[cleaned.index("-l") + 1] if "-l" in cleaned else ""
            if selector == "app.kubernetes.io/name=synara-stage2-postgres":
                sys.stdout.write(json.dumps({"items": [postgres_pod]}))
            else:
                if mode == "delay-before-delete" and not (state_dir / "leader-deleted").exists():
                    time.sleep(1.2)
                sys.stdout.write(json.dumps({"items": control_plane_pods}))
            raise SystemExit(0)
        if cleaned[:2] == ["delete", "--raw"]:
            payload = json.loads(sys.stdin.read())
            if payload.get("preconditions", {}).get("uid") != "cp-uid-a":
                sys.stderr.write("delete omitted original UID precondition\n")
                raise SystemExit(2)
            attempt_path = state_dir / "delete-attempts"
            attempt = int(attempt_path.read_text(encoding="utf-8")) + 1 if attempt_path.exists() else 1
            attempt_path.write_text(str(attempt), encoding="utf-8")
            record_event("UID_DELETE")
            if mode == "delete-applied-eof":
                (state_dir / "leader-deleted").write_text("absent\n", encoding="utf-8")
                sys.stderr.write("Unable to connect to the server: unexpected EOF\n")
                raise SystemExit(1)
            if mode in ("delete-retry", "delete-retry-guard-exit") and attempt == 1:
                sys.stderr.write("Unable to connect to the server: unexpected EOF\n")
                raise SystemExit(1)
            if mode == "delete-replacement":
                (state_dir / "leader-deleted").write_text("replacement\n", encoding="utf-8")
                sys.stderr.write("Unable to connect to the server: unexpected EOF\n")
                raise SystemExit(1)
            if mode == "delete-conflict":
                sys.stderr.write("Error from server (Conflict): UID precondition failed\n")
                raise SystemExit(1)
            (state_dir / "leader-deleted").write_text("absent\n", encoding="utf-8")
            sys.stdout.write('{"kind":"Pod","metadata":{"name":"synara-control-plane-a","uid":"cp-uid-a"}}')
            raise SystemExit(0)
        if cleaned and cleaned[0] == "exec":
            if cleaned[1:3] == ["-i", f"pod/{postgres_name}"]:
                (state_dir / "guard-exec.pid").write_text(str(os.getpid()), encoding="utf-8")
                lock_line = sys.stdin.readline()
                if "pg_advisory_lock" not in lock_line:
                    sys.stderr.write("guard SQL omitted advisory lock\n")
                    raise SystemExit(2)
                if mode == "eof-before-lease":
                    raise SystemExit(1)
                lease_sql = sys.stdin.readline()
                lease_match = re.search(r"__SYNARA_RECONCILER_GUARD_LEASE_V2__\|([0-9a-f]{32})\|", lease_sql)
                if lease_match is None:
                    sys.stderr.write("guard SQL omitted V2 lease sentinel\n")
                    raise SystemExit(2)
                nonce = lease_match.group(1)
                lease_nonce = "0" * 32 if mode == "wrong-nonce" else nonce
                lease = f"__SYNARA_RECONCILER_GUARD_LEASE_V2__|{lease_nonce}|synara-control-plane-a:instance|7"
                ready_sql = sys.stdin.readline()
                ready_match = re.search(r"(__SYNARA_RECONCILER_GUARD_READY_V2__\|[0-9a-f]{32}\|[^']+)", ready_sql)
                if ready_match is None:
                    sys.stderr.write("guard SQL omitted V2 ready sentinel\n")
                    raise SystemExit(2)
                ready = ready_match.group(1)
                if mode == "wrong-uid":
                    ready = ready.rsplit("|", 1)[0] + "|postgres-uid-wrong"
                if mode == "watchdog":
                    time.sleep(30)
                    raise SystemExit(1)
                if mode == "out-of-order":
                    sys.stdout.write(ready + "\n")
                    sys.stdout.flush()
                    record_event("READY")
                    sys.stdout.write(lease + "\n")
                    sys.stdout.flush()
                    record_event("LEASE")
                    time.sleep(0.1)
                    raise SystemExit(1)
                if mode == "partial":
                    sys.stdout.write(lease[: len(lease) // 2])
                    sys.stdout.flush()
                    time.sleep(0.2)
                    raise SystemExit(1)
                if mode == "split":
                    midpoint = len(lease) // 2
                    sys.stdout.write(lease[:midpoint])
                    sys.stdout.flush()
                    time.sleep(0.2)
                    sys.stdout.write(lease[midpoint:] + "\n")
                else:
                    sys.stdout.write(lease + "\n")
                sys.stdout.flush()
                record_event("LEASE")
                if mode == "eof-between-lease-ready":
                    raise SystemExit(1)
                if mode == "duplicate":
                    sys.stdout.write(lease + "\n")
                    sys.stdout.flush()
                if mode == "unknown-sentinel":
                    sys.stdout.write(f"__SYNARA_RECONCILER_GUARD_UNKNOWN_V2__|{nonce}\n")
                    sys.stdout.flush()
                sys.stdout.write(ready + "\n")
                sys.stdout.flush()
                record_event("READY")
                if mode == "early-unlocked":
                    sys.stdout.write(f"__SYNARA_RECONCILER_GUARD_UNLOCKED_V2__|{nonce}|true\n")
                    sys.stdout.flush()
                if mode == "eof-after-ready":
                    raise SystemExit(1)
                unlock_sql = ""
                while True:
                    if mode in ("guard-exit-during-exact-get", "delete-retry-guard-exit") and (state_dir / "terminate-guard").exists():
                        (state_dir / "guard-exited").write_text("yes\n", encoding="utf-8")
                        raise SystemExit(1)
                    if mode == "eof-after-delete-before-unlock" and (state_dir / "leader-deleted").exists():
                        raise SystemExit(1)
                    import select

                    readable, _, _ = select.select([sys.stdin], [], [], 0.05)
                    if not readable:
                        continue
                    unlock_sql = sys.stdin.readline()
                    break
                if not unlock_sql:
                    raise SystemExit(1)
                unlocked_match = re.search(r"__SYNARA_RECONCILER_GUARD_UNLOCKED_V2__\|([0-9a-f]{32})\|", unlock_sql)
                if unlocked_match is None or unlocked_match.group(1) != nonce:
                    sys.stderr.write("guard SQL omitted exact V2 unlock sentinel\n")
                    raise SystemExit(2)
                record_event("UNLOCK_COMMAND")
                if mode == "eof-after-unlock-before-sentinel":
                    raise SystemExit(1)
                sys.stdout.write(f"__SYNARA_RECONCILER_GUARD_UNLOCKED_V2__|{nonce}|true\n")
                sys.stdout.flush()
                record_event("UNLOCKED")
                quit_line = sys.stdin.readline()
                if quit_line.strip() != r"\quit":
                    sys.stderr.write("guard SQL omitted quit command\n")
                    raise SystemExit(2)
                record_event("EXIT")
                raise SystemExit(0)
            if "reconciler_leases" in " ".join(cleaned):
                if (state_dir / "leader-deleted").exists():
                    sys.stdout.write("synara-control-plane-b:instance|8\n")
                else:
                    sys.stdout.write("synara-control-plane-a:instance|7\n")
                raise SystemExit(0)

        sys.stderr.write(f"unsupported fake leader kubectl invocation: {cleaned!r}\n")
        raise SystemExit(1)
        """,
    )


def run_fake_leader_case(
    temp_dir: pathlib.Path, mode: str
) -> tuple[subprocess.CompletedProcess[str], dict, list[list[str]]]:
    case_dir = temp_dir / mode
    case_dir.mkdir(parents=True, exist_ok=True)
    fake_bin = case_dir / "bin"
    fake_bin.mkdir(exist_ok=True)
    make_fake_leader_kubectl(fake_bin / "kubectl")
    evidence_path = case_dir / "evidence.json"
    env = os.environ.copy()
    env["PATH"] = f"{fake_bin}{os.pathsep}{env.get('PATH', '')}"
    env["FAKE_LEADER_STATE_DIR"] = str(case_dir)
    env["FAKE_LEADER_MODE"] = mode
    env["SYNARA_K8S_CONTEXT"] = "managed-leader-validation"
    env["SYNARA_K8S_ACCEPTANCE_ALLOW_NONDISPOSABLE"] = "1"
    env["SYNARA_K8S_RESILIENCE_BOOTSTRAP_BASELINE"] = "0"
    env["SYNARA_K8S_RESILIENCE_CASES"] = "leader-takeover"
    env["SYNARA_K8S_RESILIENCE_PROBE_INTERVAL_SECONDS"] = "1"
    env["SYNARA_K8S_RESILIENCE_CASE_TIMEOUT_SECONDS"] = "4"
    env["SYNARA_K8S_RESILIENCE_EVIDENCE_FILE"] = str(evidence_path)
    command = ["bash", str(SCRIPT_DIR / "resilience-acceptance.sh")]
    completed = subprocess.run(command, env=env, capture_output=True, text=True, timeout=10)
    if not evidence_path.exists():
        fail(
            f"fake leader {mode}: resilience run omitted evidence "
            f"(returncode={completed.returncode}, stdout={completed.stdout!r}, stderr={completed.stderr!r})"
        )
    payload = json.loads(evidence_path.read_text(encoding="utf-8"))
    log_path = case_dir / "kubectl.jsonl"
    invocations = [json.loads(line) for line in log_path.read_text(encoding="utf-8").splitlines()]
    return completed, payload, invocations


def fake_leader_events(temp_dir: pathlib.Path, mode: str) -> list[str]:
    event_path = temp_dir / mode / "events.log"
    return event_path.read_text(encoding="utf-8").splitlines() if event_path.exists() else []


def pid_is_alive(pid: int) -> bool:
    try:
        os.kill(pid, 0)
    except ProcessLookupError:
        return False
    except PermissionError:
        return True
    else:
        return True


def wait_for_pid_exit(pid: int, timeout_seconds: float = 2.0) -> bool:
    deadline = time.monotonic() + timeout_seconds
    while pid_is_alive(pid):
        if time.monotonic() >= deadline:
            return False
        time.sleep(0.05)
    return True


def run_fake_managed_case(
    temp_dir: pathlib.Path,
    label: str,
    *,
    start_mode: str = "normal",
    verify_mode: str = "normal",
    stop_mode: str = "normal",
    start_timeout: int = 3,
    controller_mode: str | None = None,
    soak: bool = False,
    prehook_mode: str | None = None,
    identity_failure: bool = False,
) -> tuple[subprocess.CompletedProcess[str], dict, str, pathlib.Path]:
    case_dir = temp_dir / label
    case_dir.mkdir(parents=True, exist_ok=True)
    fake_bin = case_dir / "bin"
    fake_bin.mkdir(exist_ok=True)
    make_fake_kubectl(fake_bin / "kubectl")
    hook_path = case_dir / "managed-hook.py"
    hook_log_path = case_dir / "hook.log"
    late_marker = case_dir / "late.marker"
    evidence_path = case_dir / "evidence.json"
    if controller_mode is not None:
        fake_controller = case_dir / "fake-controller.py"
        write_executable(fake_controller, r"""
        #!/usr/bin/env python3
        import hashlib, json, os, subprocess, sys
        request = json.load(sys.stdin)
        mode = os.environ["FAKE_CONTROLLER_MODE"]
        real_controller = os.environ["FAKE_REAL_CONTROLLER"]
        log_path = os.environ["FAKE_CONTROLLER_LOG"]
        def digest(value):
            return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
        unit = f"synara-managed-hook-{request['phase']}-{digest({'operationId':request['operationId'],'phase':request['phase']})[:32]}.service"
        if request["action"] == "recover":
            with open(log_path, "a", encoding="utf-8") as handle: handle.write("recover:" + request["phase"] + "\n")
            operation_digest = hashlib.sha256(request["operationId"].encode()).hexdigest()
            if mode == "replayed-recovery": operation_digest = "0" * 64
            payload = {
              "schemaVersion":"synara.managed-hook-controller.result.v1","status":"recovered","reason":"unit-terminal",
              "securityBoundary":request["securityBoundary"],"operationIdDigest":operation_digest,"phase":request["phase"],
              "backend":request["backend"],"unitNameDigest":hashlib.sha256(unit.encode()).hexdigest(),"scopeDigest":None,
              "expectedTransition":None,"hookExitCode":None,"timedOut":False,"cancelled":False,"unitFound":False,
              "terminationConfirmed":True,"members":{"executable":0,"zombie":0,"deadUpper":0,"deadLower":0,"unreadable":0}}
            print(json.dumps(payload, separators=(",", ":"))); raise SystemExit(0)
        if mode == "fast-exit" and request["phase"] != "stop":
            raise SystemExit(125)
        completed = subprocess.run([sys.executable, real_controller], input=json.dumps(request), text=True, capture_output=True)
        payload = json.loads(completed.stdout)
        if request["phase"] != "stop":
            if mode == "failure-terminated":
                for key in ("artifactDigest","evidenceDigests","checkCount"): payload.pop(key, None)
                payload["status"], payload["reason"] = "failed", "injected-failure"
                completed = subprocess.CompletedProcess([], 1, "", "")
            elif mode == "wrong-operation": payload["operationIdDigest"] = "0" * 64
            elif mode == "wrong-scope": payload["scopeDigest"] = "0" * 64
            elif mode == "wrong-unit": payload["unitNameDigest"] = "0" * 64
            elif mode == "wrong-phase": payload["phase"] = "stop"
            elif mode == "exit0-failed": payload["status"], payload["reason"] = "failed", "injected"
            elif mode == "extra-secret": payload["secret"] = "must-not-persist"
            elif mode == "replayed-recovery": payload["operationIdDigest"] = "0" * 64
            elif mode == "nonzero-passed": completed = subprocess.CompletedProcess([], 1, "", "")
        print(json.dumps(payload, separators=(",", ":")))
        raise SystemExit(completed.returncode)
        """)
    else:
        fake_controller = None
    write_executable(
        hook_path,
        r"""
        #!/usr/bin/env python3
        import datetime as dt
        import json
        import os
        import pathlib
        import subprocess
        import sys
        import time

        mode, log_path, marker_path = sys.argv[1:4]
        phase = os.environ["SYNARA_MANAGED_HOOK_PHASE"]
        with open(log_path, "a", encoding="utf-8") as handle:
            handle.write(f"{phase}:{mode}\n")
        if mode == "sleep":
            time.sleep(30)
        if mode == "missing":
            raise SystemExit(0)
        if mode == "fail":
            raise SystemExit(17)
        if mode == "setsid-late":
            subprocess.Popen(
                [
                    sys.executable,
                    "-c",
                    "import pathlib,sys,time; time.sleep(.4); pathlib.Path(sys.argv[1]).write_text('late')",
                    marker_path,
                ],
                start_new_session=True,
                stdin=subprocess.DEVNULL,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                close_fds=True,
            )
            raise SystemExit(0)
        if mode == "zombie":
            subprocess.Popen([sys.executable, "-c", "pass"]).wait()
        if mode == "inspect-fds":
            root = pathlib.Path("/proc/self/fd") if pathlib.Path("/proc/self/fd").exists() else pathlib.Path("/dev/fd")
            targets = []
            for entry in root.iterdir():
                try:
                    targets.append(os.readlink(entry))
                except OSError:
                    pass
            with open(log_path, "a", encoding="utf-8") as handle:
                handle.write("fds:" + "|".join(targets) + "\n")

        transition = {"start": "terminal-applied", "verify": "applied", "stop": "terminal-healed"}[phase]
        operation_id = os.environ["SYNARA_MANAGED_HOOK_OPERATION_ID"]
        if mode == "mismatch-operation":
            operation_id = "different-operation-1234"
        if mode == "pending":
            transition = "pending"
        payload = {
            "schemaVersion": "synara.managed-hook-transition.v1",
            "operationId": operation_id,
            "phase": phase,
            "transition": transition,
            "context": os.environ["SYNARA_MANAGED_HOOK_CONTEXT"],
            "namespace": os.environ["SYNARA_MANAGED_HOOK_NAMESPACE"],
            "node": os.environ["SYNARA_MANAGED_HOOK_NODE"],
            "nodeUid": os.environ["SYNARA_MANAGED_HOOK_NODE_UID"],
            "pod": os.environ["SYNARA_MANAGED_HOOK_POD"],
            "podUid": os.environ["SYNARA_MANAGED_HOOK_POD_UID"],
            "challenge": os.environ["SYNARA_MANAGED_HOOK_CHALLENGE"],
            "observedAt": dt.datetime.now(dt.timezone.utc).isoformat().replace("+00:00", "Z"),
            "checks": [{"name": "provider-check-secret", "status": "passed", "evidenceDigest": "ab" * 32}],
        }
        pathlib.Path(os.environ["SYNARA_MANAGED_HOOK_ARTIFACT"]).write_text(
            json.dumps(payload), encoding="utf-8"
        )
        """,
    )
    command = lambda mode: f"{shlex.quote(sys.executable)} {shlex.quote(str(hook_path))} {shlex.quote(mode)} {shlex.quote(str(hook_log_path))} {shlex.quote(str(late_marker))}"
    env = os.environ.copy()
    env["PATH"] = f"{fake_bin}{os.pathsep}{env.get('PATH', '')}"
    env["SYNARA_K8S_CONTEXT"] = "managed-validation"
    env["SYNARA_K8S_ACCEPTANCE_ALLOW_NONDISPOSABLE"] = "1"
    env["SYNARA_K8S_RESILIENCE_BOOTSTRAP_BASELINE"] = "0"
    env["SYNARA_K8S_RESILIENCE_CASES"] = "node-partition"
    env["SYNARA_K8S_RESILIENCE_PARTITION_NODE"] = "safe-worker"
    env["SYNARA_K8S_RESILIENCE_PARTITION_SECONDS"] = "1"
    env["SYNARA_K8S_RESILIENCE_DISRUPTION_WINDOW_SECONDS"] = "1"
    env["SYNARA_K8S_RESILIENCE_PROBE_INTERVAL_SECONDS"] = "1"
    env["SYNARA_K8S_RESILIENCE_CASE_TIMEOUT_SECONDS"] = "5"
    env["SYNARA_K8S_NODE_PARTITION_START_HOOK_TIMEOUT_SECONDS"] = str(start_timeout)
    env["SYNARA_K8S_NODE_PARTITION_VERIFY_HOOK_TIMEOUT_SECONDS"] = "3"
    env["SYNARA_K8S_NODE_PARTITION_STOP_HOOK_TIMEOUT_SECONDS"] = "3"
    env["SYNARA_K8S_NODE_PARTITION_START_HOOK"] = command(start_mode)
    env["SYNARA_K8S_NODE_PARTITION_VERIFY_HOOK"] = command(verify_mode)
    env["SYNARA_K8S_NODE_PARTITION_STOP_HOOK"] = command(stop_mode)
    env["SYNARA_K8S_RESILIENCE_EVIDENCE_FILE"] = str(evidence_path)
    if prehook_mode is not None:
        env["FAKE_MANAGED_PREHOOK_MODE"] = prehook_mode
    if identity_failure:
        env["SYNARA_K8S_TEST_CONTROLLER_IDENTITY_FAILURE_FILE"] = str(case_dir / "identity-failure.once")
    if soak:
        env["SYNARA_K8S_RESILIENCE_SOAK_SECONDS"] = "2"
        env["SYNARA_K8S_RESILIENCE_SOAK_INTERVAL_SECONDS"] = "1"
        env["SYNARA_K8S_RESILIENCE_SOAK_CASES"] = "node-partition"
    if fake_controller is not None:
        env["SYNARA_K8S_MANAGED_HOOK_CONTROLLER"] = str(fake_controller)
        env["FAKE_CONTROLLER_MODE"] = str(controller_mode)
        env["FAKE_REAL_CONTROLLER"] = str(SCRIPT_DIR / "managed-hook-controller.py")
        env["FAKE_CONTROLLER_LOG"] = str(hook_log_path)
    completed = subprocess.run(
        ["bash", str(SCRIPT_DIR / "resilience-acceptance.sh")],
        env=env,
        capture_output=True,
        text=True,
        check=False,
        timeout=60 if soak else 20,
    )
    if not evidence_path.exists():
        fail(
            f"{label}: resilience run did not write evidence "
            f"(returncode={completed.returncode}, stdout={completed.stdout!r}, stderr={completed.stderr!r})"
        )
    payload = json.loads(evidence_path.read_text(encoding="utf-8"))
    hook_log = hook_log_path.read_text(encoding="utf-8") if hook_log_path.exists() else ""
    return completed, payload, hook_log, late_marker


def main() -> None:
    kind_text = (SCRIPT_DIR / "kind-multinode.yaml").read_text(encoding="utf-8")
    if kind_text.count("role: control-plane") != 1:
        fail("kind-multinode.yaml must define exactly one control-plane node")
    if kind_text.count("role: worker") < 3:
        fail("kind-multinode.yaml must define at least three worker nodes")
    if 'synara.io/resilience-dependency: "true"' not in kind_text:
        fail("kind-multinode.yaml must reserve one worker for acceptance dependencies")

    rbac_text = (SCRIPT_DIR / "rbac.yaml").read_text(encoding="utf-8")
    token_rule = require_regex(
        rbac_text,
        r'resources:\s*\["tokenreviews"\]\s*\n\s*verbs:\s*\[(.*?)\]',
        "TokenReview RBAC rule",
    )
    if "create" not in token_rule.group(1):
        fail("TokenReview RBAC rule must allow create")
    namespace_rule = require_regex(
        rbac_text,
        r'resources:\s*\["namespaces"\]\s*\n\s*verbs:\s*\[(.*?)\]',
        "namespace RBAC rule",
    )
    namespace_verbs = namespace_rule.group(1)
    if "patch" not in namespace_verbs or "create" not in namespace_verbs or "get" not in namespace_verbs:
        fail("namespace RBAC rule must allow get/create/patch")
    if "delete" in namespace_verbs:
        fail("namespace RBAC rule must not allow delete")
    pods_rule = require_regex(
        rbac_text,
        r'resources:\s*\["pods"\]\s*\n\s*verbs:\s*\[(.*?)\]',
        "pod RBAC rule",
    )
    for verb in ("get", "list", "create", "patch", "delete"):
        if verb not in pods_rule.group(1):
            fail(f"pod RBAC rule must allow {verb}")
    if "watch" in pods_rule.group(1) or "update" in pods_rule.group(1):
        fail("pod RBAC rule must not allow watch/update")

    kustomization_text = (SCRIPT_DIR / "kustomization.yaml").read_text(encoding="utf-8")
    if "pod-disruption-budget.yaml" not in kustomization_text:
        fail("kustomization.yaml must include pod-disruption-budget.yaml")

    deployment_text = (SCRIPT_DIR / "deployment.yaml").read_text(encoding="utf-8")
    if "topologySpreadConstraints:" not in deployment_text:
        fail("deployment.yaml must include topologySpreadConstraints")
    if "podAntiAffinity:" not in deployment_text:
        fail("deployment.yaml must include podAntiAffinity")

    acceptance_text = (SCRIPT_DIR / "acceptance.sh").read_text(encoding="utf-8")
    if acceptance_text.count("list_ready_control_plane_pods") < 3:
        fail("acceptance.sh must use one lifecycle-safe Control Plane Pod selector for both log audits")
    if 'select(.metadata.deletionTimestamp == null)' not in acceptance_text:
        fail("acceptance.sh log audit must exclude terminating Control Plane Pods")
    if 'select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))' not in acceptance_text:
        fail("acceptance.sh log audit must select Ready Control Plane Pods")
    if acceptance_text.count("logs --pod-running-timeout=60s") != 2:
        fail("acceptance.sh log audit must wait for both before/after Pod containers")
    if acceptance_text.count("wait_for_control_plane_ready_replicas") < 4:
        fail("acceptance.sh must use steady-state Ready replica waits after non-rollout disruptions")
    if acceptance_text.count("rollout status deployment/synara-control-plane") != 1:
        fail("acceptance.sh must reserve Control Plane rollout status for the initial rollout only")
    if "wait_for_namespace_absent 180" not in acceptance_text:
        fail("acceptance.sh must wait for asynchronous cleanup before recreating its selected namespace")
    if "already exists and is not terminating; refusing to reuse it" not in acceptance_text:
        fail("acceptance.sh must refuse to overwrite a live selected namespace")
    for fragment in (
        'namespace="${SYNARA_K8S_NAMESPACE:-synara-system}"',
        'SYNARA_K8S_ACCEPTANCE_RBAC_NAME',
        'SYNARA_K8S_ACCEPTANCE_OWNER',
        'created_namespace=0',
        'cleanup_rbac=0',
        'create namespace "$namespace"',
        'create -k "$overlay_dir"',
        'synara.ai~1acceptance-owner',
        'RBAC identity %s already exists; refusing to overwrite it',
        'delete clusterrolebinding "$rbac_name"',
        'value: $namespace',
        'value: $rbac_name',
    ):
        if fragment not in acceptance_text:
            fail(f"acceptance.sh omitted isolated namespace/RBAC behavior: {fragment}")
    if "namespace: synara-system" in acceptance_text:
        fail("acceptance.sh runtime resources must not retain a hard-coded namespace")

    with tempfile.TemporaryDirectory(prefix="synara-acceptance-ownership-") as temp_dir_raw:
        temp_dir = pathlib.Path(temp_dir_raw)
        fake_kubectl = temp_dir / "kubectl"
        fake_kubectl.write_text(
            """#!/bin/sh
set -eu
printf '%s\\n' "$*" >>"$SYNARA_FAKE_KUBECTL_LOG"
case "$*" in
  *"cluster-info"*) exit 0 ;;
  *"get namespace synara-existing"*)
    [ "$SYNARA_FAKE_EXISTING" = "namespace" ] && exit 0
    exit 1
    ;;
  *"get clusterrole synara-control-plane-reconciler-synara-existing"*)
    [ "$SYNARA_FAKE_EXISTING" = "rbac" ] && exit 0
    exit 1
    ;;
  *"get clusterrolebinding synara-control-plane-reconciler-synara-existing"*) exit 1 ;;
esac
exit 0
""",
            encoding="utf-8",
        )
        fake_kubectl.chmod(0o755)
        for existing_kind, expected_error in (
            ("namespace", "already exists and is not terminating; refusing to reuse it"),
            ("rbac", "already exists; refusing to overwrite it"),
        ):
            log_path = temp_dir / f"{existing_kind}.log"
            env = os.environ.copy()
            env["PATH"] = f"{temp_dir}:{env['PATH']}"
            env["SYNARA_K8S_CONTEXT"] = "kind-validation"
            env["SYNARA_K8S_NAMESPACE"] = "synara-existing"
            env["SYNARA_FAKE_EXISTING"] = existing_kind
            env["SYNARA_FAKE_KUBECTL_LOG"] = str(log_path)
            collision = subprocess.run(
                ["bash", str(SCRIPT_DIR / "acceptance.sh")],
                env=env,
                capture_output=True,
                text=True,
            )
            if collision.returncode == 0 or expected_error not in collision.stderr:
                fail(f"acceptance.sh did not fail closed for an existing {existing_kind} identity")
            kubectl_log = log_path.read_text(encoding="utf-8")
            if "delete namespace" in kubectl_log or "delete clusterrole" in kubectl_log:
                fail(f"acceptance.sh cleanup deleted an unowned {existing_kind} identity")
    if "required_reconciler_leases" not in acceptance_text:
        fail("acceptance.sh must prove the core reconciler leases become active")
    if acceptance_text.count("postgres_scalar") < 4:
        fail("acceptance.sh must use bounded PostgreSQL retries for transient kubectl exec failures")
    if acceptance_text.count("kube_raw_get") < 5:
        fail("acceptance.sh must use bounded retries for direct Kubernetes raw API probes")
    if ":'lease_name'" in acceptance_text:
        fail("acceptance.sh must not use unsupported psql variable interpolation inside -c")

    resilience_text = (SCRIPT_DIR / "resilience-acceptance.sh").read_text(encoding="utf-8")
    for fragment in (
        'get clusterrole "$rbac_name"',
        'delete clusterrolebinding "$rbac_name"',
        'acceptance_resource_owner',
        'SYNARA_K8S_NAMESPACE="$namespace"',
        'SYNARA_K8S_ACCEPTANCE_RBAC_NAME="$rbac_name"',
        'SYNARA_K8S_ACCEPTANCE_OWNER="$acceptance_owner"',
    ):
        if fragment not in resilience_text:
            fail(f"resilience-acceptance.sh omitted isolated baseline behavior: {fragment}")
    for fragment in (
        "leader-takeover",
        "synara:kubernetes-execution-reconciler",
        "fencing_token",
        "pg_advisory_lock",
        "PGAPPNAME",
        'exec -i "pod/$postgres_pod"',
        "__SYNARA_RECONCILER_GUARD_READY_V2__",
        "__SYNARA_RECONCILER_GUARD_LEASE_V2__",
        "__SYNARA_RECONCILER_GUARD_UNLOCKED_V2__",
        "parse_reconciler_takeover_guard_output",
        "verify_reconciler_takeover_guard_pod",
        "assert_reconciler_takeover_guard_active",
        "release_reconciler_takeover_guard",
        'mkfifo "$psql_fifo" "$command_fifo"',
        'exec 7<>"$psql_fifo"',
        'exec 3<>"$command_fifo"',
        'exec 4>"$psql_fifo"',
        'UNLOCK|$guard_nonce',
        "guard-watchdog-timeout",
        "guard-final-wait-timeout",
        "guardOutputMaxBytes: 2000",
        'delete --raw "$raw_uri" -f -',
        'preconditions:{uid:$uid}',
        "reconcile_pod_uid_delete",
        "wait_for_pod_uid_absent",
        "deletedPodUID",
        "start_continuous_probe",
        "wait_for_reconciler_takeover",
    ):
        if fragment not in resilience_text:
            fail(f"resilience-acceptance.sh omitted leader takeover evidence: {fragment}")
    if "pg_stat_activity" in resilience_text:
        fail("resilience-acceptance.sh must not flood kubectl exec while waiting for the takeover guard")
    guard_text = resilience_text.split("start_reconciler_takeover_guard() {", 1)[1].split("get_pdb_json() {", 1)[0]
    for forbidden in ("pg_sleep", "coproc", "wait -n", "declare -A", "exec {"):
        if forbidden in guard_text:
            fail(f"leader takeover guard contains non-V2/Bash-3 behavior: {forbidden}")
    leader_case_text = resilience_text.split("case_leader_takeover() {", 1)[1].split(
        "case_control_plane_failover() {", 1
    )[0]
    if 'wait --for=delete "pod/$before_pod"' in leader_case_text:
        fail("leader takeover must wait for the original Pod UID, not a reusable Pod name")
    if ":'lease_name'" in resilience_text:
        fail("resilience-acceptance.sh must not use unsupported psql variable interpolation inside -c")
    if "set +e" in resilience_text:
        fail("resilience-acceptance.sh must capture expected failures without mutating global errexit state")
    identity_helpers = "controller_process_identity() {" + resilience_text.split(
        "controller_process_identity() {", 1
    )[1].split("run_managed_controller() {", 1)[0]
    identity_probe = subprocess.run(
        ["bash", "-c", """
set -euo pipefail
%s
sleep 30 & probe=$!
identity="$(controller_process_identity "$probe")"
if signal_owned_process "$probe" "${identity}-reused" TERM 2>/dev/null; then
  kill "$probe" 2>/dev/null || true; wait "$probe" 2>/dev/null || true; exit 41
fi
kill -0 "$probe"
kill "$probe"
wait "$probe" 2>/dev/null || true
""" % identity_helpers],
        capture_output=True,
        text=True,
        check=False,
        timeout=5,
    )
    if identity_probe.returncode != 0:
        fail(f"guard PID identity drift probe failed or signalled a replacement: {identity_probe.stderr!r}")
    stop_owned_helper = "stop_owned_child() {" + resilience_text.split(
        "stop_owned_child() {", 1
    )[1].split("get_pdb_json() {", 1)[0]
    cleanup_probe = subprocess.run(
        ["bash", "-c", """
set -u
trap 'jobs -pr | xargs kill 2>/dev/null || true' EXIT
%s
%s
release="$(mktemp)"; rm -f "$release"
( while [[ ! -f "$release" ]]; do sleep .01; done ) & controller=$!
controller_identity="$(controller_process_identity "$controller")"
sleep 30 & guard=$!; guard_identity="$(controller_process_identity "$guard")"
touch "$release"
deadline=$((SECONDS + 3))
while kill -0 "$controller" 2>/dev/null && ! process_is_terminal_state "$controller"; do
  (( SECONDS < deadline )) || exit 50
  sleep .01
done
LEASE_GUARD_ANCHOR_OPEN=0
LEASE_GUARD_COMMAND_WRITER_PID=""; LEASE_GUARD_COMMAND_WRITER_PID_IDENTITY=""
LEASE_GUARD_CONTROLLER_PID="$controller"; LEASE_GUARD_CONTROLLER_PID_IDENTITY="$controller_identity"
LEASE_GUARD_PID="$guard"; LEASE_GUARD_PID_IDENTITY="$guard_identity"
LEASE_GUARD_WATCHDOG_PID=""; LEASE_GUARD_WATCHDOG_PID_IDENTITY=""; LEASE_GUARD_RUNTIME_DIR=""
stop_reconciler_takeover_guard || exit 51
kill -0 "$guard" 2>/dev/null && exit 52
sleep 30 & replacement=$!; replacement_identity="$(controller_process_identity "$replacement")"
sleep 30 & other=$!; other_identity="$(controller_process_identity "$other")"
LEASE_GUARD_CONTROLLER_PID="$replacement"; LEASE_GUARD_CONTROLLER_PID_IDENTITY="${replacement_identity}-drift"
LEASE_GUARD_PID="$other"; LEASE_GUARD_PID_IDENTITY="$other_identity"
if stop_reconciler_takeover_guard; then exit 53; fi
kill -0 "$replacement" || exit 54
kill -0 "$other" 2>/dev/null && exit 55
kill "$replacement"; wait "$replacement" 2>/dev/null || true
""" % (identity_helpers, stop_owned_helper)],
        capture_output=True,
        text=True,
        check=False,
        timeout=10,
    )
    if cleanup_probe.returncode != 0:
        fail(f"owned-child aggregated cleanup regression failed: rc={cleanup_probe.returncode} stderr={cleanup_probe.stderr!r}")
    for fragment in (
        "final_report_emitted",
        "unexpected_exit_status",
        "readiness probe process failed during control plane failover",
        "readiness probe process failed during drain simulation",
        "readiness probe process failed during node partition",
        "for attempt in 1 2 3",
        'trap \'\' HUP INT TERM',
        'SYNARA_K8S_MANAGED_HOOK_CONTROLLER:-$script_dir/managed-hook-controller.py',
        'schemaVersion: "synara.managed-hook-controller.request.v1"',
        'action: "recover"',
        'action: "execute"',
        'PARTITION_HOOK_ACTIVE=1',
        "recover_managed_partition_phase",
        "interrupt_managed_controller",
        "controller_process_identity",
        'backend: $backend',
        'testOnly: $testOnly',
        'securityBoundary: $controller.securityBoundary',
        'command: ["/bin/sh", "-c", $command]',
    ):
        if fragment not in resilience_text:
            fail(f"resilience-acceptance.sh omitted fail-closed harness behavior: {fragment}")
    for forbidden in ("ACTIVE_HOOK_AUTHORITY_SECRET", "exec 9", "import hmac", "processGroupId"):
        if forbidden in resilience_text:
            fail(f"legacy managed hook authority survived controller migration: {forbidden}")
    for fragment in (
        'journal_file="${evidence_file}.journal.jsonl"',
        'partial_file="${evidence_file}.partial.json"',
        "write_partial_snapshot",
        "record_progress_update",
        "commandRedacted: true",
        "SYNARA_K8S_RESILIENCE_EVIDENCE_FILE must be set explicitly when soak is enabled",
        "SYNARA_K8S_NODE_PARTITION_START_HOOK",
        "SYNARA_K8S_NODE_PARTITION_VERIFY_HOOK",
        "SYNARA_K8S_NODE_PARTITION_STOP_HOOK",
        "SYNARA_K8S_NODE_PARTITION_HOOK_TIMEOUT_SECONDS",
        "SYNARA_K8S_NODE_PARTITION_START_HOOK_TIMEOUT_SECONDS",
        "SYNARA_K8S_NODE_PARTITION_VERIFY_HOOK_TIMEOUT_SECONDS",
        "SYNARA_K8S_NODE_PARTITION_STOP_HOOK_TIMEOUT_SECONDS",
        'node_partition_start_hook_timeout_seconds="${SYNARA_K8S_NODE_PARTITION_START_HOOK_TIMEOUT_SECONDS:-$node_partition_hook_timeout_seconds}"',
        'node_partition_verify_hook_timeout_seconds="${SYNARA_K8S_NODE_PARTITION_VERIFY_HOOK_TIMEOUT_SECONDS:-$node_partition_hook_timeout_seconds}"',
        'require_positive_int "$node_partition_verify_hook_timeout_seconds" "SYNARA_K8S_NODE_PARTITION_VERIFY_HOOK_TIMEOUT_SECONDS"',
        'node_partition_stop_hook_timeout_seconds="${SYNARA_K8S_NODE_PARTITION_STOP_HOOK_TIMEOUT_SECONDS:-$node_partition_hook_timeout_seconds}"',
        "PARTITION_HOOK_OPERATION_ID",
        "managed-transition-",
        "stop_managed_partition_hook",
        "node partition for non-Kind contexts requires explicit",
        "managed node partition stop hook failed",
    ):
        if fragment not in resilience_text:
            fail(f"resilience-acceptance.sh omitted durable progress evidence behavior: {fragment}")

    managed_controller_text = (SCRIPT_DIR / "managed-hook-controller.py").read_text(encoding="utf-8")
    for fragment in (
        "SYNARA_MANAGED_HOOK_ARTIFACT",
        '"start": "terminal-applied"',
        '"verify": "applied"',
        '"stop": "terminal-healed"',
        '"--collect"',
        '"--property=ExitType=cgroup"',
        "strict_json_loads",
        "read_cgroup_populated",
    ):
        if fragment not in managed_controller_text:
            fail(f"managed-hook-controller.py omitted production containment behavior: {fragment}")

    dual_cluster_text = (SCRIPT_DIR / "kind-dual-cluster-acceptance.sh").read_text(encoding="utf-8")
    for fragment in (
        'primary_context="${SYNARA_DUAL_CLUSTER_PRIMARY_CONTEXT:-orbstack}"',
        "SYNARA_DUAL_CLUSTER_ALLOW_NON_ORBSTACK_PRIMARY=1",
        '--target worker-acceptance',
        '--allow-dirty',
        '--metadata-file "$worker_metadata_file"',
        '--label "synara.io/acceptance-run-id=$run_id"',
        'KUBECONFIG="$secondary_kubeconfig" "$kind_bin" create cluster',
        '(cd "$control_plane_dir" && env',
        'synara.io/acceptance-run-id: $run_id',
        'automountServiceAccountToken: false',
        'create token "$service_account"',
        "resource_is_owned",
        "io.x-k8s.kind.cluster",
        "SYNARA_DUAL_CLUSTER_INTEGRATION_PRIMARY_API_SERVER",
        "SYNARA_DUAL_CLUSTER_INTEGRATION_SECONDARY_API_SERVER",
        "SYNARA_DUAL_CLUSTER_INTEGRATION_EVIDENCE_DETAIL_FILE",
        "SYNARA_DUAL_CLUSTER_INTEGRATION_WORKER_IMAGE_ID",
        "SYNARA_DUAL_CLUSTER_INTEGRATION_PRIMARY_WORKER_API_HOST_ALIAS",
        "SYNARA_DUAL_CLUSTER_INTEGRATION_SECONDARY_WORKER_API_HOST_ALIAS",
        "TestStage4DualClusterDisasterRecoveryAgainstRealAPIServers",
        "detailContainsNoConnectionMaterial",
        "currentContextUnchanged",
        '[[ "$primary_api_server" != "$secondary_api_server" ]]',
        "delete_owned_worker_image_tag",
        '"$current_id" != "$worker_config_id"',
        '"$current_owner" != "$run_id"',
        'worker_manifest_digest="$(jq -er',
        'containerimage.digest',
        'resources: ["tokenreviews"]',
        'podBoundWorkloadIdentityVerified: ($assertions.podBoundWorkloadIdentityVerified // false)',
        'fullWorkerRegistrationPersistenceVerified: false',
        'source: {gitSHA: $sourceSHA, worktreeDirty: $worktreeDirty}',
        'configID: $workerConfigID, manifestDigest: $workerManifestDigest',
        'delete --raw "$raw_uri" -f -',
        'preconditions:{uid:$uid}',
        "wait_for_uid_absent",
        'created_object="$(kube "$target" create -f - -o json',
        "detailAssertionsAllowlistedBooleans",
        'all(.assertions[]; type == "boolean")',
        '(.assertions | keys | sort) == ($allowed | sort)',
        "attempts=90",
        "tag presence could not be verified",
        'primary_host_alias="${SYNARA_DUAL_CLUSTER_PRIMARY_HOST_ALIAS:-host.docker.internal}"',
        '[[ ! -e "$evidence_file" ]]',
        'primary_target_namespace_uid',
        'secondary_target_namespace_uid',
        'SYNARA_DUAL_CLUSTER_INTEGRATION_PRIMARY_NAMESPACE="$primary_target_namespace"',
        'SYNARA_DUAL_CLUSTER_INTEGRATION_PRIMARY_NAMESPACE_UID="$primary_target_namespace_uid"',
        'SYNARA_DUAL_CLUSTER_INTEGRATION_SECONDARY_NAMESPACE="$secondary_target_namespace"',
        'SYNARA_DUAL_CLUSTER_INTEGRATION_SECONDARY_NAMESPACE_UID="$secondary_target_namespace_uid"',
        'SYNARA_DUAL_CLUSTER_INTEGRATION_RUN_ID="$run_id"',
    ):
        if fragment not in dual_cluster_text:
            fail(f"kind-dual-cluster-acceptance.sh omitted safety behavior: {fragment}")
    for forbidden in (
        "kubectl config use-context",
        "SYNARA_DUAL_CLUSTER_INTEGRATION_PRIMARY_TOKEN=\"$primary_token\" >",
        "SYNARA_DUAL_CLUSTER_INTEGRATION_SECONDARY_TOKEN=\"$secondary_token\" >",
        "synara-system",
        "busybox:",
        'kube "$target" delete "$resource"',
        "resource_uid()",
    ):
        if forbidden in dual_cluster_text:
            fail(f"kind-dual-cluster-acceptance.sh contains forbidden behavior: {forbidden}")
    evidence_writer = dual_cluster_text.split("write_evidence() {", 1)[1].split("cleanup_all() {", 1)[0]
    for secret_name in ("primary_token", "secondary_token", "primary_ca", "secondary_ca"):
        if secret_name in evidence_writer:
            fail(f"kind-dual-cluster-acceptance.sh evidence writer references secret material: {secret_name}")

    contract_text = CONTRACT_PATH.read_text(encoding="utf-8")
    normalized_contract_text = " ".join(contract_text.split())
    for fragment in (
        "SYNARA_K8S_NODE_PARTITION_START_HOOK",
        "SYNARA_K8S_NODE_PARTITION_VERIFY_HOOK",
        "SYNARA_K8S_NODE_PARTITION_STOP_HOOK",
        "SYNARA_K8S_NODE_PARTITION_HOOK_TIMEOUT_SECONDS",
        "SYNARA_K8S_NODE_PARTITION_START_HOOK_TIMEOUT_SECONDS",
        "SYNARA_K8S_NODE_PARTITION_VERIFY_HOOK_TIMEOUT_SECONDS",
        "SYNARA_K8S_NODE_PARTITION_STOP_HOOK_TIMEOUT_SECONDS",
        "SYNARA_MANAGED_HOOK_CHALLENGE",
        "external adapter point",
        "does not claim real cloud or production validation",
    ):
        if fragment not in contract_text:
            fail(f"kubernetes-resilience-acceptance-v1.md omitted managed hook contract text: {fragment}")
    for fragment in (
        "DeleteOptions.preconditions.uid",
        "immutable UID",
        "read-after-write reconciliation",
        "nonce-bound unlock command",
        "UNLOCKED|true",
        "advisory-lock release caused by connection cleanup is not a successful handshake",
    ):
        if fragment not in normalized_contract_text:
            fail(f"kubernetes-resilience-acceptance-v1.md omitted leader V2 contract text: {fragment}")

    readme_text = (SCRIPT_DIR / "README.md").read_text(encoding="utf-8")
    for fragment in (
        "SYNARA_K8S_NODE_PARTITION_START_HOOK",
        "SYNARA_K8S_NODE_PARTITION_VERIFY_HOOK",
        "SYNARA_K8S_NODE_PARTITION_STOP_HOOK",
        "SYNARA_K8S_NODE_PARTITION_HOOK_TIMEOUT_SECONDS",
        "SYNARA_K8S_NODE_PARTITION_START_HOOK_TIMEOUT_SECONDS",
        "SYNARA_K8S_NODE_PARTITION_VERIFY_HOOK_TIMEOUT_SECONDS",
        "SYNARA_K8S_NODE_PARTITION_STOP_HOOK_TIMEOUT_SECONDS",
        "SYNARA_MANAGED_HOOK_CHALLENGE",
        "external adapter point",
    ):
        if fragment not in readme_text:
            fail(f"deploy/kubernetes/README.md omitted managed hook operator guidance: {fragment}")
    for fragment in (
        "nonce-bound V2 handshake",
        "immutable Pod-UID precondition",
        "UNLOCKED|true",
        "cleanup only breaks the stream",
    ):
        if fragment not in readme_text:
            fail(f"deploy/kubernetes/README.md omitted leader V2 operator guidance: {fragment}")

    with tempfile.TemporaryDirectory(prefix="synara-k8s-leader-guard-") as temp_dir_text:
        leader_temp_dir = pathlib.Path(temp_dir_text)
        split_run, split_payload, split_invocations = run_fake_leader_case(leader_temp_dir, "split")
        if split_run.returncode != 0 or split_payload.get("status") != "passed":
            fail(
                "leader guard must accept a sentinel split across local file reads "
                f"(stdout={split_run.stdout!r}, stderr={split_run.stderr!r}, payload={split_payload!r})"
            )
        split_events = fake_leader_events(leader_temp_dir, "split")
        expected_events = ["LEASE", "READY", "UID_DELETE", "UNLOCK_COMMAND", "UNLOCKED", "EXIT"]
        if split_events != expected_events:
            fail(f"leader V2 handshake event order drifted: {split_events!r}")
        guard_execs = [
            invocation
            for invocation in split_invocations
            if invocation and invocation[0] == "exec"
        ]
        exact_guard_execs = [
            invocation
            for invocation in split_invocations
            if invocation[:3] == ["exec", "-i", "pod/synara-stage2-postgres-0"]
        ]
        if len(exact_guard_execs) != 1:
            fail(f"leader guard must open exactly one exact-Pod stream, got {exact_guard_execs!r}")
        if any("pg_stat_activity" in " ".join(invocation) for invocation in split_invocations):
            fail("leader guard readiness must not poll PostgreSQL through repeated kubectl exec calls")
        if len(guard_execs) > 2:
            fail(f"leader guard produced an unexpected kubectl exec polling flood: {guard_execs!r}")

        for mode, expected_failures in (
            ("eof-before-lease", {"guard-stream-ended-before-ready"}),
            ("eof-between-lease-ready", {"guard-stream-ended-before-ready"}),
            ("eof-after-ready", {"guard-stream-ended-before-delete", "guard-controller-ended-before-delete"}),
            ("partial", {"guard-output-malformed-at-eof"}),
            ("duplicate", {"guard-output-malformed"}),
            ("wrong-nonce", {"guard-output-malformed"}),
            ("wrong-uid", {"guard-output-malformed"}),
            ("out-of-order", {"guard-output-malformed"}),
            ("unknown-sentinel", {"guard-output-malformed"}),
            ("early-unlocked", {"guard-output-malformed"}),
            ("replacement", {"postgres-pod-identity-changed"}),
            ("pg-replacement-before-delete", {"postgres-pod-identity-changed-before-delete"}),
            ("guard-exit-during-exact-get", {"guard-stream-ended-before-delete", "guard-controller-ended-before-delete"}),
            ("watchdog", {"guard-watchdog-timeout"}),
        ):
            failed_run, failed_payload, failed_invocations = run_fake_leader_case(leader_temp_dir, mode)
            if failed_run.returncode == 0 or failed_payload.get("status") != "failed":
                fail(f"leader guard fixture {mode!r} must fail closed")
            scenario = failed_payload.get("scenarios", [{}])[0]
            details = scenario.get("details", {})
            if details.get("guardFailure") not in expected_failures:
                fail(
                    f"leader guard fixture {mode!r} reported {details.get('guardFailure')!r}, "
                    f"expected one of {sorted(expected_failures)!r}"
                )
            if details.get("guardOutputRedacted") is not True or details.get("guardOutputMaxBytes") != 2000:
                fail(f"leader guard fixture {mode!r} omitted bounded redacted diagnostics")
            if any("pg_stat_activity" in " ".join(invocation) for invocation in failed_invocations):
                fail(f"leader guard fixture {mode!r} used forbidden PostgreSQL lock polling")
            if any(invocation[:2] == ["delete", "--raw"] for invocation in failed_invocations):
                fail(f"leader guard fixture {mode!r} deleted the leader after its guard became invalid")
            if "UNLOCK_COMMAND" in fake_leader_events(leader_temp_dir, mode):
                fail(f"leader guard fixture {mode!r} sent explicit unlock after pre-delete failure")

        delayed_run, delayed_payload, _ = run_fake_leader_case(leader_temp_dir, "delay-before-delete")
        if delayed_run.returncode != 0 or delayed_payload.get("status") != "passed":
            fail(f"delayed pre-delete V2 guard must remain locked: {delayed_payload!r}")
        delayed_events = fake_leader_events(leader_temp_dir, "delay-before-delete")
        if delayed_events != expected_events:
            fail(f"delayed pre-delete guard unlocked before immutable-UID delete: {delayed_events!r}")

        for mode, expected_outcome, expected_attempts, expected_observed_state in (
            ("delete-applied-eof", "accepted-after-read-after-write", 1, "original-uid-absent"),
            ("delete-retry", "submitted", 2, None),
            ("delete-replacement", "accepted-after-read-after-write", 1, "replacement-present"),
        ):
            delete_run, delete_payload, delete_invocations = run_fake_leader_case(leader_temp_dir, mode)
            if delete_run.returncode != 0 or delete_payload.get("status") != "passed":
                fail(
                    f"leader UID-delete fixture {mode!r} must pass "
                    f"(stdout={delete_run.stdout!r}, stderr={delete_run.stderr!r}, payload={delete_payload!r})"
                )
            details = delete_payload.get("scenarios", [{}])[0].get("details", {})
            deletion = details.get("deletion", {})
            if details.get("deletedPodUID") != "cp-uid-a":
                fail(f"leader UID-delete fixture {mode!r} omitted the original Pod UID")
            if deletion.get("outcome") != expected_outcome or deletion.get("attempts") != expected_attempts:
                fail(f"leader UID-delete fixture {mode!r} reported incorrect reconciliation: {deletion!r}")
            if expected_observed_state is not None and deletion.get("observedState") != expected_observed_state:
                fail(f"leader UID-delete fixture {mode!r} reported incorrect observed state: {deletion!r}")
            if deletion.get("ambiguous") is not True:
                fail(f"leader UID-delete fixture {mode!r} lost ambiguous-write evidence")
            if mode == "delete-retry":
                if deletion.get("priorResponseClass") != "unexpected-eof" or deletion.get("priorObservedState") != "same-uid-active":
                    fail(f"leader UID-delete retry fixture lost its reconciled ambiguous result: {deletion!r}")
            elif deletion.get("responseClass") != "unexpected-eof":
                fail(f"leader UID-delete fixture {mode!r} did not retain redacted EOF classification: {deletion!r}")
            raw_deletes = [invocation for invocation in delete_invocations if invocation[:2] == ["delete", "--raw"]]
            if len(raw_deletes) != expected_attempts:
                fail(f"leader UID-delete fixture {mode!r} submitted unsafe/unexpected retries: {raw_deletes!r}")
            if mode == "delete-replacement" and deletion.get("replacementUid") != "cp-uid-a-replacement":
                fail("leader replacement reconciliation did not preserve the replacement UID")
            delete_events = fake_leader_events(leader_temp_dir, mode)
            if delete_events.index("UID_DELETE") > delete_events.index("UNLOCK_COMMAND"):
                fail(f"leader UID-delete fixture {mode!r} unlocked before delete acceptance: {delete_events!r}")

        conflict_run, conflict_payload, conflict_invocations = run_fake_leader_case(leader_temp_dir, "delete-conflict")
        if conflict_run.returncode == 0 or conflict_payload.get("status") != "failed":
            fail("leader UID-precondition conflict fixture must fail closed")
        conflict_details = conflict_payload.get("scenarios", [{}])[0].get("details", {})
        conflict_deletion = conflict_details.get("deletion", {})
        if conflict_details.get("deletedPodUID") != "cp-uid-a" or conflict_deletion.get("outcome") != "same-uid-still-active":
            fail(f"leader UID-precondition conflict evidence drifted: {conflict_details!r}")
        if conflict_deletion.get("responseClass") != "precondition-conflict":
            fail(f"leader UID-precondition conflict response was not safely classified: {conflict_deletion!r}")
        conflict_raw_deletes = [
            invocation for invocation in conflict_invocations if invocation[:2] == ["delete", "--raw"]
        ]
        if len(conflict_raw_deletes) != 3:
            fail(f"leader UID-precondition conflict must use three bounded same-UID attempts: {conflict_raw_deletes!r}")
        if "UNLOCK_COMMAND" in fake_leader_events(leader_temp_dir, "delete-conflict"):
            fail("leader UID-precondition conflict sent explicit unlock after rejected delete")

        retry_exit_run, retry_exit_payload, retry_exit_invocations = run_fake_leader_case(
            leader_temp_dir, "delete-retry-guard-exit"
        )
        if retry_exit_run.returncode == 0 or retry_exit_payload.get("status") != "failed":
            fail("leader guard exit before ambiguous retry must fail closed")
        retry_exit_deletes = [item for item in retry_exit_invocations if item[:2] == ["delete", "--raw"]]
        if len(retry_exit_deletes) != 1:
            fail(f"leader guard exit before retry submitted an unsafe retry: {retry_exit_deletes!r}")
        if "UNLOCK_COMMAND" in fake_leader_events(leader_temp_dir, "delete-retry-guard-exit"):
            fail("leader guard exit before retry sent explicit unlock")

        for mode in ("eof-after-delete-before-unlock", "pg-replacement-after-delete"):
            post_delete_run, post_delete_payload, post_delete_invocations = run_fake_leader_case(leader_temp_dir, mode)
            if post_delete_run.returncode == 0 or post_delete_payload.get("status") != "failed":
                fail(f"leader post-delete guard failure {mode!r} must fail closed")
            if len([item for item in post_delete_invocations if item[:2] == ["delete", "--raw"]]) != 1:
                fail(f"leader post-delete guard failure {mode!r} did not preserve exact delete evidence")
            if "UNLOCK_COMMAND" in fake_leader_events(leader_temp_dir, mode):
                fail(f"leader post-delete guard failure {mode!r} sent unlock after guard loss")

        post_unlock_run, post_unlock_payload, _ = run_fake_leader_case(
            leader_temp_dir, "eof-after-unlock-before-sentinel"
        )
        if post_unlock_run.returncode == 0 or post_unlock_payload.get("status") != "failed":
            fail("leader EOF after explicit unlock write but before sentinel must fail")
        post_unlock_events = fake_leader_events(leader_temp_dir, "eof-after-unlock-before-sentinel")
        if "UID_DELETE" not in post_unlock_events or "UNLOCK_COMMAND" not in post_unlock_events or "UNLOCKED" in post_unlock_events:
            fail(f"leader post-unlock EOF evidence order drifted: {post_unlock_events!r}")

    with tempfile.NamedTemporaryFile(prefix="synara-k8s-resilience-", delete=False) as handle:
        evidence_path = pathlib.Path(handle.name)
    try:
        env = os.environ.copy()
        env["SYNARA_K8S_CONTEXT"] = "kind-validation"
        env["SYNARA_K8S_RESILIENCE_EVIDENCE_FILE"] = str(evidence_path)
        subprocess.run(
            ["bash", str(SCRIPT_DIR / "resilience-acceptance.sh"), "--dry-run"],
            check=True,
            env=env,
            capture_output=True,
            text=True,
        )
        payload = json.loads(evidence_path.read_text(encoding="utf-8"))
    finally:
        evidence_path.unlink(missing_ok=True)

    if payload.get("status") != "dry-run":
        fail("resilience dry-run must emit status=dry-run")
    if payload.get("plannedCases") != [
        "rbac",
        "topology",
        "leader-takeover",
        "control-plane-failover",
        "node-drain",
        "node-partition",
    ]:
        fail("resilience dry-run plannedCases drifted unexpectedly")
    soak = payload.get("soak", {})
    if soak.get("plannedCases") != ["control-plane-failover"]:
        fail("resilience dry-run soak plannedCases drifted unexpectedly")
    if payload.get("bootstrapBaseline") is not True:
        fail("resilience dry-run must default bootstrapBaseline=true")
    if payload.get("allowedSkippedCases") != []:
        fail("resilience dry-run must fail closed on skipped required cases by default")
    managed_hook_override = payload.get("safety", {}).get("nodePartitionManagedHookOverride", {})
    if managed_hook_override != {
        "startHookEnvVar": "SYNARA_K8S_NODE_PARTITION_START_HOOK",
        "verifyHookEnvVar": "SYNARA_K8S_NODE_PARTITION_VERIFY_HOOK",
        "stopHookEnvVar": "SYNARA_K8S_NODE_PARTITION_STOP_HOOK",
        "timeoutEnvVar": "SYNARA_K8S_NODE_PARTITION_HOOK_TIMEOUT_SECONDS",
        "startTimeoutEnvVar": "SYNARA_K8S_NODE_PARTITION_START_HOOK_TIMEOUT_SECONDS",
        "verifyTimeoutEnvVar": "SYNARA_K8S_NODE_PARTITION_VERIFY_HOOK_TIMEOUT_SECONDS",
        "stopTimeoutEnvVar": "SYNARA_K8S_NODE_PARTITION_STOP_HOOK_TIMEOUT_SECONDS",
    }:
        fail("resilience dry-run managed hook override metadata drifted unexpectedly")

    with tempfile.NamedTemporaryFile(prefix="synara-k8s-resilience-isolated-", delete=False) as handle:
        isolated_evidence_path = pathlib.Path(handle.name)
    try:
        env = os.environ.copy()
        env["SYNARA_K8S_CONTEXT"] = "kind-validation"
        env["SYNARA_K8S_NAMESPACE"] = "synara-stage4-isolated-validation"
        env["SYNARA_K8S_RESILIENCE_EVIDENCE_FILE"] = str(isolated_evidence_path)
        subprocess.run(
            ["bash", str(SCRIPT_DIR / "resilience-acceptance.sh"), "--dry-run"],
            check=True,
            env=env,
            capture_output=True,
            text=True,
        )
        isolated_payload = json.loads(isolated_evidence_path.read_text(encoding="utf-8"))
    finally:
        isolated_evidence_path.unlink(missing_ok=True)
    if isolated_payload.get("namespace") != "synara-stage4-isolated-validation":
        fail("resilience isolated dry-run lost the selected namespace")
    if isolated_payload.get("rbacName") != "synara-control-plane-reconciler-synara-stage4-isolated-validation":
        fail("resilience isolated dry-run did not derive an isolated ClusterRole name")

    for invalid_timeout in ("0", "-1", "not-a-number"):
        with tempfile.NamedTemporaryFile(prefix="synara-k8s-resilience-invalid-timeout-", delete=False) as handle:
            invalid_timeout_evidence = pathlib.Path(handle.name)
        try:
            env = os.environ.copy()
            env["SYNARA_K8S_CONTEXT"] = "kind-validation"
            env["SYNARA_K8S_RESILIENCE_EVIDENCE_FILE"] = str(invalid_timeout_evidence)
            env["SYNARA_K8S_NODE_PARTITION_VERIFY_HOOK_TIMEOUT_SECONDS"] = invalid_timeout
            invalid_timeout_run = subprocess.run(
                ["bash", str(SCRIPT_DIR / "resilience-acceptance.sh"), "--dry-run"],
                env=env,
                capture_output=True,
                text=True,
            )
        finally:
            invalid_timeout_evidence.unlink(missing_ok=True)
        if invalid_timeout_run.returncode == 0:
            fail(f"verify-hook timeout {invalid_timeout!r} must fail closed")
        if "SYNARA_K8S_NODE_PARTITION_VERIFY_HOOK_TIMEOUT_SECONDS" not in invalid_timeout_run.stderr:
            fail(f"verify-hook timeout {invalid_timeout!r} omitted the invalid setting name")

    soak_without_evidence_env = os.environ.copy()
    soak_without_evidence_env["SYNARA_K8S_CONTEXT"] = "kind-validation"
    soak_without_evidence_env["SYNARA_K8S_RESILIENCE_SOAK_SECONDS"] = "60"
    soak_without_evidence_env.pop("SYNARA_K8S_RESILIENCE_EVIDENCE_FILE", None)
    soak_without_evidence = subprocess.run(
        ["bash", str(SCRIPT_DIR / "resilience-acceptance.sh"), "--dry-run"],
        env=soak_without_evidence_env,
        capture_output=True,
        text=True,
    )
    if soak_without_evidence.returncode == 0:
        fail("resilience dry-run must refuse soak without an explicit evidence path")
    if "SYNARA_K8S_RESILIENCE_EVIDENCE_FILE must be set explicitly when soak is enabled" not in soak_without_evidence.stderr:
        fail("resilience dry-run must explain the explicit evidence-path requirement for soak")

    with tempfile.NamedTemporaryFile(prefix="synara-k8s-resilience-soak-", delete=False) as handle:
        soak_evidence_path = pathlib.Path(handle.name)
    try:
        env = os.environ.copy()
        env["SYNARA_K8S_CONTEXT"] = "kind-validation"
        env["SYNARA_K8S_RESILIENCE_EVIDENCE_FILE"] = str(soak_evidence_path)
        env["SYNARA_K8S_RESILIENCE_SOAK_SECONDS"] = "60"
        subprocess.run(
            ["bash", str(SCRIPT_DIR / "resilience-acceptance.sh"), "--dry-run"],
            check=True,
            env=env,
            capture_output=True,
            text=True,
        )
        soak_payload = json.loads(soak_evidence_path.read_text(encoding="utf-8"))
    finally:
        soak_evidence_path.unlink(missing_ok=True)

    soak = soak_payload.get("soak", {})
    if soak.get("enabled") is not True or soak.get("durationSeconds") != 60:
        fail("resilience dry-run soak configuration drifted unexpectedly")

    with tempfile.NamedTemporaryFile(prefix="synara-k8s-resilience-managed-", delete=False) as handle:
        managed_evidence_path = pathlib.Path(handle.name)
    try:
        env = os.environ.copy()
        env["SYNARA_K8S_CONTEXT"] = "managed-validation"
        env["SYNARA_K8S_ACCEPTANCE_ALLOW_NONDISPOSABLE"] = "1"
        env["SYNARA_K8S_RESILIENCE_EVIDENCE_FILE"] = str(managed_evidence_path)
        subprocess.run(
            ["bash", str(SCRIPT_DIR / "resilience-acceptance.sh"), "--dry-run"],
            check=True,
            env=env,
            capture_output=True,
            text=True,
        )
        managed_payload = json.loads(managed_evidence_path.read_text(encoding="utf-8"))
    finally:
        managed_evidence_path.unlink(missing_ok=True)

    if managed_payload.get("plannedCases") != payload.get("plannedCases"):
        fail("resilience managed dry-run plannedCases drifted unexpectedly")
    if managed_payload.get("safety", {}).get("allowNonDisposableOverride") != "SYNARA_K8S_ACCEPTANCE_ALLOW_NONDISPOSABLE":
        fail("resilience managed dry-run lost allowNonDisposableOverride metadata")
    if managed_payload.get("safety", {}).get("nodePartitionManagedHookOverride") != managed_hook_override:
        fail("resilience managed dry-run lost managed hook override metadata")

    controller_tests = subprocess.run(
        [sys.executable, str(SCRIPT_DIR / "test_managed_hook_controller.py")],
        capture_output=True,
        text=True,
        check=False,
        timeout=20,
    )
    if controller_tests.returncode != 0:
        fail(
            "managed hook controller focused tests failed: "
            f"stdout={controller_tests.stdout!r} stderr={controller_tests.stderr!r}"
        )

    with tempfile.TemporaryDirectory(prefix="synara-k8s-managed-hooks-") as temp_dir_text:
        temp_dir = pathlib.Path(temp_dir_text)

        normal_run, normal_payload, normal_log, _ = run_fake_managed_case(
            temp_dir,
            "normal",
            verify_mode="inspect-fds",
            stop_mode="zombie",
        )
        if normal_run.returncode != 0 or normal_payload.get("status") != "passed":
            fail(
                "managed controller normal case failed: "
                f"rc={normal_run.returncode} stdout={normal_run.stdout!r} stderr={normal_run.stderr!r}"
            )
        normal_details = normal_payload.get("scenarios", [{}])[0].get("details", {})
        serialized_managed_details = json.dumps(normal_details, sort_keys=True)
        for secret_scope in ("safe-worker", "node-uid-safe-worker", "synara-control-plane-a", "pod-uid-a"):
            if secret_scope in serialized_managed_details:
                fail(f"managed case evidence leaked raw target scope: {secret_scope}")
        phases = [
            normal_details.get("startHook", {}),
            normal_details.get("verifyHook", {}),
            normal_details.get("stopHook", {}),
        ]
        if any(item.get("backend") != "process-group-test" for item in phases):
            fail("managed-validation must use only the explicit process-group-test backend")
        if any(item.get("testOnly") is not True or item.get("securityBoundary") is not False for item in phases):
            fail("managed-validation process groups were represented as production security evidence")
        controllers = [item.get("controller", {}) for item in phases]
        if [item.get("status") for item in controllers] != ["passed", "passed", "passed"]:
            fail("managed controller normal case omitted passed start/verify/stop controller results")
        operation_digests = {item.get("operationIdDigest") for item in controllers}
        if len(operation_digests) != 1 or None in operation_digests:
            fail("managed phases were not bound to one runner-generated operationId")
        if "provider-check-secret" in json.dumps(normal_payload, sort_keys=True):
            fail("managed controller leaked hook-controlled check text")
        fd_lines = [line for line in normal_log.splitlines() if line.startswith("fds:")]
        if not fd_lines or any(
            fragment in fd_lines[0]
            for fragment in ("managed-verify-request", "evidence.json", ".journal.jsonl", ".partial.json")
        ):
            fail("managed hook inherited a runner request/evidence descriptor")

        soak_run, soak_payload, _, _ = run_fake_managed_case(temp_dir, "managed-soak", soak=True)
        if soak_run.returncode != 0 or not soak_payload.get("soak", {}).get("cycles"):
            fail("managed node-partition soak fixture did not complete a cycle")
        soak_case_dir = temp_dir / "managed-soak"
        partial_payload = json.loads((soak_case_dir / "evidence.json.partial.json").read_text(encoding="utf-8"))
        journal_lines = [json.loads(line) for line in (soak_case_dir / "evidence.json.journal.jsonl").read_text(encoding="utf-8").splitlines()]
        persisted_soak = json.dumps(
            {
                "final": soak_payload.get("soak", {}).get("cycles"),
                "partial": partial_payload.get("soak", {}).get("cycles"),
                "journal": [
                    item.get("payload", {}).get("details")
                    for item in journal_lines if item.get("event") == "soak-cycle-completed"
                ],
            },
            sort_keys=True,
        )
        for leaked in (
            "managed-validation", "synara-system", "safe-worker", "node-uid-safe-worker",
            "synara-control-plane-a", "pod-uid-a", "provider-check-secret", "must-not-persist",
        ):
            if leaked in persisted_soak:
                fail(f"managed soak sidecar/final evidence leaked raw scope or secret: {leaked}")

        for prehook_mode in ("get-node-failed", "uid-missing", "malformed-target"):
            pre_run, pre_payload, _, _ = run_fake_managed_case(
                temp_dir, f"prehook-{prehook_mode}", prehook_mode=prehook_mode
            )
            if pre_run.returncode == 0 or pre_payload.get("status") != "failed":
                fail(f"managed pre-hook fixture {prehook_mode!r} must fail closed")
            pre_dir = temp_dir / f"prehook-{prehook_mode}"
            pre_partial = json.loads((pre_dir / "evidence.json.partial.json").read_text(encoding="utf-8"))
            pre_journal = [json.loads(line) for line in (pre_dir / "evidence.json.journal.jsonl").read_text(encoding="utf-8").splitlines()]
            persisted = json.dumps(
                {
                    "final": pre_payload.get("scenarios"),
                    "partial": pre_partial.get("scenarios"),
                    "journal": [item.get("payload", {}).get("details") for item in pre_journal],
                }, sort_keys=True,
            )
            for leaked in (
                "safe-worker", "safe-worker-secret", "node-uid-safe-worker",
                "synara-control-plane-a", "pod-uid-a", "rawTarget",
                "managed-validation", "synara-system",
            ):
                if leaked in persisted:
                    fail(f"managed pre-hook {prehook_mode!r} leaked raw scope in persisted details: {leaked}")

        skip_run, skip_payload, _, _ = run_fake_managed_case(
            temp_dir, "empty-allowlist-skip", prehook_mode="empty-allowlist-skip"
        )
        if skip_run.returncode == 0 or skip_payload.get("status") != "failed":
            fail("required skip with an empty allowlist must emit failed evidence")
        if "unbound variable" in skip_run.stderr or skip_payload.get("scenarios", [{}])[0].get("status") != "skipped":
            fail("empty skip allowlist regressed to Bash-3 nounset/unexpected exit")

        for controller_mode in (
            "failure-terminated", "wrong-operation", "wrong-scope", "wrong-unit",
            "wrong-phase", "exit0-failed", "extra-secret", "replayed-recovery",
            "nonzero-passed",
            "fast-exit",
        ):
            bad_run, bad_payload, bad_log, _ = run_fake_managed_case(
                temp_dir,
                f"controller-{controller_mode}",
                controller_mode=controller_mode,
            )
            if bad_run.returncode == 0 or bad_payload.get("status") != "failed":
                fail(f"malformed/replayed controller result {controller_mode!r} must fail closed")
            if controller_mode == "failure-terminated" and "recover:start" not in bad_log:
                fail("nonzero execute with terminationConfirmed=true skipped exact recovery")
            if "stop:normal" not in bad_log:
                fail(f"controller result rejection {controller_mode!r} did not attempt safe heal")
            if "must-not-persist" in json.dumps(bad_payload, sort_keys=True):
                fail("unvalidated extra controller field leaked into evidence")

        identity_run, identity_payload, identity_log, _ = run_fake_managed_case(
            temp_dir, "controller-identity-failure", identity_failure=True
        )
        if identity_run.returncode == 0 or identity_payload.get("status") != "failed":
            fail("controller launcher identity failure must fail closed")
        if "start:normal" in identity_log or "stop:normal" not in identity_log:
            fail("identity-failed launcher executed start or failed to attempt later safe heal")
        if not (temp_dir / "controller-identity-failure" / "identity-failure.once").exists():
            fail("controller identity failure injection did not reach launcher handshake")

        mismatch_run, mismatch_payload, mismatch_log, _ = run_fake_managed_case(
            temp_dir,
            "operation-mismatch",
            verify_mode="mismatch-operation",
        )
        if mismatch_run.returncode == 0 or mismatch_payload.get("status") != "failed":
            fail("managed operation mismatch must fail closed")
        mismatch_details = mismatch_payload.get("scenarios", [{}])[0].get("details", {})
        if mismatch_details.get("verifyHook", {}).get("controller", {}).get("reason") != "transition-artifact-scope-mismatch":
            fail("managed operation mismatch lost the controller scope failure")
        if "stop:normal" not in mismatch_log:
            fail("managed operation mismatch did not run terminal-healed stop")

        pending_run, pending_payload, pending_log, _ = run_fake_managed_case(
            temp_dir,
            "async-pending",
            verify_mode="pending",
        )
        if pending_run.returncode == 0 or pending_payload.get("status") != "failed":
            fail("managed pending transition must fail closed")
        if "stop:normal" not in pending_log:
            fail("managed pending transition did not heal")

        timeout_run, timeout_payload, timeout_log, _ = run_fake_managed_case(
            temp_dir,
            "timeout-heal",
            start_mode="sleep",
            start_timeout=1,
        )
        if timeout_run.returncode == 0 or timeout_payload.get("status") != "failed":
            fail("managed timeout must fail closed")
        timeout_details = timeout_payload.get("scenarios", [{}])[0].get("details", {})
        if timeout_details.get("startHook", {}).get("controller", {}).get("timedOut") is not True:
            fail("managed timeout lost controller timedOut evidence")
        if "stop:normal" not in timeout_log:
            fail("managed timeout did not run terminal-healed stop")

        missing_heal_run, missing_heal_payload, _, _ = run_fake_managed_case(
            temp_dir,
            "missing-heal",
            stop_mode="missing",
        )
        if missing_heal_run.returncode == 0 or missing_heal_payload.get("status") != "failed":
            fail("managed missing terminal-healed artifact must fail closed")
        missing_details = missing_heal_payload.get("scenarios", [{}])[0].get("details", {})
        if missing_details.get("stopHook", {}).get("controller", {}).get("reason") != "transition-artifact-unavailable":
            fail("managed missing heal lost controller failure evidence")

        setsid_run, setsid_payload, setsid_log, late_marker = run_fake_managed_case(
            temp_dir,
            "setsid-test-only",
            start_mode="setsid-late",
        )
        if setsid_run.returncode == 0 or setsid_payload.get("status") != "failed":
            fail("test-only setsid escape must never claim managed success")
        deadline = time.monotonic() + 2
        while not late_marker.exists() and time.monotonic() < deadline:
            time.sleep(0.02)
        if not late_marker.exists() or "stop:normal" not in setsid_log:
            fail("test-only setsid case did not document escape plus cleanup heal")

    sys.stdout.write("Python resilience validation passed\n")


if __name__ == "__main__":
    main()
