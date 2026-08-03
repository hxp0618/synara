#!/usr/bin/env python3
"""Validate deployed Stage 6 browser-operation evidence without approving the GA gate."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import importlib.util
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


SCHEMA_VERSION = "synara.stage6-operations-browser-exercise.v2"
RECEIPT_SCHEMA_VERSION = "synara.stage6-operations-browser-exercise-validation.v2"
ASSESSMENT = "evidence-validated-not-operations-passed"
COMMIT_RE = re.compile(r"^[0-9a-f]{40}$")
SHA256_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
IDENTIFIER_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:/-]{1,159}$")
ENVIRONMENTS = {"production", "production-like", "staging", "fixture"}
ACCOUNT_ROLES = {
    "authenticated-user",
    "platform-operator",
    "platform-admin",
    "platform-owner",
    "support-engineer",
    "tenant-owner",
    "tenant-admin",
    "security-admin",
    "cost-admin",
    "auditor",
    "tenant-member",
}
AUTHENTICATION_METHODS = {"sso", "password-mfa", "fixture"}
OPERATION_STATUSES = {"passed", "failed", "blocked"}
NEGATIVE_RESULTS = {"denied", "unexpectedly-allowed", "not-run"}
APPROVAL_ROLES = {"operations", "security"}
ARTIFACT_KEYS = {"controlPlane", "web", "admin"}
MAX_MANIFEST_BYTES = 2 * 1024 * 1024
MAX_EVIDENCE_FILE_BYTES = 16 * 1024 * 1024
MAX_TOTAL_EVIDENCE_BYTES = 512 * 1024 * 1024
PROHIBITED_SECRET_PATTERNS = (
    ("private key", re.compile(rb"-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----")),
    ("AWS access key", re.compile(rb"\bAKIA[0-9A-Z]{16}\b")),
    ("bearer credential", re.compile(rb"(?i)authorization\s*[:=]\s*bearer\s+[^\s,;]+")),
    (
        "credential-bearing URL",
        re.compile(rb"(?i)\b(?:https?|postgres(?:ql)?|mysql)://[^\s/:@]+:[^\s/@]+@"),
    ),
)

class OperationsExerciseError(Exception):
    pass


def load_source_validator() -> Any:
    script = pathlib.Path(__file__).with_name("validate_operations_ui_matrix.py")
    spec = importlib.util.spec_from_file_location("stage6_operations_source_validator", script)
    if spec is None or spec.loader is None:
        raise OperationsExerciseError("operations source validator could not be loaded")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


SOURCE_VALIDATOR = load_source_validator()
EXPECTED_NEGATIVE_ACTORS = SOURCE_VALIDATOR.EXPECTED_NEGATIVE_ACTORS


def require_mapping(value: Any, label: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise OperationsExerciseError(f"{label} must be an object")
    return value


def require_exact_fields(value: Any, fields: set[str], label: str) -> dict[str, Any]:
    mapping = require_mapping(value, label)
    if set(mapping) != fields:
        raise OperationsExerciseError(f"{label} fields do not match the declared schema")
    return mapping


def reject_duplicate_json_fields(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise OperationsExerciseError(f"manifest contains duplicate JSON field {key!r}")
        result[key] = value
    return result


def scan_for_secret_material(content: bytes, label: str) -> None:
    for secret_label, pattern in PROHIBITED_SECRET_PATTERNS:
        if pattern.search(content):
            raise OperationsExerciseError(f"{label} contains prohibited {secret_label} material")


def require_identifier(value: Any, label: str) -> str:
    if not isinstance(value, str) or IDENTIFIER_RE.fullmatch(value.strip()) is None:
        raise OperationsExerciseError(f"{label} must be a bounded identifier")
    return value.strip()


def require_bool(value: Any, label: str) -> bool:
    if not isinstance(value, bool):
        raise OperationsExerciseError(f"{label} must be a boolean")
    return value


def parse_utc(value: Any, label: str) -> dt.datetime:
    if not isinstance(value, str) or not value.endswith("Z"):
        raise OperationsExerciseError(f"{label} must be an RFC3339 UTC timestamp ending in Z")
    try:
        parsed = dt.datetime.fromisoformat(value[:-1] + "+00:00")
    except ValueError as error:
        raise OperationsExerciseError(f"{label} must be a valid RFC3339 UTC timestamp") from error
    if parsed.tzinfo != dt.timezone.utc:
        raise OperationsExerciseError(f"{label} must use UTC")
    return parsed


def parse_https_origin(value: Any, label: str) -> str:
    if not isinstance(value, str) or not value.strip():
        raise OperationsExerciseError(f"{label} must be a non-empty HTTPS origin")
    parsed = urllib.parse.urlsplit(value.strip())
    if (
        parsed.scheme != "https"
        or not parsed.hostname
        or parsed.username is not None
        or parsed.password is not None
        or parsed.query
        or parsed.fragment
        or parsed.path not in {"", "/"}
    ):
        raise OperationsExerciseError(f"{label} must be a credential-free HTTPS origin")
    try:
        port = parsed.port
    except ValueError as error:
        raise OperationsExerciseError(f"{label} has an invalid port") from error
    host = parsed.hostname.lower()
    if ":" in host and not host.startswith("["):
        host = f"[{host}]"
    authority = host if port is None or port == 443 else f"{host}:{port}"
    return f"https://{authority}"


def resolve_evidence_file(root: pathlib.Path, raw_path: Any, label: str) -> pathlib.Path:
    if not isinstance(raw_path, str) or not raw_path.strip():
        raise OperationsExerciseError(f"{label}.path must be a non-empty relative path")
    relative = pathlib.Path(raw_path)
    if relative.is_absolute() or any(part in {".", ".."} for part in relative.parts):
        raise OperationsExerciseError(f"{label}.path must be traversal-free and relative")
    candidate = root / relative
    current = candidate
    while current != root:
        if current.is_symlink():
            raise OperationsExerciseError(f"{label}.path must not traverse a symlink")
        current = current.parent
    try:
        resolved = candidate.resolve(strict=True)
        resolved.relative_to(root)
    except (OSError, ValueError) as error:
        raise OperationsExerciseError(f"{label}.path must resolve inside the evidence root") from error
    if not resolved.is_file():
        raise OperationsExerciseError(f"{label}.path must reference a regular file")
    return resolved


def validate_reference(
    value: Any,
    root: pathlib.Path,
    label: str,
    used_paths: set[pathlib.Path],
    total_bytes: list[int],
) -> dict[str, str]:
    reference = require_exact_fields(value, {"path", "sha256"}, label)
    path = resolve_evidence_file(root, reference["path"], label)
    if path in used_paths:
        raise OperationsExerciseError(f"{label}.path duplicates another evidence file")
    used_paths.add(path)
    try:
        content = read_stable_regular_file(
            path,
            label=label,
            maximum_bytes=MAX_EVIDENCE_FILE_BYTES,
        )
    except ImmutableEvidenceIOError as error:
        raise OperationsExerciseError(str(error)) from error
    total_bytes[0] += len(content)
    if total_bytes[0] > MAX_TOTAL_EVIDENCE_BYTES:
        raise OperationsExerciseError("total operations evidence exceeds the bounded size limit")
    scan_for_secret_material(content, label)
    digest = reference["sha256"]
    if not isinstance(digest, str) or SHA256_RE.fullmatch(digest) is None:
        raise OperationsExerciseError(f"{label}.sha256 must be sha256:<64 lowercase hex>")
    actual = "sha256:" + hashlib.sha256(content).hexdigest()
    if digest != actual:
        raise OperationsExerciseError(f"{label}.sha256 does not match the evidence file")
    return {"path": path.relative_to(root).as_posix(), "sha256": actual}


def load_expected_operations(
    repository_root: pathlib.Path,
    matrix_path: pathlib.Path,
) -> tuple[dict[str, dict[str, str]], dict[str, str]]:
    try:
        receipt = SOURCE_VALIDATOR.validate(matrix_path, repository_root)
    except SOURCE_VALIDATOR.OperationsMatrixError as error:
        raise OperationsExerciseError(f"operations source matrix is invalid: {error}") from error
    expected: dict[str, dict[str, str]] = {}
    for operation in receipt["operations"]:
        expected[operation["id"]] = {
            "surface": operation["surface"],
            "actor": operation["actor"],
            "negativeActor": operation["negativeActor"],
            "access": operation["access"],
        }
    if set(expected) != set(EXPECTED_NEGATIVE_ACTORS):
        raise OperationsExerciseError("operations negative-role policy drifted from the source matrix")
    return expected, receipt["matrix"]


def validate_accounts(value: Any, started_at: dt.datetime) -> tuple[list[dict[str, Any]], dict[str, str], bool]:
    if not isinstance(value, list) or len(value) != len(ACCOUNT_ROLES):
        raise OperationsExerciseError("accounts must contain every exercise role exactly once")
    accounts: list[dict[str, Any]] = []
    role_subjects: dict[str, str] = {}
    subjects: set[str] = set()
    production_authentication = True
    for index, raw in enumerate(value):
        account = require_exact_fields(
            raw,
            {"role", "subjectReference", "authenticationMethod", "sessionFreshAt"},
            f"accounts[{index}]",
        )
        role = account["role"]
        if role not in ACCOUNT_ROLES or role in role_subjects:
            raise OperationsExerciseError("accounts must contain every exercise role exactly once")
        subject = require_identifier(account["subjectReference"], f"accounts[{index}].subjectReference")
        if subject in subjects:
            raise OperationsExerciseError("exercise roles must use distinct subject references")
        subjects.add(subject)
        authentication_method = account["authenticationMethod"]
        if authentication_method not in AUTHENTICATION_METHODS:
            raise OperationsExerciseError(f"accounts[{index}].authenticationMethod is not allowed")
        session_fresh_at = parse_utc(account["sessionFreshAt"], f"accounts[{index}].sessionFreshAt")
        if session_fresh_at > started_at:
            raise OperationsExerciseError("account sessionFreshAt cannot be after exercise startedAt")
        production_authentication = production_authentication and authentication_method != "fixture"
        role_subjects[role] = subject
        accounts.append(
            {
                "role": role,
                "subjectReference": subject,
                "authenticationMethod": authentication_method,
                "sessionFreshAt": account["sessionFreshAt"],
            }
        )
    if set(role_subjects) != ACCOUNT_ROLES:
        raise OperationsExerciseError("accounts must contain every exercise role exactly once")
    return sorted(accounts, key=lambda item: item["role"]), role_subjects, production_authentication


def validate_operation(
    value: Any,
    expected: dict[str, dict[str, str]],
    role_subjects: dict[str, str],
    started_at: dt.datetime,
    completed_at: dt.datetime,
    evidence_root: pathlib.Path,
    used_paths: set[pathlib.Path],
    used_request_ids: set[str],
    total_bytes: list[int],
    index: int,
) -> dict[str, Any]:
    label = f"operations[{index}]"
    operation = require_exact_fields(
        value,
        {
            "id",
            "surface",
            "actor",
            "access",
            "status",
            "startedAt",
            "completedAt",
            "requestIds",
            "negativeActor",
            "negativeResult",
            "usedCli",
            "usedDatabaseClient",
            "usedDeveloperTools",
            "positiveEvidence",
            "negativeEvidence",
        },
        label,
    )
    operation_id = operation["id"]
    expected_operation = expected.get(operation_id)
    if expected_operation is None:
        raise OperationsExerciseError(f"{label}.id is not in the source operations matrix")
    for field in ("surface", "actor", "access"):
        if operation[field] != expected_operation[field]:
            raise OperationsExerciseError(f"{operation_id}.{field} does not match the source matrix")
    status = operation["status"]
    if status not in OPERATION_STATUSES:
        raise OperationsExerciseError(f"{operation_id}.status is not allowed")
    operation_started = parse_utc(operation["startedAt"], f"{operation_id}.startedAt")
    operation_completed = parse_utc(operation["completedAt"], f"{operation_id}.completedAt")
    if not (started_at <= operation_started < operation_completed <= completed_at):
        raise OperationsExerciseError(f"{operation_id} timestamps must be ordered inside the exercise window")
    request_ids = operation["requestIds"]
    if not isinstance(request_ids, list) or not request_ids:
        raise OperationsExerciseError(f"{operation_id}.requestIds must be a non-empty UUID list")
    normalized_request_ids: list[str] = []
    for request_id in request_ids:
        try:
            normalized = str(uuid.UUID(request_id))
        except (AttributeError, TypeError, ValueError) as error:
            raise OperationsExerciseError(f"{operation_id}.requestIds must contain canonical UUIDs") from error
        if normalized != request_id or normalized in used_request_ids:
            raise OperationsExerciseError(f"{operation_id}.requestIds must be canonical and globally unique")
        used_request_ids.add(normalized)
        normalized_request_ids.append(normalized)
    negative_actor = operation["negativeActor"]
    if negative_actor != expected_operation["negativeActor"]:
        raise OperationsExerciseError(f"{operation_id}.negativeActor does not match the reviewed denial matrix")
    if negative_actor != "unauthenticated" and negative_actor not in role_subjects:
        raise OperationsExerciseError(f"{operation_id}.negativeActor has no exercise account")
    negative_result = operation["negativeResult"]
    if negative_result not in NEGATIVE_RESULTS:
        raise OperationsExerciseError(f"{operation_id}.negativeResult is not allowed")
    used_cli = require_bool(operation["usedCli"], f"{operation_id}.usedCli")
    used_database = require_bool(
        operation["usedDatabaseClient"], f"{operation_id}.usedDatabaseClient"
    )
    used_developer_tools = require_bool(
        operation["usedDeveloperTools"], f"{operation_id}.usedDeveloperTools"
    )
    return {
        "id": operation_id,
        **expected_operation,
        "actorSubjectReference": role_subjects[expected_operation["actor"]],
        "status": status,
        "startedAt": operation["startedAt"],
        "completedAt": operation["completedAt"],
        "requestIds": normalized_request_ids,
        "negativeActor": negative_actor,
        "negativeSubjectReference": (
            None if negative_actor == "unauthenticated" else role_subjects[negative_actor]
        ),
        "negativeResult": negative_result,
        "usedCli": used_cli,
        "usedDatabaseClient": used_database,
        "usedDeveloperTools": used_developer_tools,
        "positiveEvidence": validate_reference(
            operation["positiveEvidence"], evidence_root, f"{operation_id}.positiveEvidence", used_paths, total_bytes
        ),
        "negativeEvidence": validate_reference(
            operation["negativeEvidence"], evidence_root, f"{operation_id}.negativeEvidence", used_paths, total_bytes
        ),
    }


def validate_support_access(
    value: Any,
    role_subjects: dict[str, str],
    evidence_root: pathlib.Path,
    used_paths: set[pathlib.Path],
    total_bytes: list[int],
) -> dict[str, Any]:
    support = require_exact_fields(
        value,
        {
            "requesterSubjectReference",
            "approverSubjectReference",
            "supportSubjectReference",
            "requesterAndApproverDistinct",
            "readOnlyWriteDenied",
            "revocationAuditAction",
            "expiryAuditAction",
            "revocationAuditVisible",
            "expiryAuditVisible",
            "tenantAuditVisible",
            "evidence",
        },
        "supportAccess",
    )
    requester = require_identifier(
        support["requesterSubjectReference"], "supportAccess.requesterSubjectReference"
    )
    approver = require_identifier(
        support["approverSubjectReference"], "supportAccess.approverSubjectReference"
    )
    support_subject = require_identifier(
        support["supportSubjectReference"], "supportAccess.supportSubjectReference"
    )
    if requester != role_subjects["platform-operator"]:
        raise OperationsExerciseError("supportAccess requester must be the platform-operator account")
    if approver != role_subjects["platform-admin"]:
        raise OperationsExerciseError("supportAccess approver must be the platform-admin account")
    if support_subject != role_subjects["support-engineer"]:
        raise OperationsExerciseError("supportAccess support subject must be the support-engineer account")
    declared_distinct = require_bool(
        support["requesterAndApproverDistinct"], "supportAccess.requesterAndApproverDistinct"
    )
    read_only_denied = require_bool(
        support["readOnlyWriteDenied"], "supportAccess.readOnlyWriteDenied"
    )
    revocation_audit_action = require_identifier(
        support["revocationAuditAction"], "supportAccess.revocationAuditAction"
    )
    expiry_audit_action = require_identifier(
        support["expiryAuditAction"], "supportAccess.expiryAuditAction"
    )
    if revocation_audit_action != "support.access_revoked":
        raise OperationsExerciseError(
            "supportAccess.revocationAuditAction must be support.access_revoked"
        )
    if expiry_audit_action != "support.access_expired":
        raise OperationsExerciseError(
            "supportAccess.expiryAuditAction must be support.access_expired"
        )
    revocation_audit_visible = require_bool(
        support["revocationAuditVisible"], "supportAccess.revocationAuditVisible"
    )
    expiry_audit_visible = require_bool(
        support["expiryAuditVisible"], "supportAccess.expiryAuditVisible"
    )
    tenant_audit_visible = require_bool(
        support["tenantAuditVisible"], "supportAccess.tenantAuditVisible"
    )
    return {
        "requesterSubjectReference": requester,
        "approverSubjectReference": approver,
        "supportSubjectReference": support_subject,
        "fourEyesProved": declared_distinct and requester != approver,
        "readOnlyWriteDenied": read_only_denied,
        "revocationAuditAction": revocation_audit_action,
        "expiryAuditAction": expiry_audit_action,
        "revocationAuditVisible": revocation_audit_visible,
        "expiryAuditVisible": expiry_audit_visible,
        "tenantAuditVisible": tenant_audit_visible,
        "evidence": validate_reference(
            support["evidence"], evidence_root, "supportAccess.evidence", used_paths, total_bytes
        ),
    }


def validate_approvals(
    value: Any,
    completed_at: dt.datetime,
    evidence_root: pathlib.Path,
    used_paths: set[pathlib.Path],
    total_bytes: list[int],
) -> tuple[list[dict[str, Any]], bool]:
    if not isinstance(value, list) or len(value) != len(APPROVAL_ROLES):
        raise OperationsExerciseError("approvals must contain Operations and Security exactly once")
    approvals: list[dict[str, Any]] = []
    roles: set[str] = set()
    subjects: set[str] = set()
    all_approved = True
    for index, raw in enumerate(value):
        approval = require_exact_fields(
            raw,
            {"role", "subjectReference", "approved", "approvedAt", "evidence"},
            f"approvals[{index}]",
        )
        role = approval["role"]
        if role not in APPROVAL_ROLES or role in roles:
            raise OperationsExerciseError("approvals must contain Operations and Security exactly once")
        roles.add(role)
        subject = require_identifier(
            approval["subjectReference"], f"approvals[{index}].subjectReference"
        )
        if subject in subjects:
            raise OperationsExerciseError("Operations and Security approvers must be distinct")
        subjects.add(subject)
        approved = require_bool(approval["approved"], f"approvals[{index}].approved")
        approved_at = parse_utc(approval["approvedAt"], f"approvals[{index}].approvedAt")
        if approved_at < completed_at:
            raise OperationsExerciseError("approval cannot predate exercise completion")
        all_approved = all_approved and approved
        approvals.append(
            {
                "role": role,
                "subjectReference": subject,
                "approved": approved,
                "approvedAt": approval["approvedAt"],
                "evidence": validate_reference(
                    approval["evidence"], evidence_root, f"approvals[{index}].evidence", used_paths, total_bytes
                ),
            }
        )
    return sorted(approvals, key=lambda item: item["role"]), all_approved


def validate(
    manifest_path: pathlib.Path,
    evidence_root: pathlib.Path,
    repository_root: pathlib.Path,
    matrix_path: pathlib.Path,
    validated_at: str | None,
) -> dict[str, Any]:
    root = evidence_root.resolve()
    if evidence_root.is_symlink() or not root.is_dir():
        raise OperationsExerciseError("evidenceRoot must be a regular non-symlink directory")
    expected_operations, source_matrix = load_expected_operations(repository_root, matrix_path)
    try:
        manifest_bytes = read_stable_regular_file(
            manifest_path,
            label="operations manifest",
            maximum_bytes=MAX_MANIFEST_BYTES,
        )
        scan_for_secret_material(manifest_bytes, "operations manifest")
        manifest = json.loads(
            manifest_bytes,
            object_pairs_hook=reject_duplicate_json_fields,
        )
    except (
        OSError,
        UnicodeDecodeError,
        json.JSONDecodeError,
        ImmutableEvidenceIOError,
    ) as error:
        raise OperationsExerciseError("manifest must be readable UTF-8 JSON") from error
    manifest = require_exact_fields(
        manifest,
        {
            "schemaVersion",
            "candidate",
            "matrixSha256",
            "startedAt",
            "completedAt",
            "accounts",
            "operations",
            "supportAccess",
            "approvals",
        },
        "manifest",
    )
    if manifest["schemaVersion"] != SCHEMA_VERSION:
        raise OperationsExerciseError(f"schemaVersion must be {SCHEMA_VERSION}")
    if manifest["matrixSha256"] != source_matrix["sha256"]:
        raise OperationsExerciseError("matrixSha256 does not match the validated source matrix")
    started_at = parse_utc(manifest["startedAt"], "startedAt")
    completed_at = parse_utc(manifest["completedAt"], "completedAt")
    if completed_at <= started_at:
        raise OperationsExerciseError("completedAt must be after startedAt")

    candidate = require_exact_fields(
        manifest["candidate"],
        {
            "candidateId",
            "sourceCommit",
            "environment",
            "environmentId",
            "artifacts",
            "webBaseUrl",
            "adminBaseUrl",
        },
        "candidate",
    )
    candidate_id = require_identifier(candidate["candidateId"], "candidate.candidateId")
    source_commit = candidate["sourceCommit"]
    if not isinstance(source_commit, str) or COMMIT_RE.fullmatch(source_commit) is None:
        raise OperationsExerciseError("candidate.sourceCommit must be a full lowercase Git SHA")
    environment = candidate["environment"]
    if environment not in ENVIRONMENTS:
        raise OperationsExerciseError("candidate.environment is not allowed")
    environment_id = require_identifier(candidate["environmentId"], "candidate.environmentId")
    artifacts = require_exact_fields(candidate["artifacts"], ARTIFACT_KEYS, "candidate.artifacts")
    for key, digest in artifacts.items():
        if not isinstance(digest, str) or SHA256_RE.fullmatch(digest) is None:
            raise OperationsExerciseError(f"candidate.artifacts.{key} must be sha256:<64 lowercase hex>")
    web_base_url = parse_https_origin(candidate["webBaseUrl"], "candidate.webBaseUrl")
    admin_base_url = parse_https_origin(candidate["adminBaseUrl"], "candidate.adminBaseUrl")
    if web_base_url == admin_base_url:
        raise OperationsExerciseError("customer Web and Platform Admin must use distinct origins")

    accounts, role_subjects, production_authentication = validate_accounts(
        manifest["accounts"], started_at
    )
    raw_operations = manifest["operations"]
    if not isinstance(raw_operations, list) or len(raw_operations) != len(expected_operations):
        raise OperationsExerciseError("operations must contain every source matrix row exactly once")
    used_paths: set[pathlib.Path] = set()
    used_request_ids: set[str] = set()
    total_bytes = [0]
    operations: list[dict[str, Any]] = []
    seen_operations: set[str] = set()
    for index, raw_operation in enumerate(raw_operations):
        operation = validate_operation(
            raw_operation,
            expected_operations,
            role_subjects,
            started_at,
            completed_at,
            root,
            used_paths,
            used_request_ids,
            total_bytes,
            index,
        )
        if operation["id"] in seen_operations:
            raise OperationsExerciseError(f"duplicate operation {operation['id']}")
        seen_operations.add(operation["id"])
        operations.append(operation)
    if seen_operations != set(expected_operations):
        raise OperationsExerciseError("operations must contain every source matrix row exactly once")

    support_access = validate_support_access(
        manifest["supportAccess"], role_subjects, root, used_paths, total_bytes
    )
    approvals, approvals_complete = validate_approvals(
        manifest["approvals"], completed_at, root, used_paths, total_bytes
    )
    status_counts = {status: 0 for status in sorted(OPERATION_STATUSES)}
    negative_counts = {status: 0 for status in sorted(NEGATIVE_RESULTS)}
    fallback_counts = {"cli": 0, "databaseClient": 0, "developerTools": 0}
    for operation in operations:
        status_counts[operation["status"]] += 1
        negative_counts[operation["negativeResult"]] += 1
        fallback_counts["cli"] += int(operation["usedCli"])
        fallback_counts["databaseClient"] += int(operation["usedDatabaseClient"])
        fallback_counts["developerTools"] += int(operation["usedDeveloperTools"])
    support_complete = all(
        (
            support_access["fourEyesProved"],
            support_access["readOnlyWriteDenied"],
            support_access["revocationAuditVisible"],
            support_access["expiryAuditVisible"],
            support_access["tenantAuditVisible"],
        )
    )
    environment_eligible = environment in {"production", "production-like"}
    eligible = all(
        (
            environment_eligible,
            production_authentication,
            status_counts["passed"] == len(expected_operations),
            negative_counts["denied"] == len(expected_operations),
            sum(fallback_counts.values()) == 0,
            support_complete,
            approvals_complete,
        )
    )
    timestamp = validated_at or dt.datetime.now(dt.timezone.utc).isoformat().replace("+00:00", "Z")
    parse_utc(timestamp, "validatedAt")
    return {
        "schemaVersion": RECEIPT_SCHEMA_VERSION,
        "candidate": {
            "candidateId": candidate_id,
            "sourceCommit": source_commit,
            "environment": environment,
            "environmentId": environment_id,
            "artifacts": dict(sorted(artifacts.items())),
            "webBaseUrl": web_base_url,
            "adminBaseUrl": admin_base_url,
        },
        "matrix": source_matrix,
        "window": {"startedAt": manifest["startedAt"], "completedAt": manifest["completedAt"]},
        "accounts": accounts,
        "operations": sorted(operations, key=lambda item: item["id"]),
        "operationCounts": status_counts,
        "negativeAuthorizationCounts": negative_counts,
        "fallbackCounts": fallback_counts,
        "supportAccess": support_access,
        "approvals": approvals,
        "evidenceFileCount": len(used_paths),
        "environmentEligible": environment_eligible,
        "productionAuthenticationDeclared": production_authentication,
        "supportLifecycleComplete": support_complete,
        "approvalsComplete": approvals_complete,
        "eligibleForHumanGateReview": eligible,
        "assessment": ASSESSMENT,
        "validatedAt": timestamp,
    }


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manifest", required=True)
    parser.add_argument("--evidence-root", required=True)
    parser.add_argument("--repository-root", default=".")
    parser.add_argument(
        "--matrix", default="docs/release-matrices/stage-6-operations-ui-v1.json"
    )
    parser.add_argument("--output", required=True)
    parser.add_argument("--validated-at", help=argparse.SUPPRESS)
    return parser


def main() -> int:
    parser = build_parser()
    args = parser.parse_args()
    repository_root = pathlib.Path(args.repository_root).resolve()
    matrix_path = pathlib.Path(args.matrix)
    if not matrix_path.is_absolute():
        matrix_path = repository_root / matrix_path
    try:
        receipt = validate(
            pathlib.Path(args.manifest).resolve(),
            pathlib.Path(args.evidence_root),
            repository_root,
            matrix_path.resolve(),
            args.validated_at,
        )
        output = pathlib.Path(args.output).absolute()
        encoded = (json.dumps(receipt, indent=2, sort_keys=True) + "\n").encode("utf-8")
        publish_immutable_with_sha256(output, encoded)
    except (OperationsExerciseError, ImmutableEvidenceIOError) as error:
        parser.exit(2, f"operations exercise evidence validation failed: {error}\n")
    print(
        f"wrote {output}; eligibleForHumanGateReview={str(receipt['eligibleForHumanGateReview']).lower()}"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
