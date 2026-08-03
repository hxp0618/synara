#!/usr/bin/env python3
"""Validate and optionally execute focused Stage 6 Tenant-isolation evidence."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import pathlib
import re
import subprocess
import sys
from collections import defaultdict
from collections.abc import Mapping
from urllib.parse import urlsplit


SCHEMA_VERSION = "synara.tenant-isolation-matrix.v1"
EXPECTED_SCOPE = "focused-high-risk-resource-families-not-exhaustive"
EXPECTED_ASSESSMENT = "focused-tenant-isolation-evidence-validated-not-audit-passed"
TEST_NAME_RE = re.compile(r"^Test[A-Za-z0-9_]+$")
USER_PRINCIPAL_RE = re.compile(r"\bprincipal\s+identity\.Principal\b")
SERVICE_PRINCIPAL_METHOD_RE = re.compile(
    r"func\s+\(\s*[A-Za-z_][A-Za-z0-9_]*\s+\*Service\s*\)\s+"
    r"([A-Z][A-Za-z0-9_]*)\s*\((.*?)\)\s*(?:\([^)]*\)|[^\s{]+)?\s*\{",
    re.DOTALL,
)
ENTRYPOINT_RE = re.compile(r"^Service\.[A-Z][A-Za-z0-9_]*$")
CANONICAL_ENTRYPOINT_RE = re.compile(r"^\./internal/[A-Za-z0-9_/-]+:Service\.[A-Z][A-Za-z0-9_]*$")
GO_NON_CODE_RE = re.compile(
    r"//[^\n]*|/\*.*?\*/|`[^`]*`|\"(?:\\.|[^\"\\])*\"|'(?:\\.|[^'\\])*'",
    re.DOTALL,
)
EXPECTED_TRUSTED_INTERNAL_ENTRYPOINTS = {
    "./internal/executions:Service.RevokeExecutionTargetWorkersInTransaction": {
        "sourceFile": "services/control-plane/internal/executions/worker_revocation.go",
        "contractMarker": (
            "RevokeExecutionTargetWorkersInTransaction assumes the caller already\n"
            "// authorized the principal and holds the supplied SSH Target FOR UPDATE in tx."
        ),
    },
}


class MatrixError(Exception):
    pass


def postgres_test_environment(environ: Mapping[str, str]) -> tuple[dict[str, str], str]:
    test_url = environ.get("TEST_POSTGRES_URL", "").strip()
    legacy_url = environ.get("SYNARA_TEST_DATABASE_URL", "").strip()
    if test_url and legacy_url and test_url != legacy_url:
        raise MatrixError(
            "TEST_POSTGRES_URL and SYNARA_TEST_DATABASE_URL must identify the same PostgreSQL database"
        )
    database_url = test_url or legacy_url
    if not database_url:
        raise MatrixError(
            "PostgreSQL evidence execution requires TEST_POSTGRES_URL or SYNARA_TEST_DATABASE_URL"
        )
    try:
        scheme = urlsplit(database_url).scheme.lower()
    except ValueError as error:
        raise MatrixError("PostgreSQL evidence URL is invalid") from error
    if scheme not in {"postgres", "postgresql"}:
        raise MatrixError("PostgreSQL evidence URL must use postgres:// or postgresql://")
    child_environment = dict(environ)
    child_environment["TEST_POSTGRES_URL"] = database_url
    child_environment["SYNARA_TEST_DATABASE_URL"] = database_url
    return child_environment, database_url


def run_go_test_packages(
    control_plane_root: pathlib.Path,
    packages: Mapping[str, list[str]],
    *,
    label: str,
    environment: Mapping[str, str] | None = None,
    secret_values: tuple[str, ...] = (),
) -> None:
    for package in sorted(packages):
        names = sorted(packages[package])
        pattern = "^(" + "|".join(re.escape(name) for name in names) + ")$"
        result = subprocess.run(
            ["go", "test", "-json", "-count=1", package, "-run", pattern],
            cwd=control_plane_root,
            check=False,
            capture_output=True,
            text=True,
            env=None if environment is None else dict(environment),
        )
        if result.returncode != 0:
            detail = (result.stdout + "\n" + result.stderr).strip()
            for secret in secret_values:
                if secret:
                    detail = detail.replace(secret, "<redacted-postgres-url>")
            raise MatrixError(f"{label} Go tests failed for {package}: {detail}")
        outcomes: dict[str, str] = {}
        for line in result.stdout.splitlines():
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                continue
            test_name = event.get("Test")
            action = event.get("Action")
            if test_name in names and action in {"pass", "fail", "skip"}:
                outcomes[test_name] = action
        incomplete = [name for name in names if outcomes.get(name) != "pass"]
        if incomplete:
            detail = ", ".join(f"{name}={outcomes.get(name, 'missing')}" for name in incomplete)
            raise MatrixError(f"{label} Go tests did not pass for {package}: {detail}")


def read_json(path: pathlib.Path) -> dict[str, object]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as error:
        raise MatrixError("matrix could not be read as UTF-8 JSON") from error
    if not isinstance(value, dict):
        raise MatrixError("matrix root must be an object")
    return value


def require_text(row: dict[str, object], key: str, row_id: str) -> str:
    value = row.get(key)
    if not isinstance(value, str) or not value.strip():
        raise MatrixError(f"evidence {row_id} has invalid {key}")
    return value.strip()


def require_user_principal_package_coverage(
    repository_root: pathlib.Path,
    matrix_packages: set[str],
) -> set[str]:
    internal_root = repository_root / "services/control-plane/internal"
    principal_packages: set[str] = set()
    for source_path in sorted(internal_root.rglob("*.go")):
        if source_path.name.endswith("_test.go"):
            continue
        try:
            source = source_path.read_text(encoding="utf-8")
        except (OSError, UnicodeDecodeError) as error:
            raise MatrixError(f"Principal service source could not be read: {source_path}") from error
        if USER_PRINCIPAL_RE.search(source) is None:
            continue
        relative_package = source_path.parent.relative_to(internal_root).as_posix()
        package = "./internal" if relative_package == "." else f"./internal/{relative_package}"
        principal_packages.add(package)
    missing = sorted(principal_packages - matrix_packages)
    if missing:
        raise MatrixError(
            "matrix lacks Tenant-isolation evidence for user Principal service packages: "
            + ", ".join(missing)
        )
    return principal_packages


def discover_user_principal_entrypoints(repository_root: pathlib.Path) -> set[str]:
    internal_root = repository_root / "services/control-plane/internal"
    entrypoints: set[str] = set()
    for source_path in sorted(internal_root.rglob("*.go")):
        if source_path.name.endswith("_test.go"):
            continue
        try:
            source = source_path.read_text(encoding="utf-8")
        except (OSError, UnicodeDecodeError) as error:
            raise MatrixError(f"Principal service source could not be read: {source_path}") from error
        relative_package = source_path.parent.relative_to(internal_root).as_posix()
        package = "./internal" if relative_package == "." else f"./internal/{relative_package}"
        for method_name, parameters in SERVICE_PRINCIPAL_METHOD_RE.findall(source):
            if USER_PRINCIPAL_RE.search(parameters) is not None:
                entrypoints.add(f"{package}:Service.{method_name}")
    return entrypoints


def validate(
    repository_root: pathlib.Path,
    matrix_path: pathlib.Path,
    run_tests: bool,
    run_postgres_tests: bool = False,
    environ: Mapping[str, str] | None = None,
) -> dict[str, object]:
    matrix_bytes = matrix_path.read_bytes()
    matrix = read_json(matrix_path)
    if matrix.get("schemaVersion") != SCHEMA_VERSION:
        raise MatrixError("matrix schemaVersion is unsupported")
    if matrix.get("scope") != EXPECTED_SCOPE:
        raise MatrixError("matrix scope must remain explicitly non-exhaustive")
    if matrix.get("assessment") != EXPECTED_ASSESSMENT:
        raise MatrixError("matrix assessment must not claim that the Tenant audit passed")
    evidence = matrix.get("evidence")
    if not isinstance(evidence, list) or len(evidence) < 12:
        raise MatrixError("matrix must contain at least 12 focused evidence rows")

    seen_ids: set[str] = set()
    seen_tests: set[tuple[str, str]] = set()
    packages: dict[str, list[str]] = defaultdict(list)
    postgres_packages: dict[str, list[str]] = defaultdict(list)
    all_packages: set[str] = set()
    source_files: dict[str, str] = {}
    operation_count = 0
    source_only_rows = 0
    classified_entrypoints: set[str] = set()
    discovered_entrypoints = discover_user_principal_entrypoints(repository_root)
    trusted_internal = matrix.get("trustedInternalEntrypoints")
    if not isinstance(trusted_internal, list):
        raise MatrixError("matrix trustedInternalEntrypoints must be an array")
    trusted_internal_entrypoints: set[str] = set()
    trusted_internal_details: list[dict[str, str]] = []
    for index, raw_entrypoint in enumerate(trusted_internal):
        if not isinstance(raw_entrypoint, dict):
            raise MatrixError(f"trusted internal entrypoint {index} must be an object")
        entrypoint = require_text(raw_entrypoint, "entrypoint", f"trusted internal {index}")
        source_file = require_text(raw_entrypoint, "sourceFile", entrypoint)
        contract_marker = require_text(raw_entrypoint, "contractMarker", entrypoint)
        reason = require_text(raw_entrypoint, "reason", entrypoint)
        if CANONICAL_ENTRYPOINT_RE.fullmatch(entrypoint) is None:
            raise MatrixError(f"trusted internal entrypoint {entrypoint} is invalid")
        if entrypoint not in discovered_entrypoints:
            raise MatrixError(f"trusted internal entrypoint does not exist: {entrypoint}")
        expected_trust = EXPECTED_TRUSTED_INTERNAL_ENTRYPOINTS.get(entrypoint)
        if expected_trust is None:
            raise MatrixError(f"trusted internal entrypoint is not in the reviewed allow-list: {entrypoint}")
        if source_file != expected_trust["sourceFile"] or contract_marker != expected_trust["contractMarker"]:
            raise MatrixError(f"trusted internal entrypoint contract does not match the reviewed allow-list: {entrypoint}")
        if entrypoint in trusted_internal_entrypoints:
            raise MatrixError(f"duplicate trusted internal entrypoint {entrypoint}")
        source_path = (repository_root / source_file).resolve()
        try:
            source_path.relative_to(repository_root)
        except ValueError as error:
            raise MatrixError(f"trusted internal entrypoint {entrypoint} sourceFile escapes the repository") from error
        if not source_file.startswith("services/control-plane/internal/") or not source_file.endswith(".go"):
            raise MatrixError(f"trusted internal entrypoint {entrypoint} sourceFile is invalid")
        try:
            source = source_path.read_text(encoding="utf-8")
        except (OSError, UnicodeDecodeError) as error:
            raise MatrixError(f"trusted internal entrypoint {entrypoint} sourceFile could not be read") from error
        if contract_marker not in source:
            raise MatrixError(f"trusted internal entrypoint {entrypoint} contract marker was not found")
        trusted_internal_entrypoints.add(entrypoint)
        trusted_internal_details.append(
            {"entrypoint": entrypoint, "reason": reason, "sourceFile": source_file}
        )
    missing_trusted_internal = sorted(
        set(EXPECTED_TRUSTED_INTERNAL_ENTRYPOINTS) - trusted_internal_entrypoints
    )
    if missing_trusted_internal:
        raise MatrixError(
            "matrix is missing reviewed trusted internal entrypoints: "
            + ", ".join(missing_trusted_internal)
        )
    for index, raw_row in enumerate(evidence):
        if not isinstance(raw_row, dict):
            raise MatrixError(f"evidence row {index} must be an object")
        row_id = require_text(raw_row, "id", str(index))
        if row_id in seen_ids:
            raise MatrixError(f"duplicate evidence id {row_id}")
        seen_ids.add(row_id)
        require_text(raw_row, "family", row_id)
        require_text(raw_row, "boundary", row_id)
        package = require_text(raw_row, "package", row_id)
        test_file = require_text(raw_row, "testFile", row_id)
        test_name = require_text(raw_row, "testName", row_id)
        execution_mode = raw_row.get("executionMode", "focused")
        if execution_mode not in {"focused", "source-reference-only"}:
            raise MatrixError(f"evidence {row_id} executionMode is invalid")
        if not package.startswith("./internal/") or ".." in pathlib.PurePosixPath(package).parts:
            raise MatrixError(f"evidence {row_id} package is outside internal Control Plane packages")
        if not TEST_NAME_RE.fullmatch(test_name):
            raise MatrixError(f"evidence {row_id} testName is invalid")
        key = (test_file, test_name)
        if key in seen_tests:
            raise MatrixError(f"duplicate test evidence {test_file}:{test_name}")
        seen_tests.add(key)
        operations = raw_row.get("operations")
        if not isinstance(operations, list) or not operations or any(
            not isinstance(value, str) or not value.strip() for value in operations
        ):
            raise MatrixError(f"evidence {row_id} operations must be a non-empty string array")
        operation_count += len(operations)
        entrypoints = raw_row.get("entrypoints", [])
        if not isinstance(entrypoints, list) or any(
            not isinstance(value, str) or ENTRYPOINT_RE.fullmatch(value.strip()) is None
            for value in entrypoints
        ):
            raise MatrixError(f"evidence {row_id} entrypoints must be Service.Method strings")
        normalized_entrypoints = [value.strip() for value in entrypoints]
        for entrypoint in normalized_entrypoints:
            canonical_entrypoint = f"{package}:{entrypoint}"
            if canonical_entrypoint not in discovered_entrypoints:
                raise MatrixError(
                    "matrix classifies missing user Principal entrypoints: " + canonical_entrypoint
                )
            if canonical_entrypoint in classified_entrypoints:
                raise MatrixError(f"duplicate classified user Principal entrypoint {canonical_entrypoint}")
            classified_entrypoints.add(canonical_entrypoint)

        source_path = (repository_root / test_file).resolve()
        try:
            source_path.relative_to(repository_root)
        except ValueError as error:
            raise MatrixError(f"evidence {row_id} testFile escapes the repository") from error
        if not test_file.startswith("services/control-plane/internal/") or not test_file.endswith("_test.go"):
            raise MatrixError(f"evidence {row_id} testFile is outside Control Plane internal tests")
        try:
            source_bytes = source_path.read_bytes()
            source = source_bytes.decode("utf-8")
        except (OSError, UnicodeDecodeError) as error:
            raise MatrixError(f"evidence {row_id} testFile could not be read") from error
        declaration = re.compile(rf"^func {re.escape(test_name)}\(t \*testing\.T\) \{{", re.MULTILINE)
        declaration_match = declaration.search(source)
        if declaration_match is None:
            raise MatrixError(f"evidence {row_id} testName was not found in testFile")
        remainder = source[declaration_match.end() :]
        next_declaration = re.search(r"^func\s+", remainder, re.MULTILINE)
        function_end = len(source)
        if next_declaration is not None:
            function_end = declaration_match.end() + next_declaration.start()
        test_code = GO_NON_CODE_RE.sub("", source[declaration_match.start() : function_end])
        for entrypoint in normalized_entrypoints:
            method_name = entrypoint.removeprefix("Service.")
            if re.search(rf"\.\s*{re.escape(method_name)}\s*\(", test_code) is None:
                raise MatrixError(
                    f"evidence {row_id} entrypoint {entrypoint} is not directly invoked by testName"
                )
        source_files[test_file] = "sha256:" + hashlib.sha256(source_bytes).hexdigest()
        if execution_mode == "focused":
            packages[package].append(test_name)
        else:
            source_only_rows += 1
            postgres_packages[package].append(test_name)
        all_packages.add(package)

    principal_packages = require_user_principal_package_coverage(repository_root, all_packages)
    overlap = sorted(classified_entrypoints & trusted_internal_entrypoints)
    if overlap:
        raise MatrixError("entrypoints cannot be both evidence-classified and trusted internal: " + ", ".join(overlap))
    unclassified_entrypoints = sorted(
        discovered_entrypoints - classified_entrypoints - trusted_internal_entrypoints
    )
    if unclassified_entrypoints:
        raise MatrixError(
            "matrix has unclassified user Principal Service entrypoints: "
            + ", ".join(unclassified_entrypoints)
        )
    entrypoint_assessment = "complete-exported-method-inventory-not-operation-exhaustive"

    execution = "source-references-validated"
    postgres_rows_executed = 0
    control_plane_root = repository_root / "services/control-plane"
    if run_tests:
        run_go_test_packages(control_plane_root, packages, label="focused")
        execution = "focused-go-tests-passed"
        if source_only_rows:
            execution = "focused-go-tests-passed-source-only-rows-not-executed"
    if run_postgres_tests:
        child_environment, database_url = postgres_test_environment(
            os.environ if environ is None else environ
        )
        run_go_test_packages(
            control_plane_root,
            postgres_packages,
            label="PostgreSQL evidence",
            environment=child_environment,
            secret_values=(database_url,),
        )
        postgres_rows_executed = source_only_rows
        execution = "postgres-go-tests-passed-focused-rows-not-executed"
        if run_tests:
            execution = "focused-and-postgres-go-tests-passed"

    return {
        "schemaVersion": "synara.tenant-isolation-matrix-validation.v1",
        "matrix": {
            "path": str(matrix_path.relative_to(repository_root)),
            "sha256": "sha256:" + hashlib.sha256(matrix_bytes).hexdigest(),
        },
        "coverage": {
            "evidenceRows": len(evidence),
            "operations": operation_count,
            "packages": len(all_packages),
            "executablePackages": len(packages),
            "sourceFiles": len(source_files),
            "executableRows": len(evidence) - source_only_rows,
            "sourceOnlyRows": source_only_rows,
            "postgresRowsExecuted": postgres_rows_executed,
            "userPrincipalServicePackages": len(principal_packages),
            "userPrincipalServiceEntrypoints": len(discovered_entrypoints),
            "classifiedUserPrincipalServiceEntrypoints": len(classified_entrypoints),
            "trustedInternalUserPrincipalServiceEntrypoints": len(trusted_internal_entrypoints),
            "unclassifiedUserPrincipalServiceEntrypoints": len(unclassified_entrypoints),
        },
        "entrypointClassification": {
            "classified": sorted(classified_entrypoints),
            "trustedInternal": sorted(trusted_internal_details, key=lambda item: item["entrypoint"]),
            "unclassified": unclassified_entrypoints,
            "assessment": entrypoint_assessment,
        },
        "sourceFiles": dict(sorted(source_files.items())),
        "execution": execution,
        "postgresEvidence": {
            "requested": run_postgres_tests,
            "rowsExecuted": postgres_rows_executed,
            "connectionAuthority": "operator-supplied-postgresql-url-not-attested",
            "assessment": "postgres-behavior-executed-not-production-boundary-verified"
            if run_postgres_tests
            else "not-executed",
        },
        "assessment": EXPECTED_ASSESSMENT,
    }


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repository-root", default=".")
    parser.add_argument("--matrix", default="docs/release-matrices/stage-6-tenant-isolation-v1.json")
    parser.add_argument("--run-tests", action="store_true")
    parser.add_argument(
        "--run-postgres-tests",
        action="store_true",
        help="execute source-reference-only PostgreSQL rows using an explicitly configured test database",
    )
    return parser


def main() -> int:
    parser = build_parser()
    args = parser.parse_args()
    root = pathlib.Path(args.repository_root).resolve()
    matrix = pathlib.Path(args.matrix)
    if not matrix.is_absolute():
        matrix = root / matrix
    try:
        result = validate(root, matrix.resolve(), args.run_tests, args.run_postgres_tests)
    except (MatrixError, OSError) as error:
        parser.exit(2, f"Tenant isolation matrix validation failed: {error}\n")
    print(json.dumps(result, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    sys.exit(main())
