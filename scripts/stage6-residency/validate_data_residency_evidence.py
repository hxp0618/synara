#!/usr/bin/env python3
"""Validate semantically bound Stage 6 data-residency evidence without approving residency."""

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


SCHEMA_VERSION = "synara.data-residency-deployment-evidence.v1"
RECEIPT_SCHEMA_VERSION = "synara.data-residency-deployment-evidence-receipt.v1"
ASSESSMENT = "evidence-validated-not-residency-approved"

COMMIT_RE = re.compile(r"^[0-9a-f]{40}$")
HEX_SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
SHA256_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
IDENTIFIER_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:/-]{1,159}$")
REGION_RE = re.compile(r"^[a-z0-9][a-z0-9._-]{1,63}$")

MAX_MANIFEST_BYTES = 1024 * 1024
MAX_EVIDENCE_FILE_BYTES = 16 * 1024 * 1024
MAX_TOTAL_EVIDENCE_BYTES = 64 * 1024 * 1024
MAX_REGIONS_PER_PLANE = 16
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
PROMISE_SCOPES = {"full-data-residency", "execution-only"}
ARTIFACT_FIELDS = {
    "adminArtifact",
    "controlPlaneImage",
    "desktopArtifacts",
    "providerHostImage",
    "webArtifact",
    "workerImage",
}
DESKTOP_TARGETS = {"linux-x64", "macos-arm64", "macos-x64", "windows-x64"}
PLANE_NAMES = {
    "disasterRecoveryDestinations",
    "incidentBundles",
    "kmsKeyBackups",
    "kmsKeys",
    "logs",
    "metrics",
    "objectStorageBackups",
    "objectStorageDeletionProcessing",
    "objectStoragePrimary",
    "objectStorageReplicas",
    "postgresqlBackups",
    "postgresqlPrimary",
    "postgresqlReplicas",
    "postgresqlRestoreProcessing",
    "providerRequestProcessing",
    "providerRetention",
    "queue",
    "subprocessorProcessing",
    "supportProcessing",
    "traces",
}
EVIDENCE_FIELDS = {
    "annex",
    "browserStatement",
    "evacuationExercise",
    "failoverExercise",
    "operationsApproval",
    "privacyLegalApproval",
    "runtimeInventory",
    "securityApproval",
}
BASE_EVIDENCE_FIELDS = {
    "annex",
    "browserStatement",
    "evacuationExercise",
    "failoverExercise",
    "runtimeInventory",
}
APPROVAL_FIELDS = {
    "operationsApproval": "operations",
    "privacyLegalApproval": "privacyLegal",
    "securityApproval": "security",
}

SCHEMAS = {
    "annex": "synara.data-residency-annex-evidence.v1",
    "browserStatement": "synara.data-residency-browser-statement-evidence.v1",
    "evacuationExercise": "synara.data-residency-exercise-evidence.v1",
    "failoverExercise": "synara.data-residency-exercise-evidence.v1",
    "operationsApproval": "synara.data-residency-approval-evidence.v1",
    "privacyLegalApproval": "synara.data-residency-approval-evidence.v1",
    "runtimeInventory": "synara.data-residency-runtime-inventory-evidence.v1",
    "securityApproval": "synara.data-residency-approval-evidence.v1",
}
EXTERNAL_VERIFICATION_BOUNDARIES = {
    "annex": "signed-annex-required-not-verified",
    "browserStatement": "deployed-browser-origin-required-not-verified",
    "evacuationExercise": "live-exercise-authority-required-not-verified",
    "failoverExercise": "live-exercise-authority-required-not-verified",
    "operationsApproval": "approver-identity-and-signature-required-not-verified",
    "privacyLegalApproval": "approver-identity-and-signature-required-not-verified",
    "runtimeInventory": "deployment-inventory-authority-required-not-verified",
    "securityApproval": "approver-identity-and-signature-required-not-verified",
}


class DataResidencyEvidenceError(Exception):
    pass


def reject_duplicate_json_fields(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise DataResidencyEvidenceError(f"JSON contains duplicate field {key!r}")
        result[key] = value
    return result


def scan_for_secret_material(content: bytes, label: str) -> None:
    for secret_label, pattern in PROHIBITED_SECRET_PATTERNS:
        if pattern.search(content):
            raise DataResidencyEvidenceError(f"{label} contains prohibited {secret_label} material")


def require_mapping(value: Any, label: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise DataResidencyEvidenceError(f"{label} must be an object")
    return value


def require_exact(value: Any, fields: set[str], label: str) -> dict[str, Any]:
    mapping = require_mapping(value, label)
    if set(mapping) != fields:
        raise DataResidencyEvidenceError(f"{label} fields do not match the v1 schema")
    return mapping


def require_identifier(value: Any, label: str) -> str:
    if not isinstance(value, str) or IDENTIFIER_RE.fullmatch(value.strip()) is None:
        raise DataResidencyEvidenceError(f"{label} must be a bounded identifier")
    return value.strip()


def require_region(value: Any, label: str) -> str:
    if not isinstance(value, str) or REGION_RE.fullmatch(value.strip()) is None:
        raise DataResidencyEvidenceError(f"{label} must be a bounded lowercase Region identifier")
    return value.strip()


def require_digest(value: Any, label: str) -> str:
    if not isinstance(value, str) or SHA256_RE.fullmatch(value) is None:
        raise DataResidencyEvidenceError(f"{label} must be sha256:<64 lowercase hex>")
    return value


def require_bool(value: Any, label: str) -> bool:
    if not isinstance(value, bool):
        raise DataResidencyEvidenceError(f"{label} must be a boolean")
    return value


def parse_utc(value: Any, label: str) -> dt.datetime:
    if not isinstance(value, str) or not value.endswith("Z"):
        raise DataResidencyEvidenceError(f"{label} must be an RFC3339 UTC timestamp ending in Z")
    try:
        parsed = dt.datetime.fromisoformat(value[:-1] + "+00:00")
    except ValueError as error:
        raise DataResidencyEvidenceError(f"{label} must be a valid RFC3339 UTC timestamp") from error
    if parsed.tzinfo != dt.timezone.utc:
        raise DataResidencyEvidenceError(f"{label} must use UTC")
    return parsed


def format_utc(value: dt.datetime) -> str:
    return value.isoformat().replace("+00:00", "Z")


def resolve_evidence_file(root: pathlib.Path, raw_path: Any, label: str) -> pathlib.Path:
    if not isinstance(raw_path, str) or not raw_path.strip():
        raise DataResidencyEvidenceError(f"{label}.path must be a non-empty relative path")
    if "\\" in raw_path or "\x00" in raw_path:
        raise DataResidencyEvidenceError(f"{label}.path must use a traversal-free POSIX relative path")
    parts = raw_path.split("/")
    if raw_path.startswith("/") or any(part in {"", ".", ".."} for part in parts):
        raise DataResidencyEvidenceError(f"{label}.path must use a traversal-free POSIX relative path")
    candidate = root.joinpath(*parts)
    current = candidate
    while current != root:
        if current.is_symlink():
            raise DataResidencyEvidenceError(f"{label}.path must not traverse a symlink")
        current = current.parent
    try:
        resolved = candidate.resolve(strict=True)
        resolved.relative_to(root)
    except (OSError, ValueError) as error:
        raise DataResidencyEvidenceError(f"{label}.path must resolve inside the evidence root") from error
    if not resolved.is_file():
        raise DataResidencyEvidenceError(f"{label}.path must reference a regular file")
    return resolved


def load_evidence_document(
    value: Any,
    root: pathlib.Path,
    label: str,
    used_paths: set[pathlib.Path],
    total_bytes: list[int],
) -> tuple[dict[str, str], dict[str, Any]]:
    reference = require_exact(value, {"path", "sha256"}, label)
    path = resolve_evidence_file(root, reference["path"], label)
    if path in used_paths:
        raise DataResidencyEvidenceError(f"{label}.path duplicates another evidence file")
    used_paths.add(path)
    try:
        content = read_stable_regular_file(
            path,
            label=label,
            maximum_bytes=MAX_EVIDENCE_FILE_BYTES,
        )
    except ImmutableEvidenceIOError as error:
        raise DataResidencyEvidenceError(
            f"{label}.path exceeds the evidence file limit or is not a stable regular file"
        ) from error
    total_bytes[0] += len(content)
    if total_bytes[0] > MAX_TOTAL_EVIDENCE_BYTES:
        raise DataResidencyEvidenceError(
            f"evidence files exceed the {MAX_TOTAL_EVIDENCE_BYTES}-byte aggregate limit"
        )
    declared = require_digest(reference["sha256"], f"{label}.sha256")
    scan_for_secret_material(content, label)
    actual = "sha256:" + hashlib.sha256(content).hexdigest()
    if declared != actual:
        raise DataResidencyEvidenceError(f"{label}.sha256 does not match the evidence file")
    try:
        document = json.loads(content, object_pairs_hook=reject_duplicate_json_fields)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise DataResidencyEvidenceError(f"{label}.path must contain valid UTF-8 JSON") from error
    return {"path": path.relative_to(root).as_posix(), "sha256": actual}, require_mapping(
        document, f"{label} document"
    )


def validate_sorted_regions(value: Any, label: str, allow_empty: bool = False) -> list[str]:
    if not isinstance(value, list) or (not allow_empty and not value):
        qualifier = "a list" if allow_empty else "a non-empty list"
        raise DataResidencyEvidenceError(f"{label} must be {qualifier} of Regions")
    if len(value) > MAX_REGIONS_PER_PLANE:
        raise DataResidencyEvidenceError(
            f"{label} exceeds the {MAX_REGIONS_PER_PLANE}-Region limit"
        )
    result = [require_region(raw, f"{label}[{index}]") for index, raw in enumerate(value)]
    if result != sorted(result) or len(set(result)) != len(result):
        raise DataResidencyEvidenceError(f"{label} must be sorted and contain no duplicates")
    return result


def validate_candidate(value: Any, label: str) -> dict[str, Any]:
    candidate = require_exact(
        value,
        {
            "artifacts",
            "candidateId",
            "environmentClass",
            "environmentId",
            "lockfileSha256",
            "migrationTail",
            "sourceCommit",
        },
        label,
    )
    source_commit = candidate["sourceCommit"]
    if not isinstance(source_commit, str) or COMMIT_RE.fullmatch(source_commit) is None:
        raise DataResidencyEvidenceError(f"{label}.sourceCommit must be a full lowercase Git SHA")
    lockfile = candidate["lockfileSha256"]
    if not isinstance(lockfile, str) or HEX_SHA256_RE.fullmatch(lockfile) is None:
        raise DataResidencyEvidenceError(f"{label}.lockfileSha256 must be 64 lowercase hex")
    environment_class = candidate["environmentClass"]
    if environment_class not in ENVIRONMENT_CLASSES:
        raise DataResidencyEvidenceError(f"{label}.environmentClass is not allowed")
    artifacts = require_exact(candidate["artifacts"], ARTIFACT_FIELDS, f"{label}.artifacts")
    normalized_artifacts = {
        name: require_digest(artifacts[name], f"{label}.artifacts.{name}")
        for name in (
            "adminArtifact",
            "controlPlaneImage",
            "providerHostImage",
            "webArtifact",
            "workerImage",
        )
    }
    desktop = require_exact(
        artifacts["desktopArtifacts"], DESKTOP_TARGETS, f"{label}.artifacts.desktopArtifacts"
    )
    normalized_artifacts["desktopArtifacts"] = {
        target: require_digest(desktop[target], f"{label}.artifacts.desktopArtifacts.{target}")
        for target in sorted(DESKTOP_TARGETS)
    }
    migration = require_exact(
        candidate["migrationTail"], {"name", "sha256"}, f"{label}.migrationTail"
    )
    return {
        "candidateId": require_identifier(candidate["candidateId"], f"{label}.candidateId"),
        "sourceCommit": source_commit,
        "lockfileSha256": lockfile,
        "environmentClass": environment_class,
        "environmentId": require_identifier(
            candidate["environmentId"], f"{label}.environmentId"
        ),
        "artifacts": normalized_artifacts,
        "migrationTail": {
            "name": require_identifier(migration["name"], f"{label}.migrationTail.name"),
            "sha256": require_digest(migration["sha256"], f"{label}.migrationTail.sha256"),
        },
    }


def validate_policy(value: Any, label: str) -> dict[str, Any]:
    policy = require_exact(
        value,
        {
            "allowedRegions",
            "digest",
            "enforcementState",
            "homeRegion",
            "statementType",
            "version",
        },
        label,
    )
    version = policy["version"]
    if isinstance(version, bool) or not isinstance(version, int) or version < 1:
        raise DataResidencyEvidenceError(f"{label}.version must be an integer >= 1")
    if policy["statementType"] != "synara-data-residency-statement-v1":
        raise DataResidencyEvidenceError(
            f"{label}.statementType must be synara-data-residency-statement-v1"
        )
    enforcement = policy["enforcementState"]
    if enforcement not in {"restricted", "unrestricted"}:
        raise DataResidencyEvidenceError(f"{label}.enforcementState is not allowed")
    allowed = validate_sorted_regions(
        policy["allowedRegions"],
        f"{label}.allowedRegions",
        allow_empty=enforcement == "unrestricted",
    )
    return {
        "statementType": policy["statementType"],
        "version": version,
        "digest": require_digest(policy["digest"], f"{label}.digest"),
        "homeRegion": require_region(policy["homeRegion"], f"{label}.homeRegion"),
        "allowedRegions": allowed,
        "enforcementState": enforcement,
    }


def validate_subject(value: Any, label: str) -> dict[str, Any]:
    subject = require_exact(value, {"candidate", "policy", "tenantId"}, label)
    return {
        "tenantId": require_identifier(subject["tenantId"], f"{label}.tenantId"),
        "candidate": validate_candidate(subject["candidate"], f"{label}.candidate"),
        "policy": validate_policy(subject["policy"], f"{label}.policy"),
    }


def require_matching_subject(
    document: dict[str, Any],
    evidence_name: str,
    canonical_subject: dict[str, Any] | None,
) -> dict[str, Any]:
    if document.get("schemaVersion") != SCHEMAS[evidence_name]:
        raise DataResidencyEvidenceError(
            f"evidence.{evidence_name} schemaVersion is not {SCHEMAS[evidence_name]}"
        )
    subject = validate_subject(document.get("subject"), f"evidence.{evidence_name}.subject")
    if canonical_subject is not None and subject != canonical_subject:
        raise DataResidencyEvidenceError(
            f"evidence.{evidence_name}.subject does not match the Annex subject"
        )
    return subject


def validate_verification_boundary(value: Any, evidence_name: str) -> dict[str, str]:
    label = f"evidence.{evidence_name}.verificationBoundary"
    boundary = require_exact(value, {"contentValidation", "externalVerification"}, label)
    if boundary["contentValidation"] != "strict-schema-and-subject-validated":
        raise DataResidencyEvidenceError(f"{label}.contentValidation is not allowed")
    expected = EXTERNAL_VERIFICATION_BOUNDARIES[evidence_name]
    if boundary["externalVerification"] != expected:
        raise DataResidencyEvidenceError(
            f"{label}.externalVerification must be {expected}"
        )
    return {
        "contentValidation": boundary["contentValidation"],
        "externalVerification": boundary["externalVerification"],
    }


def validate_inventory(
    value: Any,
    allowed_regions: set[str],
    label: str,
) -> tuple[dict[str, Any], bool]:
    inventory = require_exact(value, PLANE_NAMES, label)
    result: dict[str, Any] = {}
    all_known_and_allowed = True
    for name in sorted(PLANE_NAMES):
        plane = require_exact(
            inventory[name], {"disclosed", "regions", "status"}, f"{label}.{name}"
        )
        status = plane["status"]
        if status not in {"known", "unknown", "global"}:
            raise DataResidencyEvidenceError(f"{label}.{name}.status is not allowed")
        regions = validate_sorted_regions(
            plane["regions"], f"{label}.{name}.regions", allow_empty=status != "known"
        )
        if (status == "known") != bool(regions):
            raise DataResidencyEvidenceError(
                f"{label}.{name}.regions must be non-empty only when status is known"
            )
        disclosed = require_bool(plane["disclosed"], f"{label}.{name}.disclosed")
        within = status == "known" and set(regions).issubset(allowed_regions)
        result[name] = {
            "status": status,
            "regions": regions,
            "disclosed": disclosed,
            "withinAllowedRegions": within,
        }
        all_known_and_allowed = all_known_and_allowed and within and disclosed
    return result, all_known_and_allowed


def strip_inventory_checks(inventory: dict[str, Any]) -> dict[str, Any]:
    return {
        name: {
            "status": value["status"],
            "regions": value["regions"],
            "disclosed": value["disclosed"],
        }
        for name, value in inventory.items()
    }


def validate_annex(
    document: dict[str, Any],
) -> tuple[dict[str, Any], dict[str, Any], dict[str, Any], bool]:
    require_exact(
        document,
        {"annex", "processingPlanes", "schemaVersion", "subject", "verificationBoundary"},
        "evidence.annex document",
    )
    subject = require_matching_subject(document, "annex", None)
    annex = require_exact(
        document["annex"],
        {
            "annexId",
            "disasterRecoveryOptInRequired",
            "effectiveAt",
            "noAllowedDestinationBehavior",
            "promiseScope",
            "reviewExpiresAt",
        },
        "evidence.annex.annex",
    )
    promise_scope = annex["promiseScope"]
    if promise_scope not in PROMISE_SCOPES:
        raise DataResidencyEvidenceError("evidence.annex.annex.promiseScope is not allowed")
    if annex["noAllowedDestinationBehavior"] != "fail-closed-no-placement":
        raise DataResidencyEvidenceError(
            "evidence.annex.annex.noAllowedDestinationBehavior must be fail-closed-no-placement"
        )
    opt_in = require_bool(
        annex["disasterRecoveryOptInRequired"],
        "evidence.annex.annex.disasterRecoveryOptInRequired",
    )
    normalized_annex = {
        "annexId": require_identifier(annex["annexId"], "evidence.annex.annex.annexId"),
        "promiseScope": promise_scope,
        "effectiveAt": format_utc(parse_utc(annex["effectiveAt"], "annex.effectiveAt")),
        "reviewExpiresAt": format_utc(
            parse_utc(annex["reviewExpiresAt"], "annex.reviewExpiresAt")
        ),
        "disasterRecoveryOptInRequired": opt_in,
        "noAllowedDestinationBehavior": annex["noAllowedDestinationBehavior"],
    }
    inventory, inventory_eligible = validate_inventory(
        document["processingPlanes"],
        set(subject["policy"]["allowedRegions"]),
        "evidence.annex.processingPlanes",
    )
    validate_verification_boundary(document["verificationBoundary"], "annex")
    return subject, normalized_annex, inventory, inventory_eligible


def validate_runtime_inventory(
    document: dict[str, Any],
    subject: dict[str, Any],
    annex_inventory: dict[str, Any],
) -> tuple[dict[str, Any], dict[str, Any]]:
    require_exact(
        document,
        {
            "inventory",
            "processingPlanes",
            "schemaVersion",
            "subject",
            "verificationBoundary",
        },
        "evidence.runtimeInventory document",
    )
    require_matching_subject(document, "runtimeInventory", subject)
    inventory_meta = require_exact(
        document["inventory"],
        {"capturedAt", "inventoryId", "sourceAuthority", "validUntil"},
        "evidence.runtimeInventory.inventory",
    )
    runtime_inventory, _ = validate_inventory(
        document["processingPlanes"],
        set(subject["policy"]["allowedRegions"]),
        "evidence.runtimeInventory.processingPlanes",
    )
    if strip_inventory_checks(runtime_inventory) != strip_inventory_checks(annex_inventory):
        raise DataResidencyEvidenceError(
            "runtime inventory processing planes do not match the signed Annex declaration"
        )
    validate_verification_boundary(document["verificationBoundary"], "runtimeInventory")
    return {
        "inventoryId": require_identifier(
            inventory_meta["inventoryId"], "runtimeInventory.inventoryId"
        ),
        "sourceAuthority": require_identifier(
            inventory_meta["sourceAuthority"], "runtimeInventory.sourceAuthority"
        ),
        "capturedAt": format_utc(
            parse_utc(inventory_meta["capturedAt"], "runtimeInventory.capturedAt")
        ),
        "validUntil": format_utc(
            parse_utc(inventory_meta["validUntil"], "runtimeInventory.validUntil")
        ),
    }, runtime_inventory


def validate_browser_statement(
    document: dict[str, Any],
    subject: dict[str, Any],
) -> dict[str, Any]:
    require_exact(
        document,
        {"generatedAt", "schemaVersion", "statement", "subject", "surface", "verificationBoundary"},
        "evidence.browserStatement document",
    )
    require_matching_subject(document, "browserStatement", subject)
    if document["surface"] != "tenant-settings/data-residency":
        raise DataResidencyEvidenceError("browserStatement.surface is not allowed")
    statement = require_exact(
        document["statement"],
        {
            "allowedRegions",
            "enforcementState",
            "homeRegion",
            "policyDigest",
            "policyVersion",
            "statementType",
            "tenantId",
        },
        "evidence.browserStatement.statement",
    )
    expected_statement = {
        "statementType": subject["policy"]["statementType"],
        "tenantId": subject["tenantId"],
        "homeRegion": subject["policy"]["homeRegion"],
        "policyVersion": subject["policy"]["version"],
        "policyDigest": subject["policy"]["digest"],
        "enforcementState": subject["policy"]["enforcementState"],
        "allowedRegions": subject["policy"]["allowedRegions"],
    }
    if statement != expected_statement:
        raise DataResidencyEvidenceError(
            "browser statement content does not match the common Tenant policy subject"
        )
    validate_verification_boundary(document["verificationBoundary"], "browserStatement")
    return {
        "generatedAt": format_utc(
            parse_utc(document["generatedAt"], "browserStatement.generatedAt")
        ),
        "surface": document["surface"],
        "statement": expected_statement,
    }


def validate_exercise(
    document: dict[str, Any],
    evidence_name: str,
    expected_kind: str,
    subject: dict[str, Any],
    annex_digest: str,
    runtime_digest: str,
    dr_regions: set[str],
) -> tuple[dict[str, Any], bool]:
    require_exact(
        document,
        {"exercise", "schemaVersion", "subject", "verificationBoundary"},
        f"evidence.{evidence_name} document",
    )
    require_matching_subject(document, evidence_name, subject)
    exercise = require_exact(
        document["exercise"],
        {
            "annexSha256",
            "executedAt",
            "exerciseId",
            "exerciseKind",
            "outOfPolicyProbe",
            "result",
            "runtimeInventorySha256",
            "selectedDestinationRegion",
            "sourceRegion",
        },
        f"evidence.{evidence_name}.exercise",
    )
    if exercise["exerciseKind"] != expected_kind:
        raise DataResidencyEvidenceError(
            f"evidence.{evidence_name}.exercise.exerciseKind must be {expected_kind}"
        )
    if require_digest(exercise["annexSha256"], f"{evidence_name}.annexSha256") != annex_digest:
        raise DataResidencyEvidenceError(f"{evidence_name} does not bind the exact Annex file")
    if (
        require_digest(exercise["runtimeInventorySha256"], f"{evidence_name}.runtimeInventorySha256")
        != runtime_digest
    ):
        raise DataResidencyEvidenceError(
            f"{evidence_name} does not bind the exact runtime inventory file"
        )
    source = require_region(exercise["sourceRegion"], f"{evidence_name}.sourceRegion")
    selected = require_region(
        exercise["selectedDestinationRegion"], f"{evidence_name}.selectedDestinationRegion"
    )
    if source == selected:
        raise DataResidencyEvidenceError(f"{evidence_name} destination must differ from source")
    result = exercise["result"]
    if result not in {"passed", "failed"}:
        raise DataResidencyEvidenceError(f"{evidence_name}.result is not allowed")
    probe = require_exact(
        exercise["outOfPolicyProbe"],
        {"destinationRegion", "selectionRejected"},
        f"evidence.{evidence_name}.exercise.outOfPolicyProbe",
    )
    probe_region = require_region(probe["destinationRegion"], f"{evidence_name}.probeRegion")
    allowed = set(subject["policy"]["allowedRegions"])
    if probe_region in allowed:
        raise DataResidencyEvidenceError(
            f"{evidence_name} out-of-policy probe destination is inside allowed Regions"
        )
    probe_rejected = require_bool(
        probe["selectionRejected"], f"{evidence_name}.selectionRejected"
    )
    source_allowed = source in allowed
    destination_allowed = selected in allowed
    destination_in_inventory = selected in dr_regions
    satisfied = (
        result == "passed"
        and source_allowed
        and destination_allowed
        and destination_in_inventory
        and probe_rejected
    )
    validate_verification_boundary(document["verificationBoundary"], evidence_name)
    return {
        "exerciseId": require_identifier(exercise["exerciseId"], f"{evidence_name}.exerciseId"),
        "exerciseKind": expected_kind,
        "executedAt": format_utc(parse_utc(exercise["executedAt"], f"{evidence_name}.executedAt")),
        "sourceRegion": source,
        "selectedDestinationRegion": selected,
        "result": result,
        "sourceAllowed": source_allowed,
        "selectedDestinationAllowed": destination_allowed,
        "selectedDestinationInRuntimeInventory": destination_in_inventory,
        "outOfPolicyProbe": {
            "destinationRegion": probe_region,
            "selectionRejected": probe_rejected,
        },
        "declaredAcceptanceSatisfied": satisfied,
    }, satisfied


def validate_approval(
    document: dict[str, Any],
    evidence_name: str,
    expected_role: str,
    subject: dict[str, Any],
    expected_subjects: dict[str, str],
) -> dict[str, Any]:
    require_exact(
        document,
        {"approval", "schemaVersion", "subject", "verificationBoundary"},
        f"evidence.{evidence_name} document",
    )
    require_matching_subject(document, evidence_name, subject)
    approval = require_exact(
        document["approval"],
        {"approvedAt", "approverId", "decision", "evidenceSubjects", "expiresAt", "role"},
        f"evidence.{evidence_name}.approval",
    )
    if approval["role"] != expected_role:
        raise DataResidencyEvidenceError(
            f"evidence.{evidence_name}.approval.role must be {expected_role}"
        )
    if approval["decision"] != "approved-for-human-review":
        raise DataResidencyEvidenceError(
            f"evidence.{evidence_name}.approval.decision is not allowed"
        )
    evidence_subjects = require_exact(
        approval["evidenceSubjects"], BASE_EVIDENCE_FIELDS, f"{evidence_name}.evidenceSubjects"
    )
    normalized_subjects = {
        name: require_digest(evidence_subjects[name], f"{evidence_name}.evidenceSubjects.{name}")
        for name in sorted(BASE_EVIDENCE_FIELDS)
    }
    if normalized_subjects != expected_subjects:
        raise DataResidencyEvidenceError(
            f"evidence.{evidence_name} approval does not bind the exact evidence set"
        )
    validate_verification_boundary(document["verificationBoundary"], evidence_name)
    return {
        "role": expected_role,
        "approverId": require_identifier(
            approval["approverId"], f"evidence.{evidence_name}.approval.approverId"
        ),
        "decision": approval["decision"],
        "approvedAt": format_utc(
            parse_utc(approval["approvedAt"], f"evidence.{evidence_name}.approval.approvedAt")
        ),
        "expiresAt": format_utc(
            parse_utc(approval["expiresAt"], f"evidence.{evidence_name}.approval.expiresAt")
        ),
        "evidenceSubjects": normalized_subjects,
    }


def evidence_set_digest(references: dict[str, dict[str, str]]) -> str:
    encoded = "".join(
        f"{name}={references[name]['sha256']}\n" for name in sorted(EVIDENCE_FIELDS)
    ).encode("utf-8")
    return "sha256:" + hashlib.sha256(encoded).hexdigest()


def normalized_manifest_reference(value: str) -> str:
    path = pathlib.PurePosixPath(value)
    if (
        path.is_absolute()
        or not path.parts
        or any(part in {"", ".", ".."} for part in path.parts)
        or path.as_posix() != value
    ):
        raise DataResidencyEvidenceError("manifest receipt path must be normalized and relative")
    return value


def validate(
    manifest_path: pathlib.Path,
    evidence_root: pathlib.Path,
    validated_at: str | None,
    expected_candidate_binding_sha256: str | None = None,
    manifest_reference_path: str | None = None,
) -> dict[str, Any]:
    root = evidence_root.resolve()
    if evidence_root.is_symlink() or not root.is_dir():
        raise DataResidencyEvidenceError("evidence root must be a regular non-symlink directory")
    try:
        manifest_bytes = read_stable_regular_file(
            manifest_path,
            label="data residency manifest",
            maximum_bytes=MAX_MANIFEST_BYTES,
        )
        scan_for_secret_material(manifest_bytes, "data residency manifest")
        raw = json.loads(manifest_bytes, object_pairs_hook=reject_duplicate_json_fields)
    except ImmutableEvidenceIOError as error:
        raise DataResidencyEvidenceError(str(error)) from error
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise DataResidencyEvidenceError("manifest must be valid JSON") from error
    manifest = require_exact(raw, {"evidence", "schemaVersion"}, "manifest")
    if manifest["schemaVersion"] != SCHEMA_VERSION:
        raise DataResidencyEvidenceError(f"schemaVersion is not {SCHEMA_VERSION}")
    raw_evidence = require_exact(manifest["evidence"], EVIDENCE_FIELDS, "manifest.evidence")

    used_paths: set[pathlib.Path] = set()
    total_bytes = [0]
    references: dict[str, dict[str, str]] = {}
    documents: dict[str, dict[str, Any]] = {}
    for name in sorted(EVIDENCE_FIELDS):
        references[name], documents[name] = load_evidence_document(
            raw_evidence[name], root, f"evidence.{name}", used_paths, total_bytes
        )

    subject, annex, annex_inventory, inventory_eligible = validate_annex(documents["annex"])
    candidate = subject["candidate"]
    policy = subject["policy"]
    candidate_binding = "sha256:" + hashlib.sha256(
        json.dumps(candidate, separators=(",", ":"), sort_keys=True).encode("utf-8")
    ).hexdigest()
    if expected_candidate_binding_sha256 is not None:
        expected_binding = require_digest(
            expected_candidate_binding_sha256, "expected candidate binding"
        )
        if candidate_binding != expected_binding:
            raise DataResidencyEvidenceError(
                "candidate binding does not match the expected candidate; candidate drift detected"
            )

    runtime, runtime_inventory = validate_runtime_inventory(
        documents["runtimeInventory"], subject, annex_inventory
    )
    browser = validate_browser_statement(documents["browserStatement"], subject)
    dr_regions = (
        set(runtime_inventory["disasterRecoveryDestinations"]["regions"])
        if runtime_inventory["disasterRecoveryDestinations"]["status"] == "known"
        else set()
    )
    failover, failover_satisfied = validate_exercise(
        documents["failoverExercise"],
        "failoverExercise",
        "failover",
        subject,
        references["annex"]["sha256"],
        references["runtimeInventory"]["sha256"],
        dr_regions,
    )
    evacuation, evacuation_satisfied = validate_exercise(
        documents["evacuationExercise"],
        "evacuationExercise",
        "evacuation",
        subject,
        references["annex"]["sha256"],
        references["runtimeInventory"]["sha256"],
        dr_regions,
    )
    if failover["exerciseId"] == evacuation["exerciseId"]:
        raise DataResidencyEvidenceError("failover and evacuation require distinct exerciseId values")

    base_subjects = {
        name: references[name]["sha256"] for name in sorted(BASE_EVIDENCE_FIELDS)
    }
    approvals = {
        role: validate_approval(
            documents[evidence_name], evidence_name, role, subject, base_subjects
        )
        for evidence_name, role in sorted(APPROVAL_FIELDS.items())
    }
    approver_ids = [approval["approverId"] for approval in approvals.values()]
    if len(set(approver_ids)) != len(approver_ids):
        raise DataResidencyEvidenceError("approval roles require three distinct approvers")

    validation_text = validated_at or format_utc(dt.datetime.now(dt.timezone.utc))
    validation_time = parse_utc(validation_text, "validatedAt")
    effective_at = parse_utc(annex["effectiveAt"], "annex.effectiveAt")
    review_expires_at = parse_utc(annex["reviewExpiresAt"], "annex.reviewExpiresAt")
    if not (effective_at <= validation_time < review_expires_at):
        raise DataResidencyEvidenceError(
            "validatedAt must be on or after annex.effectiveAt and before annex.reviewExpiresAt"
        )
    if review_expires_at - effective_at > dt.timedelta(days=366):
        raise DataResidencyEvidenceError("annex review validity must not exceed 366 days")

    runtime_captured = parse_utc(runtime["capturedAt"], "runtimeInventory.capturedAt")
    runtime_valid_until = parse_utc(runtime["validUntil"], "runtimeInventory.validUntil")
    if not (runtime_captured < validation_time <= runtime_valid_until):
        raise DataResidencyEvidenceError(
            "runtime inventory must be captured before validation and remain valid through validation"
        )
    failover_at = parse_utc(failover["executedAt"], "failover.executedAt")
    evacuation_at = parse_utc(evacuation["executedAt"], "evacuation.executedAt")
    browser_at = parse_utc(browser["generatedAt"], "browserStatement.generatedAt")
    for label, observed in (
        ("failover", failover_at),
        ("evacuation", evacuation_at),
        ("browser statement", browser_at),
    ):
        if not (runtime_captured <= observed <= runtime_valid_until):
            raise DataResidencyEvidenceError(
                f"{label} must fall inside the runtime inventory validity window"
            )
        if observed > effective_at or effective_at - observed > dt.timedelta(days=90):
            raise DataResidencyEvidenceError(
                f"{label} must be produced within 90 days before annex.effectiveAt"
            )

    latest_base_evidence = max(runtime_captured, failover_at, evacuation_at, browser_at)
    approvals_current = True
    for role, approval in approvals.items():
        approved_at = parse_utc(approval["approvedAt"], f"approvals.{role}.approvedAt")
        expires_at = parse_utc(approval["expiresAt"], f"approvals.{role}.expiresAt")
        if not (latest_base_evidence <= approved_at <= effective_at < expires_at):
            raise DataResidencyEvidenceError(f"approvals.{role} timestamps are out of order")
        current = validation_time < expires_at and review_expires_at <= expires_at
        approval["currentThroughAnnexReview"] = current
        approvals_current = approvals_current and current

    production_like = candidate["environmentClass"] in {"production", "production-like"}
    restricted_policy = policy["enforcementState"] == "restricted"
    home_allowed = policy["homeRegion"] in policy["allowedRegions"]
    eligible = (
        annex["promiseScope"] == "full-data-residency"
        and production_like
        and restricted_policy
        and home_allowed
        and annex["disasterRecoveryOptInRequired"]
        and inventory_eligible
        and failover_satisfied
        and evacuation_satisfied
        and approvals_current
    )
    receipt_policy = {**policy, "homeRegionAllowed": home_allowed}
    return {
        "schemaVersion": RECEIPT_SCHEMA_VERSION,
        "candidate": candidate,
        "candidateBindingSha256": candidate_binding,
        "tenantId": subject["tenantId"],
        "promiseScope": annex["promiseScope"],
        "annex": annex,
        "policy": receipt_policy,
        "inventory": runtime_inventory,
        "runtimeInventory": runtime,
        "browserStatement": browser,
        "exercises": {"evacuation": evacuation, "failover": failover},
        "approvals": approvals,
        "evidence": references,
        "evidenceSetSha256": evidence_set_digest(references),
        "manifest": {
            "path": normalized_manifest_reference(
                manifest_reference_path or manifest_path.name
            ),
            "sha256": "sha256:" + hashlib.sha256(manifest_bytes).hexdigest(),
        },
        "verificationBoundary": {
            "strictAttachmentContentAndSubjectValidated": True,
            "cryptographicSignaturesVerified": False,
            "externalSignatureIdentityAndAuthorityVerificationRequired": True,
        },
        "checks": {
            "approvalsCurrentThroughReview": approvals_current,
            "attachmentsShareExactSubject": True,
            "environmentEligible": production_like,
            "evacuationAcceptanceSatisfied": evacuation_satisfied,
            "failoverAcceptanceSatisfied": failover_satisfied,
            "fullInventoryKnownDisclosedAndAllowed": inventory_eligible,
            "homeRegionAllowed": home_allowed,
            "restrictedPolicy": restricted_policy,
            "runtimeInventoryMatchesAnnex": True,
        },
        "eligibleForHumanGateReview": eligible,
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
        )
        output = pathlib.Path(args.output).absolute()
        encoded = (json.dumps(receipt, indent=2, sort_keys=True) + "\n").encode("utf-8")
        publish_immutable_with_sha256(output, encoded)
    except (DataResidencyEvidenceError, ImmutableEvidenceIOError) as error:
        parser.exit(2, f"data residency evidence validation failed: {error}\n")
    print(f"wrote {output} and {output.name}.sha256")
    return 0


if __name__ == "__main__":
    sys.exit(main())
