#!/usr/bin/env python3
"""Validate Stage 6 third-party penetration evidence without declaring the gate passed."""

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

ASSET_TYPES = {
    "web",
    "control-plane-api",
    "worker-runtime",
    "provider-host",
}
SCOPE_AREAS = {
    "crossTenantAuthorization",
    "ssrf",
    "commandInjection",
    "pathTraversal",
    "supplyChain",
    "containerEscape",
}
REQUIRED_METHODOLOGIES = {
    "manual-business-logic",
    "owasp-web-api",
    "cloud-runtime",
}
ALLOWED_METHODOLOGIES = REQUIRED_METHODOLOGIES | {
    "owasp-asvs",
    "owasp-wstg",
    "ptes",
}
SEVERITIES = ("critical", "high", "medium", "low", "informational")
FINDING_STATUSES = ("open", "remediation-in-progress", "remediated-verified", "risk-accepted")
EVIDENCE_FIELDS = (
    "stage5CompletionEvidence",
    "targetRevalidationEvidence",
    "scopeStatementEvidence",
    "assessorIndependenceEvidence",
    "executionEvidence",
    "finalReportEvidence",
    "findingRegisterEvidence",
    "remediationRetestEvidence",
    "riskAcceptanceEvidence",
)
MANIFEST_SCHEMA = "synara.third-party-penetration-evidence.v1"
RECEIPT_SCHEMA = "synara.third-party-penetration-evidence-receipt.v1"
ASSESSMENT = "evidence-validated-not-penetration-passed"
MAX_MANIFEST_BYTES = 2 * 1024 * 1024
MAX_EVIDENCE_FILE_BYTES = 32 * 1024 * 1024
MAX_TOTAL_EVIDENCE_BYTES = 256 * 1024 * 1024
PROHIBITED_SECRET_PATTERNS = (
    ("private key", re.compile(rb"-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----")),
    ("AWS access key", re.compile(rb"\bAKIA[0-9A-Z]{16}\b")),
    ("bearer credential", re.compile(rb"(?i)authorization\s*[:=]\s*bearer\s+[^\s,;]+")),
    (
        "credential-bearing URL",
        re.compile(rb"(?i)\b(?:https?|postgres(?:ql)?|mysql)://[^\s/:@]+:[^\s/@]+@"),
    ),
)


class PenetrationEvidenceError(Exception):
    pass


def reject_duplicate_json_fields(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise PenetrationEvidenceError(
                f"manifest contains duplicate JSON field {key!r}"
            )
        result[key] = value
    return result


def scan_for_secret_material(content: bytes, label: str) -> None:
    for secret_label, pattern in PROHIBITED_SECRET_PATTERNS:
        if pattern.search(content):
            raise PenetrationEvidenceError(
                f"{label} contains prohibited {secret_label} material"
            )


def require_mapping(value: Any, label: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise PenetrationEvidenceError(f"{label} must be an object")
    return value


def require_identifier(value: Any, label: str) -> str:
    if not isinstance(value, str) or IDENTIFIER_RE.fullmatch(value.strip()) is None:
        raise PenetrationEvidenceError(f"{label} must be a bounded identifier")
    return value.strip()


def require_bool(value: Any, label: str) -> bool:
    if not isinstance(value, bool):
        raise PenetrationEvidenceError(f"{label} must be a boolean")
    return value


def parse_utc(value: Any, label: str) -> dt.datetime:
    if not isinstance(value, str) or not value.endswith("Z"):
        raise PenetrationEvidenceError(f"{label} must be an RFC3339 UTC timestamp ending in Z")
    try:
        parsed = dt.datetime.fromisoformat(value[:-1] + "+00:00")
    except ValueError as error:
        raise PenetrationEvidenceError(f"{label} must be a valid RFC3339 UTC timestamp") from error
    if parsed.tzinfo != dt.timezone.utc:
        raise PenetrationEvidenceError(f"{label} must use UTC")
    return parsed


def resolve_evidence_file(root: pathlib.Path, raw_path: Any, label: str) -> pathlib.Path:
    if not isinstance(raw_path, str) or not raw_path.strip():
        raise PenetrationEvidenceError(f"{label}.path must be a non-empty relative path")
    relative = pathlib.Path(raw_path)
    if relative.is_absolute() or any(part in {".", ".."} for part in relative.parts):
        raise PenetrationEvidenceError(f"{label}.path must be traversal-free and relative")
    candidate = root / relative
    current = candidate
    while current != root:
        if current.is_symlink():
            raise PenetrationEvidenceError(f"{label}.path must not traverse a symlink")
        current = current.parent
    try:
        resolved = candidate.resolve(strict=True)
        resolved.relative_to(root)
    except (OSError, ValueError) as error:
        raise PenetrationEvidenceError(f"{label}.path must resolve inside the evidence root") from error
    if not resolved.is_file():
        raise PenetrationEvidenceError(f"{label}.path must reference a regular file")
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
        raise PenetrationEvidenceError(f"{label} must contain exactly path and sha256")
    path = resolve_evidence_file(root, reference["path"], label)
    if path in used_paths:
        raise PenetrationEvidenceError(f"{label}.path duplicates another evidence file")
    used_paths.add(path)
    try:
        content = read_stable_regular_file(
            path,
            label=label,
            maximum_bytes=MAX_EVIDENCE_FILE_BYTES,
        )
    except ImmutableEvidenceIOError as error:
        raise PenetrationEvidenceError(str(error)) from error
    total_bytes[0] += len(content)
    if total_bytes[0] > MAX_TOTAL_EVIDENCE_BYTES:
        raise PenetrationEvidenceError(
            "total penetration evidence exceeds the bounded size limit"
        )
    scan_for_secret_material(content, label)
    digest = reference["sha256"]
    if not isinstance(digest, str) or SHA256_RE.fullmatch(digest) is None:
        raise PenetrationEvidenceError(f"{label}.sha256 must be sha256:<64 lowercase hex>")
    actual = "sha256:" + hashlib.sha256(content).hexdigest()
    if actual != digest:
        raise PenetrationEvidenceError(f"{label}.sha256 does not match the evidence file")
    return {"path": path.relative_to(root).as_posix(), "sha256": actual}


def validate_assessor(value: Any) -> dict[str, Any]:
    assessor = require_mapping(value, "assessor")
    if set(assessor) != {
        "organizationReference",
        "engagementReference",
        "thirdParty",
        "independent",
        "noConflictDeclared",
    }:
        raise PenetrationEvidenceError("assessor fields do not match the v1 schema")
    return {
        "organizationReference": require_identifier(
            assessor["organizationReference"], "assessor.organizationReference"
        ),
        "engagementReference": require_identifier(
            assessor["engagementReference"], "assessor.engagementReference"
        ),
        "thirdParty": require_bool(assessor["thirdParty"], "assessor.thirdParty"),
        "independent": require_bool(assessor["independent"], "assessor.independent"),
        "noConflictDeclared": require_bool(
            assessor["noConflictDeclared"], "assessor.noConflictDeclared"
        ),
    }


def validate_stage5_dependency(value: Any, release_commit: str, deployment_profile: str) -> dict[str, Any]:
    dependency = require_mapping(value, "stage5Dependency")
    if set(dependency) != {
        "status",
        "acceptedCommit",
        "scopeProfile",
        "targetRevalidated",
    }:
        raise PenetrationEvidenceError("stage5Dependency fields do not match the v1 schema")
    status = dependency["status"]
    if status not in {"accepted-current-supported-surface", "not-accepted"}:
        raise PenetrationEvidenceError("stage5Dependency.status is not allowed")
    accepted_commit = dependency["acceptedCommit"]
    if not isinstance(accepted_commit, str) or COMMIT_RE.fullmatch(accepted_commit) is None:
        raise PenetrationEvidenceError("stage5Dependency.acceptedCommit must be a full lowercase Git SHA")
    scope_profile = require_identifier(dependency["scopeProfile"], "stage5Dependency.scopeProfile")
    if scope_profile != deployment_profile:
        raise PenetrationEvidenceError("stage5Dependency.scopeProfile must match deploymentProfile")
    target_revalidated = require_bool(
        dependency["targetRevalidated"], "stage5Dependency.targetRevalidated"
    )
    return {
        "status": status,
        "acceptedCommit": accepted_commit,
        "acceptedCommitEqualsReleaseCommit": accepted_commit == release_commit,
        "scopeProfile": scope_profile,
        "targetRevalidated": target_revalidated,
        "declaredSatisfied": status == "accepted-current-supported-surface" and target_revalidated,
    }


def validate_assets(value: Any) -> tuple[list[dict[str, Any]], bool]:
    if not isinstance(value, list) or len(value) != len(ASSET_TYPES):
        raise PenetrationEvidenceError("assets must contain every required release asset exactly once")
    result: list[dict[str, Any]] = []
    seen: set[str] = set()
    for index, raw in enumerate(value):
        asset = require_mapping(raw, f"assets[{index}]")
        if set(asset) != {"assetType", "assetId", "artifactDigest", "tested"}:
            raise PenetrationEvidenceError(f"assets[{index}] fields do not match the v1 schema")
        asset_type = asset["assetType"]
        if asset_type not in ASSET_TYPES or asset_type in seen:
            raise PenetrationEvidenceError("assets must contain every required release asset exactly once")
        asset_id = require_identifier(asset["assetId"], f"assets[{index}].assetId")
        digest = asset["artifactDigest"]
        if not isinstance(digest, str) or SHA256_RE.fullmatch(digest) is None:
            raise PenetrationEvidenceError(
                f"assets[{index}].artifactDigest must be sha256:<64 lowercase hex>"
            )
        tested = require_bool(asset["tested"], f"assets[{index}].tested")
        result.append(
            {
                "assetType": asset_type,
                "assetId": asset_id,
                "artifactDigest": digest,
                "tested": tested,
            }
        )
        seen.add(asset_type)
    if seen != ASSET_TYPES:
        raise PenetrationEvidenceError("assets must contain every required release asset exactly once")
    return sorted(result, key=lambda item: item["assetType"]), all(item["tested"] for item in result)


def validate_scope(value: Any) -> tuple[dict[str, dict[str, Any]], bool]:
    scope = require_mapping(value, "scopeCoverage")
    if set(scope) != SCOPE_AREAS:
        raise PenetrationEvidenceError("scopeCoverage must contain every required attack area exactly once")
    result: dict[str, dict[str, Any]] = {}
    complete = True
    for name in sorted(SCOPE_AREAS):
        area = require_mapping(scope[name], f"scopeCoverage.{name}")
        if set(area) != {"tested", "result"}:
            raise PenetrationEvidenceError(f"scopeCoverage.{name} fields do not match the v1 schema")
        tested = require_bool(area["tested"], f"scopeCoverage.{name}.tested")
        outcome = area["result"]
        if outcome not in {"no-finding", "findings-recorded", "not-tested"}:
            raise PenetrationEvidenceError(f"scopeCoverage.{name}.result is not allowed")
        if tested == (outcome == "not-tested"):
            raise PenetrationEvidenceError(
                f"scopeCoverage.{name}.tested and result contradict each other"
            )
        result[name] = {"tested": tested, "result": outcome}
        complete = complete and tested
    return result, complete


def validate_methodologies(value: Any) -> tuple[list[str], bool]:
    if not isinstance(value, list) or not value:
        raise PenetrationEvidenceError("methodologies must be a non-empty list")
    result: list[str] = []
    seen: set[str] = set()
    for index, raw in enumerate(value):
        methodology = require_identifier(raw, f"methodologies[{index}]")
        if methodology not in ALLOWED_METHODOLOGIES:
            raise PenetrationEvidenceError(f"methodologies[{index}] is not allowed")
        if methodology in seen:
            raise PenetrationEvidenceError("methodologies must not contain duplicates")
        result.append(methodology)
        seen.add(methodology)
    return sorted(result), REQUIRED_METHODOLOGIES.issubset(seen)


def validate_risk_acceptance(
    value: Any,
    finding_id: str,
    reviewed_at: dt.datetime,
    engagement_completed: dt.datetime,
) -> dict[str, Any]:
    acceptance = require_mapping(value, f"{finding_id}.riskAcceptance")
    if set(acceptance) != {
        "acceptanceId",
        "securityApprover",
        "businessApprover",
        "approvedAt",
        "expiresAt",
        "compensatingControlIds",
    }:
        raise PenetrationEvidenceError(f"{finding_id}.riskAcceptance fields do not match the v1 schema")
    acceptance_id = require_identifier(acceptance["acceptanceId"], f"{finding_id}.acceptanceId")
    security_approver = require_identifier(
        acceptance["securityApprover"], f"{finding_id}.securityApprover"
    )
    business_approver = require_identifier(
        acceptance["businessApprover"], f"{finding_id}.businessApprover"
    )
    if security_approver == business_approver:
        raise PenetrationEvidenceError(f"{finding_id} risk acceptance requires two distinct approvers")
    approved_at = parse_utc(acceptance["approvedAt"], f"{finding_id}.approvedAt")
    expires_at = parse_utc(acceptance["expiresAt"], f"{finding_id}.expiresAt")
    if not (reviewed_at <= approved_at <= engagement_completed < expires_at):
        raise PenetrationEvidenceError(f"{finding_id} risk acceptance timestamps are invalid")
    if expires_at - approved_at > dt.timedelta(days=30):
        raise PenetrationEvidenceError(f"{finding_id} risk acceptance must expire within 30 days")
    controls = acceptance["compensatingControlIds"]
    if not isinstance(controls, list) or not controls:
        raise PenetrationEvidenceError(f"{finding_id}.compensatingControlIds must be non-empty")
    parsed_controls: list[str] = []
    seen: set[str] = set()
    for index, raw in enumerate(controls):
        control = require_identifier(raw, f"{finding_id}.compensatingControlIds[{index}]")
        if control in seen:
            raise PenetrationEvidenceError(f"{finding_id}.compensatingControlIds must be unique")
        seen.add(control)
        parsed_controls.append(control)
    return {
        "acceptanceId": acceptance_id,
        "securityApprover": security_approver,
        "businessApprover": business_approver,
        "approvedAt": approved_at.isoformat().replace("+00:00", "Z"),
        "expiresAt": expires_at.isoformat().replace("+00:00", "Z"),
        "compensatingControlIds": sorted(parsed_controls),
    }


def validate_findings(
    value: Any,
    started: dt.datetime,
    completed: dt.datetime,
) -> tuple[list[dict[str, Any]], dict[str, dict[str, int]], bool]:
    if not isinstance(value, list):
        raise PenetrationEvidenceError("findings must be a list")
    findings: list[dict[str, Any]] = []
    finding_ids: set[str] = set()
    counts = {
        "bySeverity": {severity: 0 for severity in SEVERITIES},
        "byStatus": {status: 0 for status in FINDING_STATUSES},
    }
    no_unaccepted_high_or_critical = True
    for index, raw in enumerate(value):
        finding = require_mapping(raw, f"findings[{index}]")
        if set(finding) != {
            "findingId",
            "severity",
            "status",
            "affectedAssetTypes",
            "discoveredAt",
            "lastReviewedAt",
            "retestPassed",
            "riskAcceptance",
        }:
            raise PenetrationEvidenceError(f"findings[{index}] fields do not match the v1 schema")
        finding_id = require_identifier(finding["findingId"], f"findings[{index}].findingId")
        if finding_id in finding_ids:
            raise PenetrationEvidenceError("findingId values must be unique")
        finding_ids.add(finding_id)
        severity = finding["severity"]
        if severity not in SEVERITIES:
            raise PenetrationEvidenceError(f"{finding_id}.severity is not allowed")
        status = finding["status"]
        if status not in FINDING_STATUSES:
            raise PenetrationEvidenceError(f"{finding_id}.status is not allowed")
        affected = finding["affectedAssetTypes"]
        if not isinstance(affected, list) or not affected:
            raise PenetrationEvidenceError(f"{finding_id}.affectedAssetTypes must be non-empty")
        affected_types: list[str] = []
        for asset_type in affected:
            if asset_type not in ASSET_TYPES or asset_type in affected_types:
                raise PenetrationEvidenceError(
                    f"{finding_id}.affectedAssetTypes must contain unique known asset types"
                )
            affected_types.append(asset_type)
        discovered_at = parse_utc(finding["discoveredAt"], f"{finding_id}.discoveredAt")
        reviewed_at = parse_utc(finding["lastReviewedAt"], f"{finding_id}.lastReviewedAt")
        if not (started <= discovered_at <= reviewed_at <= completed):
            raise PenetrationEvidenceError(f"{finding_id} timestamps fall outside the engagement")
        retest_passed = require_bool(finding["retestPassed"], f"{finding_id}.retestPassed")
        risk_acceptance: dict[str, Any] | None = None
        if status == "remediated-verified":
            if not retest_passed or finding["riskAcceptance"] is not None:
                raise PenetrationEvidenceError(
                    f"{finding_id} remediated-verified requires a passed retest and no risk acceptance"
                )
        elif status == "risk-accepted":
            if retest_passed:
                raise PenetrationEvidenceError(f"{finding_id} risk-accepted cannot declare a passed retest")
            risk_acceptance = validate_risk_acceptance(
                finding["riskAcceptance"], finding_id, reviewed_at, completed
            )
        elif retest_passed or finding["riskAcceptance"] is not None:
            raise PenetrationEvidenceError(
                f"{finding_id} open findings cannot declare retest success or risk acceptance"
            )
        if severity == "critical" and status != "remediated-verified":
            no_unaccepted_high_or_critical = False
        if severity == "high" and status not in {"remediated-verified", "risk-accepted"}:
            no_unaccepted_high_or_critical = False
        counts["bySeverity"][severity] += 1
        counts["byStatus"][status] += 1
        findings.append(
            {
                "findingId": finding_id,
                "severity": severity,
                "status": status,
                "affectedAssetTypes": sorted(affected_types),
                "discoveredAt": discovered_at.isoformat().replace("+00:00", "Z"),
                "lastReviewedAt": reviewed_at.isoformat().replace("+00:00", "Z"),
                "retestPassed": retest_passed,
                "riskAcceptance": risk_acceptance,
            }
        )
    return sorted(findings, key=lambda item: item["findingId"]), counts, no_unaccepted_high_or_critical


def normalized_manifest_reference(value: str) -> str:
    path = pathlib.PurePosixPath(value)
    if (
        path.is_absolute()
        or not path.parts
        or any(part in {"", ".", ".."} for part in path.parts)
        or path.as_posix() != value
    ):
        raise PenetrationEvidenceError(
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
        raise PenetrationEvidenceError("evidence root must be a regular non-symlink directory")
    try:
        manifest_bytes = read_stable_regular_file(
            manifest_path,
            label="penetration manifest",
            maximum_bytes=MAX_MANIFEST_BYTES,
        )
        scan_for_secret_material(manifest_bytes, "penetration manifest")
        raw = json.loads(
            manifest_bytes,
            object_pairs_hook=reject_duplicate_json_fields,
        )
    except ImmutableEvidenceIOError as error:
        raise PenetrationEvidenceError(str(error)) from error
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise PenetrationEvidenceError("manifest must be valid JSON") from error
    manifest = require_mapping(raw, "manifest")
    if set(manifest) != {
        "schemaVersion",
        "engagementId",
        "releaseCommit",
        "environmentClass",
        "environmentId",
        "deploymentProfile",
        "startedAt",
        "completedAt",
        "reportIssuedAt",
        "assessor",
        "stage5Dependency",
        "assets",
        "scopeCoverage",
        "methodologies",
        "findings",
        "evidence",
    }:
        raise PenetrationEvidenceError("manifest fields do not match the v1 schema")
    if manifest["schemaVersion"] != MANIFEST_SCHEMA:
        raise PenetrationEvidenceError(f"schemaVersion is not {MANIFEST_SCHEMA}")
    try:
        engagement_id = str(uuid.UUID(str(manifest["engagementId"])))
    except (ValueError, AttributeError) as error:
        raise PenetrationEvidenceError("engagementId must be a UUID") from error
    release_commit = manifest["releaseCommit"]
    if not isinstance(release_commit, str) or COMMIT_RE.fullmatch(release_commit) is None:
        raise PenetrationEvidenceError("releaseCommit must be a full 40-character lowercase Git SHA")
    environment_class = manifest["environmentClass"]
    if environment_class not in {"production", "production-like", "staging", "fixture"}:
        raise PenetrationEvidenceError("environmentClass is not allowed")
    environment_id = require_identifier(manifest["environmentId"], "environmentId")
    deployment_profile = require_identifier(manifest["deploymentProfile"], "deploymentProfile")
    started = parse_utc(manifest["startedAt"], "startedAt")
    completed = parse_utc(manifest["completedAt"], "completedAt")
    report_issued = parse_utc(manifest["reportIssuedAt"], "reportIssuedAt")
    if not started < completed <= report_issued:
        raise PenetrationEvidenceError("engagement and report timestamps are out of order")

    assessor = validate_assessor(manifest["assessor"])
    stage5 = validate_stage5_dependency(
        manifest["stage5Dependency"], release_commit, deployment_profile
    )
    assets, asset_coverage_complete = validate_assets(manifest["assets"])
    scope, scope_coverage_complete = validate_scope(manifest["scopeCoverage"])
    methodologies, methodology_coverage_complete = validate_methodologies(manifest["methodologies"])
    findings, finding_counts, no_unaccepted = validate_findings(
        manifest["findings"], started, completed
    )

    evidence_raw = require_mapping(manifest["evidence"], "evidence")
    if set(evidence_raw) != set(EVIDENCE_FIELDS):
        raise PenetrationEvidenceError("evidence fields do not match the v1 schema")
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
    if validated < report_issued:
        raise PenetrationEvidenceError("validatedAt must not precede reportIssuedAt")
    third_party_independence = (
        assessor["thirdParty"] and assessor["independent"] and assessor["noConflictDeclared"]
    )
    release_eligible_environment = environment_class in {"production", "production-like"}
    eligible_for_human_gate_review = all(
        (
            release_eligible_environment,
            third_party_independence,
            stage5["declaredSatisfied"],
            asset_coverage_complete,
            scope_coverage_complete,
            methodology_coverage_complete,
            no_unaccepted,
        )
    )
    return {
        "schemaVersion": RECEIPT_SCHEMA,
        "engagementId": engagement_id,
        "releaseCommit": release_commit,
        "environmentClass": environment_class,
        "environmentId": environment_id,
        "deploymentProfile": deployment_profile,
        "engagement": {
            "startedAt": started.isoformat().replace("+00:00", "Z"),
            "completedAt": completed.isoformat().replace("+00:00", "Z"),
            "reportIssuedAt": report_issued.isoformat().replace("+00:00", "Z"),
        },
        "manifest": {
            "path": normalized_manifest_reference(
                manifest_reference_path or manifest_path.name
            ),
            "sha256": "sha256:" + hashlib.sha256(manifest_bytes).hexdigest(),
        },
        "assessor": assessor,
        "thirdPartyIndependenceDeclared": third_party_independence,
        "stage5Dependency": stage5,
        "assets": assets,
        "assetCoverageComplete": asset_coverage_complete,
        "scopeCoverage": scope,
        "scopeCoverageComplete": scope_coverage_complete,
        "methodologies": methodologies,
        "methodologyCoverageComplete": methodology_coverage_complete,
        "findings": findings,
        "findingCounts": finding_counts,
        "declaredNoUnacceptedHighOrCriticalFindings": no_unaccepted,
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
    except (PenetrationEvidenceError, ImmutableEvidenceIOError) as error:
        parser.exit(2, f"penetration evidence validation failed: {error}\n")
    print(f"wrote {output} and {output.name}.sha256")
    return 0


if __name__ == "__main__":
    sys.exit(main())
