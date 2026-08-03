from __future__ import annotations

import importlib.util
import json
import pathlib
import subprocess
import sys
import tempfile
import unittest


REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
SCRIPT = pathlib.Path(__file__).with_name("validate_compatibility_matrix.py")
MATRIX = REPO_ROOT / "docs/release-matrices/stage-6-compatibility-v1.json"
VALIDATOR_SPEC = importlib.util.spec_from_file_location("stage6_compatibility_validator", SCRIPT)
if VALIDATOR_SPEC is None or VALIDATOR_SPEC.loader is None:
    raise RuntimeError("could not load Stage 6 compatibility validator")
VALIDATOR = importlib.util.module_from_spec(VALIDATOR_SPEC)
VALIDATOR_SPEC.loader.exec_module(VALIDATOR)


class ValidateCompatibilityMatrixTest(unittest.TestCase):
    def command(self, matrix: pathlib.Path = MATRIX) -> list[str]:
        return [
            sys.executable,
            str(SCRIPT),
            "--repository-root",
            str(REPO_ROOT),
            "--matrix",
            str(matrix),
        ]

    def mutated_matrix(self, mutate) -> tuple[tempfile.TemporaryDirectory[str], pathlib.Path]:
        temporary = tempfile.TemporaryDirectory()
        path = pathlib.Path(temporary.name) / "matrix.json"
        value = json.loads(MATRIX.read_text(encoding="utf-8"))
        mutate(value)
        path.write_text(json.dumps(value), encoding="utf-8")
        return temporary, path

    def test_checked_in_matrix_matches_current_source(self) -> None:
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = json.loads(result.stdout)
        self.assertEqual(receipt["assessment"], "source-compatible-not-release-approved")
        self.assertEqual(receipt["protocols"]["worker"], {"minimum": 2, "maximum": 2})
        self.assertEqual(receipt["packages"]["desktop"], receipt["packages"]["enterpriseUi"])
        self.assertGreater(receipt["sourceFileCount"], 160)
        self.assertGreater(receipt["sourceByteCount"], 0)

    def test_rejects_package_version_drift(self) -> None:
        temporary, path = self.mutated_matrix(lambda value: value["components"]["packages"].update(web="9.9.9"))
        self.addCleanup(temporary.cleanup)
        result = subprocess.run(self.command(path), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("components.packages.web", result.stderr)

    def test_rejects_migration_digest_drift(self) -> None:
        temporary, path = self.mutated_matrix(
            lambda value: value["database"]["targetTail"].update(sha256="sha256:" + "0" * 64)
        )
        self.addCleanup(temporary.cleanup)
        result = subprocess.run(self.command(path), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("database.targetTail.sha256", result.stderr)

    def test_rejects_duplicate_numeric_migration_versions(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            migration_root = pathlib.Path(temporary)
            first = migration_root / "000107_runtime_isolation.sql"
            second = migration_root / "000107_desktop_enrollment.sql"
            first.write_text("SELECT 1;", encoding="utf-8")
            second.write_text("SELECT 2;", encoding="utf-8")
            with self.assertRaisesRegex(
                VALIDATOR.CompatibilityError,
                "duplicate Control Plane migration version 107",
            ):
                VALIDATOR.validate_migration_lineage(sorted(migration_root.glob("*.sql")))

    def test_rejects_unsafe_rollback_policy(self) -> None:
        temporary, path = self.mutated_matrix(lambda value: value["database"].update(rollbackPolicy="down-migrate"))
        self.addCleanup(temporary.cleanup)
        result = subprocess.run(self.command(path), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("database.rollbackPolicy", result.stderr)

    def test_rejects_duplicate_secret_or_symlinked_matrix(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary)
            encoded = MATRIX.read_text(encoding="utf-8")
            duplicate = encoded.replace(
                "{",
                '{\n  "schemaVersion": "synara.release-compatibility-matrix.v1",',
                1,
            )
            matrix = root / "matrix.json"
            matrix.write_text(duplicate, encoding="utf-8")
            result = subprocess.run(
                self.command(matrix), check=False, capture_output=True, text=True
            )
            self.assertEqual(result.returncode, 2)
            self.assertIn("duplicate field", result.stderr)

            secret = "Authorization: Bearer compatibility-matrix-secret"
            matrix.write_text(secret + "\n", encoding="utf-8")
            result = subprocess.run(
                self.command(matrix), check=False, capture_output=True, text=True
            )
            self.assertEqual(result.returncode, 2)
            self.assertIn("prohibited bearer credential material", result.stderr)
            self.assertNotIn(secret, result.stderr)

            real_matrix = root / "real-matrix.json"
            real_matrix.write_bytes(MATRIX.read_bytes())
            matrix.unlink()
            matrix.symlink_to(real_matrix.name)
            result = subprocess.run(
                self.command(matrix), check=False, capture_output=True, text=True
            )
            self.assertEqual(result.returncode, 2)
            self.assertIn("non-symlink", result.stderr)

    def test_rejects_symlinked_or_duplicate_json_source(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = pathlib.Path(temporary).resolve()
            real = root / "real.json"
            real.write_text('{"version":"1.0.0"}\n', encoding="utf-8")
            link = root / "package.json"
            link.symlink_to(real.name)
            with self.assertRaisesRegex(VALIDATOR.CompatibilityError, "symlink"):
                VALIDATOR.read_json(root, link.name, {}, [0])

            link.unlink()
            link.write_text(
                '{"version":"1.0.0","version":"2.0.0"}\n', encoding="utf-8"
            )
            with self.assertRaisesRegex(VALIDATOR.CompatibilityError, "duplicate field"):
                VALIDATOR.read_json(root, link.name, {}, [0])


if __name__ == "__main__":
    unittest.main()
