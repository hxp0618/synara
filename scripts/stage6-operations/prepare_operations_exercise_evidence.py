#!/usr/bin/env python3
"""Materialize, validate and atomically publish one Stage 6 browser-operations evidence bundle."""

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

from validate_operations_exercise_evidence import (  # noqa: E402
    MAX_EVIDENCE_FILE_BYTES,
    MAX_MANIFEST_BYTES,
    SCHEMA_VERSION,
    OperationsExerciseError,
    load_expected_operations,
    reject_duplicate_json_fields,
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


DRAFT_SCHEMA_VERSION = "synara.stage6-operations-browser-exercise-draft.v2"
DRAFT_FIELDS = {
    "schemaVersion",
    "candidate",
    "startedAt",
    "completedAt",
    "accounts",
    "operations",
    "supportAccess",
    "approvals",
}


class OperationsPreparationError(Exception):
    pass


def relative_path(value: Any, label: str) -> pathlib.PurePosixPath:
    if not isinstance(value, str) or not value or "\\" in value or "\x00" in value:
        raise OperationsPreparationError(f"{label} must be a non-empty POSIX relative path")
    path = pathlib.PurePosixPath(value)
    if path.is_absolute() or not path.parts or any(part in {"", ".", ".."} for part in path.parts):
        raise OperationsPreparationError(f"{label} must be traversal-free and relative")
    return path


def resolve_input(root: pathlib.Path, value: Any, label: str, maximum_bytes: int) -> pathlib.Path:
    relative = relative_path(value, label)
    candidate = root.joinpath(*relative.parts)
    current = candidate
    while current != root:
        if current.is_symlink():
            raise OperationsPreparationError(f"{label} must not traverse a symlink")
        current = current.parent
    try:
        resolved = candidate.resolve(strict=True)
        resolved.relative_to(root)
        read_stable_regular_file(resolved, label=label, maximum_bytes=maximum_bytes)
    except (OSError, ValueError, ImmutableEvidenceIOError) as error:
        raise OperationsPreparationError(
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
            raise OperationsPreparationError(f"{label} must not traverse a symlink")
        current = current.parent
    try:
        parent.resolve(strict=True).relative_to(root)
    except (OSError, ValueError) as error:
        raise OperationsPreparationError(
            f"{label} parent must be an existing non-symlink directory inside evidence root"
        ) from error
    if not parent.is_dir() or output.exists() or output.is_symlink():
        raise OperationsPreparationError(f"{label} must be a new path below an existing directory")
    return output


def path_reference(root: pathlib.Path, value: Any, label: str) -> dict[str, str]:
    path = resolve_input(root, value, label, MAX_EVIDENCE_FILE_BYTES)
    try:
        content = read_stable_regular_file(
            path,
            label=label,
            maximum_bytes=MAX_EVIDENCE_FILE_BYTES,
        )
    except ImmutableEvidenceIOError as error:
        raise OperationsPreparationError(f"{label} changed while being materialized") from error
    try:
        scan_for_secret_material(content, label)
    except OperationsExerciseError as error:
        raise OperationsPreparationError(str(error)) from error
    return {
        "path": path.relative_to(root).as_posix(),
        "sha256": "sha256:" + hashlib.sha256(content).hexdigest(),
    }


def materialize_manifest(
    draft: dict[str, Any],
    root: pathlib.Path,
    repository_root: pathlib.Path,
    matrix_path: pathlib.Path,
) -> dict[str, Any]:
    prepared = copy.deepcopy(draft)
    if set(prepared) != DRAFT_FIELDS:
        raise OperationsPreparationError("draft fields do not match the operations draft schema")
    if prepared.get("schemaVersion") != DRAFT_SCHEMA_VERSION:
        raise OperationsPreparationError(f"draft schemaVersion must be {DRAFT_SCHEMA_VERSION}")
    prepared["schemaVersion"] = SCHEMA_VERSION
    try:
        _, matrix_receipt = load_expected_operations(repository_root, matrix_path)
    except OperationsExerciseError as error:
        raise OperationsPreparationError(str(error)) from error
    prepared["matrixSha256"] = matrix_receipt["sha256"]

    operations = prepared.get("operations")
    if not isinstance(operations, list):
        raise OperationsPreparationError("operations must be an array")
    for index, operation in enumerate(operations):
        if not isinstance(operation, dict):
            raise OperationsPreparationError(f"operations[{index}] must be an object")
        for field in ("positiveEvidence", "negativeEvidence"):
            if field not in operation:
                raise OperationsPreparationError(f"operations[{index}].{field} is required")
            operation[field] = path_reference(
                root,
                operation[field],
                f"operations[{index}].{field}",
            )

    support_access = prepared.get("supportAccess")
    if not isinstance(support_access, dict) or "evidence" not in support_access:
        raise OperationsPreparationError("supportAccess.evidence is required")
    support_access["evidence"] = path_reference(
        root,
        support_access["evidence"],
        "supportAccess.evidence",
    )

    approvals = prepared.get("approvals")
    if not isinstance(approvals, list):
        raise OperationsPreparationError("approvals must be an array")
    for index, approval in enumerate(approvals):
        if not isinstance(approval, dict) or "evidence" not in approval:
            raise OperationsPreparationError(f"approvals[{index}].evidence is required")
        approval["evidence"] = path_reference(
            root,
            approval["evidence"],
            f"approvals[{index}].evidence",
        )
    return prepared


def encode_json(value: dict[str, Any]) -> bytes:
    return (json.dumps(value, indent=2, sort_keys=True) + "\n").encode("utf-8")


def validate_evidence_root(value: str) -> pathlib.Path:
    supplied = pathlib.Path(value).absolute()
    if supplied.is_symlink():
        raise OperationsPreparationError("evidence root must be a non-symlink directory")
    try:
        root = supplied.resolve(strict=True)
    except OSError as error:
        raise OperationsPreparationError("evidence root does not exist") from error
    if not root.is_dir():
        raise OperationsPreparationError("evidence root must be a directory")
    return root


def prepare(args: argparse.Namespace) -> tuple[pathlib.Path, pathlib.Path, dict[str, Any]]:
    root = validate_evidence_root(args.evidence_root)
    repository_root = pathlib.Path(args.repository_root).resolve()
    matrix_path = pathlib.Path(args.matrix)
    if not matrix_path.is_absolute():
        matrix_path = repository_root / matrix_path
    matrix_path = matrix_path.resolve()
    draft_path = resolve_input(root, args.draft, "operations draft", MAX_MANIFEST_BYTES)
    try:
        draft_bytes = read_stable_regular_file(
            draft_path,
            label="operations draft",
            maximum_bytes=MAX_MANIFEST_BYTES,
        )
        scan_for_secret_material(draft_bytes, "operations draft")
        draft = json.loads(draft_bytes, object_pairs_hook=reject_duplicate_json_fields)
    except (
        UnicodeDecodeError,
        json.JSONDecodeError,
        OperationsExerciseError,
        ImmutableEvidenceIOError,
    ) as error:
        raise OperationsPreparationError(
            "operations draft must contain safe valid UTF-8 JSON"
        ) from error
    if not isinstance(draft, dict):
        raise OperationsPreparationError("operations draft must be an object")
    manifest = materialize_manifest(draft, root, repository_root, matrix_path)
    manifest_data = encode_json(manifest)
    if len(manifest_data) > MAX_MANIFEST_BYTES:
        raise OperationsPreparationError("materialized operations manifest exceeds the size limit")

    manifest_output = resolve_output(root, args.manifest_output, "manifest output")
    receipt_output = resolve_output(root, args.receipt_output, "receipt output")
    output_paths = {
        manifest_output,
        manifest_output.with_suffix(manifest_output.suffix + ".sha256"),
        receipt_output,
        receipt_output.with_suffix(receipt_output.suffix + ".sha256"),
    }
    if len(output_paths) != 4 or draft_path in output_paths:
        raise OperationsPreparationError(
            "draft, manifest and receipt output paths must be distinct"
        )
    for output in output_paths:
        if output.exists() or output.is_symlink():
            raise OperationsPreparationError(f"output already exists: {output.name}")

    temporary_path: pathlib.Path | None = None
    try:
        with tempfile.NamedTemporaryFile(
            mode="wb",
            prefix=".stage6-operations-",
            suffix=".json",
            dir=root,
            delete=False,
        ) as temporary:
            temporary.write(manifest_data)
            temporary.flush()
            temporary_path = pathlib.Path(temporary.name)
        temporary_path.chmod(0o600)
        receipt = validate(
            temporary_path,
            root,
            repository_root,
            matrix_path,
            args.validated_at,
        )
    except (OSError, OperationsExerciseError, ImmutableEvidenceIOError) as error:
        raise OperationsPreparationError(str(error)) from error
    finally:
        if temporary_path is not None:
            temporary_path.unlink(missing_ok=True)
    receipt_data = encode_json(receipt)

    manifest_published = False
    try:
        publish_immutable_with_sha256(manifest_output, manifest_data)
        manifest_published = True
        publish_immutable_with_sha256(receipt_output, receipt_data)
    except ImmutableEvidenceIOError as error:
        if manifest_published:
            manifest_output.unlink(missing_ok=True)
            manifest_output.with_suffix(manifest_output.suffix + ".sha256").unlink(
                missing_ok=True
            )
        raise OperationsPreparationError(str(error)) from error
    return manifest_output, receipt_output, receipt


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--evidence-root", required=True)
    parser.add_argument("--draft", required=True)
    parser.add_argument("--repository-root", default=".")
    parser.add_argument(
        "--matrix",
        default="docs/release-matrices/stage-6-operations-ui-v1.json",
    )
    parser.add_argument("--manifest-output", required=True)
    parser.add_argument("--receipt-output", required=True)
    parser.add_argument("--validated-at", help=argparse.SUPPRESS)
    return parser


def main() -> int:
    parser = build_parser()
    try:
        manifest_path, receipt_path, receipt = prepare(parser.parse_args())
    except OperationsPreparationError as error:
        parser.exit(2, f"operations exercise preparation failed: {error}\n")
    print(
        f"wrote immutable {manifest_path} and {receipt_path}; "
        f"eligibleForHumanGateReview={str(receipt['eligibleForHumanGateReview']).lower()}"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
