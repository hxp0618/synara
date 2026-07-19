#!/usr/bin/env python3
"""Verify external Vault audit SIEM delivery without exposing secrets."""

from __future__ import annotations

import argparse
import dataclasses
import datetime as dt
import http.client
import json
import os
import pathlib
import re
import socket
import ssl
import subprocess
import sys
import tempfile
import time
import urllib.parse
import uuid
from collections.abc import Mapping, Sequence
from typing import Any

import acceptance_runner as acceptance
import release_gate_common as common
import s3_object_lock as object_lock


SCHEMA_VERSION = "synara.vault-audit-siem-delivery-gate.v1"
JSON_REPORT_NAME = "vault-audit-siem-delivery-gate.json"
MARKDOWN_REPORT_NAME = "vault-audit-siem-delivery-gate.md"
DEFAULT_OPERATIONS_POLICY_PATH = pathlib.Path("deploy/kubernetes/security/vault/operations-policy.json")
DEFAULT_TIMEOUT_SECONDS = 60.0
DEFAULT_POLL_INTERVAL_SECONDS = 2.0
DEFAULT_VAULT_AUDITOR_TOKEN_ENV = "VAULT_OPERATOR_TOKEN"
DEFAULT_VAULT_NAMESPACE = "synara-kms"
DEFAULT_VAULT_STATEFULSET = "synara-vault"
DEFAULT_SHIPPER_CONTAINER = "vault-audit-shipper"
DEFAULT_REQUEST_PATH = "sys/audit"
OUTPUT_EXCERPT_LIMIT = 2000
ENVIRONMENT_NAME_PATTERN = re.compile(r"[A-Z][A-Z0-9_]{0,127}")
PEM_CERTIFICATE_PATTERN = re.compile(
    r"-----BEGIN CERTIFICATE-----\n.+?\n-----END CERTIFICATE-----\n?",
    re.DOTALL,
)
PEM_PRIVATE_KEY_PATTERN = re.compile(
    r"-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----\n.+?\n-----END [A-Z0-9 ]*PRIVATE KEY-----\n?",
    re.DOTALL,
)
SHA256_HEX_PATTERN = re.compile(r"[0-9a-f]{64}")
REQUIRED_OBJECT_LOCK_BUCKET_ACTIONS = {
    "s3:GetBucketLocation",
    "s3:GetBucketObjectLockConfiguration",
    "s3:GetBucketVersioning",
    "s3:ListBucket",
    "s3:ListBucketVersions",
}
REQUIRED_OBJECT_LOCK_OBJECT_ACTIONS = {
    "s3:GetObject",
    "s3:GetObjectRetention",
    "s3:GetObjectVersion",
    "s3:PutObject",
}

ReleaseGateError = common.ReleaseGateError


@dataclasses.dataclass(frozen=True)
class GateOptions:
    repo_root: pathlib.Path
    output_dir: pathlib.Path
    operations_policy_path: pathlib.Path
    vault_command: tuple[str, ...]
    vault_auditor_token_env: str
    kubectl_bin: str
    kube_context: str | None
    vault_namespace: str
    vault_statefulset: str
    shipper_container: str
    timeout_seconds: float
    poll_interval_seconds: float
    mc_bin: str = "mc"


@dataclasses.dataclass(frozen=True)
class OperationsPolicy:
    path: pathlib.Path
    sha256: str
    vault_addr_env: str
    vault_cacert_env: str
    configured_transit_audit_path: str
    auditor_role_name: str
    sink_endpoint_env: str
    sink_resolve_env: str
    sink_client_cert_env: str
    sink_client_key_env: str
    sink_ca_cert_env: str
    sink_transport: str
    sink_image: str
    sink_config_map_name: str
    sink_secret_name: str
    sink_delivery_slo: str
    retention_requirement: str
    object_lock_provider: str
    object_lock_mode: str
    object_lock_retention_days: int
    object_lock_bucket: str
    object_lock_prefix: str
    object_lock_alias_env: str
    object_lock_config_dir_env: str
    object_lock_host_env: str
    object_lock_verifier_host_env: str
    object_lock_resolve_env: str
    object_lock_credential_policy_path: pathlib.Path
    object_lock_credential_policy_sha256: str


@dataclasses.dataclass(frozen=True)
class SecretInputs:
    vault_address: str
    vault_cacert_value: str
    vault_cacert_runtime_value: str
    vault_cacert_environment: str
    auditor_token: str
    auditor_token_environment: str
    sink_endpoint: str
    sink_endpoint_environment: str
    sink_connect_host: str
    sink_client_cert_path: pathlib.Path
    sink_client_key_path: pathlib.Path
    sink_ca_cert_path: pathlib.Path
    sink_client_certificate_sha256: str
    object_lock_alias: str
    object_lock_config_dir: pathlib.Path
    object_lock_writer_host: str
    object_lock_verifier_host: str
    object_lock_resolve: tuple[str, ...]
    temporary_paths: tuple[pathlib.Path, ...]


@dataclasses.dataclass(frozen=True)
class ObjectLockClients:
    writer: object_lock.S3ObjectLockClient
    verifier: object_lock.S3ObjectLockClient


def utc_now() -> str:
    return dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def sha256_bytes(value: bytes) -> str:
    import hashlib

    return hashlib.sha256(value).hexdigest()


def stable_json_text(value: Any) -> str:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False)


def ensure_private_directory(path: pathlib.Path) -> pathlib.Path:
    path.mkdir(parents=True, exist_ok=True)
    path.chmod(0o700)
    return path


def fsync_directory(path: pathlib.Path) -> None:
    descriptor = os.open(str(path), os.O_RDONLY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def _require_mapping(value: Any, *, label: str, code: str) -> Mapping[str, Any]:
    if not isinstance(value, Mapping):
        raise ReleaseGateError(code, f"The {label} entry was not a JSON object.")
    return value


def _require_string(value: Any, *, label: str, code: str) -> str:
    if not isinstance(value, str) or not value.strip():
        raise ReleaseGateError(code, f"The {label} entry was not a non-empty string.")
    return value.strip()


def _require_environment_name(value: Any, *, label: str, code: str) -> str:
    name = _require_string(value, label=label, code=code)
    if ENVIRONMENT_NAME_PATTERN.fullmatch(name) is None:
        raise ReleaseGateError(code, f"The {label} entry was not a valid environment variable name.")
    return name


def parse_command_json(value: str) -> tuple[str, ...]:
    try:
        decoded = json.loads(value)
    except json.JSONDecodeError:
        raise SystemExit("--vault-command-json must be a JSON string array") from None
    if not isinstance(decoded, list) or not decoded:
        raise SystemExit("--vault-command-json must be a non-empty JSON string array")
    command: list[str] = []
    for index, part in enumerate(decoded):
        if not isinstance(part, str) or not part.strip() or any(character in part for character in "\r\n\x00"):
            raise SystemExit(f"--vault-command-json entry {index} was not a valid string")
        command.append(part)
    return tuple(command)


def default_output_dir(repo_root: pathlib.Path) -> pathlib.Path:
    run_id = dt.datetime.now(dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ") + "-" + uuid.uuid4().hex[:8]
    return repo_root / ".tmp" / "stage3-provider-acceptance-results" / f"{run_id}-vault-audit-siem-delivery"


def parse_args(argv: Sequence[str] | None = None) -> GateOptions:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=pathlib.Path)
    parser.add_argument("--operations-policy", type=pathlib.Path, default=DEFAULT_OPERATIONS_POLICY_PATH)
    parser.add_argument("--vault-command-json", default='["vault"]')
    parser.add_argument("--vault-auditor-token-env", default=DEFAULT_VAULT_AUDITOR_TOKEN_ENV)
    parser.add_argument("--kubectl-bin", default="kubectl")
    parser.add_argument("--kube-context")
    parser.add_argument("--vault-namespace", default=DEFAULT_VAULT_NAMESPACE)
    parser.add_argument("--vault-statefulset", default=DEFAULT_VAULT_STATEFULSET)
    parser.add_argument("--shipper-container", default=DEFAULT_SHIPPER_CONTAINER)
    parser.add_argument("--timeout-seconds", type=float, default=DEFAULT_TIMEOUT_SECONDS)
    parser.add_argument("--poll-interval-seconds", type=float, default=DEFAULT_POLL_INTERVAL_SECONDS)
    parser.add_argument("--mc-bin", default="mc")
    parsed = parser.parse_args(argv)
    repo_root = pathlib.Path.cwd().resolve()
    output_dir = parsed.output_dir or default_output_dir(repo_root)
    if not output_dir.is_absolute():
        output_dir = (repo_root / output_dir).resolve()
    operations_policy = pathlib.Path(parsed.operations_policy).expanduser()
    if not operations_policy.is_absolute():
        operations_policy = (repo_root / operations_policy).resolve()
    if parsed.timeout_seconds <= 0:
        raise SystemExit("--timeout-seconds must be positive")
    if parsed.poll_interval_seconds <= 0:
        raise SystemExit("--poll-interval-seconds must be positive")
    return GateOptions(
        repo_root=repo_root,
        output_dir=output_dir,
        operations_policy_path=operations_policy,
        vault_command=parse_command_json(str(parsed.vault_command_json)),
        vault_auditor_token_env=_require_environment_name(
            parsed.vault_auditor_token_env,
            label="--vault-auditor-token-env",
            code="release.vault_audit_siem_arguments_invalid",
        ),
        kubectl_bin=str(parsed.kubectl_bin),
        kube_context=str(parsed.kube_context).strip() or None if parsed.kube_context is not None else None,
        vault_namespace=str(parsed.vault_namespace).strip() or DEFAULT_VAULT_NAMESPACE,
        vault_statefulset=str(parsed.vault_statefulset).strip() or DEFAULT_VAULT_STATEFULSET,
        shipper_container=str(parsed.shipper_container).strip() or DEFAULT_SHIPPER_CONTAINER,
        timeout_seconds=float(parsed.timeout_seconds),
        poll_interval_seconds=float(parsed.poll_interval_seconds),
        mc_bin=str(parsed.mc_bin),
    )


def _load_object_lock_credential_policy(
    repo_root: pathlib.Path,
    value: Any,
    *,
    bucket: str,
    code: str,
) -> tuple[pathlib.Path, str]:
    relative = _require_string(
        value,
        label="audit.externalSiem.objectLock.credentialPolicyPath",
        code=code,
    )
    pure_path = pathlib.PurePosixPath(relative)
    if pure_path.is_absolute() or ".." in pure_path.parts or any(
        part in {"", "."} for part in pure_path.parts
    ):
        raise ReleaseGateError(code, "The Object Lock credential policy path was not repository-relative.")
    path = repo_root.joinpath(*pure_path.parts)
    try:
        raw = path.read_bytes()
        payload = json.loads(raw)
    except (OSError, json.JSONDecodeError):
        raise ReleaseGateError(code, "The Object Lock credential policy was unavailable or malformed.") from None
    statements = payload.get("Statement") if isinstance(payload, dict) else None
    if not isinstance(statements, list) or len(statements) != 2:
        raise ReleaseGateError(code, "The Object Lock credential policy did not use the exact two-statement boundary.")
    actions_by_resource: dict[str, set[str]] = {}
    for statement in statements:
        if not isinstance(statement, dict) or statement.get("Effect") != "Allow":
            raise ReleaseGateError(code, "The Object Lock credential policy statement was invalid.")
        resource = statement.get("Resource")
        actions = statement.get("Action")
        if not isinstance(resource, str) or not isinstance(actions, list) or not all(
            isinstance(action, str) for action in actions
        ):
            raise ReleaseGateError(code, "The Object Lock credential policy actions were invalid.")
        actions_by_resource[resource] = set(actions)
    if actions_by_resource != {
        f"arn:aws:s3:::{bucket}": REQUIRED_OBJECT_LOCK_BUCKET_ACTIONS,
        f"arn:aws:s3:::{bucket}/*": REQUIRED_OBJECT_LOCK_OBJECT_ACTIONS,
    }:
        raise ReleaseGateError(
            code,
            "The Object Lock credential policy exceeded or drifted from the scoped bucket boundary.",
        )
    return path, sha256_bytes(raw)


def load_operations_policy(policy_path: pathlib.Path) -> OperationsPolicy:
    code = "release.vault_audit_siem_policy_invalid"
    try:
        raw = policy_path.read_bytes()
    except OSError:
        raise ReleaseGateError(code, "The Vault operations policy file was unavailable.", {"path": str(policy_path)}) from None
    try:
        payload = json.loads(raw)
    except json.JSONDecodeError:
        raise ReleaseGateError(code, "The Vault operations policy file was not valid JSON.", {"path": str(policy_path)}) from None
    root = _require_mapping(payload, label="operations policy", code=code)
    if _require_string(root.get("schemaVersion"), label="schemaVersion", code=code) != "synara.vault-kms-operations-policy.v1":
        raise ReleaseGateError(code, "The operations policy schemaVersion was not the expected Synara Vault schema.")
    signing = _require_mapping(root.get("signing"), label="signing", code=code)
    vault = _require_mapping(root.get("vault"), label="vault", code=code)
    audit = _require_mapping(root.get("audit"), label="audit", code=code)
    external_siem = _require_mapping(audit.get("externalSiem"), label="audit.externalSiem", code=code)
    object_lock_policy = _require_mapping(
        external_siem.get("objectLock"),
        label="audit.externalSiem.objectLock",
        code=code,
    )
    credential_environment = signing.get("credentialEnvironment")
    if not isinstance(credential_environment, list):
        raise ReleaseGateError(code, "The signing.credentialEnvironment entry was not a string list.")
    signing_env_names = [
        _require_environment_name(
            item,
            label=f"signing.credentialEnvironment[{index}]",
            code=code,
        )
        for index, item in enumerate(credential_environment)
    ]
    if "VAULT_ADDR" not in signing_env_names or "VAULT_CACERT" not in signing_env_names:
        raise ReleaseGateError(
            code,
            "The operations policy did not declare the expected VAULT_ADDR and VAULT_CACERT environment names.",
        )
    required = external_siem.get("required")
    if required is not True:
        raise ReleaseGateError(code, "The operations policy did not require the external SIEM delivery path.")
    object_lock_provider = _require_string(
        object_lock_policy.get("provider"),
        label="audit.externalSiem.objectLock.provider",
        code=code,
    )
    object_lock_mode = _require_string(
        object_lock_policy.get("mode"),
        label="audit.externalSiem.objectLock.mode",
        code=code,
    )
    retention_days = object_lock_policy.get("retentionDays")
    object_lock_bucket = _require_string(
        object_lock_policy.get("bucket"),
        label="audit.externalSiem.objectLock.bucket",
        code=code,
    )
    object_lock_prefix = _require_string(
        object_lock_policy.get("objectPrefix"),
        label="audit.externalSiem.objectLock.objectPrefix",
        code=code,
    )
    if (
        object_lock_provider != "s3-compatible"
        or object_lock_mode != "COMPLIANCE"
        or not isinstance(retention_days, int)
        or isinstance(retention_days, bool)
        or retention_days <= 1
        or re.fullmatch(r"[a-z0-9][a-z0-9.-]{1,62}", object_lock_bucket) is None
        or object_lock_prefix != object_lock_prefix.strip("/")
        or any(part in {"", ".", ".."} for part in object_lock_prefix.split("/"))
    ):
        raise ReleaseGateError(code, "The external SIEM Object Lock policy was not a valid COMPLIANCE boundary.")
    repo_root = policy_path.resolve().parents[4]
    credential_policy_path, credential_policy_sha256 = _load_object_lock_credential_policy(
        repo_root,
        object_lock_policy.get("credentialPolicyPath"),
        bucket=object_lock_bucket,
        code=code,
    )
    return OperationsPolicy(
        path=policy_path,
        sha256=sha256_bytes(raw),
        vault_addr_env="VAULT_ADDR",
        vault_cacert_env="VAULT_CACERT",
        configured_transit_audit_path=_require_string(
            signing.get("auditRequestPath"),
            label="signing.auditRequestPath",
            code=code,
        ),
        auditor_role_name=_require_string(
            vault.get("auditorAppRoleName"),
            label="vault.auditorAppRoleName",
            code=code,
        ),
        sink_endpoint_env=_require_environment_name(
            external_siem.get("endpointEnvironment"),
            label="audit.externalSiem.endpointEnvironment",
            code=code,
        ),
        sink_resolve_env=_require_environment_name(
            external_siem.get("resolveEnvironment"),
            label="audit.externalSiem.resolveEnvironment",
            code=code,
        ),
        sink_client_cert_env=_require_environment_name(
            external_siem.get("clientCertificateEnvironment"),
            label="audit.externalSiem.clientCertificateEnvironment",
            code=code,
        ),
        sink_client_key_env=_require_environment_name(
            external_siem.get("clientKeyEnvironment"),
            label="audit.externalSiem.clientKeyEnvironment",
            code=code,
        ),
        sink_ca_cert_env=_require_environment_name(
            external_siem.get("caCertificateEnvironment"),
            label="audit.externalSiem.caCertificateEnvironment",
            code=code,
        ),
        sink_transport=_require_string(
            external_siem.get("transport"),
            label="audit.externalSiem.transport",
            code=code,
        ),
        sink_image=_require_string(
            external_siem.get("image"),
            label="audit.externalSiem.image",
            code=code,
        ),
        sink_config_map_name=_require_string(
            external_siem.get("configMapName"),
            label="audit.externalSiem.configMapName",
            code=code,
        ),
        sink_secret_name=_require_string(
            external_siem.get("secretName"),
            label="audit.externalSiem.secretName",
            code=code,
        ),
        sink_delivery_slo=_require_string(
            external_siem.get("deliverySlo"),
            label="audit.externalSiem.deliverySlo",
            code=code,
        ),
        retention_requirement=_require_string(
            external_siem.get("retentionRequirement"),
            label="audit.externalSiem.retentionRequirement",
            code=code,
        ),
        object_lock_provider=object_lock_provider,
        object_lock_mode=object_lock_mode,
        object_lock_retention_days=retention_days,
        object_lock_bucket=object_lock_bucket,
        object_lock_prefix=object_lock_prefix,
        object_lock_alias_env=_require_environment_name(
            object_lock_policy.get("mcAliasEnvironment"),
            label="audit.externalSiem.objectLock.mcAliasEnvironment",
            code=code,
        ),
        object_lock_config_dir_env=_require_environment_name(
            object_lock_policy.get("mcConfigDirectoryEnvironment"),
            label="audit.externalSiem.objectLock.mcConfigDirectoryEnvironment",
            code=code,
        ),
        object_lock_host_env=_require_environment_name(
            object_lock_policy.get("mcHostEnvironment"),
            label="audit.externalSiem.objectLock.mcHostEnvironment",
            code=code,
        ),
        object_lock_verifier_host_env=_require_environment_name(
            object_lock_policy.get("mcVerifierHostEnvironment"),
            label="audit.externalSiem.objectLock.mcVerifierHostEnvironment",
            code=code,
        ),
        object_lock_resolve_env=_require_environment_name(
            object_lock_policy.get("mcResolveEnvironment"),
            label="audit.externalSiem.objectLock.mcResolveEnvironment",
            code=code,
        ),
        object_lock_credential_policy_path=credential_policy_path,
        object_lock_credential_policy_sha256=credential_policy_sha256,
    )


def _read_environment_value(name: str, description: str) -> str:
    try:
        return acceptance.read_environment_value(
            name,
            description,
            maximum_length=256 << 10,
            forbidden_characters="\x00",
        )
    except acceptance.EnvironmentValueError as error:
        raise ReleaseGateError(
            "release.vault_audit_siem_environment_invalid",
            error.args[1] if len(error.args) > 1 else f"The configured {description} environment variable was invalid.",
            {"environment": name},
        ) from None


def _extract_first_certificate(pem_bundle: str) -> str:
    match = PEM_CERTIFICATE_PATTERN.search(pem_bundle)
    if match is None:
        raise ReleaseGateError(
            "release.vault_audit_siem_environment_invalid",
            "The configured client certificate PEM did not contain a certificate block.",
        )
    return match.group(0)


def _require_private_key_pem(pem_text: str) -> str:
    if PEM_PRIVATE_KEY_PATTERN.search(pem_text) is None:
        raise ReleaseGateError(
            "release.vault_audit_siem_environment_invalid",
            "The configured client key PEM did not contain a private key block.",
        )
    return pem_text


def _certificate_sha256(pem_text: str) -> str:
    return sha256_bytes(ssl.PEM_cert_to_DER_cert(_extract_first_certificate(pem_text)))


def _looks_like_pem_certificate(value: str) -> bool:
    return PEM_CERTIFICATE_PATTERN.search(value) is not None


def _parse_credentialed_https_authority(
    value: str,
    *,
    environment_name: str,
    description: str,
) -> urllib.parse.SplitResult:
    parsed = urllib.parse.urlsplit(value)
    if (
        parsed.scheme != "https"
        or parsed.hostname is None
        or parsed.username is None
        or parsed.password is None
        or parsed.path not in {"", "/"}
        or parsed.query
        or parsed.fragment
    ):
        raise ReleaseGateError(
            "release.vault_audit_siem_environment_invalid",
            f"The {description} did not use a credentialed HTTPS authority.",
            {"environment": environment_name},
        )
    return parsed


def _object_lock_identity(parsed: urllib.parse.SplitResult) -> tuple[str, int | None, str]:
    return (
        str(parsed.hostname or "").casefold(),
        parsed.port,
        str(parsed.username or ""),
    )


def _redact_credentialed_authority_components(
    redactor: acceptance.SecretRedactor,
    parsed: urllib.parse.SplitResult,
    *,
    username_replacement: str,
    password_replacement: str,
) -> None:
    for value, replacement in (
        (parsed.username, username_replacement),
        (parsed.password, password_replacement),
    ):
        if not value:
            continue
        redactor.add(value, replacement)
        decoded = urllib.parse.unquote(value)
        if decoded != value:
            redactor.add(decoded, replacement)


def _materialize_private_file(
    state_dir: pathlib.Path,
    filename: str,
    content: str,
) -> pathlib.Path:
    ensure_private_directory(state_dir)
    path = state_dir / filename
    with path.open("w", encoding="utf-8") as handle:
        handle.write(content)
        if not content.endswith("\n"):
            handle.write("\n")
        handle.flush()
        os.fchmod(handle.fileno(), 0o600)
        os.fsync(handle.fileno())
    fsync_directory(state_dir)
    return path


def prepare_secret_inputs(
    policy: OperationsPolicy,
    options: GateOptions,
    *,
    state_dir: pathlib.Path,
    redactor: acceptance.SecretRedactor,
) -> SecretInputs:
    vault_address = _read_environment_value(policy.vault_addr_env, "Vault address")
    vault_cacert_value = _read_environment_value(policy.vault_cacert_env, "Vault CA certificate")
    auditor_token = _read_environment_value(options.vault_auditor_token_env, "Vault auditor token")
    sink_endpoint = _read_environment_value(policy.sink_endpoint_env, "Vault audit SIEM endpoint")
    sink_resolve = _read_environment_value(policy.sink_resolve_env, "Vault audit SIEM resolve mapping")
    sink_client_cert = _read_environment_value(policy.sink_client_cert_env, "Vault audit SIEM client certificate")
    sink_client_key = _read_environment_value(policy.sink_client_key_env, "Vault audit SIEM client key")
    sink_ca_cert = _read_environment_value(policy.sink_ca_cert_env, "Vault audit SIEM CA certificate")
    object_lock_alias = _read_environment_value(policy.object_lock_alias_env, "Object Lock mc alias")
    object_lock_config_dir_value = _read_environment_value(
        policy.object_lock_config_dir_env,
        "Object Lock mc configuration directory",
    )
    object_lock_writer_host = _read_environment_value(
        policy.object_lock_host_env,
        "Object Lock writer mc host credential",
    )
    object_lock_verifier_host = _read_environment_value(
        policy.object_lock_verifier_host_env,
        "Object Lock negative-probe verifier mc host credential",
    )
    object_lock_resolve_value = _read_environment_value(
        policy.object_lock_resolve_env,
        "Object Lock mc resolve mapping",
    )
    sink_client_cert = _extract_first_certificate(sink_client_cert)
    sink_client_key = _require_private_key_pem(sink_client_key)
    sink_ca_cert = _extract_first_certificate(sink_ca_cert)
    parsed_sink_endpoint = urllib.parse.urlsplit(sink_endpoint)
    if (
        parsed_sink_endpoint.scheme != "https"
        or parsed_sink_endpoint.hostname is None
        or parsed_sink_endpoint.port is None
        or parsed_sink_endpoint.username is not None
        or parsed_sink_endpoint.password is not None
        or parsed_sink_endpoint.path not in {"", "/"}
        or parsed_sink_endpoint.query
        or parsed_sink_endpoint.fragment
        or sink_resolve.count("=") != 1
    ):
        raise ReleaseGateError(
            "release.vault_audit_siem_environment_invalid",
            "The Vault audit SIEM endpoint or resolve mapping was invalid.",
        )
    resolve_source, sink_connect_host = (item.strip() for item in sink_resolve.split("=", 1))
    if (
        resolve_source != parsed_sink_endpoint.netloc
        or not sink_connect_host
        or any(character.isspace() or ord(character) < 32 for character in sink_connect_host)
        or any(character in sink_connect_host for character in "/?#@")
    ):
        raise ReleaseGateError(
            "release.vault_audit_siem_environment_invalid",
            "The Vault audit SIEM resolve mapping did not match the endpoint authority.",
            {"environment": policy.sink_resolve_env},
        )
    redactor.add(auditor_token, "[REDACTED_VAULT_OPERATOR_TOKEN]")
    redactor.add(sink_client_cert, "[REDACTED_VAULT_AUDIT_SIEM_CLIENT_CERT]")
    redactor.add(sink_client_key, "[REDACTED_VAULT_AUDIT_SIEM_CLIENT_KEY]")
    redactor.add(sink_ca_cert, "[REDACTED_VAULT_AUDIT_SIEM_CA_CERT]")
    redactor.add(object_lock_writer_host, "[REDACTED_VAULT_AUDIT_WORM_MC_HOST]")
    redactor.add(
        object_lock_verifier_host,
        "[REDACTED_VAULT_AUDIT_WORM_MC_VERIFIER_HOST]",
    )
    parsed_object_lock_writer_host = _parse_credentialed_https_authority(
        object_lock_writer_host,
        environment_name=policy.object_lock_host_env,
        description="Object Lock writer mc host credential",
    )
    parsed_object_lock_verifier_host = _parse_credentialed_https_authority(
        object_lock_verifier_host,
        environment_name=policy.object_lock_verifier_host_env,
        description="Object Lock verifier mc host credential",
    )
    _redact_credentialed_authority_components(
        redactor,
        parsed_object_lock_writer_host,
        username_replacement="[REDACTED_VAULT_AUDIT_WORM_MC_HOST_USERNAME]",
        password_replacement="[REDACTED_VAULT_AUDIT_WORM_MC_HOST_PASSWORD]",
    )
    _redact_credentialed_authority_components(
        redactor,
        parsed_object_lock_verifier_host,
        username_replacement="[REDACTED_VAULT_AUDIT_WORM_MC_VERIFIER_HOST_USERNAME]",
        password_replacement="[REDACTED_VAULT_AUDIT_WORM_MC_VERIFIER_HOST_PASSWORD]",
    )
    if _object_lock_identity(parsed_object_lock_writer_host) == _object_lock_identity(
        parsed_object_lock_verifier_host
    ):
        raise ReleaseGateError(
            "release.vault_audit_siem_environment_invalid",
            "The Object Lock writer and verifier credentials must resolve to distinct identities.",
            {
                "writerEnvironment": policy.object_lock_host_env,
                "verifierEnvironment": policy.object_lock_verifier_host_env,
            },
        )
    temporary_paths = [
        _materialize_private_file(state_dir, "sink-client.crt", sink_client_cert),
        _materialize_private_file(state_dir, "sink-client.key", sink_client_key),
        _materialize_private_file(state_dir, "sink-ca.crt", sink_ca_cert),
    ]
    vault_cacert_runtime_value = vault_cacert_value
    if _looks_like_pem_certificate(vault_cacert_value):
        redactor.add(vault_cacert_value, "[REDACTED_VAULT_CACERT_PEM]")
        vault_cacert_runtime_value = str(
            _materialize_private_file(state_dir, "vault-cacert.crt", vault_cacert_value)
        )
        temporary_paths.append(pathlib.Path(vault_cacert_runtime_value))
    return SecretInputs(
        vault_address=vault_address,
        vault_cacert_value=vault_cacert_value,
        vault_cacert_runtime_value=vault_cacert_runtime_value,
        vault_cacert_environment=policy.vault_cacert_env,
        auditor_token=auditor_token,
        auditor_token_environment=options.vault_auditor_token_env,
        sink_endpoint=sink_endpoint,
        sink_endpoint_environment=policy.sink_endpoint_env,
        sink_connect_host=sink_connect_host,
        sink_client_cert_path=temporary_paths[0],
        sink_client_key_path=temporary_paths[1],
        sink_ca_cert_path=temporary_paths[2],
        sink_client_certificate_sha256=_certificate_sha256(sink_client_cert),
        object_lock_alias=object_lock_alias,
        object_lock_config_dir=pathlib.Path(object_lock_config_dir_value).expanduser(),
        object_lock_writer_host=object_lock_writer_host,
        object_lock_verifier_host=object_lock_verifier_host,
        object_lock_resolve=tuple(
            item.strip() for item in object_lock_resolve_value.split(",") if item.strip()
        ),
        temporary_paths=tuple(temporary_paths),
    )


def _build_object_lock_client(
    policy: OperationsPolicy,
    secret_inputs: SecretInputs,
    options: GateOptions,
    *,
    host_environment_name: str,
    host_value: str,
) -> object_lock.S3ObjectLockClient:
    parsed_host = urllib.parse.urlsplit(host_value)
    no_proxy = ",".join(
        dict.fromkeys((str(parsed_host.hostname), "127.0.0.1", "localhost"))
    )
    try:
        return object_lock.S3ObjectLockClient(
            object_lock.S3ObjectLockOptions(
                repo_root=options.repo_root,
                config_dir=secret_inputs.object_lock_config_dir,
                alias=secret_inputs.object_lock_alias,
                bucket=policy.object_lock_bucket,
                prefix=policy.object_lock_prefix,
                retention_days=policy.object_lock_retention_days,
                mc_bin=options.mc_bin,
                timeout_seconds=options.timeout_seconds,
                resolve=secret_inputs.object_lock_resolve,
                subprocess_environment={
                    f"MC_HOST_{secret_inputs.object_lock_alias}": host_value,
                    "NO_PROXY": no_proxy,
                    "no_proxy": no_proxy,
                },
            )
        )
    except ValueError as error:
        raise ReleaseGateError(
            "release.vault_audit_siem_object_lock_invalid",
            "The external Object Lock verifier configuration was invalid.",
            {"environment": host_environment_name, "reason": str(error)},
        ) from None


def build_object_lock_clients(
    policy: OperationsPolicy,
    secret_inputs: SecretInputs,
    options: GateOptions,
) -> ObjectLockClients:
    return ObjectLockClients(
        writer=_build_object_lock_client(
            policy,
            secret_inputs,
            options,
            host_environment_name=policy.object_lock_host_env,
            host_value=secret_inputs.object_lock_writer_host,
        ),
        verifier=_build_object_lock_client(
            policy,
            secret_inputs,
            options,
            host_environment_name=policy.object_lock_verifier_host_env,
            host_value=secret_inputs.object_lock_verifier_host,
        ),
    )


def _parse_utc_timestamp(value: str, *, label: str) -> dt.datetime:
    normalized = value.strip()
    if normalized.endswith("Z"):
        normalized = f"{normalized[:-1]}+00:00"
    try:
        parsed = dt.datetime.fromisoformat(normalized)
    except ValueError:
        raise ReleaseGateError(
            "release.vault_audit_siem_object_lock_invalid",
            f"The {label} timestamp was invalid.",
        ) from None
    if parsed.tzinfo is None:
        raise ReleaseGateError(
            "release.vault_audit_siem_object_lock_invalid",
            f"The {label} timestamp omitted its timezone.",
        )
    return parsed.astimezone(dt.timezone.utc)


def _require_sha256_digest(value: Any, *, label: str, code: str) -> str:
    digest = _require_string(value, label=label, code=code)
    if SHA256_HEX_PATTERN.fullmatch(digest) is None:
        raise ReleaseGateError(code, f"The {label} value was not a lowercase SHA-256 digest.")
    return digest


def _require_positive_integer(value: Any, *, label: str, code: str) -> int:
    if not isinstance(value, int) or isinstance(value, bool) or value <= 0:
        raise ReleaseGateError(code, f"The {label} value was not a positive integer.")
    return value


def _stable_json_bytes(value: Any) -> bytes:
    return stable_json_text(value).encode("utf-8")


def _canonical_entry_sha256(entry: Mapping[str, Any]) -> str:
    unsigned = dict(entry)
    unsigned.pop("entrySha256", None)
    return sha256_bytes(_stable_json_bytes(unsigned))


def _payload_identity(payload: Mapping[str, Any], *, code: str) -> dict[str, Any]:
    request = _require_mapping(payload.get("request"), label="archive payload request", code=code)
    request_id = _require_string(
        request.get("id"),
        label="archive payload request.id",
        code=code,
    )
    request_path = _require_string(
        request.get("path"),
        label="archive payload request.path",
        code=code,
    )
    request_operation = _require_string(
        request.get("operation"),
        label="archive payload request.operation",
        code=code,
    )
    audit_type = _require_string(
        payload.get("type"),
        label="archive payload type",
        code=code,
    )
    event_time = _require_string(
        payload.get("time"),
        label="archive payload time",
        code=code,
    )
    namespace_id = None
    namespace = payload.get("namespace")
    if isinstance(namespace, Mapping):
        raw_namespace_id = namespace.get("id")
        namespace_id = raw_namespace_id if isinstance(raw_namespace_id, str) and raw_namespace_id else None
    return {
        "requestId": request_id,
        "path": request_path,
        "operation": request_operation,
        "type": audit_type,
        "eventTime": event_time,
        "namespaceId": namespace_id,
    }


def verify_object_lock_archive(
    *,
    receipt: Mapping[str, Any],
    policy: OperationsPolicy,
    secret_inputs: SecretInputs,
    options: GateOptions,
) -> dict[str, Any]:
    code = "release.vault_audit_siem_object_lock_invalid"
    archive = _require_mapping(receipt.get("archive"), label="sink receipt archive", code=code)
    receipt_audit = _require_mapping(receipt.get("audit"), label="sink receipt audit", code=code)
    retention = _require_mapping(
        receipt.get("retention"),
        label="sink receipt retention",
        code=code,
    )
    entry_sha256 = _require_sha256_digest(
        receipt.get("entrySha256"),
        label="sink receipt entrySha256",
        code=code,
    )
    receipt_payload_sha256 = _require_sha256_digest(
        receipt.get("payloadSha256"),
        label="sink receipt payloadSha256",
        code=code,
    )
    object_key = _require_string(
        archive.get("objectKey"),
        label="sink receipt archive.objectKey",
        code=code,
    )
    batch_sha256 = _require_sha256_digest(
        archive.get("batchContentSha256"),
        label="sink receipt archive.batchContentSha256",
        code=code,
    )
    batch_entry_count = _require_positive_integer(
        archive.get("batchEntryCount"),
        label="sink receipt archive.batchEntryCount",
        code=code,
    )
    if (
        archive.get("provider") != "s3-compatible-object-lock"
        or archive.get("bucket") != policy.object_lock_bucket
        or archive.get("mode") != policy.object_lock_mode
        or not object_key.startswith(f"{policy.object_lock_prefix}/")
        or retention.get("immutable") is not True
        or retention.get("storageEnforced") is not True
    ):
        raise ReleaseGateError(
            code,
            "The sink receipt did not bind the exact audit entry to the required Object Lock archive.",
        )
    clients = build_object_lock_clients(policy, secret_inputs, options)
    probe_object_key = clients.writer.qualify_object_key(
        f"gate-probe-{entry_sha256[:16]}.ndjson"
    )
    probe_bytes = (
        _stable_json_bytes(
            {
                "schemaVersion": SCHEMA_VERSION,
                "requestId": receipt_audit.get("requestId"),
                "path": receipt_audit.get("path"),
                "entrySha256": entry_sha256,
            }
        )
        + b"\n"
    )
    try:
        writer_contract = clients.writer.verify_bucket_contract()
        verifier_contract = clients.verifier.verify_bucket_contract()
        if writer_contract != verifier_contract:
            raise ReleaseGateError(
                code,
                "The Object Lock writer and verifier clients disagreed about the bucket contract.",
            )
        writer_probe = clients.writer.put_bytes(probe_object_key, probe_bytes)
        writer_delete_probe = clients.writer.probe_delete_version(
            writer_probe.object_key,
            writer_probe.version_id,
        )
        if not writer_delete_probe.blocked or writer_delete_probe.denial_kind != "iam":
            raise ReleaseGateError(
                code,
                "The Object Lock writer delete probe was not denied by IAM as required.",
                {
                    "deleteBlocked": writer_delete_probe.blocked,
                    "denialKind": writer_delete_probe.denial_kind,
                },
            )
        verifier_probe = clients.verifier.verify_existing_object(
            writer_probe.object_key,
            expected_content_sha256=writer_probe.content_sha256,
            version_id=writer_probe.version_id,
        )
        if (
            verifier_probe.version_id != writer_probe.version_id
            or verifier_probe.content_sha256 != writer_probe.content_sha256
        ):
            raise ReleaseGateError(
                code,
                "The Object Lock verifier could not read back the exact writer-owned probe version.",
            )
        verifier_delete_probe = clients.verifier.probe_delete_version(
            writer_probe.object_key,
            writer_probe.version_id,
        )
        verifier_shorten_probe = clients.verifier.probe_shorten_retention(
            writer_probe.object_key,
            writer_probe.version_id,
        )
        if (
            not verifier_delete_probe.blocked
            or verifier_delete_probe.denial_kind != "object_lock"
            or not verifier_shorten_probe.blocked
            or verifier_shorten_probe.denial_kind != "object_lock"
        ):
            raise ReleaseGateError(
                code,
                "The Object Lock verifier probe did not hit explicit COMPLIANCE/WORM enforcement.",
                {
                    "deleteBlocked": verifier_delete_probe.blocked,
                    "deleteDenialKind": verifier_delete_probe.denial_kind,
                    "shortenBlocked": verifier_shorten_probe.blocked,
                    "shortenDenialKind": verifier_shorten_probe.denial_kind,
                },
            )
        evidence = clients.verifier.verify_existing_object(
            object_key,
            expected_content_sha256=batch_sha256,
        )
        content = clients.verifier.cat_version(object_key, evidence.version_id)
        recomputed_batch_sha256 = sha256_bytes(content)
        if recomputed_batch_sha256 != batch_sha256:
            raise ReleaseGateError(
                code,
                "The exact Object Lock batch content did not match the sink receipt batch hash.",
            )
        archived_entries = [
            json.loads(line)
            for raw_line in content.decode("utf-8").splitlines()
            if (line := raw_line.strip())
        ]
    except (object_lock.S3ObjectLockError, UnicodeDecodeError, json.JSONDecodeError) as error:
        raise ReleaseGateError(
            code,
            "The external Object Lock archive could not independently verify the exact audit batch.",
            {"errorCode": getattr(error, "code", "s3_object_lock.archived_batch_invalid")},
        ) from None
    if len(archived_entries) != batch_entry_count:
        raise ReleaseGateError(
            code,
            "The exact Object Lock batch entry count did not match the sink receipt archive count.",
            {"expectedEntryCount": batch_entry_count, "actualEntryCount": len(archived_entries)},
        )
    matching_entries: list[tuple[Mapping[str, Any], str]] = []
    for entry in archived_entries:
        if not isinstance(entry, dict):
            raise ReleaseGateError(
                code,
                "The exact Object Lock batch contained a non-object entry.",
            )
        recomputed_entry_sha256 = _canonical_entry_sha256(entry)
        stored_entry_sha256 = _require_string(
            entry.get("entrySha256"),
            label="archive entry entrySha256",
            code=code,
        )
        if recomputed_entry_sha256 != stored_entry_sha256:
            raise ReleaseGateError(
                code,
                "The exact Object Lock batch entry hash did not match its canonical content.",
                {"ledgerIndex": entry.get("ledgerIndex")},
            )
        payload = _require_mapping(
            entry.get("payload"),
            label="archive entry payload",
            code=code,
        )
        recomputed_payload_sha256 = sha256_bytes(_stable_json_bytes(payload))
        stored_payload_sha256 = _require_sha256_digest(
            entry.get("payloadSha256"),
            label="archive entry payloadSha256",
            code=code,
        )
        if recomputed_payload_sha256 != stored_payload_sha256:
            raise ReleaseGateError(
                code,
                "The exact Object Lock batch payload hash did not match the structured payload content.",
                {"ledgerIndex": entry.get("ledgerIndex")},
            )
        payload_identity = _payload_identity(payload, code=code)
        archived_audit = _require_mapping(
            entry.get("audit"),
            label="archive entry audit",
            code=code,
        )
        for field, archived_key in (
            ("requestId", "requestId"),
            ("path", "path"),
            ("operation", "operation"),
            ("type", "type"),
            ("eventTime", "eventTime"),
            ("namespaceId", "namespaceId"),
        ):
            if payload_identity[field] != archived_audit.get(archived_key):
                raise ReleaseGateError(
                    code,
                    "The exact Object Lock batch payload drifted from the archived audit summary.",
                    {"ledgerIndex": entry.get("ledgerIndex"), "field": field},
                )
        if recomputed_entry_sha256 == entry_sha256:
            matching_entries.append((entry, recomputed_payload_sha256))
    if len(matching_entries) != 1:
        raise ReleaseGateError(
            code,
            "The exact Object Lock batch did not contain exactly one copy of the sink receipt entry.",
            {"matchingEntryCount": len(matching_entries)},
        )
    archived_entry, recomputed_payload_sha256 = matching_entries[0]
    archived_payload_sha256 = _require_sha256_digest(
        archived_entry.get("payloadSha256"),
        label="archive entry payloadSha256",
        code=code,
    )
    if (
        recomputed_payload_sha256 != receipt_payload_sha256
        or archived_payload_sha256 != receipt_payload_sha256
    ):
        raise ReleaseGateError(
            code,
            "The exact Object Lock batch payload hash drifted from the sink receipt payload hash.",
            {
                "receiptPayloadSha256": receipt_payload_sha256,
                "archivedPayloadSha256": archived_payload_sha256,
                "recomputedPayloadSha256": recomputed_payload_sha256,
            },
        )
    archived_audit = archived_entry.get("audit")
    if (
        not isinstance(archived_audit, dict)
        or archived_audit.get("requestId") != receipt_audit.get("requestId")
        or archived_audit.get("path") != receipt_audit.get("path")
        or archived_audit.get("operation") != receipt_audit.get("operation")
        or archived_audit.get("type") != receipt_audit.get("type")
        or archived_audit.get("eventTime") != receipt_audit.get("eventTime")
        or archived_audit.get("namespaceId") != receipt_audit.get("namespaceId")
        or archived_entry.get("archive") != {
            key: archive.get(key)
            for key in (
                "provider",
                "bucket",
                "objectKey",
                "mode",
                "firstLedgerIndex",
                "lastLedgerIndex",
            )
        }
    ):
        raise ReleaseGateError(
            code,
            "The exact Object Lock entry drifted from the sink receipt identity or archive pointer.",
        )
    expires_at = _require_string(
        retention.get("expiresAt"),
        label="sink receipt retention.expiresAt",
        code=code,
    )
    if _parse_utc_timestamp(evidence.retain_until, label="Object Lock retain-until") < _parse_utc_timestamp(
        expires_at,
        label="sink receipt expiry",
    ):
        raise ReleaseGateError(
            code,
            "The storage-enforced retain-until timestamp ended before the receipt retention boundary.",
        )
    try:
        delete_probe = clients.verifier.probe_delete_version(object_key, evidence.version_id)
        shorten_probe = clients.verifier.probe_shorten_retention(object_key, evidence.version_id)
        if (
            not delete_probe.blocked
            or delete_probe.denial_kind != "object_lock"
            or not shorten_probe.blocked
            or shorten_probe.denial_kind != "object_lock"
        ):
            raise ReleaseGateError(
                code,
                "The retained audit version did not hit explicit COMPLIANCE/WORM enforcement.",
                {
                    "deleteBlocked": delete_probe.blocked,
                    "deleteDenialKind": delete_probe.denial_kind,
                    "shortenRetentionBlocked": shorten_probe.blocked,
                    "shortenRetentionDenialKind": shorten_probe.denial_kind,
                },
            )
        clients.verifier.verify_existing_object(
            object_key,
            expected_content_sha256=batch_sha256,
            version_id=evidence.version_id,
        )
    except object_lock.S3ObjectLockError as error:
        raise ReleaseGateError(
            code,
            "The retained audit version did not survive its negative mutation probes.",
            {"errorCode": error.code},
        ) from None
    return {
        "provider": policy.object_lock_provider,
        "bucket": policy.object_lock_bucket,
        "objectKey": object_key,
        "versionId": evidence.version_id,
        "etag": evidence.etag,
        "contentSha256": evidence.content_sha256,
        "batchContentSha256": recomputed_batch_sha256,
        "payloadSha256": recomputed_payload_sha256,
        "retainUntil": evidence.retain_until,
        "retentionMode": evidence.retention_mode,
        "versioning": verifier_contract.versioning_status,
        "defaultRetentionMode": verifier_contract.default_retention_mode,
        "defaultRetentionDays": verifier_contract.default_retention_days,
        "writerProbeObjectKey": writer_probe.object_key,
        "writerProbeVersionId": writer_probe.version_id,
        "writerDeleteBlocked": writer_delete_probe.blocked,
        "writerDeleteDenialKind": writer_delete_probe.denial_kind,
        "entrySha256": entry_sha256,
        "entryCount": len(archived_entries),
        "batchEntryCount": batch_entry_count,
        "deleteBlocked": delete_probe.blocked,
        "deleteDenialKind": delete_probe.denial_kind,
        "shortenRetentionBlocked": shorten_probe.blocked,
        "shortenRetentionDenialKind": shorten_probe.denial_kind,
        "writerCredentialEnvironment": policy.object_lock_host_env,
        "verifierCredentialEnvironment": policy.object_lock_verifier_host_env,
        "credentialPolicyPath": str(policy.object_lock_credential_policy_path),
        "credentialPolicySha256": policy.object_lock_credential_policy_sha256,
    }


def _output_excerpt(
    completed: subprocess.CompletedProcess[str],
    redactor: acceptance.SecretRedactor,
) -> str:
    combined = (completed.stdout or "") + "\n" + (completed.stderr or "")
    return redactor.text(combined).strip()[:OUTPUT_EXCERPT_LIMIT]


def _subprocess_environment() -> dict[str, str]:
    env: dict[str, str] = {}
    for name in ("PATH", "HOME", "LANG", "LC_ALL", "SSL_CERT_FILE", "SSL_CERT_DIR"):
        value = os.environ.get(name)
        if value:
            env[name] = value
    return env


def run_vault_sys_audit_read(
    options: GateOptions,
    secret_inputs: SecretInputs,
    policy: OperationsPolicy,
    *,
    redactor: acceptance.SecretRedactor,
) -> dict[str, Any]:
    environment = _subprocess_environment()
    environment.update(
        {
            policy.vault_addr_env: secret_inputs.vault_address,
            policy.vault_cacert_env: secret_inputs.vault_cacert_runtime_value,
            "VAULT_TOKEN": secret_inputs.auditor_token,
            secret_inputs.auditor_token_environment: secret_inputs.auditor_token,
        }
    )
    command = [*options.vault_command, "read", "-format=json", DEFAULT_REQUEST_PATH]
    try:
        completed = subprocess.run(
            command,
            env=environment,
            capture_output=True,
            text=True,
            timeout=options.timeout_seconds,
            check=False,
        )
    except OSError:
        raise ReleaseGateError(
            "release.vault_audit_siem_vault_command_failed",
            "The configured Vault CLI or wrapper could not be executed.",
            {"executable": pathlib.Path(command[0]).name},
        ) from None
    if completed.returncode != 0:
        raise ReleaseGateError(
            "release.vault_audit_siem_vault_command_failed",
            "The benign Vault sys/audit read did not succeed.",
            {"outputExcerpt": _output_excerpt(completed, redactor), "returnCode": completed.returncode},
        )
    try:
        payload = json.loads(completed.stdout)
    except json.JSONDecodeError:
        raise ReleaseGateError(
            "release.vault_audit_siem_vault_command_failed",
            "The Vault sys/audit read did not return valid JSON.",
            {"outputExcerpt": _output_excerpt(completed, redactor)},
        ) from None
    root = _require_mapping(payload, label="Vault sys/audit output", code="release.vault_audit_siem_vault_command_failed")
    request_id = _require_string(
        root.get("request_id"),
        label="Vault sys/audit request_id",
        code="release.vault_audit_siem_vault_command_failed",
    )
    data = _require_mapping(root.get("data"), label="Vault sys/audit data", code="release.vault_audit_siem_vault_command_failed")
    return {
        "requestId": request_id,
        "requestPath": DEFAULT_REQUEST_PATH,
        "configuredTransitAuditPath": policy.configured_transit_audit_path,
        "auditorRoleName": policy.auditor_role_name,
        "auditorTokenEnvironment": secret_inputs.auditor_token_environment,
        "vaultAddressEnvironment": policy.vault_addr_env,
        "vaultCaCertificateEnvironment": policy.vault_cacert_env,
        "auditDeviceCount": len(data),
        "auditDevicePaths": sorted(str(key) for key in data.keys()),
        "command": {"argv": command},
    }


def build_sink_ssl_context(secret_inputs: SecretInputs) -> ssl.SSLContext:
    context = ssl.create_default_context(
        ssl.Purpose.SERVER_AUTH,
        cafile=str(secret_inputs.sink_ca_cert_path),
    )
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    context.load_cert_chain(
        certfile=str(secret_inputs.sink_client_cert_path),
        keyfile=str(secret_inputs.sink_client_key_path),
    )
    context.check_hostname = True
    return context


class _ResolvedHTTPSConnection(http.client.HTTPSConnection):
    def __init__(self, *args: Any, connect_host: str, **kwargs: Any) -> None:
        self._connect_host = connect_host
        super().__init__(*args, **kwargs)

    def connect(self) -> None:
        self.sock = self._create_connection(
            (self._connect_host, self.port),
            self.timeout,
            self.source_address,
        )
        if self._tunnel_host:
            self._tunnel()
        self.sock = self._context.wrap_socket(self.sock, server_hostname=self.host)


def _https_json_request(
    *,
    method: str,
    url: str,
    context: ssl.SSLContext,
    timeout_seconds: float,
    connect_host: str,
) -> tuple[int, Any, dict[str, Any]]:
    parsed = urllib.parse.urlsplit(url)
    if parsed.scheme != "https" or not parsed.hostname:
        raise ReleaseGateError(
            "release.vault_audit_siem_sink_invalid",
            "The configured sink endpoint was not an HTTPS URL.",
            {"endpoint": url},
        )
    path = parsed.path or "/"
    if parsed.query:
        path = f"{path}?{parsed.query}"
    connection = _ResolvedHTTPSConnection(
        host=parsed.hostname,
        port=parsed.port or 443,
        timeout=timeout_seconds,
        context=context,
        connect_host=connect_host,
    )
    try:
        connection.request(method, path, headers={"Accept": "application/json"})
        response = connection.getresponse()
        body = response.read()
        sock = connection.sock
        tls_evidence = {
            "serverCertificateSha256": sha256_bytes(sock.getpeercert(binary_form=True)) if sock else None,
            "serverChainSha256": [
                sha256_bytes(item) for item in (sock.get_verified_chain() if sock else []) or []
            ],
            "tlsVersion": sock.version() if sock else None,
            "cipherSuite": (sock.cipher()[0] if sock and sock.cipher() else None),
        }
    except OSError as error:
        raise ReleaseGateError(
            "release.vault_audit_siem_sink_unreachable",
            "The SIEM sink endpoint could not be reached over HTTPS mTLS.",
            {"endpoint": url, "reason": type(error).__name__},
        ) from None
    finally:
        connection.close()
    if body:
        try:
            payload = json.loads(body)
        except json.JSONDecodeError:
            payload = None
    else:
        payload = None
    return response.status, payload, tls_evidence


def poll_sink_receipt(
    *,
    sink_endpoint: str,
    request_id: str,
    request_path: str,
    secret_inputs: SecretInputs,
    policy: OperationsPolicy,
    options: GateOptions,
) -> dict[str, Any]:
    context = build_sink_ssl_context(secret_inputs)
    receipt_url = (
        f"{sink_endpoint.rstrip('/')}/v1/receipts?"
        f"request_id={urllib.parse.quote(request_id, safe='')}"
        f"&path={urllib.parse.quote(request_path, safe='')}"
    )
    deadline = time.monotonic() + options.timeout_seconds
    last_status = None
    while time.monotonic() < deadline:
        status, payload, tls_evidence = _https_json_request(
            method="GET",
            url=receipt_url,
            context=context,
            timeout_seconds=options.timeout_seconds,
            connect_host=secret_inputs.sink_connect_host,
        )
        last_status = status
        if status == 404:
            time.sleep(options.poll_interval_seconds)
            continue
        if status != 200 or not isinstance(payload, dict):
            raise ReleaseGateError(
                "release.vault_audit_siem_receipt_invalid",
                "The sink receipt API returned an unexpected response.",
                {"status": status},
            )
        receipt = _require_mapping(
            payload.get("receipt"),
            label="sink receipt",
            code="release.vault_audit_siem_receipt_invalid",
        )
        audit = _require_mapping(receipt.get("audit"), label="sink receipt audit", code="release.vault_audit_siem_receipt_invalid")
        transport = _require_mapping(
            receipt.get("transport"),
            label="sink receipt transport",
            code="release.vault_audit_siem_receipt_invalid",
        )
        retention = _require_mapping(
            receipt.get("retention"),
            label="sink receipt retention",
            code="release.vault_audit_siem_receipt_invalid",
        )
        actual_request_id = _require_string(
            audit.get("requestId"),
            label="sink receipt audit.requestId",
            code="release.vault_audit_siem_receipt_invalid",
        )
        actual_path = _require_string(
            audit.get("path"),
            label="sink receipt audit.path",
            code="release.vault_audit_siem_receipt_invalid",
        )
        if actual_request_id != request_id or actual_path != request_path:
            raise ReleaseGateError(
                "release.vault_audit_siem_receipt_invalid",
                "The sink receipt did not retain the exact request identity from Vault.",
                {
                    "expectedRequestId": request_id,
                    "actualRequestId": actual_request_id,
                    "expectedPath": request_path,
                    "actualPath": actual_path,
                },
            )
        if audit.get("operation") != "read":
            raise ReleaseGateError(
                "release.vault_audit_siem_receipt_invalid",
                "The sink receipt did not preserve the benign read operation.",
            )
        if transport.get("mutualTlsVerified") is not True:
            raise ReleaseGateError(
                "release.vault_audit_siem_receipt_invalid",
                "The sink receipt did not record a mutually authenticated TLS client.",
            )
        peer_sha256 = _require_string(
            transport.get("peerCertificateSha256"),
            label="sink receipt transport.peerCertificateSha256",
            code="release.vault_audit_siem_receipt_invalid",
        )
        if peer_sha256 != secret_inputs.sink_client_certificate_sha256:
            raise ReleaseGateError(
                "release.vault_audit_siem_receipt_invalid",
                "The sink receipt peer certificate did not match the declared SIEM client certificate.",
                {
                    "expectedPeerCertificateSha256": secret_inputs.sink_client_certificate_sha256,
                    "actualPeerCertificateSha256": peer_sha256,
                },
            )
        peer_chain = transport.get("peerChainSha256")
        if not isinstance(peer_chain, list) or peer_sha256 not in peer_chain:
            raise ReleaseGateError(
                "release.vault_audit_siem_receipt_invalid",
                "The sink receipt did not retain a verified peer certificate chain.",
            )
        expires_at = _require_string(
            retention.get("expiresAt"),
            label="sink receipt retention.expiresAt",
            code="release.vault_audit_siem_receipt_invalid",
        )
        return {
            "url": receipt_url,
            "tls": tls_evidence,
            "receipt": receipt,
            "expiresAt": expires_at,
            "retentionRequirement": policy.retention_requirement,
        }
    raise ReleaseGateError(
        "release.vault_audit_siem_receipt_missing",
        "The sink did not retain the exact Vault sys/audit request before the bounded poll timeout elapsed.",
        {"requestId": request_id, "path": request_path, "lastStatus": last_status},
    )


def verify_sink_chain(
    *,
    sink_endpoint: str,
    required_ledger_index: int,
    secret_inputs: SecretInputs,
    options: GateOptions,
) -> dict[str, Any]:
    normalized_ledger_index = _require_positive_integer(
        required_ledger_index,
        label="required ledger index",
        code="release.vault_audit_siem_chain_invalid",
    )
    status, payload, tls_evidence = _https_json_request(
        method="GET",
        url=f"{sink_endpoint.rstrip('/')}/healthz",
        context=build_sink_ssl_context(secret_inputs),
        timeout_seconds=options.timeout_seconds,
        connect_host=secret_inputs.sink_connect_host,
    )
    if status != 200 or not isinstance(payload, dict):
        raise ReleaseGateError(
            "release.vault_audit_siem_chain_invalid",
            "The sink health API did not return a valid success response.",
            {"status": status},
        )
    ledger = _require_mapping(
        payload.get("ledger"),
        label="sink health ledger",
        code="release.vault_audit_siem_chain_invalid",
    )
    if ledger.get("verified") is not True:
        raise ReleaseGateError(
            "release.vault_audit_siem_chain_invalid",
            "The sink health report showed an unverified hash chain.",
            {"status": payload.get("status")},
        )
    entry_count = _require_positive_integer(
        ledger.get("entryCount"),
        label="sink health ledger.entryCount",
        code="release.vault_audit_siem_chain_invalid",
    )
    if entry_count < normalized_ledger_index:
        raise ReleaseGateError(
            "release.vault_audit_siem_chain_invalid",
            "The sink health report did not retain the expected ledger index yet.",
            {"requiredLedgerIndex": normalized_ledger_index, "actualEntryCount": entry_count},
        )
    return {
        "tls": tls_evidence,
        "entryCount": entry_count,
        "latestEntrySha256": ledger.get("latestEntrySha256"),
        "verified": True,
    }


def verify_sink_retention(
    *,
    sink_endpoint: str,
    secret_inputs: SecretInputs,
    policy: OperationsPolicy,
    options: GateOptions,
) -> dict[str, Any]:
    status, payload, tls_evidence = _https_json_request(
        method="GET",
        url=f"{sink_endpoint.rstrip('/')}/v1/retention",
        context=build_sink_ssl_context(secret_inputs),
        timeout_seconds=options.timeout_seconds,
        connect_host=secret_inputs.sink_connect_host,
    )
    if status != 200 or not isinstance(payload, dict):
        raise ReleaseGateError(
            "release.vault_audit_siem_retention_invalid",
            "The sink retention API did not return a valid success response.",
            {"status": status},
        )
    retention = _require_mapping(
        payload.get("policy"),
        label="sink retention policy",
        code="release.vault_audit_siem_retention_invalid",
    )
    object_lock_report = _require_mapping(
        payload.get("objectLock"),
        label="sink Object Lock policy",
        code="release.vault_audit_siem_retention_invalid",
    )
    if retention.get("immutable") is not True or retention.get("storageEnforced") is not True:
        raise ReleaseGateError(
            "release.vault_audit_siem_retention_invalid",
            "The sink retention policy was not immutable.",
        )
    if (
        object_lock_report.get("enabled") is not True
        or object_lock_report.get("provider") != policy.object_lock_provider
        or object_lock_report.get("bucket") != policy.object_lock_bucket
        or object_lock_report.get("objectPrefix") != policy.object_lock_prefix
        or object_lock_report.get("versioning") != "Enabled"
        or object_lock_report.get("mode") != policy.object_lock_mode
        or object_lock_report.get("retentionDays") != policy.object_lock_retention_days
    ):
        raise ReleaseGateError(
            "release.vault_audit_siem_retention_invalid",
            "The sink Object Lock report drifted from the checked-in storage contract.",
        )
    requirement = _require_string(
        retention.get("requirement"),
        label="sink retention policy.requirement",
        code="release.vault_audit_siem_retention_invalid",
    )
    if requirement != policy.retention_requirement:
        raise ReleaseGateError(
            "release.vault_audit_siem_retention_invalid",
            "The sink retention requirement did not match the checked-in operations policy.",
            {"expected": policy.retention_requirement, "actual": requirement},
        )
    _require_string(
        retention.get("earliestExpiry"),
        label="sink retention policy.earliestExpiry",
        code="release.vault_audit_siem_retention_invalid",
    )
    return {
        "tls": tls_evidence,
        "policy": retention,
        "objectLock": object_lock_report,
    }


def verify_delete_rejected(
    *,
    receipt_url: str,
    secret_inputs: SecretInputs,
    options: GateOptions,
) -> dict[str, Any]:
    status, payload, tls_evidence = _https_json_request(
        method="DELETE",
        url=receipt_url,
        context=build_sink_ssl_context(secret_inputs),
        timeout_seconds=options.timeout_seconds,
        connect_host=secret_inputs.sink_connect_host,
    )
    if status != 405 or not isinstance(payload, dict):
        raise ReleaseGateError(
            "release.vault_audit_siem_delete_not_rejected",
            "The sink did not reject a mutation-style DELETE request.",
            {"status": status},
        )
    error = _require_mapping(
        payload.get("error"),
        label="sink DELETE error",
        code="release.vault_audit_siem_delete_not_rejected",
    )
    if error.get("code") != "sink.method_not_allowed":
        raise ReleaseGateError(
            "release.vault_audit_siem_delete_not_rejected",
            "The sink did not return the expected no-mutation error code.",
            {"actualCode": error.get("code")},
        )
    return {
        "status": status,
        "tls": tls_evidence,
        "errorCode": error.get("code"),
    }


def _kubectl_base_command(options: GateOptions) -> list[str]:
    command = [options.kubectl_bin]
    if options.kube_context:
        command.extend(["--context", options.kube_context])
    return command


def _run_kubectl_json(
    options: GateOptions,
    *,
    args: Sequence[str],
    redactor: acceptance.SecretRedactor,
) -> Mapping[str, Any]:
    command = [*_kubectl_base_command(options), *args]
    try:
        completed = subprocess.run(
            command,
            capture_output=True,
            text=True,
            timeout=options.timeout_seconds,
            check=False,
        )
    except OSError:
        raise FileNotFoundError(command[0]) from None
    if completed.returncode != 0:
        raise ReleaseGateError(
            "release.vault_audit_siem_runtime_invalid",
            "The live Vault StatefulSet evidence command did not succeed.",
            {"outputExcerpt": _output_excerpt(completed, redactor), "returnCode": completed.returncode},
        )
    try:
        payload = json.loads(completed.stdout)
    except json.JSONDecodeError:
        raise ReleaseGateError(
            "release.vault_audit_siem_runtime_invalid",
            "The live Vault StatefulSet evidence command did not return valid JSON.",
            {"outputExcerpt": _output_excerpt(completed, redactor)},
        ) from None
    return _require_mapping(
        payload,
        label="kubectl JSON payload",
        code="release.vault_audit_siem_runtime_invalid",
    )


def inspect_shipper_runtime(
    options: GateOptions,
    *,
    policy: OperationsPolicy,
    request_id: str,
    request_path: str,
    redactor: acceptance.SecretRedactor,
) -> dict[str, Any]:
    try:
        statefulset = _run_kubectl_json(
            options,
            args=[
                "-n",
                options.vault_namespace,
                "get",
                "statefulset",
                options.vault_statefulset,
                "-o",
                "json",
            ],
            redactor=redactor,
        )
    except FileNotFoundError:
        return {"status": "skipped", "reason": "kubectl unavailable"}
    spec = _require_mapping(
        statefulset.get("spec"),
        label="StatefulSet spec",
        code="release.vault_audit_siem_runtime_invalid",
    )
    template = _require_mapping(
        spec.get("template"),
        label="StatefulSet spec.template",
        code="release.vault_audit_siem_runtime_invalid",
    )
    pod_spec = _require_mapping(
        template.get("spec"),
        label="StatefulSet spec.template.spec",
        code="release.vault_audit_siem_runtime_invalid",
    )
    containers = pod_spec.get("containers")
    if not isinstance(containers, list):
        raise ReleaseGateError(
            "release.vault_audit_siem_runtime_invalid",
            "The live Vault StatefulSet template omitted its container list.",
        )
    shipper_container = next(
        (
            container
            for container in containers
            if isinstance(container, Mapping) and container.get("name") == options.shipper_container
        ),
        None,
    )
    if not isinstance(shipper_container, Mapping):
        raise ReleaseGateError(
            "release.vault_audit_siem_runtime_invalid",
            "The live Vault StatefulSet did not retain the expected audit shipper sidecar container.",
            {"container": options.shipper_container},
        )
    image = _require_string(
        shipper_container.get("image"),
        label="StatefulSet shipper image",
        code="release.vault_audit_siem_runtime_invalid",
    )
    if image != policy.sink_image:
        raise ReleaseGateError(
            "release.vault_audit_siem_runtime_invalid",
            "The live Vault StatefulSet shipper image did not match the checked-in operations policy.",
            {"expectedImage": policy.sink_image, "actualImage": image},
        )
    command = [
        *_kubectl_base_command(options),
        "-n",
        options.vault_namespace,
        "logs",
        f"statefulset/{options.vault_statefulset}",
        "-c",
        options.shipper_container,
        "--tail=200",
    ]
    try:
        completed = subprocess.run(
            command,
            capture_output=True,
            text=True,
            timeout=options.timeout_seconds,
            check=False,
        )
    except OSError:
        return {"status": "skipped", "reason": "kubectl logs unavailable"}
    if completed.returncode != 0:
        return {
            "status": "skipped",
            "reason": "kubectl logs failed",
            "returnCode": completed.returncode,
        }
    logs = completed.stdout or ""
    normalized_logs = logs.casefold()
    error_markers = (
        "error",
        "fatal",
        "healthcheck failed",
        "buffer is full",
        "backpressure",
    )
    observed_error_markers = [marker for marker in error_markers if marker in normalized_logs]
    if observed_error_markers:
        raise ReleaseGateError(
            "release.vault_audit_siem_runtime_invalid",
            "The live Vault audit shipper reported an error or backlog condition.",
            {"markers": observed_error_markers},
        )
    status = statefulset.get("status") if isinstance(statefulset.get("status"), Mapping) else {}
    return {
        "status": "observed",
        "namespace": options.vault_namespace,
        "statefulSet": options.vault_statefulset,
        "shipperContainer": options.shipper_container,
        "shipperImage": image,
        "readyReplicas": status.get("readyReplicas"),
        "replicas": status.get("replicas"),
        "logSha256": sha256_bytes(logs.encode("utf-8")),
        "logErrorMarkers": [],
        "requestIdentityLogged": False,
        "receiptIsDeliveryEvidence": True,
    }


def markdown_from_report(report: Mapping[str, Any]) -> str:
    lines = [
        "# Stage 3 Vault Audit SIEM Delivery Gate",
        "",
        f"- Schema: `{report.get('schemaVersion', '')}`",
        f"- Run: `{report.get('runId', '')}`",
        f"- Status: **{report.get('status', '')}**",
        f"- Started: `{report.get('startedAt', '')}`",
        f"- Finished: `{report.get('finishedAt', '')}`",
        f"- Duration: `{report.get('durationMs', '')} ms`",
        "",
        "## Evidence boundary",
        "",
        "This gate performs one benign authenticated Vault `sys/audit` read, waits for the exact request ID and path",
        "to reach the external HTTPS+mTLS sink, verifies the sink receipt and hash chain, then independently reads the",
        "exact S3 Object Lock version and proves COMPLIANCE delete/retention-shortening rejection before optionally",
        "checking the live Vault shipper StatefulSet and log evidence when `kubectl`",
        "is practical. It does not print or persist the auditor token or PEM secrets.",
    ]
    source = report.get("source")
    if isinstance(source, dict):
        lines.extend(
            [
                "",
                "## Source",
                "",
                f"- Git SHA: `{source.get('gitSha', '')}`",
                f"- Worktree dirty: `{source.get('worktreeDirty', '')}`",
            ]
        )
    policy = report.get("policy")
    if isinstance(policy, dict):
        lines.extend(
            [
                "",
                "## Policy",
                "",
                f"- Policy SHA256: `{policy.get('sha256', '')}`",
                f"- Auditor role: `{policy.get('auditorRoleName', '')}`",
                f"- Sink transport: `{policy.get('sinkTransport', '')}`",
                f"- Delivery SLO: `{policy.get('sinkDeliverySlo', '')}`",
                f"- Retention requirement: `{policy.get('retentionRequirement', '')}`",
            ]
        )
    vault = report.get("vault")
    if isinstance(vault, dict):
        lines.extend(
            [
                "",
                "## Vault",
                "",
                f"- Request path: `{vault.get('requestPath', '')}`",
                f"- Request ID: `{vault.get('requestId', '')}`",
                f"- Audit devices: `{vault.get('auditDeviceCount', '')}`",
                f"- Auditor token env: `{vault.get('auditorTokenEnvironment', '')}`",
                f"- Vault address env: `{vault.get('vaultAddressEnvironment', '')}`",
                f"- Vault CA env: `{vault.get('vaultCaCertificateEnvironment', '')}`",
            ]
        )
    sink = report.get("sink")
    if isinstance(sink, dict):
        receipt = sink.get("receipt") if isinstance(sink.get("receipt"), dict) else {}
        transport = receipt.get("transport") if isinstance(receipt.get("transport"), dict) else {}
        object_lock_evidence = (
            sink.get("objectLock") if isinstance(sink.get("objectLock"), dict) else {}
        )
        lines.extend(
            [
                "",
                "## Sink",
                "",
                f"- Endpoint env: `{sink.get('endpointEnvironment', '')}`",
                f"- Endpoint authority: `{sink.get('endpointAuthority', '')}`",
                f"- Peer certificate SHA256: `{transport.get('peerCertificateSha256', '')}`",
                f"- Entry SHA256: `{receipt.get('entrySha256', '')}`",
                f"- Retention expiry: `{sink.get('retentionExpiry', '')}`",
                f"- DELETE rejected: `{sink.get('deleteRejected', '')}`",
                f"- WORM bucket: `{object_lock_evidence.get('bucket', '')}`",
                f"- WORM object key: `{object_lock_evidence.get('objectKey', '')}`",
                f"- WORM version ID: `{object_lock_evidence.get('versionId', '')}`",
                f"- WORM mode: `{object_lock_evidence.get('retentionMode', '')}`",
                f"- WORM delete blocked: `{object_lock_evidence.get('deleteBlocked', '')}`",
                f"- WORM shortening blocked: `{object_lock_evidence.get('shortenRetentionBlocked', '')}`",
            ]
        )
    runtime = report.get("runtime")
    if isinstance(runtime, dict):
        lines.extend(
            [
                "",
                "## Runtime",
                "",
                f"- Status: `{runtime.get('status', '')}`",
                f"- StatefulSet: `{runtime.get('statefulSet', '')}`",
                f"- Shipper image: `{runtime.get('shipperImage', '')}`",
            ]
        )
    cleanup = report.get("cleanup")
    if isinstance(cleanup, dict):
        lines.extend(
            [
                "",
                "## Cleanup",
                "",
                f"- Temporary files removed: `{cleanup.get('temporaryFilesRemoved', False)}`",
                f"- Removed file count: `{cleanup.get('removedFileCount', 0)}`",
                f"- State dir empty: `{cleanup.get('stateDirEmpty', False)}`",
            ]
        )
    errors = report.get("errors")
    if isinstance(errors, list) and errors:
        lines.extend(
            [
                "",
                "## Errors",
                "",
                "```json",
                json.dumps(errors, indent=2, sort_keys=True, ensure_ascii=False),
                "```",
            ]
        )
    return "\n".join(lines).rstrip() + "\n"


def write_report(
    report: Mapping[str, Any],
    output_dir: pathlib.Path,
    redactor: acceptance.SecretRedactor,
) -> tuple[pathlib.Path, pathlib.Path]:
    output_dir.mkdir(parents=True, exist_ok=True)
    sanitized = redactor.value(dict(report))
    json_path = output_dir / JSON_REPORT_NAME
    markdown_path = output_dir / MARKDOWN_REPORT_NAME
    json_path.write_text(
        json.dumps(sanitized, indent=2, sort_keys=True, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )
    markdown_path.write_text(markdown_from_report(sanitized), encoding="utf-8")
    return json_path, markdown_path


def _cleanup_temp_files(paths: Sequence[pathlib.Path]) -> dict[str, Any]:
    removed = 0
    removed_names: list[str] = []
    for path in paths:
        try:
            path.unlink()
        except FileNotFoundError:
            continue
        except OSError:
            continue
        removed += 1
        removed_names.append(path.name)
    state_dir = paths[0].parent if paths else None
    state_dir_empty = False
    if state_dir and state_dir.exists() and state_dir.is_dir():
        state_dir_empty = not any(state_dir.iterdir())
    return {
        "temporaryFilesRemoved": removed == len(paths),
        "removedFileCount": removed,
        "removedFileNames": removed_names,
        "stateDirEmpty": state_dir_empty,
    }


def run_vault_audit_siem_delivery_gate(
    options: GateOptions,
    *,
    repository_state: Any = common.repository_state,
) -> int:
    if options.output_dir.exists() and (
        not options.output_dir.is_dir() or any(options.output_dir.iterdir())
    ):
        print("Vault audit SIEM delivery gate output directory must be empty or absent.", file=sys.stderr)
        return 2
    options.output_dir.mkdir(parents=True, exist_ok=True)
    state_dir = ensure_private_directory(options.output_dir / "_state")
    started_at = utc_now()
    started = time.monotonic()
    run_id = f"stage3-vault-audit-siem-delivery-{uuid.uuid4()}"
    redactor = acceptance.SecretRedactor()
    source: dict[str, Any] = {}
    policy_report: dict[str, Any] = {}
    vault_report: dict[str, Any] = {}
    sink_report: dict[str, Any] = {}
    runtime_report: dict[str, Any] = {"status": "skipped", "reason": "not attempted"}
    cleanup_report: dict[str, Any] = {
        "temporaryFilesRemoved": False,
        "removedFileCount": 0,
        "removedFileNames": [],
        "stateDirEmpty": False,
    }
    errors: list[dict[str, Any]] = []
    temporary_paths: tuple[pathlib.Path, ...] = ()
    try:
        source = dict(repository_state(options.repo_root))
        policy = load_operations_policy(options.operations_policy_path)
        policy_report = {
            "path": str(policy.path),
            "sha256": policy.sha256,
            "auditorRoleName": policy.auditor_role_name,
            "configuredTransitAuditPath": policy.configured_transit_audit_path,
            "sinkTransport": policy.sink_transport,
            "sinkImage": policy.sink_image,
            "sinkConfigMapName": policy.sink_config_map_name,
            "sinkSecretName": policy.sink_secret_name,
            "sinkDeliverySlo": policy.sink_delivery_slo,
            "retentionRequirement": policy.retention_requirement,
            "objectLock": {
                "provider": policy.object_lock_provider,
                "mode": policy.object_lock_mode,
                "retentionDays": policy.object_lock_retention_days,
                "bucket": policy.object_lock_bucket,
                "objectPrefix": policy.object_lock_prefix,
                "credentialPolicyPath": str(policy.object_lock_credential_policy_path),
                "credentialPolicySha256": policy.object_lock_credential_policy_sha256,
            },
            "environmentNames": {
                "vaultAddress": policy.vault_addr_env,
                "vaultCaCertificate": policy.vault_cacert_env,
                "sinkEndpoint": policy.sink_endpoint_env,
                "sinkResolve": policy.sink_resolve_env,
                "sinkClientCertificate": policy.sink_client_cert_env,
                "sinkClientKey": policy.sink_client_key_env,
                "sinkCaCertificate": policy.sink_ca_cert_env,
                "objectLockAlias": policy.object_lock_alias_env,
                "objectLockConfigDirectory": policy.object_lock_config_dir_env,
                "objectLockHost": policy.object_lock_host_env,
                "objectLockVerifierHost": policy.object_lock_verifier_host_env,
                "objectLockResolve": policy.object_lock_resolve_env,
            },
        }
        secret_inputs = prepare_secret_inputs(policy, options, state_dir=state_dir, redactor=redactor)
        temporary_paths = secret_inputs.temporary_paths
        vault_report = run_vault_sys_audit_read(options, secret_inputs, policy, redactor=redactor)
        receipt_report = poll_sink_receipt(
            sink_endpoint=secret_inputs.sink_endpoint,
            request_id=str(vault_report["requestId"]),
            request_path=str(vault_report["requestPath"]),
            secret_inputs=secret_inputs,
            policy=policy,
            options=options,
        )
        receipt = _require_mapping(
            receipt_report.get("receipt"),
            label="sink receipt",
            code="release.vault_audit_siem_receipt_invalid",
        )
        chain_report = verify_sink_chain(
            sink_endpoint=secret_inputs.sink_endpoint,
            required_ledger_index=_require_positive_integer(
                receipt.get("ledgerIndex"),
                label="sink receipt ledgerIndex",
                code="release.vault_audit_siem_receipt_invalid",
            ),
            secret_inputs=secret_inputs,
            options=options,
        )
        retention_report = verify_sink_retention(
            sink_endpoint=secret_inputs.sink_endpoint,
            secret_inputs=secret_inputs,
            policy=policy,
            options=options,
        )
        object_lock_report = verify_object_lock_archive(
            receipt=receipt,
            policy=policy,
            secret_inputs=secret_inputs,
            options=options,
        )
        delete_report = verify_delete_rejected(
            receipt_url=str(receipt_report["url"]),
            secret_inputs=secret_inputs,
            options=options,
        )
        runtime_report = inspect_shipper_runtime(
            options,
            policy=policy,
            request_id=str(vault_report["requestId"]),
            request_path=str(vault_report["requestPath"]),
            redactor=redactor,
        )
        endpoint = urllib.parse.urlsplit(secret_inputs.sink_endpoint)
        sink_report = {
            "endpointEnvironment": secret_inputs.sink_endpoint_environment,
            "endpointAuthority": endpoint.netloc,
            "clientCertificateSha256": secret_inputs.sink_client_certificate_sha256,
            "receipt": receipt,
            "receiptTls": receipt_report["tls"],
            "retentionExpiry": receipt_report["expiresAt"],
            "chain": chain_report,
            "retention": retention_report["policy"],
            "objectLock": object_lock_report,
            "deleteRejected": delete_report["status"] == 405,
        }
    except ReleaseGateError as error:
        errors.append(error.as_report_error())
    finally:
        cleanup_report.update(_cleanup_temp_files(temporary_paths))
        finished_at = utc_now()
        duration_ms = int((time.monotonic() - started) * 1000)
        report = {
            "schemaVersion": SCHEMA_VERSION,
            "runId": run_id,
            "mode": "vault-audit-siem-delivery-gate",
            "status": "pass" if not errors else "fail",
            "startedAt": started_at,
            "finishedAt": finished_at,
            "durationMs": duration_ms,
            "source": source,
            "policy": policy_report,
            "vault": vault_report,
            "sink": sink_report,
            "runtime": runtime_report,
            "cleanup": cleanup_report,
            "errors": errors,
        }
        json_path, markdown_path = write_report(report, options.output_dir, redactor)
        output_scan = acceptance.scan_output_secrets(options.output_dir, redactor)
        if output_scan.get("status") != "pass":
            errors.append(
                ReleaseGateError(
                    "release.vault_audit_siem_output_secret_scan_failed",
                    "The Vault audit SIEM delivery gate output retained secret-like findings.",
                    {"findings": output_scan.get("findings")},
                ).as_report_error()
            )
            report["status"] = "fail"
            report["errors"] = errors
        report["security"] = {"outputSecretScan": output_scan}
        write_report(report, options.output_dir, redactor)
        print(f"JSON: {json_path}")
        print(f"Markdown: {markdown_path}")
    return 0 if not errors else 1


def main(argv: Sequence[str] | None = None) -> int:
    return run_vault_audit_siem_delivery_gate(parse_args(argv))


if __name__ == "__main__":
    raise SystemExit(main())
