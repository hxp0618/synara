from __future__ import annotations

import contextlib
import dataclasses
import io
import json
import pathlib
import tempfile
import unittest
from typing import Any, Mapping
from unittest import mock

import acceptance_runner as acceptance
import kubernetes_real_provider_release_rollout_gate as gate
import worker_release_rollout_common as rollout


DIGEST_A = "sha256:" + "a" * 64
DIGEST_B = "sha256:" + "b" * 64
IMAGE_ID_A = "sha256:" + "c" * 64
IMAGE_ID_B = "sha256:" + "d" * 64


def gate_options(output_dir: pathlib.Path, *, provider: str = "codex") -> gate.GateOptions:
    return gate.GateOptions(
        repo_root=pathlib.Path(__file__).resolve().parents[2],
        output_dir=output_dir,
        timeout_seconds=4000.0,
        skip_build=False,
        control_plane_binary=None,
        docker_socket_path=pathlib.Path("/var/run/docker.sock"),
        kubernetes_control_plane_host="host.docker.internal",
        kind_bin="kind",
        kind_node_image="kindest/node:v1.33.1",
        kind_worker_nodes=2,
        load_waves=gate.DEFAULT_LOAD_WAVES,
        registry_image=gate.DEFAULT_REGISTRY_IMAGE,
        go_proxy=None,
        provider=provider,
        real_provider_load_sla_file=(
            pathlib.Path(__file__).resolve().parents[2]
            / "deploy"
            / "worker"
            / "production-load-sla.json"
        ),
        real_provider_credential_env="CODEX_KEY" if provider == "codex" else "CLAUDE_TOKEN",
        real_provider_credential_field="apiKey" if provider == "codex" else "authToken",
        real_provider_base_url_env=None if provider == "codex" else "CLAUDE_BASE_URL",
        real_provider_model="gpt-5.6-sol" if provider == "codex" else "claude-sonnet-4-6",
        real_provider_model_env=None,
    )


def release_image(slot: str, digest: str, image_id: str) -> rollout.ReleaseImage:
    return rollout.ReleaseImage(
        slot=slot,
        version=f"0.5.4+rollout.{slot}",
        tag=f"localhost:55091/synara/worker:{slot}",
        exact_reference=f"localhost:55091/synara/worker@{digest}",
        digest=digest,
        image_id=image_id,
        metadata_path=pathlib.Path(f"/tmp/{slot}.json"),
    )


class ParseArgsTest(unittest.TestCase):
    def test_reads_only_controlled_environment_names_into_options(self) -> None:
        with tempfile.TemporaryDirectory() as directory, mock.patch.dict(
            "os.environ",
            {
                "CLAUDE_TOKEN": "claude-secret-value",
                "CLAUDE_BASE_URL": "https://claude.example.test",
                "CLAUDE_MODEL": "claude-sonnet-4-6",
            },
        ):
            options = gate.parse_args(
                [
                    "--provider",
                    "claudeAgent",
                    "--real-provider-credential-env",
                    "CLAUDE_TOKEN",
                    "--real-provider-credential-field",
                    "authToken",
                    "--real-provider-base-url-env",
                    "CLAUDE_BASE_URL",
                    "--real-provider-model-env",
                    "CLAUDE_MODEL",
                    "--output-dir",
                    directory,
                ]
            )

        encoded = json.dumps(dataclasses.asdict(options), default=str)
        self.assertEqual(options.provider, "claudeAgent")
        self.assertEqual(options.real_provider_credential_field, "authToken")
        self.assertEqual(options.real_provider_model, "claude-sonnet-4-6")
        self.assertNotIn("claude-secret-value", encoded)
        self.assertNotIn("https://claude.example.test", encoded)
        self.assertEqual(options.real_provider_model_env, "CLAUDE_MODEL")

    def test_rejects_auth_token_for_non_claude_provider(self) -> None:
        with mock.patch.dict("os.environ", {"CODEX_KEY": "codex-secret"}):
            with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit) as caught:
                gate.parse_args(
                    [
                        "--provider",
                        "codex",
                        "--real-provider-credential-env",
                        "CODEX_KEY",
                        "--real-provider-credential-field",
                        "authToken",
                    ]
                )
        self.assertEqual(caught.exception.code, 2)


class RunnerOptionsTest(unittest.TestCase):
    def test_runner_options_use_real_provider_load_suite_and_production_worker_timing(self) -> None:
        with tempfile.TemporaryDirectory() as directory, mock.patch.dict(
            "os.environ",
            {"CODEX_KEY": "codex-secret-value"},
        ):
            options = gate_options(pathlib.Path(directory))
            runner = gate.runner_options(options)

        self.assertEqual(runner.suite, "real-provider-load")
        self.assertEqual(runner.provider, "codex")
        self.assertEqual(runner.real_provider_credential_env, "CODEX_KEY")
        self.assertEqual(runner.real_provider_model, "gpt-5.6-sol")
        self.assertEqual(
            runner.worker_lease_ttl,
            gate.REAL_PROVIDER_ROLLOUT_WORKER_LEASE_TTL,
        )
        self.assertEqual(
            runner.worker_heartbeat_timeout,
            gate.REAL_PROVIDER_ROLLOUT_WORKER_HEARTBEAT_TIMEOUT,
        )
        self.assertEqual(runner.worker_lease_ttl, "30s")
        self.assertEqual(runner.worker_heartbeat_timeout, "90s")
        self.assertEqual(runner.operator_approved_sla_file, options.real_provider_load_sla_file)


class DriverConfigurationTest(unittest.TestCase):
    def build_driver(
        self, *, provider: str = "claudeAgent"
    ) -> gate.KubernetesRealProviderReleaseRolloutDriver:
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        with mock.patch.dict(
            "os.environ",
            {"CLAUDE_TOKEN": "claude-secret", "CLAUDE_BASE_URL": "https://claude.example.test"},
        ):
            options = gate_options(pathlib.Path(self.temporary.name), provider=provider)
            driver = gate.KubernetesRealProviderReleaseRolloutDriver(
                options,
                gate.runner_options(options),
                acceptance.Deadline(1200.0),
                acceptance.SecretRedactor(),
            )
        self.addCleanup(driver._release_state)
        return driver

    def test_provision_targets_enable_only_requested_provider(self) -> None:
        driver = self.build_driver()
        driver.images = {
            "baseline": release_image("baseline", DIGEST_A, IMAGE_ID_A),
            "candidate": release_image("candidate", DIGEST_B, IMAGE_ID_B),
        }
        calls: list[dict[str, Any]] = []

        def create_target(*_args: Any, **kwargs: Any) -> dict[str, Any]:
            calls.append(kwargs)
            return {
                "id": "main-target" if len(calls) == 1 else "observer-target",
                "organizationId": "organization-id",
                "kind": "kubernetes",
                "name": kwargs["name"],
                "status": "active",
            }

        with (
            mock.patch.object(driver, "_create_kubernetes_target", side_effect=create_target),
            mock.patch.object(driver, "_wait_and_label_namespace"),
        ):
            evidence = driver.provision_rollout_targets(
                "tenant-id",
                "organization-id",
                "claudeAgent",
            )

        self.assertEqual(calls[0]["experimental_providers"], ("claudeAgent",))
        self.assertEqual(calls[1]["experimental_providers"], ("claudeAgent",))
        self.assertEqual(evidence["mainExperimentalProviders"], ["claudeAgent"])
        self.assertEqual(evidence["observerExperimentalProviders"], ["claudeAgent"])


class SuiteBehaviorTest(unittest.TestCase):
    def build_suite(
        self, *, provider: str = "codex"
    ) -> gate.KubernetesRealProviderReleaseRolloutSuite:
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        environment = {"CODEX_KEY": "codex-secret-value"}
        if provider == "claudeAgent":
            environment = {
                "CLAUDE_TOKEN": "claude-secret-value",
                "CLAUDE_BASE_URL": "https://claude.example.test",
            }
        with mock.patch.dict("os.environ", environment):
            options = gate_options(pathlib.Path(self.temporary.name), provider=provider)
            runner = gate.runner_options(options)
            driver = gate.KubernetesRealProviderReleaseRolloutDriver(
                options,
                runner,
                acceptance.Deadline(1200.0),
                acceptance.SecretRedactor(),
            )
            suite = gate.KubernetesRealProviderReleaseRolloutSuite(
                runner,
                driver,
                acceptance.Deadline(1200.0),
                acceptance.SecretRedactor(),
            )
        self.addCleanup(driver._release_state)
        return suite

    def test_start_pending_approval_uses_canonical_prompt_and_command(self) -> None:
        suite = self.build_suite()
        suite.sessions = {"baseline-seed": "session-1"}

        def approval_interaction(
            turn_id: str,
            *,
            expected_command: str,
            session_id: str | None = None,
        ) -> tuple[dict[str, Any], str, str, dict[str, Any], str]:
            del session_id
            interaction = {
                "id": "interaction-1",
                "kind": "approval",
                "turnId": turn_id,
                "executionId": "execution-1",
                "requestId": "request-1",
                "payload": {
                    "requestKind": "command",
                    "command": expected_command,
                },
            }
            return (
                interaction,
                "execution-1",
                "request-1",
                {"requestKind": "command"},
                expected_command,
            )

        with (
            mock.patch.object(suite, "_create_turn", return_value={"id": "turn-1"}) as create_turn,
            mock.patch.object(
                suite,
                "_real_provider_approval_interaction",
                side_effect=approval_interaction,
            ) as approval,
            mock.patch.object(
                suite,
                "_active_approval_evidence",
                return_value={"executionId": "execution-1", "workerId": "worker-1", "generation": 1},
            ),
            mock.patch.object(
                suite.driver,
                "observe_execution",
                return_value={"podName": "pod-1", "nodeName": "worker-node-1"},
            ),
        ):
            pending = suite._start_pending_approval("baseline-seed", "target-1")

        create_turn.assert_called_once()
        prompt = create_turn.call_args.args[0]
        self.assertEqual(prompt, gate.real_provider_rollout_approval_prompt(pending["marker"]))
        self.assertIn("This is a new acceptance Turn", prompt)
        self.assertIn("Do not reuse any tool result", prompt)
        self.assertEqual(create_turn.call_args.kwargs["runtime_mode"], "approval-required")
        self.assertEqual(create_turn.call_args.kwargs["session_id"], "session-1")
        approval.assert_called_once_with(
            "turn-1",
            expected_command=acceptance.real_provider_approval_command(pending["marker"]),
            session_id="session-1",
        )
        self.assertEqual(pending["requestKind"], "command")

    def test_repeated_load_turn_uses_new_turn_prompt_without_changing_session(self) -> None:
        suite = self.build_suite()
        session = {"sessionId": "session-1"}

        def approval_interaction(
            turn_id: str,
            *,
            expected_command: str,
            session_id: str | None = None,
        ) -> tuple[dict[str, Any], str, str, dict[str, Any], str]:
            interaction = {
                "id": "interaction-1",
                "kind": "approval",
                "turnId": turn_id,
                "executionId": "execution-2",
                "requestId": "request-2",
                "payload": {
                    "requestKind": "command",
                    "command": expected_command,
                },
            }
            return (
                interaction,
                "execution-2",
                "request-2",
                {"requestKind": "command"},
                expected_command,
            )

        with (
            mock.patch.object(suite, "_all_events", return_value=[{"sequence": 10}]),
            mock.patch.object(suite, "_create_turn", return_value={"id": "turn-2"}) as create_turn,
            mock.patch.object(
                suite,
                "_real_provider_approval_interaction",
                side_effect=approval_interaction,
            ) as approval,
            mock.patch.object(
                suite,
                "_active_approval_evidence",
                return_value={
                    "executionId": "execution-2",
                    "workerId": "worker-2",
                    "generation": 1,
                },
            ),
        ):
            evidence = suite._start_real_provider_load_turn(session, 1, 1)

        prompt = create_turn.call_args.args[0]
        self.assertEqual(prompt, gate.real_provider_rollout_approval_prompt(evidence["marker"]))
        self.assertIn("This is a new acceptance Turn", prompt)
        self.assertIn("execute this Turn's command exactly once", prompt)
        self.assertEqual(create_turn.call_args.kwargs["session_id"], "session-1")
        approval.assert_called_once_with(
            "turn-2",
            expected_command=acceptance.real_provider_approval_command(evidence["marker"]),
            session_id="session-1",
        )
        self.assertEqual(evidence["sessionSequenceBeforeTurn"], 10)

    def test_resolve_real_provider_command_turn_accepts_follow_up_approvals(self) -> None:
        suite = self.build_suite()
        command = acceptance.real_provider_approval_command("MARKER")
        turn = {"id": "turn-1"}
        interaction_1 = {
            "id": "interaction-1",
            "kind": "approval",
            "turnId": "turn-1",
            "executionId": "execution-1",
            "requestId": "request-1",
            "payload": {"requestKind": "command", "command": command},
        }
        interaction_2 = {
            "id": "interaction-2",
            "kind": "approval",
            "turnId": "turn-1",
            "executionId": "execution-1",
            "requestId": "request-2",
            "payload": {"requestKind": "command", "command": command},
        }
        terminal = {
            "eventType": "execution.completed",
            "executionId": "execution-1",
            "workerId": "worker-1",
            "generation": 1,
            "sequence": 11,
            "payload": {"output": {"text": "MARKER"}},
        }
        events = [
            {
                "eventType": "turn.created",
                "executionId": "execution-1",
                "workerId": "worker-1",
                "generation": 1,
                "sequence": 1,
            },
            {
                "eventType": "execution.leased",
                "executionId": "execution-1",
                "workerId": "worker-1",
                "generation": 1,
                "payload": {
                    "providerResume": {
                        "requestedStrategy": "native-cursor",
                        "selectedStrategy": "authoritative-history",
                        "reasonCode": "cursor_absent",
                    }
                },
                "sequence": 2,
            },
            {
                "eventType": "execution.started",
                "executionId": "execution-1",
                "workerId": "worker-1",
                "generation": 1,
                "sequence": 3,
            },
            {"eventType": "request.opened", "executionId": "execution-1", "workerId": "worker-1", "generation": 1, "payload": {"requestId": "request-1"}, "sequence": 4},
            {"eventType": "request.resolved", "executionId": "execution-1", "workerId": "worker-1", "generation": 1, "payload": {"requestId": "request-1"}, "sequence": 5},
            {"eventType": "request.opened", "executionId": "execution-1", "workerId": "worker-1", "generation": 1, "payload": {"requestId": "request-2"}, "sequence": 6},
            {"eventType": "request.resolved", "executionId": "execution-1", "workerId": "worker-1", "generation": 1, "payload": {"requestId": "request-2"}, "sequence": 7},
            {
                "eventType": "item.started",
                "executionId": "execution-1",
                "workerId": "worker-1",
                "generation": 1,
                "payload": {
                    "itemType": "command_execution",
                    "status": "inProgress",
                    "data": {
                        "provider": "codex",
                        "providerItemId": "command-item-1",
                        "terminal": {
                            "terminalId": "command-item-1",
                            "eventType": "terminal.started",
                        },
                    },
                },
                "sequence": 8,
            },
            {
                "eventType": "item.completed",
                "executionId": "execution-1",
                "workerId": "worker-1",
                "generation": 1,
                "payload": {
                    "itemType": "command_execution",
                    "status": "completed",
                    "data": {
                        "provider": "codex",
                        "providerItemId": "command-item-1",
                        "terminal": {
                            "terminalId": "command-item-1",
                            "eventType": "terminal.exited",
                            "exitCode": 0,
                        },
                    },
                },
                "sequence": 9,
            },
            {
                "eventType": "content.delta",
                "eventVersion": 2,
                "executionId": "execution-1",
                "workerId": "worker-1",
                "generation": 1,
                "payload": {"streamKind": "assistant_text", "delta": "MARKER"},
                "sequence": 10,
            },
            terminal,
        ]

        suite.driver.api = mock.Mock()
        suite.driver.api.request.side_effect = [
            {"status": "resolved", "deliveryStatus": "acknowledged"},
            {"status": "resolved", "deliveryStatus": "acknowledged"},
        ]

        with (
            mock.patch.object(
                suite,
                "_wait_for_turn_terminal_or_follow_up_approval",
                side_effect=[
                    {"interaction": interaction_2},
                    {"terminal": terminal, "events": events},
                ],
            ),
        ):
            evidence = suite._resolve_real_provider_command_turn(
                turn=turn,
                interaction=interaction_1,
                session_id="session-1",
                marker="MARKER",
                expected_command=command,
            )

        self.assertEqual(evidence["approvalCount"], 2)
        self.assertEqual(evidence["executionId"], "execution-1")
        self.assertEqual(evidence["workerId"], "worker-1")
        self.assertEqual(
            evidence["providerResume"]["selectedStrategy"],
            "authoritative-history",
        )
        self.assertTrue(evidence["commandItem"]["terminalIdentityMatched"])
        self.assertEqual(evidence["approvalResolutions"][0]["requestId"], "request-1")
        self.assertEqual(evidence["approvalResolutions"][1]["requestId"], "request-2")
        self.assertEqual(suite.driver.api.request.call_count, 2)

    def test_resolve_pending_approval_uses_marker_bound_command_validation(self) -> None:
        suite = self.build_suite()
        pending = {
            "sessionId": "session-1",
            "targetId": "target-1",
            "turn": {"id": "turn-1"},
            "interaction": {"id": "interaction-1"},
            "marker": "MARKER",
        }

        with (
            mock.patch.object(
                suite,
                "_resolve_real_provider_command_turn",
                return_value={"executionId": "execution-1"},
            ) as resolve_turn,
            mock.patch.object(
                suite.driver,
                "observe_terminal_execution",
                return_value={"podName": "pod-1"},
            ) as observe_terminal,
        ):
            evidence = suite._resolve_pending_approval(pending)

        resolve_turn.assert_called_once_with(
            turn={"id": "turn-1"},
            interaction={"id": "interaction-1"},
            session_id="session-1",
            marker="MARKER",
            expected_command=acceptance.real_provider_approval_command("MARKER"),
        )
        observe_terminal.assert_called_once_with("target-1", "execution-1")
        self.assertEqual(evidence["targetTerminal"], {"podName": "pod-1"})

    def test_complete_load_turn_uses_marker_bound_command_validation(self) -> None:
        suite = self.build_suite()
        suite.state.target_id = "target-1"
        load_turn = {
            "sessionId": "session-1",
            "turn": {"id": "turn-1"},
            "interaction": {"id": "interaction-1"},
            "active": {"executionId": "execution-1", "workerId": "worker-1", "generation": 1},
            "marker": "MARKER",
            "turnOrdinal": 1,
            "sessionSequenceBeforeTurn": 0,
            "turnStartedMonotonic": 0.0,
        }
        resolution = {
            "executionId": "execution-1",
            "workerId": "worker-1",
            "generation": 1,
            "providerResume": {
                "requestedStrategy": "native-cursor",
                "selectedStrategy": "authoritative-history",
                "reasonCode": "cursor_absent",
            },
            "markerMatched": True,
            "commandItem": {"terminalIdentityMatched": True},
            "providerTurn": {"sequenceRange": {"first": 1, "last": 1}},
        }
        release = {
            "executionId": "execution-1",
            "workerId": "worker-1",
            "generation": 1,
        }

        with (
            mock.patch.object(
                suite,
                "_resolve_real_provider_command_turn",
                return_value=resolution,
            ) as resolve_turn,
            mock.patch.object(suite, "_wait_execution_release", return_value=release),
            mock.patch.object(suite, "_all_events", return_value=[{"sequence": 1}]),
            mock.patch.object(
                suite.driver,
                "observe_terminal_execution",
                return_value={"podName": "pod-1"},
            ),
            mock.patch.object(rollout, "validate_release_load_identity"),
        ):
            evidence = suite._complete_real_provider_release_load_turn(
                load_turn,
                revision_id="revision-1",
                channel="promoted",
                manifest_id="manifest-1",
                expected_resume_strategy="authoritative-history",
                expected_resume_reason="cursor_absent",
            )

        resolve_turn.assert_called_once_with(
            turn={"id": "turn-1"},
            interaction={"id": "interaction-1"},
            session_id="session-1",
            marker="MARKER",
            expected_command=acceptance.real_provider_approval_command("MARKER"),
            expected_resume_strategy="authoritative-history",
            expected_resume_reason="cursor_absent",
        )
        self.assertTrue(evidence["markerMatched"])
        self.assertTrue(evidence["commandItemVerified"])

    def test_approval_command_items_must_match_terminal_execution_fence(self) -> None:
        suite = self.build_suite()

        def command_event(event_type: str, execution_id: str, sequence: int) -> dict[str, Any]:
            started = event_type == "item.started"
            return {
                "eventType": event_type,
                "executionId": execution_id,
                "workerId": "worker-1",
                "generation": 1,
                "sequence": sequence,
                "payload": {
                    "itemType": "command_execution",
                    "status": "inProgress" if started else "completed",
                    "data": {
                        "provider": "codex",
                        "providerItemId": "command-item-1",
                        "terminal": {
                            "terminalId": "command-item-1",
                            "eventType": "terminal.started" if started else "terminal.exited",
                            **({} if started else {"exitCode": 0}),
                        },
                    },
                },
            }

        with self.assertRaises(acceptance.AcceptanceError) as caught:
            suite._approval_command_item_evidence(
                [
                    command_event("item.started", "execution-1", 8),
                    command_event("item.completed", "execution-other", 9),
                ],
                execution_id="execution-1",
                worker_id="worker-1",
                generation=1,
                terminal_sequence=10,
            )

        self.assertEqual(
            caught.exception.code,
            "runner.real_provider_approval_command_item_fence_mismatch",
        )

    def test_multi_wave_rollout_uses_distinct_workers_and_cross_revision_native_cursor(self) -> None:
        suite = self.build_suite()
        suite.options = dataclasses.replace(
            suite.options,
            load_waves=3,
            load_min_duration_seconds=0.0,
            load_max_waves=3,
            operator_approved_sla=None,
            operator_approved_sla_file=None,
        )
        suite.state.target_id = "target-1"
        suite.revisions = {
            "candidate": {"id": "candidate-revision"},
            "baseline": {"id": "baseline-revision"},
        }
        suite.manifests = {
            "candidate": {"manifestId": "candidate-manifest"},
            "baseline": {"manifestId": "baseline-manifest"},
        }
        suite.driver.images = {
            "candidate": release_image("candidate", DIGEST_B, IMAGE_ID_B),
            "baseline": release_image("baseline", DIGEST_A, IMAGE_ID_A),
        }
        sessions = [
            {"sessionId": f"session-{index}", "provider": "codex", "credentialId": "credential-1"}
            for index in range(1, acceptance.REAL_PROVIDER_LOAD_SESSIONS + 1)
        ]
        suite._real_provider_load_session_cache = [dict(session) for session in sessions]
        session_sequences = {str(session["sessionId"]): 0 for session in sessions}
        executions: dict[str, dict[str, Any]] = {}
        execution_counter = 0
        expected_resume_calls: list[tuple[str, str]] = []

        def start_turn(
            session: Mapping[str, Any], wave_index: int, position: int
        ) -> dict[str, Any]:
            nonlocal execution_counter
            execution_counter += 1
            session_id = str(session["sessionId"])
            execution_id = f"execution-{execution_counter}"
            worker_id = f"worker-{execution_counter}"
            turn_id = f"turn-{execution_counter}"
            active = {
                "sessionId": session_id,
                "executionId": execution_id,
                "workerId": worker_id,
                "generation": 1,
            }
            executions[turn_id] = {
                **active,
                "nodeName": f"worker-node-{1 if execution_counter % 2 else 2}",
            }
            return {
                "sessionId": session_id,
                "provider": "codex",
                "turn": {"id": turn_id},
                "interaction": {"id": f"interaction-{execution_counter}"},
                "active": active,
                "marker": f"MARKER-{wave_index + 1}-{position}",
                "controlPlaneAdmissionLatencyMs": 1,
                "interactionReadyLatencyMs": 1,
                "sessionSequenceBeforeTurn": session_sequences[session_id],
                "turnStartedMonotonic": 0.0,
                "targetExecution": None,
            }

        def wait_release(turn_id: str, **kwargs: Any) -> dict[str, Any]:
            execution = executions[turn_id]
            return {
                "turnId": turn_id,
                "executionId": execution["executionId"],
                "workerId": execution["workerId"],
                "generation": 1,
                "workerManifestId": kwargs["manifest_id"],
                "workerReleaseRevisionId": kwargs["revision_id"],
                "workerReleaseChannel": kwargs["channel"],
                "terminalCount": 1 if kwargs["terminal"] else 0,
            }

        def complete_turn(load_turn: Mapping[str, Any], **kwargs: Any) -> dict[str, Any]:
            del kwargs
            session_id = str(load_turn["sessionId"])
            before = int(load_turn["sessionSequenceBeforeTurn"])
            after = before + 10
            session_sequences[session_id] = after
            expected_strategy = str(load_turn["expectedResumeStrategy"])
            expected_reason = str(load_turn["expectedResumeReason"])
            expected_resume_calls.append((expected_strategy, expected_reason))
            active = dict(load_turn["active"])
            return {
                **active,
                "sessionId": session_id,
                "turnId": str(dict(load_turn["turn"])["id"]),
                "turnOrdinal": load_turn["turnOrdinal"],
                "turnCompletionLatencyMs": 1,
                "eventTypeCounts": {"execution.completed": 1},
                "providerResume": {
                    "requestedStrategy": "native-cursor",
                    "selectedStrategy": expected_strategy,
                    "reasonCode": expected_reason,
                },
                "sessionSequenceBeforeTurn": before,
                "sessionSequenceAfterTurn": after,
                "sequenceRange": {"first": before + 1, "last": after, "count": 10},
                "markerMatched": True,
                "commandItemVerified": True,
            }

        def observe_execution(_target_id: str, execution_id: str, **_kwargs: Any) -> dict[str, Any]:
            execution_number = int(execution_id.rsplit("-", 1)[1])
            return {
                "podName": f"pod-{execution_number}",
                "nodeName": f"worker-node-{1 if execution_number % 2 else 2}",
            }

        inventory = [
            {
                "name": f"worker-node-{index}",
                "ready": True,
                "unschedulable": False,
                "controlPlane": False,
                "taints": [],
                "schedulableWorker": True,
            }
            for index in (1, 2)
        ]
        with (
            mock.patch.object(suite, "_start_real_provider_load_turn", side_effect=start_turn),
            mock.patch.object(suite, "_wait_execution_release", side_effect=wait_release),
            mock.patch.object(
                suite,
                "_wait_managed_worker",
                side_effect=lambda release, **_kwargs: {
                    "podName": f"pod-{str(release['executionId']).rsplit('-', 1)[1]}"
                },
            ),
            mock.patch.object(
                suite.driver,
                "observe_release_execution",
                side_effect=observe_execution,
            ),
            mock.patch.object(
                suite.driver,
                "observe_release_resource_profile",
                return_value={"resourceProfileMatched": True},
            ),
            mock.patch.object(
                suite.driver,
                "schedulable_worker_node_inventory",
                return_value=inventory,
            ),
            mock.patch.object(suite, "_interaction_pending", return_value=True),
            mock.patch.object(
                suite,
                "_assert_real_provider_load_quota_rejected",
                return_value={"reasonCode": "execution_quota_exceeded", "stateMutated": False},
            ),
            mock.patch.object(suite, "_assert_real_provider_load_turn_pending"),
            mock.patch.object(suite, "_pending_interactions", return_value=[]),
            mock.patch.object(
                suite,
                "_set_fixture_execution_quota",
                return_value={"maxConcurrentExecutions": 2},
            ),
            mock.patch.object(
                suite,
                "_complete_real_provider_release_load_turn",
                side_effect=complete_turn,
            ),
        ):
            candidate = suite._release_load_waves(
                slot="candidate",
                channel="promoted",
                wave_start=0,
                wave_count=2,
            )
            baseline = suite._release_load_waves(
                slot="baseline",
                channel="promoted",
                wave_start=2,
                wave_count=1,
            )

        self.assertEqual(candidate["expectedDistinctWorkerCount"], 8)
        self.assertEqual(candidate["distinctWorkerCount"], 8)
        self.assertEqual(baseline["expectedDistinctWorkerCount"], 4)
        self.assertEqual(baseline["globalDistinctWorkerCount"], 12)
        self.assertEqual(
            baseline["sessionResumeContinuity"]["crossRevisionSessionCount"],
            acceptance.REAL_PROVIDER_LOAD_SESSIONS,
        )
        self.assertTrue(baseline["sessionResumeContinuity"]["allSessionsCrossedRevision"])
        self.assertEqual(
            expected_resume_calls.count(("authoritative-history", "cursor_absent")),
            acceptance.REAL_PROVIDER_LOAD_SESSIONS,
        )
        self.assertEqual(
            expected_resume_calls.count(("native-cursor", "cursor_usable")),
            acceptance.REAL_PROVIDER_LOAD_SESSIONS * 2,
        )
        for evidence in (candidate, baseline):
            for wave in evidence["waveSamples"]:
                self.assertTrue(all(count == 2 for count in wave["overlapActiveCounts"]))
                self.assertTrue(all(count == 2 for count in wave["overlapNodeCounts"]))

    def test_require_distinct_nodes_fails_closed_when_only_one_node_is_observed(self) -> None:
        suite = self.build_suite()

        with self.assertRaises(acceptance.AcceptanceError) as caught:
            suite._require_distinct_nodes(
                "promoted-load",
                [
                    {"podName": "pod-1", "nodeName": "worker-node-1"},
                    {"podName": "pod-2", "nodeName": "worker-node-1"},
                ],
            )

        self.assertEqual(
            caught.exception.code,
            "runner.kubernetes_real_provider_distinct_nodes_missing",
        )

    def test_require_distinct_nodes_rejects_control_plane_or_tainted_inventory(self) -> None:
        suite = self.build_suite()
        inventory = [
            {
                "name": "worker-node-1",
                "ready": True,
                "unschedulable": False,
                "controlPlane": False,
                "taints": [],
                "schedulableWorker": True,
            },
            {
                "name": "control-plane-node",
                "ready": True,
                "unschedulable": False,
                "controlPlane": True,
                "taints": [{"key": "node-role.kubernetes.io/control-plane", "effect": "NoSchedule"}],
                "schedulableWorker": False,
            },
        ]
        with (
            mock.patch.object(
                suite.driver,
                "schedulable_worker_node_inventory",
                return_value=inventory,
            ),
            self.assertRaises(acceptance.AcceptanceError) as caught,
        ):
            suite._require_distinct_nodes(
                "promoted-load",
                [
                    {"podName": "pod-1", "nodeName": "worker-node-1"},
                    {"podName": "pod-2", "nodeName": "control-plane-node"},
                ],
            )

        self.assertEqual(
            caught.exception.code,
            "runner.kubernetes_real_provider_schedulable_worker_nodes_invalid",
        )


class BuildReportTest(unittest.TestCase):
    def test_build_report_binds_release_images_and_hides_environment_names(self) -> None:
        with tempfile.TemporaryDirectory() as directory, mock.patch.dict(
            "os.environ",
            {"CODEX_KEY": "codex-secret-value"},
        ):
            options = gate_options(pathlib.Path(directory))
            runner = gate.runner_options(options)

        driver = mock.Mock()
        driver.images = {
            "baseline": release_image("baseline", DIGEST_A, IMAGE_ID_A),
            "candidate": release_image("candidate", DIGEST_B, IMAGE_ID_B),
        }
        cases = [
            {
                "id": acceptance.REAL_PROVIDER_LOAD_CASE_ID,
                "status": "pass",
                "evidence": {
                    "operatorApprovedSla": {
                        "requested": {"minimumDurationSeconds": 1800},
                        "metricMapping": {},
                        "checks": [],
                        "enforced": True,
                    }
                },
            }
        ]

        report = gate.build_report(
            options,
            runner,
            run_id="run-1",
            source={"gitSha": "a" * 40, "worktreeDirty": False},
            started_at="2026-07-19T00:00:00Z",
            started=0.0,
            cases=cases,
            driver=driver,
        )

        encoded = json.dumps(report, sort_keys=True)
        self.assertEqual(report["provider"], "codex")
        self.assertFalse(
            report["configuration"]["realProvider"][
                "productCredentialEnvironmentNamePersisted"
            ]
        )
        self.assertTrue(
            report["configuration"]["realProvider"][
                "sensitiveEnvironmentNameOutputScanEnforced"
            ]
        )
        self.assertEqual(
            report["configuration"]["releaseImages"]["baseline"]["digest"],
            DIGEST_A,
        )
        self.assertEqual(
            report["configuration"]["realProviderLoad"]["operatorApprovedSla"]["requested"]["minimumDurationSeconds"],
            1800,
        )
        self.assertEqual(
            report["configuration"]["workerTiming"],
            {
                "leaseTTL": gate.REAL_PROVIDER_ROLLOUT_WORKER_LEASE_TTL,
                "heartbeatTimeout": gate.REAL_PROVIDER_ROLLOUT_WORKER_HEARTBEAT_TIMEOUT,
            },
        )
        self.assertNotIn("CODEX_KEY", encoded)
        self.assertNotIn("codex-secret-value", encoded)

    def test_output_environment_name_scan_fails_without_persisting_raw_names(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            output_dir = pathlib.Path(directory)
            options = dataclasses.replace(
                gate_options(output_dir),
                real_provider_base_url_env="CODEX_BASE_URL",
                real_provider_model_env="CODEX_MODEL",
            )
            specs = gate.sensitive_environment_name_specs(options)
            gate.write_report(
                {
                    "status": "pass",
                    "configuration": {
                        "credential": "CODEX_KEY",
                        "baseUrl": "CODEX_BASE_URL",
                        "model": "CODEX_MODEL",
                    },
                    "cases": [],
                },
                output_dir,
                acceptance.SecretRedactor(),
                specs,
            )
            encoded_report = (output_dir / gate.JSON_REPORT_NAME).read_text(encoding="utf-8")
            self.assertNotIn("CODEX_KEY", encoded_report)
            self.assertNotIn("CODEX_BASE_URL", encoded_report)
            self.assertNotIn("CODEX_MODEL", encoded_report)
            logs = output_dir / "logs"
            logs.mkdir()
            (logs / "leak.log").write_text("credential env CODEX_KEY\n", encoding="utf-8")

            scan = gate.output_sensitive_environment_name_scan_case(output_dir, specs)

        self.assertEqual(scan["status"], "fail")
        self.assertEqual(
            scan["reasonCode"],
            "runner.output_sensitive_environment_name_detected",
        )
        encoded_scan = json.dumps(scan, sort_keys=True)
        self.assertNotIn("CODEX_KEY", encoded_scan)
        self.assertNotIn("CODEX_BASE_URL", encoded_scan)
        self.assertNotIn("CODEX_MODEL", encoded_scan)


if __name__ == "__main__":
    unittest.main()
