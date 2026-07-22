from __future__ import annotations

import contextlib
import hashlib
import hmac
import json
import os
import pathlib
import re
import shutil
import subprocess
import sys
import time
import uuid
from collections.abc import Callable, Mapping, Sequence
from typing import Any

import acceptance_runner as acceptance
import controlled_remote_release_gate as remote
import release_gate_common as common


ReleaseGateError = common.ReleaseGateError
GateWorkerImage = remote.GateWorkerImage
RemoteReleaseTargetSpec = remote.RemoteReleaseTargetSpec

CHECKPOINT_SCHEMA_VERSION = remote.CHECKPOINT_SCHEMA_VERSION
CHILD_ATTESTATION_SCHEMA_VERSION = remote.CHILD_ATTESTATION_SCHEMA_VERSION
CHECKPOINT_FILE_NAME = remote.CHECKPOINT_FILE_NAME
CHILD_ATTESTATION_FILE_NAME = remote.CHILD_ATTESTATION_FILE_NAME
STAGING_DIRECTORY_NAME = remote.STAGING_DIRECTORY_NAME
ATTEMPT_DIRECTORY_NAME = remote.ATTEMPT_DIRECTORY_NAME


def controlled_profile_payload(options: Any) -> dict[str, Any]:
    providers: dict[str, Any] = {}
    for provider in common.PROVIDERS:
        source = remote.credential_source(options, provider)
        providers[provider] = {
            "credential": acceptance.read_environment_value(
                source.environment_name,
                f"{provider} real Provider Credential",
                maximum_length=64 << 10,
                forbidden_characters="\r\n\x00",
            ),
            "credentialEnvironmentName": source.environment_name,
            "credentialField": source.field,
            "baseUrl": (
                acceptance.read_environment_value(
                    source.base_url_environment_name,
                    f"{provider} real Provider Base URL",
                    maximum_length=2048,
                    forbidden_characters="\r\n\t\x00",
                )
                if source.base_url_environment_name is not None
                else None
            ),
            "baseUrlEnvironmentName": source.base_url_environment_name,
            "model": remote.provider_model(options, provider),
            "modelEnvironmentName": remote.provider_model_environment_name(options, provider),
        }
    return {"providers": providers}


def controlled_profile_proof(options: Any, nonce: str) -> str:
    if re.fullmatch(r"[0-9a-f]{64}", nonce) is None:
        raise ReleaseGateError(
            "release.checkpoint_profile_nonce_invalid",
            "The release checkpoint Credential-profile nonce is invalid.",
        )
    encoded = json.dumps(
        controlled_profile_payload(options),
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
    ).encode("utf-8")
    return hashlib.sha256(bytes.fromhex(nonce) + b"\0" + encoded).hexdigest()


def _expected_run_pairs(spec: RemoteReleaseTargetSpec) -> tuple[tuple[str, str], ...]:
    return tuple((provider, matrix) for provider in common.PROVIDERS for matrix in spec.matrices)


def _checkpoint_spec(spec: RemoteReleaseTargetSpec) -> dict[str, Any]:
    return {
        "schemaVersion": spec.schema_version,
        "mode": spec.mode,
        "target": spec.target,
        "matrices": list(spec.matrices),
        "providers": list(common.PROVIDERS),
    }


def _checkpoint_path(options: Any) -> pathlib.Path:
    return options.output_dir / CHECKPOINT_FILE_NAME


def _attestation_path(options: Any, provider: str, matrix: str) -> pathlib.Path:
    return options.output_dir / provider / matrix / CHILD_ATTESTATION_FILE_NAME


def _new_checkpoint(
    *,
    options: Any,
    spec: RemoteReleaseTargetSpec,
    run_id: str,
    source: Mapping[str, Any],
    image: GateWorkerImage,
    image_build: Mapping[str, Any] | None = None,
) -> dict[str, Any]:
    nonce = os.urandom(32).hex()
    return {
        "schemaVersion": CHECKPOINT_SCHEMA_VERSION,
        "status": "running" if image_build is not None else "building",
        "runId": run_id,
        "attempt": 1,
        "createdAt": acceptance.utc_now(),
        "updatedAt": acceptance.utc_now(),
        "source": dict(source),
        "spec": _checkpoint_spec(spec),
        "configuration": remote.configuration_evidence(options, spec),
        "controlledProfileProof": {
            "algorithm": "sha256-random-salt-v1",
            "nonce": nonce,
            "digest": controlled_profile_proof(options, nonce),
            "rawValuesPersisted": False,
        },
        "workerImage": {
            "name": image.name,
            "owner": image.owner,
            "target": image.target,
            "id": image_build.get("id") if image_build is not None else None,
            "buildMetadataPath": (
                image_build.get("metadataPath") if image_build is not None else None
            ),
            "buildMetadataSha256": (
                image_build.get("metadataSha256") if image_build is not None else None
            ),
        },
        "completedRuns": [],
        "pendingRuns": [
            {"provider": provider, "matrix": matrix}
            for provider, matrix in _expected_run_pairs(spec)
        ],
    }


def _checkpoint_after_image_build(
    checkpoint: Mapping[str, Any],
    image_build: Mapping[str, Any],
) -> dict[str, Any]:
    image_id = image_build.get("id")
    metadata_path = image_build.get("metadataPath")
    metadata_sha256 = image_build.get("metadataSha256")
    if (
        not isinstance(image_id, str)
        or re.fullmatch(r"sha256:[0-9a-f]{64}", image_id) is None
        or metadata_path != "worker-image-build-metadata.json"
        or not isinstance(metadata_sha256, str)
        or re.fullmatch(r"[0-9a-f]{64}", metadata_sha256) is None
    ):
        raise ReleaseGateError(
            "release.worker_image_build_identity_invalid",
            "The resumable gate Worker build omitted valid image or metadata integrity evidence.",
        )
    worker = checkpoint.get("workerImage")
    if not isinstance(worker, dict):
        raise ReleaseGateError(
            "release.checkpoint_worker_image_invalid",
            "The release checkpoint Worker image identity is invalid.",
        )
    return {
        **dict(checkpoint),
        "status": "running",
        "updatedAt": acceptance.utc_now(),
        "workerImage": {
            **worker,
            "id": image_id,
            "buildMetadataPath": metadata_path,
            "buildMetadataSha256": metadata_sha256,
        },
    }


def _write_checkpoint(options: Any, checkpoint: Mapping[str, Any]) -> None:
    remote._reject_symlink_components(
        options.output_dir,
        _checkpoint_path(options),
        code="release.checkpoint_path_invalid",
        message="The release checkpoint path is unsafe.",
    )
    remote._atomic_write_json(_checkpoint_path(options), checkpoint)


def _checkpoint_with_runs(
    checkpoint: Mapping[str, Any],
    spec: RemoteReleaseTargetSpec,
    runs: Mapping[tuple[str, str], Mapping[str, Any]],
    *,
    status: str,
) -> dict[str, Any]:
    ordered = [
        dict(runs[pair])
        for pair in _expected_run_pairs(spec)
        if pair in runs
    ]
    return {
        **dict(checkpoint),
        "status": status,
        "updatedAt": acceptance.utc_now(),
        "completedRuns": ordered,
        "pendingRuns": [
            {"provider": provider, "matrix": matrix}
            for provider, matrix in _expected_run_pairs(spec)
            if (provider, matrix) not in runs
        ],
    }


def _load_checkpoint(
    options: Any,
    spec: RemoteReleaseTargetSpec,
    source: Mapping[str, Any],
) -> tuple[dict[str, Any], GateWorkerImage, str | None, bool]:
    checkpoint = remote._load_json_object(
        _checkpoint_path(options),
        code="release.checkpoint_missing_or_invalid",
        message="Resume requires a valid gate-owned checkpoint in the output directory.",
        root=options.output_dir,
    )
    if checkpoint.get("schemaVersion") != CHECKPOINT_SCHEMA_VERSION:
        raise ReleaseGateError(
            "release.checkpoint_schema_invalid",
            "The release checkpoint schema is not supported.",
        )
    if checkpoint.get("status") not in {
        "building",
        "running",
        "waiting",
        "finalizing",
        "completed",
    }:
        raise ReleaseGateError(
            "release.checkpoint_status_invalid",
            "The release checkpoint is not resumable.",
        )
    if checkpoint.get("source") != dict(source):
        raise ReleaseGateError(
            "release.checkpoint_source_mismatch",
            "The release checkpoint does not belong to the current clean source SHA.",
        )
    if checkpoint.get("spec") != _checkpoint_spec(spec):
        raise ReleaseGateError(
            "release.checkpoint_spec_mismatch",
            "The release checkpoint target or matrix contract changed.",
        )
    if checkpoint.get("configuration") != remote.configuration_evidence(options, spec):
        raise ReleaseGateError(
            "release.checkpoint_configuration_mismatch",
            "The release checkpoint configuration changed and cannot be resumed.",
        )
    profile = checkpoint.get("controlledProfileProof")
    if not isinstance(profile, dict) or profile.get("algorithm") != "sha256-random-salt-v1":
        raise ReleaseGateError(
            "release.checkpoint_profile_proof_invalid",
            "The release checkpoint Credential-profile proof is invalid.",
        )
    nonce = profile.get("nonce")
    digest = profile.get("digest")
    if not isinstance(nonce, str) or not isinstance(digest, str):
        raise ReleaseGateError(
            "release.checkpoint_profile_proof_invalid",
            "The release checkpoint Credential-profile proof is invalid.",
        )
    if not secrets_compare(digest, controlled_profile_proof(options, nonce)):
        raise ReleaseGateError(
            "release.checkpoint_controlled_profile_mismatch",
            "Provider Credential, Base URL, field, or model changed; cached child evidence cannot be reused.",
        )
    worker = checkpoint.get("workerImage")
    if not isinstance(worker, dict):
        raise ReleaseGateError(
            "release.checkpoint_worker_image_invalid",
            "The release checkpoint Worker image identity is invalid.",
        )
    name = worker.get("name")
    owner = worker.get("owner")
    target = worker.get("target")
    image_id = worker.get("id")
    building = checkpoint.get("status") == "building"
    if building and (
        checkpoint.get("completedRuns") != []
        or checkpoint.get("pendingRuns")
        != [
            {"provider": provider, "matrix": matrix}
            for provider, matrix in _expected_run_pairs(spec)
        ]
        or "aggregateArtifacts" in checkpoint
        or "workerImageCleanup" in checkpoint
    ):
        raise ReleaseGateError(
            "release.checkpoint_build_state_invalid",
            "A building checkpoint contains child or finalization evidence.",
        )
    if (
        not isinstance(name, str)
        or not isinstance(owner, str)
        or re.fullmatch(r"[0-9a-f]{20}", owner) is None
        or target != spec.target
        or (
            not building
            and (
                not isinstance(image_id, str)
                or re.fullmatch(r"sha256:[0-9a-f]{64}", image_id) is None
            )
        )
        or (building and image_id is not None)
    ):
        raise ReleaseGateError(
            "release.checkpoint_worker_image_invalid",
            "The release checkpoint Worker image identity is invalid.",
        )
    image = GateWorkerImage(name=name, owner=owner, target=str(target))
    if image.name != remote.gate_worker_image(str(source["gitSha"]), image.owner, spec).name:
        raise ReleaseGateError(
            "release.checkpoint_worker_image_invalid",
            "The release checkpoint Worker image name is not derived from its source and owner.",
        )
    image_present = False
    observed_image_id: str | None = image_id if isinstance(image_id, str) else None
    if checkpoint.get("status") != "completed":
        try:
            live_image_id, labels = remote.inspect_gate_worker_image(options, image)
        except ReleaseGateError as error:
            missing_allowed = checkpoint.get("status") in {"building", "finalizing"}
            if not missing_allowed or error.code not in {
                "release.worker_image_not_found",
                "release.checkpoint_worker_image_missing",
            }:
                raise
            if checkpoint.get("status") == "finalizing" and isinstance(image_id, str):
                immutable_reference = GateWorkerImage(
                    name=image_id,
                    owner=image.owner,
                    target=image.target,
                )
                try:
                    live_image_id, labels = remote.inspect_gate_worker_image(
                        options,
                        immutable_reference,
                    )
                except ReleaseGateError as immutable_error:
                    if immutable_error.code not in {
                        "release.worker_image_not_found",
                        "release.checkpoint_worker_image_missing",
                    }:
                        raise
                else:
                    image_present = True
                    observed_image_id = live_image_id
                    expected_labels = remote.required_gate_worker_image_labels(
                        image,
                        str(source["gitSha"]),
                    )
                    if live_image_id != image_id or any(
                        labels.get(label) != value
                        for label, value in expected_labels.items()
                    ):
                        raise ReleaseGateError(
                            "release.checkpoint_worker_image_mismatch",
                            "The retained gate-owned Worker image no longer matches its checkpoint identity.",
                        )
        else:
            image_present = True
            observed_image_id = live_image_id
            expected_labels = remote.required_gate_worker_image_labels(image, str(source["gitSha"]))
            if (not building and live_image_id != image_id) or any(
                labels.get(label) != value for label, value in expected_labels.items()
            ):
                raise ReleaseGateError(
                    "release.checkpoint_worker_image_mismatch",
                    "The retained gate-owned Worker image no longer matches its checkpoint identity.",
                )
    metadata_path_value = worker.get("buildMetadataPath")
    metadata_sha256 = worker.get("buildMetadataSha256")
    if building:
        if metadata_path_value is not None or metadata_sha256 is not None:
            raise ReleaseGateError(
                "release.checkpoint_build_metadata_invalid",
                "A building checkpoint contains premature Worker build metadata.",
            )
        return checkpoint, image, observed_image_id, image_present
    if not isinstance(metadata_path_value, str) or not isinstance(metadata_sha256, str):
        raise ReleaseGateError(
            "release.checkpoint_build_metadata_invalid",
            "The release checkpoint omitted Worker build metadata integrity.",
        )
    if metadata_path_value != "worker-image-build-metadata.json":
        raise ReleaseGateError(
            "release.checkpoint_build_metadata_invalid",
            "The release checkpoint Worker build metadata path is invalid.",
        )
    metadata_path = options.output_dir / metadata_path_value
    if (
        metadata_path.is_symlink()
        or not metadata_path.is_file()
        or common.file_sha256(metadata_path) != metadata_sha256
    ):
        raise ReleaseGateError(
            "release.checkpoint_build_metadata_mismatch",
            "The retained Worker build metadata no longer matches its checkpoint.",
        )
    return checkpoint, image, str(image_id), image_present


def secrets_compare(left: str, right: str) -> bool:
    return hmac.compare_digest(left, right)


def _validated_attested_runs(
    *,
    options: Any,
    spec: RemoteReleaseTargetSpec,
    checkpoint: Mapping[str, Any],
    source: Mapping[str, Any],
    image: GateWorkerImage,
    expected_image_id: str,
) -> dict[tuple[str, str], dict[str, Any]]:
    checkpoint_runs: dict[tuple[str, str], Mapping[str, Any]] = {}
    raw_runs = checkpoint.get("completedRuns")
    if not isinstance(raw_runs, list):
        raise ReleaseGateError(
            "release.checkpoint_runs_invalid",
            "The release checkpoint completed-run list is invalid.",
        )
    expected_pairs = set(_expected_run_pairs(spec))
    for value in raw_runs:
        if not isinstance(value, dict):
            raise ReleaseGateError(
                "release.checkpoint_runs_invalid",
                "The release checkpoint completed-run list is invalid.",
            )
        pair = (value.get("provider"), value.get("matrix"))
        if pair not in expected_pairs or pair in checkpoint_runs or value.get("status") != "pass":
            raise ReleaseGateError(
                "release.checkpoint_runs_invalid",
                "The release checkpoint contains an invalid or duplicate completed run.",
            )
        checkpoint_runs[(str(pair[0]), str(pair[1]))] = value

    policy = remote.child_policy(options, spec, image.name)
    validated: dict[tuple[str, str], dict[str, Any]] = {}
    for provider, matrix in _expected_run_pairs(spec):
        child_dir = options.output_dir / provider / matrix
        remote._reject_symlink_components(
            options.output_dir,
            child_dir,
            code="release.child_output_path_invalid",
            message="A reusable child output path is unsafe.",
        )
        attestation_path = _attestation_path(options, provider, matrix)
        if not attestation_path.exists():
            if (provider, matrix) in checkpoint_runs:
                raise ReleaseGateError(
                    "release.child_attestation_missing",
                    "A checkpointed pass child lost its durable attestation.",
                    {"provider": provider, "matrix": matrix},
                )
            continue
        attestation = remote._load_json_object(
            attestation_path,
            code="release.child_attestation_invalid",
            message="A reusable child attestation is invalid.",
            root=options.output_dir,
        )
        record = attestation.get("record")
        if (
            attestation.get("schemaVersion") != CHILD_ATTESTATION_SCHEMA_VERSION
            or attestation.get("gateRunId") != checkpoint.get("runId")
            or not isinstance(record, dict)
            or record.get("provider") != provider
            or record.get("matrix") != matrix
            or record.get("status") != "pass"
        ):
            raise ReleaseGateError(
                "release.child_attestation_invalid",
                "A reusable child attestation is invalid.",
                {"provider": provider, "matrix": matrix},
            )
        checkpoint_record = checkpoint_runs.get((provider, matrix))
        if checkpoint_record is not None and dict(checkpoint_record) != record:
            raise ReleaseGateError(
                "release.child_attestation_checkpoint_mismatch",
                "Child attestation and checkpoint metadata disagree.",
                {"provider": provider, "matrix": matrix},
            )
        report_sha256 = record.get("reportSha256")
        markdown_sha256 = record.get("markdownSha256")
        process_return_code = record.get("processReturnCode")
        duration_ms = record.get("durationMs")
        process_output_scan = record.get("processOutputScan")
        if (
            not isinstance(report_sha256, str)
            or not isinstance(markdown_sha256, str)
            or not isinstance(process_return_code, int)
            or isinstance(process_return_code, bool)
            or not isinstance(duration_ms, int)
            or isinstance(duration_ms, bool)
            or not isinstance(process_output_scan, dict)
            or process_output_scan.get("captured") is not True
            or process_output_scan.get("rawOutputPersisted") is not False
            or process_output_scan.get("findings") != []
        ):
            raise ReleaseGateError(
                "release.child_attestation_invalid",
                "A reusable child attestation omitted fail-closed process evidence.",
                {"provider": provider, "matrix": matrix},
            )
        loaded, child_errors, decoded = common.load_child_report_artifacts(
            output_dir=options.output_dir,
            provider=provider,
            matrix=matrix,
            expected_git_sha=str(source["gitSha"]),
            policy=policy,
            process_return_code=process_return_code,
            duration_ms=duration_ms,
            process_output_scan=process_output_scan,
            expected_report_sha256=report_sha256,
            expected_markdown_sha256=markdown_sha256,
        )
        if child_errors or loaded != record or decoded is None:
            raise ReleaseGateError(
                "release.reused_child_validation_failed",
                "A checkpointed pass child no longer satisfies the release contract.",
                {
                    "provider": provider,
                    "matrix": matrix,
                    "errorCount": len(child_errors),
                },
            )
        if loaded.get("workerImageId") != expected_image_id:
            raise ReleaseGateError(
                "release.reused_child_worker_image_mismatch",
                "A checkpointed pass child does not reference the retained gate image.",
                {"provider": provider, "matrix": matrix},
            )
        if spec.validate_reused_child_runtime is not None:
            runtime_errors = list(spec.validate_reused_child_runtime(options, decoded))
            if runtime_errors:
                raise ReleaseGateError(
                    "release.reused_child_runtime_invalid",
                    "Runtime residue prevents reuse of a checkpointed pass child.",
                    {
                        "provider": provider,
                        "matrix": matrix,
                        "errors": runtime_errors,
                    },
                )
        validated[(provider, matrix)] = loaded
    return validated


def _write_child_attestation(
    options: Any,
    checkpoint: Mapping[str, Any],
    record: Mapping[str, Any],
) -> None:
    provider = str(record["provider"])
    matrix = str(record["matrix"])
    remote._reject_symlink_components(
        options.output_dir,
        _attestation_path(options, provider, matrix),
        code="release.child_attestation_path_invalid",
        message="The child attestation path is unsafe.",
    )
    remote._atomic_write_json(
        _attestation_path(options, provider, matrix),
        {
            "schemaVersion": CHILD_ATTESTATION_SCHEMA_VERSION,
            "gateRunId": checkpoint["runId"],
            "validatedAt": acceptance.utc_now(),
            "record": dict(record),
        },
    )


def _canonical_record_paths(record: Mapping[str, Any], provider: str, matrix: str) -> dict[str, Any]:
    return {
        **dict(record),
        "reportPath": f"{provider}/{matrix}/{acceptance.JSON_REPORT_NAME}",
        "markdownPath": f"{provider}/{matrix}/{acceptance.MARKDOWN_REPORT_NAME}",
    }


def _rewrite_promoted_child_paths(
    child_dir: pathlib.Path,
    *,
    previous_root: pathlib.Path,
) -> None:
    previous = str(previous_root)
    current = str(child_dir)
    for path in sorted(child_dir.rglob("*")):
        if path.is_symlink():
            raise ReleaseGateError(
                "release.promoted_child_symlink_invalid",
                "A promoted child output contains a symbolic link.",
            )
        if not path.is_file() or path.suffix.lower() not in {
            ".json",
            ".log",
            ".md",
            ".txt",
            ".yaml",
            ".yml",
        }:
            continue
        try:
            value = path.read_text(encoding="utf-8")
        except (OSError, UnicodeDecodeError):
            continue
        if previous in value:
            remote._atomic_write_text(path, value.replace(previous, current))


def _archive_resume_inputs(
    options: Any,
    spec: RemoteReleaseTargetSpec,
    checkpoint: Mapping[str, Any],
    reusable_runs: Mapping[tuple[str, str], Mapping[str, Any]],
) -> pathlib.Path:
    attempt = int(checkpoint.get("attempt") or 1) + 1
    attempts_root = options.output_dir / ATTEMPT_DIRECTORY_NAME
    remote._ensure_safe_directory(
        options.output_dir,
        attempts_root,
        code="release.resume_archive_path_invalid",
        message="The release-gate attempt archive path is unsafe.",
    )
    archive = attempts_root / (
        f"attempt-{attempt - 1:02d}-{uuid.uuid4().hex[:8]}"
    )
    if archive.exists() or archive.is_symlink():
        raise ReleaseGateError(
            "release.resume_archive_collision",
            "The release-gate attempt archive path already exists.",
        )
    archive.mkdir(mode=0o700)
    remote._fsync_directory(attempts_root)
    for filename in (
        spec.json_report_name,
        spec.markdown_report_name,
        CHECKPOINT_FILE_NAME,
        "worker-image-build.log",
        "worker-image-build-metadata.json",
    ):
        source = options.output_dir / filename
        if source.is_file() and not source.is_symlink():
            shutil.copy2(source, archive / filename)
    staging = options.output_dir / STAGING_DIRECTORY_NAME
    if staging.exists():
        if staging.is_symlink() or not staging.is_dir():
            raise ReleaseGateError(
                "release.resume_staging_path_invalid",
                "The release-gate staging path is unsafe.",
            )
        remote._durable_replace(staging, archive / "interrupted-staging")
    for provider, matrix in _expected_run_pairs(spec):
        if (provider, matrix) in reusable_runs:
            continue
        child_dir = options.output_dir / provider / matrix
        if not child_dir.exists():
            continue
        if child_dir.is_symlink() or not child_dir.is_dir():
            raise ReleaseGateError(
                "release.resume_child_path_invalid",
                "A pending child output path is unsafe.",
                {"provider": provider, "matrix": matrix},
            )
        destination = archive / provider / matrix
        remote._ensure_safe_directory(
            archive,
            destination.parent,
            code="release.resume_archive_path_invalid",
            message="The release-gate child archive path is unsafe.",
        )
        remote._durable_replace(child_dir, destination)
    return archive


def _retained_image_evidence(
    options: Any,
    image: GateWorkerImage,
    expected_image_id: str,
    git_sha: str,
) -> dict[str, Any]:
    image_id, labels = remote.inspect_gate_worker_image(options, image)
    expected_labels = remote.required_gate_worker_image_labels(image, git_sha)
    if image_id != expected_image_id or any(
        labels.get(label) != value for label, value in expected_labels.items()
    ):
        raise ReleaseGateError(
            "release.worker_image_retention_verification_failed",
            "The resumable gate could not verify its retained Worker image.",
        )
    return {
        "name": image.name,
        "expectedImageId": expected_image_id,
        "presentBeforeCleanup": True,
        "ownershipVerified": True,
        "removed": False,
        "deferredForResume": True,
        "broadCleanupUsed": False,
    }


def _aggregate_artifact_evidence(
    options: Any,
    spec: RemoteReleaseTargetSpec,
) -> dict[str, Any]:
    evidence: dict[str, Any] = {}
    for key, filename in (
        ("json", spec.json_report_name),
        ("markdown", spec.markdown_report_name),
    ):
        path = options.output_dir / filename
        remote._reject_symlink_components(
            options.output_dir,
            path,
            code="release.aggregate_report_path_invalid",
            message="The aggregate report path is unsafe.",
        )
        if not path.is_file():
            raise ReleaseGateError(
                "release.aggregate_report_missing",
                "The completed release gate requires both aggregate report files.",
            )
        evidence[key] = {
            "path": filename,
            "sha256": common.file_sha256(path),
        }
    return evidence


def _validate_completed_aggregate(
    *,
    options: Any,
    spec: RemoteReleaseTargetSpec,
    checkpoint: Mapping[str, Any],
    runs: Mapping[tuple[str, str], Mapping[str, Any]],
) -> tuple[dict[str, Any], pathlib.Path, pathlib.Path]:
    artifacts = checkpoint.get("aggregateArtifacts")
    if not isinstance(artifacts, dict):
        raise ReleaseGateError(
            "release.completed_checkpoint_artifacts_invalid",
            "A completed checkpoint omitted aggregate report integrity evidence.",
        )
    paths: dict[str, pathlib.Path] = {}
    for key, filename in (
        ("json", spec.json_report_name),
        ("markdown", spec.markdown_report_name),
    ):
        artifact = artifacts.get(key)
        if (
            not isinstance(artifact, dict)
            or artifact.get("path") != filename
            or not isinstance(artifact.get("sha256"), str)
            or re.fullmatch(r"[0-9a-f]{64}", str(artifact["sha256"])) is None
        ):
            raise ReleaseGateError(
                "release.completed_checkpoint_artifacts_invalid",
                "A completed checkpoint contains invalid aggregate report integrity evidence.",
            )
        path = options.output_dir / filename
        remote._reject_symlink_components(
            options.output_dir,
            path,
            code="release.completed_checkpoint_report_invalid",
            message="A completed checkpoint aggregate report path is unsafe.",
        )
        if not path.is_file() or common.file_sha256(path) != artifact["sha256"]:
            raise ReleaseGateError(
                "release.completed_checkpoint_report_hash_mismatch",
                "A completed checkpoint aggregate report no longer matches its integrity evidence.",
                {"artifact": key},
            )
        paths[key] = path

    aggregate = remote._load_json_object(
        paths["json"],
        code="release.completed_checkpoint_report_invalid",
        message="A completed checkpoint requires its passing aggregate report.",
        root=options.output_dir,
    )
    expected_runs = [
        dict(runs[pair])
        for pair in _expected_run_pairs(spec)
        if pair in runs
    ]
    worker = checkpoint.get("workerImage")
    aggregate_worker = aggregate.get("workerImage")
    continuation = aggregate.get("continuation")
    coverage = aggregate.get("coverage")
    security = aggregate.get("security")
    output_scan = (
        security.get("aggregateChildOutputScan") if isinstance(security, dict) else None
    )
    cleanup = checkpoint.get("workerImageCleanup")
    if (
        aggregate.get("schemaVersion") != spec.schema_version
        or aggregate.get("runId") != checkpoint.get("runId")
        or aggregate.get("mode") != spec.mode
        or aggregate.get("target") != spec.target
        or aggregate.get("status") != "pass"
        or aggregate.get("configuration") != checkpoint.get("configuration")
        or aggregate.get("runs") != expected_runs
        or aggregate.get("errors") != []
        or not isinstance(worker, dict)
        or not isinstance(aggregate_worker, dict)
        or aggregate_worker.get("name") != worker.get("name")
        or aggregate_worker.get("owner") != worker.get("owner")
        or aggregate_worker.get("id") != worker.get("id")
        or not isinstance(cleanup, dict)
        or cleanup.get("removed") is not True
        or aggregate_worker.get("cleanup") != cleanup
        or not isinstance(continuation, dict)
        or continuation.get("checkpointStatus") != "completed"
        or not isinstance(coverage, dict)
        or coverage.get("requiredRuns") != len(_expected_run_pairs(spec))
        or coverage.get("completedRuns") != len(_expected_run_pairs(spec))
        or not isinstance(security, dict)
        or security.get("credentialEnvironmentNamesPersisted") is not False
        or security.get("gateImageCleanupDeferred") is not False
        or not isinstance(output_scan, dict)
        or output_scan.get("findings") != []
    ):
        raise ReleaseGateError(
            "release.completed_checkpoint_report_invalid",
            "A completed checkpoint aggregate report no longer matches its validated release boundary.",
        )
    aggregate_source = aggregate.get("source")
    checkpoint_source = checkpoint.get("source")
    if (
        not isinstance(aggregate_source, dict)
        or not isinstance(checkpoint_source, dict)
        or aggregate_source.get("gitSha") != checkpoint_source.get("gitSha")
        or aggregate_source.get("worktreeDirty") is not False
    ):
        raise ReleaseGateError(
            "release.completed_checkpoint_report_invalid",
            "A completed checkpoint aggregate report has an invalid source boundary.",
        )
    return aggregate, paths["json"], paths["markdown"]


def run_checkpointed_remote_release_gate(
    options: Any,
    spec: RemoteReleaseTargetSpec,
    *,
    build_image: Callable[[Any, GateWorkerImage, str], dict[str, Any]],
    cleanup_image: Callable[..., tuple[dict[str, Any], ReleaseGateError | None]] | None,
    repository_state: Callable[[pathlib.Path], dict[str, Any]],
    run_child: Callable[..., tuple[dict[str, Any], list[dict[str, Any]]]],
) -> int:
    redactor = remote.credential_redactor(options)
    started_at = acceptance.utc_now()
    started = time.monotonic()
    try:
        source = repository_state(options.repo_root)
        runtime = dict(spec.inspect_runtime(options))
    except (OSError, subprocess.SubprocessError, ReleaseGateError) as raw_error:
        error = (
            raw_error
            if isinstance(raw_error, ReleaseGateError)
            else ReleaseGateError(
                "release.preflight_failed",
                f"{spec.display_name} release gate preflight failed.",
            )
        )
        print(f"{spec.display_name} resumable release gate preflight failed: {error.message}", file=sys.stderr)
        return 2

    payload_entries = remote._output_payload_entries(options.output_dir)
    checkpoint_exists = _checkpoint_path(options).is_file() and not _checkpoint_path(options).is_symlink()
    if payload_entries and not checkpoint_exists:
        print(
            f"{spec.display_name} resumable release gate requires an empty output directory or a valid checkpoint.",
            file=sys.stderr,
        )
        return 2

    resumed = checkpoint_exists
    archive_path: pathlib.Path | None = None
    image_build_attempted = False
    image_cleanup: dict[str, Any]
    initial_reused_count = 0
    executed_count = 0
    recoverable_child_failure = False
    finalizing_image_already_absent = False
    errors: list[dict[str, Any]] = []
    run_records: dict[tuple[str, str], dict[str, Any]] = {}

    if resumed:
        try:
            checkpoint, image, expected_image_id, image_present = _load_checkpoint(
                options,
                spec,
                source,
            )
            resumed_checkpoint_status = str(checkpoint.get("status"))
            finalizing_image_already_absent = (
                resumed_checkpoint_status == "finalizing" and not image_present
            )
            if resumed_checkpoint_status == "building":
                archive_path = _archive_resume_inputs(
                    options,
                    spec,
                    checkpoint,
                    {},
                )
                checkpoint = {
                    **checkpoint,
                    "attempt": int(checkpoint.get("attempt") or 1) + 1,
                    "updatedAt": acceptance.utc_now(),
                }
                _write_checkpoint(options, checkpoint)
                if image_present:
                    if expected_image_id is None:
                        raise ReleaseGateError(
                            "release.checkpoint_worker_image_invalid",
                            "A recovered building checkpoint omitted its observed Worker image ID.",
                        )
                    if cleanup_image is None:
                        _recovery_cleanup, recovery_cleanup_error = remote.cleanup_gate_worker_image(
                            options,
                            image,
                            expected_image_id=expected_image_id,
                        )
                    else:
                        _recovery_cleanup, recovery_cleanup_error = cleanup_image(
                            options,
                            image,
                            expected_image_id=expected_image_id,
                        )
                    if recovery_cleanup_error is not None:
                        raise recovery_cleanup_error
                image_build_attempted = True
                image_build = build_image(options, image, str(source["gitSha"]))
                checkpoint = _checkpoint_after_image_build(checkpoint, image_build)
                _write_checkpoint(options, checkpoint)
                expected_image_id = str(checkpoint["workerImage"]["id"])
            else:
                if expected_image_id is None:
                    raise ReleaseGateError(
                        "release.checkpoint_worker_image_invalid",
                        "The release checkpoint Worker image identity is invalid.",
                    )
                image_build = {
                    "name": image.name,
                    "id": expected_image_id,
                    "status": "retained-from-checkpoint",
                    "metadataPath": checkpoint["workerImage"]["buildMetadataPath"],
                    "metadataSha256": checkpoint["workerImage"]["buildMetadataSha256"],
                    "ownershipVerified": True,
                }
                run_records = _validated_attested_runs(
                    options=options,
                    spec=spec,
                    checkpoint=checkpoint,
                    source=source,
                    image=image,
                    expected_image_id=expected_image_id,
                )
                initial_reused_count = len(run_records)
                if checkpoint.get("status") == "completed":
                    if len(run_records) != len(_expected_run_pairs(spec)):
                        raise ReleaseGateError(
                            "release.completed_checkpoint_coverage_invalid",
                            "A completed checkpoint does not contain every required child attestation.",
                        )
                    _aggregate, aggregate_path, markdown_path = _validate_completed_aggregate(
                        options=options,
                        spec=spec,
                        checkpoint=checkpoint,
                        runs=run_records,
                    )
                    print(
                        f"Stage 3 real Provider {spec.display_name} release gate: "
                        "pass (checkpoint complete)"
                    )
                    print(f"JSON: {aggregate_path}")
                    print(f"Markdown: {markdown_path}")
                    return 0
                archive_path = _archive_resume_inputs(
                    options,
                    spec,
                    checkpoint,
                    run_records,
                )
                checkpoint = {
                    **checkpoint,
                    "status": "running",
                    "attempt": int(checkpoint.get("attempt") or 1) + 1,
                    "updatedAt": acceptance.utc_now(),
                }
                _write_checkpoint(options, checkpoint)
        except (OSError, subprocess.SubprocessError, ReleaseGateError) as raw_error:
            error = (
                raw_error
                if isinstance(raw_error, ReleaseGateError)
                else ReleaseGateError(
                    "release.resume_preflight_failed",
                    "The resumable release gate could not validate its checkpoint.",
                )
            )
            print(f"{spec.display_name} release checkpoint rejected: {error.message}", file=sys.stderr)
            return 2
    else:
        run_id = f"stage3-provider-{spec.target}-release-{uuid.uuid4()}"
        image = remote.gate_worker_image(str(source["gitSha"]), uuid.uuid4().hex[:20], spec)
        image_build = None
        expected_image_id = ""
        checkpoint: dict[str, Any] = {
            "runId": run_id,
            "attempt": 1,
        }
        try:
            checkpoint = _new_checkpoint(
                options=options,
                spec=spec,
                run_id=run_id,
                source=source,
                image=image,
            )
            _write_checkpoint(options, checkpoint)
            image_build_attempted = True
            image_build = build_image(options, image, str(source["gitSha"]))
            checkpoint = _checkpoint_after_image_build(checkpoint, image_build)
            _write_checkpoint(options, checkpoint)
            expected_image_id = str(checkpoint["workerImage"]["id"])
        except Exception as raw_error:
            errors.append(
                (
                    raw_error
                    if isinstance(raw_error, ReleaseGateError)
                    else ReleaseGateError(
                        "release.execution_failed",
                        f"The {spec.display_name} release gate could not build its shared Worker image.",
                    )
                ).as_report_error()
            )

    policy = remote.child_policy(options, spec, image.name)
    attempt = int(checkpoint.get("attempt") or 1)
    staging_root = options.output_dir / STAGING_DIRECTORY_NAME / f"attempt-{attempt:02d}"
    if not errors:
        staging_parent = options.output_dir / STAGING_DIRECTORY_NAME
        remote._ensure_safe_directory(
            options.output_dir,
            staging_parent,
            code="release.resume_staging_path_invalid",
            message="The release-gate staging path is unsafe.",
        )
        if staging_root.exists() or staging_root.is_symlink():
            errors.append(
                ReleaseGateError(
                    "release.resume_staging_collision",
                    "The resumable release gate staging directory already exists.",
                ).as_report_error()
            )
        else:
            staging_root.mkdir(mode=0o700)
            remote._fsync_directory(staging_parent)
    if not errors:
        for provider, matrix in _expected_run_pairs(spec):
            if (provider, matrix) in run_records:
                continue
            staged_child = staging_root / provider / matrix
            try:
                run, child_errors = run_child(
                    repo_root=options.repo_root,
                    output_dir=staging_root,
                    provider=provider,
                    matrix=matrix,
                    expected_git_sha=str(source["gitSha"]),
                    command=spec.child_command(
                        options,
                        provider,
                        matrix,
                        staged_child,
                        image.name,
                    ),
                    policy=policy,
                    environment=remote.child_environment(options, provider),
                    capture_process_output=True,
                    process_output_redactor=redactor,
                    forbidden_output_tokens=remote.operator_environment_names(options),
                )
                executed_count += 1
                canonical_child = options.output_dir / provider / matrix
                if canonical_child.exists() or canonical_child.is_symlink():
                    raise ReleaseGateError(
                        "release.resume_child_promotion_collision",
                        "A canonical child directory unexpectedly exists during atomic promotion.",
                        {"provider": provider, "matrix": matrix},
                    )
                remote._ensure_safe_directory(
                    options.output_dir,
                    canonical_child.parent,
                    code="release.resume_child_promotion_path_invalid",
                    message="The canonical child promotion path is unsafe.",
                )
                remote._reject_symlink_components(
                    staging_root,
                    staged_child,
                    code="release.resume_child_staging_invalid",
                    message="The child staging path is unsafe.",
                )
                if staged_child.is_symlink() or not staged_child.is_dir():
                    raise ReleaseGateError(
                        "release.resume_child_staging_missing",
                        "The child process did not leave a safe staging directory for promotion.",
                        {"provider": provider, "matrix": matrix},
                    )
                remote._durable_replace(staged_child, canonical_child)
                _rewrite_promoted_child_paths(
                    canonical_child,
                    previous_root=staged_child,
                )
                canonical_run, child_errors, _ = common.load_child_report_artifacts(
                    output_dir=options.output_dir,
                    provider=provider,
                    matrix=matrix,
                    expected_git_sha=str(source["gitSha"]),
                    policy=policy,
                    process_return_code=int(run.get("processReturnCode") or 0),
                    duration_ms=int(run.get("durationMs") or 0),
                    process_output_scan=(
                        run.get("processOutputScan")
                        if isinstance(run.get("processOutputScan"), Mapping)
                        else None
                    ),
                )
                canonical_run = _canonical_record_paths(canonical_run, provider, matrix)
                run_records[(provider, matrix)] = canonical_run
                errors.extend(child_errors)
                if child_errors:
                    recoverable_child_failure = not any(
                        error.get("code") == "release.child_process_output_secret_scan_failed"
                        for error in child_errors
                    )
                    break
                _write_child_attestation(options, checkpoint, canonical_run)
                checkpoint = _checkpoint_with_runs(
                    checkpoint,
                    spec,
                    {
                        pair: value
                        for pair, value in run_records.items()
                        if value.get("status") == "pass"
                    },
                    status="running",
                )
                _write_checkpoint(options, checkpoint)
            except Exception as raw_error:
                errors.append(
                    (
                        raw_error
                        if isinstance(raw_error, ReleaseGateError)
                        else ReleaseGateError(
                            "release.execution_failed",
                            f"The {spec.display_name} resumable child execution failed unexpectedly.",
                        )
                    ).as_report_error(provider=provider, matrix=matrix)
                )
                break

    with contextlib.suppress(OSError):
        if staging_root.exists() and not any(
            path.is_file() or path.is_symlink() for path in staging_root.rglob("*")
        ):
            shutil.rmtree(staging_root)
            remote._fsync_directory(staging_root.parent)
            if not any(staging_root.parent.iterdir()):
                staging_root.parent.rmdir()
                remote._fsync_directory(options.output_dir)

    ordered_runs = [
        run_records[pair]
        for pair in _expected_run_pairs(spec)
        if pair in run_records
    ]
    output_secret_scan = remote.scan_child_outputs(options, redactor, spec.matrices)
    environment_name_findings = remote.credential_environment_name_findings(options)
    security_invalid = bool(output_secret_scan.get("findings") or environment_name_findings)
    passed_for_integrity = [run for run in ordered_runs if run.get("status") == "pass"]
    cached_integrity_errors = (
        common.catalog_consensus_errors(passed_for_integrity)
        + common.consensus_errors(
            passed_for_integrity,
            field="workerImageId",
            code="release.worker_image_id_mismatch",
            message="Checkpointed pass children do not reference one shared Worker image ID.",
        )
        + remote.worker_image_reference_errors(passed_for_integrity, expected_image_id or None)
        if passed_for_integrity
        else []
    )
    full_integrity_errors = (
        common.catalog_consensus_errors(ordered_runs)
        + common.consensus_errors(
            ordered_runs,
            field="workerImageId",
            code="release.worker_image_id_mismatch",
            message="Child reports do not reference one shared Worker image ID.",
        )
        + remote.worker_image_reference_errors(ordered_runs, expected_image_id or None)
        if ordered_runs
        else []
    )
    passed_runs = {
        pair: value for pair, value in run_records.items() if value.get("status") == "pass"
    }
    completion_ready_before_cleanup = (
        not errors
        and not security_invalid
        and not full_integrity_errors
        and len(passed_runs) == len(_expected_run_pairs(spec))
    )
    if completion_ready_before_cleanup and _checkpoint_path(options).is_file():
        checkpoint = {
            key: value
            for key, value in dict(checkpoint).items()
            if key not in {"aggregateArtifacts", "workerImageCleanup"}
        }
        checkpoint = _checkpoint_with_runs(
            checkpoint,
            spec,
            passed_runs,
            status="finalizing",
        )
        _write_checkpoint(options, checkpoint)
    retain_for_resume = (
        bool(image_build)
        and recoverable_child_failure
        and not security_invalid
        and not cached_integrity_errors
    )
    cleanup_error: ReleaseGateError | None = None
    if retain_for_resume:
        try:
            image_cleanup = _retained_image_evidence(
                options,
                image,
                expected_image_id,
                str(source["gitSha"]),
            )
        except ReleaseGateError as error:
            errors.append(error.as_report_error())
            retain_for_resume = False
            image_cleanup = {
                "name": image.name,
                "presentBeforeCleanup": False,
                "ownershipVerified": False,
                "removed": False,
                "broadCleanupUsed": False,
            }
    else:
        image_cleanup = {
            "name": image.name,
            "presentBeforeCleanup": False,
            "ownershipVerified": False,
            "removed": False,
            "broadCleanupUsed": False,
        }
    if finalizing_image_already_absent and completion_ready_before_cleanup:
        image_cleanup = {
            "name": image.name,
            "expectedImageId": expected_image_id,
            "presentBeforeCleanup": False,
            "ownershipVerified": True,
            "removed": True,
            "absentBeforeRecoveredFinalization": True,
            "broadCleanupUsed": False,
        }
    elif not retain_for_resume and (image_build_attempted or resumed):
        try:
            if cleanup_image is None:
                image_cleanup, cleanup_error = remote.cleanup_gate_worker_image(
                    options,
                    image,
                    expected_image_id=expected_image_id or None,
                )
            else:
                image_cleanup, cleanup_error = cleanup_image(
                    options,
                    image,
                    expected_image_id=expected_image_id or None,
                )
        except Exception:
            cleanup_error = ReleaseGateError(
                "release.worker_image_cleanup_failed",
                "The gate-owned Worker image cleanup could not run to completion.",
            )
        if cleanup_error is not None:
            errors.append(cleanup_error.as_report_error())

    checkpoint_status = (
        "waiting"
        if retain_for_resume
        else "completed"
        if not errors
        and not security_invalid
        and not full_integrity_errors
        and len(passed_runs) == len(_expected_run_pairs(spec))
        and image_cleanup.get("removed") is True
        else "invalid"
    )
    if (
        checkpoint_status != "completed"
        and isinstance(checkpoint, Mapping)
        and _checkpoint_path(options).is_file()
    ):
        checkpoint = _checkpoint_with_runs(
            checkpoint,
            spec,
            passed_runs,
            status=checkpoint_status,
        )
        _write_checkpoint(options, checkpoint)

    continuation = {
        "checkpointEnabled": True,
        "checkpointStatus": checkpoint_status,
        "attempt": attempt,
        "resumed": resumed,
        "reusedRuns": initial_reused_count,
        "executedRuns": executed_count,
        "retainedWorkerImage": retain_for_resume,
        "controlledProfileValuesPersisted": False,
        **(
            {"archivedAttempt": str(archive_path.relative_to(options.output_dir))}
            if archive_path is not None
            else {}
        ),
    }
    report, status = remote._aggregate_report(
        options=options,
        spec=spec,
        run_id=str(checkpoint.get("runId") or f"stage3-provider-{spec.target}-release-unknown"),
        started_at=started_at,
        started=started,
        source=source,
        runtime=runtime,
        image=image,
        image_build_attempted=image_build_attempted,
        image_build=image_build,
        image_cleanup=image_cleanup,
        runs=ordered_runs,
        errors=errors,
        redactor=redactor,
        output_secret_scan=output_secret_scan,
        environment_name_findings=environment_name_findings,
        continuation=continuation,
    )
    json_path, markdown_path = remote.write_report(report, options.output_dir, redactor, spec)
    if checkpoint_status == "completed" and _checkpoint_path(options).is_file():
        checkpoint = _checkpoint_with_runs(
            checkpoint,
            spec,
            passed_runs,
            status="completed",
        )
        checkpoint = {
            **checkpoint,
            "workerImageCleanup": dict(image_cleanup),
            "aggregateArtifacts": _aggregate_artifact_evidence(options, spec),
        }
        _write_checkpoint(options, checkpoint)
    print(f"Stage 3 real Provider {spec.display_name} release gate: {status}")
    print(f"JSON: {json_path}")
    print(f"Markdown: {markdown_path}")
    return 0 if status == "pass" else 1
