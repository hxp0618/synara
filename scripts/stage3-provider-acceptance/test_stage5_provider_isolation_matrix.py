from __future__ import annotations

import contextlib
import dataclasses
import io
import json
import os
import pathlib
import tempfile
import unittest
from typing import Any
from unittest import mock

import acceptance_runner as acceptance
import controlled_remote_release_gate as remote
import stage5_provider_isolation_matrix as matrix


IMMUTABLE_IMAGE = "registry.example.test/synara/worker@sha256:" + "a" * 64


def provider_configuration(provider: str) -> matrix.ProviderConfiguration:
    return matrix.ProviderConfiguration(
        provider=provider,
        credential=remote.CredentialSource(
            environment_name="CODEX_KEY" if provider == "codex" else "CLAUDE_KEY",
            field="apiKey" if provider == "codex" else "authToken",
            base_url_environment_name=None,
        ),
        model="gpt-5.6-sol" if provider == "codex" else "claude-sonnet-4-6",
    )


def matrix_options(output_dir: pathlib.Path) -> matrix.MatrixOptions:
    return matrix.MatrixOptions(
        repo_root=pathlib.Path(__file__).resolve().parents[2],
        output_dir=output_dir,
        kubectl_bin="/opt/synara/bin/kubectl-v1.36.1",
        kubernetes_context="managed-production",
        kubernetes_kubeconfig=None,
        kubernetes_api_server=None,
        kubernetes_tls_server_name=None,
        kubernetes_control_plane_host="control-plane.internal",
        kubernetes_control_plane_port=58091,
        allow_control_plane_node=False,
        node_selector="synara.dev/pool=provider",
        runtime_class=None,
        worker_image=IMMUTABLE_IMAGE,
        runner_command=("/usr/local/bin/provider-host",),
        timeout_per_cell_seconds=1800.0,
        skip_build=False,
        control_plane_binary=None,
        providers=(
            provider_configuration("codex"),
            provider_configuration("claudeAgent"),
        ),
    )


def node_payload(
    name: str,
    *,
    ready: bool = True,
    unschedulable: bool = False,
    control_plane: bool = False,
    deleting: bool = False,
) -> dict[str, Any]:
    return {
        "metadata": {
            "name": name,
            "labels": (
                {"node-role.kubernetes.io/control-plane": ""}
                if control_plane
                else {"synara.dev/pool": "provider"}
            ),
            **({"deletionTimestamp": "2026-07-29T00:00:00Z"} if deleting else {}),
        },
        "spec": {"unschedulable": unschedulable, "taints": []},
        "status": {
            "conditions": [
                {"type": "Ready", "status": "True" if ready else "False"},
            ]
        },
    }


def child_report(
    options: matrix.MatrixOptions,
    provider: str,
    node_name: str,
    *,
    git_sha: str = "b" * 40,
) -> dict[str, Any]:
    cases = []
    for case in matrix.stage5_cases(options):
        evidence: dict[str, Any] = {
            "nodeName": node_name,
            "nodePinned": True,
            "runtimeClassName": options.runtime_class,
            "runtimeIsolationProfile": (
                "gvisor-sandboxed-v1" if options.runtime_class is not None else None
            ),
        }
        if case == "metadata-egress":
            evidence.update(
                {
                    "command": {
                        "runtime": "agentd-isolation-verifier",
                        "responseBodiesOrHeadersEmitted": False,
                    },
                    "terminal": {
                        "blockedSentinelMatched": False,
                        "blockedByExactCommandZeroExit": True,
                        "zeroExitEmptyOutputAccepted": True,
                        "outputBytes": 0,
                        "outputEventCount": 0,
                        "reachableSentinelPersisted": False,
                        "probeErrorSentinelPersisted": False,
                        "responseBodiesOrHeadersPersisted": False,
                        "completion": {
                            "totalBytes": 0,
                            "previewBytes": 0,
                            "segmentCount": 0,
                            "truncated": False,
                            "exitCode": 0,
                        },
                    },
                    "approval": {
                        "supportMode": (
                            "native-approval-required-fresh-approval"
                            if provider == "codex"
                            else "host-observed-full-access-fresh-approval"
                        ),
                        "freshApprovalProved": True,
                        "sessionWideGrantAvailable": False,
                        "commandExecutionAuthorized": True,
                        "decision": "accept",
                        "interactionAssessment": matrix._fresh_assessment(
                            ("network-egress",)
                        ),
                        "requestAssessment": matrix._fresh_assessment(
                            ("network-egress",)
                        ),
                        "resolutionAssessment": matrix._fresh_assessment(
                            ("network-egress",)
                        ),
                    },
                }
            )
        elif case == "credential-scope":
            assessment = matrix._fresh_assessment(("credential-access",))
            evidence.update(
                {
                    "command": {
                        "runtime": "agentd-isolation-verifier",
                        "environmentValuesPersisted": False,
                        "credentialFileContentsRead": False,
                        "gitConfigContentEmitted": False,
                        "credentialValuesEmitted": False,
                        "intendedProviderBrokerCredentialExcluded": True,
                    },
                    "terminal": {
                        "credentialsAbsentSentinelMatched": False,
                        "credentialsAbsentByExactCommandZeroExit": True,
                        "zeroExitEmptyOutputAccepted": True,
                        "outputBytes": 0,
                        "outputEventCount": 0,
                        "credentialPresentSentinelPersisted": False,
                        "probeErrorSentinelPersisted": False,
                        "ambientCredentialValuesPersistedInProbeOutput": False,
                        "completion": {
                            "totalBytes": 0,
                            "previewBytes": 0,
                            "segmentCount": 0,
                            "truncated": False,
                            "exitCode": 0,
                        },
                    },
                    "approval": {
                        "freshApprovalProved": True,
                        "sessionWideGrantAvailable": False,
                        "commandExecutionAuthorized": True,
                        "decision": "accept",
                        "interactionAssessment": assessment,
                        "requestAssessment": assessment,
                        "resolutionAssessment": assessment,
                    },
                }
            )
        elif case == "malicious-issue-denial":
            assessment = matrix._fresh_assessment(
                ("credential-access", "protected-branch-publish")
            )
            evidence.update(
                {
                    "content": {
                        "shape": "attacker-authored-issue-replay",
                        "controlPlaneSource": "native",
                        "actualWebhookProvenanceProved": False,
                    },
                    "command": {
                        "safetyFuse": "leading-false-short-circuit",
                        "protectedBranchPublishAttempted": False,
                        "credentialEnvironmentReadAttempted": False,
                        "commandExecuted": False,
                    },
                    "approval": {
                        "freshApprovalProved": True,
                        "sessionWideGrantAvailable": False,
                        "commandExecutionAuthorized": False,
                        "decision": "decline",
                        "interactionAssessment": assessment,
                        "requestAssessment": assessment,
                        "resolutionAssessment": assessment,
                    },
                    "denial": {
                        "providerLifecycleMode": "bounded-declined-item",
                        "commandItemEventCount": 2,
                        "terminalLifecycleEventCount": 2,
                        "commandOutputEventCount": 0,
                        "artifactReadyEventCount": 0,
                        "commandNeverStarted": False,
                        "terminalNeverStarted": False,
                        "commandExecuted": False,
                        "declinedBeforeExecution": True,
                        "commandOutputPersisted": False,
                        "artifactPersisted": False,
                    },
                }
            )
        else:
            evidence.update(
                {
                    "command": {
                        "runtime": "node-bounded-gvisor-compat-v1",
                        "sha256": "c" * 64,
                        "outputContainsRawProbeErrors": False,
                    },
                    "compatibility": {
                        "schemaVersion": 1,
                        "runtime": "gvisor",
                        "tools": {
                            tool: True
                            for tool in acceptance.GVISOR_COMPATIBILITY_REQUIRED_TOOLS
                        },
                        "probes": {
                            probe: True
                            for probe in (
                                "gvisorKernel",
                                "git",
                                "node",
                                "bun",
                                "packageManagers",
                                "go",
                                "rust",
                                "java",
                                "python",
                                "pty",
                                "fileMetadata",
                                "fileWatch",
                                "signal",
                                "loopbackTcp",
                            )
                        },
                        "durationsMs": {
                            label: index
                            for index, label in enumerate(
                                acceptance.GVISOR_COMPATIBILITY_REQUIRED_DURATION_LABELS
                            )
                        },
                        "maxRssKiB": 1024,
                    },
                }
            )
        cases.append(
            {
                "id": acceptance.REAL_PROVIDER_CASE_METADATA[case]["id"],
                "status": "pass",
                "evidence": evidence,
            }
        )
    cases.extend(
        [
            {"id": "environment.cleanup", "status": "pass"},
            {"id": "security.output-secret-scan", "status": "pass"},
        ]
    )
    return {
        "schemaVersion": acceptance.SCHEMA_VERSION,
        "mode": "real-provider-smoke",
        "target": "kubernetes",
        "provider": provider,
        "status": "pass",
        "source": {"gitSha": git_sha, "worktreeDirty": False},
        "configuration": {
            "realProvider": {"requestedCases": list(matrix.stage5_cases(options))},
            "kubernetes": {
                "context": options.kubernetes_context,
                "nodeName": node_name,
                "workerImage": options.worker_image,
                "skipWorkerBuild": True,
                "allowNondisposable": True,
                "runtimeClassName": options.runtime_class,
                "kubectlBinary": options.kubectl_bin,
            },
        },
        "cases": cases,
    }


class ParseArgsTest(unittest.TestCase):
    def base_arguments(self, output_dir: str) -> list[str]:
        return [
            "--kubernetes-context",
            "managed-production",
            "--node-selector",
            "synara.dev/pool=provider",
            "--kubernetes-worker-image",
            IMMUTABLE_IMAGE,
            "--runner-command-json",
            '["/usr/local/bin/provider-host"]',
            "--kubernetes-allow-nondisposable",
            "--kubernetes-control-plane-port",
            "58091",
            "--codex-credential-env",
            "CODEX_KEY",
            "--claude-credential-env",
            "CLAUDE_KEY",
            "--claude-credential-field",
            "authToken",
            "--output-dir",
            output_dir,
        ]

    def test_parses_complete_controlled_matrix_without_persisting_values(self) -> None:
        with tempfile.TemporaryDirectory() as directory, mock.patch.dict(
            os.environ,
            {"CODEX_KEY": "codex-secret", "CLAUDE_KEY": "claude-secret"},
            clear=True,
        ):
            options = matrix.parse_args(self.base_arguments(directory))

        encoded = json.dumps(dataclasses.asdict(options), default=str)
        self.assertEqual(options.worker_image, IMMUTABLE_IMAGE)
        self.assertEqual(options.kubernetes_control_plane_port, 58091)
        self.assertEqual([item.provider for item in options.providers], list(matrix.PROVIDERS))
        self.assertNotIn("codex-secret", encoded)
        self.assertNotIn("claude-secret", encoded)

    def test_rejects_mutable_image_and_missing_existing_cluster_authorization(self) -> None:
        with tempfile.TemporaryDirectory() as directory, mock.patch.dict(
            os.environ,
            {"CODEX_KEY": "codex-secret", "CLAUDE_KEY": "claude-secret"},
            clear=True,
        ):
            arguments = self.base_arguments(directory)
            authorization_index = arguments.index("--kubernetes-allow-nondisposable")
            without_authorization = arguments[:authorization_index] + arguments[authorization_index + 1 :]
            mutable = list(arguments)
            image_index = mutable.index("--kubernetes-worker-image") + 1
            mutable[image_index] = "registry.example.test/synara/worker:latest"
            for invalid in (without_authorization, mutable):
                with (
                    self.subTest(arguments=invalid),
                    contextlib.redirect_stderr(io.StringIO()),
                    self.assertRaises(SystemExit) as caught,
                ):
                    matrix.parse_args(invalid)
                self.assertEqual(caught.exception.code, 2)

    def test_api_override_and_inventory_use_the_same_tls_boundary(self) -> None:
        with tempfile.TemporaryDirectory() as directory, mock.patch.dict(
            os.environ,
            {"CODEX_KEY": "codex-secret", "CLAUDE_KEY": "claude-secret"},
            clear=True,
        ):
            options = matrix.parse_args(
                [
                    *self.base_arguments(directory),
                    "--kubernetes-api-server",
                    "https://127.0.0.1:6443",
                    "--kubernetes-tls-server-name",
                    "cluster.example.test",
                ]
            )

        command = matrix._kubectl_command(options, ["get", "nodes"])
        self.assertIn("--server", command)
        self.assertIn("https://127.0.0.1:6443", command)
        self.assertIn("--tls-server-name", command)
        self.assertIn("cluster.example.test", command)


class NodeInventoryTest(unittest.TestCase):
    def test_keeps_only_ready_non_cordoned_worker_nodes_in_canonical_order(self) -> None:
        inventory = matrix.schedulable_worker_node_inventory(
            {
                "items": [
                    node_payload("worker-b"),
                    node_payload("worker-a"),
                    node_payload("cordoned", unschedulable=True),
                    node_payload("not-ready", ready=False),
                    node_payload("control-plane", control_plane=True),
                    node_payload("deleting", deleting=True),
                ]
            }
        )

        self.assertEqual([node["name"] for node in inventory], ["worker-a", "worker-b"])

    def test_fails_closed_on_empty_or_duplicate_inventory(self) -> None:
        for payload, code in (
            ({"items": [node_payload("not-ready", ready=False)]}, "stage5.matrix.no_schedulable_nodes"),
            (
                {"items": [node_payload("worker-a"), node_payload("worker-a")]},
                "stage5.matrix.node_inventory_invalid",
            ),
        ):
            with self.subTest(code=code), self.assertRaises(matrix.MatrixError) as caught:
                matrix.schedulable_worker_node_inventory(payload)
            self.assertEqual(caught.exception.code, code)

    def test_includes_control_plane_node_only_with_explicit_authorization(self) -> None:
        payload = {"items": [node_payload("single-node", control_plane=True)]}

        with self.assertRaises(matrix.MatrixError) as caught:
            matrix.schedulable_worker_node_inventory(payload)
        self.assertEqual(caught.exception.code, "stage5.matrix.no_schedulable_nodes")

        inventory = matrix.schedulable_worker_node_inventory(
            payload,
            allow_control_plane_node=True,
        )
        self.assertEqual([node["name"] for node in inventory], ["single-node"])
        self.assertTrue(inventory[0]["controlPlane"])


class ChildBoundaryTest(unittest.TestCase):
    def test_child_command_selects_every_stage5_case_on_one_exact_node(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            options = matrix_options(pathlib.Path(directory))
            command = matrix.child_command(
                options,
                "claudeAgent",
                "worker-a",
                pathlib.Path(directory) / "cell",
            )

        selected = [
            command[index + 1]
            for index, value in enumerate(command)
            if value == "--real-provider-case"
        ]
        self.assertEqual(selected, list(matrix.STAGE5_CASES))
        self.assertIn("worker-a", command)
        self.assertIn(IMMUTABLE_IMAGE, command)
        self.assertIn("CLAUDE_KEY", command)
        self.assertIn("--kubernetes-control-plane-port", command)
        self.assertIn("58091", command)
        self.assertIn("--kubectl-bin", command)
        self.assertIn("/opt/synara/bin/kubectl-v1.36.1", command)
        self.assertNotIn("claude-secret", command)

    def test_gvisor_matrix_requires_exact_runtime_class_in_command_and_evidence(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            options = dataclasses.replace(
                matrix_options(pathlib.Path(directory)), runtime_class="synara-gvisor"
            )
            command = matrix.child_command(
                options, "codex", "worker-a", pathlib.Path(directory) / "cell"
            )
            report = child_report(options, "codex", "worker-a")

            self.assertIn("--kubernetes-runtime-class", command)
            self.assertIn("synara-gvisor", command)
            self.assertIn(acceptance.REAL_PROVIDER_GVISOR_CASE, command)
            self.assertEqual(
                matrix.validate_child_report(
                    report,
                    options=options,
                    provider="codex",
                    node_name="worker-a",
                    expected_git_sha="b" * 40,
                ),
                [],
            )
            compatibility = matrix._gvisor_compatibility_evidence(report, options)
            self.assertIsNotNone(compatibility)
            summary = matrix._gvisor_compatibility_summary(
                [
                    {"gvisorCompatibility": compatibility},
                    {"gvisorCompatibility": compatibility},
                ],
                options,
            )
            self.assertIsNotNone(summary)
            self.assertTrue(summary["complete"])
            self.assertEqual(summary["maxRssKiB"]["sampleCount"], 2)
            rendered = matrix.markdown_from_report(
                {
                    "status": "pass",
                    "cells": [],
                    "gvisorCompatibilitySummary": summary,
                    "errors": [],
                }
            )
            self.assertIn("## gVisor compatibility distributions", rendered)
            self.assertIn("| total | 2 |", rendered)
            self.assertIn("| maxRss | 2 | 1024 | 1024 | 1024 | KiB |", rendered)
            report["cases"][0]["evidence"]["runtimeClassName"] = "runc"
            errors = matrix.validate_child_report(
                report,
                options=options,
                provider="codex",
                node_name="worker-a",
                expected_git_sha="b" * 40,
            )
        self.assertIn("stage5.matrix.child_case_invalid", {error["code"] for error in errors})

    def test_accepts_only_a_complete_passing_exact_node_child_report(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            options = matrix_options(pathlib.Path(directory))
            report = child_report(options, "codex", "worker-a")

            self.assertEqual(
                matrix.validate_child_report(
                    report,
                    options=options,
                    provider="codex",
                    node_name="worker-a",
                    expected_git_sha="b" * 40,
                ),
                [],
            )

            report["cases"][0]["evidence"]["nodeName"] = "worker-b"
            errors = matrix.validate_child_report(
                report,
                options=options,
                provider="codex",
                node_name="worker-a",
                expected_git_sha="b" * 40,
            )
        self.assertIn("stage5.matrix.child_case_invalid", {error["code"] for error in errors})

    def test_rejects_a_stage5_case_that_is_missing_or_unsupported(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            options = matrix_options(pathlib.Path(directory))
            for status in ("unsupported", None):
                report = child_report(options, "codex", "worker-a")
                case_id = acceptance.REAL_PROVIDER_CASE_METADATA[matrix.STAGE5_CASES[-1]]["id"]
                if status is None:
                    report["cases"] = [
                        case for case in report["cases"] if case.get("id") != case_id
                    ]
                else:
                    next(case for case in report["cases"] if case.get("id") == case_id)[
                        "status"
                    ] = status
                with self.subTest(status=status):
                    errors = matrix.validate_child_report(
                        report,
                        options=options,
                        provider="codex",
                        node_name="worker-a",
                        expected_git_sha="b" * 40,
                    )
                    self.assertIn(
                        "stage5.matrix.child_case_invalid",
                        {error["code"] for error in errors},
                    )

    def test_rejects_a_passing_case_with_weakened_security_evidence(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            options = matrix_options(pathlib.Path(directory))
            report = child_report(options, "codex", "worker-a")
            case_id = acceptance.REAL_PROVIDER_CASE_METADATA[
                "malicious-issue-denial"
            ]["id"]
            malicious = next(case for case in report["cases"] if case.get("id") == case_id)
            malicious["evidence"]["denial"]["commandOutputPersisted"] = True

            errors = matrix.validate_child_report(
                report,
                options=options,
                provider="codex",
                node_name="worker-a",
                expected_git_sha="b" * 40,
            )

        self.assertIn(
            "stage5.matrix.child_case_evidence_invalid",
            {error["code"] for error in errors},
        )

    def test_rejects_legacy_codex_outer_sandbox_only_metadata_evidence(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            options = matrix_options(pathlib.Path(directory))
            report = child_report(options, "codex", "worker-a")
            case_id = acceptance.REAL_PROVIDER_CASE_METADATA["metadata-egress"]["id"]
            metadata = next(case for case in report["cases"] if case.get("id") == case_id)
            metadata["evidence"]["approval"] = {
                "supportMode": "outer-sandbox-only",
                "freshApprovalProved": False,
            }

            errors = matrix.validate_child_report(
                report,
                options=options,
                provider="codex",
                node_name="worker-a",
                expected_git_sha="b" * 40,
            )

        self.assertIn(
            "stage5.matrix.child_case_evidence_invalid",
            {error["code"] for error in errors},
        )


class AggregateMatrixTest(unittest.TestCase):
    def test_runs_every_provider_node_cell_and_requires_stable_inventory(self) -> None:
        with tempfile.TemporaryDirectory() as directory, mock.patch.dict(
            os.environ,
            {"CODEX_KEY": "codex-secret", "CLAUDE_KEY": "claude-secret"},
            clear=True,
        ):
            options = matrix_options(pathlib.Path(directory) / "matrix")
            inventory = [
                {"name": "worker-a", "ready": True},
                {"name": "worker-b", "ready": True},
            ]

            def run_cell(
                _options: matrix.MatrixOptions,
                *,
                provider: str,
                node_name: str,
                expected_git_sha: str,
                redactor: acceptance.SecretRedactor,
            ) -> tuple[dict[str, Any], list[dict[str, Any]]]:
                del _options, expected_git_sha, redactor
                return (
                    {
                        "provider": provider,
                        "nodeName": node_name,
                        "status": "pass",
                        "durationMs": 1,
                    },
                    [],
                )

            with (
                mock.patch.object(
                    matrix.release_gate,
                    "repository_state",
                    return_value={"gitSha": "b" * 40, "worktreeDirty": False},
                ),
                mock.patch.object(matrix, "discover_nodes", side_effect=[inventory, inventory]),
                mock.patch.object(matrix, "run_cell", side_effect=run_cell) as run_cell_mock,
            ):
                report, exit_code = matrix.run_matrix(options)

            self.assertEqual(exit_code, 0)
            self.assertEqual(report["status"], "pass")
            self.assertEqual(report["expectedCellCount"], 4)
            self.assertEqual(report["passedCellCount"], 4)
            self.assertEqual(run_cell_mock.call_count, 4)
            self.assertTrue((options.output_dir / matrix.JSON_REPORT_NAME).is_file())

    def test_fails_when_ready_worker_inventory_changes_during_the_run(self) -> None:
        with tempfile.TemporaryDirectory() as directory, mock.patch.dict(
            os.environ,
            {"CODEX_KEY": "codex-secret", "CLAUDE_KEY": "claude-secret"},
            clear=True,
        ):
            options = matrix_options(pathlib.Path(directory) / "matrix")
            initial = [{"name": "worker-a", "ready": True}]
            final = [*initial, {"name": "worker-b", "ready": True}]
            with (
                mock.patch.object(
                    matrix.release_gate,
                    "repository_state",
                    return_value={"gitSha": "b" * 40, "worktreeDirty": False},
                ),
                mock.patch.object(matrix, "discover_nodes", side_effect=[initial, final]),
                mock.patch.object(
                    matrix,
                    "run_cell",
                    return_value=({"status": "pass"}, []),
                ),
            ):
                report, exit_code = matrix.run_matrix(options)

        self.assertEqual(exit_code, 1)
        self.assertEqual(report["status"], "fail")
        self.assertIn(
            "stage5.matrix.node_inventory_changed",
            {error["code"] for error in report["errors"]},
        )


if __name__ == "__main__":
    unittest.main()
