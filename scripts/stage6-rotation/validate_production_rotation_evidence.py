#!/usr/bin/env python3
"""Validate candidate-bound production rotation evidence without declaring the GA control passed."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import pathlib
import re
import sys
from collections import Counter
from typing import Any

SCRIPTS_ROOT = pathlib.Path(__file__).resolve().parents[1]
if str(SCRIPTS_ROOT) not in sys.path:
    sys.path.insert(0, str(SCRIPTS_ROOT))

from stage6_common.immutable_evidence_io import (  # noqa: E402
    ImmutableEvidenceIOError,
    publish_immutable_with_sha256,
    read_stable_regular_file,
)


SCHEMA_VERSION = "synara.stage6-production-rotation-evidence.v1"
RECEIPT_SCHEMA_VERSION = "synara.stage6-production-rotation-evidence-receipt.v1"
ASSESSMENT = "evidence-validated-not-production-rotation-passed"

REQUIRED_CONTROLS = {
    "artifact-storage-credential",
    "billing-provider-credential",
    "credential-envelope-kek",
    "postgresql-credential",
    "public-domain-dns",
    "public-tls-certificate",
    "runtime-secret-key",
    "telemetry-relay-identity",
    "worker-registration-token",
}
APPROVAL_ROLES = {"operations", "release", "security"}
STATUS_VALUES = {"passed", "failed", "not-assessable"}
APPROVAL_DECISIONS = {"approved-for-human-gate-review", "reviewed-not-approved"}
RELEASE_ELIGIBLE_ENVIRONMENTS = {"production", "production-like"}
EVIDENCE_FIELDS = {
    "changeRecord",
    "newPathProbe",
    "oldPathFinalState",
    "rollbackResult",
    "rolloutStatus",
}

COMMIT_RE = re.compile(r"^[0-9a-f]{40}$")
SHA256_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
IDENTIFIER_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:/@+-]{1,199}$")
MIGRATION_RE = re.compile(r"^[0-9]{6}_[a-z0-9_]+\.sql$")
PROHIBITED_SECRET_PATTERNS = (
    ("private key", re.compile(rb"-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----")),
    ("AWS access key", re.compile(rb"\bAKIA[0-9A-Z]{16}\b")),
    ("Stripe live key", re.compile(rb"\bsk_live_[A-Za-z0-9]{16,}\b")),
    ("Stripe webhook secret", re.compile(rb"\bwhsec_[A-Za-z0-9]{16,}\b")),
    ("bearer credential", re.compile(rb"(?i)authorization\s*[:=]\s*bearer\s+[^\s,;]+")),
    (
        "credential-bearing URL",
        re.compile(rb"(?i)\b(?:https?|postgres(?:ql)?|mysql)://[^\s/:@]+:[^\s/@]+@"),
    ),
)

MAX_MANIFEST_BYTES = 2 * 1024 * 1024
MAX_EVIDENCE_FILE_BYTES = 16 * 1024 * 1024
MAX_TOTAL_EVIDENCE_BYTES = 256 * 1024 * 1024
MAX_VALIDATION_DELAY = dt.timedelta(days=7)


class RotationEvidenceError(Exception):
    pass


def require_exact(value: Any, fields: set[str], label: str) -> dict[str, Any]:
    if not isinstance(value, dict) or set(value) != fields:
        raise RotationEvidenceError(f"{label} fields do not match the v1 schema")
    return value


def require_identifier(value: Any, label: str) -> str:
    if not isinstance(value, str) or IDENTIFIER_RE.fullmatch(value.strip()) is None:
        raise RotationEvidenceError(f"{label} must be a bounded identifier")
    return value.strip()


def require_digest(value: Any, label: str) -> str:
    if not isinstance(value, str) or SHA256_RE.fullmatch(value) is None:
        raise RotationEvidenceError(f"{label} must be sha256:<64 lowercase hex>")
    return value


def require_bool(value: Any, label: str) -> bool:
    if not isinstance(value, bool):
        raise RotationEvidenceError(f"{label} must be boolean")
    return value


def require_nonnegative_integer(value: Any, label: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value < 0:
        raise RotationEvidenceError(f"{label} must be a non-negative integer")
    return value


def parse_utc(value: Any, label: str) -> dt.datetime:
    if not isinstance(value, str) or not value.endswith("Z"):
        raise RotationEvidenceError(f"{label} must be an RFC3339 UTC timestamp ending in Z")
    try:
        parsed = dt.datetime.fromisoformat(value[:-1] + "+00:00")
    except ValueError as error:
        raise RotationEvidenceError(f"{label} must be a valid RFC3339 UTC timestamp") from error
    if parsed.tzinfo != dt.timezone.utc:
        raise RotationEvidenceError(f"{label} must use UTC")
    return parsed


def scan_for_secret_material(content: bytes, label: str) -> None:
    for secret_label, pattern in PROHIBITED_SECRET_PATTERNS:
        if pattern.search(content):
            raise RotationEvidenceError(f"{label} contains prohibited {secret_label} material")


def reject_duplicate_json_fields(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise RotationEvidenceError(f"manifest contains duplicate JSON field {key!r}")
        result[key] = value
    return result


def resolve_evidence_path(root: pathlib.Path, raw_path: Any, label: str) -> pathlib.Path:
    if not isinstance(raw_path, str) or not raw_path.strip() or "\\" in raw_path or "\x00" in raw_path:
        raise RotationEvidenceError(f"{label}.path must be traversal-free and relative")
    parts = raw_path.split("/")
    if raw_path.startswith("/") or any(part in {"", ".", ".."} for part in parts):
        raise RotationEvidenceError(f"{label}.path must be traversal-free and relative")
    candidate = root.joinpath(*parts)
    current = candidate
    while current != root:
        if current.is_symlink():
            raise RotationEvidenceError(f"{label}.path must not traverse a symlink")
        current = current.parent
    try:
        resolved = candidate.resolve(strict=True)
        resolved.relative_to(root)
    except (OSError, ValueError) as error:
        raise RotationEvidenceError(f"{label}.path must resolve inside evidence root") from error
    return resolved


def validate_reference(
    value: Any,
    root: pathlib.Path,
    label: str,
    used_paths: set[str],
    total_bytes: list[int],
) -> dict[str, str]:
    reference = require_exact(value, {"path", "sha256"}, label)
    resolved = resolve_evidence_path(root, reference["path"], label)
    try:
        content = read_stable_regular_file(
            resolved,
            label=label,
            maximum_bytes=MAX_EVIDENCE_FILE_BYTES,
        )
    except ImmutableEvidenceIOError as error:
        raise RotationEvidenceError(str(error)) from error
    normalized = resolved.relative_to(root).as_posix()
    if normalized in used_paths:
        raise RotationEvidenceError(f"evidence file is reused: {normalized}")
    expected_digest = require_digest(reference["sha256"], f"{label}.sha256")
    actual_digest = "sha256:" + hashlib.sha256(content).hexdigest()
    if expected_digest != actual_digest:
        raise RotationEvidenceError(f"{label}.sha256 does not match the evidence file")
    scan_for_secret_material(content, label)
    total_bytes[0] += len(content)
    if total_bytes[0] > MAX_TOTAL_EVIDENCE_BYTES:
        raise RotationEvidenceError("total evidence size exceeds the bounded limit")
    used_paths.add(normalized)
    return {"path": normalized, "sha256": actual_digest}


def latest_migration(repository_root: pathlib.Path) -> dict[str, str]:
    migration_root = repository_root / "services/control-plane/migrations"
    migrations = sorted(
        path
        for path in migration_root.glob("[0-9][0-9][0-9][0-9][0-9][0-9]_*.sql")
        if path.is_file() and not path.is_symlink()
    )
    if not migrations:
        raise RotationEvidenceError("Control Plane migrations are missing")
    tail = migrations[-1]
    return {
        "name": tail.name,
        "sha256": "sha256:" + hashlib.sha256(tail.read_bytes()).hexdigest(),
    }


def validate_candidate(value: Any, repository_root: pathlib.Path) -> dict[str, Any]:
    candidate = require_exact(
        value,
        {
            "adminDigest",
            "candidateId",
            "controlPlaneDigest",
            "environment",
            "environmentId",
            "migrationTail",
            "regions",
            "sourceCommit",
            "webDigest",
        },
        "candidate",
    )
    source_commit = candidate["sourceCommit"]
    if not isinstance(source_commit, str) or COMMIT_RE.fullmatch(source_commit) is None:
        raise RotationEvidenceError("candidate.sourceCommit must be 40 lowercase hexadecimal characters")
    environment = candidate["environment"]
    if environment not in RELEASE_ELIGIBLE_ENVIRONMENTS:
        raise RotationEvidenceError("candidate.environment must be production or production-like")
    regions = candidate["regions"]
    if not isinstance(regions, list) or not regions:
        raise RotationEvidenceError("candidate.regions must be a non-empty sorted list")
    normalized_regions = [require_identifier(region, "candidate.regions[]") for region in regions]
    if normalized_regions != sorted(set(normalized_regions)):
        raise RotationEvidenceError("candidate.regions must be sorted and unique")
    migration = require_exact(candidate["migrationTail"], {"name", "sha256"}, "candidate.migrationTail")
    if not isinstance(migration["name"], str) or MIGRATION_RE.fullmatch(migration["name"]) is None:
        raise RotationEvidenceError("candidate.migrationTail.name is invalid")
    normalized_migration = {
        "name": migration["name"],
        "sha256": require_digest(migration["sha256"], "candidate.migrationTail.sha256"),
    }
    if normalized_migration != latest_migration(repository_root):
        raise RotationEvidenceError("candidate.migrationTail does not match the repository")
    return {
        "candidateId": require_identifier(candidate["candidateId"], "candidate.candidateId"),
        "sourceCommit": source_commit,
        "controlPlaneDigest": require_digest(candidate["controlPlaneDigest"], "candidate.controlPlaneDigest"),
        "webDigest": require_digest(candidate["webDigest"], "candidate.webDigest"),
        "adminDigest": require_digest(candidate["adminDigest"], "candidate.adminDigest"),
        "environment": environment,
        "environmentId": require_identifier(candidate["environmentId"], "candidate.environmentId"),
        "regions": normalized_regions,
        "migrationTail": normalized_migration,
    }


def validate(
    manifest_path: pathlib.Path,
    evidence_root: pathlib.Path,
    repository_root: pathlib.Path,
    validated_at_raw: str | None,
    manifest_name: str | None = None,
) -> dict[str, Any]:
    root = evidence_root.resolve(strict=True)
    if evidence_root.is_symlink() or not root.is_dir():
        raise RotationEvidenceError("evidence root must be a non-symlink directory")
    try:
        manifest_bytes = read_stable_regular_file(
            manifest_path,
            label="manifest",
            maximum_bytes=MAX_MANIFEST_BYTES,
        )
    except ImmutableEvidenceIOError as error:
        raise RotationEvidenceError(str(error)) from error
    scan_for_secret_material(manifest_bytes, "manifest")
    try:
        manifest = json.loads(manifest_bytes, object_pairs_hook=reject_duplicate_json_fields)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise RotationEvidenceError("manifest must be valid UTF-8 JSON") from error
    manifest = require_exact(
        manifest,
        {"approvals", "candidate", "completedAt", "controls", "schemaVersion", "secretScan", "startedAt"},
        "manifest",
    )
    if manifest["schemaVersion"] != SCHEMA_VERSION:
        raise RotationEvidenceError(f"manifest.schemaVersion must be {SCHEMA_VERSION}")
    candidate = validate_candidate(manifest["candidate"], repository_root.resolve())
    started_at = parse_utc(manifest["startedAt"], "startedAt")
    completed_at = parse_utc(manifest["completedAt"], "completedAt")
    if completed_at <= started_at:
        raise RotationEvidenceError("completedAt must be after startedAt")
    validated_at = parse_utc(
        validated_at_raw or dt.datetime.now(dt.timezone.utc).isoformat().replace("+00:00", "Z"),
        "validatedAt",
    )
    if validated_at < completed_at or validated_at - completed_at > MAX_VALIDATION_DELAY:
        raise RotationEvidenceError("validatedAt must be within seven days after the exercise completed")

    used_paths: set[str] = set()
    total_bytes = [0]
    controls_raw = manifest["controls"]
    if not isinstance(controls_raw, list):
        raise RotationEvidenceError("controls must be an array")
    normalized_controls: list[dict[str, Any]] = []
    control_ids: list[str] = []
    for index, raw_control in enumerate(controls_raw):
        label = f"controls[{index}]"
        control = require_exact(
            raw_control,
            {
                "allConsumersUpdated",
                "approverSubject",
                "changeReference",
                "completedAt",
                "evidence",
                "id",
                "newAuthorityId",
                "newPathPassed",
                "oldPathFinalStateVerified",
                "ownerSubject",
                "previousAuthorityId",
                "rollbackVerified",
                "startedAt",
                "status",
                "unresolvedRiskCount",
            },
            label,
        )
        control_id = require_identifier(control["id"], f"{label}.id")
        control_ids.append(control_id)
        status = control["status"]
        if status not in STATUS_VALUES:
            raise RotationEvidenceError(f"{label}.status is invalid")
        owner = require_identifier(control["ownerSubject"], f"{label}.ownerSubject")
        approver = require_identifier(control["approverSubject"], f"{label}.approverSubject")
        if owner == approver:
            raise RotationEvidenceError(f"{label} owner and approver must be distinct")
        control_started = parse_utc(control["startedAt"], f"{label}.startedAt")
        control_completed = parse_utc(control["completedAt"], f"{label}.completedAt")
        if not (started_at <= control_started < control_completed <= completed_at):
            raise RotationEvidenceError(f"{label} timestamps must be inside the exercise window")
        previous_authority = require_identifier(control["previousAuthorityId"], f"{label}.previousAuthorityId")
        new_authority = require_identifier(control["newAuthorityId"], f"{label}.newAuthorityId")
        if previous_authority == new_authority:
            raise RotationEvidenceError(f"{label} previous and new authority IDs must differ")
        evidence = require_exact(control["evidence"], EVIDENCE_FIELDS, f"{label}.evidence")
        normalized_evidence = {
            field: validate_reference(
                evidence[field], root, f"{label}.evidence.{field}", used_paths, total_bytes
            )
            for field in sorted(EVIDENCE_FIELDS)
        }
        normalized_controls.append(
            {
                "id": control_id,
                "status": status,
                "changeReference": require_identifier(
                    control["changeReference"], f"{label}.changeReference"
                ),
                "ownerSubject": owner,
                "approverSubject": approver,
                "previousAuthorityId": previous_authority,
                "newAuthorityId": new_authority,
                "startedAt": control["startedAt"],
                "completedAt": control["completedAt"],
                "allConsumersUpdated": require_bool(
                    control["allConsumersUpdated"], f"{label}.allConsumersUpdated"
                ),
                "newPathPassed": require_bool(control["newPathPassed"], f"{label}.newPathPassed"),
                "oldPathFinalStateVerified": require_bool(
                    control["oldPathFinalStateVerified"], f"{label}.oldPathFinalStateVerified"
                ),
                "rollbackVerified": require_bool(
                    control["rollbackVerified"], f"{label}.rollbackVerified"
                ),
                "unresolvedRiskCount": require_nonnegative_integer(
                    control["unresolvedRiskCount"], f"{label}.unresolvedRiskCount"
                ),
                "evidence": normalized_evidence,
            }
        )
    if set(control_ids) != REQUIRED_CONTROLS or len(control_ids) != len(REQUIRED_CONTROLS):
        raise RotationEvidenceError("controls must contain every required rotation control exactly once")

    secret_scan = require_exact(
        manifest["secretScan"], {"evidence", "findingCount", "scannerVersion"}, "secretScan"
    )
    normalized_secret_scan = {
        "scannerVersion": require_identifier(secret_scan["scannerVersion"], "secretScan.scannerVersion"),
        "findingCount": require_nonnegative_integer(secret_scan["findingCount"], "secretScan.findingCount"),
        "evidence": validate_reference(
            secret_scan["evidence"], root, "secretScan.evidence", used_paths, total_bytes
        ),
    }

    approvals_raw = manifest["approvals"]
    if not isinstance(approvals_raw, list):
        raise RotationEvidenceError("approvals must be an array")
    approvals: list[dict[str, Any]] = []
    roles: list[str] = []
    subjects: list[str] = []
    for index, raw_approval in enumerate(approvals_raw):
        label = f"approvals[{index}]"
        approval = require_exact(
            raw_approval, {"decidedAt", "decision", "evidence", "role", "subjectReference"}, label
        )
        role = require_identifier(approval["role"], f"{label}.role")
        if role not in APPROVAL_ROLES:
            raise RotationEvidenceError(f"{label}.role is invalid")
        decision = approval["decision"]
        if decision not in APPROVAL_DECISIONS:
            raise RotationEvidenceError(f"{label}.decision is invalid")
        decided_at = parse_utc(approval["decidedAt"], f"{label}.decidedAt")
        if decided_at < completed_at or decided_at > validated_at:
            raise RotationEvidenceError(f"{label}.decidedAt must be after completion and no later than validation")
        subject = require_identifier(approval["subjectReference"], f"{label}.subjectReference")
        roles.append(role)
        subjects.append(subject)
        approvals.append(
            {
                "role": role,
                "subjectReference": subject,
                "decision": decision,
                "decidedAt": approval["decidedAt"],
                "evidence": validate_reference(
                    approval["evidence"], root, f"{label}.evidence", used_paths, total_bytes
                ),
            }
        )
    if set(roles) != APPROVAL_ROLES or len(roles) != len(APPROVAL_ROLES):
        raise RotationEvidenceError("approvals must contain every required role exactly once")
    if len(set(subjects)) != len(subjects):
        raise RotationEvidenceError("approval subjects must be distinct")

    controls_ready = all(
        control["status"] == "passed"
        and control["allConsumersUpdated"]
        and control["newPathPassed"]
        and control["oldPathFinalStateVerified"]
        and control["rollbackVerified"]
        and control["unresolvedRiskCount"] == 0
        for control in normalized_controls
    )
    approvals_ready = all(
        approval["decision"] == "approved-for-human-gate-review" for approval in approvals
    )
    evidence_projection = [
        f"{path}\0{digest}"
        for path, digest in sorted(
            (
                reference["path"],
                reference["sha256"],
            )
            for control in normalized_controls
            for reference in control["evidence"].values()
        )
    ]
    evidence_projection.extend(
        f"{approval['evidence']['path']}\0{approval['evidence']['sha256']}" for approval in approvals
    )
    evidence_projection.append(
        f"{normalized_secret_scan['evidence']['path']}\0{normalized_secret_scan['evidence']['sha256']}"
    )
    evidence_set_sha256 = "sha256:" + hashlib.sha256(
        "\n".join(sorted(evidence_projection)).encode("utf-8")
    ).hexdigest()

    return {
        "schemaVersion": RECEIPT_SCHEMA_VERSION,
        "assessment": ASSESSMENT,
        "validatedAt": validated_at.isoformat().replace("+00:00", "Z"),
        "manifest": {
            "path": manifest_name or manifest_path.name,
            "sha256": "sha256:" + hashlib.sha256(manifest_bytes).hexdigest(),
        },
        "candidate": candidate,
        "exerciseWindow": {"startedAt": manifest["startedAt"], "completedAt": manifest["completedAt"]},
        "controlCounts": dict(sorted(Counter(control["status"] for control in normalized_controls).items())),
        "requiredControlIds": sorted(REQUIRED_CONTROLS),
        "secretScan": normalized_secret_scan,
        "approvals": sorted(approvals, key=lambda approval: approval["role"]),
        "evidenceFileCount": len(used_paths),
        "evidenceSetSha256": evidence_set_sha256,
        "externalEvidenceAuthorityVerified": False,
        "approverAuthorityVerified": False,
        "cryptographicSignaturesVerified": False,
        "eligibleForHumanGateReview": (
            controls_ready and normalized_secret_scan["findingCount"] == 0 and approvals_ready
        ),
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manifest", required=True)
    parser.add_argument("--evidence-root", required=True)
    parser.add_argument("--repository-root", default=".")
    parser.add_argument("--validated-at")
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    try:
        receipt = validate(
            pathlib.Path(args.manifest),
            pathlib.Path(args.evidence_root),
            pathlib.Path(args.repository_root),
            args.validated_at,
        )
        encoded = (json.dumps(receipt, indent=2, sort_keys=True) + "\n").encode("utf-8")
        output = pathlib.Path(args.output)
        publish_immutable_with_sha256(output, encoded)
    except (RotationEvidenceError, ImmutableEvidenceIOError, OSError) as error:
        parser.error(str(error))
    return 0


if __name__ == "__main__":
    sys.exit(main())
