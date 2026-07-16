from __future__ import annotations

import dataclasses
import hashlib
import json
import pathlib
import re
import subprocess
import time
from collections.abc import Mapping, Sequence
from typing import Any

import acceptance_runner as acceptance


PROVIDERS = ("codex", "claudeAgent")
MATRICES = ("product", "failure")


@dataclasses.dataclass(frozen=True)
class ChildReportPolicy:
    target: str
    runner_executable: str
    expected_unsupported: Mapping[tuple[str, str], frozenset[str]]
    authentication: str
    credential_fields: Mapping[str, str | None]
    controlled_base_urls: Mapping[str, bool]
    cleanup_true_fields: tuple[str, ...]
    cleanup_false_fields: tuple[str, ...] = ()
    expected_worker_image_build: str | None = None
    expected_worker_image_name: str | None = None
    expected_skip_worker_build: bool | None = None
    worker_image_evidence_path: tuple[str, ...] = ("docker",)
    worker_image_configuration_key: str | None = None


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
            "The consolidated release gate requires a clean worktree with no untracked files.",
        )
    return {"gitSha": sha, "worktreeDirty": False}


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
    policy: ChildReportPolicy,
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
    if report.get("provider") != provider or report.get("target") != policy.target:
        fail(
            "release.child_identity_mismatch",
            "Child report Provider or Target does not match the requested release run.",
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
        if not isinstance(runner, dict) or runner.get("executable") != policy.runner_executable:
            fail(
                "release.child_runner_invalid",
                "Child report did not use the required Provider Host executable.",
                {"expectedExecutable": policy.runner_executable},
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
        if policy.authentication == "ambient":
            if real_provider.get("ambientAuthentication") is not True:
                fail(
                    "release.child_auth_boundary_invalid",
                    "Child report did not preserve ambient authentication for the baseline path.",
                )
        elif policy.authentication == "controlled":
            expected_field = policy.credential_fields.get(provider)
            expected_base_url = policy.controlled_base_urls.get(provider, False)
            if (
                real_provider.get("ambientAuthentication") is not False
                or real_provider.get("controlledProductCredential") is not True
                or real_provider.get("controlledProductCredentialField") != expected_field
                or real_provider.get("productCredentialEnvironmentNamePersisted") is not False
                or real_provider.get("controlledBaseUrl") is not expected_base_url
            ):
                fail(
                    "release.child_auth_boundary_invalid",
                    "Child report did not preserve the controlled product Credential boundary.",
                    {
                        "expectedCredentialField": expected_field,
                        "expectedControlledBaseUrl": expected_base_url,
                    },
                )
        else:
            raise ValueError(f"unknown release authentication policy: {policy.authentication}")
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

    allowed_unsupported = policy.expected_unsupported[(provider, matrix)]
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
                "Child report contains an unsupported case outside the frozen target boundary.",
                {"caseId": case_id},
            )

    cleanup = by_id.get("environment.cleanup")
    cleanup_evidence = cleanup.get("evidence") if isinstance(cleanup, dict) else None
    cleanup_valid = isinstance(cleanup, dict) and cleanup.get("status") == "pass" and isinstance(cleanup_evidence, dict)
    if cleanup_valid:
        cleanup_valid = all(cleanup_evidence.get(field) is True for field in policy.cleanup_true_fields) and all(
            cleanup_evidence.get(field) is False for field in policy.cleanup_false_fields
        )
    if not cleanup_valid:
        fail(
            "release.child_cleanup_invalid",
            "Child report did not prove the required target cleanup boundary.",
            {
                "requiredTrueFields": list(policy.cleanup_true_fields),
                "requiredFalseFields": list(policy.cleanup_false_fields),
            },
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

    if policy.expected_worker_image_build is not None:
        worker_image_evidence = child_worker_image_evidence(
            report,
            evidence_path=policy.worker_image_evidence_path,
        )
        worker_image_id = (
            worker_image_evidence.get("workerImageId")
            if isinstance(worker_image_evidence, dict)
            else None
        )
        configuration_key = policy.worker_image_configuration_key or policy.target
        worker_image_configuration = (
            configuration.get(configuration_key) if isinstance(configuration, dict) else None
        )
        if (
            not isinstance(worker_image_id, str)
            or re.fullmatch(r"sha256:[0-9a-f]{64}", worker_image_id) is None
            or not isinstance(worker_image_evidence, dict)
            or worker_image_evidence.get("build") != policy.expected_worker_image_build
            or worker_image_evidence.get("workerImage") != policy.expected_worker_image_name
            or not isinstance(worker_image_configuration, dict)
            or worker_image_configuration.get("workerImage") != policy.expected_worker_image_name
            or worker_image_configuration.get("skipWorkerBuild") is not policy.expected_skip_worker_build
        ):
            fail(
                "release.child_worker_image_invalid",
                "Remote child report did not use the expected shared Worker acceptance image.",
                {
                    "expectedBuild": policy.expected_worker_image_build,
                    "expectedSkipWorkerBuild": policy.expected_skip_worker_build,
                },
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


def child_worker_image_evidence(
    report: Mapping[str, Any],
    *,
    evidence_path: Sequence[str] = ("docker",),
) -> Mapping[str, Any] | None:
    cases = report.get("cases")
    if not isinstance(cases, list):
        return None
    target_prepare = next(
        (
            case
            for case in cases
            if isinstance(case, dict) and case.get("id") == "environment.target-prepare"
        ),
        None,
    )
    evidence = target_prepare.get("evidence") if isinstance(target_prepare, dict) else None
    current: Any = evidence
    for key in evidence_path:
        current = current.get(key) if isinstance(current, dict) else None
    return current if isinstance(current, dict) else None


def child_worker_image_id(
    report: Mapping[str, Any],
    *,
    evidence_path: Sequence[str] = ("docker",),
) -> str | None:
    worker_image = child_worker_image_evidence(report, evidence_path=evidence_path)
    value = worker_image.get("workerImageId") if isinstance(worker_image, dict) else None
    return value if isinstance(value, str) else None


def run_child_report(
    *,
    repo_root: pathlib.Path,
    output_dir: pathlib.Path,
    provider: str,
    matrix: str,
    expected_git_sha: str,
    command: Sequence[str],
    policy: ChildReportPolicy,
    environment: Mapping[str, str] | None = None,
) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    child_dir = output_dir / provider / matrix
    started = time.monotonic()
    completed = subprocess.run(command, cwd=repo_root, env=environment, check=False)
    duration_ms = round((time.monotonic() - started) * 1000)
    json_path = child_dir / acceptance.JSON_REPORT_NAME
    markdown_path = child_dir / acceptance.MARKDOWN_REPORT_NAME
    errors: list[dict[str, Any]] = []
    record: dict[str, Any] = {
        "provider": provider,
        "matrix": matrix,
        "processReturnCode": completed.returncode,
        "durationMs": duration_ms,
        "reportPath": str(json_path.relative_to(output_dir)),
        "markdownPath": str(markdown_path.relative_to(output_dir)),
    }
    if not json_path.is_file() or not markdown_path.is_file():
        errors.append(
            ReleaseGateError(
                "release.child_report_missing",
                "Child acceptance run did not produce both JSON and Markdown reports.",
                {"returnCode": completed.returncode},
            ).as_report_error(provider=provider, matrix=matrix)
        )
        record["status"] = "fail"
        return record, errors
    try:
        decoded = json.loads(json_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        errors.append(
            ReleaseGateError(
                "release.child_report_invalid_json",
                "Child acceptance JSON report could not be decoded.",
            ).as_report_error(provider=provider, matrix=matrix)
        )
        record["status"] = "fail"
        return record, errors
    if not isinstance(decoded, dict):
        errors.append(
            ReleaseGateError(
                "release.child_report_invalid_shape",
                "Child acceptance JSON report must be an object.",
            ).as_report_error(provider=provider, matrix=matrix)
        )
        record["status"] = "fail"
        return record, errors

    errors.extend(
        validate_child_report(
            decoded,
            provider=provider,
            matrix=matrix,
            expected_git_sha=expected_git_sha,
            policy=policy,
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
    worker_image_id = child_worker_image_id(
        decoded,
        evidence_path=policy.worker_image_evidence_path,
    )
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
            **(
                {"workerImageId": worker_image_id}
                if worker_image_id is not None
                else {}
            ),
        }
    )
    return record, errors


def consensus_errors(
    runs: Sequence[Mapping[str, Any]],
    *,
    field: str,
    code: str,
    message: str,
) -> list[dict[str, Any]]:
    values = {run.get(field) for run in runs if isinstance(run.get(field), str)}
    if len(values) == 1 and len(runs) > 0 and all(isinstance(run.get(field), str) for run in runs):
        return []
    return [
        {
            "code": code,
            "message": message,
            "evidence": {"distinctValueCount": len(values), "runCount": len(runs)},
        }
    ]


def catalog_consensus_errors(runs: Sequence[Mapping[str, Any]]) -> list[dict[str, Any]]:
    hashes = {
        source.get("providerCapabilityCatalogSha256")
        for run in runs
        if isinstance((source := run.get("source")), dict)
        and isinstance(source.get("providerCapabilityCatalogSha256"), str)
    }
    if len(hashes) == 1 and len(runs) > 0:
        return []
    return [
        {
            "code": "release.catalog_hash_mismatch",
            "message": "Child reports do not share one Provider Capability Catalog hash.",
            "evidence": {"distinctHashCount": len(hashes), "runCount": len(runs)},
        }
    ]
