#!/usr/bin/env python3
"""Validate fail-closed Artifact object-store deployment wiring without claiming cloud IAM acceptance."""

from __future__ import annotations

import argparse
import json
import pathlib
import re
import sys


SCHEMA_VERSION = "synara.stage6-artifact-storage-deployment.v1"
ASSESSMENT = "artifact-storage-deployment-wiring-validated-not-cloud-iam-accepted"
PINNED_MC_IMAGE = "minio/mc:RELEASE.2025-04-16T18-13-26Z"

BUCKET_ACTIONS = {
    "s3:GetBucketLocation",
    "s3:ListBucket",
    "s3:ListBucketMultipartUploads",
}
OBJECT_ACTIONS = {
    "s3:GetObject",
    "s3:PutObject",
    "s3:DeleteObject",
    "s3:AbortMultipartUpload",
    "s3:ListMultipartUploadParts",
}


class ArtifactStorageBoundaryError(Exception):
    pass


def read_text(path: pathlib.Path) -> str:
    try:
        return path.read_text(encoding="utf-8")
    except (OSError, UnicodeDecodeError) as error:
        raise ArtifactStorageBoundaryError(f"could not read {path}") from error


def require_fragment(source: str, fragment: str, label: str) -> None:
    if fragment not in source:
        raise ArtifactStorageBoundaryError(f"{label} is missing required Artifact storage wiring: {fragment}")


def service_block(compose: str, service: str, next_service: str | None) -> str:
    start = re.search(rf"^  {re.escape(service)}:\s*$", compose, re.MULTILINE)
    if start is None:
        raise ArtifactStorageBoundaryError(f"Compose is missing {service} service")
    if next_service is None:
        return compose[start.start() :]
    end = re.search(rf"^  {re.escape(next_service)}:\s*$", compose[start.end() :], re.MULTILINE)
    if end is None:
        raise ArtifactStorageBoundaryError(f"Compose is missing {next_service} service after {service}")
    return compose[start.start() : start.end() + end.start()]


def validate_policy(policy_path: pathlib.Path) -> None:
    try:
        policy = json.loads(read_text(policy_path))
    except json.JSONDecodeError as error:
        raise ArtifactStorageBoundaryError("Artifact policy is not valid JSON") from error
    if not isinstance(policy, dict) or policy.get("Version") != "2012-10-17":
        raise ArtifactStorageBoundaryError("Artifact policy must use the 2012-10-17 version")
    statements = policy.get("Statement")
    if not isinstance(statements, list) or len(statements) != 2:
        raise ArtifactStorageBoundaryError("Artifact policy must contain exactly two statements")

    expected = (
        (BUCKET_ACTIONS, {"arn:aws:s3:::__SYNARA_ARTIFACT_BUCKET__"}),
        (OBJECT_ACTIONS, {"arn:aws:s3:::__SYNARA_ARTIFACT_BUCKET__/tenants/*"}),
    )
    for index, (statement, (expected_actions, expected_resources)) in enumerate(zip(statements, expected, strict=True)):
        if not isinstance(statement, dict) or statement.get("Effect") != "Allow":
            raise ArtifactStorageBoundaryError(f"Artifact policy statement {index} must be an Allow statement")
        actions = statement.get("Action")
        resources = statement.get("Resource")
        if not isinstance(actions, list) or set(actions) != expected_actions:
            raise ArtifactStorageBoundaryError(f"Artifact policy statement {index} actions exceed the approved boundary")
        if not isinstance(resources, list) or set(resources) != expected_resources:
            raise ArtifactStorageBoundaryError(f"Artifact policy statement {index} resources exceed the approved boundary")
        if any(not isinstance(value, str) or value == "*" for value in actions + resources):
            raise ArtifactStorageBoundaryError(f"Artifact policy statement {index} contains a wildcard authority")


def validate_repository(repository_root: pathlib.Path) -> dict[str, object]:
    compose = read_text(repository_root / "deploy/saas/docker-compose.yml")
    bootstrap = read_text(repository_root / "deploy/saas/minio/bootstrap.sh")
    policy_path = repository_root / "deploy/saas/minio/artifact-policy.json"
    deployment = read_text(repository_root / "deploy/kubernetes/deployment.yaml")
    service_account = read_text(repository_root / "deploy/kubernetes/service-account.yaml")
    secret_example = read_text(repository_root / "deploy/kubernetes/secret.example.yaml")
    config_source = read_text(repository_root / "services/control-plane/internal/config/config.go")
    s3_source = read_text(repository_root / "services/control-plane/internal/artifacts/s3_store.go")

    minio = service_block(compose, "minio", "minio-bootstrap")
    bootstrap_service = service_block(compose, "minio-bootstrap", "control-plane")
    control_plane = service_block(compose, "control-plane", "admin")

    require_fragment(
        minio,
        "MINIO_API_CORS_ALLOW_ORIGIN: ${SYNARA_ARTIFACT_CORS_ALLOW_ORIGIN:?Set the exact browser origin",
        "Compose MinIO service",
    )
    if "SYNARA_ARTIFACT_CORS_ALLOW_ORIGIN:-" in minio or "SYNARA_ARTIFACT_CORS_ALLOW_ORIGIN:-*" in minio:
        raise ArtifactStorageBoundaryError("Compose MinIO CORS must not have a wildcard or fallback origin")
    require_fragment(bootstrap_service, f"image: {PINNED_MC_IMAGE}", "Compose MinIO bootstrap")
    require_fragment(bootstrap_service, 'restart: "no"', "Compose MinIO bootstrap")
    require_fragment(bootstrap_service, 'entrypoint: ["/bin/sh", "/bootstrap/bootstrap.sh"]', "Compose MinIO bootstrap")
    for variable in ("MINIO_ROOT_USER", "MINIO_ROOT_PASSWORD", "MINIO_ARTIFACT_USER", "MINIO_ARTIFACT_PASSWORD"):
        require_fragment(bootstrap_service, f"{variable}:", "Compose MinIO bootstrap")
    require_fragment(
        control_plane,
        "minio-bootstrap:\n        condition: service_completed_successfully",
        "Compose Control Plane",
    )
    for forbidden in ("MINIO_ROOT_USER", "MINIO_ROOT_PASSWORD"):
        if forbidden in control_plane:
            raise ArtifactStorageBoundaryError(f"Compose Control Plane must not receive {forbidden}")
    require_fragment(
        control_plane,
        "SYNARA_ARTIFACT_ACCESS_KEY_ID: ${MINIO_ARTIFACT_USER}",
        "Compose Control Plane",
    )
    require_fragment(
        control_plane,
        "SYNARA_ARTIFACT_SECRET_ACCESS_KEY: ${MINIO_ARTIFACT_PASSWORD}",
        "Compose Control Plane",
    )

    for fragment in (
        'if [ "${MINIO_ARTIFACT_USER}" = "${MINIO_ROOT_USER}" ]',
        '"${MINIO_ARTIFACT_PASSWORD}" = "${MINIO_ROOT_PASSWORD}"',
        'while IFS= read -r policy_line || [ -n "${policy_line}" ]; do',
        'policy_line=${policy_line//__SYNARA_ARTIFACT_BUCKET__/${SYNARA_ARTIFACT_BUCKET}}',
        'mc anonymous set none "synara/${SYNARA_ARTIFACT_BUCKET}"',
        "mc admin policy create synara synara-artifact-control-plane",
        'mc admin user add synara "${MINIO_ARTIFACT_USER}" "${MINIO_ARTIFACT_PASSWORD}"',
        'mc admin policy attach synara synara-artifact-control-plane --user "${MINIO_ARTIFACT_USER}"',
    ):
        require_fragment(bootstrap, fragment, "MinIO bootstrap script")
    validate_policy(policy_path)

    require_fragment(deployment, "serviceAccountName: synara-control-plane", "Kubernetes Deployment")
    for forbidden in (
        "SYNARA_ARTIFACT_ACCESS_KEY_ID",
        "SYNARA_ARTIFACT_SECRET_ACCESS_KEY",
        "SYNARA_ARTIFACT_SESSION_TOKEN",
    ):
        if forbidden in deployment:
            raise ArtifactStorageBoundaryError(f"Kubernetes base must not inject {forbidden}")
    for forbidden in ("artifact-access-key-id", "artifact-secret-access-key", "artifact-session-token"):
        if forbidden in secret_example:
            raise ArtifactStorageBoundaryError(f"Kubernetes example Secret must not contain {forbidden}")
    for fragment in (
        "name: synara-control-plane",
        "Production overlays bind this exact ServiceAccount",
        "Artifact bucket's tenants/* prefix",
    ):
        require_fragment(service_account, fragment, "Kubernetes ServiceAccount")

    for fragment in (
        "cfg.ArtifactPresignTTL > 15*time.Minute",
        "enterprise Artifact static credentials must be temporary and include SYNARA_ARTIFACT_SESSION_TOKEN",
        'validateArtifactEndpoint("SYNARA_ARTIFACT_ENDPOINT"',
        'validateArtifactEndpoint("SYNARA_ARTIFACT_PUBLIC_ENDPOINT"',
        'return fmt.Errorf("%s must use HTTPS for enterprise deployments", name)',
    ):
        require_fragment(config_source, fragment, "Control Plane Artifact configuration")
    for fragment in (
        "parsed.User != nil",
        'return "", false, fmt.Errorf("SYNARA_ARTIFACT_ENDPOINT must be an HTTP(S) origin without a path")',
    ):
        require_fragment(s3_source, fragment, "S3 endpoint normalization")

    return {
        "schemaVersion": SCHEMA_VERSION,
        "compose": {
            "applicationIdentity": "dedicated-non-root-minio-user",
            "cors": "exact-origin-required",
            "policyResource": "bucket/tenants/*",
        },
        "kubernetes": {
            "credentials": "workload-identity-no-static-artifact-secret",
            "serviceAccount": "synara-control-plane",
        },
        "runtime": {
            "enterpriseEndpoints": "https-only",
            "enterpriseStaticCredentials": "temporary-session-token-required",
            "presignMaximum": "15m",
        },
        "assessment": ASSESSMENT,
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repository-root", default=".")
    args = parser.parse_args()
    try:
        receipt = validate_repository(pathlib.Path(args.repository_root).resolve())
    except ArtifactStorageBoundaryError as error:
        print(f"artifact storage deployment validation failed: {error}", file=sys.stderr)
        return 2
    print(json.dumps(receipt, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
