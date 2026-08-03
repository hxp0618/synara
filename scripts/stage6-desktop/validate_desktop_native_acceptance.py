#!/usr/bin/env python3
"""Validate four-platform Desktop native acceptance evidence without approving GA."""

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


SCHEMA_VERSION = "synara.stage6-desktop-native-acceptance.v1"
ENROLLMENT_RUNTIME_EVIDENCE_SCHEMA = (
    "synara.stage6-desktop-enrollment-runtime-evidence.v1"
)
ASSESSMENT = "evidence-validated-not-desktop-ga-passed"
MAX_MANIFEST_BYTES = 2 * 1024 * 1024
MAX_EVIDENCE_FILE_BYTES = 32 * 1024 * 1024
MAX_TOTAL_EVIDENCE_BYTES = 256 * 1024 * 1024
PROHIBITED_SECRET_PATTERNS = (
    ("private key", re.compile(rb"-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----")),
    ("AWS access key", re.compile(rb"\bAKIA[0-9A-Z]{16}\b")),
    (
        "bearer credential",
        re.compile(rb"(?i)authorization\s*[:=]\s*bearer\s+[^\s,;]+"),
    ),
    (
        "credential-bearing URL",
        re.compile(rb"(?i)\b(?:https?|postgres(?:ql)?|mysql)://[^\s/:@]+:[^\s/@]+@"),
    ),
)
COMMIT_RE = re.compile(r"^[0-9a-f]{40}$")
HEX_SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
SHA256_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
IDENTIFIER_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:/@+ -]{1,199}$")
FILE_NAME_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._+() -]{0,239}$")
ENVIRONMENTS = {"production", "production-like", "staging", "fixture"}
TARGET_STATUSES = {"passed", "failed", "blocked"}
EXECUTION_MODES = {"native", "rosetta", "wine", "qemu", "other-emulation"}
APPROVAL_ROLES = {"engineering", "security", "release"}

TARGETS = {
    "macos-arm64": {
        "platform": "mac",
        "arch": "arm64",
        "target": "dmg",
        "artifactSuffix": ".dmg",
        "credentialBackend": "macos-keychain",
        "signingScheme": "apple-developer-id",
    },
    "macos-x64": {
        "platform": "mac",
        "arch": "x64",
        "target": "dmg",
        "artifactSuffix": ".dmg",
        "credentialBackend": "macos-keychain",
        "signingScheme": "apple-developer-id",
    },
    "windows-x64": {
        "platform": "win",
        "arch": "x64",
        "target": "nsis",
        "artifactSuffix": ".exe",
        "credentialBackend": "windows-dpapi",
        "signingScheme": "windows-authenticode",
    },
    "linux-x64": {
        "platform": "linux",
        "arch": "x64",
        "target": "AppImage",
        "artifactSuffix": ".AppImage",
        "credentialBackend": "linux-secret-service",
        "signingScheme": "ci-provenance",
    },
}

PROTOCOL_FIELDS = {
    "registered",
    "osLauncherUsed",
    "installedAppDispatched",
    "allowlistDeniedWithoutNetwork",
}
CREDENTIAL_FIELDS = {
    "backend",
    "available",
    "encryptedAtRest",
    "plaintextScanPassed",
    "missingStorePromptAbsent",
}
ENROLLMENT_FIELDS = {
    "firstLaunchChoiceRequired",
    "localModeRestorePassed",
    "cloudModeRestorePassed",
    "localToCloudSwitchPassed",
    "cloudToLocalSwitchPassed",
    "localModeNoCloudTrafficPassed",
    "cloudCredentialRetainedAcrossLocalSwitch",
    "deviceProofPassed",
    "initialHydrationPassed",
    "restartPersistencePassed",
    "rotationPassed",
    "replayDenied",
    "remoteRevocationPassed",
    "disconnectPassed",
    "localStatePreserved",
}
EVIDENCE_FIELDS = {
    "provenance",
    "attestationBundle",
    "artifactAttestation",
    "installation",
    "protocol",
    "credentialStore",
    "enrollment",
    "secretScan",
}
MODE_TRANSITION_SPECS = (
    ("first-use-local-choice", "unselected", "local"),
    ("local-restart-restore", "local", "local"),
    ("local-to-cloud-switch", "local", "cloud"),
    ("cloud-restart-restore", "cloud", "cloud"),
    ("cloud-to-local-switch", "cloud", "local"),
    ("local-to-cloud-resume", "local", "cloud"),
)


class DesktopAcceptanceError(Exception):
    pass


def reject_duplicate_json_fields(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            raise DesktopAcceptanceError(f"JSON input contains duplicate field {key!r}")
        result[key] = value
    return result


def scan_for_secret_material(content: bytes, label: str) -> None:
    for secret_label, pattern in PROHIBITED_SECRET_PATTERNS:
        if pattern.search(content):
            raise DesktopAcceptanceError(
                f"{label} contains prohibited {secret_label} material"
            )


def parse_json_bytes(content: bytes, label: str) -> Any:
    try:
        return json.loads(content, object_pairs_hook=reject_duplicate_json_fields)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise DesktopAcceptanceError(f"{label} must be readable UTF-8 JSON") from error


def require_mapping(value: Any, label: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise DesktopAcceptanceError(f"{label} must be an object")
    return value


def require_exact_fields(value: Any, fields: set[str], label: str) -> dict[str, Any]:
    mapping = require_mapping(value, label)
    if set(mapping) != fields:
        raise DesktopAcceptanceError(f"{label} fields do not match the v1 schema")
    return mapping


def require_identifier(value: Any, label: str) -> str:
    if not isinstance(value, str) or IDENTIFIER_RE.fullmatch(value.strip()) is None:
        raise DesktopAcceptanceError(f"{label} must be a bounded identifier")
    return value.strip()


def require_bounded_text(value: Any, label: str, maximum: int = 500) -> str:
    if (
        not isinstance(value, str)
        or not value.strip()
        or len(value.strip()) > maximum
        or any(ord(character) < 32 for character in value)
    ):
        raise DesktopAcceptanceError(f"{label} must be bounded printable text")
    return value.strip()


def require_bool(value: Any, label: str) -> bool:
    if not isinstance(value, bool):
        raise DesktopAcceptanceError(f"{label} must be a boolean")
    return value


def require_nonnegative_int(value: Any, label: str) -> int:
    if not isinstance(value, int) or isinstance(value, bool) or value < 0:
        raise DesktopAcceptanceError(f"{label} must be a non-negative integer")
    return value


def parse_utc(value: Any, label: str) -> dt.datetime:
    if not isinstance(value, str) or not value.endswith("Z"):
        raise DesktopAcceptanceError(f"{label} must be an RFC3339 UTC timestamp ending in Z")
    try:
        parsed = dt.datetime.fromisoformat(value[:-1] + "+00:00")
    except ValueError as error:
        raise DesktopAcceptanceError(f"{label} must be a valid RFC3339 UTC timestamp") from error
    if parsed.tzinfo != dt.timezone.utc:
        raise DesktopAcceptanceError(f"{label} must use UTC")
    return parsed


def parse_https_base_url(value: Any, label: str) -> str:
    if not isinstance(value, str) or not value.strip():
        raise DesktopAcceptanceError(f"{label} must be a non-empty HTTPS URL")
    parsed = urllib.parse.urlsplit(value.strip())
    if (
        parsed.scheme != "https"
        or not parsed.hostname
        or parsed.username is not None
        or parsed.password is not None
        or parsed.query
        or parsed.fragment
    ):
        raise DesktopAcceptanceError(f"{label} must be a credential-free HTTPS URL")
    try:
        port = parsed.port
    except ValueError as error:
        raise DesktopAcceptanceError(f"{label} has an invalid port") from error
    host = parsed.hostname.lower()
    if ":" in host and not host.startswith("["):
        host = f"[{host}]"
    authority = host if port is None or port == 443 else f"{host}:{port}"
    path = parsed.path.rstrip("/")
    return f"https://{authority}{path}"


def resolve_evidence_file(root: pathlib.Path, raw_path: Any, label: str) -> pathlib.Path:
    if not isinstance(raw_path, str) or not raw_path.strip():
        raise DesktopAcceptanceError(f"{label}.path must be a non-empty relative path")
    relative = pathlib.Path(raw_path)
    if relative.is_absolute() or any(part in {".", ".."} for part in relative.parts):
        raise DesktopAcceptanceError(f"{label}.path must be traversal-free and relative")
    candidate = root / relative
    current = candidate
    while current != root:
        if current.is_symlink():
            raise DesktopAcceptanceError(f"{label}.path must not traverse a symlink")
        current = current.parent
    try:
        resolved = candidate.resolve(strict=True)
        resolved.relative_to(root)
    except (OSError, ValueError) as error:
        raise DesktopAcceptanceError(f"{label}.path must resolve inside the evidence root") from error
    if not resolved.is_file():
        raise DesktopAcceptanceError(f"{label}.path must reference a regular file")
    return resolved


def validate_reference(
    value: Any,
    root: pathlib.Path,
    label: str,
    used_paths: set[pathlib.Path],
    total_bytes: list[int],
) -> tuple[dict[str, str], pathlib.Path, bytes]:
    reference = require_exact_fields(value, {"path", "sha256"}, label)
    path = resolve_evidence_file(root, reference["path"], label)
    if path in used_paths:
        raise DesktopAcceptanceError(f"{label}.path duplicates another evidence file")
    used_paths.add(path)
    digest = reference["sha256"]
    if not isinstance(digest, str) or SHA256_RE.fullmatch(digest) is None:
        raise DesktopAcceptanceError(f"{label}.sha256 must be sha256:<64 lowercase hex>")
    try:
        content = read_stable_regular_file(
            path,
            label=label,
            maximum_bytes=MAX_EVIDENCE_FILE_BYTES,
        )
    except ImmutableEvidenceIOError as error:
        raise DesktopAcceptanceError(str(error)) from error
    total_bytes[0] += len(content)
    if total_bytes[0] > MAX_TOTAL_EVIDENCE_BYTES:
        raise DesktopAcceptanceError(
            "total Desktop acceptance evidence exceeds the bounded size limit"
        )
    scan_for_secret_material(content, label)
    actual = "sha256:" + hashlib.sha256(content).hexdigest()
    if digest != actual:
        raise DesktopAcceptanceError(f"{label}.sha256 does not match the evidence file")
    return {"path": path.relative_to(root).as_posix(), "sha256": actual}, path, content


def validate_enrollment_runtime_evidence(
    content: bytes,
    *,
    target_id: str,
    runner_reference: str,
    host_architecture: str,
    execution_mode: str,
    candidate: dict[str, Any],
    exercise_started_at: dt.datetime,
    exercise_completed_at: dt.datetime,
    manifest_results: dict[str, bool],
) -> dict[str, Any]:
    raw = parse_json_bytes(content, f"{target_id} enrollment evidence")
    evidence = require_exact_fields(
        raw,
        {
            "schemaVersion",
            "candidate",
            "target",
            "startedAt",
            "completedAt",
            "modeTransitions",
            "requestCounts",
            "credentialContinuity",
            "results",
        },
        f"{target_id} enrollment evidence",
    )
    if evidence["schemaVersion"] != ENROLLMENT_RUNTIME_EVIDENCE_SCHEMA:
        raise DesktopAcceptanceError(
            f"{target_id} enrollment evidence schemaVersion is not supported"
        )

    evidence_candidate = require_exact_fields(
        evidence["candidate"],
        {"candidateId", "sourceCommit", "environmentId", "controlPlaneBaseUrl"},
        f"{target_id} enrollment evidence.candidate",
    )
    expected_candidate = {
        "candidateId": candidate["candidateId"],
        "sourceCommit": candidate["sourceCommit"],
        "environmentId": candidate["environmentId"],
        "controlPlaneBaseUrl": candidate["controlPlaneBaseUrl"],
    }
    if evidence_candidate != expected_candidate:
        raise DesktopAcceptanceError(
            f"{target_id} enrollment evidence candidate does not match manifest"
        )

    evidence_target = require_exact_fields(
        evidence["target"],
        {"id", "runnerReference", "hostArchitecture", "executionMode"},
        f"{target_id} enrollment evidence.target",
    )
    expected_target = {
        "id": target_id,
        "runnerReference": runner_reference,
        "hostArchitecture": host_architecture,
        "executionMode": execution_mode,
    }
    if evidence_target != expected_target:
        raise DesktopAcceptanceError(
            f"{target_id} enrollment evidence target does not match manifest"
        )

    started_at = parse_utc(
        evidence["startedAt"], f"{target_id} enrollment evidence.startedAt"
    )
    completed_at = parse_utc(
        evidence["completedAt"], f"{target_id} enrollment evidence.completedAt"
    )
    if (
        completed_at <= started_at
        or started_at < exercise_started_at
        or completed_at > exercise_completed_at
    ):
        raise DesktopAcceptanceError(
            f"{target_id} enrollment evidence must stay inside the exercise window"
        )

    raw_transitions = evidence["modeTransitions"]
    if not isinstance(raw_transitions, list) or len(raw_transitions) != len(
        MODE_TRANSITION_SPECS
    ):
        raise DesktopAcceptanceError(
            f"{target_id} enrollment evidence must contain the canonical mode transitions"
        )
    transitions: list[dict[str, Any]] = []
    transition_passed: dict[str, bool] = {}
    previous_observed_at = started_at
    for index, (raw_transition, expected_transition) in enumerate(
        zip(raw_transitions, MODE_TRANSITION_SPECS, strict=True)
    ):
        transition = require_exact_fields(
            raw_transition,
            {"id", "from", "to", "observedAt", "passed"},
            f"{target_id} enrollment evidence.modeTransitions[{index}]",
        )
        transition_id, from_mode, to_mode = expected_transition
        if (
            transition["id"] != transition_id
            or transition["from"] != from_mode
            or transition["to"] != to_mode
        ):
            raise DesktopAcceptanceError(
                f"{target_id} enrollment evidence mode transition order or identity is invalid"
            )
        observed_at = parse_utc(
            transition["observedAt"],
            f"{target_id} enrollment evidence.modeTransitions[{index}].observedAt",
        )
        if observed_at <= previous_observed_at or observed_at > completed_at:
            raise DesktopAcceptanceError(
                f"{target_id} enrollment evidence transition timestamps must be strictly ordered"
            )
        previous_observed_at = observed_at
        passed = require_bool(
            transition["passed"],
            f"{target_id} enrollment evidence.modeTransitions[{index}].passed",
        )
        transition_passed[transition_id] = passed
        transitions.append(
            {
                "id": transition_id,
                "from": from_mode,
                "to": to_mode,
                "observedAt": transition["observedAt"],
                "passed": passed,
            }
        )

    request_counts = require_exact_fields(
        evidence["requestCounts"],
        {
            "backendBeforeFirstChoice",
            "cloudBeforeFirstChoice",
            "cloudWhileLocal",
        },
        f"{target_id} enrollment evidence.requestCounts",
    )
    normalized_request_counts = {
        field: require_nonnegative_int(
            request_counts[field],
            f"{target_id} enrollment evidence.requestCounts.{field}",
        )
        for field in sorted(request_counts)
    }

    continuity = require_exact_fields(
        evidence["credentialContinuity"],
        {"beforeLocalSwitchSha256", "afterCloudResumeSha256"},
        f"{target_id} enrollment evidence.credentialContinuity",
    )
    continuity_values: dict[str, str | None] = {}
    for field in sorted(continuity):
        value = continuity[field]
        if value is not None and (
            not isinstance(value, str) or SHA256_RE.fullmatch(value) is None
        ):
            raise DesktopAcceptanceError(
                f"{target_id} enrollment evidence.credentialContinuity.{field} must be null or sha256:<64 lowercase hex>"
            )
        continuity_values[field] = value
    credential_continuity_passed = (
        continuity_values["beforeLocalSwitchSha256"] is not None
        and continuity_values["beforeLocalSwitchSha256"]
        == continuity_values["afterCloudResumeSha256"]
        and transition_passed["cloud-to-local-switch"]
        and transition_passed["local-to-cloud-resume"]
    )

    evidence_results, _ = validate_bool_mapping(
        evidence["results"],
        ENROLLMENT_FIELDS,
        f"{target_id} enrollment evidence.results",
    )
    derived_connection_results = {
        "firstLaunchChoiceRequired": (
            transition_passed["first-use-local-choice"]
            and normalized_request_counts["backendBeforeFirstChoice"] == 0
            and normalized_request_counts["cloudBeforeFirstChoice"] == 0
        ),
        "localModeRestorePassed": transition_passed["local-restart-restore"],
        "cloudModeRestorePassed": transition_passed["cloud-restart-restore"],
        "localToCloudSwitchPassed": transition_passed["local-to-cloud-switch"],
        "cloudToLocalSwitchPassed": transition_passed["cloud-to-local-switch"],
        "localModeNoCloudTrafficPassed": normalized_request_counts["cloudWhileLocal"]
        == 0,
        "cloudCredentialRetainedAcrossLocalSwitch": credential_continuity_passed,
    }
    for field, derived in derived_connection_results.items():
        if evidence_results[field] != derived:
            raise DesktopAcceptanceError(
                f"{target_id} enrollment evidence.results.{field} does not match structured observations"
            )
    if evidence_results != manifest_results:
        raise DesktopAcceptanceError(
            f"{target_id} enrollment evidence results do not match manifest assertions"
        )

    return {
        "schemaVersion": ENROLLMENT_RUNTIME_EVIDENCE_SCHEMA,
        "candidate": expected_candidate,
        "target": expected_target,
        "startedAt": evidence["startedAt"],
        "completedAt": evidence["completedAt"],
        "modeTransitions": transitions,
        "requestCounts": normalized_request_counts,
        "credentialContinuityMatched": credential_continuity_passed,
        "results": evidence_results,
    }


def validate_artifact_list(value: Any, label: str) -> list[dict[str, Any]]:
    if not isinstance(value, list) or not value:
        raise DesktopAcceptanceError(f"{label} must be a non-empty artifact list")
    artifacts: list[dict[str, Any]] = []
    names: set[str] = set()
    for index, raw in enumerate(value):
        artifact = require_exact_fields(
            raw, {"fileName", "size", "sha256"}, f"{label}[{index}]"
        )
        file_name = artifact["fileName"]
        if (
            not isinstance(file_name, str)
            or FILE_NAME_RE.fullmatch(file_name) is None
            or file_name in names
        ):
            raise DesktopAcceptanceError(f"{label} file names must be unique bounded base names")
        names.add(file_name)
        size = artifact["size"]
        digest = artifact["sha256"]
        if not isinstance(size, int) or isinstance(size, bool) or size <= 0:
            raise DesktopAcceptanceError(f"{label}[{index}].size must be positive")
        if not isinstance(digest, str) or HEX_SHA256_RE.fullmatch(digest) is None:
            raise DesktopAcceptanceError(f"{label}[{index}].sha256 must be 64 lowercase hex")
        artifacts.append({"fileName": file_name, "size": size, "sha256": digest})
    return sorted(artifacts, key=lambda item: item["fileName"])


def validate_signing(
    value: Any,
    target_id: str,
    expected: dict[str, str],
) -> tuple[dict[str, Any], bool]:
    signing = require_exact_fields(value, {"status", "scheme", "identity", "checks"}, "provenance.signing")
    status = signing["status"]
    scheme = signing["scheme"]
    if status not in {
        "verified",
        "not-applicable",
        "unsigned-build-only",
        "unsigned-explicit-release",
    } or scheme not in {"apple-developer-id", "windows-authenticode", "none"}:
        raise DesktopAcceptanceError("provenance.signing status or scheme is not allowed")
    checks = signing["checks"]
    if not isinstance(checks, list) or not checks or any(
        not isinstance(check, str) or not check.strip() for check in checks
    ):
        raise DesktopAcceptanceError("provenance.signing.checks must be a non-empty string list")
    normalized_checks = [check.strip() for check in checks]
    if expected["platform"] == "mac":
        if status != "verified" or scheme != "apple-developer-id":
            if signing["identity"] is not None or scheme != "none":
                raise DesktopAcceptanceError("unsigned macOS provenance must use scheme none and null identity")
            return {
                "status": status,
                "scheme": scheme,
                "identity": None,
                "checks": normalized_checks,
                "platformSigningEligible": False,
                "requiredGaScheme": expected["signingScheme"],
                "target": target_id,
            }, False
        identity = require_exact_fields(
            signing["identity"], {"teamId", "authorities", "appBundle", "diskImage"}, "provenance.signing.identity"
        )
        team_id = require_identifier(identity["teamId"], "provenance.signing.identity.teamId")
        authorities = identity["authorities"]
        if not isinstance(authorities, list) or not authorities or any(
            not isinstance(authority, str) or not authority.strip() for authority in authorities
        ):
            raise DesktopAcceptanceError("macOS signing authorities must be a non-empty string list")
        normalized_authorities = [
            require_bounded_text(
                authority, f"provenance.signing.identity.authorities[{index}]"
            )
            for index, authority in enumerate(authorities)
        ]
        required_checks = {
            "codesign --verify app",
            "spctl --assess app",
            "stapler validate app",
            "codesign --verify dmg",
            "spctl --assess dmg",
            "stapler validate dmg",
        }
        eligible = (
            status == "verified"
            and scheme == "apple-developer-id"
            and required_checks.issubset(normalized_checks)
            and any("Developer ID Application" in authority for authority in normalized_authorities)
        )
        projected_identity: Any = {
            "teamId": team_id,
            "authorities": normalized_authorities,
            "appBundle": require_bounded_text(
                identity["appBundle"], "provenance.signing.identity.appBundle"
            ),
            "diskImage": require_bounded_text(
                identity["diskImage"], "provenance.signing.identity.diskImage"
            ),
        }
    elif expected["platform"] == "win":
        if status != "verified" or scheme != "windows-authenticode":
            if signing["identity"] is not None or scheme != "none":
                raise DesktopAcceptanceError("unsigned Windows provenance must use scheme none and null identity")
            return {
                "status": status,
                "scheme": scheme,
                "identity": None,
                "checks": normalized_checks,
                "platformSigningEligible": False,
                "requiredGaScheme": expected["signingScheme"],
                "target": target_id,
            }, False
        identities = signing["identity"]
        if not isinstance(identities, list) or not identities:
            raise DesktopAcceptanceError("Windows signing identity must be a non-empty list")
        projected_identity = []
        for index, raw_identity in enumerate(identities):
            identity = require_exact_fields(
                raw_identity,
                {
                    "fileName",
                    "subject",
                    "publisher",
                    "thumbprint",
                    "timestampSubject",
                    "timestampThumbprint",
                },
                f"provenance.signing.identity[{index}]",
            )
            projected_identity.append(
                {
                    key: require_bounded_text(
                        identity[key], f"provenance.signing.identity[{index}].{key}"
                    )
                    for key in identity
                }
            )
        eligible = status == "verified" and scheme == "windows-authenticode"
    else:
        if signing["identity"] is not None:
            raise DesktopAcceptanceError("Linux provenance signing identity must be null")
        projected_identity = None
        eligible = status == "not-applicable" and scheme == "none"
    return {
        "status": status,
        "scheme": scheme,
        "identity": projected_identity,
        "checks": normalized_checks,
        "platformSigningEligible": eligible,
        "requiredGaScheme": expected["signingScheme"],
        "target": target_id,
    }, eligible


def validate_provenance(
    content: bytes,
    target_id: str,
    expected: dict[str, str],
    candidate: dict[str, Any],
    artifact_digest: str,
) -> tuple[dict[str, Any], bool]:
    provenance = parse_json_bytes(content, f"{target_id} provenance")
    provenance = require_exact_fields(
        provenance,
        {
            "schemaVersion",
            "publication",
            "platform",
            "arch",
            "target",
            "version",
            "source",
            "signing",
            "artifacts",
        },
        f"{target_id} provenance",
    )
    if provenance["schemaVersion"] != 1:
        raise DesktopAcceptanceError(f"{target_id} provenance schemaVersion must be 1")
    for key in ("platform", "arch", "target"):
        if provenance[key] != expected[key]:
            raise DesktopAcceptanceError(f"{target_id} provenance {key} does not match target")
    if provenance["version"] != candidate["version"]:
        raise DesktopAcceptanceError(f"{target_id} provenance version does not match candidate")
    source = require_exact_fields(
        provenance["source"], {"commit", "tag", "lockfileSha256"}, f"{target_id} provenance.source"
    )
    if (
        source["commit"] != candidate["sourceCommit"]
        or source["tag"] != candidate["sourceTag"]
        or source["lockfileSha256"] != candidate["lockfileSha256"]
    ):
        raise DesktopAcceptanceError(f"{target_id} provenance source does not match candidate")
    artifacts = validate_artifact_list(provenance["artifacts"], f"{target_id} provenance.artifacts")
    primary = [
        artifact
        for artifact in artifacts
        if artifact["fileName"].endswith(expected["artifactSuffix"])
    ]
    if len(primary) != 1:
        raise DesktopAcceptanceError(f"{target_id} provenance must contain one primary installer")
    if "sha256:" + primary[0]["sha256"] != artifact_digest:
        raise DesktopAcceptanceError(f"{target_id} artifactDigest does not match provenance")
    signing, platform_signing_eligible = validate_signing(
        provenance["signing"], target_id, expected
    )
    publication = require_bool(provenance["publication"], f"{target_id} provenance.publication")
    eligible = publication and platform_signing_eligible
    return {
        "publication": publication,
        "platform": provenance["platform"],
        "arch": provenance["arch"],
        "target": provenance["target"],
        "version": provenance["version"],
        "source": source,
        "signing": signing,
        "artifacts": artifacts,
        "primaryInstaller": primary[0],
        "provenanceEligible": eligible,
    }, eligible


def validate_attestation_verification(
    content: bytes,
    target_id: str,
    primary_installer: dict[str, Any],
) -> dict[str, Any]:
    verification = parse_json_bytes(
        content, f"{target_id} artifact attestation verification"
    )
    if not isinstance(verification, list) or not verification:
        raise DesktopAcceptanceError(
            f"{target_id} artifact attestation verification must be a non-empty array"
        )
    expected_name = primary_installer["fileName"]
    expected_digest = primary_installer["sha256"]
    for raw_entry in verification:
        if not isinstance(raw_entry, dict):
            continue
        result = raw_entry.get("verificationResult")
        if not isinstance(result, dict):
            continue
        statement = result.get("statement")
        signature = result.get("signature")
        timestamps = result.get("verifiedTimestamps")
        if (
            not isinstance(statement, dict)
            or statement.get("predicateType") != "https://slsa.dev/provenance/v1"
            or not isinstance(signature, dict)
            or not isinstance(signature.get("certificate"), dict)
            or not isinstance(timestamps, list)
            or not timestamps
        ):
            continue
        subjects = statement.get("subject")
        if not isinstance(subjects, list):
            continue
        for subject in subjects:
            if not isinstance(subject, dict):
                continue
            subject_name = subject.get("name")
            if (
                not isinstance(subject_name, str)
                or "\\" in subject_name
                or pathlib.PurePosixPath(subject_name).name != expected_name
            ):
                continue
            digest = subject.get("digest")
            if isinstance(digest, dict) and digest.get("sha256") == expected_digest:
                return {
                    "subjectName": subject_name,
                    "subjectSha256": "sha256:" + expected_digest,
                    "predicateType": statement["predicateType"],
                    "verifiedTimestampCount": len(timestamps),
                    "certificatePresent": True,
                    "verified": True,
                }
    raise DesktopAcceptanceError(
        f"{target_id} artifact attestation does not verify the primary installer subject"
    )


def validate_bool_mapping(value: Any, fields: set[str], label: str) -> tuple[dict[str, bool], bool]:
    mapping = require_exact_fields(value, fields, label)
    projected = {field: require_bool(mapping[field], f"{label}.{field}") for field in sorted(fields)}
    return projected, all(projected.values())


def validate_target(
    value: Any,
    candidate: dict[str, Any],
    exercise_started_at: dt.datetime,
    exercise_completed_at: dt.datetime,
    evidence_root: pathlib.Path,
    used_paths: set[pathlib.Path],
    total_bytes: list[int],
    index: int,
) -> dict[str, Any]:
    label = f"targets[{index}]"
    target = require_exact_fields(
        value,
        {
            "id",
            "status",
            "runnerReference",
            "hostOsVersion",
            "hostArchitecture",
            "executionMode",
            "artifactDigest",
            "compatibilityWarningAbsent",
            "protocol",
            "credentialStore",
            "enrollment",
            "secretScanPassed",
            "evidence",
        },
        label,
    )
    target_id = target["id"]
    expected = TARGETS.get(target_id)
    if expected is None:
        raise DesktopAcceptanceError(f"{label}.id is not a supported native target")
    status = target["status"]
    if status not in TARGET_STATUSES:
        raise DesktopAcceptanceError(f"{target_id}.status is not allowed")
    runner_reference = require_identifier(target["runnerReference"], f"{target_id}.runnerReference")
    host_os_version = require_bounded_text(
        target["hostOsVersion"], f"{target_id}.hostOsVersion", 200
    )
    host_architecture = target["hostArchitecture"]
    if host_architecture not in {"arm64", "x64"}:
        raise DesktopAcceptanceError(f"{target_id}.hostArchitecture is not allowed")
    execution_mode = target["executionMode"]
    if execution_mode not in EXECUTION_MODES:
        raise DesktopAcceptanceError(f"{target_id}.executionMode is not allowed")
    artifact_digest = target["artifactDigest"]
    if not isinstance(artifact_digest, str) or SHA256_RE.fullmatch(artifact_digest) is None:
        raise DesktopAcceptanceError(f"{target_id}.artifactDigest must be sha256:<64 lowercase hex>")
    compatibility_warning_absent = require_bool(
        target["compatibilityWarningAbsent"], f"{target_id}.compatibilityWarningAbsent"
    )
    protocol, protocol_complete = validate_bool_mapping(
        target["protocol"], PROTOCOL_FIELDS, f"{target_id}.protocol"
    )
    credential = require_exact_fields(
        target["credentialStore"], CREDENTIAL_FIELDS, f"{target_id}.credentialStore"
    )
    backend = credential["backend"]
    if not isinstance(backend, str) or not backend:
        raise DesktopAcceptanceError(f"{target_id}.credentialStore.backend is invalid")
    credential_bools = {
        field: require_bool(credential[field], f"{target_id}.credentialStore.{field}")
        for field in sorted(CREDENTIAL_FIELDS - {"backend"})
    }
    credential_complete = backend == expected["credentialBackend"] and all(
        credential_bools.values()
    )
    enrollment, enrollment_complete = validate_bool_mapping(
        target["enrollment"], ENROLLMENT_FIELDS, f"{target_id}.enrollment"
    )
    secret_scan_passed = require_bool(
        target["secretScanPassed"], f"{target_id}.secretScanPassed"
    )
    evidence = require_exact_fields(target["evidence"], EVIDENCE_FIELDS, f"{target_id}.evidence")
    projected_evidence: dict[str, dict[str, str]] = {}
    evidence_contents: dict[str, bytes] = {}
    for field in sorted(EVIDENCE_FIELDS):
        projected, _, content = validate_reference(
            evidence[field],
            evidence_root,
            f"{target_id}.evidence.{field}",
            used_paths,
            total_bytes,
        )
        projected_evidence[field] = projected
        evidence_contents[field] = content
    provenance, provenance_eligible = validate_provenance(
        evidence_contents["provenance"], target_id, expected, candidate, artifact_digest
    )
    artifact_attestation = validate_attestation_verification(
        evidence_contents["artifactAttestation"],
        target_id,
        provenance["primaryInstaller"],
    )
    enrollment_runtime_evidence = validate_enrollment_runtime_evidence(
        evidence_contents["enrollment"],
        target_id=target_id,
        runner_reference=runner_reference,
        host_architecture=host_architecture,
        execution_mode=execution_mode,
        candidate=candidate,
        exercise_started_at=exercise_started_at,
        exercise_completed_at=exercise_completed_at,
        manifest_results=enrollment,
    )
    native_execution = (
        execution_mode == "native" and host_architecture == expected["arch"]
    )
    target_eligible = all(
        (
            status == "passed",
            native_execution,
            compatibility_warning_absent,
            protocol_complete,
            credential_complete,
            enrollment_complete,
            secret_scan_passed,
            artifact_attestation["verified"],
            provenance_eligible,
        )
    )
    return {
        "id": target_id,
        "status": status,
        "runnerReference": runner_reference,
        "hostOsVersion": host_os_version,
        "hostArchitecture": host_architecture,
        "executionMode": execution_mode,
        "nativeExecutionProved": native_execution,
        "artifactDigest": artifact_digest,
        "artifactAttestation": artifact_attestation,
        "compatibilityWarningAbsent": compatibility_warning_absent,
        "protocol": protocol,
        "protocolComplete": protocol_complete,
        "credentialStore": {"backend": backend, **credential_bools},
        "credentialStoreComplete": credential_complete,
        "enrollment": enrollment,
        "enrollmentComplete": enrollment_complete,
        "enrollmentRuntimeEvidence": enrollment_runtime_evidence,
        "secretScanPassed": secret_scan_passed,
        "provenance": provenance,
        "evidence": projected_evidence,
        "eligible": target_eligible,
    }


def validate_approvals(
    value: Any,
    completed_at: dt.datetime,
    evidence_root: pathlib.Path,
    used_paths: set[pathlib.Path],
    total_bytes: list[int],
) -> tuple[list[dict[str, Any]], bool]:
    if not isinstance(value, list) or len(value) != len(APPROVAL_ROLES):
        raise DesktopAcceptanceError("approvals must contain Engineering, Security and Release exactly once")
    roles: set[str] = set()
    subjects: set[str] = set()
    approvals: list[dict[str, Any]] = []
    all_approved = True
    for index, raw in enumerate(value):
        approval = require_exact_fields(
            raw,
            {"role", "subjectReference", "approved", "approvedAt", "evidence"},
            f"approvals[{index}]",
        )
        role = approval["role"]
        if role not in APPROVAL_ROLES or role in roles:
            raise DesktopAcceptanceError("approvals must contain Engineering, Security and Release exactly once")
        roles.add(role)
        subject = require_identifier(
            approval["subjectReference"], f"approvals[{index}].subjectReference"
        )
        if subject in subjects:
            raise DesktopAcceptanceError("Desktop acceptance approvers must be distinct")
        subjects.add(subject)
        approved = require_bool(approval["approved"], f"approvals[{index}].approved")
        approved_at = parse_utc(approval["approvedAt"], f"approvals[{index}].approvedAt")
        if approved_at < completed_at:
            raise DesktopAcceptanceError("approval cannot predate acceptance completion")
        evidence, _, _ = validate_reference(
            approval["evidence"],
            evidence_root,
            f"approvals[{index}].evidence",
            used_paths,
            total_bytes,
        )
        all_approved = all_approved and approved
        approvals.append(
            {
                "role": role,
                "subjectReference": subject,
                "approved": approved,
                "approvedAt": approval["approvedAt"],
                "evidence": evidence,
            }
        )
    return sorted(approvals, key=lambda item: item["role"]), all_approved


def validate(
    manifest_path: pathlib.Path,
    evidence_root: pathlib.Path,
    validated_at: str | None,
) -> dict[str, Any]:
    root = evidence_root.resolve()
    if evidence_root.is_symlink() or not root.is_dir():
        raise DesktopAcceptanceError("evidenceRoot must be a regular non-symlink directory")
    try:
        manifest_bytes = read_stable_regular_file(
            manifest_path,
            label="manifest",
            maximum_bytes=MAX_MANIFEST_BYTES,
        )
    except ImmutableEvidenceIOError as error:
        raise DesktopAcceptanceError(str(error)) from error
    scan_for_secret_material(manifest_bytes, "manifest")
    manifest = parse_json_bytes(manifest_bytes, "manifest")
    manifest = require_exact_fields(
        manifest,
        {
            "schemaVersion",
            "candidate",
            "startedAt",
            "completedAt",
            "targets",
            "approvals",
        },
        "manifest",
    )
    if manifest["schemaVersion"] != SCHEMA_VERSION:
        raise DesktopAcceptanceError(f"schemaVersion must be {SCHEMA_VERSION}")
    candidate = require_exact_fields(
        manifest["candidate"],
        {
            "candidateId",
            "sourceCommit",
            "sourceTag",
            "lockfileSha256",
            "version",
            "environment",
            "environmentId",
            "controlPlaneBaseUrl",
            "migrationTail",
        },
        "candidate",
    )
    candidate_id = require_identifier(candidate["candidateId"], "candidate.candidateId")
    source_commit = candidate["sourceCommit"]
    if not isinstance(source_commit, str) or COMMIT_RE.fullmatch(source_commit) is None:
        raise DesktopAcceptanceError("candidate.sourceCommit must be a full lowercase Git SHA")
    version = require_identifier(candidate["version"], "candidate.version")
    source_tag = candidate["sourceTag"]
    if source_tag != f"v{version}":
        raise DesktopAcceptanceError("candidate.sourceTag must exactly match v<version>")
    lockfile_sha256 = candidate["lockfileSha256"]
    if not isinstance(lockfile_sha256, str) or HEX_SHA256_RE.fullmatch(lockfile_sha256) is None:
        raise DesktopAcceptanceError("candidate.lockfileSha256 must be 64 lowercase hex")
    environment = candidate["environment"]
    if environment not in ENVIRONMENTS:
        raise DesktopAcceptanceError("candidate.environment is not allowed")
    environment_id = require_identifier(candidate["environmentId"], "candidate.environmentId")
    control_plane_base_url = parse_https_base_url(
        candidate["controlPlaneBaseUrl"], "candidate.controlPlaneBaseUrl"
    )
    migration_tail = require_exact_fields(
        candidate["migrationTail"], {"name", "sha256"}, "candidate.migrationTail"
    )
    migration_name = migration_tail["name"]
    migration_sha256 = migration_tail["sha256"]
    if (
        not isinstance(migration_name, str)
        or re.fullmatch(r"\d{6}_[a-z0-9_]+\.sql", migration_name) is None
        or not isinstance(migration_sha256, str)
        or SHA256_RE.fullmatch(migration_sha256) is None
    ):
        raise DesktopAcceptanceError("candidate.migrationTail is invalid")
    normalized_candidate = {
        "candidateId": candidate_id,
        "sourceCommit": source_commit,
        "sourceTag": source_tag,
        "lockfileSha256": lockfile_sha256,
        "version": version,
        "environment": environment,
        "environmentId": environment_id,
        "controlPlaneBaseUrl": control_plane_base_url,
        "migrationTail": {"name": migration_name, "sha256": migration_sha256},
    }
    started_at = parse_utc(manifest["startedAt"], "startedAt")
    completed_at = parse_utc(manifest["completedAt"], "completedAt")
    if completed_at <= started_at:
        raise DesktopAcceptanceError("completedAt must be after startedAt")
    raw_targets = manifest["targets"]
    if not isinstance(raw_targets, list) or len(raw_targets) != len(TARGETS):
        raise DesktopAcceptanceError("targets must contain all four native targets exactly once")
    used_paths: set[pathlib.Path] = set()
    total_bytes = [0]
    targets: list[dict[str, Any]] = []
    seen_targets: set[str] = set()
    for index, raw_target in enumerate(raw_targets):
        target = validate_target(
            raw_target,
            normalized_candidate,
            started_at,
            completed_at,
            root,
            used_paths,
            total_bytes,
            index,
        )
        if target["id"] in seen_targets:
            raise DesktopAcceptanceError(f"duplicate target {target['id']}")
        seen_targets.add(target["id"])
        targets.append(target)
    if seen_targets != set(TARGETS):
        raise DesktopAcceptanceError("targets must contain all four native targets exactly once")
    approvals, approvals_complete = validate_approvals(
        manifest["approvals"], completed_at, root, used_paths, total_bytes
    )
    status_counts = {status: 0 for status in sorted(TARGET_STATUSES)}
    for target in targets:
        status_counts[target["status"]] += 1
    environment_eligible = environment in {"production", "production-like"}
    eligible = (
        environment_eligible
        and approvals_complete
        and all(target["eligible"] for target in targets)
    )
    timestamp = validated_at or dt.datetime.now(dt.timezone.utc).isoformat().replace("+00:00", "Z")
    parse_utc(timestamp, "validatedAt")
    return {
        "schemaVersion": "synara.stage6-desktop-native-acceptance-validation.v1",
        "candidate": normalized_candidate,
        "window": {"startedAt": manifest["startedAt"], "completedAt": manifest["completedAt"]},
        "targets": sorted(targets, key=lambda item: item["id"]),
        "targetStatusCounts": status_counts,
        "approvals": approvals,
        "approvalsComplete": approvals_complete,
        "environmentEligible": environment_eligible,
        "evidenceFileCount": len(used_paths),
        "evidenceByteCount": total_bytes[0],
        "eligibleForHumanGateReview": eligible,
        "assessment": ASSESSMENT,
        "validatedAt": timestamp,
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
    except (DesktopAcceptanceError, ImmutableEvidenceIOError) as error:
        parser.exit(2, f"desktop native acceptance validation failed: {error}\n")
    print(
        f"wrote {output}; eligibleForHumanGateReview={str(receipt['eligibleForHumanGateReview']).lower()}"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
