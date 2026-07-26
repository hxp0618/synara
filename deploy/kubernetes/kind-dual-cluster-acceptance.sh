#!/usr/bin/env bash
set -Eeuo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
control_plane_dir="$repo_root/services/control-plane"

kind_bin="${KIND_BIN:-kind}"
primary_context="${SYNARA_DUAL_CLUSTER_PRIMARY_CONTEXT:-orbstack}"
allow_non_orbstack="${SYNARA_DUAL_CLUSTER_ALLOW_NON_ORBSTACK_PRIMARY:-0}"
keep_kind_cluster="${SYNARA_DUAL_CLUSTER_KEEP_KIND_CLUSTER:-0}"
kind_node_image="${SYNARA_DUAL_CLUSTER_KIND_NODE_IMAGE:-kindest/node:v1.33.1}"
evidence_file="${SYNARA_DUAL_CLUSTER_EVIDENCE_FILE:-}"
token_duration='15m'
test_name='TestStage4DualClusterDisasterRecoveryAgainstRealAPIServers'
primary_host_alias="${SYNARA_DUAL_CLUSTER_PRIMARY_HOST_ALIAS:-host.docker.internal}"
secondary_host_alias="${SYNARA_DUAL_CLUSTER_SECONDARY_HOST_ALIAS:-host.docker.internal}"

run_started_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
run_id="$(date -u +%Y%m%d%H%M%S)-$$-${RANDOM}"
name_suffix="$(printf '%s' "$run_id" | tr '[:upper:]_' '[:lower:]-' | tr -cd 'a-z0-9-')"
name_suffix="${name_suffix:0:30}"
secondary_cluster="${SYNARA_DUAL_CLUSTER_KIND_CLUSTER:-synara-dr-$name_suffix}"
secondary_context="kind-$secondary_cluster"
auth_namespace="synara-dr-auth-$name_suffix"
primary_target_namespace="synara-dr-primary-$name_suffix"
secondary_target_namespace="synara-dr-secondary-$name_suffix"
service_account="synara-dr-$name_suffix"
cluster_role="synara-dr-$name_suffix"
role_binding="synara-dr-$name_suffix"
namespace_cluster_role="synara-dr-namespace-$name_suffix"
namespace_cluster_role_binding="synara-dr-namespace-$name_suffix"
worker_image="synara-worker:dual-cluster-$name_suffix"

temp_dir=''
secondary_kubeconfig=''
detail_file=''
kind_marker=''
worker_metadata_file=''
created_cluster=0
worker_build_attempted=0
worker_identity_verified=0
resources_created_primary=0
resources_created_secondary=0
primary_namespace_uid=''
primary_target_namespace_uid=''
primary_service_account_uid=''
primary_cluster_role_uid=''
primary_role_binding_uid=''
primary_namespace_cluster_role_uid=''
primary_namespace_binding_uid=''
secondary_namespace_uid=''
secondary_target_namespace_uid=''
secondary_service_account_uid=''
secondary_cluster_role_uid=''
secondary_role_binding_uid=''
secondary_namespace_cluster_role_uid=''
secondary_namespace_binding_uid=''
primary_api_server=''
primary_ca=''
primary_token=''
secondary_api_server=''
secondary_ca=''
secondary_token=''
primary_version='unknown'
secondary_version='unknown'
primary_node_count=0
secondary_node_count=0
initial_current_context=''
final_current_context=''
current_context_unchanged=false
test_status='not-run'
test_exit_code=0
detail_produced=false
detail_valid=false
detail_bounded=false
detail_secret_free=false
detail_assertions_valid=false
detail_digest=''
detail_bytes=0
integration_assertions='{}'
source_sha='unknown'
worktree_dirty=false
worker_config_id='unknown'
worker_manifest_digest='unknown'
worker_runtime_image_ids=''
worker_image_json=''
worker_image_owner=''
worker_image_revision=''
cleanup_status='not-run'
cleanup_failed=0
phase='preflight'
result_phase='preflight'
failure_reason=''
evidence_reserved=0
finalized=0

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf '%s is required\n' "$1" >&2
    exit 1
  fi
}

fail() {
  failure_reason="$1"
  printf '%s\n' "$failure_reason" >&2
  exit 1
}

validate_name() {
  local value="$1"
  local label="$2"
  if [[ ! "$value" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ || ${#value} -gt 63 ]]; then
    fail "$label must be a lowercase Kubernetes DNS label of at most 63 characters"
  fi
}

validate_context_label() {
  local value="$1"
  if [[ -z "$value" || ${#value} -gt 253 || "$value" == *$'\n'* || "$value" == *$'\r'* ]]; then
    fail 'primary context must be non-empty, single-line, and at most 253 characters'
  fi
}

validate_dns_hostname() {
  local value="$1" label="$2"
  local pattern='^([A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)*[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$'
  if [[ -z "$value" || ${#value} -gt 253 || ! "$value" =~ $pattern || "$value" =~ ^[0-9.]+$ ]]; then
    fail "$label must be a DNS hostname of at most 253 characters"
  fi
}

kube() {
  local target="$1"
  shift
  case "$target" in
    primary) kubectl --context "$primary_context" "$@" ;;
    secondary) kubectl --kubeconfig "$secondary_kubeconfig" --context "$secondary_context" "$@" ;;
    *) printf 'unknown Kubernetes target: %s\n' "$target" >&2; return 2 ;;
  esac
}

resource_is_owned() {
  local target="$1" resource="$2" name="$3" expected_uid="$4" namespace="${5:-}"
  local object
  [[ -n "$expected_uid" ]] || return 1
  if [[ -n "$namespace" ]]; then
    object="$(kube "$target" get "$resource" "$name" -n "$namespace" -o json 2>/dev/null)" || return 1
  elif ! object="$(kube "$target" get "$resource" "$name" -o json 2>/dev/null)"; then
    return 1
  fi
  jq -e --arg uid "$expected_uid" --arg runID "$run_id" \
    '.metadata.uid == $uid and .metadata.labels["synara.io/acceptance-run-id"] == $runID' \
    >/dev/null <<<"$object"
}

resource_raw_uri() {
  local resource="$1" name="$2" namespace="${3:-}"
  case "$resource" in
    namespace) printf '/api/v1/namespaces/%s' "$name" ;;
    clusterrole.rbac.authorization.k8s.io)
      printf '/apis/rbac.authorization.k8s.io/v1/clusterroles/%s' "$name"
      ;;
    clusterrolebinding.rbac.authorization.k8s.io)
      printf '/apis/rbac.authorization.k8s.io/v1/clusterrolebindings/%s' "$name"
      ;;
    rolebinding.rbac.authorization.k8s.io)
      [[ -n "$namespace" ]] || return 2
      printf '/apis/rbac.authorization.k8s.io/v1/namespaces/%s/rolebindings/%s' "$namespace" "$name"
      ;;
    *) return 2 ;;
  esac
}

wait_for_uid_absent() {
  local target="$1" resource="$2" name="$3" expected_uid="$4" namespace="${5:-}"
  local attempts=15 attempt object current_uid
  if [[ "$resource" == namespace ]]; then
    attempts=90
  fi
  for ((attempt = 1; attempt <= attempts; attempt += 1)); do
    if [[ -n "$namespace" ]]; then
      if ! object="$(kube "$target" get "$resource" "$name" -n "$namespace" -o json 2>&1)"; then
        if [[ "$object" == *'(NotFound)'* || "$object" == *' not found'* ]]; then return 0; fi
        sleep 1
        continue
      fi
    else
      if ! object="$(kube "$target" get "$resource" "$name" -o json 2>&1)"; then
        if [[ "$object" == *'(NotFound)'* || "$object" == *' not found'* ]]; then return 0; fi
        sleep 1
        continue
      fi
    fi
    current_uid="$(jq -er '.metadata.uid' <<<"$object")" || return 1
    [[ "$current_uid" != "$expected_uid" ]] && return 0
    sleep 1
  done
  printf 'Timed out waiting for original UID of %s/%s to disappear in %s\n' \
    "$resource" "$name" "$target" >&2
  return 1
}

delete_owned_resource() {
  local target="$1" resource="$2" name="$3" expected_uid="$4" namespace="${5:-}"
  local raw_uri delete_options
  if [[ -z "$expected_uid" ]]; then
    return 0
  fi
  if ! resource_is_owned "$target" "$resource" "$name" "$expected_uid" "$namespace"; then
    printf 'Refusing cleanup of %s/%s in %s: ownership changed or cannot be verified\n' \
      "$resource" "$name" "$target" >&2
    return 1
  fi
  raw_uri="$(resource_raw_uri "$resource" "$name" "$namespace")" || return 1
  delete_options="$(jq -nc --arg uid "$expected_uid" \
    '{apiVersion:"v1",kind:"DeleteOptions",propagationPolicy:"Background",preconditions:{uid:$uid}}')"
  printf '%s' "$delete_options" | kube "$target" delete --raw "$raw_uri" -f - >/dev/null
  wait_for_uid_absent "$target" "$resource" "$name" "$expected_uid" "$namespace"
}

create_auth_resources() {
  local target="$1"
  local target_namespace namespace_uid target_namespace_uid service_account_uid
  local cluster_role_uid role_binding_uid namespace_cluster_role_uid namespace_binding_uid
  local created_object
  if [[ "$target" == primary ]]; then
    target_namespace="$primary_target_namespace"
  else
    target_namespace="$secondary_target_namespace"
  fi
  for resource_name in \
    "namespace/$auth_namespace" \
    "namespace/$target_namespace" \
    "clusterrole.rbac.authorization.k8s.io/$cluster_role" \
    "clusterrole.rbac.authorization.k8s.io/$namespace_cluster_role" \
    "clusterrolebinding.rbac.authorization.k8s.io/$namespace_cluster_role_binding"; do
    if kube "$target" get "$resource_name" >/dev/null 2>&1; then
      fail "$resource_name already exists in $target; refusing to reuse it"
    fi
  done

  created_object="$(kube "$target" create -f - -o json <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: $auth_namespace
  labels:
    app.kubernetes.io/name: synara-dual-cluster-acceptance
    app.kubernetes.io/part-of: synara
    synara.io/acceptance-run-id: $run_id
EOF
)"
  namespace_uid="$(jq -er '.metadata.uid' <<<"$created_object")"
  if [[ "$target" == primary ]]; then
    primary_namespace_uid="$namespace_uid"
    resources_created_primary=1
  else
    secondary_namespace_uid="$namespace_uid"
    resources_created_secondary=1
  fi

  created_object="$(kube "$target" create -f - -o json <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: $target_namespace
  labels:
    app.kubernetes.io/name: synara-dual-cluster-acceptance-target
    app.kubernetes.io/part-of: synara
    synara.io/acceptance-run-id: $run_id
EOF
)"
  target_namespace_uid="$(jq -er '.metadata.uid' <<<"$created_object")"
  if [[ "$target" == primary ]]; then
    primary_target_namespace_uid="$target_namespace_uid"
  else
    secondary_target_namespace_uid="$target_namespace_uid"
  fi

  created_object="$(kube "$target" create -f - -o json <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: $service_account
  namespace: $auth_namespace
  labels:
    app.kubernetes.io/name: synara-dual-cluster-acceptance
    app.kubernetes.io/part-of: synara
    synara.io/acceptance-run-id: $run_id
automountServiceAccountToken: false
EOF
)"
  service_account_uid="$(jq -er '.metadata.uid' <<<"$created_object")"
  if [[ "$target" == primary ]]; then
    primary_service_account_uid="$service_account_uid"
  else
    secondary_service_account_uid="$service_account_uid"
  fi

  created_object="$(kube "$target" create -f - -o json <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: $cluster_role
  labels:
    app.kubernetes.io/name: synara-dual-cluster-acceptance
    app.kubernetes.io/part-of: synara
    synara.io/acceptance-run-id: $run_id
rules:
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "create", "patch", "delete"]
  - apiGroups: [""]
    resources: ["secrets", "serviceaccounts", "resourcequotas"]
    verbs: ["get", "create", "patch"]
  - apiGroups: ["networking.k8s.io"]
    resources: ["networkpolicies"]
    verbs: ["get", "create", "patch"]
EOF
)"
  cluster_role_uid="$(jq -er '.metadata.uid' <<<"$created_object")"
  if [[ "$target" == primary ]]; then
    primary_cluster_role_uid="$cluster_role_uid"
  else
    secondary_cluster_role_uid="$cluster_role_uid"
  fi

  created_object="$(kube "$target" create -f - -o json <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: $role_binding
  namespace: $target_namespace
  labels:
    app.kubernetes.io/name: synara-dual-cluster-acceptance
    app.kubernetes.io/part-of: synara
    synara.io/acceptance-run-id: $run_id
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: $cluster_role
subjects:
  - kind: ServiceAccount
    name: $service_account
    namespace: $auth_namespace
EOF
)"
  role_binding_uid="$(jq -er '.metadata.uid' <<<"$created_object")"
  if [[ "$target" == primary ]]; then
    primary_role_binding_uid="$role_binding_uid"
  else
    secondary_role_binding_uid="$role_binding_uid"
  fi

  created_object="$(kube "$target" create -f - -o json <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: $namespace_cluster_role
  labels:
    app.kubernetes.io/name: synara-dual-cluster-acceptance
    app.kubernetes.io/part-of: synara
    synara.io/acceptance-run-id: $run_id
rules:
  - apiGroups: [""]
    resources: ["namespaces"]
    resourceNames: ["$target_namespace"]
    verbs: ["get"]
  - apiGroups: ["authentication.k8s.io"]
    resources: ["tokenreviews"]
    verbs: ["create"]
EOF
)"
  namespace_cluster_role_uid="$(jq -er '.metadata.uid' <<<"$created_object")"
  if [[ "$target" == primary ]]; then
    primary_namespace_cluster_role_uid="$namespace_cluster_role_uid"
  else
    secondary_namespace_cluster_role_uid="$namespace_cluster_role_uid"
  fi

  created_object="$(kube "$target" create -f - -o json <<EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: $namespace_cluster_role_binding
  labels:
    app.kubernetes.io/name: synara-dual-cluster-acceptance
    app.kubernetes.io/part-of: synara
    synara.io/acceptance-run-id: $run_id
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: $namespace_cluster_role
subjects:
  - kind: ServiceAccount
    name: $service_account
    namespace: $auth_namespace
EOF
)"
  namespace_binding_uid="$(jq -er '.metadata.uid' <<<"$created_object")"
  if [[ "$target" == primary ]]; then
    primary_namespace_binding_uid="$namespace_binding_uid"
  else
    secondary_namespace_binding_uid="$namespace_binding_uid"
  fi
}

extract_connection() {
  local target="$1"
  local cluster_json api_server ca_data ca_file ca_value
  cluster_json="$(kube "$target" config view --raw --flatten --minify -o json | jq -ec '.clusters[0].cluster')"
  api_server="$(jq -er '.server' <<<"$cluster_json")"
  [[ "$api_server" =~ ^https:// ]] || fail "$target Kubernetes API server must use https"
  ca_data="$(jq -r '."certificate-authority-data" // empty' <<<"$cluster_json")"
  ca_file="$(jq -r '."certificate-authority" // empty' <<<"$cluster_json")"
  if [[ -n "$ca_data" ]]; then
    ca_value="$(printf '%s' "$ca_data" | openssl base64 -d -A)" || \
      fail "failed to decode $target Kubernetes certificate authority"
  elif [[ -n "$ca_file" && -f "$ca_file" ]]; then
    ca_value="$(<"$ca_file")"
  else
    fail "$target Kubernetes context has no readable certificate authority"
  fi
  [[ "$ca_value" == *'BEGIN CERTIFICATE'* ]] || \
    fail "$target Kubernetes certificate authority is not PEM encoded"

  if [[ "$target" == primary ]]; then
    primary_api_server="$api_server"
    primary_ca="$ca_value"
    primary_token="$(kube primary create token "$service_account" -n "$auth_namespace" --duration="$token_duration")"
    [[ -n "$primary_token" ]] || fail 'primary TokenRequest returned an empty token'
  else
    secondary_api_server="$api_server"
    secondary_ca="$ca_value"
    secondary_token="$(kube secondary create token "$service_account" -n "$auth_namespace" --duration="$token_duration")"
    [[ -n "$secondary_token" ]] || fail 'secondary TokenRequest returned an empty token'
  fi
}

collect_cluster_metadata() {
  local target="$1" version node_count
  version="$(kube "$target" version -o json | jq -er '.serverVersion.gitVersion')"
  node_count="$(kube "$target" get nodes -o json | jq -er '.items | length')"
  if [[ "$target" == primary ]]; then
    primary_version="$version"
    primary_node_count="$node_count"
  else
    secondary_version="$version"
    secondary_node_count="$node_count"
  fi
}

cleanup_context_resources() {
  local target="$1"
  local target_namespace namespace_uid target_namespace_uid service_account_uid
  local cluster_role_uid role_binding_uid namespace_cluster_role_uid namespace_binding_uid
  if [[ "$target" == primary ]]; then
    target_namespace="$primary_target_namespace"
    namespace_uid="$primary_namespace_uid"
    target_namespace_uid="$primary_target_namespace_uid"
    service_account_uid="$primary_service_account_uid"
    cluster_role_uid="$primary_cluster_role_uid"
    role_binding_uid="$primary_role_binding_uid"
    namespace_cluster_role_uid="$primary_namespace_cluster_role_uid"
    namespace_binding_uid="$primary_namespace_binding_uid"
  else
    target_namespace="$secondary_target_namespace"
    namespace_uid="$secondary_namespace_uid"
    target_namespace_uid="$secondary_target_namespace_uid"
    service_account_uid="$secondary_service_account_uid"
    cluster_role_uid="$secondary_cluster_role_uid"
    role_binding_uid="$secondary_role_binding_uid"
    namespace_cluster_role_uid="$secondary_namespace_cluster_role_uid"
    namespace_binding_uid="$secondary_namespace_binding_uid"
  fi
  delete_owned_resource "$target" rolebinding.rbac.authorization.k8s.io \
    "$role_binding" "$role_binding_uid" "$target_namespace" || cleanup_failed=1
  delete_owned_resource "$target" clusterrolebinding.rbac.authorization.k8s.io \
    "$namespace_cluster_role_binding" "$namespace_binding_uid" || cleanup_failed=1
  delete_owned_resource "$target" clusterrole.rbac.authorization.k8s.io \
    "$cluster_role" "$cluster_role_uid" || cleanup_failed=1
  delete_owned_resource "$target" clusterrole.rbac.authorization.k8s.io \
    "$namespace_cluster_role" "$namespace_cluster_role_uid" || cleanup_failed=1
  delete_owned_resource "$target" namespace "$target_namespace" \
    "$target_namespace_uid" || cleanup_failed=1
  if [[ -n "$namespace_uid" ]]; then
    if [[ -n "$service_account_uid" ]] && ! resource_is_owned "$target" serviceaccount "$service_account" \
      "$service_account_uid" "$auth_namespace"; then
      printf 'Refusing cleanup of namespace/%s in %s: ServiceAccount ownership changed or cannot be verified\n' \
        "$auth_namespace" "$target" >&2
      cleanup_failed=1
    else
      delete_owned_resource "$target" namespace "$auth_namespace" "$namespace_uid" || cleanup_failed=1
    fi
  fi
}

delete_owned_kind_cluster() {
  local node label nodes
  if [[ "$created_cluster" != 1 || "$keep_kind_cluster" == 1 ]]; then
    return 0
  fi
  if [[ ! -f "$kind_marker" || "$(<"$kind_marker")" != "$run_id:$secondary_cluster" ]]; then
    printf 'Refusing Kind cleanup: run ownership marker is missing or changed\n' >&2
    return 1
  fi
  if ! KUBECONFIG="$secondary_kubeconfig" "$kind_bin" get clusters | grep -Fxq "$secondary_cluster"; then
    return 0
  fi
  if ! nodes="$(KUBECONFIG="$secondary_kubeconfig" "$kind_bin" get nodes --name "$secondary_cluster")" || \
    [[ -z "$nodes" ]]; then
    printf 'Refusing Kind cleanup: could not enumerate owned nodes for %s\n' "$secondary_cluster" >&2
    return 1
  fi
  while IFS= read -r node; do
    [[ -n "$node" ]] || continue
    if ! label="$(docker inspect --format '{{ index .Config.Labels "io.x-k8s.kind.cluster" }}' "$node" 2>/dev/null)" || \
      [[ "$label" != "$secondary_cluster" ]]; then
      printf 'Refusing Kind cleanup: node %s is not owned by cluster %s\n' "$node" "$secondary_cluster" >&2
      return 1
    fi
  done <<<"$nodes"
  KUBECONFIG="$secondary_kubeconfig" "$kind_bin" delete cluster --name "$secondary_cluster" >/dev/null
}

delete_owned_worker_image_tag() {
  local image_json current_id current_owner
  if [[ "$worker_build_attempted" != 1 ]]; then
    return 0
  fi
  if ! image_json="$(docker image inspect "$worker_image" 2>&1)"; then
    if [[ "$image_json" == *'No such image'* ]]; then
      return 0
    fi
    printf 'Refusing Worker image cleanup: tag presence could not be verified for %s\n' \
      "$worker_image" >&2
    return 1
  fi
  current_id="$(jq -er '.[0].Id' <<<"$image_json")" || return 1
  current_owner="$(jq -er '.[0].Config.Labels["synara.io/acceptance-run-id"] // empty' <<<"$image_json")" || return 1
  if [[ "$current_owner" != "$run_id" ]]; then
    printf 'Refusing Worker image cleanup: run ownership label changed for %s\n' \
      "$worker_image" >&2
    return 1
  fi
  if [[ "$worker_identity_verified" == 1 && "$current_id" != "$worker_config_id" ]]; then
    printf 'Refusing Worker image cleanup: tag config ID changed for %s\n' "$worker_image" >&2
    return 1
  fi
  docker image rm "$worker_image" >/dev/null
}

finalize_detail() {
  local detail_content
  local allowed_assertion_keys='[
    "exactReadinessAccepted",
    "missingReadinessFailedClosed",
    "obsoletePrimaryPodAbsent",
    "podBoundWorkloadIdentityVerified",
    "recoveryBundleIntegrityVerified",
    "singleSuccessor",
    "sourcePlacementImmutable",
    "sourceRuntimeReady",
    "successorLineagePersisted",
    "successorRuntimeReady"
  ]'
  [[ -f "$detail_file" ]] || return 0
  detail_produced=true
  detail_bytes="$(wc -c <"$detail_file" | tr -d '[:space:]')"
  detail_digest="$(shasum -a 256 "$detail_file" | awk '{print $1}')"
  if jq -e . "$detail_file" >/dev/null 2>&1; then detail_valid=true; fi
  if (( detail_bytes <= 65536 )); then detail_bounded=true; fi
  detail_content="$(<"$detail_file")"
  if [[ "$detail_content" != *"$primary_token"* && \
    "$detail_content" != *"$secondary_token"* && \
    "$detail_content" != *"$primary_ca"* && \
    "$detail_content" != *"$secondary_ca"* && \
    "$detail_content" != *"$primary_api_server"* && \
    "$detail_content" != *"$secondary_api_server"* ]]; then
    detail_secret_free=true
  fi
  if [[ "$detail_valid" == true && "$detail_bounded" == true ]]; then
    if jq -e --argjson allowed "$allowed_assertion_keys" '
      (.assertions | type) == "object" and
      ((.assertions | keys | sort) == ($allowed | sort)) and
      all(.assertions[]; type == "boolean")
    ' "$detail_file" >/dev/null; then
      integration_assertions="$(jq -c '{
        exactReadinessAccepted: .assertions.exactReadinessAccepted,
        missingReadinessFailedClosed: .assertions.missingReadinessFailedClosed,
        obsoletePrimaryPodAbsent: .assertions.obsoletePrimaryPodAbsent,
        podBoundWorkloadIdentityVerified: .assertions.podBoundWorkloadIdentityVerified,
        recoveryBundleIntegrityVerified: .assertions.recoveryBundleIntegrityVerified,
        singleSuccessor: .assertions.singleSuccessor,
        sourcePlacementImmutable: .assertions.sourcePlacementImmutable,
        sourceRuntimeReady: .assertions.sourceRuntimeReady,
        successorLineagePersisted: .assertions.successorLineagePersisted,
        successorRuntimeReady: .assertions.successorRuntimeReady
      }' "$detail_file")"
      detail_assertions_valid=true
    fi
  fi
}

write_evidence() {
  local status="$1" exit_code="$2" finished_at evidence_tmp
  [[ "$evidence_reserved" == 1 ]] || return 0
  finished_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
  evidence_tmp="$(mktemp "$(dirname "$evidence_file")/.synara-dual-cluster-evidence.XXXXXX")"
  if ! jq -n \
    --arg schemaVersion 'synara.kubernetes-dual-cluster-acceptance.v1' \
    --arg runID "$run_id" --arg startedAt "$run_started_at" --arg finishedAt "$finished_at" \
    --arg status "$status" --arg phase "$result_phase" --arg failure "$failure_reason" \
    --arg primaryContext "$primary_context" --arg secondaryContext "$secondary_context" \
    --arg primaryVersion "$primary_version" --arg secondaryVersion "$secondary_version" \
    --argjson primaryNodes "$primary_node_count" --argjson secondaryNodes "$secondary_node_count" \
    --arg testName "$test_name" --arg testStatus "$test_status" --argjson testExitCode "$test_exit_code" \
    --argjson currentContextUnchanged "$current_context_unchanged" \
    --argjson detailProduced "$detail_produced" --argjson detailValid "$detail_valid" \
    --argjson detailBounded "$detail_bounded" --argjson detailSecretFree "$detail_secret_free" \
    --argjson detailAssertionsValid "$detail_assertions_valid" \
    --argjson detailBytes "$detail_bytes" \
    --arg detailDigest "$detail_digest" --argjson assertions "$integration_assertions" \
    --arg sourceSHA "$source_sha" --argjson worktreeDirty "$worktree_dirty" \
    --arg workerImage "$worker_image" --arg workerConfigID "${worker_config_id:-unknown}" \
    --arg workerManifestDigest "${worker_manifest_digest:-unknown}" \
    --arg cleanupStatus "$cleanup_status" --argjson exitCode "$exit_code" '
      {
        schemaVersion: $schemaVersion, runID: $runID, startedAt: $startedAt, finishedAt: $finishedAt,
        status: $status, exitCode: $exitCode, phase: $phase,
        failure: (if $failure == "" then null else $failure[0:256] end),
        contexts: {
          primary: {name: $primaryContext[0:253], kubernetesVersion: $primaryVersion[0:128], nodeCount: $primaryNodes},
          secondary: {name: $secondaryContext[0:253], kubernetesVersion: $secondaryVersion[0:128], nodeCount: $secondaryNodes}
        },
        test: {name: $testName, status: $testStatus, exitCode: $testExitCode},
        checks: {
          currentContextUnchanged: $currentContextUnchanged, detailProduced: $detailProduced,
          detailValidJSON: $detailValid, detailWithin65536Bytes: $detailBounded,
          detailContainsNoConnectionMaterial: $detailSecretFree,
          detailAssertionsAllowlistedBooleans: $detailAssertionsValid,
          integrationAssertions: $assertions
        },
        detail: {bytes: $detailBytes, sha256: (if $detailDigest == "" then null else $detailDigest end)},
        source: {gitSHA: $sourceSHA, worktreeDirty: $worktreeDirty},
        workerImage: {
          tag: $workerImage, configID: $workerConfigID, manifestDigest: $workerManifestDigest,
          acceptanceBoundary: {
            workerAPI: "bounded-stub-with-production-kubernetes-identity-verifier",
            podBoundWorkloadIdentityVerified: ($assertions.podBoundWorkloadIdentityVerified // false),
            fullWorkerRegistrationPersistenceVerified: false
          }
        },
        cleanup: {status: $cleanupStatus},
        safety: {tokenRequestCredentialsPersisted: false, touchedSynaraSystem: false, kindKubeconfigIsolated: true}
      }' >"$evidence_tmp"; then
    rm -f "$evidence_tmp"
    return 1
  fi
  if ! mv -f "$evidence_tmp" "$evidence_file"; then
    rm -f "$evidence_tmp"
    return 1
  fi
}

cleanup_all() {
  cleanup_status='passed'
  phase='cleanup'
  if [[ "$resources_created_secondary" == 1 ]]; then cleanup_context_resources secondary; fi
  if [[ "$resources_created_primary" == 1 ]]; then cleanup_context_resources primary; fi
  delete_owned_kind_cluster || cleanup_failed=1
  delete_owned_worker_image_tag || cleanup_failed=1
  final_current_context="$(kubectl config current-context 2>/dev/null || true)"
  if [[ "$final_current_context" == "$initial_current_context" ]]; then
    current_context_unchanged=true
  else
    cleanup_failed=1
  fi
  if [[ "$cleanup_failed" == 1 ]]; then cleanup_status='failed'; fi
  primary_token=''; secondary_token=''; primary_ca=''; secondary_ca=''
}

on_exit() {
  local exit_code="$?" final_status='failed'
  trap - EXIT ERR INT TERM
  if [[ "$finalized" == 1 ]]; then exit "$exit_code"; fi
  if [[ -z "$failure_reason" && "$exit_code" -ne 0 ]]; then
    failure_reason="acceptance failed during $phase"
  fi
  result_phase="$phase"
  cleanup_all
  if [[ "$exit_code" -eq 0 && "$test_status" == passed && "$cleanup_failed" == 0 ]]; then
    final_status='passed'
  elif [[ "$exit_code" -eq 0 ]]; then
    exit_code=1
  fi
  phase='complete'
  if ! write_evidence "$final_status" "$exit_code"; then
    printf 'Failed to write bounded evidence to %s\n' "$evidence_file" >&2
    exit_code=1
  fi
  finalized=1
  if [[ -n "$temp_dir" && -d "$temp_dir" ]]; then rm -rf "$temp_dir"; fi
  exit "$exit_code"
}
trap on_exit EXIT
trap 'failure_reason="acceptance interrupted"; exit 130' INT TERM

require_command docker
require_command go
require_command git
require_command jq
require_command kubectl
require_command mktemp
require_command node
require_command openssl
require_command shasum
command -v "$kind_bin" >/dev/null 2>&1 || \
  fail 'kind is required; set KIND_BIN to an explicit binary path if it is not on PATH'
[[ -n "$evidence_file" ]] || fail 'SYNARA_DUAL_CLUSTER_EVIDENCE_FILE must be an explicit JSON output path'
[[ ! -e "$evidence_file" ]] || fail "evidence path already exists: $evidence_file"
[[ -d "$(dirname "$evidence_file")" ]] || \
  fail "evidence parent directory does not exist: $(dirname "$evidence_file")"
if [[ "$allow_non_orbstack" != 1 && "$primary_context" != orbstack ]]; then
  fail 'primary context defaults to orbstack; set SYNARA_DUAL_CLUSTER_ALLOW_NON_ORBSTACK_PRIMARY=1 to opt in to another context'
fi
[[ "$primary_context" != "$secondary_context" ]] || fail 'primary and secondary contexts must be distinct'
validate_name "$secondary_cluster" 'Kind cluster name'
validate_context_label "$primary_context"
validate_dns_hostname "$primary_host_alias" 'primary Worker API host alias'
validate_dns_hostname "$secondary_host_alias" 'secondary Worker API host alias'
validate_name "$auth_namespace" 'acceptance auth namespace'
validate_name "$primary_target_namespace" 'primary target namespace'
validate_name "$secondary_target_namespace" 'secondary target namespace'
validate_name "$service_account" 'acceptance ServiceAccount name'
validate_name "$cluster_role" 'acceptance ClusterRole name'
validate_name "$role_binding" 'acceptance RoleBinding name'
validate_name "$namespace_cluster_role" 'acceptance namespace ClusterRole name'
validate_name "$namespace_cluster_role_binding" 'acceptance namespace ClusterRoleBinding name'
kubectl config get-contexts -o name | grep -Fxq "$primary_context" || \
  fail "primary context does not exist: $primary_context"
if "$kind_bin" get clusters | grep -Fxq "$secondary_cluster"; then
  fail "Kind cluster already exists; refusing to reuse it: $secondary_cluster"
fi
if docker image inspect "$worker_image" >/dev/null 2>&1; then
  fail "Worker image tag already exists; refusing to reuse it: $worker_image"
fi

source_sha="$(git -C "$repo_root" rev-parse HEAD)"
if [[ ! "$source_sha" =~ ^[0-9a-f]{40}$ && ! "$source_sha" =~ ^[0-9a-f]{64}$ ]]; then
  fail 'repository HEAD did not resolve to a full hexadecimal object ID'
fi
if [[ -n "$(git -C "$repo_root" status --porcelain --untracked-files=all)" ]]; then
  worktree_dirty=true
fi

initial_current_context="$(kubectl config current-context 2>/dev/null || true)"
temp_dir="$(mktemp -d "${TMPDIR:-/tmp}/synara-dual-cluster.XXXXXX")"
secondary_kubeconfig="$temp_dir/kind.kubeconfig"
detail_file="$temp_dir/test-detail.json"
kind_marker="$temp_dir/kind-owner"
worker_metadata_file="$temp_dir/worker-build-metadata.json"
if ! (set -o noclobber; : >"$evidence_file") 2>/dev/null; then
  fail "could not exclusively reserve evidence path: $evidence_file"
fi
evidence_reserved=1

phase='build-worker-acceptance-image'
worker_build_attempted=1
"$repo_root/deploy/worker/build.sh" \
  --target worker-acceptance \
  --image "$worker_image" \
  --metadata-file "$worker_metadata_file" \
  --allow-dirty \
  --load \
  --label "synara.io/acceptance-run-id=$run_id"
worker_image_json="$(docker image inspect "$worker_image")"
worker_config_id="$(jq -er '.[0].Id' <<<"$worker_image_json")"
worker_manifest_digest="$(jq -er '."containerimage.digest"' "$worker_metadata_file")"
worker_image_owner="$(jq -er '.[0].Config.Labels["synara.io/acceptance-run-id"]' <<<"$worker_image_json")"
worker_image_revision="$(jq -er '.[0].Config.Labels["org.opencontainers.image.revision"]' <<<"$worker_image_json")"
if [[ "$worker_image_owner" != "$run_id" || ! "$worker_config_id" =~ ^sha256:[0-9a-f]{64}$ || \
  ! "$worker_manifest_digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
  fail 'built Worker image did not retain exact run owner, config ID, and manifest digest'
fi
worker_identity_verified=1
worker_runtime_image_ids="$worker_config_id,$worker_manifest_digest"
if [[ "$worker_image_revision" != "$source_sha" ]]; then
  fail 'built Worker image did not retain the exact source revision'
fi

phase='create-secondary-cluster'
created_cluster=1
printf '%s:%s' "$run_id" "$secondary_cluster" >"$kind_marker"
KUBECONFIG="$secondary_kubeconfig" "$kind_bin" create cluster \
  --name "$secondary_cluster" --image "$kind_node_image" --wait 180s

phase='load-worker-acceptance-image'
KUBECONFIG="$secondary_kubeconfig" "$kind_bin" load docker-image \
  --name "$secondary_cluster" "$worker_image"

phase='create-primary-auth'
create_auth_resources primary
phase='create-secondary-auth'
create_auth_resources secondary
phase='collect-cluster-connections'
extract_connection primary
extract_connection secondary
[[ "$primary_api_server" != "$secondary_api_server" ]] || \
  fail 'primary and secondary contexts resolve to the same Kubernetes API server'
collect_cluster_metadata primary
collect_cluster_metadata secondary

phase='run-go-integration-test'
if (cd "$control_plane_dir" && env \
  SYNARA_DUAL_CLUSTER_INTEGRATION_PRIMARY_API_SERVER="$primary_api_server" \
  SYNARA_DUAL_CLUSTER_INTEGRATION_PRIMARY_TOKEN="$primary_token" \
  SYNARA_DUAL_CLUSTER_INTEGRATION_PRIMARY_CA="$primary_ca" \
  SYNARA_DUAL_CLUSTER_INTEGRATION_PRIMARY_NAMESPACE="$primary_target_namespace" \
  SYNARA_DUAL_CLUSTER_INTEGRATION_PRIMARY_NAMESPACE_UID="$primary_target_namespace_uid" \
  SYNARA_DUAL_CLUSTER_INTEGRATION_PRIMARY_CONTEXT_LABEL="$primary_context" \
  SYNARA_DUAL_CLUSTER_INTEGRATION_PRIMARY_WORKER_API_HOST_ALIAS="$primary_host_alias" \
  SYNARA_DUAL_CLUSTER_INTEGRATION_SECONDARY_API_SERVER="$secondary_api_server" \
  SYNARA_DUAL_CLUSTER_INTEGRATION_SECONDARY_TOKEN="$secondary_token" \
  SYNARA_DUAL_CLUSTER_INTEGRATION_SECONDARY_CA="$secondary_ca" \
  SYNARA_DUAL_CLUSTER_INTEGRATION_SECONDARY_NAMESPACE="$secondary_target_namespace" \
  SYNARA_DUAL_CLUSTER_INTEGRATION_SECONDARY_NAMESPACE_UID="$secondary_target_namespace_uid" \
  SYNARA_DUAL_CLUSTER_INTEGRATION_SECONDARY_CONTEXT_LABEL="$secondary_context" \
  SYNARA_DUAL_CLUSTER_INTEGRATION_SECONDARY_WORKER_API_HOST_ALIAS="$secondary_host_alias" \
  SYNARA_DUAL_CLUSTER_INTEGRATION_EVIDENCE_DETAIL_FILE="$detail_file" \
  SYNARA_DUAL_CLUSTER_INTEGRATION_RUN_ID="$run_id" \
  SYNARA_DUAL_CLUSTER_INTEGRATION_WORKER_IMAGE="$worker_image" \
  SYNARA_DUAL_CLUSTER_INTEGRATION_WORKER_IMAGE_ID="$worker_runtime_image_ids" \
  go test -v ./internal/sessions -run "^${test_name}$" -count=1); then
  test_status='passed'
  test_exit_code=0
else
  test_exit_code="$?"
  test_status='failed'
  failure_reason="Go integration test failed with exit code $test_exit_code"
fi
finalize_detail
if [[ "$detail_produced" != true || "$detail_valid" != true || \
  "$detail_bounded" != true || "$detail_secret_free" != true || \
  "$detail_assertions_valid" != true ]]; then
  [[ -n "$failure_reason" ]] || failure_reason='Go integration test did not produce valid bounded detail evidence'
  test_status='failed'
  [[ "$test_exit_code" -ne 0 ]] || test_exit_code=1
fi
if [[ "$test_status" != passed ]]; then exit "$test_exit_code"; fi
phase='test-complete'
exit 0
