#!/usr/bin/env python3
"""Validate Stage 6 paging/status exercise evidence without declaring operations ready."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
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
MANIFEST_SCHEMA = "synara.incident-communication-exercise-evidence.v2"
RECEIPT_SCHEMA = "synara.incident-communication-exercise-evidence-receipt.v2"
ASSESSMENT = "evidence-validated-not-operations-ready"
MAX_MANIFEST_BYTES = 2 * 1024 * 1024
MAX_EVIDENCE_FILE_BYTES = 16 * 1024 * 1024
MAX_TOTAL_EVIDENCE_BYTES = 128 * 1024 * 1024
PROHIBITED_SECRET_PATTERNS = (
    ("private key", re.compile(rb"-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----")),
    ("AWS access key", re.compile(rb"\bAKIA[0-9A-Z]{16}\b")),
    ("bearer credential", re.compile(rb"(?i)authorization\s*[:=]\s*bearer\s+[^\s,;]+")),
    (
        "credential-bearing URL",
        re.compile(rb"(?i)\b(?:https?|postgres(?:ql)?|mysql)://[^\s/:@]+:[^\s/@]+@"),
    ),
)

SEVERITY_TARGETS = {
    "SEV-0": {"ackSeconds": 300, "firstInternalUpdateSeconds": 900, "updateCadenceSeconds": 1800},
    "SEV-1": {"ackSeconds": 300, "firstInternalUpdateSeconds": 900, "updateCadenceSeconds": 1800},
    "SEV-2": {"ackSeconds": 900, "firstInternalUpdateSeconds": 1800, "updateCadenceSeconds": 3600},
}
STATUS_COMPONENTS = {
    "control-plane-api",
    "authentication-sso",
    "execution-scheduling",
    "worker-runtime",
    "artifact-service",
    "web-application",
}
ROLE_FIELDS = {
    "incidentCommander",
    "operationsLead",
    "communicationsLead",
    "scribe",
    "securityPrivacyLead",
    "releaseObserver",
}
EVIDENCE_FIELDS = (
    "onCallRotaEvidence",
    "pagingProviderEvidence",
    "internalStatusBoardTimelineEvidence",
    "employeeNotificationDeliveryEvidence",
    "internalProbeEvidence",
    "internalUserPathEvidence",
    "exerciseReviewEvidence",
)


class IncidentExerciseEvidenceError(Exception):
    pass


def reject_duplicate_json_fields(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise IncidentExerciseEvidenceError(
                f"manifest contains duplicate JSON field {key!r}"
            )
        result[key] = value
    return result


def scan_for_secret_material(content: bytes, label: str) -> None:
    for secret_label, pattern in PROHIBITED_SECRET_PATTERNS:
        if pattern.search(content):
            raise IncidentExerciseEvidenceError(
                f"{label} contains prohibited {secret_label} material"
            )


def require_mapping(value: Any, label: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise IncidentExerciseEvidenceError(f"{label} must be an object")
    return value


def require_identifier(value: Any, label: str) -> str:
    if not isinstance(value, str) or IDENTIFIER_RE.fullmatch(value.strip()) is None:
        raise IncidentExerciseEvidenceError(f"{label} must be a bounded identifier")
    return value.strip()


def require_bool(value: Any, label: str) -> bool:
    if not isinstance(value, bool):
        raise IncidentExerciseEvidenceError(f"{label} must be a boolean")
    return value


def require_integer(value: Any, label: str, minimum: int = 0) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value < minimum:
        raise IncidentExerciseEvidenceError(f"{label} must be an integer >= {minimum}")
    return value


def parse_utc(value: Any, label: str) -> dt.datetime:
    if not isinstance(value, str) or not value.endswith("Z"):
        raise IncidentExerciseEvidenceError(f"{label} must be an RFC3339 UTC timestamp ending in Z")
    try:
        parsed = dt.datetime.fromisoformat(value[:-1] + "+00:00")
    except ValueError as error:
        raise IncidentExerciseEvidenceError(f"{label} must be a valid RFC3339 UTC timestamp") from error
    if parsed.tzinfo != dt.timezone.utc:
        raise IncidentExerciseEvidenceError(f"{label} must use UTC")
    return parsed


def validate_origin(value: Any, label: str, environment_class: str) -> tuple[str, str]:
    if not isinstance(value, str) or len(value) > 512:
        raise IncidentExerciseEvidenceError(f"{label} must be a bounded URL")
    parsed = urlparse(value)
    schemes = {"https"} if environment_class != "fixture" else {"http", "https"}
    if (
        parsed.scheme not in schemes
        or not parsed.hostname
        or parsed.username is not None
        or parsed.password is not None
        or parsed.query
        or parsed.fragment
        or parsed.path not in {"", "/"}
    ):
        raise IncidentExerciseEvidenceError(f"{label} must be an origin-only URL without credentials")
    return value.rstrip("/"), parsed.netloc.lower()


def resolve_evidence_file(root: pathlib.Path, raw_path: Any, label: str) -> pathlib.Path:
    if (
        not isinstance(raw_path, str)
        or not raw_path.strip()
        or "\\" in raw_path
        or "\x00" in raw_path
    ):
        raise IncidentExerciseEvidenceError(f"{label}.path must be a non-empty relative path")
    relative = pathlib.Path(raw_path)
    if relative.is_absolute() or any(part in {".", ".."} for part in relative.parts):
        raise IncidentExerciseEvidenceError(f"{label}.path must be traversal-free and relative")
    candidate = root / relative
    current = candidate
    while current != root:
        if current.is_symlink():
            raise IncidentExerciseEvidenceError(f"{label}.path must not traverse a symlink")
        current = current.parent
    try:
        resolved = candidate.resolve(strict=True)
        resolved.relative_to(root)
    except (OSError, ValueError) as error:
        raise IncidentExerciseEvidenceError(f"{label}.path must resolve inside the evidence root") from error
    if not resolved.is_file():
        raise IncidentExerciseEvidenceError(f"{label}.path must reference a regular file")
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
        raise IncidentExerciseEvidenceError(f"{label} must contain exactly path and sha256")
    path = resolve_evidence_file(root, reference["path"], label)
    if path in used_paths:
        raise IncidentExerciseEvidenceError(f"{label}.path duplicates another evidence file")
    used_paths.add(path)
    try:
        content = read_stable_regular_file(
            path,
            label=label,
            maximum_bytes=MAX_EVIDENCE_FILE_BYTES,
        )
    except ImmutableEvidenceIOError as error:
        raise IncidentExerciseEvidenceError(str(error)) from error
    total_bytes[0] += len(content)
    if total_bytes[0] > MAX_TOTAL_EVIDENCE_BYTES:
        raise IncidentExerciseEvidenceError(
            "total incident exercise evidence exceeds the bounded size limit"
        )
    scan_for_secret_material(content, label)
    digest = reference["sha256"]
    if not isinstance(digest, str) or SHA256_RE.fullmatch(digest) is None:
        raise IncidentExerciseEvidenceError(f"{label}.sha256 must be sha256:<64 lowercase hex>")
    actual = "sha256:" + hashlib.sha256(content).hexdigest()
    if actual != digest:
        raise IncidentExerciseEvidenceError(f"{label}.sha256 does not match the evidence file")
    return {"path": path.relative_to(root).as_posix(), "sha256": actual}


def validate_roles(value: Any) -> tuple[dict[str, str], bool]:
    roles = require_mapping(value, "roles")
    if set(roles) != ROLE_FIELDS:
        raise IncidentExerciseEvidenceError("roles fields do not match the v2 schema")
    result = {field: require_identifier(roles[field], f"roles.{field}") for field in ROLE_FIELDS}
    separation = (
        result["incidentCommander"] != result["communicationsLead"]
        and result["incidentCommander"] != result["releaseObserver"]
    )
    return {key: result[key] for key in sorted(result)}, separation


def validate_paging(
    value: Any,
    started: dt.datetime,
    completed: dt.datetime,
    target_seconds: int,
) -> tuple[dict[str, Any], bool]:
    paging = require_mapping(value, "paging")
    if set(paging) != {
        "providerReference",
        "primaryResponder",
        "secondaryResponder",
        "pageTriggeredAt",
        "acknowledgedAt",
        "acknowledgedBy",
        "escalationExercised",
        "pagingSucceeded",
    }:
        raise IncidentExerciseEvidenceError("paging fields do not match the v2 schema")
    provider = require_identifier(paging["providerReference"], "paging.providerReference")
    primary = require_identifier(paging["primaryResponder"], "paging.primaryResponder")
    secondary = require_identifier(paging["secondaryResponder"], "paging.secondaryResponder")
    if primary == secondary:
        raise IncidentExerciseEvidenceError("paging primary and secondary responders must differ")
    acknowledged_by = require_identifier(paging["acknowledgedBy"], "paging.acknowledgedBy")
    if acknowledged_by not in {primary, secondary}:
        raise IncidentExerciseEvidenceError("paging.acknowledgedBy must be the primary or secondary responder")
    triggered = parse_utc(paging["pageTriggeredAt"], "paging.pageTriggeredAt")
    acknowledged = parse_utc(paging["acknowledgedAt"], "paging.acknowledgedAt")
    if not (started <= triggered <= acknowledged <= completed):
        raise IncidentExerciseEvidenceError("paging timestamps are out of order or outside the exercise")
    ack_seconds = (acknowledged - triggered).total_seconds()
    succeeded = require_bool(paging["pagingSucceeded"], "paging.pagingSucceeded")
    escalation = require_bool(paging["escalationExercised"], "paging.escalationExercised")
    within_target = succeeded and ack_seconds <= target_seconds
    return {
        "providerReference": provider,
        "primaryResponder": primary,
        "secondaryResponder": secondary,
        "pageTriggeredAt": triggered.isoformat().replace("+00:00", "Z"),
        "acknowledgedAt": acknowledged.isoformat().replace("+00:00", "Z"),
        "acknowledgedBy": acknowledged_by,
        "acknowledgementSeconds": ack_seconds,
        "targetSeconds": target_seconds,
        "withinTarget": within_target,
        "escalationExercised": escalation,
        "pagingSucceeded": succeeded,
    }, within_target and escalation


def validate_components(value: Any, region_promise_enabled: bool) -> tuple[dict[str, Any], bool]:
    components = require_mapping(value, "internalStatusBoardComponents")
    if set(components) != STATUS_COMPONENTS:
        raise IncidentExerciseEvidenceError(
            "internalStatusBoardComponents must contain every required internal service component exactly once"
        )
    result: dict[str, dict[str, bool]] = {}
    complete = True
    for name in sorted(STATUS_COMPONENTS):
        component = require_mapping(components[name], f"internalStatusBoardComponents.{name}")
        if set(component) != {"published", "regionDetailPublished"}:
            raise IncidentExerciseEvidenceError(
                f"internalStatusBoardComponents.{name} fields do not match the v2 schema"
            )
        published = require_bool(component["published"], f"internalStatusBoardComponents.{name}.published")
        region_detail = require_bool(
            component["regionDetailPublished"],
            f"internalStatusBoardComponents.{name}.regionDetailPublished",
        )
        complete = complete and published and (not region_promise_enabled or region_detail)
        result[name] = {
            "published": published,
            "regionDetailPublished": region_detail,
        }
    return result, complete


def validate_internal_timeline(
    value: Any,
    started: dt.datetime,
    completed: dt.datetime,
    targets: dict[str, int],
) -> tuple[dict[str, Any], bool, dt.datetime, dt.datetime]:
    timeline = require_mapping(value, "internalTimeline")
    if set(timeline) != {
        "impactConfirmedAt",
        "incidentOpenedAt",
        "updates",
        "internalHistoryVisible",
    }:
        raise IncidentExerciseEvidenceError("internalTimeline fields do not match the v2 schema")
    confirmed = parse_utc(timeline["impactConfirmedAt"], "internalTimeline.impactConfirmedAt")
    opened = parse_utc(timeline["incidentOpenedAt"], "internalTimeline.incidentOpenedAt")
    if not (started <= confirmed <= opened <= completed):
        raise IncidentExerciseEvidenceError("internalTimeline confirmation/open timestamps are invalid")
    updates = timeline["updates"]
    if not isinstance(updates, list) or len(updates) < 3:
        raise IncidentExerciseEvidenceError("internalTimeline.updates requires initial, progress, and resolved updates")
    parsed_updates: list[dict[str, Any]] = []
    previous = opened
    progress_count = 0
    cadence_within_target = True
    for index, raw in enumerate(updates):
        update = require_mapping(raw, f"internalTimeline.updates[{index}]")
        if set(update) != {"kind", "publishedAt"}:
            raise IncidentExerciseEvidenceError(
                f"internalTimeline.updates[{index}] fields do not match the v2 schema"
            )
        kind = update["kind"]
        if kind not in {"initial", "progress", "resolved"}:
            raise IncidentExerciseEvidenceError(f"internalTimeline.updates[{index}].kind is not allowed")
        published = parse_utc(update["publishedAt"], f"internalTimeline.updates[{index}].publishedAt")
        if not previous <= published <= completed:
            raise IncidentExerciseEvidenceError("internalTimeline updates are unordered or outside the exercise")
        if index > 0 and (published - previous).total_seconds() > targets["updateCadenceSeconds"]:
            cadence_within_target = False
        if kind == "progress":
            progress_count += 1
        parsed_updates.append(
            {"kind": kind, "publishedAt": published.isoformat().replace("+00:00", "Z")}
        )
        previous = published
    if parsed_updates[0]["kind"] != "initial" or parsed_updates[-1]["kind"] != "resolved":
        raise IncidentExerciseEvidenceError("internalTimeline must start with initial and end with resolved")
    if progress_count == 0 or any(
        update["kind"] in {"initial", "resolved"}
        for update in parsed_updates[1:-1]
    ):
        raise IncidentExerciseEvidenceError("internalTimeline middle updates must be progress updates")
    first_update = parse_utc(parsed_updates[0]["publishedAt"], "internalTimeline.initial")
    resolved = parse_utc(parsed_updates[-1]["publishedAt"], "internalTimeline.resolved")
    first_update_seconds = (first_update - confirmed).total_seconds()
    first_within_target = first_update_seconds <= targets["firstInternalUpdateSeconds"]
    internal_history = require_bool(
        timeline["internalHistoryVisible"], "internalTimeline.internalHistoryVisible"
    )
    complete = first_within_target and cadence_within_target and internal_history
    return {
        "impactConfirmedAt": confirmed.isoformat().replace("+00:00", "Z"),
        "incidentOpenedAt": opened.isoformat().replace("+00:00", "Z"),
        "updates": parsed_updates,
        "firstInternalUpdateSeconds": first_update_seconds,
        "firstInternalUpdateTargetSeconds": targets["firstInternalUpdateSeconds"],
        "firstInternalUpdateWithinTarget": first_within_target,
        "updateCadenceTargetSeconds": targets["updateCadenceSeconds"],
        "updateCadenceWithinTarget": cadence_within_target,
        "internalHistoryVisible": internal_history,
    }, complete, first_update, resolved


def validate_employee_notification_delivery(
    value: Any,
    first_update: dt.datetime,
    resolved: dt.datetime,
    completed: dt.datetime,
) -> tuple[dict[str, Any], bool]:
    delivery = require_mapping(value, "employeeNotificationDelivery")
    if set(delivery) != {
        "channelReference",
        "initialNotificationReceivedAt",
        "resolutionNotificationReceivedAt",
        "deliverySucceeded",
    }:
        raise IncidentExerciseEvidenceError("employeeNotificationDelivery fields do not match the v2 schema")
    channel = require_identifier(delivery["channelReference"], "employeeNotificationDelivery.channelReference")
    initial_received = parse_utc(
        delivery["initialNotificationReceivedAt"],
        "employeeNotificationDelivery.initialNotificationReceivedAt",
    )
    resolution_received = parse_utc(
        delivery["resolutionNotificationReceivedAt"],
        "employeeNotificationDelivery.resolutionNotificationReceivedAt",
    )
    if not (first_update <= initial_received <= resolved <= resolution_received <= completed):
        raise IncidentExerciseEvidenceError("employeeNotificationDelivery timestamps are invalid")
    succeeded = require_bool(delivery["deliverySucceeded"], "employeeNotificationDelivery.deliverySucceeded")
    return {
        "channelReference": channel,
        "initialNotificationReceivedAt": initial_received.isoformat().replace("+00:00", "Z"),
        "initialDeliverySeconds": (initial_received - first_update).total_seconds(),
        "resolutionNotificationReceivedAt": resolution_received.isoformat().replace("+00:00", "Z"),
        "resolutionDeliverySeconds": (resolution_received - resolved).total_seconds(),
        "deliverySucceeded": succeeded,
    }, succeeded


def validate_recovery(
    value: Any,
    started: dt.datetime,
    resolved: dt.datetime,
) -> tuple[dict[str, Any], bool]:
    recovery = require_mapping(value, "recoveryVerification")
    if set(recovery) != {
        "recoveredAt",
        "internalProbePassed",
        "internalUserPathPassed",
        "cleanupCompleted",
    }:
        raise IncidentExerciseEvidenceError("recoveryVerification fields do not match the v2 schema")
    recovered = parse_utc(recovery["recoveredAt"], "recoveryVerification.recoveredAt")
    if not started <= recovered <= resolved:
        raise IncidentExerciseEvidenceError("recoveryVerification.recoveredAt is outside the exercise")
    observation_seconds = (resolved - recovered).total_seconds()
    internal_probe = require_bool(
        recovery["internalProbePassed"], "recoveryVerification.internalProbePassed"
    )
    internal_user_path = require_bool(
        recovery["internalUserPathPassed"], "recoveryVerification.internalUserPathPassed"
    )
    cleanup = require_bool(recovery["cleanupCompleted"], "recoveryVerification.cleanupCompleted")
    complete = internal_probe and internal_user_path and cleanup and observation_seconds >= 900
    return {
        "recoveredAt": recovered.isoformat().replace("+00:00", "Z"),
        "observationSecondsBeforeResolution": observation_seconds,
        "minimumObservationSeconds": 900,
        "internalProbePassed": internal_probe,
        "internalUserPathPassed": internal_user_path,
        "cleanupCompleted": cleanup,
    }, complete


def validate_review(value: Any) -> tuple[dict[str, bool], bool]:
    review = require_mapping(value, "review")
    expected = {
        "incidentTimelineConsistent",
        "correctiveActionsOwned",
        "sensitiveDataAbsent",
        "operationsApproved",
        "communicationsApproved",
    }
    if set(review) != expected:
        raise IncidentExerciseEvidenceError("review fields do not match the v2 schema")
    result = {field: require_bool(review[field], f"review.{field}") for field in expected}
    return {key: result[key] for key in sorted(result)}, all(result.values())


def normalized_manifest_reference(value: str) -> str:
    path = pathlib.PurePosixPath(value)
    if (
        path.is_absolute()
        or not path.parts
        or any(part in {"", ".", ".."} for part in path.parts)
        or path.as_posix() != value
    ):
        raise IncidentExerciseEvidenceError(
            "manifest receipt path must be normalized and relative"
        )
    return value


def validate(
    manifest_path: pathlib.Path,
    evidence_root: pathlib.Path,
    validated_at: str | None,
    manifest_reference_path: str | None = None,
) -> dict[str, Any]:
    root = evidence_root.resolve()
    if evidence_root.is_symlink() or not root.is_dir():
        raise IncidentExerciseEvidenceError("evidence root must be a regular non-symlink directory")
    try:
        manifest_bytes = read_stable_regular_file(
            manifest_path,
            label="incident exercise manifest",
            maximum_bytes=MAX_MANIFEST_BYTES,
        )
        scan_for_secret_material(manifest_bytes, "incident exercise manifest")
        raw = json.loads(
            manifest_bytes,
            object_pairs_hook=reject_duplicate_json_fields,
        )
    except ImmutableEvidenceIOError as error:
        raise IncidentExerciseEvidenceError(str(error)) from error
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise IncidentExerciseEvidenceError("manifest must be valid JSON") from error
    manifest = require_mapping(raw, "manifest")
    if set(manifest) != {
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
    }:
        raise IncidentExerciseEvidenceError("manifest fields do not match the v2 schema")
    if manifest["schemaVersion"] != MANIFEST_SCHEMA:
        raise IncidentExerciseEvidenceError(
            "schemaVersion is not synara.incident-communication-exercise-evidence.v2"
        )
    try:
        exercise_id = str(uuid.UUID(str(manifest["exerciseId"])))
    except (ValueError, AttributeError) as error:
        raise IncidentExerciseEvidenceError("exerciseId must be a UUID") from error
    release_commit = manifest["releaseCommit"]
    if not isinstance(release_commit, str) or COMMIT_RE.fullmatch(release_commit) is None:
        raise IncidentExerciseEvidenceError("releaseCommit must be a full 40-character lowercase Git SHA")
    environment_class = manifest["environmentClass"]
    if environment_class not in {"production", "production-like", "staging", "fixture"}:
        raise IncidentExerciseEvidenceError("environmentClass is not allowed")
    environment_id = require_identifier(manifest["environmentId"], "environmentId")
    exercise_mode = manifest["exerciseMode"]
    if exercise_mode not in {"live-internal-communication", "tabletop"}:
        raise IncidentExerciseEvidenceError("exerciseMode is not allowed")
    severity = manifest["severity"]
    if severity not in SEVERITY_TARGETS:
        raise IncidentExerciseEvidenceError("severity is not allowed")
    targets = SEVERITY_TARGETS[severity]
    service_origin, service_netloc = validate_origin(
        manifest["serviceOrigin"], "serviceOrigin", environment_class
    )
    status_origin, status_netloc = validate_origin(
        manifest["internalStatusBoardOrigin"], "internalStatusBoardOrigin", environment_class
    )
    operationally_independent = require_bool(
        manifest["internalStatusBoardOperationallyIndependent"],
        "internalStatusBoardOperationallyIndependent",
    )
    independent_status_page = operationally_independent and service_netloc != status_netloc
    region_promise = require_bool(manifest["regionPromiseEnabled"], "regionPromiseEnabled")
    started = parse_utc(manifest["startedAt"], "startedAt")
    completed = parse_utc(manifest["completedAt"], "completedAt")
    if completed <= started:
        raise IncidentExerciseEvidenceError("completedAt must be after startedAt")

    roles, role_separation = validate_roles(manifest["roles"])
    paging, paging_complete = validate_paging(
        manifest["paging"], started, completed, targets["ackSeconds"]
    )
    components, components_complete = validate_components(
        manifest["internalStatusBoardComponents"], region_promise
    )
    timeline, timeline_complete, first_update, resolved = validate_internal_timeline(
        manifest["internalTimeline"], started, completed, targets
    )
    delivery, delivery_complete = validate_employee_notification_delivery(
        manifest["employeeNotificationDelivery"], first_update, resolved, completed
    )
    recovery, recovery_complete = validate_recovery(
        manifest["recoveryVerification"], started, resolved
    )
    review, review_complete = validate_review(manifest["review"])

    evidence_raw = require_mapping(manifest["evidence"], "evidence")
    if set(evidence_raw) != set(EVIDENCE_FIELDS):
        raise IncidentExerciseEvidenceError("evidence fields do not match the v2 schema")
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
        raise IncidentExerciseEvidenceError("validatedAt must not precede completedAt")

    release_eligible_environment = environment_class in {"production", "production-like"}
    live_exercise = exercise_mode == "live-internal-communication"
    eligible_for_human_gate_review = all(
        (
            release_eligible_environment,
            live_exercise,
            independent_status_page,
            role_separation,
            paging_complete,
            components_complete,
            timeline_complete,
            delivery_complete,
            recovery_complete,
            review_complete,
        )
    )
    return {
        "schemaVersion": RECEIPT_SCHEMA,
        "exerciseId": exercise_id,
        "releaseCommit": release_commit,
        "environmentClass": environment_class,
        "environmentId": environment_id,
        "exerciseMode": exercise_mode,
        "severity": severity,
        "serviceOrigin": service_origin,
        "internalStatusBoardOrigin": status_origin,
        "independentInternalStatusBoardDeclared": independent_status_page,
        "regionPromiseEnabled": region_promise,
        "startedAt": started.isoformat().replace("+00:00", "Z"),
        "completedAt": completed.isoformat().replace("+00:00", "Z"),
        "manifest": {
            "path": normalized_manifest_reference(
                manifest_reference_path or manifest_path.name
            ),
            "sha256": "sha256:" + hashlib.sha256(manifest_bytes).hexdigest(),
        },
        "roles": roles,
        "roleSeparationComplete": role_separation,
        "paging": paging,
        "pagingExerciseComplete": paging_complete,
        "internalStatusBoardComponents": components,
        "internalStatusBoardComponentsComplete": components_complete,
        "internalTimeline": timeline,
        "internalTimelineWithinTargets": timeline_complete,
        "employeeNotificationDelivery": delivery,
        "employeeNotificationDeliveryComplete": delivery_complete,
        "recoveryVerification": recovery,
        "recoveryVerificationComplete": recovery_complete,
        "review": review,
        "reviewComplete": review_complete,
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
    except (IncidentExerciseEvidenceError, ImmutableEvidenceIOError) as error:
        parser.exit(2, f"incident exercise evidence validation failed: {error}\n")
    print(f"wrote {output} and {output.name}.sha256")
    return 0


if __name__ == "__main__":
    sys.exit(main())
