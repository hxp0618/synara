#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
MIN_DURATION_SECONDS="${SYNARA_SANDBOX_OPERATOR_SOAK_MIN_SECONDS:-300}"
MIN_RUNS="${SYNARA_SANDBOX_OPERATOR_SOAK_MIN_RUNS:-1}"
MAX_RUNS="${SYNARA_SANDBOX_OPERATOR_SOAK_MAX_RUNS:-100}"
MAX_PROVIDER_READY_P95_MS="${SYNARA_SANDBOX_OPERATOR_SOAK_MAX_PROVIDER_READY_P95_MS:-2500}"
MAX_FAILOVER_SECONDS="${SYNARA_SANDBOX_OPERATOR_SOAK_MAX_FAILOVER_SECONDS:-30}"
MAX_CONTROLLER_CONFLICT_LINES="${SYNARA_SANDBOX_OPERATOR_SOAK_MAX_CONTROLLER_CONFLICT_LINES:-20}"
MAX_WARM_POOL_DEFICIT="${SYNARA_SANDBOX_OPERATOR_SOAK_MAX_WARM_POOL_DEFICIT:-1}"
MIN_WARM_HIT_RATE_PERCENT="${SYNARA_SANDBOX_OPERATOR_SOAK_MIN_WARM_HIT_RATE_PERCENT:-95}"
REQUIRED_NODE_LOSS_MODE="${SYNARA_SANDBOX_OPERATOR_SOAK_REQUIRED_NODE_LOSS_MODE:-pod-delete}"
RUN_ID="$(date -u +%Y%m%dT%H%M%SZ)"
RESULT_DIR="${SYNARA_SANDBOX_OPERATOR_SOAK_RESULT_DIR:-$ROOT_DIR/.tmp/sandbox-operator-soak/$RUN_ID}"
JOURNAL="$RESULT_DIR/journal.jsonl"
SUMMARY="$RESULT_DIR/summary.json"
operator_log_pid=""

cleanup() {
  if [[ -n "$operator_log_pid" ]]; then
    kill "$operator_log_pid" >/dev/null 2>&1 || true
    wait "$operator_log_pid" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

collect_operator_logs() {
  local output="$1"
  while true; do
    kubectl --context "$SYNARA_TEST_KUBERNETES_CONTEXT" \
      -n sandbox-operator-system logs -l app.kubernetes.io/name=sandbox-operator \
      --all-containers=true --prefix --since=2s >>"$output" 2>&1 || true
    sleep 1
  done
}

required_environment=(
  SYNARA_TEST_KUBERNETES_API_SERVER
  SYNARA_TEST_KUBERNETES_BEARER_TOKEN
  SYNARA_TEST_KUBERNETES_CA_CERTIFICATE
  SYNARA_TEST_KUBERNETES_WORKER_IMAGE
  SYNARA_TEST_KUBERNETES_CONTEXT
)
for name in "${required_environment[@]}"; do
  [[ -n "${!name:-}" ]] || { echo "$name is required" >&2; exit 2; }
done
for command in go kubectl python3 tee; do
  command -v "$command" >/dev/null || { echo "missing required command: $command" >&2; exit 2; }
done
for value in "$MIN_DURATION_SECONDS" "$MIN_RUNS" "$MAX_RUNS" "$MAX_PROVIDER_READY_P95_MS" "$MAX_FAILOVER_SECONDS" "$MAX_CONTROLLER_CONFLICT_LINES" "$MAX_WARM_POOL_DEFICIT" "$MIN_WARM_HIT_RATE_PERCENT"; do
  [[ "$value" =~ ^[0-9]+$ ]] || { echo "soak duration and run bounds must be non-negative integers" >&2; exit 2; }
done
(( MIN_RUNS >= 1 )) || { echo "SYNARA_SANDBOX_OPERATOR_SOAK_MIN_RUNS must be at least 1" >&2; exit 2; }
(( MAX_RUNS >= MIN_RUNS )) || { echo "SYNARA_SANDBOX_OPERATOR_SOAK_MAX_RUNS must be at least MIN_RUNS" >&2; exit 2; }
(( MIN_WARM_HIT_RATE_PERCENT <= 100 )) || { echo "SYNARA_SANDBOX_OPERATOR_SOAK_MIN_WARM_HIT_RATE_PERCENT must not exceed 100" >&2; exit 2; }
[[ "$REQUIRED_NODE_LOSS_MODE" == "pod-delete" || "$REQUIRED_NODE_LOSS_MODE" == "node-hook" ]] || {
  echo "SYNARA_SANDBOX_OPERATOR_SOAK_REQUIRED_NODE_LOSS_MODE must be pod-delete or node-hook" >&2
  exit 2
}

mkdir -p "$RESULT_DIR"
: >"$JOURNAL"
kubectl --context "$SYNARA_TEST_KUBERNETES_CONTEXT" cluster-info >/dev/null

started_epoch="$(python3 -c 'import time; print(time.time())')"
run=0
while true; do
  elapsed="$(python3 - "$started_epoch" <<'PY'
import sys, time
print(time.time() - float(sys.argv[1]))
PY
)"
  if (( run >= MIN_RUNS )) && python3 - "$elapsed" "$MIN_DURATION_SECONDS" <<'PY'
import sys
raise SystemExit(0 if float(sys.argv[1]) >= int(sys.argv[2]) else 1)
PY
  then
    break
  fi
  (( run < MAX_RUNS )) || {
    echo "soak maximum run bound reached before minimum duration" >&2
    exit 1
  }

  run=$((run + 1))
  log="$RESULT_DIR/run-$(printf '%04d' "$run").log"
  operator_log="$RESULT_DIR/run-$(printf '%04d' "$run").operator.log"
  run_started="$(python3 -c 'import time; print(time.time())')"
  : >"$operator_log"
  collect_operator_logs "$operator_log" &
  operator_log_pid=$!
  set +e
  (
    cd "$ROOT_DIR/services/control-plane"
    SYNARA_SANDBOX_OPERATOR_CONTROL_PLANE_TEST=1 \
      go test ./internal/executions \
        -run TestSandboxOperatorRealControlPlaneRegistrationAndGenerationFencing \
        -count=1 -v
  ) 2>&1 | tee "$log"
  status="${PIPESTATUS[0]}"
  kill "$operator_log_pid" >/dev/null 2>&1 || true
  wait "$operator_log_pid" >/dev/null 2>&1 || true
  operator_log_pid=""
  set -e

  python3 - "$JOURNAL" "$log" "$operator_log" "$run" "$run_started" "$status" "$REQUIRED_NODE_LOSS_MODE" <<'PY'
import json, os, re, sys, time
journal, log_path, operator_log_path, run, started, status, required_node_loss_mode = sys.argv[1:]
text = open(log_path, encoding="utf-8", errors="replace").read()
operator_text = open(operator_log_path, encoding="utf-8", errors="replace").read()
match = re.search(
    r"Sandbox real control-plane PASS .*?initialLaunchType=(?P<launch>\S+) "
    r"providerReady=(?P<provider>[0-9.]+)(?P<provider_unit>ms|s).*?"
    r"staleCode=(?P<stale>\S+) nodeLossMode=(?P<node_loss_mode>\S+) "
    r"nodeLossRecoveryAuthority=(?P<recovery_authority>\S+) "
    r"nodeLossOldNode=(?P<old_node>\S+) nodeLossNewNode=(?P<new_node>\S+) "
    r"nodeLossOldPodUID=(?P<old_pod_uid>\S+) "
    r"nodeLossNewPodUID=(?P<new_pod_uid>\S+) "
    r"nodeLossRecovery=(?P<node_loss>[0-9.]+)(?P<node_loss_unit>ms|s).*?"
    r"restartExecutions=\[(?P<restart_executions>.*?)\].*?"
    r"leaderBefore=(?P<leader_before>\S+) leaderAfter=(?P<leader_after>\S+) "
    r"failover=(?P<failover>[0-9.]+)(?P<failover_unit>ms|s).*?"
    r"maxWarmPoolDeficit=(?P<max_deficit>\d+) finalWarmPoolDeficit=(?P<final_deficit>\d+)",
    text,
)
conflict_lines = sum(
    1 for line in set(operator_text.splitlines())
    if re.search(r"conflict|object has been modified", line, re.IGNORECASE)
)
combined = text + "\n" + operator_text
secret_leak_kinds = set()
exact_secrets = {
    "testKubernetesBearerToken": os.environ.get("SYNARA_TEST_KUBERNETES_BEARER_TOKEN", ""),
    "providerCredentialSentinel": "stage3-provider-acceptance-credential-v1",
}
for kind, value in exact_secrets.items():
    if value and value in combined:
        secret_leak_kinds.add(kind)
if re.search(r"\beyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{10,}\b", combined):
    secret_leak_kinds.add("jwt")
if re.search(
    r"(?i)\b(?:authorization|bearer|leaseToken|registrationToken|apiKey|clientSecret)\b"
    r"\s*[:=]\s*[\"']?[A-Za-z0-9._~+/=-]{16,}",
    combined,
):
    secret_leak_kinds.add("credentialKeyValue")
duration = time.time() - float(started)
node_loss_fenced = bool(
    match
    and match.group("stale") == "kubernetes_sandbox_allocation_generation_stale"
    and match.group("recovery_authority") == "lease-expiry"
    and match.group("node_loss_mode") == required_node_loss_mode
    and match.group("old_pod_uid") != match.group("new_pod_uid")
    and (required_node_loss_mode != "node-hook" or match.group("old_node") != match.group("new_node"))
)
record = {
    "run": int(run),
    "passed": int(status) == 0 and match is not None and node_loss_fenced and not secret_leak_kinds,
    "exitCode": int(status),
    "durationSeconds": round(duration, 3),
    "log": log_path,
    "operatorLog": operator_log_path,
    "controllerConflictLines": conflict_lines,
    "secretLeakMatches": len(secret_leak_kinds),
    "secretLeakKinds": sorted(secret_leak_kinds),
    "secretScanPassed": not secret_leak_kinds,
    "backingPodLossFenced": node_loss_fenced,
}
if match:
    provider_value = float(match.group("provider"))
    failover_value = float(match.group("failover"))
    node_loss_value = float(match.group("node_loss"))
    record.update({
        "initialLaunchType": match.group("launch"),
        "providerReadyMilliseconds": round(provider_value * 1000 if match.group("provider_unit") == "s" else provider_value, 3),
        "executionCount": 1 + len(match.group("restart_executions").split()),
        "generationCompletionCount": 1 + len(match.group("restart_executions").split()),
        "generationTransitionCount": 3 + len(match.group("restart_executions").split()),
        "nodeLossRecoveryAuthority": match.group("recovery_authority"),
        "nodeLossMode": match.group("node_loss_mode"),
        "nodeLossOldNode": match.group("old_node"),
        "nodeLossNewNode": match.group("new_node"),
        "nodeLossOldPodUID": match.group("old_pod_uid"),
        "nodeLossNewPodUID": match.group("new_pod_uid"),
        "nodeLossRecoverySeconds": round(node_loss_value / 1000 if match.group("node_loss_unit") == "ms" else node_loss_value, 3),
        "leaderBefore": match.group("leader_before"),
        "leaderAfter": match.group("leader_after"),
        "failoverSeconds": round(failover_value / 1000 if match.group("failover_unit") == "ms" else failover_value, 3),
        "maxWarmPoolDeficit": int(match.group("max_deficit")),
        "finalWarmPoolDeficit": int(match.group("final_deficit")),
    })
with open(journal, "a", encoding="utf-8") as output:
    output.write(json.dumps(record, sort_keys=True) + "\n")
if not record["passed"]:
    raise SystemExit("real control-plane soak iteration failed or emitted incomplete evidence")
PY
done

python3 - "$JOURNAL" "$SUMMARY" "$RUN_ID" "$started_epoch" "$MIN_DURATION_SECONDS" "$MIN_RUNS" "$MAX_RUNS" "$MAX_PROVIDER_READY_P95_MS" "$MAX_FAILOVER_SECONDS" "$MAX_CONTROLLER_CONFLICT_LINES" "$MAX_WARM_POOL_DEFICIT" "$MIN_WARM_HIT_RATE_PERCENT" "$REQUIRED_NODE_LOSS_MODE" <<'PY'
import json, statistics, sys, time
journal, summary_path, run_id, started, min_duration, min_runs, max_runs, max_provider_p95, max_failover, max_conflicts, max_deficit, min_warm_rate, required_node_loss_mode = sys.argv[1:]
records = [json.loads(line) for line in open(journal, encoding="utf-8") if line.strip()]
elapsed = time.time() - float(started)
provider_values = sorted(record["providerReadyMilliseconds"] for record in records)
def percentile(values, p):
    index = (len(values) - 1) * p
    lower = int(index)
    upper = min(lower + 1, len(values) - 1)
    return values[lower] + (values[upper] - values[lower]) * (index - lower)
result = {
    "schemaVersion": 1,
    "runId": run_id,
    "passed": bool(records) and all(record["passed"] for record in records),
    "runs": len(records),
    "minimumRuns": int(min_runs),
    "maximumRuns": int(max_runs),
    "minimumDurationSeconds": int(min_duration),
    "observedDurationSeconds": round(elapsed, 3),
    "executionsCompleted": sum(record.get("executionCount", 0) for record in records),
    "generationsCompleted": sum(record.get("generationCompletionCount", 0) for record in records),
    "generationTransitionsObserved": sum(record.get("generationTransitionCount", 0) for record in records),
    "warmHits": sum(record["initialLaunchType"] == "warm" for record in records),
    "warmHitRatePercent": round(100 * sum(record["initialLaunchType"] == "warm" for record in records) / len(records), 3),
    "minimumWarmHitRatePercent": int(min_warm_rate),
    "requiredNodeLossMode": required_node_loss_mode,
    "providerReadyP95Milliseconds": round(percentile(provider_values, 0.95), 3),
    "maximumProviderReadyP95Milliseconds": int(max_provider_p95),
    "failoverP50Seconds": round(statistics.median(record["failoverSeconds"] for record in records), 3),
    "failoverMaxSeconds": round(max(record["failoverSeconds"] for record in records), 3),
    "maximumFailoverSeconds": int(max_failover),
    "controllerConflictLines": sum(record["controllerConflictLines"] for record in records),
    "controllerConflictLinesMaxPerRun": max(record["controllerConflictLines"] for record in records),
    "maximumControllerConflictLinesPerRun": int(max_conflicts),
    "warmPoolDeficitMax": max(record["maxWarmPoolDeficit"] for record in records),
    "maximumWarmPoolDeficit": int(max_deficit),
    "allWarmPoolsRecovered": all(record["finalWarmPoolDeficit"] == 0 for record in records),
    "allLeadersChanged": all(record.get("leaderBefore") != record.get("leaderAfter") for record in records),
    "backingPodLossRecoveryMaxSeconds": round(max(record["nodeLossRecoverySeconds"] for record in records), 3),
    "allBackingPodLossUIDsFenced": all(record["backingPodLossFenced"] for record in records),
    "allBackingPodLossLeaseRecovered": all(record.get("nodeLossRecoveryAuthority") == "lease-expiry" for record in records),
    "allRequiredNodeLossModesObserved": all(record.get("nodeLossMode") == required_node_loss_mode for record in records),
    "secretLeakMatches": sum(record["secretLeakMatches"] for record in records),
    "secretScanPassed": all(record["secretScanPassed"] for record in records),
    "journal": journal,
}
result["passed"] = (
    result["passed"]
    and result["allLeadersChanged"]
    and result["allBackingPodLossUIDsFenced"]
    and result["allBackingPodLossLeaseRecovered"]
    and result["allRequiredNodeLossModesObserved"]
    and result["secretScanPassed"]
    and elapsed >= int(min_duration)
    and len(records) >= int(min_runs)
    and result["providerReadyP95Milliseconds"] <= int(max_provider_p95)
    and result["failoverMaxSeconds"] <= int(max_failover)
    and result["controllerConflictLinesMaxPerRun"] <= int(max_conflicts)
    and result["warmPoolDeficitMax"] <= int(max_deficit)
    and result["allWarmPoolsRecovered"]
    and result["warmHitRatePercent"] >= int(min_warm_rate)
)
with open(summary_path, "w", encoding="utf-8") as output:
    json.dump(result, output, indent=2, sort_keys=True)
print(json.dumps(result, indent=2, sort_keys=True))
if not result["passed"]:
    raise SystemExit("soak acceptance thresholds were not satisfied")
PY

echo "Sandbox operator soak evidence: $RESULT_DIR"
