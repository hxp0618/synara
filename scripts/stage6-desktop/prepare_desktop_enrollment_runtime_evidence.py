#!/usr/bin/env python3
"""Prepare immutable Desktop Enrollment runtime evidence from a native harness capture."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import pathlib
import secrets
import sys
from typing import Any

from validate_desktop_native_acceptance import (
    COMMIT_RE,
    ENROLLMENT_FIELDS,
    ENROLLMENT_RUNTIME_EVIDENCE_SCHEMA,
    EXECUTION_MODES,
    TARGETS,
    DesktopAcceptanceError,
    parse_json_bytes,
    parse_https_base_url,
    parse_utc,
    require_exact_fields,
    require_identifier,
    scan_for_secret_material,
    validate_enrollment_runtime_evidence,
)

from stage6_common.immutable_evidence_io import (
    ImmutableEvidenceIOError,
    read_stable_regular_file,
)


CAPTURE_SCHEMA = "synara.stage6-desktop-enrollment-runtime-capture.v1"
ASSESSMENT = "capture-validated-not-desktop-ga-passed"
MAX_CAPTURE_BYTES = 1024 * 1024


class DesktopEnrollmentEvidencePreparationError(Exception):
    pass


def read_capture(path: pathlib.Path) -> dict[str, Any]:
    try:
        encoded = read_stable_regular_file(
            path, label="capture", maximum_bytes=MAX_CAPTURE_BYTES
        )
        scan_for_secret_material(encoded, "capture")
        value = parse_json_bytes(encoded, "capture")
    except (ImmutableEvidenceIOError, DesktopAcceptanceError) as error:
        raise DesktopEnrollmentEvidencePreparationError(
            f"capture must be safe stable UTF-8 JSON: {error}"
        ) from error
    return require_exact_fields(
        value,
        {
            "schemaVersion",
            "candidate",
            "target",
            "exerciseWindow",
            "startedAt",
            "completedAt",
            "modeTransitions",
            "requestCounts",
            "credentialContinuity",
            "results",
        },
        "capture",
    )


def normalize_capture(capture: dict[str, Any]) -> tuple[dict[str, Any], dict[str, Any]]:
    if capture["schemaVersion"] != CAPTURE_SCHEMA:
        raise DesktopEnrollmentEvidencePreparationError(
            "capture schemaVersion is unsupported"
        )
    candidate = require_exact_fields(
        capture["candidate"],
        {"candidateId", "sourceCommit", "environmentId", "controlPlaneBaseUrl"},
        "capture.candidate",
    )
    normalized_candidate = {
        "candidateId": require_identifier(
            candidate["candidateId"], "capture.candidate.candidateId"
        ),
        "sourceCommit": candidate["sourceCommit"],
        "environmentId": require_identifier(
            candidate["environmentId"], "capture.candidate.environmentId"
        ),
        "controlPlaneBaseUrl": parse_https_base_url(
            candidate["controlPlaneBaseUrl"], "capture.candidate.controlPlaneBaseUrl"
        ),
    }
    if (
        not isinstance(normalized_candidate["sourceCommit"], str)
        or COMMIT_RE.fullmatch(normalized_candidate["sourceCommit"]) is None
    ):
        raise DesktopEnrollmentEvidencePreparationError(
            "capture.candidate.sourceCommit must be a full lowercase Git SHA"
        )

    target = require_exact_fields(
        capture["target"],
        {"id", "runnerReference", "hostArchitecture", "executionMode"},
        "capture.target",
    )
    target_id = target["id"]
    if target_id not in TARGETS:
        raise DesktopEnrollmentEvidencePreparationError(
            "capture.target.id is not a supported native target"
        )
    host_architecture = target["hostArchitecture"]
    execution_mode = target["executionMode"]
    if host_architecture not in {"arm64", "x64"} or execution_mode not in EXECUTION_MODES:
        raise DesktopEnrollmentEvidencePreparationError(
            "capture target architecture or execution mode is unsupported"
        )
    normalized_target = {
        "id": target_id,
        "runnerReference": require_identifier(
            target["runnerReference"], "capture.target.runnerReference"
        ),
        "hostArchitecture": host_architecture,
        "executionMode": execution_mode,
    }

    window = require_exact_fields(
        capture["exerciseWindow"], {"startedAt", "completedAt"}, "capture.exerciseWindow"
    )
    exercise_started_at = parse_utc(
        window["startedAt"], "capture.exerciseWindow.startedAt"
    )
    exercise_completed_at = parse_utc(
        window["completedAt"], "capture.exerciseWindow.completedAt"
    )
    if exercise_completed_at <= exercise_started_at:
        raise DesktopEnrollmentEvidencePreparationError(
            "capture.exerciseWindow must be ordered"
        )

    results = require_exact_fields(capture["results"], ENROLLMENT_FIELDS, "capture.results")
    evidence = {
        "schemaVersion": ENROLLMENT_RUNTIME_EVIDENCE_SCHEMA,
        "candidate": normalized_candidate,
        "target": normalized_target,
        "startedAt": capture["startedAt"],
        "completedAt": capture["completedAt"],
        "modeTransitions": capture["modeTransitions"],
        "requestCounts": capture["requestCounts"],
        "credentialContinuity": capture["credentialContinuity"],
        "results": results,
    }
    context = {
        "exerciseStartedAt": exercise_started_at,
        "exerciseCompletedAt": exercise_completed_at,
        "candidate": normalized_candidate,
        "target": normalized_target,
        "results": results,
    }
    return evidence, context


def temporary_path(output: pathlib.Path, label: str) -> pathlib.Path:
    return output.parent / f".{output.name}.{label}.{os.getpid()}.{secrets.token_hex(6)}.tmp"


def write_exclusive(path: pathlib.Path, encoded: bytes) -> None:
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        offset = 0
        while offset < len(encoded):
            offset += os.write(descriptor, encoded[offset:])
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def prepare(capture_path: pathlib.Path, output: pathlib.Path) -> dict[str, str]:
    capture = read_capture(capture_path)
    evidence, context = normalize_capture(capture)
    output = output.absolute()
    sidecar = output.with_suffix(output.suffix + ".sha256")
    output.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    if output.parent.is_symlink() or not output.parent.is_dir():
        raise DesktopEnrollmentEvidencePreparationError(
            "output parent must be a regular non-symlink directory"
        )
    if output.exists() or output.is_symlink() or sidecar.exists() or sidecar.is_symlink():
        raise DesktopEnrollmentEvidencePreparationError(
            "output and SHA-256 sidecar must not already exist"
        )

    encoded = (json.dumps(evidence, indent=2, sort_keys=True) + "\n").encode("utf-8")
    digest = hashlib.sha256(encoded).hexdigest()
    sidecar_encoded = f"{digest}  {output.name}\n".encode("utf-8")
    evidence_temp = temporary_path(output, "evidence")
    sidecar_temp = temporary_path(output, "sidecar")
    published_output = False
    try:
        write_exclusive(evidence_temp, encoded)
        validate_enrollment_runtime_evidence(
            encoded,
            target_id=context["target"]["id"],
            runner_reference=context["target"]["runnerReference"],
            host_architecture=context["target"]["hostArchitecture"],
            execution_mode=context["target"]["executionMode"],
            candidate=context["candidate"],
            exercise_started_at=context["exerciseStartedAt"],
            exercise_completed_at=context["exerciseCompletedAt"],
            manifest_results=context["results"],
        )
        write_exclusive(sidecar_temp, sidecar_encoded)
        os.link(evidence_temp, output, follow_symlinks=False)
        published_output = True
        try:
            os.link(sidecar_temp, sidecar, follow_symlinks=False)
        except Exception:
            output.unlink(missing_ok=True)
            published_output = False
            raise
        os.chmod(output, 0o600)
        os.chmod(sidecar, 0o600)
    except (OSError, DesktopAcceptanceError) as error:
        sidecar.unlink(missing_ok=True)
        if published_output:
            output.unlink(missing_ok=True)
            published_output = False
        raise DesktopEnrollmentEvidencePreparationError(str(error)) from error
    finally:
        evidence_temp.unlink(missing_ok=True)
        sidecar_temp.unlink(missing_ok=True)
        if published_output and not sidecar.exists():
            output.unlink(missing_ok=True)
    return {
        "assessment": ASSESSMENT,
        "output": str(output),
        "sha256": "sha256:" + digest,
    }


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--capture", required=True)
    parser.add_argument("--output", required=True)
    return parser


def main() -> int:
    args = build_parser().parse_args()
    try:
        result = prepare(pathlib.Path(args.capture).absolute(), pathlib.Path(args.output))
    except (DesktopEnrollmentEvidencePreparationError, DesktopAcceptanceError) as error:
        print(f"Desktop Enrollment evidence preparation failed: {error}", file=sys.stderr)
        return 2
    print(json.dumps(result, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
