#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
base_url="${1:-http://127.0.0.1:3773}"
worker_protocol_version=2
worker_version="${SYNARA_ACCEPTANCE_WORKER_VERSION:-acceptance}"
run_id="$(date +%s)-$$"
work_dir="$(mktemp -d)"
sse_pid=""
cleanup() {
  if [[ -n "$sse_pid" ]]; then
    kill "$sse_pid" >/dev/null 2>&1 || true
    wait "$sse_pid" >/dev/null 2>&1 || true
  fi
  rm -rf "$work_dir"
}
trap cleanup EXIT
trap 'status=$?; printf "Self-hosted acceptance stopped at line %s with status %s\n" "$LINENO" "$status" >&2; exit "$status"' ERR

request_json() {
  local cookie_jar="$1"
  local method="$2"
  local path="$3"
  local body="${4:-}"
  local idempotency_key="${5:-}"
  local args=(-sS --fail-with-body -b "$cookie_jar" -c "$cookie_jar" -X "$method")
  if [[ -n "$idempotency_key" ]]; then
    args+=(-H "Idempotency-Key: $idempotency_key")
  fi
  if [[ -n "$body" ]]; then
    args+=(-H 'Content-Type: application/json' -d "$body")
  fi
  local response_file
  response_file="$(mktemp "$work_dir/http-response.XXXXXX")"
  if ! curl "${args[@]}" -o "$response_file" "$base_url$path"; then
    sed 's/^/HTTP error response: /' "$response_file" >&2
    rm -f "$response_file"
    return 1
  fi
  cat "$response_file"
  rm -f "$response_file"
}

worker_json() {
  local token="$1"
  local request_id="$2"
  local method="$3"
  local path="$4"
  local body="${5:-}"
  local args=(-sS --fail-with-body -X "$method" -H "Authorization: Bearer $token" -H "X-Request-ID: $request_id")
  if [[ -n "$body" ]]; then
    args+=(-H 'Content-Type: application/json' -d "$body")
  fi
  local response_file
  response_file="$(mktemp "$work_dir/worker-response.XXXXXX")"
  if ! curl "${args[@]}" -o "$response_file" "$base_url$path"; then
    sed 's/^/Worker HTTP error response: /' "$response_file" >&2
    rm -f "$response_file"
    return 1
  fi
  cat "$response_file"
  rm -f "$response_file"
}

new_uuid() {
  local value
  value="$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
  printf '%s-%s-%s-%s-%s\n' \
    "${value:0:8}" "${value:8:4}" "${value:12:4}" "${value:16:4}" "${value:20:12}"
}

owner_cookie="$work_dir/owner.cookie"
member_cookie="$work_dir/member.cookie"
outsider_cookie="$work_dir/outsider.cookie"

platform_profile="$(curl -sS --fail "$base_url/v1/platform/profile")"
jq -e '
  .commercializationMode == "internal-self-hosted" and
  (.internalStatusBoard.configured | type == "boolean") and
  (has("commercialBilling") | not) and
  (has("paymentProvider") | not) and
  (has("payment") | not) and
  (has("checkout") | not) and
  (has("stripe") | not) and
  (has("billing") | not) and
  (has("statusPage") | not)
' <<<"$platform_profile" >/dev/null

owner_session="$(request_json "$owner_cookie" POST /v1/auth/dev-login \
  "{\"email\":\"owner-$run_id@example.com\",\"displayName\":\"Acceptance Owner\"}")"
owner_id="$(jq -er '.user.userId' <<<"$owner_session")"
tenant="$(request_json "$owner_cookie" POST /v1/tenants \
  "{\"slug\":\"acceptance-tenant-$run_id\",\"name\":\"Acceptance Internal Tenant\",\"region\":\"local\"}")"
tenant_id="$(jq -er '.id' <<<"$tenant")"
jq -e '.entitlementProfileCode == "standard" and .status == "active" and .role == "owner"' \
  <<<"$tenant" >/dev/null
request_json "$owner_cookie" PUT /v1/auth/active-tenant "{\"tenantId\":\"$tenant_id\"}" >/dev/null

organization="$(request_json "$owner_cookie" POST "/v1/tenants/$tenant_id/organizations" \
  "{\"slug\":\"acceptance-$run_id\",\"name\":\"Acceptance Engineering\",\"kind\":\"department\",\"settings\":{}}")"
organization_id="$(jq -er '.id' <<<"$organization")"

execution_target="$(request_json "$owner_cookie" POST "/v1/tenants/$tenant_id/execution-targets" \
  "{\"organizationId\":\"$organization_id\",\"kind\":\"local\",\"name\":\"Acceptance Local Target\",\"configuration\":{},\"capabilities\":{\"workspaceModes\":[\"local\",\"worktree\"],\"providerPolicy\":{\"experimentalProviders\":[\"codex\",\"claudeAgent\"]}}}")"
created_execution_target_id="$(jq -er '.id' <<<"$execution_target")"
jq -e '.kind == "local" and .status == "active" and .productBoundary == "single-tenant-trusted"' \
  <<<"$execution_target" >/dev/null

project="$(request_json "$owner_cookie" POST "/v1/tenants/$tenant_id/organizations/$organization_id/projects" \
  "{\"name\":\"Acceptance Project\",\"repositoryUrl\":\"https://example.com/synara.git\",\"defaultBranch\":\"main\",\"visibility\":\"organization\"}")"
project_id="$(jq -er '.id' <<<"$project")"

agent_session="$(request_json "$owner_cookie" POST "/v1/projects/$project_id/sessions" \
  '{"title":"Acceptance Session","visibility":"project","provider":"codex","model":"gpt-5.6-sol"}')"
session_id="$(jq -er '.id' <<<"$agent_session")"
execution_target_id="$(jq -er '.executionTargetId' <<<"$agent_session")"
[[ "$execution_target_id" == "$created_execution_target_id" ]]
execution_target="$(request_json "$owner_cookie" GET "/v1/tenants/$tenant_id/execution-targets/$execution_target_id")"
target_kind="$(jq -er '.kind' <<<"$execution_target")"
worker_capabilities="$(python3 "$repo_root/scripts/stage3-provider-acceptance/worker_manifest.py" \
  --worker-version "$worker_version" \
  --target-capabilities-json "$(jq -c '.capabilities' <<<"$execution_target")")"
jq -e '.lastEventSequence == 1 and .status == "active"' <<<"$agent_session" >/dev/null

curl -sS -N --max-time 10 -b "$owner_cookie" \
  "$base_url/v1/sessions/$session_id/events/stream?afterSequence=1" \
  >"$work_dir/events.sse" &
sse_pid="$!"
for _ in {1..50}; do
  grep -q '^retry: 2000' "$work_dir/events.sse" && break
  sleep 0.1
done
grep -q '^retry: 2000' "$work_dir/events.sse"

request_json "$owner_cookie" POST "/v1/sessions/$session_id/turns" \
  '{"inputText":"First acceptance turn"}' >/dev/null

for _ in {1..50}; do
  grep -q '^id: 2$' "$work_dir/events.sse" && break
  sleep 0.1
done
grep -q '^id: 2$' "$work_dir/events.sse"
grep -q '^event: session-event$' "$work_dir/events.sse"
kill "$sse_pid" >/dev/null 2>&1 || true
wait "$sse_pid" >/dev/null 2>&1 || true
sse_pid=""

events="$(request_json "$owner_cookie" GET "/v1/sessions/$session_id/events?afterSequence=1&limit=10")"
jq -e '.lastSequence == 2 and (.items | map(.sequence) == [2])' <<<"$events" >/dev/null
jq -e '.items | all(.tenantId != null and .organizationId != null and .projectId != null)' \
  <<<"$events" >/dev/null

worker_registration_token="${SYNARA_ACCEPTANCE_WORKER_REGISTRATION_TOKEN:-acceptance-worker-registration-token}"
worker_instance_uid="$(new_uuid)"
worker_registration="$(worker_json "$worker_registration_token" "register-$run_id" POST /v1/workers/register \
  "{\"executionTargetId\":\"$execution_target_id\",\"targetKind\":\"$target_kind\",\"instanceUid\":\"$worker_instance_uid\",\"clusterId\":\"acceptance\",\"namespace\":\"default\",\"podName\":\"worker-$run_id\",\"version\":\"$worker_version\",\"protocolVersion\":$worker_protocol_version,\"capabilities\":$worker_capabilities,\"leaseSupported\":true,\"fencingSupported\":true}")"
worker_token="$(jq -er '.token' <<<"$worker_registration")"
worker_id="$(jq -er '.worker.id' <<<"$worker_registration")"

worker_json "$worker_token" "heartbeat-$run_id" POST /v1/workers/heartbeat \
  "{\"version\":\"$worker_version\",\"protocolVersion\":$worker_protocol_version,\"capabilities\":$worker_capabilities}" >/dev/null

first_claim="$(worker_json "$worker_token" "claim-first-$run_id" POST /v1/workers/executions/claim \
  "{\"executionTargetId\":\"$execution_target_id\",\"targetKind\":\"$target_kind\"}")"
first_execution_id="$(jq -er '.execution.id' <<<"$first_claim")"
first_generation="$(jq -er '.lease.generation' <<<"$first_claim")"
first_lease_token="$(jq -er '.lease.leaseToken' <<<"$first_claim")"
first_runtime_binding_id="$(jq -er '.workload.providerRuntimeBindingId' <<<"$first_claim")"
jq -e --arg session_id "$session_id" '.execution.sessionId == $session_id and .execution.status == "leased"' \
  <<<"$first_claim" >/dev/null

first_envelope="{\"tenantId\":\"$tenant_id\",\"generation\":$first_generation,\"leaseToken\":\"$first_lease_token\"}"
worker_json "$worker_token" "start-first-$run_id" POST "/v1/workers/executions/$first_execution_id/start" \
  "$first_envelope" >/dev/null
runtime_event_id="$(new_uuid)"
worker_json "$worker_token" "event-first-$run_id" POST "/v1/workers/executions/$first_execution_id/events" \
  "{\"tenantId\":\"$tenant_id\",\"generation\":$first_generation,\"leaseToken\":\"$first_lease_token\",\"eventId\":\"$runtime_event_id\",\"eventVersion\":1,\"eventType\":\"runtime.output.delta\",\"payload\":{\"text\":\"acceptance output\"},\"occurredAt\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}" >/dev/null
token_usage_event_id="$(new_uuid)"
worker_json "$worker_token" "token-usage-first-$run_id" POST "/v1/workers/executions/$first_execution_id/events" \
  "{\"tenantId\":\"$tenant_id\",\"generation\":$first_generation,\"leaseToken\":\"$first_lease_token\",\"eventId\":\"$token_usage_event_id\",\"eventVersion\":2,\"eventType\":\"thread.token-usage.updated\",\"payload\":{\"usage\":{\"usedTokens\":180,\"lastUsedTokens\":180,\"lastInputTokens\":120,\"lastCachedInputTokens\":40,\"lastOutputTokens\":60,\"lastReasoningOutputTokens\":20,\"durationMs\":2500}},\"occurredAt\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}" >/dev/null
turn_completed_event_id="$(new_uuid)"
worker_json "$worker_token" "turn-completed-first-$run_id" POST "/v1/workers/executions/$first_execution_id/events" \
  "{\"tenantId\":\"$tenant_id\",\"generation\":$first_generation,\"leaseToken\":\"$first_lease_token\",\"eventId\":\"$turn_completed_event_id\",\"eventVersion\":2,\"eventType\":\"turn.completed\",\"payload\":{\"state\":\"completed\",\"totalCostUsd\":0.012345},\"occurredAt\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}" >/dev/null
worker_json "$worker_token" "usage-first-$run_id" POST "/v1/workers/executions/$first_execution_id/usage" \
  "{\"tenantId\":\"$tenant_id\",\"generation\":$first_generation,\"leaseToken\":\"$first_lease_token\",\"reportSequence\":1,\"networkIngressBytes\":4096,\"networkEgressBytes\":2048}" >/dev/null
worker_json "$worker_token" "renew-first-$run_id" POST "/v1/workers/executions/$first_execution_id/renew" \
  "{\"tenantId\":\"$tenant_id\",\"generation\":$first_generation,\"leaseToken\":\"$first_lease_token\",\"providerResumeCursor\":\"acceptance-resume-cursor\"}" >/dev/null
worker_json "$worker_token" "release-first-$run_id" POST "/v1/workers/executions/$first_execution_id/release" \
  "{\"tenantId\":\"$tenant_id\",\"generation\":$first_generation,\"leaseToken\":\"$first_lease_token\",\"reason\":\"acceptance recovery\"}" >/dev/null

recovery_claim="$(worker_json "$worker_token" "claim-recovery-$run_id" POST /v1/workers/executions/claim \
  "{\"executionTargetId\":\"$execution_target_id\",\"targetKind\":\"$target_kind\"}")"
recovery_execution_id="$(jq -er '.execution.id' <<<"$recovery_claim")"
recovery_generation="$(jq -er '.lease.generation' <<<"$recovery_claim")"
recovery_lease_token="$(jq -er '.lease.leaseToken' <<<"$recovery_claim")"
jq -e --arg execution_id "$first_execution_id" \
  '.execution.id == $execution_id and .lease.generation == 2 and .providerResumeCursor == "acceptance-resume-cursor"' \
  <<<"$recovery_claim" >/dev/null
worker_json "$worker_token" "workspace-ready-recovery-$run_id" POST \
  "/v1/workers/executions/$recovery_execution_id/workspace/ready" \
  "{\"tenantId\":\"$tenant_id\",\"generation\":$recovery_generation,\"leaseToken\":\"$recovery_lease_token\"}" >/dev/null
worker_json "$worker_token" "complete-first-$run_id" POST "/v1/workers/executions/$recovery_execution_id/complete" \
  "{\"tenantId\":\"$tenant_id\",\"generation\":$recovery_generation,\"leaseToken\":\"$recovery_lease_token\",\"output\":{\"summary\":\"done\"}}" >/dev/null

curl -sS -N --max-time 1 -b "$owner_cookie" -H 'Last-Event-ID: 2' \
  "$base_url/v1/sessions/$session_id/events/stream" >"$work_dir/reconnected-events.sse" 2>/dev/null || true
grep -q '^id: 3$' "$work_dir/reconnected-events.sse"
grep -q '^id: 11$' "$work_dir/reconnected-events.sse"
if grep -q '^id: 2$' "$work_dir/reconnected-events.sse"; then
  printf 'SSE reconnect replayed an already acknowledged event\n' >&2
  exit 1
fi

model_switch_key="model-switch-$run_id"
model_switch_body='{"model":"gpt-5.4","expectedModel":"gpt-5.6-sol"}'
switched_session="$(request_json "$owner_cookie" POST "/v1/sessions/$session_id/model-switch" \
  "$model_switch_body" "$model_switch_key")"
jq -e '.model == "gpt-5.4" and .lastEventSequence == 12' <<<"$switched_session" >/dev/null
curl -sS -D "$work_dir/model-switch-replay.headers" -o "$work_dir/model-switch-replay.json" \
  -b "$owner_cookie" -c "$owner_cookie" -X POST -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $model_switch_key" -d "$model_switch_body" \
  "$base_url/v1/sessions/$session_id/model-switch"
tr -d '\r' <"$work_dir/model-switch-replay.headers" | grep -qi '^Idempotency-Replayed: true$'
jq -e '.model == "gpt-5.4" and .lastEventSequence == 12' \
  "$work_dir/model-switch-replay.json" >/dev/null

# A Session may only have one nonterminal Execution. The first Turn has completed,
# so this independent Turn is now legal and can be claimed separately.
request_json "$owner_cookie" POST "/v1/sessions/$session_id/turns" \
  '{"inputText":"Second acceptance turn"}' >/dev/null

second_claim="$(worker_json "$worker_token" "claim-second-$run_id" POST /v1/workers/executions/claim \
  "{\"executionTargetId\":\"$execution_target_id\",\"targetKind\":\"$target_kind\"}")"
second_execution_id="$(jq -er '.execution.id' <<<"$second_claim")"
second_generation="$(jq -er '.lease.generation' <<<"$second_claim")"
second_lease_token="$(jq -er '.lease.leaseToken' <<<"$second_claim")"
jq -e --arg session_id "$session_id" --arg previous_binding "$first_runtime_binding_id" '
  .execution.sessionId == $session_id and
  .execution.status == "leased" and
  .workload.model == "gpt-5.4" and
  .workload.resumeSnapshot.model == "gpt-5.4" and
  .workload.providerRuntimeBindingId != $previous_binding and
  .providerResumeCursor == null and
  (.workload.conversationHistory | any(.role == "assistant" and .text == "acceptance output"))
' <<<"$second_claim" >/dev/null
worker_json "$worker_token" "fail-second-$run_id" POST "/v1/workers/executions/$second_execution_id/fail" \
  "{\"tenantId\":\"$tenant_id\",\"generation\":$second_generation,\"leaseToken\":\"$second_lease_token\",\"failureCode\":\"acceptance_failure\",\"failureMessage\":\"Expected acceptance failure\"}" >/dev/null

runtime_events="$(request_json "$owner_cookie" GET "/v1/sessions/$session_id/events?afterSequence=2&limit=20")"
jq -e '.lastSequence == 15 and
  (.items | map(.sequence) == [3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15]) and
  (.items | map(.eventType) == ["execution.leased", "execution.started", "runtime.output.delta", "thread.token-usage.updated", "turn.completed", "execution.recovering", "execution.leased", "workspace.ready", "execution.completed", "session.model.changed", "turn.created", "execution.leased", "execution.failed"])' \
  <<<"$runtime_events" >/dev/null

session_usage="$(request_json "$owner_cookie" GET "/v1/sessions/$session_id/usage")"
jq -e --arg execution_id "$first_execution_id" '
  .items | any(
    .executionId == $execution_id and .generation == 1 and
    .inputTokens == 120 and .cachedInputTokens == 40 and .outputTokens == 60 and
    .reasoningTokens == 20 and .totalTokens == 180 and .durationMillis == 2500 and
    .networkIngressBytes == 4096 and .networkEgressBytes == 2048 and
    .providerCostMicros == 12345 and .providerCostReported == true and
    .providerCurrency == "USD" and .costCoverage != "provider-unavailable" and .final == true
  )
' <<<"$session_usage" >/dev/null
tenant_usage="$(request_json "$owner_cookie" GET "/v1/tenants/$tenant_id/usage")"
if ! jq -e '
  .usage.inputTokens == 120 and .usage.cachedInputTokens == 40 and
  .usage.outputTokens == 60 and .usage.reasoningTokens == 20 and .usage.totalTokens == 180 and
  .usage.networkIngressBytes == 4096 and .usage.networkEgressBytes == 2048 and
  .usage.providerCostByCurrency.USD == 12345 and .usage.providerCostReportedCount == 1 and
  .usage.providerCostMissingCount == 0 and .usage.knownCostByCurrency.USD >= 12345 and
  .softQuota.enforcement == "soft" and .softQuota.hardStop == false
' <<<"$tenant_usage" >/dev/null; then
  jq '{usage, softQuota}' <<<"$tenant_usage" >&2
  exit 1
fi

# Internal self-hosted commercialization is usage/cost governance, not payment.
# Exercise the project allocation CAS and the operator-facing CSV so a deployment
# cannot claim cost visibility while only exposing an unallocated usage summary.
cost_report="$(request_json "$owner_cookie" GET "/v1/tenants/$tenant_id/cost-accounting/report")"
jq -e --arg tenant_id "$tenant_id" --arg project_id "$project_id" '
  .tenantId == $tenant_id and
  .unallocatedProjectCount >= 1 and
  (.rows | any(
    .projectId == $project_id and
    .costCenterCode == "unallocated" and
    .departmentCode == "unallocated" and
    .totalTokens == 180 and
    .providerCostByCurrency.USD == 12345 and
    .knownCostByCurrency.USD >= 12345
  ))
' <<<"$cost_report" >/dev/null

cost_allocation="$(request_json "$owner_cookie" PUT "/v1/tenants/$tenant_id/cost-accounting/projects/$project_id" \
  '{"costCenterCode":"eng-platform","departmentCode":"internal-ai","expectedVersion":0}')"
jq -e --arg project_id "$project_id" '
  .projectId == $project_id and
  .costCenterCode == "eng-platform" and
  .departmentCode == "internal-ai" and
  .version == 1
' <<<"$cost_allocation" >/dev/null

cost_report="$(request_json "$owner_cookie" GET "/v1/tenants/$tenant_id/cost-accounting/report")"
jq -e --arg project_id "$project_id" '
  .unallocatedProjectCount == 0 and
  .knownCostByCurrency.USD >= 12345 and
  (.rows | any(
    .projectId == $project_id and
    .costCenterCode == "eng-platform" and
    .departmentCode == "internal-ai" and
    .totalTokens == 180 and
    .providerCostByCurrency.USD == 12345
  ))
' <<<"$cost_report" >/dev/null

cost_csv="$work_dir/internal-cost.csv"
curl -sS --fail -b "$owner_cookie" -H 'Accept: text/csv' \
  "$base_url/v1/tenants/$tenant_id/cost-accounting/export.csv" >"$cost_csv"
grep -q 'cost_center_code' "$cost_csv"
grep -q 'department_code' "$cost_csv"
if grep -Eiq 'checkout|stripe|payment' "$cost_csv"; then
  printf 'Internal cost CSV exposed payment semantics\n' >&2
  exit 1
fi

artifact_payload="$work_dir/acceptance-artifact.txt"
artifact_download="$work_dir/acceptance-artifact.downloaded.txt"
printf 'Synara Artifact acceptance %s\n' "$run_id" >"$artifact_payload"
if command -v sha256sum >/dev/null 2>&1; then
  artifact_sha256="$(sha256sum "$artifact_payload" | awk '{print $1}')"
else
  artifact_sha256="$(shasum -a 256 "$artifact_payload" | awk '{print $1}')"
fi
artifact_size="$(wc -c <"$artifact_payload" | tr -d ' ')"
artifact_grant="$(request_json "$owner_cookie" POST "/v1/sessions/$session_id/artifacts" \
  '{"kind":"attachment","originalName":"acceptance-artifact.txt"}')"
artifact_id="$(jq -er '.artifact.id' <<<"$artifact_grant")"
artifact_upload_url="$(jq -er '.url' <<<"$artifact_grant")"
curl -sS --fail -X PUT -H 'Content-Type: text/plain' --data-binary @"$artifact_payload" \
  "$artifact_upload_url" >/dev/null
forged_hash_status="$(curl -sS -o "$work_dir/forged-artifact-hash.json" -w '%{http_code}' \
  -b "$owner_cookie" -H 'Content-Type: application/json' \
  -d "{\"sizeBytes\":$artifact_size,\"sha256\":\"$(printf '0%.0s' {1..64})\",\"contentType\":\"text/plain\"}" \
  "$base_url/v1/artifacts/$artifact_id/complete")"
[[ "$forged_hash_status" == "409" ]]
jq -e '.error.code == "artifact_hash_mismatch"' "$work_dir/forged-artifact-hash.json" >/dev/null
artifact="$(request_json "$owner_cookie" POST "/v1/artifacts/$artifact_id/complete" \
  "{\"sizeBytes\":$artifact_size,\"sha256\":\"$artifact_sha256\",\"contentType\":\"text/plain\"}")"
jq -e --arg sha256 "$artifact_sha256" '.status == "ready" and .sha256 == $sha256' <<<"$artifact" >/dev/null
# A still-valid old presigned PUT can only recreate the isolated temporary key. Replaying Complete
# removes it again and must never change the verified final object.
printf 'tampered temporary payload\n' | curl -sS --fail -X PUT -H 'Content-Type: text/plain' --data-binary @- \
  "$artifact_upload_url" >/dev/null
artifact_replay="$(request_json "$owner_cookie" POST "/v1/artifacts/$artifact_id/complete" \
  "{\"sizeBytes\":$artifact_size,\"sha256\":\"$artifact_sha256\",\"contentType\":\"text/plain\"}")"
jq -e --arg sha256 "$artifact_sha256" '.status == "ready" and .sha256 == $sha256' <<<"$artifact_replay" >/dev/null
artifact_list="$(request_json "$owner_cookie" GET "/v1/sessions/$session_id/artifacts")"
jq -e --arg artifact_id "$artifact_id" '.items | any(.id == $artifact_id and .status == "ready")' \
  <<<"$artifact_list" >/dev/null
artifact_download_grant="$(request_json "$owner_cookie" POST "/v1/artifacts/$artifact_id/download")"
artifact_download_url="$(jq -er '.url' <<<"$artifact_download_grant")"
curl -sS --fail "$artifact_download_url" >"$artifact_download"
cmp "$artifact_payload" "$artifact_download"

outsider_session="$(request_json "$outsider_cookie" POST /v1/auth/dev-login \
  "{\"email\":\"outsider-$run_id@example.com\",\"displayName\":\"Acceptance Outsider\"}")"
outsider_id="$(jq -er '.user.userId' <<<"$outsider_session")"
cross_tenant_project_status="$(curl -sS -o "$work_dir/cross-tenant-project.json" -w '%{http_code}' \
  -b "$outsider_cookie" "$base_url/v1/projects/$project_id")"
[[ "$cross_tenant_project_status" == "404" ]]
cross_tenant_artifact_status="$(curl -sS -o "$work_dir/cross-tenant-artifact.json" -w '%{http_code}' \
  -b "$outsider_cookie" "$base_url/v1/artifacts/$artifact_id")"
[[ "$cross_tenant_artifact_status" == "404" ]]
cross_tenant_status="$(curl -sS -o "$work_dir/cross-tenant.json" -w '%{http_code}' \
  -b "$owner_cookie" -H 'Content-Type: application/json' \
  -d "{\"userId\":\"$outsider_id\",\"role\":\"member\",\"status\":\"active\"}" \
  "$base_url/v1/tenants/$tenant_id/organizations/$organization_id/members")"
[[ "$cross_tenant_status" == "409" ]]

invitation="$(request_json "$owner_cookie" POST "/v1/tenants/$tenant_id/invitations" \
  "{\"email\":\"member-$run_id@example.com\",\"role\":\"member\"}")"
invitation_token="$(jq -er '.token' <<<"$invitation")"
member_session="$(request_json "$member_cookie" POST /v1/auth/dev-login \
  "{\"email\":\"member-$run_id@example.com\",\"displayName\":\"Acceptance Member\"}")"
member_id="$(jq -er '.user.userId' <<<"$member_session")"
request_json "$member_cookie" POST "/v1/invitations/$invitation_token/accept" >/dev/null
request_json "$member_cookie" PUT /v1/auth/active-tenant \
  "{\"tenantId\":\"$tenant_id\"}" >/dev/null
request_json "$owner_cookie" POST "/v1/tenants/$tenant_id/organizations/$organization_id/members" \
  "{\"userId\":\"$member_id\",\"role\":\"member\",\"status\":\"active\"}" >/dev/null

member_projects="$(request_json "$member_cookie" GET "/v1/tenants/$tenant_id/organizations/$organization_id/projects")"
jq -e --arg project_id "$project_id" '.items | any(.id == $project_id)' <<<"$member_projects" >/dev/null

duplicate_invitation_status="$(curl -sS -o "$work_dir/duplicate-invitation.json" -w '%{http_code}' \
  -b "$owner_cookie" -H 'Content-Type: application/json' \
  -d "{\"email\":\"member-$run_id@example.com\",\"role\":\"admin\"}" \
  "$base_url/v1/tenants/$tenant_id/invitations")"
[[ "$duplicate_invitation_status" == "409" ]]

owner_demotion_status="$(curl -sS -o "$work_dir/owner-demotion.json" -w '%{http_code}' \
  -b "$owner_cookie" -X PATCH -H 'Content-Type: application/json' -d '{"role":"member"}' \
  "$base_url/v1/tenants/$tenant_id/members/$owner_id")"
[[ "$owner_demotion_status" == "409" ]]

members="$(request_json "$owner_cookie" GET "/v1/tenants/$tenant_id/members")"
jq -e --arg member_id "$member_id" '.items | any(.userId == $member_id and .status == "active")' \
  <<<"$members" >/dev/null

audit_logs="$(request_json "$owner_cookie" GET "/v1/tenants/$tenant_id/audit-logs?limit=20")"
jq -e '.items | length >= 4' <<<"$audit_logs" >/dev/null
jq -e '.items | all(.occurredAt | startswith("0001-") | not)' <<<"$audit_logs" >/dev/null

archived_session="$(request_json "$owner_cookie" POST "/v1/sessions/$session_id/archive")"
jq -e '.status == "archived" and .lastEventSequence == 17 and .archivedAt != null' \
  <<<"$archived_session" >/dev/null
request_json "$owner_cookie" DELETE "/v1/projects/$project_id" >/dev/null

outbox_ready=0
for _ in {1..50}; do
  published_outbox="$(request_json "$owner_cookie" GET "/v1/tenants/$tenant_id/outbox-messages?status=published&limit=50")"
  if jq -e '
    (.items | all(has("payload") | not)) and
    (.items | any(.topic == "execution.queued")) and
    (.items | any(.topic == "artifact.ready")) and
    (.items | any(.topic == "session.archived"))
  ' <<<"$published_outbox" >/dev/null; then
    outbox_ready=1
    break
  fi
  sleep 0.1
done
[[ "$outbox_ready" == "1" ]]

printf 'Self-hosted acceptance passed: tenant=%s organization=%s project=%s session=%s member=%s worker=%s\n' \
  "$tenant_id" "$organization_id" "$project_id" "$session_id" "$member_id" "$worker_id"
