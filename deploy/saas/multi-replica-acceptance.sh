#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
project="${SYNARA_MULTI_REPLICA_PROJECT:-synara-stage2-multi-$$}"
work_dir="$(mktemp -d)"
workspace="$work_dir/workspace"
mkdir -p "$workspace"

compose=(docker compose -p "$project" -f "$script_dir/docker-compose.yml")
cleanup() {
  "${compose[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$work_dir"
}
trap cleanup EXIT

export POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-multi-replica-postgres-password}"
export MINIO_ROOT_USER="${MINIO_ROOT_USER:-multi-replica-minio}"
export MINIO_ROOT_PASSWORD="${MINIO_ROOT_PASSWORD:-multi-replica-minio-password}"
export SYNARA_WORKER_REGISTRATION_TOKEN="${SYNARA_WORKER_REGISTRATION_TOKEN:-multi-replica-worker-registration-token}"
export SYNARA_PROVIDER_CURSOR_KEY="${SYNARA_PROVIDER_CURSOR_KEY:-MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=}"
export SYNARA_CREDENTIAL_MASTER_KEY="${SYNARA_CREDENTIAL_MASTER_KEY:-YWJjZGVmZ2hpamtsbW5vcHFyc3R1dnd4eXpBQkNERUY=}"
export SYNARA_AUTH_TOKEN="${SYNARA_AUTH_TOKEN:-multi-replica-synara-auth-token}"
export SYNARA_CONTROL_PLANE_DEV_BOOTSTRAP=true
export SYNARA_LOGIN_COOKIE_SECURE=false
export SYNARA_WORKSPACE_PATH="$workspace"
export SYNARA_HOST_PORT="${SYNARA_MULTI_REPLICA_WEB_PORT:-59890}"
export MINIO_HOST_PORT="${SYNARA_MULTI_REPLICA_MINIO_PORT:-59092}"

"${compose[@]}" up -d --build --scale control-plane=2

control_plane_ids=()
for _ in {1..60}; do
  control_plane_ids=()
  while IFS= read -r id; do
    [[ -n "$id" ]] && control_plane_ids+=("$id")
  done < <("${compose[@]}" ps -q control-plane)
  healthy=0
  for id in "${control_plane_ids[@]}"; do
    if [[ "$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{end}}' "$id")" == "healthy" ]]; then
      healthy=$((healthy + 1))
    fi
  done
  if [[ "${#control_plane_ids[@]}" == "2" && "$healthy" == "2" ]]; then
    break
  fi
  sleep 1
done
if [[ "${#control_plane_ids[@]}" != "2" ]]; then
  printf 'Expected two control-plane replicas, found %s\n' "${#control_plane_ids[@]}" >&2
  exit 1
fi
for id in "${control_plane_ids[@]}"; do
  if [[ "$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{end}}' "$id")" != "healthy" ]]; then
    printf 'Control-plane replica %s is not healthy\n' "$id" >&2
    exit 1
  fi
done

synara_id="$("${compose[@]}" ps -q synara)"
if [[ -z "$synara_id" ]]; then
  printf 'Synara test runner container is unavailable\n' >&2
  exit 1
fi
replica_a="$(docker inspect --format '{{.Name}}' "${control_plane_ids[0]}" | sed 's#^/##')"
replica_b="$(docker inspect --format '{{.Name}}' "${control_plane_ids[1]}" | sed 's#^/##')"

docker exec -i "$synara_id" bash -s -- "$replica_a" "$replica_b" "$SYNARA_WORKER_REGISTRATION_TOKEN" <<'REPLICA_PHASE_ONE'
set -euo pipefail
replica_a="$1"
replica_b="$2"
registration_token="$3"
export NO_PROXY='*' no_proxy='*'
logical_host=synara-control-plane.test
replica_a_ip="$(getent ahostsv4 "$replica_a" | awk 'NR == 1 { print $1 }')"
replica_b_ip="$(getent ahostsv4 "$replica_b" | awk 'NR == 1 { print $1 }')"
state_dir=/tmp/synara-multi-replica-acceptance
rm -rf "$state_dir"
mkdir -p "$state_dir"
cookie="$state_dir/owner.cookie"

request_json() {
  local address="$1"
  local method="$2"
  local path="$3"
  local body="${4:-}"
  local args=(-sS --fail-with-body -b "$cookie" -c "$cookie" -X "$method")
  if [[ -n "$body" ]]; then
    args+=(-H 'Content-Type: application/json' -d "$body")
  fi
  curl --resolve "$logical_host:3780:$address" "${args[@]}" "http://$logical_host:3780$path"
}

for replica in "$replica_a" "$replica_b"; do
  ready="$(curl -sS --fail-with-body "http://$replica:3780/ready")"
  jq -e '.status == "ready" and .checks.schema.status == "ready" and .checks.schema.expectedVersion >= 11 and .checks.schema.appliedVersion >= .checks.schema.expectedVersion' <<<"$ready" >/dev/null
done

run_id="$(date +%s)-$$"
revoke_cookie="$state_dir/revoke.cookie"
curl --resolve "$logical_host:3780:$replica_a_ip" -sS --fail-with-body -c "$revoke_cookie" -X POST -H 'Content-Type: application/json' \
  -d "{\"email\":\"revoke-$run_id@example.com\",\"displayName\":\"Revoke Test\"}" \
  "http://$logical_host:3780/v1/auth/dev-login" >/dev/null
curl --resolve "$logical_host:3780:$replica_b_ip" -sS --fail-with-body -b "$revoke_cookie" -X POST \
  "http://$logical_host:3780/v1/auth/logout" >/dev/null
revoked_status="$(curl --resolve "$logical_host:3780:$replica_a_ip" -sS -o "$state_dir/revoked-session.json" -w '%{http_code}' -b "$revoke_cookie" \
  "http://$logical_host:3780/v1/auth/session")"
[[ "$revoked_status" == "401" ]]

login="$(request_json "$replica_a_ip" POST /v1/auth/dev-login "{\"email\":\"multi-$run_id@example.com\",\"displayName\":\"Multi Replica Owner\"}")"
tenant_id="$(jq -er '.user.activeTenantId' <<<"$login")"
organization="$(request_json "$replica_a_ip" POST "/v1/tenants/$tenant_id/organizations" "{\"slug\":\"multi-$run_id\",\"name\":\"Multi Replica\",\"kind\":\"department\",\"settings\":{}}")"
organization_id="$(jq -er '.id' <<<"$organization")"
project="$(request_json "$replica_a_ip" POST "/v1/tenants/$tenant_id/organizations/$organization_id/projects" '{"name":"Multi Replica Project","defaultBranch":"main","visibility":"organization"}')"
project_id="$(jq -er '.id' <<<"$project")"
session="$(request_json "$replica_a_ip" POST "/v1/projects/$project_id/sessions" '{"title":"Multi Replica Session","visibility":"project","provider":"codex"}')"
session_id="$(jq -er '.id' <<<"$session")"
target_id="$(jq -er '.executionTargetId' <<<"$session")"
target="$(request_json "$replica_a_ip" GET "/v1/tenants/$tenant_id/execution-targets/$target_id")"
target_kind="$(jq -er '.kind' <<<"$target")"

curl --resolve "$logical_host:3780:$replica_a_ip" -sS -N --max-time 10 -b "$cookie" "http://$logical_host:3780/v1/sessions/$session_id/events/stream?afterSequence=1" >"$state_dir/cross-replica.sse" &
sse_pid="$!"
trap 'kill "$sse_pid" >/dev/null 2>&1 || true; wait "$sse_pid" >/dev/null 2>&1 || true' EXIT
for _ in {1..50}; do
  grep -q '^retry: 2000' "$state_dir/cross-replica.sse" && break
  sleep 0.1
done
grep -q '^retry: 2000' "$state_dir/cross-replica.sse"
request_json "$replica_b_ip" POST "/v1/sessions/$session_id/turns" '{"inputText":"written through replica B"}' >/dev/null
for _ in {1..50}; do
  grep -q '^id: 2$' "$state_dir/cross-replica.sse" && break
  sleep 0.1
done
grep -q '^id: 2$' "$state_dir/cross-replica.sse"
kill "$sse_pid" >/dev/null 2>&1 || true
wait "$sse_pid" >/dev/null 2>&1 || true
trap - EXIT
events="$(request_json "$replica_a_ip" GET "/v1/sessions/$session_id/events?afterSequence=1&limit=10")"
execution_id="$(jq -er '.items[] | select(.sequence == 2) | .payload.executionId' <<<"$events")"

registration="$(curl --resolve "$logical_host:3780:$replica_a_ip" -sS --fail-with-body -X POST -H "Authorization: Bearer $registration_token" -H 'Content-Type: application/json' \
  -d "{\"executionTargetId\":\"$target_id\",\"targetKind\":\"$target_kind\",\"clusterId\":\"multi\",\"namespace\":\"default\",\"podName\":\"multi-worker-$run_id\",\"version\":\"acceptance\",\"capabilities\":{\"codex\":true},\"leaseSupported\":true,\"fencingSupported\":true}" \
  "http://$logical_host:3780/v1/workers/register")"
worker_token="$(jq -er '.token' <<<"$registration")"
claim_body="{\"executionTargetId\":\"$target_id\",\"targetKind\":\"$target_kind\",\"executionId\":\"$execution_id\"}"
curl --resolve "$logical_host:3780:$replica_a_ip" -sS --fail-with-body -X POST -H "Authorization: Bearer $worker_token" -H 'Content-Type: application/json' -H "X-Request-ID: claim-a-$run_id" -d "$claim_body" \
  "http://$logical_host:3780/v1/workers/executions/claim" >"$state_dir/claim-a.json" &
claim_a_pid="$!"
curl --resolve "$logical_host:3780:$replica_b_ip" -sS --fail-with-body -X POST -H "Authorization: Bearer $worker_token" -H 'Content-Type: application/json' -H "X-Request-ID: claim-b-$run_id" -d "$claim_body" \
  "http://$logical_host:3780/v1/workers/executions/claim" >"$state_dir/claim-b.json" &
claim_b_pid="$!"
wait "$claim_a_pid"
wait "$claim_b_pid"
claimed_count="$(jq -s '[.[] | .execution? | select(. != null)] | length' "$state_dir/claim-a.json" "$state_dir/claim-b.json")"
[[ "$claimed_count" == "1" ]]

curl --resolve "$logical_host:3780:$replica_a_ip" -sS --fail-with-body -b "$cookie" -X POST -H 'Content-Type: application/json' \
  -d '{"inputText":"concurrent turn through replica A"}' "http://$logical_host:3780/v1/sessions/$session_id/turns" >"$state_dir/turn-a.json" &
turn_a_pid="$!"
curl --resolve "$logical_host:3780:$replica_b_ip" -sS --fail-with-body -b "$cookie" -X POST -H 'Content-Type: application/json' \
  -d '{"inputText":"concurrent turn through replica B"}' "http://$logical_host:3780/v1/sessions/$session_id/turns" >"$state_dir/turn-b.json" &
turn_b_pid="$!"
wait "$turn_a_pid"
wait "$turn_b_pid"
turn_a_id="$(jq -er '.id' "$state_dir/turn-a.json")"
turn_b_id="$(jq -er '.id' "$state_dir/turn-b.json")"
[[ "$turn_a_id" != "$turn_b_id" ]]
events="$(request_json "$replica_b_ip" GET "/v1/sessions/$session_id/events?afterSequence=2&limit=10")"
jq -e '.items | map(.sequence) == [3, 4, 5]' <<<"$events" >/dev/null

printf '%s\n' "$session_id" >"$state_dir/session-id"
printf 'Cross-replica SSE and unique Claim passed: session=%s\n' "$session_id"
REPLICA_PHASE_ONE

docker stop "${control_plane_ids[0]}" >/dev/null

docker exec -i "$synara_id" bash -s -- "$replica_b" <<'REPLICA_PHASE_TWO'
set -euo pipefail
replica_b="$1"
export NO_PROXY='*' no_proxy='*'
logical_host=synara-control-plane.test
replica_b_ip="$(getent ahostsv4 "$replica_b" | awk 'NR == 1 { print $1 }')"
state_dir=/tmp/synara-multi-replica-acceptance
cookie="$state_dir/owner.cookie"
session_id="$(cat "$state_dir/session-id")"

ready="$(curl -sS --fail-with-body "http://$replica_b:3780/ready")"
jq -e '.status == "ready" and .checks.schema.status == "ready"' <<<"$ready" >/dev/null
curl --resolve "$logical_host:3780:$replica_b_ip" -sS -N --max-time 10 -b "$cookie" -H 'Last-Event-ID: 3' \
  "http://$logical_host:3780/v1/sessions/$session_id/events/stream" >"$state_dir/failover.sse" &
sse_pid="$!"
trap 'kill "$sse_pid" >/dev/null 2>&1 || true; wait "$sse_pid" >/dev/null 2>&1 || true' EXIT
for _ in {1..50}; do
  grep -q '^id: 5$' "$state_dir/failover.sse" && break
  sleep 0.1
done
grep -q '^id: 4$' "$state_dir/failover.sse"
grep -q '^id: 5$' "$state_dir/failover.sse"
curl --resolve "$logical_host:3780:$replica_b_ip" -sS --fail-with-body -b "$cookie" -c "$cookie" -X POST -H 'Content-Type: application/json' \
  -d '{"inputText":"continued after replica A stopped"}' \
  "http://$logical_host:3780/v1/sessions/$session_id/turns" >/dev/null
for _ in {1..50}; do
  grep -q '^id: 6$' "$state_dir/failover.sse" && break
  sleep 0.1
done
grep -q '^id: 6$' "$state_dir/failover.sse"
if grep -q '^id: 3$' "$state_dir/failover.sse"; then
  printf 'Failover SSE replayed the acknowledged event 3\n' >&2
  exit 1
fi
printf 'Replica failover and Last-Event-ID catch-up passed: session=%s\n' "$session_id"
REPLICA_PHASE_TWO

printf 'Multi-replica SaaS acceptance passed: project=%s replicas=%s,%s\n' "$project" "$replica_a" "$replica_b"
