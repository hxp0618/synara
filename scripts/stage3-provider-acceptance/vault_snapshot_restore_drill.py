#!/usr/bin/env python3
"""Run an isolated Vault Raft snapshot restore drill and emit secret-safe evidence."""

from __future__ import annotations

import argparse
import dataclasses
import datetime as dt
import hashlib
import json
import os
import pathlib
import re
import shutil
import subprocess
import sys
import tempfile
import time
import uuid
from collections.abc import Mapping, Sequence
from typing import Any

import acceptance_runner as acceptance
import release_gate_common as common


SCHEMA_VERSION = "synara.vault-snapshot-restore-drill.v1"
JSON_REPORT_NAME = "vault-snapshot-restore-drill.json"
MARKDOWN_REPORT_NAME = "vault-snapshot-restore-drill.md"
DEFAULT_OPERATIONS_POLICY_PATH = pathlib.Path("deploy/kubernetes/security/vault/operations-policy.json")
DEFAULT_SNAPSHOT_OPERATOR_POLICY_PATH = pathlib.Path(
    "deploy/kubernetes/security/vault/synara-vault-snapshot-operator.hcl"
)
DEFAULT_TIMEOUT_SECONDS = 300.0
DEFAULT_VAULT_CLIENT_TIMEOUT = "10m"
DEFAULT_RESTORE_VAULT_ADDR = "http://127.0.0.1:8200"
DEFAULT_RESTORE_VAULT_CLUSTER_ADDR = "http://127.0.0.1:8201"
DEFAULT_RESTORE_STATE_SUBDIRECTORY = "restore-state"
DEFAULT_RESTORE_CONFIG_SUBDIRECTORY = "restore-config"
DEFAULT_SOURCE_SNAPSHOT_NAME = "source.snap"
DEFAULT_RESTORE_NODE_ID = "restore-0"
DEFAULT_RESTORE_RAFT_ADDRESS = "127.0.0.1:8201"
DEFAULT_RESTORE_AUDIT_TMPFS = "/vault/audit:rw,noexec,nosuid,nodev,uid=100,gid=1000,mode=0700"
EXPECTED_TRANSIT_KEY_TYPE = "ecdsa-p256"
EXPECTED_AUDIT_DEVICES = (
    {"path": "file", "filePath": "/vault/audit/audit-primary.log"},
    {"path": "file-secondary", "filePath": "/vault/audit/audit-secondary.log"},
)
ENVIRONMENT_NAME_PATTERN = re.compile(r"[A-Z][A-Z0-9_]{0,127}")
SHA256_HEX_PATTERN = re.compile(r"[0-9a-f]{64}")

ReleaseGateError = common.ReleaseGateError


@dataclasses.dataclass(frozen=True)
class SnapshotOperatorRolePolicy:
    name: str
    token_policies: tuple[str, ...]
    token_type: str
    token_ttl_seconds: int
    token_max_ttl_seconds: int
    token_num_uses: int
    secret_id_ttl_seconds: int
    secret_id_num_uses: int
    token_no_default_policy: bool


@dataclasses.dataclass(frozen=True)
class OperationsPolicy:
    path: pathlib.Path
    sha256: str
    kms_reference: str
    signing_credential_environment: tuple[str, ...]
    signer_identity: str
    transit_audit_request_path: str
    transparency_log_provider: str
    transparency_log_url: str
    admission_controller: str
    admission_validation_failure_action: str
    admission_failure_policy: str
    admission_cluster_policy_path: str
    transit_key_name: str
    signer_role_name: str
    auditor_role_name: str
    snapshot_operator_role_name: str
    snapshot_operator_policy_name: str
    snapshot_operator_role_policy: SnapshotOperatorRolePolicy
    custody_total_shares: int
    custody_threshold: int
    custody_minimum_custodians: int
    source_vault_addr_env: str
    source_vault_cacert_env: str
    source_role_id_env: str
    source_secret_id_env: str
    source_unseal_key_envs: tuple[str, ...]
    restore_docker_image: str
    restore_docker_network_mode: str
    restore_audit_tmpfs: str
    restore_require_fresh_state_dir: bool
    restore_forbid_retry_join: bool
    restore_forbid_source_cluster_restore: bool
    required_checks: tuple[str, ...]
    required_artifacts: tuple[str, ...]
    audit_devices: tuple[dict[str, str], ...]
    audit_siem_required: bool
    audit_siem_environments: tuple[str, ...]


@dataclasses.dataclass(frozen=True)
class GateOptions:
    repo_root: pathlib.Path
    output_dir: pathlib.Path
    operations_policy_path: pathlib.Path
    snapshot_operator_policy_path: pathlib.Path
    vault_bin: str
    docker_bin: str
    timeout_seconds: float
    vault_client_timeout: str


@dataclasses.dataclass(frozen=True)
class SecretInputs:
    source_vault_environment: dict[str, str]
    source_role_id: str
    source_secret_id: str
    source_unseal_keys: tuple[str, ...]
    source_role_id_env: str
    source_secret_id_env: str
    source_unseal_key_envs: tuple[str, ...]
    source_vault_addr_env: str
    source_vault_cacert_env: str


@dataclasses.dataclass(frozen=True)
class DockerRestoreContext:
    container_name: str
    state_dir: pathlib.Path
    config_path: pathlib.Path
    snapshot_path: pathlib.Path
    restore_vault_addr: str
    docker_image: str


def sha256_bytes(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def sha256_text(value: str) -> str:
    return sha256_bytes(value.encode("utf-8"))


def stable_json_sha256(value: Any) -> str:
    return sha256_text(json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=False))


def utc_now() -> dt.datetime:
    return dt.datetime.now(dt.timezone.utc)


def require_mapping(value: Any, *, label: str) -> Mapping[str, Any]:
    if not isinstance(value, Mapping):
        raise ReleaseGateError(
            "release.vault_snapshot_restore_policy_invalid",
            f"The {label} entry must be an object.",
        )
    return value


def require_string(value: Any, *, label: str) -> str:
    if not isinstance(value, str) or not value.strip():
        raise ReleaseGateError(
            "release.vault_snapshot_restore_policy_invalid",
            f"The {label} entry must be a non-empty string.",
        )
    return value.strip()


def require_bool(value: Any, *, label: str) -> bool:
    if not isinstance(value, bool):
        raise ReleaseGateError(
            "release.vault_snapshot_restore_policy_invalid",
            f"The {label} entry must be a boolean.",
        )
    return value


def require_int(value: Any, *, label: str, minimum: int = 0) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value < minimum:
        raise ReleaseGateError(
            "release.vault_snapshot_restore_policy_invalid",
            f"The {label} entry must be an integer greater than or equal to {minimum}.",
        )
    return value


def require_environment_name(value: Any, *, label: str) -> str:
    name = require_string(value, label=label)
    if ENVIRONMENT_NAME_PATTERN.fullmatch(name) is None:
        raise ReleaseGateError(
            "release.vault_snapshot_restore_policy_invalid",
            f"The {label} entry must be a valid environment variable name.",
            {"value": name},
        )
    return name


def require_string_list(value: Any, *, label: str, minimum: int = 1) -> tuple[str, ...]:
    if not isinstance(value, list):
        raise ReleaseGateError(
            "release.vault_snapshot_restore_policy_invalid",
            f"The {label} entry must be an array.",
        )
    items = tuple(require_string(item, label=f"{label}[{index}]") for index, item in enumerate(value))
    if len(items) < minimum:
        raise ReleaseGateError(
            "release.vault_snapshot_restore_policy_invalid",
            f"The {label} entry must contain at least {minimum} item(s).",
        )
    return items


def load_operations_policy(policy_path: pathlib.Path) -> OperationsPolicy:
    try:
        raw = policy_path.read_bytes()
    except OSError:
        raise ReleaseGateError(
            "release.vault_snapshot_restore_policy_invalid",
            "The operations policy file was unavailable.",
            {"path": str(policy_path)},
        ) from None
    try:
        decoded = json.loads(raw)
    except json.JSONDecodeError:
        raise ReleaseGateError(
            "release.vault_snapshot_restore_policy_invalid",
            "The operations policy file was not valid JSON.",
            {"path": str(policy_path)},
        ) from None
    root = require_mapping(decoded, label="operations policy")
    if require_string(root.get("schemaVersion"), label="schemaVersion") != "synara.vault-kms-operations-policy.v1":
        raise ReleaseGateError(
            "release.vault_snapshot_restore_policy_invalid",
            "The operations policy schemaVersion was not the expected Synara Vault operations schema.",
        )
    signing = require_mapping(root.get("signing"), label="signing")
    transparency = require_mapping(root.get("transparencyLog"), label="transparencyLog")
    admission = require_mapping(root.get("admission"), label="admission")
    vault = require_mapping(root.get("vault"), label="vault")
    snapshot_role = require_mapping(vault.get("snapshotOperatorRole"), label="vault.snapshotOperatorRole")
    custody = require_mapping(root.get("custody"), label="custody")
    drill = require_mapping(root.get("snapshotRestoreDrill"), label="snapshotRestoreDrill")
    credentials = require_mapping(drill.get("credentials"), label="snapshotRestoreDrill.credentials")
    restore_target = require_mapping(drill.get("restoreTarget"), label="snapshotRestoreDrill.restoreTarget")
    audit = require_mapping(root.get("audit"), label="audit")
    siem = require_mapping(audit.get("externalSiem"), label="audit.externalSiem")

    required_checks = require_string_list(
        drill.get("requiredChecks"), label="snapshotRestoreDrill.requiredChecks", minimum=6
    )
    required_artifacts = require_string_list(
        drill.get("requiredArtifacts"), label="snapshotRestoreDrill.requiredArtifacts", minimum=2
    )
    local_devices_value = audit.get("localDevices")
    if not isinstance(local_devices_value, list) or len(local_devices_value) < 2:
        raise ReleaseGateError(
            "release.vault_snapshot_restore_policy_invalid",
            "The audit.localDevices entry must list the two required PVC-backed audit files.",
        )
    audit_devices: list[dict[str, str]] = []
    for index, item in enumerate(local_devices_value):
        device = require_mapping(item, label=f"audit.localDevices[{index}]")
        audit_devices.append(
            {
                "path": require_string(device.get("path"), label=f"audit.localDevices[{index}].path"),
                "filePath": require_string(
                    device.get("filePath"),
                    label=f"audit.localDevices[{index}].filePath",
                ),
            }
        )
    if tuple(audit_devices) != EXPECTED_AUDIT_DEVICES:
        raise ReleaseGateError(
            "release.vault_snapshot_restore_policy_invalid",
            "The operations policy must retain exactly the two checked-in Vault audit device file sinks.",
            {
                "expectedAuditDevices": list(EXPECTED_AUDIT_DEVICES),
                "actualAuditDevices": audit_devices,
            },
        )

    custody_total_shares = require_int(custody.get("totalShares"), label="custody.totalShares", minimum=1)
    custody_threshold = require_int(custody.get("threshold"), label="custody.threshold", minimum=1)
    custody_minimum_custodians = require_int(
        custody.get("minimumParticipatingCustodians"),
        label="custody.minimumParticipatingCustodians",
        minimum=1,
    )
    if custody_threshold > custody_total_shares or custody_minimum_custodians < custody_threshold:
        raise ReleaseGateError(
            "release.vault_snapshot_restore_policy_invalid",
            "The Shamir custody threshold was inconsistent with the total share count or drill participant minimum.",
        )
    source_unseal_key_envs = require_string_list(
        credentials.get("restoreUnsealKeyEnvironments"),
        label="snapshotRestoreDrill.credentials.restoreUnsealKeyEnvironments",
        minimum=custody_threshold,
    )
    if len(source_unseal_key_envs) != custody_threshold:
        raise ReleaseGateError(
            "release.vault_snapshot_restore_policy_invalid",
            "The restore drill must declare exactly the threshold number of unseal-key environment names.",
            {
                "threshold": custody_threshold,
                "environmentCount": len(source_unseal_key_envs),
            },
        )
    snapshot_operator_role_policy = SnapshotOperatorRolePolicy(
        name=require_string(vault.get("snapshotOperatorAppRoleName"), label="vault.snapshotOperatorAppRoleName"),
        token_policies=require_string_list(
            snapshot_role.get("tokenPolicies"),
            label="vault.snapshotOperatorRole.tokenPolicies",
            minimum=1,
        ),
        token_type=require_string(snapshot_role.get("tokenType"), label="vault.snapshotOperatorRole.tokenType"),
        token_ttl_seconds=require_int(
            snapshot_role.get("tokenTtlSeconds"),
            label="vault.snapshotOperatorRole.tokenTtlSeconds",
            minimum=1,
        ),
        token_max_ttl_seconds=require_int(
            snapshot_role.get("tokenMaxTtlSeconds"),
            label="vault.snapshotOperatorRole.tokenMaxTtlSeconds",
            minimum=1,
        ),
        token_num_uses=require_int(
            snapshot_role.get("tokenNumUses"),
            label="vault.snapshotOperatorRole.tokenNumUses",
            minimum=0,
        ),
        secret_id_ttl_seconds=require_int(
            snapshot_role.get("secretIdTtlSeconds"),
            label="vault.snapshotOperatorRole.secretIdTtlSeconds",
            minimum=1,
        ),
        secret_id_num_uses=require_int(
            snapshot_role.get("secretIdNumUses"),
            label="vault.snapshotOperatorRole.secretIdNumUses",
            minimum=1,
        ),
        token_no_default_policy=require_bool(
            snapshot_role.get("tokenNoDefaultPolicy"),
            label="vault.snapshotOperatorRole.tokenNoDefaultPolicy",
        ),
    )
    if sorted(required_artifacts) != sorted((JSON_REPORT_NAME, MARKDOWN_REPORT_NAME)):
        raise ReleaseGateError(
            "release.vault_snapshot_restore_policy_invalid",
            "The restore drill artifacts must match the checked-in report filenames.",
        )
    return OperationsPolicy(
        path=policy_path,
        sha256=sha256_bytes(raw),
        kms_reference=require_string(signing.get("kmsReference"), label="signing.kmsReference"),
        signing_credential_environment=require_string_list(
            signing.get("credentialEnvironment"),
            label="signing.credentialEnvironment",
            minimum=3,
        ),
        signer_identity=require_string(signing.get("signerIdentity"), label="signing.signerIdentity"),
        transit_audit_request_path=require_string(
            signing.get("auditRequestPath"),
            label="signing.auditRequestPath",
        ),
        transparency_log_provider=require_string(
            transparency.get("provider"),
            label="transparencyLog.provider",
        ),
        transparency_log_url=require_string(transparency.get("url"), label="transparencyLog.url"),
        admission_controller=require_string(admission.get("controller"), label="admission.controller"),
        admission_validation_failure_action=require_string(
            admission.get("validationFailureAction"),
            label="admission.validationFailureAction",
        ),
        admission_failure_policy=require_string(
            admission.get("failurePolicy"),
            label="admission.failurePolicy",
        ),
        admission_cluster_policy_path=require_string(
            admission.get("clusterPolicyPath"),
            label="admission.clusterPolicyPath",
        ),
        transit_key_name=require_string(vault.get("transitKeyName"), label="vault.transitKeyName"),
        signer_role_name=require_string(vault.get("signerAppRoleName"), label="vault.signerAppRoleName"),
        auditor_role_name=require_string(vault.get("auditorAppRoleName"), label="vault.auditorAppRoleName"),
        snapshot_operator_role_name=require_string(
            vault.get("snapshotOperatorAppRoleName"),
            label="vault.snapshotOperatorAppRoleName",
        ),
        snapshot_operator_policy_name=require_string(
            vault.get("snapshotOperatorPolicyName"),
            label="vault.snapshotOperatorPolicyName",
        ),
        snapshot_operator_role_policy=snapshot_operator_role_policy,
        custody_total_shares=custody_total_shares,
        custody_threshold=custody_threshold,
        custody_minimum_custodians=custody_minimum_custodians,
        source_vault_addr_env=require_environment_name(
            credentials.get("vaultAddressEnvironment"),
            label="snapshotRestoreDrill.credentials.vaultAddressEnvironment",
        ),
        source_vault_cacert_env=require_environment_name(
            credentials.get("vaultCaCertificateEnvironment"),
            label="snapshotRestoreDrill.credentials.vaultCaCertificateEnvironment",
        ),
        source_role_id_env=require_environment_name(
            credentials.get("snapshotOperatorRoleIdEnvironment"),
            label="snapshotRestoreDrill.credentials.snapshotOperatorRoleIdEnvironment",
        ),
        source_secret_id_env=require_environment_name(
            credentials.get("snapshotOperatorSecretIdEnvironment"),
            label="snapshotRestoreDrill.credentials.snapshotOperatorSecretIdEnvironment",
        ),
        source_unseal_key_envs=tuple(
            require_environment_name(name, label=f"snapshotRestoreDrill.credentials.restoreUnsealKeyEnvironments[{index}]")
            for index, name in enumerate(source_unseal_key_envs)
        ),
        restore_docker_image=require_string(
            restore_target.get("dockerImage"),
            label="snapshotRestoreDrill.restoreTarget.dockerImage",
        ),
        restore_docker_network_mode=require_string(
            restore_target.get("dockerNetworkMode"),
            label="snapshotRestoreDrill.restoreTarget.dockerNetworkMode",
        ),
        restore_audit_tmpfs=require_string(
            restore_target.get("auditTmpfs"),
            label="snapshotRestoreDrill.restoreTarget.auditTmpfs",
        ),
        restore_require_fresh_state_dir=require_bool(
            restore_target.get("requireFreshStateDir"),
            label="snapshotRestoreDrill.restoreTarget.requireFreshStateDir",
        ),
        restore_forbid_retry_join=require_bool(
            restore_target.get("forbidRetryJoin"),
            label="snapshotRestoreDrill.restoreTarget.forbidRetryJoin",
        ),
        restore_forbid_source_cluster_restore=require_bool(
            restore_target.get("forbidSourceClusterRestore"),
            label="snapshotRestoreDrill.restoreTarget.forbidSourceClusterRestore",
        ),
        required_checks=required_checks,
        required_artifacts=required_artifacts,
        audit_devices=tuple(audit_devices),
        audit_siem_required=require_bool(siem.get("required"), label="audit.externalSiem.required"),
        audit_siem_environments=(
            require_environment_name(
                siem.get("endpointEnvironment"),
                label="audit.externalSiem.endpointEnvironment",
            ),
            require_environment_name(
                siem.get("clientCertificateEnvironment"),
                label="audit.externalSiem.clientCertificateEnvironment",
            ),
            require_environment_name(
                siem.get("clientKeyEnvironment"),
                label="audit.externalSiem.clientKeyEnvironment",
            ),
            require_environment_name(
                siem.get("caCertificateEnvironment"),
                label="audit.externalSiem.caCertificateEnvironment",
            ),
        ),
    )


def parse_args(argv: Sequence[str] | None = None) -> GateOptions:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", required=True, type=pathlib.Path)
    parser.add_argument("--operations-policy", type=pathlib.Path, default=DEFAULT_OPERATIONS_POLICY_PATH)
    parser.add_argument(
        "--snapshot-operator-policy",
        type=pathlib.Path,
        default=DEFAULT_SNAPSHOT_OPERATOR_POLICY_PATH,
    )
    parser.add_argument("--vault-bin", default="vault")
    parser.add_argument("--docker-bin", default="docker")
    parser.add_argument("--timeout-seconds", type=float, default=DEFAULT_TIMEOUT_SECONDS)
    parser.add_argument("--vault-client-timeout", default=DEFAULT_VAULT_CLIENT_TIMEOUT)
    parsed = parser.parse_args(argv)
    repo_root = pathlib.Path.cwd().resolve()
    output_dir = parsed.output_dir.expanduser()
    if not output_dir.is_absolute():
        output_dir = (repo_root / output_dir).resolve()
    operations_policy = parsed.operations_policy.expanduser()
    if not operations_policy.is_absolute():
        operations_policy = (repo_root / operations_policy).resolve()
    snapshot_operator_policy = parsed.snapshot_operator_policy.expanduser()
    if not snapshot_operator_policy.is_absolute():
        snapshot_operator_policy = (repo_root / snapshot_operator_policy).resolve()
    if parsed.timeout_seconds <= 0:
        raise SystemExit("--timeout-seconds must be positive")
    return GateOptions(
        repo_root=repo_root,
        output_dir=output_dir,
        operations_policy_path=operations_policy,
        snapshot_operator_policy_path=snapshot_operator_policy,
        vault_bin=str(parsed.vault_bin),
        docker_bin=str(parsed.docker_bin),
        timeout_seconds=float(parsed.timeout_seconds),
        vault_client_timeout=str(parsed.vault_client_timeout).strip() or DEFAULT_VAULT_CLIENT_TIMEOUT,
    )


def _read_environment_value(name: str, label: str) -> str:
    try:
        return acceptance.read_environment_value(
            name,
            label,
            maximum_length=64 << 10,
            forbidden_characters="\r\n\x00",
        )
    except acceptance.EnvironmentValueError as error:
        raise ReleaseGateError(
            "release.vault_snapshot_restore_environment_invalid",
            f"The required {label} environment value was unavailable or invalid.",
            {"environment": name, "reason": error.reason},
        ) from None


def _materialize_file_copy(
    source_path: str,
    destination: pathlib.Path,
    *,
    code: str,
    message: str,
) -> pathlib.Path:
    try:
        source = pathlib.Path(source_path).expanduser().resolve(strict=True)
        if not source.is_file():
            raise OSError
        data = source.read_bytes()
    except OSError:
        raise ReleaseGateError(code, message) from None
    if not data:
        raise ReleaseGateError(code, message)
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.write_bytes(data)
    destination.chmod(0o600)
    return destination


def prepare_secret_inputs(
    policy: OperationsPolicy,
    options: GateOptions,
    *,
    state_dir: pathlib.Path,
    redactor: acceptance.SecretRedactor,
) -> SecretInputs:
    source_vault_addr = _read_environment_value(policy.source_vault_addr_env, "Vault address")
    source_vault_cacert = _read_environment_value(
        policy.source_vault_cacert_env,
        "Vault CA certificate path",
    )
    source_role_id = _read_environment_value(
        policy.source_role_id_env,
        "snapshot operator AppRole role_id",
    )
    source_secret_id = _read_environment_value(
        policy.source_secret_id_env,
        "snapshot operator AppRole secret_id",
    )
    source_unseal_keys = tuple(
        _read_environment_value(environment, f"restore unseal key share {index}")
        for index, environment in enumerate(policy.source_unseal_key_envs, start=1)
    )
    redactor.add(source_role_id, "[REDACTED_SNAPSHOT_OPERATOR_ROLE_ID]")
    redactor.add(source_secret_id, "[REDACTED_SNAPSHOT_OPERATOR_SECRET_ID]")
    for index, key in enumerate(source_unseal_keys, start=1):
        redactor.add(key, f"[REDACTED_RESTORE_UNSEAL_KEY_{index}]")
    materialized_vault_cacert = _materialize_file_copy(
        source_vault_cacert,
        state_dir / "certificates" / "source-vault-ca.crt",
        code="release.vault_snapshot_restore_environment_invalid",
        message="The configured Vault CA certificate path was unavailable or invalid.",
    )
    return SecretInputs(
        source_vault_environment={
            "VAULT_ADDR": source_vault_addr,
            "VAULT_CACERT": str(materialized_vault_cacert),
            "VAULT_CLIENT_TIMEOUT": options.vault_client_timeout,
        },
        source_role_id=source_role_id,
        source_secret_id=source_secret_id,
        source_unseal_keys=source_unseal_keys,
        source_role_id_env=policy.source_role_id_env,
        source_secret_id_env=policy.source_secret_id_env,
        source_unseal_key_envs=policy.source_unseal_key_envs,
        source_vault_addr_env=policy.source_vault_addr_env,
        source_vault_cacert_env=policy.source_vault_cacert_env,
    )


def ensure_private_directory(path: pathlib.Path) -> None:
    path.mkdir(parents=True, exist_ok=True)
    path.chmod(0o700)


def _run_command(
    executable: str,
    arguments: Sequence[str],
    *,
    cwd: pathlib.Path,
    environment: Mapping[str, str] | None = None,
    input_text: str | None = None,
    timeout: float,
    code: str,
    message: str,
) -> subprocess.CompletedProcess[str]:
    try:
        return subprocess.run(
            [executable, *arguments],
            cwd=cwd,
            env={**os.environ, **(environment or {})},
            input=input_text,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            check=False,
            timeout=timeout,
        )
    except (OSError, subprocess.TimeoutExpired):
        raise ReleaseGateError(code, message, {"executable": pathlib.Path(executable).name}) from None


def output_excerpt(completed: subprocess.CompletedProcess[str], redactor: acceptance.SecretRedactor) -> str:
    combined = (completed.stdout + "\n" + completed.stderr).strip()
    return redactor.text(combined)[:2000]


def json_payload(
    completed: subprocess.CompletedProcess[str],
    *,
    redactor: acceptance.SecretRedactor,
    code: str,
    message: str,
    allowed_returncodes: Sequence[int] = (0,),
) -> Mapping[str, Any]:
    if completed.returncode not in allowed_returncodes:
        raise ReleaseGateError(code, message, {"outputExcerpt": output_excerpt(completed, redactor)})
    try:
        payload = json.loads(completed.stdout)
    except json.JSONDecodeError:
        raise ReleaseGateError(code, message, {"outputExcerpt": output_excerpt(completed, redactor)}) from None
    if not isinstance(payload, Mapping):
        raise ReleaseGateError(code, message)
    return payload


def approle_login(
    options: GateOptions,
    *,
    environment: Mapping[str, str],
    role_id: str,
    secret_id: str,
    redactor: acceptance.SecretRedactor,
) -> tuple[str, Mapping[str, Any]]:
    login_script = (
        'read -r ROLE_ID\n'
        'read -r SECRET_ID\n'
        'if [ -n "${SYNARA_VAULT_ADDR:-}" ]; then export VAULT_ADDR="$SYNARA_VAULT_ADDR"; fi\n'
        'exec "$SYNARA_VAULT_BIN" write -format=json auth/approle/login '
        'role_id="$ROLE_ID" secret_id="$SECRET_ID"\n'
    )
    completed = _run_command(
        "/bin/sh",
        ["-c", login_script],
        cwd=options.repo_root,
        environment={
            **environment,
            "SYNARA_VAULT_BIN": options.vault_bin,
            "SYNARA_VAULT_ADDR": str(environment.get("VAULT_ADDR") or ""),
        },
        input_text=f"{role_id}\n{secret_id}\n",
        timeout=options.timeout_seconds,
        code="release.vault_snapshot_restore_vault_failed",
        message="The source Vault AppRole login could not complete.",
    )
    payload = json_payload(
        completed,
        redactor=redactor,
        code="release.vault_snapshot_restore_vault_invalid",
        message="The Vault AppRole login did not return valid JSON.",
    )
    auth = require_mapping(payload.get("auth"), label="AppRole auth response")
    token = require_string(auth.get("client_token"), label="AppRole auth.client_token")
    redactor.add(token, "[REDACTED_VAULT_AUTH_TOKEN]")
    return token, payload


def vault_completed(
    options: GateOptions,
    arguments: Sequence[str],
    *,
    environment: Mapping[str, str],
    redactor: acceptance.SecretRedactor,
    timeout: float,
    code: str,
    message: str,
) -> subprocess.CompletedProcess[str]:
    return _run_command(
        options.vault_bin,
        arguments,
        cwd=options.repo_root,
        environment=environment,
        timeout=timeout,
        code=code,
        message=message,
    )


def vault_json(
    options: GateOptions,
    arguments: Sequence[str],
    *,
    environment: Mapping[str, str],
    redactor: acceptance.SecretRedactor,
    timeout: float,
    allowed_returncodes: Sequence[int] = (0,),
    code: str,
    message: str,
) -> Mapping[str, Any]:
    return json_payload(
        vault_completed(
            options,
            arguments,
            environment=environment,
            redactor=redactor,
            timeout=timeout,
            code=code,
            message=message,
        ),
        redactor=redactor,
        code=code,
        message=message,
        allowed_returncodes=allowed_returncodes,
    )


def docker_completed(
    options: GateOptions,
    arguments: Sequence[str],
    *,
    redactor: acceptance.SecretRedactor,
    input_text: str | None = None,
    timeout: float,
    code: str,
    message: str,
) -> subprocess.CompletedProcess[str]:
    return _run_command(
        options.docker_bin,
        arguments,
        cwd=options.repo_root,
        input_text=input_text,
        timeout=timeout,
        code=code,
        message=message,
    )


def docker_exec_completed(
    options: GateOptions,
    container_name: str,
    command: Sequence[str],
    *,
    input_text: str | None = None,
    environment: Mapping[str, str] | None = None,
    redactor: acceptance.SecretRedactor,
    timeout: float,
    code: str,
    message: str,
) -> subprocess.CompletedProcess[str]:
    arguments: list[str] = ["exec"]
    if input_text is not None:
        arguments.append("-i")
    if environment:
        for name, value in environment.items():
            arguments.extend(["--env", f"{name}={value}"])
    arguments.append(container_name)
    arguments.extend(command)
    return docker_completed(
        options,
        arguments,
        redactor=redactor,
        input_text=input_text,
        timeout=timeout,
        code=code,
        message=message,
    )


def docker_exec_json(
    options: GateOptions,
    container_name: str,
    command: Sequence[str],
    *,
    input_text: str | None = None,
    environment: Mapping[str, str] | None = None,
    redactor: acceptance.SecretRedactor,
    timeout: float,
    allowed_returncodes: Sequence[int] = (0,),
    code: str,
    message: str,
) -> Mapping[str, Any]:
    return json_payload(
        docker_exec_completed(
            options,
            container_name,
            command,
            input_text=input_text,
            environment=environment,
            redactor=redactor,
            timeout=timeout,
            code=code,
            message=message,
        ),
        redactor=redactor,
        code=code,
        message=message,
        allowed_returncodes=allowed_returncodes,
    )


def docker_vault_json(
    options: GateOptions,
    context: DockerRestoreContext,
    arguments: Sequence[str],
    *,
    token: str,
    redactor: acceptance.SecretRedactor,
    timeout: float,
    code: str,
    message: str,
) -> Mapping[str, Any]:
    return docker_exec_json(
        options,
        context.container_name,
        [
            "sh",
            "-c",
            'read -r VAULT_TOKEN\nexport VAULT_TOKEN VAULT_ADDR="$RESTORE_VAULT_ADDR"\n'
            'exec vault "$@"\n',
            "synara-vault-restore",
            *arguments,
        ],
        input_text=f"{token}\n",
        environment={"RESTORE_VAULT_ADDR": context.restore_vault_addr},
        redactor=redactor,
        timeout=timeout,
        code=code,
        message=message,
    )


def file_sha256(path: pathlib.Path) -> str:
    return sha256_bytes(path.read_bytes())


def load_snapshot_operator_policy(path: pathlib.Path) -> tuple[str, str]:
    try:
        text = path.read_text(encoding="utf-8")
    except OSError:
        raise ReleaseGateError(
            "release.vault_snapshot_restore_policy_invalid",
            "The checked-in snapshot operator policy file was unavailable.",
            {"path": str(path)},
        ) from None
    if 'path "sys/storage/raft/snapshot"' not in text:
        raise ReleaseGateError(
            "release.vault_snapshot_restore_policy_invalid",
            "The checked-in snapshot operator policy did not grant the Raft snapshot read path.",
        )
    required_audit_stanza = 'path "sys/audit" {\n  capabilities = ["read", "sudo"]\n}'
    if required_audit_stanza not in text:
        raise ReleaseGateError(
            "release.vault_snapshot_restore_policy_invalid",
            "The checked-in snapshot operator policy did not grant read-only audit device inspection.",
        )
    return text, sha256_text(text)


def token_lookup_summary(payload: Mapping[str, Any]) -> dict[str, Any]:
    data = require_mapping(payload.get("data"), label="token lookup data")
    policies_raw = data.get("policies")
    if not isinstance(policies_raw, list):
        policies_raw = data.get("token_policies")
    policies = sorted(
        str(item) for item in policies_raw if isinstance(item, str)
    ) if isinstance(policies_raw, list) else []
    token_type = data.get("type")
    if not isinstance(token_type, str) or not token_type:
        token_type = str(data.get("token_type") or "")
    return {
        "path": str(data.get("path") or ""),
        "displayName": str(data.get("display_name") or ""),
        "tokenType": token_type,
        "tokenPolicies": policies,
        "tokenPoliciesSha256": sha256_text("\n".join(policies)),
    }


def approle_login_summary(payload: Mapping[str, Any]) -> dict[str, Any]:
    auth = require_mapping(payload.get("auth"), label="AppRole login auth")
    policies_raw = auth.get("token_policies")
    if not isinstance(policies_raw, list):
        policies_raw = auth.get("policies")
    policies = sorted(
        str(item) for item in policies_raw if isinstance(item, str)
    ) if isinstance(policies_raw, list) else []
    token_type = auth.get("token_type")
    if not isinstance(token_type, str) or not token_type:
        token_type = str(auth.get("type") or "")
    return {
        "accessorSha256": sha256_text(str(auth.get("accessor") or "")),
        "displayName": str(auth.get("display_name") or ""),
        "leaseDurationSeconds": int(auth.get("lease_duration") or 0),
        "renewable": bool(auth.get("renewable")),
        "tokenPolicies": policies,
        "tokenPoliciesSha256": sha256_text("\n".join(policies)),
        "tokenType": token_type,
    }


def validate_snapshot_operator_identity(
    policy: OperationsPolicy,
    identity: Mapping[str, Any],
) -> dict[str, Any]:
    actual_policies = identity.get("tokenPolicies")
    if not isinstance(actual_policies, list):
        raise ReleaseGateError(
            "release.vault_snapshot_restore_validation_failed",
            "The snapshot operator token lookup did not expose token policies.",
        )
    expected_policies = sorted(policy.snapshot_operator_role_policy.token_policies)
    if sorted(str(item) for item in actual_policies) != expected_policies:
        raise ReleaseGateError(
            "release.vault_snapshot_restore_validation_failed",
            "The snapshot operator token policies drifted from the checked-in production boundary.",
            {
                "expectedPolicies": expected_policies,
                "actualPolicies": actual_policies,
            },
        )
    if policy.snapshot_operator_role_policy.token_no_default_policy and "default" in actual_policies:
        raise ReleaseGateError(
            "release.vault_snapshot_restore_validation_failed",
            "The snapshot operator token unexpectedly retained the default policy.",
        )
    if identity.get("tokenType") != policy.snapshot_operator_role_policy.token_type:
        raise ReleaseGateError(
            "release.vault_snapshot_restore_validation_failed",
            "The snapshot operator token type drifted from the checked-in production boundary.",
            {
                "expectedTokenType": policy.snapshot_operator_role_policy.token_type,
                "actualTokenType": identity.get("tokenType"),
            },
        )
    return {
        "tokenPolicies": expected_policies,
        "tokenPoliciesSha256": sha256_text("\n".join(expected_policies)),
        "tokenType": policy.snapshot_operator_role_policy.token_type,
        "path": str(identity.get("path") or ""),
        "displayName": str(identity.get("displayName") or ""),
    }


def approle_summary(payload: Mapping[str, Any]) -> dict[str, Any]:
    data = require_mapping(payload.get("data"), label="AppRole data")
    token_policies = data.get("token_policies")
    policies = sorted(str(item) for item in token_policies if isinstance(item, str)) if isinstance(token_policies, list) else []
    summary = {
        "tokenPolicies": policies,
        "tokenType": str(data.get("token_type") or ""),
        "tokenTtlSeconds": int(data.get("token_ttl") or 0),
        "tokenMaxTtlSeconds": int(data.get("token_max_ttl") or 0),
        "tokenNumUses": int(data.get("token_num_uses") or 0),
        "secretIdTtlSeconds": int(data.get("secret_id_ttl") or 0),
        "secretIdNumUses": int(data.get("secret_id_num_uses") or 0),
        "tokenNoDefaultPolicy": bool(data.get("token_no_default_policy")),
        "sha256": stable_json_sha256(data),
    }
    if isinstance(data.get("bind_secret_id"), bool):
        summary["bindSecretId"] = bool(data["bind_secret_id"])
    return summary


def validate_snapshot_operator_role(
    policy: OperationsPolicy,
    role_summary: Mapping[str, Any],
) -> None:
    expected = policy.snapshot_operator_role_policy
    mismatches: dict[str, Any] = {}
    if role_summary.get("tokenPolicies") != list(expected.token_policies):
        mismatches["expectedPolicies"] = list(expected.token_policies)
        mismatches["actualPolicies"] = role_summary.get("tokenPolicies")
    if role_summary.get("tokenType") != expected.token_type:
        mismatches["expectedTokenType"] = expected.token_type
        mismatches["actualTokenType"] = role_summary.get("tokenType")
    if role_summary.get("tokenTtlSeconds") != expected.token_ttl_seconds:
        mismatches["expectedTokenTtlSeconds"] = expected.token_ttl_seconds
        mismatches["actualTokenTtlSeconds"] = role_summary.get("tokenTtlSeconds")
    if role_summary.get("tokenMaxTtlSeconds") != expected.token_max_ttl_seconds:
        mismatches["expectedTokenMaxTtlSeconds"] = expected.token_max_ttl_seconds
        mismatches["actualTokenMaxTtlSeconds"] = role_summary.get("tokenMaxTtlSeconds")
    if role_summary.get("tokenNumUses") != expected.token_num_uses:
        mismatches["expectedTokenNumUses"] = expected.token_num_uses
        mismatches["actualTokenNumUses"] = role_summary.get("tokenNumUses")
    if role_summary.get("secretIdTtlSeconds") != expected.secret_id_ttl_seconds:
        mismatches["expectedSecretIdTtlSeconds"] = expected.secret_id_ttl_seconds
        mismatches["actualSecretIdTtlSeconds"] = role_summary.get("secretIdTtlSeconds")
    if role_summary.get("secretIdNumUses") != expected.secret_id_num_uses:
        mismatches["expectedSecretIdNumUses"] = expected.secret_id_num_uses
        mismatches["actualSecretIdNumUses"] = role_summary.get("secretIdNumUses")
    if role_summary.get("tokenNoDefaultPolicy") is not expected.token_no_default_policy:
        mismatches["expectedTokenNoDefaultPolicy"] = expected.token_no_default_policy
        mismatches["actualTokenNoDefaultPolicy"] = role_summary.get("tokenNoDefaultPolicy")
    if mismatches:
        raise ReleaseGateError(
            "release.vault_snapshot_restore_validation_failed",
            "The snapshot operator AppRole drifted from the checked-in production boundary.",
            mismatches,
        )


def status_summary(payload: Mapping[str, Any]) -> dict[str, Any]:
    initialized = payload.get("initialized")
    sealed = payload.get("sealed")
    storage_type = payload.get("storage_type")
    if not isinstance(initialized, bool) or not isinstance(sealed, bool) or not isinstance(storage_type, str):
        raise ReleaseGateError(
            "release.vault_snapshot_restore_validation_failed",
            "Vault status did not expose initialized, sealed, and storage_type fields.",
        )
    return {
        "initialized": initialized,
        "sealed": sealed,
        "storageType": storage_type,
        "version": str(payload.get("version") or ""),
        "haEnabled": bool(payload.get("ha_enabled")),
        "clusterIdSha256": sha256_text(str(payload.get("cluster_id") or "")),
    }


def raft_summary(payload: Mapping[str, Any]) -> dict[str, Any]:
    data = require_mapping(payload.get("data"), label="raft configuration data")
    config = require_mapping(data.get("config"), label="raft configuration")
    servers = config.get("servers")
    if not isinstance(servers, list) or not servers:
        raise ReleaseGateError(
            "release.vault_snapshot_restore_validation_failed",
            "The raft configuration did not expose at least one server.",
        )
    roles = sorted(
        f"{'leader' if server.get('leader') is True else 'follower'}-"
        f"{'voter' if server.get('voter') is True else 'nonvoter'}"
        for server in servers
        if isinstance(server, Mapping)
    )
    return {
        "serverCount": len(servers),
        "leaderCount": sum(
            1 for server in servers if isinstance(server, Mapping) and server.get("leader") is True
        ),
        "voterCount": sum(
            1 for server in servers if isinstance(server, Mapping) and server.get("voter") is True
        ),
        "serverRoles": roles,
        "sha256": stable_json_sha256(data),
    }


def key_summary(payload: Mapping[str, Any]) -> dict[str, Any]:
    data = require_mapping(payload.get("data"), label="transit key data")
    name = require_string(data.get("name"), label="transit key name")
    key_type = require_string(data.get("type"), label="transit key type")
    return {
        "name": name,
        "type": key_type,
        "sha256": stable_json_sha256(data),
    }


def audit_summary(payload: Mapping[str, Any]) -> dict[str, Any]:
    devices: list[dict[str, str]] = []
    for expected in EXPECTED_AUDIT_DEVICES:
        path = expected["path"]
        device = require_mapping(payload.get(f"{path}/"), label=f"audit device {path}")
        options = require_mapping(device.get("options"), label=f"audit device {path} options")
        devices.append(
            {
                "path": path,
                "type": require_string(device.get("type"), label=f"audit device {path} type"),
                "filePath": require_string(
                    options.get("file_path"),
                    label=f"audit device {path} file_path",
                ),
            }
        )
    if len(payload) != len(EXPECTED_AUDIT_DEVICES) or any(
        device["type"] != "file"
        or device["filePath"] != expected["filePath"]
        for device, expected in zip(devices, EXPECTED_AUDIT_DEVICES, strict=True)
    ):
        raise ReleaseGateError(
            "release.vault_snapshot_restore_validation_failed",
            "The restored Vault audit devices did not match the two approved file sinks.",
        )
    return {
        "deviceCount": len(devices),
        "devices": devices,
        "sha256": stable_json_sha256(devices),
    }


def capture_restored_audit_file_metadata(
    options: GateOptions,
    context: DockerRestoreContext,
    *,
    redactor: acceptance.SecretRedactor,
) -> dict[str, Any]:
    payload = docker_exec_json(
        options,
        context.container_name,
        [
            "sh",
            "-c",
            'set -eu\n'
            'primary=/vault/audit/audit-primary.log\n'
            'secondary=/vault/audit/audit-secondary.log\n'
            'for file in "$primary" "$secondary"; do test -f "$file"; done\n'
            'printf \'{"primary":{"mode":"%s","uid":%s,"gid":%s,"sizeBytes":%s},'
            '"secondary":{"mode":"%s","uid":%s,"gid":%s,"sizeBytes":%s}}\\n\' '
            '"$(stat -c %a "$primary")" "$(stat -c %u "$primary")" '
            '"$(stat -c %g "$primary")" "$(stat -c %s "$primary")" '
            '"$(stat -c %a "$secondary")" "$(stat -c %u "$secondary")" '
            '"$(stat -c %g "$secondary")" "$(stat -c %s "$secondary")"\n',
        ],
        redactor=redactor,
        timeout=min(options.timeout_seconds, 15.0),
        code="release.vault_snapshot_restore_docker_failed",
        message="The isolated restore Vault audit file metadata could not be captured.",
    )
    for label in ("primary", "secondary"):
        metadata = require_mapping(payload.get(label), label=f"restored audit file {label}")
        if (
            metadata.get("mode") != "600"
            or metadata.get("uid") != 100
            or metadata.get("gid") != 1000
            or isinstance(metadata.get("sizeBytes"), bool)
            or not isinstance(metadata.get("sizeBytes"), int)
            or metadata.get("sizeBytes", 0) <= 0
        ):
            raise ReleaseGateError(
                "release.vault_snapshot_restore_validation_failed",
                "The isolated restore Vault audit files were not writable with the approved ownership and mode.",
                {"file": label, "metadata": dict(metadata)},
            )
    return dict(payload)


def capture_source_state(
    policy: OperationsPolicy,
    options: GateOptions,
    *,
    source_environment: Mapping[str, str],
    redactor: acceptance.SecretRedactor,
) -> dict[str, Any]:
    identity = token_lookup_summary(
        vault_json(
            options,
            ["token", "lookup", "-format=json"],
            environment=source_environment,
            redactor=redactor,
            timeout=options.timeout_seconds,
            code="release.vault_snapshot_restore_vault_failed",
            message="The source Vault token lookup could not complete.",
        )
    )
    validated_identity = validate_snapshot_operator_identity(policy, identity)
    status = status_summary(
        vault_json(
            options,
            ["status", "-format=json"],
            environment=source_environment,
            redactor=redactor,
            timeout=options.timeout_seconds,
            code="release.vault_snapshot_restore_vault_failed",
            message="The source Vault status command could not complete.",
        )
    )
    if status["storageType"] != "raft":
        raise ReleaseGateError(
            "release.vault_snapshot_restore_validation_failed",
            "The source Vault did not use raft storage.",
            {"storageType": status["storageType"]},
        )
    raft = raft_summary(
        vault_json(
            options,
            ["read", "-format=json", "sys/storage/raft/configuration"],
            environment=source_environment,
            redactor=redactor,
            timeout=options.timeout_seconds,
            code="release.vault_snapshot_restore_vault_failed",
            message="The source Vault raft configuration read could not complete.",
        )
    )
    if raft["leaderCount"] != 1 or raft["voterCount"] != raft["serverCount"]:
        raise ReleaseGateError(
            "release.vault_snapshot_restore_validation_failed",
            "The source Vault Raft cluster did not expose exactly one leader with all servers voting.",
            raft,
        )
    key = key_summary(
        vault_json(
            options,
            ["read", "-format=json", f"transit/keys/{policy.transit_key_name}"],
            environment=source_environment,
            redactor=redactor,
            timeout=options.timeout_seconds,
            code="release.vault_snapshot_restore_vault_failed",
            message="The source Vault transit key read could not complete.",
        )
    )
    audit = audit_summary(
        vault_json(
            options,
            ["audit", "list", "-format=json"],
            environment=source_environment,
            redactor=redactor,
            timeout=options.timeout_seconds,
            code="release.vault_snapshot_restore_vault_failed",
            message="The source Vault audit device list could not complete.",
        )
    )
    if key["type"] != EXPECTED_TRANSIT_KEY_TYPE:
        raise ReleaseGateError(
            "release.vault_snapshot_restore_validation_failed",
            "The source Vault transit key drifted from the expected production key type.",
            {"expectedType": EXPECTED_TRANSIT_KEY_TYPE, "actualType": key["type"]},
        )
    roles = {
        "signer": approle_summary(
            vault_json(
                options,
                ["read", "-format=json", f"auth/approle/role/{policy.signer_role_name}"],
                environment=source_environment,
                redactor=redactor,
                timeout=options.timeout_seconds,
                code="release.vault_snapshot_restore_vault_failed",
                message="The signer AppRole read could not complete.",
            )
        ),
        "auditor": approle_summary(
            vault_json(
                options,
                ["read", "-format=json", f"auth/approle/role/{policy.auditor_role_name}"],
                environment=source_environment,
                redactor=redactor,
                timeout=options.timeout_seconds,
                code="release.vault_snapshot_restore_vault_failed",
                message="The auditor AppRole read could not complete.",
            )
        ),
        "snapshotOperator": approle_summary(
            vault_json(
                options,
                ["read", "-format=json", f"auth/approle/role/{policy.snapshot_operator_role_name}"],
                environment=source_environment,
                redactor=redactor,
                timeout=options.timeout_seconds,
                code="release.vault_snapshot_restore_vault_failed",
                message="The snapshot operator AppRole read could not complete.",
            )
        ),
    }
    validate_snapshot_operator_role(policy, roles["snapshotOperator"])
    return {
        "identity": validated_identity,
        "status": status,
        "raft": raft,
        "key": key,
        "audit": audit,
        "roles": roles,
    }


def capture_source_snapshot(
    options: GateOptions,
    *,
    source_environment: Mapping[str, str],
    snapshot_path: pathlib.Path,
    redactor: acceptance.SecretRedactor,
) -> dict[str, Any]:
    snapshot_path.parent.mkdir(parents=True, exist_ok=True)
    completed = vault_completed(
        options,
        ["operator", "raft", "snapshot", "save", str(snapshot_path)],
        environment=source_environment,
        redactor=redactor,
        timeout=options.timeout_seconds,
        code="release.vault_snapshot_restore_vault_failed",
        message="The source Vault snapshot save could not complete.",
    )
    if completed.returncode != 0 or not snapshot_path.is_file():
        raise ReleaseGateError(
            "release.vault_snapshot_restore_vault_failed",
            "The source Vault snapshot save did not produce a snapshot file.",
            {"outputExcerpt": output_excerpt(completed, redactor)},
        )
    snapshot_path.chmod(0o600)
    return {
        "sizeBytes": snapshot_path.stat().st_size,
        "sha256": file_sha256(snapshot_path),
    }


def start_restore_container(
    policy: OperationsPolicy,
    options: GateOptions,
    *,
    run_id: str,
    state_dir: pathlib.Path,
    redactor: acceptance.SecretRedactor,
) -> DockerRestoreContext:
    if policy.restore_docker_network_mode != "none":
        raise ReleaseGateError(
            "release.vault_snapshot_restore_policy_invalid",
            "The restore policy must keep the isolated Docker network mode at 'none'.",
        )
    if policy.restore_audit_tmpfs != DEFAULT_RESTORE_AUDIT_TMPFS:
        raise ReleaseGateError(
            "release.vault_snapshot_restore_policy_invalid",
            "The restore policy must mount the hardened UID 100 audit tmpfs.",
        )
    restore_root = state_dir / DEFAULT_RESTORE_STATE_SUBDIRECTORY
    config_root = state_dir / DEFAULT_RESTORE_CONFIG_SUBDIRECTORY
    ensure_private_directory(restore_root)
    ensure_private_directory(config_root)
    config_path = config_root / "vault.hcl"
    config_text = "\n".join(
        (
            'disable_mlock = true',
            'ui = false',
            'cluster_name = "synara-vault-restore-drill"',
            'listener "tcp" {',
            '  address = "127.0.0.1:8200"',
            f'  cluster_address = "{DEFAULT_RESTORE_RAFT_ADDRESS}"',
            '  tls_disable = true',
            '}',
            'storage "raft" {',
            '  path = "/vault/file"',
            f'  node_id = "{DEFAULT_RESTORE_NODE_ID}"',
            '}',
            f'api_addr = "{DEFAULT_RESTORE_VAULT_ADDR}"',
            f'cluster_addr = "{DEFAULT_RESTORE_VAULT_CLUSTER_ADDR}"',
            "",
        )
    )
    if "retry_join" in config_text:
        raise ReleaseGateError(
            "release.vault_snapshot_restore_policy_invalid",
            "The isolated restore configuration unexpectedly retained retry_join.",
        )
    config_path.write_text(config_text, encoding="utf-8")
    config_path.chmod(0o600)
    container_name = f"synara-vault-restore-{run_id[:12]}"
    completed = docker_completed(
        options,
        [
            "run",
            "--detach",
            "--name",
            container_name,
            "--network",
            policy.restore_docker_network_mode,
            "--cap-drop",
            "ALL",
            "--security-opt",
            "no-new-privileges",
            "--volume",
            f"{restore_root}:/vault/file",
            "--tmpfs",
            policy.restore_audit_tmpfs,
            "--volume",
            f"{config_path}:/vault/config/vault.hcl:ro",
            policy.restore_docker_image,
            "server",
        ],
        redactor=redactor,
        timeout=options.timeout_seconds,
        code="release.vault_snapshot_restore_docker_failed",
        message="The isolated restore Vault container could not start.",
    )
    if completed.returncode != 0:
        raise ReleaseGateError(
            "release.vault_snapshot_restore_docker_failed",
            "The isolated restore Vault container returned a failure on startup.",
            {"outputExcerpt": output_excerpt(completed, redactor)},
        )
    return DockerRestoreContext(
        container_name=container_name,
        state_dir=state_dir,
        config_path=config_path,
        snapshot_path=state_dir / DEFAULT_SOURCE_SNAPSHOT_NAME,
        restore_vault_addr=DEFAULT_RESTORE_VAULT_ADDR,
        docker_image=policy.restore_docker_image,
    )


def wait_for_restore_status(
    options: GateOptions,
    context: DockerRestoreContext,
    *,
    redactor: acceptance.SecretRedactor,
    initialized: bool | None = None,
    sealed: bool | None = None,
    attempts: int = 30,
) -> Mapping[str, Any]:
    last_error: ReleaseGateError | None = None
    for _ in range(attempts):
        try:
            payload = docker_exec_json(
                options,
                context.container_name,
                ["vault", "status", "-format=json"],
                environment={"VAULT_ADDR": context.restore_vault_addr},
                redactor=redactor,
                timeout=min(options.timeout_seconds, 15.0),
                allowed_returncodes=(0, 1, 2),
                code="release.vault_snapshot_restore_docker_failed",
                message="The isolated restore Vault status probe did not return valid JSON.",
            )
        except ReleaseGateError as error:
            last_error = error
            time.sleep(1.0)
            continue
        if (
            (initialized is None or payload.get("initialized") is initialized)
            and (sealed is None or payload.get("sealed") is sealed)
        ):
            return payload
        time.sleep(1.0)
    logs = docker_completed(
        options,
        ["logs", "--tail", "100", context.container_name],
        redactor=redactor,
        timeout=min(options.timeout_seconds, 15.0),
        code="release.vault_snapshot_restore_docker_failed",
        message="The isolated restore Vault logs could not be read.",
    )
    raise ReleaseGateError(
        "release.vault_snapshot_restore_docker_failed",
        "The isolated restore Vault did not reach the expected status in time.",
        {
            "expectedInitialized": initialized,
            "expectedSealed": sealed,
            "lastProbeError": last_error.as_report_error() if last_error is not None else None,
            "containerLogExcerpt": output_excerpt(logs, redactor),
        },
    )


def docker_exec_unseal(
    options: GateOptions,
    context: DockerRestoreContext,
    *,
    unseal_key: str,
    redactor: acceptance.SecretRedactor,
) -> Mapping[str, Any]:
    completed = docker_exec_completed(
        options,
        context.container_name,
        [
            "sh",
            "-c",
            'umask 077\nread -r UNSEAL_KEY\n'
            'PAYLOAD="/tmp/synara-unseal-$$.json"\nRESPONSE="${PAYLOAD}.response"\n'
            'trap \'rm -f "$PAYLOAD" "$RESPONSE"\' EXIT\n'
            'printf \'{"key":"%s"}\' "$UNSEAL_KEY" > "$PAYLOAD"\n'
            'CONTENT_LENGTH=$(wc -c < "$PAYLOAD" | tr -d " ")\n'
            '{ printf \'PUT /v1/sys/unseal HTTP/1.1\\r\\nHost: 127.0.0.1:8200\\r\\n'
            'Content-Type: application/json\\r\\nContent-Length: %s\\r\\n'
            'Connection: close\\r\\n\\r\\n\' "$CONTENT_LENGTH"; cat "$PAYLOAD"; } | '
            'nc -w 15 127.0.0.1 8200 > "$RESPONSE"\n'
            'if ! sed -n \'1p\' "$RESPONSE" | grep -Eq \'^HTTP/1\\.[01] 200 \' ; then '
            'sed \'1,/^\\r$/d\' "$RESPONSE"; exit 22; fi\n'
            'sed \'1,/^\\r$/d\' "$RESPONSE"\n',
        ],
        input_text=f"{unseal_key}\n",
        environment={"RESTORE_VAULT_ADDR": context.restore_vault_addr},
        redactor=redactor,
        timeout=options.timeout_seconds,
        code="release.vault_snapshot_restore_docker_failed",
        message="The isolated restore Vault unseal step failed.",
    )
    try:
        payload = json_payload(
            completed,
            redactor=redactor,
            code="release.vault_snapshot_restore_docker_failed",
            message="The isolated restore Vault unseal step failed.",
        )
    except ReleaseGateError:
        if '"context canceled"' not in completed.stdout:
            raise
        payload = docker_exec_json(
            options,
            context.container_name,
            ["vault", "status", "-format=json"],
            environment={"VAULT_ADDR": context.restore_vault_addr},
            redactor=redactor,
            timeout=min(options.timeout_seconds, 15.0),
            allowed_returncodes=(0, 1, 2),
            code="release.vault_snapshot_restore_docker_failed",
            message="The isolated restore Vault status fallback did not return valid JSON.",
        )
    if (
        not isinstance(payload.get("sealed"), bool)
        or isinstance(payload.get("progress"), bool)
        or not isinstance(payload.get("progress"), int)
        or isinstance(payload.get("t"), bool)
        or not isinstance(payload.get("t"), int)
        or isinstance(payload.get("n"), bool)
        or not isinstance(payload.get("n"), int)
    ):
        raise ReleaseGateError(
            "release.vault_snapshot_restore_docker_failed",
            "The isolated restore Vault unseal response was malformed.",
        )
    return payload


def wait_for_snapshot_restore_application(
    options: GateOptions,
    context: DockerRestoreContext,
    *,
    redactor: acceptance.SecretRedactor,
    attempts: int = 30,
) -> str:
    last_logs = ""
    for _ in range(attempts):
        completed = docker_completed(
            options,
            ["logs", "--tail", "300", context.container_name],
            redactor=redactor,
            timeout=min(options.timeout_seconds, 15.0),
            code="release.vault_snapshot_restore_docker_failed",
            message="The isolated restore Vault logs could not be read.",
        )
        if completed.returncode != 0:
            time.sleep(1.0)
            continue
        last_logs = redactor.text(completed.stdout + "\n" + completed.stderr)
        marker = "applying snapshot"
        if marker not in last_logs:
            time.sleep(1.0)
            continue
        after_apply = last_logs.rsplit(marker, 1)[1]
        if (
            "error while restoring raft snapshot" in after_apply
            or "raft snapshot restore failed preSeal" in after_apply
        ):
            raise ReleaseGateError(
                "release.vault_snapshot_restore_docker_failed",
                "The isolated restore Vault failed while applying the Raft snapshot.",
                {"containerLogExcerpt": after_apply[-2000:]},
            )
        if (
            "raft snapshot restore failed postUnseal" in after_apply
            or "post-unseal setup complete" in after_apply
            or (
                "restored user snapshot" in after_apply
                and "vault is sealed" in after_apply
            )
        ):
            return sha256_text(after_apply)
        time.sleep(1.0)
    raise ReleaseGateError(
        "release.vault_snapshot_restore_docker_failed",
        "The isolated restore Vault did not finish applying the Raft snapshot in time.",
        {"containerLogExcerpt": last_logs[-2000:]},
    )


def finish_restored_snapshot_application(
    options: GateOptions,
    context: DockerRestoreContext,
    *,
    redactor: acceptance.SecretRedactor,
) -> dict[str, Any]:
    application_log_sha256 = wait_for_snapshot_restore_application(
        options,
        context,
        redactor=redactor,
    )
    sealed_status = wait_for_restore_status(
        options,
        context,
        redactor=redactor,
        initialized=True,
        sealed=True,
    )
    return {
        "sealedStatusSha256": sha256_text(json.dumps(sealed_status, sort_keys=True)),
        "snapshotApplication": {
            "applicationLogSha256": application_log_sha256,
            "completed": True,
        },
    }


def restore_snapshot_into_isolated_vault(
    policy: OperationsPolicy,
    options: GateOptions,
    context: DockerRestoreContext,
    *,
    redactor: acceptance.SecretRedactor,
) -> dict[str, Any]:
    wait_for_restore_status(options, context, redactor=redactor, initialized=False, sealed=True)
    init_payload = docker_exec_json(
        options,
        context.container_name,
        [
            "vault",
            "operator",
            "init",
            f"-key-shares={policy.custody_total_shares}",
            f"-key-threshold={policy.custody_threshold}",
            "-format=json",
        ],
        environment={"VAULT_ADDR": context.restore_vault_addr},
        redactor=redactor,
        timeout=options.timeout_seconds,
        code="release.vault_snapshot_restore_docker_failed",
        message="The isolated restore Vault init step did not return valid JSON.",
    )
    root_token = require_string(init_payload.get("root_token"), label="restore init root_token")
    unseal_keys = init_payload.get("unseal_keys_b64")
    if (
        not isinstance(unseal_keys, list)
        or len(unseal_keys) != policy.custody_total_shares
        or any(not isinstance(key, str) or not key for key in unseal_keys)
    ):
        raise ReleaseGateError(
            "release.vault_snapshot_restore_docker_failed",
            "The isolated restore Vault init output did not match the production Shamir custody boundary.",
        )
    redactor.add(root_token, "[REDACTED_RESTORE_ROOT_TOKEN]")
    for index, key in enumerate(unseal_keys, start=1):
        redactor.add(key, f"[REDACTED_RESTORE_INIT_UNSEAL_KEY_{index}]")
    init_unseal: Mapping[str, Any] | None = None
    for key in unseal_keys[: policy.custody_threshold]:
        init_unseal = docker_exec_unseal(options, context, unseal_key=key, redactor=redactor)
    if init_unseal is None or init_unseal.get("sealed") is not False:
        raise ReleaseGateError(
            "release.vault_snapshot_restore_docker_failed",
            "The isolated restore Vault did not unseal at the production Shamir threshold.",
        )
    context.snapshot_path.chmod(0o444)
    try:
        copy = docker_completed(
            options,
            ["cp", str(context.snapshot_path), f"{context.container_name}:/tmp/{DEFAULT_SOURCE_SNAPSHOT_NAME}"],
            redactor=redactor,
            timeout=options.timeout_seconds,
            code="release.vault_snapshot_restore_docker_failed",
            message="The source snapshot could not be copied into the isolated restore container.",
        )
    finally:
        context.snapshot_path.chmod(0o600)
    if copy.returncode != 0:
        raise ReleaseGateError(
            "release.vault_snapshot_restore_docker_failed",
            "The source snapshot copy returned a failure.",
            {"outputExcerpt": output_excerpt(copy, redactor)},
        )
    restore = docker_exec_completed(
        options,
        context.container_name,
        [
            "sh",
            "-c",
            'read -r RESTORE_TOKEN\nexport VAULT_ADDR="$RESTORE_VAULT_ADDR"\n'
            'export VAULT_TOKEN="$RESTORE_TOKEN"\n'
            'exec vault operator raft snapshot restore -force "/tmp/source.snap"\n',
        ],
        input_text=f"{root_token}\n",
        environment={"RESTORE_VAULT_ADDR": context.restore_vault_addr},
        redactor=redactor,
        timeout=options.timeout_seconds,
        code="release.vault_snapshot_restore_docker_failed",
        message="The isolated Vault snapshot restore command could not complete.",
    )
    if restore.returncode != 0:
        raise ReleaseGateError(
            "release.vault_snapshot_restore_docker_failed",
            "The isolated Vault snapshot restore command returned a failure.",
            {"outputExcerpt": output_excerpt(restore, redactor)},
        )
    return finish_restored_snapshot_application(
        options,
        context,
        redactor=redactor,
    )


def unseal_restored_snapshot(
    options: GateOptions,
    context: DockerRestoreContext,
    *,
    secret_inputs: SecretInputs,
    redactor: acceptance.SecretRedactor,
) -> list[dict[str, Any]]:
    attempts: list[dict[str, Any]] = []
    for index, key in enumerate(secret_inputs.source_unseal_keys, start=1):
        payload = docker_exec_unseal(options, context, unseal_key=key, redactor=redactor)
        attempts.append(
            {
                "attempt": index,
                "sealed": bool(payload.get("sealed")),
                "progress": int(payload.get("progress") or 0),
                "t": int(payload.get("t") or 0),
                "n": int(payload.get("n") or 0),
            }
        )
        if payload.get("sealed") is False:
            break
    if not attempts or attempts[-1]["sealed"] is not False:
        raise ReleaseGateError(
            "release.vault_snapshot_restore_validation_failed",
            "The restored Vault remained sealed after the supplied Shamir threshold key shares.",
            {"attempts": attempts},
        )
    try:
        wait_for_restore_status(
            options,
            context,
            redactor=redactor,
            initialized=True,
            sealed=False,
        )
    except ReleaseGateError as error:
        raise ReleaseGateError(
            "release.vault_snapshot_restore_validation_failed",
            "The restored Vault accepted the Shamir quorum but did not remain unsealed.",
            {"attempts": attempts, "readinessError": error.as_report_error()},
        ) from None
    return attempts


def capture_restored_state(
    policy: OperationsPolicy,
    options: GateOptions,
    context: DockerRestoreContext,
    *,
    restored_token: str,
    redactor: acceptance.SecretRedactor,
) -> dict[str, Any]:
    status = status_summary(
        wait_for_restore_status(
            options,
            context,
            redactor=redactor,
            initialized=True,
            sealed=False,
        )
    )
    if status["storageType"] != "raft" or status["sealed"] is not False or status["initialized"] is not True:
        raise ReleaseGateError(
            "release.vault_snapshot_restore_validation_failed",
            "The restored Vault did not come back as an initialized unsealed raft cluster.",
            status,
        )
    identity_payload: Mapping[str, Any] | None = None
    last_identity_error: ReleaseGateError | None = None
    for _ in range(30):
        try:
            identity_payload = docker_vault_json(
                options,
                context,
                ["token", "lookup", "-format=json"],
                token=restored_token,
                redactor=redactor,
                timeout=min(options.timeout_seconds, 15.0),
                code="release.vault_snapshot_restore_docker_failed",
                message="The restored Vault token lookup could not complete.",
            )
        except ReleaseGateError as error:
            last_identity_error = error
            time.sleep(1.0)
            continue
        break
    if identity_payload is None:
        logs = docker_completed(
            options,
            ["logs", "--tail", "300", context.container_name],
            redactor=redactor,
            timeout=min(options.timeout_seconds, 15.0),
            code="release.vault_snapshot_restore_docker_failed",
            message="The restored Vault logs could not be read.",
        )
        raise ReleaseGateError(
            "release.vault_snapshot_restore_docker_failed",
            "The restored Vault authentication endpoint did not become ready in time.",
            {
                "lastProbeError": (
                    last_identity_error.as_report_error()
                    if last_identity_error is not None
                    else None
                ),
                "containerLogExcerpt": output_excerpt(logs, redactor),
            },
        )
    identity = token_lookup_summary(identity_payload)
    validated_identity = validate_snapshot_operator_identity(policy, identity)
    raft = raft_summary(
        docker_vault_json(
            options,
            context,
            ["read", "-format=json", "sys/storage/raft/configuration"],
            token=restored_token,
            redactor=redactor,
            timeout=options.timeout_seconds,
            code="release.vault_snapshot_restore_docker_failed",
            message="The restored Vault raft configuration read could not complete.",
        )
    )
    if raft["serverCount"] != 1 or raft["leaderCount"] != 1 or raft["voterCount"] != 1:
        raise ReleaseGateError(
            "release.vault_snapshot_restore_validation_failed",
            "The isolated restored Vault did not form exactly one active voting Raft node.",
            raft,
        )
    key = key_summary(
        docker_vault_json(
            options,
            context,
            ["read", "-format=json", f"transit/keys/{policy.transit_key_name}"],
            token=restored_token,
            redactor=redactor,
            timeout=options.timeout_seconds,
            code="release.vault_snapshot_restore_docker_failed",
            message="The restored Vault transit key read could not complete.",
        )
    )
    audit = audit_summary(
        docker_vault_json(
            options,
            context,
            ["audit", "list", "-format=json"],
            token=restored_token,
            redactor=redactor,
            timeout=options.timeout_seconds,
            code="release.vault_snapshot_restore_docker_failed",
            message="The restored Vault audit device list could not complete.",
        )
    )
    audit_files = capture_restored_audit_file_metadata(
        options,
        context,
        redactor=redactor,
    )
    roles = {
        "signer": approle_summary(
            docker_vault_json(
                options,
                context,
                ["read", "-format=json", f"auth/approle/role/{policy.signer_role_name}"],
                token=restored_token,
                redactor=redactor,
                timeout=options.timeout_seconds,
                code="release.vault_snapshot_restore_docker_failed",
                message="The restored signer AppRole read could not complete.",
            )
        ),
        "auditor": approle_summary(
            docker_vault_json(
                options,
                context,
                ["read", "-format=json", f"auth/approle/role/{policy.auditor_role_name}"],
                token=restored_token,
                redactor=redactor,
                timeout=options.timeout_seconds,
                code="release.vault_snapshot_restore_docker_failed",
                message="The restored auditor AppRole read could not complete.",
            )
        ),
        "snapshotOperator": approle_summary(
            docker_vault_json(
                options,
                context,
                ["read", "-format=json", f"auth/approle/role/{policy.snapshot_operator_role_name}"],
                token=restored_token,
                redactor=redactor,
                timeout=options.timeout_seconds,
                code="release.vault_snapshot_restore_docker_failed",
                message="The restored snapshot operator AppRole read could not complete.",
            )
        ),
    }
    validate_snapshot_operator_role(policy, roles["snapshotOperator"])
    return {
        "identity": validated_identity,
        "status": status,
        "raft": raft,
        "key": key,
        "audit": audit,
        "auditFiles": audit_files,
        "roles": roles,
    }


def compare_source_and_restore(
    policy: OperationsPolicy,
    *,
    source: Mapping[str, Any],
    restored: Mapping[str, Any],
) -> dict[str, Any]:
    return {
        "requiredChecks": list(policy.required_checks),
        "transitKeyHashMatch": source["key"]["sha256"] == restored["key"]["sha256"],
        "auditDeviceHashMatch": source["audit"]["sha256"] == restored["audit"]["sha256"],
        "signerRoleHashMatch": source["roles"]["signer"]["sha256"] == restored["roles"]["signer"]["sha256"],
        "auditorRoleHashMatch": source["roles"]["auditor"]["sha256"] == restored["roles"]["auditor"]["sha256"],
        "snapshotOperatorRoleHashMatch": source["roles"]["snapshotOperator"]["sha256"]
        == restored["roles"]["snapshotOperator"]["sha256"],
    }


def ensure_comparison_passes(validation: Mapping[str, Any]) -> None:
    if all(
        validation.get(key) is True
        for key in (
            "transitKeyHashMatch",
            "auditDeviceHashMatch",
            "signerRoleHashMatch",
            "auditorRoleHashMatch",
            "snapshotOperatorRoleHashMatch",
        )
    ):
        return
    raise ReleaseGateError(
        "release.vault_snapshot_restore_validation_failed",
        "The isolated restore did not reproduce the source Vault signing or AppRole configuration.",
        dict(validation),
    )


def cleanup_restore_context(
    options: GateOptions,
    state_dir: pathlib.Path,
    context: DockerRestoreContext | None,
    *,
    redactor: acceptance.SecretRedactor,
) -> dict[str, Any]:
    cleanup = {
        "snapshotDeleted": False,
        "stateDirectoryRemoved": False,
        "containerRemoved": context is None,
        "exactOwnerCleanup": False,
        "broadCleanupUsed": False,
    }
    errors: list[dict[str, Any]] = []
    snapshot_path = state_dir / DEFAULT_SOURCE_SNAPSHOT_NAME
    if context is not None:
        remove = docker_completed(
            options,
            ["rm", "-f", context.container_name],
            redactor=redactor,
            timeout=min(options.timeout_seconds, 30.0),
            code="release.vault_snapshot_restore_cleanup_failed",
            message="The isolated restore Vault container cleanup could not complete.",
        )
        cleanup["containerRemoved"] = remove.returncode == 0 or "No such container" in remove.stderr
    if snapshot_path.exists():
        try:
            snapshot_path.unlink()
        except OSError:
            errors.append(
                ReleaseGateError(
                    "release.vault_snapshot_restore_cleanup_failed",
                    "The source snapshot file could not be removed.",
                    {"path": str(snapshot_path)},
                ).as_report_error()
            )
        else:
            cleanup["snapshotDeleted"] = True
    else:
        cleanup["snapshotDeleted"] = True
    if state_dir.exists():
        try:
            shutil.rmtree(state_dir)
        except OSError:
            errors.append(
                ReleaseGateError(
                    "release.vault_snapshot_restore_cleanup_failed",
                    "The isolated restore state directory could not be removed.",
                    {"path": str(state_dir)},
                ).as_report_error()
            )
        else:
            cleanup["stateDirectoryRemoved"] = True
    else:
        cleanup["stateDirectoryRemoved"] = True
    cleanup["exactOwnerCleanup"] = (
        cleanup["snapshotDeleted"] and cleanup["stateDirectoryRemoved"] and cleanup["containerRemoved"]
    )
    return {**cleanup, "errors": errors}


def markdown_from_report(report: Mapping[str, Any]) -> str:
    source = report.get("source")
    restore = report.get("restore")
    validation = report.get("validation")
    cleanup = report.get("cleanup")
    lines = [
        "# Vault snapshot restore drill",
        "",
        f"- Schema: `{report.get('schemaVersion', '')}`",
        f"- Run: `{report.get('runId', '')}`",
        f"- Status: **{report.get('status', '')}**",
        f"- Started: `{report.get('startedAt', '')}`",
        f"- Finished: `{report.get('finishedAt', '')}`",
        f"- Duration: `{report.get('durationMs', '')} ms`",
    ]
    if isinstance(source, Mapping):
        credentials = source.get("credentials")
        identity = source.get("identity")
        lines.extend(
            [
                "",
                "## Source",
                "",
                f"- Vault address env: `{credentials.get('vaultAddressEnvironment', '') if isinstance(credentials, Mapping) else ''}`",
                f"- Vault CA env: `{credentials.get('vaultCaCertificateEnvironment', '') if isinstance(credentials, Mapping) else ''}`",
                f"- Snapshot operator role_id env: `{credentials.get('snapshotOperatorRoleIdEnvironment', '') if isinstance(credentials, Mapping) else ''}`",
                f"- Snapshot operator secret_id env: `{credentials.get('snapshotOperatorSecretIdEnvironment', '') if isinstance(credentials, Mapping) else ''}`",
                f"- Restore unseal env hash: `{credentials.get('restoreUnsealEnvironmentSetSha256', '') if isinstance(credentials, Mapping) else ''}`",
                f"- Snapshot SHA256: `{source.get('snapshotSha256', '')}`",
                f"- Snapshot size: `{source.get('snapshotSizeBytes', '')}`",
                f"- Transit key SHA256: `{source.get('transitKeySha256', '')}`",
                f"- Snapshot operator identity hash: `{identity.get('tokenPoliciesSha256', '') if isinstance(identity, Mapping) else ''}`",
            ]
        )
    if isinstance(restore, Mapping):
        lines.extend(
            [
                "",
                "## Restore target",
                "",
                f"- Strategy: `{restore.get('strategy', '')}`",
                f"- Docker image: `{restore.get('dockerImage', '')}`",
                f"- Network mode: `{restore.get('dockerNetworkMode', '')}`",
                f"- Config SHA256: `{restore.get('configSha256', '')}`",
                f"- Restored transit key SHA256: `{restore.get('transitKeySha256', '')}`",
            ]
        )
        unseal_attempts = restore.get("unsealAttempts")
        if isinstance(unseal_attempts, list):
            lines.extend(
                [
                    "",
                    "| Unseal attempt | Sealed | Progress | Threshold |",
                    "| --- | --- | --- | --- |",
                ]
            )
            for item in unseal_attempts:
                if isinstance(item, Mapping):
                    lines.append(
                        f"| `{item.get('attempt', '')}` | `{item.get('sealed', '')}` | `{item.get('progress', '')}` | `{item.get('t', '')}` |"
                    )
    if isinstance(validation, Mapping):
        lines.extend(
            [
                "",
                "## Validation",
                "",
                f"- Transit key hash match: `{validation.get('transitKeyHashMatch', False)}`",
                f"- Signer AppRole hash match: `{validation.get('signerRoleHashMatch', False)}`",
                f"- Auditor AppRole hash match: `{validation.get('auditorRoleHashMatch', False)}`",
                f"- Snapshot operator AppRole hash match: `{validation.get('snapshotOperatorRoleHashMatch', False)}`",
            ]
        )
    if isinstance(cleanup, Mapping):
        lines.extend(
            [
                "",
                "## Cleanup",
                "",
                f"- Snapshot deleted: `{cleanup.get('snapshotDeleted', False)}`",
                f"- State directory removed: `{cleanup.get('stateDirectoryRemoved', False)}`",
                f"- Container removed: `{cleanup.get('containerRemoved', False)}`",
                f"- Exact owner cleanup: `{cleanup.get('exactOwnerCleanup', False)}`",
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


def run_vault_snapshot_restore_drill(
    options: GateOptions,
    *,
    policy_loader: Any = load_operations_policy,
    repository_state_loader: Any = common.repository_state,
) -> int:
    if options.output_dir.exists() and (
        not options.output_dir.is_dir() or any(options.output_dir.iterdir())
    ):
        print("Vault snapshot restore drill output directory must be empty or absent.", file=sys.stderr)
        return 2
    redactor = acceptance.SecretRedactor()
    started_at = utc_now()
    started = time.monotonic()
    run_id = uuid.uuid4().hex
    errors: list[dict[str, Any]] = []
    context: DockerRestoreContext | None = None
    policy: OperationsPolicy | None = None
    snapshot_operator_policy_sha256 = ""
    source_repository: dict[str, Any] | None = None
    source_evidence: dict[str, Any] | None = None
    snapshot_evidence: dict[str, Any] | None = None
    restore_evidence: dict[str, Any] | None = None
    restore_transition: dict[str, Any] | None = None
    validation: dict[str, Any] | None = None
    unseal_attempts: list[dict[str, Any]] | None = None
    state_dir = pathlib.Path(
        tempfile.mkdtemp(prefix=f"synara-vault-restore-{run_id[:12]}-", dir=options.output_dir.parent)
    )
    ensure_private_directory(state_dir)
    cleanup: dict[str, Any] = {
        "snapshotDeleted": False,
        "stateDirectoryRemoved": False,
        "containerRemoved": False,
        "exactOwnerCleanup": False,
        "broadCleanupUsed": False,
    }
    try:
        source_repository = dict(repository_state_loader(options.repo_root))
        policy = policy_loader(options.operations_policy_path)
        _snapshot_operator_policy_text, snapshot_operator_policy_sha256 = load_snapshot_operator_policy(
            options.snapshot_operator_policy_path
        )
        secret_inputs = prepare_secret_inputs(policy, options, state_dir=state_dir, redactor=redactor)
        source_token, source_login = approle_login(
            options,
            environment=secret_inputs.source_vault_environment,
            role_id=secret_inputs.source_role_id,
            secret_id=secret_inputs.source_secret_id,
            redactor=redactor,
        )
        source_environment = {**secret_inputs.source_vault_environment, "VAULT_TOKEN": source_token}
        source_evidence = {
            **capture_source_state(
                policy,
                options,
                source_environment=source_environment,
                redactor=redactor,
            ),
            "login": approle_login_summary(source_login),
        }
        snapshot_path = state_dir / DEFAULT_SOURCE_SNAPSHOT_NAME
        snapshot_evidence = capture_source_snapshot(
            options,
            source_environment=source_environment,
            snapshot_path=snapshot_path,
            redactor=redactor,
        )
        context = start_restore_container(
            policy,
            options,
            run_id=run_id,
            state_dir=state_dir,
            redactor=redactor,
        )
        restore_transition = restore_snapshot_into_isolated_vault(
            policy,
            options,
            context,
            redactor=redactor,
        )
        unseal_attempts = unseal_restored_snapshot(
            options,
            context,
            secret_inputs=secret_inputs,
            redactor=redactor,
        )
        restore_evidence = {
            **capture_restored_state(
                policy,
                options,
                context,
                restored_token=source_token,
                redactor=redactor,
            ),
            **restore_transition,
            "unsealAttempts": unseal_attempts,
            "configSha256": file_sha256(context.config_path),
        }
        validation = compare_source_and_restore(
            policy,
            source=source_evidence,
            restored=restore_evidence,
        )
        ensure_comparison_passes(validation)
    except ReleaseGateError as error:
        errors.append(error.as_report_error())
    finally:
        cleanup_result = cleanup_restore_context(options, state_dir, context, redactor=redactor)
        cleanup.update({key: value for key, value in cleanup_result.items() if key != "errors"})
        errors.extend(cleanup_result.get("errors", []))
    finished_at = utc_now()
    status = "pass" if not errors else "fail"
    report = {
        "schemaVersion": SCHEMA_VERSION,
        "runId": run_id,
        "status": status,
        "startedAt": started_at.isoformat(),
        "finishedAt": finished_at.isoformat(),
        "durationMs": round((time.monotonic() - started) * 1000),
        "policy": {
            "operationsPolicyPath": str(options.operations_policy_path.relative_to(options.repo_root)),
            "operationsPolicySha256": policy.sha256 if policy is not None else None,
            "snapshotOperatorPolicyPath": str(options.snapshot_operator_policy_path.relative_to(options.repo_root)),
            "snapshotOperatorPolicySha256": snapshot_operator_policy_sha256,
        },
        "source": (
            {
                **(source_repository or {}),
                "credentials": {
                    "vaultAddressEnvironment": policy.source_vault_addr_env,
                    "vaultCaCertificateEnvironment": policy.source_vault_cacert_env,
                    "snapshotOperatorRoleIdEnvironment": policy.source_role_id_env,
                    "snapshotOperatorSecretIdEnvironment": policy.source_secret_id_env,
                    "restoreUnsealKeyEnvironments": list(policy.source_unseal_key_envs),
                    "restoreUnsealEnvironmentSetSha256": sha256_text("\n".join(policy.source_unseal_key_envs)),
                },
                "identity": source_evidence.get("identity") if isinstance(source_evidence, Mapping) else None,
                "login": source_evidence.get("login") if isinstance(source_evidence, Mapping) else None,
                "status": source_evidence.get("status") if isinstance(source_evidence, Mapping) else None,
                "raft": source_evidence.get("raft") if isinstance(source_evidence, Mapping) else None,
                "audit": source_evidence.get("audit") if isinstance(source_evidence, Mapping) else None,
                "transitKeySha256": (
                    source_evidence.get("key", {}).get("sha256")
                    if isinstance(source_evidence, Mapping) and isinstance(source_evidence.get("key"), Mapping)
                    else None
                ),
                "roles": source_evidence.get("roles") if isinstance(source_evidence, Mapping) else None,
                "snapshotSha256": snapshot_evidence.get("sha256") if isinstance(snapshot_evidence, Mapping) else None,
                "snapshotSizeBytes": snapshot_evidence.get("sizeBytes") if isinstance(snapshot_evidence, Mapping) else None,
            }
            if policy is not None
            else None
        ),
        "restore": (
            {
                "strategy": "isolated-docker-raft",
                "dockerImage": policy.restore_docker_image,
                "dockerNetworkMode": policy.restore_docker_network_mode,
                "restoreTargetConfig": {
                    "forbidRetryJoin": policy.restore_forbid_retry_join,
                    "forbidSourceClusterRestore": policy.restore_forbid_source_cluster_restore,
                    "requireFreshStateDir": policy.restore_require_fresh_state_dir,
                    "auditTmpfsSha256": sha256_text(policy.restore_audit_tmpfs),
                },
                "configSha256": restore_evidence.get("configSha256") if isinstance(restore_evidence, Mapping) else None,
                "status": restore_evidence.get("status") if isinstance(restore_evidence, Mapping) else None,
                "raft": restore_evidence.get("raft") if isinstance(restore_evidence, Mapping) else None,
                "audit": restore_evidence.get("audit") if isinstance(restore_evidence, Mapping) else None,
                "auditFiles": restore_evidence.get("auditFiles") if isinstance(restore_evidence, Mapping) else None,
                "transitKeySha256": (
                    restore_evidence.get("key", {}).get("sha256")
                    if isinstance(restore_evidence, Mapping) and isinstance(restore_evidence.get("key"), Mapping)
                    else None
                ),
                "roles": restore_evidence.get("roles") if isinstance(restore_evidence, Mapping) else None,
                "identity": restore_evidence.get("identity") if isinstance(restore_evidence, Mapping) else None,
                "sealedStatusSha256": (
                    restore_evidence.get("sealedStatusSha256")
                    if isinstance(restore_evidence, Mapping)
                    else restore_transition.get("sealedStatusSha256")
                    if isinstance(restore_transition, Mapping)
                    else None
                ),
                "snapshotApplication": (
                    restore_evidence.get("snapshotApplication")
                    if isinstance(restore_evidence, Mapping)
                    else restore_transition.get("snapshotApplication")
                    if isinstance(restore_transition, Mapping)
                    else None
                ),
                "unsealAttempts": (
                    restore_evidence.get("unsealAttempts")
                    if isinstance(restore_evidence, Mapping)
                    else unseal_attempts
                ),
            }
            if policy is not None
            else None
        ),
        "validation": validation,
        "cleanup": cleanup,
        "errors": errors,
    }
    json_path, markdown_path = write_report(report, options.output_dir, redactor)
    output_scan = acceptance.scan_output_secrets(options.output_dir, redactor)
    report["security"] = {"outputSecretScan": output_scan}
    if output_scan.get("findings"):
        report["status"] = "fail"
        report["errors"].append(
            {
                "code": "release.vault_snapshot_restore_output_secret_scan_failed",
                "message": "The vault snapshot restore drill output retained secret-like findings.",
                "evidence": {"findingCount": len(output_scan["findings"])},
            }
        )
    write_report(report, options.output_dir, redactor)
    print(f"Vault snapshot restore drill: {report['status']}")
    print(f"JSON: {json_path}")
    print(f"Markdown: {markdown_path}")
    return 0 if report["status"] == "pass" else 1


def main(argv: Sequence[str] | None = None) -> int:
    return run_vault_snapshot_restore_drill(parse_args(argv))


if __name__ == "__main__":
    raise SystemExit(main())
