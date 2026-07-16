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
from collections.abc import Mapping
from typing import Any
from unittest import mock

import acceptance_runner as acceptance
import release_gate_common as common
import ssh_release_gate as gate


REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
EXPECTED_VERSIONS = {"codex": "0.144.1", "claudeAgent": "2.1.197"}


def ssh_options(output_dir: pathlib.Path) -> gate.SSHReleaseGateOptions:
    return gate.SSHReleaseGateOptions(
        repo_root=REPO_ROOT,
        output_dir=output_dir,
        product_timeout_seconds=3600.0,
        failure_timeout_seconds=2400.0,
        codex_credential=gate.CredentialSource("CODEX_KEY", "apiKey", None),
        claude_credential=gate.CredentialSource(
            "CLAUDE_TOKEN",
            "authToken",
            "CLAUDE_BASE_URL",
        ),
        ssh_orbctl_bin="orbctl",
        ssh_machine_arch="arm64",
        ssh_machine_image="ubuntu:24.04",
        ssh_node_version="24.13.1",
    )


def sample_child_report(
    options: gate.SSHReleaseGateOptions,
    provider: str,
    matrix: str,
    *,
    machine_name: str = "synara-stage3-0123456789ab",
    git_sha: str = "a" * 40,
    catalog_hash: str = "b" * 64,
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
    target_provision = next(
        case for case in cases if case["id"] == "runtime.target-provision"
    )
    target_provision["evidence"] = {
        "driverEvidence": {
            "machineName": machine_name,
            "binarySha256": "c" * 64,
            "hostKeyMismatch": {
                "rejected": True,
                "errorCode": "ssh_connection_failed",
            },
            "service": {
                "activeState": "active",
                "subState": "running",
            },
            "controlPlaneTransport": {
                "mode": "reverse-ssh-loopback",
                "readsUserSSHConfiguration": False,
            },
            "controlPlaneCredentialLifecycle": acceptance.SSH_CREDENTIAL_LIFECYCLE,
        }
    }
    cases.extend(
        [
            {
                "id": "environment.target-prepare",
                "status": "pass",
                "evidence": {
                    "ssh": {
                        "agentd": {
                            "goos": "linux",
                            "goarch": options.ssh_machine_arch,
                            "sha256": "c" * 64,
                        },
                        "providerHost": {
                            "remotePath": acceptance.SSH_REMOTE_PROVIDER_HOST_PATH,
                            "sha256": "d" * 64,
                            "runtime": "real-provider",
                        },
                        "providerTools": {
                            "packageSha256": common.file_sha256(
                                REPO_ROOT / "deploy/worker/provider-tools/package.json"
                            ),
                            "lockSha256": common.file_sha256(
                                REPO_ROOT / "deploy/worker/provider-tools/package-lock.json"
                            ),
                            "remoteRoot": acceptance.SSH_REMOTE_PROVIDER_TOOLS_ROOT,
                        },
                        "credentialSource": "generated under isolated acceptance state",
                        "algorithm": "ssh-ed25519",
                        "localPrivateKeyPlaintextDeletedAfterProvision": True,
                        "controlPlaneCredentialLifecycle": acceptance.SSH_CREDENTIAL_LIFECYCLE,
                        "machineName": machine_name,
                        "ownedMachine": True,
                        "machineImage": options.ssh_machine_image,
                        "machineArch": options.ssh_machine_arch,
                        "nodeVersion": options.ssh_node_version,
                        "sshd": "active",
                        "initSystem": "systemd",
                        "providerRuntime": {
                            "kind": "real-provider",
                            "providerHost": {
                                "command": acceptance.SSH_PROVIDER_HOST_COMMAND_PATH,
                                "remotePath": acceptance.SSH_REMOTE_PROVIDER_HOST_PATH,
                                "sha256": "d" * 64,
                            },
                            "providerTools": {
                                "remoteRoot": acceptance.SSH_REMOTE_PROVIDER_TOOLS_ROOT,
                                "lockedInstall": True,
                                "codex": {
                                    "version": EXPECTED_VERSIONS["codex"],
                                    "versionOutput": "codex-cli 0.144.1",
                                },
                                "claudeAgent": {
                                    "version": EXPECTED_VERSIONS["claudeAgent"],
                                    "versionOutput": "2.1.197 (Claude Code)",
                                },
                            },
                        },
                    }
                },
            },
            {
                "id": "environment.cleanup",
                "status": "pass",
                "evidence": {
                    "machineName": machine_name,
                    "machineRemoved": True,
                    "machinePreservedByRequest": False,
                    "productRevokeRequested": True,
                    "machineLifecycleCompleted": True,
                    "localKeyMaterialRemoved": True,
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
        "target": "ssh",
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
            "ssh": {
                "runtime": "owned-disposable-orbstack",
                "orbctlBinary": options.ssh_orbctl_bin,
                "machineName": "generated-per-run",
                "machineArch": options.ssh_machine_arch,
                "machineImage": options.ssh_machine_image,
                "nodeVersion": options.ssh_node_version,
                "localPrivateKeyPlaintextDeletedAfterProvision": True,
                "controlPlaneCredentialLifecycle": acceptance.SSH_CREDENTIAL_LIFECYCLE,
                "readsUserSSHConfiguration": False,
                "controlPlaneTransport": {
                    "mode": "reverse-ssh-loopback",
                    "vmListenHost": acceptance.SSH_RELAY_LOOPBACK_HOST,
                },
                "runtimeBuild": "real-provider-host-plus-locked-tools-per-run",
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
        self.assertEqual(options.ssh_machine_image, "ubuntu:24.04")
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


class ChildCommandAndValidationTest(unittest.TestCase):
    def test_child_commands_use_distinct_owned_machines_and_controlled_environments(self) -> None:
        options = ssh_options(pathlib.Path("/tmp/ssh-release"))
        product = gate.child_command(
            options,
            "codex",
            "product",
            options.output_dir / "codex/product",
        )
        failure = gate.child_command(
            options,
            "claudeAgent",
            "failure",
            options.output_dir / "claudeAgent/failure",
        )

        self.assertIn("--real-provider-matrix", product)
        self.assertIn("--real-provider-failure-matrix", failure)
        self.assertIn('["/usr/local/bin/provider-host"]', product)
        self.assertNotIn("--ssh-machine-name", product)
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
            codex = gate.child_environment(options, "codex")
            claude = gate.child_environment(options, "claudeAgent")

        self.assertEqual(codex["CODEX_KEY"], "codex-secret")
        self.assertNotIn("CLAUDE_TOKEN", codex)
        self.assertEqual(claude["CLAUDE_TOKEN"], "claude-secret")
        self.assertNotIn("CODEX_KEY", claude)
        self.assertNotIn("DATABASE_URL", codex)
        self.assertNotIn("DATABASE_URL", claude)

    def test_accepts_complete_locked_ssh_product_and_failure_reports(self) -> None:
        options = ssh_options(pathlib.Path("/tmp/ssh-release"))
        policy = gate.child_policy(options)

        for provider in common.PROVIDERS:
            for matrix in common.MATRICES:
                report = sample_child_report(options, provider, matrix)
                errors = common.validate_child_report(
                    report,
                    provider=provider,
                    matrix=matrix,
                    expected_git_sha="a" * 40,
                    policy=policy,
                )
                runtime_errors, runtime = gate.validate_ssh_child_runtime(
                    report,
                    provider=provider,
                    matrix=matrix,
                    options=options,
                    expected_versions=EXPECTED_VERSIONS,
                )
                self.assertEqual(errors, [])
                self.assertEqual(runtime_errors, [])
                self.assertEqual(runtime["providerHostSha256"], "d" * 64)

    def test_rejects_fixture_runtime_or_incomplete_cleanup(self) -> None:
        options = ssh_options(pathlib.Path("/tmp/ssh-release"))
        policy = gate.child_policy(options)
        report = sample_child_report(options, "codex", "failure")
        target_prepare = next(
            case for case in report["cases"] if case["id"] == "environment.target-prepare"
        )
        target_prepare["evidence"]["ssh"]["providerRuntime"]["kind"] = "deterministic-fixture"
        cleanup = next(case for case in report["cases"] if case["id"] == "environment.cleanup")
        cleanup["evidence"]["stateRemoved"] = False

        common_errors = common.validate_child_report(
            report,
            provider="codex",
            matrix="failure",
            expected_git_sha="a" * 40,
            policy=policy,
        )
        runtime_errors, runtime = gate.validate_ssh_child_runtime(
            report,
            provider="codex",
            matrix="failure",
            options=options,
            expected_versions=EXPECTED_VERSIONS,
        )

        self.assertIn("release.child_cleanup_invalid", {error["code"] for error in common_errors})
        self.assertIn("release.child_ssh_runtime_invalid", {error["code"] for error in runtime_errors})
        self.assertIsNone(runtime)


class RuntimeInspectionTest(unittest.TestCase):
    def test_missing_provider_tools_lock_is_a_stable_preflight_error(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            repo_root = pathlib.Path(directory)
            provider_tools = repo_root / "deploy/worker/provider-tools"
            provider_tools.mkdir(parents=True)
            (provider_tools / "package.json").write_text(
                json.dumps(
                    {
                        "dependencies": {
                            "@openai/codex": EXPECTED_VERSIONS["codex"],
                            "@anthropic-ai/claude-code": EXPECTED_VERSIONS[
                                "claudeAgent"
                            ],
                        }
                    }
                ),
                encoding="utf-8",
            )

            with self.assertRaises(gate.ReleaseGateError) as caught:
                gate.expected_provider_tool_versions(repo_root)

        self.assertEqual(
            caught.exception.code,
            "release.ssh_provider_tools_lock_invalid",
        )

    def test_requires_orbctl_and_host_build_tools(self) -> None:
        options = ssh_options(pathlib.Path("/tmp/ssh-release"))
        responses = {
            "orbctl version": subprocess.CompletedProcess([], 0, "Version: 2.2.1\n", ""),
            "orbctl list --format json": subprocess.CompletedProcess([], 0, "[]\n", ""),
            "go version": subprocess.CompletedProcess([], 0, "go version go1.26.5 darwin/arm64\n", ""),
            "bun --version": subprocess.CompletedProcess([], 0, "1.3.14\n", ""),
            "ssh -V": subprocess.CompletedProcess([], 0, "OpenSSH_10.2p1\n", ""),
        }

        def run(_options: Any, command: list[str]) -> subprocess.CompletedProcess[str]:
            return responses[" ".join(command)]

        with (
            mock.patch.object(gate, "_run_metadata_command", side_effect=run),
            mock.patch.object(gate.shutil, "which", return_value="/usr/bin/ssh-keygen"),
        ):
            runtime = gate.inspect_ssh_runtime(options)

        self.assertEqual(runtime["orbctl"]["existingMachineCount"], 0)
        self.assertEqual(runtime["remoteRuntime"]["providerToolVersions"], EXPECTED_VERSIONS)

    def test_missing_orbctl_is_a_stable_preflight_error(self) -> None:
        options = ssh_options(pathlib.Path("/tmp/ssh-release"))

        with mock.patch.object(gate.subprocess, "run", side_effect=FileNotFoundError):
            with self.assertRaises(gate.ReleaseGateError) as caught:
                gate._run_metadata_command(options, ["orbctl", "version"])

        self.assertEqual(caught.exception.code, "release.ssh_runtime_command_failed")
        self.assertEqual(caught.exception.evidence["executable"], "orbctl")


class AggregateGateTest(unittest.TestCase):
    def test_emits_pass_for_four_unique_machines_and_one_runtime_consensus(self) -> None:
        with tempfile.TemporaryDirectory() as directory, mock.patch.dict(
            os.environ,
            {
                "CODEX_KEY": "codex-secret",
                "CLAUDE_TOKEN": "claude-secret",
                "CLAUDE_BASE_URL": "https://claude.example.test",
            },
        ):
            output_dir = pathlib.Path(directory) / "gate"
            options = ssh_options(output_dir)
            machine_index = 0

            def child_runner(**kwargs: Any) -> tuple[dict[str, Any], list[dict[str, Any]]]:
                nonlocal machine_index
                machine_index += 1
                return (
                    {
                        "provider": kwargs["provider"],
                        "matrix": kwargs["matrix"],
                        "status": "pass",
                        "caseCounts": {"pass": 16, "unsupported": 0, "skipped": 0, "fail": 0},
                        "unsupportedCaseIds": [],
                        "reportSha256": "1" * 64,
                        "markdownSha256": "2" * 64,
                        "source": {"providerCapabilityCatalogSha256": "b" * 64},
                        "sshRuntime": {
                            "machineName": f"synara-stage3-{machine_index:012x}",
                            "agentdSha256": "c" * 64,
                            "providerHostSha256": "d" * 64,
                            "codexVersion": EXPECTED_VERSIONS["codex"],
                            "claudeVersion": EXPECTED_VERSIONS["claudeAgent"],
                        },
                    },
                    [],
                )

            with contextlib.redirect_stdout(io.StringIO()):
                exit_code = gate.run_ssh_release_gate(
                    options,
                    repository_state=lambda _root: {
                        "gitSha": "a" * 40,
                        "worktreeDirty": False,
                    },
                    runtime_inspector=lambda _options: {
                        "orbctl": {"version": "2.2.1"}
                    },
                    child_runner=child_runner,
                )

            report = json.loads((output_dir / gate.JSON_REPORT_NAME).read_text(encoding="utf-8"))

        self.assertEqual(exit_code, 0)
        self.assertEqual(report["status"], "pass")
        self.assertEqual(report["coverage"]["completedRuns"], 4)
        self.assertEqual(report["isolation"]["distinctMachineCount"], 4)
        self.assertFalse(report["isolation"]["sharedSSHPrivateKey"])
        self.assertEqual(report["runtimeArtifacts"]["providerHostSha256"], "d" * 64)
        self.assertFalse(report["security"]["credentialEnvironmentNamesPersisted"])

    def test_rejects_reused_machine_even_when_child_statuses_pass(self) -> None:
        runs = [
            {
                "sshRuntime": {
                    "machineName": "synara-stage3-0123456789ab",
                    "agentdSha256": "c" * 64,
                    "providerHostSha256": "d" * 64,
                    "codexVersion": EXPECTED_VERSIONS["codex"],
                    "claudeVersion": EXPECTED_VERSIONS["claudeAgent"],
                }
            }
            for _ in range(4)
        ]

        errors = gate._runtime_consensus_errors(runs)

        self.assertIn("release.ssh_machine_isolation_invalid", {error["code"] for error in errors})


if __name__ == "__main__":
    unittest.main()
