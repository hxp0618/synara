from __future__ import annotations

import copy
import importlib.util
import json
import pathlib
import stat
import subprocess
import sys
import unittest
from argparse import Namespace
from unittest import mock


DIRECTORY = pathlib.Path(__file__).resolve().parent
if str(DIRECTORY) not in sys.path:
    sys.path.insert(0, str(DIRECTORY))

PREPARE_SCRIPT = DIRECTORY / "prepare_recovery_evidence.py"
PREPARE_SPEC = importlib.util.spec_from_file_location(
    "prepare_recovery_evidence", PREPARE_SCRIPT
)
assert PREPARE_SPEC is not None and PREPARE_SPEC.loader is not None
PREPARE = importlib.util.module_from_spec(PREPARE_SPEC)
PREPARE_SPEC.loader.exec_module(PREPARE)

FIXTURE_SCRIPT = DIRECTORY / "test_validate_recovery_evidence.py"
FIXTURE_SPEC = importlib.util.spec_from_file_location(
    "recovery_validator_fixture", FIXTURE_SCRIPT
)
assert FIXTURE_SPEC is not None and FIXTURE_SPEC.loader is not None
FIXTURE = importlib.util.module_from_spec(FIXTURE_SPEC)
FIXTURE_SPEC.loader.exec_module(FIXTURE)


class PrepareRecoveryEvidenceTest(unittest.TestCase):
    def setUp(self) -> None:
        self.fixture = FIXTURE.ValidateRecoveryEvidenceTest(
            "test_v2_candidate_bound_recovery_is_eligible_for_human_review_only"
        )
        self.fixture.setUp()
        self.root = self.fixture.root
        self.evidence = self.fixture.evidence
        self.subject_draft = self.root / "recovery-subject-draft.json"
        self.subject_manifest = self.root / "recovery-subject-manifest.json"
        self.subject_result = self.root / "recovery-subject.json"
        self.final_draft = self.root / "recovery-final-draft.json"
        self.final_manifest = self.root / "recovery-final-manifest.json"
        self.final_result = self.root / "recovery-receipt.json"
        self.payload = self.draft_payload()
        self.write_json(self.subject_draft, self.payload)

    def tearDown(self) -> None:
        self.fixture.tearDown()

    def draft_payload(self) -> dict[str, object]:
        payload = copy.deepcopy(self.fixture.manifest_data)
        payload["schemaVersion"] = PREPARE.DRAFT_SCHEMA_VERSION
        payload["approvals"] = {}
        for component in payload["components"]:
            for field in PREPARE.EVIDENCE_FIELDS:
                component[field] = "evidence/" + component[field]["path"]
        return payload

    @staticmethod
    def write_json(path: pathlib.Path, value: object) -> None:
        path.write_text(json.dumps(value, indent=2) + "\n", encoding="utf-8")

    def command(
        self,
        mode: str,
        draft: pathlib.Path,
        manifest: pathlib.Path,
        result: pathlib.Path,
    ) -> list[str]:
        return [
            sys.executable,
            str(PREPARE_SCRIPT),
            "--mode",
            mode,
            "--evidence-root",
            str(self.root),
            "--draft",
            draft.name,
            "--expected-candidate-binding-sha256",
            self.fixture.expected_candidate_binding,
            "--manifest-output",
            manifest.name,
            "--result-output",
            result.name,
            "--validated-at",
            self.fixture.validated_at,
        ]

    def args(
        self,
        mode: str,
        draft: pathlib.Path,
        manifest: pathlib.Path,
        result: pathlib.Path,
    ) -> Namespace:
        return Namespace(
            mode=mode,
            evidence_root=str(self.root),
            draft=draft.name,
            expected_candidate_binding_sha256=self.fixture.expected_candidate_binding,
            manifest_output=manifest.name,
            result_output=result.name,
            validated_at=self.fixture.validated_at,
        )

    def prepare_subject(self) -> str:
        completed = subprocess.run(
            self.command(
                "subject",
                self.subject_draft,
                self.subject_manifest,
                self.subject_result,
            ),
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(completed.returncode, 0, completed.stderr)
        subject = json.loads(self.subject_result.read_text(encoding="utf-8"))
        self.assertEqual(subject["assessment"], "subject-derived-not-recovery-validated")
        return subject["recoverySubjectSha256"]

    def write_final_draft(self, subject: str) -> None:
        payload = copy.deepcopy(self.payload)
        approvals = {}
        for role in PREPARE.APPROVAL_ROLES:
            document = copy.deepcopy(self.fixture.approval_documents[role])
            document["approval"]["subjectSha256"] = subject
            path = self.evidence / f"prepared-{role}-approval.json"
            self.write_json(path, document)
            approvals[role] = path.relative_to(self.root).as_posix()
        payload["approvals"] = approvals
        self.write_json(self.final_draft, payload)

    def test_two_stage_preparation_publishes_exact_bound_receipt(self) -> None:
        subject = self.prepare_subject()
        self.write_final_draft(subject)
        completed = subprocess.run(
            self.command("final", self.final_draft, self.final_manifest, self.final_result),
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(completed.returncode, 0, completed.stderr)
        manifest = json.loads(self.final_manifest.read_text(encoding="utf-8"))
        receipt = json.loads(self.final_result.read_text(encoding="utf-8"))
        self.assertEqual(manifest["schemaVersion"], PREPARE.SCHEMA_VERSION)
        self.assertEqual(set(manifest["approvals"]["security"]), {"path", "sha256"})
        self.assertEqual(receipt["manifest"]["path"], self.final_manifest.name)
        self.assertTrue(receipt["eligibleForHumanGateReview"])
        for output in (
            self.subject_manifest,
            self.subject_manifest.with_suffix(".json.sha256"),
            self.subject_result,
            self.subject_result.with_suffix(".json.sha256"),
            self.final_manifest,
            self.final_manifest.with_suffix(".json.sha256"),
            self.final_result,
            self.final_result.with_suffix(".json.sha256"),
        ):
            self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)

    def test_final_before_exact_subject_approvals_publishes_nothing(self) -> None:
        self.write_final_draft("sha256:" + "f" * 64)
        completed = subprocess.run(
            self.command("final", self.final_draft, self.final_manifest, self.final_result),
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(completed.returncode, 2)
        self.assertIn("does not bind the exact recovery subject", completed.stderr)
        self.assertFalse(self.final_manifest.exists())
        self.assertFalse(self.final_result.exists())

    def test_secret_evidence_publishes_nothing_without_echoing_secret(self) -> None:
        secret = "Authorization: Bearer recovery-preparation-secret"
        unsafe = self.evidence / "unsafe-client-evidence.json"
        unsafe.write_text(secret + "\n", encoding="utf-8")
        self.payload["components"][0]["clientEvidence"] = unsafe.relative_to(
            self.root
        ).as_posix()
        self.write_json(self.subject_draft, self.payload)
        completed = subprocess.run(
            self.command(
                "subject",
                self.subject_draft,
                self.subject_manifest,
                self.subject_result,
            ),
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(completed.returncode, 2)
        self.assertIn("prohibited bearer credential material", completed.stderr)
        self.assertNotIn(secret, completed.stderr)
        self.assertFalse(self.subject_manifest.exists())

    def test_rejects_symlinked_evidence_and_traversal_output(self) -> None:
        source = self.evidence / "postgresql-client.json"
        link = self.evidence / "linked-client.json"
        link.symlink_to(source.name)
        self.payload["components"][0]["clientEvidence"] = link.relative_to(
            self.root
        ).as_posix()
        self.write_json(self.subject_draft, self.payload)
        completed = subprocess.run(
            self.command(
                "subject",
                self.subject_draft,
                self.subject_manifest,
                self.subject_result,
            ),
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(completed.returncode, 2)
        self.assertIn("must not traverse a symlink", completed.stderr)

        self.payload["components"][0]["clientEvidence"] = source.relative_to(
            self.root
        ).as_posix()
        self.write_json(self.subject_draft, self.payload)
        command = self.command(
            "subject", self.subject_draft, self.subject_manifest, self.subject_result
        )
        command[command.index("--manifest-output") + 1] = "../outside.json"
        completed = subprocess.run(command, check=False, capture_output=True, text=True)
        self.assertEqual(completed.returncode, 2)
        self.assertIn("traversal-free and relative", completed.stderr)

    def test_result_publication_failure_rolls_back_manifest_pair(self) -> None:
        real_publish = PREPARE.publish_immutable_with_sha256
        calls = 0

        def fail_result(path: pathlib.Path, encoded: bytes) -> None:
            nonlocal calls
            calls += 1
            if calls == 2:
                raise PREPARE.ImmutableEvidenceIOError(
                    "simulated result publication failure"
                )
            real_publish(path, encoded)

        with mock.patch.object(
            PREPARE, "publish_immutable_with_sha256", side_effect=fail_result
        ):
            with self.assertRaises(PREPARE.RecoveryPreparationError):
                PREPARE.prepare(
                    self.args(
                        "subject",
                        self.subject_draft,
                        self.subject_manifest,
                        self.subject_result,
                    )
                )
        self.assertFalse(self.subject_manifest.exists())
        self.assertFalse(self.subject_manifest.with_suffix(".json.sha256").exists())
        self.assertFalse(self.subject_result.exists())


if __name__ == "__main__":
    unittest.main()
