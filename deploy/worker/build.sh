#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
sbom_generator_lock="$repo_root/deploy/worker/buildkit-sbom-generator.lock"
target="worker"
image=""
version=""
git_sha=""
source_date_epoch=""
platforms=""
metadata_file=""
output_mode="load"
builder="${SYNARA_WORKER_BUILDER:-}"
docker_bin="docker"
go_proxy="${SYNARA_WORKER_GOPROXY:-}"
network_proxy="${SYNARA_WORKER_NETWORK_PROXY:-}"
allow_dirty=0
no_cache=0
label_values=()
label_count=0

usage() {
  cat <<'EOF'
Usage: deploy/worker/build.sh [options]

Options:
  --target worker|worker-acceptance
  --image NAME:TAG
  --version VERSION
  --git-sha FULL_SHA
  --source-date-epoch SECONDS
  --platform PLATFORM[,PLATFORM]
  --metadata-file PATH
  --builder NAME
  --docker-bin PATH          Optional docker executable or wrapper path.
  --go-proxy URLS            Optional public GOPROXY list; credentials are rejected.
  --network-proxy URL        Optional credential-free HTTP(S) proxy for BuildKit RUN steps.
  --label KEY=VALUE          May be repeated.
  --load                     Load one platform into the local Docker Engine (default).
  --push                     Push the image and its attestations.
  --allow-dirty              Permit a local verification image from a dirty worktree.
  --no-cache
  --help
EOF
}

normalize_executable() {
  local value="$1"
  local flag="$2"
  local trimmed="$value"

  trimmed="${trimmed#"${trimmed%%[![:space:]]*}"}"
  trimmed="${trimmed%"${trimmed##*[![:space:]]}"}"
  if [[ -z "$trimmed" || "$trimmed" =~ [[:cntrl:]] ]]; then
    echo "$flag must be a command or executable path" >&2
    exit 2
  fi
  printf '%s' "$trimmed"
}

while (($# > 0)); do
  case "$1" in
    --target)
      target="${2:?--target requires a value}"
      shift 2
      ;;
    --image)
      image="${2:?--image requires a value}"
      shift 2
      ;;
    --version)
      version="${2:?--version requires a value}"
      shift 2
      ;;
    --git-sha)
      git_sha="${2:?--git-sha requires a value}"
      shift 2
      ;;
    --source-date-epoch)
      source_date_epoch="${2:?--source-date-epoch requires a value}"
      shift 2
      ;;
    --platform)
      platforms="${2:?--platform requires a value}"
      shift 2
      ;;
    --metadata-file)
      metadata_file="${2:?--metadata-file requires a value}"
      shift 2
      ;;
    --builder)
      builder="${2:?--builder requires a value}"
      shift 2
      ;;
    --docker-bin)
      docker_bin="${2:?--docker-bin requires a value}"
      shift 2
      ;;
    --go-proxy)
      go_proxy="${2:?--go-proxy requires a value}"
      shift 2
      ;;
    --network-proxy)
      network_proxy="${2:?--network-proxy requires a value}"
      shift 2
      ;;
    --label)
      label_values[$label_count]="${2:?--label requires a value}"
      ((label_count += 1))
      shift 2
      ;;
    --load)
      output_mode="load"
      shift
      ;;
    --push)
      output_mode="push"
      shift
      ;;
    --allow-dirty)
      allow_dirty=1
      shift
      ;;
    --no-cache)
      no_cache=1
      shift
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      echo "Unknown option: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

docker_bin="$(normalize_executable "$docker_bin" "--docker-bin")"

case "$target" in
  worker|worker-acceptance) ;;
  *)
    echo "--target must be worker or worker-acceptance" >&2
    exit 2
    ;;
esac

if [[ -z "$git_sha" ]]; then
  git_sha="$(git -C "$repo_root" rev-parse HEAD)"
fi
git_sha="$(printf '%s' "$git_sha" | tr '[:upper:]' '[:lower:]')"
if [[ ! "$git_sha" =~ ^([0-9a-f]{40}|[0-9a-f]{64})$ ]]; then
  echo "--git-sha must be a full lowercase hexadecimal Git object ID" >&2
  exit 2
fi

if [[ -z "$version" ]]; then
  version="$(node -e 'const fs=require("node:fs"); const value=JSON.parse(fs.readFileSync(process.argv[1],"utf8")).version; if(!value) process.exit(1); process.stdout.write(value)' "$repo_root/apps/server/package.json")"
fi

if [[ -n "$go_proxy" ]]; then
  if [[ "$go_proxy" =~ [[:space:][:cntrl:]] || "$go_proxy" == *"@"* || "$go_proxy" == *"?"* || "$go_proxy" == *"#"* ]]; then
    echo "--go-proxy must be a public credential-free GOPROXY list without whitespace, userinfo, query, or fragment data" >&2
    exit 2
  fi
  go_proxy_url_pattern="^https://[A-Za-z0-9.-]+(:[0-9]+)?(/[A-Za-z0-9._~!$&'()*+,;=:%/-]*)?$"
  IFS=',' read -r -a go_proxy_entries <<<"$go_proxy"
  for go_proxy_entry in "${go_proxy_entries[@]}"; do
    if [[ "$go_proxy_entry" != "direct" && "$go_proxy_entry" != "off" && ! "$go_proxy_entry" =~ $go_proxy_url_pattern ]]; then
      echo "--go-proxy entries must use https://, direct, or off" >&2
      exit 2
    fi
  done
fi
if [[ -n "$network_proxy" ]]; then
  if [[ "$network_proxy" =~ [[:space:][:cntrl:]] || "$network_proxy" == *"@"* || "$network_proxy" == *"?"* || "$network_proxy" == *"#"* ]]; then
    echo "--network-proxy must be a credential-free HTTP(S) authority without whitespace, userinfo, query, or fragment data" >&2
    exit 2
  fi
  network_proxy_pattern='^https?://[A-Za-z0-9.-]+:[0-9]+/?$'
  if [[ ! "$network_proxy" =~ $network_proxy_pattern ]]; then
    echo "--network-proxy must use http:// or https:// with an explicit host and port" >&2
    exit 2
  fi
  network_proxy="${network_proxy%/}"
fi
if [[ -z "$version" ]]; then
  echo "Worker version is empty" >&2
  exit 2
fi

if [[ -z "$source_date_epoch" ]]; then
  source_date_epoch="$(git -C "$repo_root" show -s --format=%ct "$git_sha")"
fi
if [[ ! "$source_date_epoch" =~ ^(0|[1-9][0-9]*)$ ]]; then
  echo "--source-date-epoch must be a non-negative integer" >&2
  exit 2
fi

if ((allow_dirty == 0)); then
  dirty="$(git -C "$repo_root" status --porcelain --untracked-files=all)"
  if [[ -n "$dirty" ]]; then
    echo "Refusing to label a dirty worktree as Git SHA $git_sha; commit the changes or pass --allow-dirty for local verification." >&2
    exit 1
  fi
fi

if [[ -z "$platforms" ]]; then
  if [[ "$output_mode" == "push" ]]; then
    platforms="linux/amd64,linux/arm64"
  else
    engine_arch="$("$docker_bin" info --format '{{.Architecture}}')"
    case "$engine_arch" in
      amd64|x86_64) platforms="linux/amd64" ;;
      arm64|aarch64) platforms="linux/arm64" ;;
      *)
        echo "Unsupported Docker Engine architecture: $engine_arch" >&2
        exit 1
        ;;
    esac
  fi
fi
if [[ "$output_mode" == "load" && "$platforms" == *,* ]]; then
  echo "--load supports one platform; use --push for a multi-platform image" >&2
  exit 2
fi

if [[ -z "$image" ]]; then
  image="synara-worker:${git_sha:0:12}"
fi
if [[ -z "$metadata_file" ]]; then
  metadata_file="$repo_root/build/worker-image-${target}-metadata.json"
elif [[ "$metadata_file" != /* ]]; then
  metadata_file="$repo_root/$metadata_file"
fi
mkdir -p "$(dirname "$metadata_file")"

if [[ "$output_mode" == "push" ]]; then
  if [[ ! -f "$sbom_generator_lock" ]]; then
    echo "Missing BuildKit SBOM generator lock: $sbom_generator_lock" >&2
    exit 1
  fi
  IFS= read -r sbom_generator <"$sbom_generator_lock"
  if [[ ! "$sbom_generator" =~ ^[a-z0-9][a-z0-9._:/-]*[a-z0-9]@sha256:[0-9a-f]{64}$ ]]; then
    echo "BuildKit SBOM generator lock must contain one digest-pinned image reference" >&2
    exit 1
  fi
  if [[ -z "$builder" ]]; then
    builder="synara-worker-builder"
  fi
  if ! "$docker_bin" buildx inspect "$builder" >/dev/null 2>&1; then
    "$docker_bin" buildx create --name "$builder" --driver docker-container >/dev/null
  fi
  builder_driver="$("$docker_bin" buildx inspect "$builder" | awk -F: '/^Driver:/ && driver == "" {driver = $2; gsub(/^[[:space:]]+|[[:space:]]+$/, "", driver)} END {print driver}')"
  if [[ "$builder_driver" != "docker-container" ]]; then
    echo "Buildx builder $builder uses unsupported driver $builder_driver; a docker-container builder is required for attestations." >&2
    exit 1
  fi
  "$docker_bin" buildx inspect "$builder" --bootstrap >/dev/null
elif [[ -n "$builder" ]] && ! "$docker_bin" buildx inspect "$builder" >/dev/null 2>&1; then
  echo "Buildx builder $builder does not exist." >&2
  exit 1
fi

output=(--load)
if [[ "$output_mode" == "push" ]]; then
  output=(--output "type=image,push=true,rewrite-timestamp=true")
fi
build_command=(
  "$docker_bin"
  buildx
  build
  --target "$target"
  --tag "$image"
  --platform "$platforms"
  --build-arg "SYNARA_VERSION=$version"
  --build-arg "SYNARA_GIT_SHA=$git_sha"
  --build-arg "SOURCE_DATE_EPOCH=$source_date_epoch"
  --label "org.opencontainers.image.version=$version"
  --label "org.opencontainers.image.revision=$git_sha"
  --metadata-file "$metadata_file"
)
if [[ -n "$go_proxy" ]]; then
  build_command+=(--build-arg "GOPROXY=$go_proxy")
fi
if [[ -n "$network_proxy" ]]; then
  # Docker treats these predefined proxy build arguments specially: they are
  # available to RUN steps but are excluded from image history and cache keys.
  # Pass both cases because package managers differ in which form they honor.
  build_command+=(
    --build-arg "HTTP_PROXY=$network_proxy"
    --build-arg "HTTPS_PROXY=$network_proxy"
    --build-arg "ALL_PROXY=$network_proxy"
    --build-arg "NO_PROXY=127.0.0.1,localhost,::1"
    --build-arg "http_proxy=$network_proxy"
    --build-arg "https_proxy=$network_proxy"
    --build-arg "all_proxy=$network_proxy"
    --build-arg "no_proxy=127.0.0.1,localhost,::1"
  )
fi
if [[ -n "$builder" ]]; then
  build_command+=(--builder "$builder")
fi
if [[ "$output_mode" == "push" ]]; then
  build_command+=("--sbom=generator=$sbom_generator" --provenance=mode=max)
else
  # Docker's local image store cannot retain attestations. The image still
  # contains the normalized SPDX document and strict Worker Image Manifest.
  build_command+=(--provenance=false)
fi
for ((index = 0; index < label_count; index += 1)); do
  build_command+=(--label "${label_values[$index]}")
done
if ((no_cache == 1)); then
  build_command+=(--no-cache)
fi
build_command+=("${output[@]}" "$repo_root")
"${build_command[@]}"

digest="$(node -e 'const fs=require("node:fs"); const value=JSON.parse(fs.readFileSync(process.argv[1],"utf8"))["containerimage.digest"]; if(value) process.stdout.write(value)' "$metadata_file")"
echo "Worker image: $image"
echo "Build metadata: $metadata_file"
if [[ -n "$digest" ]]; then
  echo "Image digest: $digest"
fi
