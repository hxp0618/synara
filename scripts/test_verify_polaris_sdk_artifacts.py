from __future__ import annotations

import io
import json
import tarfile
import tempfile
import unittest
import zipfile
from pathlib import Path

from verify_polaris_sdk_artifacts import safe_archive_names, verify_artifacts


NPM_VERSION = "0.1.0-beta.1"
PYTHON_VERSION = "0.1.0b1"


class PolarisSDKArtifactVerificationTest(unittest.TestCase):
    def test_accepts_the_bounded_release_archives_and_records_digests(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            self.write_valid_artifacts(root)

            result = verify_artifacts(
                root,
                npm_version=NPM_VERSION,
                python_version=PYTHON_VERSION,
            )

            self.assertEqual(result["schemaVersion"], "polaris.sdk-release-artifacts.v1")
            self.assertEqual(len(result["artifacts"]), 3)
            self.assertTrue(
                all(item["sha256"].startswith("sha256:") for item in result["artifacts"])
            )

    def test_rejects_an_unlisted_npm_file(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            self.write_valid_artifacts(root, extra_npm_file=("package/install.js", b"unsafe"))

            with self.assertRaisesRegex(ValueError, "allowlist"):
                verify_artifacts(root, npm_version=NPM_VERSION, python_version=PYTHON_VERSION)

    def test_rejects_archive_traversal_and_duplicate_paths(self) -> None:
        with self.assertRaisesRegex(ValueError, "unsafe"):
            safe_archive_names(["package/index.js", "../credential"])
        with self.assertRaisesRegex(ValueError, "duplicate"):
            safe_archive_names(["package/index.js", "package/index.js"])

    def write_valid_artifacts(
        self,
        root: Path,
        *,
        extra_npm_file: tuple[str, bytes] | None = None,
    ) -> None:
        npm = root / "npm"
        python = root / "python"
        npm.mkdir(parents=True)
        python.mkdir(parents=True)
        npm_files = {
            "package/package.json": json.dumps(
                {
                    "name": "@polaris-agents/sdk",
                    "version": NPM_VERSION,
                    "private": False,
                }
            ).encode(),
            "package/README.md": b"readme",
            "package/dist/index.mjs": b"esm",
            "package/dist/index.cjs": b"cjs",
            "package/dist/index.d.mts": b"types",
            "package/dist/index.d.cts": b"types",
        }
        if extra_npm_file is not None:
            npm_files[extra_npm_file[0]] = extra_npm_file[1]
        self.write_tar(npm / "polaris-agents-sdk-0.1.0-beta.1.tgz", npm_files)

        wheel_name = python / "polaris_agents-0.1.0b1-py3-none-any.whl"
        with zipfile.ZipFile(wheel_name, "w") as archive:
            for module in ("__init__.py", "_client.py", "_errors.py", "_generated.py", "_transport.py"):
                archive.writestr(f"polaris_agents/{module}", "")
            archive.writestr(
                "polaris_agents-0.1.0b1.dist-info/METADATA",
                "Metadata-Version: 2.4\nName: polaris-agents\nVersion: 0.1.0b1\n",
            )

        prefix = "polaris_agents-0.1.0b1/"
        self.write_tar(
            python / "polaris_agents-0.1.0b1.tar.gz",
            {
                prefix + "pyproject.toml": b"[project]",
                prefix + "README.md": b"readme",
                prefix + "src/polaris_agents/__init__.py": b"",
                prefix + "src/polaris_agents/_generated.py": b"",
            },
        )

    @staticmethod
    def write_tar(path: Path, files: dict[str, bytes]) -> None:
        with tarfile.open(path, "w:gz") as archive:
            for name, value in files.items():
                info = tarfile.TarInfo(name)
                info.size = len(value)
                archive.addfile(info, io.BytesIO(value))


if __name__ == "__main__":
    unittest.main()
