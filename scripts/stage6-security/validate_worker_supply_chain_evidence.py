#!/usr/bin/env python3
"""Bind Worker registry, supply-chain, and admission reports to a Stage 6 release manifest."""

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


SCHEMA_VERSION = "synara.stage6-worker-supply-chain-evidence.v1"
ASSESSMENT = "evidence-validated-not-worker-supply-chain-approved"
RELEASE_SCHEMA = "synara-stage6-release-evidence-v2"
REGISTRY_SCHEMA = "synara.worker-registry-release-gate.v2"
ADMISSION_SCHEMA = "synara.vault-kms-admission-gate.v1"
PLATFORMS = {"linux/amd64", "linux/arm64"}
CONTROLLERS = {"deployment", "statefulset", "job", "cronjob"}
DIGEST_RE = re.compile(r"sha256:[0-9a-f]{64}")
COMMIT_RE = re.compile(r"[0-9a-f]{40}")
SLSA_PREFIX = "https://slsa.dev/provenance/"
SPDX_PREDICATE = "https://spdx.dev/Document"
MAX_INPUT_JSON_BYTES = 32 * 1024 * 1024
PROHIBITED_SECRET_PATTERNS = (
    ("private key", re.compile(rb"-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----")),
    ("AWS access key", re.compile(rb"\bAKIA[0-9A-Z]{16}\b")),
    ("bearer credential", re.compile(rb"(?i)authorization\s*[:=]\s*bearer\s+[^\s,;]+")),
    (
        "credential-bearing URL",
        re.compile(rb"(?i)\b(?:https?|postgres(?:ql)?|mysql)://[^\s/:@]+:[^\s/@]+@"),
    ),
)


class WorkerSupplyChainEvidenceError(Exception):
    pass


def _fail(message: str) -> None:
    raise WorkerSupplyChainEvidenceError(message)


def reject_duplicate_json_fields(pairs: list[tuple[str, Any]]) -> dict[str, Any]:
    result: dict[str, Any] = {}
    for key, value in pairs:
        if key in result:
            _fail(f"JSON contains duplicate field {key!r}")
        result[key] = value
    return result


def scan_for_secret_material(content: bytes, label: str) -> None:
    for secret_label, pattern in PROHIBITED_SECRET_PATTERNS:
        if pattern.search(content):
            _fail(f"{label} contains prohibited {secret_label} material")


def _read_json(path: pathlib.Path, label: str) -> tuple[dict[str, Any], bytes]:
    try:
        raw = read_stable_regular_file(
            path,
            label=label,
            maximum_bytes=MAX_INPUT_JSON_BYTES,
        )
        scan_for_secret_material(raw, label)
        value = json.loads(raw, object_pairs_hook=reject_duplicate_json_fields)
    except ImmutableEvidenceIOError as error:
        raise WorkerSupplyChainEvidenceError(str(error)) from error
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise WorkerSupplyChainEvidenceError(f"{label} must contain valid UTF-8 JSON") from error
    if not isinstance(value, dict):
        _fail(f"{label} must be a JSON object")
    return value, raw


def _object(parent: dict[str, Any], key: str, label: str) -> dict[str, Any]:
    value = parent.get(key)
    if not isinstance(value, dict):
        _fail(f"{label} must be an object")
    return value


def _empty_errors(report: dict[str, Any], label: str) -> None:
    errors = report.get("errors")
    if errors != []:
        _fail(f"{label} must contain an empty errors list")


def _clean_output(report: dict[str, Any], label: str) -> None:
    security = _object(report, "security", f"{label}.security")
    scan = _object(security, "outputSecretScan", f"{label}.security.outputSecretScan")
    if scan.get("findings") != []:
        _fail(f"{label} output secret scan must contain no findings")


def _validate_builds(registry: dict[str, Any], worker_digest: str) -> None:
    builds = registry.get("builds")
    if not isinstance(builds, list) or len(builds) != 2 or not all(isinstance(item, dict) for item in builds):
        _fail("registry report must contain exactly two builds")
    by_slot = {item.get("slot"): item for item in builds}
    if set(by_slot) != {"cached", "no-cache"}:
        _fail("registry builds must contain exactly cached and no-cache slots")
    if by_slot["cached"].get("noCache") is not False or by_slot["no-cache"].get("noCache") is not True:
        _fail("registry build cache modes are invalid")
    for slot, build in by_slot.items():
        if build.get("registryDigest") != worker_digest:
            _fail(f"registry {slot} build digest does not match the Stage 6 Worker digest")
        platforms = build.get("platformDigests")
        predicates = build.get("attestationPredicates")
        if not isinstance(platforms, dict) or set(platforms) != PLATFORMS:
            _fail(f"registry {slot} build must contain exactly both required platform digests")
        if not all(isinstance(value, str) and DIGEST_RE.fullmatch(value) for value in platforms.values()):
            _fail(f"registry {slot} build contains an invalid platform digest")
        if not isinstance(predicates, dict) or set(predicates) != PLATFORMS:
            _fail(f"registry {slot} build must contain attestations for both platforms")
        for platform, values in predicates.items():
            if (
                not isinstance(values, list)
                or SPDX_PREDICATE not in values
                or not any(isinstance(value, str) and value.startswith(SLSA_PREFIX) for value in values)
            ):
                _fail(f"registry {slot} {platform} must contain SPDX and SLSA attestations")
    if by_slot["cached"].get("platformDigests") != by_slot["no-cache"].get("platformDigests"):
        _fail("registry cached and no-cache platform digests are not reproducible")


def _validate_supply_chain(registry: dict[str, Any]) -> None:
    supply_chain = _object(registry, "supplyChain", "registry.supplyChain")
    if supply_chain.get("status") != "pass" or supply_chain.get("errors") != []:
        _fail("registry supply-chain report did not pass cleanly")
    signing = _object(supply_chain, "signing", "registry.supplyChain.signing")
    required_signing = (
        signing.get("mode") == "kms-key"
        and signing.get("productionSigningPolicySatisfied") is True
        and signing.get("transparencyLogVerified") is True
        and signing.get("transparencyLogInclusionProofPresent") is True
        and signing.get("transparencyLogSignedEntryTimestampPresent") is True
    )
    if not required_signing:
        _fail("registry signing evidence does not satisfy the production KMS and transparency-log boundary")
    signatures = signing.get("signatures")
    if not isinstance(signatures, list) or {item.get("slot") for item in signatures if isinstance(item, dict)} != {"cached", "no-cache"}:
        _fail("registry signing evidence must contain cached and no-cache signatures")

    vulnerability = _object(supply_chain, "vulnerability", "registry.supplyChain.vulnerability")
    policy = _object(vulnerability, "policy", "registry vulnerability policy")
    if (
        set(policy.get("blockedSeverities", [])) != {"HIGH", "CRITICAL"}
        or policy.get("ignoreUnfixed") is not False
        or policy.get("failOnEndOfLifeOS") is not True
        or policy.get("maximumDatabaseAgeHours") != 24
        or policy.get("exceptionCount") != 0
    ):
        _fail("registry vulnerability policy is weaker than the Stage 6 GA boundary")
    database = _object(vulnerability, "database", "registry vulnerability database")
    age = database.get("ageSeconds")
    if not isinstance(age, int) or age < -300 or age > 24 * 3600 or database.get("maximumAgeHours") != 24:
        _fail("registry vulnerability database evidence is stale or invalid")
    scans = vulnerability.get("scans")
    if not isinstance(scans, list) or len(scans) != 2 or not all(isinstance(item, dict) for item in scans):
        _fail("registry vulnerability evidence must contain exactly two platform scans")
    if {item.get("platform") for item in scans} != PLATFORMS:
        _fail("registry vulnerability scans do not cover both required platforms")
    for scan in scans:
        os_evidence = scan.get("os")
        if not isinstance(os_evidence, dict) or os_evidence.get("EOSL") is True:
            _fail(f"registry {scan.get('platform')} scan used an end-of-life or unknown OS boundary")
        if scan.get("secretFindingCount") != 0 or scan.get("blockedFindings") != [] or scan.get("waivedFindings") != []:
            _fail(f"registry {scan.get('platform')} scan contains blocked, waived, or secret findings")
    if vulnerability.get("staleExceptionCount") != 0:
        _fail("registry vulnerability evidence contains stale exceptions")

    cleanup = _object(supply_chain, "cleanup", "registry.supplyChain.cleanup")
    if cleanup.get("isolatedStateRemoved") is not True or cleanup.get("signingSecretStateRemoved") is not True or cleanup.get("broadCleanupUsed") is not False:
        _fail("registry supply-chain cleanup boundary was not satisfied")


def _validate_admission(admission: dict[str, Any], *, commit: str, worker_digest: str, registry_sha256: str) -> None:
    if admission.get("schemaVersion") != ADMISSION_SCHEMA or admission.get("status") != "pass":
        _fail("Vault/KMS admission report schema or status is invalid")
    _empty_errors(admission, "admission report")
    _clean_output(admission, "admission report")
    source = _object(admission, "source", "admission.source")
    if source.get("gitSha") != commit:
        _fail("admission report Git SHA does not match the Stage 6 release manifest")
    registry_source = _object(source, "registryReleaseGate", "admission.source.registryReleaseGate")
    if registry_source.get("reportSha256") != registry_sha256:
        _fail("admission report does not bind the exact registry report bytes")
    cached_image = registry_source.get("cachedSignedImage")
    if not isinstance(cached_image, str) or not cached_image.endswith("@" + worker_digest):
        _fail("admission cached signed image does not match the Stage 6 Worker digest")
    for key in (
        "transparencyLogVerified",
        "transparencyLogInclusionProofPresent",
        "transparencyLogSignedEntryTimestampPresent",
    ):
        if registry_source.get(key) is not True:
            _fail(f"admission registry source is missing {key}")
    admission_evidence = _object(admission, "admission", "admission.admission")
    probes = _object(admission_evidence, "probes", "admission probes")
    expected = {"signed": "admitted", "unsigned": "denied", "wrong-key": "denied", "tag-drift": "denied"}
    if set(probes) != set(expected):
        _fail("admission report must contain the exact signed and negative probe set")
    for name, status in expected.items():
        probe = probes.get(name)
        if not isinstance(probe, dict) or probe.get("status") != status:
            _fail(f"admission {name} probe must be {status}")
    controllers = _object(admission_evidence, "controllerProbes", "admission controller probes")
    if set(controllers) != CONTROLLERS:
        _fail("admission report must contain all required controller probes")
    if any(not isinstance(probe, dict) or probe.get("status") != "denied" for probe in controllers.values()):
        _fail("all admission controller probes must deny wrong-key images")
    cleanup = _object(admission, "cleanup", "admission.cleanup")
    if cleanup.get("isolatedStateRemoved") is not True or cleanup.get("exactOwnerCleanup") is not True or cleanup.get("broadCleanupUsed") is not False:
        _fail("admission cleanup boundary was not satisfied")


def validate_evidence(
    release_manifest_path: pathlib.Path,
    registry_report_path: pathlib.Path,
    admission_report_path: pathlib.Path,
    validated_at: str | None = None,
) -> dict[str, Any]:
    release, release_raw = _read_json(release_manifest_path, "Stage 6 release manifest")
    registry, registry_raw = _read_json(registry_report_path, "registry report")
    admission, admission_raw = _read_json(admission_report_path, "Vault/KMS admission report")
    if release.get("schemaVersion") != RELEASE_SCHEMA or release.get("assessment") != "evidence-collected-not-control-passed":
        _fail("Stage 6 release manifest schema or assessment is invalid")
    source = _object(release, "source", "release.source")
    commit = source.get("commit")
    if not isinstance(commit, str) or COMMIT_RE.fullmatch(commit) is None or source.get("clean") is not True:
        _fail("Stage 6 release manifest must bind a clean full Git commit")
    worker_digest = _object(release, "artifacts", "release.artifacts").get("workerImage")
    if not isinstance(worker_digest, str) or DIGEST_RE.fullmatch(worker_digest) is None:
        _fail("Stage 6 release manifest Worker image digest is invalid")

    if registry.get("schemaVersion") != REGISTRY_SCHEMA or registry.get("mode") != "worker-registry-release-gate" or registry.get("status") != "pass":
        _fail("registry report schema, mode, or status is invalid")
    _empty_errors(registry, "registry report")
    _clean_output(registry, "registry report")
    registry_source = _object(registry, "source", "registry.source")
    if registry_source.get("gitSha") != commit or registry_source.get("worktreeDirty") is not False:
        _fail("registry report source does not match the clean Stage 6 release commit")
    configuration = _object(registry, "configuration", "registry.configuration")
    source_supply_chain = _object(registry_source, "supplyChain", "registry.source.supplyChain")
    signing_policy = _object(source_supply_chain, "signingPolicy", "registry signing policy")
    profile = _object(source_supply_chain, "productionSigningProfile", "registry production signing profile")
    if (
        configuration.get("signingPolicyProfile") != "production"
        or source_supply_chain.get("signingPolicyProfile") != "production"
        or signing_policy.get("mode") != "kms-key"
        or signing_policy.get("productionPolicy") is not True
        or not isinstance(profile.get("sha256"), str)
    ):
        _fail("registry report did not use the production KMS signing profile")
    _validate_builds(registry, worker_digest)
    _validate_supply_chain(registry)
    cleanup = _object(registry, "cleanup", "registry.cleanup")
    if cleanup.get("isolatedStateRemoved") is not True or cleanup.get("broadCleanupUsed") is not False:
        _fail("registry release-gate cleanup boundary was not satisfied")

    registry_sha256 = hashlib.sha256(registry_raw).hexdigest()
    _validate_admission(admission, commit=commit, worker_digest=worker_digest, registry_sha256=registry_sha256)
    timestamp = validated_at or dt.datetime.now(dt.timezone.utc).isoformat().replace("+00:00", "Z")
    try:
        parsed_timestamp = dt.datetime.fromisoformat(timestamp.replace("Z", "+00:00"))
    except ValueError as error:
        raise WorkerSupplyChainEvidenceError("validatedAt must be a UTC RFC3339 timestamp") from error
    if not timestamp.endswith("Z") or parsed_timestamp.tzinfo != dt.timezone.utc:
        _fail("validatedAt must be a UTC RFC3339 timestamp")
    return {
        "schemaVersion": SCHEMA_VERSION,
        "source": {"commit": commit, "workerImage": worker_digest},
        "evidence": {
            "releaseManifestSha256": hashlib.sha256(release_raw).hexdigest(),
            "registryReportSha256": registry_sha256,
            "admissionReportSha256": hashlib.sha256(admission_raw).hexdigest(),
        },
        "controls": {
            "multiArchitectureReproducibility": "pass",
            "spdxAndSlsaAttestations": "pass",
            "productionKmsSigningAndTransparencyLog": "pass",
            "vulnerabilityAndSecretPolicy": "pass",
            "signedAndNegativeAdmissionProbes": "pass",
            "exactCleanup": "pass",
        },
        "assessment": ASSESSMENT,
        "validatedAt": timestamp,
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--release-manifest", required=True)
    parser.add_argument("--registry-report", required=True)
    parser.add_argument("--admission-report", required=True)
    parser.add_argument("--output")
    parser.add_argument("--validated-at", help=argparse.SUPPRESS)
    args = parser.parse_args()
    try:
        receipt = validate_evidence(
            pathlib.Path(args.release_manifest).resolve(),
            pathlib.Path(args.registry_report).resolve(),
            pathlib.Path(args.admission_report).resolve(),
            args.validated_at,
        )
        encoded = (json.dumps(receipt, indent=2, sort_keys=True) + "\n").encode("utf-8")
        if args.output:
            output = pathlib.Path(args.output).absolute()
            publish_immutable_with_sha256(output, encoded)
        else:
            print(encoded.decode("utf-8"), end="")
    except (WorkerSupplyChainEvidenceError, ImmutableEvidenceIOError) as error:
        print(f"Worker supply-chain evidence validation failed: {error}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
