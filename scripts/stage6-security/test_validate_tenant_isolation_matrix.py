from __future__ import annotations

import importlib.util
import json
import os
import pathlib
import subprocess
import sys
import tempfile
import unittest


REPO_ROOT = pathlib.Path(__file__).resolve().parents[2]
SCRIPT = pathlib.Path(__file__).with_name("validate_tenant_isolation_matrix.py")
MATRIX = REPO_ROOT / "docs/release-matrices/stage-6-tenant-isolation-v1.json"
SPEC = importlib.util.spec_from_file_location("validate_tenant_isolation_matrix", SCRIPT)
VALIDATOR = importlib.util.module_from_spec(SPEC)
assert SPEC.loader is not None
SPEC.loader.exec_module(VALIDATOR)


class ValidateTenantIsolationMatrixTest(unittest.TestCase):
    def command(self, matrix: pathlib.Path = MATRIX) -> list[str]:
        return [
            sys.executable,
            str(SCRIPT),
            "--repository-root",
            str(REPO_ROOT),
            "--matrix",
            str(matrix),
        ]

    def mutated_matrix(self, transform) -> tuple[tempfile.TemporaryDirectory[str], pathlib.Path]:
        temporary = tempfile.TemporaryDirectory()
        value = json.loads(MATRIX.read_text(encoding="utf-8"))
        transform(value)
        path = pathlib.Path(temporary.name) / "matrix.json"
        path.write_text(json.dumps(value), encoding="utf-8")
        return temporary, path

    def test_checked_in_matrix_references_focused_tests_without_claiming_audit_passed(self) -> None:
        result = subprocess.run(self.command(), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        receipt = json.loads(result.stdout)
        self.assertEqual(receipt["execution"], "source-references-validated")
        self.assertEqual(receipt["assessment"], "focused-tenant-isolation-evidence-validated-not-audit-passed")
        self.assertGreaterEqual(receipt["coverage"]["evidenceRows"], 12)
        matrix = json.loads(MATRIX.read_text(encoding="utf-8"))
        expected_source_only = sum(
            row.get("executionMode", "focused") == "source-reference-only" for row in matrix["evidence"]
        )
        self.assertGreaterEqual(expected_source_only, 1)
        self.assertEqual(receipt["coverage"]["sourceOnlyRows"], expected_source_only)
        self.assertEqual(receipt["coverage"]["postgresRowsExecuted"], 0)
        self.assertEqual(receipt["postgresEvidence"]["assessment"], "not-executed")
        self.assertEqual(
            receipt["coverage"]["executableRows"],
            receipt["coverage"]["evidenceRows"] - expected_source_only,
        )
        self.assertGreaterEqual(receipt["coverage"]["userPrincipalServicePackages"], 1)
        self.assertGreater(receipt["coverage"]["classifiedUserPrincipalServiceEntrypoints"], 0)
        self.assertEqual(
            receipt["coverage"]["userPrincipalServiceEntrypoints"],
            receipt["coverage"]["classifiedUserPrincipalServiceEntrypoints"]
            + receipt["coverage"]["trustedInternalUserPrincipalServiceEntrypoints"]
            + receipt["coverage"]["unclassifiedUserPrincipalServiceEntrypoints"],
        )
        self.assertEqual(receipt["coverage"]["unclassifiedUserPrincipalServiceEntrypoints"], 0)
        self.assertGreater(receipt["coverage"]["trustedInternalUserPrincipalServiceEntrypoints"], 0)
        self.assertEqual(
            receipt["entrypointClassification"]["assessment"],
            "complete-exported-method-inventory-not-operation-exhaustive",
        )

    def test_rejects_unclassified_user_principal_service_package(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            repository_root = pathlib.Path(temporary)
            package = repository_root / "services/control-plane/internal/newresource"
            package.mkdir(parents=True)
            (package / "service.go").write_text(
                "package newresource\nfunc Read(principal identity.Principal) {}\n",
                encoding="utf-8",
            )
            with self.assertRaisesRegex(
                VALIDATOR.MatrixError,
                "matrix lacks Tenant-isolation evidence.*newresource",
            ):
                VALIDATOR.require_user_principal_package_coverage(repository_root, set())
            self.assertEqual(
                VALIDATOR.require_user_principal_package_coverage(
                    repository_root,
                    {"./internal/newresource"},
                ),
                {"./internal/newresource"},
            )

    def test_rejects_duplicate_evidence_id(self) -> None:
        temporary, path = self.mutated_matrix(
            lambda value: value["evidence"].__setitem__(1, {**value["evidence"][1], "id": value["evidence"][0]["id"]})
        )
        self.addCleanup(temporary.cleanup)
        result = subprocess.run(self.command(path), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("duplicate evidence id", result.stderr)

    def test_rejects_missing_classified_user_principal_entrypoint(self) -> None:
        def add_missing_entrypoint(value) -> None:
            row = next(row for row in value["evidence"] if row["id"] == "entitlement-active-context")
            row["entrypoints"] = ["Service.Missing"]

        temporary, path = self.mutated_matrix(add_missing_entrypoint)
        self.addCleanup(temporary.cleanup)
        result = subprocess.run(self.command(path), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("classifies missing user Principal entrypoints", result.stderr)

    def test_rejects_classified_entrypoint_not_invoked_by_test(self) -> None:
        def add_uninvoked_entrypoint(value) -> None:
            row = next(row for row in value["evidence"] if row["id"] == "tenant-active-context")
            row["entrypoints"].append("Service.AcceptInvitation")

        temporary, path = self.mutated_matrix(add_uninvoked_entrypoint)
        self.addCleanup(temporary.cleanup)
        result = subprocess.run(self.command(path), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("entrypoint Service.AcceptInvitation is not directly invoked by testName", result.stderr)

    def test_rejects_missing_trusted_internal_contract_marker(self) -> None:
        def replace_contract_marker(value) -> None:
            value["trustedInternalEntrypoints"][0]["contractMarker"] = "missing trusted contract marker"

        temporary, path = self.mutated_matrix(replace_contract_marker)
        self.addCleanup(temporary.cleanup)
        result = subprocess.run(self.command(path), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("contract does not match the reviewed allow-list", result.stderr)

    def test_rejects_unclassified_user_principal_service_entrypoint(self) -> None:
        def remove_classification(value) -> None:
            row = next(row for row in value["evidence"] if row["id"] == "entitlement-active-context")
            row["entrypoints"] = []

        temporary, path = self.mutated_matrix(remove_classification)
        self.addCleanup(temporary.cleanup)
        result = subprocess.run(self.command(path), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("unclassified user Principal Service entrypoints", result.stderr)

    def test_rejects_duplicate_classified_entrypoint(self) -> None:
        def duplicate_entrypoint(value) -> None:
            row = next(row for row in value["evidence"] if row["id"] == "project-active-context")
            row["entrypoints"] = ["Service.Get"]

        temporary, path = self.mutated_matrix(duplicate_entrypoint)
        self.addCleanup(temporary.cleanup)
        result = subprocess.run(self.command(path), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("duplicate classified user Principal entrypoint", result.stderr)

    def test_rejects_missing_test_symbol(self) -> None:
        temporary, path = self.mutated_matrix(
            lambda value: value["evidence"][0].__setitem__("testName", "TestMissingTenantIsolationEvidence")
        )
        self.addCleanup(temporary.cleanup)
        result = subprocess.run(self.command(path), check=False, capture_output=True, text=True)
        self.assertEqual(result.returncode, 2)
        self.assertIn("testName was not found", result.stderr)

    def test_postgres_execution_requires_an_explicit_postgres_url(self) -> None:
        environment = dict(os.environ)
        environment.pop("TEST_POSTGRES_URL", None)
        environment.pop("SYNARA_TEST_DATABASE_URL", None)
        result = subprocess.run(
            [*self.command(), "--run-postgres-tests"],
            check=False,
            capture_output=True,
            text=True,
            env=environment,
        )
        self.assertEqual(result.returncode, 2)
        self.assertIn("requires TEST_POSTGRES_URL or SYNARA_TEST_DATABASE_URL", result.stderr)

    def test_postgres_execution_rejects_split_database_authority(self) -> None:
        with self.assertRaisesRegex(VALIDATOR.MatrixError, "must identify the same PostgreSQL database"):
            VALIDATOR.postgres_test_environment(
                {
                    "TEST_POSTGRES_URL": "postgres://localhost/first",
                    "SYNARA_TEST_DATABASE_URL": "postgres://localhost/second",
                }
            )

    def test_postgres_execution_normalizes_both_test_environment_names(self) -> None:
        environment, database_url = VALIDATOR.postgres_test_environment(
            {"TEST_POSTGRES_URL": "postgresql://localhost/synara"}
        )
        self.assertEqual(database_url, "postgresql://localhost/synara")
        self.assertEqual(environment["TEST_POSTGRES_URL"], database_url)
        self.assertEqual(environment["SYNARA_TEST_DATABASE_URL"], database_url)

    def test_postgres_execution_rejects_non_postgres_urls(self) -> None:
        with self.assertRaisesRegex(VALIDATOR.MatrixError, "must use postgres"):
            VALIDATOR.postgres_test_environment({"TEST_POSTGRES_URL": "sqlite:///tmp/test.db"})


if __name__ == "__main__":
    unittest.main()
