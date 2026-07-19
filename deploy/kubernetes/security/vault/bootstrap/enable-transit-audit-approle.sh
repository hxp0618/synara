#!/usr/bin/env bash
set -euo pipefail

umask 077

require_env() {
  local name="$1"
  if [[ -z "${!name:-}" ]]; then
    echo "missing required environment variable: ${name}" >&2
    exit 1
  fi
}

require_env VAULT_ADDR
require_env VAULT_TOKEN
require_env VAULT_CACERT

require_command() {
  local name="$1"
  if ! command -v "${name}" >/dev/null 2>&1; then
    echo "${name} is required on PATH" >&2
    exit 1
  fi
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
POLICY_TEMPLATE="${SCRIPT_DIR}/transit-sign-synara-worker-release.hcl"
AUDITOR_POLICY_TEMPLATE="${SCRIPT_DIR}/vault-production-auditor.hcl"
SNAPSHOT_POLICY_TEMPLATE="${SCRIPT_DIR}/../synara-vault-snapshot-operator.hcl"

TRANSIT_MOUNT="${TRANSIT_SECRET_ENGINE_PATH:-transit}"
TRANSIT_KEY_NAME="${TRANSIT_KEY_NAME:-synara-worker-release}"
TRANSIT_KEY_TYPE="${TRANSIT_KEY_TYPE:-ecdsa-p256}"
POLICY_NAME="${VAULT_POLICY_NAME:-synara-worker-release-signer}"
APPROLE_NAME="${VAULT_APPROLE_NAME:-synara-worker-release-signer}"
AUDITOR_POLICY_NAME="${VAULT_AUDITOR_POLICY_NAME:-synara-vault-production-auditor}"
AUDITOR_APPROLE_NAME="${VAULT_AUDITOR_APPROLE_NAME:-synara-vault-production-auditor}"
SNAPSHOT_POLICY_NAME="${VAULT_SNAPSHOT_POLICY_NAME:-synara-vault-snapshot-operator}"
SNAPSHOT_APPROLE_NAME="${VAULT_SNAPSHOT_APPROLE_NAME:-synara-vault-snapshot-operator}"
AUDIT_DEVICE_PATH_PRIMARY="${AUDIT_DEVICE_PATH_PRIMARY:-file}"
AUDIT_LOG_FILE_PRIMARY="${AUDIT_LOG_FILE_PRIMARY:-/vault/audit/audit-primary.log}"
AUDIT_DEVICE_PATH_SECONDARY="${AUDIT_DEVICE_PATH_SECONDARY:-file-secondary}"
AUDIT_LOG_FILE_SECONDARY="${AUDIT_LOG_FILE_SECONDARY:-/vault/audit/audit-secondary.log}"

ROLE_TOKEN_TTL="${ROLE_TOKEN_TTL:-2h}"
ROLE_TOKEN_MAX_TTL="${ROLE_TOKEN_MAX_TTL:-4h}"
ROLE_TOKEN_NUM_USES="${ROLE_TOKEN_NUM_USES:-0}"
ROLE_SECRET_ID_TTL="${ROLE_SECRET_ID_TTL:-10m}"
ROLE_SECRET_ID_NUM_USES="${ROLE_SECRET_ID_NUM_USES:-1}"

AUDITOR_TOKEN_TTL="${AUDITOR_TOKEN_TTL:-30m}"
AUDITOR_TOKEN_MAX_TTL="${AUDITOR_TOKEN_MAX_TTL:-1h}"
AUDITOR_TOKEN_NUM_USES="${AUDITOR_TOKEN_NUM_USES:-0}"
AUDITOR_SECRET_ID_TTL="${AUDITOR_SECRET_ID_TTL:-10m}"
AUDITOR_SECRET_ID_NUM_USES="${AUDITOR_SECRET_ID_NUM_USES:-1}"

SNAPSHOT_TOKEN_TTL="${SNAPSHOT_TOKEN_TTL:-30m}"
SNAPSHOT_TOKEN_MAX_TTL="${SNAPSHOT_TOKEN_MAX_TTL:-1h}"
SNAPSHOT_TOKEN_NUM_USES="${SNAPSHOT_TOKEN_NUM_USES:-0}"
SNAPSHOT_SECRET_ID_TTL="${SNAPSHOT_SECRET_ID_TTL:-10m}"
SNAPSHOT_SECRET_ID_NUM_USES="${SNAPSHOT_SECRET_ID_NUM_USES:-1}"

require_command vault
require_command python3

if ! vault status >/dev/null 2>&1; then
  echo "vault status failed; check VAULT_ADDR, VAULT_TOKEN, and VAULT_CACERT" >&2
  exit 1
fi

rendered_policy="$(mktemp)"
rendered_auditor_policy="$(mktemp)"
rendered_snapshot_policy="$(mktemp)"
trap 'rm -f "${rendered_policy}" "${rendered_auditor_policy}" "${rendered_snapshot_policy}"' EXIT

sed \
  -e "s|__TRANSIT_MOUNT__|${TRANSIT_MOUNT}|g" \
  -e "s|__TRANSIT_KEY_NAME__|${TRANSIT_KEY_NAME}|g" \
  "${POLICY_TEMPLATE}" > "${rendered_policy}"

sed \
  -e "s|__TRANSIT_MOUNT__|${TRANSIT_MOUNT}|g" \
  -e "s|__TRANSIT_KEY_NAME__|${TRANSIT_KEY_NAME}|g" \
  -e "s|__SIGNER_APPROLE_NAME__|${APPROLE_NAME}|g" \
  -e "s|__AUDITOR_APPROLE_NAME__|${AUDITOR_APPROLE_NAME}|g" \
  -e "s|__SIGNER_POLICY_NAME__|${POLICY_NAME}|g" \
  -e "s|__AUDITOR_POLICY_NAME__|${AUDITOR_POLICY_NAME}|g" \
  "${AUDITOR_POLICY_TEMPLATE}" > "${rendered_auditor_policy}"

sed \
  -e "s|__TRANSIT_MOUNT__|${TRANSIT_MOUNT}|g" \
  -e "s|__TRANSIT_KEY_NAME__|${TRANSIT_KEY_NAME}|g" \
  -e "s|__SIGNER_APPROLE_NAME__|${APPROLE_NAME}|g" \
  -e "s|__AUDITOR_APPROLE_NAME__|${AUDITOR_APPROLE_NAME}|g" \
  -e "s|__SNAPSHOT_APPROLE_NAME__|${SNAPSHOT_APPROLE_NAME}|g" \
  -e "s|__SIGNER_POLICY_NAME__|${POLICY_NAME}|g" \
  -e "s|__AUDITOR_POLICY_NAME__|${AUDITOR_POLICY_NAME}|g" \
  -e "s|__SNAPSHOT_POLICY_NAME__|${SNAPSHOT_POLICY_NAME}|g" \
  "${SNAPSHOT_POLICY_TEMPLATE}" > "${rendered_snapshot_policy}"

if ! vault secrets list -format=json | grep -q "\"${TRANSIT_MOUNT}/\""; then
  vault secrets enable -path="${TRANSIT_MOUNT}" transit
fi

if ! vault read -format=json "${TRANSIT_MOUNT}/keys/${TRANSIT_KEY_NAME}" >/dev/null 2>&1; then
  vault write -f "${TRANSIT_MOUNT}/keys/${TRANSIT_KEY_NAME}" \
    type="${TRANSIT_KEY_TYPE}" \
    derived=false \
    exportable=false \
    allow_plaintext_backup=false
fi

# Keep signing material non-exportable and non-deletable. Rotation is staged
# explicitly with the admission trust bundle so an automatic key-version
# change cannot strand already-approved immutable Worker images.
vault write "${TRANSIT_MOUNT}/keys/${TRANSIT_KEY_NAME}/config" \
  deletion_allowed=false \
  exportable=false \
  allow_plaintext_backup=false \
  auto_rotate_period=0

audit_device_descriptors() {
  vault audit list -format=json | python3 -c '
import json
import sys

payload = json.load(sys.stdin)
for raw_path, raw_device in payload.items():
    path = str(raw_path).rstrip("/")
    if not isinstance(raw_device, dict):
        print(f"{path}\t\t")
        continue
    device_type = raw_device.get("type", "")
    options = raw_device.get("options") if isinstance(raw_device.get("options"), dict) else {}
    file_path = (
        options.get("file_path")
        or options.get("filePath")
        or raw_device.get("file_path")
        or raw_device.get("filePath")
        or ""
    )
    print(f"{path}\t{device_type}\t{file_path}")
'
}

disable_unexpected_audit_devices() {
  local primary_descriptor="${AUDIT_DEVICE_PATH_PRIMARY}"$'\t'"file"$'\t'"${AUDIT_LOG_FILE_PRIMARY}"
  local secondary_descriptor="${AUDIT_DEVICE_PATH_SECONDARY}"$'\t'"file"$'\t'"${AUDIT_LOG_FILE_SECONDARY}"
  local descriptor device_path
  while IFS= read -r descriptor; do
    [[ -z "${descriptor}" ]] && continue
    case "${descriptor}" in
      "${primary_descriptor}"|"${secondary_descriptor}")
        continue
        ;;
    esac
    device_path="${descriptor%%$'\t'*}"
    [[ -n "${device_path}" ]] || continue
    vault audit disable "${device_path}"
  done < <(audit_device_descriptors)
}

ensure_file_audit_device() {
  local device_path="$1"
  local log_file="$2"
  local expected_descriptor="${device_path}"$'\t'"file"$'\t'"${log_file}"
  if audit_device_descriptors | grep -Fxq "${expected_descriptor}"; then
    return
  fi
  vault audit enable -path="${device_path}" file file_path="${log_file}" mode=0600
}

disable_unexpected_audit_devices
ensure_file_audit_device "${AUDIT_DEVICE_PATH_PRIMARY}" "${AUDIT_LOG_FILE_PRIMARY}"
ensure_file_audit_device "${AUDIT_DEVICE_PATH_SECONDARY}" "${AUDIT_LOG_FILE_SECONDARY}"

vault policy write "${POLICY_NAME}" "${rendered_policy}"
vault policy write "${AUDITOR_POLICY_NAME}" "${rendered_auditor_policy}"
vault policy write "${SNAPSHOT_POLICY_NAME}" "${rendered_snapshot_policy}"

if ! vault auth list -format=json | grep -q '"approle/"'; then
  vault auth enable approle
fi

vault write "auth/approle/role/${APPROLE_NAME}" \
  bind_secret_id=true \
  token_type=batch \
  token_no_default_policy=true \
  token_policies="${POLICY_NAME}" \
  token_ttl="${ROLE_TOKEN_TTL}" \
  token_max_ttl="${ROLE_TOKEN_MAX_TTL}" \
  token_num_uses="${ROLE_TOKEN_NUM_USES}" \
  secret_id_ttl="${ROLE_SECRET_ID_TTL}" \
  secret_id_num_uses="${ROLE_SECRET_ID_NUM_USES}"

vault write "auth/approle/role/${AUDITOR_APPROLE_NAME}" \
  bind_secret_id=true \
  token_type=batch \
  token_no_default_policy=true \
  token_policies="${AUDITOR_POLICY_NAME}" \
  token_ttl="${AUDITOR_TOKEN_TTL}" \
  token_max_ttl="${AUDITOR_TOKEN_MAX_TTL}" \
  token_num_uses="${AUDITOR_TOKEN_NUM_USES}" \
  secret_id_ttl="${AUDITOR_SECRET_ID_TTL}" \
  secret_id_num_uses="${AUDITOR_SECRET_ID_NUM_USES}"

vault write "auth/approle/role/${SNAPSHOT_APPROLE_NAME}" \
  bind_secret_id=true \
  token_type=batch \
  token_no_default_policy=true \
  token_policies="${SNAPSHOT_POLICY_NAME}" \
  token_ttl="${SNAPSHOT_TOKEN_TTL}" \
  token_max_ttl="${SNAPSHOT_TOKEN_MAX_TTL}" \
  token_num_uses="${SNAPSHOT_TOKEN_NUM_USES}" \
  secret_id_ttl="${SNAPSHOT_SECRET_ID_TTL}" \
  secret_id_num_uses="${SNAPSHOT_SECRET_ID_NUM_USES}"

cat <<EOF
Vault Transit signer bootstrap complete.

Next steps:
1. Record the role_id without committing it:
   vault read -field=role_id auth/approle/role/${APPROLE_NAME}/role-id
2. Mint a one-time secret_id and deliver it through your secret manager:
   vault write -f -field=secret_id auth/approle/role/${APPROLE_NAME}/secret-id
3. Record and mint the independent production auditor identity the same way:
   vault read -field=role_id auth/approle/role/${AUDITOR_APPROLE_NAME}/role-id
   vault write -f -field=secret_id auth/approle/role/${AUDITOR_APPROLE_NAME}/secret-id
4. Record and mint the isolated snapshot-restore operator identity the same way:
   vault read -field=role_id auth/approle/role/${SNAPSHOT_APPROLE_NAME}/role-id
   vault write -f -field=secret_id auth/approle/role/${SNAPSHOT_APPROLE_NAME}/secret-id
5. Export the public key for the production admission ConfigMap:
   cosign public-key --key hashivault://${TRANSIT_KEY_NAME}
EOF
