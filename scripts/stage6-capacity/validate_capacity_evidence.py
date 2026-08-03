#!/usr/bin/env python3
"""Validate Stage 6 capacity/soak evidence without declaring the release gate passed."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import math
import pathlib
import re
import sys
import uuid
from typing import Any

SCRIPTS_ROOT = pathlib.Path(__file__).resolve().parents[1]
if str(SCRIPTS_ROOT) not in sys.path:
    sys.path.insert(0, str(SCRIPTS_ROOT))

from stage6_common.immutable_evidence_io import (  # noqa: E402
    ImmutableEvidenceIOError,
    publish_immutable_with_sha256,
    read_stable_regular_file,
)


COMMIT_RE = re.compile(r"^[0-9a-f]{40}$")
SHA256_RE = re.compile(r"^sha256:([0-9a-f]{64})$")
IDENTIFIER_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:/-]{1,159}$")

MINIMUM_DURATION_SECONDS = {
    "production": 72 * 60 * 60,
    "production-like": 24 * 60 * 60,
    "staging": 8 * 60 * 60,
    "fixture": 5 * 60,
}
REQUIRED_PHASES = {
    "steady-peak",
    "burst",
    "tenant-hotspot",
    "rolling-disruption",
    "cooldown",
}
CAPACITY_DIMENSIONS = (
    "peakConcurrentSessions",
    "peakExecutionStartsPerMinute",
    "peakEventAppendsPerSecond",
    "peakSSEConnections",
)
EVIDENCE_FIELDS = (
    "workloadGeneratorEvidence",
    "prometheusRangeEvidence",
    "externalCanaryEvidence",
    "databaseEvidence",
    "kubernetesEvidence",
    "applicationLogEvidence",
    "resultSummaryEvidence",
)
MANIFEST_SCHEMA = "synara.capacity-soak-evidence.v1"
RECEIPT_SCHEMA = "synara.capacity-soak-evidence-receipt.v1"
ASSESSMENT = "evidence-validated-not-capacity-passed"
MAX_MANIFEST_BYTES = 2 * 1024 * 1024
MAX_EVIDENCE_FILE_BYTES = 32 * 1024 * 1024
MAX_TOTAL_EVIDENCE_BYTES = 192 * 1024 * 1024
PROHIBITED_SECRET_PATTERNS = (
    ("private key", re.compile(rb"-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----")),
    ("AWS access key", re.compile(rb"\bAKIA[0-9A-Z]{16}\b")),
    ("bearer credential", re.compile(rb"(?i)authorization\s*[:=]\s*bearer\s+[^\s,;]+")),
    (
        "credential-bearing URL",
        re.compile(rb"(?i)\b(?:https?|postgres(?:ql)?|mysql)://[^\s/:@]+:[^\s/@]+@"),
    ),
)
MEASUREMENT_FIELDS = {
    "availabilityGoodRatio",
    "apiLatencyGoodRatio",
    "executionStartGoodRatio",
    "eventDelayGoodRatio",
    "httpRequestCount",
    "executionStartCount",
    "eventAppendCount",
    "maximumDatabaseConnectionUtilizationRatio",
    "maximumControlPlaneCPUUtilizationRatio",
    "maximumControlPlaneMemoryUtilizationRatio",
    "maximumOutboxOldestSeconds",
    "maximumQueueOldestSeconds",
    "maximumWarmDeficitUnits",
    "deadLetterCount",
    "oomKillCount",
    "unexpectedRestartCount",
    "minimumTenantSuccessRatio",
    "maximumTenantSuccessRatio",
    "failedAssertionCount",
}


class CapacityEvidenceError(Exception):
    pass


def reject_duplicate_json_fields(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise CapacityEvidenceError(f"manifest contains duplicate JSON field {key!r}")
        result[key] = value
    return result


def scan_for_secret_material(content: bytes, label: str) -> None:
    for secret_label, pattern in PROHIBITED_SECRET_PATTERNS:
        if pattern.search(content):
            raise CapacityEvidenceError(f"{label} contains prohibited {secret_label} material")


def require_mapping(value: Any, label: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise CapacityEvidenceError(f"{label} must be an object")
    return value


def require_identifier(value: Any, label: str) -> str:
    if not isinstance(value, str) or IDENTIFIER_RE.fullmatch(value.strip()) is None:
        raise CapacityEvidenceError(f"{label} must be a bounded identifier")
    return value.strip()


def require_number(value: Any, label: str, minimum: float = 0.0) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value):
        raise CapacityEvidenceError(f"{label} must be a finite number")
    result = float(value)
    if result < minimum:
        raise CapacityEvidenceError(f"{label} must be at least {minimum:g}")
    return result


def require_integer(value: Any, label: str, minimum: int = 0) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value < minimum:
        raise CapacityEvidenceError(f"{label} must be an integer >= {minimum}")
    return value


def require_ratio(value: Any, label: str) -> float:
    result = require_number(value, label)
    if result > 1:
        raise CapacityEvidenceError(f"{label} must be between 0 and 1")
    return result


def parse_utc(value: Any, label: str) -> dt.datetime:
    if not isinstance(value, str) or not value.endswith("Z"):
        raise CapacityEvidenceError(f"{label} must be an RFC3339 UTC timestamp ending in Z")
    try:
        parsed = dt.datetime.fromisoformat(value[:-1] + "+00:00")
    except ValueError as error:
        raise CapacityEvidenceError(f"{label} must be a valid RFC3339 UTC timestamp") from error
    if parsed.tzinfo != dt.timezone.utc:
        raise CapacityEvidenceError(f"{label} must use UTC")
    return parsed


def resolve_evidence_file(root: pathlib.Path, raw_path: Any, label: str) -> pathlib.Path:
    if not isinstance(raw_path, str) or not raw_path.strip():
        raise CapacityEvidenceError(f"{label}.path must be a non-empty relative path")
    relative = pathlib.Path(raw_path)
    if relative.is_absolute() or any(part in {".", ".."} for part in relative.parts):
        raise CapacityEvidenceError(f"{label}.path must be traversal-free and relative")
    candidate = root / relative
    current = candidate
    while current != root:
        if current.is_symlink():
            raise CapacityEvidenceError(f"{label}.path must not traverse a symlink")
        current = current.parent
    try:
        resolved = candidate.resolve(strict=True)
        resolved.relative_to(root)
    except (OSError, ValueError) as error:
        raise CapacityEvidenceError(f"{label}.path must resolve inside the evidence root") from error
    if not resolved.is_file():
        raise CapacityEvidenceError(f"{label}.path must reference a regular file")
    return resolved


def validate_reference(
    value: Any,
    root: pathlib.Path,
    label: str,
    used_paths: set[pathlib.Path],
    total_bytes: list[int],
) -> dict[str, str]:
    reference = require_mapping(value, label)
    if set(reference) != {"path", "sha256"}:
        raise CapacityEvidenceError(f"{label} must contain exactly path and sha256")
    path = resolve_evidence_file(root, reference["path"], label)
    if path in used_paths:
        raise CapacityEvidenceError(f"{label}.path duplicates another evidence file")
    used_paths.add(path)
    try:
        content = read_stable_regular_file(
            path,
            label=label,
            maximum_bytes=MAX_EVIDENCE_FILE_BYTES,
        )
    except ImmutableEvidenceIOError as error:
        raise CapacityEvidenceError(str(error)) from error
    total_bytes[0] += len(content)
    if total_bytes[0] > MAX_TOTAL_EVIDENCE_BYTES:
        raise CapacityEvidenceError("total capacity evidence exceeds the bounded size limit")
    scan_for_secret_material(content, label)
    digest = reference["sha256"]
    if not isinstance(digest, str) or SHA256_RE.fullmatch(digest) is None:
        raise CapacityEvidenceError(f"{label}.sha256 must be sha256:<64 lowercase hex>")
    actual = "sha256:" + hashlib.sha256(content).hexdigest()
    if actual != digest:
        raise CapacityEvidenceError(f"{label}.sha256 does not match the evidence file")
    return {"path": path.relative_to(root).as_posix(), "sha256": actual}


def validate_capacity_shape(value: Any, label: str, include_headroom: bool) -> dict[str, float]:
    shape = require_mapping(value, label)
    expected = set(CAPACITY_DIMENSIONS)
    if include_headroom:
        expected.add("requiredHeadroomPercent")
    if set(shape) != expected:
        raise CapacityEvidenceError(f"{label} fields do not match the v1 schema")
    result = {
        dimension: require_number(shape[dimension], f"{label}.{dimension}", 1)
        for dimension in CAPACITY_DIMENSIONS
    }
    if include_headroom:
        headroom = require_number(shape["requiredHeadroomPercent"], f"{label}.requiredHeadroomPercent", 20)
        if headroom > 200:
            raise CapacityEvidenceError(f"{label}.requiredHeadroomPercent must be <= 200")
        result["requiredHeadroomPercent"] = headroom
    return result


def validate_phases(
    values: Any,
    started: dt.datetime,
    completed: dt.datetime,
) -> list[dict[str, Any]]:
    if not isinstance(values, list) or len(values) != len(REQUIRED_PHASES):
        raise CapacityEvidenceError("phases must contain every required phase exactly once")
    result: list[dict[str, Any]] = []
    names: set[str] = set()
    previous_end = started
    for index, raw in enumerate(values):
        phase = require_mapping(raw, f"phases[{index}]")
        if set(phase) != {"name", "startedAt", "completedAt", "loadMultiplier", "executed"}:
            raise CapacityEvidenceError(f"phases[{index}] fields do not match the v1 schema")
        name = require_identifier(phase["name"], f"phases[{index}].name")
        if name not in REQUIRED_PHASES or name in names:
            raise CapacityEvidenceError("phases must contain every required phase exactly once")
        phase_started = parse_utc(phase["startedAt"], f"{name}.startedAt")
        phase_completed = parse_utc(phase["completedAt"], f"{name}.completedAt")
        if not (started <= phase_started < phase_completed <= completed) or phase_started < previous_end:
            raise CapacityEvidenceError(f"{name} timestamps overlap, are unordered, or fall outside the run")
        if phase["executed"] is not True:
            raise CapacityEvidenceError(f"{name}.executed must be true")
        multiplier = require_number(phase["loadMultiplier"], f"{name}.loadMultiplier", 0.1)
        result.append(
            {
                "name": name,
                "startedAt": phase_started.isoformat().replace("+00:00", "Z"),
                "completedAt": phase_completed.isoformat().replace("+00:00", "Z"),
                "durationSeconds": (phase_completed - phase_started).total_seconds(),
                "loadMultiplier": multiplier,
            }
        )
        names.add(name)
        previous_end = phase_completed
    if names != REQUIRED_PHASES:
        raise CapacityEvidenceError("phases must contain every required phase exactly once")
    return result


def validate_measurements(value: Any) -> tuple[dict[str, Any], bool]:
    measurements = require_mapping(value, "measurements")
    if set(measurements) != MEASUREMENT_FIELDS:
        raise CapacityEvidenceError("measurements fields do not match the v1 schema")
    result: dict[str, Any] = {
        "availabilityGoodRatio": require_ratio(measurements["availabilityGoodRatio"], "availabilityGoodRatio"),
        "apiLatencyGoodRatio": require_ratio(measurements["apiLatencyGoodRatio"], "apiLatencyGoodRatio"),
        "executionStartGoodRatio": require_ratio(measurements["executionStartGoodRatio"], "executionStartGoodRatio"),
        "eventDelayGoodRatio": require_ratio(measurements["eventDelayGoodRatio"], "eventDelayGoodRatio"),
        "httpRequestCount": require_integer(measurements["httpRequestCount"], "httpRequestCount"),
        "executionStartCount": require_integer(measurements["executionStartCount"], "executionStartCount"),
        "eventAppendCount": require_integer(measurements["eventAppendCount"], "eventAppendCount"),
        "maximumDatabaseConnectionUtilizationRatio": require_ratio(
            measurements["maximumDatabaseConnectionUtilizationRatio"], "maximumDatabaseConnectionUtilizationRatio"
        ),
        "maximumControlPlaneCPUUtilizationRatio": require_ratio(
            measurements["maximumControlPlaneCPUUtilizationRatio"], "maximumControlPlaneCPUUtilizationRatio"
        ),
        "maximumControlPlaneMemoryUtilizationRatio": require_ratio(
            measurements["maximumControlPlaneMemoryUtilizationRatio"], "maximumControlPlaneMemoryUtilizationRatio"
        ),
        "maximumOutboxOldestSeconds": require_number(measurements["maximumOutboxOldestSeconds"], "maximumOutboxOldestSeconds"),
        "maximumQueueOldestSeconds": require_number(measurements["maximumQueueOldestSeconds"], "maximumQueueOldestSeconds"),
        "maximumWarmDeficitUnits": require_integer(measurements["maximumWarmDeficitUnits"], "maximumWarmDeficitUnits"),
        "deadLetterCount": require_integer(measurements["deadLetterCount"], "deadLetterCount"),
        "oomKillCount": require_integer(measurements["oomKillCount"], "oomKillCount"),
        "unexpectedRestartCount": require_integer(measurements["unexpectedRestartCount"], "unexpectedRestartCount"),
        "minimumTenantSuccessRatio": require_ratio(measurements["minimumTenantSuccessRatio"], "minimumTenantSuccessRatio"),
        "maximumTenantSuccessRatio": require_ratio(measurements["maximumTenantSuccessRatio"], "maximumTenantSuccessRatio"),
        "failedAssertionCount": require_integer(measurements["failedAssertionCount"], "failedAssertionCount"),
    }
    fairness = result["maximumTenantSuccessRatio"] > 0 and (
        result["minimumTenantSuccessRatio"] / result["maximumTenantSuccessRatio"] >= 0.8
    )
    within = (
        result["availabilityGoodRatio"] >= 0.999
        and result["apiLatencyGoodRatio"] >= 0.99
        and result["executionStartGoodRatio"] >= 0.99
        and result["eventDelayGoodRatio"] >= 0.999
        and result["httpRequestCount"] >= 10_000
        and result["executionStartCount"] >= 100
        and result["eventAppendCount"] >= 1_000
        and result["maximumDatabaseConnectionUtilizationRatio"] <= 0.8
        and result["maximumControlPlaneCPUUtilizationRatio"] <= 0.8
        and result["maximumControlPlaneMemoryUtilizationRatio"] <= 0.8
        and result["maximumOutboxOldestSeconds"] <= 300
        and result["maximumQueueOldestSeconds"] <= 60
        and result["maximumWarmDeficitUnits"] == 0
        and result["deadLetterCount"] == 0
        and result["oomKillCount"] == 0
        and result["unexpectedRestartCount"] == 0
        and result["failedAssertionCount"] == 0
        and fairness
    )
    result["tenantFairnessWithinObjective"] = fairness
    return result, within


def normalized_manifest_reference(value: str) -> str:
    path = pathlib.PurePosixPath(value)
    if (
        path.is_absolute()
        or not path.parts
        or any(part in {"", ".", ".."} for part in path.parts)
        or path.as_posix() != value
    ):
        raise CapacityEvidenceError("manifest receipt path must be normalized and relative")
    return value


def validate(
    manifest_path: pathlib.Path,
    evidence_root: pathlib.Path,
    validated_at: str | None,
    manifest_reference_path: str | None = None,
) -> dict[str, Any]:
    root = evidence_root.resolve()
    if evidence_root.is_symlink() or not root.is_dir():
        raise CapacityEvidenceError("evidence root must be a regular non-symlink directory")
    try:
        manifest_bytes = read_stable_regular_file(
            manifest_path,
            label="capacity manifest",
            maximum_bytes=MAX_MANIFEST_BYTES,
        )
        scan_for_secret_material(manifest_bytes, "capacity manifest")
        raw = json.loads(manifest_bytes, object_pairs_hook=reject_duplicate_json_fields)
    except ImmutableEvidenceIOError as error:
        raise CapacityEvidenceError(str(error)) from error
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise CapacityEvidenceError("manifest must be valid JSON") from error
    manifest = require_mapping(raw, "manifest")
    expected_fields = {
        "schemaVersion",
        "runId",
        "releaseCommit",
        "environmentClass",
        "environmentId",
        "startedAt",
        "completedAt",
        "sampleIntervalSeconds",
        "observedExternalProbeSamples",
        "forecast",
        "load",
        "phases",
        "measurements",
        "exercises",
        "evidence",
    }
    if set(manifest) != expected_fields:
        raise CapacityEvidenceError("manifest fields do not match the v1 schema")
    if manifest["schemaVersion"] != MANIFEST_SCHEMA:
        raise CapacityEvidenceError(f"schemaVersion is not {MANIFEST_SCHEMA}")
    try:
        run_id = str(uuid.UUID(str(manifest["runId"])))
    except (ValueError, AttributeError) as error:
        raise CapacityEvidenceError("runId must be a UUID") from error
    release_commit = manifest["releaseCommit"]
    if not isinstance(release_commit, str) or COMMIT_RE.fullmatch(release_commit) is None:
        raise CapacityEvidenceError("releaseCommit must be a full 40-character lowercase Git SHA")
    environment_class = manifest["environmentClass"]
    if environment_class not in MINIMUM_DURATION_SECONDS:
        raise CapacityEvidenceError("environmentClass is not allowed")
    environment_id = require_identifier(manifest["environmentId"], "environmentId")
    started = parse_utc(manifest["startedAt"], "startedAt")
    completed = parse_utc(manifest["completedAt"], "completedAt")
    duration_seconds = (completed - started).total_seconds()
    minimum_duration = MINIMUM_DURATION_SECONDS[environment_class]
    if duration_seconds < minimum_duration:
        raise CapacityEvidenceError(f"run duration is below the {environment_class} minimum")
    sample_interval = require_integer(manifest["sampleIntervalSeconds"], "sampleIntervalSeconds", 1)
    if sample_interval > 60:
        raise CapacityEvidenceError("sampleIntervalSeconds must be <= 60")

    exercises = require_mapping(manifest["exercises"], "exercises")
    if set(exercises) != {
        "externalProbeRegions",
        "controlPlaneRollingRestart",
        "workerChurn",
        "databaseConnectionPressure",
        "outboxBackpressure",
        "tenantHotspot",
    }:
        raise CapacityEvidenceError("exercises fields do not match the v1 schema")
    probe_regions = require_integer(exercises["externalProbeRegions"], "externalProbeRegions", 3)
    for name in (
        "controlPlaneRollingRestart",
        "workerChurn",
        "databaseConnectionPressure",
        "outboxBackpressure",
        "tenantHotspot",
    ):
        if exercises[name] is not True:
            raise CapacityEvidenceError(f"exercises.{name} must be true")
    observed_probe_samples = require_integer(
        manifest["observedExternalProbeSamples"], "observedExternalProbeSamples", 1
    )
    expected_probe_samples = math.floor(duration_seconds / sample_interval) * probe_regions
    probe_coverage = observed_probe_samples / max(expected_probe_samples, 1)
    if probe_coverage < 0.95:
        raise CapacityEvidenceError("external probe sample coverage is below 95 percent")

    forecast = validate_capacity_shape(manifest["forecast"], "forecast", True)
    load = validate_capacity_shape(manifest["load"], "load", False)
    multiplier = 1 + forecast["requiredHeadroomPercent"] / 100
    headroom_covered = all(load[name] >= forecast[name] * multiplier for name in CAPACITY_DIMENSIONS)
    if not headroom_covered:
        raise CapacityEvidenceError("load does not cover the forecast plus required headroom")
    phases = validate_phases(manifest["phases"], started, completed)
    measurements, measurements_within = validate_measurements(manifest["measurements"])

    evidence = require_mapping(manifest["evidence"], "evidence")
    if set(evidence) != set(EVIDENCE_FIELDS):
        raise CapacityEvidenceError("evidence fields do not match the v1 schema")
    used_paths: set[pathlib.Path] = set()
    total_bytes = [0]
    validated_evidence = {
        field: validate_reference(
            evidence[field], root, f"evidence.{field}", used_paths, total_bytes
        )
        for field in EVIDENCE_FIELDS
    }
    validation_time = validated_at or dt.datetime.now(dt.timezone.utc).isoformat().replace("+00:00", "Z")
    validated = parse_utc(validation_time, "validatedAt")
    if validated < completed or validated > completed + dt.timedelta(days=30):
        raise CapacityEvidenceError("validatedAt must be after completion and within 30 days")
    release_eligible_environment = environment_class in {"production", "production-like"}
    eligible_for_human_gate_review = (
        release_eligible_environment and headroom_covered and measurements_within
    )
    return {
        "schemaVersion": RECEIPT_SCHEMA,
        "runId": run_id,
        "releaseCommit": release_commit,
        "environmentClass": environment_class,
        "environmentId": environment_id,
        "startedAt": started.isoformat().replace("+00:00", "Z"),
        "completedAt": completed.isoformat().replace("+00:00", "Z"),
        "durationSeconds": duration_seconds,
        "minimumDurationSeconds": minimum_duration,
        "sampleIntervalSeconds": sample_interval,
        "externalProbeRegions": probe_regions,
        "externalProbeCoverageRatio": probe_coverage,
        "forecast": forecast,
        "load": load,
        "forecastHeadroomCovered": headroom_covered,
        "phases": phases,
        "measurements": measurements,
        "declaredMeasurementsWithinObjectives": measurements_within,
        "exercises": exercises,
        "manifest": {
            "path": normalized_manifest_reference(
                manifest_reference_path or manifest_path.name
            ),
            "sha256": "sha256:" + hashlib.sha256(manifest_bytes).hexdigest(),
        },
        "evidence": validated_evidence,
        "releaseEligibleEnvironment": release_eligible_environment,
        "eligibleForHumanGateReview": eligible_for_human_gate_review,
        "validatedAt": validation_time,
        "assessment": ASSESSMENT,
    }


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manifest", required=True)
    parser.add_argument("--evidence-root", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--validated-at", help=argparse.SUPPRESS)
    return parser


def main() -> int:
    parser = build_parser()
    args = parser.parse_args()
    try:
        receipt = validate(
            pathlib.Path(args.manifest).resolve(),
            pathlib.Path(args.evidence_root),
            args.validated_at,
        )
        output = pathlib.Path(args.output).absolute()
        encoded = (json.dumps(receipt, indent=2, sort_keys=True) + "\n").encode("utf-8")
        publish_immutable_with_sha256(output, encoded)
    except (CapacityEvidenceError, ImmutableEvidenceIOError) as error:
        parser.exit(2, f"capacity evidence validation failed: {error}\n")
    print(f"wrote {output} and {output.name}.sha256")
    return 0


if __name__ == "__main__":
    sys.exit(main())
