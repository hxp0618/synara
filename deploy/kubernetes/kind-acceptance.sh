#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
kind_bin="${KIND_BIN:-kind}"
cluster_name="${SYNARA_KIND_CLUSTER:-synara-stage2-acceptance}"
node_image="${SYNARA_KIND_NODE_IMAGE:-kindest/node:v1.33.1}"
control_plane_image="${SYNARA_K8S_ACCEPTANCE_IMAGE:-synara-control-plane:stage2-acceptance}"
created_cluster=0
dependency_images=(
  postgres:17-alpine
  minio/minio:RELEASE.2025-04-22T22-12-26Z
  minio/mc:RELEASE.2025-04-16T18-13-26Z
)

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf '%s is required\n' "$1" >&2
    exit 1
  fi
}

require_command docker
require_command kubectl
if ! command -v "$kind_bin" >/dev/null 2>&1; then
  printf 'kind is required; set KIND_BIN to an explicit binary path if it is not on PATH\n' >&2
  exit 1
fi

cleanup() {
  if [[ "$created_cluster" == "1" && "${SYNARA_KIND_KEEP_CLUSTER:-0}" != "1" ]]; then
    "$kind_bin" delete cluster --name "$cluster_name" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

if "$kind_bin" get clusters | grep -Fxq "$cluster_name"; then
  if [[ "${SYNARA_KIND_REUSE_CLUSTER:-0}" != "1" ]]; then
    printf 'Kind cluster %s already exists; set SYNARA_KIND_REUSE_CLUSTER=1 to reuse it\n' "$cluster_name" >&2
    exit 1
  fi
else
  "$kind_bin" create cluster --name "$cluster_name" --image "$node_image" --wait 180s
  created_cluster=1
fi

docker build -t "$control_plane_image" "$repo_root/services/control-plane"
for dependency_image in "${dependency_images[@]}"; do
  if ! docker image inspect "$dependency_image" >/dev/null 2>&1; then
    docker pull "$dependency_image"
  fi
done
"$kind_bin" load docker-image --name "$cluster_name" \
  "$control_plane_image" "${dependency_images[@]}"

SYNARA_K8S_CONTEXT="kind-$cluster_name" \
SYNARA_K8S_ACCEPTANCE_IMAGE="$control_plane_image" \
  "$script_dir/acceptance.sh"

printf 'Kind Stage 2 acceptance passed: cluster=%s image=%s\n' "$cluster_name" "$control_plane_image"
