from __future__ import annotations

import copy
import hashlib
import json
import pathlib
import stat
import subprocess
import sys
import tempfile
import unittest


SCRIPT = pathlib.Path(__file__).with_name("validate_candidate_evidence_bundle.py")
COMMIT = "a" * 40
LOCKFILE_SHA256 = "b" * 64
ENVIRONMENT_ID = "production-like/stage6-rc1"
MIGRATION = {
    "name": "000164_session_settlement.sql",
    "sha256": "sha256:9094065b2ee3881d0a4c83f1b68430d7deec522cc46b557eeec97582f3407b38",
}
ARTIFACTS = {
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
}
ARTIFACT_SET_EVIDENCE_DIGESTS = {
    target: {
        field: "sha256:" + value * 64
        for field, value in zip(
            ("artifactAttestation", "attestationBundle", "provenance"),
            values,
            strict=True,
        )
    }
    for target, values in zip(
        sorted(ARTIFACTS["desktopArtifacts"]),
        ("abc", "def", "012", "345"),
        strict=True,
    )
}
ENROLLMENT_EVIDENCE_DIGESTS = {
    target: "sha256:" + value * 64
    for target, value in zip(
        sorted(ARTIFACTS["desktopArtifacts"]), "abcd", strict=True
    )
}
DESKTOP_TARGET_ARCHITECTURES = {
    "linux-x64": "x64",
    "macos-arm64": "arm64",
    "macos-x64": "x64",
    "windows-x64": "x64",
}
ENROLLMENT_FIELDS = {
    "firstLaunchChoiceRequired",
    "localModeRestorePassed",
    "cloudModeRestorePassed",
    "localToCloudSwitchPassed",
    "cloudToLocalSwitchPassed",
    "localModeNoCloudTrafficPassed",
    "cloudCredentialRetainedAcrossLocalSwitch",
    "deviceProofPassed",
    "initialHydrationPassed",
    "restartPersistencePassed",
    "rotationPassed",
    "replayDenied",
    "remoteRevocationPassed",
    "disconnectPassed",
    "localStatePreserved",
}
MODE_TRANSITION_SPECS = (
    ("first-use-local-choice", "unselected", "local"),
    ("local-restart-restore", "local", "local"),
    ("local-to-cloud-switch", "local", "cloud"),
    ("cloud-restart-restore", "cloud", "cloud"),
    ("cloud-to-local-switch", "cloud", "local"),
    ("local-to-cloud-resume", "local", "cloud"),
)


def desktop_artifact_set_sha256() -> str:
    encoded = "".join(
        f"{target}/{field}={ARTIFACT_SET_EVIDENCE_DIGESTS[target][field]}\n"
        for target in sorted(ARTIFACT_SET_EVIDENCE_DIGESTS)
        for field in ("artifactAttestation", "attestationBundle", "provenance")
    ).encode("utf-8")
    return "sha256:" + hashlib.sha256(encoded).hexdigest()


def desktop_enrollment_evidence_set_sha256() -> str:
    encoded = "".join(
        f"{target}/enrollment={ENROLLMENT_EVIDENCE_DIGESTS[target]}\n"
        for target in sorted(ENROLLMENT_EVIDENCE_DIGESTS)
    ).encode("utf-8")
    return "sha256:" + hashlib.sha256(encoded).hexdigest()


def desktop_enrollment_runtime(target: str) -> dict[str, object]:
    observed_at = [
        "2026-07-31T00:10:00Z",
        "2026-07-31T00:20:00Z",
        "2026-07-31T00:30:00Z",
        "2026-07-31T00:40:00Z",
        "2026-07-31T00:50:00Z",
        "2026-07-31T01:00:00Z",
    ]
    return {
        "schemaVersion": "synara.stage6-desktop-enrollment-runtime-evidence.v1",
        "candidate": {
            "candidateId": "stage6-rc1",
            "sourceCommit": COMMIT,
            "environmentId": ENVIRONMENT_ID,
            "controlPlaneBaseUrl": "https://control.example.test/v1",
        },
        "target": {
            "id": target,
            "runnerReference": f"runner/{target}",
            "hostArchitecture": DESKTOP_TARGET_ARCHITECTURES[target],
            "executionMode": "native",
        },
        "startedAt": "2026-07-31T00:05:00Z",
        "completedAt": "2026-07-31T01:05:00Z",
        "modeTransitions": [
            {
                "id": transition_id,
                "from": from_mode,
                "to": to_mode,
                "observedAt": observed_at[index],
                "passed": True,
            }
            for index, (transition_id, from_mode, to_mode) in enumerate(
                MODE_TRANSITION_SPECS
            )
        ],
        "requestCounts": {
            "backendBeforeFirstChoice": 0,
            "cloudBeforeFirstChoice": 0,
            "cloudWhileLocal": 0,
        },
        "credentialContinuityMatched": True,
        "results": {field: True for field in ENROLLMENT_FIELDS},
    }


class ValidateCandidateEvidenceBundleTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temporary.name)
        self.evidence = self.root / "evidence"
        self.evidence.mkdir()
        self.bundle_schema = "synara.stage6-candidate-evidence-bundle.v5"
        self.compatibility = json.loads(
            (SCRIPT.parents[2] / "docs/release-matrices/stage-6-compatibility-v1.json").read_text(
                encoding="utf-8"
            )
        )
        self.release = {
            "schemaVersion": "synara-stage6-release-evidence-v2",
            "release": "stage6-rc1",
            "source": {"commit": COMMIT, "clean": True, "lockfileSha256": LOCKFILE_SHA256},
            "artifacts": copy.deepcopy(ARTIFACTS),
            "deployment": {
                "environmentClass": "production-like",
                "environmentId": ENVIRONMENT_ID,
                "regions": ["cn-east-1"],
                "origins": {
                    "controlPlaneBaseUrl": "https://control.example.test/v1",
                    "webBaseUrl": "https://app.example.test",
                    "adminBaseUrl": "https://admin.example.test",
                },
            },
            "migrations": {
                "tail": {
                    "path": "services/control-plane/migrations/000164_session_settlement.sql",
                    "sha256": MIGRATION["sha256"].removeprefix("sha256:"),
                },
                "count": 1,
                "files": [
                    {
                        "path": "services/control-plane/migrations/000164_session_settlement.sql",
                        "sha256": MIGRATION["sha256"].removeprefix("sha256:"),
                    }
                ],
            },
            "controls": {},
            "generatedAt": "2026-07-31T12:00:00Z",
            "assessment": "evidence-collected-not-control-passed",
        }
        self.receipts = self.build_receipts()
        self.manifest = self.root / "bundle.json"
        self.write_bundle()

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def build_receipts(self) -> dict[str, dict[str, object]]:
        candidate = {
            "candidateId": "stage6-rc1",
            "sourceCommit": COMMIT,
            "environment": "production-like",
            "environmentId": ENVIRONMENT_ID,
        }
        residency_candidate = {
            "candidateId": candidate["candidateId"],
            "sourceCommit": COMMIT,
            "lockfileSha256": LOCKFILE_SHA256,
            "environmentClass": "production-like",
            "environmentId": ENVIRONMENT_ID,
            "artifacts": copy.deepcopy(ARTIFACTS),
            "migrationTail": MIGRATION,
        }
        residency_candidate_binding = "sha256:" + hashlib.sha256(
            json.dumps(
                residency_candidate, separators=(",", ":"), sort_keys=True
            ).encode("utf-8")
        ).hexdigest()
        recovery_candidate = {
            "candidateId": candidate["candidateId"],
            "sourceCommit": COMMIT,
            "lockfileSha256": LOCKFILE_SHA256,
            "environmentClass": "production-like",
            "environmentId": ENVIRONMENT_ID,
            "regions": ["cn-east-1"],
            "origins": {
                "controlPlaneBaseUrl": "https://control.example.test/v1",
                "webBaseUrl": "https://app.example.test",
                "adminBaseUrl": "https://admin.example.test",
            },
            "artifacts": copy.deepcopy(ARTIFACTS),
            "migrationTail": MIGRATION,
        }
        recovery_candidate_binding = "sha256:" + hashlib.sha256(
            json.dumps(
                recovery_candidate, separators=(",", ":"), sort_keys=True
            ).encode("utf-8")
        ).hexdigest()
        recovery_subject = "sha256:" + "d" * 64
        recovery_approvals = {
            role: {
                "role": role,
                "approverId": f"{role}-reviewer",
                "decision": "approved-for-human-gate-review",
                "approvedAt": "2026-07-31T10:30:00Z",
                "expiresAt": "2026-08-31T10:30:00Z",
                "subjectSha256": recovery_subject,
                "evidence": {
                    "path": f"{role}-approval.json",
                    "sha256": "sha256:" + "e" * 64,
                },
            }
            for role in ("database", "kms", "operations", "security", "storage")
        }
        return {
            "billing": {
                "schemaVersion": "synara.stage6-stripe-billing-exercise-validation.v1",
                "candidate": {
                    **candidate,
                    "controlPlaneDigest": ARTIFACTS["controlPlaneImage"],
                    "webDigest": ARTIFACTS["webArtifact"],
                    "controlPlaneBaseUrl": "https://control.example.test/v1",
                    "migrationTail": MIGRATION,
                },
                "eligibleForHumanGateReview": True,
                "validatedAt": "2026-07-31T11:00:00Z",
                "assessment": "evidence-validated-not-billing-passed",
            },
            "internalCost": {
                "schemaVersion": "synara.stage6-internal-cost-evidence-validation.v1",
                "candidate": {
                    **candidate,
                    "controlPlaneBaseUrl": "https://control.example.test/v1",
                    "migrationTail": MIGRATION,
                },
                "usage": {
                    "executionCount": 2,
                    "inputTokens": 1000,
                    "outputTokens": 250,
                    "cachedInputTokens": 300,
                    "cacheCreationInputTokens": 100,
                    "providerCostReportedExecutionCount": 1,
                    "providerCostUnavailableExecutionCount": 1,
                    "actualPlatformAllocationCount": 1,
                    "estimatedPlatformAllocationCount": 1,
                },
                "eligibleForHumanGateReview": True,
                "validatedAt": "2026-07-31T11:00:00Z",
                "assessment": "evidence-validated-not-internal-cost-approved",
            },
            "capacity": {
                "schemaVersion": "synara.capacity-soak-evidence-receipt.v1",
                "releaseCommit": COMMIT,
                "environmentClass": "production-like",
                "environmentId": ENVIRONMENT_ID,
                "releaseEligibleEnvironment": True,
                "externalProbeCoverageRatio": 0.99,
                "forecastHeadroomCovered": True,
                "declaredMeasurementsWithinObjectives": True,
                "validatedAt": "2026-07-31T11:00:00Z",
                "assessment": "evidence-validated-not-capacity-passed",
            },
            "desktop": {
                "schemaVersion": "synara.stage6-desktop-native-acceptance-validation.v1",
                "candidate": {
                    **candidate,
                    "lockfileSha256": LOCKFILE_SHA256,
                    "controlPlaneBaseUrl": "https://control.example.test/v1",
                    "migrationTail": MIGRATION,
                },
                "window": {
                    "startedAt": "2026-07-31T00:00:00Z",
                    "completedAt": "2026-07-31T02:00:00Z",
                },
                "targets": [
                    {
                        "id": target,
                        "runnerReference": f"runner/{target}",
                        "hostArchitecture": DESKTOP_TARGET_ARCHITECTURES[target],
                        "executionMode": "native",
                        "nativeExecutionProved": True,
                        "artifactDigest": digest,
                        "enrollment": {field: True for field in ENROLLMENT_FIELDS},
                        "enrollmentComplete": True,
                        "enrollmentRuntimeEvidence": desktop_enrollment_runtime(target),
                        "eligible": True,
                        "evidence": {
                            **{
                                field: {
                                    "path": f"{target}-{field}.json",
                                    "sha256": ARTIFACT_SET_EVIDENCE_DIGESTS[target][field],
                                }
                                for field in (
                                    "artifactAttestation",
                                    "attestationBundle",
                                    "provenance",
                                )
                            },
                            "enrollment": {
                                "path": f"{target}-enrollment.json",
                                "sha256": ENROLLMENT_EVIDENCE_DIGESTS[target],
                            },
                        },
                    }
                    for target, digest in sorted(ARTIFACTS["desktopArtifacts"].items())
                ],
                "eligibleForHumanGateReview": True,
                "validatedAt": "2026-07-31T11:00:00Z",
                "assessment": "evidence-validated-not-desktop-ga-passed",
            },
            "incident": {
                "schemaVersion": "synara.incident-communication-exercise-evidence-receipt.v2",
                "releaseCommit": COMMIT,
                "environmentClass": "production-like",
                "environmentId": ENVIRONMENT_ID,
                "serviceOrigin": "https://app.example.test",
                "eligibleForHumanGateReview": True,
                "validatedAt": "2026-07-31T11:00:00Z",
                "assessment": "evidence-validated-not-operations-ready",
            },
            "operations": {
                "schemaVersion": "synara.stage6-operations-browser-exercise-validation.v2",
                "candidate": {
                    **candidate,
                    "artifacts": {
                        "controlPlane": ARTIFACTS["controlPlaneImage"],
                        "web": ARTIFACTS["webArtifact"],
                        "admin": ARTIFACTS["adminArtifact"],
                    },
                    "webBaseUrl": "https://app.example.test",
                    "adminBaseUrl": "https://admin.example.test",
                },
                "eligibleForHumanGateReview": True,
                "validatedAt": "2026-07-31T11:00:00Z",
                "assessment": "evidence-validated-not-operations-passed",
            },
            "penetration": {
                "schemaVersion": "synara.third-party-penetration-evidence-receipt.v1",
                "releaseCommit": COMMIT,
                "environmentClass": "production-like",
                "environmentId": ENVIRONMENT_ID,
                "assets": [
                    {"assetType": "control-plane-api", "artifactDigest": ARTIFACTS["controlPlaneImage"]},
                    {"assetType": "provider-host", "artifactDigest": ARTIFACTS["providerHostImage"]},
                    {"assetType": "web", "artifactDigest": ARTIFACTS["webArtifact"]},
                    {"assetType": "worker-runtime", "artifactDigest": ARTIFACTS["workerImage"]},
                ],
                "eligibleForHumanGateReview": True,
                "validatedAt": "2026-07-31T11:00:00Z",
                "assessment": "evidence-validated-not-penetration-passed",
            },
            "recovery": {
                "schemaVersion": "synara.recovery-drill-evidence-receipt.v2",
                "candidate": recovery_candidate,
                "candidateBindingSha256": recovery_candidate_binding,
                "restoredReleaseIdentity": {
                    "sourceCommit": COMMIT,
                    "lockfileSha256": LOCKFILE_SHA256,
                    "artifacts": copy.deepcopy(ARTIFACTS),
                    "migrationTail": MIGRATION,
                },
                "startedAt": "2026-07-31T08:00:00Z",
                "completedAt": "2026-07-31T10:00:00Z",
                "recoverySubjectSha256": recovery_subject,
                "approvals": recovery_approvals,
                "declaredMeasurementsWithinObjectives": True,
                "allRestoreCanariesPassed": True,
                "allRequiredApprovalsApproved": True,
                "releaseEligibleEnvironment": True,
                "eligibleForHumanGateReview": True,
                "verificationBoundary": {
                    "approvalContentAndSubjectValidated": True,
                    "cryptographicSignaturesVerified": False,
                    "realBackupRestoreAndApproverAuthorityVerificationRequired": True,
                },
                "validatedAt": "2026-07-31T11:00:00Z",
                "assessment": "evidence-validated-not-control-passed",
            },
            "residency": {
                "schemaVersion": "synara.data-residency-deployment-evidence-receipt.v1",
                "candidate": residency_candidate,
                "candidateBindingSha256": residency_candidate_binding,
                "promiseScope": "full-data-residency",
                "evidenceSetSha256": "sha256:" + "f" * 64,
                "policy": {
                    "statementType": "synara-data-residency-statement-v1",
                    "version": 7,
                    "digest": "sha256:" + "e" * 64,
                    "homeRegion": "cn-east-1",
                    "allowedRegions": ["cn-east-1"],
                    "enforcementState": "restricted",
                    "homeRegionAllowed": True,
                },
                "checks": {
                    "approvalsCurrentThroughReview": True,
                    "attachmentsShareExactSubject": True,
                    "environmentEligible": True,
                    "evacuationAcceptanceSatisfied": True,
                    "failoverAcceptanceSatisfied": True,
                    "fullInventoryKnownDisclosedAndAllowed": True,
                    "homeRegionAllowed": True,
                    "restrictedPolicy": True,
                    "runtimeInventoryMatchesAnnex": True,
                },
                "verificationBoundary": {
                    "strictAttachmentContentAndSubjectValidated": True,
                    "cryptographicSignaturesVerified": False,
                    "externalSignatureIdentityAndAuthorityVerificationRequired": True,
                },
                "eligibleForHumanGateReview": True,
                "validatedAt": "2026-07-31T11:00:00Z",
                "assessment": "evidence-validated-not-residency-approved",
            },
            "slo": {
                "schemaVersion": "synara.slo-window-evidence-receipt.v1",
                "releaseCommit": COMMIT,
                "environmentClass": "production-like",
                "environmentId": ENVIRONMENT_ID,
                "publicOrigin": "https://app.example.test",
                "queryRevision": COMMIT,
                "eligibleForHumanGateReview": True,
                "validatedAt": "2026-07-31T11:00:00Z",
                "assessment": "evidence-validated-not-slo-passed",
            },
            "workerSupplyChain": {
                "schemaVersion": "synara.stage6-worker-supply-chain-evidence.v1",
                "source": {"commit": COMMIT, "workerImage": ARTIFACTS["workerImage"]},
                "evidence": {
                    "releaseManifestSha256": "0" * 64,
                    "registryReportSha256": "1" * 64,
                    "admissionReportSha256": "2" * 64,
                },
                "controls": {
                    "multiArchitectureReproducibility": "pass",
                    "spdxAndSlsaAttestations": "pass",
                    "productionKmsSigningAndTransparencyLog": "pass",
                    "vulnerabilityAndSecretPolicy": "pass",
                    "signedAndNegativeAdmissionProbes": "pass",
                    "exactCleanup": "pass",
                },
                "validatedAt": "2026-07-31T11:00:00Z",
                "assessment": "evidence-validated-not-worker-supply-chain-approved",
            },
        }

    def write_json(self, name: str, payload: dict[str, object]) -> dict[str, str]:
        path = self.evidence / name
        path.write_text(json.dumps(payload, indent=2, sort_keys=True) + "\n", encoding="utf-8")
        return {"path": name, "sha256": "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest()}

    def write_bundle(self) -> None:
        release_reference = self.write_json("release-evidence.json", self.release)
        compatibility_reference = self.write_json(
            "compatibility-matrix.json", self.compatibility
        )
        worker_supply_chain = self.receipts.get("workerSupplyChain")
        if worker_supply_chain is not None:
            worker_supply_chain["evidence"]["releaseManifestSha256"] = (
                release_reference["sha256"].removeprefix("sha256:")
            )
        if self.bundle_schema.endswith(".v2"):
            expected_names = {
                "billing", "capacity", "desktop", "incident", "operations",
                "penetration", "recovery", "residency", "slo",
            }
        elif self.bundle_schema.endswith(".v3"):
            expected_names = {
                "billing", "capacity", "desktop", "incident", "operations",
                "penetration", "recovery", "residency", "slo", "workerSupplyChain",
            }
        else:
            expected_names = {
                "internalCost", "capacity", "desktop", "incident", "operations",
                "penetration", "recovery", "residency", "slo", "workerSupplyChain",
            }
        selected_receipts = {
            name: payload for name, payload in self.receipts.items() if name in expected_names
        }
        receipt_references = {
            name: self.write_json(f"{name}-receipt.json", payload)
            for name, payload in selected_receipts.items()
        }
        manifest = {
            "schemaVersion": self.bundle_schema,
            "releaseEvidence": release_reference,
            "receipts": receipt_references,
        }
        if self.bundle_schema.endswith(".v5"):
            manifest["compatibilityMatrix"] = compatibility_reference
        self.manifest.write_text(
            json.dumps(
                manifest,
                indent=2,
                sort_keys=True,
            )
            + "\n",
            encoding="utf-8",
        )

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
            "--validated-at",
            "2026-08-01T00:00:00Z",
        ]

    def run_validator(self) -> subprocess.CompletedProcess[str]:
        self.write_bundle()
        return subprocess.run(self.command(), check=False, capture_output=True, text=True)

    def receipt(self) -> dict[str, object]:
        return json.loads((self.root / "receipt.json").read_text(encoding="utf-8"))

    def test_binds_all_receipts_to_one_candidate_without_claiming_ga(self) -> None:
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertTrue(receipt["candidateConsistencyValidated"])
        self.assertTrue(receipt["eligibleForCandidateEvidenceReview"])
        self.assertEqual(
            receipt["schemaVersion"],
            "synara.stage6-candidate-evidence-bundle-validation.v5",
        )
        self.assertEqual(receipt["requiredReceiptCount"], 10)
        self.assertEqual(
            receipt["compatibilityMatrix"]["sha256"],
            self.write_json("compatibility-matrix.json", self.compatibility)["sha256"],
        )
        self.assertEqual(receipt["compatibilityMatrix"]["migrationTail"], MIGRATION)
        self.assertEqual(receipt["assessment"], "evidence-consistent-not-ga-approved")
        self.assertEqual(receipt["candidate"]["sourceCommit"], COMMIT)
        self.assertEqual(
            receipt["candidate"]["desktopArtifactSetSha256"],
            desktop_artifact_set_sha256(),
        )
        self.assertEqual(
            receipt["receipts"]["desktop"]["desktopArtifactSetSha256"],
            desktop_artifact_set_sha256(),
        )
        self.assertEqual(
            receipt["receipts"]["desktop"][
                "desktopEnrollmentEvidenceSetSha256"
            ],
            desktop_enrollment_evidence_set_sha256(),
        )
        self.assertEqual(
            receipt["receipts"]["residency"]["evidenceSetSha256"],
            "sha256:" + "f" * 64,
        )
        self.assertFalse(
            receipt["receipts"]["residency"]["cryptographicSignaturesVerified"]
        )
        self.assertEqual(
            receipt["receipts"]["recovery"]["recoverySubjectSha256"],
            "sha256:" + "d" * 64,
        )
        self.assertEqual(receipt["manifest"]["path"], "bundle.json")
        self.assertTrue((self.root / "receipt.json.sha256").is_file())
        self.assertLess((self.root / "receipt.json").stat().st_size, 32 * 1024)
        self.assertEqual(stat.S_IMODE((self.root / "receipt.json").stat().st_mode), 0o600)
        self.assertEqual(
            stat.S_IMODE((self.root / "receipt.json.sha256").stat().st_mode), 0o600
        )

    def test_rejects_existing_validation_receipt_without_modifying_it(self) -> None:
        receipt = self.root / "receipt.json"
        sidecar = self.root / "receipt.json.sha256"
        receipt.write_text("retained receipt\n", encoding="utf-8")
        sidecar.write_text("retained sidecar\n", encoding="utf-8")
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("must not already exist", result.stderr)
        self.assertEqual(receipt.read_text(encoding="utf-8"), "retained receipt\n")
        self.assertEqual(sidecar.read_text(encoding="utf-8"), "retained sidecar\n")

    def test_rejects_old_or_drifted_desktop_enrollment_projection(self) -> None:
        desktop = self.receipts["desktop"]
        desktop["targets"][0].pop("enrollmentRuntimeEvidence")
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("enrollmentRuntimeEvidence must be an object", result.stderr)

        self.receipts = self.build_receipts()
        desktop = self.receipts["desktop"]
        desktop["targets"][0]["enrollmentRuntimeEvidence"]["requestCounts"][
            "cloudWhileLocal"
        ] = 1
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("cloudWhileLocal must be zero", result.stderr)

    def test_keeps_v2_offline_validation_compatible_without_worker_supply_chain(self) -> None:
        self.bundle_schema = "synara.stage6-candidate-evidence-bundle.v2"
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertEqual(
            receipt["schemaVersion"],
            "synara.stage6-candidate-evidence-bundle-validation.v2",
        )
        self.assertEqual(receipt["requiredReceiptCount"], 9)
        self.assertNotIn("workerSupplyChain", receipt["receipts"])

    def test_v3_requires_worker_supply_chain_and_exact_release_manifest_binding(self) -> None:
        self.bundle_schema = "synara.stage6-candidate-evidence-bundle.v3"
        self.receipts.pop("workerSupplyChain")
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("receipts fields", result.stderr)

        self.receipts = self.build_receipts()
        self.write_bundle()
        worker_path = self.evidence / "workerSupplyChain-receipt.json"
        worker = json.loads(worker_path.read_text(encoding="utf-8"))
        worker["evidence"]["releaseManifestSha256"] = "f" * 64
        worker_path.write_text(json.dumps(worker, indent=2, sort_keys=True) + "\n", encoding="utf-8")
        bundle = json.loads(self.manifest.read_text(encoding="utf-8"))
        bundle["receipts"]["workerSupplyChain"]["sha256"] = (
            "sha256:" + hashlib.sha256(worker_path.read_bytes()).hexdigest()
        )
        self.manifest.write_text(json.dumps(bundle, indent=2, sort_keys=True) + "\n", encoding="utf-8")
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("releaseManifestSha256", result.stderr)

    def test_v3_rejects_worker_commit_digest_or_control_drift(self) -> None:
        mutations = (
            lambda receipt: receipt["source"].update({"commit": "f" * 40}),
            lambda receipt: receipt["source"].update({"workerImage": "sha256:" + "f" * 64}),
            lambda receipt: receipt["evidence"].update({"registryReportSha256": "0" * 64}),
            lambda receipt: receipt["controls"].update({"exactCleanup": "fail"}),
        )
        for mutation in mutations:
            with self.subTest(mutation=mutation):
                self.receipts = self.build_receipts()
                mutation(self.receipts["workerSupplyChain"])
                result = self.run_validator()
                self.assertEqual(result.returncode, 2)

    def test_rejects_commit_environment_and_origin_drift(self) -> None:
        self.receipts["incident"]["releaseCommit"] = "d" * 40
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("incident.releaseCommit", result.stderr)

        self.receipts = self.build_receipts()
        self.receipts["capacity"]["environmentId"] = "production-like/other"
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("capacity.environmentId", result.stderr)

        self.receipts = self.build_receipts()
        self.receipts["operations"]["candidate"]["adminBaseUrl"] = "https://other.example.test"
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("operations.adminBaseUrl", result.stderr)

    def test_rejects_artifact_migration_lockfile_and_query_drift(self) -> None:
        self.receipts["penetration"]["assets"][0]["artifactDigest"] = "sha256:" + "f" * 64
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("penetration artifact set", result.stderr)

        self.receipts = self.build_receipts()
        self.receipts["desktop"]["candidate"]["migrationTail"] = {
            **MIGRATION,
            "sha256": "sha256:" + "f" * 64,
        }
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("desktop.migrationTail", result.stderr)

        self.receipts = self.build_receipts()
        self.receipts["desktop"]["candidate"]["lockfileSha256"] = "f" * 64
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("desktop.lockfileSha256", result.stderr)

        self.receipts = self.build_receipts()
        self.receipts["slo"]["queryRevision"] = "f" * 40
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("slo.queryRevision", result.stderr)

        self.receipts = self.build_receipts()
        self.receipts["residency"]["policy"]["allowedRegions"] = ["cn-east-2"]
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("residency.allowedRegions", result.stderr)

        self.receipts = self.build_receipts()
        self.receipts["residency"]["evidenceSetSha256"] = "sha256:" + "0" * 63
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("residency.evidenceSetSha256", result.stderr)

        self.receipts = self.build_receipts()
        self.receipts["residency"]["verificationBoundary"][
            "cryptographicSignaturesVerified"
        ] = True
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("residency.verificationBoundary", result.stderr)

    def test_records_an_ineligible_receipt_without_relabeling_it_passed(self) -> None:
        self.receipts["internalCost"]["eligibleForHumanGateReview"] = False
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertFalse(receipt["allRequiredReceiptsReadyForCandidateReview"])
        self.assertFalse(receipt["eligibleForCandidateEvidenceReview"])
        self.assertEqual(receipt["assessment"], "evidence-consistent-not-ga-approved")

    def test_rejects_recovery_candidate_time_and_approval_drift(self) -> None:
        self.receipts["recovery"]["candidate"]["lockfileSha256"] = "f" * 64
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("recovery.candidate", result.stderr)

        self.receipts = self.build_receipts()
        self.receipts["recovery"]["restoredReleaseIdentity"]["artifacts"][
            "workerImage"
        ] = "sha256:" + "f" * 64
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("recovery.restoredReleaseIdentity", result.stderr)

        self.receipts = self.build_receipts()
        self.receipts["recovery"]["completedAt"] = "2026-07-31T12:00:00Z"
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("recovery drill and validation timestamps", result.stderr)

        self.receipts = self.build_receipts()
        self.receipts["recovery"]["approvals"]["security"]["approverId"] = (
            self.receipts["recovery"]["approvals"]["operations"]["approverId"]
        )
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("role-separated", result.stderr)

    def test_rejects_dirty_release_evidence_and_tampered_reference(self) -> None:
        self.release["source"]["clean"] = False
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("exact clean source commit", result.stderr)

        self.release["source"]["clean"] = True
        self.write_bundle()
        (self.evidence / "internalCost-receipt.json").write_text("{}\n", encoding="utf-8")
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("sha256 does not match", result.stderr)

    def test_rejects_reused_receipt_file(self) -> None:
        self.write_bundle()
        bundle = json.loads(self.manifest.read_text(encoding="utf-8"))
        bundle["receipts"]["internalCost"] = bundle["receipts"]["capacity"]
        self.manifest.write_text(json.dumps(bundle, indent=2) + "\n", encoding="utf-8")
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("duplicates another bundle file", result.stderr)

    def test_v5_rejects_compatibility_matrix_source_or_migration_drift(self) -> None:
        self.compatibility["components"]["workerProtocol"]["maximum"] = 99
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("compatibilityMatrix is not current with source", result.stderr)

        self.compatibility = json.loads(
            (SCRIPT.parents[2] / "docs/release-matrices/stage-6-compatibility-v1.json").read_text(
                encoding="utf-8"
            )
        )
        self.release["migrations"]["tail"]["sha256"] = "f" * 64
        self.release["migrations"]["files"][-1]["sha256"] = "f" * 64
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("compatibilityMatrix migration tail", result.stderr)


if __name__ == "__main__":
    unittest.main()
