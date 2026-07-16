from __future__ import annotations

import contextlib
import hashlib
import io
import json
import pathlib
import subprocess
import tempfile
import unittest
from collections.abc import Callable
from typing import Any
from unittest import mock

import registry_release_gate as gate


REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
GIT_SHA = "a" * 40
VERSION = "0.0.0"
SOURCE_DATE_EPOCH = "1700000000"
INDEX_DIGESTS = {
    "linux/amd64": "sha256:" + "a" * 64,
    "linux/arm64": "sha256:" + "b" * 64,
}
ATTESTATION_DIGESTS = {
    "linux/amd64": "sha256:" + "c" * 64,
    "linux/arm64": "sha256:" + "d" * 64,
}
EMBEDDED_LOCK_FILE_NAMES = {
    "provider-tools-npm": "providerToolsLock",
    "provider-host-bun": "providerHostLock",
    "worker-apk": "workerAPKLock",
}


def options(
    output_dir: pathlib.Path,
    *,
    repo_root: pathlib.Path = REPO_ROOT,
) -> gate.RegistryReleaseGateOptions:
    return gate.RegistryReleaseGateOptions(
        repo_root=repo_root,
        output_dir=output_dir,
        image_repository="localhost:55091/synara/worker",
        builder="synara-stage3-registry-builder",
        build_timeout_seconds=7200.0,
        supply_chain_timeout_seconds=1800.0,
        docker_bin="docker",
        go_proxy="https://goproxy.cn,direct",
        insecure_registry=True,
    )


def image_config(platform: str, *, extra_environment: list[str] | None = None) -> dict[str, Any]:
    os_name, architecture = platform.split("/", 1)
    environment = [
        f"{name}={value}" for name, value in gate.EXPECTED_ENVIRONMENT.items()
    ]
    environment.extend(extra_environment or [])
    return {
        "os": os_name,
        "architecture": architecture,
        "created": gate.expected_created_at(SOURCE_DATE_EPOCH),
        "config": {
            "User": gate.EXPECTED_USER,
            "Entrypoint": list(gate.EXPECTED_ENTRYPOINT),
            "Cmd": None,
            "WorkingDir": gate.EXPECTED_WORKING_DIRECTORY,
            "Env": environment,
            "Labels": {
                "org.opencontainers.image.title": "Synara Worker",
                "org.opencontainers.image.version": VERSION,
                "org.opencontainers.image.revision": GIT_SHA,
            },
        },
        "rootfs": {"diff_ids": ["sha256:" + "e" * 64]},
    }


def attestation_manifest() -> dict[str, Any]:
    return {
        "schemaVersion": 2,
        "mediaType": gate.OCI_MANIFEST_MEDIA_TYPE,
        "layers": [
            {
                "annotations": {
                    "in-toto.io/predicate-type": gate.SPDX_PREDICATE,
                }
            },
            {
                "annotations": {
                    "in-toto.io/predicate-type": f"{gate.SLSA_PREDICATE_PREFIX}v0.2",
                }
            },
        ],
    }


def index_payload(*, attestation_mode: str = "complete") -> dict[str, Any]:
    manifests: list[dict[str, Any]] = [
        {
            "mediaType": gate.OCI_MANIFEST_MEDIA_TYPE,
            "digest": digest,
            "platform": {
                "os": platform.split("/", 1)[0],
                "architecture": platform.split("/", 1)[1],
            },
        }
        for platform, digest in INDEX_DIGESTS.items()
    ]
    attestations = [
        {
            "mediaType": gate.OCI_MANIFEST_MEDIA_TYPE,
            "digest": ATTESTATION_DIGESTS[platform],
            "platform": {"os": "unknown", "architecture": "unknown"},
            "annotations": {
                "vnd.docker.reference.type": gate.ATTESTATION_TYPE,
                "vnd.docker.reference.digest": INDEX_DIGESTS[platform],
            },
        }
        for platform in gate.REQUIRED_PLATFORMS
    ]
    if attestation_mode == "missing":
        attestations.pop()
    elif attestation_mode == "duplicate":
        attestations.append(dict(attestations[0]))
    manifests.extend(attestations)
    return {
        "schemaVersion": 2,
        "mediaType": gate.OCI_INDEX_MEDIA_TYPE,
        "manifests": manifests,
    }


def registry_inspector(
    *,
    attestation_mode: str = "complete",
    mutate_attestation: Callable[[dict[str, Any]], None] | None = None,
) -> tuple[Callable[..., tuple[dict[str, Any], bytes]], str]:
    index = index_payload(attestation_mode=attestation_mode)
    raw_index = json.dumps(index, separators=(",", ":")).encode("utf-8")
    expected_digest = "sha256:" + hashlib.sha256(raw_index).hexdigest()

    def inspect(
        _options: gate.RegistryReleaseGateOptions,
        arguments: list[str],
        **_kwargs: Any,
    ) -> tuple[dict[str, Any], bytes]:
        reference = arguments[-1]
        if arguments[-2:] == ["--raw", "example.invalid/synara/worker:tag"]:
            return index, raw_index
        if reference.startswith("localhost:55091/synara/worker@"):
            payload = attestation_manifest()
            if mutate_attestation is not None:
                mutate_attestation(payload)
            return payload, json.dumps(payload).encode("utf-8")
        if arguments[-2:] == ["--format", "{{json .Image}}"]:
            payload = {
                platform: image_config(platform) for platform in gate.REQUIRED_PLATFORMS
            }
            return payload, json.dumps(payload).encode("utf-8")
        raise AssertionError(f"unexpected inspect arguments: {arguments}")

    return inspect, expected_digest


def write_embedded_fixture(
    root: pathlib.Path,
    *,
    mutate_manifest: Callable[[dict[str, Any]], None] | None = None,
    mutate_sbom: Callable[[dict[str, Any]], None] | None = None,
) -> dict[str, pathlib.Path]:
    paths = {name: root / name for name in gate.EMBEDDED_PATHS}
    for path in paths.values():
        path.parent.mkdir(parents=True, exist_ok=True)

    runtimes = gate._expected_provider_runtimes(REPO_ROOT)
    for name, local_path in gate.LOCAL_LOCK_PATHS.items():
        embedded_name = EMBEDDED_LOCK_FILE_NAMES[name]
        paths[embedded_name].write_bytes((REPO_ROOT / local_path).read_bytes())

    sbom = {
        "spdxVersion": "SPDX-2.3",
        "creationInfo": {"created": gate.expected_sbom_created_at(SOURCE_DATE_EPOCH)},
        "packages": [
            {"name": runtime["package"], "versionInfo": runtime["version"]}
            for runtime in runtimes
            if runtime["kind"] == "cli"
        ],
    }
    if mutate_sbom is not None:
        mutate_sbom(sbom)
    sbom_bytes = (json.dumps(sbom, sort_keys=True) + "\n").encode("utf-8")
    paths["sbom"].write_bytes(sbom_bytes)

    lockfiles = []
    for name, local_path in gate.LOCAL_LOCK_PATHS.items():
        embedded_name = EMBEDDED_LOCK_FILE_NAMES[name]
        lockfiles.append(
            {
                "name": name,
                "path": gate.EMBEDDED_PATHS[embedded_name],
                "sha256": hashlib.sha256(paths[embedded_name].read_bytes()).hexdigest(),
            }
        )
    manifest = {
        "schemaVersion": 1,
        "source": {"version": VERSION, "gitSha": GIT_SHA},
        "platform": {"os": "linux", "architecture": "amd64"},
        "baseImages": [
            {"name": name, "reference": f"example.invalid/{name}@sha256:{index * 64}"}
            for index, name in zip("123", ("agentd-build", "provider-host-build", "worker-runtime"))
        ],
        "lockfiles": lockfiles,
        "providerRuntimes": runtimes,
        "sboms": [
            {
                "name": "provider-tools",
                "format": "spdx-json",
                "path": gate.EMBEDDED_PATHS["sbom"],
                "sha256": hashlib.sha256(sbom_bytes).hexdigest(),
            }
        ],
    }
    if mutate_manifest is not None:
        mutate_manifest(manifest)
    paths["manifest"].write_text(
        json.dumps(manifest, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    paths["providerHost"].write_bytes(b"provider-host-bundle")
    paths["agentd"].write_bytes(b"agentd-binary")
    paths["buildRevision"].write_text(f"{GIT_SHA}\n", encoding="utf-8")
    paths["providerHostWrapper"].write_text(
        '#!/bin/sh\nexec node /opt/synara/provider-host/index.mjs "$@"\n',
        encoding="utf-8",
    )
    return paths


def sample_build(slot: str, no_cache: bool) -> dict[str, Any]:
    return {
        "slot": slot,
        "noCache": no_cache,
        "registryDigest": "sha256:" + ("1" if no_cache else "0") * 64,
        "platformDigests": dict(INDEX_DIGESTS),
        "embedded": {
            platform: {"providerHostSha256": "f" * 64}
            for platform in gate.REQUIRED_PLATFORMS
        },
    }


def sample_supply_chain() -> dict[str, Any]:
    return {
        "status": "pass",
        "mode": "registry-supply-chain",
        "tools": {"versions": {"cosign": "v3.1.1", "trivy": "0.72.0"}},
        "signing": {
            "mode": "ephemeral-key",
            "productionSigningPolicySatisfied": False,
            "signatures": [{"slot": "cached"}, {"slot": "no-cache"}],
        },
        "vulnerability": {
            "scans": [
                {
                    "platform": platform,
                    "vulnerabilities": {
                        "total": 0,
                        "bySeverity": {
                            severity: 0 for severity in gate.supply_chain.SUPPORTED_SEVERITIES
                        },
                    },
                    "secretFindingCount": 0,
                    "os": {"EOSL": False},
                }
                for platform in gate.REQUIRED_PLATFORMS
            ]
        },
        "cleanup": {"ephemeralPrivateKeyRemoved": True, "broadCleanupUsed": False},
        "errors": [],
    }


class InputValidationTest(unittest.TestCase):
    def test_accepts_registry_port_nested_repository_and_public_go_proxy(self) -> None:
        self.assertEqual(
            gate.normalize_image_repository("localhost:55091/synara/worker-image"),
            "localhost:55091/synara/worker-image",
        )
        self.assertEqual(
            gate.normalize_go_proxy("https://goproxy.cn,direct"),
            "https://goproxy.cn,direct",
        )

    def test_rejects_tag_digest_credentials_query_and_invalid_repository_shape(self) -> None:
        invalid = [
            "registry.example.test/team/worker:latest",
            "registry.example.test/team/worker@sha256:" + "a" * 64,
            "user@registry.example.test/team/worker",
            "registry.example.test/team/worker?debug=1",
            "registry.example.test/Team/worker",
            "registry.example.test/team/worker/",
            "localhost:70000/team/worker",
        ]
        for value in invalid:
            with self.subTest(value=value), self.assertRaises(ValueError):
                gate.normalize_image_repository(value)

    def test_rejects_non_https_or_credential_like_go_proxy_values(self) -> None:
        invalid = [
            "http://proxy.example.test,direct",
            "https://user:password@proxy.example.test,direct",
            "https://proxy.example.test?token=value,direct",
            "https://proxy.example.test, direct",
            "https://proxy.example.test,,direct",
        ]
        for value in invalid:
            with self.subTest(value=value), self.assertRaises(ValueError):
                gate.normalize_go_proxy(value)

    def test_build_inputs_keep_proxy_no_cache_and_pinned_sbom_generator(self) -> None:
        command = gate.build_command(
            options(pathlib.Path("/tmp/output")),
            image="localhost:55091/synara/worker:tag",
            git_sha=GIT_SHA,
            version=VERSION,
            source_date_epoch=SOURCE_DATE_EPOCH,
            metadata_path=pathlib.Path("/tmp/metadata.json"),
            no_cache=True,
        )

        self.assertIn("--push", command)
        self.assertIn("--no-cache", command)
        self.assertEqual(command[command.index("--go-proxy") + 1], "https://goproxy.cn,direct")
        self.assertEqual(
            gate.locked_sbom_generator(REPO_ROOT),
            "docker.io/docker/buildkit-syft-scanner@sha256:"
            "79e7b013cbec16bbb436f312819a49a4a57752b2270c1a9332ae1a10fcc82a68",
        )
        build_script = (REPO_ROOT / "deploy/worker/build.sh").read_text(encoding="utf-8")
        dockerfile = (REPO_ROOT / "Dockerfile").read_text(encoding="utf-8")
        self.assertIn("type=image,push=true,rewrite-timestamp=true", build_script)
        self.assertIn("--sbom=generator=$sbom_generator", build_script)
        self.assertIn("rm -f /var/log/apk.log", dockerfile)
        self.assertIn("--mount=from=worker-provider-tools", dockerfile)
        self.assertIn('/opt/synara/.build-revision', dockerfile)
        self.assertIn('touch -d "@${SOURCE_DATE_EPOCH}" /out/synara-agentd', dockerfile)
        self.assertIn('touch -d "@${SOURCE_DATE_EPOCH}" /out/provider-host.mjs', dockerfile)
        self.assertNotIn(
            "COPY --from=worker-provider-tools /tmp/provider-tools.raw.spdx.json",
            dockerfile,
        )

        with tempfile.TemporaryDirectory() as directory:
            repo_root = pathlib.Path(directory)
            lock_path = repo_root / gate.SBOM_GENERATOR_LOCK_PATH
            lock_path.parent.mkdir(parents=True)
            lock_path.write_text("docker.io/docker/buildkit-syft-scanner:stable-1\n")
            with self.assertRaises(gate.ReleaseGateError) as caught:
                gate.locked_sbom_generator(repo_root)
        self.assertEqual(caught.exception.code, "release.registry_source_metadata_invalid")

        generator = gate.locked_sbom_generator(REPO_ROOT)
        generator_digest = generator.rsplit("@sha256:", 1)[-1]
        build_digest = "sha256:" + "8" * 64
        metadata = {
            "containerimage.digest": build_digest,
            "containerimage.descriptor": {
                "mediaType": gate.OCI_INDEX_MEDIA_TYPE,
                "digest": build_digest,
                "size": 123,
            },
            **{
                f"buildx.build.provenance/{platform}": {
                    "materials": [
                        {
                            "uri": "pkg:docker/docker/buildkit-syft-scanner"
                            f"?digest=sha256:{generator_digest}",
                            "digest": {"sha256": generator_digest},
                        }
                    ]
                }
                for platform in gate.REQUIRED_PLATFORMS
            },
        }
        with tempfile.TemporaryDirectory() as directory:
            metadata_path = pathlib.Path(directory) / "metadata.json"
            metadata_path.write_text(json.dumps(metadata), encoding="utf-8")
            evidence = gate._load_build_metadata(
                metadata_path,
                slot="cached",
                expected_sbom_generator=generator,
            )
            self.assertEqual(evidence["sbomGenerator"], generator)

            metadata["buildx.build.provenance/linux/arm64"]["materials"][0]["digest"] = {
                "sha256": "0" * 64
            }
            metadata_path.write_text(json.dumps(metadata), encoding="utf-8")
            with self.assertRaises(gate.ReleaseGateError) as metadata_error:
                gate._load_build_metadata(
                    metadata_path,
                    slot="no-cache",
                    expected_sbom_generator=generator,
                )
            self.assertEqual(
                metadata_error.exception.code,
                "release.registry_build_metadata_invalid",
            )

    def test_worker_build_script_rejects_non_https_proxy_before_docker(self) -> None:
        completed = subprocess.run(
            [
                str(REPO_ROOT / "deploy/worker/build.sh"),
                "--git-sha",
                GIT_SHA,
                "--go-proxy",
                "http://proxy.example.test,direct",
            ],
            cwd=REPO_ROOT,
            check=False,
            capture_output=True,
            text=True,
        )

        self.assertEqual(completed.returncode, 2)
        self.assertIn("entries must use https://", completed.stderr)

        valid_proxy = subprocess.run(
            [
                str(REPO_ROOT / "deploy/worker/build.sh"),
                "--git-sha",
                GIT_SHA,
                "--source-date-epoch",
                "invalid",
                "--go-proxy",
                "https://goproxy.cn,direct",
            ],
            cwd=REPO_ROOT,
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(valid_proxy.returncode, 2)
        self.assertIn("--source-date-epoch", valid_proxy.stderr)


class ImageConfigTest(unittest.TestCase):
    def test_preserves_distinct_oci_and_spdx_timestamp_shapes(self) -> None:
        self.assertEqual(gate.expected_created_at(SOURCE_DATE_EPOCH), "2023-11-14T22:13:20Z")
        self.assertEqual(
            gate.expected_sbom_created_at(SOURCE_DATE_EPOCH),
            "2023-11-14T22:13:20.000Z",
        )

    def test_accepts_non_root_config_with_null_or_empty_cmd(self) -> None:
        for command in (None, []):
            payload = image_config("linux/amd64")
            payload["config"]["Cmd"] = command

            evidence = gate._validate_image_config(
                payload,
                platform="linux/amd64",
                git_sha=GIT_SHA,
                version=VERSION,
                created_at=gate.expected_created_at(SOURCE_DATE_EPOCH),
            )

            self.assertEqual(evidence["user"], gate.EXPECTED_USER)

    def test_rejects_runtime_cmd_and_credential_environment(self) -> None:
        invalid_cmd = image_config("linux/amd64")
        invalid_cmd["config"]["Cmd"] = ["sh"]
        credential_env = image_config(
            "linux/amd64", extra_environment=["SYNARA_API_TOKEN=must-not-be-here"]
        )
        for payload in (invalid_cmd, credential_env):
            with self.assertRaises(gate.ReleaseGateError) as caught:
                gate._validate_image_config(
                    payload,
                    platform="linux/amd64",
                    git_sha=GIT_SHA,
                    version=VERSION,
                    created_at=gate.expected_created_at(SOURCE_DATE_EPOCH),
                )
            self.assertEqual(caught.exception.code, "release.registry_image_config_invalid")


class RegistryInspectionTest(unittest.TestCase):
    def test_accepts_exact_dual_platform_index_attestations_and_configs(self) -> None:
        inspector, expected_digest = registry_inspector()
        with mock.patch.object(gate, "_json_tool_output", side_effect=inspector):
            evidence = gate.inspect_registry_image(
                options(pathlib.Path("/tmp/output")),
                image="example.invalid/synara/worker:tag",
                expected_digest=expected_digest,
                git_sha=GIT_SHA,
                version=VERSION,
                source_date_epoch=SOURCE_DATE_EPOCH,
            )

        self.assertEqual(evidence["indexDigest"], expected_digest)
        self.assertEqual(evidence["platformDigests"], INDEX_DIGESTS)
        self.assertEqual(set(evidence["attestationPredicates"]), set(gate.REQUIRED_PLATFORMS))

    def test_rejects_missing_or_duplicate_attestation_descriptor(self) -> None:
        for mode in ("missing", "duplicate"):
            inspector, expected_digest = registry_inspector(attestation_mode=mode)
            with self.subTest(mode=mode), mock.patch.object(
                gate, "_json_tool_output", side_effect=inspector
            ), self.assertRaises(gate.ReleaseGateError) as caught:
                gate.inspect_registry_image(
                    options(pathlib.Path("/tmp/output")),
                    image="example.invalid/synara/worker:tag",
                    expected_digest=expected_digest,
                    git_sha=GIT_SHA,
                    version=VERSION,
                    source_date_epoch=SOURCE_DATE_EPOCH,
                )
            self.assertEqual(caught.exception.code, "release.registry_attestation_invalid")

    def test_malformed_attestation_layers_raise_stable_gate_error(self) -> None:
        inspector, expected_digest = registry_inspector(
            mutate_attestation=lambda payload: payload.update({"layers": None})
        )
        with mock.patch.object(
            gate, "_json_tool_output", side_effect=inspector
        ), self.assertRaises(gate.ReleaseGateError) as caught:
            gate.inspect_registry_image(
                options(pathlib.Path("/tmp/output")),
                image="example.invalid/synara/worker:tag",
                expected_digest=expected_digest,
                git_sha=GIT_SHA,
                version=VERSION,
                source_date_epoch=SOURCE_DATE_EPOCH,
            )

        self.assertEqual(caught.exception.code, "release.registry_attestation_invalid")


class EmbeddedArtifactTest(unittest.TestCase):
    def test_accepts_manifest_sbom_lockfiles_and_runtime_files_from_source(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            files = write_embedded_fixture(pathlib.Path(directory))
            evidence = gate.validate_embedded_artifacts(
                options(pathlib.Path(directory) / "output"),
                files,
                platform="linux/amd64",
                git_sha=GIT_SHA,
                version=VERSION,
                source_date_epoch=SOURCE_DATE_EPOCH,
            )

        self.assertEqual(set(evidence["lockfileSha256"]), set(gate.LOCAL_LOCK_PATHS))
        self.assertEqual(evidence["providerRuntimes"], gate._expected_provider_runtimes(REPO_ROOT))

    def test_rejects_embedded_lockfile_that_differs_from_source(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            files = write_embedded_fixture(pathlib.Path(directory))
            files["providerHostLock"].write_bytes(b"different lock")
            with self.assertRaises(gate.ReleaseGateError) as caught:
                gate.validate_embedded_artifacts(
                    options(pathlib.Path(directory) / "output"),
                    files,
                    platform="linux/amd64",
                    git_sha=GIT_SHA,
                    version=VERSION,
                    source_date_epoch=SOURCE_DATE_EPOCH,
                )

        self.assertEqual(caught.exception.code, "release.registry_embedded_lock_invalid")

    def test_malformed_lockfiles_packages_and_creation_info_raise_gate_error(self) -> None:
        mutations = [
            (lambda manifest: manifest.update({"lockfiles": None}), None),
            (None, lambda sbom: sbom.update({"packages": None})),
            (None, lambda sbom: sbom.update({"creationInfo": None})),
        ]
        for manifest_mutation, sbom_mutation in mutations:
            with self.subTest(
                manifest=manifest_mutation is not None,
                sbom=sbom_mutation is not None,
            ), tempfile.TemporaryDirectory() as directory:
                files = write_embedded_fixture(
                    pathlib.Path(directory),
                    mutate_manifest=manifest_mutation,
                    mutate_sbom=sbom_mutation,
                )
                with self.assertRaises(gate.ReleaseGateError) as caught:
                    gate.validate_embedded_artifacts(
                        options(pathlib.Path(directory) / "output"),
                        files,
                        platform="linux/amd64",
                        git_sha=GIT_SHA,
                        version=VERSION,
                        source_date_epoch=SOURCE_DATE_EPOCH,
                    )
                self.assertEqual(
                    caught.exception.code,
                    "release.registry_embedded_manifest_invalid",
                )


class ReproducibilityTest(unittest.TestCase):
    def test_requires_two_builds_with_identical_platform_digests(self) -> None:
        cached = sample_build("cached", False)
        no_cache = sample_build("no-cache", True)

        self.assertEqual(gate.reproducibility_errors([cached, no_cache]), [])

        no_cache["platformDigests"] = {
            **INDEX_DIGESTS,
            "linux/arm64": "sha256:" + "9" * 64,
        }
        errors = gate.reproducibility_errors([cached, no_cache])
        self.assertEqual(errors[0]["code"], "release.registry_platform_digest_mismatch")

    def test_rejects_incomplete_build_coverage(self) -> None:
        errors = gate.reproducibility_errors([sample_build("cached", False)])
        self.assertEqual(errors[0]["code"], "release.registry_build_coverage_incomplete")


class AggregateGateTest(unittest.TestCase):
    def test_emits_pass_report_for_cached_and_no_cache_consensus(self) -> None:
        sha = subprocess.run(
            ["git", "rev-parse", "HEAD"],
            cwd=REPO_ROOT,
            check=True,
            capture_output=True,
            text=True,
        ).stdout.strip()
        with tempfile.TemporaryDirectory() as directory:
            output_dir = pathlib.Path(directory) / "gate"
            gate_options = options(output_dir)

            def build_runner(_options: Any, **kwargs: Any) -> dict[str, Any]:
                return sample_build(kwargs["slot"], kwargs["no_cache"])

            with contextlib.redirect_stdout(io.StringIO()):
                exit_code = gate.run_registry_release_gate(
                    gate_options,
                    repository_state=lambda _root: {"gitSha": sha, "worktreeDirty": False},
                    runtime_inspector=lambda _options: {"builder": gate_options.builder},
                    build_runner=build_runner,
                    supply_chain_runner=lambda *_args, **_kwargs: sample_supply_chain(),
                )
            report = json.loads((output_dir / gate.JSON_REPORT_NAME).read_text(encoding="utf-8"))

        self.assertEqual(exit_code, 0)
        self.assertEqual(report["status"], "pass")
        self.assertEqual(len(report["builds"]), 2)
        self.assertEqual(report["supplyChain"]["status"], "pass")
        self.assertEqual(report["security"]["outputSecretScan"]["findings"], [])

    def test_emits_fail_report_when_supply_chain_policy_fails(self) -> None:
        sha = subprocess.run(
            ["git", "rev-parse", "HEAD"],
            cwd=REPO_ROOT,
            check=True,
            capture_output=True,
            text=True,
        ).stdout.strip()
        with tempfile.TemporaryDirectory() as directory:
            output_dir = pathlib.Path(directory) / "gate"
            gate_options = options(output_dir)

            def build_runner(_options: Any, **kwargs: Any) -> dict[str, Any]:
                return sample_build(kwargs["slot"], kwargs["no_cache"])

            supply_chain_report = sample_supply_chain()
            supply_chain_report["status"] = "fail"
            supply_chain_report["errors"] = [
                {
                    "code": "release.registry_vulnerability_policy_blocked",
                    "message": "critical vulnerability",
                }
            ]
            with contextlib.redirect_stdout(io.StringIO()):
                exit_code = gate.run_registry_release_gate(
                    gate_options,
                    repository_state=lambda _root: {"gitSha": sha, "worktreeDirty": False},
                    runtime_inspector=lambda _options: {"builder": gate_options.builder},
                    build_runner=build_runner,
                    supply_chain_runner=lambda *_args, **_kwargs: supply_chain_report,
                )
            report = json.loads((output_dir / gate.JSON_REPORT_NAME).read_text(encoding="utf-8"))

        self.assertEqual(exit_code, 1)
        self.assertEqual(report["status"], "fail")
        self.assertEqual(
            {error["code"] for error in report["errors"]},
            {"release.registry_vulnerability_policy_blocked"},
        )

    def test_emits_fail_report_when_no_cache_build_fails(self) -> None:
        sha = subprocess.run(
            ["git", "rev-parse", "HEAD"],
            cwd=REPO_ROOT,
            check=True,
            capture_output=True,
            text=True,
        ).stdout.strip()
        with tempfile.TemporaryDirectory() as directory:
            output_dir = pathlib.Path(directory) / "gate"
            gate_options = options(output_dir)

            def build_runner(_options: Any, **kwargs: Any) -> dict[str, Any]:
                if kwargs["no_cache"]:
                    raise gate.ReleaseGateError(
                        "release.registry_build_failed",
                        "no-cache build failed",
                    )
                return sample_build(kwargs["slot"], kwargs["no_cache"])

            with contextlib.redirect_stdout(io.StringIO()):
                exit_code = gate.run_registry_release_gate(
                    gate_options,
                    repository_state=lambda _root: {"gitSha": sha, "worktreeDirty": False},
                    runtime_inspector=lambda _options: {"builder": gate_options.builder},
                    build_runner=build_runner,
                )
            report = json.loads((output_dir / gate.JSON_REPORT_NAME).read_text(encoding="utf-8"))

        self.assertEqual(exit_code, 1)
        self.assertEqual(report["status"], "fail")
        self.assertEqual(
            {error["code"] for error in report["errors"]},
            {
                "release.registry_build_failed",
                "release.registry_build_coverage_incomplete",
            },
        )


if __name__ == "__main__":
    unittest.main()
