from __future__ import annotations

import contextlib
import dataclasses
import io
import json
import os
import pathlib
import shutil
import tempfile
import unittest
from typing import Any
from unittest import mock

import controlled_remote_release_gate as remote
import controlled_remote_release_checkpoint as checkpoint_module
import docker_release_gate as gate
import release_gate_common as common
from test_docker_release_gate import (
    WORKER_IMAGE_ID,
    docker_options,
    sample_child_report,
    write_child_report_artifacts,
)


GIT_SHA = "a" * 40
PROCESS_OUTPUT_SCAN = {
    "captured": True,
    "rawOutputPersisted": False,
    "findings": [],
}
TEST_RUNTIME = {
    "docker": {
        "serverVersion": "29.4.0",
        "platform": "linux/arm64",
    }
}


class SimulatedInterrupt(BaseException):
    pass


def controlled_options(output_dir: pathlib.Path) -> gate.DockerReleaseGateOptions:
    return dataclasses.replace(docker_options(output_dir), resume=True)


def controlled_spec() -> remote.RemoteReleaseTargetSpec:
    return remote.RemoteReleaseTargetSpec(
        target="docker",
        display_name="Docker",
        schema_version="synara.provider-remote-release-gate-test.v1",
        json_report_name="controlled-remote-release-gate.json",
        markdown_report_name="controlled-remote-release-gate.md",
        mode="test-controlled-remote-release-gate",
        worker_image_repository="synara-stage3-provider-release-gate",
        expected_unsupported=gate.EXPECTED_UNSUPPORTED,
        cleanup_true_fields=(
            "managedWorkerContainersRemoved",
            "workspaceVolumeRemoved",
            "ownedNetworkRemoved",
            "stateRemoved",
        ),
        cleanup_false_fields=("broadCleanupUsed", "ownedImageRemoved"),
        worker_image_evidence_path=("docker",),
        child_command=gate.child_command,
        inspect_runtime=lambda _options: TEST_RUNTIME,
        target_configuration=gate.target_configuration,
        evidence_boundary=("checkpoint test boundary start", "checkpoint test boundary end"),
        matrices=gate.REMOTE_GATE_MATRICES,
        validate_reused_child_runtime=None,
    )


class ControlledRemoteReleaseGateTest(unittest.TestCase):
    def environment(self, **overrides: str) -> dict[str, str]:
        return {
            "CODEX_KEY": "codex-secret-value",
            "CLAUDE_TOKEN": "claude-secret-value",
            "CLAUDE_BASE_URL": "https://claude.example.test",
            **overrides,
        }

    def repository_state(self, _repo_root: pathlib.Path) -> dict[str, Any]:
        return {"gitSha": GIT_SHA, "worktreeDirty": False}

    def inspect_gate_worker_image(
        self,
        _options: Any,
        image: remote.GateWorkerImage,
    ) -> tuple[str, dict[str, str]]:
        return WORKER_IMAGE_ID, remote.required_gate_worker_image_labels(image, GIT_SHA)

    def build_image(
        self,
        options: gate.DockerReleaseGateOptions,
        image: remote.GateWorkerImage,
        git_sha: str,
    ) -> dict[str, Any]:
        metadata_path = options.output_dir / "worker-image-build-metadata.json"
        metadata_path.write_text(
            json.dumps({"gitSha": git_sha, "image": image.name}, sort_keys=True) + "\n",
            encoding="utf-8",
        )
        return {
            "name": image.name,
            "id": WORKER_IMAGE_ID,
            "status": "completed",
            "metadataPath": metadata_path.name,
            "metadataSha256": common.file_sha256(metadata_path),
            "ownershipVerified": True,
        }

    def cleanup_image(
        self,
        _options: gate.DockerReleaseGateOptions,
        image: remote.GateWorkerImage,
        *,
        expected_image_id: str | None,
    ) -> tuple[dict[str, Any], None]:
        return (
            {
                "name": image.name,
                "expectedImageId": expected_image_id,
                "presentBeforeCleanup": True,
                "ownershipVerified": True,
                "removed": True,
                "broadCleanupUsed": False,
            },
            None,
        )

    def child_run_factory(
        self,
        options: gate.DockerReleaseGateOptions,
        *,
        failing_pair: tuple[str, str] | None = None,
        calls: list[tuple[str, str]] | None = None,
        report_mutator: Any | None = None,
        markdown_factory: Any | None = None,
    ):
        def run_child(**kwargs: Any) -> tuple[dict[str, Any], list[dict[str, Any]]]:
            provider = str(kwargs["provider"])
            matrix = str(kwargs["matrix"])
            if calls is not None:
                calls.append((provider, matrix))
            child_dir = pathlib.Path(kwargs["output_dir"]) / provider / matrix
            worker_image_name = kwargs["policy"].expected_worker_image_name
            assert isinstance(worker_image_name, str)
            report = sample_child_report(
                options,
                provider,
                matrix,
                git_sha=str(kwargs["expected_git_sha"]),
                worker_image_name=worker_image_name,
            )
            if report_mutator is not None:
                report_mutator(report, child_dir)
            write_child_report_artifacts(
                kwargs["output_dir"],
                provider,
                matrix,
                report,
                markdown=(
                    markdown_factory(provider, matrix, child_dir)
                    if markdown_factory is not None
                    else f"# {provider} {matrix}\n"
                ),
            )
            process_return_code = 1 if (provider, matrix) == failing_pair else 0
            record, errors, _decoded = common.load_child_report_artifacts(
                output_dir=kwargs["output_dir"],
                provider=provider,
                matrix=matrix,
                expected_git_sha=str(kwargs["expected_git_sha"]),
                policy=kwargs["policy"],
                process_return_code=process_return_code,
                duration_ms=123,
                process_output_scan=PROCESS_OUTPUT_SCAN,
            )
            return record, errors

        return run_child

    def seed_waiting_checkpoint(
        self,
        output_dir: pathlib.Path,
        *,
        report_mutator: Any | None = None,
        markdown_factory: Any | None = None,
    ) -> tuple[gate.DockerReleaseGateOptions, remote.RemoteReleaseTargetSpec]:
        options = controlled_options(output_dir)
        spec = controlled_spec()
        cleanup = mock.Mock(side_effect=self.cleanup_image)
        executed_pairs: list[tuple[str, str]] = []

        with (
            mock.patch.dict(os.environ, self.environment(), clear=False),
            mock.patch.object(
                remote,
                "inspect_gate_worker_image",
                side_effect=self.inspect_gate_worker_image,
            ),
            contextlib.redirect_stdout(io.StringIO()),
            contextlib.redirect_stderr(io.StringIO()),
        ):
            exit_code = remote.run_remote_release_gate(
                options,
                spec,
                build_image=self.build_image,
                cleanup_image=cleanup,
                repository_state=self.repository_state,
                run_child=self.child_run_factory(
                    options,
                    failing_pair=("claudeAgent", common.REMOTE_LOAD_MATRIX),
                    calls=executed_pairs,
                    report_mutator=report_mutator,
                    markdown_factory=markdown_factory,
                ),
            )

        self.assertEqual(exit_code, 1)
        self.assertEqual(
            executed_pairs,
            [
                ("codex", "product"),
                ("codex", "failure"),
                ("codex", common.REMOTE_LOAD_MATRIX),
                ("claudeAgent", "product"),
                ("claudeAgent", "failure"),
                ("claudeAgent", common.REMOTE_LOAD_MATRIX),
            ],
        )
        cleanup.assert_not_called()
        return options, spec

    def seed_completed_checkpoint(
        self,
        output_dir: pathlib.Path,
    ) -> tuple[gate.DockerReleaseGateOptions, remote.RemoteReleaseTargetSpec]:
        options, spec = self.seed_waiting_checkpoint(output_dir)
        cleanup = mock.Mock(side_effect=self.cleanup_image)
        with (
            mock.patch.dict(os.environ, self.environment(), clear=False),
            mock.patch.object(
                remote,
                "inspect_gate_worker_image",
                side_effect=self.inspect_gate_worker_image,
            ),
            contextlib.redirect_stdout(io.StringIO()),
            contextlib.redirect_stderr(io.StringIO()),
        ):
            exit_code = remote.run_remote_release_gate(
                options,
                spec,
                build_image=self.build_image,
                cleanup_image=cleanup,
                repository_state=self.repository_state,
                run_child=self.child_run_factory(options),
            )
        self.assertEqual(exit_code, 0)
        cleanup.assert_called_once()
        return options, spec

    def test_fresh_resumable_run_retains_first_five_passes_and_shared_image(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            output_dir = pathlib.Path(directory)
            options, spec = self.seed_waiting_checkpoint(output_dir)

            checkpoint = json.loads(
                (output_dir / remote.CHECKPOINT_FILE_NAME).read_text(encoding="utf-8")
            )
            report = json.loads(
                (output_dir / spec.json_report_name).read_text(encoding="utf-8")
            )
            self.assertEqual(checkpoint["status"], "waiting")
            self.assertEqual(len(checkpoint["completedRuns"]), 5)
            self.assertEqual(
                checkpoint["pendingRuns"],
                [{"provider": "claudeAgent", "matrix": common.REMOTE_LOAD_MATRIX}],
            )
            for pending in checkpoint["pendingRuns"]:
                self.assertIsInstance(pending["provider"], str)
                self.assertIsInstance(pending["matrix"], str)
            for run in checkpoint["completedRuns"]:
                attestation_path = (
                    output_dir
                    / run["provider"]
                    / run["matrix"]
                    / remote.CHILD_ATTESTATION_FILE_NAME
                )
                self.assertTrue(attestation_path.is_file())
            self.assertFalse(
                (
                    output_dir
                    / "claudeAgent"
                    / common.REMOTE_LOAD_MATRIX
                    / remote.CHILD_ATTESTATION_FILE_NAME
                ).exists()
            )
            self.assertEqual(report["status"], "fail")
            self.assertEqual(report["continuation"]["checkpointStatus"], "waiting")
            self.assertEqual(report["continuation"]["executedRuns"], 6)
            self.assertEqual(report["continuation"]["reusedRuns"], 0)
            self.assertFalse(report["continuation"]["resumed"])
            self.assertTrue(report["continuation"]["retainedWorkerImage"])
            self.assertFalse(report["workerImage"]["cleanup"]["removed"])
            self.assertTrue(report["workerImage"]["cleanup"]["deferredForResume"])
            self.assertEqual(
                {error["code"] for error in report["errors"]},
                {"release.child_process_failed"},
            )
            self.assertEqual(options.output_dir, output_dir)

    def test_resume_reuses_five_attested_runs_and_executes_only_pending_sixth_child(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            output_dir = pathlib.Path(directory)
            options, spec = self.seed_waiting_checkpoint(output_dir)
            cleanup = mock.Mock(side_effect=self.cleanup_image)
            executed_pairs: list[tuple[str, str]] = []

            with (
                mock.patch.dict(os.environ, self.environment(), clear=False),
                mock.patch.object(
                    remote,
                    "inspect_gate_worker_image",
                    side_effect=self.inspect_gate_worker_image,
                ),
                contextlib.redirect_stdout(io.StringIO()),
                contextlib.redirect_stderr(io.StringIO()),
            ):
                exit_code = remote.run_remote_release_gate(
                    options,
                    spec,
                    build_image=self.build_image,
                    cleanup_image=cleanup,
                    repository_state=self.repository_state,
                    run_child=self.child_run_factory(options, calls=executed_pairs),
                )

            checkpoint = json.loads(
                (output_dir / remote.CHECKPOINT_FILE_NAME).read_text(encoding="utf-8")
            )
            report = json.loads(
                (output_dir / spec.json_report_name).read_text(encoding="utf-8")
            )

        self.assertEqual(exit_code, 0)
        self.assertEqual(executed_pairs, [("claudeAgent", common.REMOTE_LOAD_MATRIX)])
        cleanup.assert_called_once()
        self.assertEqual(checkpoint["status"], "completed")
        self.assertEqual(len(checkpoint["completedRuns"]), 6)
        self.assertEqual(checkpoint["pendingRuns"], [])
        self.assertEqual(report["status"], "pass")
        self.assertEqual(report["continuation"]["checkpointStatus"], "completed")
        self.assertEqual(report["continuation"]["reusedRuns"], 5)
        self.assertEqual(report["continuation"]["executedRuns"], 1)
        self.assertTrue(report["continuation"]["resumed"])
        self.assertFalse(report["continuation"]["retainedWorkerImage"])
        self.assertTrue(report["workerImage"]["cleanup"]["removed"])

    def test_resume_rejects_controlled_profile_drift_before_any_child_scheduling(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            output_dir = pathlib.Path(directory)
            options, spec = self.seed_waiting_checkpoint(output_dir)
            run_child = mock.Mock(side_effect=self.child_run_factory(options))

            with (
                mock.patch.dict(
                    os.environ,
                    self.environment(CODEX_KEY="codex-secret-value-drifted"),
                    clear=False,
                ),
                mock.patch.object(
                    remote,
                    "inspect_gate_worker_image",
                    side_effect=self.inspect_gate_worker_image,
                ),
                contextlib.redirect_stdout(io.StringIO()),
                contextlib.redirect_stderr(io.StringIO()),
            ):
                exit_code = remote.run_remote_release_gate(
                    options,
                    spec,
                    build_image=self.build_image,
                    cleanup_image=self.cleanup_image,
                    repository_state=self.repository_state,
                    run_child=run_child,
                )

        self.assertEqual(exit_code, 2)
        run_child.assert_not_called()

    def test_resume_rejects_controlled_profile_credential_env_name_drift_even_when_value_matches(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            output_dir = pathlib.Path(directory)
            options, spec = self.seed_waiting_checkpoint(output_dir)
            drifted_options = dataclasses.replace(
                options,
                codex_credential=gate.CredentialSource("CODEX_KEY_ALT", "apiKey", None),
            )
            run_child = mock.Mock(side_effect=self.child_run_factory(drifted_options))

            with (
                mock.patch.dict(
                    os.environ,
                    self.environment(CODEX_KEY_ALT="codex-secret-value"),
                    clear=False,
                ),
                mock.patch.object(
                    remote,
                    "inspect_gate_worker_image",
                    side_effect=self.inspect_gate_worker_image,
                ),
                contextlib.redirect_stdout(io.StringIO()),
                contextlib.redirect_stderr(io.StringIO()),
            ):
                exit_code = remote.run_remote_release_gate(
                    drifted_options,
                    spec,
                    build_image=self.build_image,
                    cleanup_image=self.cleanup_image,
                    repository_state=self.repository_state,
                    run_child=run_child,
                )

        self.assertEqual(exit_code, 2)
        run_child.assert_not_called()

    def test_resume_rejects_attested_child_hash_tamper_before_any_child_scheduling(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            output_dir = pathlib.Path(directory)
            options, spec = self.seed_waiting_checkpoint(output_dir)
            tampered_report_path = output_dir / "codex" / "product" / "acceptance-report.json"
            tampered = json.loads(tampered_report_path.read_text(encoding="utf-8"))
            tampered["runId"] = "tampered-run-id"
            tampered_report_path.write_text(
                json.dumps(tampered, indent=2, sort_keys=True) + "\n",
                encoding="utf-8",
            )
            run_child = mock.Mock(side_effect=self.child_run_factory(options))

            with (
                mock.patch.dict(os.environ, self.environment(), clear=False),
                mock.patch.object(
                    remote,
                    "inspect_gate_worker_image",
                    side_effect=self.inspect_gate_worker_image,
                ),
                contextlib.redirect_stdout(io.StringIO()),
                contextlib.redirect_stderr(io.StringIO()),
            ):
                exit_code = remote.run_remote_release_gate(
                    options,
                    spec,
                    build_image=self.build_image,
                    cleanup_image=self.cleanup_image,
                    repository_state=self.repository_state,
                    run_child=run_child,
                )

        self.assertEqual(exit_code, 2)
        run_child.assert_not_called()

    def test_resume_rejects_retained_image_identity_or_owner_label_mismatch_before_any_child_scheduling(self) -> None:
        scenarios = (
            (
                "image_id_mismatch",
                lambda image: (
                    "sha256:" + "d" * 64,
                    remote.required_gate_worker_image_labels(image, GIT_SHA),
                ),
            ),
            (
                "owner_label_mismatch",
                lambda image: (
                    WORKER_IMAGE_ID,
                    {
                        **remote.required_gate_worker_image_labels(image, GIT_SHA),
                        remote.IMAGE_OWNER_LABEL: "f" * 20,
                    },
                ),
            ),
        )
        for name, inspect_result in scenarios:
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                output_dir = pathlib.Path(directory)
                options, spec = self.seed_waiting_checkpoint(output_dir)
                run_child = mock.Mock(side_effect=self.child_run_factory(options))

                with (
                    mock.patch.dict(os.environ, self.environment(), clear=False),
                    mock.patch.object(
                        remote,
                        "inspect_gate_worker_image",
                        side_effect=lambda _options, image: inspect_result(image),
                    ),
                    contextlib.redirect_stdout(io.StringIO()),
                    contextlib.redirect_stderr(io.StringIO()),
                ):
                    exit_code = remote.run_remote_release_gate(
                        options,
                        spec,
                        build_image=self.build_image,
                        cleanup_image=self.cleanup_image,
                        repository_state=self.repository_state,
                        run_child=run_child,
                    )

                self.assertEqual(exit_code, 2)
                run_child.assert_not_called()

    def test_resume_rejects_reused_child_runtime_residue_before_any_child_scheduling(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            output_dir = pathlib.Path(directory)
            options, spec = self.seed_waiting_checkpoint(output_dir)
            spec = dataclasses.replace(
                spec,
                validate_reused_child_runtime=lambda _options, _report: [
                    {
                        "code": "release.reused_child_resource_residue",
                        "message": "A checkpointed pass child still owns Docker runtime resources.",
                        "evidence": {"resource": "container", "resourceCount": 1},
                    }
                ],
            )
            run_child = mock.Mock(side_effect=self.child_run_factory(options))

            with (
                mock.patch.dict(os.environ, self.environment(), clear=False),
                mock.patch.object(
                    remote,
                    "inspect_gate_worker_image",
                    side_effect=self.inspect_gate_worker_image,
                ),
                contextlib.redirect_stdout(io.StringIO()),
                contextlib.redirect_stderr(io.StringIO()),
            ):
                exit_code = remote.run_remote_release_gate(
                    options,
                    spec,
                    build_image=self.build_image,
                    cleanup_image=self.cleanup_image,
                    repository_state=self.repository_state,
                    run_child=run_child,
                )

        self.assertEqual(exit_code, 2)
        run_child.assert_not_called()

    def test_cleanup_failure_on_final_resume_keeps_gate_failed_and_checkpoint_not_completed(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            output_dir = pathlib.Path(directory)
            options, spec = self.seed_waiting_checkpoint(output_dir)

            def failing_cleanup(
                _options: gate.DockerReleaseGateOptions,
                image: remote.GateWorkerImage,
                *,
                expected_image_id: str | None,
            ) -> tuple[dict[str, Any], common.ReleaseGateError]:
                return (
                    {
                        "name": image.name,
                        "expectedImageId": expected_image_id,
                        "presentBeforeCleanup": True,
                        "ownershipVerified": True,
                        "removed": False,
                        "broadCleanupUsed": False,
                    },
                    common.ReleaseGateError(
                        "release.worker_image_cleanup_failed",
                        "The gate-owned Worker image cleanup could not run to completion.",
                    ),
                )

            with (
                mock.patch.dict(os.environ, self.environment(), clear=False),
                mock.patch.object(
                    remote,
                    "inspect_gate_worker_image",
                    side_effect=self.inspect_gate_worker_image,
                ),
                contextlib.redirect_stdout(io.StringIO()),
                contextlib.redirect_stderr(io.StringIO()),
            ):
                exit_code = remote.run_remote_release_gate(
                    options,
                    spec,
                    build_image=self.build_image,
                    cleanup_image=failing_cleanup,
                    repository_state=self.repository_state,
                    run_child=self.child_run_factory(options),
                )

            checkpoint = json.loads(
                (output_dir / remote.CHECKPOINT_FILE_NAME).read_text(encoding="utf-8")
            )
            report = json.loads(
                (output_dir / spec.json_report_name).read_text(encoding="utf-8")
            )

        self.assertEqual(exit_code, 1)
        self.assertEqual(checkpoint["status"], "invalid")
        self.assertNotEqual(checkpoint["status"], "completed")
        self.assertEqual(len(checkpoint["completedRuns"]), 6)
        self.assertEqual(report["status"], "fail")
        self.assertEqual(report["continuation"]["checkpointStatus"], "invalid")
        self.assertFalse(report["workerImage"]["cleanup"]["removed"])
        self.assertIn(
            "release.worker_image_cleanup_failed",
            {error["code"] for error in report["errors"]},
        )

    def test_completed_checkpoint_noop_resume_rejects_aggregate_json_tamper(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            output_dir = pathlib.Path(directory)
            options, spec = self.seed_completed_checkpoint(output_dir)
            aggregate_path = output_dir / spec.json_report_name
            aggregate = json.loads(aggregate_path.read_text(encoding="utf-8"))
            aggregate["tampered"] = True
            aggregate_path.write_text(
                json.dumps(aggregate, indent=2, sort_keys=True) + "\n",
                encoding="utf-8",
            )
            run_child = mock.Mock(side_effect=self.child_run_factory(options))

            with (
                mock.patch.dict(os.environ, self.environment(), clear=False),
                mock.patch.object(
                    remote,
                    "inspect_gate_worker_image",
                    side_effect=self.inspect_gate_worker_image,
                ),
                contextlib.redirect_stdout(io.StringIO()),
                contextlib.redirect_stderr(io.StringIO()),
            ):
                exit_code = remote.run_remote_release_gate(
                    options,
                    spec,
                    build_image=self.build_image,
                    cleanup_image=self.cleanup_image,
                    repository_state=self.repository_state,
                    run_child=run_child,
                )

        self.assertEqual(exit_code, 2)
        run_child.assert_not_called()

    def test_completed_checkpoint_noop_resume_rejects_markdown_tamper(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            output_dir = pathlib.Path(directory)
            options, spec = self.seed_completed_checkpoint(output_dir)
            markdown_path = output_dir / spec.markdown_report_name
            markdown_path.write_text(
                markdown_path.read_text(encoding="utf-8") + "\nTAMPERED\n",
                encoding="utf-8",
            )
            run_child = mock.Mock(side_effect=self.child_run_factory(options))

            with (
                mock.patch.dict(os.environ, self.environment(), clear=False),
                mock.patch.object(
                    remote,
                    "inspect_gate_worker_image",
                    side_effect=self.inspect_gate_worker_image,
                ),
                contextlib.redirect_stdout(io.StringIO()),
                contextlib.redirect_stderr(io.StringIO()),
            ):
                exit_code = remote.run_remote_release_gate(
                    options,
                    spec,
                    build_image=self.build_image,
                    cleanup_image=self.cleanup_image,
                    repository_state=self.repository_state,
                    run_child=run_child,
                )

        self.assertEqual(exit_code, 2)
        run_child.assert_not_called()

    def test_resume_reuses_attested_crash_window_child_not_yet_recorded_in_checkpoint(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            output_dir = pathlib.Path(directory)
            options, spec = self.seed_waiting_checkpoint(output_dir)
            checkpoint_path = output_dir / remote.CHECKPOINT_FILE_NAME
            checkpoint = json.loads(checkpoint_path.read_text(encoding="utf-8"))
            provider = "claudeAgent"
            matrix = common.REMOTE_LOAD_MATRIX
            image_name = str(checkpoint["workerImage"]["name"])
            report = sample_child_report(
                options,
                provider,
                matrix,
                git_sha=GIT_SHA,
                worker_image_name=image_name,
            )
            write_child_report_artifacts(
                output_dir,
                provider,
                matrix,
                report,
                markdown=f"# {provider} {matrix}\n",
            )
            policy = remote.child_policy(options, spec, image_name)
            record, errors, _decoded = common.load_child_report_artifacts(
                output_dir=output_dir,
                provider=provider,
                matrix=matrix,
                expected_git_sha=GIT_SHA,
                policy=policy,
                process_return_code=0,
                duration_ms=123,
                process_output_scan=PROCESS_OUTPUT_SCAN,
            )
            self.assertEqual(errors, [])
            canonical_record = checkpoint_module._canonical_record_paths(record, provider, matrix)
            checkpoint_module._write_child_attestation(options, checkpoint, canonical_record)
            run_child = mock.Mock(side_effect=self.child_run_factory(options))

            with (
                mock.patch.dict(os.environ, self.environment(), clear=False),
                mock.patch.object(
                    remote,
                    "inspect_gate_worker_image",
                    side_effect=self.inspect_gate_worker_image,
                ),
                contextlib.redirect_stdout(io.StringIO()),
                contextlib.redirect_stderr(io.StringIO()),
            ):
                exit_code = remote.run_remote_release_gate(
                    options,
                    spec,
                    build_image=self.build_image,
                    cleanup_image=self.cleanup_image,
                    repository_state=self.repository_state,
                    run_child=run_child,
                )

            resumed_checkpoint = json.loads(checkpoint_path.read_text(encoding="utf-8"))
            report = json.loads(
                (output_dir / spec.json_report_name).read_text(encoding="utf-8")
            )

        self.assertEqual(exit_code, 0)
        run_child.assert_not_called()
        self.assertEqual(resumed_checkpoint["status"], "completed")
        self.assertEqual(len(resumed_checkpoint["completedRuns"]), 6)
        self.assertEqual(report["status"], "pass")
        self.assertEqual(report["continuation"]["reusedRuns"], 6)
        self.assertEqual(report["continuation"]["executedRuns"], 0)

    def test_canonical_promotion_rewrite_keeps_hashes_and_paths_valid(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            output_dir = pathlib.Path(directory)

            def mutate_report(report: dict[str, Any], child_dir: pathlib.Path) -> None:
                report["stagedChildPathEcho"] = str(child_dir)

            def markdown_factory(_provider: str, _matrix: str, child_dir: pathlib.Path) -> str:
                return f"staged-child-path: {child_dir}\n"

            options, spec = self.seed_waiting_checkpoint(
                output_dir,
                report_mutator=mutate_report,
                markdown_factory=markdown_factory,
            )
            checkpoint = json.loads(
                (output_dir / remote.CHECKPOINT_FILE_NAME).read_text(encoding="utf-8")
            )
            codex_product = next(
                run
                for run in checkpoint["completedRuns"]
                if run["provider"] == "codex" and run["matrix"] == "product"
            )
            canonical_child_dir = output_dir / "codex" / "product"
            staged_child_dir = (
                output_dir
                / remote.STAGING_DIRECTORY_NAME
                / "attempt-01"
                / "codex"
                / "product"
            )
            json_path = canonical_child_dir / "acceptance-report.json"
            markdown_path = canonical_child_dir / "acceptance-report.md"
            decoded = json.loads(json_path.read_text(encoding="utf-8"))
            markdown = markdown_path.read_text(encoding="utf-8")
            self.assertEqual(options.output_dir, output_dir)
            self.assertEqual(codex_product["reportPath"], "codex/product/acceptance-report.json")
            self.assertEqual(codex_product["markdownPath"], "codex/product/acceptance-report.md")
            self.assertEqual(codex_product["reportSha256"], common.file_sha256(json_path))
            self.assertEqual(codex_product["markdownSha256"], common.file_sha256(markdown_path))
            self.assertEqual(decoded["stagedChildPathEcho"], str(canonical_child_dir))
            self.assertIn(str(canonical_child_dir), markdown)
            self.assertNotIn(str(staged_child_dir), json.dumps(decoded, sort_keys=True))
            self.assertNotIn(str(staged_child_dir), markdown)

    def test_resume_rejects_provider_matrix_directory_symlink_escape_before_any_child_scheduling(self) -> None:
        if not hasattr(os, "symlink"):
            self.skipTest("symlink is unavailable on this platform")

        with tempfile.TemporaryDirectory() as directory, tempfile.TemporaryDirectory() as external_directory:
            output_dir = pathlib.Path(directory)
            external_root = pathlib.Path(external_directory)
            options, spec = self.seed_waiting_checkpoint(output_dir)
            escaped_pair_dir = output_dir / "codex" / "product"
            external_copy = external_root / "copied-codex-product"
            shutil.copytree(escaped_pair_dir, external_copy)
            shutil.rmtree(escaped_pair_dir)
            os.symlink(external_copy, escaped_pair_dir, target_is_directory=True)
            run_child = mock.Mock(side_effect=self.child_run_factory(options))

            with (
                mock.patch.dict(os.environ, self.environment(), clear=False),
                mock.patch.object(
                    remote,
                    "inspect_gate_worker_image",
                    side_effect=self.inspect_gate_worker_image,
                ),
                contextlib.redirect_stdout(io.StringIO()),
                contextlib.redirect_stderr(io.StringIO()),
            ):
                exit_code = remote.run_remote_release_gate(
                    options,
                    spec,
                    build_image=self.build_image,
                    cleanup_image=self.cleanup_image,
                    repository_state=self.repository_state,
                    run_child=run_child,
                )

        self.assertEqual(exit_code, 2)
        run_child.assert_not_called()

    def test_resume_recovers_from_finalizing_checkpoint_without_rerunning_children(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            output_dir = pathlib.Path(directory)
            options, spec = self.seed_completed_checkpoint(output_dir)
            checkpoint_path = output_dir / remote.CHECKPOINT_FILE_NAME
            checkpoint = json.loads(checkpoint_path.read_text(encoding="utf-8"))
            checkpoint["status"] = "finalizing"
            checkpoint_path.write_text(
                json.dumps(checkpoint, indent=2, sort_keys=True) + "\n",
                encoding="utf-8",
            )
            aggregate_path = output_dir / spec.json_report_name
            markdown_path = output_dir / spec.markdown_report_name
            aggregate_path.unlink()
            markdown_path.unlink()
            run_child = mock.Mock(side_effect=self.child_run_factory(options))

            def image_absent(
                _options: Any,
                _image: remote.GateWorkerImage,
            ) -> tuple[str, dict[str, str]]:
                raise remote.ReleaseGateError(
                    "release.checkpoint_worker_image_missing",
                    "The retained gate-owned Worker image no longer exists.",
                )

            with (
                mock.patch.dict(os.environ, self.environment(), clear=False),
                mock.patch.object(
                    remote,
                    "inspect_gate_worker_image",
                    side_effect=image_absent,
                ),
                contextlib.redirect_stdout(io.StringIO()),
                contextlib.redirect_stderr(io.StringIO()),
            ):
                exit_code = remote.run_remote_release_gate(
                    options,
                    spec,
                    build_image=self.build_image,
                    cleanup_image=self.cleanup_image,
                    repository_state=self.repository_state,
                    run_child=run_child,
                )

            recovered_checkpoint = json.loads(checkpoint_path.read_text(encoding="utf-8"))
            self.assertEqual(exit_code, 0)
            run_child.assert_not_called()
            self.assertTrue(aggregate_path.is_file())
            self.assertTrue(markdown_path.is_file())
            self.assertEqual(recovered_checkpoint["status"], "completed")
            self.assertEqual(recovered_checkpoint["pendingRuns"], [])

    def test_resume_finalizing_dangling_image_tag_cleans_exact_checkpoint_id(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            output_dir = pathlib.Path(directory)
            options, spec = self.seed_completed_checkpoint(output_dir)
            checkpoint_path = output_dir / remote.CHECKPOINT_FILE_NAME
            checkpoint = json.loads(checkpoint_path.read_text(encoding="utf-8"))
            checkpoint["status"] = "finalizing"
            checkpoint.pop("aggregateArtifacts")
            checkpoint.pop("workerImageCleanup")
            checkpoint_path.write_text(
                json.dumps(checkpoint, indent=2, sort_keys=True) + "\n",
                encoding="utf-8",
            )
            aggregate_path = output_dir / spec.json_report_name
            markdown_path = output_dir / spec.markdown_report_name
            aggregate_path.unlink()
            markdown_path.unlink()

            worker = checkpoint["workerImage"]
            image_name = str(worker["name"])
            expected_image_id = str(worker["id"])
            checkpoint_image = remote.GateWorkerImage(
                name=image_name,
                owner=str(worker["owner"]),
                target=str(worker["target"]),
            )
            inspect_references: list[str] = []

            def inspect_by_reference(
                _options: Any,
                image: remote.GateWorkerImage,
            ) -> tuple[str, dict[str, str]]:
                inspect_references.append(image.name)
                if image.name == image_name:
                    raise remote.ReleaseGateError(
                        "release.worker_image_not_found",
                        "The gate-owned Worker image tag no longer exists.",
                    )
                self.assertEqual(image.name, expected_image_id)
                return (
                    expected_image_id,
                    remote.required_gate_worker_image_labels(checkpoint_image, GIT_SHA),
                )

            docker_calls: list[list[str]] = []

            def docker_completed(
                _options: Any,
                arguments: list[str],
                *,
                timeout: float,
            ) -> mock.Mock:
                del timeout
                command = list(arguments)
                docker_calls.append(command)
                if command == ["image", "rm", "-f", expected_image_id]:
                    return mock.Mock(returncode=0, stdout="", stderr="")
                if command == ["image", "inspect", expected_image_id]:
                    return mock.Mock(
                        returncode=1,
                        stdout="",
                        stderr=f"No such image: {expected_image_id}",
                    )
                self.fail(f"unexpected Docker command: {command}")

            cleanup = mock.Mock(side_effect=remote.cleanup_gate_worker_image)
            run_child = mock.Mock(side_effect=self.child_run_factory(options))
            with (
                mock.patch.dict(os.environ, self.environment(), clear=False),
                mock.patch.object(
                    remote,
                    "inspect_gate_worker_image",
                    side_effect=inspect_by_reference,
                ),
                mock.patch.object(
                    remote,
                    "docker_completed",
                    side_effect=docker_completed,
                ),
                contextlib.redirect_stdout(io.StringIO()),
                contextlib.redirect_stderr(io.StringIO()),
            ):
                exit_code = remote.run_remote_release_gate(
                    options,
                    spec,
                    build_image=self.build_image,
                    cleanup_image=cleanup,
                    repository_state=self.repository_state,
                    run_child=run_child,
                )

            recovered_checkpoint = json.loads(checkpoint_path.read_text(encoding="utf-8"))
            report = json.loads(aggregate_path.read_text(encoding="utf-8"))
            cleanup.assert_called_once()
            self.assertIs(cleanup.call_args.args[0], options)
            self.assertEqual(cleanup.call_args.args[1], checkpoint_image)
            self.assertEqual(
                cleanup.call_args.kwargs,
                {"expected_image_id": expected_image_id},
            )
            self.assertEqual(
                inspect_references,
                [image_name, expected_image_id, image_name, expected_image_id],
            )
            self.assertEqual(
                docker_calls,
                [
                    ["image", "rm", "-f", expected_image_id],
                    ["image", "inspect", expected_image_id],
                ],
            )
            self.assertEqual(exit_code, 0)
            run_child.assert_not_called()
            self.assertTrue(markdown_path.is_file())
            self.assertEqual(recovered_checkpoint["status"], "completed")
            cleanup_evidence = report["workerImage"]["cleanup"]
            self.assertEqual(cleanup_evidence["expectedImageId"], expected_image_id)
            self.assertTrue(cleanup_evidence["resolvedByImageId"])
            self.assertTrue(cleanup_evidence["presentBeforeCleanup"])
            self.assertTrue(cleanup_evidence["removed"])
            self.assertNotIn("absentBeforeRecoveredFinalization", cleanup_evidence)
            self.assertEqual(recovered_checkpoint["workerImageCleanup"], cleanup_evidence)

    def test_interrupted_fresh_resume_build_writes_planned_building_checkpoint_without_raw_credentials(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            output_dir = pathlib.Path(directory)
            options = controlled_options(output_dir)
            spec = controlled_spec()
            interrupted_image_name: list[str] = []

            def interrupted_build_image(
                build_options: gate.DockerReleaseGateOptions,
                image: remote.GateWorkerImage,
                git_sha: str,
            ) -> dict[str, Any]:
                interrupted_image_name.append(image.name)
                metadata_path = build_options.output_dir / "worker-image-build-metadata.json"
                metadata_path.write_text(
                    json.dumps({"gitSha": git_sha, "image": image.name}, sort_keys=True) + "\n",
                    encoding="utf-8",
                )
                raise SimulatedInterrupt("simulated process interruption after image registration")

            with (
                mock.patch.dict(os.environ, self.environment(), clear=False),
                mock.patch.object(
                    remote,
                    "inspect_gate_worker_image",
                    side_effect=self.inspect_gate_worker_image,
                ),
                contextlib.redirect_stdout(io.StringIO()),
                contextlib.redirect_stderr(io.StringIO()),
            ):
                with self.assertRaises(SimulatedInterrupt):
                    remote.run_remote_release_gate(
                        options,
                        spec,
                        build_image=interrupted_build_image,
                        cleanup_image=self.cleanup_image,
                        repository_state=self.repository_state,
                        run_child=self.child_run_factory(options),
                    )

            checkpoint_path = output_dir / remote.CHECKPOINT_FILE_NAME
            self.assertTrue(interrupted_image_name)
            self.assertTrue(
                checkpoint_path.is_file(),
                "expected a durable planned checkpoint before the shared image build",
            )
            checkpoint_text = checkpoint_path.read_text(encoding="utf-8")
            checkpoint = json.loads(checkpoint_text)
            self.assertEqual(checkpoint["status"], "building")
            self.assertEqual(checkpoint["workerImage"]["name"], interrupted_image_name[0])
            self.assertNotIn("codex-secret-value", checkpoint_text)
            self.assertNotIn("claude-secret-value", checkpoint_text)
            self.assertNotIn("https://claude.example.test", checkpoint_text)

    def test_interrupted_fresh_resume_build_second_attempt_reuses_identity_and_does_not_deadlock(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            output_dir = pathlib.Path(directory)
            options = controlled_options(output_dir)
            spec = controlled_spec()
            interrupted_image_name: list[str] = []

            def interrupted_build_image(
                build_options: gate.DockerReleaseGateOptions,
                image: remote.GateWorkerImage,
                git_sha: str,
            ) -> dict[str, Any]:
                interrupted_image_name.append(image.name)
                metadata_path = build_options.output_dir / "worker-image-build-metadata.json"
                metadata_path.write_text(
                    json.dumps({"gitSha": git_sha, "image": image.name}, sort_keys=True) + "\n",
                    encoding="utf-8",
                )
                raise SimulatedInterrupt("simulated process interruption after image registration")

            with (
                mock.patch.dict(os.environ, self.environment(), clear=False),
                mock.patch.object(
                    remote,
                    "inspect_gate_worker_image",
                    side_effect=self.inspect_gate_worker_image,
                ),
                contextlib.redirect_stdout(io.StringIO()),
                contextlib.redirect_stderr(io.StringIO()),
            ):
                with self.assertRaises(SimulatedInterrupt):
                    remote.run_remote_release_gate(
                        options,
                        spec,
                        build_image=interrupted_build_image,
                        cleanup_image=self.cleanup_image,
                        repository_state=self.repository_state,
                        run_child=self.child_run_factory(options),
                    )

            resumed_build_calls: list[str] = []
            cleanup = mock.Mock(side_effect=self.cleanup_image)

            def resumed_build_image(
                build_options: gate.DockerReleaseGateOptions,
                image: remote.GateWorkerImage,
                git_sha: str,
            ) -> dict[str, Any]:
                resumed_build_calls.append(image.name)
                return self.build_image(build_options, image, git_sha)

            with (
                mock.patch.dict(os.environ, self.environment(), clear=False),
                mock.patch.object(
                    remote,
                    "inspect_gate_worker_image",
                    side_effect=self.inspect_gate_worker_image,
                ),
                contextlib.redirect_stdout(io.StringIO()),
                contextlib.redirect_stderr(io.StringIO()),
            ):
                exit_code = remote.run_remote_release_gate(
                    options,
                    spec,
                    build_image=resumed_build_image,
                    cleanup_image=cleanup,
                    repository_state=self.repository_state,
                    run_child=self.child_run_factory(options),
                )

            checkpoint_path = output_dir / remote.CHECKPOINT_FILE_NAME
            checkpoint = json.loads(checkpoint_path.read_text(encoding="utf-8"))
            self.assertEqual(exit_code, 0)
            self.assertTrue(interrupted_image_name)
            self.assertEqual(resumed_build_calls, [interrupted_image_name[0]])
            self.assertGreaterEqual(cleanup.call_count, 1)
            self.assertTrue(
                all(call.args[1].name == interrupted_image_name[0] for call in cleanup.call_args_list)
            )
            self.assertEqual(checkpoint["status"], "completed")

    def test_rejects_output_lock_conflict_before_running_the_gate(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            output_dir = pathlib.Path(directory)
            output_dir.mkdir(parents=True, exist_ok=True)
            options = controlled_options(output_dir)
            spec = controlled_spec()
            build_image = mock.Mock(side_effect=self.build_image)
            run_child = mock.Mock(side_effect=self.child_run_factory(options))

            with (
                mock.patch.dict(os.environ, self.environment(), clear=False),
                mock.patch.object(
                    remote,
                    "inspect_gate_worker_image",
                    side_effect=self.inspect_gate_worker_image,
                ),
                remote.release_output_lock(output_dir),
                contextlib.redirect_stdout(io.StringIO()),
                contextlib.redirect_stderr(io.StringIO()),
            ):
                exit_code = remote.run_remote_release_gate(
                    options,
                    spec,
                    build_image=build_image,
                    cleanup_image=self.cleanup_image,
                    repository_state=self.repository_state,
                    run_child=run_child,
                )

        self.assertEqual(exit_code, 2)
        build_image.assert_not_called()
        run_child.assert_not_called()


if __name__ == "__main__":
    unittest.main()
