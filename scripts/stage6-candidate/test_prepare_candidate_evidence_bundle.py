from __future__ import annotations

import base64
import hashlib
import json
import os
import pathlib
import stat
import subprocess
import sys
import unittest
from unittest import mock

import test_validate_candidate_evidence_bundle as candidate_fixture
import prepare_candidate_evidence_bundle as candidate_preparer


SCRIPT = pathlib.Path(__file__).with_name("prepare_candidate_evidence_bundle.py")
PROTECTED_RELEASE_VERIFIER = SCRIPT.parents[1] / "verify-stage6-candidate-release-binding.ts"
PROTECTED_ENVIRONMENT_PREPARER = (
    SCRIPT.parents[1] / "prepare-stage6-protected-environment-config.ts"
)
PROTECTED_ENVIRONMENT_APPLIER = (
    SCRIPT.parents[1] / "apply-stage6-protected-environment-config.ts"
)


class PrepareCandidateEvidenceBundleTest(unittest.TestCase):
    def setUp(self) -> None:
        self.fixture = candidate_fixture.ValidateCandidateEvidenceBundleTest(methodName="runTest")
        self.fixture.setUp()
        self.fixture.bundle_schema = "synara.stage6-candidate-evidence-bundle.v5"
        self.root = self.fixture.root
        self.evidence = self.fixture.evidence
        self.manifest_output = self.evidence / "candidate-bundle.json"
        self.receipt_output = self.evidence / "candidate-bundle-receipt.json"

    def tearDown(self) -> None:
        self.fixture.tearDown()

    def command(self, *, omit: str | None = None) -> list[str]:
        command = [
            sys.executable,
            str(SCRIPT),
            "--evidence-root",
            str(self.evidence),
            "--release-evidence",
            "release-evidence.json",
            "--compatibility-matrix",
            "compatibility-matrix.json",
        ]
        for name in sorted(candidate_preparer.RECEIPT_SPECS_V4):
            if name == omit:
                continue
            option = "--" + "".join(
                f"-{character.lower()}" if character.isupper() else character for character in name
            )
            command.extend([f"{option}-receipt", f"{name}-receipt.json"])
        command.extend(
            [
                "--manifest-output",
                self.manifest_output.name,
                "--receipt-output",
                self.receipt_output.name,
                "--validated-at",
                "2026-08-01T00:00:00Z",
            ]
        )
        return command

    def run_preparer(self, *, omit: str | None = None) -> subprocess.CompletedProcess[str]:
        self.fixture.write_bundle()
        return subprocess.run(self.command(omit=omit), check=False, capture_output=True, text=True)

    def assert_no_outputs(self) -> None:
        for path in (
            self.manifest_output,
            self.manifest_output.with_suffix(".json.sha256"),
            self.receipt_output,
            self.receipt_output.with_suffix(".json.sha256"),
        ):
            self.assertFalse(path.exists(), path)

    def test_prepares_validated_v5_manifest_receipt_and_sidecars(self) -> None:
        result = self.run_preparer()
        self.assertEqual(result.returncode, 0, result.stderr)

        manifest = json.loads(self.manifest_output.read_text(encoding="utf-8"))
        receipt = json.loads(self.receipt_output.read_text(encoding="utf-8"))
        self.assertEqual(manifest["schemaVersion"], "synara.stage6-candidate-evidence-bundle.v5")
        self.assertIn("compatibilityMatrix", manifest)
        self.assertEqual(set(manifest["receipts"]), set(candidate_preparer.RECEIPT_SPECS_V4))
        self.assertEqual(receipt["requiredReceiptCount"], 10)
        self.assertEqual(
            receipt["schemaVersion"],
            "synara.stage6-candidate-evidence-bundle-validation.v5",
        )
        self.assertEqual(receipt["manifest"]["path"], self.manifest_output.name)
        self.assertTrue(receipt["candidateConsistencyValidated"])
        self.assertTrue(receipt["eligibleForCandidateEvidenceReview"])
        self.assertEqual(receipt["assessment"], "evidence-consistent-not-ga-approved")

        for path in (self.manifest_output, self.receipt_output):
            sidecar = path.with_suffix(path.suffix + ".sha256")
            self.assertEqual(
                sidecar.read_text(encoding="utf-8"),
                f"{hashlib.sha256(path.read_bytes()).hexdigest()}  {path.name}\n",
            )

    def test_prepared_receipt_is_accepted_by_protected_release_verifier(self) -> None:
        result = self.run_preparer()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt_bytes = self.receipt_output.read_bytes()
        receipt = json.loads(receipt_bytes)
        candidate = receipt["candidate"]
        verifier = subprocess.run(
            [
                "node",
                str(PROTECTED_RELEASE_VERIFIER),
                "--receipt",
                str(self.receipt_output),
                "--expected-receipt-sha256",
                "sha256:" + hashlib.sha256(receipt_bytes).hexdigest(),
                "--expected-candidate-id",
                candidate["candidateId"],
                "--expected-source-commit",
                candidate["sourceCommit"],
                "--expected-lockfile-sha256",
                candidate["lockfileSha256"],
                "--expected-desktop-artifact-set-sha256",
                candidate["desktopArtifactSetSha256"],
            ],
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(verifier.returncode, 0, verifier.stderr)
        binding = json.loads(verifier.stdout)
        self.assertEqual(
            binding["schemaVersion"], "synara.stage6-candidate-release-binding.v2"
        )
        self.assertTrue(binding["verification"]["allReceiptProjectionsValidated"])

    def protected_environment_command(
        self, output: pathlib.Path, *, candidate_id: str = "stage6-rc1", run_id: str = "123456789"
    ) -> list[str]:
        return [
            "node",
            str(PROTECTED_ENVIRONMENT_PREPARER),
            "--receipt",
            str(self.receipt_output),
            "--candidate-id",
            candidate_id,
            "--source-commit",
            self.fixture.release["source"]["commit"],
            "--build-run-id",
            run_id,
            "--output",
            str(output),
        ]

    def test_prepares_private_non_logging_protected_environment_configuration(self) -> None:
        result = self.run_preparer()
        self.assertEqual(result.returncode, 0, result.stderr)
        output = self.evidence / "protected-environment.json"
        prepared = subprocess.run(
            self.protected_environment_command(output),
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        config = json.loads(output.read_text(encoding="utf-8"))
        receipt_bytes = self.receipt_output.read_bytes()
        receipt_sha256 = "sha256:" + hashlib.sha256(receipt_bytes).hexdigest()
        self.assertEqual(
            config["schemaVersion"],
            "synara.stage6-protected-environment-configuration.v1",
        )
        self.assertEqual(config["variables"]["SYNARA_STAGE6_CANDIDATE_ID"], "stage6-rc1")
        self.assertEqual(
            config["variables"]["SYNARA_STAGE6_CANDIDATE_BUNDLE_RECEIPT_SHA256"],
            receipt_sha256,
        )
        self.assertEqual(
            base64.b64decode(
                config["secrets"]["SYNARA_STAGE6_CANDIDATE_BUNDLE_RECEIPT_BASE64"],
                validate=True,
            ),
            receipt_bytes,
        )
        self.assertEqual(
            config["requiredReviewComment"], "SYNARA_STAGE6_APPROVE stage6-rc1 123456789"
        )
        self.assertNotIn(
            config["secrets"]["SYNARA_STAGE6_CANDIDATE_BUNDLE_RECEIPT_BASE64"],
            prepared.stdout,
        )
        sidecar = output.with_suffix(".json.sha256")
        self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(sidecar.stat().st_mode), 0o600)
        self.assertEqual(
            sidecar.read_text(encoding="utf-8"),
            f"{hashlib.sha256(output.read_bytes()).hexdigest()}  {output.name}\n",
        )

    def test_protected_environment_configuration_rejects_drift_and_overwrite(self) -> None:
        result = self.run_preparer()
        self.assertEqual(result.returncode, 0, result.stderr)
        for candidate_id, run_id in (("stage6-other", "123456789"), ("stage6-rc1", "0")):
            output = self.evidence / f"invalid-{candidate_id}-{run_id}.json"
            prepared = subprocess.run(
                self.protected_environment_command(
                    output, candidate_id=candidate_id, run_id=run_id
                ),
                check=False,
                capture_output=True,
                text=True,
            )
            self.assertNotEqual(prepared.returncode, 0)
            self.assertFalse(output.exists())
            self.assertFalse(output.with_suffix(".json.sha256").exists())

        output = self.evidence / "retained-environment.json"
        output.write_text("retained\n", encoding="utf-8")
        prepared = subprocess.run(
            self.protected_environment_command(output),
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertNotEqual(prepared.returncode, 0)
        self.assertEqual(output.read_text(encoding="utf-8"), "retained\n")
        self.assertFalse(output.with_suffix(".json.sha256").exists())

        sidecar_blocked = self.evidence / "sidecar-blocked.json"
        retained_sidecar = sidecar_blocked.with_suffix(".json.sha256")
        retained_sidecar.write_text("retained sidecar\n", encoding="utf-8")
        prepared = subprocess.run(
            self.protected_environment_command(sidecar_blocked),
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertNotEqual(prepared.returncode, 0)
        self.assertFalse(sidecar_blocked.exists())
        self.assertEqual(
            retained_sidecar.read_text(encoding="utf-8"), "retained sidecar\n"
        )

    def test_applies_real_prepared_environment_config_without_exposing_secret(self) -> None:
        result = self.run_preparer()
        self.assertEqual(result.returncode, 0, result.stderr)
        configuration = self.evidence / "protected-environment-apply-input.json"
        prepared = subprocess.run(
            self.protected_environment_command(configuration),
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(prepared.returncode, 0, prepared.stderr)
        config = json.loads(configuration.read_text(encoding="utf-8"))
        secret = config["secrets"]["SYNARA_STAGE6_CANDIDATE_BUNDLE_RECEIPT_BASE64"]

        fake_bin = self.evidence / "fake-bin"
        fake_bin.mkdir()
        fake_gh = fake_bin / "gh"
        fake_gh.write_text(
            """#!/usr/bin/env python3
import hashlib
import json
import os
import pathlib
import sys

state_path = pathlib.Path(os.environ["FAKE_GH_STATE"])
state = json.loads(state_path.read_text()) if state_path.exists() else {"variables": {}, "secretNames": [], "calls": []}
args = sys.argv[1:]
stdin = sys.stdin.read()
state["calls"].append({"args": args, "sensitiveValueInArgv": bool(stdin and stdin in args)})

def environment(reviewers=None):
    reviewer_values = reviewers if reviewers is not None else state.get("reviewers", [])
    return {
        "id": 987,
        "name": "stage6-enterprise-ga",
        "can_admins_bypass": False,
        "deployment_branch_policy": {"protected_branches": True, "custom_branch_policies": False},
        "protection_rules": [
            {
                "type": "required_reviewers",
                "prevent_self_review": True,
                "reviewers": [
                    {"type": reviewer["type"], "reviewer": {"id": reviewer["id"]}}
                    for reviewer in reviewer_values
                ],
            },
            {"type": "branch_policy"},
        ],
    }

if args[0] == "api" and "--method" not in args:
    endpoint = args[1]
    if "/actions/runs/" in endpoint:
        print(json.dumps({
            "id": int(os.environ["FAKE_GH_RUN_ID"]),
            "run_attempt": 1,
            "event": "workflow_dispatch",
            "path": ".github/workflows/release.yml",
            "head_sha": os.environ["FAKE_GH_SOURCE_COMMIT"],
            "head_branch": "main",
            "status": "in_progress",
            "repository": {"full_name": "synara-ai/synara"},
        }))
    elif "/branches/" in endpoint:
        print(json.dumps({
            "enforce_admins": {"enabled": True},
            "required_pull_request_reviews": {
                "dismiss_stale_reviews": True,
                "required_approving_review_count": 1,
            },
            "required_status_checks": {"strict": True, "checks": [{"context": "CI"}]},
            "allow_force_pushes": {"enabled": False},
            "allow_deletions": {"enabled": False},
        }))
    else:
        print(json.dumps(environment()))
elif args[0] == "api":
    request = json.loads(stdin)
    state["reviewers"] = request["reviewers"]
    print(json.dumps(environment(request["reviewers"])))
elif args[:2] == ["variable", "set"]:
    state["variables"][args[2]] = stdin
elif args[:2] == ["variable", "list"]:
    print(json.dumps([{"name": name, "value": value} for name, value in state["variables"].items()]))
elif args[:2] == ["secret", "set"]:
    state["secretNames"] = [args[2]]
    state["secretSha256"] = hashlib.sha256(stdin.encode()).hexdigest()
elif args[:2] == ["secret", "list"]:
    print(json.dumps([{"name": name} for name in state["secretNames"]]))
else:
    raise SystemExit("unexpected fake gh invocation: " + repr(args))
state_path.write_text(json.dumps(state))
""",
            encoding="utf-8",
        )
        fake_gh.chmod(0o700)
        state_path = self.evidence / "fake-gh-state.json"
        output = self.evidence / "protected-environment-application.json"
        environment = dict(os.environ)
        environment["PATH"] = str(fake_bin) + os.pathsep + environment["PATH"]
        environment["FAKE_GH_STATE"] = str(state_path)
        environment["FAKE_GH_RUN_ID"] = "123456789"
        environment["FAKE_GH_SOURCE_COMMIT"] = self.fixture.release["source"]["commit"]
        applied = subprocess.run(
            [
                "node",
                str(PROTECTED_ENVIRONMENT_APPLIER),
                "--configuration",
                str(configuration),
                "--repository",
                "synara-ai/synara",
                "--reviewer",
                "User:101",
                "--reviewer",
                "Team:202",
                "--output",
                str(output),
            ],
            check=False,
            capture_output=True,
            text=True,
            env=environment,
        )
        self.assertEqual(applied.returncode, 0, applied.stderr)
        self.assertNotIn(secret, applied.stdout)
        receipt = json.loads(output.read_text(encoding="utf-8"))
        self.assertEqual(
            receipt["assessment"], "configuration-applied-not-release-approved"
        )
        self.assertFalse(receipt["verification"]["secretValueReadableFromGitHub"])
        self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)
        self.assertEqual(
            stat.S_IMODE(output.with_suffix(".json.sha256").stat().st_mode), 0o600
        )
        state = json.loads(state_path.read_text(encoding="utf-8"))
        self.assertEqual(state["variables"], config["variables"])
        self.assertEqual(state["secretSha256"], hashlib.sha256(secret.encode()).hexdigest())
        self.assertFalse(any(call["sensitiveValueInArgv"] for call in state["calls"]))

    def test_requires_all_ten_named_receipts(self) -> None:
        result = self.run_preparer(omit="workerSupplyChain")
        self.assertEqual(result.returncode, 2)
        self.assertIn("--worker-supply-chain-receipt", result.stderr)
        self.assert_no_outputs()

    def test_semantic_failure_publishes_no_partial_outputs(self) -> None:
        self.fixture.write_bundle()
        worker_path = self.evidence / "workerSupplyChain-receipt.json"
        worker = json.loads(worker_path.read_text(encoding="utf-8"))
        worker["source"]["workerImage"] = "sha256:" + "f" * 64
        worker_path.write_text(json.dumps(worker, indent=2, sort_keys=True) + "\n", encoding="utf-8")

        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("workerSupplyChain.source.workerImage", result.stderr)
        self.assert_no_outputs()

    def test_rejects_duplicate_input_and_traversal(self) -> None:
        self.fixture.write_bundle()
        command = self.command()
        index = command.index("--internal-cost-receipt") + 1
        command[index] = "capacity-receipt.json"
        result = subprocess.run(command, check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("duplicates another bundle input", result.stderr)
        self.assert_no_outputs()

        command = self.command()
        command[command.index("--release-evidence") + 1] = "../release-evidence.json"
        result = subprocess.run(command, check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("traversal-free", result.stderr)
        self.assert_no_outputs()

    def test_rejects_symlinked_input(self) -> None:
        self.fixture.write_bundle()
        link = self.evidence / "internal-cost-link.json"
        link.symlink_to("internalCost-receipt.json")
        command = self.command()
        command[command.index("--internal-cost-receipt") + 1] = link.name
        result = subprocess.run(command, check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("symbolic link", result.stderr)
        self.assert_no_outputs()

        real_output = self.evidence / "real-output"
        real_output.mkdir()
        output_link = self.evidence / "output-link"
        output_link.symlink_to(real_output, target_is_directory=True)
        command = self.command()
        command[command.index("--manifest-output") + 1] = "output-link/bundle.json"
        result = subprocess.run(command, check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("symbolic link", result.stderr)
        self.assert_no_outputs()

    def test_never_overwrites_existing_candidate_evidence(self) -> None:
        first = self.run_preparer()
        self.assertEqual(first.returncode, 0, first.stderr)
        manifest_before = self.manifest_output.read_bytes()
        receipt_before = self.receipt_output.read_bytes()

        second = self.run_preparer()
        self.assertEqual(second.returncode, 2)
        self.assertIn("already exists", second.stderr)
        self.assertEqual(self.manifest_output.read_bytes(), manifest_before)
        self.assertEqual(self.receipt_output.read_bytes(), receipt_before)

    def test_preexisting_sidecar_prevents_any_new_output(self) -> None:
        self.fixture.write_bundle()
        sidecar = self.receipt_output.with_suffix(".json.sha256")
        sidecar.write_text("retained evidence\n", encoding="utf-8")

        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("output already exists", result.stderr)
        self.assertFalse(self.manifest_output.exists())
        self.assertFalse(self.manifest_output.with_suffix(".json.sha256").exists())
        self.assertFalse(self.receipt_output.exists())
        self.assertEqual(sidecar.read_text(encoding="utf-8"), "retained evidence\n")

    def test_publish_failure_removes_every_new_output(self) -> None:
        self.fixture.write_bundle()
        arguments = candidate_preparer.build_parser().parse_args(self.command()[2:])
        real_link = candidate_preparer.os.link
        calls = 0

        def fail_second_link(source: pathlib.Path, destination: pathlib.Path) -> None:
            nonlocal calls
            calls += 1
            if calls == 2:
                raise OSError("simulated publication failure")
            real_link(source, destination)

        with mock.patch.object(candidate_preparer.os, "link", side_effect=fail_second_link):
            with self.assertRaisesRegex(
                candidate_preparer.CandidatePreparationError,
                "could not publish the complete immutable candidate bundle",
            ):
                candidate_preparer.prepare(arguments)
        self.assert_no_outputs()


if __name__ == "__main__":
    unittest.main()
