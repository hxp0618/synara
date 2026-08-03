#!/usr/bin/env python3
"""Validate Stage 6 SLO-window evidence without declaring the production control passed."""

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
from urllib.parse import urlparse

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

MINIMUM_WINDOW_SECONDS = {
    "production": 30 * 24 * 60 * 60,
    "production-like": 30 * 24 * 60 * 60,
    "staging": 24 * 60 * 60,
    "fixture": 5 * 60,
}
OBJECTIVES = {
    "availability": {"target": 0.999, "minimumSamples": 0},
    "apiLatency": {"target": 0.99, "minimumSamples": 10_000},
    "executionStartDelay": {"target": 0.99, "minimumSamples": 100},
    "eventDelay": {"target": 0.999, "minimumSamples": 1_000},
}
ALERT_FIELDS = {
    "availabilityFastBurnCount",
    "apiLatencyFastBurnCount",
    "availabilityBudgetLowCount",
    "apiLatencyBudgetLowCount",
    "executionStartBudgetLowCount",
    "eventDelayBudgetLowCount",
    "publicProbeMissingCount",
}
COMPANION_FIELDS = {
    "executionTerminalBeforeReadyCount",
    "maximumQueueOldestSeconds",
    "maximumOutboxOldestSeconds",
    "expiredSSELeaseCount",
    "sseCatchupP95Seconds",
    "reviewed",
}
BURN_REVIEW_FIELDS = {
    "allMaterialBurnsMapped",
    "allIncidentsReviewed",
    "allMaintenanceIncluded",
    "allExceptionsDocumented",
    "openUnownedCorrectiveActionCount",
}
EVIDENCE_FIELDS = (
    "prometheusQueryEvidence",
    "externalProbeEvidence",
    "alertHistoryEvidence",
    "incidentMaintenanceEvidence",
    "deploymentIdentityEvidence",
    "resultSummaryEvidence",
)
MANIFEST_SCHEMA = "synara.slo-window-evidence.v1"
RECEIPT_SCHEMA = "synara.slo-window-evidence-receipt.v1"
ASSESSMENT = "evidence-validated-not-slo-passed"
MAX_MANIFEST_BYTES = 2 * 1024 * 1024
MAX_EVIDENCE_FILE_BYTES = 16 * 1024 * 1024
MAX_TOTAL_EVIDENCE_BYTES = 96 * 1024 * 1024
PROHIBITED_SECRET_PATTERNS = (
    ("private key", re.compile(rb"-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----")),
    ("AWS access key", re.compile(rb"\bAKIA[0-9A-Z]{16}\b")),
    ("bearer credential", re.compile(rb"(?i)authorization\s*[:=]\s*bearer\s+[^\s,;]+")),
    (
        "credential-bearing URL",
        re.compile(rb"(?i)\b(?:https?|postgres(?:ql)?|mysql)://[^\s/:@]+:[^\s/@]+@"),
    ),
)


class SLOEvidenceError(Exception):
    pass


def reject_duplicate_json_fields(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise SLOEvidenceError(f"manifest contains duplicate JSON field {key!r}")
        result[key] = value
    return result


def scan_for_secret_material(content: bytes, label: str) -> None:
    for secret_label, pattern in PROHIBITED_SECRET_PATTERNS:
        if pattern.search(content):
            raise SLOEvidenceError(f"{label} contains prohibited {secret_label} material")


def require_mapping(value: Any, label: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise SLOEvidenceError(f"{label} must be an object")
    return value


def require_identifier(value: Any, label: str) -> str:
    if not isinstance(value, str) or IDENTIFIER_RE.fullmatch(value.strip()) is None:
        raise SLOEvidenceError(f"{label} must be a bounded identifier")
    return value.strip()


def require_number(value: Any, label: str, minimum: float = 0.0) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value):
        raise SLOEvidenceError(f"{label} must be a finite number")
    result = float(value)
    if result < minimum:
        raise SLOEvidenceError(f"{label} must be at least {minimum:g}")
    return result


def require_integer(value: Any, label: str, minimum: int = 0) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value < minimum:
        raise SLOEvidenceError(f"{label} must be an integer >= {minimum}")
    return value


def require_ratio(value: Any, label: str) -> float:
    result = require_number(value, label)
    if result > 1:
        raise SLOEvidenceError(f"{label} must be between 0 and 1")
    return result


def require_bool(value: Any, label: str) -> bool:
    if not isinstance(value, bool):
        raise SLOEvidenceError(f"{label} must be a boolean")
    return value


def parse_utc(value: Any, label: str) -> dt.datetime:
    if not isinstance(value, str) or not value.endswith("Z"):
        raise SLOEvidenceError(f"{label} must be an RFC3339 UTC timestamp ending in Z")
    try:
        parsed = dt.datetime.fromisoformat(value[:-1] + "+00:00")
    except ValueError as error:
        raise SLOEvidenceError(f"{label} must be a valid RFC3339 UTC timestamp") from error
    if parsed.tzinfo != dt.timezone.utc:
        raise SLOEvidenceError(f"{label} must use UTC")
    return parsed


def resolve_evidence_file(root: pathlib.Path, raw_path: Any, label: str) -> pathlib.Path:
    if not isinstance(raw_path, str) or not raw_path.strip():
        raise SLOEvidenceError(f"{label}.path must be a non-empty relative path")
    relative = pathlib.Path(raw_path)
    if relative.is_absolute() or any(part in {".", ".."} for part in relative.parts):
        raise SLOEvidenceError(f"{label}.path must be traversal-free and relative")
    candidate = root / relative
    current = candidate
    while current != root:
        if current.is_symlink():
            raise SLOEvidenceError(f"{label}.path must not traverse a symlink")
        current = current.parent
    try:
        resolved = candidate.resolve(strict=True)
        resolved.relative_to(root)
    except (OSError, ValueError) as error:
        raise SLOEvidenceError(f"{label}.path must resolve inside the evidence root") from error
    if not resolved.is_file():
        raise SLOEvidenceError(f"{label}.path must reference a regular file")
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
        raise SLOEvidenceError(f"{label} must contain exactly path and sha256")
    path = resolve_evidence_file(root, reference["path"], label)
    if path in used_paths:
        raise SLOEvidenceError(f"{label}.path duplicates another evidence file")
    used_paths.add(path)
    try:
        content = read_stable_regular_file(
            path,
            label=label,
            maximum_bytes=MAX_EVIDENCE_FILE_BYTES,
        )
    except ImmutableEvidenceIOError as error:
        raise SLOEvidenceError(str(error)) from error
    total_bytes[0] += len(content)
    if total_bytes[0] > MAX_TOTAL_EVIDENCE_BYTES:
        raise SLOEvidenceError("total SLO evidence exceeds the bounded size limit")
    scan_for_secret_material(content, label)
    digest = reference["sha256"]
    if not isinstance(digest, str) or SHA256_RE.fullmatch(digest) is None:
        raise SLOEvidenceError(f"{label}.sha256 must be sha256:<64 lowercase hex>")
    actual = "sha256:" + hashlib.sha256(content).hexdigest()
    if actual != digest:
        raise SLOEvidenceError(f"{label}.sha256 does not match the evidence file")
    return {"path": path.relative_to(root).as_posix(), "sha256": actual}


def validate_public_origin(value: Any, environment_class: str) -> str:
    if not isinstance(value, str) or len(value) > 512:
        raise SLOEvidenceError("publicOrigin must be a bounded URL")
    parsed = urlparse(value)
    allowed_schemes = {"https"} if environment_class != "fixture" else {"http", "https"}
    if (
        parsed.scheme not in allowed_schemes
        or not parsed.hostname
        or parsed.username is not None
        or parsed.password is not None
        or parsed.query
        or parsed.fragment
        or parsed.path not in {"", "/"}
    ):
        raise SLOEvidenceError("publicOrigin must be an origin-only URL without credentials")
    return value.rstrip("/")


def calculate_budget_remaining(good_ratio: float, target: float) -> float:
    consumed = (1 - good_ratio) / (1 - target)
    return min(max(1 - consumed, 0.0), 1.0)


def budget_policy_state(remaining: float) -> str:
    if remaining == 0:
        return "reliability-freeze"
    if remaining <= 0.25:
        return "risky-rollout-paused"
    if remaining <= 0.5:
        return "risk-note-required"
    return "normal-delivery"


def validate_probe(
    value: Any,
    window_seconds: float,
) -> tuple[dict[str, Any], bool]:
    probe = require_mapping(value, "externalProbe")
    if set(probe) != {
        "regions",
        "cadenceSeconds",
        "expectedSamples",
        "observedSamples",
    }:
        raise SLOEvidenceError("externalProbe fields do not match the v1 schema")
    regions = probe["regions"]
    if not isinstance(regions, list):
        raise SLOEvidenceError("externalProbe.regions must be a list")
    parsed_regions: list[str] = []
    for index, raw in enumerate(regions):
        region = require_identifier(raw, f"externalProbe.regions[{index}]")
        if region in parsed_regions:
            raise SLOEvidenceError("externalProbe.regions must be unique")
        parsed_regions.append(region)
    cadence = require_integer(probe["cadenceSeconds"], "externalProbe.cadenceSeconds", 1)
    if cadence != 30:
        raise SLOEvidenceError("externalProbe.cadenceSeconds must be 30")
    expected = require_integer(probe["expectedSamples"], "externalProbe.expectedSamples", 1)
    observed = require_integer(probe["observedSamples"], "externalProbe.observedSamples")
    calculated_minimum = math.floor(window_seconds / cadence) * len(parsed_regions)
    if expected < calculated_minimum:
        raise SLOEvidenceError("externalProbe.expectedSamples understates the declared window and regions")
    if observed > expected:
        raise SLOEvidenceError("externalProbe.observedSamples cannot exceed expectedSamples")
    coverage = observed / expected
    complete = len(parsed_regions) >= 3 and coverage >= 0.95
    return {
        "regions": sorted(parsed_regions),
        "regionCount": len(parsed_regions),
        "cadenceSeconds": cadence,
        "expectedSamples": expected,
        "observedSamples": observed,
        "coverageRatio": coverage,
    }, complete


def validate_objectives(value: Any) -> tuple[dict[str, dict[str, Any]], bool, bool]:
    raw_objectives = require_mapping(value, "objectives")
    if set(raw_objectives) != set(OBJECTIVES):
        raise SLOEvidenceError("objectives must contain every Stage 6 SLO exactly once")
    result: dict[str, dict[str, Any]] = {}
    all_assessable = True
    all_met = True
    for name, policy in OBJECTIVES.items():
        raw = require_mapping(raw_objectives[name], f"objectives.{name}")
        if set(raw) != {
            "goodRatio",
            "sampleCount",
            "errorBudgetRemainingRatio",
            "noDataIntervalCount",
            "excludedSampleCount",
            "querySucceeded",
        }:
            raise SLOEvidenceError(f"objectives.{name} fields do not match the v1 schema")
        good_ratio = require_ratio(raw["goodRatio"], f"objectives.{name}.goodRatio")
        sample_count = require_integer(raw["sampleCount"], f"objectives.{name}.sampleCount")
        declared_remaining = require_ratio(
            raw["errorBudgetRemainingRatio"], f"objectives.{name}.errorBudgetRemainingRatio"
        )
        no_data = require_integer(
            raw["noDataIntervalCount"], f"objectives.{name}.noDataIntervalCount"
        )
        excluded = require_integer(
            raw["excludedSampleCount"], f"objectives.{name}.excludedSampleCount"
        )
        query_succeeded = require_bool(raw["querySucceeded"], f"objectives.{name}.querySucceeded")
        calculated_remaining = calculate_budget_remaining(good_ratio, policy["target"])
        if not math.isclose(declared_remaining, calculated_remaining, rel_tol=0, abs_tol=1e-9):
            raise SLOEvidenceError(f"objectives.{name}.errorBudgetRemainingRatio does not match the SLO formula")
        volume_sufficient = sample_count >= policy["minimumSamples"]
        assessable = query_succeeded and no_data == 0 and excluded == 0 and volume_sufficient
        objective_met = assessable and good_ratio >= policy["target"]
        result[name] = {
            "targetRatio": policy["target"],
            "goodRatio": good_ratio,
            "sampleCount": sample_count,
            "minimumSamples": policy["minimumSamples"],
            "volumeSufficient": volume_sufficient,
            "errorBudgetRemainingRatio": calculated_remaining,
            "errorBudgetPolicyState": budget_policy_state(calculated_remaining),
            "noDataIntervalCount": no_data,
            "excludedSampleCount": excluded,
            "querySucceeded": query_succeeded,
            "assessable": assessable,
            "objectiveMet": objective_met,
        }
        all_assessable = all_assessable and assessable
        all_met = all_met and objective_met
    return result, all_assessable, all_met


def validate_alerts(value: Any, availability_no_data: int) -> dict[str, int]:
    alerts = require_mapping(value, "alertSummary")
    if set(alerts) != ALERT_FIELDS:
        raise SLOEvidenceError("alertSummary fields do not match the v1 schema")
    result = {field: require_integer(alerts[field], f"alertSummary.{field}") for field in ALERT_FIELDS}
    if result["publicProbeMissingCount"] > 0 and availability_no_data == 0:
        raise SLOEvidenceError(
            "publicProbeMissingCount requires an Availability no-data interval"
        )
    return {key: result[key] for key in sorted(result)}


def validate_companion_signals(value: Any) -> dict[str, Any]:
    signals = require_mapping(value, "companionSignals")
    if set(signals) != COMPANION_FIELDS:
        raise SLOEvidenceError("companionSignals fields do not match the v1 schema")
    return {
        "executionTerminalBeforeReadyCount": require_integer(
            signals["executionTerminalBeforeReadyCount"],
            "companionSignals.executionTerminalBeforeReadyCount",
        ),
        "maximumQueueOldestSeconds": require_number(
            signals["maximumQueueOldestSeconds"], "companionSignals.maximumQueueOldestSeconds"
        ),
        "maximumOutboxOldestSeconds": require_number(
            signals["maximumOutboxOldestSeconds"], "companionSignals.maximumOutboxOldestSeconds"
        ),
        "expiredSSELeaseCount": require_integer(
            signals["expiredSSELeaseCount"], "companionSignals.expiredSSELeaseCount"
        ),
        "sseCatchupP95Seconds": require_number(
            signals["sseCatchupP95Seconds"], "companionSignals.sseCatchupP95Seconds"
        ),
        "reviewed": require_bool(signals["reviewed"], "companionSignals.reviewed"),
    }


def validate_burn_review(value: Any) -> tuple[dict[str, Any], bool]:
    review = require_mapping(value, "burnReview")
    if set(review) != BURN_REVIEW_FIELDS:
        raise SLOEvidenceError("burnReview fields do not match the v1 schema")
    result = {
        "allMaterialBurnsMapped": require_bool(
            review["allMaterialBurnsMapped"], "burnReview.allMaterialBurnsMapped"
        ),
        "allIncidentsReviewed": require_bool(
            review["allIncidentsReviewed"], "burnReview.allIncidentsReviewed"
        ),
        "allMaintenanceIncluded": require_bool(
            review["allMaintenanceIncluded"], "burnReview.allMaintenanceIncluded"
        ),
        "allExceptionsDocumented": require_bool(
            review["allExceptionsDocumented"], "burnReview.allExceptionsDocumented"
        ),
        "openUnownedCorrectiveActionCount": require_integer(
            review["openUnownedCorrectiveActionCount"],
            "burnReview.openUnownedCorrectiveActionCount",
        ),
    }
    complete = (
        result["allMaterialBurnsMapped"]
        and result["allIncidentsReviewed"]
        and result["allMaintenanceIncluded"]
        and result["allExceptionsDocumented"]
        and result["openUnownedCorrectiveActionCount"] == 0
    )
    return result, complete


def normalized_manifest_reference(value: str) -> str:
    path = pathlib.PurePosixPath(value)
    if (
        path.is_absolute()
        or not path.parts
        or any(part in {"", ".", ".."} for part in path.parts)
        or path.as_posix() != value
    ):
        raise SLOEvidenceError("manifest receipt path must be normalized and relative")
    return value


def validate(
    manifest_path: pathlib.Path,
    evidence_root: pathlib.Path,
    validated_at: str | None,
    manifest_reference_path: str | None = None,
) -> dict[str, Any]:
    root = evidence_root.resolve()
    if evidence_root.is_symlink() or not root.is_dir():
        raise SLOEvidenceError("evidence root must be a regular non-symlink directory")
    try:
        manifest_bytes = read_stable_regular_file(
            manifest_path,
            label="SLO manifest",
            maximum_bytes=MAX_MANIFEST_BYTES,
        )
        scan_for_secret_material(manifest_bytes, "SLO manifest")
        raw = json.loads(manifest_bytes, object_pairs_hook=reject_duplicate_json_fields)
    except ImmutableEvidenceIOError as error:
        raise SLOEvidenceError(str(error)) from error
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise SLOEvidenceError("manifest must be valid JSON") from error
    manifest = require_mapping(raw, "manifest")
    if set(manifest) != {
        "schemaVersion",
        "windowId",
        "releaseCommit",
        "environmentClass",
        "environmentId",
        "publicOrigin",
        "windowStartedAt",
        "windowCompletedAt",
        "queryRevision",
        "externalProbe",
        "objectives",
        "alertSummary",
        "companionSignals",
        "burnReview",
        "evidence",
    }:
        raise SLOEvidenceError("manifest fields do not match the v1 schema")
    if manifest["schemaVersion"] != MANIFEST_SCHEMA:
        raise SLOEvidenceError(f"schemaVersion is not {MANIFEST_SCHEMA}")
    try:
        window_id = str(uuid.UUID(str(manifest["windowId"])))
    except (ValueError, AttributeError) as error:
        raise SLOEvidenceError("windowId must be a UUID") from error
    release_commit = manifest["releaseCommit"]
    if not isinstance(release_commit, str) or COMMIT_RE.fullmatch(release_commit) is None:
        raise SLOEvidenceError("releaseCommit must be a full 40-character lowercase Git SHA")
    environment_class = manifest["environmentClass"]
    if environment_class not in MINIMUM_WINDOW_SECONDS:
        raise SLOEvidenceError("environmentClass is not allowed")
    environment_id = require_identifier(manifest["environmentId"], "environmentId")
    public_origin = validate_public_origin(manifest["publicOrigin"], environment_class)
    started = parse_utc(manifest["windowStartedAt"], "windowStartedAt")
    completed = parse_utc(manifest["windowCompletedAt"], "windowCompletedAt")
    if completed <= started:
        raise SLOEvidenceError("windowCompletedAt must be after windowStartedAt")
    window_seconds = (completed - started).total_seconds()
    if window_seconds < MINIMUM_WINDOW_SECONDS[environment_class]:
        raise SLOEvidenceError("SLO window is below the minimum duration for environmentClass")
    query_revision = manifest["queryRevision"]
    if not isinstance(query_revision, str) or COMMIT_RE.fullmatch(query_revision) is None:
        raise SLOEvidenceError("queryRevision must be a full 40-character lowercase Git SHA")

    probe, probe_coverage_complete = validate_probe(manifest["externalProbe"], window_seconds)
    objectives, all_assessable, all_met = validate_objectives(manifest["objectives"])
    if objectives["availability"]["sampleCount"] != probe["observedSamples"]:
        raise SLOEvidenceError("Availability sampleCount must equal externalProbe.observedSamples")
    alerts = validate_alerts(
        manifest["alertSummary"], objectives["availability"]["noDataIntervalCount"]
    )
    companion_signals = validate_companion_signals(manifest["companionSignals"])
    burn_review, burn_review_complete = validate_burn_review(manifest["burnReview"])

    evidence_raw = require_mapping(manifest["evidence"], "evidence")
    if set(evidence_raw) != set(EVIDENCE_FIELDS):
        raise SLOEvidenceError("evidence fields do not match the v1 schema")
    used_paths: set[pathlib.Path] = set()
    total_bytes = [0]
    evidence = {
        field: validate_reference(
            evidence_raw[field],
            root,
            f"evidence.{field}",
            used_paths,
            total_bytes,
        )
        for field in EVIDENCE_FIELDS
    }
    validation_time = validated_at or dt.datetime.now(dt.timezone.utc).isoformat().replace("+00:00", "Z")
    validated = parse_utc(validation_time, "validatedAt")
    if validated < completed:
        raise SLOEvidenceError("validatedAt must not precede windowCompletedAt")

    release_eligible_environment = environment_class in {"production", "production-like"}
    eligible_for_human_gate_review = all(
        (
            release_eligible_environment,
            probe_coverage_complete,
            all_assessable,
            all_met,
            companion_signals["reviewed"],
            burn_review_complete,
        )
    )
    return {
        "schemaVersion": RECEIPT_SCHEMA,
        "windowId": window_id,
        "releaseCommit": release_commit,
        "environmentClass": environment_class,
        "environmentId": environment_id,
        "publicOrigin": public_origin,
        "windowStartedAt": started.isoformat().replace("+00:00", "Z"),
        "windowCompletedAt": completed.isoformat().replace("+00:00", "Z"),
        "windowSeconds": window_seconds,
        "queryRevision": query_revision,
        "manifest": {
            "path": normalized_manifest_reference(
                manifest_reference_path or manifest_path.name
            ),
            "sha256": "sha256:" + hashlib.sha256(manifest_bytes).hexdigest(),
        },
        "externalProbe": probe,
        "externalProbeCoverageComplete": probe_coverage_complete,
        "objectives": objectives,
        "allObjectivesAssessable": all_assessable,
        "declaredMeasurementsWithinObjectives": all_met,
        "alertSummary": alerts,
        "companionSignals": companion_signals,
        "burnReview": burn_review,
        "burnReviewComplete": burn_review_complete,
        "evidence": evidence,
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
    except (SLOEvidenceError, ImmutableEvidenceIOError) as error:
        parser.exit(2, f"SLO evidence validation failed: {error}\n")
    print(f"wrote {output} and {output.name}.sha256")
    return 0


if __name__ == "__main__":
    sys.exit(main())
