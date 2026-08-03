#!/usr/bin/env python3
"""Fail closed when the Stage 6 release compatibility baseline drifts from source."""

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
    read_stable_regular_file,
)


class CompatibilityError(Exception):
    pass


EXPECTED_TOP_LEVEL = {
    "assessment",
    "components",
    "database",
    "matrixVersion",
    "releaseCoupling",
    "rolloutOrder",
    "schemaVersion",
    "scope",
}
EXPECTED_ROLLOUT_ORDER = [
    "collect-and-approve-artifact-digests",
    "backup-and-verify-restore-point",
    "apply-additive-migrations",
    "roll-control-plane-web-admin-and-publish-desktop",
    "canary-then-promote-worker-release",
    "verify-slo-audit-and-rollback-window",
]
MIGRATION_NAME_PATTERN = re.compile(r"^(?P<version>\d{6})_[a-z0-9_]+\.sql$")
MAX_MATRIX_BYTES = 1024 * 1024
MAX_SOURCE_FILE_BYTES = 8 * 1024 * 1024
MAX_TOTAL_SOURCE_BYTES = 32 * 1024 * 1024
PROHIBITED_SECRET_PATTERNS = (
    ("private key", re.compile(rb"-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----")),
    ("AWS access key", re.compile(rb"\bAKIA[0-9A-Z]{16}\b")),
    (
        "bearer credential",
        re.compile(rb"(?i)authorization\s*[:=]\s*bearer\s+[^\s,;]+"),
    ),
    (
        "credential-bearing URL",
        re.compile(rb"(?i)\b(?:https?|postgres(?:ql)?|mysql)://[^\s/:@]+:[^\s/@]+@"),
    ),
)


def reject_duplicate_json_fields(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise CompatibilityError(f"JSON input contains duplicate field {key!r}")
        result[key] = value
    return result


def scan_for_secret_material(content: bytes, label: str) -> None:
    for secret_label, pattern in PROHIBITED_SECRET_PATTERNS:
        if pattern.search(content):
            raise CompatibilityError(
                f"{label} contains prohibited {secret_label} material"
            )


def resolve_repository_file(
    root: pathlib.Path, relative: str, label: str
) -> pathlib.Path:
    relative_path = pathlib.PurePosixPath(relative)
    if (
        not relative
        or "\\" in relative
        or "\x00" in relative
        or relative_path.is_absolute()
        or any(part in {"", ".", ".."} for part in relative_path.parts)
    ):
        raise CompatibilityError(f"{label} must be a traversal-free relative path")
    candidate = root.joinpath(*relative_path.parts)
    current = candidate
    while current != root:
        if current.is_symlink():
            raise CompatibilityError(f"{label} must not traverse a symlink")
        current = current.parent
    try:
        resolved = candidate.resolve(strict=True)
        resolved.relative_to(root)
    except (OSError, ValueError) as error:
        raise CompatibilityError(f"{label} must resolve inside repository root") from error
    return resolved


def read_source_bytes(
    root: pathlib.Path,
    relative: str,
    cache: dict[pathlib.Path, bytes],
    total_bytes: list[int],
) -> bytes:
    path = resolve_repository_file(root, relative, f"required source {relative}")
    cached = cache.get(path)
    if cached is not None:
        return cached
    try:
        content = read_stable_regular_file(
            path,
            label=f"required source {relative}",
            maximum_bytes=MAX_SOURCE_FILE_BYTES,
        )
    except ImmutableEvidenceIOError as error:
        raise CompatibilityError(str(error)) from error
    total_bytes[0] += len(content)
    if total_bytes[0] > MAX_TOTAL_SOURCE_BYTES:
        raise CompatibilityError("compatibility source set exceeds the bounded total size")
    scan_for_secret_material(content, f"required source {relative}")
    cache[path] = content
    return content


def read_text(
    root: pathlib.Path,
    relative: str,
    cache: dict[pathlib.Path, bytes],
    total_bytes: list[int],
) -> str:
    try:
        return read_source_bytes(root, relative, cache, total_bytes).decode("utf-8")
    except UnicodeDecodeError as error:
        raise CompatibilityError(
            f"required source file is not UTF-8: {relative}"
        ) from error


def read_json(
    root: pathlib.Path,
    relative: str,
    cache: dict[pathlib.Path, bytes],
    total_bytes: list[int],
) -> dict[str, Any]:
    try:
        value = json.loads(
            read_source_bytes(root, relative, cache, total_bytes),
            object_pairs_hook=reject_duplicate_json_fields,
        )
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise CompatibilityError(f"required JSON file is invalid: {relative}") from error
    if not isinstance(value, dict):
        raise CompatibilityError(f"required JSON root is not an object: {relative}")
    return value


def source_integer(source: str, name: str, relative: str) -> int:
    match = re.search(rf"\b{re.escape(name)}\s*=\s*(\d+)\b", source)
    if match is None:
        raise CompatibilityError(f"could not read {name} from {relative}")
    return int(match.group(1))


def require_exact_keys(value: Any, keys: set[str], label: str) -> dict[str, Any]:
    if not isinstance(value, dict) or set(value) != keys:
        raise CompatibilityError(f"{label} fields do not match the v1 schema")
    return value


def require_equal(actual: Any, expected: Any, label: str) -> None:
    if actual != expected:
        raise CompatibilityError(f"{label} is {actual!r}; source requires {expected!r}")


def validate_migration_lineage(migrations: list[pathlib.Path]) -> None:
    if not migrations:
        raise CompatibilityError("Control Plane migration directory is empty")
    migration_names = [path.name for path in migrations]
    if migration_names != sorted(migration_names) or len(migration_names) != len(
        set(migration_names)
    ):
        raise CompatibilityError("Control Plane migration names are not a unique ascending lineage")

    versions: dict[int, str] = {}
    for migration in migrations:
        match = MIGRATION_NAME_PATTERN.fullmatch(migration.name)
        if match is None:
            raise CompatibilityError(f"invalid Control Plane migration filename: {migration.name}")
        version = int(match.group("version"))
        previous = versions.get(version)
        if previous is not None:
            raise CompatibilityError(
                f"duplicate Control Plane migration version {version}: {previous} and {migration.name}"
            )
        versions[version] = migration.name


def validate(root: pathlib.Path, matrix_path: pathlib.Path) -> dict[str, Any]:
    supplied_root = root.absolute()
    if supplied_root.is_symlink():
        raise CompatibilityError("repository root must not be a symlink")
    root = supplied_root.resolve()
    if not root.is_dir():
        raise CompatibilityError("repository root does not exist")
    try:
        matrix_raw = read_stable_regular_file(
            matrix_path,
            label="compatibility matrix",
            maximum_bytes=MAX_MATRIX_BYTES,
        )
        scan_for_secret_material(matrix_raw, "compatibility matrix")
        matrix = json.loads(
            matrix_raw,
            object_pairs_hook=reject_duplicate_json_fields,
        )
    except ImmutableEvidenceIOError as error:
        raise CompatibilityError(str(error)) from error
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise CompatibilityError("matrix must be valid UTF-8 JSON") from error
    source_cache: dict[pathlib.Path, bytes] = {}
    total_source_bytes = [0]
    matrix = require_exact_keys(matrix, EXPECTED_TOP_LEVEL, "matrix")
    require_equal(matrix["schemaVersion"], "synara.release-compatibility-matrix.v1", "schemaVersion")
    require_equal(matrix["matrixVersion"], 1, "matrixVersion")
    require_equal(matrix["scope"], "enterprise-internal-self-hosted", "scope")
    require_equal(matrix["assessment"], "design-current-not-release-approved", "assessment")
    require_equal(matrix["rolloutOrder"], EXPECTED_ROLLOUT_ORDER, "rolloutOrder")

    components = require_exact_keys(
        matrix["components"],
        {
            "controlPlane",
            "packages",
            "providerHostProtocol",
            "runtimeEvent",
            "workerManifestStorageSchema",
            "workerProtocol",
        },
        "components",
    )
    control_plane = require_exact_keys(components["controlPlane"], {"apiMajor", "sourceCommitRequired"}, "controlPlane")
    require_equal(control_plane["apiMajor"], 1, "controlPlane.apiMajor")
    require_equal(control_plane["sourceCommitRequired"], True, "controlPlane.sourceCommitRequired")
    if re.search(
        r'"(?:GET|POST|PUT|PATCH|DELETE) /v1/',
        read_text(
            root,
            "services/control-plane/internal/httpapi/server.go",
            source_cache,
            total_source_bytes,
        ),
    ) is None:
        raise CompatibilityError("Control Plane source no longer exposes the declared /v1 API major")

    package_paths = {
        "admin": "apps/admin/package.json",
        "controlPlaneClient": "packages/control-plane-client/package.json",
        "contracts": "packages/contracts/package.json",
        "desktop": "apps/desktop/package.json",
        "enterpriseUi": "packages/enterprise-ui/package.json",
        "providerHost": "apps/provider-host/package.json",
        "server": "apps/server/package.json",
        "web": "apps/web/package.json",
    }
    packages = require_exact_keys(components["packages"], set(package_paths), "components.packages")
    for key, path in package_paths.items():
        require_equal(
            packages[key],
            read_json(root, path, source_cache, total_source_bytes).get("version"),
            f"components.packages.{key}",
        )
    require_equal(packages["web"], packages["contracts"], "web/contracts release coupling")
    require_equal(packages["web"], packages["server"], "web/server release coupling")
    require_equal(packages["admin"], packages["contracts"], "admin/contracts release coupling")
    require_equal(packages["admin"], packages["web"], "admin/web release coupling")
    require_equal(packages["desktop"], packages["contracts"], "desktop/contracts release coupling")
    require_equal(packages["desktop"], packages["enterpriseUi"], "desktop/enterprise-ui release coupling")
    require_equal(
        packages["controlPlaneClient"], packages["contracts"], "control-plane-client/contracts release coupling"
    )
    require_equal(packages["enterpriseUi"], packages["web"], "enterprise-ui/web release coupling")

    execution_models_path = "services/control-plane/internal/executions/models.go"
    execution_models = read_text(
        root, execution_models_path, source_cache, total_source_bytes
    )
    worker_protocol_source = source_integer(execution_models, "WorkerProtocolVersion", execution_models_path)
    worker_protocol = require_exact_keys(components["workerProtocol"], {"minimum", "maximum"}, "workerProtocol")
    require_equal(worker_protocol, {"minimum": worker_protocol_source, "maximum": worker_protocol_source}, "workerProtocol")

    runtime_event = require_exact_keys(
        components["runtimeEvent"],
        {"controlPlaneReadMinimum", "controlPlaneReadMaximum", "managedWorkerMinimum", "managedWorkerMaximum"},
        "runtimeEvent",
    )
    runtime_v1 = source_integer(execution_models, "RuntimeEventVersionV1", execution_models_path)
    runtime_v2 = source_integer(execution_models, "RuntimeEventVersionV2", execution_models_path)
    require_equal(runtime_event["controlPlaneReadMinimum"], runtime_v1, "runtimeEvent.controlPlaneReadMinimum")
    require_equal(runtime_event["controlPlaneReadMaximum"], runtime_v2, "runtimeEvent.controlPlaneReadMaximum")
    require_equal(runtime_event["managedWorkerMinimum"], runtime_v2, "runtimeEvent.managedWorkerMinimum")
    require_equal(runtime_event["managedWorkerMaximum"], runtime_v2, "runtimeEvent.managedWorkerMaximum")

    manifests_path = "services/control-plane/internal/executions/manifests.go"
    manifests = read_text(root, manifests_path, source_cache, total_source_bytes)
    require_equal(
        components["workerManifestStorageSchema"],
        source_integer(manifests, "workerManifestStorageSchemaVersion", manifests_path),
        "workerManifestStorageSchema",
    )
    provider_host = require_exact_keys(
        components["providerHostProtocol"],
        {"acceptedMajor", "minimumAcceptedMinor", "producedMajor", "producedMinor"},
        "providerHostProtocol",
    )
    require_equal(
        provider_host["acceptedMajor"],
        source_integer(manifests, "providerHostProtocolMajor", manifests_path),
        "providerHostProtocol.acceptedMajor",
    )
    require_equal(
        provider_host["minimumAcceptedMinor"],
        source_integer(manifests, "providerHostProtocolMinimumMinor", manifests_path),
        "providerHostProtocol.minimumAcceptedMinor",
    )
    agentd_path = "services/control-plane/internal/agentd/provider_host_v2.go"
    agentd = read_text(root, agentd_path, source_cache, total_source_bytes)
    require_equal(
        provider_host["producedMajor"],
        source_integer(agentd, "providerHostProtocolMajor", agentd_path),
        "providerHostProtocol.producedMajor",
    )
    require_equal(
        provider_host["producedMinor"],
        source_integer(agentd, "providerHostProtocolMinor", agentd_path),
        "providerHostProtocol.producedMinor",
    )
    if provider_host["producedMajor"] != provider_host["acceptedMajor"] or provider_host["producedMinor"] < provider_host["minimumAcceptedMinor"]:
        raise CompatibilityError("produced Provider Host Protocol is outside the accepted range")

    database = require_exact_keys(matrix["database"], {"rollbackPolicy", "targetTail", "upgradePolicy"}, "database")
    require_equal(database["upgradePolicy"], "forward-only-expand-contract", "database.upgradePolicy")
    require_equal(
        database["rollbackPolicy"],
        "application-only-after-previous-build-target-schema-proof",
        "database.rollbackPolicy",
    )
    migrations_directory = root / "services/control-plane/migrations"
    if migrations_directory.is_symlink() or not migrations_directory.is_dir():
        raise CompatibilityError(
            "Control Plane migration directory must be a non-symlink directory"
        )
    migrations = sorted(
        (
            path
            for path in migrations_directory.iterdir()
            if path.name.endswith(".sql")
        ),
        key=lambda path: path.name,
    )
    validate_migration_lineage(migrations)
    migration_contents: dict[str, bytes] = {}
    for migration in migrations:
        relative = migration.relative_to(root).as_posix()
        migration_contents[migration.name] = read_source_bytes(
            root, relative, source_cache, total_source_bytes
        )
    tail = require_exact_keys(database["targetTail"], {"name", "sha256"}, "database.targetTail")
    require_equal(tail["name"], migrations[-1].name, "database.targetTail.name")
    require_equal(
        tail["sha256"],
        "sha256:" + hashlib.sha256(migration_contents[migrations[-1].name]).hexdigest(),
        "database.targetTail.sha256",
    )

    coupling = require_exact_keys(
        matrix["releaseCoupling"],
        {
            "adminAndContractsVersionMustMatch",
            "adminAndWebVersionMustMatch",
            "controlPlaneClientAndContractsVersionMustMatch",
            "desktopAndContractsVersionMustMatch",
            "desktopAndEnterpriseUiVersionMustMatch",
            "enterpriseUiAndWebVersionMustMatch",
            "managedWorkerGitCommitMustMatchRelease",
            "webAndContractsVersionMustMatch",
            "webAndServerVersionMustMatch",
        },
        "releaseCoupling",
    )
    for key, value in coupling.items():
        require_equal(value, True, f"releaseCoupling.{key}")

    return {
        "schemaVersion": "synara.release-compatibility-validation.v1",
        "matrixSha256": "sha256:" + hashlib.sha256(matrix_raw).hexdigest(),
        "sourceByteCount": total_source_bytes[0],
        "sourceFileCount": len(source_cache),
        "migrationTail": tail,
        "packages": packages,
        "protocols": {
            "providerHost": provider_host,
            "runtimeEvent": runtime_event,
            "worker": worker_protocol,
            "workerManifestStorageSchema": components["workerManifestStorageSchema"],
        },
        "assessment": "source-compatible-not-release-approved",
    }


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repository-root", default=".")
    parser.add_argument("--matrix", default="docs/release-matrices/stage-6-compatibility-v1.json")
    return parser


def main() -> int:
    parser = build_parser()
    args = parser.parse_args()
    root = pathlib.Path(args.repository_root).absolute()
    matrix = pathlib.Path(args.matrix)
    if not matrix.is_absolute():
        matrix = root / matrix
    try:
        result = validate(root, matrix.absolute())
    except CompatibilityError as error:
        parser.exit(2, f"compatibility validation failed: {error}\n")
    print(json.dumps(result, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
