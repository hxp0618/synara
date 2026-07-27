#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
context="${SYNARA_K8S_CONTEXT:-$(kubectl config current-context 2>/dev/null || true)}"
namespace="${SYNARA_K8S_NAMESPACE:-synara-system}"
rbac_name="${SYNARA_K8S_ACCEPTANCE_RBAC_NAME:-}"
if [[ -z "$rbac_name" ]]; then
  if [[ "$namespace" == "synara-system" ]]; then
    rbac_name="synara-control-plane-reconciler"
  else
    rbac_name="synara-control-plane-reconciler-$namespace"
  fi
fi
acceptance_owner="${SYNARA_K8S_ACCEPTANCE_OWNER:-resilience-$(date +%s)-$$}"
baseline_script="${SYNARA_K8S_RESILIENCE_BASELINE_SCRIPT:-$script_dir/acceptance.sh}"
bootstrap_baseline="${SYNARA_K8S_RESILIENCE_BOOTSTRAP_BASELINE:-1}"
session_authority_mode="${SYNARA_K8S_RESILIENCE_SESSION_AUTHORITY_MODE:-}"
if [[ -z "$session_authority_mode" ]]; then
  if [[ "$bootstrap_baseline" == "1" ]]; then
    session_authority_mode="stage2-postgres"
  else
    session_authority_mode="disabled"
  fi
fi
resource_lifecycle_worker_image="${SYNARA_K8S_RESILIENCE_WORKER_IMAGE:-}"
resource_lifecycle_worker_namespace="${SYNARA_K8S_RESILIENCE_WORKER_NAMESPACE:-synara-lifecycle-$(date +%s)-$$}"
resource_lifecycle_runner="$script_dir/resource-lifecycle-acceptance.py"
cases_csv="${SYNARA_K8S_RESILIENCE_CASES:-rbac,topology,leader-takeover,control-plane-failover,node-drain,node-partition}"
allow_skipped_cases_csv="${SYNARA_K8S_RESILIENCE_ALLOW_SKIPPED_CASES:-}"
soak_cases_csv="${SYNARA_K8S_RESILIENCE_SOAK_CASES:-control-plane-failover}"
soak_seconds="${SYNARA_K8S_RESILIENCE_SOAK_SECONDS:-0}"
soak_interval_seconds="${SYNARA_K8S_RESILIENCE_SOAK_INTERVAL_SECONDS:-30}"
probe_interval_seconds="${SYNARA_K8S_RESILIENCE_PROBE_INTERVAL_SECONDS:-2}"
disruption_probe_window_seconds="${SYNARA_K8S_RESILIENCE_DISRUPTION_WINDOW_SECONDS:-30}"
partition_seconds="${SYNARA_K8S_RESILIENCE_PARTITION_SECONDS:-20}"
node_partition_hook_timeout_seconds="${SYNARA_K8S_NODE_PARTITION_HOOK_TIMEOUT_SECONDS:-30}"
node_partition_start_hook_timeout_seconds="${SYNARA_K8S_NODE_PARTITION_START_HOOK_TIMEOUT_SECONDS:-$node_partition_hook_timeout_seconds}"
node_partition_verify_hook_timeout_seconds="${SYNARA_K8S_NODE_PARTITION_VERIFY_HOOK_TIMEOUT_SECONDS:-$node_partition_hook_timeout_seconds}"
node_partition_stop_hook_timeout_seconds="${SYNARA_K8S_NODE_PARTITION_STOP_HOOK_TIMEOUT_SECONDS:-$node_partition_hook_timeout_seconds}"
case_timeout_seconds="${SYNARA_K8S_RESILIENCE_CASE_TIMEOUT_SECONDS:-240}"
min_worker_nodes="${SYNARA_K8S_RESILIENCE_MIN_WORKER_NODES:-3}"
max_failover_ready_failures="${SYNARA_K8S_RESILIENCE_MAX_FAILOVER_READY_FAILURES:-0}"
max_drain_ready_failures="${SYNARA_K8S_RESILIENCE_MAX_DRAIN_READY_FAILURES:-0}"
max_partition_ready_failures="${SYNARA_K8S_RESILIENCE_MAX_PARTITION_READY_FAILURES:-2}"
node_partition_start_hook="${SYNARA_K8S_NODE_PARTITION_START_HOOK:-}"
node_partition_verify_hook="${SYNARA_K8S_NODE_PARTITION_VERIFY_HOOK:-}"
node_partition_stop_hook="${SYNARA_K8S_NODE_PARTITION_STOP_HOOK:-}"
dry_run="${SYNARA_K8S_RESILIENCE_DRY_RUN:-0}"
keep_resources="${SYNARA_K8S_KEEP_RESOURCES:-0}"
allow_non_disposable="${SYNARA_K8S_ACCEPTANCE_ALLOW_NONDISPOSABLE:-0}"
if [[ -n "${SYNARA_K8S_RESILIENCE_EVIDENCE_FILE:-}" ]]; then
  evidence_file="$SYNARA_K8S_RESILIENCE_EVIDENCE_FILE"
  evidence_file_is_explicit=1
else
  implicit_evidence_dir="$(mktemp -d "${TMPDIR:-/tmp}/synara-k8s-resilience-XXXXXX")"
  evidence_file="$implicit_evidence_dir/evidence.json"
  evidence_file_is_explicit=0
fi
journal_file="${evidence_file}.journal.jsonl"
partial_file="${evidence_file}.partial.json"
for reserved_evidence_path in "$evidence_file" "$journal_file" "$partial_file"; do
  if [[ -e "$reserved_evidence_path" ]]; then
    printf 'Refusing to overwrite existing Kubernetes resilience evidence path: %s\n' \
      "$reserved_evidence_path" >&2
    exit 1
  fi
done
work_dir="$(mktemp -d)"
permissions_file="$work_dir/permissions.jsonl"
scenarios_file="$work_dir/scenarios.jsonl"
soak_cycles_file="$work_dir/soak-cycles.jsonl"
partition_verification_file="$work_dir/node-partition-verification.json"
managed_hook_controller="${SYNARA_K8S_MANAGED_HOOK_CONTROLLER:-$script_dir/managed-hook-controller.py}"
if [[ "$managed_hook_controller" != "$script_dir/managed-hook-controller.py" && "$context" != "managed-validation" ]]; then
  printf 'SYNARA_K8S_MANAGED_HOOK_CONTROLLER is test-only and requires managed-validation\n' >&2
  exit 1
fi
touch "$permissions_file" "$scenarios_file" "$soak_cycles_file"

acceptance_resource_owner() {
  local resource="$1"
  if [[ -z "$context" ]]; then
    return 0
  fi
  kubectl --context "$context" get "$resource" \
    -o go-template='{{index .metadata.labels "synara.ai/acceptance-owner"}}' 2>/dev/null || true
}

started_at="$(date -u +"%Y-%m-%dT%H:%M:%SZ")"
started_epoch="$(date +%s)"
baseline_status="skipped"
baseline_duration_seconds=0
created_baseline=0
overall_failed=0
reporting_ready=0
final_report_emitted=0
unexpected_exit_status=0
topology_json='null'
soak_json='{"executed":false,"status":"skipped","reason":"disabled"}'
CASE_DETAILS_JSON='{}'
current_phase_json='{"type":"startup"}'
last_progress_json='null'
ACTIVE_CASE_NAME=""
ACTIVE_CASE_STARTED_AT=""
ACTIVE_CASE_RECORDED=1
PROBE_PID=""
LEASE_GUARD_PID=""
LEASE_GUARD_ROW=""
LEASE_GUARD_APP=""
LEASE_GUARD_POD=""
LEASE_GUARD_POD_UID=""
LEASE_GUARD_NONCE=""
LEASE_GUARD_OUTPUT_FILE=""
LEASE_GUARD_FINISH_DEADLINE=0
LEASE_GUARD_ERROR=""
LEASE_GUARD_CONTROLLER_PID=""
LEASE_GUARD_WATCHDOG_PID=""
LEASE_GUARD_COMMAND_WRITER_PID=""
LEASE_GUARD_PID_IDENTITY=""
LEASE_GUARD_CONTROLLER_PID_IDENTITY=""
LEASE_GUARD_WATCHDOG_PID_IDENTITY=""
LEASE_GUARD_COMMAND_WRITER_PID_IDENTITY=""
LEASE_GUARD_RUNTIME_DIR=""
LEASE_GUARD_PSQL_FIFO=""
LEASE_GUARD_COMMAND_FIFO=""
LEASE_GUARD_UNLOCK_SENT=0
LEASE_GUARD_ANCHOR_OPEN=0
POD_DELETE_AMBIGUOUS=false
POD_DELETE_RECONCILIATION='null'
CORDONED_NODE=""
PARTITION_NETWORK=""
PARTITION_NODE=""
PARTITION_HOOK_ACTIVE=0
PARTITION_HOOK_ATTEMPTED=0
PARTITION_HOOK_NODE=""
PARTITION_HOOK_TARGET_JSON='null'
PARTITION_HOOK_CHALLENGE=""
PARTITION_HOOK_CHALLENGE_ISSUED_EPOCH=0
PARTITION_HOOK_START_JSON='null'
PARTITION_HOOK_VERIFY_JSON='null'
PARTITION_HOOK_STOP_JSON='null'
PARTITION_HOOK_RESULT_JSON='null'
PARTITION_HOOK_OPERATION_ID=""
PARTITION_HOOK_CONTROLLER_PID=""
PARTITION_HOOK_CONTROLLER_IDENTITY=""
PARTITION_HOOK_LAST_PHASE=""
PARTITION_HOOK_NEEDS_RECOVERY=0
kube=()
proxy_path=""

cleanup() {
  local exit_status=$?
  trap '' HUP INT TERM
  if [[ -n "${PROBE_PID:-}" ]]; then
    kill "$PROBE_PID" >/dev/null 2>&1 || true
    wait "$PROBE_PID" >/dev/null 2>&1 || true
    PROBE_PID=""
  fi
  if [[ -n "${LEASE_GUARD_PID:-}" || -n "${LEASE_GUARD_CONTROLLER_PID:-}" \
    || -n "${LEASE_GUARD_WATCHDOG_PID:-}" || -n "${LEASE_GUARD_COMMAND_WRITER_PID:-}" ]]; then
    stop_reconciler_takeover_guard >/dev/null 2>&1 || true
  fi
  if [[ "$PARTITION_HOOK_ACTIVE" == "1" ]]; then
    local cleanup_stop_json cleanup_stop_rc=0 cleanup_details_json cleanup_node cleanup_target_json cleanup_start_json cleanup_verify_json
    cleanup_node="$PARTITION_HOOK_NODE"
    cleanup_target_json="$PARTITION_HOOK_TARGET_JSON"
    cleanup_start_json="$PARTITION_HOOK_START_JSON"
    cleanup_verify_json="$PARTITION_HOOK_VERIFY_JSON"
    if stop_managed_partition_hook "cleanup-exit"; then
      cleanup_stop_rc=0
      cleanup_stop_json="$PARTITION_HOOK_RESULT_JSON"
    else
      cleanup_stop_rc=$?
      cleanup_stop_json="$PARTITION_HOOK_RESULT_JSON"
    fi
    overall_failed=1
    if [[ -n "$ACTIVE_CASE_NAME" && "$ACTIVE_CASE_RECORDED" != "1" ]]; then
      cleanup_details_json="$(jq -nc \
        --arg node "$cleanup_node" \
        --argjson target "$cleanup_target_json" \
        --argjson startHook "$cleanup_start_json" \
        --argjson verifyHook "$cleanup_verify_json" \
        --argjson stopHook "$cleanup_stop_json" \
        --argjson exitStatus "$exit_status" '
        {
          error: (
            if ($stopHook.exitCode // 1) == 0 then
              "node partition case exited before completion"
            else
              "node partition cleanup hook failed during exit"
            end
          ),
          backend: "managed-hook",
          node: $node,
          target: $target,
          partitionSeconds: '"$partition_seconds"',
          exitStatus: $exitStatus,
          startHook: $startHook,
          verifyHook: $verifyHook,
          stopHook: $stopHook
        }')"
      append_case_result "$ACTIVE_CASE_NAME" "failed" "$ACTIVE_CASE_STARTED_AT" "$(iso_now)" "$cleanup_details_json"
      ACTIVE_CASE_RECORDED=1
    fi
    if (( cleanup_stop_rc != 0 )); then
      overall_failed=1
    fi
  fi
  if [[ "$reporting_ready" == "1" && "$final_report_emitted" != "1" ]] && (( exit_status != 0 || overall_failed != 0 )); then
    if (( exit_status != 0 )); then
      overall_failed=1
      unexpected_exit_status="$exit_status"
    fi
    emit_final_report >/dev/null 2>&1 || true
  fi
  if [[ "$dry_run" != "1" && -n "${PARTITION_NETWORK:-}" && -n "${PARTITION_NODE:-}" ]]; then
    docker network connect "$PARTITION_NETWORK" "$PARTITION_NODE" >/dev/null 2>&1 || true
    PARTITION_NETWORK=""
    PARTITION_NODE=""
  fi
  if [[ "$dry_run" != "1" && -n "${CORDONED_NODE:-}" && -n "$context" ]]; then
    kubectl --context "$context" uncordon "$CORDONED_NODE" >/dev/null 2>&1 || true
    CORDONED_NODE=""
  fi
  if [[ "$dry_run" != "1" && "$created_baseline" == "1" && "$keep_resources" != "1" && -n "$context" ]]; then
    if [[ "$(acceptance_resource_owner "namespace/$namespace")" == "$acceptance_owner" ]]; then
      kubectl --context "$context" delete namespace "$namespace" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    fi
    if [[ "$(acceptance_resource_owner "clusterrolebinding/$rbac_name")" == "$acceptance_owner" ]]; then
      kubectl --context "$context" delete clusterrolebinding "$rbac_name" --ignore-not-found >/dev/null 2>&1 || true
    fi
    if [[ "$(acceptance_resource_owner "clusterrole/$rbac_name")" == "$acceptance_owner" ]]; then
      kubectl --context "$context" delete clusterrole "$rbac_name" --ignore-not-found >/dev/null 2>&1 || true
    fi
  fi
  rm -rf "$work_dir"
  return "$exit_status"
}

handle_termination_signal() {
  local signum="$1"
  trap '' HUP INT TERM
  exit "$((128 + signum))"
}

trap cleanup EXIT
trap 'handle_termination_signal 1' HUP
trap 'handle_termination_signal 2' INT
trap 'handle_termination_signal 15' TERM

iso_now() {
  date -u +"%Y-%m-%dT%H:%M:%SZ"
}

same_dir_tmp_file() {
  local target="$1"
  local target_dir target_name
  target_dir="$(dirname "$target")"
  target_name="$(basename "$target")"
  mktemp "$target_dir/.${target_name}.tmp.XXXXXX"
}

publish_create_only_file() {
  local temporary_file="$1" destination="$2"
  if ! ln "$temporary_file" "$destination"; then
    printf 'Refusing to overwrite existing Kubernetes resilience evidence path: %s\n' \
      "$destination" >&2
    return 1
  fi
  rm -f "$temporary_file" || true
  return 0
}

sha256_text() {
  python3 - "$1" <<'PY'
import hashlib
import sys

sys.stdout.write(hashlib.sha256(sys.argv[1].encode("utf-8")).hexdigest())
PY
}

sha256_json() {
  python3 - "$1" <<'PY'
import hashlib
import json
import sys

text = sys.argv[1]
try:
    payload = json.dumps(
        json.loads(text),
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")
except json.JSONDecodeError:
    payload = text.encode("utf-8")
sys.stdout.write(hashlib.sha256(payload).hexdigest())
PY
}

text_bytes() {
  printf '%s' "$1" | wc -c | tr -d '[:space:]'
}

sanitize_progress_details() {
  local details_json="$1"
  local digest
  digest="$(sha256_json "$details_json")"
  jq -nc \
    --argjson details "$details_json" \
    --arg digest "$digest" '
    {
      digest: $digest,
      error: ($details.error // null),
      reason: ($details.reason // null),
      leaseName: ($details.leaseName // null),
      node: ($details.node // null),
      deletedPod: ($details.deletedPod // null),
      network: ($details.network // null),
      partitionSeconds: ($details.partitionSeconds // null),
      readyProbeFailures: ($details.readyProbeFailures // null),
      beforeHolderId: ($details.before.holderId // null),
      afterHolderId: ($details.after.holderId // null),
      beforeFencingToken: ($details.before.fencingToken // null),
      afterFencingToken: ($details.after.fencingToken // null),
      hookExitCode: ($details.hook.exitCode // null),
      startHookExitCode: ($details.startHook.exitCode // null),
      stopHookExitCode: ($details.stopHook.exitCode // null),
      preHookExitCode: ($details.preHook.exitCode // null),
      postHookExitCode: ($details.postHook.exitCode // null)
    }
    | with_entries(select(.value != null))'
}

sanitize_managed_details() {
  local case_name="$1" details_json="$2"
  if [[ "$case_name" == "node-partition" && "$context" != kind-* ]]; then
    jq -c '
      walk(
        if type == "object" then
          del(.target, .rawTarget, .node, .nodeName, .nodeUid, .nodeUID,
              .pod, .podName, .podUid, .podUID, .targetPod, .targetPodUid,
              .challenge, .command, .context, .namespace, .extraSecret, .secret)
        else . end
      )
    ' <<<"$details_json"
  else
    printf '%s\n' "$details_json"
  fi
}

append_journal_entry() {
  local event_name="$1"
  local payload_json="$2"
  jq -nc \
    --arg schemaVersion "synara.kubernetes.resilience.acceptance.progress.v1" \
    --arg event "$event_name" \
    --arg at "$(iso_now)" \
    --arg startedAt "$started_at" \
    --arg context "$context" \
    --arg namespace "$namespace" \
    --arg rbacName "$rbac_name" \
    --arg evidenceFile "$evidence_file" \
    --arg journalFile "$journal_file" \
    --arg partialFile "$partial_file" \
    --argjson payload "$payload_json" '
    {
      schemaVersion: $schemaVersion,
      event: $event,
      at: $at,
      run: {
        startedAt: $startedAt,
        context: $context,
        namespace: $namespace,
        rbacName: $rbacName,
        evidenceFile: $evidenceFile,
        journalFile: $journalFile,
        partialFile: $partialFile
      },
      payload: $payload
    }' >>"$journal_file"
}

write_partial_snapshot() {
  local run_state="$1"
  local status="$2"
  local finished_at="${3:-}"
  local tmp_file updated_at
  updated_at="$(iso_now)"
  tmp_file="$(same_dir_tmp_file "$partial_file")"
  jq -n \
    --arg schemaVersion "synara.kubernetes.resilience.acceptance.progress.v1" \
    --arg runState "$run_state" \
    --arg status "$status" \
    --arg startedAt "$started_at" \
    --arg updatedAt "$updated_at" \
    --arg finishedAt "$finished_at" \
    --arg context "$context" \
    --arg namespace "$namespace" \
    --arg rbacName "$rbac_name" \
    --arg cases "$cases_csv" \
    --arg allowSkippedCases "$allow_skipped_cases_csv" \
    --arg soakCases "$soak_cases_csv" \
    --arg evidenceFile "$evidence_file" \
    --arg journalFile "$journal_file" \
    --arg partialFile "$partial_file" \
    --argjson bootstrapBaseline "$bootstrap_baseline" \
    --arg baselineStatus "$baseline_status" \
    --argjson baselineDurationSeconds "$baseline_duration_seconds" \
    --argjson soakSeconds "$soak_seconds" \
    --argjson soakIntervalSeconds "$soak_interval_seconds" \
    --argjson currentPhase "$current_phase_json" \
    --argjson lastProgress "$last_progress_json" \
    --argjson soak "$soak_json" \
    --slurpfile scenarios "$scenarios_file" \
    --slurpfile soakCycles "$soak_cycles_file" '
    {
      schemaVersion: $schemaVersion,
      runState: $runState,
      status: $status,
      startedAt: $startedAt,
      updatedAt: $updatedAt,
      finishedAt: (if $finishedAt == "" then null else $finishedAt end),
      context: $context,
      namespace: $namespace,
      rbacName: $rbacName,
      evidenceFile: $evidenceFile,
      journalFile: $journalFile,
      partialFile: $partialFile,
      baseline: {
        executed: ($bootstrapBaseline == 1),
        status: $baselineStatus,
        durationSeconds: $baselineDurationSeconds
      },
      plannedCases: ($cases | split(",") | map(gsub("[[:space:]]"; "") | select(length > 0))),
      allowedSkippedCases: ($allowSkippedCases | split(",") | map(gsub("[[:space:]]"; "") | select(length > 0))),
      caseCounts: {
        passed: ([($scenarios // [])[] | select(.status == "passed")] | length),
        failed: ([($scenarios // [])[] | select(.status == "failed")] | length),
        skipped: ([($scenarios // [])[] | select(.status == "skipped")] | length)
      },
      scenarios: $scenarios,
      soak: (
        $soak + {
          enabled: ($soakSeconds > 0),
          configuredDurationSeconds: $soakSeconds,
          intervalSeconds: $soakIntervalSeconds,
          plannedCases: ($soakCases | split(",") | map(gsub("[[:space:]]"; "") | select(length > 0))),
          cyclesCompleted: (($soakCycles // []) | length),
          cycles: $soakCycles
        }
      ),
      currentPhase: (if $runState == "completed" then null else $currentPhase end),
      lastProgress: $lastProgress
    }' >"$tmp_file"
  mv "$tmp_file" "$partial_file"
}

update_running_snapshot() {
  write_partial_snapshot "running" "running"
}

record_progress_update() {
  local event_name="$1"
  local payload_json="$2"
  last_progress_json="$payload_json"
  append_journal_entry "$event_name" "$payload_json"
  update_running_snapshot
}

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf '%s is required\n' "$1" >&2
    exit 1
  fi
}

require_boolean_flag() {
  local value="$1"
  local label="$2"
  if [[ "$value" != "0" && "$value" != "1" ]]; then
    printf '%s must be 0 or 1\n' "$label" >&2
    exit 1
  fi
}

require_non_negative_int() {
  local value="$1"
  local label="$2"
  if [[ ! "$value" =~ ^[0-9]+$ ]]; then
    printf '%s must be a non-negative integer\n' "$label" >&2
    exit 1
  fi
}

require_positive_int() {
  local value="$1"
  local label="$2"
  require_non_negative_int "$value" "$label"
  if (( value < 1 )); then
    printf '%s must be at least 1\n' "$label" >&2
    exit 1
  fi
}

run_command_capture() {
  local command_text="$1"
  local stdout_file="$2"
  local stderr_file="$3"
  local timeout_seconds="${4:-0}"
  python3 - "$command_text" "$stdout_file" "$stderr_file" "$timeout_seconds" <<'PY'
import json
import subprocess
import sys

command, stdout_path, stderr_path, timeout_text = sys.argv[1:5]
timeout = int(timeout_text)
timed_out = False
with open(stdout_path, "wb") as stdout_handle, open(stderr_path, "wb") as stderr_handle:
    try:
        completed = subprocess.run(
            ["/bin/sh", "-c", command],
            stdin=subprocess.DEVNULL,
            stdout=stdout_handle,
            stderr=stderr_handle,
            timeout=None if timeout <= 0 else timeout,
            check=False,
            close_fds=True,
        )
        exit_code = completed.returncode
    except subprocess.TimeoutExpired:
        timed_out = True
        exit_code = 124
json.dump(
    {"exitCode": exit_code, "timedOut": timed_out, "terminationConfirmed": True},
    sys.stdout,
    separators=(",", ":"),
    sort_keys=True,
)
raise SystemExit(exit_code)
PY
}

redacted_command_result() {
  local phase="$1"
  local command_text="$2"
  local status_json="$3"
  local stdout_file="$4"
  local stderr_file="$5"
  local extra_json="${6-}"
  local stdout_text stderr_text stdout_bytes stderr_bytes command_digest stdout_digest stderr_digest
  if [[ -z "$extra_json" ]]; then
    extra_json='{}'
  fi
  stdout_text="$(cat "$stdout_file" 2>/dev/null || true)"
  stderr_text="$(cat "$stderr_file" 2>/dev/null || true)"
  stdout_bytes="$(text_bytes "$stdout_text")"
  stderr_bytes="$(text_bytes "$stderr_text")"
  command_digest="$(sha256_text "$command_text")"
  stdout_digest="$(sha256_text "$stdout_text")"
  stderr_digest="$(sha256_text "$stderr_text")"
  jq -nc \
    --arg phase "$phase" \
    --arg command "[redacted]" \
    --arg stdout "$([[ "$stdout_bytes" == "0" ]] && printf '' || printf '[redacted]')" \
    --arg stderr "$([[ "$stderr_bytes" == "0" ]] && printf '' || printf '[redacted]')" \
    --arg commandDigest "$command_digest" \
    --arg stdoutDigest "$stdout_digest" \
    --arg stderrDigest "$stderr_digest" \
    --argjson status "$status_json" \
    --argjson stdoutBytes "$stdout_bytes" \
    --argjson stderrBytes "$stderr_bytes" \
    --argjson extra "$extra_json" '
    {
      phase: $phase,
      command: $command,
      commandRedacted: true,
      commandDigest: $commandDigest,
      exitCode: $status.exitCode,
      timedOut: ($status.timedOut // false),
      terminationConfirmed: ($status.terminationConfirmed // false),
      stdout: $stdout,
      stdoutDigest: $stdoutDigest,
      stdoutBytes: $stdoutBytes,
      stderr: $stderr,
      stderrDigest: $stderrDigest,
      stderrBytes: $stderrBytes
    } + $extra'
}

record_permission_result() {
  local expected="$1"
  local allowed="$2"
  local verb="$3"
  local resource="$4"
  local api_group="$5"
  local target_namespace="$6"
  jq -nc \
    --arg verb "$verb" \
    --arg resource "$resource" \
    --arg apiGroup "$api_group" \
    --arg namespace "$target_namespace" \
    --argjson expectedAllowed "$expected" \
    --argjson allowed "$allowed" \
    '{
      verb: $verb,
      resource: $resource,
      apiGroup: $apiGroup,
      namespace: $namespace,
      expectedAllowed: $expectedAllowed,
      allowed: $allowed
    }' >>"$permissions_file"
}

run_can_i_check() {
  local expected="$1"
  local verb="$2"
  local resource="$3"
  local api_group="$4"
  local target_namespace="$5"
  local subject="system:serviceaccount:$namespace:synara-control-plane"
  local allowed="false"
  local resource_ref="$resource"
  local stdout_file="$work_dir/can-i.stdout"
  local stderr_file="$work_dir/can-i.stderr"
  local rc=0 response=""
  if [[ -n "$api_group" ]]; then
    resource_ref="$resource.$api_group"
  fi
  local cmd=("${kube[@]}" auth can-i "$verb" "$resource_ref" --as="$subject")
  if [[ -n "$target_namespace" ]]; then
    cmd+=(--namespace "$target_namespace")
  fi
  if response="$("${cmd[@]}" >"$stdout_file" 2>"$stderr_file")"; then
    rc=0
  else
    rc=$?
  fi
  response="$(cat "$stdout_file" 2>/dev/null || true)"
  if [[ "$rc" == "0" && "$response" == "yes" ]]; then
    allowed="true"
  elif [[ "$rc" != "1" || "$response" != "no" ]]; then
    return 1
  fi
  record_permission_result "$expected" "$allowed" "$verb" "$resource" "$api_group" "$target_namespace"
  if [[ "$allowed" != "$expected" ]]; then
    return 1
  fi
}

permissions_results_json() {
  jq -s '.' "$permissions_file" 2>/dev/null || printf '[]\n'
}

run_rbac_check() {
  local role_json="$1"
  local expected="$2"
  local verb="$3"
  local resource="$4"
  local api_group="$5"
  local target_namespace="$6"
  if ! run_can_i_check "$expected" "$verb" "$resource" "$api_group" "$target_namespace"; then
    CASE_DETAILS_JSON="$(jq -nc \
      --argjson role "$role_json" \
      --arg verb "$verb" \
      --arg resource "$resource" \
      --arg apiGroup "$api_group" \
      --arg targetNamespace "$target_namespace" \
      --argjson expectedAllowed "$expected" \
      --argjson checks "$(permissions_results_json)" '
      {
        error: "RBAC permission check failed",
        failedCheck: {
          verb: $verb,
          resource: $resource,
          apiGroup: (if $apiGroup == "" then null else $apiGroup end),
          namespace: (if $targetNamespace == "" then null else $targetNamespace end),
          expectedAllowed: $expectedAllowed
        },
        clusterRoleName: $role.metadata.name,
        rules: $role.rules,
        checks: $checks
      }')"
    return 1
  fi
}

read_probe_failures() {
  local failures_file="$1"
  cat "$failures_file" 2>/dev/null || printf '0\n'
}

probe_ready() {
  local ready_json attempt
  for attempt in 1 2 3; do
    if ready_json="$("${kube[@]}" get --raw "$proxy_path/ready" 2>/dev/null)"; then
      jq -e '.status == "ready" and .checks.database.status == "ready" and .checks.schema.status == "ready"' \
        <<<"$ready_json" >/dev/null
      return
    fi
    sleep 0.2
  done
  return 1
}

probe_ready_window_failures() {
  local duration_seconds="$1"
  local failures=0
  local elapsed=0
  while (( elapsed < duration_seconds )); do
    if ! probe_ready; then
      failures=$((failures + 1))
    fi
    sleep "$probe_interval_seconds"
    elapsed=$((elapsed + probe_interval_seconds))
  done
  printf '%s\n' "$failures"
}

start_probe_window() {
  local duration_seconds="$1"
  local output_file="$2"
  PROBE_PID=""
  (
    probe_ready_window_failures "$duration_seconds" >"$output_file"
  ) &
  PROBE_PID="$!"
}

stop_probe_window() {
  if [[ -z "$PROBE_PID" ]]; then
    return 0
  fi
  local probe_pid="$PROBE_PID"
  PROBE_PID=""
  kill "$probe_pid" >/dev/null 2>&1 || true
  wait "$probe_pid" >/dev/null 2>&1 || true
}

start_continuous_probe() {
  local stop_file="$1"
  local output_file="$2"
  rm -f "$stop_file"
  PROBE_PID=""
  (
    local failures=0
    while [[ ! -f "$stop_file" ]]; do
      if ! probe_ready; then
        failures=$((failures + 1))
      fi
      sleep "$probe_interval_seconds"
    done
    printf '%s\n' "$failures" >"$output_file"
  ) &
  PROBE_PID="$!"
}

finish_continuous_probe() {
  local stop_file="$1"
  if [[ -z "$PROBE_PID" ]]; then
    return 0
  fi
  local probe_pid="$PROBE_PID"
  PROBE_PID=""
  touch "$stop_file"
  wait "$probe_pid"
}

wait_for_control_plane_ready() {
  local attempts=$(( case_timeout_seconds / probe_interval_seconds ))
  if (( attempts < 1 )); then
    attempts=1
  fi
  "${kube[@]}" -n "$namespace" rollout status deployment/synara-control-plane \
    --timeout="${case_timeout_seconds}s" >/dev/null
  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    if probe_ready; then
      return 0
    fi
    sleep "$probe_interval_seconds"
  done
  return 1
}

get_control_plane_pods_json() {
  "${kube[@]}" -n "$namespace" get pods \
    -l app.kubernetes.io/name=synara-control-plane -o json
}

stage2_postgres_scalar() {
  local query="$1"
  local attempts="${2:-10}"
  local output=""
  local stderr_file="$work_dir/session-authority-postgres.stderr"
  for ((attempt = 1; attempt <= attempts; attempt += 1)); do
    if output="$("${kube[@]}" -n "$namespace" exec deployment/synara-stage2-postgres -- \
      psql -U synara -d synara -v ON_ERROR_STOP=1 -At -c "$query" 2>"$stderr_file")"; then
      printf '%s\n' "$output"
      return 0
    fi
    sleep 1
  done
  cat "$stderr_file" >&2 2>/dev/null || true
  return 1
}

create_session_authority_sentinel() {
  local session_id project_id user_id tenant_id organization_id suffix query row reconciled
  session_id="$(python3 -c 'import uuid; print(uuid.uuid4())')"
  project_id="$(python3 -c 'import uuid; print(uuid.uuid4())')"
  user_id="$(python3 -c 'import uuid; print(uuid.uuid4())')"
  tenant_id="$(python3 -c 'import uuid; print(uuid.uuid4())')"
  organization_id="$(python3 -c 'import uuid; print(uuid.uuid4())')"
  suffix="${session_id//-/}"
  suffix="${suffix:0:12}"
  query="$(cat <<SQL
WITH selected_target AS (
  SELECT target.id
  FROM execution_targets AS target
  WHERE target.tenant_id IS NULL
    AND target.organization_id IS NULL
    AND target.kind = 'local'
    AND target.status = 'active'
  ORDER BY target.id
  LIMIT 1
), inserted_user AS (
  INSERT INTO users (id, email, display_name, status, email_verified_at)
  SELECT
    '$user_id'::uuid,
    'session-authority-$suffix@localhost.invalid',
    'Stage 4 Session Authority',
    'active',
    clock_timestamp()
  FROM selected_target
  RETURNING id
), inserted_tenant AS (
  INSERT INTO tenants (id, slug, name, status, plan_code, region, created_by)
  SELECT
    '$tenant_id'::uuid,
    'sentinel-$suffix',
    'Stage 4 Session Authority',
    'active',
    'acceptance',
    'local',
    inserted_user.id
  FROM inserted_user
  RETURNING id, created_by
), inserted_tenant_membership AS (
  INSERT INTO tenant_memberships (tenant_id, user_id, role, status, joined_at)
  SELECT
    inserted_tenant.id,
    inserted_tenant.created_by,
    'owner',
    'active',
    clock_timestamp()
  FROM inserted_tenant
  RETURNING tenant_id, user_id
), inserted_organization AS (
  INSERT INTO organizations (id, tenant_id, slug, name, kind, status, created_by)
  SELECT
    '$organization_id'::uuid,
    inserted_tenant_membership.tenant_id,
    'sentinel-$suffix',
    'Stage 4 Session Authority',
    'root',
    'active',
    inserted_tenant_membership.user_id
  FROM inserted_tenant_membership
  RETURNING id, tenant_id, created_by
), inserted_organization_membership AS (
  INSERT INTO organization_memberships (tenant_id, organization_id, user_id, role, status)
  SELECT
    inserted_organization.tenant_id,
    inserted_organization.id,
    inserted_organization.created_by,
    'owner',
    'active'
  FROM inserted_organization
  RETURNING tenant_id, organization_id, user_id
), inserted_project AS (
  INSERT INTO projects (
    id, tenant_id, organization_id, name, default_branch, visibility, created_by
  )
  SELECT
    '$project_id'::uuid,
    inserted_organization_membership.tenant_id,
    inserted_organization_membership.organization_id,
    'Stage 4 Session authority sentinel',
    'main',
    'private',
    inserted_organization_membership.user_id
  FROM inserted_organization_membership
  RETURNING id, tenant_id, organization_id, created_by
), inserted_session AS (
  INSERT INTO agent_sessions (
    id, tenant_id, organization_id, project_id, created_by,
    title, status, visibility, provider,
    execution_target_id, requested_execution_target_id,
    resource_state, meaningful_activity_at, resource_idle_since,
    suspend_after_idle_seconds
  )
  SELECT
    '$session_id'::uuid,
    inserted_project.tenant_id,
    inserted_project.organization_id,
    inserted_project.id,
    inserted_project.created_by,
    'Stage 4 Session authority sentinel',
    'active',
    'private',
    'codex',
    selected_target.id,
    selected_target.id,
    'idle',
    clock_timestamp(),
    clock_timestamp(),
    604800
  FROM inserted_project
  CROSS JOIN selected_target
  RETURNING *
)
SELECT inserted_session.id::text || '|' ||
       encode(digest(convert_to(to_jsonb(inserted_session)::text, 'UTF8'), 'sha256'), 'hex')
FROM inserted_session;
SQL
)"
  if row="$(stage2_postgres_scalar "$query" 1)" &&
    [[ "$row" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\|[0-9a-f]{64}$ ]]; then
    printf '%s\n' "$row"
    return 0
  fi
  if reconciled="$(read_session_authority_sentinel "$session_id")" &&
    [[ "$reconciled" =~ ^1\|[0-9a-f]{64}$ ]]; then
    printf '%s|%s\n' "$session_id" "${reconciled##*|}"
    return 0
  fi
  if [[ ! "$row" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}\|[0-9a-f]{64}$ ]]; then
    printf 'Session authority sentinel returned an unexpected bounded row shape: bytes=%s digest=%s\n' \
      "${#row}" "$(sha256_text "$row")" >&2
  fi
  return 1
}

read_session_authority_sentinel() {
  local session_id="$1"
  if [[ ! "$session_id" =~ ^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$ ]]; then
    return 1
  fi
  stage2_postgres_scalar "
SELECT count(*)::text || '|' ||
       COALESCE(
         min(encode(digest(convert_to(to_jsonb(session_row)::text, 'UTF8'), 'sha256'), 'hex')),
         '-'
       )
FROM agent_sessions AS session_row
WHERE session_row.id = '$session_id'::uuid;
"
}

read_reconciler_lease() {
  local lease_name="$1"
  if [[ ! "$lease_name" =~ ^[A-Za-z0-9:._-]+$ ]]; then
    return 1
  fi
  "${kube[@]}" -n "$namespace" exec deployment/synara-stage2-postgres -- \
    psql -U synara -d synara -v ON_ERROR_STOP=1 -At -F '|' \
      -c "SELECT holder_id, fencing_token FROM reconciler_leases WHERE lease_name = '$lease_name' AND expires_at > clock_timestamp()"
}

wait_for_reconciler_lease() {
  local lease_name="$1"
  local timeout_seconds="${2:-$case_timeout_seconds}"
  local deadline=$((SECONDS + timeout_seconds))
  local lease_row=""
  while (( SECONDS < deadline )); do
    lease_row="$(read_reconciler_lease "$lease_name" 2>/dev/null || true)"
    if [[ "$lease_row" =~ ^[^|]+\|[1-9][0-9]*$ ]]; then
      printf '%s\n' "$lease_row"
      return 0
    fi
    sleep 1
  done
  return 1
}

wait_for_reconciler_takeover() {
  local lease_name="$1"
  local previous_holder="$2"
  local previous_token="$3"
  local timeout_seconds="${4:-$case_timeout_seconds}"
  local deadline=$((SECONDS + timeout_seconds))
  local lease_row="" holder="" token=""
  while (( SECONDS < deadline )); do
    lease_row="$(read_reconciler_lease "$lease_name" 2>/dev/null || true)"
    if [[ "$lease_row" =~ ^[^|]+\|[1-9][0-9]*$ ]]; then
      holder="${lease_row%%|*}"
      token="${lease_row##*|}"
      if (( token > previous_token + 1 )); then
        return 2
      fi
      if [[ "$holder" != "$previous_holder" && "$token" == "$((previous_token + 1))" ]]; then
        printf '%s\n' "$lease_row"
        return 0
      fi
    fi
    sleep 1
  done
  return 1
}

start_reconciler_takeover_guard() {
  local lease_name="$1"
  local output_file="$2"
  local timeout_seconds="${3:-$case_timeout_seconds}"
  if [[ ! "$lease_name" =~ ^[A-Za-z0-9:._-]+$ ]] || [[ ! "$timeout_seconds" =~ ^[1-9][0-9]*$ ]]; then
    return 1
  fi
  local deadline=$((SECONDS + timeout_seconds))
  local identity_json="" identity="" guard_state_rc=1
  local postgres_pod="" postgres_pod_uid=""
  local guard_nonce=""
  local guard_app="synara-leader-takeover-guard-$$-$RANDOM"
  local runtime_dir="$work_dir/leader-takeover-guard-runtime"
  local psql_fifo="$runtime_dir/psql.in"
  local command_fifo="$runtime_dir/command.in"
  local reader_open_file="$runtime_dir/reader-open"
  local writer_open_file="$runtime_dir/writer-open"
  local guard_pid_file="$runtime_dir/guard.pid"
  local controller_pid_file="$runtime_dir/controller.pid"
  local command_writer_pid_file="$runtime_dir/command-writer.pid"
  local completion_file="$runtime_dir/completed"
  local watchdog_file="$runtime_dir/watchdog-timeout"
  local controller_output_file="$runtime_dir/controller.output"
  rm -f "$output_file"
  rm -f "$output_file.state"
  rm -rf "$runtime_dir"
  if ! mkdir -m 700 "$runtime_dir" || ! mkfifo "$psql_fifo" "$command_fifo"; then
    LEASE_GUARD_ERROR="guard-fifo-creation-failed"
    return 1
  fi
  chmod 600 "$psql_fifo" "$command_fifo"
  guard_nonce="$(od -An -N16 -tx1 /dev/urandom 2>/dev/null | tr -d '[:space:]')"
  if [[ ! "$guard_nonce" =~ ^[0-9a-f]{32}$ ]]; then
    LEASE_GUARD_ERROR="guard-nonce-generation-failed"
    return 1
  fi
  LEASE_GUARD_PID=""
  LEASE_GUARD_ROW=""
  LEASE_GUARD_APP="$guard_app"
  LEASE_GUARD_POD=""
  LEASE_GUARD_POD_UID=""
  LEASE_GUARD_NONCE="$guard_nonce"
  LEASE_GUARD_OUTPUT_FILE="$output_file"
  LEASE_GUARD_FINISH_DEADLINE="$deadline"
  LEASE_GUARD_ERROR=""
  LEASE_GUARD_CONTROLLER_PID=""
  LEASE_GUARD_WATCHDOG_PID=""
  LEASE_GUARD_COMMAND_WRITER_PID=""
  LEASE_GUARD_PID_IDENTITY=""
  LEASE_GUARD_CONTROLLER_PID_IDENTITY=""
  LEASE_GUARD_WATCHDOG_PID_IDENTITY=""
  LEASE_GUARD_COMMAND_WRITER_PID_IDENTITY=""
  LEASE_GUARD_RUNTIME_DIR="$runtime_dir"
  LEASE_GUARD_PSQL_FIFO="$psql_fifo"
  LEASE_GUARD_COMMAND_FIFO="$command_fifo"
  LEASE_GUARD_UNLOCK_SENT=0
  LEASE_GUARD_ANCHOR_OPEN=0
  if ! identity_json="$("${kube[@]}" -n "$namespace" get pods \
    -l app.kubernetes.io/name=synara-stage2-postgres -o json)"; then
    LEASE_GUARD_ERROR="postgres-pod-list-failed"
    return 1
  fi
  if ! identity="$(jq -er '
    [(.items // [])[]
      | select(.metadata.deletionTimestamp == null)
      | select(.status.phase == "Running")
      | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))
      | {name: .metadata.name, uid: .metadata.uid}]
    | if length == 1 then "\(.[0].name)|\(.[0].uid)" else empty end
  ' <<<"$identity_json")"; then
    LEASE_GUARD_ERROR="postgres-pod-identity-not-unique"
    return 1
  fi
  postgres_pod="${identity%%|*}"
  postgres_pod_uid="${identity##*|}"
  if [[ ! "$postgres_pod" =~ ^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$ ]] \
    || [[ ! "$postgres_pod_uid" =~ ^[A-Za-z0-9_-]+$ ]]; then
    LEASE_GUARD_ERROR="postgres-pod-identity-malformed"
    return 1
  fi
  LEASE_GUARD_POD="$postgres_pod"
  LEASE_GUARD_POD_UID="$postgres_pod_uid"

  (
    exec 7>&- 3>&- 4>&- 5>&-
    while (( SECONDS < deadline )); do
      if [[ -f "$completion_file" ]]; then
        exit 0
      fi
      sleep 0.1
    done
    printf 'guard-watchdog-timeout\n' >"$watchdog_file"
    local_pid="$(cat "$controller_pid_file" 2>/dev/null || true)"
    local_identity="$(cat "$runtime_dir/controller.identity" 2>/dev/null || true)"
    if [[ "$local_pid" =~ ^[1-9][0-9]*$ ]] && process_identity_is_live "$local_pid" "$local_identity"; then
      signal_owned_process "$local_pid" "$local_identity" TERM >/dev/null 2>&1 || : >"$runtime_dir/watchdog-identity-failure"
    elif [[ "$local_pid" =~ ^[1-9][0-9]*$ ]]; then : >"$runtime_dir/watchdog-identity-failure"
    fi
    sleep 0.5
    local_pid="$(cat "$command_writer_pid_file" 2>/dev/null || true)"
    local_identity="$(cat "$runtime_dir/command-writer.identity" 2>/dev/null || true)"
    if [[ "$local_pid" =~ ^[1-9][0-9]*$ ]] && process_identity_is_live "$local_pid" "$local_identity"; then
      signal_owned_process "$local_pid" "$local_identity" TERM >/dev/null 2>&1 || : >"$runtime_dir/watchdog-identity-failure"
      sleep 0.1
      signal_owned_process "$local_pid" "$local_identity" KILL >/dev/null 2>&1 || true
    elif [[ "$local_pid" =~ ^[1-9][0-9]*$ ]]; then : >"$runtime_dir/watchdog-identity-failure"
    fi
    local_pid="$(cat "$guard_pid_file" 2>/dev/null || true)"
    local_identity="$(cat "$runtime_dir/guard.identity" 2>/dev/null || true)"
    if [[ "$local_pid" =~ ^[1-9][0-9]*$ ]] && process_identity_is_live "$local_pid" "$local_identity"; then
      signal_owned_process "$local_pid" "$local_identity" TERM >/dev/null 2>&1 || : >"$runtime_dir/watchdog-identity-failure"
      sleep 0.5
      signal_owned_process "$local_pid" "$local_identity" KILL >/dev/null 2>&1 || true
    elif [[ "$local_pid" =~ ^[1-9][0-9]*$ ]]; then : >"$runtime_dir/watchdog-identity-failure"
    fi
  ) &
  LEASE_GUARD_WATCHDOG_PID="$!"
  LEASE_GUARD_WATCHDOG_PID_IDENTITY="$(controller_process_identity "$LEASE_GUARD_WATCHDOG_PID")" || return 1

  exec 7<>"$psql_fifo"
  LEASE_GUARD_ANCHOR_OPEN=1
  (
    exec 5<"$psql_fifo"
    : >"$reader_open_file"
    exec 7>&-
    exec 0<&5
    exec 5<&-
    exec "${kube[@]}" -n "$namespace" exec -i "pod/$postgres_pod" -- \
      env PGAPPNAME="$guard_app" \
      psql -X -q -U synara -d synara -v ON_ERROR_STOP=1 -At
  ) >"$output_file" 2>&1 &
  LEASE_GUARD_PID="$!"
  LEASE_GUARD_PID_IDENTITY="$(controller_process_identity "$LEASE_GUARD_PID")" || return 1
  printf '%s\n' "$LEASE_GUARD_PID" >"$guard_pid_file"
  printf '%s\n' "$LEASE_GUARD_PID_IDENTITY" >"$runtime_dir/guard.identity"

  (
    exec 3<>"$command_fifo"
    exec 4>"$psql_fifo"
    exec 7>&-
    : >"$writer_open_file"
    if ! printf '%s\n' \
      "SELECT pg_advisory_lock(hashtextextended('$lease_name', 0));" \
      "SELECT '__SYNARA_RECONCILER_GUARD_LEASE_V2__|$guard_nonce|' || holder_id || '|' || fencing_token::text FROM reconciler_leases WHERE lease_name = '$lease_name' AND expires_at > clock_timestamp();" \
      "SELECT '__SYNARA_RECONCILER_GUARD_READY_V2__|$guard_nonce|$postgres_pod|$postgres_pod_uid';" >&4; then
      exit 71
    fi
    command=""
    if ! IFS= read -r command <&3; then
      exit 72
    fi
    if [[ "$command" != "UNLOCK|$guard_nonce" ]]; then
      exit 73
    fi
    if ! printf '%s\n' \
      "SELECT '__SYNARA_RECONCILER_GUARD_UNLOCKED_V2__|$guard_nonce|' || pg_advisory_unlock(hashtextextended('$lease_name', 0))::text;" \
      '\quit' >&4; then
      exit 74
    fi
    : >"$runtime_dir/unlock-sql-written"
    exec 4>&-
    : >"$runtime_dir/controller-complete"
  ) >"$controller_output_file" 2>&1 &
  LEASE_GUARD_CONTROLLER_PID="$!"
  LEASE_GUARD_CONTROLLER_PID_IDENTITY="$(controller_process_identity "$LEASE_GUARD_CONTROLLER_PID")" || return 1
  printf '%s\n' "$LEASE_GUARD_CONTROLLER_PID" >"$controller_pid_file"
  printf '%s\n' "$LEASE_GUARD_CONTROLLER_PID_IDENTITY" >"$runtime_dir/controller.identity"

  while (( SECONDS < deadline )); do
    if [[ -f "$watchdog_file" ]]; then
      LEASE_GUARD_ERROR="guard-watchdog-timeout"
      return 1
    fi
    if [[ -f "$reader_open_file" && -f "$writer_open_file" && "$LEASE_GUARD_ANCHOR_OPEN" == "1" ]]; then
      exec 7>&-
      LEASE_GUARD_ANCHOR_OPEN=0
    fi
    if parse_reconciler_takeover_guard_output "$output_file" "$guard_nonce" \
      "$postgres_pod" "$postgres_pod_uid" 0 0; then
      guard_state_rc=0
    else
      guard_state_rc=$?
    fi
    if (( guard_state_rc == 0 )); then
      if [[ "$LEASE_GUARD_ANCHOR_OPEN" != "0" ]] \
        || ! process_identity_is_live "$LEASE_GUARD_PID" "$LEASE_GUARD_PID_IDENTITY" \
        || ! process_identity_is_live "$LEASE_GUARD_CONTROLLER_PID" "$LEASE_GUARD_CONTROLLER_PID_IDENTITY"; then
        LEASE_GUARD_ERROR="guard-stream-ended-before-delete"
        return 1
      fi
      if ! verify_reconciler_takeover_guard_pod; then
        LEASE_GUARD_ERROR="postgres-pod-identity-changed"
        return 1
      fi
      if ! process_identity_is_live "$LEASE_GUARD_PID" "$LEASE_GUARD_PID_IDENTITY" \
        || ! process_identity_is_live "$LEASE_GUARD_CONTROLLER_PID" "$LEASE_GUARD_CONTROLLER_PID_IDENTITY"; then
        LEASE_GUARD_ERROR="guard-stream-ended-before-delete"
        return 1
      fi
      printf 'status=ready\nprotocol=v2\npostgresPod=%s\npostgresPodUid=%s\n' \
        "$postgres_pod" "$postgres_pod_uid" >"$output_file.state"
      return 0
    fi
    if (( guard_state_rc == 2 )); then
      LEASE_GUARD_ERROR="guard-output-malformed"
      printf 'status=malformed\npostgresPod=%s\npostgresPodUid=%s\n' \
        "$postgres_pod" "$postgres_pod_uid" >"$output_file.state"
      return 1
    fi
    if ! process_identity_is_live "$LEASE_GUARD_PID" "$LEASE_GUARD_PID_IDENTITY" \
      || ! process_identity_is_live "$LEASE_GUARD_CONTROLLER_PID" "$LEASE_GUARD_CONTROLLER_PID_IDENTITY"; then
      if parse_reconciler_takeover_guard_output "$output_file" "$guard_nonce" \
        "$postgres_pod" "$postgres_pod_uid" 0 1; then
        guard_state_rc=0
      else
        guard_state_rc=$?
      fi
      if (( guard_state_rc == 2 )); then
        LEASE_GUARD_ERROR="guard-output-malformed-at-eof"
      else
        LEASE_GUARD_ERROR="guard-stream-ended-before-ready"
      fi
      printf 'status=stream-ended\npostgresPod=%s\npostgresPodUid=%s\n' \
        "$postgres_pod" "$postgres_pod_uid" >"$output_file.state"
      return 1
    fi
    sleep 0.1
  done
  LEASE_GUARD_ERROR="guard-watchdog-timeout"
  printf 'status=timeout\npostgresPod=%s\npostgresPodUid=%s\n' \
    "$postgres_pod" "$postgres_pod_uid" >"$output_file.state"
  return 1
}

parse_reconciler_takeover_guard_output() {
  local output_file="$1"
  local expected_nonce="$2"
  local expected_pod="$3"
  local expected_pod_uid="$4"
  local require_unlocked="${5:-0}"
  local stream_ended="${6:-0}"
  local ready_prefix="__SYNARA_RECONCILER_GUARD_READY_V2__|"
  local lease_prefix="__SYNARA_RECONCILER_GUARD_LEASE_V2__|"
  local unlocked_prefix="__SYNARA_RECONCILER_GUARD_UNLOCKED_V2__|"
  local guard_prefix="__SYNARA_RECONCILER_GUARD_"
  local line="" trailing_line="" line_complete=0 ready_count=0 lease_count=0 unlocked_count=0 malformed=0 lease_row=""
  local payload="" stage=0
  [[ -f "$output_file" ]] || return 1
  while true; do
    line=""
    if IFS= read -r line; then
      line_complete=1
    else
      line_complete=0
    fi
    if (( line_complete == 0 )); then
      trailing_line="$line"
      break
    fi
    case "$line" in
      "$lease_prefix"*)
        lease_count=$((lease_count + 1))
        payload="${line#"$lease_prefix"}"
        if (( stage != 0 )) || [[ "$payload" != "$expected_nonce|"* ]]; then
          malformed=1
        else
          lease_row="${payload#"$expected_nonce|"}"
          if [[ ! "$lease_row" =~ ^[^|]+\|[1-9][0-9]*$ ]]; then
            malformed=1
          fi
        fi
        stage=1
        ;;
      "$ready_prefix"*)
        ready_count=$((ready_count + 1))
        if (( stage != 1 )) \
          || [[ "$line" != "${ready_prefix}${expected_nonce}|${expected_pod}|${expected_pod_uid}" ]]; then
          malformed=1
        fi
        stage=2
        ;;
      "$unlocked_prefix"*)
        unlocked_count=$((unlocked_count + 1))
        if (( stage != 2 )) || [[ "$require_unlocked" != "1" ]] \
          || [[ "$line" != "${unlocked_prefix}${expected_nonce}|true" ]]; then
          malformed=1
        fi
        stage=3
        ;;
      "$guard_prefix"*)
        malformed=1
        ;;
    esac
  done <"$output_file"
  if [[ "$stream_ended" == "1" && -n "$trailing_line" && "$trailing_line" == __SYNARA_* ]]; then
    malformed=1
  fi
  if (( malformed != 0 || ready_count > 1 || lease_count > 1 || unlocked_count > 1 )); then
    return 2
  fi
  if (( ready_count != 1 || lease_count != 1 )); then
    return 1
  fi
  if [[ "$require_unlocked" == "1" ]] && (( unlocked_count != 1 )); then
    return 1
  fi
  if [[ "$require_unlocked" != "1" ]] && (( unlocked_count != 0 )); then
    return 2
  fi
  LEASE_GUARD_ROW="$lease_row"
  return 0
}

verify_reconciler_takeover_guard_pod() {
  local pod_json=""
  [[ -n "$LEASE_GUARD_POD" && -n "$LEASE_GUARD_POD_UID" ]] || return 1
  if ! pod_json="$("${kube[@]}" -n "$namespace" get "pod/$LEASE_GUARD_POD" -o json)"; then
    return 1
  fi
  jq -e --arg uid "$LEASE_GUARD_POD_UID" '
    .metadata.uid == $uid
    and .metadata.deletionTimestamp == null
    and .status.phase == "Running"
    and any(.status.conditions[]?; .type == "Ready" and .status == "True")
  ' <<<"$pod_json" >/dev/null
}

assert_reconciler_takeover_guard_active() {
  [[ -n "$LEASE_GUARD_PID" && -n "$LEASE_GUARD_CONTROLLER_PID" ]] || {
    LEASE_GUARD_ERROR="guard-process-missing-before-delete"
    return 1
  }
  if [[ -f "$LEASE_GUARD_RUNTIME_DIR/watchdog-timeout" \
    || -f "$LEASE_GUARD_RUNTIME_DIR/watchdog-identity-failure" \
    || "$LEASE_GUARD_UNLOCK_SENT" != "0" ]]; then
    LEASE_GUARD_ERROR="guard-not-in-pre-unlock-state"
    return 1
  fi
  if ! parse_reconciler_takeover_guard_output "$LEASE_GUARD_OUTPUT_FILE" "$LEASE_GUARD_NONCE" \
    "$LEASE_GUARD_POD" "$LEASE_GUARD_POD_UID" 0 0; then
    LEASE_GUARD_ERROR="guard-output-invalid-before-delete"
    return 1
  fi
  if ! process_identity_is_live "$LEASE_GUARD_PID" "$LEASE_GUARD_PID_IDENTITY"; then
    LEASE_GUARD_ERROR="guard-stream-ended-before-delete"
    return 1
  fi
  if ! process_identity_is_live "$LEASE_GUARD_CONTROLLER_PID" "$LEASE_GUARD_CONTROLLER_PID_IDENTITY"; then
    LEASE_GUARD_ERROR="guard-controller-ended-before-delete"
    return 1
  fi
  if ! verify_reconciler_takeover_guard_pod; then
    LEASE_GUARD_ERROR="postgres-pod-identity-changed-before-delete"
    return 1
  fi
  if ! process_identity_is_live "$LEASE_GUARD_PID" "$LEASE_GUARD_PID_IDENTITY" \
    || ! process_identity_is_live "$LEASE_GUARD_CONTROLLER_PID" "$LEASE_GUARD_CONTROLLER_PID_IDENTITY"; then
    LEASE_GUARD_ERROR="guard-stream-ended-before-delete"
    return 1
  fi
  return 0
}

release_reconciler_takeover_guard() {
  local deletion_json="$1"
  local writer_pid="" writer_rc=0
  if ! jq -e '
    .outcome == "submitted" or .outcome == "accepted-after-read-after-write"
  ' <<<"$deletion_json" >/dev/null 2>&1; then
    LEASE_GUARD_ERROR="guard-unlock-without-accepted-delete"
    return 1
  fi
  if ! assert_reconciler_takeover_guard_active; then
    return 1
  fi
  (
    exec 7>&- 3>&- 4>&- 5>&-
    printf 'UNLOCK|%s\n' "$LEASE_GUARD_NONCE" >"$LEASE_GUARD_COMMAND_FIFO"
  ) &
  writer_pid="$!"
  LEASE_GUARD_COMMAND_WRITER_PID="$writer_pid"
  if ! LEASE_GUARD_COMMAND_WRITER_PID_IDENTITY="$(controller_process_identity "$writer_pid")"; then
    if wait "$writer_pid"; then
      LEASE_GUARD_COMMAND_WRITER_PID=""
      LEASE_GUARD_COMMAND_WRITER_PID_IDENTITY=""
      LEASE_GUARD_UNLOCK_SENT=1
      : >"$LEASE_GUARD_RUNTIME_DIR/unlock-command-sent"
      return 0
    fi
    LEASE_GUARD_ERROR="guard-unlock-writer-identity-unavailable"
    return 1
  fi
  printf '%s\n' "$writer_pid" >"$LEASE_GUARD_RUNTIME_DIR/command-writer.pid"
  printf '%s\n' "$LEASE_GUARD_COMMAND_WRITER_PID_IDENTITY" >"$LEASE_GUARD_RUNTIME_DIR/command-writer.identity"
  while process_identity_is_live "$writer_pid" "$LEASE_GUARD_COMMAND_WRITER_PID_IDENTITY"; do
    if [[ -f "$LEASE_GUARD_RUNTIME_DIR/watchdog-timeout" ]] \
      || (( SECONDS >= LEASE_GUARD_FINISH_DEADLINE )); then
      signal_owned_process "$writer_pid" "$LEASE_GUARD_COMMAND_WRITER_PID_IDENTITY" TERM >/dev/null 2>&1 || true
      wait "$writer_pid" >/dev/null 2>&1 || true
      LEASE_GUARD_COMMAND_WRITER_PID=""
      LEASE_GUARD_COMMAND_WRITER_PID_IDENTITY=""
      LEASE_GUARD_ERROR="guard-unlock-writer-timeout"
      return 1
    fi
    sleep 0.05
  done
  if wait "$writer_pid"; then
    writer_rc=0
  else
    writer_rc=$?
  fi
  LEASE_GUARD_COMMAND_WRITER_PID=""
  LEASE_GUARD_COMMAND_WRITER_PID_IDENTITY=""
  if (( writer_rc != 0 )); then
    LEASE_GUARD_ERROR="guard-unlock-writer-failed"
    return 1
  fi
  LEASE_GUARD_UNLOCK_SENT=1
  : >"$LEASE_GUARD_RUNTIME_DIR/unlock-command-sent"
  return 0
}

read_reconciler_takeover_guard_diagnostic() {
  local output_file="$1"
  local nonce="${2:-}"
  if [[ ! -f "$output_file" ]]; then
    return 0
  fi
  if [[ -n "$nonce" ]]; then
    sed "s/${nonce}/[redacted]/g" "$output_file" 2>/dev/null | tail -c 2000
  else
    tail -c 2000 "$output_file" 2>/dev/null
  fi
}

reconcile_pod_uid_delete() {
  local pod_name="$1"
  local pod_uid="$2"
  local attempts="${3:-3}"
  local pod_json="" observed_uid="" observed_deletion="" valid_observations=0
  POD_DELETE_OBSERVED_STATE="unresolved"
  POD_DELETE_REPLACEMENT_UID=""
  for ((reconcile_attempt = 1; reconcile_attempt <= attempts; reconcile_attempt += 1)); do
    pod_json=""
    if pod_json="$("${kube[@]}" -n "$namespace" get "pod/$pod_name" -o json --ignore-not-found 2>/dev/null)"; then
      if [[ -z "$pod_json" ]]; then
        POD_DELETE_OBSERVED_STATE="original-uid-absent"
        return 0
      fi
      if ! observed_uid="$(jq -er '.metadata.uid' <<<"$pod_json")"; then
        sleep 0.1
        continue
      fi
      valid_observations=$((valid_observations + 1))
      if [[ "$observed_uid" != "$pod_uid" ]]; then
        POD_DELETE_OBSERVED_STATE="replacement-present"
        POD_DELETE_REPLACEMENT_UID="$observed_uid"
        return 0
      fi
      observed_deletion="$(jq -r '.metadata.deletionTimestamp // empty' <<<"$pod_json" 2>/dev/null || true)"
      if [[ -n "$observed_deletion" ]]; then
        POD_DELETE_OBSERVED_STATE="original-uid-terminating"
        return 0
      fi
      POD_DELETE_OBSERVED_STATE="same-uid-active"
    fi
    if (( reconcile_attempt < attempts )); then
      sleep 0.1
    fi
  done
  if (( valid_observations > 0 )) && [[ "$POD_DELETE_OBSERVED_STATE" == "same-uid-active" ]]; then
    return 1
  fi
  POD_DELETE_OBSERVED_STATE="unresolved"
  return 2
}

classify_pod_delete_response() {
  local output_file="$1"
  local bounded_response=""
  bounded_response="$(tail -c 2000 "$output_file" 2>/dev/null || true)"
  if grep -qi 'unexpected EOF' <<<"$bounded_response"; then
    printf 'unexpected-eof\n'
  elif grep -qi 'conflict\|precondition' <<<"$bounded_response"; then
    printf 'precondition-conflict\n'
  elif grep -qi 'not[ -]*found' <<<"$bounded_response"; then
    printf 'not-found\n'
  else
    printf 'nonzero-exit\n'
  fi
}

delete_pod_uid_preconditioned() {
  local pod_name="$1"
  local pod_uid="$2"
  local output_file="$3"
  local raw_uri="/api/v1/namespaces/$namespace/pods/$pod_name"
  local delete_options="" response_class="" reconcile_rc=0 delete_rc=0
  local max_attempts=3 attempt=0
  POD_DELETE_AMBIGUOUS=false
  POD_DELETE_RECONCILIATION='null'
  POD_DELETE_OBSERVED_STATE="not-checked"
  POD_DELETE_REPLACEMENT_UID=""
  if [[ ! "$pod_name" =~ ^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$ ]] \
    || [[ ! "$pod_uid" =~ ^[A-Za-z0-9_-]+$ ]]; then
    POD_DELETE_RECONCILIATION='{"outcome":"invalid-identity","attempts":0,"ambiguous":false}'
    return 1
  fi
  delete_options="$(jq -nc --arg uid "$pod_uid" \
    '{apiVersion:"v1",kind:"DeleteOptions",propagationPolicy:"Background",preconditions:{uid:$uid}}')"
  : >"$output_file"
  for ((attempt = 1; attempt <= max_attempts; attempt += 1)); do
    if ! assert_reconciler_takeover_guard_active; then
      POD_DELETE_RECONCILIATION="$(jq -nc --arg outcome "guard-invalid-before-retry" \
        --arg observedState "$POD_DELETE_OBSERVED_STATE" --argjson attempts "$((attempt - 1))" \
        '{outcome:$outcome,attempts:$attempts,ambiguous:true,observedState:$observedState,responseRedacted:true,responseMaxBytes:2000}')"
      return 1
    fi
    if printf '%s' "$delete_options" | "${kube[@]}" delete --raw "$raw_uri" -f - >"$output_file" 2>&1; then
      POD_DELETE_RECONCILIATION="$(jq -nc --arg outcome "submitted" --argjson attempts "$attempt" \
        --argjson ambiguous "$POD_DELETE_AMBIGUOUS" --arg responseClass "$response_class" \
        --arg observedState "$POD_DELETE_OBSERVED_STATE" '
        {outcome:$outcome,attempts:$attempts,ambiguous:$ambiguous,
         priorResponseClass:(if $ambiguous then $responseClass else null end),
         priorObservedState:(if $ambiguous then $observedState else null end),
         responseRedacted:true,responseMaxBytes:2000}')"
      return 0
    else
      delete_rc=$?
    fi
    POD_DELETE_AMBIGUOUS=true
    response_class="$(classify_pod_delete_response "$output_file")"
    if reconcile_pod_uid_delete "$pod_name" "$pod_uid" 3; then
      reconcile_rc=0
    else
      reconcile_rc=$?
    fi
    if (( reconcile_rc == 0 )); then
      POD_DELETE_RECONCILIATION="$(jq -nc --arg outcome "accepted-after-read-after-write" \
        --arg observedState "$POD_DELETE_OBSERVED_STATE" --arg replacementUid "$POD_DELETE_REPLACEMENT_UID" \
        --arg responseClass "$response_class" --argjson attempts "$attempt" --argjson deleteExitCode "$delete_rc" \
        '{outcome:$outcome,attempts:$attempts,ambiguous:true,deleteExitCode:$deleteExitCode,
          observedState:$observedState,replacementUid:(if $replacementUid == "" then null else $replacementUid end),
          responseClass:$responseClass,responseRedacted:true,responseMaxBytes:2000}')"
      return 0
    fi
    if (( reconcile_rc == 2 )); then
      POD_DELETE_RECONCILIATION="$(jq -nc --arg outcome "reconciliation-unresolved" \
        --arg responseClass "$response_class" --argjson attempts "$attempt" --argjson deleteExitCode "$delete_rc" \
        '{outcome:$outcome,attempts:$attempts,ambiguous:true,deleteExitCode:$deleteExitCode,
          observedState:"unresolved",responseClass:$responseClass,responseRedacted:true,responseMaxBytes:2000}')"
      return 1
    fi
    if (( attempt < max_attempts )); then
      sleep 0.1
    fi
  done
  POD_DELETE_RECONCILIATION="$(jq -nc --arg outcome "same-uid-still-active" \
    --arg responseClass "$response_class" --argjson attempts "$max_attempts" --argjson deleteExitCode "$delete_rc" \
    '{outcome:$outcome,attempts:$attempts,ambiguous:true,deleteExitCode:$deleteExitCode,
      observedState:"same-uid-active",responseClass:$responseClass,responseRedacted:true,responseMaxBytes:2000}')"
  return 1
}

wait_for_pod_uid_absent() {
  local pod_name="$1"
  local pod_uid="$2"
  local timeout_seconds="${3:-$case_timeout_seconds}"
  local deadline=$((SECONDS + timeout_seconds))
  local pod_json="" observed_uid=""
  while (( SECONDS < deadline )); do
    pod_json=""
    if pod_json="$("${kube[@]}" -n "$namespace" get "pod/$pod_name" -o json --ignore-not-found 2>/dev/null)"; then
      if [[ -z "$pod_json" ]]; then
        return 0
      fi
      if observed_uid="$(jq -er '.metadata.uid' <<<"$pod_json" 2>/dev/null)" \
        && [[ "$observed_uid" != "$pod_uid" ]]; then
        return 0
      fi
    fi
    sleep 0.2
  done
  return 1
}

finish_reconciler_takeover_guard() {
  if [[ -z "$LEASE_GUARD_PID" || -z "$LEASE_GUARD_CONTROLLER_PID" \
    || "$LEASE_GUARD_UNLOCK_SENT" != "1" ]]; then
    LEASE_GUARD_ERROR="guard-process-missing-at-finish"
    return 1
  fi
  local guard_pid="$LEASE_GUARD_PID"
  local controller_pid="$LEASE_GUARD_CONTROLLER_PID"
  local guard_rc=0 controller_rc=0 parse_rc=0 current_watchdog_identity="" guard_live=0 controller_live=0
  while true; do
    guard_live=0
    controller_live=0
    if process_identity_is_live "$guard_pid" "$LEASE_GUARD_PID_IDENTITY"; then
      guard_live=1
    elif kill -0 "$guard_pid" >/dev/null 2>&1 && ! process_is_terminal_state "$guard_pid"; then
      LEASE_GUARD_ERROR="guard-stream-identity-changed-at-finish"
      return 1
    fi
    if process_identity_is_live "$controller_pid" "$LEASE_GUARD_CONTROLLER_PID_IDENTITY"; then
      controller_live=1
    elif kill -0 "$controller_pid" >/dev/null 2>&1 && ! process_is_terminal_state "$controller_pid"; then
      LEASE_GUARD_ERROR="guard-controller-identity-changed-at-finish"
      return 1
    fi
    (( guard_live == 0 && controller_live == 0 )) && break
    if [[ -f "$LEASE_GUARD_RUNTIME_DIR/watchdog-timeout" ]] \
      || (( SECONDS >= LEASE_GUARD_FINISH_DEADLINE )); then
      LEASE_GUARD_ERROR="guard-final-wait-timeout"
      return 1
    fi
    sleep 0.1
  done
  if wait "$controller_pid"; then
    controller_rc=0
  else
    controller_rc=$?
  fi
  LEASE_GUARD_CONTROLLER_PID=""
  LEASE_GUARD_CONTROLLER_PID_IDENTITY=""
  if wait "$guard_pid"; then
    guard_rc=0
  else
    guard_rc=$?
  fi
  LEASE_GUARD_PID=""
  LEASE_GUARD_PID_IDENTITY=""
  if (( controller_rc != 0 )); then
    LEASE_GUARD_ERROR="guard-controller-exit-nonzero"
    return 1
  fi
  if (( guard_rc != 0 )); then
    LEASE_GUARD_ERROR="guard-stream-exit-nonzero"
    return 1
  fi
  if parse_reconciler_takeover_guard_output "$LEASE_GUARD_OUTPUT_FILE" "$LEASE_GUARD_NONCE" \
    "$LEASE_GUARD_POD" "$LEASE_GUARD_POD_UID" 1 1; then
    parse_rc=0
  else
    parse_rc=$?
  fi
  if (( parse_rc != 0 )); then
    LEASE_GUARD_ERROR="$([[ "$parse_rc" == "2" ]] && printf guard-final-output-malformed || printf guard-unlock-sentinel-missing)"
    return 1
  fi
  if ! verify_reconciler_takeover_guard_pod; then
    LEASE_GUARD_ERROR="postgres-pod-identity-changed-before-unlock"
    return 1
  fi
  if [[ ! -f "$LEASE_GUARD_RUNTIME_DIR/unlock-command-sent" \
    || ! -f "$LEASE_GUARD_RUNTIME_DIR/unlock-sql-written" \
    || ! -f "$LEASE_GUARD_RUNTIME_DIR/controller-complete" ]]; then
    LEASE_GUARD_ERROR="guard-unlock-sequence-incomplete"
    return 1
  fi
  : >"$LEASE_GUARD_RUNTIME_DIR/completed"
  if [[ -n "$LEASE_GUARD_WATCHDOG_PID" ]]; then
    if kill -0 "$LEASE_GUARD_WATCHDOG_PID" >/dev/null 2>&1; then
      current_watchdog_identity="$(controller_process_identity "$LEASE_GUARD_WATCHDOG_PID" 2>/dev/null || true)"
      if process_identity_is_live "$LEASE_GUARD_WATCHDOG_PID" "$LEASE_GUARD_WATCHDOG_PID_IDENTITY"; then
        signal_owned_process "$LEASE_GUARD_WATCHDOG_PID" "$LEASE_GUARD_WATCHDOG_PID_IDENTITY" TERM >/dev/null 2>&1 || true
      elif ! process_is_terminal_state "$LEASE_GUARD_WATCHDOG_PID" \
        && kill -0 "$LEASE_GUARD_WATCHDOG_PID" >/dev/null 2>&1 \
        && [[ "$current_watchdog_identity" != "$LEASE_GUARD_WATCHDOG_PID_IDENTITY" ]]; then
        LEASE_GUARD_ERROR="guard-watchdog-identity-changed"
        return 1
      fi
    fi
    wait "$LEASE_GUARD_WATCHDOG_PID" >/dev/null 2>&1 || true
    LEASE_GUARD_WATCHDOG_PID=""
    LEASE_GUARD_WATCHDOG_PID_IDENTITY=""
  fi
  rm -rf "$LEASE_GUARD_RUNTIME_DIR"
  return 0
}

stop_owned_child() {
  local pid="$1" identity="$2" grace_seconds="${3:-1}" deadline
  [[ -n "$pid" ]] || return 0
  if process_identity_is_live "$pid" "$identity"; then
    signal_owned_process "$pid" "$identity" TERM >/dev/null 2>&1 || return 1
    deadline=$((SECONDS + grace_seconds))
    while process_identity_is_live "$pid" "$identity" && (( SECONDS < deadline )); do sleep 0.05; done
    if process_identity_is_live "$pid" "$identity"; then
      signal_owned_process "$pid" "$identity" KILL >/dev/null 2>&1 || return 1
    fi
  elif kill -0 "$pid" >/dev/null 2>&1 && ! process_is_terminal_state "$pid"; then
    return 1
  fi
  wait "$pid" >/dev/null 2>&1 || true
  return 0
}

stop_reconciler_takeover_guard() {
  local cleanup_failed=0
  if [[ "$LEASE_GUARD_ANCHOR_OPEN" == "1" ]]; then
    exec 7>&-
    LEASE_GUARD_ANCHOR_OPEN=0
  fi
  stop_owned_child "$LEASE_GUARD_COMMAND_WRITER_PID" "$LEASE_GUARD_COMMAND_WRITER_PID_IDENTITY" 1 || cleanup_failed=1
  LEASE_GUARD_COMMAND_WRITER_PID=""
  LEASE_GUARD_COMMAND_WRITER_PID_IDENTITY=""
  stop_owned_child "$LEASE_GUARD_CONTROLLER_PID" "$LEASE_GUARD_CONTROLLER_PID_IDENTITY" 1 || cleanup_failed=1
  LEASE_GUARD_CONTROLLER_PID=""
  LEASE_GUARD_CONTROLLER_PID_IDENTITY=""
  stop_owned_child "$LEASE_GUARD_PID" "$LEASE_GUARD_PID_IDENTITY" 1 || cleanup_failed=1
  LEASE_GUARD_PID=""
  LEASE_GUARD_PID_IDENTITY=""
  stop_owned_child "$LEASE_GUARD_WATCHDOG_PID" "$LEASE_GUARD_WATCHDOG_PID_IDENTITY" 1 || cleanup_failed=1
  LEASE_GUARD_WATCHDOG_PID=""
  LEASE_GUARD_WATCHDOG_PID_IDENTITY=""
  if [[ -n "$LEASE_GUARD_RUNTIME_DIR" ]]; then rm -rf "$LEASE_GUARD_RUNTIME_DIR"; fi
  (( cleanup_failed == 0 ))
}
get_pdb_json() {
  local json
  if json="$("${kube[@]}" -n "$namespace" get poddisruptionbudget synara-control-plane -o json 2>/dev/null)"; then
    printf '%s\n' "$json"
  else
    printf 'null\n'
  fi
}

collect_topology_json() {
  local nodes_json control_plane_pods_json all_pods_json pdb_json
  nodes_json="$("${kube[@]}" get nodes -o json)"
  control_plane_pods_json="$(get_control_plane_pods_json)"
  all_pods_json="$("${kube[@]}" -n "$namespace" get pods -o json)"
  pdb_json="$(get_pdb_json)"
  jq -nc \
    --argjson nodes "$nodes_json" \
    --argjson pods "$control_plane_pods_json" \
    --argjson allPods "$all_pods_json" \
    --argjson pdb "$pdb_json" '
    [
      ($allPods.items // [])[]
      | select(
          .metadata.labels["app.kubernetes.io/name"] == "synara-stage2-postgres"
          or .metadata.labels["app.kubernetes.io/name"] == "synara-stage2-minio"
        )
      | {name: .metadata.name, node: .spec.nodeName}
    ] as $dependencies
    | {
      nodeCount: ($nodes.items | length),
      controlPlaneNodeCount: ([
        ($nodes.items // [])[] |
        select(.metadata.labels["node-role.kubernetes.io/control-plane"] != null)
      ] | length),
      workerNodeCount: ([
        ($nodes.items // [])[] |
        select(.metadata.labels["node-role.kubernetes.io/control-plane"] == null)
      ] | length),
      controlPlanePods: [
        ($pods.items // [])[] |
        {
          name: .metadata.name,
          node: .spec.nodeName,
          phase: .status.phase,
          ready: ([.status.conditions[]? | select(.type == "Ready") | .status] | any(. == "True"))
        }
      ],
      distinctControlPlaneHosts: ([
        ($pods.items // [])[] |
        .spec.nodeName
      ] | unique | length),
      dependencyPods: $dependencies,
      safeControlPlaneTargetCount: ([
        ($pods.items // [])[]
        | .spec.nodeName as $node
        | select(([$dependencies[] | select(.node == $node)] | length) == 0)
      ] | length),
      podDisruptionBudget: (
        if $pdb == null then
          null
        else
          {
            name: $pdb.metadata.name,
            minAvailable: $pdb.spec.minAvailable,
            currentHealthy: $pdb.status.currentHealthy,
            desiredHealthy: $pdb.status.desiredHealthy
          }
        end
      )
    }'
}

select_safe_control_plane_target() {
  local override_node="${1:-}"
  local control_plane_pods_json all_pods_json
  control_plane_pods_json="$(get_control_plane_pods_json)"
  all_pods_json="$("${kube[@]}" -n "$namespace" get pods -o json)"
  jq -nec \
    --arg override "$override_node" \
    --argjson controlPlanePods "$control_plane_pods_json" \
    --argjson allPods "$all_pods_json" '
    [
      ($controlPlanePods.items // [])[]
      | {node: .spec.nodeName, pod: .metadata.name, podUid: .metadata.uid}
    ] as $controlPlanePlacements
    | [
        ($allPods.items // [])[]
        | select(
            .metadata.labels["app.kubernetes.io/name"] == "synara-stage2-postgres"
            or .metadata.labels["app.kubernetes.io/name"] == "synara-stage2-minio"
          )
        | .spec.nodeName
      ] as $dependencyNodes
    | (
        $controlPlanePlacements
        | sort_by(.node)
        | group_by(.node)
        | map(
            select(length == 1)
            | .[0]
            | select(.node as $node | ($dependencyNodes | index($node) | not))
          )
      ) as $safeTargets
    | (
        if $override == "" then
          $safeTargets[0]
        else
          ($safeTargets | map(select(.node == $override)) | .[0])
        end
      ) as $target
    | select($target != null)
    | {node: $target.node, pod: $target.pod, podUid: $target.podUid, colocatedDependencyPods: []}'
}

run_hook() {
  local phase="$1"
  local command_text="$2"
  local stdout_file="$work_dir/hook-${phase}.stdout"
  local stderr_file="$work_dir/hook-${phase}.stderr"
  local rc=0 status_json result_json
  if [[ -z "$command_text" ]]; then
    printf 'null\n'
    return 0
  fi
  rm -f "$stdout_file" "$stderr_file"
  if status_json="$(
    SYNARA_K8S_CONTEXT="$context" \
    SYNARA_K8S_NAMESPACE="$namespace" \
    SYNARA_K8S_HOOK_PHASE="$phase" \
    SYNARA_K8S_EVIDENCE_FILE="$evidence_file" \
      run_command_capture "$command_text" "$stdout_file" "$stderr_file" 0
  )"; then
    rc=0
  else
    rc=$?
  fi
  if [[ -z "$status_json" ]]; then
    status_json='{"exitCode":1,"timedOut":false}'
    rc=1
  fi
  result_json="$(redacted_command_result "$phase" "$command_text" "$status_json" "$stdout_file" "$stderr_file")"
  printf '%s\n' "$result_json"
  return "$rc"
}

clear_partition_hook_state() {
  PARTITION_HOOK_ACTIVE=0
  PARTITION_HOOK_ATTEMPTED=0
  PARTITION_HOOK_NODE=""
  PARTITION_HOOK_TARGET_JSON='null'
  PARTITION_HOOK_CHALLENGE=""
  PARTITION_HOOK_CHALLENGE_ISSUED_EPOCH=0
  PARTITION_HOOK_VERIFY_JSON='null'
  PARTITION_HOOK_OPERATION_ID=""
  PARTITION_HOOK_CONTROLLER_PID=""
  PARTITION_HOOK_CONTROLLER_IDENTITY=""
  PARTITION_HOOK_LAST_PHASE=""
  PARTITION_HOOK_NEEDS_RECOVERY=0
}

managed_hook_backend() {
  if [[ "$context" == "managed-validation" ]]; then
    printf 'process-group-test\n'
  else
    printf 'systemd-user\n'
  fi
}

controller_process_identity() {
  python3 - "$1" <<'PY'
import pathlib
import subprocess
import sys

pid = int(sys.argv[1])
if sys.platform.startswith("linux"):
    try:
        data = pathlib.Path(f"/proc/{pid}/stat").read_text(encoding="utf-8")
        close = data.rfind(")")
        fields = data[close + 2 :].split()
        print(f"linux-proc-start:{fields[19]}")
    except (OSError, IndexError, ValueError):
        raise SystemExit(1)
elif sys.platform == "darwin":
    completed = subprocess.run(
        ["ps", "-o", "lstart=", "-o", "ppid=", "-o", "pgid=", "-o", "comm=", "-p", str(pid)],
        capture_output=True,
        text=True,
        check=False,
        timeout=2,
    )
    value = completed.stdout.strip()
    if completed.returncode != 0 or not value:
        raise SystemExit(1)
    print(f"darwin-ps-identity:{' '.join(value.split())}")
else:
    raise SystemExit(1)
PY
}

process_identity_is_live() {
  local pid="$1" expected="$2"
  [[ "$pid" =~ ^[1-9][0-9]*$ && -n "$expected" ]] || return 1
  python3 - "$pid" "$expected" <<'PY'
import pathlib, subprocess, sys
pid, expected = int(sys.argv[1]), sys.argv[2]
try:
    if sys.platform.startswith("linux"):
        data = pathlib.Path(f"/proc/{pid}/stat").read_text(encoding="utf-8")
        close = data.rfind(")")
        fields = data[close + 2:].split()
        state, identity = fields[0], f"linux-proc-start:{fields[19]}"
    elif sys.platform == "darwin":
        state_run = subprocess.run(["ps", "-o", "stat=", "-p", str(pid)], capture_output=True, text=True, timeout=2)
        identity_run = subprocess.run(["ps", "-o", "lstart=", "-o", "ppid=", "-o", "pgid=", "-o", "comm=", "-p", str(pid)], capture_output=True, text=True, timeout=2)
        if state_run.returncode != 0 or identity_run.returncode != 0 or not state_run.stdout.strip() or not identity_run.stdout.strip():
            raise ValueError
        state = state_run.stdout.strip()[0]
        identity = f"darwin-ps-identity:{' '.join(identity_run.stdout.strip().split())}"
    else:
        raise ValueError
except (OSError, IndexError, ValueError, subprocess.SubprocessError):
    raise SystemExit(1)
raise SystemExit(0 if identity == expected and state not in {"Z", "X", "x"} else 1)
PY
}

process_is_terminal_state() {
  python3 - "$1" <<'PY'
import pathlib, subprocess, sys
pid = int(sys.argv[1])
try:
    if sys.platform.startswith("linux"):
        data = pathlib.Path(f"/proc/{pid}/stat").read_text(encoding="utf-8")
        state = data[data.rfind(")") + 2:].split()[0]
    else:
        result = subprocess.run(["ps", "-o", "stat=", "-p", str(pid)], capture_output=True, text=True, timeout=2)
        state = result.stdout.strip()[0]
except (OSError, IndexError, subprocess.SubprocessError):
    raise SystemExit(1)
raise SystemExit(0 if state in {"Z", "X", "x"} else 1)
PY
}

signal_owned_process() {
  local pid="$1" identity="$2" signal_name="${3:-TERM}"
  process_identity_is_live "$pid" "$identity" || return 1
  kill -s "$signal_name" "$pid"
}

run_managed_controller() {
  local request_file="$1"
  local result_file="$2"
  local controller_pid controller_identity ready_file release_file ready_json ready_pid rc=0 deadline
  ready_file="$work_dir/managed-controller-ready-${RANDOM}-$(date +%s%N 2>/dev/null || date +%s)"
  release_file="${ready_file}.release"
  rm -f "$ready_file" "$release_file"
  python3 - "$request_file" "$ready_file" "$release_file" "$managed_hook_controller" <<'PY' >"$result_file" &
import json, os, pathlib, subprocess, sys, time
request_path, ready_path, release_path, controller_path = sys.argv[1:5]
pid = os.getpid()
try:
    if sys.platform.startswith("linux"):
        data = pathlib.Path(f"/proc/{pid}/stat").read_text(encoding="utf-8")
        fields = data[data.rfind(")") + 2:].split()
        identity = f"linux-proc-start:{fields[19]}"
    elif sys.platform == "darwin":
        result = subprocess.run(
            ["ps", "-o", "lstart=", "-o", "ppid=", "-o", "pgid=", "-o", "comm=", "-p", str(pid)],
            capture_output=True, text=True, timeout=2,
        )
        if result.returncode != 0 or not result.stdout.strip():
            raise RuntimeError
        identity = f"darwin-ps-identity:{' '.join(result.stdout.strip().split())}"
    else:
        raise RuntimeError
    failure_file = os.environ.get("SYNARA_K8S_TEST_CONTROLLER_IDENTITY_FAILURE_FILE")
    if failure_file:
        try:
            marker = os.open(failure_file, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            os.close(marker)
            identity += "-injected-drift"
        except FileExistsError:
            pass
    descriptor = os.open(ready_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
        json.dump({"pid": pid, "identity": identity}, handle, separators=(",", ":"))
        handle.flush()
        os.fsync(handle.fileno())
    deadline = time.monotonic() + 5
    while not os.path.exists(release_path):
        if time.monotonic() >= deadline:
            raise SystemExit(125)
        time.sleep(0.01)
    request_fd = os.open(request_path, os.O_RDONLY)
    os.dup2(request_fd, 0)
    os.close(request_fd)
    os.execv(sys.executable, [sys.executable, controller_path])
except (OSError, RuntimeError, subprocess.SubprocessError):
    raise SystemExit(125)
PY
  controller_pid="$!"
  PARTITION_HOOK_CONTROLLER_PID="$controller_pid"
  deadline=$((SECONDS + 6))
  while [[ ! -s "$ready_file" ]]; do
    if ! kill -0 "$controller_pid" >/dev/null 2>&1 || (( SECONDS >= deadline )); then
      wait "$controller_pid" >/dev/null 2>&1 || true
      PARTITION_HOOK_CONTROLLER_PID=""
      PARTITION_HOOK_CONTROLLER_IDENTITY=""
      rm -f "$ready_file" "$release_file"
      return 125
    fi
    sleep 0.01
  done
  ready_json="$(cat "$ready_file" 2>/dev/null || true)"
  ready_pid="$(jq -r '.pid // empty' <<<"$ready_json" 2>/dev/null || true)"
  controller_identity="$(jq -r '.identity // empty' <<<"$ready_json" 2>/dev/null || true)"
  if [[ "$ready_pid" != "$controller_pid" ]] ||
    ! process_identity_is_live "$controller_pid" "$controller_identity"; then
    wait "$controller_pid" >/dev/null 2>&1 || true
    PARTITION_HOOK_CONTROLLER_PID=""
    PARTITION_HOOK_CONTROLLER_IDENTITY=""
    rm -f "$ready_file" "$release_file"
    return 125
  fi
  PARTITION_HOOK_CONTROLLER_IDENTITY="$controller_identity"
  : >"$release_file"
  if wait "$controller_pid"; then rc=0; else rc=$?; fi
  rm -f "$ready_file" "$release_file"
  return "$rc"
}

interrupt_managed_controller() {
  local pid="$PARTITION_HOOK_CONTROLLER_PID" identity="$PARTITION_HOOK_CONTROLLER_IDENTITY"
  [[ -n "$pid" ]] || return 0
  if process_identity_is_live "$pid" "$identity"; then
    signal_owned_process "$pid" "$identity" TERM >/dev/null 2>&1 || return 1
  elif kill -0 "$pid" >/dev/null 2>&1 && ! process_is_terminal_state "$pid"; then
    return 1
  fi
  wait "$pid" >/dev/null 2>&1 || true
  PARTITION_HOOK_CONTROLLER_PID=""
  PARTITION_HOOK_CONTROLLER_IDENTITY=""
  return 0
}
recover_managed_partition_phase() {
  local phase="$1"
  local backend security_boundary request_file result_file rc=0 operation_digest unit_seed unit_name unit_digest
  [[ -n "$PARTITION_HOOK_OPERATION_ID" ]] || return 125
  backend="$(managed_hook_backend)"
  security_boundary=true
  [[ "$backend" == "process-group-test" ]] && security_boundary=false
  request_file="$work_dir/managed-recover-$phase-request.json"
  result_file="$work_dir/managed-recover-$phase-result.json"
  jq -nc \
    --arg operationId "$PARTITION_HOOK_OPERATION_ID" \
    --arg phase "$phase" \
    --arg backend "$backend" \
    --argjson securityBoundary "$security_boundary" '
    {
      schemaVersion: "synara.managed-hook-controller.request.v1",
      action: "recover",
      operationId: $operationId,
      phase: $phase,
      backend: $backend,
      securityBoundary: $securityBoundary
    }' >"$request_file"
  chmod 600 "$request_file"
  if run_managed_controller "$request_file" "$result_file"; then
    rc=0
  else
    rc=$?
  fi
  operation_digest="$(sha256_text "$PARTITION_HOOK_OPERATION_ID")"
  unit_seed="$(jq -nc --arg operationId "$PARTITION_HOOK_OPERATION_ID" --arg phase "$phase" '{operationId:$operationId,phase:$phase}')"
  unit_name="synara-managed-hook-$phase-$(sha256_json "$unit_seed" | cut -c1-32).service"
  unit_digest="$(sha256_text "$unit_name")"
  (( rc == 0 )) && jq -e \
    --arg operationDigest "$operation_digest" --arg unitDigest "$unit_digest" \
    --arg phase "$phase" --arg backend "$backend" --argjson boundary "$security_boundary" '
    (keys | sort) == (["backend","cancelled","expectedTransition","hookExitCode","members","operationIdDigest","phase","reason","schemaVersion","scopeDigest","securityBoundary","status","terminationConfirmed","timedOut","unitFound","unitNameDigest"] | sort)
    and .schemaVersion == "synara.managed-hook-controller.result.v1"
    and .status == "recovered" and .reason == "unit-terminal"
    and .operationIdDigest == $operationDigest and .unitNameDigest == $unitDigest
    and .phase == $phase and .backend == $backend and .securityBoundary == $boundary
    and .scopeDigest == null and .expectedTransition == null
    and .hookExitCode == null and .timedOut == false and .cancelled == false
    and (.unitFound | type == "boolean") and .terminationConfirmed == true
    and (.members | keys | sort) == (["deadLower","deadUpper","executable","unreadable","zombie"] | sort)
    and ([.members[]] | all(type == "number" and . >= 0))
  ' "$result_file" >/dev/null 2>&1
}

project_execute_controller_result() {
  local result_file="$1" phase="$2" backend="$3" boundary="$4" rc="$5" target_json="$6"
  local operation_digest scope_json scope_digest unit_seed unit_name unit_digest transition
  operation_digest="$(sha256_text "$PARTITION_HOOK_OPERATION_ID")"
  scope_json="$(jq -nc --arg context "$context" --arg namespace "$namespace" \
    --arg node "$(jq -r '.node' <<<"$target_json")" --arg nodeUid "$(jq -r '.nodeUid' <<<"$target_json")" \
    --arg pod "$(jq -r '.pod' <<<"$target_json")" --arg podUid "$(jq -r '.podUid' <<<"$target_json")" \
    --arg challenge "$PARTITION_HOOK_CHALLENGE" \
    '{challenge:$challenge,context:$context,namespace:$namespace,node:$node,nodeUid:$nodeUid,pod:$pod,podUid:$podUid}')"
  scope_digest="$(sha256_json "$scope_json")"
  unit_seed="$(jq -nc --arg operationId "$PARTITION_HOOK_OPERATION_ID" --arg phase "$phase" '{operationId:$operationId,phase:$phase}')"
  unit_name="synara-managed-hook-$phase-$(sha256_json "$unit_seed" | cut -c1-32).service"
  unit_digest="$(sha256_text "$unit_name")"
  case "$phase" in start) transition=terminal-applied ;; verify) transition=applied ;; stop) transition=terminal-healed ;; *) return 1 ;; esac
  jq -ec --arg operationDigest "$operation_digest" --arg scopeDigest "$scope_digest" \
    --arg unitDigest "$unit_digest" --arg phase "$phase" --arg backend "$backend" \
    --arg transition "$transition" --argjson boundary "$boundary" --argjson rc "$rc" '
    def base_ok:
      .schemaVersion == "synara.managed-hook-controller.result.v1"
      and .operationIdDigest == $operationDigest and .scopeDigest == $scopeDigest
      and .unitNameDigest == $unitDigest and .phase == $phase and .backend == $backend
      and .securityBoundary == $boundary and .expectedTransition == $transition
      and (.status | type == "string") and (.reason | type == "string");
    def members_ok:
      (.members | keys | sort) == (["deadLower","deadUpper","executable","unreadable","zombie"] | sort)
      and ([.members[]] | all(type == "number" and . >= 0));
    def outcome_keys:
      (["backend","cancelled","expectedTransition","hookExitCode","members","operationIdDigest","phase","reason","schemaVersion","scopeDigest","securityBoundary","status","terminationConfirmed","timedOut","unitFound","unitNameDigest"] | sort);
    def success_keys:
      (outcome_keys + ["artifactDigest","checkCount","evidenceDigests"] | sort);
    . as $result |
    if $rc == 0 then
      (keys | sort) == success_keys and base_ok
      and .status == "passed" and .reason == "transition-proved"
      and .hookExitCode == 0 and .timedOut == false and .cancelled == false
      and (.unitFound | type == "boolean") and .terminationConfirmed == true and members_ok
      and (.artifactDigest | test("^[0-9a-f]{64}$"))
      and (.evidenceDigests | type == "array" and length > 0 and all(test("^[0-9a-f]{64}$")))
      and (.checkCount | type == "number") and .checkCount == (.evidenceDigests | length)
    else
      (keys | sort) == outcome_keys and base_ok and members_ok
      and (.status == "failed" or .status == "cancelled" or .status == "unsupported")
      and .reason != "transition-proved"
      and (.hookExitCode == null or (.hookExitCode | type == "number"))
      and (.timedOut | type == "boolean") and (.cancelled | type == "boolean")
      and (.unitFound | type == "boolean") and (.terminationConfirmed | type == "boolean")
      and (if .status == "cancelled" then .cancelled == true and .timedOut == false else .cancelled == false end)
      and (if .timedOut then .status == "failed" and .reason == "hook-timed-out" else true end)
      and (if .reason == "hook-exit-nonzero" then (.hookExitCode | type == "number") and .hookExitCode != 0 else true end)
    end
    | select(.)
    | $result
    | {
        schemaVersion,status,reason,securityBoundary,operationIdDigest,phase,backend,
        unitNameDigest,scopeDigest,expectedTransition,hookExitCode,timedOut,cancelled,
        unitFound,terminationConfirmed,members,
        artifactDigest:(.artifactDigest // null),evidenceDigests:(.evidenceDigests // []),checkCount:(.checkCount // 0)
      }
  ' "$result_file"
}

run_partition_hook() {
  local phase="$1"
  local command_text="$2"
  local node="$3"
  local target_json="${4:-null}"
  local reason="${5:-}"
  local timeout_seconds="$6"
  local target_pod target_pod_uid node_uid backend security_boundary test_only
  local artifact_file request_file result_file controller_json recovery_json='null'
  local rc=0 recovery_rc=0 identity_digest
  target_pod="$(jq -r '.pod // empty' <<<"$target_json")"
  target_pod_uid="$(jq -r '.podUid // empty' <<<"$target_json")"
  node_uid="$(jq -r '.nodeUid // empty' <<<"$target_json")"
  backend="$(managed_hook_backend)"
  security_boundary=true
  test_only=false
  if [[ "$backend" == "process-group-test" ]]; then
    security_boundary=false
    test_only=true
  fi
  artifact_file="$work_dir/managed-transition-${PARTITION_HOOK_OPERATION_ID}-$phase-${RANDOM}-$(date +%s).json"
  request_file="$work_dir/managed-$phase-request.json"
  result_file="$work_dir/managed-$phase-result.json"
  rm -f "$artifact_file" "$result_file"
  jq -nc \
    --arg operationId "$PARTITION_HOOK_OPERATION_ID" \
    --arg phase "$phase" \
    --arg backend "$backend" \
    --arg context "$context" \
    --arg namespace "$namespace" \
    --arg node "$node" \
    --arg nodeUid "$node_uid" \
    --arg pod "$target_pod" \
    --arg podUid "$target_pod_uid" \
    --arg challenge "$PARTITION_HOOK_CHALLENGE" \
    --arg command "$command_text" \
    --arg artifactPath "$artifact_file" \
    --argjson securityBoundary "$security_boundary" \
    --argjson timeoutSeconds "$timeout_seconds" '
    {
      schemaVersion: "synara.managed-hook-controller.request.v1",
      action: "execute",
      operationId: $operationId,
      phase: $phase,
      backend: $backend,
      securityBoundary: $securityBoundary,
      scope: {
        context: $context,
        namespace: $namespace,
        node: $node,
        nodeUid: $nodeUid,
        pod: $pod,
        podUid: $podUid,
        challenge: $challenge
      },
      command: ["/bin/sh", "-c", $command],
      artifactPath: $artifactPath,
      timeoutSeconds: $timeoutSeconds,
      stopTimeoutSeconds: 5,
      freshnessSeconds: 60
    }' >"$request_file"
  chmod 600 "$request_file"
  PARTITION_HOOK_LAST_PHASE="$phase"
  PARTITION_HOOK_NEEDS_RECOVERY=1
  if run_managed_controller "$request_file" "$result_file"; then
    rc=0
  else
    rc=$?
  fi
  if [[ -s "$result_file" ]] && controller_json="$(project_execute_controller_result \
    "$result_file" "$phase" "$backend" "$security_boundary" "$rc" "$target_json")"; then
    if (( rc == 0 )); then PARTITION_HOOK_NEEDS_RECOVERY=0; fi
  else
    controller_json='{"schemaVersion":"synara.managed-hook-controller.result.v1","status":"failed","reason":"invalid-controller-result","terminationConfirmed":false}'
    rc=125
  fi
  if (( rc != 0 )); then
    if recover_managed_partition_phase "$phase"; then
      recovery_rc=0
      recovery_json='{"status":"recovered","terminationConfirmed":true}'
      PARTITION_HOOK_NEEDS_RECOVERY=0
    else
      recovery_rc=$?
      recovery_json="$(jq -nc --argjson exitCode "$recovery_rc" '{status:"failed",terminationConfirmed:false,exitCode:$exitCode}')"
    fi
  fi
  identity_digest="$(printf '%s' "$PARTITION_HOOK_CONTROLLER_IDENTITY" | shasum -a 256 | awk '{print $1}')"
  PARTITION_HOOK_RESULT_JSON="$(jq -nc \
    --arg backend "$backend" \
    --arg reason "$reason" \
    --arg identityDigest "$identity_digest" \
    --argjson testOnly "$test_only" \
    --arg targetDigest "$(sha256_json "$target_json")" \
    --argjson timeoutSeconds "$timeout_seconds" \
    --argjson exitCode "$rc" \
    --argjson controller "$controller_json" \
    --argjson recovery "$recovery_json" '
    {
      backend: $backend,
      testOnly: $testOnly,
      securityBoundary: $controller.securityBoundary,
      targetDigest: $targetDigest,
      timeoutSeconds: $timeoutSeconds,
      reason: (if $reason == "" then null else $reason end),
      exitCode: $exitCode,
      timedOut: ($controller.timedOut // false),
      terminationConfirmed: ($controller.terminationConfirmed // false),
      controllerIdentityDigest: $identityDigest,
      controller: $controller,
      recovery: $recovery
    }
    | with_entries(select(.value != null))')"
  if (( recovery_rc != 0 )); then
    return 125
  fi
  return "$rc"
}

start_managed_partition_hook() {
  local node="$1"
  local target_json="$2"
  local start_json challenge operation_id rc=0
  PARTITION_HOOK_NODE="$node"
  PARTITION_HOOK_TARGET_JSON="$target_json"
  PARTITION_HOOK_ATTEMPTED=1
  PARTITION_HOOK_ACTIVE=1
  if ! challenge="$(python3 -c 'import secrets; print(secrets.token_hex(32))')" ||
    ! operation_id="$(python3 -c 'import secrets; print("managed-" + secrets.token_hex(24))')" ||
    [[ ! "$challenge" =~ ^[0-9a-f]{64}$ ]] ||
    [[ ! "$operation_id" =~ ^managed-[0-9a-f]{48}$ ]]; then
    PARTITION_HOOK_RESULT_JSON='{"error":"managed node partition operation identity generation failed","exitCode":125}'
    PARTITION_HOOK_START_JSON="$PARTITION_HOOK_RESULT_JSON"
    return 125
  fi
  PARTITION_HOOK_CHALLENGE="$challenge"
  PARTITION_HOOK_OPERATION_ID="$operation_id"
  PARTITION_HOOK_CHALLENGE_ISSUED_EPOCH="$(date +%s)"
  PARTITION_HOOK_START_JSON='null'
  PARTITION_HOOK_VERIFY_JSON='null'
  PARTITION_HOOK_STOP_JSON='null'
  PARTITION_HOOK_RESULT_JSON='null'
  if run_partition_hook start "$node_partition_start_hook" "$node" "$target_json" "case-start" "$node_partition_start_hook_timeout_seconds"; then
    rc=0
  else
    rc=$?
  fi
  start_json="$PARTITION_HOOK_RESULT_JSON"
  PARTITION_HOOK_RESULT_JSON="$start_json"
  PARTITION_HOOK_START_JSON="$start_json"
  return "$rc"
}

run_managed_partition_verification() {
  local node="$1"
  local target_json="$2"
  local verify_json rc=0
  if run_partition_hook verify "$node_partition_verify_hook" "$node" "$target_json" "observe-isolation" "$node_partition_verify_hook_timeout_seconds"; then
    rc=0
  else
    rc=$?
  fi
  verify_json="$PARTITION_HOOK_RESULT_JSON"
  PARTITION_HOOK_VERIFY_JSON="$verify_json"
  PARTITION_HOOK_RESULT_JSON="$verify_json"
  return "$rc"
}

stop_managed_partition_hook() {
  local reason="${1:-case-complete}"
  local stop_json rc=0 recovery_failed=0
  if [[ "$PARTITION_HOOK_ACTIVE" != "1" ]]; then
    PARTITION_HOOK_RESULT_JSON='null'
    printf 'null\n'
    return 0
  fi
  if ! interrupt_managed_controller; then
    recovery_failed=1
  fi
  if [[ "$PARTITION_HOOK_NEEDS_RECOVERY" == "1" && -n "$PARTITION_HOOK_LAST_PHASE" ]]; then
    if recover_managed_partition_phase "$PARTITION_HOOK_LAST_PHASE"; then
      PARTITION_HOOK_NEEDS_RECOVERY=0
    else
      recovery_failed=1
    fi
  fi
  if run_partition_hook stop "$node_partition_stop_hook" "$PARTITION_HOOK_NODE" "$PARTITION_HOOK_TARGET_JSON" "$reason" "$node_partition_stop_hook_timeout_seconds"; then
    rc=0
  else
    rc=$?
  fi
  stop_json="$PARTITION_HOOK_RESULT_JSON"
  PARTITION_HOOK_RESULT_JSON="$stop_json"
  PARTITION_HOOK_STOP_JSON="$stop_json"
  if (( recovery_failed != 0 )); then
    return 125
  fi
  if (( rc == 0 )); then
    clear_partition_hook_state
  fi
  return "$rc"
}

append_case_result() {
  local case_name="$1"
  local status="$2"
  local started_case_at="$3"
  local finished_case_at="$4"
  local details_json="$5"
  local progress_json
  details_json="$(sanitize_managed_details "$case_name" "$details_json")"
  jq -nc \
    --arg name "$case_name" \
    --arg status "$status" \
    --arg startedAt "$started_case_at" \
    --arg finishedAt "$finished_case_at" \
    --argjson details "$details_json" \
    '{
      name: $name,
      status: $status,
      startedAt: $startedAt,
      finishedAt: $finishedAt,
      details: $details
    }' >>"$scenarios_file"
  progress_json="$(jq -nc \
    --arg scope "top-level-case" \
    --arg caseName "$case_name" \
    --arg status "$status" \
    --arg startedAt "$started_case_at" \
    --arg finishedAt "$finished_case_at" \
    --argjson details "$(sanitize_progress_details "$details_json")" '
    {
      scope: $scope,
      case: $caseName,
      status: $status,
      startedAt: $startedAt,
      finishedAt: $finishedAt,
      details: $details
    }')"
  record_progress_update "case-completed" "$progress_json"
}

case_rbac() {
  local role_json
  : >"$permissions_file"
  if ! role_json="$("${kube[@]}" get clusterrole "$rbac_name" -o json)"; then
    CASE_DETAILS_JSON='{"error":"failed to read cluster role for RBAC validation"}'
    return 1
  fi
  run_rbac_check "$role_json" true create tokenreviews authentication.k8s.io "" || return 1
  run_rbac_check "$role_json" true get priorityclasses scheduling.k8s.io "" || return 1
  run_rbac_check "$role_json" true get namespaces "" "" || return 1
  run_rbac_check "$role_json" true create namespaces "" "" || return 1
  run_rbac_check "$role_json" true patch namespaces "" "" || return 1
  run_rbac_check "$role_json" true get pods "" "$namespace" || return 1
  run_rbac_check "$role_json" true list pods "" "$namespace" || return 1
  run_rbac_check "$role_json" true create pods "" "$namespace" || return 1
  run_rbac_check "$role_json" true patch pods "" "$namespace" || return 1
  run_rbac_check "$role_json" true delete pods "" "$namespace" || return 1
  run_rbac_check "$role_json" true get serviceaccounts "" "$namespace" || return 1
  run_rbac_check "$role_json" true create serviceaccounts "" "$namespace" || return 1
  run_rbac_check "$role_json" true patch serviceaccounts "" "$namespace" || return 1
  run_rbac_check "$role_json" true get secrets "" "$namespace" || return 1
  run_rbac_check "$role_json" true create secrets "" "$namespace" || return 1
  run_rbac_check "$role_json" true patch secrets "" "$namespace" || return 1
  run_rbac_check "$role_json" true get resourcequotas "" "$namespace" || return 1
  run_rbac_check "$role_json" true create resourcequotas "" "$namespace" || return 1
  run_rbac_check "$role_json" true patch resourcequotas "" "$namespace" || return 1
  run_rbac_check "$role_json" true get networkpolicies networking.k8s.io "$namespace" || return 1
  run_rbac_check "$role_json" true create networkpolicies networking.k8s.io "$namespace" || return 1
  run_rbac_check "$role_json" true patch networkpolicies networking.k8s.io "$namespace" || return 1
  run_rbac_check "$role_json" false delete namespaces "" "" || return 1
  run_rbac_check "$role_json" false delete secrets "" "$namespace" || return 1
  run_rbac_check "$role_json" false delete serviceaccounts "" "$namespace" || return 1
  run_rbac_check "$role_json" false delete resourcequotas "" "$namespace" || return 1
  run_rbac_check "$role_json" false delete networkpolicies networking.k8s.io "$namespace" || return 1
  run_rbac_check "$role_json" false update pods "" "$namespace" || return 1
  run_rbac_check "$role_json" false watch pods "" "$namespace" || return 1
  run_rbac_check "$role_json" false create priorityclasses scheduling.k8s.io "" || return 1
  run_rbac_check "$role_json" false patch priorityclasses scheduling.k8s.io "" || return 1
  run_rbac_check "$role_json" false delete priorityclasses scheduling.k8s.io "" || return 1
  CASE_DETAILS_JSON="$(jq -nc \
    --argjson role "$role_json" \
    --argjson checks "$(permissions_results_json)" \
    '{clusterRoleName: $role.metadata.name, rules: $role.rules, checks: $checks}')"
}

case_topology() {
  local worker_node_count control_plane_pod_count distinct_control_plane_hosts safe_target_count
  if ! topology_json="$(collect_topology_json)"; then
    CASE_DETAILS_JSON='{"error":"failed to collect topology evidence"}'
    return 1
  fi
  if ! worker_node_count="$(jq -er '.workerNodeCount' <<<"$topology_json")"; then
    CASE_DETAILS_JSON="$(jq -nc --arg rawTopology "$topology_json" \
      '{error: "failed to read topology workerNodeCount", rawTopology: $rawTopology}')"
    return 1
  fi
  if (( worker_node_count < min_worker_nodes )); then
    CASE_DETAILS_JSON="$topology_json"
    return 1
  fi
  if ! control_plane_pod_count="$(jq -er '.controlPlanePods | length' <<<"$topology_json")"; then
    CASE_DETAILS_JSON="$(jq -nc --arg rawTopology "$topology_json" \
      '{error: "failed to read topology controlPlanePods count", rawTopology: $rawTopology}')"
    return 1
  fi
  if (( control_plane_pod_count < 2 )); then
    CASE_DETAILS_JSON="$topology_json"
    return 1
  fi
  if ! distinct_control_plane_hosts="$(jq -er '.distinctControlPlaneHosts' <<<"$topology_json")"; then
    CASE_DETAILS_JSON="$(jq -nc --arg rawTopology "$topology_json" \
      '{error: "failed to read topology distinctControlPlaneHosts", rawTopology: $rawTopology}')"
    return 1
  fi
  if (( distinct_control_plane_hosts < 2 )); then
    CASE_DETAILS_JSON="$topology_json"
    return 1
  fi
  if ! safe_target_count="$(jq -er '.safeControlPlaneTargetCount' <<<"$topology_json")"; then
    CASE_DETAILS_JSON="$(jq -nc --arg rawTopology "$topology_json" \
      '{error: "failed to read topology safeControlPlaneTargetCount", rawTopology: $rawTopology}')"
    return 1
  fi
  if (( safe_target_count < 1 )); then
    CASE_DETAILS_JSON="$topology_json"
    return 1
  fi
  if ! jq -e '.podDisruptionBudget != null and (.podDisruptionBudget.minAvailable | tostring) == "1"' \
    <<<"$topology_json" >/dev/null; then
    CASE_DETAILS_JSON="$topology_json"
    return 1
  fi
  CASE_DETAILS_JSON="$topology_json"
}

case_leader_takeover() {
  local lease_name="synara:kubernetes-execution-reconciler"
  local before_lease before_holder before_token before_pod before_pod_uid before_json
  local after_lease after_holder after_token after_pod after_json
  local failures_file="$work_dir/leader-takeover-probe-failures"
  local probe_stop_file="$work_dir/leader-takeover-probe-stop"
  local lease_guard_file="$work_dir/leader-takeover-lease-guard"
  local delete_output_file="$work_dir/leader-takeover-delete-output"
  local takeover_rc failures guard_app guard_state guard_output current_lease guard_error guard_pod guard_pod_uid
  local deletion_json='null'

  start_continuous_probe "$probe_stop_file" "$failures_file"
  if ! start_reconciler_takeover_guard "$lease_name" "$lease_guard_file" "$case_timeout_seconds"; then
    guard_app="$LEASE_GUARD_APP"
    guard_error="$LEASE_GUARD_ERROR"
    guard_pod="$LEASE_GUARD_POD"
    guard_pod_uid="$LEASE_GUARD_POD_UID"
    stop_reconciler_takeover_guard
    finish_continuous_probe "$probe_stop_file" || true
    failures="$(cat "$failures_file" 2>/dev/null || printf 0)"
    guard_state="$(cat "$lease_guard_file.state" 2>/dev/null || true)"
    guard_output="$(read_reconciler_takeover_guard_diagnostic "$lease_guard_file" "$LEASE_GUARD_NONCE" || true)"
    current_lease="$(read_reconciler_lease "$lease_name" 2>/dev/null || true)"
    CASE_DETAILS_JSON="$(jq -nc --arg lease "$lease_name" --arg guardApp "$guard_app" \
      --arg guardState "$guard_state" --arg guardOutput "$guard_output" --arg currentLease "$current_lease" \
      --arg guardFailure "$guard_error" --arg postgresPod "$guard_pod" --arg postgresPodUid "$guard_pod_uid" \
      --argjson failures "$failures" '
      {error: "active reconciler lease could not be guarded", leaseName: $lease,
       guardApplicationName: $guardApp, guardState: $guardState, guardOutput: $guardOutput,
       guardFailure: $guardFailure, postgresPod: $postgresPod, postgresPodUid: $postgresPodUid,
       guardOutputRedacted: true, guardOutputMaxBytes: 2000,
       currentLease: $currentLease, readyProbeFailures: $failures}')"
    return 1
  fi
  before_lease="$LEASE_GUARD_ROW"
  before_holder="${before_lease%%|*}"
  before_token="${before_lease##*|}"
  before_pod="${before_holder%%:*}"
  if ! before_json="$(get_control_plane_pods_json)"; then
    stop_reconciler_takeover_guard
    finish_continuous_probe "$probe_stop_file" || true
    failures="$(read_probe_failures "$failures_file")"
    CASE_DETAILS_JSON="$(jq -nc --arg lease "$lease_name" --arg holder "$before_holder" --argjson token "$before_token" \
      --argjson failures "$failures" '
      {error: "failed to list control plane Pods before leader takeover", leaseName: $lease,
       before: {holderId: $holder, fencingToken: $token}, readyProbeFailures: $failures}')"
    return 1
  fi
  if ! before_pod_uid="$(jq -er --arg pod "$before_pod" '
    [(.items // [])[]
      | select(.metadata.name == $pod)
      | select(.metadata.deletionTimestamp == null)
      | select(.status.phase == "Running")
      | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))
      | .metadata.uid]
    | if length == 1 and (.[0] | type == "string") and (.[0] | length > 0) then .[0] else empty end
  ' <<<"$before_json")"; then
    stop_reconciler_takeover_guard
    finish_continuous_probe "$probe_stop_file" || true
    failures="$(cat "$failures_file" 2>/dev/null || printf 0)"
    CASE_DETAILS_JSON="$(jq -nc --arg lease "$lease_name" --arg holder "$before_holder" --argjson token "$before_token" '
      {error: "reconciler lease holder is not a Ready Control Plane Pod", leaseName: $lease,
       before: {holderId: $holder, fencingToken: $token}}')"
    return 1
  fi

  if ! assert_reconciler_takeover_guard_active; then
    guard_error="$LEASE_GUARD_ERROR"
    guard_output="$(read_reconciler_takeover_guard_diagnostic "$lease_guard_file" "$LEASE_GUARD_NONCE" || true)"
    stop_reconciler_takeover_guard
    finish_continuous_probe "$probe_stop_file" || true
    failures="$(cat "$failures_file" 2>/dev/null || printf 0)"
    CASE_DETAILS_JSON="$(jq -nc --arg lease "$lease_name" --arg holder "$before_holder" --argjson token "$before_token" \
      --arg guardFailure "$guard_error" --arg guardOutput "$guard_output" \
      --arg postgresPod "$LEASE_GUARD_POD" --arg postgresPodUid "$LEASE_GUARD_POD_UID" \
      --argjson failures "$failures" '
      {error: "reconciler takeover guard was not active immediately before leader deletion", leaseName: $lease,
       before: {holderId: $holder, fencingToken: $token}, guardFailure: $guardFailure,
       postgresPod: $postgresPod, postgresPodUid: $postgresPodUid,
       guardOutput: $guardOutput, guardOutputRedacted: true, guardOutputMaxBytes: 2000,
       readyProbeFailures: $failures}')"
    return 1
  fi

  if ! delete_pod_uid_preconditioned "$before_pod" "$before_pod_uid" "$delete_output_file"; then
    deletion_json="$POD_DELETE_RECONCILIATION"
    stop_reconciler_takeover_guard
    finish_continuous_probe "$probe_stop_file" || true
    failures="$(cat "$failures_file" 2>/dev/null || printf 0)"
    CASE_DETAILS_JSON="$(jq -nc --arg lease "$lease_name" --arg pod "$before_pod" --arg podUid "$before_pod_uid" \
      --argjson deletion "$deletion_json" --argjson failures "$failures" '
      {error: "reconciler leader Pod deletion could not be reconciled", leaseName: $lease,
       deletedPod: $pod, deletedPodUID: $podUid, deletion: $deletion, readyProbeFailures: $failures}')"
    return 1
  fi
  deletion_json="$POD_DELETE_RECONCILIATION"
  if ! release_reconciler_takeover_guard "$deletion_json"; then
    guard_error="$LEASE_GUARD_ERROR"
    guard_output="$(read_reconciler_takeover_guard_diagnostic "$lease_guard_file" "$LEASE_GUARD_NONCE" || true)"
    stop_reconciler_takeover_guard
    finish_continuous_probe "$probe_stop_file" || true
    failures="$(cat "$failures_file" 2>/dev/null || printf 0)"
    CASE_DETAILS_JSON="$(jq -nc --arg lease "$lease_name" --arg pod "$before_pod" --arg podUid "$before_pod_uid" \
      --argjson deletion "$deletion_json" --arg guardFailure "$guard_error" --arg guardOutput "$guard_output" \
      --arg postgresPod "$LEASE_GUARD_POD" --arg postgresPodUid "$LEASE_GUARD_POD_UID" --argjson failures "$failures" '
      {error: "reconciler takeover guard explicit unlock failed", leaseName: $lease,
       deletedPod: $pod, deletedPodUID: $podUid, deletion: $deletion, guardFailure: $guardFailure,
       postgresPod: $postgresPod, postgresPodUid: $postgresPodUid,
       guardOutput: $guardOutput, guardOutputRedacted: true, guardOutputMaxBytes: 2000,
       readyProbeFailures: $failures}')"
    return 1
  fi
  if ! finish_reconciler_takeover_guard; then
    guard_error="$LEASE_GUARD_ERROR"
    guard_output="$(read_reconciler_takeover_guard_diagnostic "$lease_guard_file" "$LEASE_GUARD_NONCE" || true)"
    stop_reconciler_takeover_guard
    finish_continuous_probe "$probe_stop_file" || true
    failures="$(cat "$failures_file" 2>/dev/null || printf 0)"
    CASE_DETAILS_JSON="$(jq -nc --arg lease "$lease_name" --arg pod "$before_pod" --arg podUid "$before_pod_uid" \
      --argjson deletion "$deletion_json" --arg guardFailure "$guard_error" \
      --arg guardOutput "$guard_output" --arg postgresPod "$LEASE_GUARD_POD" --arg postgresPodUid "$LEASE_GUARD_POD_UID" \
      --argjson failures "$failures" '
      {error: "reconciler takeover guard failed after leader deletion was submitted", leaseName: $lease,
       deletedPod: $pod, deletedPodUID: $podUid, deletion: $deletion, guardFailure: $guardFailure,
       postgresPod: $postgresPod, postgresPodUid: $postgresPodUid,
       guardOutput: $guardOutput, guardOutputRedacted: true, guardOutputMaxBytes: 2000,
       readyProbeFailures: $failures}')"
    return 1
  fi
  if ! wait_for_pod_uid_absent "$before_pod" "$before_pod_uid" "$case_timeout_seconds"; then
    finish_continuous_probe "$probe_stop_file" || true
    failures="$(cat "$failures_file" 2>/dev/null || printf 0)"
    CASE_DETAILS_JSON="$(jq -nc --arg lease "$lease_name" --arg pod "$before_pod" --arg podUid "$before_pod_uid" \
      --argjson deletion "$deletion_json" --argjson failures "$failures" '
      {error: "original reconciler leader Pod UID did not disappear", leaseName: $lease,
       deletedPod: $pod, deletedPodUID: $podUid, deletion: $deletion, readyProbeFailures: $failures}')"
    return 1
  fi

  if after_lease="$(wait_for_reconciler_takeover "$lease_name" "$before_holder" "$before_token")"; then
    takeover_rc=0
  else
    takeover_rc=$?
  fi
  if (( takeover_rc != 0 )); then
    finish_continuous_probe "$probe_stop_file" || true
    failures="$(cat "$failures_file" 2>/dev/null || printf 0)"
    CASE_DETAILS_JSON="$(jq -nc --arg lease "$lease_name" --arg holder "$before_holder" --arg podUid "$before_pod_uid" \
      --argjson deletion "$deletion_json" --argjson token "$before_token" --argjson failures "$failures" \
      --argjson skipped "$([[ "$takeover_rc" == "2" ]] && printf true || printf false)" '
      {error: (if $skipped then "reconciler fencing token skipped an epoch" else "reconciler lease did not transfer" end),
       leaseName: $lease, deletedPodUID: $podUid, deletion: $deletion,
       before: {holderId: $holder, fencingToken: $token},
       readyProbeFailures: $failures}')"
    return 1
  fi
  after_holder="${after_lease%%|*}"
  after_token="${after_lease##*|}"
  after_pod="${after_holder%%:*}"
  if ! wait_for_control_plane_ready; then
    finish_continuous_probe "$probe_stop_file" || true
    failures="$(cat "$failures_file" 2>/dev/null || printf 0)"
    CASE_DETAILS_JSON="$(jq -nc --arg lease "$lease_name" --arg pod "$before_pod" --arg podUid "$before_pod_uid" \
      --argjson deletion "$deletion_json" --arg holder "$after_holder" --argjson token "$after_token" --argjson failures "$failures" '
      {error: "control plane did not become ready after leader takeover", leaseName: $lease,
       deletedPod: $pod, deletedPodUID: $podUid, deletion: $deletion,
       after: {holderId: $holder, fencingToken: $token},
       readyProbeFailures: $failures}')"
    return 1
  fi
  if ! after_json="$(get_control_plane_pods_json)"; then
    finish_continuous_probe "$probe_stop_file" || true
    failures="$(read_probe_failures "$failures_file")"
    CASE_DETAILS_JSON="$(jq -nc --arg lease "$lease_name" --arg holder "$after_holder" --arg podUid "$before_pod_uid" \
      --argjson deletion "$deletion_json" --argjson token "$after_token" --argjson failures "$failures" '
      {error: "failed to list control plane Pods after leader takeover", leaseName: $lease,
       deletedPodUID: $podUid, deletion: $deletion,
       after: {holderId: $holder, fencingToken: $token}, readyProbeFailures: $failures}')"
    return 1
  fi
  if ! jq -e --arg pod "$after_pod" '
    any(.items[]?;
      .metadata.name == $pod
      and .metadata.deletionTimestamp == null
      and .status.phase == "Running"
      and any(.status.conditions[]?; .type == "Ready" and .status == "True")
    )
  ' <<<"$after_json" >/dev/null; then
    finish_continuous_probe "$probe_stop_file" || true
    failures="$(read_probe_failures "$failures_file")"
    CASE_DETAILS_JSON="$(jq -nc --arg lease "$lease_name" --arg holder "$after_holder" --arg podUid "$before_pod_uid" \
      --argjson deletion "$deletion_json" --argjson token "$after_token" --argjson failures "$failures" '
      {error: "new reconciler lease holder is not a Ready Control Plane Pod", leaseName: $lease,
       deletedPodUID: $podUid, deletion: $deletion,
       after: {holderId: $holder, fencingToken: $token}, readyProbeFailures: $failures}')"
    return 1
  fi
  if ! finish_continuous_probe "$probe_stop_file"; then
    CASE_DETAILS_JSON="$(jq -nc --arg lease "$lease_name" --arg holder "$after_holder" --arg podUid "$before_pod_uid" \
      --argjson deletion "$deletion_json" --argjson token "$after_token" '
      {error: "continuous readiness probe failed during leader takeover", leaseName: $lease,
       deletedPodUID: $podUid, deletion: $deletion,
       after: {holderId: $holder, fencingToken: $token}}')"
    return 1
  fi
  failures="$(read_probe_failures "$failures_file")"
  if (( failures > max_failover_ready_failures )); then
    CASE_DETAILS_JSON="$(jq -nc --arg lease "$lease_name" --arg beforeHolder "$before_holder" --arg deletedPodUID "$before_pod_uid" \
      --arg afterHolder "$after_holder" --arg beforePod "$before_pod" --arg afterPod "$after_pod" \
      --argjson deletion "$deletion_json" --argjson beforeToken "$before_token" --argjson afterToken "$after_token" --argjson failures "$failures" '
      {error: "readiness probe failures exceeded leader takeover threshold", leaseName: $lease,
       deletedPodUID: $deletedPodUID, deletion: $deletion,
       before: {holderId: $beforeHolder, pod: $beforePod, podUID: $deletedPodUID, fencingToken: $beforeToken},
       after: {holderId: $afterHolder, pod: $afterPod, fencingToken: $afterToken},
       readyProbeFailures: $failures}')"
    return 1
  fi
  CASE_DETAILS_JSON="$(jq -nc --arg lease "$lease_name" --arg beforeHolder "$before_holder" --arg deletedPodUID "$before_pod_uid" \
    --arg postgresPod "$LEASE_GUARD_POD" --arg postgresPodUid "$LEASE_GUARD_POD_UID" \
    --arg afterHolder "$after_holder" --arg beforePod "$before_pod" --arg afterPod "$after_pod" \
    --argjson deletion "$deletion_json" --argjson beforeToken "$before_token" --argjson afterToken "$after_token" --argjson failures "$failures" '
    {leaseName: $lease,
     guardProtocol: "v2", guardPostgresPod: $postgresPod, guardPostgresPodUID: $postgresPodUid,
     deletedPodUID: $deletedPodUID, deletion: $deletion,
     before: {holderId: $beforeHolder, pod: $beforePod, podUID: $deletedPodUID, fencingToken: $beforeToken},
     after: {holderId: $afterHolder, pod: $afterPod, fencingToken: $afterToken},
     readyProbeFailures: $failures}')"
}

case_control_plane_failover() {
  local before_json after_json deleted_pod replacement_json
  local failures_file="$work_dir/failover-probe-failures"
  local probe_pid pre_hook_json post_hook_json
  local session_authority_json sentinel_row sentinel_id before_digest sentinel_id_digest
  local after_sentinel_row after_sentinel_count after_digest
  if ! before_json="$(get_control_plane_pods_json)"; then
    CASE_DETAILS_JSON='{"error":"failed to list control plane Pods before failover"}'
    return 1
  fi
  deleted_pod="${SYNARA_K8S_RESILIENCE_FAILOVER_POD:-$(jq -r '.items | sort_by(.metadata.creationTimestamp) | .[0].metadata.name' <<<"$before_json")}"
  if [[ -z "$deleted_pod" || "$deleted_pod" == "null" ]]; then
    CASE_DETAILS_JSON='{"error":"no control-plane pod available for failover"}'
    return 1
  fi

  if [[ "$session_authority_mode" == "stage2-postgres" ]]; then
    if ! sentinel_row="$(create_session_authority_sentinel)"; then
      CASE_DETAILS_JSON='{"error":"failed to create the pre-failover Session authority sentinel","sessionAuthority":{"mode":"stage2-postgres","verified":false}}'
      return 1
    fi
    sentinel_id="${sentinel_row%%|*}"
    before_digest="${sentinel_row##*|}"
    sentinel_id_digest="$(sha256_text "$sentinel_id")"
    session_authority_json="$(jq -nc \
      --arg sentinelIdDigest "$sentinel_id_digest" \
      --arg beforeDigest "$before_digest" '
      {
        mode: "stage2-postgres",
        verified: false,
        sentinelIdDigest: $sentinelIdDigest,
        beforeDigest: $beforeDigest,
        afterDigest: null,
        rowCount: null
      }')"
  else
    session_authority_json='{"mode":"disabled","verified":false,"reason":"not-configured"}'
  fi

  pre_hook_json="$(run_hook pre-failover "${SYNARA_K8S_RESILIENCE_PRE_FAILOVER_HOOK:-}")" || {
    CASE_DETAILS_JSON="$(jq -nc --argjson hook "$pre_hook_json" '{error: "pre-failover hook failed", hook: $hook}')"
    return 1
  }
  start_probe_window "$disruption_probe_window_seconds" "$failures_file"
  probe_pid="$PROBE_PID"
  if ! "${kube[@]}" -n "$namespace" delete pod "$deleted_pod" --wait=false >/dev/null; then
    stop_probe_window
    CASE_DETAILS_JSON="$(jq -nc --arg pod "$deleted_pod" --argjson failures "$(read_probe_failures "$failures_file")" \
      '{error: "control plane Pod deletion failed during failover", deletedPod: $pod, readyProbeFailures: $failures}')"
    return 1
  fi
  if ! "${kube[@]}" -n "$namespace" wait --for=delete "pod/$deleted_pod" --timeout="${case_timeout_seconds}s" >/dev/null; then
    stop_probe_window
    CASE_DETAILS_JSON="$(jq -nc --arg pod "$deleted_pod" --argjson failures "$(read_probe_failures "$failures_file")" \
      '{error: "control plane Pod did not terminate during failover", deletedPod: $pod, readyProbeFailures: $failures}')"
    return 1
  fi
  if ! wait "$probe_pid"; then
    PROBE_PID=""
    CASE_DETAILS_JSON="$(jq -nc --arg pod "$deleted_pod" \
      '{error: "readiness probe process failed during control plane failover", deletedPod: $pod}')"
    return 1
  fi
  PROBE_PID=""
  if ! wait_for_control_plane_ready; then
    CASE_DETAILS_JSON="$(jq -nc --arg pod "$deleted_pod" --argjson failures "$(read_probe_failures "$failures_file")" \
      '{error: "control plane did not become ready after failover", deletedPod: $pod, readyProbeFailures: $failures}')"
    return 1
  fi
  if ! after_json="$(get_control_plane_pods_json)"; then
    CASE_DETAILS_JSON="$(jq -nc --arg pod "$deleted_pod" --argjson failures "$(read_probe_failures "$failures_file")" \
      '{error: "failed to list control plane Pods after failover", deletedPod: $pod, readyProbeFailures: $failures}')"
    return 1
  fi
  if [[ "$session_authority_mode" == "stage2-postgres" ]]; then
    if ! after_sentinel_row="$(read_session_authority_sentinel "$sentinel_id")" ||
      [[ ! "$after_sentinel_row" =~ ^[0-9]+\|([0-9a-f]{64}|-)$ ]]; then
      CASE_DETAILS_JSON="$(jq -nc --arg pod "$deleted_pod" \
        --argjson failures "$(read_probe_failures "$failures_file")" \
        --argjson sessionAuthority "$session_authority_json" '
        {
          error: "failed to read the post-failover Session authority sentinel",
          deletedPod: $pod,
          readyProbeFailures: $failures,
          sessionAuthority: $sessionAuthority
        }')"
      return 1
    fi
    after_sentinel_count="${after_sentinel_row%%|*}"
    after_digest="${after_sentinel_row##*|}"
    session_authority_json="$(jq -nc \
      --arg sentinelIdDigest "$sentinel_id_digest" \
      --arg beforeDigest "$before_digest" \
      --arg afterDigest "$after_digest" \
      --argjson rowCount "$after_sentinel_count" '
      {
        mode: "stage2-postgres",
        verified: ($rowCount == 1 and $beforeDigest == $afterDigest),
        sentinelIdDigest: $sentinelIdDigest,
        beforeDigest: $beforeDigest,
        afterDigest: $afterDigest,
        rowCount: $rowCount
      }')"
    if [[ "$after_sentinel_count" != "1" || "$after_digest" != "$before_digest" ]]; then
      CASE_DETAILS_JSON="$(jq -nc --arg pod "$deleted_pod" \
        --argjson failures "$(read_probe_failures "$failures_file")" \
        --argjson sessionAuthority "$session_authority_json" '
        {
          error: "Session authority changed across Control Plane Pod failover",
          deletedPod: $pod,
          readyProbeFailures: $failures,
          sessionAuthority: $sessionAuthority
        }')"
      return 1
    fi
  fi
  if ! replacement_json="$(jq -nc --argjson before "$before_json" --argjson after "$after_json" '
    {
      beforePods: [($before.items // [])[] | {name: .metadata.name, node: .spec.nodeName}],
      afterPods: [($after.items // [])[] | {name: .metadata.name, node: .spec.nodeName}],
      replacementPods: [
        (($after.items // [])[] | {name: .metadata.name, node: .spec.nodeName}) as $pod
        | select(([($before.items // [])[] | .metadata.name] | index($pod.name)) | not)
        | $pod
      ]
    }')"; then
    CASE_DETAILS_JSON="$(jq -nc --arg pod "$deleted_pod" --argjson failures "$(read_probe_failures "$failures_file")" \
      '{error: "failed to summarize control plane replacement Pods after failover", deletedPod: $pod, readyProbeFailures: $failures}')"
    return 1
  fi
  post_hook_json="$(run_hook post-failover "${SYNARA_K8S_RESILIENCE_POST_FAILOVER_HOOK:-}")" || {
    CASE_DETAILS_JSON="$(jq -nc --arg pod "$deleted_pod" --argjson failures "$(read_probe_failures "$failures_file")" \
      --argjson replacement "$replacement_json" --argjson hook "$post_hook_json" \
      --argjson sessionAuthority "$session_authority_json" '
      {
        error: "post-failover hook failed",
        deletedPod: $pod,
        readyProbeFailures: $failures,
        replacement: $replacement,
        sessionAuthority: $sessionAuthority,
        hook: $hook
      }')"
    return 1
  }
  local failover_failures
  failover_failures="$(read_probe_failures "$failures_file")"
  if (( failover_failures > max_failover_ready_failures )); then
    CASE_DETAILS_JSON="$(jq -nc --arg pod "$deleted_pod" --argjson failures "$failover_failures" \
      --argjson replacement "$replacement_json" --argjson preHook "$pre_hook_json" --argjson postHook "$post_hook_json" \
      --argjson sessionAuthority "$session_authority_json" '
      {
        error: "readiness probe failures exceeded failover threshold",
        deletedPod: $pod,
        readyProbeFailures: $failures,
        replacement: $replacement,
        sessionAuthority: $sessionAuthority,
        preHook: $preHook,
        postHook: $postHook
      }')"
    return 1
  fi
  CASE_DETAILS_JSON="$(jq -nc --arg pod "$deleted_pod" --argjson failures "$failover_failures" \
    --argjson replacement "$replacement_json" --argjson preHook "$pre_hook_json" --argjson postHook "$post_hook_json" \
    --argjson sessionAuthority "$session_authority_json" '
    {
      deletedPod: $pod,
      readyProbeFailures: $failures,
      replacement: $replacement,
      sessionAuthority: $sessionAuthority,
      preHook: $preHook,
      postHook: $postHook
    }')"
}

case_node_drain() {
  local target_json node pod before_json after_json
  local failures_file="$work_dir/drain-probe-failures"
  local probe_pid drain_failures
  local cordoned=0
  if ! target_json="$(select_safe_control_plane_target "${SYNARA_K8S_RESILIENCE_DRAIN_NODE:-}")"; then
    CASE_DETAILS_JSON='{"reason":"no safe single-control-plane node without stage2 dependencies was available for bounded drain simulation"}'
    return 2
  fi
  if ! node="$(jq -er '.node' <<<"$target_json")"; then
    CASE_DETAILS_JSON="$(jq -nc --arg rawTarget "$target_json" \
      '{error: "failed to read drain target node", rawTarget: $rawTarget}')"
    return 1
  fi
  if ! pod="$(jq -er '.pod' <<<"$target_json")"; then
    CASE_DETAILS_JSON="$(jq -nc --arg node "$node" --arg rawTarget "$target_json" \
      '{error: "failed to read drain target Pod", node: $node, rawTarget: $rawTarget}')"
    return 1
  fi
  if ! before_json="$(get_control_plane_pods_json)"; then
    CASE_DETAILS_JSON="$(jq -nc --arg node "$node" --arg pod "$pod" \
      '{error: "failed to list control plane Pods before drain simulation", node: $node, deletedPod: $pod}')"
    return 1
  fi
  if ! "${kube[@]}" cordon "$node" >/dev/null; then
    CASE_DETAILS_JSON="$(jq -nc --arg node "$node" '{error: "failed to cordon node for drain simulation", node: $node}')"
    return 1
  fi
  cordoned=1
  CORDONED_NODE="$node"
  start_probe_window "$disruption_probe_window_seconds" "$failures_file"
  probe_pid="$PROBE_PID"
  if ! "${kube[@]}" -n "$namespace" delete pod "$pod" --wait=false >/dev/null; then
    stop_probe_window
    if "${kube[@]}" uncordon "$node" >/dev/null 2>&1; then
      CORDONED_NODE=""
    fi
    CASE_DETAILS_JSON="$(jq -nc --arg node "$node" --arg pod "$pod" --argjson failures "$(read_probe_failures "$failures_file")" \
      '{error: "control plane Pod deletion failed during drain simulation", node: $node, deletedPod: $pod, readyProbeFailures: $failures}')"
    return 1
  fi
  if ! "${kube[@]}" -n "$namespace" wait --for=delete "pod/$pod" --timeout="${case_timeout_seconds}s" >/dev/null; then
    stop_probe_window
    if "${kube[@]}" uncordon "$node" >/dev/null 2>&1; then
      CORDONED_NODE=""
    fi
    CASE_DETAILS_JSON="$(jq -nc --arg node "$node" --arg pod "$pod" --argjson failures "$(read_probe_failures "$failures_file")" \
      '{error: "control plane Pod did not terminate during drain simulation", node: $node, deletedPod: $pod, readyProbeFailures: $failures}')"
    return 1
  fi
  if ! wait "$probe_pid"; then
    PROBE_PID=""
    if "${kube[@]}" uncordon "$node" >/dev/null 2>&1; then
      CORDONED_NODE=""
    fi
    CASE_DETAILS_JSON="$(jq -nc --arg node "$node" --arg pod "$pod" \
      '{error: "readiness probe process failed during drain simulation", node: $node, deletedPod: $pod}')"
    return 1
  fi
  PROBE_PID=""
  if ! wait_for_control_plane_ready; then
    if [[ "$cordoned" == "1" ]]; then
      if "${kube[@]}" uncordon "$node" >/dev/null 2>&1; then
        CORDONED_NODE=""
      fi
    fi
    CASE_DETAILS_JSON="$(jq -nc --arg node "$node" --arg pod "$pod" --argjson failures "$(read_probe_failures "$failures_file")" '
      {
        error: "control plane did not become ready after drain simulation",
        node: $node,
        deletedPod: $pod,
        readyProbeFailures: $failures
      }')"
    return 1
  fi
  if ! after_json="$(get_control_plane_pods_json)"; then
    if [[ "$cordoned" == "1" ]]; then
      if "${kube[@]}" uncordon "$node" >/dev/null 2>&1; then
        CORDONED_NODE=""
      fi
    fi
    CASE_DETAILS_JSON="$(jq -nc --arg node "$node" --arg pod "$pod" --argjson failures "$(read_probe_failures "$failures_file")" '
      {
        error: "failed to list control plane Pods after drain simulation",
        node: $node,
        deletedPod: $pod,
        readyProbeFailures: $failures
      }')"
    return 1
  fi
  if [[ "$cordoned" == "1" ]]; then
    if ! "${kube[@]}" uncordon "$node" >/dev/null; then
      CASE_DETAILS_JSON="$(jq -nc --arg node "$node" '{error: "failed to uncordon node after drain simulation", node: $node}')"
      return 1
    fi
    CORDONED_NODE=""
  fi
  drain_failures="$(read_probe_failures "$failures_file")"
  if (( drain_failures > max_drain_ready_failures )); then
    CASE_DETAILS_JSON="$(jq -nc --arg node "$node" --arg pod "$pod" --argjson failures "$drain_failures" \
      --argjson before "$before_json" --argjson after "$after_json" '
      {
        error: "readiness probe failures exceeded drain threshold",
        node: $node,
        deletedPod: $pod,
        readyProbeFailures: $failures,
        beforePods: [($before.items // [])[] | {name: .metadata.name, node: .spec.nodeName}],
        afterPods: [($after.items // [])[] | {name: .metadata.name, node: .spec.nodeName}]
      }')"
    return 1
  fi
  CASE_DETAILS_JSON="$(jq -nc --arg node "$node" --arg pod "$pod" --argjson failures "$drain_failures" \
    --argjson before "$before_json" --argjson after "$after_json" '
    {
      node: $node,
      deletedPod: $pod,
      readyProbeFailures: $failures,
      beforePods: [($before.items // [])[] | {name: .metadata.name, node: .spec.nodeName}],
      afterPods: [($after.items // [])[] | {name: .metadata.name, node: .spec.nodeName}],
      podsOnCordonedNodeAfter: [
        ($after.items // [])[]
        | select(.spec.nodeName == $node)
        | .metadata.name
      ]
    }')"
}

case_node_partition() {
  local target_json node node_uid pod_uid network before_ready_json after_ready_json
  local failures_file="$work_dir/partition-probe-failures"
  local probe_pid partition_failures managed_probe_window_seconds start_hook_json verify_hook_json stop_hook_json
  local start_hook_rc verify_hook_rc stop_hook_rc
  if ! target_json="$(select_safe_control_plane_target "${SYNARA_K8S_RESILIENCE_PARTITION_NODE:-}")"; then
    CASE_DETAILS_JSON='{"reason":"no safe single-control-plane node without stage2 dependencies was available for node partition simulation"}'
    return 2
  fi
  if ! node="$(jq -er '.node' <<<"$target_json")"; then
    CASE_DETAILS_JSON="$(jq -nc --arg rawTarget "$target_json" \
      '{error: "failed to read partition target node", rawTarget: $rawTarget}')"
    return 1
  fi
  if ! before_ready_json="$("${kube[@]}" get "node/$node" -o json)"; then
    CASE_DETAILS_JSON="$(jq -nc --arg node "$node" '{error: "failed to read node state before partition", node: $node}')"
    return 1
  fi
  if ! node_uid="$(jq -er '.metadata.uid | select(type == "string" and length > 0 and length <= 128)' <<<"$before_ready_json")" ||
    ! pod_uid="$(jq -er '.podUid | select(type == "string" and length > 0 and length <= 128)' <<<"$target_json")"; then
    CASE_DETAILS_JSON="$(jq -nc --arg node "$node" '{error: "partition target omitted immutable Node or Pod UID", node: $node}')"
    return 1
  fi
  target_json="$(jq -nc --argjson target "$target_json" --arg nodeUid "$node_uid" '$target + {nodeUid: $nodeUid}')"
  if [[ "$context" != kind-* ]]; then
    if [[ -z "$node_partition_start_hook" && -z "$node_partition_verify_hook" && -z "$node_partition_stop_hook" ]]; then
      CASE_DETAILS_JSON='{"reason":"node partition for non-Kind contexts requires explicit start, verify, and stop hooks"}'
      return 2
    fi
    if [[ -z "$node_partition_start_hook" || -z "$node_partition_verify_hook" || -z "$node_partition_stop_hook" ]]; then
      CASE_DETAILS_JSON="$(jq -nc \
        --argjson startConfigured "$([[ -n "$node_partition_start_hook" ]] && printf true || printf false)" \
        --argjson verifyConfigured "$([[ -n "$node_partition_verify_hook" ]] && printf true || printf false)" \
        --argjson stopConfigured "$([[ -n "$node_partition_stop_hook" ]] && printf true || printf false)" '
        {
          error: "managed node partition hooks must configure start, verify, and stop commands",
          backend: "managed-hook",
          startHookConfigured: $startConfigured,
          verifyHookConfigured: $verifyConfigured,
          stopHookConfigured: $stopConfigured
        }')"
      return 1
    fi
    managed_probe_window_seconds=$((partition_seconds + disruption_probe_window_seconds + node_partition_start_hook_timeout_seconds + node_partition_verify_hook_timeout_seconds + node_partition_stop_hook_timeout_seconds))
    start_probe_window "$managed_probe_window_seconds" "$failures_file"
    probe_pid="$PROBE_PID"
    if start_managed_partition_hook "$node" "$target_json"; then
      start_hook_rc=0
    else
      start_hook_rc=$?
    fi
    start_hook_json="$PARTITION_HOOK_RESULT_JSON"
    if (( start_hook_rc != 0 )); then
      if stop_managed_partition_hook "start-failed"; then
        stop_hook_rc=0
      else
        stop_hook_rc=$?
      fi
      stop_hook_json="$PARTITION_HOOK_RESULT_JSON"
      stop_probe_window
      CASE_DETAILS_JSON="$(jq -nc \
        --arg node "$node" \
        --argjson target "$target_json" \
        --argjson startHook "$start_hook_json" \
        --argjson stopHook "$stop_hook_json" '
        {
          error: (
            if ($stopHook.exitCode // 1) == 0 then
              "managed node partition start hook failed after cleanup heal"
            else
              "managed node partition start hook failed and cleanup heal also failed"
            end
          ),
          backend: "managed-hook",
          node: $node,
          target: $target,
          startHook: $startHook,
          stopHook: $stopHook
      }')"
      return 1
    fi
    if run_managed_partition_verification "$node" "$target_json"; then
      verify_hook_rc=0
    else
      verify_hook_rc=$?
    fi
    verify_hook_json="$PARTITION_HOOK_VERIFY_JSON"
    if (( verify_hook_rc != 0 )); then
      if stop_managed_partition_hook "verify-failed"; then
        stop_hook_rc=0
      else
        stop_hook_rc=$?
      fi
      stop_hook_json="$PARTITION_HOOK_RESULT_JSON"
      stop_probe_window
      CASE_DETAILS_JSON="$(jq -nc \
        --arg node "$node" \
        --argjson target "$target_json" \
        --argjson startHook "$start_hook_json" \
        --argjson verifyHook "$verify_hook_json" \
        --argjson stopHook "$stop_hook_json" '
        {
          error: (
            if ($stopHook.exitCode // 1) == 0 then
              "managed node partition verification failed after cleanup heal"
            else
              "managed node partition verification failed and cleanup heal also failed"
            end
          ),
          backend: "managed-hook",
          node: $node,
          target: $target,
          startHook: $startHook,
          verifyHook: $verifyHook,
          stopHook: $stopHook
        }')"
      return 1
    fi
    sleep "$partition_seconds"
    if stop_managed_partition_hook "case-complete"; then
      stop_hook_rc=0
    else
      stop_hook_rc=$?
    fi
    stop_hook_json="$PARTITION_HOOK_RESULT_JSON"
    if (( stop_hook_rc != 0 )); then
      stop_probe_window
      CASE_DETAILS_JSON="$(jq -nc \
        --arg node "$node" \
        --argjson target "$target_json" \
        --argjson startHook "$start_hook_json" \
        --argjson verifyHook "$verify_hook_json" \
        --argjson stopHook "$stop_hook_json" '
        {
          error: "managed node partition stop hook failed",
          backend: "managed-hook",
          node: $node,
          target: $target,
          partitionSeconds: '"$partition_seconds"',
          startHook: $startHook,
          verifyHook: $verifyHook,
          stopHook: $stopHook
        }')"
      return 1
    fi
    if ! "${kube[@]}" wait --for=condition=Ready "node/$node" --timeout="${case_timeout_seconds}s" >/dev/null; then
      stop_probe_window
      CASE_DETAILS_JSON="$(jq -nc \
        --arg node "$node" \
        --argjson target "$target_json" \
        --argjson startHook "$start_hook_json" \
        --argjson verifyHook "$verify_hook_json" \
        --argjson stopHook "$stop_hook_json" \
        --argjson failures "$(read_probe_failures "$failures_file")" '
        {
          error: "node did not become Ready again after managed node partition",
          backend: "managed-hook",
          node: $node,
          target: $target,
          partitionSeconds: '"$partition_seconds"',
          readyProbeFailures: $failures,
          startHook: $startHook,
          verifyHook: $verifyHook,
          stopHook: $stopHook
        }')"
      return 1
    fi
    if ! wait "$probe_pid"; then
      PROBE_PID=""
      CASE_DETAILS_JSON="$(jq -nc \
        --arg node "$node" \
        --argjson target "$target_json" \
        --argjson startHook "$start_hook_json" \
        --argjson verifyHook "$verify_hook_json" \
        --argjson stopHook "$stop_hook_json" '
        {
          error: "readiness probe process failed during managed node partition",
          backend: "managed-hook",
          node: $node,
          target: $target,
          startHook: $startHook,
          verifyHook: $verifyHook,
          stopHook: $stopHook
        }')"
      return 1
    fi
    PROBE_PID=""
    if ! after_ready_json="$("${kube[@]}" get "node/$node" -o json)"; then
      CASE_DETAILS_JSON="$(jq -nc \
        --arg node "$node" \
        --argjson target "$target_json" \
        --argjson startHook "$start_hook_json" \
        --argjson verifyHook "$verify_hook_json" \
        --argjson stopHook "$stop_hook_json" \
        --argjson failures "$(read_probe_failures "$failures_file")" '
        {
          error: "failed to read node state after managed node partition",
          backend: "managed-hook",
          node: $node,
          target: $target,
          readyProbeFailures: $failures,
          startHook: $startHook,
          verifyHook: $verifyHook,
          stopHook: $stopHook
        }')"
      return 1
    fi
    if ! wait_for_control_plane_ready; then
      CASE_DETAILS_JSON="$(jq -nc \
        --arg node "$node" \
        --argjson target "$target_json" \
        --argjson startHook "$start_hook_json" \
        --argjson verifyHook "$verify_hook_json" \
        --argjson stopHook "$stop_hook_json" \
        --argjson failures "$(read_probe_failures "$failures_file")" '
        {
          error: "control plane did not become ready after managed node partition",
          backend: "managed-hook",
          node: $node,
          target: $target,
          readyProbeFailures: $failures,
          startHook: $startHook,
          verifyHook: $verifyHook,
          stopHook: $stopHook
        }')"
      return 1
    fi
    partition_failures="$(read_probe_failures "$failures_file")"
    if (( partition_failures > max_partition_ready_failures )); then
      CASE_DETAILS_JSON="$(jq -nc \
        --arg node "$node" \
        --argjson target "$target_json" \
        --argjson failures "$partition_failures" \
        --argjson beforeNode "$before_ready_json" \
        --argjson afterNode "$after_ready_json" \
        --argjson startHook "$start_hook_json" \
        --argjson verifyHook "$verify_hook_json" \
        --argjson stopHook "$stop_hook_json" '
        {
          error: "readiness probe failures exceeded partition threshold",
          backend: "managed-hook",
          node: $node,
          target: $target,
          partitionSeconds: '"$partition_seconds"',
          readyProbeFailures: $failures,
          nodeReadyBefore: [($beforeNode.status.conditions // [])[] | select(.type == "Ready")][0],
          nodeReadyAfter: [($afterNode.status.conditions // [])[] | select(.type == "Ready")][0],
          startHook: $startHook,
          verifyHook: $verifyHook,
          stopHook: $stopHook
        }')"
      return 1
    fi
    CASE_DETAILS_JSON="$(jq -nc \
      --arg node "$node" \
      --argjson target "$target_json" \
      --argjson failures "$partition_failures" \
      --argjson beforeNode "$before_ready_json" \
      --argjson afterNode "$after_ready_json" \
      --argjson startHook "$start_hook_json" \
      --argjson verifyHook "$verify_hook_json" \
      --argjson stopHook "$stop_hook_json" '
      {
        backend: "managed-hook",
        node: $node,
        target: $target,
        partitionSeconds: '"$partition_seconds"',
        readyProbeFailures: $failures,
        nodeReadyBefore: [($beforeNode.status.conditions // [])[] | select(.type == "Ready")][0],
        nodeReadyAfter: [($afterNode.status.conditions // [])[] | select(.type == "Ready")][0],
        startHook: $startHook,
        verifyHook: $verifyHook,
        stopHook: $stopHook
      }')"
    return 0
  fi
  if ! network="$(docker inspect --format '{{range $name, $_ := .NetworkSettings.Networks}}{{printf "%s\n" $name}}{{end}}' "$node" | head -n1)"; then
    CASE_DETAILS_JSON="$(jq -nc --arg node "$node" '{error: "failed to inspect Docker network for Kind node", node: $node}')"
    return 1
  fi
  if [[ -z "$network" ]]; then
    CASE_DETAILS_JSON="$(jq -nc --arg node "$node" '{error: "could not determine Docker network for Kind node", node: $node}')"
    return 1
  fi
  start_probe_window "$((partition_seconds + disruption_probe_window_seconds))" "$failures_file"
  probe_pid="$PROBE_PID"
  if ! docker network disconnect "$network" "$node" >/dev/null; then
    stop_probe_window
    CASE_DETAILS_JSON="$(jq -nc --arg node "$node" --arg network "$network" '{error: "failed to disconnect Kind node for partition", node: $node, network: $network}')"
    return 1
  fi
  PARTITION_NETWORK="$network"
  PARTITION_NODE="$node"
  sleep "$partition_seconds"
  if ! docker network connect "$network" "$node" >/dev/null; then
    stop_probe_window
    CASE_DETAILS_JSON="$(jq -nc --arg node "$node" --arg network "$network" '{error: "failed to reconnect Kind node after partition", node: $node, network: $network}')"
    return 1
  fi
  PARTITION_NETWORK=""
  PARTITION_NODE=""
  if ! "${kube[@]}" wait --for=condition=Ready "node/$node" --timeout="${case_timeout_seconds}s" >/dev/null; then
    stop_probe_window
    CASE_DETAILS_JSON="$(jq -nc --arg node "$node" --arg network "$network" --argjson failures "$(read_probe_failures "$failures_file")" '
      {
        error: "node did not become Ready again after partition",
        node: $node,
        network: $network,
        readyProbeFailures: $failures
      }')"
    return 1
  fi
  if ! wait "$probe_pid"; then
    PROBE_PID=""
    CASE_DETAILS_JSON="$(jq -nc --arg node "$node" --arg network "$network" \
      '{error: "readiness probe process failed during node partition", node: $node, network: $network}')"
    return 1
  fi
  PROBE_PID=""
  if ! after_ready_json="$("${kube[@]}" get "node/$node" -o json)"; then
    CASE_DETAILS_JSON="$(jq -nc --arg node "$node" --arg network "$network" --argjson failures "$(read_probe_failures "$failures_file")" '
      {
        error: "failed to read node state after partition",
        node: $node,
        network: $network,
        readyProbeFailures: $failures
      }')"
    return 1
  fi
  if ! wait_for_control_plane_ready; then
    CASE_DETAILS_JSON="$(jq -nc --arg node "$node" --arg network "$network" --argjson failures "$(read_probe_failures "$failures_file")" '
      {
        error: "control plane did not become ready after node partition",
        node: $node,
        network: $network,
        readyProbeFailures: $failures
      }')"
    return 1
  fi
  partition_failures="$(read_probe_failures "$failures_file")"
  if (( partition_failures > max_partition_ready_failures )); then
    CASE_DETAILS_JSON="$(jq -nc --arg node "$node" --arg network "$network" --argjson failures "$partition_failures" \
      --argjson beforeNode "$before_ready_json" --argjson afterNode "$after_ready_json" '
      {
        error: "readiness probe failures exceeded partition threshold",
        node: $node,
        network: $network,
        partitionSeconds: '"$partition_seconds"',
        readyProbeFailures: $failures,
        nodeReadyBefore: [($beforeNode.status.conditions // [])[] | select(.type == "Ready")][0],
        nodeReadyAfter: [($afterNode.status.conditions // [])[] | select(.type == "Ready")][0]
      }')"
    return 1
  fi
  CASE_DETAILS_JSON="$(jq -nc --arg node "$node" --arg network "$network" --argjson failures "$partition_failures" \
    --argjson beforeNode "$before_ready_json" --argjson afterNode "$after_ready_json" '
    {
      node: $node,
      network: $network,
      partitionSeconds: '"$partition_seconds"',
      readyProbeFailures: $failures,
      nodeReadyBefore: [($beforeNode.status.conditions // [])[] | select(.type == "Ready")][0],
      nodeReadyAfter: [($afterNode.status.conditions // [])[] | select(.type == "Ready")][0]
    }')"
}

case_resource_lifecycle() {
  local result_file="$work_dir/resource-lifecycle-result.json"
  local rc=0 payload
  if [[ "$bootstrap_baseline" != "1" ]]; then
    CASE_DETAILS_JSON='{"error":"resource lifecycle acceptance requires the owned Stage 2 baseline"}'
    return 1
  fi
  if [[ -z "$resource_lifecycle_worker_image" ]]; then
    CASE_DETAILS_JSON='{"error":"SYNARA_K8S_RESILIENCE_WORKER_IMAGE is required for resource-lifecycle"}'
    return 1
  fi
  if [[ ! -f "$resource_lifecycle_runner" ]]; then
    CASE_DETAILS_JSON='{"error":"resource lifecycle acceptance runner is missing"}'
    return 1
  fi
  if python3 "$resource_lifecycle_runner" \
    --context "$context" \
    --control-plane-namespace "$namespace" \
    --worker-namespace "$resource_lifecycle_worker_namespace" \
    --worker-image "$resource_lifecycle_worker_image" \
    --result-file "$result_file" \
    --phase-timeout "$case_timeout_seconds" \
    --command-timeout 60 \
    --cleanup-timeout 180; then
    rc=0
  else
    rc=$?
  fi
  if [[ ! -s "$result_file" ]] || ! payload="$(jq -c '
    select(.schemaVersion == "synara.kubernetes.resource-lifecycle.acceptance.v1")
    | {
        status,evidenceLevel,durationSeconds,identities,lifecycle,pendingInteraction,
        workspace,recoveryBundle,podLifecycle,cleanup,
        reasonCode:(.reasonCode // null),message:(.message // null),details:(.details // null)
      }
  ' "$result_file")" || [[ -z "$payload" ]]; then
    CASE_DETAILS_JSON='{"error":"resource lifecycle acceptance emitted invalid evidence"}'
    return 1
  fi
  CASE_DETAILS_JSON="$payload"
  if (( rc != 0 )) || [[ "$(jq -r '.status' <<<"$payload")" != "passed" ]] || \
    [[ "$(jq -r '.cleanup.deleted // false' <<<"$payload")" != "true" ]]; then
    return 1
  fi
  return 0
}

execute_case() {
  local case_name="$1"
  case "$case_name" in
    rbac) case_rbac ;;
    topology) case_topology ;;
    leader-takeover) case_leader_takeover ;;
    control-plane-failover) case_control_plane_failover ;;
    node-drain) case_node_drain ;;
    node-partition) case_node_partition ;;
    resource-lifecycle) case_resource_lifecycle ;;
    *)
      CASE_DETAILS_JSON="$(jq -nc --arg caseName "$case_name" '{error: "unknown resilience case", case: $caseName}')"
      return 1
      ;;
  esac
}

case_may_skip() {
  local candidate="$1"
  local -a allowed_cases=()
  local raw_case case_name
  [[ -n "$allow_skipped_cases_csv" ]] || return 1
  IFS=',' read -r -a allowed_cases <<<"$allow_skipped_cases_csv"
  for raw_case in "${allowed_cases[@]}"; do
    case_name="${raw_case//[[:space:]]/}"
    if [[ -n "$case_name" && "$case_name" == "$candidate" ]]; then
      return 0
    fi
  done
  return 1
}

record_case() {
  local case_name="$1"
  local started_case_at finished_case_at rc status
  started_case_at="$(iso_now)"
  CASE_DETAILS_JSON='{}'
  ACTIVE_CASE_NAME="$case_name"
  ACTIVE_CASE_STARTED_AT="$started_case_at"
  ACTIVE_CASE_RECORDED=0
  if execute_case "$case_name"; then
    rc=0
  else
    rc=$?
  fi
  status="passed"
  if (( rc == 2 )); then
    status="skipped"
    if ! case_may_skip "$case_name"; then
      overall_failed=1
    fi
  elif (( rc != 0 )); then
    status="failed"
    overall_failed=1
  fi
  finished_case_at="$(iso_now)"
  append_case_result "$case_name" "$status" "$started_case_at" "$finished_case_at" "$CASE_DETAILS_JSON"
  ACTIVE_CASE_NAME=""
  ACTIVE_CASE_STARTED_AT=""
  ACTIVE_CASE_RECORDED=1
  return 0
}

run_soak() {
  local -a soak_cases=()
  local cycle=0
  local deadline idle_probe_failures_total=0 required_skipped_cycles=0 cycle_rc status cycle_started_at cycle_finished_at
  local cycle_probe_failures soak_started_at soak_finished_at soak_started_epoch soak_finished_epoch actual_duration_seconds
  local remaining_seconds probe_duration_seconds progress_json
  IFS=',' read -r -a soak_cases <<<"$soak_cases_csv"
  soak_started_at="$(iso_now)"
  soak_started_epoch="$(date +%s)"
  deadline=$(( soak_started_epoch + soak_seconds ))
  if (( ${#soak_cases[@]} == 0 )); then
    soak_json='{"executed":false,"status":"skipped","reason":"no soak cases configured"}'
    return 0
  fi
  soak_json="$(jq -nc \
    --arg cases "$soak_cases_csv" \
    --arg startedAt "$soak_started_at" \
    --argjson configuredDurationSeconds "$soak_seconds" '
    {
      executed: true,
      status: "running",
      startedAt: $startedAt,
      configuredDurationSeconds: $configuredDurationSeconds,
      plannedCases: ($cases | split(",") | map(gsub("[[:space:]]"; "") | select(length > 0)))
    }')"
  update_running_snapshot
  while (( $(date +%s) < deadline )); do
    local raw_case="${soak_cases[$((cycle % ${#soak_cases[@]}))]}"
    local case_name="${raw_case//[[:space:]]/}"
    current_phase_json="$(jq -nc \
      --arg type "soak-cycle" \
      --arg caseName "$case_name" \
      --argjson cycle "$((cycle + 1))" \
      '{type: $type, case: $caseName, cycle: $cycle}')"
    update_running_snapshot
    cycle_started_at="$(iso_now)"
    CASE_DETAILS_JSON='{}'
    if execute_case "$case_name"; then
      cycle_rc=0
    else
      cycle_rc=$?
    fi
    CASE_DETAILS_JSON="$(sanitize_managed_details "$case_name" "$CASE_DETAILS_JSON")"
    status="passed"
    if (( cycle_rc == 2 )); then
      status="skipped"
      if ! case_may_skip "$case_name"; then
        overall_failed=1
        required_skipped_cycles=$((required_skipped_cycles + 1))
      fi
    elif (( cycle_rc != 0 )); then
      status="failed"
      overall_failed=1
    fi
    cycle_finished_at="$(iso_now)"
    jq -nc \
      --arg case "$case_name" \
      --arg status "$status" \
      --arg startedAt "$cycle_started_at" \
      --arg finishedAt "$cycle_finished_at" \
      --argjson details "$CASE_DETAILS_JSON" \
      '{
        case: $case,
        status: $status,
        startedAt: $startedAt,
        finishedAt: $finishedAt,
        details: $details
      }' >>"$soak_cycles_file"
    progress_json="$(jq -nc \
      --arg caseName "$case_name" \
      --arg status "$status" \
      --arg startedAt "$cycle_started_at" \
      --arg finishedAt "$cycle_finished_at" \
      --argjson cycle "$((cycle + 1))" \
      --argjson details "$(sanitize_progress_details "$CASE_DETAILS_JSON")" '
      {
        cycle: $cycle,
        case: $caseName,
        status: $status,
        startedAt: $startedAt,
        finishedAt: $finishedAt,
        details: $details
      }')"
    record_progress_update "soak-cycle-completed" "$progress_json"
    if [[ "$status" == "failed" ]] || (( required_skipped_cycles > 0 )); then
      break
    fi
    cycle=$((cycle + 1))
    if (( $(date +%s) >= deadline )); then
      break
    fi
    remaining_seconds=$(( deadline - $(date +%s) ))
    if (( remaining_seconds <= 0 )); then
      break
    fi
    probe_duration_seconds="$soak_interval_seconds"
    if (( probe_duration_seconds > remaining_seconds )); then
      probe_duration_seconds="$remaining_seconds"
    fi
    cycle_probe_failures="$(probe_ready_window_failures "$probe_duration_seconds")"
    idle_probe_failures_total=$((idle_probe_failures_total + cycle_probe_failures))
    if (( cycle_probe_failures > 0 )); then
      overall_failed=1
      break
    fi
  done
  soak_finished_at="$(iso_now)"
  soak_finished_epoch="$(date +%s)"
  actual_duration_seconds=$(( soak_finished_epoch - soak_started_epoch ))
  soak_json="$(jq -nc \
    --arg cases "$soak_cases_csv" \
    --arg startedAt "$soak_started_at" \
    --arg finishedAt "$soak_finished_at" \
    --argjson configuredDurationSeconds "$soak_seconds" \
    --argjson durationSeconds "$actual_duration_seconds" \
    --argjson idleReadyProbeFailures "$idle_probe_failures_total" \
    --argjson requiredSkippedCycles "$required_skipped_cycles" \
    --slurpfile cycles "$soak_cycles_file" '
    {
      executed: true,
      status: (
        if ([($cycles // [])[] | select(.status == "failed")] | length) > 0
          or $idleReadyProbeFailures > 0 or $requiredSkippedCycles > 0 then
          "failed"
        elif ($cycles | length) == 0 then
          "skipped"
        else
          "passed"
        end
      ),
      startedAt: $startedAt,
      finishedAt: $finishedAt,
      configuredDurationSeconds: $configuredDurationSeconds,
      durationSeconds: $durationSeconds,
      plannedCases: ($cases | split(",") | map(gsub("[[:space:]]"; "") | select(length > 0))),
      idleReadyProbeFailures: $idleReadyProbeFailures,
      requiredSkippedCycles: $requiredSkippedCycles,
      cycles: $cycles
    }')"
}

emit_final_report() {
  local finished_at duration_seconds overall_status
  local final_progress_json final_report_tmp
  finished_at="$(iso_now)"
  duration_seconds=$(( $(date +%s) - started_epoch ))
  overall_status="passed"
  if (( overall_failed != 0 )) || [[ "$baseline_status" == "failed" ]]; then
    overall_status="failed"
  fi
  current_phase_json='{"type":"finalizing"}'
  final_report_tmp="$(same_dir_tmp_file "$evidence_file")"
  jq -n \
    --arg schemaVersion "synara.kubernetes.resilience.acceptance.v1" \
    --arg status "$overall_status" \
    --arg startedAt "$started_at" \
    --arg finishedAt "$finished_at" \
    --arg context "$context" \
    --arg namespace "$namespace" \
    --arg rbacName "$rbac_name" \
    --arg sessionAuthorityMode "$session_authority_mode" \
    --arg cases "$cases_csv" \
    --arg allowSkippedCases "$allow_skipped_cases_csv" \
    --arg evidenceFile "$evidence_file" \
    --argjson durationSeconds "$duration_seconds" \
    --argjson bootstrapBaseline "$bootstrap_baseline" \
    --arg baselineStatus "$baseline_status" \
    --argjson baselineDurationSeconds "$baseline_duration_seconds" \
    --argjson unexpectedExitStatus "$unexpected_exit_status" \
    --argjson topology "$topology_json" \
    --argjson soak "$soak_json" \
    --slurpfile permissions "$permissions_file" \
    --slurpfile scenarios "$scenarios_file" '
    {
      schemaVersion: $schemaVersion,
      status: $status,
      startedAt: $startedAt,
      finishedAt: $finishedAt,
      durationSeconds: $durationSeconds,
      context: $context,
      namespace: $namespace,
      rbacName: $rbacName,
      evidenceFile: $evidenceFile,
      safety: {
        kindOnlyByDefault: true,
        allowNonDisposableOverride: "SYNARA_K8S_ACCEPTANCE_ALLOW_NONDISPOSABLE",
        sessionAuthorityMode: $sessionAuthorityMode,
        nodePartitionManagedHookOverride: {
          startHookEnvVar: "SYNARA_K8S_NODE_PARTITION_START_HOOK",
          verifyHookEnvVar: "SYNARA_K8S_NODE_PARTITION_VERIFY_HOOK",
          stopHookEnvVar: "SYNARA_K8S_NODE_PARTITION_STOP_HOOK",
          timeoutEnvVar: "SYNARA_K8S_NODE_PARTITION_HOOK_TIMEOUT_SECONDS",
          startTimeoutEnvVar: "SYNARA_K8S_NODE_PARTITION_START_HOOK_TIMEOUT_SECONDS",
          verifyTimeoutEnvVar: "SYNARA_K8S_NODE_PARTITION_VERIFY_HOOK_TIMEOUT_SECONDS",
          stopTimeoutEnvVar: "SYNARA_K8S_NODE_PARTITION_STOP_HOOK_TIMEOUT_SECONDS"
        }
      },
      baseline: {
        executed: ($bootstrapBaseline == 1),
        status: $baselineStatus,
        durationSeconds: $baselineDurationSeconds
      },
      unexpectedExitStatus: (if $unexpectedExitStatus == 0 then null else $unexpectedExitStatus end),
      plannedCases: ($cases | split(",") | map(gsub("[[:space:]]"; "") | select(length > 0))),
      allowedSkippedCases: ($allowSkippedCases | split(",") | map(gsub("[[:space:]]"; "") | select(length > 0))),
      caseCounts: {
        passed: ([($scenarios // [])[] | select(.status == "passed")] | length),
        failed: ([($scenarios // [])[] | select(.status == "failed")] | length),
        skipped: ([($scenarios // [])[] | select(.status == "skipped")] | length)
      },
      permissions: $permissions,
      topology: $topology,
      scenarios: $scenarios,
      soak: $soak
    }' >"$final_report_tmp"
  if ! publish_create_only_file "$final_report_tmp" "$evidence_file"; then
    rm -f "$final_report_tmp"
    return 1
  fi
  final_progress_json="$(jq -nc \
    --arg status "$overall_status" \
    --arg startedAt "$started_at" \
    --arg finishedAt "$finished_at" \
    --arg evidenceFile "$evidence_file" \
    --argjson durationSeconds "$duration_seconds" \
    --argjson unexpectedExitStatus "$unexpected_exit_status" '
    {
      status: $status,
      startedAt: $startedAt,
      finishedAt: $finishedAt,
      durationSeconds: $durationSeconds,
      evidenceFile: $evidenceFile,
      unexpectedExitStatus: (if $unexpectedExitStatus == 0 then null else $unexpectedExitStatus end)
    }')"
  last_progress_json="$final_progress_json"
  append_journal_entry "report-completed" "$final_progress_json"
  write_partial_snapshot "completed" "$overall_status" "$finished_at"
  final_report_emitted=1
}

for arg in "$@"; do
  case "$arg" in
    --dry-run)
      dry_run=1
      ;;
    *)
      printf 'Unknown argument: %s\n' "$arg" >&2
      exit 1
      ;;
  esac
done

require_boolean_flag "$bootstrap_baseline" "SYNARA_K8S_RESILIENCE_BOOTSTRAP_BASELINE"
require_boolean_flag "$dry_run" "SYNARA_K8S_RESILIENCE_DRY_RUN"
require_boolean_flag "$keep_resources" "SYNARA_K8S_KEEP_RESOURCES"
require_boolean_flag "$allow_non_disposable" "SYNARA_K8S_ACCEPTANCE_ALLOW_NONDISPOSABLE"
case "$session_authority_mode" in
  stage2-postgres | disabled) ;;
  *)
    printf 'SYNARA_K8S_RESILIENCE_SESSION_AUTHORITY_MODE must be stage2-postgres or disabled\n' >&2
    exit 1
    ;;
esac
require_non_negative_int "$soak_seconds" "SYNARA_K8S_RESILIENCE_SOAK_SECONDS"
require_positive_int "$soak_interval_seconds" "SYNARA_K8S_RESILIENCE_SOAK_INTERVAL_SECONDS"
require_positive_int "$probe_interval_seconds" "SYNARA_K8S_RESILIENCE_PROBE_INTERVAL_SECONDS"
require_positive_int "$disruption_probe_window_seconds" "SYNARA_K8S_RESILIENCE_DISRUPTION_WINDOW_SECONDS"
require_positive_int "$partition_seconds" "SYNARA_K8S_RESILIENCE_PARTITION_SECONDS"
require_positive_int "$node_partition_hook_timeout_seconds" "SYNARA_K8S_NODE_PARTITION_HOOK_TIMEOUT_SECONDS"
require_positive_int "$node_partition_start_hook_timeout_seconds" "SYNARA_K8S_NODE_PARTITION_START_HOOK_TIMEOUT_SECONDS"
require_positive_int "$node_partition_verify_hook_timeout_seconds" "SYNARA_K8S_NODE_PARTITION_VERIFY_HOOK_TIMEOUT_SECONDS"
require_positive_int "$node_partition_stop_hook_timeout_seconds" "SYNARA_K8S_NODE_PARTITION_STOP_HOOK_TIMEOUT_SECONDS"
require_positive_int "$case_timeout_seconds" "SYNARA_K8S_RESILIENCE_CASE_TIMEOUT_SECONDS"
require_positive_int "$min_worker_nodes" "SYNARA_K8S_RESILIENCE_MIN_WORKER_NODES"
require_non_negative_int "$max_failover_ready_failures" "SYNARA_K8S_RESILIENCE_MAX_FAILOVER_READY_FAILURES"
require_non_negative_int "$max_drain_ready_failures" "SYNARA_K8S_RESILIENCE_MAX_DRAIN_READY_FAILURES"
require_non_negative_int "$max_partition_ready_failures" "SYNARA_K8S_RESILIENCE_MAX_PARTITION_READY_FAILURES"
if [[ ! "$namespace" =~ ^synara-[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || (( ${#namespace} > 63 )); then
  printf 'SYNARA_K8S_NAMESPACE must be a synara-* DNS label no longer than 63 characters\n' >&2
  exit 1
fi
if [[ ! "$resource_lifecycle_worker_namespace" =~ ^synara-[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || \
  (( ${#resource_lifecycle_worker_namespace} > 63 )) || \
  [[ "$resource_lifecycle_worker_namespace" == "$namespace" ]]; then
  printf 'SYNARA_K8S_RESILIENCE_WORKER_NAMESPACE must be a distinct synara-* DNS label no longer than 63 characters\n' >&2
  exit 1
fi
if [[ ! "$rbac_name" =~ ^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$ ]] || (( ${#rbac_name} > 253 )); then
  printf 'SYNARA_K8S_ACCEPTANCE_RBAC_NAME must be a DNS subdomain no longer than 253 characters\n' >&2
  exit 1
fi
if [[ ! "$acceptance_owner" =~ ^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$ ]] || (( ${#acceptance_owner} > 63 )); then
  printf 'SYNARA_K8S_ACCEPTANCE_OWNER must be a Kubernetes label value no longer than 63 characters\n' >&2
  exit 1
fi
if (( soak_seconds > 0 )) && [[ "$evidence_file_is_explicit" != "1" ]]; then
  printf 'SYNARA_K8S_RESILIENCE_EVIDENCE_FILE must be set explicitly when soak is enabled\n' >&2
  exit 1
fi
normalized_cases=",${cases_csv//[[:space:]]/},"
resource_lifecycle_worker_image_digest=""
if [[ -n "$resource_lifecycle_worker_image" ]]; then
  resource_lifecycle_worker_image_digest="$(sha256_text "$resource_lifecycle_worker_image")"
fi
if [[ "$normalized_cases" == *,resource-lifecycle,* && -z "$resource_lifecycle_worker_image" ]]; then
  printf 'SYNARA_K8S_RESILIENCE_WORKER_IMAGE is required when resource-lifecycle is selected\n' >&2
  exit 1
fi

if [[ "$dry_run" == "1" ]]; then
  dry_run_report_tmp="$(same_dir_tmp_file "$evidence_file")"
  jq -n \
    --arg schemaVersion "synara.kubernetes.resilience.acceptance.v1" \
    --arg baselineScript "$baseline_script" \
    --arg context "$context" \
    --arg namespace "$namespace" \
    --arg rbacName "$rbac_name" \
    --arg sessionAuthorityMode "$session_authority_mode" \
    --arg cases "$cases_csv" \
    --arg allowSkippedCases "$allow_skipped_cases_csv" \
    --arg soakCases "$soak_cases_csv" \
    --arg evidenceFile "$evidence_file" \
    --arg resourceLifecycleWorkerNamespace "$resource_lifecycle_worker_namespace" \
    --arg resourceLifecycleWorkerImageDigest "$resource_lifecycle_worker_image_digest" \
    --argjson bootstrapBaseline "$bootstrap_baseline" \
    --argjson soakSeconds "$soak_seconds" \
    --argjson soakIntervalSeconds "$soak_interval_seconds" \
    --argjson probeIntervalSeconds "$probe_interval_seconds" \
    --argjson disruptionProbeWindowSeconds "$disruption_probe_window_seconds" \
    --argjson partitionSeconds "$partition_seconds" \
    --argjson caseTimeoutSeconds "$case_timeout_seconds" '
    {
      schemaVersion: $schemaVersion,
      status: "dry-run",
      context: $context,
      namespace: $namespace,
      rbacName: $rbacName,
      baselineScript: $baselineScript,
      bootstrapBaseline: ($bootstrapBaseline == 1),
      plannedCases: ($cases | split(",") | map(gsub("[[:space:]]"; "") | select(length > 0))),
      allowedSkippedCases: ($allowSkippedCases | split(",") | map(gsub("[[:space:]]"; "") | select(length > 0))),
      soak: {
        enabled: ($soakSeconds > 0),
        durationSeconds: $soakSeconds,
        intervalSeconds: $soakIntervalSeconds,
        plannedCases: ($soakCases | split(",") | map(gsub("[[:space:]]"; "") | select(length > 0)))
      },
      disruptionProbeWindowSeconds: $disruptionProbeWindowSeconds,
      partitionSeconds: $partitionSeconds,
      caseTimeoutSeconds: $caseTimeoutSeconds,
      probeIntervalSeconds: $probeIntervalSeconds,
      resourceLifecycle: {
        workerNamespace: $resourceLifecycleWorkerNamespace,
        workerImageConfigured: ($resourceLifecycleWorkerImageDigest != ""),
        workerImageDigest: $resourceLifecycleWorkerImageDigest
      },
      safety: {
        kindOnlyByDefault: true,
        allowNonDisposableOverride: "SYNARA_K8S_ACCEPTANCE_ALLOW_NONDISPOSABLE",
        sessionAuthorityMode: $sessionAuthorityMode,
        nodePartitionManagedHookOverride: {
          startHookEnvVar: "SYNARA_K8S_NODE_PARTITION_START_HOOK",
          verifyHookEnvVar: "SYNARA_K8S_NODE_PARTITION_VERIFY_HOOK",
          stopHookEnvVar: "SYNARA_K8S_NODE_PARTITION_STOP_HOOK",
          timeoutEnvVar: "SYNARA_K8S_NODE_PARTITION_HOOK_TIMEOUT_SECONDS",
          startTimeoutEnvVar: "SYNARA_K8S_NODE_PARTITION_START_HOOK_TIMEOUT_SECONDS",
          verifyTimeoutEnvVar: "SYNARA_K8S_NODE_PARTITION_VERIFY_HOOK_TIMEOUT_SECONDS",
          stopTimeoutEnvVar: "SYNARA_K8S_NODE_PARTITION_STOP_HOOK_TIMEOUT_SECONDS"
        }
      },
      evidenceFile: $evidenceFile
    }' >"$dry_run_report_tmp"
  if ! publish_create_only_file "$dry_run_report_tmp" "$evidence_file"; then
    rm -f "$dry_run_report_tmp"
    exit 1
  fi
  printf 'Kubernetes resilience acceptance dry-run wrote %s\n' "$evidence_file"
  exit 0
fi

require_command jq
require_command kubectl
require_command python3
reporting_ready=1
if [[ "$context" == kind-* ]]; then
  require_command docker
fi
if [[ ! -f "$baseline_script" ]]; then
  printf 'Baseline script %s does not exist\n' "$baseline_script" >&2
  exit 1
fi
if [[ -z "$context" ]]; then
  printf 'A Kubernetes context is required through SYNARA_K8S_CONTEXT or current-context\n' >&2
  exit 1
fi
if [[ "$context" != kind-* && "$allow_non_disposable" != "1" ]]; then
  printf 'Refusing to run resilience acceptance against non-Kind context %s\n' "$context" >&2
  printf 'Set SYNARA_K8S_ACCEPTANCE_ALLOW_NONDISPOSABLE=1 only for an explicitly disposable cluster\n' >&2
  exit 1
fi

kube=(kubectl --context "$context")
"${kube[@]}" cluster-info >/dev/null
proxy_path="/api/v1/namespaces/$namespace/services/http:synara-control-plane:3780/proxy"
current_phase_json='{"type":"starting"}'
update_running_snapshot

if [[ "$bootstrap_baseline" == "1" ]]; then
  local_baseline_started_at="$(iso_now)"
  local_baseline_started_epoch="$(date +%s)"
  created_baseline=1
  baseline_status="running"
  current_phase_json="$(jq -nc --arg type "baseline" --arg script "$baseline_script" \
    '{type: $type, script: $script}')"
  update_running_snapshot
  if SYNARA_K8S_KEEP_RESOURCES=1 \
    SYNARA_K8S_CONTEXT="$context" \
    SYNARA_K8S_NAMESPACE="$namespace" \
    SYNARA_K8S_ACCEPTANCE_RBAC_NAME="$rbac_name" \
    SYNARA_K8S_ACCEPTANCE_OWNER="$acceptance_owner" \
    SYNARA_K8S_ACCEPTANCE_ALLOW_NONDISPOSABLE="$allow_non_disposable" \
      bash "$baseline_script"; then
    baseline_rc=0
  else
    baseline_rc=$?
  fi
  baseline_duration_seconds=$(( $(date +%s) - local_baseline_started_epoch ))
  local_baseline_finished_at="$(iso_now)"
  if (( baseline_rc != 0 )); then
    baseline_status="failed"
    overall_failed=1
    record_progress_update "baseline-completed" "$(jq -nc \
      --arg status "$baseline_status" \
      --arg startedAt "$local_baseline_started_at" \
      --arg finishedAt "$local_baseline_finished_at" \
      --arg script "$baseline_script" \
      --argjson durationSeconds "$baseline_duration_seconds" '
      {
        status: $status,
        startedAt: $startedAt,
        finishedAt: $finishedAt,
        durationSeconds: $durationSeconds,
        script: $script
      }')"
    emit_final_report
    printf 'Kubernetes resilience acceptance failed during baseline bootstrap: context=%s evidence=%s\n' \
      "$context" "$evidence_file" >&2
    exit 1
  fi
  baseline_status="passed"
  record_progress_update "baseline-completed" "$(jq -nc \
    --arg status "$baseline_status" \
    --arg startedAt "$local_baseline_started_at" \
    --arg finishedAt "$local_baseline_finished_at" \
    --arg script "$baseline_script" \
    --argjson durationSeconds "$baseline_duration_seconds" '
    {
      status: $status,
      startedAt: $startedAt,
      finishedAt: $finishedAt,
      durationSeconds: $durationSeconds,
      script: $script
    }')"
fi

IFS=',' read -r -a selected_cases <<<"$cases_csv"
for raw_case in "${selected_cases[@]}"; do
  case_name="${raw_case//[[:space:]]/}"
  [[ -z "$case_name" ]] && continue
  current_phase_json="$(jq -nc --arg type "top-level-case" --arg caseName "$case_name" \
    '{type: $type, case: $caseName}')"
  update_running_snapshot
  record_case "$case_name"
  if (( overall_failed != 0 )); then
    break
  fi
done

if (( soak_seconds > 0 && overall_failed == 0 )); then
  run_soak
fi

emit_final_report

if (( overall_failed != 0 )); then
  printf 'Kubernetes resilience acceptance failed: context=%s evidence=%s\n' "$context" "$evidence_file" >&2
  exit 1
fi

printf 'Kubernetes resilience acceptance passed: context=%s evidence=%s cases=%s\n' \
  "$context" "$evidence_file" "$cases_csv"
