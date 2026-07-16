#!/usr/bin/env python3
"""Verify immutable Docker Worker Release canary, promotion, and rollback."""

from __future__ import annotations

import argparse
import dataclasses
import datetime as dt
import hashlib
import json
import os
import pathlib
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid
from collections.abc import Callable, Mapping, Sequence
from typing import Any

import acceptance_runner as acceptance
import release_gate_common as common


SCHEMA_VERSION = "synara.worker-release-rollout-gate.v1"
JSON_REPORT_NAME = "docker-worker-release-rollout-gate.json"
MARKDOWN_REPORT_NAME = "docker-worker-release-rollout-gate.md"
ROLLOUT_LABEL = "synara.io/stage3-worker-release-rollout"
OWNER_LABEL = "synara.io/stage3-worker-release-rollout-owner"
SLOT_LABEL = "synara.io/stage3-worker-release-rollout-slot"
DEFAULT_REGISTRY_IMAGE = "registry:2.8.3"
EXPECTED_POLICY_ACTIONS = (
    (1, "promote"),
    (2, "canary"),
    (3, "promote"),
    (4, "rollback"),
)
EXPECTED_AUDIT_ACTIONS = {
    1: "worker_release.promoted",
    2: "worker_release.canary_started",
    3: "worker_release.promoted",
    4: "worker_release.rolled_back",
}
EXPECTED_OUTBOX_TOPICS = {
    1: "worker.release.promoted",
    2: "worker.release.canary-started",
    3: "worker.release.promoted",
    4: "worker.release.rolled-back",
}


@dataclasses.dataclass(frozen=True)
class GateOptions:
    repo_root: pathlib.Path
    output_dir: pathlib.Path
    timeout_seconds: float
    skip_build: bool
    control_plane_binary: pathlib.Path | None
    docker_socket_path: pathlib.Path
    docker_control_plane_host: str
    docker_memory_bytes: int
    docker_nano_cpus: int
    registry_image: str
    go_proxy: str | None


@dataclasses.dataclass(frozen=True)
class ReleaseImage:
    slot: str
    version: str
    tag: str
    exact_reference: str
    digest: str
    image_id: str
    metadata_path: pathlib.Path


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
    parser.add_argument("--docker-control-plane-host", default="host.docker.internal")
    parser.add_argument("--docker-memory-bytes", type=int, default=2 << 30)
    parser.add_argument("--docker-nano-cpus", type=int, default=1_000_000_000)
    parser.add_argument("--registry-image", default=DEFAULT_REGISTRY_IMAGE)
    parser.add_argument("--go-proxy")
    parsed = parser.parse_args(argv)
    if parsed.timeout <= 0:
        parser.error("--timeout must be positive")
    if parsed.control_plane_binary is not None and not parsed.skip_build:
        parser.error("--control-plane-binary requires --skip-build")
    if parsed.skip_build and parsed.control_plane_binary is None:
        parser.error("--skip-build requires --control-plane-binary")
    if parsed.docker_memory_bytes < 64 << 20:
        parser.error("--docker-memory-bytes must be at least 67108864")
    if parsed.docker_nano_cpus <= 0:
        parser.error("--docker-nano-cpus must be positive")
    docker_socket_path = parsed.docker_socket_path.expanduser().resolve()
    if not docker_socket_path.is_absolute():
        parser.error("--docker-socket-path must be absolute")
    docker_host = parsed.docker_control_plane_host.strip()
    if not docker_host or re.fullmatch(r"[A-Za-z0-9._-]+", docker_host) is None:
        parser.error(
            "--docker-control-plane-host must be a hostname or address without scheme or port"
        )
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
        go_proxy = normalize_go_proxy(parsed.go_proxy)
    except ValueError as error:
        parser.error(str(error))
    run_id = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    output_dir = parsed.output_dir or (
        repo_root
        / ".tmp"
        / "stage3-provider-acceptance-results"
        / f"{run_id}-{uuid.uuid4().hex[:8]}-docker-worker-release-rollout"
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
        docker_control_plane_host=docker_host,
        docker_memory_bytes=parsed.docker_memory_bytes,
        docker_nano_cpus=parsed.docker_nano_cpus,
        registry_image=registry_image,
        go_proxy=go_proxy,
    )


normalize_go_proxy = common.normalize_go_proxy


def rollout_version(base_version: str, slot: str) -> str:
    suffix = f"rollout.{slot}"
    return f"{base_version}.{suffix}" if "+" in base_version else f"{base_version}+{suffix}"


def runner_options(options: GateOptions) -> acceptance.RunnerOptions:
    arguments = [
        "--target",
        "docker",
        "--provider",
        "codex",
        "--output-dir",
        str(options.output_dir),
        "--timeout",
        str(options.timeout_seconds),
        "--docker-socket-path",
        str(options.docker_socket_path),
        "--docker-control-plane-host",
        options.docker_control_plane_host,
        "--docker-memory-bytes",
        str(options.docker_memory_bytes),
        "--docker-nano-cpus",
        str(options.docker_nano_cpus),
        "--docker-worker-image",
        "synara-stage3-worker-release-rollout:placeholder",
        "--docker-skip-worker-build",
    ]
    if options.skip_build:
        arguments.extend(
            ["--skip-build", "--control-plane-binary", str(options.control_plane_binary)]
        )
    return acceptance.parse_args(arguments)


class DockerWorkerReleaseRolloutDriver(acceptance.DockerDriver):
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
        self.target_name = f"stage3-rollout-main-{suffix}"
        self.observer_target_name = f"stage3-rollout-observer-{suffix}"
        self.volume_name = f"synara-stage3-rollout-main-{suffix}"
        self.observer_volume_name = f"synara-stage3-rollout-observer-{suffix}"
        self.registry_volume_name = f"synara-stage3-rollout-registry-{suffix}"
        self.network_name = f"synara-stage3-rollout-{suffix}"
        self.registry_container_name = f"synara-stage3-rollout-registry-{suffix}"
        self.registry_port = acceptance.reserve_loopback_port()
        self.registry_repository = f"127.0.0.1:{self.registry_port}/synara/worker-rollout"
        self.observer_target_id: str | None = None
        self.target_volumes: dict[str, str] = {}
        self.images: dict[str, ReleaseImage] = {}
        self.created_volumes: set[str] = set()
        self.network_created = False
        self.registry_created = False
        self.owns_network = True
        self.owns_image = True

    def prepare(self) -> Mapping[str, Any]:
        control_plane = acceptance.LocalDriver.prepare(self)
        socket_evidence = self._ping_socket()
        server_version = self._docker_command(
            ["version", "--format", "{{.Server.Version}}"],
            log_path=self.logs_dir / "docker-version.log",
        ).strip()
        platform = normalize_engine_platform(
            self._docker_command(
                ["info", "--format", "{{.OSType}}/{{.Architecture}}"],
                log_path=self.logs_dir / "docker-platform.log",
            ).strip()
        )
        self._create_network()
        for volume in (
            self.volume_name,
            self.observer_volume_name,
            self.registry_volume_name,
        ):
            self._create_volume(volume)
        self._start_registry()
        source_version = self._source_version()
        baseline = self._build_release_image(
            "baseline", rollout_version(source_version, "baseline"), platform
        )
        candidate = self._build_release_image(
            "candidate", rollout_version(source_version, "candidate"), platform
        )
        if baseline.digest == candidate.digest or baseline.image_id == candidate.image_id:
            raise acceptance.AcceptanceError(
                "runner.worker_release_images_not_distinct",
                "The baseline and candidate Worker images did not produce distinct immutable identities.",
            )
        self.images = {"baseline": baseline, "candidate": candidate}
        self.image = baseline.exact_reference
        return {
            "controlPlane": control_plane,
            "docker": {
                "serverVersion": server_version,
                "platform": platform,
                "socket": socket_evidence,
                "network": self.network_name,
                "resourceOwner": self.resource_owner,
            },
            "registry": {
                "image": self.gate_options.registry_image,
                "container": self.registry_container_name,
                "repository": self.registry_repository,
                "loopbackOnly": True,
                "storageVolume": self.registry_volume_name,
            },
            "images": {
                slot: release_image_evidence(image)
                for slot, image in self.images.items()
            },
        }

    def _create_network(self) -> None:
        self._docker_command(
            [
                "network",
                "create",
                "--label",
                f"{ROLLOUT_LABEL}=true",
                "--label",
                f"{OWNER_LABEL}={self.resource_owner}",
                self.network_name,
            ],
            log_path=self.logs_dir / "docker-network-create.log",
        )
        self.network_created = True

    def _create_volume(self, name: str) -> None:
        self._docker_command(
            [
                "volume",
                "create",
                "--label",
                f"{ROLLOUT_LABEL}=true",
                "--label",
                f"{OWNER_LABEL}={self.resource_owner}",
                name,
            ],
            log_path=self.logs_dir / f"{name}-create.log",
        )
        self.created_volumes.add(name)

    def _start_registry(self) -> None:
        self._docker_command(
            [
                "run",
                "-d",
                "--name",
                self.registry_container_name,
                "--label",
                f"{ROLLOUT_LABEL}=true",
                "--label",
                f"{OWNER_LABEL}={self.resource_owner}",
                "--publish",
                f"127.0.0.1:{self.registry_port}:5000",
                "--volume",
                f"{self.registry_volume_name}:/var/lib/registry",
                self.gate_options.registry_image,
            ],
            log_path=self.logs_dir / "registry-start.log",
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

    def _build_release_image(self, slot: str, version: str, platform: str) -> ReleaseImage:
        git_sha = self._head_sha()
        tag = f"{self.registry_repository}:{slot}-{git_sha[:12]}-{self.resource_owner[:8]}"
        metadata_path = self.logs_dir / f"worker-{slot}-build-metadata.json"
        arguments = [
            "--target",
            "worker-acceptance",
            "--image",
            tag,
            "--version",
            version,
            "--git-sha",
            git_sha,
            "--source-date-epoch",
            self._source_date_epoch(git_sha),
            "--platform",
            platform,
            "--metadata-file",
            str(metadata_path),
            "--label",
            f"{ROLLOUT_LABEL}=true",
            "--label",
            f"{OWNER_LABEL}={self.resource_owner}",
            "--label",
            f"{SLOT_LABEL}={slot}",
            "--load",
        ]
        if self.gate_options.go_proxy is not None:
            arguments.extend(["--go-proxy", self.gate_options.go_proxy])
        self._worker_build_command(
            arguments,
            log_path=self.logs_dir / f"worker-{slot}-build.log",
            maximum_timeout=max(120.0, self.deadline.remaining()),
        )
        self._docker_command(
            ["push", tag],
            log_path=self.logs_dir / f"worker-{slot}-push.log",
            maximum_timeout=max(120.0, self.deadline.remaining()),
        )
        image_id = self._docker_command(
            ["image", "inspect", "--format", "{{.Id}}", tag]
        ).strip()
        raw_repo_digests = self._docker_command(
            ["image", "inspect", "--format", "{{json .RepoDigests}}", tag]
        ).strip()
        try:
            repo_digests = json.loads(raw_repo_digests)
        except json.JSONDecodeError:
            repo_digests = None
        matches = [
            value
            for value in repo_digests or []
            if isinstance(value, str) and value.startswith(self.registry_repository + "@")
        ]
        if len(matches) != 1:
            raise acceptance.AcceptanceError(
                "runner.worker_release_registry_digest_missing",
                "The local Registry push did not return one exact repository digest.",
                {"slot": slot, "repoDigestCount": len(matches)},
            )
        exact_reference = matches[0]
        digest = exact_reference.rsplit("@", 1)[-1]
        if (
            re.fullmatch(r"sha256:[0-9a-f]{64}", digest) is None
            or re.fullmatch(r"sha256:[0-9a-f]{64}", image_id) is None
        ):
            raise acceptance.AcceptanceError(
                "runner.worker_release_registry_digest_invalid",
                "The pushed Worker image did not expose canonical SHA-256 identities.",
                {"slot": slot},
            )
        self._validate_owned_image(image_id, slot)
        return ReleaseImage(
            slot=slot,
            version=version,
            tag=tag,
            exact_reference=exact_reference,
            digest=digest,
            image_id=image_id,
            metadata_path=metadata_path,
        )

    def _validate_owned_image(self, image: str, slot: str) -> bool:
        raw = self._docker_command(
            ["image", "inspect", "--format", "{{json .Config.Labels}}", image]
        ).strip()
        try:
            labels = json.loads(raw)
        except json.JSONDecodeError:
            labels = None
        if (
            not isinstance(labels, dict)
            or labels.get(OWNER_LABEL) != self.resource_owner
            or labels.get(SLOT_LABEL) != slot
        ):
            raise acceptance.AcceptanceError(
                "runner.worker_release_image_not_owned",
                "The Worker rollout gate refused an image without its exact ownership labels.",
                {"slot": slot},
            )
        return True

    def provision_rollout_targets(
        self,
        tenant_id: str,
        organization_id: str,
        provider: str,
    ) -> Mapping[str, Any]:
        baseline = self.images["baseline"]
        candidate = self.images["candidate"]
        main = self._create_target(
            tenant_id,
            organization_id,
            provider,
            name=self.target_name,
            image=baseline.exact_reference,
            desired_workers=2,
            volume=self.volume_name,
        )
        observer = self._create_target(
            tenant_id,
            organization_id,
            provider,
            name=self.observer_target_name,
            image=candidate.exact_reference,
            desired_workers=1,
            volume=self.observer_volume_name,
        )
        main_id = required_string(main, "id", "main Docker Target")
        observer_id = required_string(observer, "id", "observer Docker Target")
        self.target_id = main_id
        self.observer_target_id = observer_id
        self.target_volumes = {
            main_id: self.volume_name,
            observer_id: self.observer_volume_name,
        }
        main_pool = self.wait_container_pool(
            main_id,
            [pool_class(None, None, baseline, 2)],
        )
        observer_pool = self.wait_container_pool(
            observer_id,
            [pool_class(None, None, candidate, 1)],
        )
        return {
            "mainTarget": acceptance.AcceptanceSuite._target_summary(main),
            "observerTarget": acceptance.AcceptanceSuite._target_summary(observer),
            "mainPool": main_pool,
            "observerPool": observer_pool,
        }

    def _create_target(
        self,
        tenant_id: str,
        organization_id: str,
        provider: str,
        *,
        name: str,
        image: str,
        desired_workers: int,
        volume: str,
    ) -> dict[str, Any]:
        return acceptance.json_object(
            self.api.request(
                "POST",
                f"/v1/tenants/{tenant_id}/execution-targets",
                {
                    "organizationId": organization_id,
                    "kind": "docker",
                    "name": name,
                    "configuration": {
                        "socketPath": str(self.options.docker_socket_path),
                        "image": image,
                        "pullPolicy": "always",
                        "controlPlaneUrl": self._worker_proxy_url(),
                        "allowInsecureControlPlane": True,
                        "runnerCommand": list(self.options.runner_command),
                        "desiredWorkers": desired_workers,
                        "workspaceVolume": volume,
                        "workspaceMount": "/data",
                        "workspaceRoot": "/data/workspaces",
                        "gitCacheRoot": "/data/git-cache",
                        "networkMode": self.network_name,
                        "user": "10001:10001",
                        "memoryBytes": self.options.docker_memory_bytes,
                        "nanoCpus": self.options.docker_nano_cpus,
                    },
                    "capabilities": {
                        "workspaceModes": ["local", "worktree"],
                        "providerPolicy": {"experimentalProviders": [provider]},
                    },
                },
                expected=(201,),
            ),
            f"{name} Docker Target",
        )

    def wait_container_pool(
        self,
        target_id: str,
        expected_classes: Sequence[Mapping[str, Any]],
        *,
        excluded_container_ids: Sequence[str] = (),
    ) -> Mapping[str, Any]:
        expected_count = sum(int(item["count"]) for item in expected_classes)
        excluded = set(excluded_container_ids)

        def probe() -> Mapping[str, Any] | None:
            containers = self._managed_containers(target_id)
            if len(containers) != expected_count or not container_pool_running(containers):
                return None
            summaries = [self._container_summary(target_id, container) for container in containers]
            if excluded.intersection(
                str(summary.get("id") or "") for summary in summaries
            ):
                return None
            actual = container_pool_counts(summaries)
            expected = {
                (
                    item.get("channel"),
                    item.get("revisionId"),
                    item.get("digest"),
                ): int(item["count"])
                for item in expected_classes
            }
            if actual != expected:
                return None
            return {
                "targetId": target_id,
                "containerCount": len(summaries),
                "classes": [dict(item) for item in expected_classes],
                "containers": summaries,
            }

        return self.api.wait_until(
            f"Docker Worker release pool {target_id}", probe, interval=0.25
        )

    def _managed_containers(self, target_id: str) -> list[dict[str, Any]]:
        completed = self._docker_completed(
            [
                "ps",
                "-aq",
                "--filter",
                "label=synara.io/managed=true",
                "--filter",
                f"label=synara.io/execution-target-id={target_id}",
            ]
        )
        if completed.returncode != 0:
            raise acceptance.AcceptanceError(
                "runner.docker_container_list_failed",
                "Managed Docker Worker containers could not be listed.",
                {"targetId": target_id},
            )
        ids = [line.strip() for line in completed.stdout.splitlines() if line.strip()]
        if not ids:
            return []
        inspected = self._docker_completed(["inspect", *ids])
        if inspected.returncode != 0:
            if docker_container_missing(inspected.stdout):
                return []
            raise acceptance.AcceptanceError(
                "runner.docker_inspect_invalid",
                "Managed Docker Worker containers could not be inspected.",
                {"targetId": target_id},
            )
        try:
            payload = json.loads(inspected.stdout)
        except json.JSONDecodeError:
            payload = None
        if not isinstance(payload, list) or not all(isinstance(item, dict) for item in payload):
            raise acceptance.AcceptanceError(
                "runner.docker_inspect_invalid",
                "Docker inspect returned an invalid managed Worker pool.",
                {"targetId": target_id},
            )
        return payload

    def _container_summary(
        self,
        target_id: str,
        container: Mapping[str, Any],
    ) -> dict[str, Any]:
        state = acceptance.json_object(container.get("State"), "Docker Worker state")
        config = acceptance.json_object(container.get("Config"), "Docker Worker config")
        host = acceptance.json_object(container.get("HostConfig"), "Docker Worker host config")
        labels = acceptance.json_object(config.get("Labels"), "Docker Worker labels")
        environment = {
            value.partition("=")[0]: value.partition("=")[2]
            for value in config.get("Env", [])
            if isinstance(value, str) and "=" in value
        }
        mounts = container.get("Mounts")
        expected_volume = self.target_volumes.get(target_id)
        volume_ok = isinstance(mounts, list) and any(
            isinstance(item, dict)
            and item.get("Type") == "volume"
            and item.get("Name") == expected_volume
            and item.get("Destination") == "/data"
            for item in mounts
        )
        expected_contract = {
            "running": True,
            "user": "10001:10001",
            "memoryBytes": self.options.docker_memory_bytes,
            "nanoCpus": self.options.docker_nano_cpus,
            "networkMode": self.network_name,
            "targetId": target_id,
            "volumeMounted": True,
        }
        actual_contract = {
            "running": state.get("Running"),
            "user": config.get("User"),
            "memoryBytes": host.get("Memory"),
            "nanoCpus": host.get("NanoCpus"),
            "networkMode": host.get("NetworkMode"),
            "targetId": labels.get("synara.io/execution-target-id"),
            "volumeMounted": volume_ok,
        }
        if actual_contract != expected_contract:
            raise acceptance.AcceptanceError(
                "runner.docker_container_contract_mismatch",
                "A managed Docker Worker did not retain the rollout isolation contract.",
                {"expected": expected_contract, "actual": actual_contract},
            )
        digest = environment.get("SYNARA_AGENTD_IMAGE_DIGEST")
        if re.fullmatch(r"sha256:[0-9a-f]{64}", digest or "") is None:
            raise acceptance.AcceptanceError(
                "runner.worker_release_container_digest_missing",
                "A managed Docker Worker omitted its immutable image digest.",
                {"targetId": target_id, "container": str(container.get("Name") or "")},
            )
        return {
            "id": str(container.get("Id") or "")[:12],
            "name": str(container.get("Name") or "").lstrip("/"),
            "imageId": container.get("Image"),
            "digest": digest,
            "revisionId": labels.get("synara.io/worker-release-revision-id"),
            "channel": labels.get("synara.io/worker-release-channel"),
            "volume": expected_volume,
        }

    def cleanup(self) -> Mapping[str, Any]:
        errors: list[str] = []

        def attempt(operation: str, action: Callable[[], Any]) -> Any:
            try:
                return action()
            except Exception as error:  # Cleanup must retain every exact failure.
                errors.append(f"{operation}: {self.redactor.text(str(error))}")
                return None

        attempt("stop Control Plane", self.stop)
        attempt("stop Worker-only proxy", self._stop_worker_proxy)
        target_ids = [
            target_id
            for target_id in (self.target_id, self.observer_target_id)
            if isinstance(target_id, str) and target_id
        ]
        removed_containers = 0
        managed_workers_removed = True
        for target_id in target_ids:
            completed = attempt(
                f"list managed Workers for {target_id}",
                lambda target_id=target_id: self._docker_completed(
                    [
                        "ps",
                        "-aq",
                        "--filter",
                        "label=synara.io/managed=true",
                        "--filter",
                        f"label=synara.io/execution-target-id={target_id}",
                    ],
                    cleanup_timeout=10.0,
                ),
            )
            ids = (
                [line.strip() for line in completed.stdout.splitlines() if line.strip()]
                if isinstance(completed, subprocess.CompletedProcess) and completed.returncode == 0
                else []
            )
            if ids:
                removed = attempt(
                    f"remove managed Workers for {target_id}",
                    lambda ids=ids: self._docker_completed(
                        ["rm", "-f", *ids], cleanup_timeout=20.0
                    ),
                )
                if isinstance(removed, subprocess.CompletedProcess) and removed.returncode == 0:
                    removed_containers += len(ids)
                elif isinstance(removed, subprocess.CompletedProcess):
                    managed_workers_removed = False
                    errors.append(
                        f"remove managed Workers for {target_id}: {self.redactor.text(removed.stdout.strip())}"
                    )
            remaining = attempt(
                f"verify managed Workers absent for {target_id}",
                lambda target_id=target_id: self._docker_completed(
                    [
                        "ps",
                        "-aq",
                        "--filter",
                        "label=synara.io/managed=true",
                        "--filter",
                        f"label=synara.io/execution-target-id={target_id}",
                    ],
                    cleanup_timeout=10.0,
                ),
            )
            if (
                not isinstance(remaining, subprocess.CompletedProcess)
                or remaining.returncode != 0
                or remaining.stdout.strip()
            ):
                managed_workers_removed = False
                errors.append(f"managed Workers remained for {target_id}")
        registry_removed = False
        if self.registry_created:
            owned = attempt(
                "verify Registry ownership",
                lambda: self._container_owned(self.registry_container_name),
            )
            if owned is True:
                completed = attempt(
                    "remove Registry container",
                    lambda: self._docker_completed(
                        ["rm", "-f", self.registry_container_name], cleanup_timeout=20.0
                    ),
                )
                registry_removed = isinstance(completed, subprocess.CompletedProcess) and completed.returncode == 0
                if isinstance(completed, subprocess.CompletedProcess) and completed.returncode != 0:
                    errors.append(
                        "remove Registry container: "
                        + self.redactor.text(completed.stdout.strip())
                    )
        removed_images: list[str] = []
        for image in self.images.values():
            owned = attempt(
                f"verify {image.slot} image ownership",
                lambda image=image: self._validate_owned_image(image.image_id, image.slot),
            )
            if owned is not True:
                continue
            completed = attempt(
                f"remove {image.slot} Worker image",
                lambda image=image: self._docker_completed(
                    ["image", "rm", "-f", image.image_id], cleanup_timeout=60.0
                ),
            )
            if isinstance(completed, subprocess.CompletedProcess) and completed.returncode == 0:
                removed_images.append(image.slot)
            elif isinstance(completed, subprocess.CompletedProcess):
                errors.append(
                    f"remove {image.slot} Worker image: {self.redactor.text(completed.stdout.strip())}"
                )
        removed_volumes: list[str] = []
        for volume in sorted(self.created_volumes):
            owned = attempt(
                f"verify volume {volume} ownership",
                lambda volume=volume: self._rollout_resource_owned("volume", volume),
            )
            if owned is not True:
                continue
            completed = attempt(
                f"remove volume {volume}",
                lambda volume=volume: self._docker_completed(
                    ["volume", "rm", "-f", volume], cleanup_timeout=20.0
                ),
            )
            if isinstance(completed, subprocess.CompletedProcess) and completed.returncode == 0:
                removed_volumes.append(volume)
            elif isinstance(completed, subprocess.CompletedProcess):
                errors.append(
                    f"remove volume {volume}: {self.redactor.text(completed.stdout.strip())}"
                )
        network_removed = False
        if self.network_created:
            owned = attempt(
                "verify rollout network ownership",
                lambda: self._rollout_resource_owned("network", self.network_name),
            )
            if owned is True:
                completed = attempt(
                    "remove rollout network",
                    lambda: self._docker_completed(
                        ["network", "rm", self.network_name], cleanup_timeout=20.0
                    ),
                )
                network_removed = isinstance(completed, subprocess.CompletedProcess) and completed.returncode == 0
                if isinstance(completed, subprocess.CompletedProcess) and completed.returncode != 0:
                    errors.append(
                        "remove rollout network: "
                        + self.redactor.text(completed.stdout.strip())
                    )
        self.registration_token = ""
        attempt("release isolated state", self._release_state)
        if errors:
            raise acceptance.AcceptanceError(
                "runner.worker_release_rollout_cleanup_failed",
                "Worker Release rollout resources could not be cleaned exactly.",
                {"errors": errors},
            )
        return {
            "target": self.name,
            "resourceOwner": self.resource_owner,
            "managedWorkerContainersRemoved": managed_workers_removed,
            "managedWorkerContainerCountRemoved": removed_containers,
            "registryContainerRemoved": registry_removed or not self.registry_created,
            "workerImagesRemoved": sorted(removed_images) == sorted(self.images),
            "removedImageSlots": sorted(removed_images),
            "volumesRemoved": sorted(removed_volumes) == sorted(self.created_volumes),
            "removedVolumes": removed_volumes,
            "networkRemoved": network_removed or not self.network_created,
            "stateRemoved": self._temporary_state and not self.state_dir.exists(),
            "registryBaseImageRemoved": False,
            "broadCleanupUsed": False,
        }

    def _container_owned(self, name: str) -> bool:
        completed = self._docker_completed(
            [
                "container",
                "inspect",
                "--format",
                f'{{{{ index .Config.Labels "{OWNER_LABEL}" }}}}',
                name,
            ],
            cleanup_timeout=5.0,
        )
        if completed.returncode != 0 or completed.stdout.strip() != self.resource_owner:
            raise acceptance.AcceptanceError(
                "runner.worker_release_resource_not_owned",
                "The rollout gate refused to remove a Registry container without its exact owner label.",
                {"resource": "container", "name": name},
            )
        return True

    def _rollout_resource_owned(self, resource: str, name: str) -> bool:
        completed = self._docker_completed(
            [
                resource,
                "inspect",
                "--format",
                f'{{{{ index .Labels "{OWNER_LABEL}" }}}}',
                name,
            ],
            cleanup_timeout=5.0,
        )
        if completed.returncode != 0 or completed.stdout.strip() != self.resource_owner:
            raise acceptance.AcceptanceError(
                "runner.worker_release_resource_not_owned",
                "The rollout gate refused to remove a Docker resource without its exact owner label.",
                {"resource": resource, "name": name},
            )
        return True


class WorkerReleaseRolloutSuite(acceptance.AcceptanceSuite):
    driver: DockerWorkerReleaseRolloutDriver

    def __init__(
        self,
        options: acceptance.RunnerOptions,
        driver: DockerWorkerReleaseRolloutDriver,
        deadline: acceptance.Deadline,
        redactor: acceptance.SecretRedactor,
    ) -> None:
        super().__init__(options, driver, deadline, redactor)
        self.manifests: dict[str, dict[str, Any]] = {}
        self.revisions: dict[str, dict[str, Any]] = {}
        self.busy_baseline: dict[str, Any] | None = None

    def run(self) -> list[dict[str, Any]]:
        self._case(
            "environment.target-prepare",
            "Build and push two clean-SHA Worker images to an isolated loopback Registry",
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
            "Provision the two-Worker rollout Target and one-Worker candidate observer",
            self._provision_rollout_targets,
            requires=("identity.dev-login",),
        )
        self._case(
            "release.revisions",
            "Create two immutable Release Revisions and reject duplicate registration",
            self._create_release_revisions,
            requires=("runtime.rollout-targets",),
        )
        self._case(
            "release.initial-promote",
            "Promote the baseline Revision into a two-Worker exact-digest pool",
            self._initial_promote,
            requires=("release.revisions",),
        )
        self._case(
            "resources.credential-project-session",
            "Create the deterministic fixture Credential, Project, and rollout Session",
            self._create_resources,
            requires=("release.initial-promote",),
        )
        self._case(
            "release.baseline-active",
            "Hold a baseline Approval Execution and bind its active Lease to one exact Worker container",
            self._begin_busy_baseline,
            requires=("resources.credential-project-session",),
        )
        self._case(
            "release.canary",
            "Start canary without replacing the busy baseline Worker and fence promotion until terminal",
            self._start_canary,
            requires=("release.baseline-active",),
        )
        self._case(
            "release.canary-active-fence",
            "Pin an Approval Execution to canary and block promote/rollback until terminal",
            self._canary_active_fence,
            requires=("release.canary",),
        )
        self._case(
            "release.promote",
            "Promote the candidate and prove new Executions use only candidate/promoted",
            self._promote_candidate,
            requires=("release.canary-active-fence",),
        )
        self._case(
            "release.rollback",
            "Roll back to baseline and prove new Executions use only baseline/promoted",
            self._rollback_baseline,
            requires=("release.promote",),
        )
        self._case(
            "release.history-audit-outbox",
            "Verify immutable history, audit, Outbox, and contiguous terminal sequencing",
            self._history_audit_outbox,
            requires=("release.rollback",),
        )
        return self.cases

    def _provision_rollout_targets(self) -> Mapping[str, Any]:
        tenant_id = self._required("tenant_id")
        organization_id = self._required("organization_id")
        evidence = self.driver.provision_rollout_targets(
            tenant_id, organization_id, self.options.provider
        )
        main_id = required_string(evidence["mainTarget"], "id", "main Target")
        observer_id = required_string(evidence["observerTarget"], "id", "observer Target")
        self.state.target_id = main_id
        baseline = self.driver.images["baseline"]
        candidate = self.driver.images["candidate"]
        self.manifests["baseline"] = self._wait_manifest(
            main_id, baseline, expected_online=2
        )
        self.manifests["candidate"] = self._wait_manifest(
            observer_id, candidate, expected_online=1
        )
        return {
            **dict(evidence),
            "baselineManifest": manifest_evidence(self.manifests["baseline"]),
            "candidateManifest": manifest_evidence(self.manifests["candidate"]),
        }

    def _wait_manifest(
        self,
        target_id: str,
        image: ReleaseImage,
        *,
        expected_online: int,
        manifest_id: str | None = None,
    ) -> dict[str, Any]:
        tenant_id = self._required("tenant_id")

        def probe() -> dict[str, Any] | None:
            manifests = acceptance.json_items(
                self.api.request("GET", f"/v1/tenants/{tenant_id}/worker-manifests"),
                "worker manifests",
            )
            candidates = []
            for item in manifests:
                build = item.get("workerBuild")
                counts = item.get("workerStatusCounts")
                if (
                    item.get("executionTargetId") != target_id
                    or not isinstance(build, dict)
                    or not isinstance(counts, dict)
                    or build.get("imageDigest") != image.digest
                    or (manifest_id is not None and item.get("manifestId") != manifest_id)
                ):
                    continue
                candidates.append(item)
            if expected_online == 0:
                if any(int(item["workerStatusCounts"].get("online") or 0) != 0 for item in candidates):
                    return None
                return candidates[0] if candidates else {
                    "executionTargetId": target_id,
                    "manifestId": manifest_id,
                    "workerStatusCounts": {"online": 0, "draining": 0, "offline": 0},
                    "workerBuild": {
                        "version": image.version,
                        "gitSha": self.driver.head_sha,
                        "imageDigest": image.digest,
                    },
                }
            if len(candidates) != 1:
                return None
            item = candidates[0]
            counts = acceptance.json_object(
                item.get("workerStatusCounts"), "Worker manifest status counts"
            )
            build = acceptance.json_object(item.get("workerBuild"), "Worker manifest build")
            if int(counts.get("online") or 0) != expected_online:
                return None
            if build.get("version") != image.version or build.get("gitSha") != self.driver.head_sha:
                raise acceptance.AcceptanceError(
                    "runner.worker_release_manifest_identity_mismatch",
                    "A Worker manifest did not retain the clean source identity.",
                    {"targetId": target_id, "slot": image.slot},
                )
            return item

        return self.api.wait_until(
            f"{image.slot} Worker manifest on Target {target_id}", probe, interval=0.25
        )

    def _create_release_revisions(self) -> Mapping[str, Any]:
        tenant_id = self._required("tenant_id")
        target_id = self._required("target_id")
        base_path = release_base_path(tenant_id, target_id)
        for slot in ("baseline", "candidate"):
            manifest_id = required_string(
                self.manifests[slot], "manifestId", f"{slot} Worker manifest"
            )
            revision = acceptance.json_object(
                self.api.request(
                    "POST",
                    base_path,
                    {
                        "workerManifestId": manifest_id,
                        "description": f"Stage 3 Docker rollout {slot}",
                    },
                    expected=(201,),
                ),
                f"{slot} Release Revision",
            )
            validate_revision(revision, self.driver.images[slot], manifest_id)
            self.revisions[slot] = revision
        duplicate = expect_problem(
            self.api,
            "POST",
            base_path,
            {
                "workerManifestId": self.manifests["baseline"]["manifestId"],
                "description": "duplicate immutable registration must fail",
            },
            "worker_release_manifest_already_registered",
        )
        overview = acceptance.json_object(
            self.api.request("GET", base_path), "Worker Release overview"
        )
        if len(overview.get("revisions", [])) != 2:
            raise acceptance.AcceptanceError(
                "runner.worker_release_revision_count_invalid",
                "Duplicate Release registration changed the immutable Revision set.",
            )
        return {
            "baseline": revision_evidence(self.revisions["baseline"]),
            "candidate": revision_evidence(self.revisions["candidate"]),
            "duplicateRegistration": duplicate,
        }

    def _initial_promote(self) -> Mapping[str, Any]:
        policy = self._policy_change(
            "promote", "baseline", expected_version=0, reason="Establish baseline rollout"
        )
        validate_policy(
            policy,
            expected_version=1,
            promoted_id=self.revisions["baseline"]["id"],
            canary_id=None,
            canary_percent=0,
        )
        pool = self.driver.wait_container_pool(
            self._required("target_id"),
            [pool_class("promoted", self.revisions["baseline"]["id"], self.driver.images["baseline"], 2)],
        )
        manifest = self._wait_manifest(
            self._required("target_id"),
            self.driver.images["baseline"],
            expected_online=2,
            manifest_id=self.manifests["baseline"]["manifestId"],
        )
        return {"policy": policy_evidence(policy), "pool": pool, "manifest": manifest_evidence(manifest)}

    def _begin_busy_baseline(self) -> Mapping[str, Any]:
        barrier = self._begin_approval_readiness_barrier()
        turn_id = required_string(barrier, "turnId", "baseline Approval barrier")
        execution = self._wait_execution_release(
            turn_id,
            revision_id=self.revisions["baseline"]["id"],
            channel="promoted",
            manifest_id=self.manifests["baseline"]["manifestId"],
            terminal=False,
        )
        worker = self._wait_managed_worker(
            execution,
            revision_id=self.revisions["baseline"]["id"],
            channel="promoted",
            manifest_id=self.manifests["baseline"]["manifestId"],
        )
        pool = self.driver.wait_container_pool(
            self._required("target_id"),
            [pool_class("promoted", self.revisions["baseline"]["id"], self.driver.images["baseline"], 2)],
        )
        container = pool_container_for_worker(pool, worker)
        evidence = {
            "barrier": barrier,
            "execution": execution,
            "worker": worker,
            "container": container,
            "pool": pool,
        }
        self.busy_baseline = evidence
        return evidence

    def _start_canary(self) -> Mapping[str, Any]:
        busy = self.busy_baseline
        if busy is None:
            raise acceptance.AcceptanceError(
                "runner.worker_release_busy_baseline_missing",
                "The baseline Approval Execution was not active before canary rollout.",
            )
        busy_worker = acceptance.json_object(busy.get("worker"), "busy baseline Worker")
        busy_container = acceptance.json_object(
            busy.get("container"), "busy baseline Worker container"
        )
        policy = self._policy_change(
            "canary",
            "candidate",
            expected_version=1,
            reason="Start exact-digest candidate canary",
            canary_percent=100,
        )
        validate_policy(
            policy,
            expected_version=2,
            promoted_id=self.revisions["baseline"]["id"],
            canary_id=self.revisions["candidate"]["id"],
            canary_percent=100,
        )
        pool = self.driver.wait_container_pool(
            self._required("target_id"),
            [
                pool_class("promoted", self.revisions["baseline"]["id"], self.driver.images["baseline"], 1),
                pool_class("canary", self.revisions["candidate"]["id"], self.driver.images["candidate"], 1),
            ],
        )
        baseline_manifest = self._wait_manifest(
            self._required("target_id"),
            self.driver.images["baseline"],
            expected_online=1,
            manifest_id=self.manifests["baseline"]["manifestId"],
        )
        candidate_manifest = self._wait_manifest(
            self._required("target_id"),
            self.driver.images["candidate"],
            expected_online=1,
            manifest_id=self.manifests["candidate"]["manifestId"],
        )
        busy_after = pool_container_for_worker(pool, busy_worker)
        preservation = validate_busy_container_preserved(busy_container, busy_after)
        stale = expect_problem(
            self.api,
            "POST",
            release_action_path(
                self._required("tenant_id"),
                self._required("target_id"),
                self.revisions["candidate"]["id"],
                "promote",
            ),
            {"expectedPolicyVersion": 1, "reason": "stale CAS must fail"},
            "worker_release_policy_version_conflict",
        )
        promote_conflict = expect_problem(
            self.api,
            "POST",
            release_action_path(
                self._required("tenant_id"),
                self._required("target_id"),
                self.revisions["candidate"]["id"],
                "promote",
            ),
            {
                "expectedPolicyVersion": 2,
                "reason": "busy baseline blocks retirement during promotion",
            },
            "worker_release_active_executions",
        )
        validate_active_execution_conflict(
            promote_conflict,
            revision_id=self.revisions["baseline"]["id"],
            channel="promoted",
        )
        resolution = self._approval_resolution()
        terminal = self._wait_execution_release(
            required_string(busy["barrier"], "turnId", "baseline Approval barrier"),
            revision_id=self.revisions["baseline"]["id"],
            channel="promoted",
            manifest_id=self.manifests["baseline"]["manifestId"],
            terminal=True,
        )
        converged_pool = self.driver.wait_container_pool(
            self._required("target_id"),
            [
                pool_class("promoted", self.revisions["baseline"]["id"], self.driver.images["baseline"], 1),
                pool_class("canary", self.revisions["candidate"]["id"], self.driver.images["candidate"], 1),
            ],
            excluded_container_ids=(required_string(busy_container, "id", "busy container"),),
        )
        self.busy_baseline = None
        return {
            "policy": policy_evidence(policy),
            "pool": pool,
            "baselineManifest": manifest_evidence(baseline_manifest),
            "candidateManifest": manifest_evidence(candidate_manifest),
            "busyBaselinePreserved": preservation,
            "stalePolicy": stale,
            "promoteConflict": promote_conflict,
            "resolution": resolution,
            "terminal": terminal,
            "postTerminalPool": converged_pool,
        }

    def _wait_managed_worker(
        self,
        execution: Mapping[str, Any],
        *,
        revision_id: str,
        channel: str,
        manifest_id: str,
    ) -> dict[str, Any]:
        tenant_id = self._required("tenant_id")
        target_id = self._required("target_id")

        def probe() -> dict[str, Any] | None:
            workers = acceptance.json_items(
                self.api.request("GET", f"/v1/tenants/{tenant_id}/workers"),
                "managed Workers",
            )
            try:
                return validate_managed_worker_binding(
                    workers,
                    execution=execution,
                    target_id=target_id,
                    revision_id=revision_id,
                    channel=channel,
                    manifest_id=manifest_id,
                )
            except PendingReleaseEvidence:
                return None

        return self.api.wait_until(
            f"managed Worker binding for Execution {execution.get('executionId')}",
            probe,
            interval=0.2,
        )

    def _canary_active_fence(self) -> Mapping[str, Any]:
        barrier = self._begin_approval_readiness_barrier()
        turn_id = required_string(barrier, "turnId", "Approval barrier")
        execution = self._wait_execution_release(
            turn_id,
            revision_id=self.revisions["candidate"]["id"],
            channel="canary",
            manifest_id=self.manifests["candidate"]["manifestId"],
            terminal=False,
        )
        promote_conflict = expect_problem(
            self.api,
            "POST",
            release_action_path(
                self._required("tenant_id"),
                self._required("target_id"),
                self.revisions["candidate"]["id"],
                "promote",
            ),
            {"expectedPolicyVersion": 2, "reason": "active canary blocks promotion"},
            "worker_release_active_executions",
        )
        rollback_conflict = expect_problem(
            self.api,
            "POST",
            release_action_path(
                self._required("tenant_id"),
                self._required("target_id"),
                self.revisions["baseline"]["id"],
                "rollback",
            ),
            {"expectedPolicyVersion": 2, "reason": "active canary blocks rollback"},
            "worker_release_active_executions",
        )
        resolution = self._approval_resolution()
        terminal = self._wait_execution_release(
            turn_id,
            revision_id=self.revisions["candidate"]["id"],
            channel="canary",
            manifest_id=self.manifests["candidate"]["manifestId"],
            terminal=True,
        )
        return {
            "barrier": barrier,
            "execution": execution,
            "promoteConflict": promote_conflict,
            "rollbackConflict": rollback_conflict,
            "resolution": resolution,
            "terminal": terminal,
        }

    def _promote_candidate(self) -> Mapping[str, Any]:
        policy = self._policy_change(
            "promote",
            "candidate",
            expected_version=2,
            reason="Canary completed without duplicate terminal or claim",
        )
        validate_policy(
            policy,
            expected_version=3,
            promoted_id=self.revisions["candidate"]["id"],
            canary_id=None,
            canary_percent=0,
        )
        pool = self.driver.wait_container_pool(
            self._required("target_id"),
            [pool_class("promoted", self.revisions["candidate"]["id"], self.driver.images["candidate"], 2)],
        )
        candidate_manifest = self._wait_manifest(
            self._required("target_id"),
            self.driver.images["candidate"],
            expected_online=2,
            manifest_id=self.manifests["candidate"]["manifestId"],
        )
        baseline_manifest = self._wait_manifest(
            self._required("target_id"),
            self.driver.images["baseline"],
            expected_online=0,
            manifest_id=self.manifests["baseline"]["manifestId"],
        )
        smoke = self._release_smoke(
            revision_id=self.revisions["candidate"]["id"],
            channel="promoted",
            manifest_id=self.manifests["candidate"]["manifestId"],
        )
        return {
            "policy": policy_evidence(policy),
            "pool": pool,
            "candidateManifest": manifest_evidence(candidate_manifest),
            "retiredBaselineManifest": manifest_evidence(baseline_manifest),
            "execution": smoke,
            "oldWorkerClaimed": False,
        }

    def _rollback_baseline(self) -> Mapping[str, Any]:
        policy = self._policy_change(
            "rollback",
            "baseline",
            expected_version=3,
            reason="Rollback to the previous immutable baseline",
        )
        validate_policy(
            policy,
            expected_version=4,
            promoted_id=self.revisions["baseline"]["id"],
            canary_id=None,
            canary_percent=0,
        )
        pool = self.driver.wait_container_pool(
            self._required("target_id"),
            [pool_class("promoted", self.revisions["baseline"]["id"], self.driver.images["baseline"], 2)],
        )
        baseline_manifest = self._wait_manifest(
            self._required("target_id"),
            self.driver.images["baseline"],
            expected_online=2,
            manifest_id=self.manifests["baseline"]["manifestId"],
        )
        candidate_manifest = self._wait_manifest(
            self._required("target_id"),
            self.driver.images["candidate"],
            expected_online=0,
            manifest_id=self.manifests["candidate"]["manifestId"],
        )
        observer_target_id = self.driver.observer_target_id
        if not isinstance(observer_target_id, str) or not observer_target_id:
            raise acceptance.AcceptanceError(
                "runner.response_identity_missing",
                "The observer Target ID was unavailable during rollback validation.",
            )
        observer_manifest = self._wait_manifest(
            observer_target_id,
            self.driver.images["candidate"],
            expected_online=1,
            manifest_id=self.manifests["candidate"]["manifestId"],
        )
        smoke = self._release_smoke(
            revision_id=self.revisions["baseline"]["id"],
            channel="promoted",
            manifest_id=self.manifests["baseline"]["manifestId"],
        )
        return {
            "policy": policy_evidence(policy),
            "pool": pool,
            "baselineManifest": manifest_evidence(baseline_manifest),
            "retiredCandidateManifest": manifest_evidence(candidate_manifest),
            "observerCandidateManifest": manifest_evidence(observer_manifest),
            "execution": smoke,
        }

    def _release_smoke(
        self,
        *,
        revision_id: str,
        channel: str,
        manifest_id: str,
    ) -> Mapping[str, Any]:
        turn = self._create_turn("[text] [usage]")
        turn_id = required_string(turn, "id", "release smoke Turn")
        self._wait_for_turn_terminal(turn_id, "execution.completed")
        return self._wait_execution_release(
            turn_id,
            revision_id=revision_id,
            channel=channel,
            manifest_id=manifest_id,
            terminal=True,
        )

    def _wait_execution_release(
        self,
        turn_id: str,
        *,
        revision_id: str,
        channel: str,
        manifest_id: str,
        terminal: bool,
    ) -> Mapping[str, Any]:
        def probe() -> Mapping[str, Any] | None:
            events = self._all_events()
            try:
                return validate_execution_release_events(
                    events,
                    turn_id=turn_id,
                    revision_id=revision_id,
                    channel=channel,
                    manifest_id=manifest_id,
                    terminal_required=terminal,
                )
            except PendingReleaseEvidence:
                return None

        return self.api.wait_until(
            f"release-pinned Execution for Turn {turn_id}", probe, interval=0.2
        )

    def _policy_change(
        self,
        action: str,
        slot: str,
        *,
        expected_version: int,
        reason: str,
        canary_percent: int = 0,
    ) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "expectedPolicyVersion": expected_version,
            "reason": reason,
        }
        if action == "canary":
            payload["canaryPercent"] = canary_percent
        return acceptance.json_object(
            self.api.request(
                "POST",
                release_action_path(
                    self._required("tenant_id"),
                    self._required("target_id"),
                    self.revisions[slot]["id"],
                    action,
                ),
                payload,
            ),
            f"Worker Release {action}",
        )

    def _history_audit_outbox(self) -> Mapping[str, Any]:
        tenant_id = self._required("tenant_id")
        target_id = self._required("target_id")
        overview = acceptance.json_object(
            self.api.request("GET", release_base_path(tenant_id, target_id)),
            "final Worker Release overview",
        )
        history = validate_release_overview(
            overview,
            baseline_revision_id=self.revisions["baseline"]["id"],
            candidate_revision_id=self.revisions["candidate"]["id"],
            baseline_digest=self.driver.images["baseline"].digest,
            candidate_digest=self.driver.images["candidate"].digest,
        )
        audit_page = acceptance.json_object(
            self.api.request("GET", f"/v1/tenants/{tenant_id}/audit-logs?limit=200"),
            "Worker Release audit page",
        )
        audits = audit_page.get("items")
        if not isinstance(audits, list) or not all(isinstance(item, dict) for item in audits):
            raise acceptance.AcceptanceError(
                "runner.worker_release_audit_invalid",
                "Worker Release audit API returned an invalid item list.",
            )
        audit = validate_release_audit(
            audits,
            target_id=target_id,
            revision_ids={
                self.revisions["baseline"]["id"],
                self.revisions["candidate"]["id"],
            },
        )
        outbox_page = acceptance.json_object(
            self.api.request(
                "GET", f"/v1/tenants/{tenant_id}/outbox-messages?status=all&limit=200"
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
        outbox = validate_release_outbox(
            outbox_items,
            target_id=target_id,
            revision_ids={
                self.revisions["baseline"]["id"],
                self.revisions["candidate"]["id"],
            },
        )
        events = self._all_events()
        turns = [event for event in events if event.get("eventType") == "turn.created"]
        execution_ids = [acceptance.AcceptanceSuite._event_execution_id(event) for event in turns]
        if len(execution_ids) != 4 or len(set(execution_ids)) != 4:
            raise acceptance.AcceptanceError(
                "runner.worker_release_execution_count_invalid",
                "The rollout Session did not retain exactly four distinct Executions.",
                {"executionCount": len(execution_ids), "distinctCount": len(set(execution_ids))},
            )
        terminal_counts = {
            execution_id: sum(
                1
                for event in events
                if event.get("executionId") == execution_id
                and event.get("eventType") in acceptance.TERMINAL_EVENT_TYPES
            )
            for execution_id in execution_ids
        }
        if set(terminal_counts.values()) != {1}:
            raise acceptance.AcceptanceError(
                "runner.worker_release_terminal_count_invalid",
                "A rollout Execution emitted a missing or duplicate terminal event.",
                {"terminalCounts": terminal_counts},
            )
        return {
            "overview": history,
            "audit": audit,
            "outbox": outbox,
            "sessionEvents": {
                "sequenceRange": self._sequence_range(events),
                "executionIds": execution_ids,
                "terminalCounts": terminal_counts,
                "duplicateTerminal": False,
                "doubleExecution": False,
            },
        }


class PendingReleaseEvidence(Exception):
    pass


def normalize_engine_platform(value: str) -> str:
    normalized = value.strip().lower()
    aliases = {
        "linux/x86_64": "linux/amd64",
        "linux/aarch64": "linux/arm64",
    }
    normalized = aliases.get(normalized, normalized)
    if normalized not in {"linux/amd64", "linux/arm64"}:
        raise acceptance.AcceptanceError(
            "runner.worker_release_platform_unsupported",
            "The Docker Worker rollout gate requires a Linux amd64 or arm64 Engine.",
            {"platform": value},
        )
    return normalized


def required_string(value: Mapping[str, Any], key: str, description: str) -> str:
    item = value.get(key)
    if not isinstance(item, str) or not item:
        raise acceptance.AcceptanceError(
            "runner.response_identity_missing",
            f"The {description} omitted {key}.",
        )
    return item


def release_base_path(tenant_id: str, target_id: str) -> str:
    return f"/v1/tenants/{tenant_id}/execution-targets/{target_id}/worker-releases"


def release_action_path(
    tenant_id: str,
    target_id: str,
    revision_id: str,
    action: str,
) -> str:
    return f"{release_base_path(tenant_id, target_id)}/{revision_id}/{action}"


def pool_class(
    channel: str | None,
    revision_id: str | None,
    image: ReleaseImage,
    count: int,
) -> dict[str, Any]:
    return {
        "channel": channel,
        "revisionId": revision_id,
        "digest": image.digest,
        "count": count,
    }


def container_pool_counts(
    containers: Sequence[Mapping[str, Any]],
) -> dict[tuple[Any, Any, Any], int]:
    counts: dict[tuple[Any, Any, Any], int] = {}
    for item in containers:
        key = (item.get("channel"), item.get("revisionId"), item.get("digest"))
        counts[key] = counts.get(key, 0) + 1
    return counts


def container_pool_running(containers: Sequence[Mapping[str, Any]]) -> bool:
    return all(
        isinstance(container.get("State"), dict)
        and container["State"].get("Running") is True
        for container in containers
    )


def docker_container_missing(output: str) -> bool:
    normalized = output.lower()
    return "no such object" in normalized or "no such container" in normalized


def release_image_evidence(image: ReleaseImage) -> dict[str, Any]:
    return {
        "slot": image.slot,
        "version": image.version,
        "tag": image.tag,
        "exactReference": image.exact_reference,
        "digest": image.digest,
        "imageId": image.image_id,
        "buildMetadata": str(image.metadata_path),
    }


def manifest_evidence(manifest: Mapping[str, Any]) -> dict[str, Any]:
    return {
        "executionTargetId": manifest.get("executionTargetId"),
        "manifestId": manifest.get("manifestId"),
        "workerStatusCounts": manifest.get("workerStatusCounts"),
        "workerBuild": manifest.get("workerBuild"),
    }


def revision_evidence(revision: Mapping[str, Any]) -> dict[str, Any]:
    return {
        key: revision.get(key)
        for key in (
            "id",
            "revision",
            "workerManifestId",
            "workerBuildVersion",
            "workerBuildGitSha",
            "imageDigest",
            "description",
        )
    }


def policy_evidence(policy: Mapping[str, Any]) -> dict[str, Any]:
    return {
        key: policy.get(key)
        for key in (
            "policyVersion",
            "promotedRevisionId",
            "canaryRevisionId",
            "canaryPercent",
            "updatedAt",
        )
    }


def validate_managed_worker_binding(
    workers: Sequence[Mapping[str, Any]],
    *,
    execution: Mapping[str, Any],
    target_id: str,
    revision_id: str,
    channel: str,
    manifest_id: str,
) -> dict[str, Any]:
    worker_id = execution.get("workerId")
    if not isinstance(worker_id, str) or not worker_id:
        raise acceptance.AcceptanceError(
            "runner.worker_release_execution_worker_missing",
            "The release-pinned Execution omitted its Worker ID.",
        )
    matches = [worker for worker in workers if worker.get("id") == worker_id]
    if not matches:
        raise PendingReleaseEvidence()
    if len(matches) != 1:
        raise acceptance.AcceptanceError(
            "runner.worker_release_worker_identity_ambiguous",
            "The managed Worker API returned duplicate rows for one release-pinned Execution.",
            {"workerId": worker_id, "matchCount": len(matches)},
        )
    worker = matches[0]
    actual = {
        "executionTargetId": worker.get("executionTargetId"),
        "currentManifestId": worker.get("currentManifestId"),
        "workerReleaseRevisionId": worker.get("workerReleaseRevisionId"),
        "workerReleaseChannel": worker.get("workerReleaseChannel"),
        "workerReleaseStatus": worker.get("workerReleaseStatus"),
        "administrativeStatus": worker.get("administrativeStatus"),
        "status": worker.get("status"),
    }
    expected = {
        "executionTargetId": target_id,
        "currentManifestId": manifest_id,
        "workerReleaseRevisionId": revision_id,
        "workerReleaseChannel": channel,
        "workerReleaseStatus": "active",
        "administrativeStatus": "active",
        "status": "online",
    }
    pod_name = worker.get("podName")
    if actual != expected or not isinstance(pod_name, str) or not pod_name:
        raise acceptance.AcceptanceError(
            "runner.worker_release_worker_binding_mismatch",
            "The active Execution Worker did not retain its exact Manifest, Revision, Channel, and container identity.",
            {"workerId": worker_id, "expected": expected, "actual": actual},
        )
    return {
        "id": worker_id,
        "podName": pod_name,
        **actual,
    }


def pool_container_for_worker(
    pool: Mapping[str, Any], worker: Mapping[str, Any]
) -> dict[str, Any]:
    pod_name = worker.get("podName")
    containers = pool.get("containers")
    if not isinstance(pod_name, str) or not pod_name or not isinstance(containers, list):
        raise acceptance.AcceptanceError(
            "runner.worker_release_pool_identity_invalid",
            "Busy Worker container evidence was malformed.",
        )
    matches = [
        container
        for container in containers
        if isinstance(container, dict) and container.get("name") == pod_name
    ]
    if len(matches) != 1:
        raise acceptance.AcceptanceError(
            "runner.worker_release_busy_container_missing",
            "The active Execution Worker did not map to exactly one managed Docker container.",
            {"podName": pod_name, "matchCount": len(matches)},
        )
    return dict(matches[0])


def validate_busy_container_preserved(
    before: Mapping[str, Any], after: Mapping[str, Any]
) -> dict[str, Any]:
    expected = {
        key: before.get(key)
        for key in ("id", "name", "imageId", "digest", "revisionId", "channel", "volume")
    }
    actual = {key: after.get(key) for key in expected}
    if not isinstance(expected["id"], str) or not expected["id"] or actual != expected:
        raise acceptance.AcceptanceError(
            "runner.worker_release_busy_container_replaced",
            "Canary rollout replaced or rebound the Worker that still held an active Execution Lease.",
            {"expected": expected, "actual": actual},
        )
    return {**actual, "preservedWhileBusy": True}


def validate_active_execution_conflict(
    conflict: Mapping[str, Any], *, revision_id: str, channel: str
) -> None:
    details = conflict.get("details")
    if (
        not isinstance(details, dict)
        or details.get("releaseRevisionId") != revision_id
        or details.get("releaseChannel") != channel
        or not isinstance(details.get("activeExecutions"), int)
        or details["activeExecutions"] < 1
    ):
        raise acceptance.AcceptanceError(
            "runner.worker_release_active_execution_conflict_mismatch",
            "Worker Release promotion did not identify the exact busy release being retired.",
            {"expectedRevisionId": revision_id, "expectedChannel": channel},
        )


def validate_revision(
    revision: Mapping[str, Any],
    image: ReleaseImage,
    manifest_id: str,
) -> None:
    if (
        revision.get("workerManifestId") != manifest_id
        or revision.get("workerBuildVersion") != image.version
        or revision.get("imageDigest") != image.digest
        or not isinstance(revision.get("id"), str)
        or not isinstance(revision.get("revision"), int)
    ):
        raise acceptance.AcceptanceError(
            "runner.worker_release_revision_identity_mismatch",
            "An immutable Release Revision did not retain its Worker image identity.",
            {"slot": image.slot, "revision": revision_evidence(revision)},
        )


def validate_policy(
    policy: Mapping[str, Any],
    *,
    expected_version: int,
    promoted_id: str,
    canary_id: str | None,
    canary_percent: int,
) -> None:
    actual = {
        "policyVersion": policy.get("policyVersion"),
        "promotedRevisionId": policy.get("promotedRevisionId"),
        "canaryRevisionId": policy.get("canaryRevisionId"),
        "canaryPercent": policy.get("canaryPercent"),
    }
    expected = {
        "policyVersion": expected_version,
        "promotedRevisionId": promoted_id,
        "canaryRevisionId": canary_id,
        "canaryPercent": canary_percent,
    }
    if actual != expected:
        raise acceptance.AcceptanceError(
            "runner.worker_release_policy_mismatch",
            "Worker Release policy did not match the expected strict-CAS transition.",
            {"expected": expected, "actual": actual},
        )


def expect_problem(
    api: Any,
    method: str,
    path: str,
    payload: Mapping[str, Any],
    code: str,
) -> dict[str, Any]:
    try:
        api.request(method, path, payload)
    except acceptance.HTTPFailure as error:
        if error.code != code or error.evidence.get("status") != 409:
            raise acceptance.AcceptanceError(
                "runner.expected_problem_mismatch",
                "A Worker Release negative assertion returned the wrong Problem code.",
                {
                    "expectedCode": code,
                    "actualCode": error.code,
                    "actual": error.evidence,
                },
            ) from None
        problem = error.evidence.get("problem")
        return {
            "status": error.evidence.get("status"),
            "code": error.code,
            "details": problem.get("details") if isinstance(problem, dict) else None,
        }
    raise acceptance.AcceptanceError(
        "runner.expected_problem_missing",
        "A Worker Release negative assertion unexpectedly succeeded.",
        {"expectedCode": code, "path": path},
    )


def validate_execution_release_events(
    events: Sequence[Mapping[str, Any]],
    *,
    turn_id: str,
    revision_id: str,
    channel: str,
    manifest_id: str,
    terminal_required: bool,
) -> dict[str, Any]:
    created = next(
        (
            event
            for event in events
            if event.get("eventType") == "turn.created"
            and isinstance(event.get("payload"), dict)
            and event["payload"].get("turnId") == turn_id
        ),
        None,
    )
    if created is None:
        raise PendingReleaseEvidence()
    execution_id = acceptance.AcceptanceSuite._event_execution_id(created)
    matching = [event for event in events if event.get("executionId") == execution_id]
    leased = next((event for event in matching if event.get("eventType") == "execution.leased"), None)
    if leased is None:
        raise PendingReleaseEvidence()
    for label, event in (("turn.created", created), ("execution.leased", leased)):
        payload = event.get("payload")
        if (
            not isinstance(payload, dict)
            or payload.get("workerReleaseRevisionId") != revision_id
            or payload.get("workerReleaseChannel") != channel
        ):
            raise acceptance.AcceptanceError(
                "runner.worker_release_execution_pin_mismatch",
                f"{label} did not retain the expected Worker Release pin.",
                {"executionId": execution_id, "eventType": label},
            )
    leased_payload = leased["payload"]
    if leased_payload.get("workerManifestId") != manifest_id:
        raise acceptance.AcceptanceError(
            "runner.worker_release_execution_manifest_mismatch",
            "The leased Worker manifest did not match the selected Release Revision.",
            {"executionId": execution_id},
        )
    terminals = [
        event for event in matching if event.get("eventType") in acceptance.TERMINAL_EVENT_TYPES
    ]
    if len(terminals) > 1:
        raise acceptance.AcceptanceError(
            "runner.turn_terminal_duplicate",
            "A Worker Release Execution emitted more than one terminal event.",
            {"executionId": execution_id, "terminalCount": len(terminals)},
        )
    if terminal_required and len(terminals) != 1:
        raise PendingReleaseEvidence()
    terminal = terminals[0] if terminals else None
    return {
        "turnId": turn_id,
        "executionId": execution_id,
        "workerId": leased.get("workerId"),
        "generation": leased.get("generation"),
        "workerManifestId": manifest_id,
        "workerReleaseRevisionId": revision_id,
        "workerReleaseChannel": channel,
        "terminal": (
            acceptance.AcceptanceSuite._event_summary(terminal)
            if isinstance(terminal, Mapping)
            else None
        ),
        "terminalCount": len(terminals),
        "sequenceRange": acceptance.AcceptanceSuite._sequence_range(matching),
    }


def validate_release_overview(
    overview: Mapping[str, Any],
    *,
    baseline_revision_id: str,
    candidate_revision_id: str,
    baseline_digest: str,
    candidate_digest: str,
) -> dict[str, Any]:
    policy = overview.get("policy")
    revisions = overview.get("revisions")
    transitions = overview.get("transitions")
    if not isinstance(policy, dict) or not isinstance(revisions, list) or not isinstance(transitions, list):
        raise acceptance.AcceptanceError(
            "runner.worker_release_overview_invalid",
            "Final Worker Release overview was malformed.",
        )
    validate_policy(
        policy,
        expected_version=4,
        promoted_id=baseline_revision_id,
        canary_id=None,
        canary_percent=0,
    )
    revision_by_id = {
        item.get("id"): item for item in revisions if isinstance(item, dict)
    }
    if set(revision_by_id) != {baseline_revision_id, candidate_revision_id}:
        raise acceptance.AcceptanceError(
            "runner.worker_release_revision_history_invalid",
            "Final Worker Release history did not retain exactly two immutable Revisions.",
        )
    if (
        revision_by_id[baseline_revision_id].get("imageDigest") != baseline_digest
        or revision_by_id[candidate_revision_id].get("imageDigest") != candidate_digest
    ):
        raise acceptance.AcceptanceError(
            "runner.worker_release_revision_history_invalid",
            "Final Worker Release history changed an immutable image digest.",
        )
    transition_by_version = {
        item.get("policyVersion"): item
        for item in transitions
        if isinstance(item, dict) and isinstance(item.get("policyVersion"), int)
    }
    if set(transition_by_version) != {1, 2, 3, 4}:
        raise acceptance.AcceptanceError(
            "runner.worker_release_transition_history_invalid",
            "Final Worker Release history did not contain exactly four policy transitions.",
            {"policyVersions": sorted(transition_by_version)},
        )
    for version, action in EXPECTED_POLICY_ACTIONS:
        transition = transition_by_version[version]
        if transition.get("action") != action:
            raise acceptance.AcceptanceError(
                "runner.worker_release_transition_history_invalid",
                "A Worker Release transition changed action or order.",
                {"policyVersion": version, "action": transition.get("action")},
            )
    expected_targets = {
        1: (baseline_revision_id, None, 0),
        2: (baseline_revision_id, candidate_revision_id, 100),
        3: (candidate_revision_id, None, 0),
        4: (baseline_revision_id, None, 0),
    }
    for version, (promoted, canary, percent) in expected_targets.items():
        transition = transition_by_version[version]
        if (
            transition.get("toPromotedRevisionId") != promoted
            or transition.get("toCanaryRevisionId") != canary
            or transition.get("canaryPercent") != percent
        ):
            raise acceptance.AcceptanceError(
                "runner.worker_release_transition_history_invalid",
                "A Worker Release transition changed its immutable target state.",
                {"policyVersion": version},
            )
    return {
        "policy": policy_evidence(policy),
        "revisionIds": sorted(revision_by_id),
        "transitionVersions": sorted(transition_by_version),
        "transitionActions": [transition_by_version[index]["action"] for index in range(1, 5)],
    }


def validate_release_audit(
    items: Sequence[Mapping[str, Any]],
    *,
    target_id: str,
    revision_ids: set[str],
) -> dict[str, Any]:
    release_items = [item for item in items if str(item.get("action", "")).startswith("worker_release.")]
    revision_items = [item for item in release_items if item.get("action") == "worker_release.revision_created"]
    policy_items = [item for item in release_items if item.get("action") != "worker_release.revision_created"]
    if {item.get("resourceId") for item in revision_items} != revision_ids or len(revision_items) != 2:
        raise acceptance.AcceptanceError(
            "runner.worker_release_audit_invalid",
            "Worker Release audit did not retain exactly two Revision creation entries.",
        )
    by_version: dict[int, Mapping[str, Any]] = {}
    for item in policy_items:
        metadata = item.get("metadata")
        version = metadata.get("policyVersion") if isinstance(metadata, dict) else None
        if isinstance(version, int):
            by_version[version] = item
    if set(by_version) != {1, 2, 3, 4} or len(policy_items) != 4:
        raise acceptance.AcceptanceError(
            "runner.worker_release_audit_invalid",
            "Worker Release audit did not retain exactly four policy entries.",
            {"policyVersions": sorted(by_version)},
        )
    for version, action in EXPECTED_AUDIT_ACTIONS.items():
        item = by_version[version]
        if item.get("action") != action or item.get("resourceId") != target_id:
            raise acceptance.AcceptanceError(
                "runner.worker_release_audit_invalid",
                "A Worker Release audit entry did not match its target or policy action.",
                {"policyVersion": version},
            )
    return {
        "revisionEntryCount": len(revision_items),
        "policyEntryCount": len(policy_items),
        "policyActions": [by_version[index]["action"] for index in range(1, 5)],
    }


def validate_release_outbox(
    items: Sequence[Mapping[str, Any]],
    *,
    target_id: str,
    revision_ids: set[str],
) -> dict[str, Any]:
    release_items = [item for item in items if str(item.get("topic", "")).startswith("worker.release.")]
    expected = {
        ("worker.release.revision-created", revision_id) for revision_id in revision_ids
    }
    expected.update(
        (topic, f"{target_id}:{version}")
        for version, topic in EXPECTED_OUTBOX_TOPICS.items()
    )
    actual = {(item.get("topic"), item.get("messageKey")) for item in release_items}
    if actual != expected or len(release_items) != len(expected):
        raise acceptance.AcceptanceError(
            "runner.worker_release_outbox_invalid",
            "Worker Release Outbox did not retain exactly the immutable Revision and policy messages.",
            {"expectedCount": len(expected), "actualCount": len(release_items)},
        )
    invalid_statuses = [
        item.get("status")
        for item in release_items
        if item.get("status") not in {"pending", "retrying", "published"}
    ]
    if invalid_statuses:
        raise acceptance.AcceptanceError(
            "runner.worker_release_outbox_invalid",
            "Worker Release Outbox contained a dead-lettered or unknown status.",
            {"statuses": invalid_statuses},
        )
    return {
        "messageCount": len(release_items),
        "topics": sorted(str(item.get("topic")) for item in release_items),
        "statuses": sorted({str(item.get("status")) for item in release_items}),
    }


def markdown_from_report(report: Mapping[str, Any]) -> str:
    lines = [
        "# Stage 3 Docker Worker Release Rollout Gate",
        "",
        f"- Schema: `{report.get('schemaVersion')}`",
        f"- Run: `{report.get('runId')}`",
        f"- Status: **{report.get('status')}**",
        f"- Git SHA: `{report.get('source', {}).get('gitSha')}`",
        f"- Duration: `{report.get('durationMs')} ms`",
        "",
        "## Evidence boundary",
        "",
        "This gate proves the product Docker Target path for two registry-pushed immutable Worker images: "
        "Revision creation, initial promotion, Busy Worker preservation, canary, active-Execution fencing, "
        "promotion, rollback, exact "
        "container/manifest/Execution release pins, audit/Outbox history, Secret scan, and exact cleanup. It does "
        "not replace production Registry Credential/TLS, real Provider Credentials, Kubernetes multi-node rollout, "
        "load, or soak evidence.",
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
    if isinstance(error, common.ReleaseGateError):
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
        "name": "Require a clean source and initialize the Docker rollout gate",
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
    run_id = f"stage3-worker-release-rollout-{uuid.uuid4()}"
    source: Mapping[str, Any] = acceptance.repository_metadata(options.repo_root)
    cases: list[dict[str, Any]] = []
    driver: DockerWorkerReleaseRolloutDriver | None = None
    suite: WorkerReleaseRolloutSuite | None = None
    try:
        common.repository_state(options.repo_root)
        if os.name != "posix":
            raise acceptance.AcceptanceError(
                "runner.platform_unsupported",
                "The Docker Worker Release rollout gate requires POSIX process groups.",
            )
        child_options = runner_options(options)
        driver = DockerWorkerReleaseRolloutDriver(
            options, child_options, deadline, redactor
        )
        suite = WorkerReleaseRolloutSuite(child_options, driver, deadline, redactor)
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
                        "runner.worker_release_rollout_cleanup_failed",
                        "Worker Release rollout cleanup raised an unexpected error.",
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
                    "Worker Release rollout initialization failed and its exact cleanup also failed.",
                    {
                        "initialExceptionType": error.__class__.__name__,
                        "cleanupExceptionType": cleanup_error.__class__.__name__,
                    },
                )
        cases = [failure_case(error, started_at=started_at, started=started)]
    report: dict[str, Any] = {
        "schemaVersion": SCHEMA_VERSION,
        "runId": run_id,
        "mode": "docker-worker-release-rollout",
        "target": "docker",
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
            "dockerControlPlaneHost": options.docker_control_plane_host,
            "dockerMemoryBytes": options.docker_memory_bytes,
            "dockerNanoCpus": options.docker_nano_cpus,
            "registryImage": options.registry_image,
            "registryAuthentication": False,
            "registryTLS": False,
            "desiredWorkers": 2,
            "candidateObserverWorkers": 1,
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
    print(f"Stage 3 Docker Worker Release rollout: {report['status']}")
    print(f"JSON: {json_path}")
    print(f"Markdown: {markdown_path}")
    return 0 if report["status"] == "pass" else 1


if __name__ == "__main__":
    raise SystemExit(main())
