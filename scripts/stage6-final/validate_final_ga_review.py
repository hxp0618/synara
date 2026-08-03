#!/usr/bin/env python3
"""Validate one Stage 6 final GA review archive without asserting external authority."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import pathlib
import re
import sys
from typing import Any

SCRIPTS_ROOT = pathlib.Path(__file__).resolve().parents[1]
if str(SCRIPTS_ROOT) not in sys.path:
    sys.path.insert(0, str(SCRIPTS_ROOT))

from stage6_common.immutable_evidence_io import (  # noqa: E402
    ImmutableEvidenceIOError,
    publish_immutable_with_sha256,
    read_stable_regular_file,
)


SCHEMA_VERSION = "synara.stage6-final-ga-review.v1"
RECEIPT_SCHEMA_VERSION = "synara.stage6-final-ga-review-validation.v1"
ASSESSMENT = "final-review-consistent-not-ga-authority-verified"
COMMIT_RE = re.compile(r"^[0-9a-f]{40}$")
SHA256_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
HEX_SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
IDENTIFIER_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._+:/@-]{1,199}$")
UUID_RE = re.compile(
    r"^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$"
)
MAX_MANIFEST_BYTES = 2 * 1024 * 1024
MAX_EVIDENCE_BYTES = 16 * 1024 * 1024
MAX_TOTAL_EVIDENCE_BYTES = 96 * 1024 * 1024

CONTROL_OWNERS = {
    "tenant-registration": "product",
    "tenant-lifecycle": "operations",
    "identity-lifecycle": "security",
    "sso-domain": "security",
    "identity-governance": "security",
    "offer-admission": "product",
    "usage-reconciliation": "engineering",
    "quota-concurrency": "engineering",
    "usage-explainability": "product",
    "internal-cost-reconciliation": "operations",
    "operations-matrix": "operations",
    "authority-views": "operations",
    "support-access": "security",
    "incident-communications": "operations",
    "audit-retention-legal-hold": "security",
    "privacy-workflows": "privacy_legal",
    "data-residency": "privacy_legal",
    "provider-compliance": "privacy_legal",
    "compliance-program": "security",
    "desktop-native": "engineering",
    "desktop-security": "security",
    "desktop-postgres-concurrency": "engineering",
    "recovery": "operations",
    "slo": "operations",
    "tracing-isolation": "security",
    "penetration-stage5": "security",
    "worker-supply-chain": "security",
    "compatibility-rollback": "engineering",
    "key-rotation": "security",
    "capacity": "operations",
    "documentation": "product",
    "change-notice": "product",
}
BASE_FINAL_APPROVAL_ROLES = {"engineering", "operations", "security", "product"}
PRIVACY_LEGAL_IMPACT_DOMAINS = {
    "data_residency",
    "personal_data",
    "provider_commercial",
    "regulated_customer",
    "retention_legal_hold",
    "security_incident",
}
ALLOWED_IMPACT_DOMAINS = PRIVACY_LEGAL_IMPACT_DOMAINS | {
    "code_change",
    "data_migration",
    "desktop_distribution",
    "internal_cost",
    "runtime_isolation",
}
REQUIRED_CHECKLIST_HEADINGS = (
    "## Release identity",
    "## Tenant and identity lifecycle",
    "## Plan, quota, usage and internal cost",
    "## Operations and support",
    "## Data governance and compliance",
    "## Reliability, security and release",
    "## Final decision",
)
REQUIRED_NOTICE_HEADINGS = (
    "## Identity",
    "## Customer summary",
    "## Administrator and user action",
    "## Security, privacy, and data",
    "## Breaking change and timing",
    "## Publication proof",
)


class FinalGAReviewError(Exception):
    pass


def require_mapping(value: Any, label: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise FinalGAReviewError(f"{label} must be an object")
    return value


def require_exact(value: Any, fields: set[str], label: str) -> dict[str, Any]:
    result = require_mapping(value, label)
    if set(result) != fields:
        raise FinalGAReviewError(f"{label} fields do not match the v1 schema")
    return result


def require_identifier(value: Any, label: str) -> str:
    if not isinstance(value, str) or IDENTIFIER_RE.fullmatch(value.strip()) is None:
        raise FinalGAReviewError(f"{label} must be a bounded identifier")
    return value.strip()


def require_commit(value: Any, label: str) -> str:
    if not isinstance(value, str) or COMMIT_RE.fullmatch(value) is None:
        raise FinalGAReviewError(f"{label} must be a lowercase 40-character commit")
    return value


def require_sha256(value: Any, label: str) -> str:
    if not isinstance(value, str) or SHA256_RE.fullmatch(value) is None:
        raise FinalGAReviewError(f"{label} must be a sha256-prefixed digest")
    return value


def require_hex_sha256(value: Any, label: str) -> str:
    if not isinstance(value, str) or HEX_SHA256_RE.fullmatch(value) is None or set(value) == {"0"}:
        raise FinalGAReviewError(f"{label} must be a non-zero lowercase SHA-256 digest")
    return value


def parse_utc(value: Any, label: str) -> dt.datetime:
    if not isinstance(value, str) or not value.endswith("Z"):
        raise FinalGAReviewError(f"{label} must be an RFC3339 UTC timestamp ending in Z")
    try:
        parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as error:
        raise FinalGAReviewError(f"{label} must be an RFC3339 UTC timestamp") from error
    if parsed.tzinfo is None or parsed.utcoffset() != dt.timedelta(0):
        raise FinalGAReviewError(f"{label} must use UTC")
    return parsed


def sha256_bytes(value: bytes) -> str:
    return "sha256:" + hashlib.sha256(value).hexdigest()


def validate_evidence_root(value: pathlib.Path) -> pathlib.Path:
    if value.is_symlink():
        raise FinalGAReviewError("evidenceRoot must not be a symlink")
    try:
        root = value.resolve(strict=True)
    except OSError as error:
        raise FinalGAReviewError("evidenceRoot must exist") from error
    if not root.is_dir():
        raise FinalGAReviewError("evidenceRoot must be a directory")
    return root


def resolve_reference_path(root: pathlib.Path, value: Any, label: str) -> pathlib.Path:
    if not isinstance(value, str) or not value:
        raise FinalGAReviewError(f"{label}.path must be a non-empty relative path")
    relative = pathlib.Path(value)
    if relative.is_absolute() or any(part in {"", ".", ".."} for part in relative.parts):
        raise FinalGAReviewError(f"{label}.path must be traversal-free and relative")
    candidate = root / relative
    current = candidate
    while current != root:
        if current.is_symlink():
            raise FinalGAReviewError(f"{label}.path must not traverse a symlink")
        current = current.parent
    try:
        resolved = candidate.resolve(strict=True)
        resolved.relative_to(root)
    except (OSError, ValueError) as error:
        raise FinalGAReviewError(f"{label}.path must resolve inside evidenceRoot") from error
    return resolved


def validate_reference(
    value: Any,
    root: pathlib.Path,
    label: str,
    references: dict[str, str],
    total_bytes: list[int],
) -> tuple[dict[str, str], bytes]:
    reference = require_exact(value, {"path", "sha256"}, label)
    path = resolve_reference_path(root, reference["path"], label)
    digest = require_sha256(reference["sha256"], f"{label}.sha256")
    try:
        encoded = read_stable_regular_file(
            path, label=label, maximum_bytes=MAX_EVIDENCE_BYTES
        )
    except ImmutableEvidenceIOError as error:
        raise FinalGAReviewError(str(error)) from error
    actual = sha256_bytes(encoded)
    if actual != digest:
        raise FinalGAReviewError(f"{label}.sha256 does not match the referenced bytes")
    relative = path.relative_to(root).as_posix()
    previous = references.get(relative)
    if previous is not None and previous != digest:
        raise FinalGAReviewError(f"{label}.path is reused with a different digest")
    references[relative] = digest
    total_bytes[0] += len(encoded)
    if total_bytes[0] > MAX_TOTAL_EVIDENCE_BYTES:
        raise FinalGAReviewError("referenced evidence exceeds the total byte limit")
    return {"path": relative, "sha256": digest}, encoded


def parse_json(encoded: bytes, label: str) -> dict[str, Any]:
    try:
        value = json.loads(encoded)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise FinalGAReviewError(f"{label} must contain valid UTF-8 JSON") from error
    return require_mapping(value, label)


def validate_completed_markdown(
    encoded: bytes,
    label: str,
    headings: tuple[str, ...],
    minimum_checked_rows: int,
    candidate_id: str,
    source_commit: str,
) -> None:
    try:
        text = encoded.decode("utf-8")
    except UnicodeDecodeError as error:
        raise FinalGAReviewError(f"{label} must be UTF-8 Markdown") from error
    for heading in headings:
        if heading not in text:
            raise FinalGAReviewError(f"{label} is missing required heading {heading}")
    if "- [ ]" in text:
        raise FinalGAReviewError(f"{label} still contains unchecked rows")
    checked = text.count("- [x]") + text.count("- [X]")
    if checked < minimum_checked_rows:
        raise FinalGAReviewError(f"{label} does not contain the completed candidate checklist")
    if candidate_id not in text or source_commit not in text:
        raise FinalGAReviewError(f"{label} does not bind the exact candidate and commit")


def validate(
    manifest_path: pathlib.Path, evidence_root: pathlib.Path, validated_at: str | None
) -> dict[str, Any]:
    root = validate_evidence_root(evidence_root)
    try:
        manifest_bytes = read_stable_regular_file(
            manifest_path, label="final GA review manifest", maximum_bytes=MAX_MANIFEST_BYTES
        )
    except ImmutableEvidenceIOError as error:
        raise FinalGAReviewError(str(error)) from error
    manifest = parse_json(manifest_bytes, "final GA review manifest")
    manifest = require_exact(
        manifest,
        {
            "schemaVersion",
            "candidate",
            "releaseManagerId",
            "impactDomains",
            "controlDecisions",
            "finalApprovals",
            "platformAuditRequestIds",
            "decisionSummary",
            "residualRiskDisposition",
            "residualRisks",
            "startedAt",
            "completedAt",
        },
        "final GA review manifest",
    )
    if manifest["schemaVersion"] != SCHEMA_VERSION:
        raise FinalGAReviewError(f"schemaVersion must be {SCHEMA_VERSION}")
    candidate = require_exact(
        manifest["candidate"],
        {
            "candidateId",
            "releaseTag",
            "sourceCommit",
            "environmentId",
            "candidateBundleReceipt",
            "protectedReleaseApproval",
            "finalAssetSet",
            "copiedChecklist",
            "changeNotice",
            "platformAuditExport",
        },
        "candidate",
    )
    candidate_id = require_identifier(candidate["candidateId"], "candidate.candidateId")
    release_tag = require_identifier(candidate["releaseTag"], "candidate.releaseTag")
    if release_tag != candidate_id:
        raise FinalGAReviewError("candidate.releaseTag must match candidateId")
    source_commit = require_commit(candidate["sourceCommit"], "candidate.sourceCommit")
    environment_id = require_identifier(candidate["environmentId"], "candidate.environmentId")
    release_manager_id = require_identifier(manifest["releaseManagerId"], "releaseManagerId")
    decision_summary = manifest["decisionSummary"]
    if not isinstance(decision_summary, str) or not 20 <= len(decision_summary.strip()) <= 4000:
        raise FinalGAReviewError("decisionSummary must be bounded and non-empty")
    decision_summary = decision_summary.strip()
    started_at = parse_utc(manifest["startedAt"], "startedAt")
    completed_at = parse_utc(manifest["completedAt"], "completedAt")
    if completed_at <= started_at:
        raise FinalGAReviewError("completedAt must be after startedAt")
    timestamp = validated_at or dt.datetime.now(dt.timezone.utc).isoformat().replace("+00:00", "Z")
    validation_time = parse_utc(timestamp, "validatedAt")
    if validation_time < completed_at:
        raise FinalGAReviewError("validatedAt cannot predate completedAt")

    references: dict[str, str] = {}
    total_bytes = [0]
    candidate_ref, candidate_bytes = validate_reference(
        candidate["candidateBundleReceipt"], root, "candidate.candidateBundleReceipt", references, total_bytes
    )
    candidate_receipt = parse_json(candidate_bytes, "candidate bundle receipt")
    if (
        candidate_receipt.get("schemaVersion")
        != "synara.stage6-candidate-evidence-bundle-validation.v5"
        or candidate_receipt.get("assessment") != "evidence-consistent-not-ga-approved"
        or candidate_receipt.get("requiredReceiptCount") != 10
        or candidate_receipt.get("allRequiredReceiptsReadyForCandidateReview") is not True
        or candidate_receipt.get("eligibleForCandidateEvidenceReview") is not True
        or candidate_receipt.get("candidateConsistencyValidated") is not True
    ):
        raise FinalGAReviewError("candidate bundle receipt is not an eligible internal-self-hosted v4 consistency receipt")
    candidate_validated_at = parse_utc(
        candidate_receipt.get("validatedAt"), "candidate bundle receipt.validatedAt"
    )
    projected_candidate = require_mapping(candidate_receipt.get("candidate"), "candidate bundle receipt.candidate")
    candidate_lockfile_sha256 = require_hex_sha256(
        projected_candidate.get("lockfileSha256"), "candidate bundle receipt.candidate.lockfileSha256"
    )
    candidate_desktop_artifact_set_sha256 = require_sha256(
        candidate_receipt.get("desktopArtifactSetSha256"),
        "candidate bundle receipt.desktopArtifactSetSha256",
    )
    if (
        projected_candidate.get("candidateId") != candidate_id
        or projected_candidate.get("sourceCommit") != source_commit
        or projected_candidate.get("environmentId") != environment_id
    ):
        raise FinalGAReviewError("candidate bundle receipt does not match final review identity")

    approval_ref, approval_bytes = validate_reference(
        candidate["protectedReleaseApproval"], root, "candidate.protectedReleaseApproval", references, total_bytes
    )
    approval = require_exact(
        parse_json(approval_bytes, "protected release approval"),
        {
            "schemaVersion",
            "candidate",
            "build",
            "approval",
            "exactCandidatePublicationAuthorized",
            "assessment",
        },
        "protected release approval",
    )
    approval_candidate = require_exact(
        approval["candidate"],
        {
            "candidateId",
            "sourceCommit",
            "lockfileSha256",
            "candidateBundleReceiptSha256",
            "desktopArtifactSetSha256",
            "finalAssetSetSha256",
            "rawFinalCrossBindingSha256",
        },
        "protected release approval.candidate",
    )
    require_hex_sha256(
        approval_candidate["lockfileSha256"],
        "protected release approval.candidate.lockfileSha256",
    )
    for digest_field in (
        "candidateBundleReceiptSha256",
        "desktopArtifactSetSha256",
        "finalAssetSetSha256",
        "rawFinalCrossBindingSha256",
    ):
        require_sha256(
            approval_candidate[digest_field],
            f"protected release approval.candidate.{digest_field}",
        )
    approval_build = require_exact(
        approval["build"],
        {"runId", "runAttempt", "repository", "workflowRef", "sourceBranch", "actor", "triggeringActor"},
        "protected release approval.build",
    )
    run_id = approval_build["runId"]
    if not isinstance(run_id, str) or re.fullmatch(r"[1-9][0-9]{0,19}", run_id) is None:
        raise FinalGAReviewError("protected release approval.build.runId is invalid")
    if approval_build["runAttempt"] != 1:
        raise FinalGAReviewError("protected release approval must come from the first run attempt")
    repository = approval_build["repository"]
    source_branch = require_identifier(
        approval_build["sourceBranch"], "protected release approval.build.sourceBranch"
    )
    if (
        not isinstance(repository, str)
        or re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository) is None
        or approval_build["workflowRef"]
        != f"{repository}/.github/workflows/release.yml@refs/heads/{source_branch}"
    ):
        raise FinalGAReviewError("protected release approval workflow identity is invalid")
    build_actor = require_identifier(
        approval_build["actor"], "protected release approval.build.actor"
    )
    triggering_actor = require_identifier(
        approval_build["triggeringActor"], "protected release approval.build.triggeringActor"
    )
    approval_metadata = require_exact(
        approval["approval"],
        {
            "environment",
            "environmentId",
            "configuredReviewers",
            "preventSelfReview",
            "administratorBypassDisabled",
            "protectedBranchesOnly",
            "actualReviewer",
            "commentSha256",
            "environmentConfigurationSha256",
            "reviewSha256",
            "approvalEvidenceSha256",
            "recordedAt",
        },
        "protected release approval.approval",
    )
    configured_reviewers = approval_metadata["configuredReviewers"]
    actual_reviewer = require_mapping(
        approval_metadata["actualReviewer"], "protected release approval.approval.actualReviewer"
    )
    reviewer_login = require_identifier(
        actual_reviewer.get("login"), "protected release approval.approval.actualReviewer.login"
    )
    if (
        approval_metadata["environment"] != "stage6-enterprise-ga"
        or not isinstance(approval_metadata["environmentId"], int)
        or isinstance(approval_metadata["environmentId"], bool)
        or approval_metadata["environmentId"] <= 0
        or not isinstance(configured_reviewers, list)
        or not 2 <= len(configured_reviewers) <= 6
        or approval_metadata["preventSelfReview"] is not True
        or approval_metadata["administratorBypassDisabled"] is not True
        or approval_metadata["protectedBranchesOnly"] is not True
        or reviewer_login in {build_actor, triggering_actor}
    ):
        raise FinalGAReviewError("protected release approval environment controls are invalid")
    for digest_field in (
        "commentSha256",
        "environmentConfigurationSha256",
        "reviewSha256",
        "approvalEvidenceSha256",
    ):
        require_sha256(
            approval_metadata[digest_field],
            f"protected release approval.approval.{digest_field}",
        )
    if (
        approval.get("schemaVersion") != "synara.stage6-release-approval.v4"
        or approval.get("assessment") != "protected-environment-gate-recorded-not-ga-approved"
        or approval.get("exactCandidatePublicationAuthorized") is not True
        or approval_candidate.get("candidateId") != candidate_id
        or approval_candidate.get("sourceCommit") != source_commit
        or approval_candidate.get("lockfileSha256") != candidate_lockfile_sha256
        or approval_candidate.get("candidateBundleReceiptSha256") != candidate_ref["sha256"]
        or approval_candidate.get("desktopArtifactSetSha256")
        != candidate_desktop_artifact_set_sha256
    ):
        raise FinalGAReviewError("protected release approval does not match the exact candidate receipt")
    approval_time = parse_utc(
        approval_metadata["recordedAt"],
        "protected release approval.approval.recordedAt",
    )
    if approval_time < candidate_validated_at or approval_time > completed_at:
        raise FinalGAReviewError(
            "protected release approval must follow candidate validation and not postdate review completion"
        )

    asset_ref, asset_bytes = validate_reference(
        candidate["finalAssetSet"], root, "candidate.finalAssetSet", references, total_bytes
    )
    final_assets = parse_json(asset_bytes, "final asset set")
    asset_source = require_mapping(final_assets.get("source"), "final asset set.source")
    final_asset_set_sha256 = require_sha256(
        final_assets.get("finalAssetSetSha256"), "final asset set.finalAssetSetSha256"
    )
    if (
        final_assets.get("schemaVersion") != "synara.release-final-asset-set.v1"
        or final_assets.get("assessment") != "final-assets-bound-not-published"
        or asset_source.get("tag") != release_tag
        or asset_source.get("commit") != source_commit
        or asset_source.get("lockfileSha256") != candidate_lockfile_sha256
        or approval_candidate.get("finalAssetSetSha256") != final_asset_set_sha256
    ):
        raise FinalGAReviewError("final asset set does not match protected release approval")

    checklist_ref, checklist_bytes = validate_reference(
        candidate["copiedChecklist"], root, "candidate.copiedChecklist", references, total_bytes
    )
    validate_completed_markdown(
        checklist_bytes,
        "copied checklist",
        REQUIRED_CHECKLIST_HEADINGS,
        len(CONTROL_OWNERS) + 10,
        candidate_id,
        source_commit,
    )
    notice_ref, notice_bytes = validate_reference(
        candidate["changeNotice"], root, "candidate.changeNotice", references, total_bytes
    )
    validate_completed_markdown(
        notice_bytes,
        "change notice",
        REQUIRED_NOTICE_HEADINGS,
        20,
        candidate_id,
        source_commit,
    )
    audit_ref, audit_bytes = validate_reference(
        candidate["platformAuditExport"], root, "candidate.platformAuditExport", references, total_bytes
    )

    audit_request_ids = manifest["platformAuditRequestIds"]
    if not isinstance(audit_request_ids, list) or not audit_request_ids:
        raise FinalGAReviewError("platformAuditRequestIds must be a non-empty list")
    normalized_request_ids: list[str] = []
    for index, value in enumerate(audit_request_ids):
        if not isinstance(value, str) or UUID_RE.fullmatch(value) is None:
            raise FinalGAReviewError(f"platformAuditRequestIds[{index}] must be a UUID")
        if value in normalized_request_ids:
            raise FinalGAReviewError("platformAuditRequestIds must be unique")
        if value.encode("utf-8") not in audit_bytes:
            raise FinalGAReviewError(f"platform Audit export is missing request ID {value}")
        normalized_request_ids.append(value)
    if candidate_id.encode("utf-8") not in audit_bytes or source_commit.encode("utf-8") not in audit_bytes:
        raise FinalGAReviewError("platform Audit export does not bind the exact candidate and commit")

    raw_domains = manifest["impactDomains"]
    if not isinstance(raw_domains, list) or not raw_domains:
        raise FinalGAReviewError("impactDomains must be a non-empty list")
    impact_domains: list[str] = []
    for value in raw_domains:
        if not isinstance(value, str) or value not in ALLOWED_IMPACT_DOMAINS or value in impact_domains:
            raise FinalGAReviewError("impactDomains contains an unknown or duplicate value")
        impact_domains.append(value)

    raw_controls = manifest["controlDecisions"]
    if not isinstance(raw_controls, list):
        raise FinalGAReviewError("controlDecisions must be a list")
    controls: list[dict[str, Any]] = []
    seen_controls: set[str] = set()
    status_counts = {"passed": 0, "failed": 0, "blocked": 0}
    latest_control_decision = started_at
    for index, raw_control in enumerate(raw_controls):
        control = require_exact(
            raw_control,
            {"id", "ownerRole", "approverId", "status", "decidedAt", "evidence"},
            f"controlDecisions[{index}]",
        )
        control_id = control["id"]
        if not isinstance(control_id, str) or control_id not in CONTROL_OWNERS or control_id in seen_controls:
            raise FinalGAReviewError("controlDecisions contains an unknown or duplicate control")
        seen_controls.add(control_id)
        owner_role = control["ownerRole"]
        if owner_role != CONTROL_OWNERS[control_id]:
            raise FinalGAReviewError(f"{control_id}.ownerRole must be {CONTROL_OWNERS[control_id]}")
        approver_id = require_identifier(control["approverId"], f"{control_id}.approverId")
        status = control["status"]
        if status not in status_counts:
            raise FinalGAReviewError(f"{control_id}.status is not allowed")
        decided_at = parse_utc(control["decidedAt"], f"{control_id}.decidedAt")
        if decided_at < started_at or decided_at > completed_at:
            raise FinalGAReviewError(f"{control_id}.decidedAt is outside the review window")
        latest_control_decision = max(latest_control_decision, decided_at)
        raw_evidence = control["evidence"]
        if not isinstance(raw_evidence, list) or not raw_evidence:
            raise FinalGAReviewError(f"{control_id}.evidence must be a non-empty list")
        evidence = [
            validate_reference(item, root, f"{control_id}.evidence[{evidence_index}]", references, total_bytes)[0]
            for evidence_index, item in enumerate(raw_evidence)
        ]
        status_counts[status] += 1
        controls.append(
            {
                "id": control_id,
                "ownerRole": owner_role,
                "approverId": approver_id,
                "status": status,
                "decidedAt": control["decidedAt"],
                "evidence": evidence,
            }
        )
    if seen_controls != set(CONTROL_OWNERS):
        raise FinalGAReviewError(
            f"control inventory drifted; missing={sorted(set(CONTROL_OWNERS) - seen_controls)}"
        )

    required_approval_roles = set(BASE_FINAL_APPROVAL_ROLES)
    if set(impact_domains) & PRIVACY_LEGAL_IMPACT_DOMAINS:
        required_approval_roles.add("privacy_legal")
    raw_approvals = manifest["finalApprovals"]
    if not isinstance(raw_approvals, list):
        raise FinalGAReviewError("finalApprovals must be a list")
    approvals: list[dict[str, Any]] = []
    seen_roles: set[str] = set()
    approver_ids: set[str] = set()
    approvals_passed = True
    for index, raw_approval in enumerate(raw_approvals):
        approval_entry = require_exact(
            raw_approval,
            {"role", "approverId", "decision", "approvedAt", "evidence"},
            f"finalApprovals[{index}]",
        )
        role = approval_entry["role"]
        if not isinstance(role, str) or role not in required_approval_roles or role in seen_roles:
            raise FinalGAReviewError("finalApprovals contains an unknown or duplicate role")
        approver_id = require_identifier(approval_entry["approverId"], f"{role}.approverId")
        if approver_id == release_manager_id or approver_id in approver_ids:
            raise FinalGAReviewError("final approval identities must be distinct from each other and the release manager")
        decision = approval_entry["decision"]
        if decision not in {"approved-for-ga-authority-review", "denied"}:
            raise FinalGAReviewError(f"{role}.decision is not allowed")
        approved_at = parse_utc(approval_entry["approvedAt"], f"{role}.approvedAt")
        if approved_at < started_at or approved_at > completed_at:
            raise FinalGAReviewError(f"{role}.approvedAt is outside the review window")
        if approved_at < max(latest_control_decision, approval_time):
            raise FinalGAReviewError(
                f"{role}.approvedAt must not predate protected approval or control decisions"
            )
        evidence, _ = validate_reference(
            approval_entry["evidence"], root, f"{role}.evidence", references, total_bytes
        )
        seen_roles.add(role)
        approver_ids.add(approver_id)
        approvals_passed = approvals_passed and decision == "approved-for-ga-authority-review"
        approvals.append(
            {
                "role": role,
                "approverId": approver_id,
                "decision": decision,
                "approvedAt": approval_entry["approvedAt"],
                "evidence": evidence,
            }
        )
    if seen_roles != required_approval_roles:
        raise FinalGAReviewError(
            f"final approval inventory drifted; missing={sorted(required_approval_roles - seen_roles)}"
        )

    disposition = manifest["residualRiskDisposition"]
    raw_risks = manifest["residualRisks"]
    if disposition not in {"none", "accepted"} or not isinstance(raw_risks, list):
        raise FinalGAReviewError("residual risk disposition is invalid")
    risks: list[dict[str, str]] = []
    seen_risks: set[str] = set()
    if disposition == "none" and raw_risks:
        raise FinalGAReviewError("none residual-risk disposition requires an empty inventory")
    if disposition == "accepted" and not raw_risks:
        raise FinalGAReviewError("accepted residual-risk disposition requires at least one risk")
    for index, raw_risk in enumerate(raw_risks):
        risk = require_exact(
            raw_risk,
            {"id", "summary", "owner", "dueAt", "acceptanceReason", "evidenceReference"},
            f"residualRisks[{index}]",
        )
        risk_id = require_identifier(risk["id"], f"residualRisks[{index}].id")
        if risk_id in seen_risks:
            raise FinalGAReviewError("residual risk IDs must be unique")
        owner = require_identifier(risk["owner"], f"residualRisks[{index}].owner")
        due_at = parse_utc(risk["dueAt"], f"residualRisks[{index}].dueAt")
        if due_at <= completed_at:
            raise FinalGAReviewError("accepted residual risk dueAt must be after review completion")
        summary = risk["summary"]
        reason = risk["acceptanceReason"]
        evidence_url = risk["evidenceReference"]
        if not isinstance(summary, str) or not 10 <= len(summary.strip()) <= 500:
            raise FinalGAReviewError("residual risk summary must be bounded and non-empty")
        if not isinstance(reason, str) or not 10 <= len(reason.strip()) <= 1000:
            raise FinalGAReviewError("residual risk acceptanceReason must be bounded and non-empty")
        if not isinstance(evidence_url, str) or not evidence_url.startswith("https://") or len(evidence_url) > 2048:
            raise FinalGAReviewError("residual risk evidenceReference must be bounded HTTPS")
        seen_risks.add(risk_id)
        risks.append(
            {
                "id": risk_id,
                "summary": summary.strip(),
                "owner": owner,
                "dueAt": risk["dueAt"],
                "acceptanceReason": reason.strip(),
                "evidenceReference": evidence_url,
            }
        )

    eligible = status_counts["passed"] == len(CONTROL_OWNERS) and approvals_passed
    return {
        "schemaVersion": RECEIPT_SCHEMA_VERSION,
        "candidate": {
            "candidateId": candidate_id,
            "releaseTag": release_tag,
            "sourceCommit": source_commit,
            "environmentId": environment_id,
            "candidateBundleReceipt": candidate_ref,
            "protectedReleaseApproval": approval_ref,
            "finalAssetSet": asset_ref,
            "finalAssetSetSha256": final_asset_set_sha256,
            "lockfileSha256": candidate_lockfile_sha256,
            "desktopArtifactSetSha256": candidate_desktop_artifact_set_sha256,
            "copiedChecklist": checklist_ref,
            "changeNotice": notice_ref,
            "platformAuditExport": audit_ref,
            "candidateValidatedAt": candidate_receipt["validatedAt"],
            "protectedReleaseApprovedAt": approval_metadata["recordedAt"],
        },
        "releaseManagerId": release_manager_id,
        "impactDomains": sorted(impact_domains),
        "controlInventorySha256": sha256_bytes(
            json.dumps(CONTROL_OWNERS, sort_keys=True, separators=(",", ":")).encode("utf-8")
        ),
        "controlCount": len(controls),
        "controlStatusCounts": status_counts,
        "controlDecisions": sorted(controls, key=lambda item: item["id"]),
        "finalApprovals": sorted(approvals, key=lambda item: item["role"]),
        "platformAuditRequestIds": sorted(normalized_request_ids),
        "decisionSummary": decision_summary,
        "residualRiskDisposition": disposition,
        "residualRisks": sorted(risks, key=lambda item: item["id"]),
        "startedAt": manifest["startedAt"],
        "completedAt": manifest["completedAt"],
        "validatedAt": timestamp,
        "allRequiredControlsPassed": status_counts["passed"] == len(CONTROL_OWNERS),
        "allRequiredFinalApprovalsApproved": approvals_passed,
        "eligibleForExternalGAAuthorityReview": eligible,
        "verificationBoundary": {
            "externalEvidenceAuthorityVerified": False,
            "approverCorporateAuthorityVerified": False,
            "externalSignaturesVerified": False,
            "publicationDeliveryVerifiedBySynara": False,
        },
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
            pathlib.Path(args.manifest).absolute(),
            pathlib.Path(args.evidence_root).absolute(),
            args.validated_at,
        )
        output = pathlib.Path(args.output).absolute()
        encoded = (json.dumps(receipt, indent=2, sort_keys=True) + "\n").encode("utf-8")
        publish_immutable_with_sha256(output, encoded)
    except (FinalGAReviewError, ImmutableEvidenceIOError) as error:
        parser.exit(2, f"final GA review validation failed: {error}\n")
    print(
        f"wrote {output}; eligibleForExternalGAAuthorityReview="
        f"{str(receipt['eligibleForExternalGAAuthorityReview']).lower()}"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
