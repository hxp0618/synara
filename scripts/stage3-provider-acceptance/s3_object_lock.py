from __future__ import annotations

import dataclasses
import datetime as dt
import hashlib
import json
import os
import pathlib
import re
import subprocess
from collections.abc import Mapping, Sequence
from typing import Any


CANONICAL_OBJECT_BASENAME = "synara-stage3-object-lock-v1.txt"
CANONICAL_OBJECT_BYTES = b"synara-stage3-object-lock-v1\n"
CANONICAL_OBJECT_SHA256 = hashlib.sha256(CANONICAL_OBJECT_BYTES).hexdigest()
DEFAULT_TIMEOUT_SECONDS = 15.0
_VALIDITY_PATTERN = re.compile(r"^(?P<days>[1-9][0-9]*)(?:D|DAY|DAYS)$", re.IGNORECASE)
_SAFE_TOKEN_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")
_SAFE_ENVIRONMENT_NAME_PATTERN = re.compile(r"^[A-Za-z_][A-Za-z0-9_]{0,127}$")
_SHA256_PATTERN = re.compile(r"[0-9a-f]{64}")
ENTRY_SHA256_METADATA_KEYS = (
    "X-Amz-Meta-Synara-Entry-Sha256",
    "X-Amz-Meta-Entry-Sha256",
)


class S3ObjectLockError(RuntimeError):
    def __init__(
        self,
        code: str,
        message: str,
        evidence: Mapping[str, Any] | None = None,
    ) -> None:
        super().__init__(message)
        self.code = code
        self.evidence = dict(evidence or {})


@dataclasses.dataclass(frozen=True)
class S3ObjectLockOptions:
    repo_root: pathlib.Path
    config_dir: pathlib.Path
    alias: str
    bucket: str
    prefix: str
    retention_days: int
    mc_bin: str = "mc"
    timeout_seconds: float = DEFAULT_TIMEOUT_SECONDS
    shorten_probe_days: int = 1
    resolve: tuple[str, ...] = ()
    subprocess_environment: Mapping[str, str] = dataclasses.field(
        default_factory=dict,
        repr=False,
        compare=False,
    )


@dataclasses.dataclass(frozen=True)
class BucketContract:
    versioning_status: str
    default_retention_mode: str
    default_retention_days: int


@dataclasses.dataclass(frozen=True)
class ObjectVersionEvidence:
    object_key: str
    version_id: str
    etag: str
    content_sha256: str
    retain_until: str
    retention_mode: str
    metadata_entry_sha256: str | None = None


@dataclasses.dataclass(frozen=True)
class NegativeProbeResult:
    operation: str
    blocked: bool
    return_code: int
    statuses: tuple[str, ...]
    error_codes: tuple[str, ...]
    denial_kind: str | None = None


@dataclasses.dataclass(frozen=True)
class ObjectLockVerificationResult:
    object_key: str
    version_id: str
    etag: str
    content_sha256: str
    retain_until: str
    versioning_status: str
    default_retention_mode: str
    default_retention_days: int
    delete_probe: NegativeProbeResult
    shorten_retention_probe: NegativeProbeResult


@dataclasses.dataclass(frozen=True)
class _McJsonResult:
    return_code: int
    payloads: tuple[dict[str, Any], ...]


def verify_object_lock(options: S3ObjectLockOptions) -> ObjectLockVerificationResult:
    return S3ObjectLockClient(options).verify()


class S3ObjectLockClient:
    def __init__(self, options: S3ObjectLockOptions) -> None:
        self.options = _normalize_options(options)

    @property
    def object_key(self) -> str:
        return self.qualify_object_key(CANONICAL_OBJECT_BASENAME)

    def qualify_object_key(self, basename: str) -> str:
        safe_basename = _normalize_object_key(basename)
        return f"{self.options.prefix}/{safe_basename}" if self.options.prefix else safe_basename

    def verify(self) -> ObjectLockVerificationResult:
        contract = self.verify_bucket_contract()
        evidence = self.put_bytes(self.object_key, CANONICAL_OBJECT_BYTES)
        delete_probe = self.probe_delete_version(evidence.object_key, evidence.version_id)
        if not delete_probe.blocked or delete_probe.denial_kind != "object_lock":
            raise S3ObjectLockError(
                "s3_object_lock.delete_probe_unexpectedly_allowed",
                "Deleting the exact retained object version unexpectedly succeeded.",
                {
                    "statuses": list(delete_probe.statuses),
                    "errorCodes": list(delete_probe.error_codes),
                    "denialKind": delete_probe.denial_kind,
                },
            )
        shorten_probe = self.probe_shorten_retention(evidence.object_key, evidence.version_id)
        if not shorten_probe.blocked or shorten_probe.denial_kind != "object_lock":
            raise S3ObjectLockError(
                "s3_object_lock.shorten_retention_probe_unexpectedly_allowed",
                "Shortening the exact retained object version unexpectedly succeeded.",
                {
                    "statuses": list(shorten_probe.statuses),
                    "errorCodes": list(shorten_probe.error_codes),
                    "denialKind": shorten_probe.denial_kind,
                },
            )
        return ObjectLockVerificationResult(
            object_key=evidence.object_key,
            version_id=evidence.version_id,
            etag=evidence.etag,
            content_sha256=evidence.content_sha256,
            retain_until=evidence.retain_until,
            versioning_status=contract.versioning_status,
            default_retention_mode=contract.default_retention_mode,
            default_retention_days=contract.default_retention_days,
            delete_probe=delete_probe,
            shorten_retention_probe=shorten_probe,
        )

    def put_bytes(self, object_key: str, content: bytes) -> ObjectVersionEvidence:
        expected_content_sha256 = self.upload_bytes(object_key, content)
        return self.verify_existing_object(
            object_key,
            expected_content_sha256=expected_content_sha256,
        )

    def verify_bucket_contract(self) -> BucketContract:
        versioning = self._single_success_payload(
            self._run_json("version info", ["version", "info", self._bucket_target()]),
            code="s3_object_lock.version_info_invalid",
            message="The bucket versioning response was missing or malformed.",
        )
        versioning_map = _require_mapping(
            versioning.get("versioning"),
            code="s3_object_lock.versioning_missing",
            message="The bucket versioning response omitted versioning status.",
        )
        versioning_status = _require_string(
            versioning_map.get("status"),
            code="s3_object_lock.versioning_missing",
            message="The bucket versioning response omitted versioning status.",
        )
        if versioning_status != "Enabled":
            raise S3ObjectLockError(
                "s3_object_lock.versioning_not_enabled",
                "The bucket versioning boundary was not Enabled.",
                {"status": versioning_status},
            )
        retention = self._single_success_payload(
            self._run_json(
                "retention info",
                ["retention", "info", "--default", self._bucket_target(with_trailing_slash=True)],
            ),
            code="s3_object_lock.default_retention_invalid",
            message="The bucket default retention response was missing or malformed.",
        )
        mode = _require_string(
            retention.get("mode"),
            code="s3_object_lock.default_retention_invalid",
            message="The bucket default retention response omitted the retention mode.",
        )
        if mode != "COMPLIANCE":
            raise S3ObjectLockError(
                "s3_object_lock.default_retention_mode_invalid",
                "The bucket default retention boundary was not COMPLIANCE.",
                {"mode": mode},
            )
        validity = _require_string(
            retention.get("validity"),
            code="s3_object_lock.default_retention_invalid",
            message="The bucket default retention response omitted the retention validity.",
        )
        match = _VALIDITY_PATTERN.fullmatch(validity)
        if match is None:
            raise S3ObjectLockError(
                "s3_object_lock.default_retention_validity_invalid",
                "The bucket default retention validity was not an exact whole-day value.",
            )
        days = int(match.group("days"))
        if days != self.options.retention_days:
            raise S3ObjectLockError(
                "s3_object_lock.default_retention_days_mismatch",
                "The bucket default retention boundary did not match the required exact day count.",
                {"expectedDays": self.options.retention_days, "actualDays": days},
            )
        return BucketContract(
            versioning_status=versioning_status,
            default_retention_mode=mode,
            default_retention_days=days,
        )

    def upload_bytes(self, object_key: str, content: bytes) -> str:
        normalized_key = _normalize_object_key(object_key)
        if not isinstance(content, bytes) or not content:
            raise ValueError("content must be non-empty bytes")
        self.verify_bucket_contract()
        payload = self._single_success_payload(
            self._run_json(
                "pipe",
                ["pipe", self._object_target(normalized_key)],
                input_bytes=content,
            ),
            code="s3_object_lock.upload_invalid",
            message="The retained object upload did not return a valid success payload.",
        )
        if payload.get("size") != len(content):
            raise S3ObjectLockError(
                "s3_object_lock.upload_size_mismatch",
                "The retained object upload returned an unexpected byte count.",
                {"expectedSize": len(content), "actualSize": payload.get("size")},
            )
        return hashlib.sha256(content).hexdigest()

    def verify_existing_object(
        self,
        object_key: str,
        *,
        expected_content_sha256: str,
        version_id: str | None = None,
        required_entry_sha256: str | None = None,
    ) -> ObjectVersionEvidence:
        normalized_key = _normalize_object_key(object_key)
        if _SHA256_PATTERN.fullmatch(expected_content_sha256) is None:
            raise ValueError("expected_content_sha256 must be a lowercase SHA-256 digest")
        normalized_expected_sha256 = expected_content_sha256.lower()
        normalized_required_entry_sha256: str | None = None
        if required_entry_sha256 is not None:
            if _SHA256_PATTERN.fullmatch(required_entry_sha256) is None:
                raise ValueError("required_entry_sha256 must be a lowercase SHA-256 digest")
            normalized_required_entry_sha256 = required_entry_sha256.lower()
        current_stat = self._stat_object(normalized_key, version_id=version_id)
        resolved_version_id = _require_string(
            current_stat.get("versionID"),
            code="s3_object_lock.version_id_missing",
            message="The retained object did not expose a concrete version ID.",
        )
        if version_id is not None and resolved_version_id != version_id:
            raise S3ObjectLockError(
                "s3_object_lock.version_id_drift",
                "The exact-version stat response returned a different version ID.",
            )
        retention = self._retention_info(normalized_key, version_id=resolved_version_id)
        exact_stat = self._stat_object(normalized_key, version_id=resolved_version_id)
        exact_version_id = _require_string(
            exact_stat.get("versionID"),
            code="s3_object_lock.version_id_drift",
            message="The exact-version stat response omitted the expected version ID.",
        )
        if exact_version_id != resolved_version_id:
            raise S3ObjectLockError(
                "s3_object_lock.version_id_drift",
                "The exact-version stat response returned a different version ID.",
            )
        etag = _normalize_etag(
            exact_stat.get("etag"),
            code="s3_object_lock.version_etag_missing",
            message="The exact retained object version did not expose an ETag.",
        )
        metadata = _require_mapping(
            exact_stat.get("metadata"),
            code="s3_object_lock.metadata_missing",
            message="The exact-version stat response omitted object-lock metadata.",
        )
        metadata_mode = _upper_string(metadata.get("X-Amz-Object-Lock-Mode"))
        if metadata_mode != "COMPLIANCE":
            raise S3ObjectLockError(
                "s3_object_lock.metadata_mode_invalid",
                "The exact-version stat response did not preserve COMPLIANCE object-lock metadata.",
                {"mode": metadata_mode},
            )
        metadata_until = _require_string(
            metadata.get("X-Amz-Object-Lock-Retain-Until-Date"),
            code="s3_object_lock.metadata_retain_until_missing",
            message="The exact-version stat response omitted the retain-until metadata.",
        )
        retention_mode = _upper_string(retention.get("mode"))
        if retention_mode != "COMPLIANCE":
            raise S3ObjectLockError(
                "s3_object_lock.retention_mode_invalid",
                "The exact object version did not preserve COMPLIANCE retention.",
                {"mode": retention_mode},
            )
        retain_until = _require_string(
            retention.get("until"),
            code="s3_object_lock.retain_until_missing",
            message="The exact object version did not expose a retention-until timestamp.",
        )
        if _parse_timestamp(metadata_until) != _parse_timestamp(retain_until):
            raise S3ObjectLockError(
                "s3_object_lock.retain_until_drift",
                "The exact-version stat and retention responses disagreed about retention.",
            )
        content = self.cat_version(normalized_key, resolved_version_id)
        content_sha256 = hashlib.sha256(content).hexdigest()
        if content_sha256 != normalized_expected_sha256:
            raise S3ObjectLockError(
                "s3_object_lock.content_hash_drift",
                "The exact object version content drifted from the expected payload.",
                {
                    "expectedSha256": normalized_expected_sha256,
                    "actualSha256": content_sha256,
                },
            )
        metadata_entry_sha256 = _metadata_entry_sha256(metadata)
        if normalized_required_entry_sha256 is not None:
            if metadata_entry_sha256 is None:
                raise S3ObjectLockError(
                    "s3_object_lock.entry_sha256_missing",
                    "The exact retained object version omitted the required entry SHA256 metadata.",
                )
            if metadata_entry_sha256 != normalized_required_entry_sha256:
                raise S3ObjectLockError(
                    "s3_object_lock.entry_sha256_mismatch",
                    "The exact retained object version entry SHA256 metadata drifted from the required value.",
                    {
                        "expectedEntrySha256": normalized_required_entry_sha256,
                        "actualEntrySha256": metadata_entry_sha256,
                    },
                )
        return ObjectVersionEvidence(
            object_key=normalized_key,
            version_id=resolved_version_id,
            etag=etag,
            content_sha256=content_sha256,
            retain_until=retain_until,
            retention_mode=str(retention_mode),
            metadata_entry_sha256=metadata_entry_sha256,
        )

    def stat_version(self, object_key: str, version_id: str) -> Mapping[str, Any]:
        return self._stat_object(_normalize_object_key(object_key), version_id=version_id)

    def cat_version(self, object_key: str, version_id: str) -> bytes:
        command = self._base_command(json_output=False)
        command.extend(["cat", "--version-id", version_id, self._object_target(object_key)])
        completed = self._run_process(command, input_bytes=None)
        if completed.returncode != 0:
            raise S3ObjectLockError(
                "s3_object_lock.cat_failed",
                "The exact object version could not be downloaded for content verification.",
                {"returnCode": completed.returncode},
            )
        return completed.stdout

    def probe_delete_version(self, object_key: str, version_id: str) -> NegativeProbeResult:
        return self._negative_probe(
            "deleteVersion",
            ["rm", "--version-id", version_id, self._object_target(_normalize_object_key(object_key))],
        )

    def probe_shorten_retention(self, object_key: str, version_id: str) -> NegativeProbeResult:
        return self._negative_probe(
            "shortenRetention",
            [
                "retention",
                "set",
                "--version-id",
                version_id,
                "compliance",
                f"{self.options.shorten_probe_days}d",
                self._object_target(_normalize_object_key(object_key)),
            ],
        )

    def _stat_object(self, object_key: str, *, version_id: str | None = None) -> Mapping[str, Any]:
        arguments = ["stat", "--no-list"]
        if version_id is not None:
            arguments.extend(["--version-id", version_id])
        arguments.append(self._object_target(object_key))
        return self._single_success_payload(
            self._run_json("stat", arguments),
            code="s3_object_lock.stat_invalid",
            message="The object stat response was missing or malformed.",
        )

    def _retention_info(self, object_key: str, *, version_id: str) -> Mapping[str, Any]:
        return self._single_success_payload(
            self._run_json(
                "retention info",
                [
                    "retention",
                    "info",
                    "--version-id",
                    version_id,
                    self._object_target(object_key),
                ],
            ),
            code="s3_object_lock.retention_info_invalid",
            message="The exact-version retention response was missing or malformed.",
        )

    def _negative_probe(self, operation: str, arguments: Sequence[str]) -> NegativeProbeResult:
        result = self._run_json(operation, arguments, allowed_return_codes=(0, 1))
        statuses = tuple(
            value
            for payload in result.payloads
            if (value := _safe_string(payload.get("status"))) is not None
        )
        error_codes = tuple(
            code for payload in result.payloads for code in _payload_error_codes(payload)
        )
        denial_kinds = {
            kind for payload in result.payloads if (kind := _payload_denial_kind(payload)) is not None
        }
        denial_kind = "object_lock" if "object_lock" in denial_kinds else "iam" if "iam" in denial_kinds else None
        blocked = (
            result.return_code != 0
            and "success" not in statuses
            and any(status in {"failure", "error"} for status in statuses)
            and denial_kind is not None
        )
        return NegativeProbeResult(
            operation=operation,
            blocked=blocked,
            return_code=result.return_code,
            statuses=statuses,
            error_codes=error_codes,
            denial_kind=denial_kind,
        )

    def _single_success_payload(
        self,
        result: _McJsonResult,
        *,
        code: str,
        message: str,
    ) -> Mapping[str, Any]:
        payloads = [payload for payload in result.payloads if payload.get("status") == "success"]
        if result.return_code != 0 or len(payloads) != 1:
            raise S3ObjectLockError(
                code,
                message,
                {
                    "returnCode": result.return_code,
                    "payloadCount": len(result.payloads),
                    "successPayloadCount": len(payloads),
                },
            )
        return payloads[0]

    def _run_json(
        self,
        command_name: str,
        arguments: Sequence[str],
        *,
        input_bytes: bytes | None = None,
        allowed_return_codes: Sequence[int] = (0,),
    ) -> _McJsonResult:
        command = self._base_command(json_output=True)
        command.extend(arguments)
        completed = self._run_process(command, input_bytes=input_bytes)
        if completed.returncode not in set(allowed_return_codes):
            raise S3ObjectLockError(
                "s3_object_lock.command_failed",
                f"The `{command_name}` command returned an unexpected status.",
                {"returnCode": completed.returncode},
            )
        payloads = _parse_ndjson(completed.stdout, completed.stderr)
        if not payloads:
            raise S3ObjectLockError(
                "s3_object_lock.command_output_missing",
                f"The `{command_name}` command did not return JSON evidence.",
                {"returnCode": completed.returncode},
            )
        return _McJsonResult(completed.returncode, tuple(payloads))

    def _run_process(
        self,
        command: Sequence[str],
        *,
        input_bytes: bytes | None,
    ) -> subprocess.CompletedProcess[bytes]:
        environment = os.environ.copy()
        environment.update(self.options.subprocess_environment)
        try:
            return subprocess.run(
                command,
                cwd=self.options.repo_root,
                check=False,
                capture_output=True,
                input=input_bytes,
                timeout=self.options.timeout_seconds,
                env=environment,
            )
        except subprocess.TimeoutExpired:
            raise S3ObjectLockError(
                "s3_object_lock.command_timeout",
                "The object-lock command exceeded the bounded timeout.",
                {"timeoutSeconds": self.options.timeout_seconds},
            ) from None
        except OSError as error:
            raise S3ObjectLockError(
                "s3_object_lock.command_launch_failed",
                "The object-lock command could not be started.",
                {"errorType": type(error).__name__},
            ) from None

    def _base_command(self, *, json_output: bool) -> list[str]:
        command = [
            self.options.mc_bin,
            "--config-dir",
            str(self.options.config_dir),
            "--quiet",
            "--disable-pager",
            "--no-color",
        ]
        for resolve in self.options.resolve:
            command.extend(["--resolve", resolve])
        if json_output:
            command.append("--json")
        return command

    def _bucket_target(self, *, with_trailing_slash: bool = False) -> str:
        target = f"{self.options.alias}/{self.options.bucket}"
        return f"{target}/" if with_trailing_slash else target

    def _object_target(self, object_key: str) -> str:
        return f"{self._bucket_target()}/{_normalize_object_key(object_key)}"


def _normalize_options(options: S3ObjectLockOptions) -> S3ObjectLockOptions:
    repo_root = options.repo_root.expanduser().resolve()
    if not repo_root.is_dir():
        raise ValueError("repo_root must reference an existing directory")
    config_dir = _resolve_repository_external_directory(options.config_dir, repo_root, "config_dir")
    alias = _normalize_token(options.alias, label="alias")
    bucket = _normalize_token(options.bucket, label="bucket")
    prefix = _normalize_prefix(options.prefix)
    mc_bin = options.mc_bin.strip()
    if not mc_bin or any(character in mc_bin for character in "\r\n\t\x00"):
        raise ValueError("mc_bin must be a command or executable path")
    if options.retention_days <= 0:
        raise ValueError("retention_days must be positive")
    if options.timeout_seconds <= 0:
        raise ValueError("timeout_seconds must be positive")
    if options.shorten_probe_days <= 0 or options.shorten_probe_days >= options.retention_days:
        raise ValueError("shorten_probe_days must be positive and shorter than retention_days")
    environment: dict[str, str] = {}
    for name, value in options.subprocess_environment.items():
        if _SAFE_ENVIRONMENT_NAME_PATTERN.fullmatch(name) is None:
            raise ValueError("subprocess environment contained an invalid name")
        if not isinstance(value, str) or not value or any(character in value for character in "\r\n\x00"):
            raise ValueError("subprocess environment contained an invalid value")
        environment[name] = value
    return S3ObjectLockOptions(
        repo_root=repo_root,
        config_dir=config_dir,
        alias=alias,
        bucket=bucket,
        prefix=prefix,
        retention_days=int(options.retention_days),
        mc_bin=mc_bin,
        timeout_seconds=float(options.timeout_seconds),
        shorten_probe_days=int(options.shorten_probe_days),
        resolve=tuple(_normalize_resolve(value) for value in options.resolve),
        subprocess_environment=environment,
    )


def _resolve_repository_external_directory(
    raw: pathlib.Path,
    repo_root: pathlib.Path,
    label: str,
) -> pathlib.Path:
    expanded = raw.expanduser()
    if not expanded.is_absolute():
        raise ValueError(f"{label} must be absolute")
    lexical = pathlib.Path(os.path.abspath(expanded))
    resolved = expanded.resolve()
    if (
        lexical == repo_root
        or repo_root in lexical.parents
        or resolved == repo_root
        or repo_root in resolved.parents
    ):
        raise ValueError(f"{label} must be outside the repository")
    if not resolved.is_dir():
        raise ValueError(f"{label} must reference an existing directory")
    return resolved


def _normalize_token(value: str, *, label: str) -> str:
    token = value.strip()
    if _SAFE_TOKEN_PATTERN.fullmatch(token) is None:
        raise ValueError(f"{label} must be a non-empty safe token")
    return token


def _normalize_prefix(value: str) -> str:
    prefix = value.strip().strip("/")
    if not prefix:
        return ""
    _normalize_object_key(prefix)
    return prefix


def _normalize_object_key(value: str) -> str:
    key = value.strip().strip("/")
    parts = key.split("/")
    if (
        not key
        or len(key) > 1024
        or any(
            not part
            or part in {".", ".."}
            or any(character in part for character in "\\\r\n\t\x00 ")
            for part in parts
        )
    ):
        raise ValueError("object key must be a safe slash-delimited path")
    return "/".join(parts)


def _normalize_resolve(value: str) -> str:
    candidate = value.strip()
    if not candidate or candidate.count("=") != 1:
        raise ValueError("resolve entries must be formatted as host[:port]=address")
    if any(character.isspace() or ord(character) < 32 for character in candidate):
        raise ValueError("resolve entries must not contain whitespace or controls")
    left, right = candidate.split("=", 1)
    if not left or not right:
        raise ValueError("resolve entries must include both source and destination")
    return candidate


def _parse_ndjson(stdout: bytes, stderr: bytes) -> list[dict[str, Any]]:
    payloads: list[dict[str, Any]] = []
    for stream in (stdout, stderr):
        for line in stream.decode("utf-8", errors="replace").replace("\r", "\n").splitlines():
            stripped = line.strip()
            if not stripped:
                continue
            if not stripped.startswith("{"):
                json_start = stripped.find("{")
                if json_start >= 0:
                    stripped = stripped[json_start:]
            try:
                payload = json.loads(stripped)
            except json.JSONDecodeError:
                raise S3ObjectLockError(
                    "s3_object_lock.command_output_invalid",
                    "The object-lock command returned malformed JSON output.",
                ) from None
            if not isinstance(payload, dict):
                raise S3ObjectLockError(
                    "s3_object_lock.command_output_invalid",
                    "The object-lock command returned a non-object JSON payload.",
                )
            payloads.append(payload)
    return payloads


def _payload_error_codes(payload: Mapping[str, Any]) -> tuple[str, ...]:
    codes: list[str] = []

    def visit(value: Any) -> None:
        if not isinstance(value, dict):
            return
        for key in ("code", "Code"):
            code = _safe_string(value.get(key))
            if code is not None:
                codes.append(code)
        for key in ("error", "cause"):
            visit(value.get(key))

    visit(payload.get("error"))
    visit(payload.get("cause"))
    return tuple(dict.fromkeys(codes))


def _payload_has_object_lock_denial(value: Any) -> bool:
    if isinstance(value, str):
        normalized = value.casefold()
        return (
            "worm protected" in normalized
            or "objectlocked" in normalized
            or "object lock" in normalized and "cannot" in normalized
        )
    if isinstance(value, Mapping):
        return any(_payload_has_object_lock_denial(item) for item in value.values())
    if isinstance(value, Sequence) and not isinstance(value, (bytes, bytearray)):
        return any(_payload_has_object_lock_denial(item) for item in value)
    return False


def _payload_has_iam_denial(value: Any) -> bool:
    if isinstance(value, str):
        normalized = value.casefold()
        return (
            "accessdenied" in normalized
            or "access denied" in normalized
            or "not authorized" in normalized
            or "unauthorized" in normalized
            or "permission denied" in normalized
        )
    if isinstance(value, Mapping):
        return any(_payload_has_iam_denial(item) for item in value.values())
    if isinstance(value, Sequence) and not isinstance(value, (bytes, bytearray)):
        return any(_payload_has_iam_denial(item) for item in value)
    return False


def _payload_denial_kind(payload: Mapping[str, Any]) -> str | None:
    if _payload_has_object_lock_denial(payload):
        return "object_lock"
    if _payload_has_iam_denial(payload):
        return "iam"
    return None


def _require_mapping(value: Any, *, code: str, message: str) -> Mapping[str, Any]:
    if not isinstance(value, dict):
        raise S3ObjectLockError(code, message)
    return value


def _require_string(value: Any, *, code: str, message: str) -> str:
    if not isinstance(value, str) or not value.strip():
        raise S3ObjectLockError(code, message)
    return value.strip()


def _normalize_etag(value: Any, *, code: str, message: str) -> str:
    return _require_string(value, code=code, message=message).strip('"')


def _upper_string(value: Any) -> str | None:
    result = _safe_string(value)
    return result.upper() if result is not None else None


def _safe_string(value: Any) -> str | None:
    if isinstance(value, str) and value.strip():
        return value.strip()
    return None


def _metadata_entry_sha256(metadata: Mapping[str, Any]) -> str | None:
    for key in ENTRY_SHA256_METADATA_KEYS:
        value = _safe_string(metadata.get(key))
        if value is not None and _SHA256_PATTERN.fullmatch(value.lower()) is not None:
            return value.lower()
    return None


def _parse_timestamp(value: str) -> dt.datetime:
    normalized = value.strip()
    if normalized.endswith("Z"):
        normalized = f"{normalized[:-1]}+00:00"
    try:
        parsed = dt.datetime.fromisoformat(normalized)
    except ValueError:
        raise S3ObjectLockError(
            "s3_object_lock.timestamp_invalid",
            "The object-lock response contained an invalid timestamp.",
        ) from None
    if parsed.tzinfo is None:
        raise S3ObjectLockError(
            "s3_object_lock.timestamp_invalid",
            "The object-lock response timestamp omitted its timezone.",
        )
    return parsed.astimezone(dt.timezone.utc)
