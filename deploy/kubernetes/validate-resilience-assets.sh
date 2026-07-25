#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

bash -n \
  "$script_dir/acceptance.sh" \
  "$script_dir/kind-acceptance.sh" \
  "$script_dir/kind-resilience-acceptance.sh" \
  "$script_dir/resilience-acceptance.sh" \
  "$script_dir/validate-resilience-assets.sh"

kubectl kustomize "$script_dir" >/dev/null
python3 "$script_dir/validate-resilience-assets.py"

printf 'Kubernetes resilience assets validation passed\n'
