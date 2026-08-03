import hashlib
import importlib.util
import json
import pathlib
import stat
import subprocess
import sys
import tempfile
import unittest
from unittest import mock


SCRIPT = pathlib.Path(__file__).with_name("collect_release_evidence.py")
SPEC = importlib.util.spec_from_file_location("collect_release_evidence", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
MODULE = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = MODULE
SPEC.loader.exec_module(MODULE)
DIGEST = "sha256:" + "a" * 64


class CollectReleaseEvidenceTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.output_temporary = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temporary.name)
        self.output_root = pathlib.Path(self.output_temporary.name)
        subprocess.run(["git", "init", "-q", str(self.root)], check=True)
        subprocess.run(["git", "-C", str(self.root), "config", "user.email", "test@example.com"], check=True)
        subprocess.run(["git", "-C", str(self.root), "config", "user.name", "Stage 6 Test"], check=True)
        migrations = self.root / "services/control-plane/migrations"
        migrations.mkdir(parents=True)
        (migrations / "000001_initial.sql").write_text("SELECT 1;\n", encoding="utf-8")
        evidence = self.root / "docs/reports/control.md"
        evidence.parent.mkdir(parents=True)
        evidence.write_text("control evidence\n", encoding="utf-8")
        (self.root / "bun.lock").write_text("lockfileVersion = 1\n", encoding="utf-8")
        subprocess.run(["git", "-C", str(self.root), "add", "."], check=True)
        subprocess.run(["git", "-C", str(self.root), "commit", "-qm", "fixture"], check=True)

    def tearDown(self) -> None:
        self.temporary.cleanup()
        self.output_temporary.cleanup()

    def command(self, *extra: str) -> list[str]:
        return [
            sys.executable,
            str(SCRIPT),
            "--release",
            "stage6-rc1",
            "--repository-root",
            str(self.root),
            "--control-plane-image",
            "registry/control-plane@" + DIGEST,
            "--worker-image",
            DIGEST,
            "--provider-host-image",
            DIGEST,
            "--web-artifact",
            DIGEST,
            "--admin-artifact",
            DIGEST,
            "--desktop-artifact",
            "macos-arm64=" + DIGEST,
            "--desktop-artifact",
            "macos-x64=" + DIGEST,
            "--desktop-artifact",
            "windows-x64=" + DIGEST,
            "--desktop-artifact",
            "linux-x64=" + DIGEST,
            "--environment-class",
            "staging",
            "--environment-id",
            "staging/stage6-rc1",
            "--control-plane-base-url",
            "https://control.example.test/v1",
            "--web-base-url",
            "https://app.example.test",
            "--admin-base-url",
            "https://admin.example.test",
            "--region",
            "cn-east-1",
            "--evidence",
            "cc6.1=docs/reports/control.md",
            "--output",
            str(self.output_root / "evidence.json"),
            "--generated-at",
            "2026-07-30T12:00:00Z",
            *extra,
        ]

    def test_collects_digests_migration_tail_and_explicit_non_pass_status(self) -> None:
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        manifest_path = self.output_root / "evidence.json"
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        self.assertEqual(manifest["schemaVersion"], "synara-stage6-release-evidence-v2")
        self.assertEqual(manifest["assessment"], "evidence-collected-not-control-passed")
        self.assertEqual(manifest["migrations"]["tail"]["path"], "services/control-plane/migrations/000001_initial.sql")
        self.assertEqual(manifest["controls"]["cc6.1"]["status"], "collected")
        self.assertEqual(manifest["artifacts"]["controlPlaneImage"], DIGEST)
        self.assertEqual(manifest["artifacts"]["adminArtifact"], DIGEST)
        self.assertEqual(manifest["deployment"]["environmentClass"], "staging")
        self.assertEqual(manifest["deployment"]["environmentId"], "staging/stage6-rc1")
        self.assertEqual(
            manifest["deployment"]["origins"]["controlPlaneBaseUrl"],
            "https://control.example.test/v1",
        )
        self.assertEqual(
            manifest["source"]["lockfileSha256"],
            hashlib.sha256((self.root / "bun.lock").read_bytes()).hexdigest(),
        )
        self.assertEqual(
            set(manifest["artifacts"]["desktopArtifacts"]),
            {"macos-arm64", "macos-x64", "windows-x64", "linux-x64"},
        )
        sidecar = self.output_root / "evidence.json.sha256"
        self.assertTrue(sidecar.is_file())
        self.assertEqual(stat.S_IMODE(manifest_path.stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(sidecar.stat().st_mode), 0o600)
        original = manifest_path.read_bytes()
        repeated = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(repeated.returncode, 2)
        self.assertIn("must not already exist", repeated.stderr)
        self.assertEqual(manifest_path.read_bytes(), original)

    def test_rejects_dirty_source_tree(self) -> None:
        (self.root / "dirty.txt").write_text("uncommitted\n", encoding="utf-8")
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("repository is dirty", result.stderr)

    def test_rejects_invalid_artifact_digest(self) -> None:
        command = self.command()
        command[command.index("--worker-image") + 1] = "latest"
        result = subprocess.run(command, check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("worker image must be", result.stderr)

    def test_rejects_commit_that_does_not_match_current_head(self) -> None:
        result = subprocess.run(
            self.command("--commit", "b" * 40),
            check=False,
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, 2)
        self.assertIn("commit must match the exact current Git HEAD", result.stderr)

    def test_rejects_symlinked_evidence_file(self) -> None:
        link = self.root / "docs/reports/control-link.md"
        link.symlink_to("control.md")
        subprocess.run(["git", "-C", str(self.root), "add", str(link)], check=True)
        subprocess.run(["git", "-C", str(self.root), "commit", "-qm", "add evidence link"], check=True)
        command = self.command()
        command[command.index("--evidence") + 1] = "cc6.1=docs/reports/control-link.md"
        result = subprocess.run(command, check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("must not use a symbolic link", result.stderr)

    def test_rejects_incomplete_desktop_artifact_set(self) -> None:
        command = self.command()
        index = command.index("--desktop-artifact")
        del command[index : index + 2]
        result = subprocess.run(command, check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("missing desktop artifacts: macos-arm64", result.stderr)

    def test_rejects_unsafe_or_shared_deployment_origins(self) -> None:
        command = self.command()
        command[command.index("--control-plane-base-url") + 1] = "http://control.example.test/v1"
        result = subprocess.run(command, check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("credential-free HTTPS", result.stderr)

        command = self.command()
        command[command.index("--admin-base-url") + 1] = "https://app.example.test"
        result = subprocess.run(command, check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("must use distinct origins", result.stderr)

    def test_rolls_back_manifest_if_sidecar_publication_fails(self) -> None:
        output = self.output_root / "rollback.json"
        sidecar = output.with_suffix(".json.sha256")
        real_link = MODULE.os.link

        def fail_sidecar(source: pathlib.Path, destination: pathlib.Path, **kwargs: object) -> None:
            if pathlib.Path(destination) == sidecar:
                raise OSError("simulated sidecar failure")
            real_link(source, destination, **kwargs)

        with mock.patch.object(MODULE.os, "link", side_effect=fail_sidecar):
            with self.assertRaises(MODULE.EvidenceError):
                MODULE.publish_release_evidence(output, b'{"release":"stage6-rc1"}\n')
        self.assertFalse(output.exists())
        self.assertFalse(sidecar.exists())


if __name__ == "__main__":
    unittest.main()
