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


SCRIPT = pathlib.Path(__file__).with_name("validate_final_ga_review.py")
SPEC = importlib.util.spec_from_file_location("validate_final_ga_review", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)

CANDIDATE_ID = "v0.6.3-stage6-rc1"
COMMIT = "a" * 40
ENVIRONMENT_ID = "production-like/stage6-rc1"
FINAL_ASSET_SET_SHA256 = "sha256:" + "b" * 64
AUDIT_REQUEST_IDS = [
    "11111111-1111-4111-8111-111111111111",
    "22222222-2222-4222-8222-222222222222",
]


class ValidateFinalGAReviewTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temporary.name)
        self.manifest_path = self.root / "final-review.json"
        self.output = self.root / "final-review-receipt.json"
        self.candidate_receipt = {
            "schemaVersion": "synara.stage6-candidate-evidence-bundle-validation.v5",
            "candidate": {
                "candidateId": CANDIDATE_ID,
                "sourceCommit": COMMIT,
                "environmentId": ENVIRONMENT_ID,
                "lockfileSha256": "c" * 64,
            },
            "desktopArtifactSetSha256": "sha256:" + "e" * 64,
            "candidateConsistencyValidated": True,
            "requiredReceiptCount": 10,
            "allRequiredReceiptsReadyForCandidateReview": True,
            "eligibleForCandidateEvidenceReview": True,
            "assessment": "evidence-consistent-not-ga-approved",
            "validatedAt": "2026-08-01T23:00:00Z",
        }
        self.candidate_reference = self.write_json("candidate-receipt.json", self.candidate_receipt)
        self.final_assets = {
            "schemaVersion": "synara.release-final-asset-set.v1",
            "source": {"tag": CANDIDATE_ID, "commit": COMMIT, "lockfileSha256": "c" * 64},
            "version": "0.6.3",
            "updateChannel": "latest",
            "updaterFeed": {"schemaVersion": "synara.release-updater-feed.v1", "sha256": "d" * 64},
            "files": [],
            "finalAssetSetSha256": FINAL_ASSET_SET_SHA256,
            "assessment": "final-assets-bound-not-published",
        }
        self.final_asset_reference = self.write_json("release-final-asset-set.json", self.final_assets)
        self.protected_approval = {
            "schemaVersion": "synara.stage6-release-approval.v4",
            "candidate": {
                "candidateId": CANDIDATE_ID,
                "sourceCommit": COMMIT,
                "lockfileSha256": "c" * 64,
                "candidateBundleReceiptSha256": self.candidate_reference["sha256"],
                "desktopArtifactSetSha256": "sha256:" + "e" * 64,
                "finalAssetSetSha256": FINAL_ASSET_SET_SHA256,
                "rawFinalCrossBindingSha256": "sha256:" + "f" * 64,
            },
            "build": {
                "runId": "123456",
                "runAttempt": 1,
                "repository": "synara-ai/synara",
                "workflowRef": "synara-ai/synara/.github/workflows/release.yml@refs/heads/main",
                "sourceBranch": "main",
                "actor": "release-operator",
                "triggeringActor": "release-operator",
            },
            "approval": {
                "environment": "stage6-enterprise-ga",
                "environmentId": 987,
                "configuredReviewers": [
                    {"type": "User", "id": 1, "login": "release-reviewer"},
                    {"type": "Team", "id": 2, "slug": "security"},
                ],
                "preventSelfReview": True,
                "administratorBypassDisabled": True,
                "protectedBranchesOnly": True,
                "actualReviewer": {"id": 3, "login": "security-reviewer"},
                "commentSha256": "sha256:" + "1" * 64,
                "environmentConfigurationSha256": "sha256:" + "2" * 64,
                "reviewSha256": "sha256:" + "3" * 64,
                "approvalEvidenceSha256": "sha256:" + "4" * 64,
                "recordedAt": "2026-08-02T00:30:00Z",
            },
            "exactCandidatePublicationAuthorized": True,
            "assessment": "protected-environment-gate-recorded-not-ga-approved",
        }
        self.approval_reference = self.write_json("protected-release-approval.json", self.protected_approval)
        checklist = "\n".join(
            ["# Final candidate checklist", *MODULE.REQUIRED_CHECKLIST_HEADINGS]
            + [f"- [x] completed row {index}" for index in range(len(MODULE.CONTROL_OWNERS) + 10)]
            + [f"Candidate: {CANDIDATE_ID}", f"Commit: {COMMIT}"]
        )
        self.checklist_reference = self.write_bytes("candidate-checklist.md", (checklist + "\n").encode())
        notice = "\n".join(
            ["# Candidate change notice", *MODULE.REQUIRED_NOTICE_HEADINGS]
            + [f"- [x] completed notice row {index}" for index in range(20)]
            + [f"Candidate: {CANDIDATE_ID}", f"Commit: {COMMIT}"]
        )
        self.notice_reference = self.write_bytes("change-notice.md", (notice + "\n").encode())
        audit = {
            "candidateId": CANDIDATE_ID,
            "sourceCommit": COMMIT,
            "requestIds": AUDIT_REQUEST_IDS,
        }
        self.audit_reference = self.write_json("platform-audit-export.json", audit)
        self.control_evidence = self.write_json("control-evidence.json", {"candidateId": CANDIDATE_ID})
        self.approval_evidence = {
            role: self.write_json(f"{role}-approval.json", {"candidateId": CANDIDATE_ID, "role": role})
            for role in sorted(MODULE.BASE_FINAL_APPROVAL_ROLES | {"privacy_legal"})
        }
        self.reset_manifest()

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def write_bytes(self, name: str, encoded: bytes) -> dict[str, str]:
        path = self.root / name
        path.write_bytes(encoded)
        return {"path": name, "sha256": "sha256:" + hashlib.sha256(encoded).hexdigest()}

    def write_json(self, name: str, payload: object) -> dict[str, str]:
        return self.write_bytes(name, (json.dumps(payload, indent=2, sort_keys=True) + "\n").encode())

    def rewrite_json(self, reference: dict[str, str], payload: object) -> None:
        encoded = (json.dumps(payload, indent=2, sort_keys=True) + "\n").encode()
        (self.root / reference["path"]).write_bytes(encoded)
        reference["sha256"] = "sha256:" + hashlib.sha256(encoded).hexdigest()

    def reset_manifest(self) -> None:
        final_roles = sorted(MODULE.BASE_FINAL_APPROVAL_ROLES)
        self.manifest = {
            "schemaVersion": MODULE.SCHEMA_VERSION,
            "candidate": {
                "candidateId": CANDIDATE_ID,
                "releaseTag": CANDIDATE_ID,
                "sourceCommit": COMMIT,
                "environmentId": ENVIRONMENT_ID,
                "candidateBundleReceipt": copy.deepcopy(self.candidate_reference),
                "protectedReleaseApproval": copy.deepcopy(self.approval_reference),
                "finalAssetSet": copy.deepcopy(self.final_asset_reference),
                "copiedChecklist": copy.deepcopy(self.checklist_reference),
                "changeNotice": copy.deepcopy(self.notice_reference),
                "platformAuditExport": copy.deepcopy(self.audit_reference),
            },
            "releaseManagerId": "release-manager-1",
            "impactDomains": ["internal_cost", "code_change"],
            "controlDecisions": [
                {
                    "id": control_id,
                    "ownerRole": owner,
                    "approverId": f"{owner}-control-owner",
                    "status": "passed",
                    "decidedAt": "2026-08-02T01:00:00Z",
                    "evidence": [copy.deepcopy(self.control_evidence)],
                }
                for control_id, owner in sorted(MODULE.CONTROL_OWNERS.items())
            ],
            "finalApprovals": [
                {
                    "role": role,
                    "approverId": f"{role}-final-approver",
                    "decision": "approved-for-ga-authority-review",
                    "approvedAt": "2026-08-02T02:00:00Z",
                    "evidence": copy.deepcopy(self.approval_evidence[role]),
                }
                for role in final_roles
            ],
            "platformAuditRequestIds": list(AUDIT_REQUEST_IDS),
            "decisionSummary": "All required evidence and the observation window were reviewed for this candidate.",
            "residualRiskDisposition": "none",
            "residualRisks": [],
            "startedAt": "2026-08-02T00:00:00Z",
            "completedAt": "2026-08-02T03:00:00Z",
        }

    def run_validator(self) -> subprocess.CompletedProcess[str]:
        self.manifest_path.write_text(json.dumps(self.manifest, indent=2) + "\n", encoding="utf-8")
        return subprocess.run(
            [
                sys.executable,
                str(SCRIPT),
                "--manifest",
                str(self.manifest_path),
                "--evidence-root",
                str(self.root),
                "--output",
                str(self.output),
                "--validated-at",
                "2026-08-02T04:00:00Z",
            ],
            check=False,
            capture_output=True,
            text=True,
        )

    def test_validates_exact_final_archive_without_claiming_ga_authority(self) -> None:
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = json.loads(self.output.read_text(encoding="utf-8"))
        self.assertTrue(receipt["eligibleForExternalGAAuthorityReview"])
        self.assertTrue(receipt["allRequiredControlsPassed"])
        self.assertTrue(receipt["allRequiredFinalApprovalsApproved"])
        self.assertEqual(receipt["controlCount"], len(MODULE.CONTROL_OWNERS))
        self.assertEqual(receipt["assessment"], MODULE.ASSESSMENT)
        self.assertFalse(receipt["verificationBoundary"]["externalEvidenceAuthorityVerified"])
        self.assertEqual(stat.S_IMODE(self.output.stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE((self.root / "final-review-receipt.json.sha256").stat().st_mode), 0o600)

    def test_records_failed_control_without_relabeling_it_eligible(self) -> None:
        self.manifest["controlDecisions"][0]["status"] = "failed"
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = json.loads(self.output.read_text(encoding="utf-8"))
        self.assertFalse(receipt["allRequiredControlsPassed"])
        self.assertFalse(receipt["eligibleForExternalGAAuthorityReview"])
        self.assertEqual(receipt["controlStatusCounts"]["failed"], 1)

    def test_records_denied_final_approval_without_relabeling_it_eligible(self) -> None:
        self.manifest["finalApprovals"][0]["decision"] = "denied"
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = json.loads(self.output.read_text(encoding="utf-8"))
        self.assertFalse(receipt["allRequiredFinalApprovalsApproved"])
        self.assertFalse(receipt["eligibleForExternalGAAuthorityReview"])

    def test_accepts_bounded_future_residual_risk_record(self) -> None:
        self.manifest["residualRiskDisposition"] = "accepted"
        self.manifest["residualRisks"] = [
            {
                "id": "risk-1",
                "owner": "product-owner-1",
                "dueAt": "2026-09-02T03:00:00Z",
                "summary": "A bounded remaining documentation timing risk.",
                "acceptanceReason": "A bounded non-prohibited residual risk remains under active remediation.",
                "evidenceReference": "https://evidence.example.test/stage6/risk-1",
            }
        ]
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = json.loads(self.output.read_text(encoding="utf-8"))
        self.assertEqual(receipt["residualRiskDisposition"], "accepted")
        self.assertEqual(receipt["residualRisks"][0]["id"], "risk-1")

    def test_rejects_final_approval_that_predates_control_review(self) -> None:
        self.manifest["finalApprovals"][0]["approvedAt"] = "2026-08-02T00:45:00Z"
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("must not predate", result.stderr)

    def test_rejects_cross_candidate_protected_approval(self) -> None:
        self.protected_approval["candidate"]["sourceCommit"] = "f" * 40
        approval_reference = self.manifest["candidate"]["protectedReleaseApproval"]
        self.rewrite_json(approval_reference, self.protected_approval)
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("protected release approval does not match", result.stderr)

    def test_rejects_unready_candidate_and_weakened_protected_environment(self) -> None:
        self.candidate_receipt["allRequiredReceiptsReadyForCandidateReview"] = False
        candidate_reference = self.manifest["candidate"]["candidateBundleReceipt"]
        self.rewrite_json(candidate_reference, self.candidate_receipt)
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("not an eligible internal-self-hosted v4 consistency receipt", result.stderr)

        self.candidate_receipt["allRequiredReceiptsReadyForCandidateReview"] = True
        self.rewrite_json(candidate_reference, self.candidate_receipt)
        self.protected_approval["candidate"]["candidateBundleReceiptSha256"] = candidate_reference["sha256"]
        self.protected_approval["build"]["runAttempt"] = 2
        approval_reference = self.manifest["candidate"]["protectedReleaseApproval"]
        self.rewrite_json(approval_reference, self.protected_approval)
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("first run attempt", result.stderr)

    def test_rejects_unfinished_checklist(self) -> None:
        checklist_reference = self.manifest["candidate"]["copiedChecklist"]
        path = self.root / checklist_reference["path"]
        encoded = path.read_bytes().replace(b"- [x] completed row 0", b"- [ ] completed row 0")
        path.write_bytes(encoded)
        checklist_reference["sha256"] = "sha256:" + hashlib.sha256(encoded).hexdigest()
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("still contains unchecked rows", result.stderr)

    def test_requires_privacy_legal_and_distinct_final_approvers(self) -> None:
        self.manifest["impactDomains"].append("data_residency")
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("final approval inventory drifted", result.stderr)

        self.reset_manifest()
        self.manifest["impactDomains"].append("data_residency")
        self.manifest["finalApprovals"].append(
            {
                "role": "privacy_legal",
                "approverId": "privacy-legal-final-approver",
                "decision": "approved-for-ga-authority-review",
                "approvedAt": "2026-08-02T02:00:00Z",
                "evidence": copy.deepcopy(self.approval_evidence["privacy_legal"]),
            }
        )
        self.manifest["finalApprovals"][1]["approverId"] = self.manifest["finalApprovals"][0]["approverId"]
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("must be distinct", result.stderr)

    def test_rejects_missing_platform_audit_request_id(self) -> None:
        self.manifest["platformAuditRequestIds"].append("33333333-3333-4333-8333-333333333333")
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("platform Audit export is missing request ID", result.stderr)

    def test_rejects_symlinked_evidence(self) -> None:
        outside = self.root / "outside.json"
        outside.write_text("{}\n", encoding="utf-8")
        linked = self.root / "linked-control-evidence.json"
        linked.symlink_to(outside)
        control_reference = self.manifest["controlDecisions"][0]["evidence"][0]
        control_reference["path"] = linked.name
        control_reference["sha256"] = "sha256:" + hashlib.sha256(outside.read_bytes()).hexdigest()
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("must not traverse a symlink", result.stderr)

    def test_rejects_existing_output_without_modifying_it(self) -> None:
        sidecar = self.root / "final-review-receipt.json.sha256"
        self.output.write_text("retained receipt\n", encoding="utf-8")
        sidecar.write_text("retained sidecar\n", encoding="utf-8")
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("must not already exist", result.stderr)
        self.assertEqual(self.output.read_text(encoding="utf-8"), "retained receipt\n")
        self.assertEqual(sidecar.read_text(encoding="utf-8"), "retained sidecar\n")


if __name__ == "__main__":
    unittest.main()
