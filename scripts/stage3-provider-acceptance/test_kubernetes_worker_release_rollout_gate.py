from __future__ import annotations

import dataclasses
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
        self.assertEqual(runner.kind_worker_nodes, 2)
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
        self.assertEqual(calls[1]["image"], driver.images["candidate"].exact_reference)
        self.assertEqual(calls[1]["max_active_pods"], 1)
        self.assertEqual(evidence["mainTarget"]["id"], "main-target")
        self.assertEqual(evidence["observerTarget"]["id"], "observer-target")


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
                mock.patch.object(rollout, "validate_owned_release_image", return_value=True),
                mock.patch.object(driver, "_require_docker_resource_owner"),
                mock.patch.object(driver, "_docker_completed", side_effect=completed),
            ):
                evidence = driver.cleanup()

        self.assertTrue(evidence["workerImagesRemoved"])
        self.assertTrue(evidence["registryContainerRemoved"])
        self.assertTrue(evidence["registryVolumeRemoved"])
        self.assertFalse(evidence["broadCleanupUsed"])
        self.assertFalse(evidence["registryBaseImageRemoved"])
        self.assertFalse(any(command and command[0] in {"prune", "system"} for command in commands))


if __name__ == "__main__":
    unittest.main()
