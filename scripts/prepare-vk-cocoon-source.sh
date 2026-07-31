#!/usr/bin/env bash
set -euo pipefail

readonly upstream_url="https://github.com/cocoonstack/vk-cocoon.git"
readonly upstream_commit="de972e73a711b6e147a2042b8f1a92e58356b3bc"
readonly expected_tree="2872e3b14f3cf516940546219e9dffb388d9fd0a"
readonly script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly repository_root="$(cd -- "${script_dir}/.." && pwd)"
readonly patch_dir="${repository_root}/deploy/kubernetes/vk-cocoon/patches"

if [[ $# -ne 1 || -z "$1" ]]; then
  echo "usage: $0 /absolute/output/path" >&2
  exit 64
fi

readonly output_dir="$1"
if [[ "${output_dir}" != /* ]]; then
  echo "output path must be absolute: ${output_dir}" >&2
  exit 64
fi
if [[ -e "${output_dir}" ]]; then
  echo "output path already exists: ${output_dir}" >&2
  exit 73
fi

git clone --no-checkout "${upstream_url}" "${output_dir}"
git -C "${output_dir}" checkout --detach "${upstream_commit}"
git -C "${output_dir}" am "${patch_dir}"/*.patch

readonly actual_tree="$(git -C "${output_dir}" rev-parse 'HEAD^{tree}')"
if [[ "${actual_tree}" != "${expected_tree}" ]]; then
  echo "patched vk-cocoon tree mismatch: expected=${expected_tree} actual=${actual_tree}" >&2
  exit 65
fi

git -C "${output_dir}" diff --check "${upstream_commit}..HEAD"
go -C "${output_dir}" test ./...
