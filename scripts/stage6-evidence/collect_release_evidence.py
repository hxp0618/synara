#!/usr/bin/env python3
"""Collect immutable Stage 6 release evidence references without deciding control outcomes."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
import pathlib
import re
import secrets
import subprocess
import sys
import urllib.parse
from dataclasses import dataclass


SHA256_RE = re.compile(r"^(?:[^\s@]+@)?sha256:([0-9a-f]{64})$")
COMMIT_RE = re.compile(r"^[0-9a-f]{40}$")
CONTROL_RE = re.compile(r"^[a-z0-9][a-z0-9._-]{1,79}$")
IDENTIFIER_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._:/-]{1,159}$")
ENVIRONMENT_CLASSES = {"production", "production-like", "staging", "fixture"}
DESKTOP_ARTIFACT_NAMES = (
    "linux-x64",
    "macos-arm64",
    "macos-x64",
    "windows-x64",
)


class EvidenceError(Exception):
    pass


@dataclass(frozen=True)
class EvidenceReference:
    control_id: str
    path: pathlib.Path


def run_git(root: pathlib.Path, *arguments: str) -> str:
    result = subprocess.run(
        ["git", "-C", str(root), *arguments],
        check=False,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    if result.returncode != 0:
        raise EvidenceError(result.stderr.strip() or "git command failed")
    return result.stdout.strip()


def sha256_file(path: pathlib.Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def write_exclusive(path: pathlib.Path, encoded: bytes) -> None:
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        offset = 0
        while offset < len(encoded):
            offset += os.write(descriptor, encoded[offset:])
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def temporary_path(output: pathlib.Path, label: str) -> pathlib.Path:
    return output.parent / f".{output.name}.{label}.{os.getpid()}.{secrets.token_hex(6)}.tmp"


def publish_release_evidence(output: pathlib.Path, encoded: bytes) -> None:
    output = output.absolute()
    sidecar = output.with_suffix(output.suffix + ".sha256")
    output.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    if output.parent.is_symlink() or not output.parent.is_dir():
        raise EvidenceError("output parent must be a regular non-symlink directory")
    if output.exists() or output.is_symlink() or sidecar.exists() or sidecar.is_symlink():
        raise EvidenceError("output and SHA-256 sidecar must not already exist")
    digest = hashlib.sha256(encoded).hexdigest()
    sidecar_encoded = f"{digest}  {output.name}\n".encode("utf-8")
    manifest_temp = temporary_path(output, "manifest")
    sidecar_temp = temporary_path(output, "sidecar")
    published_output = False
    try:
        write_exclusive(manifest_temp, encoded)
        write_exclusive(sidecar_temp, sidecar_encoded)
        os.link(manifest_temp, output, follow_symlinks=False)
        published_output = True
        try:
            os.link(sidecar_temp, sidecar, follow_symlinks=False)
        except Exception:
            output.unlink(missing_ok=True)
            published_output = False
            raise
        os.chmod(output, 0o600)
        os.chmod(sidecar, 0o600)
    except OSError as error:
        sidecar.unlink(missing_ok=True)
        if published_output:
            output.unlink(missing_ok=True)
            published_output = False
        raise EvidenceError(f"release evidence publication failed: {error}") from error
    finally:
        manifest_temp.unlink(missing_ok=True)
        sidecar_temp.unlink(missing_ok=True)
        if published_output and not sidecar.exists():
            output.unlink(missing_ok=True)


def verify_source_identity_after_collection(
    args: argparse.Namespace, manifest: dict[str, object]
) -> None:
    root = pathlib.Path(args.repository_root).resolve()
    source = manifest["source"]
    assert isinstance(source, dict)
    if run_git(root, "rev-parse", "--verify", "HEAD^{commit}") != source["commit"]:
        raise EvidenceError("repository HEAD changed while release evidence was collected")
    if not args.allow_dirty:
        dirty = run_git(root, "status", "--porcelain=v1", "--untracked-files=all")
        if dirty:
            raise EvidenceError("repository changed while release evidence was collected")


def normalize_digest(value: str, label: str) -> str:
    match = SHA256_RE.fullmatch(value.strip())
    if match is None:
        raise EvidenceError(f"{label} must be a sha256:<64 lowercase hex> digest or image@digest reference")
    return "sha256:" + match.group(1)


def normalize_identifier(value: str, label: str) -> str:
    normalized = value.strip()
    if IDENTIFIER_RE.fullmatch(normalized) is None:
        raise EvidenceError(f"{label} must be a bounded identifier")
    return normalized


def normalize_https_url(value: str, label: str, *, origin_only: bool = False) -> str:
    parsed = urllib.parse.urlsplit(value.strip())
    if (
        parsed.scheme != "https"
        or not parsed.hostname
        or parsed.username is not None
        or parsed.password is not None
        or parsed.query
        or parsed.fragment
        or (origin_only and parsed.path not in {"", "/"})
    ):
        suffix = " origin" if origin_only else " URL"
        raise EvidenceError(f"{label} must be a credential-free HTTPS{suffix}")
    try:
        port = parsed.port
    except ValueError as error:
        raise EvidenceError(f"{label} has an invalid port") from error
    host = parsed.hostname.lower()
    if ":" in host and not host.startswith("["):
        host = f"[{host}]"
    authority = host if port is None or port == 443 else f"{host}:{port}"
    path = "" if origin_only else parsed.path.rstrip("/")
    return f"https://{authority}{path}"


def repository_path_without_symlinks(
    root: pathlib.Path,
    raw_path: str,
    label: str,
) -> pathlib.Path:
    supplied = pathlib.Path(raw_path)
    if supplied.is_absolute():
        try:
            relative = supplied.relative_to(root)
        except ValueError as error:
            raise EvidenceError(f"{label} escapes the repository root") from error
    else:
        relative = supplied
    if not relative.parts or any(part == ".." for part in relative.parts):
        raise EvidenceError(f"{label} escapes the repository root")
    candidate = root
    for part in relative.parts:
        candidate /= part
        if candidate.is_symlink():
            raise EvidenceError(f"{label} must not use a symbolic link")
    try:
        candidate.resolve().relative_to(root)
    except (OSError, ValueError) as error:
        raise EvidenceError(f"{label} escapes the repository root") from error
    return candidate


def parse_evidence(value: str, root: pathlib.Path) -> EvidenceReference:
    control_id, separator, raw_path = value.partition("=")
    if separator == "" or CONTROL_RE.fullmatch(control_id) is None:
        raise EvidenceError("evidence must use control-id=repository-relative-file")
    path = repository_path_without_symlinks(root, raw_path, f"evidence {control_id}")
    if not path.is_file():
        raise EvidenceError(f"evidence {control_id} must reference a regular non-symlink file")
    return EvidenceReference(control_id=control_id, path=path)


def collect_migrations(root: pathlib.Path, migration_directory: str) -> list[dict[str, str]]:
    directory = repository_path_without_symlinks(root, migration_directory, "migration directory")
    if not directory.is_dir():
        raise EvidenceError("migration directory does not exist")
    migrations = sorted(directory.glob("*.sql"), key=lambda item: item.name)
    if not migrations:
        raise EvidenceError("migration directory contains no SQL migrations")
    if any(migration.is_symlink() or not migration.is_file() for migration in migrations):
        raise EvidenceError("migration files must be regular non-symlink files")
    return [
        {
            "path": migration.relative_to(root).as_posix(),
            "sha256": sha256_file(migration),
        }
        for migration in migrations
    ]


def parse_desktop_artifacts(values: list[str]) -> dict[str, str]:
    artifacts: dict[str, str] = {}
    allowed = set(DESKTOP_ARTIFACT_NAMES)
    for value in values:
        name, separator, digest = value.partition("=")
        if separator == "" or name not in allowed:
            raise EvidenceError(
                "desktop artifact must use one of "
                + ", ".join(DESKTOP_ARTIFACT_NAMES)
                + " as name=sha256:<64 lowercase hex>"
            )
        if name in artifacts:
            raise EvidenceError(f"duplicate desktop artifact {name}")
        artifacts[name] = normalize_digest(digest, f"desktop artifact {name}")
    missing = sorted(allowed - set(artifacts))
    if missing:
        raise EvidenceError("missing desktop artifacts: " + ", ".join(missing))
    return dict(sorted(artifacts.items()))


def collect(args: argparse.Namespace) -> dict[str, object]:
    root = pathlib.Path(args.repository_root).resolve()
    if not root.is_dir():
        raise EvidenceError("repository root does not exist")
    head_commit = run_git(root, "rev-parse", "--verify", "HEAD^{commit}")
    commit = args.commit or head_commit
    if COMMIT_RE.fullmatch(commit) is None:
        raise EvidenceError("commit must be a full 40-character lowercase Git SHA")
    if commit != head_commit:
        raise EvidenceError("commit must match the exact current Git HEAD")
    if not args.allow_dirty:
        dirty = run_git(root, "status", "--porcelain=v1", "--untracked-files=all")
        if dirty:
            raise EvidenceError("repository is dirty; collect release evidence from an exact clean commit")
    regions = sorted(set(region.strip() for region in args.region if region.strip()))
    if not regions:
        raise EvidenceError("at least one non-empty region is required")
    environment_id = normalize_identifier(args.environment_id, "environment ID")
    control_plane_base_url = normalize_https_url(
        args.control_plane_base_url, "Control Plane base URL"
    )
    web_base_url = normalize_https_url(args.web_base_url, "Web base URL", origin_only=True)
    admin_base_url = normalize_https_url(
        args.admin_base_url, "Platform Admin base URL", origin_only=True
    )
    if web_base_url == admin_base_url:
        raise EvidenceError("Web and Platform Admin must use distinct origins")
    lockfile = repository_path_without_symlinks(root, "bun.lock", "bun.lock")
    if not lockfile.is_file():
        raise EvidenceError("bun.lock must be a regular non-symlink file")
    references = [parse_evidence(value, root) for value in args.evidence]
    release = normalize_identifier(args.release, "release")
    controls: dict[str, dict[str, str]] = {}
    for reference in references:
        if reference.control_id in controls:
            raise EvidenceError(f"duplicate evidence control {reference.control_id}")
        controls[reference.control_id] = {
            "status": "collected",
            "path": reference.path.relative_to(root).as_posix(),
            "sha256": sha256_file(reference.path),
        }
    migrations = collect_migrations(root, args.migration_directory)
    generated_at = args.generated_at or dt.datetime.now(dt.timezone.utc).isoformat().replace("+00:00", "Z")
    return {
        "schemaVersion": "synara-stage6-release-evidence-v2",
        "release": release,
        "source": {
            "commit": commit,
            "clean": not args.allow_dirty,
            "lockfileSha256": sha256_file(lockfile),
        },
        "artifacts": {
            "controlPlaneImage": normalize_digest(args.control_plane_image, "control-plane image"),
            "workerImage": normalize_digest(args.worker_image, "worker image"),
            "providerHostImage": normalize_digest(args.provider_host_image, "Provider Host image"),
            "webArtifact": normalize_digest(args.web_artifact, "web artifact"),
            "adminArtifact": normalize_digest(args.admin_artifact, "Platform Admin artifact"),
            "desktopArtifacts": parse_desktop_artifacts(args.desktop_artifact),
        },
        "deployment": {
            "environmentClass": args.environment_class,
            "environmentId": environment_id,
            "regions": regions,
            "origins": {
                "controlPlaneBaseUrl": control_plane_base_url,
                "webBaseUrl": web_base_url,
                "adminBaseUrl": admin_base_url,
            },
        },
        "migrations": {
            "tail": migrations[-1],
            "count": len(migrations),
            "files": migrations,
        },
        "controls": controls,
        "generatedAt": generated_at,
        "assessment": "evidence-collected-not-control-passed",
    }


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--release", required=True)
    parser.add_argument("--repository-root", default=".")
    parser.add_argument("--commit")
    parser.add_argument("--migration-directory", default="services/control-plane/migrations")
    parser.add_argument("--control-plane-image", required=True)
    parser.add_argument("--worker-image", required=True)
    parser.add_argument("--provider-host-image", required=True)
    parser.add_argument("--web-artifact", required=True)
    parser.add_argument("--admin-artifact", required=True)
    parser.add_argument(
        "--desktop-artifact",
        action="append",
        required=True,
        help="repeat name=sha256:digest for macos-arm64, macos-x64, windows-x64 and linux-x64",
    )
    parser.add_argument("--environment-class", choices=sorted(ENVIRONMENT_CLASSES), required=True)
    parser.add_argument("--environment-id", required=True)
    parser.add_argument("--control-plane-base-url", required=True)
    parser.add_argument("--web-base-url", required=True)
    parser.add_argument("--admin-base-url", required=True)
    parser.add_argument("--region", action="append", required=True)
    parser.add_argument("--evidence", action="append", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--generated-at", help=argparse.SUPPRESS)
    parser.add_argument("--allow-dirty", action="store_true")
    return parser


def main() -> int:
    parser = build_parser()
    args = parser.parse_args()
    try:
        manifest = collect(args)
        verify_source_identity_after_collection(args, manifest)
        output = pathlib.Path(args.output).absolute()
        encoded = (json.dumps(manifest, indent=2, sort_keys=True) + "\n").encode("utf-8")
        publish_release_evidence(output, encoded)
    except EvidenceError as error:
        parser.exit(2, f"evidence collection failed: {error}\n")
    print(f"wrote {output} and {output.name}.sha256")
    return 0


if __name__ == "__main__":
    sys.exit(main())
