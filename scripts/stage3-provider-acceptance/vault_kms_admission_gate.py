#!/usr/bin/env python3
"""Verify or temporarily apply Vault Transit KMS, TLS registry, and Kyverno admission controls."""

from __future__ import annotations

import argparse
import base64
import copy
import dataclasses
import datetime as dt
import hashlib
import http.client
import json
import os
import pathlib
import re
import shutil
import ssl
import subprocess
import sys
import time
import urllib.parse
import uuid
from collections.abc import Mapping, Sequence
from typing import Any

import acceptance_runner as acceptance
import registry_release_gate as registry_gate
import registry_supply_chain as supply_chain
import release_gate_common as common


SCHEMA_VERSION = "synara.vault-kms-admission-gate.v1"
JSON_REPORT_NAME = "vault-kms-admission-gate.json"
MARKDOWN_REPORT_NAME = "vault-kms-admission-gate.md"
ADMISSION_MODES = ("verify-existing", "apply-owned")
OWNER_LABEL = "synara.dev/vault-kms-admission-owner"
OWNER_ANNOTATION = "synara.dev/vault-kms-admission-run-id"
REQUIRED_VAULT_PEERS = 3
REQUIRED_AUDIT_DEVICES = 2
EXPECTED_VAULT_KEY_NAME = "synara-worker-release"
EXPECTED_VAULT_KEY_REFERENCE = supply_chain.VAULT_TRANSIT_KEY_REFERENCE
EXPECTED_VAULT_KEY_TYPE = "ecdsa-p256"
EXPECTED_VAULT_KEY_AUTO_ROTATE_PERIOD_SECONDS = 0
DEFAULT_VAULT_APPROLE_NAME = "synara-worker-release-signer"
DEFAULT_VAULT_OPERATOR_APPROLE_NAME = "synara-vault-production-auditor"
DEFAULT_VAULT_SELECTOR = "app.kubernetes.io/name=vault"
DEFAULT_VAULT_OPERATOR_TOKEN_ENV = "VAULT_OPERATOR_TOKEN"
DEFAULT_REGISTRY_CA_ENV = "REGISTRY_CA_CERT"
DEFAULT_REGISTRY_USERNAME_ENV = "REGISTRY_USERNAME"
DEFAULT_REGISTRY_PASSWORD_ENV = "REGISTRY_PASSWORD"
DEFAULT_TIMEOUT_SECONDS = 300.0
MAX_OUTPUT_EXCERPT = 2000
KYVERNO_CA_BUNDLE_KEY = "ca-certificates.crt"
KYVERNO_CA_MOUNT_PATH = "/etc/ssl/certs/ca-certificates.crt"
KYVERNO_REGISTRY_PULL_SECRET_NAME = supply_chain.SECURITY_REGISTRY_PULL_SECRET_NAME
KYVERNO_VERIFY_IMAGE_COMPONENTS = ("admission-controller", "background-controller")
KYVERNO_VERIFY_IMAGE_SELECTOR = (
    "app.kubernetes.io/component in (admission-controller,background-controller)"
)
KYVERNO_CHART_PATTERN = re.compile(r"kyverno-[0-9][0-9A-Za-z._-]*")
AUTOGEN_CONTROLLERS_ANNOTATION = "pod-policies.kyverno.io/autogen-controllers"
REQUIRED_AUTOGEN_CONTROLLERS = ("DaemonSet", "Deployment", "Job", "StatefulSet", "CronJob")
REQUIRED_CONTROLLER_PROBE_KINDS = ("Deployment", "StatefulSet", "Job", "CronJob")
VAULT_AUDIT_MOUNT_PATH = "/vault/audit"
VAULT_AUDIT_PATH_PREFIX = f"{VAULT_AUDIT_MOUNT_PATH}/"
VAULT_AUDIT_DEVICE_PRIMARY_NAME = "file"
VAULT_AUDIT_DEVICE_PRIMARY_FILE_PATH = f"{VAULT_AUDIT_MOUNT_PATH}/audit-primary.log"
VAULT_AUDIT_DEVICE_SECONDARY_NAME = "file-secondary"
VAULT_AUDIT_DEVICE_SECONDARY_FILE_PATH = f"{VAULT_AUDIT_MOUNT_PATH}/audit-secondary.log"
VAULT_AUDIT_OBSERVABILITY_CONFIGMAP_NAME = "synara-vault-audit-observability"
VAULT_AUDIT_OBSERVABILITY_VOLUME_NAME = "audit-observability-config"
VAULT_AUDIT_VECTOR_CONFIG_KEY = "vector.yaml"
VAULT_AUDIT_SHIPPER_SCRIPT_KEY = "ship-audit.sh"
VAULT_AUDIT_ROTATION_SCRIPT_KEY = "rotate-audit.sh"
VAULT_AUDIT_CONFIG_MOUNT_PATH = "/etc/vault-audit"
VAULT_AUDIT_SIEM_SECRET_NAME = "synara-vault-audit-siem"
VAULT_AUDIT_SIEM_TLS_VOLUME_NAME = "audit-siem-tls"
VAULT_AUDIT_SIEM_TLS_MOUNT_PATH = "/var/run/vault-audit-siem"
VAULT_AUDIT_SHIPPER_NAME = "vault-audit-shipper"
VAULT_AUDIT_ROTATION_NAME = "vault-audit-rotation"
VAULT_AUDIT_RUN_AS_USER = 100
VAULT_AUDIT_RUN_AS_GROUP = 1000
VAULT_AUDIT_ACTIVE_FILE_PERMISSIONS = "0600"
VAULT_AUDIT_ROTATION_MAX_BYTES = 104857600
VAULT_AUDIT_ROTATION_KEEP_ARCHIVES = 7
VAULT_AUDIT_ROTATION_INTERVAL_SECONDS = 60
VAULT_AUDIT_FD_WAIT_ATTEMPTS = 60
VAULT_AUDIT_SHIPPER_IMAGE_REFERENCE = (
    "timberio/vector:0.45.0-debian@sha256:987a15ebfb2eac3a4d5efb26252d140f799553feffb753dc215bdf738a7d4174"
)
VAULT_AUDIT_ROTATION_IMAGE_REFERENCE = (
    "alpine:3.22.1@sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1"
)
VAULT_AUDIT_SIEM_EGRESS_CIDR = "0.250.250.254/32"
VAULT_AUDIT_SIEM_EGRESS_PORT = 18443
VAULT_AUDIT_SIEM_ENVIRONMENT = (
    ("VAULT_AUDIT_SIEM_ENDPOINT", "VAULT_AUDIT_SIEM_ENDPOINT"),
    ("VAULT_AUDIT_SIEM_CLIENT_CERT", "VAULT_AUDIT_SIEM_CLIENT_CERT"),
    ("VAULT_AUDIT_SIEM_CLIENT_KEY", "VAULT_AUDIT_SIEM_CLIENT_KEY"),
    ("VAULT_AUDIT_SIEM_CA_CERT", "VAULT_AUDIT_SIEM_CA_CERT"),
)
VAULT_READINESS_HEALTH_PATH = (
    "/v1/sys/health?standbyok=true&perfstandbyok=true&sealedcode=204&uninitcode=204"
)
VAULT_MAIN_CONTAINER_NAME = "vault"
PEM_CERTIFICATE_PATTERN = re.compile(
    r"-----BEGIN CERTIFICATE-----\n.+?\n-----END CERTIFICATE-----\n?",
    re.DOTALL,
)
PEM_PUBLIC_KEY_PATTERN = re.compile(
    r"-----BEGIN PUBLIC KEY-----\n.+?\n-----END PUBLIC KEY-----\n?",
    re.DOTALL,
)
ENVIRONMENT_NAME_PATTERN = re.compile(r"[A-Z][A-Z0-9_]{0,127}")
EXECUTABLE_PATTERN = re.compile(r"[^\r\n\t\x00]+")
LABEL_SELECTOR_PATTERN = re.compile(r"[A-Za-z0-9./=,_:-]{1,1024}")
REPOSITORY_COMPONENT_PATTERN = re.compile(r"[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*")
REGISTRY_HOST_PATTERN = re.compile(
    r"[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?(?::([0-9]{1,5}))?"
)
TAG_PATTERN = re.compile(r"[A-Za-z0-9_][A-Za-z0-9._-]{0,127}")
DIGEST_PATTERN = re.compile(r"sha256:[0-9a-f]{64}")
TAG_DRIFT_IMAGE_TAG_PREFIX = "synara-stage3-tag-drift-"
TAG_DRIFT_IMAGE_TAG_SUFFIX_MIN_LENGTH = 16
TAG_DRIFT_IMAGE_TAG_SUFFIX_MAX_LENGTH = 128 - len(TAG_DRIFT_IMAGE_TAG_PREFIX)
TAG_DRIFT_IMAGE_TAG_SUFFIX_PATTERN = re.compile(
    rf"[a-z0-9](?:[a-z0-9-]{{{TAG_DRIFT_IMAGE_TAG_SUFFIX_MIN_LENGTH - 1},{TAG_DRIFT_IMAGE_TAG_SUFFIX_MAX_LENGTH - 1}}})"
)
ADMISSION_DENIAL_MARKERS = (
    "kyverno",
    "verifyimages",
    "verify image",
    "admission webhook",
    "image verification",
    "failed to verify image",
)
REGISTRY_ACCEPT_HEADERS = (
    "application/vnd.oci.image.index.v1+json",
    "application/vnd.oci.image.manifest.v1+json",
    "application/vnd.docker.distribution.manifest.list.v2+json",
    "application/vnd.docker.distribution.manifest.v2+json",
)
REGISTRY_RELEASE_SOURCE_PATHS = supply_chain.PRODUCTION_RELEASE_SOURCE_PATHS
VAULT_SIGNER_POLICY_PATH = pathlib.Path(
    "deploy/kubernetes/security/vault/synara-worker-release-signer.hcl"
)
VAULT_SIGNER_POLICY_NAME = VAULT_SIGNER_POLICY_PATH.stem
VAULT_OPERATOR_POLICY_PATH = pathlib.Path(
    "deploy/kubernetes/security/vault/synara-vault-production-auditor.hcl"
)
VAULT_OPERATOR_POLICY_NAME = VAULT_OPERATOR_POLICY_PATH.stem
VAULT_VALUES_PRODUCTION_PATH = pathlib.Path("deploy/kubernetes/security/vault/values.production.yaml")
VAULT_SERVER_PDB_PATH = pathlib.Path("deploy/kubernetes/security/vault/manifests/vault-server-pdb.yaml")
VAULT_OPERATIONS_POLICY_PATH = pathlib.Path("deploy/kubernetes/security/vault/operations-policy.json")
VAULT_HELM_POST_RENDERER_PATH = pathlib.Path(
    "deploy/kubernetes/security/vault/helm-post-renderer.sh"
)
VAULT_HELM_POST_RENDERER_PLUGIN_PATH = pathlib.Path(
    "deploy/kubernetes/security/vault/helm-plugins/synara-vault-tls-readiness/plugin.yaml"
)
VAULT_AUDIT_OBSERVABILITY_CONFIGMAP_PATH = pathlib.Path(
    "deploy/kubernetes/security/vault/manifests/vault-audit-observability-configmap.yaml"
)
VAULT_BOOTSTRAP_PATH = pathlib.Path(
    "deploy/kubernetes/security/vault/bootstrap/enable-transit-audit-approle.sh"
)
VAULT_NETWORK_POLICY_NAME = "synara-vault"
VAULT_CONFIGMAP_NAME = "synara-vault-config"
VAULT_RETRY_JOIN_ADDRESSES = tuple(
    f"https://synara-vault-{index}.synara-vault-internal:8200"
    for index in range(REQUIRED_VAULT_PEERS)
)
VAULT_RETRY_JOIN_CA_CERT_FILE = "/vault/tls/ca.crt"
VAULT_BREAK_GLASS_UNAUTHENTICATED_ACCESS = "generate-root"
VAULT_BREAK_GLASS_MAXIMUM_WINDOW_MINUTES = 15
VAULT_BREAK_GLASS_PROBE_MAX_RESPONSE_BYTES = 1 << 20
VAULT_RELEASE_LABELS = {
    "app.kubernetes.io/name": "vault",
    "app.kubernetes.io/instance": "synara-vault",
}
VAULT_SERVER_LABELS = {**VAULT_RELEASE_LABELS, "component": "server"}
VAULT_DNS_NAMESPACE_LABELS = {"kubernetes.io/metadata.name": "kube-system"}
VAULT_DNS_POD_LABELS = {"k8s-app": "kube-dns"}
VAULT_KUBERNETES_SERVICE_IP_CIDR = "10.96.0.1/32"
# Kind/OrbStack can reassign the control-plane container IP within this node subnet.
VAULT_KUBERNETES_APISERVER_CIDR = "192.168.155.0/24"
SIGNER_ROLE_CONSTRAINTS = {
    "token_ttl": 2 * 60 * 60,
    "token_max_ttl": 4 * 60 * 60,
    "token_num_uses": 0,
    "secret_id_ttl": 10 * 60,
    "secret_id_num_uses": 1,
}
OPERATOR_ROLE_CONSTRAINTS = {
    "token_ttl": 30 * 60,
    "token_max_ttl": 60 * 60,
    "token_num_uses": 0,
    "secret_id_ttl": 10 * 60,
    "secret_id_num_uses": 1,
}

ReleaseGateError = common.ReleaseGateError


@dataclasses.dataclass(frozen=True)
class GateOptions:
    repo_root: pathlib.Path
    output_dir: pathlib.Path
    kube_context: str
    vault_namespace: str
    security_namespace: str
    admission_test_namespace: str
    vault_selector: str
    vault_approle_name: str
    expected_approle_policies: tuple[str, ...]
    registry_release_gate_report: pathlib.Path
    signed_image_ref: str | None
    unsigned_image_ref: str
    wrong_key_image_ref: str
    tag_drift_image_ref: str
    admission_mode: str
    kubectl_bin: str
    vault_bin: str
    cosign_bin: str
    vault_address_env: str
    vault_token_env: str
    vault_operator_token_env: str
    vault_cacert_env: str
    registry_ca_cert_env: str
    registry_username_env: str
    registry_password_env: str
    timeout_seconds: float


@dataclasses.dataclass(frozen=True)
class ImageReference:
    original: str
    registry: str
    repository: str
    tag: str | None
    digest: str | None

    @property
    def repository_root(self) -> str:
        return f"{self.registry}/{self.repository}"

    @property
    def digest_reference(self) -> str:
        if self.digest is None:
            raise ValueError("image reference did not include a digest")
        return f"{self.repository_root}@{self.digest}"


@dataclasses.dataclass(frozen=True)
class RegistryReleaseEvidence:
    run_id: str
    report_sha256: str
    git_sha: str
    version: str
    image_repository: str
    cached_signed_image: str
    signing_policy_sha256: str
    production_signing_profile_sha256: str
    source_hashes: dict[str, str]
    transparency_log_verified: bool
    transparency_log_inclusion_proof_present: bool
    transparency_log_signed_entry_timestamp_present: bool
    cached_signature_transparency_log: dict[str, Any]
    registry_access: dict[str, str]
    production_registry_boundary: dict[str, Any]
    signer_identity: dict[str, Any]

    @property
    def cached_signature_annotations(self) -> dict[str, str]:
        return {
            "synara.git-sha": self.git_sha,
            "synara.run-id": self.run_id,
            "synara.slot": "cached",
            "synara.version": self.version,
        }

    def as_report(self) -> dict[str, Any]:
        return {
            "runId": self.run_id,
            "reportSha256": self.report_sha256,
            "gitSha": self.git_sha,
            "version": self.version,
            "imageRepository": self.image_repository,
            "cachedSignedImage": self.cached_signed_image,
            "signingPolicySha256": self.signing_policy_sha256,
            "productionSigningProfileSha256": self.production_signing_profile_sha256,
            "cachedSignatureAnnotations": dict(sorted(self.cached_signature_annotations.items())),
            "transparencyLogVerified": self.transparency_log_verified,
            "transparencyLogInclusionProofPresent": self.transparency_log_inclusion_proof_present,
            "transparencyLogSignedEntryTimestampPresent": self.transparency_log_signed_entry_timestamp_present,
            "cachedSignatureTransparencyLog": copy.deepcopy(self.cached_signature_transparency_log),
            "registryAccess": dict(sorted(self.registry_access.items())),
            "productionRegistryBoundary": copy.deepcopy(self.production_registry_boundary),
            "signerIdentity": copy.deepcopy(self.signer_identity),
            "sourceHashes": dict(sorted(self.source_hashes.items())),
        }


@dataclasses.dataclass(frozen=True)
class SecretInputs:
    vault_environment: dict[str, str]
    vault_operator_environment: dict[str, str]
    registry_ca_path: pathlib.Path
    registry_username: str
    registry_password: str
    registry_username_env: str
    registry_password_env: str
    registry_ca_env: str
    vault_env_names: tuple[str, ...]


@dataclasses.dataclass(frozen=True)
class AdmissionResourceNames:
    public_key_configmap: str | None
    repository_configmap: str | None
    cluster_policy: str | None
    probe_pull_secret: str | None = None
    probe_secret_reader_role: str | None = None
    probe_secret_reader_binding: str | None = None


@dataclasses.dataclass(frozen=True)
class KyvernoController:
    namespace: str
    deployment: str
    service_account: str
    component: str


@dataclasses.dataclass(frozen=True)
class KyvernoControllerPatch:
    namespace: str
    deployment: str
    service_account: str
    component: str
    ca_configmap_name: str
    ca_data_key: str
    volume_name: str
    injected_mount: bool
    original_ca_bundle: str | None = None


@dataclasses.dataclass(frozen=True)
class AdmissionBundle:
    mode: str
    namespace: str
    repository_pattern: str
    names: AdmissionResourceNames
    source_hashes: dict[str, str]
    rendered_hashes: dict[str, str]
    created_resources: tuple[tuple[str, str, str | None], ...]
    controller_patches: tuple[KyvernoControllerPatch, ...] = ()

    def as_report(self) -> dict[str, Any]:
        report = {
            "mode": self.mode,
            "namespace": self.namespace,
            "repositoryPatternSha256": sha256_text(self.repository_pattern),
            "resourceNames": dataclasses.asdict(self.names),
            "sourceHashes": dict(sorted(self.source_hashes.items())),
            "renderedHashes": dict(sorted(self.rendered_hashes.items())),
        }
        if self.controller_patches:
            report["controllerPatches"] = [
                {
                    "namespace": patch.namespace,
                    "deployment": patch.deployment,
                    "serviceAccount": patch.service_account,
                    "component": patch.component,
                    "caConfigMap": patch.ca_configmap_name,
                    "volumeName": patch.volume_name,
                    "injectedMount": patch.injected_mount,
                    "restoredExistingBundle": patch.original_ca_bundle is not None,
                }
                for patch in self.controller_patches
            ]
        return report


def sha256_bytes(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def sha256_text(value: str) -> str:
    return sha256_bytes(value.encode("utf-8"))


def utc_now() -> str:
    return dt.datetime.now(dt.timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def elapsed_ms(started: float) -> int:
    return max(0, round((time.monotonic() - started) * 1000))


def normalize_environment_name(value: str, flag: str) -> str:
    name = value.strip()
    if ENVIRONMENT_NAME_PATTERN.fullmatch(name) is None:
        raise ValueError(f"{flag} must be a valid environment variable name")
    return name


def normalize_executable(value: str, flag: str) -> str:
    executable = value.strip()
    if (
        any(character in value for character in "\r\n\t\x00")
        or EXECUTABLE_PATTERN.fullmatch(executable) is None
    ):
        raise ValueError(f"{flag} must be a command or executable path")
    return executable


def normalize_kubernetes_name(value: str, flag: str) -> str:
    name = value.strip()
    if supply_chain.KUBERNETES_NAME_PATTERN.fullmatch(name) is None:
        raise ValueError(f"{flag} must be a lowercase Kubernetes DNS label")
    return name


def normalize_label_selector(value: str, flag: str) -> str:
    selector = value.strip()
    if LABEL_SELECTOR_PATTERN.fullmatch(selector) is None:
        raise ValueError(f"{flag} must be a non-empty Kubernetes label selector")
    return selector


def _normalize_registry_host(value: str, *, flag: str) -> str:
    host = value.strip()
    match = REGISTRY_HOST_PATTERN.fullmatch(host)
    port = match.group(1) if match is not None else None
    if match is None or (port is not None and not 1 <= int(port) <= 65535):
        raise ValueError(f"{flag} used an invalid registry host")
    return host


def normalize_image_reference(
    value: str,
    *,
    flag: str,
    require_digest: bool | None = None,
) -> ImageReference:
    original = value.strip()
    if (
        not original
        or len(original) > 1024
        or any(character.isspace() or ord(character) < 32 for character in original)
        or any(character in original for character in "?#")
        or "://" in original
    ):
        raise ValueError(f"{flag} must be a credential-free image reference")
    if "@" in original:
        name, digest = original.rsplit("@", 1)
        if DIGEST_PATTERN.fullmatch(digest) is None:
            raise ValueError(f"{flag} must use a sha256 digest when @ is present")
    else:
        name = original
        digest = None
    slash_index = name.rfind("/")
    colon_index = name.rfind(":")
    tag: str | None = None
    repository_root = name
    if colon_index > slash_index:
        tag = name[colon_index + 1 :]
        repository_root = name[:colon_index]
        if TAG_PATTERN.fullmatch(tag) is None:
            raise ValueError(f"{flag} used an invalid image tag")
    components = repository_root.split("/")
    if len(components) < 2:
        raise ValueError(f"{flag} must include a registry host and repository path")
    registry = _normalize_registry_host(components[0], flag=flag)
    repository_components = components[1:]
    if any(
        REPOSITORY_COMPONENT_PATTERN.fullmatch(component) is None
        for component in repository_components
    ):
        raise ValueError(f"{flag} used an invalid repository path")
    if require_digest is True and digest is None:
        raise ValueError(f"{flag} must be digest-pinned")
    if require_digest is False and digest is not None:
        raise ValueError(f"{flag} must not be digest-pinned")
    return ImageReference(
        original=original,
        registry=registry,
        repository="/".join(repository_components),
        tag=tag,
        digest=digest,
    )


def normalize_tag_drift_image_reference(value: str, *, flag: str) -> ImageReference:
    reference = normalize_image_reference(value, flag=flag, require_digest=False)
    tag = reference.tag
    if tag is None:
        raise ValueError(
            f"{flag} must use a gate-owned run-scoped tag starting with "
            f"'{TAG_DRIFT_IMAGE_TAG_PREFIX}'"
        )
    if tag.lower() == "latest":
        raise ValueError(
            f"{flag} must not use the mutable latest tag; use a gate-owned run-scoped tag "
            f"starting with '{TAG_DRIFT_IMAGE_TAG_PREFIX}'"
        )
    if not tag.startswith(TAG_DRIFT_IMAGE_TAG_PREFIX):
        raise ValueError(
            f"{flag} must use a gate-owned run-scoped tag starting with "
            f"'{TAG_DRIFT_IMAGE_TAG_PREFIX}'"
        )
    suffix = tag[len(TAG_DRIFT_IMAGE_TAG_PREFIX) :]
    if TAG_DRIFT_IMAGE_TAG_SUFFIX_PATTERN.fullmatch(suffix) is None:
        raise ValueError(
            f"{flag} must use a gate-owned run-scoped tag with a lowercase [a-z0-9-] "
            f"suffix of at least {TAG_DRIFT_IMAGE_TAG_SUFFIX_MIN_LENGTH} characters"
        )
    return reference


def parse_args(argv: Sequence[str]) -> GateOptions:
    repo_root = pathlib.Path(__file__).resolve().parents[2]
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--kube-context", required=True)
    parser.add_argument("--vault-namespace", required=True)
    parser.add_argument("--security-namespace", required=True)
    parser.add_argument("--admission-test-namespace", required=True)
    parser.add_argument("--vault-selector", default=DEFAULT_VAULT_SELECTOR)
    parser.add_argument("--vault-approle-name", default=DEFAULT_VAULT_APPROLE_NAME)
    parser.add_argument(
        "--expected-approle-policy",
        action="append",
        default=[],
        help="Expected exact AppRole token policy name; repeat for multiple policies",
    )
    parser.add_argument(
        "--registry-release-gate-report",
        type=pathlib.Path,
        required=True,
        help="Passing production worker-registry-release-gate JSON report",
    )
    parser.add_argument("--signed-image-ref")
    parser.add_argument("--unsigned-image-ref", required=True)
    parser.add_argument("--wrong-key-image-ref", required=True)
    parser.add_argument("--tag-drift-image-ref", required=True)
    parser.add_argument(
        "--admission-mode",
        choices=ADMISSION_MODES,
        default="verify-existing",
    )
    parser.add_argument("--kubectl-bin", default="kubectl")
    parser.add_argument("--vault-bin", default="vault")
    parser.add_argument("--cosign-bin", default="cosign")
    parser.add_argument("--vault-address-env", default="VAULT_ADDR")
    parser.add_argument("--vault-token-env", default="VAULT_TOKEN")
    parser.add_argument(
        "--vault-operator-token-env",
        default=DEFAULT_VAULT_OPERATOR_TOKEN_ENV,
    )
    parser.add_argument("--vault-cacert-env", default="VAULT_CACERT")
    parser.add_argument("--registry-ca-cert-env", default=DEFAULT_REGISTRY_CA_ENV)
    parser.add_argument("--registry-username-env", default=DEFAULT_REGISTRY_USERNAME_ENV)
    parser.add_argument("--registry-password-env", default=DEFAULT_REGISTRY_PASSWORD_ENV)
    parser.add_argument("--timeout", type=float, default=DEFAULT_TIMEOUT_SECONDS)
    parser.add_argument("--output-dir", type=pathlib.Path)
    parsed = parser.parse_args(argv)
    if parsed.timeout <= 0:
        parser.error("--timeout must be positive")
    if not parsed.expected_approle_policy:
        parser.error("at least one --expected-approle-policy is required")
    try:
        kube_context = parsed.kube_context.strip()
        if not kube_context or any(character in kube_context for character in "\r\n\t\x00"):
            raise ValueError("--kube-context must be a non-empty Kubernetes context name")
        vault_namespace = normalize_kubernetes_name(parsed.vault_namespace, "--vault-namespace")
        security_namespace = normalize_kubernetes_name(parsed.security_namespace, "--security-namespace")
        admission_test_namespace = normalize_kubernetes_name(
            parsed.admission_test_namespace,
            "--admission-test-namespace",
        )
        vault_selector = normalize_label_selector(parsed.vault_selector, "--vault-selector")
        vault_approle_name = normalize_kubernetes_name(parsed.vault_approle_name, "--vault-approle-name")
        expected_policies = tuple(
            normalize_kubernetes_name(item, "--expected-approle-policy")
            for item in parsed.expected_approle_policy
        )
        if len(set(expected_policies)) != len(expected_policies):
            raise ValueError("--expected-approle-policy values must be unique")
        if parsed.signed_image_ref is not None:
            normalize_image_reference(
                parsed.signed_image_ref,
                flag="--signed-image-ref",
                require_digest=True,
            )
        normalize_image_reference(
            parsed.unsigned_image_ref,
            flag="--unsigned-image-ref",
            require_digest=True,
        )
        normalize_image_reference(
            parsed.wrong_key_image_ref,
            flag="--wrong-key-image-ref",
            require_digest=True,
        )
        normalize_tag_drift_image_reference(parsed.tag_drift_image_ref, flag="--tag-drift-image-ref")
        kubectl_bin = normalize_executable(parsed.kubectl_bin, "--kubectl-bin")
        vault_bin = normalize_executable(parsed.vault_bin, "--vault-bin")
        cosign_bin = normalize_executable(parsed.cosign_bin, "--cosign-bin")
        vault_address_env = normalize_environment_name(parsed.vault_address_env, "--vault-address-env")
        vault_token_env = normalize_environment_name(parsed.vault_token_env, "--vault-token-env")
        vault_operator_token_env = normalize_environment_name(
            parsed.vault_operator_token_env,
            "--vault-operator-token-env",
        )
        if vault_operator_token_env == vault_token_env:
            raise ValueError("--vault-token-env and --vault-operator-token-env must be different")
        vault_cacert_env = normalize_environment_name(parsed.vault_cacert_env, "--vault-cacert-env")
        registry_ca_cert_env = normalize_environment_name(
            parsed.registry_ca_cert_env,
            "--registry-ca-cert-env",
        )
        registry_username_env = normalize_environment_name(
            parsed.registry_username_env,
            "--registry-username-env",
        )
        registry_password_env = normalize_environment_name(
            parsed.registry_password_env,
            "--registry-password-env",
        )
    except ValueError as error:
        parser.error(str(error))
    output_dir = parsed.output_dir or (
        repo_root
        / ".tmp"
        / "stage3-provider-acceptance-results"
        / f"vault-kms-admission-gate-{dt.datetime.now(dt.timezone.utc).strftime('%Y%m%dT%H%M%SZ')}"
    )
    return GateOptions(
        repo_root=repo_root,
        output_dir=output_dir.expanduser().resolve(),
        kube_context=kube_context,
        vault_namespace=vault_namespace,
        security_namespace=security_namespace,
        admission_test_namespace=admission_test_namespace,
        vault_selector=vault_selector,
        vault_approle_name=vault_approle_name,
        expected_approle_policies=expected_policies,
        registry_release_gate_report=parsed.registry_release_gate_report.expanduser().resolve(),
        signed_image_ref=parsed.signed_image_ref.strip() if parsed.signed_image_ref else None,
        unsigned_image_ref=parsed.unsigned_image_ref.strip(),
        wrong_key_image_ref=parsed.wrong_key_image_ref.strip(),
        tag_drift_image_ref=parsed.tag_drift_image_ref.strip(),
        admission_mode=parsed.admission_mode,
        kubectl_bin=kubectl_bin,
        vault_bin=vault_bin,
        cosign_bin=cosign_bin,
        vault_address_env=vault_address_env,
        vault_token_env=vault_token_env,
        vault_operator_token_env=vault_operator_token_env,
        vault_cacert_env=vault_cacert_env,
        registry_ca_cert_env=registry_ca_cert_env,
        registry_username_env=registry_username_env,
        registry_password_env=registry_password_env,
        timeout_seconds=parsed.timeout,
    )


def _read_json_file(path: pathlib.Path, *, code: str, message: str) -> Any:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError):
        raise ReleaseGateError(code, message, {"path": str(path)}) from None


def _read_text_file(path: pathlib.Path, *, code: str, message: str) -> str:
    try:
        return path.read_text(encoding="utf-8")
    except OSError:
        raise ReleaseGateError(code, message, {"path": str(path)}) from None


def _source_file_hashes(repo_root: pathlib.Path) -> dict[str, str]:
    hashes: dict[str, str] = {}
    for relative_path in REGISTRY_RELEASE_SOURCE_PATHS:
        absolute_path = repo_root / relative_path
        try:
            hashes[str(relative_path)] = sha256_bytes(absolute_path.read_bytes())
        except OSError:
            raise ReleaseGateError(
                "release.vault_kms_source_invalid",
                "The Vault KMS admission gate could not read a required checked-in source artifact.",
                {"path": str(relative_path)},
            ) from None
    return hashes


def _normalized_release_source_hashes(value: Any) -> dict[str, str]:
    expected_paths = {str(path) for path in REGISTRY_RELEASE_SOURCE_PATHS}
    if not isinstance(value, dict):
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate report omitted the exact checked-in production source hash set.",
        )
    actual_paths = set(value)
    if actual_paths != expected_paths:
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate report omitted the exact checked-in production source hash set.",
            {
                "missingPaths": sorted(expected_paths - actual_paths),
                "unexpectedPaths": sorted(actual_paths - expected_paths),
            },
        )
    hashes: dict[str, str] = {}
    for path in sorted(expected_paths):
        raw_hash = value.get(path)
        if not isinstance(raw_hash, str) or re.fullmatch(r"[0-9a-f]{64}", raw_hash) is None:
            raise ReleaseGateError(
                "release.vault_kms_registry_release_gate_invalid",
                "The production registry_release_gate report contained an invalid checked-in production source hash.",
                {"path": path},
            )
        hashes[path] = raw_hash
    return hashes


def _normalized_release_version(value: Any) -> str:
    if (
        not isinstance(value, str)
        or not value.strip()
        or len(value) > 128
        or any(ord(character) < 32 for character in value)
    ):
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate report omitted its source version.",
        )
    return value.strip()


def _normalized_vault_policy_text(text: str) -> str:
    normalized_lines: list[str] = []
    for raw_line in text.splitlines():
        stripped = raw_line.strip()
        if not stripped or stripped.startswith("#") or stripped.startswith("//"):
            continue
        normalized_lines.append(" ".join(stripped.split()))
    return "\n".join(normalized_lines)


def _yaml_scalar_value(text: str, path: Sequence[str]) -> str | None:
    wanted = tuple(path)
    stack: list[str] = []
    for raw_line in text.splitlines():
        if not raw_line.strip() or raw_line.lstrip().startswith("#"):
            continue
        indent = len(raw_line) - len(raw_line.lstrip(" "))
        if indent % 2 != 0:
            continue
        stripped = raw_line.strip()
        if stripped.startswith("- "):
            continue
        key, separator, remainder = stripped.partition(":")
        if not separator:
            continue
        level = indent // 2
        stack = stack[:level]
        normalized_key = key.strip()
        value = remainder.strip()
        if not value or value in {"|", ">"}:
            stack.append(normalized_key)
            continue
        current = tuple([*stack, normalized_key])
        if current != wanted:
            continue
        if len(value) >= 2 and value[0] == value[-1] and value[0] in {'"', "'"}:
            value = value[1:-1]
        return value
    return None


def _literal_configmap_data(text: str, *, code: str) -> dict[str, str]:
    entries: dict[str, str] = {}
    current_key: str | None = None
    current_lines: list[str] = []
    for line in text.splitlines(keepends=True):
        key_match = re.match(r"^  (?P<key>[A-Za-z0-9._-]+): \|\n?$", line)
        if key_match is not None:
            if current_key is not None:
                entries[current_key] = "".join(current_lines)
            current_key = key_match.group("key")
            current_lines = []
            continue
        if current_key is None:
            continue
        if line.startswith("    "):
            current_lines.append(line[4:])
            continue
        if line in {"\n", "\r\n"}:
            current_lines.append(line)
            continue
        entries[current_key] = "".join(current_lines)
        current_key = None
        current_lines = []
    if current_key is not None:
        entries[current_key] = "".join(current_lines)
    if not entries:
        raise ReleaseGateError(
            code,
            "The checked-in Vault audit observability ConfigMap did not expose literal config data.",
        )
    return entries


def _expected_vault_audit_siem_secret_refs() -> list[dict[str, str]]:
    return sorted(
        [
        {
            "name": env_name,
            "secretName": VAULT_AUDIT_SIEM_SECRET_NAME,
            "secretKey": secret_key,
        }
        for env_name, secret_key in VAULT_AUDIT_SIEM_ENVIRONMENT
        ],
        key=_stringify_json,
    )


def _expected_vault_audit_sidecar_security_context() -> dict[str, Any]:
    return {
        "runAsNonRoot": True,
        "runAsUser": VAULT_AUDIT_RUN_AS_USER,
        "runAsGroup": VAULT_AUDIT_RUN_AS_GROUP,
        "allowPrivilegeEscalation": False,
        "readOnlyRootFilesystem": True,
        "seccompProfile": {"type": "RuntimeDefault"},
        "capabilities": {"drop": ["ALL"]},
    }


def _expected_vault_liveness_probe() -> dict[str, Any]:
    return {
        "execCommand": [
            "/bin/sh",
            "-c",
            'vault status >/dev/null 2>&1; status=$?; [ "$status" -eq 0 ] || [ "$status" -eq 2 ]',
        ],
        "failureThreshold": 3,
        "initialDelaySeconds": 300,
        "periodSeconds": 10,
        "successThreshold": 1,
        "timeoutSeconds": 3,
    }


def _expected_vault_readiness_probe() -> dict[str, Any]:
    return {
        "execCommand": ["/bin/sh", "-ec", "vault status >/dev/null"],
        "failureThreshold": 3,
        "initialDelaySeconds": 300,
        "periodSeconds": 10,
        "successThreshold": 1,
        "timeoutSeconds": 3,
    }


def _normalized_volume_mount_contract(value: Any, *, code: str, message: str) -> list[dict[str, Any]]:
    if not isinstance(value, list) or not all(isinstance(item, dict) for item in value):
        raise ReleaseGateError(code, message)
    normalized: list[dict[str, Any]] = []
    for mount in value:
        name = mount.get("name")
        mount_path = mount.get("mountPath")
        if not isinstance(name, str) or not name or not isinstance(mount_path, str) or not mount_path:
            raise ReleaseGateError(code, message)
        normalized.append(
            {
                "name": name,
                "mountPath": mount_path,
                "readOnly": mount.get("readOnly") is True,
            }
        )
    return sorted(normalized, key=_stringify_json)


def _normalized_secret_env_contract(value: Any, *, code: str, message: str) -> list[dict[str, str]]:
    if not isinstance(value, list) or not all(isinstance(item, dict) for item in value):
        raise ReleaseGateError(code, message)
    normalized: list[dict[str, str]] = []
    for item in value:
        env_name = item.get("name")
        value_from = item.get("valueFrom")
        if not isinstance(env_name, str) or not env_name or not isinstance(value_from, dict):
            raise ReleaseGateError(code, message)
        secret_key_ref = value_from.get("secretKeyRef")
        if not isinstance(secret_key_ref, dict):
            raise ReleaseGateError(code, message)
        secret_name = secret_key_ref.get("name")
        secret_key = secret_key_ref.get("key")
        if (
            not isinstance(secret_name, str)
            or not secret_name
            or not isinstance(secret_key, str)
            or not secret_key
        ):
            raise ReleaseGateError(code, message)
        normalized.append(
            {
                "name": env_name,
                "secretName": secret_name,
                "secretKey": secret_key,
            }
        )
    return sorted(normalized, key=_stringify_json)


def _normalized_cached_signature_transparency_log(value: Any) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate report omitted cached Rekor transparency-log evidence.",
        )
    bundle_sha256 = value.get("bundleSha256")
    bundle_media_type = value.get("bundleMediaType")
    entry_count = value.get("entryCount")
    entries = value.get("entries")
    if (
        value.get("bundlePresent") is not True
        or value.get("verificationMode") != "cosign-online-tlog-verification"
        or value.get("inclusionProofPresent") is not True
        or value.get("signedEntryTimestampPresent") is not True
        or not isinstance(bundle_media_type, str)
        or not bundle_media_type
        or not isinstance(bundle_sha256, str)
        or re.fullmatch(r"[0-9a-f]{64}", bundle_sha256) is None
        or not isinstance(entry_count, int)
        or entry_count < 1
        or not isinstance(entries, list)
        or len(entries) != entry_count
    ):
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate report omitted cached Rekor inclusion-proof or signed-entry-timestamp evidence.",
        )
    normalized_entries: list[dict[str, Any]] = []
    for entry in entries:
        if not isinstance(entry, dict):
            raise ReleaseGateError(
                "release.vault_kms_registry_release_gate_invalid",
                "The production registry_release_gate report contained malformed cached Rekor entry evidence.",
            )
        log_index = entry.get("logIndex")
        integrated_time = entry.get("integratedTime")
        inclusion_proof_hash_count = entry.get("inclusionProofHashCount")
        signed_entry_timestamp_sha256 = entry.get("signedEntryTimestampSha256")
        if (
            not isinstance(log_index, int)
            or log_index < 0
            or not isinstance(integrated_time, int)
            or integrated_time < 1
            or entry.get("inclusionProofPresent") is not True
            or not isinstance(inclusion_proof_hash_count, int)
            or inclusion_proof_hash_count < 1
            or entry.get("signedEntryTimestampPresent") is not True
            or not isinstance(signed_entry_timestamp_sha256, str)
            or re.fullmatch(r"[0-9a-f]{64}", signed_entry_timestamp_sha256) is None
        ):
            raise ReleaseGateError(
                "release.vault_kms_registry_release_gate_invalid",
                "The production registry_release_gate report contained incomplete cached Rekor entry evidence.",
            )
        normalized_entries.append(
            {
                "logIndex": log_index,
                "integratedTime": integrated_time,
                "inclusionProofPresent": True,
                "inclusionProofHashCount": inclusion_proof_hash_count,
                "signedEntryTimestampPresent": True,
                "signedEntryTimestampSha256": signed_entry_timestamp_sha256,
            }
        )
    return {
        "bundlePresent": True,
        "verificationMode": "cosign-online-tlog-verification",
        "bundleMediaType": bundle_media_type,
        "bundleSha256": bundle_sha256,
        "entryCount": entry_count,
        "entries": normalized_entries,
        "inclusionProofPresent": True,
        "signedEntryTimestampPresent": True,
    }


def _normalized_registry_signer_identity(value: Any) -> dict[str, Any]:
    expected_policies_sha256 = sha256_text(
        json.dumps([VAULT_SIGNER_POLICY_NAME], separators=(",", ":"), sort_keys=True)
    )
    expected = {
        "verified": True,
        "displayName": supply_chain.VAULT_TRANSIT_AUTH_METHOD,
        "roleName": DEFAULT_VAULT_APPROLE_NAME,
        "type": "batch",
        "orphan": True,
        "policyCount": 1,
        "policiesSha256": expected_policies_sha256,
    }
    if not isinstance(value, dict) or set(value) != set(expected) or value != expected:
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate report did not prove the exact Vault signer identity.",
            {
                "expectedIdentitySha256": sha256_text(_stringify_json(expected)),
                "actualIdentitySha256": (
                    sha256_text(_stringify_json(value)) if isinstance(value, dict) else None
                ),
            },
        )
    return {**expected, "identitySha256": sha256_text(_stringify_json(expected))}


def _normalized_registry_access(value: Any) -> dict[str, str]:
    required_fields = {"usernameEnvironment", "passwordEnvironment", "caCertEnvironment"}
    if not isinstance(value, dict) or set(value) != required_fields:
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate report omitted its non-secret Registry CA/auth environment evidence.",
        )
    try:
        return {
            "usernameEnvironment": normalize_environment_name(
                str(value["usernameEnvironment"]),
                "configuration.registryAccess.usernameEnvironment",
            ),
            "passwordEnvironment": normalize_environment_name(
                str(value["passwordEnvironment"]),
                "configuration.registryAccess.passwordEnvironment",
            ),
            "caCertEnvironment": normalize_environment_name(
                str(value["caCertEnvironment"]),
                "configuration.registryAccess.caCertEnvironment",
            ),
        }
    except ValueError:
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate report omitted its non-secret Registry CA/auth environment evidence.",
        ) from None


def _normalized_runtime_evidence_inputs(value: Any) -> dict[str, str]:
    required_fields = {"container", "runtimeConfigPath"}
    if not isinstance(value, dict) or set(value) != required_fields:
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate report omitted its Registry runtime evidence input boundary.",
        )
    try:
        container = registry_gate.normalize_container_name(
            str(value["container"]),
            "configuration.productionRegistryRuntimeEvidenceInputs.container",
        )
        runtime_config_path = registry_gate.normalize_container_path(
            str(value["runtimeConfigPath"]),
            "configuration.productionRegistryRuntimeEvidenceInputs.runtimeConfigPath",
        )
    except ValueError:
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate report omitted its Registry runtime evidence input boundary.",
        ) from None
    assert container is not None
    assert runtime_config_path is not None
    return {
        "container": container,
        "runtimeConfigPath": runtime_config_path,
    }


def _normalized_production_registry_boundary_inputs(value: Any) -> dict[str, str]:
    required_fields = {"registryConfig", "retentionPolicy"}
    if not isinstance(value, dict) or set(value) != required_fields:
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate report omitted its Registry boundary export paths.",
        )
    registry_config = value.get("registryConfig")
    retention_policy = value.get("retentionPolicy")
    if (
        not isinstance(registry_config, str)
        or not registry_config.strip()
        or not isinstance(retention_policy, str)
        or not retention_policy.strip()
    ):
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate report omitted its Registry boundary export paths.",
        )
    return {
        "registryConfig": registry_config.strip(),
        "retentionPolicy": retention_policy.strip(),
    }


def _normalized_production_registry_boundary(
    payload: Mapping[str, Any],
    *,
    image_repository: str,
    source_hashes: Mapping[str, str],
) -> tuple[dict[str, Any], dict[str, str]]:
    configuration = payload.get("configuration")
    runtime = payload.get("runtime")
    if not isinstance(configuration, dict) or not isinstance(runtime, dict):
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate report omitted its live Registry runtime boundary evidence.",
        )
    registry_access = _normalized_registry_access(configuration.get("registryAccess"))
    boundary_inputs = _normalized_production_registry_boundary_inputs(
        configuration.get("productionRegistryBoundaryInputs")
    )
    runtime_inputs = _normalized_runtime_evidence_inputs(
        configuration.get("productionRegistryRuntimeEvidenceInputs")
    )
    production_registry = runtime.get("productionRegistryBoundary")
    required_fields = {
        "registryConfigPath",
        "retentionPolicyPath",
        "deleteEnabled",
        "promotionBoundary",
        "releaseEvidenceDays",
        "garbageCollectionMode",
        "archiveRequiredBeforeGc",
        "liveRuntimeEvidence",
    }
    if not isinstance(production_registry, dict) or set(production_registry) != required_fields:
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate report omitted its live Registry runtime boundary evidence.",
        )
    registry_config_path = pathlib.Path(boundary_inputs["registryConfig"]).expanduser()
    retention_policy_path = pathlib.Path(boundary_inputs["retentionPolicy"]).expanduser()
    if (
        production_registry.get("registryConfigPath") != str(registry_config_path)
        or production_registry.get("retentionPolicyPath") != str(retention_policy_path)
    ):
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate report drifted from its declared Registry boundary export paths.",
        )
    retention_policy_payload = _read_json_file(
        retention_policy_path,
        code="release.vault_kms_registry_release_gate_invalid",
        message="The production registry_release_gate report referenced an unreadable live Registry retention policy export.",
    )
    if not isinstance(retention_policy_payload, dict):
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate report referenced an unreadable live Registry retention policy export.",
        )
    live_retention_policy = registry_gate._normalize_retention_policy(
        retention_policy_payload,
        code="release.vault_kms_registry_release_gate_invalid",
        runtime_config_path=runtime_inputs["runtimeConfigPath"],
    )
    live_retention_policy_sha256 = registry_gate._stable_json_sha256(live_retention_policy)
    registry_config_text = _read_text_file(
        registry_config_path,
        code="release.vault_kms_registry_release_gate_invalid",
        message="The production registry_release_gate report referenced an unreadable live Registry configuration export.",
    )
    normalized_registry_config_text = registry_gate._normalize_text_content(registry_config_text)
    exported_config_sha256 = sha256_text(normalized_registry_config_text)
    registry_host = image_repository.split("/", 1)[0]
    registry_authority, _certificate_path = registry_gate._extract_runtime_registry_details(
        normalized_registry_config_text,
        registry_host=registry_host,
    )
    live_runtime_evidence = registry_gate._validate_live_registry_boundary_evidence(
        production_registry.get("liveRuntimeEvidence"),
        registry_host=registry_host,
        image_repository=image_repository,
        runtime_config_path=runtime_inputs["runtimeConfigPath"],
        exported_config_sha256=exported_config_sha256,
        live_policy_sha256=live_retention_policy_sha256,
        checked_in_policy_sha256=live_retention_policy_sha256,
    )
    if live_runtime_evidence.get("checkedInRetentionPolicySha256") != live_retention_policy_sha256:
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate report drifted from the checked-in Registry retention-policy boundary it claimed to verify.",
        )
    if live_runtime_evidence.get("registryAuthority") != registry_authority:
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate report drifted from the live Registry authority encoded in its exported runtime configuration.",
        )
    expected_container = runtime_inputs["container"]
    runtime_container = (
        live_runtime_evidence.get("container")
        if isinstance(live_runtime_evidence.get("container"), dict)
        else {}
    )
    runtime_container_name = runtime_container.get("name")
    runtime_container_id = runtime_container.get("id")
    if (
        runtime_container_name != expected_container
        and not (
            isinstance(runtime_container_id, str)
            and runtime_container_id.startswith(expected_container)
        )
    ):
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate report drifted from its declared live Registry container identity input.",
        )
    if (
        production_registry.get("deleteEnabled") is not False
        or production_registry.get("promotionBoundary") != "digest-only"
        or production_registry.get("releaseEvidenceDays")
        != live_retention_policy["retention"]["releaseEvidenceDays"]
        or production_registry.get("garbageCollectionMode")
        != live_retention_policy["garbageCollection"]["mode"]
        or production_registry.get("archiveRequiredBeforeGc")
        != live_retention_policy["garbageCollection"]["requiresReleaseEvidenceArchive"]
    ):
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate report drifted from the live Registry boundary values it claimed to verify.",
        )
    if source_hashes.get(str(registry_gate.PRODUCTION_REGISTRY_RETENTION_POLICY_PATH)) is None:
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate report omitted the checked-in Registry retention-policy source hash.",
        )
    return (
        {
            "registryConfigPath": str(registry_config_path),
            "retentionPolicyPath": str(retention_policy_path),
            "deleteEnabled": False,
            "promotionBoundary": "digest-only",
            "releaseEvidenceDays": live_retention_policy["retention"]["releaseEvidenceDays"],
            "garbageCollectionMode": live_retention_policy["garbageCollection"]["mode"],
            "archiveRequiredBeforeGc": live_retention_policy["garbageCollection"][
                "requiresReleaseEvidenceArchive"
            ],
            "liveRuntimeEvidence": dict(live_runtime_evidence),
        },
        registry_access,
    )


def load_registry_release_evidence(path: pathlib.Path) -> RegistryReleaseEvidence:
    try:
        raw = path.read_bytes()
    except OSError:
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_missing",
            "The production registry_release_gate report was unavailable.",
            {"path": str(path)},
        ) from None
    try:
        payload = json.loads(raw)
    except json.JSONDecodeError:
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate report was not valid JSON.",
            {"path": str(path)},
        ) from None
    if (
        not isinstance(payload, dict)
        or payload.get("schemaVersion") != registry_gate.SCHEMA_VERSION
        or payload.get("status") != "pass"
        or payload.get("mode") != "worker-registry-release-gate"
    ):
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate report did not describe a passing worker registry release gate.",
        )
    configuration = payload.get("configuration")
    supply_chain_report = payload.get("supplyChain")
    source = payload.get("source")
    source_supply_chain = source.get("supplyChain") if isinstance(source, dict) else None
    builds = payload.get("builds")
    security = payload.get("security")
    output_scan = security.get("outputSecretScan") if isinstance(security, dict) else None
    signing = supply_chain_report.get("signing") if isinstance(supply_chain_report, dict) else None
    signing_policy = (
        source_supply_chain.get("signingPolicy")
        if isinstance(source_supply_chain, dict)
        else None
    )
    production_profile = (
        source_supply_chain.get("productionSigningProfile")
        if isinstance(source_supply_chain, dict)
        else None
    )
    if (
        not isinstance(configuration, dict)
        or configuration.get("signingPolicyProfile") != "production"
        or not isinstance(signing, dict)
        or signing.get("productionSigningPolicySatisfied") is not True
        or signing.get("transparencyLogVerified") is not True
        or signing.get("transparencyLogInclusionProofPresent") is not True
        or signing.get("transparencyLogSignedEntryTimestampPresent") is not True
        or not isinstance(source, dict)
        or source.get("worktreeDirty") is not False
        or not isinstance(source_supply_chain, dict)
        or source_supply_chain.get("signingPolicyProfile") != "production"
        or not isinstance(builds, list)
        or not all(isinstance(item, dict) for item in builds)
    ):
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate report did not satisfy the required production signing boundary.",
        )
    if isinstance(output_scan, dict) and output_scan.get("findings"):
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate report retained secret-like findings.",
            {"findingCount": len(output_scan["findings"])},
        )
    signer_identity = _normalized_registry_signer_identity(signing.get("signerIdentity"))
    image_repository = configuration.get("imageRepository")
    git_sha = source.get("gitSha")
    version = _normalized_release_version(
        source.get("version") if isinstance(source, dict) else None
    )
    run_id = payload.get("runId")
    source_hashes = _normalized_release_source_hashes(
        source.get("sourceHashes") if isinstance(source, dict) else None
    )
    signing_policy_sha256 = signing_policy.get("sha256") if isinstance(signing_policy, dict) else None
    production_signing_profile_sha256 = (
        production_profile.get("sha256") if isinstance(production_profile, dict) else None
    )
    signatures = signing.get("signatures") if isinstance(signing, dict) else None
    if (
        not isinstance(image_repository, str)
        or not image_repository
        or not isinstance(git_sha, str)
        or re.fullmatch(r"[0-9a-f]{40}", git_sha) is None
        or not isinstance(run_id, str)
        or not run_id
        or not isinstance(signing_policy_sha256, str)
        or re.fullmatch(r"[0-9a-f]{64}", signing_policy_sha256) is None
        or not isinstance(production_signing_profile_sha256, str)
        or re.fullmatch(r"[0-9a-f]{64}", production_signing_profile_sha256) is None
        or not isinstance(signatures, list)
        or not all(isinstance(item, dict) for item in signatures)
    ):
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate report omitted its image repository, Git SHA, run ID, or production signing source hashes.",
        )
    registry_root = registry_gate.normalize_image_repository(image_repository)
    if registry_root.startswith("localhost:") or registry_root.startswith("127.0.0.1:"):
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate report used a non-production loopback registry repository.",
            {"imageRepository": registry_root},
        )
    production_registry_boundary, registry_access = _normalized_production_registry_boundary(
        payload,
        image_repository=registry_root,
        source_hashes=source_hashes,
    )
    builds_by_slot = {
        str(item.get("slot")): item for item in builds if isinstance(item.get("slot"), str)
    }
    cached = builds_by_slot.get("cached")
    if cached is None:
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate report omitted the cached build.",
        )
    image_value = cached.get("image")
    digest_value = cached.get("registryDigest")
    if not isinstance(image_value, str) or DIGEST_PATTERN.fullmatch(str(digest_value)) is None:
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate cached build omitted a valid image or digest.",
        )
    tagged = normalize_image_reference(image_value, flag="cached build image", require_digest=False)
    if tagged.repository_root != registry_root:
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate cached build image did not match its configured repository.",
            {
                "expectedRepository": registry_root,
                "actualRepository": tagged.repository_root,
            },
        )
    cached_signed_image = f"{tagged.repository_root}@{digest_value}"
    expected_annotations = {
        "synara.git-sha": git_sha,
        "synara.run-id": run_id,
        "synara.slot": "cached",
        "synara.version": version,
    }
    matching_cached_signatures = [
        item
        for item in signatures
        if item.get("slot") == "cached"
        and item.get("reference") == cached_signed_image
        and item.get("digest") == digest_value
        and item.get("annotations") == expected_annotations
    ]
    if len(matching_cached_signatures) != 1:
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate report did not retain exactly one cached signature with the expected release annotations.",
            {"matchingCachedSignatures": len(matching_cached_signatures)},
        )
    cached_signature_transparency_log = _normalized_cached_signature_transparency_log(
        matching_cached_signatures[0].get("transparencyLog")
    )
    return RegistryReleaseEvidence(
        run_id=run_id,
        report_sha256=sha256_bytes(raw),
        git_sha=git_sha,
        version=version,
        image_repository=registry_root,
        cached_signed_image=cached_signed_image,
        signing_policy_sha256=signing_policy_sha256,
        production_signing_profile_sha256=production_signing_profile_sha256,
        source_hashes=source_hashes,
        transparency_log_verified=True,
        transparency_log_inclusion_proof_present=True,
        transparency_log_signed_entry_timestamp_present=True,
        cached_signature_transparency_log=cached_signature_transparency_log,
        registry_access=registry_access,
        production_registry_boundary=production_registry_boundary,
        signer_identity=signer_identity,
    )


def validate_release_evidence_against_source(
    release_evidence: RegistryReleaseEvidence,
    *,
    source: Mapping[str, Any],
    configuration: supply_chain.SupplyChainConfiguration,
) -> None:
    profile = configuration.production_signing_profile
    source_git_sha = source.get("gitSha")
    current_source_hashes = _source_file_hashes(pathlib.Path(__file__).resolve().parents[2])
    mismatches: dict[str, Any] = {}
    if not isinstance(source_git_sha, str):
        raise ReleaseGateError(
            "release.vault_kms_source_invalid",
            "The Vault KMS admission gate source metadata omitted its Git SHA.",
        )
    if release_evidence.git_sha != source_git_sha:
        mismatches["expectedGitSha"] = source_git_sha
        mismatches["actualGitSha"] = release_evidence.git_sha
    if release_evidence.signing_policy_sha256 != configuration.signing_policy.sha256:
        mismatches["expectedSigningPolicySha256"] = configuration.signing_policy.sha256
        mismatches["actualSigningPolicySha256"] = release_evidence.signing_policy_sha256
    if profile is None:
        raise ReleaseGateError(
            "release.vault_kms_source_invalid",
            "The checked-in production signing profile was unavailable.",
        )
    if release_evidence.production_signing_profile_sha256 != profile.sha256:
        mismatches["expectedProductionSigningProfileSha256"] = profile.sha256
        mismatches["actualProductionSigningProfileSha256"] = (
            release_evidence.production_signing_profile_sha256
        )
    source_hash_mismatches = {
        path: {
            "expectedSha256": current_hash,
            "actualSha256": release_evidence.source_hashes.get(path),
        }
        for path, current_hash in current_source_hashes.items()
        if release_evidence.source_hashes.get(path) != current_hash
    }
    if source_hash_mismatches:
        mismatches["sourceHashMismatchCount"] = len(source_hash_mismatches)
    if mismatches:
        evidence: dict[str, Any] = dict(mismatches)
        if source_hash_mismatches:
            evidence["sourceHashMismatches"] = source_hash_mismatches
        raise ReleaseGateError(
            "release.vault_kms_registry_release_gate_invalid",
            "The production registry_release_gate report did not match the current clean source boundary.",
            evidence,
        )


def validate_registry_signer_identity_against_live(
    release_evidence: RegistryReleaseEvidence,
    vault_evidence: Mapping[str, Any],
) -> None:
    vault_details = vault_evidence.get("vault")
    identities = vault_details.get("identities") if isinstance(vault_details, dict) else None
    signer = identities.get("signer") if isinstance(identities, dict) else None
    expected = release_evidence.signer_identity
    if (
        not isinstance(signer, dict)
        or signer.get("roleName") != expected.get("roleName")
        or signer.get("tokenType") != expected.get("type")
        or signer.get("orphan") is not expected.get("orphan")
        or signer.get("policyHash") != expected.get("policiesSha256")
        or signer.get("registryIdentitySha256") != expected.get("identitySha256")
    ):
        raise ReleaseGateError(
            "release.vault_kms_signer_identity_drift",
            "The live Vault signer identity did not match the identity that signed the production image.",
            {
                "releaseIdentitySha256": expected.get("identitySha256"),
                "liveIdentitySha256": (
                    signer.get("registryIdentitySha256") if isinstance(signer, dict) else None
                ),
            },
        )


def choose_signed_image(
    explicit: str | None,
    release_evidence: RegistryReleaseEvidence,
) -> ImageReference:
    expected = normalize_image_reference(
        release_evidence.cached_signed_image,
        flag="signed image reference",
        require_digest=True,
    )
    if explicit is None:
        return expected
    parsed = normalize_image_reference(
        explicit,
        flag="signed image reference",
        require_digest=True,
    )
    if parsed.repository_root != expected.repository_root:
        raise ReleaseGateError(
            "release.vault_kms_image_identity_invalid",
            "The admitted signed image did not match the production registry_release_gate repository.",
            {
                "expectedRepository": expected.repository_root,
                "actualRepository": parsed.repository_root,
            },
        )
    if parsed.digest != expected.digest:
        raise ReleaseGateError(
            "release.vault_kms_image_identity_invalid",
            "The admitted signed image digest did not match the production registry_release_gate cached signature digest.",
            {
                "expectedDigest": expected.digest,
                "actualDigest": parsed.digest,
            },
        )
    return parsed


def ensure_matching_repository(
    reference: ImageReference,
    *,
    expected_repository: str,
    code: str,
    label: str,
) -> None:
    if reference.repository_root != expected_repository:
        raise ReleaseGateError(
            code,
            f"The {label} did not use the production worker repository.",
            {
                "expectedRepository": expected_repository,
                "actualRepository": reference.repository_root,
            },
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
            "release.vault_kms_environment_invalid",
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
    options: GateOptions,
    *,
    state_dir: pathlib.Path,
    redactor: acceptance.SecretRedactor,
) -> SecretInputs:
    vault_addr = _read_environment_value(options.vault_address_env, "Vault address")
    vault_token = _read_environment_value(options.vault_token_env, "Vault token")
    vault_operator_token = _read_environment_value(
        options.vault_operator_token_env,
        "Vault operator token",
    )
    if vault_token == vault_operator_token:
        raise ReleaseGateError(
            "release.vault_kms_identity_invalid",
            "The Vault signer and operator credentials must use different tokens.",
            {
                "signerTokenEnvironment": options.vault_token_env,
                "operatorTokenEnvironment": options.vault_operator_token_env,
            },
        )
    vault_cacert = _read_environment_value(options.vault_cacert_env, "Vault CA certificate path")
    registry_ca = _read_environment_value(options.registry_ca_cert_env, "Registry CA certificate path")
    registry_username = _read_environment_value(options.registry_username_env, "Registry Basic auth username")
    registry_password = _read_environment_value(options.registry_password_env, "Registry Basic auth password")
    redactor.add(vault_token, "[REDACTED_VAULT_TOKEN]")
    redactor.add(vault_operator_token, "[REDACTED_VAULT_OPERATOR_TOKEN]")
    redactor.add(registry_username, "[REDACTED_REGISTRY_USERNAME]")
    redactor.add(registry_password, "[REDACTED_REGISTRY_PASSWORD]")
    redactor.add(f"{registry_username}:{registry_password}", "[REDACTED_REGISTRY_BASIC_AUTH]")
    redactor.add(base64.b64encode(f"{registry_username}:{registry_password}".encode("utf-8")).decode("ascii"), "[REDACTED_REGISTRY_BASIC_AUTH_B64]")
    materialized_vault_cacert = _materialize_file_copy(
        vault_cacert,
        state_dir / "certificates" / "vault-ca.crt",
        code="release.vault_kms_environment_invalid",
        message="The configured Vault CA certificate path was unavailable or invalid.",
    )
    materialized_registry_ca = _materialize_file_copy(
        registry_ca,
        state_dir / "certificates" / "registry-ca.crt",
        code="release.vault_kms_environment_invalid",
        message="The configured Registry CA certificate path was unavailable or invalid.",
    )
    return SecretInputs(
        vault_environment={
            "VAULT_ADDR": vault_addr,
            "VAULT_TOKEN": vault_token,
            "VAULT_CACERT": str(materialized_vault_cacert),
        },
        vault_operator_environment={
            "VAULT_ADDR": vault_addr,
            "VAULT_TOKEN": vault_operator_token,
            "VAULT_CACERT": str(materialized_vault_cacert),
        },
        registry_ca_path=materialized_registry_ca,
        registry_username=registry_username,
        registry_password=registry_password,
        registry_username_env=options.registry_username_env,
        registry_password_env=options.registry_password_env,
        registry_ca_env=options.registry_ca_cert_env,
        vault_env_names=(
            options.vault_address_env,
            options.vault_token_env,
            options.vault_operator_token_env,
            options.vault_cacert_env,
        ),
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
    redactor: acceptance.SecretRedactor,
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
        raise ReleaseGateError(
            code,
            message,
            {"executable": pathlib.Path(executable).name},
        ) from None


def _output_excerpt(completed: subprocess.CompletedProcess[str], redactor: acceptance.SecretRedactor) -> str:
    combined = (completed.stdout + "\n" + completed.stderr).strip()
    return redactor.text(combined)[:MAX_OUTPUT_EXCERPT]


def kubectl_completed(
    options: GateOptions,
    arguments: Sequence[str],
    *,
    redactor: acceptance.SecretRedactor,
    input_text: str | None = None,
    timeout: float,
) -> subprocess.CompletedProcess[str]:
    return _run_command(
        options.kubectl_bin,
        ["--context", options.kube_context, *arguments],
        cwd=options.repo_root,
        input_text=input_text,
        timeout=timeout,
        redactor=redactor,
        code="release.vault_kms_kubectl_failed",
        message="A required Kubernetes command could not complete.",
    )


def kubectl_json(
    options: GateOptions,
    arguments: Sequence[str],
    *,
    redactor: acceptance.SecretRedactor,
    timeout: float,
) -> Any:
    completed = kubectl_completed(
        options,
        arguments,
        redactor=redactor,
        timeout=timeout,
    )
    if completed.returncode != 0:
        raise ReleaseGateError(
            "release.vault_kms_kubectl_failed",
            "A required Kubernetes command returned a failure.",
            {"outputExcerpt": _output_excerpt(completed, redactor)},
        )
    try:
        return json.loads(completed.stdout)
    except json.JSONDecodeError:
        raise ReleaseGateError(
            "release.vault_kms_kubectl_invalid",
            "A required Kubernetes command did not return valid JSON.",
        ) from None


def vault_json(
    options: GateOptions,
    secret_inputs: SecretInputs,
    arguments: Sequence[str],
    *,
    vault_environment: Mapping[str, str] | None = None,
    redactor: acceptance.SecretRedactor,
    timeout: float,
) -> Mapping[str, Any]:
    completed = vault_completed(
        options.vault_bin,
        options,
        secret_inputs,
        arguments,
        vault_environment=vault_environment,
        timeout=timeout,
        redactor=redactor,
    )
    if completed.returncode != 0:
        raise ReleaseGateError(
            "release.vault_kms_vault_failed",
            "A required Vault command returned a failure.",
            {"outputExcerpt": _output_excerpt(completed, redactor)},
        )
    try:
        payload = json.loads(completed.stdout)
    except json.JSONDecodeError:
        raise ReleaseGateError(
            "release.vault_kms_vault_invalid",
            "A required Vault command did not return valid JSON.",
        ) from None
    if not isinstance(payload, dict):
        raise ReleaseGateError(
            "release.vault_kms_vault_invalid",
            "A required Vault command did not return a JSON object.",
        )
    return payload


def vault_completed(
    executable: str,
    options: GateOptions,
    secret_inputs: SecretInputs,
    arguments: Sequence[str],
    *,
    vault_environment: Mapping[str, str] | None = None,
    redactor: acceptance.SecretRedactor,
    timeout: float,
) -> subprocess.CompletedProcess[str]:
    return _run_command(
        executable,
        arguments,
        cwd=options.repo_root,
        environment=vault_environment or secret_inputs.vault_environment,
        timeout=timeout,
        redactor=redactor,
        code="release.vault_kms_vault_failed",
        message="A required Vault command could not complete.",
    )


def vault_text(
    options: GateOptions,
    secret_inputs: SecretInputs,
    arguments: Sequence[str],
    *,
    vault_environment: Mapping[str, str] | None = None,
    redactor: acceptance.SecretRedactor,
    timeout: float,
) -> str:
    completed = vault_completed(
        options.vault_bin,
        options,
        secret_inputs,
        arguments,
        vault_environment=vault_environment,
        redactor=redactor,
        timeout=timeout,
    )
    if completed.returncode != 0:
        raise ReleaseGateError(
            "release.vault_kms_vault_failed",
            "A required Vault command returned a failure.",
            {"outputExcerpt": _output_excerpt(completed, redactor)},
        )
    return completed.stdout


def _probe_unauthenticated_generate_root_attempt_status(
    vault_address: str,
    *,
    vault_cacert: pathlib.Path,
    timeout: float,
) -> int:
    parsed_address = urllib.parse.urlsplit(vault_address)
    request_path = f"{parsed_address.path.rstrip('/')}/v1/sys/generate-root/attempt"
    if not request_path.startswith("/"):
        request_path = f"/{request_path}"
    connection = http.client.HTTPSConnection(
        parsed_address.netloc,
        timeout=timeout,
        context=ssl.create_default_context(cafile=str(vault_cacert)),
    )
    try:
        connection.request("GET", request_path)
        response = connection.getresponse()
        body = response.read(VAULT_BREAK_GLASS_PROBE_MAX_RESPONSE_BYTES + 1)
        if len(body) > VAULT_BREAK_GLASS_PROBE_MAX_RESPONSE_BYTES:
            raise ReleaseGateError(
                "release.vault_kms_vault_failed",
                "The unauthenticated Vault generate-root boundary probe returned an oversized response.",
                {"path": "/v1/sys/generate-root/attempt"},
            )
        return response.status
    except (OSError, ValueError, http.client.HTTPException):
        raise ReleaseGateError(
            "release.vault_kms_vault_failed",
            "The unauthenticated Vault generate-root boundary probe could not complete.",
            {"path": "/v1/sys/generate-root/attempt"},
        ) from None
    finally:
        connection.close()


def cosign_completed(
    options: GateOptions,
    secret_inputs: SecretInputs,
    arguments: Sequence[str],
    *,
    redactor: acceptance.SecretRedactor,
    timeout: float,
) -> subprocess.CompletedProcess[str]:
    return _run_command(
        options.cosign_bin,
        arguments,
        cwd=options.repo_root,
        environment=secret_inputs.vault_environment,
        timeout=timeout,
        redactor=redactor,
        code="release.vault_kms_cosign_failed",
        message="A required Cosign command could not complete.",
    )


def _json_object(value: Any, *, code: str, message: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise ReleaseGateError(code, message)
    return value


def _stringify_json(value: Any) -> str:
    return json.dumps(value, sort_keys=True, separators=(",", ":"))


def _normalized_unauthenticated_access_configuration(
    config_text: Any,
    *,
    code: str,
    message: str,
) -> list[str]:
    if not isinstance(config_text, str):
        raise ReleaseGateError(code, message)
    matches = re.findall(
        r'(?m)^\s*enable_unauthenticated_access\s*=\s*\[(?P<items>[^\]]*)\]\s*$',
        config_text,
    )
    if len(matches) != 1:
        raise ReleaseGateError(code, message, {"matchCount": len(matches)})
    raw_items = matches[0].strip()
    if not raw_items:
        return []
    normalized: list[str] = []
    for raw_item in raw_items.split(","):
        match = re.fullmatch(r'"([A-Za-z0-9._/-]+)"', raw_item.strip())
        if match is None:
            raise ReleaseGateError(code, message)
        normalized.append(match.group(1))
    return normalized


def _normalized_retry_join_configuration(
    config_text: Any,
    *,
    code: str,
) -> list[dict[str, str]]:
    if not isinstance(config_text, str):
        raise ReleaseGateError(code, "The Vault Raft retry_join configuration was unavailable.")
    lowered = config_text.lower()
    if (
        "tls_skip" in lowered
        or "leader_client_cert_file" in lowered
        or "leader_client_key_file" in lowered
    ):
        raise ReleaseGateError(
            code,
            "The Vault Raft retry_join configuration weakened the CA-only TLS boundary.",
        )
    blocks = re.findall(r"(?ms)^\s*retry_join\s*\{(?P<body>.*?)^\s*\}", config_text)
    normalized: list[dict[str, str]] = []
    for body in blocks:
        fields: dict[str, str] = {}
        for raw_line in body.splitlines():
            line = raw_line.strip()
            if not line or line.startswith("#") or line.startswith("//"):
                continue
            match = re.fullmatch(r'([a-z_]+)\s*=\s*"([^"]+)"', line)
            if match is None or match.group(1) in fields:
                raise ReleaseGateError(
                    code,
                    "The Vault Raft retry_join block contained an unexpected field.",
                )
            fields[match.group(1)] = match.group(2)
        if set(fields) != {"leader_api_addr", "leader_ca_cert_file"}:
            raise ReleaseGateError(
                code,
                "The Vault Raft retry_join block did not use the exact CA-only field set.",
            )
        normalized.append(fields)
    addresses = [item["leader_api_addr"] for item in normalized]
    if (
        len(blocks) != REQUIRED_VAULT_PEERS
        or len(set(addresses)) != REQUIRED_VAULT_PEERS
        or set(addresses) != set(VAULT_RETRY_JOIN_ADDRESSES)
        or any(
            item["leader_ca_cert_file"] != VAULT_RETRY_JOIN_CA_CERT_FILE
            for item in normalized
        )
    ):
        raise ReleaseGateError(
            code,
            "The Vault Raft retry_join configuration did not contain the exact three TLS peers.",
            {"peerCount": len(blocks), "uniquePeerCount": len(set(addresses))},
        )
    return sorted(normalized, key=lambda item: item["leader_api_addr"])


def _load_vault_baseline(repo_root: pathlib.Path) -> dict[str, Any]:
    values_text = _read_text_file(
        repo_root / VAULT_VALUES_PRODUCTION_PATH,
        code="release.vault_kms_source_invalid",
        message="The checked-in Vault production values were unavailable.",
    )
    pdb_text = _read_text_file(
        repo_root / VAULT_SERVER_PDB_PATH,
        code="release.vault_kms_source_invalid",
        message="The checked-in Vault PodDisruptionBudget manifest was unavailable.",
    )
    audit_observability_text = _read_text_file(
        repo_root / VAULT_AUDIT_OBSERVABILITY_CONFIGMAP_PATH,
        code="release.vault_kms_source_invalid",
        message="The checked-in Vault audit observability ConfigMap was unavailable.",
    )
    bootstrap_text = _read_text_file(
        repo_root / VAULT_BOOTSTRAP_PATH,
        code="release.vault_kms_source_invalid",
        message="The checked-in Vault production bootstrap was unavailable.",
    )
    operations_policy = _read_json_file(
        repo_root / VAULT_OPERATIONS_POLICY_PATH,
        code="release.vault_kms_source_invalid",
        message="The checked-in Vault operations policy was unavailable or malformed.",
    )
    helm_post_renderer_text = _read_text_file(
        repo_root / VAULT_HELM_POST_RENDERER_PATH,
        code="release.vault_kms_source_invalid",
        message="The checked-in Vault Helm post-renderer was unavailable.",
    )
    helm_post_renderer_plugin_text = _read_text_file(
        repo_root / VAULT_HELM_POST_RENDERER_PLUGIN_PATH,
        code="release.vault_kms_source_invalid",
        message="The checked-in Vault Helm post-renderer plugin was unavailable.",
    )
    secret_match = re.search(r"secretName:\s*([A-Za-z0-9.-]+)", values_text)
    pdb_name_match = re.search(r"name:\s*([A-Za-z0-9.-]+)", pdb_text)
    pdb_match = re.search(r"minAvailable:\s*([0-9]+)", pdb_text)
    audit_observability_name_match = re.search(
        r"(?m)^  name:\s*([A-Za-z0-9.-]+)\s*$",
        audit_observability_text,
    )
    image_repository = _yaml_scalar_value(values_text, ("server", "image", "repository"))
    image_tag = _yaml_scalar_value(values_text, ("server", "image", "tag"))
    audit_observability_data = _literal_configmap_data(
        audit_observability_text,
        code="release.vault_kms_source_invalid",
    )
    vector_config = audit_observability_data.get(VAULT_AUDIT_VECTOR_CONFIG_KEY)
    shipper_script = audit_observability_data.get(VAULT_AUDIT_SHIPPER_SCRIPT_KEY)
    rotation_script = audit_observability_data.get(VAULT_AUDIT_ROTATION_SCRIPT_KEY)
    audit_policy = operations_policy.get("audit") if isinstance(operations_policy, dict) else None
    vault_operations_policy = (
        operations_policy.get("vault") if isinstance(operations_policy, dict) else None
    )
    transit_key_policy = (
        vault_operations_policy.get("transitKey")
        if isinstance(vault_operations_policy, dict)
        else None
    )
    local_rotation_policy = (
        audit_policy.get("localRotation") if isinstance(audit_policy, dict) else None
    )
    external_siem_policy = (
        audit_policy.get("externalSiem") if isinstance(audit_policy, dict) else None
    )
    custody_policy = operations_policy.get("custody") if isinstance(operations_policy, dict) else None
    root_generation_break_glass_policy = (
        operations_policy.get("rootGenerationBreakGlass")
        if isinstance(operations_policy, dict)
        else None
    )
    steady_state_break_glass_policy = (
        root_generation_break_glass_policy.get("steadyState")
        if isinstance(root_generation_break_glass_policy, dict)
        else None
    )
    temporary_break_glass_policy = (
        root_generation_break_glass_policy.get("temporaryWindow")
        if isinstance(root_generation_break_glass_policy, dict)
        else None
    )
    break_glass_reload_policy = (
        temporary_break_glass_policy.get("configReload")
        if isinstance(temporary_break_glass_policy, dict)
        else None
    )
    break_glass_quorum_policy = (
        temporary_break_glass_policy.get("requiredQuorum")
        if isinstance(temporary_break_glass_policy, dict)
        else None
    )
    break_glass_post_close_policy = (
        temporary_break_glass_policy.get("postClose")
        if isinstance(temporary_break_glass_policy, dict)
        else None
    )
    enable_unauthenticated_access = _normalized_unauthenticated_access_configuration(
        values_text,
        code="release.vault_kms_source_invalid",
        message="The checked-in Vault production baseline did not define a single top-level unauthenticated-access policy.",
    )
    expected_liveness_probe = _expected_vault_liveness_probe()
    expected_liveness_exec = """    execCommand:
      - /bin/sh
      - -c
      - 'vault status >/dev/null 2>&1; status=$?; [ "$status" -eq 0 ] || [ "$status" -eq 2 ]'"""
    values_scalars_valid = (
        _yaml_scalar_value(values_text, ("ui", "enabled")) == "false"
        and _yaml_scalar_value(values_text, ("server", "terminationGracePeriodSeconds")) == "30"
        and _yaml_scalar_value(values_text, ("server", "shareProcessNamespace")) == "true"
        and _yaml_scalar_value(values_text, ("server", "livenessProbe", "enabled")) == "true"
        and _yaml_scalar_value(values_text, ("server", "livenessProbe", "initialDelaySeconds"))
        == str(expected_liveness_probe["initialDelaySeconds"])
        and _yaml_scalar_value(values_text, ("server", "livenessProbe", "periodSeconds"))
        == str(expected_liveness_probe["periodSeconds"])
        and _yaml_scalar_value(values_text, ("server", "livenessProbe", "failureThreshold"))
        == str(expected_liveness_probe["failureThreshold"])
        and _yaml_scalar_value(values_text, ("server", "livenessProbe", "successThreshold"))
        == str(expected_liveness_probe["successThreshold"])
        and _yaml_scalar_value(values_text, ("server", "livenessProbe", "timeoutSeconds"))
        == str(expected_liveness_probe["timeoutSeconds"])
        and _yaml_scalar_value(values_text, ("server", "readinessProbe", "enabled")) == "true"
        and _yaml_scalar_value(values_text, ("server", "readinessProbe", "path"))
        == VAULT_READINESS_HEALTH_PATH
        and _yaml_scalar_value(values_text, ("server", "readinessProbe", "port")) == "8200"
        and _yaml_scalar_value(values_text, ("server", "readinessProbe", "initialDelaySeconds"))
        == str(expected_liveness_probe["initialDelaySeconds"])
        and _yaml_scalar_value(values_text, ("server", "readinessProbe", "periodSeconds"))
        == str(expected_liveness_probe["periodSeconds"])
        and _yaml_scalar_value(values_text, ("server", "readinessProbe", "failureThreshold"))
        == str(expected_liveness_probe["failureThreshold"])
        and _yaml_scalar_value(values_text, ("server", "readinessProbe", "successThreshold"))
        == str(expected_liveness_probe["successThreshold"])
        and _yaml_scalar_value(values_text, ("server", "readinessProbe", "timeoutSeconds"))
        == str(expected_liveness_probe["timeoutSeconds"])
        and expected_liveness_exec in values_text
        and "tls-skip-verify" not in values_text
        and _yaml_scalar_value(values_text, ("server", "networkPolicy", "enabled")) == "true"
        and _yaml_scalar_value(values_text, ("server", "extraEnvironmentVars", "VAULT_CACERT"))
        == "/vault/tls/ca.crt"
        and "        ui = false" in values_text
    )
    expected_peer_ingress = """      - from:
          - podSelector:
              matchLabels:
                app.kubernetes.io/name: vault
                app.kubernetes.io/instance: synara-vault
                component: server
        ports:
          - port: 8200
            protocol: TCP
          - port: 8201
            protocol: TCP"""
    expected_client_ingress = """      - from:
          - namespaceSelector:
              matchLabels:
                kubernetes.io/metadata.name: synara-system
        ports:
          - port: 8200
            protocol: TCP"""
    expected_dns_egress = f"""      - to:
          - namespaceSelector:
              matchLabels:
                kubernetes.io/metadata.name: {VAULT_DNS_NAMESPACE_LABELS["kubernetes.io/metadata.name"]}
            podSelector:
              matchLabels:
                k8s-app: {VAULT_DNS_POD_LABELS["k8s-app"]}
        ports:
          - port: 53
            protocol: UDP
          - port: 53
            protocol: TCP"""
    expected_kubernetes_service_egress = f"""      - to:
          - ipBlock:
              cidr: {VAULT_KUBERNETES_SERVICE_IP_CIDR}
        ports:
          - port: 443
            protocol: TCP"""
    expected_apiserver_egress = f"""      - to:
          - ipBlock:
              cidr: {VAULT_KUBERNETES_APISERVER_CIDR}
        ports:
          - port: 6443
            protocol: TCP"""
    expected_siem_egress = f"""      - to:
          - ipBlock:
              cidr: {VAULT_AUDIT_SIEM_EGRESS_CIDR}
        ports:
          - port: {VAULT_AUDIT_SIEM_EGRESS_PORT}
            protocol: TCP"""
    required_sidecar_fragments = (
        f"name: {VAULT_AUDIT_OBSERVABILITY_VOLUME_NAME}",
        f"name: {VAULT_AUDIT_OBSERVABILITY_CONFIGMAP_NAME}",
        f"name: {VAULT_AUDIT_SIEM_TLS_VOLUME_NAME}",
        f"image: {VAULT_AUDIT_SHIPPER_IMAGE_REFERENCE}",
        f"image: {VAULT_AUDIT_ROTATION_IMAGE_REFERENCE}",
        f"- name: {VAULT_AUDIT_SHIPPER_NAME}",
        f"- name: {VAULT_AUDIT_ROTATION_NAME}",
        f"mountPath: {VAULT_AUDIT_CONFIG_MOUNT_PATH}",
        f"mountPath: {VAULT_AUDIT_SIEM_TLS_MOUNT_PATH}",
        f"name: {VAULT_AUDIT_SIEM_SECRET_NAME}",
        "runAsNonRoot: true",
        f"runAsUser: {VAULT_AUDIT_RUN_AS_USER}",
        f"runAsGroup: {VAULT_AUDIT_RUN_AS_GROUP}",
        "readOnlyRootFilesystem: true",
        "allowPrivilegeEscalation: false",
        "type: RuntimeDefault",
        f"key: {VAULT_AUDIT_SIEM_ENVIRONMENT[0][1]}",
        f"key: {VAULT_AUDIT_SIEM_ENVIRONMENT[1][1]}",
        f"key: {VAULT_AUDIT_SIEM_ENVIRONMENT[2][1]}",
        f"key: {VAULT_AUDIT_SIEM_ENVIRONMENT[3][1]}",
    )
    required_bootstrap_fragments = (
        'ROLE_TOKEN_TTL="${ROLE_TOKEN_TTL:-2h}"',
        'ROLE_TOKEN_MAX_TTL="${ROLE_TOKEN_MAX_TTL:-4h}"',
        'ROLE_TOKEN_NUM_USES="${ROLE_TOKEN_NUM_USES:-0}"',
        'ROLE_SECRET_ID_TTL="${ROLE_SECRET_ID_TTL:-10m}"',
        'AUDITOR_TOKEN_TTL="${AUDITOR_TOKEN_TTL:-30m}"',
        'AUDITOR_TOKEN_MAX_TTL="${AUDITOR_TOKEN_MAX_TTL:-1h}"',
        'SNAPSHOT_TOKEN_TTL="${SNAPSHOT_TOKEN_TTL:-30m}"',
        'SNAPSHOT_TOKEN_MAX_TTL="${SNAPSHOT_TOKEN_MAX_TTL:-1h}"',
        f'AUDIT_DEVICE_PATH_PRIMARY="${{AUDIT_DEVICE_PATH_PRIMARY:-{VAULT_AUDIT_DEVICE_PRIMARY_NAME}}}"',
        f'AUDIT_LOG_FILE_PRIMARY="${{AUDIT_LOG_FILE_PRIMARY:-{VAULT_AUDIT_DEVICE_PRIMARY_FILE_PATH}}}"',
        f'AUDIT_DEVICE_PATH_SECONDARY="${{AUDIT_DEVICE_PATH_SECONDARY:-{VAULT_AUDIT_DEVICE_SECONDARY_NAME}}}"',
        f'AUDIT_LOG_FILE_SECONDARY="${{AUDIT_LOG_FILE_SECONDARY:-{VAULT_AUDIT_DEVICE_SECONDARY_FILE_PATH}}}"',
        'SNAPSHOT_POLICY_TEMPLATE="${SCRIPT_DIR}/../synara-vault-snapshot-operator.hcl"',
        'SNAPSHOT_POLICY_NAME="${VAULT_SNAPSHOT_POLICY_NAME:-synara-vault-snapshot-operator}"',
        'SNAPSHOT_APPROLE_NAME="${VAULT_SNAPSHOT_APPROLE_NAME:-synara-vault-snapshot-operator}"',
        '"${TRANSIT_MOUNT}/keys/${TRANSIT_KEY_NAME}/config"',
        "deletion_allowed=false",
        "exportable=false",
        "allow_plaintext_backup=false",
        "auto_rotate_period=0",
        "disable_unexpected_audit_devices() {",
        'ensure_file_audit_device "${AUDIT_DEVICE_PATH_PRIMARY}" "${AUDIT_LOG_FILE_PRIMARY}"',
        'ensure_file_audit_device "${AUDIT_DEVICE_PATH_SECONDARY}" "${AUDIT_LOG_FILE_SECONDARY}"',
        "bind_secret_id=true",
        "token_type=batch",
        "token_no_default_policy=true",
    )
    required_vector_fragments = (
        f"data_dir: {VAULT_AUDIT_MOUNT_PATH}/vector-data",
        f"      - {VAULT_AUDIT_DEVICE_PRIMARY_FILE_PATH}\n",
        f"      - {VAULT_AUDIT_DEVICE_PRIMARY_FILE_PATH}.*\n",
        f"    exclude:\n      - {VAULT_AUDIT_DEVICE_PRIMARY_FILE_PATH}.*.gz\n",
        "strategy: device_and_inode",
        "offset_key: offset",
        "vault_audit_primary_json:",
        ".vault_audit = parse_json!(string!(.message))",
        "del(.message)",
        "max_size: 536870912",
        "when_full: block",
    )
    required_rotation_fragments = (
        f"MAX_BYTES={VAULT_AUDIT_ROTATION_MAX_BYTES}",
        f"KEEP_ARCHIVES={VAULT_AUDIT_ROTATION_KEEP_ARCHIVES}",
        f"ROTATION_INTERVAL_SECONDS={VAULT_AUDIT_ROTATION_INTERVAL_SECONDS}",
        f"FD_WAIT_ATTEMPTS={VAULT_AUDIT_FD_WAIT_ATTEMPTS}",
        "find_vault_server_pid() {",
        'cmdline="$(tr \'\\000\' \' \' < "${proc_dir}/cmdline" 2>/dev/null || true)"',
        '*"vault server"*)',
        'mv -- "${current_file}" "${archived_file}"',
        'chmod 0600 "${current_file}"',
        "ensure_active_file() {",
        'ensure_active_file "${current_file}"',
        "vault_fd_points_to() {",
        "wait_for_vault_fd_rollover() {",
        'kill -HUP "${vault_pid}"',
        'wait_for_vault_fd_rollover "${vault_pid}" "${rotation_plan}"',
        'gzip -f -- "${candidate}"',
        'rm -f -- "${candidate}"',
        'if ! vault_fd_points_to "${vault_pid}" "${current_file}" || vault_fd_points_to "${vault_pid}" "${archived_file}"; then',
        f"for current_file in {VAULT_AUDIT_DEVICE_PRIMARY_FILE_PATH} {VAULT_AUDIT_DEVICE_SECONDARY_FILE_PATH}; do",
        'compress_stale_archives "${current_file}"',
        'prune_archives "${current_file}"',
    )
    forbidden_rotation_fragments = (
        'cp "${current_file}"',
        "copytruncate",
        'gzip -f "${archived_file}"',
    )
    retry_join = _normalized_retry_join_configuration(
        values_text,
        code="release.vault_kms_source_invalid",
    )
    operations_policy_valid = (
        isinstance(audit_policy, dict)
        and isinstance(vault_operations_policy, dict)
        and isinstance(custody_policy, dict)
        and vault_operations_policy.get("transitKeyName") == EXPECTED_VAULT_KEY_NAME
        and transit_key_policy
        == {
            "type": EXPECTED_VAULT_KEY_TYPE,
            "exportable": False,
            "allowPlaintextBackup": False,
            "deletionAllowed": False,
            "derived": False,
            "autoRotatePeriodSeconds": EXPECTED_VAULT_KEY_AUTO_ROTATE_PERIOD_SECONDS,
            "rotationMode": "staged-manual-with-admission-key-overlap",
            "rotationCadence": "At least annually and immediately after suspected signer or key-boundary compromise.",
        }
        and isinstance(local_rotation_policy, dict)
        and isinstance(external_siem_policy, dict)
        and local_rotation_policy.get("required") is True
        and local_rotation_policy.get("image") == VAULT_AUDIT_ROTATION_IMAGE_REFERENCE
        and local_rotation_policy.get("configMapName") == VAULT_AUDIT_OBSERVABILITY_CONFIGMAP_NAME
        and local_rotation_policy.get("scriptKey") == VAULT_AUDIT_ROTATION_SCRIPT_KEY
        and local_rotation_policy.get("shareProcessNamespace") is True
        and local_rotation_policy.get("runAsUser") == VAULT_AUDIT_RUN_AS_USER
        and local_rotation_policy.get("runAsGroup") == VAULT_AUDIT_RUN_AS_GROUP
        and local_rotation_policy.get("activeFilePermissions")
        == VAULT_AUDIT_ACTIVE_FILE_PERMISSIONS
        and local_rotation_policy.get("initializeMissingActiveFiles") is True
        and local_rotation_policy.get("reloadSignal") == "SIGHUP"
        and local_rotation_policy.get("fdRolloverVerification")
        == "wait until Vault no longer holds /proc fd references to renamed archives and has reopened recreated active files"
        and local_rotation_policy.get("maxMiBPerFile") == 100
        and local_rotation_policy.get("retainedFilesPerStream") == VAULT_AUDIT_ROTATION_KEEP_ARCHIVES
        and local_rotation_policy.get("compression") == "gzip"
        and local_rotation_policy.get("delayCompressNewestArchive") is True
        and external_siem_policy.get("required") is True
        and external_siem_policy.get("image") == VAULT_AUDIT_SHIPPER_IMAGE_REFERENCE
        and external_siem_policy.get("configMapName") == VAULT_AUDIT_OBSERVABILITY_CONFIGMAP_NAME
        and external_siem_policy.get("configKey") == VAULT_AUDIT_VECTOR_CONFIG_KEY
        and external_siem_policy.get("bootstrapScriptKey") == VAULT_AUDIT_SHIPPER_SCRIPT_KEY
        and external_siem_policy.get("secretName") == VAULT_AUDIT_SIEM_SECRET_NAME
        and external_siem_policy.get("includePaths")
        == [
            VAULT_AUDIT_DEVICE_PRIMARY_FILE_PATH,
            f"{VAULT_AUDIT_DEVICE_PRIMARY_FILE_PATH}.*",
        ]
        and external_siem_policy.get("excludePaths")
        == [f"{VAULT_AUDIT_DEVICE_PRIMARY_FILE_PATH}.*.gz"]
        and external_siem_policy.get("parseJsonPerLine") is True
        and external_siem_policy.get("offsetKey") == "offset"
        and external_siem_policy.get("parsedEventField") == "vault_audit"
        and external_siem_policy.get("preservedEnvelopeFields") == ["file", "host", "offset"]
        and external_siem_policy.get("endpointEnvironment") == VAULT_AUDIT_SIEM_ENVIRONMENT[0][0]
        and external_siem_policy.get("clientCertificateEnvironment")
        == VAULT_AUDIT_SIEM_ENVIRONMENT[1][0]
        and external_siem_policy.get("clientKeyEnvironment") == VAULT_AUDIT_SIEM_ENVIRONMENT[2][0]
        and external_siem_policy.get("caCertificateEnvironment") == VAULT_AUDIT_SIEM_ENVIRONMENT[3][0]
        and external_siem_policy.get("secretKeys")
        == {env_name: secret_key for env_name, secret_key in VAULT_AUDIT_SIEM_ENVIRONMENT}
        and external_siem_policy.get("allowedEgress")
        == [
            {
                "cidr": VAULT_AUDIT_SIEM_EGRESS_CIDR,
                "port": VAULT_AUDIT_SIEM_EGRESS_PORT,
                "protocol": "TCP",
            }
        ]
        and enable_unauthenticated_access == []
        and isinstance(root_generation_break_glass_policy, dict)
        and isinstance(steady_state_break_glass_policy, dict)
        and isinstance(temporary_break_glass_policy, dict)
        and steady_state_break_glass_policy.get("enableUnauthenticatedAccess") == []
        and steady_state_break_glass_policy.get("expectedAttemptStatusCode") == 403
        and temporary_break_glass_policy.get("enableUnauthenticatedAccess")
        == [VAULT_BREAK_GLASS_UNAUTHENTICATED_ACCESS]
        and temporary_break_glass_policy.get("maximumDurationMinutes")
        == VAULT_BREAK_GLASS_MAXIMUM_WINDOW_MINUTES
        and temporary_break_glass_policy.get("expectedAttemptStatusCode") == 200
        and isinstance(break_glass_reload_policy, dict)
        and break_glass_reload_policy
        == {
            "mode": "temporary-top-level-hcl-edit-plus-sighup",
            "targetProcess": "/bin/vault server",
            "requireExactPidTargeting": True,
            "restoreCheckedInConfigAfterWindow": True,
        }
        and isinstance(break_glass_quorum_policy, dict)
        and break_glass_quorum_policy
        == {
            "scheme": custody_policy.get("scheme"),
            "totalShares": custody_policy.get("totalShares"),
            "threshold": custody_policy.get("threshold"),
            "minimumParticipatingCustodians": custody_policy.get(
                "minimumParticipatingCustodians"
            ),
        }
        and temporary_break_glass_policy.get("requiredAuditEvidence")
        == [
            "approved-change-record",
            "pre-open-403-check",
            "window-open-200-check",
            "three-custodian-share-submissions",
            "post-close-403-check",
            "generated-root-revoked",
        ]
        and isinstance(break_glass_post_close_policy, dict)
        and break_glass_post_close_policy.get("expectedAttemptStatusCode") == 403
        and break_glass_post_close_policy.get("revokeGeneratedRootImmediately") is True
    )
    config_data_valid = (
        audit_observability_name_match is not None
        and audit_observability_name_match.group(1) == VAULT_AUDIT_OBSERVABILITY_CONFIGMAP_NAME
        and set(audit_observability_data) == {
            VAULT_AUDIT_VECTOR_CONFIG_KEY,
            VAULT_AUDIT_SHIPPER_SCRIPT_KEY,
            VAULT_AUDIT_ROTATION_SCRIPT_KEY,
        }
        and isinstance(vector_config, str)
        and isinstance(shipper_script, str)
        and isinstance(rotation_script, str)
        and all(fragment in vector_config for fragment in required_vector_fragments)
        and VAULT_AUDIT_DEVICE_SECONDARY_FILE_PATH not in vector_config
        and f"uri: ${{{VAULT_AUDIT_SIEM_ENVIRONMENT[0][0]}}}" in vector_config
        and f"ca_file: {VAULT_AUDIT_SIEM_TLS_MOUNT_PATH}/ca.crt" in vector_config
        and f"crt_file: {VAULT_AUDIT_SIEM_TLS_MOUNT_PATH}/client.crt" in vector_config
        and f"key_file: {VAULT_AUDIT_SIEM_TLS_MOUNT_PATH}/client.key" in vector_config
        and 'exec /usr/bin/vector --config /etc/vault-audit/vector.yaml' in shipper_script
        and (
            ': "${'
            + VAULT_AUDIT_SIEM_ENVIRONMENT[0][0]
            + ':?missing '
            + VAULT_AUDIT_SIEM_ENVIRONMENT[0][0]
            + '}"'
        )
        in shipper_script
        and (
            ': "${'
            + VAULT_AUDIT_SIEM_ENVIRONMENT[1][0]
            + ':?missing '
            + VAULT_AUDIT_SIEM_ENVIRONMENT[1][0]
            + '}"'
        )
        in shipper_script
        and (
            ': "${'
            + VAULT_AUDIT_SIEM_ENVIRONMENT[2][0]
            + ':?missing '
            + VAULT_AUDIT_SIEM_ENVIRONMENT[2][0]
            + '}"'
        )
        in shipper_script
        and (
            ': "${'
            + VAULT_AUDIT_SIEM_ENVIRONMENT[3][0]
            + ':?missing '
            + VAULT_AUDIT_SIEM_ENVIRONMENT[3][0]
            + '}"'
        )
        in shipper_script
        and "mkdir -p /vault/audit/vector-data" in shipper_script
        and "chmod 0700 /vault/audit/vector-data" in shipper_script
        and all(fragment in rotation_script for fragment in required_rotation_fragments)
        and all(fragment not in rotation_script for fragment in forbidden_rotation_fragments)
    )
    if (
        secret_match is None
        or pdb_name_match is None
        or pdb_match is None
        or not isinstance(image_repository, str)
        or not image_repository
        or not isinstance(image_tag, str)
        or re.fullmatch(r"[A-Za-z0-9._-]{1,128}@sha256:[0-9a-f]{64}", image_tag) is None
        or not values_scalars_valid
        or expected_peer_ingress not in values_text
        or expected_client_ingress not in values_text
        or expected_dns_egress not in values_text
        or expected_kubernetes_service_egress not in values_text
        or expected_apiserver_egress not in values_text
        or expected_siem_egress not in values_text
        or not all(fragment in values_text for fragment in required_sidecar_fragments)
        or not all(fragment in bootstrap_text for fragment in required_bootstrap_fragments)
        or bootstrap_text.count("token_type=batch") != 3
        or bootstrap_text.count("token_no_default_policy=true") != 3
        or bootstrap_text.count("bind_secret_id=true") != 3
        or not all(
            fragment in helm_post_renderer_text
            for fragment in (
                "refusing a rendered Vault readiness probe that disables TLS verification",
                "VAULT_ADDR",
                "VAULT_CACERT",
                'print indentation "      - \\\"vault status >/dev/null\\\""',
                "expected at most one Vault readiness probe per rendered manifest file",
            )
        )
        or not all(
            fragment in helm_post_renderer_plugin_text
            for fragment in (
                "name: synara-vault-tls-readiness",
                "type: postrenderer/v1",
                "runtime: subprocess",
                '${HELM_PLUGIN_DIR}/../../helm-post-renderer.sh',
            )
        )
        or not config_data_valid
        or not operations_policy_valid
    ):
        raise ReleaseGateError(
            "release.vault_kms_source_invalid",
            "The checked-in Vault production baseline was malformed.",
        )
    return {
        "imageReference": f"{image_repository}:{image_tag}",
        "tlsSecretName": secret_match.group(1),
        "pdbName": pdb_name_match.group(1),
        "pdbMinAvailable": int(pdb_match.group(1)),
        "shareProcessNamespace": True,
        "livenessProbe": expected_liveness_probe,
        "readinessProbe": _expected_vault_readiness_probe(),
        "pdbSelector": dict(VAULT_SERVER_LABELS),
        "networkPolicyName": VAULT_NETWORK_POLICY_NAME,
        "networkPolicyPodSelector": dict(VAULT_RELEASE_LABELS),
        "configMapName": VAULT_CONFIGMAP_NAME,
        "auditObservabilityConfigMapName": VAULT_AUDIT_OBSERVABILITY_CONFIGMAP_NAME,
        "auditObservabilityConfigData": {
            key: sha256_text(value)
            for key, value in sorted(audit_observability_data.items())
        },
        "auditShipper": {
            "name": VAULT_AUDIT_SHIPPER_NAME,
            "imageReference": VAULT_AUDIT_SHIPPER_IMAGE_REFERENCE,
            "command": ["/bin/sh", f"{VAULT_AUDIT_CONFIG_MOUNT_PATH}/{VAULT_AUDIT_SHIPPER_SCRIPT_KEY}"],
            "securityContext": _expected_vault_audit_sidecar_security_context(),
            "envSecretRefs": _expected_vault_audit_siem_secret_refs(),
            "volumeMounts": [
                {"name": "audit", "mountPath": VAULT_AUDIT_MOUNT_PATH},
                {
                    "name": VAULT_AUDIT_OBSERVABILITY_VOLUME_NAME,
                    "mountPath": VAULT_AUDIT_CONFIG_MOUNT_PATH,
                    "readOnly": True,
                },
                {
                    "name": VAULT_AUDIT_SIEM_TLS_VOLUME_NAME,
                    "mountPath": VAULT_AUDIT_SIEM_TLS_MOUNT_PATH,
                },
                {"name": "tmp", "mountPath": "/tmp"},
            ],
        },
        "auditRotation": {
            "name": VAULT_AUDIT_ROTATION_NAME,
            "imageReference": VAULT_AUDIT_ROTATION_IMAGE_REFERENCE,
            "command": ["/bin/sh", f"{VAULT_AUDIT_CONFIG_MOUNT_PATH}/{VAULT_AUDIT_ROTATION_SCRIPT_KEY}"],
            "securityContext": _expected_vault_audit_sidecar_security_context(),
            "volumeMounts": [
                {"name": "audit", "mountPath": VAULT_AUDIT_MOUNT_PATH},
                {
                    "name": VAULT_AUDIT_OBSERVABILITY_VOLUME_NAME,
                    "mountPath": VAULT_AUDIT_CONFIG_MOUNT_PATH,
                    "readOnly": True,
                },
                {"name": "tmp", "mountPath": "/tmp"},
            ],
        },
        "auditSiemSecretName": VAULT_AUDIT_SIEM_SECRET_NAME,
        "auditSiemEgress": {
            "cidr": VAULT_AUDIT_SIEM_EGRESS_CIDR,
            "port": VAULT_AUDIT_SIEM_EGRESS_PORT,
            "protocol": "TCP",
        },
        "enableUnauthenticatedAccess": enable_unauthenticated_access,
        "retryJoin": retry_join,
        "listenerFragments": (
            "ui = false",
            "enable_unauthenticated_access = []",
            'listener "tcp" {',
            "tls_disable = 0",
            'address = "[::]:8200"',
            'cluster_address = "[::]:8201"',
            'tls_cert_file = "/vault/tls/tls.crt"',
            'tls_key_file = "/vault/tls/tls.key"',
            'tls_client_ca_file = "/vault/tls/ca.crt"',
            'storage "raft" {',
            'path = "/vault/data"',
        ),
    }


def _normalized_network_policy_rules(value: Any, *, peer_key: str) -> list[dict[str, Any]]:
    if not isinstance(value, list):
        raise ReleaseGateError(
            "release.vault_kms_network_policy_invalid",
            "The live Vault NetworkPolicy rules were malformed.",
            {"direction": "ingress" if peer_key == "from" else "egress"},
        )
    normalized: list[dict[str, Any]] = []
    for rule in value:
        if not isinstance(rule, dict) or set(rule) - {peer_key, "ports"}:
            raise ReleaseGateError(
                "release.vault_kms_network_policy_invalid",
                "The live Vault NetworkPolicy contained an unexpected rule shape.",
                {"direction": "ingress" if peer_key == "from" else "egress"},
            )
        peers = rule.get(peer_key, [])
        ports = rule.get("ports")
        if not isinstance(peers, list) or not isinstance(ports, list):
            raise ReleaseGateError(
                "release.vault_kms_network_policy_invalid",
                "The live Vault NetworkPolicy rule omitted peers or ports.",
            )
        normalized_peers: list[dict[str, Any]] = []
        for peer in peers:
            if not isinstance(peer, dict) or set(peer) - {"podSelector", "namespaceSelector", "ipBlock"}:
                raise ReleaseGateError(
                    "release.vault_kms_network_policy_invalid",
                    "The live Vault NetworkPolicy used an unexpected peer selector.",
                )
            normalized_peer: dict[str, Any] = {}
            for selector_name in ("podSelector", "namespaceSelector"):
                if selector_name not in peer:
                    continue
                selector = peer.get(selector_name)
                if (
                    not isinstance(selector, dict)
                    or set(selector) - {"matchLabels"}
                    or not isinstance(selector.get("matchLabels"), dict)
                    or not all(
                        isinstance(key, str) and isinstance(item, str)
                        for key, item in selector["matchLabels"].items()
                    )
                ):
                    raise ReleaseGateError(
                        "release.vault_kms_network_policy_invalid",
                        "The live Vault NetworkPolicy selector was malformed or used matchExpressions.",
                    )
                normalized_peer[selector_name] = {
                    "matchLabels": dict(sorted(selector["matchLabels"].items()))
                }
            if "ipBlock" in peer:
                ip_block = peer.get("ipBlock")
                if (
                    not isinstance(ip_block, dict)
                    or set(ip_block) - {"cidr", "except"}
                    or not isinstance(ip_block.get("cidr"), str)
                    or not ip_block.get("cidr")
                    or (
                        "except" in ip_block
                        and (
                            not isinstance(ip_block.get("except"), list)
                            or not all(isinstance(item, str) and item for item in ip_block["except"])
                        )
                    )
                ):
                    raise ReleaseGateError(
                        "release.vault_kms_network_policy_invalid",
                        "The live Vault NetworkPolicy ipBlock selector was malformed.",
                    )
                normalized_peer["ipBlock"] = {"cidr": ip_block["cidr"]}
                if "except" in ip_block:
                    normalized_peer["ipBlock"]["except"] = sorted(ip_block["except"])
            if not normalized_peer and peers:
                raise ReleaseGateError(
                    "release.vault_kms_network_policy_invalid",
                    "The live Vault NetworkPolicy contained an unrestricted peer.",
                )
            normalized_peers.append(normalized_peer)
        normalized_ports: list[dict[str, Any]] = []
        for port in ports:
            if (
                not isinstance(port, dict)
                or set(port) - {"port", "protocol"}
                or isinstance(port.get("port"), bool)
                or not isinstance(port.get("port"), (int, str))
                or port.get("protocol", "TCP") not in {"TCP", "UDP"}
            ):
                raise ReleaseGateError(
                    "release.vault_kms_network_policy_invalid",
                    "The live Vault NetworkPolicy port declaration was malformed.",
                )
            normalized_ports.append(
                {"port": port["port"], "protocol": port.get("protocol", "TCP")}
            )
        normalized.append(
            {
                peer_key: sorted(normalized_peers, key=_stringify_json),
                "ports": sorted(normalized_ports, key=_stringify_json),
            }
        )
    return sorted(normalized, key=_stringify_json)


def _expected_vault_network_policy_rules() -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
    ingress = [
        {
            "from": [{"podSelector": {"matchLabels": dict(VAULT_SERVER_LABELS)}}],
            "ports": [
                {"port": 8200, "protocol": "TCP"},
                {"port": 8201, "protocol": "TCP"},
            ],
        },
        {
            "from": [
                {
                    "namespaceSelector": {
                        "matchLabels": {"kubernetes.io/metadata.name": "synara-system"}
                    }
                }
            ],
            "ports": [{"port": 8200, "protocol": "TCP"}],
        },
    ]
    egress = [
        {
            "to": [{"podSelector": {"matchLabels": dict(VAULT_SERVER_LABELS)}}],
            "ports": [
                {"port": 8200, "protocol": "TCP"},
                {"port": 8201, "protocol": "TCP"},
            ],
        },
        {
            "to": [
                {
                    "namespaceSelector": {"matchLabels": dict(VAULT_DNS_NAMESPACE_LABELS)},
                    "podSelector": {"matchLabels": dict(VAULT_DNS_POD_LABELS)},
                }
            ],
            "ports": [
                {"port": 53, "protocol": "UDP"},
                {"port": 53, "protocol": "TCP"},
            ],
        },
        {
            "to": [{"ipBlock": {"cidr": VAULT_KUBERNETES_SERVICE_IP_CIDR}}],
            "ports": [{"port": 443, "protocol": "TCP"}],
        },
        {
            "to": [{"ipBlock": {"cidr": VAULT_KUBERNETES_APISERVER_CIDR}}],
            "ports": [{"port": 6443, "protocol": "TCP"}],
        },
        {
            "to": [{"ipBlock": {"cidr": VAULT_AUDIT_SIEM_EGRESS_CIDR}}],
            "ports": [{"port": VAULT_AUDIT_SIEM_EGRESS_PORT, "protocol": "TCP"}],
        },
    ]
    return (
        _normalized_network_policy_rules(ingress, peer_key="from"),
        _normalized_network_policy_rules(egress, peer_key="to"),
    )


def _expected_vault_audit_devices() -> list[dict[str, str]]:
    return [
        {
            "path": VAULT_AUDIT_DEVICE_PRIMARY_NAME,
            "type": "file",
            "filePath": VAULT_AUDIT_DEVICE_PRIMARY_FILE_PATH,
        },
        {
            "path": VAULT_AUDIT_DEVICE_SECONDARY_NAME,
            "type": "file",
            "filePath": VAULT_AUDIT_DEVICE_SECONDARY_FILE_PATH,
        },
    ]


def _verify_approle_configuration(
    payload: Any,
    *,
    role_name: str,
    expected_policies: tuple[str, ...],
    constraints: Mapping[str, int],
) -> dict[str, Any]:
    data = _json_object(
        payload.get("data") if isinstance(payload, dict) else None,
        code="release.vault_kms_approle_invalid",
        message="Vault AppRole output was malformed.",
    )
    raw_policies = data.get("token_policies", data.get("policies"))
    actual_policies = (
        tuple(sorted(set(raw_policies)))
        if isinstance(raw_policies, list) and all(isinstance(item, str) for item in raw_policies)
        else ()
    )
    expected = tuple(sorted(expected_policies))
    mismatches = {
        key: data.get(key)
        for key, value in constraints.items()
        if data.get(key) != value
    }
    if (
        data.get("token_type") != "batch"
        or data.get("token_no_default_policy") is not True
        or data.get("bind_secret_id") is not True
        or actual_policies != expected
        or "root" in actual_policies
        or "default" in actual_policies
        or mismatches
    ):
        raise ReleaseGateError(
            "release.vault_kms_approle_invalid",
            "Vault AppRole did not preserve the exact batch-token identity boundary.",
            {
                "roleName": role_name,
                "expectedPolicyHash": sha256_text("\n".join(expected)),
                "actualPolicyHash": sha256_text("\n".join(actual_policies)),
                "mismatchedConstraintNames": sorted(mismatches),
            },
        )
    identity = {
        "roleName": role_name,
        "tokenType": "batch",
        "noDefaultPolicy": True,
        "bindSecretId": True,
        "policyNames": list(expected),
        "constraints": dict(sorted(constraints.items())),
    }
    return {**identity, "configurationSha256": sha256_text(_stringify_json(identity))}


def _verify_vault_token_identity(
    payload: Any,
    *,
    role_name: str,
    expected_policies: tuple[str, ...],
    token_environment: str,
    maximum_creation_ttl: int,
) -> dict[str, Any]:
    data = _json_object(
        payload.get("data") if isinstance(payload, dict) else None,
        code="release.vault_kms_identity_invalid",
        message="Vault token lookup-self output was malformed.",
    )
    raw_policies = data.get("policies")
    actual_policies = (
        tuple(sorted(set(raw_policies)))
        if isinstance(raw_policies, list) and all(isinstance(item, str) for item in raw_policies)
        else ()
    )
    metadata = data.get("meta")
    creation_ttl = data.get("creation_ttl")
    ttl = data.get("ttl")
    if (
        data.get("display_name") != supply_chain.VAULT_TRANSIT_AUTH_METHOD
        or data.get("type") != "batch"
        or data.get("orphan") is not True
        or data.get("path") != "auth/approle/login"
        or not isinstance(metadata, dict)
        or metadata.get("role_name") != role_name
        or actual_policies != tuple(sorted(expected_policies))
        or "root" in actual_policies
        or "default" in actual_policies
        or data.get("num_uses") != 0
        or isinstance(creation_ttl, bool)
        or not isinstance(creation_ttl, int)
        or not 0 < creation_ttl <= maximum_creation_ttl
        or isinstance(ttl, bool)
        or not isinstance(ttl, int)
        or not 0 < ttl <= creation_ttl
    ):
        raise ReleaseGateError(
            "release.vault_kms_identity_invalid",
            "Vault lookup-self did not prove the expected AppRole batch-token identity.",
            {
                "tokenEnvironment": token_environment,
                "expectedRoleName": role_name,
                "expectedPolicyHash": sha256_text("\n".join(sorted(expected_policies))),
                "actualPolicyHash": sha256_text("\n".join(actual_policies)),
            },
        )
    policies_sha256 = sha256_text(
        json.dumps(list(actual_policies), separators=(",", ":"), sort_keys=True)
    )
    shared_identity = {
        "verified": True,
        "displayName": supply_chain.VAULT_TRANSIT_AUTH_METHOD,
        "roleName": role_name,
        "type": "batch",
        "orphan": True,
        "policyCount": len(actual_policies),
        "policiesSha256": policies_sha256,
    }
    identity = {
        "tokenEnvironment": token_environment,
        "principal": f"auth/approle/role/{role_name}",
        "roleName": role_name,
        "tokenType": "batch",
        "orphan": True,
        "policyNames": list(actual_policies),
        "policyHash": policies_sha256,
        "registryIdentitySha256": sha256_text(_stringify_json(shared_identity)),
    }
    return {**identity, "identitySha256": sha256_text(_stringify_json(identity))}


def _cleanup_resource_document(document: Mapping[str, Any]) -> dict[str, Any]:
    payload = copy.deepcopy(dict(document))
    payload.pop("status", None)
    metadata = payload.get("metadata")
    if isinstance(metadata, dict):
        for key in ("creationTimestamp", "generation", "managedFields", "resourceVersion", "selfLink", "uid"):
            metadata.pop(key, None)
    return payload


def _deployment_template_spec(
    deployment: Mapping[str, Any],
    *,
    code: str,
    message: str,
) -> tuple[dict[str, Any], dict[str, Any], list[dict[str, Any]], list[dict[str, Any]]]:
    spec = deployment.get("spec")
    if not isinstance(spec, dict):
        raise ReleaseGateError(code, message)
    template = spec.get("template")
    if not isinstance(template, dict):
        raise ReleaseGateError(code, message)
    template_metadata = template.get("metadata")
    if not isinstance(template_metadata, dict):
        template_metadata = {}
        template["metadata"] = template_metadata
    template_spec = template.get("spec")
    if not isinstance(template_spec, dict):
        raise ReleaseGateError(code, message)
    containers = template_spec.get("containers")
    if not isinstance(containers, list) or not containers or not all(isinstance(item, dict) for item in containers):
        raise ReleaseGateError(code, message)
    volumes = template_spec.get("volumes")
    if volumes is None:
        volumes = []
        template_spec["volumes"] = volumes
    if not isinstance(volumes, list) or not all(isinstance(item, dict) for item in volumes):
        raise ReleaseGateError(code, message)
    return spec, template_metadata, containers, volumes


def _configmap_data_key_from_mount(volume: Mapping[str, Any], mount: Mapping[str, Any]) -> str:
    sub_path = mount.get("subPath")
    config_map = volume.get("configMap")
    if not isinstance(config_map, dict):
        raise ReleaseGateError(
            "release.vault_kms_admission_apply_failed",
            "The existing Kyverno controller CA volume did not use a ConfigMap source.",
        )
    if isinstance(sub_path, str) and sub_path:
        items = config_map.get("items")
        if isinstance(items, list):
            for item in items:
                if isinstance(item, dict) and item.get("path") == sub_path and isinstance(item.get("key"), str):
                    return item["key"]
        return sub_path
    items = config_map.get("items")
    if isinstance(items, list) and len(items) == 1 and isinstance(items[0], dict) and isinstance(items[0].get("key"), str):
        return items[0]["key"]
    raise ReleaseGateError(
        "release.vault_kms_admission_apply_failed",
        "The existing Kyverno controller CA ConfigMap did not expose a stable key mapping.",
    )


def _merge_pem_bundle(existing_bundle: str | None, registry_bundle: str) -> str:
    normalized_registry = registry_bundle.strip()
    if not normalized_registry:
        raise ReleaseGateError(
            "release.vault_kms_environment_invalid",
            "The configured Registry CA certificate was empty.",
        )
    parts: list[str] = []
    normalized_existing = ""
    if existing_bundle:
        normalized_existing = existing_bundle.strip()
        if normalized_existing:
            parts.append(normalized_existing)
    if normalized_registry and normalized_registry not in normalized_existing:
        parts.append(normalized_registry)
    return "\n".join(parts).rstrip() + "\n"


def verify_vault_cluster(
    options: GateOptions,
    secret_inputs: SecretInputs,
    *,
    redactor: acceptance.SecretRedactor,
) -> dict[str, Any]:
    baseline = _load_vault_baseline(options.repo_root)
    expected_policy_text = _normalized_vault_policy_text(
        _read_text_file(
            options.repo_root / VAULT_SIGNER_POLICY_PATH,
            code="release.vault_kms_source_invalid",
            message="The checked-in Vault signer policy was unavailable.",
        )
    )
    expected_operator_policy_text = _normalized_vault_policy_text(
        _read_text_file(
            options.repo_root / VAULT_OPERATOR_POLICY_PATH,
            code="release.vault_kms_source_invalid",
            message="The checked-in Vault operator policy was unavailable.",
        )
    )
    pods_payload = kubectl_json(
        options,
        [
            "-n",
            options.vault_namespace,
            "get",
            "pods",
            "-l",
            options.vault_selector,
            "-o",
            "json",
        ],
        redactor=redactor,
        timeout=30.0,
    )
    items = pods_payload.get("items")
    if not isinstance(items, list) or not all(isinstance(item, dict) for item in items):
        raise ReleaseGateError(
            "release.vault_kms_kubernetes_invalid",
            "The Kubernetes Vault Pod listing was malformed.",
        )
    pvc_payload = kubectl_json(
        options,
        [
            "-n",
            options.vault_namespace,
            "get",
            "pvc",
            "-o",
            "json",
        ],
        redactor=redactor,
        timeout=30.0,
    )
    statefulsets_payload = kubectl_json(
        options,
        [
            "-n",
            options.vault_namespace,
            "get",
            "statefulset",
            "-l",
            options.vault_selector,
            "-o",
            "json",
        ],
        redactor=redactor,
        timeout=30.0,
    )
    pdb_payload = kubectl_json(
        options,
        [
            "-n",
            options.vault_namespace,
            "get",
            "pdb",
            "-o",
            "json",
        ],
        redactor=redactor,
        timeout=30.0,
    )
    tls_secret_payload = kubectl_json(
        options,
        [
            "-n",
            options.vault_namespace,
            "get",
            "secret",
            str(baseline["tlsSecretName"]),
            "-o",
            "json",
        ],
        redactor=redactor,
        timeout=30.0,
    )
    network_policy_payload = kubectl_json(
        options,
        [
            "-n",
            options.vault_namespace,
            "get",
            "networkpolicy",
            str(baseline["networkPolicyName"]),
            "-o",
            "json",
        ],
        redactor=redactor,
        timeout=30.0,
    )
    audit_observability_configmap_payload = kubectl_json(
        options,
        [
            "-n",
            options.vault_namespace,
            "get",
            "configmap",
            str(baseline["auditObservabilityConfigMapName"]),
            "-o",
            "json",
        ],
        redactor=redactor,
        timeout=30.0,
    )
    configmap_payload = kubectl_json(
        options,
        [
            "-n",
            options.vault_namespace,
            "get",
            "configmap",
            str(baseline["configMapName"]),
            "-o",
            "json",
        ],
        redactor=redactor,
        timeout=30.0,
    )
    ready_pods = 0
    running_pods = 0
    pod_names: list[str] = []
    node_names: set[str] = set()
    for item in items:
        metadata = item.get("metadata")
        spec = item.get("spec")
        status = item.get("status")
        metadata_obj = metadata if isinstance(metadata, dict) else {}
        spec_obj = spec if isinstance(spec, dict) else {}
        status_obj = status if isinstance(status, dict) else {}
        pod_name = metadata_obj.get("name")
        if isinstance(pod_name, str):
            pod_names.append(pod_name)
        node_name = spec_obj.get("nodeName")
        if isinstance(node_name, str) and node_name:
            node_names.add(node_name)
        if status_obj.get("phase") == "Running":
            running_pods += 1
        conditions = status_obj.get("conditions")
        if isinstance(conditions, list) and any(
            isinstance(condition, dict)
            and condition.get("type") == "Ready"
            and condition.get("status") == "True"
            for condition in conditions
        ):
            ready_pods += 1
    if len(items) < REQUIRED_VAULT_PEERS or ready_pods < REQUIRED_VAULT_PEERS or running_pods < REQUIRED_VAULT_PEERS:
        raise ReleaseGateError(
            "release.vault_kms_cluster_not_ready",
            "The target Kubernetes namespace did not expose three ready Vault Pods.",
            {
                "runningPods": running_pods,
                "readyPods": ready_pods,
                "podCount": len(items),
            },
        )
    if len(node_names) < REQUIRED_VAULT_PEERS:
        raise ReleaseGateError(
            "release.vault_kms_cluster_not_ready",
            "The target Kubernetes namespace did not spread the Vault Pods across three distinct nodes.",
            {
                "distinctNodeCount": len(node_names),
                "nodeNames": sorted(node_names),
            },
        )
    pvc_items = pvc_payload.get("items")
    if not isinstance(pvc_items, list) or not all(isinstance(item, dict) for item in pvc_items):
        raise ReleaseGateError(
            "release.vault_kms_kubernetes_invalid",
            "The Kubernetes Vault PVC listing was malformed.",
        )
    statefulset_items = statefulsets_payload.get("items")
    if (
        not isinstance(statefulset_items, list)
        or len(statefulset_items) != 1
        or not isinstance(statefulset_items[0], dict)
    ):
        raise ReleaseGateError(
            "release.vault_kms_kubernetes_invalid",
            "The Kubernetes Vault StatefulSet listing did not resolve to exactly one server StatefulSet.",
            {"statefulSetCount": len(statefulset_items) if isinstance(statefulset_items, list) else None},
        )
    statefulset = statefulset_items[0]
    statefulset_metadata = _json_object(
        statefulset.get("metadata"),
        code="release.vault_kms_kubernetes_invalid",
        message="The Kubernetes Vault StatefulSet metadata was malformed.",
    )
    statefulset_name = statefulset_metadata.get("name")
    if not isinstance(statefulset_name, str) or not statefulset_name:
        raise ReleaseGateError(
            "release.vault_kms_kubernetes_invalid",
            "The Kubernetes Vault StatefulSet omitted its name.",
        )
    statefulset_spec = _json_object(
        statefulset.get("spec"),
        code="release.vault_kms_kubernetes_invalid",
        message="The Kubernetes Vault StatefulSet spec was malformed.",
    )
    if statefulset_spec.get("replicas") != REQUIRED_VAULT_PEERS:
        raise ReleaseGateError(
            "release.vault_kms_cluster_not_ready",
            "The live Vault StatefulSet did not retain three replicas.",
            {"replicas": statefulset_spec.get("replicas")},
        )
    _spec, _template_metadata, containers, volumes = _deployment_template_spec(
        {"spec": {"template": statefulset_spec.get("template"), "volumes": statefulset_spec.get("template", {}).get("spec", {}).get("volumes")}},
        code="release.vault_kms_kubernetes_invalid",
        message="The Kubernetes Vault StatefulSet template was malformed.",
    )
    main_container = next(
        (
            container
            for container in containers
            if isinstance(container.get("name"), str) and container.get("name") == VAULT_MAIN_CONTAINER_NAME
        ),
        containers[0],
    )
    live_image = main_container.get("image") if isinstance(main_container.get("image"), str) else None
    if live_image != baseline["imageReference"]:
        raise ReleaseGateError(
            "release.vault_kms_kubernetes_invalid",
            "The live Vault StatefulSet image did not match the checked-in exact production tag+digest pin.",
            {
                "statefulSet": statefulset_name,
                "expectedImage": baseline["imageReference"],
                "actualImage": live_image,
            },
        )
    template_spec = statefulset_spec.get("template", {}).get("spec")
    expected_liveness_probe = {
        "exec": {"command": baseline["livenessProbe"]["execCommand"]},
        "failureThreshold": baseline["livenessProbe"]["failureThreshold"],
        "initialDelaySeconds": baseline["livenessProbe"]["initialDelaySeconds"],
        "periodSeconds": baseline["livenessProbe"]["periodSeconds"],
        "successThreshold": baseline["livenessProbe"]["successThreshold"],
        "timeoutSeconds": baseline["livenessProbe"]["timeoutSeconds"],
    }
    expected_readiness_probe = {
        "exec": {"command": baseline["readinessProbe"]["execCommand"]},
        "failureThreshold": baseline["readinessProbe"]["failureThreshold"],
        "initialDelaySeconds": baseline["readinessProbe"]["initialDelaySeconds"],
        "periodSeconds": baseline["readinessProbe"]["periodSeconds"],
        "successThreshold": baseline["readinessProbe"]["successThreshold"],
        "timeoutSeconds": baseline["readinessProbe"]["timeoutSeconds"],
    }
    if (
        not isinstance(template_spec, dict)
        or template_spec.get("terminationGracePeriodSeconds") != 30
        or template_spec.get("shareProcessNamespace") is not baseline["shareProcessNamespace"]
        or statefulset_spec.get("updateStrategy") != {"type": "OnDelete"}
        or main_container.get("livenessProbe") != expected_liveness_probe
        or main_container.get("readinessProbe") != expected_readiness_probe
    ):
        raise ReleaseGateError(
            "release.vault_kms_kubernetes_invalid",
            "The live Vault StatefulSet did not preserve the seal-aware lifecycle boundary.",
            {"statefulSet": statefulset_name},
        )
    audit_volume_mounts = [
        mount
        for mount in (
            main_container.get("volumeMounts") if isinstance(main_container.get("volumeMounts"), list) else []
        )
        if isinstance(mount, dict) and mount.get("mountPath") == VAULT_AUDIT_MOUNT_PATH
    ]
    audit_volume_names = {
        str(mount.get("name"))
        for mount in audit_volume_mounts
        if isinstance(mount.get("name"), str) and mount.get("name")
    }
    if "audit" not in audit_volume_names:
        raise ReleaseGateError(
            "release.vault_kms_kubernetes_invalid",
            "The live Vault StatefulSet did not mount its audit PVC at /vault/audit.",
            {
                "statefulSet": statefulset_name,
                "auditVolumeNames": sorted(audit_volume_names),
            },
        )
    configmap = _json_object(
        configmap_payload,
        code="release.vault_kms_kubernetes_invalid",
        message="The live Vault server ConfigMap output was malformed.",
    )
    configmap_metadata = _json_object(
        configmap.get("metadata"),
        code="release.vault_kms_kubernetes_invalid",
        message="The live Vault server ConfigMap metadata was malformed.",
    )
    configmap_data = configmap.get("data")
    live_server_config = (
        configmap_data.get("extraconfig-from-values.hcl")
        if isinstance(configmap_data, dict)
        else None
    )
    live_retry_join = _normalized_retry_join_configuration(
        live_server_config,
        code="release.vault_kms_kubernetes_invalid",
    )
    if (
        configmap_metadata.get("name") != baseline["configMapName"]
        or not isinstance(live_server_config, str)
        or not all(fragment in live_server_config for fragment in baseline["listenerFragments"])
        or _normalized_unauthenticated_access_configuration(
            live_server_config,
            code="release.vault_kms_kubernetes_invalid",
            message="The live Vault ConfigMap did not expose a single top-level unauthenticated-access policy.",
        )
        != baseline["enableUnauthenticatedAccess"]
        or live_retry_join != baseline["retryJoin"]
    ):
        raise ReleaseGateError(
            "release.vault_kms_kubernetes_invalid",
            "The live Vault ConfigMap did not preserve the checked-in UI-disabled TLS Raft baseline.",
            {"configMap": baseline["configMapName"]},
        )
    audit_observability_configmap = _json_object(
        audit_observability_configmap_payload,
        code="release.vault_kms_kubernetes_invalid",
        message="The live Vault audit observability ConfigMap output was malformed.",
    )
    audit_observability_metadata = _json_object(
        audit_observability_configmap.get("metadata"),
        code="release.vault_kms_kubernetes_invalid",
        message="The live Vault audit observability ConfigMap metadata was malformed.",
    )
    audit_observability_data = audit_observability_configmap.get("data")
    actual_audit_observability_hashes = (
        {
            key: sha256_text(value)
            for key, value in sorted(audit_observability_data.items())
            if isinstance(key, str) and isinstance(value, str)
        }
        if isinstance(audit_observability_data, dict)
        else None
    )
    if (
        audit_observability_metadata.get("name") != baseline["auditObservabilityConfigMapName"]
        or actual_audit_observability_hashes != baseline["auditObservabilityConfigData"]
    ):
        raise ReleaseGateError(
            "release.vault_kms_kubernetes_invalid",
            "The live Vault audit observability ConfigMap drifted from the checked-in config or script hashes.",
            {"configMap": baseline["auditObservabilityConfigMapName"]},
        )
    vault_cacert_env = [
        item
        for item in (main_container.get("env") if isinstance(main_container.get("env"), list) else [])
        if isinstance(item, dict) and item.get("name") == "VAULT_CACERT"
    ]
    if vault_cacert_env != [{"name": "VAULT_CACERT", "value": "/vault/tls/ca.crt"}]:
        raise ReleaseGateError(
            "release.vault_kms_kubernetes_invalid",
            "The live Vault container did not expose the mounted CA certificate to operator CLI commands.",
            {"statefulSet": statefulset_name, "environment": "VAULT_CACERT"},
        )
    tls_volume_present = any(
        isinstance(volume.get("secret"), dict) and volume["secret"].get("secretName") == baseline["tlsSecretName"]
        for volume in volumes
    )
    tls_mount_present = any(
        any(
            isinstance(mount, dict)
            and mount.get("mountPath") == "/vault/tls"
            and mount.get("readOnly") is True
            for mount in (container.get("volumeMounts") if isinstance(container.get("volumeMounts"), list) else [])
        )
        for container in containers
    )
    if not tls_volume_present or not tls_mount_present:
        raise ReleaseGateError(
            "release.vault_kms_kubernetes_invalid",
            "The live Vault StatefulSet did not mount the checked-in TLS Secret at /vault/tls.",
            {"statefulSet": statefulset_name, "tlsSecretName": baseline["tlsSecretName"]},
        )
    audit_observability_volume = next(
        (
            volume
            for volume in volumes
            if isinstance(volume.get("name"), str)
            and volume.get("name") == VAULT_AUDIT_OBSERVABILITY_VOLUME_NAME
        ),
        None,
    )
    audit_siem_tls_volume = next(
        (
            volume
            for volume in volumes
            if isinstance(volume.get("name"), str)
            and volume.get("name") == VAULT_AUDIT_SIEM_TLS_VOLUME_NAME
        ),
        None,
    )
    if (
        not isinstance(audit_observability_volume, dict)
        or not isinstance(audit_observability_volume.get("configMap"), dict)
        or audit_observability_volume["configMap"].get("name")
        != baseline["auditObservabilityConfigMapName"]
        or not isinstance(audit_siem_tls_volume, dict)
        or not isinstance(audit_siem_tls_volume.get("emptyDir"), dict)
    ):
        raise ReleaseGateError(
            "release.vault_kms_kubernetes_invalid",
            "The live Vault StatefulSet did not preserve the checked-in audit observability support volumes.",
            {"statefulSet": statefulset_name},
        )
    expected_sidecars = {
        baseline["auditShipper"]["name"]: baseline["auditShipper"],
        baseline["auditRotation"]["name"]: baseline["auditRotation"],
    }
    actual_sidecars = {
        str(container.get("name")): container
        for container in containers
        if isinstance(container.get("name"), str) and container.get("name") in expected_sidecars
    }
    if set(actual_sidecars) != set(expected_sidecars):
        raise ReleaseGateError(
            "release.vault_kms_kubernetes_invalid",
            "The live Vault StatefulSet did not preserve the checked-in audit sidecar set.",
            {
                "expectedSidecars": sorted(expected_sidecars),
                "actualSidecars": sorted(actual_sidecars),
            },
        )
    for sidecar_name, expected_sidecar in expected_sidecars.items():
        actual_sidecar = actual_sidecars[sidecar_name]
        if (
            actual_sidecar.get("image") != expected_sidecar["imageReference"]
            or actual_sidecar.get("command") != expected_sidecar["command"]
            or actual_sidecar.get("securityContext") != expected_sidecar["securityContext"]
            or _normalized_volume_mount_contract(
                actual_sidecar.get("volumeMounts"),
                code="release.vault_kms_kubernetes_invalid",
                message="The live Vault audit sidecar volume mounts were malformed.",
            )
            != _normalized_volume_mount_contract(
                expected_sidecar["volumeMounts"],
                code="release.vault_kms_source_invalid",
                message="The checked-in Vault audit sidecar volume mounts were malformed.",
            )
        ):
            raise ReleaseGateError(
                "release.vault_kms_kubernetes_invalid",
                "The live Vault audit sidecars drifted from the checked-in image, command, mount, or security baseline.",
                {"statefulSet": statefulset_name, "sidecar": sidecar_name},
            )
    actual_shipper_env = _normalized_secret_env_contract(
        actual_sidecars[baseline["auditShipper"]["name"]].get("env"),
        code="release.vault_kms_kubernetes_invalid",
        message="The live Vault audit shipper Secret environment references were malformed.",
    )
    if actual_shipper_env != baseline["auditShipper"]["envSecretRefs"]:
        raise ReleaseGateError(
            "release.vault_kms_kubernetes_invalid",
            "The live Vault audit shipper did not preserve the checked-in Secret environment mapping.",
            {
                "sidecar": baseline["auditShipper"]["name"],
                "secretName": baseline["auditSiemSecretName"],
            },
        )
    claim_templates = statefulset_spec.get("volumeClaimTemplates")
    if not isinstance(claim_templates, list) or not all(isinstance(item, dict) for item in claim_templates):
        raise ReleaseGateError(
            "release.vault_kms_kubernetes_invalid",
            "The live Vault StatefulSet did not expose its volume claim templates.",
            {"statefulSet": statefulset_name},
        )
    claim_names = {
        claim.get("metadata", {}).get("name")
        for claim in claim_templates
        if isinstance(claim.get("metadata"), dict) and isinstance(claim.get("metadata", {}).get("name"), str)
    }
    if not {"data", "audit"}.issubset(claim_names):
        raise ReleaseGateError(
            "release.vault_kms_kubernetes_invalid",
            "The live Vault StatefulSet did not preserve both data and audit PVC claim templates.",
            {"claimTemplates": sorted(str(name) for name in claim_names)},
        )
    bound_pvc_names = [
        item.get("metadata", {}).get("name")
        for item in pvc_items
        if isinstance(item.get("metadata"), dict)
        and isinstance(item.get("metadata", {}).get("name"), str)
        and item.get("status", {}).get("phase") == "Bound"
    ]
    data_pvcs = sorted(
        name for name in bound_pvc_names if isinstance(name, str) and name.startswith(f"data-{statefulset_name}-")
    )
    audit_pvcs = sorted(
        name for name in bound_pvc_names if isinstance(name, str) and name.startswith(f"audit-{statefulset_name}-")
    )
    if len(data_pvcs) < REQUIRED_VAULT_PEERS or len(audit_pvcs) < REQUIRED_VAULT_PEERS:
        raise ReleaseGateError(
            "release.vault_kms_cluster_not_ready",
            "The live Vault cluster did not expose three bound data PVCs and three bound audit PVCs.",
            {
                "dataPvcCount": len(data_pvcs),
                "auditPvcCount": len(audit_pvcs),
                "statefulSet": statefulset_name,
            },
        )
    pdb_items = pdb_payload.get("items")
    if not isinstance(pdb_items, list) or not all(isinstance(item, dict) for item in pdb_items):
        raise ReleaseGateError(
            "release.vault_kms_kubernetes_invalid",
            "The Kubernetes Vault PodDisruptionBudget listing was malformed.",
        )
    matching_pdbs = [
        item
        for item in pdb_items
        if isinstance(item.get("metadata"), dict) and item["metadata"].get("name") == baseline["pdbName"]
    ]
    if len(matching_pdbs) != 1:
        raise ReleaseGateError(
            "release.vault_kms_kubernetes_invalid",
            "The live Vault namespace did not expose the checked-in PodDisruptionBudget.",
            {"matchingPdbCount": len(matching_pdbs)},
        )
    pdb_spec = _json_object(
        matching_pdbs[0].get("spec"),
        code="release.vault_kms_kubernetes_invalid",
        message="The live Vault PodDisruptionBudget spec was malformed.",
    )
    if pdb_spec.get("minAvailable") != baseline["pdbMinAvailable"]:
        raise ReleaseGateError(
            "release.vault_kms_kubernetes_invalid",
            "The live Vault PodDisruptionBudget did not preserve the checked-in minAvailable boundary.",
            {"minAvailable": pdb_spec.get("minAvailable")},
        )
    pdb_selector = pdb_spec.get("selector")
    if (
        not isinstance(pdb_selector, dict)
        or pdb_selector.get("matchLabels") != baseline["pdbSelector"]
        or set(pdb_selector) - {"matchLabels"}
    ):
        raise ReleaseGateError(
            "release.vault_kms_kubernetes_invalid",
            "The live Vault PodDisruptionBudget did not select the exact production server Pods.",
            {"pdbName": baseline["pdbName"]},
        )
    tls_secret = _json_object(
        tls_secret_payload,
        code="release.vault_kms_kubernetes_invalid",
        message="The live Vault TLS Secret output was malformed.",
    )
    tls_secret_type = tls_secret.get("type")
    tls_secret_data = tls_secret.get("data")
    if (
        tls_secret_type != "kubernetes.io/tls"
        or not isinstance(tls_secret_data, dict)
        or not {"tls.crt", "tls.key", "ca.crt"}.issubset(tls_secret_data)
    ):
        raise ReleaseGateError(
            "release.vault_kms_kubernetes_invalid",
            "The live Vault TLS Secret did not preserve the expected metadata-only key set.",
            {
                "secretName": baseline["tlsSecretName"],
                "type": tls_secret_type,
                "keys": sorted(tls_secret_data) if isinstance(tls_secret_data, dict) else None,
            },
        )
    network_policy = _json_object(
        network_policy_payload,
        code="release.vault_kms_network_policy_invalid",
        message="The live Vault NetworkPolicy output was malformed.",
    )
    network_policy_metadata = _json_object(
        network_policy.get("metadata"),
        code="release.vault_kms_network_policy_invalid",
        message="The live Vault NetworkPolicy metadata was malformed.",
    )
    network_policy_spec = _json_object(
        network_policy.get("spec"),
        code="release.vault_kms_network_policy_invalid",
        message="The live Vault NetworkPolicy spec was malformed.",
    )
    expected_ingress, expected_egress = _expected_vault_network_policy_rules()
    actual_ingress = _normalized_network_policy_rules(
        network_policy_spec.get("ingress"),
        peer_key="from",
    )
    actual_egress = _normalized_network_policy_rules(
        network_policy_spec.get("egress"),
        peer_key="to",
    )
    raw_policy_types = network_policy_spec.get("policyTypes")
    actual_policy_types = (
        set(raw_policy_types)
        if isinstance(raw_policy_types, list)
        and all(isinstance(item, str) for item in raw_policy_types)
        else set()
    )
    if (
        network_policy_metadata.get("name") != baseline["networkPolicyName"]
        or network_policy_spec.get("podSelector")
        != {"matchLabels": baseline["networkPolicyPodSelector"]}
        or actual_policy_types != {"Ingress", "Egress"}
        or actual_ingress != expected_ingress
        or actual_egress != expected_egress
    ):
        raise ReleaseGateError(
            "release.vault_kms_network_policy_invalid",
            "The live Vault NetworkPolicy did not preserve the exact peer and client traffic boundary.",
            {
                "networkPolicyName": network_policy_metadata.get("name"),
                "ingressRuleHash": sha256_text(_stringify_json(actual_ingress)),
                "egressRuleHash": sha256_text(_stringify_json(actual_egress)),
            },
        )
    status_payload = vault_json(
        options,
        secret_inputs,
        ["status", "-format=json"],
        vault_environment=secret_inputs.vault_operator_environment,
        redactor=redactor,
        timeout=30.0,
    )
    operator_identity_payload = vault_json(
        options,
        secret_inputs,
        ["token", "lookup", "-format=json"],
        vault_environment=secret_inputs.vault_operator_environment,
        redactor=redactor,
        timeout=30.0,
    )
    raft_payload = vault_json(
        options,
        secret_inputs,
        ["operator", "raft", "list-peers", "-format=json"],
        vault_environment=secret_inputs.vault_operator_environment,
        redactor=redactor,
        timeout=30.0,
    )
    key_payload = vault_json(
        options,
        secret_inputs,
        ["read", "-format=json", f"transit/keys/{EXPECTED_VAULT_KEY_NAME}"],
        vault_environment=secret_inputs.vault_operator_environment,
        redactor=redactor,
        timeout=30.0,
    )
    signer_approle_payload = vault_json(
        options,
        secret_inputs,
        ["read", "-format=json", f"auth/approle/role/{options.vault_approle_name}"],
        vault_environment=secret_inputs.vault_operator_environment,
        redactor=redactor,
        timeout=30.0,
    )
    operator_approle_payload = vault_json(
        options,
        secret_inputs,
        [
            "read",
            "-format=json",
            f"auth/approle/role/{DEFAULT_VAULT_OPERATOR_APPROLE_NAME}",
        ],
        vault_environment=secret_inputs.vault_operator_environment,
        redactor=redactor,
        timeout=30.0,
    )
    audit_payload = vault_json(
        options,
        secret_inputs,
        ["audit", "list", "-format=json"],
        vault_environment=secret_inputs.vault_operator_environment,
        redactor=redactor,
        timeout=30.0,
    )
    actual_policy_text = _normalized_vault_policy_text(
        vault_text(
            options,
            secret_inputs,
            ["policy", "read", VAULT_SIGNER_POLICY_NAME],
            vault_environment=secret_inputs.vault_operator_environment,
            redactor=redactor,
            timeout=30.0,
        )
    )
    actual_operator_policy_text = _normalized_vault_policy_text(
        vault_text(
            options,
            secret_inputs,
            ["policy", "read", VAULT_OPERATOR_POLICY_NAME],
            vault_environment=secret_inputs.vault_operator_environment,
            redactor=redactor,
            timeout=30.0,
        )
    )
    signer_identity_payload = vault_json(
        options,
        secret_inputs,
        ["token", "lookup", "-format=json"],
        vault_environment=secret_inputs.vault_environment,
        redactor=redactor,
        timeout=30.0,
    )
    sign_input = base64.b64encode(
        f"synara-vault-kms-admission-gate::{uuid.uuid4()}".encode("utf-8")
    ).decode("ascii")
    sign_payload = vault_json(
        options,
        secret_inputs,
        [
            "write",
            "-format=json",
            supply_chain.VAULT_TRANSIT_AUDIT_REQUEST_PATH,
            f"input={sign_input}",
        ],
        vault_environment=secret_inputs.vault_environment,
        redactor=redactor,
        timeout=30.0,
    )
    vault_address = secret_inputs.vault_environment["VAULT_ADDR"]
    parsed_address = urllib.parse.urlsplit(vault_address)
    status_obj = _json_object(
        status_payload,
        code="release.vault_kms_vault_invalid",
        message="Vault status output was malformed.",
    )
    raft_obj = _json_object(
        raft_payload,
        code="release.vault_kms_vault_invalid",
        message="Vault raft peer output was malformed.",
    )
    key_obj = _json_object(
        key_payload.get("data"),
        code="release.vault_kms_vault_invalid",
        message="Vault transit key output was malformed.",
    )
    if parsed_address.scheme != "https":
        raise ReleaseGateError(
            "release.vault_kms_tls_required",
            "The configured Vault address did not use HTTPS.",
            {"environment": options.vault_address_env},
        )
    unauthenticated_generate_root_attempt_status = (
        _probe_unauthenticated_generate_root_attempt_status(
            vault_address,
            vault_cacert=pathlib.Path(secret_inputs.vault_environment["VAULT_CACERT"]),
            timeout=30.0,
        )
    )
    if unauthenticated_generate_root_attempt_status != 403:
        raise ReleaseGateError(
            "release.vault_kms_break_glass_invalid",
            "Vault still allowed unauthenticated generate-root access outside the checked-in fail-closed baseline.",
            {"status": unauthenticated_generate_root_attempt_status},
        )
    peer_section = raft_obj.get("data")
    peers = peer_section.get("config", {}).get("servers") if isinstance(peer_section, dict) else None
    if not isinstance(peers, list) or len(peers) < REQUIRED_VAULT_PEERS:
        raise ReleaseGateError(
            "release.vault_kms_raft_invalid",
            "Vault did not report three raft peers.",
            {"peerCount": len(peers) if isinstance(peers, list) else None},
        )
    if (
        status_obj.get("initialized") is not True
        or status_obj.get("sealed") is not False
        or status_obj.get("ha_enabled") is not True
        or status_obj.get("storage_type") != "raft"
    ):
        raise ReleaseGateError(
            "release.vault_kms_status_invalid",
            "Vault did not report initialized HA raft status.",
            {
                "initialized": status_obj.get("initialized"),
                "sealed": status_obj.get("sealed"),
                "haEnabled": status_obj.get("ha_enabled"),
                "storageType": status_obj.get("storage_type"),
            },
        )
    latest_key_version = key_obj.get("latest_version")
    key_versions = key_obj.get("keys")
    latest_key = (
        key_versions.get(str(latest_key_version))
        if isinstance(key_versions, dict)
        and isinstance(latest_key_version, int)
        and not isinstance(latest_key_version, bool)
        else None
    )
    if (
        key_obj.get("type") != EXPECTED_VAULT_KEY_TYPE
        or key_obj.get("exportable") is not False
        or key_obj.get("allow_plaintext_backup") is not False
        or key_obj.get("deletion_allowed") is not False
        or key_obj.get("derived") is not False
        or key_obj.get("supports_signing") is not True
        or key_obj.get("supports_encryption") is not False
        or key_obj.get("supports_decryption") is not False
        or key_obj.get("auto_rotate_period")
        != EXPECTED_VAULT_KEY_AUTO_ROTATE_PERIOD_SECONDS
        or not isinstance(latest_key_version, int)
        or isinstance(latest_key_version, bool)
        or latest_key_version < 1
        or not isinstance(latest_key, dict)
        or not isinstance(latest_key.get("public_key"), str)
        or not latest_key["public_key"].strip()
    ):
        raise ReleaseGateError(
            "release.vault_kms_key_invalid",
            "Vault Transit did not expose the required non-exportable, non-deletable ecdsa-p256 signing-key boundary.",
            {
                "type": key_obj.get("type"),
                "exportable": key_obj.get("exportable"),
                "allowPlaintextBackup": key_obj.get("allow_plaintext_backup"),
                "deletionAllowed": key_obj.get("deletion_allowed"),
                "derived": key_obj.get("derived"),
                "supportsSigning": key_obj.get("supports_signing"),
                "supportsEncryption": key_obj.get("supports_encryption"),
                "supportsDecryption": key_obj.get("supports_decryption"),
                "autoRotatePeriodSeconds": key_obj.get("auto_rotate_period"),
                "latestVersion": latest_key_version,
            },
        )
    expected_policies = tuple(sorted(set(options.expected_approle_policies)))
    if expected_policies != (VAULT_SIGNER_POLICY_NAME,):
        raise ReleaseGateError(
            "release.vault_kms_source_invalid",
            "The production signer must use only the checked-in signer policy.",
            {"expectedPolicyHash": sha256_text(VAULT_SIGNER_POLICY_NAME)},
        )
    signer_role = _verify_approle_configuration(
        signer_approle_payload,
        role_name=options.vault_approle_name,
        expected_policies=expected_policies,
        constraints=SIGNER_ROLE_CONSTRAINTS,
    )
    operator_role = _verify_approle_configuration(
        operator_approle_payload,
        role_name=DEFAULT_VAULT_OPERATOR_APPROLE_NAME,
        expected_policies=(VAULT_OPERATOR_POLICY_NAME,),
        constraints=OPERATOR_ROLE_CONSTRAINTS,
    )
    operator_identity = _verify_vault_token_identity(
        operator_identity_payload,
        role_name=DEFAULT_VAULT_OPERATOR_APPROLE_NAME,
        expected_policies=(VAULT_OPERATOR_POLICY_NAME,),
        token_environment=options.vault_operator_token_env,
        maximum_creation_ttl=OPERATOR_ROLE_CONSTRAINTS["token_ttl"],
    )
    signer_identity = _verify_vault_token_identity(
        signer_identity_payload,
        role_name=options.vault_approle_name,
        expected_policies=expected_policies,
        token_environment=options.vault_token_env,
        maximum_creation_ttl=SIGNER_ROLE_CONSTRAINTS["token_ttl"],
    )
    if signer_identity["identitySha256"] == operator_identity["identitySha256"]:
        raise ReleaseGateError(
            "release.vault_kms_identity_invalid",
            "The Vault signer and operator identities were not independent.",
        )
    if actual_policy_text != expected_policy_text:
        raise ReleaseGateError(
            "release.vault_kms_policy_invalid",
            "Vault did not expose the checked-in least-privilege signer policy text.",
            {
                "policyName": VAULT_SIGNER_POLICY_NAME,
                "expectedPolicySha256": sha256_text(expected_policy_text),
                "actualPolicySha256": sha256_text(actual_policy_text),
            },
        )
    if actual_operator_policy_text != expected_operator_policy_text:
        raise ReleaseGateError(
            "release.vault_kms_policy_invalid",
            "Vault did not expose the checked-in read-only operator policy text.",
            {
                "policyName": VAULT_OPERATOR_POLICY_NAME,
                "expectedPolicySha256": sha256_text(expected_operator_policy_text),
                "actualPolicySha256": sha256_text(actual_operator_policy_text),
            },
        )
    if not isinstance(audit_payload, dict):
        raise ReleaseGateError(
            "release.vault_kms_audit_invalid",
            "Vault did not expose a valid audit-device listing.",
        )
    expected_audit_devices = sorted(
        _expected_vault_audit_devices(),
        key=lambda item: (item["path"], item["filePath"]),
    )
    file_audit_devices: list[dict[str, Any]] = []
    for raw_device_path, raw_device in audit_payload.items():
        if not isinstance(raw_device, dict) or raw_device.get("type") != "file":
            raise ReleaseGateError(
                "release.vault_kms_audit_invalid",
                "Vault exposed an unexpected non-file audit device outside the checked-in production baseline.",
                {
                    "devicePath": str(raw_device_path).rstrip("/"),
                    "deviceType": raw_device.get("type") if isinstance(raw_device, dict) else None,
                },
            )
        options_obj = raw_device.get("options") if isinstance(raw_device.get("options"), dict) else {}
        file_path = (
            options_obj.get("file_path")
            or options_obj.get("filePath")
            or raw_device.get("file_path")
            or raw_device.get("filePath")
        )
        if not isinstance(file_path, str) or not file_path.startswith(VAULT_AUDIT_PATH_PREFIX):
            raise ReleaseGateError(
                "release.vault_kms_audit_invalid",
                "Vault audit devices did not keep their file sink under /vault/audit/.",
                {
                    "devicePath": str(raw_device_path).rstrip("/"),
                    "filePath": file_path,
                },
            )
        file_audit_devices.append(
            {
                "path": str(raw_device_path).rstrip("/"),
                "type": "file",
                "filePath": file_path,
            }
        )
    file_audit_devices = sorted(file_audit_devices, key=lambda item: (item["path"], item["filePath"]))
    if file_audit_devices != expected_audit_devices:
        raise ReleaseGateError(
            "release.vault_kms_audit_invalid",
            "Vault did not expose exactly the two checked-in PVC-backed file audit devices.",
            {
                "expectedAuditDevices": expected_audit_devices,
                "actualAuditDevices": file_audit_devices,
                "auditDeviceCount": len(audit_payload),
            },
        )
    request_id = sign_payload.get("request_id")
    if not isinstance(request_id, str) or not request_id:
        raise ReleaseGateError(
            "release.vault_kms_audit_invalid",
            "Vault canary Transit sign output omitted its request ID.",
        )
    return {
        "kubernetes": {
            "namespace": options.vault_namespace,
            "selector": options.vault_selector,
            "statefulSet": statefulset_name,
            "imageReference": live_image,
            "podCount": len(items),
            "runningPods": running_pods,
            "readyPods": ready_pods,
            "podNames": sorted(pod_names),
            "distinctNodeCount": len(node_names),
            "nodeNames": sorted(node_names),
            "dataPvcCount": len(data_pvcs),
            "auditPvcCount": len(audit_pvcs),
            "dataPvcNames": data_pvcs,
            "auditPvcNames": audit_pvcs,
            "auditVolumeMountPath": VAULT_AUDIT_MOUNT_PATH,
            "auditVolumeNames": sorted(audit_volume_names),
            "pdbName": baseline["pdbName"],
            "pdbMinAvailable": baseline["pdbMinAvailable"],
            "pdbSelector": baseline["pdbSelector"],
            "networkPolicyName": baseline["networkPolicyName"],
            "networkPolicyIngressSha256": sha256_text(_stringify_json(actual_ingress)),
            "networkPolicyEgressSha256": sha256_text(_stringify_json(actual_egress)),
            "configMapName": baseline["configMapName"],
            "serverConfigSha256": sha256_text(live_server_config),
            "auditObservabilityConfigMapName": baseline["auditObservabilityConfigMapName"],
            "auditObservabilityConfigSha256": sha256_text(
                _stringify_json(actual_audit_observability_hashes)
            ),
            "auditShipperImageReference": baseline["auditShipper"]["imageReference"],
            "auditRotationImageReference": baseline["auditRotation"]["imageReference"],
            "auditShipperSecretRefs": baseline["auditShipper"]["envSecretRefs"],
            "auditSiemSecretName": baseline["auditSiemSecretName"],
            "auditSiemEgress": baseline["auditSiemEgress"],
            "retryJoinPeerCount": len(live_retry_join),
            "retryJoinSha256": sha256_text(_stringify_json(live_retry_join)),
            "enableUnauthenticatedAccess": list(baseline["enableUnauthenticatedAccess"]),
            "unauthenticatedGenerateRootAttemptStatus": unauthenticated_generate_root_attempt_status,
            "vaultCaEnvironment": "VAULT_CACERT",
            "tlsSecretName": baseline["tlsSecretName"],
            "tlsSecretType": tls_secret_type,
            "tlsSecretKeys": sorted(tls_secret_data),
        },
        "vault": {
            "addressEnvironment": options.vault_address_env,
            "tlsEnabled": True,
            "haEnabled": True,
            "storageType": "raft",
            "raftPeerCount": len(peers),
            "keyReference": EXPECTED_VAULT_KEY_REFERENCE,
            "keyType": EXPECTED_VAULT_KEY_TYPE,
            "keyExportable": False,
            "keyPlaintextBackupAllowed": False,
            "keyDeletionAllowed": False,
            "keyDerived": False,
            "keyLatestVersion": latest_key_version,
            "keyAutoRotatePeriodSeconds": EXPECTED_VAULT_KEY_AUTO_ROTATE_PERIOD_SECONDS,
            "principal": f"auth/approle/role/{options.vault_approle_name}",
            "tokenType": "batch",
            "policyName": VAULT_SIGNER_POLICY_NAME,
            "policyHash": sha256_text(expected_policy_text),
            "policyCount": len(expected_policies),
            "operatorPolicyName": VAULT_OPERATOR_POLICY_NAME,
            "operatorPolicyHash": sha256_text(expected_operator_policy_text),
            "identities": {
                "signer": signer_identity,
                "operator": operator_identity,
            },
            "appRoles": {
                "signer": signer_role,
                "operator": operator_role,
            },
            "auditDeviceCount": len(audit_payload),
            "fileAuditDeviceCount": len(file_audit_devices),
            "auditDeviceTypes": sorted(
                str(device.get("type"))
                for device in audit_payload.values()
                if isinstance(device, dict) and isinstance(device.get("type"), str)
            ),
            "auditDevices": file_audit_devices,
            "transitAuditRequestId": request_id,
        },
    }


def export_vault_public_key(
    options: GateOptions,
    secret_inputs: SecretInputs,
    *,
    state_dir: pathlib.Path,
    redactor: acceptance.SecretRedactor,
) -> dict[str, Any]:
    completed = cosign_completed(
        options,
        secret_inputs,
        ["public-key", "--key", EXPECTED_VAULT_KEY_REFERENCE],
        redactor=redactor,
        timeout=30.0,
    )
    if completed.returncode != 0:
        raise ReleaseGateError(
            "release.vault_kms_cosign_failed",
            "Cosign could not export the Vault Transit public key.",
            {"outputExcerpt": _output_excerpt(completed, redactor)},
        )
    public_key = completed.stdout.strip() + "\n"
    if PEM_PUBLIC_KEY_PATTERN.fullmatch(public_key) is None:
        raise ReleaseGateError(
            "release.vault_kms_public_key_invalid",
            "Cosign did not export a valid PEM public key.",
        )
    public_key_path = state_dir / "public-key" / "cosign.pub"
    public_key_path.parent.mkdir(parents=True, exist_ok=True)
    public_key_path.write_text(public_key, encoding="utf-8")
    public_key_path.chmod(0o600)
    return {
        "path": public_key_path,
        "publicKeySha256": sha256_text(public_key),
        "keyReference": EXPECTED_VAULT_KEY_REFERENCE,
    }


class RegistryHttpsClient:
    def __init__(
        self,
        host: str,
        *,
        cafile: pathlib.Path,
        username: str,
        password: str,
        timeout: float,
    ) -> None:
        self.host = host
        self.cafile = cafile
        self.username = username
        self.password = password
        self.timeout = timeout

    def request(
        self,
        method: str,
        path: str,
        *,
        authenticated: bool,
        accept: Sequence[str] | None = None,
    ) -> tuple[int, Mapping[str, str], bytes, str]:
        context = ssl.create_default_context(cafile=str(self.cafile))
        headers: dict[str, str] = {}
        if accept:
            headers["Accept"] = ", ".join(accept)
        if authenticated:
            token = base64.b64encode(f"{self.username}:{self.password}".encode("utf-8")).decode("ascii")
            headers["Authorization"] = f"Basic {token}"
        connection = http.client.HTTPSConnection(
            self.host,
            timeout=self.timeout,
            context=context,
        )
        try:
            connection.request(method, path, headers=headers)
            response = connection.getresponse()
            body = response.read()
            certificate = connection.sock.getpeercert(binary_form=True) if connection.sock else None
            certificate_sha256 = sha256_bytes(certificate or b"")
            return response.status, dict(response.getheaders()), body, certificate_sha256
        except OSError:
            raise ReleaseGateError(
                "release.vault_kms_registry_probe_failed",
                "The TLS registry probe could not complete.",
                {"registryHost": self.host},
            ) from None
        finally:
            connection.close()


def http_header_value(headers: Mapping[str, str], name: str) -> str:
    normalized_name = name.casefold()
    return next(
        (value for header_name, value in headers.items() if header_name.casefold() == normalized_name),
        "",
    )


def _manifest_path(reference: ImageReference) -> str:
    target = reference.digest or reference.tag
    if target is None:
        raise ValueError("image reference did not include a tag or digest")
    quoted_target = urllib.parse.quote(target, safe=":@")
    repository = "/".join(
        urllib.parse.quote(component, safe="") for component in reference.repository.split("/")
    )
    return f"/v2/{repository}/manifests/{quoted_target}"


def verify_tls_registry(
    release_evidence: RegistryReleaseEvidence,
    secret_inputs: SecretInputs,
) -> tuple[RegistryHttpsClient, dict[str, Any]]:
    registry_host = release_evidence.image_repository.split("/", 1)[0]
    client = RegistryHttpsClient(
        registry_host,
        cafile=secret_inputs.registry_ca_path,
        username=secret_inputs.registry_username,
        password=secret_inputs.registry_password,
        timeout=20.0,
    )
    unauthenticated = client.request("GET", "/v2/", authenticated=False)
    authenticated = client.request("GET", "/v2/", authenticated=True)
    unauth_status, unauth_headers, _body, certificate_sha256 = unauthenticated
    auth_status, _auth_headers, _auth_body, _certificate_sha256 = authenticated
    challenge = http_header_value(unauth_headers, "WWW-Authenticate")
    if unauth_status != 401 or "basic" not in challenge.lower():
        raise ReleaseGateError(
            "release.vault_kms_registry_boundary_invalid",
            "The production registry did not enforce an unauthenticated Basic-auth challenge over TLS.",
            {"status": unauth_status, "challenge": challenge[:200]},
        )
    if auth_status != 200:
        raise ReleaseGateError(
            "release.vault_kms_registry_boundary_invalid",
            "The production registry did not accept authenticated TLS access.",
            {"status": auth_status},
        )
    return client, {
        "registryHost": registry_host,
        "tlsCertificateSha256": certificate_sha256,
        "basicAuthEnforced": True,
        "principalSha256": sha256_text(secret_inputs.registry_username),
        "principalEnvironment": secret_inputs.registry_username_env,
        "passwordEnvironment": secret_inputs.registry_password_env,
        "caCertificateEnvironment": secret_inputs.registry_ca_env,
    }


def validate_registry_runtime_evidence_against_live_boundary(
    release_evidence: RegistryReleaseEvidence,
    *,
    options: GateOptions,
    registry_boundary: Mapping[str, Any],
) -> dict[str, Any]:
    registry_access = release_evidence.registry_access
    expected_access = {
        "usernameEnvironment": options.registry_username_env,
        "passwordEnvironment": options.registry_password_env,
        "caCertEnvironment": options.registry_ca_cert_env,
    }
    if registry_access != expected_access:
        raise ReleaseGateError(
            "release.vault_kms_registry_boundary_invalid",
            "The current Vault gate Registry CA/auth boundary did not match the fresh production registry_release_gate runtime evidence.",
            {
                "expectedRegistryAccess": registry_access,
                "actualRegistryAccess": expected_access,
            },
        )
    runtime_evidence = release_evidence.production_registry_boundary.get("liveRuntimeEvidence")
    if not isinstance(runtime_evidence, dict):
        raise ReleaseGateError(
            "release.vault_kms_registry_boundary_invalid",
            "The production registry_release_gate report omitted the fresh live Registry runtime evidence required for downstream binding.",
        )
    current_registry_host = registry_boundary.get("registryHost")
    current_peer_certificate_sha256 = registry_boundary.get("tlsCertificateSha256")
    current_registry_authority = (
        f"https://{current_registry_host}" if isinstance(current_registry_host, str) else None
    )
    if (
        runtime_evidence.get("registryHost") != current_registry_host
        or runtime_evidence.get("registryAuthority") != current_registry_authority
        or runtime_evidence.get("repositoryAuthority") != release_evidence.image_repository
        or runtime_evidence.get("tlsPeerCertificateSha256") != current_peer_certificate_sha256
    ):
        raise ReleaseGateError(
            "release.vault_kms_registry_boundary_invalid",
            "The current Vault gate TLS registry probe did not match the fresh production registry_release_gate runtime evidence.",
            {
                "expectedRegistryHost": runtime_evidence.get("registryHost"),
                "actualRegistryHost": current_registry_host,
                "expectedRegistryAuthority": runtime_evidence.get("registryAuthority"),
                "actualRegistryAuthority": current_registry_authority,
                "expectedRepositoryAuthority": runtime_evidence.get("repositoryAuthority"),
                "actualRepositoryAuthority": release_evidence.image_repository,
                "expectedTlsPeerCertificateSha256": runtime_evidence.get("tlsPeerCertificateSha256"),
                "actualTlsPeerCertificateSha256": current_peer_certificate_sha256,
            },
        )
    if (
        registry_boundary.get("principalEnvironment") != registry_access["usernameEnvironment"]
        or registry_boundary.get("passwordEnvironment") != registry_access["passwordEnvironment"]
        or registry_boundary.get("caCertificateEnvironment") != registry_access["caCertEnvironment"]
    ):
        raise ReleaseGateError(
            "release.vault_kms_registry_boundary_invalid",
            "The current Vault gate Registry probe did not use the same non-secret CA/auth environment boundary recorded by the production registry_release_gate report.",
        )
    return {
        "registryAccess": dict(sorted(registry_access.items())),
        "registryAuthority": str(runtime_evidence["registryAuthority"]),
        "repositoryAuthority": str(runtime_evidence["repositoryAuthority"]),
        "tlsPeerCertificateSha256": str(runtime_evidence["tlsPeerCertificateSha256"]),
        "runtimeEvidenceCollectedAt": str(runtime_evidence["collectedAt"]),
        "runtimeEvidenceSha256": str(runtime_evidence["runtimeEvidenceSha256"]),
    }


def resolve_registry_digest_for_tag(
    client: RegistryHttpsClient,
    reference: ImageReference,
) -> str:
    if reference.tag is None:
        raise ValueError("tag-drift resolution requires a tag reference")
    status, headers, _body, _certificate_sha256 = client.request(
        "GET",
        _manifest_path(reference),
        authenticated=True,
        accept=REGISTRY_ACCEPT_HEADERS,
    )
    if status != 200:
        raise ReleaseGateError(
            "release.vault_kms_registry_probe_failed",
            "The production registry tag manifest probe did not return success.",
            {"status": status},
        )
    digest = headers.get("Docker-Content-Digest")
    if not isinstance(digest, str) or DIGEST_PATTERN.fullmatch(digest) is None:
        raise ReleaseGateError(
            "release.vault_kms_registry_probe_failed",
            "The production registry tag manifest probe did not return a digest header.",
        )
    return digest


def verify_positive_signature(
    options: GateOptions,
    secret_inputs: SecretInputs,
    public_key_path: pathlib.Path,
    signed_image: ImageReference,
    release_evidence: RegistryReleaseEvidence,
    *,
    redactor: acceptance.SecretRedactor,
) -> dict[str, Any]:
    completed = cosign_completed(
        options,
        secret_inputs,
        [
            "verify",
            "--insecure-ignore-tlog=false",
            "--key",
            str(public_key_path),
            "--output",
            "json",
            signed_image.digest_reference,
        ],
        redactor=redactor,
        timeout=60.0,
    )
    if completed.returncode != 0:
        raise ReleaseGateError(
            "release.vault_kms_cosign_failed",
            "Cosign could not verify the signed production image with the exported public key.",
            {"outputExcerpt": _output_excerpt(completed, redactor)},
        )
    try:
        payload = json.loads(completed.stdout)
    except json.JSONDecodeError:
        raise ReleaseGateError(
            "release.vault_kms_cosign_invalid",
            "Cosign verification output was not valid JSON.",
        ) from None
    if not isinstance(payload, list) or not payload or not all(isinstance(item, dict) for item in payload):
        raise ReleaseGateError(
            "release.vault_kms_cosign_invalid",
            "Cosign verification output did not contain signature claim objects.",
        )
    try:
        verification = supply_chain.validate_cosign_verification(
            payload,
            reference=release_evidence.cached_signed_image,
            digest=signed_image.digest or "",
            annotations=release_evidence.cached_signature_annotations,
        )
    except common.ReleaseGateError as error:
        raise ReleaseGateError(
            "release.vault_kms_cosign_invalid",
            "Cosign verification did not return the cached production signature with the expected release annotations.",
            error.evidence,
        ) from None
    transparency_log = copy.deepcopy(release_evidence.cached_signature_transparency_log)
    rekor_entries = [
        {
            "index": entry["logIndex"],
            "integratedTime": entry["integratedTime"],
            "inclusionProofPresent": entry["inclusionProofPresent"],
            "inclusionProofHashCount": entry["inclusionProofHashCount"],
            "signedEntryTimestampPresent": entry["signedEntryTimestampPresent"],
            "signedEntryTimestampSha256": entry["signedEntryTimestampSha256"],
        }
        for entry in transparency_log["entries"]
    ]
    if not rekor_entries:
        raise ReleaseGateError(
            "release.vault_kms_cosign_invalid",
            "The fresh production Registry release evidence did not expose Rekor bundle entries.",
        )
    return {
        "reference": signed_image.digest_reference,
        "matchingSignatureCount": int(verification["verifiedSignatureCount"]),
        "annotations": dict(sorted(release_evidence.cached_signature_annotations.items())),
        "rekorEntries": rekor_entries,
        "transparencyLog": transparency_log,
        "verificationPayloadSha256": verification["verificationPayloadSha256"],
    }


def _owner_labels(run_id: str) -> dict[str, str]:
    return {
        OWNER_LABEL: run_id,
        OWNER_ANNOTATION: run_id,
    }


def _merge_admission_resource_names(
    primary: AdmissionResourceNames,
    secondary: AdmissionResourceNames,
) -> AdmissionResourceNames:
    return AdmissionResourceNames(
        public_key_configmap=primary.public_key_configmap or secondary.public_key_configmap,
        repository_configmap=primary.repository_configmap or secondary.repository_configmap,
        cluster_policy=primary.cluster_policy or secondary.cluster_policy,
        probe_pull_secret=primary.probe_pull_secret or secondary.probe_pull_secret,
        probe_secret_reader_role=primary.probe_secret_reader_role or secondary.probe_secret_reader_role,
        probe_secret_reader_binding=primary.probe_secret_reader_binding or secondary.probe_secret_reader_binding,
    )


def _kyverno_controllers_from_runtime(runtime: Mapping[str, Any]) -> tuple[KyvernoController, ...]:
    raw = runtime.get("verifyImageControllers")
    if not isinstance(raw, list) or not raw:
        raise ReleaseGateError(
            "release.vault_kms_kyverno_missing",
            "Kyverno did not expose the admission/background controller metadata required for private-registry verification.",
        )
    deduped: set[tuple[str, str, str, str]] = set()
    for item in raw:
        if not isinstance(item, dict):
            raise ReleaseGateError(
                "release.vault_kms_kyverno_missing",
                "Kyverno controller metadata was malformed.",
            )
        namespace = item.get("namespace")
        deployment = item.get("deployment")
        service_account = item.get("serviceAccount")
        component = item.get("component")
        if (
            not isinstance(namespace, str)
            or not isinstance(deployment, str)
            or not isinstance(service_account, str)
            or not isinstance(component, str)
            or component not in KYVERNO_VERIFY_IMAGE_COMPONENTS
        ):
            raise ReleaseGateError(
                "release.vault_kms_kyverno_missing",
                "Kyverno controller metadata was malformed.",
            )
        deduped.add((namespace, deployment, service_account, component))
    return tuple(
        KyvernoController(
            namespace=namespace,
            deployment=deployment,
            service_account=service_account,
            component=component,
        )
        for namespace, deployment, service_account, component in sorted(deduped)
    )


def _registry_dockerconfigjson(
    *,
    registry_host: str,
    secret_inputs: SecretInputs,
) -> str:
    auth = base64.b64encode(
        f"{secret_inputs.registry_username}:{secret_inputs.registry_password}".encode("utf-8")
    ).decode("ascii")
    return json.dumps(
        {
            "auths": {
                registry_host: {
                    "username": secret_inputs.registry_username,
                    "password": secret_inputs.registry_password,
                    "auth": auth,
                }
            }
        },
        sort_keys=True,
        separators=(",", ":"),
    )


def render_probe_access_resources(
    *,
    run_id: str,
    namespace: str,
    registry_host: str,
    secret_inputs: SecretInputs,
    kyverno_runtime: Mapping[str, Any],
    redactor: acceptance.SecretRedactor,
) -> tuple[AdmissionResourceNames, dict[str, str], tuple[tuple[str, str, str | None], ...], list[dict[str, Any]]]:
    controllers = _kyverno_controllers_from_runtime(kyverno_runtime)
    suffix = uuid.uuid5(uuid.NAMESPACE_URL, run_id + "::probe").hex[:10]
    names = AdmissionResourceNames(
        public_key_configmap=None,
        repository_configmap=None,
        cluster_policy=None,
        probe_pull_secret=f"synara-registry-probe-pull-{suffix}",
        probe_secret_reader_role=f"synara-registry-probe-reader-{suffix}",
        probe_secret_reader_binding=f"synara-registry-probe-reader-{suffix}",
    )
    labels = _owner_labels(run_id)
    dockerconfigjson = _registry_dockerconfigjson(registry_host=registry_host, secret_inputs=secret_inputs)
    dockerconfigjson_b64 = base64.b64encode(dockerconfigjson.encode("utf-8")).decode("ascii")
    redactor.add(dockerconfigjson, "[REDACTED_REGISTRY_DOCKERCONFIGJSON]")
    redactor.add(dockerconfigjson_b64, "[REDACTED_REGISTRY_DOCKERCONFIGJSON_B64]")
    probe_secret = {
        "apiVersion": "v1",
        "kind": "Secret",
        "metadata": {
            "name": names.probe_pull_secret,
            "namespace": namespace,
            "labels": labels,
        },
        "type": "kubernetes.io/dockerconfigjson",
        "data": {
            ".dockerconfigjson": dockerconfigjson_b64,
        },
    }
    role = {
        "apiVersion": "rbac.authorization.k8s.io/v1",
        "kind": "Role",
        "metadata": {
            "name": names.probe_secret_reader_role,
            "namespace": namespace,
            "labels": labels,
        },
        "rules": [
            {
                "apiGroups": [""],
                "resources": ["secrets"],
                "resourceNames": [names.probe_pull_secret],
                "verbs": ["get"],
            }
        ],
    }
    subjects = [
        {
            "kind": "ServiceAccount",
            "name": controller.service_account,
            "namespace": controller.namespace,
        }
        for controller in controllers
    ]
    role_binding = {
        "apiVersion": "rbac.authorization.k8s.io/v1",
        "kind": "RoleBinding",
        "metadata": {
            "name": names.probe_secret_reader_binding,
            "namespace": namespace,
            "labels": labels,
        },
        "roleRef": {
            "apiGroup": "rbac.authorization.k8s.io",
            "kind": "Role",
            "name": names.probe_secret_reader_role,
        },
        "subjects": subjects,
    }
    rendered_hashes = {
        "probePullSecret": sha256_text(_stringify_json(probe_secret)),
        "probeSecretReaderRole": sha256_text(_stringify_json(role)),
        "probeSecretReaderBinding": sha256_text(_stringify_json(role_binding)),
    }
    created_resources = (
        ("secret", str(names.probe_pull_secret), namespace),
        ("role", str(names.probe_secret_reader_role), namespace),
        ("rolebinding", str(names.probe_secret_reader_binding), namespace),
    )
    return names, rendered_hashes, created_resources, [probe_secret, role, role_binding]


def _apply_resource_documents(
    options: GateOptions,
    bundle_objects: Sequence[dict[str, Any]],
    *,
    redactor: acceptance.SecretRedactor,
) -> None:
    for document in bundle_objects:
        completed = kubectl_completed(
            options,
            ["apply", "-f", "-"],
            redactor=redactor,
            input_text=json.dumps(document),
            timeout=60.0,
        )
        if completed.returncode != 0:
            raise ReleaseGateError(
                "release.vault_kms_admission_apply_failed",
                "The temporary Kyverno admission bundle could not be applied.",
                {"outputExcerpt": _output_excerpt(completed, redactor)},
            )


def render_admission_bundle(
    *,
    run_id: str,
    namespace: str,
    repository_pattern: str,
    public_key: str,
    source_hashes: Mapping[str, str],
) -> tuple[AdmissionBundle, dict[str, Any], dict[str, Any], dict[str, Any]]:
    suffix = uuid.uuid5(uuid.NAMESPACE_URL, run_id).hex[:10]
    names = AdmissionResourceNames(
        public_key_configmap=f"synara-worker-cosign-public-key-{suffix}",
        repository_configmap=f"synara-worker-signing-settings-{suffix}",
        cluster_policy=f"verify-synara-worker-images-{suffix}",
    )
    labels = _owner_labels(run_id)
    public_key_configmap = {
        "apiVersion": "v1",
        "kind": "ConfigMap",
        "metadata": {
            "name": names.public_key_configmap,
            "namespace": namespace,
            "labels": {
                "cache.kyverno.io/enabled": "true",
                **labels,
            },
        },
        "data": {
            "cosignPublicKey": public_key,
        },
    }
    repository_configmap = {
        "apiVersion": "v1",
        "kind": "ConfigMap",
        "metadata": {
            "name": names.repository_configmap,
            "namespace": namespace,
            "labels": {
                "cache.kyverno.io/enabled": "true",
                **labels,
            },
        },
        "data": {
            "repositoryPattern": repository_pattern,
        },
    }
    cluster_policy = {
        "apiVersion": "kyverno.io/v1",
        "kind": "ClusterPolicy",
        "metadata": {
            "name": names.cluster_policy,
            "labels": labels,
            "annotations": {
                "pod-policies.kyverno.io/title": "Verify Synara Worker Images",
                "pod-policies.kyverno.io/category": "Supply Chain Security",
                AUTOGEN_CONTROLLERS_ANNOTATION: ",".join(REQUIRED_AUTOGEN_CONTROLLERS),
                OWNER_ANNOTATION: run_id,
            },
        },
        "spec": {
            "validationFailureAction": supply_chain.ADMISSION_VALIDATION_FAILURE_ACTION,
            "failurePolicy": supply_chain.ADMISSION_FAILURE_POLICY,
            "background": False,
            "webhookTimeoutSeconds": 30,
            "rules": [
                {
                    "name": "verify-synara-worker-images",
                    "match": {
                        "any": [{"resources": {"kinds": ["Pod"]}}],
                    },
                    "context": [
                        {
                            "name": "workerPublicKey",
                            "configMap": {
                                "namespace": namespace,
                                "name": names.public_key_configmap,
                            },
                        },
                    ],
                    "verifyImages": [
                        {
                            "imageReferences": [repository_pattern],
                            "imageRegistryCredentials": {
                                "secrets": [KYVERNO_REGISTRY_PULL_SECRET_NAME],
                            },
                            "mutateDigest": True,
                            "verifyDigest": True,
                            "required": True,
                            "attestors": [
                                {
                                    "count": 1,
                                    "entries": [
                                        {
                                            "keys": {
                                                "publicKeys": "{{ workerPublicKey.data.cosignPublicKey }}",
                                                "rekor": {
                                                    "url": supply_chain.TRANSPARENCY_LOG_URL,
                                                    "ignoreTlog": False,
                                                },
                                            }
                                        }
                                    ],
                                }
                            ],
                        }
                    ],
                }
            ],
        },
    }
    validate_cluster_policy_spec(
        cluster_policy["spec"],
        public_key_configmap_namespace=namespace,
        public_key_configmap_name=names.public_key_configmap,
        repository_pattern=repository_pattern,
    )
    rendered_hashes = {
        "publicKeyConfigMap": sha256_text(
            json.dumps(public_key_configmap, sort_keys=True, separators=(",", ":"))
        ),
        "repositoryConfigMap": sha256_text(
            json.dumps(repository_configmap, sort_keys=True, separators=(",", ":"))
        ),
        "clusterPolicy": sha256_text(
            json.dumps(cluster_policy, sort_keys=True, separators=(",", ":"))
        ),
    }
    bundle = AdmissionBundle(
        mode="apply-owned",
        namespace=namespace,
        repository_pattern=repository_pattern,
        names=names,
        source_hashes=dict(sorted(source_hashes.items())),
        rendered_hashes=dict(sorted(rendered_hashes.items())),
        created_resources=(
            ("configmap", names.public_key_configmap, namespace),
            ("configmap", names.repository_configmap, namespace),
            ("clusterpolicy", names.cluster_policy, None),
        ),
    )
    return bundle, public_key_configmap, repository_configmap, cluster_policy


def apply_owned_admission_bundle(
    options: GateOptions,
    bundle: AdmissionBundle,
    bundle_objects: Sequence[dict[str, Any]],
    *,
    run_id: str,
    kyverno_runtime: Mapping[str, Any],
    secret_inputs: SecretInputs,
    redactor: acceptance.SecretRedactor,
) -> AdmissionBundle:
    _apply_resource_documents(options, bundle_objects, redactor=redactor)
    registry_bundle = secret_inputs.registry_ca_path.read_text(encoding="utf-8")
    if PEM_CERTIFICATE_PATTERN.fullmatch(registry_bundle) is None:
        raise ReleaseGateError(
            "release.vault_kms_environment_invalid",
            "The configured Registry CA certificate was not PEM encoded.",
            {"environment": secret_inputs.registry_ca_env},
        )
    updated_patches: list[KyvernoControllerPatch] = []
    created_resources = list(bundle.created_resources)
    for index, controller in enumerate(_kyverno_controllers_from_runtime(kyverno_runtime)):
        deployment = kubectl_json(
            options,
            [
                "-n",
                controller.namespace,
                "get",
                "deployment",
                controller.deployment,
                "-o",
                "json",
            ],
            redactor=redactor,
            timeout=30.0,
        )
        deployment_metadata = deployment.get("metadata")
        if not isinstance(deployment_metadata, dict):
            raise ReleaseGateError(
                "release.vault_kms_admission_apply_failed",
                "The target Kyverno Deployment metadata was malformed.",
                {"deployment": controller.deployment, "namespace": controller.namespace},
            )
        deployment_annotations = deployment_metadata.setdefault("annotations", {})
        if not isinstance(deployment_annotations, dict):
            deployment_metadata["annotations"] = {}
            deployment_annotations = deployment_metadata["annotations"]
        existing_owner = deployment_annotations.get(OWNER_ANNOTATION)
        if existing_owner not in (None, run_id):
            raise ReleaseGateError(
                "release.vault_kms_admission_apply_failed",
                "A Kyverno Deployment was already claimed by another admission-gate run.",
                {
                    "deployment": controller.deployment,
                    "namespace": controller.namespace,
                    "existingOwner": existing_owner,
                },
            )
        spec, template_metadata, containers, volumes = _deployment_template_spec(
            deployment,
            code="release.vault_kms_admission_apply_failed",
            message="The target Kyverno Deployment template was malformed.",
        )
        template_annotations = template_metadata.setdefault("annotations", {})
        if not isinstance(template_annotations, dict):
            template_metadata["annotations"] = {}
            template_annotations = template_metadata["annotations"]
        deployment_annotations[OWNER_ANNOTATION] = run_id
        template_annotations[OWNER_ANNOTATION] = run_id
        template_annotations["synara.dev/registry-ca-refresh"] = run_id
        injected_mount = False
        ca_configmap_name: str
        ca_data_key: str
        volume_name: str
        original_ca_bundle: str | None = None
        existing_mount: dict[str, Any] | None = None
        for container in containers:
            mounts = container.get("volumeMounts")
            if isinstance(mounts, list):
                match = next(
                    (
                        mount
                        for mount in mounts
                        if isinstance(mount, dict) and mount.get("mountPath") == KYVERNO_CA_MOUNT_PATH
                    ),
                    None,
                )
                if match is not None:
                    existing_mount = match
                    break
        if existing_mount is not None:
            volume_name_value = existing_mount.get("name")
            if not isinstance(volume_name_value, str) or not volume_name_value:
                raise ReleaseGateError(
                    "release.vault_kms_admission_apply_failed",
                    "The existing Kyverno controller CA mount did not expose a stable volume name.",
                    {"deployment": controller.deployment, "namespace": controller.namespace},
                )
            matching_volume = next(
                (volume for volume in volumes if volume.get("name") == volume_name_value),
                None,
            )
            if not isinstance(matching_volume, dict):
                raise ReleaseGateError(
                    "release.vault_kms_admission_apply_failed",
                    "The existing Kyverno controller CA volume could not be resolved.",
                    {"deployment": controller.deployment, "namespace": controller.namespace},
                )
            config_map = matching_volume.get("configMap")
            if not isinstance(config_map, dict) or not isinstance(config_map.get("name"), str):
                raise ReleaseGateError(
                    "release.vault_kms_admission_apply_failed",
                    "The existing Kyverno controller CA volume did not use an official ConfigMap source.",
                    {"deployment": controller.deployment, "namespace": controller.namespace},
                )
            ca_configmap_name = config_map["name"]
            ca_data_key = _configmap_data_key_from_mount(matching_volume, existing_mount)
            volume_name = volume_name_value
            config_map_resource = kubectl_json(
                options,
                [
                    "-n",
                    controller.namespace,
                    "get",
                    "configmap",
                    ca_configmap_name,
                    "-o",
                    "json",
                ],
                redactor=redactor,
                timeout=30.0,
            )
            config_map_data = config_map_resource.get("data")
            if not isinstance(config_map_data, dict):
                raise ReleaseGateError(
                    "release.vault_kms_admission_apply_failed",
                    "The existing Kyverno controller CA ConfigMap did not expose text data.",
                    {"deployment": controller.deployment, "namespace": controller.namespace},
                )
            original_ca_bundle = config_map_data.get(ca_data_key)
            if original_ca_bundle is not None and not isinstance(original_ca_bundle, str):
                raise ReleaseGateError(
                    "release.vault_kms_admission_apply_failed",
                    "The existing Kyverno controller CA ConfigMap data was malformed.",
                    {"deployment": controller.deployment, "namespace": controller.namespace},
                )
            config_map_data[ca_data_key] = _merge_pem_bundle(original_ca_bundle, registry_bundle)
            _apply_resource_documents(
                options,
                [_cleanup_resource_document(config_map_resource)],
                redactor=redactor,
            )
        else:
            injected_mount = True
            volume_name = f"synara-registry-ca-{uuid.uuid5(uuid.NAMESPACE_URL, run_id + controller.deployment).hex[:10]}"
            ca_configmap_name = f"synara-kyverno-registry-ca-{index}-{uuid.uuid5(uuid.NAMESPACE_URL, run_id).hex[:8]}"
            ca_data_key = KYVERNO_CA_BUNDLE_KEY
            config_map_resource = {
                "apiVersion": "v1",
                "kind": "ConfigMap",
                "metadata": {
                    "name": ca_configmap_name,
                    "namespace": controller.namespace,
                    "labels": _owner_labels(run_id),
                },
                "data": {
                    ca_data_key: registry_bundle.rstrip() + "\n",
                },
            }
            _apply_resource_documents(options, [config_map_resource], redactor=redactor)
            created_resources.append(("configmap", ca_configmap_name, controller.namespace))
            volumes.append(
                {
                    "name": volume_name,
                    "configMap": {
                        "name": ca_configmap_name,
                        "items": [{"key": ca_data_key, "path": ca_data_key}],
                    },
                }
            )
            for container in containers:
                mounts = container.get("volumeMounts")
                if not isinstance(mounts, list):
                    mounts = []
                    container["volumeMounts"] = mounts
                mounts.append(
                    {
                        "name": volume_name,
                        "mountPath": KYVERNO_CA_MOUNT_PATH,
                        "subPath": ca_data_key,
                        "readOnly": True,
                    }
                )
        _apply_resource_documents(
            options,
            [_cleanup_resource_document(deployment)],
            redactor=redactor,
        )
        rollout = kubectl_completed(
            options,
            [
                "-n",
                controller.namespace,
                "rollout",
                "status",
                f"deployment/{controller.deployment}",
                "--timeout=180s",
            ],
            redactor=redactor,
            timeout=200.0,
        )
        if rollout.returncode != 0:
            raise ReleaseGateError(
                "release.vault_kms_admission_apply_failed",
                "A Kyverno controller did not become ready after the private-registry CA update.",
                {
                    "deployment": controller.deployment,
                    "namespace": controller.namespace,
                    "outputExcerpt": _output_excerpt(rollout, redactor),
                },
            )
        updated_patches.append(
            KyvernoControllerPatch(
                namespace=controller.namespace,
                deployment=controller.deployment,
                service_account=controller.service_account,
                component=controller.component,
                ca_configmap_name=ca_configmap_name,
                ca_data_key=ca_data_key,
                volume_name=volume_name,
                injected_mount=injected_mount,
                original_ca_bundle=original_ca_bundle,
            )
        )
    return dataclasses.replace(
        bundle,
        created_resources=tuple(created_resources),
        controller_patches=tuple(updated_patches),
    )


def _resource_owner(
    options: GateOptions,
    *,
    kind: str,
    name: str,
    namespace: str | None,
    redactor: acceptance.SecretRedactor,
) -> str | None:
    arguments = ["get", kind, name]
    if namespace is not None:
        arguments[0:0] = ["-n", namespace]
    arguments.extend(["-o", "json"])
    completed = kubectl_completed(
        options,
        arguments,
        redactor=redactor,
        timeout=20.0,
    )
    if completed.returncode != 0:
        return None
    try:
        payload = json.loads(completed.stdout)
    except json.JSONDecodeError:
        raise ReleaseGateError(
            "release.vault_kms_cleanup_failed",
            "The owned admission resource could not be inspected as JSON.",
        ) from None
    metadata = payload.get("metadata")
    labels = metadata.get("labels") if isinstance(metadata, dict) else None
    if not isinstance(labels, dict):
        return None
    owner = labels.get(OWNER_LABEL)
    return owner if isinstance(owner, str) else None


def cleanup_owned_resources(
    options: GateOptions,
    bundle: AdmissionBundle,
    *,
    run_id: str,
    redactor: acceptance.SecretRedactor,
) -> dict[str, Any]:
    removed: list[str] = []
    restored: list[str] = []
    for patch in bundle.controller_patches:
        if patch.original_ca_bundle is not None:
            config_map_resource = kubectl_json(
                options,
                [
                    "-n",
                    patch.namespace,
                    "get",
                    "configmap",
                    patch.ca_configmap_name,
                    "-o",
                    "json",
                ],
                redactor=redactor,
                timeout=30.0,
            )
            config_map_data = config_map_resource.get("data")
            if not isinstance(config_map_data, dict):
                raise ReleaseGateError(
                    "release.vault_kms_cleanup_failed",
                    "The existing Kyverno controller CA ConfigMap could not be restored.",
                    {"deployment": patch.deployment, "namespace": patch.namespace},
                )
            config_map_data[patch.ca_data_key] = patch.original_ca_bundle
            _apply_resource_documents(
                options,
                [_cleanup_resource_document(config_map_resource)],
                redactor=redactor,
            )
            restored.append(f"configmap:{patch.namespace}:{patch.ca_configmap_name}")
        deployment = kubectl_json(
            options,
            [
                "-n",
                patch.namespace,
                "get",
                "deployment",
                patch.deployment,
                "-o",
                "json",
            ],
            redactor=redactor,
            timeout=30.0,
        )
        deployment_metadata = deployment.get("metadata")
        if not isinstance(deployment_metadata, dict):
            raise ReleaseGateError(
                "release.vault_kms_cleanup_failed",
                "The Kyverno Deployment metadata could not be restored.",
                {"deployment": patch.deployment, "namespace": patch.namespace},
            )
        deployment_annotations = deployment_metadata.get("annotations")
        if isinstance(deployment_annotations, dict):
            owner = deployment_annotations.get(OWNER_ANNOTATION)
            if owner not in (None, run_id):
                raise ReleaseGateError(
                    "release.vault_kms_cleanup_failed",
                    "The admission gate refused to restore a Kyverno Deployment claimed by another run.",
                    {"deployment": patch.deployment, "namespace": patch.namespace},
                )
            deployment_annotations.pop(OWNER_ANNOTATION, None)
        _spec, template_metadata, containers, volumes = _deployment_template_spec(
            deployment,
            code="release.vault_kms_cleanup_failed",
            message="The Kyverno Deployment template could not be restored.",
        )
        template_annotations = template_metadata.get("annotations")
        if isinstance(template_annotations, dict):
            owner = template_annotations.get(OWNER_ANNOTATION)
            if owner not in (None, run_id):
                raise ReleaseGateError(
                    "release.vault_kms_cleanup_failed",
                    "The admission gate refused to restore a Kyverno Pod template claimed by another run.",
                    {"deployment": patch.deployment, "namespace": patch.namespace},
                )
            template_annotations.pop(OWNER_ANNOTATION, None)
            template_annotations.pop("synara.dev/registry-ca-refresh", None)
        if patch.injected_mount:
            deployment["spec"]["template"]["spec"]["volumes"] = [
                volume
                for volume in volumes
                if not (isinstance(volume, dict) and volume.get("name") == patch.volume_name)
            ]
            for container in containers:
                mounts = container.get("volumeMounts")
                if isinstance(mounts, list):
                    container["volumeMounts"] = [
                        mount
                        for mount in mounts
                        if not (
                            isinstance(mount, dict)
                            and mount.get("name") == patch.volume_name
                            and mount.get("mountPath") == KYVERNO_CA_MOUNT_PATH
                        )
                    ]
        _apply_resource_documents(
            options,
            [_cleanup_resource_document(deployment)],
            redactor=redactor,
        )
        rollout = kubectl_completed(
            options,
            [
                "-n",
                patch.namespace,
                "rollout",
                "status",
                f"deployment/{patch.deployment}",
                "--timeout=180s",
            ],
            redactor=redactor,
            timeout=200.0,
        )
        if rollout.returncode != 0:
            raise ReleaseGateError(
                "release.vault_kms_cleanup_failed",
                "A Kyverno controller did not become ready after cleanup.",
                {
                    "deployment": patch.deployment,
                    "namespace": patch.namespace,
                    "outputExcerpt": _output_excerpt(rollout, redactor),
                },
            )
        restored.append(f"deployment:{patch.namespace}:{patch.deployment}")
    for kind, name, namespace in bundle.created_resources:
        owner = _resource_owner(
            options,
            kind=kind,
            name=name,
            namespace=namespace,
            redactor=redactor,
        )
        if owner is None:
            continue
        if owner != run_id:
            raise ReleaseGateError(
                "release.vault_kms_cleanup_failed",
                "The admission gate refused to delete a resource without its exact owner label.",
                {"kind": kind, "name": name, "namespace": namespace},
            )
        arguments = ["delete", kind, name, "--ignore-not-found=true"]
        if namespace is not None:
            arguments[0:0] = ["-n", namespace]
        completed = kubectl_completed(
            options,
            arguments,
            redactor=redactor,
            timeout=30.0,
        )
        if completed.returncode != 0:
            raise ReleaseGateError(
                "release.vault_kms_cleanup_failed",
                "The owned admission resource could not be removed.",
                {"kind": kind, "name": name, "namespace": namespace},
            )
        removed.append(f"{kind}:{namespace or '_cluster'}:{name}")
    return {
        "ownedResourcesRemoved": sorted(removed),
        "kyvernoResourcesRestored": sorted(restored),
        "exactOwnerCleanup": True,
        "broadCleanupUsed": False,
    }


def verify_existing_admission_resources(
    options: GateOptions,
    *,
    profile: supply_chain.ProductionSigningProfile,
    public_key: str,
    repository_pattern: str,
    source_hashes: Mapping[str, str],
    redactor: acceptance.SecretRedactor,
) -> AdmissionBundle:
    public_key_cm = kubectl_json(
        options,
        [
            "-n",
            profile.public_key_configmap_namespace,
            "get",
            "configmap",
            profile.public_key_configmap_name,
            "-o",
            "json",
        ],
        redactor=redactor,
        timeout=20.0,
    )
    repository_cm = kubectl_json(
        options,
        [
            "-n",
            profile.repository_configmap_namespace,
            "get",
            "configmap",
            profile.repository_configmap_name,
            "-o",
            "json",
        ],
        redactor=redactor,
        timeout=20.0,
    )
    cluster_policy = kubectl_json(
        options,
        ["get", "clusterpolicy", profile.cluster_policy_path.rsplit("/", 1)[-1].removesuffix(".yaml"), "-o", "json"],
        redactor=redactor,
        timeout=20.0,
    )
    public_key_data = public_key_cm.get("data") if isinstance(public_key_cm, dict) else None
    repository_data = repository_cm.get("data") if isinstance(repository_cm, dict) else None
    cluster_policy_spec = cluster_policy.get("spec") if isinstance(cluster_policy, dict) else None
    if (
        not isinstance(public_key_data, dict)
        or public_key_data.get(profile.public_key_configmap_key) != public_key
        or not isinstance(repository_data, dict)
        or repository_data.get(profile.repository_configmap_key) != repository_pattern
    ):
        raise ReleaseGateError(
            "release.vault_kms_admission_invalid",
            "The existing Kyverno admission ConfigMaps did not match the exported public key or repository pattern.",
        )
    validate_cluster_policy_spec(
        cluster_policy_spec,
        public_key_configmap_namespace=profile.public_key_configmap_namespace,
        public_key_configmap_name=profile.public_key_configmap_name,
        repository_pattern=repository_pattern,
    )
    return AdmissionBundle(
        mode="verify-existing",
        namespace=profile.public_key_configmap_namespace,
        repository_pattern=repository_pattern,
        names=AdmissionResourceNames(
            public_key_configmap=profile.public_key_configmap_name,
            repository_configmap=profile.repository_configmap_name,
            cluster_policy=pathlib.Path(profile.cluster_policy_path).stem,
        ),
        source_hashes=dict(sorted(source_hashes.items())),
        rendered_hashes={
            "clusterPolicySpec": sha256_text(
                json.dumps(cluster_policy_spec, sort_keys=True, separators=(",", ":"))
            ),
            "publicKeyConfigMap": sha256_text(public_key),
            "repositoryConfigMap": sha256_text(repository_pattern),
        },
        created_resources=(),
    )


def _normalized_autogen_controllers(value: Any) -> tuple[str, ...]:
    if not isinstance(value, str) or not value.strip():
        raise ReleaseGateError(
            "release.vault_kms_admission_invalid",
            "The Kyverno ClusterPolicy omitted its autogen-controllers annotation.",
        )
    controllers = tuple(item.strip() for item in value.split(",") if item.strip())
    if len(set(controllers)) != len(controllers) or set(controllers) != set(REQUIRED_AUTOGEN_CONTROLLERS):
        raise ReleaseGateError(
            "release.vault_kms_admission_invalid",
            "The Kyverno ClusterPolicy did not preserve the required autogen controller set.",
            {
                "expectedAutogenControllers": list(REQUIRED_AUTOGEN_CONTROLLERS),
                "actualAutogenControllers": sorted(set(controllers)),
            },
        )
    return tuple(sorted(controllers))


def verify_live_cluster_policy(
    options: GateOptions,
    *,
    cluster_policy_name: str,
    public_key_configmap_namespace: str,
    public_key_configmap_name: str,
    repository_pattern: str,
    redactor: acceptance.SecretRedactor,
) -> dict[str, Any]:
    payload = kubectl_json(
        options,
        ["get", "clusterpolicy", cluster_policy_name, "-o", "json"],
        redactor=redactor,
        timeout=20.0,
    )
    metadata = _json_object(
        payload.get("metadata"),
        code="release.vault_kms_admission_invalid",
        message="The live Kyverno ClusterPolicy metadata was malformed.",
    )
    spec = payload.get("spec")
    validate_cluster_policy_spec(
        spec,
        public_key_configmap_namespace=public_key_configmap_namespace,
        public_key_configmap_name=public_key_configmap_name,
        repository_pattern=repository_pattern,
    )
    annotations = metadata.get("annotations")
    if not isinstance(annotations, dict):
        raise ReleaseGateError(
            "release.vault_kms_admission_invalid",
            "The live Kyverno ClusterPolicy omitted its annotations map.",
        )
    autogen_controllers = _normalized_autogen_controllers(
        annotations.get(AUTOGEN_CONTROLLERS_ANNOTATION)
    )
    return {
        "name": cluster_policy_name,
        "documentSha256": sha256_text(
            json.dumps(_cleanup_resource_document(payload), sort_keys=True, separators=(",", ":"))
        ),
        "specSha256": sha256_text(json.dumps(spec, sort_keys=True, separators=(",", ":"))),
        "autogenControllers": list(autogen_controllers),
        "autogenControllerCount": len(autogen_controllers),
        "autogenControllersAnnotationSha256": sha256_text(
            str(annotations.get(AUTOGEN_CONTROLLERS_ANNOTATION))
        ),
    }


def validate_cluster_policy_spec(
    spec: Any,
    *,
    public_key_configmap_namespace: str,
    public_key_configmap_name: str,
    repository_pattern: str,
) -> None:
    if not isinstance(spec, dict):
        raise ReleaseGateError(
            "release.vault_kms_admission_invalid",
            "The Kyverno ClusterPolicy spec was unavailable.",
        )
    rules = spec.get("rules")
    if (
        spec.get("validationFailureAction") != supply_chain.ADMISSION_VALIDATION_FAILURE_ACTION
        or spec.get("failurePolicy") != supply_chain.ADMISSION_FAILURE_POLICY
        or spec.get("background") is not False
        or spec.get("webhookTimeoutSeconds") != 30
        or not isinstance(rules, list)
        or len(rules) != 1
    ):
        raise ReleaseGateError(
            "release.vault_kms_admission_invalid",
            "The Kyverno ClusterPolicy did not preserve the required fail-closed verifyImages configuration.",
        )
    rule = rules[0]
    verify_images = rule.get("verifyImages") if isinstance(rule, dict) else None
    contexts = rule.get("context") if isinstance(rule, dict) else None
    match = rule.get("match") if isinstance(rule, dict) else None
    if (
        not isinstance(rule, dict)
        or rule.get("name") != "verify-synara-worker-images"
        or match != {"any": [{"resources": {"kinds": ["Pod"]}}]}
        or not isinstance(verify_images, list)
        or len(verify_images) != 1
        or not isinstance(contexts, list)
        or len(contexts) != 1
    ):
        raise ReleaseGateError(
            "release.vault_kms_admission_invalid",
            "The Kyverno ClusterPolicy did not preserve its verifyImages rule shape.",
        )
    verify_image = verify_images[0]
    attestors = verify_image.get("attestors") if isinstance(verify_image, dict) else None
    registry_credentials = (
        verify_image.get("imageRegistryCredentials")
        if isinstance(verify_image, dict)
        else None
    )
    if (
        verify_image.get("mutateDigest") is not True
        or verify_image.get("verifyDigest") is not True
        or verify_image.get("required") is not True
        or verify_image.get("imageReferences") != [repository_pattern]
        or registry_credentials
        != {"secrets": [KYVERNO_REGISTRY_PULL_SECRET_NAME]}
        or not isinstance(attestors, list)
        or len(attestors) != 1
    ):
        raise ReleaseGateError(
            "release.vault_kms_admission_invalid",
            "The Kyverno ClusterPolicy verifyImages rule did not preserve digest verification and private Registry credentials.",
        )
    attestor = attestors[0]
    entries = attestor.get("entries") if isinstance(attestor, dict) else None
    entry = entries[0] if isinstance(entries, list) and len(entries) == 1 else None
    keys = entry.get("keys") if isinstance(entry, dict) else None
    rekor = keys.get("rekor") if isinstance(keys, dict) else None
    if (
        not isinstance(attestor, dict)
        or attestor.get("count") != 1
        or not isinstance(entries, list)
        or len(entries) != 1
        or not isinstance(entry, dict)
        or not isinstance(keys, dict)
        or keys.get("publicKeys") != "{{ workerPublicKey.data.cosignPublicKey }}"
        or not isinstance(rekor, dict)
        or rekor.get("url") != supply_chain.TRANSPARENCY_LOG_URL
        or rekor.get("ignoreTlog") is not False
        or set(rekor) != {"url", "ignoreTlog"}
    ):
        raise ReleaseGateError(
            "release.vault_kms_admission_invalid",
            "The Kyverno ClusterPolicy attestor definition did not preserve the required public-key and transparency-log boundary.",
        )
    context_lookup = {}
    for context in contexts:
        configmap = context.get("configMap") if isinstance(context, dict) else None
        name = context.get("name") if isinstance(context, dict) else None
        if isinstance(name, str) and isinstance(configmap, dict):
            context_lookup[name] = configmap
    if context_lookup != {
        "workerPublicKey": {
            "namespace": public_key_configmap_namespace,
            "name": public_key_configmap_name,
        },
    }:
        raise ReleaseGateError(
            "release.vault_kms_admission_invalid",
            "The Kyverno ClusterPolicy did not reference the expected public-key ConfigMap.",
        )


def verify_kyverno_runtime(
    options: GateOptions,
    *,
    redactor: acceptance.SecretRedactor,
) -> dict[str, Any]:
    deployments = kubectl_json(
        options,
        ["get", "deploy", "-A", "-l", KYVERNO_VERIFY_IMAGE_SELECTOR, "-o", "json"],
        redactor=redactor,
        timeout=20.0,
    )
    items = deployments.get("items")
    if not isinstance(items, list) or not items:
        raise ReleaseGateError(
            "release.vault_kms_kyverno_missing",
            "Kyverno was not installed in the target cluster.",
        )
    names: list[str] = []
    controllers: list[dict[str, str]] = []
    for item in items:
        if not isinstance(item, dict):
            continue
        metadata = item.get("metadata")
        spec = item.get("spec")
        status = item.get("status")
        metadata_obj = metadata if isinstance(metadata, dict) else {}
        spec_obj = spec if isinstance(spec, dict) else {}
        status_obj = status if isinstance(status, dict) else {}
        labels = metadata_obj.get("labels") if isinstance(metadata_obj.get("labels"), dict) else {}
        name = metadata_obj.get("name")
        namespace = metadata_obj.get("namespace")
        component = labels.get("app.kubernetes.io/component")
        application_name = labels.get("app.kubernetes.io/name")
        chart = labels.get("helm.sh/chart")
        expected_name = f"kyverno-{component}"
        if (
            component not in KYVERNO_VERIFY_IMAGE_COMPONENTS
            or name != expected_name
            or application_name != expected_name
            or not isinstance(chart, str)
            or KYVERNO_CHART_PATTERN.fullmatch(chart) is None
            or not isinstance(namespace, str)
            or supply_chain.KUBERNETES_NAME_PATTERN.fullmatch(namespace) is None
        ):
            continue
        names.append(f"{namespace}/{name}")
        template = spec_obj.get("template")
        template_spec = template.get("spec") if isinstance(template, dict) else None
        service_account = (
            template_spec.get("serviceAccountName") if isinstance(template_spec, dict) else None
        )
        available_replicas = status_obj.get("availableReplicas")
        if (
            not isinstance(available_replicas, int)
            or available_replicas <= 0
            or not isinstance(service_account, str)
            or supply_chain.KUBERNETES_NAME_PATTERN.fullmatch(service_account) is None
        ):
            continue
        controllers.append(
            {
                "namespace": namespace,
                "deployment": name,
                "serviceAccount": service_account,
                "component": component,
            }
        )
    component_counts = {
        component: sum(controller["component"] == component for controller in controllers)
        for component in KYVERNO_VERIFY_IMAGE_COMPONENTS
    }
    if any(component_counts[component] != 1 for component in KYVERNO_VERIFY_IMAGE_COMPONENTS):
        raise ReleaseGateError(
            "release.vault_kms_kyverno_missing",
            "Kyverno did not expose exactly one available admission/background controller with a valid service account.",
            {"availableComponentCounts": component_counts},
        )
    return {
        "deploymentCount": len(names),
        "availableDeploymentCount": len(controllers),
        "deployments": sorted(names),
        "verifyImageControllers": sorted(
            controllers,
            key=lambda item: (item["namespace"], item["component"], item["deployment"]),
        ),
    }


def _probe_pod_spec(
    *,
    image: str,
    image_pull_secret_name: str | None,
    restart_policy: str | None,
) -> dict[str, Any]:
    spec: dict[str, Any] = {
        "containers": [
            {
                "name": "worker",
                "image": image,
                "command": ["sh", "-c", "exit 0"],
                "imagePullPolicy": "Always",
            }
        ]
    }
    if restart_policy is not None:
        spec["restartPolicy"] = restart_policy
    if image_pull_secret_name is not None:
        spec["imagePullSecrets"] = [{"name": image_pull_secret_name}]
    return spec


def _dry_run_workload_manifest(
    *,
    name: str,
    namespace: str,
    image: str,
    run_id: str,
    image_pull_secret_name: str | None = None,
    resource_kind: str = "Pod",
) -> str:
    labels = _owner_labels(run_id)
    kind = resource_kind.strip()
    if kind == "Pod":
        manifest = {
            "apiVersion": "v1",
            "kind": "Pod",
            "metadata": {
                "name": name,
                "namespace": namespace,
                "labels": labels,
            },
            "spec": _probe_pod_spec(
                image=image,
                image_pull_secret_name=image_pull_secret_name,
                restart_policy="Never",
            ),
        }
        return json.dumps(manifest)
    pod_template = {
        "metadata": {"labels": labels},
        "spec": _probe_pod_spec(
            image=image,
            image_pull_secret_name=image_pull_secret_name,
            restart_policy="Never" if kind in {"Job", "CronJob"} else None,
        ),
    }
    selector = {"matchLabels": labels}
    if kind == "Deployment":
        manifest = {
            "apiVersion": "apps/v1",
            "kind": kind,
            "metadata": {"name": name, "namespace": namespace, "labels": labels},
            "spec": {
                "replicas": 1,
                "selector": selector,
                "template": pod_template,
            },
        }
    elif kind == "StatefulSet":
        manifest = {
            "apiVersion": "apps/v1",
            "kind": kind,
            "metadata": {"name": name, "namespace": namespace, "labels": labels},
            "spec": {
                "serviceName": f"{name}-headless",
                "replicas": 1,
                "selector": selector,
                "template": pod_template,
            },
        }
    elif kind == "Job":
        manifest = {
            "apiVersion": "batch/v1",
            "kind": kind,
            "metadata": {"name": name, "namespace": namespace, "labels": labels},
            "spec": {
                "template": pod_template,
            },
        }
    elif kind == "CronJob":
        manifest = {
            "apiVersion": "batch/v1",
            "kind": kind,
            "metadata": {"name": name, "namespace": namespace, "labels": labels},
            "spec": {
                "schedule": "*/15 * * * *",
                "jobTemplate": {
                    "spec": {
                        "template": pod_template,
                    }
                },
            },
        }
    else:
        raise ValueError(f"unsupported dry-run workload kind: {resource_kind}")
    return json.dumps(manifest)


def _dry_run_pod_manifest(
    *,
    name: str,
    namespace: str,
    image: str,
    run_id: str,
    image_pull_secret_name: str | None = None,
) -> str:
    return _dry_run_workload_manifest(
        name=name,
        namespace=namespace,
        image=image,
        run_id=run_id,
        image_pull_secret_name=image_pull_secret_name,
        resource_kind="Pod",
    )


def run_admission_probe(
    options: GateOptions,
    *,
    namespace: str,
    case_id: str,
    image: str,
    run_id: str,
    image_pull_secret_name: str | None,
    expect_allowed: bool,
    redactor: acceptance.SecretRedactor,
    resource_kind: str = "Pod",
) -> dict[str, Any]:
    pod_name = f"synara-{case_id}-{uuid.uuid5(uuid.NAMESPACE_DNS, run_id + case_id).hex[:12]}"
    completed = kubectl_completed(
        options,
        ["create", "--dry-run=server", "-o", "json", "-f", "-"],
        redactor=redactor,
        input_text=_dry_run_workload_manifest(
            name=pod_name,
            namespace=namespace,
            image=image,
            run_id=run_id,
            image_pull_secret_name=image_pull_secret_name,
            resource_kind=resource_kind,
        ),
        timeout=30.0,
    )
    excerpt = _output_excerpt(completed, redactor)
    if expect_allowed:
        if completed.returncode != 0:
            raise ReleaseGateError(
                "release.vault_kms_admission_invalid",
                "The signed production image was not admitted by Kyverno.",
                {"case": case_id, "outputExcerpt": excerpt},
            )
        try:
            payload = json.loads(completed.stdout)
        except json.JSONDecodeError:
            raise ReleaseGateError(
                "release.vault_kms_admission_invalid",
                "The admitted dry-run Pod output was not valid JSON.",
                {"case": case_id},
            ) from None
        metadata = payload.get("metadata") if isinstance(payload, dict) else None
        if not isinstance(metadata, dict) or metadata.get("name") != pod_name:
            raise ReleaseGateError(
                "release.vault_kms_admission_invalid",
                "The admitted dry-run Pod output omitted its expected metadata.",
                {"case": case_id},
            )
        return {
            "case": case_id,
            "status": "admitted",
            "dryRun": "server",
            "resourceKind": resource_kind,
            "name": pod_name,
            "namespace": namespace,
            "imagePullSecret": image_pull_secret_name,
        }
    if completed.returncode == 0:
        raise ReleaseGateError(
            "release.vault_kms_admission_invalid",
            "A negative admission probe unexpectedly succeeded.",
            {"case": case_id},
        )
    lowered = excerpt.lower()
    if not any(marker in lowered for marker in ADMISSION_DENIAL_MARKERS):
        raise ReleaseGateError(
            "release.vault_kms_admission_invalid",
            "A negative admission probe failed for a reason that did not look like Kyverno image verification denial.",
            {"case": case_id, "outputExcerpt": excerpt},
        )
    return {
        "case": case_id,
        "status": "denied",
        "dryRun": "server",
        "resourceKind": resource_kind,
        "reasonExcerpt": excerpt,
        "imagePullSecret": image_pull_secret_name,
    }


def markdown_from_report(report: Mapping[str, Any]) -> str:
    vault = report.get("vault")
    registry = report.get("registry")
    admission = report.get("admission")
    cleanup = report.get("cleanup")
    lines = [
        "# Stage 3 Vault KMS Admission Gate",
        "",
        f"- Schema: `{report['schemaVersion']}`",
        f"- Run: `{report['runId']}`",
        f"- Status: **{report['status']}**",
        f"- Started: `{report['startedAt']}`",
        f"- Finished: `{report['finishedAt']}`",
        f"- Duration: `{report['durationMs']} ms`",
        "",
        "## Evidence boundary",
        "",
        "This gate verifies the checked-in production signing policy/profile, a three-node HTTPS Vault raft cluster,",
        "the exact Transit ECDSA P-256 key, independent signer/operator batch-token identities, the live peer",
        "NetworkPolicy, two audit devices, a TLS+Basic-auth registry, exported public-key evidence, live",
        "ClusterPolicy autogen coverage, and Kyverno admit/deny behavior",
        "for signed, unsigned, wrong-key, and tag-drift image probes. It does not claim to bootstrap Vault, install",
        "Kyverno, or mint unsigned images.",
    ]
    if isinstance(vault, dict):
        kubernetes_details = (
            vault.get("kubernetes") if isinstance(vault.get("kubernetes"), dict) else {}
        )
        vault_details = vault.get("vault") if isinstance(vault.get("vault"), dict) else {}
        identities = (
            vault_details.get("identities")
            if isinstance(vault_details.get("identities"), dict)
            else {}
        )
        signer_identity = identities.get("signer") if isinstance(identities.get("signer"), dict) else {}
        operator_identity = (
            identities.get("operator") if isinstance(identities.get("operator"), dict) else {}
        )
        lines.extend(
            [
                "",
                "## Vault",
                "",
                f"- Principal: `{vault_details.get('principal', '')}`",
                f"- Signer token env: `{signer_identity.get('tokenEnvironment', '')}`",
                f"- Signer identity SHA256: `{signer_identity.get('identitySha256', '')}`",
                f"- Operator token env: `{operator_identity.get('tokenEnvironment', '')}`",
                f"- Operator identity SHA256: `{operator_identity.get('identitySha256', '')}`",
                f"- Key reference: `{vault_details.get('keyReference', '')}`",
                f"- Key type: `{vault_details.get('keyType', '')}`",
                f"- Audit request ID: `{vault_details.get('transitAuditRequestId', '')}`",
                f"- Policy hash: `{vault_details.get('policyHash', '')}`",
                f"- Audit devices: `{vault_details.get('auditDeviceCount', '')}`",
                f"- File audit devices: `{vault_details.get('fileAuditDeviceCount', '')}`",
                f"- StatefulSet image: `{kubernetes_details.get('imageReference', '')}`",
                f"- Audit shipper image: `{kubernetes_details.get('auditShipperImageReference', '')}`",
                f"- Audit rotation image: `{kubernetes_details.get('auditRotationImageReference', '')}`",
                f"- Audit observability ConfigMap: `{kubernetes_details.get('auditObservabilityConfigMapName', '')}`",
                f"- Audit observability SHA256: `{kubernetes_details.get('auditObservabilityConfigSha256', '')}`",
                f"- Audit SIEM Secret: `{kubernetes_details.get('auditSiemSecretName', '')}`",
                f"- Unauthenticated generate-root attempt status: `{kubernetes_details.get('unauthenticatedGenerateRootAttemptStatus', '')}`",
            ]
        )
    if isinstance(registry, dict):
        lines.extend(
            [
                "",
                "## Registry",
                "",
                f"- Host: `{registry.get('registryHost', '')}`",
                f"- Principal env: `{registry.get('principalEnvironment', '')}`",
                f"- Principal hash: `{registry.get('principalSha256', '')}`",
                f"- CA env: `{registry.get('caCertificateEnvironment', '')}`",
                f"- TLS certificate SHA256: `{registry.get('tlsCertificateSha256', '')}`",
            ]
        )
    if isinstance(admission, dict):
        cluster_policy = (
            admission.get("clusterPolicy") if isinstance(admission.get("clusterPolicy"), dict) else {}
        )
        signature = admission.get("signature") if isinstance(admission.get("signature"), dict) else {}
        rekor_entries = signature.get("rekorEntries") if isinstance(signature.get("rekorEntries"), list) else []
        lines.extend(
            [
                "",
                "## Admission",
                "",
                f"- Mode: `{admission.get('mode', '')}`",
                f"- Repository pattern hash: `{admission.get('repositoryPatternSha256', '')}`",
                f"- Public key SHA256: `{admission.get('publicKeySha256', '')}`",
                f"- Live autogen controllers: `{json.dumps(cluster_policy.get('autogenControllers', []), ensure_ascii=False)}`",
            ]
        )
        if rekor_entries:
            lines.append(f"- Rekor entries: `{json.dumps(rekor_entries, ensure_ascii=False)}`")
        probe_results = admission.get("probes")
        if isinstance(probe_results, dict):
            lines.extend(
                [
                    "",
                    "| Probe | Result |",
                    "| --- | --- |",
                ]
            )
            for case_id, result in sorted(probe_results.items()):
                if isinstance(result, dict):
                    lines.append(f"| `{case_id}` | `{result.get('status', '')}` |")
        controller_probe_results = admission.get("controllerProbes")
        if isinstance(controller_probe_results, dict):
            lines.extend(
                [
                    "",
                    "| Controller probe | Result |",
                    "| --- | --- |",
                ]
            )
            for case_id, result in sorted(controller_probe_results.items()):
                if isinstance(result, dict):
                    lines.append(f"| `{case_id}` | `{result.get('status', '')}` |")
    if isinstance(cleanup, dict):
        lines.extend(
            [
                "",
                "## Cleanup",
                "",
                f"- Isolated state removed: `{cleanup.get('isolatedStateRemoved', False)}`",
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


def run_vault_kms_admission_gate(
    options: GateOptions,
    *,
    repository_state: Any = common.repository_state,
    configuration_loader: Any = supply_chain.load_configuration,
    registry_release_loader: Any = load_registry_release_evidence,
) -> int:
    if options.output_dir.exists() and (
        not options.output_dir.is_dir() or any(options.output_dir.iterdir())
    ):
        print("Vault KMS admission gate output directory must be empty or absent.", file=sys.stderr)
        return 2
    options.output_dir.mkdir(parents=True, exist_ok=True)
    state_dir = options.output_dir / "_state"
    ensure_private_directory(state_dir)
    started_at = utc_now()
    started = time.monotonic()
    run_id = f"stage3-vault-kms-admission-{uuid.uuid4()}"
    redactor = acceptance.SecretRedactor()
    created_bundle: AdmissionBundle | None = None
    source: dict[str, Any] = {}
    vault_evidence: dict[str, Any] = {}
    registry_evidence: dict[str, Any] = {}
    admission_evidence: dict[str, Any] = {}
    cluster_policy_evidence: dict[str, Any] = {}
    cleanup: dict[str, Any] = {"isolatedStateRemoved": False, "exactOwnerCleanup": False, "broadCleanupUsed": False}
    errors: list[dict[str, Any]] = []
    try:
        source = dict(repository_state(options.repo_root))
        configuration = configuration_loader(
            options.repo_root,
            signing_policy_profile="production",
        )
        if (
            not isinstance(configuration, supply_chain.SupplyChainConfiguration)
            or configuration.production_signing_profile is None
        ):
            raise ReleaseGateError(
                "release.vault_kms_source_invalid",
                "The checked-in production signing configuration was unavailable.",
            )
        profile = configuration.production_signing_profile
        release_evidence = registry_release_loader(options.registry_release_gate_report)
        validate_release_evidence_against_source(
            release_evidence,
            source=source,
            configuration=configuration,
        )
        signed_image = choose_signed_image(options.signed_image_ref, release_evidence)
        unsigned_image = normalize_image_reference(
            options.unsigned_image_ref,
            flag="--unsigned-image-ref",
            require_digest=True,
        )
        wrong_key_image = normalize_image_reference(
            options.wrong_key_image_ref,
            flag="--wrong-key-image-ref",
            require_digest=True,
        )
        try:
            tag_drift_image = normalize_tag_drift_image_reference(
                options.tag_drift_image_ref,
                flag="--tag-drift-image-ref",
            )
        except ValueError as error:
            raise ReleaseGateError(
                "release.vault_kms_configuration_invalid",
                str(error),
            ) from None
        for label, reference in (
            ("unsigned image", unsigned_image),
            ("wrong-key image", wrong_key_image),
            ("tag-drift image", tag_drift_image),
        ):
            ensure_matching_repository(
                reference,
                expected_repository=release_evidence.image_repository,
                code="release.vault_kms_image_identity_invalid",
                label=label,
            )
        repository_pattern = f"{release_evidence.image_repository}*"
        secret_inputs = prepare_secret_inputs(
            options,
            state_dir=state_dir,
            redactor=redactor,
        )
        source.update(
            {
                "registryReleaseGate": release_evidence.as_report(),
                "productionSigningPolicy": configuration.signing_policy.as_report(),
                "productionSigningProfile": profile.as_report(),
            }
        )
        kyverno_runtime = verify_kyverno_runtime(options, redactor=redactor)
        vault_evidence = verify_vault_cluster(
            options,
            secret_inputs,
            redactor=redactor,
        )
        validate_registry_signer_identity_against_live(release_evidence, vault_evidence)
        public_key_evidence = export_vault_public_key(
            options,
            secret_inputs,
            state_dir=state_dir,
            redactor=redactor,
        )
        client, registry_boundary = verify_tls_registry(release_evidence, secret_inputs)
        registry_runtime_cross_check = validate_registry_runtime_evidence_against_live_boundary(
            release_evidence,
            options=options,
            registry_boundary=registry_boundary,
        )
        tag_digest = resolve_registry_digest_for_tag(client, tag_drift_image)
        if tag_digest == signed_image.digest:
            raise ReleaseGateError(
                "release.vault_kms_tag_drift_invalid",
                "The configured tag-drift image did not currently resolve to a digest different from the signed baseline.",
                {"tag": tag_drift_image.original},
            )
        signature_evidence = verify_positive_signature(
            options,
            secret_inputs,
            public_key_evidence["path"],
            signed_image,
            release_evidence,
            redactor=redactor,
        )
        probe_names, probe_hashes, probe_resources, probe_documents = render_probe_access_resources(
            run_id=run_id,
            namespace=options.admission_test_namespace,
            registry_host=release_evidence.image_repository.split("/", 1)[0],
            secret_inputs=secret_inputs,
            kyverno_runtime=kyverno_runtime,
            redactor=redactor,
        )
        if options.admission_mode == "verify-existing":
            bundle = verify_existing_admission_resources(
                options,
                profile=profile,
                public_key=_read_text_file(
                    public_key_evidence["path"],
                    code="release.vault_kms_public_key_invalid",
                    message="The exported public key could not be reread.",
                ),
                repository_pattern=repository_pattern,
                source_hashes=release_evidence.source_hashes,
                redactor=redactor,
            )
            bundle = dataclasses.replace(
                bundle,
                names=_merge_admission_resource_names(bundle.names, probe_names),
                rendered_hashes=dict(sorted({**bundle.rendered_hashes, **probe_hashes}.items())),
                created_resources=probe_resources,
            )
            created_bundle = bundle
            _apply_resource_documents(options, probe_documents, redactor=redactor)
        else:
            bundle, public_key_cm, repository_cm, cluster_policy = render_admission_bundle(
                run_id=run_id,
                namespace=options.security_namespace,
                repository_pattern=repository_pattern,
                public_key=_read_text_file(
                    public_key_evidence["path"],
                    code="release.vault_kms_public_key_invalid",
                    message="The exported public key could not be reread.",
                ),
                source_hashes=release_evidence.source_hashes,
            )
            bundle = dataclasses.replace(
                bundle,
                names=_merge_admission_resource_names(bundle.names, probe_names),
                rendered_hashes=dict(sorted({**bundle.rendered_hashes, **probe_hashes}.items())),
                created_resources=tuple((*bundle.created_resources, *probe_resources)),
            )
            created_bundle = bundle
            bundle = apply_owned_admission_bundle(
                options,
                bundle,
                [public_key_cm, repository_cm, cluster_policy, *probe_documents],
                run_id=run_id,
                kyverno_runtime=kyverno_runtime,
                secret_inputs=secret_inputs,
                redactor=redactor,
            )
            created_bundle = bundle
        cluster_policy_name = bundle.names.cluster_policy
        if not isinstance(cluster_policy_name, str) or not cluster_policy_name:
            raise ReleaseGateError(
                "release.vault_kms_admission_invalid",
                "The admission gate did not retain its ClusterPolicy name.",
            )
        live_public_key_namespace = profile.public_key_configmap_namespace
        live_public_key_name = profile.public_key_configmap_name
        live_repository_namespace = profile.repository_configmap_namespace
        live_repository_name = profile.repository_configmap_name
        if options.admission_mode == "apply-owned":
            live_public_key_namespace = bundle.namespace
            live_public_key_name = str(bundle.names.public_key_configmap)
            live_repository_namespace = bundle.namespace
            live_repository_name = str(bundle.names.repository_configmap)
        cluster_policy_evidence = verify_live_cluster_policy(
            options,
            cluster_policy_name=cluster_policy_name,
            public_key_configmap_namespace=live_public_key_namespace,
            public_key_configmap_name=live_public_key_name,
            repository_pattern=repository_pattern,
            redactor=redactor,
        )
        probes = {
            "signed": run_admission_probe(
                options,
                namespace=options.admission_test_namespace,
                case_id="signed",
                image=signed_image.digest_reference,
                run_id=run_id,
                image_pull_secret_name=bundle.names.probe_pull_secret,
                expect_allowed=True,
                redactor=redactor,
            ),
            "unsigned": run_admission_probe(
                options,
                namespace=options.admission_test_namespace,
                case_id="unsigned",
                image=unsigned_image.digest_reference,
                run_id=run_id,
                image_pull_secret_name=bundle.names.probe_pull_secret,
                expect_allowed=False,
                redactor=redactor,
            ),
            "wrong-key": run_admission_probe(
                options,
                namespace=options.admission_test_namespace,
                case_id="wrong-key",
                image=wrong_key_image.digest_reference,
                run_id=run_id,
                image_pull_secret_name=bundle.names.probe_pull_secret,
                expect_allowed=False,
                redactor=redactor,
            ),
            "tag-drift": run_admission_probe(
                options,
                namespace=options.admission_test_namespace,
                case_id="tag-drift",
                image=tag_drift_image.original,
                run_id=run_id,
                image_pull_secret_name=bundle.names.probe_pull_secret,
                expect_allowed=False,
                redactor=redactor,
            ),
        }
        controller_probes = {
            kind.lower(): run_admission_probe(
                options,
                namespace=options.admission_test_namespace,
                case_id=f"{kind.lower()}-wrong-key",
                image=wrong_key_image.digest_reference,
                run_id=run_id,
                image_pull_secret_name=bundle.names.probe_pull_secret,
                expect_allowed=False,
                redactor=redactor,
                resource_kind=kind,
            )
            for kind in REQUIRED_CONTROLLER_PROBE_KINDS
        }
        registry_evidence = {
            **registry_boundary,
            "runtimeEvidence": registry_runtime_cross_check,
            "repositoryPatternSha256": sha256_text(repository_pattern),
            "tagDriftCurrentDigest": tag_digest,
            "tagDriftExpectedDigest": signed_image.digest,
        }
        admission_evidence = {
            **bundle.as_report(),
            "publicKeySha256": public_key_evidence["publicKeySha256"],
            "signature": signature_evidence,
            "clusterPolicy": cluster_policy_evidence,
            "probes": probes,
            "controllerProbes": controller_probes,
            "kyvernoRuntime": kyverno_runtime,
        }
    except ReleaseGateError as error:
        errors.append(error.as_report_error())
    finally:
        if created_bundle is not None:
            try:
                cleanup.update(
                    cleanup_owned_resources(
                        options,
                        created_bundle,
                        run_id=run_id,
                        redactor=redactor,
                    )
                )
            except ReleaseGateError as error:
                errors.append(error.as_report_error())
        shutil.rmtree(state_dir, ignore_errors=True)
        cleanup["isolatedStateRemoved"] = not state_dir.exists()
        if not cleanup["isolatedStateRemoved"]:
            errors.append(
                {
                    "code": "release.vault_kms_cleanup_failed",
                    "message": "The Vault KMS admission gate isolated state directory was not removed.",
                }
            )
    status = "pass" if not errors else "fail"
    report = {
        "schemaVersion": SCHEMA_VERSION,
        "runId": run_id,
        "mode": "vault-kms-admission-gate",
        "status": status,
        "startedAt": started_at,
        "finishedAt": utc_now(),
        "durationMs": elapsed_ms(started),
        "source": source,
        "configuration": {
            "kubeContext": options.kube_context,
            "vaultNamespace": options.vault_namespace,
            "securityNamespace": options.security_namespace,
            "admissionTestNamespace": options.admission_test_namespace,
            "vaultSelector": options.vault_selector,
            "vaultAppRolePrincipal": f"auth/approle/role/{options.vault_approle_name}",
            "vaultOperatorAppRolePrincipal": (
                f"auth/approle/role/{DEFAULT_VAULT_OPERATOR_APPROLE_NAME}"
            ),
            "vaultAppRolePolicyHash": sha256_text("\n".join(sorted(options.expected_approle_policies))),
            "vaultOperatorPolicyHash": sha256_text(VAULT_OPERATOR_POLICY_NAME),
            "keyReference": EXPECTED_VAULT_KEY_REFERENCE,
            "keyType": EXPECTED_VAULT_KEY_TYPE,
            "admissionMode": options.admission_mode,
            "environmentNames": {
                "vault": list(
                    (
                        options.vault_address_env,
                        options.vault_token_env,
                        options.vault_operator_token_env,
                        options.vault_cacert_env,
                    )
                ),
                "registry": [
                    options.registry_username_env,
                    options.registry_password_env,
                    options.registry_ca_cert_env,
                ],
            },
        },
        "vault": vault_evidence,
        "registry": registry_evidence,
        "admission": admission_evidence,
        "cleanup": cleanup,
        "errors": errors,
    }
    json_path, markdown_path = write_report(report, options.output_dir, redactor)
    output_scan = acceptance.scan_output_secrets(options.output_dir, redactor)
    if output_scan.get("findings"):
        report["status"] = "fail"
        report["errors"].append(
            {
                "code": "release.vault_kms_output_secret_scan_failed",
                "message": "The Vault KMS admission gate report retained secret-like findings.",
                "evidence": {"findingCount": len(output_scan["findings"])},
            }
        )
    report["security"] = {"outputSecretScan": output_scan}
    json_path, markdown_path = write_report(report, options.output_dir, redactor)
    print(f"Stage 3 Vault KMS admission gate: {report['status']}")
    print(f"JSON: {json_path}")
    print(f"Markdown: {markdown_path}")
    return 0 if report["status"] == "pass" else 1


def main(argv: Sequence[str] | None = None) -> int:
    options = parse_args(argv if argv is not None else sys.argv[1:])
    return run_vault_kms_admission_gate(options)


if __name__ == "__main__":
    raise SystemExit(main())
