#!/usr/bin/env python3
from __future__ import annotations

import json
import os
import pathlib
import re
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
        import sys

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
                "metadata": {"name": "synara-control-plane-a", "deletionTimestamp": None},
                "spec": {"nodeName": "safe-worker"},
                "status": {
                    "phase": "Running",
                    "conditions": [{"type": "Ready", "status": "True"}],
                },
            },
            {
                "metadata": {"name": "synara-control-plane-b", "deletionTimestamp": None},
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
            "metadata": {"name": "safe-worker"},
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
    start_hook_script: str,
    stop_hook_script: str,
    start_hook_timeout_seconds: int = 3,
    stop_hook_timeout_seconds: int = 10,
    partition_seconds: int = 1,
) -> tuple[subprocess.CompletedProcess[str], dict, str, pathlib.Path]:
    case_dir = temp_dir / label
    case_dir.mkdir(parents=True, exist_ok=True)
    fake_bin = case_dir / "bin"
    fake_bin.mkdir(exist_ok=True)
    make_fake_kubectl(fake_bin / "kubectl")

    start_hook_path = case_dir / "start-hook.sh"
    stop_hook_path = case_dir / "stop-hook.sh"
    hook_log_path = case_dir / "hook.log"
    child_pid_path = case_dir / "child.pid"
    evidence_path = case_dir / "evidence.json"
    write_executable(start_hook_path, start_hook_script)
    write_executable(stop_hook_path, stop_hook_script)

    env = os.environ.copy()
    env["PATH"] = f"{fake_bin}{os.pathsep}{env.get('PATH', '')}"
    env["HOOK_LOG_FILE"] = str(hook_log_path)
    env["CHILD_PID_FILE"] = str(child_pid_path)
    env["SYNARA_K8S_CONTEXT"] = "managed-validation"
    env["SYNARA_K8S_ACCEPTANCE_ALLOW_NONDISPOSABLE"] = "1"
    env["SYNARA_K8S_RESILIENCE_BOOTSTRAP_BASELINE"] = "0"
    env["SYNARA_K8S_RESILIENCE_CASES"] = "node-partition"
    env["SYNARA_K8S_RESILIENCE_PARTITION_NODE"] = "safe-worker"
    env["SYNARA_K8S_RESILIENCE_PARTITION_SECONDS"] = str(partition_seconds)
    env["SYNARA_K8S_RESILIENCE_DISRUPTION_WINDOW_SECONDS"] = "1"
    env["SYNARA_K8S_RESILIENCE_PROBE_INTERVAL_SECONDS"] = "1"
    env["SYNARA_K8S_RESILIENCE_CASE_TIMEOUT_SECONDS"] = "5"
    env["SYNARA_K8S_NODE_PARTITION_HOOK_TIMEOUT_SECONDS"] = "30"
    env["SYNARA_K8S_NODE_PARTITION_START_HOOK_TIMEOUT_SECONDS"] = str(start_hook_timeout_seconds)
    env["SYNARA_K8S_NODE_PARTITION_STOP_HOOK_TIMEOUT_SECONDS"] = str(stop_hook_timeout_seconds)
    env["SYNARA_K8S_NODE_PARTITION_START_HOOK"] = str(start_hook_path)
    env["SYNARA_K8S_NODE_PARTITION_STOP_HOOK"] = str(stop_hook_path)
    env["SYNARA_K8S_RESILIENCE_EVIDENCE_FILE"] = str(evidence_path)

    completed = subprocess.run(
        ["bash", str(SCRIPT_DIR / "resilience-acceptance.sh")],
        env=env,
        capture_output=True,
        text=True,
    )
    if not evidence_path.exists():
        fail(f"{label}: resilience run did not write evidence")
    payload = json.loads(evidence_path.read_text(encoding="utf-8"))
    hook_log = hook_log_path.read_text(encoding="utf-8") if hook_log_path.exists() else ""
    return completed, payload, hook_log, child_pid_path


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
        fail("acceptance.sh must wait for asynchronous cleanup before recreating its fixed namespace")
    if "already exists and is not terminating; refusing to reuse it" not in acceptance_text:
        fail("acceptance.sh must refuse to overwrite a live fixed namespace")
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
        "leader-takeover",
        "synara:kubernetes-execution-reconciler",
        "fencing_token",
        "pg_advisory_lock",
        "PGAPPNAME",
        "pg_stat_activity",
        "start_continuous_probe",
        "wait_for_reconciler_takeover",
    ):
        if fragment not in resilience_text:
            fail(f"resilience-acceptance.sh omitted leader takeover evidence: {fragment}")
    if ":'lease_name'" in resilience_text:
        fail("resilience-acceptance.sh must not use unsupported psql variable interpolation inside -c")
    if "set +e" in resilience_text:
        fail("resilience-acceptance.sh must capture expected failures without mutating global errexit state")
    for fragment in (
        "final_report_emitted",
        "unexpected_exit_status",
        "readiness probe process failed during control plane failover",
        "readiness probe process failed during drain simulation",
        "readiness probe process failed during node partition",
        "for attempt in 1 2 3",
    ):
        if fragment not in resilience_text:
            fail(f"resilience-acceptance.sh omitted fail-closed harness behavior: {fragment}")
    for fragment in (
        'journal_file="${evidence_file}.journal.jsonl"',
        'partial_file="${evidence_file}.partial.json"',
        "write_partial_snapshot",
        "record_progress_update",
        "commandRedacted: true",
        "SYNARA_K8S_RESILIENCE_EVIDENCE_FILE must be set explicitly when soak is enabled",
        "SYNARA_K8S_NODE_PARTITION_START_HOOK",
        "SYNARA_K8S_NODE_PARTITION_STOP_HOOK",
        "SYNARA_K8S_NODE_PARTITION_HOOK_TIMEOUT_SECONDS",
        "SYNARA_K8S_NODE_PARTITION_START_HOOK_TIMEOUT_SECONDS",
        "SYNARA_K8S_NODE_PARTITION_STOP_HOOK_TIMEOUT_SECONDS",
        'node_partition_start_hook_timeout_seconds="${SYNARA_K8S_NODE_PARTITION_START_HOOK_TIMEOUT_SECONDS:-$node_partition_hook_timeout_seconds}"',
        'node_partition_stop_hook_timeout_seconds="${SYNARA_K8S_NODE_PARTITION_STOP_HOOK_TIMEOUT_SECONDS:-$node_partition_hook_timeout_seconds}"',
        "stop_managed_partition_hook",
        "node partition for non-Kind contexts requires explicit",
        "managed node partition stop hook failed",
    ):
        if fragment not in resilience_text:
            fail(f"resilience-acceptance.sh omitted durable progress evidence behavior: {fragment}")

    contract_text = CONTRACT_PATH.read_text(encoding="utf-8")
    for fragment in (
        "SYNARA_K8S_NODE_PARTITION_START_HOOK",
        "SYNARA_K8S_NODE_PARTITION_STOP_HOOK",
        "SYNARA_K8S_NODE_PARTITION_HOOK_TIMEOUT_SECONDS",
        "SYNARA_K8S_NODE_PARTITION_START_HOOK_TIMEOUT_SECONDS",
        "SYNARA_K8S_NODE_PARTITION_STOP_HOOK_TIMEOUT_SECONDS",
        "external adapter point",
        "does not claim real cloud or production validation",
    ):
        if fragment not in contract_text:
            fail(f"kubernetes-resilience-acceptance-v1.md omitted managed hook contract text: {fragment}")

    readme_text = (SCRIPT_DIR / "README.md").read_text(encoding="utf-8")
    for fragment in (
        "SYNARA_K8S_NODE_PARTITION_START_HOOK",
        "SYNARA_K8S_NODE_PARTITION_STOP_HOOK",
        "SYNARA_K8S_NODE_PARTITION_HOOK_TIMEOUT_SECONDS",
        "SYNARA_K8S_NODE_PARTITION_START_HOOK_TIMEOUT_SECONDS",
        "SYNARA_K8S_NODE_PARTITION_STOP_HOOK_TIMEOUT_SECONDS",
        "external adapter point",
    ):
        if fragment not in readme_text:
            fail(f"deploy/kubernetes/README.md omitted managed hook operator guidance: {fragment}")

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
        "stopHookEnvVar": "SYNARA_K8S_NODE_PARTITION_STOP_HOOK",
        "timeoutEnvVar": "SYNARA_K8S_NODE_PARTITION_HOOK_TIMEOUT_SECONDS",
        "startTimeoutEnvVar": "SYNARA_K8S_NODE_PARTITION_START_HOOK_TIMEOUT_SECONDS",
        "stopTimeoutEnvVar": "SYNARA_K8S_NODE_PARTITION_STOP_HOOK_TIMEOUT_SECONDS",
    }:
        fail("resilience dry-run managed hook override metadata drifted unexpectedly")

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

    with tempfile.TemporaryDirectory(prefix="synara-k8s-managed-hooks-") as temp_dir_text:
        temp_dir = pathlib.Path(temp_dir_text)

        timeout_run, timeout_payload, timeout_log, timeout_child_pid_path = run_fake_managed_case(
            temp_dir,
            "timeout-heal",
            # The timeout starts at process creation, before the hook can
            # publish its child PID. Keep this validation-only window wide
            # enough for heavily loaded CI hosts while remaining bounded.
            start_hook_timeout_seconds=10,
            stop_hook_timeout_seconds=30,
            start_hook_script="""
            #!/usr/bin/env bash
            set -euo pipefail
            printf 'start:%s:%s\n' "$SYNARA_K8S_NODE_PARTITION_PHASE" "$SYNARA_K8S_NODE_PARTITION_REASON" >>"$HOOK_LOG_FILE"
            sleep 30 &
            child_pid=$!
            printf '%s\n' "$child_pid" >"$CHILD_PID_FILE"
            wait "$child_pid"
            """,
            stop_hook_script="""
            #!/usr/bin/env bash
            set -euo pipefail
            printf 'stop:%s:%s\n' "$SYNARA_K8S_NODE_PARTITION_PHASE" "$SYNARA_K8S_NODE_PARTITION_REASON" >>"$HOOK_LOG_FILE"
            exit 0
            """,
        )
        if timeout_run.returncode == 0:
            fail("managed timeout-heal case must fail closed")
        timeout_scenario = timeout_payload.get("scenarios", [{}])[0]
        timeout_details = timeout_scenario.get("details", {})
        if timeout_payload.get("status") != "failed" or timeout_scenario.get("status") != "failed":
            fail("managed timeout-heal case must record failed case/final status")
        if timeout_details.get("startHook", {}).get("timedOut") is not True:
            fail("managed timeout-heal case must record timedOut start hook evidence")
        if timeout_details.get("startHook", {}).get("timeoutSeconds") != 10:
            fail("managed timeout-heal case must record the independent start-hook timeout")
        if timeout_details.get("stopHook", {}).get("reason") != "start-failed":
            fail("managed timeout-heal case must heal with stop reason=start-failed")
        if timeout_details.get("stopHook", {}).get("timeoutSeconds") != 30:
            fail("managed timeout-heal case must record the independent stop-hook timeout")
        timeout_stop_exit_code = timeout_details.get("stopHook", {}).get("exitCode")
        if timeout_stop_exit_code != 0:
            fail(
                "managed timeout-heal case must record successful cleanup heal "
                f"(stop exitCode={timeout_stop_exit_code!r})"
            )
        if "stop:stop:start-failed" not in timeout_log:
            fail("managed timeout-heal case did not invoke stop hook after start failure")
        if not timeout_child_pid_path.exists():
            fail("managed timeout-heal case did not record child pid for timeout cleanup check")
        timeout_child_pid = int(timeout_child_pid_path.read_text(encoding="utf-8").strip())
        if not wait_for_pid_exit(timeout_child_pid):
            fail("managed timeout-heal case left timed-out hook child process running")

        cleanup_run, cleanup_payload, cleanup_log, _ = run_fake_managed_case(
            temp_dir,
            "cleanup-exit",
            start_hook_script="""
            #!/usr/bin/env bash
            set -euo pipefail
            printf 'start:%s:%s\n' "$SYNARA_K8S_NODE_PARTITION_PHASE" "$SYNARA_K8S_NODE_PARTITION_REASON" >>"$HOOK_LOG_FILE"
            (
              sleep 0.2
              kill -TERM "$SYNARA_K8S_NODE_PARTITION_RUNNER_PID"
            ) &
            exit 0
            """,
            stop_hook_script="""
            #!/usr/bin/env bash
            set -euo pipefail
            printf 'stop:%s:%s\n' "$SYNARA_K8S_NODE_PARTITION_PHASE" "$SYNARA_K8S_NODE_PARTITION_REASON" >>"$HOOK_LOG_FILE"
            exit 0
            """,
            partition_seconds=2,
        )
        if cleanup_run.returncode == 0:
            fail("managed cleanup-exit case must fail after forced interruption")
        cleanup_scenario = cleanup_payload.get("scenarios", [{}])[0]
        cleanup_details = cleanup_scenario.get("details", {})
        if cleanup_payload.get("status") != "failed" or cleanup_scenario.get("status") != "failed":
            fail("managed cleanup-exit case must record failed case/final status")
        if cleanup_details.get("error") != "node partition case exited before completion":
            fail("managed cleanup-exit case must preserve explicit interrupted-case evidence")
        if cleanup_details.get("stopHook", {}).get("reason") != "cleanup-exit":
            fail("managed cleanup-exit case must invoke stop hook with cleanup-exit reason")
        if cleanup_details.get("stopHook", {}).get("exitCode") != 0:
            fail("managed cleanup-exit case must record successful cleanup-exit stop hook")
        if "stop:stop:cleanup-exit" not in cleanup_log:
            fail("managed cleanup-exit case did not execute cleanup-exit stop hook")

        stop_fail_run, stop_fail_payload, stop_fail_log, _ = run_fake_managed_case(
            temp_dir,
            "stop-failure",
            start_hook_script="""
            #!/usr/bin/env bash
            set -euo pipefail
            printf 'start:%s:%s\n' "$SYNARA_K8S_NODE_PARTITION_PHASE" "$SYNARA_K8S_NODE_PARTITION_REASON" >>"$HOOK_LOG_FILE"
            exit 0
            """,
            stop_hook_script="""
            #!/usr/bin/env bash
            set -euo pipefail
            printf 'stop:%s:%s\n' "$SYNARA_K8S_NODE_PARTITION_PHASE" "$SYNARA_K8S_NODE_PARTITION_REASON" >>"$HOOK_LOG_FILE"
            exit 17
            """,
        )
        if stop_fail_run.returncode == 0:
            fail("managed stop-failure case must fail closed")
        stop_fail_scenario = stop_fail_payload.get("scenarios", [{}])[0]
        stop_fail_details = stop_fail_scenario.get("details", {})
        if stop_fail_payload.get("status") != "failed" or stop_fail_scenario.get("status") != "failed":
            fail("managed stop-failure case must record failed case/final status")
        if stop_fail_details.get("error") != "managed node partition stop hook failed":
            fail("managed stop-failure case must preserve explicit stop-hook failure evidence")
        if stop_fail_details.get("stopHook", {}).get("exitCode") != 17:
            fail("managed stop-failure case must retain failing stop-hook exitCode")
        if "stop:stop:case-complete" not in stop_fail_log:
            fail("managed stop-failure case did not execute stop hook with case-complete reason")

    sys.stdout.write("Python resilience validation passed\n")


if __name__ == "__main__":
    main()
