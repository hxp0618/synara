#!/usr/bin/env python3
"""Aggregate clean-SHA real Provider Local matrices into one release-gate report.

The product/capability and controlled-failure matrices remain separate child
runs. This gate verifies that all four Codex/Claude reports come from the same
clean commit and satisfy the required coverage, cleanup, and Secret-scan
boundaries before emitting a consolidated result.
"""

from __future__ import annotations

import argparse
import dataclasses
import datetime as dt
import hashlib
import json
import pathlib
import re
import subprocess
import sys
import time
import uuid
from collections.abc import Mapping, Sequence
from typing import Any

import acceptance_runner as acceptance


SCHEMA_VERSION = "synara.provider-local-release-gate.v1"
JSON_REPORT_NAME = "local-release-gate.json"
MARKDOWN_REPORT_NAME = "local-release-gate.md"
PROVIDERS = ("codex", "claudeAgent")
MATRICES = ("product", "failure")
NODE_MINIMUM = (24, 13, 1)
NODE_MAXIMUM_EXCLUSIVE = (25, 0, 0)
EXPECTED_UNSUPPORTED: Mapping[tuple[str, str], frozenset[str]] = {
    ("codex", "product"): frozenset({"real-provider.terminal-large-log"}),
    ("claudeAgent", "product"): frozenset(
        {"real-provider.terminal-large-log", "real-provider.compact-boundary"}
    ),
    ("codex", "failure"): frozenset(),
    ("claudeAgent", "failure"): frozenset(),
}


@dataclasses.dataclass(frozen=True)
class ReleaseGateOptions:
    repo_root: pathlib.Path
    output_dir: pathlib.Path
    runner_command: tuple[str, str]
    product_timeout_seconds: float
    failure_timeout_seconds: float


class ReleaseGateError(Exception):
    def __init__(
        self,
        code: str,
        message: str,
        evidence: Mapping[str, Any] | None = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.message = message
        self.evidence = dict(evidence or {})

    def as_report_error(
        self,
        *,
        provider: str | None = None,
        matrix: str | None = None,
    ) -> dict[str, Any]:
        return {
            "code": self.code,
            "message": self.message,
            **({"provider": provider} if provider else {}),
            **({"matrix": matrix} if matrix else {}),
            **({"evidence": self.evidence} if self.evidence else {}),
        }


def parse_args(argv: Sequence[str]) -> ReleaseGateOptions:
    repo_root = pathlib.Path(__file__).resolve().parents[2]
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--runner-command-json",
        required=True,
        help="Exact built Provider Host command as [node-24.13.1+, apps/provider-host/dist/index.mjs]",
    )
    parser.add_argument("--output-dir", type=pathlib.Path)
    parser.add_argument("--product-timeout", type=float, default=1800.0)
    parser.add_argument("--failure-timeout", type=float, default=420.0)
    parsed = parser.parse_args(argv)
    if parsed.product_timeout <= 0 or parsed.failure_timeout <= 0:
        parser.error("matrix timeouts must be positive")
    try:
        runner_command = parse_runner_command(parsed.runner_command_json, repo_root)
    except ReleaseGateError as error:
        parser.error(error.message)
    run_id = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    output_dir = (
        parsed.output_dir
        or repo_root / ".tmp" / "stage3-provider-acceptance-results" / f"{run_id}-local-release"
    )
    return ReleaseGateOptions(
        repo_root=repo_root,
        output_dir=output_dir.expanduser().resolve(),
        runner_command=runner_command,
        product_timeout_seconds=parsed.product_timeout,
        failure_timeout_seconds=parsed.failure_timeout,
    )


def parse_runner_command(raw: str, repo_root: pathlib.Path) -> tuple[str, str]:
    try:
        decoded = json.loads(raw)
    except json.JSONDecodeError as error:
        raise ReleaseGateError(
            "release.runner_command_invalid",
            f"--runner-command-json is invalid JSON: {error.msg}",
        ) from None
    if not isinstance(decoded, list) or len(decoded) != 2:
        raise ReleaseGateError(
            "release.runner_command_invalid",
            "--runner-command-json must contain exactly the Node executable and built Provider Host path.",
        )
    if not all(isinstance(value, str) and value.strip() for value in decoded):
        raise ReleaseGateError(
            "release.runner_command_invalid",
            "--runner-command-json entries must be non-empty strings.",
        )
    node_path = pathlib.Path(decoded[0]).expanduser().resolve()
    host_path = pathlib.Path(decoded[1]).expanduser().resolve()
    expected_host = (repo_root / "apps" / "provider-host" / "dist" / "index.mjs").resolve()
    if node_path.name not in {"node", "node.exe"}:
        raise ReleaseGateError(
            "release.node_executable_invalid",
            "The consolidated Local gate requires a direct Node executable.",
        )
    if host_path != expected_host:
        raise ReleaseGateError(
            "release.provider_host_path_invalid",
            "The Provider Host must be the current repository apps/provider-host/dist/index.mjs.",
        )
    if not node_path.is_file():
        raise ReleaseGateError(
            "release.runner_command_missing",
            "The Node executable must exist before the Local gate starts.",
        )
    return str(node_path), str(host_path)


def parse_node_version(value: str) -> tuple[int, int, int]:
    match = re.fullmatch(r"v?(\d+)\.(\d+)\.(\d+)", value.strip())
    if match is None:
        raise ReleaseGateError(
            "release.node_version_invalid",
            "Node --version did not return a three-component semantic version.",
        )
    major, minor, patch = (int(component) for component in match.groups())
    return major, minor, patch


def inspect_node_runtime(node_path: str) -> dict[str, Any]:
    completed = subprocess.run(
        [node_path, "--version"],
        check=False,
        capture_output=True,
        text=True,
        timeout=15.0,
    )
    if completed.returncode != 0:
        raise ReleaseGateError(
            "release.node_version_probe_failed",
            "The configured Node executable did not return its version.",
            {"returnCode": completed.returncode},
        )
    version = parse_node_version(completed.stdout)
    if version < NODE_MINIMUM or version >= NODE_MAXIMUM_EXCLUSIVE:
        raise ReleaseGateError(
            "release.node_version_unsupported",
            "The consolidated Local gate requires Node >=24.13.1 and <25.0.0.",
            {"actualVersion": ".".join(str(component) for component in version)},
        )
    return {
        "path": str(pathlib.Path(node_path).resolve()),
        "version": ".".join(str(component) for component in version),
        "requiredRange": ">=24.13.1 <25.0.0",
    }


def repository_state(repo_root: pathlib.Path) -> dict[str, Any]:
    sha = subprocess.run(
        ["git", "rev-parse", "HEAD"],
        cwd=repo_root,
        check=True,
        capture_output=True,
        text=True,
    ).stdout.strip()
    status = subprocess.run(
        ["git", "status", "--porcelain", "--untracked-files=all"],
        cwd=repo_root,
        check=True,
        capture_output=True,
        text=True,
    ).stdout
    if status.strip():
        raise ReleaseGateError(
            "release.worktree_dirty",
            "The consolidated Local release gate requires a clean worktree with no untracked files.",
        )
    return {"gitSha": sha, "worktreeDirty": False}


def build_provider_host(options: ReleaseGateOptions) -> dict[str, Any]:
    started = time.monotonic()
    completed = subprocess.run(
        ["bun", "run", "--cwd", "apps/provider-host", "build"],
        cwd=options.repo_root,
        check=False,
    )
    if completed.returncode != 0:
        raise ReleaseGateError(
            "release.provider_host_build_failed",
            "Provider Host build failed before the consolidated Local matrices.",
            {"returnCode": completed.returncode},
        )
    host_path = pathlib.Path(options.runner_command[1])
    if not host_path.is_file():
        raise ReleaseGateError(
            "release.provider_host_build_missing",
            "Provider Host build completed without producing dist/index.mjs.",
        )
    return {
        "status": "pass",
        "durationMs": round((time.monotonic() - started) * 1000),
        "output": str(host_path.relative_to(options.repo_root)),
        "sha256": file_sha256(host_path),
    }


def child_command(
    options: ReleaseGateOptions,
    provider: str,
    matrix: str,
    output_dir: pathlib.Path,
) -> list[str]:
    timeout = (
        options.product_timeout_seconds
        if matrix == "product"
        else options.failure_timeout_seconds
    )
    matrix_flag = "--real-provider-matrix" if matrix == "product" else "--real-provider-failure-matrix"
    return [
        sys.executable,
        str(options.repo_root / "scripts" / "stage3-provider-acceptance" / "acceptance_runner.py"),
        "--suite",
        "real-provider-smoke",
        "--target",
        "local",
        "--provider",
        provider,
        "--runner-command-json",
        json.dumps(list(options.runner_command), separators=(",", ":")),
        matrix_flag,
        "--output-dir",
        str(output_dir),
        "--timeout",
        str(timeout),
    ]


def expected_case_ids(matrix: str) -> frozenset[str]:
    if matrix == "product":
        return frozenset(
            metadata["id"] for metadata in acceptance.REAL_PROVIDER_CASE_METADATA.values()
        )
    return frozenset(
        metadata["id"] for metadata in acceptance.REAL_PROVIDER_FAILURE_CASE_METADATA.values()
    )


def validate_child_report(
    report: Mapping[str, Any],
    *,
    provider: str,
    matrix: str,
    expected_git_sha: str,
) -> list[dict[str, Any]]:
    errors: list[dict[str, Any]] = []

    def fail(code: str, message: str, evidence: Mapping[str, Any] | None = None) -> None:
        errors.append(
            {
                "code": code,
                "message": message,
                "provider": provider,
                "matrix": matrix,
                **({"evidence": dict(evidence)} if evidence else {}),
            }
        )

    if report.get("schemaVersion") != acceptance.SCHEMA_VERSION:
        fail("release.child_schema_invalid", "Child report schema is not the Provider acceptance schema.")
    if report.get("provider") != provider or report.get("target") != "local":
        fail(
            "release.child_identity_mismatch",
            "Child report Provider or Target does not match the requested Local run.",
        )
    if report.get("mode") != "real-provider-smoke" or report.get("status") != "pass":
        fail(
            "release.child_status_invalid",
            "Child report is not a passing real Provider smoke report.",
            {"status": report.get("status"), "mode": report.get("mode")},
        )

    source = report.get("source")
    if not isinstance(source, dict):
        fail("release.child_source_missing", "Child report omitted source metadata.")
    else:
        if source.get("gitSha") != expected_git_sha or source.get("worktreeDirty") is not False:
            fail(
                "release.child_source_mismatch",
                "Child report did not use the expected clean Git SHA.",
                {"gitSha": source.get("gitSha"), "worktreeDirty": source.get("worktreeDirty")},
            )
        catalog_hash = source.get("providerCapabilityCatalogSha256")
        if not isinstance(catalog_hash, str) or re.fullmatch(r"[0-9a-f]{64}", catalog_hash) is None:
            fail(
                "release.child_catalog_hash_invalid",
                "Child report omitted a valid Provider Capability Catalog hash.",
            )

    configuration = report.get("configuration")
    real_provider = configuration.get("realProvider") if isinstance(configuration, dict) else None
    if not isinstance(configuration, dict) or not isinstance(real_provider, dict):
        fail("release.child_configuration_missing", "Child report omitted real Provider configuration.")
    else:
        if configuration.get("restartControlPlane") is not True or configuration.get("keepState") is not False:
            fail(
                "release.child_lifecycle_invalid",
                "Child report must restart the Control Plane and remove its isolated state.",
            )
        runner = configuration.get("runnerCommand")
        if not isinstance(runner, dict) or runner.get("executable") != "node":
            fail(
                "release.child_runner_invalid",
                "Child report did not use the required direct Node Provider Host command.",
            )
        expected_product = list(acceptance.REAL_PROVIDER_CASES) if matrix == "product" else []
        expected_failure = list(acceptance.REAL_PROVIDER_FAILURE_CASES) if matrix == "failure" else []
        if real_provider.get("requestedCases") != expected_product:
            fail(
                "release.child_product_coverage_invalid",
                "Child report did not request the canonical product/capability matrix.",
            )
        if real_provider.get("requestedFailureCases", []) != expected_failure:
            fail(
                "release.child_failure_coverage_invalid",
                "Child report did not request the canonical real Provider failure matrix.",
            )
        if real_provider.get("ambientAuthentication") is not True:
            fail(
                "release.child_auth_boundary_invalid",
                "Child report did not preserve ambient authentication for the baseline real Provider path.",
            )
        if matrix == "failure" and real_provider.get("controlledFaultCredentials") is not True:
            fail(
                "release.child_fault_credential_boundary_invalid",
                "Failure child report did not use controlled fault Credentials.",
            )
        if matrix == "failure" and real_provider.get("cursorMaximumAge") != acceptance.REAL_PROVIDER_CURSOR_MAX_AGE:
            fail(
                "release.child_cursor_policy_invalid",
                "Failure child report did not use the canonical Cursor expiry policy.",
            )

    cases = report.get("cases")
    if not isinstance(cases, list) or not all(isinstance(case, dict) for case in cases):
        fail("release.child_cases_invalid", "Child report cases are missing or malformed.")
        return errors
    case_ids = [str(case.get("id")) for case in cases]
    duplicate_ids = sorted({case_id for case_id in case_ids if case_ids.count(case_id) > 1})
    if duplicate_ids:
        fail(
            "release.child_case_duplicate",
            "Child report contains duplicate case IDs.",
            {"caseIds": duplicate_ids},
        )
    by_id = {str(case.get("id")): case for case in cases}
    missing = sorted(expected_case_ids(matrix).difference(by_id))
    if missing:
        fail(
            "release.child_cases_missing",
            "Child report omitted required matrix cases.",
            {"missingCaseIds": missing},
        )

    allowed_unsupported = EXPECTED_UNSUPPORTED[(provider, matrix)]
    for case_id, case in by_id.items():
        status = case.get("status")
        if status not in acceptance.CASE_STATUSES:
            fail(
                "release.child_case_status_invalid",
                "Child report contains an unknown case status.",
                {"caseId": case_id, "status": status},
            )
            continue
        if status in {"fail", "skipped"}:
            fail(
                "release.child_case_not_complete",
                "Child report contains a failed or skipped case.",
                {"caseId": case_id, "status": status},
            )
        if status == "unsupported" and case_id not in allowed_unsupported:
            fail(
                "release.child_unsupported_unexpected",
                "Child report contains an unsupported case outside the frozen Local boundary.",
                {"caseId": case_id},
            )

    cleanup = by_id.get("environment.cleanup")
    cleanup_evidence = cleanup.get("evidence") if isinstance(cleanup, dict) else None
    if (
        not isinstance(cleanup, dict)
        or cleanup.get("status") != "pass"
        or not isinstance(cleanup_evidence, dict)
        or cleanup_evidence.get("controlPlaneStopped") is not True
        or cleanup_evidence.get("stateRemoved") is not True
    ):
        fail(
            "release.child_cleanup_invalid",
            "Child report did not prove exact Local Control Plane and state cleanup.",
        )

    secret_scan = by_id.get("security.output-secret-scan")
    secret_evidence = secret_scan.get("evidence") if isinstance(secret_scan, dict) else None
    if (
        not isinstance(secret_scan, dict)
        or secret_scan.get("status") != "pass"
        or not isinstance(secret_evidence, dict)
        or secret_evidence.get("findings") != []
    ):
        fail(
            "release.child_secret_scan_invalid",
            "Child report did not prove an empty output Secret scan.",
        )
    return errors


def file_sha256(path: pathlib.Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        while chunk := source.read(1 << 20):
            digest.update(chunk)
    return digest.hexdigest()


def case_counts(report: Mapping[str, Any]) -> dict[str, int]:
    result = {status: 0 for status in sorted(acceptance.CASE_STATUSES)}
    for case in report.get("cases", []):
        if isinstance(case, dict) and case.get("status") in result:
            result[str(case["status"])] += 1
    return result


def run_child(
    options: ReleaseGateOptions,
    *,
    provider: str,
    matrix: str,
    expected_git_sha: str,
) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    child_dir = options.output_dir / provider / matrix
    command = child_command(options, provider, matrix, child_dir)
    started = time.monotonic()
    completed = subprocess.run(command, cwd=options.repo_root, check=False)
    duration_ms = round((time.monotonic() - started) * 1000)
    json_path = child_dir / acceptance.JSON_REPORT_NAME
    markdown_path = child_dir / acceptance.MARKDOWN_REPORT_NAME
    errors: list[dict[str, Any]] = []
    record: dict[str, Any] = {
        "provider": provider,
        "matrix": matrix,
        "processReturnCode": completed.returncode,
        "durationMs": duration_ms,
        "reportPath": str(json_path.relative_to(options.output_dir)),
        "markdownPath": str(markdown_path.relative_to(options.output_dir)),
    }
    if not json_path.is_file() or not markdown_path.is_file():
        error = ReleaseGateError(
            "release.child_report_missing",
            "Child acceptance run did not produce both JSON and Markdown reports.",
            {"returnCode": completed.returncode},
        ).as_report_error(provider=provider, matrix=matrix)
        errors.append(error)
        record["status"] = "fail"
        return record, errors
    try:
        decoded = json.loads(json_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        error = ReleaseGateError(
            "release.child_report_invalid_json",
            "Child acceptance JSON report could not be decoded.",
        ).as_report_error(provider=provider, matrix=matrix)
        errors.append(error)
        record["status"] = "fail"
        return record, errors
    if not isinstance(decoded, dict):
        error = ReleaseGateError(
            "release.child_report_invalid_shape",
            "Child acceptance JSON report must be an object.",
        ).as_report_error(provider=provider, matrix=matrix)
        errors.append(error)
        record["status"] = "fail"
        return record, errors

    errors.extend(
        validate_child_report(
            decoded,
            provider=provider,
            matrix=matrix,
            expected_git_sha=expected_git_sha,
        )
    )
    if completed.returncode != 0:
        errors.append(
            ReleaseGateError(
                "release.child_process_failed",
                "Child acceptance process returned a non-zero status.",
                {"returnCode": completed.returncode},
            ).as_report_error(provider=provider, matrix=matrix)
        )
    cases = decoded.get("cases") if isinstance(decoded.get("cases"), list) else []
    record.update(
        {
            "status": "pass" if not errors else "fail",
            "childRunId": decoded.get("runId"),
            "childDurationMs": decoded.get("durationMs"),
            "caseCounts": case_counts(decoded),
            "unsupportedCaseIds": sorted(
                str(case.get("id"))
                for case in cases
                if isinstance(case, dict) and case.get("status") == "unsupported"
            ),
            "reportSha256": file_sha256(json_path),
            "markdownSha256": file_sha256(markdown_path),
            "source": decoded.get("source"),
        }
    )
    return record, errors


def catalog_consensus_errors(runs: Sequence[Mapping[str, Any]]) -> list[dict[str, Any]]:
    hashes = {
        source.get("providerCapabilityCatalogSha256")
        for run in runs
        if isinstance((source := run.get("source")), dict)
        and isinstance(source.get("providerCapabilityCatalogSha256"), str)
    }
    if len(hashes) == 1:
        return []
    return [
        {
            "code": "release.catalog_hash_mismatch",
            "message": "Child reports do not share one Provider Capability Catalog hash.",
            "evidence": {"distinctHashCount": len(hashes)},
        }
    ]


def markdown_from_report(report: Mapping[str, Any]) -> str:
    lines = [
        "# Stage 3 Real Provider Local Release Gate",
        "",
        f"- Schema: `{report['schemaVersion']}`",
        f"- Run: `{report['runId']}`",
        f"- Status: **{report['status']}**",
        f"- Git SHA: `{report.get('source', {}).get('gitSha', '')}`",
        f"- Node: `{report.get('runtime', {}).get('node', {}).get('version', '')}`",
        f"- Duration: `{report['durationMs']} ms`",
        "",
        "The product/capability and controlled-failure matrices remain separate child runs. This aggregate passes",
        "only when all four Codex/Claude Local reports share one clean SHA and satisfy coverage, cleanup, and",
        "Secret-scan requirements.",
        "",
        "## Child matrices",
        "",
        "| Provider | Matrix | Status | Cases | Unsupported | JSON SHA-256 |",
        "| --- | --- | --- | --- | --- | --- |",
    ]
    for run in report.get("runs", []):
        if not isinstance(run, dict):
            continue
        counts = run.get("caseCounts") if isinstance(run.get("caseCounts"), dict) else {}
        case_summary = ", ".join(
            f"{status}={counts.get(status, 0)}" for status in ("pass", "unsupported", "skipped", "fail")
        )
        unsupported = ", ".join(run.get("unsupportedCaseIds", [])) or "none"
        lines.append(
            f"| `{run.get('provider', '')}` | `{run.get('matrix', '')}` | {run.get('status', '')} | "
            f"{case_summary} | {unsupported} | `{run.get('reportSha256', '')}` |"
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
            "A pass closes the consolidated real Codex/Claude Local release slice for the implemented cases. It does",
            "not close SSH, Docker, Kubernetes, registry-pushed immutable image, concurrency, or soak gates.",
        ]
    )
    return "\n".join(lines) + "\n"


def write_report(report: Mapping[str, Any], output_dir: pathlib.Path) -> tuple[pathlib.Path, pathlib.Path]:
    json_path = output_dir / JSON_REPORT_NAME
    markdown_path = output_dir / MARKDOWN_REPORT_NAME
    json_path.write_text(
        json.dumps(report, indent=2, sort_keys=True, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )
    markdown_path.write_text(markdown_from_report(report), encoding="utf-8")
    return json_path, markdown_path


def failure_report(
    *,
    run_id: str,
    started_at: str,
    started: float,
    options: ReleaseGateOptions,
    error: ReleaseGateError,
) -> dict[str, Any]:
    return {
        "schemaVersion": SCHEMA_VERSION,
        "runId": run_id,
        "mode": "real-provider-local-release-gate",
        "target": "local",
        "status": "fail",
        "startedAt": started_at,
        "finishedAt": acceptance.utc_now(),
        "durationMs": acceptance.elapsed_ms(started),
        "configuration": {
            "runnerCommand": {
                "executable": pathlib.Path(options.runner_command[0]).name,
                "providerHost": str(pathlib.Path(options.runner_command[1]).relative_to(options.repo_root)),
            },
            "productTimeoutSeconds": options.product_timeout_seconds,
            "failureTimeoutSeconds": options.failure_timeout_seconds,
            "separateChildBoundaries": True,
        },
        "runs": [],
        "errors": [error.as_report_error()],
    }


def main(argv: Sequence[str] | None = None) -> int:
    options = parse_args(argv if argv is not None else sys.argv[1:])
    if options.output_dir.exists() and (
        not options.output_dir.is_dir() or any(options.output_dir.iterdir())
    ):
        print("Local release gate output directory must be empty or absent.", file=sys.stderr)
        return 2
    options.output_dir.mkdir(parents=True, exist_ok=True)
    started_at = acceptance.utc_now()
    started = time.monotonic()
    run_id = f"stage3-provider-local-release-{uuid.uuid4()}"
    try:
        source = repository_state(options.repo_root)
        runtime = {"node": inspect_node_runtime(options.runner_command[0])}
        build = build_provider_host(options)
    except (OSError, subprocess.SubprocessError, ReleaseGateError) as raw_error:
        error = (
            raw_error
            if isinstance(raw_error, ReleaseGateError)
            else ReleaseGateError("release.preflight_failed", "Local release gate preflight failed.")
        )
        report = failure_report(
            run_id=run_id,
            started_at=started_at,
            started=started,
            options=options,
            error=error,
        )
        json_path, markdown_path = write_report(report, options.output_dir)
        print("Stage 3 real Provider Local release gate: fail")
        print(f"JSON: {json_path}")
        print(f"Markdown: {markdown_path}")
        return 1

    runs: list[dict[str, Any]] = []
    errors: list[dict[str, Any]] = []
    for provider in PROVIDERS:
        for matrix in MATRICES:
            run, child_errors = run_child(
                options,
                provider=provider,
                matrix=matrix,
                expected_git_sha=str(source["gitSha"]),
            )
            runs.append(run)
            errors.extend(child_errors)
    errors.extend(catalog_consensus_errors(runs))
    status = "pass" if not errors and all(run.get("status") == "pass" for run in runs) else "fail"
    catalog_hashes = {
        source_value.get("providerCapabilityCatalogSha256")
        for run in runs
        if isinstance((source_value := run.get("source")), dict)
        and isinstance(source_value.get("providerCapabilityCatalogSha256"), str)
    }
    report = {
        "schemaVersion": SCHEMA_VERSION,
        "runId": run_id,
        "mode": "real-provider-local-release-gate",
        "target": "local",
        "status": status,
        "source": {
            **source,
            "providerCapabilityCatalogSha256": (
                next(iter(catalog_hashes)) if len(catalog_hashes) == 1 else None
            ),
        },
        "runtime": runtime,
        "build": build,
        "startedAt": started_at,
        "finishedAt": acceptance.utc_now(),
        "durationMs": acceptance.elapsed_ms(started),
        "configuration": {
            "runnerCommand": {
                "executable": pathlib.Path(options.runner_command[0]).name,
                "providerHost": str(pathlib.Path(options.runner_command[1]).relative_to(options.repo_root)),
            },
            "productTimeoutSeconds": options.product_timeout_seconds,
            "failureTimeoutSeconds": options.failure_timeout_seconds,
            "separateChildBoundaries": True,
        },
        "coverage": {
            "requiredRuns": len(PROVIDERS) * len(MATRICES),
            "completedRuns": len(runs),
            "providers": list(PROVIDERS),
            "productCases": list(acceptance.REAL_PROVIDER_CASES),
            "failureCases": list(acceptance.REAL_PROVIDER_FAILURE_CASES),
        },
        "security": {
            "rawChildOutputPersisted": False,
            "childSecretScansRequired": True,
            "childCleanupRequired": True,
        },
        "runs": runs,
        "errors": errors,
    }
    json_path, markdown_path = write_report(report, options.output_dir)
    print(f"Stage 3 real Provider Local release gate: {status}")
    print(f"JSON: {json_path}")
    print(f"Markdown: {markdown_path}")
    return 0 if status == "pass" else 1


if __name__ == "__main__":
    raise SystemExit(main())
