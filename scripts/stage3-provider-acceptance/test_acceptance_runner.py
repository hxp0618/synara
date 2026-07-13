from __future__ import annotations

import contextlib
import dataclasses
import io
import pathlib
import tempfile
import unittest
from collections.abc import Callable, Mapping, Sequence
from typing import Any

import acceptance_runner as acceptance


def runner_options(*, restart_control_plane: bool = True) -> acceptance.RunnerOptions:
    return acceptance.RunnerOptions(
        target="fake",
        provider="codex",
        output_dir=pathlib.Path(tempfile.gettempdir()) / "synara-stage3-acceptance-runner-tests",
        timeout_seconds=30.0,
        runner_command=("fixture",),
        skip_build=True,
        control_plane_binary=pathlib.Path("/tmp/fake-control-plane"),
        keep=False,
        restart_control_plane=restart_control_plane,
        docker_socket_path=pathlib.Path("/var/run/docker.sock"),
        docker_worker_image=None,
        docker_skip_worker_build=False,
        docker_control_plane_host="host.docker.internal",
        docker_network_mode=None,
        docker_memory_bytes=2 << 30,
        docker_nano_cpus=1_000_000_000,
        kubernetes_context=None,
        kubernetes_kubeconfig=None,
        kubernetes_allow_nondisposable=False,
        kubernetes_worker_image=None,
        kubernetes_skip_worker_build=False,
        kubernetes_control_plane_host="host.docker.internal",
        kind_bin="kind",
        kind_cluster_name=None,
        kind_node_image="kindest/node:v1.33.1",
    )


class FakeAPI:
    def __init__(self) -> None:
        self.requests: list[tuple[str, str, Mapping[str, Any] | None]] = []

    def request(
        self,
        method: str,
        path: str,
        payload: Mapping[str, Any] | None = None,
        expected: Sequence[int] = (200,),
    ) -> Any:
        del expected
        self.requests.append((method, path, payload))
        return {"status": "resolved", "deliveryStatus": "delivered"}


class FakeDriver:
    def __init__(self, lifecycle: acceptance.TargetLifecycle, *, name: str = "fake") -> None:
        self.name = name
        self.lifecycle = lifecycle
        self.api = FakeAPI()
        self.restart_calls = 0

    def prepare(self) -> Mapping[str, Any]:
        return {}

    def start(self) -> Mapping[str, Any]:
        return {}

    def restart(self) -> Mapping[str, Any]:
        self.restart_calls += 1
        return {"processGeneration": self.restart_calls + 1}

    def provision_target(
        self,
        tenant_id: str,
        organization_id: str,
        provider: str,
    ) -> Mapping[str, Any]:
        del tenant_id, provider
        return {"id": "target-id", "organizationId": organization_id, "kind": self.name}

    def replace_worker(self, tenant_id: str, target_id: str, provider: str) -> Mapping[str, Any]:
        del tenant_id, target_id, provider
        return {"replacementWorkerId": "worker-replacement"}

    def observe_execution(self, target_id: str, execution_id: str) -> Mapping[str, Any]:
        return {"targetId": target_id, "executionId": execution_id}

    def observe_terminal_execution(self, target_id: str, execution_id: str) -> Mapping[str, Any]:
        return {"targetId": target_id, "executionId": execution_id, "terminal": True}

    def stop(self) -> None:
        return None

    def cleanup(self) -> None:
        return None


class CaseOrderSuite(acceptance.AcceptanceSuite):
    def __init__(self, driver: FakeDriver) -> None:
        super().__init__(
            runner_options(),
            driver,
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        self.case_order: list[str] = []

    def _case(
        self,
        case_id: str,
        name: str,
        operation: Callable[[], Mapping[str, Any] | None],
        requires: Sequence[str] = (),
    ) -> None:
        del name, operation, requires
        self.case_order.append(case_id)


class BarrierSuite(acceptance.AcceptanceSuite):
    def __init__(self, lifecycle: acceptance.TargetLifecycle) -> None:
        self.fake_driver = FakeDriver(lifecycle)
        super().__init__(
            runner_options(),
            self.fake_driver,
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        self.state.tenant_id = "tenant-id"
        self.state.organization_id = "organization-id"
        self.state.target_id = "target-id"
        self.state.session_id = "session-id"
        self.created_turns: list[str] = []
        self.interaction_waits = 0
        self.post_restart_waits = 0

    def _create_turn(self, input_text: str) -> dict[str, Any]:
        self.created_turns.append(input_text)
        return {"id": f"turn-{len(self.created_turns)}"}

    def _wait_for_interaction(self, turn_id: str, kind: str) -> dict[str, Any]:
        self.interaction_waits += 1
        return {
            "id": "interaction-1",
            "turnId": turn_id,
            "kind": kind,
            "executionId": "execution-1",
            "requestId": "approval-request-1",
        }

    def _wait_compatible_manifest(self, target_id: str) -> dict[str, Any]:
        if target_id != "target-id":
            raise AssertionError(f"unexpected target ID: {target_id}")
        return {
            "manifest": {
                "manifestId": "manifest-1",
                "workerStatusCounts": {"online": 1},
                "workerProtocol": {"minimum": 2, "maximum": 2},
                "runtimeEvent": {"minimum": 2, "maximum": 2},
                "workerBuild": {"gitSha": "abcdef0"},
            },
            "provider": {
                "provider": "codex",
                "supportTier": "experimental",
                "compatibilityStatus": "compatible",
                "runtime": {"available": True, "compatible": True},
                "releasePolicy": {"enabled": True},
            },
        }

    def _wait_for_turn_terminal(
        self,
        turn_id: str,
        expected_event_type: str,
    ) -> tuple[dict[str, Any], list[dict[str, Any]]]:
        terminal = {
            "sequence": 2,
            "eventType": expected_event_type,
            "executionId": "execution-1",
            "workerId": "worker-1",
            "generation": 1,
        }
        return terminal, [
            {"sequence": 1, "eventType": "request.resolved", "executionId": "execution-1"},
            terminal,
        ]

    def _all_events(self) -> list[dict[str, Any]]:
        return [{"sequence": 1, "eventType": "execution.completed"}]

    def _wait_post_restart_online_worker(self, target_id: str) -> dict[str, Any]:
        self.post_restart_waits += 1
        if target_id != "target-id":
            raise AssertionError(f"unexpected target ID: {target_id}")
        return {"manifestId": "manifest-after-restart", "workerStatusCounts": {"online": 1}}


class AcceptanceSuiteLifecycleTest(unittest.TestCase):
    def test_standing_worker_preserves_existing_case_order(self) -> None:
        suite = CaseOrderSuite(FakeDriver(acceptance.STANDING_WORKER))

        suite.run()

        self.assertEqual(
            suite.case_order,
            [
                "environment.target-prepare",
                "environment.control-plane-start",
                "identity.dev-login",
                "runtime.worker-discovery",
                "resources.credential-project-session",
                "fixture.text-tool-usage-artifact",
                "fixture.approval-resolution",
                "fixture.user-input-resolution",
                "fixture.provider-error",
                "recovery.control-plane-restart",
                "fixture.second-turn-continuity",
            ],
        )

    def test_execution_pinned_worker_provisions_resources_before_barrier(self) -> None:
        suite = CaseOrderSuite(FakeDriver(acceptance.EXECUTION_PINNED_WORKER))

        suite.run()

        self.assertEqual(
            suite.case_order,
            [
                "environment.target-prepare",
                "environment.control-plane-start",
                "identity.dev-login",
                "runtime.target-provision",
                "resources.credential-project-session",
                "runtime.worker-discovery",
                "fixture.approval-resolution",
                "fixture.text-tool-usage-artifact",
                "fixture.user-input-resolution",
                "fixture.provider-error",
                "recovery.control-plane-restart",
                "fixture.second-turn-continuity",
            ],
        )

    def test_replacement_cases_follow_capability_not_driver_name(self) -> None:
        managed = CaseOrderSuite(FakeDriver(acceptance.STANDING_MANAGED_WORKER, name="not-docker"))
        unmanaged_docker = CaseOrderSuite(FakeDriver(acceptance.STANDING_WORKER, name="docker"))

        managed.run()
        unmanaged_docker.run()

        self.assertIn("recovery.worker-replacement", managed.case_order)
        self.assertIn("recovery.post-replacement-workspace-turn", managed.case_order)
        self.assertNotIn("recovery.worker-replacement", unmanaged_docker.case_order)
        self.assertNotIn("recovery.post-replacement-workspace-turn", unmanaged_docker.case_order)

    def test_execution_pinned_approval_barrier_is_resolved_without_second_turn(self) -> None:
        suite = BarrierSuite(acceptance.EXECUTION_PINNED_WORKER)

        discovery = suite._discover_worker()
        resolution = suite._approval_resolution()

        self.assertEqual(suite.created_turns, ["[approval]"])
        self.assertEqual(suite.interaction_waits, 1)
        self.assertIsNone(suite.state.pending_approval)
        self.assertEqual(discovery["readinessBarrier"]["executionId"], "execution-1")
        self.assertEqual(resolution["turnId"], "turn-1")
        self.assertEqual(
            suite.fake_driver.api.requests,
            [
                (
                    "POST",
                    "/v1/executions/execution-1/approvals/approval-request-1/resolve",
                    {"decision": "accept"},
                )
            ],
        )

    def test_execution_pinned_restart_defers_worker_wait_until_next_execution(self) -> None:
        suite = BarrierSuite(acceptance.EXECUTION_PINNED_WORKER)

        evidence = suite._restart_control_plane()

        self.assertEqual(suite.post_restart_waits, 0)
        self.assertEqual(evidence["workerAllocation"], "execution-pinned")
        self.assertEqual(evidence["postRestartWorkerExpectation"], "deferred-until-next-execution")

    def test_standing_restart_waits_for_online_worker(self) -> None:
        suite = BarrierSuite(acceptance.STANDING_WORKER)

        evidence = suite._restart_control_plane()

        self.assertEqual(suite.post_restart_waits, 1)
        self.assertEqual(evidence["postRestartManifestId"], "manifest-after-restart")


class RunnerOptionsTest(unittest.TestCase):
    def test_kubernetes_options_use_release_timeout_and_explicit_kind_context(self) -> None:
        options = acceptance.parse_args(
            [
                "--target",
                "kubernetes",
                "--kubernetes-context",
                "kind-fixture",
                "--kubernetes-kubeconfig",
                "/tmp/kind-fixture-kubeconfig",
                "--kubernetes-worker-image",
                "synara-worker:test",
                "--kubernetes-skip-worker-build",
            ]
        )

        self.assertEqual(options.timeout_seconds, 1200.0)
        self.assertEqual(options.kubernetes_context, "kind-fixture")
        self.assertEqual(
            options.kubernetes_kubeconfig,
            pathlib.Path("/tmp/kind-fixture-kubeconfig").resolve(),
        )
        self.assertEqual(options.kubernetes_worker_image, "synara-worker:test")
        self.assertTrue(options.kubernetes_skip_worker_build)

    def test_kubernetes_existing_image_requires_skip_build(self) -> None:
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            acceptance.parse_args(
                ["--target", "kubernetes", "--kubernetes-worker-image", "operator-owned:image"]
            )


class KubernetesDriverObservationTest(unittest.TestCase):
    def test_observe_execution_validates_real_pod_contract(self) -> None:
        options = dataclasses.replace(
            runner_options(),
            target="kubernetes",
            kubernetes_context="kind-fixture",
            kubernetes_kubeconfig=pathlib.Path("/tmp/kind-fixture-kubeconfig"),
        )

        class ObservationDriver(acceptance.KubernetesDriver):
            def _wait_execution_pod(self, target_id: str, execution_id: str) -> dict[str, Any]:
                compact = target_id.replace("-", "")[:12]
                return {
                    "metadata": {
                        "name": "synara-exec-fixture",
                        "uid": "pod-uid",
                        "labels": {
                            "synara.io/managed": "true",
                            "synara.io/execution-target-id": target_id,
                            "synara.io/execution-id": execution_id,
                            "synara.io/generation": "1",
                        },
                    },
                    "spec": {
                        "serviceAccountName": self.worker_service_account,
                        "automountServiceAccountToken": False,
                        "restartPolicy": "Never",
                        "securityContext": {"runAsNonRoot": True, "fsGroup": 10001},
                        "volumes": [
                            {"name": "workspace", "emptyDir": {}},
                            {"name": "tmp", "emptyDir": {}},
                            {"name": "home", "emptyDir": {}},
                        ],
                        "containers": [
                            {
                                "name": "agentd",
                                "image": self.image,
                                "imagePullPolicy": "Never",
                                "securityContext": {
                                    "allowPrivilegeEscalation": False,
                                    "readOnlyRootFilesystem": True,
                                    "runAsNonRoot": True,
                                    "runAsUser": 10001,
                                    "runAsGroup": 10001,
                                    "capabilities": {"drop": ["ALL"]},
                                },
                                "env": [
                                    {
                                        "name": "SYNARA_AGENTD_ASSIGNED_EXECUTION_ID",
                                        "value": execution_id,
                                    },
                                    {
                                        "name": "SYNARA_WORKER_REGISTRATION_TOKEN",
                                        "valueFrom": {
                                            "secretKeyRef": {
                                                "name": f"synara-agentd-{compact}",
                                                "key": "registration-token",
                                            }
                                        },
                                    },
                                ],
                            }
                        ],
                    },
                    "status": {"phase": "Running"},
                }

            def _foundation_evidence(self, target_id: str, secret_name: Any) -> Mapping[str, Any]:
                del target_id, secret_name
                return {"serviceAccount": self.worker_service_account}

        driver = ObservationDriver(
            pathlib.Path.cwd(),
            options,
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        self.addCleanup(driver._release_state)
        target_id = "11111111-2222-3333-4444-555555555555"
        evidence = driver.observe_execution(target_id, "execution-id")

        self.assertEqual(evidence["phase"], "Running")
        self.assertEqual(evidence["volumes"], ["home", "tmp", "workspace"])
        self.assertEqual(evidence["foundation"]["serviceAccount"], driver.worker_service_account)


if __name__ == "__main__":
    unittest.main()
