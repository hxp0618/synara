#!/bin/sh
set -eu

: "${MINIO_ENDPOINT:?MINIO_ENDPOINT is required}"
: "${MINIO_ROOT_USER:?MINIO_ROOT_USER is required}"
: "${MINIO_ROOT_PASSWORD:?MINIO_ROOT_PASSWORD is required}"
: "${MINIO_ARTIFACT_USER:?MINIO_ARTIFACT_USER is required}"
: "${MINIO_ARTIFACT_PASSWORD:?MINIO_ARTIFACT_PASSWORD is required}"
: "${SYNARA_ARTIFACT_BUCKET:?SYNARA_ARTIFACT_BUCKET is required}"

case "${SYNARA_ARTIFACT_BUCKET}" in
  *[!a-z0-9.-]* | .* | *.)
    echo "SYNARA_ARTIFACT_BUCKET must be a lowercase DNS-compatible bucket name" >&2
    exit 1
    ;;
esac

if [ "${MINIO_ARTIFACT_USER}" = "${MINIO_ROOT_USER}" ] || [ "${MINIO_ARTIFACT_PASSWORD}" = "${MINIO_ROOT_PASSWORD}" ]; then
  echo "The Artifact identity must not reuse MinIO root credentials" >&2
  exit 1
fi

policy_path=/tmp/synara-artifact-policy.json
: > "${policy_path}"
while IFS= read -r policy_line || [ -n "${policy_line}" ]; do
  policy_line=${policy_line//__SYNARA_ARTIFACT_BUCKET__/${SYNARA_ARTIFACT_BUCKET}}
  printf '%s\n' "${policy_line}" >> "${policy_path}"
done < /bootstrap/artifact-policy.json

mc alias set synara "${MINIO_ENDPOINT}" "${MINIO_ROOT_USER}" "${MINIO_ROOT_PASSWORD}"
mc mb --ignore-existing "synara/${SYNARA_ARTIFACT_BUCKET}"
mc anonymous set none "synara/${SYNARA_ARTIFACT_BUCKET}"
mc admin policy create synara synara-artifact-control-plane "${policy_path}"
mc admin user add synara "${MINIO_ARTIFACT_USER}" "${MINIO_ARTIFACT_PASSWORD}"
mc admin policy attach synara synara-artifact-control-plane --user "${MINIO_ARTIFACT_USER}"
