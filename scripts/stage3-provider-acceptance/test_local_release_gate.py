from __future__ import annotations

import json
import pathlib
import subprocess
import tempfile
import unittest
from typing import Any
from unittest import mock

import acceptance_runner as acceptance
import local_release_gate as gate


def sample_child_report(
    provider: str,
    matrix: str,
    *,
    git_sha: str = "a" * 40,
    catalog_hash: str = "b" * 64,
) -> dict[str, Any]:
    requested_product = list(acceptance.REAL_PROVIDER_CASES) if matrix == "product" else []
    requested_failure = list(acceptance.REAL_PROVIDER_FAILURE_CASES) if matrix == "failure" else []
    case_statuses: dict[str, str] = {
        "environment.target-prepare": "pass",
        "environment.control-plane-start": "pass",
        "identity.dev-login": "pass",
        "runtime.target-provision": "pass",
        "resources.real-provider-project-session": "pass",
        "real-provider.turn-1-start": "pass",
        "runtime.real-provider-worker-discovery": "pass",
        "real-provider.turn-1": "pass",
        "recovery.control-plane-restart": "pass",
        "real-provider.turn-2-continuity": "pass",
    }
    for case_id in gate.expected_case_ids(matrix):
        case_statuses[case_id] = "pass"
    for case_id in gate.EXPECTED_UNSUPPORTED[(provider, matrix)]:
        case_statuses[case_id] = "unsupported"
    cases = [
        {"id": case_id, "status": status}
        for case_id, status in sorted(case_statuses.items())
    ]
    cases.extend(
        [
            {
                "id": "environment.cleanup",
                "status": "pass",
                "evidence": {"controlPlaneStopped": True, "stateRemoved": True},
            },
            {
                "id": "security.output-secret-scan",
                "status": "pass",
                "evidence": {"findings": []},
            },
        ]
    )
    return {
        "schemaVersion": acceptance.SCHEMA_VERSION,
        "runId": f"run-{provider}-{matrix}",
        "mode": "real-provider-smoke",
        "target": "local",
        "provider": provider,
        "status": "pass",
        "durationMs": 123,
        "source": {
            "gitSha": git_sha,
            "worktreeDirty": False,
            "providerCapabilityCatalogSha256": catalog_hash,
        },
        "configuration": {
            "restartControlPlane": True,
            "keepState": False,
            "runnerCommand": {"executable": "node"},
            "realProvider": {
                "requestedCases": requested_product,
                "requestedFailureCases": requested_failure,
                "ambientAuthentication": True,
                "controlledFaultCredentials": matrix == "failure",
                "cursorMaximumAge": (
                    acceptance.REAL_PROVIDER_CURSOR_MAX_AGE if matrix == "failure" else None
                ),
            },
        },
        "cases": cases,
    }


class RunnerCommandTest(unittest.TestCase):
    def test_requires_the_repository_provider_host_and_direct_node(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            repo_root = pathlib.Path(directory)
            host_path = repo_root / "apps" / "provider-host" / "dist" / "index.mjs"
            host_path.parent.mkdir(parents=True)
            host_path.write_text("", encoding="utf-8")
            node_path = repo_root / "node"
            node_path.write_text("", encoding="utf-8")

            command = gate.parse_runner_command(
                json.dumps([str(node_path), str(host_path)]),
                repo_root,
            )

            self.assertEqual(command, (str(node_path.resolve()), str(host_path.resolve())))

    def test_rejects_a_provider_host_outside_the_repository(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            repo_root = pathlib.Path(directory)
            node_path = repo_root / "node"
            node_path.write_text("", encoding="utf-8")
            outside = repo_root / "outside.mjs"
            outside.write_text("", encoding="utf-8")

            with self.assertRaisesRegex(gate.ReleaseGateError, "current repository"):
                gate.parse_runner_command(json.dumps([str(node_path), str(outside)]), repo_root)


class NodeVersionTest(unittest.TestCase):
    def test_parses_supported_node_version(self) -> None:
        self.assertEqual(gate.parse_node_version("v24.13.1\n"), (24, 13, 1))

    def test_rejects_non_semantic_node_version(self) -> None:
        with self.assertRaisesRegex(gate.ReleaseGateError, "three-component"):
            gate.parse_node_version("nightly")


class RepositoryStateTest(unittest.TestCase):
    def test_rejects_untracked_files(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            repo_root = pathlib.Path(directory)
            subprocess.run(["git", "init", "-q"], cwd=repo_root, check=True)
            subprocess.run(
                ["git", "config", "user.email", "stage3-release-gate@example.invalid"],
                cwd=repo_root,
                check=True,
            )
            subprocess.run(
                ["git", "config", "user.name", "Stage 3 Release Gate"],
                cwd=repo_root,
                check=True,
            )
            tracked = repo_root / "tracked.txt"
            tracked.write_text("tracked\n", encoding="utf-8")
            subprocess.run(["git", "add", "tracked.txt"], cwd=repo_root, check=True)
            subprocess.run(["git", "commit", "-qm", "initial"], cwd=repo_root, check=True)
            self.assertFalse(gate.repository_state(repo_root)["worktreeDirty"])

            (repo_root / "untracked.txt").write_text("untracked\n", encoding="utf-8")

            with self.assertRaisesRegex(gate.ReleaseGateError, "no untracked files"):
                gate.repository_state(repo_root)


class ChildCommandTest(unittest.TestCase):
    def test_keeps_product_and_failure_boundaries_separate(self) -> None:
        repo_root = pathlib.Path("/tmp/synara").resolve()
        options = gate.ReleaseGateOptions(
            repo_root=repo_root,
            output_dir=repo_root / ".tmp" / "release",
            runner_command=("/tmp/node", str(repo_root / "apps/provider-host/dist/index.mjs")),
            product_timeout_seconds=900.0,
            failure_timeout_seconds=420.0,
        )

        product = gate.child_command(options, "codex", "product", options.output_dir / "codex/product")
        failure = gate.child_command(options, "codex", "failure", options.output_dir / "codex/failure")

        self.assertIn("--real-provider-matrix", product)
        self.assertNotIn("--real-provider-failure-matrix", product)
        self.assertEqual(product.count("--provider"), 1)
        self.assertIn("900.0", product)
        self.assertIn("--real-provider-failure-matrix", failure)
        self.assertNotIn("--real-provider-matrix", failure)
        self.assertEqual(failure.count("--provider"), 1)
        self.assertIn("420.0", failure)


class ChildReportValidationTest(unittest.TestCase):
    def test_accepts_complete_product_and_failure_reports(self) -> None:
        for provider in gate.PROVIDERS:
            for matrix in gate.MATRICES:
                with self.subTest(provider=provider, matrix=matrix):
                    errors = gate.validate_child_report(
                        sample_child_report(provider, matrix),
                        provider=provider,
                        matrix=matrix,
                        expected_git_sha="a" * 40,
                    )
                    self.assertEqual(errors, [])

    def test_accepts_a_previously_unsupported_case_when_it_becomes_pass(self) -> None:
        report = sample_child_report("claudeAgent", "product")
        for case in report["cases"]:
            if case["id"] == "real-provider.compact-boundary":
                case["status"] = "pass"

        errors = gate.validate_child_report(
            report,
            provider="claudeAgent",
            matrix="product",
            expected_git_sha="a" * 40,
        )

        self.assertEqual(errors, [])

    def test_rejects_dirty_or_different_source(self) -> None:
        report = sample_child_report("codex", "failure", git_sha="c" * 40)
        report["source"]["worktreeDirty"] = True

        errors = gate.validate_child_report(
            report,
            provider="codex",
            matrix="failure",
            expected_git_sha="a" * 40,
        )

        self.assertIn("release.child_source_mismatch", {error["code"] for error in errors})

    def test_rejects_missing_required_case(self) -> None:
        report = sample_child_report("codex", "failure")
        report["cases"] = [
            case
            for case in report["cases"]
            if case["id"] != "real-provider.failure-cursor-expiry"
        ]

        errors = gate.validate_child_report(
            report,
            provider="codex",
            matrix="failure",
            expected_git_sha="a" * 40,
        )

        self.assertIn("release.child_cases_missing", {error["code"] for error in errors})

    def test_rejects_unexpected_unsupported_case(self) -> None:
        report = sample_child_report("codex", "product")
        for case in report["cases"]:
            if case["id"] == "real-provider.large-diff-artifact":
                case["status"] = "unsupported"

        errors = gate.validate_child_report(
            report,
            provider="codex",
            matrix="product",
            expected_git_sha="a" * 40,
        )

        self.assertIn("release.child_unsupported_unexpected", {error["code"] for error in errors})

    def test_rejects_cleanup_or_secret_scan_failure(self) -> None:
        report = sample_child_report("claudeAgent", "failure")
        for case in report["cases"]:
            if case["id"] == "environment.cleanup":
                case["evidence"]["stateRemoved"] = False
            if case["id"] == "security.output-secret-scan":
                case["evidence"]["findings"] = [{"kind": "known-secret-1"}]

        errors = gate.validate_child_report(
            report,
            provider="claudeAgent",
            matrix="failure",
            expected_git_sha="a" * 40,
        )

        codes = {error["code"] for error in errors}
        self.assertIn("release.child_cleanup_invalid", codes)
        self.assertIn("release.child_secret_scan_invalid", codes)


class CatalogConsensusTest(unittest.TestCase):
    def test_rejects_different_catalog_hashes(self) -> None:
        runs = [
            {"source": {"providerCapabilityCatalogSha256": "a" * 64}},
            {"source": {"providerCapabilityCatalogSha256": "b" * 64}},
        ]

        errors = gate.catalog_consensus_errors(runs)

        self.assertEqual(errors[0]["code"], "release.catalog_hash_mismatch")


class ChildRunTest(unittest.TestCase):
    def test_hashes_and_validates_child_reports_without_persisting_process_output(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            repo_root = pathlib.Path(directory)
            output_dir = repo_root / "results"
            child_dir = output_dir / "codex" / "failure"
            child_dir.mkdir(parents=True)
            report = sample_child_report("codex", "failure")
            json_path = child_dir / acceptance.JSON_REPORT_NAME
            markdown_path = child_dir / acceptance.MARKDOWN_REPORT_NAME
            json_path.write_text(json.dumps(report), encoding="utf-8")
            markdown_path.write_text("# child\n", encoding="utf-8")
            options = gate.ReleaseGateOptions(
                repo_root=repo_root,
                output_dir=output_dir,
                runner_command=("/tmp/node", str(repo_root / "apps/provider-host/dist/index.mjs")),
                product_timeout_seconds=900.0,
                failure_timeout_seconds=420.0,
            )

            with mock.patch.object(
                gate.subprocess,
                "run",
                return_value=subprocess.CompletedProcess(args=[], returncode=0),
            ):
                run, errors = gate.run_child(
                    options,
                    provider="codex",
                    matrix="failure",
                    expected_git_sha="a" * 40,
                )

            self.assertEqual(errors, [])
            self.assertEqual(run["status"], "pass")
            self.assertEqual(run["reportSha256"], gate.file_sha256(json_path))
            self.assertEqual(run["markdownSha256"], gate.file_sha256(markdown_path))
            self.assertNotIn("processOutput", run)


if __name__ == "__main__":
    unittest.main()
