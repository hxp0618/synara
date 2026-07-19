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
import json
import os
import pathlib
import re
import subprocess
import sys
import time
import uuid
from collections.abc import Mapping, Sequence
from typing import Any

import acceptance_runner as acceptance
import controlled_remote_release_gate as remote
import release_gate_common as common


SCHEMA_VERSION = "synara.provider-local-release-gate.v1"
JSON_REPORT_NAME = "local-release-gate.json"
MARKDOWN_REPORT_NAME = "local-release-gate.md"
PROVIDERS = common.PROVIDERS
MATRICES = common.MATRICES
NODE_MINIMUM = (24, 13, 1)
NODE_MAXIMUM_EXCLUSIVE = (25, 0, 0)
EXPECTED_UNSUPPORTED: Mapping[tuple[str, str], frozenset[str]] = {
    ("codex", "product"): frozenset({"real-provider.terminal-large-log"}),
    ("claudeAgent", "product"): frozenset({"real-provider.compact-boundary"}),
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
    codex_credential: remote.CredentialSource
    claude_credential: remote.CredentialSource
    codex_model: str | None = None
    claude_model: str | None = None
    codex_model_environment_name: str | None = None
    claude_model_environment_name: str | None = None


ReleaseGateError = common.ReleaseGateError


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
    parser.add_argument("--codex-credential-env", required=True)
    parser.add_argument("--codex-base-url-env")
    remote.add_provider_model_arguments(parser, "codex")
    parser.add_argument("--claude-credential-env", required=True)
    parser.add_argument(
        "--claude-credential-field",
        choices=acceptance.REAL_PROVIDER_CREDENTIAL_FIELDS,
        default="apiKey",
    )
    parser.add_argument("--claude-base-url-env")
    remote.add_provider_model_arguments(parser, "claude")
    parsed = parser.parse_args(argv)
    if parsed.product_timeout <= 0 or parsed.failure_timeout <= 0:
        parser.error("matrix timeouts must be positive")
    try:
        runner_command = parse_runner_command(parsed.runner_command_json, repo_root)
        codex_credential = remote.parse_credential_source(
            parsed.codex_credential_env,
            "apiKey",
            parsed.codex_base_url_env,
            "Codex",
        )
        claude_credential = remote.parse_credential_source(
            parsed.claude_credential_env,
            parsed.claude_credential_field,
            parsed.claude_base_url_env,
            "Claude",
        )
        codex_model_environment_name = acceptance.parse_environment_variable_name(
            parsed.codex_model_env,
            "--codex-model-env",
        )
        claude_model_environment_name = acceptance.parse_environment_variable_name(
            parsed.claude_model_env,
            "--claude-model-env",
        )
        codex_model = remote.parse_provider_model_argument(
            parsed.codex_model,
            codex_model_environment_name,
            provider_label="Codex",
            model_option="--codex-model",
            model_env_option="--codex-model-env",
        )
        claude_model = remote.parse_provider_model_argument(
            parsed.claude_model,
            claude_model_environment_name,
            provider_label="Claude",
            model_option="--claude-model",
            model_env_option="--claude-model-env",
        )
    except (ReleaseGateError, ValueError) as error:
        parser.error(error.message if isinstance(error, ReleaseGateError) else str(error))
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
        codex_credential=codex_credential,
        claude_credential=claude_credential,
        codex_model=codex_model,
        claude_model=claude_model,
        codex_model_environment_name=codex_model_environment_name,
        claude_model_environment_name=claude_model_environment_name,
    )


def credential_source(options: ReleaseGateOptions, provider: str) -> remote.CredentialSource:
    return remote.credential_source(options, provider)


def provider_model(options: ReleaseGateOptions, provider: str) -> str | None:
    return remote.provider_model(options, provider)


def provider_model_environment_name(
    options: ReleaseGateOptions,
    provider: str,
) -> str | None:
    if provider == "codex":
        return options.codex_model_environment_name
    if provider == "claudeAgent":
        return options.claude_model_environment_name
    raise ValueError(f"unsupported Local release Provider: {provider}")


def forbidden_environment_names(options: ReleaseGateOptions) -> tuple[str, ...]:
    return tuple(
        sorted(
            {
                value
                for provider in PROVIDERS
                for value in (
                    credential_source(options, provider).environment_name,
                    credential_source(options, provider).base_url_environment_name,
                    provider_model_environment_name(options, provider),
                )
                if value is not None
            }
        )
    )


def local_process_environment(environment: Mapping[str, str]) -> dict[str, str]:
    allowed = (
        "PATH",
        "HOME",
        "USER",
        "LOGNAME",
        "USERNAME",
        "USERPROFILE",
        "HOMEDRIVE",
        "HOMEPATH",
        "TMPDIR",
        "TMP",
        "TEMP",
        "SYSTEMROOT",
        "WINDIR",
        "COMSPEC",
        "PATHEXT",
        "LANG",
        "LANGUAGE",
        "LC_ALL",
        "LC_CTYPE",
        "TZ",
        "TERM",
        "COLORTERM",
        "SHELL",
        "NO_COLOR",
        "FORCE_COLOR",
        "CLICOLOR",
        "CLICOLOR_FORCE",
        "SSL_CERT_FILE",
        "SSL_CERT_DIR",
        "NODE_EXTRA_CA_CERTS",
        "GOCACHE",
        "GOMODCACHE",
        "GOPATH",
        "GOROOT",
        "DOCKER_HOST",
        "DOCKER_CONTEXT",
        "DOCKER_CONFIG",
        "XDG_RUNTIME_DIR",
    )
    selected = {key: environment[key] for key in allowed if key in environment}
    selected.setdefault("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")
    return selected


def local_child_environment(
    options: ReleaseGateOptions,
    provider: str,
    environment: Mapping[str, str],
) -> dict[str, str]:
    return remote.controlled_child_environment(
        options,
        provider,
        local_process_environment(environment),
    )


def child_policy(options: ReleaseGateOptions) -> common.ChildReportPolicy:
    return common.ChildReportPolicy(
        target="local",
        runner_executable="node",
        expected_unsupported=EXPECTED_UNSUPPORTED,
        authentication="controlled",
        credential_fields={
            provider: credential_source(options, provider).field for provider in PROVIDERS
        },
        controlled_base_urls={
            provider: credential_source(options, provider).base_url_environment_name is not None
            for provider in PROVIDERS
        },
        provider_models={provider: provider_model(options, provider) for provider in PROVIDERS},
        cleanup_true_fields=("controlPlaneStopped", "stateRemoved"),
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
    return common.repository_state(repo_root)


def build_provider_host(
    options: ReleaseGateOptions,
    redactor: acceptance.SecretRedactor,
) -> dict[str, Any]:
    started = time.monotonic()
    completed = subprocess.run(
        ["bun", "run", "--cwd", "apps/provider-host", "build"],
        cwd=options.repo_root,
        env=local_process_environment(os.environ),
        check=False,
        capture_output=True,
        text=True,
        encoding="utf-8",
        errors="replace",
    )
    process_output_scan = common.scan_process_output(
        completed.stdout if isinstance(completed.stdout, str) else "",
        completed.stderr if isinstance(completed.stderr, str) else "",
        redactor=redactor,
        forbidden_tokens=forbidden_environment_names(options),
    )
    if process_output_scan["findings"]:
        raise ReleaseGateError(
            "release.provider_host_build_output_secret_scan_failed",
            "Provider Host build stdout or stderr contained controlled Provider material.",
            {"findingCount": len(process_output_scan["findings"])},
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
        "processOutputScan": process_output_scan,
    }


def child_command(
    options: ReleaseGateOptions,
    provider: str,
    matrix: str,
    output_dir: pathlib.Path,
) -> list[str]:
    source = credential_source(options, provider)
    timeout = (
        options.product_timeout_seconds
        if matrix == "product"
        else options.failure_timeout_seconds
    )
    matrix_flag = "--real-provider-matrix" if matrix == "product" else "--real-provider-failure-matrix"
    command = [
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
        "--real-provider-credential-env",
        source.environment_name,
        "--real-provider-credential-field",
        source.field,
        "--output-dir",
        str(output_dir),
        "--timeout",
        str(timeout),
    ]
    if source.base_url_environment_name is not None:
        command.extend(["--real-provider-base-url-env", source.base_url_environment_name])
    model = provider_model(options, provider)
    if model is not None:
        command.extend(["--real-provider-model", model])
    return command


def expected_case_ids(matrix: str) -> frozenset[str]:
    return common.expected_case_ids(matrix)


def validate_child_report(
    report: Mapping[str, Any],
    *,
    options: ReleaseGateOptions,
    provider: str,
    matrix: str,
    expected_git_sha: str,
) -> list[dict[str, Any]]:
    return common.validate_child_report(
        report,
        provider=provider,
        matrix=matrix,
        expected_git_sha=expected_git_sha,
        policy=child_policy(options),
    )


def file_sha256(path: pathlib.Path) -> str:
    return common.file_sha256(path)


def case_counts(report: Mapping[str, Any]) -> dict[str, int]:
    return common.case_counts(report)


def run_child(
    options: ReleaseGateOptions,
    *,
    provider: str,
    matrix: str,
    expected_git_sha: str,
    redactor: acceptance.SecretRedactor,
) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    child_dir = options.output_dir / provider / matrix
    return common.run_child_report(
        repo_root=options.repo_root,
        output_dir=options.output_dir,
        provider=provider,
        matrix=matrix,
        expected_git_sha=expected_git_sha,
        command=child_command(options, provider, matrix, child_dir),
        policy=child_policy(options),
        environment=local_child_environment(options, provider, os.environ),
        capture_process_output=True,
        process_output_redactor=redactor,
        forbidden_output_tokens=forbidden_environment_names(options),
    )


def catalog_consensus_errors(runs: Sequence[Mapping[str, Any]]) -> list[dict[str, Any]]:
    return common.catalog_consensus_errors(runs)


def operator_environment_name_findings(
    options: ReleaseGateOptions,
) -> list[dict[str, Any]]:
    return common.output_file_token_findings(
        options.output_dir,
        forbidden_environment_names(options),
    )


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
            "authentication": "controlled",
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
        redactor = remote.credential_redactor(options)
        source = repository_state(options.repo_root)
        runtime = {"node": inspect_node_runtime(options.runner_command[0])}
        build = build_provider_host(options, redactor)
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
                redactor=redactor,
            )
            runs.append(run)
            errors.extend(child_errors)
    errors.extend(catalog_consensus_errors(runs))
    output_secret_scan = remote.scan_child_outputs(options, redactor, MATRICES)
    if output_secret_scan["findings"]:
        errors.append(
            {
                "code": "release.aggregate_secret_scan_failed",
                "message": "Aggregate child output scan found controlled Credential material.",
                "evidence": {"findingCount": len(output_secret_scan["findings"])},
            }
        )
    environment_name_findings = operator_environment_name_findings(options)
    if environment_name_findings:
        errors.append(
            {
                "code": "release.credential_environment_name_persisted",
                "message": (
                    "Child output persisted an operator Credential, model, or Base URL "
                    "environment-variable name."
                ),
                "evidence": {"files": environment_name_findings},
            }
        )
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
            "authentication": "controlled",
            "providers": {
                provider: {
                    "credentialField": credential_source(options, provider).field,
                    "controlledBaseUrl": (
                        credential_source(options, provider).base_url_environment_name is not None
                    ),
                    "model": provider_model(options, provider),
                }
                for provider in PROVIDERS
            },
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
            "credentialEnvironmentNamesPersisted": bool(environment_name_findings),
            "aggregateChildOutputScan": output_secret_scan,
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
