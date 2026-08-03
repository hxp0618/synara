#!/usr/bin/env python3
"""Validate Stage 6 daily-operation UI-to-route source coverage without declaring the workflow passed."""

from __future__ import annotations

import argparse
import hashlib
import json
import pathlib
import re
import sys
from typing import Any

SCRIPTS_ROOT = pathlib.Path(__file__).resolve().parents[1]
if str(SCRIPTS_ROOT) not in sys.path:
    sys.path.insert(0, str(SCRIPTS_ROOT))

from stage6_common.immutable_evidence_io import (  # noqa: E402
    ImmutableEvidenceIOError,
    publish_immutable_with_sha256,
)


IDENTIFIER_RE = re.compile(r"^[a-z0-9][a-z0-9._-]{2,95}$")
REQUIRED_OPERATIONS = {
    "platform.tenant-provision",
    "platform.tenant-entitlement",
    "platform.tenant-overview",
    "platform.support-request",
    "platform.support-approve-revoke",
    "platform.release-candidate-lifecycle",
    "platform.release-role-decision",
    "platform.compliance-program-lifecycle",
    "platform.compliance-evidence-review",
    "platform.compliance-start-gate-decision",
    "platform.provider-commercial-lifecycle",
    "platform.provider-commercial-approval",
    "platform.governance-authority-lifecycle",
    "platform.incident-lifecycle",
    "platform.incident-internal-communication",
    "platform.incident-security-resolution",
    "platform.slo-window-import",
    "platform.slo-window-decision",
    "platform.recovery-drill-import",
    "platform.recovery-drill-decision",
    "platform.penetration-engagement-import",
    "platform.penetration-engagement-decision",
    "platform.capacity-run-import",
    "platform.capacity-run-decision",
    "platform.internal-cost-review-import",
    "platform.internal-cost-review-decision",
    "platform.incident-exercise-import",
    "platform.incident-exercise-decision",
    "support.enter-readonly",
    "tenant.lifecycle-transition",
    "tenant.deletion-request-recovery",
    "tenant.member-governance",
    "tenant.identity-governance",
    "tenant.service-account-governance",
    "tenant.credential-governance",
    "support.credential-read",
    "tenant.worker-governance",
    "tenant.worker-release",
    "user.tenant-self-service",
    "tenant.outbox-read",
    "tenant.outbox-replay",
    "tenant.audit-search-export",
    "tenant.legal-hold",
    "tenant.privacy-request",
    "tenant.data-export",
    "tenant.data-residency",
    "tenant.usage-quota",
    "tenant.support-policy",
    "tenant.support-diagnostic-export",
}
ACTORS = {
    "authenticated-user",
    "platform-operator",
    "platform-admin",
    "platform-owner",
    "support-engineer",
    "tenant-owner",
    "tenant-admin",
    "security-admin",
    "cost-admin",
    "auditor",
}
ACCESS_MODES = {"read", "write", "read-only", "four-eyes"}
SURFACES = {"tenant-web", "platform-admin"}
SURFACE_STATUSES = {"reachable", "pending-surface"}
PLATFORM_OPERATIONS = {
    "platform.tenant-provision",
    "platform.tenant-entitlement",
    "platform.tenant-overview",
    "platform.support-request",
    "platform.support-approve-revoke",
    "platform.release-candidate-lifecycle",
    "platform.release-role-decision",
    "platform.compliance-program-lifecycle",
    "platform.compliance-evidence-review",
    "platform.compliance-start-gate-decision",
    "platform.provider-commercial-lifecycle",
    "platform.provider-commercial-approval",
    "platform.governance-authority-lifecycle",
    "platform.incident-lifecycle",
    "platform.incident-internal-communication",
    "platform.incident-security-resolution",
    "platform.slo-window-import",
    "platform.slo-window-decision",
    "platform.recovery-drill-import",
    "platform.recovery-drill-decision",
    "platform.penetration-engagement-import",
    "platform.penetration-engagement-decision",
    "platform.capacity-run-import",
    "platform.capacity-run-decision",
    "platform.internal-cost-review-import",
    "platform.internal-cost-review-decision",
    "platform.incident-exercise-import",
    "platform.incident-exercise-decision",
    "support.enter-readonly",
}
EXPECTED_NEGATIVE_ACTORS = {
    "platform.tenant-overview": "tenant-admin",
    "platform.tenant-provision": "platform-operator",
    "platform.tenant-entitlement": "platform-operator",
    "user.tenant-self-service": "unauthenticated",
    "platform.support-request": "tenant-admin",
    "platform.support-approve-revoke": "platform-operator",
    "platform.release-candidate-lifecycle": "security-admin",
    "platform.release-role-decision": "platform-operator",
    "platform.compliance-program-lifecycle": "security-admin",
    "platform.compliance-evidence-review": "platform-operator",
    "platform.compliance-start-gate-decision": "platform-operator",
    "platform.provider-commercial-lifecycle": "security-admin",
    "platform.provider-commercial-approval": "platform-operator",
    "platform.governance-authority-lifecycle": "platform-admin",
    "platform.incident-lifecycle": "tenant-admin",
    "platform.incident-internal-communication": "tenant-admin",
    "platform.incident-security-resolution": "platform-operator",
    "platform.slo-window-import": "security-admin",
    "platform.slo-window-decision": "platform-operator",
    "platform.recovery-drill-import": "security-admin",
    "platform.recovery-drill-decision": "platform-operator",
    "platform.penetration-engagement-import": "security-admin",
    "platform.penetration-engagement-decision": "platform-operator",
    "platform.capacity-run-import": "security-admin",
    "platform.capacity-run-decision": "tenant-admin",
    "platform.internal-cost-review-import": "security-admin",
    "platform.internal-cost-review-decision": "tenant-admin",
    "platform.incident-exercise-import": "security-admin",
    "platform.incident-exercise-decision": "tenant-admin",
    "support.enter-readonly": "tenant-admin",
    "tenant.lifecycle-transition": "tenant-admin",
    "tenant.deletion-request-recovery": "tenant-admin",
    "tenant.member-governance": "auditor",
    "tenant.identity-governance": "tenant-member",
    "tenant.service-account-governance": "tenant-member",
    "tenant.credential-governance": "tenant-member",
    "support.credential-read": "tenant-member",
    "tenant.worker-governance": "auditor",
    "tenant.worker-release": "auditor",
    "tenant.outbox-read": "tenant-member",
    "tenant.outbox-replay": "auditor",
    "tenant.audit-search-export": "tenant-member",
    "tenant.legal-hold": "tenant-member",
    "tenant.privacy-request": "tenant-member",
    "tenant.data-export": "tenant-member",
    "tenant.data-residency": "tenant-member",
    "tenant.usage-quota": "auditor",
    "tenant.support-policy": "auditor",
    "tenant.support-diagnostic-export": "tenant-member",
}
NEGATIVE_ACTORS = ACTORS | {"tenant-member", "unauthenticated"}


class OperationsMatrixError(Exception):
    pass


def require_mapping(value: Any, label: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise OperationsMatrixError(f"{label} must be an object")
    return value


def sha256_file(path: pathlib.Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def resolve_source(root: pathlib.Path, value: Any, label: str) -> pathlib.Path:
    if not isinstance(value, str) or not value:
        raise OperationsMatrixError(f"{label}.path must be a non-empty relative path")
    relative = pathlib.Path(value)
    if relative.is_absolute() or any(part in {".", ".."} for part in relative.parts):
        raise OperationsMatrixError(f"{label}.path must be traversal-free and relative")
    candidate = root / relative
    current = candidate
    while current != root:
        if current.is_symlink():
            raise OperationsMatrixError(f"{label}.path must not traverse a symlink")
        current = current.parent
    try:
        resolved = candidate.resolve(strict=True)
        resolved.relative_to(root)
    except (OSError, ValueError) as error:
        raise OperationsMatrixError(f"{label}.path must resolve inside repositoryRoot") from error
    if not resolved.is_file():
        raise OperationsMatrixError(f"{label}.path must reference a regular file")
    return resolved


def validate_source_reference(
    value: Any,
    root: pathlib.Path,
    label: str,
    source_digests: dict[str, str],
) -> dict[str, Any]:
    reference = require_mapping(value, label)
    if set(reference) != {"path", "contains"}:
        raise OperationsMatrixError(f"{label} must contain exactly path and contains")
    path = resolve_source(root, reference["path"], label)
    markers = reference["contains"]
    if not isinstance(markers, list) or not markers:
        raise OperationsMatrixError(f"{label}.contains must be a non-empty list")
    text = path.read_text(encoding="utf-8")
    parsed_markers: list[str] = []
    for index, marker in enumerate(markers):
        if (
            not isinstance(marker, str)
            or not marker
            or len(marker) > 500
            or "\n" in marker
            or "\r" in marker
        ):
            raise OperationsMatrixError(f"{label}.contains[{index}] must be a bounded single-line marker")
        if marker in parsed_markers:
            raise OperationsMatrixError(f"{label}.contains markers must be unique")
        if marker not in text:
            raise OperationsMatrixError(f"{label}.contains marker is missing from {reference['path']}: {marker}")
        parsed_markers.append(marker)
    relative_path = path.relative_to(root).as_posix()
    source_digests[relative_path] = "sha256:" + sha256_file(path)
    return {"path": relative_path, "contains": parsed_markers}


def validate(matrix_path: pathlib.Path, repository_root: pathlib.Path) -> dict[str, Any]:
    root = repository_root.resolve()
    if repository_root.is_symlink() or not root.is_dir():
        raise OperationsMatrixError("repositoryRoot must be a regular non-symlink directory")
    try:
        matrix_bytes = matrix_path.read_bytes()
        raw = json.loads(matrix_bytes)
    except OSError as error:
        raise OperationsMatrixError("matrix could not be read") from error
    except json.JSONDecodeError as error:
        raise OperationsMatrixError("matrix must be valid JSON") from error
    matrix = require_mapping(raw, "matrix")
    if set(matrix) != {"schemaVersion", "surfaceBindings", "operations"}:
        raise OperationsMatrixError("matrix fields do not match the v2 schema")
    if matrix["schemaVersion"] != "synara.operations-ui-matrix.v2":
        raise OperationsMatrixError("schemaVersion is not synara.operations-ui-matrix.v2")
    operations = matrix["operations"]
    if not isinstance(operations, list):
        raise OperationsMatrixError("operations must be a list")

    seen: set[str] = set()
    source_digests: dict[str, str] = {}
    surface_bindings = matrix["surfaceBindings"]
    if not isinstance(surface_bindings, list) or not surface_bindings:
        raise OperationsMatrixError("surfaceBindings must be a non-empty list")
    operation_bindings: dict[str, dict[str, str]] = {}
    validated_bindings: list[dict[str, Any]] = []
    seen_binding_ids: set[str] = set()
    surface_counts = {surface: 0 for surface in sorted(SURFACES)}
    status_counts = {status: 0 for status in sorted(SURFACE_STATUSES)}
    for index, raw_binding in enumerate(surface_bindings):
        binding = require_mapping(raw_binding, f"surfaceBindings[{index}]")
        if set(binding) != {
            "id",
            "surface",
            "status",
            "operations",
            "registration",
            "entrypoint",
        }:
            raise OperationsMatrixError(
                f"surfaceBindings[{index}] fields do not match the v2 schema"
            )
        binding_id = binding["id"]
        if not isinstance(binding_id, str) or IDENTIFIER_RE.fullmatch(binding_id) is None:
            raise OperationsMatrixError(f"surfaceBindings[{index}].id is invalid")
        if binding_id in seen_binding_ids:
            raise OperationsMatrixError(f"duplicate surface binding id {binding_id}")
        seen_binding_ids.add(binding_id)
        surface = binding["surface"]
        status = binding["status"]
        if surface not in SURFACES:
            raise OperationsMatrixError(f"{binding_id}.surface is not allowed")
        if status not in SURFACE_STATUSES:
            raise OperationsMatrixError(f"{binding_id}.status is not allowed")
        binding_operations = binding["operations"]
        if (
            not isinstance(binding_operations, list)
            or not binding_operations
            or any(not isinstance(value, str) for value in binding_operations)
        ):
            raise OperationsMatrixError(f"{binding_id}.operations must be a non-empty string list")
        for operation_id in binding_operations:
            if operation_id not in REQUIRED_OPERATIONS:
                raise OperationsMatrixError(
                    f"{binding_id}.operations contains unexpected operation {operation_id}"
                )
            if operation_id in operation_bindings:
                raise OperationsMatrixError(
                    f"operation {operation_id} has more than one surface binding"
                )
            expected_surface = "platform-admin" if operation_id in PLATFORM_OPERATIONS else "tenant-web"
            expected_status = "reachable"
            if surface != expected_surface or status != expected_status:
                raise OperationsMatrixError(
                    f"operation {operation_id} must be {expected_status} on {expected_surface}"
                )
            operation_bindings[operation_id] = {
                "bindingId": binding_id,
                "surface": surface,
                "status": status,
            }
            surface_counts[surface] += 1
            status_counts[status] += 1
        references = {
            kind: validate_source_reference(
                binding[kind], root, f"{binding_id}.{kind}", source_digests
            )
            for kind in ("registration", "entrypoint")
        }
        validated_bindings.append(
            {
                "id": binding_id,
                "surface": surface,
                "status": status,
                "operations": sorted(binding_operations),
                **references,
            }
        )
    missing_bindings = sorted(REQUIRED_OPERATIONS - set(operation_bindings))
    if missing_bindings:
        raise OperationsMatrixError(
            "surface bindings do not cover required operations: " + ", ".join(missing_bindings)
        )

    validated_operations: list[dict[str, Any]] = []
    actor_counts = {actor: 0 for actor in sorted(ACTORS)}
    access_counts = {access: 0 for access in sorted(ACCESS_MODES)}
    for index, raw_operation in enumerate(operations):
        operation = require_mapping(raw_operation, f"operations[{index}]")
        if set(operation) != {
            "id",
            "actor",
            "negativeActor",
            "access",
            "ui",
            "client",
            "route",
            "cliRequired",
            "databaseClientRequired",
        }:
            raise OperationsMatrixError(f"operations[{index}] fields do not match the v1 schema")
        operation_id = operation["id"]
        if not isinstance(operation_id, str) or IDENTIFIER_RE.fullmatch(operation_id) is None:
            raise OperationsMatrixError(f"operations[{index}].id is invalid")
        if operation_id in seen:
            raise OperationsMatrixError(f"duplicate operation id {operation_id}")
        seen.add(operation_id)
        surface_binding = operation_bindings.get(operation_id)
        if surface_binding is None:
            raise OperationsMatrixError(
                f"operation {operation_id} has no reviewed surface binding"
            )
        actor = operation["actor"]
        if actor not in ACTORS:
            raise OperationsMatrixError(f"{operation_id}.actor is not allowed")
        negative_actor = operation["negativeActor"]
        if (
            negative_actor not in NEGATIVE_ACTORS
            or negative_actor == actor
            or negative_actor != EXPECTED_NEGATIVE_ACTORS.get(operation_id)
        ):
            raise OperationsMatrixError(
                f"{operation_id}.negativeActor does not match the reviewed denial matrix"
            )
        access = operation["access"]
        if access not in ACCESS_MODES:
            raise OperationsMatrixError(f"{operation_id}.access is not allowed")
        if operation["cliRequired"] is not False or operation["databaseClientRequired"] is not False:
            raise OperationsMatrixError(f"{operation_id} still requires CLI or database-client access")
        references = {
            kind: validate_source_reference(
                operation[kind], root, f"{operation_id}.{kind}", source_digests
            )
            for kind in ("ui", "client", "route")
        }
        actor_counts[actor] += 1
        access_counts[access] += 1
        validated_operations.append(
            {
                "id": operation_id,
                "actor": actor,
                "negativeActor": negative_actor,
                "access": access,
                **surface_binding,
                **references,
            }
        )
    missing = sorted(REQUIRED_OPERATIONS - seen)
    unexpected = sorted(seen - REQUIRED_OPERATIONS)
    if missing or unexpected:
        raise OperationsMatrixError(
            f"operation inventory drifted; missing={missing}, unexpected={unexpected}"
        )
    try:
        matrix_relative = matrix_path.resolve(strict=True).relative_to(root).as_posix()
    except (OSError, ValueError) as error:
        raise OperationsMatrixError("matrix must resolve inside repositoryRoot") from error
    return {
        "schemaVersion": "synara.operations-ui-matrix-validation.v2",
        "matrix": {
            "path": matrix_relative,
            "sha256": "sha256:" + hashlib.sha256(matrix_bytes).hexdigest(),
        },
        "operationCount": len(validated_operations),
        "operations": sorted(validated_operations, key=lambda item: item["id"]),
        "surfaceBindings": sorted(validated_bindings, key=lambda item: item["id"]),
        "actorCounts": {key: value for key, value in actor_counts.items() if value > 0},
        "accessCounts": {key: value for key, value in access_counts.items() if value > 0},
        "surfaceCounts": {key: value for key, value in surface_counts.items() if value > 0},
        "statusCounts": {key: value for key, value in status_counts.items() if value > 0},
        "pendingOperations": [],
        "sources": [
            {"path": path, "sha256": digest}
            for path, digest in sorted(source_digests.items())
        ],
        "assessment": "source-ui-routes-validated-all-surfaces-reachable-not-operations-passed",
    }


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repository-root", required=True)
    parser.add_argument("--matrix", required=True)
    parser.add_argument("--output")
    return parser


def main() -> int:
    parser = build_parser()
    args = parser.parse_args()
    try:
        receipt = validate(
            pathlib.Path(args.matrix).resolve(),
            pathlib.Path(args.repository_root),
        )
        encoded = json.dumps(receipt, indent=2, sort_keys=True) + "\n"
        if args.output:
            output = pathlib.Path(args.output).absolute()
            publish_immutable_with_sha256(output, encoded.encode("utf-8"))
        else:
            print(encoded, end="")
    except (OperationsMatrixError, ImmutableEvidenceIOError) as error:
        parser.exit(2, f"operations UI matrix validation failed: {error}\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
