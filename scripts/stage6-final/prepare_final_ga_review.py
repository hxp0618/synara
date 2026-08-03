#!/usr/bin/env python3
"""Prepare and validate one immutable Stage 6 final GA review archive."""

from __future__ import annotations

import argparse
import copy
import hashlib
import json
import pathlib
import tempfile
import sys
from typing import Any

from validate_final_ga_review import (
    FinalGAReviewError,
    MAX_EVIDENCE_BYTES,
    MAX_MANIFEST_BYTES,
    SCHEMA_VERSION,
    require_exact,
    validate,
    validate_evidence_root,
)

SCRIPTS_ROOT = pathlib.Path(__file__).resolve().parents[1]
if str(SCRIPTS_ROOT) not in sys.path:
    sys.path.insert(0, str(SCRIPTS_ROOT))

from stage6_common.immutable_evidence_io import (  # noqa: E402
    ImmutableEvidenceIOError,
    publish_immutable_with_sha256,
    read_stable_regular_file,
)


DRAFT_SCHEMA_VERSION = "synara.stage6-final-ga-review-draft.v1"
CANDIDATE_REFERENCE_FIELDS = (
    "candidateBundleReceipt",
    "protectedReleaseApproval",
    "finalAssetSet",
    "copiedChecklist",
    "changeNotice",
    "platformAuditExport",
)


class FinalGAReviewPreparationError(Exception):
    pass


def relative_path(value: Any, label: str) -> pathlib.PurePosixPath:
    if not isinstance(value, str) or not value or "\\" in value:
        raise FinalGAReviewPreparationError(f"{label} must be a non-empty POSIX relative path")
    path = pathlib.PurePosixPath(value)
    if path.is_absolute() or not path.parts or any(part in {"", ".", ".."} for part in path.parts):
        raise FinalGAReviewPreparationError(f"{label} must be traversal-free and relative")
    return path


def resolve_input(root: pathlib.Path, value: Any, label: str, maximum_bytes: int) -> pathlib.Path:
    relative = relative_path(value, label)
    candidate = root.joinpath(*relative.parts)
    current = candidate
    while current != root:
        if current.is_symlink():
            raise FinalGAReviewPreparationError(f"{label} must not traverse a symlink")
        current = current.parent
    try:
        resolved = candidate.resolve(strict=True)
        resolved.relative_to(root)
        read_stable_regular_file(resolved, label=label, maximum_bytes=maximum_bytes)
    except (OSError, ValueError, ImmutableEvidenceIOError) as error:
        raise FinalGAReviewPreparationError(f"{label} must be a stable bounded file inside evidenceRoot") from error
    return resolved


def resolve_output(root: pathlib.Path, value: Any, label: str) -> pathlib.Path:
    relative = relative_path(value, label)
    output = root.joinpath(*relative.parts)
    parent = output.parent
    current = parent
    while current != root:
        if current.is_symlink():
            raise FinalGAReviewPreparationError(f"{label} must not traverse a symlink")
        current = current.parent
    try:
        parent.resolve(strict=True).relative_to(root)
    except (OSError, ValueError) as error:
        raise FinalGAReviewPreparationError(
            f"{label} parent must be an existing non-symlink directory inside evidenceRoot"
        ) from error
    if not parent.is_dir() or output.exists() or output.is_symlink():
        raise FinalGAReviewPreparationError(f"{label} must be a new path below an existing directory")
    return output


def encode_json(value: dict[str, Any]) -> bytes:
    return (json.dumps(value, indent=2, sort_keys=True) + "\n").encode("utf-8")


def path_reference(root: pathlib.Path, value: Any, label: str) -> dict[str, str]:
    path = resolve_input(root, value, label, MAX_EVIDENCE_BYTES)
    encoded = read_stable_regular_file(path, label=label, maximum_bytes=MAX_EVIDENCE_BYTES)
    return {
        "path": path.relative_to(root).as_posix(),
        "sha256": "sha256:" + hashlib.sha256(encoded).hexdigest(),
    }


def materialize_manifest(draft: dict[str, Any], root: pathlib.Path) -> dict[str, Any]:
    prepared = copy.deepcopy(draft)
    if prepared.get("schemaVersion") != DRAFT_SCHEMA_VERSION:
        raise FinalGAReviewPreparationError(f"draft schemaVersion must be {DRAFT_SCHEMA_VERSION}")
    prepared["schemaVersion"] = SCHEMA_VERSION
    candidate = require_exact(
        prepared.get("candidate"),
        {
            "candidateId",
            "releaseTag",
            "sourceCommit",
            "environmentId",
            *CANDIDATE_REFERENCE_FIELDS,
        },
        "candidate",
    )
    for field in CANDIDATE_REFERENCE_FIELDS:
        candidate[field] = path_reference(root, candidate[field], f"candidate.{field}")

    controls = prepared.get("controlDecisions")
    if not isinstance(controls, list):
        raise FinalGAReviewPreparationError("controlDecisions must be a list")
    for index, raw_control in enumerate(controls):
        control = require_exact(
            raw_control,
            {"id", "ownerRole", "approverId", "status", "decidedAt", "evidence"},
            f"controlDecisions[{index}]",
        )
        if not isinstance(control["evidence"], list):
            raise FinalGAReviewPreparationError(f"controlDecisions[{index}].evidence must be a list")
        control["evidence"] = [
            path_reference(root, value, f"controlDecisions[{index}].evidence[{evidence_index}]")
            for evidence_index, value in enumerate(control["evidence"])
        ]

    approvals = prepared.get("finalApprovals")
    if not isinstance(approvals, list):
        raise FinalGAReviewPreparationError("finalApprovals must be a list")
    for index, raw_approval in enumerate(approvals):
        approval = require_exact(
            raw_approval,
            {"role", "approverId", "decision", "approvedAt", "evidence"},
            f"finalApprovals[{index}]",
        )
        approval["evidence"] = path_reference(
            root, approval["evidence"], f"finalApprovals[{index}].evidence"
        )
    return prepared


def prepare(args: argparse.Namespace) -> tuple[pathlib.Path, pathlib.Path, dict[str, Any]]:
    try:
        root = validate_evidence_root(pathlib.Path(args.evidence_root).absolute())
    except FinalGAReviewError as error:
        raise FinalGAReviewPreparationError(str(error)) from error
    draft_path = resolve_input(root, args.draft, "final GA review draft", MAX_MANIFEST_BYTES)
    try:
        draft = json.loads(
            read_stable_regular_file(
                draft_path, label="final GA review draft", maximum_bytes=MAX_MANIFEST_BYTES
            )
        )
    except (UnicodeDecodeError, json.JSONDecodeError, ImmutableEvidenceIOError) as error:
        raise FinalGAReviewPreparationError("final GA review draft must contain valid UTF-8 JSON") from error
    if not isinstance(draft, dict):
        raise FinalGAReviewPreparationError("final GA review draft must be an object")
    try:
        manifest = materialize_manifest(draft, root)
    except FinalGAReviewError as error:
        raise FinalGAReviewPreparationError(str(error)) from error
    manifest_data = encode_json(manifest)

    manifest_output = resolve_output(root, args.manifest_output, "manifest output")
    receipt_output = resolve_output(root, args.receipt_output, "receipt output")
    outputs = {
        manifest_output,
        manifest_output.with_suffix(manifest_output.suffix + ".sha256"),
        receipt_output,
        receipt_output.with_suffix(receipt_output.suffix + ".sha256"),
    }
    if len(outputs) != 4 or draft_path in outputs:
        raise FinalGAReviewPreparationError("draft, manifest and receipt output paths must be distinct")
    for output in outputs:
        if output.exists() or output.is_symlink():
            raise FinalGAReviewPreparationError(f"output already exists: {output.name}")

    temporary_path: pathlib.Path | None = None
    try:
        with tempfile.NamedTemporaryFile(
            mode="wb", prefix=".stage6-final-review-", suffix=".json", dir=root, delete=False
        ) as temporary:
            temporary.write(manifest_data)
            temporary.flush()
            temporary_path = pathlib.Path(temporary.name)
        temporary_path.chmod(0o600)
        receipt = validate(temporary_path, root, args.validated_at)
    except (OSError, FinalGAReviewError) as error:
        raise FinalGAReviewPreparationError(str(error)) from error
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
            manifest_output.with_suffix(manifest_output.suffix + ".sha256").unlink(missing_ok=True)
        raise FinalGAReviewPreparationError(str(error)) from error
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
    args = parser.parse_args()
    try:
        manifest_path, receipt_path, receipt = prepare(args)
    except FinalGAReviewPreparationError as error:
        parser.exit(2, f"final GA review preparation failed: {error}\n")
    print(
        f"wrote immutable {manifest_path} and {receipt_path}; "
        "eligibleForExternalGAAuthorityReview="
        f"{str(receipt['eligibleForExternalGAAuthorityReview']).lower()}"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
