#!/usr/bin/env bash
set -Eeuo pipefail

# This is an operator acceptance profile, not the enterprise production base.
# It deliberately runs PostgreSQL and MinIO in one namespace, enables the
# Control Plane development bootstrap, and keeps every Service ClusterIP-only.
# Production self-hosting must use deploy/kubernetes with external PostgreSQL,
# HTTPS ingress, an IdP/MFA, Secret-manager materialisation, and S3/workload
# identity as described in deploy/kubernetes/README.md.

namespace="${SYNARA_REMOTE_K8S_NAMESPACE:-synara-stage6-remote}"
context="${SYNARA_K8S_CONTEXT:-}"
keep_resources="${SYNARA_REMOTE_K8S_KEEP_RESOURCES:-0}"
allow_existing="${SYNARA_REMOTE_K8S_ALLOW_EXISTING:-0}"
dry_run=0
created_namespace=0
work_dir=""

usage() {
  cat <<'EOF'
Usage: deploy/kubernetes/remote-stage6-acceptance.sh [--dry-run] [--keep]

Required image inputs (prefer immutable @sha256 references):
  SYNARA_REMOTE_K8S_CONTROL_PLANE_IMAGE
  SYNARA_REMOTE_K8S_NODE_IMAGE

Optional inputs:
  SYNARA_REMOTE_K8S_POSTGRES_IMAGE (default: postgres:17-alpine)
  SYNARA_REMOTE_K8S_MINIO_IMAGE (default: minio/minio:RELEASE.2025-04-22T22-12-26Z)
  SYNARA_PLATFORM_OPERATOR_TENANT_ID (generated when omitted; provision it
    through the API before exercising Platform Admin authority)
  SYNARA_REMOTE_K8S_ROLLOUT_TIMEOUT (default: 5m)
  SYNARA_K8S_CONTEXT (default: current kubectl context)
  SYNARA_REMOTE_K8S_NAMESPACE (default: synara-stage6-remote)

The namespace is created only when absent. An existing namespace requires
SYNARA_REMOTE_K8S_ALLOW_EXISTING=1 and is never deleted by this script.
Without --keep, a namespace created by this run is deleted on exit.
EOF
}

cleanup() {
  local exit_code=$?
  if [[ -n "$work_dir" ]]; then
    rm -rf "$work_dir"
  fi
  if (( created_namespace == 1 )) && (( dry_run == 0 )) && [[ "$keep_resources" != "1" ]]; then
    kubectl_cmd delete namespace "$namespace" --ignore-not-found --wait=true >/dev/null 2>&1 || true
  fi
  exit "$exit_code"
}
trap cleanup EXIT
trap 'rc=$?; printf "remote Stage 6 acceptance stopped at line %s (status %s)\n" "$LINENO" "$rc" >&2; exit "$rc"' ERR

while (($# > 0)); do
  case "$1" in
    --dry-run)
      dry_run=1
      ;;
    --keep)
      keep_resources=1
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      printf 'unknown argument: %s\n' "$1" >&2
      usage >&2
      exit 2
      ;;
  esac
  shift
done

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf '%s is required\n' "$1" >&2
    exit 1
  fi
}

require_command kubectl
require_command openssl

if [[ -z "$context" ]]; then
  context="$(kubectl config current-context 2>/dev/null || true)"
fi
if [[ -z "$context" ]]; then
  printf 'SYNARA_K8S_CONTEXT or a current kubectl context is required\n' >&2
  exit 1
fi

if [[ ! "$namespace" =~ ^synara-[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || (( ${#namespace} > 63 )); then
  printf 'SYNARA_REMOTE_K8S_NAMESPACE must be a synara-* DNS label no longer than 63 characters\n' >&2
  exit 1
fi

control_plane_image="${SYNARA_REMOTE_K8S_CONTROL_PLANE_IMAGE:-}"
node_image="${SYNARA_REMOTE_K8S_NODE_IMAGE:-}"
postgres_image="${SYNARA_REMOTE_K8S_POSTGRES_IMAGE:-postgres:17-alpine}"
minio_image="${SYNARA_REMOTE_K8S_MINIO_IMAGE:-minio/minio:RELEASE.2025-04-22T22-12-26Z}"
rollout_timeout="${SYNARA_REMOTE_K8S_ROLLOUT_TIMEOUT:-5m}"

if [[ -z "$control_plane_image" || -z "$node_image" ]]; then
  printf 'SYNARA_REMOTE_K8S_CONTROL_PLANE_IMAGE and SYNARA_REMOTE_K8S_NODE_IMAGE are required\n' >&2
  exit 1
fi
if [[ "$control_plane_image" != *@sha256:* || "$node_image" != *@sha256:* ]]; then
  if [[ "${SYNARA_REMOTE_K8S_ALLOW_TAGGED_IMAGES:-0}" != "1" ]]; then
    printf 'Control Plane and Node images must use immutable @sha256 references; set SYNARA_REMOTE_K8S_ALLOW_TAGGED_IMAGES=1 only for a disposable lab\n' >&2
    exit 1
  fi
fi

kubectl_cmd=(kubectl --context "$context")
if (( dry_run == 0 )); then
  "${kubectl_cmd[@]}" cluster-info >/dev/null
fi

random_uuid() {
  local hex
  if command -v uuidgen >/dev/null 2>&1; then
    uuidgen | tr '[:upper:]' '[:lower:]'
    return
  fi
  hex="$(openssl rand -hex 16)"
  printf '%s-%s-4%s-%s%s-%s\n' \
    "${hex:0:8}" "${hex:8:4}" "${hex:13:3}" \
    "$(printf '%x' $(( (16#${hex:16:2} & 63) | 128 )))" "${hex:18:2}" "${hex:20:12}"
}

operator_tenant_id="${SYNARA_PLATFORM_OPERATOR_TENANT_ID:-$(random_uuid)}"
if [[ ! "$operator_tenant_id" =~ ^[0-9a-fA-F-]{36}$ ]]; then
  printf 'SYNARA_PLATFORM_OPERATOR_TENANT_ID must be a UUID\n' >&2
  exit 1
fi

if (( dry_run == 0 )); then
  namespace_state=0
  if "${kubectl_cmd[@]}" get namespace "$namespace" >/dev/null 2>&1; then
    namespace_state=1
  fi
  if (( namespace_state == 1 )); then
    if [[ "$allow_existing" != "1" ]]; then
      printf 'namespace %s already exists; refusing to modify it (set SYNARA_REMOTE_K8S_ALLOW_EXISTING=1 after reviewing it)\n' "$namespace" >&2
      exit 1
    fi
    existing_part="$("${kubectl_cmd[@]}" get namespace "$namespace" -o jsonpath='{.metadata.labels.app\.kubernetes\.io/part-of}' 2>/dev/null || true)"
    if [[ "$existing_part" != "synara-stage6-acceptance" ]]; then
      printf 'existing namespace %s is not labelled synara-stage6-acceptance; refusing to modify it\n' "$namespace" >&2
      exit 1
    fi
  else
    "${kubectl_cmd[@]}" create namespace "$namespace" --dry-run=client -o yaml |
      "${kubectl_cmd[@]}" apply -f - >/dev/null
    created_namespace=1
    "${kubectl_cmd[@]}" label namespace "$namespace" \
      app.kubernetes.io/name=synara \
      app.kubernetes.io/part-of=synara-stage6-acceptance \
      app.kubernetes.io/managed-by=synara-acceptance >/dev/null
  fi
fi

work_dir="$(mktemp -d "${TMPDIR:-/tmp}/synara-stage6-k8s.XXXXXX")"

# Keep generated authorities in process memory and pass them to kubectl only
# through --from-literal. They are never printed or written to the evidence.
postgres_password="$(openssl rand -hex 24)"
minio_root_user="synara$(openssl rand -hex 6)"
minio_root_password="$(openssl rand -hex 24)"
worker_registration_token="$(openssl rand -hex 32)"
auth_token="$(openssl rand -hex 32)"
provider_cursor_key="$(openssl rand -base64 32 | tr -d '\n')"
credential_master_key="$(openssl rand -base64 32 | tr -d '\n')"
database_url="postgresql://synara:${postgres_password}@postgres:5432/synara?sslmode=disable"

apply_secret() {
  if (( dry_run == 1 )); then
    return
  fi
  "${kubectl_cmd[@]}" -n "$namespace" create secret generic synara-stage6-secrets \
    --from-literal=database-url="$database_url" \
    --from-literal=postgres-password="$postgres_password" \
    --from-literal=worker-registration-token="$worker_registration_token" \
    --from-literal=provider-cursor-key="$provider_cursor_key" \
    --from-literal=credential-master-key="$credential_master_key" \
    --from-literal=auth-token="$auth_token" \
    --from-literal=minio-root-user="$minio_root_user" \
    --from-literal=minio-root-password="$minio_root_password" \
    --dry-run=client -o yaml | "${kubectl_cmd[@]}" apply -f - >/dev/null
}

apply_secret

render_resources() {
  cat <<YAML
apiVersion: v1
kind: Namespace
metadata:
  name: ${namespace}
  labels:
    app.kubernetes.io/name: synara
    app.kubernetes.io/part-of: synara-stage6-acceptance
    app.kubernetes.io/managed-by: synara-acceptance
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: synara-stage6-config
  namespace: ${namespace}
data:
  public-control-plane-url: http://127.0.0.1:3780
  public-admin-url: http://127.0.0.1:3774
  internal-status-board-url: ""
  internal-incident-publisher-url: ""
  commercialization-mode: internal-self-hosted
  platform-operator-tenant-id: ${operator_tenant_id}
  artifact-bucket: synara-artifacts
  artifact-region: local
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: postgres-data
  namespace: ${namespace}
  labels:
    app.kubernetes.io/name: postgres
    app.kubernetes.io/part-of: synara-stage6-acceptance
spec:
  accessModes: ["ReadWriteOnce"]
  resources:
    requests:
      storage: 2Gi
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: minio-data
  namespace: ${namespace}
  labels:
    app.kubernetes.io/name: minio
    app.kubernetes.io/part-of: synara-stage6-acceptance
spec:
  accessModes: ["ReadWriteOnce"]
  resources:
    requests:
      storage: 2Gi
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: postgres
  namespace: ${namespace}
  labels:
    app.kubernetes.io/name: postgres
    app.kubernetes.io/part-of: synara-stage6-acceptance
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: postgres
  template:
    metadata:
      labels:
        app.kubernetes.io/name: postgres
        app.kubernetes.io/part-of: synara-stage6-acceptance
    spec:
      containers:
        - name: postgres
          image: ${postgres_image}
          imagePullPolicy: IfNotPresent
          env:
            - name: POSTGRES_DB
              value: synara
            - name: POSTGRES_USER
              value: synara
            - name: POSTGRES_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: synara-stage6-secrets
                  key: postgres-password
          ports:
            - name: postgres
              containerPort: 5432
          readinessProbe:
            exec:
              command: ["/bin/sh", "-c", "pg_isready -U synara -d synara"]
            periodSeconds: 5
            timeoutSeconds: 3
          livenessProbe:
            exec:
              command: ["/bin/sh", "-c", "pg_isready -U synara -d synara"]
            initialDelaySeconds: 20
            periodSeconds: 10
            timeoutSeconds: 3
          volumeMounts:
            - name: data
              mountPath: /var/lib/postgresql/data
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: postgres-data
---
apiVersion: v1
kind: Service
metadata:
  name: postgres
  namespace: ${namespace}
  labels:
    app.kubernetes.io/name: postgres
    app.kubernetes.io/part-of: synara-stage6-acceptance
spec:
  type: ClusterIP
  selector:
    app.kubernetes.io/name: postgres
  ports:
    - name: postgres
      port: 5432
      targetPort: postgres
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: minio
  namespace: ${namespace}
  labels:
    app.kubernetes.io/name: minio
    app.kubernetes.io/part-of: synara-stage6-acceptance
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: minio
  template:
    metadata:
      labels:
        app.kubernetes.io/name: minio
        app.kubernetes.io/part-of: synara-stage6-acceptance
    spec:
      containers:
        - name: minio
          image: ${minio_image}
          imagePullPolicy: IfNotPresent
          args: ["server", "/data", "--console-address", ":9001"]
          env:
            - name: MINIO_ROOT_USER
              valueFrom:
                secretKeyRef:
                  name: synara-stage6-secrets
                  key: minio-root-user
            - name: MINIO_ROOT_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: synara-stage6-secrets
                  key: minio-root-password
          ports:
            - name: api
              containerPort: 9000
            - name: console
              containerPort: 9001
          readinessProbe:
            httpGet:
              path: /minio/health/ready
              port: api
            periodSeconds: 5
            timeoutSeconds: 3
          livenessProbe:
            httpGet:
              path: /minio/health/live
              port: api
            initialDelaySeconds: 20
            periodSeconds: 10
            timeoutSeconds: 3
          volumeMounts:
            - name: data
              mountPath: /data
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: minio-data
---
apiVersion: v1
kind: Service
metadata:
  name: minio
  namespace: ${namespace}
  labels:
    app.kubernetes.io/name: minio
    app.kubernetes.io/part-of: synara-stage6-acceptance
spec:
  type: ClusterIP
  selector:
    app.kubernetes.io/name: minio
  ports:
    - name: api
      port: 9000
      targetPort: api
    - name: console
      port: 9001
      targetPort: console
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: synara-control-plane
  namespace: ${namespace}
  labels:
    app.kubernetes.io/name: synara-control-plane
    app.kubernetes.io/part-of: synara-stage6-acceptance
spec:
  replicas: 1
  strategy:
    type: Recreate
  selector:
    matchLabels:
      app.kubernetes.io/name: synara-control-plane
  template:
    metadata:
      labels:
        app.kubernetes.io/name: synara-control-plane
        app.kubernetes.io/part-of: synara-stage6-acceptance
    spec:
      terminationGracePeriodSeconds: 30
      containers:
        - name: control-plane
          image: ${control_plane_image}
          imagePullPolicy: IfNotPresent
          ports:
            - name: http
              containerPort: 3780
          env:
            - name: SYNARA_DEPLOYMENT_PROFILE
              value: single-node
            - name: SYNARA_METADATA_STORE
              value: postgresql
            - name: SYNARA_ARTIFACT_STORE
              value: minio
            - name: SYNARA_QUEUE_DRIVER
              value: postgres-outbox
            - name: SYNARA_CONTROL_PLANE_REPLICAS
              value: "1"
            - name: SYNARA_CONTROL_PLANE_LISTEN
              value: 0.0.0.0:3780
            - name: SYNARA_DATABASE_URL
              valueFrom:
                secretKeyRef:
                  name: synara-stage6-secrets
                  key: database-url
            - name: SYNARA_DATABASE_MIGRATION_LOCK_TIMEOUT
              value: 90s
            - name: SYNARA_CONTROL_PLANE_DEV_BOOTSTRAP
              value: "true"
            - name: SYNARA_LOGIN_COOKIE_SECURE
              value: "false"
            - name: SYNARA_LOGIN_COOKIE_SAME_SITE
              value: lax
            - name: SYNARA_PUBLIC_CONTROL_PLANE_URL
              valueFrom:
                configMapKeyRef:
                  name: synara-stage6-config
                  key: public-control-plane-url
            - name: SYNARA_PUBLIC_ADMIN_URL
              valueFrom:
                configMapKeyRef:
                  name: synara-stage6-config
                  key: public-admin-url
            - name: SYNARA_INTERNAL_STATUS_BOARD_URL
              valueFrom:
                configMapKeyRef:
                  name: synara-stage6-config
                  key: internal-status-board-url
            - name: SYNARA_INTERNAL_INCIDENT_PUBLISHER_URL
              valueFrom:
                configMapKeyRef:
                  name: synara-stage6-config
                  key: internal-incident-publisher-url
            - name: SYNARA_COMMERCIALIZATION_MODE
              valueFrom:
                configMapKeyRef:
                  name: synara-stage6-config
                  key: commercialization-mode
            - name: SYNARA_PLATFORM_OPERATOR_TENANT_ID
              valueFrom:
                configMapKeyRef:
                  name: synara-stage6-config
                  key: platform-operator-tenant-id
            - name: SYNARA_WORKER_REGISTRATION_TOKEN
              valueFrom:
                secretKeyRef:
                  name: synara-stage6-secrets
                  key: worker-registration-token
            - name: SYNARA_PROVIDER_CURSOR_KEY
              valueFrom:
                secretKeyRef:
                  name: synara-stage6-secrets
                  key: provider-cursor-key
            - name: SYNARA_CREDENTIAL_KMS_PROVIDER
              value: local
            - name: SYNARA_CREDENTIAL_KMS_KEY_ID
              value: single-node-local-v1
            - name: SYNARA_CREDENTIAL_MASTER_KEY
              valueFrom:
                secretKeyRef:
                  name: synara-stage6-secrets
                  key: credential-master-key
            - name: SYNARA_ARTIFACT_ENDPOINT
              value: http://minio:9000
            - name: SYNARA_ARTIFACT_PUBLIC_ENDPOINT
              value: http://minio:9000
            - name: SYNARA_ARTIFACT_BUCKET
              valueFrom:
                configMapKeyRef:
                  name: synara-stage6-config
                  key: artifact-bucket
            - name: SYNARA_ARTIFACT_REGION
              valueFrom:
                configMapKeyRef:
                  name: synara-stage6-config
                  key: artifact-region
            - name: SYNARA_ARTIFACT_ACCESS_KEY_ID
              valueFrom:
                secretKeyRef:
                  name: synara-stage6-secrets
                  key: minio-root-user
            - name: SYNARA_ARTIFACT_SECRET_ACCESS_KEY
              valueFrom:
                secretKeyRef:
                  name: synara-stage6-secrets
                  key: minio-root-password
            - name: SYNARA_ARTIFACT_USE_PATH_STYLE
              value: "true"
            - name: SYNARA_COST_ACCOUNTING_BLOB_SOURCE
              value: disabled
            - name: SYNARA_WORKER_LEASES_ENABLED
              value: "true"
            - name: SYNARA_WORKER_FENCING_ENABLED
              value: "true"
            - name: SYNARA_OUTBOX_POLL_INTERVAL
              value: 500ms
            - name: SYNARA_OUTBOX_CLAIM_TTL
              value: 30s
          startupProbe:
            httpGet:
              path: /ready
              port: http
            periodSeconds: 5
            timeoutSeconds: 3
            failureThreshold: 60
          readinessProbe:
            httpGet:
              path: /ready
              port: http
            periodSeconds: 5
            timeoutSeconds: 3
          livenessProbe:
            httpGet:
              path: /health
              port: http
            initialDelaySeconds: 10
            periodSeconds: 10
            timeoutSeconds: 3
          resources:
            requests:
              cpu: 50m
              memory: 128Mi
            limits:
              cpu: 500m
              memory: 512Mi
---
apiVersion: v1
kind: Service
metadata:
  name: synara-control-plane
  namespace: ${namespace}
  labels:
    app.kubernetes.io/name: synara-control-plane
    app.kubernetes.io/part-of: synara-stage6-acceptance
spec:
  type: ClusterIP
  selector:
    app.kubernetes.io/name: synara-control-plane
  ports:
    - name: http
      port: 3780
      targetPort: http
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: synara-web
  namespace: ${namespace}
  labels:
    app.kubernetes.io/name: synara-web
    app.kubernetes.io/part-of: synara-stage6-acceptance
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: synara-web
  template:
    metadata:
      labels:
        app.kubernetes.io/name: synara-web
        app.kubernetes.io/part-of: synara-stage6-acceptance
    spec:
      containers:
        - name: web
          image: ${node_image}
          imagePullPolicy: IfNotPresent
          args: ["/app/apps/server/dist/index.mjs"]
          workingDir: /workspace
          env:
            - name: HOME
              value: /home/synara
            - name: SYNARA_HOME
              value: /home/synara/.synara
            - name: SYNARA_MODE
              value: web
            - name: SYNARA_HOST
              value: 0.0.0.0
            - name: SYNARA_PORT
              value: "3773"
            - name: SYNARA_NO_BROWSER
              value: "1"
            - name: SYNARA_AUTO_BOOTSTRAP_PROJECT_FROM_CWD
              value: "0"
            - name: SYNARA_AUTH_TOKEN
              valueFrom:
                secretKeyRef:
                  name: synara-stage6-secrets
                  key: auth-token
            - name: SYNARA_CONTROL_PLANE_URL
              value: http://synara-control-plane:3780
            - name: SYNARA_PUBLIC_URL
              value: ""
            - name: SYNARA_ALLOW_INSECURE_REMOTE
              value: "true"
          ports:
            - name: http
              containerPort: 3773
          readinessProbe:
            httpGet:
              path: /ready
              port: http
            periodSeconds: 5
            timeoutSeconds: 3
          livenessProbe:
            httpGet:
              path: /ready
              port: http
            initialDelaySeconds: 20
            periodSeconds: 10
            timeoutSeconds: 3
          resources:
            requests:
              cpu: 25m
              memory: 96Mi
            limits:
              cpu: 300m
              memory: 384Mi
---
apiVersion: v1
kind: Service
metadata:
  name: synara-web
  namespace: ${namespace}
  labels:
    app.kubernetes.io/name: synara-web
    app.kubernetes.io/part-of: synara-stage6-acceptance
spec:
  type: ClusterIP
  selector:
    app.kubernetes.io/name: synara-web
  ports:
    - name: http
      port: 3773
      targetPort: http
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: platform-admin
  namespace: ${namespace}
  labels:
    app.kubernetes.io/name: platform-admin
    app.kubernetes.io/part-of: synara-stage6-acceptance
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: platform-admin
  template:
    metadata:
      labels:
        app.kubernetes.io/name: platform-admin
        app.kubernetes.io/part-of: synara-stage6-acceptance
    spec:
      containers:
        - name: admin
          image: ${node_image}
          imagePullPolicy: IfNotPresent
          args: ["/app/apps/admin/server.mjs"]
          env:
            - name: SYNARA_ADMIN_HOST
              value: 0.0.0.0
            - name: SYNARA_ADMIN_PORT
              value: "3774"
            - name: SYNARA_CONTROL_PLANE_URL
              value: http://synara-control-plane:3780
            - name: SYNARA_TENANT_APP_URL
              value: http://synara-web:3773
          ports:
            - name: http
              containerPort: 3774
          readinessProbe:
            httpGet:
              path: /ready
              port: http
            periodSeconds: 5
            timeoutSeconds: 3
          livenessProbe:
            httpGet:
              path: /ready
              port: http
            initialDelaySeconds: 20
            periodSeconds: 10
            timeoutSeconds: 3
          resources:
            requests:
              cpu: 25m
              memory: 48Mi
            limits:
              cpu: 250m
              memory: 256Mi
---
apiVersion: v1
kind: Service
metadata:
  name: platform-admin
  namespace: ${namespace}
  labels:
    app.kubernetes.io/name: platform-admin
    app.kubernetes.io/part-of: synara-stage6-acceptance
spec:
  type: ClusterIP
  selector:
    app.kubernetes.io/name: platform-admin
  ports:
    - name: http
      port: 3774
      targetPort: http
YAML
}

render_resources > "$work_dir/resources.yaml"

if (( dry_run == 1 )); then
  # Client-side validation is intentionally non-mutating and does not need a
  # reachable cluster. The generated Secret is not rendered, so no authority
  # appears in the validation stream.
  "${kubectl_cmd[@]}" apply --dry-run=client -f "$work_dir/resources.yaml" >/dev/null
  printf 'remote Stage 6 acceptance render validated (namespace=%s; no resources applied)\n' "$namespace"
  exit 0
fi

"${kubectl_cmd[@]}" -n "$namespace" apply -f "$work_dir/resources.yaml" >/dev/null

for deployment in postgres minio synara-control-plane synara-web platform-admin; do
  "${kubectl_cmd[@]}" -n "$namespace" rollout status "deployment/$deployment" --timeout="$rollout_timeout" >/dev/null
done

service_types="$("${kubectl_cmd[@]}" -n "$namespace" get services -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.spec.type}{"\n"}{end}')"
if printf '%s\n' "$service_types" | awk 'NF && $2 != "ClusterIP" { bad=1 } END { exit bad }'; then
  :
else
  printf 'a non-ClusterIP Service was rendered in %s\n' "$namespace" >&2
  exit 1
fi

printf 'remote Stage 6 acceptance deployed (context=%s namespace=%s)\n' "$context" "$namespace"
printf 'profile=single-node devBootstrap=true publicServices=0 operatorTenant=%s\n' "$operator_tenant_id"
"${kubectl_cmd[@]}" -n "$namespace" get pods -o custom-columns='NAME:.metadata.name,READY:.status.containerStatuses[0].ready,STATUS:.status.phase,NODE:.spec.nodeName' --no-headers
if [[ "$keep_resources" == "1" ]]; then
  printf 'resources kept; exact cleanup: kubectl --context %s delete namespace %s\n' "$context" "$namespace"
else
  printf 'resources will be removed on exit because --keep/SYNARA_REMOTE_K8S_KEEP_RESOURCES=1 was not set\n'
fi
