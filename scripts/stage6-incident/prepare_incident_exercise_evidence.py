#!/usr/bin/env python3
"""Materialize, validate and atomically publish one Stage 6 internal incident exercise bundle."""

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

from validate_incident_exercise_evidence import (  # noqa: E402
    ASSESSMENT,
    EVIDENCE_FIELDS,
    MANIFEST_SCHEMA,
    MAX_EVIDENCE_FILE_BYTES,
    MAX_MANIFEST_BYTES,
    IncidentExerciseEvidenceError,
    reject_duplicate_json_fields,
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


DRAFT_SCHEMA = "synara.incident-communication-exercise-evidence-draft.v2"
MANIFEST_FIELDS = {
    "schemaVersion",
    "exerciseId",
    "releaseCommit",
    "environmentClass",
    "environmentId",
    "exerciseMode",
    "severity",
    "serviceOrigin",
    "internalStatusBoardOrigin",
    "internalStatusBoardOperationallyIndependent",
    "regionPromiseEnabled",
    "startedAt",
    "completedAt",
    "roles",
    "paging",
    "internalStatusBoardComponents",
    "internalTimeline",
    "employeeNotificationDelivery",
    "recoveryVerification",
    "review",
    "evidence",
}


class IncidentPreparationError(Exception):
    pass


def relative_path(value: Any, label: str) -> pathlib.PurePosixPath:
    if not isinstance(value, str) or not value or "\\" in value or "\x00" in value:
        raise IncidentPreparationError(f"{label} must be a non-empty POSIX relative path")
    path = pathlib.PurePosixPath(value)
    if path.is_absolute() or not path.parts or any(part in {"", ".", ".."} for part in path.parts):
        raise IncidentPreparationError(f"{label} must be traversal-free and relative")
    return path


def resolve_input(root: pathlib.Path, value: Any, label: str, maximum_bytes: int) -> pathlib.Path:
    relative = relative_path(value, label)
    candidate = root.joinpath(*relative.parts)
    current = candidate
    while current != root:
        if current.is_symlink():
            raise IncidentPreparationError(f"{label} must not traverse a symlink")
        current = current.parent
    try:
        resolved = candidate.resolve(strict=True)
        resolved.relative_to(root)
        read_stable_regular_file(resolved, label=label, maximum_bytes=maximum_bytes)
    except (OSError, ValueError, ImmutableEvidenceIOError) as error:
        raise IncidentPreparationError(
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
            raise IncidentPreparationError(f"{label} must not traverse a symlink")
        current = current.parent
    try:
        parent.resolve(strict=True).relative_to(root)
    except (OSError, ValueError) as error:
        raise IncidentPreparationError(
            f"{label} parent must be an existing non-symlink directory inside evidence root"
        ) from error
    if not parent.is_dir() or output.exists() or output.is_symlink():
        raise IncidentPreparationError(f"{label} must be a new path below an existing directory")
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
    except (ImmutableEvidenceIOError, IncidentExerciseEvidenceError) as error:
        raise IncidentPreparationError(str(error)) from error
    return {
        "path": path.relative_to(root).as_posix(),
        "sha256": "sha256:" + hashlib.sha256(content).hexdigest(),
    }


def materialize_manifest(draft: dict[str, Any], root: pathlib.Path) -> dict[str, Any]:
    prepared = copy.deepcopy(draft)
    if set(prepared) != MANIFEST_FIELDS:
        raise IncidentPreparationError("draft fields do not match the incident draft schema")
    if prepared.get("schemaVersion") != DRAFT_SCHEMA:
        raise IncidentPreparationError(f"draft schemaVersion must be {DRAFT_SCHEMA}")
    prepared["schemaVersion"] = MANIFEST_SCHEMA
    try:
        evidence = require_mapping(prepared.get("evidence"), "evidence")
    except IncidentExerciseEvidenceError as error:
        raise IncidentPreparationError(str(error)) from error
    if set(evidence) != set(EVIDENCE_FIELDS):
        raise IncidentPreparationError("evidence fields do not match the incident draft schema")
    prepared["evidence"] = {
        field: path_reference(root, evidence[field], f"evidence.{field}")
        for field in EVIDENCE_FIELDS
    }
    return prepared


def encode_json(value: dict[str, Any]) -> bytes:
    return (json.dumps(value, indent=2, sort_keys=True) + "\n").encode("utf-8")


def validate_evidence_root(value: str) -> pathlib.Path:
    supplied = pathlib.Path(value).absolute()
    if supplied.is_symlink():
        raise IncidentPreparationError("evidence root must be a non-symlink directory")
    try:
        root = supplied.resolve(strict=True)
    except OSError as error:
        raise IncidentPreparationError("evidence root does not exist") from error
    if not root.is_dir():
        raise IncidentPreparationError("evidence root must be a directory")
    return root


def prepare(args: argparse.Namespace) -> tuple[pathlib.Path, pathlib.Path, dict[str, Any]]:
    root = validate_evidence_root(args.evidence_root)
    draft_path = resolve_input(root, args.draft, "incident draft", MAX_MANIFEST_BYTES)
    try:
        draft_bytes = read_stable_regular_file(
            draft_path,
            label="incident draft",
            maximum_bytes=MAX_MANIFEST_BYTES,
        )
        scan_for_secret_material(draft_bytes, "incident draft")
        draft = json.loads(draft_bytes, object_pairs_hook=reject_duplicate_json_fields)
    except (
        UnicodeDecodeError,
        json.JSONDecodeError,
        IncidentExerciseEvidenceError,
        ImmutableEvidenceIOError,
    ) as error:
        raise IncidentPreparationError(
            "incident draft must contain safe valid UTF-8 JSON"
        ) from error
    if not isinstance(draft, dict):
        raise IncidentPreparationError("incident draft must be an object")
    manifest = materialize_manifest(draft, root)
    manifest_data = encode_json(manifest)
    if len(manifest_data) > MAX_MANIFEST_BYTES:
        raise IncidentPreparationError("materialized incident manifest exceeds the size limit")

    manifest_output = resolve_output(root, args.manifest_output, "manifest output")
    receipt_output = resolve_output(root, args.receipt_output, "receipt output")
    output_paths = {
        manifest_output,
        manifest_output.with_suffix(manifest_output.suffix + ".sha256"),
        receipt_output,
        receipt_output.with_suffix(receipt_output.suffix + ".sha256"),
    }
    if len(output_paths) != 4 or draft_path in output_paths:
        raise IncidentPreparationError("draft, manifest and receipt output paths must be distinct")
    for output in output_paths:
        if output.exists() or output.is_symlink():
            raise IncidentPreparationError(f"output already exists: {output.name}")

    temporary_path: pathlib.Path | None = None
    try:
        with tempfile.NamedTemporaryFile(
            mode="wb",
            prefix=".stage6-incident-",
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
            args.validated_at,
            manifest_output.relative_to(root).as_posix(),
        )
    except (OSError, IncidentExerciseEvidenceError, ImmutableEvidenceIOError) as error:
        raise IncidentPreparationError(str(error)) from error
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
        raise IncidentPreparationError(str(error)) from error
    return manifest_output, receipt_output, receipt


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--evidence-root", required=True)
    parser.add_argument("--draft", required=True)
    parser.add_argument("--manifest-output", required=True)
    parser.add_argument("--receipt-output", required=True)
    parser.add_argument("--validated-at", help=argparse.SUPPRESS)
    return parser


def main() -> int:
    parser = build_parser()
    try:
        manifest_path, receipt_path, receipt = prepare(parser.parse_args())
    except IncidentPreparationError as error:
        parser.exit(2, f"incident exercise preparation failed: {error}\n")
    print(
        f"wrote immutable {manifest_path} and {receipt_path}; "
        f"eligibleForHumanGateReview={str(receipt['eligibleForHumanGateReview']).lower()}"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
