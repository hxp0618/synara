from __future__ import annotations

import contextlib
import dataclasses
import io
import json
import os
import pathlib
import subprocess
import tempfile
import unittest
from typing import Any
from unittest import mock

import acceptance_runner as acceptance
import controlled_remote_release_gate as remote
import kubernetes_release_gate as gate
import release_gate_common as common


WORKER_IMAGE_ID = "sha256:" + "e" * 64
WORKER_IMAGE_NAME = "synara-stage3-provider-kubernetes-release-gate:aaaaaaaaaaaa-owner"


def kubernetes_options(output_dir: pathlib.Path) -> gate.KubernetesReleaseGateOptions:
    return gate.KubernetesReleaseGateOptions(
        repo_root=pathlib.Path("/tmp/synara").resolve(),
        output_dir=output_dir,
        product_timeout_seconds=3600.0,
        failure_timeout_seconds=1200.0,
        real_provider_load_sla_file=(
            pathlib.Path("/tmp/synara/deploy/worker/production-load-sla.json").resolve()
        ),
        real_provider_load_restart_every_waves=10,
        codex_credential=gate.CredentialSource("CODEX_KEY", "apiKey", None),
        claude_credential=gate.CredentialSource(
            "CLAUDE_TOKEN",
            "authToken",
            "CLAUDE_BASE_URL",
        ),
        docker_socket_path=pathlib.Path("/var/run/docker.sock"),
        kubernetes_control_plane_host="host.docker.internal",
        kind_bin="kind",
        kind_node_image="kindest/node:v1.33.1",
        codex_model="gpt-5.6-sol",
        claude_model="claude-sonnet-4-6",
    )


def sample_load_operator_approved_sla() -> dict[str, Any]:
    return {
        "requested": {
            "minimumDurationSeconds": 1800,
            "latencyMs": {"p95Max": 10000, "p99Max": 15000},
            "recoveryTimeMs": {"p95Max": 2000, "p99Max": 3000},
            "unexpectedErrorRateMax": 0.0,
        },
        "metricMapping": {
            "minimumDurationSeconds": {"observedEvidencePath": "durationMs"},
            "latencyMs.p95Max": {"observedEvidencePath": "turnLatencyMs.p95"},
            "latencyMs.p99Max": {"observedEvidencePath": "turnLatencyMs.p99"},
            "recoveryTimeMs.p95Max": {"observedEvidencePath": "admissionRecoveryMs.p95"},
            "recoveryTimeMs.p99Max": {"observedEvidencePath": "admissionRecoveryMs.p99"},
            "unexpectedErrorRateMax": {"observedEvidencePath": "unexpectedErrorRate"},
        },
        "checks": [
            {"id": "minimumDurationSeconds", "status": "pass"},
            {"id": "latencyMs.p95Max", "status": "pass"},
            {"id": "latencyMs.p99Max", "status": "pass"},
            {"id": "recoveryTimeMs.p95Max", "status": "pass"},
            {"id": "recoveryTimeMs.p99Max", "status": "pass"},
            {"id": "unexpectedErrorRateMax", "status": "pass"},
        ],
        "enforced": True,
    }


def sample_load_case_evidence(provider: str) -> dict[str, Any]:
    summary = sample_load_operator_approved_sla()
    return {
        "maxConcurrentExecutions": 2,
        "workers": 2,
        "sessions": 4,
        "providers": [provider],
        "wavesRequested": 1,
        "minimumWavesRequested": 1,
        "maximumWaves": 400,
        "restartEveryWaves": 10,
        "minimumDurationSeconds": 1800,
        "durationTargetMet": True,
        "stopReason": "minimum-waves-and-duration-satisfied",
        "wavesCompleted": 1,
        "firstWave": 1,
        "lastWave": 1,
        "controlPlaneRestartCount": 0,
        "controlPlaneRestarts": [],
        "executionsCompleted": 4,
        "distinctExecutionCount": 4,
        "distinctWorkerCount": 2,
        "quotaRejections": 2,
        "admissionRetriesSucceeded": 2,
        "admissionAttempts": 6,
        "expectedQuotaRejectionRate": 0.333333,
        "overlapObservations": 3,
        "effectiveConcurrency": 2,
        "executionSuccessRate": 1.0,
        "unexpectedFailureCount": 0,
        "unexpectedErrorRate": 0.0,
        "doubleExecution": False,
        "duplicateTerminal": False,
        "pendingInteractionCount": 0,
        "providerExecutionCounts": {provider: 4},
        "sessionExecutionCounts": {
            "session-1": 1,
            "session-2": 1,
            "session-3": 1,
            "session-4": 1,
        },
        "eventTypeCounts": {
            "execution.completed": 4,
            "request.opened": 4,
            "request.resolved": 4,
        },
        "resourceProfile": {"target": "kubernetes"},
        "durationMs": 1_800_000,
        "observedCompletedExecutionsPerSecond": 0.002,
        "turnLatencyMs": {
            "sampleCount": 4,
            "minimum": 9000,
            "maximum": 12000,
            "average": 10250.0,
            "p50": 10000,
            "p95": 12000,
            "p99": 12000,
        },
        "waveDurationMs": {
            "sampleCount": 1,
            "minimum": 1_800_000,
            "maximum": 1_800_000,
            "average": 1_800_000.0,
            "p50": 1_800_000,
            "p95": 1_800_000,
            "p99": 1_800_000,
        },
        "admissionRecoveryMs": {
            "sampleCount": 2,
            "minimum": 1200,
            "maximum": 1800,
            "average": 1500.0,
            "p50": 1200,
            "p95": 1800,
            "p99": 1800,
        },
        "sessionsEvidence": [
            {"sessionId": "session-1", "provider": provider},
            {"sessionId": "session-2", "provider": provider},
            {"sessionId": "session-3", "provider": provider},
            {"sessionId": "session-4", "provider": provider},
        ],
        "waveSamples": [
            {
                "wave": 1,
                "sessionOrder": ["session-1", "session-2", "session-3", "session-4"],
                "providerOrder": [provider] * 4,
                "overlapWorkerIds": [
                    ["worker-1", "worker-2"],
                    ["worker-1", "worker-2"],
                    ["worker-1", "worker-2"],
                ],
                "targetExecutionIdentities": [
                    ["pod-1", "pod-2"],
                    ["pod-1", "pod-2"],
                    ["pod-1", "pod-2"],
                ],
                "quotaRejections": [
                    {
                        "sessionId": "session-3",
                        "reasonCode": "execution_quota_exceeded",
                        "stateMutated": False,
                        "wave": 1,
                        "durationMs": 100,
                    },
                    {
                        "sessionId": "session-4",
                        "reasonCode": "execution_quota_exceeded",
                        "stateMutated": False,
                        "wave": 1,
                        "durationMs": 100,
                    },
                ],
                "turnLatencyMs": {
                    "sampleCount": 4,
                    "minimum": 9000,
                    "maximum": 12000,
                    "average": 10250.0,
                    "p50": 10000,
                    "p95": 12000,
                    "p99": 12000,
                },
                "durationMs": 1_800_000,
            }
        ],
        "operatorApprovedSla": summary,
    }


def sample_child_report(
    options: gate.KubernetesReleaseGateOptions,
    provider: str,
    matrix: str,
    *,
    git_sha: str = "a" * 40,
    catalog_hash: str = "b" * 64,
    worker_image_name: str = WORKER_IMAGE_NAME,
) -> dict[str, Any]:
    requested_product = list(acceptance.REAL_PROVIDER_CASES) if matrix == "product" else []
    requested_failure = list(acceptance.REAL_PROVIDER_FAILURE_CASES) if matrix == "failure" else []
    requested_load = [acceptance.REAL_PROVIDER_LOAD_CASE_ID] if matrix == common.REMOTE_LOAD_MATRIX else []
    source = gate.credential_source(options, provider)
    load_sla = sample_load_operator_approved_sla()
    case_statuses: dict[str, str] = {
        "environment.control-plane-start": "pass",
        "identity.dev-login": "pass",
        "runtime.target-provision": "pass",
        "resources.real-provider-project-session": "pass",
        "real-provider.turn-1-start": "pass",
        "runtime.real-provider-worker-discovery": "pass",
        "real-provider.turn-1": "pass",
        "recovery.control-plane-restart": "pass",
        "real-provider.turn-2-continuity": "pass",
    }
    for case_id in common.expected_case_ids(matrix):
        case_statuses[case_id] = "pass"
    for case_id in gate.EXPECTED_UNSUPPORTED[(provider, matrix)]:
        case_statuses[case_id] = "unsupported"
    cases = []
    for case_id, status in sorted(case_statuses.items()):
        case: dict[str, Any] = {"id": case_id, "status": status}
        if matrix == common.REMOTE_LOAD_MATRIX and case_id == acceptance.REAL_PROVIDER_LOAD_CASE_ID:
            case["evidence"] = sample_load_case_evidence(provider)
        cases.append(case)
    cases.extend(
        [
            {
                "id": "environment.target-prepare",
                "status": "pass",
                "evidence": {
                    "kubernetes": {
                        "containerEngine": {
                            "build": "skipped",
                            "workerImage": worker_image_name,
                            "workerImageId": WORKER_IMAGE_ID,
                        }
                    }
                },
            },
            {
                "id": "environment.cleanup",
                "status": "pass",
                "evidence": {
                    "ownedClusterRemoved": True,
                    "ownedWorkerImageRemoved": False,
                    "ownedCanaryImageRemoved": False,
                    "reusedClusterResourcesRemoved": False,
                    "stateRemoved": True,
                    "broadCleanupUsed": False,
                },
            },
            {
                "id": "security.output-secret-scan",
                "status": "pass",
                "evidence": {"findings": []},
            },
        ]
    )
    return {
        "schemaVersion": acceptance.SCHEMA_VERSION,
        "runId": f"run-{provider}-{matrix}",
        "mode": "real-provider-load" if matrix == common.REMOTE_LOAD_MATRIX else "real-provider-smoke",
        "target": "kubernetes",
        "provider": provider,
        "status": "pass",
        "durationMs": 123,
        "source": {
            "gitSha": git_sha,
            "worktreeDirty": False,
            "providerCapabilityCatalogSha256": catalog_hash,
        },
        "configuration": {
            "restartControlPlane": matrix != common.REMOTE_LOAD_MATRIX,
            "keepState": False,
            "runnerCommand": {"executable": "provider-host"},
            "kubernetes": {
                "workerImage": worker_image_name,
                "skipWorkerBuild": True,
            },
            "realProviderLoad": {
                "workers": 2 if matrix == common.REMOTE_LOAD_MATRIX else 0,
                "sessions": 4 if matrix == common.REMOTE_LOAD_MATRIX else 0,
                "waves": 1 if matrix == common.REMOTE_LOAD_MATRIX else 0,
                "restartEveryWaves": (
                    options.real_provider_load_restart_every_waves
                    if matrix == common.REMOTE_LOAD_MATRIX
                    else 0
                ),
                "minimumDurationSeconds": 1800 if matrix == common.REMOTE_LOAD_MATRIX else 0,
                "maximumWaves": 400 if matrix == common.REMOTE_LOAD_MATRIX else 0,
                "maxConcurrentExecutions": 2 if matrix == common.REMOTE_LOAD_MATRIX else None,
                "resourceProfile": (
                    sample_load_case_evidence(provider)["resourceProfile"]
                    if matrix == common.REMOTE_LOAD_MATRIX
                    else None
                ),
                "measurement": {
                    "durationTargetEnforced": matrix == common.REMOTE_LOAD_MATRIX,
                    "latencyPercentiles": [50, 95, 99] if matrix == common.REMOTE_LOAD_MATRIX else [],
                    "unexpectedErrorRateRecorded": matrix == common.REMOTE_LOAD_MATRIX,
                    "operatorApprovedSlaThresholdsEnforced": matrix == common.REMOTE_LOAD_MATRIX,
                    "operatorApprovedSla": load_sla if matrix == common.REMOTE_LOAD_MATRIX else None,
                    "operatorApprovedSlaFile": (
                        {
                            "path": "deploy/worker/production-load-sla.json",
                            "sourceKind": "repo-relative",
                            "sha256": "d" * 64,
                            "requested": load_sla["requested"],
                        }
                        if matrix == common.REMOTE_LOAD_MATRIX
                        else None
                    ),
                },
                "boundary": "operator-approved real Provider load",
            },
            "realProvider": {
                "requestedCases": requested_product,
                "requestedFailureCases": requested_failure,
                "requestedLoadCases": requested_load,
                "ambientAuthentication": False,
                "controlledProductCredential": True,
                "controlledProductCredentialField": source.field,
                "productCredentialEnvironmentNamePersisted": False,
                "controlledBaseUrl": source.base_url_environment_name is not None,
                "model": gate.provider_model(options, provider),
                "controlledFaultCredentials": matrix == "failure",
                "cursorMaximumAge": (
                    acceptance.REAL_PROVIDER_CURSOR_MAX_AGE if matrix == "failure" else None
                ),
            },
        },
        "cases": cases,
    }


class ParseArgsTest(unittest.TestCase):
    def test_reads_only_controlled_environment_names_into_options(self) -> None:
        with tempfile.TemporaryDirectory() as directory, mock.patch.dict(
            os.environ,
            {
                "CODEX_KEY": "codex-secret-value",
                "CLAUDE_TOKEN": "claude-secret-value",
                "CLAUDE_BASE_URL": "https://claude.example.test",
            },
        ):
            options = gate.parse_args(
                [
                    "--codex-credential-env",
                    "CODEX_KEY",
                    "--codex-model",
                    "gpt-5.6-sol",
                    "--claude-credential-env",
                    "CLAUDE_TOKEN",
                    "--claude-credential-field",
                    "authToken",
                    "--claude-base-url-env",
                    "CLAUDE_BASE_URL",
                    "--claude-model",
                    "claude-sonnet-4-6",
                    "--output-dir",
                    directory,
                ]
            )

        encoded = json.dumps(dataclasses.asdict(options), default=str)
        self.assertEqual(options.claude_credential.field, "authToken")
        self.assertEqual(options.codex_model, "gpt-5.6-sol")
        self.assertEqual(options.claude_model, "claude-sonnet-4-6")
        self.assertEqual(options.kind_node_image, "kindest/node:v1.33.1")
        self.assertEqual(options.real_provider_load_restart_every_waves, 10)
        self.assertNotIn("codex-secret-value", encoded)
        self.assertNotIn("claude-secret-value", encoded)
        self.assertNotIn("https://claude.example.test", encoded)

    def test_accepts_custom_real_provider_load_restart_cadence(self) -> None:
        with tempfile.TemporaryDirectory() as directory, mock.patch.dict(
            os.environ,
            {
                "CODEX_KEY": "codex-secret-value",
                "CLAUDE_TOKEN": "claude-secret-value",
            },
        ):
            options = gate.parse_args(
                [
                    "--codex-credential-env",
                    "CODEX_KEY",
                    "--claude-credential-env",
                    "CLAUDE_TOKEN",
                    "--real-provider-load-restart-every-waves",
                    "12",
                    "--output-dir",
                    directory,
                ]
            )

        self.assertEqual(options.real_provider_load_restart_every_waves, 12)

    def test_resolves_provider_models_from_environment_names(self) -> None:
        with tempfile.TemporaryDirectory() as directory, mock.patch.dict(
            os.environ,
            {
                "CODEX_KEY": "codex-secret-value",
                "CLAUDE_TOKEN": "claude-secret-value",
                "CODEX_MODEL": "gpt-5.6-sol",
                "CLAUDE_MODEL": "claude-sonnet-4-6",
            },
        ):
            options = gate.parse_args(
                [
                    "--codex-credential-env",
                    "CODEX_KEY",
                    "--codex-model-env",
                    "CODEX_MODEL",
                    "--claude-credential-env",
                    "CLAUDE_TOKEN",
                    "--claude-model-env",
                    "CLAUDE_MODEL",
                    "--output-dir",
                    directory,
                ]
            )

        encoded = json.dumps(dataclasses.asdict(options), default=str)
        self.assertEqual(options.codex_model, "gpt-5.6-sol")
        self.assertEqual(options.claude_model, "claude-sonnet-4-6")
        self.assertEqual(options.codex_model_environment_name, "CODEX_MODEL")
        self.assertEqual(options.claude_model_environment_name, "CLAUDE_MODEL")
        self.assertIn("CODEX_MODEL", encoded)
        self.assertIn("CLAUDE_MODEL", encoded)

    def test_model_literal_and_environment_name_are_mutually_exclusive(self) -> None:
        with mock.patch.dict(
            os.environ,
            {
                "CODEX_KEY": "codex-secret-value",
                "CLAUDE_TOKEN": "claude-secret-value",
                "CODEX_MODEL": "gpt-5.6-sol",
                "CLAUDE_MODEL": "claude-sonnet-4-6",
            },
        ):
            for provider_args in (
                ["--codex-model", "gpt-5.6-sol", "--codex-model-env", "CODEX_MODEL"],
                [
                    "--claude-model",
                    "claude-sonnet-4-6",
                    "--claude-model-env",
                    "CLAUDE_MODEL",
                ],
            ):
                with self.subTest(provider_args=provider_args):
                    with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(
                        SystemExit
                    ) as caught:
                        gate.parse_args(
                            [
                                "--codex-credential-env",
                                "CODEX_KEY",
                                "--claude-credential-env",
                                "CLAUDE_TOKEN",
                                *provider_args,
                            ]
                        )
                    self.assertEqual(caught.exception.code, 2)

    def test_fails_before_work_when_a_credential_value_is_missing(self) -> None:
        with mock.patch.dict(os.environ, {"CODEX_KEY": "", "CLAUDE_KEY": "claude-secret"}):
            with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as caught:
                gate.parse_args(
                    [
                        "--codex-credential-env",
                        "CODEX_KEY",
                        "--claude-credential-env",
                        "CLAUDE_KEY",
                    ]
                )

        self.assertEqual(caught.exception.code, 2)


class ChildCommandAndPolicyTest(unittest.TestCase):
    def test_child_commands_share_one_image_and_keep_provider_environments_separate(self) -> None:
        options = kubernetes_options(pathlib.Path("/tmp/kubernetes-release"))
        product = gate.child_command(
            options,
            "codex",
            "product",
            options.output_dir / "codex/product",
            WORKER_IMAGE_NAME,
        )
        load = gate.child_command(
            options,
            "codex",
            common.REMOTE_LOAD_MATRIX,
            options.output_dir / "codex/load",
            WORKER_IMAGE_NAME,
        )
        failure = gate.child_command(
            options,
            "claudeAgent",
            "failure",
            options.output_dir / "claudeAgent/failure",
            WORKER_IMAGE_NAME,
        )

        self.assertIn("--real-provider-matrix", product)
        self.assertNotIn("--real-provider-failure-matrix", product)
        self.assertEqual(load[load.index("--suite") + 1], "real-provider-load")
        self.assertNotIn("--real-provider-matrix", load)
        self.assertNotIn("--real-provider-failure-matrix", load)
        self.assertEqual(load[load.index("--timeout") + 1], str(options.product_timeout_seconds))
        self.assertEqual(
            load[load.index("--real-provider-load-restart-every-waves") + 1],
            str(options.real_provider_load_restart_every_waves),
        )
        self.assertEqual(
            load[load.index("--operator-approved-sla-file") + 1],
            str(options.real_provider_load_sla_file),
        )
        self.assertIn("--real-provider-failure-matrix", failure)
        self.assertIn("--kubernetes-skip-worker-build", product)
        self.assertIn("--kubernetes-worker-image", product)
        self.assertIn(WORKER_IMAGE_NAME, product)
        self.assertIn("--kubernetes-skip-worker-build", load)
        self.assertIn("--kubernetes-worker-image", load)
        self.assertIn(WORKER_IMAGE_NAME, load)
        self.assertIn("kindest/node:v1.33.1", product)
        self.assertIn("CODEX_KEY", product)
        self.assertEqual(product[product.index("--real-provider-model") + 1], "gpt-5.6-sol")
        self.assertIn("CLAUDE_TOKEN", failure)
        self.assertIn("CLAUDE_BASE_URL", failure)
        self.assertEqual(
            failure[failure.index("--real-provider-model") + 1],
            "claude-sonnet-4-6",
        )

        with mock.patch.dict(
            os.environ,
            {
                "CODEX_KEY": "codex-secret",
                "CLAUDE_TOKEN": "claude-secret",
                "CLAUDE_BASE_URL": "https://claude.example.test",
                "DATABASE_URL": "postgres://ambient-secret",
            },
        ):
            codex = remote.child_environment(options, "codex")
            claude = remote.child_environment(options, "claudeAgent")

        self.assertEqual(codex["CODEX_KEY"], "codex-secret")
        self.assertNotIn("CLAUDE_TOKEN", codex)
        self.assertEqual(claude["CLAUDE_TOKEN"], "claude-secret")
        self.assertNotIn("CODEX_KEY", claude)
        self.assertNotIn("DATABASE_URL", codex)
        self.assertNotIn("DATABASE_URL", claude)
        self.assertEqual(codex["DOCKER_HOST"], "unix:///var/run/docker.sock")
        self.assertEqual(claude["DOCKER_HOST"], "unix:///var/run/docker.sock")
        self.assertNotIn("DOCKER_CONTEXT", codex)
        self.assertNotIn("DOCKER_CONTEXT", claude)

    def test_child_command_uses_resolved_model_without_forwarding_model_env_name(self) -> None:
        with tempfile.TemporaryDirectory() as directory, mock.patch.dict(
            os.environ,
            {
                "CODEX_KEY": "codex-secret-value",
                "CLAUDE_TOKEN": "claude-secret-value",
                "CODEX_MODEL": "gpt-5.6-sol",
                "CLAUDE_MODEL": "claude-sonnet-4-6",
            },
        ):
            options = gate.parse_args(
                [
                    "--codex-credential-env",
                    "CODEX_KEY",
                    "--codex-model-env",
                    "CODEX_MODEL",
                    "--claude-credential-env",
                    "CLAUDE_TOKEN",
                    "--claude-model-env",
                    "CLAUDE_MODEL",
                    "--output-dir",
                    directory,
                ]
            )

        product = gate.child_command(
            options,
            "codex",
            "product",
            options.output_dir / "codex/product",
            WORKER_IMAGE_NAME,
        )
        failure = gate.child_command(
            options,
            "claudeAgent",
            "failure",
            options.output_dir / "claudeAgent/failure",
            WORKER_IMAGE_NAME,
        )

        self.assertEqual(product[product.index("--real-provider-model") + 1], "gpt-5.6-sol")
        self.assertEqual(
            failure[failure.index("--real-provider-model") + 1],
            "claude-sonnet-4-6",
        )
        self.assertNotIn("CODEX_MODEL", product)
        self.assertNotIn("CLAUDE_MODEL", failure)
        self.assertNotIn("CODEX_MODEL", json.dumps([product, failure]))
        self.assertNotIn("CLAUDE_MODEL", json.dumps([product, failure]))

    def test_accepts_nested_kubernetes_worker_image_and_exact_cleanup(self) -> None:
        options = kubernetes_options(pathlib.Path("/tmp/kubernetes-release"))
        policy = gate.child_policy(options, WORKER_IMAGE_NAME)
        for provider in common.PROVIDERS:
            for matrix in gate.REMOTE_GATE_MATRICES:
                with self.subTest(provider=provider, matrix=matrix):
                    errors = common.validate_child_report(
                        sample_child_report(options, provider, matrix),
                        provider=provider,
                        matrix=matrix,
                        expected_git_sha="a" * 40,
                        policy=policy,
                    )
                    self.assertEqual(errors, [])

    def test_rejects_missing_real_provider_smoke_baseline_cases(self) -> None:
        options = kubernetes_options(pathlib.Path("/tmp/kubernetes-release"))
        policy = gate.child_policy(options, WORKER_IMAGE_NAME)
        report = sample_child_report(options, "codex", "product")
        report["cases"] = [
            case
            for case in report["cases"]
            if case["id"] != "real-provider.turn-1-start"
        ]

        errors = common.validate_child_report(
            report,
            provider="codex",
            matrix="product",
            expected_git_sha="a" * 40,
            policy=policy,
        )

        self.assertEqual(errors[0]["code"], "release.child_cases_missing")
        self.assertIn("real-provider.turn-1-start", errors[0]["evidence"]["missingCaseIds"])

    def test_rejects_status_only_real_provider_load_case(self) -> None:
        options = kubernetes_options(pathlib.Path("/tmp/kubernetes-release"))
        policy = gate.child_policy(options, WORKER_IMAGE_NAME)
        report = sample_child_report(options, "codex", common.REMOTE_LOAD_MATRIX)
        for case in report["cases"]:
            if case["id"] == acceptance.REAL_PROVIDER_LOAD_CASE_ID:
                case.pop("evidence", None)

        errors = common.validate_child_report(
            report,
            provider="codex",
            matrix=common.REMOTE_LOAD_MATRIX,
            expected_git_sha="a" * 40,
            policy=policy,
        )

        self.assertEqual(errors[0]["code"], "release.child_load_evidence_invalid")

    def test_rejects_child_image_removal_or_cluster_cleanup_failure(self) -> None:
        options = kubernetes_options(pathlib.Path("/tmp/kubernetes-release"))
        report = sample_child_report(options, "codex", "failure")
        for case in report["cases"]:
            if case["id"] == "environment.cleanup":
                case["evidence"]["ownedClusterRemoved"] = False
                case["evidence"]["ownedWorkerImageRemoved"] = True

        errors = common.validate_child_report(
            report,
            provider="codex",
            matrix="failure",
            expected_git_sha="a" * 40,
            policy=gate.child_policy(options, WORKER_IMAGE_NAME),
        )

        self.assertIn("release.child_cleanup_invalid", {error["code"] for error in errors})


class RuntimeInspectionTest(unittest.TestCase):
    def test_requires_docker_kind_and_kubectl_metadata(self) -> None:
        options = kubernetes_options(pathlib.Path("/tmp/kubernetes-release"))
        completed = [
            subprocess.CompletedProcess([], 0, "29.4.0\n", ""),
            subprocess.CompletedProcess([], 0, "linux/arm64\n", ""),
            subprocess.CompletedProcess([], 0, "kind v0.30.0 go1.24.0 darwin/arm64\n", ""),
            subprocess.CompletedProcess(
                [],
                0,
                json.dumps({"clientVersion": {"gitVersion": "v1.33.1"}}),
                "",
            ),
        ]

        with mock.patch.object(gate, "_run_metadata_command", side_effect=completed):
            evidence = gate.inspect_kubernetes_runtime(options)

        self.assertEqual(evidence["docker"]["serverVersion"], "29.4.0")
        self.assertEqual(evidence["kind"]["nodeImage"], "kindest/node:v1.33.1")
        self.assertEqual(evidence["kubectl"]["clientVersion"], "v1.33.1")

    def test_missing_kind_binary_is_a_stable_preflight_error(self) -> None:
        options = kubernetes_options(pathlib.Path("/tmp/kubernetes-release"))

        with mock.patch.object(gate.subprocess, "run", side_effect=FileNotFoundError):
            with self.assertRaises(gate.ReleaseGateError) as caught:
                gate._run_metadata_command(
                    options,
                    ["kind", "version"],
                    environment=remote.tool_environment(),
                )

        self.assertEqual(caught.exception.code, "release.kubernetes_runtime_command_failed")
        self.assertEqual(caught.exception.evidence["executable"], "kind")


class AggregateMainTest(unittest.TestCase):
    def test_builds_once_runs_all_owned_clusters_and_cleans_the_shared_image(self) -> None:
        with tempfile.TemporaryDirectory() as directory, mock.patch.dict(
            os.environ,
            {
                "CODEX_KEY": "codex-secret",
                "CLAUDE_TOKEN": "claude-secret",
                "CLAUDE_BASE_URL": "https://claude.example.test",
            },
        ):
            output_dir = pathlib.Path(directory) / "gate"
            options = dataclasses.replace(
                kubernetes_options(output_dir),
                codex_model_environment_name="SYNARA_ACCEPTANCE_CODEX_MODEL",
                claude_model_environment_name="SYNARA_ACCEPTANCE_CLAUDE_MODEL",
            )

            def child_run(**kwargs: Any) -> tuple[dict[str, Any], list[dict[str, Any]]]:
                return (
                    {
                        "provider": kwargs["provider"],
                        "matrix": kwargs["matrix"],
                        "status": "pass",
                        "caseCounts": {"pass": 16, "unsupported": 0, "skipped": 0, "fail": 0},
                        "unsupportedCaseIds": [],
                        "reportSha256": "d" * 64,
                        "workerImageId": WORKER_IMAGE_ID,
                        "source": {"providerCapabilityCatalogSha256": "b" * 64},
                    },
                    [],
                )

            def build_image(
                _options: gate.KubernetesReleaseGateOptions,
                image: gate.GateWorkerImage,
                _git_sha: str,
            ) -> dict[str, Any]:
                return {
                    "name": image.name,
                    "id": WORKER_IMAGE_ID,
                    "status": "completed",
                    "ownershipVerified": True,
                }

            def cleanup_image(
                _options: gate.KubernetesReleaseGateOptions,
                image: gate.GateWorkerImage,
                *,
                expected_image_id: str | None,
            ) -> tuple[dict[str, Any], None]:
                return (
                    {
                        "name": image.name,
                        "expectedImageId": expected_image_id,
                        "presentBeforeCleanup": True,
                        "ownershipVerified": True,
                        "removed": True,
                        "broadCleanupUsed": False,
                    },
                    None,
                )

            with (
                mock.patch.object(gate, "parse_args", return_value=options),
                mock.patch.object(
                    common,
                    "repository_state",
                    return_value={"gitSha": "a" * 40, "worktreeDirty": False},
                ),
                mock.patch.object(
                    gate,
                    "inspect_kubernetes_runtime",
                    return_value={
                        "docker": {"serverVersion": "29.4.0"},
                        "kind": {"version": "kind v0.30.0"},
                        "kubectl": {"clientVersion": "v1.33.1"},
                    },
                ),
                mock.patch.object(remote, "build_gate_worker_image", side_effect=build_image) as build,
                mock.patch.object(remote, "cleanup_gate_worker_image", side_effect=cleanup_image) as cleanup,
                mock.patch.object(common, "run_child_report", side_effect=child_run) as run_child,
                contextlib.redirect_stdout(io.StringIO()),
            ):
                exit_code = gate.main([])

            report = json.loads((output_dir / gate.JSON_REPORT_NAME).read_text(encoding="utf-8"))

        self.assertEqual(exit_code, 0)
        self.assertEqual(build.call_count, 1)
        self.assertEqual(run_child.call_count, 6)
        self.assertEqual(cleanup.call_count, 1)
        self.assertTrue(all(call.kwargs["capture_process_output"] for call in run_child.call_args_list))
        self.assertTrue(
            all(
                {
                    "CODEX_KEY",
                    "CLAUDE_TOKEN",
                    "CLAUDE_BASE_URL",
                    "SYNARA_ACCEPTANCE_CODEX_MODEL",
                    "SYNARA_ACCEPTANCE_CLAUDE_MODEL",
                }.issubset(set(call.kwargs["forbidden_output_tokens"]))
                for call in run_child.call_args_list
            )
        )
        commands = [call.kwargs["command"] for call in run_child.call_args_list]
        image_names = {
            command[command.index("--kubernetes-worker-image") + 1] for command in commands
        }
        self.assertEqual(len(image_names), 1)
        self.assertTrue(all("--kubernetes-skip-worker-build" in command for command in commands))
        self.assertEqual(
            sum(command[command.index("--suite") + 1] == "real-provider-load" for command in commands),
            len(common.PROVIDERS),
        )
        self.assertEqual(report["status"], "pass")
        self.assertEqual(report["target"], "kubernetes")
        self.assertEqual(report["coverage"]["completedRuns"], 6)
        self.assertEqual(report["coverage"]["matrices"], list(gate.REMOTE_GATE_MATRICES))
        self.assertEqual(report["coverage"]["loadCases"], [acceptance.REAL_PROVIDER_LOAD_CASE_ID])
        self.assertTrue(report["workerImage"]["sharedAcrossRuns"])
        self.assertTrue(report["workerImage"]["cleanup"]["removed"])
        self.assertFalse(report["security"]["credentialEnvironmentNamesPersisted"])

    def test_scan_process_output_rejects_stderr_only_model_environment_name(self) -> None:
        evidence = common.scan_process_output(
            "",
            "stderr leaked SYNARA_ACCEPTANCE_CLAUDE_MODEL\n",
            redactor=acceptance.SecretRedactor(),
            forbidden_tokens=("SYNARA_ACCEPTANCE_CLAUDE_MODEL",),
        )

        self.assertEqual(evidence["findings"], [{"kind": "forbidden-environment-name"}])


if __name__ == "__main__":
    unittest.main()
