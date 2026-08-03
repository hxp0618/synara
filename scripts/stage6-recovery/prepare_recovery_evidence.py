#!/usr/bin/env python3
"""Materialize and atomically publish a two-stage Stage 6 recovery subject or receipt."""

from __future__ import annotations

import argparse
import copy
import hashlib
import json
import pathlib
import sys
import tempfile
from typing import Any

CURRENT_DIRECTORY = pathlib.Path(__file__).resolve().parent
if str(CURRENT_DIRECTORY) not in sys.path:
    sys.path.insert(0, str(CURRENT_DIRECTORY))

from validate_recovery_evidence import (  # noqa: E402
    APPROVAL_ROLES,
    EVIDENCE_FIELDS,
    MAX_EVIDENCE_FILE_BYTES,
    MAX_MANIFEST_BYTES,
    SCHEMA_VERSION,
    RecoveryEvidenceError,
    reject_duplicate_json_fields,
    require_digest,
    require_mapping,
    scan_for_secret_material,
    validate,
)

SCRIPTS_ROOT = pathlib.Path(__file__).resolve().parents[1]
if str(SCRIPTS_ROOT) not in sys.path:
    sys.path.insert(0, str(SCRIPTS_ROOT))

from stage6_common.immutable_evidence_io import (  # noqa: E402
    ImmutableEvidenceIOError,
    publish_immutable_with_sha256,
    read_stable_regular_file,
)


DRAFT_SCHEMA_VERSION = "synara.recovery-drill-evidence-draft.v2"
MANIFEST_FIELDS = {
    "approvals",
    "candidate",
    "components",
    "drill",
    "restoredReleaseIdentity",
    "schemaVersion",
}
MODES = {"subject", "final"}


class RecoveryPreparationError(Exception):
    pass


def relative_path(value: Any, label: str) -> pathlib.PurePosixPath:
    if not isinstance(value, str) or not value or "\\" in value or "\x00" in value:
        raise RecoveryPreparationError(f"{label} must be a non-empty POSIX relative path")
    path = pathlib.PurePosixPath(value)
    if path.is_absolute() or not path.parts or any(part in {"", ".", ".."} for part in path.parts):
        raise RecoveryPreparationError(f"{label} must be traversal-free and relative")
    return path


def resolve_input(root: pathlib.Path, value: Any, label: str, maximum_bytes: int) -> pathlib.Path:
    relative = relative_path(value, label)
    candidate = root.joinpath(*relative.parts)
    current = candidate
    while current != root:
        if current.is_symlink():
            raise RecoveryPreparationError(f"{label} must not traverse a symlink")
        current = current.parent
    try:
        resolved = candidate.resolve(strict=True)
        resolved.relative_to(root)
        read_stable_regular_file(resolved, label=label, maximum_bytes=maximum_bytes)
    except (OSError, ValueError, ImmutableEvidenceIOError) as error:
        raise RecoveryPreparationError(
            f"{label} must be a stable bounded file inside evidence root"
        ) from error
    return resolved


def resolve_output(root: pathlib.Path, value: Any, label: str) -> pathlib.Path:
    relative = relative_path(value, label)
    output = root.joinpath(*relative.parts)
    parent = output.parent
    current = parent
    while current != root:
        if current.is_symlink():
            raise RecoveryPreparationError(f"{label} must not traverse a symlink")
        current = current.parent
    try:
        parent.resolve(strict=True).relative_to(root)
    except (OSError, ValueError) as error:
        raise RecoveryPreparationError(
            f"{label} parent must be an existing non-symlink directory inside evidence root"
        ) from error
    if not parent.is_dir() or output.exists() or output.is_symlink():
        raise RecoveryPreparationError(f"{label} must be a new path below an existing directory")
    return output


def path_reference(root: pathlib.Path, value: Any, label: str) -> dict[str, str]:
    path = resolve_input(root, value, label, MAX_EVIDENCE_FILE_BYTES)
    try:
        content = read_stable_regular_file(
            path,
            label=label,
            maximum_bytes=MAX_EVIDENCE_FILE_BYTES,
        )
        scan_for_secret_material(content, label)
    except (ImmutableEvidenceIOError, RecoveryEvidenceError) as error:
        raise RecoveryPreparationError(str(error)) from error
    return {
        "path": path.relative_to(root).as_posix(),
        "sha256": "sha256:" + hashlib.sha256(content).hexdigest(),
    }


def materialize_manifest(
    draft: dict[str, Any],
    root: pathlib.Path,
    mode: str,
) -> dict[str, Any]:
    prepared = copy.deepcopy(draft)
    if set(prepared) != MANIFEST_FIELDS:
        raise RecoveryPreparationError("draft fields do not match the recovery draft schema")
    if prepared.get("schemaVersion") != DRAFT_SCHEMA_VERSION:
        raise RecoveryPreparationError(
            f"draft schemaVersion must be {DRAFT_SCHEMA_VERSION}"
        )
    prepared["schemaVersion"] = SCHEMA_VERSION
    components = prepared.get("components")
    if not isinstance(components, list):
        raise RecoveryPreparationError("components must be an array")
    for index, component in enumerate(components):
        if not isinstance(component, dict):
            raise RecoveryPreparationError(f"components[{index}] must be an object")
        for field in EVIDENCE_FIELDS:
            if field not in component:
                raise RecoveryPreparationError(f"components[{index}].{field} is required")
            component[field] = path_reference(
                root,
                component[field],
                f"components[{index}].{field}",
            )

    try:
        approvals = require_mapping(prepared.get("approvals"), "approvals")
    except RecoveryEvidenceError as error:
        raise RecoveryPreparationError(str(error)) from error
    if mode == "subject":
        if approvals:
            raise RecoveryPreparationError(
                "subject draft approvals must be empty until the recovery subject is frozen"
            )
    else:
        if set(approvals) != APPROVAL_ROLES:
            raise RecoveryPreparationError(
                "final draft approvals must contain all five recovery roles"
            )
        prepared["approvals"] = {
            role: path_reference(root, approvals[role], f"approvals.{role}")
            for role in sorted(APPROVAL_ROLES)
        }
    return prepared


def encode_json(value: dict[str, Any]) -> bytes:
    return (json.dumps(value, indent=2, sort_keys=True) + "\n").encode("utf-8")


def validate_evidence_root(value: str) -> pathlib.Path:
    supplied = pathlib.Path(value).absolute()
    if supplied.is_symlink():
        raise RecoveryPreparationError("evidence root must be a non-symlink directory")
    try:
        root = supplied.resolve(strict=True)
    except OSError as error:
        raise RecoveryPreparationError("evidence root does not exist") from error
    if not root.is_dir():
        raise RecoveryPreparationError("evidence root must be a directory")
    return root


def prepare(args: argparse.Namespace) -> tuple[pathlib.Path, pathlib.Path, dict[str, Any]]:
    if args.mode not in MODES:
        raise RecoveryPreparationError("mode must be subject or final")
    root = validate_evidence_root(args.evidence_root)
    try:
        expected_candidate_binding = require_digest(
            args.expected_candidate_binding_sha256,
            "expected candidate binding",
        )
    except RecoveryEvidenceError as error:
        raise RecoveryPreparationError(str(error)) from error
    draft_path = resolve_input(root, args.draft, "recovery draft", MAX_MANIFEST_BYTES)
    try:
        draft_bytes = read_stable_regular_file(
            draft_path,
            label="recovery draft",
            maximum_bytes=MAX_MANIFEST_BYTES,
        )
        scan_for_secret_material(draft_bytes, "recovery draft")
        draft = json.loads(draft_bytes, object_pairs_hook=reject_duplicate_json_fields)
    except (
        UnicodeDecodeError,
        json.JSONDecodeError,
        RecoveryEvidenceError,
        ImmutableEvidenceIOError,
    ) as error:
        raise RecoveryPreparationError(
            "recovery draft must contain safe valid UTF-8 JSON"
        ) from error
    if not isinstance(draft, dict):
        raise RecoveryPreparationError("recovery draft must be an object")
    manifest = materialize_manifest(draft, root, args.mode)
    manifest_data = encode_json(manifest)
    if len(manifest_data) > MAX_MANIFEST_BYTES:
        raise RecoveryPreparationError("materialized recovery manifest exceeds the size limit")

    manifest_output = resolve_output(root, args.manifest_output, "manifest output")
    result_output = resolve_output(root, args.result_output, "result output")
    output_paths = {
        manifest_output,
        manifest_output.with_suffix(manifest_output.suffix + ".sha256"),
        result_output,
        result_output.with_suffix(result_output.suffix + ".sha256"),
    }
    if len(output_paths) != 4 or draft_path in output_paths:
        raise RecoveryPreparationError("draft, manifest and result output paths must be distinct")
    for output in output_paths:
        if output.exists() or output.is_symlink():
            raise RecoveryPreparationError(f"output already exists: {output.name}")

    temporary_path: pathlib.Path | None = None
    try:
        with tempfile.NamedTemporaryFile(
            mode="wb",
            prefix=".stage6-recovery-",
            suffix=".json",
            dir=root,
            delete=False,
        ) as temporary:
            temporary.write(manifest_data)
            temporary.flush()
            temporary_path = pathlib.Path(temporary.name)
        temporary_path.chmod(0o600)
        result = validate(
            temporary_path,
            root,
            args.validated_at,
            expected_candidate_binding,
            args.mode == "subject",
            manifest_output.relative_to(root).as_posix(),
        )
    except (OSError, RecoveryEvidenceError, ImmutableEvidenceIOError) as error:
        raise RecoveryPreparationError(str(error)) from error
    finally:
        if temporary_path is not None:
            temporary_path.unlink(missing_ok=True)
    result_data = encode_json(result)

    manifest_published = False
    try:
        publish_immutable_with_sha256(manifest_output, manifest_data)
        manifest_published = True
        publish_immutable_with_sha256(result_output, result_data)
    except ImmutableEvidenceIOError as error:
        if manifest_published:
            manifest_output.unlink(missing_ok=True)
            manifest_output.with_suffix(manifest_output.suffix + ".sha256").unlink(
                missing_ok=True
            )
        raise RecoveryPreparationError(str(error)) from error
    return manifest_output, result_output, result


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mode", choices=sorted(MODES), required=True)
    parser.add_argument("--evidence-root", required=True)
    parser.add_argument("--draft", required=True)
    parser.add_argument("--expected-candidate-binding-sha256", required=True)
    parser.add_argument("--manifest-output", required=True)
    parser.add_argument("--result-output", required=True)
    parser.add_argument("--validated-at", help=argparse.SUPPRESS)
    return parser


def main() -> int:
    parser = build_parser()
    try:
        manifest_path, result_path, result = prepare(parser.parse_args())
    except RecoveryPreparationError as error:
        parser.exit(2, f"recovery evidence preparation failed: {error}\n")
    print(
        f"wrote immutable {manifest_path} and {result_path}; "
        f"assessment={result['assessment']}"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
