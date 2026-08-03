#!/usr/bin/env python3
"""Validate the Stage 6 documentation source set without declaring a release verified."""

from __future__ import annotations

import argparse
import hashlib
import json
import pathlib
import re
import sys
from typing import Any
from urllib.parse import unquote, urlsplit

SCRIPTS_ROOT = pathlib.Path(__file__).resolve().parents[1]
if str(SCRIPTS_ROOT) not in sys.path:
    sys.path.insert(0, str(SCRIPTS_ROOT))

from stage6_common.immutable_evidence_io import (  # noqa: E402
    ImmutableEvidenceIOError,
    publish_immutable_with_sha256,
    read_stable_regular_file,
)


IDENTIFIER_RE = re.compile(r"^[a-z0-9][a-z0-9-]{2,63}$")
MARKDOWN_LINK_RE = re.compile(r"\[[^\]]+\]\(([^)\s]+)(?:\s+['\"][^'\"]*['\"])?\)")
REQUIRED_DOCUMENTS = {
    "documentation-index": "all",
    "user-guide": "end-user",
    "administrator-guide": "tenant-administrator",
    "deployment-guide": "deployment-operator",
    "troubleshooting-guide": "support-operator",
}
MAX_MATRIX_BYTES = 1024 * 1024
MAX_DOCUMENT_BYTES = 4 * 1024 * 1024
MAX_TOTAL_DOCUMENT_BYTES = 16 * 1024 * 1024
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


class DocumentationMatrixError(Exception):
    pass


def reject_duplicate_json_fields(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise DocumentationMatrixError(
                f"matrix contains duplicate JSON field {key!r}"
            )
        result[key] = value
    return result


def scan_for_secret_material(content: bytes, label: str) -> None:
    for secret_label, pattern in PROHIBITED_SECRET_PATTERNS:
        if pattern.search(content):
            raise DocumentationMatrixError(
                f"{label} contains prohibited {secret_label} material"
            )


def require_mapping(value: Any, label: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise DocumentationMatrixError(f"{label} must be an object")
    return value


def sha256_bytes(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def resolve_repository_file(root: pathlib.Path, value: Any, label: str) -> pathlib.Path:
    if not isinstance(value, str) or not value:
        raise DocumentationMatrixError(f"{label} must be a non-empty relative path")
    relative = pathlib.Path(value)
    if relative.is_absolute() or any(part in {".", ".."} for part in relative.parts):
        raise DocumentationMatrixError(f"{label} must be traversal-free and relative")
    candidate = root / relative
    current = candidate
    while current != root:
        if current.is_symlink():
            raise DocumentationMatrixError(f"{label} must not traverse a symlink")
        current = current.parent
    try:
        resolved = candidate.resolve(strict=True)
        resolved.relative_to(root)
    except (OSError, ValueError) as error:
        raise DocumentationMatrixError(f"{label} must resolve inside repositoryRoot") from error
    if not resolved.is_file():
        raise DocumentationMatrixError(f"{label} must reference a regular file")
    return resolved


def validate_local_links(path: pathlib.Path, root: pathlib.Path, text: str, label: str) -> int:
    checked = 0
    for raw_target in MARKDOWN_LINK_RE.findall(text):
        target = raw_target.strip("<>")
        parsed = urlsplit(target)
        if parsed.scheme:
            if parsed.scheme != "https":
                raise DocumentationMatrixError(f"{label} has a non-HTTPS external link: {target}")
            continue
        if target.startswith("#"):
            continue
        if target.startswith("/") or parsed.netloc:
            raise DocumentationMatrixError(f"{label} has an unsupported absolute link: {target}")
        relative_target = pathlib.Path(unquote(parsed.path))
        candidate = path.parent / relative_target
        try:
            resolved = candidate.resolve(strict=True)
            resolved.relative_to(root)
        except (OSError, ValueError) as error:
            raise DocumentationMatrixError(f"{label} has a broken local link: {target}") from error
        if not resolved.is_file():
            raise DocumentationMatrixError(f"{label} local link is not a file: {target}")
        checked += 1
    return checked


def validate(matrix_path: pathlib.Path, repository_root: pathlib.Path) -> dict[str, Any]:
    root = repository_root.resolve()
    if repository_root.is_symlink() or not root.is_dir():
        raise DocumentationMatrixError("repositoryRoot must be a regular non-symlink directory")
    try:
        matrix_bytes = read_stable_regular_file(
            matrix_path,
            label="matrix",
            maximum_bytes=MAX_MATRIX_BYTES,
        )
        scan_for_secret_material(matrix_bytes, "matrix")
        raw = json.loads(
            matrix_bytes,
            object_pairs_hook=reject_duplicate_json_fields,
        )
    except ImmutableEvidenceIOError as error:
        raise DocumentationMatrixError(str(error)) from error
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise DocumentationMatrixError("matrix must be valid UTF-8 JSON") from error

    matrix = require_mapping(raw, "matrix")
    if set(matrix) != {"schemaVersion", "documents"}:
        raise DocumentationMatrixError("matrix fields do not match the v1 schema")
    if matrix["schemaVersion"] != "synara.enterprise-documentation-matrix.v1":
        raise DocumentationMatrixError(
            "schemaVersion is not synara.enterprise-documentation-matrix.v1"
        )
    documents = matrix["documents"]
    if not isinstance(documents, list):
        raise DocumentationMatrixError("documents must be a list")

    seen: set[str] = set()
    validated: list[dict[str, Any]] = []
    total_document_bytes = 0
    for index, raw_document in enumerate(documents):
        document = require_mapping(raw_document, f"documents[{index}]")
        if set(document) != {"id", "audience", "path", "contains"}:
            raise DocumentationMatrixError(f"documents[{index}] fields do not match the v1 schema")
        document_id = document["id"]
        if not isinstance(document_id, str) or IDENTIFIER_RE.fullmatch(document_id) is None:
            raise DocumentationMatrixError(f"documents[{index}].id is invalid")
        if document_id in seen:
            raise DocumentationMatrixError(f"duplicate document id {document_id}")
        seen.add(document_id)
        expected_audience = REQUIRED_DOCUMENTS.get(document_id)
        if document["audience"] != expected_audience:
            raise DocumentationMatrixError(f"{document_id}.audience is not {expected_audience}")

        path = resolve_repository_file(root, document["path"], f"{document_id}.path")
        if path.suffix != ".md":
            raise DocumentationMatrixError(f"{document_id}.path must reference Markdown")
        try:
            document_bytes = read_stable_regular_file(
                path,
                label=f"{document_id}.path",
                maximum_bytes=MAX_DOCUMENT_BYTES,
            )
        except ImmutableEvidenceIOError as error:
            raise DocumentationMatrixError(str(error)) from error
        total_document_bytes += len(document_bytes)
        if total_document_bytes > MAX_TOTAL_DOCUMENT_BYTES:
            raise DocumentationMatrixError(
                "documentation source set exceeds the bounded total size"
            )
        scan_for_secret_material(document_bytes, document_id)
        try:
            text = document_bytes.decode("utf-8")
        except UnicodeDecodeError as error:
            raise DocumentationMatrixError(
                f"{document_id}.path must be valid UTF-8 Markdown"
            ) from error
        markers = document["contains"]
        if not isinstance(markers, list) or not markers:
            raise DocumentationMatrixError(f"{document_id}.contains must be a non-empty list")
        parsed_markers: list[str] = []
        for marker_index, marker in enumerate(markers):
            if (
                not isinstance(marker, str)
                or not marker
                or len(marker) > 240
                or "\n" in marker
                or "\r" in marker
            ):
                raise DocumentationMatrixError(
                    f"{document_id}.contains[{marker_index}] must be a bounded single-line marker"
                )
            if marker in parsed_markers:
                raise DocumentationMatrixError(f"{document_id}.contains markers must be unique")
            if marker not in text:
                raise DocumentationMatrixError(
                    f"{document_id}.contains marker is missing from {document['path']}: {marker}"
                )
            parsed_markers.append(marker)
        relative_path = path.relative_to(root).as_posix()
        validated.append(
            {
                "id": document_id,
                "audience": document["audience"],
                "path": relative_path,
                "contains": parsed_markers,
                "localLinkCount": validate_local_links(path, root, text, document_id),
                "sizeBytes": len(document_bytes),
                "sha256": "sha256:" + sha256_bytes(document_bytes),
            }
        )

    missing = sorted(set(REQUIRED_DOCUMENTS) - seen)
    unexpected = sorted(seen - set(REQUIRED_DOCUMENTS))
    if missing or unexpected:
        raise DocumentationMatrixError(
            f"documentation inventory drifted; missing={missing}, unexpected={unexpected}"
        )
    try:
        matrix_relative = matrix_path.resolve(strict=True).relative_to(root).as_posix()
    except (OSError, ValueError) as error:
        raise DocumentationMatrixError("matrix must resolve inside repositoryRoot") from error
    return {
        "schemaVersion": "synara.enterprise-documentation-validation.v1",
        "matrix": {
            "path": matrix_relative,
            "sha256": "sha256:" + sha256_bytes(matrix_bytes),
        },
        "documentCount": len(validated),
        "documentByteCount": total_document_bytes,
        "documents": sorted(validated, key=lambda item: item["id"]),
        "assessment": "source-documentation-validated-not-release-verified",
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
        receipt = validate(pathlib.Path(args.matrix).absolute(), pathlib.Path(args.repository_root))
        encoded = json.dumps(receipt, indent=2, sort_keys=True) + "\n"
        if args.output:
            output = pathlib.Path(args.output).absolute()
            publish_immutable_with_sha256(output, encoded.encode("utf-8"))
        else:
            print(encoded, end="")
    except (DocumentationMatrixError, ImmutableEvidenceIOError) as error:
        parser.exit(2, f"documentation validation failed: {error}\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
