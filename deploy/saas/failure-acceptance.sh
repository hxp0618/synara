#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
project="${SYNARA_FAILURE_ACCEPTANCE_PROJECT:-synara-stage2-failure-$$}"
work_dir="$(mktemp -d)"
workspace="$work_dir/workspace"
mkdir -p "$workspace"

pick_port() {
  python3 -c 'import socket; sock = socket.socket(); sock.bind(("127.0.0.1", 0)); print(sock.getsockname()[1]); sock.close()'
}

control_plane_port="${SYNARA_FAILURE_CONTROL_PLANE_PORT:-$(pick_port)}"
minio_port="${SYNARA_FAILURE_MINIO_PORT:-$(pick_port)}"
base_url="http://127.0.0.1:$control_plane_port"
worker_protocol_version=2

compose=(
  docker compose -p "$project"
  -f "$script_dir/docker-compose.yml"
  -f "$script_dir/control-plane-acceptance.override.yml"
)

cleanup() {
  if [[ "${SYNARA_FAILURE_KEEP_RESOURCES:-0}" != "1" ]]; then
    "${compose[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || true
    rm -rf "$work_dir"
  else
    printf 'Keeping failure-acceptance resources: project=%s work_dir=%s\n' "$project" "$work_dir" >&2
  fi
}
trap cleanup EXIT
trap 'status=$?; printf "Stage 2 failure acceptance stopped at line %s with status %s\n" "$LINENO" "$status" >&2; exit "$status"' ERR

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf '%s is required\n' "$1" >&2
    exit 1
  fi
}

require_command curl
require_command docker
require_command jq
require_command openssl
require_command python3

run_id="$(date +%s)-$$"
export POSTGRES_PASSWORD="stage2-postgres-$run_id-$(openssl rand -hex 8)"
export MINIO_ROOT_USER="stage2-minio-$run_id"
export MINIO_ROOT_PASSWORD="stage2-minio-secret-$run_id-$(openssl rand -hex 8)"
export SYNARA_WORKER_REGISTRATION_TOKEN="stage2-worker-registration-$run_id-$(openssl rand -hex 8)"
export SYNARA_PROVIDER_CURSOR_KEY="$(openssl rand -base64 32 | tr -d '\n')"
export SYNARA_CREDENTIAL_MASTER_KEY="$(openssl rand -base64 32 | tr -d '\n')"
export SYNARA_AUTH_TOKEN="stage2-synara-auth-$run_id-$(openssl rand -hex 8)"
export SYNARA_CONTROL_PLANE_DEV_BOOTSTRAP=true
export SYNARA_LOGIN_COOKIE_SECURE=false
export SYNARA_PUBLIC_CONTROL_PLANE_URL="$base_url"
export SYNARA_WORKER_LEASE_TTL=2s
export SYNARA_WORKER_HEARTBEAT_TIMEOUT=4s
export SYNARA_KUBERNETES_RECONCILE_INTERVAL=1s
export SYNARA_CONTROL_PLANE_HOST_PORT="$control_plane_port"
export MINIO_HOST_PORT="$minio_port"
export SYNARA_WORKSPACE_PATH="$workspace"

wait_service_health() {
  local service="$1"
  local container_id
  for _ in {1..120}; do
    container_id="$("${compose[@]}" ps -q "$service")"
    if [[ -n "$container_id" ]] &&
      [[ "$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$container_id")" == "healthy" ]]; then
      return 0
    fi
    sleep 1
  done
  printf '%s did not become healthy\n' "$service" >&2
  "${compose[@]}" ps >&2 || true
  return 1
}

wait_http_status() {
  local path="$1"
  local expected="$2"
  local attempts="${3:-60}"
  local status
  for ((attempt = 0; attempt < attempts; attempt += 1)); do
    status="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 3 "$base_url$path" 2>/dev/null || true)"
    if [[ "$status" == "$expected" ]]; then
      return 0
    fi
    sleep 1
  done
  printf '%s did not return HTTP %s (last status: %s)\n' "$path" "$expected" "${status:-none}" >&2
  return 1
}

request_json() {
  local cookie_jar="$1"
  local method="$2"
  local path="$3"
  local body="${4:-}"
  local args=(-sS --fail-with-body -b "$cookie_jar" -c "$cookie_jar" -X "$method")
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
  local args=(-sS --fail-with-body -X "$method" -H "Authorization: Bearer $token" -H "X-Request-ID: $request_id")
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

printf 'Starting isolated Stage 2 failure acceptance: project=%s\n' "$project"
"${compose[@]}" up -d --build postgres minio control-plane
wait_service_health postgres
wait_service_health minio
wait_service_health control-plane
wait_http_status /ready 200
printf 'Failure acceptance setup is ready\n'

owner_cookie="$work_dir/owner.cookie"
prompt_sentinel="stage2-prompt-sentinel-$run_id"
owner_session="$(request_json "$owner_cookie" POST /v1/auth/dev-login \
  "{\"email\":\"failure-$run_id@example.com\",\"displayName\":\"Failure Acceptance Owner\"}")"
tenant_id="$(jq -er '.user.activeTenantId' <<<"$owner_session")"
owner_id="$(jq -er '.user.userId' <<<"$owner_session")"
organization="$(request_json "$owner_cookie" POST "/v1/tenants/$tenant_id/organizations" \
  "{\"slug\":\"failure-$run_id\",\"name\":\"Failure Acceptance\",\"kind\":\"department\",\"settings\":{}}")"
organization_id="$(jq -er '.id' <<<"$organization")"
project_json="$(request_json "$owner_cookie" POST "/v1/tenants/$tenant_id/organizations/$organization_id/projects" \
  '{"name":"Failure Acceptance Project","defaultBranch":"main","visibility":"organization"}')"
project_id="$(jq -er '.id' <<<"$project_json")"
session_json="$(request_json "$owner_cookie" POST "/v1/projects/$project_id/sessions" \
  '{"title":"Failure Acceptance Session","visibility":"project","provider":"codex"}')"
session_id="$(jq -er '.id' <<<"$session_json")"
execution_target_id="$(jq -er '.executionTargetId' <<<"$session_json")"
execution_target="$(request_json "$owner_cookie" GET "/v1/tenants/$tenant_id/execution-targets/$execution_target_id")"
target_kind="$(jq -er '.kind' <<<"$execution_target")"
worker_capabilities="$(python3 "$repo_root/scripts/stage3-provider-acceptance/worker_manifest.py" \
  --target-capabilities-json "$(jq -c '.capabilities' <<<"$execution_target")")"

request_json "$owner_cookie" POST "/v1/sessions/$session_id/turns" \
  "{\"inputText\":\"$prompt_sentinel\"}" >/dev/null

worker_instance_uid="$(new_uuid)"
worker_registration="$(curl -sS --fail-with-body -X POST \
  -H "Authorization: Bearer $SYNARA_WORKER_REGISTRATION_TOKEN" \
  -H "X-Request-ID: register-$run_id" \
  -H 'Content-Type: application/json' \
  -d "{\"executionTargetId\":\"$execution_target_id\",\"targetKind\":\"$target_kind\",\"instanceUid\":\"$worker_instance_uid\",\"clusterId\":\"failure\",\"namespace\":\"default\",\"podName\":\"worker-$run_id\",\"version\":\"acceptance\",\"protocolVersion\":$worker_protocol_version,\"capabilities\":$worker_capabilities,\"leaseSupported\":true,\"fencingSupported\":true}" \
  "$base_url/v1/workers/register")"
worker_token="$(jq -er '.token' <<<"$worker_registration")"
worker_id="$(jq -er '.worker.id' <<<"$worker_registration")"
worker_json "$worker_token" "heartbeat-$run_id" POST /v1/workers/heartbeat \
  "{\"version\":\"acceptance\",\"protocolVersion\":$worker_protocol_version,\"capabilities\":$worker_capabilities}" >/dev/null

first_claim="$(worker_json "$worker_token" "claim-$run_id" POST /v1/workers/executions/claim \
  "{\"executionTargetId\":\"$execution_target_id\",\"targetKind\":\"$target_kind\"}")"
execution_id="$(jq -er '.execution.id' <<<"$first_claim")"
first_generation="$(jq -er '.lease.generation' <<<"$first_claim")"
first_lease_token="$(jq -er '.lease.leaseToken' <<<"$first_claim")"
worker_json "$worker_token" "start-$run_id" POST "/v1/workers/executions/$execution_id/start" \
  "{\"tenantId\":\"$tenant_id\",\"generation\":$first_generation,\"leaseToken\":\"$first_lease_token\"}" >/dev/null

sleep 5
stale_claim_status="$(curl -sS -o "$work_dir/stale-worker.json" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $worker_token" -H "X-Request-ID: stale-claim-$run_id" \
  -H 'Content-Type: application/json' \
  -d "{\"executionTargetId\":\"$execution_target_id\",\"targetKind\":\"$target_kind\"}" \
  "$base_url/v1/workers/executions/claim")"
[[ "$stale_claim_status" == "409" ]]
jq -e '.error.code == "worker_not_claimable" or .error.code == "worker_heartbeat_stale"' \
  "$work_dir/stale-worker.json" >/dev/null

replacement_instance_uid="$(new_uuid)"
replacement_registration="$(curl -sS --fail-with-body -X POST \
  -H "Authorization: Bearer $SYNARA_WORKER_REGISTRATION_TOKEN" \
  -H "X-Request-ID: replacement-register-$run_id" \
  -H 'Content-Type: application/json' \
  -d "{\"executionTargetId\":\"$execution_target_id\",\"targetKind\":\"$target_kind\",\"instanceUid\":\"$replacement_instance_uid\",\"clusterId\":\"failure\",\"namespace\":\"default\",\"podName\":\"replacement-$run_id\",\"version\":\"acceptance\",\"protocolVersion\":$worker_protocol_version,\"capabilities\":$worker_capabilities,\"leaseSupported\":true,\"fencingSupported\":true}" \
  "$base_url/v1/workers/register")"
replacement_token="$(jq -er '.token' <<<"$replacement_registration")"
replacement_worker_id="$(jq -er '.worker.id' <<<"$replacement_registration")"
replacement_claim="$(worker_json "$replacement_token" "replacement-claim-$run_id" POST /v1/workers/executions/claim \
  "{\"executionTargetId\":\"$execution_target_id\",\"targetKind\":\"$target_kind\"}")"
replacement_execution_id="$(jq -er '.execution.id' <<<"$replacement_claim")"
replacement_generation="$(jq -er '.lease.generation' <<<"$replacement_claim")"
replacement_lease_token="$(jq -er '.lease.leaseToken' <<<"$replacement_claim")"
jq -e --arg execution_id "$execution_id" \
  '.execution.id == $execution_id and .execution.status == "leased" and .lease.generation == 2' \
  <<<"$replacement_claim" >/dev/null

old_event_id="$(new_uuid)"
old_generation_status="$(curl -sS -o "$work_dir/old-generation.json" -w '%{http_code}' -X POST \
  -H "Authorization: Bearer $worker_token" -H "X-Request-ID: old-generation-$run_id" \
  -H 'Content-Type: application/json' \
  -d "{\"tenantId\":\"$tenant_id\",\"generation\":$first_generation,\"leaseToken\":\"$first_lease_token\",\"eventId\":\"$old_event_id\",\"eventVersion\":1,\"eventType\":\"runtime.output.delta\",\"payload\":{\"text\":\"stale output\"}}" \
  "$base_url/v1/workers/executions/$execution_id/events")"
[[ "$old_generation_status" == "409" ]]
jq -e '.error.code == "generation_fenced" or .error.code == "lease_not_current" or .error.code == "lease_expired"' \
  "$work_dir/old-generation.json" >/dev/null

worker_json "$replacement_token" "replacement-workspace-ready-$run_id" POST \
  "/v1/workers/executions/$replacement_execution_id/workspace/ready" \
  "{\"tenantId\":\"$tenant_id\",\"generation\":$replacement_generation,\"leaseToken\":\"$replacement_lease_token\"}" >/dev/null
worker_json "$replacement_token" "replacement-complete-$run_id" POST \
  "/v1/workers/executions/$replacement_execution_id/complete" \
  "{\"tenantId\":\"$tenant_id\",\"generation\":$replacement_generation,\"leaseToken\":\"$replacement_lease_token\",\"output\":{\"summary\":\"recovered\"}}" >/dev/null

events="$(request_json "$owner_cookie" GET "/v1/sessions/$session_id/events?afterSequence=1&limit=50")"
jq -e '.items | any(.eventType == "execution.recovering")' <<<"$events" >/dev/null
worker_offline_outbox_count=0
for _ in {1..50}; do
  worker_offline_outbox_count="$("${compose[@]}" exec -T postgres \
    psql -U "${POSTGRES_USER:-synara}" -d "${POSTGRES_DB:-synara}" -Atc \
    "SELECT count(*) FROM outbox_messages WHERE topic = 'worker.offline'" | tr -d '[:space:]')"
  if [[ "$worker_offline_outbox_count" -ge 1 ]]; then
    break
  fi
  sleep 0.1
done
[[ "$worker_offline_outbox_count" -ge 1 ]]
printf 'Worker-offline recovery passed\n'

artifact_payload="$work_dir/failure-artifact.txt"
printf 'Stage 2 failure recovery artifact %s\n' "$run_id" >"$artifact_payload"
artifact_size="$(wc -c <"$artifact_payload" | tr -d ' ')"
artifact_sha256="$(shasum -a 256 "$artifact_payload" | awk '{print $1}')"
artifact_grant="$(request_json "$owner_cookie" POST "/v1/sessions/$session_id/artifacts" \
  '{"kind":"attachment","originalName":"failure-artifact.txt"}')"
artifact_id="$(jq -er '.artifact.id' <<<"$artifact_grant")"
artifact_upload_url="$(jq -er '.url' <<<"$artifact_grant")"

"${compose[@]}" stop minio >/dev/null
if curl -sS --fail --max-time 3 -X PUT -H 'Content-Type: text/plain' \
  --data-binary @"$artifact_payload" "$artifact_upload_url" >/dev/null 2>&1; then
  printf 'Artifact upload unexpectedly succeeded while MinIO was stopped\n' >&2
  exit 1
fi
wait_http_status /ready 503 20
minio_outage_health_status="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 3 "$base_url/health")"
[[ "$minio_outage_health_status" == "200" ]]
recovered_session="$(request_json "$owner_cookie" GET /v1/auth/session)"
jq -e --arg owner_id "$owner_id" '.user.userId == $owner_id' <<<"$recovered_session" >/dev/null

"${compose[@]}" start minio >/dev/null
wait_service_health minio
wait_http_status /ready 200 30
curl -sS --fail -X PUT -H 'Content-Type: text/plain' --data-binary @"$artifact_payload" \
  "$artifact_upload_url" >/dev/null
artifact="$(request_json "$owner_cookie" POST "/v1/artifacts/$artifact_id/complete" \
  "{\"sizeBytes\":$artifact_size,\"sha256\":\"$artifact_sha256\",\"contentType\":\"text/plain\"}")"
jq -e --arg sha256 "$artifact_sha256" '.status == "ready" and .sha256 == $sha256' <<<"$artifact" >/dev/null
printf 'MinIO outage and recovery passed\n'

"${compose[@]}" stop postgres >/dev/null
wait_http_status /ready 503 40
health_status="$(curl -sS -o /dev/null -w '%{http_code}' --max-time 3 "$base_url/health")"
[[ "$health_status" == "200" ]]

"${compose[@]}" start postgres >/dev/null
wait_service_health postgres
wait_http_status /ready 200 60
session_after_database_recovery="$(request_json "$owner_cookie" GET /v1/auth/session)"
jq -e --arg owner_id "$owner_id" --arg tenant_id "$tenant_id" \
  '.user.userId == $owner_id and .user.activeTenantId == $tenant_id' \
  <<<"$session_after_database_recovery" >/dev/null
worker_json "$replacement_token" "post-db-heartbeat-$run_id" POST /v1/workers/heartbeat \
  "{\"version\":\"acceptance\",\"protocolVersion\":$worker_protocol_version,\"capabilities\":$worker_capabilities}" >/dev/null
printf 'PostgreSQL outage and recovery passed\n'

logs_file="$work_dir/control-plane.log"
"${compose[@]}" logs --no-color control-plane >"$logs_file"
sensitive_values=(
  "$POSTGRES_PASSWORD"
  "$MINIO_ROOT_PASSWORD"
  "$SYNARA_WORKER_REGISTRATION_TOKEN"
  "$SYNARA_PROVIDER_CURSOR_KEY"
  "$SYNARA_CREDENTIAL_MASTER_KEY"
  "$worker_token"
  "$first_lease_token"
  "$replacement_token"
  "$replacement_lease_token"
  "$prompt_sentinel"
)
for sensitive_value in "${sensitive_values[@]}"; do
  if grep -Fq -- "$sensitive_value" "$logs_file"; then
    printf 'Sensitive value was found in Control Plane logs\n' >&2
    exit 1
  fi
done
if grep -Fq 'X-Amz-Signature=' "$logs_file"; then
  printf 'Presigned URL query was found in Control Plane logs\n' >&2
  exit 1
fi
printf 'Sensitive-log audit passed\n'

printf 'Stage 2 failure acceptance passed: tenant=%s session=%s workers=%s,%s\n' \
  "$tenant_id" "$session_id" "$worker_id" "$replacement_worker_id"
