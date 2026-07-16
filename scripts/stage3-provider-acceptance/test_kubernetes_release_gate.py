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
    )


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
    source = gate.credential_source(options, provider)
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
    cases = [
        {"id": case_id, "status": status}
        for case_id, status in sorted(case_statuses.items())
    ]
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
        "mode": "real-provider-smoke",
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
            "restartControlPlane": True,
            "keepState": False,
            "runnerCommand": {"executable": "provider-host"},
            "kubernetes": {
                "workerImage": worker_image_name,
                "skipWorkerBuild": True,
            },
            "realProvider": {
                "requestedCases": requested_product,
                "requestedFailureCases": requested_failure,
                "ambientAuthentication": False,
                "controlledProductCredential": True,
                "controlledProductCredentialField": source.field,
                "productCredentialEnvironmentNamePersisted": False,
                "controlledBaseUrl": source.base_url_environment_name is not None,
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
                    "--claude-credential-env",
                    "CLAUDE_TOKEN",
                    "--claude-credential-field",
                    "authToken",
                    "--claude-base-url-env",
                    "CLAUDE_BASE_URL",
                    "--output-dir",
                    directory,
                ]
            )

        encoded = json.dumps(dataclasses.asdict(options), default=str)
        self.assertEqual(options.claude_credential.field, "authToken")
        self.assertEqual(options.kind_node_image, "kindest/node:v1.33.1")
        self.assertNotIn("codex-secret-value", encoded)
        self.assertNotIn("claude-secret-value", encoded)
        self.assertNotIn("https://claude.example.test", encoded)

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
        failure = gate.child_command(
            options,
            "claudeAgent",
            "failure",
            options.output_dir / "claudeAgent/failure",
            WORKER_IMAGE_NAME,
        )

        self.assertIn("--real-provider-matrix", product)
        self.assertNotIn("--real-provider-failure-matrix", product)
        self.assertIn("--real-provider-failure-matrix", failure)
        self.assertIn("--kubernetes-skip-worker-build", product)
        self.assertIn("--kubernetes-worker-image", product)
        self.assertIn(WORKER_IMAGE_NAME, product)
        self.assertIn("kindest/node:v1.33.1", product)
        self.assertIn("CODEX_KEY", product)
        self.assertIn("CLAUDE_TOKEN", failure)
        self.assertIn("CLAUDE_BASE_URL", failure)

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

    def test_accepts_nested_kubernetes_worker_image_and_exact_cleanup(self) -> None:
        options = kubernetes_options(pathlib.Path("/tmp/kubernetes-release"))
        policy = gate.child_policy(options, WORKER_IMAGE_NAME)
        for provider in common.PROVIDERS:
            for matrix in common.MATRICES:
                with self.subTest(provider=provider, matrix=matrix):
                    errors = common.validate_child_report(
                        sample_child_report(options, provider, matrix),
                        provider=provider,
                        matrix=matrix,
                        expected_git_sha="a" * 40,
                        policy=policy,
                    )
                    self.assertEqual(errors, [])

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
    def test_builds_once_runs_four_owned_clusters_and_cleans_the_shared_image(self) -> None:
        with tempfile.TemporaryDirectory() as directory, mock.patch.dict(
            os.environ,
            {
                "CODEX_KEY": "codex-secret",
                "CLAUDE_TOKEN": "claude-secret",
                "CLAUDE_BASE_URL": "https://claude.example.test",
            },
        ):
            output_dir = pathlib.Path(directory) / "gate"
            options = kubernetes_options(output_dir)

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
        self.assertEqual(run_child.call_count, 4)
        self.assertEqual(cleanup.call_count, 1)
        commands = [call.kwargs["command"] for call in run_child.call_args_list]
        image_names = {
            command[command.index("--kubernetes-worker-image") + 1] for command in commands
        }
        self.assertEqual(len(image_names), 1)
        self.assertTrue(all("--kubernetes-skip-worker-build" in command for command in commands))
        self.assertEqual(report["status"], "pass")
        self.assertEqual(report["target"], "kubernetes")
        self.assertTrue(report["workerImage"]["sharedAcrossRuns"])
        self.assertTrue(report["workerImage"]["cleanup"]["removed"])
        self.assertFalse(report["security"]["credentialEnvironmentNamesPersisted"])


if __name__ == "__main__":
    unittest.main()
