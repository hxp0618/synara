#!/usr/bin/env bash
set -euo pipefail

base_url="${1:-http://127.0.0.1:3773}"
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

request_json() {
  local cookie_jar="$1"
  local method="$2"
  local path="$3"
  local body="${4:-}"
  local args=(-sS -b "$cookie_jar" -c "$cookie_jar" -X "$method")
  if [[ -n "$body" ]]; then
    args+=(-H 'Content-Type: application/json' -d "$body")
  fi
  curl "${args[@]}" "$base_url$path"
}

worker_json() {
  local token="$1"
  local request_id="$2"
  local method="$3"
  local path="$4"
  local body="${5:-}"
  local args=(-sS -X "$method" -H "Authorization: Bearer $token" -H "X-Request-ID: $request_id")
  if [[ -n "$body" ]]; then
    args+=(-H 'Content-Type: application/json' -d "$body")
  fi
  curl "${args[@]}" "$base_url$path"
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

owner_session="$(request_json "$owner_cookie" POST /v1/auth/dev-login \
  "{\"email\":\"owner-$run_id@example.com\",\"displayName\":\"Acceptance Owner\"}")"
tenant_id="$(jq -er '.user.activeTenantId' <<<"$owner_session")"
owner_id="$(jq -er '.user.userId' <<<"$owner_session")"

organization="$(request_json "$owner_cookie" POST "/v1/tenants/$tenant_id/organizations" \
  "{\"slug\":\"acceptance-$run_id\",\"name\":\"Acceptance Engineering\",\"kind\":\"department\",\"settings\":{}}")"
organization_id="$(jq -er '.id' <<<"$organization")"

project="$(request_json "$owner_cookie" POST "/v1/tenants/$tenant_id/organizations/$organization_id/projects" \
  "{\"name\":\"Acceptance Project\",\"repositoryUrl\":\"https://example.com/synara.git\",\"defaultBranch\":\"main\",\"visibility\":\"organization\"}")"
project_id="$(jq -er '.id' <<<"$project")"

agent_session="$(request_json "$owner_cookie" POST "/v1/projects/$project_id/sessions" \
  '{"title":"Acceptance Session","visibility":"project","provider":"codex","model":"gpt-5.6-sol"}')"
session_id="$(jq -er '.id' <<<"$agent_session")"
execution_target_id="$(jq -er '.executionTargetId' <<<"$agent_session")"
execution_target="$(request_json "$owner_cookie" GET "/v1/tenants/$tenant_id/execution-targets/$execution_target_id")"
target_kind="$(jq -er '.kind' <<<"$execution_target")"
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
request_json "$owner_cookie" POST "/v1/sessions/$session_id/turns" \
  '{"inputText":"Second acceptance turn"}' >/dev/null

for _ in {1..50}; do
  grep -q '^id: 3$' "$work_dir/events.sse" && break
  sleep 0.1
done
grep -q '^id: 2$' "$work_dir/events.sse"
grep -q '^id: 3$' "$work_dir/events.sse"
grep -q '^event: session-event$' "$work_dir/events.sse"
kill "$sse_pid" >/dev/null 2>&1 || true
wait "$sse_pid" >/dev/null 2>&1 || true
sse_pid=""

curl -sS -N --max-time 1 -b "$owner_cookie" -H 'Last-Event-ID: 2' \
  "$base_url/v1/sessions/$session_id/events/stream" >"$work_dir/reconnected-events.sse" 2>/dev/null || true
grep -q '^id: 3$' "$work_dir/reconnected-events.sse"
if grep -q '^id: 2$' "$work_dir/reconnected-events.sse"; then
  printf 'SSE reconnect replayed an already acknowledged event\n' >&2
  exit 1
fi

events="$(request_json "$owner_cookie" GET "/v1/sessions/$session_id/events?afterSequence=1&limit=10")"
jq -e '.lastSequence == 3 and (.items | map(.sequence) == [2, 3])' <<<"$events" >/dev/null
jq -e '.items | all(.tenantId != null and .organizationId != null and .projectId != null)' \
  <<<"$events" >/dev/null

worker_registration_token="${SYNARA_ACCEPTANCE_WORKER_REGISTRATION_TOKEN:-acceptance-worker-registration-token}"
worker_registration="$(worker_json "$worker_registration_token" "register-$run_id" POST /v1/workers/register \
  "{\"executionTargetId\":\"$execution_target_id\",\"targetKind\":\"$target_kind\",\"clusterId\":\"acceptance\",\"namespace\":\"default\",\"podName\":\"worker-$run_id\",\"version\":\"acceptance\",\"capabilities\":{\"codex\":true},\"leaseSupported\":true,\"fencingSupported\":true}")"
worker_token="$(jq -er '.token' <<<"$worker_registration")"
worker_id="$(jq -er '.worker.id' <<<"$worker_registration")"

worker_json "$worker_token" "heartbeat-$run_id" POST /v1/workers/heartbeat \
  '{"version":"acceptance","capabilities":{"codex":true}}' >/dev/null

first_claim="$(worker_json "$worker_token" "claim-first-$run_id" POST /v1/workers/executions/claim \
  "{\"executionTargetId\":\"$execution_target_id\",\"targetKind\":\"$target_kind\"}")"
first_execution_id="$(jq -er '.execution.id' <<<"$first_claim")"
first_generation="$(jq -er '.lease.generation' <<<"$first_claim")"
first_lease_token="$(jq -er '.lease.leaseToken' <<<"$first_claim")"
jq -e --arg session_id "$session_id" '.execution.sessionId == $session_id and .execution.status == "leased"' \
  <<<"$first_claim" >/dev/null

first_envelope="{\"tenantId\":\"$tenant_id\",\"generation\":$first_generation,\"leaseToken\":\"$first_lease_token\"}"
worker_json "$worker_token" "start-first-$run_id" POST "/v1/workers/executions/$first_execution_id/start" \
  "$first_envelope" >/dev/null
runtime_event_id="$(new_uuid)"
worker_json "$worker_token" "event-first-$run_id" POST "/v1/workers/executions/$first_execution_id/events" \
  "{\"tenantId\":\"$tenant_id\",\"generation\":$first_generation,\"leaseToken\":\"$first_lease_token\",\"eventId\":\"$runtime_event_id\",\"eventVersion\":1,\"eventType\":\"runtime.output.delta\",\"payload\":{\"text\":\"acceptance output\"},\"occurredAt\":\"$(date -u +%Y-%m-%dT%H:%M:%SZ)\"}" >/dev/null
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
worker_json "$worker_token" "complete-first-$run_id" POST "/v1/workers/executions/$recovery_execution_id/complete" \
  "{\"tenantId\":\"$tenant_id\",\"generation\":$recovery_generation,\"leaseToken\":\"$recovery_lease_token\",\"output\":{\"summary\":\"done\"}}" >/dev/null

second_claim="$(worker_json "$worker_token" "claim-second-$run_id" POST /v1/workers/executions/claim \
  "{\"executionTargetId\":\"$execution_target_id\",\"targetKind\":\"$target_kind\"}")"
second_execution_id="$(jq -er '.execution.id' <<<"$second_claim")"
second_generation="$(jq -er '.lease.generation' <<<"$second_claim")"
second_lease_token="$(jq -er '.lease.leaseToken' <<<"$second_claim")"
jq -e --arg session_id "$session_id" '.execution.sessionId == $session_id and .execution.status == "leased"' \
  <<<"$second_claim" >/dev/null
worker_json "$worker_token" "fail-second-$run_id" POST "/v1/workers/executions/$second_execution_id/fail" \
  "{\"tenantId\":\"$tenant_id\",\"generation\":$second_generation,\"leaseToken\":\"$second_lease_token\",\"failureCode\":\"acceptance_failure\",\"failureMessage\":\"Expected acceptance failure\"}" >/dev/null

runtime_events="$(request_json "$owner_cookie" GET "/v1/sessions/$session_id/events?afterSequence=3&limit=20")"
jq -e '.lastSequence == 11 and (.items | map(.eventType) == ["execution.leased", "execution.started", "runtime.output.delta", "execution.recovering", "execution.leased", "execution.completed", "execution.leased", "execution.failed"])' \
  <<<"$runtime_events" >/dev/null

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
artifact="$(request_json "$owner_cookie" POST "/v1/artifacts/$artifact_id/complete" \
  "{\"sizeBytes\":$artifact_size,\"sha256\":\"$artifact_sha256\",\"contentType\":\"text/plain\"}")"
jq -e --arg sha256 "$artifact_sha256" '.status == "ready" and .sha256 == $sha256' <<<"$artifact" >/dev/null
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
jq -e '.status == "archived" and .lastEventSequence == 13 and .archivedAt != null' \
  <<<"$archived_session" >/dev/null
request_json "$owner_cookie" DELETE "/v1/projects/$project_id" >/dev/null

printf 'SaaS acceptance passed: tenant=%s organization=%s project=%s session=%s member=%s worker=%s\n' \
  "$tenant_id" "$organization_id" "$project_id" "$session_id" "$member_id" "$worker_id"
