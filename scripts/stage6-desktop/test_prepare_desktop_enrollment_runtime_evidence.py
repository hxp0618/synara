from __future__ import annotations

import hashlib
import json
import pathlib
import stat
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

import prepare_desktop_enrollment_runtime_evidence as MODULE


SCRIPT = pathlib.Path(__file__).with_name(
    "prepare_desktop_enrollment_runtime_evidence.py"
)
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
MODE_TRANSITIONS = (
    ("first-use-local-choice", "unselected", "local"),
    ("local-restart-restore", "local", "local"),
    ("local-to-cloud-switch", "local", "cloud"),
    ("cloud-restart-restore", "cloud", "cloud"),
    ("cloud-to-local-switch", "cloud", "local"),
    ("local-to-cloud-resume", "local", "cloud"),
)


class PrepareDesktopEnrollmentRuntimeEvidenceTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temporary.name)
        self.capture_path = self.root / "capture.json"
        self.output = self.root / "enrollment-evidence.json"
        observed_at = [
            "2026-08-02T00:10:00Z",
            "2026-08-02T00:20:00Z",
            "2026-08-02T00:30:00Z",
            "2026-08-02T00:40:00Z",
            "2026-08-02T00:50:00Z",
            "2026-08-02T01:00:00Z",
        ]
        self.capture = {
            "schemaVersion": "synara.stage6-desktop-enrollment-runtime-capture.v1",
            "candidate": {
                "candidateId": "stage6-rc1",
                "sourceCommit": "a" * 40,
                "environmentId": "production-like/stage6-rc1",
                "controlPlaneBaseUrl": "https://CONTROL.example.test:443/v1/",
            },
            "target": {
                "id": "macos-arm64",
                "runnerReference": "github/run/123/job/456",
                "hostArchitecture": "arm64",
                "executionMode": "native",
            },
            "exerciseWindow": {
                "startedAt": "2026-08-02T00:00:00Z",
                "completedAt": "2026-08-02T02:00:00Z",
            },
            "startedAt": "2026-08-02T00:05:00Z",
            "completedAt": "2026-08-02T01:05:00Z",
            "modeTransitions": [
                {
                    "id": transition_id,
                    "from": from_mode,
                    "to": to_mode,
                    "observedAt": observed_at[index],
                    "passed": True,
                }
                for index, (transition_id, from_mode, to_mode) in enumerate(
                    MODE_TRANSITIONS
                )
            ],
            "requestCounts": {
                "backendBeforeFirstChoice": 0,
                "cloudBeforeFirstChoice": 0,
                "cloudWhileLocal": 0,
            },
            "credentialContinuity": {
                "beforeLocalSwitchSha256": "sha256:" + "b" * 64,
                "afterCloudResumeSha256": "sha256:" + "b" * 64,
            },
            "results": {field: True for field in ENROLLMENT_FIELDS},
        }

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def write_capture(self) -> None:
        self.capture_path.write_text(
            json.dumps(self.capture, indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )

    def run_preparer(self) -> subprocess.CompletedProcess[str]:
        self.write_capture()
        return self.run_existing_capture()

    def run_existing_capture(self) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            [
                sys.executable,
                str(SCRIPT),
                "--capture",
                str(self.capture_path),
                "--output",
                str(self.output),
            ],
            check=False,
            capture_output=True,
            text=True,
        )

    def test_prepares_canonical_private_immutable_evidence(self) -> None:
        result = self.run_preparer()
        self.assertEqual(result.returncode, 0, result.stderr)
        summary = json.loads(result.stdout)
        evidence = json.loads(self.output.read_text(encoding="utf-8"))
        self.assertEqual(
            evidence["schemaVersion"],
            "synara.stage6-desktop-enrollment-runtime-evidence.v1",
        )
        self.assertEqual(
            evidence["candidate"]["controlPlaneBaseUrl"],
            "https://control.example.test/v1",
        )
        self.assertNotIn("exerciseWindow", evidence)
        self.assertEqual(stat.S_IMODE(self.output.stat().st_mode), 0o600)
        sidecar = self.output.with_suffix(".json.sha256")
        self.assertEqual(stat.S_IMODE(sidecar.stat().st_mode), 0o600)
        digest = hashlib.sha256(self.output.read_bytes()).hexdigest()
        self.assertEqual(summary["sha256"], "sha256:" + digest)
        self.assertEqual(sidecar.read_text(encoding="utf-8"), f"{digest}  {self.output.name}\n")
        self.assertNotIn("b" * 64, result.stdout)

        original = self.output.read_bytes()
        repeated = self.run_preparer()
        self.assertEqual(repeated.returncode, 2)
        self.assertEqual(self.output.read_bytes(), original)

    def test_rejects_observation_result_drift_without_partial_publication(self) -> None:
        self.capture["requestCounts"]["cloudWhileLocal"] = 1
        result = self.run_preparer()
        self.assertEqual(result.returncode, 2)
        self.assertIn("localModeNoCloudTrafficPassed", result.stderr)
        self.assertFalse(self.output.exists())
        self.assertFalse(self.output.with_suffix(".json.sha256").exists())

    def test_rejects_symlink_capture_and_sidecar_collision(self) -> None:
        real_capture = self.root / "real-capture.json"
        real_capture.write_text(
            json.dumps(self.capture, indent=2, sort_keys=True) + "\n", encoding="utf-8"
        )
        self.capture_path.symlink_to(real_capture.name)
        result = subprocess.run(
            [
                sys.executable,
                str(SCRIPT),
                "--capture",
                str(self.capture_path),
                "--output",
                str(self.output),
            ],
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, 2)
        self.assertIn("non-symlink", result.stderr)
        self.assertFalse(self.output.exists())

        self.capture_path.unlink()
        sidecar = self.output.with_suffix(".json.sha256")
        sidecar.write_text("retain\n", encoding="utf-8")
        result = self.run_preparer()
        self.assertEqual(result.returncode, 2)
        self.assertFalse(self.output.exists())
        self.assertEqual(sidecar.read_text(encoding="utf-8"), "retain\n")

    def test_rejects_duplicate_capture_fields_and_secret_material(self) -> None:
        encoded = json.dumps(self.capture, indent=2, sort_keys=True)
        duplicate = encoded.replace(
            "{",
            '{\n  "schemaVersion": "synara.stage6-desktop-enrollment-runtime-capture.v1",',
            1,
        )
        self.capture_path.write_text(duplicate + "\n", encoding="utf-8")
        result = self.run_existing_capture()
        self.assertEqual(result.returncode, 2)
        self.assertIn("duplicate field", result.stderr)
        self.assertFalse(self.output.exists())

        secret = "Authorization: Bearer desktop-enrollment-capture-secret"
        self.capture_path.write_text(secret + "\n", encoding="utf-8")
        result = self.run_existing_capture()
        self.assertEqual(result.returncode, 2)
        self.assertIn("prohibited bearer credential material", result.stderr)
        self.assertNotIn(secret, result.stderr)
        self.assertFalse(self.output.exists())

    def test_rolls_back_evidence_if_sidecar_publication_fails(self) -> None:
        self.write_capture()
        sidecar = self.output.with_suffix(".json.sha256")
        real_link = MODULE.os.link

        def fail_sidecar(source: pathlib.Path, target: pathlib.Path, **kwargs: object) -> None:
            if pathlib.Path(target) == sidecar:
                raise OSError("simulated sidecar publication failure")
            real_link(source, target, **kwargs)

        with mock.patch.object(MODULE.os, "link", side_effect=fail_sidecar):
            with self.assertRaises(MODULE.DesktopEnrollmentEvidencePreparationError):
                MODULE.prepare(self.capture_path, self.output)
        self.assertFalse(self.output.exists())
        self.assertFalse(sidecar.exists())


if __name__ == "__main__":
    unittest.main()
