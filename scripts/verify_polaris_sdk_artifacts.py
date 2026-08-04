from __future__ import annotations

import argparse
import hashlib
import json
import tarfile
import zipfile
from pathlib import Path, PurePosixPath
from typing import Any


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("artifact_root", type=Path)
    parser.add_argument("--npm-version", required=True)
    parser.add_argument("--python-version", required=True)
    arguments = parser.parse_args()
    result = verify_artifacts(
        arguments.artifact_root,
        npm_version=arguments.npm_version,
        python_version=arguments.python_version,
    )
    output = arguments.artifact_root / "artifact-manifest.json"
    output.write_text(json.dumps(result, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    print(f"Verified {len(result['artifacts'])} Polaris SDK artifacts.")


def verify_artifacts(root: Path, *, npm_version: str, python_version: str) -> dict[str, Any]:
    if not root.is_dir() or root.is_symlink():
        raise ValueError("artifact root must be a real directory")
    npm = exactly_one(root / "npm", "*.tgz")
    wheel = exactly_one(root / "python", "*.whl")
    sdist = exactly_one(root / "python", "*.tar.gz")
    verify_npm(npm, npm_version)
    verify_wheel(wheel, python_version)
    verify_sdist(sdist, python_version)
    artifacts = []
    for path, kind in ((npm, "npm"), (wheel, "python-wheel"), (sdist, "python-sdist")):
        value = path.read_bytes()
        artifacts.append(
            {
                "kind": kind,
                "path": path.relative_to(root).as_posix(),
                "bytes": len(value),
                "sha256": "sha256:" + hashlib.sha256(value).hexdigest(),
            }
        )
    return {
        "schemaVersion": "polaris.sdk-release-artifacts.v1",
        "npmVersion": npm_version,
        "pythonVersion": python_version,
        "artifacts": artifacts,
    }


def verify_npm(path: Path, version: str) -> None:
    required = {
        "package/package.json",
        "package/README.md",
        "package/dist/index.mjs",
        "package/dist/index.cjs",
        "package/dist/index.d.mts",
        "package/dist/index.d.cts",
    }
    with tarfile.open(path, "r:gz") as archive:
        names = safe_archive_names(member.name for member in archive.getmembers() if member.isfile())
        if names != required:
            raise ValueError(f"npm artifact contents differ from the release allowlist: {sorted(names)}")
        manifest_file = archive.extractfile("package/package.json")
        if manifest_file is None:
            raise ValueError("npm artifact has no package.json")
        manifest = json.loads(manifest_file.read())
    if (
        manifest.get("name") != "@polaris-agents/sdk"
        or manifest.get("version") != version
        or manifest.get("private") is not False
        or "scripts" in manifest
        or "devDependencies" in manifest
    ):
        raise ValueError("npm artifact manifest is not the bounded public SDK manifest")


def verify_wheel(path: Path, version: str) -> None:
    required_modules = {
        "polaris_agents/__init__.py",
        "polaris_agents/_client.py",
        "polaris_agents/_errors.py",
        "polaris_agents/_generated.py",
        "polaris_agents/_transport.py",
    }
    with zipfile.ZipFile(path) as archive:
        names = safe_archive_names(name for name in archive.namelist() if not name.endswith("/"))
        if not required_modules.issubset(names):
            raise ValueError("Python wheel omits a required Polaris module")
        metadata_names = [name for name in names if name.endswith(".dist-info/METADATA")]
        if len(metadata_names) != 1:
            raise ValueError("Python wheel must contain exactly one METADATA file")
        metadata = archive.read(metadata_names[0]).decode("utf-8")
    reject_generated_cache(names)
    if "Name: polaris-agents\n" not in metadata or f"Version: {version}\n" not in metadata:
        raise ValueError("Python wheel metadata does not match the release version")


def verify_sdist(path: Path, version: str) -> None:
    prefix = f"polaris_agents-{version}/"
    with tarfile.open(path, "r:gz") as archive:
        names = safe_archive_names(member.name for member in archive.getmembers() if member.isfile())
    reject_generated_cache(names)
    required = {
        prefix + "pyproject.toml",
        prefix + "README.md",
        prefix + "src/polaris_agents/__init__.py",
        prefix + "src/polaris_agents/_generated.py",
    }
    if not required.issubset(names) or any(not name.startswith(prefix) for name in names):
        raise ValueError("Python sdist contents or root prefix are invalid")


def safe_archive_names(values: Any) -> set[str]:
    names: set[str] = set()
    for raw in values:
        if not isinstance(raw, str) or raw == "":
            raise ValueError("archive paths must be non-empty strings")
        name = PurePosixPath(raw)
        if name.is_absolute() or ".." in name.parts or raw in names:
            raise ValueError(f"unsafe or duplicate archive path: {raw}")
        names.add(raw)
    return names


def reject_generated_cache(names: set[str]) -> None:
    if any("__pycache__" in name or name.endswith((".pyc", ".pyo")) for name in names):
        raise ValueError("Python artifact contains generated interpreter cache")


def exactly_one(root: Path, pattern: str) -> Path:
    matches = sorted(root.glob(pattern))
    if len(matches) != 1 or not matches[0].is_file() or matches[0].is_symlink():
        raise ValueError(f"expected exactly one regular {pattern} artifact under {root}")
    return matches[0]


if __name__ == "__main__":
    main()
