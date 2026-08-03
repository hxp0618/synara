from __future__ import annotations

import copy
import hashlib
import importlib.util
import json
import pathlib
import stat
import subprocess
import sys
import tempfile
import unittest


SCRIPT = pathlib.Path(__file__).with_name("validate_recovery_evidence.py")
SPEC = importlib.util.spec_from_file_location("stage6_recovery_validator", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
VALIDATOR = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(VALIDATOR)

APPROVAL_ROLES = ("database", "kms", "operations", "security", "storage")


class ValidateRecoveryEvidenceTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temporary.name)
        self.evidence = self.root / "evidence"
        self.evidence.mkdir()
        self.candidate = {
            "candidateId": "stage6-rc1",
            "sourceCommit": "a" * 40,
            "lockfileSha256": "b" * 64,
            "environmentClass": "production-like",
            "environmentId": "production-like/stage6-rc1",
            "regions": ["cn-east-1", "cn-east-2"],
            "origins": {
                "controlPlaneBaseUrl": "https://control.example.test/v1",
                "webBaseUrl": "https://app.example.test",
                "adminBaseUrl": "https://admin.example.test",
            },
            "artifacts": {
                "controlPlaneImage": "sha256:" + "1" * 64,
                "workerImage": "sha256:" + "2" * 64,
                "providerHostImage": "sha256:" + "3" * 64,
                "webArtifact": "sha256:" + "4" * 64,
                "adminArtifact": "sha256:" + "5" * 64,
                "desktopArtifacts": {
                    "linux-x64": "sha256:" + "6" * 64,
                    "macos-arm64": "sha256:" + "7" * 64,
                    "macos-x64": "sha256:" + "8" * 64,
                    "windows-x64": "sha256:" + "9" * 64,
                },
            },
            "migrationTail": {
                "name": "000110_support_access_operator_authority.sql",
                "sha256": "sha256:" + "c" * 64,
            },
        }
        self.restored_identity = {
            "sourceCommit": self.candidate["sourceCommit"],
            "lockfileSha256": self.candidate["lockfileSha256"],
            "artifacts": copy.deepcopy(self.candidate["artifacts"]),
            "migrationTail": copy.deepcopy(self.candidate["migrationTail"]),
        }
        self.components: list[dict[str, object]] = []
        profiles = {
            "postgresql": "postgresql-pitr",
            "object-storage": "object-versioned-replica",
            "kms": "kms-vault-raft-snapshot",
            "queue": "queue-postgres-outbox-replay",
        }
        for index, (component, profile) in enumerate(profiles.items()):
            references = {}
            for kind in ("backup", "restore", "client"):
                path = self.evidence / f"{component}-{kind}.json"
                path.write_text(f"{component} {kind} proof\n", encoding="utf-8")
                references[f"{kind}Evidence"] = self.reference(path)
            self.components.append(
                {
                    "component": component,
                    "profile": profile,
                    "sourceAuthority": f"source-{component}",
                    "restoreTarget": f"restore-{component}",
                    "sourceRegion": "cn-east-1",
                    "restoreRegion": "cn-east-2",
                    "sourceFailureDomain": "cn-east-1/account-prod",
                    "restoreFailureDomain": "cn-east-2/account-drill",
                    "sourceCutoffAt": f"2026-07-30T00:{10 + index:02d}:00Z",
                    "restoredThroughAt": f"2026-07-30T00:{8 + index:02d}:00Z",
                    "recoveryStartedAt": f"2026-07-30T00:{15 + index:02d}:00Z",
                    "serviceReadyAt": f"2026-07-30T00:{45 + index:02d}:00Z",
                    "restoreServedCanary": True,
                    **references,
                }
            )
        self.approval_documents = {
            role: {
                "schemaVersion": "synara.recovery-drill-approval-evidence.v1",
                "approval": {
                    "role": role,
                    "approverId": f"{role}-approver-1",
                    "decision": "approved-for-human-gate-review",
                    "approvedAt": "2026-07-30T02:30:00Z",
                    "expiresAt": "2026-08-30T02:30:00Z",
                    "subjectSha256": "sha256:" + "0" * 64,
                },
                "verificationBoundary": {
                    "contentValidation": "strict-schema-and-subject-validated",
                    "externalVerification": (
                        "approver-identity-and-signature-required-not-verified"
                    ),
                },
            }
            for role in APPROVAL_ROLES
        }
        self.approvals: dict[str, dict[str, str]] = {}
        self.manifest_data = {
            "schemaVersion": "synara.recovery-drill-evidence.v2",
            "candidate": self.candidate,
            "drill": {
                "drillId": "4dc18f4e-7d97-40e9-85f2-38f313ebf246",
                "startedAt": "2026-07-30T00:00:00Z",
                "completedAt": "2026-07-30T02:00:00Z",
            },
            "restoredReleaseIdentity": self.restored_identity,
            "components": self.components,
            "approvals": self.approvals,
        }
        self.expected_candidate_binding = self.candidate_binding(self.candidate)
        self.validated_at = "2026-07-30T03:00:00Z"
        self.manifest = self.root / "manifest.json"
        self.write_manifest()

    def tearDown(self) -> None:
        self.temporary.cleanup()

    @staticmethod
    def reference(path: pathlib.Path) -> dict[str, str]:
        return {
            "path": path.name,
            "sha256": "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest(),
        }

    @staticmethod
    def candidate_binding(candidate: dict[str, object]) -> str:
        encoded = json.dumps(candidate, separators=(",", ":"), sort_keys=True).encode("utf-8")
        return "sha256:" + hashlib.sha256(encoded).hexdigest()

    def normalized_subject(self) -> str:
        candidate = VALIDATOR.validate_candidate(self.candidate)
        restored = VALIDATOR.validate_restored_release_identity(
            self.restored_identity, candidate
        )
        started = VALIDATOR.parse_utc(
            self.manifest_data["drill"]["startedAt"], "drill.startedAt"
        )
        completed = VALIDATOR.parse_utc(
            self.manifest_data["drill"]["completedAt"], "drill.completedAt"
        )
        used_paths: set[pathlib.Path] = set()
        total_bytes = [0]
        normalized_components = {}
        for component in self.components:
            name, normalized = VALIDATOR.validate_component(
                component,
                self.evidence.resolve(),
                started,
                completed,
                set(candidate["regions"]),
                used_paths,
                total_bytes,
            )
            normalized_components[name] = normalized
        return VALIDATOR.recovery_subject_digest(
            self.manifest_data["drill"]["drillId"],
            candidate,
            VALIDATOR.format_utc(started),
            VALIDATOR.format_utc(completed),
            restored,
            {name: normalized_components[name] for name in sorted(normalized_components)},
        )

    def write_approvals(self, subject_digest: str) -> None:
        for role in APPROVAL_ROLES:
            self.approval_documents[role]["approval"]["subjectSha256"] = subject_digest
            path = self.evidence / f"{role}-approval.json"
            path.write_text(
                json.dumps(self.approval_documents[role], indent=2) + "\n",
                encoding="utf-8",
            )
            self.approvals[role] = self.reference(path)

    def write_manifest(self, *, rebuild_approvals: bool = True) -> None:
        if rebuild_approvals:
            self.write_approvals(self.normalized_subject())
        self.manifest.write_text(
            json.dumps(self.manifest_data, indent=2) + "\n", encoding="utf-8"
        )

    def rewrite_approval(self, role: str) -> None:
        path = self.evidence / f"{role}-approval.json"
        path.write_text(
            json.dumps(self.approval_documents[role], indent=2) + "\n", encoding="utf-8"
        )
        self.approvals[role] = self.reference(path)
        self.write_manifest(rebuild_approvals=False)

    def command(self) -> list[str]:
        return [
            sys.executable,
            str(SCRIPT),
            "--manifest",
            str(self.manifest),
            "--evidence-root",
            str(self.evidence),
            "--output",
            str(self.root / "receipt.json"),
            "--expected-candidate-binding-sha256",
            self.expected_candidate_binding,
            "--validated-at",
            self.validated_at,
        ]

    def run_validator(self) -> subprocess.CompletedProcess[str]:
        return subprocess.run(self.command(), check=False, capture_output=True, text=True)

    def receipt(self) -> dict[str, object]:
        return json.loads((self.root / "receipt.json").read_text(encoding="utf-8"))

    def test_v2_candidate_bound_recovery_is_eligible_for_human_review_only(self) -> None:
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertEqual(receipt["schemaVersion"], "synara.recovery-drill-evidence-receipt.v2")
        self.assertEqual(receipt["assessment"], "evidence-validated-not-control-passed")
        self.assertTrue(receipt["eligibleForHumanGateReview"])
        self.assertTrue(receipt["allRestoreCanariesPassed"])
        self.assertEqual(receipt["components"]["postgresql"]["measuredRpoSeconds"], 120)
        self.assertEqual(receipt["candidate"], self.candidate)
        self.assertFalse(receipt["verificationBoundary"]["cryptographicSignaturesVerified"])
        output = self.root / "receipt.json"
        self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)
        self.assertEqual(
            stat.S_IMODE(output.with_suffix(".json.sha256").stat().st_mode), 0o600
        )

    def test_subject_only_mode_bootstraps_five_approval_files(self) -> None:
        self.manifest_data["approvals"] = {}
        self.write_manifest(rebuild_approvals=False)
        result = subprocess.run(
            [*self.command(), "--subject-only"],
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        subject = self.receipt()
        self.assertEqual(
            subject["schemaVersion"], "synara.recovery-drill-approval-subject.v1"
        )
        self.assertEqual(subject["recoverySubjectSha256"], self.normalized_subject())
        self.assertEqual(subject["assessment"], "subject-derived-not-recovery-validated")

    def test_rejects_candidate_drift(self) -> None:
        self.candidate["environmentId"] = "production-like/drifted"
        self.write_manifest()
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("candidate drift detected", result.stderr)

    def test_rejects_old_restored_artifact(self) -> None:
        self.restored_identity["artifacts"]["webArtifact"] = "sha256:" + "d" * 64
        self.write_manifest(rebuild_approvals=False)
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("stale artifact detected", result.stderr)

    def test_rejects_component_time_inversion(self) -> None:
        self.components[0]["restoredThroughAt"] = "2026-07-30T00:20:00Z"
        self.write_manifest(rebuild_approvals=False)
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("timestamps are out of order", result.stderr)

    def test_rejects_validation_before_approval(self) -> None:
        self.validated_at = "2026-07-30T02:15:00Z"
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("timestamps are out of order", result.stderr)

    def test_rejects_missing_approval_role(self) -> None:
        del self.approvals["storage"]
        self.write_manifest(rebuild_approvals=False)
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("approvals fields do not match", result.stderr)

    def test_rejects_reused_approval_role(self) -> None:
        self.approval_documents["security"]["approval"]["role"] = "operations"
        self.rewrite_approval("security")
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("role must be security", result.stderr)

    def test_rejects_reused_approver_identity(self) -> None:
        self.approval_documents["security"]["approval"]["approverId"] = (
            "operations-approver-1"
        )
        self.rewrite_approval("security")
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("five distinct approvers", result.stderr)

    def test_rejects_approval_for_another_subject(self) -> None:
        self.approval_documents["database"]["approval"]["subjectSha256"] = (
            "sha256:" + "e" * 64
        )
        self.rewrite_approval("database")
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("does not bind the exact recovery subject", result.stderr)

    def test_failed_canary_emits_non_pass_receipt(self) -> None:
        self.components[0]["restoreServedCanary"] = False
        self.write_manifest()
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertFalse(receipt["allRestoreCanariesPassed"])
        self.assertFalse(receipt["eligibleForHumanGateReview"])
        self.assertEqual(receipt["assessment"], "evidence-validated-not-control-passed")

    def test_missed_objective_emits_non_pass_receipt(self) -> None:
        self.components[0]["restoredThroughAt"] = "2026-07-30T00:01:00Z"
        self.write_manifest()
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertFalse(receipt["declaredMeasurementsWithinObjectives"])
        self.assertFalse(receipt["eligibleForHumanGateReview"])

    def test_reviewed_not_approved_decision_emits_non_pass_receipt(self) -> None:
        self.approval_documents["security"]["approval"]["decision"] = (
            "reviewed-not-approved"
        )
        self.rewrite_approval("security")
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertFalse(receipt["allRequiredApprovalsApproved"])
        self.assertFalse(receipt["eligibleForHumanGateReview"])

    def test_staging_environment_is_not_review_eligible(self) -> None:
        self.candidate["environmentClass"] = "staging"
        self.expected_candidate_binding = self.candidate_binding(self.candidate)
        self.write_manifest()
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.receipt()["eligibleForHumanGateReview"])

    def test_rejects_unsorted_candidate_regions(self) -> None:
        self.candidate["regions"] = ["cn-east-2", "cn-east-1"]
        self.write_manifest(rebuild_approvals=False)
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("must be sorted and unique", result.stderr)

    def test_rejects_same_failure_domain(self) -> None:
        self.components[0]["restoreFailureDomain"] = self.components[0][
            "sourceFailureDomain"
        ]
        self.write_manifest(rebuild_approvals=False)
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("restoreFailureDomain must differ", result.stderr)

    def test_rejects_tampered_component_evidence(self) -> None:
        (self.evidence / "queue-client.json").write_text("tampered\n", encoding="utf-8")
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("sha256 does not match", result.stderr)

    def test_rejects_symlink_component_evidence(self) -> None:
        original = self.evidence / "kms-client.json"
        target = self.root / "outside.json"
        target.write_bytes(original.read_bytes())
        original.unlink()
        original.symlink_to(target)
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("must not traverse a symlink", result.stderr)

    def test_rejects_secret_component_evidence_without_echoing_secret(self) -> None:
        secret = "Authorization: Bearer recovery-validator-secret"
        path = self.evidence / "queue-client.json"
        path.write_text(secret + "\n", encoding="utf-8")
        self.components[-1]["clientEvidence"] = self.reference(path)
        self.write_manifest(rebuild_approvals=False)
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("prohibited bearer credential material", result.stderr)
        self.assertNotIn(secret, result.stderr)

    def test_rejects_duplicate_manifest_fields(self) -> None:
        encoded = json.dumps(self.manifest_data, indent=2) + "\n"
        duplicated = encoded.replace(
            '  "schemaVersion": "synara.recovery-drill-evidence.v2",',
            '  "schemaVersion": "synara.recovery-drill-evidence.v2",\n'
            '  "schemaVersion": "synara.recovery-drill-evidence.v2",',
            1,
        )
        self.manifest.write_text(duplicated, encoding="utf-8")
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("duplicate JSON field", result.stderr)


if __name__ == "__main__":
    unittest.main()
