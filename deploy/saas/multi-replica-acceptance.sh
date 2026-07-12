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

"${compose[@]}" up -d --build --scale control-plane=2 postgres minio control-plane

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

network_id="$(docker network ls \
  --filter "label=com.docker.compose.project=$project" \
  --filter 'label=com.docker.compose.network=default' \
  --format '{{.ID}}' | head -n 1)"
if [[ -z "$network_id" ]]; then
  printf 'Compose test network is unavailable\n' >&2
  exit 1
fi
replica_a="$(docker inspect --format '{{.Name}}' "${control_plane_ids[0]}" | sed 's#^/##')"
replica_b="$(docker inspect --format '{{.Name}}' "${control_plane_ids[1]}" | sed 's#^/##')"
runner=(
  docker run --rm --network "$network_id"
  -v "$work_dir:/state"
  -v "$script_dir/multi-replica-acceptance.py:/runner.py:ro"
  python:3.13-alpine python /runner.py
)

"${runner[@]}" phase-one \
  --replica-a "$replica_a" \
  --replica-b "$replica_b" \
  --registration-token "$SYNARA_WORKER_REGISTRATION_TOKEN"

docker stop "${control_plane_ids[0]}" >/dev/null

"${runner[@]}" phase-two --replica-b "$replica_b"

printf 'Multi-replica SaaS acceptance passed: project=%s replicas=%s,%s\n' "$project" "$replica_a" "$replica_b"
