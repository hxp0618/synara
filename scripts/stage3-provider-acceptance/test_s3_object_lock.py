from __future__ import annotations

import hashlib
import json
import os
import pathlib
import subprocess
import sys
import tempfile
import unittest
from typing import Any
from unittest import mock

SCRIPT_DIR = pathlib.Path(__file__).resolve().parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

import s3_object_lock as lock


def completed(
    arguments: list[str] | tuple[str, ...],
    *,
    returncode: int = 0,
    stdout: bytes = b"",
    stderr: bytes = b"",
) -> subprocess.CompletedProcess[bytes]:
    return subprocess.CompletedProcess(arguments, returncode, stdout=stdout, stderr=stderr)


def json_lines(*payloads: dict[str, Any]) -> bytes:
    return b"".join(json.dumps(payload).encode("utf-8") + b"\n" for payload in payloads)


def versioning_payload(status: str = "Enabled") -> dict[str, Any]:
    return {
        "status": "success",
        "versioning": {"status": status, "MFADelete": ""},
    }


def default_retention_payload(
    *,
    mode: str = "COMPLIANCE",
    validity: str = "365DAYS",
) -> dict[str, Any]:
    return {
        "status": "success",
        "mode": mode,
        "validity": validity,
    }


def object_stat_payload(
    *,
    object_key: str,
    version_id: str = "3Jr2x6fqlBUsVzbvPihBO3HgNpgZgAnp",
    etag: str = "9a0364b9e99bb480dd25e1f0284c8555",
    retain_until: str = "2027-07-19T00:00:00Z",
    metadata_entry_sha256: str | None = None,
) -> dict[str, Any]:
    metadata = {
        "X-Amz-Object-Lock-Mode": "COMPLIANCE",
        "X-Amz-Object-Lock-Retain-Until-Date": retain_until,
    }
    if metadata_entry_sha256 is not None:
        metadata["X-Amz-Meta-Synara-Entry-Sha256"] = metadata_entry_sha256
    return {
        "status": "success",
        "name": pathlib.Path(object_key).name,
        "etag": etag,
        "versionID": version_id,
        "metadata": metadata,
    }


def object_retention_payload(
    *,
    version_id: str = "3Jr2x6fqlBUsVzbvPihBO3HgNpgZgAnp",
    until: str = "2027-07-19T00:00:00Z",
    mode: str = "COMPLIANCE",
) -> dict[str, Any]:
    return {
        "status": "success",
        "versionID": version_id,
        "mode": mode,
        "until": until,
    }


def blocked_probe_payloads() -> bytes:
    return json_lines(
        {
            "status": "failure",
            "error": {
                "code": "ObjectLocked",
                "cause": {"code": "WORM"},
            },
        },
        {
            "status": "error",
            "error": {
                "code": "AccessDenied",
            },
        },
    )


class FakeMcRunner:
    def __init__(self, responses: list[subprocess.CompletedProcess[bytes]]) -> None:
        self.responses = list(responses)
        self.calls: list[dict[str, Any]] = []

    def __call__(
        self,
        arguments: list[str] | tuple[str, ...],
        *,
        cwd: pathlib.Path,
        check: bool,
        capture_output: bool,
        input: bytes | None = None,
        timeout: float | None = None,
        env: dict[str, str] | None = None,
    ) -> subprocess.CompletedProcess[bytes]:
        del check, capture_output
        if not self.responses:
            raise AssertionError(f"unexpected subprocess invocation: {arguments}")
        response = self.responses.pop(0)
        self.calls.append(
            {
                "args": list(arguments),
                "cwd": cwd,
                "input": input,
                "timeout": timeout,
                "env": dict(env or {}),
            }
        )
        return response


class ObjectLockClientTest(unittest.TestCase):
    def make_options(
        self,
        repo_root: pathlib.Path,
        config_dir: pathlib.Path,
        **overrides: Any,
    ) -> lock.S3ObjectLockOptions:
        payload: dict[str, Any] = {
            "repo_root": repo_root,
            "config_dir": config_dir,
            "alias": "fixture",
            "bucket": "stage3-object-lock",
            "prefix": "acceptance/probe",
            "retention_days": 365,
            "timeout_seconds": 12.0,
            "resolve": (
                "host.docker.internal:19443=127.0.0.1",
                "minio.example.test:443=10.0.0.5",
            ),
        }
        payload.update(overrides)
        return lock.S3ObjectLockOptions(**payload)

    def test_verify_returns_exact_version_evidence_and_blocked_negative_probes(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            repo_root = root / "repo"
            repo_root.mkdir()
            config_dir = root / "mc-config"
            config_dir.mkdir()
            options = self.make_options(repo_root, config_dir)
            object_key = f"{options.prefix}/{lock.CANONICAL_OBJECT_BASENAME}"
            runner = FakeMcRunner(
                [
                    completed(["mc"], stdout=json_lines(versioning_payload())),
                    completed(["mc"], stdout=json_lines(default_retention_payload())),
                    completed(["mc"], stdout=json_lines(versioning_payload())),
                    completed(["mc"], stdout=json_lines(default_retention_payload())),
                    completed(
                        ["mc"],
                        stdout=json_lines(
                            {
                                "status": "success",
                                "target": f"{options.alias}/{options.bucket}/{object_key}",
                                "size": len(lock.CANONICAL_OBJECT_BYTES),
                            }
                        ),
                    ),
                    completed(["mc"], stdout=json_lines(object_stat_payload(object_key=object_key))),
                    completed(["mc"], stdout=json_lines(object_retention_payload())),
                    completed(["mc"], stdout=json_lines(object_stat_payload(object_key=object_key))),
                    completed(["mc"], stdout=lock.CANONICAL_OBJECT_BYTES),
                    completed(["mc"], returncode=1, stdout=blocked_probe_payloads()),
                    completed(["mc"], returncode=1, stdout=blocked_probe_payloads()),
                ]
            )

            with (
                mock.patch.dict(
                    os.environ,
                    {
                        "MC_HOST_fixture": "https://ACCESSKEY:secret-value@example.invalid",
                    },
                    clear=False,
                ),
                mock.patch.object(lock.subprocess, "run", side_effect=runner),
            ):
                result = lock.verify_object_lock(options)

        self.assertEqual(result.object_key, object_key)
        self.assertEqual(result.version_id, "3Jr2x6fqlBUsVzbvPihBO3HgNpgZgAnp")
        self.assertEqual(result.etag, "9a0364b9e99bb480dd25e1f0284c8555")
        self.assertEqual(result.content_sha256, lock.CANONICAL_OBJECT_SHA256)
        self.assertEqual(result.retain_until, "2027-07-19T00:00:00Z")
        self.assertEqual(result.versioning_status, "Enabled")
        self.assertEqual(result.default_retention_mode, "COMPLIANCE")
        self.assertEqual(result.default_retention_days, 365)
        self.assertTrue(result.delete_probe.blocked)
        self.assertTrue(result.shorten_retention_probe.blocked)
        self.assertEqual(result.delete_probe.denial_kind, "object_lock")
        self.assertEqual(result.shorten_retention_probe.denial_kind, "object_lock")
        self.assertEqual(result.delete_probe.statuses, ("failure", "error"))
        self.assertEqual(result.delete_probe.error_codes, ("ObjectLocked", "WORM", "AccessDenied"))
        self.assertEqual(runner.calls[4]["input"], lock.CANONICAL_OBJECT_BYTES)
        for call in runner.calls:
            self.assertEqual(call["cwd"], repo_root.resolve())
            self.assertEqual(call["timeout"], 12.0)
            self.assertIn("--config-dir", call["args"])
            self.assertIn(str(config_dir.resolve()), call["args"])
            self.assertIn("--quiet", call["args"])
            self.assertIn("--disable-pager", call["args"])
            self.assertEqual(call["args"].count("--resolve"), 2)
            self.assertNotIn("secret-value", " ".join(call["args"]))
            self.assertEqual(
                call["env"].get("MC_HOST_fixture"),
                "https://ACCESSKEY:secret-value@example.invalid",
            )

    def test_put_bytes_returns_exact_version_evidence(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            repo_root = root / "repo"
            repo_root.mkdir()
            config_dir = root / "mc-config"
            config_dir.mkdir()
            options = self.make_options(repo_root, config_dir)
            client = lock.S3ObjectLockClient(options)
            object_key = f"{options.prefix}/custom.ndjson"
            content = b'{"ok":true}\n'
            expected_sha256 = hashlib.sha256(content).hexdigest()
            runner = FakeMcRunner(
                [
                    completed(["mc"], stdout=json_lines(versioning_payload())),
                    completed(["mc"], stdout=json_lines(default_retention_payload())),
                    completed(
                        ["mc"],
                        stdout=json_lines(
                            {
                                "status": "success",
                                "target": f"{options.alias}/{options.bucket}/{object_key}",
                                "size": len(content),
                            }
                        ),
                    ),
                    completed(
                        ["mc"],
                        stdout=json_lines(
                            object_stat_payload(
                                object_key=object_key,
                                etag="6eff3b84fd89ca3d4b2d9ee4f3c9dd43",
                            )
                        ),
                    ),
                    completed(["mc"], stdout=json_lines(object_retention_payload())),
                    completed(
                        ["mc"],
                        stdout=json_lines(
                            object_stat_payload(
                                object_key=object_key,
                                etag="6eff3b84fd89ca3d4b2d9ee4f3c9dd43",
                            )
                        ),
                    ),
                    completed(["mc"], stdout=content),
                ]
            )

            with mock.patch.object(lock.subprocess, "run", side_effect=runner):
                evidence = client.put_bytes(object_key, content)

        self.assertEqual(evidence.object_key, object_key)
        self.assertEqual(evidence.content_sha256, expected_sha256)
        self.assertEqual(evidence.retention_mode, "COMPLIANCE")
        self.assertIsNone(evidence.metadata_entry_sha256)
        self.assertEqual(runner.calls[2]["input"], content)

    def test_verify_existing_object_accepts_required_entry_sha256(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            repo_root = root / "repo"
            repo_root.mkdir()
            config_dir = root / "mc-config"
            config_dir.mkdir()
            client = lock.S3ObjectLockClient(self.make_options(repo_root, config_dir))
            object_key = "entries/archived-batch.ndjson"
            content = b'{"entrySha256":"abc"}\n'
            content_sha256 = hashlib.sha256(content).hexdigest()
            entry_sha256 = "1" * 64
            runner = FakeMcRunner(
                [
                    completed(
                        ["mc"],
                        stdout=json_lines(
                            object_stat_payload(
                                object_key=object_key,
                                metadata_entry_sha256=entry_sha256,
                            )
                        ),
                    ),
                    completed(["mc"], stdout=json_lines(object_retention_payload())),
                    completed(
                        ["mc"],
                        stdout=json_lines(
                            object_stat_payload(
                                object_key=object_key,
                                metadata_entry_sha256=entry_sha256,
                            )
                        ),
                    ),
                    completed(["mc"], stdout=content),
                ]
            )

            with mock.patch.object(lock.subprocess, "run", side_effect=runner):
                evidence = client.verify_existing_object(
                    object_key,
                    expected_content_sha256=content_sha256,
                    required_entry_sha256=entry_sha256,
                )

        self.assertEqual(evidence.metadata_entry_sha256, entry_sha256)

    def test_rejects_bucket_when_versioning_is_not_enabled(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            repo_root = root / "repo"
            repo_root.mkdir()
            config_dir = root / "mc-config"
            config_dir.mkdir()
            runner = FakeMcRunner(
                [
                    completed(["mc"], stdout=json_lines(versioning_payload(status="Suspended"))),
                ]
            )

            with mock.patch.object(lock.subprocess, "run", side_effect=runner):
                with self.assertRaises(lock.S3ObjectLockError) as caught:
                    lock.verify_object_lock(self.make_options(repo_root, config_dir))

        self.assertEqual(caught.exception.code, "s3_object_lock.versioning_not_enabled")

    def test_rejects_bucket_when_default_retention_mode_is_not_compliance(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            repo_root = root / "repo"
            repo_root.mkdir()
            config_dir = root / "mc-config"
            config_dir.mkdir()
            runner = FakeMcRunner(
                [
                    completed(["mc"], stdout=json_lines(versioning_payload())),
                    completed(
                        ["mc"],
                        stdout=json_lines(default_retention_payload(mode="GOVERNANCE")),
                    ),
                ]
            )

            with mock.patch.object(lock.subprocess, "run", side_effect=runner):
                with self.assertRaises(lock.S3ObjectLockError) as caught:
                    lock.verify_object_lock(self.make_options(repo_root, config_dir))

        self.assertEqual(caught.exception.code, "s3_object_lock.default_retention_mode_invalid")

    def test_rejects_hash_drift_from_exact_version_cat(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            repo_root = root / "repo"
            repo_root.mkdir()
            config_dir = root / "mc-config"
            config_dir.mkdir()
            options = self.make_options(repo_root, config_dir)
            object_key = f"{options.prefix}/{lock.CANONICAL_OBJECT_BASENAME}"
            runner = FakeMcRunner(
                [
                    completed(["mc"], stdout=json_lines(versioning_payload())),
                    completed(["mc"], stdout=json_lines(default_retention_payload())),
                    completed(["mc"], stdout=json_lines(versioning_payload())),
                    completed(["mc"], stdout=json_lines(default_retention_payload())),
                    completed(
                        ["mc"],
                        stdout=json_lines(
                            {
                                "status": "success",
                                "target": f"{options.alias}/{options.bucket}/{object_key}",
                                "size": len(lock.CANONICAL_OBJECT_BYTES),
                            }
                        ),
                    ),
                    completed(["mc"], stdout=json_lines(object_stat_payload(object_key=object_key))),
                    completed(["mc"], stdout=json_lines(object_retention_payload())),
                    completed(["mc"], stdout=json_lines(object_stat_payload(object_key=object_key))),
                    completed(["mc"], stdout=b"tampered\n"),
                ]
            )

            with mock.patch.object(lock.subprocess, "run", side_effect=runner):
                with self.assertRaises(lock.S3ObjectLockError) as caught:
                    lock.verify_object_lock(options)

        self.assertEqual(caught.exception.code, "s3_object_lock.content_hash_drift")
        self.assertEqual(
            caught.exception.evidence["expectedSha256"],
            lock.CANONICAL_OBJECT_SHA256,
        )

    def test_rejects_when_delete_probe_is_unexpectedly_allowed(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            repo_root = root / "repo"
            repo_root.mkdir()
            config_dir = root / "mc-config"
            config_dir.mkdir()
            options = self.make_options(repo_root, config_dir)
            object_key = f"{options.prefix}/{lock.CANONICAL_OBJECT_BASENAME}"
            runner = FakeMcRunner(
                [
                    completed(["mc"], stdout=json_lines(versioning_payload())),
                    completed(["mc"], stdout=json_lines(default_retention_payload())),
                    completed(["mc"], stdout=json_lines(versioning_payload())),
                    completed(["mc"], stdout=json_lines(default_retention_payload())),
                    completed(
                        ["mc"],
                        stdout=json_lines(
                            {
                                "status": "success",
                                "target": f"{options.alias}/{options.bucket}/{object_key}",
                                "size": len(lock.CANONICAL_OBJECT_BYTES),
                            }
                        ),
                    ),
                    completed(["mc"], stdout=json_lines(object_stat_payload(object_key=object_key))),
                    completed(["mc"], stdout=json_lines(object_retention_payload())),
                    completed(["mc"], stdout=json_lines(object_stat_payload(object_key=object_key))),
                    completed(["mc"], stdout=lock.CANONICAL_OBJECT_BYTES),
                    completed(
                        ["mc"],
                        returncode=0,
                        stdout=json_lines({"status": "success", "versionID": "deleted"}),
                    ),
                ]
            )

            with mock.patch.object(lock.subprocess, "run", side_effect=runner):
                with self.assertRaises(lock.S3ObjectLockError) as caught:
                    lock.verify_object_lock(options)

        self.assertEqual(
            caught.exception.code,
            "s3_object_lock.delete_probe_unexpectedly_allowed",
        )

    def test_rejects_when_shorten_retention_probe_is_unexpectedly_allowed(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            repo_root = root / "repo"
            repo_root.mkdir()
            config_dir = root / "mc-config"
            config_dir.mkdir()
            options = self.make_options(repo_root, config_dir)
            object_key = f"{options.prefix}/{lock.CANONICAL_OBJECT_BASENAME}"
            runner = FakeMcRunner(
                [
                    completed(["mc"], stdout=json_lines(versioning_payload())),
                    completed(["mc"], stdout=json_lines(default_retention_payload())),
                    completed(["mc"], stdout=json_lines(versioning_payload())),
                    completed(["mc"], stdout=json_lines(default_retention_payload())),
                    completed(
                        ["mc"],
                        stdout=json_lines(
                            {
                                "status": "success",
                                "target": f"{options.alias}/{options.bucket}/{object_key}",
                                "size": len(lock.CANONICAL_OBJECT_BYTES),
                            }
                        ),
                    ),
                    completed(["mc"], stdout=json_lines(object_stat_payload(object_key=object_key))),
                    completed(["mc"], stdout=json_lines(object_retention_payload())),
                    completed(["mc"], stdout=json_lines(object_stat_payload(object_key=object_key))),
                    completed(["mc"], stdout=lock.CANONICAL_OBJECT_BYTES),
                    completed(["mc"], returncode=1, stdout=blocked_probe_payloads()),
                    completed(
                        ["mc"],
                        returncode=0,
                        stdout=json_lines({"status": "success", "versionID": "shortened"}),
                    ),
                ]
            )

            with mock.patch.object(lock.subprocess, "run", side_effect=runner):
                with self.assertRaises(lock.S3ObjectLockError) as caught:
                    lock.verify_object_lock(options)

        self.assertEqual(
            caught.exception.code,
            "s3_object_lock.shorten_retention_probe_unexpectedly_allowed",
        )

    def test_fails_closed_without_echoing_secret_output(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            repo_root = root / "repo"
            repo_root.mkdir()
            config_dir = root / "mc-config"
            config_dir.mkdir()
            secret = "s3-object-lock-secret-value"
            runner = FakeMcRunner(
                [
                    completed(["mc"], stdout=f"{secret}\n".encode("utf-8")),
                ]
            )

            with (
                mock.patch.dict(
                    os.environ,
                    {"MC_HOST_fixture": f"https://ACCESSKEY:{secret}@example.invalid"},
                    clear=False,
                ),
                mock.patch.object(lock.subprocess, "run", side_effect=runner),
            ):
                with self.assertRaises(lock.S3ObjectLockError) as caught:
                    lock.verify_object_lock(self.make_options(repo_root, config_dir))

        self.assertEqual(caught.exception.code, "s3_object_lock.command_output_invalid")
        self.assertNotIn(secret, str(caught.exception))
        self.assertNotIn(secret, str(caught.exception.evidence))

    def test_generic_access_denial_is_classified_as_iam_block(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            repo_root = root / "repo"
            repo_root.mkdir()
            config_dir = root / "mc-config"
            config_dir.mkdir()
            runner = FakeMcRunner(
                [
                    completed(
                        ["mc"],
                        returncode=1,
                        stdout=json_lines(
                            {"status": "error", "error": {"code": "AccessDenied"}}
                        ),
                    )
                ]
            )
            client = lock.S3ObjectLockClient(self.make_options(repo_root, config_dir))
            with mock.patch.object(lock.subprocess, "run", side_effect=runner):
                result = client.probe_delete_version("entries/probe.json", "version-1")

        self.assertTrue(result.blocked)
        self.assertEqual(result.denial_kind, "iam")


if __name__ == "__main__":
    unittest.main()
