#!/usr/bin/env python3
"""Validate exact-candidate internal usage and cost evidence without payment semantics."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import pathlib
import re
import sys
import urllib.parse
from typing import Any

SCRIPTS_ROOT = pathlib.Path(__file__).resolve().parents[1]
if str(SCRIPTS_ROOT) not in sys.path:
    sys.path.insert(0, str(SCRIPTS_ROOT))

from stage6_common.immutable_evidence_io import (  # noqa: E402
    ImmutableEvidenceIOError,
    publish_immutable_with_sha256,
    read_stable_regular_file,
)

MANIFEST_SCHEMA = "synara.stage6-internal-cost-evidence.v1"
RECEIPT_SCHEMA = "synara.stage6-internal-cost-evidence-validation.v1"
ASSESSMENT = "evidence-validated-not-internal-cost-approved"
ENVIRONMENTS = {"production", "production-like"}
EVIDENCE_IDS = {
    "usage-export",
    "provider-cost-export",
    "platform-allocation-export",
    "reconciliation-report",
}
FORBIDDEN_PAYMENT_KEYS = {
    "stripe",
    "payment",
    "checkout",
    "portal",
    "invoice",
    "tax",
    "card",
    "settlement",
    "subscription",
}
COMMIT_RE = re.compile(r"^[0-9a-f]{40}$")
SHA256_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
MIGRATION_RE = re.compile(r"^\d{6}_[a-z0-9_]+\.sql$")
IDENTIFIER_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:/@+-]{1,199}$")
CURRENCY_RE = re.compile(r"^[A-Z]{3}$")
MAX_MANIFEST_BYTES = 2 * 1024 * 1024
MAX_EVIDENCE_BYTES = 32 * 1024 * 1024
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


class CostEvidenceError(Exception):
    pass


def reject_duplicate_json_fields(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise CostEvidenceError(f"manifest contains duplicate JSON field {key!r}")
        result[key] = value
    return result


def scan_for_secret_material(content: bytes, label: str) -> None:
    for secret_label, pattern in PROHIBITED_SECRET_PATTERNS:
        if pattern.search(content):
            raise CostEvidenceError(f"{label} contains prohibited {secret_label} material")


def exact_keys(value: Any, expected: set[str], label: str) -> dict[str, Any]:
    if not isinstance(value, dict) or set(value) != expected:
        raise CostEvidenceError(f"{label} must contain exactly {sorted(expected)}")
    return value


def require_bool(value: Any, label: str) -> bool:
    if not isinstance(value, bool):
        raise CostEvidenceError(f"{label} must be boolean")
    return value


def require_int(value: Any, label: str, *, minimum: int = 0) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value < minimum:
        raise CostEvidenceError(f"{label} must be an integer >= {minimum}")
    return value


def require_string(value: Any, label: str, pattern: re.Pattern[str] | None = None) -> str:
    if not isinstance(value, str) or not value:
        raise CostEvidenceError(f"{label} must be a non-empty string")
    if pattern is not None and pattern.fullmatch(value) is None:
        raise CostEvidenceError(f"{label} has an invalid format")
    return value


def parse_utc(value: Any, label: str) -> dt.datetime:
    text = require_string(value, label)
    if not text.endswith("Z"):
        raise CostEvidenceError(f"{label} must be UTC")
    try:
        parsed = dt.datetime.fromisoformat(text[:-1] + "+00:00")
    except ValueError as error:
        raise CostEvidenceError(f"{label} must be RFC3339") from error
    return parsed


def require_https(value: Any, label: str) -> str:
    text = require_string(value, label)
    parsed = urllib.parse.urlsplit(text)
    if (
        parsed.scheme != "https"
        or not parsed.hostname
        or parsed.username
        or parsed.password
        or parsed.query
        or parsed.fragment
    ):
        raise CostEvidenceError(f"{label} must be a credential-free HTTPS URL")
    return text.rstrip("/")


def read_regular_file(path: pathlib.Path, maximum: int, label: str) -> bytes:
    try:
        data = read_stable_regular_file(path, label=label, maximum_bytes=maximum)
    except ImmutableEvidenceIOError as error:
        raise CostEvidenceError(str(error)) from error
    scan_for_secret_material(data, label)
    return data


def safe_evidence_path(root: pathlib.Path, value: Any, label: str) -> tuple[str, pathlib.Path]:
    relative = require_string(value, label)
    candidate = pathlib.PurePosixPath(relative)
    if (
        candidate.is_absolute()
        or not candidate.parts
        or any(part in {"", ".", ".."} for part in candidate.parts)
        or candidate.as_posix() != relative
        or "\\" in relative
        or "\x00" in relative
    ):
        raise CostEvidenceError(f"{label} must be a normalized relative path")
    resolved_root = root.resolve()
    supplied = resolved_root.joinpath(*candidate.parts)
    current = supplied
    while current != resolved_root:
        if current.is_symlink():
            raise CostEvidenceError(f"{label} must not traverse a symlink")
        current = current.parent
    try:
        resolved = supplied.resolve(strict=True)
    except OSError as error:
        raise CostEvidenceError(f"{label} does not exist") from error
    if resolved.parent != resolved_root and resolved_root not in resolved.parents:
        raise CostEvidenceError(f"{label} escapes the evidence root")
    return relative, resolved


def normalized_manifest_reference(value: str) -> str:
    candidate = pathlib.PurePosixPath(value)
    if (
        candidate.is_absolute()
        or not candidate.parts
        or any(part in {"", ".", ".."} for part in candidate.parts)
        or candidate.as_posix() != value
    ):
        raise CostEvidenceError("manifest receipt path must be normalized and relative")
    return value


def validate_money_map(value: Any, label: str) -> dict[str, int]:
    if not isinstance(value, dict) or not value:
        raise CostEvidenceError(f"{label} must be a non-empty currency map")
    result: dict[str, int] = {}
    for currency, amount in value.items():
        require_string(currency, f"{label} currency", CURRENCY_RE)
        result[currency] = require_int(amount, f"{label}.{currency}")
    return dict(sorted(result.items()))


def reject_payment_semantics(value: Any, path: str = "manifest") -> None:
    if isinstance(value, dict):
        for key, child in value.items():
            normalized = re.sub(r"[^a-z]", "", str(key).lower())
            allowed_absence_assertion = path == "manifest.controls" and key == "noPaymentDataPresent"
            if not allowed_absence_assertion and any(
                token in normalized for token in FORBIDDEN_PAYMENT_KEYS
            ):
                raise CostEvidenceError(f"{path}.{key} uses forbidden payment semantics")
            reject_payment_semantics(child, f"{path}.{key}")
    elif isinstance(value, list):
        for index, child in enumerate(value):
            reject_payment_semantics(child, f"{path}[{index}]")


def validate(
    manifest_path: pathlib.Path,
    evidence_root: pathlib.Path,
    validated_at: str | None = None,
    manifest_reference_path: str | None = None,
) -> dict[str, Any]:
    manifest_bytes = read_regular_file(manifest_path, MAX_MANIFEST_BYTES, "manifest")
    try:
        manifest = json.loads(manifest_bytes, object_pairs_hook=reject_duplicate_json_fields)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise CostEvidenceError("manifest must be UTF-8 JSON") from error
    exact_keys(
        manifest,
        {"schemaVersion", "candidate", "period", "usage", "costs", "controls", "evidence"},
        "manifest",
    )
    reject_payment_semantics(manifest)
    if manifest["schemaVersion"] != MANIFEST_SCHEMA:
        raise CostEvidenceError("manifest schemaVersion is unsupported")

    candidate = exact_keys(
        manifest["candidate"],
        {
            "candidateId",
            "sourceCommit",
            "environment",
            "environmentId",
            "controlPlaneBaseUrl",
            "migrationTail",
        },
        "candidate",
    )
    require_string(candidate["candidateId"], "candidate.candidateId", IDENTIFIER_RE)
    require_string(candidate["sourceCommit"], "candidate.sourceCommit", COMMIT_RE)
    if candidate["environment"] not in ENVIRONMENTS:
        raise CostEvidenceError("candidate.environment must be production or production-like")
    require_string(candidate["environmentId"], "candidate.environmentId", IDENTIFIER_RE)
    candidate["controlPlaneBaseUrl"] = require_https(
        candidate["controlPlaneBaseUrl"], "candidate.controlPlaneBaseUrl"
    )
    migration = exact_keys(candidate["migrationTail"], {"name", "sha256"}, "candidate.migrationTail")
    require_string(migration["name"], "candidate.migrationTail.name", MIGRATION_RE)
    require_string(migration["sha256"], "candidate.migrationTail.sha256", SHA256_RE)

    period = exact_keys(manifest["period"], {"start", "end"}, "period")
    period_start = parse_utc(period["start"], "period.start")
    period_end = parse_utc(period["end"], "period.end")
    if period_start >= period_end or period_end - period_start > dt.timedelta(days=93):
        raise CostEvidenceError("period must be positive and no longer than 93 days")

    usage = exact_keys(
        manifest["usage"],
        {
            "executionCount",
            "inputTokens",
            "outputTokens",
            "cachedInputTokens",
            "cacheCreationInputTokens",
            "providerCostReportedExecutionCount",
            "providerCostUnavailableExecutionCount",
            "actualPlatformAllocationCount",
            "estimatedPlatformAllocationCount",
        },
        "usage",
    )
    execution_count = require_int(usage["executionCount"], "usage.executionCount", minimum=1)
    for key in ("inputTokens", "outputTokens", "cachedInputTokens", "cacheCreationInputTokens"):
        require_int(usage[key], f"usage.{key}")
    provider_reported = require_int(
        usage["providerCostReportedExecutionCount"], "usage.providerCostReportedExecutionCount"
    )
    provider_unavailable = require_int(
        usage["providerCostUnavailableExecutionCount"], "usage.providerCostUnavailableExecutionCount"
    )
    actual_allocations = require_int(
        usage["actualPlatformAllocationCount"], "usage.actualPlatformAllocationCount"
    )
    estimated_allocations = require_int(
        usage["estimatedPlatformAllocationCount"], "usage.estimatedPlatformAllocationCount"
    )
    if provider_reported + provider_unavailable != execution_count:
        raise CostEvidenceError("Provider cost coverage must classify every execution exactly once")
    if actual_allocations + estimated_allocations != execution_count:
        raise CostEvidenceError("Platform allocation coverage must classify every execution exactly once")

    costs = exact_keys(
        manifest["costs"],
        {"providerByCurrency", "platformByCurrency", "knownByCurrency"},
        "costs",
    )
    provider_costs = validate_money_map(costs["providerByCurrency"], "costs.providerByCurrency")
    platform_costs = validate_money_map(costs["platformByCurrency"], "costs.platformByCurrency")
    known_costs = validate_money_map(costs["knownByCurrency"], "costs.knownByCurrency")
    currencies = set(provider_costs) | set(platform_costs)
    expected_known = {
        currency: provider_costs.get(currency, 0) + platform_costs.get(currency, 0)
        for currency in sorted(currencies)
    }
    if known_costs != expected_known:
        raise CostEvidenceError("knownByCurrency must exactly equal Provider plus platform costs")

    controls = exact_keys(
        manifest["controls"],
        {
            "tokenTotalsReconciled",
            "providerCoverageComplete",
            "actualOverridesEstimate",
            "currencySafeAggregation",
            "tenantIsolationValidated",
            "noPaymentDataPresent",
        },
        "controls",
    )
    if not all(require_bool(value, f"controls.{key}") for key, value in controls.items()):
        raise CostEvidenceError("all internal usage and cost controls must pass")

    evidence = manifest["evidence"]
    if not isinstance(evidence, list) or len(evidence) != len(EVIDENCE_IDS):
        raise CostEvidenceError("evidence must contain the four required files")
    seen: set[str] = set()
    normalized_evidence: list[dict[str, str]] = []
    total_evidence_bytes = 0
    for index, item in enumerate(evidence):
        row = exact_keys(item, {"id", "path", "sha256"}, f"evidence[{index}]")
        evidence_id = require_string(row["id"], f"evidence[{index}].id")
        if evidence_id not in EVIDENCE_IDS or evidence_id in seen:
            raise CostEvidenceError("evidence IDs must be unique and complete")
        seen.add(evidence_id)
        relative, path = safe_evidence_path(evidence_root, row["path"], f"evidence[{index}].path")
        expected_digest = require_string(row["sha256"], f"evidence[{index}].sha256", SHA256_RE)
        data = read_regular_file(path, MAX_EVIDENCE_BYTES, f"evidence[{index}]")
        total_evidence_bytes += len(data)
        if total_evidence_bytes > MAX_TOTAL_EVIDENCE_BYTES:
            raise CostEvidenceError("total internal cost evidence exceeds the bounded size limit")
        actual_digest = "sha256:" + hashlib.sha256(data).hexdigest()
        if actual_digest != expected_digest:
            raise CostEvidenceError(f"evidence digest mismatch for {evidence_id}")
        normalized_evidence.append({"id": evidence_id, "path": relative, "sha256": actual_digest})
    if seen != EVIDENCE_IDS:
        raise CostEvidenceError("evidence IDs must be unique and complete")

    if validated_at is None:
        validated = dt.datetime.now(dt.timezone.utc).replace(microsecond=0)
    else:
        validated = parse_utc(validated_at, "validatedAt")
    manifest_reference = normalized_manifest_reference(
        manifest_reference_path or manifest_path.name
    )
    return {
        "schemaVersion": RECEIPT_SCHEMA,
        "assessment": ASSESSMENT,
        "candidate": candidate,
        "period": period,
        "usage": usage,
        "costs": {
            "providerByCurrency": provider_costs,
            "platformByCurrency": platform_costs,
            "knownByCurrency": known_costs,
        },
        "controls": controls,
        "manifest": {
            "path": manifest_reference,
            "sha256": "sha256:" + hashlib.sha256(manifest_bytes).hexdigest(),
        },
        "evidence": sorted(normalized_evidence, key=lambda row: row["id"]),
        "evidenceFileCount": len(normalized_evidence),
        "cryptographicSignaturesVerified": False,
        "externalSourceAndAuthorityVerificationRequired": True,
        "eligibleForHumanGateReview": True,
        "validatedAt": validated.isoformat().replace("+00:00", "Z"),
    }


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--manifest", required=True, type=pathlib.Path)
    parser.add_argument("--evidence-root", required=True, type=pathlib.Path)
    parser.add_argument("--output", type=pathlib.Path)
    parser.add_argument("--validated-at", help=argparse.SUPPRESS)
    args = parser.parse_args()
    try:
        receipt = validate(args.manifest, args.evidence_root, args.validated_at)
        encoded = (json.dumps(receipt, indent=2, sort_keys=True) + "\n").encode()
        if args.output:
            publish_immutable_with_sha256(args.output, encoded)
        else:
            sys.stdout.buffer.write(encoded)
        return 0
    except (CostEvidenceError, ImmutableEvidenceIOError) as error:
        print(f"internal cost evidence validation failed: {error}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
