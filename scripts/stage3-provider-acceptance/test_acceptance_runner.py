from __future__ import annotations

import contextlib
import base64
import dataclasses
import io
import pathlib
import subprocess
import tempfile
import unittest
from collections.abc import Callable, Mapping, Sequence
from typing import Any
from unittest import mock

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
        ssh_orbctl_bin="orbctl",
        ssh_machine_name=None,
        ssh_machine_arch="arm64",
        ssh_machine_image="ubuntu:24.04",
        ssh_node_version="24.13.1",
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
    def test_ssh_options_use_owned_orbstack_defaults_and_release_timeout(self) -> None:
        options = acceptance.parse_args(["--target", "ssh", "--ssh-machine-name", "synara-stage3-test"])

        self.assertEqual(options.timeout_seconds, 900.0)
        self.assertEqual(options.ssh_orbctl_bin, "orbctl")
        self.assertEqual(options.ssh_machine_name, "synara-stage3-test")
        self.assertEqual(options.ssh_machine_arch, "arm64")
        self.assertEqual(options.ssh_machine_image, "ubuntu:24.04")
        self.assertEqual(options.ssh_node_version, "24.13.1")

    def test_ssh_machine_name_rejects_non_dns_input(self) -> None:
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            acceptance.parse_args(["--target", "ssh", "--ssh-machine-name", "../../user-host"])

    def test_ssh_rejects_global_skip_build_without_separate_linux_runtime_inputs(self) -> None:
        with contextlib.redirect_stderr(io.StringIO()), self.assertRaises(SystemExit):
            acceptance.parse_args(
                [
                    "--target",
                    "ssh",
                    "--skip-build",
                    "--control-plane-binary",
                    "/tmp/synara-control-plane",
                ]
            )

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


class SSHDriverTest(unittest.TestCase):
    @staticmethod
    def _key(label: bytes) -> str:
        return "ssh-ed25519 " + base64.b64encode(label).decode("ascii")

    def test_provision_uses_one_time_key_pinned_host_key_and_product_install(self) -> None:
        target_ids = [
            "11111111-1111-4111-8111-111111111111",
            "22222222-2222-4222-8222-222222222222",
        ]

        class ProvisionAPI:
            def __init__(self) -> None:
                self.requests: list[tuple[str, str, Mapping[str, Any] | None]] = []
                self.created: list[Mapping[str, Any]] = []

            def request(
                inner_self,
                method: str,
                path: str,
                payload: Mapping[str, Any] | None = None,
                expected: Sequence[int] = (200,),
            ) -> Any:
                del expected
                inner_self.requests.append((method, path, payload))
                if method == "POST" and path.endswith("/execution-targets"):
                    assert payload is not None
                    inner_self.created.append(payload)
                    target_id = target_ids[len(inner_self.created) - 1]
                    return {
                        "id": target_id,
                        "organizationId": payload["organizationId"],
                        "kind": payload["kind"],
                        "name": payload["name"],
                        "status": "active",
                    }
                if path.endswith(f"/{target_ids[0]}/ssh/install"):
                    raise acceptance.AcceptanceError(
                        "ssh_connection_failed",
                        "The SSH execution target could not be reached.",
                    )
                if path.endswith(f"/{target_ids[1]}/ssh/install"):
                    return {
                        "targetId": target_ids[1],
                        "operation": "install",
                        "status": "active",
                        "serviceName": f"synara-agentd-{target_ids[1]}.service",
                        "binarySha256": "a" * 64,
                    }
                raise AssertionError(f"unexpected request: {method} {path}")

        class ProvisionDriver(acceptance.SSHDriver):
            def _worker_proxy_url(self) -> str:
                return "http://127.0.0.1:41234"

            def _worker_proxy_relay_evidence(self) -> Mapping[str, Any]:
                return {
                    "mode": "reverse-ssh-loopback",
                    "vmListenHost": "127.0.0.1",
                    "vmListenPort": 41234,
                    "upstreamAddress": "127.0.0.1:49999",
                    "readsUserSSHConfiguration": False,
                    "log": "/tmp/relay.log",
                }

            def _assert_remote_target_absent(
                self,
                target_id: str,
                *,
                cleanup_timeout: float | None = None,
            ) -> None:
                del cleanup_timeout
                self.absent_target = target_id

            def _require_service_active(self, service_name: str) -> dict[str, Any]:
                return {
                    "serviceName": service_name,
                    "activeState": "active",
                    "subState": "running",
                    "unitFileState": "enabled",
                    "mainPid": 42,
                    "restartCount": 0,
                }

        redactor = acceptance.SecretRedactor()
        options = dataclasses.replace(runner_options(), target="ssh")
        driver = ProvisionDriver(pathlib.Path.cwd(), options, acceptance.Deadline(30.0), redactor)
        self.addCleanup(driver._release_state)
        api = ProvisionAPI()
        driver.api = api  # type: ignore[assignment]
        driver.machine_ip = "192.0.2.10"
        driver.host_key = self._key(b"trusted-host-key")
        driver.client_public_key = self._key(b"wrong-host-key")
        driver.client_private_key = "-----BEGIN OPENSSH PRIVATE KEY-----\none-time-private-secret\n-----END OPENSSH PRIVATE KEY-----\n"
        redactor.add(driver.client_private_key, "[REDACTED_SSH_PRIVATE_KEY]")

        target = driver.provision_target("tenant-id", "organization-id", "codex")

        self.assertEqual(target["id"], target_ids[1])
        self.assertEqual(driver.absent_target, target_ids[0])
        self.assertEqual(driver.client_private_key, "")
        self.assertEqual(len(api.created), 2)
        negative_configuration = api.created[0]["configuration"]
        configuration = api.created[1]["configuration"]
        self.assertEqual(negative_configuration["hostKey"], driver.client_public_key)
        self.assertEqual(configuration["hostKey"], driver.host_key)
        self.assertEqual(configuration["privateKey"], "-----BEGIN OPENSSH PRIVATE KEY-----\none-time-private-secret\n-----END OPENSSH PRIVATE KEY-----\n")
        self.assertEqual(configuration["controlPlaneUrl"], "http://127.0.0.1:41234")
        self.assertEqual(configuration["serviceUser"], acceptance.SSH_SERVICE_USER)
        self.assertFalse(configuration["useSudo"])
        self.assertEqual(
            target["driverEvidence"]["controlPlaneCredentialLifecycle"],
            acceptance.SSH_CREDENTIAL_LIFECYCLE,
        )
        self.assertNotIn("one-time-private-secret", str(redactor.value(target)))

    def test_replace_restarts_sshd_and_systemd_then_waits_for_new_incarnation(self) -> None:
        events: list[str] = []

        class ReplacementAPI:
            def request(
                self,
                method: str,
                path: str,
                payload: Mapping[str, Any] | None = None,
                expected: Sequence[int] = (200,),
            ) -> Any:
                del payload, expected
                events.append(f"api:{method}:{path}")
                if path.endswith("/provider-policy"):
                    return {"status": "active"}
                if path.endswith("/ssh/upgrade"):
                    return {
                        "targetId": "target-id",
                        "operation": "upgrade",
                        "status": "active",
                        "serviceName": "synara-agentd-target-id.service",
                    }
                raise AssertionError(path)

            def wait_until(
                self,
                description: str,
                probe: Callable[[], Any],
                interval: float = 0.25,
            ) -> Any:
                del description, interval
                value = probe()
                if value is None:
                    raise AssertionError("replacement probe did not complete")
                return value

        class ReplacementDriver(acceptance.SSHDriver):
            def __init__(self, *args: Any, **kwargs: Any) -> None:
                super().__init__(*args, **kwargs)
                self.worker_calls = 0
                self.service_calls = 0

            def _worker_identity(self, target_id: str, *, required: bool = True) -> dict[str, Any] | None:
                del target_id, required
                self.worker_calls += 1
                if self.worker_calls == 1:
                    return {"id": "worker-id", "incarnation": 1, "instanceUid": "old", "status": "online", "podName": "ssh"}
                return {"id": "worker-id", "incarnation": 2, "instanceUid": "new", "status": "online", "podName": "ssh"}

            def _require_service_active(self, service_name: str) -> dict[str, Any]:
                self.service_calls += 1
                return {
                    "serviceName": service_name,
                    "activeState": "active",
                    "subState": "running",
                    "unitFileState": "enabled",
                    "mainPid": 100 if self.service_calls == 1 else 200,
                    "restartCount": 0,
                }

            def _remote_command(self, command: Sequence[str], **kwargs: Any) -> str:
                del kwargs
                events.append("remote:" + " ".join(command))
                return "active\n" if command[:2] == ["systemctl", "is-active"] else ""

        options = dataclasses.replace(runner_options(), target="ssh")
        driver = ReplacementDriver(
            pathlib.Path.cwd(), options, acceptance.Deadline(30.0), acceptance.SecretRedactor()
        )
        self.addCleanup(driver._release_state)
        driver.api = ReplacementAPI()  # type: ignore[assignment]
        driver.service_name = "synara-agentd-target-id.service"
        driver.host_key = self._key(b"trusted-host-key")

        evidence = driver.replace_worker("tenant-id", "target-id", "codex")

        self.assertTrue(evidence["sshdRestarted"])
        self.assertTrue(evidence["workerIdStable"])
        self.assertTrue(evidence["instanceUidChanged"])
        self.assertEqual(evidence["previousMainPid"], 100)
        self.assertEqual(evidence["replacementMainPid"], 200)
        self.assertIn("remote:systemctl restart ssh", events)
        self.assertTrue(any(event.endswith("/ssh/upgrade") for event in events))

    def test_cleanup_revokes_before_stopping_and_deletes_only_owned_machine(self) -> None:
        events: list[str] = []

        class CleanupAPI:
            def request(
                self,
                method: str,
                path: str,
                payload: Mapping[str, Any] | None = None,
                expected: Sequence[int] = (200,),
            ) -> Any:
                del payload, expected
                events.append(f"api:{method}:{path}")
                return {"operation": "revoke", "status": "disabled"}

        class RunningProcess:
            @staticmethod
            def poll() -> None:
                return None

        class CleanupDriver(acceptance.SSHDriver):
            def _remote_command(self, command: Sequence[str], **kwargs: Any) -> str:
                del kwargs
                events.append("remote:" + " ".join(command))
                return ""

            def _assert_remote_target_absent(self, target_id: str, **kwargs: Any) -> None:
                del kwargs
                events.append(f"absent:{target_id}")

            def _orbctl_completed(self, arguments: Sequence[str], **kwargs: Any) -> subprocess.CompletedProcess[str]:
                del kwargs
                events.append("orbctl:" + " ".join(arguments))
                return subprocess.CompletedProcess(list(arguments), 0, "deleted")

            def stop(self) -> None:
                events.append("stop-control-plane")
                self.process = None

            def _stop_worker_proxy_relay(self) -> None:
                events.append("stop-worker-proxy-relay")

            def _stop_worker_proxy(self) -> None:
                events.append("stop-worker-proxy")

            def _release_state(self) -> None:
                events.append("release-state")

        options = dataclasses.replace(
            runner_options(), target="ssh", ssh_machine_name="synara-stage3-owned"
        )
        driver = CleanupDriver(
            pathlib.Path.cwd(), options, acceptance.Deadline(30.0), acceptance.SecretRedactor()
        )
        driver.api = CleanupAPI()  # type: ignore[assignment]
        driver.process = RunningProcess()  # type: ignore[assignment]
        driver.machine_create_attempted = True
        driver.machine_created = True
        driver.tenant_id = "tenant-id"
        driver.target_id = "target-id"
        driver.service_name = "synara-agentd-target-id.service"

        driver.cleanup()

        revoke_index = next(index for index, event in enumerate(events) if event.endswith("/ssh/revoke"))
        relay_stop_index = events.index("stop-worker-proxy-relay")
        stop_index = events.index("stop-control-plane")
        delete = next(event for event in events if event.startswith("orbctl:delete"))
        self.assertLess(revoke_index, stop_index)
        self.assertLess(relay_stop_index, stop_index)
        self.assertEqual(delete, "orbctl:delete --force synara-stage3-owned")
        self.assertNotIn("--all", delete)

    def test_failed_machine_create_still_triggers_exact_cleanup_without_remote_mutation(self) -> None:
        events: list[str] = []

        class CreateFailureDriver(acceptance.SSHDriver):
            def _orbctl_command(self, arguments: Sequence[str], **kwargs: Any) -> str:
                del kwargs
                events.append("command:" + " ".join(arguments))
                if arguments[:2] == ["list", "--format"]:
                    return "[]"
                if arguments and arguments[0] == "create":
                    raise acceptance.AcceptanceError("runner.orbstack_command_failed", "create failed")
                raise AssertionError(arguments)

            def _remote_command(self, command: Sequence[str], **kwargs: Any) -> str:
                del kwargs
                events.append("unexpected-remote:" + " ".join(command))
                return ""

        options = dataclasses.replace(
            runner_options(), target="ssh", ssh_machine_name="synara-stage3-create-failed"
        )
        driver = CreateFailureDriver(
            pathlib.Path.cwd(), options, acceptance.Deadline(30.0), acceptance.SecretRedactor()
        )

        with self.assertRaises(acceptance.AcceptanceError):
            driver._prepare_machine()

        self.assertTrue(driver.machine_create_attempted)
        self.assertFalse(driver.machine_created)
        create = next(event for event in events if event.startswith("command:create"))
        self.assertIn("--arch arm64", create)
        self.assertIn(f"--user {acceptance.SSH_SERVICE_USER}", create)
        self.assertIn("--isolated ubuntu:24.04 synara-stage3-create-failed", create)
        driver.cleanup()
        self.assertFalse(any(event.startswith("cleanup:delete") for event in events))
        self.assertFalse(any(event.startswith("unexpected-remote:") for event in events))

    def test_worker_proxy_relay_uses_runner_owned_ssh_args_and_stops_cleanly(self) -> None:
        events: list[str] = []

        class FakeThread:
            @staticmethod
            def is_alive() -> bool:
                return True

        class FakeWorkerProxy:
            port = 43123
            thread = FakeThread()

        class RelayProcess:
            def __init__(self) -> None:
                self.pid = 4321
                self.returncode: int | None = None

            def poll(self) -> int | None:
                return self.returncode

            def wait(self, timeout: float | None = None) -> int:
                del timeout
                self.returncode = 0
                events.append("relay-wait")
                return 0

        class RelayDriver(acceptance.SSHDriver):
            def _spawn_worker_proxy_relay(
                self,
                command: Sequence[str],
                log_handle: Any,
            ) -> RelayProcess:
                del log_handle
                events.append("spawn:" + " ".join(command))
                return RelayProcess()

        options = dataclasses.replace(runner_options(), target="ssh")
        driver = RelayDriver(pathlib.Path.cwd(), options, acceptance.Deadline(30.0), acceptance.SecretRedactor())
        self.addCleanup(driver._release_state)
        driver.credentials_dir.mkdir(parents=True, exist_ok=True)
        driver.client_key_path.write_text("private-key", encoding="utf-8")
        driver.machine_created = True
        driver.machine_ip = "192.0.2.10"
        driver.host_key = self._key(b"trusted-host-key")
        driver.worker_proxy = FakeWorkerProxy()  # type: ignore[assignment]

        with mock.patch.object(acceptance, "reserve_loopback_port", return_value=41234), mock.patch.object(
            acceptance.os,
            "killpg",
            side_effect=lambda pid, sig: events.append(f"killpg:{pid}:{sig}"),
        ):
            driver._start_worker_proxy_relay()
            evidence = driver._worker_proxy_relay_evidence()
            known_hosts = driver.known_hosts_path.read_text(encoding="utf-8")
            driver._stop_worker_proxy_relay()

        spawn = next(event for event in events if event.startswith("spawn:"))
        self.assertIn("ssh -F /dev/null", spawn)
        self.assertIn("IdentityAgent=none", spawn)
        self.assertIn("StrictHostKeyChecking=yes", spawn)
        self.assertIn("GlobalKnownHostsFile=/dev/null", spawn)
        self.assertIn(f"UserKnownHostsFile={driver.known_hosts_path}", spawn)
        self.assertIn("-R 127.0.0.1:41234:127.0.0.1:43123", spawn)
        self.assertIn("root@192.0.2.10", spawn)
        self.assertEqual(known_hosts, f"192.0.2.10 {driver.host_key}\n")
        self.assertEqual(evidence["mode"], "reverse-ssh-loopback")
        self.assertEqual(evidence["vmListenPort"], 41234)
        self.assertTrue(any(event.startswith("killpg:4321:") for event in events))
        self.assertIn("relay-wait", events)
        self.assertFalse(driver.known_hosts_path.exists())

    def test_generated_key_evidence_describes_local_deletion_and_encrypted_lifecycle(self) -> None:
        class KeyEvidenceDriver(acceptance.SSHDriver):
            def _local_command(
                self,
                arguments: Sequence[str],
                *,
                cwd: pathlib.Path,
                environment: Mapping[str, str],
                log_path: pathlib.Path,
                maximum_timeout: float,
                error_code: str,
                description: str,
            ) -> None:
                del arguments, cwd, environment, log_path, maximum_timeout, error_code, description
                self.credentials_dir.mkdir(parents=True, exist_ok=True)
                self.client_key_path.write_text(
                    "-----BEGIN OPENSSH PRIVATE KEY-----\nfixture-private-key\n-----END OPENSSH PRIVATE KEY-----\n",
                    encoding="utf-8",
                )
                self.client_public_key_path.write_text(SSHDriverTest._key(b"generated-host-key"), encoding="utf-8")

        driver = KeyEvidenceDriver(
            pathlib.Path.cwd(),
            dataclasses.replace(runner_options(), target="ssh"),
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        self.addCleanup(driver._discard_local_key_material)
        self.addCleanup(driver._release_state)

        evidence = driver._generate_client_key()

        self.assertTrue(evidence["localPrivateKeyPlaintextDeletedAfterProvision"])
        self.assertEqual(evidence["controlPlaneCredentialLifecycle"], acceptance.SSH_CREDENTIAL_LIFECYCLE)
        self.assertNotIn("privateKeyPersistedAfterProvision", evidence)

    def test_machine_setup_script_creates_run_sshd_before_validation_and_restart(self) -> None:
        driver = acceptance.SSHDriver(
            pathlib.Path.cwd(),
            dataclasses.replace(runner_options(), target="ssh"),
            acceptance.Deadline(30.0),
            acceptance.SecretRedactor(),
        )
        self.addCleanup(driver._release_state)

        script_lines = driver._machine_setup_script().splitlines()

        run_sshd_index = script_lines.index("install -d -o root -g root -m 0755 /run/sshd")
        validate_index = script_lines.index("sshd -t")
        restart_index = script_lines.index("systemctl restart ssh")
        self.assertLess(run_sshd_index, validate_index)
        self.assertLess(validate_index, restart_index)


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
