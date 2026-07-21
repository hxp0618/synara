from __future__ import annotations

import dataclasses
import json
import pathlib
import subprocess
import tempfile
import unittest
from typing import Any
from unittest import mock

import acceptance_runner as acceptance
import kubernetes_worker_release_rollout_gate as gate
import worker_release_rollout_common as rollout


DIGEST_A = "sha256:" + "a" * 64
DIGEST_B = "sha256:" + "b" * 64
IMAGE_ID_A = "sha256:" + "c" * 64
IMAGE_ID_B = "sha256:" + "d" * 64


def gate_options(output_dir: pathlib.Path) -> gate.GateOptions:
    return gate.GateOptions(
        repo_root=pathlib.Path(__file__).resolve().parents[2],
        output_dir=output_dir,
        timeout_seconds=1200.0,
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
    def test_defaults_require_multi_node_owned_kind(self) -> None:
        options = gate.parse_args([])

        self.assertEqual(options.kind_worker_nodes, 2)
        self.assertEqual(options.load_waves, gate.DEFAULT_LOAD_WAVES)
        self.assertEqual(options.registry_image, gate.DEFAULT_REGISTRY_IMAGE)
        self.assertEqual(options.kind_node_image, "kindest/node:v1.33.1")

    def test_rejects_single_worker_and_credential_bearing_registry_reference(self) -> None:
        with self.assertRaises(SystemExit):
            gate.parse_args(["--kind-worker-nodes", "1"])
        with self.assertRaises(SystemExit):
            gate.parse_args(["--registry-image", "https://registry.invalid/image"])

    def test_runner_options_pin_existing_placeholder_without_local_kind_load(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            options = gate_options(pathlib.Path(temporary))
            runner = gate.runner_options(options)

        self.assertEqual(runner.target, "kubernetes")
        self.assertEqual(runner.suite, "fixture")
        self.assertEqual(runner.load_waves, gate.DEFAULT_LOAD_WAVES)
        self.assertEqual(runner.kind_worker_nodes, 2)
        self.assertEqual(runner.worker_lease_ttl, gate.ROLLOUT_WORKER_LEASE_TTL)
        self.assertEqual(
            runner.worker_heartbeat_timeout,
            gate.ROLLOUT_WORKER_HEARTBEAT_TIMEOUT,
        )
        self.assertTrue(runner.kubernetes_skip_worker_build)
        self.assertEqual(
            runner.kubernetes_worker_image,
            "localhost.invalid/synara/worker-rollout:placeholder",
        )


class DriverConfigurationTest(unittest.TestCase):
    def build_driver(self) -> gate.KubernetesWorkerReleaseRolloutDriver:
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        options = gate_options(pathlib.Path(self.temporary.name))
        driver = gate.KubernetesWorkerReleaseRolloutDriver(
            options,
            gate.runner_options(options),
            acceptance.Deadline(1200.0),
            acceptance.SecretRedactor(),
        )
        self.addCleanup(driver._release_state)
        return driver

    def test_kind_configuration_uses_exact_localhost_mirror_and_two_workers(self) -> None:
        driver = self.build_driver()

        configuration = driver._kind_cluster_configuration()

        assert configuration is not None
        nodes = configuration["nodes"]
        self.assertEqual([node["role"] for node in nodes], ["control-plane", "worker", "worker"])
        patches = configuration["containerdConfigPatches"]
        self.assertEqual(len(patches), 1)
        self.assertIn(f'"localhost:{driver.registry_port}"', patches[0])
        self.assertIn(f'http://{driver.registry_container_name}:5000', patches[0])
        self.assertEqual(driver.image_pull_policy, "Always")

    def test_rollout_gate_uses_a_dedicated_non_failure_matrix_lease_window(self) -> None:
        driver = self.build_driver()

        environment = driver._control_plane_environment()

        self.assertEqual(
            environment["SYNARA_WORKER_LEASE_TTL"],
            gate.ROLLOUT_WORKER_LEASE_TTL,
        )
        self.assertEqual(
            environment["SYNARA_WORKER_HEARTBEAT_TIMEOUT"],
            gate.ROLLOUT_WORKER_HEARTBEAT_TIMEOUT,
        )
        self.assertNotEqual(gate.ROLLOUT_WORKER_LEASE_TTL, "6s")

    def test_provision_targets_use_same_repository_and_distinct_digests(self) -> None:
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
                "codex",
            )

        self.assertEqual(calls[0]["image"], driver.images["baseline"].exact_reference)
        self.assertEqual(calls[0]["max_active_pods"], 2)
        self.assertEqual(
            calls[0]["experimental_providers"],
            acceptance.FIXTURE_CONCURRENCY_PROVIDERS,
        )
        self.assertEqual(calls[1]["image"], driver.images["candidate"].exact_reference)
        self.assertEqual(calls[1]["max_active_pods"], 1)
        self.assertEqual(
            calls[1]["experimental_providers"],
            acceptance.FIXTURE_CONCURRENCY_PROVIDERS,
        )
        self.assertEqual(evidence["mainTarget"]["id"], "main-target")
        self.assertEqual(evidence["observerTarget"]["id"], "observer-target")


class ReleaseLoadTopologyTest(unittest.TestCase):
    def test_execution_pinned_load_requires_one_worker_identity_per_execution(self) -> None:
        suite = object.__new__(gate.KubernetesWorkerReleaseRolloutSuite)
        suite._fixture_load_session_cache = []
        parent_evidence = {"activeRuntimeChecks": 12}

        with mock.patch.object(
            rollout.WorkerReleaseAcceptanceSuite,
            "_release_load_waves",
            return_value=parent_evidence,
        ) as parent:
            evidence = suite._release_load_waves(
                slot="candidate",
                channel="promoted",
                wave_start=0,
                wave_count=3,
            )

        parent.assert_called_once_with(
            slot="candidate",
            channel="promoted",
            wave_start=0,
            wave_count=3,
            require_runtime_evidence=True,
            expected_distinct_workers=12,
        )
        self.assertEqual(evidence["podResourceProfileChecks"], 12)


class ReleasePodObservationTest(unittest.TestCase):
    def test_release_observation_requires_exact_digest_labels_and_runtime_image_id(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            options = gate_options(pathlib.Path(temporary))
            runner = gate.runner_options(options)

            class ObservationDriver(gate.KubernetesWorkerReleaseRolloutDriver):
                def _wait_execution_pod(self, target_id: str, execution_id: str) -> dict[str, Any]:
                    compact = target_id.replace("-", "")[:12]
                    return {
                        "metadata": {
                            "name": "synara-exec-release",
                            "uid": "pod-uid-release",
                            "labels": {
                                "synara.io/managed": "true",
                                "synara.io/execution-target-id": target_id,
                                "synara.io/execution-id": execution_id,
                                "synara.io/generation": "1",
                                "synara.io/worker-release-revision-id": "revision-id",
                                "synara.io/worker-release-channel": "canary",
                            },
                        },
                        "spec": {
                            "nodeName": "worker-node-1",
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
                                    "image": f"localhost:55091/synara/worker@{DIGEST_B}",
                                    "imagePullPolicy": "Always",
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
                                            "name": "SYNARA_AGENTD_IMAGE_DIGEST",
                                            "value": DIGEST_B,
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
                        "status": {
                            "phase": "Running",
                            "containerStatuses": [
                                {
                                    "name": "agentd",
                                    "imageID": f"docker-pullable://localhost:55091/synara/worker@{DIGEST_B}",
                                }
                            ],
                        },
                    }

                def _foundation_evidence(
                    self,
                    target_id: str,
                    secret_name: Any,
                    **_kwargs: Any,
                ) -> dict[str, Any]:
                    del target_id, secret_name
                    return {"serviceAccount": self.worker_service_account}

            driver = ObservationDriver(
                options,
                runner,
                acceptance.Deadline(1200.0),
                acceptance.SecretRedactor(),
            )
            self.addCleanup(driver._release_state)
            target_id = "11111111-2222-3333-4444-555555555555"
            driver._remember_target_runtime(
                target_id,
                namespace=driver.target_namespace,
                service_account=driver.worker_service_account,
                image=f"localhost:55091/synara/worker@{DIGEST_A}",
            )

            evidence = driver.observe_release_execution(
                target_id,
                "execution-id",
                expected_image=f"localhost:55091/synara/worker@{DIGEST_B}",
                expected_release_revision_id="revision-id",
                expected_release_channel="canary",
            )

        self.assertEqual(evidence["imageDigest"], DIGEST_B)
        self.assertEqual(evidence["workerReleaseRevisionId"], "revision-id")
        self.assertEqual(evidence["workerReleaseChannel"], "canary")
        self.assertEqual(evidence["nodeName"], "worker-node-1")


class PodPreservationTest(unittest.TestCase):
    def test_busy_pod_identity_must_remain_exact(self) -> None:
        pod = {
            "podName": "pod",
            "podUid": "uid",
            "generation": "1",
            "image": f"repository@{DIGEST_A}",
            "imageDigest": DIGEST_A,
            "containerImageId": f"docker-pullable://repository@{DIGEST_A}",
            "imagePullPolicy": "Always",
            "workerReleaseRevisionId": "revision",
            "workerReleaseChannel": "promoted",
            "nodeName": "worker-1",
        }

        evidence = gate.KubernetesWorkerReleaseRolloutSuite._validate_pod_preserved(pod, dict(pod))
        self.assertTrue(evidence["preserved"])

        changed = dict(pod)
        changed["podUid"] = "replacement"
        with self.assertRaises(acceptance.AcceptanceError):
            gate.KubernetesWorkerReleaseRolloutSuite._validate_pod_preserved(pod, changed)


class ResourceProfileTest(unittest.TestCase):
    def test_pod_and_resource_quota_must_match_bounded_profile(self) -> None:
        configuration = acceptance.KUBERNETES_ACCEPTANCE_RESOURCE_CONFIGURATION
        pod = {
            "metadata": {
                "name": "pod",
                "uid": "pod-uid",
                "labels": {
                    "synara.io/execution-id": "execution-id",
                    "synara.io/generation": "1",
                },
            },
            "spec": {
                "nodeName": "worker-1",
                "containers": [
                    {
                        "name": "agentd",
                        "resources": {
                            "requests": {
                                "cpu": configuration["cpuRequest"],
                                "memory": configuration["memoryRequest"],
                                "ephemeral-storage": configuration[
                                    "ephemeralStorageRequest"
                                ],
                            },
                            "limits": {
                                "cpu": configuration["cpuLimit"],
                                "memory": configuration["memoryLimit"],
                                "ephemeral-storage": configuration[
                                    "ephemeralStorageLimit"
                                ],
                            },
                        },
                    }
                ],
                "volumes": [
                    {
                        "name": "workspace",
                        "emptyDir": {
                            "sizeLimit": configuration["workspaceSizeLimit"]
                        },
                    }
                ],
            },
        }
        quota = {
            "metadata": {"name": "synara-agentd-target"},
            "spec": {
                "hard": {
                    "pods": "2",
                    "requests.cpu": configuration["quotaCpuRequests"],
                    "limits.cpu": configuration["quotaCpuLimits"],
                    "requests.memory": configuration["quotaMemoryRequests"],
                    "limits.memory": configuration["quotaMemoryLimits"],
                    "requests.ephemeral-storage": configuration[
                        "quotaEphemeralStorage"
                    ],
                }
            },
            "status": {"used": {"pods": "1"}},
        }

        evidence = gate.validate_kubernetes_resource_profile(
            pod,
            quota,
            max_active_pods=2,
            configuration=configuration,
        )

        self.assertEqual(evidence["requests"]["cpu"], "100m")
        self.assertEqual(evidence["quota"]["hard"]["pods"], "2")
        drifted = dict(pod)
        drifted_spec = dict(pod["spec"])
        drifted_container = dict(pod["spec"]["containers"][0])
        drifted_resources = dict(drifted_container["resources"])
        drifted_requests = dict(drifted_resources["requests"])
        drifted_requests["cpu"] = "50m"
        drifted_resources["requests"] = drifted_requests
        drifted_container["resources"] = drifted_resources
        drifted_spec["containers"] = [drifted_container]
        drifted["spec"] = drifted_spec
        with self.assertRaises(acceptance.AcceptanceError):
            gate.validate_kubernetes_resource_profile(
                drifted,
                quota,
                max_active_pods=2,
                configuration=configuration,
            )


class PodRecoveryTest(unittest.TestCase):
    def test_recovered_pending_approval_carries_replacement_generation(self) -> None:
        recovered = gate.KubernetesWorkerReleaseRolloutSuite._recovered_pending_approval(
            {
                "interaction": {"id": "stale-interaction"},
                "requestId": "stale-request",
                "replacementGeneration": 99,
            },
            {
                "id": "replacement-interaction",
                "requestId": "replacement-request",
            },
            {"replacementGeneration": 2},
        )

        self.assertEqual(recovered["replacementGeneration"], 2)
        self.assertEqual(recovered["requestId"], "replacement-request")
        self.assertEqual(recovered["interaction"]["id"], "replacement-interaction")

    def test_recovered_pending_approval_requires_positive_replacement_generation(self) -> None:
        for invalid_generation in (None, 0, -1, True, "2"):
            with self.subTest(replacement_generation=invalid_generation):
                with self.assertRaises(acceptance.AcceptanceError) as raised:
                    gate.KubernetesWorkerReleaseRolloutSuite._recovered_pending_approval(
                        {"interaction": {"id": "stale-interaction"}},
                        {
                            "id": "replacement-interaction",
                            "requestId": "replacement-request",
                        },
                        {"replacementGeneration": invalid_generation},
                    )

                self.assertEqual(
                    raised.exception.code,
                    "runner.kubernetes_rollout_replacement_generation_invalid",
                )

    def test_candidate_recovery_preserves_release_and_advances_generation(self) -> None:
        image = release_image("candidate", DIGEST_B, IMAGE_ID_B)
        before_pod = {
            "podName": "candidate-1",
            "podUid": "candidate-uid-1",
            "generation": "1",
            "image": image.exact_reference,
            "imageDigest": image.digest,
            "containerImageId": f"docker-pullable://repository@{image.digest}",
            "imagePullPolicy": "Always",
            "workerReleaseRevisionId": "candidate-revision",
            "workerReleaseChannel": "canary",
            "serviceAccountName": "worker",
            "nodeName": "worker-1",
        }
        after_pod = {
            **before_pod,
            "podName": "candidate-2",
            "podUid": "candidate-uid-2",
            "generation": "2",
            "nodeName": "worker-2",
        }
        release = {
            "executionId": "candidate-execution",
            "workerManifestId": "candidate-manifest",
            "workerReleaseRevisionId": "candidate-revision",
            "workerReleaseChannel": "canary",
        }
        recovery = {
            "staleExecutionId": "candidate-execution",
            "replacementExecutionId": "candidate-execution",
            "staleWorkerId": "worker-id-1",
            "replacementWorkerId": "worker-id-2",
            "staleGeneration": 1,
            "replacementGeneration": 2,
            "targetRecovery": {
                "recoveryMode": "delete-pod",
                "deletedPodUid": "candidate-uid-1",
                "deletedPodGeneration": "1",
            },
            "targetRuntime": {
                "podUid": "candidate-uid-2",
                "generation": "2",
            },
        }

        evidence = gate.validate_release_pod_recovery(
            before_pod,
            after_pod,
            before_release=release,
            after_release=release,
            recovery=recovery,
            revision_id="candidate-revision",
            channel="canary",
            manifest_id="candidate-manifest",
            image=image,
        )

        self.assertEqual(evidence["generation"], {"before": 1, "after": 2})
        self.assertTrue(evidence["immutableReleasePreserved"])
        with self.assertRaises(acceptance.AcceptanceError):
            gate.validate_release_pod_recovery(
                before_pod,
                {**after_pod, "imageDigest": DIGEST_A},
                before_release=release,
                after_release=release,
                recovery=recovery,
                revision_id="candidate-revision",
                channel="canary",
                manifest_id="candidate-manifest",
                image=image,
            )


class CleanupTest(unittest.TestCase):
    def test_cleanup_removes_only_owned_registry_images_and_volume(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            options = gate_options(pathlib.Path(temporary))
            driver = gate.KubernetesWorkerReleaseRolloutDriver(
                options,
                gate.runner_options(options),
                acceptance.Deadline(1200.0),
                acceptance.SecretRedactor(),
            )
            self.addCleanup(driver._release_state)
            driver.images = {
                "baseline": release_image("baseline", DIGEST_A, IMAGE_ID_A),
                "candidate": release_image("candidate", DIGEST_B, IMAGE_ID_B),
            }
            driver.registry_created = True
            driver.registry_volume_created = True
            commands: list[list[str]] = []

            def completed(arguments: list[str], **_kwargs: Any) -> subprocess.CompletedProcess[str]:
                commands.append(arguments)
                if arguments[:2] in (["image", "inspect"], ["container", "inspect"], ["volume", "inspect"]):
                    return subprocess.CompletedProcess(arguments, 1, "", "")
                return subprocess.CompletedProcess(arguments, 0, "ok\n", "")

            with (
                mock.patch.object(acceptance.KubernetesDriver, "cleanup", return_value={"ownedClusterRemoved": True}),
                mock.patch.object(rollout, "validate_owned_release_image", return_value=True) as validate_image,
                mock.patch.object(driver, "_require_docker_resource_owner"),
                mock.patch.object(driver, "_docker_completed", side_effect=completed),
            ):
                evidence = driver.cleanup()

        self.assertTrue(evidence["workerImagesRemoved"])
        self.assertTrue(evidence["registryContainerRemoved"])
        self.assertTrue(evidence["registryVolumeRemoved"])
        self.assertFalse(evidence["broadCleanupUsed"])
        self.assertFalse(evidence["registryBaseImageRemoved"])
        self.assertEqual(
            [call.kwargs["cleanup_timeout"] for call in validate_image.call_args_list],
            [10.0, 10.0],
        )
        self.assertFalse(any(command and command[0] in {"prune", "system"} for command in commands))

    def test_cleanup_still_removes_owned_images_after_main_deadline_expires(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            options = gate_options(pathlib.Path(temporary))
            driver = gate.KubernetesWorkerReleaseRolloutDriver(
                options,
                gate.runner_options(options),
                acceptance.Deadline(0.0),
                acceptance.SecretRedactor(),
            )
            self.addCleanup(driver._release_state)
            driver.images = {
                "baseline": release_image("baseline", DIGEST_A, IMAGE_ID_A),
                "candidate": release_image("candidate", DIGEST_B, IMAGE_ID_B),
            }
            commands: list[tuple[list[str], float | None]] = []
            labels_by_image = {
                IMAGE_ID_A: json.dumps(
                    {
                        rollout.OWNER_LABEL: driver.resource_owner,
                        rollout.SLOT_LABEL: "baseline",
                    }
                ),
                IMAGE_ID_B: json.dumps(
                    {
                        rollout.OWNER_LABEL: driver.resource_owner,
                        rollout.SLOT_LABEL: "candidate",
                    }
                ),
            }

            def completed(
                arguments: list[str],
                *,
                cleanup_timeout: float | None = None,
                **_kwargs: Any,
            ) -> subprocess.CompletedProcess[str]:
                commands.append((arguments, cleanup_timeout))
                if arguments[:3] == ["image", "inspect", "--format"]:
                    image_id = arguments[-1]
                    return subprocess.CompletedProcess(arguments, 0, labels_by_image[image_id], "")
                if arguments[:3] == ["image", "rm", "-f"]:
                    return subprocess.CompletedProcess(arguments, 0, "removed\n", "")
                if arguments[:2] == ["image", "inspect"]:
                    return subprocess.CompletedProcess(arguments, 1, "", "")
                raise AssertionError(f"unexpected docker cleanup command: {arguments}")

            with (
                mock.patch.object(acceptance.KubernetesDriver, "cleanup", return_value={"ownedClusterRemoved": True}),
                mock.patch.object(driver, "_docker_completed", side_effect=completed),
            ):
                evidence = driver.cleanup()

        self.assertTrue(evidence["workerImagesRemoved"])
        self.assertEqual(evidence["removedImageSlots"], ["baseline", "candidate"])
        self.assertEqual(
            commands,
            [
                (
                    ["image", "inspect", "--format", "{{json .Config.Labels}}", IMAGE_ID_A],
                    10.0,
                ),
                (["image", "rm", "-f", IMAGE_ID_A], 60.0),
                (["image", "inspect", IMAGE_ID_A], 10.0),
                (
                    ["image", "inspect", "--format", "{{json .Config.Labels}}", IMAGE_ID_B],
                    10.0,
                ),
                (["image", "rm", "-f", IMAGE_ID_B], 60.0),
                (["image", "inspect", IMAGE_ID_B], 10.0),
            ],
        )

    def test_cleanup_refuses_image_removal_when_owned_labels_do_not_match(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            options = gate_options(pathlib.Path(temporary))
            driver = gate.KubernetesWorkerReleaseRolloutDriver(
                options,
                gate.runner_options(options),
                acceptance.Deadline(0.0),
                acceptance.SecretRedactor(),
            )
            self.addCleanup(driver._release_state)
            driver.images = {
                "baseline": release_image("baseline", DIGEST_A, IMAGE_ID_A),
            }
            commands: list[list[str]] = []

            def completed(
                arguments: list[str],
                *,
                cleanup_timeout: float | None = None,
                **_kwargs: Any,
            ) -> subprocess.CompletedProcess[str]:
                del cleanup_timeout
                commands.append(arguments)
                if arguments[:3] == ["image", "inspect", "--format"]:
                    labels = {
                        rollout.OWNER_LABEL: "different-owner",
                        rollout.SLOT_LABEL: "baseline",
                    }
                    return subprocess.CompletedProcess(
                        arguments,
                        0,
                        json.dumps(labels),
                        "",
                    )
                raise AssertionError(f"unexpected Docker cleanup command: {arguments}")

            with (
                mock.patch.object(
                    acceptance.KubernetesDriver,
                    "cleanup",
                    return_value={"ownedClusterRemoved": True},
                ),
                mock.patch.object(driver, "_docker_completed", side_effect=completed),
            ):
                with self.assertRaises(acceptance.AcceptanceError) as caught:
                    driver.cleanup()

        self.assertEqual(
            caught.exception.code,
            "runner.kubernetes_worker_release_rollout_cleanup_failed",
        )
        self.assertEqual(
            commands,
            [
                [
                    "image",
                    "inspect",
                    "--format",
                    "{{json .Config.Labels}}",
                    IMAGE_ID_A,
                ]
            ],
        )
        self.assertFalse(any(command[:3] == ["image", "rm", "-f"] for command in commands))


if __name__ == "__main__":
    unittest.main()
