#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
context="${SYNARA_K8S_CONTEXT:-$(kubectl config current-context 2>/dev/null || true)}"
namespace="${SYNARA_K8S_NAMESPACE:-synara-system}"
baseline_script="${SYNARA_K8S_RESILIENCE_BASELINE_SCRIPT:-$script_dir/acceptance.sh}"
bootstrap_baseline="${SYNARA_K8S_RESILIENCE_BOOTSTRAP_BASELINE:-1}"
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
node_partition_stop_hook_timeout_seconds="${SYNARA_K8S_NODE_PARTITION_STOP_HOOK_TIMEOUT_SECONDS:-$node_partition_hook_timeout_seconds}"
case_timeout_seconds="${SYNARA_K8S_RESILIENCE_CASE_TIMEOUT_SECONDS:-240}"
min_worker_nodes="${SYNARA_K8S_RESILIENCE_MIN_WORKER_NODES:-3}"
max_failover_ready_failures="${SYNARA_K8S_RESILIENCE_MAX_FAILOVER_READY_FAILURES:-0}"
max_drain_ready_failures="${SYNARA_K8S_RESILIENCE_MAX_DRAIN_READY_FAILURES:-0}"
max_partition_ready_failures="${SYNARA_K8S_RESILIENCE_MAX_PARTITION_READY_FAILURES:-2}"
node_partition_start_hook="${SYNARA_K8S_NODE_PARTITION_START_HOOK:-}"
node_partition_stop_hook="${SYNARA_K8S_NODE_PARTITION_STOP_HOOK:-}"
dry_run="${SYNARA_K8S_RESILIENCE_DRY_RUN:-0}"
keep_resources="${SYNARA_K8S_KEEP_RESOURCES:-0}"
allow_non_disposable="${SYNARA_K8S_ACCEPTANCE_ALLOW_NONDISPOSABLE:-0}"
if [[ -n "${SYNARA_K8S_RESILIENCE_EVIDENCE_FILE:-}" ]]; then
  evidence_file="$SYNARA_K8S_RESILIENCE_EVIDENCE_FILE"
  evidence_file_is_explicit=1
else
  evidence_file="$(mktemp "${TMPDIR:-/tmp}/synara-k8s-resilience-XXXXXX")"
  evidence_file_is_explicit=0
fi
journal_file="${evidence_file}.journal.jsonl"
partial_file="${evidence_file}.partial.json"
work_dir="$(mktemp -d)"
permissions_file="$work_dir/permissions.jsonl"
scenarios_file="$work_dir/scenarios.jsonl"
soak_cycles_file="$work_dir/soak-cycles.jsonl"
touch "$permissions_file" "$scenarios_file" "$soak_cycles_file"

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
CORDONED_NODE=""
PARTITION_NETWORK=""
PARTITION_NODE=""
PARTITION_HOOK_ACTIVE=0
PARTITION_HOOK_ATTEMPTED=0
PARTITION_HOOK_NODE=""
PARTITION_HOOK_TARGET_JSON='null'
PARTITION_HOOK_START_JSON='null'
PARTITION_HOOK_STOP_JSON='null'
PARTITION_HOOK_RESULT_JSON='null'
kube=()
proxy_path=""

cleanup() {
  local exit_status=$?
  if [[ -n "${PROBE_PID:-}" ]]; then
    kill "$PROBE_PID" >/dev/null 2>&1 || true
    wait "$PROBE_PID" >/dev/null 2>&1 || true
    PROBE_PID=""
  fi
  if [[ -n "${LEASE_GUARD_PID:-}" ]]; then
    kill "$LEASE_GUARD_PID" >/dev/null 2>&1 || true
    wait "$LEASE_GUARD_PID" >/dev/null 2>&1 || true
    LEASE_GUARD_PID=""
    LEASE_GUARD_APP=""
  fi
  if [[ "$PARTITION_HOOK_ACTIVE" == "1" ]]; then
    local cleanup_stop_json cleanup_stop_rc=0 cleanup_details_json cleanup_node cleanup_target_json cleanup_start_json
    cleanup_node="$PARTITION_HOOK_NODE"
    cleanup_target_json="$PARTITION_HOOK_TARGET_JSON"
    cleanup_start_json="$PARTITION_HOOK_START_JSON"
    if stop_managed_partition_hook "cleanup-exit"; then
      cleanup_stop_rc=0
    else
      cleanup_stop_rc=$?
    fi
    cleanup_stop_json="$PARTITION_HOOK_RESULT_JSON"
    overall_failed=1
    if [[ -n "$ACTIVE_CASE_NAME" && "$ACTIVE_CASE_RECORDED" != "1" ]]; then
      cleanup_details_json="$(jq -nc \
        --arg node "$cleanup_node" \
        --argjson target "$cleanup_target_json" \
        --argjson startHook "$cleanup_start_json" \
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
    kubectl --context "$context" delete namespace "$namespace" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    kubectl --context "$context" delete clusterrolebinding synara-control-plane-reconciler --ignore-not-found >/dev/null 2>&1 || true
    kubectl --context "$context" delete clusterrole synara-control-plane-reconciler --ignore-not-found >/dev/null 2>&1 || true
  fi
  rm -rf "$work_dir"
  return "$exit_status"
}
trap cleanup EXIT

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
  local status_json exit_code=1
  if ! status_json="$(python3 - "$command_text" "$stdout_file" "$stderr_file" "$timeout_seconds" <<'PY'
import json
import os
import signal
import subprocess
import sys

command, stdout_path, stderr_path, timeout_text = sys.argv[1:5]
timeout_seconds = int(timeout_text)
timed_out = False
exit_code = 0

with open(stdout_path, "wb") as stdout_handle, open(stderr_path, "wb") as stderr_handle:
    proc = None
    try:
        proc = subprocess.Popen(
            ["bash", "-lc", command],
            env=os.environ.copy(),
            stdout=stdout_handle,
            stderr=stderr_handle,
            start_new_session=True,
        )
        exit_code = proc.wait(timeout=None if timeout_seconds <= 0 else timeout_seconds)
    except subprocess.TimeoutExpired:
        timed_out = True
        exit_code = 124
        if proc is not None:
            try:
                os.killpg(proc.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                try:
                    os.killpg(proc.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                proc.wait()

json.dump({"exitCode": exit_code, "timedOut": timed_out}, sys.stdout)
PY
  )"; then
    return 1
  fi
  printf '%s\n' "$status_json"
  exit_code="$(jq -er '.exitCode' <<<"$status_json" 2>/dev/null || printf '1\n')"
  return "$exit_code"
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
  local hold_seconds="${3:-10}"
  if [[ ! "$lease_name" =~ ^[A-Za-z0-9:._-]+$ ]] || [[ ! "$hold_seconds" =~ ^[1-9][0-9]*$ ]]; then
    return 1
  fi
  local deadline=$((SECONDS + 30))
  local lease_row="" guard_held=""
  local guard_app="synara-leader-takeover-guard-$$-$RANDOM"
  rm -f "$output_file"
  rm -f "$output_file.state"
  LEASE_GUARD_PID=""
  LEASE_GUARD_ROW=""
  LEASE_GUARD_APP="$guard_app"
  (
    "${kube[@]}" -n "$namespace" exec deployment/synara-stage2-postgres -- \
      env PGAPPNAME="$guard_app" \
      psql -U synara -d synara -v ON_ERROR_STOP=1 -At \
        -c "SELECT pg_advisory_lock(hashtextextended('$lease_name', 0)); SELECT pg_sleep($hold_seconds); SELECT pg_advisory_unlock(hashtextextended('$lease_name', 0));"
  ) >"$output_file" 2>&1 &
  LEASE_GUARD_PID="$!"
  while (( SECONDS < deadline )); do
    lease_row=""
    guard_held="$("${kube[@]}" -n "$namespace" exec deployment/synara-stage2-postgres -- \
      psql -U synara -d synara -v ON_ERROR_STOP=1 -At \
        -c "SELECT EXISTS (SELECT 1 FROM pg_stat_activity AS activity JOIN pg_locks AS held_lock ON held_lock.pid = activity.pid WHERE activity.application_name = '$guard_app' AND held_lock.locktype = 'advisory' AND held_lock.granted)" \
      2>/dev/null || true)"
    if [[ "$guard_held" == "t" ]]; then
      lease_row="$(read_reconciler_lease "$lease_name" 2>/dev/null || true)"
      printf 'guardHeld=%s\nleaseRow=%s\n' "$guard_held" "$lease_row" >"$output_file.state"
      if [[ "$lease_row" =~ ^[^|]+\|[1-9][0-9]*$ ]]; then
        LEASE_GUARD_ROW="$lease_row"
        return 0
      fi
    else
      printf 'guardHeld=%s\nleaseRow=\n' "$guard_held" >"$output_file.state"
    fi
    if ! kill -0 "$LEASE_GUARD_PID" >/dev/null 2>&1; then
      wait "$LEASE_GUARD_PID" >/dev/null 2>&1 || true
      LEASE_GUARD_PID=""
      LEASE_GUARD_APP=""
      return 1
    fi
    sleep 0.1
  done
  return 1
}

finish_reconciler_takeover_guard() {
  if [[ -z "$LEASE_GUARD_PID" ]]; then
    return 0
  fi
  local guard_pid="$LEASE_GUARD_PID"
  LEASE_GUARD_PID=""
  LEASE_GUARD_APP=""
  wait "$guard_pid"
}

stop_reconciler_takeover_guard() {
  if [[ -z "$LEASE_GUARD_PID" ]]; then
    return 0
  fi
  local guard_pid="$LEASE_GUARD_PID"
  LEASE_GUARD_PID=""
  LEASE_GUARD_APP=""
  kill "$guard_pid" >/dev/null 2>&1 || true
  wait "$guard_pid" >/dev/null 2>&1 || true
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
      | {node: .spec.nodeName, pod: .metadata.name}
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
    | {node: $target.node, pod: $target.pod, colocatedDependencyPods: []}'
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
}

run_partition_hook() {
  local phase="$1"
  local command_text="$2"
  local node="$3"
  local target_json="${4:-null}"
  local reason="${5:-}"
  local timeout_seconds="$6"
  local stdout_file="$work_dir/node-partition-hook-${phase}.stdout"
  local stderr_file="$work_dir/node-partition-hook-${phase}.stderr"
  local target_pod="" rc=0 status_json result_json extra_json
  if [[ -n "$target_json" && "$target_json" != "null" ]]; then
    target_pod="$(jq -r '.pod // empty' <<<"$target_json" 2>/dev/null || true)"
  fi
  rm -f "$stdout_file" "$stderr_file"
  if status_json="$(
    SYNARA_K8S_CONTEXT="$context" \
    SYNARA_K8S_NAMESPACE="$namespace" \
    SYNARA_K8S_EVIDENCE_FILE="$evidence_file" \
    SYNARA_K8S_HOOK_PHASE="node-partition-$phase" \
    SYNARA_K8S_NODE_PARTITION_PHASE="$phase" \
    SYNARA_K8S_NODE_PARTITION_CASE="node-partition" \
    SYNARA_K8S_NODE_PARTITION_CONTEXT="$context" \
    SYNARA_K8S_NODE_PARTITION_NAMESPACE="$namespace" \
    SYNARA_K8S_NODE_PARTITION_NODE="$node" \
    SYNARA_K8S_NODE_PARTITION_TARGET_POD="$target_pod" \
    SYNARA_K8S_NODE_PARTITION_TARGET_JSON="$target_json" \
    SYNARA_K8S_NODE_PARTITION_SECONDS="$partition_seconds" \
    SYNARA_K8S_NODE_PARTITION_TIMEOUT_SECONDS="$timeout_seconds" \
    SYNARA_K8S_NODE_PARTITION_RUNNER_PID="$$" \
    SYNARA_K8S_NODE_PARTITION_REASON="$reason" \
    SYNARA_K8S_NODE_PARTITION_EVIDENCE_FILE="$evidence_file" \
    SYNARA_K8S_NODE_PARTITION_JOURNAL_FILE="$journal_file" \
    SYNARA_K8S_NODE_PARTITION_PARTIAL_FILE="$partial_file" \
      run_command_capture "$command_text" "$stdout_file" "$stderr_file" "$timeout_seconds"
  )"; then
    rc=0
  else
    rc=$?
  fi
  if [[ -z "$status_json" ]]; then
    status_json='{"exitCode":1,"timedOut":false}'
    rc=1
  fi
  extra_json="$(jq -nc \
    --arg backend "managed-hook" \
    --arg node "$node" \
    --arg reason "$reason" \
    --argjson target "$target_json" \
    --argjson timeoutSeconds "$timeout_seconds" '
    {
      backend: $backend,
      node: $node,
      target: $target,
      timeoutSeconds: $timeoutSeconds,
      reason: (if $reason == "" then null else $reason end)
    }
    | with_entries(select(.value != null))')"
  result_json="$(redacted_command_result "$phase" "$command_text" "$status_json" "$stdout_file" "$stderr_file" "$extra_json")"
  printf '%s\n' "$result_json"
  return "$rc"
}

start_managed_partition_hook() {
  local node="$1"
  local target_json="$2"
  local start_json rc=0
  PARTITION_HOOK_NODE="$node"
  PARTITION_HOOK_TARGET_JSON="$target_json"
  PARTITION_HOOK_ATTEMPTED=1
  PARTITION_HOOK_START_JSON='null'
  PARTITION_HOOK_STOP_JSON='null'
  PARTITION_HOOK_RESULT_JSON='null'
  PARTITION_HOOK_ACTIVE=1
  if start_json="$(run_partition_hook start "$node_partition_start_hook" "$node" "$target_json" "case-start" "$node_partition_start_hook_timeout_seconds")"; then
    rc=0
  else
    rc=$?
  fi
  PARTITION_HOOK_RESULT_JSON="$start_json"
  PARTITION_HOOK_START_JSON="$start_json"
  return "$rc"
}

stop_managed_partition_hook() {
  local reason="${1:-case-complete}"
  local stop_json rc=0
  if [[ "$PARTITION_HOOK_ACTIVE" != "1" ]]; then
    PARTITION_HOOK_RESULT_JSON='null'
    printf 'null\n'
    return 0
  fi
  if stop_json="$(run_partition_hook stop "$node_partition_stop_hook" "$PARTITION_HOOK_NODE" "$PARTITION_HOOK_TARGET_JSON" "$reason" "$node_partition_stop_hook_timeout_seconds")"; then
    rc=0
  else
    rc=$?
  fi
  PARTITION_HOOK_RESULT_JSON="$stop_json"
  PARTITION_HOOK_STOP_JSON="$stop_json"
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
  if ! role_json="$("${kube[@]}" get clusterrole synara-control-plane-reconciler -o json)"; then
    CASE_DETAILS_JSON='{"error":"failed to read cluster role for RBAC validation"}'
    return 1
  fi
  run_rbac_check "$role_json" true create tokenreviews authentication.k8s.io "" || return 1
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
  local before_lease before_holder before_token before_pod before_json
  local after_lease after_holder after_token after_pod after_json
  local failures_file="$work_dir/leader-takeover-probe-failures"
  local probe_stop_file="$work_dir/leader-takeover-probe-stop"
  local lease_guard_file="$work_dir/leader-takeover-lease-guard"
  local takeover_rc failures guard_app guard_state guard_output current_lease

  start_continuous_probe "$probe_stop_file" "$failures_file"
  if ! start_reconciler_takeover_guard "$lease_name" "$lease_guard_file" 30; then
    guard_app="$LEASE_GUARD_APP"
    stop_reconciler_takeover_guard
    finish_continuous_probe "$probe_stop_file" || true
    failures="$(cat "$failures_file" 2>/dev/null || printf 0)"
    guard_state="$(cat "$lease_guard_file.state" 2>/dev/null || true)"
    guard_output="$(tail -c 2000 "$lease_guard_file" 2>/dev/null || true)"
    current_lease="$(read_reconciler_lease "$lease_name" 2>/dev/null || true)"
    CASE_DETAILS_JSON="$(jq -nc --arg lease "$lease_name" --arg guardApp "$guard_app" \
      --arg guardState "$guard_state" --arg guardOutput "$guard_output" --arg currentLease "$current_lease" \
      --argjson failures "$failures" '
      {error: "active reconciler lease could not be guarded", leaseName: $lease,
       guardApplicationName: $guardApp, guardState: $guardState, guardOutput: $guardOutput,
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
  if ! jq -e --arg pod "$before_pod" '
    any(.items[]?;
      .metadata.name == $pod
      and .metadata.deletionTimestamp == null
      and .status.phase == "Running"
      and any(.status.conditions[]?; .type == "Ready" and .status == "True")
    )
  ' <<<"$before_json" >/dev/null; then
    stop_reconciler_takeover_guard
    finish_continuous_probe "$probe_stop_file" || true
    failures="$(cat "$failures_file" 2>/dev/null || printf 0)"
    CASE_DETAILS_JSON="$(jq -nc --arg lease "$lease_name" --arg holder "$before_holder" --argjson token "$before_token" '
      {error: "reconciler lease holder is not a Ready Control Plane Pod", leaseName: $lease,
       before: {holderId: $holder, fencingToken: $token}}')"
    return 1
  fi

  if ! "${kube[@]}" -n "$namespace" delete pod "$before_pod" --wait=false >/dev/null; then
    stop_reconciler_takeover_guard
    finish_continuous_probe "$probe_stop_file" || true
    failures="$(cat "$failures_file" 2>/dev/null || printf 0)"
    CASE_DETAILS_JSON="$(jq -nc --arg lease "$lease_name" --arg pod "$before_pod" '
      {error: "reconciler leader Pod deletion failed", leaseName: $lease, deletedPod: $pod}')"
    return 1
  fi
  if ! finish_reconciler_takeover_guard; then
    finish_continuous_probe "$probe_stop_file" || true
    failures="$(cat "$failures_file" 2>/dev/null || printf 0)"
    CASE_DETAILS_JSON="$(jq -nc --arg lease "$lease_name" --arg pod "$before_pod" --argjson failures "$failures" '
      {error: "reconciler takeover guard failed after leader deletion was submitted", leaseName: $lease,
       deletedPod: $pod, readyProbeFailures: $failures}')"
    return 1
  fi
  if ! "${kube[@]}" -n "$namespace" wait --for=delete "pod/$before_pod" --timeout="${case_timeout_seconds}s" >/dev/null; then
    finish_continuous_probe "$probe_stop_file" || true
    failures="$(cat "$failures_file" 2>/dev/null || printf 0)"
    CASE_DETAILS_JSON="$(jq -nc --arg lease "$lease_name" --arg pod "$before_pod" --argjson failures "$failures" '
      {error: "reconciler leader Pod did not terminate", leaseName: $lease, deletedPod: $pod,
       readyProbeFailures: $failures}')"
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
    CASE_DETAILS_JSON="$(jq -nc --arg lease "$lease_name" --arg holder "$before_holder" --argjson token "$before_token" \
      --argjson failures "$failures" --argjson skipped "$([[ "$takeover_rc" == "2" ]] && printf true || printf false)" '
      {error: (if $skipped then "reconciler fencing token skipped an epoch" else "reconciler lease did not transfer" end),
       leaseName: $lease, before: {holderId: $holder, fencingToken: $token},
       readyProbeFailures: $failures}')"
    return 1
  fi
  after_holder="${after_lease%%|*}"
  after_token="${after_lease##*|}"
  after_pod="${after_holder%%:*}"
  if ! wait_for_control_plane_ready; then
    finish_continuous_probe "$probe_stop_file" || true
    failures="$(cat "$failures_file" 2>/dev/null || printf 0)"
    CASE_DETAILS_JSON="$(jq -nc --arg lease "$lease_name" --arg pod "$before_pod" \
      --arg holder "$after_holder" --argjson token "$after_token" --argjson failures "$failures" '
      {error: "control plane did not become ready after leader takeover", leaseName: $lease,
       deletedPod: $pod, after: {holderId: $holder, fencingToken: $token},
       readyProbeFailures: $failures}')"
    return 1
  fi
  if ! after_json="$(get_control_plane_pods_json)"; then
    finish_continuous_probe "$probe_stop_file" || true
    failures="$(read_probe_failures "$failures_file")"
    CASE_DETAILS_JSON="$(jq -nc --arg lease "$lease_name" --arg holder "$after_holder" --argjson token "$after_token" \
      --argjson failures "$failures" '
      {error: "failed to list control plane Pods after leader takeover", leaseName: $lease,
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
    CASE_DETAILS_JSON="$(jq -nc --arg lease "$lease_name" --arg holder "$after_holder" --argjson token "$after_token" \
      --argjson failures "$failures" '
      {error: "new reconciler lease holder is not a Ready Control Plane Pod", leaseName: $lease,
       after: {holderId: $holder, fencingToken: $token}, readyProbeFailures: $failures}')"
    return 1
  fi
  if ! finish_continuous_probe "$probe_stop_file"; then
    CASE_DETAILS_JSON="$(jq -nc --arg lease "$lease_name" --arg holder "$after_holder" --argjson token "$after_token" '
      {error: "continuous readiness probe failed during leader takeover", leaseName: $lease,
       after: {holderId: $holder, fencingToken: $token}}')"
    return 1
  fi
  failures="$(read_probe_failures "$failures_file")"
  if (( failures > max_failover_ready_failures )); then
    CASE_DETAILS_JSON="$(jq -nc --arg lease "$lease_name" --arg beforeHolder "$before_holder" \
      --arg afterHolder "$after_holder" --arg beforePod "$before_pod" --arg afterPod "$after_pod" \
      --argjson beforeToken "$before_token" --argjson afterToken "$after_token" --argjson failures "$failures" '
      {error: "readiness probe failures exceeded leader takeover threshold", leaseName: $lease,
       before: {holderId: $beforeHolder, pod: $beforePod, fencingToken: $beforeToken},
       after: {holderId: $afterHolder, pod: $afterPod, fencingToken: $afterToken},
       readyProbeFailures: $failures}')"
    return 1
  fi
  CASE_DETAILS_JSON="$(jq -nc --arg lease "$lease_name" --arg beforeHolder "$before_holder" \
    --arg afterHolder "$after_holder" --arg beforePod "$before_pod" --arg afterPod "$after_pod" \
    --argjson beforeToken "$before_token" --argjson afterToken "$after_token" --argjson failures "$failures" '
    {leaseName: $lease,
     before: {holderId: $beforeHolder, pod: $beforePod, fencingToken: $beforeToken},
     after: {holderId: $afterHolder, pod: $afterPod, fencingToken: $afterToken},
     readyProbeFailures: $failures}')"
}

case_control_plane_failover() {
  local before_json after_json deleted_pod replacement_json
  local failures_file="$work_dir/failover-probe-failures"
  local probe_pid pre_hook_json post_hook_json
  if ! before_json="$(get_control_plane_pods_json)"; then
    CASE_DETAILS_JSON='{"error":"failed to list control plane Pods before failover"}'
    return 1
  fi
  deleted_pod="${SYNARA_K8S_RESILIENCE_FAILOVER_POD:-$(jq -r '.items | sort_by(.metadata.creationTimestamp) | .[0].metadata.name' <<<"$before_json")}"
  if [[ -z "$deleted_pod" || "$deleted_pod" == "null" ]]; then
    CASE_DETAILS_JSON='{"error":"no control-plane pod available for failover"}'
    return 1
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
      --argjson replacement "$replacement_json" --argjson hook "$post_hook_json" '
      {
        error: "post-failover hook failed",
        deletedPod: $pod,
        readyProbeFailures: $failures,
        replacement: $replacement,
        hook: $hook
      }')"
    return 1
  }
  local failover_failures
  failover_failures="$(read_probe_failures "$failures_file")"
  if (( failover_failures > max_failover_ready_failures )); then
    CASE_DETAILS_JSON="$(jq -nc --arg pod "$deleted_pod" --argjson failures "$failover_failures" \
      --argjson replacement "$replacement_json" --argjson preHook "$pre_hook_json" --argjson postHook "$post_hook_json" '
      {
        error: "readiness probe failures exceeded failover threshold",
        deletedPod: $pod,
        readyProbeFailures: $failures,
        replacement: $replacement,
        preHook: $preHook,
        postHook: $postHook
      }')"
    return 1
  fi
  CASE_DETAILS_JSON="$(jq -nc --arg pod "$deleted_pod" --argjson failures "$failover_failures" \
    --argjson replacement "$replacement_json" --argjson preHook "$pre_hook_json" --argjson postHook "$post_hook_json" '
    {
      deletedPod: $pod,
      readyProbeFailures: $failures,
      replacement: $replacement,
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
  local target_json node network before_ready_json after_ready_json
  local failures_file="$work_dir/partition-probe-failures"
  local probe_pid partition_failures managed_probe_window_seconds start_hook_json stop_hook_json start_hook_rc stop_hook_rc
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
  if [[ "$context" != kind-* ]]; then
    if [[ -z "$node_partition_start_hook" && -z "$node_partition_stop_hook" ]]; then
      CASE_DETAILS_JSON='{"reason":"node partition for non-Kind contexts requires explicit SYNARA_K8S_NODE_PARTITION_START_HOOK and SYNARA_K8S_NODE_PARTITION_STOP_HOOK commands"}'
      return 2
    fi
    if [[ -z "$node_partition_start_hook" || -z "$node_partition_stop_hook" ]]; then
      CASE_DETAILS_JSON="$(jq -nc \
        --argjson startConfigured "$([[ -n "$node_partition_start_hook" ]] && printf true || printf false)" \
        --argjson stopConfigured "$([[ -n "$node_partition_stop_hook" ]] && printf true || printf false)" '
        {
          error: "managed node partition hooks must configure both start and stop commands",
          backend: "managed-hook",
          startHookConfigured: $startConfigured,
          stopHookConfigured: $stopConfigured
        }')"
      return 1
    fi
    managed_probe_window_seconds=$((partition_seconds + disruption_probe_window_seconds + node_partition_start_hook_timeout_seconds + node_partition_stop_hook_timeout_seconds))
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
        --argjson stopHook "$stop_hook_json" '
        {
          error: "managed node partition stop hook failed",
          backend: "managed-hook",
          node: $node,
          target: $target,
          partitionSeconds: '"$partition_seconds"',
          startHook: $startHook,
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
        --argjson stopHook "$stop_hook_json" '
        {
          error: "readiness probe process failed during managed node partition",
          backend: "managed-hook",
          node: $node,
          target: $target,
          startHook: $startHook,
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
        --argjson stopHook "$stop_hook_json" \
        --argjson failures "$(read_probe_failures "$failures_file")" '
        {
          error: "failed to read node state after managed node partition",
          backend: "managed-hook",
          node: $node,
          target: $target,
          readyProbeFailures: $failures,
          startHook: $startHook,
          stopHook: $stopHook
        }')"
      return 1
    fi
    if ! wait_for_control_plane_ready; then
      CASE_DETAILS_JSON="$(jq -nc \
        --arg node "$node" \
        --argjson target "$target_json" \
        --argjson startHook "$start_hook_json" \
        --argjson stopHook "$stop_hook_json" \
        --argjson failures "$(read_probe_failures "$failures_file")" '
        {
          error: "control plane did not become ready after managed node partition",
          backend: "managed-hook",
          node: $node,
          target: $target,
          readyProbeFailures: $failures,
          startHook: $startHook,
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

execute_case() {
  local case_name="$1"
  case "$case_name" in
    rbac) case_rbac ;;
    topology) case_topology ;;
    leader-takeover) case_leader_takeover ;;
    control-plane-failover) case_control_plane_failover ;;
    node-drain) case_node_drain ;;
    node-partition) case_node_partition ;;
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
  local final_progress_json
  finished_at="$(iso_now)"
  duration_seconds=$(( $(date +%s) - started_epoch ))
  overall_status="passed"
  if (( overall_failed != 0 )) || [[ "$baseline_status" == "failed" ]]; then
    overall_status="failed"
  fi
  current_phase_json='{"type":"finalizing"}'
  jq -n \
    --arg schemaVersion "synara.kubernetes.resilience.acceptance.v1" \
    --arg status "$overall_status" \
    --arg startedAt "$started_at" \
    --arg finishedAt "$finished_at" \
    --arg context "$context" \
    --arg namespace "$namespace" \
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
      evidenceFile: $evidenceFile,
      safety: {
        kindOnlyByDefault: true,
        allowNonDisposableOverride: "SYNARA_K8S_ACCEPTANCE_ALLOW_NONDISPOSABLE",
        nodePartitionManagedHookOverride: {
          startHookEnvVar: "SYNARA_K8S_NODE_PARTITION_START_HOOK",
          stopHookEnvVar: "SYNARA_K8S_NODE_PARTITION_STOP_HOOK",
          timeoutEnvVar: "SYNARA_K8S_NODE_PARTITION_HOOK_TIMEOUT_SECONDS",
          startTimeoutEnvVar: "SYNARA_K8S_NODE_PARTITION_START_HOOK_TIMEOUT_SECONDS",
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
    }' >"$evidence_file"
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
require_non_negative_int "$soak_seconds" "SYNARA_K8S_RESILIENCE_SOAK_SECONDS"
require_positive_int "$soak_interval_seconds" "SYNARA_K8S_RESILIENCE_SOAK_INTERVAL_SECONDS"
require_positive_int "$probe_interval_seconds" "SYNARA_K8S_RESILIENCE_PROBE_INTERVAL_SECONDS"
require_positive_int "$disruption_probe_window_seconds" "SYNARA_K8S_RESILIENCE_DISRUPTION_WINDOW_SECONDS"
require_positive_int "$partition_seconds" "SYNARA_K8S_RESILIENCE_PARTITION_SECONDS"
require_positive_int "$node_partition_hook_timeout_seconds" "SYNARA_K8S_NODE_PARTITION_HOOK_TIMEOUT_SECONDS"
require_positive_int "$node_partition_start_hook_timeout_seconds" "SYNARA_K8S_NODE_PARTITION_START_HOOK_TIMEOUT_SECONDS"
require_positive_int "$node_partition_stop_hook_timeout_seconds" "SYNARA_K8S_NODE_PARTITION_STOP_HOOK_TIMEOUT_SECONDS"
require_positive_int "$case_timeout_seconds" "SYNARA_K8S_RESILIENCE_CASE_TIMEOUT_SECONDS"
require_positive_int "$min_worker_nodes" "SYNARA_K8S_RESILIENCE_MIN_WORKER_NODES"
require_non_negative_int "$max_failover_ready_failures" "SYNARA_K8S_RESILIENCE_MAX_FAILOVER_READY_FAILURES"
require_non_negative_int "$max_drain_ready_failures" "SYNARA_K8S_RESILIENCE_MAX_DRAIN_READY_FAILURES"
require_non_negative_int "$max_partition_ready_failures" "SYNARA_K8S_RESILIENCE_MAX_PARTITION_READY_FAILURES"
if (( soak_seconds > 0 )) && [[ "$evidence_file_is_explicit" != "1" ]]; then
  printf 'SYNARA_K8S_RESILIENCE_EVIDENCE_FILE must be set explicitly when soak is enabled\n' >&2
  exit 1
fi

if [[ "$dry_run" == "1" ]]; then
  jq -n \
    --arg schemaVersion "synara.kubernetes.resilience.acceptance.v1" \
    --arg baselineScript "$baseline_script" \
    --arg context "$context" \
    --arg namespace "$namespace" \
    --arg cases "$cases_csv" \
    --arg allowSkippedCases "$allow_skipped_cases_csv" \
    --arg soakCases "$soak_cases_csv" \
    --arg evidenceFile "$evidence_file" \
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
      safety: {
        kindOnlyByDefault: true,
        allowNonDisposableOverride: "SYNARA_K8S_ACCEPTANCE_ALLOW_NONDISPOSABLE",
        nodePartitionManagedHookOverride: {
          startHookEnvVar: "SYNARA_K8S_NODE_PARTITION_START_HOOK",
          stopHookEnvVar: "SYNARA_K8S_NODE_PARTITION_STOP_HOOK",
          timeoutEnvVar: "SYNARA_K8S_NODE_PARTITION_HOOK_TIMEOUT_SECONDS",
          startTimeoutEnvVar: "SYNARA_K8S_NODE_PARTITION_START_HOOK_TIMEOUT_SECONDS",
          stopTimeoutEnvVar: "SYNARA_K8S_NODE_PARTITION_STOP_HOOK_TIMEOUT_SECONDS"
        }
      },
      evidenceFile: $evidenceFile
    }' >"$evidence_file"
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
