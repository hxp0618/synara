from __future__ import annotations

import hashlib
import importlib.util
import json
import pathlib
import stat
import subprocess
import sys
import tempfile
import unittest


SCRIPT = pathlib.Path(__file__).with_name("validate_desktop_native_acceptance.py")
SPEC = importlib.util.spec_from_file_location("validate_desktop_native_acceptance", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class ValidateDesktopNativeAcceptanceTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temporary.name)
        self.evidence = self.root / "evidence"
        self.evidence.mkdir()
        self.manifest = self.root / "manifest.json"
        self.output = self.root / "receipt.json"
        self.version = "0.6.4-rc.1"
        self.source_commit = "a" * 40
        self.lockfile_sha256 = "b" * 64
        self.provenance_paths: dict[str, pathlib.Path] = {}
        self.enrollment_paths: dict[str, pathlib.Path] = {}
        self.payload = {
            "schemaVersion": MODULE.SCHEMA_VERSION,
            "candidate": {
                "candidateId": "stage6-rc1",
                "sourceCommit": self.source_commit,
                "sourceTag": f"v{self.version}",
                "lockfileSha256": self.lockfile_sha256,
                "version": self.version,
                "environment": "production-like",
                "environmentId": "production-like/stage6-rc1",
                "controlPlaneBaseUrl": "https://control.example.test/v1",
                "migrationTail": {
                    "name": "000108_desktop_enrollment.sql",
                    "sha256": "sha256:" + "c" * 64,
                },
            },
            "startedAt": "2026-07-31T00:00:00Z",
            "completedAt": "2026-07-31T02:00:00Z",
            "targets": [self.target(target_id) for target_id in sorted(MODULE.TARGETS)],
            "approvals": [
                {
                    "role": role,
                    "subjectReference": f"approver/{role}",
                    "approved": True,
                    "approvedAt": "2026-07-31T03:00:00Z",
                    "evidence": self.reference(f"approval-{role}"),
                }
                for role in sorted(MODULE.APPROVAL_ROLES)
            ],
        }

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def reference(self, name: str, content: str | None = None) -> dict[str, str]:
        path = self.evidence / f"{name}.json"
        encoded = content or (json.dumps({"evidence": name}, sort_keys=True) + "\n")
        path.write_text(encoded, encoding="utf-8")
        return {
            "path": path.relative_to(self.evidence).as_posix(),
            "sha256": "sha256:" + hashlib.sha256(encoded.encode("utf-8")).hexdigest(),
        }

    def provenance(self, target_id: str, artifact_hex: str) -> dict[str, str]:
        expected = MODULE.TARGETS[target_id]
        artifact_name = f"Synara-{self.version}-{expected['arch']}{expected['artifactSuffix']}"
        if expected["platform"] == "mac":
            signing = {
                "status": "verified",
                "scheme": "apple-developer-id",
                "identity": {
                    "teamId": "TEAM123456",
                    "authorities": [
                        "Developer ID Application: Synara Example, Inc. (TEAM123456)"
                    ],
                    "appBundle": "Synara.app",
                    "diskImage": artifact_name,
                },
                "checks": [
                    "codesign --verify app",
                    "spctl --assess app",
                    "stapler validate app",
                    "codesign --verify dmg",
                    "spctl --assess dmg",
                    "stapler validate dmg",
                ],
            }
        elif expected["platform"] == "win":
            signing = {
                "status": "verified",
                "scheme": "windows-authenticode",
                "identity": [
                    {
                        "fileName": artifact_name,
                        "subject": "CN=Synara Example, O=Synara Example Inc, C=US",
                        "publisher": "Synara Example",
                        "thumbprint": "A1B2C3D4",
                        "timestampSubject": "CN=Trusted Timestamp Authority",
                        "timestampThumbprint": "E5F6A7B8",
                    }
                ],
                "checks": ["Get-AuthenticodeSignature", "trusted timestamp present"],
            }
        else:
            signing = {
                "status": "not-applicable",
                "scheme": "none",
                "identity": None,
                "checks": ["AppImage payload present"],
            }
        manifest = {
            "schemaVersion": 1,
            "publication": True,
            "platform": expected["platform"],
            "arch": expected["arch"],
            "target": expected["target"],
            "version": self.version,
            "source": {
                "commit": self.source_commit,
                "tag": f"v{self.version}",
                "lockfileSha256": self.lockfile_sha256,
            },
            "signing": signing,
            "artifacts": [
                {"fileName": artifact_name, "size": 123456, "sha256": artifact_hex}
            ],
        }
        path = self.evidence / f"{target_id}-provenance.json"
        encoded = json.dumps(manifest, indent=2, sort_keys=True) + "\n"
        path.write_text(encoded, encoding="utf-8")
        self.provenance_paths[target_id] = path
        return {
            "path": path.relative_to(self.evidence).as_posix(),
            "sha256": "sha256:" + hashlib.sha256(encoded.encode("utf-8")).hexdigest(),
        }

    def enrollment_evidence(self, target_id: str) -> dict[str, str]:
        expected = MODULE.TARGETS[target_id]
        observed_at = [
            "2026-07-31T00:10:00Z",
            "2026-07-31T00:20:00Z",
            "2026-07-31T00:30:00Z",
            "2026-07-31T00:40:00Z",
            "2026-07-31T00:50:00Z",
            "2026-07-31T01:00:00Z",
        ]
        payload = {
            "schemaVersion": MODULE.ENROLLMENT_RUNTIME_EVIDENCE_SCHEMA,
            "candidate": {
                "candidateId": "stage6-rc1",
                "sourceCommit": self.source_commit,
                "environmentId": "production-like/stage6-rc1",
                "controlPlaneBaseUrl": "https://control.example.test/v1",
            },
            "target": {
                "id": target_id,
                "runnerReference": f"runner/{target_id}",
                "hostArchitecture": expected["arch"],
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
                    MODULE.MODE_TRANSITION_SPECS
                )
            ],
            "requestCounts": {
                "backendBeforeFirstChoice": 0,
                "cloudBeforeFirstChoice": 0,
                "cloudWhileLocal": 0,
            },
            "credentialContinuity": {
                "beforeLocalSwitchSha256": "sha256:" + "d" * 64,
                "afterCloudResumeSha256": "sha256:" + "d" * 64,
            },
            "results": {field: True for field in MODULE.ENROLLMENT_FIELDS},
        }
        path = self.evidence / f"{target_id}-enrollment.json"
        encoded = json.dumps(payload, indent=2, sort_keys=True) + "\n"
        path.write_text(encoded, encoding="utf-8")
        self.enrollment_paths[target_id] = path
        return {
            "path": path.relative_to(self.evidence).as_posix(),
            "sha256": "sha256:" + hashlib.sha256(encoded.encode("utf-8")).hexdigest(),
        }

    def target(self, target_id: str) -> dict[str, object]:
        expected = MODULE.TARGETS[target_id]
        artifact_hex = hashlib.sha256(target_id.encode("utf-8")).hexdigest()
        artifact_name = f"Synara-{self.version}-{expected['arch']}{expected['artifactSuffix']}"
        attestation_verification = json.dumps(
            [
                {
                    "verificationResult": {
                        "signature": {"certificate": {"issuer": "sigstore-public-good"}},
                        "verifiedTimestamps": [{"type": "transparency-log"}],
                        "statement": {
                            "predicateType": "https://slsa.dev/provenance/v1",
                            "subject": [
                                {
                                    "name": f"release-publish/{artifact_name}",
                                    "digest": {"sha256": artifact_hex},
                                }
                            ],
                        },
                    }
                }
            ],
            indent=2,
            sort_keys=True,
        ) + "\n"
        return {
            "id": target_id,
            "status": "passed",
            "runnerReference": f"runner/{target_id}",
            "hostOsVersion": f"{target_id} native host 2026.07",
            "hostArchitecture": expected["arch"],
            "executionMode": "native",
            "artifactDigest": "sha256:" + artifact_hex,
            "compatibilityWarningAbsent": True,
            "protocol": {field: True for field in MODULE.PROTOCOL_FIELDS},
            "credentialStore": {
                "backend": expected["credentialBackend"],
                "available": True,
                "encryptedAtRest": True,
                "plaintextScanPassed": True,
                "missingStorePromptAbsent": True,
            },
            "enrollment": {field: True for field in MODULE.ENROLLMENT_FIELDS},
            "secretScanPassed": True,
            "evidence": {
                "provenance": self.provenance(target_id, artifact_hex),
                "attestationBundle": self.reference(f"{target_id}-attestation-bundle"),
                "artifactAttestation": self.reference(
                    f"{target_id}-attestation-verification", attestation_verification
                ),
                "installation": self.reference(f"{target_id}-installation"),
                "protocol": self.reference(f"{target_id}-protocol"),
                "credentialStore": self.reference(f"{target_id}-credential-store"),
                "enrollment": self.enrollment_evidence(target_id),
                "secretScan": self.reference(f"{target_id}-secret-scan"),
            },
        }

    def write_manifest(self) -> None:
        self.manifest.write_text(json.dumps(self.payload, indent=2) + "\n", encoding="utf-8")

    def run_validator(self) -> subprocess.CompletedProcess[str]:
        self.write_manifest()
        return subprocess.run(
            [
                sys.executable,
                str(SCRIPT),
                "--manifest",
                str(self.manifest),
                "--evidence-root",
                str(self.evidence),
                "--output",
                str(self.output),
                "--validated-at",
                "2026-07-31T04:00:00Z",
            ],
            check=False,
            capture_output=True,
            text=True,
        )

    def receipt(self) -> dict[str, object]:
        return json.loads(self.output.read_text(encoding="utf-8"))

    def refresh_provenance_reference(self, target_id: str) -> None:
        path = self.provenance_paths[target_id]
        digest = "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest()
        target = next(item for item in self.payload["targets"] if item["id"] == target_id)
        target["evidence"]["provenance"] = {
            "path": path.relative_to(self.evidence).as_posix(),
            "sha256": digest,
        }

    def refresh_enrollment_reference(self, target_id: str) -> None:
        path = self.enrollment_paths[target_id]
        digest = "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest()
        target = next(item for item in self.payload["targets"] if item["id"] == target_id)
        target["evidence"]["enrollment"] = {
            "path": path.relative_to(self.evidence).as_posix(),
            "sha256": digest,
        }

    def test_complete_native_candidate_is_review_eligible_but_not_ga_approved(self) -> None:
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertTrue(receipt["eligibleForHumanGateReview"])
        self.assertEqual(receipt["targetStatusCounts"]["passed"], 4)
        self.assertTrue(all(target["nativeExecutionProved"] for target in receipt["targets"]))
        self.assertTrue(
            all(
                target["enrollmentRuntimeEvidence"]["credentialContinuityMatched"]
                for target in receipt["targets"]
            )
        )
        self.assertNotIn("d" * 64, json.dumps(receipt, sort_keys=True))
        self.assertEqual(receipt["assessment"], MODULE.ASSESSMENT)
        self.assertTrue(self.output.with_suffix(".json.sha256").is_file())
        self.assertEqual(stat.S_IMODE(self.output.stat().st_mode), 0o600)
        self.assertEqual(
            stat.S_IMODE(self.output.with_suffix(".json.sha256").stat().st_mode), 0o600
        )

    def test_rosetta_or_missing_credential_boundary_remains_ineligible(self) -> None:
        target = next(item for item in self.payload["targets"] if item["id"] == "macos-x64")
        target["hostArchitecture"] = "arm64"
        target["executionMode"] = "rosetta"
        target["credentialStore"]["missingStorePromptAbsent"] = False
        path = self.enrollment_paths["macos-x64"]
        evidence = json.loads(path.read_text(encoding="utf-8"))
        evidence["target"]["hostArchitecture"] = "arm64"
        evidence["target"]["executionMode"] = "rosetta"
        path.write_text(
            json.dumps(evidence, indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )
        self.refresh_enrollment_reference("macos-x64")
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertFalse(receipt["eligibleForHumanGateReview"])
        projected = next(item for item in receipt["targets"] if item["id"] == "macos-x64")
        self.assertFalse(projected["nativeExecutionProved"])
        self.assertFalse(projected["credentialStoreComplete"])

    def test_unsigned_build_only_provenance_is_preserved_as_ineligible(self) -> None:
        target_id = "macos-arm64"
        path = self.provenance_paths[target_id]
        provenance = json.loads(path.read_text(encoding="utf-8"))
        provenance["publication"] = False
        provenance["signing"] = {
            "status": "unsigned-build-only",
            "scheme": "none",
            "identity": None,
            "checks": ["unsigned build-only artifact"],
        }
        path.write_text(json.dumps(provenance, indent=2, sort_keys=True) + "\n", encoding="utf-8")
        self.refresh_provenance_reference(target_id)
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertFalse(receipt["eligibleForHumanGateReview"])
        projected = next(item for item in receipt["targets"] if item["id"] == target_id)
        self.assertFalse(projected["provenance"]["provenanceEligible"])

    def test_failed_lifecycle_or_unapproved_review_remains_ineligible(self) -> None:
        self.payload["targets"][0]["status"] = "failed"
        target_id = self.payload["targets"][0]["id"]
        self.payload["targets"][0]["enrollment"]["replayDenied"] = False
        path = self.enrollment_paths[target_id]
        evidence = json.loads(path.read_text(encoding="utf-8"))
        evidence["results"]["replayDenied"] = False
        path.write_text(
            json.dumps(evidence, indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )
        self.refresh_enrollment_reference(target_id)
        self.payload["approvals"][0]["approved"] = False
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertFalse(receipt["eligibleForHumanGateReview"])
        self.assertEqual(receipt["targetStatusCounts"]["failed"], 1)
        self.assertFalse(receipt["approvalsComplete"])

    def test_failed_explicit_mode_restore_or_switch_remains_ineligible(self) -> None:
        target = self.payload["targets"][0]
        enrollment = target["enrollment"]
        enrollment["firstLaunchChoiceRequired"] = False
        enrollment["localModeNoCloudTrafficPassed"] = False
        enrollment["cloudCredentialRetainedAcrossLocalSwitch"] = False
        target_id = target["id"]
        path = self.enrollment_paths[target_id]
        evidence = json.loads(path.read_text(encoding="utf-8"))
        evidence["requestCounts"]["cloudBeforeFirstChoice"] = 1
        evidence["requestCounts"]["cloudWhileLocal"] = 1
        evidence["credentialContinuity"]["afterCloudResumeSha256"] = (
            "sha256:" + "e" * 64
        )
        evidence["results"]["firstLaunchChoiceRequired"] = False
        evidence["results"]["localModeNoCloudTrafficPassed"] = False
        evidence["results"]["cloudCredentialRetainedAcrossLocalSwitch"] = False
        path.write_text(
            json.dumps(evidence, indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )
        self.refresh_enrollment_reference(target_id)
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = self.receipt()
        self.assertFalse(receipt["eligibleForHumanGateReview"])
        projected = next(item for item in receipt["targets"] if item["id"] == target["id"])
        self.assertFalse(projected["enrollmentComplete"])

    def test_rejects_enrollment_result_drift_from_manifest(self) -> None:
        self.payload["targets"][0]["enrollment"]["localModeRestorePassed"] = False
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("results do not match manifest assertions", result.stderr)

    def test_rejects_opaque_enrollment_evidence_even_with_matching_hash(self) -> None:
        target = self.payload["targets"][0]
        target["evidence"]["enrollment"] = self.reference("opaque-enrollment")
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("enrollment evidence fields do not match", result.stderr)

    def test_rejects_enrollment_candidate_drift(self) -> None:
        target_id = self.payload["targets"][0]["id"]
        path = self.enrollment_paths[target_id]
        evidence = json.loads(path.read_text(encoding="utf-8"))
        evidence["candidate"]["sourceCommit"] = "f" * 40
        path.write_text(
            json.dumps(evidence, indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )
        self.refresh_enrollment_reference(target_id)
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("candidate does not match manifest", result.stderr)

    def test_rejects_enrollment_evidence_outside_exercise_window(self) -> None:
        target_id = self.payload["targets"][0]["id"]
        path = self.enrollment_paths[target_id]
        evidence = json.loads(path.read_text(encoding="utf-8"))
        evidence["startedAt"] = "2026-07-30T23:59:00Z"
        path.write_text(
            json.dumps(evidence, indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )
        self.refresh_enrollment_reference(target_id)
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("inside the exercise window", result.stderr)

    def test_rejects_missing_target_or_provenance_identity_mismatch(self) -> None:
        removed = self.payload["targets"].pop()
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("all four native targets", result.stderr)

        self.payload["targets"].append(removed)
        target_id = "windows-x64"
        path = self.provenance_paths[target_id]
        provenance = json.loads(path.read_text(encoding="utf-8"))
        provenance["source"]["commit"] = "d" * 40
        path.write_text(json.dumps(provenance, indent=2, sort_keys=True) + "\n", encoding="utf-8")
        self.refresh_provenance_reference(target_id)
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("provenance source does not match candidate", result.stderr)

    def test_rejects_tampered_reused_or_symlinked_evidence(self) -> None:
        target = self.payload["targets"][0]
        target["evidence"]["installation"] = target["evidence"]["protocol"]
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("duplicates another evidence file", result.stderr)

        target["evidence"]["installation"] = self.reference("replacement-installation")
        path = self.evidence / target["evidence"]["protocol"]["path"]
        original = path.read_text(encoding="utf-8")
        path.write_text("tampered\n", encoding="utf-8")
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("sha256 does not match", result.stderr)

        path.write_text(original, encoding="utf-8")
        link = self.evidence / "linked-protocol.json"
        link.symlink_to(path.name)
        target["evidence"]["protocol"] = {
            "path": link.name,
            "sha256": "sha256:" + hashlib.sha256(path.read_bytes()).hexdigest(),
        }
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("must not traverse a symlink", result.stderr)

    def test_rejects_attestation_for_a_different_installer_digest(self) -> None:
        target = next(item for item in self.payload["targets"] if item["id"] == "linux-x64")
        reference = target["evidence"]["artifactAttestation"]
        path = self.evidence / reference["path"]
        verification = json.loads(path.read_text(encoding="utf-8"))
        verification[0]["verificationResult"]["statement"]["subject"][0]["digest"][
            "sha256"
        ] = "f" * 64
        encoded = json.dumps(verification, indent=2, sort_keys=True) + "\n"
        path.write_text(encoded, encoding="utf-8")
        reference["sha256"] = "sha256:" + hashlib.sha256(encoded.encode("utf-8")).hexdigest()
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("does not verify the primary installer subject", result.stderr)

    def test_rejects_duplicate_manifest_or_structured_evidence_fields(self) -> None:
        encoded = json.dumps(self.payload, indent=2)
        duplicate = encoded.replace(
            f'"schemaVersion": "{MODULE.SCHEMA_VERSION}",',
            f'"schemaVersion": "{MODULE.SCHEMA_VERSION}",\n'
            f'  "schemaVersion": "{MODULE.SCHEMA_VERSION}",',
            1,
        )
        self.manifest.write_text(duplicate + "\n", encoding="utf-8")
        result = subprocess.run(
            [
                sys.executable,
                str(SCRIPT),
                "--manifest",
                str(self.manifest),
                "--evidence-root",
                str(self.evidence),
                "--output",
                str(self.output),
            ],
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, 2)
        self.assertIn("duplicate field", result.stderr)

        target_id = self.payload["targets"][0]["id"]
        path = self.provenance_paths[target_id]
        provenance = path.read_text(encoding="utf-8").replace(
            '"publication": true,',
            '"publication": true,\n  "publication": true,',
            1,
        )
        path.write_text(provenance, encoding="utf-8")
        self.refresh_provenance_reference(target_id)
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("duplicate field", result.stderr)

    def test_rejects_secret_evidence_without_echoing_secret(self) -> None:
        secret = "Authorization: Bearer desktop-native-validator-secret"
        target = self.payload["targets"][0]
        path = self.evidence / target["evidence"]["installation"]["path"]
        path.write_text(secret + "\n", encoding="utf-8")
        target["evidence"]["installation"]["sha256"] = (
            "sha256:" + hashlib.sha256((secret + "\n").encode("utf-8")).hexdigest()
        )
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("prohibited bearer credential material", result.stderr)
        self.assertNotIn(secret, result.stderr)


if __name__ == "__main__":
    unittest.main()
