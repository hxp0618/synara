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
REMOTE_LOAD_MATRIX = "load"
PRODUCTION_WORKER_LEASE_TTL = "30s"
PRODUCTION_WORKER_HEARTBEAT_TIMEOUT = "90s"
COMMON_BASELINE_CASE_IDS = frozenset(
    {
        "environment.target-prepare",
        "environment.control-plane-start",
        "identity.dev-login",
    }
)
REAL_PROVIDER_SMOKE_BASELINE_CASE_IDS = frozenset(
    {
        "runtime.target-provision",
        "resources.real-provider-project-session",
        "real-provider.turn-1-start",
        "runtime.real-provider-worker-discovery",
        "real-provider.turn-1",
        "recovery.control-plane-restart",
        "real-provider.turn-2-continuity",
    }
)
REAL_PROVIDER_LOAD_BASELINE_CASE_IDS = frozenset(
    {
        "runtime.target-provision",
        "resources.real-provider-project-session",
    }
)


@dataclasses.dataclass(frozen=True)
class ChildReportPolicy:
    target: str
    runner_executable: str
    expected_unsupported: Mapping[tuple[str, str], frozenset[str]]
    authentication: str
    credential_fields: Mapping[str, str | None]
    controlled_base_urls: Mapping[str, bool]
    provider_models: Mapping[str, str | None]
    cleanup_true_fields: tuple[str, ...]
    cleanup_false_fields: tuple[str, ...] = ()
    expected_worker_image_build: str | None = None
    expected_worker_image_name: str | None = None
    expected_skip_worker_build: bool | None = None
    worker_image_evidence_path: tuple[str, ...] = ("docker",)
    worker_image_configuration_key: str | None = None
    expected_worker_lease_ttl: str | None = None
    expected_worker_heartbeat_timeout: str | None = None


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


def normalize_go_proxy(value: str | None) -> str | None:
    if value is None:
        return None
    proxy = value.strip()
    if (
        not proxy
        or len(proxy) > 2048
        or any(character.isspace() or ord(character) < 32 for character in proxy)
        or any(character in proxy for character in "@?#")
    ):
        raise ValueError(
            "--go-proxy must be a public credential-free GOPROXY list without whitespace, "
            "userinfo, query, or fragment data"
        )
    pattern = re.compile(
        r"https://[A-Za-z0-9.-]+(?::[0-9]+)?(?:/[A-Za-z0-9._~!$&'()*+,;=:@%/-]*)?"
    )
    if any(
        entry not in {"direct", "off"} and pattern.fullmatch(entry) is None
        for entry in proxy.split(",")
    ):
        raise ValueError("--go-proxy entries must use https://, direct, or off")
    return proxy


def normalize_apk_repositories(value: str | None) -> str | None:
    if value is None:
        return None
    repositories = value.strip()
    if (
        not repositories
        or len(repositories) > 4096
        or any(character.isspace() or ord(character) < 32 for character in repositories)
        or any(character in repositories for character in "@?#")
    ):
        raise ValueError(
            "--apk-repositories must be a public credential-free comma-separated HTTPS repository list "
            "without whitespace, userinfo, query, or fragment data"
        )
    pattern = re.compile(
        r"https://[A-Za-z0-9.-]+(?::[0-9]+)?(?:/[A-Za-z0-9._~!$&'()*+,;=:@%/-]*)?"
    )
    if any(pattern.fullmatch(entry) is None for entry in repositories.split(",")):
        raise ValueError("--apk-repositories entries must use https://")
    return repositories


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
    if matrix == REMOTE_LOAD_MATRIX:
        return frozenset({acceptance.REAL_PROVIDER_LOAD_CASE_ID})
    return frozenset(
        metadata["id"] for metadata in acceptance.REAL_PROVIDER_FAILURE_CASE_METADATA.values()
    )


def required_case_ids(matrix: str) -> frozenset[str]:
    baseline = (
        REAL_PROVIDER_LOAD_BASELINE_CASE_IDS
        if matrix == REMOTE_LOAD_MATRIX
        else REAL_PROVIDER_SMOKE_BASELINE_CASE_IDS
    )
    return COMMON_BASELINE_CASE_IDS | baseline | expected_case_ids(matrix)


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
    expected_mode = "real-provider-load" if matrix == REMOTE_LOAD_MATRIX else "real-provider-smoke"
    if report.get("mode") != expected_mode or report.get("status") != "pass":
        fail(
            "release.child_status_invalid",
            "Child report is not a passing real Provider acceptance report for the requested matrix.",
            {
                "status": report.get("status"),
                "mode": report.get("mode"),
                "expectedMode": expected_mode,
            },
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
        expected_restart_control_plane = matrix != REMOTE_LOAD_MATRIX
        if (
            configuration.get("restartControlPlane") is not expected_restart_control_plane
            or configuration.get("keepState") is not False
        ):
            fail(
                "release.child_lifecycle_invalid",
                "Child report did not preserve the expected Control Plane restart and cleanup boundary.",
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
        expected_load = [acceptance.REAL_PROVIDER_LOAD_CASE_ID] if matrix == REMOTE_LOAD_MATRIX else []
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
        if real_provider.get("requestedLoadCases", []) != expected_load:
            fail(
                "release.child_load_coverage_invalid",
                "Child report did not request the canonical real Provider load suite.",
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
            expected_model = policy.provider_models.get(provider)
            if (
                real_provider.get("ambientAuthentication") is not False
                or real_provider.get("controlledProductCredential") is not True
                or real_provider.get("controlledProductCredentialField") != expected_field
                or real_provider.get("productCredentialEnvironmentNamePersisted") is not False
                or real_provider.get("controlledBaseUrl") is not expected_base_url
                or real_provider.get("model") != expected_model
            ):
                fail(
                    "release.child_auth_boundary_invalid",
                    "Child report did not preserve the controlled product Credential boundary.",
                    {
                        "expectedCredentialField": expected_field,
                        "expectedControlledBaseUrl": expected_base_url,
                        "expectedModel": expected_model,
                    },
                )
        else:
            raise ValueError(f"unknown release authentication policy: {policy.authentication}")
        if matrix == "failure" and real_provider.get("controlledFaultCredentials") is not True:
            fail(
                "release.child_fault_credential_boundary_invalid",
                "Failure child report did not use controlled fault Credentials.",
            )
        if matrix == REMOTE_LOAD_MATRIX and real_provider.get("controlledFaultCredentials") is not False:
            fail(
                "release.child_fault_credential_boundary_invalid",
                "Load child report must not claim controlled fault Credentials.",
            )
        if matrix == "failure" and real_provider.get("cursorMaximumAge") != acceptance.REAL_PROVIDER_CURSOR_MAX_AGE:
            fail(
                "release.child_cursor_policy_invalid",
                "Failure child report did not use the canonical Cursor expiry policy.",
            )
        if matrix == REMOTE_LOAD_MATRIX and real_provider.get("cursorMaximumAge") is not None:
            fail(
                "release.child_cursor_policy_invalid",
                "Load child report must not claim the failure-only Cursor expiry policy.",
            )
        if (
            policy.expected_worker_lease_ttl is not None
            or policy.expected_worker_heartbeat_timeout is not None
        ):
            worker_timing = configuration.get("workerTiming")
            if (
                not isinstance(worker_timing, Mapping)
                or worker_timing.get("leaseTTL") != policy.expected_worker_lease_ttl
                or worker_timing.get("heartbeatTimeout")
                != policy.expected_worker_heartbeat_timeout
            ):
                fail(
                    "release.child_worker_timing_invalid",
                    "Child report did not preserve the release gate worker timing policy.",
                    {
                        "expectedLeaseTTL": policy.expected_worker_lease_ttl,
                        "expectedHeartbeatTimeout": policy.expected_worker_heartbeat_timeout,
                    },
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
    missing = sorted(required_case_ids(matrix).difference(by_id))
    if missing:
        fail(
            "release.child_cases_missing",
            "Child report omitted required baseline or matrix cases.",
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

    def require_distribution(
        value: Any,
        *,
        label: str,
        expected_sample_count: int | None = None,
    ) -> None:
        if not isinstance(value, Mapping):
            fail(
                "release.child_load_evidence_invalid",
                "Load child report omitted a required duration distribution mapping.",
                {"field": label},
            )
            return
        sample_count = value.get("sampleCount")
        if not isinstance(sample_count, int) or sample_count <= 0:
            fail(
                "release.child_load_evidence_invalid",
                "Load child report omitted a positive sampleCount in a duration distribution.",
                {"field": label, "sampleCount": sample_count},
            )
        elif expected_sample_count is not None and sample_count != expected_sample_count:
            fail(
                "release.child_load_evidence_invalid",
                "Load child report used an unexpected sampleCount in a duration distribution.",
                {
                    "field": label,
                    "expectedSampleCount": expected_sample_count,
                    "actualSampleCount": sample_count,
                },
            )
        for percentile in ("p95", "p99"):
            observed = value.get(percentile)
            if isinstance(observed, bool) or not isinstance(observed, (int, float)):
                fail(
                    "release.child_load_evidence_invalid",
                    "Load child report omitted a numeric percentile in a duration distribution.",
                    {"field": f"{label}.{percentile}", "value": observed},
                )

    def require_operator_approved_sla(summary: Any) -> None:
        if not isinstance(summary, Mapping):
            fail(
                "release.child_load_sla_invalid",
                "Load child report omitted its operator-approved SLA evaluation summary.",
            )
            return
        if summary.get("enforced") is not True or summary.get("notEvaluated") is True:
            fail(
                "release.child_load_sla_invalid",
                "Load child report did not complete an enforced operator-approved SLA evaluation.",
                {
                    "enforced": summary.get("enforced"),
                    "notEvaluated": summary.get("notEvaluated"),
                    "notEvaluatedReason": summary.get("notEvaluatedReason"),
                },
            )
        checks = summary.get("checks")
        if not isinstance(checks, list) or not checks or any(
            not isinstance(check, Mapping) or check.get("status") != "pass" for check in checks
        ):
            fail(
                "release.child_load_sla_invalid",
                "Load child report operator-approved SLA checks were missing or not fully passing.",
            )
        metric_mapping = summary.get("metricMapping")
        if not isinstance(metric_mapping, Mapping):
            fail(
                "release.child_load_sla_invalid",
                "Load child report operator-approved SLA metric mapping was missing.",
            )
        else:
            expected_paths = {
                "minimumDurationSeconds": "durationMs",
                "controlPlaneAdmissionLatencyMs.p95Max": "controlPlaneAdmissionLatencyMs.p95",
                "controlPlaneAdmissionLatencyMs.p99Max": "controlPlaneAdmissionLatencyMs.p99",
                "slotReuseAdmissionLatencyMs.p95Max": "slotReuseAdmissionLatencyMs.p95",
                "slotReuseAdmissionLatencyMs.p99Max": "slotReuseAdmissionLatencyMs.p99",
                "unexpectedErrorRateMax": "unexpectedErrorRate",
            }
            for check_id, expected_path in expected_paths.items():
                entry = metric_mapping.get(check_id)
                if not isinstance(entry, Mapping) or entry.get("observedEvidencePath") != expected_path:
                    fail(
                        "release.child_load_sla_invalid",
                        "Load child report operator-approved SLA metric mapping did not match the canonical evidence paths.",
                        {
                            "checkId": check_id,
                            "expectedObservedEvidencePath": expected_path,
                            "actualObservedEvidencePath": (
                                entry.get("observedEvidencePath") if isinstance(entry, Mapping) else None
                            ),
                        },
                    )

    if matrix == REMOTE_LOAD_MATRIX:
        load_case = by_id.get(acceptance.REAL_PROVIDER_LOAD_CASE_ID)
        load_evidence = load_case.get("evidence") if isinstance(load_case, dict) else None
        if not isinstance(load_case, dict) or load_case.get("status") != "pass" or not isinstance(load_evidence, Mapping):
            fail(
                "release.child_load_evidence_invalid",
                "Load child report omitted the canonical load evidence payload.",
            )
        else:
            waves_completed = load_evidence.get("wavesCompleted")
            sessions = load_evidence.get("sessions")
            workers = load_evidence.get("workers")
            executions_completed = load_evidence.get("executionsCompleted")
            restart_every_waves = load_evidence.get("restartEveryWaves")
            restart_count = load_evidence.get("controlPlaneRestartCount")
            restarts = load_evidence.get("controlPlaneRestarts")
            if workers != acceptance.REAL_PROVIDER_LOAD_CONCURRENCY or sessions != acceptance.REAL_PROVIDER_LOAD_SESSIONS:
                fail(
                    "release.child_load_evidence_invalid",
                    "Load child report did not retain the canonical worker/session topology.",
                    {"workers": workers, "sessions": sessions},
                )
            if not isinstance(waves_completed, int) or waves_completed < 1:
                fail(
                    "release.child_load_evidence_invalid",
                    "Load child report omitted a positive completed-wave count.",
                    {"wavesCompleted": waves_completed},
                )
            if not isinstance(executions_completed, int):
                fail(
                    "release.child_load_evidence_invalid",
                    "Load child report omitted a numeric completed execution count.",
                    {"executionsCompleted": executions_completed},
                )
            expected_rejections = (
                (acceptance.REAL_PROVIDER_LOAD_SESSIONS - acceptance.REAL_PROVIDER_LOAD_CONCURRENCY)
                * waves_completed
                if isinstance(waves_completed, int)
                else None
            )
            if (
                isinstance(waves_completed, int)
                and isinstance(executions_completed, int)
                and executions_completed != acceptance.REAL_PROVIDER_LOAD_SESSIONS * waves_completed
            ):
                fail(
                    "release.child_load_evidence_invalid",
                    "Load child report did not retain the canonical execution count for the completed waves.",
                    {
                        "wavesCompleted": waves_completed,
                        "executionsCompleted": executions_completed,
                    },
                )
            if load_evidence.get("quotaRejections") != expected_rejections:
                fail(
                    "release.child_load_evidence_invalid",
                    "Load child report did not retain the canonical quota-rejection count.",
                    {
                        "quotaRejections": load_evidence.get("quotaRejections"),
                        "expectedQuotaRejections": expected_rejections,
                    },
                )
            if load_evidence.get("effectiveConcurrency") != acceptance.REAL_PROVIDER_LOAD_CONCURRENCY:
                fail(
                    "release.child_load_evidence_invalid",
                    "Load child report did not retain the canonical effective concurrency.",
                    {"effectiveConcurrency": load_evidence.get("effectiveConcurrency")},
                )
            if (
                load_evidence.get("unexpectedFailureCount") != 0
                or load_evidence.get("unexpectedErrorRate") != 0.0
                or load_evidence.get("doubleExecution") is not False
                or load_evidence.get("duplicateTerminal") is not False
                or load_evidence.get("pendingInteractionCount") != 0
                or load_evidence.get("durationTargetMet") is not True
            ):
                fail(
                    "release.child_load_evidence_invalid",
                    "Load child report did not preserve the canonical zero-error or duration-target boundary.",
                    {
                        "unexpectedFailureCount": load_evidence.get("unexpectedFailureCount"),
                        "unexpectedErrorRate": load_evidence.get("unexpectedErrorRate"),
                        "doubleExecution": load_evidence.get("doubleExecution"),
                        "duplicateTerminal": load_evidence.get("duplicateTerminal"),
                        "pendingInteractionCount": load_evidence.get("pendingInteractionCount"),
                        "durationTargetMet": load_evidence.get("durationTargetMet"),
                    },
                )
            if (
                not isinstance(restart_every_waves, int)
                or restart_every_waves < 0
                or not isinstance(restart_count, int)
                or restart_count < 0
                or not isinstance(restarts, list)
            ):
                fail(
                    "release.child_load_evidence_invalid",
                    "Load child report omitted restart cadence evidence or used the wrong types.",
                    {
                        "restartEveryWaves": restart_every_waves,
                        "controlPlaneRestartCount": restart_count,
                        "controlPlaneRestartsType": type(restarts).__name__,
                    },
                )
            elif isinstance(waves_completed, int):
                expected_restart_waves = (
                    list(range(restart_every_waves, waves_completed, restart_every_waves))
                    if restart_every_waves > 0
                    else []
                )
                if restart_count != len(expected_restart_waves) or len(restarts) != restart_count:
                    fail(
                        "release.child_load_evidence_invalid",
                        "Load child report did not retain the canonical Control Plane restart count.",
                        {
                            "restartEveryWaves": restart_every_waves,
                            "wavesCompleted": waves_completed,
                            "expectedRestartCount": len(expected_restart_waves),
                            "controlPlaneRestartCount": restart_count,
                            "controlPlaneRestarts": restarts,
                        },
                    )
                else:
                    actual_restart_waves: list[int] = []
                    for restart in restarts:
                        if not isinstance(restart, Mapping):
                            fail(
                                "release.child_load_evidence_invalid",
                                "Load child report contained a malformed Control Plane restart record.",
                            )
                            continue
                        after_wave = restart.get("afterWave")
                        pre_restart_sequences = restart.get("preRestartSequences")
                        process_generation = restart.get("processGeneration")
                        pre_restart_sequence = restart.get("preRestartSequence")
                        if not isinstance(after_wave, int):
                            fail(
                                "release.child_load_evidence_invalid",
                                "Load child report restart evidence omitted its completed-wave boundary.",
                                {"restart": dict(restart)},
                            )
                            continue
                        actual_restart_waves.append(after_wave)
                        if (
                            not isinstance(pre_restart_sequences, Mapping)
                            or len(pre_restart_sequences) != acceptance.REAL_PROVIDER_LOAD_SESSIONS
                            or any(
                                not isinstance(sequence, int) or sequence <= 0
                                for sequence in pre_restart_sequences.values()
                            )
                            or not isinstance(process_generation, int)
                            or process_generation <= 0
                            or not isinstance(pre_restart_sequence, int)
                            or pre_restart_sequence <= 0
                        ):
                            fail(
                                "release.child_load_evidence_invalid",
                                "Load child report restart evidence omitted canonical pre-restart sequence metadata.",
                                {"restart": dict(restart)},
                            )
                        if (
                            restart.get("postRestartWave") != after_wave + 1
                            or restart.get("postRestartNativeCursorVerified") is not True
                            or restart.get("postRestartSessionSequenceContinuityVerified")
                            is not True
                            or restart.get("postRestartExecutionIdReuseVerified") is not True
                            or restart.get("postRestartTerminalPathUniquenessVerified")
                            is not True
                        ):
                            fail(
                                "release.child_load_evidence_invalid",
                                "Load child report restart evidence did not prove the required post-restart continuity checks.",
                                {"restart": dict(restart)},
                            )
                    if actual_restart_waves != expected_restart_waves:
                        fail(
                            "release.child_load_evidence_invalid",
                            "Load child report restart evidence used the wrong wave boundaries.",
                            {
                                "expectedRestartWaves": expected_restart_waves,
                                "actualRestartWaves": actual_restart_waves,
                            },
                        )
            require_distribution(
                load_evidence.get("controlPlaneAdmissionLatencyMs"),
                label="controlPlaneAdmissionLatencyMs",
                expected_sample_count=executions_completed if isinstance(executions_completed, int) else None,
            )
            require_distribution(
                load_evidence.get("slotReuseAdmissionLatencyMs"),
                label="slotReuseAdmissionLatencyMs",
                expected_sample_count=expected_rejections if isinstance(expected_rejections, int) else None,
            )
            require_distribution(
                load_evidence.get("interactionReadyLatencyMs"),
                label="interactionReadyLatencyMs",
                expected_sample_count=executions_completed if isinstance(executions_completed, int) else None,
            )
            require_distribution(
                load_evidence.get("turnCompletionLatencyMs"),
                label="turnCompletionLatencyMs",
                expected_sample_count=executions_completed if isinstance(executions_completed, int) else None,
            )
            require_distribution(
                load_evidence.get("waveDurationMs"),
                label="waveDurationMs",
                expected_sample_count=waves_completed if isinstance(waves_completed, int) else None,
            )
            provider_counts = load_evidence.get("providerExecutionCounts")
            if provider_counts != {provider: executions_completed}:
                fail(
                    "release.child_load_evidence_invalid",
                    "Load child report did not retain the canonical provider execution counts.",
                    {"providerExecutionCounts": provider_counts},
                )
            session_counts = load_evidence.get("sessionExecutionCounts")
            if (
                not isinstance(session_counts, Mapping)
                or len(session_counts) != acceptance.REAL_PROVIDER_LOAD_SESSIONS
                or (
                    isinstance(waves_completed, int)
                    and any(count != waves_completed for count in session_counts.values())
                )
            ):
                fail(
                    "release.child_load_evidence_invalid",
                    "Load child report did not retain the canonical per-session execution counts.",
                    {"sessionExecutionCounts": session_counts},
                )
            wave_samples = load_evidence.get("waveSamples")
            if not isinstance(wave_samples, list) or not wave_samples:
                fail(
                    "release.child_load_evidence_invalid",
                    "Load child report omitted its canonical waveSamples diagnostics.",
                )
            else:
                for sample in wave_samples:
                    if not isinstance(sample, Mapping):
                        fail(
                            "release.child_load_evidence_invalid",
                            "Load child report contained a malformed wave sample.",
                        )
                        continue
                    quota_rejections = sample.get("quotaRejections")
                    overlap_worker_ids = sample.get("overlapWorkerIds")
                    if not isinstance(quota_rejections, list) or len(quota_rejections) != (
                        acceptance.REAL_PROVIDER_LOAD_SESSIONS - acceptance.REAL_PROVIDER_LOAD_CONCURRENCY
                    ):
                        fail(
                            "release.child_load_evidence_invalid",
                            "Load child report waveSamples did not retain the canonical quota-rejection diagnostics.",
                            {"wave": sample.get("wave")},
                        )
                    if not isinstance(overlap_worker_ids, list) or len(overlap_worker_ids) != (
                        acceptance.REAL_PROVIDER_LOAD_SESSIONS - 1
                    ):
                        fail(
                            "release.child_load_evidence_invalid",
                            "Load child report waveSamples did not retain the canonical overlap diagnostics.",
                            {"wave": sample.get("wave")},
                        )
                    require_distribution(
                        sample.get("controlPlaneAdmissionLatencyMs"),
                        label="waveSamples.controlPlaneAdmissionLatencyMs",
                        expected_sample_count=acceptance.REAL_PROVIDER_LOAD_SESSIONS,
                    )
                    require_distribution(
                        sample.get("slotReuseAdmissionLatencyMs"),
                        label="waveSamples.slotReuseAdmissionLatencyMs",
                        expected_sample_count=(
                            acceptance.REAL_PROVIDER_LOAD_SESSIONS
                            - acceptance.REAL_PROVIDER_LOAD_CONCURRENCY
                        ),
                    )
                    require_distribution(
                        sample.get("interactionReadyLatencyMs"),
                        label="waveSamples.interactionReadyLatencyMs",
                        expected_sample_count=acceptance.REAL_PROVIDER_LOAD_SESSIONS,
                    )
                    require_distribution(
                        sample.get("turnCompletionLatencyMs"),
                        label="waveSamples.turnCompletionLatencyMs",
                        expected_sample_count=acceptance.REAL_PROVIDER_LOAD_SESSIONS,
                    )
            load_configuration = (
                configuration.get("realProviderLoad") if isinstance(configuration, Mapping) else None
            )
            measurement = (
                load_configuration.get("measurement")
                if isinstance(load_configuration, Mapping)
                else None
            )
            sla_file = (
                measurement.get("operatorApprovedSlaFile")
                if isinstance(measurement, Mapping)
                else None
            )
            configured_waves = (
                load_configuration.get("waves")
                if isinstance(load_configuration, Mapping)
                else None
            )
            configured_maximum_waves = (
                load_configuration.get("maximumWaves")
                if isinstance(load_configuration, Mapping)
                else None
            )
            configured_restart_every_waves = (
                load_configuration.get("restartEveryWaves")
                if isinstance(load_configuration, Mapping)
                else None
            )
            if (
                not isinstance(load_configuration, Mapping)
                or load_configuration.get("workers") != acceptance.REAL_PROVIDER_LOAD_CONCURRENCY
                or load_configuration.get("sessions") != acceptance.REAL_PROVIDER_LOAD_SESSIONS
                or not isinstance(configured_waves, int)
                or configured_waves < 1
                or not isinstance(configured_restart_every_waves, int)
                or configured_restart_every_waves < 0
                or not isinstance(load_configuration.get("minimumDurationSeconds"), (int, float))
                or float(load_configuration.get("minimumDurationSeconds")) <= 0
                or not isinstance(configured_maximum_waves, int)
                or configured_maximum_waves < configured_waves
                or not isinstance(measurement, Mapping)
                or measurement.get("operatorApprovedSlaThresholdsEnforced") is not True
                or not isinstance(sla_file, Mapping)
                or re.fullmatch(r"[0-9a-f]{64}", str(sla_file.get("sha256") or "")) is None
            ):
                fail(
                    "release.child_load_configuration_invalid",
                    "Load child report omitted the canonical operator-approved SLA configuration metadata.",
                )
            else:
                if load_evidence.get("restartEveryWaves") != configured_restart_every_waves:
                    fail(
                        "release.child_load_configuration_invalid",
                        "Load child report restart cadence evidence did not match its configuration.",
                        {
                            "configuredRestartEveryWaves": configured_restart_every_waves,
                            "evidenceRestartEveryWaves": load_evidence.get("restartEveryWaves"),
                        },
                    )
                require_operator_approved_sla(measurement.get("operatorApprovedSla"))

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
    capture_process_output: bool = False,
    process_output_redactor: acceptance.SecretRedactor | None = None,
    forbidden_output_tokens: Sequence[str] = (),
) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    child_dir = output_dir / provider / matrix
    started = time.monotonic()
    completed = subprocess.run(
        command,
        cwd=repo_root,
        env=environment,
        check=False,
        capture_output=capture_process_output,
        text=capture_process_output,
        encoding="utf-8" if capture_process_output else None,
        errors="replace" if capture_process_output else None,
    )
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
    if capture_process_output:
        process_output_scan = scan_process_output(
            completed.stdout if isinstance(completed.stdout, str) else "",
            completed.stderr if isinstance(completed.stderr, str) else "",
            redactor=process_output_redactor,
            forbidden_tokens=forbidden_output_tokens,
        )
        record["processOutputScan"] = process_output_scan
        if process_output_scan["findings"]:
            errors.append(
                ReleaseGateError(
                    "release.child_process_output_secret_scan_failed",
                    "Child process stdout or stderr contained controlled Credential material or forbidden environment names.",
                    {"findingCount": len(process_output_scan["findings"])},
                ).as_report_error(provider=provider, matrix=matrix)
            )
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


def scan_process_output(
    stdout: str,
    stderr: str,
    *,
    redactor: acceptance.SecretRedactor | None,
    forbidden_tokens: Sequence[str] = (),
) -> dict[str, Any]:
    combined = f"{stdout}\n{stderr}".encode("utf-8", errors="replace")
    findings: list[dict[str, Any]] = []
    if redactor is not None:
        for index, secret in enumerate(redactor.secret_values(), start=1):
            if secret.encode("utf-8") in combined:
                findings.append({"kind": f"known-secret-{index}"})
        # Keep a redacted representation as the only form that may be handled
        # after the raw capture has been scanned. Neither representation is
        # persisted by the release gate.
        redactor.text(stdout)
        redactor.text(stderr)
    for token in sorted({token for token in forbidden_tokens if token}):
        if token.encode("utf-8") in combined:
            findings.append({"kind": "forbidden-environment-name"})
    for kind, pattern in acceptance.SECRET_SCAN_PATTERNS:
        if pattern.search(combined) is not None:
            findings.append({"kind": kind})
    return {
        "captured": True,
        "stdoutBytes": len(stdout.encode("utf-8")),
        "stderrBytes": len(stderr.encode("utf-8")),
        "findings": findings,
        "redactionApplied": redactor is not None,
        "rawOutputPersisted": False,
        "redactedOutputPersisted": False,
    }


def output_file_token_findings(
    output_dir: pathlib.Path,
    forbidden_tokens: Sequence[str],
) -> list[dict[str, Any]]:
    tokens = sorted(
        {token.encode("utf-8") for token in forbidden_tokens if token},
        key=len,
        reverse=True,
    )
    if not tokens or not output_dir.is_dir():
        return []
    overlap = max(len(token) for token in tokens) - 1
    findings: list[dict[str, Any]] = []
    for path in sorted(output_dir.rglob("*")):
        if path.is_symlink() or not path.is_file():
            continue
        carry = b""
        found = False
        with path.open("rb") as source:
            while chunk := source.read(1 << 20):
                window = carry + chunk
                if any(token in window for token in tokens):
                    found = True
                    break
                carry = window[-overlap:] if overlap > 0 else b""
        if found:
            findings.append(
                {
                    "file": str(path.relative_to(output_dir)),
                    "kind": "forbidden-environment-name",
                }
            )
    return findings


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
