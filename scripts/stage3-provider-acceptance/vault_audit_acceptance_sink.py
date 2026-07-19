#!/usr/bin/env python3
"""Accept Vault audit Vector NDJSON over HTTPS mTLS and persist a hash-chain ledger."""

from __future__ import annotations

import argparse
import dataclasses
import datetime as dt
import hashlib
import http.server
import json
import os
import pathlib
import re
import ssl
import tempfile
import threading
import urllib.parse
from collections.abc import Mapping, Sequence
from typing import Any

import s3_object_lock as object_lock


SCHEMA_VERSION = "synara.vault-audit-acceptance-sink.v3"
LEDGER_FILE_NAME = "ledger.ndjson"
METADATA_FILE_NAME = "collector-metadata.json"
GENESIS_HASH = "0" * 64
DEFAULT_RETENTION_DAYS = 365
DEFAULT_MAX_BODY_BYTES = 8 << 20
ROTATION_MIN_PRIOR_SEQUENCE = 90 << 20
ROTATION_MAX_RESET_SEQUENCE = 8 << 20
READ_ONLY_ALLOW_HEADER = "GET, POST"
CERTIFICATE_PATTERN = (
    "-----BEGIN CERTIFICATE-----",
    "-----END CERTIFICATE-----",
)
DEFAULT_OBJECT_LOCK_ALIAS_ENV = "VAULT_AUDIT_WORM_MC_ALIAS"
DEFAULT_OBJECT_LOCK_CONFIG_DIR_ENV = "VAULT_AUDIT_WORM_MC_CONFIG_DIR"
DEFAULT_OBJECT_LOCK_HOST_ENV = "VAULT_AUDIT_WORM_MC_HOST"
DEFAULT_OBJECT_LOCK_RESOLVE_ENV = "VAULT_AUDIT_WORM_MC_RESOLVE"
DEFAULT_OBJECT_LOCK_BUCKET = "synara-vault-audit"
DEFAULT_OBJECT_LOCK_PREFIX = "entries"
ENVIRONMENT_NAME_PATTERN = re.compile(r"[A-Z][A-Z0-9_]{0,127}")


class SinkError(Exception):
    def __init__(
        self,
        status: int,
        code: str,
        message: str,
        evidence: Mapping[str, Any] | None = None,
    ) -> None:
        super().__init__(message)
        self.status = status
        self.code = code
        self.message = message
        self.evidence = dict(evidence or {})

    def as_payload(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "schemaVersion": SCHEMA_VERSION,
            "status": "error",
            "error": {
                "code": self.code,
                "message": self.message,
            },
        }
        if self.evidence:
            payload["error"]["evidence"] = dict(self.evidence)
        return payload


@dataclasses.dataclass(frozen=True)
class SinkOptions:
    bind_host: str
    port: int
    state_dir: pathlib.Path
    server_cert_path: pathlib.Path
    server_key_path: pathlib.Path
    client_ca_cert_path: pathlib.Path
    retention_days: int = DEFAULT_RETENTION_DAYS
    max_body_bytes: int = DEFAULT_MAX_BODY_BYTES
    object_lock_options: object_lock.S3ObjectLockOptions | None = None
    object_lock_environment_names: tuple[str, ...] = ()


@dataclasses.dataclass(frozen=True)
class LedgerFileState:
    inode: int | None
    size: int
    mtime_ns: int
    ctime_ns: int


def utc_now() -> str:
    return dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def sha256_bytes(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def stable_json_bytes(value: Any) -> bytes:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")


def stable_json_value(value: Any) -> Any:
    return json.loads(stable_json_bytes(value))


def ensure_private_directory(path: pathlib.Path) -> pathlib.Path:
    path.mkdir(parents=True, exist_ok=True)
    path.chmod(0o700)
    return path


def ensure_private_file(path: pathlib.Path) -> pathlib.Path:
    if path.exists():
        path.chmod(0o600)
    return path


def read_ledger_file_state(path: pathlib.Path) -> LedgerFileState:
    if not path.exists():
        return LedgerFileState(inode=None, size=0, mtime_ns=0, ctime_ns=0)
    stat_result = path.stat()
    return LedgerFileState(
        inode=int(stat_result.st_ino),
        size=int(stat_result.st_size),
        mtime_ns=int(stat_result.st_mtime_ns),
        ctime_ns=int(stat_result.st_ctime_ns),
    )


def fsync_directory(path: pathlib.Path) -> None:
    descriptor = os.open(str(path), os.O_RDONLY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def write_private_json(path: pathlib.Path, payload: Mapping[str, Any]) -> None:
    ensure_private_directory(path.parent)
    with tempfile.NamedTemporaryFile(
        "w",
        encoding="utf-8",
        dir=path.parent,
        prefix=f".{path.name}.",
        delete=False,
    ) as handle:
        os.fchmod(handle.fileno(), 0o600)
        handle.write(json.dumps(payload, indent=2, sort_keys=True, ensure_ascii=False))
        handle.write("\n")
        handle.flush()
        os.fsync(handle.fileno())
        temp_path = pathlib.Path(handle.name)
    temp_path.replace(path)
    ensure_private_file(path)
    fsync_directory(path.parent)


def _require_mapping(value: Any, *, label: str) -> Mapping[str, Any]:
    if not isinstance(value, Mapping):
        raise SinkError(400, "sink.payload_invalid", f"{label} must be a JSON object.")
    return value


def _require_string(value: Any, *, label: str) -> str:
    if not isinstance(value, str) or not value.strip():
        raise SinkError(400, "sink.payload_invalid", f"{label} must be a non-empty string.")
    return value.strip()


def _require_integer(value: Any, *, label: str) -> int:
    if not isinstance(value, int) or isinstance(value, bool):
        raise SinkError(400, "sink.payload_invalid", f"{label} must be an integer.")
    return value


def _first_integer(value: Mapping[str, Any], *paths: tuple[str, ...]) -> int | None:
    for path in paths:
        current: Any = value
        for part in path:
            if not isinstance(current, Mapping) or part not in current:
                current = None
                break
            current = current[part]
        if isinstance(current, int) and not isinstance(current, bool):
            return current
    return None


def _first_string(value: Mapping[str, Any], *paths: tuple[str, ...]) -> str | None:
    for path in paths:
        current: Any = value
        for part in path:
            if not isinstance(current, Mapping) or part not in current:
                current = None
                break
            current = current[part]
        if isinstance(current, str) and current.strip():
            return current.strip()
    return None


def _looks_like_certificate(text: str) -> bool:
    return CERTIFICATE_PATTERN[0] in text and CERTIFICATE_PATTERN[1] in text


def _name_to_string(value: Any) -> str:
    if not isinstance(value, (list, tuple)):
        return ""
    pairs: list[str] = []
    for relative_name in value:
        if not isinstance(relative_name, (list, tuple)):
            continue
        for pair in relative_name:
            if (
                isinstance(pair, (list, tuple))
                and len(pair) == 2
                and isinstance(pair[0], str)
                and isinstance(pair[1], str)
            ):
                pairs.append(f"{pair[0]}={pair[1]}")
    return ", ".join(pairs)


def _normalize_payload(event: Mapping[str, Any]) -> tuple[Mapping[str, Any], dict[str, Any]]:
    if "vault_audit" in event:
        payload = _require_mapping(event.get("vault_audit"), label="vault_audit")
        envelope = dict(event)
        envelope.pop("vault_audit", None)
        return payload, envelope
    if "message" in event:
        message = event.get("message")
        if isinstance(message, str):
            try:
                payload = json.loads(message)
            except json.JSONDecodeError as error:
                raise SinkError(
                    400,
                    "sink.payload_invalid",
                    "Vector message field was not valid Vault audit JSON.",
                    {"lineColumn": error.colno},
                ) from None
            payload = _require_mapping(payload, label="message")
        else:
            payload = _require_mapping(message, label="message")
        envelope = dict(event)
        envelope.pop("message", None)
        return payload, envelope
    reserved = {
        "stream_id",
        "sequence",
        "line_number",
        "offset",
        "file",
        "host",
        "vector",
        "metadata",
        "source",
    }
    payload = {key: value for key, value in event.items() if key not in reserved}
    if "request" not in payload and "request" in event:
        payload["request"] = event["request"]
    return _require_mapping(payload, label="event"), {key: value for key, value in event.items() if key in reserved}


def normalize_vector_event(event: Mapping[str, Any]) -> dict[str, Any]:
    payload, envelope = _normalize_payload(event)
    payload_bytes = stable_json_bytes(payload)
    canonical_payload = stable_json_value(payload)
    request = _require_mapping(payload.get("request"), label="request")
    request_id = _require_string(request.get("id"), label="request.id")
    request_path = _require_string(request.get("path"), label="request.path")
    request_operation = _require_string(request.get("operation"), label="request.operation")
    audit_type = _require_string(payload.get("type"), label="type")
    event_time = _require_string(payload.get("time"), label="time")
    sequence = _first_integer(
        envelope,
        ("sequence",),
        ("line_number",),
        ("offset",),
        ("vector", "sequence"),
        ("metadata", "sequence"),
        ("source", "sequence"),
    )
    if sequence is None:
        raise SinkError(
            409,
            "sink.sequence_missing",
            "The Vector envelope omitted a usable sequence number for ledger ordering.",
        )
    sequence = _require_integer(sequence, label="sequence")
    if sequence < 0:
        raise SinkError(409, "sink.sequence_invalid", "Sequence numbers must be non-negative.")
    stream_file = _first_string(envelope, ("file",), ("source", "file"))
    stream_host = _first_string(envelope, ("host",), ("source", "host"))
    stream_id = _first_string(envelope, ("stream_id",), ("vector", "stream_id"), ("metadata", "stream_id"))
    if stream_id is None:
        if stream_host and stream_file:
            stream_id = f"{stream_host}:{stream_file}"
        elif stream_file:
            stream_id = stream_file
        elif stream_host:
            stream_id = stream_host
        else:
            stream_id = "default"
    namespace_id = None
    if isinstance(payload.get("namespace"), Mapping):
        namespace_id = payload["namespace"].get("id")
    return {
        "payloadHash": sha256_bytes(payload_bytes),
        "payload": canonical_payload,
        "stream": {
            "id": stream_id,
            "sequence": sequence,
            "file": stream_file,
            "host": stream_host,
        },
        "audit": {
            "eventTime": event_time,
            "type": audit_type,
            "requestId": request_id,
            "path": request_path,
            "operation": request_operation,
            "namespaceId": namespace_id if isinstance(namespace_id, str) and namespace_id else None,
        },
    }


def _canonical_entry_hash(entry: Mapping[str, Any]) -> str:
    unsigned = dict(entry)
    unsigned.pop("entrySha256", None)
    return sha256_bytes(stable_json_bytes(unsigned))


def _index_entry(entry: Mapping[str, Any]) -> dict[str, Any]:
    indexed = {
        "ledgerIndex": entry.get("ledgerIndex"),
        "entrySha256": entry.get("entrySha256"),
        "previousEntrySha256": entry.get("previousEntrySha256"),
        "receivedAt": entry.get("receivedAt"),
        "retention": entry.get("retention"),
        "stream": entry.get("stream"),
        "audit": entry.get("audit"),
        "transport": entry.get("transport"),
        "payloadSha256": entry.get("payloadSha256"),
    }
    archive = entry.get("archive")
    if isinstance(archive, Mapping):
        indexed["archive"] = dict(archive)
    return indexed


def _receipt_view(entry: Mapping[str, Any]) -> dict[str, Any]:
    return dict(_index_entry(entry))


@dataclasses.dataclass(frozen=True)
class ChainVerification:
    verified: bool
    entries: tuple[dict[str, Any], ...]
    entry_count: int
    latest_entry_sha256: str
    last_sequence_by_stream: dict[str, int]
    last_generation_by_stream: dict[str, int]
    receipt_by_request: dict[tuple[str, str], dict[str, Any]]
    seen_request_type_keys: set[tuple[str, str, str]]
    errors: tuple[dict[str, Any], ...]
    earliest_expiry: str | None
    latest_expiry: str | None
    archive_summary_by_object_key: dict[str, dict[str, Any]]


def verify_ledger_chain(ledger_path: pathlib.Path) -> ChainVerification:
    entries: list[dict[str, Any]] = []
    errors: list[dict[str, Any]] = []
    last_sequence_by_stream: dict[str, int] = {}
    last_generation_by_stream: dict[str, int] = {}
    receipt_by_request: dict[tuple[str, str], dict[str, Any]] = {}
    seen_request_type_keys: set[tuple[str, str, str]] = set()
    earliest_expiry: str | None = None
    latest_expiry: str | None = None
    archive_hashers: dict[str, Any] = {}
    archive_entry_count_by_object_key: dict[str, int] = {}
    latest_hash = GENESIS_HASH
    if not ledger_path.exists():
        return ChainVerification(
            verified=True,
            entries=(),
            entry_count=0,
            latest_entry_sha256=latest_hash,
            last_sequence_by_stream={},
            last_generation_by_stream={},
            receipt_by_request={},
            seen_request_type_keys=set(),
            errors=(),
            earliest_expiry=None,
            latest_expiry=None,
            archive_summary_by_object_key={},
        )
    with ledger_path.open("r", encoding="utf-8") as handle:
        for line_number, raw_line in enumerate(handle, start=1):
            line = raw_line.strip()
            if not line:
                continue
            try:
                record = json.loads(line)
            except json.JSONDecodeError:
                errors.append(
                    {
                        "code": "sink.chain_json_invalid",
                        "line": line_number,
                        "message": "Ledger line was not valid JSON.",
                    }
                )
                continue
            if not isinstance(record, dict):
                errors.append(
                    {
                        "code": "sink.chain_entry_invalid",
                        "line": line_number,
                        "message": "Ledger line was not a JSON object.",
                    }
                )
                continue
            payload = record.get("payload")
            payload_sha256 = record.get("payloadSha256")
            if not isinstance(payload, Mapping):
                errors.append(
                    {
                        "code": "sink.chain_payload_invalid",
                        "line": line_number,
                        "message": "Ledger entry did not retain the full structured Vault audit payload.",
                    }
                )
            else:
                expected_payload_hash = sha256_bytes(stable_json_bytes(payload))
                if payload_sha256 != expected_payload_hash:
                    errors.append(
                        {
                            "code": "sink.chain_payload_hash_mismatch",
                            "line": line_number,
                            "message": "Ledger payload hash did not match the retained Vault audit payload.",
                        }
                    )
            expected_hash = _canonical_entry_hash(record)
            entry_hash = record.get("entrySha256")
            previous_hash = record.get("previousEntrySha256")
            if entry_hash != expected_hash:
                errors.append(
                    {
                        "code": "sink.chain_hash_mismatch",
                        "line": line_number,
                        "message": "Ledger entry hash did not match the stored content hash.",
                    }
                )
            if previous_hash != latest_hash:
                errors.append(
                    {
                        "code": "sink.chain_link_mismatch",
                        "line": line_number,
                        "message": "Ledger entry previous hash did not match the preceding ledger entry.",
                    }
                )
            stream = record.get("stream")
            audit = record.get("audit")
            retention = record.get("retention")
            if isinstance(stream, dict) and isinstance(audit, dict):
                stream_id = stream.get("id")
                sequence = stream.get("sequence")
                generation = stream.get("generation", 0)
                audit_type = audit.get("type")
                request_id = audit.get("requestId")
                request_path = audit.get("path")
                if (
                    isinstance(stream_id, str)
                    and isinstance(sequence, int)
                    and not isinstance(sequence, bool)
                    and isinstance(generation, int)
                    and not isinstance(generation, bool)
                    and generation >= 0
                ):
                    prior = last_sequence_by_stream.get(stream_id)
                    prior_generation = last_generation_by_stream.get(stream_id)
                    if prior is None:
                        if generation != 0:
                            errors.append(
                                {
                                    "code": "sink.chain_generation_invalid",
                                    "line": line_number,
                                    "message": "The first ledger event for a stream did not use generation zero.",
                                }
                            )
                    elif generation == prior_generation:
                        if sequence <= prior:
                            errors.append(
                                {
                                    "code": "sink.chain_sequence_invalid",
                                    "line": line_number,
                                    "message": "Ledger sequence was not strictly increasing for its stream generation.",
                                }
                            )
                    elif generation == int(prior_generation or 0) + 1:
                        if (
                            prior < ROTATION_MIN_PRIOR_SEQUENCE
                            or sequence > ROTATION_MAX_RESET_SEQUENCE
                        ):
                            errors.append(
                                {
                                    "code": "sink.chain_generation_invalid",
                                    "line": line_number,
                                    "message": "Ledger stream generation changed without a valid audit rotation offset reset.",
                                }
                            )
                    else:
                        errors.append(
                            {
                                "code": "sink.chain_generation_invalid",
                                "line": line_number,
                                "message": "Ledger stream generation was not contiguous.",
                            }
                        )
                    last_sequence_by_stream[stream_id] = sequence
                    last_generation_by_stream[stream_id] = generation
                else:
                    errors.append(
                        {
                            "code": "sink.chain_stream_invalid",
                            "line": line_number,
                            "message": "Ledger stream identity, sequence, or generation was malformed.",
                        }
                    )
                if (
                    isinstance(request_id, str)
                    and isinstance(request_path, str)
                    and isinstance(audit_type, str)
                ):
                    request_key = (request_id, request_path, audit_type)
                    if request_key in seen_request_type_keys:
                        errors.append(
                            {
                                "code": "sink.chain_duplicate_request",
                                "line": line_number,
                                "message": "Ledger retained a duplicate request identity.",
                            }
                        )
                    seen_request_type_keys.add(request_key)
                    receipt_key = (request_id, request_path)
                    if audit_type == "request" or receipt_key not in receipt_by_request:
                        receipt_by_request[receipt_key] = _index_entry(record)
            if isinstance(retention, dict):
                expires_at = retention.get("expiresAt")
                if isinstance(expires_at, str) and expires_at:
                    if earliest_expiry is None or expires_at < earliest_expiry:
                        earliest_expiry = expires_at
                    if latest_expiry is None or expires_at > latest_expiry:
                        latest_expiry = expires_at
            archive = record.get("archive")
            if isinstance(archive, Mapping):
                object_key = archive.get("objectKey")
                if isinstance(object_key, str) and object_key:
                    hasher = archive_hashers.setdefault(object_key, hashlib.sha256())
                    hasher.update(stable_json_bytes(record) + b"\n")
                    archive_entry_count_by_object_key[object_key] = (
                        archive_entry_count_by_object_key.get(object_key, 0) + 1
                    )
            latest_hash = str(entry_hash or latest_hash)
            entries.append(_index_entry(record))
    return ChainVerification(
        verified=not errors,
        entries=tuple(entries),
        entry_count=len(entries),
        latest_entry_sha256=latest_hash,
        last_sequence_by_stream=last_sequence_by_stream,
        last_generation_by_stream=last_generation_by_stream,
        receipt_by_request=receipt_by_request,
        seen_request_type_keys=seen_request_type_keys,
        errors=tuple(errors),
        earliest_expiry=earliest_expiry,
        latest_expiry=latest_expiry,
        archive_summary_by_object_key={
            object_key: {
                "batchContentSha256": hasher.hexdigest(),
                "batchEntryCount": archive_entry_count_by_object_key.get(object_key, 0),
            }
            for object_key, hasher in archive_hashers.items()
        },
    )


class AuditLedger:
    def __init__(
        self,
        options: SinkOptions,
        *,
        object_lock_client: object_lock.S3ObjectLockClient | None = None,
    ) -> None:
        self.options = options
        self._object_lock_client = object_lock_client
        if self._object_lock_client is None and options.object_lock_options is not None:
            self._object_lock_client = object_lock.S3ObjectLockClient(options.object_lock_options)
        self._object_lock_contract = self._verify_object_lock_contract()
        self.state_dir = ensure_private_directory(options.state_dir)
        self.ledger_path = ensure_private_file(self.state_dir / LEDGER_FILE_NAME)
        self.metadata_path = self.state_dir / METADATA_FILE_NAME
        self._lock = threading.Lock()
        self._metadata = self._ensure_metadata()
        verification = verify_ledger_chain(self.ledger_path)
        if not verification.verified:
            raise SinkError(
                409,
                "sink.state_invalid",
                "The existing sink ledger state was already tampered or malformed.",
                {"errorCount": len(verification.errors)},
            )
        self._verification_state = verification
        self._ledger_file_state = read_ledger_file_state(self.ledger_path)

    def _ensure_metadata(self) -> dict[str, Any]:
        metadata = {
            "schemaVersion": SCHEMA_VERSION,
            "createdAt": utc_now(),
            "mtlsRequired": True,
            "ledgerFile": LEDGER_FILE_NAME,
            "retention": {
                "immutable": self._object_lock_contract is not None,
                "storageEnforced": self._object_lock_contract is not None,
                "retentionDays": self.options.retention_days,
                "requirement": "immutable retention outside the Vault cluster",
                "disposition": (
                    "external S3-compatible Object Lock COMPLIANCE archive"
                    if self._object_lock_contract is not None
                    else "tamper-evident local hash chain; external immutable retention not configured"
                ),
            },
            "objectLock": self._object_lock_metadata(),
            "streamSequence": {
                "rotationMinPriorSequence": ROTATION_MIN_PRIOR_SEQUENCE,
                "rotationMaxResetSequence": ROTATION_MAX_RESET_SEQUENCE,
                "generationRule": "increment exactly once when a rotated active file resets to a bounded offset",
            },
            "tls": {
                "clientCaCertificateSha256": sha256_bytes(
                    self.options.client_ca_cert_path.read_bytes()
                ),
                "serverCertificateSha256": sha256_bytes(
                    ssl.PEM_cert_to_DER_cert(
                        self.options.server_cert_path.read_text(encoding="utf-8")
                    )
                ),
            },
        }
        if self.metadata_path.exists():
            try:
                existing = json.loads(self.metadata_path.read_text(encoding="utf-8"))
            except (OSError, json.JSONDecodeError):
                raise SinkError(
                    409,
                    "sink.state_invalid",
                    "The existing sink metadata was unreadable.",
                    {"path": str(self.metadata_path)},
                ) from None
            if not isinstance(existing, dict):
                raise SinkError(
                    409,
                    "sink.state_invalid",
                    "The existing sink metadata was not a JSON object.",
                    {"path": str(self.metadata_path)},
                )
            if existing.get("schemaVersion") != metadata["schemaVersion"] or existing.get(
                "retention"
            ) != metadata["retention"] or existing.get("streamSequence") != metadata[
                "streamSequence"
            ] or existing.get("tls") != metadata["tls"] or existing.get(
                "objectLock"
            ) != metadata["objectLock"]:
                raise SinkError(
                    409,
                    "sink.state_invalid",
                    "The existing sink metadata did not match the configured TLS or retention boundary.",
                )
            ensure_private_file(self.metadata_path)
            return existing
        write_private_json(self.metadata_path, metadata)
        return metadata

    def _verify_object_lock_contract(self) -> object_lock.BucketContract | None:
        if self._object_lock_client is None:
            return None
        try:
            contract = self._object_lock_client.verify_bucket_contract()
        except (object_lock.S3ObjectLockError, ValueError) as error:
            raise SinkError(
                503,
                "sink.object_lock_invalid",
                "The external Object Lock archive did not satisfy its storage contract.",
                {"errorCode": getattr(error, "code", "s3_object_lock.configuration_invalid")},
            ) from None
        if (
            contract.versioning_status != "Enabled"
            or contract.default_retention_mode != "COMPLIANCE"
            or contract.default_retention_days != self.options.retention_days
        ):
            raise SinkError(
                503,
                "sink.object_lock_invalid",
                "The external Object Lock archive contract drifted from the sink retention boundary.",
            )
        return contract

    def _object_lock_metadata(self) -> dict[str, Any]:
        if self._object_lock_contract is None or self._object_lock_client is None:
            return {"enabled": False}
        client_options = self._object_lock_client.options
        return {
            "enabled": True,
            "provider": "s3-compatible",
            "bucket": client_options.bucket,
            "objectPrefix": client_options.prefix,
            "versioning": self._object_lock_contract.versioning_status,
            "mode": self._object_lock_contract.default_retention_mode,
            "retentionDays": self._object_lock_contract.default_retention_days,
            "credentialEnvironmentNames": list(self.options.object_lock_environment_names),
        }

    def _runtime_drift_verification(
        self,
        verification: ChainVerification,
        *,
        current_state: LedgerFileState,
    ) -> ChainVerification:
        errors = list(verification.errors)
        errors.append(
            {
                "code": "sink.chain_runtime_drift",
                "message": "Ledger state changed outside the sink append path after startup.",
                "expectedFileSize": self._ledger_file_state.size,
                "observedFileSize": current_state.size,
            }
        )
        return dataclasses.replace(verification, verified=False, errors=tuple(errors))

    def _verification_locked(self) -> ChainVerification:
        current_state = read_ledger_file_state(self.ledger_path)
        if current_state == self._ledger_file_state:
            return self._verification_state
        verification = verify_ledger_chain(self.ledger_path)
        verification = self._runtime_drift_verification(
            verification,
            current_state=current_state,
        )
        self._verification_state = verification
        self._ledger_file_state = current_state
        return verification

    def _require_verified_locked(self) -> ChainVerification:
        verification = self._verification_locked()
        if not verification.verified:
            raise SinkError(
                409,
                "sink.state_invalid",
                "The sink ledger state was tampered after startup.",
                {"errorCount": len(verification.errors)},
            )
        return verification

    def health_report(self) -> dict[str, Any]:
        with self._lock:
            verification = self._verification_locked()
            return {
                "schemaVersion": SCHEMA_VERSION,
                "status": "ok" if verification.verified else "fail",
                "mtlsRequired": True,
                "ledger": {
                    "entryCount": verification.entry_count,
                    "latestEntrySha256": verification.latest_entry_sha256,
                    "verified": verification.verified,
                },
                "retention": dict(self._metadata["retention"]),
                "objectLock": dict(self._metadata["objectLock"]),
                "streamSequence": dict(self._metadata["streamSequence"]),
            }

    def receipt(self, request_id: str, path: str) -> dict[str, Any] | None:
        with self._lock:
            verification = self._require_verified_locked()
            entry = verification.receipt_by_request.get((request_id, path))
            if entry is None:
                return None
            return self._receipt_view(entry, verification.archive_summary_by_object_key)

    def _receipt_view(
        self,
        entry: Mapping[str, Any],
        archive_summary_by_object_key: Mapping[str, Mapping[str, Any]],
    ) -> dict[str, Any]:
        receipt = _receipt_view(entry)
        archive = entry.get("archive")
        if not isinstance(archive, Mapping):
            return receipt
        object_key = archive.get("objectKey")
        if not isinstance(object_key, str) or not object_key:
            return receipt
        archive_summary = archive_summary_by_object_key.get(object_key)
        receipt["archive"] = dict(archive)
        if isinstance(archive_summary, Mapping):
            receipt["archive"].update(dict(archive_summary))
        return receipt

    def chain_report(self) -> tuple[int, dict[str, Any]]:
        with self._lock:
            verification = self._verification_locked()
            status = 200 if verification.verified else 409
            return status, {
                "schemaVersion": SCHEMA_VERSION,
                "status": "ok" if verification.verified else "fail",
                "verified": verification.verified,
                "entryCount": verification.entry_count,
                "latestEntrySha256": verification.latest_entry_sha256,
                "earliestExpiry": verification.earliest_expiry,
                "latestExpiry": verification.latest_expiry,
                "entries": [_receipt_view(entry) for entry in verification.entries],
                "errors": list(verification.errors),
            }

    def retention_report(self) -> dict[str, Any]:
        with self._lock:
            verification = self._verification_locked()
            self._object_lock_contract = self._verify_object_lock_contract()
            return {
                "schemaVersion": SCHEMA_VERSION,
                "status": "ok",
                "policy": {
                    **dict(self._metadata["retention"]),
                    "earliestExpiry": verification.earliest_expiry,
                    "latestExpiry": verification.latest_expiry,
                    "entryCount": verification.entry_count,
                },
                "objectLock": self._object_lock_metadata(),
            }

    def append_batch(
        self,
        events: Sequence[Mapping[str, Any]],
        *,
        transport: Mapping[str, Any],
    ) -> list[dict[str, Any]]:
        if not events:
            raise SinkError(400, "sink.batch_empty", "At least one audit event line is required.")
        normalized_events = [normalize_vector_event(_require_mapping(event, label="event")) for event in events]
        with self._lock:
            verification = self._require_verified_locked()
            last_sequence_by_stream = dict(verification.last_sequence_by_stream)
            last_generation_by_stream = dict(verification.last_generation_by_stream)
            seen_request_keys = set(verification.seen_request_type_keys)
            receipt_by_request = dict(verification.receipt_by_request)
            archive_summary_by_object_key = dict(verification.archive_summary_by_object_key)
            previous_hash = verification.latest_entry_sha256
            next_index = verification.entry_count + 1
            received_at = utc_now()
            entries: list[dict[str, Any]] = []
            archive_pointer: dict[str, Any] | None = None
            if self._object_lock_client is not None:
                last_index = next_index + len(normalized_events) - 1
                object_key = self._object_lock_client.qualify_object_key(
                    f"{next_index:020d}-{last_index:020d}.ndjson"
                )
                archive_pointer = {
                    "provider": "s3-compatible-object-lock",
                    "bucket": self._object_lock_client.options.bucket,
                    "objectKey": object_key,
                    "mode": "COMPLIANCE",
                    "firstLedgerIndex": next_index,
                    "lastLedgerIndex": last_index,
                }
            for normalized in normalized_events:
                stream = normalized["stream"]
                audit = normalized["audit"]
                stream_id = str(stream["id"])
                sequence = int(stream["sequence"])
                prior = last_sequence_by_stream.get(stream_id)
                generation = last_generation_by_stream.get(stream_id, 0)
                if prior is not None and sequence <= prior:
                    if (
                        prior >= ROTATION_MIN_PRIOR_SEQUENCE
                        and sequence <= ROTATION_MAX_RESET_SEQUENCE
                    ):
                        generation += 1
                    else:
                        raise SinkError(
                            409,
                            "sink.sequence_invalid",
                            "The submitted sequence number was not strictly newer than the persisted stream generation.",
                            {
                                "streamId": stream_id,
                                "streamGeneration": generation,
                                "priorSequence": prior,
                                "submittedSequence": sequence,
                            },
                        )
                request_key = (
                    str(audit["requestId"]),
                    str(audit["path"]),
                    str(audit["type"]),
                )
                if request_key in seen_request_keys:
                    raise SinkError(
                        409,
                        "sink.duplicate_request",
                        "The submitted request identity was already retained in the ledger.",
                        {"requestId": request_key[0], "path": request_key[1], "type": request_key[2]},
                    )
                expires_at = (
                    dt.datetime.now(dt.timezone.utc) + dt.timedelta(days=self.options.retention_days)
                ).replace(microsecond=0).isoformat().replace("+00:00", "Z")
                entry = {
                    "schemaVersion": SCHEMA_VERSION,
                    "ledgerIndex": next_index,
                    "receivedAt": received_at,
                    "previousEntrySha256": previous_hash,
                    "retention": {
                        "immutable": archive_pointer is not None,
                        "storageEnforced": archive_pointer is not None,
                        "retentionDays": self.options.retention_days,
                        "expiresAt": expires_at,
                    },
                    "stream": {**dict(stream), "generation": generation},
                    "audit": dict(audit),
                    "transport": dict(transport),
                    "payload": dict(normalized["payload"]),
                    "payloadSha256": normalized["payloadHash"],
                }
                if archive_pointer is not None:
                    entry["archive"] = dict(archive_pointer)
                entry["entrySha256"] = _canonical_entry_hash(entry)
                entries.append(entry)
                previous_hash = str(entry["entrySha256"])
                last_sequence_by_stream[stream_id] = sequence
                last_generation_by_stream[stream_id] = generation
                seen_request_keys.add(request_key)
                next_index += 1
            batch_bytes = b"".join(stable_json_bytes(entry) + b"\n" for entry in entries)
            batch_content_sha256 = sha256_bytes(batch_bytes)
            if archive_pointer is not None and self._object_lock_client is not None:
                try:
                    archived_sha256 = self._object_lock_client.upload_bytes(
                        str(archive_pointer["objectKey"]),
                        batch_bytes,
                    )
                except (object_lock.S3ObjectLockError, ValueError) as error:
                    raise SinkError(
                        503,
                        "sink.object_lock_archive_failed",
                        "The external Object Lock archive rejected the canonical audit batch.",
                        {"errorCode": getattr(error, "code", "s3_object_lock.upload_invalid")},
                    ) from None
                if archived_sha256 != batch_content_sha256:
                    raise SinkError(
                        503,
                        "sink.object_lock_archive_failed",
                        "The external Object Lock archive returned a mismatched audit batch hash.",
                    )
            with self.ledger_path.open("a", encoding="utf-8") as handle:
                handle.write(batch_bytes.decode("utf-8"))
                handle.flush()
                os.fsync(handle.fileno())
            ensure_private_file(self.ledger_path)
            fsync_directory(self.state_dir)
            indexed_entries = tuple(_index_entry(entry) for entry in entries)
            if archive_pointer is not None:
                archive_summary_by_object_key[str(archive_pointer["objectKey"])] = {
                    "batchContentSha256": batch_content_sha256,
                    "batchEntryCount": len(entries),
                }
            for indexed_entry in indexed_entries:
                audit = indexed_entry.get("audit")
                if not isinstance(audit, Mapping):
                    continue
                request_id = audit.get("requestId")
                request_path = audit.get("path")
                audit_type = audit.get("type")
                if (
                    isinstance(request_id, str)
                    and request_id
                    and isinstance(request_path, str)
                    and request_path
                    and isinstance(audit_type, str)
                    and audit_type
                ):
                    receipt_key = (request_id, request_path)
                    if audit_type == "request" or receipt_key not in receipt_by_request:
                        receipt_by_request[receipt_key] = indexed_entry
            earliest_expiry = verification.earliest_expiry
            latest_expiry = verification.latest_expiry
            for indexed_entry in indexed_entries:
                retention = indexed_entry.get("retention")
                if not isinstance(retention, Mapping):
                    continue
                expires_at = retention.get("expiresAt")
                if not isinstance(expires_at, str) or not expires_at:
                    continue
                if earliest_expiry is None or expires_at < earliest_expiry:
                    earliest_expiry = expires_at
                if latest_expiry is None or expires_at > latest_expiry:
                    latest_expiry = expires_at
            self._verification_state = ChainVerification(
                verified=True,
                entries=verification.entries + indexed_entries,
                entry_count=verification.entry_count + len(indexed_entries),
                latest_entry_sha256=previous_hash,
                last_sequence_by_stream=last_sequence_by_stream,
                last_generation_by_stream=last_generation_by_stream,
                receipt_by_request=receipt_by_request,
                seen_request_type_keys=seen_request_keys,
                errors=(),
                earliest_expiry=earliest_expiry,
                latest_expiry=latest_expiry,
                archive_summary_by_object_key=archive_summary_by_object_key,
            )
            self._ledger_file_state = read_ledger_file_state(self.ledger_path)
            return [
                self._receipt_view(indexed_entry, archive_summary_by_object_key)
                for indexed_entry in indexed_entries
            ]


class AuditSinkServer(http.server.ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(
        self,
        server_address: tuple[str, int],
        handler_class: type[http.server.BaseHTTPRequestHandler],
        *,
        ledger: AuditLedger,
        max_body_bytes: int,
    ) -> None:
        super().__init__(server_address, handler_class)
        self.ledger = ledger
        self.max_body_bytes = max_body_bytes


class AuditSinkHandler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server: AuditSinkServer

    def log_message(self, format: str, *args: Any) -> None:  # noqa: A003
        return

    def do_GET(self) -> None:  # noqa: N802
        try:
            self._dispatch_get()
        except SinkError as error:
            self._send_json(error.status, error.as_payload())

    def do_POST(self) -> None:  # noqa: N802
        try:
            self._dispatch_post()
        except SinkError as error:
            self._send_json(error.status, error.as_payload())

    def do_DELETE(self) -> None:  # noqa: N802
        self._send_json(
            405,
            SinkError(
                405,
                "sink.method_not_allowed",
                "The sink exposes no mutation or deletion API.",
            ).as_payload(),
        )

    def do_PUT(self) -> None:  # noqa: N802
        self.do_DELETE()

    def do_PATCH(self) -> None:  # noqa: N802
        self.do_DELETE()

    def _dispatch_get(self) -> None:
        parsed = urllib.parse.urlsplit(self.path)
        query = urllib.parse.parse_qs(parsed.query, keep_blank_values=False)
        path = parsed.path
        if path == "/healthz":
            self._send_json(200, self.server.ledger.health_report())
            return
        if path == "/v1/receipts":
            request_id = _require_string(
                query.get("request_id", [None])[0],
                label="request_id",
            )
            request_path = _require_string(query.get("path", [None])[0], label="path")
            receipt = self.server.ledger.receipt(request_id, request_path)
            if receipt is None:
                self._send_json(
                    404,
                    {
                        "schemaVersion": SCHEMA_VERSION,
                        "status": "not_found",
                        "requestId": request_id,
                        "path": request_path,
                    },
                )
                return
            self._send_json(
                200,
                {
                    "schemaVersion": SCHEMA_VERSION,
                    "status": "ok",
                    "requestId": request_id,
                    "path": request_path,
                    "receipt": receipt,
                },
            )
            return
        if path == "/v1/chain":
            status, payload = self.server.ledger.chain_report()
            self._send_json(status, payload)
            return
        if path == "/v1/retention":
            self._send_json(200, self.server.ledger.retention_report())
            return
        raise SinkError(404, "sink.path_not_found", "The requested sink path does not exist.")

    def _dispatch_post(self) -> None:
        parsed = urllib.parse.urlsplit(self.path)
        if parsed.path != "/v1/audit/events":
            raise SinkError(404, "sink.path_not_found", "The requested sink path does not exist.")
        content_length_header = self.headers.get("Content-Length")
        if content_length_header is None:
            raise SinkError(411, "sink.length_required", "Content-Length is required for NDJSON ingestion.")
        try:
            content_length = int(content_length_header)
        except ValueError:
            raise SinkError(400, "sink.length_invalid", "Content-Length was not a valid integer.") from None
        if content_length <= 0:
            raise SinkError(400, "sink.batch_empty", "At least one audit event line is required.")
        if content_length > self.server.max_body_bytes:
            raise SinkError(
                413,
                "sink.body_too_large",
                "The NDJSON payload exceeded the configured body size limit.",
            )
        content_type = (self.headers.get("Content-Type") or "").split(";", 1)[0].strip().lower()
        if content_type not in {"", "application/x-ndjson", "application/json", "text/plain"}:
            raise SinkError(
                415,
                "sink.content_type_invalid",
                "The sink only accepts newline-delimited JSON bodies.",
            )
        raw_body = self.rfile.read(content_length)
        try:
            body = raw_body.decode("utf-8")
        except UnicodeDecodeError:
            raise SinkError(400, "sink.body_invalid", "The NDJSON payload was not valid UTF-8.") from None
        events: list[dict[str, Any]] = []
        for line_number, raw_line in enumerate(body.splitlines(), start=1):
            line = raw_line.strip()
            if not line:
                continue
            try:
                payload = json.loads(line)
            except json.JSONDecodeError as error:
                raise SinkError(
                    400,
                    "sink.body_invalid",
                    "The NDJSON payload contained malformed JSON.",
                    {"line": line_number, "column": error.colno},
                ) from None
            events.append(dict(_require_mapping(payload, label=f"line {line_number}")))
        transport = self._transport_evidence()
        receipts = self.server.ledger.append_batch(events, transport=transport)
        self._send_json(
            202,
            {
                "schemaVersion": SCHEMA_VERSION,
                "status": "accepted",
                "accepted": len(receipts),
                "receipts": receipts,
            },
        )

    def _transport_evidence(self) -> dict[str, Any]:
        if not isinstance(self.connection, ssl.SSLSocket):
            raise SinkError(401, "sink.mtls_required", "The sink requires HTTPS with a client certificate.")
        peer_der = self.connection.getpeercert(binary_form=True)
        if not peer_der:
            raise SinkError(401, "sink.mtls_required", "The sink requires HTTPS with a client certificate.")
        peer_chain = self.connection.get_verified_chain() or [peer_der]
        peer_cert = self.connection.getpeercert() or {}
        cipher = self.connection.cipher()
        return {
            "mutualTlsVerified": True,
            "clientAddress": self.client_address[0] if self.client_address else None,
            "tlsVersion": self.connection.version(),
            "cipherSuite": cipher[0] if isinstance(cipher, tuple) and cipher else None,
            "peerCertificateSha256": sha256_bytes(peer_der),
            "peerChainSha256": [sha256_bytes(item) for item in peer_chain],
            "peerSubject": _name_to_string(peer_cert.get("subject")),
            "peerIssuer": _name_to_string(peer_cert.get("issuer")),
            "peerSerialNumber": peer_cert.get("serialNumber"),
        }

    def _send_json(self, status: int, payload: Mapping[str, Any]) -> None:
        body = json.dumps(payload, indent=2, sort_keys=True, ensure_ascii=False).encode("utf-8") + b"\n"
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        if status == 405:
            self.send_header("Allow", READ_ONLY_ALLOW_HEADER)
        self.end_headers()
        self.wfile.write(body)


def build_ssl_context(options: SinkOptions) -> ssl.SSLContext:
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    context.load_cert_chain(
        certfile=str(options.server_cert_path),
        keyfile=str(options.server_key_path),
    )
    context.load_verify_locations(cafile=str(options.client_ca_cert_path))
    context.verify_mode = ssl.CERT_REQUIRED
    context.check_hostname = False
    return context


def create_server(
    options: SinkOptions,
    *,
    object_lock_client: object_lock.S3ObjectLockClient | None = None,
) -> AuditSinkServer:
    ledger = AuditLedger(options, object_lock_client=object_lock_client)
    server = AuditSinkServer(
        (options.bind_host, options.port),
        AuditSinkHandler,
        ledger=ledger,
        max_body_bytes=options.max_body_bytes,
    )
    context = build_ssl_context(options)
    server.socket = context.wrap_socket(server.socket, server_side=True)
    return server


def parse_args(argv: Sequence[str] | None = None) -> SinkOptions:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--bind-host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=18443)
    parser.add_argument("--state-dir", required=True, type=pathlib.Path)
    parser.add_argument("--server-cert", required=True, type=pathlib.Path)
    parser.add_argument("--server-key", required=True, type=pathlib.Path)
    parser.add_argument("--client-ca-cert", required=True, type=pathlib.Path)
    parser.add_argument("--retention-days", type=int, default=DEFAULT_RETENTION_DAYS)
    parser.add_argument("--max-body-bytes", type=int, default=DEFAULT_MAX_BODY_BYTES)
    parser.add_argument("--object-lock-required", action="store_true")
    parser.add_argument("--object-lock-mc-bin", default="mc")
    parser.add_argument("--object-lock-mc-alias-env", default=DEFAULT_OBJECT_LOCK_ALIAS_ENV)
    parser.add_argument(
        "--object-lock-mc-config-dir-env",
        default=DEFAULT_OBJECT_LOCK_CONFIG_DIR_ENV,
    )
    parser.add_argument("--object-lock-mc-host-env", default=DEFAULT_OBJECT_LOCK_HOST_ENV)
    parser.add_argument("--object-lock-mc-resolve-env", default=DEFAULT_OBJECT_LOCK_RESOLVE_ENV)
    parser.add_argument("--object-lock-bucket", default=DEFAULT_OBJECT_LOCK_BUCKET)
    parser.add_argument("--object-lock-prefix", default=DEFAULT_OBJECT_LOCK_PREFIX)
    parsed = parser.parse_args(argv)
    if parsed.port < 0 or parsed.port > 65535:
        raise SystemExit("--port must be between 0 and 65535")
    if parsed.retention_days <= 0:
        raise SystemExit("--retention-days must be positive")
    if parsed.max_body_bytes <= 0:
        raise SystemExit("--max-body-bytes must be positive")
    object_lock_options: object_lock.S3ObjectLockOptions | None = None
    object_lock_environment_names: tuple[str, ...] = ()
    if parsed.object_lock_required:
        environment_names = (
            str(parsed.object_lock_mc_alias_env),
            str(parsed.object_lock_mc_config_dir_env),
            str(parsed.object_lock_mc_host_env),
            str(parsed.object_lock_mc_resolve_env),
        )
        if any(ENVIRONMENT_NAME_PATTERN.fullmatch(name) is None for name in environment_names):
            raise SystemExit("Object Lock environment names must be uppercase environment variable names")
        alias = _required_environment_value(environment_names[0], maximum_length=128)
        config_dir_value = _required_environment_value(environment_names[1], maximum_length=4096)
        host_value = _required_environment_value(environment_names[2], maximum_length=16 << 10)
        resolve_value = _required_environment_value(environment_names[3], maximum_length=4096)
        parsed_host = urllib.parse.urlsplit(host_value)
        if (
            parsed_host.scheme != "https"
            or parsed_host.hostname is None
            or parsed_host.username is None
            or parsed_host.password is None
            or parsed_host.path not in {"", "/"}
            or parsed_host.query
            or parsed_host.fragment
        ):
            raise SystemExit(
                f"{environment_names[2]} must contain a credentialed HTTPS mc host URL"
            )
        no_proxy = ",".join(
            dict.fromkeys((parsed_host.hostname, "127.0.0.1", "localhost"))
        )
        try:
            object_lock_options = object_lock.S3ObjectLockOptions(
                repo_root=pathlib.Path.cwd().resolve(),
                config_dir=pathlib.Path(config_dir_value).expanduser(),
                alias=alias,
                bucket=str(parsed.object_lock_bucket),
                prefix=str(parsed.object_lock_prefix),
                retention_days=int(parsed.retention_days),
                mc_bin=str(parsed.object_lock_mc_bin),
                resolve=tuple(
                    item.strip() for item in resolve_value.split(",") if item.strip()
                ),
                subprocess_environment={
                    f"MC_HOST_{alias}": host_value,
                    "NO_PROXY": no_proxy,
                    "no_proxy": no_proxy,
                },
            )
            object_lock.S3ObjectLockClient(object_lock_options)
        except ValueError as error:
            raise SystemExit(f"Object Lock configuration was invalid: {error}") from None
        object_lock_environment_names = environment_names
    return SinkOptions(
        bind_host=str(parsed.bind_host),
        port=int(parsed.port),
        state_dir=pathlib.Path(parsed.state_dir).expanduser().resolve(),
        server_cert_path=pathlib.Path(parsed.server_cert).expanduser().resolve(),
        server_key_path=pathlib.Path(parsed.server_key).expanduser().resolve(),
        client_ca_cert_path=pathlib.Path(parsed.client_ca_cert).expanduser().resolve(),
        retention_days=int(parsed.retention_days),
        max_body_bytes=int(parsed.max_body_bytes),
        object_lock_options=object_lock_options,
        object_lock_environment_names=object_lock_environment_names,
    )


def _required_environment_value(name: str, *, maximum_length: int) -> str:
    value = os.environ.get(name)
    if value is None or not value.strip():
        raise SystemExit(f"{name} must be set when --object-lock-required is used")
    if len(value) > maximum_length or any(character in value for character in "\r\n\x00"):
        raise SystemExit(f"{name} contained an invalid value")
    return value.strip()


def main(argv: Sequence[str] | None = None) -> int:
    options = parse_args(argv)
    server = create_server(options)
    address = server.server_address
    print(f"Vault audit acceptance sink listening on https://{address[0]}:{address[1]}")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        return 130
    finally:
        server.server_close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
