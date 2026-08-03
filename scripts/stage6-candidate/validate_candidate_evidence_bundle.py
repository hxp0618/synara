#!/usr/bin/env python3
"""Bind every Stage 6 external-control receipt to one immutable release candidate."""

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
)

COMPATIBILITY_ROOT = SCRIPTS_ROOT / "stage6-compatibility"
if str(COMPATIBILITY_ROOT) not in sys.path:
    sys.path.insert(0, str(COMPATIBILITY_ROOT))
from validate_compatibility_matrix import (  # noqa: E402
    CompatibilityError,
    validate as validate_source_compatibility,
)


SCHEMA_VERSION_V2 = "synara.stage6-candidate-evidence-bundle.v2"
SCHEMA_VERSION_V3 = "synara.stage6-candidate-evidence-bundle.v3"
SCHEMA_VERSION_V4 = "synara.stage6-candidate-evidence-bundle.v4"
SCHEMA_VERSION_V5 = "synara.stage6-candidate-evidence-bundle.v5"
RECEIPT_SCHEMA_VERSION_V2 = "synara.stage6-candidate-evidence-bundle-validation.v2"
RECEIPT_SCHEMA_VERSION_V3 = "synara.stage6-candidate-evidence-bundle-validation.v3"
RECEIPT_SCHEMA_VERSION_V4 = "synara.stage6-candidate-evidence-bundle-validation.v4"
RECEIPT_SCHEMA_VERSION_V5 = "synara.stage6-candidate-evidence-bundle-validation.v5"
ASSESSMENT = "evidence-consistent-not-ga-approved"
RELEASE_EVIDENCE_SCHEMA = "synara-stage6-release-evidence-v2"
RELEASE_EVIDENCE_ASSESSMENT = "evidence-collected-not-control-passed"
ENVIRONMENT_CLASSES = {"production", "production-like", "staging", "fixture"}
RELEASE_ELIGIBLE_ENVIRONMENTS = {"production", "production-like"}
COMMIT_RE = re.compile(r"^[0-9a-f]{40}$")
HEX_SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
SHA256_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
IDENTIFIER_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:/@+-]{1,199}$")
MIGRATION_RE = re.compile(r"^\d{6}_[a-z0-9_]+\.sql$")
MAX_JSON_BYTES = 32 * 1024 * 1024

RECEIPT_SPECS_V2 = {
    "billing": (
        "synara.stage6-stripe-billing-exercise-validation.v1",
        "evidence-validated-not-billing-passed",
    ),
    "capacity": (
        "synara.capacity-soak-evidence-receipt.v1",
        "evidence-validated-not-capacity-passed",
    ),
    "desktop": (
        "synara.stage6-desktop-native-acceptance-validation.v1",
        "evidence-validated-not-desktop-ga-passed",
    ),
    "incident": (
        "synara.incident-communication-exercise-evidence-receipt.v2",
        "evidence-validated-not-operations-ready",
    ),
    "operations": (
        "synara.stage6-operations-browser-exercise-validation.v2",
        "evidence-validated-not-operations-passed",
    ),
    "penetration": (
        "synara.third-party-penetration-evidence-receipt.v1",
        "evidence-validated-not-penetration-passed",
    ),
    "recovery": (
        "synara.recovery-drill-evidence-receipt.v2",
        "evidence-validated-not-control-passed",
    ),
    "residency": (
        "synara.data-residency-deployment-evidence-receipt.v1",
        "evidence-validated-not-residency-approved",
    ),
    "slo": (
        "synara.slo-window-evidence-receipt.v1",
        "evidence-validated-not-slo-passed",
    ),
}
RECEIPT_SPECS_V3 = {
    **RECEIPT_SPECS_V2,
    "workerSupplyChain": (
        "synara.stage6-worker-supply-chain-evidence.v1",
        "evidence-validated-not-worker-supply-chain-approved",
    ),
}
RECEIPT_SPECS_V4 = {
    **{name: spec for name, spec in RECEIPT_SPECS_V3.items() if name != "billing"},
    "internalCost": (
        "synara.stage6-internal-cost-evidence-validation.v1",
        "evidence-validated-not-internal-cost-approved",
    ),
}
WORKER_SUPPLY_CHAIN_CONTROLS = {
    "exactCleanup",
    "multiArchitectureReproducibility",
    "productionKmsSigningAndTransparencyLog",
    "signedAndNegativeAdmissionProbes",
    "spdxAndSlsaAttestations",
    "vulnerabilityAndSecretPolicy",
}

DESKTOP_TARGETS = {"macos-arm64", "macos-x64", "windows-x64", "linux-x64"}
DESKTOP_TARGET_ARCHITECTURES = {
    "linux-x64": "x64",
    "macos-arm64": "arm64",
    "macos-x64": "x64",
    "windows-x64": "x64",
}
DESKTOP_ARTIFACT_SET_EVIDENCE = (
    "artifactAttestation",
    "attestationBundle",
    "provenance",
)
DESKTOP_ENROLLMENT_RUNTIME_EVIDENCE_SCHEMA = (
    "synara.stage6-desktop-enrollment-runtime-evidence.v1"
)
DESKTOP_ENROLLMENT_FIELDS = {
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
DESKTOP_MODE_TRANSITION_SPECS = (
    ("first-use-local-choice", "unselected", "local"),
    ("local-restart-restore", "local", "local"),
    ("local-to-cloud-switch", "local", "cloud"),
    ("cloud-restart-restore", "cloud", "cloud"),
    ("cloud-to-local-switch", "cloud", "local"),
    ("local-to-cloud-resume", "local", "cloud"),
)
PENETRATION_ARTIFACTS = {
    "control-plane-api": "controlPlaneImage",
    "web": "webArtifact",
    "worker-runtime": "workerImage",
    "provider-host": "providerHostImage",
}
OPERATIONS_ARTIFACTS = {
    "controlPlane": "controlPlaneImage",
    "web": "webArtifact",
    "admin": "adminArtifact",
}


class CandidateEvidenceError(Exception):
    pass


def require_mapping(value: Any, label: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise CandidateEvidenceError(f"{label} must be an object")
    return value


def require_exact_fields(value: Any, fields: set[str], label: str) -> dict[str, Any]:
    mapping = require_mapping(value, label)
    if set(mapping) != fields:
        raise CandidateEvidenceError(f"{label} fields do not match the required schema")
    return mapping


def require_identifier(value: Any, label: str) -> str:
    if not isinstance(value, str) or IDENTIFIER_RE.fullmatch(value.strip()) is None:
        raise CandidateEvidenceError(f"{label} must be a bounded identifier")
    return value.strip()


def require_commit(value: Any, label: str) -> str:
    if not isinstance(value, str) or COMMIT_RE.fullmatch(value) is None:
        raise CandidateEvidenceError(f"{label} must be a full lowercase Git SHA")
    return value


def require_hex_sha256(value: Any, label: str) -> str:
    if not isinstance(value, str) or HEX_SHA256_RE.fullmatch(value) is None:
        raise CandidateEvidenceError(f"{label} must be 64 lowercase hex characters")
    return value


def require_sha256(value: Any, label: str) -> str:
    if not isinstance(value, str) or SHA256_RE.fullmatch(value) is None:
        raise CandidateEvidenceError(f"{label} must be sha256:<64 lowercase hex>")
    return value


def require_bool(value: Any, label: str) -> bool:
    if not isinstance(value, bool):
        raise CandidateEvidenceError(f"{label} must be a boolean")
    return value


def require_int(value: Any, label: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value < 0:
        raise CandidateEvidenceError(f"{label} must be an integer >= 0")
    return value


def require_number(value: Any, label: str) -> float:
    if not isinstance(value, (int, float)) or isinstance(value, bool):
        raise CandidateEvidenceError(f"{label} must be numeric")
    return float(value)


def parse_utc(value: Any, label: str) -> dt.datetime:
    if not isinstance(value, str) or not value.endswith("Z"):
        raise CandidateEvidenceError(f"{label} must be an RFC3339 UTC timestamp ending in Z")
    try:
        parsed = dt.datetime.fromisoformat(value[:-1] + "+00:00")
    except ValueError as error:
        raise CandidateEvidenceError(f"{label} must be a valid RFC3339 timestamp") from error
    if parsed.tzinfo != dt.timezone.utc:
        raise CandidateEvidenceError(f"{label} must use UTC")
    return parsed


def normalize_https_url(value: Any, label: str, *, origin_only: bool = False) -> str:
    if not isinstance(value, str) or not value.strip():
        raise CandidateEvidenceError(f"{label} must be a non-empty HTTPS URL")
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
        raise CandidateEvidenceError(f"{label} must be a credential-free HTTPS URL")
    try:
        port = parsed.port
    except ValueError as error:
        raise CandidateEvidenceError(f"{label} has an invalid port") from error
    host = parsed.hostname.lower()
    if ":" in host and not host.startswith("["):
        host = f"[{host}]"
    authority = host if port is None or port == 443 else f"{host}:{port}"
    path = "" if origin_only else parsed.path.rstrip("/")
    return f"https://{authority}{path}"


def sha256_file(path: pathlib.Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return "sha256:" + digest.hexdigest()


def desktop_artifact_set_sha256(
    evidence_digests: dict[str, dict[str, str]],
) -> str:
    if set(evidence_digests) != DESKTOP_TARGETS:
        raise CandidateEvidenceError("desktop artifact-set evidence is incomplete")
    encoded = "".join(
        f"{target_id}/{field}="
        f"{require_sha256(evidence_digests[target_id].get(field), f'desktop.{target_id}.{field}')}\n"
        for target_id in sorted(DESKTOP_TARGETS)
        for field in DESKTOP_ARTIFACT_SET_EVIDENCE
    ).encode("utf-8")
    return "sha256:" + hashlib.sha256(encoded).hexdigest()


def desktop_enrollment_evidence_set_sha256(evidence_digests: dict[str, str]) -> str:
    if set(evidence_digests) != DESKTOP_TARGETS:
        raise CandidateEvidenceError("desktop Enrollment evidence set is incomplete")
    encoded = "".join(
        f"{target_id}/enrollment="
        f"{require_sha256(evidence_digests[target_id], f'desktop.{target_id}.enrollment')}\n"
        for target_id in sorted(DESKTOP_TARGETS)
    ).encode("utf-8")
    return "sha256:" + hashlib.sha256(encoded).hexdigest()


def resolve_reference(
    value: Any,
    evidence_root: pathlib.Path,
    label: str,
    used_paths: set[pathlib.Path],
) -> tuple[dict[str, str], pathlib.Path]:
    reference = require_exact_fields(value, {"path", "sha256"}, label)
    raw_path = reference["path"]
    if not isinstance(raw_path, str) or not raw_path.strip():
        raise CandidateEvidenceError(f"{label}.path must be a non-empty relative path")
    relative = pathlib.PurePosixPath(raw_path)
    if relative.is_absolute() or any(part in {"", ".", ".."} for part in relative.parts):
        raise CandidateEvidenceError(f"{label}.path must be traversal-free and relative")
    candidate = evidence_root.joinpath(*relative.parts)
    current = candidate
    while current != evidence_root:
        if current.is_symlink():
            raise CandidateEvidenceError(f"{label}.path must not traverse a symlink")
        current = current.parent
    try:
        resolved = candidate.resolve(strict=True)
        resolved.relative_to(evidence_root)
    except (OSError, ValueError) as error:
        raise CandidateEvidenceError(f"{label}.path must resolve inside the evidence root") from error
    if not resolved.is_file() or resolved.stat().st_size > MAX_JSON_BYTES:
        raise CandidateEvidenceError(f"{label}.path must reference a bounded regular file")
    if resolved in used_paths:
        raise CandidateEvidenceError(f"{label}.path duplicates another bundle file")
    used_paths.add(resolved)
    expected_digest = require_sha256(reference["sha256"], f"{label}.sha256")
    actual_digest = sha256_file(resolved)
    if expected_digest != actual_digest:
        raise CandidateEvidenceError(f"{label}.sha256 does not match the referenced file")
    return {"path": relative.as_posix(), "sha256": actual_digest}, resolved


def load_json(path: pathlib.Path, label: str) -> dict[str, Any]:
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as error:
        raise CandidateEvidenceError(f"{label} must be readable UTF-8 JSON") from error
    return require_mapping(payload, label)


def expect_equal(actual: Any, expected: Any, label: str) -> None:
    if actual != expected:
        raise CandidateEvidenceError(f"{label} does not match the release candidate")


def validate_release_evidence(payload: dict[str, Any]) -> dict[str, Any]:
    release = require_exact_fields(
        payload,
        {
            "schemaVersion",
            "release",
            "source",
            "artifacts",
            "deployment",
            "migrations",
            "controls",
            "generatedAt",
            "assessment",
        },
        "releaseEvidence",
    )
    if release["schemaVersion"] != RELEASE_EVIDENCE_SCHEMA:
        raise CandidateEvidenceError("releaseEvidence schemaVersion is unsupported")
    if release["assessment"] != RELEASE_EVIDENCE_ASSESSMENT:
        raise CandidateEvidenceError("releaseEvidence assessment is not the reviewed non-pass value")
    candidate_id = require_identifier(release["release"], "releaseEvidence.release")
    source = require_exact_fields(
        release["source"], {"commit", "clean", "lockfileSha256"}, "releaseEvidence.source"
    )
    source_commit = require_commit(source["commit"], "releaseEvidence.source.commit")
    if require_bool(source["clean"], "releaseEvidence.source.clean") is not True:
        raise CandidateEvidenceError("releaseEvidence must come from an exact clean source commit")
    lockfile_sha256 = require_hex_sha256(
        source["lockfileSha256"], "releaseEvidence.source.lockfileSha256"
    )

    artifacts = require_exact_fields(
        release["artifacts"],
        {
            "controlPlaneImage",
            "workerImage",
            "providerHostImage",
            "webArtifact",
            "adminArtifact",
            "desktopArtifacts",
        },
        "releaseEvidence.artifacts",
    )
    normalized_artifacts = {
        field: require_sha256(artifacts[field], f"releaseEvidence.artifacts.{field}")
        for field in (
            "controlPlaneImage",
            "workerImage",
            "providerHostImage",
            "webArtifact",
            "adminArtifact",
        )
    }
    desktop = require_exact_fields(
        artifacts["desktopArtifacts"], DESKTOP_TARGETS, "releaseEvidence.artifacts.desktopArtifacts"
    )
    normalized_artifacts["desktopArtifacts"] = {
        target: require_sha256(
            desktop[target], f"releaseEvidence.artifacts.desktopArtifacts.{target}"
        )
        for target in sorted(DESKTOP_TARGETS)
    }

    deployment = require_exact_fields(
        release["deployment"],
        {"environmentClass", "environmentId", "regions", "origins"},
        "releaseEvidence.deployment",
    )
    environment_class = deployment["environmentClass"]
    if environment_class not in ENVIRONMENT_CLASSES:
        raise CandidateEvidenceError("releaseEvidence.deployment.environmentClass is not allowed")
    environment_id = require_identifier(
        deployment["environmentId"], "releaseEvidence.deployment.environmentId"
    )
    raw_regions = deployment["regions"]
    if not isinstance(raw_regions, list) or not raw_regions:
        raise CandidateEvidenceError("releaseEvidence.deployment.regions must be non-empty")
    regions = [
        require_identifier(region, f"releaseEvidence.deployment.regions[{index}]")
        for index, region in enumerate(raw_regions)
    ]
    if len(set(regions)) != len(regions) or regions != sorted(regions):
        raise CandidateEvidenceError("releaseEvidence.deployment.regions must be unique and sorted")
    origins = require_exact_fields(
        deployment["origins"],
        {"controlPlaneBaseUrl", "webBaseUrl", "adminBaseUrl"},
        "releaseEvidence.deployment.origins",
    )
    normalized_origins = {
        "controlPlaneBaseUrl": normalize_https_url(
            origins["controlPlaneBaseUrl"], "releaseEvidence.deployment.origins.controlPlaneBaseUrl"
        ),
        "webBaseUrl": normalize_https_url(
            origins["webBaseUrl"], "releaseEvidence.deployment.origins.webBaseUrl", origin_only=True
        ),
        "adminBaseUrl": normalize_https_url(
            origins["adminBaseUrl"], "releaseEvidence.deployment.origins.adminBaseUrl", origin_only=True
        ),
    }
    if normalized_origins["webBaseUrl"] == normalized_origins["adminBaseUrl"]:
        raise CandidateEvidenceError("releaseEvidence Web and Admin origins must be distinct")

    migrations = require_exact_fields(
        release["migrations"], {"tail", "count", "files"}, "releaseEvidence.migrations"
    )
    files = migrations["files"]
    count = migrations["count"]
    if (
        not isinstance(files, list)
        or not files
        or not isinstance(count, int)
        or isinstance(count, bool)
        or count != len(files)
    ):
        raise CandidateEvidenceError("releaseEvidence migration count does not match its file list")
    normalized_files: list[dict[str, str]] = []
    for index, raw in enumerate(files):
        item = require_exact_fields(raw, {"path", "sha256"}, f"releaseEvidence.migrations.files[{index}]")
        path = item["path"]
        if not isinstance(path, str) or pathlib.PurePosixPath(path).name == "":
            raise CandidateEvidenceError("releaseEvidence migration path is invalid")
        name = pathlib.PurePosixPath(path).name
        if MIGRATION_RE.fullmatch(name) is None:
            raise CandidateEvidenceError("releaseEvidence migration name is invalid")
        normalized_files.append(
            {"path": path, "sha256": require_hex_sha256(item["sha256"], "migration sha256")}
        )
    if [item["path"] for item in normalized_files] != sorted(item["path"] for item in normalized_files):
        raise CandidateEvidenceError("releaseEvidence migrations must be sorted")
    tail = require_exact_fields(migrations["tail"], {"path", "sha256"}, "releaseEvidence.migrations.tail")
    if tail != files[-1]:
        raise CandidateEvidenceError("releaseEvidence migration tail must equal the final migration file")
    migration_name = pathlib.PurePosixPath(tail["path"]).name
    migration_tail = {
        "name": migration_name,
        "sha256": "sha256:" + require_hex_sha256(tail["sha256"], "migration tail sha256"),
    }
    require_mapping(release["controls"], "releaseEvidence.controls")
    generated_at = release["generatedAt"]
    parse_utc(generated_at, "releaseEvidence.generatedAt")
    return {
        "candidateId": candidate_id,
        "sourceCommit": source_commit,
        "lockfileSha256": lockfile_sha256,
        "environmentClass": environment_class,
        "environmentId": environment_id,
        "regions": regions,
        "origins": normalized_origins,
        "artifacts": normalized_artifacts,
        "migrationTail": migration_tail,
        "generatedAt": generated_at,
    }


def validate_common_receipt(
    name: str,
    receipt: dict[str, Any],
    receipt_specs: dict[str, tuple[str, str]],
) -> str:
    expected_schema, expected_assessment = receipt_specs[name]
    if receipt.get("schemaVersion") != expected_schema:
        raise CandidateEvidenceError(f"{name} receipt schemaVersion is unsupported")
    if receipt.get("assessment") != expected_assessment:
        raise CandidateEvidenceError(f"{name} receipt assessment is not the reviewed non-pass value")
    validated_at = receipt.get("validatedAt")
    parse_utc(validated_at, f"{name}.validatedAt")
    return validated_at


def validate_candidate_identity(
    value: Any,
    canonical: dict[str, Any],
    label: str,
) -> dict[str, Any]:
    candidate = require_mapping(value, f"{label}.candidate")
    expect_equal(candidate.get("candidateId"), canonical["candidateId"], f"{label}.candidateId")
    expect_equal(candidate.get("sourceCommit"), canonical["sourceCommit"], f"{label}.sourceCommit")
    expect_equal(candidate.get("environment"), canonical["environmentClass"], f"{label}.environment")
    expect_equal(candidate.get("environmentId"), canonical["environmentId"], f"{label}.environmentId")
    return candidate


def validate_desktop_enrollment_projection(
    target: dict[str, Any],
    *,
    target_id: str,
    target_index: int,
    canonical: dict[str, Any],
    window_started_at: dt.datetime,
    window_completed_at: dt.datetime,
) -> str:
    label = f"desktop.targets[{target_index}]"
    if require_bool(target.get("eligible"), f"{label}.eligible") is not True:
        raise CandidateEvidenceError(f"{label} must be eligible")
    if require_bool(
        target.get("nativeExecutionProved"), f"{label}.nativeExecutionProved"
    ) is not True:
        raise CandidateEvidenceError(f"{label} must prove native execution")
    if require_bool(
        target.get("enrollmentComplete"), f"{label}.enrollmentComplete"
    ) is not True:
        raise CandidateEvidenceError(f"{label} Enrollment must be complete")

    manifest_results = require_exact_fields(
        target.get("enrollment"), DESKTOP_ENROLLMENT_FIELDS, f"{label}.enrollment"
    )
    for field in sorted(DESKTOP_ENROLLMENT_FIELDS):
        if require_bool(manifest_results[field], f"{label}.enrollment.{field}") is not True:
            raise CandidateEvidenceError(f"{label}.enrollment.{field} must pass")

    runtime = require_exact_fields(
        target.get("enrollmentRuntimeEvidence"),
        {
            "schemaVersion",
            "candidate",
            "target",
            "startedAt",
            "completedAt",
            "modeTransitions",
            "requestCounts",
            "credentialContinuityMatched",
            "results",
        },
        f"{label}.enrollmentRuntimeEvidence",
    )
    if runtime["schemaVersion"] != DESKTOP_ENROLLMENT_RUNTIME_EVIDENCE_SCHEMA:
        raise CandidateEvidenceError(
            f"{label}.enrollmentRuntimeEvidence schemaVersion is unsupported"
        )
    runtime_candidate = require_exact_fields(
        runtime["candidate"],
        {"candidateId", "sourceCommit", "environmentId", "controlPlaneBaseUrl"},
        f"{label}.enrollmentRuntimeEvidence.candidate",
    )
    expected_candidate = {
        "candidateId": canonical["candidateId"],
        "sourceCommit": canonical["sourceCommit"],
        "environmentId": canonical["environmentId"],
        "controlPlaneBaseUrl": canonical["origins"]["controlPlaneBaseUrl"],
    }
    if runtime_candidate != expected_candidate:
        raise CandidateEvidenceError(
            f"{label}.enrollmentRuntimeEvidence candidate does not match release evidence"
        )

    runner_reference = target.get("runnerReference")
    if not isinstance(runner_reference, str) or not runner_reference.strip():
        raise CandidateEvidenceError(f"{label}.runnerReference must be non-empty")
    expected_runtime_target = {
        "id": target_id,
        "runnerReference": runner_reference,
        "hostArchitecture": DESKTOP_TARGET_ARCHITECTURES[target_id],
        "executionMode": "native",
    }
    runtime_target = require_exact_fields(
        runtime["target"],
        {"id", "runnerReference", "hostArchitecture", "executionMode"},
        f"{label}.enrollmentRuntimeEvidence.target",
    )
    if runtime_target != expected_runtime_target:
        raise CandidateEvidenceError(
            f"{label}.enrollmentRuntimeEvidence target is not the native receipt target"
        )
    expect_equal(
        target.get("hostArchitecture"),
        expected_runtime_target["hostArchitecture"],
        f"{label}.hostArchitecture",
    )
    expect_equal(target.get("executionMode"), "native", f"{label}.executionMode")

    runtime_started_at = parse_utc(
        runtime["startedAt"], f"{label}.enrollmentRuntimeEvidence.startedAt"
    )
    runtime_completed_at = parse_utc(
        runtime["completedAt"], f"{label}.enrollmentRuntimeEvidence.completedAt"
    )
    if (
        runtime_completed_at <= runtime_started_at
        or runtime_started_at < window_started_at
        or runtime_completed_at > window_completed_at
    ):
        raise CandidateEvidenceError(
            f"{label}.enrollmentRuntimeEvidence must remain inside the Desktop receipt window"
        )

    transitions = runtime["modeTransitions"]
    if not isinstance(transitions, list) or len(transitions) != len(
        DESKTOP_MODE_TRANSITION_SPECS
    ):
        raise CandidateEvidenceError(
            f"{label}.enrollmentRuntimeEvidence mode transitions are incomplete"
        )
    previous_observed_at = runtime_started_at
    for transition_index, (raw_transition, expected_transition) in enumerate(
        zip(transitions, DESKTOP_MODE_TRANSITION_SPECS, strict=True)
    ):
        transition = require_exact_fields(
            raw_transition,
            {"id", "from", "to", "observedAt", "passed"},
            f"{label}.enrollmentRuntimeEvidence.modeTransitions[{transition_index}]",
        )
        transition_id, from_mode, to_mode = expected_transition
        if (
            transition.get("id") != transition_id
            or transition.get("from") != from_mode
            or transition.get("to") != to_mode
            or require_bool(
                transition.get("passed"),
                f"{label}.enrollmentRuntimeEvidence.modeTransitions[{transition_index}].passed",
            )
            is not True
        ):
            raise CandidateEvidenceError(
                f"{label}.enrollmentRuntimeEvidence mode transition is not a passing canonical step"
            )
        observed_at = parse_utc(
            transition.get("observedAt"),
            f"{label}.enrollmentRuntimeEvidence.modeTransitions[{transition_index}].observedAt",
        )
        if observed_at <= previous_observed_at or observed_at > runtime_completed_at:
            raise CandidateEvidenceError(
                f"{label}.enrollmentRuntimeEvidence transition timestamps are not ordered"
            )
        previous_observed_at = observed_at

    request_counts = require_exact_fields(
        runtime["requestCounts"],
        {
            "backendBeforeFirstChoice",
            "cloudBeforeFirstChoice",
            "cloudWhileLocal",
        },
        f"{label}.enrollmentRuntimeEvidence.requestCounts",
    )
    for field in sorted(request_counts):
        value = request_counts[field]
        if not isinstance(value, int) or isinstance(value, bool) or value != 0:
            raise CandidateEvidenceError(
                f"{label}.enrollmentRuntimeEvidence.requestCounts.{field} must be zero"
            )
    if require_bool(
        runtime["credentialContinuityMatched"],
        f"{label}.enrollmentRuntimeEvidence.credentialContinuityMatched",
    ) is not True:
        raise CandidateEvidenceError(
            f"{label}.enrollmentRuntimeEvidence credential continuity must pass"
        )
    runtime_results = require_exact_fields(
        runtime["results"],
        DESKTOP_ENROLLMENT_FIELDS,
        f"{label}.enrollmentRuntimeEvidence.results",
    )
    if runtime_results != manifest_results:
        raise CandidateEvidenceError(
            f"{label}.enrollmentRuntimeEvidence results do not match target Enrollment"
        )

    evidence = require_mapping(target.get("evidence"), f"{label}.evidence")
    enrollment_reference = require_exact_fields(
        evidence.get("enrollment"), {"path", "sha256"}, f"{label}.evidence.enrollment"
    )
    return require_sha256(
        enrollment_reference.get("sha256"), f"{label}.evidence.enrollment.sha256"
    )


def validate_receipt(
    name: str,
    receipt: dict[str, Any],
    canonical: dict[str, Any],
    receipt_specs: dict[str, tuple[str, str]],
) -> tuple[bool, str, dict[str, Any]]:
    validated_at = validate_common_receipt(name, receipt, receipt_specs)
    artifacts = canonical["artifacts"]
    origins = canonical["origins"]
    migration_tail = canonical["migrationTail"]
    metadata: dict[str, Any] = {}

    if name == "workerSupplyChain":
        require_exact_fields(
            receipt,
            {"assessment", "controls", "evidence", "schemaVersion", "source", "validatedAt"},
            "workerSupplyChain receipt",
        )
        source = require_exact_fields(
            receipt.get("source"), {"commit", "workerImage"}, "workerSupplyChain.source"
        )
        expect_equal(source.get("commit"), canonical["sourceCommit"], "workerSupplyChain.source.commit")
        expect_equal(
            source.get("workerImage"),
            artifacts["workerImage"],
            "workerSupplyChain.source.workerImage",
        )
        evidence = require_exact_fields(
            receipt.get("evidence"),
            {"admissionReportSha256", "registryReportSha256", "releaseManifestSha256"},
            "workerSupplyChain.evidence",
        )
        for field in sorted(evidence):
            digest = require_hex_sha256(evidence[field], f"workerSupplyChain.evidence.{field}")
            if digest == "0" * 64:
                raise CandidateEvidenceError(
                    f"workerSupplyChain.evidence.{field} must not be an all-zero digest"
                )
        expect_equal(
            evidence["releaseManifestSha256"],
            canonical["releaseEvidenceSha256"].removeprefix("sha256:"),
            "workerSupplyChain.evidence.releaseManifestSha256",
        )
        controls = require_exact_fields(
            receipt.get("controls"),
            WORKER_SUPPLY_CHAIN_CONTROLS,
            "workerSupplyChain.controls",
        )
        if any(value != "pass" for value in controls.values()):
            raise CandidateEvidenceError("workerSupplyChain controls must all pass")
        ready = True
        metadata = {
            "registryReportSha256": "sha256:" + evidence["registryReportSha256"],
            "admissionReportSha256": "sha256:" + evidence["admissionReportSha256"],
        }
    elif name == "billing":
        candidate = validate_candidate_identity(receipt.get("candidate"), canonical, name)
        expect_equal(
            candidate.get("controlPlaneDigest"), artifacts["controlPlaneImage"], "billing.controlPlaneDigest"
        )
        expect_equal(candidate.get("webDigest"), artifacts["webArtifact"], "billing.webDigest")
        expect_equal(
            normalize_https_url(candidate.get("controlPlaneBaseUrl"), "billing.controlPlaneBaseUrl"),
            origins["controlPlaneBaseUrl"],
            "billing.controlPlaneBaseUrl",
        )
        expect_equal(candidate.get("migrationTail"), migration_tail, "billing.migrationTail")
        ready = require_bool(receipt.get("eligibleForHumanGateReview"), "billing.eligibleForHumanGateReview")
    elif name == "internalCost":
        candidate = validate_candidate_identity(receipt.get("candidate"), canonical, name)
        expect_equal(
            normalize_https_url(
                candidate.get("controlPlaneBaseUrl"), "internalCost.controlPlaneBaseUrl"
            ),
            origins["controlPlaneBaseUrl"],
            "internalCost.controlPlaneBaseUrl",
        )
        expect_equal(candidate.get("migrationTail"), migration_tail, "internalCost.migrationTail")
        usage = require_exact_fields(
            receipt.get("usage"),
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
            "internalCost.usage",
        )
        execution_count = require_int(usage.get("executionCount"), "internalCost.usage.executionCount")
        if execution_count < 1:
            raise CandidateEvidenceError("internalCost must cover at least one execution")
        if (
            require_int(
                usage.get("providerCostReportedExecutionCount"),
                "internalCost.usage.providerCostReportedExecutionCount",
            )
            + require_int(
                usage.get("providerCostUnavailableExecutionCount"),
                "internalCost.usage.providerCostUnavailableExecutionCount",
            )
            != execution_count
        ):
            raise CandidateEvidenceError("internalCost Provider coverage is incomplete")
        if (
            require_int(
                usage.get("actualPlatformAllocationCount"),
                "internalCost.usage.actualPlatformAllocationCount",
            )
            + require_int(
                usage.get("estimatedPlatformAllocationCount"),
                "internalCost.usage.estimatedPlatformAllocationCount",
            )
            != execution_count
        ):
            raise CandidateEvidenceError("internalCost platform allocation coverage is incomplete")
        ready = require_bool(
            receipt.get("eligibleForHumanGateReview"),
            "internalCost.eligibleForHumanGateReview",
        )
    elif name == "desktop":
        candidate = validate_candidate_identity(receipt.get("candidate"), canonical, name)
        expect_equal(
            candidate.get("lockfileSha256"), canonical["lockfileSha256"], "desktop.lockfileSha256"
        )
        expect_equal(
            normalize_https_url(candidate.get("controlPlaneBaseUrl"), "desktop.controlPlaneBaseUrl"),
            origins["controlPlaneBaseUrl"],
            "desktop.controlPlaneBaseUrl",
        )
        expect_equal(candidate.get("migrationTail"), migration_tail, "desktop.migrationTail")
        window = require_exact_fields(
            receipt.get("window"), {"startedAt", "completedAt"}, "desktop.window"
        )
        window_started_at = parse_utc(window["startedAt"], "desktop.window.startedAt")
        window_completed_at = parse_utc(
            window["completedAt"], "desktop.window.completedAt"
        )
        if window_completed_at <= window_started_at or window_completed_at > parse_utc(
            validated_at, "desktop.validatedAt"
        ):
            raise CandidateEvidenceError(
                "desktop.window must be ordered and complete before receipt validation"
            )
        targets = receipt.get("targets")
        if not isinstance(targets, list) or len(targets) != len(DESKTOP_TARGETS):
            raise CandidateEvidenceError("desktop.targets must contain all four native targets")
        observed: dict[str, str] = {}
        artifact_set_evidence: dict[str, dict[str, str]] = {}
        enrollment_evidence: dict[str, str] = {}
        for index, raw in enumerate(targets):
            target = require_mapping(raw, f"desktop.targets[{index}]")
            target_id = target.get("id")
            if target_id not in DESKTOP_TARGETS or target_id in observed:
                raise CandidateEvidenceError("desktop.targets contain an invalid or duplicate target")
            observed[target_id] = require_sha256(
                target.get("artifactDigest"), f"desktop.targets[{index}].artifactDigest"
            )
            enrollment_evidence[target_id] = validate_desktop_enrollment_projection(
                target,
                target_id=target_id,
                target_index=index,
                canonical=canonical,
                window_started_at=window_started_at,
                window_completed_at=window_completed_at,
            )
            evidence = require_mapping(target.get("evidence"), f"desktop.targets[{index}].evidence")
            artifact_set_evidence[target_id] = {}
            for field in DESKTOP_ARTIFACT_SET_EVIDENCE:
                reference = require_exact_fields(
                    evidence.get(field),
                    {"path", "sha256"},
                    f"desktop.targets[{index}].evidence.{field}",
                )
                artifact_set_evidence[target_id][field] = require_sha256(
                    reference.get("sha256"),
                    f"desktop.targets[{index}].evidence.{field}.sha256",
                )
        expect_equal(observed, artifacts["desktopArtifacts"], "desktop artifact set")
        metadata["desktopArtifactSetSha256"] = desktop_artifact_set_sha256(
            artifact_set_evidence
        )
        metadata["desktopEnrollmentEvidenceSetSha256"] = (
            desktop_enrollment_evidence_set_sha256(enrollment_evidence)
        )
        ready = require_bool(receipt.get("eligibleForHumanGateReview"), "desktop.eligibleForHumanGateReview")
    elif name == "operations":
        candidate = validate_candidate_identity(receipt.get("candidate"), canonical, name)
        operation_artifacts = require_mapping(candidate.get("artifacts"), "operations.candidate.artifacts")
        for receipt_key, canonical_key in OPERATIONS_ARTIFACTS.items():
            expect_equal(
                operation_artifacts.get(receipt_key),
                artifacts[canonical_key],
                f"operations.artifacts.{receipt_key}",
            )
        expect_equal(
            normalize_https_url(candidate.get("webBaseUrl"), "operations.webBaseUrl", origin_only=True),
            origins["webBaseUrl"],
            "operations.webBaseUrl",
        )
        expect_equal(
            normalize_https_url(candidate.get("adminBaseUrl"), "operations.adminBaseUrl", origin_only=True),
            origins["adminBaseUrl"],
            "operations.adminBaseUrl",
        )
        ready = require_bool(receipt.get("eligibleForHumanGateReview"), "operations.eligibleForHumanGateReview")
    elif name == "recovery":
        candidate = require_exact_fields(
            receipt.get("candidate"),
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
            "recovery.candidate",
        )
        recovery_candidate = {
            "candidateId": canonical["candidateId"],
            "sourceCommit": canonical["sourceCommit"],
            "lockfileSha256": canonical["lockfileSha256"],
            "environmentClass": canonical["environmentClass"],
            "environmentId": canonical["environmentId"],
            "regions": canonical["regions"],
            "origins": origins,
            "artifacts": artifacts,
            "migrationTail": migration_tail,
        }
        expect_equal(candidate, recovery_candidate, "recovery.candidate")
        candidate_binding = "sha256:" + hashlib.sha256(
            json.dumps(
                recovery_candidate, separators=(",", ":"), sort_keys=True
            ).encode("utf-8")
        ).hexdigest()
        expect_equal(
            receipt.get("candidateBindingSha256"),
            candidate_binding,
            "recovery.candidateBindingSha256",
        )
        restored_identity = require_exact_fields(
            receipt.get("restoredReleaseIdentity"),
            {"artifacts", "lockfileSha256", "migrationTail", "sourceCommit"},
            "recovery.restoredReleaseIdentity",
        )
        expect_equal(
            restored_identity,
            {
                "sourceCommit": canonical["sourceCommit"],
                "lockfileSha256": canonical["lockfileSha256"],
                "artifacts": artifacts,
                "migrationTail": migration_tail,
            },
            "recovery.restoredReleaseIdentity",
        )
        started_at = parse_utc(receipt.get("startedAt"), "recovery.startedAt")
        completed_at = parse_utc(receipt.get("completedAt"), "recovery.completedAt")
        validation_time = parse_utc(validated_at, "recovery.validatedAt")
        if not started_at < completed_at <= validation_time:
            raise CandidateEvidenceError("recovery drill and validation timestamps are out of order")
        recovery_subject = require_sha256(
            receipt.get("recoverySubjectSha256"), "recovery.recoverySubjectSha256"
        )
        approvals = require_exact_fields(
            receipt.get("approvals"),
            {"database", "kms", "operations", "security", "storage"},
            "recovery.approvals",
        )
        approver_ids: set[str] = set()
        for role in sorted(approvals):
            approval = require_exact_fields(
                approvals[role],
                {
                    "approvedAt",
                    "approverId",
                    "decision",
                    "evidence",
                    "expiresAt",
                    "role",
                    "subjectSha256",
                },
                f"recovery.approvals.{role}",
            )
            expect_equal(approval.get("role"), role, f"recovery.approvals.{role}.role")
            expect_equal(
                approval.get("subjectSha256"),
                recovery_subject,
                f"recovery.approvals.{role}.subjectSha256",
            )
            if approval.get("decision") != "approved-for-human-gate-review":
                raise CandidateEvidenceError(f"recovery.approvals.{role}.decision is not approved")
            approver_id = require_identifier(
                approval.get("approverId"), f"recovery.approvals.{role}.approverId"
            )
            if approver_id in approver_ids:
                raise CandidateEvidenceError("recovery approval identities must be role-separated")
            approver_ids.add(approver_id)
            evidence = require_exact_fields(
                approval.get("evidence"),
                {"path", "sha256"},
                f"recovery.approvals.{role}.evidence",
            )
            require_identifier(
                evidence.get("path"), f"recovery.approvals.{role}.evidence.path"
            )
            require_sha256(
                evidence.get("sha256"), f"recovery.approvals.{role}.evidence.sha256"
            )
            approved_at = parse_utc(
                approval.get("approvedAt"), f"recovery.approvals.{role}.approvedAt"
            )
            expires_at = parse_utc(
                approval.get("expiresAt"), f"recovery.approvals.{role}.expiresAt"
            )
            if not completed_at <= approved_at <= validation_time < expires_at:
                raise CandidateEvidenceError(
                    f"recovery.approvals.{role} timestamps are out of order"
                )
        verification_boundary = require_exact_fields(
            receipt.get("verificationBoundary"),
            {
                "approvalContentAndSubjectValidated",
                "cryptographicSignaturesVerified",
                "realBackupRestoreAndApproverAuthorityVerificationRequired",
            },
            "recovery.verificationBoundary",
        )
        if verification_boundary != {
            "approvalContentAndSubjectValidated": True,
            "cryptographicSignaturesVerified": False,
            "realBackupRestoreAndApproverAuthorityVerificationRequired": True,
        }:
            raise CandidateEvidenceError(
                "recovery.verificationBoundary must preserve the external-authority-required boundary"
            )
        release_eligible = require_bool(
            receipt.get("releaseEligibleEnvironment"),
            "recovery.releaseEligibleEnvironment",
        )
        expect_equal(
            release_eligible,
            canonical["environmentClass"] in RELEASE_ELIGIBLE_ENVIRONMENTS,
            "recovery.releaseEligibleEnvironment",
        )
        ready = all(
            (
                release_eligible,
                require_bool(
                    receipt.get("declaredMeasurementsWithinObjectives"),
                    "recovery.declaredMeasurementsWithinObjectives",
                ),
                require_bool(
                    receipt.get("allRestoreCanariesPassed"),
                    "recovery.allRestoreCanariesPassed",
                ),
                require_bool(
                    receipt.get("allRequiredApprovalsApproved"),
                    "recovery.allRequiredApprovalsApproved",
                ),
                require_bool(
                    receipt.get("eligibleForHumanGateReview"),
                    "recovery.eligibleForHumanGateReview",
                ),
            )
        )
        metadata = {
            "candidateBindingSha256": candidate_binding,
            "recoverySubjectSha256": recovery_subject,
            "cryptographicSignaturesVerified": False,
            "realBackupRestoreAndApproverAuthorityVerificationRequired": True,
        }
    elif name == "residency":
        candidate = require_exact_fields(
            receipt.get("candidate"),
            {
                "artifacts",
                "candidateId",
                "environmentClass",
                "environmentId",
                "lockfileSha256",
                "migrationTail",
                "sourceCommit",
            },
            "residency.candidate",
        )
        residency_candidate = {
            "candidateId": canonical["candidateId"],
            "sourceCommit": canonical["sourceCommit"],
            "lockfileSha256": canonical["lockfileSha256"],
            "environmentClass": canonical["environmentClass"],
            "environmentId": canonical["environmentId"],
            "artifacts": artifacts,
            "migrationTail": migration_tail,
        }
        expect_equal(candidate, residency_candidate, "residency.candidate")
        candidate_binding = "sha256:" + hashlib.sha256(
            json.dumps(
                residency_candidate, separators=(",", ":"), sort_keys=True
            ).encode("utf-8")
        ).hexdigest()
        expect_equal(
            receipt.get("candidateBindingSha256"),
            candidate_binding,
            "residency.candidateBindingSha256",
        )
        policy = require_exact_fields(
            receipt.get("policy"),
            {
                "allowedRegions",
                "digest",
                "enforcementState",
                "homeRegion",
                "homeRegionAllowed",
                "statementType",
                "version",
            },
            "residency.policy",
        )
        expect_equal(policy.get("allowedRegions"), canonical["regions"], "residency.allowedRegions")
        if policy.get("statementType") != "synara-data-residency-statement-v1":
            raise CandidateEvidenceError("residency.policy.statementType is unsupported")
        policy_version = policy.get("version")
        if isinstance(policy_version, bool) or not isinstance(policy_version, int) or policy_version < 1:
            raise CandidateEvidenceError("residency.policy.version must be an integer >= 1")
        home_region = require_identifier(policy.get("homeRegion"), "residency.policy.homeRegion")
        checks = require_exact_fields(
            receipt.get("checks"),
            {
                "approvalsCurrentThroughReview",
                "attachmentsShareExactSubject",
                "environmentEligible",
                "evacuationAcceptanceSatisfied",
                "failoverAcceptanceSatisfied",
                "fullInventoryKnownDisclosedAndAllowed",
                "homeRegionAllowed",
                "restrictedPolicy",
                "runtimeInventoryMatchesAnnex",
            },
            "residency.checks",
        )
        check_values = [
            require_bool(checks[field], f"residency.checks.{field}")
            for field in sorted(checks)
        ]
        declared_ready = require_bool(
            receipt.get("eligibleForHumanGateReview"),
            "residency.eligibleForHumanGateReview",
        )
        verification_boundary = require_exact_fields(
            receipt.get("verificationBoundary"),
            {
                "cryptographicSignaturesVerified",
                "externalSignatureIdentityAndAuthorityVerificationRequired",
                "strictAttachmentContentAndSubjectValidated",
            },
            "residency.verificationBoundary",
        )
        if (
            verification_boundary.get("strictAttachmentContentAndSubjectValidated") is not True
            or verification_boundary.get("cryptographicSignaturesVerified") is not False
            or verification_boundary.get("externalSignatureIdentityAndAuthorityVerificationRequired") is not True
        ):
            raise CandidateEvidenceError(
                "residency.verificationBoundary must preserve the semantic-only, external-signature-required boundary"
            )
        evidence_set_sha256 = require_sha256(
            receipt.get("evidenceSetSha256"), "residency.evidenceSetSha256"
        )
        ready = all(
            (
                declared_ready,
                receipt.get("promiseScope") == "full-data-residency",
                policy.get("enforcementState") == "restricted",
                all(check_values),
            )
        )
        metadata = {
            "candidateBindingSha256": candidate_binding,
            "evidenceSetSha256": evidence_set_sha256,
            "policyDigest": require_sha256(policy.get("digest"), "residency.policy.digest"),
            "policyVersion": policy_version,
            "homeRegion": home_region,
            "allowedRegions": canonical["regions"],
            "cryptographicSignaturesVerified": False,
            "externalSignatureIdentityAndAuthorityVerificationRequired": True,
        }
    else:
        expect_equal(receipt.get("releaseCommit"), canonical["sourceCommit"], f"{name}.releaseCommit")
        expect_equal(
            receipt.get("environmentClass"), canonical["environmentClass"], f"{name}.environmentClass"
        )
        expect_equal(receipt.get("environmentId"), canonical["environmentId"], f"{name}.environmentId")
        if name == "incident":
            expect_equal(
                normalize_https_url(receipt.get("serviceOrigin"), "incident.serviceOrigin", origin_only=True),
                origins["webBaseUrl"],
                "incident.serviceOrigin",
            )
            ready = require_bool(
                receipt.get("eligibleForHumanGateReview"), "incident.eligibleForHumanGateReview"
            )
        elif name == "slo":
            expect_equal(
                normalize_https_url(receipt.get("publicOrigin"), "slo.publicOrigin", origin_only=True),
                origins["webBaseUrl"],
                "slo.publicOrigin",
            )
            expect_equal(receipt.get("queryRevision"), canonical["sourceCommit"], "slo.queryRevision")
            ready = require_bool(receipt.get("eligibleForHumanGateReview"), "slo.eligibleForHumanGateReview")
        elif name == "penetration":
            raw_assets = receipt.get("assets")
            if not isinstance(raw_assets, list) or len(raw_assets) != len(PENETRATION_ARTIFACTS):
                raise CandidateEvidenceError("penetration.assets must contain every candidate runtime artifact")
            observed_assets: dict[str, str] = {}
            for index, raw in enumerate(raw_assets):
                asset = require_mapping(raw, f"penetration.assets[{index}]")
                asset_type = asset.get("assetType")
                if asset_type not in PENETRATION_ARTIFACTS or asset_type in observed_assets:
                    raise CandidateEvidenceError("penetration.assets contain an invalid or duplicate asset type")
                observed_assets[asset_type] = require_sha256(
                    asset.get("artifactDigest"), f"penetration.assets[{index}].artifactDigest"
                )
            expected_assets = {
                asset_type: artifacts[canonical_key]
                for asset_type, canonical_key in PENETRATION_ARTIFACTS.items()
            }
            expect_equal(observed_assets, expected_assets, "penetration artifact set")
            ready = require_bool(
                receipt.get("eligibleForHumanGateReview"), "penetration.eligibleForHumanGateReview"
            )
        elif name == "capacity":
            ready = all(
                (
                    require_bool(
                        receipt.get("releaseEligibleEnvironment"),
                        "capacity.releaseEligibleEnvironment",
                    ),
                    require_number(
                        receipt.get("externalProbeCoverageRatio"),
                        "capacity.externalProbeCoverageRatio",
                    )
                    >= 0.95,
                    require_bool(receipt.get("forecastHeadroomCovered"), "capacity.forecastHeadroomCovered"),
                    require_bool(
                        receipt.get("declaredMeasurementsWithinObjectives"),
                        "capacity.declaredMeasurementsWithinObjectives",
                    ),
                )
            )
        else:
            raise CandidateEvidenceError(f"unsupported receipt {name}")
    return ready, validated_at, metadata


def validate(
    manifest_path: pathlib.Path,
    evidence_root: pathlib.Path,
    validated_at: str | None,
) -> dict[str, Any]:
    root = evidence_root.resolve()
    if evidence_root.is_symlink() or not root.is_dir():
        raise CandidateEvidenceError("evidence root must be a regular non-symlink directory")
    if manifest_path.is_symlink() or not manifest_path.is_file() or manifest_path.stat().st_size > MAX_JSON_BYTES:
        raise CandidateEvidenceError("bundle manifest must be a bounded regular non-symlink file")
    manifest_digest = sha256_file(manifest_path)
    manifest = require_mapping(load_json(manifest_path, "bundle manifest"), "bundle manifest")
    manifest_schema = manifest.get("schemaVersion")
    if manifest_schema == SCHEMA_VERSION_V2:
        receipt_schema = RECEIPT_SCHEMA_VERSION_V2
        receipt_specs = RECEIPT_SPECS_V2
    elif manifest_schema == SCHEMA_VERSION_V3:
        receipt_schema = RECEIPT_SCHEMA_VERSION_V3
        receipt_specs = RECEIPT_SPECS_V3
    elif manifest_schema == SCHEMA_VERSION_V4:
        receipt_schema = RECEIPT_SCHEMA_VERSION_V4
        receipt_specs = RECEIPT_SPECS_V4
    elif manifest_schema == SCHEMA_VERSION_V5:
        receipt_schema = RECEIPT_SCHEMA_VERSION_V5
        receipt_specs = RECEIPT_SPECS_V4
    else:
        raise CandidateEvidenceError("bundle manifest schemaVersion is unsupported")
    expected_manifest_fields = {"schemaVersion", "releaseEvidence", "receipts"}
    if manifest_schema == SCHEMA_VERSION_V5:
        expected_manifest_fields.add("compatibilityMatrix")
    manifest = require_exact_fields(manifest, expected_manifest_fields, "bundle manifest")
    used_paths: set[pathlib.Path] = set()
    release_reference, release_path = resolve_reference(
        manifest["releaseEvidence"], root, "releaseEvidence", used_paths
    )
    canonical = validate_release_evidence(load_json(release_path, "releaseEvidence"))
    canonical["releaseEvidenceSha256"] = release_reference["sha256"]
    compatibility_projection: dict[str, Any] | None = None
    if manifest_schema == SCHEMA_VERSION_V5:
        compatibility_reference, compatibility_path = resolve_reference(
            manifest["compatibilityMatrix"], root, "compatibilityMatrix", used_paths
        )
        try:
            compatibility = validate_source_compatibility(
                SCRIPTS_ROOT.parent, compatibility_path
            )
        except CompatibilityError as error:
            raise CandidateEvidenceError(
                f"compatibilityMatrix is not current with source: {error}"
            ) from error
        if compatibility["migrationTail"] != canonical["migrationTail"]:
            raise CandidateEvidenceError(
                "compatibilityMatrix migration tail does not match releaseEvidence"
            )
        compatibility_projection = {
            **compatibility_reference,
            "schemaVersion": "synara.release-compatibility-matrix.v1",
            "matrixVersion": 1,
            "assessment": compatibility["assessment"],
            "sourceFileCount": compatibility["sourceFileCount"],
            "sourceByteCount": compatibility["sourceByteCount"],
            "migrationTail": compatibility["migrationTail"],
        }
    raw_receipts = require_exact_fields(manifest["receipts"], set(receipt_specs), "receipts")
    projected_receipts: dict[str, dict[str, Any]] = {}
    receipt_times: list[dt.datetime] = []
    for name in sorted(receipt_specs):
        reference, path = resolve_reference(raw_receipts[name], root, f"receipts.{name}", used_paths)
        receipt = load_json(path, f"{name} receipt")
        ready, receipt_validated_at, metadata = validate_receipt(
            name, receipt, canonical, receipt_specs
        )
        receipt_times.append(parse_utc(receipt_validated_at, f"{name}.validatedAt"))
        projected_receipts[name] = {
            **reference,
            "schemaVersion": receipt_specs[name][0],
            "assessment": receipt_specs[name][1],
            "readyForCandidateReview": ready,
            "validatedAt": receipt_validated_at,
            **metadata,
        }
    timestamp = validated_at or dt.datetime.now(dt.timezone.utc).isoformat().replace("+00:00", "Z")
    validation_time = parse_utc(timestamp, "validatedAt")
    release_generated = parse_utc(canonical["generatedAt"], "releaseEvidence.generatedAt")
    if validation_time < max([release_generated, *receipt_times]):
        raise CandidateEvidenceError("validatedAt must not precede release evidence or receipt validation")
    all_ready = all(receipt["readyForCandidateReview"] for receipt in projected_receipts.values())
    environment_eligible = canonical["environmentClass"] in RELEASE_ELIGIBLE_ENVIRONMENTS
    candidate = {
        **{
            key: value
            for key, value in canonical.items()
            if key not in {"generatedAt", "releaseEvidenceSha256"}
        },
        "desktopArtifactSetSha256": projected_receipts["desktop"]["desktopArtifactSetSha256"],
    }
    result = {
        "schemaVersion": receipt_schema,
        "manifest": {"path": manifest_path.name, "sha256": manifest_digest},
        "candidate": candidate,
        "releaseEvidence": {
            **release_reference,
            "schemaVersion": RELEASE_EVIDENCE_SCHEMA,
            "assessment": RELEASE_EVIDENCE_ASSESSMENT,
            "generatedAt": canonical["generatedAt"],
        },
        "receipts": projected_receipts,
        "requiredReceiptCount": len(receipt_specs),
        "candidateConsistencyValidated": True,
        "environmentEligible": environment_eligible,
        "allRequiredReceiptsReadyForCandidateReview": all_ready,
        "eligibleForCandidateEvidenceReview": environment_eligible and all_ready,
        "assessment": ASSESSMENT,
        "validatedAt": timestamp,
    }
    if compatibility_projection is not None:
        result["compatibilityMatrix"] = compatibility_projection
    return result


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
    except (CandidateEvidenceError, ImmutableEvidenceIOError) as error:
        parser.exit(2, f"candidate evidence validation failed: {error}\n")
    print(
        f"wrote {output}; eligibleForCandidateEvidenceReview="
        f"{str(receipt['eligibleForCandidateEvidenceReview']).lower()}"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
