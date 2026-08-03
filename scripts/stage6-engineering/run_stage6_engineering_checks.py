#!/usr/bin/env python3
"""Run every source-level Stage 6 engineering gate without claiming GA approval."""

from __future__ import annotations

import argparse
import json
import os
import pathlib
import subprocess
import sys


STATIC_CHECKS = (
    ("compatibility", "scripts/stage6-compatibility/validate_compatibility_matrix.py", ()),
    (
        "documentation",
        "scripts/stage6-documentation/validate_documentation_set.py",
        ("--matrix", "docs/release-matrices/stage-6-documentation-v1.json"),
    ),
    (
        "operations",
        "scripts/stage6-operations/validate_operations_ui_matrix.py",
        ("--matrix", "docs/release-matrices/stage-6-operations-ui-v1.json"),
    ),
    ("route-auth", "scripts/stage6-security/validate_route_auth_boundaries.py", ()),
    (
        "internal-self-hosted-boundary",
        "scripts/stage6-security/validate_internal_self_hosted_boundary.py",
        (),
    ),
    (
        "artifact-storage-deployment",
        "scripts/stage6-security/validate_artifact_storage_deployment.py",
        (),
    ),
    (
        "observability-deployment",
        "scripts/stage6-security/validate_observability_deployment.py",
        (),
    ),
    ("tenant-isolation", "scripts/stage6-security/validate_tenant_isolation_matrix.py", ()),
)

UNIT_TEST_DIRECTORIES = (
    "scripts/stage6-candidate",
    "scripts/stage6-capacity",
    "scripts/stage6-compatibility",
    "scripts/stage6-cost",
    "scripts/stage6-documentation",
    "scripts/stage6-desktop",
    "scripts/stage6-evidence",
    "scripts/stage6-final",
    "scripts/stage6-incident",
    "scripts/stage6-operations",
    "scripts/stage6-penetration",
    "scripts/stage6-recovery",
    "scripts/stage6-residency",
    "scripts/stage6-rotation",
    "scripts/stage6-security",
    "scripts/stage6-slo",
)


def run(root: pathlib.Path, label: str, arguments: list[str]) -> None:
    print(f"[stage6-engineering] {label}", flush=True)
    environment = os.environ.copy()
    environment["PYTHONDONTWRITEBYTECODE"] = "1"
    result = subprocess.run(arguments, cwd=root, env=environment, check=False)
    if result.returncode != 0:
        raise SystemExit(result.returncode)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repository-root", default=".")
    args = parser.parse_args()
    root = pathlib.Path(args.repository_root).resolve()
    if not root.is_dir():
        parser.error("repository root does not exist")

    for label, script, extra_arguments in STATIC_CHECKS:
        run(
            root,
            f"validate {label}",
            [
                sys.executable,
                script,
                "--repository-root",
                str(root),
                *extra_arguments,
            ],
        )
    for directory in UNIT_TEST_DIRECTORIES:
        run(
            root,
            f"test {directory}",
            [sys.executable, "-m", "unittest", "discover", "-s", directory, "-p", "test_*.py"],
        )

    print(
        json.dumps(
            {
                "schemaVersion": "synara.stage6-engineering-checks.v1",
                "staticChecks": len(STATIC_CHECKS),
                "unitTestSuites": len(UNIT_TEST_DIRECTORIES),
                "assessment": "stage6-engineering-source-gates-passed-not-ga-approved",
            },
            indent=2,
            sort_keys=True,
        )
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
