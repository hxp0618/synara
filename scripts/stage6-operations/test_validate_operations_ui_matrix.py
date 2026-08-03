from __future__ import annotations

import importlib.util
import json
import pathlib
import stat
import subprocess
import sys
import tempfile
import unittest


SCRIPT = pathlib.Path(__file__).with_name("validate_operations_ui_matrix.py")
SPEC = importlib.util.spec_from_file_location("validate_operations_ui_matrix", SCRIPT)
assert SPEC is not None and SPEC.loader is not None
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)
REQUIRED_OPERATIONS = MODULE.REQUIRED_OPERATIONS
PLATFORM_OPERATIONS = MODULE.PLATFORM_OPERATIONS


class ValidateOperationsUIMatrixTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temporary.name)
        (self.root / "ui.tsx").write_text(
            "\n".join(f"ui:{operation}" for operation in sorted(REQUIRED_OPERATIONS)) + "\n",
            encoding="utf-8",
        )
        (self.root / "client.ts").write_text(
            "\n".join(f"client:{operation}" for operation in sorted(REQUIRED_OPERATIONS)) + "\n",
            encoding="utf-8",
        )
        (self.root / "route.go").write_text(
            "\n".join(f"route:{operation}" for operation in sorted(REQUIRED_OPERATIONS)) + "\n",
            encoding="utf-8",
        )
        self.payload = {
            "schemaVersion": "synara.operations-ui-matrix.v2",
            "surfaceBindings": [
                {
                    "id": "platform-admin",
                    "surface": "platform-admin",
                    "status": "reachable",
                    "operations": sorted(PLATFORM_OPERATIONS),
                    "registration": {
                        "path": "ui.tsx",
                        "contains": [f"ui:{sorted(PLATFORM_OPERATIONS)[0]}"],
                    },
                    "entrypoint": {
                        "path": "ui.tsx",
                        "contains": [f"ui:{sorted(PLATFORM_OPERATIONS)[-1]}"],
                    },
                },
                {
                    "id": "tenant-web-reachable",
                    "surface": "tenant-web",
                    "status": "reachable",
                    "operations": sorted(REQUIRED_OPERATIONS - PLATFORM_OPERATIONS),
                    "registration": {
                        "path": "ui.tsx",
                        "contains": [
                            f"ui:{sorted(REQUIRED_OPERATIONS - PLATFORM_OPERATIONS)[0]}"
                        ],
                    },
                    "entrypoint": {
                        "path": "ui.tsx",
                        "contains": [
                            f"ui:{sorted(REQUIRED_OPERATIONS - PLATFORM_OPERATIONS)[-1]}"
                        ],
                    },
                },
            ],
            "operations": [
                {
                    "id": operation,
                    "actor": "tenant-owner",
                    "negativeActor": MODULE.EXPECTED_NEGATIVE_ACTORS[operation],
                    "access": "write",
                    "ui": {"path": "ui.tsx", "contains": [f"ui:{operation}"]},
                    "client": {"path": "client.ts", "contains": [f"client:{operation}"]},
                    "route": {"path": "route.go", "contains": [f"route:{operation}"]},
                    "cliRequired": False,
                    "databaseClientRequired": False,
                }
                for operation in sorted(REQUIRED_OPERATIONS)
            ],
        }
        self.matrix = self.root / "matrix.json"
        self.write_matrix()

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def write_matrix(self) -> None:
        self.matrix.write_text(json.dumps(self.payload, indent=2) + "\n", encoding="utf-8")

    def command(self) -> list[str]:
        return [
            sys.executable,
            str(SCRIPT),
            "--repository-root",
            str(self.root),
            "--matrix",
            str(self.matrix),
            "--output",
            str(self.root / "receipt.json"),
        ]

    def run_validator(self) -> subprocess.CompletedProcess[str]:
        self.write_matrix()
        return subprocess.run(self.command(), check=False, capture_output=True, text=True)

    def test_validates_complete_source_matrix_without_declaring_operations_passed(self) -> None:
        result = self.run_validator()
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = json.loads((self.root / "receipt.json").read_text(encoding="utf-8"))
        self.assertEqual(receipt["operationCount"], len(REQUIRED_OPERATIONS))
        self.assertEqual(
            receipt["assessment"],
            "source-ui-routes-validated-all-surfaces-reachable-not-operations-passed",
        )
        self.assertEqual(
            receipt["statusCounts"]["reachable"], len(REQUIRED_OPERATIONS)
        )
        self.assertEqual(receipt["pendingOperations"], [])
        self.assertTrue((self.root / "receipt.json.sha256").is_file())
        self.assertEqual(stat.S_IMODE((self.root / "receipt.json").stat().st_mode), 0o600)
        self.assertEqual(
            stat.S_IMODE((self.root / "receipt.json.sha256").stat().st_mode), 0o600
        )

    def test_rejects_existing_receipt_without_modifying_it(self) -> None:
        receipt = self.root / "receipt.json"
        sidecar = self.root / "receipt.json.sha256"
        receipt.write_text("retained receipt\n", encoding="utf-8")
        sidecar.write_text("retained sidecar\n", encoding="utf-8")
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("must not already exist", result.stderr)
        self.assertEqual(receipt.read_text(encoding="utf-8"), "retained receipt\n")
        self.assertEqual(sidecar.read_text(encoding="utf-8"), "retained sidecar\n")

    def test_rejects_missing_operation_or_source_marker(self) -> None:
        self.payload["operations"].pop()
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("operation inventory drifted", result.stderr)

        self.setUp_payload_again()
        self.payload["operations"][0]["ui"]["contains"] = ["missing marker"]
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("marker is missing", result.stderr)

    def setUp_payload_again(self) -> None:
        self.payload["operations"] = [
            {
                "id": operation,
                "actor": "tenant-owner",
                "negativeActor": MODULE.EXPECTED_NEGATIVE_ACTORS[operation],
                "access": "write",
                "ui": {"path": "ui.tsx", "contains": [f"ui:{operation}"]},
                "client": {"path": "client.ts", "contains": [f"client:{operation}"]},
                "route": {"path": "route.go", "contains": [f"route:{operation}"]},
                "cliRequired": False,
                "databaseClientRequired": False,
            }
            for operation in sorted(REQUIRED_OPERATIONS)
        ]

    def test_rejects_cli_database_or_path_bypass(self) -> None:
        self.payload["operations"][0]["databaseClientRequired"] = True
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("still requires CLI or database-client access", result.stderr)

        self.payload["operations"][0]["databaseClientRequired"] = False
        self.payload["operations"][0]["route"]["path"] = "../route.go"
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("traversal-free", result.stderr)

    def test_rejects_platform_operations_claimed_as_tenant_ui_or_pending(self) -> None:
        self.payload["surfaceBindings"][0]["surface"] = "tenant-web"
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("must be reachable on platform-admin", result.stderr)

    def test_rejects_unreviewed_negative_actor(self) -> None:
        self.payload["operations"][0]["negativeActor"] = "unauthenticated"
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("reviewed denial matrix", result.stderr)

        self.payload["surfaceBindings"][0]["surface"] = "platform-admin"
        self.payload["surfaceBindings"][0]["status"] = "pending-surface"
        result = self.run_validator()
        self.assertEqual(result.returncode, 2)
        self.assertIn("must be reachable on platform-admin", result.stderr)


if __name__ == "__main__":
    unittest.main()
