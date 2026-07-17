#!/usr/bin/env python3
"""Verify registry-pushed immutable Worker rollout and rollback on disposable multi-node Kind."""

from __future__ import annotations

import argparse
import dataclasses
import datetime as dt
import hashlib
import json
import os
import pathlib
import re
import sys
import time
import urllib.error
import urllib.request
import uuid
from collections.abc import Mapping, Sequence
from typing import Any

import acceptance_runner as acceptance
import release_gate_common as release_gate
import worker_release_rollout_common as rollout


SCHEMA_VERSION = "synara.kubernetes-worker-release-rollout-gate.v1"
JSON_REPORT_NAME = "kubernetes-worker-release-rollout-gate.json"
MARKDOWN_REPORT_NAME = "kubernetes-worker-release-rollout-gate.md"
DEFAULT_REGISTRY_IMAGE = "registry:2.8.3"


@dataclasses.dataclass(frozen=True)
class GateOptions:
    repo_root: pathlib.Path
    output_dir: pathlib.Path
    timeout_seconds: float
    skip_build: bool
    control_plane_binary: pathlib.Path | None
    docker_socket_path: pathlib.Path
    kubernetes_control_plane_host: str
    kind_bin: str
    kind_node_image: str
    kind_worker_nodes: int
    registry_image: str
    go_proxy: str | None


def parse_args(argv: Sequence[str]) -> GateOptions:
    repo_root = pathlib.Path(__file__).resolve().parents[2]
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=pathlib.Path)
    parser.add_argument("--timeout", type=float, default=3600.0)
    parser.add_argument("--skip-build", action="store_true")
    parser.add_argument("--control-plane-binary", type=pathlib.Path)
    parser.add_argument(
        "--docker-socket-path",
        type=pathlib.Path,
        default=pathlib.Path("/var/run/docker.sock"),
    )
    parser.add_argument("--kubernetes-control-plane-host", default="host.docker.internal")
    parser.add_argument("--kind-bin", default="kind")
    parser.add_argument("--kind-node-image", default="kindest/node:v1.33.1")
    parser.add_argument("--kind-worker-nodes", type=int, default=2)
    parser.add_argument("--registry-image", default=DEFAULT_REGISTRY_IMAGE)
    parser.add_argument("--go-proxy")
    parsed = parser.parse_args(argv)
    if parsed.timeout <= 0:
        parser.error("--timeout must be positive")
    if parsed.control_plane_binary is not None and not parsed.skip_build:
        parser.error("--control-plane-binary requires --skip-build")
    if parsed.skip_build and parsed.control_plane_binary is None:
        parser.error("--skip-build requires --control-plane-binary")
    if parsed.kind_worker_nodes < 2 or parsed.kind_worker_nodes > 8:
        parser.error("--kind-worker-nodes must be between 2 and 8")
    docker_socket_path = parsed.docker_socket_path.expanduser().resolve()
    if not docker_socket_path.is_absolute():
        parser.error("--docker-socket-path must be absolute")
    control_plane_host = parsed.kubernetes_control_plane_host.strip()
    if not control_plane_host or re.fullmatch(r"[A-Za-z0-9._-]+", control_plane_host) is None:
        parser.error(
            "--kubernetes-control-plane-host must be a hostname or address without scheme or port"
        )
    kind_bin = parsed.kind_bin.strip()
    kind_node_image = parsed.kind_node_image.strip()
    if not kind_bin or any(character in kind_bin for character in "\r\n\t\x00"):
        parser.error("--kind-bin must be a non-empty executable name or path")
    if not kind_node_image or any(character in kind_node_image for character in "\r\n\t\x00"):
        parser.error("--kind-node-image must be a non-empty image reference")
    registry_image = parsed.registry_image.strip()
    if (
        not registry_image
        or len(registry_image) > 512
        or any(character.isspace() or ord(character) < 32 for character in registry_image)
        or any(character in registry_image for character in "?#")
        or "://" in registry_image
    ):
        parser.error("--registry-image must be a credential-free Docker image reference")
    try:
        go_proxy = release_gate.normalize_go_proxy(parsed.go_proxy)
    except ValueError as error:
        parser.error(str(error))
    run_id = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    output_dir = parsed.output_dir or (
        repo_root
        / ".tmp"
        / "stage3-provider-acceptance-results"
        / f"{run_id}-{uuid.uuid4().hex[:8]}-kubernetes-worker-release-rollout"
    )
    return GateOptions(
        repo_root=repo_root,
        output_dir=output_dir.expanduser().resolve(),
        timeout_seconds=parsed.timeout,
        skip_build=parsed.skip_build,
        control_plane_binary=(
            parsed.control_plane_binary.expanduser().resolve()
            if parsed.control_plane_binary is not None
            else None
        ),
        docker_socket_path=docker_socket_path,
        kubernetes_control_plane_host=control_plane_host,
        kind_bin=kind_bin,
        kind_node_image=kind_node_image,
        kind_worker_nodes=parsed.kind_worker_nodes,
        registry_image=registry_image,
        go_proxy=go_proxy,
    )


def runner_options(options: GateOptions) -> acceptance.RunnerOptions:
    arguments = [
        "--target",
        "kubernetes",
        "--provider",
        "codex",
        "--suite",
        "fixture",
        "--output-dir",
        str(options.output_dir),
        "--timeout",
        str(options.timeout_seconds),
        "--docker-socket-path",
        str(options.docker_socket_path),
        "--kubernetes-control-plane-host",
        options.kubernetes_control_plane_host,
        "--kind-bin",
        options.kind_bin,
        "--kind-node-image",
        options.kind_node_image,
        "--kind-worker-nodes",
        str(options.kind_worker_nodes),
        "--kubernetes-worker-image",
        "localhost.invalid/synara/worker-rollout:placeholder",
        "--kubernetes-skip-worker-build",
    ]
    if options.skip_build:
        arguments.extend(
            ["--skip-build", "--control-plane-binary", str(options.control_plane_binary)]
        )
    return acceptance.parse_args(arguments)


class KubernetesWorkerReleaseRolloutDriver(acceptance.KubernetesDriver):
    def __init__(
        self,
        gate_options: GateOptions,
        options: acceptance.RunnerOptions,
        deadline: acceptance.Deadline,
        redactor: acceptance.SecretRedactor,
    ) -> None:
        super().__init__(gate_options.repo_root, options, deadline, redactor)
        suffix = uuid.uuid4().hex[:12]
        self.gate_options = gate_options
        self.target_name = f"stage3-kubernetes-rollout-main-{suffix}"
        self.canary_target_name = f"stage3-kubernetes-rollout-observer-{suffix}"
        self.registry_container_name = f"synara-stage3-kubernetes-registry-{suffix}"
        self.registry_volume_name = f"synara-stage3-kubernetes-registry-{suffix}"
        self.registry_port = acceptance.reserve_loopback_port()
        self.registry_repository = f"localhost:{self.registry_port}/synara/worker-rollout"
        self.registry_created = False
        self.registry_volume_created = False
        self.registry_network_connected = False
        self.images: dict[str, rollout.ReleaseImage] = {}
        self.head_sha = self._head_sha()
        self.owns_image = False

    @property
    def image_pull_policy(self) -> str:
        return "Always"

    def prepare(self) -> Mapping[str, Any]:
        control_plane = acceptance.LocalDriver.prepare(self)
        socket_evidence = self._ping_socket()
        server_version = self._docker_command(
            ["version", "--format", "{{.Server.Version}}"],
            log_path=self.logs_dir / "kubernetes-rollout-docker-version.log",
        ).strip()
        platform = rollout.normalize_engine_platform(
            self._docker_command(
                ["info", "--format", "{{.OSType}}/{{.Architecture}}"],
                log_path=self.logs_dir / "kubernetes-rollout-docker-platform.log",
            ).strip()
        )
        self._start_registry()
        source_version = self._source_version()
        baseline = rollout.build_release_image(
            self,
            repository=self.registry_repository,
            slot="baseline",
            version=rollout.rollout_version(source_version, "baseline"),
            platform=platform,
            owner=self.resource_owner,
            logs_dir=self.logs_dir,
            go_proxy=self.gate_options.go_proxy,
        )
        candidate = rollout.build_release_image(
            self,
            repository=self.registry_repository,
            slot="candidate",
            version=rollout.rollout_version(source_version, "candidate"),
            platform=platform,
            owner=self.resource_owner,
            logs_dir=self.logs_dir,
            go_proxy=self.gate_options.go_proxy,
        )
        if baseline.digest == candidate.digest or baseline.image_id == candidate.image_id:
            raise acceptance.AcceptanceError(
                "runner.worker_release_images_not_distinct",
                "The baseline and candidate Worker images did not produce distinct immutable identities.",
            )
        self.images = {"baseline": baseline, "candidate": candidate}
        self.image = baseline.exact_reference
        self.canary_image = candidate.exact_reference
        cluster_evidence = self._prepare_cluster()
        self._connect_registry_to_kind_network()
        access_evidence = self._prepare_cluster_access()
        return {
            "controlPlane": control_plane,
            "docker": {
                "serverVersion": server_version,
                "platform": platform,
                "socket": socket_evidence,
            },
            "registry": {
                "image": self.gate_options.registry_image,
                "container": self.registry_container_name,
                "repository": self.registry_repository,
                "loopbackOnly": True,
                "storageVolume": self.registry_volume_name,
                "kindNetworkConnected": self.registry_network_connected,
                "authentication": False,
                "tls": False,
            },
            "kubernetes": {
                **cluster_evidence,
                **access_evidence,
                "resourceOwner": self.resource_owner,
                "containerEngine": {
                    "clusterImageTransport": "kind-containerd-registry-mirror",
                    "imagePullPolicy": self.image_pull_policy,
                },
            },
            "images": {
                slot: rollout.release_image_evidence(image)
                for slot, image in self.images.items()
            },
        }

    def _kind_cluster_configuration(self) -> Mapping[str, Any] | None:
        base = super()._kind_cluster_configuration()
        if base is None:
            raise acceptance.AcceptanceError(
                "runner.kubernetes_rollout_topology_invalid",
                "The Kubernetes rollout gate requires explicit Worker nodes.",
            )
        mirror = (
            f'[plugins."io.containerd.grpc.v1.cri".registry.mirrors."localhost:{self.registry_port}"]\n'
            f'  endpoint = ["http://{self.registry_container_name}:5000"]'
        )
        return {**dict(base), "containerdConfigPatches": [mirror]}

    def _start_registry(self) -> None:
        self._docker_command(
            [
                "volume",
                "create",
                "--label",
                f"{rollout.ROLLOUT_LABEL}=true",
                "--label",
                f"{rollout.OWNER_LABEL}={self.resource_owner}",
                self.registry_volume_name,
            ],
            log_path=self.logs_dir / "kubernetes-rollout-registry-volume.log",
        )
        self.registry_volume_created = True
        self._docker_command(
            [
                "run",
                "-d",
                "--name",
                self.registry_container_name,
                "--label",
                f"{rollout.ROLLOUT_LABEL}=true",
                "--label",
                f"{rollout.OWNER_LABEL}={self.resource_owner}",
                "--publish",
                f"127.0.0.1:{self.registry_port}:5000",
                "--volume",
                f"{self.registry_volume_name}:/var/lib/registry",
                self.gate_options.registry_image,
            ],
            log_path=self.logs_dir / "kubernetes-rollout-registry-start.log",
            maximum_timeout=120.0,
        )
        self.registry_created = True

        def registry_probe() -> Mapping[str, Any] | None:
            request = urllib.request.Request(
                f"http://127.0.0.1:{self.registry_port}/v2/",
                headers={"Accept": "application/json"},
            )
            try:
                with urllib.request.urlopen(
                    request,
                    timeout=self.deadline.request_timeout(maximum=2.0),
                ) as response:
                    body = response.read(1024)
                    if int(response.status) != 200:
                        return None
            except (urllib.error.URLError, TimeoutError, OSError):
                return None
            return {"status": 200, "bodySha256": hashlib.sha256(body).hexdigest()}

        self.api.wait_until("the runner-owned loopback Registry", registry_probe, interval=0.2)

    def _connect_registry_to_kind_network(self) -> None:
        self._docker_command(
            ["network", "connect", "kind", self.registry_container_name],
            log_path=self.logs_dir / "kubernetes-rollout-registry-network.log",
        )
        self.registry_network_connected = True

    def provision_rollout_targets(
        self,
        tenant_id: str,
        organization_id: str,
        provider: str,
    ) -> Mapping[str, Any]:
        baseline = self.images["baseline"]
        candidate = self.images["candidate"]
        main = self._create_kubernetes_target(
            tenant_id,
            organization_id,
            provider,
            name=self.target_name,
            namespace=self.target_namespace,
            service_account=self.worker_service_account,
            image=baseline.exact_reference,
            max_active_pods=2,
        )
        observer = self._create_kubernetes_target(
            tenant_id,
            organization_id,
            provider,
            name=self.canary_target_name,
            namespace=self.canary_namespace,
            service_account=self.canary_service_account,
            image=candidate.exact_reference,
            max_active_pods=1,
        )
        main_id = rollout.required_string(main, "id", "main Kubernetes Target")
        observer_id = rollout.required_string(observer, "id", "observer Kubernetes Target")
        self.target_id = main_id
        self.canary_target_id = observer_id
        self._remember_target_runtime(
            main_id,
            namespace=self.target_namespace,
            service_account=self.worker_service_account,
            image=baseline.exact_reference,
        )
        self._remember_target_runtime(
            observer_id,
            namespace=self.canary_namespace,
            service_account=self.canary_service_account,
            image=candidate.exact_reference,
        )
        self._wait_and_label_namespace(self.target_namespace)
        self._wait_and_label_namespace(self.canary_namespace)
        return {
            "mainTarget": acceptance.AcceptanceSuite._target_summary(main),
            "observerTarget": acceptance.AcceptanceSuite._target_summary(observer),
            "mainNamespace": self.target_namespace,
            "observerNamespace": self.canary_namespace,
            "mainMaxActivePods": 2,
            "observerMaxActivePods": 1,
        }

    def cleanup(self) -> Mapping[str, Any]:
        errors: list[str] = []
        base_evidence: Mapping[str, Any] = {}

        try:
            base_evidence = super().cleanup()
        except Exception as error:  # Exact auxiliary cleanup must still run.
            errors.append(f"Kubernetes cleanup: {self.redactor.text(str(error))}")

        removed_images: list[str] = []
        for image in self.images.values():
            try:
                rollout.validate_owned_release_image(
                    self,
                    image,
                    owner=self.resource_owner,
                )
                completed = self._docker_completed(
                    ["image", "rm", "-f", image.image_id], cleanup_timeout=60.0
                )
                if completed.returncode != 0:
                    raise acceptance.AcceptanceError(
                        "runner.kubernetes_rollout_image_cleanup_failed",
                        "A rollout Worker image could not be removed.",
                        {"slot": image.slot, "outputExcerpt": self.redactor.text(completed.stdout)[-1000:]},
                    )
                remaining = self._docker_completed(
                    ["image", "inspect", image.image_id], cleanup_timeout=10.0
                )
                if remaining.returncode == 0:
                    raise acceptance.AcceptanceError(
                        "runner.kubernetes_rollout_image_cleanup_failed",
                        "A rollout Worker image remained after exact removal.",
                        {"slot": image.slot},
                    )
                removed_images.append(image.slot)
            except Exception as error:
                errors.append(f"remove {image.slot} image: {self.redactor.text(str(error))}")

        registry_removed = not self.registry_created
        if self.registry_created:
            try:
                self._require_docker_resource_owner("container", self.registry_container_name)
                completed = self._docker_completed(
                    ["rm", "-f", self.registry_container_name], cleanup_timeout=30.0
                )
                if completed.returncode != 0:
                    raise acceptance.AcceptanceError(
                        "runner.kubernetes_rollout_registry_cleanup_failed",
                        "The rollout Registry container could not be removed.",
                    )
                remaining = self._docker_completed(
                    ["container", "inspect", self.registry_container_name], cleanup_timeout=10.0
                )
                registry_removed = remaining.returncode != 0
                if not registry_removed:
                    raise acceptance.AcceptanceError(
                        "runner.kubernetes_rollout_registry_cleanup_failed",
                        "The rollout Registry container remained after removal.",
                    )
            except Exception as error:
                errors.append(f"remove Registry: {self.redactor.text(str(error))}")

        volume_removed = not self.registry_volume_created
        if self.registry_volume_created:
            try:
                self._require_docker_resource_owner("volume", self.registry_volume_name)
                completed = self._docker_completed(
                    ["volume", "rm", self.registry_volume_name], cleanup_timeout=30.0
                )
                if completed.returncode != 0:
                    raise acceptance.AcceptanceError(
                        "runner.kubernetes_rollout_registry_cleanup_failed",
                        "The rollout Registry storage volume could not be removed.",
                    )
                remaining = self._docker_completed(
                    ["volume", "inspect", self.registry_volume_name], cleanup_timeout=10.0
                )
                volume_removed = remaining.returncode != 0
                if not volume_removed:
                    raise acceptance.AcceptanceError(
                        "runner.kubernetes_rollout_registry_cleanup_failed",
                        "The rollout Registry storage volume remained after removal.",
                    )
            except Exception as error:
                errors.append(f"remove Registry volume: {self.redactor.text(str(error))}")

        if errors:
            raise acceptance.AcceptanceError(
                "runner.kubernetes_worker_release_rollout_cleanup_failed",
                "Kubernetes Worker rollout resources could not be cleaned exactly.",
                {"errors": errors},
            )
        return {
            **dict(base_evidence),
            "workerImagesRemoved": sorted(removed_images) == sorted(self.images),
            "removedImageSlots": sorted(removed_images),
            "registryContainerRemoved": registry_removed,
            "registryVolumeRemoved": volume_removed,
            "registryNetworkAttachmentRemoved": registry_removed,
            "registryBaseImageRemoved": False,
            "broadCleanupUsed": False,
        }

    def _require_docker_resource_owner(self, resource: str, name: str) -> None:
        if resource == "container":
            arguments = [
                "container",
                "inspect",
                "--format",
                f'{{{{ index .Config.Labels "{rollout.OWNER_LABEL}" }}}}',
                name,
            ]
        else:
            arguments = [
                "volume",
                "inspect",
                "--format",
                f'{{{{ index .Labels "{rollout.OWNER_LABEL}" }}}}',
                name,
            ]
        completed = self._docker_completed(arguments, cleanup_timeout=10.0)
        if completed.returncode != 0 or completed.stdout.strip() != self.resource_owner:
            raise acceptance.AcceptanceError(
                "runner.worker_release_resource_not_owned",
                "The rollout gate refused to delete a Docker resource without its exact owner label.",
                {"resource": resource, "name": name},
            )


class KubernetesWorkerReleaseRolloutSuite(rollout.WorkerReleaseAcceptanceSuite):
    driver: KubernetesWorkerReleaseRolloutDriver
    release_description_prefix = "Stage 3 Kubernetes rollout"

    def __init__(
        self,
        options: acceptance.RunnerOptions,
        driver: KubernetesWorkerReleaseRolloutDriver,
        deadline: acceptance.Deadline,
        redactor: acceptance.SecretRedactor,
    ) -> None:
        super().__init__(options, driver, deadline, redactor)
        self.sessions: dict[str, str] = {}
        self.active_baseline: dict[str, Any] | None = None
        self.completed_release_executions: list[dict[str, Any]] = []

    def run(self) -> list[dict[str, Any]]:
        self._case(
            "environment.target-prepare",
            "Build and push two immutable images, then create a multi-node Kind Registry mirror",
            self.driver.prepare,
        )
        self._case(
            "environment.control-plane-start",
            "Start the isolated Control Plane and Worker-only proxy",
            self.driver.start,
            requires=("environment.target-prepare",),
        )
        self._case(
            "identity.dev-login",
            "Authenticate through dev-login",
            self._dev_login,
            requires=("environment.control-plane-start",),
        )
        self._case(
            "runtime.rollout-targets",
            "Provision main and observer Kubernetes Targets against the same Registry repository",
            self._provision_rollout_targets,
            requires=("identity.dev-login",),
        )
        self._case(
            "resources.credential-project-sessions",
            "Create one fixture Credential, one Project, and isolated rollout Sessions",
            self._create_rollout_resources,
            requires=("runtime.rollout-targets",),
        )
        self._case(
            "release.seed-manifests",
            "Pull both exact digests through Kind and capture baseline/candidate Worker Manifests",
            self._seed_release_manifests,
            requires=("resources.credential-project-sessions",),
        )
        self._case(
            "release.revisions",
            "Create two immutable Release Revisions and reject duplicate registration",
            self._create_release_revisions,
            requires=("release.seed-manifests",),
        )
        self._case(
            "release.initial-promote",
            "Promote the baseline Revision with strict policy CAS",
            self._initial_promote,
            requires=("release.revisions",),
        )
        self._case(
            "release.baseline-active",
            "Hold one exact baseline Pod and verify its Revision, Channel, Manifest, and digest",
            self._begin_baseline_active,
            requires=("release.initial-promote",),
        )
        self._case(
            "release.canary-overlap",
            "Run a 100% candidate canary beside the busy baseline and fence unsafe transitions",
            self._canary_overlap,
            requires=("release.baseline-active",),
        )
        self._case(
            "release.promote",
            "Promote candidate and complete an exact candidate-digest Kubernetes Execution",
            self._promote_candidate,
            requires=("release.canary-overlap",),
        )
        self._case(
            "release.rollback",
            "Roll back to baseline and complete an exact baseline-digest Kubernetes Execution",
            self._rollback_baseline,
            requires=("release.promote",),
        )
        self._case(
            "release.history-audit-outbox",
            "Verify immutable history, Audit, Outbox, Session sequencing, and single terminal outcomes",
            self._history_audit_outbox,
            requires=("release.rollback",),
        )
        return self.cases

    def _provision_rollout_targets(self) -> Mapping[str, Any]:
        evidence = self.driver.provision_rollout_targets(
            self._required("tenant_id"),
            self._required("organization_id"),
            self.options.provider,
        )
        main_id = rollout.required_string(evidence["mainTarget"], "id", "main Target")
        observer_id = rollout.required_string(
            evidence["observerTarget"], "id", "observer Target"
        )
        self.state.target_id = main_id
        return {**dict(evidence), "mainTargetId": main_id, "observerTargetId": observer_id}

    def _create_rollout_resources(self) -> Mapping[str, Any]:
        resources = self._create_resources()
        baseline_seed = self._required("session_id")
        self.sessions["baseline-seed"] = baseline_seed
        observer_id = rollout.required_string(
            {"id": self.driver.canary_target_id}, "id", "observer Target"
        )
        session_targets = {
            "candidate-seed": observer_id,
            "baseline-active": self._required("target_id"),
            "candidate-canary": self._required("target_id"),
            "candidate-promoted": self._required("target_id"),
            "baseline-rollback": self._required("target_id"),
        }
        summaries: dict[str, Mapping[str, Any]] = {
            "baseline-seed": {
                "id": baseline_seed,
                "executionTargetId": self._required("target_id"),
            }
        }
        for key, target_id in session_targets.items():
            previous_target = self.state.target_id
            self.state.target_id = target_id
            try:
                session = self._create_project_session(
                    provider=self.options.provider,
                    title=f"Stage 3 Kubernetes Rollout {key}",
                    credential_id=self.state.credential_id,
                    model="stage3-acceptance-fixture",
                    description=f"{key} Session",
                )
            finally:
                self.state.target_id = previous_target
            session_id = rollout.required_string(session, "id", f"{key} Session")
            self.sessions[key] = session_id
            summaries[key] = {
                "id": session_id,
                "executionTargetId": session.get("executionTargetId"),
            }
        self.state.target_id = self.driver.target_id
        self.state.session_id = self.sessions["baseline-active"]
        self.state.last_sequence = 0
        return {
            "credential": resources.get("credential"),
            "project": resources.get("project"),
            "sessions": summaries,
        }

    def _seed_release_manifests(self) -> Mapping[str, Any]:
        stages = (
            ("baseline", "baseline-seed", self.driver.target_id),
            ("candidate", "candidate-seed", self.driver.canary_target_id),
        )
        evidence: dict[str, Any] = {}
        for slot, session_key, target_id_value in stages:
            target_id = rollout.required_string(
                {"id": target_id_value}, "id", f"{slot} seed Target"
            )
            pending = self._start_pending_approval(session_key, target_id)
            execution_id = rollout.required_string(
                pending, "executionId", f"{slot} seed Execution"
            )
            pod = self.driver.observe_execution(target_id, execution_id)
            manifest = self._wait_manifest(
                target_id,
                self.driver.images[slot],
                expected_online=1,
            )
            self.manifests[slot] = manifest
            terminal = self._resolve_pending_approval(pending)
            evidence[slot] = {
                "pod": pod,
                "manifest": rollout.manifest_evidence(manifest),
                "terminal": terminal,
            }
        return evidence

    def _initial_promote(self) -> Mapping[str, Any]:
        policy = self._policy_change(
            "promote",
            "baseline",
            expected_version=0,
            reason="Establish Kubernetes baseline rollout",
        )
        rollout.validate_policy(
            policy,
            expected_version=1,
            promoted_id=self.revisions["baseline"]["id"],
            canary_id=None,
            canary_percent=0,
        )
        return {"policy": rollout.policy_evidence(policy)}

    def _begin_baseline_active(self) -> Mapping[str, Any]:
        active = self._begin_release_execution(
            "baseline-active",
            slot="baseline",
            channel="promoted",
        )
        self.active_baseline = active
        return active

    def _canary_overlap(self) -> Mapping[str, Any]:
        baseline = self.active_baseline
        if baseline is None:
            raise acceptance.AcceptanceError(
                "runner.worker_release_busy_baseline_missing",
                "The baseline Kubernetes Execution was not active before canary rollout.",
            )
        policy = self._policy_change(
            "canary",
            "candidate",
            expected_version=1,
            reason="Start exact-digest Kubernetes candidate canary",
            canary_percent=100,
        )
        rollout.validate_policy(
            policy,
            expected_version=2,
            promoted_id=self.revisions["baseline"]["id"],
            canary_id=self.revisions["candidate"]["id"],
            canary_percent=100,
        )
        baseline_after = self.driver.observe_release_execution(
            self._required("target_id"),
            rollout.required_string(baseline["release"], "executionId", "baseline Execution"),
            expected_image=self.driver.images["baseline"].exact_reference,
            expected_release_revision_id=self.revisions["baseline"]["id"],
            expected_release_channel="promoted",
        )
        preservation = self._validate_pod_preserved(baseline["pod"], baseline_after)
        candidate = self._begin_release_execution(
            "candidate-canary",
            slot="candidate",
            channel="canary",
        )
        if candidate["pod"].get("podUid") == baseline_after.get("podUid"):
            raise acceptance.AcceptanceError(
                "runner.kubernetes_rollout_pod_identity_reused",
                "Baseline and candidate release Executions used the same Pod identity.",
            )
        stale = rollout.expect_problem(
            self.api,
            "POST",
            rollout.release_action_path(
                self._required("tenant_id"),
                self._required("target_id"),
                self.revisions["candidate"]["id"],
                "promote",
            ),
            {"expectedPolicyVersion": 1, "reason": "stale Kubernetes CAS must fail"},
            "worker_release_policy_version_conflict",
        )
        promote_conflict = rollout.expect_problem(
            self.api,
            "POST",
            rollout.release_action_path(
                self._required("tenant_id"),
                self._required("target_id"),
                self.revisions["candidate"]["id"],
                "promote",
            ),
            {
                "expectedPolicyVersion": 2,
                "reason": "busy baseline Kubernetes Pod blocks promotion",
            },
            "worker_release_active_executions",
        )
        rollout.validate_active_execution_conflict(
            promote_conflict,
            revision_id=self.revisions["baseline"]["id"],
            channel="promoted",
        )
        rollback_conflict = rollout.expect_problem(
            self.api,
            "POST",
            rollout.release_action_path(
                self._required("tenant_id"),
                self._required("target_id"),
                self.revisions["baseline"]["id"],
                "rollback",
            ),
            {
                "expectedPolicyVersion": 2,
                "reason": "busy candidate Kubernetes Pod blocks rollback",
            },
            "worker_release_active_executions",
        )
        rollout.validate_active_execution_conflict(
            rollback_conflict,
            revision_id=self.revisions["candidate"]["id"],
            channel="canary",
        )
        candidate_terminal = self._complete_release_execution(candidate)
        baseline_terminal = self._complete_release_execution(baseline)
        self.active_baseline = None
        return {
            "policy": rollout.policy_evidence(policy),
            "baseline": baseline,
            "baselinePreserved": preservation,
            "candidate": candidate,
            "stalePolicy": stale,
            "promoteConflict": promote_conflict,
            "rollbackConflict": rollback_conflict,
            "candidateTerminal": candidate_terminal,
            "baselineTerminal": baseline_terminal,
            "overlap": True,
        }

    def _promote_candidate(self) -> Mapping[str, Any]:
        policy = self._policy_change(
            "promote",
            "candidate",
            expected_version=2,
            reason="Promote exact Kubernetes candidate digest",
        )
        rollout.validate_policy(
            policy,
            expected_version=3,
            promoted_id=self.revisions["candidate"]["id"],
            canary_id=None,
            canary_percent=0,
        )
        execution = self._begin_release_execution(
            "candidate-promoted",
            slot="candidate",
            channel="promoted",
        )
        terminal = self._complete_release_execution(execution)
        return {
            "policy": rollout.policy_evidence(policy),
            "execution": execution,
            "terminal": terminal,
        }

    def _rollback_baseline(self) -> Mapping[str, Any]:
        policy = self._policy_change(
            "rollback",
            "baseline",
            expected_version=3,
            reason="Roll back to exact Kubernetes baseline digest",
        )
        rollout.validate_policy(
            policy,
            expected_version=4,
            promoted_id=self.revisions["baseline"]["id"],
            canary_id=None,
            canary_percent=0,
        )
        execution = self._begin_release_execution(
            "baseline-rollback",
            slot="baseline",
            channel="promoted",
        )
        terminal = self._complete_release_execution(execution)
        return {
            "policy": rollout.policy_evidence(policy),
            "execution": execution,
            "terminal": terminal,
        }

    def _begin_release_execution(
        self,
        session_key: str,
        *,
        slot: str,
        channel: str,
    ) -> dict[str, Any]:
        target_id = self._required("target_id")
        pending = self._start_pending_approval(session_key, target_id)
        turn_id = rollout.required_string(pending, "turnId", f"{session_key} Turn")
        manifest_id = rollout.required_string(
            self.manifests[slot], "manifestId", f"{slot} Worker Manifest"
        )
        revision_id = rollout.required_string(
            self.revisions[slot], "id", f"{slot} Release Revision"
        )
        release = self._wait_execution_release(
            turn_id,
            revision_id=revision_id,
            channel=channel,
            manifest_id=manifest_id,
            terminal=False,
            session_id=pending["sessionId"],
        )
        execution_id = rollout.required_string(release, "executionId", f"{session_key} Execution")
        pod = self.driver.observe_release_execution(
            target_id,
            execution_id,
            expected_image=self.driver.images[slot].exact_reference,
            expected_release_revision_id=revision_id,
            expected_release_channel=channel,
        )
        worker = self._wait_managed_worker(
            release,
            revision_id=revision_id,
            channel=channel,
            manifest_id=manifest_id,
        )
        if worker.get("podName") != pod.get("podName"):
            raise acceptance.AcceptanceError(
                "runner.kubernetes_rollout_worker_pod_mismatch",
                "The managed Worker API and Kubernetes API identified different release Pods.",
                {"workerPodName": worker.get("podName"), "kubernetesPodName": pod.get("podName")},
            )
        return {
            "slot": slot,
            "channel": channel,
            "pending": pending,
            "release": release,
            "pod": pod,
            "worker": worker,
        }

    def _complete_release_execution(self, active: Mapping[str, Any]) -> Mapping[str, Any]:
        pending = acceptance.json_object(active.get("pending"), "release pending approval")
        release = acceptance.json_object(active.get("release"), "release Execution")
        resolution = self._resolve_pending_approval(pending)
        terminal = self._wait_execution_release(
            rollout.required_string(pending, "turnId", "release Turn"),
            revision_id=rollout.required_string(
                {"id": release.get("workerReleaseRevisionId")},
                "id",
                "release Revision",
            ),
            channel=rollout.required_string(
                {"channel": release.get("workerReleaseChannel")},
                "channel",
                "release Channel",
            ),
            manifest_id=rollout.required_string(
                {"manifestId": release.get("workerManifestId")},
                "manifestId",
                "release Manifest",
            ),
            terminal=True,
            session_id=rollout.required_string(pending, "sessionId", "release Session"),
        )
        if terminal.get("executionId") != resolution.get("executionId"):
            raise acceptance.AcceptanceError(
                "runner.kubernetes_rollout_terminal_identity_mismatch",
                "Approval resolution and terminal release evidence identified different Executions.",
            )
        record = {
            "sessionId": pending["sessionId"],
            "turnId": pending["turnId"],
            "executionId": terminal["executionId"],
            "revisionId": terminal["workerReleaseRevisionId"],
            "channel": terminal["workerReleaseChannel"],
            "terminalCount": terminal["terminalCount"],
        }
        self.completed_release_executions.append(record)
        return {"resolution": resolution, "release": terminal}

    def _start_pending_approval(self, session_key: str, target_id: str) -> dict[str, Any]:
        session_id = self.sessions[session_key]
        turn = self._create_turn("[approval]", session_id=session_id)
        turn_id = self._turn_id(turn, f"{session_key} Turn")
        interaction = self._wait_for_interaction(turn_id, "approval", session_id=session_id)
        execution_id, request_id = self._interaction_identity(
            interaction, f"{session_key} Approval"
        )
        return {
            "sessionKey": session_key,
            "sessionId": session_id,
            "targetId": target_id,
            "turn": turn,
            "interaction": interaction,
            "turnId": turn_id,
            "executionId": execution_id,
            "requestId": request_id,
        }

    def _resolve_pending_approval(self, pending: Mapping[str, Any]) -> Mapping[str, Any]:
        session_id = rollout.required_string(pending, "sessionId", "pending Session")
        target_id = rollout.required_string(pending, "targetId", "pending Target")
        turn = acceptance.json_object(pending.get("turn"), "pending Turn")
        interaction = acceptance.json_object(pending.get("interaction"), "pending interaction")
        resolution = self._resolve_approval_turn(
            turn,
            interaction,
            session_id=session_id,
        )
        terminal = self.driver.observe_terminal_execution(
            target_id,
            rollout.required_string(resolution, "executionId", "resolved Execution"),
        )
        return {**dict(resolution), "targetTerminal": terminal}

    @staticmethod
    def _validate_pod_preserved(
        before: Mapping[str, Any], after: Mapping[str, Any]
    ) -> Mapping[str, Any]:
        keys = (
            "podName",
            "podUid",
            "generation",
            "image",
            "imageDigest",
            "containerImageId",
            "imagePullPolicy",
            "workerReleaseRevisionId",
            "workerReleaseChannel",
            "nodeName",
        )
        expected = {key: before.get(key) for key in keys}
        actual = {key: after.get(key) for key in keys}
        if actual != expected:
            raise acceptance.AcceptanceError(
                "runner.kubernetes_rollout_busy_pod_changed",
                "The busy baseline Pod changed while the canary policy was activated.",
                {"expected": expected, "actual": actual},
            )
        return {**actual, "preserved": True}

    def _history_audit_outbox(self) -> Mapping[str, Any]:
        tenant_id = self._required("tenant_id")
        target_id = self._required("target_id")
        overview = acceptance.json_object(
            self.api.request("GET", rollout.release_base_path(tenant_id, target_id)),
            "final Worker Release overview",
        )
        history = rollout.validate_release_overview(
            overview,
            baseline_revision_id=self.revisions["baseline"]["id"],
            candidate_revision_id=self.revisions["candidate"]["id"],
            baseline_digest=self.driver.images["baseline"].digest,
            candidate_digest=self.driver.images["candidate"].digest,
        )
        audits, audit_pagination = rollout.load_all_audit_logs(self.api, tenant_id)
        audit = rollout.validate_release_audit(
            audits,
            target_id=target_id,
            revision_ids={self.revisions["baseline"]["id"], self.revisions["candidate"]["id"]},
        )
        outbox_page = acceptance.json_object(
            self.api.request(
                "GET",
                f"/v1/tenants/{tenant_id}/outbox-messages"
                "?status=all&topicPrefix=worker.release.&limit=200",
            ),
            "Worker Release Outbox page",
        )
        outbox_items = outbox_page.get("items")
        if not isinstance(outbox_items, list) or not all(
            isinstance(item, dict) for item in outbox_items
        ):
            raise acceptance.AcceptanceError(
                "runner.worker_release_outbox_invalid",
                "Worker Release Outbox API returned an invalid item list.",
            )
        outbox = rollout.validate_release_outbox(
            outbox_items,
            target_id=target_id,
            revision_ids={self.revisions["baseline"]["id"], self.revisions["candidate"]["id"]},
        )
        sequence_ranges: dict[str, Mapping[str, Any]] = {}
        execution_ids: list[str] = []
        terminal_counts: dict[str, int] = {}
        for key, session_id in self.sessions.items():
            events = self._all_events(session_id=session_id)
            sequences = [int(event["sequence"]) for event in events]
            expected = list(range(1, sequences[-1] + 1)) if sequences else []
            if sequences != expected:
                raise acceptance.AcceptanceError(
                    "runner.worker_release_session_sequence_discontinuous",
                    "A Kubernetes rollout Session did not retain contiguous Event sequence.",
                    {"sessionKey": key, "sequences": sequences},
                )
            turns = [event for event in events if event.get("eventType") == "turn.created"]
            if len(turns) != 1:
                raise acceptance.AcceptanceError(
                    "runner.kubernetes_rollout_turn_count_invalid",
                    "Each Kubernetes rollout Session must contain exactly one Turn.",
                    {"sessionKey": key, "turnCount": len(turns)},
                )
            execution_id = self._event_execution_id(turns[0])
            terminals = [
                event
                for event in events
                if event.get("executionId") == execution_id
                and event.get("eventType") in acceptance.TERMINAL_EVENT_TYPES
            ]
            if len(terminals) != 1:
                raise acceptance.AcceptanceError(
                    "runner.worker_release_terminal_count_invalid",
                    "A Kubernetes rollout Execution emitted a missing or duplicate terminal event.",
                    {"sessionKey": key, "terminalCount": len(terminals)},
                )
            sequence_ranges[key] = self._sequence_range(events)
            execution_ids.append(execution_id)
            terminal_counts[execution_id] = len(terminals)
        if len(execution_ids) != 6 or len(set(execution_ids)) != 6:
            raise acceptance.AcceptanceError(
                "runner.kubernetes_rollout_execution_count_invalid",
                "The rollout did not retain six distinct seed and release Executions.",
                {"executionCount": len(execution_ids), "distinctCount": len(set(execution_ids))},
            )
        if len(self.completed_release_executions) != 4:
            raise acceptance.AcceptanceError(
                "runner.kubernetes_rollout_release_execution_count_invalid",
                "The rollout did not complete exactly four release-pinned Executions.",
                {"count": len(self.completed_release_executions)},
            )
        return {
            "overview": history,
            "audit": {**audit, "pagination": audit_pagination},
            "outbox": outbox,
            "sessionEvents": {
                "sequenceRanges": sequence_ranges,
                "executionCount": len(execution_ids),
                "distinctExecutionCount": len(set(execution_ids)),
                "terminalCounts": terminal_counts,
                "releaseExecutions": self.completed_release_executions,
                "duplicateTerminal": False,
                "doubleExecution": False,
            },
        }


def markdown_from_report(report: Mapping[str, Any]) -> str:
    lines = [
        "# Stage 3 Kubernetes Worker Release Rollout Gate",
        "",
        f"- Schema: `{report.get('schemaVersion')}`",
        f"- Run: `{report.get('runId')}`",
        f"- Status: **{report.get('status')}**",
        f"- Git SHA: `{report.get('source', {}).get('gitSha')}`",
        f"- Duration: `{report.get('durationMs')} ms`",
        "",
        "## Evidence boundary",
        "",
        "This gate proves registry-pushed exact-digest Worker Revision creation, baseline promotion, 100% canary, "
        "busy-Execution fencing, candidate promotion, baseline rollback, Kubernetes Pod/Worker/Event release pins, "
        "multi-node disposable Kind execution, Audit/Outbox history, Secret scan, and exact owned-resource cleanup. "
        "It does not replace production Registry TLS/auth/retention, production KMS/tlog/admission, real Provider "
        "credentials, cloud CNI/Eviction behavior, production SLA, or production-duration soak evidence.",
        "",
        "## Cases",
        "",
        "| Case | Status | Duration | Reason |",
        "| --- | --- | ---: | --- |",
    ]
    for case in report.get("cases", []):
        if not isinstance(case, dict):
            continue
        reason = str(case.get("reasonCode") or case.get("message") or "").replace("|", "\\|").replace("\n", " ")
        lines.append(
            f"| `{case.get('id', '')}` | {case.get('status', '')} | {case.get('durationMs', 0)} ms | {reason} |"
        )
    lines.extend(["", "## Evidence", ""])
    for case in report.get("cases", []):
        if not isinstance(case, dict):
            continue
        lines.extend([f"### {case.get('id', '')}", ""])
        if case.get("message"):
            lines.extend([str(case["message"]), ""])
        if case.get("evidence"):
            lines.extend(
                [
                    "```json",
                    json.dumps(case["evidence"], indent=2, sort_keys=True, ensure_ascii=False),
                    "```",
                    "",
                ]
            )
    return "\n".join(lines).rstrip() + "\n"


def write_report(
    report: Mapping[str, Any],
    output_dir: pathlib.Path,
    redactor: acceptance.SecretRedactor,
) -> tuple[pathlib.Path, pathlib.Path]:
    output_dir.mkdir(parents=True, exist_ok=True)
    sanitized = redactor.value(dict(report))
    json_path = output_dir / JSON_REPORT_NAME
    markdown_path = output_dir / MARKDOWN_REPORT_NAME
    json_path.write_text(
        json.dumps(sanitized, indent=2, sort_keys=True, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )
    markdown_path.write_text(markdown_from_report(sanitized), encoding="utf-8")
    return json_path, markdown_path


def failure_case(error: Exception, *, started_at: str, started: float) -> dict[str, Any]:
    if isinstance(error, release_gate.ReleaseGateError):
        code = error.code
        evidence = error.evidence
        message = error.message
    elif isinstance(error, acceptance.AcceptanceError):
        code = error.code
        evidence = error.evidence
        message = str(error)
    else:
        code = "runner.internal_error"
        evidence = {"exceptionType": error.__class__.__name__}
        message = str(error) or error.__class__.__name__
    return {
        "id": "environment.preflight",
        "name": "Require a clean source and initialize the Kubernetes rollout gate",
        "status": "fail",
        "startedAt": started_at,
        "finishedAt": acceptance.utc_now(),
        "durationMs": acceptance.elapsed_ms(started),
        "reasonCode": code,
        "message": message,
        "evidence": evidence,
    }


def main(argv: Sequence[str] | None = None) -> int:
    options = parse_args(argv if argv is not None else sys.argv[1:])
    options.output_dir.mkdir(parents=True, exist_ok=True)
    redactor = acceptance.SecretRedactor()
    deadline = acceptance.Deadline(options.timeout_seconds)
    started_at = acceptance.utc_now()
    started = time.monotonic()
    run_id = f"stage3-kubernetes-worker-release-rollout-{uuid.uuid4()}"
    source: Mapping[str, Any] = acceptance.repository_metadata(options.repo_root)
    cases: list[dict[str, Any]] = []
    driver: KubernetesWorkerReleaseRolloutDriver | None = None
    suite: KubernetesWorkerReleaseRolloutSuite | None = None
    try:
        release_gate.repository_state(options.repo_root)
        if os.name != "posix":
            raise acceptance.AcceptanceError(
                "runner.platform_unsupported",
                "The Kubernetes Worker Release rollout gate requires POSIX process groups.",
            )
        child_options = runner_options(options)
        driver = KubernetesWorkerReleaseRolloutDriver(
            options,
            child_options,
            deadline,
            redactor,
        )
        suite = KubernetesWorkerReleaseRolloutSuite(
            child_options,
            driver,
            deadline,
            redactor,
        )
        driver.install_signal_handlers()
        try:
            suite.run()
        except acceptance.RunnerInterrupted as error:
            suite.record_interruption(error)
        finally:
            driver.suppress_signals_for_cleanup()
            try:
                cleanup = driver.cleanup()
                suite.record_cleanup_success(cleanup)
            except acceptance.AcceptanceError as error:
                suite.record_cleanup_failure(error)
            except Exception as error:
                suite.record_cleanup_failure(
                    acceptance.AcceptanceError(
                        "runner.kubernetes_worker_release_rollout_cleanup_failed",
                        "Kubernetes Worker rollout cleanup raised an unexpected error.",
                        {"exceptionType": error.__class__.__name__},
                    )
                )
            finally:
                driver.restore_signal_handlers()
        cases = suite.cases
    except Exception as error:
        if driver is not None and suite is None:
            try:
                driver.cleanup()
            except Exception as cleanup_error:
                error = acceptance.AcceptanceError(
                    "runner.preflight_cleanup_failed",
                    "Kubernetes rollout initialization failed and its exact cleanup also failed.",
                    {
                        "initialExceptionType": error.__class__.__name__,
                        "cleanupExceptionType": cleanup_error.__class__.__name__,
                    },
                )
        cases = [failure_case(error, started_at=started_at, started=started)]
    report: dict[str, Any] = {
        "schemaVersion": SCHEMA_VERSION,
        "runId": run_id,
        "mode": "kubernetes-worker-release-rollout",
        "target": "kubernetes",
        "provider": "codex",
        "source": source,
        "startedAt": started_at,
        "finishedAt": acceptance.utc_now(),
        "durationMs": acceptance.elapsed_ms(started),
        "status": acceptance.aggregate_status(cases),
        "configuration": {
            "timeoutSeconds": options.timeout_seconds,
            "skipControlPlaneBuild": options.skip_build,
            "dockerSocketPath": str(options.docker_socket_path),
            "kubernetesControlPlaneHost": options.kubernetes_control_plane_host,
            "kindBinary": options.kind_bin,
            "kindNodeImage": options.kind_node_image,
            "kindWorkerNodes": options.kind_worker_nodes,
            "registryImage": options.registry_image,
            "registryAuthentication": False,
            "registryTLS": False,
            "mainMaxActivePods": 2,
            "observerMaxActivePods": 1,
            "canaryPercent": 100,
            "broadCleanupAllowed": False,
        },
        "cases": cases,
        "artifacts": {
            "jsonReport": str(options.output_dir / JSON_REPORT_NAME),
            "markdownReport": str(options.output_dir / MARKDOWN_REPORT_NAME),
            "logsDirectory": str(options.output_dir / "logs"),
        },
    }
    write_report(report, options.output_dir, redactor)
    secret_scan = acceptance.output_secret_scan_case(options.output_dir, redactor)
    cases.append(secret_scan)
    report["cases"] = cases
    report["status"] = acceptance.aggregate_status(cases)
    report["finishedAt"] = acceptance.utc_now()
    report["durationMs"] = acceptance.elapsed_ms(started)
    json_path, markdown_path = write_report(report, options.output_dir, redactor)
    print(f"Stage 3 Kubernetes Worker Release rollout: {report['status']}")
    print(f"JSON: {json_path}")
    print(f"Markdown: {markdown_path}")
    return 0 if report["status"] == "pass" else 1


if __name__ == "__main__":
    raise SystemExit(main())
