#!/usr/bin/env python3
"""Validate candidate-bound Stage 6 recovery evidence without declaring control passage."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import pathlib
import re
import sys
import urllib.parse
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


SCHEMA_VERSION = "synara.recovery-drill-evidence.v2"
RECEIPT_SCHEMA_VERSION = "synara.recovery-drill-evidence-receipt.v2"
APPROVAL_SCHEMA_VERSION = "synara.recovery-drill-approval-evidence.v1"
ASSESSMENT = "evidence-validated-not-control-passed"

COMMIT_RE = re.compile(r"^[0-9a-f]{40}$")
HEX_SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
SHA256_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
IDENTIFIER_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:/@+-]{1,199}$")

MAX_MANIFEST_BYTES = 2 * 1024 * 1024
MAX_EVIDENCE_FILE_BYTES = 16 * 1024 * 1024
MAX_TOTAL_EVIDENCE_BYTES = 256 * 1024 * 1024
MAX_VALIDATION_DELAY = dt.timedelta(days=7)
PROHIBITED_SECRET_PATTERNS = (
    ("private key", re.compile(rb"-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----")),
    ("AWS access key", re.compile(rb"\bAKIA[0-9A-Z]{16}\b")),
    ("bearer credential", re.compile(rb"(?i)authorization\s*[:=]\s*bearer\s+[^\s,;]+")),
    (
        "credential-bearing URL",
        re.compile(rb"(?i)\b(?:https?|postgres(?:ql)?|mysql)://[^\s/:@]+:[^\s/@]+@"),
    ),
)

ENVIRONMENT_CLASSES = {"production", "production-like", "staging", "fixture"}
RELEASE_ELIGIBLE_ENVIRONMENTS = {"production", "production-like"}
DESKTOP_TARGETS = {"linux-x64", "macos-arm64", "macos-x64", "windows-x64"}
ARTIFACT_FIELDS = {
    "adminArtifact",
    "controlPlaneImage",
    "desktopArtifacts",
    "providerHostImage",
    "webArtifact",
    "workerImage",
}
ORIGIN_FIELDS = {"adminBaseUrl", "controlPlaneBaseUrl", "webBaseUrl"}
APPROVAL_ROLES = {"database", "kms", "operations", "security", "storage"}
APPROVAL_DECISIONS = {"approved-for-human-gate-review", "reviewed-not-approved"}
REQUIRED_COMPONENTS = {"kms", "object-storage", "postgresql", "queue"}
EVIDENCE_FIELDS = ("backupEvidence", "restoreEvidence", "clientEvidence")

PROFILE_OBJECTIVES = {
    ("postgresql", "postgresql-pitr"): (300.0, 3600.0),
    ("object-storage", "object-versioned-replica"): (900.0, 7200.0),
    ("kms", "kms-cloud-multi-region"): (0.0, 3600.0),
    ("kms", "kms-vault-raft-snapshot"): (86400.0, 7200.0),
    ("queue", "queue-postgres-outbox-replay"): (300.0, 3600.0),
}


class RecoveryEvidenceError(Exception):
    pass


def reject_duplicate_json_fields(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise RecoveryEvidenceError(f"manifest contains duplicate JSON field {key!r}")
        result[key] = value
    return result


def scan_for_secret_material(content: bytes, label: str) -> None:
    for secret_label, pattern in PROHIBITED_SECRET_PATTERNS:
        if pattern.search(content):
            raise RecoveryEvidenceError(f"{label} contains prohibited {secret_label} material")


def require_mapping(value: Any, label: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise RecoveryEvidenceError(f"{label} must be an object")
    return value


def require_exact(value: Any, fields: set[str], label: str) -> dict[str, Any]:
    mapping = require_mapping(value, label)
    if set(mapping) != fields:
        raise RecoveryEvidenceError(f"{label} fields do not match the v2 schema")
    return mapping


def require_identifier(value: Any, label: str) -> str:
    if not isinstance(value, str) or IDENTIFIER_RE.fullmatch(value.strip()) is None:
        raise RecoveryEvidenceError(f"{label} must be a bounded identifier")
    return value.strip()


def require_digest(value: Any, label: str) -> str:
    if not isinstance(value, str) or SHA256_RE.fullmatch(value) is None:
        raise RecoveryEvidenceError(f"{label} must be sha256:<64 lowercase hex>")
    return value


def require_bool(value: Any, label: str) -> bool:
    if not isinstance(value, bool):
        raise RecoveryEvidenceError(f"{label} must be a boolean")
    return value


def parse_utc(value: Any, label: str) -> dt.datetime:
    if not isinstance(value, str) or not value.endswith("Z"):
        raise RecoveryEvidenceError(f"{label} must be an RFC3339 UTC timestamp ending in Z")
    try:
        parsed = dt.datetime.fromisoformat(value[:-1] + "+00:00")
    except ValueError as error:
        raise RecoveryEvidenceError(f"{label} must be a valid RFC3339 UTC timestamp") from error
    if parsed.tzinfo != dt.timezone.utc:
        raise RecoveryEvidenceError(f"{label} must use UTC")
    return parsed


def format_utc(value: dt.datetime) -> str:
    return value.isoformat().replace("+00:00", "Z")


def normalize_https_url(value: Any, label: str, *, origin_only: bool = False) -> str:
    if not isinstance(value, str) or not value.strip():
        raise RecoveryEvidenceError(f"{label} must be a non-empty HTTPS URL")
    parsed = urllib.parse.urlsplit(value.strip())
    if (
        parsed.scheme != "https"
        or not parsed.hostname
        or parsed.username is not None
        or parsed.password is not None
        or parsed.query
        or parsed.fragment
        or (origin_only and parsed.path not in {"", "/"})
    ):
        raise RecoveryEvidenceError(f"{label} must be a credential-free HTTPS URL")
    try:
        port = parsed.port
    except ValueError as error:
        raise RecoveryEvidenceError(f"{label} has an invalid port") from error
    host = parsed.hostname.lower()
    if ":" in host and not host.startswith("["):
        host = f"[{host}]"
    authority = host if port is None or port == 443 else f"{host}:{port}"
    path = "" if origin_only else parsed.path.rstrip("/")
    return f"https://{authority}{path}"


def resolve_evidence_file(root: pathlib.Path, raw_path: Any, label: str) -> pathlib.Path:
    if not isinstance(raw_path, str) or not raw_path.strip():
        raise RecoveryEvidenceError(f"{label}.path must be a non-empty relative path")
    if "\\" in raw_path or "\x00" in raw_path:
        raise RecoveryEvidenceError(f"{label}.path must be traversal-free and relative")
    parts = raw_path.split("/")
    if raw_path.startswith("/") or any(part in {"", ".", ".."} for part in parts):
        raise RecoveryEvidenceError(f"{label}.path must be traversal-free and relative")
    candidate = root.joinpath(*parts)
    current = candidate
    while current != root:
        if current.is_symlink():
            raise RecoveryEvidenceError(f"{label}.path must not traverse a symlink")
        current = current.parent
    try:
        resolved = candidate.resolve(strict=True)
        resolved.relative_to(root)
    except (OSError, ValueError) as error:
        raise RecoveryEvidenceError(f"{label}.path must resolve inside the evidence root") from error
    if not resolved.is_file():
        raise RecoveryEvidenceError(f"{label}.path must reference a regular file")
    return resolved


def normalized_manifest_reference(value: str) -> str:
    path = pathlib.PurePosixPath(value)
    if (
        path.is_absolute()
        or not path.parts
        or any(part in {"", ".", ".."} for part in path.parts)
        or path.as_posix() != value
    ):
        raise RecoveryEvidenceError(
            "manifest receipt path must be normalized and relative"
        )
    return value


def validate_evidence_reference(
    value: Any,
    root: pathlib.Path,
    label: str,
    used_paths: set[pathlib.Path],
    total_bytes: list[int],
) -> tuple[dict[str, str], pathlib.Path, bytes]:
    reference = require_exact(value, {"path", "sha256"}, label)
    path = resolve_evidence_file(root, reference["path"], label)
    if path in used_paths:
        raise RecoveryEvidenceError(f"{label}.path duplicates another evidence file")
    used_paths.add(path)
    try:
        content = read_stable_regular_file(
            path,
            label=label,
            maximum_bytes=MAX_EVIDENCE_FILE_BYTES,
        )
    except ImmutableEvidenceIOError as error:
        raise RecoveryEvidenceError(str(error)) from error
    scan_for_secret_material(content, label)
    total_bytes[0] += len(content)
    if total_bytes[0] > MAX_TOTAL_EVIDENCE_BYTES:
        raise RecoveryEvidenceError(
            f"evidence exceeds the {MAX_TOTAL_EVIDENCE_BYTES}-byte aggregate limit"
        )
    declared = require_digest(reference["sha256"], f"{label}.sha256")
    actual = "sha256:" + hashlib.sha256(content).hexdigest()
    if declared != actual:
        raise RecoveryEvidenceError(f"{label}.sha256 does not match the evidence file")
    return {"path": path.relative_to(root).as_posix(), "sha256": actual}, path, content


def validate_regions(value: Any, label: str) -> list[str]:
    if not isinstance(value, list) or not value or len(value) > 16:
        raise RecoveryEvidenceError(f"{label} must contain 1..16 Regions")
    regions = [require_identifier(raw, f"{label}[{index}]") for index, raw in enumerate(value)]
    if regions != sorted(regions) or len(set(regions)) != len(regions):
        raise RecoveryEvidenceError(f"{label} must be sorted and unique")
    return regions


def validate_artifacts(value: Any, label: str) -> dict[str, Any]:
    artifacts = require_exact(value, ARTIFACT_FIELDS, label)
    normalized = {
        field: require_digest(artifacts[field], f"{label}.{field}")
        for field in (
            "adminArtifact",
            "controlPlaneImage",
            "providerHostImage",
            "webArtifact",
            "workerImage",
        )
    }
    desktop = require_exact(artifacts["desktopArtifacts"], DESKTOP_TARGETS, f"{label}.desktopArtifacts")
    normalized["desktopArtifacts"] = {
        target: require_digest(desktop[target], f"{label}.desktopArtifacts.{target}")
        for target in sorted(DESKTOP_TARGETS)
    }
    return normalized


def validate_migration(value: Any, label: str) -> dict[str, str]:
    migration = require_exact(value, {"name", "sha256"}, label)
    return {
        "name": require_identifier(migration["name"], f"{label}.name"),
        "sha256": require_digest(migration["sha256"], f"{label}.sha256"),
    }


def validate_candidate(value: Any) -> dict[str, Any]:
    candidate = require_exact(
        value,
        {
            "artifacts",
            "candidateId",
            "environmentClass",
            "environmentId",
            "lockfileSha256",
            "migrationTail",
            "origins",
            "regions",
            "sourceCommit",
        },
        "candidate",
    )
    commit = candidate["sourceCommit"]
    if not isinstance(commit, str) or COMMIT_RE.fullmatch(commit) is None:
        raise RecoveryEvidenceError("candidate.sourceCommit must be a full lowercase Git SHA")
    lockfile = candidate["lockfileSha256"]
    if not isinstance(lockfile, str) or HEX_SHA256_RE.fullmatch(lockfile) is None:
        raise RecoveryEvidenceError("candidate.lockfileSha256 must be 64 lowercase hex")
    environment_class = candidate["environmentClass"]
    if environment_class not in ENVIRONMENT_CLASSES:
        raise RecoveryEvidenceError("candidate.environmentClass is not allowed")
    origins = require_exact(candidate["origins"], ORIGIN_FIELDS, "candidate.origins")
    normalized_origins = {
        "controlPlaneBaseUrl": normalize_https_url(
            origins["controlPlaneBaseUrl"], "candidate.origins.controlPlaneBaseUrl"
        ),
        "webBaseUrl": normalize_https_url(
            origins["webBaseUrl"], "candidate.origins.webBaseUrl", origin_only=True
        ),
        "adminBaseUrl": normalize_https_url(
            origins["adminBaseUrl"], "candidate.origins.adminBaseUrl", origin_only=True
        ),
    }
    if normalized_origins["webBaseUrl"] == normalized_origins["adminBaseUrl"]:
        raise RecoveryEvidenceError("candidate Web and Admin origins must be distinct")
    return {
        "candidateId": require_identifier(candidate["candidateId"], "candidate.candidateId"),
        "sourceCommit": commit,
        "lockfileSha256": lockfile,
        "environmentClass": environment_class,
        "environmentId": require_identifier(candidate["environmentId"], "candidate.environmentId"),
        "regions": validate_regions(candidate["regions"], "candidate.regions"),
        "origins": normalized_origins,
        "artifacts": validate_artifacts(candidate["artifacts"], "candidate.artifacts"),
        "migrationTail": validate_migration(candidate["migrationTail"], "candidate.migrationTail"),
    }


def validate_restored_release_identity(value: Any, candidate: dict[str, Any]) -> dict[str, Any]:
    identity = require_exact(
        value,
        {"artifacts", "lockfileSha256", "migrationTail", "sourceCommit"},
        "restoredReleaseIdentity",
    )
    commit = identity["sourceCommit"]
    if not isinstance(commit, str) or COMMIT_RE.fullmatch(commit) is None:
        raise RecoveryEvidenceError("restoredReleaseIdentity.sourceCommit is invalid")
    lockfile = identity["lockfileSha256"]
    if not isinstance(lockfile, str) or HEX_SHA256_RE.fullmatch(lockfile) is None:
        raise RecoveryEvidenceError("restoredReleaseIdentity.lockfileSha256 is invalid")
    normalized = {
        "sourceCommit": commit,
        "lockfileSha256": lockfile,
        "artifacts": validate_artifacts(identity["artifacts"], "restoredReleaseIdentity.artifacts"),
        "migrationTail": validate_migration(
            identity["migrationTail"], "restoredReleaseIdentity.migrationTail"
        ),
    }
    expected = {
        "sourceCommit": candidate["sourceCommit"],
        "lockfileSha256": candidate["lockfileSha256"],
        "artifacts": candidate["artifacts"],
        "migrationTail": candidate["migrationTail"],
    }
    if normalized != expected:
        raise RecoveryEvidenceError(
            "restoredReleaseIdentity does not match the exact release candidate; stale artifact detected"
        )
    return normalized


def validate_component(
    value: Any,
    root: pathlib.Path,
    drill_started: dt.datetime,
    drill_completed: dt.datetime,
    candidate_regions: set[str],
    used_paths: set[pathlib.Path],
    total_bytes: list[int],
) -> tuple[str, dict[str, Any]]:
    expected_fields = {
        "backupEvidence",
        "clientEvidence",
        "component",
        "profile",
        "recoveryStartedAt",
        "restoreEvidence",
        "restoreFailureDomain",
        "restoreRegion",
        "restoreServedCanary",
        "restoreTarget",
        "restoredThroughAt",
        "serviceReadyAt",
        "sourceAuthority",
        "sourceCutoffAt",
        "sourceFailureDomain",
        "sourceRegion",
    }
    component = require_exact(value, expected_fields, "component")
    name = require_identifier(component["component"], "component.component")
    profile = require_identifier(component["profile"], f"{name}.profile")
    objective = PROFILE_OBJECTIVES.get((name, profile))
    if objective is None:
        raise RecoveryEvidenceError(f"{name}.profile is not allowed for that component")
    source_authority = require_identifier(component["sourceAuthority"], f"{name}.sourceAuthority")
    restore_target = require_identifier(component["restoreTarget"], f"{name}.restoreTarget")
    if source_authority == restore_target:
        raise RecoveryEvidenceError(f"{name} restoreTarget must differ from sourceAuthority")
    source_domain = require_identifier(
        component["sourceFailureDomain"], f"{name}.sourceFailureDomain"
    )
    restore_domain = require_identifier(
        component["restoreFailureDomain"], f"{name}.restoreFailureDomain"
    )
    if source_domain == restore_domain:
        raise RecoveryEvidenceError(
            f"{name} restoreFailureDomain must differ from sourceFailureDomain"
        )
    source_region = require_identifier(component["sourceRegion"], f"{name}.sourceRegion")
    restore_region = require_identifier(component["restoreRegion"], f"{name}.restoreRegion")
    if source_region == restore_region:
        raise RecoveryEvidenceError(f"{name} restoreRegion must differ from sourceRegion")
    if {source_region, restore_region} - candidate_regions:
        raise RecoveryEvidenceError(f"{name} source/restore Regions must belong to candidate.regions")
    restored_through = parse_utc(component["restoredThroughAt"], f"{name}.restoredThroughAt")
    source_cutoff = parse_utc(component["sourceCutoffAt"], f"{name}.sourceCutoffAt")
    recovery_started = parse_utc(component["recoveryStartedAt"], f"{name}.recoveryStartedAt")
    service_ready = parse_utc(component["serviceReadyAt"], f"{name}.serviceReadyAt")
    if not (
        drill_started
        <= restored_through
        <= source_cutoff
        <= recovery_started
        <= service_ready
        <= drill_completed
    ):
        raise RecoveryEvidenceError(
            f"{name} recovery timestamps are out of order or outside the drill window"
        )
    canary = require_bool(component["restoreServedCanary"], f"{name}.restoreServedCanary")
    rpo_seconds = (source_cutoff - restored_through).total_seconds()
    rto_seconds = (service_ready - recovery_started).total_seconds()
    rpo_objective, rto_objective = objective
    evidence: dict[str, dict[str, str]] = {}
    for field in EVIDENCE_FIELDS:
        evidence[field], _, _ = validate_evidence_reference(
            component[field], root, f"{name}.{field}", used_paths, total_bytes
        )
    return name, {
        "profile": profile,
        "sourceAuthority": source_authority,
        "restoreTarget": restore_target,
        "sourceRegion": source_region,
        "restoreRegion": restore_region,
        "sourceFailureDomain": source_domain,
        "restoreFailureDomain": restore_domain,
        "sourceCutoffAt": format_utc(source_cutoff),
        "restoredThroughAt": format_utc(restored_through),
        "recoveryStartedAt": format_utc(recovery_started),
        "serviceReadyAt": format_utc(service_ready),
        "measuredRpoSeconds": rpo_seconds,
        "rpoObjectiveSeconds": rpo_objective,
        "rpoWithinObjective": rpo_seconds <= rpo_objective,
        "measuredRtoSeconds": rto_seconds,
        "rtoObjectiveSeconds": rto_objective,
        "rtoWithinObjective": rto_seconds <= rto_objective,
        "restoreServedCanary": canary,
        "evidence": evidence,
    }


def recovery_subject_digest(
    drill_id: str,
    candidate: dict[str, Any],
    started_at: str,
    completed_at: str,
    restored_identity: dict[str, Any],
    components: dict[str, dict[str, Any]],
) -> str:
    subject = {
        "drillId": drill_id,
        "candidate": candidate,
        "startedAt": started_at,
        "completedAt": completed_at,
        "restoredReleaseIdentity": restored_identity,
        "components": components,
    }
    encoded = json.dumps(subject, separators=(",", ":"), sort_keys=True).encode("utf-8")
    return "sha256:" + hashlib.sha256(encoded).hexdigest()


def validate_approval(
    value: Any,
    role: str,
    expected_subject_digest: str,
    root: pathlib.Path,
    used_paths: set[pathlib.Path],
    total_bytes: list[int],
) -> tuple[dict[str, Any], dict[str, str]]:
    reference, _, content = validate_evidence_reference(
        value, root, f"approvals.{role}", used_paths, total_bytes
    )
    try:
        document = json.loads(
            content,
            object_pairs_hook=reject_duplicate_json_fields,
        )
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise RecoveryEvidenceError(f"approvals.{role} must contain valid UTF-8 JSON") from error
    approval_document = require_exact(
        document,
        {"approval", "schemaVersion", "verificationBoundary"},
        f"approvals.{role} document",
    )
    if approval_document["schemaVersion"] != APPROVAL_SCHEMA_VERSION:
        raise RecoveryEvidenceError(f"approvals.{role} schemaVersion is unsupported")
    approval = require_exact(
        approval_document["approval"],
        {"approvedAt", "approverId", "decision", "expiresAt", "role", "subjectSha256"},
        f"approvals.{role}.approval",
    )
    if approval["role"] != role:
        raise RecoveryEvidenceError(f"approvals.{role}.approval.role must be {role}")
    decision = approval["decision"]
    if decision not in APPROVAL_DECISIONS:
        raise RecoveryEvidenceError(f"approvals.{role}.approval.decision is not allowed")
    subject_digest = require_digest(
        approval["subjectSha256"], f"approvals.{role}.subjectSha256"
    )
    if subject_digest != expected_subject_digest:
        raise RecoveryEvidenceError(f"approvals.{role} does not bind the exact recovery subject")
    boundary = require_exact(
        approval_document["verificationBoundary"],
        {"contentValidation", "externalVerification"},
        f"approvals.{role}.verificationBoundary",
    )
    if boundary != {
        "contentValidation": "strict-schema-and-subject-validated",
        "externalVerification": "approver-identity-and-signature-required-not-verified",
    }:
        raise RecoveryEvidenceError(f"approvals.{role}.verificationBoundary is not allowed")
    return {
        "role": role,
        "approverId": require_identifier(approval["approverId"], f"approvals.{role}.approverId"),
        "decision": decision,
        "approvedAt": format_utc(parse_utc(approval["approvedAt"], f"approvals.{role}.approvedAt")),
        "expiresAt": format_utc(parse_utc(approval["expiresAt"], f"approvals.{role}.expiresAt")),
        "subjectSha256": expected_subject_digest,
        "evidence": reference,
    }, reference


def validate(
    manifest_path: pathlib.Path,
    evidence_root: pathlib.Path,
    validated_at: str | None,
    expected_candidate_binding_sha256: str | None = None,
    subject_only: bool = False,
    manifest_reference_path: str | None = None,
) -> dict[str, Any]:
    root = evidence_root.resolve()
    if evidence_root.is_symlink() or not root.is_dir():
        raise RecoveryEvidenceError("evidence root must be a regular non-symlink directory")
    try:
        manifest_bytes = read_stable_regular_file(
            manifest_path,
            label="recovery manifest",
            maximum_bytes=MAX_MANIFEST_BYTES,
        )
        scan_for_secret_material(manifest_bytes, "recovery manifest")
        raw = json.loads(
            manifest_bytes,
            object_pairs_hook=reject_duplicate_json_fields,
        )
    except ImmutableEvidenceIOError as error:
        raise RecoveryEvidenceError(str(error)) from error
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise RecoveryEvidenceError("manifest must be valid JSON") from error
    manifest = require_exact(
        raw,
        {
            "approvals",
            "candidate",
            "components",
            "drill",
            "restoredReleaseIdentity",
            "schemaVersion",
        },
        "manifest",
    )
    if manifest["schemaVersion"] != SCHEMA_VERSION:
        raise RecoveryEvidenceError(f"schemaVersion is not {SCHEMA_VERSION}")

    candidate = validate_candidate(manifest["candidate"])
    candidate_binding = "sha256:" + hashlib.sha256(
        json.dumps(candidate, separators=(",", ":"), sort_keys=True).encode("utf-8")
    ).hexdigest()
    if expected_candidate_binding_sha256 is not None:
        expected_binding = require_digest(
            expected_candidate_binding_sha256, "expected candidate binding"
        )
        if candidate_binding != expected_binding:
            raise RecoveryEvidenceError(
                "candidate binding does not match the expected candidate; candidate drift detected"
            )
    restored_identity = validate_restored_release_identity(
        manifest["restoredReleaseIdentity"], candidate
    )
    drill = require_exact(manifest["drill"], {"completedAt", "drillId", "startedAt"}, "drill")
    try:
        drill_id = str(uuid.UUID(str(drill["drillId"])))
    except (ValueError, AttributeError) as error:
        raise RecoveryEvidenceError("drill.drillId must be a UUID") from error
    started = parse_utc(drill["startedAt"], "drill.startedAt")
    completed = parse_utc(drill["completedAt"], "drill.completedAt")
    if completed <= started:
        raise RecoveryEvidenceError("drill.completedAt must be after drill.startedAt")

    raw_components = manifest["components"]
    if not isinstance(raw_components, list) or len(raw_components) != len(REQUIRED_COMPONENTS):
        raise RecoveryEvidenceError(
            "components must contain PostgreSQL, object storage, KMS, and Queue exactly once"
        )
    used_paths: set[pathlib.Path] = set()
    total_bytes = [0]
    components: dict[str, dict[str, Any]] = {}
    for raw_component in raw_components:
        name, component = validate_component(
            raw_component,
            root,
            started,
            completed,
            set(candidate["regions"]),
            used_paths,
            total_bytes,
        )
        if name in components:
            raise RecoveryEvidenceError(f"duplicate component {name}")
        components[name] = component
    if set(components) != REQUIRED_COMPONENTS:
        raise RecoveryEvidenceError(
            "components must contain PostgreSQL, object storage, KMS, and Queue exactly once"
        )
    components = {name: components[name] for name in sorted(components)}
    started_text = format_utc(started)
    completed_text = format_utc(completed)
    subject_digest = recovery_subject_digest(
        drill_id,
        candidate,
        started_text,
        completed_text,
        restored_identity,
        components,
    )

    if subject_only:
        approvals = require_mapping(manifest["approvals"], "approvals")
        if approvals:
            raise RecoveryEvidenceError(
                "subject-only manifest approvals must be empty until the subject is frozen"
            )
        return {
            "schemaVersion": "synara.recovery-drill-approval-subject.v1",
            "drillId": drill_id,
            "candidate": candidate,
            "candidateBindingSha256": candidate_binding,
            "restoredReleaseIdentity": restored_identity,
            "startedAt": started_text,
            "completedAt": completed_text,
            "components": components,
            "recoverySubjectSha256": subject_digest,
            "assessment": "subject-derived-not-recovery-validated",
        }

    raw_approvals = require_exact(manifest["approvals"], APPROVAL_ROLES, "approvals")
    approvals: dict[str, dict[str, Any]] = {}
    approval_references: dict[str, dict[str, str]] = {}
    for role in sorted(APPROVAL_ROLES):
        approvals[role], approval_references[role] = validate_approval(
            raw_approvals[role], role, subject_digest, root, used_paths, total_bytes
        )
    approver_ids = [approval["approverId"] for approval in approvals.values()]
    if len(set(approver_ids)) != len(approver_ids):
        raise RecoveryEvidenceError("recovery approvals require five distinct approvers")

    validation_text = validated_at or format_utc(dt.datetime.now(dt.timezone.utc))
    validation_time = parse_utc(validation_text, "validatedAt")
    if not (completed <= validation_time <= completed + MAX_VALIDATION_DELAY):
        raise RecoveryEvidenceError(
            "validatedAt must be after drill completion and within seven days"
        )
    approvals_approved = True
    for role, approval in approvals.items():
        approved_at = parse_utc(approval["approvedAt"], f"approvals.{role}.approvedAt")
        expires_at = parse_utc(approval["expiresAt"], f"approvals.{role}.expiresAt")
        if not (completed <= approved_at <= validation_time < expires_at):
            raise RecoveryEvidenceError(f"approvals.{role} timestamps are out of order")
        approvals_approved = (
            approvals_approved and approval["decision"] == "approved-for-human-gate-review"
        )

    within_objectives = all(
        component["rpoWithinObjective"] and component["rtoWithinObjective"]
        for component in components.values()
    )
    all_canaries_passed = all(
        component["restoreServedCanary"] for component in components.values()
    )
    environment_eligible = candidate["environmentClass"] in RELEASE_ELIGIBLE_ENVIRONMENTS
    eligible = (
        environment_eligible
        and within_objectives
        and all_canaries_passed
        and approvals_approved
    )
    return {
        "schemaVersion": RECEIPT_SCHEMA_VERSION,
        "drillId": drill_id,
        "candidate": candidate,
        "candidateBindingSha256": candidate_binding,
        "restoredReleaseIdentity": restored_identity,
        "startedAt": started_text,
        "completedAt": completed_text,
        "recoverySubjectSha256": subject_digest,
        "manifest": {
            "path": normalized_manifest_reference(
                manifest_reference_path or manifest_path.name
            ),
            "sha256": "sha256:" + hashlib.sha256(manifest_bytes).hexdigest(),
        },
        "components": components,
        "approvals": approvals,
        "approvalEvidence": approval_references,
        "declaredMeasurementsWithinObjectives": within_objectives,
        "allRestoreCanariesPassed": all_canaries_passed,
        "allRequiredApprovalsApproved": approvals_approved,
        "releaseEligibleEnvironment": environment_eligible,
        "eligibleForHumanGateReview": eligible,
        "verificationBoundary": {
            "approvalContentAndSubjectValidated": True,
            "cryptographicSignaturesVerified": False,
            "realBackupRestoreAndApproverAuthorityVerificationRequired": True,
        },
        "validatedAt": format_utc(validation_time),
        "assessment": ASSESSMENT,
    }


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manifest", required=True)
    parser.add_argument("--evidence-root", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--expected-candidate-binding-sha256", help=argparse.SUPPRESS)
    parser.add_argument("--validated-at", help=argparse.SUPPRESS)
    parser.add_argument(
        "--subject-only",
        action="store_true",
        help="derive the approval subject from a manifest whose approvals object is empty",
    )
    return parser


def main() -> int:
    parser = build_parser()
    args = parser.parse_args()
    try:
        receipt = validate(
            pathlib.Path(args.manifest),
            pathlib.Path(args.evidence_root),
            args.validated_at,
            args.expected_candidate_binding_sha256,
            args.subject_only,
        )
        output = pathlib.Path(args.output).absolute()
        encoded = (json.dumps(receipt, indent=2, sort_keys=True) + "\n").encode("utf-8")
        publish_immutable_with_sha256(output, encoded)
    except (RecoveryEvidenceError, ImmutableEvidenceIOError) as error:
        parser.exit(2, f"recovery evidence validation failed: {error}\n")
    mode = "approval subject" if args.subject_only else "recovery receipt"
    print(f"wrote {mode} {output} and {output.name}.sha256")
    return 0


if __name__ == "__main__":
    sys.exit(main())
