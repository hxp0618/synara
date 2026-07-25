#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

export SYNARA_KIND_CLUSTER="${SYNARA_KIND_CLUSTER:-synara-stage2-resilience}"
export SYNARA_KIND_CLUSTER_CONFIG="${SYNARA_KIND_CLUSTER_CONFIG:-$script_dir/kind-multinode.yaml}"
export SYNARA_K8S_ACCEPTANCE_SCRIPT="${SYNARA_K8S_ACCEPTANCE_SCRIPT:-$script_dir/resilience-acceptance.sh}"
export SYNARA_K8S_ACCEPTANCE_DEPENDENCY_NODE_SELECTOR_KEY="${SYNARA_K8S_ACCEPTANCE_DEPENDENCY_NODE_SELECTOR_KEY:-synara.io/resilience-dependency}"
export SYNARA_K8S_ACCEPTANCE_DEPENDENCY_NODE_SELECTOR_VALUE="${SYNARA_K8S_ACCEPTANCE_DEPENDENCY_NODE_SELECTOR_VALUE:-true}"

bash "$script_dir/kind-acceptance.sh" "$@"
