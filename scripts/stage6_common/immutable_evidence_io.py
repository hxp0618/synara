"""Stable input reads and immutable receipt+sidecar publication for Stage 6 tooling."""

from __future__ import annotations

import hashlib
import os
import pathlib
import secrets
import stat


class ImmutableEvidenceIOError(Exception):
    pass


def read_stable_regular_file(
    path: pathlib.Path, *, label: str, maximum_bytes: int
) -> bytes:
    try:
        metadata = path.lstat()
    except OSError as error:
        raise ImmutableEvidenceIOError(
            f"{label} must be a readable regular file"
        ) from error
    if (
        stat.S_ISLNK(metadata.st_mode)
        or not stat.S_ISREG(metadata.st_mode)
        or metadata.st_size <= 0
        or metadata.st_size > maximum_bytes
    ):
        raise ImmutableEvidenceIOError(
            f"{label} must be a non-empty bounded regular non-symlink file"
        )
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
    try:
        descriptor = os.open(path, flags)
    except OSError as error:
        raise ImmutableEvidenceIOError(
            f"{label} could not be opened without following links"
        ) from error
    try:
        opened = os.fstat(descriptor)
        if (
            not stat.S_ISREG(opened.st_mode)
            or opened.st_dev != metadata.st_dev
            or opened.st_ino != metadata.st_ino
            or opened.st_size != metadata.st_size
            or opened.st_mtime_ns != metadata.st_mtime_ns
            or opened.st_ctime_ns != metadata.st_ctime_ns
        ):
            raise ImmutableEvidenceIOError(f"{label} changed while it was being opened")
        chunks: list[bytes] = []
        observed_size = 0
        while observed_size <= maximum_bytes:
            chunk = os.read(
                descriptor, min(1024 * 1024, maximum_bytes + 1 - observed_size)
            )
            if not chunk:
                break
            chunks.append(chunk)
            observed_size += len(chunk)
        final = os.fstat(descriptor)
        if (
            observed_size != opened.st_size
            or observed_size > maximum_bytes
            or final.st_size != opened.st_size
            or final.st_mtime_ns != opened.st_mtime_ns
            or final.st_ctime_ns != opened.st_ctime_ns
        ):
            raise ImmutableEvidenceIOError(f"{label} changed while it was being read")
        return b"".join(chunks)
    finally:
        os.close(descriptor)


def _write_exclusive(path: pathlib.Path, encoded: bytes) -> None:
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        offset = 0
        while offset < len(encoded):
            offset += os.write(descriptor, encoded[offset:])
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def _temporary_path(output: pathlib.Path, label: str) -> pathlib.Path:
    return output.parent / f".{output.name}.{label}.{os.getpid()}.{secrets.token_hex(6)}.tmp"


def publish_immutable_with_sha256(output: pathlib.Path, encoded: bytes) -> None:
    output = output.absolute()
    sidecar = output.with_suffix(output.suffix + ".sha256")
    output.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    if output.parent.is_symlink() or not output.parent.is_dir():
        raise ImmutableEvidenceIOError(
            "output parent must be a regular non-symlink directory"
        )
    if output.exists() or output.is_symlink() or sidecar.exists() or sidecar.is_symlink():
        raise ImmutableEvidenceIOError(
            "output and SHA-256 sidecar must not already exist"
        )
    digest = hashlib.sha256(encoded).hexdigest()
    sidecar_encoded = f"{digest}  {output.name}\n".encode("utf-8")
    receipt_temp = _temporary_path(output, "receipt")
    sidecar_temp = _temporary_path(output, "sidecar")
    published_output = False
    try:
        _write_exclusive(receipt_temp, encoded)
        _write_exclusive(sidecar_temp, sidecar_encoded)
        os.link(receipt_temp, output, follow_symlinks=False)
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
        raise ImmutableEvidenceIOError(f"immutable evidence publication failed: {error}") from error
    finally:
        receipt_temp.unlink(missing_ok=True)
        sidecar_temp.unlink(missing_ok=True)
        if published_output and not sidecar.exists():
            output.unlink(missing_ok=True)
