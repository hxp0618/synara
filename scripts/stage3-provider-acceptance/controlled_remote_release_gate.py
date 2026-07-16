from __future__ import annotations

import dataclasses
import datetime as dt
import json
import os
import pathlib
import re
import subprocess
import sys
import time
import uuid
from collections.abc import Callable, Mapping, Sequence
from typing import Any

import acceptance_runner as acceptance
import release_gate_common as common


IMAGE_ACCEPTANCE_LABEL = "synara.io/stage3-provider-acceptance"
IMAGE_GATE_LABEL = "synara.io/stage3-provider-release-gate"
IMAGE_OWNER_LABEL = "synara.io/stage3-provider-acceptance-owner"
IMAGE_TARGET_LABEL = "synara.io/stage3-provider-release-gate-target"


@dataclasses.dataclass(frozen=True)
class CredentialSource:
    environment_name: str
    field: str
    base_url_environment_name: str | None


@dataclasses.dataclass(frozen=True)
class GateWorkerImage:
    name: str
    owner: str
    target: str = "docker"


@dataclasses.dataclass(frozen=True)
class RemoteReleaseTargetSpec:
    target: str
    display_name: str
    schema_version: str
    json_report_name: str
    markdown_report_name: str
    mode: str
    worker_image_repository: str
    expected_unsupported: Mapping[tuple[str, str], frozenset[str]]
    cleanup_true_fields: tuple[str, ...]
    cleanup_false_fields: tuple[str, ...]
    worker_image_evidence_path: tuple[str, ...]
    child_command: Callable[[Any, str, str, pathlib.Path, str], Sequence[str]]
    inspect_runtime: Callable[[Any], Mapping[str, Any]]
    target_configuration: Callable[[Any], Mapping[str, Any]]
    evidence_boundary: tuple[str, str]
    runner_executable: str = "provider-host"


ReleaseGateError = common.ReleaseGateError


def parse_credential_source(
    environment_name: str,
    field: str,
    base_url_environment_name: str | None,
    label: str,
) -> CredentialSource:
    parsed_environment = acceptance.parse_environment_variable_name(
        environment_name,
        f"--{label.lower()}-credential-env",
    )
    parsed_base_url_environment = acceptance.parse_environment_variable_name(
        base_url_environment_name,
        f"--{label.lower()}-base-url-env",
    )
    if parsed_environment is None:
        raise ValueError(f"{label} Credential environment variable name is required")
    credential_value = acceptance.read_environment_value(
        parsed_environment,
        f"{label} real Provider Credential",
        maximum_length=64 << 10,
        forbidden_characters="\r\n\x00",
    )
    if len(credential_value) < 6:
        raise ValueError(f"{label} Credential must contain at least 6 characters")
    if parsed_base_url_environment == parsed_environment:
        raise ValueError(
            f"{label} Credential and Base URL must use different environment variables"
        )
    if parsed_base_url_environment is not None:
        acceptance.read_environment_value(
            parsed_base_url_environment,
            f"{label} real Provider Base URL",
            maximum_length=2048,
            forbidden_characters="\r\n\t\x00",
        )
    return CredentialSource(
        environment_name=parsed_environment,
        field=field,
        base_url_environment_name=parsed_base_url_environment,
    )


def credential_source(options: Any, provider: str) -> CredentialSource:
    if provider == "codex":
        return options.codex_credential
    if provider == "claudeAgent":
        return options.claude_credential
    raise ValueError(f"unsupported controlled remote release Provider: {provider}")


def child_policy(
    options: Any,
    spec: RemoteReleaseTargetSpec,
    worker_image_name: str,
) -> common.ChildReportPolicy:
    return common.ChildReportPolicy(
        target=spec.target,
        runner_executable=spec.runner_executable,
        expected_unsupported=spec.expected_unsupported,
        authentication="controlled",
        credential_fields={
            provider: credential_source(options, provider).field for provider in common.PROVIDERS
        },
        controlled_base_urls={
            provider: credential_source(options, provider).base_url_environment_name is not None
            for provider in common.PROVIDERS
        },
        cleanup_true_fields=spec.cleanup_true_fields,
        cleanup_false_fields=spec.cleanup_false_fields,
        expected_worker_image_build="skipped",
        expected_worker_image_name=worker_image_name,
        expected_skip_worker_build=True,
        worker_image_evidence_path=spec.worker_image_evidence_path,
        worker_image_configuration_key=spec.target,
    )


def tool_environment() -> dict[str, str]:
    allowed = (
        "PATH",
        "HOME",
        "TMPDIR",
        "GOCACHE",
        "GOMODCACHE",
        "GOPATH",
        "GOROOT",
        "DOCKER_HOST",
        "DOCKER_CONFIG",
        "XDG_RUNTIME_DIR",
    )
    environment = {key: os.environ[key] for key in allowed if key in os.environ}
    environment.setdefault("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")
    return environment


def docker_environment(options: Any) -> dict[str, str]:
    environment = tool_environment()
    environment.pop("DOCKER_CONTEXT", None)
    environment["DOCKER_HOST"] = f"unix://{options.docker_socket_path}"
    return environment


def gate_worker_image(
    git_sha: str,
    owner: str,
    spec: RemoteReleaseTargetSpec,
) -> GateWorkerImage:
    return GateWorkerImage(
        name=f"{spec.worker_image_repository}:{git_sha[:12]}-{owner}",
        owner=owner,
        target=spec.target,
    )


def docker_completed(
    options: Any,
    arguments: Sequence[str],
    *,
    timeout: float,
) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        ["docker", *arguments],
        cwd=options.repo_root,
        env=docker_environment(options),
        check=False,
        capture_output=True,
        text=True,
        timeout=timeout,
    )


def docker_result_not_found(result: subprocess.CompletedProcess[str]) -> bool:
    output = f"{result.stdout}\n{result.stderr}".lower()
    return any(marker in output for marker in ("no such image", "not found", "notfound"))


def inspect_gate_worker_image(
    options: Any,
    image: GateWorkerImage,
) -> tuple[str, dict[str, Any]]:
    image_id_result = docker_completed(
        options,
        ["image", "inspect", "--format", "{{.Id}}", image.name],
        timeout=15.0,
    )
    labels_result = docker_completed(
        options,
        ["image", "inspect", "--format", "{{json .Config.Labels}}", image.name],
        timeout=15.0,
    )
    image_id = image_id_result.stdout.strip()
    if (
        image_id_result.returncode != 0
        or labels_result.returncode != 0
        or re.fullmatch(r"sha256:[0-9a-f]{64}", image_id) is None
    ):
        if docker_result_not_found(image_id_result) and docker_result_not_found(labels_result):
            raise ReleaseGateError(
                "release.worker_image_not_found",
                "The gate-owned Worker image does not exist.",
            )
        raise ReleaseGateError(
            "release.worker_image_inspect_failed",
            "The gate-owned Worker image could not be inspected after build.",
            {
                "idReturnCode": image_id_result.returncode,
                "labelsReturnCode": labels_result.returncode,
            },
        )
    try:
        labels = json.loads(labels_result.stdout)
    except json.JSONDecodeError:
        labels = None
    if not isinstance(labels, dict):
        raise ReleaseGateError(
            "release.worker_image_labels_invalid",
            "The gate-owned Worker image did not expose a valid label object.",
        )
    return image_id, labels


def required_gate_worker_image_labels(
    image: GateWorkerImage,
    git_sha: str,
) -> dict[str, str]:
    return {
        IMAGE_ACCEPTANCE_LABEL: "true",
        IMAGE_GATE_LABEL: "true",
        IMAGE_OWNER_LABEL: image.owner,
        IMAGE_TARGET_LABEL: image.target,
        "org.opencontainers.image.revision": git_sha,
    }


def build_gate_worker_image(
    options: Any,
    image: GateWorkerImage,
    git_sha: str,
) -> dict[str, Any]:
    metadata_path = options.output_dir / "worker-image-build-metadata.json"
    log_path = options.output_dir / "worker-image-build.log"
    command = [
        str(options.repo_root / "deploy" / "worker" / "build.sh"),
        "--target",
        "worker-acceptance",
        "--image",
        image.name,
        "--git-sha",
        git_sha,
        "--metadata-file",
        str(metadata_path),
        "--label",
        f"{IMAGE_ACCEPTANCE_LABEL}=true",
        "--label",
        f"{IMAGE_GATE_LABEL}=true",
        "--label",
        f"{IMAGE_OWNER_LABEL}={image.owner}",
        "--label",
        f"{IMAGE_TARGET_LABEL}={image.target}",
        "--load",
    ]
    started = time.monotonic()
    try:
        completed = subprocess.run(
            command,
            cwd=options.repo_root,
            env=docker_environment(options),
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            timeout=max(600.0, options.product_timeout_seconds),
        )
    except (OSError, subprocess.TimeoutExpired):
        raise ReleaseGateError(
            "release.worker_image_build_failed",
            "The gate-owned Worker image build could not run to completion.",
        ) from None
    log_path.write_text(completed.stdout, encoding="utf-8")
    if completed.returncode != 0:
        raise ReleaseGateError(
            "release.worker_image_build_failed",
            "The gate-owned Worker image build returned a non-zero status.",
            {"returnCode": completed.returncode, "log": log_path.name},
        )
    image_id, labels = inspect_gate_worker_image(options, image)
    expected_labels = required_gate_worker_image_labels(image, git_sha)
    invalid_labels = sorted(
        label for label, value in expected_labels.items() if labels.get(label) != value
    )
    if invalid_labels:
        raise ReleaseGateError(
            "release.worker_image_ownership_invalid",
            "The built Worker image does not carry the required gate ownership labels.",
            {"invalidLabelKeys": invalid_labels},
        )
    if not metadata_path.is_file():
        raise ReleaseGateError(
            "release.worker_image_metadata_missing",
            "The gate-owned Worker image build did not produce build metadata.",
        )
    return {
        "name": image.name,
        "id": image_id,
        "status": "completed",
        "durationMs": acceptance.elapsed_ms(started),
        "metadataPath": metadata_path.name,
        "metadataSha256": common.file_sha256(metadata_path),
        "logPath": log_path.name,
        "ownershipVerified": True,
    }


def cleanup_gate_worker_image(
    options: Any,
    image: GateWorkerImage,
    *,
    expected_image_id: str | None,
) -> tuple[dict[str, Any], ReleaseGateError | None]:
    evidence: dict[str, Any] = {
        "name": image.name,
        "expectedImageId": expected_image_id,
        "presentBeforeCleanup": False,
        "ownershipVerified": False,
        "removed": False,
        "broadCleanupUsed": False,
    }
    try:
        image_id, labels = inspect_gate_worker_image(options, image)
    except (OSError, subprocess.SubprocessError, ReleaseGateError) as raw_error:
        if isinstance(raw_error, ReleaseGateError) and raw_error.code == "release.worker_image_not_found":
            if expected_image_id is None:
                evidence["absentAfterIncompleteBuild"] = True
                return evidence, None
            return (
                evidence,
                ReleaseGateError(
                    "release.worker_image_missing_before_cleanup",
                    "The shared Worker image disappeared before gate-owned cleanup.",
                ),
            )
        return (
            evidence,
            raw_error
            if isinstance(raw_error, ReleaseGateError)
            else ReleaseGateError(
                "release.worker_image_cleanup_failed",
                "The gate-owned Worker image could not be inspected for cleanup.",
            ),
        )

    evidence["presentBeforeCleanup"] = True
    evidence["imageId"] = image_id
    expected_ownership_labels = {
        IMAGE_ACCEPTANCE_LABEL: "true",
        IMAGE_GATE_LABEL: "true",
        IMAGE_OWNER_LABEL: image.owner,
        IMAGE_TARGET_LABEL: image.target,
    }
    invalid_ownership_keys = sorted(
        key for key, value in expected_ownership_labels.items() if labels.get(key) != value
    )
    if invalid_ownership_keys:
        return (
            evidence,
            ReleaseGateError(
                "release.worker_image_ownership_invalid",
                "Refusing to delete a Worker image without the gate ownership labels.",
                {"invalidLabelKeys": invalid_ownership_keys},
            ),
        )
    if expected_image_id is not None and image_id != expected_image_id:
        return (
            evidence,
            ReleaseGateError(
                "release.worker_image_id_changed_before_cleanup",
                "Refusing to delete a Worker image whose ID changed during the gate.",
            ),
        )
    evidence["ownershipVerified"] = True
    try:
        removal = docker_completed(
            options,
            ["image", "rm", "-f", image_id],
            timeout=60.0,
        )
    except (OSError, subprocess.SubprocessError):
        return (
            evidence,
            ReleaseGateError(
                "release.worker_image_cleanup_failed",
                "The gate-owned Worker image removal command could not complete.",
            ),
        )
    if removal.returncode != 0:
        return (
            evidence,
            ReleaseGateError(
                "release.worker_image_cleanup_failed",
                "The gate-owned Worker image could not be removed.",
                {"returnCode": removal.returncode},
            ),
        )
    try:
        verification = docker_completed(
            options,
            ["image", "inspect", image_id],
            timeout=15.0,
        )
    except (OSError, subprocess.SubprocessError):
        return (
            evidence,
            ReleaseGateError(
                "release.worker_image_cleanup_verification_failed",
                "The gate-owned Worker image cleanup could not be verified.",
            ),
        )
    if verification.returncode == 0:
        return (
            evidence,
            ReleaseGateError(
                "release.worker_image_cleanup_incomplete",
                "The gate-owned Worker image still exists after cleanup.",
            ),
        )
    if not docker_result_not_found(verification):
        return (
            evidence,
            ReleaseGateError(
                "release.worker_image_cleanup_verification_failed",
                "Docker did not confirm that the gate-owned Worker image is absent.",
            ),
        )
    evidence["removed"] = True
    return evidence, None


def child_environment(options: Any, provider: str) -> dict[str, str]:
    environment = docker_environment(options)
    source = credential_source(options, provider)
    environment[source.environment_name] = acceptance.read_environment_value(
        source.environment_name,
        f"{provider} real Provider Credential",
        maximum_length=64 << 10,
        forbidden_characters="\r\n\x00",
    )
    if source.base_url_environment_name is not None:
        environment[source.base_url_environment_name] = acceptance.read_environment_value(
            source.base_url_environment_name,
            f"{provider} real Provider Base URL",
            maximum_length=2048,
            forbidden_characters="\r\n\t\x00",
        )
    return environment


def credential_redactor(options: Any) -> acceptance.SecretRedactor:
    redactor = acceptance.SecretRedactor()
    for provider in common.PROVIDERS:
        source = credential_source(options, provider)
        secret = acceptance.read_environment_value(
            source.environment_name,
            f"{provider} real Provider Credential",
            maximum_length=64 << 10,
            forbidden_characters="\r\n\x00",
        )
        redactor.add(secret, "[REDACTED_REMOTE_RELEASE_CREDENTIAL]")
        if source.base_url_environment_name is not None:
            base_url = acceptance.read_environment_value(
                source.base_url_environment_name,
                f"{provider} real Provider Base URL",
                maximum_length=2048,
                forbidden_characters="\r\n\t\x00",
            ).strip()
            redactor.add(base_url, "[REDACTED_REMOTE_RELEASE_BASE_URL]")
    return redactor


def scan_child_outputs(
    options: Any,
    redactor: acceptance.SecretRedactor,
) -> dict[str, Any]:
    findings: list[dict[str, Any]] = []
    scanned_files = 0
    scanned_bytes = 0
    for provider in common.PROVIDERS:
        for matrix in common.MATRICES:
            child_dir = options.output_dir / provider / matrix
            if not child_dir.is_dir():
                continue
            evidence = acceptance.scan_output_secrets(child_dir, redactor)
            scanned_files += int(evidence.get("scannedFiles") or 0)
            scanned_bytes += int(evidence.get("scannedBytes") or 0)
            for finding in evidence.get("findings", []):
                if isinstance(finding, dict):
                    findings.append(
                        {
                            **finding,
                            "file": f"{provider}/{matrix}/{finding.get('file', '')}",
                        }
                    )
    return {
        "scope": "all four child JSON, Markdown, text metadata, and redacted logs",
        "scannedFiles": scanned_files,
        "scannedBytes": scanned_bytes,
        "findings": findings,
    }


def credential_environment_name_findings(options: Any) -> list[dict[str, Any]]:
    names = {
        value.encode("utf-8")
        for provider in common.PROVIDERS
        for value in (
            credential_source(options, provider).environment_name,
            credential_source(options, provider).base_url_environment_name,
        )
        if value is not None
    }
    patterns = [
        re.compile(rb"(?<![A-Za-z0-9_])" + re.escape(name) + rb"(?![A-Za-z0-9_])")
        for name in names
    ]
    findings: list[dict[str, Any]] = []
    allowed_suffixes = {".json", ".log", ".md", ".txt", ".yaml", ".yml"}
    for path in sorted(options.output_dir.rglob("*")):
        if path.is_symlink() or not path.is_file() or path.suffix.lower() not in allowed_suffixes:
            continue
        overlap = max((len(name) for name in names), default=1) + 1
        carry = b""
        found = False
        with path.open("rb") as source:
            while chunk := source.read(1 << 20):
                window = carry + chunk
                if any(pattern.search(window) is not None for pattern in patterns):
                    found = True
                    break
                carry = window[-overlap:] if overlap > 0 else b""
        if found:
            findings.append({"file": str(path.relative_to(options.output_dir))})
    return findings


def worker_image_reference_errors(
    runs: Sequence[Mapping[str, Any]],
    expected_image_id: str | None,
) -> list[dict[str, Any]]:
    mismatched_runs = [
        {"provider": run.get("provider"), "matrix": run.get("matrix")}
        for run in runs
        if expected_image_id is None or run.get("workerImageId") != expected_image_id
    ]
    if not mismatched_runs:
        return []
    return [
        {
            "code": "release.worker_image_reference_mismatch",
            "message": "A child report did not reference the gate-owned Worker image ID.",
            "evidence": {"runs": mismatched_runs},
        }
    ]


def configuration_evidence(
    options: Any,
    spec: RemoteReleaseTargetSpec,
) -> dict[str, Any]:
    return {
        "runnerCommand": {"executable": spec.runner_executable, "argumentCount": 0},
        "productTimeoutSeconds": options.product_timeout_seconds,
        "failureTimeoutSeconds": options.failure_timeout_seconds,
        "separateChildBoundaries": True,
        "credentialEnvironmentNamesRecordedByGate": False,
        "credentials": {
            provider: {
                "field": credential_source(options, provider).field,
                "controlledBaseUrl": (
                    credential_source(options, provider).base_url_environment_name is not None
                ),
            }
            for provider in common.PROVIDERS
        },
        spec.target: {
            **spec.target_configuration(options),
            "workerImageBuild": "gate-owned-once-from-clean-sha",
            "childWorkerImageBuild": "skipped-shared-gate-image",
        },
    }


def markdown_from_report(
    report: Mapping[str, Any],
    spec: RemoteReleaseTargetSpec,
) -> str:
    lines = [
        f"# Stage 3 Real Provider {spec.display_name} Release Gate",
        "",
        f"- Schema: `{report['schemaVersion']}`",
        f"- Run: `{report['runId']}`",
        f"- Status: **{report['status']}**",
        f"- Git SHA: `{report.get('source', {}).get('gitSha', '')}`",
        f"- Worker Image ID: `{report.get('workerImage', {}).get('id', '')}`",
        f"- Duration: `{report['durationMs']} ms`",
        "",
        "The gate builds one owned Worker acceptance image from the clean SHA, shares it with all four child",
        "runs, then removes it itself. The aggregate passes only when Codex/Claude product and failure reports",
        "share the same Capability Catalog and image ID, use controlled Credentials, clean child resources,",
        "and have empty Secret scans.",
        "",
        "## Child matrices",
        "",
        "| Provider | Matrix | Status | Cases | Unsupported | Worker Image ID | JSON SHA-256 |",
        "| --- | --- | --- | --- | --- | --- | --- |",
    ]
    for run in report.get("runs", []):
        if not isinstance(run, dict):
            continue
        counts = run.get("caseCounts") if isinstance(run.get("caseCounts"), dict) else {}
        case_summary = ", ".join(
            f"{status}={counts.get(status, 0)}"
            for status in ("pass", "unsupported", "skipped", "fail")
        )
        unsupported = ", ".join(run.get("unsupportedCaseIds", [])) or "none"
        lines.append(
            f"| `{run.get('provider', '')}` | `{run.get('matrix', '')}` | {run.get('status', '')} | "
            f"{case_summary} | {unsupported} | `{run.get('workerImageId', '')}` | "
            f"`{run.get('reportSha256', '')}` |"
        )
    errors = report.get("errors")
    if isinstance(errors, list) and errors:
        lines.extend(
            [
                "",
                "## Errors",
                "",
                "```json",
                json.dumps(errors, indent=2, sort_keys=True, ensure_ascii=False),
                "```",
            ]
        )
    lines.extend(
        [
            "",
            "## Evidence boundary",
            "",
            *spec.evidence_boundary,
        ]
    )
    return "\n".join(lines) + "\n"


def write_report(
    report: Mapping[str, Any],
    output_dir: pathlib.Path,
    redactor: acceptance.SecretRedactor,
    spec: RemoteReleaseTargetSpec,
) -> tuple[pathlib.Path, pathlib.Path]:
    sanitized = redactor.value(report)
    encoded = json.dumps(sanitized, indent=2, sort_keys=True, ensure_ascii=False) + "\n"
    json_path = output_dir / spec.json_report_name
    markdown_path = output_dir / spec.markdown_report_name
    json_path.write_text(encoded, encoding="utf-8")
    markdown_path.write_text(markdown_from_report(sanitized, spec), encoding="utf-8")
    return json_path, markdown_path


def failure_report(
    *,
    run_id: str,
    started_at: str,
    started: float,
    options: Any,
    spec: RemoteReleaseTargetSpec,
    error: ReleaseGateError,
) -> dict[str, Any]:
    return {
        "schemaVersion": spec.schema_version,
        "runId": run_id,
        "mode": spec.mode,
        "target": spec.target,
        "status": "fail",
        "startedAt": started_at,
        "finishedAt": acceptance.utc_now(),
        "durationMs": acceptance.elapsed_ms(started),
        "configuration": configuration_evidence(options, spec),
        "runs": [],
        "errors": [error.as_report_error()],
    }


def run_remote_release_gate(
    options: Any,
    spec: RemoteReleaseTargetSpec,
    *,
    build_image: Callable[[Any, GateWorkerImage, str], dict[str, Any]] = build_gate_worker_image,
    cleanup_image: Callable[..., tuple[dict[str, Any], ReleaseGateError | None]] | None = None,
    repository_state: Callable[[pathlib.Path], dict[str, Any]] = common.repository_state,
    run_child: Callable[..., tuple[dict[str, Any], list[dict[str, Any]]]] = common.run_child_report,
) -> int:
    if options.output_dir.exists() and (
        not options.output_dir.is_dir() or any(options.output_dir.iterdir())
    ):
        print(
            f"{spec.display_name} release gate output directory must be empty or absent.",
            file=sys.stderr,
        )
        return 2
    options.output_dir.mkdir(parents=True, exist_ok=True)
    redactor = credential_redactor(options)
    started_at = acceptance.utc_now()
    started = time.monotonic()
    run_id = f"stage3-provider-{spec.target}-release-{uuid.uuid4()}"
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
        report = failure_report(
            run_id=run_id,
            started_at=started_at,
            started=started,
            options=options,
            spec=spec,
            error=error,
        )
        json_path, markdown_path = write_report(report, options.output_dir, redactor, spec)
        print(f"Stage 3 real Provider {spec.display_name} release gate: fail")
        print(f"JSON: {json_path}")
        print(f"Markdown: {markdown_path}")
        return 1

    image = gate_worker_image(str(source["gitSha"]), uuid.uuid4().hex[:20], spec)
    image_build_attempted = False
    image_build: dict[str, Any] | None = None
    image_cleanup: dict[str, Any] = {
        "name": image.name,
        "presentBeforeCleanup": False,
        "ownershipVerified": False,
        "removed": False,
        "broadCleanupUsed": False,
    }
    runs: list[dict[str, Any]] = []
    errors: list[dict[str, Any]] = []
    try:
        image_build_attempted = True
        image_build = build_image(options, image, str(source["gitSha"]))
        policy = child_policy(options, spec, image.name)
        for provider in common.PROVIDERS:
            for matrix in common.MATRICES:
                child_dir = options.output_dir / provider / matrix
                run, child_errors = run_child(
                    repo_root=options.repo_root,
                    output_dir=options.output_dir,
                    provider=provider,
                    matrix=matrix,
                    expected_git_sha=str(source["gitSha"]),
                    command=spec.child_command(options, provider, matrix, child_dir, image.name),
                    policy=policy,
                    environment=child_environment(options, provider),
                )
                runs.append(run)
                errors.extend(child_errors)
    except Exception as raw_error:
        errors.append(
            (
                raw_error
                if isinstance(raw_error, ReleaseGateError)
                else ReleaseGateError(
                    "release.execution_failed",
                    f"The {spec.display_name} release gate could not complete its build or child execution phase.",
                )
            ).as_report_error()
        )
    finally:
        if image_build_attempted:
            expected_image_id = (
                str(image_build["id"])
                if isinstance(image_build, dict) and isinstance(image_build.get("id"), str)
                else None
            )
            try:
                if cleanup_image is None:
                    image_cleanup, cleanup_error = cleanup_gate_worker_image(
                        options,
                        image,
                        expected_image_id=expected_image_id,
                    )
                else:
                    image_cleanup, cleanup_error = cleanup_image(
                        options,
                        image,
                        expected_image_id=expected_image_id,
                    )
            except Exception:
                cleanup_error = ReleaseGateError(
                    "release.worker_image_cleanup_failed",
                    "The gate-owned Worker image cleanup could not run to completion.",
                )
            if cleanup_error is not None:
                errors.append(cleanup_error.as_report_error())

    required_runs = len(common.PROVIDERS) * len(common.MATRICES)
    if len(runs) != required_runs:
        errors.append(
            {
                "code": "release.child_coverage_incomplete",
                "message": f"The {spec.display_name} release gate did not complete all required child matrices.",
                "evidence": {"requiredRuns": required_runs, "completedRuns": len(runs)},
            }
        )
    if runs:
        errors.extend(common.catalog_consensus_errors(runs))
        errors.extend(
            common.consensus_errors(
                runs,
                field="workerImageId",
                code="release.worker_image_id_mismatch",
                message="Child reports do not reference one shared Worker image ID.",
            )
        )
    expected_image_id = (
        str(image_build["id"])
        if isinstance(image_build, dict) and isinstance(image_build.get("id"), str)
        else None
    )
    errors.extend(worker_image_reference_errors(runs, expected_image_id))
    output_secret_scan = scan_child_outputs(options, redactor)
    if output_secret_scan["findings"]:
        errors.append(
            {
                "code": "release.aggregate_secret_scan_failed",
                "message": "Aggregate child output scan found controlled Credential material.",
                "evidence": {"findingCount": len(output_secret_scan["findings"])},
            }
        )
    environment_name_findings = credential_environment_name_findings(options)
    if environment_name_findings:
        errors.append(
            {
                "code": "release.credential_environment_name_persisted",
                "message": "Child output persisted an operator Credential environment-variable name.",
                "evidence": {"files": environment_name_findings},
            }
        )
    status = (
        "pass"
        if not errors
        and len(runs) == required_runs
        and all(run.get("status") == "pass" for run in runs)
        else "fail"
    )
    catalog_hashes = {
        source_value.get("providerCapabilityCatalogSha256")
        for run in runs
        if isinstance((source_value := run.get("source")), dict)
        and isinstance(source_value.get("providerCapabilityCatalogSha256"), str)
    }
    image_ids = {
        run.get("workerImageId") for run in runs if isinstance(run.get("workerImageId"), str)
    }
    shared_image = (
        expected_image_id is not None
        and len(runs) == required_runs
        and image_ids == {expected_image_id}
    )
    child_builds_skipped = shared_image and not any(
        error.get("code") == "release.child_worker_image_invalid" for error in errors
    )
    report = {
        "schemaVersion": spec.schema_version,
        "runId": run_id,
        "mode": spec.mode,
        "target": spec.target,
        "status": status,
        "source": {
            **source,
            "providerCapabilityCatalogSha256": (
                next(iter(catalog_hashes)) if len(catalog_hashes) == 1 else None
            ),
        },
        "runtime": runtime,
        "workerImage": {
            "name": image.name,
            "id": expected_image_id,
            "build": (
                {key: value for key, value in image_build.items() if key not in {"name", "id"}}
                if image_build is not None
                else {"status": "failed" if image_build_attempted else "not-started"}
            ),
            "sharedAcrossRuns": shared_image,
            "childBuildsSkipped": child_builds_skipped,
            "cleanup": image_cleanup,
        },
        "startedAt": started_at,
        "finishedAt": acceptance.utc_now(),
        "durationMs": acceptance.elapsed_ms(started),
        "configuration": configuration_evidence(options, spec),
        "coverage": {
            "requiredRuns": required_runs,
            "completedRuns": len(runs),
            "providers": list(common.PROVIDERS),
            "productCases": list(acceptance.REAL_PROVIDER_CASES),
            "failureCases": list(acceptance.REAL_PROVIDER_FAILURE_CASES),
        },
        "security": {
            "rawChildOutputPersisted": False,
            "childSecretScansRequired": True,
            "childCleanupRequired": True,
            "gateImageCleanupRequired": True,
            "credentialEnvironmentNamesPersisted": bool(environment_name_findings),
            "aggregateChildOutputScan": output_secret_scan,
        },
        "runs": runs,
        "errors": errors,
    }
    json_path, markdown_path = write_report(report, options.output_dir, redactor, spec)
    print(f"Stage 3 real Provider {spec.display_name} release gate: {status}")
    print(f"JSON: {json_path}")
    print(f"Markdown: {markdown_path}")
    return 0 if status == "pass" else 1


def default_output_dir(repo_root: pathlib.Path, target: str) -> pathlib.Path:
    run_id = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    return repo_root / ".tmp" / "stage3-provider-acceptance-results" / f"{run_id}-{target}-release"
