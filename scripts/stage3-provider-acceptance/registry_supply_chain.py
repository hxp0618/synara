from __future__ import annotations

import base64
import binascii
import dataclasses
import datetime as dt
import hashlib
import http.client
import json
import os
import pathlib
import re
import secrets
import ssl
import subprocess
import time
import urllib.parse
from collections.abc import Mapping, Sequence
from typing import Any

import acceptance_runner as acceptance
import controlled_remote_release_gate as remote
import release_gate_common as common


TOOLS_LOCK_PATH = pathlib.Path("deploy/worker/supply-chain-tools.lock")
SIGNING_POLICY_PATH = pathlib.Path("deploy/worker/signing-policy.json")
PRODUCTION_SIGNING_POLICY_PATH = pathlib.Path("deploy/worker/production-signing-policy.json")
PRODUCTION_SIGNING_PROFILE_PATH = pathlib.Path("deploy/worker/production-signing-profile.json")
VULNERABILITY_POLICY_PATH = pathlib.Path("deploy/worker/vulnerability-policy.json")
SECURITY_NAMESPACE_KUSTOMIZATION_PATH = pathlib.Path("deploy/kubernetes/security/namespaced")
SECURITY_CLUSTER_KUSTOMIZATION_PATH = pathlib.Path("deploy/kubernetes/security/cluster")
SECURITY_CLUSTER_POLICY_PATH = pathlib.Path(
    "deploy/kubernetes/security/cluster/verify-synara-worker-images.yaml"
)
SECURITY_PUBLIC_KEY_CONFIGMAP_PATH = pathlib.Path(
    "deploy/kubernetes/security/namespaced/synara-worker-cosign-public-key-configmap.yaml"
)
SECURITY_REPOSITORY_CONFIGMAP_PATH = pathlib.Path(
    "deploy/kubernetes/security/namespaced/synara-worker-signing-settings-configmap.yaml"
)
SECURITY_PRODUCTION_KUSTOMIZATION_PATH = pathlib.Path(
    "deploy/kubernetes/security/production"
)
PRODUCTION_REGISTRY_RETENTION_POLICY_PATH = pathlib.Path(
    "deploy/kubernetes/security/registry/retention-policy.json"
)
SECURITY_CLUSTER_POLICY_NAME = "verify-synara-worker-images"
SECURITY_PUBLIC_KEY_CONTEXT_NAME = "workerPublicKey"
SECURITY_IMAGE_REFERENCE_FIELD_PATH = "spec.rules.0.verifyImages.0.imageReferences.0"
SECURITY_IMAGE_REFERENCE_PLACEHOLDER = "registry.invalid/synara/worker*"
PRODUCTION_RELEASE_SOURCE_PATHS = (
    PRODUCTION_SIGNING_POLICY_PATH,
    PRODUCTION_SIGNING_PROFILE_PATH,
    SECURITY_PRODUCTION_KUSTOMIZATION_PATH / "kustomization.yaml",
    SECURITY_CLUSTER_KUSTOMIZATION_PATH / "kustomization.yaml",
    SECURITY_CLUSTER_POLICY_PATH,
    SECURITY_NAMESPACE_KUSTOMIZATION_PATH / "kustomization.yaml",
    SECURITY_PUBLIC_KEY_CONFIGMAP_PATH,
    SECURITY_REPOSITORY_CONFIGMAP_PATH,
    PRODUCTION_REGISTRY_RETENTION_POLICY_PATH,
)
REQUIRED_TOOLS = ("cosign", "trivy")
SUPPORTED_SIGNING_MODES = ("ephemeral-key", "keyless", "kms-key")
SUPPORTED_SIGNING_POLICY_PROFILES = ("disposable", "production")
SUPPORTED_SEVERITIES = ("UNKNOWN", "LOW", "MEDIUM", "HIGH", "CRITICAL")
TRIVY_DATABASE_DOWNLOAD_RETRY_DELAY_SECONDS = 1.0
TRIVY_DATABASE_DOWNLOAD_RETRY_MARKERS = (
    "unexpected eof",
    "connection reset by peer",
    "i/o timeout",
    "tls handshake timeout",
    "temporary failure in name resolution",
    "502 bad gateway",
    "503 service unavailable",
)
COSIGN_CLAIM_TYPE = "https://sigstore.dev/cosign/sign/v1"
COSIGN_KYVERNO_COMPATIBILITY_ARGUMENT = "--new-bundle-format=false"
IMMUTABLE_IMAGE_PATTERN = re.compile(
    r"[A-Za-z0-9][A-Za-z0-9._:-]*(?:/[A-Za-z0-9][A-Za-z0-9._:-]*)+"
    r"@sha256:[0-9a-f]{64}"
)
VULNERABILITY_ID_PATTERN = re.compile(
    r"(?:CVE-[0-9]{4}-[0-9]{4,}|GHSA-[0-9a-z]{4}-[0-9a-z]{4}-[0-9a-z]{4})",
    re.IGNORECASE,
)
ENVIRONMENT_NAME_PATTERN = re.compile(r"[A-Z][A-Z0-9_]{0,127}")
KMS_KEY_REFERENCE_PATTERN = re.compile(
    r"(?:awskms|gcpkms|azurekms|hashivault)://[^\s@?#]{1,2048}"
)
KUBERNETES_NAME_PATTERN = re.compile(r"[a-z0-9](?:[-a-z0-9]*[a-z0-9])?")
CONFIGMAP_KEY_PATTERN = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,127}")
REGISTRY_HOST_PATTERN = re.compile(
    r"[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?(?::([0-9]{1,5}))?"
)
REPOSITORY_PATH_COMPONENT_PATTERN = re.compile(r"[a-z0-9]+(?:[._-][a-z0-9]+)*")
VAULT_TRANSIT_KEY_REFERENCE = "hashivault://synara-worker-release"
VAULT_TRANSIT_REQUIRED_ENVIRONMENT = ("VAULT_ADDR", "VAULT_TOKEN", "VAULT_CACERT")
VAULT_TRANSIT_OPTIONAL_ENVIRONMENT = ("TRANSIT_SECRET_ENGINE_PATH",)
MATERIALIZED_VAULT_CACERT_RELATIVE_PATH = pathlib.Path("vault/ca-certificates/vault-ca.crt")
MATERIALIZED_KMS_PUBLIC_KEY_RELATIVE_PATH = pathlib.Path("cosign/production-kms.pub")
VAULT_TRANSIT_AUTH_METHOD = "approle"
VAULT_TRANSIT_PRINCIPAL = "auth/approle/role/synara-worker-release-signer"
VAULT_TRANSIT_AUDIT_REQUEST_PATH = "transit/sign/synara-worker-release"
VAULT_TOKEN_LOOKUP_PATH = "/v1/auth/token/lookup-self"
VAULT_TOKEN_LOOKUP_MAX_RESPONSE_BYTES = 64 << 10
VAULT_TOKEN_LOOKUP_TIMEOUT_SECONDS = 20.0
PRODUCTION_REGISTRY_USERNAME_ENV = "REGISTRY_USERNAME"
PRODUCTION_REGISTRY_PASSWORD_ENV = "REGISTRY_PASSWORD"
PRODUCTION_REGISTRY_CA_CERT_ENV = "REGISTRY_CA_CERT"
PRODUCTION_REGISTRY_ACCESS_ENVIRONMENT = (
    PRODUCTION_REGISTRY_USERNAME_ENV,
    PRODUCTION_REGISTRY_PASSWORD_ENV,
    PRODUCTION_REGISTRY_CA_CERT_ENV,
)
TRANSPARENCY_LOG_PROVIDER = "public-rekor"
TRANSPARENCY_LOG_URL = "https://rekor.sigstore.dev"
ADMISSION_FAILURE_POLICY = "Fail"
ADMISSION_VALIDATION_FAILURE_ACTION = "Enforce"
PEM_PUBLIC_KEY_PATTERN = re.compile(
    r"-----BEGIN PUBLIC KEY-----\n.+?\n-----END PUBLIC KEY-----\n?",
    re.DOTALL,
)
PLACEHOLDER_PUBLIC_KEY_MARKER = "REPLACE_WITH_COSIGN_PUBLIC_KEY_PEM"
PLACEHOLDER_REPOSITORY_PATTERN = "registry.example.com/synara/worker*"


@dataclasses.dataclass(frozen=True)
class ToolImages:
    cosign: str
    trivy: str
    lock_sha256: str

    def as_report(self) -> dict[str, str]:
        return {
            "cosign": self.cosign,
            "trivy": self.trivy,
            "lockSha256": self.lock_sha256,
        }


@dataclasses.dataclass(frozen=True)
class SigningPolicy:
    path: str
    mode: str
    require_transparency_log: bool
    key_reference: str | None
    credential_environment: tuple[str, ...]
    identity_token_environment: str | None
    certificate_identity: str | None
    certificate_identity_regexp: str | None
    certificate_oidc_issuer: str | None
    certificate_oidc_issuer_regexp: str | None
    sha256: str

    @property
    def production_policy(self) -> bool:
        return self.mode in {"keyless", "kms-key"}

    def as_report(self) -> dict[str, Any]:
        report: dict[str, Any] = {
            "path": self.path,
            "sha256": self.sha256,
            "mode": self.mode,
            "requireTransparencyLog": self.require_transparency_log,
            "productionPolicy": self.production_policy,
        }
        if self.key_reference is not None:
            report["keyReference"] = self.key_reference
            report["credentialEnvironmentCount"] = len(self.credential_environment)
        if self.certificate_identity is not None:
            report["certificateIdentity"] = self.certificate_identity
        if self.certificate_identity_regexp is not None:
            report["certificateIdentityRegexp"] = self.certificate_identity_regexp
        if self.certificate_oidc_issuer is not None:
            report["certificateOidcIssuer"] = self.certificate_oidc_issuer
        if self.certificate_oidc_issuer_regexp is not None:
            report["certificateOidcIssuerRegexp"] = self.certificate_oidc_issuer_regexp
        return report


@dataclasses.dataclass(frozen=True)
class ProductionSigningProfile:
    path: str
    signing_policy_path: str
    signer_type: str
    key_reference: str
    auth_method: str
    principal: str
    audit_request_path: str
    credential_environment: tuple[str, ...]
    registry_username_environment: str
    registry_password_environment: str
    registry_ca_cert_environment: str
    transparency_log_provider: str
    transparency_log_url: str
    transparency_log_upload: bool
    transparency_log_required: bool
    transparency_log_verify: bool
    transparency_log_inclusion_proof_required: bool
    transparency_log_signed_entry_timestamp_required: bool
    admission_provider: str
    admission_required: bool
    admission_failure_policy: str
    admission_validation_failure_action: str
    admission_mutate_digest: bool
    admission_verify_digest: bool
    public_key_configmap_namespace: str
    public_key_configmap_name: str
    public_key_configmap_key: str
    repository_configmap_namespace: str
    repository_configmap_name: str
    repository_configmap_key: str
    cluster_policy_path: str
    cluster_kustomization_path: str
    namespace_kustomization_path: str
    sha256: str

    def as_report(self) -> dict[str, Any]:
        return {
            "path": self.path,
            "sha256": self.sha256,
            "signingPolicyPath": self.signing_policy_path,
            "signer": {
                "type": self.signer_type,
                "keyReference": self.key_reference,
                "authMethod": self.auth_method,
                "principal": self.principal,
                "auditRequestPath": self.audit_request_path,
                "credentialEnvironment": list(self.credential_environment),
                "credentialEnvironmentCount": len(self.credential_environment),
            },
            "registryAccess": {
                "usernameEnvironment": self.registry_username_environment,
                "passwordEnvironment": self.registry_password_environment,
                "caCertEnvironment": self.registry_ca_cert_environment,
            },
            "transparencyLog": {
                "provider": self.transparency_log_provider,
                "url": self.transparency_log_url,
                "upload": self.transparency_log_upload,
                "required": self.transparency_log_required,
                "verify": self.transparency_log_verify,
                "inclusionProofRequired": self.transparency_log_inclusion_proof_required,
                "signedEntryTimestampRequired": self.transparency_log_signed_entry_timestamp_required,
            },
            "admission": {
                "provider": self.admission_provider,
                "required": self.admission_required,
                "failurePolicy": self.admission_failure_policy,
                "validationFailureAction": self.admission_validation_failure_action,
                "mutateDigest": self.admission_mutate_digest,
                "verifyDigest": self.admission_verify_digest,
                "publicKeyConfigMap": {
                    "namespace": self.public_key_configmap_namespace,
                    "name": self.public_key_configmap_name,
                    "key": self.public_key_configmap_key,
                },
                "repositoryConfigMap": {
                    "namespace": self.repository_configmap_namespace,
                    "name": self.repository_configmap_name,
                    "key": self.repository_configmap_key,
                },
                "clusterPolicyPath": self.cluster_policy_path,
                "clusterKustomizationPath": self.cluster_kustomization_path,
                "namespaceKustomizationPath": self.namespace_kustomization_path,
            },
        }


@dataclasses.dataclass(frozen=True)
class VulnerabilityException:
    vulnerability_id: str
    package: str
    platform: str
    expires_at: dt.datetime
    owner: str
    reason: str

    @property
    def identity(self) -> tuple[str, str, str]:
        return (self.vulnerability_id, self.package, self.platform)


@dataclasses.dataclass(frozen=True)
class VulnerabilityPolicy:
    blocked_severities: tuple[str, ...]
    ignore_unfixed: bool
    fail_on_end_of_life_os: bool
    maximum_database_age_hours: int
    exceptions: tuple[VulnerabilityException, ...]
    sha256: str

    def as_report(self) -> dict[str, Any]:
        return {
            "path": str(VULNERABILITY_POLICY_PATH),
            "sha256": self.sha256,
            "blockedSeverities": list(self.blocked_severities),
            "ignoreUnfixed": self.ignore_unfixed,
            "failOnEndOfLifeOS": self.fail_on_end_of_life_os,
            "maximumDatabaseAgeHours": self.maximum_database_age_hours,
            "exceptionCount": len(self.exceptions),
        }


@dataclasses.dataclass(frozen=True)
class SupplyChainConfiguration:
    tools: ToolImages
    signing_policy_profile: str
    signing_policy: SigningPolicy
    production_signing_profile: ProductionSigningProfile | None
    vulnerability_policy: VulnerabilityPolicy

    def source_evidence(self) -> dict[str, Any]:
        report: dict[str, Any] = {
            "tools": self.tools.as_report(),
            "signingPolicyProfile": self.signing_policy_profile,
            "signingPolicy": self.signing_policy.as_report(),
            "vulnerabilityPolicy": self.vulnerability_policy.as_report(),
        }
        if self.production_signing_profile is not None:
            report["productionSigningProfile"] = self.production_signing_profile.as_report()
        return report


@dataclasses.dataclass(frozen=True)
class SupplyChainOptions:
    repo_root: pathlib.Path
    state_dir: pathlib.Path
    image_repository: str
    docker_bin: str
    timeout_seconds: float
    insecure_registry: bool
    registry_auth_username_environment: str | None = None
    registry_auth_password_environment: str | None = None
    registry_ca_cert_environment: str | None = None
    production_public_key_configmap_path: pathlib.Path | None = None
    production_repository_configmap_path: pathlib.Path | None = None


@dataclasses.dataclass(frozen=True)
class PreparedKmsEnvironment:
    secret_environment: dict[str, str]
    vault_ca_materialized: bool


@dataclasses.dataclass(frozen=True)
class PreparedRegistryAccess:
    environment: dict[str, str]
    host_environment: dict[str, str]
    registry_host: str
    auth_configured: bool
    ca_materialized: bool
    registry_ca_container_path: str | None


@dataclasses.dataclass(frozen=True)
class ParsedConfigMapManifest:
    name: str
    namespace: str
    data: dict[str, str]


def _sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def checked_in_source_hashes(
    repo_root: pathlib.Path,
    paths: Sequence[pathlib.Path],
) -> dict[str, str]:
    return {
        str(relative_path): _sha256((repo_root / relative_path).read_bytes())
        for relative_path in paths
    }


def _normalized_manifest_lines(text: str) -> tuple[str, ...]:
    return tuple(
        line.rstrip()
        for line in text.replace("\r\n", "\n").replace("\r", "\n").split("\n")
        if line.strip() and not line.lstrip().startswith("#")
    )


def _require_normalized_manifest_lines(
    text: str,
    expected: Sequence[str],
    *,
    code: str,
    message: str,
    path: pathlib.Path,
) -> None:
    if _normalized_manifest_lines(text) != tuple(expected):
        raise common.ReleaseGateError(code, message, {"path": str(path)})


def _parse_der_tlv(data: bytes, offset: int) -> tuple[int, int, int]:
    if offset >= len(data):
        raise ValueError("missing tlv")
    tag = data[offset]
    offset += 1
    if offset >= len(data):
        raise ValueError("missing length")
    first_length_byte = data[offset]
    offset += 1
    if first_length_byte & 0x80 == 0:
        length = first_length_byte
    else:
        length_length = first_length_byte & 0x7F
        if length_length == 0 or length_length > 4 or offset + length_length > len(data):
            raise ValueError("invalid length")
        length = int.from_bytes(data[offset : offset + length_length], "big")
        offset += length_length
    end = offset + length
    if end > len(data):
        raise ValueError("truncated value")
    return tag, offset, end


def _validate_subject_public_key_info_der(der: bytes) -> None:
    outer_tag, outer_start, outer_end = _parse_der_tlv(der, 0)
    if outer_tag != 0x30 or outer_start >= outer_end or outer_end != len(der):
        raise ValueError("invalid spki sequence")
    algorithm_tag, algorithm_start, algorithm_end = _parse_der_tlv(der, outer_start)
    if algorithm_tag != 0x30 or algorithm_start >= algorithm_end:
        raise ValueError("invalid algorithm identifier")
    object_identifier_tag, _, object_identifier_end = _parse_der_tlv(der, algorithm_start)
    if object_identifier_tag != 0x06 or object_identifier_end > algorithm_end:
        raise ValueError("invalid algorithm object identifier")
    bit_string_tag, bit_string_start, bit_string_end = _parse_der_tlv(der, algorithm_end)
    if bit_string_tag != 0x03 or bit_string_end != outer_end:
        raise ValueError("invalid subject public key")
    bit_string = der[bit_string_start:bit_string_end]
    if len(bit_string) < 33 or bit_string[0] != 0:
        raise ValueError("invalid subject public key bit string")


def _parse_timestamp(value: Any, *, field: str) -> dt.datetime:
    if not isinstance(value, str) or not value.strip():
        raise common.ReleaseGateError(
            "release.registry_supply_chain_policy_invalid",
            "Worker Registry vulnerability policy contained an invalid timestamp.",
            {"field": field},
        )
    normalized = value.strip().replace("Z", "+00:00")
    try:
        parsed = dt.datetime.fromisoformat(normalized)
    except ValueError:
        raise common.ReleaseGateError(
            "release.registry_supply_chain_policy_invalid",
            "Worker Registry vulnerability policy contained an invalid timestamp.",
            {"field": field},
        ) from None
    if parsed.tzinfo is None:
        raise common.ReleaseGateError(
            "release.registry_supply_chain_policy_invalid",
            "Worker Registry vulnerability policy timestamps must include a timezone.",
            {"field": field},
        )
    return parsed.astimezone(dt.timezone.utc)


def _read_bytes(repo_root: pathlib.Path, relative_path: pathlib.Path, *, label: str) -> bytes:
    try:
        return (repo_root / relative_path).read_bytes()
    except OSError:
        raise common.ReleaseGateError(
            "release.registry_supply_chain_source_invalid",
            f"Worker Registry gate could not read the checked-in {label}.",
        ) from None


def load_tool_images(repo_root: pathlib.Path) -> ToolImages:
    raw = _read_bytes(repo_root, TOOLS_LOCK_PATH, label="supply-chain tool lock")
    values: dict[str, str] = {}
    try:
        text = raw.decode("utf-8")
    except UnicodeDecodeError:
        raise common.ReleaseGateError(
            "release.registry_supply_chain_source_invalid",
            "Worker Registry supply-chain tool lock was not UTF-8.",
        ) from None
    for line_number, raw_line in enumerate(text.splitlines(), start=1):
        line = raw_line.strip()
        if not line:
            continue
        if line.count("=") != 1:
            raise common.ReleaseGateError(
                "release.registry_supply_chain_source_invalid",
                "Worker Registry supply-chain tool lock contained a malformed entry.",
                {"line": line_number},
            )
        name, reference = (part.strip() for part in line.split("=", 1))
        if name not in REQUIRED_TOOLS or name in values:
            raise common.ReleaseGateError(
                "release.registry_supply_chain_source_invalid",
                "Worker Registry supply-chain tool lock contained an unknown or duplicate tool.",
                {"line": line_number, "tool": name},
            )
        if (
            IMMUTABLE_IMAGE_PATTERN.fullmatch(reference) is None
            or any(character.isspace() or ord(character) < 32 for character in reference)
            or "://" in reference
        ):
            raise common.ReleaseGateError(
                "release.registry_supply_chain_source_invalid",
                "Worker Registry supply-chain tools must use credential-free digest-pinned image references.",
                {"line": line_number, "tool": name},
            )
        values[name] = reference
    if set(values) != set(REQUIRED_TOOLS):
        raise common.ReleaseGateError(
            "release.registry_supply_chain_source_invalid",
            "Worker Registry supply-chain tool lock did not contain every required tool.",
            {"requiredTools": list(REQUIRED_TOOLS), "foundTools": sorted(values)},
        )
    return ToolImages(
        cosign=values["cosign"],
        trivy=values["trivy"],
        lock_sha256=_sha256(raw),
    )


def _optional_policy_text(value: Any, *, field: str, maximum_length: int = 2048) -> str | None:
    if value is None:
        return None
    if (
        not isinstance(value, str)
        or not value.strip()
        or len(value) > maximum_length
        or any(ord(character) < 32 for character in value)
    ):
        raise common.ReleaseGateError(
            "release.registry_signing_policy_invalid",
            "Worker Registry signing policy contained an invalid text value.",
            {"field": field},
        )
    return value.strip()


def _validate_policy_regexp(value: str | None, *, field: str) -> None:
    if value is None:
        return
    if (
        not value.startswith("^")
        or not value.endswith("$")
        or re.search(r"\(\?(?:[=!]|<[=!]|P<)|\\[1-9]", value) is not None
    ):
        raise common.ReleaseGateError(
            "release.registry_signing_policy_invalid",
            "Worker Registry signing policy used an unanchored or unsupported Cosign RE2 regexp.",
            {"field": field},
        )
    try:
        re.compile(value)
    except re.error:
        raise common.ReleaseGateError(
            "release.registry_signing_policy_invalid",
            "Worker Registry signing policy contained an invalid regexp.",
            {"field": field},
        ) from None


def _validate_exact_issuer(value: str | None) -> None:
    if value is None:
        return
    try:
        parsed = urllib.parse.urlsplit(value)
        hostname = parsed.hostname
    except ValueError:
        parsed = None
        hostname = None
    if (
        parsed is None
        or parsed.scheme != "https"
        or not hostname
        or parsed.username is not None
        or parsed.password is not None
        or parsed.query
        or parsed.fragment
    ):
        raise common.ReleaseGateError(
            "release.registry_signing_policy_invalid",
            "Worker Registry keyless OIDC issuer must be a credential-free HTTPS URL.",
            {"field": "certificateOidcIssuer"},
        )


def signing_policy_path(profile: str) -> pathlib.Path:
    if profile == "disposable":
        return SIGNING_POLICY_PATH
    if profile == "production":
        return PRODUCTION_SIGNING_POLICY_PATH
    raise ValueError(f"unsupported signing policy profile: {profile}")


def _normalize_relative_repo_path(
    value: Any,
    *,
    field: str,
    code: str,
    message: str,
    directory: bool = False,
) -> tuple[str, pathlib.Path]:
    text = _optional_policy_text(value, field=field)
    if text is None:
        raise common.ReleaseGateError(code, message, {"field": field})
    pure_path = pathlib.PurePosixPath(text)
    if (
        pure_path.is_absolute()
        or ".." in pure_path.parts
        or any(part in {"", "."} for part in pure_path.parts)
    ):
        raise common.ReleaseGateError(code, message, {"field": field})
    resolved = pathlib.Path(*pure_path.parts)
    return text, resolved


def _validated_relative_repo_path(
    repo_root: pathlib.Path,
    value: Any,
    *,
    field: str,
    code: str,
    message: str,
    directory: bool = False,
) -> tuple[str, pathlib.Path]:
    text, relative_path = _normalize_relative_repo_path(
        value,
        field=field,
        code=code,
        message=message,
        directory=directory,
    )
    absolute_path = repo_root / relative_path
    valid = absolute_path.is_dir() if directory else absolute_path.is_file()
    if not valid:
        raise common.ReleaseGateError(code, message, {"field": field})
    return text, relative_path


def _validate_kubernetes_name(
    value: Any,
    *,
    field: str,
    code: str,
    message: str,
) -> str:
    text = _optional_policy_text(value, field=field, maximum_length=128)
    if text is None or KUBERNETES_NAME_PATTERN.fullmatch(text) is None:
        raise common.ReleaseGateError(code, message, {"field": field})
    return text


def _validate_configmap_key(
    value: Any,
    *,
    field: str,
    code: str,
    message: str,
) -> str:
    text = _optional_policy_text(value, field=field, maximum_length=128)
    if text is None or CONFIGMAP_KEY_PATTERN.fullmatch(text) is None:
        raise common.ReleaseGateError(code, message, {"field": field})
    return text


def load_signing_policy(
    repo_root: pathlib.Path,
    *,
    policy_path: pathlib.Path = SIGNING_POLICY_PATH,
) -> SigningPolicy:
    raw = _read_bytes(repo_root, policy_path, label="signing policy")
    try:
        payload = json.loads(raw)
    except (UnicodeDecodeError, json.JSONDecodeError):
        raise common.ReleaseGateError(
            "release.registry_signing_policy_invalid",
            "Worker Registry signing policy was not valid JSON.",
        ) from None
    expected_keys = {
        "schemaVersion",
        "mode",
        "requireTransparencyLog",
        "keyReference",
        "credentialEnvironment",
        "identityTokenEnvironment",
        "certificateIdentity",
        "certificateIdentityRegexp",
        "certificateOidcIssuer",
        "certificateOidcIssuerRegexp",
    }
    if not isinstance(payload, dict) or set(payload) != expected_keys or payload.get("schemaVersion") != 1:
        raise common.ReleaseGateError(
            "release.registry_signing_policy_invalid",
            "Worker Registry signing policy schema was invalid.",
        )
    mode = payload.get("mode")
    require_tlog = payload.get("requireTransparencyLog")
    raw_environment = payload.get("credentialEnvironment")
    if (
        mode not in SUPPORTED_SIGNING_MODES
        or not isinstance(require_tlog, bool)
        or not isinstance(raw_environment, list)
        or not all(
            isinstance(name, str) and ENVIRONMENT_NAME_PATTERN.fullmatch(name) is not None
            for name in raw_environment
        )
        or len(set(raw_environment)) != len(raw_environment)
    ):
        raise common.ReleaseGateError(
            "release.registry_signing_policy_invalid",
            "Worker Registry signing policy values were invalid.",
        )
    key_reference = _optional_policy_text(payload.get("keyReference"), field="keyReference")
    token_environment = _optional_policy_text(
        payload.get("identityTokenEnvironment"),
        field="identityTokenEnvironment",
        maximum_length=128,
    )
    certificate_identity = _optional_policy_text(
        payload.get("certificateIdentity"), field="certificateIdentity"
    )
    certificate_identity_regexp = _optional_policy_text(
        payload.get("certificateIdentityRegexp"), field="certificateIdentityRegexp"
    )
    certificate_issuer = _optional_policy_text(
        payload.get("certificateOidcIssuer"), field="certificateOidcIssuer"
    )
    certificate_issuer_regexp = _optional_policy_text(
        payload.get("certificateOidcIssuerRegexp"), field="certificateOidcIssuerRegexp"
    )
    _validate_policy_regexp(certificate_identity_regexp, field="certificateIdentityRegexp")
    _validate_policy_regexp(certificate_issuer_regexp, field="certificateOidcIssuerRegexp")
    _validate_exact_issuer(certificate_issuer)

    if mode == "ephemeral-key":
        valid = (
            not require_tlog
            and key_reference is None
            and not raw_environment
            and token_environment is None
            and certificate_identity is None
            and certificate_identity_regexp is None
            and certificate_issuer is None
            and certificate_issuer_regexp is None
        )
    elif mode == "kms-key":
        valid = (
            require_tlog
            and key_reference is not None
            and KMS_KEY_REFERENCE_PATTERN.fullmatch(key_reference) is not None
            and token_environment is None
            and certificate_identity is None
            and certificate_identity_regexp is None
            and certificate_issuer is None
            and certificate_issuer_regexp is None
        )
    else:
        valid = (
            require_tlog
            and key_reference is None
            and not raw_environment
            and token_environment is not None
            and ENVIRONMENT_NAME_PATTERN.fullmatch(token_environment) is not None
            and "TOKEN" in token_environment
            and (certificate_identity is None) != (certificate_identity_regexp is None)
            and (certificate_issuer is None) != (certificate_issuer_regexp is None)
            and (
                certificate_issuer_regexp is None
                or certificate_issuer_regexp.startswith("^https://")
            )
        )
    if not valid:
        raise common.ReleaseGateError(
            "release.registry_signing_policy_invalid",
            "Worker Registry signing policy fields did not match its selected mode.",
            {"mode": mode},
        )
    return SigningPolicy(
        path=str(policy_path),
        mode=str(mode),
        require_transparency_log=require_tlog,
        key_reference=key_reference,
        credential_environment=tuple(raw_environment),
        identity_token_environment=token_environment,
        certificate_identity=certificate_identity,
        certificate_identity_regexp=certificate_identity_regexp,
        certificate_oidc_issuer=certificate_issuer,
        certificate_oidc_issuer_regexp=certificate_issuer_regexp,
        sha256=_sha256(raw),
    )


def _expected_cluster_policy_lines(profile: ProductionSigningProfile) -> tuple[str, ...]:
    return (
        "apiVersion: kyverno.io/v1",
        "kind: ClusterPolicy",
        "metadata:",
        f"  name: {SECURITY_CLUSTER_POLICY_NAME}",
        "  annotations:",
        "    pod-policies.kyverno.io/title: Verify Synara Worker Images",
        "    pod-policies.kyverno.io/category: Supply Chain Security",
        "    pod-policies.kyverno.io/autogen-controllers: DaemonSet,Deployment,Job,StatefulSet,CronJob",
        "spec:",
        f"  validationFailureAction: {profile.admission_validation_failure_action}",
        f"  failurePolicy: {profile.admission_failure_policy}",
        "  background: false",
        "  webhookTimeoutSeconds: 30",
        "  rules:",
        f"    - name: {SECURITY_CLUSTER_POLICY_NAME}",
        "      match:",
        "        any:",
        "          - resources:",
        "              kinds:",
        "                - Pod",
        "      context:",
        f"        - name: {SECURITY_PUBLIC_KEY_CONTEXT_NAME}",
        "          configMap:",
        f"            namespace: {profile.public_key_configmap_namespace}",
        f"            name: {profile.public_key_configmap_name}",
        "      verifyImages:",
        "        - imageReferences:",
        f"            - {SECURITY_IMAGE_REFERENCE_PLACEHOLDER}",
        f"          mutateDigest: {str(profile.admission_mutate_digest).lower()}",
        f"          verifyDigest: {str(profile.admission_verify_digest).lower()}",
        f"          required: {str(profile.admission_required).lower()}",
        "          attestors:",
        "            - count: 1",
        "              entries:",
        "                - keys:",
        f'                    publicKeys: "{{{{ {SECURITY_PUBLIC_KEY_CONTEXT_NAME}.data.{profile.public_key_configmap_key} }}}}"',
        "                    rekor:",
        f"                      url: {TRANSPARENCY_LOG_URL}",
        "                      ignoreTlog: false",
    )


def _expected_production_kustomization_lines(
    profile: ProductionSigningProfile,
) -> tuple[str, ...]:
    return (
        "apiVersion: kustomize.config.k8s.io/v1beta1",
        "kind: Kustomization",
        "resources:",
        "  - ../namespaced",
        "  - ../cluster",
        "replacements:",
        "  - source:",
        "      kind: ConfigMap",
        f"      name: {profile.repository_configmap_name}",
        f"      fieldPath: data.{profile.repository_configmap_key}",
        "    targets:",
        "      - select:",
        "          group: kyverno.io",
        "          version: v1",
        "          kind: ClusterPolicy",
        f"          name: {SECURITY_CLUSTER_POLICY_NAME}",
        "        fieldPaths:",
        f"          - {SECURITY_IMAGE_REFERENCE_FIELD_PATH}",
    )


def _expected_cluster_kustomization_lines() -> tuple[str, ...]:
    return (
        "apiVersion: kustomize.config.k8s.io/v1beta1",
        "kind: Kustomization",
        "resources:",
        f"  - {SECURITY_CLUSTER_POLICY_PATH.name}",
    )


def _expected_namespaced_kustomization_lines() -> tuple[str, ...]:
    return (
        "apiVersion: kustomize.config.k8s.io/v1beta1",
        "kind: Kustomization",
        "resources:",
        f"  - {SECURITY_PUBLIC_KEY_CONFIGMAP_PATH.name}",
        f"  - {SECURITY_REPOSITORY_CONFIGMAP_PATH.name}",
    )


def _validate_registry_host(
    value: str,
    *,
    code: str,
    message: str,
    field: str,
) -> str:
    host = value.strip()
    match = REGISTRY_HOST_PATTERN.fullmatch(host)
    port = match.group(1) if match is not None else None
    if match is None or (port is not None and not 1 <= int(port) <= 65535):
        raise common.ReleaseGateError(code, message, {"field": field})
    return host


def _validate_repository_reference(
    value: str,
    *,
    code: str,
    message: str,
    field: str,
) -> str:
    repository = value.strip()
    if (
        not repository
        or any(character.isspace() or ord(character) < 32 for character in repository)
        or repository.startswith("/")
        or repository.endswith("/")
        or "//" in repository
        or "@" in repository
        or "://" in repository
    ):
        raise common.ReleaseGateError(code, message, {"field": field})
    host, separator, remainder = repository.partition("/")
    if not separator or not remainder:
        raise common.ReleaseGateError(code, message, {"field": field})
    _validate_registry_host(host, code=code, message=message, field=field)
    components = remainder.split("/")
    if not all(REPOSITORY_PATH_COMPONENT_PATTERN.fullmatch(component) for component in components):
        raise common.ReleaseGateError(code, message, {"field": field})
    return repository


def _validate_repository_pattern(
    value: str,
    *,
    code: str,
    message: str,
    field: str,
    expected_repository: str | None = None,
) -> str:
    pattern = value.strip()
    if (
        not pattern
        or any(character.isspace() or ord(character) < 32 for character in pattern)
        or pattern == PLACEHOLDER_REPOSITORY_PATTERN
        or pattern.count("*") != 1
        or not pattern.endswith("*")
    ):
        raise common.ReleaseGateError(code, message, {"field": field})
    repository = _validate_repository_reference(
        pattern[:-1],
        code=code,
        message=message,
        field=field,
    )
    if expected_repository is not None and repository != expected_repository:
        raise common.ReleaseGateError(
            code,
            message,
            {"field": field, "expectedPattern": f"{expected_repository}*"},
        )
    return pattern


def _validate_production_signing_artifacts(
    repo_root: pathlib.Path,
    profile: ProductionSigningProfile,
) -> None:
    cluster_policy_path = repo_root / profile.cluster_policy_path
    production_kustomization_path = repo_root / profile.cluster_kustomization_path / "kustomization.yaml"
    cluster_kustomization_path = repo_root / SECURITY_CLUSTER_KUSTOMIZATION_PATH / "kustomization.yaml"
    namespace_kustomization_path = (
        repo_root / SECURITY_NAMESPACE_KUSTOMIZATION_PATH / "kustomization.yaml"
    )
    public_key_configmap_path = repo_root / SECURITY_PUBLIC_KEY_CONFIGMAP_PATH
    repository_configmap_path = repo_root / SECURITY_REPOSITORY_CONFIGMAP_PATH
    try:
        cluster_policy = cluster_policy_path.read_text(encoding="utf-8")
        production_kustomization = production_kustomization_path.read_text(encoding="utf-8")
        cluster_kustomization = cluster_kustomization_path.read_text(encoding="utf-8")
        namespace_kustomization = namespace_kustomization_path.read_text(encoding="utf-8")
        public_key_configmap = public_key_configmap_path.read_text(encoding="utf-8")
        repository_configmap = repository_configmap_path.read_text(encoding="utf-8")
    except OSError:
        raise common.ReleaseGateError(
            "release.registry_production_signing_profile_invalid",
            "Worker Registry production signing profile referenced unreadable security artifacts.",
        ) from None
    _require_normalized_manifest_lines(
        cluster_policy,
        _expected_cluster_policy_lines(profile),
        code="release.registry_production_signing_profile_invalid",
        message="Worker Registry production signing artifacts did not preserve the required Kyverno admission policy.",
        path=cluster_policy_path,
    )
    _require_normalized_manifest_lines(
        production_kustomization,
        _expected_production_kustomization_lines(profile),
        code="release.registry_production_signing_profile_invalid",
        message="Worker Registry production signing production kustomization was invalid.",
        path=production_kustomization_path,
    )
    _require_normalized_manifest_lines(
        cluster_kustomization,
        _expected_cluster_kustomization_lines(),
        code="release.registry_production_signing_profile_invalid",
        message="Worker Registry production signing cluster kustomization was invalid.",
        path=cluster_kustomization_path,
    )
    _require_normalized_manifest_lines(
        namespace_kustomization,
        _expected_namespaced_kustomization_lines(),
        code="release.registry_production_signing_profile_invalid",
        message="Worker Registry production signing namespace kustomization was invalid.",
        path=namespace_kustomization_path,
    )
    public_key_manifest = _load_configmap_manifest(
        SECURITY_PUBLIC_KEY_CONFIGMAP_PATH,
        public_key_configmap,
        expected_name=profile.public_key_configmap_name,
        expected_namespace=profile.public_key_configmap_namespace,
        expected_key=profile.public_key_configmap_key,
        code="release.registry_production_signing_profile_invalid",
        message="Worker Registry production signing public-key ConfigMap was invalid.",
    )
    repository_manifest = _load_configmap_manifest(
        SECURITY_REPOSITORY_CONFIGMAP_PATH,
        repository_configmap,
        expected_name=profile.repository_configmap_name,
        expected_namespace=profile.repository_configmap_namespace,
        expected_key=profile.repository_configmap_key,
        code="release.registry_production_signing_profile_invalid",
        message="Worker Registry production signing repository ConfigMap was invalid.",
    )
    _public_key_sha256(
        public_key_manifest.data[profile.public_key_configmap_key],
        code="release.registry_production_signing_profile_invalid",
        message="Worker Registry production signing public-key ConfigMap was invalid.",
        field=f"{SECURITY_PUBLIC_KEY_CONFIGMAP_PATH}:{profile.public_key_configmap_key}",
    )
    _validate_repository_pattern(
        repository_manifest.data[profile.repository_configmap_key],
        code="release.registry_production_signing_profile_invalid",
        message="Worker Registry production signing repository ConfigMap was invalid.",
        field=f"{SECURITY_REPOSITORY_CONFIGMAP_PATH}:{profile.repository_configmap_key}",
    )


def _split_yaml_documents(text: str) -> list[str]:
    documents: list[list[str]] = [[]]
    for raw_line in text.replace("\r\n", "\n").replace("\r", "\n").split("\n"):
        if raw_line.strip() == "---":
            if documents[-1]:
                documents.append([])
            continue
        documents[-1].append(raw_line)
    return ["\n".join(lines).strip("\n") for lines in documents if any(line.strip() for line in lines)]


def _parse_yaml_scalar(value: str) -> str:
    text = value.strip()
    if len(text) >= 2 and text[0] == text[-1] and text[0] in {'"', "'"}:
        return text[1:-1]
    return text


def _capture_yaml_block_scalar(
    lines: Sequence[str],
    start_index: int,
    *,
    parent_indent: int,
) -> tuple[str, int]:
    block_lines: list[str] = []
    block_indent: int | None = None
    index = start_index
    while index < len(lines):
        raw_line = lines[index]
        if not raw_line.strip():
            if block_indent is not None:
                block_lines.append("")
            index += 1
            continue
        indent = len(raw_line) - len(raw_line.lstrip(" "))
        if block_indent is None:
            if indent <= parent_indent:
                break
            block_indent = indent
        if indent < block_indent:
            break
        block_lines.append(raw_line[block_indent:])
        index += 1
    return ("\n".join(block_lines)).strip("\n"), index


def _parse_configmap_manifest_document(document: str) -> ParsedConfigMapManifest | None:
    lines = document.splitlines()
    kind: str | None = None
    metadata: dict[str, str] = {}
    data: dict[str, str] = {}
    section: str | None = None
    index = 0
    while index < len(lines):
        raw_line = lines[index]
        stripped = raw_line.strip()
        if not stripped or stripped.startswith("#"):
            index += 1
            continue
        indent = len(raw_line) - len(raw_line.lstrip(" "))
        if indent == 0:
            key, separator, value = stripped.partition(":")
            if not separator:
                return None
            section = key.strip()
            value = value.lstrip()
            if section == "kind":
                kind = _parse_yaml_scalar(value)
            index += 1
            continue
        if section == "metadata" and indent == 2:
            key, separator, value = stripped.partition(":")
            if not separator:
                return None
            key = key.strip()
            value = value.lstrip()
            if key in {"name", "namespace"} and value:
                metadata[key] = _parse_yaml_scalar(value)
            index += 1
            continue
        if section == "data" and indent == 2:
            key, separator, value = stripped.partition(":")
            if not separator:
                return None
            key = key.strip()
            value = value.lstrip()
            if value in {"|", "|-", "|+"}:
                block, index = _capture_yaml_block_scalar(
                    lines,
                    index + 1,
                    parent_indent=indent,
                )
                data[key] = block
                continue
            if value:
                data[key] = _parse_yaml_scalar(value)
            index += 1
            continue
        index += 1
    if kind != "ConfigMap":
        return None
    name = metadata.get("name")
    namespace = metadata.get("namespace")
    if not name or not namespace:
        return None
    return ParsedConfigMapManifest(
        name=name,
        namespace=namespace,
        data=data,
    )


def _load_configmap_manifest(
    path: pathlib.Path,
    text: str,
    *,
    expected_name: str,
    expected_namespace: str,
    expected_key: str,
    code: str,
    message: str,
) -> ParsedConfigMapManifest:
    matches: list[ParsedConfigMapManifest] = []
    for document in _split_yaml_documents(text):
        parsed = _parse_configmap_manifest_document(document)
        if parsed is None:
            continue
        if parsed.name == expected_name and parsed.namespace == expected_namespace:
            matches.append(parsed)
    if len(matches) != 1 or expected_key not in matches[0].data:
        raise common.ReleaseGateError(code, message, {"path": str(path)})
    return matches[0]


def _read_runtime_manifest(
    path: pathlib.Path,
    *,
    expected_name: str,
    expected_namespace: str,
    expected_key: str,
    code: str,
    message: str,
) -> ParsedConfigMapManifest:
    try:
        text = path.read_text(encoding="utf-8")
    except OSError:
        raise common.ReleaseGateError(code, message, {"path": str(path)}) from None
    return _load_configmap_manifest(
        path,
        text,
        expected_name=expected_name,
        expected_namespace=expected_namespace,
        expected_key=expected_key,
        code=code,
        message=message,
    )


def _public_key_sha256(
    value: str,
    *,
    code: str,
    message: str,
    field: str,
) -> str:
    normalized = value.strip().replace("\r\n", "\n").replace("\r", "\n")
    if PLACEHOLDER_PUBLIC_KEY_MARKER in normalized:
        raise common.ReleaseGateError(code, message, {"field": field})
    pem_text = normalized if normalized.endswith("\n") else normalized + "\n"
    if PEM_PUBLIC_KEY_PATTERN.fullmatch(pem_text) is None:
        raise common.ReleaseGateError(code, message, {"field": field})
    lines = [line.strip() for line in normalized.split("\n") if line.strip()]
    if len(lines) < 3 or lines[0] != "-----BEGIN PUBLIC KEY-----" or lines[-1] != "-----END PUBLIC KEY-----":
        raise common.ReleaseGateError(code, message, {"field": field})
    try:
        der = base64.b64decode("".join(lines[1:-1]), validate=True)
    except binascii.Error:
        raise common.ReleaseGateError(code, message, {"field": field}) from None
    if len(der) < 64 or len(der) > 8192:
        raise common.ReleaseGateError(code, message, {"field": field})
    try:
        _validate_subject_public_key_info_der(der)
    except ValueError:
        raise common.ReleaseGateError(code, message, {"field": field})
    return _sha256(der)


def _validate_runtime_repository_pattern(
    value: str,
    *,
    image_repository: str,
    code: str,
    message: str,
    field: str,
) -> str:
    return _validate_repository_pattern(
        value,
        code=code,
        message=message,
        field=field,
        expected_repository=image_repository,
    )


def load_production_signing_profile(
    repo_root: pathlib.Path,
    *,
    signing_policy: SigningPolicy,
) -> ProductionSigningProfile:
    raw = _read_bytes(
        repo_root,
        PRODUCTION_SIGNING_PROFILE_PATH,
        label="production signing profile",
    )
    try:
        payload = json.loads(raw)
    except (UnicodeDecodeError, json.JSONDecodeError):
        raise common.ReleaseGateError(
            "release.registry_production_signing_profile_invalid",
            "Worker Registry production signing profile was not valid JSON.",
        ) from None
    expected_keys = {
        "schemaVersion",
        "signingPolicyPath",
        "signer",
        "registryAccess",
        "transparencyLog",
        "admission",
    }
    signer_keys = {
        "type",
        "keyReference",
        "authMethod",
        "principal",
        "auditRequestPath",
        "credentialEnvironment",
    }
    registry_access_keys = {
        "usernameEnvironment",
        "passwordEnvironment",
        "caCertEnvironment",
    }
    transparency_keys = {
        "provider",
        "url",
        "upload",
        "required",
        "verify",
        "inclusionProofRequired",
        "signedEntryTimestampRequired",
    }
    configmap_keys = {"namespace", "name", "key"}
    admission_keys = {
        "provider",
        "required",
        "failurePolicy",
        "validationFailureAction",
        "mutateDigest",
        "verifyDigest",
        "publicKeyConfigMap",
        "repositoryConfigMap",
        "clusterPolicyPath",
        "clusterKustomizationPath",
        "namespaceKustomizationPath",
    }
    signer = payload.get("signer") if isinstance(payload, dict) else None
    registry_access = payload.get("registryAccess") if isinstance(payload, dict) else None
    transparency_log = payload.get("transparencyLog") if isinstance(payload, dict) else None
    admission = payload.get("admission") if isinstance(payload, dict) else None
    public_key_configmap = admission.get("publicKeyConfigMap") if isinstance(admission, dict) else None
    repository_configmap = (
        admission.get("repositoryConfigMap") if isinstance(admission, dict) else None
    )
    if (
        not isinstance(payload, dict)
        or set(payload) != expected_keys
        or payload.get("schemaVersion") != 1
        or not isinstance(signer, dict)
        or set(signer) != signer_keys
        or not isinstance(registry_access, dict)
        or set(registry_access) != registry_access_keys
        or not isinstance(transparency_log, dict)
        or set(transparency_log) != transparency_keys
        or not isinstance(admission, dict)
        or set(admission) != admission_keys
        or not isinstance(public_key_configmap, dict)
        or set(public_key_configmap) != configmap_keys
        or not isinstance(repository_configmap, dict)
        or set(repository_configmap) != configmap_keys
    ):
        raise common.ReleaseGateError(
            "release.registry_production_signing_profile_invalid",
            "Worker Registry production signing profile schema was invalid.",
        )
    signing_policy_path, _ = _validated_relative_repo_path(
        repo_root,
        payload.get("signingPolicyPath"),
        field="signingPolicyPath",
        code="release.registry_production_signing_profile_invalid",
        message="Worker Registry production signing profile referenced an invalid signing policy path.",
    )
    if signing_policy_path != str(PRODUCTION_SIGNING_POLICY_PATH) or signing_policy_path != signing_policy.path:
        raise common.ReleaseGateError(
            "release.registry_production_signing_profile_invalid",
            "Worker Registry production signing profile did not point at the selected production signing policy.",
        )
    signer_type = _optional_policy_text(
        signer.get("type"),
        field="signer.type",
        maximum_length=128,
    )
    key_reference = _optional_policy_text(
        signer.get("keyReference"),
        field="signer.keyReference",
    )
    auth_method = _optional_policy_text(
        signer.get("authMethod"),
        field="signer.authMethod",
        maximum_length=64,
    )
    principal = _optional_policy_text(
        signer.get("principal"),
        field="signer.principal",
        maximum_length=256,
    )
    audit_request_path = _optional_policy_text(
        signer.get("auditRequestPath"),
        field="signer.auditRequestPath",
        maximum_length=256,
    )
    raw_environment = signer.get("credentialEnvironment")
    if (
        signer_type != "vault-transit-kms"
        or key_reference != VAULT_TRANSIT_KEY_REFERENCE
        or key_reference != signing_policy.key_reference
        or auth_method != VAULT_TRANSIT_AUTH_METHOD
        or principal != VAULT_TRANSIT_PRINCIPAL
        or audit_request_path != VAULT_TRANSIT_AUDIT_REQUEST_PATH
        or not isinstance(raw_environment, list)
        or not all(
            isinstance(name, str) and ENVIRONMENT_NAME_PATTERN.fullmatch(name) is not None
            for name in raw_environment
        )
        or len(set(raw_environment)) != len(raw_environment)
    ):
        raise common.ReleaseGateError(
            "release.registry_production_signing_profile_invalid",
            "Worker Registry production signing profile signer values were invalid.",
        )
    credential_environment = tuple(raw_environment)
    allowed_environments = (
        VAULT_TRANSIT_REQUIRED_ENVIRONMENT,
        VAULT_TRANSIT_REQUIRED_ENVIRONMENT + VAULT_TRANSIT_OPTIONAL_ENVIRONMENT,
    )
    if (
        credential_environment not in allowed_environments
        or credential_environment != signing_policy.credential_environment
    ):
        raise common.ReleaseGateError(
            "release.registry_production_signing_profile_invalid",
            "Worker Registry production signing profile signer environment names did not match the selected policy.",
        )
    registry_username_environment = _optional_policy_text(
        registry_access.get("usernameEnvironment"),
        field="registryAccess.usernameEnvironment",
        maximum_length=128,
    )
    registry_password_environment = _optional_policy_text(
        registry_access.get("passwordEnvironment"),
        field="registryAccess.passwordEnvironment",
        maximum_length=128,
    )
    registry_ca_cert_environment = _optional_policy_text(
        registry_access.get("caCertEnvironment"),
        field="registryAccess.caCertEnvironment",
        maximum_length=128,
    )
    if (
        registry_username_environment != PRODUCTION_REGISTRY_USERNAME_ENV
        or registry_password_environment != PRODUCTION_REGISTRY_PASSWORD_ENV
        or registry_ca_cert_environment != PRODUCTION_REGISTRY_CA_CERT_ENV
    ):
        raise common.ReleaseGateError(
            "release.registry_production_signing_profile_invalid",
            "Worker Registry production signing profile registry access environment names did not match the production boundary.",
        )
    if (
        transparency_log.get("provider") != TRANSPARENCY_LOG_PROVIDER
        or transparency_log.get("url") != TRANSPARENCY_LOG_URL
        or transparency_log.get("upload") is not True
        or transparency_log.get("required") is not True
        or transparency_log.get("verify") is not True
        or transparency_log.get("inclusionProofRequired") is not True
        or transparency_log.get("signedEntryTimestampRequired") is not True
        or signing_policy.require_transparency_log is not True
    ):
        raise common.ReleaseGateError(
            "release.registry_production_signing_profile_invalid",
            "Worker Registry production signing profile did not require transparency-log upload and verification.",
        )
    admission_provider = _optional_policy_text(
        admission.get("provider"),
        field="admission.provider",
        maximum_length=64,
    )
    admission_required = admission.get("required")
    admission_failure_policy = _optional_policy_text(
        admission.get("failurePolicy"),
        field="admission.failurePolicy",
        maximum_length=64,
    )
    admission_validation_failure_action = _optional_policy_text(
        admission.get("validationFailureAction"),
        field="admission.validationFailureAction",
        maximum_length=64,
    )
    admission_mutate_digest = admission.get("mutateDigest")
    admission_verify_digest = admission.get("verifyDigest")
    if (
        admission_provider != "kyverno"
        or admission_required is not True
        or admission_failure_policy != ADMISSION_FAILURE_POLICY
        or admission_validation_failure_action != ADMISSION_VALIDATION_FAILURE_ACTION
        or admission_mutate_digest is not True
        or admission_verify_digest is not True
    ):
        raise common.ReleaseGateError(
            "release.registry_production_signing_profile_invalid",
            "Worker Registry production signing profile did not require Kyverno admission enforcement.",
        )
    public_key_configmap_namespace = _validate_kubernetes_name(
        public_key_configmap.get("namespace"),
        field="admission.publicKeyConfigMap.namespace",
        code="release.registry_production_signing_profile_invalid",
        message="Worker Registry production signing profile used an invalid public-key ConfigMap namespace.",
    )
    public_key_configmap_name = _validate_kubernetes_name(
        public_key_configmap.get("name"),
        field="admission.publicKeyConfigMap.name",
        code="release.registry_production_signing_profile_invalid",
        message="Worker Registry production signing profile used an invalid public-key ConfigMap name.",
    )
    public_key_configmap_key = _validate_configmap_key(
        public_key_configmap.get("key"),
        field="admission.publicKeyConfigMap.key",
        code="release.registry_production_signing_profile_invalid",
        message="Worker Registry production signing profile used an invalid public-key ConfigMap key.",
    )
    repository_configmap_namespace = _validate_kubernetes_name(
        repository_configmap.get("namespace"),
        field="admission.repositoryConfigMap.namespace",
        code="release.registry_production_signing_profile_invalid",
        message="Worker Registry production signing profile used an invalid repository ConfigMap namespace.",
    )
    repository_configmap_name = _validate_kubernetes_name(
        repository_configmap.get("name"),
        field="admission.repositoryConfigMap.name",
        code="release.registry_production_signing_profile_invalid",
        message="Worker Registry production signing profile used an invalid repository ConfigMap name.",
    )
    repository_configmap_key = _validate_configmap_key(
        repository_configmap.get("key"),
        field="admission.repositoryConfigMap.key",
        code="release.registry_production_signing_profile_invalid",
        message="Worker Registry production signing profile used an invalid repository ConfigMap key.",
    )
    cluster_policy_path, _ = _validated_relative_repo_path(
        repo_root,
        admission.get("clusterPolicyPath"),
        field="admission.clusterPolicyPath",
        code="release.registry_production_signing_profile_invalid",
        message="Worker Registry production signing profile referenced an invalid Kyverno ClusterPolicy path.",
    )
    cluster_kustomization_path, _ = _validated_relative_repo_path(
        repo_root,
        admission.get("clusterKustomizationPath"),
        field="admission.clusterKustomizationPath",
        code="release.registry_production_signing_profile_invalid",
        message="Worker Registry production signing profile referenced an invalid cluster kustomization path.",
        directory=True,
    )
    namespace_kustomization_path, _ = _validated_relative_repo_path(
        repo_root,
        admission.get("namespaceKustomizationPath"),
        field="admission.namespaceKustomizationPath",
        code="release.registry_production_signing_profile_invalid",
        message="Worker Registry production signing profile referenced an invalid namespace kustomization path.",
        directory=True,
    )
    if (
        cluster_policy_path != str(SECURITY_CLUSTER_POLICY_PATH)
        or cluster_kustomization_path != str(SECURITY_PRODUCTION_KUSTOMIZATION_PATH)
        or namespace_kustomization_path != str(SECURITY_PRODUCTION_KUSTOMIZATION_PATH)
    ):
        raise common.ReleaseGateError(
            "release.registry_production_signing_profile_invalid",
            "Worker Registry production signing profile referenced unexpected security artifact locations.",
        )
    profile = ProductionSigningProfile(
        path=str(PRODUCTION_SIGNING_PROFILE_PATH),
        signing_policy_path=signing_policy_path,
        signer_type=str(signer_type),
        key_reference=str(key_reference),
        auth_method=str(auth_method),
        principal=str(principal),
        audit_request_path=str(audit_request_path),
        credential_environment=credential_environment,
        registry_username_environment=str(registry_username_environment),
        registry_password_environment=str(registry_password_environment),
        registry_ca_cert_environment=str(registry_ca_cert_environment),
        transparency_log_provider=TRANSPARENCY_LOG_PROVIDER,
        transparency_log_url=TRANSPARENCY_LOG_URL,
        transparency_log_upload=True,
        transparency_log_required=True,
        transparency_log_verify=True,
        transparency_log_inclusion_proof_required=True,
        transparency_log_signed_entry_timestamp_required=True,
        admission_provider=str(admission_provider),
        admission_required=True,
        admission_failure_policy=str(admission_failure_policy),
        admission_validation_failure_action=str(admission_validation_failure_action),
        admission_mutate_digest=True,
        admission_verify_digest=True,
        public_key_configmap_namespace=public_key_configmap_namespace,
        public_key_configmap_name=public_key_configmap_name,
        public_key_configmap_key=public_key_configmap_key,
        repository_configmap_namespace=repository_configmap_namespace,
        repository_configmap_name=repository_configmap_name,
        repository_configmap_key=repository_configmap_key,
        cluster_policy_path=cluster_policy_path,
        cluster_kustomization_path=cluster_kustomization_path,
        namespace_kustomization_path=namespace_kustomization_path,
        sha256=_sha256(raw),
    )
    _validate_production_signing_artifacts(repo_root, profile)
    return profile


def load_vulnerability_policy(repo_root: pathlib.Path) -> VulnerabilityPolicy:
    raw = _read_bytes(repo_root, VULNERABILITY_POLICY_PATH, label="vulnerability policy")
    try:
        payload = json.loads(raw)
    except (UnicodeDecodeError, json.JSONDecodeError):
        raise common.ReleaseGateError(
            "release.registry_supply_chain_policy_invalid",
            "Worker Registry vulnerability policy was not valid JSON.",
        ) from None
    expected_keys = {
        "schemaVersion",
        "blockedSeverities",
        "ignoreUnfixed",
        "failOnEndOfLifeOS",
        "maximumDatabaseAgeHours",
        "exceptions",
    }
    if not isinstance(payload, dict) or set(payload) != expected_keys or payload.get("schemaVersion") != 1:
        raise common.ReleaseGateError(
            "release.registry_supply_chain_policy_invalid",
            "Worker Registry vulnerability policy schema was invalid.",
        )
    severities = payload.get("blockedSeverities")
    maximum_age = payload.get("maximumDatabaseAgeHours")
    raw_exceptions = payload.get("exceptions")
    if (
        not isinstance(severities, list)
        or not severities
        or not all(isinstance(value, str) and value in SUPPORTED_SEVERITIES for value in severities)
        or len(set(severities)) != len(severities)
        or not isinstance(payload.get("ignoreUnfixed"), bool)
        or not isinstance(payload.get("failOnEndOfLifeOS"), bool)
        or isinstance(maximum_age, bool)
        or not isinstance(maximum_age, int)
        or not 1 <= maximum_age <= 168
        or not isinstance(raw_exceptions, list)
    ):
        raise common.ReleaseGateError(
            "release.registry_supply_chain_policy_invalid",
            "Worker Registry vulnerability policy values were invalid.",
        )
    exceptions: list[VulnerabilityException] = []
    identities: set[tuple[str, str, str]] = set()
    exception_keys = {"vulnerabilityId", "package", "platform", "expiresAt", "owner", "reason"}
    for index, item in enumerate(raw_exceptions):
        if not isinstance(item, dict) or set(item) != exception_keys:
            raise common.ReleaseGateError(
                "release.registry_supply_chain_policy_invalid",
                "Worker Registry vulnerability policy exception schema was invalid.",
                {"exceptionIndex": index},
            )
        vulnerability_id = item.get("vulnerabilityId")
        package = item.get("package")
        platform = item.get("platform")
        owner = item.get("owner")
        reason = item.get("reason")
        if (
            not isinstance(vulnerability_id, str)
            or VULNERABILITY_ID_PATTERN.fullmatch(vulnerability_id) is None
            or not isinstance(package, str)
            or not package.strip()
            or len(package) > 256
            or platform not in {"linux/amd64", "linux/arm64"}
            or not isinstance(owner, str)
            or len(owner.strip()) < 2
            or len(owner) > 200
            or not isinstance(reason, str)
            or len(reason.strip()) < 10
            or len(reason) > 1000
        ):
            raise common.ReleaseGateError(
                "release.registry_supply_chain_policy_invalid",
                "Worker Registry vulnerability policy exception values were invalid.",
                {"exceptionIndex": index},
            )
        exception = VulnerabilityException(
            vulnerability_id=vulnerability_id.upper(),
            package=package.strip(),
            platform=str(platform),
            expires_at=_parse_timestamp(item.get("expiresAt"), field=f"exceptions[{index}].expiresAt"),
            owner=owner.strip(),
            reason=reason.strip(),
        )
        if exception.identity in identities:
            raise common.ReleaseGateError(
                "release.registry_supply_chain_policy_invalid",
                "Worker Registry vulnerability policy contained duplicate exceptions.",
                {"exceptionIndex": index},
            )
        identities.add(exception.identity)
        exceptions.append(exception)
    return VulnerabilityPolicy(
        blocked_severities=tuple(severities),
        ignore_unfixed=bool(payload["ignoreUnfixed"]),
        fail_on_end_of_life_os=bool(payload["failOnEndOfLifeOS"]),
        maximum_database_age_hours=int(maximum_age),
        exceptions=tuple(exceptions),
        sha256=_sha256(raw),
    )


def load_configuration(
    repo_root: pathlib.Path,
    *,
    signing_policy_profile: str = "disposable",
) -> SupplyChainConfiguration:
    selected_policy_path = signing_policy_path(signing_policy_profile)
    signing_policy = load_signing_policy(repo_root, policy_path=selected_policy_path)
    production_signing_profile: ProductionSigningProfile | None = None
    if signing_policy_profile == "production":
        if not signing_policy.production_policy:
            raise common.ReleaseGateError(
                "release.registry_production_signing_profile_invalid",
                "Worker Registry production signing selector did not choose a production signing policy.",
            )
        production_signing_profile = load_production_signing_profile(
            repo_root,
            signing_policy=signing_policy,
        )
    return SupplyChainConfiguration(
        tools=load_tool_images(repo_root),
        signing_policy_profile=signing_policy_profile,
        signing_policy=signing_policy,
        production_signing_profile=production_signing_profile,
        vulnerability_policy=load_vulnerability_policy(repo_root),
    )


def _locked_version(reference: str, *, tool: str) -> str:
    named_reference = reference.rsplit("@", 1)[0]
    last_component = named_reference.rsplit("/", 1)[-1]
    if ":" not in last_component:
        raise common.ReleaseGateError(
            "release.registry_supply_chain_source_invalid",
            "Worker Registry supply-chain tool reference omitted a human-readable version tag.",
            {"tool": tool},
        )
    version = last_component.rsplit(":", 1)[-1]
    if re.fullmatch(r"v?[0-9]+\.[0-9]+\.[0-9]+", version) is None:
        raise common.ReleaseGateError(
            "release.registry_supply_chain_source_invalid",
            "Worker Registry supply-chain tool reference used an invalid version tag.",
            {"tool": tool},
        )
    return version


def _remaining(deadline: float, *, maximum: float) -> float:
    remaining = deadline - time.monotonic()
    if remaining <= 0:
        raise common.ReleaseGateError(
            "release.registry_supply_chain_timeout",
            "Worker Registry supply-chain verification exceeded its deadline.",
        )
    return max(1.0, min(maximum, remaining))


def _registry_host(image_repository: str) -> str:
    host = image_repository.split("/", 1)[0].strip()
    match = REGISTRY_HOST_PATTERN.fullmatch(host)
    port = match.group(1) if match is not None else None
    if match is None or (port is not None and not 1 <= int(port) <= 65535):
        raise common.ReleaseGateError(
            "release.registry_signing_credential_invalid",
            "Worker Registry supply-chain gate used an invalid registry host.",
        )
    return host


def _registry_state_paths(options: SupplyChainOptions) -> dict[str, pathlib.Path]:
    registry_host = _registry_host(options.image_repository)
    root = options.state_dir / "registry-access"
    docker_config_dir = root / "docker-config"
    certs_dir = docker_config_dir / "certs.d" / registry_host
    return {
        "root": root,
        "docker_config_dir": docker_config_dir,
        "docker_config": docker_config_dir / "config.json",
        "registry_ca": certs_dir / "ca.crt",
    }


def _prepare_registry_access(
    options: SupplyChainOptions,
    *,
    redactor: acceptance.SecretRedactor,
) -> PreparedRegistryAccess:
    username_env = options.registry_auth_username_environment
    password_env = options.registry_auth_password_environment
    ca_env = options.registry_ca_cert_environment
    if username_env is None and password_env is None and ca_env is None:
        return PreparedRegistryAccess(
            environment={},
            host_environment={},
            registry_host=_registry_host(options.image_repository),
            auth_configured=False,
            ca_materialized=False,
            registry_ca_container_path=None,
        )
    if username_env is None or password_env is None or ca_env is None:
        raise common.ReleaseGateError(
            "release.registry_signing_credential_invalid",
            "Worker Registry production registry access was incompletely configured.",
        )
    try:
        username = acceptance.read_environment_value(
            username_env,
            "Registry Basic auth username",
            maximum_length=1024,
            forbidden_characters="\r\n\x00",
        )
        password = acceptance.read_environment_value(
            password_env,
            "Registry Basic auth password",
            maximum_length=64 << 10,
            forbidden_characters="\r\n\x00",
        )
        ca_host_path = acceptance.read_environment_value(
            ca_env,
            "Registry CA certificate path",
            maximum_length=4096,
            forbidden_characters="\r\n\x00",
        )
    except acceptance.EnvironmentValueError as error:
        raise common.ReleaseGateError(
            "release.registry_signing_credential_invalid",
            "A configured production registry access environment value was unavailable or invalid.",
            {"reason": error.reason},
        ) from None
    redactor.add(username, "[REDACTED_REGISTRY_USERNAME]")
    redactor.add(password, "[REDACTED_REGISTRY_PASSWORD]")
    redactor.add(f"{username}:{password}", "[REDACTED_REGISTRY_BASIC_AUTH]")
    redactor.add(ca_host_path, "[REDACTED_REGISTRY_CA_PATH]")
    auth_token = base64.b64encode(f"{username}:{password}".encode("utf-8")).decode("ascii")
    redactor.add(auth_token, "[REDACTED_REGISTRY_BASIC_AUTH_B64]")
    paths = _registry_state_paths(options)
    try:
        source_path = pathlib.Path(ca_host_path).expanduser().resolve(strict=True)
        if not source_path.is_file():
            raise OSError
        ca_bytes = source_path.read_bytes()
    except OSError:
        raise common.ReleaseGateError(
            "release.registry_signing_credential_invalid",
            "The configured production registry CA certificate path was unavailable or invalid.",
            {"environment": ca_env},
        ) from None
    if not ca_bytes or len(ca_bytes) > (1 << 20):
        raise common.ReleaseGateError(
            "release.registry_signing_credential_invalid",
            "The configured production registry CA certificate path was unavailable or invalid.",
            {"environment": ca_env},
        )
    paths["docker_config_dir"].mkdir(parents=True, exist_ok=True)
    paths["registry_ca"].parent.mkdir(parents=True, exist_ok=True)
    try:
        paths["docker_config"].write_text(
            json.dumps(
                {
                    "auths": {
                        _registry_host(options.image_repository): {
                            "auth": auth_token,
                        }
                    }
                },
                sort_keys=True,
            )
            + "\n",
            encoding="utf-8",
        )
        paths["docker_config"].chmod(0o600)
        paths["registry_ca"].write_bytes(ca_bytes)
        paths["registry_ca"].chmod(0o600)
    except OSError:
        paths["docker_config"].unlink(missing_ok=True)
        paths["registry_ca"].unlink(missing_ok=True)
        raise common.ReleaseGateError(
            "release.registry_signing_credential_invalid",
            "Worker Registry gate could not create isolated registry access state.",
        ) from None
    registry_host = _registry_host(options.image_repository)
    return PreparedRegistryAccess(
        environment={
            "DOCKER_CONFIG": "/workspace/registry-access/docker-config",
        },
        host_environment={
            "DOCKER_CONFIG": str(paths["docker_config_dir"]),
        },
        registry_host=registry_host,
        auth_configured=True,
        ca_materialized=True,
        registry_ca_container_path=(
            f"/workspace/registry-access/docker-config/certs.d/{registry_host}/ca.crt"
        ),
    )


def _tool_arguments_with_registry_access(
    *,
    tool: str,
    arguments: Sequence[str],
    registry_access: PreparedRegistryAccess,
    insecure_registry: bool,
) -> list[str]:
    result = list(arguments)
    if tool == "cosign" and result and result[0] == "sign":
        # Kyverno v1.18 discovers classic Cosign signatures through the digest
        # .sig tag; Cosign v3's bundle format uses an incompatible tag index.
        result = [
            result[0],
            COSIGN_KYVERNO_COMPATIBILITY_ARGUMENT,
            *result[1:],
        ]
    registry_ca = registry_access.registry_ca_container_path
    if insecure_registry or registry_ca is None:
        return result
    if tool == "cosign" and result and result[0] in {"sign", "verify"}:
        return [result[0], "--registry-cacert", registry_ca, *result[1:]]
    if tool == "trivy" and result and result[0] == "image":
        return ["--cacert", registry_ca, *result]
    return result


def _run_tool(
    options: SupplyChainOptions,
    *,
    image: str,
    arguments: Sequence[str],
    tool: str,
    deadline: float,
    redactor: acceptance.SecretRedactor,
    secret_environment: Mapping[str, str] | None = None,
    maximum_timeout: float = 900.0,
) -> subprocess.CompletedProcess[str]:
    options.state_dir.mkdir(parents=True, exist_ok=True)
    (options.state_dir / "tool-home").mkdir(parents=True, exist_ok=True)
    registry_access = _prepare_registry_access(options, redactor=redactor)
    tool_arguments = _tool_arguments_with_registry_access(
        tool=tool,
        arguments=arguments,
        registry_access=registry_access,
        insecure_registry=options.insecure_registry,
    )
    command = [
        options.docker_bin,
        "run",
        "--rm",
        "--network",
        "host",
        "--user",
        f"{os.getuid()}:{os.getgid()}",
        "--cap-drop",
        "ALL",
        "--security-opt",
        "no-new-privileges",
        "--env",
        "HOME=/workspace/tool-home",
        "--volume",
        f"{options.state_dir}:/workspace",
        "--workdir",
        "/workspace",
    ]
    environment = remote.tool_environment()
    for name, value in registry_access.environment.items():
        command.extend(["--env", f"{name}={value}"])
    for name, value in (secret_environment or {}).items():
        environment[name] = value
        command.extend(["--env", name])
    command.extend([image, *tool_arguments])
    try:
        completed = subprocess.run(
            command,
            cwd=options.repo_root,
            env=environment,
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            timeout=_remaining(deadline, maximum=maximum_timeout),
        )
    except (OSError, subprocess.TimeoutExpired):
        raise common.ReleaseGateError(
            "release.registry_supply_chain_command_failed",
            "A digest-pinned Worker Registry supply-chain tool could not complete.",
            {"tool": tool},
        ) from None
    if completed.returncode != 0:
        output = redactor.text((completed.stdout + "\n" + completed.stderr).strip())[:2000]
        raise common.ReleaseGateError(
            "release.registry_supply_chain_command_failed",
            "A digest-pinned Worker Registry supply-chain tool returned a failure.",
            {"tool": tool, "returnCode": completed.returncode, "outputExcerpt": output},
        )
    return completed


def _retryable_trivy_database_download(error: common.ReleaseGateError) -> bool:
    output = error.evidence.get("outputExcerpt")
    if (
        error.code != "release.registry_supply_chain_command_failed"
        or error.evidence.get("tool") != "trivy"
        or not isinstance(output, str)
    ):
        return False
    normalized = output.lower()
    return "failed to download vulnerability db" in normalized and any(
        marker in normalized for marker in TRIVY_DATABASE_DOWNLOAD_RETRY_MARKERS
    )


def _run_trivy_scan(
    options: SupplyChainOptions,
    configuration: SupplyChainConfiguration,
    *,
    arguments: Sequence[str],
    report_path: pathlib.Path,
    deadline: float,
    redactor: acceptance.SecretRedactor,
) -> int:
    for attempt in range(2):
        try:
            _run_tool(
                options,
                image=configuration.tools.trivy,
                arguments=arguments,
                tool="trivy",
                deadline=deadline,
                redactor=redactor,
                maximum_timeout=1200.0,
            )
            return attempt
        except common.ReleaseGateError as error:
            if attempt > 0 or not _retryable_trivy_database_download(error):
                raise
            report_path.unlink(missing_ok=True)
            if deadline - time.monotonic() <= TRIVY_DATABASE_DOWNLOAD_RETRY_DELAY_SECONDS:
                raise
            time.sleep(TRIVY_DATABASE_DOWNLOAD_RETRY_DELAY_SECONDS)
    raise AssertionError("unreachable")


def _load_json_file(path: pathlib.Path, *, code: str, message: str) -> Any:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        raise common.ReleaseGateError(code, message) from None


def validate_cosign_verification(
    payload: Any,
    *,
    reference: str,
    digest: str,
    annotations: Mapping[str, str],
) -> dict[str, Any]:
    if not isinstance(payload, list) or not payload or not all(isinstance(item, dict) for item in payload):
        raise common.ReleaseGateError(
            "release.registry_signature_verification_invalid",
            "Cosign verification did not return a valid signature claim list.",
        )
    matching: list[dict[str, Any]] = []
    for item in payload:
        critical = item.get("critical")
        optional = item.get("optional")
        identity = critical.get("identity") if isinstance(critical, dict) else None
        image = critical.get("image") if isinstance(critical, dict) else None
        if (
            isinstance(identity, dict)
            and isinstance(image, dict)
            and identity.get("docker-reference") == reference
            and image.get("docker-manifest-digest") == digest
            and critical.get("type") == COSIGN_CLAIM_TYPE
            and isinstance(optional, dict)
            and all(optional.get(key) == value for key, value in annotations.items())
        ):
            matching.append(item)
    if len(matching) != 1:
        raise common.ReleaseGateError(
            "release.registry_signature_verification_invalid",
            "Cosign did not return exactly one signature for the expected digest and source annotations.",
            {"matchingSignatures": len(matching)},
        )
    return {
        "verifiedSignatureCount": 1,
        "claimType": COSIGN_CLAIM_TYPE,
        "annotations": dict(annotations),
        "verificationPayloadSha256": _sha256(
            json.dumps(matching, sort_keys=True, separators=(",", ":")).encode("utf-8")
        ),
    }


def _signature_subjects(
    options: SupplyChainOptions,
    *,
    builds: Sequence[Mapping[str, Any]],
    git_sha: str,
    version: str,
    run_id: str,
) -> list[dict[str, Any]]:
    subjects: list[dict[str, Any]] = []
    for build in builds:
        slot = build.get("slot")
        digest = build.get("registryDigest")
        if not isinstance(slot, str) or re.fullmatch(r"sha256:[0-9a-f]{64}", str(digest)) is None:
            raise common.ReleaseGateError(
                "release.registry_signature_input_invalid",
                "Worker Registry signature input omitted a valid slot or registry digest.",
            )
        reference = f"{options.image_repository}@{digest}"
        annotations = {
            "synara.git-sha": git_sha,
            "synara.run-id": run_id,
            "synara.slot": slot,
            "synara.version": version,
        }
        subjects.append(
            {
                "slot": slot,
                "digest": str(digest),
                "reference": reference,
                "annotations": annotations,
                "annotationArguments": [
                    value
                    for key, item in annotations.items()
                    for value in ("-a", f"{key}={item}")
                ],
            }
        )
    return subjects


def _verification_evidence(
    completed: subprocess.CompletedProcess[str],
    *,
    subject: Mapping[str, Any],
) -> dict[str, Any]:
    try:
        payload = json.loads(completed.stdout)
    except json.JSONDecodeError:
        raise common.ReleaseGateError(
            "release.registry_signature_verification_invalid",
            "Cosign verification output was not valid JSON.",
            {"slot": subject.get("slot")},
        ) from None
    return {
        "slot": subject["slot"],
        "reference": subject["reference"],
        "digest": subject["digest"],
        **validate_cosign_verification(
            payload,
            reference=str(subject["reference"]),
            digest=str(subject["digest"]),
            annotations=subject["annotations"],
        ),
    }


def _bundle_payload(
    value: Any,
    *,
    code: str,
    message: str,
) -> Mapping[str, Any]:
    if isinstance(value, dict):
        return value
    if isinstance(value, str) and value.strip():
        try:
            parsed = json.loads(value)
        except json.JSONDecodeError:
            raise common.ReleaseGateError(code, message) from None
        if isinstance(parsed, dict):
            return parsed
    raise common.ReleaseGateError(code, message)


def _nonempty_text(value: Any) -> str | None:
    return value.strip() if isinstance(value, str) and value.strip() else None


def _protobuf_uint64(value: Any) -> int | None:
    if isinstance(value, bool):
        return None
    if isinstance(value, int):
        parsed = value
    elif isinstance(value, str) and re.fullmatch(r"(?:0|[1-9][0-9]*)", value) is not None:
        parsed = int(value)
    else:
        return None
    return parsed if 0 <= parsed <= (1 << 64) - 1 else None


def _valid_inclusion_proof(value: Any) -> bool:
    if not isinstance(value, dict):
        return False
    root_hash = _nonempty_text(value.get("rootHash", value.get("root_hash")))
    hashes = value.get("hashes")
    log_index = _protobuf_uint64(value.get("logIndex", value.get("log_index")))
    tree_size = _protobuf_uint64(value.get("treeSize", value.get("tree_size")))
    return (
        root_hash is not None
        and isinstance(hashes, list)
        and all(_nonempty_text(item) is not None for item in hashes)
        and log_index is not None
        and tree_size is not None
        and tree_size >= 1
    )


def _bundle_transparency_entries(
    bundle: Mapping[str, Any],
    *,
    require_inclusion_proof: bool,
    require_signed_entry_timestamp: bool,
    code: str,
    message: str,
) -> tuple[str, list[dict[str, Any]]]:
    verification_material = bundle.get("verificationMaterial", bundle.get("verification_material"))
    if isinstance(verification_material, dict):
        entries = verification_material.get("tlogEntries", verification_material.get("tlog_entries"))
        if not isinstance(entries, list) or not entries:
            raise common.ReleaseGateError(code, message)
        normalized_entries: list[dict[str, Any]] = []
        for entry in entries:
            if not isinstance(entry, dict):
                raise common.ReleaseGateError(code, message)
            inclusion_proof = entry.get("inclusionProof", entry.get("inclusion_proof"))
            inclusion_promise = entry.get("inclusionPromise", entry.get("inclusion_promise"))
            signed_entry_timestamp = (
                inclusion_promise.get("signedEntryTimestamp")
                if isinstance(inclusion_promise, dict)
                else None
            )
            if require_inclusion_proof and not _valid_inclusion_proof(inclusion_proof):
                raise common.ReleaseGateError(code, message)
            if require_signed_entry_timestamp and _nonempty_text(signed_entry_timestamp) is None:
                raise common.ReleaseGateError(code, message)
            normalized_entries.append(
                {
                    "logIndex": _protobuf_uint64(
                        entry.get("logIndex", entry.get("log_index"))
                    ),
                    "integratedTime": _protobuf_uint64(
                        entry.get("integratedTime", entry.get("integrated_time"))
                    ),
                    "inclusionProofPresent": _valid_inclusion_proof(inclusion_proof),
                    "inclusionProofHashCount": (
                        len(inclusion_proof.get("hashes"))
                        if isinstance(inclusion_proof, dict)
                        and isinstance(inclusion_proof.get("hashes"), list)
                        else 0
                    ),
                    "signedEntryTimestampPresent": _nonempty_text(signed_entry_timestamp) is not None,
                    "signedEntryTimestampSha256": (
                        _sha256(_nonempty_text(signed_entry_timestamp).encode("utf-8"))
                        if _nonempty_text(signed_entry_timestamp) is not None
                        else None
                    ),
                }
            )
        media_type = bundle.get("mediaType", bundle.get("media_type"))
        return (
            str(media_type)
            if isinstance(media_type, str) and media_type
            else "application/vnd.dev.sigstore.bundle",
            normalized_entries,
        )
    payload = _bundle_payload(
        bundle.get("Payload", bundle.get("payload")),
        code=code,
        message=message,
    )
    signed_entry_timestamp = _nonempty_text(
        bundle.get("SignedEntryTimestamp", bundle.get("signedEntryTimestamp"))
    )
    inclusion_proof = payload.get("inclusionProof", payload.get("inclusion_proof"))
    if require_inclusion_proof and not _valid_inclusion_proof(inclusion_proof):
        raise common.ReleaseGateError(code, message)
    if require_signed_entry_timestamp and signed_entry_timestamp is None:
        raise common.ReleaseGateError(code, message)
    return (
        "dev.cosign.legacy.bundle",
        [
            {
                "logIndex": payload.get("logIndex", payload.get("log_index")),
                "integratedTime": payload.get("integratedTime", payload.get("integrated_time")),
                "inclusionProofPresent": _valid_inclusion_proof(inclusion_proof),
                "inclusionProofHashCount": (
                    len(inclusion_proof.get("hashes"))
                    if isinstance(inclusion_proof, dict)
                    and isinstance(inclusion_proof.get("hashes"), list)
                    else 0
                ),
                "signedEntryTimestampPresent": signed_entry_timestamp is not None,
                "signedEntryTimestampSha256": (
                    _sha256(signed_entry_timestamp.encode("utf-8"))
                    if signed_entry_timestamp is not None
                    else None
                ),
            }
        ],
    )


def _signature_bundle_evidence(
    bundle_path: pathlib.Path,
    *,
    require_inclusion_proof: bool,
    require_signed_entry_timestamp: bool,
) -> dict[str, Any]:
    try:
        bundle_raw = bundle_path.read_bytes()
        bundle = json.loads(bundle_raw)
    except (OSError, json.JSONDecodeError):
        raise common.ReleaseGateError(
            "release.registry_transparency_log_invalid",
            "Cosign did not write a valid transparency-log bundle file.",
            {"path": str(bundle_path)},
        ) from None
    if not isinstance(bundle, dict):
        raise common.ReleaseGateError(
            "release.registry_transparency_log_invalid",
            "Cosign did not write a valid transparency-log bundle file.",
            {"path": str(bundle_path)},
        )
    media_type, entries = _bundle_transparency_entries(
        bundle,
        require_inclusion_proof=require_inclusion_proof,
        require_signed_entry_timestamp=require_signed_entry_timestamp,
        code="release.registry_transparency_log_invalid",
        message="Cosign did not return the required transparency-log inclusion proof and signed entry timestamp evidence.",
    )
    return {
        "bundlePresent": True,
        "verificationMode": "cosign-online-tlog-verification",
        "bundleMediaType": media_type,
        "bundleSha256": _sha256(bundle_raw),
        "entryCount": len(entries),
        "entries": entries,
        "inclusionProofPresent": all(bool(entry["inclusionProofPresent"]) for entry in entries),
        "signedEntryTimestampPresent": all(
            bool(entry["signedEntryTimestampPresent"]) for entry in entries
        ),
    }


def _transparency_log_requirements(
    configuration: SupplyChainConfiguration,
) -> tuple[bool, bool]:
    profile = configuration.production_signing_profile
    if profile is not None:
        return (
            profile.transparency_log_inclusion_proof_required,
            profile.transparency_log_signed_entry_timestamp_required,
        )
    if configuration.signing_policy.production_policy:
        return (True, True)
    return (False, False)


def _require_production_registry_access_environment_names(
    options: SupplyChainOptions,
    configuration: SupplyChainConfiguration,
) -> None:
    profile = configuration.production_signing_profile
    if profile is None:
        return
    actual = (
        options.registry_auth_username_environment,
        options.registry_auth_password_environment,
        options.registry_ca_cert_environment,
    )
    expected = (
        profile.registry_username_environment,
        profile.registry_password_environment,
        profile.registry_ca_cert_environment,
    )
    if actual != expected:
        raise common.ReleaseGateError(
            "release.registry_signing_credential_invalid",
            "Worker Registry production registry access environment names drifted from the checked-in production profile.",
            {
                "expected": list(expected),
                "actual": list(actual),
            },
        )


def _require_production_registry(options: SupplyChainOptions) -> None:
    if options.insecure_registry:
        raise common.ReleaseGateError(
            "release.registry_production_signing_insecure_registry",
            "Production Worker Registry signing requires a TLS Registry.",
        )


def _read_signing_environment(
    names: Sequence[str],
    *,
    redactor: acceptance.SecretRedactor,
) -> dict[str, str]:
    values: dict[str, str] = {}
    for name in names:
        try:
            value = acceptance.read_environment_value(
                name,
                "Cosign KMS credential",
                maximum_length=64 << 10,
                forbidden_characters="\r\n\x00",
            )
        except acceptance.EnvironmentValueError as error:
            raise common.ReleaseGateError(
                "release.registry_signing_credential_invalid",
                "A configured Cosign KMS credential environment value was unavailable or invalid.",
                {"reason": error.reason},
            ) from None
        redactor.add(value, "[REDACTED_COSIGN_KMS_CREDENTIAL]")
        values[name] = value
    return values


def _prepare_kms_environment(
    options: SupplyChainOptions,
    policy: SigningPolicy,
    *,
    redactor: acceptance.SecretRedactor,
) -> PreparedKmsEnvironment:
    secret_environment = _read_signing_environment(
        policy.credential_environment,
        redactor=redactor,
    )
    if (
        policy.key_reference is None
        or not policy.key_reference.startswith("hashivault://")
        or "VAULT_CACERT" not in secret_environment
    ):
        return PreparedKmsEnvironment(
            secret_environment=secret_environment,
            vault_ca_materialized=False,
        )
    raw_path = secret_environment["VAULT_CACERT"]
    redactor.add(raw_path, "[REDACTED_VAULT_CACERT_PATH]")
    candidate_path = pathlib.Path(raw_path).expanduser()
    try:
        source_path = candidate_path.resolve(strict=True)
        if not source_path.is_file():
            raise OSError
        contents = source_path.read_bytes()
    except OSError:
        raise common.ReleaseGateError(
            "release.registry_signing_credential_invalid",
            "The configured Vault CA certificate path was unavailable or invalid.",
            {"environment": "VAULT_CACERT"},
        ) from None
    if not contents or len(contents) > (1 << 20):
        raise common.ReleaseGateError(
            "release.registry_signing_credential_invalid",
            "The configured Vault CA certificate path was unavailable or invalid.",
            {"environment": "VAULT_CACERT"},
        )
    materialized_path = options.state_dir / MATERIALIZED_VAULT_CACERT_RELATIVE_PATH
    materialized_path.parent.mkdir(parents=True, exist_ok=True)
    try:
        materialized_path.write_bytes(contents)
        materialized_path.chmod(0o600)
    except OSError:
        materialized_path.unlink(missing_ok=True)
        raise common.ReleaseGateError(
            "release.registry_signing_credential_invalid",
            "Worker Registry gate could not create isolated Vault CA certificate state.",
            {"environment": "VAULT_CACERT"},
        ) from None
    secret_environment["VAULT_CACERT"] = f"/workspace/{MATERIALIZED_VAULT_CACERT_RELATIVE_PATH.as_posix()}"
    return PreparedKmsEnvironment(
        secret_environment=secret_environment,
        vault_ca_materialized=True,
    )


def _vault_authority(vault_address: str) -> str:
    try:
        parsed = urllib.parse.urlsplit(vault_address)
        parsed_port = parsed.port
    except ValueError:
        parsed = None
        parsed_port = None
    if (
        parsed is None
        or vault_address != vault_address.strip()
        or any(character.isspace() or ord(character) < 0x20 for character in vault_address)
        or parsed.scheme != "https"
        or not parsed.netloc
        or parsed.hostname is None
        or parsed.username is not None
        or parsed.password is not None
        or parsed.path not in {"", "/"}
        or parsed.query
        or parsed.fragment
        or "?" in vault_address
        or "#" in vault_address
        or (parsed_port is not None and not 1 <= parsed_port <= 65535)
        or (":" in parsed.netloc and parsed_port is None and not parsed.netloc.endswith("]"))
    ):
        raise common.ReleaseGateError(
            "release.registry_signing_credential_invalid",
            "The configured Vault address was not a strict HTTPS authority.",
            {"environment": "VAULT_ADDR"},
        )
    return parsed.netloc


def _vault_approle_name(principal: str) -> str:
    prefix = "auth/approle/role/"
    role_name = principal.removeprefix(prefix)
    if (
        not principal.startswith(prefix)
        or not role_name
        or "/" in role_name
        or role_name != role_name.strip()
    ):
        raise common.ReleaseGateError(
            "release.registry_signing_credential_invalid",
            "The production Vault signer principal was invalid.",
            {"field": "productionSigningProfile.signer.principal"},
        )
    return role_name


def _production_vault_signer_identity(
    options: SupplyChainOptions,
    configuration: SupplyChainConfiguration,
    *,
    prepared_environment: PreparedKmsEnvironment,
    deadline: float,
) -> dict[str, Any]:
    profile = configuration.production_signing_profile
    if profile is None:
        return {}
    environment = prepared_environment.secret_environment
    expected_cacert = f"/workspace/{MATERIALIZED_VAULT_CACERT_RELATIVE_PATH.as_posix()}"
    if (
        profile.auth_method != VAULT_TRANSIT_AUTH_METHOD
        or profile.key_reference != VAULT_TRANSIT_KEY_REFERENCE
        or profile.principal != VAULT_TRANSIT_PRINCIPAL
        or configuration.signing_policy.key_reference != profile.key_reference
        or configuration.signing_policy.credential_environment != profile.credential_environment
        or not prepared_environment.vault_ca_materialized
        or environment.get("VAULT_CACERT") != expected_cacert
        or not isinstance(environment.get("VAULT_ADDR"), str)
        or not isinstance(environment.get("VAULT_TOKEN"), str)
    ):
        raise common.ReleaseGateError(
            "release.registry_signing_credential_invalid",
            "Production Vault signer identity could not be verified.",
            {"reason": "production Vault signer inputs were invalid"},
        )

    vault_address = environment["VAULT_ADDR"]
    vault_token = environment["VAULT_TOKEN"]
    authority = _vault_authority(vault_address)
    role_name = _vault_approle_name(profile.principal)
    materialized_cacert = options.state_dir / MATERIALIZED_VAULT_CACERT_RELATIVE_PATH
    try:
        resolved_cacert = materialized_cacert.resolve(strict=True)
        if not resolved_cacert.is_file():
            raise OSError
    except OSError:
        raise common.ReleaseGateError(
            "release.registry_signing_credential_invalid",
            "Production Vault signer identity could not be verified.",
            {"reason": "materialized Vault CA certificate was unavailable"},
        ) from None

    connection: http.client.HTTPSConnection | None = None
    try:
        context = ssl.create_default_context(cafile=str(resolved_cacert))
        connection = http.client.HTTPSConnection(
            authority,
            timeout=_remaining(deadline, maximum=VAULT_TOKEN_LOOKUP_TIMEOUT_SECONDS),
            context=context,
        )
        connection.request(
            "GET",
            VAULT_TOKEN_LOOKUP_PATH,
            headers={
                "Accept": "application/json",
                "X-Vault-Token": vault_token,
            },
        )
        response = connection.getresponse()
        body = response.read(VAULT_TOKEN_LOOKUP_MAX_RESPONSE_BYTES + 1)
        status = response.status
    except common.ReleaseGateError:
        raise
    except (OSError, ValueError, http.client.HTTPException):
        raise common.ReleaseGateError(
            "release.registry_signing_credential_invalid",
            "Production Vault signer identity lookup failed.",
            {"reason": "Vault HTTPS lookup failed"},
        ) from None
    finally:
        if connection is not None:
            try:
                connection.close()
            except (OSError, http.client.HTTPException):
                pass

    if status != 200:
        raise common.ReleaseGateError(
            "release.registry_signing_credential_invalid",
            "Production Vault signer identity lookup failed.",
            {"reason": "Vault returned an unexpected status", "status": status},
        )
    if not isinstance(body, bytes) or len(body) > VAULT_TOKEN_LOOKUP_MAX_RESPONSE_BYTES:
        raise common.ReleaseGateError(
            "release.registry_signing_credential_invalid",
            "Production Vault signer identity lookup failed.",
            {"reason": "Vault response body was invalid"},
        )
    try:
        payload = json.loads(body)
    except (json.JSONDecodeError, UnicodeDecodeError):
        raise common.ReleaseGateError(
            "release.registry_signing_credential_invalid",
            "Production Vault signer identity lookup failed.",
            {"reason": "Vault returned malformed JSON"},
        ) from None
    data = payload.get("data") if isinstance(payload, dict) else None
    metadata = data.get("meta") if isinstance(data, dict) else None
    policies = data.get("policies") if isinstance(data, dict) else None
    if not isinstance(data, dict) or not isinstance(metadata, dict) or not isinstance(policies, list):
        raise common.ReleaseGateError(
            "release.registry_signing_credential_invalid",
            "Production Vault signer identity was invalid.",
            {"reason": "Vault token identity response was malformed"},
        )
    expected_policies = [role_name]
    identity_fields_match = (
        data.get("display_name") == VAULT_TRANSIT_AUTH_METHOD
        and metadata.get("role_name") == role_name
        and data.get("type") == "batch"
        and data.get("orphan") is True
        and policies == expected_policies
    )
    if not identity_fields_match:
        raise common.ReleaseGateError(
            "release.registry_signing_credential_invalid",
            "Production Vault signer identity did not match the frozen AppRole boundary.",
            {"reason": "Vault token identity did not match the production signer"},
        )
    policies_sha256 = _sha256(
        json.dumps(expected_policies, separators=(",", ":"), sort_keys=True).encode("utf-8")
    )
    return {
        "verified": True,
        "displayName": VAULT_TRANSIT_AUTH_METHOD,
        "roleName": role_name,
        "type": "batch",
        "orphan": True,
        "policyCount": len(expected_policies),
        "policiesSha256": policies_sha256,
    }


def _export_cosign_public_key(
    options: SupplyChainOptions,
    configuration: SupplyChainConfiguration,
    *,
    key_reference: str,
    secret_environment: Mapping[str, str],
    deadline: float,
    redactor: acceptance.SecretRedactor,
) -> tuple[str, pathlib.Path]:
    completed = _run_tool(
        options,
        image=configuration.tools.cosign,
        arguments=["public-key", "--key", key_reference],
        tool="cosign",
        deadline=deadline,
        redactor=redactor,
        secret_environment=secret_environment,
    )
    public_key_sha256 = _public_key_sha256(
        completed.stdout,
        code="release.registry_production_admission_input_invalid",
        message="Worker Registry production admission inputs were invalid.",
        field="kmsPublicKey",
    )
    public_key_path = options.state_dir / MATERIALIZED_KMS_PUBLIC_KEY_RELATIVE_PATH
    public_key_path.parent.mkdir(parents=True, exist_ok=True)
    normalized_public_key = completed.stdout.strip().replace("\r\n", "\n").replace("\r", "\n") + "\n"
    try:
        public_key_path.write_text(normalized_public_key, encoding="utf-8")
        public_key_path.chmod(0o600)
    except OSError:
        public_key_path.unlink(missing_ok=True)
        raise common.ReleaseGateError(
            "release.registry_production_admission_input_invalid",
            "Worker Registry production admission inputs were invalid.",
            {"reason": "KMS public key could not be materialized in isolated gate state"},
        ) from None
    return public_key_sha256, MATERIALIZED_KMS_PUBLIC_KEY_RELATIVE_PATH


def _validate_runtime_admission_inputs(
    options: SupplyChainOptions,
    configuration: SupplyChainConfiguration,
    *,
    key_reference: str,
    secret_environment: Mapping[str, str],
    deadline: float,
    redactor: acceptance.SecretRedactor,
) -> tuple[dict[str, Any], pathlib.Path | None]:
    profile = configuration.production_signing_profile
    if profile is None:
        return {}, None
    public_key_path = options.production_public_key_configmap_path
    repository_path = options.production_repository_configmap_path
    if public_key_path is None or repository_path is None:
        raise common.ReleaseGateError(
            "release.registry_production_admission_input_invalid",
            "Worker Registry production admission inputs were invalid.",
            {"reason": "runtime ConfigMap paths were not configured"},
        )
    public_key_manifest = _read_runtime_manifest(
        public_key_path,
        expected_name=profile.public_key_configmap_name,
        expected_namespace=profile.public_key_configmap_namespace,
        expected_key=profile.public_key_configmap_key,
        code="release.registry_production_admission_input_invalid",
        message="Worker Registry production admission inputs were invalid.",
    )
    repository_manifest = _read_runtime_manifest(
        repository_path,
        expected_name=profile.repository_configmap_name,
        expected_namespace=profile.repository_configmap_namespace,
        expected_key=profile.repository_configmap_key,
        code="release.registry_production_admission_input_invalid",
        message="Worker Registry production admission inputs were invalid.",
    )
    runtime_public_key_sha256 = _public_key_sha256(
        public_key_manifest.data[profile.public_key_configmap_key],
        code="release.registry_production_admission_input_invalid",
        message="Worker Registry production admission inputs were invalid.",
        field="admission.publicKeyConfigMap.data",
    )
    kms_public_key_sha256, verification_key_path = _export_cosign_public_key(
        options,
        configuration,
        key_reference=key_reference,
        secret_environment=secret_environment,
        deadline=deadline,
        redactor=redactor,
    )
    if runtime_public_key_sha256 != kms_public_key_sha256:
        raise common.ReleaseGateError(
            "release.registry_production_admission_input_invalid",
            "Worker Registry production admission inputs were invalid.",
            {
                "reason": "runtime public key fingerprint did not match the configured KMS key",
                "path": str(public_key_path),
            },
        )
    repository_pattern = _validate_runtime_repository_pattern(
        repository_manifest.data[profile.repository_configmap_key],
        image_repository=options.image_repository,
        code="release.registry_production_admission_input_invalid",
        message="Worker Registry production admission inputs were invalid.",
        field="admission.repositoryConfigMap.data",
    )
    return (
        {
            "provider": profile.admission_provider,
            "runtimeValidated": True,
            "publicKeyConfigMapPath": str(public_key_path),
            "repositoryConfigMapPath": str(repository_path),
            "publicKeyConfigMap": {
                "namespace": profile.public_key_configmap_namespace,
                "name": profile.public_key_configmap_name,
                "key": profile.public_key_configmap_key,
                "sha256": runtime_public_key_sha256,
                "kmsSha256": kms_public_key_sha256,
            },
            "repositoryConfigMap": {
                "namespace": profile.repository_configmap_namespace,
                "name": profile.repository_configmap_name,
                "key": profile.repository_configmap_key,
                "pattern": repository_pattern,
            },
        },
        verification_key_path,
    )


def _sign_and_verify_ephemeral(
    options: SupplyChainOptions,
    configuration: SupplyChainConfiguration,
    *,
    builds: Sequence[Mapping[str, Any]],
    git_sha: str,
    version: str,
    run_id: str,
    deadline: float,
    redactor: acceptance.SecretRedactor,
) -> dict[str, Any]:
    cosign_dir = options.state_dir / "cosign"
    cosign_dir.mkdir(parents=True, exist_ok=True)
    signing_config = pathlib.Path("cosign/signing-config.json")
    key_prefix = pathlib.Path("cosign/ephemeral")
    private_key = options.state_dir / "cosign" / "ephemeral.key"
    public_key = options.state_dir / "cosign" / "ephemeral.pub"
    passphrase = secrets.token_urlsafe(48)
    redactor.add(passphrase, "[REDACTED_EPHEMERAL_COSIGN_PASSWORD]")
    secret_environment = {"COSIGN_PASSWORD": passphrase}
    signatures: list[dict[str, Any]] = []
    public_key_sha256: str | None = None
    try:
        _run_tool(
            options,
            image=configuration.tools.cosign,
            arguments=[
                "signing-config",
                "create",
                "--no-default-fulcio",
                "--no-default-oidc",
                "--no-default-rekor",
                "--no-default-tsa",
                "--out",
                str(signing_config),
            ],
            tool="cosign",
            deadline=deadline,
            redactor=redactor,
        )
        _run_tool(
            options,
            image=configuration.tools.cosign,
            arguments=["generate-key-pair", "--output-key-prefix", str(key_prefix)],
            tool="cosign",
            deadline=deadline,
            redactor=redactor,
            secret_environment=secret_environment,
        )
        if not private_key.is_file() or not public_key.is_file():
            raise common.ReleaseGateError(
                "release.registry_signature_key_invalid",
                "Cosign did not create the isolated ephemeral key pair.",
            )
        public_key_sha256 = _sha256(public_key.read_bytes())
        for subject in _signature_subjects(
            options,
            builds=builds,
            git_sha=git_sha,
            version=version,
            run_id=run_id,
        ):
            insecure_arguments = ["--allow-insecure-registry"] if options.insecure_registry else []
            _run_tool(
                options,
                image=configuration.tools.cosign,
                arguments=[
                    "sign",
                    "--yes",
                    "--signing-config",
                    str(signing_config),
                    *insecure_arguments,
                    "--key",
                    str(key_prefix.with_suffix(".key")),
                    *subject["annotationArguments"],
                    subject["reference"],
                ],
                tool="cosign",
                deadline=deadline,
                redactor=redactor,
                secret_environment=secret_environment,
            )
            verification = _run_tool(
                options,
                image=configuration.tools.cosign,
                arguments=[
                    "verify",
                    *insecure_arguments,
                    "--insecure-ignore-tlog=true",
                    "--key",
                    str(key_prefix.with_suffix(".pub")),
                    *subject["annotationArguments"],
                    "--output",
                    "json",
                    subject["reference"],
                ],
                tool="cosign",
                deadline=deadline,
                redactor=redactor,
            )
            signatures.append(_verification_evidence(verification, subject=subject))
    finally:
        private_key.unlink(missing_ok=True)
    private_key_removed = not private_key.exists()
    if not private_key_removed:
        raise common.ReleaseGateError(
            "release.registry_signature_key_cleanup_failed",
            "Worker Registry supply-chain gate did not remove its ephemeral private key.",
        )
    return {
        "mode": "ephemeral-key",
        "transparencyLog": False,
        "productionSigningPolicySatisfied": False,
        "policySha256": configuration.signing_policy.sha256,
        "publicKeySha256": public_key_sha256,
        "signatures": signatures,
        "privateKeyRemoved": private_key_removed,
    }


def _keyless_verification_arguments(policy: SigningPolicy) -> list[str]:
    identity_arguments = (
        ["--certificate-identity", policy.certificate_identity]
        if policy.certificate_identity is not None
        else ["--certificate-identity-regexp", policy.certificate_identity_regexp]
    )
    issuer_arguments = (
        ["--certificate-oidc-issuer", policy.certificate_oidc_issuer]
        if policy.certificate_oidc_issuer is not None
        else ["--certificate-oidc-issuer-regexp", policy.certificate_oidc_issuer_regexp]
    )
    return [str(value) for value in (*identity_arguments, *issuer_arguments)]


def _sign_and_verify_keyless(
    options: SupplyChainOptions,
    configuration: SupplyChainConfiguration,
    *,
    builds: Sequence[Mapping[str, Any]],
    git_sha: str,
    version: str,
    run_id: str,
    deadline: float,
    redactor: acceptance.SecretRedactor,
) -> dict[str, Any]:
    _require_production_registry(options)
    _require_production_registry_access_environment_names(options, configuration)
    policy = configuration.signing_policy
    require_inclusion_proof, require_signed_entry_timestamp = _transparency_log_requirements(
        configuration
    )
    token_environment = policy.identity_token_environment
    if token_environment is None:
        raise common.ReleaseGateError(
            "release.registry_signing_policy_invalid",
            "Worker Registry keyless signing policy omitted its identity token environment.",
        )
    try:
        identity_token = acceptance.read_environment_value(
            token_environment,
            "Cosign keyless identity token",
            maximum_length=1 << 20,
            forbidden_characters="\r\n\x00",
        )
    except acceptance.EnvironmentValueError as error:
        raise common.ReleaseGateError(
            "release.registry_signing_credential_invalid",
            "The configured Cosign keyless identity token was unavailable or invalid.",
            {"reason": error.reason},
        ) from None
    redactor.add(identity_token, "[REDACTED_COSIGN_IDENTITY_TOKEN]")
    token_relative = pathlib.Path("cosign/identity-token")
    token_path = options.state_dir / token_relative
    token_path.parent.mkdir(parents=True, exist_ok=True)
    try:
        token_path.write_text(identity_token, encoding="utf-8")
        token_path.chmod(0o600)
    except OSError:
        token_path.unlink(missing_ok=True)
        raise common.ReleaseGateError(
            "release.registry_signing_credential_invalid",
            "Worker Registry gate could not create isolated keyless identity-token state.",
        ) from None
    signatures: list[dict[str, Any]] = []
    try:
        for subject in _signature_subjects(
            options,
            builds=builds,
            git_sha=git_sha,
            version=version,
            run_id=run_id,
        ):
            bundle_relative = pathlib.Path(f"cosign/{subject['slot']}.bundle.json")
            bundle_path = options.state_dir / bundle_relative
            bundle_path.parent.mkdir(parents=True, exist_ok=True)
            _run_tool(
                options,
                image=configuration.tools.cosign,
                arguments=[
                    "sign",
                    "--yes",
                    "--tlog-upload=true",
                    "--bundle",
                    str(bundle_relative),
                    "--identity-token",
                    str(token_relative),
                    *subject["annotationArguments"],
                    subject["reference"],
                ],
                tool="cosign",
                deadline=deadline,
                redactor=redactor,
            )
            verification = _run_tool(
                options,
                image=configuration.tools.cosign,
                arguments=[
                    "verify",
                    "--insecure-ignore-tlog=false",
                    *_keyless_verification_arguments(policy),
                    *subject["annotationArguments"],
                    "--output",
                    "json",
                    subject["reference"],
                ],
                tool="cosign",
                deadline=deadline,
                redactor=redactor,
            )
            signatures.append(
                {
                    **_verification_evidence(verification, subject=subject),
                    "transparencyLog": _signature_bundle_evidence(
                        bundle_path,
                        require_inclusion_proof=require_inclusion_proof,
                        require_signed_entry_timestamp=require_signed_entry_timestamp,
                    ),
                }
            )
    finally:
        token_path.unlink(missing_ok=True)
    token_removed = not token_path.exists()
    if not token_removed:
        raise common.ReleaseGateError(
            "release.registry_signature_key_cleanup_failed",
            "Worker Registry supply-chain gate did not remove its keyless identity token.",
        )
    return {
        "mode": "keyless",
        "transparencyLog": True,
        "transparencyLogVerified": all(
            isinstance(signature.get("transparencyLog"), dict)
            and signature["transparencyLog"].get("verificationMode")
            == "cosign-online-tlog-verification"
            for signature in signatures
        ),
        "transparencyLogInclusionProofPresent": all(
            isinstance(signature.get("transparencyLog"), dict)
            and signature["transparencyLog"].get("inclusionProofPresent") is True
            for signature in signatures
        ),
        "transparencyLogSignedEntryTimestampPresent": all(
            isinstance(signature.get("transparencyLog"), dict)
            and signature["transparencyLog"].get("signedEntryTimestampPresent") is True
            for signature in signatures
        ),
        "productionSigningPolicySatisfied": True,
        "policySha256": policy.sha256,
        **{
            key: value
            for key, value in policy.as_report().items()
            if key.startswith("certificate")
        },
        "signatures": signatures,
        "identityTokenRemoved": token_removed,
    }


def _sign_and_verify_kms(
    options: SupplyChainOptions,
    configuration: SupplyChainConfiguration,
    *,
    builds: Sequence[Mapping[str, Any]],
    git_sha: str,
    version: str,
    run_id: str,
    deadline: float,
    redactor: acceptance.SecretRedactor,
) -> dict[str, Any]:
    _require_production_registry(options)
    _require_production_registry_access_environment_names(options, configuration)
    policy = configuration.signing_policy
    require_inclusion_proof, require_signed_entry_timestamp = _transparency_log_requirements(
        configuration
    )
    key_reference = policy.key_reference
    if key_reference is None:
        raise common.ReleaseGateError(
            "release.registry_signing_policy_invalid",
            "Worker Registry KMS signing policy omitted its key reference.",
        )
    prepared_environment = _prepare_kms_environment(
        options,
        policy,
        redactor=redactor,
    )
    secret_environment = prepared_environment.secret_environment
    admission, verification_key_path = _validate_runtime_admission_inputs(
        options,
        configuration,
        key_reference=key_reference,
        secret_environment=secret_environment,
        deadline=deadline,
        redactor=redactor,
    )
    signer_identity = _production_vault_signer_identity(
        options,
        configuration,
        prepared_environment=prepared_environment,
        deadline=deadline,
    )
    signatures: list[dict[str, Any]] = []
    for subject in _signature_subjects(
        options,
        builds=builds,
        git_sha=git_sha,
        version=version,
        run_id=run_id,
    ):
        bundle_relative = pathlib.Path(f"cosign/{subject['slot']}.bundle.json")
        bundle_path = options.state_dir / bundle_relative
        bundle_path.parent.mkdir(parents=True, exist_ok=True)
        _run_tool(
            options,
            image=configuration.tools.cosign,
            arguments=[
                "sign",
                "--yes",
                "--tlog-upload=true",
                "--bundle",
                str(bundle_relative),
                "--key",
                key_reference,
                *subject["annotationArguments"],
                subject["reference"],
            ],
            tool="cosign",
            deadline=deadline,
            redactor=redactor,
            secret_environment=secret_environment,
        )
        verification_key = (
            verification_key_path.as_posix() if verification_key_path is not None else key_reference
        )
        verification = _run_tool(
            options,
            image=configuration.tools.cosign,
            arguments=[
                "verify",
                "--insecure-ignore-tlog=false",
                "--key",
                verification_key,
                *subject["annotationArguments"],
                "--output",
                "json",
                subject["reference"],
            ],
            tool="cosign",
            deadline=deadline,
            redactor=redactor,
            secret_environment=(secret_environment if verification_key_path is None else None),
        )
        signatures.append(
            {
                **_verification_evidence(verification, subject=subject),
                "transparencyLog": _signature_bundle_evidence(
                    bundle_path,
                    require_inclusion_proof=require_inclusion_proof,
                    require_signed_entry_timestamp=require_signed_entry_timestamp,
                ),
            }
        )
    return {
        "mode": "kms-key",
        "transparencyLog": True,
        "transparencyLogVerified": all(
            isinstance(signature.get("transparencyLog"), dict)
            and signature["transparencyLog"].get("verificationMode")
            == "cosign-online-tlog-verification"
            for signature in signatures
        ),
        "transparencyLogInclusionProofPresent": all(
            isinstance(signature.get("transparencyLog"), dict)
            and signature["transparencyLog"].get("inclusionProofPresent") is True
            for signature in signatures
        ),
        "transparencyLogSignedEntryTimestampPresent": all(
            isinstance(signature.get("transparencyLog"), dict)
            and signature["transparencyLog"].get("signedEntryTimestampPresent") is True
            for signature in signatures
        ),
        "productionSigningPolicySatisfied": True,
        "productionAdmissionValidated": bool(admission),
        "verificationKeyMode": (
            "kms-exported-public-key" if verification_key_path is not None else "kms-reference"
        ),
        "policySha256": policy.sha256,
        "keyReference": key_reference,
        "credentialEnvironmentCount": len(policy.credential_environment),
        "credentialEnvironmentNames": list(policy.credential_environment),
        "vaultCaMaterialized": prepared_environment.vault_ca_materialized,
        "signerIdentity": signer_identity,
        "admission": admission,
        "signatures": signatures,
    }


def _sign_and_verify(
    options: SupplyChainOptions,
    configuration: SupplyChainConfiguration,
    *,
    builds: Sequence[Mapping[str, Any]],
    git_sha: str,
    version: str,
    run_id: str,
    deadline: float,
    redactor: acceptance.SecretRedactor,
) -> dict[str, Any]:
    policy = configuration.signing_policy
    if policy.mode == "ephemeral-key":
        return _sign_and_verify_ephemeral(
            options,
            configuration,
            builds=builds,
            git_sha=git_sha,
            version=version,
            run_id=run_id,
            deadline=deadline,
            redactor=redactor,
        )
    if policy.mode == "keyless":
        return _sign_and_verify_keyless(
            options,
            configuration,
            builds=builds,
            git_sha=git_sha,
            version=version,
            run_id=run_id,
            deadline=deadline,
            redactor=redactor,
        )
    if policy.mode == "kms-key":
        return _sign_and_verify_kms(
            options,
            configuration,
            builds=builds,
            git_sha=git_sha,
            version=version,
            run_id=run_id,
            deadline=deadline,
            redactor=redactor,
        )
    raise common.ReleaseGateError(
        "release.registry_signing_policy_invalid",
        "Worker Registry signing policy selected an unsupported mode.",
    )


def _vulnerability_summary(vulnerabilities: Sequence[Mapping[str, Any]]) -> dict[str, Any]:
    by_severity = {severity: 0 for severity in SUPPORTED_SEVERITIES}
    fixable = 0
    for vulnerability in vulnerabilities:
        severity = vulnerability.get("Severity")
        if severity in by_severity:
            by_severity[str(severity)] += 1
        fixed_version = vulnerability.get("FixedVersion")
        if isinstance(fixed_version, str) and fixed_version.strip():
            fixable += 1
    return {
        "total": len(vulnerabilities),
        "fixable": fixable,
        "unfixed": len(vulnerabilities) - fixable,
        "bySeverity": by_severity,
    }


def _exception_matches(
    exception: VulnerabilityException,
    vulnerability: Mapping[str, Any],
    *,
    platform: str,
) -> bool:
    vulnerability_id = vulnerability.get("VulnerabilityID")
    package = vulnerability.get("PkgName")
    return (
        isinstance(vulnerability_id, str)
        and vulnerability_id.upper() == exception.vulnerability_id
        and package == exception.package
        and platform == exception.platform
    )


def _safe_vulnerability(vulnerability: Mapping[str, Any]) -> dict[str, Any]:
    return {
        "vulnerabilityId": vulnerability.get("VulnerabilityID"),
        "package": vulnerability.get("PkgName"),
        "installedVersion": vulnerability.get("InstalledVersion"),
        "fixedVersion": vulnerability.get("FixedVersion"),
        "severity": vulnerability.get("Severity"),
        "status": vulnerability.get("Status"),
        "primaryUrl": vulnerability.get("PrimaryURL"),
        "target": vulnerability.get("_Target"),
        "class": vulnerability.get("_Class"),
        "type": vulnerability.get("_Type"),
    }


def evaluate_trivy_report(
    payload: Any,
    *,
    platform: str,
    reference: str,
    policy: VulnerabilityPolicy,
    now: dt.datetime,
) -> tuple[dict[str, Any], list[dict[str, Any]], set[tuple[str, str, str]]]:
    if not isinstance(payload, dict) or payload.get("SchemaVersion") != 2:
        raise common.ReleaseGateError(
            "release.registry_vulnerability_report_invalid",
            "Trivy did not produce the expected JSON report schema.",
            {"platform": platform},
        )
    metadata = payload.get("Metadata")
    repo_digests = metadata.get("RepoDigests") if isinstance(metadata, dict) else None
    if (
        payload.get("ArtifactName") != reference
        or payload.get("ArtifactType") != "container_image"
        or not isinstance(repo_digests, list)
        or reference not in repo_digests
    ):
        raise common.ReleaseGateError(
            "release.registry_vulnerability_report_invalid",
            "Trivy report identity did not match the requested immutable platform digest.",
            {"platform": platform},
        )
    results = payload.get("Results")
    if not isinstance(results, list) or not all(isinstance(item, dict) for item in results):
        raise common.ReleaseGateError(
            "release.registry_vulnerability_report_invalid",
            "Trivy report omitted its result list.",
            {"platform": platform},
        )
    vulnerabilities = [
        {
            **vulnerability,
            "_Target": result.get("Target"),
            "_Class": result.get("Class"),
            "_Type": result.get("Type"),
        }
        for result in results
        for vulnerability in (result.get("Vulnerabilities") or [])
        if isinstance(vulnerability, dict)
    ]
    secret_findings = [
        secret
        for result in results
        for secret in (result.get("Secrets") or [])
        if isinstance(secret, dict)
    ]
    errors: list[dict[str, Any]] = []
    used_exceptions: set[tuple[str, str, str]] = set()
    blocked: list[dict[str, Any]] = []
    waived: list[dict[str, Any]] = []
    expired: list[dict[str, Any]] = []
    for vulnerability in vulnerabilities:
        severity = vulnerability.get("Severity")
        fixed_version = vulnerability.get("FixedVersion")
        if severity not in policy.blocked_severities:
            continue
        if policy.ignore_unfixed and not (isinstance(fixed_version, str) and fixed_version.strip()):
            continue
        matching = [
            exception
            for exception in policy.exceptions
            if _exception_matches(exception, vulnerability, platform=platform)
        ]
        active = [exception for exception in matching if exception.expires_at > now]
        finding = _safe_vulnerability(vulnerability)
        if active:
            exception = active[0]
            used_exceptions.add(exception.identity)
            waived.append(
                {
                    **finding,
                    "owner": exception.owner,
                    "expiresAt": exception.expires_at.isoformat().replace("+00:00", "Z"),
                    "reason": exception.reason,
                }
            )
        else:
            blocked.append(finding)
            for exception in matching:
                used_exceptions.add(exception.identity)
                expired.append(
                    {
                        "vulnerabilityId": exception.vulnerability_id,
                        "package": exception.package,
                        "platform": exception.platform,
                        "expiresAt": exception.expires_at.isoformat().replace("+00:00", "Z"),
                    }
                )
    os_metadata = metadata.get("OS") if isinstance(metadata, dict) else None
    end_of_life = os_metadata.get("EOSL") if isinstance(os_metadata, dict) else None
    if policy.fail_on_end_of_life_os and end_of_life is True:
        errors.append(
            {
                "code": "release.registry_vulnerability_os_eol",
                "message": "Worker Registry image used an end-of-life operating-system release.",
                "evidence": {"platform": platform, "os": os_metadata},
            }
        )
    if secret_findings:
        safe_findings = [
            {
                key: finding.get(key)
                for key in ("RuleID", "Category", "Title", "Target", "StartLine", "EndLine")
                if key in finding
            }
            for finding in secret_findings[:50]
        ]
        errors.append(
            {
                "code": "release.registry_image_secret_detected",
                "message": "Trivy found Secret-like material in the Worker Registry image.",
                "evidence": {
                    "platform": platform,
                    "findingCount": len(secret_findings),
                    "findings": safe_findings,
                },
            }
        )
    if expired:
        errors.append(
            {
                "code": "release.registry_vulnerability_exception_expired",
                "message": "Worker Registry vulnerability policy contained an expired matching exception.",
                "evidence": {"platform": platform, "exceptions": expired},
            }
        )
    if blocked:
        errors.append(
            {
                "code": "release.registry_vulnerability_policy_blocked",
                "message": "Worker Registry image violated the checked-in vulnerability policy.",
                "evidence": {
                    "platform": platform,
                    "findingCount": len(blocked),
                    "findings": blocked[:100],
                },
            }
        )
    evidence = {
        "platform": platform,
        "reference": reference,
        "artifactId": metadata.get("ImageID") if isinstance(metadata, dict) else None,
        "os": os_metadata,
        "vulnerabilities": _vulnerability_summary(vulnerabilities),
        "reviewFindings": sorted(
            (
                _safe_vulnerability(vulnerability)
                for vulnerability in vulnerabilities
                if vulnerability.get("Severity") in {"UNKNOWN", "HIGH", "CRITICAL"}
            ),
            key=lambda finding: (
                str(finding.get("severity")),
                str(finding.get("vulnerabilityId")),
                str(finding.get("package")),
            ),
        ),
        "reviewFindingCount": sum(
            1
            for vulnerability in vulnerabilities
            if vulnerability.get("Severity") in {"UNKNOWN", "HIGH", "CRITICAL"}
        ),
        "blockedFindings": blocked,
        "waivedFindings": waived,
        "secretFindingCount": len(secret_findings),
        "reportSha256": _sha256(
            json.dumps(payload, sort_keys=True, separators=(",", ":")).encode("utf-8")
        ),
    }
    return evidence, errors, used_exceptions


def evaluate_trivy_database(
    payload: Any,
    *,
    expected_version: str,
    policy: VulnerabilityPolicy,
    now: dt.datetime,
) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    if not isinstance(payload, dict) or payload.get("Version") != expected_version:
        raise common.ReleaseGateError(
            "release.registry_vulnerability_database_invalid",
            "Trivy version output did not match the checked-in tool lock.",
        )
    database = payload.get("VulnerabilityDB")
    if not isinstance(database, dict):
        raise common.ReleaseGateError(
            "release.registry_vulnerability_database_invalid",
            "Trivy did not report vulnerability database metadata.",
        )
    updated_at = _parse_timestamp(database.get("UpdatedAt"), field="VulnerabilityDB.UpdatedAt")
    age_seconds = (now - updated_at).total_seconds()
    errors: list[dict[str, Any]] = []
    if age_seconds < -300 or age_seconds > policy.maximum_database_age_hours * 3600:
        errors.append(
            {
                "code": "release.registry_vulnerability_database_stale",
                "message": "Trivy vulnerability database was outside the checked-in freshness policy.",
                "evidence": {
                    "updatedAt": updated_at.isoformat().replace("+00:00", "Z"),
                    "ageSeconds": int(age_seconds),
                    "maximumAgeHours": policy.maximum_database_age_hours,
                },
            }
        )
    return (
        {
            "toolVersion": expected_version,
            "schemaVersion": database.get("Version"),
            "updatedAt": updated_at.isoformat().replace("+00:00", "Z"),
            "nextUpdate": database.get("NextUpdate"),
            "downloadedAt": database.get("DownloadedAt"),
            "ageSeconds": int(age_seconds),
            "maximumAgeHours": policy.maximum_database_age_hours,
        },
        errors,
    )


def _scan_platforms(
    options: SupplyChainOptions,
    configuration: SupplyChainConfiguration,
    *,
    platform_digests: Mapping[str, Any],
    deadline: float,
    redactor: acceptance.SecretRedactor,
) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    policy = configuration.vulnerability_policy
    scans: list[dict[str, Any]] = []
    errors: list[dict[str, Any]] = []
    used_exceptions: set[tuple[str, str, str]] = set()
    transient_database_download_retries = 0
    now = dt.datetime.now(tz=dt.timezone.utc)
    for platform in ("linux/amd64", "linux/arm64"):
        digest = platform_digests.get(platform)
        if re.fullmatch(r"sha256:[0-9a-f]{64}", str(digest)) is None:
            raise common.ReleaseGateError(
                "release.registry_vulnerability_input_invalid",
                "Worker Registry vulnerability scan omitted a required platform digest.",
                {"platform": platform},
            )
        reference = f"{options.image_repository}@{digest}"
        report_name = f"trivy-{platform.replace('/', '-')}.json"
        report_path = options.state_dir / report_name
        insecure_arguments = ["--insecure"] if options.insecure_registry else []
        scan_timeout = int(_remaining(deadline, maximum=1200.0))
        arguments = [
            "image",
            "--quiet",
            "--image-src",
            "remote",
            *insecure_arguments,
            "--skip-version-check",
            "--cache-dir",
            "/workspace/trivy-cache",
            "--timeout",
            f"{max(60, scan_timeout)}s",
            "--scanners",
            "vuln,secret",
            "--severity",
            ",".join(SUPPORTED_SEVERITIES),
            "--format",
            "json",
            "--output",
            f"/workspace/{report_name}",
        ]
        if policy.ignore_unfixed:
            arguments.append("--ignore-unfixed")
        arguments.append(reference)
        transient_database_download_retries += _run_trivy_scan(
            options,
            configuration,
            arguments=arguments,
            report_path=report_path,
            deadline=deadline,
            redactor=redactor,
        )
        payload = _load_json_file(
            report_path,
            code="release.registry_vulnerability_report_invalid",
            message="Trivy did not write a valid Worker Registry vulnerability report.",
        )
        evidence, platform_errors, platform_exceptions = evaluate_trivy_report(
            payload,
            platform=platform,
            reference=reference,
            policy=policy,
            now=now,
        )
        scans.append(evidence)
        errors.extend(platform_errors)
        used_exceptions.update(platform_exceptions)
        report_path.unlink(missing_ok=True)
    stale_exceptions = [
        {
            "vulnerabilityId": exception.vulnerability_id,
            "package": exception.package,
            "platform": exception.platform,
            "expiresAt": exception.expires_at.isoformat().replace("+00:00", "Z"),
            "owner": exception.owner,
        }
        for exception in policy.exceptions
        if exception.identity not in used_exceptions
    ]
    if stale_exceptions:
        errors.append(
            {
                "code": "release.registry_vulnerability_exception_stale",
                "message": "Worker Registry vulnerability policy contained an unused exception.",
                "evidence": {"exceptions": stale_exceptions},
            }
        )
    version = _run_tool(
        options,
        image=configuration.tools.trivy,
        arguments=[
            "--cache-dir",
            "/workspace/trivy-cache",
            "--version",
            "--format",
            "json",
        ],
        tool="trivy",
        deadline=deadline,
        redactor=redactor,
    )
    try:
        version_payload = json.loads(version.stdout)
    except json.JSONDecodeError:
        raise common.ReleaseGateError(
            "release.registry_vulnerability_database_invalid",
            "Trivy version output was not valid JSON.",
        ) from None
    expected_version = _locked_version(configuration.tools.trivy, tool="trivy").removeprefix("v")
    database, database_errors = evaluate_trivy_database(
        version_payload,
        expected_version=expected_version,
        policy=policy,
        now=now,
    )
    errors.extend(database_errors)
    return (
        {
            "policy": policy.as_report(),
            "database": database,
            "scans": scans,
            "staleExceptionCount": len(stale_exceptions),
            "transientDatabaseDownloadRetries": transient_database_download_retries,
        },
        errors,
    )


def _tool_versions(
    options: SupplyChainOptions,
    configuration: SupplyChainConfiguration,
    *,
    deadline: float,
    redactor: acceptance.SecretRedactor,
) -> dict[str, str]:
    cosign = _run_tool(
        options,
        image=configuration.tools.cosign,
        arguments=["version"],
        tool="cosign",
        deadline=deadline,
        redactor=redactor,
        maximum_timeout=300.0,
    )
    match = re.search(r"(?m)^GitVersion:\s+(v[0-9]+\.[0-9]+\.[0-9]+)\s*$", cosign.stdout)
    expected_cosign = _locked_version(configuration.tools.cosign, tool="cosign")
    if match is None or match.group(1) != expected_cosign:
        raise common.ReleaseGateError(
            "release.registry_supply_chain_tool_version_invalid",
            "Cosign runtime version did not match the checked-in digest lock.",
        )
    trivy = _run_tool(
        options,
        image=configuration.tools.trivy,
        arguments=["--version", "--format", "json"],
        tool="trivy",
        deadline=deadline,
        redactor=redactor,
        maximum_timeout=300.0,
    )
    try:
        trivy_payload = json.loads(trivy.stdout)
    except json.JSONDecodeError:
        trivy_payload = None
    expected_trivy = _locked_version(configuration.tools.trivy, tool="trivy").removeprefix("v")
    if not isinstance(trivy_payload, dict) or trivy_payload.get("Version") != expected_trivy:
        raise common.ReleaseGateError(
            "release.registry_supply_chain_tool_version_invalid",
            "Trivy runtime version did not match the checked-in digest lock.",
        )
    return {"cosign": expected_cosign, "trivy": expected_trivy}


def verify_supply_chain(
    options: SupplyChainOptions,
    configuration: SupplyChainConfiguration,
    *,
    builds: Sequence[Mapping[str, Any]],
    git_sha: str,
    version: str,
    run_id: str,
    redactor: acceptance.SecretRedactor,
) -> dict[str, Any]:
    started = time.monotonic()
    deadline = started + options.timeout_seconds
    errors: list[dict[str, Any]] = []
    versions: dict[str, str] = {}
    signing: dict[str, Any] = {}
    vulnerability: dict[str, Any] = {}
    try:
        versions = _tool_versions(
            options,
            configuration,
            deadline=deadline,
            redactor=redactor,
        )
    except common.ReleaseGateError as error:
        errors.append(error.as_report_error())
    tools_ready = not errors
    if tools_ready:
        try:
            signing = _sign_and_verify(
                options,
                configuration,
                builds=builds,
                git_sha=git_sha,
                version=version,
                run_id=run_id,
                deadline=deadline,
                redactor=redactor,
            )
        except common.ReleaseGateError as error:
            errors.append(error.as_report_error())
    platform_digests = builds[0].get("platformDigests") if builds else None
    if tools_ready and isinstance(platform_digests, dict):
        try:
            vulnerability, vulnerability_errors = _scan_platforms(
                options,
                configuration,
                platform_digests=platform_digests,
                deadline=deadline,
                redactor=redactor,
            )
            errors.extend(vulnerability_errors)
        except common.ReleaseGateError as error:
            errors.append(error.as_report_error())
    private_key = options.state_dir / "cosign" / "ephemeral.key"
    identity_token = options.state_dir / "cosign" / "identity-token"
    vault_cacert = options.state_dir / MATERIALIZED_VAULT_CACERT_RELATIVE_PATH
    registry_paths = _registry_state_paths(options)
    private_key.unlink(missing_ok=True)
    identity_token.unlink(missing_ok=True)
    vault_cacert.unlink(missing_ok=True)
    registry_paths["docker_config"].unlink(missing_ok=True)
    registry_paths["registry_ca"].unlink(missing_ok=True)
    private_key_removed = not private_key.exists()
    identity_token_removed = not identity_token.exists()
    vault_cacert_removed = not vault_cacert.exists()
    registry_auth_config_removed = not registry_paths["docker_config"].exists()
    registry_ca_removed = not registry_paths["registry_ca"].exists()
    signing_secret_state_removed = private_key_removed and identity_token_removed
    if signing:
        signing["privateKeyRemoved"] = private_key_removed
        signing["identityTokenRemoved"] = identity_token_removed
        signing["vaultCaRemoved"] = vault_cacert_removed
        signing["registryAuthConfigRemoved"] = registry_auth_config_removed
        signing["registryCaRemoved"] = registry_ca_removed
        signing["secretStateRemoved"] = signing_secret_state_removed
    if not signing_secret_state_removed:
        errors.append(
            {
                "code": "release.registry_signature_key_cleanup_failed",
                "message": "Worker Registry supply-chain gate did not remove isolated signing Secret state.",
            }
        )
    if signing.get("vaultCaMaterialized") and not vault_cacert_removed:
        errors.append(
            {
                "code": "release.registry_signature_key_cleanup_failed",
                "message": "Worker Registry supply-chain gate did not remove the materialized Vault CA certificate state.",
            }
        )
    if options.registry_auth_username_environment is not None and not registry_auth_config_removed:
        errors.append(
            {
                "code": "release.registry_signature_key_cleanup_failed",
                "message": "Worker Registry supply-chain gate did not remove the materialized registry auth state.",
            }
        )
    if options.registry_ca_cert_environment is not None and not registry_ca_removed:
        errors.append(
            {
                "code": "release.registry_signature_key_cleanup_failed",
                "message": "Worker Registry supply-chain gate did not remove the materialized registry CA certificate state.",
            }
        )
    return {
        "status": "pass" if not errors else "fail",
        "mode": "registry-supply-chain",
        "tools": {**configuration.tools.as_report(), "versions": versions},
        "signing": signing,
        "vulnerability": vulnerability,
        "cleanup": {
            "ephemeralPrivateKeyRemoved": private_key_removed,
            "identityTokenRemoved": identity_token_removed,
            "vaultCaRemoved": vault_cacert_removed,
            "registryAuthConfigRemoved": registry_auth_config_removed,
            "registryCaRemoved": registry_ca_removed,
            "signingSecretStateRemoved": signing_secret_state_removed,
            "broadCleanupUsed": False,
        },
        "durationMs": acceptance.elapsed_ms(started),
        "errors": errors,
    }
