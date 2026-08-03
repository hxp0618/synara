#!/usr/bin/env python3
"""Prepare and validate one immutable internal-self-hosted Stage 6 v5 candidate evidence bundle."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import pathlib
import tempfile
import sys
from typing import Any

from validate_candidate_evidence_bundle import (
    CandidateEvidenceError,
    MAX_JSON_BYTES,
    RECEIPT_SPECS_V4,
    SCHEMA_VERSION_V5,
    sha256_file,
    validate,
)


class CandidatePreparationError(Exception):
    pass


def relative_path(value: str, label: str) -> pathlib.PurePosixPath:
    if "\\" in value:
        raise CandidatePreparationError(f"{label} must use POSIX path separators")
    path = pathlib.PurePosixPath(value)
    if path.is_absolute() or not path.parts or any(part in {"", ".", ".."} for part in path.parts):
        raise CandidatePreparationError(f"{label} must be traversal-free and relative")
    return path


def resolve_input(
    root: pathlib.Path,
    value: str,
    label: str,
    used_paths: set[pathlib.Path],
) -> tuple[pathlib.PurePosixPath, pathlib.Path]:
    relative = relative_path(value, label)
    candidate = root.joinpath(*relative.parts)
    current = candidate
    while current != root:
        if current.is_symlink():
            raise CandidatePreparationError(f"{label} must not traverse a symbolic link")
        current = current.parent
    try:
        resolved = candidate.resolve(strict=True)
        resolved.relative_to(root)
    except (OSError, ValueError) as error:
        raise CandidatePreparationError(f"{label} must resolve inside the evidence root") from error
    if not resolved.is_file() or resolved.stat().st_size > MAX_JSON_BYTES:
        raise CandidatePreparationError(f"{label} must reference a bounded regular file")
    if resolved in used_paths:
        raise CandidatePreparationError(f"{label} duplicates another bundle input")
    used_paths.add(resolved)
    return relative, resolved


def resolve_output(root: pathlib.Path, value: str, label: str) -> pathlib.Path:
    relative = relative_path(value, label)
    candidate = root.joinpath(*relative.parts)
    parent = candidate.parent
    current = parent
    while current != root:
        if current.is_symlink():
            raise CandidatePreparationError(f"{label} must not traverse a symbolic link")
        current = current.parent
    if not parent.is_dir():
        raise CandidatePreparationError(f"{label} parent must be an existing non-symlink directory")
    try:
        parent.resolve(strict=True).relative_to(root)
    except (OSError, ValueError) as error:
        raise CandidatePreparationError(f"{label} must remain inside the evidence root") from error
    if candidate.exists() or candidate.is_symlink():
        raise CandidatePreparationError(f"{label} already exists; candidate evidence is immutable")
    return candidate


def encoded_json(payload: dict[str, Any]) -> bytes:
    return (json.dumps(payload, indent=2, sort_keys=True) + "\n").encode("utf-8")


def sidecar_bytes(data: bytes, filename: str) -> bytes:
    return f"{hashlib.sha256(data).hexdigest()}  {filename}\n".encode("utf-8")


def publish_exclusive(files: list[tuple[pathlib.Path, bytes]]) -> None:
    paths = [path for path, _ in files]
    if len(set(paths)) != len(paths):
        raise CandidatePreparationError("candidate output paths must be distinct")
    for path in paths:
        if path.exists() or path.is_symlink():
            raise CandidatePreparationError(f"output already exists: {path.name}")

    created: list[pathlib.Path] = []
    temporary_paths: list[pathlib.Path] = []
    try:
        for destination, data in files:
            descriptor, raw_temporary = tempfile.mkstemp(
                prefix=f".{destination.name}.", suffix=".tmp", dir=destination.parent
            )
            temporary = pathlib.Path(raw_temporary)
            temporary_paths.append(temporary)
            try:
                try:
                    os.fchmod(descriptor, 0o600)
                    with os.fdopen(descriptor, "wb") as output:
                        descriptor = -1
                        output.write(data)
                        output.flush()
                        os.fsync(output.fileno())
                finally:
                    if descriptor >= 0:
                        os.close(descriptor)
                os.link(temporary, destination)
                created.append(destination)
            finally:
                temporary.unlink(missing_ok=True)
        for directory in {path.parent for path in created}:
            directory_descriptor = os.open(directory, os.O_RDONLY)
            try:
                os.fsync(directory_descriptor)
            finally:
                os.close(directory_descriptor)
    except BaseException as error:
        for path in reversed(created):
            path.unlink(missing_ok=True)
        if isinstance(error, OSError):
            raise CandidatePreparationError(
                "could not publish the complete immutable candidate bundle"
            ) from error
        raise
    finally:
        for path in temporary_paths:
            path.unlink(missing_ok=True)


def prepare(args: argparse.Namespace) -> tuple[pathlib.Path, pathlib.Path, dict[str, Any]]:
    root_input = pathlib.Path(args.evidence_root)
    if root_input.is_symlink():
        raise CandidatePreparationError("evidence root must be a non-symlink directory")
    try:
        root = root_input.resolve(strict=True)
    except OSError as error:
        raise CandidatePreparationError("evidence root does not exist") from error
    if not root.is_dir():
        raise CandidatePreparationError("evidence root must be a directory")

    used_inputs: set[pathlib.Path] = set()
    release_relative, release_path = resolve_input(
        root, args.release_evidence, "release evidence", used_inputs
    )
    compatibility_relative, compatibility_path = resolve_input(
        root, args.compatibility_matrix, "compatibility matrix", used_inputs
    )
    receipt_references: dict[str, dict[str, str]] = {}
    for name in sorted(RECEIPT_SPECS_V4):
        raw_path = getattr(args, f"{name}_receipt")
        relative, path = resolve_input(root, raw_path, f"{name} receipt", used_inputs)
        receipt_references[name] = {"path": relative.as_posix(), "sha256": sha256_file(path)}

    manifest_output = resolve_output(root, args.manifest_output, "manifest output")
    receipt_output = resolve_output(root, args.receipt_output, "receipt output")
    output_paths = {
        manifest_output,
        manifest_output.with_suffix(manifest_output.suffix + ".sha256"),
        receipt_output,
        receipt_output.with_suffix(receipt_output.suffix + ".sha256"),
    }
    if len(output_paths) != 4:
        raise CandidatePreparationError("manifest and receipt outputs must be distinct")
    if any(path.resolve() in used_inputs for path in output_paths):
        raise CandidatePreparationError("candidate outputs must not replace bundle inputs")
    for path in output_paths:
        if path.exists() or path.is_symlink():
            raise CandidatePreparationError(f"output already exists: {path.name}")

    manifest = {
        "schemaVersion": SCHEMA_VERSION_V5,
        "releaseEvidence": {
            "path": release_relative.as_posix(),
            "sha256": sha256_file(release_path),
        },
        "compatibilityMatrix": {
            "path": compatibility_relative.as_posix(),
            "sha256": sha256_file(compatibility_path),
        },
        "receipts": receipt_references,
    }
    manifest_data = encoded_json(manifest)
    with tempfile.TemporaryDirectory(prefix="synara-stage6-candidate-") as temporary_directory:
        temporary_manifest = pathlib.Path(temporary_directory) / manifest_output.name
        temporary_manifest.write_bytes(manifest_data)
        try:
            receipt = validate(temporary_manifest, root, args.validated_at)
        except CandidateEvidenceError as error:
            raise CandidatePreparationError(str(error)) from error
    receipt_data = encoded_json(receipt)

    publish_exclusive(
        [
            (manifest_output, manifest_data),
            (
                manifest_output.with_suffix(manifest_output.suffix + ".sha256"),
                sidecar_bytes(manifest_data, manifest_output.name),
            ),
            (receipt_output, receipt_data),
            (
                receipt_output.with_suffix(receipt_output.suffix + ".sha256"),
                sidecar_bytes(receipt_data, receipt_output.name),
            ),
        ]
    )
    return manifest_output, receipt_output, receipt


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--evidence-root", required=True)
    parser.add_argument("--release-evidence", required=True)
    parser.add_argument("--compatibility-matrix", required=True)
    for name in sorted(RECEIPT_SPECS_V4):
        option = "--" + "".join(f"-{character.lower()}" if character.isupper() else character for character in name)
        parser.add_argument(f"{option}-receipt", required=True, dest=f"{name}_receipt")
    parser.add_argument("--manifest-output", required=True)
    parser.add_argument("--receipt-output", required=True)
    parser.add_argument("--validated-at", help=argparse.SUPPRESS)
    return parser


def main() -> int:
    parser = build_parser()
    args = parser.parse_args()
    try:
        manifest_path, receipt_path, receipt = prepare(args)
    except CandidatePreparationError as error:
        parser.exit(2, f"candidate evidence preparation failed: {error}\n")
    print(
        f"wrote immutable {manifest_path} and {receipt_path}; "
        "eligibleForCandidateEvidenceReview="
        f"{str(receipt['eligibleForCandidateEvidenceReview']).lower()}"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
