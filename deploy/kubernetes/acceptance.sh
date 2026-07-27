#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
context="${SYNARA_K8S_CONTEXT:-$(kubectl config current-context 2>/dev/null || true)}"
namespace="${SYNARA_K8S_NAMESPACE:-synara-system}"
rbac_name="${SYNARA_K8S_ACCEPTANCE_RBAC_NAME:-}"
acceptance_owner="${SYNARA_K8S_ACCEPTANCE_OWNER:-acceptance-$(date +%s)-$$}"
image="${SYNARA_K8S_ACCEPTANCE_IMAGE:-synara-control-plane:stage2-acceptance}"
dependency_node_selector_key="${SYNARA_K8S_ACCEPTANCE_DEPENDENCY_NODE_SELECTOR_KEY:-}"
dependency_node_selector_value="${SYNARA_K8S_ACCEPTANCE_DEPENDENCY_NODE_SELECTOR_VALUE:-}"
worker_protocol_version=2
worker_version="${SYNARA_ACCEPTANCE_WORKER_VERSION:-acceptance}"
work_dir="$(mktemp -d)"
port_forward_pid=""
created_namespace=0
cleanup_rbac=0

cleanup() {
  if [[ -n "$port_forward_pid" ]]; then
    kill "$port_forward_pid" >/dev/null 2>&1 || true
    wait "$port_forward_pid" >/dev/null 2>&1 || true
  fi
  if [[ "${SYNARA_K8S_KEEP_RESOURCES:-0}" != "1" && -n "$context" ]]; then
    if [[ "$created_namespace" == "1" ]]; then
      kubectl --context "$context" delete namespace "$namespace" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    fi
    if [[ "$cleanup_rbac" == "1" && -n "$rbac_name" ]]; then
      kubectl --context "$context" delete clusterrolebinding "$rbac_name" --ignore-not-found >/dev/null 2>&1 || true
      kubectl --context "$context" delete clusterrole "$rbac_name" --ignore-not-found >/dev/null 2>&1 || true
    fi
  fi
  rm -rf "$work_dir"
}
trap cleanup EXIT
trap 'status=$?; printf "Kubernetes Stage 2 acceptance stopped at line %s with status %s\n" "$LINENO" "$status" >&2; exit "$status"' ERR

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf '%s is required\n' "$1" >&2
    exit 1
  fi
}

require_command curl
require_command jq
require_command kubectl
require_command openssl
require_command python3

list_ready_control_plane_pods() {
  "${kube[@]}" -n "$namespace" get pods \
    -l app.kubernetes.io/name=synara-control-plane -o json | jq -r '
      .items[]
      | select(.metadata.deletionTimestamp == null)
      | select(.status.phase == "Running")
      | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))
      | "pod/\(.metadata.name)"
    ' | sort
}

wait_for_control_plane_ready_replicas() {
  local expected_replicas="$1"
  local timeout_seconds="${2:-180}"
  local deadline=$((SECONDS + timeout_seconds))
  while (( SECONDS < deadline )); do
    local ready_replicas
    ready_replicas="$("${kube[@]}" -n "$namespace" get deployment synara-control-plane \
      -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)"
    local ready_pod_count=0
    while IFS= read -r pod; do
      [[ -n "$pod" ]] && ready_pod_count=$((ready_pod_count + 1))
    done < <(list_ready_control_plane_pods)
    if [[ "$ready_replicas" == "$expected_replicas" && "$ready_pod_count" == "$expected_replicas" ]]; then
      return 0
    fi
    sleep 1
  done
  printf 'Control Plane did not return to %s Ready replicas within %ss\n' \
    "$expected_replicas" "$timeout_seconds" >&2
  return 1
}

wait_for_namespace_absent() {
  local timeout_seconds="${1:-180}"
  local deadline=$((SECONDS + timeout_seconds))
  while (( SECONDS < deadline )); do
    local deletion_timestamp=""
    if ! deletion_timestamp="$("${kube[@]}" get namespace "$namespace" \
      -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null)"; then
      return 0
    fi
    if [[ -z "$deletion_timestamp" ]]; then
      printf 'Namespace %s already exists and is not terminating; refusing to reuse it\n' \
        "$namespace" >&2
      return 1
    fi
    sleep 1
  done
  printf 'Namespace %s is still terminating after %ss\n' "$namespace" "$timeout_seconds" >&2
  return 1
}

read_reconciler_lease() {
  local lease_name="$1"
  if [[ ! "$lease_name" =~ ^[A-Za-z0-9:._-]+$ ]]; then
    printf 'Invalid reconciler lease name %s\n' "$lease_name" >&2
    return 1
  fi
  "${kube[@]}" -n "$namespace" exec deployment/synara-stage2-postgres -- \
    psql -U synara -d synara -v ON_ERROR_STOP=1 -At -F '|' \
      -c "SELECT holder_id, fencing_token FROM reconciler_leases WHERE lease_name = '$lease_name' AND expires_at > clock_timestamp()"
}

wait_for_reconciler_lease() {
  local lease_name="$1"
  local timeout_seconds="${2:-60}"
  local deadline=$((SECONDS + timeout_seconds))
  local lease_row=""
  while (( SECONDS < deadline )); do
    lease_row="$(read_reconciler_lease "$lease_name" 2>/dev/null || true)"
    if [[ "$lease_row" =~ ^[^|]+\|[1-9][0-9]*$ ]]; then
      printf '%s\n' "$lease_row"
      return 0
    fi
    sleep 1
  done
  printf 'Reconciler lease %s did not become active within %ss\n' "$lease_name" "$timeout_seconds" >&2
  return 1
}

postgres_scalar() {
  local query="$1"
  local attempts="${2:-10}"
  local output=""
  local stderr_file="$work_dir/postgres-scalar.stderr"
  for ((attempt = 1; attempt <= attempts; attempt += 1)); do
    if output="$("${kube[@]}" -n "$namespace" exec deployment/synara-stage2-postgres -- \
      psql -U synara -d synara -v ON_ERROR_STOP=1 -At -c "$query" 2>"$stderr_file")"; then
      printf '%s\n' "$output"
      return 0
    fi
    sleep 1
  done
  cat "$stderr_file" >&2 2>/dev/null || true
  printf 'PostgreSQL acceptance probe failed after %s attempts\n' "$attempts" >&2
  return 1
}

kube_raw_get() {
  local path="$1"
  local attempts="${2:-10}"
  local output=""
  local stderr_file="$work_dir/kube-raw-get.stderr"
  for ((attempt = 1; attempt <= attempts; attempt += 1)); do
    if output="$("${kube[@]}" get --raw "$path" 2>"$stderr_file")"; then
      printf '%s\n' "$output"
      return 0
    fi
    sleep 1
  done
  cat "$stderr_file" >&2 2>/dev/null || true
  printf 'Kubernetes raw API probe failed after %s attempts for %s\n' "$attempts" "$path" >&2
  return 1
}

if [[ -n "$dependency_node_selector_key" || -n "$dependency_node_selector_value" ]]; then
  if [[ -z "$dependency_node_selector_key" || -z "$dependency_node_selector_value" ]]; then
    printf 'Dependency node selector key and value must be configured together\n' >&2
    exit 1
  fi
  if [[ ! "$dependency_node_selector_key" =~ ^[A-Za-z0-9./_-]+$ ]] ||
    [[ ! "$dependency_node_selector_value" =~ ^[A-Za-z0-9._-]+$ ]]; then
    printf 'Dependency node selector key or value contains unsupported characters\n' >&2
    exit 1
  fi
  dependency_node_selector_yaml="$(printf '      nodeSelector:\n        %s: "%s"' "$dependency_node_selector_key" "$dependency_node_selector_value")"
else
  dependency_node_selector_yaml=""
fi

if [[ -z "$context" ]]; then
  printf 'A Kubernetes context is required through SYNARA_K8S_CONTEXT or current-context\n' >&2
  exit 1
fi
if [[ ! "$namespace" =~ ^synara-[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || (( ${#namespace} > 63 )); then
  printf 'SYNARA_K8S_NAMESPACE must be a synara-* DNS label no longer than 63 characters\n' >&2
  exit 1
fi
if [[ -z "$rbac_name" ]]; then
  if [[ "$namespace" == "synara-system" ]]; then
    rbac_name="synara-control-plane-reconciler"
  else
    rbac_name="synara-control-plane-reconciler-$namespace"
  fi
fi
if [[ ! "$rbac_name" =~ ^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$ ]] || (( ${#rbac_name} > 253 )); then
  printf 'SYNARA_K8S_ACCEPTANCE_RBAC_NAME must be a DNS subdomain no longer than 253 characters\n' >&2
  exit 1
fi
if [[ ! "$acceptance_owner" =~ ^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$ ]] || (( ${#acceptance_owner} > 63 )); then
  printf 'SYNARA_K8S_ACCEPTANCE_OWNER must be a Kubernetes label value no longer than 63 characters\n' >&2
  exit 1
fi
if [[ "$context" != kind-* && "${SYNARA_K8S_ACCEPTANCE_ALLOW_NONDISPOSABLE:-0}" != "1" ]]; then
  printf 'Refusing to run destructive acceptance against non-Kind context %s\n' "$context" >&2
  printf 'Set SYNARA_K8S_ACCEPTANCE_ALLOW_NONDISPOSABLE=1 only for an explicitly disposable cluster\n' >&2
  exit 1
fi

kube=(kubectl --context "$context")
"${kube[@]}" cluster-info >/dev/null
wait_for_namespace_absent 180
if "${kube[@]}" get clusterrole "$rbac_name" >/dev/null 2>&1 ||
  "${kube[@]}" get clusterrolebinding "$rbac_name" >/dev/null 2>&1; then
  printf 'RBAC identity %s already exists; refusing to overwrite it\n' "$rbac_name" >&2
  exit 1
fi

run_id="$(date +%s)-$$"
postgres_password="stage2-postgres-$run_id-$(openssl rand -hex 8)"
minio_user="stage2-minio-$run_id"
minio_password="stage2-minio-secret-$run_id-$(openssl rand -hex 8)"
worker_registration_token="stage2-worker-$run_id-$(openssl rand -hex 8)"
provider_cursor_key="$(openssl rand -base64 32 | tr -d '\n')"
credential_master_key="$(openssl rand -base64 32 | tr -d '\n')"

"${kube[@]}" create namespace "$namespace" >/dev/null
created_namespace=1
"${kube[@]}" label namespace "$namespace" \
  "synara.ai/acceptance-owner=$acceptance_owner" >/dev/null
"${kube[@]}" -n "$namespace" create secret generic synara-stage2-dependencies \
  --from-literal=POSTGRES_PASSWORD="$postgres_password" \
  --from-literal=MINIO_ROOT_USER="$minio_user" \
  --from-literal=MINIO_ROOT_PASSWORD="$minio_password" \
  --dry-run=client -o yaml | "${kube[@]}" apply -f - >/dev/null
"${kube[@]}" -n "$namespace" create secret generic synara-control-plane-acceptance-env \
  --from-literal=SYNARA_CREDENTIAL_MASTER_KEY="$credential_master_key" \
  --from-literal=SYNARA_ARTIFACT_ACCESS_KEY_ID="$minio_user" \
  --from-literal=SYNARA_ARTIFACT_SECRET_ACCESS_KEY="$minio_password" \
  --from-literal=AWS_ACCESS_KEY_ID="$minio_user" \
  --from-literal=AWS_SECRET_ACCESS_KEY="$minio_password" \
  --dry-run=client -o yaml | "${kube[@]}" apply -f - >/dev/null

"${kube[@]}" -n "$namespace" apply -f - >/dev/null <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: synara-stage2-postgres
spec:
  accessModes: ["ReadWriteOnce"]
  resources:
    requests:
      storage: 1Gi
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: synara-stage2-postgres
  labels:
    app.kubernetes.io/name: synara-stage2-postgres
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: synara-stage2-postgres
  template:
    metadata:
      labels:
        app.kubernetes.io/name: synara-stage2-postgres
    spec:
${dependency_node_selector_yaml}
      containers:
        - name: postgres
          image: postgres:17-alpine
          env:
            - name: POSTGRES_DB
              value: synara
            - name: POSTGRES_USER
              value: synara
            - name: POSTGRES_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: synara-stage2-dependencies
                  key: POSTGRES_PASSWORD
          ports:
            - name: postgres
              containerPort: 5432
          readinessProbe:
            exec:
              command: ["pg_isready", "-U", "synara", "-d", "synara"]
            periodSeconds: 2
            timeoutSeconds: 2
          volumeMounts:
            - name: data
              mountPath: /var/lib/postgresql/data
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: synara-stage2-postgres
---
apiVersion: v1
kind: Service
metadata:
  name: synara-stage2-postgres
spec:
  selector:
    app.kubernetes.io/name: synara-stage2-postgres
  ports:
    - name: postgres
      port: 5432
      targetPort: postgres
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: synara-stage2-minio
spec:
  accessModes: ["ReadWriteOnce"]
  resources:
    requests:
      storage: 1Gi
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: synara-stage2-minio
  labels:
    app.kubernetes.io/name: synara-stage2-minio
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: synara-stage2-minio
  template:
    metadata:
      labels:
        app.kubernetes.io/name: synara-stage2-minio
    spec:
${dependency_node_selector_yaml}
      containers:
        - name: minio
          image: minio/minio:RELEASE.2025-04-22T22-12-26Z
          args: ["server", "/data", "--console-address", ":9001"]
          envFrom:
            - secretRef:
                name: synara-stage2-dependencies
          ports:
            - name: api
              containerPort: 9000
          readinessProbe:
            httpGet:
              path: /minio/health/live
              port: api
            periodSeconds: 2
            timeoutSeconds: 2
          volumeMounts:
            - name: data
              mountPath: /data
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: synara-stage2-minio
---
apiVersion: v1
kind: Service
metadata:
  name: synara-stage2-minio
spec:
  selector:
    app.kubernetes.io/name: synara-stage2-minio
  ports:
    - name: api
      port: 9000
      targetPort: api
YAML

apply_bucket_job() {
  "${kube[@]}" -n "$namespace" apply -f - >/dev/null <<YAML
apiVersion: batch/v1
kind: Job
metadata:
  name: synara-stage2-create-bucket
spec:
  backoffLimit: 6
  template:
    metadata:
      labels:
        app.kubernetes.io/name: synara-stage2-create-bucket
    spec:
      restartPolicy: OnFailure
      containers:
        - name: mc
          image: minio/mc:RELEASE.2025-04-16T18-13-26Z
          envFrom:
            - secretRef:
                name: synara-stage2-dependencies
          command: ["/bin/sh", "-ec"]
          args:
            - |
              until mc alias set stage2 http://synara-stage2-minio:9000 "\$MINIO_ROOT_USER" "\$MINIO_ROOT_PASSWORD"; do sleep 1; done
              mc mb --ignore-existing stage2/synara-artifacts
YAML
}

"${kube[@]}" -n "$namespace" rollout status deployment/synara-stage2-postgres --timeout=180s
"${kube[@]}" -n "$namespace" rollout status deployment/synara-stage2-minio --timeout=180s
apply_bucket_job
"${kube[@]}" -n "$namespace" wait --for=condition=complete job/synara-stage2-create-bucket --timeout=120s

database_url="postgres://synara:$postgres_password@synara-stage2-postgres.$namespace.svc.cluster.local:5432/synara?sslmode=disable"
"${kube[@]}" -n "$namespace" create configmap synara-control-plane-config \
  --from-literal=public-control-plane-url=https://synara-control-plane.test \
  --from-literal=trusted-proxy-cidrs= \
  --from-literal=artifact-bucket=synara-artifacts \
  --from-literal=artifact-region=us-east-1 \
  --from-literal=artifact-endpoint="http://synara-stage2-minio.$namespace.svc.cluster.local:9000" \
  --from-literal=artifact-public-endpoint="http://synara-stage2-minio.$namespace.svc.cluster.local:9000" \
  --from-literal=artifact-use-path-style=true \
  --from-literal=billing-tariff-operator-tenant-id= \
  --from-literal=billing-blob-source=disabled \
  --from-literal=platform-routing-publishers-json='[]' \
  --dry-run=client -o yaml | "${kube[@]}" apply -f - >/dev/null
"${kube[@]}" -n "$namespace" create secret generic synara-control-plane-secrets \
  --from-literal=database-url="$database_url" \
  --from-literal=worker-registration-token="$worker_registration_token" \
  --from-literal=provider-cursor-key="$provider_cursor_key" \
  --from-literal=credential-master-key="$credential_master_key" \
  --from-literal=artifact-access-key-id="$minio_user" \
  --from-literal=artifact-secret-access-key="$minio_password" \
  --dry-run=client -o yaml | "${kube[@]}" apply -f - >/dev/null

overlay_dir="$work_dir/kustomize"
mkdir -p "$overlay_dir"
cp \
  "$script_dir/service-account.yaml" \
  "$script_dir/rbac.yaml" \
  "$script_dir/pod-disruption-budget.yaml" \
  "$script_dir/deployment.yaml" \
  "$script_dir/service.yaml" \
  "$overlay_dir/"
cat >"$overlay_dir/kustomization.yaml" <<YAML
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: $namespace
resources:
  - service-account.yaml
  - rbac.yaml
  - pod-disruption-budget.yaml
  - deployment.yaml
  - service.yaml
patches:
  - target:
      group: rbac.authorization.k8s.io
      version: v1
      kind: ClusterRole
      name: synara-control-plane-reconciler
    patch: |-
      - op: replace
        path: /metadata/name
        value: $rbac_name
      - op: add
        path: /metadata/labels/synara.ai~1acceptance-owner
        value: $acceptance_owner
  - target:
      group: rbac.authorization.k8s.io
      version: v1
      kind: ClusterRoleBinding
      name: synara-control-plane-reconciler
    patch: |-
      - op: replace
        path: /metadata/name
        value: $rbac_name
      - op: replace
        path: /roleRef/name
        value: $rbac_name
      - op: replace
        path: /subjects/0/namespace
        value: $namespace
      - op: add
        path: /metadata/labels/synara.ai~1acceptance-owner
        value: $acceptance_owner
YAML
cleanup_rbac=1
"${kube[@]}" create -k "$overlay_dir" >/dev/null
"${kube[@]}" -n "$namespace" set image deployment/synara-control-plane control-plane="$image" >/dev/null
"${kube[@]}" -n "$namespace" set env deployment/synara-control-plane \
  --from=secret/synara-control-plane-acceptance-env >/dev/null
"${kube[@]}" -n "$namespace" set env deployment/synara-control-plane \
  SYNARA_CREDENTIAL_KMS_PROVIDER=local \
  SYNARA_ARTIFACT_ENDPOINT=http://synara-stage2-minio.$namespace.svc.cluster.local:9000 \
  SYNARA_ARTIFACT_PUBLIC_ENDPOINT=http://synara-stage2-minio.$namespace.svc.cluster.local:9000 \
  SYNARA_ARTIFACT_USE_PATH_STYLE=true \
  AWS_REGION=us-east-1 \
  AWS_EC2_METADATA_DISABLED=true \
  SYNARA_KUBERNETES_RECONCILE_INTERVAL=1s \
  SYNARA_RECONCILER_LEASE_TTL=12s \
  SYNARA_RECONCILER_LEASE_RENEW_INTERVAL=2s \
  SYNARA_RECONCILER_LEASE_ASSERT_INTERVAL=1s \
  SYNARA_RECONCILER_LEASE_ACQUIRE_RETRY_DELAY=1s \
  SYNARA_OUTBOX_POLL_INTERVAL=100ms >/dev/null

"${kube[@]}" -n "$namespace" rollout status deployment/synara-control-plane --timeout=240s
ready_replicas="$("${kube[@]}" -n "$namespace" get deployment synara-control-plane -o jsonpath='{.status.readyReplicas}')"
[[ "$ready_replicas" == "2" ]]
printf 'Kubernetes dependencies and two Control Plane replicas are ready\n'

proxy_path="/api/v1/namespaces/$namespace/services/http:synara-control-plane:3780/proxy"
ready_json="$(kube_raw_get "$proxy_path/ready")"
jq -e '.status == "ready" and .checks.database.status == "ready" and .checks.schema.status == "ready"' \
  <<<"$ready_json" >/dev/null
metrics="$(kube_raw_get "$proxy_path/metrics")"
grep -q '^synara_database_connections' <<<"$metrics"
grep -q '^synara_outbox_pending' <<<"$metrics"

migration_count="$(postgres_scalar 'SELECT count(*) FROM control_plane_schema_migrations')"
expected_migration_count="$(jq -er '.checks.schema.expectedVersion | select(type == "number" and . > 0)' <<<"$ready_json")"
[[ "$migration_count" == "$expected_migration_count" ]]

local_target_id="$(postgres_scalar "SELECT id FROM execution_targets WHERE tenant_id IS NULL AND kind = 'local' ORDER BY created_at LIMIT 1")"
[[ -n "$local_target_id" ]]
local_target_capabilities="$(postgres_scalar "SELECT capabilities::text FROM execution_targets WHERE id = '$local_target_id'")"
worker_capabilities="$(python3 "$repo_root/scripts/stage3-provider-acceptance/worker_manifest.py" \
  --worker-version "$worker_version" \
  --target-capabilities-json "$local_target_capabilities")"

pick_port() {
  python3 -c 'import socket; sock = socket.socket(); sock.bind(("127.0.0.1", 0)); print(sock.getsockname()[1]); sock.close()'
}

start_port_forward() {
  if [[ -n "$port_forward_pid" ]]; then
    kill "$port_forward_pid" >/dev/null 2>&1 || true
    wait "$port_forward_pid" >/dev/null 2>&1 || true
  fi
  local_port="$(pick_port)"
  "${kube[@]}" -n "$namespace" port-forward service/synara-control-plane "$local_port:3780" \
    >"$work_dir/port-forward.log" 2>&1 &
  port_forward_pid="$!"
  for _ in {1..50}; do
    if curl -sS --fail --max-time 2 "http://127.0.0.1:$local_port/health" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.1
  done
  printf 'Control Plane port-forward did not become ready\n' >&2
  return 1
}

start_port_forward
worker_instance_uid="$(python3 -c 'import uuid; print(uuid.uuid4())')"
worker_registration="$(curl -sS --fail-with-body -X POST \
  -H "Authorization: Bearer $worker_registration_token" \
  -H "X-Request-ID: k8s-register-$run_id" \
  -H 'Content-Type: application/json' \
  -d "{\"executionTargetId\":\"$local_target_id\",\"targetKind\":\"local\",\"instanceUid\":\"$worker_instance_uid\",\"clusterId\":\"stage2-kind\",\"namespace\":\"$namespace\",\"podName\":\"stage2-worker-$run_id\",\"version\":\"$worker_version\",\"protocolVersion\":$worker_protocol_version,\"capabilities\":$worker_capabilities,\"leaseSupported\":true,\"fencingSupported\":true}" \
  "http://127.0.0.1:$local_port/v1/workers/register")"
worker_token="$(jq -er '.token' <<<"$worker_registration")"
worker_id="$(jq -er '.worker.id' <<<"$worker_registration")"
curl -sS --fail-with-body -X POST \
  -H "Authorization: Bearer $worker_token" -H "X-Request-ID: k8s-heartbeat-$run_id" \
  -H 'Content-Type: application/json' \
  -d "{\"version\":\"$worker_version\",\"protocolVersion\":$worker_protocol_version,\"capabilities\":$worker_capabilities}" \
  "http://127.0.0.1:$local_port/v1/workers/heartbeat" >/dev/null

pods_before=()
while IFS= read -r pod; do
  [[ -n "$pod" ]] && pods_before+=("$pod")
done < <(list_ready_control_plane_pods)
[[ "${#pods_before[@]}" == "2" ]]

required_reconciler_leases=(
  synara:docker-worker-pool-reconciler
  synara:kubernetes-execution-reconciler
  synara:global-target-failover-sweep
  synara:session-resource-lifecycle
  synara:worker-release-auto-rollback
  synara:tenant-retention-sweeper
)
for required_lease in "${required_reconciler_leases[@]}"; do
  wait_for_reconciler_lease "$required_lease" 60 >/dev/null
done
for pod in "${pods_before[@]}"; do
  "${kube[@]}" -n "$namespace" logs --pod-running-timeout=60s "$pod" >>"$work_dir/control-plane-before.log"
done

availability_file="$work_dir/pod-delete-failures"
(
  failures=0
  for _ in {1..30}; do
    if ! "${kube[@]}" get --raw "$proxy_path/ready" 2>/dev/null | jq -e '.status == "ready"' >/dev/null; then
      failures=$((failures + 1))
    fi
    sleep 0.2
  done
  printf '%s\n' "$failures" >"$availability_file"
) &
availability_pid="$!"
"${kube[@]}" -n "$namespace" delete "${pods_before[0]}" --wait=false >/dev/null
wait "$availability_pid"
[[ "$(<"$availability_file")" == "0" ]]
"${kube[@]}" -n "$namespace" wait --for=delete "${pods_before[0]}" --timeout=90s >/dev/null
wait_for_control_plane_ready_replicas 2 180
pods_after=()
while IFS= read -r pod; do
  [[ -n "$pod" ]] && pods_after+=("$pod")
done < <("${kube[@]}" -n "$namespace" get pods \
  -l app.kubernetes.io/name=synara-control-plane -o name | sort)
[[ "${#pods_after[@]}" == "2" ]]
if [[ " ${pods_after[*]} " == *" ${pods_before[0]} "* ]]; then
  printf 'Deleted Control Plane Pod was not replaced\n' >&2
  exit 1
fi
printf 'Control Plane Pod deletion passed without readiness interruption\n'

start_port_forward
curl -sS --fail-with-body -X POST \
  -H "Authorization: Bearer $worker_token" -H "X-Request-ID: post-delete-heartbeat-$run_id" \
  -H 'Content-Type: application/json' \
  -d "{\"version\":\"$worker_version\",\"protocolVersion\":$worker_protocol_version,\"capabilities\":$worker_capabilities}" \
  "http://127.0.0.1:$local_port/v1/workers/heartbeat" >/dev/null
printf 'Worker token remained valid after Pod replacement\n'

"${kube[@]}" -n "$namespace" scale deployment/synara-stage2-postgres --replicas=0 >/dev/null
"${kube[@]}" -n "$namespace" wait --for=delete pod \
  -l app.kubernetes.io/name=synara-stage2-postgres --timeout=90s >/dev/null
for _ in {1..60}; do
  ready_replicas="$("${kube[@]}" -n "$namespace" get deployment synara-control-plane -o jsonpath='{.status.readyReplicas}')"
  if [[ -z "$ready_replicas" || "$ready_replicas" == "0" ]]; then
    break
  fi
  sleep 1
done
ready_replicas="$("${kube[@]}" -n "$namespace" get deployment synara-control-plane -o jsonpath='{.status.readyReplicas}')"
[[ -z "$ready_replicas" || "$ready_replicas" == "0" ]]

"${kube[@]}" -n "$namespace" scale deployment/synara-stage2-postgres --replicas=1 >/dev/null
"${kube[@]}" -n "$namespace" rollout status deployment/synara-stage2-postgres --timeout=180s
wait_for_control_plane_ready_replicas 2 180
ready_json="$(kube_raw_get "$proxy_path/ready")"
jq -e '.status == "ready"' <<<"$ready_json" >/dev/null
printf 'PostgreSQL outage and readiness recovery passed\n'

start_port_forward
curl -sS --fail-with-body -X POST \
  -H "Authorization: Bearer $worker_token" -H "X-Request-ID: post-db-heartbeat-$run_id" \
  -H 'Content-Type: application/json' \
  -d "{\"version\":\"$worker_version\",\"protocolVersion\":$worker_protocol_version,\"capabilities\":$worker_capabilities}" \
  "http://127.0.0.1:$local_port/v1/workers/heartbeat" >/dev/null

"${kube[@]}" -n "$namespace" scale deployment/synara-stage2-minio --replicas=0 >/dev/null
"${kube[@]}" -n "$namespace" wait --for=delete pod \
  -l app.kubernetes.io/name=synara-stage2-minio --timeout=90s >/dev/null
for _ in {1..60}; do
  ready_replicas="$("${kube[@]}" -n "$namespace" get deployment synara-control-plane -o jsonpath='{.status.readyReplicas}')"
  if [[ -z "$ready_replicas" || "$ready_replicas" == "0" ]]; then
    break
  fi
  sleep 1
done
ready_replicas="$("${kube[@]}" -n "$namespace" get deployment synara-control-plane -o jsonpath='{.status.readyReplicas}')"
[[ -z "$ready_replicas" || "$ready_replicas" == "0" ]]

"${kube[@]}" -n "$namespace" scale deployment/synara-stage2-minio --replicas=1 >/dev/null
"${kube[@]}" -n "$namespace" rollout status deployment/synara-stage2-minio --timeout=180s
"${kube[@]}" -n "$namespace" delete job synara-stage2-create-bucket --ignore-not-found >/dev/null
apply_bucket_job
"${kube[@]}" -n "$namespace" wait --for=condition=complete job/synara-stage2-create-bucket --timeout=120s
wait_for_control_plane_ready_replicas 2 180
ready_json="$(kube_raw_get "$proxy_path/ready")"
jq -e '.status == "ready"' <<<"$ready_json" >/dev/null
printf 'MinIO outage and readiness recovery passed\n'

current_pods=()
while IFS= read -r pod; do
  [[ -n "$pod" ]] && current_pods+=("$pod")
done < <(list_ready_control_plane_pods)
[[ "${#current_pods[@]}" == "2" ]]
for pod in "${current_pods[@]}"; do
  "${kube[@]}" -n "$namespace" logs --pod-running-timeout=60s "$pod" >>"$work_dir/control-plane-after.log"
done
logs_file="$work_dir/control-plane-combined.log"
cp "$work_dir/control-plane-before.log" "$logs_file"
cat "$work_dir/control-plane-after.log" >>"$logs_file"
sensitive_values=(
  "$postgres_password"
  "$minio_password"
  "$worker_registration_token"
  "$provider_cursor_key"
  "$credential_master_key"
  "$worker_token"
)
for sensitive_value in "${sensitive_values[@]}"; do
  if grep -Fq -- "$sensitive_value" "$logs_file"; then
    printf 'Sensitive value was found in Kubernetes Control Plane logs\n' >&2
    exit 1
  fi
done
if grep -Fq 'X-Amz-Signature=' "$logs_file"; then
  printf 'Presigned URL query was found in Kubernetes Control Plane logs\n' >&2
  exit 1
fi
printf 'Kubernetes sensitive-log audit passed\n'

"${kube[@]}" auth can-i create pods \
  --as="system:serviceaccount:$namespace:synara-control-plane" >/dev/null
"${kube[@]}" auth can-i create secrets \
  --as="system:serviceaccount:$namespace:synara-control-plane" >/dev/null
"${kube[@]}" auth can-i patch namespaces \
  --as="system:serviceaccount:$namespace:synara-control-plane" >/dev/null
"${kube[@]}" auth can-i create tokenreviews.authentication.k8s.io \
  --as="system:serviceaccount:$namespace:synara-control-plane" >/dev/null
"${kube[@]}" auth can-i get priorityclasses.scheduling.k8s.io \
  --as="system:serviceaccount:$namespace:synara-control-plane" >/dev/null
if "${kube[@]}" auth can-i create priorityclasses.scheduling.k8s.io \
  --as="system:serviceaccount:$namespace:synara-control-plane" >/dev/null; then
  printf 'Control Plane ServiceAccount unexpectedly can create PriorityClasses\n' >&2
  exit 1
fi
if "${kube[@]}" auth can-i delete namespaces \
  --as="system:serviceaccount:$namespace:synara-control-plane" >/dev/null; then
  printf 'Control Plane ServiceAccount unexpectedly can delete Namespaces\n' >&2
  exit 1
fi
if "${kube[@]}" auth can-i delete secrets --namespace "$namespace" \
  --as="system:serviceaccount:$namespace:synara-control-plane" >/dev/null; then
  printf 'Control Plane ServiceAccount unexpectedly can delete Secrets\n' >&2
  exit 1
fi
printf 'Kubernetes least-privilege RBAC spot audit passed\n'

printf 'Kubernetes Stage 2 acceptance passed: context=%s namespace=%s rbac=%s replicas=2 worker=%s migrations=%s\n' \
  "$context" "$namespace" "$rbac_name" "$worker_id" "$migration_count"
